package e2e

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// lockedBuffer is a log sink read while the daemon still writes to it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// TestTraceContextFromTheEnvironment runs decision 27 end to end: sfx
// reads TRACEPARENT and TRACESTATE from its environment and sends them on
// each request, and the daemon carries them on the request's context,
// which is where the ids in its log line come from. A TRACEPARENT that is
// not a valid one is not sent, and the command works as without it.
func TestTraceContextFromTheEnvironment(t *testing.T) {
	const traceID, spanID = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	logs := &lockedBuffer{}
	w := newWorld(t, daemonOpts{logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	alice := w.newUser("alice", "")
	line := func(op string) string {
		t.Helper()
		for l := range strings.SplitSeq(logs.String(), "\n") {
			if strings.Contains(l, "msg=request ") && strings.Contains(l, " op="+op+" ") {
				return l
			}
		}
		t.Fatalf("the daemon logged no %s request:\n%s", op, logs.String())
		return ""
	}

	alice.env = map[string]string{"TRACEPARENT": "00-" + traceID + "-" + spanID + "-01", "TRACESTATE": "vendor=opaque-STATE"}
	alice.ok("ready")
	if got := line("ready"); !strings.Contains(got, " trace_id="+traceID+" span_id="+spanID) {
		t.Errorf("with TRACEPARENT set, the daemon's line for the request names no trace:\n%s", got)
	}

	alice.env = map[string]string{"TRACEPARENT": "00-" + strings.ToUpper(traceID) + "-" + spanID + "-01", "TRACESTATE": "vendor=opaque-STATE"}
	alice.ok("blocked")
	if got := line("blocked"); strings.Contains(got, "trace_id") {
		t.Errorf("with a TRACEPARENT that is not valid, the daemon's line names a trace:\n%s", got)
	}

	alice.env = nil
	alice.ok("list")
	if got := line("list"); strings.Contains(got, "trace_id") {
		t.Errorf("with no TRACEPARENT, the daemon's line names a trace:\n%s", got)
	}
	if strings.Contains(logs.String(), "opaque-STATE") {
		t.Errorf("the daemon logged a tracestate:\n%s", logs.String())
	}
}
