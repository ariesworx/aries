package server

import (
	"context"

	"github.com/ariesworx/starfix/internal/proto"
)

// traceKey is the context key of a request's trace context.
type traceKey struct{}

// withTrace returns ctx carrying tc, the trace context a request frame
// brought and proto.ParseTraceContext accepted.
func withTrace(ctx context.Context, tc proto.TraceContext) context.Context {
	return context.WithValue(ctx, traceKey{}, tc)
}

// TraceFrom returns the W3C trace context of the request ctx belongs to:
// the caller's trace, and the caller's span as the parent of any span
// started for the request. It reports false for a request that brought
// none, or none that was valid, and outside a request.
//
// The server only carries it (design §14, decision 27): it starts and
// exports no spans, and nothing but the request's log line reads it yet.
func TraceFrom(ctx context.Context) (proto.TraceContext, bool) {
	tc, ok := ctx.Value(traceKey{}).(proto.TraceContext)
	return tc, ok
}
