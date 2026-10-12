// Package telemetry is starfixd's metrics, and the only package that
// imports the OpenTelemetry SDK, its exporters or the Prometheus client
// (design §16.2, decision 28). The server, the store's observer and the
// commands see what is declared here and nothing of the SDK, so the SDK
// can be replaced, or left out of a test, in this one place. `sfx` links
// none of it; a test fails if it ever does.
//
// A [Telemetry] records measurements on the instruments of
// instruments.go, each with a fixed set of attribute keys whose values
// come from fixed lists, so the number of series is bounded by
// construction (AGENTS.md rule 18). It exports them two ways, each on
// its own: pushed over OTLP/HTTP when the standard OTEL_* variables name
// an endpoint, and offered for pull in Prometheus's text format on the
// loopback listener ([Listen], [Telemetry.Serve]), which also answers
// /healthz and /readyz.
//
// A nil *Telemetry, and one made with nothing configured, are usable and
// do nothing: every method returns at once. The package reads no
// environment variable and writes to no stream of the process itself:
// the main package hands it the environment's lookup and the logger. The
// SDK reads a few variables this package does not parse (TLS files,
// compression) from the process environment on its own.
//
// Recording never waits for an export. A measurement updates memory
// under the SDK's per-instrument aggregation lock, which a collection
// holds only while it copies the numbers out; the network call of an
// export, and the write of a scrape's response, happen after that, with
// no lock an instrument takes.
//
// Traces are slice O2: [Telemetry.Start] is where a span will begin, and
// returns a span that does nothing until then.
package telemetry
