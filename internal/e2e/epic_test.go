package e2e

import (
	"slices"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/mcpserver"
	"github.com/ariesworx/aries/internal/proto"
)

// An epic waits on its open children: ready, the MCP ready tool and
// prime leave it out, blocked says why, and making it wait on a child
// is refused as already so. Once its children close it is ready.
func TestEpicWaitsOnChildren(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{})
	alice := w.newUser("alice", "")

	epic := strings.TrimSpace(alice.ok("create", "Release v1.0.0", "-t", "epic", "-p", "0"))
	a := strings.TrimSpace(alice.ok("create", "Write the notes", "--parent", epic))
	b := strings.TrimSpace(alice.ok("create", "Tag the release", "--parent", epic))

	if out := alice.ok("ready"); strings.Contains(out, epic) || !strings.Contains(out, a) || !strings.Contains(out, b) {
		t.Fatalf("ready:\n%s", out)
	}
	if out := alice.ok("blocked"); !strings.Contains(out, epic) || !strings.Contains(out, "waiting on 2 open children") {
		t.Fatalf("blocked:\n%s", out)
	}
	bl := decode[proto.BlockedResult](t, alice.ok("blocked", "--json"))
	if len(bl.Issues) != 1 || bl.Issues[0].ID != epic || bl.Issues[0].Children != 2 || len(bl.Issues[0].BlockedBy) != 0 {
		t.Fatalf("blocked --json: %+v", bl)
	}

	e := alice.refused(proto.CodeCycle, "dep", "add", epic, a)
	if !strings.HasPrefix(e.Fix, proto.FixNothing) || !strings.Contains(e.Fix, "aries blocked") {
		t.Fatalf("dep add epic child refused with %+v, want a fix saying the epic waits already", e)
	}
	alice.refused(proto.CodeCycle, "dep", "add", a, epic)

	ag := alice.mcp("v0.2.0")
	if ids := ag.readyIDs(); slices.Contains(ids, epic) || !slices.Contains(ids, a) {
		t.Fatalf("MCP ready: %v", ids)
	}
	p := decode[mcpserver.Prime](t, ag.ok("prime", nil))
	for _, s := range p.Ready {
		if s.ID == epic {
			t.Fatalf("prime ready lists the epic: %+v", p.Ready)
		}
	}
	mb := decode[mcpserver.Blocked](t, ag.ok("blocked", nil))
	if len(mb.Issues) != 1 || mb.Issues[0].ID != epic || mb.Issues[0].Children != 2 {
		t.Fatalf("MCP blocked: %+v", mb)
	}

	alice.ok("close", a)
	if out := alice.ok("blocked"); !strings.Contains(out, "waiting on 1 open child") {
		t.Fatalf("blocked with one child open:\n%s", out)
	}
	alice.ok("close", b)
	if out := alice.ok("ready"); !strings.Contains(out, epic) {
		t.Fatalf("ready once the children closed:\n%s", out)
	}
	if out := alice.ok("blocked"); strings.Contains(out, epic) {
		t.Fatalf("blocked once the children closed:\n%s", out)
	}
}
