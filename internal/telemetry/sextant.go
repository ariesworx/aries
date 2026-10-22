package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"sync"

	"github.com/ariesworx/aries/internal/lazyregexp"
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
	SextantAccountCostName  = "sextant.account.cost.usd"
	SextantHoursName        = "sextant.hours"
	SextantTokensName       = "sextant.tokens"
	SextantUnpricedName     = "sextant.cost.unpriced.tokens"
	SextantScrapeErrorsName = "sextant.scrape.errors"
)

// KeyAccount is the attribute an account's figures carry. Its values are
// capped by [DefaultSextantAccounts] and shaped by accountShape; any
// other is [AccountOther].
const (
	KeyAccount   = "account"
	AccountOther = "(other)"
)

// accountShape is what an account's code name must look like to be a
// label's value; the server holds accounts to little, so Sextant holds
// them to this before one reaches a series (rule 2).
var accountShape = lazyregexp.New(`^[A-Za-z0-9][A-Za-z0-9_.:/+@-]{0,63}$`)

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
	// DefaultSextantAccounts caps the account label, as the models' cap
	// does the model label.
	DefaultSextantAccounts = 20
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
	// Accounts caps how many distinct accounts the account attribute
	// takes; one more is recorded as (other). Zero takes the default.
	Accounts int
}

// SextantAccount is one account's cost in a project's window in US
// dollars at list price, and the hours people logged against it.
type SextantAccount struct {
	Account string
	USD     float64
	Hours   float64
}

// SextantTokens is one model's tokens in a project's window, by kind,
// and Unpriced, the part of them no price covers.
type SextantTokens struct {
	Model                                string
	Input, Output, CacheWrite, CacheRead int64
	Unpriced                             int64
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
	accounts projectSet

	mu sync.Mutex
	// cost is each project's last figures, by the project's id, which
	// the gauge's callback reads at each collection.
	cost map[string]projectCost
	// byAccount and byTokens are the same for the account and token
	// figures.
	byAccount map[string]projectAccounts
	byTokens  map[string]projectTokens
}

// projectAccounts is a project's last figures by account, under the
// values it and they were admitted as.
type projectAccounts struct {
	label string
	usd   map[string]float64
	hours map[string]float64
}

// projectTokens is a project's last token figures by model.
type projectTokens struct {
	label    string
	byKind   map[[2]string]int64 // model, kind
	unpriced map[string]int64    // model
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
	s := &Sextant{t: t, cost: map[string]projectCost{}, byAccount: map[string]projectAccounts{}, byTokens: map[string]projectTokens{}}
	s.projects.max = positiveOr(cfg.Projects, DefaultSextantProjects)
	s.models.max = positiveOr(cfg.Models, DefaultSextantModels)
	s.accounts.max = positiveOr(cfg.Accounts, DefaultSextantAccounts)
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
	if err == nil && gaugeErr == nil {
		_, gaugeErr = meter.Float64ObservableGauge(SextantAccountCostName,
			metric.WithDescription("The list-price cost of the tokens of a project's window, by account, as of the last poll."),
			metric.WithFloat64Callback(s.observeAccountCost))
	}
	if err == nil && gaugeErr == nil {
		_, gaugeErr = meter.Float64ObservableGauge(SextantHoursName,
			metric.WithDescription("The hours people logged against a project's accounts in its window, as of the last poll."),
			metric.WithFloat64Callback(s.observeHours))
	}
	if err == nil && gaugeErr == nil {
		_, gaugeErr = meter.Int64ObservableGauge(SextantTokensName,
			metric.WithDescription("The tokens of a project's window, by model and kind, as of the last poll."),
			metric.WithInt64Callback(s.observeTokens))
	}
	if err == nil && gaugeErr == nil {
		_, gaugeErr = meter.Int64ObservableGauge(SextantUnpricedName,
			metric.WithDescription("The tokens of a project's window that no price covers, by model, as of the last poll: a missing price is never $0."),
			metric.WithInt64Callback(s.observeUnpriced))
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

// projectLabel is the value project takes as a label: its id if it is a
// project's id within the cap, and otherwise other.
func (s *Sextant) projectLabel(project string) string {
	if s.projects.admit(project) {
		return project
	}
	return Other
}

// SetAccounts replaces project's figures by account with accounts, the
// last poll's; nil clears them. An account of the wrong shape, or past
// the cap, is recorded as (other), and figures that share their values
// are summed.
func (s *Sextant) SetAccounts(project string, accounts []SextantAccount) {
	if !s.Enabled() {
		return
	}
	pa := projectAccounts{label: s.projectLabel(project), usd: map[string]float64{}, hours: map[string]float64{}}
	for _, a := range accounts {
		name := AccountOther
		if a.Account != AccountOther && accountShape.MatchString(a.Account) && s.accounts.take(a.Account) {
			name = a.Account
		}
		pa.usd[name] += a.USD
		pa.hours[name] += a.Hours
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(accounts) == 0 {
		delete(s.byAccount, project)
		return
	}
	s.byAccount[project] = pa
}

// SetTokens replaces project's token figures by model with tokens, the
// last poll's; nil clears them. A model of the wrong shape, or past the
// cap, is recorded as (other), and figures that share their values are
// summed.
func (s *Sextant) SetTokens(project string, tokens []SextantTokens) {
	if !s.Enabled() {
		return
	}
	pt := projectTokens{label: s.projectLabel(project), byKind: map[[2]string]int64{}, unpriced: map[string]int64{}}
	for _, t := range tokens {
		m := ModelOther
		if t.Model != ModelOther && proto.UsageModel.MatchString(t.Model) && s.models.take(t.Model) {
			m = t.Model
		}
		pt.byKind[[2]string{m, TokenInput}] += t.Input
		pt.byKind[[2]string{m, TokenOutput}] += t.Output
		pt.byKind[[2]string{m, TokenCacheWrite}] += t.CacheWrite
		pt.byKind[[2]string{m, TokenCacheRead}] += t.CacheRead
		pt.unpriced[m] += t.Unpriced
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(tokens) == 0 {
		delete(s.byTokens, project)
		return
	}
	s.byTokens[project] = pt
}

// observeAccountCost reports every project's last cost by account.
func (s *Sextant) observeAccountCost(_ context.Context, o metric.Float64Observer) error {
	s.observeAccounts(o, func(pa projectAccounts) map[string]float64 { return pa.usd })
	return nil
}

// observeHours reports every project's last logged hours by account.
func (s *Sextant) observeHours(_ context.Context, o metric.Float64Observer) error {
	s.observeAccounts(o, func(pa projectAccounts) map[string]float64 { return pa.hours })
	return nil
}

func (s *Sextant) observeAccounts(o metric.Float64Observer, pick func(projectAccounts) map[string]float64) {
	s.mu.Lock()
	sums := map[[2]string]float64{}
	for _, pa := range s.byAccount {
		for a, v := range pick(pa) {
			sums[[2]string{pa.label, a}] += v
		}
	}
	s.mu.Unlock()
	for k, v := range sums {
		o.Observe(v, metric.WithAttributes(attribute.String(KeyProject, k[0]), attribute.String(KeyAccount, k[1])))
	}
}

// observeTokens reports every project's last tokens by model and kind.
func (s *Sextant) observeTokens(_ context.Context, o metric.Int64Observer) error {
	s.mu.Lock()
	sums := map[[3]string]int64{}
	for _, pt := range s.byTokens {
		for k, n := range pt.byKind {
			sums[[3]string{pt.label, k[0], k[1]}] += n
		}
	}
	s.mu.Unlock()
	for k, n := range sums {
		o.Observe(n, metric.WithAttributes(attribute.String(KeyProject, k[0]), attribute.String(KeyModel, k[1]), attribute.String(KeyKind, k[2])))
	}
	return nil
}

// observeUnpriced reports every project's last unpriced tokens by model.
func (s *Sextant) observeUnpriced(_ context.Context, o metric.Int64Observer) error {
	s.mu.Lock()
	sums := map[[2]string]int64{}
	for _, pt := range s.byTokens {
		for m, n := range pt.unpriced {
			sums[[2]string{pt.label, m}] += n
		}
	}
	s.mu.Unlock()
	for k, n := range sums {
		o.Observe(n, metric.WithAttributes(attribute.String(KeyProject, k[0]), attribute.String(KeyModel, k[1])))
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
