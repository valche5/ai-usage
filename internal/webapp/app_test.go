package webapp

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/valche5/ai-usage/internal/connection"
	"github.com/valche5/ai-usage/internal/httpx"
	"github.com/valche5/ai-usage/internal/oauthflow"
	"github.com/valche5/ai-usage/internal/provider"
)

func testApp(t *testing.T) (*App, *connection.Store) {
	t.Helper()
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32))
	store, err := connection.Open(filepath.Join(t.TempDir(), "data"), key)
	if err != nil {
		t.Fatal(err)
	}
	client := httpx.Client(0)
	client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":"test"}`)),
		}, nil
	})
	oauth := oauthflow.New(oauthflow.DefaultConfig(client), store)
	app, err := New(Config{Password: "correct horse"}, store, oauth, client)
	if err != nil {
		t.Fatal(err)
	}
	return app, store
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func authenticate(t *testing.T, app *App, req *http.Request) {
	t.Helper()
	token, err := app.newSession()
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
}

func TestAuthenticationAndHealth(t *testing.T) {
	app, _ := testApp(t)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	res := httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("health status = %d", res.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	res = httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusSeeOther || !strings.HasPrefix(res.Header().Get("Location"), "/login") || res.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("unauthenticated response = %d, location=%q, authenticate=%q", res.Code, res.Header().Get("Location"), res.Header().Get("WWW-Authenticate"))
	}

	wrong := url.Values{"csrf_token": {app.csrf}, "password": {"wrong"}}
	req = httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(wrong.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res = httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized || !strings.Contains(res.Body.String(), "Mot de passe incorrect") {
		t.Fatalf("wrong password response = %d %q", res.Code, res.Body.String())
	}

	correct := url.Values{"csrf_token": {app.csrf}, "password": {"correct horse"}}
	req = httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(correct.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res = httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusSeeOther || len(res.Result().Cookies()) != 1 {
		t.Fatalf("login response = %d cookies=%v", res.Code, res.Result().Cookies())
	}
	cookie := res.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("unsafe session cookie = %#v", cookie)
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	res = httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("authenticated dashboard status = %d", res.Code)
	}

	logout := url.Values{"csrf_token": {app.csrf}}
	req = httptest.NewRequest(http.MethodPost, "/logout", strings.NewReader(logout.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	res = httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusSeeOther || app.validSession(req) {
		t.Fatalf("logout response/session = %d/%v", res.Code, app.validSession(req))
	}
}

func TestSaveAPIKeyAndNeverExposeIt(t *testing.T) {
	app, store := testApp(t)
	form := url.Values{"key": {"sk-or-v1-super-secret-value"}, "csrf_token": {app.csrf}}
	req := httptest.NewRequest(http.MethodPost, "/key/openrouter", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	authenticate(t, app, req)
	res := httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusSeeOther {
		t.Fatalf("save key status = %d, body = %s", res.Code, res.Body.String())
	}
	if c, ok := store.Get("openrouter"); !ok || c.Access != "sk-or-v1-super-secret-value" {
		t.Fatalf("stored connection = %#v, %v", c, ok)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/reports", nil)
	authenticate(t, app, req)
	res = httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	body, _ := io.ReadAll(res.Result().Body)
	if bytes.Contains(body, []byte("super-secret")) {
		t.Fatal("reports API leaked the API key")
	}
}

func TestPostRejectsMissingCSRF(t *testing.T) {
	app, _ := testApp(t)
	req := httptest.NewRequest(http.MethodPost, "http://ai-usage.local/refresh", nil)
	req.Host = "ai-usage.local"
	req.Header.Set("Origin", "https://evil.invalid")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	authenticate(t, app, req)
	res := httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusForbidden {
		t.Fatalf("foreign origin status = %d, want 403", res.Code)
	}
}

func TestPostAcceptsNullOriginWithCSRF(t *testing.T) {
	app, _ := testApp(t)
	form := url.Values{"csrf_token": {app.csrf}}
	req := httptest.NewRequest(http.MethodPost, "http://internal:8080/refresh", strings.NewReader(form.Encode()))
	req.Host = "internal:8080"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "null")
	authenticate(t, app, req)
	res := httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusSeeOther {
		t.Fatalf("null-origin POST with CSRF status = %d, want 303", res.Code)
	}
}

func TestDashboardDefaultsToReadMode(t *testing.T) {
	app, store := testApp(t)
	if err := store.Put(connection.Connection{ID: "copilot:11", Provider: "copilot", Kind: "oauth", Access: "token", Email: "alice"}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	authenticate(t, app, req)
	res := httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	body := res.Body.String()
	for _, want := range []string{`id="edit-button"`, "Éditer", `body.editing .actions{display:flex}`, `data-add-account`, "Ajouter un compte"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	if strings.Contains(body, `<body class="editing"`) {
		t.Fatal("dashboard started in edit mode")
	}
}

func TestDashboardRendersTypedReportStatus(t *testing.T) {
	app, store := testApp(t)
	reset := time.Now().Add(time.Hour)
	if err := store.Put(connection.Connection{Provider: "openrouter", Kind: "api", Access: "test-token"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutReports([]provider.Report{
		{ID: "openrouter", Name: "OpenRouter", Status: provider.StatusError, Reason: "timed out", Windows: []provider.Window{{Label: "credits", UsedPercent: 42, ResetsAt: &reset}}},
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	authenticate(t, app, req)
	res := httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "</html>") || !strings.Contains(res.Body.String(), "timed out") {
		t.Fatalf("dashboard did not render completely: status=%d body=%q", res.Code, res.Body.String())
	}
}

func TestDashboardRendersOpenRouterDollarBalance(t *testing.T) {
	app, store := testApp(t)
	remaining, total := 17.34, 25.50
	if err := store.Put(connection.Connection{Provider: "openrouter", Kind: "api", Access: "test-token"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutReports([]provider.Report{{
		ID: "openrouter", Name: "OpenRouter", Status: provider.StatusOK,
		Windows: []provider.Window{{
			Label: "crédit restant", UsedPercent: 32,
			RemainingAmount: &remaining, TotalAmount: &total, Currency: "USD",
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	authenticate(t, app, req)
	res := httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	body := res.Body.String()
	if res.Code != http.StatusOK || !strings.Contains(body, "$17.34") || !strings.Contains(body, "$25.50") {
		t.Fatalf("dashboard balance missing: status=%d body=%q", res.Code, body)
	}
}

func TestDashboardRendersAndCollectsMultipleCopilotAccounts(t *testing.T) {
	app, store := testApp(t)
	for _, c := range []connection.Connection{
		{ID: "copilot:11", Provider: "copilot", Kind: "oauth", Access: "token-one", Email: "alice"},
		{ID: "copilot:22", Provider: "copilot", Kind: "oauth", Access: "token-two", Email: "bob"},
	} {
		if err := store.Put(c); err != nil {
			t.Fatal(err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	authenticate(t, app, req)
	res := httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	body := res.Body.String()
	for _, want := range []string{"alice", "bob", `data-provider="copilot:11"`, `data-provider="copilot:22"`, "Ajouter un compte"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}

	reports := app.Collect(t.Context(), true)
	if len(reports) != 2 || reports[0].ID != "copilot:11" || reports[1].ID != "copilot:22" {
		t.Fatalf("multi-account reports = %#v", reports)
	}

	form := url.Values{"csrf_token": {app.csrf}}
	req = httptest.NewRequest(http.MethodPost, "/disconnect/copilot:11", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	authenticate(t, app, req)
	res = httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusSeeOther {
		t.Fatalf("disconnect status = %d", res.Code)
	}
	if _, ok := store.Get("copilot:11"); ok {
		t.Fatal("first Copilot account still connected")
	}
	if _, ok := store.Get("copilot:22"); !ok {
		t.Fatal("second Copilot account was removed too")
	}
}

func TestDashboardRendersCopilotCreditCounts(t *testing.T) {
	app, store := testApp(t)
	used, total := 55.0, 300.0
	if err := store.Put(connection.Connection{ID: "copilot:42", Provider: "copilot", Kind: "oauth", Access: "token", Email: "octocat"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutReports([]provider.Report{{
		ID: "copilot:42", Name: "Copilot", Status: provider.StatusOK,
		Windows: []provider.Window{{Label: "premium", UsedPercent: 18.3, UsedCount: &used, TotalCount: &total}},
	}}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	authenticate(t, app, req)
	res := httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	body := res.Body.String()
	if res.Code != http.StatusOK || !strings.Contains(body, "18 %") || !strings.Contains(body, "55 / 300 crédits utilisés") {
		t.Fatalf("Copilot credit counts missing: status=%d body=%q", res.Code, body)
	}
}

func TestWebSocketDrivesCollectionAndManualRefresh(t *testing.T) {
	app, _ := testApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app.Run(ctx)
	server := httptest.NewServer(app.Handler())
	defer server.Close()

	app.lastRefreshMu.RLock()
	before := app.lastRefresh
	app.lastRefreshMu.RUnlock()
	if !before.IsZero() {
		t.Fatalf("collection ran without a client: %v", before)
	}

	token, err := app.newSession()
	if err != nil {
		t.Fatal(err)
	}
	header := http.Header{}
	header.Set("Cookie", (&http.Cookie{Name: sessionCookieName, Value: token}).String())
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws?csrf=" + url.QueryEscape(app.csrf)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	var snapshot reportsPayload
	for snapshot.LastRefresh == nil {
		if err := conn.ReadJSON(&snapshot); err != nil {
			t.Fatal(err)
		}
	}
	firstRefresh := *snapshot.LastRefresh
	if snapshot.Type != "reports" || app.clientCount() != 1 {
		t.Fatalf("snapshot/client count = %#v/%d", snapshot, app.clientCount())
	}

	if err := conn.WriteJSON(map[string]string{"type": "refresh"}); err != nil {
		t.Fatal(err)
	}
	snapshot = reportsPayload{}
	if err := conn.ReadJSON(&snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.LastRefresh == nil || snapshot.LastRefresh.Before(firstRefresh) {
		t.Fatalf("manual refresh timestamp = %v, first = %v", snapshot.LastRefresh, firstRefresh)
	}

	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for app.clientCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if count := app.clientCount(); count != 0 {
		t.Fatalf("client count after close = %d", count)
	}
}
