package server

import (
	"context"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/telemetry"
)

// traceKey is the context key of a request's trace context.
type traceKey struct{}

// withTrace returns ctx carrying tc, the trace context a request frame
// brought and proto.ParseTraceContext accepted.
func withTrace(ctx context.Context, tc proto.TraceContext) context.Context {
	return context.WithValue(ctx, traceKey{}, tc)
}

// LogIDs returns the trace and span ids a log line logged with ctx
// carries (logs.IDs): those of the server's own span when it traces,
// sampled or not, so that the line and the trace can be joined; and with
// traces off the caller's, as the request brought them, where span_id is
// the span that sent the request. It reports false for a context with
// neither.
func LogIDs(ctx context.Context) (traceID, spanID string, ok bool) {
	if traceID, spanID, ok = telemetry.IDs(ctx); ok {
		return traceID, spanID, true
	}
	if tc, ok := TraceFrom(ctx); ok {
		return tc.TraceID, tc.SpanID, true
	}
	return "", "", false
}

// TraceFrom returns the W3C trace context of the request ctx belongs to:
// the caller's trace, and the caller's span, which is the parent of the
// server's span for the request (design §14, decision 27; §16.4). It
// reports false for a request that brought none, or none that was valid,
// and outside a request.
//
// It is the caller's context as the frame carried it. The server's own
// span, when it traces, is on the same context: telemetry.IDs reads its
// ids.
func TraceFrom(ctx context.Context) (proto.TraceContext, bool) {
	tc, ok := ctx.Value(traceKey{}).(proto.TraceContext)
	return tc, ok
}
