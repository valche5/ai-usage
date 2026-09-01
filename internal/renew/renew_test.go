package renew

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/valche5/ai-usage/internal/credstore"
)

func TestStrictlyExpired(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name string
		exp  time.Time
		want bool
	}{
		{"unknown", time.Time{}, false},
		{"future", now.Add(time.Nanosecond), false},
		{"equal", now, true},
		{"past", now.Add(-time.Nanosecond), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := strictlyExpired(credstore.Cred{Expires: tt.exp}, now); got != tt.want {
				t.Fatalf("strictlyExpired() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRenewPostLockStatesNeverLaunch(t *testing.T) {
	now := time.Now()
	expired := credstore.Cred{Expires: now.Add(-time.Hour)}
	valid := credstore.Cred{Expires: now.Add(time.Hour)}
	readErr := errors.New("malformed")

	for _, tt := range []struct {
		name       string
		secondCred credstore.Cred
		exists     bool
		err        error
		want       Status
	}{
		{"unreadable", credstore.Cred{}, false, readErr, StatusError},
		{"disappeared-or-api-key-mode", credstore.Cred{}, false, nil, StatusUnconfigured},
		{"refreshed-by-other-process", valid, true, nil, StatusAlreadyValid},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			reads := 0
			launched := false
			client := Client{
				ID:   "test-" + tt.name,
				Name: tt.name,
				Wait: time.Second,
				cred: func() (credstore.Cred, bool, error) {
					reads++
					if reads == 1 {
						return expired, true, nil
					}
					return tt.secondCred, tt.exists, tt.err
				},
				Cmd: func() *exec.Cmd {
					launched = true
					return helperCommand("success")
				},
			}

			result := Renew(context.Background(), []Client{client}, now)[0]
			if result.Status != tt.want {
				t.Fatalf("status = %q (%s), want %q", result.Status, result.Reason, tt.want)
			}
			if launched {
				t.Fatal("CLI command was constructed for an unconfirmed credential")
			}
		})
	}
}

func TestRenewFakeCommandSuccess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	now := time.Now()
	expired := credstore.Cred{Expires: now.Add(-time.Hour)}
	valid := credstore.Cred{Expires: now.Add(time.Hour)}
	reads := 0
	client := Client{
		ID:   "fake-success",
		Name: "Fake",
		Wait: 2 * time.Second,
		cred: func() (credstore.Cred, bool, error) {
			reads++
			if reads < 3 {
				return expired, true, nil
			}
			return valid, true, nil
		},
		Cmd: func() *exec.Cmd { return helperCommand("success") },
	}

	result := Renew(context.Background(), []Client{client}, now)[0]
	if result.Status != StatusOK {
		t.Fatalf("status = %q (%s), want %q", result.Status, result.Reason, StatusOK)
	}
}

func TestRenewDeadlineKillsFakeCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	now := time.Now()
	expired := credstore.Cred{Expires: now.Add(-time.Hour)}
	client := Client{
		ID:   "fake-timeout",
		Name: "Fake",
		Wait: 75 * time.Millisecond,
		cred: func() (credstore.Cred, bool, error) {
			return expired, true, nil
		},
		Cmd: func() *exec.Cmd { return helperCommand("sleep") },
	}

	start := time.Now()
	result := Renew(context.Background(), []Client{client}, now)[0]
	if result.Status != StatusError {
		t.Fatalf("status = %q, want %q", result.Status, StatusError)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("renewal exceeded bounded deadline: %s", elapsed)
	}
}

func TestLockHonorsContext(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	unlock, err := lock(context.Background(), "contended")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := lock(ctx, "contended"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock error = %v, want context deadline", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("lock ignored context for %s", elapsed)
	}
}

func TestScrubCLIEnv(t *testing.T) {
	env := []string{
		"PATH=/trusted/bin",
		"CODEX_HOME=/wrong-codex",
		"CLAUDE_CONFIG_DIR=/wrong-claude",
		"CLAUDE_CODE_OAUTH_TOKEN=secret",
		"OPENAI_API_KEY=secret",
		"KEEP_ME=yes",
	}
	got := scrubCLIEnv(env)
	joined := strings.Join(got, "\n")
	for _, forbidden := range []string{"CODEX_HOME=", "CLAUDE_CONFIG_DIR=", "CLAUDE_CODE_OAUTH_TOKEN=", "OPENAI_API_KEY="} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("scrubCLIEnv retained %q in %q", forbidden, joined)
		}
	}
	for _, retained := range []string{"PATH=/trusted/bin", "KEEP_ME=yes"} {
		if !strings.Contains(joined, retained) {
			t.Fatalf("scrubCLIEnv removed %q from %q", retained, joined)
		}
	}
}

func helperCommand(mode string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestRenewHelperProcess$")
	cmd.Env = append(os.Environ(), "AI_USAGE_RENEW_HELPER=1", "AI_USAGE_RENEW_HELPER_MODE="+mode)
	return cmd
}

func TestRenewHelperProcess(t *testing.T) {
	if os.Getenv("AI_USAGE_RENEW_HELPER") != "1" {
		return
	}
	if os.Getenv("AI_USAGE_RENEW_HELPER_MODE") == "sleep" {
		time.Sleep(10 * time.Second)
	}
}
