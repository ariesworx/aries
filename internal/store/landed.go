package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ariesworx/aries/internal/proto"
)

// Close means landed (design §21.5, decision 95; slice W1). A profile's
// checks are names of ci gates (ci_gates.go) that an issue made under it
// requires: finish and close are refused while one has not passed, with
// an [*AcceptanceError] naming it, and the passing report closes the
// issue itself when nothing else is open.
//
// What an issue requires is read from its own log, never from the
// workflow in force: the workflow stamps on its issue.create and on each
// change of type (WorkflowStamp), each naming a rev, whose document the
// workflow.set event of that rev holds. Every profile stamped on an
// issue counts, as a change of type keeps what the old type added. A
// later set of the workflow changes nothing an issue requires.
//
// The first report of a required check makes its ci gate, in the
// report's write, reported by the workflow's ci principals; a principal
// it does not name is refused, and the write, gate and all, with it. A
// pass ticks the items bound to its check, and when it leaves the
// subject with every required check passed (or waived), no item open
// and no open gate holding it, closes it: issue.close by the reporter
// with reason landed, ending any claim, and gate.passed to the assignee
// and the last handoff's author. A later failure of a passed check
// unticks those items and, if the subject is closed, reopens it
// (issue.reopen, reason check failed); gate.failed tells its assignee.
//
// The closing pass and the reopen change an issue whoever holds it, with
// no guard: they are the CI principal's report, which the workflow
// names, and the close ends the claim as an admin's would, telling the
// holder (claim.lost).

// The close reason of the closing pass and the reopen reason of a check
// that failed after it.
const (
	ReasonLanded      = "landed"
	ReasonCheckFailed = "check failed"
)

// CheckWait is a required check an issue waits for: its gate, when a
// report made one, and the gate's last report.
type CheckWait struct {
	Check  string      `json:"check"`
	Gate   IssueID     `json:"gate,omitempty"`
	Report *GateReport `json:"report,omitempty"`
}

// String is the check and where it stands, as a refusal and a digest
// line give it: "landed (no report yet)", "landed (failed at 0123abcd)".
func (c CheckWait) String() string {
	r := c.Report
	switch {
	case r == nil:
		return c.Check + " (no report yet)"
	case r.SHA != "":
		return fmt.Sprintf("%s (%s at %s)", c.Check, r.Result, r.SHA[:min(len(r.SHA), 12)])
	}
	return fmt.Sprintf("%s (%s)", c.Check, r.Result)
}

// checksText joins ws as a refusal names them.
func checksText(ws []CheckWait) string {
	s := make([]string, len(ws))
	for i, w := range ws {
		s[i] = w.String()
	}
	return strings.Join(s, ", ")
}

// requiredCheck is a check an issue's workflow requires: the principals
// that report it, from the latest stamp that requires it, and the texts
// of the items bound to it.
type requiredCheck struct {
	check string
	ci    []string
	items []string
}

// stampedProfile is a profile stamped on an issue: the document in force
// at the stamp's rev and the profile's type in it.
type stampedProfile struct {
	doc *proto.Workflow
	typ string
}

// stampedProfiles returns the profiles stamped on issue id's events, in
// the order they were stamped; none for an issue no workflow governed.
func stampedProfiles(ctx context.Context, q querier, id IssueID) ([]stampedProfile, error) {
	rows, err := q.QueryContext(ctx, `SELECT CAST(after_state->'workflow' AS TEXT) FROM events
  WHERE target = $1 AND op IN ($2, $3) AND after_state->'workflow' IS NOT NULL ORDER BY seq`,
		string(id), string(OpIssueCreate), string(OpIssueUpdate))
	if err != nil {
		return nil, fmt.Errorf("workflow of %s: %w", id, err)
	}
	var stamps []WorkflowStamp
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("workflow of %s: %w", id, err)
		}
		var st WorkflowStamp
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("workflow of %s: %w", id, err)
		}
		if st.Type != "" {
			stamps = append(stamps, st)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("workflow of %s: %w", id, err)
	}
	var out []stampedProfile
	docs := map[int64]*proto.Workflow{}
	for _, st := range stamps {
		doc, ok := docs[st.Rev]
		if !ok {
			if doc, err = workflowAt(ctx, q, st.Rev); err != nil {
				return nil, err
			}
			docs[st.Rev] = doc
		}
		if doc != nil {
			out = append(out, stampedProfile{doc: doc, typ: st.Type})
		}
	}
	return out, nil
}

// requiredChecks returns the checks the profiles stamped on issue id's
// events require, in the order the profiles name them; none for an issue
// no workflow governed.
func requiredChecks(ctx context.Context, q querier, id IssueID) ([]requiredCheck, error) {
	profiles, err := stampedProfiles(ctx, q, id)
	if err != nil {
		return nil, err
	}
	var out []requiredCheck
	for _, sp := range profiles {
		doc, p := sp.doc, sp.doc.Types[sp.typ]
		for _, c := range p.Checks {
			i := slices.IndexFunc(out, func(r requiredCheck) bool { return r.check == c })
			if i < 0 {
				out, i = append(out, requiredCheck{check: c}), len(out)
			}
			out[i].ci = doc.CI
			for _, it := range p.Acceptance {
				if it.Check == c && !slices.Contains(out[i].items, it.Text) {
					out[i].items = append(out[i].items, it.Text)
				}
			}
		}
	}
	return out, nil
}

// closeoutLines returns the close-out lines (design §21.7) of the
// profiles stamped on issue id, in the order they were stamped, each
// once; none for an issue no workflow governed.
func closeoutLines(ctx context.Context, q querier, id IssueID) ([]string, error) {
	profiles, err := stampedProfiles(ctx, q, id)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, sp := range profiles {
		for _, l := range sp.doc.Types[sp.typ].Closeout {
			if !slices.Contains(out, l) {
				out = append(out, l)
			}
		}
	}
	return out, nil
}

// Closeout returns the close-out lines of the workflow profiles stamped
// on issue id (design §21.7): what the harness does when its work ends.
// They are shown, never tracked. None for an issue no workflow governed
// or whose profiles name none.
func (s *Store) Closeout(ctx context.Context, id IssueID) ([]string, error) {
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("closeout of %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }() // read-only: nothing to keep
	return closeoutLines(ctx, tx, id)
}

// workflowAt reads the workflow's document at rev from the workflow.set
// event that made it; nil when there is none.
func workflowAt(ctx context.Context, q querier, rev int64) (*proto.Workflow, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT CAST(after_state->'workflow' AS TEXT) FROM events
  WHERE target = $1 AND op = $2 AND after_state->>'rev' = $3 ORDER BY seq DESC LIMIT 1`,
		WorkflowTarget, string(OpWorkflowSet), fmt.Sprint(rev)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("workflow rev %d: %w", rev, err)
	}
	var w proto.Workflow
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		return nil, fmt.Errorf("workflow rev %d: %w", rev, err)
	}
	return &w, nil
}

// findRequired returns the required check named c, or nil.
func findRequired(req []requiredCheck, c string) *requiredCheck {
	for i := range req {
		if req[i].check == c {
			return &req[i]
		}
	}
	return nil
}

// checkWaits returns the checks issue id requires whose gate has not
// passed and was not waived, in the order requiredChecks gives.
func checkWaits(ctx context.Context, q querier, id IssueID) ([]CheckWait, error) {
	req, err := requiredChecks(ctx, q, id)
	if err != nil || len(req) == 0 {
		return nil, err
	}
	gates, err := loadGates(ctx, q, `WHERE g.kind = 'ci' AND g.subject = $1`, maxGateChecks, string(id))
	if err != nil {
		return nil, err
	}
	var out []CheckWait
	for _, r := range req {
		w := CheckWait{Check: r.check}
		if i := slices.IndexFunc(gates, func(g gateRow) bool { return g.Check == r.check }); i >= 0 {
			if !gates[i].open {
				continue
			}
			w.Gate, w.Report = gates[i].ID, gates[i].Report
		}
		out = append(out, w)
	}
	return out, nil
}

// makeCheckGate makes the ci gate of check rc on subject, as the first
// report of a required check does: an AddGate in the report's write,
// reported by the workflow's ci principals.
func (s *Store) makeCheckGate(ctx context.Context, w *wtx, subject IssueID, rc *requiredCheck) (*gateRow, error) {
	if len(rc.ci) == 0 {
		return nil, fmt.Errorf("%w: the workflow requires check %s of %s but names no principal to report it; a project admin adds them under ci",
			ErrInvalid, rc.check, subject)
	}
	in := NewGate{Kind: GateCI, Check: rc.check, Subject: subject, Approvers: rc.ci}
	if err := in.normalize(s.opts.Limits.GateApprovers); err != nil {
		return nil, err
	}
	id, err := NewID(s.opts.Prefix)
	if err != nil {
		return nil, err
	}
	g, err := s.insertGate(ctx, w, id, in)
	if err != nil {
		return nil, fmt.Errorf("make the gate of check %s on %s: %w", rc.check, subject, err)
	}
	return g, nil
}

// landing is what a report of a check on its subject does to the subject
// under its workflow, once the gate has taken it: rc is the required
// check (nil when the subject does not require it), passedBefore whether
// the gate had passed before this report, and rep the report.
func (w *wtx) landing(ctx context.Context, g *gateRow, rc *requiredCheck, passedBefore bool, rep GateReport) ([]InboxItem, error) {
	if rc == nil || g.Subject == "" {
		return nil, nil
	}
	is, err := loadIssue(ctx, w.tx, g.Subject)
	if err != nil {
		return nil, err
	}
	if rep.Result == CheckFailed {
		if !passedBefore {
			return nil, nil
		}
		if is.Status == StatusClosed {
			if _, err := setStatus(ctx, w, is, OpIssueReopen, []any{string(StatusOpen), nil, nil},
				map[string]any{"reason": ReasonCheckFailed}); err != nil {
				return nil, err
			}
			if is, err = loadIssue(ctx, w.tx, g.Subject); err != nil {
				return nil, err
			}
		}
		return nil, w.setBound(ctx, is, rc, ItemOpen)
	}
	if is.Status == StatusClosed {
		return nil, nil
	}
	if err := w.setBound(ctx, is, rc, ItemTicked); err != nil {
		return nil, err
	}
	waits, err := checkWaits(ctx, w.tx, is.ID)
	if err != nil || len(waits) > 0 {
		return nil, err
	}
	items, err := acceptanceItems(ctx, w.tx, is)
	if err != nil {
		return nil, err
	}
	held, err := holdingGates(ctx, w.tx, is.ID)
	if err != nil {
		return nil, err
	}
	reviews, err := reviewWaits(ctx, w.tx, is.ID)
	if err != nil {
		return nil, err
	}
	what := fmt.Sprintf("%s passed on %s%s (gate %s)", g.Check, is.ID, evidenceText(rep), g.ID)
	var closeout []string
	switch open := openItems(items); {
	case len(reviews) > 0:
		what += fmt.Sprintf("; waits for review %s, then finish it", reviewsText(reviews))
	case len(open) > 0:
		what += fmt.Sprintf("; still open: items %s, then finish it", joinInts(open))
	case len(held) > 0:
		what += fmt.Sprintf("; held by gate %s, then finish it", held[0])
	default:
		w.closingPass = true
		if _, err := closeTx(ctx, w, is, ReasonLanded, false); err != nil {
			return nil, err
		}
		if err := w.landedAfter(ctx, is); err != nil {
			return nil, err
		}
		what = fmt.Sprintf("%s closed: %s", is.ID, what)
		// The session that did the work may be gone: the close-out goes
		// where its people will read it (design §21.7).
		if closeout, err = closeoutLines(ctx, w.tx, is.ID); err != nil {
			return nil, err
		}
	}
	to, err := landingPeople(ctx, w.tx, is)
	if err != nil {
		return nil, err
	}
	var out []InboxItem
	for _, p := range to {
		out = append(out, InboxItem{To: p, Kind: InboxGatePassed, Issue: is.ID, Body: what})
		if len(closeout) > 0 {
			out = append(out, InboxItem{To: p, Kind: InboxGatePassed, Issue: is.ID,
				Body: fmt.Sprintf("close-out of %s: %s", is.ID, strings.Join(closeout, "; "))})
		}
	}
	return out, nil
}

// setBound sets the items of is bound to rc's check to state, ticked or
// open, with an event for each it changes, by w's actor: a pass ticks
// them, and a failure after a pass unticks them.
func (w *wtx) setBound(ctx context.Context, is Issue, rc *requiredCheck, state ItemState) error {
	if len(rc.items) == 0 {
		return nil
	}
	items, err := acceptanceItems(ctx, w.tx, is)
	if err != nil {
		return err
	}
	var a Acceptance
	for _, it := range items {
		if !slices.Contains(rc.items, it.Text) {
			continue
		}
		switch {
		case state == ItemTicked && it.State == ItemOpen:
			a.Tick = append(a.Tick, it.N)
		case state == ItemOpen && it.State == ItemTicked:
			a.Untick = append(a.Untick, it.N)
		}
	}
	if a.empty() {
		return nil
	}
	_, err = applyAcceptance(ctx, w, is, a)
	return err
}

// landingPeople are whom the closing pass tells: the assignee and the
// author of the last handoff, each once.
func landingPeople(ctx context.Context, q querier, is Issue) ([]string, error) {
	var out []string
	if PrincipalPattern.MatchString(is.Assignee) {
		out = append(out, is.Assignee)
	}
	var author string
	err := q.QueryRowContext(ctx, `SELECT c.author FROM handoffs h JOIN comments c ON c.id = h.comment_id
  WHERE h.issue_id = $1 ORDER BY c.created_at DESC, c.id DESC LIMIT 1`, string(is.ID)).Scan(&author)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, fmt.Errorf("last handoff of %s: %w", is.ID, err)
	case PrincipalPattern.MatchString(author) && !slices.Contains(out, author):
		out = append(out, author)
	}
	return out, nil
}
