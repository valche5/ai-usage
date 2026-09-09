package connection

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/valche5/ai-usage/internal/provider"
)

func testKey(fill byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
}

func TestStoreEncryptsAndReopensState(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, testKey(7))
	if err != nil {
		t.Fatal(err)
	}
	const secret = "secret-access-token-that-must-not-leak"
	if err := store.Put(Connection{Provider: "chatgpt", Kind: "oauth", Access: secret, Refresh: "refresh-secret"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutReports([]provider.Report{{ID: "chatgpt", Name: "ChatGPT", Status: provider.StatusOK}}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "state.enc"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(secret)) || bytes.Contains(raw, []byte("refresh-secret")) {
		t.Fatal("state.enc contains a plaintext token")
	}
	info, err := os.Stat(filepath.Join(dir, "state.enc"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state.enc mode = %o, want 600", info.Mode().Perm())
	}

	reopened, err := Open(dir, testKey(7))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.Get("chatgpt")
	if !ok || got.Access != secret || got.Refresh != "refresh-secret" {
		t.Fatalf("reopened connection = %#v, %v", got, ok)
	}
	if _, err := Open(dir, testKey(8)); err == nil {
		t.Fatal("opening with the wrong key unexpectedly succeeded")
	}
}

func TestOpenRejectsInvalidKey(t *testing.T) {
	if _, err := Open(t.TempDir(), "not-base64"); err == nil {
		t.Fatal("invalid key unexpectedly accepted")
	}
}

func TestReplaceInvalidatesPreviousReport(t *testing.T) {
	store, err := Open(t.TempDir(), testKey(9))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(Connection{Provider: "grok", Kind: "oauth", Access: "old-access"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutReports([]provider.Report{{ID: "grok", Windows: []provider.Window{{UsedPercent: 42}}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Replace(Connection{Provider: "grok", Kind: "oauth", Access: "new-access"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Reports()["grok"]; ok {
		t.Fatal("old report survived a newly authorized connection")
	}
}

func TestRekeyPreservesConnectionAndReport(t *testing.T) {
	store, err := Open(t.TempDir(), testKey(10))
	if err != nil {
		t.Fatal(err)
	}
	legacy := Connection{Provider: "copilot", Kind: "oauth", Access: "github-token"}
	if err := store.Put(legacy); err != nil {
		t.Fatal(err)
	}
	if err := store.PutReports([]provider.Report{{ID: "copilot", Name: "Copilot", Status: provider.StatusOK}}); err != nil {
		t.Fatal(err)
	}
	legacy.ID = "copilot:42"
	legacy.AccountID = "42"
	legacy.Email = "octocat"
	if err := store.Rekey("copilot", legacy); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Get("copilot"); ok {
		t.Fatal("legacy connection still exists")
	}
	if got, ok := store.Get("copilot:42"); !ok || got.Access != "github-token" || got.Provider != "copilot" {
		t.Fatalf("migrated connection = %#v, %v", got, ok)
	}
	if report, ok := store.Reports()["copilot:42"]; !ok || report.ID != "copilot:42" {
		t.Fatalf("migrated report = %#v, %v", report, ok)
	}
}
