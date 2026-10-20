package dispatch

import (
	"context"
	"strings"
	"testing"
	"time"
)

// loopConfig is a loop that wakes on nothing but what a test does: the
// fallback and the branch poll are far off, and the lock poll is short.
func loopConfig() Config {
	cfg := DefaultConfig()
	cfg.Fallback, cfg.BranchPoll = time.Hour, time.Hour
	cfg.Tick, cfg.Debounce, cfg.LockPoll = 5*time.Millisecond, 0, 20*time.Millisecond
	return cfg
}

// runLoop runs d's loop until the test ends.
func runLoop(t *testing.T, d *Dispatcher) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- d.Loop(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// waitFor fails the test unless ok becomes true within a few seconds.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(2 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("no %s", what)
}

// A pause that is released starts the next run within the lock poll,
// with no event, no run's exit and no timer to wake the loop.
func TestLoopWakesWhenPauseReleased(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.tracker.add("ar-1", "task", "model:sonnet")
	w.tracker.setPaused(true)
	d, _ := w.dispatcher(loopConfig())
	runLoop(t, d)
	waitFor(t, "hold [pause]", func() bool { return strings.Contains(w.out.String(), "hold [pause]") })
	if n := w.calls.count("launch "); n != 0 {
		t.Fatalf("launched %d runs while paused", n)
	}
	w.tracker.setPaused(false)
	waitFor(t, "run after the pause was released", func() bool { return w.calls.count("launch ar-1") == 1 })
	if !strings.Contains(w.out.String(), "wake "+WakeLock) {
		t.Errorf("the wake was not a %s wake:\n%s", WakeLock, w.out.String())
	}
}

// A slot that frees, because another machine's run ended or its lease
// lapsed, starts the next run within the lock poll.
func TestLoopWakesWhenSlotFrees(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.tracker.add("ar-1", "task", "model:sonnet")
	w.slots.held = w.slots.capacity
	d, _ := w.dispatcher(loopConfig())
	runLoop(t, d)
	waitFor(t, "hold [slots]", func() bool { return strings.Contains(w.out.String(), "hold [slots]") })
	w.slots.give()
	waitFor(t, "run after a slot freed", func() bool { return w.calls.count("launch ar-1") == 1 })
}

// With a free slot and nothing held, the loop reads the locks at a wake
// and never on a poll.
func TestLoopDoesNotPollWhenIdle(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	d, _ := w.dispatcher(loopConfig())
	runLoop(t, d)
	waitFor(t, "the first wake", func() bool { return strings.Contains(w.out.String(), "wake start") })
	time.Sleep(40 * time.Millisecond)
	reads := w.tracker.lockReads.Load()
	time.Sleep(200 * time.Millisecond) // ten polls' worth
	if got := w.tracker.lockReads.Load(); got != reads {
		t.Errorf("an idle loop with a free slot read the locks %d more times", got-reads)
	}
}

// While it holds, the loop reads the locks only each poll, not at each
// tick.
func TestLoopPollIsBounded(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.tracker.setPaused(true)
	d, _ := w.dispatcher(loopConfig())
	runLoop(t, d)
	waitFor(t, "hold [pause]", func() bool { return strings.Contains(w.out.String(), "hold [pause]") })
	before := w.tracker.lockReads.Load()
	time.Sleep(200 * time.Millisecond)
	// 200ms / 20ms polls is ten; each poll reads two locks. Ticks (5ms)
	// would be forty reads of each.
	if got := w.tracker.lockReads.Load() - before; got > 30 {
		t.Errorf("the loop read the locks %d times in ten polls", got)
	}
}
