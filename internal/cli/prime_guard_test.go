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

// `aries prime --hook` takes a Desktop session's snapshot of the policy
// from the default branch as origin has it, never from the worktree, by
// the hook's session id, and says at the session's start when the guard
// cannot apply it (docs/design/guard.md §6, §8). The guard then reads it.
func TestPrimeHookTakesGuardSnapshot(t *testing.T) {
	t.Parallel()
	pol, err := os.ReadFile(filepath.Join("..", "..", guard.File))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		// policy is origin/main's file; nil for none.
		policy  []byte
		present bool
		note    string
	}{
		{name: "policy", policy: pol, present: true},
		{name: "none", note: ""},
		{name: "invalid", policy: []byte("version: 1\nalow: []\n"), present: true,
			note: `aries guard: policy not applied: .aries/guard.yaml: line 2: unknown key "alow"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo, data := gitRepo(t), t.TempDir()
			if err := os.WriteFile(filepath.Join(repo, ".aries.yaml"), []byte(testConfig), 0o600); err != nil {
				t.Fatal(err)
			}
			gitTest(t, repo, "add", ".aries.yaml")
			if tc.policy != nil {
				writeFile(t, filepath.Join(repo, guard.File), string(tc.policy))
				gitTest(t, repo, "add", guard.File)
			}
			gitTest(t, repo, "commit", "-q", "-m", "first")
			gitTest(t, repo, "update-ref", "refs/remotes/origin/main", "HEAD")
			// The worktree's copy is the agent's to edit: never read.
			writeFile(t, filepath.Join(repo, guard.File), "version: 1\n")

			in, _ := json.Marshal(map[string]string{"session_id": "cc-1", "cwd": repo, "hook_event_name": "SessionStart"})
			var out, errb bytes.Buffer
			getenv := func(k string) string { return map[string]string{"ARIES_DATA_DIR": data}[k] }
			if code := Run(t.Context(), []string{"prime", "--hook"}, Env{Stdin: bytes.NewReader(in), Stdout: &out, Stderr: &errb,
				Getenv: getenv, Version: "v0.3.0"}); code != ExitOK || errb.Len() > 0 {
				t.Fatalf("exit %d, stderr %q", code, errb.String())
			}
			path, _ := guard.SnapshotPath(data, "cc-1")
			b, err := os.ReadFile(path) //nolint:gosec // the test's own data directory
			if err != nil {
				t.Fatalf("no snapshot: %v", err)
			}
			if want := guard.Snapshot(guard.Desktop, tc.policy, tc.present); !bytes.Equal(b, want) {
				t.Errorf("snapshot =\n%s\nwant\n%s", b, want)
			}
			if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
				t.Errorf("snapshot mode %v, %v", fi.Mode().Perm(), err)
			}
			ctx := hookContext(t, out.String())
			if tc.note == "" && strings.Contains(ctx, "aries guard") || tc.note != "" && !strings.Contains(ctx, tc.note) {
				t.Errorf("context %q; want note %q", ctx, tc.note)
			}

			// The guard reads the snapshot: merges are denied.
			in, _ = json.Marshal(map[string]any{"session_id": "cc-1", "cwd": repo, "hook_event_name": "PreToolUse",
				"tool_name": "Bash", "tool_input": map[string]string{"command": "gh pr merge 1"}})
			out.Reset()
			Run(t.Context(), []string{"guard", "--hook"}, Env{Stdin: bytes.NewReader(in), Stdout: &out, Stderr: &errb, Getenv: getenv})
			if !strings.Contains(out.String(), `"permissionDecision":"deny"`) {
				t.Errorf("guard after prime: %q", out.String())
			}
		})
	}
}

// hookContext is the additionalContext of a SessionStart hook's output.
func hookContext(t *testing.T, out string) string {
	t.Helper()
	var doc struct {
		Out struct {
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not hook JSON: %q %v", out, err)
	}
	return doc.Out.Context
}
