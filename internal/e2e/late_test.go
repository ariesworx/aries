package e2e

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/cli"
	"github.com/ariesworx/aries/internal/mcpserver"
	"github.com/ariesworx/aries/internal/proto"
)

// A person going away extends their claims first (design §18.6): with
// `aries away 3d`, a claim taken for an hour holds for three days, against
// another principal's start, and lets go after.
func TestAwayHoldsPastTheLease(t *testing.T) {
	t.Parallel()
	clk := &clock{t: time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)}
	w := newWorld(t, daemonOpts{now: clk.now})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	id := strings.TrimSpace(alice.ok("create", "Held while away"))
	alice.ok("start", id, "--for", "1h")
	if out := alice.ok("away", "3d"); !strings.Contains(out, id+" held until ") {
		t.Fatalf("aries away 3d printed %q, want the claim held", out)
	}
	if r := alice.run("v0.2.0", "away", "8d"); r.code != cli.ExitFailure || !strings.Contains(r.stderr, "fix: give `aries away` a duration from 1m to at most 7d, this server's away_max") {
		t.Errorf("aries away 8d = exit %d, %q; want refused, naming the cap", r.code, r.stderr)
	}

	clk.add(2 * 24 * time.Hour)
	if r := bob.run("v0.2.0", "start", id); r.code != cli.ExitFailure || !strings.Contains(r.stderr, "in progress by alice") {
		t.Errorf("bob's start two days into alice's away = exit %d, %q; want it refused, alice holding it", r.code, r.stderr)
	}
	clk.add(24*time.Hour + time.Minute)
	bob.ok("start", id)
	if ops, _ := bob.history(id); tally(ops, "claim.away") != 1 {
		t.Errorf("history = %v, want one claim.away", ops)
	}
}

// lateWorld is a world in which alice's agent holds an issue and
// finishes it with the server away, until its lease has lapsed; the
// server is back when it returns.
func lateWorld(t *testing.T, args map[string]any) (w *world, clk *clock, alice, bob *user, ag *agent, id string) {
	t.Helper()
	clk = &clock{t: time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)}
	w = newWorld(t, daemonOpts{now: clk.now})
	alice, bob = w.newUser("alice", ""), w.newUser("bob", "")
	alice.now, bob.now = clk.now, clk.now
	alice.queues = true
	alice.env["CLAUDE_CODE_SESSION_ID"] = "cc-late"
	ag = alice.mcp("v0.2.0")
	id = strings.TrimSpace(alice.ok("create", "Finished offline"))
	if st := decode[mcpserver.Started](t, ag.ok("start", map[string]any{"id": id})); st.ID != id {
		t.Fatalf("the agent started %s, want %s", st.ID, id)
	}
	bob.ok("ready") // bob's key is known before the outage

	w.stop()
	args["id"] = id
	if out, isErr := ag.call("finish", args); isErr || !strings.HasPrefix(out, "queued: ") {
		t.Fatalf("finish with the server away = %q, error %t; want it queued", out, isErr)
	}
	clk.add(20 * time.Minute) // past the agent's 15-minute lease
	w.start()
	return w, clk, alice, bob, ag, id
}

// The agent's finish arrives after its lease lapsed, and nobody took the
// issue meanwhile: it applies, and the history says the lease had
// lapsed.
func TestLateFinishNobodyTookIt(t *testing.T) {
	t.Parallel()
	_, _, alice, bob, _, id := lateWorld(t, map[string]any{"reason": "done offline", "handoff": "all of it"})
	if r := alice.run("v0.2.0", "ready"); r.code != cli.ExitOK || r.stderr != "aries: outbox: delivered 1\n" {
		t.Fatalf("the first command with the server back = exit %d, stderr %q; want the finish delivered", r.code, r.stderr)
	}
	s := decode[proto.ShowResult](t, bob.ok("show", id, "--json"))
	if s.Issue.Status != "closed" || s.Issue.CloseReason != "done offline" {
		t.Errorf("after the late finish the issue is %s (%q), want it closed as the agent finished it", s.Issue.Status, s.Issue.CloseReason)
	}
	ops, _ := bob.history(id)
	lapsed, closed := slices.Index(ops, "claim.lapsed"), slices.Index(ops, "issue.close")
	if lapsed < 0 || closed < lapsed || tally(ops, "issue.kept") != 0 {
		t.Errorf("history = %v; want claim.lapsed, then the close", ops)
	}
}

// The agent's finish arrives after bob took the issue: it is kept as a
// comment, its discovered issue is filed, bob's claim is untouched, and
// both are told.
func TestLateFinishTakenByAnother(t *testing.T) {
	t.Parallel()
	_, _, alice, bob, ag, id := lateWorld(t, map[string]any{"reason": "done offline", "handoff": "all of it",
		"discovered": []any{map[string]any{"title": "Found offline"}}})
	bob.ok("start", id)

	const kept = "aries: outbox: delivered 1 (1 kept as a comment, as the issue moved on while it waited: `aries inbox` says why)\n"
	if r := alice.run("v0.2.0", "ready"); r.code != cli.ExitOK || r.stderr != kept {
		t.Fatalf("the first command with the server back = exit %d, stderr %q; want %q", r.code, r.stderr, kept)
	}
	s := decode[proto.ShowResult](t, bob.ok("show", id, "--json"))
	if s.Issue.Status != "in_progress" || s.Claim == nil || s.Claim.By != "bob" {
		t.Errorf("after the kept finish the issue is %s, claim %+v; want bob's, in progress", s.Issue.Status, s.Claim)
	}
	if n := alice.titled("Found offline"); n != 1 {
		t.Errorf("%d issues titled Found offline, want the discovered one filed", n)
	}
	cs := decode[proto.CommentsResult](t, bob.ok("comments", id, "--json")).Comments
	if len(cs) != 1 || cs[0].Author != "alice" || !strings.Contains(cs[0].Body, "kept as a comment: the claim was taken since, by bob") ||
		!strings.Contains(cs[0].Body, "all of it") || !strings.Contains(cs[0].Body, "reason: done offline") {
		t.Errorf("comments = %+v; want alice's finish kept, saying why", cs)
	}
	if ops, _ := bob.history(id); tally(ops, "issue.kept") != 1 || tally(ops, "issue.close") != 0 {
		t.Errorf("history = %v; want issue.kept and no close", ops)
	}

	box := decode[mcpserver.Inbox](t, ag.content("inbox", nil)[0])
	var agentItems []proto.InboxItem
	for _, it := range box.Items {
		agentItems = append(agentItems, proto.InboxItem{Kind: it.Kind, Issue: it.Issue, Body: it.Body})
	}
	if !inboxSays(agentItems, "conflict.parked", id, "was kept as comment "+cs[0].ID) {
		t.Errorf("the agent's inbox = %+v; want a conflict item naming the comment", box.Items)
	}
	bobBox := decode[proto.InboxResult](t, bob.ok("inbox", "--json"))
	if !inboxSays(bobBox.Items, "conflict.parked", id, "alice's finish") {
		t.Errorf("bob's inbox = %+v; want a conflict item on the issue he holds", bobBox.Items)
	}
}

func inboxSays(items []proto.InboxItem, kind, issue, what string) bool {
	for _, it := range items {
		if it.Kind == kind && it.Issue == issue && strings.Contains(it.Body, what) {
			return true
		}
	}
	return false
}

// A client speaks protocol 9 (and a server serves 8 to 9): against a
// server of 7 alone it is refused at the handshake with exit code 3 and
// a fix, rather than say hello again in 7, and what it queued while the
// server was away waits, unsent and not refused, until a server of 9
// takes it.
func TestServerOfAnotherProtocol(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{})
	alice := w.newUser("alice", "")
	id := strings.TrimSpace(alice.ok("create", "Commented while away"))

	w.stop()
	if out := alice.ok("comment", id, "written while away"); !strings.HasPrefix(out, "queued (") {
		t.Fatalf("a comment with the server away printed %q, want it queued", out)
	}
	w.opts.protoMin, w.opts.protoMax = 7, 7
	w.start()
	r := alice.run("v0.2.0", "ready")
	if r.code != cli.ExitVersion || !strings.Contains(r.stderr, "aries speaks protocol 9; the server (") ||
		!strings.Contains(r.stderr, "is older and accepts protocol 7") || !strings.Contains(r.stderr, "\nfix: ") {
		t.Errorf("against a server of 7 alone = exit %d, %q; want refused at the handshake with version and a fix", r.code, r.stderr)
	}
	if out := alice.ok("outbox"); !strings.Contains(out, "1 waiting, 0 refused") {
		t.Errorf("aries outbox after the refusal:\n%s\nwant the comment waiting, not refused", out)
	}

	w.stop()
	w.opts.protoMin, w.opts.protoMax = 0, 0
	w.start()
	if r := alice.run("v0.2.0", "ready"); r.code != cli.ExitOK || r.stderr != "aries: outbox: delivered 1\n" {
		t.Errorf("against a server of 8 = exit %d, %q; want the comment delivered", r.code, r.stderr)
	}
}
