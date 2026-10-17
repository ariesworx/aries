package e2e

import (
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/mcpserver"
	"github.com/ariesworx/aries/internal/proto"
)

// reviewWorkflow has carol approve an epic's plan before its children
// start, and review a feature before it is finished.
const reviewWorkflow = `version: 1
types:
  epic:
    approve: {approvers: [carol]}
  feature:
    review: {approvers: [carol]}
`

// Slice W3's gate (design §21.11): an epic created under approve keeps its
// child out of ready until carol passes the gate from cli; a feature under
// review can be started, but finish is refused with a fix naming the gate
// until carol passes it; the agent is told and then finishes. MCP offers
// no way to pass either.
func TestReviewAndApproval(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{admins: []string{"dana"}})
	dana, alice, carol := w.newUser("dana", ""), w.newUser("alice", ""), w.newUser("carol", "")
	dana.ok("admin", "workflow", "set", dana.writeWorkflow(t, reviewWorkflow))

	agent := alice.mcp("v0.2.0")
	epic := decode[proto.WriteResult](t, agent.ok("create", map[string]any{"title": "Plan the parser", "type": "epic"})).ID
	child := decode[proto.WriteResult](t, agent.ok("create", map[string]any{"title": "Step one", "parent": epic, "type": "task"})).ID
	if out := alice.ok("ready"); strings.Contains(out, child) || strings.Contains(out, epic) {
		t.Errorf("ready before the approval lists the epic's plan:\n%s", out)
	}
	if out, isErr := agent.call("start", map[string]any{"id": child}); !isErr || !strings.Contains(out, "gate") {
		t.Errorf("start of the child behind the approval = %q (error %v); want a refusal naming the gate", out, isErr)
	}
	pending := carol.ok("inbox")
	if !strings.Contains(pending, "gate.pending") {
		t.Fatalf("carol's inbox lacks the pending approval:\n%s", pending)
	}
	gate := firstGate(t, carol.ok("gate", "list"))
	if r := alice.run("v0.2.0", "gate", "pass", gate); r.code != 1 {
		t.Errorf("gate pass by alice = exit %d; want a refusal: she is not an approver", r.code)
	}
	carol.ok("gate", "pass", gate, "--reason", "plan approved")
	if out := alice.ok("ready"); !strings.Contains(out, child) {
		t.Errorf("ready after the approval lacks the child:\n%s", out)
	}

	// A review holds nothing: start works, finish is refused until passed.
	feat := decode[proto.WriteResult](t, agent.ok("create", map[string]any{"title": "Parser design", "type": "feature"})).ID
	agent.ok("start", map[string]any{"id": feat})
	out, isErr := agent.call("finish", map[string]any{"id": feat})
	if !isErr || !strings.HasPrefix(out, "acceptance") || !strings.Contains(out, "review gate") ||
		!strings.Contains(out, "never run aries gate yourself") {
		t.Fatalf("finish under review = %q (error %v); want acceptance, naming the review gate and a person to pass it", out, isErr)
	}
	r := alice.run("v0.2.0", "--json", "close", feat)
	if e := cliError(t, r.stdout); r.code != 1 || e.Code != proto.CodeAcceptance || !strings.HasPrefix(e.Fix, proto.FixReview) ||
		!strings.Contains(e.Fix, "carol") {
		t.Errorf("aries close under review = exit %d, %+v; want acceptance, with a fix naming carol", r.code, e)
	}
	review := firstGate(t, carol.ok("gate", "list"))
	carol.ok("gate", "pass", review, "--reason", "design reviewed")
	if out := agent.ok("inbox", nil); !strings.Contains(out, "review gate "+review+" passed by carol") {
		t.Errorf("the agent's inbox = %s; want it told the review passed", out)
	}
	agent.ok("finish", map[string]any{"id": feat})
	if p := decode[mcpserver.Prime](t, agent.ok("prime", nil)); len(p.Landing) != 0 {
		t.Errorf("prime's landing = %+v; want none", p.Landing)
	}
}

// firstGate is the id of the first gate in out, a `gate list`.
func firstGate(t *testing.T, out string) string {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		if f := strings.Fields(line); len(f) > 0 && strings.Contains(f[0], "-") {
			return f[0]
		}
	}
	t.Fatalf("no gate in:\n%s", out)
	return ""
}
