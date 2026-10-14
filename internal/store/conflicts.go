package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Conflicts (design §18.7). An update queued while the server could not
// be reached carries the rev it was based on, and may arrive after
// others changed the issue. Replayed, it is a merge ([Store.MergeIssue]):
// each field it sets is compared with the issue's state at that rev,
// which the event log gives (the rev each change of the issue's row left
// is on its event, revEvent). A field nobody changed since applies; a
// field somebody did change is parked, as a row of the conflicts table
// with base, mine and theirs, and the server's value stays until a
// person resolves it. Status and assignee, which the claim decides, are
// the server's; an issue closed since keeps the update as a comment.
// Long text parks whole: there is no merge of hunks (decision 43).
//
// An agent may propose a resolution, kept with the conflict; a person
// approves it ([Store.ResolveConflict] with [ChoiceAccept]) or resolves
// otherwise (decision 42). A later change to a field closes its open
// conflicts (supersede). Every one of these is a write through
// Store.write with its event, guarded like any change to the issue.

// Conflict events.
const (
	OpConflictPark    Op = "conflict.park"
	OpConflictPropose Op = "conflict.propose"
	OpConflictResolve Op = "conflict.resolve"
)

// Choice is how a conflict is resolved, or proposed to be.
type Choice string

// Choices. ChoiceAccept resolves with the conflict's proposal, and is
// not itself a proposal.
const (
	ChoiceMine   Choice = "mine"
	ChoiceTheirs Choice = "theirs"
	ChoiceText   Choice = "text"
	ChoiceAccept Choice = "accept"
)

// ResolvedSuperseded is the resolution of a conflict closed by a later
// change to its field.
const ResolvedSuperseded = "superseded"

// DefaultConflictAge is how long a conflict is open before a digest
// lists it, when it is given no age.
const DefaultConflictAge = 24 * time.Hour

// Conflict bounds.
const (
	// DefaultConflictLimit and MaxConflictLimit bound one Conflicts read.
	DefaultConflictLimit = 20
	MaxConflictLimit     = 100
	// maxWhy bounds a proposal's reason, in bytes.
	maxWhy = 2000
)

// Proposal is a suggested resolution of a conflict.
type Proposal struct {
	Choice Choice    `json:"choice"`
	Text   string    `json:"text,omitempty"`
	Why    string    `json:"why,omitempty"`
	By     string    `json:"by"`
	At     time.Time `json:"at"`
}

// Conflict is one parked field. Base is the field's value at BaseRev,
// Mine the update's, and Theirs the server's when it was parked, made by
// TheirsBy at TheirsAt. Author and Session made the update. A long Base
// may be the event log's shortened copy (EventTextMax); Mine and Theirs
// are whole. Resolution, ResolvedBy and ResolvedAt are set once it is
// resolved. Advice is computed when it is read, and is never applied by
// itself.
type Conflict struct {
	ID                 int64
	Issue              IssueID
	Field              string
	BaseRev            Rev
	Base, Mine, Theirs string
	Author, Session    string
	TheirsBy           string
	TheirsAt           time.Time
	ParkedAt           time.Time
	Proposal           *Proposal
	Resolution         string
	ResolvedBy         string
	ResolvedAt         *time.Time
	Advice             string
}

// Parked names a field a merge parked, and its conflict.
type Parked struct {
	Field    string `json:"field"`
	Conflict int64  `json:"conflict"`
}

// Merged is what [Store.MergeIssue] did: the issue after it, the fields
// it parked, the fields the server kept (Dropped), and the comment the
// update was kept as when the issue was closed since its base (Comment).
type Merged struct {
	Issue
	Parked  []Parked `json:"parked,omitempty"`
	Dropped []string `json:"dropped,omitempty"`
	Comment string   `json:"comment,omitempty"`
}

// ConflictLimitError refuses an update that would park past a cap on
// open conflicts: Limit is conflicts_per_issue or
// conflicts_per_principal. It wraps ErrInvalid.
type ConflictLimitError struct {
	Limit string
	Max   int
	// Issue or Principal is what holds the conflicts.
	Issue     IssueID
	Principal string
}

func (e *ConflictLimitError) Error() string {
	if e.Limit == "conflicts_per_issue" {
		return fmt.Sprintf("%v: %s has %d open conflicts, the most an issue holds", ErrInvalid, e.Issue, e.Max)
	}
	return fmt.Sprintf("%v: %s has %d open conflicts, the most a principal holds", ErrInvalid, e.Principal, e.Max)
}

// Unwrap makes errors.Is(err, ErrInvalid) hold.
func (e *ConflictLimitError) Unwrap() error { return ErrInvalid }

// fieldSpec is a field a merge compares, as text: the update's value and
// whether it sets one, the issue's, and how to set it in a patch.
// serverWins fields are never parked: a change since the base keeps the
// server's (design §8, "status and claim").
type fieldSpec struct {
	mine       func(IssuePatch) (string, bool)
	theirs     func(Issue) string
	set        func(*IssuePatch, string) error
	clear      func(*IssuePatch)
	serverWins bool
}

// textField is a fieldSpec for a string field of the patch.
func textField(get func(*IssuePatch) **string, cur func(Issue) string) fieldSpec {
	return fieldSpec{
		mine: func(p IssuePatch) (string, bool) {
			v := *get(&p)
			if v == nil {
				return "", false
			}
			return *v, true
		},
		theirs: cur,
		set:    func(p *IssuePatch, v string) error { *get(p) = &v; return nil },
		clear:  func(p *IssuePatch) { *get(p) = nil },
	}
}

// conflictFields are the fields a merge compares, by their names in the
// event log; fieldOrder is the order it goes through them in.
var (
	conflictFields = map[string]fieldSpec{
		"title":      textField(func(p *IssuePatch) **string { return &p.Title }, func(is Issue) string { return is.Title }),
		"body":       textField(func(p *IssuePatch) **string { return &p.Body }, func(is Issue) string { return is.Body }),
		"design":     textField(func(p *IssuePatch) **string { return &p.Design }, func(is Issue) string { return is.Design }),
		"acceptance": textField(func(p *IssuePatch) **string { return &p.Acceptance }, func(is Issue) string { return is.Acceptance }),
		"notes":      textField(func(p *IssuePatch) **string { return &p.Notes }, func(is Issue) string { return is.Notes }),
		"owner":      textField(func(p *IssuePatch) **string { return &p.Owner }, func(is Issue) string { return is.Owner }),
		"account":    textField(func(p *IssuePatch) **string { return &p.Account }, func(is Issue) string { return is.Account }),
		"priority": {
			mine: func(p IssuePatch) (string, bool) {
				if p.Priority == nil {
					return "", false
				}
				return strconv.Itoa(int(*p.Priority)), true
			},
			theirs: func(is Issue) string { return strconv.Itoa(int(is.Priority)) },
			set: func(p *IssuePatch, v string) error {
				n, err := strconv.Atoi(v)
				if err != nil {
					return fmt.Errorf("%w: priority %q is not a number from 0 to 4", ErrInvalid, v)
				}
				pr := Priority(n)
				p.Priority = &pr
				return nil
			},
			clear: func(p *IssuePatch) { p.Priority = nil },
		},
		"type": {
			mine: func(p IssuePatch) (string, bool) {
				if p.Type == nil {
					return "", false
				}
				return string(*p.Type), true
			},
			theirs: func(is Issue) string { return string(is.Type) },
			set:    func(p *IssuePatch, v string) error { t := IssueType(v); p.Type = &t; return nil },
			clear:  func(p *IssuePatch) { p.Type = nil },
		},
		"parent_id": {
			mine: func(p IssuePatch) (string, bool) {
				if p.ParentID == nil {
					return "", false
				}
				return string(*p.ParentID), true
			},
			theirs: func(is Issue) string { return string(is.ParentID) },
			set:    func(p *IssuePatch, v string) error { id := IssueID(v); p.ParentID = &id; return nil },
			clear:  func(p *IssuePatch) { p.ParentID = nil },
		},
		"status": {
			mine: func(p IssuePatch) (string, bool) {
				if p.Status == nil {
					return "", false
				}
				return string(*p.Status), true
			},
			theirs:     func(is Issue) string { return string(is.Status) },
			clear:      func(p *IssuePatch) { p.Status = nil },
			serverWins: true,
		},
		"assignee": {
			mine:       textField(func(p *IssuePatch) **string { return &p.Assignee }, nil).mine,
			theirs:     func(is Issue) string { return is.Assignee },
			clear:      func(p *IssuePatch) { p.Assignee = nil },
			serverWins: true,
		},
	}
	fieldOrder = []string{"title", "body", "design", "acceptance", "notes", "status", "priority", "type",
		"assignee", "owner", "parent_id", "account"}
)

// stateText is a field's value in an event's state as text: a string as
// it is, a number in decimal, and null or absent as "".
func stateText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	b, _ := json.Marshal(v) // decoded from JSON, so it encodes
	return string(b)
}

// change is a field changed since a merge's base: its value then, and
// who last changed it and when.
type change struct {
	base string
	by   string
	at   time.Time
}

// changedSince returns the fields of issue id that changed after rev
// base, from the event log, and whether its declared paths did. A base
// older than the issue's first event that carries a rev cannot be
// compared, and is ErrConflict, as a stale update is.
func changedSince(ctx context.Context, q querier, id IssueID, base Rev) (map[string]change, bool, error) {
	var from int64
	err := q.QueryRowContext(ctx, `SELECT seq FROM events WHERE target = ? AND rev IS NOT NULL AND rev <= ?
  ORDER BY seq DESC LIMIT 1`, string(id), int64(base)).Scan(&from)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, fmt.Errorf("issue %s: rev %d is older than the history a replayed update is compared with: %w", id, base, ErrConflict)
	}
	if err != nil {
		return nil, false, fmt.Errorf("history of %s: %w", id, err)
	}
	rows, err := q.QueryContext(ctx, `SELECT principal, at, op, before_state, after_state, rev FROM events
  WHERE target = ? AND seq > ? ORDER BY seq`, string(id), from)
	if err != nil {
		return nil, false, fmt.Errorf("history of %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	changed := map[string]change{}
	paths := false
	for rows.Next() {
		var (
			by            string
			at            time.Time
			op            Op
			before, after []byte
			rev           sql.NullInt64
		)
		if err := rows.Scan(&by, &at, &op, &before, &after, &rev); err != nil {
			return nil, false, fmt.Errorf("history of %s: %w", id, err)
		}
		if op == OpIssuePaths {
			var p struct {
				Source PathSource `json:"source"`
			}
			paths = paths || json.Unmarshal(after, &p) == nil && p.Source == PathDeclared
			continue
		}
		if !rev.Valid || len(before) == 0 {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal(before, &fields); err != nil {
			return nil, false, fmt.Errorf("history of %s: %w", id, err)
		}
		for k, v := range fields {
			if _, ok := conflictFields[k]; !ok {
				continue
			}
			c, seen := changed[k]
			if !seen {
				c.base = stateText(v)
			}
			c.by, c.at = by, at.UTC()
			changed[k] = c
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("history of %s: %w", id, err)
	}
	return changed, paths, nil
}

// MergeIssue applies patch, an update based on rev base, as a replayed
// update is applied (design §18.7): at the issue's rev it is
// [Store.UpdateIssue]; at a later one, each field patch sets is compared
// with the issue's state at base. A field nobody changed since applies,
// and so does one whose value is the patch's, or is back at base's. A
// changed status or assignee keeps the server's, listed in Dropped, as
// do declared paths changed since. Any other changed field is parked as
// a [Conflict], with a conflict.park event, and told to the author's
// inbox and to the claim holder's when that is another principal. An
// issue closed since base takes nothing, and the update is kept as a
// comment.
//
// It is guarded as an update is: an issue another principal holds is
// refused with a [*ForbiddenError] unless the actor is an admin. Parking
// past conflicts_per_issue or conflicts_per_principal is refused with a
// [*ConflictLimitError], and nothing of the update applies. A base ahead
// of the issue, or older than its history can say, is ErrConflict. The
// fields a replayed update cannot carry (the times, flags and metadata
// no client sends) are refused with ErrInvalid. With an idempotency key,
// a repeat returns the first result, also one [Store.UpdateIssue] gave.
func (s *Store) MergeIssue(ctx context.Context, actor Actor, id IssueID, base Rev, patch IssuePatch) (Merged, error) {
	if err := id.Validate(); err != nil {
		return Merged{}, err
	}
	if base < 1 {
		return Merged{}, fmt.Errorf("%w: expected rev is required", ErrInvalid)
	}
	if err := validIdem(patch.IdempotencyKey); err != nil {
		return Merged{}, err
	}
	if patch.DueAt != nil || patch.DeferUntil != nil || patch.ExpiresAt != nil || patch.Ephemeral != nil ||
		patch.Pinned != nil || patch.Template != nil || patch.Metadata != nil {
		return Merged{}, fmt.Errorf("%w: a replayed update carries only the fields a client sends", ErrInvalid)
	}
	pp, err := s.preparePatch(patch)
	if err != nil {
		return Merged{}, err
	}
	var out Merged
	err = s.write(ctx, actor, func(w *wtx) error {
		out = Merged{}
		if done, err := replay(ctx, w, patch.IdempotencyKey, "update", updateRequest{id, base, patch}, &out); done || err != nil {
			return err
		}
		before, err := loadIssue(ctx, w.tx, id)
		if err != nil {
			return err
		}
		switch {
		case before.Rev == base:
			if out.Issue, err = applyPatch(ctx, w, before, pp); err != nil {
				return err
			}
			return w.settleChanged(out)
		case before.Rev < base:
			return fmt.Errorf("issue %s at rev %d, not %d: %w", id, before.Rev, base, ErrConflict)
		}
		changed, paths, err := changedSince(ctx, w.tx, id, base)
		if err != nil {
			return err
		}
		if _, ok := changed["status"]; ok && before.Status == StatusClosed {
			return keepAsComment(ctx, w, before, base, patch, &out)
		}
		apply := patch
		var parks []Conflict
		for _, name := range fieldOrder {
			spec := conflictFields[name]
			mine, ok := spec.mine(patch)
			if !ok {
				continue
			}
			c, was := changed[name]
			theirs := spec.theirs(before)
			if !was || mine == theirs || theirs == c.base {
				continue
			}
			spec.clear(&apply)
			if spec.serverWins {
				out.Dropped = append(out.Dropped, name)
				continue
			}
			parks = append(parks, Conflict{Issue: id, Field: name, BaseRev: base, Base: c.base, Mine: mine, Theirs: theirs,
				TheirsBy: c.by, TheirsAt: c.at})
		}
		if patch.Paths != nil && paths {
			apply.Paths = nil
			out.Dropped = append(out.Dropped, "paths")
		}
		if len(parks) > 0 {
			if err := checkConflictCaps(ctx, w, id, len(parks)); err != nil {
				return err
			}
		}
		app, err := s.preparePatch(apply)
		if err != nil {
			return err
		}
		if len(app.sets) == 0 && apply.Paths == nil && len(parks) > 0 {
			// Nothing applies, so applyPatch guards nothing: parking is
			// a change to the issue's record all the same.
			cl, err := loadClaim(ctx, w.tx, id)
			if err != nil {
				return err
			}
			if err := w.guard(ctx, cl, "update"); err != nil {
				return err
			}
		}
		if out.Issue, err = applyPatch(ctx, w, before, app); err != nil {
			return err
		}
		for _, c := range parks {
			cid, err := park(ctx, w, c, patch.IdempotencyKey)
			if err != nil {
				return err
			}
			out.Parked = append(out.Parked, Parked{Field: c.Field, Conflict: cid})
		}
		if len(parks) > 0 {
			if err := tellParked(ctx, w, id, base, out.Parked); err != nil {
				return err
			}
		}
		return w.settleChanged(out)
	})
	if err != nil {
		return Merged{}, err
	}
	return out, nil
}

// checkConflictCaps refuses parking n more conflicts on issue id, by w's
// actor, past the caps on open conflicts.
func checkConflictCaps(ctx context.Context, w *wtx, id IssueID, n int) error {
	var onIssue, byAuthor int
	if err := w.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM conflicts WHERE issue_id = ? AND resolved_at IS NULL`,
		string(id)).Scan(&onIssue); err != nil {
		return fmt.Errorf("count conflicts: %w", err)
	}
	if onIssue+n > w.lim.ConflictsPerIssue {
		return &ConflictLimitError{Limit: "conflicts_per_issue", Max: w.lim.ConflictsPerIssue, Issue: id}
	}
	if err := w.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM conflicts WHERE author = ? AND resolved_at IS NULL`,
		w.actor.Principal).Scan(&byAuthor); err != nil {
		return fmt.Errorf("count conflicts: %w", err)
	}
	if byAuthor+n > w.lim.ConflictsPerPrincipal {
		return &ConflictLimitError{Limit: "conflicts_per_principal", Max: w.lim.ConflictsPerPrincipal, Principal: w.actor.Principal}
	}
	return nil
}

// park writes c, by w's actor, as an open conflict with its event, and
// returns its id.
func park(ctx context.Context, w *wtx, c Conflict, idem string) (int64, error) {
	last, err := lastKey(ctx, w.tx, `SELECT id FROM conflicts ORDER BY id DESC LIMIT 1`)
	if err != nil {
		return 0, fmt.Errorf("next conflict id: %w", err)
	}
	cid := last + 1
	if _, err := w.exec(ctx, `INSERT INTO conflicts (id, issue_id, field, base_rev, base, mine, theirs, author, session,
  idem_key, theirs_by, theirs_at, parked_at, write_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		cid, string(c.Issue), c.Field, int64(c.BaseRev), c.Base, c.Mine, c.Theirs, w.actor.Principal, w.actor.Session,
		nullStr(idem), c.TheirsBy, c.TheirsAt, w.now, randomInt63()); err != nil {
		return 0, fmt.Errorf("park conflict: %w", err)
	}
	return cid, w.event(ctx, OpConflictPark, string(c.Issue), nil, map[string]any{
		"conflict": cid, "field": c.Field, "base_rev": c.BaseRev, "base": c.Base, "mine": c.Mine, "theirs": c.Theirs,
		"theirs_by": c.TheirsBy,
	})
}

// tellParked tells the author of the parked fields, w's actor, in every
// session, and the claim holder when another principal holds the issue.
func tellParked(ctx context.Context, w *wtx, id IssueID, base Rev, parked []Parked) error {
	fields := make([]string, len(parked))
	for i, p := range parked {
		fields[i] = p.Field
	}
	what := strings.Join(fields, ", ")
	if err := w.notify(ctx, InboxItem{To: w.actor.Principal, Kind: InboxConflict, Issue: id,
		Body: fmt.Sprintf("your update from rev %d parked %s: someone changed it since; a person resolves it (sfx conflicts %s)", base, what, id)}); err != nil {
		return err
	}
	c, err := loadClaim(ctx, w.tx, id)
	if err != nil || !c.active(w.now) || c.Holder.Principal == w.actor.Principal {
		return err
	}
	return w.notify(ctx, InboxItem{To: c.Holder.Principal, Session: c.Holder.Session, Kind: InboxConflict, Issue: id,
		Body: fmt.Sprintf("%s's update from rev %d parked %s against the issue you hold (sfx conflicts %s)", w.actor.Principal, base, what, id)})
}

// keepAsComment keeps patch, an update from rev base of before, which
// was closed since, as a comment by w's actor (close wins over edit,
// design §8), and tells the author.
func keepAsComment(ctx context.Context, w *wtx, before Issue, base Rev, patch IssuePatch, out *Merged) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Update from rev %d, kept as a comment: the issue was closed since.", base)
	for _, name := range fieldOrder {
		if v, ok := conflictFields[name].mine(patch); ok {
			fmt.Fprintf(&b, "\n%s: %s", name, v)
		}
	}
	if patch.Paths != nil {
		fmt.Fprintf(&b, "\npaths: %s", strings.Join(*patch.Paths, " "))
	}
	body := b.String()
	if len(body) > maxText {
		const cut = "\n[cut]"
		n := maxText - len(cut)
		for n > 0 && !utf8.RuneStart(body[n]) {
			n--
		}
		body = body[:n] + cut
	}
	c, err := insertComment(ctx, w, before.ID, body, CommentPlain)
	if err != nil {
		return err
	}
	*out = Merged{Issue: before, Comment: c.ID}
	if err := w.notify(ctx, InboxItem{To: w.actor.Principal, Kind: InboxConflict, Issue: before.ID,
		Body: fmt.Sprintf("your update from rev %d was kept as comment %s: the issue was closed since", base, c.ID)}); err != nil {
		return err
	}
	return w.settle(*out)
}

// supersede closes the open conflicts on issue id's fields that changed,
// the before state of a change to its row: a later change to a field
// decides it (design §18.7), recorded as its conflict.resolve.
func supersede(ctx context.Context, w *wtx, id IssueID, changed map[string]any) error {
	var fields []any
	for k := range changed {
		if spec, ok := conflictFields[k]; ok && !spec.serverWins {
			fields = append(fields, k)
		}
	}
	if len(fields) == 0 {
		return nil
	}
	in := placeholders(len(fields))
	query := `SELECT id, field FROM conflicts WHERE issue_id = ? AND resolved_at IS NULL AND field IN (` + in + `) ORDER BY id` //nolint:gosec // placeholders only; the values are arguments
	rows, err := w.tx.QueryContext(ctx, query, append([]any{string(id)}, fields...)...)
	if err != nil {
		return fmt.Errorf("open conflicts of %s: %w", id, err)
	}
	type open struct {
		id    int64
		field string
	}
	var found []open
	for rows.Next() {
		var o open
		if err := rows.Scan(&o.id, &o.field); err != nil {
			_ = rows.Close()
			return fmt.Errorf("open conflicts of %s: %w", id, err)
		}
		found = append(found, o)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("open conflicts of %s: %w", id, err)
	}
	for _, o := range found {
		if err := markResolved(ctx, w, o.id, ResolvedSuperseded); err != nil {
			return err
		}
		if err := w.event(ctx, OpConflictResolve, string(id), nil,
			map[string]any{"conflict": o.id, "field": o.field, "choice": ResolvedSuperseded}); err != nil {
			return err
		}
	}
	return nil
}

// markResolved resolves the open conflict id as resolution, by w's
// actor.
func markResolved(ctx context.Context, w *wtx, id int64, resolution string) error {
	n, err := w.exec(ctx, `UPDATE conflicts SET resolution = ?, resolved_by = ?, resolved_at = ?, write_id = ?
  WHERE id = ? AND resolved_at IS NULL`, resolution, w.actor.Principal, w.now, randomInt63(), id)
	if err != nil {
		return fmt.Errorf("resolve conflict %d: %w", id, err)
	}
	if n != 1 {
		return fmt.Errorf("conflict %d changed during the write: %w", id, ErrConflict)
	}
	return nil
}

// ResolveConflict resolves the open conflict id: with mine, the update's
// value; with theirs, the server's, as it is; with text, text; with
// accept, as its proposal says. The field's change is an update of the
// issue, guarded and checked as [Store.UpdateIssue]'s is, and the
// conflict.resolve event says which choice decided it. A conflict that
// does not exist is ErrNotFound; one already resolved, accept with no
// proposal, text given with a choice other than text, and text the field
// does not take are ErrInvalid.
func (s *Store) ResolveConflict(ctx context.Context, actor Actor, id int64, choice Choice, text string) (Issue, error) {
	switch choice {
	case ChoiceMine, ChoiceTheirs, ChoiceAccept:
		if text != "" {
			return Issue{}, fmt.Errorf("%w: text goes only with the choice text", ErrInvalid)
		}
	case ChoiceText:
	default:
		return Issue{}, fmt.Errorf("%w: choice %q: give mine, theirs, text or accept", ErrInvalid, choice)
	}
	var out Issue
	err := s.write(ctx, actor, func(w *wtx) error {
		c, err := loadConflict(ctx, w.tx, id)
		if err != nil {
			return err
		}
		if c.ResolvedAt != nil {
			return fmt.Errorf("%w: conflict %d is already resolved (%s)", ErrInvalid, id, c.Resolution)
		}
		pick, value := choice, text
		if choice == ChoiceAccept {
			if c.Proposal == nil {
				return fmt.Errorf("%w: conflict %d has no proposal to accept", ErrInvalid, id)
			}
			pick, value = c.Proposal.Choice, c.Proposal.Text
		}
		if pick == ChoiceMine {
			value = c.Mine
		}
		before, err := loadIssue(ctx, w.tx, c.Issue)
		if err != nil {
			return err
		}
		if err := markResolved(ctx, w, id, string(pick)); err != nil {
			return err
		}
		spec := conflictFields[c.Field]
		out = before
		if pick == ChoiceTheirs || value == spec.theirs(before) {
			cl, err := loadClaim(ctx, w.tx, c.Issue)
			if err != nil {
				return err
			}
			if err := w.guard(ctx, cl, "resolve"); err != nil {
				return err
			}
		} else {
			pp, err := fieldPatch(s, spec, value)
			if err != nil {
				return err
			}
			if out, err = applyPatch(ctx, w, before, pp); err != nil {
				return err
			}
		}
		return w.event(ctx, OpConflictResolve, string(c.Issue), nil, map[string]any{
			"conflict": id, "field": c.Field, "choice": pick, "accepted": choice == ChoiceAccept,
		})
	})
	if err != nil {
		return Issue{}, err
	}
	return out, nil
}

// fieldPatch is the patch that sets spec's field to v, checked.
func fieldPatch(s *Store, spec fieldSpec, v string) (prepared, error) {
	var p IssuePatch
	if err := spec.set(&p, v); err != nil {
		return prepared{}, err
	}
	return s.preparePatch(p)
}

// ProposeResolution keeps a suggested resolution of the open conflict
// id, with why, on the conflict, replacing any earlier one, with a
// conflict.propose event. It changes nothing else: a person accepts it
// with [Store.ResolveConflict], or resolves otherwise (decision 42). It
// is guarded like a change to the issue. choice is mine, theirs or text,
// and text, which only text takes, must be a value the field takes;
// otherwise, and for a conflict already resolved, it is ErrInvalid. A
// conflict that does not exist is ErrNotFound.
func (s *Store) ProposeResolution(ctx context.Context, actor Actor, id int64, choice Choice, text, why string) (Conflict, error) {
	switch choice {
	case ChoiceMine, ChoiceTheirs:
		if text != "" {
			return Conflict{}, fmt.Errorf("%w: text goes only with the choice text", ErrInvalid)
		}
	case ChoiceText:
	default:
		return Conflict{}, fmt.Errorf("%w: choice %q: propose mine, theirs or text", ErrInvalid, choice)
	}
	if err := checkText("why", why, maxWhy, false); err != nil {
		return Conflict{}, err
	}
	var out Conflict
	err := s.write(ctx, actor, func(w *wtx) error {
		c, err := loadConflict(ctx, w.tx, id)
		if err != nil {
			return err
		}
		if c.ResolvedAt != nil {
			return fmt.Errorf("%w: conflict %d is already resolved (%s)", ErrInvalid, id, c.Resolution)
		}
		if choice == ChoiceText {
			if _, err := fieldPatch(s, conflictFields[c.Field], text); err != nil {
				return err
			}
		}
		cl, err := loadClaim(ctx, w.tx, c.Issue)
		if err != nil {
			return err
		}
		if err := w.guard(ctx, cl, "propose"); err != nil {
			return err
		}
		if _, err := w.exec(ctx, `UPDATE conflicts SET proposal = ?, proposal_text = ?, proposal_why = ?, proposed_by = ?,
  proposed_at = ?, write_id = ? WHERE id = ?`, string(choice), text, why, w.actor.Principal, w.now, randomInt63(), id); err != nil {
			return fmt.Errorf("propose for conflict %d: %w", id, err)
		}
		if err := w.event(ctx, OpConflictPropose, string(c.Issue), nil, map[string]any{
			"conflict": id, "field": c.Field, "choice": choice, "text": text, "why": why,
		}); err != nil {
			return err
		}
		out, err = loadConflict(ctx, w.tx, id)
		return err
	})
	if err != nil {
		return Conflict{}, err
	}
	return out, nil
}

// conflictCols are the columns scanConflict reads, in its order.
const conflictCols = `id, issue_id, field, base_rev, base, mine, theirs, author, session, theirs_by, theirs_at,
  parked_at, proposal, proposal_text, proposal_why, proposed_by, proposed_at, resolution, resolved_by, resolved_at`

func scanConflict(sc scanner) (Conflict, error) {
	var (
		c                       Conflict
		proposal, text, why, by sql.NullString
		resolution, resolvedBy  sql.NullString
		proposedAt, resolvedAt  sql.NullTime
	)
	if err := sc.Scan(&c.ID, &c.Issue, &c.Field, &c.BaseRev, &c.Base, &c.Mine, &c.Theirs, &c.Author, &c.Session,
		&c.TheirsBy, &c.TheirsAt, &c.ParkedAt, &proposal, &text, &why, &by, &proposedAt, &resolution, &resolvedBy,
		&resolvedAt); err != nil {
		return Conflict{}, err
	}
	c.TheirsAt, c.ParkedAt = c.TheirsAt.UTC(), c.ParkedAt.UTC()
	if proposal.Valid {
		c.Proposal = &Proposal{Choice: Choice(proposal.String), Text: text.String, Why: why.String, By: by.String,
			At: proposedAt.Time.UTC()}
	}
	c.Resolution, c.ResolvedBy, c.ResolvedAt = resolution.String, resolvedBy.String, timePtr(resolvedAt)
	return c, nil
}

// loadConflict reads conflict id, or returns ErrNotFound.
func loadConflict(ctx context.Context, q querier, id int64) (Conflict, error) {
	c, err := scanConflict(q.QueryRowContext(ctx, `SELECT `+conflictCols+` FROM conflicts WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Conflict{}, fmt.Errorf("conflict %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Conflict{}, fmt.Errorf("get conflict %d: %w", id, err)
	}
	return c, nil
}

// GetConflict returns conflict id, open or resolved, with its advice,
// or ErrNotFound.
func (s *Store) GetConflict(ctx context.Context, id int64) (Conflict, error) {
	tx, end, err := s.beginRead(ctx)
	if err != nil {
		return Conflict{}, fmt.Errorf("get conflict %d: %w", id, err)
	}
	defer end()
	c, err := loadConflict(ctx, tx, id)
	if err != nil {
		return Conflict{}, err
	}
	if err := s.advise(ctx, tx, []*Conflict{&c}); err != nil {
		return Conflict{}, err
	}
	return c, nil
}

// ConflictFilter selects open conflicts: those of Issue, when it is set,
// at most Limit (0 means DefaultConflictLimit, at most MaxConflictLimit),
// after the conflict After, for the next page.
type ConflictFilter struct {
	Issue IssueID
	Limit int
	After int64
}

// Conflicts returns the open conflicts f selects, oldest first, each
// with its advice, and whether more follow them. An Issue that is not a
// valid id is ErrInvalid.
func (s *Store) Conflicts(ctx context.Context, f ConflictFilter) ([]Conflict, bool, error) {
	where, args := `resolved_at IS NULL AND id > ?`, []any{f.After}
	if f.Issue != "" {
		if err := f.Issue.Validate(); err != nil {
			return nil, false, err
		}
		where += ` AND issue_id = ?`
		args = append(args, string(f.Issue))
	}
	limit := clampLimit(f.Limit, DefaultConflictLimit, MaxConflictLimit)
	tx, end, err := s.beginRead(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("conflicts: %w", err)
	}
	defer end()
	// One more than the page says whether there is a next.
	rows, err := tx.QueryContext(ctx, `SELECT `+conflictCols+` FROM conflicts WHERE `+where+` ORDER BY id LIMIT ?`,
		append(args, limit+1)...)
	if err != nil {
		return nil, false, fmt.Errorf("conflicts: %w", err)
	}
	var out []Conflict
	for rows.Next() {
		c, err := scanConflict(rows)
		if err != nil {
			_ = rows.Close()
			return nil, false, fmt.Errorf("conflicts: %w", err)
		}
		out = append(out, c)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, false, fmt.Errorf("conflicts: %w", err)
	}
	more := len(out) > limit
	out = out[:min(len(out), limit)]
	ptrs := make([]*Conflict, len(out))
	for i := range out {
		ptrs[i] = &out[i]
	}
	if err := s.advise(ctx, tx, ptrs); err != nil {
		return nil, false, err
	}
	return out, more, nil
}

// advise sets each open conflict's Advice from who holds its issue now
// and who made each side (design §18.7, "the recommendation").
func (s *Store) advise(ctx context.Context, q querier, cs []*Conflict) error {
	now := s.now()
	holders := map[IssueID]string{}
	for _, c := range cs {
		if c.ResolvedAt != nil {
			continue
		}
		h, seen := holders[c.Issue]
		if !seen {
			cl, err := loadClaim(ctx, q, c.Issue)
			if err != nil {
				return err
			}
			if cl.active(now) {
				h = cl.Holder.Principal
			}
			holders[c.Issue] = h
		}
		c.Advice = advice(*c, h)
	}
	return nil
}

// advice is the recommendation for c when holder holds its issue ("" for
// nobody). It is advice only.
func advice(c Conflict, holder string) string {
	switch {
	case holder != "" && holder == c.TheirsBy && holder != c.Author:
		return fmt.Sprintf("theirs is from %s, who holds the issue; take theirs", holder)
	case holder != "" && holder == c.Author && holder != c.TheirsBy:
		return fmt.Sprintf("mine is from %s, who holds the issue; take mine", holder)
	}
	return "neither side holds the issue: compare them, or write text that keeps both"
}

// OpenConflicts counts the open conflicts that concern principal: those
// it parked, and those on issues assigned to it.
func (s *Store) OpenConflicts(ctx context.Context, principal string) (int, error) {
	var n int
	if err := s.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM conflicts c LEFT JOIN issues i ON i.id = c.issue_id
  WHERE c.resolved_at IS NULL AND (c.author = ? OR i.assignee = ?)`, principal, principal).Scan(&n); err != nil {
		return 0, fmt.Errorf("count conflicts: %w", err)
	}
	return n, nil
}
