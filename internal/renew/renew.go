// Package renew refreshes an expired subscription access token by running the
// provider's own CLI once, non-interactively, in print mode, with a trivial
// prompt pinned to a cheap model. The OAuth refresh happens during the CLI's
// startup bootstrap, *before* any model call — the trivial prompt merely gives
// the CLI a reason to reach that bootstrap and exit on its own. Renew then
// re-reads the credential file and confirms the token became usable again.
//
// This is deliberately zero-dependency and pty-free: there is no cross-account
// refresh and ai-usage never writes to or parses a refresh token — the owning
// CLI refreshes with its own identity and stored refresh token, and ai-usage
// only launches the tool and inspects the file it controls. (It does read the
// access token — that is unavoidable to decide expiry.)
//
// Error path: when the refresh token is dead (OAuth session expired
// server-side), the CLI's bootstrap cannot renew and the token never becomes
// valid again. Renew detects this (token still expired after the run) and
// reports "please re-login" — the web OAuth flow is the one step that
// genuinely needs the human.
package renew

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/valche5/ai-usage/internal/credstore"
)

// credRead returns the credential, whether it exists, and any read error.
// A present-but-malformed file is reported as an error, distinct from a
// missing one (a soft skip).
type credRead func() (credstore.Cred, bool, error)

// Client describes how to renew one provider's credential using its own CLI.
type Client struct {
	ID   string // provider id (must match provider.Provider.ID)
	Name string

	// Relogin is the command to hand to the human when the refresh token is
	// dead (e.g. "claude" or "codex login").
	Relogin string

	cred credRead

	// Cmd builds the non-interactive print command that triggers the CLI's
	// OAuth bootstrap refresh, pinned to a cheap model.
	Cmd func() *exec.Cmd

	// Wait bounds how long renew waits for the CLI to finish (its bootstrap
	// refresh) before giving up and reporting "please re-login".
	Wait time.Duration
}

// Result is the outcome of renewing one provider.
type Result struct {
	ID     string
	Name   string
	Status Status
	Reason string
	Took   time.Duration
}

// Status describes a renewal outcome.
type Status string

const (
	StatusOK           Status = "ok"
	StatusUnconfigured Status = "unconfigured"  // no credential file: nothing to do
	StatusAlreadyValid Status = "already-valid" // token not expired: deliberately skipped
	StatusError        Status = "error"         // refresh token dead / run failed
)

// DefaultWait bounds the entire per-client renewal: credential checks, lock
// acquisition, CLI execution, and normal reaping. A successful bootstrap
// refresh typically lands within a couple of seconds.
const DefaultWait = 90 * time.Second

// strictlyExpired reports strict expiry with no safety skew: the reporting
// path's 60-second skew exists to avoid using a token over HTTP just before it
// dies, but the "never renew an un-expired token" requirement is literal.
func strictlyExpired(cr credstore.Cred, now time.Time) bool {
	if cr.Expires.IsZero() {
		return false // unknown expiry: let the server decide
	}
	return !now.Before(cr.Expires)
}

// Seal defaults any zero field on c so callers can write sparse literals.
func (c Client) Seal() Client {
	if c.Wait == 0 {
		c.Wait = DefaultWait
	}
	return c
}

// Renew renews every client that is configured and genuinely expired, and
// returns one Result per client. Clients whose token is not expired are
// deliberately skipped (never launched).
func Renew(ctx context.Context, clients []Client, now time.Time) []Result {
	out := make([]Result, 0, len(clients))
	for _, raw := range clients {
		c := raw.Seal()
		// One per-client deadline covers the complete operation: credential
		// checks, waiting for the cross-process lock, launching, and reaping.
		clientCtx, cancel := context.WithTimeout(ctx, c.Wait)
		out = append(out, c.renew(clientCtx, now))
		cancel()
	}
	return out
}

// renew guards, renews (under a cross-process lock), and reports.
// Handles every post-lock state explicitly: a credential that became malformed,
// disappeared, or switched to API-key mode while waiting for the lock is never
// passed through to a launch — only a confirmed-expired subscription token is.
func (c Client) renew(ctx context.Context, now time.Time) Result {
	start := time.Now()

	cr, exists, err := c.cred()
	if err != nil {
		return Result{ID: c.ID, Name: c.Name, Status: StatusError,
			Reason: "credential illisible: " + err.Error(), Took: time.Since(start)}
	}
	if !exists {
		return Result{ID: c.ID, Name: c.Name, Status: StatusUnconfigured,
			Reason: "aucun credential"}
	}
	if !strictlyExpired(cr, now) {
		return Result{ID: c.ID, Name: c.Name, Status: StatusAlreadyValid,
			Reason: "token non expiré — rien à renouveler"}
	}

	// Serialize renewal per provider across processes: two concurrent
	// ai-usage invocations could otherwise both see the expired token and
	// both launch the CLI (matters with refresh-token rotation). The lock is
	// context-aware and bounded, so waiting on another process cannot hang or
	// outlive the caller's deadline.
	unlock, err := lock(ctx, c.ID)
	if err != nil {
		return Result{ID: c.ID, Name: c.Name, Status: StatusError,
			Reason: "verrou impossible: " + err.Error(), Took: time.Since(start)}
	}
	defer unlock()

	// Re-read under the lock: another ai-usage may have refreshed it, removed
	// it, or changed its mode in the meantime. Only a still-confirmed-expired
	// subscription token proceeds to a launch.
	switch cr, exists, err := c.cred(); {
	case err != nil:
		return Result{ID: c.ID, Name: c.Name, Status: StatusError,
			Reason: "credential illisible: " + err.Error(), Took: time.Since(start)}
	case !exists:
		return Result{ID: c.ID, Name: c.Name, Status: StatusUnconfigured,
			Reason: "credential disparu entre temps"}
	case !strictlyExpired(cr, time.Now()):
		return Result{ID: c.ID, Name: c.Name, Status: StatusAlreadyValid,
			Reason: "déjà rafraîchi (autre process)", Took: time.Since(start)}
	}

	// Confirm the caller still wants the work done (lock wait may have spent
	// the budget) before spawning the CLI.
	if err := ctx.Err(); err != nil {
		return Result{ID: c.ID, Name: c.Name, Status: StatusError,
			Reason: "interrompu: " + err.Error(), Took: time.Since(start)}
	}
	return c.launch(ctx, c.reloginHint())
}

// reloginHint returns the human-facing re-login command.
func (c Client) reloginHint() string {
	if c.Relogin != "" {
		return c.Relogin
	}
	return c.ID
}

// launch runs the CLI once, waits for it (with a bound), then verifies the
// token became valid again.
func (c Client) launch(ctx context.Context, hint string) Result {
	start := time.Now()
	if err := ctx.Err(); err != nil {
		return Result{ID: c.ID, Name: c.Name, Status: StatusError,
			Reason: interruptionReason(err), Took: time.Since(start)}
	}
	cmd := c.Cmd()
	prepareCommand(cmd)

	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err == nil {
		cmd.Stdout = null
		cmd.Stderr = null
		defer null.Close()
	}

	if err := cmd.Start(); err != nil {
		return Result{ID: c.ID, Name: c.Name, Status: StatusError,
			Reason: "lancement impossible: " + err.Error(), Took: time.Since(start)}
	}

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		killProcessTree(cmd)
		_, reaped := waitAfterKill(waitErr)
		valid, validErr := c.validNow()
		if valid {
			return Result{ID: c.ID, Name: c.Name, Status: StatusOK,
				Reason: "token rafraîchi", Took: time.Since(start)}
		}
		if validErr != nil {
			return Result{ID: c.ID, Name: c.Name, Status: StatusError,
				Reason: "credential illisible après renouvellement: " + validErr.Error(),
				Took:   time.Since(start)}
		}
		if !reaped {
			return Result{ID: c.ID, Name: c.Name, Status: StatusError,
				Reason: "interrompu; arrêt du CLI non confirmé", Took: time.Since(start)}
		}
		return Result{ID: c.ID, Name: c.Name, Status: StatusError,
			Reason: interruptionReason(ctx.Err()), Took: time.Since(start)}

	case werr := <-waitErr:
		valid, validErr := c.validNow()
		if valid {
			return Result{ID: c.ID, Name: c.Name, Status: StatusOK,
				Reason: "token rafraîchi", Took: time.Since(start)}
		}
		if validErr != nil {
			return Result{ID: c.ID, Name: c.Name, Status: StatusError,
				Reason: "credential illisible après renouvellement: " + validErr.Error(),
				Took:   time.Since(start)}
		}
		// Distinguish "CLI genuinely failed" from "CLI ran but the token is
		// still dead". The latter is the dead-refresh-token case.
		if werr != nil {
			return Result{ID: c.ID, Name: c.Name, Status: StatusError,
				Reason: "échec du CLI (" + werr.Error() + ") — token toujours expiré",
				Took:   time.Since(start)}
		}
		return Result{ID: c.ID, Name: c.Name, Status: StatusError,
			Reason: "refresh token invalide — relance manuelle `" + hint + "` requise (re-login web)",
			Took:   time.Since(start)}
	}
}

// validNow reports whether the credential file now holds a usable token.
func (c Client) validNow() (bool, error) {
	cr, exists, err := c.cred()
	if err != nil {
		return false, err
	}
	return exists && !cr.Expires.IsZero() && time.Now().Before(cr.Expires), nil
}

const postKillWait = 2 * time.Second

// waitAfterKill bounds how long the caller waits for the one goroutine that
// owns cmd.Wait. Wait is still called exactly once and will reap the process
// even in the pathological case where the OS does not report termination
// within the grace period.
func waitAfterKill(done <-chan error) (error, bool) {
	timer := time.NewTimer(postKillWait)
	defer timer.Stop()
	select {
	case err := <-done:
		return err, true
	case <-timer.C:
		return nil, false
	}
}

func interruptionReason(err error) string {
	if err == context.DeadlineExceeded {
		return "délai de renouvellement atteint"
	}
	return "interrompu"
}

// lock acquires an exclusive advisory lock (flock) for a provider so that two
// concurrent ai-usage processes never renew the same credential at once. It
// returns a release func and an error. The platform primitive is implemented
// without third-party dependencies.
const lockRetry = 25 * time.Millisecond

func lock(ctx context.Context, id string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := lockPath(id)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		acquired, err := tryFlock(f)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		if acquired {
			return func() {
				_ = f.Close() // closing releases the flock
			}, nil
		}

		timer := time.NewTimer(lockRetry)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// lockPath returns the cross-process lock file for a provider.
func lockPath(id string) string {
	return filepath.Join(credstore.Home(), ".ai-usage", "renew-"+id+".lock")
}
