package provider

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/valche5/ai-usage/internal/credstore"
)

func TestOpenRouterCollectExposesRemainingUSD(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != openrouterCreditsURL {
			t.Fatalf("URL = %s, want %s", r.URL, openrouterCreditsURL)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"data":{"total_credits":25.50,"total_usage":8.16}}`)),
		}, nil
	})}
	loader := func(time.Time) ([]credstore.Cred, error) {
		return []credstore.Cred{{Token: "test-openrouter-key"}}, nil
	}

	report := (OpenRouter{Credentials: loader}).Collect(context.Background(), Options{
		Now:  time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC),
		HTTP: client,
	})
	if report.Status != StatusOK || len(report.Windows) != 1 {
		t.Fatalf("report = %#v", report)
	}
	window := report.Windows[0]
	if window.RemainingAmount == nil || *window.RemainingAmount != 17.34 {
		t.Fatalf("remaining = %v, want 17.34", window.RemainingAmount)
	}
	if window.TotalAmount == nil || *window.TotalAmount != 25.50 || window.Currency != "USD" {
		t.Fatalf("total/currency = %v/%q", window.TotalAmount, window.Currency)
	}
}
