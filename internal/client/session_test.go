package client

import (
	"testing"

	"github.com/ariesworx/starfix/internal/proto"
)

func TestSessionFromEnv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"none", nil, ""},
		{"claude code", map[string]string{"CLAUDE_CODE_SESSION_ID": "cc-1"}, "cc-1"},
		{"older claude spelling", map[string]string{"CLAUDE_SESSION_ID": "c-1"}, "c-1"},
		{"claude code over older", map[string]string{"CLAUDE_SESSION_ID": "c-1", "CLAUDE_CODE_SESSION_ID": "cc-1"}, "cc-1"},
		{"starfix wins", map[string]string{"STARFIX_SESSION": "sf-1", "CLAUDE_CODE_SESSION_ID": "cc-1"}, "sf-1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SessionFromEnv(func(k string) string { return tc.env[k] }); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHarnessFromEnv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"none", nil, ""},
		{"claude code", map[string]string{"CLAUDECODE": "1"}, "claude-code"},
		{"gemini cli", map[string]string{"GEMINI_CLI": "1"}, "gemini"},
		{"only the value 1 counts", map[string]string{"CLAUDECODE": "0", "GEMINI_CLI": "yes"}, ""},
		{"claude code over gemini", map[string]string{"GEMINI_CLI": "1", "CLAUDECODE": "1"}, "claude-code"},
		{"starfix wins", map[string]string{"STARFIX_HARNESS": "codex", "CLAUDECODE": "1"}, "codex"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := HarnessFromEnv(func(k string) string { return tc.env[k] }); got != tc.want {
				t.Errorf("HarnessFromEnv(%v) = %q, want %q", tc.env, got, tc.want)
			}
		})
	}
}

func TestNewSessionID(t *testing.T) {
	t.Parallel()
	a, b := NewSessionID(), NewSessionID()
	if len(a) != 18 || a[:2] != "m-" || a == b {
		t.Fatalf("NewSessionID() = %q, %q; want two distinct m-<16 hex>", a, b)
	}
}

func TestTraceFromEnv(t *testing.T) {
	t.Parallel()
	const parent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	valid := proto.TraceContext{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7", Flags: "01"}
	withState := valid
	withState.State = "vendor=opaque"
	tests := []struct {
		name string
		env  map[string]string
		want proto.TraceContext
	}{
		{"none", nil, proto.TraceContext{}},
		{"a traceparent", map[string]string{"TRACEPARENT": parent}, valid},
		{"with a tracestate", map[string]string{"TRACEPARENT": parent, "TRACESTATE": "vendor=opaque"}, withState},
		{"a tracestate alone is nothing", map[string]string{"TRACESTATE": "vendor=opaque"}, proto.TraceContext{}},
		{"a bad tracestate is dropped", map[string]string{"TRACEPARENT": parent, "TRACESTATE": "two\nlines"}, valid},
		{"an invalid traceparent is not sent", map[string]string{"TRACEPARENT": "00-xyz", "TRACESTATE": "vendor=opaque"}, proto.TraceContext{}},
		{"uppercase is not valid", map[string]string{"TRACEPARENT": "00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01"}, proto.TraceContext{}},
		{"lowercase names are not read", map[string]string{"traceparent": parent}, proto.TraceContext{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := TraceFromEnv(func(k string) string { return tc.env[k] }); got != tc.want {
				t.Errorf("TraceFromEnv(%v) = %+v, want %+v", tc.env, got, tc.want)
			}
		})
	}
}
