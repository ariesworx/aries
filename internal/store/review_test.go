package store

import (
	"errors"
	"slices"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
)

// reviewWorkflow is a workflow with a review on features, a review that
// names nobody on bugs, an approval on epics, and nothing on tasks.
func reviewWorkflow(t *testing.T) proto.Workflow {
	t.Helper()
	w, err := proto.ParseWorkflow([]byte(`{"version": 1, "ci": ["ci-example"], "types": {
  "feature": {"review": {"approvers": ["carol"]}},
  "bug": {"review": {"approvers": []}, "checks": ["landed"]},
  "epic": {"approve": {"approvers": ["carol"]}},
  "task": {"acceptance": ["Done"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// An epic created under approve is held by a human gate made in the
// create's write: it and its children stay out of ready and cannot be
// started until an approver passes the gate from the session cli.
func TestApproveHoldsEpicUntilPassed(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	mustSetWorkflow(t, s, reviewWorkflow(t))
	epic := mustCreate(t, s, NewIssue{Title: "plan", Type: TypeEpic})
	child := mustCreate(t, s, NewIssue{Title: "step 1", ParentID: epic.ID})
	gates, err := s.GatesHolding(ctx, child.ID)
	if err != nil || len(gates) != 1 {
		t.Fatalf("GatesHolding(child) = %+v, %v; want the epic's approval gate", gates, err)
	}
	g := gates[0]
	if g.Kind != GateHuman || !slices.Equal(g.Approvers, []string{"carol"}) || !slices.Equal(g.Holds, []IssueID{epic.ID}) || g.Subject != "" {
		t.Errorf("approval gate = %+v; want a human gate for carol holding the epic", g)
	}
	if got := readyIDs(t, s); slices.Contains(got, child.ID) || slices.Contains(got, epic.ID) {
		t.Errorf("Ready = %v; want neither the epic nor its child", got)
	}
	if _, _, err := s.StartIssue(ctx, bob, child.ID, 0, false); !gateErr(err, GateHeld) {
		t.Errorf("StartIssue(child) = %v; want a GateError (held)", err)
	}
	page, err := s.Inbox(ctx, carolCLI, false, 0)
	if err != nil || len(page.Items) != 1 || page.Items[0].Kind != InboxGatePending {
		t.Errorf("carol's inbox = %+v, %v; want one gate.pending", page, err)
	}
	if _, err := s.PassGate(ctx, Actor{Principal: "carol", Session: "sess-c", Machine: "m"}, g.ID, "ok"); !gateErr(err, GateNotPerson) {
		t.Errorf("PassGate from an agent's session = %v; want a GateError (not a person)", err)
	}
	if _, err := s.PassGate(ctx, bobCLI, g.ID, "ok"); !gateErr(err, GateNotApprover) {
		t.Errorf("PassGate by bob = %v; want a GateError (not an approver)", err)
	}
	if _, err := s.PassGate(ctx, carolCLI, g.ID, "plan approved"); err != nil {
		t.Fatalf("PassGate by carol from cli: %v", err)
	}
	if got := readyIDs(t, s); !slices.Contains(got, child.ID) {
		t.Errorf("Ready after the pass = %v; want the child", got)
	}
}

// An issue created under review has a human gate that holds nothing: it
// can be started and worked, and finish is refused, naming the gate and
// its approvers, until an approver passes it from cli (or an admin
// waives it). A profile with no review makes no gate.
func TestReviewRefusesFinishUntilPassed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		decide func(*Store, IssueID) error
	}{
		{"passed by the approver", func(s *Store, g IssueID) error {
			_, err := s.PassGate(t.Context(), carolCLI, g, "reviewed")
			return err
		}},
		{"waived by an admin", func(s *Store, g IssueID) error {
			_, err := s.WaiveGate(t.Context(), danaCLI, g, "no reviewer free")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			ctx := t.Context()
			mustSetWorkflow(t, s, reviewWorkflow(t))
			is := mustCreate(t, s, NewIssue{Title: "design", Type: TypeFeature})
			plain := mustCreate(t, s, NewIssue{Title: "plain", Type: TypeTask})
			if gs, err := s.GatesHolding(ctx, is.ID); err != nil || len(gs) != 0 {
				t.Fatalf("GatesHolding = %+v, %v; want none: a review holds nothing", gs, err)
			}
			if gs, err := s.Gates(ctx, GateFilter{}); err != nil || len(gs) != 1 {
				t.Fatalf("Gates = %+v, %v; want only the review gate", gs, err)
			}
			if _, _, err := s.StartIssue(ctx, alice, is.ID, 0, false); err != nil {
				t.Fatalf("StartIssue under review = %v; want it started", err)
			}
			_, _, err := s.FinishIssue(ctx, alice, is.ID, 0, Finish{})
			var unmet *AcceptanceError
			if !errors.As(err, &unmet) || len(unmet.Reviews) != 1 || !slices.Equal(unmet.Reviews[0].Approvers, []string{"carol"}) ||
				len(unmet.Open) != 0 || len(unmet.Checks) != 0 {
				t.Fatalf("FinishIssue = %v; want an AcceptanceError naming the review alone", err)
			}
			gate := unmet.Reviews[0].Gate
			if _, err := s.CloseIssue(ctx, alice, is.ID, 0, "done"); !errors.As(err, &unmet) || len(unmet.Reviews) != 1 {
				t.Errorf("CloseIssue = %v; want the same refusal", err)
			}
			if _, err := s.PassGate(ctx, bobCLI, gate, ""); !gateErr(err, GateNotApprover) {
				t.Errorf("PassGate by bob = %v; want a GateError (not an approver)", err)
			}
			if err := tc.decide(s, gate); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.FinishIssue(ctx, alice, is.ID, 0, Finish{}); err != nil {
				t.Errorf("FinishIssue after the decision = %v; want it closed", err)
			}
			if gs, err := s.Gates(ctx, GateFilter{All: true}); err != nil || len(gs) != 1 {
				t.Errorf("Gates after = %+v, %v; want the one review gate, closed", gs, err)
			}
			if _, err := s.CloseIssue(ctx, alice, plain.ID, 0, "done"); errors.As(err, &unmet) && len(unmet.Reviews) > 0 {
				t.Errorf("CloseIssue of a task = %v; want no review", err)
			}
		})
	}
}

// A review that names no approver is passed by an admin who did not
// make the issue; a forced close by an admin records it.
func TestReviewWithoutApproversAndForce(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	mustSetWorkflow(t, s, reviewWorkflow(t))
	is := mustCreate(t, s, NewIssue{Title: "defect", Type: TypeBug})
	waits, err := reviewWaits(ctx, s.r, is.ID)
	if err != nil || len(waits) != 1 || len(waits[0].Approvers) != 0 {
		t.Fatalf("reviewWaits = %+v, %v; want one with no approvers named", waits, err)
	}
	forced, err := s.ForceClose(ctx, dana, is.ID, 0, "abandoned")
	if err != nil || forced.Status != StatusClosed {
		t.Fatalf("ForceClose = %+v, %v", forced, err)
	}
	if after := lastAfter(t, s, is.ID, OpIssueClose); after["reviews_overridden"] == nil {
		t.Errorf("the forced close's event = %v; want reviews_overridden", after)
	}
}

// The closing pass of a required check does not close an issue whose
// review is still open, and says so; once the review passes, finish
// closes it.
func TestClosingPassWaitsForReview(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	mustSetWorkflow(t, s, reviewWorkflow(t))
	is := mustCreate(t, s, NewIssue{Title: "defect", Type: TypeBug})
	if _, _, err := s.StartIssue(ctx, alice, is.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PassCheck(ctx, ciJob, landed(is.ID)); err != nil {
		t.Fatalf("report landed: %v", err)
	}
	got, err := s.GetIssue(ctx, is.ID)
	if err != nil || got.Status == StatusClosed {
		t.Fatalf("issue after landed = %+v, %v; want it open: its review is not passed", got, err)
	}
	waits, err := reviewWaits(ctx, s.r, is.ID)
	if err != nil || len(waits) != 1 {
		t.Fatalf("reviewWaits = %+v, %v", waits, err)
	}
	if _, err := s.PassGate(ctx, danaCLI, waits[0].Gate, "reviewed"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.FinishIssue(ctx, alice, is.ID, 0, Finish{}); err != nil {
		t.Errorf("FinishIssue after landed and the review = %v; want it closed", err)
	}
}
