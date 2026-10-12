package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/ariesworx/starfix/internal/logs"
)

// Logs over OTLP (design §16.4). The journal is the logs' home; with
// OTEL_LOGS_EXPORTER=otlp the same records also leave the machine, and
// this file decides what of a record may.
//
// THE RULE of trace.go holds for a record as for a span. A record
// carries the line's message, which is fixed text in the code (a test
// of internal/logs holds every log call to a string literal), its level,
// its event, and the fields whose kind internal/logs gives as an id, a
// word or a number, each only in its own shape. Who it is about is said
// as telemetry: principals: allows, in every field that names a person.
// An error's text, a path, a name and any field the schema does not
// list stay in the journal, whatever the setting: a message quotes what
// it refused. The journal's line is found from a record by its req, its
// trace id, or its time and event.

// maxWord bounds an exported word.
const maxWord = 64

// Logging reports whether t exports log records.
func (t *Telemetry) Logging() bool { return t != nil && t.logger != nil }

// LogHandler returns the handler that exports each record it is given
// over OTLP, or nil when logs are not exported. starfixd's log handler
// gives it every record it writes to the journal (logs.Handler.Tap),
// whole. Handle only puts the record in a bounded queue, or counts it
// as dropped when the queue is full: it never waits for an export.
func (t *Telemetry) LogHandler() slog.Handler {
	if !t.Logging() {
		return nil
	}
	return logTap{t}
}

// logTap is the slog.Handler over the SDK's logger.
type logTap struct{ t *Telemetry }

func (logTap) Enabled(context.Context, slog.Level) bool { return true }

// WithAttrs and WithGroup are never called: logs.Handler gives each
// record whole.
func (h logTap) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h logTap) WithGroup(string) slog.Handler      { return h }

func (h logTap) Handle(ctx context.Context, r slog.Record) error {
	var rec otellog.Record
	rec.SetTimestamp(r.Time)
	rec.SetSeverity(severity(r.Level))
	rec.SetSeverityText(r.Level.String())
	rec.SetBody(attribute.StringValue(r.Message))
	r.Attrs(func(a slog.Attr) bool {
		if kv, ok := h.t.logAttr(a); ok {
			if a.Key == logs.KeyEvent {
				rec.SetEventName(kv.Value.AsString())
			}
			rec.AddAttributes(kv)
		}
		return true
	})
	// The span's ids come from ctx, which the SDK reads.
	h.t.logger.Emit(ctx, rec)
	return nil
}

// severity is level as OpenTelemetry numbers it.
func severity(level slog.Level) otellog.Severity {
	switch {
	case level >= slog.LevelError:
		return otellog.SeverityError
	case level >= slog.LevelWarn:
		return otellog.SeverityWarn
	case level >= slog.LevelInfo:
		return otellog.SeverityInfo
	}
	return otellog.SeverityDebug
}

// logAttr is a as an exported record may carry it, and false when it
// may not (THE RULE, above).
func (t *Telemetry) logAttr(a slog.Attr) (attribute.KeyValue, bool) {
	v := a.Value.Resolve()
	text := func() string { return textOf(v) }
	str := func(s string, ok bool) (attribute.KeyValue, bool) {
		return attribute.String(a.Key, s), ok && s != ""
	}
	switch logs.KindOf(a.Key) {
	case logs.KindWord:
		switch v.Kind() {
		case slog.KindInt64:
			return attribute.Int64(a.Key, v.Int64()), true
		case slog.KindUint64:
			return attribute.Int64(a.Key, int64(min(v.Uint64(), 1<<62))), true //nolint:gosec // bounded just here
		case slog.KindFloat64:
			return attribute.Float64(a.Key, v.Float64()), true
		case slog.KindBool:
			return attribute.Bool(a.Key, v.Bool()), true
		case slog.KindDuration:
			return str(v.Duration().String(), true)
		case slog.KindTime:
			return str(v.Time().UTC().Format(time.RFC3339Nano), true)
		}
		s := text()
		if a.Key == logs.KeyEvent {
			return str(s, logs.IsEvent(s))
		}
		return str(s, word(s))
	case logs.KindPrincipal:
		return str(t.who.principal(text()), true)
	case logs.KindSession:
		return str(t.who.session(text()), true)
	case logs.KindProject:
		return str(text(), uuidLike(text()))
	case logs.KindOp:
		return str(t.op(text()), true)
	case logs.KindCode:
		return str(oneOf(results, text()), true)
	case logs.KindReq:
		return str(text(), hexDigits(text(), 16))
	case logs.KindIssue:
		return str(text(), issueID(text()))
	case logs.KindFingerprint:
		return str(text(), fingerprint(text()))
	}
	return attribute.KeyValue{}, false
}

// textOf is v as text when it is a string or of a type made of one, as
// logs.Event, a code and the store's ids are, and "" for anything else:
// nothing is formatted into text here.
func textOf(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindAny:
		if rv := reflect.ValueOf(v.Any()); rv.Kind() == reflect.String {
			return rv.String()
		}
	}
	return ""
}

// word reports whether s is a word or a few of the code's own: letters,
// digits, spaces and the marks of a name, a duration or a time, and
// nothing a sentence, a path, a command or a key would need.
func word(s string) bool {
	if s == "" || len(s) > maxWord {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune(" _.:+-", rune(c)) {
			return false
		}
	}
	return true
}

// hexDigits reports whether s is n lowercase hex digits.
func hexDigits(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// issueID reports whether s has an issue id's shape: hyphen-separated
// runs of lowercase letters and digits, starting with a letter, and
// optionally dotted child numbers.
func issueID(s string) bool {
	if len(s) < 3 || len(s) > maxWord || s[0] < 'a' || s[0] > 'z' || !strings.Contains(s, "-") {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

// fingerprint reports whether s has the shape of a key's SHA-256
// fingerprint, as OpenSSH writes it: never a key.
func fingerprint(s string) bool {
	b64, ok := strings.CutPrefix(s, "SHA256:")
	if !ok || len(b64) != 43 {
		return false
	}
	for i := range len(b64) {
		c := b64[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '+' && c != '/' {
			return false
		}
	}
	return true
}

// logging makes the logger provider over procs.
func (t *Telemetry) logging(res *resource.Resource, procs ...sdklog.Processor) {
	opts := []sdklog.LoggerProviderOption{sdklog.WithResource(res)}
	for _, p := range procs {
		opts = append(opts, sdklog.WithProcessor(p))
	}
	t.lp = sdklog.NewLoggerProvider(opts...)
	t.logger = t.lp.Logger(scope)
}

// logBatcher returns the processor that exports records to exp in
// batches, on a goroutine of the SDK's, from a queue of at most le.queue
// records (design §16.6). Emitting a record only puts it in the queue.
//
// The SDK's batching processor overwrites the oldest record when its
// queue is full, and reports how many only to its own logger. So the
// bound is enforced here, a step earlier, where it can be counted, as
// for spans (batcher): a record is passed on only while fewer than
// le.queue are waiting, and one that is not is counted on
// starfix.telemetry.dropped, as are the records of an export that
// failed.
func (t *Telemetry) logBatcher(exp sdklog.Exporter, le logEnv) sdklog.Processor {
	g := &logGate{t: t, max: int64(le.queue)}
	g.ctx, g.cancel = context.WithCancel(context.Background())
	g.next = sdklog.NewBatchProcessor(&guardedLogs{exp: exp, g: g},
		sdklog.WithMaxQueueSize(le.queue), sdklog.WithExportMaxBatchSize(le.batch),
		sdklog.WithExportInterval(le.delay), sdklog.WithExportTimeout(le.exportTimeout))
	return g
}

// logGate is the bound on the records waiting for export, in front of
// the SDK's batching processor (logBatcher).
type logGate struct {
	t    *Telemetry
	next *sdklog.BatchProcessor
	max  int64
	// waiting is how many records were passed on and not yet handed to
	// the exporter.
	waiting atomic.Int64
	// stopped says Shutdown has begun: the SDK's processor takes no
	// record from then on, so none is counted as waiting.
	stopped atomic.Bool
	// ctx ends when the stop's deadline has passed, as gate's does. It
	// is the processor's lifetime, not a request's, which is why it is
	// kept.
	ctx    context.Context
	cancel context.CancelFunc
}

func (*logGate) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }

// OnEmit queues r for export, or counts it as dropped when the queue is
// full. It never waits.
func (g *logGate) OnEmit(ctx context.Context, r *sdklog.Record) error {
	if g.stopped.Load() {
		g.t.Count(Dropped, SignalLogs)
		return nil
	}
	if g.waiting.Add(1) > g.max {
		g.waiting.Add(-1)
		g.t.Count(Dropped, SignalLogs)
		return nil
	}
	return g.next.OnEmit(ctx, r)
}

func (g *logGate) ForceFlush(ctx context.Context) error { return g.next.ForceFlush(ctx) }

// Shutdown exports what is queued and stops the processor. When ctx
// ends first, the exports still to make are abandoned and their records
// counted as dropped, so it returns soon after ctx does, with nothing
// left running.
func (g *logGate) Shutdown(ctx context.Context) error {
	g.stopped.Store(true)
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

// guardedLogs is the log exporter with its failures handled here, once:
// the records of a failed export are counted on
// starfix.telemetry.dropped and the failure logged, sampled, and never
// returned to the SDK, whose handler would log each one.
//
// The line that says an export failed is itself a record, and is
// exported with the rest. It is sampled, one line in ten minutes, so it
// cannot feed itself.
type guardedLogs struct {
	exp sdklog.Exporter
	g   *logGate
}

// Export sends recs. One that fails loses what it carried, from the
// export only: the journal has every line.
func (e *guardedLogs) Export(ctx context.Context, recs []sdklog.Record) error {
	if len(recs) == 0 {
		return nil
	}
	g := e.g
	g.waiting.Add(-int64(len(recs)))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(g.ctx, cancel)()
	err := g.ctx.Err()
	if err == nil {
		err = e.exp.Export(ctx, recs)
	}
	switch {
	case err == nil:
		g.t.logErrs.ok()
	case g.ctx.Err() != nil:
		// The stop's deadline passed. The caller of Shutdown is told,
		// and says so; it is no failure of the collector's to log.
		g.t.Record(Dropped, float64(len(recs)), SignalLogs)
	default:
		g.t.Record(Dropped, float64(len(recs)), SignalLogs)
		g.t.logErrs.failed(logs.TelemetryExportFailed, "telemetry: a log export failed; its records are in the journal alone", err)
	}
	return nil
}

func (e *guardedLogs) Shutdown(ctx context.Context) error   { return e.exp.Shutdown(ctx) }
func (e *guardedLogs) ForceFlush(ctx context.Context) error { return e.exp.ForceFlush(ctx) }

// stopLogs exports the records that are queued and stops the log
// exporter. When it returns, every record emitted before it was
// exported or is counted as dropped.
func (t *Telemetry) stopLogs(ctx context.Context) error {
	if t.lp == nil {
		return nil
	}
	if err := t.lp.Shutdown(ctx); err != nil {
		return fmt.Errorf("telemetry: stop the log exporter: %s", t.errs.redact(err.Error()))
	}
	return nil
}
