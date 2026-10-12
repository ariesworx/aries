package proto

// MaxTracestate bounds a tracestate a request may carry, in bytes: what
// the W3C Trace Context recommendation asks every party to pass on.
const MaxTracestate = 512

// TraceContext is a W3C trace context (https://www.w3.org/TR/trace-context/)
// as a request carries it: the trace it belongs to and the span that
// sent it, which is the parent of whatever span the receiver starts.
// The zero value is no trace context.
//
// Requests only carry it (design §14, decision 27). Nothing here or in
// the server makes a span or exports one.
type TraceContext struct {
	// TraceID is the trace's id: 32 lowercase hex digits, not all zero.
	TraceID string
	// SpanID is the sender's span, the parent-id of the traceparent: 16
	// lowercase hex digits, not all zero.
	SpanID string
	// Flags are the trace flags: 2 lowercase hex digits.
	Flags string
	// State is the tracestate, passed on unread: one line of printable
	// ASCII of at most MaxTracestate bytes, or empty.
	State string
}

// ParseTraceContext reads a traceparent and a tracestate from a party
// that is not trusted: a request frame, or the environment. It returns
// false, and no trace context, unless traceparent is exactly the
// version-00 form, 00-TRACEID-PARENTID-FLAGS in lowercase hex, with
// neither id all zeros. A later version's traceparent is not read as a
// 00 one.
//
// A tracestate that is longer than MaxTracestate, or anything but one
// line of printable ASCII, is dropped and the traceparent kept, as the
// recommendation lets a receiver do. A tracestate with no valid
// traceparent is nothing.
//
// A trace context that does not parse is to be ignored, never refused:
// the recommendation asks that of receivers, and a caller's broken
// tracing must not fail its request. What does parse is safe to log:
// the ids are hex. The state is printable, but it is the sender's text,
// so pass it on and do not log it.
func ParseTraceContext(traceparent, tracestate string) (TraceContext, bool) {
	const (
		traceAt, spanAt, flagsAt = 3, 36, 53
		length                   = 55
	)
	p := traceparent
	if len(p) != length || p[:traceAt] != "00-" || p[spanAt-1] != '-' || p[flagsAt-1] != '-' {
		return TraceContext{}, false
	}
	tc := TraceContext{TraceID: p[traceAt : spanAt-1], SpanID: p[spanAt : flagsAt-1], Flags: p[flagsAt:]}
	if !lowerHex(tc.TraceID) || !lowerHex(tc.SpanID) || !lowerHex(tc.Flags) || allZero(tc.TraceID) || allZero(tc.SpanID) {
		return TraceContext{}, false
	}
	if printableLine(tracestate) && len(tracestate) <= MaxTracestate {
		tc.State = tracestate
	}
	return tc, true
}

// Traceparent returns the traceparent of tc, in the version-00 form
// ParseTraceContext reads, or "" for the zero value.
func (tc TraceContext) Traceparent() string {
	if tc.TraceID == "" {
		return ""
	}
	return "00-" + tc.TraceID + "-" + tc.SpanID + "-" + tc.Flags
}

// lowerHex reports whether s is only the digits 0-9 and a-f.
func lowerHex(s string) bool {
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// allZero reports whether s is only the digit 0.
func allZero(s string) bool {
	for i := range len(s) {
		if s[i] != '0' {
			return false
		}
	}
	return true
}

// printableLine reports whether s is only printable ASCII, space to
// tilde: no control character, so no second line.
func printableLine(s string) bool {
	for i := range len(s) {
		if c := s[i]; c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}
