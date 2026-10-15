package mcpserver

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ariesworx/aries/internal/proto"
)

// turns is a dial that answers each attempt with the next of its
// results, a Conn or a refusal, and counts the attempts.
type turns struct {
	mu   sync.Mutex
	next []any // *fakeConn or error
	n    int
}

func (d *turns) dial(context.Context) (Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.n++
	if len(d.next) == 0 {
		return nil, proto.ConnLimit("alice", 2)
	}
	r := d.next[0]
	d.next = d.next[1:]
	if c, ok := r.(*fakeConn); ok {
		return c, nil
	}
	return nil, r.(error)
}

func (d *turns) then(r ...any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.next = append(d.next, r...)
}

func (d *turns) dials() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.n
}

// A session whose connection is refused because its principal is at its
// connection limit does not dial again for limitWait: a burst of tool
// calls and a renewal in that time cost one refused handshake, not one
// each. After it, the next call dials, and takes a connection as soon as
// one is free.
func TestAtConnLimitWaitsBeforeDialingAgain(t *testing.T) {
	t.Parallel()
	holds := func() *fakeConn {
		return &fakeConn{who: "alice", reply: func(op string, _ any) (any, error) {
			switch op {
			case proto.OpStart:
				return proto.StartResult{Issue: proto.Issue{ID: "sf-1", Type: "task"}, Claim: &proto.Claim{ID: "sf-1", Epoch: 1}}, nil
			case proto.OpRenew:
				return proto.ClaimsResult{Claims: []proto.Claim{{ID: "sf-1", Epoch: 1}}}, nil
			}
			return proto.ListResult{Issues: []proto.Summary{}}, nil
		}}
	}
	first, second := holds(), holds()
	d := &turns{next: []any{first}}
	clk := &clock{t: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	s := New(Options{Version: "v0.2.0", Dial: d.dial, RenewEvery: -1})
	s.now = clk.now
	t.Cleanup(func() { _ = s.Close() })
	cs := mcpSession(t, s)

	if res := callTool(t, cs, "start", map[string]any{"id": "sf-1"}); res.IsError {
		t.Fatalf("start: %s", text(t, res))
	}
	drop(first) // the redial finds the principal at its limit

	refused := func(when string) {
		t.Helper()
		res := callTool(t, cs, "ready", nil)
		if got := text(t, res); !res.IsError || !strings.HasPrefix(got, "busy: alice is at its connection limit (2)\nfix: the user's other aries sessions") {
			t.Fatalf("ready %s = %q, want the refusal at the connection limit", when, got)
		}
	}
	refused("after the connection dropped")
	if n := d.dials(); n != 2 {
		t.Fatalf("dials after the refusal = %d, want 2", n)
	}
	refused("just after the refusal")
	clk.add(limitWait - time.Second)
	s.Renew(t.Context())
	refused("within limitWait of the refusal")
	if n := d.dials(); n != 2 {
		t.Errorf("dials within limitWait of a refusal = %d, want 2: the calls and the renewal waited", n)
	}

	clk.add(time.Second)
	refused("once limitWait has passed, and the limit still holds")
	if n := d.dials(); n != 3 {
		t.Errorf("dials once limitWait passed = %d, want 3", n)
	}
	d.then(second) // a slot comes free
	clk.add(limitWait)
	if res := callTool(t, cs, "ready", nil); res.IsError {
		t.Fatalf("ready once a slot is free: %s", text(t, res))
	}
	s.Renew(t.Context())
	if ops := second.ops(); !strings.Contains(strings.Join(ops, " "), proto.OpRenew) {
		t.Errorf("ops on the new connection = %v, want the claim renewed on it", ops)
	}
	if n := d.dials(); n != 4 {
		t.Errorf("dials = %d, want 4", n)
	}
}

// Other refusals at the handshake are not held back: the next call
// dials again, as it always has.
func TestOtherDialRefusalsDialAgain(t *testing.T) {
	t.Parallel()
	d := &turns{next: []any{
		proto.Errf(proto.CodeBusy, "close other aries sessions, or wait for them to end, and retry", "the server is at its connection limit (1024)"),
		proto.Errf(proto.CodeBusy, "wait for your other aries sessions to finish connecting, or close them, and retry",
			"alice has 16 connections that have not said hello, the most a principal may (16)"),
		&fakeConn{who: "alice", reply: func(string, any) (any, error) { return proto.ListResult{Issues: []proto.Summary{}}, nil }},
	}}
	clk := &clock{t: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	s := New(Options{Version: "v0.2.0", Dial: d.dial, RenewEvery: -1})
	s.now = clk.now
	t.Cleanup(func() { _ = s.Close() })
	cs := mcpSession(t, s)
	for i, want := range []bool{true, true, false} {
		if res := callTool(t, cs, "ready", nil); res.IsError != want {
			t.Errorf("ready, call %d: error %v (%s), want %v", i+1, res.IsError, text(t, res), want)
		}
	}
	if n := d.dials(); n != 3 {
		t.Errorf("dials = %d, want 3: one for each call", n)
	}
}

// mcpSession connects an MCP client to s, and closes both when t ends.
func mcpSession(t *testing.T, s *Server) *mcp.ClientSession {
	t.Helper()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := s.MCP().Connect(t.Context(), st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}
