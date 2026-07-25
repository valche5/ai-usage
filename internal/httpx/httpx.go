// Package httpx centralizes outbound HTTP: hardcoded hosts, short timeouts,
// no retries, and mandatory redaction of anything that could echo a token back
// into a log or an error message.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

// HostileEnv are variables that would silently redirect or re-authenticate our
// requests. main() unsets them before the first request is ever built, which
// makes the "oops I hit the local proxy" class of bug structurally impossible
// rather than a discipline each provider has to remember.
var HostileEnv = []string{
	"OPENAI_BASE_URL", "OPENAI_API_KEY",
	"ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN",
	"XAI_API_KEY", "GROK_API_KEY",
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
	"http_proxy", "https_proxy", "all_proxy",
}

// ScrubEnv removes HostileEnv from the process environment. Call it first in
// main, before any http.Client is used: Go's default transport reads the proxy
// variables lazily on first use.
func ScrubEnv() {
	for _, k := range HostileEnv {
		os.Unsetenv(k)
	}
}

// secretish matches every token shape we handle, so an error body can never
// carry one to the terminal.
var secretish = regexp.MustCompile(strings.Join([]string{
	`sk-ant-[A-Za-z0-9_-]+`,
	`rt\.[0-9]\.[A-Za-z0-9_.\-]+`,
	`ey[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]*`,
	`gh[opsu]_[A-Za-z0-9]+`,
	`sk-[A-Za-z0-9]{16,}`,
	`Bearer\s+[A-Za-z0-9._\-]+`,
}, "|"))

// Redact replaces anything token-shaped with a placeholder.
func Redact(s string) string {
	return secretish.ReplaceAllString(s, "[redacted]")
}

// Client builds the single http.Client used by every provider. Proxy is nil on
// purpose: we never want an interception layer between us and these hosts.
func Client(timeout time.Duration) *http.Client {
	tr := &http.Transport{
		Proxy:               nil,
		MaxIdleConns:        8,
		IdleConnTimeout:     20 * time.Second,
		TLSHandshakeTimeout: 8 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

// Req describes one request. There is deliberately no retry knob: on 429 the
// caller falls back to cached data rather than compounding the throttle.
type Req struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    []byte
}

// Res is a response with an already-redacted, already-truncated body excerpt.
type Res struct {
	Status  int
	Body    []byte
	Excerpt string
}

const maxExcerpt = 200

// Do performs the request. The returned error never contains a secret.
func Do(ctx context.Context, c *http.Client, r Req) (*Res, error) {
	method := r.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if len(r.Body) > 0 {
		body = bytes.NewReader(r.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.URL, body)
	if err != nil {
		return nil, fmt.Errorf("bad request: %s", Redact(err.Error()))
	}
	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, classify(err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading response: %s", Redact(err.Error()))
	}
	ex := Redact(strings.TrimSpace(string(b)))
	if len(ex) > maxExcerpt {
		ex = ex[:maxExcerpt] + "…"
	}
	return &Res{Status: resp.StatusCode, Body: b, Excerpt: ex}, nil
}

// JSON performs the request and decodes a JSON body into v.
func JSON(ctx context.Context, c *http.Client, r Req, v any) (*Res, error) {
	res, err := Do(ctx, c, r)
	if err != nil {
		return nil, err
	}
	if res.Status < 200 || res.Status > 299 {
		return res, HTTPError{Status: res.Status, Excerpt: res.Excerpt}
	}
	if err := json.Unmarshal(res.Body, v); err != nil {
		return res, fmt.Errorf("unexpected response shape (HTTP %d)", res.Status)
	}
	return res, nil
}

// HTTPError is a non-2xx response.
type HTTPError struct {
	Status  int
	Excerpt string
}

func (e HTTPError) Error() string {
	switch e.Status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Sprintf("HTTP %d — token rejected", e.Status)
	case http.StatusTooManyRequests:
		return "HTTP 429 — rate-limited"
	}
	if e.Excerpt != "" {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Excerpt)
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

// IsRateLimited reports whether err is a 429, which callers treat as "serve
// cached data now" rather than "retry".
func IsRateLimited(err error) bool {
	var he HTTPError
	return errors.As(err, &he) && he.Status == http.StatusTooManyRequests
}

// IsUnauthorized reports whether the server refused our credential.
func IsUnauthorized(err error) bool {
	var he HTTPError
	if !errors.As(err, &he) {
		return false
	}
	return he.Status == http.StatusUnauthorized || he.Status == http.StatusForbidden
}

func classify(err error) error {
	s := err.Error()
	switch {
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(s, "context deadline exceeded"),
		strings.Contains(s, "Client.Timeout"):
		return errors.New("timed out")
	case strings.Contains(s, "no such host"):
		return errors.New("offline (DNS failed)")
	case strings.Contains(s, "connection refused"):
		return errors.New("connection refused")
	case strings.Contains(s, "network is unreachable"):
		return errors.New("network unreachable")
	case errors.Is(err, context.Canceled):
		return errors.New("cancelled")
	}
	return errors.New(Redact(s))
}
