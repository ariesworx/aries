package cli

import (
	"bytes"
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
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

// The exit code lock run passes on: the command's own, else 130 for one
// an interrupt ended, else 1.
func TestExitCodeOf(t *testing.T) {
	t.Parallel()
	exited := func(code int) error {
		t.Helper()
		err := exec.Command("sh", "-c", "exit "+string(rune('0'+code))).Run()
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
