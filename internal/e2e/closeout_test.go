package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ariesworx/aries/internal/mcpserver"
	"github.com/ariesworx/aries/internal/proto"
)

// blocks runs a tool and returns the text of each block of its result.
func (a *agent) blocks(name string, args map[string]any) []string {
	a.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := a.cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil || res.IsError {
		a.t.Fatalf("%s %v: %v %+v", name, args, err, res)
	}
	var out []string
	for _, c := range res.Content {
		out = append(out, c.(*mcp.TextContent).Text)
	}
	return out
}

// closeoutWorkflow gives each type close-out lines: a task's name a
// compose stack, an epic's the worktrees.
const closeoutWorkflow = `version: 1
ci: [ci-example]
types:
  default:
    acceptance:
      - PR opened
      - text: Merged
        check: landed
    checks: [landed]
    closeout:
      - Stop your compose stack
      - Archive the session
  epic:
    closeout:
      - Prune worktrees
  chore:
    acceptance: [Posted]
    closeout:
      - Delete your scratch files
`

// Slice W4's gate (design §21.7): a profile's closeout lines are shown
// after the result of handoff (done), finish and the close of an epic,
// in the inbox item of the closing pass, and in digest once an epic's
// last child has closed. A result field only: the protocol is the same.
func TestCloseout(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{admins: []string{"dana"}})
	dana, alice, ci := w.newUser("dana", ""), w.newUser("alice", ""), w.newUser("ci-example", "")
	dana.ok("admin", "workflow", "set", dana.writeWorkflow(t, closeoutWorkflow))
	ci.env["CI"] = "true"
	agent := alice.mcp("v0.2.0")

	// A task: its handoff, once done, shows the checklist; one that is
	// not done shows none.
	task := decode[proto.WriteResult](t, agent.ok("create", map[string]any{"title": "Implement the parser"})).ID
	agent.ok("start", map[string]any{"id": task})
	alice.ok("accept", task, "1")
	if got := agent.blocks("handoff", map[string]any{"id": task, "note": "wip"}); len(got) != 1 {
		t.Errorf("handoff without a state done = %q; want the result line alone", got)
	}
	got := agent.blocks("handoff", map[string]any{"id": task, "note": "https://example.com/pr/1", "state": "done"})
	if len(got) != 2 || got[1] != "closeout:\n- Stop your compose stack\n- Archive the session" {
		t.Fatalf("handoff done = %q; want the result line, then the checklist", got)
	}
	if out := alice.ok("handoff", task, "again", "--state", "done"); !strings.HasSuffix(out,
		"\ncloseout:\n  - Stop your compose stack\n  - Archive the session\n") {
		t.Errorf("aries handoff --state done =\n%s", out)
	}
	if out := alice.ok("--json", "handoff", task, "json", "--state", "done"); !strings.Contains(out, `"closeout":["Stop your compose stack","Archive the session"]`) {
		t.Errorf("aries --json handoff = %s; want closeout", out)
	}

	// The closing pass, when the session is gone, leaves the checklist
	// in the inbox.
	ci.ok("gate", "pass", "--check", "landed", "--issue", task, "--sha", "0a1b2c3")
	inbox := agent.ok("inbox", nil)
	if !strings.Contains(inbox, "close-out of "+task+": Stop your compose stack; Archive the session") {
		t.Errorf("the assignee's inbox lacks the checklist:\n%s", inbox)
	}

	// An epic with two chores: digest names it once the last has
	// closed, and the finish that closes it shows its checklist.
	epic := decode[proto.WriteResult](t, agent.ok("create", map[string]any{"title": "The epic", "type": "epic"})).ID
	var chores []string
	for _, title := range []string{"first chore", "second chore"} {
		id := decode[proto.WriteResult](t, agent.ok("create", map[string]any{"title": title, "type": "chore", "parent": epic})).ID
		chores = append(chores, id)
	}
	for i, id := range chores {
		if out := alice.ok("digest"); strings.Contains(out, "epics to close out") {
			t.Fatalf("digest names the epic with %d chores left:\n%s", len(chores)-i, out)
		}
		agent.ok("start", map[string]any{"id": id})
		got := agent.blocks("finish", map[string]any{"id": id, "ticked": []any{1}})
		if len(got) != 2 || got[1] != "closeout:\n- Delete your scratch files" {
			t.Errorf("finish of a chore = %q; want its own checklist", got)
		}
	}
	d := decode[mcpserver.Digest](t, agent.ok("digest", nil))
	if len(d.Closeout) != 1 || d.Closeout[0].ID != epic || d.Closeout[0].Note != "Prune worktrees" || d.Totals.Closeout != 1 {
		t.Errorf("digest closeout = %+v (total %d); want %s, Prune worktrees", d.Closeout, d.Totals.Closeout, epic)
	}
	if out := alice.ok("digest"); !strings.Contains(out, "epics to close out 1") || !strings.Contains(out, "Prune worktrees") {
		t.Errorf("aries digest lacks the epic to close out:\n%s", out)
	}
	agent.ok("start", map[string]any{"id": epic})
	if got := agent.blocks("finish", map[string]any{"id": epic}); len(got) != 2 || got[1] != "closeout:\n- Prune worktrees" {
		t.Errorf("finish of the epic = %q; want its checklist", got)
	}
	if out := alice.ok("digest"); strings.Contains(out, "epics to close out") {
		t.Errorf("digest names the epic once closed:\n%s", out)
	}

	// close shows an epic's checklist and no other issue's.
	other := decode[proto.WriteResult](t, agent.ok("create", map[string]any{"title": "Another epic", "type": "epic"})).ID
	if got := agent.blocks("close", map[string]any{"id": other}); len(got) != 2 || got[1] != "closeout:\n- Prune worktrees" {
		t.Errorf("close of an epic = %q; want its checklist", got)
	}
	chore := decode[proto.WriteResult](t, agent.ok("create", map[string]any{"title": "A chore", "type": "chore"})).ID
	agent.ok("start", map[string]any{"id": chore})
	alice.ok("accept", chore, "1")
	if got := agent.blocks("close", map[string]any{"id": chore, "reason": "x"}); len(got) != 1 {
		t.Errorf("close of a chore = %q; want the result line alone", got)
	}
}
