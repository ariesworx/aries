package mcpserver

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
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

// An agent sees a ci gate's check and last report in the gate tool's
// list, and an issue's checks, with their evidence, in show; a closed
// issue whose landed check waits has not landed.
func TestCIGateResults(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 15, 9, 30, 0, 0, time.UTC)
	failed := &proto.GateReport{Result: "failed", By: "ci-example", At: at, SHA: "0123456789abcdef", URL: "https://ci.example.com/runs/7", Reason: "2 tests failed"}
	f := &fakeConn{reply: func(op string, _ any) (any, error) {
		switch op {
		case proto.OpGates:
			return proto.GatesResult{Gates: []proto.Gate{{ID: "sf-g1", Title: "build of sf-a1", Kind: "ci", Approvers: []string{"ci-example"},
				Check: "build", Subject: "sf-a1", Holds: []string{"sf-b1"}, Report: failed}}}, nil
		case proto.OpShow:
			return proto.ShowResult{Issue: proto.Issue{ID: "sf-a1", Title: "Do it", Status: "closed", Rev: 4}, Checks: []proto.Gate{
				{ID: "sf-g1", Kind: "ci", Check: "build", Approvers: []string{"ci-example"}, Report: failed},
				{ID: "sf-g2", Kind: "ci", Check: "landed", Approvers: []string{"ci-example"}},
			}}, nil
		}
		return nil, nil
	}}
	cs, _, _ := connectServer(t, f)
	res := callTool(t, cs, "gate", map[string]any{"action": "list"})
	if want := `{"id":"sf-g1","title":"build of sf-a1","approvers":["ci-example"],"holds":["sf-b1"],"check":"build","subject":"sf-a1",` +
		`"report":"failed at 0123456789ab: https://ci.example.com/runs/7 2 tests failed"}`; res.IsError || !strings.Contains(text(t, res), want) {
		t.Errorf("gate list = %s\nwant it to hold %s", text(t, res), want)
	}
	res = callTool(t, cs, "show", map[string]any{"id": "sf-a1"})
	if want := `"checks":["build failed at 0123456789ab: https://ci.example.com/runs/7 2 tests failed (sf-g1)","landed waits for ci-example (sf-g2)"]`; res.IsError || !strings.Contains(text(t, res), want) {
		t.Errorf("show = %s\nwant it to hold %s", text(t, res), want)
	}
}

// Checks with long links still fit a result: the last are dropped.
func TestShowChecksFit(t *testing.T) {
	t.Parallel()
	long := &proto.GateReport{Result: "failed", By: "ci-example", URL: "https://ci.example.com/" + strings.Repeat("r", 1000)}
	var checks []proto.Gate
	for i := range 20 {
		checks = append(checks, proto.Gate{ID: fmt.Sprintf("sf-g%d", i), Kind: "ci", Check: fmt.Sprintf("c%d", i), Report: long})
	}
	f := &fakeConn{reply: func(string, any) (any, error) {
		return proto.ShowResult{Issue: proto.Issue{ID: "sf-a1", Title: "Do it", Status: "open", Rev: 1}, Checks: checks}, nil
	}}
	cs, _, _ := connectServer(t, f)
	out := text(t, callTool(t, cs, "show", map[string]any{"id": "sf-a1"}))
	var got Issue
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if n := Tokens([]byte(out)); n > MaxResultTokens || len(got.Checks) == 0 || len(got.Checks) == 20 || !got.Truncated {
		t.Errorf("show with 20 long checks = %d tokens, %d checks, truncated %v; want under %d, some checks, truncated",
			n, len(got.Checks), got.Truncated, MaxResultTokens)
	}
}
