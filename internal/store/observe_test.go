package store

import (
	"context"
	"errors"
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
	claims  []string // "change" or "change:reason"
	locks   []string // "change", or "grant:waited"
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
func (o *seen) take() (wrote, retried int, claims []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	wrote, retried, claims = len(o.wrote), o.retried, o.claims
	o.wrote, o.retried, o.claims = nil, 0, nil
	return wrote, retried, claims
}

// The observer is told of each write that changed a row, each retry,
// and each change of a claim, once it has committed.
func TestObserver(t *testing.T) {
	t.Parallel()
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
	}
	for _, st := range steps {
		if err := st.do(); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		wrote, retried, claims := obs.take()
		if wrote != st.wrote || retried != st.retried || !slices.Equal(claims, st.claims) {
			t.Errorf("%s: observed %d writes, %d retries, claims %v; want %d, %d, %v",
				st.name, wrote, retried, claims, st.wrote, st.retried, st.claims)
		}
	}
	// The observer saw the writes status counts: nothing is measured twice.
	if got := s.WriteStats().Samples; got != 12 {
		t.Errorf("WriteStats().Samples = %d, want the 12 writes the observer was told of", got)
	}
}

// A database already migrated reports none.
func TestMigratedOnReopen(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	first, err := Open(t.Context(), dsn, Options{})
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

func (o *seen) LockChanged(change LockChange, waited time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	s := string(change)
	if change == LockGranted {
		s += ":" + waited.String()
	}
	o.locks = append(o.locks, s)
}

// takeLocks returns the changes of a lock seen since the last take.
func (o *seen) takeLocks() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := o.locks
	o.locks = nil
	return out
}

// The observer is told of each change of a lock once it has committed,
// with how long a granted place waited, and of nothing a refusal made.
func TestObserverLocks(t *testing.T) {
	t.Parallel()
	obs := &seen{}
	clk := &clock{t: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)}
	s := openStore(t, newDSN(t), Options{Now: clk.now, Observer: obs})
	ctx := t.Context()
	steps := []struct {
		name string
		do   func() error
		want []string
	}{
		{"an acquire", func() error { _, err := s.AcquireLock(ctx, alice, LockAcquire{Name: "deploy"}); return err }, []string{"acquire"}},
		{"a refused acquire", func() error {
			if _, err := s.AcquireLock(ctx, bob, LockAcquire{Name: "deploy"}); !errors.Is(err, ErrBusy) {
				return fmt.Errorf("AcquireLock while held = %v, want busy", err)
			}
			return nil
		}, nil},
		{"a place in the queue", func() error {
			_, err := s.AcquireLock(ctx, bob, LockAcquire{Name: "deploy", Wait: true})
			return err
		}, []string{"wait"}},
		{"a renewal", func() error {
			clk.add(time.Minute)
			_, err := s.RenewLock(ctx, bob, "deploy", 0, 0)
			return err
		}, []string{"renew"}},
		{"a release grants the queue", func() error {
			clk.add(30 * time.Second)
			return s.ReleaseLock(ctx, alice, "deploy", 1)
		}, []string{"release", "grant:1m30s"}},
		{"the reaper", func() error {
			clk.add(DefaultLease)
			_, err := s.ReapLocks(ctx)
			return err
		}, []string{"expire"}},
	}
	for _, st := range steps {
		if err := st.do(); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		if got := obs.takeLocks(); !slices.Equal(got, st.want) {
			t.Errorf("%s: observed %v, want %v", st.name, got, st.want)
		}
	}
}
