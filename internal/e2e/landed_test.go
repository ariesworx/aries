package e2e

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/cli"
	"github.com/ariesworx/aries/internal/mcpserver"
	"github.com/ariesworx/aries/internal/proto"
)

// landedWorkflow requires landed of every type, reported by ci-example,
// with the last item bound to it.
const landedWorkflow = `version: 1
ci: [ci-example]
types:
  default:
    acceptance:
      - PR opened from the template
      - text: Reviewed and merged by the maintainer
        check: landed
    checks: [landed]
`

// Slice W1's gate (design §21.11): an issue whose profile requires
// landed is refused at finish, with a fix to hand off; it waits as
// awaiting landing in prime and digest; a principal the workflow does not
// name is refused; the CI principal's --from-commit pass, with no gate
// added, closes it, landed, and tells its assignee; and a failure after
// reopens it.
func TestLanded(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{admins: []string{"dana"}})
	dana, alice := w.newUser("dana", ""), w.newUser("alice", "")
	ci, intruder := w.newUser("ci-example", ""), w.newUser("ci-other", "")
	if out := dana.ok("admin", "workflow", "set", dana.writeWorkflow(t, landedWorkflow)); !strings.Contains(out, "workflow rev 1 set") {
		t.Fatalf("workflow set = %q", out)
	}

	agent := alice.mcp("v0.2.0")
	made := decode[proto.WriteResult](t, agent.ok("create", map[string]any{"title": "Implement the parser"}))
	id := made.ID
	agent.ok("start", map[string]any{"id": id})

	// finish is refused, with every item ticked, and told to hand off.
	out, isErr := agent.call("finish", map[string]any{"id": id, "ticked": []any{1, 2}})
	if !isErr || !strings.HasPrefix(out, "acceptance") || !strings.Contains(out, "check landed") ||
		!strings.Contains(out, "call handoff with state: done") {
		t.Fatalf("finish of an issue requiring landed = %q (error %v); want acceptance, naming landed, and handoff next", out, isErr)
	}
	r := alice.run("v0.2.0", "--json", "close", id)
	if e := cliError(t, r.stdout); r.code != 1 || e.Code != proto.CodeAcceptance || !strings.HasPrefix(e.Fix, proto.FixLand) {
		t.Errorf("aries close = exit %d, %+v; want acceptance, with a fix to hand off", r.code, e)
	}
	alice.ok("accept", id, "1")
	agent.ok("handoff", map[string]any{"id": id, "note": "https://example.com/example/parser/pull/12", "state": "done"})

	// Awaiting landing, in prime and digest.
	p := decode[mcpserver.Prime](t, agent.ok("prime", nil))
	if len(p.Landing) != 1 || p.Landing[0].ID != id || p.Landing[0].At != "landed (no report yet)" {
		t.Errorf("prime's landing = %+v; want %s, waiting for landed", p.Landing, id)
	}
	if out := alice.ok("digest"); !strings.Contains(out, "awaiting landing 1") || !strings.Contains(out, "landed (no report yet)") {
		t.Errorf("digest lacks the issue awaiting landing:\n%s", out)
	}

	// The job on the default branch after the merge.
	branch := "feature/" + id + "-implement-the-parser"
	for _, u := range []*user{ci, intruder} {
		u.env["CI"] = "true"
	}
	from := []string{"gate", "pass", "--check", "landed", "--issue", id, "--sha", "0a1b2c3"}
	if _, err := exec.LookPath("git"); err == nil {
		for _, u := range []*user{ci, intruder} {
			gitIn(t, u.repo, "init", "-q", "-b", "main")
			gitIn(t, u.repo, "-c", "user.name=CI", "-c", "user.email=ci@example.com", "commit", "-q", "--allow-empty",
				"-m", "Merge pull request #12 from example/"+branch)
		}
		from = []string{"gate", "pass", "--check", "landed", "--from-commit", "--sha", "0a1b2c3"}
	}
	if r := intruder.run("v0.2.0", from...); r.code != 1 || !strings.Contains(r.stderr, "ci-example") {
		t.Errorf("aries gate pass by ci-other = exit %d\n%s%s\nwant a refusal naming ci-example", r.code, r.stdout, r.stderr)
	}
	if out := alice.ok("gate", "list", "--all"); strings.Contains(out, id) {
		t.Errorf("a refused report left a gate:\n%s", out)
	}
	ci.ok(from...)
	shown := decode[proto.ShowResult](t, alice.ok("show", id, "--json"))
	if shown.Issue.Status != "closed" || shown.Issue.CloseReason != "landed" {
		t.Errorf("after the pass = %s (%q); want closed, landed", shown.Issue.Status, shown.Issue.CloseReason)
	}
	if out := alice.ok("show", id); !strings.Contains(out, "check landed: passed at 0a1b2c3") {
		t.Errorf("show after landing lacks the check:\n%s", out)
	}
	if out := agent.ok("inbox", nil); !strings.Contains(out, `"kind":"gate.passed"`) || !strings.Contains(out, id+" closed") {
		t.Errorf("the assignee's inbox = %s; want a gate.passed saying %s closed", out, id)
	}
	if p := decode[mcpserver.Prime](t, agent.ok("prime", nil)); len(p.Landing) != 0 {
		t.Errorf("prime's landing once landed = %+v; want none", p.Landing)
	}

	// A revert: the default branch's job fails landed on a later commit.
	ci.ok("gate", "fail", "--check", "landed", "--issue", id, "--sha", "4d5e6f7", "--reason", "reverted")
	shown = decode[proto.ShowResult](t, alice.ok("show", id, "--json"))
	if shown.Issue.Status != "open" {
		t.Errorf("after the failure = %s; want open", shown.Issue.Status)
	}
	if out := agent.ok("inbox", nil); !strings.Contains(out, `"kind":"gate.failed"`) || !strings.Contains(out, "reverted") {
		t.Errorf("the assignee's inbox = %s; want a gate.failed with the reason", out)
	}
	if ops, _ := alice.history(id); tally(ops, "issue.reopen") != 1 {
		t.Errorf("history = %v; want one reopen", ops)
	}
}

// A finish queued while the server was away, of an issue that requires
// landed, is refused when it arrives, and goes to the refused list with
// the fix to hand off.
func TestLandedReplayedFinishRefused(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{admins: []string{"dana"}})
	dana, alice := w.newUser("dana", ""), w.newUser("alice", "")
	dana.ok("admin", "workflow", "set", dana.writeWorkflow(t, landedWorkflow))
	id := strings.TrimSpace(alice.ok("create", "Needs landing"))
	alice.ok("start", id)

	w.stop()
	alice.ok("finish", id, "--reason", "done", "--tick", "1,2")
	w.start()

	r := alice.run("v0.2.0", "ready")
	if r.code != cli.ExitOK || r.stderr != "aries: outbox: refused 1 (see `aries outbox`)\n" {
		t.Fatalf("the first command with the server back = exit %d, stderr %q; want the finish refused", r.code, r.stderr)
	}
	out := alice.ok("outbox")
	for _, want := range []string{"  1  finish  " + id, "     acceptance: issue " + id + " waits for check landed",
		"     fix: " + proto.FixLand + " with `aries handoff " + id + " --state done`"} {
		if !strings.Contains(out, want) {
			t.Errorf("aries outbox lacks %q:\n%s", want, out)
		}
	}
	if ops, _ := alice.history(id); tally(ops, "issue.close") != 0 {
		t.Errorf("history = %v; want no close", ops)
	}
}

// An agent that hands off with state done and its met items in ticked
// needs nothing else: the landed pass closes its issue, landed, with no
// finish and no accept after the handoff. Ticks need state done.
func TestHandoffTicksLand(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{admins: []string{"dana"}})
	dana, alice, ci := w.newUser("dana", ""), w.newUser("alice", ""), w.newUser("ci-example", "")
	dana.ok("admin", "workflow", "set", dana.writeWorkflow(t, landedWorkflow))
	ci.env["CI"] = "true"

	agent := alice.mcp("v0.2.0")
	id := decode[proto.WriteResult](t, agent.ok("create", map[string]any{"title": "Ticked at handoff"})).ID
	agent.ok("start", map[string]any{"id": id})

	out, isErr := agent.call("handoff", map[string]any{"id": id, "note": "https://example.com/example/parser/pull/13", "ticked": []any{1}})
	if !isErr || !strings.Contains(out, "state done") {
		t.Fatalf("handoff with ticks and no state = %q (error %v); want it refused, naming state done", out, isErr)
	}
	agent.ok("handoff", map[string]any{"id": id, "note": "https://example.com/example/parser/pull/13", "state": "done", "ticked": []any{1}})
	ci.ok("gate", "pass", "--check", "landed", "--issue", id, "--sha", "0a1b2c3")

	shown := decode[proto.ShowResult](t, alice.ok("show", id, "--json"))
	if shown.Issue.Status != "closed" || shown.Issue.CloseReason != "landed" {
		t.Errorf("after the pass = %s (%q); want closed, landed", shown.Issue.Status, shown.Issue.CloseReason)
	}
}
