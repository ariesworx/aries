package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ariesworx/starfix/internal/client"
	"github.com/ariesworx/starfix/internal/offline"
	"github.com/ariesworx/starfix/internal/outbox"
	"github.com/ariesworx/starfix/internal/proto"
)

// budget is the replay budget of the tests here, on the bubble's clock.
const budget = time.Second

// wire is what a slow server saw, over every connection to it.
type wire struct {
	mu sync.Mutex
	// sent are the replayed writes the server answered, as "op id".
	sent []string
	// busy counts the requests in flight, and most the most at once.
	busy, most int
	dials      int
	down       bool
	// per is how long the server takes over one replayed write.
	per time.Duration
	key string
}

// dial is the server's Dial: a new connection each time it is up.
func (w *wire) dial(context.Context) (Conn, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.down {
		return nil, errDown
	}
	w.dials++
	return &slowConn{w: w}, nil
}

func (w *wire) set(down bool, per time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.down, w.per = down, per
}

func (w *wire) seen() (sent []string, most, dials int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.sent), w.most, w.dials
}

// slowConn is a connection to a server that takes wire.per over each
// replayed write. A call whose context ends first fails and takes the
// connection with it, as client.Conn does.
type slowConn struct {
	w    *wire
	mu   sync.Mutex
	lost error
}

func (c *slowConn) Call(ctx context.Context, op string, args, _ any) error {
	if op == proto.OpWatch {
		return nil
	}
	c.w.mu.Lock()
	c.w.busy++
	c.w.most = max(c.w.most, c.w.busy)
	per, down := c.w.per, c.w.down
	c.w.mu.Unlock()
	defer func() {
		c.w.mu.Lock()
		c.w.busy--
		c.w.mu.Unlock()
	}()
	if down {
		c.fail(errLost)
		return errLost
	}
	if !outbox.Queueable(op) {
		return nil
	}
	select {
	case <-time.After(per):
	case <-ctx.Done():
		err := fmt.Errorf("%s: %w", op, ctx.Err())
		c.fail(err)
		return err
	}
	var a struct{ ID string }
	b, _ := json.Marshal(args)
	_ = json.Unmarshal(b, &a)
	c.w.mu.Lock()
	c.w.sent = append(c.w.sent, op+" "+a.ID)
	c.w.mu.Unlock()
	return nil
}

func (c *slowConn) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lost = err
}

func (c *slowConn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lost
}
func (*slowConn) Close() error            { return nil }
func (*slowConn) Principal() string       { return "alice" }
func (*slowConn) Session() string         { return "s-test" }
func (*slowConn) Project() string         { return "example" }
func (*slowConn) Notices(string) []string { return nil }
func (c *slowConn) Key() client.KeyInfo {
	return client.KeyInfo{Fingerprint: c.w.key, Source: "file", By: "key: in " + client.ConfigFile}
}

// served is `sfx mcp` as it runs: Serve on a pair of pipes, with its
// loops, and an MCP client on the other ends. It must be made inside a
// synctest bubble.
type served struct {
	cs     *mcp.ClientSession
	w      *wire
	off    *offline.Client
	cancel context.CancelFunc
	done   chan error // Serve's result
}

func serve(t *testing.T) *served {
	t.Helper()
	off, key := checkout(t)
	off.Outbox = outbox.Options{ReplayBudget: budget, SendTimeout: budget}
	s := &served{w: &wire{key: key}, off: off, done: make(chan error, 1)}
	srv := New(Options{Version: "v0.2.0", Dial: s.w.dial, Offline: off, RenewEvery: -1})
	ctx, cancel := context.WithCancel(t.Context())
	s.cancel = cancel
	toServer, fromClient := io.Pipe()
	toClient, fromServer := io.Pipe()
	go func() { s.done <- srv.Serve(ctx, toServer, fromServer) }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx,
		&mcp.IOTransport{Reader: toClient, Writer: fromClient}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.cs = cs
	return s
}

// stop ends the session and waits for Serve, which waits for its loops.
func (s *served) stop(t *testing.T) {
	t.Helper()
	s.cancel()
	if err := <-s.done; err != nil {
		t.Errorf("Serve = %v", err)
	}
	_ = s.cs.Close()
}

// queue has the server go away and the agent write n comments, which
// are queued; the server then comes back, taking per over each.
func (s *served) queue(t *testing.T, n int, per time.Duration) (want []string) {
	t.Helper()
	if res := callTool(t, s.cs, "ready", nil); res.IsError {
		t.Fatalf("ready before the outage: %s", text(t, res))
	}
	s.w.set(true, 0)
	for i := range n {
		id := fmt.Sprintf("sf-%d", i+1)
		if res := callTool(t, s.cs, "comment", map[string]any{"id": id, "body": "hi"}); res.IsError {
			t.Fatalf("comment while away: %s", text(t, res))
		}
		want = append(want, proto.OpComment+" "+id)
	}
	s.w.set(false, per)
	return want
}

func (s *served) pending(t *testing.T) int {
	t.Helper()
	l, _, err := s.off.Local()
	if err != nil {
		t.Fatal(err)
	}
	n, err := l.Waiting("alice")
	if err != nil {
		t.Fatal(err)
	}
	return n.Pending
}

// TestDrainSendsWhatTheBudgetLeft is the replay budget, end to end for
// `sfx mcp`: with more waiting than one budget sends, the call that
// found the server back is held for the budget and no longer, on a new
// connection, since running out of time closed the first; what was not
// sent stays pending, and is sent in the background, once each and in
// order, over connections the background dials itself. Requests reach
// the server one at a time throughout.
func TestDrainSendsWhatTheBudgetLeft(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := serve(t)
		// 400ms a write: a budget of one second sends two, and is cut off
		// in the third.
		want := s.queue(t, 8, 400*time.Millisecond)

		start := time.Now()
		if res := callTool(t, s.cs, "ready", nil); res.IsError {
			t.Fatalf("ready with the server back: %s", text(t, res))
		}
		if held := time.Since(start); held != budget {
			t.Errorf("the first call with the server back took %s, want the replay budget, %s", held, budget)
		}
		if sent, _, dials := s.w.seen(); len(sent) != 2 || dials != 3 {
			t.Errorf("when that call returned the server had %v over %d connections; want two of the writes, and a second connection since the outage for the call", sent, dials)
		}
		if n := s.pending(t); n != 6 {
			t.Errorf("%d writes are still pending when that call returned, want 6", n)
		}

		// No call is made from here on: the background has the rest to
		// itself, connections included. Ten budgets is far more than the
		// rounds it needs, on the bubble's clock.
		time.Sleep(10 * budget)
		synctest.Wait()
		sent, most, _ := s.w.seen()
		if !slices.Equal(sent, want) {
			t.Errorf("the server answered %v\nwant each write once, in order: %v", sent, want)
		}
		if most != 1 {
			t.Errorf("%d requests were at the server at once, want one at a time", most)
		}
		if n := s.pending(t); n != 0 {
			t.Errorf("%d writes are still pending once the background is done", n)
		}
		s.stop(t)
	})
}

// A call made while the background sends is not held for the rest of
// the queue: it waits for the round in progress at most, and then at
// most for one replay of its own, should it be the one to find the
// round's connection closed. The two never have a request at the server
// at once, and between them each write still arrives once, in order.
func TestCallDuringDrainIsNotHeld(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := serve(t)
		want := s.queue(t, 12, 400*time.Millisecond)
		callTool(t, s.cs, "ready", nil)
		for range 3 {
			time.Sleep(200 * time.Millisecond) // the background is mid-round
			start := time.Now()
			if res := callTool(t, s.cs, "ready", nil); res.IsError {
				t.Fatalf("ready while the background sends: %s", text(t, res))
			}
			if held := time.Since(start); held > 2*budget {
				t.Errorf("a call made while the background sends took %s, want at most a round and a replay of its own, %s", held, 2*budget)
			}
		}
		time.Sleep(20 * budget)
		synctest.Wait()
		sent, most, _ := s.w.seen()
		if !slices.Equal(sent, want) || most != 1 {
			t.Errorf("the server answered %v, %d at once\nwant each write once, in order, one at a time: %v", sent, most, want)
		}
		s.stop(t)
	})
}

// A write lost with its connection is queued for that connection's
// principal, which the server said, also when this machine has no record
// to guess from: here the connection names no key, as one to an older
// server does not.
func TestQueuedForTheConnectionsPrincipal(t *testing.T) {
	first := &fakeConn{reply: func(op string, _ any) (any, error) {
		if op == proto.OpComment {
			return nil, errLost
		}
		return proto.Empty{}, nil
	}}
	a := newAway(t, first)
	first.key = "" // nothing for the record
	callTool(t, a.cs, "ready", nil)
	a.net.set(true)
	res := callTool(t, a.cs, "comment", map[string]any{"id": "sf-1", "body": "hi"})
	if res.IsError || len(a.list(t).Pending) != 1 {
		t.Errorf("comment lost with alice's connection = %q (error %v), with %d entries in her outbox; want it queued there", text(t, res), res.IsError, len(a.list(t).Pending))
	}
}

// A server too slow to answer one write within the budget is not
// redialed round after round: the background stops when a round sends
// nothing, and the call that opened the connection still goes through.
func TestDrainStopsWithoutProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := serve(t)
		s.queue(t, 3, 3*budget)
		start := time.Now()
		if res := callTool(t, s.cs, "ready", nil); res.IsError {
			t.Fatalf("ready with a slow server: %s", text(t, res))
		}
		if held := time.Since(start); held != budget {
			t.Errorf("the call took %s, want the replay budget, %s", held, budget)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if sent, _, dials := s.w.seen(); len(sent) != 0 || dials != 3 {
			t.Errorf("after a minute the server had %v over %d connections; want nothing sent, and no connection beyond the call's two", sent, dials)
		}
		if n := s.pending(t); n != 3 {
			t.Errorf("%d writes are pending, want all 3 kept", n)
		}
		s.stop(t)
	})
}

// The background ends with Serve, in the middle of a round or idle: the
// bubble fails the test if a goroutine is left behind.
func TestDrainEndsWithServe(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(s *served)
	}{
		{"the context ends mid-round", func(s *served) { s.cancel() }},
		{"the agent disconnects mid-round", func(s *served) { _ = s.cs.Close() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := serve(t)
				s.queue(t, 5, 400*time.Millisecond)
				callTool(t, s.cs, "ready", nil)
				time.Sleep(200 * time.Millisecond) // the background is mid-round
				tc.end(s)
				if err := <-s.done; err != nil {
					t.Errorf("Serve = %v", err)
				}
				if n := s.pending(t); n == 0 || n > 3 {
					t.Errorf("%d writes are pending after the session ended mid-round, want what was not sent kept", n)
				}
				s.cancel()
				_ = s.cs.Close()
			})
		})
	}
}
