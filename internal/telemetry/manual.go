package telemetry

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Manual reads a Telemetry's instruments and spans in process, on
// demand: what a test asserts values and structure with, and what a soak
// compares against the event log. It exports nothing.
type Manual struct {
	r     *sdkmetric.ManualReader
	spans recorder
	logs  logRecorder
}

// NewManual makes a Telemetry that records as [New]'s does, with the
// same instruments, attributes, buckets and spans, and is read through
// the Manual. cfg.Getenv is read for the resource and the sampler;
// cfg.Pull also offers the Prometheus handler; and the stand-in key is
// cfg.KeyFile's, made if it is not there, as when spans are exported,
// or, with no file named, one kept in memory for the test's life.
// The process's SDK error handler is left alone.
func NewManual(cfg Config) (*Telemetry, *Manual, error) {
	env, err := readEnv(cfg.Getenv)
	if err != nil {
		return nil, nil, err
	}
	t := newTelemetry(cfg, env)
	if t.who, err = openPeople(cfg, true, true, t.log); err != nil {
		return nil, nil, err
	}
	m := &Manual{r: sdkmetric.NewManualReader()}
	readers := []sdkmetric.Reader{m.r}
	if cfg.Pull {
		pull, reader, err := newPull(t.errs)
		if err != nil {
			return nil, nil, err
		}
		t.pull = pull
		readers = append(readers, reader)
	}
	res := resource.NewSchemaless(env.resource(cfg.Version)...)
	if err := t.build(res, readers...); err != nil {
		return nil, nil, err
	}
	t.trace(res, env.traces, &m.spans)
	t.logging(res, &m.logs)
	return t, m, nil
}

// logRecorder keeps the log records that were exported, in order.
type logRecorder struct {
	mu   sync.Mutex
	recs []sdklog.Record
}

func (*logRecorder) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }

func (r *logRecorder) OnEmit(_ context.Context, rec *sdklog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, rec.Clone())
	return nil
}

func (*logRecorder) Shutdown(context.Context) error   { return nil }
func (*logRecorder) ForceFlush(context.Context) error { return nil }

// LogData is one log record as it would leave the machine.
type LogData struct {
	// Body is the line's message, Event its event's name, and Severity
	// its level: DEBUG, INFO, WARN or ERROR.
	Body, Event, Severity string
	// TraceID and SpanID are the ids of the span the line was logged
	// under, in hex, or empty.
	TraceID, SpanID string
	// Attrs are the record's attributes.
	Attrs map[string]string
	// Time is the line's time.
	Time time.Time
}

// Logs returns the log records exported so far, in order: every record
// given to [Telemetry.LogHandler], as [New]'s Telemetry would send it.
func (m *Manual) Logs() []LogData {
	m.logs.mu.Lock()
	defer m.logs.mu.Unlock()
	out := make([]LogData, 0, len(m.logs.recs))
	for _, r := range m.logs.recs {
		d := LogData{Body: r.Body().AsString(), Event: r.EventName(), Severity: r.SeverityText(), Time: r.Timestamp(), Attrs: map[string]string{}}
		if id := r.TraceID(); id.IsValid() {
			d.TraceID, d.SpanID = id.String(), r.SpanID().String()
		}
		r.WalkAttributes(func(kv attribute.KeyValue) bool {
			d.Attrs[string(kv.Key)] = kv.Value.String()
			return true
		})
		out = append(out, d)
	}
	return out
}

// ResetLogs forgets the log records exported so far.
func (m *Manual) ResetLogs() {
	m.logs.mu.Lock()
	defer m.logs.mu.Unlock()
	m.logs.recs = nil
}

// recorder keeps the spans that ended, in the order they ended.
type recorder struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

func (*recorder) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

func (r *recorder) OnEnd(s sdktrace.ReadOnlySpan) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spans = append(r.spans, s)
}

func (*recorder) Shutdown(context.Context) error   { return nil }
func (*recorder) ForceFlush(context.Context) error { return nil }

// SpanData is one span that ended and was sampled.
type SpanData struct {
	// Name is the span's name: an op, or one of the SpanNames.
	Name string
	// Kind is "server" for a request's or a handshake's span, and
	// "internal" for every other.
	Kind string
	// TraceID and SpanID are the span's ids, in hex. ParentID is its
	// parent's span id, or empty for the root of a trace, and
	// RemoteParent says the parent is the caller's span, from the trace
	// context a request brought.
	TraceID, SpanID, ParentID string
	RemoteParent              bool
	// Attrs are the span's attributes.
	Attrs map[string]string
	// Status is "error" for a span marked failed and StatusNote its
	// description; both are empty otherwise.
	Status, StatusNote string
	// Events and Links count what the span carries beyond attributes,
	// and TraceState is its tracestate; starfixd's spans carry none.
	Events, Links int
	TraceState    string
	// Start and End are when the span began and ended.
	Start, End time.Time
}

// Spans returns the sampled spans that have ended, in the order they
// ended: a child before its parent.
func (m *Manual) Spans() []SpanData {
	m.spans.mu.Lock()
	defer m.spans.mu.Unlock()
	return spanData(m.spans.spans)
}

// TakeSpans returns the sampled spans that have ended since it was last
// called, or since [Manual.ResetSpans], and forgets them: a long run
// counts its spans with it, a few at a time, without keeping them all.
func (m *Manual) TakeSpans() []SpanData {
	m.spans.mu.Lock()
	ended := m.spans.spans
	m.spans.spans = nil
	m.spans.mu.Unlock()
	return spanData(ended)
}

func spanData(spans []sdktrace.ReadOnlySpan) []SpanData {
	out := make([]SpanData, 0, len(spans))
	for _, s := range spans {
		d := SpanData{Name: s.Name(), Kind: s.SpanKind().String(), TraceID: s.SpanContext().TraceID().String(),
			SpanID: s.SpanContext().SpanID().String(), RemoteParent: s.Parent().IsRemote(),
			Attrs: make(map[string]string, len(s.Attributes())), Events: len(s.Events()), Links: len(s.Links()),
			TraceState: s.SpanContext().TraceState().String(),
			Start:      s.StartTime(), End: s.EndTime()}
		if s.Parent().IsValid() {
			d.ParentID = s.Parent().SpanID().String()
		}
		for _, kv := range s.Attributes() {
			d.Attrs[string(kv.Key)] = kv.Value.String()
		}
		if st := s.Status(); st.Code == codes.Error {
			d.Status, d.StatusNote = "error", st.Description
		}
		out = append(out, d)
	}
	return out
}

// ResetSpans forgets the spans that have ended so far.
func (m *Manual) ResetSpans() {
	m.spans.mu.Lock()
	defer m.spans.mu.Unlock()
	m.spans.spans = nil
}

// SpanNames returns the names of spans, in order.
func SpanNames(spans []SpanData) []string {
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = s.Name
	}
	return out
}

// SpanList lists spans, one a line, for a test's failure message.
func SpanList(spans []SpanData) string {
	var b strings.Builder
	for _, s := range spans {
		fmt.Fprintf(&b, "%s (%s) trace=%s span=%s parent=%s", s.Name, s.Kind, s.TraceID, s.SpanID, s.ParentID)
		if s.RemoteParent {
			b.WriteString(" (remote)")
		}
		for _, k := range slices.Sorted(maps.Keys(s.Attrs)) {
			fmt.Fprintf(&b, " %s=%s", k, s.Attrs[k])
		}
		if s.Status != "" {
			fmt.Fprintf(&b, " status=%s:%s", s.Status, s.StatusNote)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// Point is one series of an instrument as last collected.
type Point struct {
	// Name is the instrument's OpenTelemetry name.
	Name string
	// Attrs are the series' attributes.
	Attrs map[string]string
	// Value is a counter's or up-down counter's total, or a gauge's
	// value; for a histogram it is the sum of the observations.
	Value float64
	// Count is how many observations a histogram has, and Bounds its
	// buckets' upper bounds.
	Count  uint64
	Bounds []float64
}

// Snapshot is every series collected at one time.
type Snapshot []Point

// Collect reads every instrument now.
func (m *Manual) Collect(ctx context.Context) (Snapshot, error) {
	var rm metricdata.ResourceMetrics
	if err := m.r.Collect(ctx, &rm); err != nil {
		return nil, fmt.Errorf("telemetry: collect: %w", err)
	}
	var out Snapshot
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			switch d := md.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					out = append(out, Point{Name: md.Name, Attrs: attrMap(p.Attributes), Value: float64(p.Value)})
				}
			case metricdata.Sum[float64]:
				for _, p := range d.DataPoints {
					out = append(out, Point{Name: md.Name, Attrs: attrMap(p.Attributes), Value: p.Value})
				}
			case metricdata.Gauge[int64]:
				for _, p := range d.DataPoints {
					out = append(out, Point{Name: md.Name, Attrs: attrMap(p.Attributes), Value: float64(p.Value)})
				}
			case metricdata.Gauge[float64]:
				for _, p := range d.DataPoints {
					out = append(out, Point{Name: md.Name, Attrs: attrMap(p.Attributes), Value: p.Value})
				}
			case metricdata.Histogram[float64]:
				for _, p := range d.DataPoints {
					out = append(out, Point{Name: md.Name, Attrs: attrMap(p.Attributes), Value: p.Sum, Count: p.Count, Bounds: p.Bounds})
				}
			case metricdata.Histogram[int64]:
				for _, p := range d.DataPoints {
					out = append(out, Point{Name: md.Name, Attrs: attrMap(p.Attributes), Value: float64(p.Sum), Count: p.Count, Bounds: p.Bounds})
				}
			}
		}
	}
	return out, nil
}

func attrMap(set attribute.Set) map[string]string {
	m := make(map[string]string, set.Len())
	for _, kv := range set.ToSlice() {
		m[string(kv.Key)] = kv.Value.String()
	}
	return m
}

// Series returns the series of the instrument named name.
func (s Snapshot) Series(name string) Snapshot {
	var out Snapshot
	for _, p := range s {
		if p.Name == name {
			out = append(out, p)
		}
	}
	return out
}

// Find returns the series of name whose attributes are exactly attrs,
// given as key, value, key, value. It reports whether there is one.
func (s Snapshot) Find(name string, attrs ...string) (Point, bool) {
	want := map[string]string{}
	for i := 0; i+1 < len(attrs); i += 2 {
		want[attrs[i]] = attrs[i+1]
	}
	for _, p := range s {
		if p.Name == name && maps.Equal(p.Attrs, want) {
			return p, true
		}
	}
	return Point{}, false
}

// Value returns the value of the series Find finds, or 0 when there is
// none.
func (s Snapshot) Value(name string, attrs ...string) float64 {
	p, _ := s.Find(name, attrs...)
	return p.Value
}

// Sum returns the total of every series of name whose attributes
// include attrs, given as key, value, key, value.
func (s Snapshot) Sum(name string, attrs ...string) float64 {
	var total float64
	for _, p := range s.matching(name, attrs) {
		total += p.Value
	}
	return total
}

// Observations returns how many observations the histogram name has in
// the series whose attributes include attrs.
func (s Snapshot) Observations(name string, attrs ...string) uint64 {
	var n uint64
	for _, p := range s.matching(name, attrs) {
		n += p.Count
	}
	return n
}

func (s Snapshot) matching(name string, attrs []string) []Point {
	var out []Point
next:
	for _, p := range s {
		if p.Name != name {
			continue
		}
		for i := 0; i+1 < len(attrs); i += 2 {
			if p.Attrs[attrs[i]] != attrs[i+1] {
				continue next
			}
		}
		out = append(out, p)
	}
	return out
}

// Names returns the names of the instruments that have a series, sorted.
func (s Snapshot) Names() []string {
	seen := map[string]bool{}
	for _, p := range s {
		seen[p.Name] = true
	}
	return slices.Sorted(maps.Keys(seen))
}

// String lists the series, one a line, for a test's failure message.
func (s Snapshot) String() string {
	var b strings.Builder
	for _, p := range s {
		keys := slices.Sorted(maps.Keys(p.Attrs))
		fmt.Fprintf(&b, "%s{", p.Name)
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "%s=%s", k, p.Attrs[k])
		}
		fmt.Fprintf(&b, "} %g", p.Value)
		if p.Count > 0 {
			fmt.Fprintf(&b, " (%d observations)", p.Count)
		}
		b.WriteByte('\n')
	}
	return b.String()
}
