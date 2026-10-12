package telemetry

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
)

// The standard OpenTelemetry variables this package reads, through
// Config.Getenv. A signal's own variable wins over the general one.
// envHeaders and envMetricsHeaders may hold a token: they are read from
// the environment only, and their values are never logged.
const (
	envDisabled        = "OTEL_SDK_DISABLED"
	envMetricsExporter = "OTEL_METRICS_EXPORTER"
	envEndpoint        = "OTEL_EXPORTER_OTLP_ENDPOINT"
	envMetricsEndpoint = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"
	envProtocol        = "OTEL_EXPORTER_OTLP_PROTOCOL"
	envMetricsProtocol = "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"
	envHeaders         = "OTEL_EXPORTER_OTLP_HEADERS"
	envMetricsHeaders  = "OTEL_EXPORTER_OTLP_METRICS_HEADERS"
	envTimeout         = "OTEL_EXPORTER_OTLP_TIMEOUT"
	envMetricsTimeout  = "OTEL_EXPORTER_OTLP_METRICS_TIMEOUT"
	envExportInterval  = "OTEL_METRIC_EXPORT_INTERVAL"
	envExportTimeout   = "OTEL_METRIC_EXPORT_TIMEOUT"
	envServiceName     = "OTEL_SERVICE_NAME"
	envResourceAttrs   = "OTEL_RESOURCE_ATTRIBUTES"
)

// What the variables default to and may hold, and the resource's keys.
const (
	defaultService      = "starfixd"
	keyServiceName      = "service.name"
	keyServiceVersion   = "service.version"
	keyServiceInstance  = "service.instance.id"
	protocolHTTP        = "http/protobuf"
	metricsPath         = "/v1/metrics"
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
	// headers go with every export; a value may be a token.
	headers map[string]string
	// interval is how often metrics are pushed; exportTimeout bounds one
	// collection and export, and requestTimeout its HTTP request.
	interval, exportTimeout, requestTimeout time.Duration
	// service is the resource's service.name, and named says
	// OTEL_SERVICE_NAME set it; attrs are the resource attributes of
	// OTEL_RESOURCE_ATTRIBUTES, in order.
	service string
	named   bool
	attrs   [][2]string
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
	c := envConfig{service: defaultService, interval: defaultInterval, exportTimeout: defaultExportTime, requestTimeout: defaultRequestTime}
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
	switch exp := get(envMetricsExporter); exp {
	case "", exporterOTLP:
	case exporterNone:
		return c, nil
	default:
		return c, fmt.Errorf("telemetry: %s is %q, and starfixd pushes metrics with otlp only; fix: set it to otlp or none; to have Prometheus scrape starfixd, set telemetry: listen: in its config file instead",
			envMetricsExporter, exp)
	}
	if name, v := first(envMetricsProtocol, envProtocol); v != "" && v != protocolHTTP {
		return c, fmt.Errorf("telemetry: %s is %q, and starfixd exports OTLP over HTTP only; fix: set it to %s and point the endpoint at the collector's HTTP port, usually 4318",
			name, v, protocolHTTP)
	}
	name, raw := first(envMetricsEndpoint, envEndpoint)
	if raw == "" {
		return c, nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		// The value is not repeated: a URL may hold a password.
		return c, fmt.Errorf("telemetry: %s is not an http or https URL; fix: set it to the collector's address, such as http://127.0.0.1:4318", name)
	}
	if name == envEndpoint {
		// The general variable names the collector; each signal has its
		// own path under it.
		u.Path = strings.TrimSuffix(u.Path, "/") + metricsPath
	}
	c.endpoint = u.String()
	c.headers = map[string]string{}
	for _, n := range []string{envHeaders, envMetricsHeaders} {
		hs, err := pairs(n, get(n), true)
		if err != nil {
			return c, err
		}
		for _, h := range hs {
			c.headers[h[0]] = h[1]
		}
	}
	return c, nil
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
// gives one).
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

// shownEndpoint is the endpoint as a log line may show it: without the
// user, password or query, any of which may hold a credential.
func (c envConfig) shownEndpoint() string {
	u, err := url.Parse(c.endpoint)
	if err != nil {
		return "set"
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return u.String()
}

// secrets are the strings no log line may hold: every header's value,
// and the endpoint's password and query.
func (c envConfig) secrets() []string {
	var out []string
	for _, v := range c.headers {
		out = append(out, v)
	}
	if u, err := url.Parse(c.endpoint); err == nil {
		if p, ok := u.User.Password(); ok {
			out = append(out, p)
		}
		if u.RawQuery != "" {
			out = append(out, u.RawQuery)
		}
	}
	return out
}
