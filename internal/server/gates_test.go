package server

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

// carol is an approver of the gates these tests add; the CLI actors are
// people at their own terminals, and the rest agents' sessions.
var (
	carolCLI = store.Actor{Principal: "carol", Session: proto.CLISession, Machine: "laptop-c"}
	bobCLI   = store.Actor{Principal: "bob", Session: proto.CLISession, Machine: "laptop-b"}
	danaCLI  = store.Actor{Principal: "dana", Session: proto.CLISession, Machine: "laptop-d"}
	aliceCLI = store.Actor{Principal: "alice", Session: proto.CLISession, Machine: "laptop-a"}
)

// Who may pass a human gate (decision 68): the principals it names, else
// the project's admins; from the session cli only; its creator only when
// named. Anyone else is refused forbidden, with the approvers named in
// the message and the fix.
func TestGatePassWho(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	tests := []struct {
		name      string
		approvers []string
		by        store.Actor
		// names is what the refusal names; "" wants the pass.
		names string
	}{
		{name: "the approver at their terminal", approvers: []string{"carol"}, by: carolCLI},
		{name: "the approver from an agent's session", approvers: []string{"carol"}, by: carol, names: "agent's session"},
		{name: "someone else", approvers: []string{"carol"}, by: bobCLI, names: "carol"},
		{name: "an admin the gate does not name", approvers: []string{"carol"}, by: danaCLI, names: "carol"},
		{name: "its creator, not named", approvers: []string{"carol"}, by: aliceCLI, names: "carol"},
		{name: "its creator, named", approvers: []string{"alice", "carol"}, by: aliceCLI},
		{name: "an admin when it names none", by: danaCLI},
		{name: "a member when it names none", by: bobCLI, names: "the project's admins"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			is := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: tc.name})
			g := mustCall[proto.Gate](t, s, alice, proto.OpGateAdd, proto.GateAddArgs{Blocks: is.ID, Approvers: tc.approvers})
			got, perr := call[proto.Gate](t, s, tc.by, proto.OpGatePass, proto.GateArgs{ID: g.ID})
			if tc.names == "" {
				if perr != nil || got.Outcome != "passed" || got.By != tc.by.Principal {
					t.Errorf("gate.pass by %s/%s = %+v, %+v; want passed", tc.by.Principal, tc.by.Session, got, perr)
				}
				return
			}
			if perr == nil || perr.Code != proto.CodeForbidden || !strings.Contains(perr.Message, tc.names) ||
				!strings.HasPrefix(perr.Fix, proto.FixGate) {
				t.Errorf("gate.pass by %s/%s = %+v, want forbidden naming %q, its fix starting %q", tc.by.Principal, tc.by.Session, perr, tc.names, proto.FixGate)
			}
		})
	}
}

// An agent's undo of a hold is refused, naming the approvers: starting a
// held issue, closing the gate, removing the edge, closing the epic.
func TestGateRefusalsNameApprovers(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	epic := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "plan", Type: "epic"})
	child := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "step", Parent: epic.ID})
	g := mustCall[proto.Gate](t, s, alice, proto.OpGateAdd, proto.GateAddArgs{Blocks: epic.ID, Approvers: []string{"carol"}})
	for _, tc := range []struct {
		name string
		op   string
		args any
	}{
		{"start the child", proto.OpStart, proto.StartArgs{ID: child.ID}},
		{"close the gate", proto.OpClose, proto.CloseArgs{ID: g.ID}},
		{"reopen the gate", proto.OpReopen, proto.ReopenArgs{ID: g.ID}},
		{"remove the edge", proto.OpDepRm, proto.DepArgs{From: epic.ID, To: g.ID}},
		{"close the epic", proto.OpClose, proto.CloseArgs{ID: epic.ID}},
		{"finish the child", proto.OpFinish, proto.FinishArgs{ID: child.ID}},
	} {
		_, perr := call[proto.WriteResult](t, s, bob, tc.op, tc.args)
		if perr == nil || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Message, "carol") ||
			!strings.HasPrefix(perr.Fix, proto.FixGate) || !strings.Contains(perr.Fix, "aries gate pass "+g.ID) {
			t.Errorf("%s = %+v, want invalid naming carol, with a fix naming `aries gate pass %s`", tc.name, perr, g.ID)
		}
	}
	if _, perr := call[proto.WriteResult](t, s, bob, proto.OpCreate, proto.CreateArgs{Title: "x", Type: "gate"}); perr == nil ||
		!strings.HasPrefix(perr.Fix, proto.FixGateAdd) {
		t.Errorf("create with type gate = %+v, want a fix starting %q", perr, proto.FixGateAdd)
	}
	// show and blocked say who may pass the gate.
	sh := mustCall[proto.ShowResult](t, s, bob, proto.OpShow, proto.ShowArgs{ID: child.ID})
	if len(sh.Gates) != 1 || sh.Gates[0].ID != g.ID || !slices.Equal(sh.Gates[0].Approvers, []string{"carol"}) {
		t.Errorf("show(child).gates = %+v, want %s naming carol", sh.Gates, g.ID)
	}
	gs := mustCall[proto.ShowResult](t, s, bob, proto.OpShow, proto.ShowArgs{ID: g.ID})
	if gs.Gate == nil || gs.Gate.Kind != "human" || !slices.Equal(gs.Gate.Holds, []string{epic.ID}) {
		t.Errorf("show(gate).gate = %+v, want a human gate holding %s", gs.Gate, epic.ID)
	}
	bl := mustCall[proto.BlockedResult](t, s, bob, proto.OpBlocked, proto.LimitArgs{})
	if len(bl.Gates) != 1 || bl.Gates[0].ID != g.ID {
		t.Errorf("blocked.gates = %+v, want %s", bl.Gates, g.ID)
	}
	list := mustCall[proto.GatesResult](t, s, bob, proto.OpGates, proto.GatesArgs{})
	if len(list.Gates) != 1 || list.Gates[0].ID != g.ID {
		t.Errorf("gates = %+v, want %s", list.Gates, g.ID)
	}
	// Waiving is an admin's; carol, the approver, is told to pass it.
	if _, perr := call[proto.Gate](t, s, carolCLI, proto.OpGateWaive, proto.GateArgs{ID: g.ID, Reason: "x"}); perr == nil ||
		perr.Code != proto.CodeForbidden || !strings.Contains(perr.Fix, "admin") {
		t.Errorf("gate.waive by carol = %+v, want forbidden naming the admins", perr)
	}
}

// Dates on the wire: create and update take defer_until and due, as a
// date or a time; an empty one clears; show and list return them; one
// that is no date is refused with a fix; dates and digest list what is
// due soon, by the server's due_soon, and overdue.
func TestDatesOnTheWire(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	clk := &clock{t: start}
	s, _ := newServerConfig(t, Config{DueSoon: 48 * time.Hour}, clk.now)
	is := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate,
		proto.CreateArgs{Title: "upgrade", DeferUntil: "2026-10-16", Due: "2026-10-16T18:00:00Z"})
	sh := mustCall[proto.ShowResult](t, s, alice, proto.OpShow, proto.ShowArgs{ID: is.ID})
	defer16 := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 10, 16, 18, 0, 0, 0, time.UTC)
	if sh.Issue.DeferUntil == nil || !sh.Issue.DeferUntil.Equal(defer16) || sh.Issue.Due == nil || !sh.Issue.Due.Equal(due) {
		t.Fatalf("show = defer %v, due %v; want %v and %v", sh.Issue.DeferUntil, sh.Issue.Due, defer16, due)
	}
	ls := mustCall[proto.ListResult](t, s, alice, proto.OpList, proto.ListArgs{})
	if len(ls.Issues) != 1 || ls.Issues[0].Due == nil || ls.Issues[0].DeferUntil == nil {
		t.Errorf("list = %+v, want the issue with its dates", ls.Issues)
	}
	if _, perr := call[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "x", Due: "next tuesday"}); perr == nil ||
		perr.Code != proto.CodeInvalid || !strings.Contains(perr.Fix, "2026-11-02") {
		t.Errorf("create with due %q = %+v, want invalid with a fix showing a date", "next tuesday", perr)
	}
	d := mustCall[proto.DatesResult](t, s, alice, proto.OpDates, proto.DatesArgs{})
	if len(d.DueSoon) != 1 || d.DueSoon[0].ID != is.ID || d.Totals.DueSoon != 1 {
		t.Errorf("dates within due_soon 48h = %+v, want %s due soon", d, is.ID)
	}
	clk.advance(36 * time.Hour) // past the due date, and the deferral
	dg := mustCall[proto.DigestResult](t, s, alice, proto.OpDigest, proto.DigestArgs{Since: "2026-10-15T09:00:00Z"})
	if len(dg.Overdue) != 1 || dg.Totals.Overdue != 1 || len(dg.Ready) != 1 || dg.Ready[0].ID != is.ID {
		t.Errorf("digest = ready %+v, overdue %+v; want %s in both", dg.Ready, dg.Overdue, is.ID)
	}
	empty := ""
	w := mustCall[proto.WriteResult](t, s, alice, proto.OpUpdate, proto.UpdateArgs{ID: is.ID, Rev: sh.Issue.Rev, Due: &empty, DeferUntil: &empty})
	sh = mustCall[proto.ShowResult](t, s, alice, proto.OpShow, proto.ShowArgs{ID: is.ID})
	if sh.Issue.Rev != w.Rev || sh.Issue.Due != nil || sh.Issue.DeferUntil != nil {
		t.Errorf("show after clearing = due %v, defer %v; want neither", sh.Issue.Due, sh.Issue.DeferUntil)
	}
}
