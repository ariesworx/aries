package e2e

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ariesworx/starfix/internal/cli"
	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/server"
	"github.com/ariesworx/starfix/internal/telemetry"
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
	t.Parallel()
	const traceID, spanID = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	logs := &lockedBuffer{}
	w := newWorld(t, daemonOpts{logger: daemonLog(t, logs)})
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

// TestTraceContextPerToolCall runs `sfx mcp` as a harness does, over
// pipes, started with a TRACEPARENT in its environment. A tool call
// whose request carries a trace context in its metadata (_meta's
// traceparent and tracestate) reaches the daemon under that trace; one
// that carries none, or one that is not valid, under the environment's.
// No call is failed by its metadata.
func TestTraceContextPerToolCall(t *testing.T) {
	t.Parallel()
	const (
		envTrace, envSpan   = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
		callTrace, callSpan = "0af7651916cd43dd8448eb211c80319c", "b7ad6b7169203331"
	)
	logs := &lockedBuffer{}
	w := newWorld(t, daemonOpts{logger: daemonLog(t, logs)})
	alice := w.newUser("alice", "")
	alice.env = map[string]string{"TRACEPARENT": "00-" + envTrace + "-" + envSpan + "-01", "TRACESTATE": "vendor=env-STATE"}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan int, 1)
	go func() {
		var errb bytes.Buffer
		done <- cli.Run(context.Background(), []string{"-C", alice.repo, "mcp"}, cli.Env{
			Stdin: inR, Stdout: outW, Stderr: &errb,
			Getenv:   func(k string) string { return alice.env[k] },
			Hostname: func() (string, error) { return "laptop-test", nil },
			Version:  "v0.2.0",
		})
		_ = outW.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "harness"}, nil).Connect(ctx, &mcp.IOTransport{Reader: outR, Writer: inW}, nil)
	if err != nil {
		t.Fatal(err)
	}
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
	// Each case calls a tool of its own, whose one request names it.
	for _, tc := range []struct {
		name, tool string
		meta       mcp.Meta
		trace      string
		span       string
	}{
		{"the call's own trace context", "ready", mcp.Meta{"traceparent": "00-" + callTrace + "-" + callSpan + "-01", "tracestate": "vendor=call-STATE"}, callTrace, callSpan},
		{"a call with none falls back to the environment's", "blocked", nil, envTrace, envSpan},
		{"a call with one that is not valid does too", "who", mcp.Meta{"traceparent": "00-EVIL-" + callSpan + "-01", "tracestate": "EVIL=state"}, envTrace, envSpan},
		{"and so does one whose traceparent is no string", "list", mcp.Meta{"traceparent": map[string]any{"EVIL": callTrace}}, envTrace, envSpan},
	} {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Meta: tc.meta, Name: tc.tool})
		if err != nil || res.IsError {
			t.Fatalf("%s: the %s tool = %v, %+v; a trace context must never fail a call", tc.name, tc.tool, err, res)
		}
		if got := line(tc.tool); !strings.Contains(got, " trace_id="+tc.trace+" span_id="+tc.span) {
			t.Errorf("%s: the daemon's line for %s does not name trace %s and span %s:\n%s", tc.name, tc.tool, tc.trace, tc.span, got)
		}
	}
	_ = cs.Close()
	_ = inW.Close()
	select {
	case code := <-done:
		if code != cli.ExitOK {
			t.Fatalf("sfx mcp exited %d", code)
		}
	case <-ctx.Done():
		t.Fatal("sfx mcp did not exit when its input closed")
	}
	for _, leaked := range []string{"EVIL", "call-STATE", "env-STATE"} {
		if strings.Contains(logs.String(), leaked) {
			t.Errorf("the daemon's log holds %q, from a trace context:\n%s", leaked, logs.String())
		}
	}
}

// TestTracesEndToEnd runs slice O2 through everything: `sfx` with a
// TRACEPARENT in its environment, SSH, the bridge, a daemon that traces,
// and its store. The daemon's span for the request is a child of the
// caller's span, with the store's work beneath it; it names the person
// by a stand-in; and `sfx admin principal whois`, for a server admin,
// says whose the stand-in is.
func TestTracesEndToEnd(t *testing.T) {
	t.Parallel()
	const traceID, spanID = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	tel, spans, err := telemetry.NewManual(telemetry.Config{Version: "v0.2.0", Ops: server.Ops()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	w := newWorld(t, daemonOpts{many: true, admins: []string{"dana"}, telemetry: tel, limits: server.Limits{ControlWriteBurst: 500}})
	dana := w.newUser("dana", "")
	dana.ok("admin", "principal", "add", "alice")
	alice := w.newUser("alice", "")
	alice.env = map[string]string{"TRACEPARENT": "00-" + traceID + "-" + spanID + "-01", "TRACESTATE": "vendor=opaque-STATE"}
	alice.ok("create", "Ship the thing", "--body", "with care")

	var create telemetry.SpanData
	all := spans.Spans()
	for _, sp := range all {
		if sp.Name == proto.OpCreate {
			create = sp
		}
	}
	if create.TraceID != traceID || create.ParentID != spanID || !create.RemoteParent || create.Kind != "server" {
		t.Fatalf("the daemon's span for the create = %+v; want a server span in trace %s under the caller's span %s\n%s",
			create, traceID, spanID, telemetry.SpanList(all))
	}
	var below []string
	for _, sp := range all {
		if sp.TraceID == traceID && sp.SpanID != create.SpanID {
			below = append(below, sp.Name)
		}
	}
	if want := []string{"store.write.wait", "store.write.attempt"}; !slices.Equal(below, want) {
		t.Errorf("spans beneath the create, as they ended = %v, want %v", below, want)
	}
	alias := create.Attrs["principal"]
	if !strings.HasPrefix(alias, "p_") || len(alias) != 18 || create.Attrs["result"] != "ok" || create.Attrs["project"] != project {
		t.Fatalf("the create's attributes = %v; want a stand-in for alice, result ok and the project", create.Attrs)
	}
	for _, sp := range all {
		for k, v := range sp.Attrs {
			if strings.Contains(v, "Ship") || strings.Contains(v, "care") || strings.Contains(v, "alice") || strings.Contains(v, "opaque") {
				t.Errorf("span %s carries %s=%q: text, a name or a tracestate", sp.Name, k, v)
			}
		}
	}

	// A server admin resolves the stand-in; nobody else does.
	if got := dana.ok("admin", "principal", "whois", alias); got != alias+" is alice\n" {
		t.Errorf("sfx admin principal whois = %q, want %q", got, alias+" is alice\n")
	}
	if got := decode[proto.PrincipalWhoisResult](t, dana.ok("admin", "principal", "whois", alias, "--json")); got !=
		(proto.PrincipalWhoisResult{StandIn: alias, Name: "alice", State: "active"}) {
		t.Errorf("whois --json = %+v", got)
	}
	alice.env = nil
	if e := alice.refused(proto.CodeForbidden, "admin", "principal", "whois", alias); !strings.Contains(e.Fix, "server admin") {
		t.Errorf("whois by a member: fix %q, want it to say a server admin can", e.Fix)
	}
	if e := dana.refused(proto.CodeNotFound, "admin", "principal", "whois", "p_0123456789abcdef"); !strings.Contains(e.Message, "stand-in of no principal") {
		t.Errorf("whois of nobody's stand-in: %q", e.Message)
	}
	dana.refused(proto.CodeInvalid, "admin", "principal", "whois", "alice")

	// A daemon that traces nothing has made no stand-in to resolve.
	plain := adminWorld(t, 0, 0)
	if e := plain.newUser("dana", "").refused(proto.CodeUnavailable, "admin", "principal", "whois", alias); !strings.Contains(e.Message, "has no key for stand-ins: it exports no telemetry") {
		t.Errorf("whois on a server with no key: %q", e.Message)
	}
}

// daemonLog is the logger `starfixd serve` makes, at debug, writing to
// w: its handler is what puts a request's id and its trace's ids on a
// line.
func daemonLog(t *testing.T, w io.Writer) *slog.Logger {
	t.Helper()
	log, err := server.NewLogger(w, "debug", "text")
	if err != nil {
		t.Fatal(err)
	}
	return log
}
