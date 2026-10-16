package e2e

// Slice F4 (design §18.7, §18.10): conflicts parked on the server and
// resolved by a person, with an agent able to propose; through the CLI,
// MCP, SSH and a real daemon.

import (
	"strconv"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/cli"
	"github.com/ariesworx/aries/internal/mcpserver"
	"github.com/ariesworx/aries/internal/proto"
)

// TestConflictParkedAndResolved is F4's gate: "Two edits of one field
// park; two of different fields both apply." Alice's agent holds an
// issue and renames it; alice, at her terminal with the server away,
// edits its title and body from the rev she read. Her replay applies the
// body and parks the title; both sessions are told; the agent reads the
// conflict in show and proposes; and alice, a person, approves the
// proposal with `aries resolve --accept`.
func TestConflictParkedAndResolved(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{})
	alice := w.newUser("alice", "")
	id := strings.TrimSpace(alice.ok("create", "Original title", "--body", "original body"))
	rev := decode[proto.ShowResult](t, alice.ok("show", id, "--json")).Issue.Rev

	alice.env["CLAUDE_CODE_SESSION_ID"] = "cc-agent"
	ag := alice.mcp("v0.2.0")
	ag.ok("start", map[string]any{"id": id})
	delete(alice.env, "CLAUDE_CODE_SESSION_ID") // the terminal's own session from here
	ag.ok("update", map[string]any{"id": id, "title": "The agent's title"})

	w.stop()
	if out := alice.ok("update", id, "--rev", strconv.FormatInt(rev, 10), "--title", "Alice's title", "--body", "alice's body"); !strings.HasPrefix(out, "queued (") {
		t.Fatalf("an update with the server away printed %q, want it queued", out)
	}
	w.start()
	r := alice.run("v0.2.0", "ready")
	if r.code != cli.ExitOK || r.stderr != "aries: outbox: delivered 1 (1 parked conflicts for a person to resolve: `aries conflicts`)\n" {
		t.Fatalf("the replay = exit %d, stderr %q; want it delivered and parked", r.code, r.stderr)
	}
	s := decode[proto.ShowResult](t, alice.ok("show", id, "--json"))
	if s.Issue.Title != "The agent's title" || s.Issue.Body != "alice's body" || len(s.Conflicts) != 1 {
		t.Fatalf("after the replay the issue is %q, %q with conflicts %+v; want the agent's title, alice's body and one conflict",
			s.Issue.Title, s.Issue.Body, s.Conflicts)
	}
	c := s.Conflicts[0]
	if c.Field != "title" || c.Base != "Original title" || c.Mine != "Alice's title" || c.Theirs != "The agent's title" {
		t.Errorf("the conflict is %+v; want the title, with base, mine and theirs", c)
	}
	list := alice.ok("conflicts", id)
	for _, want := range []string{"#" + strconv.FormatInt(c.ID, 10) + " " + id + " title: alice's update from rev", "mine:   Alice's title", "theirs: The agent's title"} {
		if !strings.Contains(list, want) {
			t.Errorf("aries conflicts lacks %q:\n%s", want, list)
		}
	}

	// Both sessions are told: the terminal's inbox, and the agent's.
	in := decode[proto.InboxResult](t, alice.ok("inbox", "--json"))
	if in.Conflicts != 1 || !hasKind(in.Items, "conflict.parked") {
		t.Errorf("alice's inbox = %+v; want a conflict item and the count", in)
	}
	p := decode[mcpserver.Prime](t, ag.ok("prime", nil))
	if p.Conflicts != 1 {
		t.Errorf("the agent's prime counts %d conflicts, want 1", p.Conflicts)
	}
	if out := ag.ok("inbox", nil); !strings.Contains(out, `"kind":"conflict.parked"`) {
		t.Errorf("the agent's inbox = %s, want the conflict item", out)
	}

	// The agent reads the sides in show, and proposes; nothing changes.
	shown := decode[mcpserver.Issue](t, ag.ok("show", map[string]any{"id": id}))
	if len(shown.Conflicts) != 1 || shown.Conflicts[0].Mine != "Alice's title" {
		t.Fatalf("the agent's show = %+v, want the conflict", shown.Conflicts)
	}
	out := ag.ok("comment", map[string]any{"id": id, "conflict": shown.Conflicts[0].ID, "propose": "Alice's title, the agent's wording",
		"body": "keeps alice's subject with the agent's wording"})
	if !strings.Contains(out, "a person accepts it") {
		t.Errorf("the proposal answered %s", out)
	}
	if got := decode[proto.ShowResult](t, alice.ok("show", id, "--json")).Issue.Title; got != "The agent's title" {
		t.Errorf("a proposal changed the title to %q", got)
	}
	one := alice.ok("conflicts", strconv.FormatInt(c.ID, 10))
	if !strings.Contains(one, "proposed by alice: text: Alice's title, the agent's wording (keeps alice's subject with the agent's wording)") {
		t.Errorf("aries conflicts N lacks the proposal:\n%s", one)
	}

	// A person approves it.
	alice.ok("resolve", strconv.FormatInt(c.ID, 10), "--accept")
	s = decode[proto.ShowResult](t, alice.ok("show", id, "--json"))
	if s.Issue.Title != "Alice's title, the agent's wording" || len(s.Conflicts) != 0 {
		t.Errorf("after the approval the issue is %q with conflicts %+v; want the proposal applied and none left", s.Issue.Title, s.Conflicts)
	}
	if out := alice.ok("conflicts"); out != "no open conflicts\n" {
		t.Errorf("aries conflicts after the approval = %q", out)
	}
	ops, _ := alice.history(id)
	for _, op := range []string{"conflict.park", "conflict.propose", "conflict.resolve"} {
		if tally(ops, op) != 1 {
			t.Errorf("history %v; want one %s", ops, op)
		}
	}
	if r := alice.run("v0.2.0", "resolve", strconv.FormatInt(c.ID, 10), "--mine"); r.code != cli.ExitFailure ||
		!strings.Contains(r.stderr, "already resolved") || !strings.Contains(r.stderr, "fix: ") {
		t.Errorf("resolving it again = exit %d, %q; want refused with a fix", r.code, r.stderr)
	}
}

func hasKind(items []proto.InboxItem, kind string) bool {
	for _, it := range items {
		if it.Kind == kind {
			return true
		}
	}
	return false
}
