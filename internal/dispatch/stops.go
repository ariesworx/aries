package dispatch

import "fmt"

// What holds every start (dispatcher.md §6, §12).
const (
	HoldStop        = "stop"
	HoldAuth        = "auth"
	HoldFailures    = "ci-failures"
	HoldBudget      = "budget"
	HoldPause       = "pause"
	HoldSlots       = "slots"
	HoldUnreachable = "unreachable"
)

// lasting are the holds that stop the dispatcher until a person acts or
// the day turns, logged once as a stop; the rest are logged at each wake.
var lasting = map[string]bool{HoldStop: true, HoldAuth: true, HoldFailures: true, HoldBudget: true}

// Inputs are what Hold reads besides the state: the stop asked for, the
// pause lock, the day's Opus output so far, and the free slots.
type Inputs struct {
	Stop       string
	Paused     bool
	OpusOutput int64
	Free       int
}

// Hold returns what holds starting, the most lasting first, and says why;
// "" when a run may start.
func Hold(cfg Config, s *State, in Inputs) (rule, why string) {
	switch {
	case in.Stop != "":
		return HoldStop, "aries dispatch stop asked it to start nothing more"
	case s.Stopped == HoldAuth:
		return HoldAuth, "a run failed to authenticate, and every later one would; check the API key, then run again with --clear"
	case s.Failures >= cfg.MaxFailures:
		return HoldFailures, fmt.Sprintf("%d CI failures in a row on PRs its runs opened; look at them, then run again with --clear", s.Failures)
	case in.OpusOutput >= cfg.OpusCap:
		return HoldBudget, fmt.Sprintf("today's Opus output is %d tokens, at or past the cap of %d (UTC day)", in.OpusOutput, cfg.OpusCap)
	case in.Paused:
		return HoldPause, "the lock " + PauseLock + " is held"
	case in.Free <= 0:
		return HoldSlots, "no slot of the lock " + RunsLock + " is free"
	}
	return "", ""
}

// RecordCI counts a CI result of a PR its runs opened: a failure adds one,
// a pass resets the count. It reports whether this result tripped the
// stop.
func RecordCI(cfg Config, s *State, pass bool) (tripped bool) {
	if pass {
		s.Failures = 0
		return false
	}
	s.Failures++
	return s.Failures == cfg.MaxFailures
}

// RecordExit counts how a run ended: an auth failure stops the
// dispatcher at once; nothing else that ends a run is a CI failure.
func RecordExit(s *State, outcome string) {
	if outcome == OutcomeAuth {
		s.Stopped = HoldAuth
	}
}
