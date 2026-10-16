package store

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
)

// People at their own terminals: the session a gate is passed from.
var (
	carolCLI = Actor{Principal: "carol", Session: proto.CLISession, Machine: "laptop-c"}
	danaCLI  = Actor{Principal: "dana", Session: proto.CLISession, Machine: "laptop-d"}
	bobCLI   = Actor{Principal: "bob", Session: proto.CLISession, Machine: "laptop-b"}
	aliceCLI = Actor{Principal: "alice", Session: proto.CLISession, Machine: "laptop-a"}
)

func mustGate(t *testing.T, s *Store, by Actor, in NewGate) Gate {
	t.Helper()
	g, err := s.AddGate(t.Context(), by, in)
	if err != nil {
		t.Fatalf("AddGate(%+v) by %s: %v", in, by.Principal, err)
	}
	return g
}

// gateErr reports whether err is a *GateError with reason r.
func gateErr(err error, r GateReason) bool {
	ge, ok := errors.AsType[*GateError](err)
	return ok && ge.Reason == r
}

// A gate on an epic holds its whole plan: neither the epic, its child,
// a task waiting on the child, nor the gate itself is ready, and none
// can be started, until an approver passes the gate; then all of them
// are ready but the epic, which waits on its open child.
func TestGateHoldsThePlanUntilPassed(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	epic := mustCreate(t, s, NewIssue{Title: "plan", Type: TypeEpic})
	child := mustCreate(t, s, NewIssue{Title: "step 1", ParentID: epic.ID})
	after := mustCreate(t, s, NewIssue{Title: "step 2"})
	if err := s.AddDep(ctx, alice, after.ID, child.ID, DepBlocks, ""); err != nil {
		t.Fatal(err)
	}
	other := mustCreate(t, s, NewIssue{Title: "unrelated"})
	g := mustGate(t, s, alice, NewGate{Blocks: epic.ID, Title: "approve the plan", Approvers: []string{"carol"}})
	if g.Kind != GateHuman || !slices.Equal(g.Approvers, []string{"carol"}) || g.Outcome != "" || !slices.Equal(g.Holds, []IssueID{epic.ID}) {
		t.Fatalf("AddGate = %+v, want an open human gate for carol holding %s", g, epic.ID)
	}
	if got := readyIDs(t, s); !slices.Equal(got, []IssueID{other.ID}) {
		t.Errorf("Ready behind the gate = %v, want only %s", got, other.ID)
	}
	for _, id := range []IssueID{epic.ID, child.ID, after.ID} {
		if _, _, err := s.StartIssue(ctx, bob, id, 0, false); !gateErr(err, GateHeld) {
			t.Errorf("StartIssue(%s) behind the gate = %v, want a GateError (held)", id, err)
		}
	}
	if _, _, err := s.StartIssue(ctx, bob, g.ID, 0, false); !gateErr(err, GateIsGate) {
		t.Errorf("StartIssue(the gate) = %v, want a GateError (is a gate)", err)
	}
	passed, err := s.PassGate(ctx, carolCLI, g.ID, "looks right")
	if err != nil {
		t.Fatalf("PassGate by carol from cli: %v", err)
	}
	if passed.Outcome != GatePassed || passed.By != "carol" || passed.Reason != "looks right" {
		t.Errorf("PassGate = %+v, want passed by carol with the reason", passed)
	}
	if is, _ := s.GetIssue(ctx, g.ID); is.Status != StatusClosed {
		t.Errorf("gate after pass is %s, want closed", is.Status)
	}
	got := readyIDs(t, s)
	for _, id := range []IssueID{child.ID, other.ID} {
		if !slices.Contains(got, id) {
			t.Errorf("Ready after the pass = %v, want %s in it", got, id)
		}
	}
	if slices.Contains(got, epic.ID) {
		t.Errorf("Ready after the pass = %v, want the epic %s left out while its child is open", got, epic.ID)
	}
	if _, _, err := s.StartIssue(ctx, bob, child.ID, 0, false); err != nil {
		t.Errorf("StartIssue(child) after the pass: %v", err)
	}
}

// Who passes a human gate (decision 68): the principals it names, or the
// project's admins when it names none; only from the session cli; its
// creator only when named.
func TestGatePassWho(t *testing.T) {
	t.Parallel()
	s := openStore(t, newDSN(t), Options{Admins: []string{"dana", "erin"}})
	erinCLI := Actor{Principal: "erin", Session: proto.CLISession, Machine: "laptop-e"}
	tests := []struct {
		name      string
		creator   Actor
		approvers []string
		by        Actor
		want      GateReason // "" passes
	}{
		{"a named approver", alice, []string{"carol"}, carolCLI, ""},
		{"a named approver from an agent's session", alice, []string{"carol"}, carol, GateNotPerson},
		{"someone not named", alice, []string{"carol"}, bobCLI, GateNotApprover},
		{"an admin not named", alice, []string{"carol"}, danaCLI, GateNotApprover},
		{"the creator not named", alice, []string{"carol"}, aliceCLI, GateNotApprover},
		{"the creator when named", alice, []string{"alice"}, aliceCLI, ""},
		{"an admin when none named", alice, nil, danaCLI, ""},
		{"an admin from an agent's session", alice, nil, Actor{Principal: "dana", Session: "s1", Machine: "m"}, GateNotPerson},
		{"a member when none named", alice, nil, bobCLI, GateNotApprover},
		{"the admin who made it, none named", dana, nil, danaCLI, GateNotApprover},
		{"another admin than its maker, none named", dana, nil, erinCLI, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			is := mustCreate(t, s, NewIssue{Title: tc.name})
			g := mustGate(t, s, tc.creator, NewGate{Blocks: is.ID, Approvers: tc.approvers})
			_, err := s.PassGate(t.Context(), tc.by, g.ID, "")
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("PassGate by %s/%s = %v, want it passed", tc.by.Principal, tc.by.Session, err)
			case tc.want != "" && !gateErr(err, tc.want):
				t.Errorf("PassGate by %s/%s = %v, want a GateError (%s)", tc.by.Principal, tc.by.Session, err, tc.want)
			case tc.want != "":
				if !errors.Is(err, ErrForbidden) {
					t.Errorf("PassGate by %s/%s = %v, want it to wrap ErrForbidden", tc.by.Principal, tc.by.Session, err)
				}
				ge, _ := errors.AsType[*GateError](err)
				if !slices.Equal(ge.Approvers, tc.approvers) {
					t.Errorf("refusal names approvers %v, want %v", ge.Approvers, tc.approvers)
				}
			}
		})
	}
}

// Nothing but the gate ops opens or closes a gate, and nothing undoes a
// hold for someone who could not pass the gate: an update of the held
// issue's status, removing its edge to the gate, moving it out from under
// a gated parent, and closing or finishing what the gate holds are
// refused, as is making an issue a gate, or a gate something else,
// without the gate ops.
func TestGateGuards(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	epic := mustCreate(t, s, NewIssue{Title: "plan", Type: TypeEpic})
	child := mustCreate(t, s, NewIssue{Title: "step", ParentID: epic.ID})
	free := mustCreate(t, s, NewIssue{Title: "elsewhere"})
	g := mustGate(t, s, alice, NewGate{Blocks: epic.ID, Approvers: []string{"carol"}})
	gi, err := s.GetIssue(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	refused := []struct {
		name string
		do   func() error
		want GateReason
	}{
		{"close the gate", func() error { _, err := s.CloseIssue(ctx, bob, g.ID, 0, ""); return err }, GateIsGate},
		{"force-close the gate", func() error { _, err := s.ForceClose(ctx, dana, g.ID, 0, ""); return err }, GateIsGate},
		{"finish the gate", func() error { _, _, err := s.FinishIssue(ctx, bob, g.ID, 0, Finish{}); return err }, GateIsGate},
		{"set the gate's status", func() error {
			_, err := s.UpdateIssue(ctx, bob, g.ID, gi.Rev, IssuePatch{Status: ptr(StatusDeferred)})
			return err
		}, GateIsGate},
		{"make the gate a task", func() error {
			_, err := s.UpdateIssue(ctx, bob, g.ID, gi.Rev, IssuePatch{Type: ptr(TypeTask)})
			return err
		}, GateIsGate},
		{"make a task a gate", func() error {
			_, err := s.UpdateIssue(ctx, bob, free.ID, free.Rev, IssuePatch{Type: ptr(TypeGate)})
			return err
		}, GateIsGate},
		{"create a gate without gate add", func() error {
			_, err := s.CreateIssue(ctx, bob, NewIssue{Title: "sneaky", Type: TypeGate})
			return err
		}, GateIsGate},
		{"remove the edge to the gate", func() error { return s.RemoveDep(ctx, bob, epic.ID, g.ID, DepBlocks, "") }, GateHeld},
		{"move the child out from under the epic", func() error {
			_, err := s.UpdateIssue(ctx, bob, child.ID, child.Rev, IssuePatch{ParentID: ptr(IssueID(""))})
			return err
		}, GateHeld},
		{"close the epic", func() error { _, err := s.CloseIssue(ctx, bob, epic.ID, 0, ""); return err }, GateHeld},
		{"close the child", func() error { _, err := s.CloseIssue(ctx, bob, child.ID, 0, ""); return err }, GateHeld},
		{"finish the child", func() error { _, _, err := s.FinishIssue(ctx, bob, child.ID, 0, Finish{}); return err }, GateHeld},
		{"an approver's agent closes the epic", func() error { _, err := s.CloseIssue(ctx, carol, epic.ID, 0, ""); return err }, GateHeld},
	}
	for _, tc := range refused {
		if err := tc.do(); !gateErr(err, tc.want) {
			t.Errorf("%s = %v, want a GateError (%s)", tc.name, err, tc.want)
		}
	}
	// A status change of a held issue is allowed, and holds nothing back:
	// the gate holds through the graph, not the status.
	if _, err := s.UpdateIssue(ctx, bob, child.ID, child.Rev, IssuePatch{Status: ptr(StatusBlocked)}); err != nil {
		t.Fatal(err)
	}
	child, _ = s.GetIssue(ctx, child.ID)
	if _, err := s.UpdateIssue(ctx, bob, child.ID, child.Rev, IssuePatch{Status: ptr(StatusOpen)}); err != nil {
		t.Fatalf("UpdateIssue(child open) = %v, want it allowed", err)
	}
	if got := readyIDs(t, s); slices.Contains(got, child.ID) {
		t.Errorf("Ready after setting the held child open = %v, want it still held", got)
	}
	// An approver at their terminal may remove the edge: they could pass.
	if err := s.RemoveDep(ctx, carolCLI, epic.ID, g.ID, DepBlocks, ""); err != nil {
		t.Errorf("RemoveDep by carol from cli = %v, want it allowed", err)
	}
	// And the gate is still open: only its ops close it.
	if is, _ := s.GetIssue(ctx, g.ID); is.Status != StatusOpen {
		t.Errorf("gate is %s, want open", is.Status)
	}
}

// Waive is an admin's pass, with a reason; rearm, by those the gate
// names or an admin, reopens a gate and holds its issues again.
func TestGateWaiveAndRearm(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "deploy"})
	g := mustGate(t, s, alice, NewGate{Blocks: is.ID, Approvers: []string{"carol"}})
	if _, err := s.WaiveGate(ctx, bobCLI, g.ID, "urgent"); !gateErr(err, GateNotAdmin) {
		t.Errorf("WaiveGate by bob = %v, want a GateError (admins only)", err)
	}
	if _, err := s.WaiveGate(ctx, danaCLI, g.ID, ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("WaiveGate with no reason = %v, want ErrInvalid", err)
	}
	if _, err := s.WaiveGate(ctx, dana, g.ID, "urgent"); !gateErr(err, GateNotPerson) {
		t.Errorf("WaiveGate by dana from an agent's session = %v, want a GateError (not a person)", err)
	}
	w, err := s.WaiveGate(ctx, danaCLI, g.ID, "urgent")
	if err != nil || w.Outcome != GateWaived || w.By != "dana" {
		t.Fatalf("WaiveGate by dana = %+v, %v, want waived by dana", w, err)
	}
	if _, err := s.PassGate(ctx, carolCLI, g.ID, ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("PassGate of a closed gate = %v, want ErrInvalid", err)
	}
	if got := readyIDs(t, s); !slices.Contains(got, is.ID) {
		t.Errorf("Ready after the waive = %v, want %s", got, is.ID)
	}
	if _, err := s.RearmGate(ctx, bobCLI, g.ID, ""); !gateErr(err, GateNotApprover) {
		t.Errorf("RearmGate by bob = %v, want a GateError (not an approver)", err)
	}
	r, err := s.RearmGate(ctx, carolCLI, g.ID, "a revert")
	if err != nil || r.Outcome != "" {
		t.Fatalf("RearmGate by carol = %+v, %v, want an open gate", r, err)
	}
	if got := readyIDs(t, s); slices.Contains(got, is.ID) {
		t.Errorf("Ready after the rearm = %v, want %s held again", got, is.ID)
	}
	if _, err := s.ReopenIssue(ctx, danaCLI, g.ID, 0); !gateErr(err, GateIsGate) {
		t.Errorf("ReopenIssue of a gate = %v, want a GateError (is a gate)", err)
	}
	evs, err := s.History(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ops []Op
	for _, e := range evs {
		ops = append(ops, e.Op)
	}
	want := []Op{OpIssueCreate, OpGateAdd, OpGateWaive, OpIssueClose, OpGateRearm, OpIssueReopen}
	if !slices.Equal(ops, want) {
		t.Errorf("gate history = %v, want %v", ops, want)
	}
}

// The approvers hear of a new gate, and the assignees of what it held
// hear that it passed.
func TestGateInbox(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "release", Assignee: "bob"})
	g := mustGate(t, s, alice, NewGate{Blocks: is.ID, Approvers: []string{"carol"}})
	page, err := s.Inbox(ctx, carol, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) == 0 || page.Items[0].Kind != InboxGatePending || page.Items[0].Issue != g.ID {
		t.Errorf("carol's inbox after the gate = %+v, want a gate.pending item for %s", page.Items, g.ID)
	}
	if _, err := s.PassGate(ctx, carolCLI, g.ID, ""); err != nil {
		t.Fatal(err)
	}
	page, err = s.Inbox(ctx, bob, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) == 0 || page.Items[0].Kind != InboxGatePassed || page.Items[0].Issue != is.ID {
		t.Errorf("bob's inbox after the pass = %+v, want a gate.passed item for %s", page.Items, is.ID)
	}
}

// Gates and what they hold are read back, open ones by default.
func TestGatesList(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	a := mustCreate(t, s, NewIssue{Title: "a"})
	b := mustCreate(t, s, NewIssue{Title: "b"})
	ga := mustGate(t, s, alice, NewGate{Blocks: a.ID, Approvers: []string{"carol"}})
	gb := mustGate(t, s, alice, NewGate{Blocks: b.ID})
	if _, err := s.PassGate(ctx, danaCLI, gb.ID, ""); err != nil {
		t.Fatal(err)
	}
	open, err := s.Gates(ctx, GateFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].ID != ga.ID || !slices.Equal(open[0].Holds, []IssueID{a.ID}) {
		t.Errorf("Gates() = %+v, want only %s holding %s", open, ga.ID, a.ID)
	}
	all, err := s.Gates(ctx, GateFilter{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("Gates(all) = %+v, want both", all)
	}
	holding, err := s.GatesHolding(ctx, a.ID)
	if err != nil || len(holding) != 1 || holding[0].ID != ga.ID {
		t.Errorf("GatesHolding(%s) = %+v, %v, want %s", a.ID, holding, err, ga.ID)
	}
	if g, err := s.GateOf(ctx, a.ID); err != nil || g != nil {
		t.Errorf("GateOf(a task) = %+v, %v, want nil", g, err)
	}
}

// Approvers per gate and open gates in a project are capped (rule 18).
func TestGateLimits(t *testing.T) {
	t.Parallel()
	s := openStore(t, newDSN(t), Options{Limits: Limits{GateApprovers: 2, OpenGates: 2}})
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "x"})
	tests := []struct {
		name string
		in   NewGate
		want error
	}{
		{"approvers at the cap", NewGate{Blocks: is.ID, Approvers: []string{"carol", "bob"}}, nil},
		{"approvers past the cap", NewGate{Blocks: is.ID, Approvers: []string{"carol", "bob", "erin"}}, ErrInvalid},
		{"a reserved approver", NewGate{Blocks: is.ID, Approvers: []string{"import"}}, ErrInvalid},
		{"an approver that is no principal name", NewGate{Blocks: is.ID, Approvers: []string{"Not A Name"}}, ErrInvalid},
		{"no issue to hold", NewGate{Approvers: []string{"carol"}}, ErrInvalid},
		{"an issue that does not exist", NewGate{Blocks: "tst-zzzz"}, ErrNotFound},
		{"the second open gate", NewGate{Blocks: is.ID}, nil},
		{"open gates past the cap", NewGate{Blocks: is.ID}, ErrBusy},
	}
	for _, tc := range tests {
		_, err := s.AddGate(ctx, alice, tc.in)
		if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("%s: AddGate = %v, want %v", tc.name, err, tc.want)
		}
	}
}

// pendingFor returns principal p's unread gate.pending items, newest
// first.
func pendingFor(t *testing.T, s *Store, p string) []InboxItem {
	t.Helper()
	return inboxOf(t, s, Actor{Principal: p, Session: "sess-" + p, Machine: "m"}, InboxGatePending)
}

// A gate that names no approvers waits for the project's admins, and
// tells them so (decision 68): when it is added, and when more work is
// put behind it. Its creator, who may not pass it, is not told, and no
// more admins are told than a gate may name (approvers_per_gate). A gate
// that names its approvers tells them when more work is put behind it
// too.
func TestGatePendingTellsWhoPasses(t *testing.T) {
	t.Parallel()
	admins := []string{"erin", "frank", "alice", "gail"}
	s := openStore(t, newDSN(t), Options{Admins: []string{"dana"}, Limits: Limits{GateApprovers: 3},
		IsAdmin:    func(p string) bool { return slices.Contains(admins, p) },
		ListAdmins: func() []string { return admins }})
	ctx := t.Context()
	release := mustCreate(t, s, NewIssue{Title: "release"})
	g := mustGate(t, s, alice, NewGate{Blocks: release.ID})
	told := func() map[string]int {
		out := map[string]int{}
		for _, p := range []string{"alice", "dana", "erin", "frank", "gail"} {
			for _, it := range pendingFor(t, s, p) {
				if it.Issue == g.ID {
					out[p]++
				}
			}
		}
		return out
	}
	// The project's admins in the order listed, then the configured ones,
	// past the creator, three in all.
	if got, want := told(), map[string]int{"erin": 1, "frank": 1, "gail": 1}; !maps.Equal(got, want) {
		t.Errorf("gate.pending items for a gate with no approvers = %v, want %v", got, want)
	}
	notes := mustCreate(t, s, NewIssue{Title: "release notes"})
	if err := s.AddDep(ctx, bob, notes.ID, g.ID, DepBlocks, ""); err != nil {
		t.Fatal(err)
	}
	if got, want := told(), map[string]int{"erin": 2, "frank": 2, "gail": 2}; !maps.Equal(got, want) {
		t.Errorf("gate.pending items once more work is behind it = %v, want %v", got, want)
	}
	if items := pendingFor(t, s, "erin"); len(items) == 0 || !strings.Contains(items[0].Body, string(notes.ID)) {
		t.Errorf("erin's latest gate.pending = %+v, want one naming %s", items, notes.ID)
	}
	// An edge that is there already tells nobody again.
	if err := s.AddDep(ctx, bob, notes.ID, g.ID, DepBlocks, ""); err != nil {
		t.Fatal(err)
	}
	if got := told()["erin"]; got != 2 {
		t.Errorf("gate.pending items for erin after the same edge again = %d, want 2", got)
	}

	named := mustGate(t, s, alice, NewGate{Blocks: release.ID, Approvers: []string{"carol"}})
	later := mustCreate(t, s, NewIssue{Title: "announce"})
	if err := s.AddDep(ctx, bob, later.ID, named.ID, DepBlocks, ""); err != nil {
		t.Fatal(err)
	}
	if got := len(pendingFor(t, s, "carol")); got != 2 {
		t.Errorf("carol's gate.pending items = %d, want 2: the add, and the work put behind it", got)
	}
	if got := len(pendingFor(t, s, "erin")); got != 2 {
		t.Errorf("erin's gate.pending items = %d, want 2: a gate that names carol is not the admins'", got)
	}
}
