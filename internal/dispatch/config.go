package dispatch

import "time"

// Config is how the loop paces and bounds itself. DefaultConfig holds
// the values the design and the memory `dispatch` give.
type Config struct {
	// Capacity is the lock dispatch/runs's capacity: how many runs, of
	// every dispatcher together, at once (rule 1).
	Capacity int
	// Lease is the lock's hold and the claim's lease when the dispatcher
	// takes them; the run renews both while it lives.
	Lease time.Duration
	// Fallback is the timer that wakes the loop when nothing else has.
	Fallback time.Duration
	// BranchPoll is how often the default branch's ref is read.
	BranchPoll time.Duration
	// Tick is how often the loop looks at its runs and the stop file,
	// both on this machine.
	Tick time.Duration
	// Debounce is the least time between two wakes that inbox items and
	// events cause.
	Debounce time.Duration
	// RunLimit is a run's wall-clock limit; StopGrace how long an
	// interrupted run has before it is terminated.
	RunLimit  time.Duration
	StopGrace time.Duration
	// OpusCap is a UTC day's Opus output tokens past which nothing more
	// starts that day (rule 3).
	OpusCap int64
	// MaxFailures is how many CI failures in a row stop it.
	MaxFailures int
	// ReadyLimit is how many ready issues each wake reads.
	ReadyLimit int
	// DryRun says what it would do and does none of it.
	DryRun bool
}

// DefaultConfig returns the design's values (dispatcher.md §5, §6, §14
// Q11).
func DefaultConfig() Config {
	return Config{
		Capacity:    4,
		Lease:       10 * time.Minute,
		Fallback:    20 * time.Minute,
		BranchPoll:  5 * time.Minute,
		Tick:        2 * time.Second,
		Debounce:    10 * time.Second,
		RunLimit:    90 * time.Minute,
		StopGrace:   30 * time.Second,
		OpusCap:     1_300_000,
		MaxFailures: 2,
		ReadyLimit:  20,
	}
}

// The locks the dispatcher uses (dispatcher.md §5, §12).
const (
	// RunsLock bounds the runs of every dispatcher together.
	RunsLock = "dispatch/runs"
	// PauseLock, while anyone holds it, starts nothing anywhere.
	PauseLock = "dispatch/pause"
)

// Wake sources: what woke the loop (dispatcher.md §3).
const (
	WakeStart  = "start"
	WakeInbox  = "inbox"
	WakeEvent  = "event"
	WakeResync = "resync"
	WakeExit   = "exit"
	WakeBranch = "branch"
	WakeTimer  = "timer"
)

// How a run ended.
const (
	// OutcomeOK: the harness exited 0 with no error.
	OutcomeOK = "ok"
	// OutcomeFailed: it ended otherwise, for its own reasons.
	OutcomeFailed = "failed"
	// OutcomeAuth: it failed to authenticate, which stops the dispatcher.
	OutcomeAuth = "auth"
	// OutcomeRefused: the harness never started: the lock was full, or
	// aries failed before it.
	OutcomeRefused = "refused"
	// OutcomeTimeout: past its wall-clock limit, it was interrupted.
	OutcomeTimeout = "timeout"
	// OutcomeStopped: `aries dispatch stop --now` interrupted it.
	OutcomeStopped = "stopped"
	// OutcomeLost: found dead on restart, how it ended unknown.
	OutcomeLost = "lost"
	// OutcomeUnknown: an adopted run ended, how unknown.
	OutcomeUnknown = "unknown"
)

// Stop requests, as `aries dispatch stop` writes them.
const (
	// StopDrain starts nothing more and lets runs end.
	StopDrain = "drain"
	// StopNow also interrupts the runs.
	StopNow = "now"
)

// Harness and effort, as the brief: comment names them. Only Claude Code
// is driven (dispatcher.md §10).
const (
	HarnessClaudeCode = "claude-code"
	EffortDefault     = "default"
)

// KeyName is the name the API key is kept under in the credential store
// (internal/credential).
const KeyName = "anthropic-api-key"
