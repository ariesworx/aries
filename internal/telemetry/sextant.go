package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"sync"

	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/proto"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// Sextant's metrics (docs/design/sextant.md §3). Sextant is a binary of
// its own that reads one ariesd's public API and exports what it reads;
// its instruments are here, apart from ariesd's, so that this package
// stays the only one that imports the SDK (decision 28) and Sextant
// links what ariesd already does. A Sextant has a meter provider of its
// own, which holds none of ariesd's instruments, not even the Go
// runtime's, and ariesd never makes one.

// sextantScope names the meter Sextant's instruments belong to.
const sextantScope = "github.com/ariesworx/aries/sextant"

// sextantService is the resource's service.name unless
// OTEL_SERVICE_NAME gives another.
const sextantService = "sextant"

// The names of Sextant's instruments.
const (
	SextantCostName         = "sextant.cost.usd"
	SextantScrapeErrorsName = "sextant.scrape.errors"
)

// The values of a scrape error's op: what Sextant was doing when it
// failed. Any other is recorded as unknown.
const (
	// SextantOpConnect is the connection to the server: the dial, the
	// sign-in and the handshake.
	SextantOpConnect = "connect"
	SextantOpPrices  = proto.OpPrices
	SextantOpCost    = proto.OpCost
)

var sextantOps = []string{SextantOpConnect, SextantOpPrices, SextantOpCost}

// sextantCodes are the values a scrape error's code takes: the
// protocol's error codes. Any other is [Other].
var sextantCodes = []string{
	string(proto.CodeNotFound), string(proto.CodeConflict), string(proto.CodeExists), string(proto.CodeCycle),
	string(proto.CodeInvalid), string(proto.CodeAcceptance), string(proto.CodeForbidden), string(proto.CodeUnavailable),
	string(proto.CodeVersion), string(proto.CodeAuth), string(proto.CodeBusy), string(proto.CodeStale),
}

// Defaults of Sextant's caps (sextant.md §3).
const (
	DefaultSextantProjects = 50
	DefaultSextantModels   = 20
)

// SextantConfig configures a Sextant. The zero value exports nothing.
type SextantConfig struct {
	// Version is the build's version, the resource's service.version.
	Version string
	// Getenv looks up the OTEL_* variables of metrics (env.go); nil
	// reads none, so nothing is pushed. Those of traces and logs are
	// read and checked, and nothing of either is exported.
	Getenv func(string) string
	// Logger receives one line at the start saying what is exported, and
	// export failures, sampled. Nil discards.
	Logger *slog.Logger
	// Pull offers the instruments in Prometheus's text format
	// ([Sextant.Serve] answers /metrics with them).
	Pull bool
	// Projects caps how many distinct projects the project attribute
	// takes, and Models how many distinct models the model attribute
	// does; one more is recorded as other, and (other) for a model. Zero
	// takes the default.
	Projects, Models int
}

// SextantCost is one model's cost in a project's window, in US dollars
// at list price.
type SextantCost struct {
	Model string
	USD   float64
}

// Sextant records what Sextant reads and exports it. A nil *Sextant is
// usable and does nothing, as is one [NewSextant] made with nothing to
// export to. It is safe for concurrent use.
type Sextant struct {
	// t holds the error log and the Prometheus handler, and serves the
	// listener; it records nothing.
	t *Telemetry
	// mp is nil when nothing is exported.
	mp       *sdkmetric.MeterProvider
	errs     metric.Int64Counter
	projects projectSet
	models   projectSet

	mu sync.Mutex
	// cost is each project's last figures, by the project's id, which
	// the gauge's callback reads at each collection.
	cost map[string]projectCost
}

// projectCost is a project's last figures, by model, under the values it
// and they were admitted as.
type projectCost struct {
	label   string
	byModel map[string]float64
}

// NewSextant makes a Sextant from cfg and the OTEL_* variables
// cfg.Getenv answers for: it pushes over OTLP/HTTP when they name an
// endpoint for metrics, and offers the metrics for pull when cfg.Pull is
// set. With neither, or with OTEL_SDK_DISABLED true, it exports nothing
// and starts no goroutine. The caller calls Shutdown.
func NewSextant(ctx context.Context, cfg SextantConfig) (*Sextant, error) {
	env, err := readEnv(cfg.Getenv)
	if err != nil {
		return nil, err
	}
	if !env.named {
		env.service = sextantService
	}
	t := newTelemetry(Config{Logger: cfg.Logger}, env)
	s := &Sextant{t: t, cost: map[string]projectCost{}}
	s.projects.max = positiveOr(cfg.Projects, DefaultSextantProjects)
	s.models.max = positiveOr(cfg.Models, DefaultSextantModels)
	if env.disabled {
		if env.endpoint != "" || cfg.Pull {
			t.log.Info("telemetry is off: OTEL_SDK_DISABLED is true", "event", logs.TelemetryOff)
		}
		return s, nil
	}
	var readers []sdkmetric.Reader
	if env.endpoint != "" {
		opts := []otlpmetrichttp.Option{
			otlpmetrichttp.WithEndpointURL(env.endpoint),
			otlpmetrichttp.WithTimeout(env.requestTimeout),
			// As ariesd's: the next export carries what a failed one did.
			otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{Enabled: false}),
		}
		if len(env.headers) > 0 {
			opts = append(opts, otlpmetrichttp.WithHeaders(env.headers))
		}
		exp, err := otlpmetrichttp.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("telemetry: %s; fix: correct the OTEL_EXPORTER_OTLP_* variables in sextant's environment, or unset %s to push nothing",
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
		return s, nil
	}
	opts := []sdkmetric.Option{sdkmetric.WithResource(resource.NewSchemaless(env.resource(cfg.Version)...))}
	for _, r := range readers {
		opts = append(opts, sdkmetric.WithReader(r))
	}
	mp := sdkmetric.NewMeterProvider(opts...)
	meter := mp.Meter(sextantScope)
	errs, err := meter.Int64Counter(SextantScrapeErrorsName, metric.WithDescription("Sextant's failures to read the server, by project, op and code."))
	var gaugeErr error
	if err == nil {
		_, gaugeErr = meter.Float64ObservableGauge(SextantCostName,
			metric.WithDescription("The list-price cost of the tokens of a project's window, by model, as of the last poll."),
			metric.WithFloat64Callback(s.observeCost))
	}
	if err := errors.Join(err, gaugeErr); err != nil {
		return nil, errors.Join(fmt.Errorf("telemetry: make Sextant's instruments: %w", err), mp.Shutdown(context.WithoutCancel(ctx)))
	}
	s.mp, s.errs = mp, errs
	// Sextant counts no failed export, having no instrument of ariesd's
	// to count it on; the error log has each, sampled. Marking t built
	// keeps it from holding the early ones for a count that never comes.
	t.builtInstruments()
	setSDKLogging(t.errs)
	t.log.Info("telemetry on", "event", logs.TelemetryOn, "push", shown(env.endpoint), "push_interval", env.interval.String(),
		"push_headers", len(env.headers), "pull", cfg.Pull, "service", env.service)
	return s, nil
}

// positiveOr is n, or def when n is not positive.
func positiveOr(n, def int) int {
	if n <= 0 {
		return def
	}
	return n
}

// Enabled reports whether s exports anything.
func (s *Sextant) Enabled() bool { return s != nil && s.mp != nil }

// SetCost replaces project's figures with costs, the last poll's; nil
// clears them, so that a project Sextant cannot read shows no figure
// rather than an old one. A project that is not a project's id, or one
// past the cap, is recorded as other; a model of the wrong shape, or one
// past the cap, as (other); figures that share their values are summed.
func (s *Sextant) SetCost(project string, costs []SextantCost) {
	if !s.Enabled() {
		return
	}
	p := Other
	if s.projects.admit(project) {
		p = project
	}
	byModel := map[string]float64{}
	for _, c := range costs {
		m := ModelOther
		if c.Model != ModelOther && proto.UsageModel.MatchString(c.Model) && s.models.take(c.Model) {
			m = c.Model
		}
		byModel[m] += c.USD
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Projects that are other share their series, and each keeps its own
	// figures, so that one's poll replaces only its own (observeCost sums
	// them).
	if len(byModel) == 0 {
		delete(s.cost, project)
		return
	}
	s.cost[project] = projectCost{label: p, byModel: byModel}
}

// observeCost reports every project's last figures.
func (s *Sextant) observeCost(_ context.Context, o metric.Float64Observer) error {
	s.mu.Lock()
	sums := map[[2]string]float64{}
	for _, pc := range s.cost {
		for m, usd := range pc.byModel {
			sums[[2]string{pc.label, m}] += usd
		}
	}
	s.mu.Unlock()
	for k, usd := range sums {
		o.Observe(usd, metric.WithAttributes(attribute.String(KeyProject, k[0]), attribute.String(KeyModel, k[1])))
	}
	return nil
}

// ScrapeError counts one failure to read project: op is one of the
// SextantOp values, or is recorded as unknown, and code is a protocol
// error code, or is recorded as other.
func (s *Sextant) ScrapeError(project, op, code string) {
	if !s.Enabled() {
		return
	}
	p := Other
	if s.projects.admit(project) {
		p = project
	}
	if !slices.Contains(sextantOps, op) {
		op = Unknown
	}
	if !slices.Contains(sextantCodes, code) {
		code = Other
	}
	s.errs.Add(context.Background(), 1, metric.WithAttributes(attribute.String(KeyProject, p), attribute.String(KeyOp, op), attribute.String("code", code)))
}

// Serve answers on l as [Telemetry.Serve] does: /metrics, when s offers
// them, /healthz and /readyz.
func (s *Sextant) Serve(ctx context.Context, l net.Listener, o ServeOptions) error {
	var t *Telemetry
	if s != nil {
		t = s.t
	}
	return t.Serve(ctx, l, o)
}

// Shutdown makes the last export and stops the exporters, giving up when
// ctx ends. Give it a deadline.
func (s *Sextant) Shutdown(ctx context.Context) error {
	if !s.Enabled() {
		return nil
	}
	if err := s.mp.Shutdown(ctx); err != nil {
		return fmt.Errorf("telemetry: stop the exporters: %s", s.t.errs.redact(err.Error()))
	}
	return nil
}
