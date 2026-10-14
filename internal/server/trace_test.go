package server

import (
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/proto"
)

// traced sends op with a trace context on the request frame, as a client
// that has one does, and returns the response.
func traced(c *conn, op string, args any, traceparent, tracestate string) *proto.Frame {
	c.t.Helper()
	c.nextID++
	f, err := proto.Request(c.nextID, op, args)
	if err != nil {
		c.t.Fatal(err)
	}
	f.Traceparent, f.Tracestate = traceparent, tracestate
	c.wg.Go(func() { _ = c.enc.Encode(f) })
	for {
		if r := c.read(); r.T == proto.FrameRes && r.ID == f.ID {
			return r
		}
	}
}

// requestLine returns the daemon's log line for a request of op, or "".
func requestLine(m *multi, op string) string {
	for line := range strings.SplitSeq(m.log.String(), "\n") {
		if strings.Contains(line, "msg=request ") && strings.Contains(line, " op="+op+" ") {
			return line
		}
	}
	return ""
}

// TestRequestTraceContext pins decision 27: a request may carry the
// caller's W3C trace context. A valid one rides the request's context
// and its ids are on the request's log line; one that is not valid is
// ignored, the request served as any other and nothing of it logged; and
// a request without one is unchanged.
func TestRequestTraceContext(t *testing.T) {
	t.Parallel()
	const traceID, spanID = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	const parent = "00-" + traceID + "-" + spanID + "-01"
	m := newMulti(t, Limits{}, nil)
	m.project(t, projA, "alice")
	c := mustDialTo(t, m.Server, alice, projA)

	// Each case uses an op of its own, to find its log line by.
	tests := []struct {
		name          string
		op            string
		args          any
		parent, state string
		refused       proto.Code
		logged        bool
	}{
		{name: "a valid trace context", op: proto.OpReady, args: proto.LimitArgs{}, parent: parent, logged: true},
		{name: "with a tracestate", op: proto.OpBlocked, args: proto.LimitArgs{}, parent: parent, state: "vendor=opaque-STATE", logged: true},
		{name: "on a request that is refused", op: proto.OpShow, args: proto.ShowArgs{ID: "pa-nonesuch"}, parent: parent,
			refused: proto.CodeNotFound, logged: true},
		{name: "a traceparent that is not one", op: proto.OpList, args: proto.ListArgs{},
			parent: "00-EVIL\x1b[2K\nlevel=ERROR msg=forged-" + spanID + "-01", state: "EVIL=state"},
		{name: "a later version's traceparent", op: proto.OpWho, args: proto.WhoArgs{}, parent: "01-" + traceID + "-" + spanID + "-01"},
		{name: "a tracestate alone", op: proto.OpInbox, args: proto.InboxArgs{}, state: "EVIL=alone"},
		{name: "no trace context", op: proto.OpClaims, args: proto.LimitArgs{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := traced(c, tc.op, tc.args, tc.parent, tc.state)
			switch {
			case tc.refused == "" && r.Err != nil:
				t.Fatalf("%s with traceparent %q = %v; a trace context must never fail a request", tc.op, tc.parent, r.Err)
			case tc.refused != "" && (r.Err == nil || r.Err.Code != tc.refused):
				t.Fatalf("%s = %+v, want the refusal %s it gets without a trace context", tc.op, r.Err, tc.refused)
			}
			line := requestLine(m, tc.op)
			if line == "" {
				t.Fatalf("no log line for the %s request:\n%s", tc.op, m.log.String())
			}
			ids := " trace_id=" + traceID + " span_id=" + spanID
			if got := strings.Contains(line, ids); got != tc.logged {
				t.Errorf("the log line of %s names the trace: %t, want %t\n%s", tc.op, got, tc.logged, line)
			}
			if !tc.logged && (strings.Contains(line, "trace_id") || strings.Contains(line, "span_id")) {
				t.Errorf("the log line of %s names a trace it was not given a valid one for:\n%s", tc.op, line)
			}
		})
	}
	// Nothing of a context that was ignored, and no tracestate at all,
	// reaches the log.
	for _, leaked := range []string{"EVIL", "forged", "opaque-STATE"} {
		if strings.Contains(m.log.String(), leaked) {
			t.Errorf("the log holds %q, from a trace context:\n%s", leaked, m.log.String())
		}
	}
}

func TestTraceFrom(t *testing.T) {
	t.Parallel()
	if tc, ok := TraceFrom(t.Context()); ok {
		t.Errorf("TraceFrom of a context with no request = %+v, true", tc)
	}
	want := proto.TraceContext{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7", Flags: "01", State: "a=1"}
	if got, ok := TraceFrom(withTrace(t.Context(), want)); !ok || got != want {
		t.Errorf("TraceFrom(withTrace(ctx, %+v)) = %+v, %t", want, got, ok)
	}
}
