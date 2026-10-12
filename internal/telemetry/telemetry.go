package telemetry

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// scope names the meter starfixd's instruments belong to.
const scope = "github.com/ariesworx/starfix"

// DefaultProjects is how many distinct projects an instrument's project
// attribute takes when Config.Projects is zero: the default of the
// projects limit.
const DefaultProjects = 500

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
}

// Telemetry records starfixd's measurements and exports them. A nil
// *Telemetry is usable and does nothing, as is one that [New] made with
// nothing to export to. It is safe for concurrent use.
type Telemetry struct {
	log *slog.Logger
	// mp is nil when nothing is exported; every method then returns at
	// once.
	mp         *sdkmetric.MeterProvider
	meter      metric.Meter
	perProject bool
	ops        map[string]struct{}
	inst       [numInstruments]instrument
	projects   projectSet
	// pull is the Prometheus handler, or nil when Config.Pull is off.
	pull http.Handler
	errs *errlog
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
// answers for. It pushes metrics over OTLP/HTTP when the variables name
// an endpoint, offers them for pull when cfg.Pull is set, and does
// neither, starting no goroutine, when nothing is configured or
// OTEL_SDK_DISABLED is true. It refuses, with a fix, a variable it
// cannot honor. The caller calls Shutdown.
//
// A Telemetry that exports sets the SDK's process-wide error handler and
// logger to cfg.Logger's, so a process makes one.
func New(ctx context.Context, cfg Config) (*Telemetry, error) {
	env, err := readEnv(cfg.Getenv)
	if err != nil {
		return nil, err
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	t := &Telemetry{log: log, errs: newErrlog(log, env.secrets())}
	if env.disabled {
		if env.endpoint != "" || cfg.Pull {
			log.Info("telemetry is off: OTEL_SDK_DISABLED is true")
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
			return nil, fmt.Errorf("telemetry: %s; fix: correct the OTEL_EXPORTER_OTLP_* variables in starfixd's environment, or unset %s to push nothing",
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
	if len(readers) == 0 {
		return t, nil
	}
	if err := t.build(cfg, env, readers...); err != nil {
		return nil, err
	}
	setSDKLogging(t.errs)
	push := "off"
	if env.endpoint != "" {
		push = env.shownEndpoint()
	}
	// The headers' names and values are never logged: a value may be a
	// token (design §16.3), and a name alone can say whose.
	log.Info("telemetry on", "push", push, "push_interval", env.interval.String(), "push_headers", len(env.headers),
		"pull", cfg.Pull, "per_project", cfg.PerProject, "service", env.service)
	return t, nil
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

// build makes the meter provider over readers, and every instrument.
func (t *Telemetry) build(cfg Config, env envConfig, readers ...sdkmetric.Reader) error {
	res := resource.NewSchemaless(env.resource(cfg.Version)...)
	opts := []sdkmetric.Option{
		sdkmetric.WithResource(res),
		sdkmetric.WithView(sdkmetric.NewView(
			sdkmetric.Instrument{Name: "starfix.*", Kind: sdkmetric.InstrumentKindHistogram},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{Boundaries: Buckets}})),
	}
	for _, r := range readers {
		opts = append(opts, sdkmetric.WithReader(r))
	}
	mp := sdkmetric.NewMeterProvider(opts...)
	t.meter = mp.Meter(scope)
	t.perProject = cfg.PerProject
	t.ops = make(map[string]struct{}, len(cfg.Ops))
	for _, op := range cfg.Ops {
		t.ops[op] = struct{}{}
	}
	t.projects.max = cfg.Projects
	if t.projects.max <= 0 {
		t.projects.max = DefaultProjects
	}
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
	return nil
}

// Enabled reports whether t exports anything.
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
	kvs := buf[:0]
	for j, a := range in.attrs {
		if a.key == KeyProject && !t.perProject {
			continue
		}
		val := ""
		if j < len(vals) {
			val = vals[j]
		}
		kvs = append(kvs, attribute.String(a.key, t.value(a, val)))
	}
	ctx := context.Background()
	set := metric.WithAttributes(kvs...)
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

// Count adds one to counter i.
func (t *Telemetry) Count(i Instrument, vals ...string) { t.Record(i, 1, vals...) }

// Since observes on histogram i the seconds since start.
func (t *Telemetry) Since(i Instrument, start time.Time, vals ...string) {
	if t == nil || t.mp == nil {
		return
	}
	t.Record(i, time.Since(start).Seconds(), vals...)
}

// Watch has fn read gauge i whenever the instruments are collected: at
// each export and each scrape. fn must be fast and safe for concurrent
// use. Only [StoresOpen] is watched; any other instrument is ignored.
func (t *Telemetry) Watch(i Instrument, fn func() float64) {
	if t == nil || t.mp == nil || i < 0 || i >= numInstruments || !specs[i].watched {
		return
	}
	sp := specs[i]
	_, err := t.meter.Float64ObservableGauge(sp.name, metric.WithDescription(sp.desc), metric.WithUnit(sp.unit),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			o.Observe(fn())
			return nil
		}))
	if err != nil {
		t.errs.failed("telemetry: watch "+sp.name, err)
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
	default:
		if a.allowed(v) {
			return v
		}
	}
	return a.fallback
}

// Span is a unit of work in a trace. Until slice O2 it records nothing.
type Span struct{}

// End ends the span.
func (Span) End() {}

// Start begins a span named name under ctx's span, if it has one, and
// returns a context that carries it. Traces are slice O2 (design
// §16.8): until then the context is returned as it came and the span
// does nothing.
func (t *Telemetry) Start(ctx context.Context, _ string) (context.Context, Span) {
	return ctx, Span{}
}

// Shutdown exports what is pending and stops the exporters, giving up
// when ctx ends, so a collector that is down or silent holds a stop for
// no longer than ctx allows. Give it a deadline. A Telemetry that
// exports nothing has nothing to stop.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	if t == nil || t.mp == nil {
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
func (p *projectSet) admit(id string) bool {
	if !uuidLike(id) {
		return false
	}
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
