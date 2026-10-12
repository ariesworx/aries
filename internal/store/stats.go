package store

import (
	"cmp"
	"slices"
	"strings"
	"sync"
	"time"
)

// What a store's writer has been doing lately, kept in memory for an
// admin to watch a server of many projects by (design §15.4, `sfx admin
// status`). Nothing here is stored: a store that is closed and opened
// again starts from zero.

// writeSamples is how many of the latest writes' latencies are kept.
const writeSamples = 256

// WriteStats is how busy a store's writer is, as [Store.WriteStats]
// reports it. A write is one transaction that changed a row and
// committed: a request that was refused, and a reap or a sweep that
// found nothing to do, are not counted.
type WriteStats struct {
	// LastMinute is how many writes committed in the last 60 seconds.
	LastMinute int
	// Samples is how many writes P50 and P99 are taken over: the latest,
	// at most 256, however long ago they were.
	Samples int
	// P50 and P99 are the median and the 99th percentile of those
	// writes' latencies, by nearest rank. A write's latency runs from the
	// request reaching the store to its commit, so it includes the wait
	// for the single writer and any retry after a lost race.
	P50, P99 time.Duration
}

// WriteStats returns the store's write counters, read from memory. They
// are on the machine's clock, not Options.Now, since they measure the
// machine.
func (s *Store) WriteStats() WriteStats { return s.stats.snapshot(time.Now()) }

// writeStats holds a store's write counters: a ring of the latest
// latencies, and a count for each of the last 60 seconds.
type writeStats struct {
	mu   sync.Mutex
	took [writeSamples]time.Duration
	n    int // writes recorded in all; took[(n-1)%writeSamples] is the latest
	// secs counts the writes of the second stamp says, by second modulo
	// 60.
	secs  [60]int
	stamp [60]int64
}

// record counts a write that committed at now and took d.
func (ws *writeStats) record(now time.Time, d time.Duration) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.took[ws.n%writeSamples] = d
	ws.n++
	sec := now.Unix()
	i := int(((sec % 60) + 60) % 60)
	if ws.stamp[i] != sec {
		ws.stamp[i], ws.secs[i] = sec, 0
	}
	ws.secs[i]++
}

// snapshot reads the counters as of now.
func (ws *writeStats) snapshot(now time.Time) WriteStats {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	var out WriteStats
	sec := now.Unix()
	for i, n := range ws.secs {
		if ws.stamp[i] > sec-60 && ws.stamp[i] <= sec {
			out.LastMinute += n
		}
	}
	out.Samples = min(ws.n, writeSamples)
	if out.Samples == 0 {
		return out
	}
	sorted := slices.Sorted(slices.Values(ws.took[:out.Samples]))
	rank := func(pct int) time.Duration { return sorted[(pct*len(sorted)+99)/100-1] }
	out.P50, out.P99 = rank(50), rank(99)
	return out
}

// ControlCounts is how many projects and principals a control store
// holds, by state.
type ControlCounts struct {
	Projects   map[ProjectState]int
	Principals map[PrincipalState]int
}

// Counts returns how many projects and principals the store holds, by
// state, from memory as [Control.Access] reads it. A state that nothing
// is in has no entry.
func (c *Control) Counts() ControlCounts {
	c.ix.mu.RLock()
	defer c.ix.mu.RUnlock()
	out := ControlCounts{Projects: map[ProjectState]int{}, Principals: map[PrincipalState]int{}}
	for _, p := range c.ix.projects {
		out.Projects[p.State]++
	}
	if n := len(c.ix.active); n > 0 {
		out.Principals[PrincipalActive] = n
	}
	if n := len(c.ix.disabled); n > 0 {
		out.Principals[PrincipalDisabled] = n
	}
	return out
}

// KnownProjects returns every project, in every state, by name, from
// memory: what [Control.Projects] reads from the database.
func (c *Control) KnownProjects() []Project {
	c.ix.mu.RLock()
	defer c.ix.mu.RUnlock()
	out := make([]Project, 0, len(c.ix.projects))
	for _, p := range c.ix.projects {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b Project) int { return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID)) })
	return out
}
