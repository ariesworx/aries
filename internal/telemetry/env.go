package telemetry

import (
	"fmt"
	"maps"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
)

// The standard OpenTelemetry variables this package reads, through
// Config.Getenv. A signal's own variable wins over the general one.
// The headers' variables may hold a token: they are read from the
// environment only, and their values are never logged.
const (
	envDisabled        = "OTEL_SDK_DISABLED"
	envMetricsExporter = "OTEL_METRICS_EXPORTER"
	envTracesExporter  = "OTEL_TRACES_EXPORTER"
	envEndpoint        = "OTEL_EXPORTER_OTLP_ENDPOINT"
	envMetricsEndpoint = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"
	envTracesEndpoint  = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	envProtocol        = "OTEL_EXPORTER_OTLP_PROTOCOL"
	envMetricsProtocol = "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"
	envTracesProtocol  = "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"
	envHeaders         = "OTEL_EXPORTER_OTLP_HEADERS"
	envMetricsHeaders  = "OTEL_EXPORTER_OTLP_METRICS_HEADERS"
	envTracesHeaders   = "OTEL_EXPORTER_OTLP_TRACES_HEADERS"
	envTimeout         = "OTEL_EXPORTER_OTLP_TIMEOUT"
	envMetricsTimeout  = "OTEL_EXPORTER_OTLP_METRICS_TIMEOUT"
	envTracesTimeout   = "OTEL_EXPORTER_OTLP_TRACES_TIMEOUT"
	envExportInterval  = "OTEL_METRIC_EXPORT_INTERVAL"
	envExportTimeout   = "OTEL_METRIC_EXPORT_TIMEOUT"
	envSampler         = "OTEL_TRACES_SAMPLER"
	envSamplerArg      = "OTEL_TRACES_SAMPLER_ARG"
	envBatchDelay      = "OTEL_BSP_SCHEDULE_DELAY"
	envBatchTimeout    = "OTEL_BSP_EXPORT_TIMEOUT"
	envBatchQueue      = "OTEL_BSP_MAX_QUEUE_SIZE"
	envBatchSize       = "OTEL_BSP_MAX_EXPORT_BATCH_SIZE"
	envServiceName     = "OTEL_SERVICE_NAME"
	envResourceAttrs   = "OTEL_RESOURCE_ATTRIBUTES"
)

// The samplers OTEL_TRACES_SAMPLER may name: the SDK's own. The default
// follows the caller's decision, and samples a trace that starts here.
const (
	samplerAlwaysOn    = "always_on"
	samplerAlwaysOff   = "always_off"
	samplerRatio       = "traceidratio"
	samplerParentOn    = "parentbased_always_on"
	samplerParentOff   = "parentbased_always_off"
	samplerParentRatio = "parentbased_traceidratio"
	samplerDefault     = samplerParentOn
)

// How spans are batched unless the OTEL_BSP_* variables say otherwise:
// the SDK's defaults. maxQueue bounds what OTEL_BSP_MAX_QUEUE_SIZE may
// ask for, since the queue is memory a dead collector fills.
const (
	defaultQueue      = 2048
	defaultBatch      = 512
	defaultBatchDelay = 5 * time.Second
	maxQueue          = 1 << 16
)

// What the variables default to and may hold, and the resource's keys.
const (
	defaultService      = "starfixd"
	keyServiceName      = "service.name"
	keyServiceVersion   = "service.version"
	keyServiceInstance  = "service.instance.id"
	protocolHTTP        = "http/protobuf"
	metricsPath         = "/v1/metrics"
	tracesPath          = "/v1/traces"
	defaultInterval     = 60 * time.Second
	defaultExportTime   = 30 * time.Second
	defaultRequestTime  = 10 * time.Second
	maxResourceAttrs    = 64
	exporterOTLP        = "otlp"
	exporterNone        = "none"
	fixUnsetOrCorrectIt = "correct it in starfixd's environment (the unit's Environment= or EnvironmentFile=), or unset it"
)

// envConfig is what the OTEL_* variables configure.
type envConfig struct {
	// disabled is OTEL_SDK_DISABLED: nothing is pushed or offered.
	disabled bool
	// endpoint is the URL metrics are posted to, or empty to push none.
	endpoint string
	// headers go with every export of metrics; a value may be a token.
	headers map[string]string
	// interval is how often metrics are pushed; exportTimeout bounds one
	// collection and export, and requestTimeout its HTTP request.
	interval, exportTimeout, requestTimeout time.Duration
	// traces is where and how spans are exported.
	traces traceEnv
	// service is the resource's service.name, and named says
	// OTEL_SERVICE_NAME set it; attrs are the resource attributes of
	// OTEL_RESOURCE_ATTRIBUTES, in order.
	service string
	named   bool
	attrs   [][2]string
}

// traceEnv is what the variables configure of traces.
type traceEnv struct {
	// endpoint is the URL spans are posted to, or empty to export none.
	endpoint string
	// headers go with every export of spans; a value may be a token.
	headers map[string]string
	// requestTimeout bounds one export's HTTP request.
	requestTimeout time.Duration
	// sampler is one of the sampler names above, and ratio the argument
	// of the two that take one.
	sampler string
	ratio   float64
	// queue is how many ended spans may wait for export, batch how many
	// go in one export, delay how long a span waits at most for its
	// batch to fill, and exportTimeout how long one export may take,
	// its retries included.
	queue, batch         int
	delay, exportTimeout time.Duration
}

// readEnv reads the variables above. A nil getenv reads none. Each
// refusal names the variable and a fix, and never repeats a header's
// value.
func readEnv(getenv func(string) string) (envConfig, error) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	get := func(name string) string { return strings.TrimSpace(getenv(name)) }
	first := func(names ...string) (name, value string) {
		for _, n := range names {
			if v := get(n); v != "" {
				return n, v
			}
		}
		return names[0], ""
	}
	c := envConfig{service: defaultService, interval: defaultInterval, exportTimeout: defaultExportTime, requestTimeout: defaultRequestTime,
		traces: traceEnv{requestTimeout: defaultRequestTime, sampler: samplerDefault, ratio: 1,
			queue: defaultQueue, batch: defaultBatch, delay: defaultBatchDelay, exportTimeout: defaultExportTime}}
	c.disabled = strings.EqualFold(get(envDisabled), "true")
	if v := get(envServiceName); v != "" {
		c.service, c.named = v, true
	}
	var err error
	if c.attrs, err = pairs(envResourceAttrs, get(envResourceAttrs), false); err != nil {
		return c, err
	}
	if len(c.attrs) > maxResourceAttrs {
		return c, fmt.Errorf("telemetry: %s holds %d attributes, more than %d; fix: list fewer", envResourceAttrs, len(c.attrs), maxResourceAttrs)
	}
	for _, d := range []struct {
		dst   *time.Duration
		names []string
	}{
		{&c.interval, []string{envExportInterval}},
		{&c.exportTimeout, []string{envExportTimeout}},
		{&c.requestTimeout, []string{envMetricsTimeout, envTimeout}},
		{&c.traces.requestTimeout, []string{envTracesTimeout, envTimeout}},
		{&c.traces.delay, []string{envBatchDelay}},
		{&c.traces.exportTimeout, []string{envBatchTimeout}},
	} {
		name, v := first(d.names...)
		if v == "" {
			continue
		}
		ms, err := strconv.Atoi(v)
		if err != nil || ms <= 0 {
			return c, fmt.Errorf("telemetry: %s is %q, not a positive number of milliseconds; fix: %s", name, v, fixUnsetOrCorrectIt)
		}
		*d.dst = time.Duration(ms) * time.Millisecond
	}
	if err := c.traces.readBatch(get); err != nil {
		return c, err
	}
	if err := c.traces.readSampler(get); err != nil {
		return c, err
	}
	metrics, traces := true, true
	switch exp := get(envMetricsExporter); exp {
	case "", exporterOTLP:
	case exporterNone:
		metrics = false
	default:
		return c, fmt.Errorf("telemetry: %s is %q, and starfixd pushes metrics with otlp only; fix: set it to otlp or none; to have Prometheus scrape starfixd, set telemetry: listen: in its config file instead",
			envMetricsExporter, exp)
	}
	switch exp := get(envTracesExporter); exp {
	case "", exporterOTLP:
	case exporterNone:
		traces = false
	default:
		return c, fmt.Errorf("telemetry: %s is %q, and starfixd exports traces with otlp only; fix: set it to otlp or none", envTracesExporter, exp)
	}
	// endpoint reads one signal's endpoint and headers: the signal's own
	// variables, then the general ones.
	endpoint := func(own, ownProtocol, ownHeaders, path string) (string, map[string]string, error) {
		if name, v := first(ownProtocol, envProtocol); v != "" && v != protocolHTTP {
			return "", nil, fmt.Errorf("telemetry: %s is %q, and starfixd exports OTLP over HTTP only; fix: set it to %s and point the endpoint at the collector's HTTP port, usually 4318",
				name, v, protocolHTTP)
		}
		name, raw := first(own, envEndpoint)
		if raw == "" {
			return "", nil, nil
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			// The value is not repeated: a URL may hold a password.
			return "", nil, fmt.Errorf("telemetry: %s is not an http or https URL; fix: set it to the collector's address, such as http://127.0.0.1:4318", name)
		}
		if name == envEndpoint {
			// The general variable names the collector; each signal has
			// its own path under it.
			u.Path = strings.TrimSuffix(u.Path, "/") + path
		}
		headers := map[string]string{}
		for _, n := range []string{envHeaders, ownHeaders} {
			hs, err := pairs(n, get(n), true)
			if err != nil {
				return "", nil, err
			}
			for _, h := range hs {
				headers[h[0]] = h[1]
			}
		}
		return u.String(), headers, nil
	}
	if metrics {
		if c.endpoint, c.headers, err = endpoint(envMetricsEndpoint, envMetricsProtocol, envMetricsHeaders, metricsPath); err != nil {
			return c, err
		}
	}
	if traces {
		if c.traces.endpoint, c.traces.headers, err = endpoint(envTracesEndpoint, envTracesProtocol, envTracesHeaders, tracesPath); err != nil {
			return c, err
		}
	}
	return c, nil
}

// readBatch reads the queue's and the batch's sizes. A queue is refused
// past maxQueue and a batch past its queue; a batch that was not set is
// at most its queue, as in the SDK.
func (te *traceEnv) readBatch(get func(string) string) error {
	size := func(name string, dst *int, most int) (set bool, err error) {
		v := get(name)
		if v == "" {
			return false, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > most {
			return false, fmt.Errorf("telemetry: %s is %q, not a number from 1 to %d; fix: %s", name, v, most, fixUnsetOrCorrectIt)
		}
		*dst = n
		return true, nil
	}
	if _, err := size(envBatchQueue, &te.queue, maxQueue); err != nil {
		return err
	}
	set, err := size(envBatchSize, &te.batch, te.queue)
	if err != nil {
		return err
	}
	if !set {
		te.batch = min(te.batch, te.queue)
	}
	return nil
}

// readSampler reads OTEL_TRACES_SAMPLER and its argument.
func (te *traceEnv) readSampler(get func(string) string) error {
	switch v := get(envSampler); v {
	case "":
	case samplerAlwaysOn, samplerAlwaysOff, samplerRatio, samplerParentOn, samplerParentOff, samplerParentRatio:
		te.sampler = v
	default:
		return fmt.Errorf("telemetry: %s is %q, and starfixd has the samplers %s; fix: %s", envSampler, v,
			strings.Join([]string{samplerAlwaysOn, samplerAlwaysOff, samplerRatio, samplerParentOn, samplerParentOff, samplerParentRatio}, ", "),
			fixUnsetOrCorrectIt)
	}
	if te.sampler != samplerRatio && te.sampler != samplerParentRatio {
		return nil
	}
	if v := get(envSamplerArg); v != "" {
		r, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(r) || r < 0 || r > 1 {
			return fmt.Errorf("telemetry: %s is %q, and the share of traces to sample is a number from 0 to 1; fix: %s", envSamplerArg, v, fixUnsetOrCorrectIt)
		}
		te.ratio = r
	}
	return nil
}

// pairs parses a variable of the form k1=v1,k2=v2, percent-decoding
// each part, as the OpenTelemetry specification gives it. With secret
// set, a refusal never repeats any of the value.
func pairs(name, v string, secret bool) ([][2]string, error) {
	if v == "" {
		return nil, nil
	}
	var out [][2]string
	for part := range strings.SplitSeq(v, ",") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		k, val, ok := strings.Cut(part, "=")
		key, kerr := url.PathUnescape(strings.TrimSpace(k))
		value, verr := url.PathUnescape(strings.TrimSpace(val))
		if !ok || key == "" || kerr != nil || verr != nil {
			if secret {
				return nil, fmt.Errorf("telemetry: %s is not a list of name=value pairs separated by commas; fix: %s (its value is not shown, since it may hold a token)", name, fixUnsetOrCorrectIt)
			}
			return nil, fmt.Errorf("telemetry: %s has the part %q, which is not name=value; fix: %s", name, part, fixUnsetOrCorrectIt)
		}
		out = append(out, [2]string{key, value})
	}
	return out, nil
}

// resource returns the resource's attributes: those of
// OTEL_RESOURCE_ATTRIBUTES, in order, with service.name (which
// OTEL_SERVICE_NAME decides when it is set), service.version (always the
// build's) and service.instance.id (a random UUID unless the environment
// gives one). A Telemetry calls it once, so that its metrics and its
// spans carry the one instance id.
func (c envConfig) resource(version string) []attribute.KeyValue {
	m := map[string]string{}
	var order []string
	set := func(k, v string, override bool) {
		if _, ok := m[k]; !ok {
			order = append(order, k)
		} else if !override {
			return
		}
		m[k] = v
	}
	for _, a := range c.attrs {
		set(a[0], a[1], true)
	}
	set(keyServiceName, c.service, c.named)
	set(keyServiceVersion, version, true)
	set(keyServiceInstance, instanceID(), false)
	out := make([]attribute.KeyValue, 0, len(order))
	for _, k := range order {
		out = append(out, attribute.String(k, m[k]))
	}
	return out
}

// shown is endpoint as a log line may show it: without the user,
// password or query, any of which may hold a credential; and "off" for
// none.
func shown(endpoint string) string {
	if endpoint == "" {
		return "off"
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "set"
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return u.String()
}

// secrets are the strings no log line may hold: every header's value,
// and each endpoint's password and query.
func (c envConfig) secrets() []string {
	var out []string
	for _, hs := range []map[string]string{c.headers, c.traces.headers} {
		for v := range maps.Values(hs) {
			out = append(out, v)
		}
	}
	for _, e := range []string{c.endpoint, c.traces.endpoint} {
		if u, err := url.Parse(e); err == nil {
			if p, ok := u.User.Password(); ok {
				out = append(out, p)
			}
			if u.RawQuery != "" {
				out = append(out, u.RawQuery)
			}
		}
	}
	return out
}
