package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	briefrec "github.com/ariesworx/aries/internal/brief"
	"github.com/ariesworx/aries/internal/secretscan"
)

// coordinator records briefs: the same principal as worker, in a session
// of its own, as every session of a one-person project is.
var (
	coordinator = Actor{Principal: "alice", Session: "sess-coord", Machine: "laptop-a"}
	worker      = Actor{Principal: "alice", Session: "sess-work", Machine: "laptop-a"}
)

func briefFields(session string) BriefFields {
	return BriefFields{Harness: briefrec.Desktop, Model: "claude-opus-5-5", Effort: "high", Session: session, Template: "brief@3"}
}

// A brief recorded before its session starts the issue waits, and the
// start that takes the claim joins it to that claim, once: show,
// history and comments return it, with the model, effort and the
// claim's session.
func TestRecordBriefJoinsStart(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{})
	text := "Do the work.\n\nThen hand off.\n"
	b, err := s.RecordBrief(ctx, coordinator, is.ID, briefFields("local_1"), text, "")
	if err != nil {
		t.Fatal(err)
	}
	if b.State != BriefPending || b.Claim != nil || b.SHA256 != briefrec.Hash(text) || b.By != "alice" || b.BySession != coordinator.Session {
		t.Fatalf("recorded = %+v", b)
	}
	v, err := s.ShowIssue(ctx, alice, is.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if v.Brief == nil || v.Brief.Comment != b.Comment || v.Brief.State != BriefPending {
		t.Fatalf("show before start: brief %+v", v.Brief)
	}
	if _, c, err := s.StartIssue(ctx, worker, is.ID, 0, false); err != nil || c.Epoch != 1 {
		t.Fatalf("start: %v (epoch %d)", err, c.Epoch)
	}
	// A renewal through start joins nothing more.
	if _, _, err := s.StartIssue(ctx, worker, is.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	v, err = s.ShowIssue(ctx, alice, is.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := v.Brief
	if got == nil || got.State != BriefBound || got.Claim == nil || got.Claim.Session != worker.Session ||
		got.Claim.Principal != "alice" || got.Claim.Epoch != 1 {
		t.Fatalf("show after start: brief %+v", got)
	}
	if got.Model != "claude-opus-5-5" || got.Effort != "high" || got.Harness != briefrec.Desktop || got.Template != "brief@3" ||
		got.Session != "local_1" || got.Text != strings.TrimSpace(text) {
		t.Errorf("show after start: fields %+v", got)
	}
	cs, err := s.Comments(ctx, is.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Kind != CommentBrief || cs[0].Session != coordinator.Session {
		t.Fatalf("comments = %+v", cs)
	}
	rec, body, ok := briefrec.ParseComment(cs[0].Body)
	if !ok || rec.Model != "claude-opus-5-5" || rec.Session != "local_1" || briefrec.Hash(body) != b.SHA256 {
		t.Errorf("the comment's record = %+v, %v; body %q", rec, ok, cs[0].Body)
	}
	evs, err := s.History(ctx, is.ID)
	if err != nil {
		t.Fatal(err)
	}
	var adds, binds int
	for _, e := range evs {
		switch e.Op {
		case OpCommentAdd:
			var after struct {
				Kind  CommentKind `json:"kind"`
				Brief struct {
					Model  string `json:"model"`
					Effort string `json:"effort"`
					SHA256 string `json:"sha256"`
				} `json:"brief"`
			}
			if err := json.Unmarshal(e.After, &after); err != nil {
				t.Fatal(err)
			}
			if after.Kind == CommentBrief && after.Brief.Model == "claude-opus-5-5" && after.Brief.Effort == "high" && after.Brief.SHA256 == b.SHA256 {
				adds++
			}
		case OpBriefBind:
			var after struct {
				Comment string `json:"comment"`
				Session string `json:"session"`
				Epoch   int64  `json:"epoch"`
			}
			if err := json.Unmarshal(e.After, &after); err != nil {
				t.Fatal(err)
			}
			if after.Comment != b.Comment || after.Session != worker.Session || after.Epoch != 1 {
				t.Errorf("brief.bind = %s", e.After)
			}
			binds++
		}
	}
	if adds != 1 || binds != 1 {
		t.Errorf("history has %d brief comments and %d joins, want 1 of each: %+v", adds, binds, evs)
	}
	// The claim has its brief: another naming its session is refused.
	_, err = s.RecordBrief(ctx, coordinator, is.ID, briefFields(worker.Session), "again", "")
	if be, ok := errors.AsType[*BriefError](err); !ok || be.Reason != BriefClaimHasOne || !errors.Is(err, ErrInvalid) {
		t.Errorf("a second brief for the claim: %v", err)
	}
}

// The dispatcher claims before it launches, then records the run's
// brief from its own session, naming the run's: it is joined at once.
func TestRecordBriefJoinsLiveClaim(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{})
	if _, _, err := s.StartIssue(ctx, worker, is.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	b, err := s.RecordBrief(ctx, coordinator, is.ID, briefFields(worker.Session), "run brief", "")
	if err != nil {
		t.Fatal(err)
	}
	if b.State != BriefBound || b.Claim == nil || b.Claim.Session != worker.Session || b.Claim.Epoch != 1 {
		t.Fatalf("recorded = %+v", b)
	}
	// One naming another session waits for the next claim.
	other, err := s.RecordBrief(ctx, coordinator, is.ID, briefFields("local_2"), "later brief", "")
	if err != nil {
		t.Fatal(err)
	}
	if other.State != BriefPending {
		t.Errorf("a brief naming another session = %+v, want pending", other)
	}
}

// A brief recorded while another waits replaces it: the first stays as
// history and is never joined.
func TestRecordBriefReplacesPending(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{})
	first, err := s.RecordBrief(ctx, coordinator, is.ID, briefFields("local_1"), "first", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.RecordBrief(ctx, coordinator, is.ID, briefFields("local_1"), "second", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.StartIssue(ctx, worker, is.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	bs, err := s.Briefs(ctx, is.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 2 || bs[0].Comment != first.Comment || bs[0].State != BriefReplaced || bs[0].Claim != nil ||
		bs[1].Comment != second.Comment || bs[1].State != BriefBound || bs[1].Claim == nil {
		t.Fatalf("briefs = %+v", bs)
	}
}

// A repeat with the same idempotency key returns the first brief and
// writes nothing.
func TestRecordBriefIdempotent(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{})
	a, err := s.RecordBrief(ctx, coordinator, is.ID, briefFields("local_1"), "text", "k-1")
	if err != nil {
		t.Fatal(err)
	}
	seq := lastSeq(t, s)
	b, err := s.RecordBrief(ctx, coordinator, is.ID, briefFields("local_1"), "text", "k-1")
	if err != nil || b.Comment != a.Comment {
		t.Fatalf("repeat = %+v, %v; want %s", b, err, a.Comment)
	}
	if lastSeq(t, s) != seq {
		t.Error("the repeat wrote an event")
	}
}

// Each refusal is ErrInvalid, and a brief or field that looks like a
// secret is refused naming the field and kind, never the text.
func TestRecordBriefRefusals(t *testing.T) {
	t.Parallel()
	s := openStore(t, newDSN(t), Options{Limits: Limits{BriefBytes: 64}})
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{})
	held := mustCreate(t, s, NewIssue{})
	if _, _, err := s.StartIssue(ctx, worker, held.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	key := "AKIA" + "FAKEFAKEFAKEFAKE"
	ok := briefFields("local_1")
	with := func(f func(*BriefFields)) BriefFields { c := ok; f(&c); return c }
	for _, tc := range []struct {
		name   string
		actor  Actor
		id     IssueID
		f      BriefFields
		text   string
		reason BriefReason
		field  string
	}{
		{"over the cap", coordinator, is.ID, ok, strings.Repeat("x", 65), "", ""},
		{"empty", coordinator, is.ID, ok, "", "", ""},
		{"control character", coordinator, is.ID, ok, "a\x07b", "", ""},
		{"no model", coordinator, is.ID, with(func(f *BriefFields) { f.Model = "" }), "t", "", ""},
		{"no harness", coordinator, is.ID, with(func(f *BriefFields) { f.Harness = "" }), "t", "", ""},
		{"no template", coordinator, is.ID, with(func(f *BriefFields) { f.Template = "" }), "t", "", ""},
		{"a space in a field", coordinator, is.ID, with(func(f *BriefFields) { f.Model = "claude opus" }), "t", "", ""},
		{"unknown effort", coordinator, is.ID, with(func(f *BriefFields) { f.Effort = "huge" }), "t", "", ""},
		{"a secret in the text", coordinator, is.ID, ok, "use " + key, BriefSecret, "text"},
		{"a secret in a field", coordinator, is.ID, with(func(f *BriefFields) { f.Session = key }), "t", BriefSecret, "session"},
		{"its own claim", worker, held.ID, ok, "t", BriefOwnClaim, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.RecordBrief(ctx, tc.actor, tc.id, tc.f, tc.text, "")
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("RecordBrief = %v, want ErrInvalid", err)
			}
			if tc.reason == "" {
				return
			}
			be, ok := errors.AsType[*BriefError](err)
			if !ok || be.Reason != tc.reason || be.Field != tc.field {
				t.Fatalf("RecordBrief = %v (%+v), want reason %s on %q", err, be, tc.reason, tc.field)
			}
			if tc.reason == BriefSecret && (be.Kind != secretscan.KindAWSAccessKey || strings.Contains(err.Error(), key)) {
				t.Errorf("secret refusal %q (kind %s)", err, be.Kind)
			}
		})
	}
	if _, err := s.RecordBrief(ctx, coordinator, "tst-missing", ok, "t", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing issue: %v", err)
	}
	cs, err := s.Comments(ctx, is.ID)
	if err != nil || len(cs) != 0 {
		t.Errorf("refusals left comments %+v, %v", cs, err)
	}
}

// brief_bytes is a limit with a default and a ceiling.
func TestBriefBytesLimit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		v    int
		want bool
	}{{0, true}, {1, true}, {maxBrief, true}, {maxBrief + 1, false}, {-1, false}} {
		err := Limits{BriefBytes: tc.v}.Validate()
		if (err == nil) != tc.want {
			t.Errorf("brief_bytes %d: %v", tc.v, err)
		}
	}
	if DefaultLimits.BriefBytes != 32768 {
		t.Errorf("default brief_bytes = %d", DefaultLimits.BriefBytes)
	}
}
