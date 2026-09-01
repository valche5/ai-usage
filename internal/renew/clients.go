// Concrete renew clients: how to refresh each provider's credential using its
// own CLI in a cheap, non-interactive print invocation.
//
// The renewal decision uses a *strict* expiry predicate (no safety skew): the
// requirement is "never renew unless the token is actually expired", whereas
// the report path's skewed predicate is about whether a token is safe to use
// over HTTP. The two intents differ, so they use different predicates.
package renew

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/valche5/ai-usage/internal/credstore"
)

// Claude returns the client that renews the Anthropic subscription token.
//
// The bootstrap OAuth refresh runs as soon as `claude` starts — before any
// model call. The trivial prompt, pinned to the cheap Haiku model, gives the
// CLI a reason to reach that bootstrap and then exit on its own.
func Claude() Client {
	return Client{
		ID:      "claude",
		Name:    "Claude",
		Relogin: "claude",
		cred:    readClaudeCred,
		Cmd: func() *exec.Cmd {
			return cliCommand(claudeBin(), "-p", "OK", "--model", "haiku")
		},
	}
}

// Codex returns the client that renews the ChatGPT subscription token via the
// codex CLI. It reads ONLY ~/.codex/auth.json — never the Pi fallback — so the
// credential being renewed is exactly the one the codex CLI owns (we would not
// want to launch `codex` to refresh a Pi-side credential).
func Codex() Client {
	return Client{
		ID:      "chatgpt",
		Name:    "ChatGPT",
		Relogin: "codex login",
		cred:    readCodexCLI,
		Cmd: func() *exec.Cmd {
			cmd := cliCommand(codexBin(), "exec", "OK", "-m", "luna")
			// codex exec refuses to run outside a trusted directory.
			cmd.Args = append(cmd.Args, "--skip-git-repo-check")
			return cmd
		},
	}
}

// readClaudeCred reads the claude credential. A present-but-malformed file is
// surfaced distinctly from a missing one so we can report it as an error
// rather than as an unconfigured skip.
func readClaudeCred() (credstore.Cred, bool, error) {
	c, err := credstore.Claude()
	if errors.Is(err, credstore.ErrMissing) {
		return credstore.Cred{}, false, nil
	}
	if err != nil {
		return credstore.Cred{}, false, err
	}
	return c, true, nil
}

// codexCLIFile mirrors the subset of ~/.codex/auth.json the codex CLI owns.
type codexCLIFile struct {
	AuthMode string `json:"auth_mode"`
	Tokens   struct {
		AccessToken string `json:"access_token"`
	} `json:"tokens"`
}

// readCodexCLI reads ONLY the codex CLI's own credential file, never the Pi
// fallback, so renewal targets exactly what the codex binary refreshes.
func readCodexCLI() (credstore.Cred, bool, error) {
	p := filepath.Join(credstore.Home(), ".codex", "auth.json")
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return credstore.Cred{}, false, nil
	}
	if err != nil {
		return credstore.Cred{}, false, err
	}
	var f codexCLIFile
	if err := json.Unmarshal(b, &f); err != nil {
		return credstore.Cred{}, false, err
	}
	if f.AuthMode != "" && f.AuthMode != "chatgpt" {
		// API-key mode: no subscription token to refresh.
		return credstore.Cred{}, false, nil
	}
	if f.Tokens.AccessToken == "" {
		return credstore.Cred{}, false, nil
	}
	c := credstore.Cred{Path: p, Token: f.Tokens.AccessToken}
	if exp, ok := credstore.JWTExpiry(f.Tokens.AccessToken); ok {
		c.Expires = exp
	}
	return c, true, nil
}

// claudeBin resolves the installed claude binary from PATH, keeping the
// invoked version aligned with the one whose User-Agent the usage endpoint
// expects.
func claudeBin() string {
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	return "claude"
}

func codexBin() string {
	if p, err := exec.LookPath("codex"); err == nil {
		return p
	}
	return "codex"
}

// cliCommand aligns the child with the default credential locations inspected
// above. In particular, config-home or environment-token overrides must not
// make the CLI refresh a different account. The application-owned work
// directory also prevents project-level instructions from affecting the tiny
// bootstrap prompt.
func cliCommand(bin string, args ...string) *exec.Cmd {
	cmd := exec.Command(bin, args...)
	cmd.Dir = filepath.Join(credstore.Home(), ".ai-usage")
	cmd.Env = scrubCLIEnv(os.Environ())
	return cmd
}

var cliEnvDenied = map[string]struct{}{
	"ANTHROPIC_API_KEY":       {},
	"ANTHROPIC_AUTH_TOKEN":    {},
	"ANTHROPIC_BASE_URL":      {},
	"CLAUDE_CODE_OAUTH_TOKEN": {},
	"CLAUDE_CONFIG_DIR":       {},
	"CODEX_HOME":              {},
	"OPENAI_API_KEY":          {},
	"OPENAI_BASE_URL":         {},
}

func scrubCLIEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, denied := cliEnvDenied[strings.ToUpper(key)]; denied {
			continue
		}
		out = append(out, entry)
	}
	return out
}
