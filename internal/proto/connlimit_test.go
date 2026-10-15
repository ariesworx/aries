package proto

import (
	"errors"
	"fmt"
	"testing"
)

// AtConnLimit tells the refusal of a principal at its connection limit
// from every other: the server's own connection limit and its cap on
// handshakes are busy too, with messages alike. Released servers send the
// message ConnLimit makes, so the matcher reads theirs as well.
func TestAtConnLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"ConnLimit", ConnLimit("alice", 64), true},
		{"a principal with every allowed character", ConnLimit("a.b_c-9", 1), true},
		{"a released server's refusal", Errf(CodeBusy, "close other aries sessions, or wait for them to end, and retry",
			"alice is at its connection limit (64)"), true},
		{"wrapped", fmt.Errorf("dial: %w", ConnLimit("alice", 64)), true},
		{"the server's connection limit", Errf(CodeBusy, "close other aries sessions, or wait for them to end, and retry",
			"the server is at its connection limit (1024)"), false},
		{"the cap on handshakes", Errf(CodeBusy, "wait for your other aries sessions to finish connecting, or close them, and retry",
			"alice has 16 connections that have not said hello, the most a principal may (16)"), false},
		{"the write limit", Errf(CodeBusy, "wait 3s and retry", "alice is over the write limit (10 a second, bursts of 100)"), false},
		{"another code", Errf(CodeUnavailable, "retry", "alice is at its connection limit (64)"), false},
		{"no number", Errf(CodeBusy, "", "alice is at its connection limit ()"), false},
		{"not a principal", Errf(CodeBusy, "", "Alice is at its connection limit (64)"), false},
		{"text after it", Errf(CodeBusy, "", "alice is at its connection limit (64); and more"), false},
		{"not a refusal", errors.New("alice is at its connection limit (64)"), false},
		{"nil", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AtConnLimit(tc.err); got != tc.want {
				t.Errorf("AtConnLimit(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// ConnLimit is busy, names the principal and the limit as released
// servers did, and has the fix that says what a person can do.
func TestConnLimit(t *testing.T) {
	t.Parallel()
	e := ConnLimit("alice", 64)
	if e.Code != CodeBusy || e.Message != "alice is at its connection limit (64)" || e.Fix != FixConnLimit {
		t.Errorf("ConnLimit(alice, 64) = %+v, want busy, the released message and FixConnLimit", e)
	}
}
