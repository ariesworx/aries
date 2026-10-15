package store

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// Going away (`aries away D`, design §18.6) extends every claim the
// principal holds, in every session, to at least now plus D: a claim
// with less left than D, however much, is extended, which a renewal,
// extending only past half its lease, would not do.
func TestAwayExtendsToAtLeastTheDuration(t *testing.T) {
	t.Parallel()
	s, clk := clockStore(t)
	ctx := t.Context()
	a := mustCreate(t, s, NewIssue{Title: "a"})
	b := mustCreate(t, s, NewIssue{Title: "b"})
	if _, _, err := s.StartIssue(ctx, alice, a.ID, 3*time.Hour, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.StartIssue(ctx, alice2, b.ID, 8*time.Hour, false); err != nil {
		t.Fatal(err)
	}
	start := clk.now()
	got, err := s.RenewClaims(ctx, alice, 4*time.Hour, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[IssueID]time.Time{a.ID: start.Add(4 * time.Hour), b.ID: start.Add(8 * time.Hour)}
	if len(got) != len(want) {
		t.Fatalf("RenewClaims(away 4h) = %+v, want %d claims", got, len(want))
	}
	for _, c := range got {
		if !c.ExpiresAt.Equal(want[c.Issue]) {
			t.Errorf("RenewClaims(away 4h) has %s expiring at %s, want %s", c.Issue, c.ExpiresAt, want[c.Issue])
		}
		if live, err := s.ClaimOf(ctx, c.Issue); err != nil || live == nil || !live.ExpiresAt.Equal(want[c.Issue]) {
			t.Errorf("ClaimOf(%s) = %+v, %v after away 4h, want it to expire at %s", c.Issue, live, err, want[c.Issue])
		}
	}
}

// Going away is a person's act, and on the record: each claim it extends
// gets a claim.away event with its expiry before and after. One that
// extends nothing records nothing.
func TestAwayIsRecorded(t *testing.T) {
	t.Parallel()
	s, clk := clockStore(t)
	ctx := t.Context()
	a := mustCreate(t, s, NewIssue{Title: "a"})
	_, claim, err := s.StartIssue(ctx, alice2, a.ID, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	seq := lastSeq(t, s)
	if _, err := s.RenewClaims(ctx, alice, 2*24*time.Hour, true, nil); err != nil {
		t.Fatal(err)
	}
	evs, err := s.History(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := evs[len(evs)-1]
	if last.Seq != seq+1 || last.Op != OpClaimAway || last.Actor != alice {
		t.Fatalf("last event after away = %d %s by %+v, want %d %s by %+v", last.Seq, last.Op, last.Actor, seq+1, OpClaimAway, alice)
	}
	var before, after struct {
		Holder    Actor     `json:"holder"`
		Epoch     int64     `json:"epoch"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(last.Before, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(last.After, &after); err != nil {
		t.Fatal(err)
	}
	if before.Holder != alice2 || before.Epoch != claim.Epoch || !before.ExpiresAt.Equal(claim.ExpiresAt) {
		t.Errorf("claim.away before = %+v, want the claim as it was, %+v", before, claim)
	}
	if want := clk.now().Add(2 * 24 * time.Hour); !after.ExpiresAt.Equal(want) {
		t.Errorf("claim.away after expires at %s, want %s", after.ExpiresAt, want)
	}

	// Asking again for no more than it has extends nothing, and records
	// nothing; nor does asking again a moment later, as `aries away` does
	// when it sends each claim's paths.
	seq = lastSeq(t, s)
	if _, err := s.RenewClaims(ctx, alice, 24*time.Hour, true, nil); err != nil {
		t.Fatal(err)
	}
	clk.add(30 * time.Second)
	if _, err := s.RenewClaims(ctx, alice, 2*24*time.Hour, true, map[IssueID][]string{a.ID: {"go.mod"}}); err != nil {
		t.Fatal(err)
	}
	if ops := opsSince(t, s, seq); len(ops) != 1 || ops[0] != OpIssuePaths {
		t.Errorf("aways that extended nothing recorded %v, want only the paths sent", ops)
	}
}

// How far away may reach is the store's AwayMax, a server setting
// (away_max, rule 18), 7 days by default; a renewal of one session's
// claims stays under MaxClaimLease whatever it is.
func TestAwayCap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		max     time.Duration
		lease   time.Duration
		all     bool
		wantErr error
	}{
		{name: "default, a week", lease: 7 * 24 * time.Hour, all: true},
		{name: "default, past a week", lease: 7*24*time.Hour + time.Minute, all: true, wantErr: ErrInvalid},
		{name: "set lower, at it", max: 2 * 24 * time.Hour, lease: 2 * 24 * time.Hour, all: true},
		{name: "set lower, past it", max: 2 * 24 * time.Hour, lease: 3 * 24 * time.Hour, all: true, wantErr: ErrInvalid},
		{name: "set higher, at it", max: 30 * 24 * time.Hour, lease: 30 * 24 * time.Hour, all: true},
		{name: "one session, set higher, past a day", max: 30 * 24 * time.Hour, lease: 2 * 24 * time.Hour, wantErr: ErrInvalid},
		{name: "under a minute", lease: time.Second, all: true, wantErr: ErrInvalid},
	}
	dsn := newDSN(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t, dsn, Options{AwayMax: tc.max})
			if want := cmpOr(tc.max, MaxLease); s.AwayMax() != want {
				t.Errorf("AwayMax() = %s, want %s", s.AwayMax(), want)
			}
			_, err := s.RenewClaims(t.Context(), alice, tc.lease, tc.all, nil)
			if !errors.Is(err, tc.wantErr) || (err != nil) != (tc.wantErr != nil) {
				t.Errorf("RenewClaims(%s, all %t) with AwayMax %s = %v, want %v", tc.lease, tc.all, s.AwayMax(), err, tc.wantErr)
			}
		})
	}
	if _, err := Open(t.Context(), dsn, Options{AwayMax: -time.Hour}); !errors.Is(err, ErrInvalid) {
		t.Errorf("Open with AwayMax -1h = %v, want ErrInvalid", err)
	}
}

func cmpOr(a, b time.Duration) time.Duration {
	if a != 0 {
		return a
	}
	return b
}

// The reaper honors a lease away extended: it ends the claim only once
// the extended lease has run out.
func TestAwayHoldsAgainstTheReaper(t *testing.T) {
	t.Parallel()
	s, clk := clockStore(t)
	ctx := t.Context()
	a := mustCreate(t, s, NewIssue{Title: "a"})
	if _, _, err := s.StartIssue(ctx, alice, a.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenewClaims(ctx, alice, 3*24*time.Hour, true, nil); err != nil {
		t.Fatal(err)
	}
	clk.add(2 * 24 * time.Hour)
	if reaped, err := s.ReapClaims(ctx); err != nil || len(reaped) != 0 {
		t.Fatalf("ReapClaims two days into a three-day away = %+v, %v, want nothing reaped", reaped, err)
	}
	if c, err := s.ClaimOf(ctx, a.ID); err != nil || c == nil || c.Holder != alice {
		t.Fatalf("ClaimOf two days into a three-day away = %+v, %v, want alice's claim", c, err)
	}
	clk.add(24*time.Hour + time.Second)
	if reaped, err := s.ReapClaims(ctx); err != nil || len(reaped) != 1 || reaped[0].Issue != a.ID {
		t.Fatalf("ReapClaims past a three-day away = %+v, %v, want %s reaped", reaped, err, a.ID)
	}
}

// A clock that jumps forward while a person is away does not cut their
// absence short: the store adds the jump to the extended lease, as to
// any live lease (decision 37).
func TestAwayLeaseKeepsAClockJump(t *testing.T) {
	t.Parallel()
	c := newTwoClocks()
	s, _ := steppedStore(t, "", c, Options{})
	ctx := t.Context()
	a := mustCreate(t, s, NewIssue{Title: "a"})
	if _, _, err := s.StartIssue(ctx, alice, a.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	got, err := s.RenewClaims(ctx, alice, 2*24*time.Hour, true, nil)
	if err != nil || len(got) != 1 {
		t.Fatalf("away: %+v, %v", got, err)
	}
	c.run(time.Hour)
	c.step(36 * time.Hour)
	if reaped, err := s.ReapClaims(ctx); err != nil || len(reaped) != 0 {
		t.Fatalf("ReapClaims after a 36h jump into a two-day away = %+v, %v, want nothing reaped", reaped, err)
	}
	want := got[0].ExpiresAt.Add(36 * time.Hour)
	if live, err := s.ClaimOf(ctx, a.ID); err != nil || live == nil || !live.ExpiresAt.Equal(want) {
		t.Errorf("ClaimOf after the jump = %+v, %v, want the away lease extended by the jump, to %s", live, err, want)
	}
}
