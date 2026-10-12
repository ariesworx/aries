package proto

import (
	"strings"
	"testing"
)

const (
	testTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	testSpanID  = "00f067aa0ba902b7"
	testParent  = "00-" + testTraceID + "-" + testSpanID + "-01"
)

func TestParseTraceContext(t *testing.T) {
	valid := TraceContext{TraceID: testTraceID, SpanID: testSpanID, Flags: "01"}
	withState := valid
	withState.State = "rojo=00f067aa0ba902b7,congo=t61rcWkgMzE"
	tests := []struct {
		name          string
		parent, state string
		want          TraceContext
		ok            bool
	}{
		{name: "the W3C example", parent: testParent, want: valid, ok: true},
		{name: "not sampled", parent: "00-" + testTraceID + "-" + testSpanID + "-00",
			want: TraceContext{TraceID: testTraceID, SpanID: testSpanID, Flags: "00"}, ok: true},
		{name: "with a tracestate", parent: testParent, state: withState.State, want: withState, ok: true},
		{name: "a tracestate of 512 bytes", parent: testParent, state: strings.Repeat("a", MaxTracestate),
			want: TraceContext{TraceID: testTraceID, SpanID: testSpanID, Flags: "01", State: strings.Repeat("a", MaxTracestate)}, ok: true},

		// A tracestate that is not acceptable is dropped, and the parent kept.
		{name: "a tracestate of 513 bytes", parent: testParent, state: strings.Repeat("a", MaxTracestate+1), want: valid, ok: true},
		{name: "a tracestate of two lines", parent: testParent, state: "a=1\nb=2", want: valid, ok: true},
		{name: "a tracestate with a tab", parent: testParent, state: "a=1\tb=2", want: valid, ok: true},
		{name: "a tracestate with an escape", parent: testParent, state: "a=\x1b[2K", want: valid, ok: true},
		{name: "a tracestate with DEL", parent: testParent, state: "a=\x7f", want: valid, ok: true},
		{name: "a tracestate that is not ASCII", parent: testParent, state: "a=é", want: valid, ok: true},

		// Anything but the exact version-00 form is no trace context.
		{name: "empty"},
		{name: "a tracestate alone", state: "a=1"},
		{name: "a later version", parent: "01-" + testTraceID + "-" + testSpanID + "-01"},
		{name: "version ff", parent: "ff-" + testTraceID + "-" + testSpanID + "-01"},
		{name: "a later version's extra field", parent: testParent + "-what"},
		{name: "uppercase hex in the trace id", parent: "00-" + strings.ToUpper(testTraceID) + "-" + testSpanID + "-01"},
		{name: "uppercase hex in the span id", parent: "00-" + testTraceID + "-00F067AA0BA902B7-01"},
		{name: "uppercase hex in the flags", parent: "00-" + testTraceID + "-" + testSpanID + "-0A"},
		{name: "a trace id of zeros", parent: "00-" + strings.Repeat("0", 32) + "-" + testSpanID + "-01"},
		{name: "a span id of zeros", parent: "00-" + testTraceID + "-" + strings.Repeat("0", 16) + "-01"},
		{name: "a short trace id", parent: "00-" + testTraceID[1:] + "-" + testSpanID + "-01"},
		{name: "a long span id", parent: "00-" + testTraceID + "-" + testSpanID + "0-01"},
		{name: "one flag digit", parent: "00-" + testTraceID + "-" + testSpanID + "-1"},
		{name: "not hex", parent: "00-" + strings.Repeat("g", 32) + "-" + testSpanID + "-01"},
		{name: "other separators", parent: "00_" + testTraceID + "_" + testSpanID + "_01"},
		{name: "a space before", parent: " " + testParent},
		{name: "a newline after", parent: testParent + "\n"},
		{name: "an escape inside", parent: "00-" + testTraceID[:30] + "\x1b[-" + testSpanID + "-01"},
		{name: "very long", parent: strings.Repeat(testParent, 1000)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseTraceContext(tc.parent, tc.state)
			if got != tc.want || ok != tc.ok {
				t.Errorf("ParseTraceContext(%q, %q) = %+v, %t; want %+v, %t", tc.parent, tc.state, got, ok, tc.want, tc.ok)
			}
			if ok && got.Traceparent() != tc.parent {
				t.Errorf("ParseTraceContext(%q).Traceparent() = %q, want it back unchanged", tc.parent, got.Traceparent())
			}
		})
	}
	if got := (TraceContext{}).Traceparent(); got != "" {
		t.Errorf("TraceContext{}.Traceparent() = %q, want empty", got)
	}
}

// FuzzParseTraceContext gives the parser what a client may send. It never
// panics, and whatever it accepts is the exact W3C form: ids of lowercase
// hex that are not all zeros, a state of one printable ASCII line within
// the bound, and a traceparent that parses back to the same value.
func FuzzParseTraceContext(f *testing.F) {
	f.Add(testParent, "")
	f.Add(testParent, "rojo=00f067aa0ba902b7")
	f.Add("00-"+strings.Repeat("0", 32)+"-"+testSpanID+"-01", "")
	f.Add("01-"+testTraceID+"-"+testSpanID+"-01-extra", "a=1\nb=2")
	f.Add("", "\x1b[2K")
	f.Add(strings.ToUpper(testParent), strings.Repeat("a", MaxTracestate+1))
	f.Fuzz(func(t *testing.T, parent, state string) {
		tc, ok := ParseTraceContext(parent, state)
		if !ok {
			if tc != (TraceContext{}) {
				t.Fatalf("ParseTraceContext(%q, %q) refused and returned %+v", parent, state, tc)
			}
			return
		}
		if tc.Traceparent() != parent || len(parent) != 55 {
			t.Fatalf("ParseTraceContext(%q) accepted a traceparent it does not give back: %q", parent, tc.Traceparent())
		}
		for _, id := range []string{tc.TraceID, tc.SpanID, tc.Flags} {
			if strings.Trim(id, "0123456789abcdef") != "" {
				t.Fatalf("ParseTraceContext(%q) accepted %q, which is not lowercase hex", parent, id)
			}
		}
		if len(tc.TraceID) != 32 || len(tc.SpanID) != 16 || len(tc.Flags) != 2 ||
			strings.Trim(tc.TraceID, "0") == "" || strings.Trim(tc.SpanID, "0") == "" {
			t.Fatalf("ParseTraceContext(%q) accepted ids %q and %q", parent, tc.TraceID, tc.SpanID)
		}
		if tc.State != "" && tc.State != state {
			t.Fatalf("ParseTraceContext(%q, %q) changed the state to %q", parent, state, tc.State)
		}
		if len(tc.State) > MaxTracestate {
			t.Fatalf("ParseTraceContext accepted a state of %d bytes", len(tc.State))
		}
		for i := range len(tc.State) {
			if c := tc.State[i]; c < 0x20 || c > 0x7e {
				t.Fatalf("ParseTraceContext(%q, %q) accepted a state with byte %#x", parent, state, c)
			}
		}
		if again, ok := ParseTraceContext(tc.Traceparent(), tc.State); !ok || again != tc {
			t.Fatalf("ParseTraceContext does not parse its own result back: %+v, %t", again, ok)
		}
	})
}
