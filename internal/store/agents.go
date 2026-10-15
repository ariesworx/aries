package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ariesworx/aries/internal/lazyregexp"
)

// Agents registry (design §3, §7): who is at work, on which machine and
// under which harness, and what each session holds. The server touches a
// session's row when it connects and on each claim renewal; Who lists
// the rows seen within a window. Touches are presence, not history, so
// like claim renewals they record no event.

// Registry timing. A row is rewritten at most every AgentTouchEvery
// unless the machine or harness changed; Who's default window is
// AgentActiveFor, and its widest MaxAgentWindow.
const (
	AgentTouchEvery = time.Minute
	AgentActiveFor  = 5 * time.Minute
	MaxAgentWindow  = 7 * 24 * time.Hour
)

// Agent is one session in the registry, with the issues it holds under
// an active claim.
type Agent struct {
	Actor
	Harness  string    `json:"harness,omitempty"`
	Started  time.Time `json:"started"`
	LastSeen time.Time `json:"last_seen"`
	Claims   []IssueID `json:"claims,omitempty"`
}

// harnessPattern is what a harness name may look like: the `aries setup`
// agent names and others like them.
var harnessPattern = lazyregexp.New(`^[a-z0-9-]{1,32}$`)

// ValidHarness reports whether h may be recorded: empty, or 1-32
// lowercase letters, digits and hyphens.
func ValidHarness(h string) bool { return h == "" || harnessPattern.MatchString(h) }

// TouchAgent records that actor's session is present, running under
// harness. It writes only when the row is new, the machine or harness
// changed, or last_seen is at least AgentTouchEvery old, and records no
// event. An empty harness keeps the one already recorded, so a client
// that does not send one does not erase it; a harness that is not valid
// is refused with ErrInvalid. A new session past the Sessions limit drops
// the principal's least recently seen rows.
func (s *Store) TouchAgent(ctx context.Context, actor Actor, harness string) error {
	if !ValidHarness(harness) {
		return fmt.Errorf("%w: harness %q must be 1-32 lowercase letters, digits or hyphens", ErrInvalid, harness)
	}
	if err := actor.validate(); err != nil {
		return err
	}
	// Most touches find the row fresh. Such a one is decided from a read,
	// and does not wait for the writer behind other sessions' writes. It
	// settles a step of the clock first, as a write would.
	now, err := s.settledNow(ctx)
	if err != nil {
		return err
	}
	switch a, err := readAgent(ctx, s.r, actor); {
	case err == nil && a.fresh(actor, harness, now):
		return nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return err
	}
	return s.write(ctx, actor, func(w *wtx) error {
		a, err := readAgent(ctx, w.tx, actor)
		if errors.Is(err, sql.ErrNoRows) {
			wid := randomInt63()
			if _, err := w.exec(ctx, `INSERT INTO agents
  (principal, session, machine, harness, started, last_seen, rev, write_id) VALUES (?, ?, ?, ?, ?, ?, 1, ?)`,
				actor.Principal, actor.Session, actor.Machine, harness, w.now, w.now, wid); err != nil {
				return fmt.Errorf("insert agent %s/%s: %w", actor.Principal, actor.Session, err)
			}
			w.quiet = true
			return capSessions(ctx, w, actor.Principal)
		}
		if err != nil {
			return err
		}
		if a.fresh(actor, harness, w.now) {
			return nil
		}
		wid := randomInt63()
		n, err := w.exec(ctx, `UPDATE agents SET machine = ?, harness = ?, last_seen = ?, rev = rev + 1, write_id = ?
  WHERE principal = ? AND session = ? AND rev = ?`,
			actor.Machine, a.harness(harness), w.now, wid, actor.Principal, actor.Session, a.rev)
		if err != nil {
			return fmt.Errorf("update agent %s/%s: %w", actor.Principal, actor.Session, err)
		}
		if n != 1 {
			return fmt.Errorf("agent %s/%s changed during update: %w", actor.Principal, actor.Session, ErrConflict)
		}
		w.quiet = true
		return nil
	})
}

// sessionRow is what TouchAgent reads of a session's row: known is its harness.
type sessionRow struct {
	machine, known string
	seen           time.Time
	rev            int64
}

// readAgent reads actor's session's row, or returns sql.ErrNoRows.
func readAgent(ctx context.Context, q querier, actor Actor) (sessionRow, error) {
	var a sessionRow
	err := q.QueryRowContext(ctx, `SELECT machine, harness, last_seen, rev FROM agents
  WHERE principal = ? AND session = ?`, actor.Principal, actor.Session).Scan(&a.machine, &a.known, &a.seen, &a.rev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return sessionRow{}, fmt.Errorf("read agent %s/%s: %w", actor.Principal, actor.Session, err)
	}
	return a, err
}

// harness is the harness a touch with harness records: the one given, or
// the one known when it is empty. It is worked out on each attempt from
// the argument, not kept, so a rerun goes from the row it reads.
func (a sessionRow) harness(harness string) string {
	if harness == "" {
		return a.known
	}
	return harness
}

// fresh reports whether a touch of actor with harness at now has nothing
// to write: the machine and harness are the row's, and the row was seen
// less than AgentTouchEvery ago.
func (a sessionRow) fresh(actor Actor, harness string, now time.Time) bool {
	return a.machine == actor.Machine && a.harness(harness) == a.known && now.Sub(a.seen) < AgentTouchEvery
}

// Who returns the sessions seen within since of the server's now (0
// means AgentActiveFor), most recently seen first, each with the issues
// it holds under an active claim, from one snapshot. A window below 0 or
// past MaxAgentWindow is refused with ErrInvalid.
func (s *Store) Who(ctx context.Context, since time.Duration) ([]Agent, error) {
	if since == 0 {
		since = AgentActiveFor
	}
	if since < 0 || since > MaxAgentWindow {
		return nil, fmt.Errorf("%w: who's window must be up to 7d", ErrInvalid)
	}
	now := s.now()
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("who: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // read-only: nothing to keep

	rows, err := tx.QueryContext(ctx, `SELECT principal, session, machine, harness, started, last_seen FROM agents
  WHERE last_seen >= ? ORDER BY last_seen DESC, principal, session`, now.Add(-since))
	if err != nil {
		return nil, fmt.Errorf("who: %w", err)
	}
	out := []Agent{}
	at := map[Actor]int{} // by principal and session only
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.Principal, &a.Session, &a.Machine, &a.Harness, &a.Started, &a.LastSeen); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("who: %w", err)
		}
		a.Started, a.LastSeen = a.Started.UTC(), a.LastSeen.UTC()
		at[Actor{Principal: a.Principal, Session: a.Session}] = len(out)
		out = append(out, a)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("who: %w", err)
	}
	if len(out) == 0 {
		return out, nil
	}
	held, err := s.claims(ctx, tx, `principal IS NOT NULL AND expires_at > ?`, now)
	if err != nil {
		return nil, err
	}
	for _, c := range held {
		if i, ok := at[Actor{Principal: c.Holder.Principal, Session: c.Holder.Session}]; ok {
			out[i].Claims = append(out[i].Claims, c.Issue)
		}
	}
	for i := range out {
		slices.Sort(out[i].Claims)
	}
	return out, nil
}

// SessionsSeen returns how many sessions were seen within since of the
// server's now (0 means AgentActiveFor): how many rows [Store.Who] would
// list, counted on the index by last_seen without reading them. A window
// below 0 or past MaxAgentWindow is refused with ErrInvalid.
func (s *Store) SessionsSeen(ctx context.Context, since time.Duration) (int, error) {
	if since == 0 {
		since = AgentActiveFor
	}
	if since < 0 || since > MaxAgentWindow {
		return 0, fmt.Errorf("%w: who's window must be up to 7d", ErrInvalid)
	}
	var n int
	if err := s.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM agents WHERE last_seen >= ?`, s.now().Add(-since)).Scan(&n); err != nil {
		return 0, fmt.Errorf("sessions seen: %w", err)
	}
	return n, nil
}

// capSessions deletes principal's least recently seen registry rows past
// the Sessions limit (S-6): each handshake with a fresh session id adds a
// row, and nothing else would bound them.
func capSessions(ctx context.Context, w *wtx, principal string) error {
	var n int
	if err := w.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agents WHERE principal = ?`, principal).Scan(&n); err != nil {
		return fmt.Errorf("count sessions of %s: %w", principal, err)
	}
	if n <= w.lim.Sessions {
		return nil
	}
	rows, err := w.tx.QueryContext(ctx, `SELECT session FROM agents WHERE principal = ?
  ORDER BY last_seen, session LIMIT ?`, principal, n-w.lim.Sessions)
	if err != nil {
		return fmt.Errorf("oldest sessions of %s: %w", principal, err)
	}
	var old []string
	for rows.Next() {
		var sess string
		if err := rows.Scan(&sess); err != nil {
			_ = rows.Close()
			return fmt.Errorf("oldest sessions of %s: %w", principal, err)
		}
		old = append(old, sess)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("oldest sessions of %s: %w", principal, err)
	}
	for _, sess := range old {
		if _, err := w.exec(ctx, `DELETE FROM agents WHERE principal = ? AND session = ?`, principal, sess); err != nil {
			return fmt.Errorf("drop session %s/%s: %w", principal, sess, err)
		}
	}
	return nil
}
