// Package telemetry is starfixd's metrics and traces, and the only
// package that imports the OpenTelemetry SDK, its exporters or the
// Prometheus client (design §16.2, decision 28). The server, the store's
// hooks and the commands see what is declared here and nothing of the
// SDK, so the SDK can be replaced, or left out of a test, in this one
// place. `sfx` links none of it; a test fails if it ever does.
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
// It also starts spans (trace.go; design §16.4), exported over OTLP/HTTP
// when the variables name an endpoint for traces: [Telemetry.StartRequest]
// for a request, under the caller's trace context when it brought one,
// and [Telemetry.Start] for the work beneath it and for background work.
// A [Span] takes a name from a fixed list or the list of ops, attributes
// from a fixed set of keys, and a status whose description is a code,
// and nothing else: no text a person or an agent wrote can reach a span,
// whatever a caller passes. Who a span is about is said as
// `telemetry: principals:` allows (people.go): by default a stand-in
// derived from a key this package keeps in a 0600 file.
//
// A nil *Telemetry, and one made with nothing configured, are usable and
// do nothing: every method returns at once, and a span is the zero Span.
// The package reads no environment variable and writes to no stream of
// the process itself: the main package hands it the environment's lookup
// and the logger. The SDK reads a few variables this package does not
// parse (TLS files, compression, a span's limits) from the process
// environment on its own.
//
// Recording never waits for an export. A measurement updates memory
// under the SDK's per-instrument aggregation lock, which a collection
// holds only while it copies the numbers out; the network call of an
// export, and the write of a scrape's response, happen after that, with
// no lock an instrument takes. Ending a span puts it in a bounded queue,
// or counts it as dropped when the queue is full; a goroutine of the
// SDK's exports the queue in batches.
//
// [Manual] reads the instruments and the spans in process, for tests.
package telemetry
