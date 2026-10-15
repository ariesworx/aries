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
	gate := strings.TrimSpace(alice.ok("gate", "add", "--kind", "ci", "--check", "build", "--subject", impl,
		"--by", "ci-example", "--blocks", next))
	landed := strings.TrimSpace(alice.ok("gate", "add", "--kind", "ci", "--check", "landed", "--subject", impl, "--by", "ci-example"))

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
		t.Errorf("sfx gate pass by ci-other = exit %d\n%s%s\nwant a refusal naming ci-example", r.code, r.stdout, r.stderr)
	}
	// The agent itself, running sfx in its shell, is no CI job.
	bob.env["CLAUDE_CODE_SESSION_ID"] = "claude-bob"
	if r := bob.run("v0.2.0", "gate", "pass", "--check", "build", "--issue", impl); r.code != 1 || !strings.Contains(r.stderr, "ci-example") {
		t.Errorf("sfx gate pass from bob's agent = exit %d\n%s%s\nwant it refused", r.code, r.stdout, r.stderr)
	}
	delete(bob.env, "CLAUDE_CODE_SESSION_ID")

	const runURL = "https://ci.example.com/example/parser/actions/runs/1001"
	if out := ci.ok("gate", "fail", "--check", "build", "--from-branch", "--sha", "abc1234def", "--url", runURL,
		"--reason", "2 tests failed"); !strings.Contains(out, gate+" failed") {
		t.Errorf("sfx gate fail = %q, want %s failed", out, gate)
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
		t.Errorf("sfx gate pass = %q, want %s passed", out, gate)
	}
	if got := agent.readyIDs(); !slices.Contains(got, next) {
		t.Errorf("ready after the pass = %v, want %s", got, next)
	}

	// A branch that names no issue has nothing to report on: with
	// --optional, one workflow serves every branch.
	ci.env["GITHUB_HEAD_REF"] = "dependabot/go_modules/example.com/lib-1.2.3"
	if r := ci.run("v0.2.0", "gate", "pass", "--check", "build", "--from-branch", "--optional"); r.code != 0 ||
		!strings.Contains(r.stderr, "nothing reported") {
		t.Errorf("sfx gate pass --optional on another branch = exit %d\n%s%s\nwant exit 0, nothing reported", r.code, r.stdout, r.stderr)
	}
	if r := ci.run("v0.2.0", "gate", "pass", "--check", "build", "--from-branch"); r.code != 1 || !strings.Contains(r.stderr, "names no issue") {
		t.Errorf("sfx gate pass on another branch = exit %d\n%s%s\nwant it refused", r.code, r.stdout, r.stderr)
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
