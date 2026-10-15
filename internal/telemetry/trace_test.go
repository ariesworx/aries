package telemetry

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/ariesworx/aries/internal/proto"
)

// An invented trace context, as a client sends one.
const (
	remoteTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	remoteSpan  = "00f067aa0ba902b7"
)

func remote(flags, state string) proto.TraceContext {
	return proto.TraceContext{TraceID: remoteTrace, SpanID: remoteSpan, Flags: flags, State: state}
}

// traced makes a Telemetry whose spans a test reads in process.
func traced(t *testing.T, cfg Config) (*Telemetry, *Manual) {
	t.Helper()
	if cfg.Ops == nil {
		cfg.Ops = testOps
	}
	tel, m, err := NewManual(cfg)
	if err != nil {
		t.Fatalf("NewManual: %v", err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	return tel, m
}

// one returns the only span named name, failing the test otherwise.
func one(t *testing.T, spans []SpanData, name string) SpanData {
	t.Helper()
	var found []SpanData
	for _, s := range spans {
		if s.Name == name {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d spans named %q, want 1, among:\n%s", len(found), name, SpanList(spans))
	}
	return found[0]
}

// A request's span is a server span named for its op, a child of the
// caller's span when the request brought a trace context, and the spans
// started under it are its children, in the same trace.
func TestSpanTree(t *testing.T) {
	t.Parallel()
	tel, m := traced(t, Config{})
	if !tel.Tracing() {
		t.Fatalf("Tracing() = false for a Telemetry made to trace")
	}
	ctx, req := tel.StartRequest(t.Context(), "create", remote("01", "vendor=opaque"))
	actx, attempt := tel.Start(ctx, SpanWriteAttempt)
	_, wait := tel.Start(actx, SpanWriteWait)
	wait.End()
	attempt.SetInt(KeyAttempt, 1)
	attempt.Set(KeyOutcome, OutcomeOK)
	attempt.End()
	req.Set(KeyResult, ResultOK)
	req.Set(KeyProject, projA)
	req.SetInt(KeyProtocol, 5)
	if got := len(m.Spans()); got != 2 {
		t.Errorf("%d spans ended before the request's span ends, want 2", got)
	}
	req.End()

	spans := m.Spans()
	if got := SpanNames(spans); !slices.Equal(got, []string{"store.write.wait", "store.write.attempt", "create"}) {
		t.Fatalf("spans, in the order they ended = %v, want the wait, the attempt, then the request", got)
	}
	r, a, w := one(t, spans, "create"), one(t, spans, "store.write.attempt"), one(t, spans, "store.write.wait")
	if r.Kind != "server" || a.Kind != "internal" || w.Kind != "internal" {
		t.Errorf("kinds = %s, %s, %s; want server, internal, internal", r.Kind, a.Kind, w.Kind)
	}
	if r.TraceID != remoteTrace || r.ParentID != remoteSpan || !r.RemoteParent {
		t.Errorf("the request's span is in trace %s under %s (remote %t); want the caller's trace %s and span %s, remote",
			r.TraceID, r.ParentID, r.RemoteParent, remoteTrace, remoteSpan)
	}
	if a.ParentID != r.SpanID || a.RemoteParent || a.TraceID != remoteTrace {
		t.Errorf("the attempt's parent is %s in trace %s, want the request's span %s in %s", a.ParentID, a.TraceID, r.SpanID, remoteTrace)
	}
	if w.ParentID != a.SpanID || w.TraceID != remoteTrace {
		t.Errorf("the wait's parent is %s, want the attempt's span %s", w.ParentID, a.SpanID)
	}
	if want := map[string]string{"op": "create", "result": "ok", "project": projA, "protocol": "5"}; !maps.Equal(r.Attrs, want) {
		t.Errorf("the request's attributes = %v, want %v", r.Attrs, want)
	}
	if want := map[string]string{"attempt": "1", "outcome": "ok"}; !maps.Equal(a.Attrs, want) {
		t.Errorf("the attempt's attributes = %v, want %v", a.Attrs, want)
	}
	if r.Status != "" || a.Status != "" {
		t.Errorf("statuses = %q, %q; want none set for work that succeeded", r.Status, a.Status)
	}
	if r.End.Before(r.Start) || a.Start.Before(r.Start) {
		t.Errorf("times out of order: request %s to %s, attempt from %s", r.Start, r.End, a.Start)
	}
	if id, sp, ok := req.IDs(); !ok || id != remoteTrace || sp != r.SpanID {
		t.Errorf("IDs() = %s, %s, %t; want the trace %s and the span %s", id, sp, ok, remoteTrace, r.SpanID)
	}
	if id, sp, ok := IDs(actx); !ok || id != remoteTrace || sp != a.SpanID {
		t.Errorf("IDs(the attempt's context) = %s, %s, %t; want %s, %s", id, sp, ok, remoteTrace, a.SpanID)
	}
	if _, _, ok := IDs(t.Context()); ok {
		t.Errorf("IDs of a context with no span reports one")
	}
}

// A request with no trace context, or one that is not valid, starts a
// trace of its own. Nothing of an invalid one is kept.
func TestSpanWithoutAParent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		parent proto.TraceContext
	}{
		{"none", proto.TraceContext{}},
		{"a trace id that is not hex", proto.TraceContext{TraceID: "EVIL" + remoteTrace[4:], SpanID: remoteSpan, Flags: "01"}},
		{"a short span id", proto.TraceContext{TraceID: remoteTrace, SpanID: "EVIL", Flags: "01"}},
		{"all zeros", proto.TraceContext{TraceID: strings.Repeat("0", 32), SpanID: remoteSpan, Flags: "01"}},
		{"flags that are not hex", proto.TraceContext{TraceID: remoteTrace, SpanID: remoteSpan, Flags: "zz"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tel, m := traced(t, Config{})
			_, sp := tel.StartRequest(t.Context(), "show", tc.parent)
			sp.End()
			got := one(t, m.Spans(), "show")
			if got.ParentID != "" || got.RemoteParent || got.TraceID == remoteTrace || len(got.TraceID) != 32 {
				t.Errorf("span = trace %q, parent %q (remote %t); want a root span in a trace of its own", got.TraceID, got.ParentID, got.RemoteParent)
			}
		})
	}
	// A tracestate is the caller's text, and no span takes it: the
	// parent is kept and the state left behind, whether or not it is
	// one the SDK would accept.
	for _, state := range []string{"vendor=opaque-STATE", "not a tracestate"} {
		tel, m := traced(t, Config{})
		ctx, sp := tel.StartRequest(t.Context(), "show", remote("01", state))
		_, child := tel.Start(ctx, SpanWriteAttempt)
		child.End()
		sp.End()
		for _, got := range m.Spans() {
			if got.TraceState != "" {
				t.Errorf("tracestate %q: span %s carries the tracestate %q, want none", state, got.Name, got.TraceState)
			}
		}
		if got := one(t, m.Spans(), "show"); got.ParentID != remoteSpan {
			t.Errorf("tracestate %q: the parent is %q, want %s", state, got.ParentID, remoteSpan)
		}
	}
}

// spanKeys is every attribute key a span may have. A new key is added
// here only with a rule for its values: nothing a span carries may be
// text a person or an agent wrote (design §16.4).
var spanKeys = []string{"op", "result", "project", "protocol", "principal", "session", "attempt", "outcome", "migrated", "changed"}

// spanNames is every name a span may have that is not an op.
var spanNames = []string{"unknown", "handshake", "store.open", "store.migrate", "store.write.attempt", "store.write.wait",
	"reaper", "access.sweep", "keys_file.write", "control.reload", "clock.check"}

// Whatever a span is given, its name is an op the server knows or one of
// the fixed names, its keys are from the fixed set, and no value is a
// string the caller chose freely.
func TestSpanAttributesAreVetted(t *testing.T) {
	t.Parallel()
	if got := SpanKeys(); !slices.Equal(got, spanKeys) {
		t.Fatalf("SpanKeys() = %v, want %v: list a new key here, with the rule for its values", got, spanKeys)
	}
	if got := Spans(); !slices.Equal(got, spanNames[1:]) {
		t.Fatalf("Spans() = %v, want %v", got, spanNames[1:])
	}
	tel, m := traced(t, Config{Principals: PrincipalsOmit})
	values := append([]string{projA, "show", ResultOK, "true", "retry", "7"}, hostile...)
	for _, op := range append([]string{"show", "create"}, hostile...) {
		ctx, sp := tel.StartRequest(t.Context(), op, proto.TraceContext{})
		for _, k := range append([]string{"title", "body", "", "error", "exception.message"}, spanKeys...) {
			for _, v := range values {
				sp.Set(k, v)
			}
			sp.SetInt(k, 3)
			sp.SetInt(k, -1)
			sp.SetInt(k, 1<<40)
		}
		for _, v := range values {
			sp.Fail(v)
			sp.SetWho(v, v)
			_, child := tel.Start(ctx, SpanName(v))
			child.Fail(v)
			child.End()
		}
		sp.End()
	}
	spans := m.Spans()
	if len(spans) == 0 {
		t.Fatal("no span was recorded")
	}
	allowedNames := append(slices.Clone(testOps), spanNames...)
	for _, s := range spans {
		if !slices.Contains(allowedNames, s.Name) {
			t.Errorf("a span is named %q, which is neither an op nor a fixed name", s.Name)
		}
		for k, v := range s.Attrs {
			if !slices.Contains(spanKeys, k) {
				t.Errorf("span %s has the attribute key %q, which is not one of %v", s.Name, k, spanKeys)
			}
			if slices.Contains(hostile, v) {
				t.Errorf("span %s has %s=%q, a value no rule allows", s.Name, k, v)
			}
		}
		if slices.Contains(hostile, s.StatusNote) && s.StatusNote != "" {
			t.Errorf("span %s has the status description %q, a value no rule allows", s.Name, s.StatusNote)
		}
		if _, ok := s.Attrs["principal"]; ok {
			t.Errorf("span %s names a principal with telemetry.principals omit", s.Name)
		}
		if s.Events != 0 {
			t.Errorf("span %s has %d events, want none: an event's text is not vetted", s.Name, s.Events)
		}
	}
	// What is allowed is kept.
	_, sp := tel.StartRequest(t.Context(), "show", proto.TraceContext{})
	sp.Set(KeyResult, "not_found")
	sp.Set(KeyProject, projA)
	sp.Set(KeyMigrated, "true")
	sp.Set(KeyChanged, "false")
	sp.Set(KeyOutcome, OutcomeRetry)
	sp.SetInt(KeyAttempt, 4)
	sp.Fail("unavailable")
	sp.End()
	got := m.Spans()[len(m.Spans())-1]
	want := map[string]string{"op": "show", "result": "not_found", "project": projA, "migrated": "true", "changed": "false", "outcome": "retry", "attempt": "4"}
	if !maps.Equal(got.Attrs, want) {
		t.Errorf("attributes = %v, want %v", got.Attrs, want)
	}
	if got.Status != "error" || got.StatusNote != "unavailable" {
		t.Errorf("status = %q %q, want error unavailable", got.Status, got.StatusNote)
	}
}

// The sampler is the SDK's, chosen by the standard variables. An
// unsampled span records nothing and still has ids, for the log.
func TestSampling(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		env     env
		parent  proto.TraceContext
		sampled bool
	}{
		{"the default samples a root", env{}, proto.TraceContext{}, true},
		{"the default follows a sampled parent", env{}, remote("01", ""), true},
		{"the default follows an unsampled parent", env{}, remote("00", ""), false},
		{"always_on ignores the parent", env{"OTEL_TRACES_SAMPLER": "always_on"}, remote("00", ""), true},
		{"always_off", env{"OTEL_TRACES_SAMPLER": "always_off"}, remote("01", ""), false},
		{"parentbased_always_off samples no root", env{"OTEL_TRACES_SAMPLER": "parentbased_always_off"}, proto.TraceContext{}, false},
		{"parentbased_always_off follows a sampled parent", env{"OTEL_TRACES_SAMPLER": "parentbased_always_off"}, remote("01", ""), true},
		{"a ratio of 0", env{"OTEL_TRACES_SAMPLER": "traceidratio", "OTEL_TRACES_SAMPLER_ARG": "0"}, proto.TraceContext{}, false},
		{"a ratio of 1", env{"OTEL_TRACES_SAMPLER": "parentbased_traceidratio", "OTEL_TRACES_SAMPLER_ARG": "1.0"}, proto.TraceContext{}, true},
		{"a ratio with no argument is 1", env{"OTEL_TRACES_SAMPLER": "traceidratio"}, proto.TraceContext{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tel, m := traced(t, Config{Getenv: tc.env.get})
			ctx, sp := tel.StartRequest(t.Context(), "show", tc.parent)
			sp.Set(KeyResult, ResultOK)
			_, child := tel.Start(ctx, SpanWriteAttempt)
			if sp.Recording() != tc.sampled || child.Recording() != tc.sampled {
				t.Errorf("Recording() = %t and %t for the child, want %t", sp.Recording(), child.Recording(), tc.sampled)
			}
			child.End()
			sp.End()
			if got := len(m.Spans()); (got == 2) != tc.sampled || (got != 0 && !tc.sampled) {
				t.Errorf("%d spans recorded, want %t", got, tc.sampled)
			}
			id, span, ok := sp.IDs()
			if !ok || len(id) != 32 || len(span) != 16 {
				t.Errorf("IDs() = %q, %q, %t; want ids, sampled or not", id, span, ok)
			}
			if tc.parent.TraceID != "" && id != remoteTrace {
				t.Errorf("the span is in trace %s, want the caller's, %s", id, remoteTrace)
			}
		})
	}
}

func TestReadEnvTraces(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		env      env
		traces   string
		metrics  string
		headers  map[string]string
		sampler  string
		ratio    float64
		queue    int
		batch    int
		delay    time.Duration
		export   time.Duration
		wantErr  string
		noSecret string
	}{
		{name: "empty"},
		{name: "a collector on the machine takes both signals", env: env{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318"},
			traces: "http://127.0.0.1:4318/v1/traces", metrics: "http://127.0.0.1:4318/v1/metrics"},
		{name: "the traces endpoint is used as it is", env: env{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318",
			"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "https://traces.example.com/api/push"},
			traces: "https://traces.example.com/api/push", metrics: "http://127.0.0.1:4318/v1/metrics"},
		{name: "traces alone", env: env{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://127.0.0.1:4318/v1/traces"},
			traces: "http://127.0.0.1:4318/v1/traces"},
		{name: "metrics alone", env: env{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://127.0.0.1:4318/v1/metrics"},
			metrics: "http://127.0.0.1:4318/v1/metrics"},
		{name: "OTEL_TRACES_EXPORTER=none", env: env{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318", "OTEL_TRACES_EXPORTER": "none"},
			metrics: "http://127.0.0.1:4318/v1/metrics"},
		{name: "OTEL_METRICS_EXPORTER=none leaves traces on", env: env{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318", "OTEL_METRICS_EXPORTER": "none"},
			traces: "http://127.0.0.1:4318/v1/traces"},
		{name: "headers, the signal's winning", env: env{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318",
			"OTEL_EXPORTER_OTLP_HEADERS": "a=1,b=2", "OTEL_EXPORTER_OTLP_TRACES_HEADERS": "a=3", "OTEL_EXPORTER_OTLP_METRICS_HEADERS": "a=4"},
			traces: "http://127.0.0.1:4318/v1/traces", metrics: "http://127.0.0.1:4318/v1/metrics", headers: map[string]string{"a": "3", "b": "2"}},
		{name: "a sampler", env: env{"OTEL_TRACES_SAMPLER": "parentbased_traceidratio", "OTEL_TRACES_SAMPLER_ARG": "0.25"},
			sampler: "parentbased_traceidratio", ratio: 0.25},
		{name: "the batching", env: env{"OTEL_BSP_MAX_QUEUE_SIZE": "100", "OTEL_BSP_MAX_EXPORT_BATCH_SIZE": "10",
			"OTEL_BSP_SCHEDULE_DELAY": "250", "OTEL_BSP_EXPORT_TIMEOUT": "1500"},
			queue: 100, batch: 10, delay: 250 * time.Millisecond, export: 1500 * time.Millisecond},
		{name: "a batch larger than a small queue is the queue", env: env{"OTEL_BSP_MAX_QUEUE_SIZE": "100"}, queue: 100, batch: 100},
		{name: "an exporter ariesd does not have", env: env{"OTEL_TRACES_EXPORTER": "zipkin"}, wantErr: "otlp or none"},
		{name: "a sampler ariesd does not have", env: env{"OTEL_TRACES_SAMPLER": "jaeger_remote"}, wantErr: "OTEL_TRACES_SAMPLER"},
		{name: "a ratio that is not a number", env: env{"OTEL_TRACES_SAMPLER": "traceidratio", "OTEL_TRACES_SAMPLER_ARG": "half"}, wantErr: "0 to 1"},
		{name: "a ratio past 1", env: env{"OTEL_TRACES_SAMPLER": "traceidratio", "OTEL_TRACES_SAMPLER_ARG": "1.5"}, wantErr: "0 to 1"},
		{name: "grpc for traces", env: env{"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "grpc", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4317"},
			wantErr: "OTLP over HTTP only"},
		{name: "a traces endpoint that is not a URL", env: env{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "user:" + token + "@collector"},
			wantErr: "not an http or https URL", noSecret: token},
		{name: "a queue with no bound", env: env{"OTEL_BSP_MAX_QUEUE_SIZE": "10000000"}, wantErr: "OTEL_BSP_MAX_QUEUE_SIZE"},
		{name: "a queue of nothing", env: env{"OTEL_BSP_MAX_QUEUE_SIZE": "0"}, wantErr: "OTEL_BSP_MAX_QUEUE_SIZE"},
		{name: "a batch larger than the queue", env: env{"OTEL_BSP_MAX_QUEUE_SIZE": "100", "OTEL_BSP_MAX_EXPORT_BATCH_SIZE": "200"},
			wantErr: "OTEL_BSP_MAX_EXPORT_BATCH_SIZE"},
		{name: "a bad delay", env: env{"OTEL_BSP_SCHEDULE_DELAY": "5s"}, wantErr: "milliseconds"},
		{name: "bad trace headers", env: env{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318", "OTEL_EXPORTER_OTLP_TRACES_HEADERS": "Authorization " + token},
			wantErr: "name=value", noSecret: token},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := readEnv(tc.env.get)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "fix: ") {
					t.Fatalf("readEnv = %v, want an error containing %q and a fix", err, tc.wantErr)
				}
				if tc.noSecret != "" && strings.Contains(err.Error(), tc.noSecret) {
					t.Errorf("the refusal repeats a secret: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("readEnv = %v", err)
			}
			if c.traces.endpoint != tc.traces || c.endpoint != tc.metrics {
				t.Errorf("endpoints: traces %q, metrics %q; want %q, %q", c.traces.endpoint, c.endpoint, tc.traces, tc.metrics)
			}
			if tc.headers != nil && !maps.Equal(c.traces.headers, tc.headers) {
				t.Errorf("trace headers = %v, want %v", c.traces.headers, tc.headers)
			}
			if want := cmp(tc.sampler, samplerDefault); c.traces.sampler != want {
				t.Errorf("sampler = %q, want %q", c.traces.sampler, want)
			}
			if tc.sampler != "" && c.traces.ratio != tc.ratio {
				t.Errorf("ratio = %v, want %v", c.traces.ratio, tc.ratio)
			}
			if want := cmp(tc.queue, defaultQueue); c.traces.queue != want {
				t.Errorf("queue = %d, want %d", c.traces.queue, want)
			}
			if want := cmp(tc.batch, defaultBatch); c.traces.batch != want {
				t.Errorf("batch = %d, want %d", c.traces.batch, want)
			}
			if want := cmp(tc.delay, defaultBatchDelay); c.traces.delay != want {
				t.Errorf("delay = %s, want %s", c.traces.delay, want)
			}
			if want := cmp(tc.export, defaultExportTime); c.traces.exportTimeout != want {
				t.Errorf("export timeout = %s, want %s", c.traces.exportTimeout, want)
			}
		})
	}
}

func cmp[T comparable](a, b T) T {
	var zero T
	if a != zero {
		return a
	}
	return b
}

// Traces are pushed over OTLP/HTTP to /v1/traces under the endpoint,
// with the headers, carrying the same resource as metrics; a stop
// flushes what is pending.
func TestTracePush(t *testing.T) {
	// Serial: counts the process's goroutines (noLeak).
	noLeak(t)
	col := newCollector(t, http.StatusOK, nil)
	log := &syncBuffer{}
	tel, err := New(t.Context(), Config{Version: "v1", Logger: logTo(log), Ops: testOps, KeyFile: filepath.Join(t.TempDir(), "principals.key"),
		Getenv: env{
			"OTEL_EXPORTER_OTLP_ENDPOINT": col.URL, "OTEL_EXPORTER_OTLP_HEADERS": "x-test=one%20two",
			"OTEL_METRICS_EXPORTER": "none", "OTEL_BSP_SCHEDULE_DELAY": "60000",
		}.get})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !tel.Tracing() || tel.Enabled() {
		t.Fatalf("Tracing() = %t, Enabled() = %t; want traces on and metrics off", tel.Tracing(), tel.Enabled())
	}
	ctx, sp := tel.StartRequest(t.Context(), "create", proto.TraceContext{})
	sp.SetWho("alice", "s-abcdefgh")
	_, child := tel.Start(ctx, SpanWriteAttempt)
	child.End()
	sp.End()
	if col.count() != 0 {
		t.Errorf("an export was made before the batch's delay or a stop")
	}
	sctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := tel.Shutdown(sctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	if col.count() != 1 {
		t.Fatalf("%d exports reached the collector after the stop, want the 1 that flushes", col.count())
	}
	col.mu.Lock()
	body, hdr, path := col.bodies[0], col.headers[0], col.paths[0]
	col.mu.Unlock()
	if path != "/v1/traces" {
		t.Errorf("export posted to %q, want /v1/traces", path)
	}
	if got := hdr.Get("x-test"); got != "one two" {
		t.Errorf("export header x-test = %q, want %q", got, "one two")
	}
	for _, s := range []string{"create", "store.write.attempt", "service.name", "ariesd", "service.instance.id", "principal", "p_"} {
		if !bytes.Contains(body, []byte(s)) {
			t.Errorf("the export does not carry %q", s)
		}
	}
	for _, s := range []string{"alice", "s-abcdefgh"} {
		if bytes.Contains(body, []byte(s)) {
			t.Errorf("the export carries %q with telemetry.principals at its default, pseudonym", s)
		}
	}
	out := log.String()
	if !strings.Contains(out, "telemetry on") || !strings.Contains(out, "traces="+col.URL+"/v1/traces") || !strings.Contains(out, "sampler=parentbased_always_on") {
		t.Errorf("the start-up line does not say where traces go and how they are sampled:\n%s", out)
	}
}

// With nothing configured for traces there is no tracer, no goroutine
// and no key file, and a span costs nothing.
func TestTracesOffByDefault(t *testing.T) {
	// Serial: counts the process's goroutines (noLeak).
	noLeak(t)
	for _, tc := range []struct {
		name string
		env  env
	}{
		{"nothing configured", env{}},
		{"OTEL_SDK_DISABLED", env{"OTEL_SDK_DISABLED": "true", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:1"}},
		{"OTEL_TRACES_EXPORTER=none", env{"OTEL_TRACES_EXPORTER": "none", "OTEL_METRICS_EXPORTER": "none", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:1"}},
		{"a sampler and no endpoint", env{"OTEL_TRACES_SAMPLER": "always_on"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := filepath.Join(t.TempDir(), "telemetry", "principals.key")
			before := runtime.NumGoroutine()
			tel, err := New(t.Context(), Config{Getenv: tc.env.get, KeyFile: key})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if tel.Tracing() {
				t.Errorf("Tracing() = true")
			}
			if n := runtime.NumGoroutine(); n > before {
				t.Errorf("%d goroutines after New, %d before: want none started", n, before)
			}
			ctx, sp := tel.StartRequest(t.Context(), "show", remote("01", ""))
			if ctx != t.Context() {
				t.Errorf("StartRequest changed the context with traces off")
			}
			sp.Set(KeyResult, ResultOK)
			sp.SetWho("alice", "s-1")
			sp.Fail("unavailable")
			if _, _, ok := sp.IDs(); ok || sp.Recording() {
				t.Errorf("a span with traces off has ids, or records")
			}
			sp.End()
			if cctx, child := tel.Start(ctx, SpanReaper); cctx != ctx || child.Recording() {
				t.Errorf("Start changed the context, or records, with traces off")
			}
			if _, ok := tel.StandIn("alice"); ok {
				t.Errorf("StandIn gave a stand-in with no key")
			}
			if _, err := os.Stat(filepath.Dir(key)); !os.IsNotExist(err) {
				t.Errorf("the key file's directory exists with traces off (stat: %v): nothing may be written", err)
			}
			if allocs := testing.AllocsPerRun(100, func() {
				c, s := tel.StartRequest(t.Context(), "show", proto.TraceContext{})
				_, w := tel.Start(c, SpanWriteAttempt)
				w.End()
				s.Set(KeyResult, ResultOK)
				s.End()
			}); allocs != 0 {
				t.Errorf("a request's spans allocate %v times with traces off, want 0", allocs)
			}
			if err := tel.Shutdown(t.Context()); err != nil {
				t.Errorf("Shutdown: %v", err)
			}
		})
	}
	var none *Telemetry
	if _, sp := none.StartRequest(t.Context(), "show", proto.TraceContext{}); sp.Recording() || none.Tracing() {
		t.Errorf("a nil Telemetry traces")
	}
}

// A header's value may be a token: no line about traces may hold it,
// even when the backend's answer repeats it.
func TestTraceTokenIsNeverLogged(t *testing.T) {
	// Serial: counts the process's goroutines (noLeak).
	noLeak(t)
	col := newCollector(t, http.StatusUnauthorized, func(r *http.Request) string {
		return "bad credentials: " + r.Header.Get("Authorization") + " for " + r.URL.String()
	})
	log := &syncBuffer{}
	u := strings.Replace(col.URL, "http://", "http://user:pw-"+token+"@", 1) + "/v1/traces?api_key=q-" + token
	tel, err := New(t.Context(), Config{Version: "v1", Logger: logTo(log), Pull: true, Ops: testOps, Getenv: env{
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": u,
		"OTEL_EXPORTER_OTLP_TRACES_HEADERS":  "Authorization=" + token + ",x-scope=team",
		"OTEL_BSP_SCHEDULE_DELAY":            "20",
		"OTEL_BSP_EXPORT_TIMEOUT":            "2000",
	}.get})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for range 3 {
		_, sp := tel.StartRequest(t.Context(), "show", proto.TraceContext{})
		sp.End()
		col.wait(t)
	}
	col.mu.Lock()
	sent := col.headers[0].Get("Authorization")
	col.mu.Unlock()
	if sent != token {
		t.Fatalf("the collector received Authorization %q, want the token", sent)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(log.String(), "trace export failed") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_ = tel.stopTraces(ctx)
	// The spans a refusing collector lost are counted where a scrape
	// sees them.
	if text := promText(t, tel); !strings.Contains(text, `aries_telemetry_dropped_total{signal="traces"} 3`) {
		t.Errorf("/metrics does not count the 3 spans lost:\n%s", grep(text, "dropped"))
	}
	out := log.String()
	if !strings.Contains(out, "telemetry on") || !strings.Contains(out, "trace export failed") {
		t.Errorf("want a start-up line and a trace export failure in the log:\n%s", out)
	}
	if strings.Contains(out, token) || strings.Contains(out, "s3cr3t") {
		t.Errorf("the log holds the token:\n%s", out)
	}
	if n := strings.Count(out, "trace export failed"); n != 1 {
		t.Errorf("%d trace export failures logged for a collector that keeps refusing, want 1 until %s has passed:\n%s", n, errEvery, out)
	}
}

func grep(text, sub string) string {
	var b strings.Builder
	for line := range strings.Lines(text) {
		if strings.Contains(line, sub) {
			b.WriteString(line)
		}
	}
	return b.String()
}

// A collector that is down, and one that accepts and never answers, hold
// neither a span nor a stop; the queue is bounded, and what it could not
// take is counted.
func TestDeadCollectorTraces(t *testing.T) {
	// Serial: counts the process's goroutines (noLeak).
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	_ = closed.Close()
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = silent.Close() })

	const spans = 20000
	for _, tc := range []struct{ name, addr string }{{"closed port", closedAddr}, {"silent listener", silent.Addr().String()}} {
		t.Run(tc.name, func(t *testing.T) {
			noLeak(t)
			log := &syncBuffer{}
			tel, err := New(t.Context(), Config{Logger: logTo(log), Ops: testOps, Pull: true, Getenv: env{
				"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://" + tc.addr + "/v1/traces",
				"OTEL_BSP_SCHEDULE_DELAY":            "20", "OTEL_BSP_EXPORT_TIMEOUT": "150", "OTEL_EXPORTER_OTLP_TIMEOUT": "150",
				"OTEL_BSP_MAX_QUEUE_SIZE": "256", "OTEL_BSP_MAX_EXPORT_BATCH_SIZE": "64",
			}.get})
			if err != nil {
				t.Fatal(err)
			}
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			start := time.Now()
			for range spans / 2 {
				ctx, sp := tel.StartRequest(t.Context(), "show", proto.TraceContext{})
				sp.Set(KeyResult, ResultOK)
				_, child := tel.Start(ctx, SpanWriteAttempt)
				child.End()
				sp.End()
			}
			// Spans that waited for the export of their batch would wait
			// its timeout, 150 ms, once for each batch of 64: about 47
			// seconds over these. Half of that is the bound, which the
			// spans themselves come nowhere near on a slow machine.
			const waited = spans / 64 * 150 * time.Millisecond
			if d := time.Since(start); d > waited/2 {
				t.Errorf("%d spans took %s with the collector dead, want them not to wait for it: waiting for each batch's export would take %s", spans, d, waited)
			}
			runtime.GC()
			runtime.ReadMemStats(&after)
			if after.HeapAlloc > before.HeapAlloc+32<<20 {
				t.Errorf("the heap grew from %d to %d MiB over %d spans with the collector dead: the queue is not bounded",
					before.HeapAlloc>>20, after.HeapAlloc>>20, spans)
			}
			t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			start = time.Now()
			_ = tel.stopTraces(ctx)
			if d := time.Since(start); d > 4*time.Second {
				t.Errorf("stopping the traces took %s with a 1s deadline", d)
			}
			if n := strings.Count(log.String(), "trace export failed"); n > 1 {
				t.Errorf("%d trace export failures logged, want at most 1:\n%s", n, log)
			}
			// Every span was dropped, at the queue or with its export.
			text := promText(t, tel)
			var dropped int
			if _, err := fmt.Sscanf(grep(text, `aries_telemetry_dropped_total{signal="traces"}`), `aries_telemetry_dropped_total{signal="traces"} %d`, &dropped); err != nil {
				t.Fatalf("no count of dropped spans on /metrics: %v\n%s", err, grep(text, "dropped"))
			}
			if dropped != spans {
				t.Errorf("aries.telemetry.dropped{signal=traces} = %d, want every one of the %d spans", dropped, spans)
			}
		})
	}
}

// blocking is a span exporter that holds each export until released, and
// counts the spans it was given.
type blocking struct {
	release chan struct{}
	mu      sync.Mutex
	got     int
	fail    bool
}

func (b *blocking) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	select {
	case <-b.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail {
		return fmt.Errorf("refused")
	}
	b.got += len(spans)
	return nil
}

func (*blocking) Shutdown(context.Context) error { return nil }

// Every sampled span is exported or counted as dropped, exactly: the
// queue's bound is enforced where it can be counted.
func TestDroppedSpansAreCounted(t *testing.T) {
	// Serial: counts the process's goroutines (noLeak).
	for _, tc := range []struct {
		name string
		fail bool
	}{{"a slow exporter", false}, {"a slow exporter that then refuses", true}} {
		t.Run(tc.name, func(t *testing.T) {
			noLeak(t)
			tel, m := traced(t, Config{})
			exp := &blocking{release: make(chan struct{}), fail: tc.fail}
			tel.tp.RegisterSpanProcessor(tel.batcher(exp, traceEnv{queue: 50, batch: 10, delay: time.Millisecond, exportTimeout: 30 * time.Second}))
			const ended = 500
			for range ended {
				_, sp := tel.StartRequest(t.Context(), "show", proto.TraceContext{})
				sp.End()
			}
			// Unsampled spans are not queued, so they are not dropped.
			for range 100 {
				_, sp := tel.StartRequest(t.Context(), "show", remote("00", ""))
				sp.End()
			}
			close(exp.release)
			if err := tel.stopTraces(t.Context()); err != nil {
				t.Fatalf("stopTraces: %v", err)
			}
			dropped := int(collect(t, m).Value("aries.telemetry.dropped", "signal", "traces"))
			if tc.fail {
				if dropped != ended {
					t.Errorf("dropped = %d with an exporter that refuses, want all %d", dropped, ended)
				}
				return
			}
			exp.mu.Lock()
			got := exp.got
			exp.mu.Unlock()
			if got+dropped != ended {
				t.Errorf("%d exported and %d dropped of %d spans ended: every one must be one or the other", got, dropped, ended)
			}
			if dropped < ended-50-10-10 || dropped == 0 {
				t.Errorf("dropped = %d of %d with a queue of 50 and an exporter that was held: the queue is not bounded", dropped, ended)
			}
		})
	}
}

// A stop whose deadline passes while an export hangs cuts the export
// short, however long the export's own timeout is: it returns soon after
// the deadline, counts what it abandoned, and leaves nothing running.
func TestStopCutsShortASlowExport(t *testing.T) {
	// Serial: counts the process's goroutines (noLeak).
	noLeak(t)
	tel, m := traced(t, Config{})
	exp := &blocking{release: make(chan struct{})} // never released
	tel.tp.RegisterSpanProcessor(tel.batcher(exp, traceEnv{queue: 100, batch: 10, delay: time.Millisecond, exportTimeout: time.Minute}))
	const ended = 35
	for range ended {
		_, sp := tel.StartRequest(t.Context(), "show", proto.TraceContext{})
		sp.End()
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := tel.stopTraces(ctx)
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the stop took %s with a deadline of 200ms and an export that never returns", d)
	}
	if err == nil {
		t.Errorf("stopTraces = nil, want it to say the deadline passed")
	}
	if dropped := int(collect(t, m).Value("aries.telemetry.dropped", "signal", "traces")); dropped != ended {
		t.Errorf("dropped = %d, want the %d spans the stop abandoned", dropped, ended)
	}
}

// What a span costs on the request path: nothing with traces off, and
// little when the trace is not sampled.
func BenchmarkRequestSpans(b *testing.B) {
	run := func(b *testing.B, tel *Telemetry, parent proto.TraceContext) {
		b.ReportAllocs()
		ctx := context.Background()
		for b.Loop() {
			rctx, sp := tel.StartRequest(ctx, "create", parent)
			sp.SetWho("alice", "s-abcdefgh")
			actx, attempt := tel.Start(rctx, SpanWriteAttempt)
			_, wait := tel.Start(actx, SpanWriteWait)
			wait.End()
			attempt.SetInt(KeyAttempt, 1)
			attempt.Set(KeyOutcome, OutcomeOK)
			attempt.End()
			sp.Set(KeyResult, ResultOK)
			sp.Set(KeyProject, projA)
			sp.SetInt(KeyProtocol, 5)
			_, _, _ = sp.IDs()
			sp.End()
		}
	}
	b.Run("off", func(b *testing.B) {
		tel, err := New(b.Context(), Config{Ops: testOps})
		if err != nil {
			b.Fatal(err)
		}
		run(b, tel, remote("01", ""))
	})
	for _, bc := range []struct {
		name   string
		parent proto.TraceContext
	}{{"unsampled", remote("00", "")}, {"sampled", remote("01", "")}} {
		b.Run(bc.name, func(b *testing.B) {
			tel, err := New(b.Context(), Config{Ops: testOps})
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
			// Exported to nowhere, through the real batching processor,
			// so that the cost is the request path's and not a test
			// recorder's.
			te := traceEnv{sampler: samplerDefault, ratio: 1, queue: defaultQueue, batch: defaultBatch,
				delay: defaultBatchDelay, exportTimeout: defaultExportTime}
			tel.trace(resource.Empty(), te, tel.batcher(discard{}, te))
			run(b, tel, bc.parent)
		})
	}
}

// TakeSpans hands over the spans that ended since it was last called,
// each once, so that a long run can count its spans without keeping
// them all.
func TestTakeSpans(t *testing.T) {
	t.Parallel()
	tel, m := traced(t, Config{})
	end := func(name SpanName) {
		_, sp := tel.Start(t.Context(), name)
		sp.End()
	}
	end(SpanWriteAttempt)
	end(SpanWriteWait)
	if got := SpanNames(m.TakeSpans()); !slices.Equal(got, []string{"store.write.attempt", "store.write.wait"}) {
		t.Errorf("TakeSpans() after two spans ended = %v, want the attempt and the wait, in the order they ended", got)
	}
	if got := m.Spans(); len(got) != 0 {
		t.Errorf("Spans() after TakeSpans() = %v, want none left", SpanNames(got))
	}
	end(SpanReaper)
	if got := SpanNames(m.TakeSpans()); !slices.Equal(got, []string{"reaper"}) {
		t.Errorf("TakeSpans() after one more span ended = %v, want only the reaper's", got)
	}
	if got := m.TakeSpans(); len(got) != 0 {
		t.Errorf("TakeSpans() with nothing ended since = %v, want none", SpanNames(got))
	}
}

// discard is a span exporter that keeps nothing.
type discard struct{}

func (discard) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return nil }
func (discard) Shutdown(context.Context) error                             { return nil }
