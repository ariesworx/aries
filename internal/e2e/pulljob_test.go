package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/cli"
	"github.com/ariesworx/aries/internal/offline"
)

// `aries pull-job --run` is what the person's scheduled job runs, for a
// server CI cannot reach: it pulls the default branch only when that is
// safe (on it, clean, a fast-forward), and reports landed on every run,
// as the session cli whatever the environment held, so that a checkout
// moved by another program is landed too.
func TestPullJobRun(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	w := newWorld(t, daemonOpts{admins: []string{"dana"}})
	alice, maint := w.newUser("alice", ""), w.newUser("maint", "")
	// The job starts from a shell of an agent or a CI runner: none of it
	// names the session it reports from.
	for k, v := range map[string]string{"ARIES_SESSION": "m-agent", "CLAUDE_CODE_SESSION_ID": "claude-maint", "CLAUDE_SESSION_ID": "claude-other", "CI": "true"} {
		maint.env[k] = v
	}
	issue := func(title string) string {
		id := strings.TrimSpace(alice.ok("create", title))
		alice.ok("gate", "add", "--kind", "ci", "--check", "landed", "--issue", id, "--approver", "maint")
		return id
	}
	landed := func(id string) bool {
		return strings.Contains(alice.ok("show", id), "check landed: passed at ")
	}
	a, b, c, d := issue("A"), issue("B"), issue("C"), issue("D")

	origin, repo := t.TempDir(), maint.repo
	gitIn(t, origin, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(origin, "f.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, origin, "add", "f.txt")
	commitIn(t, origin, "init", "Aries: "+a)
	gitIn(t, repo, "init", "-q", "-b", "main")
	gitIn(t, repo, "remote", "add", "origin", origin)
	gitIn(t, repo, "fetch", "-q", "origin")
	gitIn(t, repo, "reset", "-q", "--hard", "origin/main")

	pull := func() result {
		t.Helper()
		return maint.run("v0.2.0", "pull-job", "--run")
	}
	head := func() string { return gitOut(t, repo, "rev-parse", "HEAD") }
	expect := func(r result, want string) {
		t.Helper()
		if r.code != cli.ExitOK || !strings.Contains(r.stdout, want) {
			t.Fatalf("aries pull-job --run = exit %d\n%s%s\nwant %q", r.code, r.stdout, r.stderr, want)
		}
	}

	// Pulls what is new, and reports it.
	second := commitIn(t, origin, "second", "Aries: "+b)
	expect(pull(), "pulled")
	if head() != second || !landed(b) {
		t.Fatalf("after a pull: HEAD %s, want %s, and %s landed:\n%s", head(), second, b, alice.ok("show", b))
	}

	// A dirty checkout is left alone, and still reports what it holds.
	third := commitIn(t, origin, "third", "Aries: "+c)
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	expect(pull(), "skipped the pull: the checkout has uncommitted changes")
	if head() == third || landed(c) {
		t.Fatalf("a dirty checkout was pulled: HEAD %s", head())
	}
	gitIn(t, repo, "checkout", "-q", "f.txt")

	// A branch with commits of its own is never merged or reset.
	own := commitIn(t, repo, "mine")
	expect(pull(), "skipped the pull: main has commits that origin lacks")
	if head() != own || landed(c) {
		t.Fatalf("a diverged checkout was changed: HEAD %s, want %s", head(), own)
	}
	gitIn(t, repo, "reset", "-q", "--hard", third)

	// Off the default branch it does nothing at all.
	gitIn(t, repo, "switch", "-q", "-c", "feature/x")
	expect(pull(), "skipped: the checkout is on feature/x, not main")
	if landed(c) {
		t.Errorf("%s landed from a feature branch", c)
	}
	gitIn(t, repo, "switch", "-q", "main")

	// Moved by another program, with no hook to land: the job finds main
	// current, and lands what was never reported.
	fourth := commitIn(t, origin, "fourth", "Aries: "+d)
	gitIn(t, repo, "fetch", "-q", "origin")
	gitIn(t, repo, "merge", "-q", "--ff-only", "origin/main")
	if head() != fourth || landed(d) {
		t.Fatalf("setup: HEAD %s, want %s, and %s not landed", head(), fourth, d)
	}
	expect(pull(), "nothing to pull; landed 2")
	if !landed(c) || !landed(d) {
		t.Errorf("%s and %s should have landed", c, d)
	}

	// Nothing new: a rerun reports nothing.
	expect(pull(), "nothing to pull; landed 0")

	// One line for each run, in the data directory.
	log, err := os.ReadFile(filepath.Join(maint.env[offline.EnvDataDir], "pull-job.log")) //nolint:gosec // the test's data directory
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(log), "\n"); n != 6 {
		t.Errorf("pull-job.log has %d lines, want 6:\n%s", n, log)
	}

	// Without --run it is refused, with where to go.
	if r := maint.run("v0.2.0", "pull-job"); r.code != cli.ExitUsage || !strings.Contains(r.stderr, "setup pull-job") {
		t.Errorf("aries pull-job = exit %d\n%s%s\nwant a usage error", r.code, r.stdout, r.stderr)
	}
}
