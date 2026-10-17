package e2e

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/cli"
)

// gitOut runs git in dir and returns its trimmed output.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output() //nolint:gosec // test fixture
	if err != nil {
		t.Fatalf("git %s: %v", args[0], err)
	}
	return strings.TrimSpace(string(out))
}

// commitIn adds an empty commit with the message lines given.
func commitIn(t *testing.T, dir string, msg ...string) string {
	t.Helper()
	args := []string{"-c", "user.name=Maint", "-c", "user.email=maint@example.com", "commit", "-q", "--allow-empty"}
	for _, m := range msg {
		args = append(args, "-m", m)
	}
	gitIn(t, dir, args...)
	return gitOut(t, dir, "rev-parse", "HEAD")
}

// Design §21.8: the person who keeps the server on a laptop runs `aries
// land` after a pull, and it reports `landed` for each commit of the
// default branch since the last it reported, from the gates added by hand
// (W1 makes a report create its gate; this must work before it).
func TestLand(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	w := newWorld(t, daemonOpts{admins: []string{"dana"}})
	alice, maint := w.newUser("alice", ""), w.newUser("maint", "")
	// Run from a coordinating agent's shell: the report is the person's
	// act, from the session cli, whatever the environment says.
	maint.env["CLAUDE_CODE_SESSION_ID"] = "claude-maint"
	create := func(title string, gate bool) string {
		id := strings.TrimSpace(alice.ok("create", title))
		if gate {
			alice.ok("gate", "add", "--kind", "ci", "--check", "landed", "--issue", id, "--approver", "maint")
		}
		return id
	}
	a, b, c, d := create("First change", true), create("Second change", true), create("Third change", false), create("Fourth change", true)
	landed := func(id string) bool {
		return strings.Contains(alice.ok("show", id), "check landed: passed at ")
	}

	repo := maint.repo
	gitIn(t, repo, "init", "-q", "-b", "main")
	base := commitIn(t, repo, "init")
	first := commitIn(t, repo, "first change", "Aries: "+a)

	// With nothing reported yet, only HEAD is read: a first run in a
	// long history does not report all of it.
	r := maint.run("v0.2.0", "land")
	if r.code != cli.ExitOK || !strings.Contains(r.stdout, first[:12]+" "+a) || !strings.Contains(r.stderr, "--from") {
		t.Fatalf("aries land, first = exit %d\n%s%s\nwant %s reported, and a note on --from", r.code, r.stdout, r.stderr, a)
	}
	if !landed(a) {
		t.Errorf("show %s lacks the landed report:\n%s", a, alice.ok("show", a))
	}
	before := alice.ok("history", a)
	r = maint.run("v0.2.0", "land")
	if r.code != cli.ExitOK || !strings.Contains(r.stdout, "nothing new since "+first[:12]) || alice.ok("history", a) != before {
		t.Errorf("aries land, again = exit %d\n%s%s\nwant nothing new, and the history unchanged", r.code, r.stdout, r.stderr)
	}

	// A dry run says what it would report, and remembers nothing.
	noIssue := commitIn(t, repo, "chore: bump a thing")
	merge := commitIn(t, repo, "Merge pull request #7 from example/feature/"+b+"-second-change")
	noGate := commitIn(t, repo, "third change", "Aries: "+c)
	if r := maint.run("v0.2.0", "land", "--dry-run"); r.code != cli.ExitOK || !strings.Contains(r.stdout, merge[:12]+" example/feature/"+b) || landed(b) {
		t.Errorf("aries land --dry-run = exit %d\n%s%s\nwant the merged branch of %s named and not reported", r.code, r.stdout, r.stderr, b)
	}

	// Off the default branch it reports nothing: the work there is not
	// merged.
	gitIn(t, repo, "switch", "-q", "-c", "feature/"+d)
	commitIn(t, repo, "fourth change", "Aries: "+d)
	if r := maint.run("v0.2.0", "land", "--from", base); r.code != cli.ExitFailure || !strings.Contains(r.stderr, "default branch") {
		t.Errorf("aries land on a feature branch = exit %d\n%s%s\nwant it refused", r.code, r.stdout, r.stderr)
	}
	gitIn(t, repo, "switch", "-q", "main")

	r = maint.run("v0.2.0", "land")
	if r.code != cli.ExitOK || !strings.Contains(r.stdout, merge[:12]+" "+b) ||
		!strings.Contains(r.stderr, "skipped "+noIssue[:12]+": it names no issue") ||
		!strings.Contains(r.stderr, "skipped "+noGate[:12]) || !landed(b) {
		t.Fatalf("aries land = exit %d\n%s%s\nwant %s reported, and the other two skipped with a note", r.code, r.stdout, r.stderr, b)
	}
	if r := maint.run("v0.2.0", "land", "--json"); r.code != cli.ExitOK || !strings.Contains(r.stdout, `"reported":[]`) {
		t.Errorf("aries land --json, again = exit %d\n%s%s\nwant nothing reported", r.code, r.stdout, r.stderr)
	}

	// Offline it reports nothing and remembers nothing, queues nothing,
	// and the next run reports it all.
	fourth := commitIn(t, repo, "fourth change", "Aries: "+d)
	w.stop()
	if r := maint.run("v0.2.0", "land"); r.code != cli.ExitFailure || !strings.Contains(r.stderr, "aries: ") {
		t.Errorf("aries land with the server away = exit %d\n%s%s\nwant it to fail", r.code, r.stdout, r.stderr)
	}
	w.start()
	if out := maint.ok("outbox"); strings.Contains(out, "gate") {
		t.Errorf("aries outbox after a land offline = %q, want nothing queued", out)
	}
	if r := maint.run("v0.2.0", "land"); r.code != cli.ExitOK || !strings.Contains(r.stdout, fourth[:12]+" "+d) || !landed(d) {
		t.Errorf("aries land with the server back = exit %d\n%s%s\nwant %s reported", r.code, r.stdout, r.stderr, d)
	}

	// As git's post-merge hook it says nothing off the default branch,
	// and exits 0 whatever happens on it.
	gitIn(t, repo, "switch", "-q", "feature/"+d)
	if r := maint.run("v0.2.0", "land", "--hook"); r.code != cli.ExitOK || r.stdout != "" || r.stderr != "" {
		t.Errorf("aries land --hook off the default branch = exit %d\n%s%s\nwant nothing", r.code, r.stdout, r.stderr)
	}
	gitIn(t, repo, "switch", "-q", "main")
	commitIn(t, repo, "a change", "Aries: "+d)
	w.stop()
	if r := maint.run("v0.2.0", "land", "--hook"); r.code != cli.ExitOK || !strings.HasPrefix(r.stderr, "aries land: ") {
		t.Errorf("aries land --hook with the server away = exit %d\n%s%s\nwant exit 0 and one line", r.code, r.stdout, r.stderr)
	}
	w.start()
}

// A principal the gate does not name is refused, and the refusal leaves
// the commit to be reported again by whoever may.
func TestLandRefusedKeepsItsPlace(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	w := newWorld(t, daemonOpts{admins: []string{"dana"}})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	id := strings.TrimSpace(alice.ok("create", "A change"))
	alice.ok("gate", "add", "--kind", "ci", "--check", "landed", "--issue", id, "--approver", "maint")
	gitIn(t, bob.repo, "init", "-q", "-b", "main")
	commitIn(t, bob.repo, "a change", "Aries: "+id)
	for range 2 {
		if r := bob.run("v0.2.0", "land"); r.code != cli.ExitFailure || !strings.Contains(r.stderr, "only maint may pass") {
			t.Fatalf("aries land by bob = exit %d\n%s%s\nwant it refused each time", r.code, r.stdout, r.stderr)
		}
	}
}
