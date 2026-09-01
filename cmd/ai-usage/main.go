// Command ai-usage prints how much of each AI subscription you have consumed.
//
// It discovers credentials that the already-installed CLIs (claude, codex, pi,
// grok, opencode) wrote to $HOME, reads each provider's own usage source, and
// renders one aligned report. It is strictly read-only: no token is ever
// refreshed, rewritten or logged.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/valche5/ai-usage/internal/cache"
	"github.com/valche5/ai-usage/internal/httpx"
	"github.com/valche5/ai-usage/internal/provider"
	"github.com/valche5/ai-usage/internal/render"
)

// version is stamped by the Makefile via -ldflags.
var version = "dev"

// Exit codes.
const (
	exitOK      = 0
	exitFailure = 1 // at least one provider errored
	exitUsage   = 2 // bad flags
	exitNoData  = 3 // nothing reportable at all
)

// ttl is how long a cached report stays fresh, per provider.
var ttl = map[string]time.Duration{
	"claude":  180 * time.Second,
	"chatgpt": 60 * time.Second,
	"grok":    60 * time.Second,
	// Copilot and OpenRouter are tracked at 60s because large-model spend can
	// move a quota window fast; nothing behind them has a hard floor like
	// Anthropic's, and the cost of a 429 is just serving the stale cache.
	"copilot":    60 * time.Second,
	"openrouter": 60 * time.Second,
	// Kimi's 5h allowance is only 100 units wide, so it is deliberately polled
	// at a slower rhythm: nothing in that window moves fast enough to justify
	// the extra calls per window.
	"kimi": 300 * time.Second,
	// OpenCode is a prepaid-pool account where the numbers move at the pace of
	// real money spent, never fast enough to poll harder.
	"opencode": 300 * time.Second,
}

// anthropicFloor is the minimum interval between calls to the Anthropic usage
// endpoint. The limit is scoped to the access token, which means it is shared
// with the user's real Claude Code client, so --refresh does not lift it;
// only the explicit --force does.
const anthropicFloor = 180 * time.Second

type options struct {
	jsonOut  bool
	short    bool
	only     string
	all      bool
	offline  bool
	refresh  bool
	force    bool
	noCache  bool
	cacheDir string
	verbose  bool
	debug    bool
	timeout  time.Duration
	colorArg string
	noColor  bool
	strict   bool
	failOver float64
	showVer  bool
	check    bool
}

func main() {
	// First thing, before any HTTP client exists: drop the variables that could
	// redirect or re-authenticate our requests. On this machine OPENAI_BASE_URL
	// points at a local proxy, and honouring it would silently report the
	// wrong numbers.
	httpx.ScrubEnv()

	os.Exit(run(os.Args[1:]))
}

func run(args []string) (code int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintln(os.Stderr, "ai-usage: erreur interne:", httpx.Redact(fmt.Sprint(r)))
			code = exitFailure
		}
	}()

	var o options
	fs := flag.NewFlagSet("ai-usage", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { usage(fs) }

	fs.BoolVar(&o.jsonOut, "json", false, "sortie JSON stable")
	fs.BoolVar(&o.short, "short", false, "une seule ligne, pour un prompt ou une statusline")
	fs.StringVar(&o.only, "only", "", "limiter aux providers listés (claude,chatgpt,grok,kimi,copilot,openrouter,opencode)")
	fs.BoolVar(&o.all, "all", false, "afficher aussi les providers non configurés")
	fs.BoolVar(&o.offline, "offline", false, "aucun appel réseau : caches locaux uniquement")
	fs.BoolVar(&o.refresh, "refresh", false, "ignorer le cache (plancher Anthropic de 180s conservé)")
	fs.BoolVar(&o.force, "force", false, "ignorer aussi le plancher Anthropic (peut te faire 429)")
	fs.BoolVar(&o.noCache, "no-cache", false, "ne pas lire ni écrire le cache")
	fs.StringVar(&o.cacheDir, "cache-dir", "", "répertoire de cache (défaut : $XDG_CACHE_HOME/ai-usage)")
	fs.BoolVar(&o.verbose, "verbose", false, "afficher les chemins de credentials et l'id de compte")
	fs.BoolVar(&o.debug, "debug", false, "diagnostics sur stderr")
	fs.DurationVar(&o.timeout, "timeout", 8*time.Second, "délai maximum par provider")
	fs.StringVar(&o.colorArg, "color", "auto", "couleurs : auto, always, never")
	fs.BoolVar(&o.noColor, "no-color", false, "désactiver les couleurs")
	fs.BoolVar(&o.strict, "strict", false, "traiter les données périmées comme un échec")
	fs.Float64Var(&o.failOver, "fail-over", -1, "sortir en échec si une fenêtre dépasse ce pourcentage")
	fs.BoolVar(&o.check, "check", false, "diagnostic : ce qui a été reconnu dans chaque réponse")
	fs.BoolVar(&o.showVer, "version", false, "afficher la version")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "ai-usage: argument inattendu %q\n", fs.Arg(0))
		usage(fs)
		return exitUsage
	}
	if o.showVer {
		fmt.Println("ai-usage", version)
		return exitOK
	}

	all := []provider.Provider{
		provider.Claude{}, provider.Codex{}, provider.Grok{}, provider.Kimi{}, provider.Copilot{},
		provider.OpenRouter{}, provider.OpenCode{},
	}
	selected, err := selectProviders(all, o.only)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ai-usage:", err)
		return exitUsage
	}

	now := time.Now()
	dir := o.cacheDir
	if dir == "" {
		dir = cache.DefaultDir()
	}
	c := cache.Open(dir, !o.noCache && !o.check)

	reports := collect(selected, c, o, now)
	if !o.noCache {
		if err := c.Flush(); err != nil && o.debug {
			fmt.Fprintln(os.Stderr, "debug: cache non écrit:", err)
		}
	}

	ropts := render.Opts{
		Color:    useColor(o),
		Now:      now,
		BarWidth: 16,
		Verbose:  o.verbose,
	}

	switch {
	case o.check:
		fmt.Print(render.Check(reports, ropts))
	case o.jsonOut:
		if err := writeJSON(reports, now); err != nil {
			fmt.Fprintln(os.Stderr, "ai-usage:", err)
			return exitFailure
		}
	case o.short:
		if line := render.Short(visible(reports, true), ropts); line != "" {
			fmt.Println(line)
		}
	default:
		fmt.Print(render.Table(visible(reports, !o.all), ropts))
		if footer := render.Footer(reports, ropts); footer != "" {
			fmt.Fprintln(os.Stderr, footer)
		}
	}

	if o.debug {
		writeDebug(reports, now)
	}
	return exitCode(reports, o)
}

// collect runs every provider concurrently, mediating the cache around each.
// One provider failing must never sink the report, so nothing here propagates
// an error.
func collect(ps []provider.Provider, c *cache.Cache, o options, now time.Time) []provider.Report {
	out := make([]provider.Report, len(ps))
	client := httpx.Client(o.timeout)

	var wg sync.WaitGroup
	for i, p := range ps {
		wg.Add(1)
		go func(i int, p provider.Provider) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					out[i] = provider.Errorf(p.ID(), p.Name(),
						"erreur interne: "+httpx.Redact(fmt.Sprint(r)))
				}
			}()

			fp := p.Fingerprint(now)
			cached, hasCached := c.Get(p.ID(), fp)

			// A fresh cache hit is the normal path: it is what keeps us within
			// the provider's polling guidance. Without a fingerprint we only
			// trust it offline: in a live run an empty fingerprint means the
			// credential that wrote the entry is gone from $HOME, so the entry
			// must be dropped, not replayed.
			if hasCached && cached.Fresh(now, effectiveTTL(p.ID(), o)) && (o.offline || fp != "") {
				r := cached.Report
				r.Source = provider.SourceCache
				r.FetchedAt = cached.StoredAt
				out[i] = r
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
			defer cancel()

			r := p.Collect(ctx, provider.Options{
				Now:     now,
				Offline: o.offline,
				Verbose: o.verbose,
				Debug:   o.debug,
				HTTP:    client,
			})

			// The credential this entry belonged to no longer exists anywhere
			// in $HOME. The cached figures describe a subscription that is not
			// installed here anymore, so the entry is deleted rather than
			// resurfacing on every subsequent run.
			if r.Status == provider.StatusUnconfigured {
				c.Delete(p.ID())
			}

			// Live failed but we still have numbers from a previous run: show
			// them rather than nothing, clearly marked as stale.
			if !r.HasNumbers() && r.Status == provider.StatusError && hasCached && cached.Report.HasNumbers() {
				stale := cached.Report
				stale.Source = provider.SourceCache
				stale.Status = provider.StatusStale
				stale.FetchedAt = cached.StoredAt
				stale.Reason = r.Reason
				out[i] = stale
				return
			}

			// Sanity-check before caching so a drifted payload is never
			// persisted as if it were good.
			r.Validate(now)
			c.Put(r, now)
			out[i] = r
		}(i, p)
	}
	wg.Wait()
	return out
}

// effectiveTTL applies --refresh / --force, keeping the Anthropic floor unless
// the user explicitly asked to override it.
func effectiveTTL(id string, o options) time.Duration {
	base, ok := ttl[id]
	if !ok {
		base = 60 * time.Second
	}
	if o.force {
		return 0
	}
	if o.refresh {
		if id == "claude" {
			return anthropicFloor
		}
		return 0
	}
	return base
}

func selectProviders(all []provider.Provider, only string) ([]provider.Provider, error) {
	if strings.TrimSpace(only) == "" {
		return all, nil
	}
	byID := map[string]provider.Provider{}
	var ids []string
	for _, p := range all {
		byID[p.ID()] = p
		byID[strings.ToLower(p.Name())] = p
		ids = append(ids, p.ID())
	}
	// Common aliases, so muscle memory works.
	for alias, id := range map[string]string{
		"openai": "chatgpt", "codex": "chatgpt", "gpt": "chatgpt",
		"anthropic": "claude", "xai": "grok", "gh": "copilot", "github": "copilot",
		"moonshot": "kimi", "kimi-for-coding": "kimi",
		"or": "openrouter", "oc": "opencode", "zen": "opencode",
	} {
		byID[alias] = byID[id]
	}

	var out []provider.Provider
	seen := map[string]bool{}
	for _, raw := range strings.Split(only, ",") {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" {
			continue
		}
		p, ok := byID[name]
		if !ok {
			sort.Strings(ids)
			return nil, fmt.Errorf("provider inconnu %q (connus : %s)", raw, strings.Join(ids, ", "))
		}
		if !seen[p.ID()] {
			seen[p.ID()] = true
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("--only ne sélectionne aucun provider")
	}
	return out, nil
}

// visible drops soft-skipped providers unless the caller wants them.
func visible(reports []provider.Report, hideUnconfigured bool) []provider.Report {
	if !hideUnconfigured {
		return reports
	}
	out := make([]provider.Report, 0, len(reports))
	for _, r := range reports {
		if r.Status == provider.StatusUnconfigured && !r.HasNumbers() {
			continue
		}
		out = append(out, r)
	}
	return out
}

func writeJSON(reports []provider.Report, now time.Time) error {
	clean := make([]provider.Report, len(reports))
	for i, r := range reports {
		// The fingerprint is an internal cache key and means nothing outside
		// this process. The account stays: the table names it too, and a
		// machine consumer aggregating several accounts needs to tell them
		// apart.
		r.TokenFP = ""
		if r.Windows == nil {
			r.Windows = []provider.Window{}
		}
		clean[i] = r
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(struct {
		SchemaVersion int               `json:"schema_version"`
		GeneratedAt   time.Time         `json:"generated_at"`
		Providers     []provider.Report `json:"providers"`
	}{1, now, clean})
}

func writeDebug(reports []provider.Report, now time.Time) {
	for _, r := range reports {
		age := "-"
		if !r.FetchedAt.IsZero() {
			age = now.Sub(r.FetchedAt).Truncate(time.Second).String()
		}
		fmt.Fprintf(os.Stderr, "debug: %-8s status=%-12s source=%-5s age=%-8s cred=%s %s\n",
			r.ID, r.Status, dash(string(r.Source)), age, dash(r.CredPath), r.Reason)
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func exitCode(reports []provider.Report, o options) int {
	var anyNumbers, anyError, anyStale, overLimit, anyDrift bool
	for _, r := range reports {
		if r.HasNumbers() {
			anyNumbers = true
		}
		if r.Drifted() {
			anyDrift = true
		}
		switch r.Status {
		case provider.StatusError:
			anyError = true
		case provider.StatusStale:
			anyStale = true
		}
		if o.failOver >= 0 {
			for _, w := range r.Windows {
				if !w.Unlimited && w.UsedPercent >= o.failOver {
					overLimit = true
				}
			}
		}
	}
	switch {
	case !anyNumbers && !o.check:
		return exitNoData
	// Drift means we may be printing a confident wrong number, so it fails the
	// run even though a value was produced.
	case anyError, anyDrift, o.strict && anyStale, overLimit:
		return exitFailure
	}
	return exitOK
}

func useColor(o options) bool {
	if o.noColor || o.colorArg == "never" {
		return false
	}
	if o.colorArg == "always" {
		return true
	}
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func usage(fs *flag.FlagSet) {
	fmt.Fprint(os.Stderr, `ai-usage — pourcentage d'usage de tes abonnements IA

Usage:
  ai-usage [flags]

Lit les credentials déjà présents dans $HOME (claude, codex, pi, grok, opencode)
en lecture seule, et n'écrit jamais dans les fichiers d'auth d'un autre outil.

Flags:
`)
	fs.PrintDefaults()
	fmt.Fprint(os.Stderr, `
Codes de sortie:
  0  tout va bien
  1  au moins un provider en échec (ou --strict / --fail-over déclenché)
  2  erreur d'usage
  3  aucune donnée exploitable
`)
}
