package e2e

import (
	"slices"
	"strconv"
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
	if !inboxSays(agentItems, "conflict", id, "was kept as comment "+cs[0].ID) {
		t.Errorf("the agent's inbox = %+v; want a conflict item naming the comment", box.Items)
	}
	bobBox := decode[proto.InboxResult](t, bob.ok("inbox", "--json"))
	if !inboxSays(bobBox.Items, "conflict", id, "alice's finish") {
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

// A client of protocol 8 against a server of 7 speaks 7 (design §18.6):
// a finish queued after its claim was taken goes there without park, and
// is refused as before, and waits for a person; once the server is at 8,
// a retry keeps it as a comment. A server of 6 alone is too old for this
// client, and refuses it with version. It replaces F4's test of protocol
// 6, which this client no longer speaks.
func TestClientStepsDownToProtocol7(t *testing.T) {
	t.Parallel()
	clk := &clock{t: time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)}
	w := newWorld(t, daemonOpts{now: clk.now, protoMin: 7, protoMax: 7})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	alice.now, bob.now = clk.now, clk.now
	id := strings.TrimSpace(alice.ok("create", "On a server of protocol 7"))
	alice.ok("start", id, "--for", "1h")
	rev := decode[proto.ShowResult](t, bob.ok("show", id, "--json")).Issue.Rev

	w.stop()
	if out := alice.ok("finish", id, "--reason", "done while away"); !strings.HasPrefix(out, "queued (") {
		t.Fatalf("a finish with the server away printed %q, want it queued", out)
	}
	if out := alice.ok("update", id, "--rev", strconv.FormatInt(rev, 10), "--title", "Renamed while away"); !strings.HasPrefix(out, "queued (") {
		t.Fatalf("an update with the server away printed %q, want it queued", out)
	}
	clk.add(2 * time.Hour)
	w.start()
	bob.ok("start", id)
	if r := alice.run("v0.2.0", "ready"); r.stderr != "aries: outbox: refused 2 (see `aries outbox`)\n" {
		t.Errorf("the replay to a server of 7 said %q, want the finish and the update refused", r.stderr)
	}
	out := alice.ok("outbox")
	if !strings.Contains(out, "     forbidden: ") {
		t.Errorf("aries outbox with the server at 7:\n%s", out)
	}

	w.stop()
	w.opts.protoMin, w.opts.protoMax = 0, 0
	w.start()
	alice.ok("outbox", "retry", firstRefused(t, out))
	if r := alice.run("v0.2.0", "ready"); r.stderr != "aries: outbox: delivered 1 (1 kept as a comment, as the issue moved on while it waited: `aries inbox` says why)\n" {
		t.Errorf("the retry at 8 said %q, want it kept as a comment", r.stderr)
	}
	if s := decode[proto.ShowResult](t, bob.ok("show", id, "--json")); s.Issue.Status != "in_progress" || s.Claim == nil || s.Claim.By != "bob" {
		t.Errorf("after the retry the issue is %s with claim %+v; want bob's", s.Issue.Status, s.Claim)
	}

	w.stop()
	w.opts.protoMin, w.opts.protoMax = 6, 6
	w.start()
	if r := alice.run("v0.2.0", "ready"); r.code != cli.ExitVersion || !strings.Contains(r.stderr, "the server (") {
		t.Errorf("against a server of 6 alone = exit %d, %q; want refused with version", r.code, r.stderr)
	}
}
