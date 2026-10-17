package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q", "-b", "main").CombinedOutput(); err != nil { //nolint:gosec // test fixture
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return dir
}

// `aries setup post-merge` offers git's post-merge hook, which runs
// `aries land` after a pull: it prints by default and writes only with
// --write, once, and takes out only its own.
func TestSetupPostMerge(t *testing.T) {
	t.Parallel()
	repo := gitRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "post-merge")

	code, out, errb := runIn(t, t.TempDir(), "-C", repo, "setup", "post-merge")
	if code != ExitOK || !strings.Contains(out, "aries land --hook") || !strings.Contains(out, "post-merge") {
		t.Fatalf("setup post-merge = exit %d\n%s%s\nwant the hook printed", code, out, errb)
	}
	if _, err := os.Stat(hook); err == nil {
		t.Fatal("setup post-merge wrote the hook unasked")
	}
	if code, _, _ := runIn(t, t.TempDir(), "-C", repo, "setup", "post-merge", "--check"); code != ExitFailure {
		t.Errorf("--check with no hook = exit %d, want 1", code)
	}

	if code, out, errb = runIn(t, t.TempDir(), "-C", repo, "setup", "post-merge", "--write"); code != ExitOK {
		t.Fatalf("--write = exit %d\n%s%s", code, out, errb)
	}
	b, err := os.ReadFile(hook) //nolint:gosec // test fixture
	if err != nil || !strings.HasPrefix(string(b), "#!/bin/sh\n") || !strings.Contains(string(b), "aries land --hook\n") {
		t.Fatalf("hook = %q, %v; want a shell script that runs aries land --hook", b, err)
	}
	if st, _ := os.Stat(hook); runtime.GOOS != "windows" && st.Mode().Perm()&0o100 == 0 {
		t.Errorf("hook mode = %v, want it executable", st.Mode())
	}
	if code, _, _ := runIn(t, t.TempDir(), "-C", repo, "setup", "post-merge", "--check"); code != ExitOK {
		t.Errorf("--check with the hook = exit %d, want 0", code)
	}
	if code, _, _ := runIn(t, t.TempDir(), "-C", repo, "setup", "post-merge", "--write"); code != ExitOK {
		t.Errorf("--write again = exit %d, want 0", code)
	}
	if again, _ := os.ReadFile(hook); string(again) != string(b) { //nolint:gosec // test fixture
		t.Errorf("hook after a second --write = %q, want it unchanged", again)
	}

	if code, _, _ := runIn(t, t.TempDir(), "-C", repo, "setup", "post-merge", "--remove"); code != ExitOK {
		t.Fatalf("--remove = exit %d", code)
	}
	if _, err := os.Stat(hook); err == nil {
		t.Error("--remove left the hook")
	}
}

// A hook someone else wrote is neither replaced nor removed.
func TestSetupPostMergeLeavesOthersHooks(t *testing.T) {
	t.Parallel()
	repo := gitRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "post-merge")
	if err := os.MkdirAll(filepath.Dir(hook), 0o750); err != nil {
		t.Fatal(err)
	}
	mine := "#!/bin/sh\nmake generate\n"
	if err := os.WriteFile(hook, []byte(mine), 0o700); err != nil { //nolint:gosec // an executable hook
		t.Fatal(err)
	}
	code, _, errb := runIn(t, t.TempDir(), "-C", repo, "setup", "post-merge", "--write")
	if code != ExitFailure || !strings.Contains(errb, "fix: ") || !strings.Contains(errb, "aries land --hook") {
		t.Errorf("--write over another's hook = exit %d, %q; want a refusal that says what to add", code, errb)
	}
	if code, _, _ := runIn(t, t.TempDir(), "-C", repo, "setup", "post-merge", "--remove"); code != ExitOK {
		t.Errorf("--remove of a hook that does not run aries = exit %d, want 0", code)
	}
	if got, _ := os.ReadFile(hook); string(got) != mine { //nolint:gosec // test fixture
		t.Errorf("setup changed another's hook to %q", got)
	}
	// Its own line, added by hand, is found.
	if err := os.WriteFile(hook, []byte(mine+"aries land --hook\n"), 0o700); err != nil { //nolint:gosec // an executable hook
		t.Fatal(err)
	}
	if code, _, _ := runIn(t, t.TempDir(), "-C", repo, "setup", "post-merge", "--check"); code != ExitOK {
		t.Errorf("--check with the line added by hand = exit %d, want 0", code)
	}
	if code, _, errb := runIn(t, t.TempDir(), "-C", repo, "setup", "post-merge", "--remove"); code != ExitFailure || !strings.Contains(errb, "fix: ") {
		t.Errorf("--remove of a hook with more in it = exit %d, %q; want a refusal", code, errb)
	}
}

func TestSetupPostMergeUsage(t *testing.T) {
	t.Parallel()
	repo := gitRepo(t)
	for _, args := range [][]string{{"post-merge", "--global"}, {"post-merge", "--all"}, {"post-merge", "--write", "--check"}} {
		if code, _, _ := runIn(t, t.TempDir(), append([]string{"-C", repo, "setup"}, args...)...); code != ExitUsage {
			t.Errorf("setup %v = exit %d, want usage", args, code)
		}
	}
}
