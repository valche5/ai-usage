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

func TestCopilotShowsOnlyPremiumWithUsedAndTotalCredits(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		body := `{
          "copilot_plan":"individual_pro",
          "login":"octocat",
          "quota_reset_date":"2026-10-01",
          "quota_snapshots":{
            "premium_interactions":{"entitlement":300,"remaining":245,"percent_remaining":81.6666667},
            "chat":{"unlimited":true,"percent_remaining":100},
            "completions":{"unlimited":true,"percent_remaining":100}
          }
        }`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	loader := func(time.Time) ([]credstore.Cred, error) {
		return []credstore.Cred{{Token: "github-token"}}, nil
	}

	report := (Copilot{Credentials: loader}).Collect(context.Background(), Options{Now: time.Now(), HTTP: client})
	if report.Status != StatusOK || len(report.Windows) != 1 {
		t.Fatalf("report = %#v", report)
	}
	w := report.Windows[0]
	if w.Key != "premium_interactions" || w.UsedCount == nil || *w.UsedCount != 55 || w.TotalCount == nil || *w.TotalCount != 300 {
		t.Fatalf("premium window = %#v", w)
	}
}

func TestCopilotFreeTierPremiumCounts(t *testing.T) {
	windows, _ := copilotWindows(copilotResponse{
		MonthlyQuotas:     map[string]float64{"premium_interactions": 50, "chat": 2000},
		LimitedUserQuotas: map[string]float64{"premium_interactions": 37, "chat": 1999},
	})
	if len(windows) != 1 || windows[0].Key != "premium_interactions" {
		t.Fatalf("windows = %#v", windows)
	}
	w := windows[0]
	if w.UsedCount == nil || *w.UsedCount != 13 || w.TotalCount == nil || *w.TotalCount != 50 || w.UsedPercent != 26 {
		t.Fatalf("free premium window = %#v", w)
	}
}
