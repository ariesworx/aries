package store

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// Token usage (design §12.1). A harness reports what each request cost,
// as counts by model; aries sends those records here, and AddUsage keeps
// one row per (principal, session, request id), so a batch sent twice is
// stored once. Nothing about an issue is stored with a row: which issue
// it belongs to is worked out when read, from the session's claims
// (attribution.go).

// Granularity says what one usage record covers.
type Granularity string

// Granularities. A request record is one API request, at one instant; a
// turn or session record sums a span, from SpanStart to At.
const (
	GranularityRequest Granularity = "request"
	GranularityTurn    Granularity = "turn"
	GranularitySession Granularity = "session"
)

// OpUsageAdd records one AddUsage call that stored rows. Its after state
// counts what was added and what was already there, and gives the
// earliest and latest record times; the rows themselves are in
// token_usage, so the log does not grow by a row per API request.
const OpUsageAdd Op = "usage.add"

// UsageTarget is the target of usage.add events. It cannot be an issue
// ID, which always has a hyphen; the actor names whose records they were.
const UsageTarget = "usage"

// Usage bounds.
const (
	// MaxUsageCount bounds each count in a record, so sums cannot
	// overflow. A request is far smaller; a whole session's cumulative
	// record fits.
	MaxUsageCount = 1_000_000_000_000
	// DefaultClientAhead is how far past the server's clock a time a
	// client supplies may be ([Options.ClientAhead]): a usage record's.
	// Nobody's synchronized clock is minutes ahead of another's, and a
	// record dated ahead would be priced, attributed and windowed as if
	// it had not happened yet. A time in the past has no such bound:
	// records are legitimately sent late, and the daily cap counts by
	// when the server stored a record, not by the time it claims.
	DefaultClientAhead = 5 * time.Minute
	// MaxClientAhead is the most Options.ClientAhead may be.
	MaxClientAhead = 24 * time.Hour
	// MaxUsageSpan bounds a turn's or session's span, so attribution
	// finds the spans that reach into a hold within that much of its end.
	// A longer session reports in parts.
	MaxUsageSpan = 7 * 24 * time.Hour
)

// usageEpoch is the earliest time a record may carry: older is a client
// bug, not a backfill.
var usageEpoch = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// Tokens are a record's counts. Nil is unknown, which is not 0.
// CacheWrite1h is the part of CacheWrite written with a one-hour
// lifetime.
type Tokens struct {
	Input        *int64 `json:"input,omitempty"`
	Output       *int64 `json:"output,omitempty"`
	CacheWrite   *int64 `json:"cache_write,omitempty"`
	CacheWrite1h *int64 `json:"cache_write_1h,omitempty"`
	CacheRead    *int64 `json:"cache_read,omitempty"`
}

// UsageRecord is what a harness reported for one request, turn or
// session. At is the time the source gives; SpanStart starts a turn's or
// session's span, and a request has none.
type UsageRecord struct {
	Harness     string
	RequestID   string
	Model       string
	At          time.Time
	Granularity Granularity
	SpanStart   *time.Time
	Tokens
}

// UsageAdded counts the records one AddUsage stored, and those it
// already had.
type UsageAdded struct {
	Added      int `json:"added"`
	Duplicates int `json:"duplicates"`
}

// validate refuses, with ErrInvalid, a record with a field out of
// pattern or range, at the server time now, past which its time may be
// at most ahead. n numbers it in the message, from 1.
func (r UsageRecord) validate(n int, now time.Time, ahead time.Duration) error {
	bad := func(field, rule string) error {
		return fmt.Errorf("%w: usage record %d: %s %s", ErrInvalid, n, field, rule)
	}
	switch {
	case !harnessPattern.MatchString(r.Harness):
		return bad("harness", "must be 1-32 lowercase letters, digits or hyphens")
	case !proto.UsageRequestID.MatchString(r.RequestID):
		return bad("request_id", "must be 1-255 ASCII letters, digits or _.:/+=@-, starting with a letter or digit")
	case !proto.UsageModel.MatchString(r.Model):
		return bad("model", "must be 1-128 ASCII letters, digits or _.:/+@-, starting with a letter or digit")
	case r.At.Before(usageEpoch):
		return bad("at", "must be the request's time, after 2020")
	case r.At.After(now.Add(ahead)):
		return bad("at", fmt.Sprintf("%s is more than %s past the server's clock, %s: check the clock of the machine that recorded it",
			r.At.UTC().Format(time.RFC3339), ahead, now.Format(time.RFC3339)))
	}
	switch r.Granularity {
	case GranularityRequest:
		if r.SpanStart != nil {
			return bad("span_start", "is for turn and session records; a request has none")
		}
	case GranularityTurn, GranularitySession:
		if s := r.SpanStart; s != nil && (s.After(r.At) || s.Before(usageEpoch)) {
			return bad("span_start", "must be after 2020 and no later than at")
		}
		if s := r.SpanStart; s != nil && r.At.Sub(*s) > MaxUsageSpan {
			return bad("span_start", "must be within 7 days of at; report a longer session in parts")
		}
	default:
		return bad("granularity", "must be request, turn or session")
	}
	for _, c := range []struct {
		name string
		v    *int64
	}{
		{"input", r.Input}, {"output", r.Output}, {"cache_write", r.CacheWrite},
		{"cache_write_1h", r.CacheWrite1h}, {"cache_read", r.CacheRead},
	} {
		if c.v != nil && (*c.v < 0 || *c.v > MaxUsageCount) {
			return bad(c.name, fmt.Sprintf("must be from 0 to %d, or left out when unknown", int64(MaxUsageCount)))
		}
	}
	if h := r.CacheWrite1h; h != nil && (r.CacheWrite == nil || *h > *r.CacheWrite) {
		return bad("cache_write_1h", "is part of cache_write, so needs it and cannot exceed it")
	}
	return nil
}

// usageCols are the token_usage columns AddUsage writes, in order.
const usageCols = `principal, session, request_id, machine, harness, model, at, granularity, span_start,
  input, output, cache_write, cache_write_1h, cache_read, added_at`

// UsageBatchError refuses an AddUsage batch of Got records, more than
// Limits.UsageRecords (Max). It wraps ErrInvalid.
type UsageBatchError struct{ Max, Got int }

func (e *UsageBatchError) Error() string {
	return fmt.Sprintf("%v: at most %d usage records per call, not %d", ErrInvalid, e.Max, e.Got)
}

// Unwrap makes errors.Is(err, ErrInvalid) hold.
func (e *UsageBatchError) Unwrap() error { return ErrInvalid }

// AddUsage stores the actor's usage records and returns how many were
// new and how many it already had: a record whose request id the actor's
// session already reported is ignored, so sending a batch again changes
// nothing. A call that stores rows records one usage.add event. AddUsage
// refuses, storing nothing, a batch of more than Limits.UsageRecords with
// a [*UsageBatchError], one with an invalid record with ErrInvalid, and
// one that would take the principal past Limits.UsagePerDay rows in 24
// hours with ErrBusy.
//
// A record is the session's: sent again by another session of the
// principal, as an outbox delivers it, it would be stored again. An
// idempotency key (idem) prevents that: a repeat of a call that stored
// rows returns its counts and writes nothing, from any session of the
// principal, and the key reused for another batch is refused with an
// [*IdemError].
func (s *Store) AddUsage(ctx context.Context, actor Actor, recs []UsageRecord, idem string) (UsageAdded, error) {
	if len(recs) == 0 {
		return UsageAdded{}, nil
	}
	if limit := s.opts.Limits.UsageRecords; len(recs) > limit {
		return UsageAdded{}, &UsageBatchError{Max: limit, Got: len(recs)}
	}
	if err := validIdem(idem); err != nil {
		return UsageAdded{}, err
	}
	// A client's time is checked against now, so a forward step of the
	// clock is settled first: a record made since is not in the future.
	now, err := s.settledNow(ctx)
	if err != nil {
		return UsageAdded{}, err
	}
	from, to := recs[0].At.UTC(), recs[0].At.UTC()
	for i, r := range recs {
		if err := r.validate(i+1, now, s.opts.ClientAhead); err != nil {
			return UsageAdded{}, err
		}
		from, to = minTime(from, r.At.UTC()), maxTime(to, r.At.UTC())
	}
	var out UsageAdded
	err = s.write(ctx, actor, func(w *wtx) error {
		out = UsageAdded{}
		if done, err := replay(ctx, w, idem, "usage", recs, &out); done || err != nil {
			return err
		}
		var p params
		rows := make([]string, len(recs))
		for i, r := range recs {
			var span any
			if r.SpanStart != nil {
				span = r.SpanStart.UTC().Truncate(time.Microsecond)
			}
			rows[i] = "(" + p.add(w.actor.Principal, w.actor.Session, r.RequestID, w.actor.Machine, r.Harness, r.Model,
				r.At.UTC().Truncate(time.Microsecond), string(r.Granularity), span,
				r.Input, r.Output, r.CacheWrite, r.CacheWrite1h, r.CacheRead, w.now) + ")"
		}
		// RETURNING tells which rows are new: a record the store had is
		// not counted, here or by the observer.
		added, err := w.insertUsage(ctx, `INSERT INTO token_usage (`+usageCols+`) VALUES `+strings.Join(rows, ", ")+
			` ON CONFLICT DO NOTHING RETURNING model, input, output, cache_write, cache_read`, p.args...)
		if err != nil {
			return fmt.Errorf("insert usage: %w", err)
		}
		n := len(added)
		out = UsageAdded{Added: n, Duplicates: len(recs) - n}
		if n == 0 {
			return nil
		}
		w.mutated = true
		if s.opts.Observer != nil {
			if err := w.countUsage(ctx, added); err != nil {
				return err
			}
		}
		if err := w.checkUsageDay(ctx); err != nil {
			return err
		}
		if err := w.event(ctx, OpUsageAdd, UsageTarget, nil, map[string]any{
			"added": out.Added, "duplicates": out.Duplicates, "from": from, "to": to}); err != nil {
			return err
		}
		return w.settle(out)
	})
	if err != nil {
		return UsageAdded{}, err
	}
	return out, nil
}

// insertUsage runs the insert q and returns the rows it stored, with
// their model and counts.
func (w *wtx) insertUsage(ctx context.Context, q string, args ...any) ([]UsageRecord, error) {
	rs, err := w.tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rs.Close() }()
	var out []UsageRecord
	for rs.Next() {
		var r UsageRecord
		var in, o, cw, cr sql.NullInt64
		if err := rs.Scan(&r.Model, &in, &o, &cw, &cr); err != nil {
			return nil, err
		}
		r.Input, r.Output, r.CacheWrite, r.CacheRead = nullInt(in), nullInt(o), nullInt(cw), nullInt(cr)
		out = append(out, r)
	}
	return out, rs.Err()
}

func nullInt(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	return &n.Int64
}

// countUsage notes, for the store's observer, the tokens of the rows
// just stored, summed for each model. A model without a price in the
// store is counted as [UsageOther]: the table is the server's, so the
// models counted are bounded by it and not by what a client reports.
func (w *wtx) countUsage(ctx context.Context, added []UsageRecord) error {
	var p params
	seen := map[string]bool{}
	var holes []string
	for _, r := range added {
		if !seen[r.Model] {
			seen[r.Model] = true
			holes = append(holes, p.add(r.Model))
		}
	}
	rs, err := w.tx.QueryContext(ctx, `SELECT DISTINCT model FROM prices WHERE model IN (`+strings.Join(holes, ", ")+`)`, //nolint:gosec // holes are $n placeholders
		p.args...)
	if err != nil {
		return fmt.Errorf("read priced models: %w", err)
	}
	defer func() { _ = rs.Close() }()
	priced := map[string]bool{}
	for rs.Next() {
		var m string
		if err := rs.Scan(&m); err != nil {
			return fmt.Errorf("read priced models: %w", err)
		}
		priced[m] = true
	}
	if err := rs.Err(); err != nil {
		return fmt.Errorf("read priced models: %w", err)
	}
	byModel := map[string]*UsageTokens{}
	for _, r := range added {
		m := r.Model
		if !priced[m] {
			m = UsageOther
		}
		u := byModel[m]
		if u == nil {
			u = &UsageTokens{Model: m}
			byModel[m] = u
		}
		u.Input += val(r.Input)
		u.Output += val(r.Output)
		u.CacheWrite += val(r.CacheWrite)
		u.CacheRead += val(r.CacheRead)
	}
	for _, m := range slices.Sorted(maps.Keys(byModel)) {
		w.usage = append(w.usage, *byModel[m])
	}
	return nil
}

func val(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// checkUsageDay refuses, with ErrBusy, a write that left w's principal
// holding more than Limits.UsagePerDay rows added in the last 24 hours.
// It runs after the insert, so the count includes the new rows; the
// refusal rolls them back.
func (w *wtx) checkUsageDay(ctx context.Context) error {
	var n int
	if err := w.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM token_usage WHERE principal = $1 AND added_at > $2`,
		w.actor.Principal, w.now.Add(-24*time.Hour)).Scan(&n); err != nil {
		return fmt.Errorf("count usage: %w", err)
	}
	if limit := w.lim.UsagePerDay; n > limit {
		return fmt.Errorf("%w: %s may add %d usage records a day, and this batch would make %d", ErrBusy, w.actor.Principal, limit, n)
	}
	return nil
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
