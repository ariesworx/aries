package server

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

// The note of a session in the registry runs beside the wait for the
// connection's first request. These tests hold that note where it
// starts, as a database that does not answer would, with a store.Tracer
// that does not return.

// writeGate is a store.Tracer that records where each attempt of a write
// begins and ends, and while it is shut holds an attempt at its start
// until it is opened or the write's context ends.
type writeGate struct {
	mu   sync.Mutex
	shut chan struct{} // nil while the gate is open
	seq  []string      // "begin" and "end", in the order they happened
}

func (g *writeGate) Begin(ctx context.Context, stage store.Stage, _ int) (context.Context, func(store.Outcome)) {
	if stage != store.StageAttempt {
		return ctx, func(store.Outcome) {}
	}
	g.mu.Lock()
	g.seq = append(g.seq, "begin")
	shut := g.shut
	g.mu.Unlock()
	if shut != nil {
		select {
		case <-shut:
		case <-ctx.Done():
		}
	}
	return ctx, func(store.Outcome) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.seq = append(g.seq, "end")
	}
}

// close shuts the gate and forgets what it recorded.
func (g *writeGate) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.shut, g.seq = make(chan struct{}), nil
}

// open lets through what the gate holds, and everything after.
func (g *writeGate) open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.shut != nil {
		close(g.shut)
		g.shut = nil
	}
}

func (g *writeGate) events() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.seq)
}

// conns returns how many connections the registry counts for project
// id's store, and 0 when it holds none.
func (r *registry) conns(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.entries[id]; e != nil {
		return e.conns
	}
	return 0
}

// gatedWorld is adminWorld with every project store's writes passing g,
// which is open, with project a's store open, and with requests given
// timeout when it is set.
func gatedWorld(t *testing.T, timeout time.Duration) (*multi, *writeGate) {
	t.Helper()
	m := adminWorld(t, Limits{})
	if timeout > 0 {
		m.cfg.RequestTimeout = timeout
	}
	g := &writeGate{}
	t.Cleanup(g.open)
	m.cfg.Open = func(ctx context.Context, p store.Project) (Store, error) {
		dsn, err := DatabaseDSN(m.dsn, p.DBName)
		if err != nil {
			return nil, err
		}
		return store.Open(ctx, dsn, store.Options{CommitInterval: -1, Prefix: p.Prefix, Tracer: g})
	}
	// Its own note has been made, so the gate sees none of it later.
	c := mustDialTo(t, m.Server, dana, projA)
	waitUntil(t, "project a's first session to be noted", func() bool { return seen(c, dana) })
	return m, g
}

// sendNow writes a request and returns its id once the server has read
// it: a write to a pipe returns when the other side has taken it all.
func (c *conn) sendNow(op string, args any) uint64 {
	c.t.Helper()
	c.nextID++
	f, err := proto.Request(c.nextID, op, args)
	if err != nil {
		c.t.Fatal(err)
	}
	if err := c.enc.Encode(f); err != nil {
		c.t.Fatalf("send %s: %v", op, err)
	}
	return c.nextID
}

// seen reports whether who lists a's session.
func seen(c *conn, a store.Actor) bool {
	c.t.Helper()
	who := mustAsk[proto.WhoResult](c, proto.OpWho, proto.WhoArgs{})
	return slices.ContainsFunc(who.Agents, func(g proto.Agent) bool {
		return g.Principal == a.Principal && g.Session == a.Session
	})
}

// A connection whose session is still being noted in the registry is
// answered its status once the report has waited as long as it waits on
// Dolt at all, with the sessions unknown, and nothing else until the
// note has been made: the op it sent meanwhile starts only after the
// note's write has ended, and who then lists the session.
func TestSessionNotedBeforeFirstStoreOp(t *testing.T) {
	// Serial: bounds elapsed time, which a test running beside it would stretch.
	m, g := gatedWorld(t, 0)
	g.close()
	c := mustDialTo(t, m.Server, alice, projA)

	start := time.Now()
	res, perr := ask[proto.StatusResult](c, proto.OpStatus, proto.StatusArgs{})
	if perr != nil {
		t.Fatalf("status while the session is being noted = %v, want a report", perr)
	}
	// Half a request's time is a bound a busy machine does not reach.
	if took, most := time.Since(start), m.cfg.RequestTimeout/2; took < statusReadTimeout || took > most {
		t.Errorf("status while the session is being noted took %s; want %s, what it waits on Dolt, and at most %s", took.Round(time.Millisecond), statusReadTimeout, most)
	}
	if p := byName(res)["proj-a"]; !p.Open || p.Sessions != nil || p.SessionsUnknown != sessionsUnanswered {
		t.Errorf("status while the session is being noted has proj-a as %s; want it open, with its sessions unknown: %q", js(p), sessionsUnanswered)
	}
	if got, want := g.events(), []string{"begin"}; !slices.Equal(got, want) {
		t.Fatalf("writes when status was answered = %v, want %v: the note begun, and held", got, want)
	}

	id := c.sendNow(proto.OpCreate, proto.CreateArgs{Title: "after the note"})
	g.open()
	if f := c.read(); f.ID != id || f.Err != nil {
		t.Fatalf("create after the note = response %d, error %v; want response %d and no error", f.ID, f.Err, id)
	}
	if got, want := g.events(), []string{"begin", "end", "begin", "end"}; !slices.Equal(got, want) {
		t.Errorf("writes = %v, want %v: the note ended before create's write began", got, want)
	}
	if !seen(c, alice) {
		t.Errorf("who after a store op does not list %s/%s", alice.Principal, alice.Session)
	}
}

// A session whose note failed is noted by its next store op, and from
// then on is not noted again by each request.
func TestSessionNotedAgainAfterFailure(t *testing.T) {
	t.Parallel()
	// Long enough for a request to Dolt on a busy machine, and short
	// enough to wait out here.
	m, g := gatedWorld(t, 3*time.Second)
	g.close()
	c := mustDialTo(t, m.Server, alice, projA)

	// The create waits for the note, which runs out of time, and then
	// runs out of time itself.
	_, perr := ask[proto.CreateResult](c, proto.OpCreate, proto.CreateArgs{Title: "while writes are held"})
	if perr == nil || perr.Code != proto.CodeUnavailable || perr.Fix == "" {
		t.Errorf("create while writes are held = %+v, want unavailable with a fix", perr)
	}
	// The note ran out of time as the request did, and is logged by a
	// goroutine of its own.
	waitUntil(t, "an agent.register_failed line for the note that ran out of time", func() bool {
		return strings.Contains(m.log.String(), "event=agent.register_failed")
	})
	g.open()
	if !seen(c, alice) {
		t.Errorf("who, once writes pass again, does not list %s/%s: the next store op notes a session whose note failed", alice.Principal, alice.Session)
	}
	before := len(g.events())
	if !c.alive() {
		t.Error("the connection does not answer a list")
	}
	if after := len(g.events()); after != before {
		t.Errorf("a read on a session already noted made %d write events, want none", after-before)
	}
}

// A connection that closes while its session is being noted ends: its
// handler returns well before the note would have run out of time, the
// goroutine making the note has ended, and the project's store counts
// no connection.
func TestConnClosedWhileSessionNoted(t *testing.T) {
	// Serial: bounds elapsed time, which a test running beside it would stretch, and reads every goroutine's stack.
	m, g := gatedWorld(t, 0)
	first := m.stores.conns(projA)
	g.close()
	c := mustDialTo(t, m.Server, alice, projA)
	if got := m.stores.conns(projA); got != first+1 {
		t.Fatalf("project a's store counts %d connections, want %d", got, first+1)
	}

	start := time.Now()
	c.hangUp() // returns when the handler has
	// The note is given up after noteGrace. Half the time it would have
	// run for is a bound that a busy machine does not reach.
	took, most := time.Since(start), m.cfg.RequestTimeout/2
	t.Logf("the handler returned %s after the hang-up", took.Round(time.Millisecond))
	if took > most {
		t.Errorf("the handler of a connection closed mid-note returned after %s, want at most %s", took.Round(time.Millisecond), most)
	}
	if got := m.stores.conns(projA); got != first {
		t.Errorf("project a's store counts %d connections after the hang-up, want %d", got, first)
	}
	buf := make([]byte, 1<<20)
	if stacks := string(buf[:runtime.Stack(buf, true)]); strings.Contains(stacks, ".TouchAgent(") {
		t.Errorf("a goroutine is still in TouchAgent after the handler returned:\n%s", stacks)
	}
	if got := g.events(); !slices.Equal(got, []string{"begin", "end"}) {
		t.Errorf("writes = %v, want the one note, begun and ended", got)
	}
}

// A connection that asks only for status and leaves has been noted, as
// before: the report counts its asker's session among those seen, and
// who lists it.
func TestStatusOnlySessionIsNoted(t *testing.T) {
	t.Parallel()
	m, _ := gatedWorld(t, 0)
	for i := range 20 {
		a := store.Actor{Principal: dana.Principal, Session: fmt.Sprintf("s-status-%d", i), Machine: dana.Machine}
		c := mustDialTo(t, m.Server, a, projA)
		res := mustAsk[proto.StatusResult](c, proto.OpStatus, proto.StatusArgs{})
		// The world's first session, and each that has asked so far.
		if p := byName(res)["proj-a"]; p.Sessions == nil || *p.Sessions != i+2 {
			t.Fatalf("status asked by session %d counts proj-a's sessions as %s, want %d: its asker's among them", i, js(p.Sessions), i+2)
		}
		c.hangUp()
		if d := mustDialTo(t, m.Server, dana, projA); !seen(d, a) {
			t.Fatalf("who does not list %s/%s, which asked for status and left", a.Principal, a.Session)
		}
	}
}
