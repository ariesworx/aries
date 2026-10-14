package store

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// ShowIssue gives what the reads it stands for give, one by one, on a
// store that does not change in between.
func TestShowIssueMatchesItsParts(t *testing.T) {
	t.Parallel()
	clk := &clock{t: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	s := openStore(t, newDSN(t), Options{Now: clk.now})
	ctx := t.Context()
	parent := mustCreate(t, s, NewIssue{Title: "parent", Account: "acme"})
	is := mustCreate(t, s, NewIssue{Title: "shown", ParentID: parent.ID, Labels: []string{"ui", "bug"},
		Acceptance: "- [ ] one\n- [ ] two\n- [ ] three", Paths: []string{"internal/a/", "docs/a.md"}})
	blocker := mustCreate(t, s, NewIssue{Title: "blocker"})
	after := mustCreate(t, s, NewIssue{Title: "after"})
	held := mustCreate(t, s, NewIssue{Title: "held", Paths: []string{"internal/a/x.go"}})
	if err := s.AddDep(ctx, alice, is.ID, blocker.ID, DepBlocks, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.AddDep(ctx, alice, after.ID, is.ID, DepRelated, ""); err != nil {
		t.Fatal(err)
	}
	mustStart(t, s, carol, held.ID)
	mustStart(t, s, bob, is.ID)
	clk.add(time.Minute)
	if _, err := s.Accept(ctx, bob, is.ID, Acceptance{Tick: []int{1}}); err != nil {
		t.Fatal(err)
	}
	n := int64(1200)
	if _, err := s.AddUsage(ctx, bob, []UsageRecord{{RequestID: "r1", Model: "m", Harness: "h", At: clk.now(),
		Granularity: GranularityRequest, Input: &n}}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LogHours(ctx, bob, NewHours{Issue: is.ID, Duration: time.Hour, On: clk.now().Truncate(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HandoffIssue(ctx, bob, is.ID, 0, HandoffNote{Note: "half done", HandoffFields: HandoffFields{Next: "the rest"}},
		false, "", nil); err != nil {
		t.Fatal(err)
	}
	clk.add(time.Minute)

	got, err := s.ShowIssue(ctx, alice, is.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	var want IssueView
	if want.Issue, err = s.GetIssue(ctx, is.ID); err != nil {
		t.Fatal(err)
	}
	if want.Deps, err = s.Deps(ctx, is.ID); err != nil {
		t.Fatal(err)
	}
	if want.Claim, err = s.ClaimOf(ctx, is.ID); err != nil {
		t.Fatal(err)
	}
	if want.Handoff, err = s.LastHandoff(ctx, is.ID); err != nil {
		t.Fatal(err)
	}
	if want.Items, err = s.AcceptanceItems(ctx, is.ID); err != nil {
		t.Fatal(err)
	}
	if want.Usage, err = s.IssueUsage(ctx, is.ID); err != nil {
		t.Fatal(err)
	}
	if want.Files, err = s.IssueFiles(ctx, alice, is.ID, 1); err != nil {
		t.Fatal(err)
	}
	// Each part has something in it, so a part left out shows.
	if len(want.Deps) != 2 || want.Claim == nil || want.Handoff == nil || len(want.Items) != 3 || want.Usage.Held == 0 ||
		want.Usage.Logged == 0 || want.Usage.Account != "acme" || len(want.Files.Paths) != 1 || want.Files.More != 1 ||
		len(want.Files.Overlaps) != 1 || len(want.Issue.Labels) != 2 {
		t.Fatalf("the fixture is missing a part: %+v", want)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ShowIssue(%s) =\n  %+v\nwant, from the reads one by one,\n  %+v", is.ID, got, want)
	}

	if _, err := s.ShowIssue(ctx, alice, "tst-nosuch1", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("ShowIssue(an issue that is not there) = %v, want ErrNotFound", err)
	}
	if _, err := s.ShowIssue(ctx, alice, "not an id", 0); !errors.Is(err, ErrInvalid) {
		t.Errorf("ShowIssue(a malformed id) = %v, want ErrInvalid", err)
	}
}
