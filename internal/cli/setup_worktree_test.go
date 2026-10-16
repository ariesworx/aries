package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// In a linked worktree that reads the main worktree's .aries.yaml,
// aries setup writes the project's files in the worktree it ran in,
// never in the main worktree.
func TestSetupInLinkedWorktree(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	top := t.TempDir()
	main, wt := filepath.Join(top, "main"), filepath.Join(top, "wt")
	for _, c := range []struct {
		dir  string
		args []string
	}{
		{top, []string{"init", "-q", "-b", "main", main}},
		{main, []string{"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "--allow-empty", "-m", "init"}},
		{main, []string{"worktree", "add", "-q", "-b", "wt", wt}},
	} {
		cmd := exec.CommandContext(t.Context(), "git", c.args...) //nolint:gosec // test fixture
		cmd.Dir = c.dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", c.args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(main, ".aries.yaml"), []byte(testConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, errb := runIn(t, t.TempDir(), "-C", wt, "setup", "claude-code", "--write"); code != ExitOK {
		t.Fatalf("aries setup claude-code --write: exit %d\n%s%s", code, out, errb)
	}
	if _, err := os.Stat(filepath.Join(wt, ".mcp.json")); err != nil {
		t.Errorf("the worktree has no .mcp.json: %v", err)
	}
	if _, err := os.Stat(filepath.Join(main, ".mcp.json")); err == nil {
		t.Error("setup wrote .mcp.json in the main worktree")
	}
}
