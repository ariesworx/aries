package outbox

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"time"
)

// WaitBounds are the upper bounds of the first buckets of
// [WaitStats.Buckets], each inclusive; the last bucket counts every
// longer wait. Do not change its elements.
var WaitBounds = [...]time.Duration{
	time.Second, 10 * time.Second, time.Minute, 10 * time.Minute,
	time.Hour, 6 * time.Hour, 24 * time.Hour,
}

// WaitStats summarizes how long delivered entries waited, from queued to
// answered: not every wait, but their count, sum, longest and a fixed
// histogram.
type WaitStats struct {
	Count   uint64                      `json:"count"`
	Total   time.Duration               `json:"total"`
	Max     time.Duration               `json:"max"`
	Buckets [len(WaitBounds) + 1]uint64 `json:"buckets"`
}

// OpStats counts one op's entries: how many were queued, and how many
// left the outbox in each way.
type OpStats struct {
	Queued    uint64 `json:"queued"`
	Delivered uint64 `json:"delivered"`
	// Refused are the server's refusals; Stale are the entries that
	// waited too long to be sent.
	Refused uint64 `json:"refused"`
	Stale   uint64 `json:"stale"`
	// Dropped are the entries a person dropped.
	Dropped uint64 `json:"dropped"`
	// Wait is of the delivered entries.
	Wait WaitStats `json:"wait"`
}

// OutageStats counts the times the server could not be reached, as the
// outbox sees them: an outage opens at the first Append with none in
// progress, since an entry is only queued when the server was not
// reached, and closes at the next answer the server gives a replayed
// entry, a refusal included.
type OutageStats struct {
	Count   uint64        `json:"count"`
	Total   time.Duration `json:"total"`
	Longest time.Duration `json:"longest"`
	// Since is when the outage in progress began; zero when there is
	// none.
	Since time.Time `json:"since,omitzero"`
}

// Stats are an outbox's counts since its directory was made, or since
// the file that holds them was last lost. They hold op names, counts and
// durations, and no text of any entry (rule 2). Their size is fixed: one
// OpStats for each op the outbox queues, at most.
type Stats struct {
	Ops     map[string]OpStats `json:"ops"`
	Outages OutageStats        `json:"outages"`
}

// Stats returns the counts. They are kept beside the outbox, written
// with it and never synced, so a crash may lose the latest; a file that
// does not parse counts as none.
func (o *Outbox) Stats() (Stats, error) {
	var s Stats
	err := o.locked(func() error {
		s = o.readStats()
		return nil
	})
	if err != nil {
		return Stats{}, fmt.Errorf("read outbox counts: %w", err)
	}
	return s, nil
}

// readStats reads the stats file, as empty when it is missing or does
// not parse, and without any op the outbox does not queue, which is what
// keeps the file's size fixed. The caller holds the append lock.
func (o *Outbox) readStats() Stats {
	var s Stats
	b, err := os.ReadFile(o.path(statsFile)) //nolint:gosec // the outbox's own file
	if err != nil || json.Unmarshal(b, &s) != nil {
		return Stats{}
	}
	for op := range s.Ops {
		if !queued[op] {
			delete(s.Ops, op)
		}
	}
	return s
}

// count applies change to the stats file. The caller holds the append
// lock, which is the one lock every writer of the file holds: an append
// has it already, and a replay takes it once, when it ends. A failure is
// dropped on purpose: the counts are a measurement, and must not fail an
// append that is already on disk or a replay that already delivered.
func (o *Outbox) count(change func(*Stats)) {
	s := o.readStats()
	change(&s)
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	_ = replace(o.path(statsFile), b, false)
}

// op changes the counts of the op name.
func (s *Stats) op(name string, change func(*OpStats)) {
	if s.Ops == nil {
		s.Ops = make(map[string]OpStats)
	}
	c := s.Ops[name]
	change(&c)
	s.Ops[name] = c
}

// queued counts an entry appended at the time at, which opens an outage
// when none is in progress.
func (s *Stats) queued(op string, at time.Time) {
	s.op(op, func(c *OpStats) { c.Queued++ })
	if s.Outages.Since.IsZero() {
		s.Outages.Since = at
	}
}

// delivered counts an entry the server applied at the time at, after
// wait.
func (s *Stats) delivered(op string, at time.Time, wait time.Duration) {
	wait = max(wait, 0) // a clock that went back
	s.op(op, func(c *OpStats) {
		c.Delivered++
		w := &c.Wait
		w.Count++
		w.Total = add(w.Total, wait)
		w.Max = max(w.Max, wait)
		bucket := len(WaitBounds)
		for i, bound := range WaitBounds {
			if wait <= bound {
				bucket = i
				break
			}
		}
		w.Buckets[bucket]++
	})
	s.reached(at)
}

// refused counts an entry the server refused at the time at.
func (s *Stats) refused(op string, at time.Time) {
	s.op(op, func(c *OpStats) { c.Refused++ })
	s.reached(at)
}

func (s *Stats) stale(op string) { s.op(op, func(c *OpStats) { c.Stale++ }) }

func (s *Stats) dropped(op string) { s.op(op, func(c *OpStats) { c.Dropped++ }) }

// reached closes the outage in progress, if there is one: the server
// answered at the time at.
func (s *Stats) reached(at time.Time) {
	out := &s.Outages
	if out.Since.IsZero() {
		return
	}
	d := max(at.Sub(out.Since), 0)
	out.Count++
	out.Total = add(out.Total, d)
	out.Longest = max(out.Longest, d)
	out.Since = time.Time{}
}

// add is a + b, and the longest duration there is when that is past it.
func add(a, b time.Duration) time.Duration {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}
