package dolttest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// PoolLimit is the most servers a [Pool] runs at once, on a machine with
// eight processors or more. A pool runs one for every two processors Go
// may use (GOMAXPROCS), since a test and its Dolt are each about a
// processor's work, and a test that asks for one while that many are
// held waits. On CI's four processors that is two. Four was measured
// there too: the suite took about 66 s in place of 72, 8% less, with half
// again as many Dolt processes (26 to 35 in a run, against 17 to 19),
// which is not worth it where the tests of four packages run at once. A
// server is a few hundred megabytes by the end of a package's tests.
const PoolLimit = 4

// poolLimit is the servers a pool on this machine runs at once.
func poolLimit() int {
	return min(PoolLimit, max(1, runtime.GOMAXPROCS(0)/2))
}

// Pool is the Dolt servers of one test binary. A test has a server to
// itself while it runs ([Pool.Server]), so that tests can run in parallel
// (t.Parallel) and still each see a Dolt no other test is using: one it
// may stop, count the connections of, or drop a database in. Dolt takes
// one data definition statement at a time, and a session that reads
// information_schema while another drops a database fails with "no root
// value found in session", so tests that shared a server would wait for
// each other and fail now and then.
//
// The pool starts a server when a test asks and none is free, up to one
// for every two processors and [PoolLimit] at most, each in a temporary
// directory of its own; a server a test
// has done with goes to the next. [Pool.Close] stops them all and removes
// their directories. The zero value is not usable: make one with
// [NewPool].
type Pool struct {
	prefix string
	opts   Options
	limit  int
	orphan sync.Once // starts the watch for the parent's end (Warm)

	mu      sync.Mutex
	free    *sync.Cond // signaled when a server is given up, a start fails, or the pool closes
	all     []*Server
	idle    []*Server
	starts  int // servers starting or started
	waiting int // holders waiting for a server
	held    map[string]*hold
	closed  bool
}

// hold is a holder's server and how many times it has taken it.
type hold struct {
	srv  *Server
	refs int
}

// NewPool returns a pool whose servers are started with opts, each in a
// new directory under the default temporary directory whose name begins
// with prefix. It starts none: the caller should Close it.
func NewPool(prefix string, opts Options) *Pool {
	p := &Pool{prefix: prefix, opts: opts, limit: poolLimit(), held: map[string]*hold{}}
	p.free = sync.NewCond(&p.mu)
	return p
}

// Server returns the server t's test holds, having first taken one for
// it if it held none: a test and its subtests have the same one, for as
// long as any of them that asked is running. The test gives it up when
// it ends, after the cleanups it registers from here on, so a database
// made on it can be dropped in one. The error is [Start]'s, [ErrNoDolt]
// included, on which callers skip.
func (p *Pool) Server(t testing.TB) (*Server, error) {
	t.Helper()
	// A test's server is its top-level test's: a subtest reads what its
	// parent wrote.
	holder, _, _ := strings.Cut(t.Name(), "/")
	s, err := p.take(holder)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() {
		if err := p.release(holder); err != nil {
			t.Errorf("drop what the test left on its Dolt server: %v", err)
		}
	})
	return s, nil
}

// Database makes an empty database on the server t's test holds
// ([Pool.Server]) and returns the DSN of [User] on it. The database is
// dropped when t ends, after the cleanups t registers from here on, which
// close what the test opened on it; a drop that fails fails t.
func (p *Pool) Database(t testing.TB) (string, error) {
	t.Helper()
	s, err := p.Server(t)
	if err != nil {
		return "", err
	}
	dsn, err := s.NewDatabase(t.Context())
	if err != nil {
		return "", err
	}
	t.Cleanup(func() {
		if err := s.DropDatabase(dsn); err != nil {
			t.Errorf("drop the test's database: %v", err)
		}
	})
	return dsn, nil
}

// Warm starts the pool's first server, if it has none, and leaves it for
// the first test that asks. Called before the tests run, it keeps a test
// that counts goroutines from being the one to start a server. A failure
// to start is reported to the tests, by Server.
//
// From then on the process also ends, and its servers with it, once the
// process that started it is gone: a test binary whose go test was
// killed would otherwise run on to the end of its tests.
func (p *Pool) Warm() {
	p.orphan.Do(func() { endWithParent(func() { _ = p.Close() }) })
	const holder = "\x00warm" // no test's name
	if _, err := p.take(holder); err == nil {
		_ = p.release(holder)
	}
}

// take gives holder a server: the one it has, or else one no one holds,
// or a new one, or, at the limit, the next to be given up.
func (p *Pool) take(holder string) (*Server, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		if p.closed {
			return nil, errors.New("dolttest: the pool is closed")
		}
		if h := p.held[holder]; h != nil {
			h.refs++
			return h.srv, nil
		}
		if n := len(p.idle); n > 0 {
			s := p.idle[n-1]
			p.idle = p.idle[:n-1]
			p.held[holder] = &hold{srv: s, refs: 1}
			return s, nil
		}
		if p.starts < p.limit {
			p.starts++
			p.mu.Unlock()
			s, err := p.start()
			p.mu.Lock()
			if err != nil {
				p.starts--
				p.free.Broadcast()
				return nil, err
			}
			if p.closed {
				// Close did not know of this one.
				p.mu.Unlock()
				err := errors.Join(s.Stop(), os.RemoveAll(s.dir))
				p.mu.Lock()
				return nil, errors.Join(errors.New("dolttest: the pool is closed"), err)
			}
			p.all = append(p.all, s)
			// Another goroutine of the same test may have taken one
			// meanwhile: go round again.
			p.idle = append(p.idle, s)
			continue
		}
		p.waiting++
		p.free.Wait()
		p.waiting--
	}
}

// release gives up one of holder's takes, and the server with the last.
// The server goes to the next holder with no database on it: what the
// holder left, such as a database the daemon under test made for a new
// project, is dropped first, and a failure to drop it is the error.
func (p *Pool) release(holder string) error {
	p.mu.Lock()
	h := p.held[holder]
	if h == nil {
		p.mu.Unlock()
		return nil
	}
	if h.refs--; h.refs > 0 {
		p.mu.Unlock()
		return nil
	}
	delete(p.held, holder)
	closed := p.closed
	p.mu.Unlock()
	var err error
	if !closed {
		err = h.srv.dropAll()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.idle = append(p.idle, h.srv)
	p.free.Broadcast()
	if p.closed {
		// Close stopped the server, perhaps under the drop.
		return nil
	}
	return err
}

// start starts one server in a directory of its own.
func (p *Pool) start() (*Server, error) {
	// Start looks for dolt too, but only after the directory is made.
	if _, err := lookDolt(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", p.prefix)
	if err != nil {
		return nil, fmt.Errorf("dolttest: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s, err := StartWith(ctx, dir, p.opts)
	if err != nil {
		return nil, errors.Join(err, os.RemoveAll(dir))
	}
	return s, nil
}

// Close stops every server the pool started and removes its directory.
// The tests have ended by then; a test still running finds its server
// gone.
func (p *Pool) Close() error {
	p.mu.Lock()
	p.closed = true
	all := p.all
	p.all, p.idle = nil, nil
	p.free.Broadcast()
	p.mu.Unlock()
	errs := make([]error, len(all))
	var wg sync.WaitGroup
	for i, s := range all {
		wg.Go(func() { errs[i] = errors.Join(s.Stop(), os.RemoveAll(s.dir)) })
	}
	wg.Wait()
	return errors.Join(errs...)
}
