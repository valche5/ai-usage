package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/valche5/ai-usage/internal/credstore"
	"github.com/valche5/ai-usage/internal/httpx"
)

// claudeUsageURL is the undocumented endpoint that backs Claude Code's /usage.
// Hardcoded on purpose: no env var may redirect it.
const claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"

// Claude reports Anthropic subscription utilization.
type Claude struct{}

func (Claude) ID() string   { return "claude" }
func (Claude) Name() string { return "Claude" }

func (Claude) Fingerprint(time.Time) string {
	c, err := credstore.Claude()
	if err != nil {
		return ""
	}
	return credstore.Fingerprint(c.Token)
}

// claudeWindow is the shape Anthropic uses in both the live response and the
// blob Claude Code caches locally, which is what lets the two paths agree
// exactly instead of approximately.
type claudeWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

type claudeBuckets struct {
	FiveHour       *claudeWindow `json:"five_hour"`
	SevenDay       *claudeWindow `json:"seven_day"`
	SevenDayOpus   *claudeWindow `json:"seven_day_opus"`
	SevenDaySonnet *claudeWindow `json:"seven_day_sonnet"`
}

type claudeExtraUsage struct {
	IsEnabled    bool     `json:"is_enabled"`
	MonthlyLimit *float64 `json:"monthly_limit"`
	UsedCredits  *float64 `json:"used_credits"`
	Utilization  *float64 `json:"utilization"`
}

type claudeUsageResponse struct {
	claudeBuckets
	ExtraUsage *claudeExtraUsage `json:"extra_usage"`
}

func (c Claude) Collect(ctx context.Context, o Options) Report {
	cred, err := credstore.Claude()
	switch {
	case errors.Is(err, credstore.ErrMissing):
		return Unconfigured(c.ID(), c.Name(),
			"pas de ~/.claude/.credentials.json — lance `claude` pour te connecter")
	case err != nil:
		return Errorf(c.ID(), c.Name(), err.Error())
	}

	base := Report{
		ID:       c.ID(),
		Name:     c.Name(),
		Plan:     cred.Plan,
		CredPath: credstore.Display(cred.Path),
		Account:  cred.Email,
		TokenFP:  credstore.Fingerprint(cred.Token),
	}

	if o.Offline {
		return claudeLocal(base, o, "mode hors ligne")
	}
	if cred.Expired(o.Now) {
		return claudeLocal(base, o, "token expiré — lance `claude` une fois pour le rafraîchir")
	}

	var resp claudeUsageResponse
	_, err = httpx.JSON(ctx, o.HTTP, httpx.Req{
		URL: claudeUsageURL,
		Headers: map[string]string{
			"Authorization": "Bearer " + cred.Token,
			// Required. Any other User-Agent lands in an aggressively
			// rate-limited bucket that answers 429 indefinitely. We always
			// send the version that is actually installed.
			"User-Agent":     "claude-code/" + credstore.ClaudeCodeVersion(),
			"anthropic-beta": "oauth-2025-04-20",
			"Content-Type":   "application/json",
			"Accept":         "application/json",
		},
	}, &resp)
	if err != nil {
		reason := err.Error()
		if httpx.IsRateLimited(err) {
			reason = "rate-limited (429)"
		} else if httpx.IsUnauthorized(err) {
			reason = "token refusé — lance `claude` une fois"
		}
		return claudeLocal(base, o, reason)
	}

	base.Status = StatusOK
	base.Source = SourceLive
	base.FetchedAt = o.Now
	base.Windows = claudeWindows(resp.claudeBuckets)
	base.Notes = claudeNotes(resp.ExtraUsage)
	if len(base.Windows) == 0 {
		return claudeLocal(base, o, driftReason("aucun champ utilization reconnu"))
	}
	return base
}

// claudeLocal falls back to the utilization blob Claude Code itself refreshes
// in ~/.claude.json. Zero network, zero client impersonation — which is also
// why --offline is the clean way to run this tool.
func claudeLocal(base Report, o Options, why string) Report {
	cached, err := readClaudeCachedUsage()
	if err != nil {
		base.Status = StatusError
		base.Reason = why
		return base
	}
	windows := claudeWindows(cached.Utilization.claudeBuckets)
	if len(windows) == 0 {
		windows = claudeLimitWindows(cached.Utilization.Limits)
	}
	if len(windows) == 0 {
		base.Status = StatusError
		base.Reason = why
		return base
	}
	base.Status = StatusStale
	base.Source = SourceLocal
	base.Windows = windows
	base.Notes = claudeNotes(cached.Utilization.ExtraUsage)
	base.Reason = why
	if cached.FetchedAtMs > 0 {
		base.FetchedAt = time.UnixMilli(cached.FetchedAtMs)
	}
	return base
}

type claudeCachedUsage struct {
	FetchedAtMs int64 `json:"fetchedAtMs"`
	Utilization struct {
		claudeBuckets
		ExtraUsage *claudeExtraUsage `json:"extra_usage"`
		Limits     []struct {
			Kind     string   `json:"kind"`
			Group    string   `json:"group"`
			Percent  *float64 `json:"percent"`
			ResetsAt *string  `json:"resets_at"`
			IsActive bool     `json:"is_active"`
		} `json:"limits"`
	} `json:"utilization"`
}

func readClaudeCachedUsage() (*claudeCachedUsage, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return nil, err
	}
	var top struct {
		Cached *claudeCachedUsage `json:"cachedUsageUtilization"`
	}
	if err := json.Unmarshal(b, &top); err != nil {
		return nil, err
	}
	if top.Cached == nil {
		return nil, errors.New("no cachedUsageUtilization")
	}
	return top.Cached, nil
}

func claudeWindows(b claudeBuckets) []Window {
	var out []Window
	add := func(key, label string, w *claudeWindow) {
		if w == nil || w.Utilization == nil {
			return
		}
		out = append(out, Window{
			Key:         key,
			Label:       label,
			UsedPercent: *w.Utilization,
			ResetsAt:    parseISO(w.ResetsAt),
		})
	}
	add("five_hour", "5h", b.FiveHour)
	add("seven_day", "7d", b.SevenDay)
	add("seven_day_opus", "7d opus", b.SevenDayOpus)
	add("seven_day_sonnet", "7d sonnet", b.SevenDaySonnet)
	return out
}

// claudeLimitWindows reads the forward-compatible limits[] array, used only
// when the legacy buckets are absent.
func claudeLimitWindows(limits []struct {
	Kind     string   `json:"kind"`
	Group    string   `json:"group"`
	Percent  *float64 `json:"percent"`
	ResetsAt *string  `json:"resets_at"`
	IsActive bool     `json:"is_active"`
}) []Window {
	var out []Window
	for _, l := range limits {
		if l.Percent == nil {
			continue
		}
		label := l.Kind
		switch l.Kind {
		case "session":
			label = "5h"
		case "weekly_all":
			label = "7d"
		}
		out = append(out, Window{
			Key:         l.Kind,
			Label:       label,
			UsedPercent: *l.Percent,
			ResetsAt:    parseISO(l.ResetsAt),
		})
	}
	return out
}

func claudeNotes(e *claudeExtraUsage) []string {
	if e == nil || !e.IsEnabled {
		return nil
	}
	note := "extra usage activé"
	if e.UsedCredits != nil && e.MonthlyLimit != nil {
		note = fmt.Sprintf("extra usage %.2f/%.2f crédits", *e.UsedCredits, *e.MonthlyLimit)
	} else if e.Utilization != nil {
		note = fmt.Sprintf("extra usage %.0f%%", Clamp(*e.Utilization))
	}
	return []string{note}
}

func parseISO(s *string) *time.Time {
	if s == nil || *s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		return nil
	}
	return &t
}
