package cli

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

func TestPrintLocks(t *testing.T) {
	t.Parallel()
	until := time.Date(2026, 10, 15, 9, 30, 0, 0, time.UTC)
	tests := []struct {
		name  string
		locks []proto.Lock
		want  string
	}{
		{"none", nil, "no locks held\n"},
		{"held and waited for", []proto.Lock{
			{Name: "deploy/prod", Capacity: 1, Holders: []proto.LockHolder{{By: "alice", Session: "cli.lock-1a2b", Epoch: 7, ExpiresAt: until}},
				Waiters: []proto.LockHolder{{By: "bob", Session: "cli", Position: 1}, {By: "carol", Session: "s-c", Position: 2}}},
			{Name: "staging", Capacity: 3, Holders: []proto.LockHolder{{By: "erin", Session: "cli", Epoch: 2, ExpiresAt: until}}},
		}, "deploy/prod  1/1  alice/cli.lock-1a2b epoch 7 until 2026-10-15 09:30 UTC  waiting: bob/cli, carol/s-c\n" +
			"staging      1/3  erin/cli epoch 2 until 2026-10-15 09:30 UTC\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			printLocks(&b, tc.locks)
			if b.String() != tc.want {
				t.Errorf("printLocks:\n%q\nwant:\n%q", b.String(), tc.want)
			}
		})
	}
}

// stop waits the grace it is given before it kills the command, and a
// command that has stopped by then is not killed.
func TestStopGrace(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		script string
		killed bool
	}{
		{"ignores the interrupt", "trap '' INT; echo ready; while :; do sleep 1; done", true},
		{"stops at the interrupt", "trap 'exit 0' INT; echo ready; while :; do sleep 1; done", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			const grace = 40 * time.Second
			var asked time.Duration
			r := &runner{env: Env{After: func(d time.Duration) <-chan time.Time {
				asked = d
				c := make(chan time.Time, 1)
				if tc.killed {
					c <- time.Time{} // the grace runs out at once
				}
				return c
			}}}
			cmd := exec.Command("sh", "-c", tc.script) //nolint:gosec // a script of the test's own
			pipe, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Skipf("no POSIX shell here: %v", err)
			}
			if _, err := io.ReadFull(pipe, make([]byte, len("ready\n"))); err != nil { // the trap is set
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			err = r.stop(cmd, done, grace)
			if asked != grace {
				t.Errorf("stop waited %v, want %v", asked, grace)
			}
			if killed := err != nil && err.Error() == "signal: killed"; killed != tc.killed {
				t.Errorf("stop = %v, want killed %v", err, tc.killed)
			}
		})
	}
}

func TestGraceOf(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"", 10 * time.Second, false},
		{"40s", 40 * time.Second, false},
		{"0s", 0, true},
		{"junk", 0, true},
	} {
		got, err := (&acquireFlags{grace: tc.in}).graceOf()
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("graceOf(%q) = %v, %v; want %v, error %v", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
}

// The exit code lock run passes on: the command's own, else 130 for one
// an interrupt ended, else 1.
func TestExitCodeOf(t *testing.T) {
	t.Parallel()
	exited := func(code int) error {
		t.Helper()
		err := exec.Command("sh", "-c", "exit "+strconv.Itoa(code)).Run() //nolint:gosec // a script of the test's own
		if _, ok := errors.AsType[*exec.ExitError](err); !ok {
			t.Skipf("sh -c exit %d = %v: no POSIX shell here", code, err)
		}
		return err
	}
	for _, tc := range []struct {
		name        string
		err         error
		interrupted bool
		want        int
	}{
		{"success", nil, false, 0},
		{"its own code", exited(7), false, 7},
		{"its own code after an interrupt", exited(3), true, 3},
		{"no exit of its own, interrupted", errors.New("signal: interrupt"), true, 130},
		{"no exit of its own", errors.New("signal: killed"), false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCodeOf(tc.err, tc.interrupted); got != tc.want {
				t.Errorf("exitCodeOf(%v, %v) = %d, want %d", tc.err, tc.interrupted, got, tc.want)
			}
		})
	}
}
