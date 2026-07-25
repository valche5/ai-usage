// Package provider defines the common shape every subscription-usage source
// reports, plus the interface each concrete provider implements.
package provider

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

// Source says where the numbers came from.
type Source string

const (
	SourceLive  Source = "live"  // fetched from the provider's API this run
	SourceCache Source = "cache" // our own cache file
	SourceLocal Source = "local" // a cache the owning CLI wrote itself
	SourceNone  Source = ""
)

// Status is the outcome of a collection attempt.
type Status string

const (
	StatusOK           Status = "ok"
	StatusStale        Status = "stale"        // real numbers, but not fresh
	StatusUnconfigured Status = "unconfigured" // no credential at all: soft skip
	StatusError        Status = "error"        // credential present but unusable
)

// Window is one quota window (a 5-hour bucket, a weekly bucket, ...).
type Window struct {
	Key           string     `json:"key"`
	Label         string     `json:"label"`
	UsedPercent   float64    `json:"used_percent"`
	ResetsAt      *time.Time `json:"resets_at,omitempty"`
	WindowMinutes int        `json:"window_minutes,omitempty"`
	Unlimited     bool       `json:"unlimited,omitempty"`
}

// Report is the normalized result for one provider. It is the only thing we
// ever persist to the cache, so it must never hold a secret.
type Report struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Plan      string    `json:"plan,omitempty"`
	Status    Status    `json:"status"`
	Source    Source    `json:"source"`
	Windows   []Window  `json:"windows"`
	Notes     []string  `json:"notes,omitempty"`
	FetchedAt time.Time `json:"fetched_at"`

	// Reason explains a non-ok status in one human sentence. Never a secret.
	Reason string `json:"reason,omitempty"`
	// CredPath is the file the credential came from (path only, no token).
	CredPath string `json:"cred_path,omitempty"`
	// Account names the account the figures belong to: an email, or a login for
	// Copilot. Rendered next to the provider so a report is never ambiguous
	// about which of several accounts it describes.
	Account string `json:"account,omitempty"`
	// AccountID is the stable identifier behind that name (uuid, org id). It
	// tells two accounts apart when they share an email, and is only rendered
	// under --verbose since the name is what a human reads.
	AccountID string `json:"account_id,omitempty"`

	// Warnings flag values that look wrong rather than merely absent — the
	// signature of an upstream response shape or unit change. These are always
	// rendered: a silently plausible-but-wrong percentage is the worst failure
	// this tool can have, so drift must be loud.
	Warnings []string `json:"warnings,omitempty"`

	// TokenFP keys the cache entry so switching accounts invalidates it.
	// It is a truncated hash, never the token.
	TokenFP string `json:"token_fp,omitempty"`
}

// Options are the knobs a provider needs at collection time.
type Options struct {
	Now     time.Time
	Offline bool // no network at all
	Verbose bool
	Debug   bool
	HTTP    *http.Client
}

// Provider collects usage for one subscription.
//
// Collect must never return an error and must never panic on malformed input:
// a failure is expressed as a Report with StatusError or StatusUnconfigured so
// that one broken provider can never sink the whole report.
type Provider interface {
	ID() string
	Name() string
	// Fingerprint identifies the credential that would be used, so a cached
	// entry can be invalidated when the account changes. It is a truncated
	// hash and returns "" when no credential is available.
	Fingerprint(now time.Time) string
	Collect(ctx context.Context, o Options) Report
}

// Unconfigured builds the soft-skip report for a provider with no credential.
func Unconfigured(id, name, reason string) Report {
	return Report{ID: id, Name: name, Status: StatusUnconfigured, Reason: reason}
}

// Errorf builds an error report.
func Errorf(id, name, reason string) Report {
	return Report{ID: id, Name: name, Status: StatusError, Reason: reason}
}

// Clamp bounds a percentage into [0,100].
//
// Providers deliberately do NOT call this: they store the raw upstream value so
// that Validate can tell the difference between "0%" and "the units changed".
// Clamping happens once, in Validate, after the anomaly has been recorded.
func Clamp(p float64) float64 {
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

// maxPlausibleWindow is the longest quota window we consider believable. A
// reset further out than this means we are reading the wrong field or the wrong
// unit (seconds parsed as milliseconds, for instance).
const maxPlausibleWindow = 45 * 24 * time.Hour

// Validate sanity-checks a freshly collected report, records anything that
// smells like upstream drift, then clamps the values for display.
//
// This is the guard against the failure mode that matters: these are all
// undocumented endpoints, so the realistic risk is not that they disappear
// (that shows up as an error) but that a field is renamed, re-scaled, or
// flipped in meaning while still decoding cleanly.
func (r *Report) Validate(now time.Time) {
	for i := range r.Windows {
		w := &r.Windows[i]

		if math.IsNaN(w.UsedPercent) || math.IsInf(w.UsedPercent, 0) {
			r.warn("%s : pourcentage non numérique — forme de réponse inattendue", w.Label)
			w.UsedPercent = 0
		} else if w.UsedPercent < 0 || w.UsedPercent > 100 {
			// Out of range means the scale changed under us. Say so instead of
			// quietly pinning it to a believable-looking bound.
			r.warn("%s : pourcentage hors bornes (%.4g) — l'API a probablement changé d'échelle",
				w.Label, w.UsedPercent)
		}
		w.UsedPercent = Clamp(w.UsedPercent)

		if w.ResetsAt == nil {
			continue
		}
		switch d := w.ResetsAt.Sub(now); {
		case d < -5*time.Minute:
			r.warn("%s : reset dans le passé (%s) — champ de date probablement changé",
				w.Label, w.ResetsAt.Local().Format("02/01/2006 15:04"))
		case d > maxPlausibleWindow:
			r.warn("%s : reset dans %d jours, invraisemblable — unité de date probablement changée",
				w.Label, int(d.Hours()/24))
		}
	}
}

// driftReason phrases a decode failure that followed a successful HTTP call.
// The distinction matters: a network error is transient, whereas "the server
// answered fine but we no longer recognise the payload" means the undocumented
// contract moved and the parser needs updating.
func driftReason(detail string) string {
	return "forme de réponse inattendue (" + detail + ") — l'API a probablement changé"
}

// Drifted reports whether this report shows signs the upstream contract moved.
func (r Report) Drifted() bool {
	return len(r.Warnings) > 0 || strings.Contains(r.Reason, "forme de réponse inattendue")
}

func (r *Report) warn(format string, args ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, args...))
}

// LabelForMinutes derives a window label from its duration. Never assume
// "primary means 5h": recent Codex payloads emit a single weekly window.
func LabelForMinutes(m int) string {
	switch {
	case m <= 0:
		return "window"
	case m == 300:
		return "5h"
	case m == 10080:
		return "weekly"
	case m%(60*24) == 0:
		d := m / (60 * 24)
		if d == 1 {
			return "24h"
		}
		return itoa(d) + "d"
	case m%60 == 0:
		return itoa(m/60) + "h"
	default:
		return itoa(m) + "min"
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// HasNumbers reports whether the report carries any usable figure.
func (r Report) HasNumbers() bool { return len(r.Windows) > 0 }

// Degraded reports whether the numbers should be treated with suspicion. A
// fresh cache hit is not degraded — it is the normal, throttle-respecting path,
// and its provenance is already shown next to the provider name.
func (r Report) Degraded() bool {
	return r.Status == StatusStale || r.Status == StatusError
}
