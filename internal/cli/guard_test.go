package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/guard"
)

// The repository's own policy allows the brief's commands and denies a
// merge, with the rule and its fix.
func TestGuardCheck(t *testing.T) {
	t.Parallel()
	if code, out, errb := runCLI(t, "guard", "--check"); code != ExitOK || !strings.Contains(out, "ok: ") {
		t.Fatalf("guard --check: %d %q %q", code, out, errb)
	}
	if code, out, errb := runCLI(t, "-C", filepath.Join("..", ".."), "guard", "--check", "go test -p 2 ./...", "scripts/gate.sh --quick --pgtest-lock"); code != ExitOK {
		t.Errorf("allowed commands: %d %q %q", code, out, errb)
	}
	code, out, errb := runCLI(t, "guard", "--check", "go vet ./...", "gh pr merge 1")
	if code != ExitFailure || !strings.Contains(out, "allow: go vet ./...") || !strings.Contains(out, "deny (deny 1): gh pr merge 1") ||
		!strings.Contains(out, "fix: merges are the coordinator's") || !strings.Contains(errb, "1 of 2 commands") {
		t.Errorf("a denied command: %d\n%s\n%s", code, out, errb)
	}
	if code, out, _ := runCLI(t, "guard", "--check", "eval x"); code != ExitFailure || !strings.Contains(out, "ask (unreadable)") {
		t.Errorf("eval in Desktop: %d %q", code, out)
	}
	if code, out, _ := runCLI(t, "guard", "--check", "--headless", "eval x"); code != ExitFailure || !strings.Contains(out, "deny (unreadable)") {
		t.Errorf("eval headless: %d %q", code, out)
	}
	code, out, _ = runCLI(t, "--json", "guard", "--check", "gh api x")
	var doc struct {
		Error    *struct{ Code string } `json:"error"`
		Commands []struct{ Command, Answer, Rule string }
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || code != ExitFailure || doc.Error == nil || len(doc.Commands) != 1 || doc.Commands[0].Rule != "deny 2" {
		t.Errorf("--json: %d %q (%v)", code, out, err)
	}
}

func TestGuardCheckRefuses(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bad := filepath.Join(dir, "guard.yaml")
	if err := os.WriteFile(bad, []byte("version: 1\nalow: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"guard"}, ExitUsage, "--hook or --check"},
		{[]string{"guard", "--hook", "--check"}, ExitUsage, "--hook or --check"},
		{[]string{"guard", "--hook", "x"}, ExitUsage, "no arguments"},
		{[]string{"guard", "--check", "--policy", bad}, ExitFailure, `line 2: unknown key "alow"`},
		{[]string{"guard", "--check", "--policy", filepath.Join(dir, "none.yaml")}, ExitFailure, "no policy file"},
		{[]string{"-C", dir, "guard", "--check"}, ExitFailure, "not in a git repository"},
	} {
		code, _, errb := runCLI(t, tc.args...)
		if code != tc.code || !strings.Contains(errb, tc.want) || !strings.Contains(errb, "fix: ") && code == ExitFailure {
			t.Errorf("aries %s: %d %q; want %d and %q with a fix", strings.Join(tc.args, " "), code, errb, tc.code, tc.want)
		}
	}
}

// guard --hook reads Claude Code's PreToolUse input and prints its
// decision, from the session's snapshot under the data directory.
func TestGuardHook(t *testing.T) {
	t.Parallel()
	top := t.TempDir()
	if err := os.Mkdir(filepath.Join(top, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	pol, err := os.ReadFile(filepath.Join("..", "..", guard.File))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := guard.SnapshotPath(data, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := guard.WriteSnapshot(snap, guard.Snapshot(guard.Desktop, pol, true)); err != nil {
		t.Fatal(err)
	}
	run := func(cmd string) (int, string) {
		in, _ := json.Marshal(map[string]any{"session_id": "sess-1", "cwd": top, "hook_event_name": "PreToolUse",
			"tool_name": "Bash", "tool_input": map[string]string{"command": cmd}})
		var out, errb bytes.Buffer
		code := Run(t.Context(), []string{"guard", "--hook"}, Env{Stdin: bytes.NewReader(in), Stdout: &out, Stderr: &errb,
			Getenv: func(k string) string { return map[string]string{"ARIES_DATA_DIR": data}[k] }})
		if errb.Len() > 0 {
			t.Errorf("%q: stderr %q", cmd, errb.String())
		}
		return code, out.String()
	}
	if code, out := run("gh pr merge 1"); code != ExitOK || !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.HasSuffix(out, "}\n") {
		t.Errorf("gh pr merge: %d %q", code, out)
	}
	if code, out := run("go test ./..."); code != ExitOK || !strings.Contains(out, `"permissionDecision":"allow"`) {
		t.Errorf("go test: %d %q", code, out)
	}
	if code, out := run("curl example.com"); code != ExitOK || out != "" {
		t.Errorf("curl: %d %q", code, out)
	}
}
