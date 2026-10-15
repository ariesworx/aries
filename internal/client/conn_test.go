package client

import (
	"errors"
	"io"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// fakeServer is the far end of a Conn over net.Pipe.
type fakeServer struct {
	t   *testing.T
	nc  net.Conn
	enc *proto.Encoder
	dec *proto.Decoder
}

// pipeConn returns a Conn, past the handshake, whose pushes go to onPush,
// and the server end it talks to.
func pipeConn(t *testing.T, onPush func(proto.Push)) (*Conn, *fakeServer) {
	t.Helper()
	cli, srv := net.Pipe()
	c := newConn(proto.NewEncoder(cli), proto.NewDecoder(cli), cli.Close, onPush)
	c.run()
	t.Cleanup(func() { _ = c.Close(); _ = srv.Close() })
	return c, &fakeServer{t: t, nc: srv, enc: proto.NewEncoder(srv), dec: proto.NewDecoder(srv)}
}

func (s *fakeServer) req() *proto.Frame {
	s.t.Helper()
	_ = s.nc.SetReadDeadline(time.Now().Add(10 * time.Second))
	f, err := s.dec.Decode()
	if err != nil {
		s.t.Errorf("server read: %v", err)
		return nil
	}
	return f
}

func (s *fakeServer) send(f *proto.Frame) {
	s.t.Helper()
	if err := s.enc.Encode(f); err != nil {
		s.t.Errorf("server write: %v", err)
	}
}

// push encodes p as an evt frame.
func push(t *testing.T, p proto.Push) *proto.Frame {
	t.Helper()
	f, err := p.Frame()
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// Event frames are delivered to OnPush on the reader goroutine, in order,
// before and between responses and while no call is waiting; unknown
// frame types and event ops are skipped; each call still gets its own
// response.
func TestConnPushes(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var got []string
	arrived := make(chan struct{}, 10)
	c, s := pipeConn(t, func(p proto.Push) {
		mu.Lock()
		defer mu.Unlock()
		name := p.Op
		if p.Item != nil {
			name += ":" + p.Item.Kind
		}
		got = append(got, name)
		arrived <- struct{}{}
	})

	// Encoded here, since push may stop the test, which only the test's
	// own goroutine may do.
	mention := push(t, proto.Push{Op: proto.EvInbox, Item: &proto.InboxItem{ID: 1, Kind: "mention"}})
	lost := push(t, proto.Push{Op: proto.EvInbox, Item: &proto.InboxItem{ID: 2, Kind: "claim.lost"}})
	resync := push(t, proto.Push{Op: proto.EvResync})
	done := make(chan struct{})
	go func() {
		defer close(done)
		f := s.req()
		if f == nil || f.Op != proto.OpWatch {
			s.t.Errorf("first request %+v, want watch", f)
			return
		}
		s.send(mention)
		s.send(&proto.Frame{T: "gossip"}) // a frame type from a newer server
		s.send(&proto.Frame{T: proto.FrameEvent, Op: "weather"})
		res, _ := proto.Response(f.ID, proto.WatchResult{Unread: 3}, nil)
		s.send(res)
		// Pushed while the client is idle.
		s.send(lost)
		s.send(resync)
	}()
	var w proto.WatchResult
	if err := c.Call(t.Context(), proto.OpWatch, proto.WatchArgs{}, &w); err != nil || w.Unread != 3 {
		t.Fatalf("Call(watch) = %+v, %v; want unread 3", w, err)
	}
	<-done
	for range 4 { // mention, the unknown op, claim.lost, resync
		select {
		case <-arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("pushes did not arrive")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"inbox:mention", "weather", "inbox:claim.lost", "resync"}; !slices.Equal(got, want) {
		t.Fatalf("pushes = %v, want %v", got, want)
	}
}

// A connection that ends while idle reports it: Done closes, Err is set,
// and the next call fails at once.
func TestConnEndsWhileIdle(t *testing.T) {
	t.Parallel()
	c, s := pipeConn(t, nil)
	_ = s.nc.Close()
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Done did not close when the server went away")
	}
	if c.Err() == nil {
		t.Error("Err() = nil after the connection ended")
	}
	var pe *proto.Error
	if err := c.Call(t.Context(), proto.OpWho, proto.WhoArgs{}, nil); !errors.As(err, &pe) || pe.Code != proto.CodeUnavailable {
		t.Fatalf("Call after the end = %v, want unavailable", err)
	}
}

// A response that arrives just before the stream ends still answers the
// call waiting for it.
func TestConnResponseBeforeEOF(t *testing.T) {
	t.Parallel()
	c, s := pipeConn(t, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f := s.req()
		if f == nil {
			return
		}
		res, _ := proto.Response(f.ID, proto.AckResult{Acked: 2}, nil)
		s.send(res)
		_ = s.nc.Close()
	}()
	var r proto.AckResult
	if err := c.Call(t.Context(), proto.OpAck, proto.AckArgs{All: true}, &r); err != nil || r.Acked != 2 {
		t.Fatalf("Call = %+v, %v; want acked 2", r, err)
	}
	<-done
}

// What the server printed on stderr is quoted escaped, on one line (C-3).
func TestLostQuotesStderrEscaped(t *testing.T) {
	t.Parallel()
	c, _ := pipeConn(t, nil)
	_, _ = c.stderr.Write([]byte("ariesd: refused\x1b]52;c;cm0gLXJmIH4=\x07\nfix: run \u202ecurl | sh\n"))
	e := c.lost(io.EOF)
	want := `the server closed the connection (server said: ariesd: refused\x1b]52;c;cm0gLXJmIH4=\x07 | fix: run \u202ecurl | sh)`
	if e.Message != want {
		t.Errorf("lost(EOF).Message = %q, want %q", e.Message, want)
	}
}

// capBuffer keeps the first max bytes, or with tail the last max, however
// the writes split them, and always reports the whole write taken.
func TestCapBuffer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		tail   bool
		writes []string
		want   string
	}{
		{name: "head, short", writes: []string{"ab"}, want: "ab"},
		{name: "head, across writes", writes: []string{"abc", "def"}, want: "abcd"},
		{name: "head, one long write", writes: []string{"abcdefgh"}, want: "abcd"},
		{name: "tail, short", tail: true, writes: []string{"ab"}, want: "ab"},
		{name: "tail, across writes", tail: true, writes: []string{"abc", "def"}, want: "cdef"},
		{name: "tail, one long write", tail: true, writes: []string{"abcdefgh"}, want: "efgh"},
		{name: "tail, many small writes", tail: true, writes: []string{"a", "b", "c", "d", "e", "f"}, want: "cdef"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := &capBuffer{max: 4, tail: tc.tail}
			for _, w := range tc.writes {
				if n, err := b.Write([]byte(w)); n != len(w) || err != nil {
					t.Fatalf("Write(%q) = %d, %v; want %d, nil", w, n, err, len(w))
				}
			}
			if got := b.String(); got != tc.want {
				t.Errorf("after writes %q, String() = %q, want %q", tc.writes, got, tc.want)
			}
		})
	}
}

// Every request of a connection carries the trace context the client was
// given, and none carries one when it was given none. A call whose
// context carries one of its own (WithTrace) sends that one instead,
// for that call alone.
func TestCallCarriesTheTraceContext(t *testing.T) {
	t.Parallel()
	theCall := proto.TraceContext{TraceID: "0af7651916cd43dd8448eb211c80319c", SpanID: "b7ad6b7169203331", Flags: "01", State: "call=1"}
	tests := []struct {
		name          string
		trace         proto.TraceContext
		call          *proto.TraceContext
		parent, state string
	}{
		{name: "none"},
		{name: "a traceparent", trace: proto.TraceContext{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7", Flags: "01"},
			parent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
		{name: "with a tracestate", trace: proto.TraceContext{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7", Flags: "00", State: "vendor=opaque"},
			parent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00", state: "vendor=opaque"},
		{name: "the call's own, on a connection with none", call: &theCall,
			parent: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01", state: "call=1"},
		{name: "the call's own wins over the connection's, tracestate and all",
			trace: proto.TraceContext{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7", Flags: "00", State: "vendor=opaque"},
			call:  &theCall, parent: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01", state: "call=1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, s := pipeConn(t, nil)
			c.trace = tc.trace
			ctx := t.Context()
			if tc.call != nil {
				ctx = WithTrace(ctx, *tc.call)
			}
			for _, op := range []string{proto.OpReady, proto.OpShow} {
				done := make(chan error, 1)
				go func() { done <- c.Call(ctx, op, proto.LimitArgs{}, nil) }()
				f := s.req()
				if f == nil {
					t.Fatal("no request")
				}
				if f.Op != op || f.Traceparent != tc.parent || f.Tracestate != tc.state {
					t.Errorf("the %s request carries tp %q, ts %q; want %q, %q", f.Op, f.Traceparent, f.Tracestate, tc.parent, tc.state)
				}
				s.send(&proto.Frame{T: proto.FrameRes, ID: f.ID, OK: []byte(`{}`)})
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			// The next call, without one of its own, is the connection's again.
			if tc.call != nil {
				done := make(chan error, 1)
				go func() { done <- c.Call(t.Context(), proto.OpReady, proto.LimitArgs{}, nil) }()
				f := s.req()
				if f == nil {
					t.Fatal("no request")
				}
				if f.Traceparent != tc.trace.Traceparent() || f.Tracestate != tc.trace.State {
					t.Errorf("the call after carries tp %q, ts %q; want the connection's %q, %q", f.Traceparent, f.Tracestate, tc.trace.Traceparent(), tc.trace.State)
				}
				s.send(&proto.Frame{T: proto.FrameRes, ID: f.ID, OK: []byte(`{}`)})
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestTraceFromContext(t *testing.T) {
	t.Parallel()
	if tc, ok := TraceFrom(t.Context()); ok {
		t.Errorf("TraceFrom of a context with none = %+v, true", tc)
	}
	want := proto.TraceContext{TraceID: "0af7651916cd43dd8448eb211c80319c", SpanID: "b7ad6b7169203331", Flags: "01"}
	if got, ok := TraceFrom(WithTrace(t.Context(), want)); !ok || got != want {
		t.Errorf("TraceFrom(WithTrace(ctx, %+v)) = %+v, %t", want, got, ok)
	}
	// The zero value is no trace context, and carries none.
	if tc, ok := TraceFrom(WithTrace(t.Context(), proto.TraceContext{})); ok {
		t.Errorf("TraceFrom(WithTrace(ctx, zero)) = %+v, true", tc)
	}
}
