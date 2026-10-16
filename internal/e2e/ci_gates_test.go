package e2e

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// Stage 6, slice G1's gate (design §20.10): a CI principal passes the
// build gate of the issue its branch names, and the dependent becomes
// ready; a principal the gate does not name is refused; a failure tells
// the subject's holder, with the evidence. Then the job on the default
// branch after the merge reports landed, from the merge commit, and the
// closed issue shows as landed.
func TestCIGate(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{admins: []string{"dana"}})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	ci, intruder := w.newUser("ci-example", ""), w.newUser("ci-other", "")
	impl := strings.TrimSpace(alice.ok("create", "Implement the parser"))
	next := strings.TrimSpace(alice.ok("create", "Use the parser"))
	gate := strings.TrimSpace(alice.ok("gate", "add", "--kind", "ci", "--check", "build", "--issue", impl,
		"--approver", "ci-example", "--blocks", next))
	landed := strings.TrimSpace(alice.ok("gate", "add", "--kind", "ci", "--check", "landed", "--issue", impl, "--approver", "ci-example"))

	// bob's agent holds the work the check runs on.
	agent := bob.mcp("v0.2.0")
	agent.ok("start", map[string]any{"id": impl})
	if got := agent.readyIDs(); slices.Contains(got, next) {
		t.Fatalf("ready behind the ci gate = %v, want %s held", got, next)
	}

	// A GitHub Actions job on the pull request's branch: CI=true, and the
	// branch in GITHUB_HEAD_REF, since the checkout is detached.
	branch := "feature/" + impl + "-implement-the-parser"
	for _, u := range []*user{ci, intruder} {
		u.env["CI"], u.env["GITHUB_HEAD_REF"] = "true", branch
	}
	if r := intruder.run("v0.2.0", "gate", "pass", "--check", "build", "--from-branch"); r.code != 1 ||
		!strings.Contains(r.stderr, "only ci-example may pass gate "+gate) || !strings.Contains(r.stderr, "fix: ") {
		t.Errorf("aries gate pass by ci-other = exit %d\n%s%s\nwant a refusal naming ci-example", r.code, r.stdout, r.stderr)
	}
	// The agent itself, running aries in its shell, is no CI job.
	bob.env["CLAUDE_CODE_SESSION_ID"] = "claude-bob"
	if r := bob.run("v0.2.0", "gate", "pass", "--check", "build", "--issue", impl); r.code != 1 || !strings.Contains(r.stderr, "ci-example") {
		t.Errorf("aries gate pass from bob's agent = exit %d\n%s%s\nwant it refused", r.code, r.stdout, r.stderr)
	}
	delete(bob.env, "CLAUDE_CODE_SESSION_ID")

	const runURL = "https://ci.example.com/example/parser/actions/runs/1001"
	if out := ci.ok("gate", "fail", "--check", "build", "--from-branch", "--sha", "abc1234def", "--url", runURL,
		"--reason", "2 tests failed"); !strings.Contains(out, gate+" failed") {
		t.Errorf("aries gate fail = %q, want %s failed", out, gate)
	}
	if out := agent.ok("inbox", nil); !strings.Contains(out, `"kind":"gate.failed"`) || !strings.Contains(out, runURL) ||
		!strings.Contains(out, "2 tests failed") {
		t.Errorf("the holder's inbox = %s, want a gate.failed item with the run and the reason", out)
	}
	if got := agent.readyIDs(); slices.Contains(got, next) {
		t.Errorf("ready after the failure = %v, want %s held", got, next)
	}
	if out := alice.ok("show", impl); !strings.Contains(out, "check build: failed at abc1234def: "+runURL+" 2 tests failed") {
		t.Errorf("show after the failure lacks the check:\n%s", out)
	}

	if out := ci.ok("gate", "pass", "--check", "build", "--from-branch", "--sha", "fed4321", "--url", runURL); !strings.Contains(out, gate+" passed") {
		t.Errorf("aries gate pass = %q, want %s passed", out, gate)
	}
	if got := agent.readyIDs(); !slices.Contains(got, next) {
		t.Errorf("ready after the pass = %v, want %s", got, next)
	}

	// A branch that names no issue has nothing to report on: with
	// --optional, one workflow serves every branch.
	ci.env["GITHUB_HEAD_REF"] = "dependabot/go_modules/example.com/lib-1.2.3"
	if r := ci.run("v0.2.0", "gate", "pass", "--check", "build", "--from-branch", "--optional"); r.code != 0 ||
		!strings.Contains(r.stderr, "nothing reported") {
		t.Errorf("aries gate pass --optional on another branch = exit %d\n%s%s\nwant exit 0, nothing reported", r.code, r.stdout, r.stderr)
	}
	if r := ci.run("v0.2.0", "gate", "pass", "--check", "build", "--from-branch"); r.code != 1 || !strings.Contains(r.stderr, "names no issue") {
		t.Errorf("aries gate pass on another branch = exit %d\n%s%s\nwant it refused", r.code, r.stdout, r.stderr)
	}

	// Closed is not landed until the default branch's job says so.
	agent.ok("finish", map[string]any{"id": impl})
	if out := alice.ok("show", impl); !strings.Contains(out, "closed, not landed") {
		t.Errorf("show of the finished issue lacks closed, not landed:\n%s", out)
	}
	delete(ci.env, "GITHUB_HEAD_REF")
	if _, err := exec.LookPath("git"); err == nil {
		// The merge commit on main names the branch it merged.
		gitIn(t, ci.repo, "init", "-q", "-b", "main")
		gitIn(t, ci.repo, "-c", "user.name=CI", "-c", "user.email=ci@example.com", "commit", "-q", "--allow-empty",
			"-m", "Merge pull request #12 from example/"+branch)
		ci.ok("gate", "pass", "--check", "landed", "--from-commit", "--sha", "0a1b2c3")
	} else {
		ci.ok("gate", "pass", "--check", "landed", "--issue", impl, "--sha", "0a1b2c3")
	}
	if out := alice.ok("show", impl); !strings.Contains(out, "check landed: passed at 0a1b2c3") || !strings.Contains(out, "closed, landed ") {
		t.Errorf("show after landing lacks it:\n%s", out)
	}
	if out := alice.ok("gate", "list", "--all"); !strings.Contains(out, landed) {
		t.Errorf("gate list --all lacks %s:\n%s", landed, out)
	}
}

// A slow run of an older commit that fails after a newer run passed
// does not hold the work back again: aries sends the run's number from
// the CI system's variables, and the server takes no report older than
// the gate's last.
func TestCIGateLateFailure(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{admins: []string{"dana"}})
	alice, ci := w.newUser("alice", ""), w.newUser("ci-example", "")
	impl := strings.TrimSpace(alice.ok("create", "Implement the lexer"))
	next := strings.TrimSpace(alice.ok("create", "Use the lexer"))
	gate := strings.TrimSpace(alice.ok("gate", "add", "--kind", "ci", "--check", "build", "--issue", impl,
		"--approver", "ci-example", "--blocks", next))
	ci.env["CI"], ci.env["GITHUB_HEAD_REF"] = "true", "feature/"+impl+"-implement-the-lexer"

	ci.env["GITHUB_RUN_NUMBER"] = "12"
	if out := ci.ok("gate", "pass", "--check", "build", "--from-branch", "--sha", "bbbbbbb"); !strings.Contains(out, gate+" passed") {
		t.Fatalf("aries gate pass in run 12 = %q, want %s passed", out, gate)
	}
	ci.env["GITHUB_RUN_NUMBER"] = "11"
	r := ci.run("v0.2.0", "gate", "fail", "--check", "build", "--from-branch", "--sha", "aaaaaaa")
	if r.code != 0 || !strings.Contains(r.stdout, gate+" passed") || !strings.Contains(r.stderr, "run 11") {
		t.Errorf("aries gate fail in run 11, after run 12 passed = exit %d\n%s%s\nwant exit 0, the gate still passed, and a note that run 11 is older",
			r.code, r.stdout, r.stderr)
	}
	if out := alice.ok("ready"); !strings.Contains(out, next) {
		t.Errorf("ready after a late failure of an older run = %s, want %s", out, next)
	}
	// The run given on the command line wins over the variable.
	if out := ci.ok("gate", "fail", "--check", "build", "--from-branch", "--sha", "ccccccc", "--run", "13"); !strings.Contains(out, gate+" failed") {
		t.Errorf("aries gate fail --run 13 = %q, want %s failed", out, gate)
	}
}
