// Package render turns normalized reports into terminal output.
package render

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/valche5/ai-usage/internal/provider"
)

// Opts controls presentation.
type Opts struct {
	Color    bool
	BarWidth int
	Now      time.Time
	Verbose  bool
}

const defaultBarWidth = 16

// ANSI SGR codes, applied directly so the binary keeps zero dependencies.
const (
	reset  = "\x1b[0m"
	dim    = "\x1b[2m"
	green  = "\x1b[32m"
	yellow = "\x1b[33m"
	red    = "\x1b[31m"
	bold   = "\x1b[1m"
)

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// StripANSI removes SGR sequences.
func StripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

// VisibleWidth is the printable width of s, ignoring colour codes.
func VisibleWidth(s string) int { return utf8.RuneCountInString(StripANSI(s)) }

func (o Opts) paint(code, s string) string {
	if !o.Color || code == "" {
		return s
	}
	return code + s + reset
}

func (o Opts) barWidth() int {
	if o.BarWidth > 0 {
		return o.BarWidth
	}
	return defaultBarWidth
}

// Severity buckets a percentage for colouring.
func Severity(pct float64) string {
	switch {
	case pct >= 85:
		return "crit"
	case pct >= 60:
		return "warn"
	default:
		return "ok"
	}
}

func (o Opts) colorFor(pct float64) string {
	switch Severity(pct) {
	case "crit":
		return red
	case "warn":
		return yellow
	default:
		return green
	}
}

// Bar draws a proportional gauge. Any non-zero usage gets at least one cell,
// otherwise "1%" would be indistinguishable from "0%".
func Bar(pct float64, width int, color bool, o Opts) string {
	if width <= 0 {
		width = defaultBarWidth
	}
	filled := int(pct/100*float64(width) + 0.5)
	// Rounding must never erase the distinction between "barely started" and
	// "not started", nor between "nearly full" and "full".
	if pct > 0 && filled < 1 {
		filled = 1
	}
	if pct < 100 && filled >= width {
		filled = width - 1
	}
	if filled > width {
		filled = width
	}
	if filled < 0 {
		filled = 0
	}
	s := strings.Repeat("▓", filled) + strings.Repeat("░", width-filled)
	if !color {
		return s
	}
	return o.paint(o.colorFor(pct), s)
}

// RelTime renders how far away t is, compactly.
func RelTime(t time.Time, now time.Time) string {
	d := t.Sub(now)
	if d < 0 {
		return "dépassé"
	}
	switch {
	case d < time.Minute:
		return "<1min"
	case d < time.Hour:
		return fmt.Sprintf("%dmin", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		return fmt.Sprintf("%dh%02d", h, m)
	default:
		days := int(d.Hours()) / 24
		h := int(d.Hours()) % 24
		if h == 0 {
			return fmt.Sprintf("%dj", days)
		}
		return fmt.Sprintf("%dj%dh", days, h)
	}
}

var frWeekday = map[time.Weekday]string{
	time.Sunday: "dim.", time.Monday: "lun.", time.Tuesday: "mar.",
	time.Wednesday: "mer.", time.Thursday: "jeu.", time.Friday: "ven.", time.Saturday: "sam.",
}

// LocalTime renders an absolute reset moment in local time.
func LocalTime(t time.Time, now time.Time) string {
	t = t.Local()
	n := now.Local()
	if t.YearDay() == n.YearDay() && t.Year() == n.Year() {
		return t.Format("15:04")
	}
	if t.After(n) && t.Sub(n) < 7*24*time.Hour {
		return frWeekday[t.Weekday()] + " " + t.Format("15:04")
	}
	return t.Format("02/01")
}

func (o Opts) resetText(w provider.Window) string {
	if w.ResetsAt == nil {
		return o.paint(dim, "reset —")
	}
	return fmt.Sprintf("reset %s %s",
		LocalTime(*w.ResetsAt, o.Now),
		o.paint(dim, "("+RelTime(*w.ResetsAt, o.Now)+")"))
}

// account renders who the figures belong to: the name alone normally, and the
// full identifier next to it under --verbose, which is what tells two accounts
// apart when they share an email.
//
// When the source knows no name at all — a Kimi API key is attached to nothing
// but an opaque userId, and Moonshot exposes no profile endpoint — the id is
// shown in its place even without --verbose. An ugly identifier still says
// which account the percentage belongs to; a blank says nothing.
func account(r provider.Report, verbose bool) string {
	switch {
	case r.Account == "":
		return r.AccountID
	case !verbose || r.AccountID == "":
		return r.Account
	default:
		return r.Account + " (" + r.AccountID + ")"
	}
}

// sourceNote labels non-live data so a stale number is never mistaken for a
// fresh one.
func (o Opts) sourceNote(r provider.Report) string {
	switch r.Source {
	case provider.SourceCache:
		return fmt.Sprintf("cache %s", age(r.FetchedAt, o.Now))
	case provider.SourceLocal:
		return fmt.Sprintf("local, %s", age(r.FetchedAt, o.Now))
	}
	return ""
}

func age(t, now time.Time) string {
	if t.IsZero() {
		return "âge inconnu"
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dmin", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dj", int(d.Hours())/24)
	}
}

// Table renders the default multi-line report.
func Table(reports []provider.Report, o Opts) string {
	nameW, labelW := 0, 0
	for _, r := range reports {
		if w := utf8.RuneCountInString(r.Name); w > nameW {
			nameW = w
		}
		for _, win := range r.Windows {
			if w := utf8.RuneCountInString(win.Label); w > labelW {
				labelW = w
			}
		}
	}

	var b strings.Builder
	first := true
	prevBlock := false
	for _, r := range reports {
		// Blank lines separate multi-line blocks; consecutive one-liners stay
		// tight so a list of skips does not sprawl.
		block := len(r.Windows) > 0
		if !first && (block || prevBlock) {
			b.WriteByte('\n')
		}
		first, prevBlock = false, block

		if r.Status == provider.StatusUnconfigured && len(r.Windows) == 0 {
			b.WriteString(fmt.Sprintf("%-*s %s\n", nameW, r.Name,
				o.paint(dim, "— "+fallback(r.Reason, "non configuré"))))
			continue
		}

		head := fmt.Sprintf("%-*s", nameW, r.Name)
		if r.Plan != "" {
			head += " " + o.paint(bold, r.Plan)
		}
		// The account is shown unconditionally: with several accounts in play
		// (a personal Claude, a work Copilot seat), a percentage with no owner
		// is a number you cannot act on.
		if acct := account(r, o.Verbose); acct != "" {
			head += "  " + o.paint(dim, acct)
		}
		if note := o.sourceNote(r); note != "" {
			head += "  " + o.paint(yellow, "("+note+")")
		}
		if o.Verbose && r.CredPath != "" {
			head += "  " + o.paint(dim, r.CredPath)
		}
		b.WriteString(strings.TrimRight(head, " ") + "\n")

		if len(r.Windows) == 0 {
			b.WriteString("  " + o.paint(dim, "n/a  "+fallback(r.Reason, "aucune donnée")) + "\n")
			continue
		}
		for _, w := range r.Windows {
			if w.Unlimited {
				b.WriteString(fmt.Sprintf("  %-*s %s\n", labelW, w.Label, o.paint(green, "illimité")))
				continue
			}
			b.WriteString(fmt.Sprintf("  %-*s %s %3.0f%%   %s\n",
				labelW, w.Label,
				Bar(w.UsedPercent, o.barWidth(), o.Color, o),
				w.UsedPercent,
				o.resetText(w)))
		}
		for _, n := range r.Notes {
			b.WriteString("  " + o.paint(dim, n) + "\n")
		}
		// Drift warnings are never hidden behind a flag: a plausible-looking
		// wrong number is the one failure the user cannot detect themselves.
		for _, w := range r.Warnings {
			b.WriteString("  " + o.paint(red, "⚠ "+w) + "\n")
		}
		// Stale numbers carry the reason too, not just failed ones: "(local, 22h)"
		// says the data is old, but only the reason says what to run to fix it.
		if r.Degraded() && len(r.Windows) > 0 && r.Reason != "" {
			b.WriteString("  " + o.paint(yellow, "! "+r.Reason) + "\n")
		}
	}
	return b.String()
}

// Short renders one line suitable for a shell prompt or a statusline.
func Short(reports []provider.Report, o Opts) string {
	var parts []string
	for _, r := range reports {
		if len(r.Windows) == 0 {
			continue
		}
		nums := make([]string, 0, len(r.Windows))
		for _, w := range r.Windows {
			if w.Unlimited {
				nums = append(nums, "∞")
				continue
			}
			nums = append(nums, fmt.Sprintf("%.0f%%", w.UsedPercent))
		}
		s := strings.ToLower(r.Name) + " " + strings.Join(nums, "/")
		if r.Degraded() {
			s += "!"
		}
		worst := 0.0
		for _, w := range r.Windows {
			if !w.Unlimited && w.UsedPercent > worst {
				worst = w.UsedPercent
			}
		}
		parts = append(parts, o.paint(o.colorFor(worst), s))
	}
	return strings.Join(parts, " · ")
}

// Footer summarizes degradation. It belongs on stderr: the table is the
// machine-readable-ish payload, diagnostics are not.
func Footer(reports []provider.Report, o Opts) string {
	var degraded, failed, drifted []string
	for _, r := range reports {
		if r.Drifted() {
			drifted = append(drifted, strings.ToLower(r.Name))
		}
		switch {
		case r.Status == provider.StatusError:
			failed = append(failed, strings.ToLower(r.Name))
		case r.Degraded() && r.Status != provider.StatusUnconfigured:
			degraded = append(degraded, strings.ToLower(r.Name))
		}
	}
	sort.Strings(degraded)
	sort.Strings(failed)
	sort.Strings(drifted)

	var out []string
	if len(drifted) > 0 {
		// Loudest line, and it names the fix: this is the failure mode that
		// silently produces wrong numbers.
		out = append(out, o.paint(red, fmt.Sprintf(
			"⚠ %s : l'API semble avoir changé — le parseur est à mettre à jour (`ai-usage --check`)",
			strings.Join(drifted, ", "))))
	}

	var msgs []string
	if len(failed) > 0 {
		msgs = append(msgs, fmt.Sprintf("%s en échec", strings.Join(failed, ", ")))
	}
	if len(degraded) > 0 {
		msgs = append(msgs, fmt.Sprintf("%s pas à jour", strings.Join(degraded, ", ")))
	}
	if len(msgs) > 0 {
		out = append(out, o.paint(yellow, "! "+strings.Join(msgs, " · ")+" — relance avec --debug pour le détail"))
	}
	return strings.Join(out, "\n")
}

// Check renders the drift diagnostic: for each provider, what was actually
// recognized in the response. This is the "did the API change?" answer, meant
// to be run by hand after an unexpected number or from a cron canary.
func Check(reports []provider.Report, o Opts) string {
	var b strings.Builder
	for _, r := range reports {
		verdict, code := "OK", green
		switch {
		case r.Drifted():
			verdict, code = "DÉRIVE", red
		case r.Status == provider.StatusError:
			verdict, code = "ÉCHEC", red
		case r.Status == provider.StatusStale:
			verdict, code = "PÉRIMÉ", yellow
		case r.Status == provider.StatusUnconfigured:
			verdict, code = "ABSENT", dim
		}
		fmt.Fprintf(&b, "%-8s %s  source=%-5s fenêtres=%d\n",
			r.ID, o.paint(code, fmt.Sprintf("%-6s", verdict)), orDash(string(r.Source)), len(r.Windows))

		// A drift diagnostic without the account is ambiguous: the same
		// endpoint answers differently for a personal and a business seat.
		fmt.Fprintf(&b, "           compte=%s cred=%s\n",
			orDash(account(r, true)), orDash(r.CredPath))

		for _, w := range r.Windows {
			resetInfo := "reset=—"
			if w.ResetsAt != nil {
				resetInfo = "reset=" + w.ResetsAt.Local().Format("2006-01-02 15:04")
			}
			mins := "durée=?"
			if w.WindowMinutes > 0 {
				mins = fmt.Sprintf("durée=%dmin", w.WindowMinutes)
			}
			fmt.Fprintf(&b, "           %-12s used=%-6.2f %-14s %s\n", w.Label, w.UsedPercent, mins, resetInfo)
		}
		if r.Reason != "" {
			fmt.Fprintf(&b, "           %s\n", o.paint(dim, r.Reason))
		}
		for _, w := range r.Warnings {
			fmt.Fprintf(&b, "           %s\n", o.paint(red, "⚠ "+w))
		}
	}
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func fallback(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
