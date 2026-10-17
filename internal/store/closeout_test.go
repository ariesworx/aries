package store

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// closeoutWorkflow gives a default issue two close-out lines and an
// epic one of its own, and requires landed of the default.
func closeoutWorkflow(t *testing.T) proto.Workflow {
	t.Helper()
	w, err := proto.ParseWorkflow([]byte(`{"version": 1, "ci": ["ci-example"], "types": {
  "default": {
    "acceptance": ["PR opened", {"text": "Merged", "check": "landed"}],
    "checks": ["landed"],
    "closeout": ["Stop your compose stack", "Archive the session"]},
  "epic": {"closeout": ["Prune worktrees"]},
  "chore": {"acceptance": ["Posted"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// An issue's close-out is the lines of the profiles stamped on it, read
// from its log: a later workflow changes nothing, a change of type adds
// the new profile's lines, and an issue under no profile has none.
func TestCloseoutLines(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	before := mustCreate(t, s, NewIssue{Title: "before any workflow"})
	mustSetWorkflow(t, s, closeoutWorkflow(t))
	task := mustCreate(t, s, NewIssue{Title: "a task"})
	epic := mustCreate(t, s, NewIssue{Title: "an epic", Type: TypeEpic})
	chore := mustCreate(t, s, NewIssue{Title: "a chore", Type: "chore"})
	for _, tc := range []struct {
		id   IssueID
		want []string
	}{
		{before.ID, nil},
		{task.ID, []string{"Stop your compose stack", "Archive the session"}},
		{epic.ID, []string{"Prune worktrees"}},
		{chore.ID, nil},
	} {
		got, err := s.Closeout(ctx, tc.id)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("Closeout(%s) = %q, %v; want %q", tc.id, got, err, tc.want)
		}
	}
	// Retyped to an epic, the task keeps its lines and gains the epic's.
	ty := TypeEpic
	if _, err := s.UpdateIssue(ctx, alice, task.ID, task.Rev, IssuePatch{Type: &ty}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Closeout(ctx, task.ID)
	if want := []string{"Stop your compose stack", "Archive the session", "Prune worktrees"}; err != nil || !slices.Equal(got, want) {
		t.Errorf("Closeout after a change of type = %q, %v; want %q", got, err, want)
	}
}

// The closing pass tells the people it tells what to do next in an item
// of its own, since the session that did the work may be gone; a pass
// that leaves an item open sends none.
func TestClosingPassItemCarriesCloseout(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	mustSetWorkflow(t, s, closeoutWorkflow(t))
	is := mustCreate(t, s, NewIssue{Title: "to land"})
	if _, _, err := s.StartIssue(ctx, alice, is.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PassCheck(ctx, ciJob, landed(is.ID)); err != nil {
		t.Fatal(err)
	}
	for _, it := range inboxOf(t, s, alice, InboxGatePassed) {
		if strings.Contains(it.Body, "Archive the session") {
			t.Errorf("close-out sent while item 1 is open: %+v", it)
		}
	}
	if _, err := s.Accept(ctx, alice, is.ID, Acceptance{Tick: []int{1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FailCheck(ctx, ciJob, landed(is.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PassCheck(ctx, ciJob, landed(is.ID)); err != nil {
		t.Fatal(err)
	}
	var n int
	for _, it := range inboxOf(t, s, alice, InboxGatePassed) {
		if it.Issue == is.ID && it.Body == "close-out of "+string(is.ID)+": Stop your compose stack; Archive the session" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("close-out items in alice's inbox = %d; want 1: %+v", n, inboxOf(t, s, alice, InboxGatePassed))
	}
}

// Digest lists an epic once its last child has closed, with its lines,
// and not before, nor after the epic itself closes.
func TestDigestCloseoutOfEpic(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	mustSetWorkflow(t, s, closeoutWorkflow(t))
	epic := mustCreate(t, s, NewIssue{Title: "the epic", Type: TypeEpic})
	a := mustCreate(t, s, NewIssue{Title: "child a", Type: "chore", ParentID: epic.ID})
	b := mustCreate(t, s, NewIssue{Title: "child b", Type: "chore", ParentID: epic.ID})
	section := func() DigestSection {
		d, err := s.Digest(ctx, DigestFilter{Window: 24 * time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		return d.Closeout
	}
	closeChild := func(id IssueID) {
		t.Helper()
		if _, _, err := s.StartIssue(ctx, alice, id, 0, false); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.FinishIssue(ctx, alice, id, 0, Finish{Accept: Acceptance{Tick: []int{1}}}); err != nil {
			t.Fatal(err)
		}
	}
	if got := section(); got.Total != 0 {
		t.Errorf("closeout with both children open = %+v; want none", got)
	}
	closeChild(a.ID)
	if got := section(); got.Total != 0 {
		t.Errorf("closeout with one child open = %+v; want none", got)
	}
	closeChild(b.ID)
	got := section()
	if got.Total != 1 || got.Items[0].ID != epic.ID || got.Items[0].Note != "Prune worktrees" {
		t.Fatalf("closeout once the last child closed = %+v; want %s, Prune worktrees", got, epic.ID)
	}
	if _, _, err := s.StartIssue(ctx, alice, epic.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.FinishIssue(ctx, alice, epic.ID, 0, Finish{}); err != nil {
		t.Fatal(err)
	}
	if got := section(); got.Total != 0 {
		t.Errorf("closeout after the epic closed = %+v; want none", got)
	}
}
