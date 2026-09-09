// Package oauthflow implements the headless OAuth device flows used by the
// homelab service. The protocol shapes follow OpenCode's MIT-licensed provider
// plugins; endpoints and client IDs are configurable so a future registered
// application can replace the public-client defaults without code changes.
package oauthflow

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/valche5/ai-usage/internal/connection"
	"github.com/valche5/ai-usage/internal/credstore"
	"github.com/valche5/ai-usage/internal/httpx"
)

const (
	ProviderChatGPT = "chatgpt"
	ProviderGrok    = "grok"
	ProviderCopilot = "copilot"

	deviceGrant = "urn:ietf:params:oauth:grant-type:device_code"
)

type Config struct {
	HTTP *http.Client

	ChatGPTIssuer   string
	ChatGPTClientID string
	XAIAuthBase     string
	XAIClientID     string
	GitHubBase      string
	GitHubAPI       string
	GitHubClientID  string
}

func DefaultConfig(client *http.Client) Config {
	return Config{
		HTTP:            client,
		ChatGPTIssuer:   "https://auth.openai.com",
		ChatGPTClientID: "app_EMoamEEZ73f0CkXaXp7hrann",
		XAIAuthBase:     "https://auth.x.ai",
		XAIClientID:     "b1a00492-073a-47ea-816f-4c329264a828",
		GitHubBase:      "https://github.com",
		GitHubAPI:       "https://api.github.com",
		GitHubClientID:  "Iv1.b507a08c87ecfe98",
	}
}

type Session struct {
	ID              string    `json:"id"`
	Provider        string    `json:"provider"`
	VerificationURL string    `json:"verification_url"`
	UserCode        string    `json:"user_code"`
	Status          string    `json:"status"` // pending, connected, error
	Error           string    `json:"error,omitempty"`
	ExpiresAt       time.Time `json:"expires_at"`
}

type deviceSecret struct {
	DeviceCode string
	Interval   time.Duration
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
}

type pending struct {
	Session
	secret deviceSecret
	cancel context.CancelFunc
}

// Manager owns in-flight device flows and serializes refresh-token rotation.
type Manager struct {
	mu       sync.RWMutex
	refresh  sync.Mutex
	config   Config
	store    *connection.Store
	sessions map[string]*pending
}

func New(config Config, store *connection.Store) *Manager {
	return &Manager{config: config, store: store, sessions: map[string]*pending{}}
}

func (m *Manager) Start(ctx context.Context, provider string) (Session, error) {
	var session Session
	var secret deviceSecret
	var err error
	switch provider {
	case ProviderChatGPT:
		session, secret, err = m.startChatGPT(ctx)
	case ProviderGrok:
		session, secret, err = m.startXAI(ctx)
	case ProviderCopilot:
		session, secret, err = m.startGitHub(ctx)
	default:
		return Session{}, fmt.Errorf("OAuth indisponible pour %q", provider)
	}
	if err != nil {
		return Session{}, err
	}
	session.ID, err = randomID()
	if err != nil {
		return Session{}, err
	}
	session.Provider = provider
	session.Status = "pending"
	flowCtx, cancel := context.WithDeadline(context.Background(), session.ExpiresAt)
	p := &pending{Session: session, secret: secret, cancel: cancel}
	m.mu.Lock()
	for id, old := range m.sessions {
		if old.Provider == provider && old.Status == "pending" {
			old.cancel()
			delete(m.sessions, id)
		}
	}
	m.sessions[session.ID] = p
	m.mu.Unlock()
	go m.poll(flowCtx, p)
	return session, nil
}

func (m *Manager) Session(id string) (Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.sessions[id]
	if !ok {
		return Session{}, false
	}
	return p.Session, true
}

func (m *Manager) poll(ctx context.Context, p *pending) {
	defer p.cancel()
	var c connection.Connection
	var err error
	switch p.Provider {
	case ProviderChatGPT:
		c, err = m.pollChatGPT(ctx, p.secret)
	case ProviderGrok:
		c, err = m.pollXAI(ctx, p.secret)
	case ProviderCopilot:
		c, err = m.pollGitHub(ctx, p.secret)
	}
	if err == nil && c.Provider == ProviderCopilot {
		_ = m.MigrateLegacyCopilot(ctx)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.sessions[p.ID]
	if !ok || current != p {
		return
	}
	if err == nil {
		// Coordinate with Fresh so a late device-flow completion cannot race a
		// rotating refresh-token write for the same provider.
		m.refresh.Lock()
		err = m.store.Replace(c)
		m.refresh.Unlock()
	}
	if err != nil {
		current.Status = "error"
		current.Error = httpx.Redact(err.Error())
		return
	}
	current.Status = "connected"
}

// Cancel stops in-flight authorization for provider. It is used when a user
// disconnects before completing a device flow.
func (m *Manager) Cancel(provider string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, p := range m.sessions {
		if p.Provider == provider && p.Status == "pending" {
			p.cancel()
			delete(m.sessions, id)
		}
	}
}

func (m *Manager) startChatGPT(ctx context.Context) (Session, deviceSecret, error) {
	var data struct {
		DeviceAuthID string `json:"device_auth_id"`
		UserCode     string `json:"user_code"`
		Interval     any    `json:"interval"`
	}
	body, _ := json.Marshal(map[string]string{"client_id": m.config.ChatGPTClientID})
	err := m.postJSON(ctx, m.config.ChatGPTIssuer+"/api/accounts/deviceauth/usercode", body, &data)
	if err != nil {
		return Session{}, deviceSecret{}, fmt.Errorf("démarrer OAuth ChatGPT: %w", err)
	}
	if data.DeviceAuthID == "" || data.UserCode == "" {
		return Session{}, deviceSecret{}, errors.New("réponse device OAuth ChatGPT incomplète")
	}
	return Session{
		VerificationURL: m.config.ChatGPTIssuer + "/codex/device",
		UserCode:        data.UserCode,
		ExpiresAt:       time.Now().Add(10 * time.Minute),
	}, deviceSecret{DeviceCode: data.DeviceAuthID + "\x00" + data.UserCode, Interval: seconds(data.Interval, 5) + 3*time.Second}, nil
}

func (m *Manager) pollChatGPT(ctx context.Context, secret deviceSecret) (connection.Connection, error) {
	parts := strings.SplitN(secret.DeviceCode, "\x00", 2)
	if len(parts) != 2 {
		return connection.Connection{}, errors.New("état OAuth ChatGPT invalide")
	}
	for {
		body, _ := json.Marshal(map[string]string{"device_auth_id": parts[0], "user_code": parts[1]})
		var code struct {
			AuthorizationCode string `json:"authorization_code"`
			CodeVerifier      string `json:"code_verifier"`
		}
		status, err := m.postJSONStatus(ctx, m.config.ChatGPTIssuer+"/api/accounts/deviceauth/token", body, &code)
		if err == nil && status >= 200 && status < 300 {
			if code.AuthorizationCode == "" || code.CodeVerifier == "" {
				return connection.Connection{}, errors.New("réponse OAuth ChatGPT incomplète")
			}
			values := url.Values{
				"grant_type":    {"authorization_code"},
				"code":          {code.AuthorizationCode},
				"redirect_uri":  {m.config.ChatGPTIssuer + "/deviceauth/callback"},
				"client_id":     {m.config.ChatGPTClientID},
				"code_verifier": {code.CodeVerifier},
			}
			tokens, err := m.token(ctx, m.config.ChatGPTIssuer+"/oauth/token", values)
			if err != nil {
				return connection.Connection{}, fmt.Errorf("échanger le code ChatGPT: %w", err)
			}
			return openAIConnection(tokens), nil
		}
		if err != nil && status != http.StatusForbidden && status != http.StatusNotFound {
			return connection.Connection{}, fmt.Errorf("attendre OAuth ChatGPT: %w", err)
		}
		if err := sleep(ctx, secret.Interval); err != nil {
			return connection.Connection{}, errors.New("autorisation ChatGPT expirée")
		}
	}
}

func (m *Manager) startXAI(ctx context.Context) (Session, deviceSecret, error) {
	values := url.Values{
		"client_id": {m.config.XAIClientID},
		"scope":     {"openid profile email offline_access grok-cli:access api:access"},
		"referrer":  {"opencode"},
	}
	var data struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               any    `json:"expires_in"`
		Interval                any    `json:"interval"`
	}
	if err := m.postForm(ctx, m.config.XAIAuthBase+"/oauth2/device/code", values, &data); err != nil {
		return Session{}, deviceSecret{}, fmt.Errorf("démarrer OAuth xAI: %w", err)
	}
	if data.DeviceCode == "" || data.UserCode == "" || data.VerificationURI == "" {
		return Session{}, deviceSecret{}, errors.New("réponse device OAuth xAI incomplète")
	}
	verification := data.VerificationURIComplete
	if verification == "" {
		verification = data.VerificationURI
	}
	return Session{
		VerificationURL: verification,
		UserCode:        data.UserCode,
		ExpiresAt:       time.Now().Add(seconds(data.ExpiresIn, 300)),
	}, deviceSecret{DeviceCode: data.DeviceCode, Interval: seconds(data.Interval, 5)}, nil
}

func (m *Manager) pollXAI(ctx context.Context, secret deviceSecret) (connection.Connection, error) {
	interval := secret.Interval
	for {
		values := url.Values{
			"grant_type":  {deviceGrant},
			"client_id":   {m.config.XAIClientID},
			"device_code": {secret.DeviceCode},
		}
		tokens, status, err := m.tokenStatus(ctx, m.config.XAIAuthBase+"/oauth2/token", values)
		if err == nil {
			return xaiConnection(tokens), nil
		}
		switch tokens.Error {
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		case "access_denied", "authorization_denied":
			return connection.Connection{}, errors.New("autorisation xAI refusée")
		case "expired_token":
			return connection.Connection{}, errors.New("code OAuth xAI expiré")
		default:
			return connection.Connection{}, fmt.Errorf("OAuth xAI HTTP %d: %s", status, safeOAuthError(tokens))
		}
		if err := sleep(ctx, interval+3*time.Second); err != nil {
			return connection.Connection{}, errors.New("autorisation xAI expirée")
		}
	}
}

func (m *Manager) startGitHub(ctx context.Context) (Session, deviceSecret, error) {
	values := url.Values{"client_id": {m.config.GitHubClientID}, "scope": {"read:user"}}
	var data struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       any    `json:"expires_in"`
		Interval        any    `json:"interval"`
	}
	if err := m.postForm(ctx, m.config.GitHubBase+"/login/device/code", values, &data); err != nil {
		return Session{}, deviceSecret{}, fmt.Errorf("démarrer OAuth GitHub: %w", err)
	}
	if data.DeviceCode == "" || data.UserCode == "" || data.VerificationURI == "" {
		return Session{}, deviceSecret{}, errors.New("réponse device OAuth GitHub incomplète")
	}
	return Session{
		VerificationURL: data.VerificationURI,
		UserCode:        data.UserCode,
		ExpiresAt:       time.Now().Add(seconds(data.ExpiresIn, 900)),
	}, deviceSecret{DeviceCode: data.DeviceCode, Interval: seconds(data.Interval, 5)}, nil
}

func (m *Manager) pollGitHub(ctx context.Context, secret deviceSecret) (connection.Connection, error) {
	for {
		values := url.Values{
			"client_id":   {m.config.GitHubClientID},
			"device_code": {secret.DeviceCode},
			"grant_type":  {deviceGrant},
		}
		tokens, status, err := m.tokenStatus(ctx, m.config.GitHubBase+"/login/oauth/access_token", values)
		if err == nil {
			connectionID := ProviderCopilot + ":" + credstore.Fingerprint(tokens.AccessToken)
			c := connection.Connection{ID: connectionID, Provider: ProviderCopilot, Kind: "oauth", Access: tokens.AccessToken}
			if id, login, profileErr := m.githubProfile(ctx, tokens.AccessToken); profileErr == nil {
				c.ID = ProviderCopilot + ":" + id
				c.AccountID = id
				c.Email = login
			}
			return c, nil
		}
		switch tokens.Error {
		case "authorization_pending":
		case "slow_down":
			secret.Interval += 5 * time.Second
		case "access_denied":
			return connection.Connection{}, errors.New("autorisation GitHub refusée")
		case "expired_token":
			return connection.Connection{}, errors.New("code OAuth GitHub expiré")
		default:
			return connection.Connection{}, fmt.Errorf("OAuth GitHub HTTP %d: %s", status, safeOAuthError(tokens))
		}
		if err := sleep(ctx, secret.Interval); err != nil {
			return connection.Connection{}, errors.New("autorisation GitHub expirée")
		}
	}
}

func (m *Manager) githubProfile(ctx context.Context, token string) (string, string, error) {
	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	_, err := httpx.JSON(ctx, m.config.HTTP, httpx.Req{
		URL: m.config.GitHubAPI + "/user",
		Headers: map[string]string{
			"Authorization":        "Bearer " + token,
			"Accept":               "application/vnd.github+json",
			"User-Agent":           "ai-usage",
			"X-GitHub-Api-Version": "2022-11-28",
		},
	}, &user)
	if err != nil {
		return "", "", err
	}
	if user.ID <= 0 || user.Login == "" {
		return "", "", errors.New("profil GitHub incomplet")
	}
	return strconv.FormatInt(user.ID, 10), user.Login, nil
}

// MigrateLegacyCopilot identifies and atomically rekeys the historical single
// Copilot slot. A profile failure leaves the encrypted entry untouched.
func (m *Manager) MigrateLegacyCopilot(ctx context.Context) error {
	m.refresh.Lock()
	defer m.refresh.Unlock()
	legacy, ok := m.store.Get(ProviderCopilot)
	if !ok || legacy.ID != "" {
		return nil
	}
	id, login, err := m.githubProfile(ctx, legacy.Access)
	if err != nil {
		return fmt.Errorf("identifier l'ancien compte Copilot: %w", err)
	}
	legacy.ID = ProviderCopilot + ":" + id
	legacy.AccountID = id
	legacy.Email = login
	return m.store.Rekey(ProviderCopilot, legacy)
}

// Fresh returns a usable connection, atomically refreshing rotating OAuth
// tokens when they are close to expiry.
func (m *Manager) Fresh(ctx context.Context, provider string) (connection.Connection, error) {
	m.refresh.Lock()
	defer m.refresh.Unlock()
	c, ok := m.store.Get(provider)
	if !ok {
		return connection.Connection{}, credstore.ErrMissing
	}
	if c.Kind != "oauth" || c.ExpiresAt.IsZero() || time.Now().Add(time.Minute).Before(c.ExpiresAt) {
		return c, nil
	}
	if c.Refresh == "" {
		return connection.Connection{}, errors.New("session expirée sans refresh token — reconnecte le provider")
	}
	values := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {c.Refresh}}
	var endpoint string
	switch c.Provider {
	case ProviderChatGPT:
		values.Set("client_id", m.config.ChatGPTClientID)
		endpoint = m.config.ChatGPTIssuer + "/oauth/token"
	case ProviderGrok:
		values.Set("client_id", m.config.XAIClientID)
		endpoint = m.config.XAIAuthBase + "/oauth2/token"
	default:
		return c, nil
	}
	tokens, err := m.token(ctx, endpoint, values)
	if err != nil {
		return connection.Connection{}, fmt.Errorf("renouveler %s: %w", provider, err)
	}
	oldConnection := c
	oldRefresh := c.Refresh
	if oldConnection.Provider == ProviderChatGPT {
		c = openAIConnection(tokens)
	} else {
		c = xaiConnection(tokens)
	}
	c.ID = oldConnection.ID
	if c.Refresh == "" {
		c.Refresh = oldRefresh
	}
	if c.AccountID == "" {
		c.AccountID = oldConnection.AccountID
	}
	if c.Email == "" {
		c.Email = oldConnection.Email
	}
	if c.Plan == "" {
		c.Plan = oldConnection.Plan
	}
	if err := m.store.Put(c); err != nil {
		return connection.Connection{}, err
	}
	return c, nil
}

func openAIConnection(tokens tokenResponse) connection.Connection {
	c := connection.Connection{
		Provider:  ProviderChatGPT,
		Kind:      "oauth",
		Access:    tokens.AccessToken,
		Refresh:   tokens.RefreshToken,
		ExpiresAt: time.Now().Add(expires(tokens.ExpiresIn)),
	}
	claims := firstClaims(tokens.IDToken, tokens.AccessToken)
	c.Email = claimString(claims, "email")
	c.AccountID = claimString(claims, "chatgpt_account_id")
	if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if c.AccountID == "" {
			c.AccountID = claimString(auth, "chatgpt_account_id")
		}
		c.Plan = claimString(auth, "chatgpt_plan_type")
	}
	if profile, ok := claims["https://api.openai.com/profile"].(map[string]any); ok && c.Email == "" {
		c.Email = claimString(profile, "email")
	}
	return c
}

func xaiConnection(tokens tokenResponse) connection.Connection {
	claims := firstClaims(tokens.IDToken, tokens.AccessToken)
	accountID := claimString(claims, "principal_id")
	if accountID == "" {
		accountID = claimString(claims, "sub")
	}
	return connection.Connection{
		Provider:  ProviderGrok,
		Kind:      "oauth",
		Access:    tokens.AccessToken,
		Refresh:   tokens.RefreshToken,
		ExpiresAt: time.Now().Add(expires(tokens.ExpiresIn)),
		AccountID: accountID,
		Email:     claimString(claims, "email"),
	}
}

func firstClaims(tokens ...string) map[string]any {
	for _, token := range tokens {
		if claims, err := credstore.DecodeJWT(token); err == nil {
			return claims
		}
	}
	return map[string]any{}
}

func claimString(claims map[string]any, key string) string {
	value, _ := claims[key].(string)
	return value
}

func expires(seconds int64) time.Duration {
	if seconds <= 0 {
		seconds = 3600
	}
	return time.Duration(seconds) * time.Second
}

func seconds(value any, fallback int64) time.Duration {
	var n int64
	switch value := value.(type) {
	case float64:
		n = int64(value)
	case json.Number:
		n, _ = value.Int64()
	case string:
		n, _ = strconv.ParseInt(value, 10, 64)
	}
	if n <= 0 {
		n = fallback
	}
	return time.Duration(n) * time.Second
}

func safeOAuthError(tokens tokenResponse) string {
	if tokens.Error != "" {
		return httpx.Redact(tokens.Error)
	}
	// Descriptions are deliberately not surfaced: some OAuth servers echo
	// submitted values in diagnostics, and refresh tokens can be opaque strings
	// that no pattern-based redactor can recognize reliably.
	return "réponse inattendue"
}

func randomID() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (m *Manager) postJSON(ctx context.Context, endpoint string, body []byte, out any) error {
	status, err := m.postJSONStatus(ctx, endpoint, body, out)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("HTTP %d", status)
	}
	return nil
}

func (m *Manager) postJSONStatus(ctx context.Context, endpoint string, body []byte, out any) (int, error) {
	res, err := httpx.Do(ctx, m.config.HTTP, httpx.Req{
		Method: http.MethodPost,
		URL:    endpoint,
		Headers: map[string]string{
			"Content-Type": "application/json",
			"Accept":       "application/json",
			"User-Agent":   "ai-usage-web/1",
		},
		Body: body,
	})
	if err != nil {
		return 0, err
	}
	if res.Status >= 200 && res.Status < 300 {
		if err := json.Unmarshal(res.Body, out); err != nil {
			return res.Status, errors.New("réponse JSON invalide")
		}
		return res.Status, nil
	}
	return res.Status, fmt.Errorf("HTTP %d", res.Status)
}

func (m *Manager) postForm(ctx context.Context, endpoint string, values url.Values, out any) error {
	res, err := httpx.Do(ctx, m.config.HTTP, httpx.Req{
		Method:  http.MethodPost,
		URL:     endpoint,
		Headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json", "User-Agent": "ai-usage-web/1"},
		Body:    []byte(values.Encode()),
	})
	if err != nil {
		return err
	}
	if res.Status < 200 || res.Status > 299 {
		return httpx.HTTPError{Status: res.Status, Excerpt: res.Excerpt}
	}
	if err := json.Unmarshal(res.Body, out); err != nil {
		return errors.New("réponse JSON invalide")
	}
	return nil
}

func (m *Manager) token(ctx context.Context, endpoint string, values url.Values) (tokenResponse, error) {
	tokens, status, err := m.tokenStatus(ctx, endpoint, values)
	if err != nil {
		return tokenResponse{}, err
	}
	if status < 200 || status > 299 || tokens.AccessToken == "" {
		return tokenResponse{}, fmt.Errorf("HTTP %d: %s", status, safeOAuthError(tokens))
	}
	return tokens, nil
}

func (m *Manager) tokenStatus(ctx context.Context, endpoint string, values url.Values) (tokenResponse, int, error) {
	return m.form(ctx, endpoint, values)
}

func (m *Manager) form(ctx context.Context, endpoint string, values url.Values) (tokenResponse, int, error) {
	res, err := httpx.Do(ctx, m.config.HTTP, httpx.Req{
		Method: http.MethodPost,
		URL:    endpoint,
		Headers: map[string]string{
			"Content-Type": "application/x-www-form-urlencoded",
			"Accept":       "application/json",
			"User-Agent":   "ai-usage-web/1",
		},
		Body: []byte(values.Encode()),
	})
	if err != nil {
		return tokenResponse{}, 0, err
	}
	var tokens tokenResponse
	if err := json.Unmarshal(res.Body, &tokens); err != nil {
		return tokenResponse{}, res.Status, errors.New("réponse JSON invalide")
	}
	if res.Status < 200 || res.Status > 299 {
		return tokens, res.Status, errors.New(safeOAuthError(tokens))
	}
	if tokens.AccessToken == "" {
		return tokens, res.Status, errors.New(safeOAuthError(tokens))
	}
	return tokens, res.Status, nil
}
