package e2e

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// readyIDs is the ids an agent's ready tool lists.
func (a *agent) readyIDs() []string {
	a.t.Helper()
	r := decode[proto.ListResult](a.t, a.ok("ready", map[string]any{"limit": 50}))
	var ids []string
	for _, s := range r.Issues {
		ids = append(ids, s.ID)
	}
	return ids
}

// Stage 6, slice G0's gate (design §20.10): an issue behind a human gate
// is never in ready, and no MCP tool or CLI command of a non-approver
// starts it, or closes or reopens the gate; an approver's pass from the
// session cli makes it ready and pushes the event to a watching board;
// anyone else is refused, with the approvers named.
func TestHumanGate(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{admins: []string{"dana"}})
	alice, bob, carol, dana := w.newUser("alice", ""), w.newUser("bob", ""), w.newUser("carol", ""), w.newUser("dana", "")
	epic := strings.TrimSpace(alice.ok("create", "Plan", "the", "migration", "-t", "epic"))
	step := strings.TrimSpace(alice.ok("create", "Copy", "the", "tables", "--parent", epic, "--assignee", "bob"))
	other := strings.TrimSpace(alice.ok("create", "Unrelated", "work"))

	// A planning agent holds its plan for a person: the gate tool.
	planner := alice.mcp("v0.2.0")
	added := decode[struct {
		ID        string   `json:"id"`
		Approvers []string `json:"approvers"`
	}](t, planner.ok("gate", map[string]any{"action": "add", "blocks": epic, "approvers": []string{"carol"}}))
	gate := added.ID
	if !slices.Equal(added.Approvers, []string{"carol"}) {
		t.Fatalf("gate add = %+v, want carol as approver", added)
	}
	board := dana.board()
	board.until("the first read", "live")

	agent := bob.mcp("v0.2.0")
	if got := agent.readyIDs(); !slices.Equal(got, []string{other}) {
		t.Errorf("ready behind the gate = %v, want only %s", got, other)
	}
	refusedMCP := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"start the step", "start", map[string]any{"id": step}},
		{"start the epic", "start", map[string]any{"id": epic}},
		{"start the gate", "start", map[string]any{"id": gate}},
		{"close the gate", "close", map[string]any{"id": gate}},
		{"finish the gate", "finish", map[string]any{"id": gate}},
		{"set the gate's status", "update", map[string]any{"id": gate, "status": "deferred"}},
		{"remove the edge", "dep", map[string]any{"action": "rm", "id": epic, "depends_on": gate}},
		{"close the epic", "close", map[string]any{"id": epic}},
		{"move the step out", "update", map[string]any{"id": step, "parent": ""}},
	}
	for _, tc := range refusedMCP {
		out, isErr := agent.call(tc.tool, tc.args)
		if !isErr || !strings.Contains(out, "carol") || !strings.Contains(out, "fix: ") {
			t.Errorf("MCP %s = %q (error %v), want a refusal naming carol", tc.name, out, isErr)
		}
	}
	// Setting the held step back to open is let through, and undoes
	// nothing: the gate holds through the plan, not the status.
	agent.ok("update", map[string]any{"id": step, "status": "blocked"})
	agent.ok("update", map[string]any{"id": step, "status": "open"})
	if got := agent.readyIDs(); slices.Contains(got, step) {
		t.Errorf("ready after the agent set the step open = %v, want it still held", got)
	}

	refusedCLI := []struct {
		name string
		u    *user
		args []string
	}{
		{"start the step", bob, []string{"start", step}},
		{"close the gate", bob, []string{"close", gate}},
		{"pass the gate, not named", bob, []string{"gate", "pass", gate}},
		{"pass the gate, an admin not named", dana, []string{"gate", "pass", gate}},
		{"pass the gate, its maker's side", alice, []string{"gate", "pass", gate}},
		{"rearm an open gate's issue", bob, []string{"reopen", gate}},
	}
	for _, tc := range refusedCLI {
		r := tc.u.run("v0.2.0", tc.args...)
		if r.code != 1 || !strings.Contains(r.stderr, "carol") || !strings.Contains(r.stderr, "fix: ") {
			t.Errorf("sfx %s (%s) = exit %d\n%s%s\nwant exit 1 naming carol", strings.Join(tc.args, " "), tc.name, r.code, r.stdout, r.stderr)
		}
	}
	// The approver's own agent, running sfx in its shell, is not her.
	carol.env["CLAUDE_CODE_SESSION_ID"] = "claude-carol"
	if r := carol.run("v0.2.0", "gate", "pass", gate); r.code != 1 || !strings.Contains(r.stderr, "agent") {
		t.Errorf("sfx gate pass from carol's agent session = exit %d\n%s%s\nwant it refused", r.code, r.stdout, r.stderr)
	}
	delete(carol.env, "CLAUDE_CODE_SESSION_ID")
	if out := carol.ok("gate", "pass", gate, "--reason", "plan reviewed"); !strings.Contains(out, gate+" passed") {
		t.Errorf("sfx gate pass by carol = %q, want it passed", out)
	}
	board.until("the pass, pushed", "carol/cli gate.pass "+gate)

	got := agent.readyIDs()
	for _, id := range []string{epic, step, other} {
		if !slices.Contains(got, id) {
			t.Errorf("ready after the pass = %v, want %s", got, id)
		}
	}
	if slices.Contains(got, gate) {
		t.Errorf("ready after the pass = %v, want no gate", got)
	}
	if out, isErr := agent.call("reopen", map[string]any{"id": gate}); !isErr || !strings.Contains(out, "carol") {
		t.Errorf("MCP reopen of the passed gate = %q, want a refusal naming carol", out)
	}
	in := decode[proto.InboxResult](t, bob.ok("inbox", "--json"))
	if !slices.ContainsFunc(in.Items, func(it proto.InboxItem) bool { return it.Kind == "gate.passed" && it.Issue == step }) {
		t.Errorf("bob's inbox = %+v, want a gate.passed item for %s", in.Items, step)
	}
	agent.ok("start", map[string]any{"id": step})
}

// A maintenance window (design §20.3): an issue deferred to a date is out
// of ready until then, digest lists it once it became ready, and lists
// it as overdue once its due date passed; prime says the same.
func TestMaintenanceWindow(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	clk := &clock{t: start}
	w := newWorld(t, daemonOpts{now: clk.now})
	alice := w.newUser("alice", "")
	id := strings.TrimSpace(alice.ok("create", "Upgrade", "the", "database",
		"--defer-until", "2026-10-15T11:00:00Z", "--due", "2026-10-15T13:00:00Z"))
	if out := alice.ok("show", id); !strings.Contains(out, "not before 2026-10-15 11:00 UTC; due 2026-10-15 13:00 UTC") {
		t.Errorf("show lacks the dates:\n%s", out)
	}
	if out := alice.ok("ready"); strings.Contains(out, id) {
		t.Errorf("ready before the window:\n%s", out)
	}
	if out := alice.ok("digest"); !strings.Contains(out, "due soon 1\n  "+id) {
		t.Errorf("digest before the window lacks it as due soon:\n%s", out)
	}

	clk.add(3 * time.Hour) // 12:00, in the window
	if out := alice.ok("ready"); !strings.Contains(out, id) {
		t.Errorf("ready in the window lacks %s:\n%s", id, out)
	}
	if out := alice.ok("digest"); !strings.Contains(out, "ready since its date 1\n  "+id) {
		t.Errorf("digest in the window lacks it as ready since its date:\n%s", out)
	}

	clk.add(2 * time.Hour) // 14:00, past its due date
	if out := alice.ok("digest"); !strings.Contains(out, "overdue 1\n  "+id) {
		t.Errorf("digest past the due date lacks it as overdue:\n%s", out)
	}
	if out := alice.ok("prime"); !strings.Contains(out, "overdue:\n  "+id) {
		t.Errorf("prime past the due date lacks it as overdue:\n%s", out)
	}
	// Nothing was written when the dates passed: the issue is as it was.
	if h := decode[proto.HistoryResult](t, alice.ok("history", id, "--json")); len(h.Events) != 1 {
		t.Errorf("history = %+v, want only its create", h.Events)
	}
}
