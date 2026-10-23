package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
)

// closeWaitsWorkflow requires landed of every type, with one item bound
// to it, reported by ci-example or dana; a chore requires build too, and
// a feature a review by carol.
func closeWaitsWorkflow(t *testing.T) proto.Workflow {
	t.Helper()
	items := `"acceptance": ["PR opened", {"text": "Merged", "check": "landed"}]`
	w, err := proto.ParseWorkflow([]byte(`{"version": 1, "ci": ["ci-example", "dana"], "types": {
  "default": {` + items + `, "checks": ["landed"]},
  "chore": {` + items + `, "checks": ["landed", "build"]},
  "feature": {` + items + `, "checks": ["landed"], "review": {"approvers": ["carol"]}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// closeVerdict says what a close or finish returned: "closed", or the
// waits that refused it.
func closeVerdict(err error) string {
	if err == nil {
		return "closed"
	}
	if gateErr(err, GateHeld) {
		return "held by a gate"
	}
	ae, ok := errors.AsType[*AcceptanceError](err)
	if !ok {
		return err.Error()
	}
	var parts []string
	if len(ae.Open) > 0 {
		parts = append(parts, fmt.Sprintf("items %v", ae.Open))
	}
	if len(ae.Checks) > 0 {
		var names []string
		for _, c := range ae.Checks {
			names = append(names, c.Check)
		}
		parts = append(parts, fmt.Sprintf("checks %v", names))
	}
	for _, r := range ae.Reviews {
		parts = append(parts, fmt.Sprintf("review by %v", r.Approvers))
	}
	return strings.Join(parts, "; ")
}

// Close, finish and the closing pass wait for the same things
// (closeWaits): a gate holding the issue, an open acceptance item, a
// required check not passed and an open review. Each row's issue is
// made three times. One is closed and one finished by the row's closer
// once landed no longer waits, its gate waived and its item ticked by
// hand; on the third the row's reporter passes landed, and the closing
// pass closes it or not. The one difference is a gate the closer may
// release: a close releases it, and the closing pass, which is nobody's
// act in person, never does.
func TestCloseWaits(t *testing.T) {
	t.Parallel()
	tickFirst := func(t *testing.T, s *Store, is Issue) {
		t.Helper()
		if _, err := s.Accept(t.Context(), alice, is.ID, Acceptance{Tick: []int{1}}); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name     string
		typ      IssueType
		prep     func(t *testing.T, s *Store, is Issue)
		closer   Actor // closes, finishes and reports landed
		want     string
		passDone bool // whether the closing pass closes it
	}{
		{name: "nothing waits", prep: tickFirst, closer: alice, want: "closed", passDone: true},
		{name: "an open item", prep: func(*testing.T, *Store, Issue) {}, closer: alice, want: "items [1]"},
		{name: "a waived item", prep: func(t *testing.T, s *Store, is Issue) {
			t.Helper()
			if _, err := s.Accept(t.Context(), alice, is.ID, Acceptance{Waive: map[int]string{1: "no PR needed"}}); err != nil {
				t.Fatal(err)
			}
		}, closer: alice, want: "closed", passDone: true},
		{name: "another required check", typ: TypeChore, prep: tickFirst, closer: alice, want: "checks [build]"},
		{name: "an open review", typ: TypeFeature, prep: tickFirst, closer: alice, want: "review by [carol]"},
		{name: "a passed review", typ: TypeFeature, prep: func(t *testing.T, s *Store, is Issue) {
			t.Helper()
			tickFirst(t, s, is)
			waits, err := reviewWaits(t.Context(), s.r, is.ID)
			if err != nil || len(waits) != 1 {
				t.Fatalf("reviewWaits(%s) = %+v, %v; want one", is.ID, waits, err)
			}
			if _, err := s.PassGate(t.Context(), carolCLI, waits[0].Gate, "reviewed"); err != nil {
				t.Fatal(err)
			}
		}, closer: alice, want: "closed", passDone: true},
		{name: "a gate holding it", prep: func(t *testing.T, s *Store, is Issue) {
			t.Helper()
			tickFirst(t, s, is)
			mustGate(t, s, alice, NewGate{Blocks: is.ID, Title: "approve", Approvers: []string{"carol"}})
		}, closer: alice, want: "held by a gate"},
		{name: "a gate its closer may release", prep: func(t *testing.T, s *Store, is Issue) {
			t.Helper()
			tickFirst(t, s, is)
			mustGate(t, s, alice, NewGate{Blocks: is.ID, Title: "approve", Approvers: []string{"carol"}})
		}, closer: danaCLI, want: "closed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			ctx := t.Context()
			mustSetWorkflow(t, s, closeWaitsWorkflow(t))
			reporter := ciJob
			if tc.closer.Session == proto.CLISession {
				reporter = tc.closer
			}
			made := func() Issue {
				t.Helper()
				is := mustCreate(t, s, NewIssue{Title: tc.name, Type: tc.typ})
				if _, _, err := s.StartIssue(ctx, alice, is.ID, 0, false); err != nil {
					t.Fatal(err)
				}
				tc.prep(t, s, is)
				return is
			}
			// landedByHand leaves landed waiting no more without a pass:
			// its gate waived and its bound item ticked.
			landedByHand := func(is Issue) {
				t.Helper()
				g, err := s.FailCheck(ctx, reporter, landed(is.ID))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.WaiveGate(ctx, danaCLI, g.ID, "landed by hand"); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Accept(ctx, alice, is.ID, Acceptance{Tick: []int{2}}); err != nil {
					t.Fatal(err)
				}
			}

			is := made()
			landedByHand(is)
			_, err := s.CloseIssue(ctx, tc.closer, is.ID, 0, "done")
			if got := closeVerdict(err); got != tc.want {
				t.Errorf("CloseIssue(%s) by %s = %q, want %q", is.ID, tc.closer.Principal, got, tc.want)
			}

			is = made()
			landedByHand(is)
			_, _, err = s.FinishIssue(ctx, tc.closer, is.ID, 0, Finish{})
			if got := closeVerdict(err); got != tc.want {
				t.Errorf("FinishIssue(%s) by %s = %q, want %q", is.ID, tc.closer.Principal, got, tc.want)
			}

			is = made()
			if _, err := s.PassCheck(ctx, reporter, landed(is.ID)); err != nil {
				t.Fatalf("PassCheck(landed on %s) by %s: %v", is.ID, reporter.Principal, err)
			}
			if got := mustShow(t, s, is.ID).Status == StatusClosed; got != tc.passDone {
				t.Errorf("closed by the closing pass of %s = %t, want %t", is.ID, got, tc.passDone)
			}
		})
	}
}
