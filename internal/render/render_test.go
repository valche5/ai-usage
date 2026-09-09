package render

import (
	"strings"
	"testing"

	"github.com/valche5/ai-usage/internal/provider"
)

func TestOpenRouterUsesDollarBalance(t *testing.T) {
	remaining, total := 17.34, 25.50
	reports := []provider.Report{{
		ID: "openrouter", Name: "OpenRouter", Status: provider.StatusOK,
		Windows: []provider.Window{{
			Label: "crédit restant", UsedPercent: 32,
			RemainingAmount: &remaining, TotalAmount: &total, Currency: "USD",
		}},
	}}

	table := Table(reports, Opts{})
	if !strings.Contains(table, "$17.34 restant sur $25.50") || strings.Contains(table, "32%") {
		t.Fatalf("unexpected table: %q", table)
	}
	if short := Short(reports, Opts{}); !strings.Contains(short, "$17.34") || strings.Contains(short, "32%") {
		t.Fatalf("unexpected short output: %q", short)
	}
}

func TestCopilotShowsPercentAndCreditCounts(t *testing.T) {
	used, total := 55.0, 300.0
	reports := []provider.Report{{
		ID: "copilot:42", Name: "Copilot", Status: provider.StatusOK,
		Windows: []provider.Window{{
			Label: "premium", UsedPercent: 18.3, UsedCount: &used, TotalCount: &total,
		}},
	}}
	table := Table(reports, Opts{})
	if !strings.Contains(table, "18%") || !strings.Contains(table, "55/300 crédits") {
		t.Fatalf("unexpected table: %q", table)
	}
	short := Short(reports, Opts{})
	if !strings.Contains(short, "18% 55/300") {
		t.Fatalf("unexpected short output: %q", short)
	}
}
