package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// Gates (design §20.3; decisions 66, 68 and 69). A gate is an issue of
// type gate with a row in the gates table, and what it holds back depends
// on it with an ordinary blocks edge, so the blocked derivation holds it
// back unchanged: a gate on an epic holds every descendant, and whatever
// waits on those. Ready and start leave gates out, and only the gate ops
// close or reopen one.
//
// A gate holds an issue when the issue reaches the open gate by blocking
// edges to unclosed issues and by parent links. Nothing that would undo
// that is let through for someone who could not pass the gate: a start of
// a held issue (for anyone: pass the gate first), a close or finish of
// one, removing an edge or a parent link that the hold runs through. An
// update of a held issue's status is let through, since the gate holds
// through the graph and not the status.
//
// Who may pass a human gate: the principals it names, or when it names
// none the project's admins other than its creator, and only from the
// session cli, a person's own terminal. Waive, an admin's pass with a
// reason, and rearm, which reopens a gate, take the same session. Who may
// release a hold (remove an edge to a gate, or close what it holds) is
// whoever could pass or waive it.

// TypeGate is the issue type of a gate. Only AddGate makes one.
const TypeGate IssueType = "gate"

// GateKind is what passes a gate.
type GateKind string

// Gate kinds.
const (
	// GateHuman is passed by a principal it names, or an admin.
	GateHuman GateKind = "human"
	// GateCI is passed or failed by a CI principal it names, which
	// reports a check on its subject (ci_gates.go).
	GateCI GateKind = "ci"
)

// GateOutcome is how a gate was closed; empty while it is open.
type GateOutcome string

// Gate outcomes.
const (
	GatePassed GateOutcome = "passed"
	GateWaived GateOutcome = "waived"
)

// The gate events and inbox kinds.
const (
	OpGateAdd   Op = "gate.add"
	OpGatePass  Op = "gate.pass"
	OpGateWaive Op = "gate.waive"
	OpGateRearm Op = "gate.rearm"
	OpGateFail  Op = "gate.fail"

	// InboxGatePending tells a gate's approvers it waits for them.
	InboxGatePending InboxKind = "gate.pending"
	// InboxGatePassed tells the assignees of what a gate held that it
	// passed or was waived.
	InboxGatePassed InboxKind = "gate.passed"
	// InboxGateFailed tells the holder of a ci gate's subject, or its
	// assignee, that a check failed, with the evidence.
	InboxGateFailed InboxKind = "gate.failed"
)

// Gate is a gate as read back: its issue's id, title, creator and time,
// and its own fields. Holds are the issues that depend on it directly,
// at most maxGateHolds. A ci gate's Approvers are the principals that
// report its Check on its Subject, and Report is the last report made.
type Gate struct {
	ID        IssueID     `json:"id"`
	Title     string      `json:"title"`
	Kind      GateKind    `json:"kind"`
	Approvers []string    `json:"approvers"`
	Check     string      `json:"check,omitempty"`
	Subject   IssueID     `json:"subject,omitempty"`
	Report    *GateReport `json:"report,omitempty"`
	Outcome   GateOutcome `json:"outcome,omitempty"`
	By        string      `json:"by,omitempty"`
	At        *time.Time  `json:"at,omitempty"`
	Reason    string      `json:"reason,omitempty"`
	CreatedBy string      `json:"created_by"`
	CreatedAt time.Time   `json:"created_at"`
	Holds     []IssueID   `json:"holds,omitempty"`
}

// NewGate is the input to AddGate.
type NewGate struct {
	// Kind is GateHuman (the default) or GateCI.
	Kind GateKind
	// Blocks is the issue the gate holds back. A ci gate may hold none,
	// and then only records its check's outcome on its subject.
	Blocks IssueID
	// Title defaults to "Approve " and the held issue's id, and for a ci
	// gate to its check, " of " and its subject.
	Title string
	// Approvers may pass a human gate; none leaves it to the project's
	// admins. A ci gate names at least one: the principals that report
	// its check.
	Approvers []string
	// Check and Subject are a ci gate's: the check's name (such as build,
	// test or landed) and the issue whose branch it runs on. A subject has
	// at most one ci gate for each check.
	Check   string
	Subject IssueID
	// review is the issue a human gate reviews instead of holding back
	// (review.go): Blocks is then empty, and the gate's subject is the
	// issue. Only a workflow's profile sets it.
	review IssueID
	// Priority defaults to the held issue's.
	Priority *Priority
	// IdempotencyKey makes a retried add return the first result.
	IdempotencyKey string
}

// GateFilter selects gates for Gates: open ones, or with All every one,
// oldest first, at most Limit (0 means 50, at most 500).
type GateFilter struct {
	All   bool
	Limit int
}

// maxGateHolds bounds the issues a Gate lists as held.
const maxGateHolds = 50

// GateReason says why a [*GateError] refused a change.
type GateReason string

// The reasons a [*GateError] gives.
const (
	// GateHeld: the issue is held behind an open gate the actor may not
	// release, or a start of a held issue, which waits for the gate.
	GateHeld GateReason = "held"
	// GateIsGate: a gate is opened and closed only by the gate ops, and
	// made only by AddGate.
	GateIsGate GateReason = "gate"
	// GateNotApprover: the actor is not one the gate names, nor an admin
	// when it names none.
	GateNotApprover GateReason = "not approver"
	// GateNotPerson: the request came from an agent's session, not the
	// session cli.
	GateNotPerson GateReason = "not person"
	// GateNotAdmin: a waive, which is for admins.
	GateNotAdmin GateReason = "not admin"
	// GateNotCI: a ci gate's report came from an agent's session, not a
	// CI job's (proto.CISession) or a person's terminal.
	GateNotCI GateReason = "not ci"
	// GateNoCheck: the subject has no ci gate for the check named.
	GateNoCheck GateReason = "no check"
)

// GateError refuses a change a gate does not allow. ID is the issue the
// change was to, Gate the gate in the way (the same as ID for a change to
// a gate), and Approvers those the gate names, empty for the project's
// admins. Held and IsGate wrap ErrInvalid, NoCheck ErrNotFound, and the
// rest ErrForbidden. Check is the check of a ci gate's refusal: for
// NoCheck, the one that subject ID has no gate for.
type GateError struct {
	ID        IssueID
	Gate      IssueID
	Approvers []string
	Check     string
	Reason    GateReason
	// Op names the change that was refused, such as "close".
	Op string
}

// ApproverText names who may pass the gate, in a message.
func (e *GateError) ApproverText() string {
	if len(e.Approvers) == 0 {
		return "the project's admins"
	}
	return strings.Join(e.Approvers, ", ")
}

func (e *GateError) Error() string {
	switch e.Reason {
	case GateHeld:
		if e.Op == "start" {
			return fmt.Sprintf("%v: issue %s waits behind gate %s, which %s may pass", ErrInvalid, e.ID, e.Gate, e.ApproverText())
		}
		return fmt.Sprintf("%v: issue %s is held by gate %s; only %s may %s it while the gate is open", ErrInvalid, e.ID, e.Gate, e.ApproverText(), e.Op)
	case GateIsGate:
		if e.Gate == "" {
			return fmt.Sprintf("%v: an issue becomes a gate only through gate add", ErrInvalid)
		}
		return fmt.Sprintf("%v: %s is a gate; only %s may pass it, with gate pass", ErrInvalid, e.ID, e.ApproverText())
	case GateNotPerson:
		return fmt.Sprintf("gate %s is passed by a person at their own terminal, not from an agent's session", e.Gate)
	case GateNotAdmin:
		return fmt.Sprintf("waiving gate %s is for admins", e.Gate)
	case GateNotCI:
		return fmt.Sprintf("gate %s is reported from a CI job or a person's terminal, not from an agent's session", e.Gate)
	case GateNoCheck:
		return fmt.Sprintf("%v: issue %s has no ci gate for check %s", ErrNotFound, e.ID, e.Check)
	}
	return fmt.Sprintf("only %s may %s gate %s", e.ApproverText(), e.Op, e.Gate)
}

// Unwrap makes errors.Is hold for ErrInvalid, ErrNotFound or
// ErrForbidden.
func (e *GateError) Unwrap() error {
	switch e.Reason {
	case GateHeld, GateIsGate:
		return ErrInvalid
	case GateNoCheck:
		return ErrNotFound
	}
	return ErrForbidden
}

// gateRow is a gate's own row, with its issue's creator.
type gateRow struct {
	Gate
	open bool
}

// loadGate reads the gate whose issue is id, or nil when id is no gate.
func loadGate(ctx context.Context, q querier, id IssueID) (*gateRow, error) {
	gs, err := loadGates(ctx, q, `WHERE g.issue_id = $1`, 1, string(id))
	if err != nil || len(gs) == 0 {
		return nil, err
	}
	return &gs[0], nil
}

// loadGates reads the first limit gates that where, a constant clause
// with placeholders for args, selects, oldest first, with what each
// holds.
func loadGates(ctx context.Context, q querier, where string, limit int, args ...any) ([]gateRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT g.issue_id, i.title, g.kind, g.approvers, g.outcome, g.decided_by,
  g.decided_at, g.reason, i.created_by, i.created_at, g.check_name, g.subject, g.report
  FROM gates g JOIN issues i ON i.id = g.issue_id `+where+ //nolint:gosec // where is a constant from the callers
		` ORDER BY i.created_at, i.id LIMIT `+placeholder(len(args)+1), append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("gates: %w", err)
	}
	var out []gateRow
	for rows.Next() {
		var g gateRow
		var approvers, report []byte
		var outcome, by, subject sql.NullString
		var at sql.NullTime
		if err := rows.Scan(&g.ID, &g.Title, &g.Kind, &approvers, &outcome, &by, &at, &g.Reason, &g.CreatedBy, &g.CreatedAt,
			&g.Check, &subject, &report); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("gates: %w", err)
		}
		if err := json.Unmarshal(approvers, &g.Approvers); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("gate %s approvers: %w", g.ID, err)
		}
		if len(report) > 0 && string(report) != "null" {
			if err := json.Unmarshal(report, &g.Report); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("gate %s report: %w", g.ID, err)
			}
			g.Report.At = g.Report.At.UTC()
		}
		g.Subject = IssueID(subject.String)
		g.Outcome, g.By, g.At, g.CreatedAt = GateOutcome(outcome.String), by.String, timePtr(at), g.CreatedAt.UTC()
		g.open = !outcome.Valid
		out = append(out, g)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("gates: %w", err)
	}
	if len(out) == 0 {
		return out, nil
	}
	ids := make([]any, len(out))
	at := map[IssueID]int{}
	for i, g := range out {
		ids[i], at[g.ID] = string(g.ID), i
	}
	hr, err := q.QueryContext(ctx, `SELECT to_id, from_id FROM deps WHERE to_id IN (`+placeholders(1, len(ids))+
		`) AND type IN (`+readyBlocking+`) ORDER BY to_id, from_id`, ids...)
	if err != nil {
		return nil, fmt.Errorf("gate holds: %w", err)
	}
	defer func() { _ = hr.Close() }()
	for hr.Next() {
		var to, from IssueID
		if err := hr.Scan(&to, &from); err != nil {
			return nil, fmt.Errorf("gate holds: %w", err)
		}
		if g := &out[at[to]]; len(g.Holds) < maxGateHolds {
			g.Holds = append(g.Holds, from)
		}
	}
	if err := hr.Err(); err != nil {
		return nil, fmt.Errorf("gate holds: %w", err)
	}
	return out, nil
}

// openGates counts the open gates in the project.
func openGates(ctx context.Context, q querier) (int, error) {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM gates WHERE outcome IS NULL`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count open gates: %w", err)
	}
	return n, nil
}

// holdingGates returns the open gates that hold issue id, as the package
// comment defines it, in id order. A project with no open gate is
// answered without walking the graph.
func holdingGates(ctx context.Context, q querier, id IssueID) ([]IssueID, error) {
	if n, err := openGates(ctx, q); err != nil || n == 0 {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `WITH RECURSIVE
edges (src, dst) AS (
  SELECT d.from_id, d.to_id FROM deps d
  JOIN issues f ON f.id = d.from_id
  JOIN issues t ON t.id = d.to_id
  WHERE d.type IN (`+readyBlocking+`) AND f.status <> 'closed' AND t.status <> 'closed'
  UNION ALL
  SELECT id, parent_id FROM issues WHERE parent_id IS NOT NULL
),
reach (id) AS (
  SELECT CAST($1 AS VARCHAR(64))
  UNION
  SELECT e.dst FROM edges e JOIN reach r ON e.src = r.id
)
SELECT g.issue_id FROM reach r JOIN gates g ON g.issue_id = r.id
WHERE g.outcome IS NULL AND g.issue_id <> $1 ORDER BY g.issue_id`, string(id))
	if err != nil {
		return nil, fmt.Errorf("gates holding %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var out []IssueID
	for rows.Next() {
		var g IssueID
		if err := rows.Scan(&g); err != nil {
			return nil, fmt.Errorf("gates holding %s: %w", id, err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gates holding %s: %w", id, err)
	}
	return out, nil
}

// approves reports whether w's actor is one g names, or, when g names
// none, an admin who did not make it. It does not look at the session.
func (w *wtx) approves(g *gateRow) bool {
	if len(g.Approvers) > 0 {
		return slices.Contains(g.Approvers, w.actor.Principal)
	}
	return w.admin && w.actor.Principal != g.CreatedBy
}

// inPerson reports whether w's actor is a person at their own terminal:
// the session cli, which no agent harness's session is.
func (w *wtx) inPerson() bool { return w.actor.Session == proto.CLISession }

// sessionRefusal is why w's actor's session may not make decision
// outcome on g, or "" when it may. A person decides at their own
// terminal; a ci gate's pass, failure or rearm may come from a CI job's
// session too. A waive is a person's, whatever the gate.
func (w *wtx) sessionRefusal(g *gateRow, outcome GateOutcome) GateReason {
	switch {
	case w.inPerson():
		return ""
	case g.Kind != GateCI || outcome == GateWaived:
		return GateNotPerson
	case w.actor.Session == proto.CISession:
		return ""
	}
	return GateNotCI
}

// mayRelease reports whether w's actor may undo g's hold: one who could
// pass or waive it, in person.
func (w *wtx) mayRelease(g *gateRow) bool {
	return w.inPerson() && (w.approves(g) || w.admin)
}

// heldError is the GateError for a change op to id, which gate holds.
func heldError(id IssueID, g *gateRow, op string) error {
	return &GateError{ID: id, Gate: g.ID, Approvers: g.Approvers, Reason: GateHeld, Op: op}
}

// releasable refuses op on issue id, unless w's actor may release every
// gate in gates.
func (w *wtx) releasable(ctx context.Context, id IssueID, gates []IssueID, op string) error {
	for _, gid := range gates {
		g, err := loadGate(ctx, w.tx, gid)
		if err != nil {
			return err
		}
		if g != nil && !w.mayRelease(g) {
			return heldError(id, g, op)
		}
	}
	return nil
}

// checkHolds refuses op, a change already made in w, if issue id was
// held by a gate in before that no longer holds it and that w's actor
// may not release.
func (w *wtx) checkHolds(ctx context.Context, id IssueID, before []IssueID, op string) error {
	if len(before) == 0 {
		return nil
	}
	after, err := holdingGates(ctx, w.tx, id)
	if err != nil {
		return err
	}
	var lost []IssueID
	for _, g := range before {
		if !slices.Contains(after, g) {
			lost = append(lost, g)
		}
	}
	return w.releasable(ctx, id, lost, op)
}

// isGateError is the GateError for a change op to gate g, made outside
// the gate ops.
func isGateError(ctx context.Context, q querier, id IssueID, op string) error {
	g, err := loadGate(ctx, q, id)
	if err != nil {
		return err
	}
	e := &GateError{ID: id, Gate: id, Reason: GateIsGate, Op: op}
	if g != nil {
		e.Approvers = g.Approvers
	}
	return e
}

// normalize checks a new gate and fills in its defaults, refusing with
// ErrInvalid what is not valid or more approvers than limit.
func (n *NewGate) normalize(limit int) error {
	switch n.Kind {
	case "", GateHuman:
		n.Kind = GateHuman
		if n.Check != "" || n.Subject != "" {
			return fmt.Errorf("%w: a check and a subject are a ci gate's; add it with kind ci", ErrInvalid)
		}
		if n.Blocks == "" && n.review == "" {
			return fmt.Errorf("%w: a gate holds an issue back; name it", ErrInvalid)
		}
	case GateCI:
		if err := n.normalizeCI(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: gate kind %q; give human or ci", ErrInvalid, n.Kind)
	}
	if n.Blocks != "" {
		if err := n.Blocks.Validate(); err != nil {
			return err
		}
	}
	if n.Title == "" {
		n.Title = "Approve " + string(n.Blocks)
		if n.review != "" {
			n.Title = "Review " + string(n.review)
		}
		if n.Kind == GateCI {
			n.Title = n.Check + " of " + string(n.Subject)
		}
	}
	if err := checkTitle(n.Title); err != nil {
		return err
	}
	if err := validIdem(n.IdempotencyKey); err != nil {
		return err
	}
	if n.Priority != nil && !n.Priority.Valid() {
		return fmt.Errorf("%w: priority %d", ErrInvalid, *n.Priority)
	}
	var approvers []string
	for _, a := range n.Approvers {
		if !PrincipalPattern.MatchString(a) || Reserved(a) {
			return fmt.Errorf("%w: approver %q is not a principal name, or is reserved", ErrInvalid, a)
		}
		if !slices.Contains(approvers, a) {
			approvers = append(approvers, a)
		}
	}
	if len(approvers) > limit {
		return fmt.Errorf("%w: a gate names at most %d approvers, not %d", ErrInvalid, limit, len(approvers))
	}
	if n.Kind == GateCI && len(approvers) == 0 {
		return fmt.Errorf("%w: a ci gate names the principals that report its check; name at least one", ErrInvalid)
	}
	n.Approvers = approvers
	if n.Approvers == nil {
		n.Approvers = []string{}
	}
	return nil
}

// AddGate creates a gate holding in.Blocks back: a gate issue, its row
// and the blocks edge, in one write. A human gate tells each approver
// (gate.pending); a ci gate, which may hold nothing back, tells nobody.
// Anyone may add one. Invalid input, more approvers than the
// approvers_per_gate limit, a second ci gate for one subject and check, or
// more than checks_per_issue on a subject, is refused with ErrInvalid; an
// issue that does not exist with ErrNotFound; a project at its
// open_gates_per_project limit with ErrBusy. With an idempotency key, a repeat returns the first
// result.
func (s *Store) AddGate(ctx context.Context, actor Actor, in NewGate) (Gate, error) {
	if err := in.normalize(s.opts.Limits.GateApprovers); err != nil {
		return Gate{}, err
	}
	id, err := NewID(s.opts.Prefix)
	if err != nil {
		return Gate{}, err
	}
	var out Gate
	err = s.write(ctx, actor, func(w *wtx) error {
		if done, err := replay(ctx, w, in.IdempotencyKey, "gate.add", in, &out); done || err != nil {
			return err
		}
		g, err := s.insertGate(ctx, w, id, in)
		if err != nil {
			return err
		}
		out = g.Gate
		return w.settle(out)
	})
	if err != nil {
		return Gate{}, err
	}
	return out, nil
}

// insertGate makes gate id from in, normalized, in w: its issue, its
// row, its gate.add event and the edge to what it holds, and tells whom
// it waits for. It refuses as AddGate says, and returns the gate.
func (s *Store) insertGate(ctx context.Context, w *wtx, id IssueID, in NewGate) (*gateRow, error) {
	approvers, err := json.Marshal(in.Approvers)
	if err != nil {
		return nil, fmt.Errorf("gate approvers: %w", err)
	}
	// The issue the gate is about: the one it holds back, or a ci gate's
	// subject when it holds none.
	about := cmp.Or(in.Blocks, in.Subject, in.review)
	held, err := loadIssue(ctx, w.tx, about)
	if err != nil {
		return nil, err
	}
	if in.Kind == GateCI {
		if err := checkNewCI(ctx, w, in); err != nil {
			return nil, err
		}
	}
	n, err := openGates(ctx, w.tx)
	if err != nil {
		return nil, err
	}
	if n >= w.lim.OpenGates {
		return nil, fmt.Errorf("%w: the project has %d open gates, its limit (open_gates_per_project)", ErrBusy, n)
	}
	prio := held.Priority
	if in.Priority != nil {
		prio = *in.Priority
	}
	ni := NewIssue{Title: in.Title}
	if err := ni.normalize(); err != nil {
		return nil, err
	}
	ni.Type, ni.Priority = TypeGate, &prio
	if _, _, err := insertIssue(ctx, w, id, ni, nil, false); err != nil {
		return nil, err
	}
	if _, err := w.exec(ctx, `INSERT INTO gates (issue_id, kind, approvers, outcome, decided_by, decided_at, reason, check_name, subject, report, rev)
  VALUES ($1, $2, $3, NULL, NULL, NULL, '', $4, $5, NULL, 1)`, string(id), string(in.Kind), string(approvers),
		in.Check, nullStr(cmp.Or(in.Subject, in.review))); err != nil {
		return nil, fmt.Errorf("insert gate: %w", err)
	}
	added := map[string]any{"kind": in.Kind, "approvers": in.Approvers, "blocks": nullStr(in.Blocks)}
	if in.Kind == GateCI {
		added["check"], added["subject"] = in.Check, in.Subject
	}
	if in.review != "" {
		added["review"] = in.review
	}
	if err := w.event(ctx, OpGateAdd, string(id), nil, added); err != nil {
		return nil, err
	}
	if in.Blocks != "" {
		if _, err := insertDep(ctx, w, in.Blocks, id, DepBlocks); err != nil {
			return nil, err
		}
	}
	g, err := loadGate(ctx, w.tx, id)
	if err != nil {
		return nil, err
	}
	if err := w.tellPending(ctx, s.waitsFor(g.Gate), id, held); err != nil {
		return nil, err
	}
	w.gated(g.Gate, GateChangeAdded)
	return g, nil
}

// waitsFor returns whom gate g waits for, to be told of the work it
// holds: the principals a human gate names, or when it names none the
// project's admins other than its creator, who may not pass it (decision
// 68), at most approvers_per_gate of them, as many as it could have
// named, so that a server of many admins tells a bounded few. The
// project's own admins come first (Options.ListAdmins), then the
// configured ones. A ci gate's principals are CI jobs, which read no
// inbox: none.
func (s *Store) waitsFor(g Gate) []string {
	if g.Kind != GateHuman {
		return nil
	}
	if len(g.Approvers) > 0 {
		return g.Approvers
	}
	var listed []string
	if s.opts.ListAdmins != nil {
		listed = s.opts.ListAdmins()
	}
	var out []string
	for _, p := range slices.Concat(listed, s.opts.Admins) {
		if p == g.CreatedBy || slices.Contains(out, p) || !s.IsAdmin(p) {
			continue
		}
		if out = append(out, p); len(out) == s.opts.Limits.GateApprovers {
			break
		}
	}
	return out
}

// tellPending tells each of to that gate id holds held (gate.pending).
func (w *wtx) tellPending(ctx context.Context, to []string, id IssueID, held Issue) error {
	for _, a := range to {
		if err := w.notify(ctx, InboxItem{To: a, Kind: InboxGatePending, Issue: id,
			Body: fmt.Sprintf("holds %s %s: pass it with `aries gate pass %s`", held.ID, held.Title, id)}); err != nil {
			return err
		}
	}
	return nil
}

// tellHeld tells whom gate id waits for that it now holds issue from
// too, when id is an open gate: the work behind it grew.
func (s *Store) tellHeld(ctx context.Context, w *wtx, id, from IssueID) error {
	g, err := loadGate(ctx, w.tx, id)
	if err != nil || g == nil || !g.open {
		return err
	}
	held, err := loadIssue(ctx, w.tx, from)
	if err != nil {
		return err
	}
	return w.tellPending(ctx, s.waitsFor(g.Gate), id, held)
}

// PassGate passes an open human gate: it closes the gate's issue, which
// lets what it held become ready, and tells the assignees of what it held
// (gate.passed). Only those the gate names, or the project's admins other
// than its creator when it names none, may pass it, and only from the
// session cli: anyone else is refused with a [*GateError]. A closed gate,
// or an issue that is no gate, is refused with ErrInvalid.
func (s *Store) PassGate(ctx context.Context, actor Actor, id IssueID, reason string) (Gate, error) {
	return s.decide(ctx, actor, id, reason, GatePassed)
}

// WaiveGate passes an open gate as an admin may, whoever it names, with a
// reason, which is required. Anyone but an admin in the session cli is
// refused with a [*GateError].
func (s *Store) WaiveGate(ctx context.Context, actor Actor, id IssueID, reason string) (Gate, error) {
	if strings.TrimSpace(reason) == "" {
		return Gate{}, fmt.Errorf("%w: a waive says why", ErrInvalid)
	}
	return s.decide(ctx, actor, id, reason, GateWaived)
}

// RearmGate reopens a passed or waived gate, so that it holds its issues
// again, by those it names or an admin, from the session cli; anyone
// else is refused with a [*GateError]. An open gate is refused with
// ErrInvalid. Work already begun on what it holds is not stopped.
func (s *Store) RearmGate(ctx context.Context, actor Actor, id IssueID, reason string) (Gate, error) {
	return s.decide(ctx, actor, id, reason, "")
}

// decide passes (GatePassed), waives (GateWaived) or rearms (empty) gate
// id, as PassGate, WaiveGate and RearmGate say.
func (s *Store) decide(ctx context.Context, actor Actor, id IssueID, reason string, outcome GateOutcome) (Gate, error) {
	if err := id.Validate(); err != nil {
		return Gate{}, err
	}
	if err := checkLine("reason", reason, maxReason, false); err != nil {
		return Gate{}, err
	}
	op, verb, change := OpGateRearm, "rearm", GateChangeRearmed
	switch outcome {
	case GatePassed:
		op, verb, change = OpGatePass, "pass", GateChangePassed
	case GateWaived:
		op, verb, change = OpGateWaive, "waive", GateChangeWaived
	}
	var out Gate
	err := s.write(ctx, actor, func(w *wtx) error {
		before, err := loadIssue(ctx, w.tx, id)
		if err != nil {
			return err
		}
		g, err := loadGate(ctx, w.tx, id)
		if err != nil {
			return err
		}
		if g == nil {
			return fmt.Errorf("%w: issue %s is not a gate", ErrInvalid, id)
		}
		refuse := func(r GateReason) error {
			return &GateError{ID: id, Gate: id, Approvers: g.Approvers, Check: g.Check, Reason: r, Op: verb}
		}
		switch {
		case outcome == GateWaived && !w.admin:
			return refuse(GateNotAdmin)
		case outcome == GatePassed && !w.approves(g), outcome == "" && !w.approves(g) && !w.admin:
			return refuse(GateNotApprover)
		case w.sessionRefusal(g, outcome) != "":
			return refuse(w.sessionRefusal(g, outcome))
		case g.Kind == GateCI && outcome == GatePassed:
			out, err = w.report(ctx, before, g, Report{Reason: reason}, CheckPassed)
			return err
		case outcome != "" && !g.open:
			return &StateError{ID: id, Reason: StateAlreadyClosed}
		case outcome == "" && g.open:
			return &StateError{ID: id, Reason: StateNotClosed}
		}
		var told []InboxItem
		if outcome != "" {
			// Read before the close: once the gate is closed, the edges
			// into it hold nothing.
			if told, err = heldAssignees(ctx, w.tx, g); err != nil {
				return err
			}
		}
		var decidedBy, decidedAt, set any
		if outcome != "" {
			decidedBy, decidedAt, set = w.actor.Principal, w.now, string(outcome)
		}
		report, err := rearmedReport(g, outcome)
		if err != nil {
			return err
		}
		if _, err := w.exec(ctx, `UPDATE gates SET outcome = $1, decided_by = $2, decided_at = $3, reason = $4, report = $5, rev = rev + 1
  WHERE issue_id = $6`, set, decidedBy, decidedAt, reason, report, string(id)); err != nil {
			return fmt.Errorf("%s gate %s: %w", verb, id, err)
		}
		if err := w.event(ctx, op, string(id), map[string]any{"outcome": nullStr(g.Outcome)},
			map[string]any{"outcome": nullStr(outcome), "reason": reason}); err != nil {
			return err
		}
		if outcome != "" {
			_, err = setStatus(ctx, w, before, OpIssueClose, []any{string(StatusClosed), w.now, nullStr(reason)}, nil)
		} else {
			_, err = setStatus(ctx, w, before, OpIssueReopen, []any{string(StatusOpen), nil, nil}, nil)
		}
		if err != nil {
			return err
		}
		if outcome != "" && g.Kind == GateHuman && g.Subject != "" {
			// A review holds nothing: tell who works on its subject.
			people, err := w.subjectPeople(ctx, g.Subject)
			if err != nil {
				return err
			}
			for _, it := range people {
				it.Kind, it.Issue = InboxGatePassed, g.Subject
				it.Body = fmt.Sprintf("review gate %s %s by %s: finish %s when its items are met", id, outcome, w.actor.Principal, g.Subject)
				if err := w.notify(ctx, it); err != nil {
					return err
				}
			}
		}
		for _, it := range told {
			it.Body = fmt.Sprintf("gate %s %s by %s: %s", id, outcome, w.actor.Principal, it.Body)
			if err := w.notify(ctx, it); err != nil {
				return err
			}
		}
		after, err := loadGate(ctx, w.tx, id)
		if err != nil {
			return err
		}
		out = after.Gate
		w.gated(out, change)
		return nil
	})
	if err != nil {
		return Gate{}, err
	}
	return out, nil
}

// rearmedReport is g's report as a decision of outcome leaves it: as it
// is, but that a rearm (outcome "") forgets its run, so that CI's next
// report is taken whatever run it names (Report.stale). nil for none.
func rearmedReport(g *gateRow, outcome GateOutcome) (any, error) {
	if g.Report == nil {
		return nil, nil
	}
	r := *g.Report
	if outcome == "" {
		r.Run = 0
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("gate %s report: %w", g.ID, err)
	}
	return string(b), nil
}

// maxGateNotices bounds the assignees a gate's pass tells.
const maxGateNotices = 100

// heldAssignees returns an item for each assignee of an unclosed issue g
// holds, at most maxGateNotices, each naming the first such issue of
// theirs and its title.
func heldAssignees(ctx context.Context, q querier, g *gateRow) ([]InboxItem, error) {
	// One recursive term over the edges, as holdingGates has it: the SQL
	// standard, and so Postgres, refuses a CTE with two.
	rows, err := q.QueryContext(ctx, `WITH RECURSIVE
edges (src, dst) AS (
  SELECT d.to_id, d.from_id FROM deps d JOIN issues f ON f.id = d.from_id
  WHERE d.type IN (`+readyBlocking+`) AND f.status <> 'closed'
  UNION ALL
  SELECT parent_id, id FROM issues WHERE parent_id IS NOT NULL
),
held (id) AS (
  SELECT CAST($1 AS VARCHAR(64))
  UNION
  SELECT e.dst FROM edges e JOIN held h ON e.src = h.id
)
SELECT i.assignee, i.id, i.title FROM held h JOIN issues i ON i.id = h.id
WHERE i.id <> $1 AND i.status <> 'closed' AND i.assignee IS NOT NULL ORDER BY i.assignee, i.id LIMIT $2`,
		string(g.ID), maxGateNotices*10)
	if err != nil {
		return nil, fmt.Errorf("held by gate %s: %w", g.ID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []InboxItem
	for rows.Next() {
		var to string
		var id IssueID
		var title string
		if err := rows.Scan(&to, &id, &title); err != nil {
			return nil, fmt.Errorf("held by gate %s: %w", g.ID, err)
		}
		if !PrincipalPattern.MatchString(to) || len(out) > 0 && out[len(out)-1].To == to {
			continue
		}
		if len(out) == maxGateNotices {
			break
		}
		out = append(out, InboxItem{To: to, Kind: InboxGatePassed, Issue: id, Body: string(id) + " " + title + " may be ready"})
	}
	return out, rows.Err()
}

// Gates returns the gates f selects, from one snapshot.
func (s *Store) Gates(ctx context.Context, f GateFilter) ([]Gate, error) {
	limit := clampLimit(f.Limit, 50, 500)
	tx, end, err := s.beginRead(ctx)
	if err != nil {
		return nil, fmt.Errorf("gates: %w", err)
	}
	defer end()
	where := `WHERE g.outcome IS NULL`
	if f.All {
		where = ``
	}
	rows, err := loadGates(ctx, tx, where, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Gate, len(rows))
	for i, g := range rows {
		out[i] = g.Gate
	}
	return out, nil
}

// GateOf returns the gate whose issue is id, or nil when id is no gate.
func (s *Store) GateOf(ctx context.Context, id IssueID) (*Gate, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	g, err := loadGate(ctx, s.r, id)
	if err != nil || g == nil {
		return nil, err
	}
	return &g.Gate, nil
}

// GatesHolding returns the open gates that hold issue id, from one
// snapshot, in id order.
func (s *Store) GatesHolding(ctx context.Context, id IssueID) ([]Gate, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	tx, end, err := s.beginRead(ctx)
	if err != nil {
		return nil, fmt.Errorf("gates: %w", err)
	}
	defer end()
	ids, err := holdingGates(ctx, tx, id)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	return gatesOf(ctx, tx, ids)
}

// GatesOf returns the open gates among ids, at most 500 of them, in the
// order Gates gives; an id that is no open gate is left out.
func (s *Store) GatesOf(ctx context.Context, ids []IssueID) ([]Gate, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return gatesOf(ctx, s.r, ids[:min(len(ids), 500)])
}

// gatesOf is GatesOf on q.
func gatesOf(ctx context.Context, q querier, ids []IssueID) ([]Gate, error) {
	args := make([]any, len(ids))
	for i, g := range ids {
		args[i] = string(g)
	}
	rows, err := loadGates(ctx, q, `WHERE g.outcome IS NULL AND g.issue_id IN (`+placeholders(1, len(args))+`)`, len(args), args...)
	if err != nil {
		return nil, err
	}
	out := make([]Gate, len(rows))
	for i, g := range rows {
		out[i] = g.Gate
	}
	return out, nil
}
