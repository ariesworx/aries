package server

import (
	"context"
	"sync"
	"time"

	"github.com/ariesworx/starfix/internal/doltmetrics"
	"github.com/ariesworx/starfix/internal/proto"
)

// Dolt's own figures in the status report (design §16.5, decision 30).
// With Config.DoltMetrics set, the daemon reads Dolt's metrics endpoint
// on a timer of its own, never for a request, and keeps the last
// answer for `sfx admin status` and `starfixd status`. Dolt's metrics
// are not exported again: a collector scrapes Dolt itself.
//
// A read that fails makes the report say "unknown" with the last read
// that succeeded. It is never an error of the daemon's and never makes
// it unready: /readyz asks Dolt a query (Server.Ready), and whether its
// metrics endpoint answers says nothing of whether it serves SQL.
//
// One figure exists nowhere else: the cycles of Dolt's garbage
// collector for each 100 writes the daemon made, over the latest
// doltWindow. Under a Go memory limit it is what told a limit with room
// from one too low, where resident memory against the limit did not
// (spike/multidb/RESULTS.md, section 5).

const (
	// DefaultDoltMetricsEvery is how often Dolt's metrics are read.
	DefaultDoltMetricsEvery = 15 * time.Second
	// doltWindow is how far back the collector's cycles are set against
	// the daemon's writes.
	doltWindow = 10 * time.Minute
	// doltPoints is the most readings kept for the window: more than
	// fill it at DefaultDoltMetricsEvery.
	doltPoints = 64
	// doltWarnEvery is how often the log repeats that Dolt's metrics
	// cannot be read, while that lasts.
	doltWarnEvery = 10 * time.Minute
)

// doltWatch is what the daemon last read of Dolt's metrics.
type doltWatch struct {
	// reading is held for a whole read, so that one runs at a time;
	// seen and writes are guarded by it.
	reading sync.Mutex
	// seen is how many writes each open store had made at the last
	// read, and writes the sum of what each had added by then: the
	// daemon's writes since it started, as far as the reads saw them.
	seen   map[*served]int
	writes int64
	// now is the clock the reads are timed by; nil is time.Now. It is
	// the machine's, not Server.now, which tests move by hours: Dolt's
	// start time is on the machine's clock too.
	now func() time.Time
	// timeout bounds one read; zero is doltmetrics.Timeout.
	timeout time.Duration

	mu sync.Mutex // guards what follows
	// tried is set once a read has ended, and failed while the latest
	// one failed.
	tried, failed bool
	// readAt is when the last read that succeeded did, and sample what
	// it gave.
	readAt time.Time
	sample doltmetrics.Sample
	// points are the readings of the collector's cycles within
	// doltWindow and one before it, oldest first, all of one Dolt
	// process.
	points []doltPoint
	// failures counts the reads that failed since the last that
	// succeeded, and warned is when the log last said so.
	failures int
	warned   time.Time
}

// doltPoint is one reading of Dolt's collector beside the daemon's
// writes.
type doltPoint struct {
	at      time.Time
	started time.Time // Dolt's start time, or zero when it gave none
	cycles  int64
	writes  int64
}

// clock is the watch's clock.
func (w *doltWatch) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

// doltLoop reads Dolt's metrics now and then every
// Config.DoltMetricsEvery, until ctx is done.
func (s *Server) doltLoop(ctx context.Context) {
	s.ReadDolt(ctx)
	t := time.NewTicker(s.cfg.DoltMetricsEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.ReadDolt(ctx)
		}
	}
}

// ReadDolt reads Dolt's metrics endpoint once, as Serve does on its
// timer, and keeps the answer for the status report. It returns nothing:
// a failure is kept too, and logged when it starts, every doltWarnEvery
// while it lasts, and when it ends. Without Config.DoltMetrics it does
// nothing.
func (s *Server) ReadDolt(ctx context.Context) {
	r := s.cfg.DoltMetrics
	if r == nil {
		return
	}
	w := &s.doltm
	w.reading.Lock()
	defer w.reading.Unlock()
	timeout := w.timeout
	if timeout <= 0 {
		timeout = doltmetrics.Timeout
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	sample, err := r.Read(rctx)
	cancel()
	if ctx.Err() != nil {
		return // the daemon is stopping, which says nothing of Dolt
	}
	writes := s.countWrites()
	now := w.clock()

	w.mu.Lock()
	defer w.mu.Unlock()
	w.tried = true
	if err != nil {
		w.failed = true
		w.failures++
		if w.failures == 1 || now.Sub(w.warned) >= doltWarnEvery {
			w.warned = now
			s.cfg.Logger.Warn("could not read Dolt's metrics; the status report shows Dolt as unknown until a read succeeds",
				"url", r.URL(), "failures", w.failures, "err", err)
		}
		return
	}
	if w.failures > 0 {
		s.cfg.Logger.Info("reading Dolt's metrics again", "url", r.URL(), "after_failures", w.failures)
	}
	w.failed, w.failures = false, 0
	w.readAt, w.sample = now, sample
	if sample.GCCycles == nil {
		return
	}
	p := doltPoint{at: now, cycles: *sample.GCCycles, writes: writes}
	if sample.Started != nil {
		p.started = *sample.Started
	}
	// A counter that went back, or another start time, is another Dolt
	// process: its cycles are not the last one's.
	if n := len(w.points); n > 0 && (p.cycles < w.points[n-1].cycles || !p.started.Equal(w.points[n-1].started)) {
		w.points = w.points[:0]
	}
	w.points = append(w.points, p)
	// The first reading kept is the latest that is doltWindow old.
	for len(w.points) > doltPoints || (len(w.points) > 1 && !w.points[1].at.After(now.Add(-doltWindow))) {
		w.points = append(w.points[:0], w.points[1:]...)
	}
}

// countWrites returns how many writes the daemon's project stores have
// committed since it started, as far as its reads of Dolt saw them: at
// each read, what every open store added since the last. A store that
// closed between two reads takes with it what it wrote since the
// earlier one, which is little: a store closes when it has been idle,
// or its project was archived. s.doltm.reading is held.
func (s *Server) countWrites() int64 {
	w := &s.doltm
	seen := make(map[*served]int, len(w.seen))
	s.each(func(pr *served) {
		n := pr.store.Writes()
		w.writes += int64(n - w.seen[pr])
		seen[pr] = n
	})
	w.seen = seen
	return w.writes
}

// doltStatus is the status report's part about Dolt: nil when the
// daemon reads nothing of it.
func (s *Server) doltStatus() *proto.DoltStatus {
	if s.cfg.DoltMetrics == nil {
		return nil
	}
	w := &s.doltm
	w.mu.Lock()
	defer w.mu.Unlock()
	out := &proto.DoltStatus{}
	if !w.readAt.IsZero() {
		out.ReadAt = new(w.readAt.UTC())
	}
	if w.failed || !w.tried {
		out.Unknown = true
		return out
	}
	m := w.sample
	out.Version, out.ResidentBytes, out.Conns, out.MemLimitBytes = m.Version, m.Resident, m.Conns, m.MemLimit
	if m.Started != nil {
		out.StartedAt = m.Started
		out.UptimeSeconds = new(max(int64(w.readAt.Sub(*m.Started)/time.Second), 0))
	}
	// The points are only as new as the last read that gave the cycles.
	if n := len(w.points); n > 1 && m.GCCycles != nil {
		first, last := w.points[0], w.points[n-1]
		g := &proto.DoltGC{WindowSeconds: int64(last.at.Sub(first.at).Round(time.Second) / time.Second),
			Cycles: last.cycles - first.cycles, Writes: last.writes - first.writes}
		if g.Writes > 0 {
			g.Per100Writes = new(100 * float64(g.Cycles) / float64(g.Writes))
		}
		out.GC = g
	}
	return out
}
