package provider

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/valche5/ai-usage/internal/credstore"
	"github.com/valche5/ai-usage/internal/httpx"
)

// kimiUsageURL is the usage endpoint of the Kimi For Coding plan. Hardcoded
// like every other host here: no env var may redirect it.
const kimiUsageURL = "https://api.kimi.com/coding/v1/usages"

// Kimi reports Moonshot "Kimi For Coding" subscription utilization.
type Kimi struct{}

func (Kimi) ID() string   { return "kimi" }
func (Kimi) Name() string { return "Kimi" }

func (Kimi) Fingerprint(now time.Time) string {
	cands, err := credstore.Kimi()
	if err != nil {
		return ""
	}
	cred, _ := credstore.Choose(cands, now)
	return credstore.Fingerprint(cred.Token)
}

// kimiNumber accepts a quota figure written either as a JSON string or as a
// number. Moonshot serializes these int64 fields as strings today (protobuf
// convention), so a plain float64 field would decode to zero the day they stop.
type kimiNumber struct {
	Value float64
	Set   bool
}

func (n *kimiNumber) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil // tolerated: an unusable figure is reported as drift, not as a decode error
	}
	n.Value, n.Set = f, true
	return nil
}

// kimiDetail is one quota: an allowance, what has been eaten out of it, and
// when it starts over.
type kimiDetail struct {
	Limit     kimiNumber `json:"limit"`
	Used      kimiNumber `json:"used"`
	Remaining kimiNumber `json:"remaining"`
	ResetTime *string    `json:"resetTime"`
}

type kimiUsageResponse struct {
	// Usage is the plan-level allowance; limits[] are the shorter rate-limit
	// windows layered on top of it.
	Usage  *kimiDetail `json:"usage"`
	Limits []struct {
		Window struct {
			Duration float64 `json:"duration"`
			TimeUnit string  `json:"timeUnit"`
		} `json:"window"`
		Detail *kimiDetail `json:"detail"`
	} `json:"limits"`
	User struct {
		UserID     string `json:"userId"`
		Membership struct {
			Level string `json:"level"`
		} `json:"membership"`
	} `json:"user"`
}

func (k Kimi) Collect(ctx context.Context, o Options) Report {
	cands, err := credstore.Kimi()
	switch {
	case errors.Is(err, credstore.ErrMissing):
		return Unconfigured(k.ID(), k.Name(),
			"pas de clé Kimi (entrée `kimi-for-coding` dans l'auth opencode, ou $KIMI_API_KEY)")
	case err != nil:
		return Errorf(k.ID(), k.Name(), err.Error())
	}

	cred, valid := credstore.Choose(cands, o.Now)
	base := Report{
		ID:        k.ID(),
		Name:      k.Name(),
		CredPath:  credstore.Display(cred.Path),
		Account:   cred.Email,
		AccountID: cred.AccountID,
		TokenFP:   credstore.Fingerprint(cred.Token),
	}

	// Moonshot exposes no local usage cache, so this provider goes quiet
	// offline rather than showing a number it cannot date.
	if o.Offline {
		base.Status = StatusError
		base.Reason = "mode hors ligne — Kimi n'a aucun cache local"
		return base
	}
	// An API key has no expiry at all, so this only fires for an OAuth entry.
	if !valid {
		base.Status = StatusError
		base.Reason = "session Kimi expirée — reconnecte le provider kimi-for-coding dans opencode"
		return base
	}

	var resp kimiUsageResponse
	_, err = httpx.JSON(ctx, o.HTTP, httpx.Req{
		URL: kimiUsageURL,
		Headers: map[string]string{
			"Authorization": "Bearer " + cred.Token,
			"Accept":        "application/json",
		},
	}, &resp)
	if err != nil {
		base.Status = StatusError
		base.Reason = err.Error()
		if httpx.IsUnauthorized(err) {
			// Either source can go bad, so name both remedies rather than
			// sending the user to opencode for a key they exported by hand.
			base.Reason = "clé Kimi refusée — reconnecte kimi-for-coding dans opencode, ou regénère la clé sur la console Kimi Code"
		}
		return base
	}

	windows, warnings := kimiWindows(resp)
	base.Warnings = warnings
	if len(windows) == 0 {
		base.Status = StatusError
		base.Reason = driftReason("aucun quota exploitable dans usage/limits")
		return base
	}

	base.Status = StatusOK
	base.Source = SourceLive
	base.FetchedAt = o.Now
	base.Windows = windows
	base.Plan = kimiPlan(resp.User.Membership.Level)
	base.AccountID = resp.User.UserID
	return base
}

// kimiWindows turns the response into windows, shortest first so the quota
// that bites soonest is read first. Anything that decodes but cannot be turned
// into a percentage is reported as drift rather than dropped in silence.
func kimiWindows(resp kimiUsageResponse) ([]Window, []string) {
	var warnings []string

	type entry struct {
		w    Window
		sort int // window length in minutes; the plan quota sorts last
	}
	var entries []entry

	for _, l := range resp.Limits {
		mins, ok := kimiWindowMinutes(l.Window.Duration, l.Window.TimeUnit)
		if !ok {
			warnings = append(warnings, fmt.Sprintf(
				"durée de fenêtre non interprétée (%g %s) — l'API a probablement changé d'unité",
				l.Window.Duration, orUnset(l.Window.TimeUnit)))
		}
		pct, ok := kimiPercent(l.Detail)
		if !ok {
			continue
		}
		label := LabelForMinutes(mins)
		entries = append(entries, entry{
			w: Window{
				Key:           label,
				Label:         label,
				UsedPercent:   pct,
				ResetsAt:      parseISO(l.Detail.ResetTime),
				WindowMinutes: mins,
			},
			sort: mins,
		})
	}

	// The plan allowance states no duration, so it is labelled for what it is
	// rather than given an invented window length.
	if pct, ok := kimiPercent(resp.Usage); ok {
		entries = append(entries, entry{
			w: Window{
				Key:         "plan",
				Label:       "plan",
				UsedPercent: pct,
				ResetsAt:    parseISO(resp.Usage.ResetTime),
			},
			sort: 1 << 30,
		})
	}

	sort.SliceStable(entries, func(i, j int) bool { return entries[i].sort < entries[j].sort })
	out := make([]Window, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.w)
	}
	return out, warnings
}

// kimiPercent derives consumption from the allowance. used is preferred and
// remaining is the fallback, because the two are redundant upstream and a
// missing field must not silently read as 0%.
func kimiPercent(d *kimiDetail) (float64, bool) {
	if d == nil || !d.Limit.Set || d.Limit.Value <= 0 {
		return 0, false
	}
	switch {
	case d.Used.Set:
		return d.Used.Value / d.Limit.Value * 100, true
	case d.Remaining.Set:
		return (1 - d.Remaining.Value/d.Limit.Value) * 100, true
	}
	return 0, false
}

// kimiWindowMinutes converts a protobuf-style duration to minutes. An
// unrecognized unit returns ok=false so the caller can flag it: guessing here
// would mislabel a 5-hour window as a 5-minute one.
func kimiWindowMinutes(duration float64, unit string) (int, bool) {
	if duration <= 0 {
		return 0, false
	}
	perUnit := map[string]float64{
		"TIME_UNIT_SECOND": 1.0 / 60,
		"TIME_UNIT_MINUTE": 1,
		"TIME_UNIT_HOUR":   60,
		"TIME_UNIT_DAY":    60 * 24,
		"TIME_UNIT_WEEK":   60 * 24 * 7,
		"TIME_UNIT_MONTH":  60 * 24 * 30,
	}
	f, ok := perUnit[strings.ToUpper(unit)]
	if !ok {
		return 0, false
	}
	return int(duration * f), true
}

func orUnset(s string) string {
	if s == "" {
		return "unité absente"
	}
	return s
}

// kimiPlan turns the enum spelling into the plan label a human recognizes.
func kimiPlan(level string) string {
	l := strings.ToLower(strings.TrimPrefix(strings.ToUpper(level), "LEVEL_"))
	if l == "" || l == "unspecified" {
		return ""
	}
	return l
}
