package e2e

import (
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/mcpserver"
	"github.com/ariesworx/aries/internal/proto"
)

// An issue under an epic whose approval gate is open leaves ready, and
// the agent is told why where it looks: the create or the reparenting
// that held it, blocked, and show. The message names the gate and says a
// person passes it; MCP still has no way to pass it.
func TestHeldUnderUnapprovedEpic(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{admins: []string{"dana"}})
	dana, alice, carol := w.newUser("dana", ""), w.newUser("alice", ""), w.newUser("carol", "")
	dana.ok("admin", "workflow", "set", dana.writeWorkflow(t, reviewWorkflow))
	agent := alice.mcp("v0.2.0")

	epic := decode[proto.WriteResult](t, agent.ok("create", map[string]any{"title": "Plan the parser", "type": "epic"})).ID
	gate := firstGate(t, carol.ok("gate", "list"))

	// Creating under the epic says the child is held.
	made := decode[mcpserver.Created](t, agent.ok("create", map[string]any{"title": "Step one", "parent": epic, "type": "task"}))
	if !strings.Contains(made.Held, gate) || !strings.Contains(made.Held, "aries gate pass "+gate) {
		t.Errorf("create under the epic: held = %q; want the gate %s and the command a person runs", made.Held, gate)
	}
	// An issue that is ready, moved under the epic, is told the same.
	loose := decode[proto.WriteResult](t, agent.ok("create", map[string]any{"title": "Step two", "type": "task"})).ID
	moved := decode[mcpserver.Updated](t, agent.ok("update", map[string]any{"id": loose, "parent": epic}))
	if !strings.Contains(moved.Held, gate) {
		t.Errorf("reparenting under the epic: held = %q; want it to name %s", moved.Held, gate)
	}
	// An update that leaves the parent alone says nothing of it.
	if u := decode[mcpserver.Updated](t, agent.ok("update", map[string]any{"id": loose, "title": "Step 2"})); u.Held != "" {
		t.Errorf("a title change reported held = %q; want none", u.Held)
	}
	// A task with no unapproved ancestor is not held.
	free := decode[mcpserver.Created](t, agent.ok("create", map[string]any{"title": "Free", "type": "task"}))
	if free.Held != "" {
		t.Errorf("create with no parent: held = %q; want none", free.Held)
	}

	// blocked names the gate and who passes it; show on a child does too.
	blocked := decode[mcpserver.Blocked](t, agent.ok("blocked", nil))
	if len(blocked.Gates) != 1 || !strings.Contains(blocked.Gates[0], gate) || !strings.Contains(blocked.Gates[0], "aries gate pass "+gate) ||
		!strings.Contains(blocked.Gates[0], "carol") {
		t.Errorf("blocked gates = %q; want %s, carol and the command", blocked.Gates, gate)
	}
	shown := decode[mcpserver.Issue](t, agent.ok("show", map[string]any{"id": made.ID}))
	if len(shown.Held) != 1 || !strings.Contains(shown.Held[0], gate) || !strings.Contains(shown.Held[0], "aries gate pass "+gate) {
		t.Errorf("show on the child: held = %q; want %s and the command", shown.Held, gate)
	}
	if free := decode[mcpserver.Issue](t, agent.ok("show", map[string]any{"id": free.ID})); len(free.Held) != 0 {
		t.Errorf("show on a free issue: held = %q; want none", free.Held)
	}

	// Once carol passes it, nothing is held.
	carol.ok("gate", "pass", gate, "--reason", "plan approved")
	if got := decode[mcpserver.Issue](t, agent.ok("show", map[string]any{"id": made.ID})); len(got.Held) != 0 {
		t.Errorf("show after the approval: held = %q; want none", got.Held)
	}
}
