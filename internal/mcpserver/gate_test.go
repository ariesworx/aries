package mcpserver

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

// callsOf returns the arguments of f's calls of op, in order.
func (f *fakeConn) callsOf(op string) []any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []any
	for _, c := range f.calls {
		if c.op == op {
			out = append(out, c.args)
		}
	}
	return out
}

// An agent adds a gate and reads gates through the gate tool; passing
// one is a person's, so the tool has no pass (decision 76).
func TestGateTool(t *testing.T) {
	t.Parallel()
	f := &fakeConn{reply: func(op string, args any) (any, error) {
		switch op {
		case proto.OpGateAdd:
			a := args.(proto.GateAddArgs)
			return proto.Gate{ID: "sf-g1", Kind: "human", Approvers: a.Approvers, Holds: []string{a.Blocks}}, nil
		case proto.OpGates:
			return proto.GatesResult{Gates: []proto.Gate{{ID: "sf-g1", Title: "Approve the plan", Kind: "human",
				Approvers: []string{"carol"}, Holds: []string{"sf-e1"}}}}, nil
		}
		return nil, nil
	}}
	cs, _, _ := connectServer(t, f)
	res := callTool(t, cs, "gate", map[string]any{"action": "add", "blocks": "sf-e1", "approvers": []string{"carol"}})
	if res.IsError || text(t, res) != `{"id":"sf-g1","approvers":["carol"],"holds":["sf-e1"],"next":"a person passes it; until then sf-e1 is not ready"}` {
		t.Errorf("gate add = %s", text(t, res))
	}
	calls := f.callsOf(proto.OpGateAdd)
	if len(calls) != 1 {
		t.Fatalf("gate.add calls = %d, want 1", len(calls))
	}
	if a := calls[0].(proto.GateAddArgs); a.Blocks != "sf-e1" || !slices.Equal(a.Approvers, []string{"carol"}) || a.Idem == "" {
		t.Errorf("gate.add args = %+v, want sf-e1 for carol with a key", a)
	}
	res = callTool(t, cs, "gate", map[string]any{"action": "list"})
	if res.IsError || !strings.Contains(text(t, res), `"approvers":["carol"]`) {
		t.Errorf("gate list = %s", text(t, res))
	}
	if res := callTool(t, cs, "gate", map[string]any{"action": "add"}); !res.IsError {
		t.Errorf("gate add with no blocks = %s, want an error", text(t, res))
	}
}

// create and update carry defer_until and due; an empty one clears.
func TestToolDates(t *testing.T) {
	t.Parallel()
	f := &fakeConn{reply: func(op string, _ any) (any, error) {
		if op == proto.OpShow {
			return proto.ShowResult{Issue: proto.Issue{ID: "sf-a1", Rev: 3}}, nil
		}
		return proto.WriteResult{ID: "sf-a1", Rev: 4}, nil
	}}
	cs, _, _ := connectServer(t, f)
	if res := callTool(t, cs, "create", map[string]any{"title": "upgrade", "defer_until": "2026-11-02", "due": "2026-11-03T18:00:00Z"}); res.IsError {
		t.Fatalf("create = %s", text(t, res))
	}
	if a := f.callsOf(proto.OpCreate)[0].(proto.CreateArgs); a.DeferUntil != "2026-11-02" || a.Due != "2026-11-03T18:00:00Z" {
		t.Errorf("create args = %+v, want both dates", a)
	}
	if res := callTool(t, cs, "update", map[string]any{"id": "sf-a1", "due": ""}); res.IsError {
		t.Fatalf("update = %s", text(t, res))
	}
	if a := f.callsOf(proto.OpUpdate)[0].(proto.UpdateArgs); a.Due == nil || *a.Due != "" || a.DeferUntil != nil {
		t.Errorf("update args = %+v, want due cleared and defer_until left alone", a)
	}
}

// prime lists what is overdue, due soon and newly ready by its date.
func TestPrimeDates(t *testing.T) {
	t.Parallel()
	due := time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)
	f := &fakeConn{who: "alice", reply: func(op string, _ any) (any, error) {
		if op == proto.OpDates {
			return proto.DatesResult{Until: due.Add(time.Hour),
				Overdue: []proto.DigestItem{{ID: "sf-o1", Title: "late", Priority: 1, At: due}},
				DueSoon: []proto.DigestItem{{ID: "sf-s1", Title: "soon", Priority: 2, At: due.Add(5 * time.Hour)}},
				Ready:   []proto.DigestItem{{ID: "sf-r1", Title: "window open", Priority: 2, At: due}},
				Totals:  proto.DateTotals{Overdue: 1, DueSoon: 1, Ready: 1}}, nil
		}
		return proto.ListResult{Issues: []proto.Summary{}}, nil
	}}
	p, err := BuildPrime(t.Context(), f, "v0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	txt := p.Text()
	for _, want := range []string{
		"overdue:\n  sf-o1 P1 \"late\" due 2026-10-15T08:00Z\n",
		"due soon:\n  sf-s1 P2 \"soon\" due 2026-10-15T13:00Z\n",
		"ready by date:\n  sf-r1 P2 \"window open\" from 2026-10-15T08:00Z\n",
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("prime text:\n%s\nwant %q", txt, want)
		}
	}
	b, _ := json.Marshal(p)
	if !strings.Contains(string(b), `"overdue":[{"id":"sf-o1"`) {
		t.Errorf("prime JSON = %s, want its overdue", b)
	}
}
