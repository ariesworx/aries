package server

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
	"github.com/ariesworx/aries/internal/telemetry"
)

// The server's clock (design §14, decision 37). Each store keeps its own
// time from going backward and settles a forward step itself
// (store/clock.go); what is here is what the daemon does about a clock
// it finds wrong: it refuses to start, or starts with a warning, when
// the clock is behind a database's history (Config.ClockBehind), and it
// reports that, and every step it sees while it runs, in its log and in
// `aries admin status`.

// What a daemon does at start when its clock is behind the history of
// one of its databases by more than Config.BehindTolerance.
const (
	// ClockStop refuses to start, with a [*ClockBehindError]. The
	// default: a clock that went back is worth a person's look before
	// the server carries on.
	ClockStop = "stop"
	// ClockWarn starts, with an error in the log that repeats while the
	// clock is behind.
	ClockWarn = "warn"
)

// DefaultBehindTolerance is how far the clock may be behind a database's
// history before Config.ClockBehind applies. With stop as the default a
// false refusal is an outage, so it covers the worst a healthy machine
// does: one that boots without a working real-time clock starts from
// the time its time service last saved, which systemd-timesyncd does
// every 60 seconds. It is far below any restore, rollback or reset,
// which are what the setting is for.
const DefaultBehindTolerance = 2 * time.Minute

// Bounds of the clock tolerances a settings file may set.
const (
	minClockTolerance  = time.Second
	maxStepTolerance   = 10 * time.Minute
	maxBehindTolerance = 24 * time.Hour
)

// clockWarnEvery is how often the log repeats that the clock is behind
// history, while it is.
const clockWarnEvery = 10 * time.Minute

// ClockBehindError refuses to start a daemon whose clock is behind the
// history of a database it serves.
type ClockBehindError struct {
	// Log names the database whose history is furthest ahead, and More
	// counts the other databases ahead of the clock.
	Log  string
	More int
	// Clock is the server's clock and History the time of that
	// database's latest event.
	Clock, History time.Time
}

func (e *ClockBehindError) Error() string {
	also := ""
	if e.More > 0 {
		also = fmt.Sprintf(" (and of %d more)", e.More)
	}
	accept := e.History.UTC().Truncate(time.Second).Add(2 * time.Second).Format(time.RFC3339)
	return fmt.Sprintf("the server's clock is behind its own history: the clock reads %s, and the latest event of %s%s is at %s, %s later; "+
		"fix: check the clock (`timedatectl` must say \"System clock synchronized: yes\"; `timedatectl timesync-status` names the source), correct it, and start ariesd again. "+
		"If the clock is right and the history is ahead of it, as after restoring a database written by a machine whose clock ran ahead, start once with `ariesd serve --accept-clock-behind %s`: "+
		"ariesd's time then runs on from the history at half speed until the clock has caught up. "+
		"To start with a warning whenever the clock is behind, set clock_behind: warn",
		e.Clock.UTC().Format(time.RFC3339), e.Log, also, e.History.UTC().Format(time.RFC3339),
		e.History.Sub(e.Clock).Round(time.Second), accept)
}

// Lines is the refusal as serve prints it: what happened, the two
// times, and each way on, a short line each, so that the ten lines
// `systemctl status` shows of a unit's journal hold all of it at a
// terminal's width.
func (e *ClockBehindError) Lines() []string {
	also := ""
	switch {
	case e.More == 1:
		also = ", and 1 more database is ahead of the clock too"
	case e.More > 1:
		also = fmt.Sprintf(", and %d more databases are ahead of the clock too", e.More)
	}
	accept := e.History.UTC().Truncate(time.Second).Add(2 * time.Second).Format(time.RFC3339)
	return []string{
		"not starting: the server's clock is behind its own history (clock_behind: stop)",
		fmt.Sprintf("  the clock reads   %s", e.Clock.UTC().Format(time.RFC3339)),
		fmt.Sprintf("  the latest event  %s, %s later", e.History.UTC().Format(time.RFC3339), e.History.Sub(e.Clock).Round(time.Second)),
		fmt.Sprintf("  is in %s%s", e.Log, also),
		"fix: if the clock is wrong, correct it (`timedatectl` must say synchronized: yes), then `systemctl start ariesd`",
		fmt.Sprintf("fix: if the clock is right and the history is ahead, start once with: ariesd serve --accept-clock-behind %s", accept),
		"fix: or set clock_behind: warn in the config file, to start with a warning (docs/server.md, The server's clock)",
	}
}

// clockWatch is what the daemon has seen of its clock.
type clockWatch struct {
	mu sync.Mutex
	// steps is how many steps had been logged, and forward and backward
	// how many of each had been counted (telemetry.ClockSteps).
	steps             int
	forward, backward int
	// behind is the database furthest ahead of the clock at the last
	// look, if any was past the tolerance; warned is when the log last
	// said so, by Server.now.
	behind *behindLog
	warned time.Time
}

// behindLog is one database's history ahead of the clock.
type behindLog struct {
	log     string
	by      time.Duration
	since   time.Time
	history time.Time
	clock   time.Time
	// mark is the database's latest event when its store opened.
	mark time.Time
}

// behindOf returns st as a behindLog when the clock is behind the
// store's time at all, and nil otherwise. A server's stores share one
// time, so every open store is behind together; mark tells which
// database's history put them there.
func (s *Server) behindOf(log string, st store.ClockState) *behindLog {
	if st.Behind <= 0 {
		return nil
	}
	return &behindLog{log: log, by: st.Behind, since: st.Since, history: st.Now, clock: st.Clock, mark: st.Mark}
}

// startBehind is behindOf for a daemon about to start, which has given
// out no time yet: it compares the clock with the database's own latest
// event.
func (s *Server) startBehind(log string, st store.ClockState) *behindLog {
	if e := BehindHistory(s.cfg.BehindTolerance, log, st); e != nil {
		return &behindLog{log: log, by: e.History.Sub(e.Clock), since: e.Clock, history: e.History, clock: e.Clock, mark: e.History}
	}
	return nil
}

// accepted reports whether the operator accepted, for this start, a
// history as far ahead as b's (Config.AcceptHistory).
func (s *Server) accepted(b *behindLog) bool {
	return !s.cfg.AcceptHistory.IsZero() && !b.history.After(s.cfg.AcceptHistory)
}

// startClock decides whether a daemon may start with the databases in
// logs, which are behind's findings: nil when none is ahead of the clock
// past the tolerance. With Config.ClockBehind stop it returns a
// [*ClockBehindError] naming the one furthest ahead, unless the operator
// accepted that for this start. Otherwise it logs the warning and
// remembers it for the status.
func (s *Server) startClock(logs []*behindLog) error {
	var worst *behindLog
	n := 0
	for _, b := range logs {
		if b == nil {
			continue
		}
		n++
		if worst == nil || b.history.After(worst.history) {
			worst = b
		}
	}
	if worst == nil {
		return nil
	}
	if s.cfg.ClockBehind == ClockStop && !s.accepted(worst) {
		return &ClockBehindError{Log: worst.log, More: n - 1, Clock: worst.clock, History: worst.history}
	}
	s.reportBehind(context.Background(), worst)
	return nil
}

// reportBehind records that the clock is behind b's history, or behind
// none when b is nil, and logs it: at error when it is first seen, and
// again every clockWarnEvery; at info when it clears.
func (s *Server) reportBehind(ctx context.Context, b *behindLog) {
	log := s.cfg.Logger
	w := &s.clock
	w.mu.Lock()
	defer w.mu.Unlock()
	was := w.behind
	w.behind = b
	switch {
	case b == nil && was != nil:
		log.InfoContext(ctx, "clock caught up with history", "event", logs.ClockCaughtUp, "log", was.log, "was_behind_since", was.since)
	case b != nil && (was == nil || s.now().Sub(w.warned) >= clockWarnEvery):
		w.warned = s.now()
		restart := "it will start again as it is (clock_behind: warn)"
		if s.cfg.ClockBehind == ClockStop {
			restart = "THIS SERVER WILL NOT START AGAIN until the clock is corrected (clock_behind: stop)"
		}
		log.ErrorContext(ctx, "clock behind history", "event", logs.ClockBehind, "log", b.log, "clock", b.clock, "history", b.history,
			"behind", b.by.Round(time.Second).String(), "since", b.since,
			"effect", "ariesd's time runs on from the history at half speed until the clock has caught up; "+restart,
			"fix", "check the clock with `timedatectl` and `timedatectl timesync-status`, and correct it; if the history is what is wrong, do nothing: the clock catches up")
	}
}

// CheckClock looks at the clock as the reaper does on each tick: it
// logs each step seen since the last look, settles a forward step in the
// control store, and reports whether the clock is behind the history of
// the control store or of any open project's store. It is a clock.check
// span, and its lines carry the span's ids.
func (s *Server) CheckClock(ctx context.Context) {
	ctx, span := s.cfg.Telemetry.Start(ctx, telemetry.SpanClockCheck)
	defer span.End()
	log := s.cfg.Logger
	steps := s.cfg.Clock.Steps()
	w := &s.clock
	w.mu.Lock()
	seen := steps.Forward + steps.Backward
	fresh := seen - w.steps
	fwd, back := steps.Forward-w.forward, steps.Backward-w.backward
	w.steps, w.forward, w.backward = seen, steps.Forward, steps.Backward
	w.mu.Unlock()
	if fwd > 0 {
		s.cfg.Telemetry.Record(telemetry.ClockSteps, float64(fwd), telemetry.StepForward)
	}
	if back > 0 {
		s.cfg.Telemetry.Record(telemetry.ClockSteps, float64(back), telemetry.StepBackward)
	}
	if fresh > 0 {
		dir, effect := "forward", "every live lease is extended by the step, so no claim ends for it (a clock.step event in each log)"
		if steps.LastBy < 0 {
			dir, effect = "backward", "ariesd's time does not go back: it runs on at half speed until the clock has caught up"
		}
		log.WarnContext(ctx, "clock stepped", "event", logs.ClockStepped, "direction", dir, "step", steps.LastBy.Round(time.Millisecond).String(), "clock", steps.LastAt,
			"steps", fresh, "forward_since_start", steps.Forward, "backward_since_start", steps.Backward, "effect", effect,
			"fix", "find what set the clock: a suspend or a restored snapshot of the machine, an operator, or the time service (`timedatectl timesync-status`, `journalctl -u systemd-timesyncd`)")
	}
	var behinds []*behindLog
	if ctl := s.cfg.Control; ctl != nil {
		sctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
		if err := ctl.SettleClock(sctx); err != nil && ctx.Err() == nil {
			log.ErrorContext(ctx, "settle a clock step in the control store", "event", logs.ClockSettleFailed, "err", err)
		}
		cancel()
		behinds = append(behinds, s.behindOf(ControlLog, ctl.ClockState()))
	}
	s.each(func(pr *served) { behinds = append(behinds, s.behindOf(pr.logName(), pr.store.ClockState())) })
	var worst *behindLog
	for _, b := range behinds {
		if b != nil && (worst == nil || b.mark.After(worst.mark)) {
			worst = b
		}
	}
	// A monitor sees the clock behind its history here, to the second,
	// and alerts on it; /readyz does not answer for it, since the daemon
	// serves correctly meanwhile and a daemon reported unready may be
	// taken out of service, which would hand whoever set the clock an
	// outage.
	var by float64
	if worst != nil {
		by = worst.by.Seconds()
	}
	s.cfg.Telemetry.Record(telemetry.ClockBehind, by)
	// The log and the status start to say so past the tolerance, and
	// then go on until the clock has caught up altogether: a report
	// that ended at the tolerance would call the clock caught up while
	// it was still that far behind.
	w.mu.Lock()
	reported := w.behind != nil
	w.mu.Unlock()
	if worst != nil && worst.by <= s.cfg.BehindTolerance && !reported {
		worst = nil
	}
	s.reportBehind(ctx, worst)
}

// ControlLog names the control database's history in a report.
const ControlLog = "the control database"

// ProjectLog names a project's history in a report: its id, and its
// database's name where that is known.
func ProjectLog(id, db string) string {
	if db == "" {
		return "project " + id
	}
	return fmt.Sprintf("project %s (database %s)", id, db)
}

// logName names the project's history in a report.
func (pr *served) logName() string { return ProjectLog(pr.id, pr.db) }

// BehindHistory returns, as the refusal serve would give, that the
// clock is behind the latest event of the database st was read from,
// named log, by more than tolerance; and nil when it is not, or the
// database holds no event. `ariesd upgrade` asks it before it swaps
// anything.
func BehindHistory(tolerance time.Duration, log string, st store.ClockState) *ClockBehindError {
	if st.Mark.IsZero() || st.Mark.Sub(st.Clock) <= tolerance {
		return nil
	}
	return &ClockBehindError{Log: log, Clock: st.Clock, History: st.Mark}
}

// clockStatus is the clock as `aries admin status` shows it.
func (s *Server) clockStatus() *proto.ClockStatus {
	steps := s.cfg.Clock.Steps()
	out := &proto.ClockStatus{Now: s.cfg.Clock.Now(), StepsForward: steps.Forward, StepsBackward: steps.Backward}
	if !steps.LastAt.IsZero() {
		at := steps.LastAt
		out.LastStepAt, out.LastStepSeconds = &at, steps.LastBy.Seconds()
	}
	w := &s.clock
	w.mu.Lock()
	defer w.mu.Unlock()
	if b := w.behind; b != nil {
		out.Behind = &proto.ClockBehind{Seconds: int64(b.by / time.Second), Since: b.since, Log: b.log, History: b.history,
			RefusesStart: s.cfg.ClockBehind == ClockStop}
	}
	return out
}
