package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/valche5/ai-usage/internal/credstore"
	"github.com/valche5/ai-usage/internal/httpx"
)

const copilotUsageURL = "https://api.github.com/copilot_internal/user"

// Copilot reports GitHub Copilot quota.
type Copilot struct{}

func (Copilot) ID() string   { return "copilot" }
func (Copilot) Name() string { return "Copilot" }

func (Copilot) Fingerprint(time.Time) string {
	cands, err := credstore.Copilot()
	if err != nil || len(cands) == 0 {
		return ""
	}
	return credstore.Fingerprint(cands[0].Token)
}

type copilotQuota struct {
	Entitlement      float64 `json:"entitlement"`
	Remaining        float64 `json:"remaining"`
	PercentRemaining float64 `json:"percent_remaining"`
	Unlimited        bool    `json:"unlimited"`
	OveragePermitted bool    `json:"overage_permitted"`
	QuotaID          string  `json:"quota_id"`
}

type copilotResponse struct {
	CopilotPlan    string                  `json:"copilot_plan"`
	QuotaResetDate string                  `json:"quota_reset_date"`
	QuotaSnapshots map[string]copilotQuota `json:"quota_snapshots"`
	// Login is the GitHub account the seat belongs to. It is the only account
	// name available here: the opencode and $GITHUB_TOKEN sources carry an
	// opaque token and no identity at all.
	Login string `json:"login"`

	// Free-tier shape.
	LimitedUserQuotas    map[string]float64 `json:"limited_user_quotas"`
	MonthlyQuotas        map[string]float64 `json:"monthly_quotas"`
	LimitedUserResetDate string             `json:"limited_user_reset_date"`
}

func (c Copilot) Collect(ctx context.Context, o Options) Report {
	cands, err := credstore.Copilot()
	switch {
	case errors.Is(err, credstore.ErrMissing):
		return Unconfigured(c.ID(), c.Name(), "aucun token GitHub trouvé pour Copilot")
	case err != nil:
		return Errorf(c.ID(), c.Name(), err.Error())
	}

	base := Report{ID: c.ID(), Name: c.Name()}
	// Name the credential we would have used, so even a failing line says which
	// account it is about. The loop below refines this per candidate.
	base.CredPath = credstore.Display(cands[0].Path)
	base.Account = cands[0].Email
	if o.Offline {
		base.Status = StatusError
		base.Reason = "mode hors ligne — Copilot n'a aucun cache local"
		return base
	}

	// Copilot tokens carry no expiry metadata, so a dead one is only visible
	// from the server's answer. Walk the candidates until one is accepted.
	var lastReason string
	for _, cred := range cands {
		base.CredPath = credstore.Display(cred.Path)
		base.Account = cred.Email
		base.TokenFP = credstore.Fingerprint(cred.Token)

		var resp copilotResponse
		_, err := httpx.JSON(ctx, o.HTTP, httpx.Req{
			URL: copilotUsageURL,
			Headers: map[string]string{
				"Authorization":         "token " + cred.Token,
				"Accept":                "application/json",
				"Editor-Version":        "vscode/1.96.2",
				"Editor-Plugin-Version": "copilot-chat/0.26.7",
				"User-Agent":            "GitHubCopilotChat/0.26.7",
				"X-Github-Api-Version":  "2025-04-01",
			},
		}, &resp)
		if err != nil {
			lastReason = err.Error()
			if httpx.IsUnauthorized(err) {
				lastReason = "token GitHub refusé — reconnecte Copilot"
				continue // try the next candidate
			}
			break // a network problem will not improve with another token
		}

		windows, notes := copilotWindows(resp)
		if len(windows) == 0 {
			lastReason = driftReason("aucun quota_snapshots reconnu — ou pas d'abonnement actif")
			break
		}
		base.Status = StatusOK
		base.Source = SourceLive
		base.FetchedAt = o.Now
		base.Plan = resp.CopilotPlan
		if resp.Login != "" {
			base.Account = resp.Login
		}
		base.Windows = windows
		base.Notes = notes
		return base
	}

	base.Status = StatusError
	base.Reason = fallbackReason(lastReason, "quota Copilot indisponible")
	return base
}

// copilotOrder keeps the most interesting quota first and gives each a short
// label.
var copilotOrder = []struct{ key, label string }{
	{"premium_interactions", "premium"},
	{"chat", "chat"},
	{"completions", "complétions"},
}

func copilotWindows(r copilotResponse) ([]Window, []string) {
	var out []Window
	var notes []string
	reset := copilotDate(r.QuotaResetDate)

	seen := map[string]bool{}
	for _, o := range copilotOrder {
		q, ok := r.QuotaSnapshots[o.key]
		if !ok {
			continue
		}
		seen[o.key] = true
		out = append(out, copilotWindow(o.key, o.label, q, reset))
		if q.OveragePermitted {
			notes = append(notes, o.label+" : dépassement autorisé")
		}
	}
	for key, q := range r.QuotaSnapshots {
		if seen[key] {
			continue
		}
		out = append(out, copilotWindow(key, key, q, reset))
	}
	if len(out) > 0 {
		return out, notes
	}

	// Free tier reports raw counts instead of percentages.
	freeReset := copilotDate(r.LimitedUserResetDate)
	for _, o := range copilotOrder {
		limit, hasLimit := r.MonthlyQuotas[o.key]
		remaining, hasRemaining := r.LimitedUserQuotas[o.key]
		if !hasLimit || !hasRemaining || limit <= 0 {
			continue
		}
		out = append(out, Window{
			Key:         o.key,
			Label:       o.label,
			UsedPercent: (1 - remaining/limit) * 100,
			ResetsAt:    freeReset,
		})
		notes = append(notes, fmt.Sprintf("%s : %.0f/%.0f restants", o.label, remaining, limit))
	}
	return out, notes
}

func copilotWindow(key, label string, q copilotQuota, reset *time.Time) Window {
	w := Window{Key: key, Label: label, ResetsAt: reset, Unlimited: q.Unlimited}
	if !q.Unlimited {
		w.UsedPercent = 100 - q.PercentRemaining
	}
	return w
}

// copilotDate accepts both a plain date and a full timestamp.
func copilotDate(s string) *time.Time {
	if s == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return &t
		}
	}
	return nil
}

func fallbackReason(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
