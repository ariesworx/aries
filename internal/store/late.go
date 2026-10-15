package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"
)

// Late writes (design §18.3, §18.6). A finish or handoff written while
// the server could not be reached waits in the client's outbox, and may
// arrive after the lease of the claim it was made under ran out. If
// nobody took the issue since, it applies as it would have, and a
// claim.lapsed event, recorded first, says that the lease had lapsed.
// If somebody did, or the issue was closed since, it is not thrown away
// and not refused: its note is kept as a comment, with an issue.kept
// event, a finish's discovered issues are still created, and the
// principal who wrote it and the session that holds the issue now are
// told in their inboxes.
//
// Only a replay is treated so ([Store.ReplayFinish],
// [Store.ReplayHandoff]): the server is told so by the request (proto's
// park). A live finish or handoff is fenced as it always was (principle
// 4): a stale epoch is refused, and so is an issue another principal
// holds. A live one that applies after its lease lapsed records
// claim.lapsed too, since that is what happened.

// Events of late writes. claim.lapsed's before state is the claim the
// write was made under, as it was, and its after state names the op.
// issue.kept's after state names the op, the epoch it carried, why it
// did not apply, the comment it was kept as, and its reason and note.
const (
	OpClaimLapsed Op = "claim.lapsed"
	OpIssueKept   Op = "issue.kept"
)

// Finished is what a replayed finish came to: the issue, closed or as it
// was, and the discovered issues' IDs, in the order given. Lapsed says
// it applied after the claim's lease ran out; Comment, when set, is the
// comment it was kept as, because the issue had moved on.
type Finished struct {
	Issue
	IDs     []IssueID
	Lapsed  bool
	Comment string
}

// HandedOff is what a replayed handoff came to: the issue, and Lapsed
// and Comment as in [Finished].
type HandedOff struct {
	Issue
	Lapsed  bool
	Comment string
}

// ReplayFinish is [Store.FinishIssue] for a finish replayed from an
// outbox. Where FinishIssue refuses because the issue moved on (a later
// epoch than epoch, another principal holding it, or the issue closed),
// ReplayFinish keeps the finish as a comment, with its discovered issues
// created; it refuses everything else as FinishIssue does. A finish
// that applies after the claim at epoch lapsed says so (Lapsed). With an
// idempotency key, a repeat returns the first result, live or replayed.
func (s *Store) ReplayFinish(ctx context.Context, actor Actor, id IssueID, epoch int64, f Finish) (Finished, error) {
	out, err := s.finish(ctx, actor, id, epoch, f, true)
	if err != nil {
		return Finished{}, err
	}
	return Finished{Issue: out.Issue, IDs: out.IDs, Lapsed: out.Lapsed, Comment: out.Comment}, nil
}

// ReplayHandoff is [Store.HandoffIssue] for a handoff replayed from an
// outbox, kept as a comment where the issue moved on, as ReplayFinish
// keeps a finish. A handoff that does not release the claim is kept only
// when another principal holds the issue, or its epoch is stale.
func (s *Store) ReplayHandoff(ctx context.Context, actor Actor, id IssueID, epoch int64, h HandoffNote, release bool, idem string,
	paths []string) (HandedOff, error) {
	out, err := s.handoff(ctx, actor, id, epoch, h, release, idem, paths, true)
	if err != nil {
		return HandedOff{}, err
	}
	return HandedOff(out), nil
}

// moved says why a late write, made under the claim at epoch (0 for
// none) on before, whose claim is now c, no longer applies, or "" when
// it does: the claim was taken since, another principal holds it, or,
// for a write that ends the claim (closing), the issue was closed.
func moved(w *wtx, before Issue, c claimRow, epoch int64, ends bool) string {
	switch {
	case ends && before.Status == StatusClosed:
		return "the issue was closed since"
	case epoch != 0 && epoch < c.Epoch && c.active(w.now):
		return fmt.Sprintf("the claim was taken since, by %s/%s at epoch %d", c.Holder.Principal, c.Holder.Session, c.Epoch)
	case epoch != 0 && epoch < c.Epoch:
		return fmt.Sprintf("the claim was taken since, at epoch %d", c.Epoch)
	case c.active(w.now) && c.Holder.Principal != w.actor.Principal && !w.admin:
		return fmt.Sprintf("%s/%s holds it", c.Holder.Principal, c.Holder.Session)
	}
	return ""
}

// markLapsed records claim.lapsed when a write made under the claim c
// applies though c is no longer live because its lease ran out, whether
// or not the reaper got to it, and nobody took the issue since. The
// write was made under c when it names c's epoch, or, naming none, when
// c was its actor's principal's. A claim its holder let go, by a
// releasing handoff, did not lapse. It reports whether it recorded it.
func markLapsed(ctx context.Context, w *wtx, c claimRow, epoch int64, op string) (bool, error) {
	if !c.exists || c.active(w.now) || epoch != 0 && epoch != c.Epoch {
		return false, nil
	}
	holder := c.Holder
	if holder.Principal == "" {
		// Released: by the reaper, or by its holder or someone else.
		var err error
		if holder, err = reapedHolder(ctx, w, c); err != nil || holder.Principal == "" {
			return false, err
		}
	}
	if epoch == 0 && holder.Principal != w.actor.Principal {
		return false, nil
	}
	before := map[string]any{"epoch": c.Epoch}
	if c.Holder.Principal != "" {
		before["holder"], before["expires_at"] = c.Holder, c.ExpiresAt
	}
	return true, w.event(ctx, OpClaimLapsed, string(c.Issue), before, map[string]any{"op": op})
}

// reapedHolder returns who held c, a claim with no holder now, when the
// reaper ended it as it lapsed, and the zero Actor when anything else
// ended it: the last event that ended a claim of its issue is then not
// the reaper's claim.expire of c's epoch.
func reapedHolder(ctx context.Context, w *wtx, c claimRow) (Actor, error) {
	var op string
	var before []byte
	err := w.tx.QueryRowContext(ctx, `SELECT op, before_state FROM events WHERE target = $1 AND op IN ($2, $3)
  ORDER BY seq DESC LIMIT 1`, string(c.Issue), string(OpClaimExpire), string(OpClaimRelease)).Scan(&op, &before)
	if errors.Is(err, sql.ErrNoRows) || err == nil && Op(op) != OpClaimExpire {
		return Actor{}, nil
	}
	if err != nil {
		return Actor{}, fmt.Errorf("last end of the claim on %s: %w", c.Issue, err)
	}
	var b struct {
		Holder Actor `json:"holder"`
		Epoch  int64 `json:"epoch"`
	}
	if json.Unmarshal(before, &b) != nil || b.Epoch != c.Epoch {
		return Actor{}, nil
	}
	return b.Holder, nil
}

// lateText is the part of a late write kept as a comment: its note and
// what else it carried, a line each.
type lateText struct {
	lines []string
	// reason and note are recorded in issue.kept as well.
	reason, note string
}

// finishText is what of f a kept finish keeps.
func finishText(f Finish) lateText {
	t := lateText{reason: f.Reason, note: f.Handoff.Note}
	if f.Reason != "" {
		t.lines = append(t.lines, "reason: "+f.Reason)
	}
	if f.Handoff.Note != "" {
		t.lines = append(t.lines, handoffText(f.Handoff).lines...)
	}
	if len(f.Accept.Tick) > 0 {
		t.lines = append(t.lines, fmt.Sprintf("ticked: %v", f.Accept.Tick))
	}
	for _, n := range slices.Sorted(maps.Keys(f.Accept.Waive)) {
		t.lines = append(t.lines, fmt.Sprintf("waived %d: %s", n, f.Accept.Waive[n]))
	}
	return t
}

// handoffText is what of h a kept handoff keeps.
func handoffText(h HandoffNote) lateText {
	t := lateText{note: h.Note}
	for _, kv := range [][2]string{{"state", string(h.State)}, {"next", h.Next}, {"branch", h.Branch},
		{"worktree", h.Worktree}, {"to", h.To}} {
		if kv[1] != "" {
			t.lines = append(t.lines, kv[0]+": "+kv[1])
		}
	}
	t.lines = append(t.lines, "", h.Note)
	return t
}

// keepLate keeps op, a late write made under the claim at epoch on
// before, as a comment by w's actor, because of why, with its issue.kept
// event, and tells the actor's principal, in every session, and the
// session that holds the issue now. It returns the comment's id.
func keepLate(ctx context.Context, w *wtx, before Issue, c claimRow, epoch int64, op, why string, t lateText) (string, error) {
	made := fmt.Sprintf("made at epoch %d", epoch)
	if epoch == 0 {
		made = "made with no epoch"
	}
	body := fmt.Sprintf("%s %s, delivered late, kept as a comment: %s.\n\n%s",
		strings.ToUpper(op[:1])+op[1:], made, why, strings.Join(t.lines, "\n"))
	cm, err := insertComment(ctx, w, before.ID, cutText(body), CommentPlain)
	if err != nil {
		return "", err
	}
	if err := w.event(ctx, OpIssueKept, string(before.ID), nil, map[string]any{"op": op, "epoch": epoch, "why": why,
		"comment": cm.ID, "reason": t.reason, "note": t.note}); err != nil {
		return "", err
	}
	if err := w.notify(ctx, InboxItem{To: w.actor.Principal, Kind: InboxConflict, Issue: before.ID,
		Body: fmt.Sprintf("your %s %s, delivered late, was kept as comment %s: %s", op, made, cm.ID, why)}); err != nil {
		return "", err
	}
	if !c.active(w.now) || c.Holder == w.actor {
		return cm.ID, nil
	}
	return cm.ID, w.notify(ctx, InboxItem{To: c.Holder.Principal, Session: c.Holder.Session, Kind: InboxConflict, Issue: before.ID,
		Body: fmt.Sprintf("%s's %s %s, delivered late, was kept as comment %s on the issue you hold", w.actor.Principal, op, made, cm.ID)})
}

// cutText cuts body to the longest a comment may be, marking the cut.
func cutText(body string) string {
	if len(body) <= maxText {
		return body
	}
	const cut = "\n[cut]"
	n := maxText - len(cut)
	for n > 0 && !utf8.RuneStart(body[n]) {
		n--
	}
	return body[:n] + cut
}
