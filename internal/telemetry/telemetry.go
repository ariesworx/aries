package telemetry

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// scope names the meter ariesd's instruments belong to, and the
// tracer of its spans.
const scope = "github.com/ariesworx/aries"

// DefaultProjects is how many distinct projects an instrument's project
// attribute takes when Config.Projects is zero: the default of the
// projects limit.
const DefaultProjects = 500

// MaxModels is how many distinct models the model attribute of
// [UsageTokens] takes, whatever the price tables hold: a model past it
// is counted as [ModelOther].
const MaxModels = 64

// Config configures a Telemetry. The zero value exports nothing.
type Config struct {
	// Version is the build's version, the resource's service.version.
	Version string
	// Getenv looks up an environment variable, as os.Getenv does. The
	// OTEL_* variables are read through it (env.go lists them); nil
	// reads none, so nothing is pushed.
	Getenv func(string) string
	// Logger receives one line at the start saying what is exported, and
	// export failures, sampled. Nil discards.
	Logger *slog.Logger
	// Pull offers the instruments in Prometheus's text format
	// ([Telemetry.Serve] answers /metrics with them).
	Pull bool
	// PerProject keeps the project attribute on the instruments that
	// have one. Off, no instrument names a project.
	PerProject bool
	// Ops are the operations the server knows: the values the op
	// attribute may take. Any other op is recorded as unknown.
	Ops []string
	// Projects caps how many distinct projects the project attribute
	// takes; one more is recorded as other. Zero takes DefaultProjects.
	Projects int
	// Principals is how a person appears on a span: PrincipalsPseudonym,
	// which empty also means, PrincipalsName or PrincipalsOmit.
	Principals string
	// KeyFile is the file that holds the key the stand-ins of
	// PrincipalsPseudonym are made with. It is made, mode 0600, the
	// first time spans are exported with stand-ins, and never without
	// that. When it cannot be read or made, spans say nothing about who
	// until it can ([Telemetry.RetryKey]); that never stops a start.
	// KeyFileErr is why no file could be named, when KeyFile is empty.
	KeyFile    string
	KeyFileErr error
}

// Telemetry records ariesd's measurements and exports them. A nil
// *Telemetry is usable and does nothing, as is one that [New] made with
// nothing to export to. It is safe for concurrent use.
type Telemetry struct {
	log *slog.Logger
	// mp is nil when no metric is exported; every recording then returns
	// at once.
	mp *sdkmetric.MeterProvider
	// buildMu guards built, which says that build has made mp and the
	// instruments, and early, the metric exports that failed before it
	// had: the periodic reader's goroutine runs from the moment the
	// reader is made, and must read neither until then.
	buildMu    sync.Mutex
	built      bool
	early      int
	meter      metric.Meter
	perProject bool
	ops        map[string]struct{}
	inst       [numInstruments]instrument
	projects   projectSet
	models     projectSet
	// pull is the Prometheus handler, or nil when Config.Pull is off.
	pull http.Handler
	errs *errlog

	// tp and tracer are nil when no span is exported; every Start then
	// returns the zero Span. traceErrs logs the trace exporter's
	// failures, apart from the metrics', so that one signal's recovery
	// does not speak for the other's.
	tp        *sdktrace.TracerProvider
	tracer    trace.Tracer
	traceErrs *errlog
	// who is how a principal and a session appear on a span and on an
	// exported log record; never nil.
	who *people

	// lp and logger are nil when no log record is exported (logs.go);
	// logErrs logs the log exporter's failures.
	lp      *sdklog.LoggerProvider
	logger  otellog.Logger
	logErrs *errlog
}

// instrument is one instrument of the SDK's, by its kind.
type instrument struct {
	spec
	counter metric.Int64Counter
	updown  metric.Int64UpDownCounter
	hist    metric.Float64Histogram
	gauge   metric.Float64Gauge
}

// New makes a Telemetry from cfg and the OTEL_* variables cfg.Getenv
// answers for. It pushes metrics and spans over OTLP/HTTP when the
// variables name an endpoint for them, offers the metrics for pull when
// cfg.Pull is set, and does none of it, starting no goroutine and
// writing no file, when nothing is configured or OTEL_SDK_DISABLED is
// true. It refuses, with a fix, a variable it cannot honor. A key for
// stand-ins that it cannot read or make does not stop it: spans then
// say nothing about who. The caller calls Shutdown.
//
// A Telemetry that exports sets the SDK's process-wide error handler and
// logger to cfg.Logger's, so a process makes one.
func New(ctx context.Context, cfg Config) (*Telemetry, error) {
	env, err := readEnv(cfg.Getenv)
	if err != nil {
		return nil, err
	}
	t := newTelemetry(cfg, env)
	log := t.log
	tracing := !env.disabled && env.traces.endpoint != ""
	logging := !env.disabled && env.logs.endpoint != ""
	// Spans and exported log records both say who by stand-ins, so
	// either wants the key.
	if t.who, err = openPeople(cfg, tracing || logging, false, log); err != nil {
		return nil, err
	}
	if env.disabled {
		if env.endpoint != "" || env.traces.endpoint != "" || env.logs.endpoint != "" || cfg.Pull {
			log.Info("telemetry is off: OTEL_SDK_DISABLED is true", "event", logs.TelemetryOff)
		}
		return t, nil
	}
	var readers []sdkmetric.Reader
	if env.endpoint != "" {
		opts := []otlpmetrichttp.Option{
			otlpmetrichttp.WithEndpointURL(env.endpoint),
			otlpmetrichttp.WithTimeout(env.requestTimeout),
			// Totals are cumulative, so the next export carries what a
			// failed one did; retrying within an interval only holds the
			// exporter's goroutine for longer.
			otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{Enabled: false}),
		}
		if len(env.headers) > 0 {
			opts = append(opts, otlpmetrichttp.WithHeaders(env.headers))
		}
		exp, err := otlpmetrichttp.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("telemetry: %s; fix: correct the OTEL_EXPORTER_OTLP_* variables in ariesd's environment, or unset %s to push nothing",
				t.errs.redact(err.Error()), envEndpoint)
		}
		readers = append(readers, sdkmetric.NewPeriodicReader(&guarded{Exporter: exp, t: t},
			sdkmetric.WithInterval(env.interval), sdkmetric.WithTimeout(env.exportTimeout)))
	}
	if cfg.Pull {
		pull, reader, err := newPull(t.errs)
		if err != nil {
			return nil, err
		}
		t.pull = pull
		readers = append(readers, reader)
	}
	if len(readers) == 0 && !tracing && !logging {
		return t, nil
	}
	// One resource, so that metrics and spans carry the one instance id.
	res := resource.NewSchemaless(env.resource(cfg.Version)...)
	if len(readers) > 0 {
		if err := t.build(res, readers...); err != nil {
			return nil, err
		}
	}
	// The headers' names and values are never logged: a value may be a
	// token (design §16.3), and a name alone can say whose.
	line := []any{"event", logs.TelemetryOn, "push", shown(env.endpoint), "push_interval", env.interval.String(), "push_headers", len(env.headers),
		"pull", cfg.Pull, "per_project", cfg.PerProject, "traces", shown(env.traces.endpoint), "logs", shown(env.logs.endpoint)}
	if tracing {
		te := env.traces
		opts := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(te.endpoint), otlptracehttp.WithTimeout(te.requestTimeout)}
		if len(te.headers) > 0 {
			opts = append(opts, otlptracehttp.WithHeaders(te.headers))
		}
		// The exporter's own retries are left on, unlike the metrics': a
		// span that is not exported is lost, and the retries end with
		// the export's timeout.
		exp, err := otlptracehttp.New(ctx, opts...)
		if err != nil {
			return nil, errors.Join(
				fmt.Errorf("telemetry: %s; fix: correct the OTEL_EXPORTER_OTLP_* variables in ariesd's environment, or set %s to %s to export no traces",
					t.errs.redact(err.Error()), envTracesExporter, exporterNone),
				t.stopMetrics(context.WithoutCancel(ctx)))
		}
		t.trace(res, te, t.batcher(exp, te))
		line = append(line, "trace_headers", len(te.headers), "sampler", te.shownSampler())
	}
	if logging {
		le := env.logs
		opts := []otlploghttp.Option{otlploghttp.WithEndpointURL(le.endpoint), otlploghttp.WithTimeout(le.requestTimeout)}
		if len(le.headers) > 0 {
			opts = append(opts, otlploghttp.WithHeaders(le.headers))
		}
		// The exporter's retries are left on, as for traces: they end
		// with the export's timeout.
		exp, err := otlploghttp.New(ctx, opts...)
		if err != nil {
			stop := context.WithoutCancel(ctx)
			return nil, errors.Join(
				fmt.Errorf("telemetry: %s; fix: correct the OTEL_EXPORTER_OTLP_* variables in ariesd's environment, or unset %s to leave the logs in the journal alone",
					t.errs.redact(err.Error()), envLogsExporter),
				t.stopTraces(stop), t.stopMetrics(stop))
		}
		t.logging(res, t.logBatcher(exp, le))
		line = append(line, "log_headers", len(le.headers))
	}
	if tracing || logging {
		line = append(line, "principals", t.who.mode)
	}
	setSDKLogging(t.errs)
	log.Info("telemetry on", append(line, "service", env.service)...)
	return t, nil
}

// newTelemetry is a Telemetry that exports nothing yet.
func newTelemetry(cfg Config, env envConfig) *Telemetry {
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	t := &Telemetry{log: log, errs: newErrlog(log, env.secrets(), SignalMetrics), traceErrs: newErrlog(log, env.secrets(), SignalTraces),
		logErrs:    newErrlog(log, env.secrets(), SignalLogs),
		perProject: cfg.PerProject, ops: make(map[string]struct{}, len(cfg.Ops)), who: &people{mode: PrincipalsOmit, log: log}}
	t.who.state.Store(&keyState{})
	for _, op := range cfg.Ops {
		t.ops[op] = struct{}{}
	}
	t.projects.max = cfg.Projects
	if t.projects.max <= 0 {
		t.projects.max = DefaultProjects
	}
	t.models.max = MaxModels
	return t
}

// scrapeTimeout bounds the gathering of one scrape.
const scrapeTimeout = 10 * time.Second

// newPull makes the SDK's Prometheus exporter on a registry of its own,
// which holds nothing else, and the handler that writes it as text.
func newPull(errs *errlog) (http.Handler, sdkmetric.Reader, error) {
	reg := prometheus.NewRegistry()
	exp, err := otelprom.New(otelprom.WithRegisterer(reg), otelprom.WithoutScopeInfo())
	if err != nil {
		return nil, nil, fmt.Errorf("telemetry: the Prometheus exporter: %w", err)
	}
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{ErrorLog: promLog{errs}, Timeout: scrapeTimeout}), exp, nil
}

// histogramBuckets gives each of ariesd's histograms its buckets:
// [CycleBuckets] to the issues' cycle time, which runs to days, and
// [Buckets] to every other. It is one view, since two that match an
// instrument would each make a stream of it.
func histogramBuckets(i sdkmetric.Instrument) (sdkmetric.Stream, bool) {
	if i.Kind != sdkmetric.InstrumentKindHistogram || !strings.HasPrefix(i.Name, "aries.") {
		return sdkmetric.Stream{}, false
	}
	b := Buckets
	if i.Name == specs[IssueCycleDuration].name {
		b = CycleBuckets
	}
	return sdkmetric.Stream{Name: i.Name, Description: i.Description, Unit: i.Unit,
		Aggregation: sdkmetric.AggregationExplicitBucketHistogram{Boundaries: b}}, true
}

// build makes the meter provider over readers, and every instrument.
func (t *Telemetry) build(res *resource.Resource, readers ...sdkmetric.Reader) error {
	opts := []sdkmetric.Option{
		sdkmetric.WithResource(res),
		sdkmetric.WithView(histogramBuckets),
	}
	for _, r := range readers {
		opts = append(opts, sdkmetric.WithReader(r))
	}
	mp := sdkmetric.NewMeterProvider(opts...)
	t.meter = mp.Meter(scope)
	var errs []error
	for i, sp := range specs {
		in := instrument{spec: sp}
		var err error
		switch {
		case sp.watched:
			// Made by Watch, with its callback.
		case sp.kind == Counter:
			in.counter, err = t.meter.Int64Counter(sp.name, metric.WithDescription(sp.desc), metric.WithUnit(sp.unit))
		case sp.kind == UpDownCounter:
			in.updown, err = t.meter.Int64UpDownCounter(sp.name, metric.WithDescription(sp.desc), metric.WithUnit(sp.unit))
		case sp.kind == Histogram:
			in.hist, err = t.meter.Float64Histogram(sp.name, metric.WithDescription(sp.desc), metric.WithUnit(sp.unit))
		case sp.kind == Gauge:
			in.gauge, err = t.meter.Float64Gauge(sp.name, metric.WithDescription(sp.desc), metric.WithUnit(sp.unit))
		}
		errs = append(errs, err)
		t.inst[i] = in
	}
	// The Go runtime's metrics, under the names of OpenTelemetry's
	// semantic conventions (decision 33). They are read from
	// runtime/metrics when a reader collects, and start no goroutine.
	errs = append(errs, runtime.Start(runtime.WithMeterProvider(mp)))
	if err := errors.Join(errs...); err != nil {
		return errors.Join(fmt.Errorf("telemetry: make the instruments: %w", err), mp.Shutdown(context.Background()))
	}
	t.mp = mp
	t.builtInstruments()
	return nil
}

// Enabled reports whether t records metrics. Whether it starts spans is
// [Telemetry.Tracing].
func (t *Telemetry) Enabled() bool { return t != nil && t.mp != nil }

// Record records v on instrument i. vals are the values of i's
// attributes, in the order its constant's doc comment gives. A value
// that is not on its attribute's list is recorded as unknown (an op) or
// other, and a missing one likewise, so no caller can add a series the
// lists do not allow. A counter adds v, an up-down counter adds v, which
// may be negative, a histogram observes v, and a gauge is set to v.
//
// Record touches memory only: it never waits for an export.
func (t *Telemetry) Record(i Instrument, v float64, vals ...string) {
	if t == nil || t.mp == nil || i < 0 || i >= numInstruments {
		return
	}
	in := &t.inst[i]
	var buf [4]attribute.KeyValue
	ctx := context.Background()
	set := metric.WithAttributes(t.attributes(buf[:0], in.attrs, vals)...)
	switch {
	case in.counter != nil:
		in.counter.Add(ctx, int64(v), set)
	case in.updown != nil:
		in.updown.Add(ctx, int64(v), set)
	case in.hist != nil:
		in.hist.Record(ctx, v, set)
	case in.gauge != nil:
		in.gauge.Record(ctx, v, set)
	}
}

// attributes appends to kvs the attributes of attrs with the values vals,
// in order, as Record sets them: a project's only when the server records
// per project, and a value not on its list as the fallback.
func (t *Telemetry) attributes(kvs []attribute.KeyValue, attrs []attr, vals []string) []attribute.KeyValue {
	for j, a := range attrs {
		if a.key == KeyProject && !t.perProject {
			continue
		}
		val := ""
		if j < len(vals) {
			val = vals[j]
		}
		kvs = append(kvs, attribute.String(a.key, t.value(a, val)))
	}
	return kvs
}

// Count adds one to counter i.
func (t *Telemetry) Count(i Instrument, vals ...string) { t.Record(i, 1, vals...) }

// Since observes on histogram i the seconds since start.
func (t *Telemetry) Since(i Instrument, start time.Time, vals ...string) {
	if t == nil || t.mp == nil {
		return
	}
	t.Record(i, time.Since(start).Seconds(), vals...)
}

// Watch has fn read instrument i whenever the instruments are collected:
// at each export and each scrape. fn must be fast and safe for
// concurrent use. Only the watched instruments are read so, the gauges
// [StoresOpen], [DBPoolConns] and [DBPoolInUse] and the counter
// [DBPoolWaits], whose fn returns a total that only grows; any other
// instrument is ignored.
func (t *Telemetry) Watch(i Instrument, fn func() float64) {
	if t == nil || t.mp == nil || i < 0 || i >= numInstruments || !specs[i].watched || len(specs[i].attrs) > 0 {
		return
	}
	sp := specs[i]
	observe := metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
		o.Observe(fn())
		return nil
	})
	var err error
	if sp.kind == Counter {
		// A total kept elsewhere, which fn reads: it only grows.
		_, err = t.meter.Float64ObservableCounter(sp.name, metric.WithDescription(sp.desc), metric.WithUnit(sp.unit), observe)
	} else {
		_, err = t.meter.Float64ObservableGauge(sp.name, metric.WithDescription(sp.desc), metric.WithUnit(sp.unit), observe)
	}
	if err != nil {
		t.errs.failed(logs.TelemetrySDKError, "telemetry: an instrument could not be watched", fmt.Errorf("%s: %w", sp.name, err))
	}
}

// WatchSeries has fn read the gauge i, which has attributes, whenever the
// instruments are collected: fn calls observe once for each series, with
// its value and the values of i's attributes in order, checked as Record
// checks them. Only [Issues], [ClaimsHeld] and [IssuesReady] are read so;
// any other instrument is ignored. fn runs at each export and each scrape, so it must be bounded
// and safe for concurrent use. Series that more than one observation
// names (a project past the cap on projects, all as other) are summed.
func (t *Telemetry) WatchSeries(i Instrument, fn func(observe func(v float64, vals ...string))) {
	if t == nil || t.mp == nil || (i != Issues && i != ClaimsHeld && i != IssuesReady) {
		return
	}
	sp := specs[i]
	_, err := t.meter.Float64ObservableGauge(sp.name, metric.WithDescription(sp.desc), metric.WithUnit(sp.unit),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			sums := map[attribute.Set]float64{}
			fn(func(v float64, vals ...string) {
				var buf [4]attribute.KeyValue
				sums[attribute.NewSet(t.attributes(buf[:0], sp.attrs, vals)...)] += v
			})
			for set, v := range sums {
				o.Observe(v, metric.WithAttributeSet(set))
			}
			return nil
		}))
	if err != nil {
		t.errs.failed(logs.TelemetrySDKError, "telemetry: an instrument could not be watched", fmt.Errorf("%s: %w", sp.name, err))
	}
}

// value returns v if a allows it, and a's fallback if not.
func (t *Telemetry) value(a attr, v string) string {
	switch a.key {
	case KeyOp:
		if _, ok := t.ops[v]; ok {
			return v
		}
	case KeyProject:
		if t.projects.admit(v) {
			return v
		}
	case KeyModel:
		if t.models.has(v) {
			return v
		}
	default:
		if a.allowed(v) {
			return v
		}
	}
	return a.fallback
}

// AdmitModel makes model a value of the model attribute, and reports
// whether it is one. The caller admits only a model the server has a
// price for: the price table is the server's, and a model a client
// reports is text it chose (rule 2). A model of the wrong shape, one
// past [MaxModels] and [ModelOther] are not admitted, and are counted
// as [ModelOther].
func (t *Telemetry) AdmitModel(model string) bool {
	if t == nil || model == ModelOther || !proto.UsageModel.MatchString(model) {
		return false
	}
	return t.models.take(model)
}

// Shutdown exports what is pending and stops the exporters, giving up
// when ctx ends, so a collector that is down or silent holds a stop for
// no longer than ctx allows. Give it a deadline. The log records and the
// spans go first, so that the last export of metrics counts those that
// were lost. A Telemetry that exports nothing has nothing to stop.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	if t == nil {
		return nil
	}
	return errors.Join(t.stopLogs(ctx), t.stopTraces(ctx), t.stopMetrics(ctx))
}

// stopTraces exports the spans that are queued and stops the tracer's
// exporter. When it returns, every span that ended was exported or is
// counted as dropped. Spans started afterward go nowhere.
func (t *Telemetry) stopTraces(ctx context.Context) error {
	if t.tp == nil {
		return nil
	}
	if err := t.tp.Shutdown(ctx); err != nil {
		return fmt.Errorf("telemetry: stop the trace exporter: %s", t.errs.redact(err.Error()))
	}
	return nil
}

// stopMetrics makes the last export of metrics and stops their
// exporters.
func (t *Telemetry) stopMetrics(ctx context.Context) error {
	if t.mp == nil {
		return nil
	}
	if err := t.mp.Shutdown(ctx); err != nil {
		return fmt.Errorf("telemetry: stop the exporters: %s", t.errs.redact(err.Error()))
	}
	return nil
}

// projectSet is the projects the project attribute has taken: at most
// max of them, each a lowercase UUID, so that neither a string a client
// chose nor a server with more projects than its limit can add series
// without bound.
type projectSet struct {
	mu  sync.RWMutex
	m   map[string]struct{}
	max int
}

// admit reports whether id may be a project attribute's value.
func (p *projectSet) admit(id string) bool { return uuidLike(id) && p.take(id) }

// has reports whether id is among the values p has taken.
func (p *projectSet) has(id string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.m[id]
	return ok
}

// take reports whether id is among the values p has taken, taking it if
// there is room.
func (p *projectSet) take(id string) bool {
	p.mu.RLock()
	_, ok := p.m[id]
	p.mu.RUnlock()
	if ok {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.m[id]; ok {
		return true
	}
	if len(p.m) >= p.max {
		return false
	}
	if p.m == nil {
		p.m = map[string]struct{}{}
	}
	p.m[id] = struct{}{}
	return true
}

// uuidLike reports whether s has a lowercase UUID's shape, as every
// project id does.
func uuidLike(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return true
}

// instanceID returns a random UUID, the resource's service.instance.id:
// it tells one run of the daemon from the next.
func instanceID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
