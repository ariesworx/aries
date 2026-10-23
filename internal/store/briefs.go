package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	briefrec "github.com/ariesworx/aries/internal/brief"
	"github.com/ariesworx/aries/internal/secretscan"
)

// A brief is what the session that works an issue was started with,
// recorded by whoever started it (design §22): a comment of kind
// [CommentBrief] whose body is the record's line and the brief, as the
// memory `briefs` fixed it, with its fields, its hash and the claim it is
// joined to in briefs.

// OpBriefBind records a brief joined to the claim a start took. Its
// after state names the comment, the claim's principal, session and
// epoch.
const OpBriefBind Op = "brief.bind"

// The states of a brief.
const (
	// BriefPending waits for the next start that takes a new claim.
	BriefPending = "pending"
	// BriefBound is joined to a claim.
	BriefBound = "bound"
	// BriefReplaced was replaced by a later brief while it waited, and is
	// never joined.
	BriefReplaced = "replaced"
)

// maxBrief bounds the brief_bytes limit: the comment holds the record's
// line too, and a comment's text is at most maxText.
const maxBrief = 64000

// maxBriefField bounds each field but session, which is at most maxName.
const maxBriefField = 128

// Efforts are the efforts a brief may name.
var Efforts = []string{"low", "medium", "high", "xhigh", "max", "default"}

// BriefFields are what the starter says of a brief: the harness, model
// and effort it runs on, the session as the starter names it (a Desktop
// session's local_… id, or a headless run's Aries session), and the
// template it was rendered from (brief@N, or workflow@REV).
type BriefFields struct {
	Harness  string `json:"harness"`
	Model    string `json:"model"`
	Effort   string `json:"effort"`
	Session  string `json:"session,omitempty"`
	Template string `json:"template"`
}

// BriefClaim is the claim a brief is joined to.
type BriefClaim struct {
	Principal string    `json:"principal"`
	Session   string    `json:"session"`
	Epoch     int64     `json:"epoch"`
	At        time.Time `json:"at"`
}

// Brief is a recorded brief. By and BySession are who recorded it, from
// the connection; Text is the brief, trimmed, as its hash is taken.
type Brief struct {
	Comment   string  `json:"comment"`
	Issue     IssueID `json:"issue"`
	By        string  `json:"by"`
	BySession string  `json:"by_session"`
	BriefFields
	SHA256 string      `json:"sha256"`
	At     time.Time   `json:"at"`
	State  string      `json:"state"`
	Claim  *BriefClaim `json:"claim,omitempty"`
	Text   string      `json:"text,omitempty"`
}

// BriefReason says why a brief was refused.
type BriefReason string

// The reasons a [*BriefError] gives.
const (
	// BriefSecret: the text or a field looks like it holds a credential.
	BriefSecret BriefReason = "secret"
	// BriefOwnClaim: the recorder's session holds the issue's claim; a
	// session never briefs itself.
	BriefOwnClaim BriefReason = "own_claim"
	// BriefClaimHasOne: the claim the brief names has its brief already.
	BriefClaimHasOne BriefReason = "claim_has_one"
)

// BriefError refuses a brief. Field and Kind are set for BriefSecret,
// naming the field and what it looks like, never the text; Session for
// the two claim reasons. It wraps ErrInvalid.
type BriefError struct {
	Issue   IssueID
	Reason  BriefReason
	Field   string
	Kind    secretscan.Kind
	Session string
}

func (e *BriefError) Error() string {
	switch e.Reason {
	case BriefSecret:
		return fmt.Sprintf("%v: the brief's %s looks like it holds a secret (%s)", ErrInvalid, e.Field, e.Kind)
	case BriefOwnClaim:
		return fmt.Sprintf("%v: session %s holds %s, and a session never briefs itself", ErrInvalid, e.Session, e.Issue)
	default:
		return fmt.Sprintf("%v: the claim of session %s on %s has its brief already", ErrInvalid, e.Session, e.Issue)
	}
}

// Unwrap makes errors.Is(err, ErrInvalid) hold.
func (e *BriefError) Unwrap() error { return ErrInvalid }

// check refuses fields that are missing, too long, unsafe, hold a space
// or name an unknown effort, with ErrInvalid.
func (f BriefFields) check() error {
	for _, c := range []struct {
		name, v  string
		limit    int
		required bool
	}{
		{"harness", f.Harness, maxBriefField, true}, {"model", f.Model, maxBriefField, true},
		{"effort", f.Effort, maxBriefField, false}, {"session", f.Session, maxName, false},
		{"template", f.Template, maxBriefField, true},
	} {
		if err := checkLine(c.name, c.v, c.limit, c.required); err != nil {
			return err
		}
		if strings.ContainsAny(c.v, " =") {
			return fmt.Errorf("%w: %s must be one word, without a space or =", ErrInvalid, c.name)
		}
	}
	if f.Effort != "" && !slices.Contains(Efforts, f.Effort) {
		return fmt.Errorf("%w: effort must be one of %s", ErrInvalid, strings.Join(Efforts, ", "))
	}
	return nil
}

// secrets refuses fields or a text that look like a credential.
func (f BriefFields) secrets(id IssueID, text string) error {
	for _, c := range []struct{ name, v string }{
		{"text", text}, {"harness", f.Harness}, {"model", f.Model}, {"session", f.Session}, {"template", f.Template},
	} {
		if found, ok := secretscan.Find(c.v); ok {
			return &BriefError{Issue: id, Reason: BriefSecret, Field: c.name, Kind: found.Kind}
		}
	}
	return nil
}

// RecordBrief records the brief text that the actor starts a session on
// issue id with (design §22.2, §22.3). The comment's body is the
// record's line, built from f, then the brief. It is joined at once to
// the issue's live claim when that claim is another session's, has no
// brief, and is the session f names; otherwise it waits for the next
// start that takes a new claim, replacing a brief that waits already.
// With an idempotency key, a repeat returns the first brief and writes
// nothing. RecordBrief refuses, with ErrInvalid: missing or malformed
// fields; a text that is empty, unsafe or over Limits.BriefBytes; and,
// with a [*BriefError], what looks like a secret, a brief from the
// session that holds the claim, and one naming a claim that has one. A
// missing issue is ErrNotFound. Effort defaults to "default".
func (s *Store) RecordBrief(ctx context.Context, actor Actor, id IssueID, f BriefFields, text, idem string) (Brief, error) {
	if err := id.Validate(); err != nil {
		return Brief{}, err
	}
	if f.Effort == "" {
		f.Effort = "default"
	}
	if err := f.check(); err != nil {
		return Brief{}, err
	}
	if err := checkText("brief", text, s.opts.Limits.BriefBytes, true); err != nil {
		return Brief{}, err
	}
	if strings.TrimSpace(text) == "" {
		return Brief{}, fmt.Errorf("%w: the brief is only space", ErrInvalid)
	}
	if err := f.secrets(id, text); err != nil {
		return Brief{}, err
	}
	if err := validIdem(idem); err != nil {
		return Brief{}, err
	}
	var out Brief
	err := s.write(ctx, actor, func(w *wtx) error {
		if done, err := replay(ctx, w, idem, "brief", struct {
			ID   IssueID
			F    BriefFields
			Text string
		}{id, f, text}, &out); done || err != nil {
			return err
		}
		if err := mustExist(ctx, w.tx, id); err != nil {
			return err
		}
		c, err := loadClaim(ctx, w.tx, id)
		if err != nil {
			return err
		}
		live := c.active(w.now)
		if live && c.Holder.Session == w.actor.Session && c.Holder.Principal == w.actor.Principal {
			return &BriefError{Issue: id, Reason: BriefOwnClaim, Session: c.Holder.Session}
		}
		join := live && f.Session != "" && c.Holder.Session == f.Session
		if join {
			has, err := claimHasBrief(ctx, w.tx, id, c.Epoch)
			if err != nil {
				return err
			}
			if has {
				return &BriefError{Issue: id, Reason: BriefClaimHasOne, Session: c.Holder.Session}
			}
		}
		body := briefrec.Comment(briefrec.Record{Harness: f.Harness, Model: f.Model, Effort: f.Effort, Session: f.Session, Template: f.Template}, text)
		row, err := insertCommentRow(ctx, w, id, body, CommentBrief)
		if err != nil {
			return err
		}
		out = Brief{Comment: row.ID, Issue: id, By: row.Author, BySession: row.Session, BriefFields: f,
			SHA256: briefrec.Hash(text), At: row.CreatedAt, State: BriefPending}
		var replaced []string
		if !join {
			if replaced, err = replacePending(ctx, w, id); err != nil {
				return err
			}
		}
		if _, err := w.exec(ctx, `INSERT INTO briefs (comment_id, issue_id, harness, model, effort, session, template, sha256, state)
  VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, out.Comment, string(id), f.Harness, f.Model, f.Effort, f.Session, f.Template,
			out.SHA256, BriefPending); err != nil {
			return fmt.Errorf("insert brief: %w", err)
		}
		if err := w.event(ctx, OpCommentAdd, string(id), nil, struct {
			Comment
			Brief struct {
				BriefFields
				SHA256   string   `json:"sha256"`
				Replaces []string `json:"replaces,omitempty"`
			} `json:"brief"`
		}{row, struct {
			BriefFields
			SHA256   string   `json:"sha256"`
			Replaces []string `json:"replaces,omitempty"`
		}{f, out.SHA256, replaced}}); err != nil {
			return err
		}
		if join {
			cl, err := bindBrief(ctx, w, id, out.Comment, c.Holder, c.Epoch)
			if err != nil {
				return err
			}
			out.State, out.Claim = BriefBound, &cl
		}
		return w.settle(out)
	})
	if err != nil {
		return Brief{}, err
	}
	return out, nil
}

// claimHasBrief reports whether epoch of issue id's claim has a brief.
func claimHasBrief(ctx context.Context, q querier, id IssueID, epoch int64) (bool, error) {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM briefs WHERE issue_id = $1 AND claim_epoch = $2`,
		string(id), epoch).Scan(&n); err != nil {
		return false, fmt.Errorf("briefs of %s: %w", id, err)
	}
	return n > 0, nil
}

// replacePending marks the briefs waiting on issue id replaced, and
// returns their comments. The event of the brief that replaces them
// names them.
func replacePending(ctx context.Context, w *wtx, id IssueID) ([]string, error) {
	rows, err := w.tx.QueryContext(ctx, `UPDATE briefs SET state = $1 WHERE issue_id = $2 AND state = $3 RETURNING comment_id`,
		BriefReplaced, string(id), BriefPending)
	if err != nil {
		return nil, fmt.Errorf("replace briefs of %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, fmt.Errorf("replace briefs of %s: %w", id, err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// bindBrief joins the brief in comment to the claim holder took at
// epoch, and records the event.
func bindBrief(ctx context.Context, w *wtx, id IssueID, comment string, holder Actor, epoch int64) (BriefClaim, error) {
	cl := BriefClaim{Principal: holder.Principal, Session: holder.Session, Epoch: epoch, At: w.now}
	if _, err := w.exec(ctx, `UPDATE briefs SET state = $1, claim_principal = $2, claim_session = $3, claim_epoch = $4, bound_at = $5
  WHERE comment_id = $6`, BriefBound, cl.Principal, cl.Session, cl.Epoch, cl.At, comment); err != nil {
		return BriefClaim{}, fmt.Errorf("join brief %s: %w", comment, err)
	}
	return cl, w.event(ctx, OpBriefBind, string(id), nil, map[string]any{
		"comment": comment, "principal": cl.Principal, "session": cl.Session, "epoch": cl.Epoch})
}

// joinPending joins the brief waiting on issue id, if there is one, to
// the new claim holder took at epoch: start calls it in its write when it
// takes a new claim. A brief its holder's own session recorded is left
// waiting, since a session never briefs itself.
func joinPending(ctx context.Context, w *wtx, id IssueID, holder Actor, epoch int64) error {
	var comment string
	err := w.tx.QueryRowContext(ctx, `SELECT b.comment_id FROM briefs b JOIN comments c ON c.id = b.comment_id
  WHERE b.issue_id = $1 AND b.state = $2 AND NOT (c.author = $3 AND c.session = $4)
  ORDER BY c.created_at DESC, c.id DESC LIMIT 1`, string(id), BriefPending, holder.Principal, holder.Session).Scan(&comment)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("pending brief of %s: %w", id, err)
	}
	_, err = bindBrief(ctx, w, id, comment, holder, epoch)
	return err
}

// Briefs returns issue id's briefs, oldest first, with their text.
func (s *Store) Briefs(ctx context.Context, id IssueID) ([]Brief, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	return queryBriefs(ctx, s.r, id, 0)
}

// latestBrief is issue id's latest brief that was not replaced, or nil.
func latestBrief(ctx context.Context, q querier, id IssueID) (*Brief, error) {
	bs, err := queryBriefs(ctx, q, id, 1)
	if err != nil || len(bs) == 0 {
		return nil, err
	}
	return &bs[0], nil
}

// queryBriefs reads issue id's briefs: all of them oldest first, or with
// latest > 0, that many of the newest not replaced, newest first.
func queryBriefs(ctx context.Context, q querier, id IssueID, latest int) ([]Brief, error) {
	query := `SELECT b.comment_id, c.author, c.session, b.harness, b.model, b.effort, b.session, b.template, b.sha256,
  c.created_at, b.state, b.claim_principal, b.claim_session, b.claim_epoch, b.bound_at, c.body
  FROM briefs b JOIN comments c ON c.id = b.comment_id WHERE b.issue_id = $1`
	args := []any{string(id)}
	if latest > 0 {
		query += ` AND b.state <> $2 ORDER BY c.created_at DESC, c.id DESC LIMIT $3`
		args = append(args, BriefReplaced, latest)
	} else {
		query += ` ORDER BY c.created_at, c.id`
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("briefs of %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Brief
	for rows.Next() {
		b := Brief{Issue: id}
		var p, sess sql.NullString
		var epoch sql.NullInt64
		var bound sql.NullTime
		var body string
		if err := rows.Scan(&b.Comment, &b.By, &b.BySession, &b.Harness, &b.Model, &b.Effort, &b.Session, &b.Template,
			&b.SHA256, &b.At, &b.State, &p, &sess, &epoch, &bound, &body); err != nil {
			return nil, fmt.Errorf("briefs of %s: %w", id, err)
		}
		b.At = b.At.UTC()
		if epoch.Valid {
			b.Claim = &BriefClaim{Principal: p.String, Session: sess.String, Epoch: epoch.Int64, At: bound.Time.UTC()}
		}
		_, b.Text, _ = briefrec.ParseComment(body)
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("briefs of %s: %w", id, err)
	}
	return out, nil
}
