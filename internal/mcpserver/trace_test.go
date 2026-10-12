package mcpserver

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ariesworx/starfix/internal/proto"
)

// primeReply answers every request prime makes with a short list.
func primeReply(string, any) (any, error) {
	return proto.ListResult{Issues: summaries(2, 20)}, nil
}

// A tool call may bring the agent's W3C trace context in its request's
// metadata: "traceparent" and "tracestate" in _meta, as OpenTelemetry's
// convention for MCP has it. Every request the call makes of starfixd
// then carries it; a call that brings none, or one that is not valid,
// carries none of its own, so the connection's (the environment's) goes
// instead. A malformed one never fails the call.
func TestToolCallTraceContext(t *testing.T) {
	const parent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	valid := proto.TraceContext{TraceID: "0af7651916cd43dd8448eb211c80319c", SpanID: "b7ad6b7169203331", Flags: "01"}
	withState := valid
	withState.State = "vendor=opaque"
	for _, tc := range []struct {
		name string
		meta mcp.Meta
		want proto.TraceContext
	}{
		{"no metadata", nil, proto.TraceContext{}},
		{"metadata without one", mcp.Meta{"progressToken": "p-1"}, proto.TraceContext{}},
		{"a traceparent", mcp.Meta{"traceparent": parent}, valid},
		{"with a tracestate", mcp.Meta{"traceparent": parent, "tracestate": "vendor=opaque"}, withState},
		{"beside other metadata", mcp.Meta{"traceparent": parent, "progressToken": "p-1", "example.com/x": 1}, valid},
		{"a tracestate alone is nothing", mcp.Meta{"tracestate": "vendor=opaque"}, proto.TraceContext{}},
		{"a bad tracestate is dropped", mcp.Meta{"traceparent": parent, "tracestate": "two\nlines"}, valid},
		{"a tracestate that is not a string is dropped", mcp.Meta{"traceparent": parent, "tracestate": []any{"a=1"}}, valid},
		{"a traceparent that is not one", mcp.Meta{"traceparent": "00-EVIL-01", "tracestate": "vendor=opaque"}, proto.TraceContext{}},
		{"uppercase is not valid", mcp.Meta{"traceparent": strings.ToUpper(parent)}, proto.TraceContext{}},
		{"a later version's", mcp.Meta{"traceparent": "01" + parent[2:]}, proto.TraceContext{}},
		{"a traceparent that is not a string", mcp.Meta{"traceparent": 42}, proto.TraceContext{}},
		{"a traceparent that is an object", mcp.Meta{"traceparent": map[string]any{"id": parent}}, proto.TraceContext{}},
		{"other spellings are not read", mcp.Meta{"TRACEPARENT": parent, "Traceparent": parent, "trace_parent": parent}, proto.TraceContext{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &fakeConn{who: "alice", reply: primeReply}
			cs, _ := connect(t, conn)
			// prime makes several requests: each carries the call's.
			res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Meta: tc.meta, Name: "prime"})
			if err != nil || res.IsError {
				t.Fatalf("prime with _meta %v = %v, %s; a trace context must never fail a call", tc.meta, err, text(t, res))
			}
			conn.mu.Lock()
			calls := append([]call(nil), conn.calls...)
			conn.mu.Unlock()
			if len(calls) < 2 {
				t.Fatalf("prime made %d requests, want several", len(calls))
			}
			for _, c := range calls {
				if c.trace != tc.want {
					t.Errorf("the %s request carries %+v, want %+v", c.op, c.trace, tc.want)
				}
			}
			// The next call, with no metadata, inherits nothing.
			callTool(t, cs, "ready", nil)
			conn.mu.Lock()
			last := conn.calls[len(conn.calls)-1]
			conn.mu.Unlock()
			if last.op != proto.OpReady || last.trace != (proto.TraceContext{}) {
				t.Errorf("the call after carries %+v on its %s request, want none", last.trace, last.op)
			}
		})
	}
}
