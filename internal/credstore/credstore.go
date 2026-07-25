// Package credstore discovers the OAuth credentials the already-installed CLIs
// wrote to $HOME.
//
// Everything here is strictly read-only. We never refresh a token and never
// write to another tool's auth file: OAuth refresh rotates the refresh token
// server-side, which would invalidate the copy claude/codex/pi/grok still hold
// and could log the user out of their primary coding tool. On expiry we degrade
// and say what to run instead.
package credstore

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Cred is one usable credential plus whatever non-secret metadata came with it.
type Cred struct {
	Path      string    // file it came from, for display
	Token     string    // the secret; never logged, never persisted
	Expires   time.Time // zero means unknown
	AccountID string
	Plan      string
	Tier      string
	Email     string // PII: only shown under --verbose
}

// Skew is the safety margin before treating a token as expired.
const Skew = 60 * time.Second

// Expired reports whether the token is past its usable life.
func (c Cred) Expired(now time.Time) bool {
	if c.Expires.IsZero() {
		return false // unknown expiry: let the server decide
	}
	return !now.Add(Skew).Before(c.Expires)
}

// Fingerprint keys a cache entry to a token without storing the token.
func Fingerprint(tok string) string {
	if tok == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:4])
}

// Home is $HOME, honoured dynamically so tests can point it at a temp dir.
func Home() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return os.Getenv("HOME")
}

func home(parts ...string) string {
	return filepath.Join(append([]string{Home()}, parts...)...)
}

// Display shortens a path for output, so we can name the credential source
// without ever printing its contents.
func Display(p string) string {
	h := Home()
	if h != "" && strings.HasPrefix(p, h+string(filepath.Separator)) {
		return "~" + p[len(h):]
	}
	return p
}

// ErrMissing means the file simply is not there: a soft skip, not a failure.
var ErrMissing = errors.New("not found")

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrMissing
		}
		return fmt.Errorf("%s: %w", Display(path), err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: malformed JSON", Display(path))
	}
	return nil
}

// ---------------------------------------------------------------- JWT

// DecodeJWT returns the payload claims. It does NOT verify the signature: we
// only ever read our own token in order to display its plan label, we never
// trust it for authentication.
func DecodeJWT(tok string) (map[string]any, error) {
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return nil, errors.New("not a JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, errors.New("bad JWT payload encoding")
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil, errors.New("bad JWT payload JSON")
	}
	return claims, nil
}

// JWTExpiry reads the standard exp claim.
func JWTExpiry(tok string) (time.Time, bool) {
	claims, err := DecodeJWT(tok)
	if err != nil {
		return time.Time{}, false
	}
	exp, ok := numeric(claims["exp"])
	if !ok || exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(exp), 0), true
}

// openAIAuthClaim is the namespaced claim carrying the ChatGPT plan.
const openAIAuthClaim = "https://api.openai.com/auth"

func numeric(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	return 0, false
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// ---------------------------------------------------------------- Claude

type claudeCredFile struct {
	OAuth struct {
		AccessToken      string `json:"accessToken"`
		ExpiresAt        int64  `json:"expiresAt"` // epoch MILLIseconds
		SubscriptionType string `json:"subscriptionType"`
		RateLimitTier    string `json:"rateLimitTier"`
	} `json:"claudeAiOauth"`
}

// Claude reads ~/.claude/.credentials.json.
func Claude() (Cred, error) {
	p := home(".claude", ".credentials.json")
	var f claudeCredFile
	if err := readJSON(p, &f); err != nil {
		return Cred{}, err
	}
	if f.OAuth.AccessToken == "" {
		return Cred{}, ErrMissing
	}
	c := Cred{
		Path:  p,
		Token: f.OAuth.AccessToken,
		Plan:  f.OAuth.SubscriptionType,
		Tier:  f.OAuth.RateLimitTier,
	}
	if f.OAuth.ExpiresAt > 0 {
		c.Expires = time.UnixMilli(f.OAuth.ExpiresAt)
	}
	c.Email = claudeEmail()
	return c, nil
}

func claudeEmail() string {
	var top struct {
		OAuthAccount struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	if err := readJSON(home(".claude.json"), &top); err != nil {
		return ""
	}
	return top.OAuthAccount.EmailAddress
}

// ---------------------------------------------------------------- Codex

type codexAuthFile struct {
	AuthMode string  `json:"auth_mode"`
	APIKey   *string `json:"OPENAI_API_KEY"`
	Tokens   struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
	} `json:"tokens"`
}

// ErrAPIKeyMode means codex is authenticated with an API key, so there is no
// subscription usage to report.
var ErrAPIKeyMode = errors.New("codex is in API-key mode; no subscription usage")

// Codex returns candidate ChatGPT credentials, best source first.
//
// Freshness comes from the access token's own exp claim. Deliberately we never
// read id_token and never look at last_refresh: on a real install the id_token
// can be days expired while the access token is still valid for days, and a
// naive check would report ChatGPT as expired when it is perfectly usable.
func Codex() ([]Cred, error) {
	var out []Cred
	var firstErr error

	p := home(".codex", "auth.json")
	var f codexAuthFile
	switch err := readJSON(p, &f); {
	case err == nil:
		if f.AuthMode != "" && f.AuthMode != "chatgpt" {
			firstErr = ErrAPIKeyMode
		} else if f.Tokens.AccessToken != "" {
			out = append(out, chatgptCred(p, f.Tokens.AccessToken, f.Tokens.AccountID, time.Time{}))
		}
	case errors.Is(err, ErrMissing):
	default:
		firstErr = err
	}

	if c, ok := piCred("openai-codex"); ok {
		out = append(out, c)
	}
	if len(out) == 0 {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, ErrMissing
	}
	return out, nil
}

func chatgptCred(path, tok, accountID string, expires time.Time) Cred {
	c := Cred{Path: path, Token: tok, AccountID: accountID, Expires: expires}
	if exp, ok := JWTExpiry(tok); ok {
		// Take the tighter of the two when the file also states an expiry.
		if c.Expires.IsZero() || exp.Before(c.Expires) {
			c.Expires = exp
		}
	}
	if claims, err := DecodeJWT(tok); err == nil {
		if auth, ok := claims[openAIAuthClaim].(map[string]any); ok {
			c.Plan = str(auth["chatgpt_plan_type"])
			if c.AccountID == "" {
				c.AccountID = str(auth["chatgpt_account_id"])
			}
		}
		if profile, ok := claims["https://api.openai.com/profile"].(map[string]any); ok {
			c.Email = str(profile["email"])
		}
		if c.Email == "" {
			c.Email = str(claims["email"])
		}
	}
	return c
}

// ---------------------------------------------------------------- pi

type piAuthEntry struct {
	Type      string `json:"type"`
	Access    string `json:"access"`
	Expires   int64  `json:"expires"` // epoch milliseconds
	AccountID string `json:"accountId"`
}

func piAuthPath() string { return home(".pi", "agent", "auth.json") }

func piEntry(provider string) (piAuthEntry, string, bool) {
	p := piAuthPath()
	var f map[string]piAuthEntry
	if err := readJSON(p, &f); err != nil {
		return piAuthEntry{}, p, false
	}
	e, ok := f[provider]
	if !ok || e.Type != "oauth" || e.Access == "" {
		return piAuthEntry{}, p, false
	}
	return e, p, true
}

func piCred(provider string) (Cred, bool) {
	e, p, ok := piEntry(provider)
	if !ok {
		return Cred{}, false
	}
	var exp time.Time
	if e.Expires > 0 {
		exp = time.UnixMilli(e.Expires)
	}
	if provider == "openai-codex" {
		return chatgptCred(p, e.Access, e.AccountID, exp), true
	}
	c := Cred{Path: p, Token: e.Access, AccountID: e.AccountID, Expires: exp}
	if jexp, ok := JWTExpiry(e.Access); ok && (c.Expires.IsZero() || jexp.Before(c.Expires)) {
		c.Expires = jexp
	}
	return c, true
}

// ---------------------------------------------------------------- Grok

type grokAuthEntry struct {
	Key       string `json:"key"`
	AuthMode  string `json:"auth_mode"`
	ExpiresAt string `json:"expires_at"` // ISO8601
	Email     string `json:"email"`
}

// Grok returns candidate xAI credentials, best source first.
//
// ~/.grok/auth.json is a map keyed by "<issuer>::<client-uuid>" and the token
// lives in "key", not in anything named like a token. pi keeps its own,
// independent xAI session, which is why it is a real fallback and not a copy.
func Grok() ([]Cred, error) {
	var out []Cred

	p := home(".grok", "auth.json")
	var f map[string]grokAuthEntry
	if err := readJSON(p, &f); err == nil {
		keys := make([]string, 0, len(f))
		for k := range f {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic when several sessions coexist
		for _, k := range keys {
			e := f[k]
			if e.Key == "" || !strings.HasPrefix(k, "https://auth.x.ai::") {
				continue
			}
			c := Cred{Path: p, Token: e.Key, Email: e.Email}
			if t, err := time.Parse(time.RFC3339, e.ExpiresAt); err == nil {
				c.Expires = t
			}
			if jexp, ok := JWTExpiry(e.Key); ok && (c.Expires.IsZero() || jexp.Before(c.Expires)) {
				c.Expires = jexp
			}
			out = append(out, c)
		}
	}

	if c, ok := piCred("xai"); ok {
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, ErrMissing
	}
	return out, nil
}

// ---------------------------------------------------------------- Copilot

// Copilot returns candidate GitHub tokens able to read the Copilot quota.
func Copilot() ([]Cred, error) {
	var out []Cred

	// Standard Copilot editor plugin locations.
	for _, name := range []string{"hosts.json", "apps.json"} {
		p := home(".config", "github-copilot", name)
		var f map[string]struct {
			OAuthToken string `json:"oauth_token"`
			User       string `json:"user"`
		}
		if err := readJSON(p, &f); err != nil {
			continue
		}
		keys := make([]string, 0, len(f))
		for k := range f {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if e := f[k]; e.OAuthToken != "" {
				out = append(out, Cred{Path: p, Token: e.OAuthToken, Email: e.User})
			}
		}
	}

	// opencode keeps a github-copilot OAuth token too.
	{
		p := filepath.Join(xdgData(), "opencode", "auth.json")
		var f map[string]struct {
			Type   string `json:"type"`
			Access string `json:"access"`
		}
		if err := readJSON(p, &f); err == nil {
			if e, ok := f["github-copilot"]; ok && e.Access != "" {
				out = append(out, Cred{Path: p, Token: e.Access})
			}
		}
	}

	for _, env := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := os.Getenv(env); v != "" {
			out = append(out, Cred{Path: "$" + env, Token: v})
		}
	}

	if len(out) == 0 {
		return nil, ErrMissing
	}
	return out, nil
}

func xdgData() string {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return v
	}
	return home(".local", "share")
}

// ---------------------------------------------------------------- selection

// Choose picks the first candidate that is still valid, in declared preference
// order. If every candidate is expired it returns the one that expired most
// recently together with ok=false, so the caller can say how stale things are.
func Choose(cands []Cred, now time.Time) (Cred, bool) {
	for _, c := range cands {
		if !c.Expired(now) {
			return c, true
		}
	}
	var best Cred
	for _, c := range cands {
		if best.Token == "" || c.Expires.After(best.Expires) {
			best = c
		}
	}
	return best, false
}

// ---------------------------------------------------------------- versions

const (
	fallbackClaudeVersion = "2.1.220"
	fallbackCodexVersion  = "0.145.0"
)

var semverish = func(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		if _, err := strconv.Atoi(p); err != nil {
			return false
		}
	}
	return true
}

// ClaudeCodeVersion resolves the installed Claude Code version without
// spawning anything: ~/.local/bin/claude is a symlink whose target basename is
// the version. We always send the version that is actually installed rather
// than inventing one.
func ClaudeCodeVersion() string {
	if v := os.Getenv("CLAUDE_CODE_VERSION"); semverish(v) {
		return v
	}
	for _, p := range []string{
		home(".local", "bin", "claude"),
		"/usr/local/bin/claude",
	} {
		if target, err := filepath.EvalSymlinks(p); err == nil {
			if base := filepath.Base(target); semverish(base) {
				return base
			}
		}
	}
	if v := newestVersionDir(home(".local", "share", "claude", "versions")); v != "" {
		return v
	}
	return fallbackClaudeVersion
}

// CodexVersion resolves the installed codex-cli version.
func CodexVersion() string {
	var vf struct {
		LatestVersion string `json:"latest_version"`
	}
	if err := readJSON(home(".codex", "version.json"), &vf); err == nil && semverish(vf.LatestVersion) {
		return vf.LatestVersion
	}
	// .../packages/standalone/releases/<version>-<triple>/bin/codex
	if target, err := filepath.EvalSymlinks(home(".local", "bin", "codex")); err == nil {
		for _, seg := range strings.Split(target, string(filepath.Separator)) {
			if i := strings.IndexByte(seg, '-'); i > 0 && semverish(seg[:i]) {
				return seg[:i]
			}
		}
	}
	return fallbackCodexVersion
}

func newestVersionDir(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var best string
	var bestKey []int
	for _, e := range entries {
		name := e.Name()
		if !semverish(name) {
			continue
		}
		key := versionKey(name)
		if best == "" || less(bestKey, key) {
			best, bestKey = name, key
		}
	}
	return best
}

func versionKey(v string) []int {
	parts := strings.Split(v, ".")
	out := make([]int, len(parts))
	for i, p := range parts {
		out[i], _ = strconv.Atoi(p)
	}
	return out
}

func less(a, b []int) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}
