package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// Manual reads a Telemetry's instruments in process, on demand: what a
// test asserts values with, and what a soak compares against the event
// log. It exports nothing.
type Manual struct {
	r *sdkmetric.ManualReader
}

// NewManual makes a Telemetry that records as [New]'s does, with the
// same instruments, attributes and buckets, and is read through the
// Manual. cfg.Getenv is read only for the resource; cfg.Pull also
// offers the Prometheus handler. The process's SDK error handler is left
// alone.
func NewManual(cfg Config) (*Telemetry, *Manual, error) {
	env, err := readEnv(cfg.Getenv)
	if err != nil {
		return nil, nil, err
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	t := &Telemetry{log: log, errs: newErrlog(log, env.secrets())}
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
	if err := t.build(cfg, env, readers...); err != nil {
		return nil, nil, err
	}
	return t, m, nil
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
