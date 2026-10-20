package outcome

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// usage is one record attributed to the issue: output tokens at a time.
type usage struct {
	at  time.Time
	out int64
}

// fakeStore answers from fixtures; Cost sums the usage in its window, as
// the server's report by issue would for records of request granularity.
type fakeStore struct {
	show      proto.ShowResult
	events    []proto.Event
	comments  []proto.Comment
	usage     []usage
	truncated bool
	costs     int
}

func (f *fakeStore) Show(context.Context, string) (proto.ShowResult, error) { return f.show, nil }
func (f *fakeStore) History(context.Context, string) ([]proto.Event, error) {
	return append([]proto.Event(nil), f.events...), nil
}
func (f *fakeStore) Comments(context.Context, string) ([]proto.Comment, error) {
	return f.comments, nil
}
func (f *fakeStore) Cost(_ context.Context, since, until time.Time) (proto.CostResult, error) {
	f.costs++
	var n int64
	for _, u := range f.usage {
		if !u.at.Before(since) && u.at.Before(until) {
			n += u.out
		}
	}
	res := proto.CostResult{By: "issue", Truncated: f.truncated}
	if n > 0 {
		res.Groups = []proto.CostGroup{{Key: f.show.Issue.ID, Tokens: proto.Tokens{Output: &n}}}
	}
	return res, nil
}

type fakeHost struct {
	changes []Change
	detail  Detail
	err     error
}

func (h *fakeHost) Name() string { return "github" }
func (h *fakeHost) Changes(context.Context, string, []string) ([]Change, error) {
	return h.changes, h.err
}
func (h *fakeHost) Closed(context.Context, time.Time) ([]Change, error) { return nil, nil }
func (h *fakeHost) Detail(context.Context, Change) (Detail, error)      { return h.detail, h.err }

func ev(m int, session, op string, before, after string) proto.Event {
	e := proto.Event{At: at(m), Principal: "alice", Session: session, Op: op}
	if before != "" {
		e.Before = json.RawMessage(before)
	}
	if after != "" {
		e.After = json.RawMessage(after)
	}
	return e
}

func stamp(m int) string { return `"` + at(m).Format(time.RFC3339Nano) + `"` }

func cm(id string, m int, body string) proto.Comment {
	return proto.Comment{ID: id, Author: "alice", Body: body, CreatedAt: at(m)}
}

// known is an issue's history with every case §4 names: a CI failure
// before the first done handoff, a catch-up merge after it that
// conflicted and was committed outside every hold, a woken session's
// hold that ran past its lease, usage on both sides of each boundary,
// and a landed close after the merge.
func known() (*fakeStore, *fakeHost) {
	s := &fakeStore{
		show: proto.ShowResult{
			Issue: proto.Issue{ID: "ar-a1b2", Type: "feature", Parent: "ar-e0e0", Status: "closed", Rev: 14, CreatedAt: at(0)},
			Checks: []proto.Gate{{Check: "landed", Outcome: "passed", At: ptr(at(108))},
				{Check: "smoke", Outcome: "waived", At: ptr(at(96))}},
			Usage: &proto.IssueUsage{Models: []proto.ModelTokens{{Model: "claude-opus-5-5"}}},
		},
		events: []proto.Event{
			ev(0, "s0", "issue.create", "", `{"id":"ar-a1b2"}`),
			ev(10, "s1", "claim.take", "", `{"epoch":1,"expires_at":`+stamp(25)+`}`),
			ev(40, "s1", "comment.add", "", `{"kind":"handoff","handoff":{"state":"done","branch":"feature/ar-a1b2-x"}}`),
			ev(40, "s1", "claim.release", `{"epoch":1,"expires_at":`+stamp(55)+`}`, ""),
			ev(70, "s2", "claim.take", "", `{"epoch":2,"expires_at":`+stamp(85)+`}`),
			ev(90, "reaper", "claim.expire", `{"epoch":2,"expires_at":`+stamp(85)+`}`, "null"),
			ev(95, "cli", "acceptance.waive", "", `{"n":3,"reason":"not this time"}`),
			ev(110, "cli", "issue.close", `{"status":"in_progress"}`, `{"status":"closed","close_reason":"landed"}`),
		},
		comments: []proto.Comment{
			cm("b1", 5, "brief: harness=claude-code model=claude-opus-5-5 effort=high\nDo the thing."),
			cm("l1", 41, "lesson: a thing\nstage: test"),
			cm("w1", 62, "rework: conflict sent to s2"),
		},
		usage: []usage{{at(15), 100}, {at(35), 200}, {at(45), 50}, {at(78), 400}, {at(105), 25}},
	}
	h := &fakeHost{
		changes: []Change{{ID: "271", URL: "https://example.com/acme/widgets/pull/271", Base: "main",
			Branch: "feature/ar-a1b2-x", Opened: at(19), Merged: at(100), Closed: at(100)}},
		detail: Detail{
			Commits: []Commit{
				{SHA: "c1", Parents: []string{"b0"}, At: at(20)},
				{SHA: "c2", Parents: []string{"c1"}, At: at(30)},
				{SHA: "c3", Parents: []string{"c2", "m1"}, At: at(60)},
				{SHA: "c4", Parents: []string{"c3"}, At: at(75)},
			},
			Runs: []Run{
				run("1", "c1", 1, 21, Fail, "test"),
				run("2", "c2", 1, 31, Pass),
				run("3", "c3", 1, 61, Pass),
				run("4", "c4", 1, 76, Pass),
				queued(run("9", "q1", 1, 90, Pass)),
			},
			ChangesRequested: 1, Comments: 2, Fetched: true,
		},
	}
	return s, h
}

func capture(s *fakeStore, h Host) *Capture {
	return &Capture{Store: s, Host: h, Repo: "/nonexistent", Now: func() time.Time { return at(200) },
		Conflicts: func(_ context.Context, _, a, b string) (bool, error) { return a == "c2" && b == "m1", nil }}
}

func n64(n int64) *int64 { return &n }

// The record of the known history, every field held to a value computed
// by hand.
func TestCaptureKnownHistory(t *testing.T) {
	t.Parallel()
	s, h := known()
	got, err := capture(s, h).Issue(t.Context(), "ar-a1b2")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Records) != 1 {
		t.Fatalf("records = %d, want 1", len(got.Records))
	}
	r := got.Records[0]
	want := Record{
		V: 1, Issue: "ar-a1b2", Type: "feature", Epic: "ar-e0e0", Seq: 1, Outcome: Landed, Rev: 14,
		CapturedAt: at(200), Capturer: "aries outcome",
		Change: &ChangeRef{Host: "github", ID: "271", URL: "https://example.com/acme/widgets/pull/271", Base: "main"},
		Brief:  &Brief{SHA256: "5e7d4ef7d4b6bd2c7c3a1fe67d3e4c6dc5d0ea0bdc1a26ef7df1e5ae5b0f0a85", From: "comment"},
		Runs: []RunRecord{
			// s1 held from 10 to its release at 40; s2 from 70 to its
			// lease's end at 85, not the reaper's 90.
			{Session: "s1", Kind: "agent", Harness: "claude-code", Models: []string{"claude-opus-5-5"}, Effort: "high",
				FirstTake: at(10), HeldS: 1800, Handoffs: []string{"done"}},
			{Session: "s2", Kind: "agent", FirstTake: at(70), HeldS: 900, Handoffs: []string{}},
		},
		Times: Times{Created: at(0), FirstStart: ptr(at(10)), ChangeOpened: ptr(at(19)), FirstDone: ptr(at(40)),
			Merged: ptr(at(100)), OutcomeAt: at(110)},
		HeldS: 2700, CycleS: 6600,
		CI: &CI{Host: "github", Attempts: 4, QueueAttempts: 1, Failures: []Failure{
			{Run: "ci:1", Attempt: 1, SHA: "c1", Event: "change", Jobs: []string{"test"}, Cause: CauseChange, At: at(22)},
		}},
		DefaultBranch: &DefaultBranch{Merges: 1, Conflicted: 1, Sweeps: 1},
		Review:        &Review{ChangesRequested: 1, Comments: 2},
		Reopens:       []Reopen{},
		Waived:        Waived{Items: []int{3}, Gates: []string{"smoke"}},
		// All usage before the close: 100 + 200 + 50 + 400 + 25.
		Tokens: &Tokens{Tokens: proto.Tokens{Output: n64(775)},
			ByModel: []proto.ModelTokens{{Model: "claude-opus-5-5"}}},
		// The CI failure at 22 comes before the done handoff at 40, so the
		// boundary is 22, and rework runs to the merge at 100: 200 + 50 +
		// 400. The catch-up at 60 was committed under no hold.
		Rework: Rework{From: ptr(at(22)), Trigger: "ci", PreMerge: proto.Tokens{Output: n64(650)},
			Attributed: "partial"},
		Lessons: 1,
		Missing: []Missing{{"harness", "none"}},
	}
	// The brief's hash is of the text after its first line.
	want.Brief.SHA256 = briefs([]proto.Comment{{Body: "brief: x\nDo the thing."}}, at(1))[0].sha
	if !reflect.DeepEqual(r, want) {
		g, _ := json.Marshal(r)
		w, _ := json.Marshal(want)
		t.Errorf("record =\n%s\nwant\n%s", g, w)
	}
	if s := r.Summary(); s != "outcome: v1 landed seq 1, 4 CI attempts, 1 conflict, rework 83% pre-merge, 1 missing" {
		t.Errorf("summary = %q", s)
	}
}

// Without a CI failure the boundary is the first done handoff; without
// either there is no boundary and no pre-merge rework, which is 0, not
// missing.
func TestReworkBoundary(t *testing.T) {
	t.Parallel()
	s, h := known()
	h.detail.Runs = []Run{run("2", "c2", 1, 31, Pass)}
	r := mustOne(t, capture(s, h))
	if r.Rework.Trigger != "done" || !r.Rework.From.Equal(at(40)) || *r.Rework.PreMerge.Output != 50+400 {
		t.Errorf("rework = %+v, want from 40 by done, 450", r.Rework)
	}

	s, h = known()
	h.changes, h.detail = nil, Detail{}
	s.events = []proto.Event{
		ev(0, "s0", "issue.create", "", `{}`),
		ev(10, "s1", "claim.take", "", `{"epoch":1}`),
		ev(20, "s1", "issue.close", "", `{"status":"closed","close_reason":"duplicate"}`),
	}
	s.show.Checks = nil
	r = mustOne(t, capture(s, h))
	if r.Outcome != Closed || r.Rework.From != nil || r.Rework.PreMerge.Output != nil || r.Change != nil {
		t.Errorf("closed without a change: %+v", r)
	}
	if r.CI == nil || r.CI.Attempts != 0 || missingField(r.Missing, "change") {
		t.Errorf("closed without a change: ci %+v, missing %+v", r.CI, r.Missing)
	}
}

func missingField(ms []Missing, field string) bool {
	for _, m := range ms {
		if m.Field == field {
			return true
		}
	}
	return false
}

func mustOne(t *testing.T, c *Capture) Record {
	t.Helper()
	got, err := c.Issue(t.Context(), "ar-a1b2")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Records) != 1 {
		t.Fatalf("records = %d, want 1", len(got.Records))
	}
	return got.Records[0]
}

// A reopen by a failed check and a second landing give two records, the
// second with the post-close rework and the reopen, each covering the
// issue up to its own outcome.
func TestCaptureReopened(t *testing.T) {
	t.Parallel()
	s, h := known()
	s.events = append(s.events,
		ev(120, "cli", "issue.reopen", "", `{"status":"open","reason":"check failed"}`),
		ev(130, "s3", "claim.take", "", `{"epoch":3}`),
		ev(150, "s3", "issue.close", "", `{"status":"closed","close_reason":"landed"}`),
	)
	s.usage = append(s.usage, usage{at(140), 60})
	got, err := capture(s, h).Issue(t.Context(), "ar-a1b2")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Records) != 2 {
		t.Fatalf("records = %d, want 2", len(got.Records))
	}
	one, two := got.Records[0], got.Records[1]
	if one.Seq != 1 || len(one.Reopens) != 0 || one.Tokens.ByModel != nil || one.Rework.PostClose.Output != nil {
		t.Errorf("first record: %+v", one)
	}
	if two.Seq != 2 || len(two.Reopens) != 1 || two.Reopens[0].Reason != "check failed" {
		t.Errorf("second record's reopens: %+v", two.Reopens)
	}
	if two.Rework.PostClose.Output == nil || *two.Rework.PostClose.Output != 60 || *two.Tokens.Output != 835 {
		t.Errorf("second record's tokens %+v, post-close %+v", two.Tokens, two.Rework.PostClose)
	}
	if len(two.Runs) != 3 || two.Runs[2].Session != "s3" || two.Runs[2].HeldS != 1200 {
		t.Errorf("second record's runs: %+v", two.Runs)
	}
}

// A change closed unmerged while its issue stays open is an outcome of
// its own, pr-closed; an open issue with no closed change has none.
func TestCapturePRClosed(t *testing.T) {
	t.Parallel()
	s, h := known()
	s.show.Issue.Status, s.show.Checks = "in_progress", nil
	s.events = s.events[:4]
	h.changes[0].Merged, h.changes[0].Unmerge, h.changes[0].Closed = time.Time{}, true, at(50)
	r := mustOne(t, capture(s, h))
	if r.Outcome != PRClosed || r.Times.OutcomeAt != at(50) || r.Times.Merged != nil {
		t.Errorf("pr-closed: %+v", r)
	}
	// Rework to the close at 50: 200 + 50, from the failure at 22.
	if *r.Rework.PreMerge.Output != 250 || r.CI.Attempts != 2 {
		t.Errorf("pr-closed rework %+v, ci %+v", r.Rework, r.CI)
	}

	h.changes[0].Closed, h.changes[0].Unmerge = time.Time{}, false
	got, err := capture(s, h).Issue(t.Context(), "ar-a1b2")
	if err != nil || len(got.Records) != 0 {
		t.Errorf("an open issue: %d records, %v", len(got.Records), err)
	}
}

// What the capture cannot read is listed in missing with its reason,
// never given as a lower bound.
func TestCaptureMissing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(*fakeStore, *fakeHost) Host
		want  []Missing
	}{
		{"no host", func(*fakeStore, *fakeHost) Host { return nil },
			[]Missing{{"change", "nohost"}, {"ci", "nohost"}, {"default_branch", "nohost"}, {"harness", "none"}, {"review", "nohost"}}},
		{"the host failed", func(_ *fakeStore, h *fakeHost) Host { h.err = errors.New("gh: not signed in"); return h },
			[]Missing{{"change", "host"}, {"ci", "host"}, {"default_branch", "host"}, {"harness", "none"}, {"review", "host"}}},
		{"a capped cost report", func(s *fakeStore, h *fakeHost) Host { s.truncated = true; return h },
			[]Missing{{"harness", "none"}, {"rework", "capped"}, {"tokens", "capped"}}},
		{"a rewritten branch", func(_ *fakeStore, h *fakeHost) Host { h.detail.Rewritten = true; return h },
			[]Missing{{"default_branch", "rewritten"}, {"harness", "none"}}},
		{"commits not fetched", func(_ *fakeStore, h *fakeHost) Host { h.detail.Fetched = false; return h },
			[]Missing{{"default_branch", "git"}, {"harness", "none"}}},
		{"no brief", func(s *fakeStore, h *fakeHost) Host { s.comments = s.comments[1:]; return h },
			[]Missing{{"brief", "none"}, {"harness", "none"}}},
		{"no merged change", func(_ *fakeStore, h *fakeHost) Host { h.changes = nil; return h },
			[]Missing{{"change", "none"}, {"ci", "none"}, {"default_branch", "none"}, {"harness", "none"}, {"review", "none"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, h := known()
			r := mustOne(t, capture(s, tc.setup(s, h)))
			if !reflect.DeepEqual(r.Missing, tc.want) {
				t.Errorf("missing = %+v, want %+v", r.Missing, tc.want)
			}
		})
	}
}

// A record round-trips through its comment, and a comment that begins
// outcome: but holds no record is a failed capture.
func TestCommentRoundTrip(t *testing.T) {
	t.Parallel()
	s, h := known()
	r := mustOne(t, capture(s, h))
	body, err := r.Comment()
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(body, "\n"); lines != 1 {
		t.Errorf("the comment has %d newlines, want 1: %q", lines, body)
	}
	back, ok, err := Parse(body)
	if !ok || err != nil || !same(back, r) {
		t.Errorf("Parse = %v, %v; same %v", ok, err, same(back, r))
	}
	for _, bad := range []string{"outcome: v1 landed", "outcome: v1\n{not json", "outcome: v1\n{\"v\":1}"} {
		if _, ok, err := Parse(bad); !ok || !errors.Is(err, ErrNotRecord) {
			t.Errorf("Parse(%q) = %v, %v; want a failed capture", bad, ok, err)
		}
	}
	if _, ok, _ := Parse("lesson: outcome: no"); ok {
		t.Error("a lesson parsed as an outcome comment")
	}
}

// A record is posted once per seq; a later capture that says the same
// posts nothing, and one that differs supersedes the latest, never
// editing it.
func TestPlan(t *testing.T) {
	t.Parallel()
	s, h := known()
	r := mustOne(t, capture(s, h))
	other := r
	other.Lessons = 2
	newer := r
	newer.CapturedAt, newer.Rev, newer.Backfill = at(500), 99, true
	tests := []struct {
		name               string
		posted             []Posted
		action, supersedes string
	}{
		{"none posted", nil, "post", ""},
		{"another seq posted", []Posted{{"c1", at(1), withSeq(r, 2)}}, "post", ""},
		{"the same posted", []Posted{{"c1", at(1), newer}}, "unchanged", ""},
		{"a different one posted", []Posted{{"c1", at(1), other}}, "supersede", "c1"},
		{"the latest decides", []Posted{{"c1", at(1), r}, {"c2", at(2), other}}, "supersede", "c2"},
		{"superseded already", []Posted{{"c1", at(1), other}, {"c2", at(2), r}}, "unchanged", ""},
	}
	for _, tc := range tests {
		action, sup := Plan(r, tc.posted)
		if action != tc.action || sup != tc.supersedes {
			t.Errorf("%s: Plan = %q, %q; want %q, %q", tc.name, action, sup, tc.action, tc.supersedes)
		}
	}
}

func withSeq(r Record, n int) Record { r.Seq = n; return r }

// A second capture of an issue whose records are posted finds nothing
// new to post.
func TestSecondCapturePostsNothing(t *testing.T) {
	t.Parallel()
	s, h := known()
	first, err := capture(s, h).Issue(t.Context(), "ar-a1b2")
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range first.Records {
		body, err := r.Comment()
		if err != nil {
			t.Fatal(err)
		}
		s.comments = append(s.comments, cm("o"+string(rune('1'+i)), 300, body))
		s.events = append(s.events, ev(300, "s9", "comment.add", "", `{"body":"outcome: …"}`))
	}
	c := capture(s, h)
	c.Now = func() time.Time { return at(400) }
	second, err := c.Issue(t.Context(), "ar-a1b2")
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Posted) != len(first.Records) {
		t.Fatalf("posted = %d, want %d", len(second.Posted), len(first.Records))
	}
	for _, r := range second.Records {
		if action, _ := Plan(r, second.Posted); action != "unchanged" {
			t.Errorf("seq %d: %s, want unchanged", r.Seq, action)
		}
	}
}
