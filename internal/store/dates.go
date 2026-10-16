package store

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Dates on an issue (design §20.3): defer_until, not before, and due_at,
// due by. Ready leaves an issue out until its defer_until, on itself or
// an ancestor. Neither date ends, expires or reassigns anything when it
// passes; Dates and Digest notice them when read, from the store's time
// (rule 19), with no timer.

// DefaultDueSoon is how far ahead an issue's due date makes it due soon,
// when the reader gives no window: the server's due_soon setting.
const DefaultDueSoon = 24 * time.Hour

// MaxDueSoon bounds the due-soon window.
const MaxDueSoon = 366 * 24 * time.Hour

// DatesFilter selects what Dates reads: issues whose defer_until passed
// since Since (zero means a day back from now), and those due within
// DueSoon of now (zero means DefaultDueSoon).
type DatesFilter struct {
	Since   time.Time
	DueSoon time.Duration
}

// Dates is what dates say at Now. Each section is a capped list and the
// count of what it matched; an item's At is its date, and By its
// assignee.
type Dates struct {
	Since, Now time.Time
	// Ready lists the issues ready now whose defer_until passed after
	// Since: those that became ready because their date came. Earliest
	// date first.
	Ready DigestSection
	// DueSoon lists unclosed issues due after now and within the window,
	// soonest first.
	DueSoon DigestSection
	// Overdue lists unclosed issues due at or before now, longest
	// overdue first.
	Overdue DigestSection
	// Capped reports that a section matched more rows than are read, so
	// its total is a lower bound.
	Capped bool
}

// Dates reads what f selects in one snapshot. A Since in the future, or
// more than MaxDigestWindow back, and a window outside zero to
// MaxDueSoon, are refused with ErrInvalid.
func (s *Store) Dates(ctx context.Context, f DatesFilter) (Dates, error) {
	now := s.now()
	since := f.Since.UTC()
	if since.IsZero() {
		since = now.Add(-24 * time.Hour)
	}
	switch {
	case since.After(now):
		return Dates{}, fmt.Errorf("%w: since %s is in the future", ErrInvalid, since.Format(time.RFC3339))
	case now.Sub(since) > MaxDigestWindow:
		return Dates{}, fmt.Errorf("%w: since reaches back more than %d days", ErrInvalid, MaxDigestWindow/(24*time.Hour))
	case f.DueSoon < 0 || f.DueSoon > MaxDueSoon:
		return Dates{}, fmt.Errorf("%w: due soon must be a window of up to %d days", ErrInvalid, MaxDueSoon/(24*time.Hour))
	}
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Dates{}, fmt.Errorf("dates: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // read-only: nothing to keep
	q := &digestQuery{tx: tx, f: DigestFilter{Since: since, DueSoon: f.DueSoon}, now: now}
	d := Dates{Since: since, Now: now}
	if err := q.dates(ctx, &d); err != nil {
		return Dates{}, fmt.Errorf("dates: %w", err)
	}
	d.Capped = q.capped
	return d, nil
}

// dates fills in d's sections, as of q.now, for the digest's window and
// due-soon window, filtered by its By (the assignee) and Label.
func (q *digestQuery) dates(ctx context.Context, d *Dates) error {
	soon := cmp.Or(q.f.DueSoon, DefaultDueSoon)
	const cols = `SELECT i.id, i.title, i.priority, COALESCE(i.assignee, ''), `
	// Each query's own arguments come first, numbered in its text; the
	// filters' and the limit follow.
	for _, sc := range []struct {
		name  string
		out   *DigestSection
		query string
		args  []any
		order string
	}{
		{"ready", &d.Ready, blockedCTE + cols + `i.defer_until FROM issues i
WHERE i.status = 'open' AND i.template = FALSE AND i.type <> 'gate'
  AND i.id NOT IN (SELECT id FROM blocked) AND i.id NOT IN (SELECT id FROM deferred)
  AND i.id NOT IN (SELECT id FROM waiting)
  AND i.defer_until > $2 AND i.defer_until <= $1`, []any{q.now, q.f.Since}, ` ORDER BY i.defer_until, i.id`},
		{"due soon", &d.DueSoon, cols + `i.due_at FROM issues i
WHERE i.status <> 'closed' AND i.due_at > $1 AND i.due_at <= $2`, []any{q.now, q.now.Add(soon)}, ` ORDER BY i.due_at, i.id`},
		{"overdue", &d.Overdue, cols + `i.due_at FROM issues i
WHERE i.status <> 'closed' AND i.due_at <= $1`, []any{q.now}, ` ORDER BY i.due_at, i.id`},
	} {
		p := params{args: sc.args}
		query := sc.query + q.filters(&p, "i.assignee", "i.id") + sc.order + ` LIMIT ` + p.add(digestRows+1)
		items, err := q.dated(ctx, query, p.args)
		if err != nil {
			return fmt.Errorf("%s: %w", sc.name, err)
		}
		*sc.out = DigestSection{Total: len(items), Items: items[:min(len(items), DigestItems)]}
	}
	return nil
}

// dated runs one of dates' queries, which select an issue's id, title,
// priority, assignee and date, and reads at most digestRows rows.
func (q *digestQuery) dated(ctx context.Context, query string, args []any) ([]DigestItem, error) {
	rows, err := q.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []DigestItem
	for rows.Next() {
		var it DigestItem
		if err := rows.Scan(&it.ID, &it.Title, &it.Priority, &it.By, &it.At); err != nil {
			return nil, err
		}
		if len(out) == digestRows {
			q.capped = true
			break
		}
		it.At = it.At.UTC()
		out = append(out, it)
	}
	return out, rows.Err()
}
