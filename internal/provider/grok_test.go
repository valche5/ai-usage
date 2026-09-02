package provider

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const grokBillingFixture = `{
  "config": {
    "currentPeriod": {
      "type": "USAGE_PERIOD_TYPE_WEEKLY",
      "start": "2026-09-01T19:36:58.503838+00:00",
      "end": "2026-09-08T19:36:58.503838+00:00"
    },
    "creditUsagePercent": 1.0,
    "productUsage": [{"product":"GrokBuild","usagePercent":1.0}],
    "billingPeriodEnd": "2026-09-08T19:36:58.503838+00:00"
  }
}`

func TestParseGrokBilling(t *testing.T) {
	got, err := parseGrokBilling([]byte(grokBillingFixture))
	if err != nil {
		t.Fatal(err)
	}
	if got.UsagePercent != 1 {
		t.Fatalf("usage = %v, want 1", got.UsagePercent)
	}
	want := time.Date(2026, 9, 8, 19, 36, 58, 503838000, time.UTC)
	if got.ResetAt == nil || !got.ResetAt.Equal(want) {
		t.Fatalf("reset = %v, want %v", got.ResetAt, want)
	}
}

func TestParseGrokBillingAcceptsOmittedZeroUsage(t *testing.T) {
	got, err := parseGrokBilling([]byte(`{"config":{"currentPeriod":{"end":"2026-09-08T19:36:58Z"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.UsagePercent != 0 {
		t.Fatalf("usage = %v, want 0", got.UsagePercent)
	}
}

func TestParseGrokBillingRejectsMissingConfig(t *testing.T) {
	if _, err := parseGrokBilling([]byte(`{}`)); err == nil {
		t.Fatal("expected missing config error")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGrokCollectUsesCLIEndpoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, ".grok")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	auth := `{"https://auth.x.ai::test":{"key":"opaque-test-token","expires_at":"2027-01-01T00:00:00Z","email":"user@example.com","user_id":"user-id"}}`
	if err := os.WriteFile(filepath.Join(authDir, "auth.json"), []byte(auth), 0o600); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.String() != grokUsageURL {
			t.Errorf("URL = %s, want %s", r.URL, grokUsageURL)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer opaque-test-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("X-XAI-Token-Auth"); got != "xai-grok-cli" {
			t.Errorf("X-XAI-Token-Auth = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(grokBillingFixture)),
			Header:     make(http.Header),
		}, nil
	})}

	now := time.Date(2026, 9, 2, 12, 50, 9, 0, time.UTC)
	report := (Grok{}).Collect(context.Background(), Options{Now: now, HTTP: client})
	if report.Status != StatusOK {
		t.Fatalf("status = %s, reason = %s", report.Status, report.Reason)
	}
	if len(report.Windows) != 1 || report.Windows[0].Label != "weekly" || report.Windows[0].UsedPercent != 1 {
		t.Fatalf("windows = %#v", report.Windows)
	}
}
