// Package cache persists normalized reports between runs.
//
// This is not a convenience: it is the mechanism that enforces Anthropic's
// "at least 180s between calls" guidance. That rate limit is scoped to the
// access token, which means it is shared with the user's real Claude Code
// client — polling hard here would throttle their actual work.
//
// Only normalized reports are stored. Never a token, never a raw response,
// never a header.
package cache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/valche5/ai-usage/internal/provider"
)

// Entry is one cached report plus when we wrote it.
type Entry struct {
	Report   provider.Report `json:"report"`
	StoredAt time.Time       `json:"stored_at"`
}

// Cache is a single JSON file holding one entry per provider.
//
// Providers are collected concurrently, so every accessor takes the mutex.
type Cache struct {
	mu      sync.Mutex
	path    string
	enabled bool
	dirty   bool
	entries map[string]Entry
}

// DefaultDir is $XDG_CACHE_HOME/ai-usage, falling back to ~/.cache/ai-usage.
func DefaultDir() string {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(os.TempDir(), "ai-usage")
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "ai-usage")
}

// Open loads the cache. A missing or corrupt file is not an error: we start
// empty rather than fail the run.
func Open(dir string, enabled bool) *Cache {
	c := &Cache{
		path:    filepath.Join(dir, "usage.json"),
		enabled: enabled,
		entries: map[string]Entry{},
	}
	if !enabled {
		return c
	}
	b, err := os.ReadFile(c.path)
	if err != nil {
		return c
	}
	var onDisk struct {
		Entries map[string]Entry `json:"entries"`
	}
	if err := json.Unmarshal(b, &onDisk); err != nil || onDisk.Entries == nil {
		return c
	}
	c.entries = onDisk.Entries
	return c
}

// Get returns the cached entry for id, provided it was stored against the same
// token fingerprint. A different fingerprint means the account changed, so the
// entry is discarded — without us ever having stored the token itself.
func (c *Cache) Get(id, fingerprint string) (Entry, bool) {
	if !c.enabled {
		return Entry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[id]
	if !ok {
		return Entry{}, false
	}
	if fingerprint != "" && e.Report.TokenFP != "" && e.Report.TokenFP != fingerprint {
		return Entry{}, false
	}
	return e, true
}

// Fresh reports whether e is within ttl of now.
func (e Entry) Fresh(now time.Time, ttl time.Duration) bool {
	return !e.StoredAt.IsZero() && now.Sub(e.StoredAt) < ttl
}

// Age is how long ago the entry was stored.
func (e Entry) Age(now time.Time) time.Duration {
	if e.StoredAt.IsZero() {
		return 0
	}
	return now.Sub(e.StoredAt)
}

// Put records a report. Only successful, number-bearing reports are worth
// keeping: caching a failure would just replay the failure.
func (c *Cache) Put(r provider.Report, now time.Time) {
	if !c.enabled || !r.HasNumbers() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[r.ID] = Entry{Report: r, StoredAt: now}
	c.dirty = true
}

// Flush writes the cache atomically: a temp file in the same directory, then a
// rename, so a crash can never leave a half-written cache behind.
func (c *Cache) Flush() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enabled || !c.dirty {
		return nil
	}
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("cache dir: %w", err)
	}
	b, err := json.MarshalIndent(struct {
		Entries map[string]Entry `json:"entries"`
	}{c.entries}, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", c.path, os.Getpid())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, c.path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
