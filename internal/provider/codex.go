package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/valche5/ai-usage/internal/credstore"
	"github.com/valche5/ai-usage/internal/httpx"
)

// codexUsageURL is hardcoded deliberately. The user's shell exports
// OPENAI_BASE_URL pointing at a local proxy; reading it here would silently
// send the request to the proxy instead of ChatGPT. main() also scrubs the
// variable, so this is defence in depth.
const codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

// codexLimitID is the snapshot we want. Never select by position: responses
// also carry unrelated ids such as "codex_bengalfox".
const codexLimitID = "codex"

// Codex reports ChatGPT subscription utilization.
type Codex struct{}

func (Codex) ID() string   { return "chatgpt" }
func (Codex) Name() string { return "ChatGPT" }

func (Codex) Fingerprint(now time.Time) string {
	cands, err := credstore.Codex()
	if err != nil {
		return ""
	}
	cred, _ := credstore.Choose(cands, now)
	return credstore.Fingerprint(cred.Token)
}

func (c Codex) Collect(ctx context.Context, o Options) Report {
	cands, err := credstore.Codex()
	switch {
	case errors.Is(err, credstore.ErrAPIKeyMode):
		return Unconfigured(c.ID(), c.Name(), "codex est en mode clé API — pas d'usage d'abonnement")
	case errors.Is(err, credstore.ErrMissing):
		return Unconfigured(c.ID(), c.Name(), "pas de ~/.codex/auth.json — lance `codex login`")
	case err != nil:
		return Errorf(c.ID(), c.Name(), err.Error())
	}

	cred, valid := credstore.Choose(cands, o.Now)
	base := Report{
		ID:       c.ID(),
		Name:     c.Name(),
		Plan:     cred.Plan,
		CredPath: credstore.Display(cred.Path),
		Account:  cred.Email,
		TokenFP:  credstore.Fingerprint(cred.Token),
	}

	if o.Offline {
		return codexLocal(base, "mode hors ligne")
	}
	if !valid {
		return codexLocal(base, "token expiré — lance `codex login`")
	}

	var raw any
	_, err = httpx.JSON(ctx, o.HTTP, httpx.Req{
		URL: codexUsageURL,
		Headers: map[string]string{
			"Authorization":      "Bearer " + cred.Token,
			"ChatGPT-Account-Id": cred.AccountID,
			"originator":         "codex_cli_rs",
			"User-Agent":         "codex_cli_rs/" + credstore.CodexVersion(),
			"Accept":             "application/json",
		},
	}, &raw)
	if err != nil {
		reason := err.Error()
		if httpx.IsUnauthorized(err) {
			reason = "token refusé — lance `codex login`"
		}
		return codexLocal(base, reason)
	}

	snap, ok := pickCodexSnapshot(findCodexSnapshots(raw))
	if !ok {
		return codexLocal(base, driftReason("aucun objet rate_limit reconnu"))
	}
	topPlan, topCredits := codexTopLevel(raw)
	if snap.PlanType == "" {
		snap.PlanType = topPlan
	}
	if snap.Credits == nil {
		snap.Credits = topCredits
	}

	base.Status = StatusOK
	base.Source = SourceLive
	base.FetchedAt = o.Now
	base.Windows = snap.windows()
	base.Notes = snap.notes()
	if snap.PlanType != "" {
		base.Plan = snap.PlanType
	}
	if len(base.Windows) == 0 {
		return codexLocal(base, driftReason("aucune fenêtre exploitable"))
	}
	return base
}

// ---------------------------------------------------------------- snapshots

type codexWindow struct {
	UsedPercent   float64
	WindowMinutes int
	ResetsAt      *time.Time
}

type codexSnapshot struct {
	LimitID   string
	PlanType  string
	Primary   *codexWindow
	Secondary *codexWindow
	Credits   *codexCredits
}

type codexCredits struct {
	HasCredits bool
	Unlimited  bool
	Balance    string
}

func (s codexSnapshot) windows() []Window {
	var out []Window
	add := func(key string, w *codexWindow) {
		if w == nil {
			return
		}
		out = append(out, Window{
			Key:           key,
			Label:         LabelForMinutes(w.WindowMinutes),
			UsedPercent:   w.UsedPercent,
			ResetsAt:      w.ResetsAt,
			WindowMinutes: w.WindowMinutes,
		})
	}
	add("primary", s.Primary)
	add("secondary", s.Secondary)
	return out
}

func (s codexSnapshot) notes() []string {
	if s.Credits == nil {
		return nil
	}
	switch {
	case s.Credits.Unlimited:
		return []string{"crédits illimités"}
	case s.Credits.HasCredits && s.Credits.Balance != "":
		return []string{"crédits : " + s.Credits.Balance}
	}
	return nil
}

// pickCodexSnapshot prefers the "codex" limit id, then any snapshot that
// actually carries a window.
func pickCodexSnapshot(snaps []codexSnapshot) (codexSnapshot, bool) {
	for _, s := range snaps {
		if s.LimitID == codexLimitID && (s.Primary != nil || s.Secondary != nil) {
			return s, true
		}
	}
	for _, s := range snaps {
		if s.Primary != nil || s.Secondary != nil {
			return s, true
		}
	}
	return codexSnapshot{}, false
}

// findCodexSnapshots walks decoded JSON looking for rate-limit snapshots.
//
// A tolerant walk rather than a fixed struct because the same data reaches us
// in several shapes: the HTTP response nests a list under rate_limits, the
// session rollout stores a single object, and the app-server protocol uses
// camelCase (usedPercent / windowDurationMins). Walking makes all of them work
// and keeps a future rename from silently zeroing the report.
func findCodexSnapshots(v any) []codexSnapshot {
	var out []codexSnapshot
	var walk func(any)
	walk = func(n any) {
		switch t := n.(type) {
		case map[string]any:
			if s, ok := parseCodexSnapshot(t); ok {
				out = append(out, s)
				return
			}
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys) // deterministic traversal
			for _, k := range keys {
				walk(t[k])
			}
		case []any:
			for _, item := range t {
				walk(item)
			}
		}
	}
	walk(v)
	return out
}

// parseCodexSnapshot recognizes a snapshot by the presence of a window.
//
// Two live spellings exist for the same thing: the session rollout writes
// rate_limits.primary.window_minutes, while /wham/usage writes
// rate_limit.primary_window.limit_window_seconds. Both are accepted.
func parseCodexSnapshot(m map[string]any) (codexSnapshot, bool) {
	primary, okP := parseCodexWindow(pick(m, "primary", "primary_window", "primaryWindow"))
	secondary, okS := parseCodexWindow(pick(m, "secondary", "secondary_window", "secondaryWindow"))
	if !okP && !okS {
		return codexSnapshot{}, false
	}
	s := codexSnapshot{
		LimitID:  jsonString(pick(m, "limit_id", "limitId")),
		PlanType: jsonString(pick(m, "plan_type", "planType")),
	}
	if okP {
		s.Primary = primary
	}
	if okS {
		s.Secondary = secondary
	}
	s.Credits = parseCodexCredits(pick(m, "credits"))
	return s, true
}

func parseCodexCredits(v any) *codexCredits {
	c, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return &codexCredits{
		HasCredits: jsonBool(pick(c, "has_credits", "hasCredits")),
		Unlimited:  jsonBool(pick(c, "unlimited")),
		Balance:    jsonString(pick(c, "balance")),
	}
}

func parseCodexWindow(v any) (*codexWindow, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	pct, ok := jsonNumber(pick(m, "used_percent", "usedPercent"))
	if !ok {
		return nil, false
	}
	w := &codexWindow{UsedPercent: pct}
	switch {
	case has(m, "window_minutes", "windowDurationMins", "windowMinutes"):
		mins, _ := jsonNumber(pick(m, "window_minutes", "windowDurationMins", "windowMinutes"))
		w.WindowMinutes = int(mins)
	case has(m, "limit_window_seconds", "limitWindowSeconds"):
		secs, _ := jsonNumber(pick(m, "limit_window_seconds", "limitWindowSeconds"))
		w.WindowMinutes = int(secs / 60)
	}
	w.ResetsAt = jsonTime(pick(m, "resets_at", "resetsAt", "reset_at", "resetAt"))
	if w.ResetsAt == nil {
		// Some payloads only give a relative offset.
		if after, ok := jsonNumber(pick(m, "reset_after_seconds", "resetAfterSeconds")); ok && after > 0 {
			t := time.Now().Add(time.Duration(after) * time.Second)
			w.ResetsAt = &t
		}
	}
	return w, true
}

// codexTopLevel picks up plan and credits when they sit beside the rate-limit
// object rather than inside it, which is how /wham/usage lays them out.
func codexTopLevel(raw any) (plan string, credits *codexCredits) {
	m, ok := raw.(map[string]any)
	if !ok {
		return "", nil
	}
	return jsonString(pick(m, "plan_type", "planType")), parseCodexCredits(pick(m, "credits"))
}

func has(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- local

// codexLocal reads the newest rate-limit snapshot Codex wrote into a session
// rollout. These can be days old even when codex ran today, so the caller
// always labels the result as stale.
func codexLocal(base Report, why string) Report {
	snap, at, err := newestCodexRollupRateLimits()
	if err != nil {
		base.Status = StatusError
		base.Reason = why
		return base
	}
	windows := snap.windows()
	if len(windows) == 0 {
		base.Status = StatusError
		base.Reason = why
		return base
	}
	base.Status = StatusStale
	base.Source = SourceLocal
	base.Windows = windows
	base.Notes = snap.notes()
	base.Reason = why
	base.FetchedAt = at
	if snap.PlanType != "" && base.Plan == "" {
		base.Plan = snap.PlanType
	}
	return base
}

const maxRolloutFilesScanned = 5

func newestCodexRollupRateLimits() (codexSnapshot, time.Time, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return codexSnapshot{}, time.Time{}, err
	}
	root := filepath.Join(home, ".codex", "sessions")

	var files []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable corner: keep going
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, ".jsonl") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil || len(files) == 0 {
		return codexSnapshot{}, time.Time{}, errors.New("no codex session rollouts")
	}
	// Paths embed YYYY/MM/DD and an ISO timestamp, so reverse lexical order is
	// newest-first.
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	if len(files) > maxRolloutFilesScanned {
		files = files[:maxRolloutFilesScanned]
	}

	for _, f := range files {
		if snap, at, ok := scanRolloutForRateLimits(f); ok {
			return snap, at, nil
		}
	}
	return codexSnapshot{}, time.Time{}, errors.New("no rate limits in recent codex sessions")
}

const maxRolloutLine = 8 << 20

func scanRolloutForRateLimits(path string) (codexSnapshot, time.Time, bool) {
	f, err := os.Open(path)
	if err != nil {
		return codexSnapshot{}, time.Time{}, false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxRolloutLine)

	var lastLine string
	for sc.Scan() {
		line := sc.Text()
		// Cheap prefilter before paying for JSON parsing.
		if strings.Contains(line, `"rate_limits"`) && strings.Contains(line, `"token_count"`) {
			lastLine = line
		}
	}
	if lastLine == "" {
		return codexSnapshot{}, time.Time{}, false
	}

	var rec struct {
		Timestamp string `json:"timestamp"`
		Payload   struct {
			RateLimits any `json:"rate_limits"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(lastLine), &rec); err != nil {
		return codexSnapshot{}, time.Time{}, false
	}
	snap, ok := pickCodexSnapshot(findCodexSnapshots(rec.Payload.RateLimits))
	if !ok {
		return codexSnapshot{}, time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, rec.Timestamp)
	if err != nil {
		if fi, err := f.Stat(); err == nil {
			at = fi.ModTime()
		}
	}
	return snap, at, true
}

// ---------------------------------------------------------------- json utils

// pick returns the first present key, so both snake_case and camelCase
// spellings of the same field resolve.
func pick(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return v
		}
	}
	return nil
}

func jsonString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return fmt.Sprintf("%.0f", t)
	}
	return ""
}

func jsonBool(v any) bool {
	b, ok := v.(bool)
	return ok && b
}

func jsonNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
}

// jsonTime accepts both an epoch-seconds number and an ISO8601 string.
func jsonTime(v any) *time.Time {
	switch t := v.(type) {
	case float64:
		if t <= 0 {
			return nil
		}
		// Values this large are milliseconds, not seconds.
		if t > 1e11 {
			tm := time.UnixMilli(int64(t))
			return &tm
		}
		tm := time.Unix(int64(t), 0)
		return &tm
	case string:
		if tm, err := time.Parse(time.RFC3339, t); err == nil {
			return &tm
		}
	}
	return nil
}
