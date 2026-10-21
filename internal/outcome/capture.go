package outcome

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	briefpkg "github.com/ariesworx/aries/internal/brief"
	"github.com/ariesworx/aries/internal/gitx"
	"github.com/ariesworx/aries/internal/proto"
)

// Store is what the capture reads of Aries, all through ops a client
// already sends: show, history and comments whole, and a cost report by
// issue over a window.
type Store interface {
	Show(ctx context.Context, id string) (proto.ShowResult, error)
	History(ctx context.Context, id string) ([]proto.Event, error)
	Comments(ctx context.Context, id string) ([]proto.Comment, error)
	Cost(ctx context.Context, since, until time.Time) (proto.CostResult, error)
}

// Capture builds records. Host may be nil, and then every field the
// host gives is missing with the reason "nohost".
type Capture struct {
	Store Store
	Host  Host
	// Repo is the local repository git reads a change's commits in.
	Repo string
	// Now is the capture's clock.
	Now func() time.Time
	// Backfill marks records captured after the fact (Q9).
	Backfill bool
	// Conflicts says whether two commits conflict; gitx.MergeConflicts
	// when nil.
	Conflicts func(ctx context.Context, dir, a, b string) (bool, error)
}

// Posted is an outcome comment already on the issue.
type Posted struct {
	Comment string
	At      time.Time
	Record  Record
}

// Issue is what a capture of one issue found: a record for each of its
// outcomes, oldest first; the records already posted; and how many
// outcome comments did not parse.
type Issue struct {
	ID       string
	Records  []Record
	Posted   []Posted
	Unparsed int
}

// outcomeAt is one outcome before its record is built.
type outcomeAt struct {
	kind   string
	at     time.Time
	close  *proto.Event // the issue.close, for landed and closed
	change *Change      // the change closed unmerged, for pr-closed
}

// Issue captures every outcome of the issue id. An issue with none yet
// returns no records.
func (c *Capture) Issue(ctx context.Context, id string) (Issue, error) {
	show, err := c.Store.Show(ctx, id)
	if err != nil {
		return Issue{}, err
	}
	events, err := c.Store.History(ctx, id)
	if err != nil {
		return Issue{}, err
	}
	slices.SortStableFunc(events, func(a, b proto.Event) int { return a.At.Compare(b.At) })
	comments, err := c.Store.Comments(ctx, id)
	if err != nil {
		return Issue{}, err
	}
	out := Issue{ID: id}
	for _, cm := range comments {
		r, ok, err := Parse(cm.Body)
		switch {
		case !ok:
		case err != nil:
			out.Unparsed++
		default:
			out.Posted = append(out.Posted, Posted{Comment: cm.ID, At: cm.CreatedAt, Record: r})
		}
	}
	var changes []Change
	var hostErr error
	if c.Host != nil {
		changes, hostErr = c.Host.Changes(ctx, id, handoffBranches(events))
	}
	outs := outcomes(show, events, changes)
	for i, o := range outs {
		r := c.record(ctx, show, events, comments, changes, hostErr, o, i == len(outs)-1)
		r.Seq = i + 1
		out.Records = append(out.Records, r)
	}
	return out, nil
}

// outcomes lists the issue's outcomes in time order: each close, landed
// when the closing pass closed it or its landed check had passed, and
// each change closed unmerged while the issue was open.
func outcomes(show proto.ShowResult, events []proto.Event, changes []Change) []outcomeAt {
	var out []outcomeAt
	for i := range events {
		e := &events[i]
		if e.Op != "issue.close" {
			continue
		}
		kind := Closed
		if str(e.After, "close_reason") == "landed" || landedBy(show, e.At) {
			kind = Landed
		}
		out = append(out, outcomeAt{kind: kind, at: e.At, close: e})
	}
	for i := range changes {
		ch := &changes[i]
		if ch.Unmerge && !ch.Closed.IsZero() && !closedAt(events, ch.Closed) {
			out = append(out, outcomeAt{kind: PRClosed, at: ch.Closed, change: ch})
		}
	}
	slices.SortStableFunc(out, func(a, b outcomeAt) int { return a.at.Compare(b.at) })
	return out
}

// landedBy says the issue's landed check had passed by t.
func landedBy(show proto.ShowResult, t time.Time) bool {
	return slices.ContainsFunc(show.Checks, func(g proto.Gate) bool {
		return g.Check == "landed" && g.Outcome == "passed" && g.At != nil && !g.At.After(t)
	})
}

// closedAt says the issue was closed at t, by its events.
func closedAt(events []proto.Event, t time.Time) bool {
	closed := false
	for _, e := range events {
		if e.At.After(t) {
			break
		}
		switch e.Op {
		case "issue.close":
			closed = true
		case "issue.reopen":
			closed = false
		}
	}
	return closed
}

// handoffBranches are the branches handoffs named, which find a change
// whose branch is not the issue's by name.
func handoffBranches(events []proto.Event) []string {
	var out []string
	for _, e := range events {
		if e.Op != "comment.add" {
			continue
		}
		var a struct {
			Handoff struct {
				Branch string `json:"branch"`
			} `json:"handoff"`
		}
		if json.Unmarshal(e.After, &a) == nil && a.Handoff.Branch != "" && !slices.Contains(out, a.Handoff.Branch) {
			out = append(out, a.Handoff.Branch)
		}
	}
	return out
}

// hold is one session's hold of the claim.
type hold struct {
	session, principal string
	start, end         time.Time
}

// holds reads the claim's holds from the events up to cutoff, as
// store/attribution.go does: a hold ends at the next take, a release, an
// expiry or a close, or at its lease's end if that came first; one still
// open ends at the cutoff.
func holds(events []proto.Event, cutoff time.Time) []hold {
	var out []hold
	var cur *hold
	end := func(at time.Time, before json.RawMessage) {
		if cur == nil {
			return
		}
		if exp, ok := timeOf(before, "expires_at"); ok && exp.Before(at) && !exp.Before(cur.start) {
			at = exp
		}
		cur.end = at
		out = append(out, *cur)
		cur = nil
	}
	for _, e := range events {
		if e.At.After(cutoff) {
			break
		}
		switch e.Op {
		case "claim.take":
			end(e.At, e.Before)
			cur = &hold{session: e.Session, principal: e.Principal, start: e.At}
		case "claim.release", "claim.expire":
			end(e.At, e.Before)
		case "claim.lapsed", "issue.close":
			end(e.At, nil)
		}
	}
	end(cutoff, nil)
	return out
}

// kind is a session's principal's kind as the record can tell it: a
// person's terminal, a CI job, or an agent.
func kind(session string) string {
	switch session {
	case proto.CLISession:
		return "person"
	case proto.CISession:
		return "service"
	default:
		return "agent"
	}
}

// brief is one `brief:` comment: its first line's fields and the hash of
// the brief after it.
type brief struct {
	at                              time.Time
	harness, model, effort, session string
	sha                             string
}

// briefs reads the `brief:` comments made by cutoff (dispatcher.md §7):
// a first line of key=value fields, then the brief as given, hashed by
// the rule the dispatcher's log uses (brief.Hash).
func briefs(comments []proto.Comment, cutoff time.Time) []brief {
	var out []brief
	for _, cm := range comments {
		if cm.CreatedAt.After(cutoff) {
			continue
		}
		r, text, ok := briefpkg.ParseComment(cm.Body)
		if !ok {
			continue
		}
		out = append(out, brief{at: cm.CreatedAt, harness: r.Harness, model: r.Model, effort: r.Effort,
			session: r.Session, sha: briefpkg.Hash(text)})
	}
	return out
}

// runs groups holds by session, in the order each session first took
// the claim, with the handoffs each made and the brief each was given.
func runs(hs []hold, events []proto.Event, bs []brief, cutoff time.Time) []RunRecord {
	var out []RunRecord
	index := map[string]int{}
	for _, h := range hs {
		i, ok := index[h.session]
		if !ok {
			i = len(out)
			index[h.session] = i
			out = append(out, RunRecord{Session: h.session, Kind: kind(h.session), FirstTake: h.start, Handoffs: []string{}})
		}
		out[i].HeldS += int64(h.end.Sub(h.start) / time.Second)
	}
	for _, e := range events {
		if e.At.After(cutoff) {
			break
		}
		if st, ok := handoffState(e, events); ok {
			if i, ok := index[e.Session]; ok {
				out[i].Handoffs = append(out[i].Handoffs, st)
			}
		}
	}
	// A brief naming a run's session goes to that run, the first such
	// for each; another for the same session goes nowhere. The briefs
	// left, naming no run's session, go to the agents' runs left, in
	// order.
	given := make([]bool, len(out))
	var rest []brief
	for _, b := range bs {
		i, ok := index[b.session]
		switch {
		case ok && out[i].Kind == "agent" && !given[i]:
			given[i] = true
			out[i].give(b)
		case !ok:
			rest = append(rest, b)
		}
	}
	for i := range out {
		if out[i].Kind != "agent" || given[i] || len(rest) == 0 {
			continue
		}
		given[i] = true
		out[i].give(rest[0])
		rest = rest[1:]
	}
	return out
}

// give records b as the brief r's run was given.
func (r *RunRecord) give(b brief) {
	r.Harness, r.Effort = b.harness, b.effort
	if b.model != "" {
		r.Models = []string{b.model}
	}
}

// handoffState is the state of a handoff event: its own, or done for a
// finish's handoff, which closes the issue in the same write.
func handoffState(e proto.Event, events []proto.Event) (string, bool) {
	if e.Op != "comment.add" {
		return "", false
	}
	var a struct {
		Kind    string `json:"kind"`
		Handoff struct {
			State string `json:"state"`
		} `json:"handoff"`
	}
	if json.Unmarshal(e.After, &a) != nil || a.Kind != "handoff" {
		return "", false
	}
	if a.Handoff.State != "" {
		return a.Handoff.State, true
	}
	if slices.ContainsFunc(events, func(c proto.Event) bool { return c.Op == "issue.close" && c.At.Equal(e.At) }) {
		return "done", true
	}
	return "", false
}

// record builds the record of outcome o. latest says it is the issue's
// latest outcome, the only one show's figures describe.
func (c *Capture) record(ctx context.Context, show proto.ShowResult, events []proto.Event, comments []proto.Comment,
	changes []Change, hostErr error, o outcomeAt, latest bool) Record {
	is := show.Issue
	r := Record{V: V, Issue: is.ID, Type: is.Type, Epic: is.Parent, Outcome: o.kind, Rev: is.Rev,
		CapturedAt: c.now().UTC().Truncate(time.Second), Capturer: Capturer, Backfill: c.Backfill,
		Runs: []RunRecord{}, Reopens: []Reopen{}, Missing: []Missing{},
		Waived: Waived{Items: []int{}, Gates: []string{}}}
	miss := func(field, reason string) { r.Missing = append(r.Missing, Missing{field, reason}) }
	cut := o.at
	var upTo []proto.Event
	for _, e := range events {
		if !e.At.After(cut) {
			upTo = append(upTo, e)
		}
	}

	// Runs, holds and times.
	hs := holds(upTo, cut)
	bs := briefs(comments, cut)
	r.Runs = runs(hs, upTo, bs, cut)
	for _, h := range hs {
		r.HeldS += int64(h.end.Sub(h.start) / time.Second)
	}
	r.Times = Times{Created: is.CreatedAt.UTC(), OutcomeAt: cut.UTC()}
	r.CycleS = int64(cut.Sub(is.CreatedAt) / time.Second)
	if len(hs) > 0 {
		r.Times.FirstStart = ptr(hs[0].start.UTC())
	}
	for _, e := range upTo {
		if st, ok := handoffState(e, upTo); ok && st == "done" {
			r.Times.FirstDone = ptr(e.At.UTC())
			break
		}
	}
	if len(bs) > 0 {
		r.Brief = &Brief{SHA256: bs[0].sha, From: "comment"}
	} else {
		miss("brief", "none")
	}
	if slices.ContainsFunc(r.Runs, func(x RunRecord) bool { return x.Kind == "agent" && x.Harness == "" }) {
		miss("harness", "none")
	}

	// Reopens, waivers, lessons and sweeps.
	waived := map[int]bool{}
	for _, e := range upTo {
		switch e.Op {
		case "issue.reopen":
			r.Reopens = append(r.Reopens, Reopen{At: e.At.UTC(), Reason: str(e.After, "reason")})
		case "acceptance.waive":
			if n, ok := intOf(e.After, "n"); ok {
				waived[n] = true
			}
		case "acceptance.untick", "acceptance.tick":
			if n, ok := intOf(e.After, "n"); ok {
				delete(waived, n)
			}
		}
	}
	for n := range waived {
		r.Waived.Items = append(r.Waived.Items, n)
	}
	slices.Sort(r.Waived.Items)
	for _, g := range append(slices.Clone(show.Gates), show.Checks...) {
		if g.Outcome == "waived" && (g.At == nil || !g.At.After(cut)) {
			name := g.Check
			if name == "" {
				name = g.Title
			}
			if !slices.Contains(r.Waived.Gates, name) {
				r.Waived.Gates = append(r.Waived.Gates, name)
			}
		}
	}
	if o.close != nil {
		for _, k := range []string{"acceptance_overridden", "checks_overridden", "reviews_overridden"} {
			if has(o.close.After, k) {
				r.Waived.Forced = true
			}
		}
	}
	sweeps := 0
	for _, cm := range comments {
		if cm.CreatedAt.After(cut) {
			continue
		}
		switch {
		case strings.HasPrefix(cm.Body, "lesson:"):
			r.Lessons++
		case strings.HasPrefix(cm.Body, "rework:"):
			sweeps++
		}
	}

	// The change and what its host says.
	end := cut
	var ch *Change
	switch {
	case o.change != nil:
		ch = o.change
	default:
		ch = changeFor(changes, cut)
	}
	var detail Detail
	hostOK := false
	switch {
	case c.Host == nil:
		for _, f := range []string{"change", "ci", "default_branch", "review"} {
			miss(f, "nohost")
		}
	case hostErr != nil:
		for _, f := range []string{"change", "ci", "default_branch", "review"} {
			miss(f, "host")
		}
	case ch == nil && o.kind == Landed:
		// Landed, and the host has no change for it: what the change
		// would have given is missing too.
		for _, f := range []string{"change", "ci", "default_branch", "review"} {
			miss(f, "none")
		}
	case ch == nil:
		// Closed with no change: the host answered, and there is none.
	default:
		r.Change = &ChangeRef{Host: c.Host.Name(), ID: ch.ID, URL: ch.URL, Base: ch.Base}
		r.Times.ChangeOpened = ptr(ch.Opened.UTC())
		if !ch.Merged.IsZero() {
			r.Times.Merged = ptr(ch.Merged.UTC())
			end = ch.Merged
		} else if !ch.Closed.IsZero() && ch.Closed.Before(end) {
			end = ch.Closed
		}
		d, err := c.Host.Detail(ctx, *ch)
		if err != nil {
			for _, f := range []string{"ci", "default_branch", "review"} {
				miss(f, "host")
			}
			break
		}
		detail, hostOK = d, true
		ci := ciOf(c.Host.Name(), d.Runs, d.Commits, end)
		r.CI = &ci
		r.Review = &Review{ChangesRequested: d.ChangesRequested, Comments: d.Comments}
		db := DefaultBranch{Sweeps: sweeps}
		ups := catchUps(d.Commits)
		db.Merges = len(ups)
		conflicts := c.Conflicts
		if conflicts == nil {
			conflicts = gitx.MergeConflicts
		}
		gitOK := d.Fetched
		for _, u := range ups {
			if !gitOK {
				break
			}
			bad, err := conflicts(ctx, c.Repo, u.Parents[0], u.Parents[1])
			if err != nil {
				gitOK = false
				break
			}
			if bad {
				db.Conflicted++
			}
		}
		r.DefaultBranch = &db
		switch {
		case d.Rewritten:
			miss("default_branch", "rewritten")
		case len(ups) > 0 && !gitOK:
			miss("default_branch", "git")
		}
	}
	if c.Host != nil && hostErr == nil && ch == nil && o.kind != Landed {
		// No change: no CI ran for it, nothing merged, nothing reviewed.
		r.CI = &CI{Host: c.Host.Name(), Failures: []Failure{}}
		r.DefaultBranch = &DefaultBranch{Sweeps: sweeps}
		r.Review = &Review{}
	}

	// Tokens and rework.
	tok, ok := c.tokens(ctx, is.ID, is.CreatedAt, cut)
	if ok != "" {
		miss("tokens", ok)
	} else {
		r.Tokens = &tok
		if latest && is.Status == "closed" && show.Usage != nil {
			r.Tokens.ByModel = show.Usage.Models
			r.Tokens.Split = r.Tokens.Split || show.Usage.Split
			r.Tokens.Capped = r.Tokens.Capped || show.Usage.Capped
		}
	}
	r.Rework.Attributed = "full"
	var from *time.Time
	if r.Times.FirstDone != nil {
		from, r.Rework.Trigger = r.Times.FirstDone, "done"
	}
	if r.CI != nil && len(r.CI.Failures) > 0 {
		f := r.CI.Failures[0].At
		if from == nil || f.Before(*from) {
			from, r.Rework.Trigger = &f, "ci"
		}
	}
	r.Rework.From = from
	if from != nil && from.Before(end) {
		pre, why := c.tokens(ctx, is.ID, *from, end)
		if why != "" {
			miss("rework", why)
		}
		r.Rework.PreMerge = pre.Tokens
		if hostOK {
			for _, cm := range detail.Commits {
				if cm.At.After(*from) && !cm.At.After(end) && !held(hs, cm.At) {
					r.Rework.Attributed = "partial"
				}
			}
		}
	}
	for _, e := range upTo {
		if e.Op == "issue.close" && e.At.Before(cut) {
			post, why := c.tokens(ctx, is.ID, e.At, cut)
			if why != "" {
				miss("rework", why)
			}
			r.Rework.PostClose = post.Tokens
			break
		}
	}
	slices.SortStableFunc(r.Missing, func(a, b Missing) int { return strings.Compare(a.Field, b.Field) })
	r.Missing = slices.CompactFunc(r.Missing, func(a, b Missing) bool { return a.Field == b.Field })
	return r
}

// changeFor is the change an outcome at t is about: the last merged by
// then, else the last opened by then.
func changeFor(changes []Change, t time.Time) *Change {
	var merged, opened *Change
	for i := range changes {
		ch := &changes[i]
		if !ch.Merged.IsZero() && !ch.Merged.After(t) && (merged == nil || ch.Merged.After(merged.Merged)) {
			merged = ch
		}
		if !ch.Opened.After(t) && (opened == nil || ch.Opened.After(opened.Opened)) {
			opened = ch
		}
	}
	if merged != nil {
		return merged
	}
	return opened
}

// held says a hold covered t.
func held(hs []hold, t time.Time) bool {
	return slices.ContainsFunc(hs, func(h hold) bool { return !t.Before(h.start) && !t.After(h.end) })
}

// tokens reads the issue's tokens in [since, until) from a cost report
// by issue. why is the reason the figure is missing, empty when it is
// not: "capped" when the window had more records than the server reads,
// "rest" when the issue was summed with the rest, "store" when the read
// failed.
func (c *Capture) tokens(ctx context.Context, id string, since, until time.Time) (Tokens, string) {
	res, err := c.Store.Cost(ctx, since, until)
	if err != nil {
		return Tokens{}, "store"
	}
	if res.Truncated {
		return Tokens{}, "capped"
	}
	for _, g := range res.Groups {
		if g.Key == id {
			return Tokens{Tokens: g.Tokens, Split: g.Split}, ""
		}
	}
	if slices.ContainsFunc(res.Groups, func(g proto.CostGroup) bool { return g.Key == proto.OtherModels }) {
		return Tokens{}, "rest"
	}
	return Tokens{}, ""
}

func (c *Capture) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func ptr[T any](v T) *T { return &v }

// str reads a string field of an event's state; "" when absent or null.
func str(raw json.RawMessage, key string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	var s string
	_ = json.Unmarshal(m[key], &s)
	return s
}

func intOf(raw json.RawMessage, key string) (int, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return 0, false
	}
	var n int
	if json.Unmarshal(m[key], &n) != nil {
		return 0, false
	}
	return n, true
}

func timeOf(raw json.RawMessage, key string) (time.Time, bool) {
	s := str(raw, key)
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

func has(raw json.RawMessage, key string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	v, ok := m[key]
	return ok && string(v) != "null"
}

// Plan says what to post for record r given the records already posted
// on its issue: "post" when none has its seq, "unchanged" when the latest
// with its seq says the same, and otherwise "supersede", naming the
// comment it supersedes. Records are never edited (§1).
func Plan(r Record, posted []Posted) (action, supersedes string) {
	var last *Posted
	for i := range posted {
		p := &posted[i]
		if p.Record.Seq == r.Seq && (last == nil || !p.At.Before(last.At)) {
			last = p
		}
	}
	if last == nil {
		return "post", ""
	}
	if same(r, last.Record) {
		return "unchanged", ""
	}
	return "supersede", last.Comment
}

// same compares two records but for what a capture always changes: its
// time, its capturer, the rev it read, what it supersedes and whether it
// was a backfill.
func same(a, b Record) bool {
	norm := func(r Record) string {
		r.CapturedAt, r.Capturer, r.Rev, r.Supersedes, r.Backfill = time.Time{}, "", 0, "", false
		bs, err := json.Marshal(r)
		if err != nil {
			panic(fmt.Sprintf("outcome: a record that does not encode: %v", err)) // a programming error tests catch
		}
		return string(bs)
	}
	return norm(a) == norm(b)
}
