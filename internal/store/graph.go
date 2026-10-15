package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// AddDep records that from depends on to. Adding an existing edge is a
// no-op. A blocking edge that would close a cycle of blocking edges and
// parent links is refused with ErrCycle; an edge past the Deps limit, or
// a non-blocking edge from an issue to itself, with ErrInvalid; and a
// missing issue with ErrNotFound. Dependencies need no hold. With an
// idempotency key (idem), a repeat of an add that added the edge writes
// nothing, and the key reused for another request is refused with an
// [*IdemError].
func (s *Store) AddDep(ctx context.Context, actor Actor, from, to IssueID, typ DepType, idem string) error {
	if err := validEdge(from, to, typ); err != nil {
		return err
	}
	if err := validIdem(idem); err != nil {
		return err
	}
	return s.write(ctx, actor, func(w *wtx) error {
		var done struct{}
		if again, err := replay(ctx, w, idem, "dep.add", Dep{From: from, To: to, Type: typ}, &done); again || err != nil {
			return err
		}
		for _, id := range []IssueID{from, to} {
			if err := mustExist(ctx, w.tx, id); err != nil {
				return err
			}
		}
		if err := insertDep(ctx, w, from, to, typ); err != nil {
			return err
		}
		return w.settleChanged(struct{}{})
	})
}

// insertDep adds the edge from→to, recording an event, unless it is
// already there. Both issues must exist.
func insertDep(ctx context.Context, w *wtx, from, to IssueID, typ DepType) error {
	var n int
	if err := w.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM deps WHERE from_id = $1 AND to_id = $2 AND type = $3`,
		string(from), string(to), string(typ)).Scan(&n); err != nil {
		return fmt.Errorf("lookup dep: %w", err)
	}
	if n > 0 {
		return nil
	}
	if err := w.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM deps WHERE from_id = $1`, string(from)).Scan(&n); err != nil {
		return fmt.Errorf("count deps: %w", err)
	}
	if n >= w.lim.Deps {
		return fmt.Errorf("%w: issue %s has %d dependencies; an issue has at most %d dependencies", ErrInvalid, from, n, w.lim.Deps)
	}
	if typ.Blocking() {
		if err := checkEdge(ctx, w.tx, from, to); err != nil {
			return err
		}
	}
	if _, err := w.exec(ctx, `INSERT INTO deps (from_id, to_id, type, created_by, created_at, rev)
  VALUES ($1, $2, $3, $4, $5, 1)`, string(from), string(to), string(typ), w.actor.Principal, w.now); err != nil {
		return fmt.Errorf("insert dep: %w", err)
	}
	d := Dep{From: from, To: to, Type: typ, CreatedBy: w.actor.Principal, CreatedAt: w.now}
	return w.event(ctx, OpDepAdd, string(from), nil, d)
}

// RemoveDep deletes an edge. Removing a missing edge is a no-op.
func (s *Store) RemoveDep(ctx context.Context, actor Actor, from, to IssueID, typ DepType) error {
	if err := validEdge(from, to, typ); err != nil {
		return err
	}
	return s.write(ctx, actor, func(w *wtx) error {
		holds, err := holdingGates(ctx, w.tx, from)
		if err != nil {
			return err
		}
		n, err := w.exec(ctx, `DELETE FROM deps WHERE from_id = $1 AND to_id = $2 AND type = $3`,
			string(from), string(to), string(typ))
		if err != nil {
			return fmt.Errorf("delete dep: %w", err)
		}
		if n == 0 {
			return nil
		}
		if err := w.checkHolds(ctx, from, holds, "unlink"); err != nil {
			return err
		}
		return w.event(ctx, OpDepRemove, string(from), Dep{From: from, To: to, Type: typ}, nil)
	})
}

// validEdge checks an edge's ends and type. A blocking edge from an issue
// to itself passes, so that the cycle check refuses it with ErrCycle; any
// other edge to itself is ErrInvalid.
func validEdge(from, to IssueID, typ DepType) error {
	if err := from.Validate(); err != nil {
		return err
	}
	if err := to.Validate(); err != nil {
		return err
	}
	if !typ.Valid() {
		return fmt.Errorf("%w: dependency type %q", ErrInvalid, typ)
	}
	if from == to && !typ.Blocking() {
		return fmt.Errorf("%w: issue %s cannot depend on itself", ErrInvalid, from)
	}
	return nil
}

// checkEdge refuses, with ErrCycle, a new waits-on edge from→to (a
// blocking dep, or a parent link) when to already reaches from.
func checkEdge(ctx context.Context, q querier, from, to IssueID) error {
	if from == to {
		return fmt.Errorf("%s depends on itself: %w", from, ErrCycle)
	}
	var n int
	err := q.QueryRowContext(ctx, `WITH RECURSIVE
edges (src, dst) AS (
  SELECT from_id, to_id FROM deps WHERE type IN ('blocks','conditional-blocks','waits-for')
  UNION ALL
  SELECT id, parent_id FROM issues WHERE parent_id IS NOT NULL
),
reach (id) AS (
  SELECT CAST($1 AS VARCHAR(64))
  UNION
  SELECT e.dst FROM edges e JOIN reach r ON e.src = r.id
)
SELECT COUNT(*) FROM reach WHERE id = $2`, string(to), string(from)).Scan(&n)
	if err != nil {
		return fmt.Errorf("cycle check: %w", err)
	}
	if n > 0 {
		return fmt.Errorf("%s already depends on %s: %w", to, from, ErrCycle)
	}
	return nil
}

// Deps returns the edges into and out of an issue, ordered by from, to
// and type.
func (s *Store) Deps(ctx context.Context, id IssueID) ([]Dep, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	return issueDeps(ctx, s.r, id)
}

// issueDeps is Deps on q.
func issueDeps(ctx context.Context, q querier, id IssueID) ([]Dep, error) {
	return deps(ctx, q, `WHERE from_id = $1 OR to_id = $2`, string(id), string(id))
}

// AllDeps returns every edge, ordered by from, to and type.
func (s *Store) AllDeps(ctx context.Context) ([]Dep, error) {
	return deps(ctx, s.r, ``)
}

// deps reads the edges that where, a constant clause with placeholders
// for args, selects, on q.
func deps(ctx context.Context, q querier, where string, args ...any) ([]Dep, error) {
	query := `SELECT from_id, to_id, type, created_by, created_at, metadata FROM deps ` + where + //nolint:gosec // where is a constant from the callers above
		` ORDER BY from_id, to_id, type`
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("deps: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Dep
	for rows.Next() {
		var d Dep
		var meta []byte
		if err := rows.Scan(&d.From, &d.To, &d.Type, &d.CreatedBy, &d.CreatedAt, &meta); err != nil {
			return nil, fmt.Errorf("deps: %w", err)
		}
		d.CreatedAt = d.CreatedAt.UTC()
		if len(meta) > 0 {
			d.Metadata = json.RawMessage(meta)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("deps: %w", err)
	}
	return out, nil
}

// blockedCTE yields blocked(id, via): unclosed issues with an unclosed
// target of a readyBlocking edge, and their descendants, with via naming
// the ancestor whose blockers apply and depth how far up the tree it is
// (0: the issue itself). Callers that report one via order by depth, so
// the nearest ancestor is named whatever order the engine emits rows in.
// deferred(id) is the same for issues deferred by status or by a future
// defer_until. Its one parameter, now, is the statement's first ($1).
const blockedCTE = `WITH RECURSIVE
direct (id) AS (
  SELECT DISTINCT d.from_id FROM deps d
  JOIN issues f ON f.id = d.from_id
  JOIN issues t ON t.id = d.to_id
  WHERE d.type IN (` + readyBlocking + `) AND f.status <> 'closed' AND t.status <> 'closed'
),
blocked (id, via, depth) AS (
  SELECT id, id, 0 FROM direct
  UNION
  SELECT c.id, b.via, b.depth + 1 FROM issues c JOIN blocked b ON c.parent_id = b.id
),
deferred (id) AS (
  SELECT id FROM issues WHERE status = 'deferred' OR defer_until > $1
  UNION
  SELECT c.id FROM issues c JOIN deferred d ON c.parent_id = d.id
)
`

// readyFilter selects and orders the ready issues of blockedCTE, best
// first. Ready and StartIssue share it, through rankReady, so start takes
// what ready shows first.
const readyFilter = `WHERE i.status = 'open' AND i.template = FALSE AND i.type <> 'gate'
  AND i.id NOT IN (SELECT id FROM blocked)
  AND i.id NOT IN (SELECT id FROM deferred)
ORDER BY i.priority, i.created_at, i.id`

// clampLimit returns def when n is not positive, and otherwise n capped
// at maxN.
func clampLimit(n, def, maxN int) int {
	if n <= 0 {
		return def
	}
	return min(n, maxN)
}

// Ready returns open issues that nothing holds back: no unclosed blocks or
// conditional-blocks target on the issue or an ancestor, not deferred (by
// status or a future defer_until) on the issue or an ancestor, and not a
// template. Ordered by priority, then age, except that an issue whose
// paths overlap those of an issue another session than actor's holds
// comes after every issue that does not, with those held issues as its
// Overlaps. At most limit (0 means 10, at most 500). Computed at read
// time, from one snapshot.
func (s *Store) Ready(ctx context.Context, actor Actor, limit int) ([]ReadyIssue, error) {
	limit = clampLimit(limit, 10, 500)
	tx, end, err := s.beginRead(ctx)
	if err != nil {
		return nil, fmt.Errorf("ready: %w", err)
	}
	defer end()
	ids, overlaps, err := rankReady(ctx, tx, actor, s.now(), limit)
	if err != nil {
		return nil, err
	}
	sums, err := loadSummaries(ctx, tx, ids)
	if err != nil {
		return nil, fmt.Errorf("ready: %w", err)
	}
	out := make([]ReadyIssue, len(sums))
	for i, sm := range sums {
		out[i] = ReadyIssue{Summary: sm, Overlaps: overlaps[sm.ID]}
	}
	return out, nil
}

// loadSummaries reads the summaries of the issues ids, in the order
// given; an id with no issue is left out.
func loadSummaries(ctx context.Context, q querier, ids []IssueID) ([]Summary, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = string(id)
	}
	rows, err := q.QueryContext(ctx, `SELECT i.id, i.title, i.status, i.priority, i.due_at, i.defer_until FROM issues i WHERE i.id IN (`+
		placeholders(1, len(ids))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	byID := make(map[IssueID]Summary, len(ids))
	for rows.Next() {
		var sm Summary
		var due, deferUntil sql.NullTime
		if err := rows.Scan(&sm.ID, &sm.Title, &sm.Status, &sm.Priority, &due, &deferUntil); err != nil {
			return nil, err
		}
		sm.DueAt, sm.DeferUntil = orZero(due), orZero(deferUntil)
		byID[sm.ID] = sm
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Summary, 0, len(ids))
	for _, id := range ids {
		if sm, ok := byID[id]; ok {
			out = append(out, sm)
		}
	}
	return out, nil
}

// Blocked returns unclosed issues held back by unclosed blockers, their own
// or an ancestor's, ordered by priority, then age, at most limit (0 means
// 50, at most 500), from one snapshot.
//
// An issue blocked through several ancestors has a row for each, and the
// first in the order, its own blockers before an ancestor's, says which
// apply. So the page is counted in issues, not rows: the query numbers
// the issues in the order (DENSE_RANK) and returns the rows of the first
// limit, and no row past the page leaves the database. A query that picked the
// page with IN (SELECT ... FROM blocked) and joined blocked again ran
// for over 20 minutes at 50,000 issues.
func (s *Store) Blocked(ctx context.Context, limit int) ([]BlockedIssue, error) {
	limit = clampLimit(limit, 50, 500)
	tx, end, err := s.beginRead(ctx)
	if err != nil {
		return nil, fmt.Errorf("blocked: %w", err)
	}
	defer end()
	rows, err := tx.QueryContext(ctx, blockedCTE+`SELECT id, title, status, priority, via FROM (
  SELECT i.id, i.title, i.status, i.priority, i.created_at, b.via, b.depth,
    DENSE_RANK() OVER (ORDER BY i.priority, i.created_at, i.id) AS n
  FROM issues i JOIN blocked b ON b.id = i.id
  WHERE i.status <> 'closed'
) x WHERE n <= $2
ORDER BY priority, created_at, id, depth`, s.now(), limit)
	if err != nil {
		return nil, fmt.Errorf("blocked: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []BlockedIssue
	seen := map[IssueID]bool{}
	for rows.Next() {
		var b BlockedIssue
		if err := rows.Scan(&b.ID, &b.Title, &b.Status, &b.Priority, &b.Via); err != nil {
			return nil, fmt.Errorf("blocked: %w", err)
		}
		if seen[b.ID] {
			continue
		}
		seen[b.ID] = true
		if b.Via == b.ID {
			b.Via = ""
		}
		out = append(out, b)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("blocked: %w", err)
	}
	return out, attachBlockers(ctx, tx, out)
}

// attachBlockers fills BlockedBy for each issue: its own unclosed
// blockers, or those of the ancestor it is blocked through (Via).
func attachBlockers(ctx context.Context, q querier, bs []BlockedIssue) error {
	src := make([]IssueID, len(bs))
	for i, b := range bs {
		src[i] = b.ID
		if b.Via != "" {
			src[i] = b.Via
		}
	}
	by, err := blockerMap(ctx, q, src)
	if err != nil {
		return err
	}
	for i := range bs {
		bs[i].BlockedBy = by[src[i]]
	}
	return nil
}

// blockerMap maps each issue of from, at most 500, to its unclosed
// blocking targets, in id order.
func blockerMap(ctx context.Context, q querier, from []IssueID) (map[IssueID][]IssueID, error) {
	by := map[IssueID][]IssueID{}
	if len(from) == 0 {
		return by, nil
	}
	args := make([]any, len(from))
	for i, id := range from {
		args[i] = string(id)
	}
	rows, err := q.QueryContext(ctx, `SELECT d.from_id, d.to_id FROM deps d
JOIN issues t ON t.id = d.to_id
WHERE d.from_id IN (`+placeholders(1, len(from))+`) AND d.type IN (`+readyBlocking+`) AND t.status <> 'closed'
ORDER BY d.from_id, d.to_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("blockers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var f, t IssueID
		if err := rows.Scan(&f, &t); err != nil {
			return nil, fmt.Errorf("blockers: %w", err)
		}
		by[f] = append(by[f], t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("blockers: %w", err)
	}
	return by, nil
}

// withLabels is attachLabels for a slice of issues.
func withLabels(ctx context.Context, q querier, issues []Issue) error {
	ps := make([]*Issue, len(issues))
	for i := range issues {
		ps[i] = &issues[i]
	}
	return attachLabels(ctx, q, ps)
}
