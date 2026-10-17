package store

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// landedWorkflow requires landed of every type but chore, reported by
// ci-example, with one item bound to it; a chore requires nothing.
func landedWorkflow(t *testing.T) proto.Workflow {
	t.Helper()
	w, err := proto.ParseWorkflow([]byte(`{"version": 1, "ci": ["ci-example"], "types": {
  "default": {
    "acceptance": ["PR opened", {"text": "Merged", "check": "landed"}],
    "checks": ["landed"]},
  "chore": {"acceptance": ["Posted"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// landed is ci-example's report of landed on id.
func landed(id IssueID) Report {
	return Report{Subject: id, Check: "landed", SHA: "0123abcd0123abcd0123abcd0123abcd0123abcd"}
}

// startedUnder sets the landed workflow, creates an issue under it and
// starts it as alice.
func startedUnder(t *testing.T, s *Store, title string) Issue {
	t.Helper()
	mustSetWorkflow(t, s, landedWorkflow(t))
	is := mustCreate(t, s, NewIssue{Title: title})
	if _, _, err := s.StartIssue(t.Context(), alice, is.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	return is
}

// historyOps lists the ops of id's events, oldest first.
func historyOps(t *testing.T, s *Store, id IssueID) []Op {
	t.Helper()
	evs, err := s.History(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	var ops []Op
	for _, e := range evs {
		ops = append(ops, e.Op)
	}
	return ops
}

// lastAfter decodes the after state of id's last event of op.
func lastAfter(t *testing.T, s *Store, id IssueID, op Op) map[string]any {
	t.Helper()
	evs, err := s.History(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range slices.Backward(evs) {
		if e.Op == op {
			var m map[string]any
			if err := json.Unmarshal(e.After, &m); err != nil {
				t.Fatal(err)
			}
			return m
		}
	}
	t.Fatalf("no %s event on %s", op, id)
	return nil
}

// An issue whose profile requires landed is refused at finish and close,
// with every item ticked, by an AcceptanceError naming the check and
// wrapping ErrInvalid; an admin's force closes it on the record. An issue
// made before the workflow, or of a type that requires nothing, is not.
func TestRequiredCheckRefusesClose(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	before := mustCreate(t, s, NewIssue{Title: "made before the workflow"})
	is := startedUnder(t, s, "needs landing")
	all := Acceptance{Tick: []int{1, 2}}
	_, _, err := s.FinishIssue(ctx, alice, is.ID, 0, Finish{Accept: all})
	var unmet *AcceptanceError
	if !errors.As(err, &unmet) || len(unmet.Open) != 0 || len(unmet.Checks) != 1 || unmet.Checks[0].Check != "landed" ||
		!errors.Is(err, ErrInvalid) {
		t.Fatalf("FinishIssue with every item ticked = %v; want an AcceptanceError naming check landed alone", err)
	}
	if !strings.Contains(err.Error(), "landed") {
		t.Errorf("the refusal %q does not name the check", err)
	}
	if got := itemStates(t, s, is.ID); got[0] != ItemOpen {
		t.Errorf("items after the refused finish = %v; want the finish's ticks undone with it", got)
	}
	if _, err := s.CloseIssue(ctx, alice, is.ID, 0, "done"); !errors.As(err, &unmet) || len(unmet.Checks) != 1 {
		t.Errorf("CloseIssue = %v; want an AcceptanceError naming the check", err)
	}

	// Waiting with items open names both.
	_, _, err = s.FinishIssue(ctx, alice, is.ID, 0, Finish{})
	if !errors.As(err, &unmet) || !slices.Equal(unmet.Open, []int{1, 2}) || len(unmet.Checks) != 1 {
		t.Errorf("FinishIssue with items open = %v; want items 1, 2 and the check", err)
	}

	forced, err := s.ForceClose(ctx, dana, is.ID, 0, "abandoned")
	if err != nil || forced.Status != StatusClosed {
		t.Fatalf("ForceClose by an admin = %+v, %v; want it closed", forced, err)
	}
	if after := lastAfter(t, s, is.ID, OpIssueClose); after["checks_overridden"] == nil {
		t.Errorf("the forced close's event = %v; want checks_overridden", after)
	}

	if _, err := s.CloseIssue(ctx, alice, before.ID, 0, "done"); err != nil {
		t.Errorf("CloseIssue of an issue made before the workflow = %v; want it closed", err)
	}
	chore := mustCreate(t, s, NewIssue{Title: "a run", Type: TypeChore})
	if _, err := s.Accept(ctx, alice, chore.ID, Acceptance{Tick: []int{1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseIssue(ctx, alice, chore.ID, 0, "posted"); err != nil {
		t.Errorf("CloseIssue of a chore = %v; want it closed", err)
	}
}

// itemStates lists the states of id's items.
func itemStates(t *testing.T, s *Store, id IssueID) []ItemState {
	t.Helper()
	items, err := s.AcceptanceItems(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []ItemState
	for _, it := range items {
		out = append(out, it.State)
	}
	return out
}

// The first report of a required check makes its ci gate in the report's
// write, reported by the workflow's ci principals: gate.add before the
// report. A principal the workflow does not name is refused, and no gate
// is left behind; a check the profile does not require still needs a
// gate someone added.
func TestReportMakesRequiredGate(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	is := startedUnder(t, s, "gate on first report")
	if _, err := s.FailCheck(ctx, ciOther, landed(is.ID)); !gateErr(err, GateNotApprover) || !errors.Is(err, ErrForbidden) {
		t.Fatalf("FailCheck by ci-other = %v; want a GateError (not approver)", err)
	}
	if got, err := s.ChecksOf(ctx, is.ID); err != nil || len(got) != 0 {
		t.Fatalf("ChecksOf after the refused report = %+v, %v; want none", got, err)
	}
	if _, err := s.FailCheck(ctx, ciJob, Report{Subject: is.ID, Check: "build"}); !gateErr(err, GateNoCheck) {
		t.Errorf("FailCheck of build, which is not required = %v; want a GateError (no check)", err)
	}
	g, err := s.FailCheck(ctx, ciJob, landed(is.ID))
	if err != nil {
		t.Fatalf("FailCheck by ci-example: %v", err)
	}
	if g.Kind != GateCI || g.Check != "landed" || g.Subject != is.ID || !slices.Equal(g.Approvers, []string{"ci-example"}) ||
		g.Report == nil || g.Report.Result != CheckFailed {
		t.Errorf("the gate the report made = %+v; want a ci gate of landed on %s by ci-example, failed", g, is.ID)
	}
	if got, want := historyOps(t, s, g.ID), []Op{OpIssueCreate, OpGateAdd, OpGateFail}; !slices.Equal(got, want) {
		t.Errorf("the gate's history = %v; want %v", got, want)
	}
	if again, err := s.FailCheck(ctx, ciJob, landed(is.ID)); err != nil || again.ID != g.ID {
		t.Errorf("a second report = %+v, %v; want the same gate %s", again, err, g.ID)
	}
	// An issue made before the workflow requires nothing: no gate.
	plain := mustCreate(t, s, NewIssue{Title: "plain", Type: TypeChore})
	if _, err := s.PassCheck(ctx, ciJob, landed(plain.ID)); !gateErr(err, GateNoCheck) {
		t.Errorf("PassCheck of landed on a chore = %v; want a GateError (no check)", err)
	}
}

// The passing report ticks the item bound to it. With another item open,
// nothing closes and the assignee is told what is left; whoever then
// finishes is not refused. With none open, the pass closes the issue
// (reason landed, by the reporter), ends the claim and tells the assignee
// and the last handoff's author.
func TestClosingPass(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	is := startedUnder(t, s, "item left")
	if _, err := s.PassCheck(ctx, ciJob, landed(is.ID)); err != nil {
		t.Fatalf("PassCheck: %v", err)
	}
	if got := itemStates(t, s, is.ID); !slices.Equal(got, []ItemState{ItemOpen, ItemTicked}) {
		t.Errorf("items after the pass = %v; want the bound item ticked alone", got)
	}
	if got := mustShow(t, s, is.ID); got.Status != StatusInProgress {
		t.Errorf("status with an item open = %s; want in_progress", got.Status)
	}
	if got := inboxOf(t, s, alice, InboxGatePassed); len(got) != 1 || !strings.Contains(got[0].Body, "1") {
		t.Errorf("alice's gate.passed = %+v; want one saying item 1 is left", got)
	}
	if _, _, err := s.FinishIssue(ctx, alice, is.ID, 0, Finish{Accept: Acceptance{Tick: []int{1}}}); err != nil {
		t.Errorf("FinishIssue once landed passed = %v; want it closed", err)
	}

	mustSetWorkflow(t, s, landedWorkflow(t))
	done := mustCreate(t, s, NewIssue{Title: "all ticked"})
	if _, _, err := s.StartIssue(ctx, alice, done.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Accept(ctx, alice, done.ID, Acceptance{Tick: []int{1, 2}}); err != nil {
		t.Fatal(err)
	}
	// The last handoff is an admin's, dana's, on alice's work.
	if _, err := s.HandoffIssue(ctx, dana, done.ID, 0, HandoffNote{Note: "https://example.com/pr/2",
		HandoffFields: HandoffFields{State: HandoffDone}}, false, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PassCheck(ctx, ciJob, landed(done.ID)); err != nil {
		t.Fatalf("PassCheck: %v", err)
	}
	got := mustShow(t, s, done.ID)
	if got.Status != StatusClosed || got.CloseReason != "landed" {
		t.Errorf("after the closing pass = %s (%q); want closed, landed", got.Status, got.CloseReason)
	}
	evs, err := s.History(ctx, done.ID)
	if err != nil {
		t.Fatal(err)
	}
	if e := evs[len(evs)-1]; e.Op != OpIssueClose || e.Actor.Principal != "ci-example" {
		t.Errorf("the last event = %s by %s; want issue.close by ci-example", e.Op, e.Actor.Principal)
	}
	if c, err := s.ClaimOf(ctx, done.ID); err != nil || c != nil {
		t.Errorf("claim after the closing pass = %+v, %v; want none", c, err)
	}
	for _, a := range []Actor{alice, dana} {
		passed := inboxOf(t, s, a, InboxGatePassed)
		if !slices.ContainsFunc(passed, func(it InboxItem) bool { return it.Issue == done.ID && strings.Contains(it.Body, "closed") }) {
			t.Errorf("%s's gate.passed = %+v; want one saying %s closed", a.Principal, passed, done.ID)
		}
	}
}

// An agent may tick an item bound to a check itself; the check is still
// required.
func TestAgentTicksBoundItem(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	is := startedUnder(t, s, "ticked by hand")
	if _, err := s.Accept(ctx, alice, is.ID, Acceptance{Tick: []int{1, 2}}); err != nil {
		t.Fatalf("Accept of the bound item by alice = %v; want it ticked", err)
	}
	var unmet *AcceptanceError
	if _, _, err := s.FinishIssue(ctx, alice, is.ID, 0, Finish{}); !errors.As(err, &unmet) || len(unmet.Checks) != 1 {
		t.Errorf("FinishIssue = %v; want the check still required", err)
	}
}

// A failure after the closing pass reopens the issue (reason check
// failed, by the reporter), unticks the bound item, and tells the
// assignee; the next pass closes it again.
func TestFailureAfterLandedReopens(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	is := startedUnder(t, s, "reverted")
	if _, err := s.Accept(ctx, alice, is.ID, Acceptance{Tick: []int{1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PassCheck(ctx, ciJob, landed(is.ID)); err != nil {
		t.Fatal(err)
	}
	if got := mustShow(t, s, is.ID); got.Status != StatusClosed {
		t.Fatalf("after the pass = %s; want closed", got.Status)
	}
	r := landed(is.ID)
	r.SHA, r.Reason = "4567abcd4567abcd4567abcd4567abcd4567abcd", "reverted"
	if _, err := s.FailCheck(ctx, ciJob, r); err != nil {
		t.Fatalf("FailCheck after the pass: %v", err)
	}
	if got := mustShow(t, s, is.ID); got.Status != StatusOpen {
		t.Errorf("after the failure = %s; want open", got.Status)
	}
	if after := lastAfter(t, s, is.ID, OpIssueReopen); after["reason"] != "check failed" {
		t.Errorf("the reopen's event = %v; want reason check failed", after)
	}
	if got := itemStates(t, s, is.ID); !slices.Equal(got, []ItemState{ItemTicked, ItemOpen}) {
		t.Errorf("items after the failure = %v; want the bound item unticked", got)
	}
	if got := inboxOf(t, s, alice, InboxGateFailed); len(got) != 1 || got[0].Issue != is.ID {
		t.Errorf("alice's gate.failed = %+v; want one on %s", got, is.ID)
	}
	if _, err := s.PassCheck(ctx, ciJob, landed(is.ID)); err != nil {
		t.Fatal(err)
	}
	if got := mustShow(t, s, is.ID); got.Status != StatusClosed || got.CloseReason != "landed" {
		t.Errorf("after the next pass = %s (%q); want closed, landed", got.Status, got.CloseReason)
	}
}

// Digest and Dates list an issue in progress whose last handoff is done
// and whose required check has not passed as awaiting landing, with its
// checks; an issue that landed leaves the list.
func TestAwaitingLanding(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	is := startedUnder(t, s, "waiting to land")
	other := mustCreate(t, s, NewIssue{Title: "not handed off"})
	if _, _, err := s.StartIssue(ctx, alice, other.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Accept(ctx, alice, is.ID, Acceptance{Tick: []int{1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HandoffIssue(ctx, alice, is.ID, 0, HandoffNote{Note: "https://example.com/pr/3",
		HandoffFields: HandoffFields{State: HandoffDone}}, false, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FailCheck(ctx, ciJob, landed(is.ID)); err != nil {
		t.Fatal(err)
	}
	d, err := s.Dates(ctx, DatesFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Landing.Total != 1 || len(d.Landing.Items) != 1 || d.Landing.Items[0].ID != is.ID ||
		d.Landing.Items[0].Note != "landed (failed at 0123abcd0123)" {
		t.Fatalf("Dates().Landing = %+v; want %s alone, its landed failed", d.Landing, is.ID)
	}
	dg, err := s.Digest(ctx, DigestFilter{Window: 24 * time.Hour})
	if err != nil || dg.Landing.Total != 1 {
		t.Fatalf("Digest().Landing = %+v, %v; want %s", dg.Landing, err, is.ID)
	}
	if _, err := s.PassCheck(ctx, ciJob, landed(is.ID)); err != nil {
		t.Fatal(err)
	}
	if d, err = s.Dates(ctx, DatesFilter{}); err != nil || d.Landing.Total != 0 {
		t.Errorf("Dates().Landing once landed = %+v, %v; want none", d.Landing, err)
	}
}
