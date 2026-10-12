package store

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

// seen is an Observer that keeps what it was told.
type seen struct {
	mu      sync.Mutex
	wrote   []time.Duration
	retried int
	commits []error
	claims  []string // "change" or "change:reason"
}

func (o *seen) Wrote(took time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.wrote = append(o.wrote, took)
}

func (o *seen) Retried() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.retried++
}

func (o *seen) Committed(_ time.Duration, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.commits = append(o.commits, err)
}

func (o *seen) ClaimChanged(change ClaimChange, reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	s := string(change)
	if reason != "" {
		s += ":" + reason
	}
	o.claims = append(o.claims, s)
}

// take returns what was seen since the last take.
func (o *seen) take() (wrote, retried, commits int, claims []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	wrote, retried, commits, claims = len(o.wrote), o.retried, len(o.commits), o.claims
	o.wrote, o.retried, o.commits, o.claims = nil, 0, nil, nil
	return wrote, retried, commits, claims
}

// The observer is told of each write that changed a row, each retry,
// each Dolt commit and each change of a claim, once it has committed.
func TestObserver(t *testing.T) {
	obs := &seen{}
	clk := &clock{t: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)}
	s := openStore(t, newDSN(t), Options{Now: clk.now, Observer: obs})
	ctx := t.Context()
	if n := s.Migrated(); n == 0 {
		t.Errorf("Migrated() = 0 for a new database, want its migrations")
	}
	obs.take() // whatever opening did

	a := mustCreate(t, s, NewIssue{Title: "a"})
	b := mustCreate(t, s, NewIssue{Title: "b"})
	c := mustCreate(t, s, NewIssue{Title: "c"})
	steps := []struct {
		name    string
		do      func() error
		wrote   int
		retried int
		commits int
		claims  []string
	}{
		{name: "creates", do: func() error { return nil }, wrote: 3},
		{name: "a read", do: func() error { _, err := s.GetIssue(ctx, a.ID); return err }},
		{name: "a refused write", do: func() error {
			if _, err := s.CreateIssue(ctx, alice, NewIssue{}); err == nil {
				return fmt.Errorf("an issue with no title was created")
			}
			return nil
		}},
		{name: "start takes a claim", do: func() error { _, _, err := s.StartIssue(ctx, alice, a.ID, 0, false); return err },
			wrote: 1, claims: []string{"take"}},
		{name: "the same session starting again only extends", do: func() error {
			clk.add(time.Minute)
			_, _, err := s.StartIssue(ctx, alice, a.ID, 0, false)
			return err
		}, wrote: 1},
		{name: "a renew with most of the lease left writes nothing", do: func() error {
			_, err := s.RenewClaims(ctx, alice, DefaultLease, false, nil)
			return err
		}},
		{name: "a renew past half the lease", do: func() error {
			clk.add(DefaultLease/2 + time.Minute)
			_, err := s.RenewClaims(ctx, alice, DefaultLease, false, nil)
			return err
		}, wrote: 1, claims: []string{"renew"}},
		{name: "a finish releases", do: func() error {
			_, _, err := s.FinishIssue(ctx, alice, a.ID, 1, Finish{Reason: "done"})
			return err
		}, wrote: 1, claims: []string{"release"}},
		{name: "a lapsed claim expires", do: func() error {
			if _, _, err := s.StartIssue(ctx, bob, b.ID, 0, false); err != nil {
				return err
			}
			clk.add(DefaultLease + time.Minute)
			_, err := s.ReapClaims(ctx)
			return err
		}, wrote: 2, claims: []string{"take", "expire"}},
		{name: "a reap that finds nothing", do: func() error { _, err := s.ReapClaims(ctx); return err }},
		{name: "a claim ends with its holder's access", do: func() error {
			if _, _, err := s.StartIssue(ctx, bob, c.ID, 0, false); err != nil {
				return err
			}
			_, err := s.EndClaims(ctx, dana, func(string) string { return "membership removed" })
			return err
		}, wrote: 2, claims: []string{"take", "release:membership removed"}},
		{name: "a write that loses once is retried, and reported once", do: func() error {
			lost := false
			s.beforeCommit = func(context.Context) error {
				if lost {
					return nil
				}
				lost = true
				return fmt.Errorf("%w: injected", errRetry)
			}
			defer func() { s.beforeCommit = nil }()
			_, _, err := s.StartIssue(ctx, bob, b.ID, 0, false)
			return err
		}, wrote: 1, retried: 1, claims: []string{"take"}},
		{name: "a Dolt commit", do: func() error { return s.Flush(ctx) }, commits: 1},
		{name: "a flush with nothing pending commits nothing", do: func() error { return s.Flush(ctx) }},
	}
	for _, st := range steps {
		if err := st.do(); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		wrote, retried, commits, claims := obs.take()
		if wrote != st.wrote || retried != st.retried || commits != st.commits || !slices.Equal(claims, st.claims) {
			t.Errorf("%s: observed %d writes, %d retries, %d commits, claims %v; want %d, %d, %d, %v",
				st.name, wrote, retried, commits, claims, st.wrote, st.retried, st.commits, st.claims)
		}
	}
	// The observer saw the writes status counts: nothing is measured twice.
	if got := s.WriteStats().Samples; got != 12 {
		t.Errorf("WriteStats().Samples = %d, want the 12 writes the observer was told of", got)
	}
}

// A database already migrated reports none.
func TestMigratedOnReopen(t *testing.T) {
	dsn := newDSN(t)
	first, err := Open(t.Context(), dsn, Options{CommitInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	if first.Migrated() == 0 {
		t.Errorf("Migrated() = 0 on the first open")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if n := openStore(t, dsn, Options{}).Migrated(); n != 0 {
		t.Errorf("Migrated() = %d on the second open, want 0", n)
	}
}
