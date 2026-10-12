package server

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/ariesworx/starfix/internal/store"
)

// registry holds the open project stores of a server of many projects
// (design §15.1). A project's store is opened by its first handshake,
// shared by its connections, and closed once it has had none for idle.
// At most max are open: past that the longest-idle store with no
// connection is closed to make room, and when every store has
// connections the handshake is refused.
//
// Closing a store gives back its connections to Dolt, the writer and the
// readers, and nothing else: Dolt's memory follows the data written and
// stays (spike/multidb).
type registry struct {
	// open opens a project's store and readies it to serve.
	open func(context.Context, store.Project) (*served, error)
	max  int
	idle time.Duration
	// now is the clock idleness is measured by.
	now func() time.Time

	mu      sync.Mutex
	entries map[string]*entry // by project id, guarded by mu
}

// entry is one project's store in the registry: being opened, open, or
// being closed. Its fields are guarded by registry.mu, apart from the
// channels' closing.
type entry struct {
	// ready is closed when the opening ended: pr is then set, or the
	// entry has been removed and err says why.
	ready chan struct{}
	pr    *served
	err   error
	// conns counts the connections using the store, and those waiting
	// for it to open; pins the background runs using it (each). A store
	// is closed only when both are zero.
	conns, pins int
	// since is when conns last fell to zero.
	since time.Time
	// gone is set when the store starts closing, and closed when it has
	// closed and the entry is removed. Until then a handshake for the
	// project waits, so the project never has two stores, and so two
	// writers.
	gone chan struct{}
	// retired is set when the project was archived while its store was
	// in use: the store is closed as soon as nothing uses it, without
	// waiting to be idle.
	retired bool
}

// closable reports whether e's store is open and unused.
func (e *entry) closable() bool {
	return e.pr != nil && e.gone == nil && e.conns == 0 && e.pins == 0
}

// errStoresFull refuses a store past registry.max when every open store
// has connections.
var errStoresFull = errors.New("every open project store is in use")

// acquire returns p's entry with its store open (entry.pr), opening it
// if need be, and counts the caller as one of its connections until
// release. Handshakes that arrive while it opens wait for the one
// opening. It returns errStoresFull at the cap, ctx's error if ctx ends
// first, and otherwise the opening's error.
func (r *registry) acquire(ctx context.Context, p store.Project) (*entry, error) {
	for {
		r.mu.Lock()
		e := r.entries[p.ID]
		if e == nil && len(r.entries) >= r.max {
			e = r.evict()
			if e == nil {
				r.mu.Unlock()
				return nil, errStoresFull
			}
		}
		switch {
		case e == nil:
			e = &entry{ready: make(chan struct{}), conns: 1}
			r.entries[p.ID] = e
			r.mu.Unlock()
			pr, err := r.open(ctx, p)
			r.mu.Lock()
			e.pr, e.err = pr, err
			if err != nil {
				delete(r.entries, p.ID)
			}
			r.mu.Unlock()
			close(e.ready)
			if err != nil {
				return nil, err
			}
			return e, nil

		case e.gone != nil:
			// Closing: p's own store, or one that makes room. Wait for
			// it, then look again.
			gone := e.gone
			r.mu.Unlock()
			select {
			case <-gone:
			case <-ctx.Done():
				return nil, ctx.Err()
			}

		default:
			e.conns++
			r.mu.Unlock()
			select {
			case <-e.ready:
				if e.err != nil {
					return nil, e.err
				}
				return e, nil
			case <-ctx.Done():
				r.release(e)
				return nil, ctx.Err()
			}
		}
	}
}

// evict makes room for one more store. It returns an entry to wait for,
// one already closing or the longest-idle unused store, which it starts
// closing, or nil when every store is in use. r.mu is held.
func (r *registry) evict() *entry {
	var id string
	var victim *entry
	for k, e := range r.entries {
		if e.gone != nil {
			return e
		}
		if e.closable() && (victim == nil || e.since.Before(victim.since)) {
			id, victim = k, e
		}
	}
	if victim != nil {
		r.close(id, victim, "to make room")
	}
	return victim
}

// close starts closing e, the closable store of project id, and returns
// at once; e.gone is closed when it is done. r.mu is held.
func (r *registry) close(id string, e *entry, why string) {
	e.gone = make(chan struct{})
	go func() {
		defer close(e.gone)
		if err := e.pr.store.Close(); err != nil {
			e.pr.log.Error("close project store", "err", err)
		} else {
			e.pr.log.Info("project store closed", "reason", why)
		}
		r.mu.Lock()
		delete(r.entries, id)
		r.mu.Unlock()
	}()
}

// release ends one connection's use of e.
func (r *registry) release(e *entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e.conns--; e.conns == 0 {
		e.since = r.now()
	}
	r.closeRetired(e)
}

// retire closes the store of project id, which is no longer served: at
// once when nothing uses it, and otherwise when its last connection and
// background run end. A project with no store open has nothing to
// close.
func (r *registry) retire(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.entries[id]; e != nil && e.gone == nil {
		e.retired = true
		r.closeRetired(e)
	}
}

// closeRetired starts closing e if it was retired and is now unused.
// r.mu is held.
func (r *registry) closeRetired(e *entry) {
	if e.retired && e.closable() {
		r.close(e.pr.id, e, "archived")
	}
}

// closeIdle closes every store that has had no connection for r.idle,
// and waits for them.
func (r *registry) closeIdle() {
	now := r.now()
	r.closeWhere("idle", func(e *entry) bool { return now.Sub(e.since) >= r.idle })
}

// closeAll closes every unused store and waits for them. Once the
// server's connections have ended, that is every store.
func (r *registry) closeAll() {
	r.closeWhere("shutdown", func(*entry) bool { return true })
}

// closeWhere closes the closable stores that match, and waits for them
// and for any already closing.
func (r *registry) closeWhere(why string, match func(*entry) bool) {
	r.mu.Lock()
	var closing []chan struct{}
	for id, e := range r.entries {
		if e.closable() && match(e) {
			r.close(id, e, why)
		}
		if e.gone != nil {
			closing = append(closing, e.gone)
		}
	}
	r.mu.Unlock()
	for _, gone := range closing {
		<-gone
	}
}

// each runs fn for every open store, one after another. A store is not
// closed while fn runs on it.
func (r *registry) each(fn func(*served)) {
	r.mu.Lock()
	var open []*entry
	for _, e := range r.entries {
		if e.pr != nil && e.gone == nil {
			e.pins++
			open = append(open, e)
		}
	}
	r.mu.Unlock()
	for _, e := range open {
		fn(e.pr)
		r.mu.Lock()
		e.pins--
		r.closeRetired(e)
		r.mu.Unlock()
	}
}

// size returns how many stores are open, opening or closing.
func (r *registry) size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}
