package oauthflow

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valche5/ai-usage/internal/connection"
)

func oauthTestStore(t *testing.T) *connection.Store {
	t.Helper()
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
	store, err := connection.Open(filepath.Join(t.TempDir(), "data"), key)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func fakeJWT(claims map[string]any) string {
	payload, _ := json.Marshal(claims)
	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func waitConnected(t *testing.T, manager *Manager, id string) Session {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if session, ok := manager.Session(id); ok && session.Status != "pending" {
			return session
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("OAuth session did not finish")
	return Session{}
}

func TestChatGPTDeviceFlow(t *testing.T) {
	access := fakeJWT(map[string]any{
		"email":                       "me@example.com",
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-1", "chatgpt_plan_type": "plus"},
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			_, _ = w.Write([]byte(`{"device_auth_id":"dev-1","user_code":"ABCD-EFGH","interval":"1"}`))
		case "/api/accounts/deviceauth/token":
			_, _ = w.Write([]byte(`{"authorization_code":"code-1","code_verifier":"verifier-1"}`))
		case "/oauth/token":
			_, _ = w.Write([]byte(`{"access_token":"` + access + `","refresh_token":"refresh-1","expires_in":3600}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	store := oauthTestStore(t)
	config := DefaultConfig(server.Client())
	config.ChatGPTIssuer = server.URL
	manager := New(config, store)
	session, err := manager.Start(t.Context(), ProviderChatGPT)
	if err != nil {
		t.Fatal(err)
	}
	if session.UserCode != "ABCD-EFGH" {
		t.Fatalf("user code = %q", session.UserCode)
	}
	if got := waitConnected(t, manager, session.ID); got.Status != "connected" {
		t.Fatalf("status = %s, error = %s", got.Status, got.Error)
	}
	c, ok := store.Get(ProviderChatGPT)
	if !ok || c.Access != access || c.Refresh != "refresh-1" || c.AccountID != "acct-1" || c.Email != "me@example.com" {
		t.Fatalf("stored connection = %#v, %v", c, ok)
	}
}

func TestXAIDeviceFlowAndRotatingRefresh(t *testing.T) {
	var refreshes atomic.Int32
	access := fakeJWT(map[string]any{"email": "grok@example.com", "sub": "grok-user"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth2/device/code":
			_, _ = w.Write([]byte(`{"device_code":"dev-x","user_code":"XAI-123","verification_uri":"https://verify.invalid","expires_in":300,"interval":1}`))
		case "/oauth2/token":
			_ = r.ParseForm()
			if r.FormValue("grant_type") == "refresh_token" {
				refreshes.Add(1)
				_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"refresh-2","expires_in":3600}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"` + access + `","refresh_token":"refresh-1","expires_in":1}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	store := oauthTestStore(t)
	config := DefaultConfig(server.Client())
	config.XAIAuthBase = server.URL
	manager := New(config, store)
	session, err := manager.Start(t.Context(), ProviderGrok)
	if err != nil {
		t.Fatal(err)
	}
	if got := waitConnected(t, manager, session.ID); got.Status != "connected" {
		t.Fatalf("status = %s, error = %s", got.Status, got.Error)
	}
	c, err := manager.Fresh(t.Context(), ProviderGrok)
	if err != nil {
		t.Fatal(err)
	}
	if refreshes.Load() != 1 || c.Access != "new-access" || c.Refresh != "refresh-2" || c.Email != "grok@example.com" {
		t.Fatalf("refreshed connection = %#v; refreshes = %d", c, refreshes.Load())
	}
}

func TestGitHubDeviceFlow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/login/device/code":
			_, _ = w.Write([]byte(`{"device_code":"dev-gh","user_code":"GH-123","verification_uri":"https://github.invalid/device","expires_in":900,"interval":1}`))
		case "/login/oauth/access_token":
			_, _ = w.Write([]byte(`{"access_token":"gho_test_token"}`))
		case "/user":
			_, _ = w.Write([]byte(`{"id":42,"login":"octocat"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	store := oauthTestStore(t)
	config := DefaultConfig(server.Client())
	config.GitHubBase = server.URL
	config.GitHubAPI = server.URL
	manager := New(config, store)
	session, err := manager.Start(t.Context(), ProviderCopilot)
	if err != nil {
		t.Fatal(err)
	}
	if got := waitConnected(t, manager, session.ID); got.Status != "connected" {
		t.Fatalf("status = %s, error = %s", got.Status, got.Error)
	}
	c, ok := store.Get("copilot:42")
	if !ok || c.Access != "gho_test_token" || c.Refresh != "" || c.Provider != ProviderCopilot || c.Email != "octocat" {
		t.Fatalf("stored connection = %#v, %v", c, ok)
	}
}

func TestMigrateLegacyCopilot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" || r.Header.Get("Authorization") != "Bearer legacy-token" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":7,"login":"legacy-user"}`))
	}))
	defer server.Close()
	store := oauthTestStore(t)
	if err := store.Put(connection.Connection{Provider: ProviderCopilot, Kind: "oauth", Access: "legacy-token"}); err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig(server.Client())
	config.GitHubAPI = server.URL
	manager := New(config, store)
	if err := manager.MigrateLegacyCopilot(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Get(ProviderCopilot); ok {
		t.Fatal("legacy slot still exists")
	}
	if got, ok := store.Get("copilot:7"); !ok || got.Email != "legacy-user" || got.Access != "legacy-token" {
		t.Fatalf("migrated connection = %#v, %v", got, ok)
	}
}
