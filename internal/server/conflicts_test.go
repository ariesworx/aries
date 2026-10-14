package server

import (
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// TestConflictOps: a replayed update (park) parks the field another
// changed, lists it, tells the author, takes a proposal and is resolved
// by accepting it; a live update at the same stale rev is refused as
// before.
func TestConflictOps(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	is := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "base", Body: "body"})
	theirs, mine := "bob's title", "alice's title"
	mustCall[proto.WriteResult](t, s, bob, proto.OpUpdate, proto.UpdateArgs{ID: is.ID, Rev: is.Rev, Title: &theirs})

	body := "alice's body"
	if _, perr := call[proto.WriteResult](t, s, alice, proto.OpUpdate,
		proto.UpdateArgs{ID: is.ID, Rev: is.Rev, Title: &mine}); perr == nil || perr.Code != proto.CodeConflict {
		t.Errorf("a live update at a stale rev = %v, want conflict", perr)
	}
	res := mustCall[proto.UpdateResult](t, s, alice, proto.OpUpdate,
		proto.UpdateArgs{ID: is.ID, Rev: is.Rev, Title: &mine, Body: &body, Park: true, Idem: "cli-park-1"})
	if len(res.Parked) != 1 || res.Parked[0].Field != "title" || res.Rev != is.Rev+2 {
		t.Fatalf("replayed update = %+v; want the title parked and the body applied", res)
	}
	cid := res.Parked[0].Conflict

	list := mustCall[proto.ConflictsResult](t, s, alice, proto.OpConflicts, proto.ConflictsArgs{Issue: is.ID})
	if len(list.Conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want 1", list)
	}
	c := list.Conflicts[0]
	if c.ID != cid || c.Base != "base" || c.Mine != mine || c.Theirs != theirs || c.By != "alice" || c.TheirsBy != "bob" || c.Advice == "" {
		t.Errorf("conflict = %+v; want base, mine, theirs, who, and advice", c)
	}
	in := mustCall[proto.InboxResult](t, s, alice, proto.OpInbox, proto.InboxArgs{})
	if in.Conflicts != 1 || len(in.Items) == 0 || in.Items[0].Kind != "conflict" {
		t.Errorf("alice's inbox = %+v; want a conflict item and the count", in)
	}
	show := mustCall[proto.ShowResult](t, s, bob, proto.OpShow, proto.ShowArgs{ID: is.ID})
	if len(show.Conflicts) != 1 || show.Conflicts[0].ID != cid {
		t.Errorf("show's conflicts = %+v, want the one open", show.Conflicts)
	}

	if _, perr := call[proto.Empty](t, s, bob, proto.OpPropose, proto.ProposeArgs{ID: cid, Choice: "both"}); perr == nil ||
		perr.Code != proto.CodeInvalid || perr.Fix == "" {
		t.Errorf("a proposal of no known choice = %v, want invalid with a fix", perr)
	}
	mustCall[proto.Empty](t, s, bob, proto.OpPropose,
		proto.ProposeArgs{ID: cid, Choice: proto.ChoiceText, Text: "both titles", Why: "keeps both"})
	got := mustCall[proto.ConflictsResult](t, s, alice, proto.OpConflicts, proto.ConflictsArgs{ID: cid})
	if len(got.Conflicts) != 1 || got.Conflicts[0].Proposal == nil || got.Conflicts[0].Proposal.Text != "both titles" {
		t.Errorf("the conflict after a proposal = %+v", got)
	}

	w := mustCall[proto.WriteResult](t, s, alice, proto.OpResolve, proto.ResolveArgs{ID: cid, Choice: proto.ChoiceAccept})
	after := mustCall[proto.ShowResult](t, s, alice, proto.OpShow, proto.ShowArgs{ID: is.ID})
	if w.ID != is.ID || after.Issue.Title != "both titles" || after.Issue.Body != body || len(after.Conflicts) != 0 {
		t.Errorf("after accepting = %+v, issue %+v; want the proposal applied and no conflict left", w, after.Issue)
	}
	if _, perr := call[proto.WriteResult](t, s, alice, proto.OpResolve, proto.ResolveArgs{ID: cid, Choice: proto.ChoiceMine}); perr == nil ||
		perr.Code != proto.CodeInvalid || !strings.Contains(perr.Message, "already resolved") {
		t.Errorf("resolving it again = %v, want invalid", perr)
	}
	if _, perr := call[proto.WriteResult](t, s, alice, proto.OpResolve, proto.ResolveArgs{ID: 999, Choice: proto.ChoiceMine}); perr == nil ||
		perr.Code != proto.CodeNotFound || !strings.Contains(perr.Fix, "sfx conflicts") {
		t.Errorf("resolving a conflict that is not there = %v, want not_found naming sfx conflicts", perr)
	}
	h := mustCall[proto.HistoryResult](t, s, alice, proto.OpHistory, proto.PageArgs{ID: is.ID})
	var ops []string
	for _, e := range h.Events {
		ops = append(ops, e.Op)
	}
	for _, op := range []string{"conflict.park", "conflict.propose", "conflict.resolve"} {
		if !strings.Contains(strings.Join(ops, " "), op) {
			t.Errorf("history = %v, want %s", ops, op)
		}
	}
}

// TestConflictCapRefused: past a cap on open conflicts the replayed
// update is refused invalid, with a fix that names how to resolve them.
func TestConflictCapRefused(t *testing.T) {
	t.Parallel()
	s := newServerWith(t, Limits{Limits: store.Limits{ConflictsPerIssue: 1}})
	is := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "base", Body: "b"})
	theirs, mine := "theirs", "mine"
	mustCall[proto.WriteResult](t, s, bob, proto.OpUpdate, proto.UpdateArgs{ID: is.ID, Rev: is.Rev, Title: &theirs, Body: &theirs})
	mustCall[proto.UpdateResult](t, s, alice, proto.OpUpdate, proto.UpdateArgs{ID: is.ID, Rev: is.Rev, Title: &mine, Park: true})
	_, perr := call[proto.UpdateResult](t, s, alice, proto.OpUpdate, proto.UpdateArgs{ID: is.ID, Rev: is.Rev, Body: &mine, Park: true})
	if perr == nil || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Message, "open conflicts") ||
		!strings.Contains(perr.Fix, "sfx resolve") || !strings.Contains(perr.Fix, "conflicts_per_issue") {
		t.Errorf("an update past the cap = %+v; want invalid, saying to resolve them or raise conflicts_per_issue", perr)
	}
}

// TestDigestConflicts: digest lists the conflicts open past the
// server's conflict_age.
func TestDigestConflicts(t *testing.T) {
	t.Parallel()
	clk := newClock()
	s, _ := newServerClock(t, Limits{ConflictAge: Duration(time.Hour)}, clk.now)
	is := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "base"})
	theirs, mine := "theirs", "mine"
	mustCall[proto.WriteResult](t, s, bob, proto.OpUpdate, proto.UpdateArgs{ID: is.ID, Rev: is.Rev, Title: &theirs})
	mustCall[proto.UpdateResult](t, s, alice, proto.OpUpdate, proto.UpdateArgs{ID: is.ID, Rev: is.Rev, Title: &mine, Park: true})

	d := mustCall[proto.DigestResult](t, s, alice, proto.OpDigest, proto.DigestArgs{})
	if len(d.Conflicts) != 0 {
		t.Errorf("digest's conflicts at once = %+v, want none yet", d.Conflicts)
	}
	clk.advance(2 * time.Hour)
	d = mustCall[proto.DigestResult](t, s, alice, proto.OpDigest, proto.DigestArgs{})
	if len(d.Conflicts) != 1 || d.Conflicts[0].ID != is.ID || d.Totals.Conflicts != 1 || !strings.Contains(d.Conflicts[0].Note, "title") {
		t.Errorf("digest's conflicts past conflict_age = %+v, totals %+v", d.Conflicts, d.Totals)
	}
}
