// Package webapp exposes the single-user homelab dashboard and collection API.
package webapp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/valche5/ai-usage/internal/connection"
	"github.com/valche5/ai-usage/internal/credstore"
	"github.com/valche5/ai-usage/internal/httpx"
	"github.com/valche5/ai-usage/internal/oauthflow"
	"github.com/valche5/ai-usage/internal/provider"
)

type Config struct {
	Password        string
	SessionTTL      time.Duration
	RefreshInterval time.Duration
	HTTPTimeout     time.Duration
}

type App struct {
	config  Config
	store   *connection.Store
	oauth   *oauthflow.Manager
	client  *http.Client
	collect sync.Mutex
	csrf    string
	tmpl    *template.Template
	handler http.Handler

	clientsMu        sync.Mutex
	clients          map[*wsClient]struct{}
	clientChanged    chan struct{}
	refreshRequested chan struct{}
	lastRefreshMu    sync.RWMutex
	lastRefresh      time.Time
	sessionsMu       sync.Mutex
	sessions         map[string]time.Time
}

type wsClient struct {
	conn  *websocket.Conn
	write sync.Mutex
}

type providerInfo struct {
	ID     string
	Name   string
	OAuth  bool
	APIKey bool
	Hint   string
	TTL    time.Duration
}

var supported = []providerInfo{
	{ID: "chatgpt", Name: "ChatGPT", OAuth: true, Hint: "Connexion Plus/Pro par device code", TTL: time.Minute},
	{ID: "grok", Name: "Grok", OAuth: true, Hint: "Connexion SuperGrok par device code", TTL: time.Minute},
	{ID: "copilot", Name: "GitHub Copilot", OAuth: true, Hint: "Connexion GitHub par device code", TTL: time.Minute},
	{ID: "openrouter", Name: "OpenRouter", APIKey: true, Hint: "Clé API OpenRouter", TTL: time.Minute},
	{ID: "opencode", Name: "OpenCode", APIKey: true, Hint: "Clé OpenCode Zen ou Go", TTL: 5 * time.Minute},
}

func New(config Config, store *connection.Store, oauth *oauthflow.Manager, client *http.Client) (*App, error) {
	if config.Password == "" {
		return nil, errors.New("AI_USAGE_PASSWORD est requis")
	}
	if config.RefreshInterval <= 0 {
		config.RefreshInterval = time.Minute
	}
	if config.HTTPTimeout <= 0 {
		config.HTTPTimeout = 8 * time.Second
	}
	if config.SessionTTL <= 0 {
		config.SessionTTL = 7 * 24 * time.Hour
	}
	csrfBytes := make([]byte, 32)
	if _, err := rand.Read(csrfBytes); err != nil {
		return nil, fmt.Errorf("générer le jeton CSRF: %w", err)
	}
	csrf := base64.RawURLEncoding.EncodeToString(csrfBytes)
	tmpl, err := template.New("dashboard").Funcs(template.FuncMap{
		"percent": func(value float64) string { return fmt.Sprintf("%.0f", value) },
		"count": func(value *float64) string {
			if value == nil {
				return "—"
			}
			return fmt.Sprintf("%.0f", *value)
		},
		"money": func(value *float64, currency string) string {
			if value == nil {
				return "—"
			}
			if strings.EqualFold(currency, "USD") {
				return fmt.Sprintf("$%.2f", *value)
			}
			return fmt.Sprintf("%.2f %s", *value, currency)
		},
		"clock": func(value *time.Time) string {
			if value == nil || value.IsZero() {
				return "—"
			}
			return value.Local().Format("02/01 15:04")
		},
		"lower": func(value any) string { return strings.ToLower(fmt.Sprint(value)) },
	}).Parse(pageTemplate)
	if err != nil {
		return nil, err
	}
	app := &App{
		config:           config,
		store:            store,
		oauth:            oauth,
		client:           client,
		csrf:             csrf,
		tmpl:             tmpl,
		clients:          make(map[*wsClient]struct{}),
		clientChanged:    make(chan struct{}, 1),
		refreshRequested: make(chan struct{}, 1),
		sessions:         make(map[string]time.Time),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", app.health)
	mux.HandleFunc("GET /login", app.loginPage)
	mux.HandleFunc("POST /login", app.login)
	mux.HandleFunc("POST /logout", app.logout)
	mux.HandleFunc("GET /", app.home)
	mux.HandleFunc("GET /api/reports", app.reportsAPI)
	mux.HandleFunc("GET /ws", app.webSocket)
	mux.HandleFunc("POST /refresh", app.refresh)
	mux.HandleFunc("POST /connect/{provider}", app.connect)
	mux.HandleFunc("GET /oauth/{id}", app.oauthPage)
	mux.HandleFunc("GET /api/oauth/{id}", app.oauthAPI)
	mux.HandleFunc("POST /key/{provider}", app.saveKey)
	mux.HandleFunc("POST /disconnect/{provider}", app.disconnect)
	app.handler = app.security(app.sessionAuth(mux))
	return app, nil
}

func (a *App) Handler() http.Handler { return a.handler }

// Run starts collection only while at least one dashboard WebSocket is open.
func (a *App) Run(ctx context.Context) {
	go a.updateLoop(ctx)
}

func (a *App) updateLoop(ctx context.Context) {
	var timer *time.Timer
	var timerC <-chan time.Time
	stopTimer := func() {
		if timer != nil && !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timerC = nil
	}
	schedule := func() {
		if timer == nil {
			timer = time.NewTimer(a.config.RefreshInterval)
		} else {
			timer.Reset(a.config.RefreshInterval)
		}
		timerC = timer.C
	}
	defer stopTimer()

	for {
		select {
		case <-ctx.Done():
			a.closeClients()
			return
		case <-a.clientChanged:
			if a.clientCount() == 0 {
				stopTimer()
				continue
			}
			if timerC == nil {
				a.collectAndBroadcast(ctx, false)
				schedule()
			}
		case <-a.refreshRequested:
			if a.clientCount() == 0 {
				continue
			}
			a.collectAndBroadcast(ctx, true)
			schedule()
		case <-timerC:
			timerC = nil
			if a.clientCount() == 0 {
				continue
			}
			a.collectAndBroadcast(ctx, false)
			schedule()
		}
	}
}

func (a *App) Collect(ctx context.Context, force bool) []provider.Report {
	a.collect.Lock()
	defer a.collect.Unlock()
	previous := a.store.Reports()
	connectionIDs := a.store.IDs()
	var wg sync.WaitGroup
	var mu sync.Mutex
	results := make([]provider.Report, 0, len(connectionIDs))
	for _, connectionID := range connectionIDs {
		connection, ok := a.store.Get(connectionID)
		if !ok {
			continue
		}
		info, ok := infoFor(connection.Provider)
		if !ok {
			continue
		}
		if old, ok := previous[connectionID]; !force && ok && !old.FetchedAt.IsZero() && time.Since(old.FetchedAt) < info.TTL {
			results = append(results, old)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			requestCtx, cancel := context.WithTimeout(ctx, a.config.HTTPTimeout)
			defer cancel()
			report := a.collectOne(requestCtx, connectionID, info, previous[connectionID])
			mu.Lock()
			results = append(results, report)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(results, func(i, j int) bool { return results[i].ID < results[j].ID })
	if len(results) > 0 {
		if err := a.store.PutReports(results); err != nil {
			log.Printf("persister les rapports: %v", err)
		}
	}
	return results
}

func (a *App) collectOne(ctx context.Context, connectionID string, info providerInfo, previous provider.Report) provider.Report {
	c, err := a.oauth.Fresh(ctx, connectionID)
	if err != nil {
		report := provider.Errorf(connectionID, info.Name, httpx.Redact(err.Error()))
		return staleFallback(report, previous)
	}
	cred := credstore.Cred{
		Path:      "service web",
		Token:     c.Access,
		Expires:   c.ExpiresAt,
		AccountID: c.AccountID,
		Email:     c.Email,
		Plan:      c.Plan,
	}
	loader := func(time.Time) ([]credstore.Cred, error) { return []credstore.Cred{cred}, nil }
	var collector provider.Provider
	switch info.ID {
	case "chatgpt":
		collector = provider.Codex{Credentials: loader}
	case "grok":
		collector = provider.Grok{Credentials: loader}
	case "copilot":
		collector = provider.Copilot{Credentials: loader}
	case "openrouter":
		collector = provider.OpenRouter{Credentials: loader}
	case "opencode":
		collector = provider.OpenCode{Credentials: loader}
	default:
		return provider.Errorf(info.ID, info.Name, "provider web inconnu")
	}
	now := time.Now()
	report := collector.Collect(ctx, provider.Options{Now: now, HTTP: a.client})
	report.ID = connectionID
	if report.Account == "" {
		report.Account = c.Email
	}
	if report.AccountID == "" {
		report.AccountID = c.AccountID
	}
	report.Validate(now)
	return staleFallback(report, previous)
}

func staleFallback(current, previous provider.Report) provider.Report {
	if current.Status != provider.StatusError || !previous.HasNumbers() {
		return current
	}
	previous.Status = provider.StatusStale
	previous.Source = provider.SourceCache
	previous.Reason = current.Reason
	return previous
}

type card struct {
	providerInfo
	ConnectionID string
	Connected    bool
	AddAccount   bool
	Account      string
	Report       provider.Report
	HasReport    bool
}

type homeData struct {
	Cards     []card
	Message   string
	Generated time.Time
	CSRFToken string
}

func (a *App) home(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	reports := a.store.Reports()
	data := homeData{Message: r.URL.Query().Get("message"), Generated: time.Now(), CSRFToken: a.csrf}
	for _, info := range supported {
		if info.ID == oauthflow.ProviderCopilot {
			count := 0
			for _, connectionID := range a.store.IDs() {
				c, ok := a.store.Get(connectionID)
				if !ok || c.Provider != info.ID {
					continue
				}
				data.Cards = append(data.Cards, connectedCard(info, connectionID, c, reports))
				count++
			}
			add := card{providerInfo: info, ConnectionID: info.ID, AddAccount: count > 0}
			if add.AddAccount {
				add.Hint = "Ajouter un autre compte GitHub"
			}
			data.Cards = append(data.Cards, add)
			continue
		}
		if c, ok := a.store.Get(info.ID); ok {
			data.Cards = append(data.Cards, connectedCard(info, info.ID, c, reports))
		} else {
			data.Cards = append(data.Cards, card{providerInfo: info, ConnectionID: info.ID})
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpl.ExecuteTemplate(w, "home", data); err != nil {
		log.Printf("rendre le dashboard: %v", err)
	}
}

func connectedCard(info providerInfo, connectionID string, c connection.Connection, reports map[string]provider.Report) card {
	item := card{providerInfo: info, ConnectionID: connectionID, Connected: true, Account: c.Email}
	if item.Account == "" {
		item.Account = c.AccountID
	}
	item.Report, item.HasReport = reports[connectionID]
	if item.Account == "" && item.HasReport {
		item.Account = item.Report.Account
	}
	return item
}

func (a *App) connect(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("provider")
	if !oauthProvider(providerID) {
		http.Error(w, "provider OAuth inconnu", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	session, err := a.oauth.Start(ctx, providerID)
	if err != nil {
		redirectMessage(w, r, "Impossible de démarrer OAuth: "+httpx.Redact(err.Error()))
		return
	}
	http.Redirect(w, r, "/oauth/"+url.PathEscape(session.ID), http.StatusSeeOther)
}

func (a *App) oauthPage(w http.ResponseWriter, r *http.Request) {
	session, ok := a.oauth.Session(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if session.Status == "connected" {
		a.requestRefresh()
		redirectMessage(w, r, "Connexion "+session.Provider+" réussie")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpl.ExecuteTemplate(w, "oauth", session); err != nil {
		log.Printf("rendre OAuth: %v", err)
	}
}

func (a *App) oauthAPI(w http.ResponseWriter, r *http.Request) {
	session, ok := a.oauth.Session(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, session)
}

func (a *App) saveKey(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("provider")
	if providerID != "openrouter" && providerID != "opencode" {
		http.Error(w, "provider à clé inconnu", http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "formulaire invalide", http.StatusBadRequest)
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	if len(key) < 8 {
		redirectMessage(w, r, "La clé semble invalide")
		return
	}
	if err := a.store.Replace(connection.Connection{Provider: providerID, Kind: "api", Access: key}); err != nil {
		redirectMessage(w, r, "Impossible d'enregistrer la clé")
		return
	}
	a.requestRefresh()
	redirectMessage(w, r, "Clé "+providerID+" enregistrée")
}

func (a *App) disconnect(w http.ResponseWriter, r *http.Request) {
	connectionID := r.PathValue("provider")
	c, ok := a.store.Get(connectionID)
	if !ok {
		http.Error(w, "provider inconnu", http.StatusBadRequest)
		return
	}
	a.oauth.Cancel(c.Provider)
	if err := a.store.Delete(connectionID); err != nil {
		redirectMessage(w, r, "Impossible de supprimer la connexion")
		return
	}
	redirectMessage(w, r, "Connexion "+connectionID+" supprimée")
}

func (a *App) refresh(w http.ResponseWriter, r *http.Request) {
	a.collectAndBroadcast(r.Context(), true)
	redirectMessage(w, r, "Données actualisées")
}

type reportsPayload struct {
	Type          string            `json:"type,omitempty"`
	SchemaVersion int               `json:"schema_version"`
	GeneratedAt   time.Time         `json:"generated_at"`
	LastRefresh   *time.Time        `json:"last_refresh,omitempty"`
	Providers     []provider.Report `json:"providers"`
}

func (a *App) reportsAPI(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, a.snapshot(""))
}

func (a *App) snapshot(messageType string) reportsPayload {
	reportsMap := a.store.Reports()
	reports := make([]provider.Report, 0, len(reportsMap))
	for _, report := range reportsMap {
		reports = append(reports, report)
	}
	sort.Slice(reports, func(i, j int) bool { return reports[i].ID < reports[j].ID })
	a.lastRefreshMu.RLock()
	last := a.lastRefresh
	a.lastRefreshMu.RUnlock()
	var lastPtr *time.Time
	if !last.IsZero() {
		lastCopy := last
		lastPtr = &lastCopy
	}
	return reportsPayload{
		Type:          messageType,
		SchemaVersion: 1,
		GeneratedAt:   time.Now(),
		LastRefresh:   lastPtr,
		Providers:     reports,
	}
}

func (a *App) collectAndBroadcast(ctx context.Context, force bool) {
	a.Collect(ctx, force)
	a.lastRefreshMu.Lock()
	a.lastRefresh = time.Now()
	a.lastRefreshMu.Unlock()
	a.broadcast(a.snapshot("reports"))
}

func (a *App) requestRefresh() {
	select {
	case a.refreshRequested <- struct{}{}:
	default:
	}
}

var wsUpgrader = websocket.Upgrader{
	// Le jeton CSRF de l'URL protège le handshake même lorsque le navigateur
	// produit Origin:null (cas observé avec le client Tailscale de test).
	CheckOrigin: func(*http.Request) bool { return true },
}

func (a *App) webSocket(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("csrf")), []byte(a.csrf)) != 1 {
		http.Error(w, "jeton WebSocket refusé", http.StatusForbidden)
		return
	}
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(1024)
	client := &wsClient{conn: conn}
	a.addClient(client)
	defer func() {
		a.removeClient(client)
		_ = conn.Close()
	}()

	if err := client.writeJSON(a.snapshot("reports")); err != nil {
		return
	}
	for {
		var command struct {
			Type string `json:"type"`
		}
		if err := conn.ReadJSON(&command); err != nil {
			return
		}
		if command.Type == "refresh" {
			a.requestRefresh()
		}
	}
}

func (c *wsClient) writeJSON(value any) error {
	c.write.Lock()
	defer c.write.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.conn.WriteJSON(value)
}

func (a *App) addClient(client *wsClient) {
	a.clientsMu.Lock()
	a.clients[client] = struct{}{}
	a.clientsMu.Unlock()
	a.signalClientChange()
}

func (a *App) removeClient(client *wsClient) {
	a.clientsMu.Lock()
	_, existed := a.clients[client]
	delete(a.clients, client)
	a.clientsMu.Unlock()
	if existed {
		a.signalClientChange()
	}
}

func (a *App) clientCount() int {
	a.clientsMu.Lock()
	defer a.clientsMu.Unlock()
	return len(a.clients)
}

func (a *App) signalClientChange() {
	select {
	case a.clientChanged <- struct{}{}:
	default:
	}
}

func (a *App) broadcast(payload reportsPayload) {
	a.clientsMu.Lock()
	clients := make([]*wsClient, 0, len(a.clients))
	for client := range a.clients {
		clients = append(clients, client)
	}
	a.clientsMu.Unlock()
	for _, client := range clients {
		if err := client.writeJSON(payload); err != nil {
			a.removeClient(client)
			_ = client.conn.Close()
		}
	}
}

func (a *App) closeClients() {
	a.clientsMu.Lock()
	clients := make([]*wsClient, 0, len(a.clients))
	for client := range a.clients {
		clients = append(clients, client)
		delete(a.clients, client)
	}
	a.clientsMu.Unlock()
	for _, client := range clients {
		client.write.Lock()
		_ = client.conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseGoingAway, "serveur arrêté"), time.Now().Add(time.Second))
		_ = client.conn.Close()
		client.write.Unlock()
	}
}

func (a *App) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

const sessionCookieName = "ai_usage_session"

type loginData struct {
	CSRFToken string
	Next      string
	Message   string
}

func (a *App) loginPage(w http.ResponseWriter, r *http.Request) {
	if a.validSession(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.renderLogin(w, loginData{CSRFToken: a.csrf, Next: safeNext(r.URL.Query().Get("next"))}, http.StatusOK)
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if !a.validCSRF(w, r) {
		a.renderLogin(w, loginData{CSRFToken: a.csrf, Message: "Page expirée — réessaie."}, http.StatusForbidden)
		return
	}
	password := r.FormValue("password")
	next := safeNext(r.FormValue("next"))
	if subtle.ConstantTimeCompare([]byte(password), []byte(a.config.Password)) != 1 {
		a.renderLogin(w, loginData{CSRFToken: a.csrf, Next: next, Message: "Mot de passe incorrect."}, http.StatusUnauthorized)
		return
	}
	token, err := a.newSession()
	if err != nil {
		http.Error(w, "impossible de créer la session", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  time.Now().Add(a.config.SessionTTL),
		MaxAge:   int(a.config.SessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		a.sessionsMu.Lock()
		delete(a.sessions, cookie.Value)
		a.sessionsMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *App) renderLogin(w http.ResponseWriter, data loginData, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := a.tmpl.ExecuteTemplate(w, "login", data); err != nil {
		log.Printf("rendre la connexion: %v", err)
	}
}

func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	return next
}

func (a *App) newSession() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	now := time.Now()
	a.sessionsMu.Lock()
	for existing, expiry := range a.sessions {
		if !expiry.After(now) {
			delete(a.sessions, existing)
		}
	}
	a.sessions[token] = now.Add(a.config.SessionTTL)
	a.sessionsMu.Unlock()
	return token, nil
}

func (a *App) validSession(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	now := time.Now()
	a.sessionsMu.Lock()
	expiry, ok := a.sessions[cookie.Value]
	if ok && !expiry.After(now) {
		delete(a.sessions, cookie.Value)
		ok = false
	}
	a.sessionsMu.Unlock()
	return ok
}

func (a *App) sessionAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/login" {
			next.ServeHTTP(w, r)
			return
		}
		if !a.validSession(r) {
			if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/ws" {
				http.Error(w, "session requise", http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost && !a.validCSRF(w, r) {
			log.Printf("POST refusé: origin=%q host=%q sec-fetch-site=%q", r.Header.Get("Origin"), r.Host, r.Header.Get("Sec-Fetch-Site"))
			http.Error(w, "jeton CSRF refusé — recharge la page", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) security(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func (a *App) validCSRF(w http.ResponseWriter, r *http.Request) bool {
	provided := r.Header.Get("X-CSRF-Token")
	if provided == "" {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := r.ParseForm(); err != nil {
			return false
		}
		provided = r.FormValue("csrf_token")
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(a.csrf)) == 1
}

func infoFor(id string) (providerInfo, bool) {
	for _, info := range supported {
		if info.ID == id {
			return info, true
		}
	}
	return providerInfo{}, false
}

func oauthProvider(id string) bool {
	return id == "chatgpt" || id == "grok" || id == "copilot"
}

func redirectMessage(w http.ResponseWriter, r *http.Request, message string) {
	http.Redirect(w, r, "/?message="+url.QueryEscape(message), http.StatusSeeOther)
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		log.Printf("encoder JSON: %v", err)
	}
}

const pageTemplate = `
{{define "home"}}<!doctype html>
<html lang="fr"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>ai-usage</title><style>
:root{color-scheme:dark;--bg:#0d1117;--card:#161b22;--line:#30363d;--text:#e6edf3;--muted:#8b949e;--accent:#58a6ff;--ok:#3fb950;--warn:#d29922;--bad:#f85149}*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:15px/1.5 system-ui,sans-serif}.wrap{max-width:1000px;margin:0 auto;padding:32px 20px}header{display:flex;align-items:center;justify-content:space-between;gap:16px;margin-bottom:24px}h1{font-size:24px;margin:0}button,.button{border:1px solid var(--line);border-radius:7px;background:#21262d;color:var(--text);padding:8px 12px;font-weight:600;cursor:pointer;text-decoration:none}button.primary{background:#238636;border-color:#2ea043}button:disabled{cursor:wait;opacity:.6}.controls{display:flex;align-items:center;gap:8px}.controls form{margin:0}.flash{border:1px solid #1f6feb;background:#0d2746;padding:10px 14px;border-radius:7px;margin-bottom:18px}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(290px,1fr));gap:14px}.card{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:17px}.head{display:flex;justify-content:space-between;gap:12px;align-items:flex-start}.head h2{font-size:18px;margin:0}.muted{color:var(--muted);font-size:13px}.status{font-size:12px;border-radius:999px;padding:3px 8px;background:#21262d}.status.ok{color:var(--ok)}.status.error{color:var(--bad)}.status.stale{color:var(--warn)}.window{margin-top:14px}.row{display:flex;justify-content:space-between;gap:8px}.balance{font-size:22px;font-weight:700;color:var(--ok)}.bar{height:8px;background:#30363d;border-radius:99px;overflow:hidden;margin-top:5px}.fill{height:100%;background:var(--accent);max-width:100%}.actions{display:flex;gap:8px;align-items:center;margin-top:16px;flex-wrap:wrap}.actions form{margin:0}.key-form{display:flex;gap:7px;width:100%}.key-form input{min-width:0;flex:1;background:#0d1117;color:var(--text);border:1px solid var(--line);border-radius:7px;padding:8px}.reason{color:var(--warn);font-size:13px;margin-top:12px}footer{margin-top:24px;color:var(--muted);font-size:13px}code{font-family:ui-monospace,monospace}
</style><meta name="csrf-token" content="{{.CSRFToken}}"></head><body><div class="wrap"><header><div><h1>ai-usage</h1><div class="muted">Abonnements IA · homelab</div><div class="muted" id="last-refresh">Dernier refresh : en attente</div></div><div class="controls"><button id="refresh-button" class="primary" type="button" disabled>Actualiser</button><form method="post" action="/logout"><input type="hidden" name="csrf_token" value="{{.CSRFToken}}"><button>Déconnexion</button></form></div></header>
{{if .Message}}<div class="flash">{{.Message}}</div>{{end}}<main class="grid">{{range .Cards}}<section class="card" data-provider="{{.ConnectionID}}"><div class="head"><div><h2>{{.Name}}</h2><div class="muted">{{if .Account}}{{.Account}}{{else}}{{.Hint}}{{end}}</div></div>{{if .Connected}}<span data-role="status" class="status {{if .HasReport}}{{lower .Report.Status}}{{else}}ok{{end}}">{{if .HasReport}}{{.Report.Status}}{{else}}connecté{{end}}</span>{{else}}<span class="status">{{if .AddAccount}}nouveau compte{{else}}déconnecté{{end}}</span>{{end}}</div><div data-role="report">
{{if .HasReport}}{{range .Report.Windows}}<div class="window"><div class="row"><span>{{.Label}}</span>{{if .RemainingAmount}}<span class="balance">{{money .RemainingAmount .Currency}}</span>{{else}}<span>{{if .Unlimited}}illimité{{else}}{{percent .UsedPercent}} %{{end}}</span>{{end}}</div>{{if .RemainingAmount}}{{if .TotalAmount}}<div class="muted">sur {{money .TotalAmount .Currency}} achetés</div>{{end}}{{else}}{{if not .Unlimited}}<div class="bar"><div class="fill" style="width:{{percent .UsedPercent}}%"></div></div>{{end}}{{if .TotalCount}}<div class="muted">{{count .UsedCount}} / {{count .TotalCount}} crédits utilisés</div>{{end}}{{end}}{{if .ResetsAt}}<div class="muted">reset {{clock .ResetsAt}}</div>{{end}}</div>{{end}}{{if .Report.Reason}}<div class="reason">{{.Report.Reason}}</div>{{end}}{{end}}</div>
<div class="actions">{{if .Connected}}<form method="post" action="/disconnect/{{.ConnectionID}}"><input type="hidden" name="csrf_token" value="{{$.CSRFToken}}"><button>Déconnecter</button></form>{{else}}{{if .OAuth}}<form method="post" action="/connect/{{.ID}}"><input type="hidden" name="csrf_token" value="{{$.CSRFToken}}"><button class="primary">{{if .AddAccount}}Ajouter un compte{{else}}Connecter{{end}}</button></form>{{end}}{{if .APIKey}}<form class="key-form" method="post" action="/key/{{.ID}}"><input type="hidden" name="csrf_token" value="{{$.CSRFToken}}"><input name="key" type="password" autocomplete="off" placeholder="Clé API" required><button class="primary">Enregistrer</button></form>{{end}}{{end}}</div></section>{{end}}</main><footer><span id="live-status">Temps réel : connexion…</span> · API JSON : <code>/api/reports</code> · secrets chiffrés dans <code>/data/state.enc</code></footer></div>
<script>(()=>{const token=document.querySelector('meta[name="csrf-token"]').content;const button=document.getElementById('refresh-button');const last=document.getElementById('last-refresh');const live=document.getElementById('live-status');let ws,retry;
const node=(tag,cls,text)=>{const n=document.createElement(tag);if(cls)n.className=cls;if(text!==undefined)n.textContent=text;return n};
const money=(value,currency)=>String(currency).toUpperCase()==='USD'?'$'+Number(value).toFixed(2):Number(value).toFixed(2)+' '+currency;
const when=value=>new Intl.DateTimeFormat('fr-FR',{dateStyle:'short',timeStyle:'medium'}).format(new Date(value));
function renderReport(container,report){container.replaceChildren();for(const w of report.windows||[]){const box=node('div','window');const row=node('div','row');row.append(node('span','',w.label));if(w.remaining_amount!==undefined&&w.remaining_amount!==null){row.append(node('span','balance',money(w.remaining_amount,w.currency)));box.append(row);if(w.total_amount!==undefined&&w.total_amount!==null)box.append(node('div','muted','sur '+money(w.total_amount,w.currency)+' achetés'));}else{row.append(node('span','',w.unlimited?'illimité':Math.round(w.used_percent)+' %'));box.append(row);if(!w.unlimited){const bar=node('div','bar');const fill=node('div','fill');fill.style.width=Math.max(0,Math.min(100,w.used_percent))+'%';bar.append(fill);box.append(bar);}if(w.total_count!==undefined&&w.total_count!==null)box.append(node('div','muted',Math.round(w.used_count)+' / '+Math.round(w.total_count)+' crédits utilisés'));}if(w.resets_at)box.append(node('div','muted','reset '+when(w.resets_at)));container.append(box);}if(report.reason)container.append(node('div','reason',report.reason));}
function update(data){if(data.last_refresh)last.textContent='Dernier refresh : '+when(data.last_refresh);const reports=new Map((data.providers||[]).map(r=>[r.id,r]));for(const card of document.querySelectorAll('[data-provider]')){const report=reports.get(card.dataset.provider);if(!report)continue;const status=card.querySelector('[data-role="status"]');if(status){status.textContent=report.status;status.className='status '+String(report.status).toLowerCase();}const container=card.querySelector('[data-role="report"]');if(container)renderReport(container,report);}button.disabled=false;button.textContent='Actualiser';}
function connect(){const scheme=location.protocol==='https:'?'wss:':'ws:';ws=new WebSocket(scheme+'//'+location.host+'/ws?csrf='+encodeURIComponent(token));ws.onopen=()=>{live.textContent='Temps réel : connecté';button.disabled=false;clearTimeout(retry)};ws.onmessage=event=>{try{const data=JSON.parse(event.data);if(data.type==='reports')update(data)}catch(_){}};ws.onclose=()=>{live.textContent='Temps réel : déconnecté, reconnexion…';button.disabled=true;retry=setTimeout(connect,2000)};ws.onerror=()=>ws.close();}
button.addEventListener('click',()=>{if(ws&&ws.readyState===WebSocket.OPEN){button.disabled=true;button.textContent='Actualisation…';ws.send(JSON.stringify({type:'refresh'}));}});connect();})();</script></body></html>{{end}}
{{define "login"}}<!doctype html><html lang="fr"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Connexion · ai-usage</title><style>:root{color-scheme:dark;--bg:#0d1117;--card:#161b22;--line:#30363d;--text:#e6edf3;--muted:#8b949e;--bad:#f85149}*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:15px/1.5 system-ui,sans-serif;display:grid;place-items:center;min-height:100vh}.box{width:min(390px,calc(100% - 32px));background:var(--card);border:1px solid var(--line);border-radius:12px;padding:28px}h1{margin:0 0 4px;font-size:24px}.muted{color:var(--muted);margin:0 0 22px}.error{color:var(--bad);margin-bottom:14px}label{display:block;margin-bottom:6px;font-weight:600}input{width:100%;background:var(--bg);color:var(--text);border:1px solid var(--line);border-radius:7px;padding:10px;margin-bottom:14px;font:inherit}button{width:100%;border:1px solid #2ea043;border-radius:7px;background:#238636;color:#fff;padding:10px;font-weight:700;cursor:pointer}</style></head><body><main class="box"><h1>ai-usage</h1><p class="muted">Dashboard homelab</p>{{if .Message}}<div class="error">{{.Message}}</div>{{end}}<form method="post" action="/login"><input type="hidden" name="csrf_token" value="{{.CSRFToken}}"><input type="hidden" name="next" value="{{.Next}}"><label for="password">Mot de passe</label><input id="password" name="password" type="password" autocomplete="current-password" autofocus required><button type="submit">Connexion</button></form></main></body></html>{{end}}
{{define "oauth"}}<!doctype html><html lang="fr"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Connexion {{.Provider}}</title><style>body{margin:0;background:#0d1117;color:#e6edf3;font:16px/1.5 system-ui,sans-serif;display:grid;place-items:center;min-height:100vh}.box{width:min(520px,calc(100% - 32px));background:#161b22;border:1px solid #30363d;border-radius:12px;padding:28px;text-align:center}a{color:#58a6ff}.code{font:700 30px ui-monospace,monospace;letter-spacing:3px;background:#0d1117;padding:14px;border-radius:8px;margin:20px 0}.error{color:#f85149}</style></head><body><main class="box"><h1>Connexion {{.Provider}}</h1>{{if eq .Status "error"}}<p class="error">{{.Error}}</p><p><a href="/">Retour</a></p>{{else}}<p>Ouvre <a href="{{.VerificationURL}}" target="_blank" rel="noreferrer">la page d’autorisation</a> puis saisis :</p><div class="code">{{.UserCode}}</div><p>En attente de la confirmation…</p><script>setInterval(async()=>{const r=await fetch('/api/oauth/{{.ID}}');if(!r.ok)return;const s=await r.json();if(s.status==='connected')location.href='/?message='+encodeURIComponent('Connexion '+s.provider+' réussie');if(s.status==='error')location.reload()},2000)</script>{{end}}</main></body></html>{{end}}`
