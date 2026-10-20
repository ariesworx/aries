package dispatch

import (
	"testing"
)

// Before every start (design §6, §12): a stop asked for, a run that failed
// for auth, two CI failures in a row, the day's Opus output past the cap,
// the pause lock, and no free slot each hold starting, in that order.
func TestHold(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	tests := []struct {
		name  string
		state State
		in    Inputs
		rule  string
	}{
		{name: "nothing holds it", in: Inputs{Free: 4}, rule: ""},
		{name: "one slot is enough", in: Inputs{Free: 1, OpusOutput: cfg.OpusCap - 1}, rule: ""},
		{name: "no slot", in: Inputs{Free: 0}, rule: HoldSlots},
		{name: "budget at the cap", in: Inputs{Free: 4, OpusOutput: cfg.OpusCap}, rule: HoldBudget},
		{name: "budget past the cap", in: Inputs{Free: 4, OpusOutput: cfg.OpusCap + 1}, rule: HoldBudget},
		{name: "paused", in: Inputs{Free: 4, Paused: true}, rule: HoldPause},
		{name: "one CI failure is not a stop", state: State{Failures: 1}, in: Inputs{Free: 4}, rule: ""},
		{name: "two CI failures", state: State{Failures: 2}, in: Inputs{Free: 4}, rule: HoldFailures},
		{name: "auth", state: State{Stopped: HoldAuth}, in: Inputs{Free: 4}, rule: HoldAuth},
		{name: "stop asked", in: Inputs{Free: 4, Stop: StopDrain}, rule: HoldStop},
		{name: "stop now asked", in: Inputs{Free: 4, Stop: StopNow}, rule: HoldStop},
		// Precedence: the most lasting reason is named.
		{name: "stop before budget", in: Inputs{Stop: StopDrain, OpusOutput: cfg.OpusCap}, rule: HoldStop},
		{name: "auth before failures", state: State{Stopped: HoldAuth, Failures: 3}, in: Inputs{Free: 4}, rule: HoldAuth},
		{name: "failures before budget", state: State{Failures: 2}, in: Inputs{Free: 4, OpusOutput: cfg.OpusCap}, rule: HoldFailures},
		{name: "budget before pause", in: Inputs{Free: 4, OpusOutput: cfg.OpusCap, Paused: true}, rule: HoldBudget},
		{name: "pause before slots", in: Inputs{Paused: true}, rule: HoldPause},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rule, why := Hold(cfg, &tc.state, tc.in)
			if rule != tc.rule {
				t.Fatalf("Hold = %q (%s); want %q", rule, why, tc.rule)
			}
			if rule != "" && why == "" {
				t.Errorf("hold %s says no why", rule)
			}
		})
	}
}

// A CI failure on a PR its runs opened counts; a green one resets the
// count; a second failure in a row trips the stop once.
func TestRecordCI(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		results  []bool
		failures int
		tripped  []bool
	}{
		{name: "one failure", results: []bool{false}, failures: 1, tripped: []bool{false}},
		{name: "two in a row", results: []bool{false, false}, failures: 2, tripped: []bool{false, true}},
		{name: "a green between resets", results: []bool{false, true, false}, failures: 1, tripped: []bool{false, false, false}},
		{name: "a third reports no second trip", results: []bool{false, false, false}, failures: 3, tripped: []bool{false, true, false}},
		{name: "greens", results: []bool{true, true}, failures: 0, tripped: []bool{false, false}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var s State
			for i, pass := range tc.results {
				if got := RecordCI(DefaultConfig(), &s, pass); got != tc.tripped[i] {
					t.Errorf("result %d (pass %v): tripped %v; want %v", i, pass, got, tc.tripped[i])
				}
			}
			if s.Failures != tc.failures {
				t.Errorf("failures = %d; want %d", s.Failures, tc.failures)
			}
		})
	}
}

// Clear clears the stops that wait on a person, and lets an issue a run
// failed on be started again; a live run is left as it is.
func TestClear(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.tracker.add("ar-1", "task", "model:opus")
	d, _ := w.dispatcher(DefaultConfig())
	d.State = &State{Failures: 2, Stopped: HoldAuth, Runs: []*Run{
		{Issue: "ar-1", Session: "s1", Started: w.now, Ended: w.now, Outcome: OutcomeFailed, Worktree: "/elsewhere"},
		{Issue: "ar-9", Session: "s9", Started: w.now},
	}}
	if dec := d.judge(t.Context(), "ar-1", w.now); dec.Rule != RuleFailedBefore {
		t.Fatalf("before the clear: %+v", dec)
	}
	d.State.Clear()
	if d.State.Failures != 0 || d.State.Stopped != "" || d.State.Runs[1].Cleared {
		t.Errorf("after the clear: %+v, live run %+v", d.State, d.State.Runs[1])
	}
	if dec := d.judge(t.Context(), "ar-1", w.now); dec.Action != ActStart {
		t.Errorf("after the clear: %+v", dec)
	}
}

// How a run ended decides what it counts toward (design §6): an auth
// failure stops at once; a run that failed for its own reasons, or was
// refused before the harness started, is not a CI failure.
func TestRecordExit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		outcome  string
		stopped  string
		failures int
	}{
		{outcome: OutcomeOK},
		{outcome: OutcomeFailed},
		{outcome: OutcomeRefused},
		{outcome: OutcomeTimeout},
		{outcome: OutcomeStopped},
		{outcome: OutcomeAuth, stopped: HoldAuth},
	}
	for _, tc := range tests {
		t.Run(tc.outcome, func(t *testing.T) {
			t.Parallel()
			s := State{Failures: 1}
			RecordExit(&s, tc.outcome)
			if s.Stopped != tc.stopped || s.Failures != 1 {
				t.Errorf("after %s: stopped %q, failures %d; want %q and 1", tc.outcome, s.Stopped, s.Failures, tc.stopped)
			}
		})
	}
}
