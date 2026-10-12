package main

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/server"
	"github.com/ariesworx/starfix/internal/store"
)

// writtenAhead writes one event to the database at dsn by a clock ahead
// of this machine's, as a machine whose clock ran ahead did, and returns
// the event's time.
func writtenAhead(t *testing.T, dsn string, ahead time.Duration) time.Time {
	t.Helper()
	at := time.Now().Add(ahead).UTC().Truncate(time.Microsecond)
	st, err := store.Open(t.Context(), dsn, store.Options{Prefix: "sf", CommitInterval: -1, Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	actor := store.Actor{Principal: "alice", Session: "s", Machine: "m"}
	if _, err := st.CreateIssue(t.Context(), actor, store.NewIssue{Title: "written ahead"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return at
}

// serve refuses to start when its clock is behind its database's
// history, unless told to warn, or to accept that history once.
func TestServeClockBehindHistory(t *testing.T) {
	tests := []struct {
		name    string
		ahead   time.Duration // how far ahead of the clock the history is
		config  string
		accept  time.Duration // --accept-clock-behind, from the history's time; 0 is unset
		refused bool
	}{
		{name: "no history ahead", ahead: -time.Hour},
		{name: "ahead by less than the tolerance", ahead: 20 * time.Second},
		{name: "ahead, by default", ahead: 2 * time.Hour, refused: true},
		{name: "ahead, a wider tolerance", ahead: 2 * time.Hour, config: "clock_behind_tolerance: 3h\n"},
		{name: "ahead, warn", ahead: 2 * time.Hour, config: "clock_behind: warn\n"},
		{name: "ahead, accepted once", ahead: 2 * time.Hour, accept: 2 * time.Second},
		{name: "ahead, an older history accepted", ahead: 2 * time.Hour, accept: -time.Hour, refused: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newSite(t, "")
			rewrite(t, s, "project: "+adopted+"\nadmins: [dana]\n"+tc.config)
			at := writtenAhead(t, s.dsn, tc.ahead)
			set := s.settings(t)
			if tc.accept != 0 {
				set.AcceptClockBehind = at.Add(tc.accept)
			}
			d, err := openDaemon(t.Context(), set, slog.New(slog.DiscardHandler))
			if err == nil {
				defer func() { _ = d.Close() }()
			}
			behind, isBehind := errors.AsType[*server.ClockBehindError](err)
			if isBehind != tc.refused || (err != nil && !isBehind) {
				t.Fatalf("openDaemon with the history %s ahead, config %q = %v; want refused %v", tc.ahead, tc.config, err, tc.refused)
			}
			if tc.refused && (behind.History.Before(at) || behind.History.Sub(at) > time.Second) {
				t.Errorf("the refusal gives the history's time as %s, want %s", behind.History, at)
			}
		})
	}
}

func TestServeAcceptClockBehindFlag(t *testing.T) {
	err := serve(t.Context(), []string{"--dev", "--accept-clock-behind", "tomorrow"})
	if _, ok := errors.AsType[usageError](err); !ok || !strings.Contains(err.Error(), "--accept-clock-behind") {
		t.Errorf("serve --accept-clock-behind tomorrow = %v, want a usage error naming the flag", err)
	}
}

// An upgrade asks first what the restarted daemon would find. Its
// refusal to start would fail the health check, and the upgrade would
// put the old binary back and blame the release.
func TestUpgradeChecksTheClockFirst(t *testing.T) {
	t.Run("behind, stop", func(t *testing.T) {
		r := newRig(t, "v0.2.0", "v0.3.0", "systemd_unit: starfixd.service\n")
		r.dsn = newDSN(t)
		r.d.clock = clockPreflight
		at := writtenAhead(t, r.dsn, 3*time.Hour)
		_, err := r.run(t)
		if _, ok := errors.AsType[*server.ClockBehindError](err); !ok {
			t.Fatalf("upgrade with the clock 3h behind the history = %v, want a *server.ClockBehindError", err)
		}
		for _, w := range []string{"nothing was changed", "--accept-clock-behind " + at.Truncate(time.Second).Add(2*time.Second).Format(time.RFC3339), "fix:"} {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("the refusal lacks %q:\n%v", w, err)
			}
		}
		if r.exeHolds(t) != "v0.2.0" || len(r.backups)+len(r.restarts) != 0 {
			t.Errorf("the refused upgrade changed something: binary %s, backups %v, restarts %v", r.exeHolds(t), r.backups, r.restarts)
		}
		r.d.euid = func() int { return 0 }
		out, err := r.run(t, "--check")
		if err != nil || !strings.Contains(out, "clock: STARFIXD WILL NOT START AGAIN") {
			t.Errorf("upgrade --check = %q, %v; want a clock line saying starfixd will not start again", out, err)
		}
	})
	t.Run("behind, warn", func(t *testing.T) {
		r := newRig(t, "v0.2.0", "v0.3.0", "systemd_unit: starfixd.service\nclock_behind: warn\n")
		r.dsn = newDSN(t)
		r.d.clock = clockPreflight
		writtenAhead(t, r.dsn, 3*time.Hour)
		out, err := r.run(t)
		if err != nil || !strings.Contains(out, "warning: the server's clock is behind its own history") || !strings.Contains(out, "starfixd upgraded v0.2.0 -> v0.3.0") {
			t.Errorf("upgrade with clock_behind: warn = %q, %v; want a warning and the upgrade", out, err)
		}
	})
	t.Run("not behind", func(t *testing.T) {
		r := newRig(t, "v0.2.0", "v0.3.0", "systemd_unit: starfixd.service\n")
		r.dsn = newDSN(t)
		r.d.clock = clockPreflight
		writtenAhead(t, r.dsn, -time.Hour)
		r.d.euid = func() int { return 0 }
		if out, err := r.run(t, "--check"); err != nil || !strings.Contains(out, "clock: not behind the server's history\n") {
			t.Errorf("upgrade --check = %q, %v; want a clock line saying it is not behind", out, err)
		}
		r.d.euid = func() int { return 1000 }
		if out, err := r.run(t); err != nil || !strings.Contains(out, "starfixd upgraded v0.2.0 -> v0.3.0") {
			t.Errorf("upgrade = %q, %v", out, err)
		}
	})
}
