package store

import (
	"sync"
	"testing"
)

// A pool counts a wait only when it holds its most and every connection
// is in use, not when it makes a new one. (pgxpool counts an acquire
// that waited once it succeeds, so a wait is seen after the fact.)
func TestPoolWaits(t *testing.T) {
	t.Parallel()
	db := openDB(t, newDSN(t), DBOptions{Conns: 1})
	s := openIn(t, db, DefaultSchema, Options{})
	mustCreate(t, s, NewIssue{})
	if p := db.Pool(); p.Waits != 0 || p.Max != 1 || p.Conns != 1 || p.InUse != 0 {
		t.Fatalf("Pool() after a write on an idle pool = %+v, want no wait, one connection held and none in use", p)
	}
	// Twenty readers on a pool of one: some must wait for it.
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			for range 10 {
				if _, err := s.List(t.Context(), Filter{}); err != nil {
					t.Errorf("list: %v", err)
					return
				}
			}
		})
	}
	wg.Wait()
	if p := db.Pool(); p.Waits < 1 || p.Conns != 1 || p.InUse != 0 {
		t.Errorf("Pool() after 20 readers shared a pool of one = %+v, want waits counted, and its one connection idle", p)
	}
}
