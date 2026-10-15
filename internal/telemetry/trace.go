package telemetry

import (
	"context"
	"slices"
	"strconv"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/proto"
)

// Traces (design §16.4). A span is started here and nowhere else, so
// that what a span may carry is decided in this file.
//
// THE RULE: nothing a span carries is text a person or an agent wrote.
// No issue's title or body, no memory's text, no comment, no handoff, no
// key, no DSN, no token, and no error's message, since a message quotes
// what it refused. A span has a name from a fixed list or the list of
// ops, attributes from the fixed keys below with the values each key
// allows (ids, codes and small numbers), and a status whose description
// is a code. It has no events and no links. Anything else given to a
// span is dropped or replaced by Unknown or Other, never recorded as it
// came: a caller cannot widen this by mistake.

// SpanName names a span that is not a request's. A request's span is
// named for its op.
type SpanName string

// The spans that are not a request's.
const (
	// SpanHandshake is a connection's handshake, from its first frame to
	// the welcome.
	SpanHandshake SpanName = "handshake"
	// SpanStoreOpen is the opening of a project's store, and
	// SpanStoreMigrate the check and application of its migrations.
	SpanStoreOpen    SpanName = "store.open"
	SpanStoreMigrate SpanName = "store.migrate"
	// SpanWriteAttempt is one attempt of a write, from the wait for the
	// writer to its commit, and SpanWriteWait that wait. A write that is
	// retried has a span for each attempt.
	SpanWriteAttempt SpanName = "store.write.attempt"
	SpanWriteWait    SpanName = "store.write.wait"
	// SpanReaper is one tick of the reaper, and the next four are parts
	// of it that also run on their own: ending the claims of holders who
	// lost access, writing the keys file, rereading the control store's
	// index, and looking at the clock.
	SpanReaper        SpanName = "reaper"
	SpanSweep         SpanName = "access.sweep"
	SpanKeysFile      SpanName = "keys_file.write"
	SpanControlReload SpanName = "control.reload"
	SpanClockCheck    SpanName = "clock.check"
)

// fixedSpans are the SpanNames, in the order the guide lists them.
var fixedSpans = []SpanName{SpanHandshake, SpanStoreOpen, SpanStoreMigrate, SpanWriteAttempt, SpanWriteWait,
	SpanReaper, SpanSweep, SpanKeysFile, SpanControlReload, SpanClockCheck}

// Spans returns the names of the spans that are not a request's.
func Spans() []string {
	out := make([]string, len(fixedSpans))
	for i, n := range fixedSpans {
		out[i] = string(n)
	}
	return out
}

// A span's attribute keys beyond KeyOp, KeyResult, KeyProject and
// KeyMigrated, which a span shares with the instruments.
const (
	// KeyProtocol is the protocol version a connection speaks.
	KeyProtocol = "protocol"
	// KeyPrincipal and KeySession say who, as telemetry: principals:
	// allows ([Span.SetWho]). No instrument has them.
	KeyPrincipal = "principal"
	KeySession   = "session"
	// KeyAttempt numbers a write's attempts from 1.
	KeyAttempt = "attempt"
	// KeyOutcome is how a part of the store's work ended: one of the
	// Outcome values.
	KeyOutcome = "outcome"
	// KeyChanged says whether the keys file was written: true or false.
	KeyChanged = "changed"
)

// The values of KeyOutcome.
const (
	OutcomeOK    = "ok"
	OutcomeRetry = "retry"
	OutcomeError = "error"
)

var (
	outcomes = []string{OutcomeOK, OutcomeRetry, OutcomeError}
	bools    = []string{"true", "false"}
)

// maxSpanInt bounds a span's numbers: none is ever near it.
const maxSpanInt = 1_000_000

// SpanKeys returns every attribute key a span may have, in the order
// the guide lists them.
func SpanKeys() []string {
	return []string{KeyOp, KeyResult, KeyProject, KeyProtocol, KeyPrincipal, KeySession, KeyAttempt, KeyOutcome, KeyMigrated, KeyChanged}
}

// Span is a unit of work in a trace. The zero Span, which is what a
// Telemetry that traces nothing starts, does nothing and costs nothing.
type Span struct {
	s trace.Span
	t *Telemetry
}

// End ends the span. Ending is memory only: the span waits in a bounded
// queue for an export that happens on another goroutine.
func (s Span) End() {
	if s.s != nil {
		s.s.End()
	}
}

// Recording reports whether the span is sampled: an unsampled span has
// ids and nothing else, and every Set on it returns at once.
func (s Span) Recording() bool { return s.s != nil && s.s.IsRecording() }

// IDs returns the span's trace and span ids, in hex, for a log line: a
// span has them whether or not it is sampled. It reports false for the
// zero Span.
func (s Span) IDs() (traceID, spanID string, ok bool) {
	if s.s == nil {
		return "", "", false
	}
	return ids(s.s.SpanContext())
}

// IDs returns the ids of the span ctx carries, as [Span.IDs] does.
func IDs(ctx context.Context) (traceID, spanID string, ok bool) {
	return ids(trace.SpanContextFromContext(ctx))
}

func ids(sc trace.SpanContext) (traceID, spanID string, ok bool) {
	if !sc.IsValid() {
		return "", "", false
	}
	return sc.TraceID().String(), sc.SpanID().String(), true
}

// Set sets one of the span's attributes that takes a word: KeyOp (an op
// the server knows, or Unknown), KeyResult (ok, failed or a protocol
// code), KeyProject (a UUID), KeyOutcome, KeyMigrated or KeyChanged. A
// value its key does not allow is recorded as Other, and any other key
// is ignored (THE RULE, above).
func (s Span) Set(key, value string) {
	if !s.Recording() {
		return
	}
	switch key {
	case KeyOp:
		value = s.t.op(value)
	case KeyProject:
		if !uuidLike(value) {
			value = Other
		}
	case KeyResult:
		value = oneOf(results, value)
	case KeyOutcome:
		value = oneOf(outcomes, value)
	case KeyMigrated, KeyChanged:
		value = oneOf(bools, value)
	default:
		return
	}
	s.s.SetAttributes(attribute.String(key, value))
}

// SetInt sets one of the span's attributes that takes a small number:
// KeyProtocol or KeyAttempt. Any other key, and a number that is
// negative or absurd, is ignored.
func (s Span) SetInt(key string, n int) {
	if !s.Recording() || n < 0 || n > maxSpanInt || (key != KeyProtocol && key != KeyAttempt) {
		return
	}
	s.s.SetAttributes(attribute.Int(key, n))
}

// SetWho says whose work the span is, as telemetry: principals: allows:
// stand-ins by default, the names themselves, or nothing. A metric never
// says who; a span may (decision 32).
func (s Span) SetWho(principal, session string) {
	if !s.Recording() {
		return
	}
	if p := s.t.who.principal(principal); p != "" {
		s.s.SetAttributes(attribute.String(KeyPrincipal, p))
	}
	if v := s.t.who.session(session); v != "" {
		s.s.SetAttributes(attribute.String(KeySession, v))
	}
}

// Fail marks the span as failed, with why as its description: a result
// or an outcome, and Other for anything else. It takes no error: an
// error's message quotes what it refused (THE RULE, above).
func (s Span) Fail(why string) {
	if !s.Recording() {
		return
	}
	if !slices.Contains(outcomes, why) {
		why = oneOf(results, why)
	}
	s.s.SetStatus(codes.Error, why)
}

// oneOf returns v if list holds it, and Other if not.
func oneOf(list []string, v string) string {
	if slices.Contains(list, v) {
		return v
	}
	return Other
}

// op returns op if the server knows it, and Unknown if not: an op is a
// string the client sent.
func (t *Telemetry) op(op string) string {
	if _, ok := t.ops[op]; ok {
		return op
	}
	return Unknown
}

// Tracing reports whether t starts spans.
func (t *Telemetry) Tracing() bool { return t != nil && t.tracer != nil }

// StartRequest begins the server span of a request for op, named for
// the op, and returns a context that carries it. Its parent is the
// caller's span when parent is a trace context the request brought
// (decision 27), and it starts a trace of its own when parent is the
// zero value or does not parse: a caller's broken tracing never fails
// its request. An op the server does not know is named Unknown.
func (t *Telemetry) StartRequest(ctx context.Context, op string, parent proto.TraceContext) (context.Context, Span) {
	if t == nil || t.tracer == nil {
		return ctx, Span{}
	}
	if sc, ok := remoteParent(parent); ok {
		ctx = trace.ContextWithRemoteSpanContext(ctx, sc)
	}
	op = t.op(op)
	ctx, s := t.tracer.Start(ctx, op, trace.WithSpanKind(trace.SpanKindServer))
	if s.IsRecording() {
		s.SetAttributes(attribute.String(KeyOp, op))
	}
	return ctx, Span{s: s, t: t}
}

// StartServer begins a server span named name that no request brought:
// a handshake. It starts a trace of its own.
func (t *Telemetry) StartServer(ctx context.Context, name SpanName) (context.Context, Span) {
	return t.start(ctx, name, trace.SpanKindServer)
}

// Start begins a span named name under ctx's span, or a trace of its
// own when ctx carries none, and returns a context that carries it. A
// name that is not one of the SpanNames is Unknown.
func (t *Telemetry) Start(ctx context.Context, name SpanName) (context.Context, Span) {
	return t.start(ctx, name, trace.SpanKindInternal)
}

func (t *Telemetry) start(ctx context.Context, name SpanName, kind trace.SpanKind) (context.Context, Span) {
	if t == nil || t.tracer == nil {
		return ctx, Span{}
	}
	if !slices.Contains(fixedSpans, name) {
		name = Unknown
	}
	ctx, s := t.tracer.Start(ctx, string(name), trace.WithSpanKind(kind))
	return ctx, Span{s: s, t: t}
}

// remoteParent is tc as the SDK's span context of a remote parent. It
// reports false for a trace context that is not valid.
//
// Of what the caller sent, the ids and the flags are all a span takes.
// The tracestate is left out: it is the caller's text, the server calls
// nothing it could pass it on to, and on a span it would be exported
// with everything else (THE RULE, above).
func remoteParent(tc proto.TraceContext) (trace.SpanContext, bool) {
	if tc.TraceID == "" {
		return trace.SpanContext{}, false
	}
	tid, err := trace.TraceIDFromHex(tc.TraceID)
	if err != nil {
		return trace.SpanContext{}, false
	}
	sid, err := trace.SpanIDFromHex(tc.SpanID)
	if err != nil {
		return trace.SpanContext{}, false
	}
	flags, err := strconv.ParseUint(tc.Flags, 16, 8)
	if err != nil || len(tc.Flags) != 2 {
		return trace.SpanContext{}, false
	}
	return trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.TraceFlags(flags), Remote: true}), true
}

// trace makes the tracer provider over procs, sampling as te says.
func (t *Telemetry) trace(res *resource.Resource, te traceEnv, procs ...sdktrace.SpanProcessor) {
	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res), sdktrace.WithSampler(te.newSampler())}
	for _, p := range procs {
		opts = append(opts, sdktrace.WithSpanProcessor(p))
	}
	t.tp = sdktrace.NewTracerProvider(opts...)
	t.tracer = t.tp.Tracer(scope)
}

// newSampler is the SDK's sampler te names. The default follows the
// caller's decision and samples every trace that starts here.
func (te traceEnv) newSampler() sdktrace.Sampler {
	switch te.sampler {
	case samplerAlwaysOn:
		return sdktrace.AlwaysSample()
	case samplerAlwaysOff:
		return sdktrace.NeverSample()
	case samplerRatio:
		return sdktrace.TraceIDRatioBased(te.ratio)
	case samplerParentOff:
		return sdktrace.ParentBased(sdktrace.NeverSample())
	case samplerParentRatio:
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(te.ratio))
	}
	return sdktrace.ParentBased(sdktrace.AlwaysSample())
}

// shownSampler is the sampler as the start-up line names it.
func (te traceEnv) shownSampler() string {
	if te.sampler == samplerRatio || te.sampler == samplerParentRatio {
		return te.sampler + "(" + strconv.FormatFloat(te.ratio, 'g', -1, 64) + ")"
	}
	return te.sampler
}

// batcher returns the processor that exports ended spans to exp in
// batches, on a goroutine of the SDK's, from a queue of at most te.queue
// spans (design §16.6). Ending a span only puts it in the queue.
//
// The SDK's batching processor drops a span its queue has no room for,
// and tells nobody how many. So the bound is enforced here, a step
// earlier, where it can be counted: a span is passed on only while fewer
// than te.queue are waiting, which leaves the SDK's own queue never
// full, and one that is not passed on is counted on
// aries.telemetry.dropped, as are the spans of an export that failed.
func (t *Telemetry) batcher(exp sdktrace.SpanExporter, te traceEnv) sdktrace.SpanProcessor {
	g := &gate{t: t, max: int64(te.queue)}
	g.ctx, g.cancel = context.WithCancel(context.Background())
	g.next = sdktrace.NewBatchSpanProcessor(&guardedSpans{exp: exp, g: g},
		sdktrace.WithMaxQueueSize(te.queue), sdktrace.WithMaxExportBatchSize(te.batch),
		sdktrace.WithBatchTimeout(te.delay), sdktrace.WithExportTimeout(te.exportTimeout))
	return g
}

// gate is the bound on the spans waiting for export, in front of the
// SDK's batching processor (batcher).
type gate struct {
	t    *Telemetry
	next sdktrace.SpanProcessor
	max  int64
	// waiting is how many spans were passed on and not yet handed to the
	// exporter.
	waiting atomic.Int64
	// ctx ends when the stop's deadline has passed: exports still under
	// way are cut short, and those that follow are not made. It is the
	// processor's lifetime, not a request's, which is why it is kept.
	ctx    context.Context
	cancel context.CancelFunc
}

func (*gate) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

// OnEnd queues s for export, or counts it as dropped when the queue is
// full. It never waits.
func (g *gate) OnEnd(s sdktrace.ReadOnlySpan) {
	if !s.SpanContext().IsSampled() {
		return
	}
	if g.waiting.Add(1) > g.max {
		g.waiting.Add(-1)
		g.t.Count(Dropped, SignalTraces)
		return
	}
	g.next.OnEnd(s)
}

func (g *gate) ForceFlush(ctx context.Context) error { return g.next.ForceFlush(ctx) }

// Shutdown exports what is queued and stops the processor. When ctx
// ends first, the exports still to make are abandoned and their spans
// counted as dropped, so it returns soon after ctx does, with nothing
// left running.
func (g *gate) Shutdown(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- g.next.Shutdown(context.WithoutCancel(ctx)) }()
	select {
	case err := <-done:
		g.cancel()
		return err
	case <-ctx.Done():
		g.cancel()
		<-done
		return ctx.Err()
	}
}

// guardedSpans is the trace exporter with its failures handled here,
// once: the spans of a failed export are counted on
// aries.telemetry.dropped and the failure logged, sampled, and never
// returned to the SDK, whose handler would log each one.
type guardedSpans struct {
	exp sdktrace.SpanExporter
	g   *gate
}

// ExportSpans sends spans. Unlike a metrics export, one that fails loses
// what it carried: the exporter's own retries, within the export's
// timeout, are the only second chance.
func (e *guardedSpans) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	g := e.g
	g.waiting.Add(-int64(len(spans)))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(g.ctx, cancel)()
	err := g.ctx.Err()
	if err == nil {
		err = e.exp.ExportSpans(ctx, spans)
	}
	switch {
	case err == nil:
		g.t.traceErrs.ok()
	case g.ctx.Err() != nil:
		// The stop's deadline passed. The caller of Shutdown is told,
		// and says so; it is no failure of the collector's to log.
		g.t.Record(Dropped, float64(len(spans)), SignalTraces)
	default:
		g.t.Record(Dropped, float64(len(spans)), SignalTraces)
		g.t.traceErrs.failed(logs.TelemetryExportFailed, "telemetry: a trace export failed; its spans are lost", err)
	}
	return nil
}

func (e *guardedSpans) Shutdown(ctx context.Context) error { return e.exp.Shutdown(ctx) }
