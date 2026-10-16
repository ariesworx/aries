package telemetry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/logs"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
)

// Invented projects.
const (
	projA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	projB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

var testOps = []string{"create", "show", "project.add"}

// env is a fake environment.
type env map[string]string

func (e env) get(name string) string { return e[name] }

// syncBuffer is a log sink read while exporters still write to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func logTo(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// noLeak fails the test if, once its cleanups have run, more goroutines
// are running than when it was called. It waits for them to settle: a
// closed connection's goroutines take a moment to return.
func noLeak(t *testing.T) {
	t.Helper()
	before := runtime.NumGoroutine()
	t.Cleanup(func() {
		deadline := time.Now().Add(10 * time.Second)
		for {
			n := runtime.NumGoroutine()
			if n <= before {
				return
			}
			if time.Now().After(deadline) {
				buf := make([]byte, 1<<20)
				t.Errorf("%d goroutines running, %d before the test:\n%s", n, before, buf[:runtime.Stack(buf, true)])
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
}

// manual makes a Telemetry read in process, with per_project as given.
func manual(t *testing.T, perProject bool) (*Telemetry, *Manual) {
	t.Helper()
	tel, m, err := NewManual(Config{Version: "v0.0.0-test", PerProject: perProject, Ops: testOps})
	if err != nil {
		t.Fatalf("NewManual: %v", err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	return tel, m
}

func collect(t *testing.T, m *Manual) Snapshot {
	t.Helper()
	snap, err := m.Collect(t.Context())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return snap
}

// With nothing configured there is no exporter, no listener and no
// goroutine, and every method is a no-op; a nil Telemetry likewise.
func TestOffByDefault(t *testing.T) {
	// Serial: counts the process's goroutines (noLeak).
	noLeak(t)
	for _, tc := range []struct {
		name string
		make func(t *testing.T) *Telemetry
	}{
		{"nil", func(*testing.T) *Telemetry { return nil }},
		{"nothing configured", func(t *testing.T) *Telemetry {
			tel, err := New(t.Context(), Config{Version: "v1"})
			if err != nil {
				t.Fatalf("New(zero) = %v", err)
			}
			return tel
		}},
		{"an environment that names nothing", func(t *testing.T) *Telemetry {
			tel, err := New(t.Context(), Config{Getenv: env{"OTEL_SERVICE_NAME": "x"}.get})
			if err != nil {
				t.Fatalf("New = %v", err)
			}
			return tel
		}},
		{"OTEL_SDK_DISABLED", func(t *testing.T) *Telemetry {
			tel, err := New(t.Context(), Config{Pull: true,
				Getenv: env{"OTEL_SDK_DISABLED": "true", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:1"}.get})
			if err != nil {
				t.Fatalf("New = %v", err)
			}
			return tel
		}},
		// Each signal has its own switch: with an endpoint set, metrics
		// alone set to none leaves traces on (TestReadEnvTraces).
		{"OTEL_METRICS_EXPORTER=none and OTEL_TRACES_EXPORTER=none", func(t *testing.T) *Telemetry {
			tel, err := New(t.Context(), Config{
				Getenv: env{"OTEL_METRICS_EXPORTER": "none", "OTEL_TRACES_EXPORTER": "none", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:1"}.get})
			if err != nil {
				t.Fatalf("New = %v", err)
			}
			return tel
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := runtime.NumGoroutine()
			tel := tc.make(t)
			if tel.Enabled() {
				t.Errorf("Enabled() = true, want false")
			}
			if tel != nil && tel.pull != nil {
				t.Errorf("a Prometheus handler was made, want none")
			}
			if n := runtime.NumGoroutine(); n > before {
				t.Errorf("%d goroutines after New, %d before: want none started", n, before)
			}
			tel.Count(Handshakes, ResultOK)
			tel.Record(RequestDuration, 1, "show", ResultOK)
			tel.Since(ReaperDuration, time.Now())
			tel.Watch(StoresOpen, func() float64 { return 1 })
			ctx, span := tel.Start(t.Context(), "request")
			span.End()
			if ctx != t.Context() {
				t.Errorf("Start changed the context before traces are built")
			}
			if err := tel.Shutdown(t.Context()); err != nil {
				t.Errorf("Shutdown() = %v, want nil", err)
			}
		})
	}
}

func TestRecord(t *testing.T) {
	t.Parallel()
	tel, m := manual(t, true)
	tel.Count(Handshakes, ResultOK)
	tel.Count(Handshakes, ResultOK)
	tel.Count(Handshakes, "busy")
	tel.Record(Connections, 1, projA)
	tel.Record(Connections, 1, projA)
	tel.Record(Connections, -1, projA)
	tel.Record(RequestDuration, 0.004, "show", ResultOK)
	tel.Record(RequestDuration, 2, "show", ResultOK)
	tel.Record(RequestDuration, 0.2, "create", "invalid")
	tel.Record(KeysFileLastWrite, 1700000000)
	tel.Record(KeysFileLastWrite, 1700000060)
	tel.Count(ProjectRequests, projA, ClassOK)
	tel.Count(ProjectRequests, projB, ClassRefused)
	tel.Count(Claims, ClaimRelease, ReasonArchived)
	open := 3.0
	tel.Watch(StoresOpen, func() float64 { return open })
	tel.Watch(DBPoolConns, func() float64 { return 5 })
	tel.Watch(DBPoolInUse, func() float64 { return 2 })
	tel.Watch(DBPoolWaits, func() float64 { return 7 })
	snap := collect(t, m)

	for _, tc := range []struct {
		name  string
		attrs []string
		want  float64
	}{
		{"aries.handshakes", []string{"result", "ok"}, 2},
		{"aries.handshakes", []string{"result", "busy"}, 1},
		{"aries.connections", []string{"project", projA}, 1},
		{"aries.keys_file.last_write.time", nil, 1700000060},
		{"aries.project.requests", []string{"project", projA, "result_class", "ok"}, 1},
		{"aries.project.requests", []string{"project", projB, "result_class", "refused"}, 1},
		{"aries.claims", []string{"event", "release", "reason", "archived"}, 1},
		{"aries.stores.open", nil, 3},
		{"aries.db.pool.conns", nil, 5},
		{"aries.db.pool.in_use", nil, 2},
		{"aries.db.pool.waits", nil, 7},
		{"aries.request.duration", []string{"op", "show", "result", "ok"}, 2.004},
	} {
		p, ok := snap.Find(tc.name, tc.attrs...)
		if !ok || p.Value != tc.want {
			t.Errorf("%s%v = %v (found %t), want %v\n%s", tc.name, tc.attrs, p.Value, ok, tc.want, snap)
		}
	}
	if n := snap.Observations("aries.request.duration", "op", "show"); n != 2 {
		t.Errorf("observations of aries.request.duration{op=show} = %d, want 2", n)
	}
	if n := snap.Observations("aries.request.duration"); n != 3 {
		t.Errorf("observations of aries.request.duration = %d, want 3", n)
	}
	// Every histogram has the one short list of buckets.
	p, _ := snap.Find("aries.request.duration", "op", "show", "result", "ok")
	if !slices.Equal(p.Bounds, Buckets) {
		t.Errorf("buckets of aries.request.duration = %v, want %v", p.Bounds, Buckets)
	}
	if first, last := Buckets[0], Buckets[len(Buckets)-1]; first != 0.005 || last != 10 {
		t.Errorf("Buckets run from %v to %v, want 5 ms to 10 s", first, last)
	}
	// The Go runtime's metrics come with them.
	if !slices.ContainsFunc(snap.Names(), func(n string) bool { return strings.HasPrefix(n, "go.") }) {
		t.Errorf("no go.* runtime metric among %v", snap.Names())
	}
}

// allKeys is every attribute key an instrument may have. A new key is a
// new dimension of every series: add it here only with a fixed list of
// values (AGENTS.md rule 18).
var allKeys = []string{"op", "result", "result_class", "project", "reason", "limit", "migrated", "event", "signal", "direction", "kind", "model"}

// wantKeys is each instrument's attribute keys (design §16.4).
var wantKeys = map[string][]string{
	"aries.request.duration":            {"op", "result"},
	"aries.project.requests":            {"project", "result_class"},
	"aries.connections":                 {"project"},
	"aries.handshakes":                  {"result"},
	"aries.connections.dropped":         {"reason"},
	"aries.limit.refusals":              {"limit"},
	"aries.store.write.duration":        {"project"},
	"aries.store.write.retries":         {"project"},
	"aries.stores.open":                 nil,
	"aries.store.open.duration":         {"migrated"},
	"aries.claims":                      {"event", "reason"},
	"aries.locks":                       {"event"},
	"aries.lock.wait.duration":          nil,
	"aries.gates":                       {"kind", "event"},
	"aries.usage.tokens":                {"project", "kind", "model"},
	"aries.control.writes":              {"op", "result"},
	"aries.keys_file.writes":            {"result"},
	"aries.keys_file.last_write.time":   nil,
	"aries.reaper.duration":             nil,
	"aries.reaper.last_run.time":        nil,
	"aries.clock.behind":                nil,
	"aries.clock.steps":                 {"direction"},
	"aries.telemetry.dropped":           {"signal"},
	"aries.db.pool.conns":               nil,
	"aries.db.pool.in_use":              nil,
	"aries.db.pool.waits":               nil,
	"aries.https.connections":           nil,
	"aries.https.connections.refused":   {"reason"},
	"aries.https.upgrades":              {"result", "reason"},
	"aries.https.key_proofs":            {"result"},
	"aries.https.tokens":                {"result"},
	"aries.https.tls_handshakes.failed": nil,
	"aries.https.cert.reloads":          {"result"},
}

// hostile are strings a client might choose, none of them a value any
// attribute lists.
var hostile = []string{"", " ", "ok\n", "../../etc/passwd", "alice", "s-1234567890", "ar-1a2b", `"},{"`, "\x00\x01", "\u202e",
	strings.Repeat("x", 300), "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa", "unknown-op", "create ", "Create"}

// recordAll records on every instrument with each of the values given
// for each of its attributes.
func recordAll(tel *Telemetry, values []string) {
	for i := range numInstruments {
		for _, a := range values {
			for _, b := range values {
				tel.Record(i, 1, a, b, a, b) // more values than any instrument has keys
			}
			tel.Record(i, 1, a)
			tel.Record(i, 1)
		}
	}
	for i := range numInstruments {
		if specs[i].watched {
			tel.Watch(i, func() float64 { return 0 })
		}
	}
}

// No instrument has an attribute key that is not on its list, whatever
// it is recorded with, and none has both a project and an op.
func TestAttributeKeys(t *testing.T) {
	t.Parallel()
	if len(wantKeys) != int(numInstruments) {
		t.Fatalf("wantKeys lists %d instruments, the package has %d: list the new one, with its keys", len(wantKeys), numInstruments)
	}
	for _, d := range Instruments() {
		want, ok := wantKeys[d.Name]
		if !ok {
			t.Errorf("instrument %s is not in wantKeys: list it, with its keys", d.Name)
			continue
		}
		if !slices.Equal(d.Keys, want) {
			t.Errorf("Instruments(): %s has keys %v, want %v", d.Name, d.Keys, want)
		}
		for _, k := range d.Keys {
			if !slices.Contains(allKeys, k) {
				t.Errorf("%s has the attribute key %q, which is not one of %v", d.Name, k, allKeys)
			}
		}
		if slices.Contains(d.Keys, KeyProject) && slices.Contains(d.Keys, KeyOp) {
			t.Errorf("%s has both project and op: projects times ops is too many series", d.Name)
		}
	}
	for _, perProject := range []bool{true, false} {
		t.Run(fmt.Sprintf("per_project=%t", perProject), func(t *testing.T) {
			tel, m := manual(t, perProject)
			recordAll(tel, append([]string{projA, "show", ResultOK}, hostile...))
			snap := collect(t, m)
			seen := map[string]bool{}
			for _, p := range snap {
				if !strings.HasPrefix(p.Name, "aries.") {
					continue
				}
				seen[p.Name] = true
				want := slices.Clone(wantKeys[p.Name])
				if !perProject {
					want = slices.DeleteFunc(want, func(k string) bool { return k == KeyProject })
				}
				slices.Sort(want)
				if got := slices.Sorted(maps.Keys(p.Attrs)); !slices.Equal(got, want) {
					t.Errorf("a series of %s has keys %v, want %v", p.Name, got, want)
				}
			}
			for name := range wantKeys {
				if !seen[name] {
					t.Errorf("%s has no series after being recorded", name)
				}
			}
		})
	}
}

// Whatever strings the attributes are given, the number of series is
// bounded by the lists in the code: an unknown op is unknown, and
// nothing a client chooses becomes a value.
func TestSeriesAreBounded(t *testing.T) {
	t.Parallel()
	tel, m, err := NewManual(Config{PerProject: true, Ops: testOps, Projects: 3})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	values := slices.Clone(hostile)
	for i := range 40 {
		values = append(values, fmt.Sprintf("op-%d", i), fmt.Sprintf("%08x-0000-4000-8000-000000000000", i))
	}
	recordAll(tel, values)
	snap := collect(t, m)

	ops, res := len(testOps)+1, len(results)+1 // and the fallback
	projects := 3 + 1
	for name, most := range map[string]int{
		"aries.request.duration":          ops * res,
		"aries.project.requests":          projects * 4,
		"aries.connections":               projects,
		"aries.handshakes":                res,
		"aries.connections.dropped":       8,
		"aries.limit.refusals":            len(limits) + 1,
		"aries.store.write.duration":      projects,
		"aries.store.write.retries":       projects,
		"aries.stores.open":               1,
		"aries.store.open.duration":       3,
		"aries.claims":                    5 * 5,
		"aries.usage.tokens":              projects * 5, // kinds and the fallback, models all (other)
		"aries.control.writes":            ops * res,
		"aries.keys_file.writes":          3,
		"aries.keys_file.last_write.time": 1,
		"aries.reaper.duration":           1,
		"aries.reaper.last_run.time":      1,
		"aries.telemetry.dropped":         2,
	} {
		if n := len(snap.Series(name)); n == 0 || n > most {
			t.Errorf("%s has %d series, want 1 to %d", name, n, most)
		}
	}
	for _, p := range snap {
		if !strings.HasPrefix(p.Name, "aries.") {
			continue
		}
		for k, v := range p.Attrs {
			if slices.Contains(hostile, v) || strings.HasPrefix(v, "op-") {
				t.Errorf("%s has %s=%q, a value no list holds", p.Name, k, v)
			}
		}
	}
	// The hostile ops all became one value.
	if v := snap.Sum("aries.request.duration", "op", Unknown); v == 0 {
		t.Errorf("no series of aries.request.duration has op=%s", Unknown)
	}
	// Only as many projects as the cap are named; the rest are other.
	named := map[string]bool{}
	for _, p := range snap.Series("aries.connections") {
		named[p.Attrs["project"]] = true
	}
	if len(named) != 4 || !named[Other] {
		t.Errorf("aries.connections names projects %v, want 3 and %q", slices.Sorted(maps.Keys(named)), Other)
	}
}

// promText fetches path from a Telemetry served on a listener of its
// own.
func promText(t *testing.T, tel *Telemetry) string {
	t.Helper()
	srv := httptest.NewServer(tel.pull)
	t.Cleanup(srv.Close)
	return get(t, srv.URL)
}

// get fetches url with a client that keeps no connection.
func get(t *testing.T, url string) string {
	t.Helper()
	_, body := getStatus(t, url)
	return body
}

func getStatus(t *testing.T, url string) (int, string) {
	t.Helper()
	tr := &http.Transport{DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Transport: tr, Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s: read: %v", url, err)
	}
	return resp.StatusCode, string(b)
}

// parseProm parses Prometheus's text format strictly enough to catch a
// malformed line: it returns each metric family's type and the names of
// its samples.
func parseProm(t *testing.T, text string) (types map[string]string, samples map[string]int) {
	t.Helper()
	types, samples = map[string]string{}, map[string]int{}
	for line := range strings.Lines(text) {
		line = strings.TrimSuffix(line, "\n")
		switch {
		case line == "":
		case strings.HasPrefix(line, "# TYPE "):
			f := strings.Fields(line)
			if len(f) != 4 {
				t.Fatalf("malformed TYPE line %q", line)
			}
			types[f[2]] = f[3]
		case strings.HasPrefix(line, "# HELP "):
		case strings.HasPrefix(line, "#"):
			t.Fatalf("unexpected comment line %q", line)
		default:
			name := line
			if i := strings.IndexAny(line, "{ "); i >= 0 {
				name = line[:i]
			}
			rest := line[len(name):]
			if strings.HasPrefix(rest, "{") {
				end := strings.LastIndex(rest, "}")
				if end < 0 {
					t.Fatalf("unclosed labels in %q", line)
				}
				rest = rest[end+1:]
			}
			if f := strings.Fields(rest); len(f) < 1 || len(f) > 2 || name == "" {
				t.Fatalf("malformed sample line %q", line)
			}
			samples[name]++
		}
	}
	return types, samples
}

// The Prometheus endpoint's text parses, and carries every instrument
// under the name and with the unit suffix the guide documents.
func TestPrometheusNames(t *testing.T) {
	t.Parallel()
	tel, _, err := NewManual(Config{Version: "v1.2.3", PerProject: true, Ops: testOps, Pull: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	recordAll(tel, []string{projA, "show", ResultOK})
	text := promText(t, tel)
	types, samples := parseProm(t, text)

	//nolint:gosec // G101: metric names; aries.https.tokens counts tokens and holds none
	want := map[string]string{
		"aries.request.duration":            "aries_request_duration_seconds",
		"aries.project.requests":            "aries_project_requests_total",
		"aries.connections":                 "aries_connections",
		"aries.handshakes":                  "aries_handshakes_total",
		"aries.connections.dropped":         "aries_connections_dropped_total",
		"aries.limit.refusals":              "aries_limit_refusals_total",
		"aries.store.write.duration":        "aries_store_write_duration_seconds",
		"aries.store.write.retries":         "aries_store_write_retries_total",
		"aries.stores.open":                 "aries_stores_open",
		"aries.store.open.duration":         "aries_store_open_duration_seconds",
		"aries.claims":                      "aries_claims_total",
		"aries.locks":                       "aries_locks_total",
		"aries.lock.wait.duration":          "aries_lock_wait_duration_seconds",
		"aries.gates":                       "aries_gates_total",
		"aries.usage.tokens":                "aries_usage_tokens_total",
		"aries.control.writes":              "aries_control_writes_total",
		"aries.keys_file.writes":            "aries_keys_file_writes_total",
		"aries.keys_file.last_write.time":   "aries_keys_file_last_write_time_seconds",
		"aries.reaper.duration":             "aries_reaper_duration_seconds",
		"aries.reaper.last_run.time":        "aries_reaper_last_run_time_seconds",
		"aries.clock.behind":                "aries_clock_behind_seconds",
		"aries.clock.steps":                 "aries_clock_steps_total",
		"aries.telemetry.dropped":           "aries_telemetry_dropped_total",
		"aries.db.pool.conns":               "aries_db_pool_conns",
		"aries.db.pool.in_use":              "aries_db_pool_in_use",
		"aries.db.pool.waits":               "aries_db_pool_waits_total",
		"aries.https.connections":           "aries_https_connections",
		"aries.https.connections.refused":   "aries_https_connections_refused_total",
		"aries.https.upgrades":              "aries_https_upgrades_total",
		"aries.https.key_proofs":            "aries_https_key_proofs_total",
		"aries.https.tokens":                "aries_https_tokens_total",
		"aries.https.tls_handshakes.failed": "aries_https_tls_handshakes_failed_total",
		"aries.https.cert.reloads":          "aries_https_cert_reloads_total",
	}
	promType := map[Kind]string{Counter: "counter", UpDownCounter: "gauge", Gauge: "gauge", Histogram: "histogram"}
	for _, d := range Instruments() {
		if d.Prometheus != want[d.Name] {
			t.Errorf("Instruments(): %s is %q in Prometheus, want %q", d.Name, d.Prometheus, want[d.Name])
		}
		if got := types[d.Prometheus]; got != promType[d.Kind] {
			t.Errorf("/metrics: # TYPE %s = %q, want %q", d.Prometheus, got, promType[d.Kind])
		}
		sample := d.Prometheus
		if d.Kind == Histogram {
			sample += "_bucket"
			if n := samples[d.Prometheus+"_bucket"]; n%(len(Buckets)+1) != 0 {
				t.Errorf("/metrics: %s_bucket has %d samples, want a multiple of %d buckets", d.Prometheus, n, len(Buckets)+1)
			}
		}
		if samples[sample] == 0 {
			t.Errorf("/metrics has no sample named %s", sample)
		}
	}
	for _, s := range []string{
		`aries_handshakes_total{result="ok"} `,
		`aries_request_duration_seconds_bucket{op="show",result="ok",le="0.005"} `,
		`aries_request_duration_seconds_bucket{op="show",result="ok",le="10"} `,
		`aries_project_requests_total{project="` + projA + `",result_class="other"} `,
		`service_name="ariesd"`, `service_version="v1.2.3"`, `service_instance_id="`,
		"go_goroutine_count ",
	} {
		if !strings.Contains(text, s) {
			t.Errorf("/metrics does not contain %q", s)
		}
	}
	if t.Failed() {
		t.Logf("/metrics:\n%s", text)
	}
}

// collector is a fake OTLP/HTTP receiver.
type collector struct {
	*httptest.Server
	mu      sync.Mutex
	bodies  [][]byte
	headers []http.Header
	paths   []string
	got     chan struct{}
}

// newCollector answers every export with status, and body if it is set.
func newCollector(t *testing.T, status int, body func(*http.Request) string) *collector {
	t.Helper()
	c := &collector{got: make(chan struct{}, 1024)}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies, c.headers, c.paths = append(c.bodies, b), append(c.headers, r.Header.Clone()), append(c.paths, r.URL.Path)
		c.mu.Unlock()
		w.WriteHeader(status)
		if body != nil {
			_, _ = io.WriteString(w, body(r))
		}
		select {
		case c.got <- struct{}{}:
		default:
		}
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *collector) wait(t *testing.T) {
	t.Helper()
	select {
	case <-c.got:
	case <-time.After(20 * time.Second):
		t.Fatal("no export reached the collector in 20s")
	}
}

// carrying waits for an export whose body holds want, and returns it
// with its headers and path. Exports are cumulative, so one made after a
// recording holds it and everything recorded before.
func (c *collector) carrying(t *testing.T, want string) (body []byte, hdr http.Header, path string) {
	t.Helper()
	for seen := 0; ; {
		c.wait(t)
		c.mu.Lock()
		for ; seen < len(c.bodies); seen++ {
			if bytes.Contains(c.bodies[seen], []byte(want)) {
				body, hdr, path = c.bodies[seen], c.headers[seen], c.paths[seen]
				c.mu.Unlock()
				return body, hdr, path
			}
		}
		c.mu.Unlock()
	}
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

// Push and pull are each optional, on their own.
func TestPushAndPull(t *testing.T) {
	// Serial: counts the process's goroutines (noLeak).
	for _, tc := range []struct {
		name       string
		push, pull bool
	}{
		{"push alone", true, false},
		{"pull alone", false, true},
		{"both", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noLeak(t)
			col := newCollector(t, http.StatusOK, nil)
			log := &syncBuffer{}
			e := env{"OTEL_METRIC_EXPORT_INTERVAL": "50", "OTEL_EXPORTER_OTLP_HEADERS": "x-test=one%20two"}
			if tc.push {
				e["OTEL_EXPORTER_OTLP_ENDPOINT"] = col.URL
			}
			tel, err := New(t.Context(), Config{Version: "v1", Getenv: e.get, Logger: logTo(log), Pull: tc.pull, PerProject: true, Ops: testOps})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if !tel.Enabled() {
				t.Fatalf("Enabled() = false")
			}
			tel.Count(Handshakes, ResultOK)
			tel.Record(RequestDuration, 0.01, "show", ResultOK)
			if tc.push {
				// Not the first export whatever it holds: on a slow machine
				// the reader's first interval, 50 ms from New, ends before
				// the two recordings above are made, and that export has
				// neither.
				body, hdr, path := col.carrying(t, "aries.handshakes")
				if path != "/v1/metrics" {
					t.Errorf("export posted to %q, want /v1/metrics", path)
				}
				if got := hdr.Get("x-test"); got != "one two" {
					t.Errorf("export header x-test = %q, want %q", got, "one two")
				}
				if ct := hdr.Get("Content-Type"); ct != "application/x-protobuf" {
					t.Errorf("export Content-Type = %q, want application/x-protobuf", ct)
				}
				for _, s := range []string{"aries.handshakes", "aries.request.duration", "service.name", "ariesd", "service.instance.id", "go.goroutine.count"} {
					if !bytes.Contains(body, []byte(s)) {
						t.Errorf("the export does not carry %q", s)
					}
				}
			}
			if (tel.pull != nil) != tc.pull {
				t.Errorf("Prometheus handler made = %t, want %t", tel.pull != nil, tc.pull)
			}
			if tc.pull {
				if text := promText(t, tel); !strings.Contains(text, `aries_handshakes_total{result="ok"} 1`) {
					t.Errorf("/metrics lacks the handshake counted:\n%s", text)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := tel.Shutdown(ctx); err != nil {
				t.Errorf("Shutdown: %v", err)
			}
			if !tc.push && col.count() != 0 {
				t.Errorf("%d exports reached the collector with no endpoint configured", col.count())
			}
			if !strings.Contains(log.String(), "telemetry on") {
				t.Errorf("no start-up line in the log:\n%s", log)
			}
		})
	}
}

const token = "Bearer-s3cr3t-T0KEN-0123456789abcdef"

// A header's value may be a token: no start-up line, export failure or
// refusal of the settings may hold it, even when the collector's answer
// repeats it.
func TestTokenIsNeverLogged(t *testing.T) {
	// Serial: counts the process's goroutines (noLeak).
	noLeak(t)
	col := newCollector(t, http.StatusUnauthorized, func(r *http.Request) string {
		return "bad credentials: " + r.Header.Get("Authorization") + " for " + r.URL.String()
	})
	log := &syncBuffer{}
	u := strings.Replace(col.URL, "http://", "http://user:pw-"+token+"@", 1) + "/v1/metrics?api_key=q-" + token
	tel, err := New(t.Context(), Config{Version: "v1", Logger: logTo(log), Pull: true, Ops: testOps, Getenv: env{
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": u,
		"OTEL_EXPORTER_OTLP_HEADERS":          "Authorization=" + token + ",x-scope=team",
		"OTEL_METRIC_EXPORT_INTERVAL":         "50",
	}.get})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tel.Count(Handshakes, ResultOK)
	col.wait(t)
	col.wait(t)
	col.mu.Lock()
	sent := col.headers[0].Get("Authorization")
	col.mu.Unlock()
	if sent != token {
		t.Fatalf("the collector received Authorization %q, want the token", sent)
	}
	// The failure is logged, and counted where a scrape can see it.
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(log.String(), "export failed") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if text := promText(t, tel); !strings.Contains(text, `aries_telemetry_dropped_total{signal="metrics"}`) {
		t.Errorf("/metrics does not count the failed export:\n%s", text)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_ = tel.Shutdown(ctx)
	out := log.String()
	if !strings.Contains(out, "telemetry on") || !strings.Contains(out, "export failed") {
		t.Errorf("want a start-up line and an export failure in the log:\n%s", out)
	}
	if strings.Contains(out, token) || strings.Contains(out, "s3cr3t") {
		t.Errorf("the log holds the token:\n%s", out)
	}
	if n := strings.Count(out, "export failed"); n != 1 {
		t.Errorf("%d export failures logged for a collector that keeps refusing, want 1 until %s has passed:\n%s", n, errEvery, out)
	}

	// A refusal of the settings does not repeat a secret either.
	for _, e := range []env{
		{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318", "OTEL_EXPORTER_OTLP_HEADERS": "Authorization " + token},
		{"OTEL_EXPORTER_OTLP_ENDPOINT": "ftp://user:" + token + "@example.com"},
		{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318", "OTEL_EXPORTER_OTLP_METRICS_HEADERS": "a=%zz" + token},
	} {
		_, err := New(t.Context(), Config{Getenv: e.get})
		if err == nil {
			t.Errorf("New(%v) = nil error, want a refusal", slices.Sorted(maps.Keys(e)))
			continue
		}
		if strings.Contains(err.Error(), token) {
			t.Errorf("the refusal repeats the token: %v", err)
		}
		if !strings.Contains(err.Error(), "fix: ") {
			t.Errorf("the refusal names no fix: %v", err)
		}
	}
}

// A failure that keeps happening is logged once, then once every
// errEvery with a count of those left out; a success says it recovered.
func TestExportErrorsAreSampled(t *testing.T) {
	t.Parallel()
	log := &syncBuffer{}
	now := time.Unix(1_700_000_000, 0)
	e := newErrlog(logTo(log), []string{"hunter2"}, "metrics")
	e.now = func() time.Time { return now }
	fail := func() {
		e.failed(logs.TelemetryExportFailed, "export failed", fmt.Errorf("dial: refused (password hunter2)"))
	}
	for range 100 {
		fail()
		now = now.Add(time.Second)
	}
	if n := strings.Count(log.String(), "export failed"); n != 1 {
		t.Fatalf("%d lines for 100 failures in 100s, want 1:\n%s", n, log)
	}
	now = now.Add(errEvery)
	fail()
	out := log.String()
	if n := strings.Count(out, `msg="export failed" event=telemetry.export_failed signal=metrics`); n != 2 || !strings.Contains(out, "failures_not_logged=99") {
		t.Errorf("after %s: %d lines, want 2, the second saying 99 were not logged:\n%s", errEvery, n, out)
	}
	if strings.Contains(out, "hunter2") {
		t.Errorf("the log holds a secret:\n%s", out)
	}
	e.ok()
	e.ok()
	if n := strings.Count(log.String(), `msg="telemetry: exports succeed again" event=telemetry.export_recovered signal=metrics`); n != 1 {
		t.Errorf("%d recovery lines, want 1:\n%s", n, log)
	}
	fail()
	if n := strings.Count(log.String(), "export failed"); n != 3 {
		t.Errorf("a failure after a recovery was not logged at once:\n%s", log)
	}
}

func TestReadEnv(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		env      env
		endpoint string
		headers  map[string]string
		interval time.Duration
		disabled bool
		wantErr  string
	}{
		{name: "empty"},
		{name: "collector on the machine", env: env{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318"},
			endpoint: "http://127.0.0.1:4318/v1/metrics"},
		{name: "a base path keeps its path", env: env{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://otlp.example.com/otlp/"},
			endpoint: "https://otlp.example.com/otlp/v1/metrics"},
		{name: "the signal's endpoint is used as it is", env: env{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318",
			"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "https://metrics.example.com/api/push"},
			endpoint: "https://metrics.example.com/api/push"},
		{name: "headers, the signal's winning", env: env{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318",
			"OTEL_EXPORTER_OTLP_HEADERS": "a=1, b=two%20words", "OTEL_EXPORTER_OTLP_METRICS_HEADERS": "a=3"},
			endpoint: "http://127.0.0.1:4318/v1/metrics", headers: map[string]string{"a": "3", "b": "two words"}},
		{name: "interval", env: env{"OTEL_METRIC_EXPORT_INTERVAL": "15000"}, interval: 15 * time.Second},
		{name: "disabled", env: env{"OTEL_SDK_DISABLED": "TRUE", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318"},
			endpoint: "http://127.0.0.1:4318/v1/metrics", disabled: true},
		{name: "exporter none", env: env{"OTEL_METRICS_EXPORTER": "none", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318"}},
		{name: "exporter prometheus", env: env{"OTEL_METRICS_EXPORTER": "prometheus"}, wantErr: "telemetry: listen:"},
		{name: "grpc", env: env{"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4317"},
			wantErr: "OTLP over HTTP only"},
		{name: "not a URL", env: env{"OTEL_EXPORTER_OTLP_ENDPOINT": "127.0.0.1:4318"}, wantErr: "not an http or https URL"},
		{name: "bad interval", env: env{"OTEL_METRIC_EXPORT_INTERVAL": "1m"}, wantErr: "milliseconds"},
		{name: "bad timeout", env: env{"OTEL_EXPORTER_OTLP_TIMEOUT": "-5"}, wantErr: "milliseconds"},
		{name: "bad resource attributes", env: env{"OTEL_RESOURCE_ATTRIBUTES": "novalue"}, wantErr: "not name=value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := readEnv(tc.env.get)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "fix: ") {
					t.Fatalf("readEnv = %v, want an error containing %q and a fix", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("readEnv = %v", err)
			}
			if c.endpoint != tc.endpoint {
				t.Errorf("endpoint = %q, want %q", c.endpoint, tc.endpoint)
			}
			if len(tc.headers) > 0 && !maps.Equal(c.headers, tc.headers) {
				t.Errorf("headers = %v, want %v", c.headers, tc.headers)
			}
			if want := cmpOr(tc.interval, defaultInterval); c.interval != want {
				t.Errorf("interval = %s, want %s", c.interval, want)
			}
			if c.disabled != tc.disabled {
				t.Errorf("disabled = %t, want %t", c.disabled, tc.disabled)
			}
		})
	}
}

func cmpOr(a, b time.Duration) time.Duration {
	if a != 0 {
		return a
	}
	return b
}

func TestResource(t *testing.T) {
	t.Parallel()
	attrs := func(e env) map[string]string {
		c, err := readEnv(e.get)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]string{}
		for _, kv := range c.resource("v9") {
			m[string(kv.Key)] = kv.Value.String()
		}
		return m
	}
	a := attrs(env{})
	if a["service.name"] != "ariesd" || a["service.version"] != "v9" || !uuidLike(a["service.instance.id"]) {
		t.Errorf("default resource = %v, want service.name ariesd, version v9 and a UUID instance id", a)
	}
	if b := attrs(env{}); b["service.instance.id"] == a["service.instance.id"] {
		t.Errorf("two resources share the instance id %q", a["service.instance.id"])
	}
	a = attrs(env{"OTEL_SERVICE_NAME": "tracker", "OTEL_RESOURCE_ATTRIBUTES": "service.name=other,deployment.environment.name=prod,service.instance.id=host-1,service.version=fake"})
	want := map[string]string{"service.name": "tracker", "service.version": "v9", "service.instance.id": "host-1", "deployment.environment.name": "prod"}
	if !maps.Equal(a, want) {
		t.Errorf("resource = %v, want %v", a, want)
	}
	if a = attrs(env{"OTEL_RESOURCE_ATTRIBUTES": "service.name=from-attrs"}); a["service.name"] != "from-attrs" {
		t.Errorf("service.name = %q, want the one OTEL_RESOURCE_ATTRIBUTES gives", a["service.name"])
	}
}

// The periodic reader exports from the moment New makes it, on a
// goroutine of its own, and an export that fails counts what it dropped.
// With a short interval and a collector that is down, its first failures
// come while New is still making the instruments, as they did on a
// machine with no processor to spare. Such a failure must not read what
// is being made, which the race detector fails this test for, and it is
// counted all the same: the count of what was dropped is every failed
// export's, those from before the instruments included.
func TestExportFailsWhileNewMakesTheInstruments(t *testing.T) {
	t.Parallel()
	tel := newTelemetry(Config{Logger: logTo(&syncBuffer{}), Ops: testOps}, envConfig{})
	m := &Manual{r: sdkmetric.NewManualReader()}
	// The reader's goroutine: exports that fail, one after another, from
	// before the instruments are made until after.
	g := &guarded{Exporter: failing{err: errors.New("connection refused")}, t: tel}
	var failed atomic.Int64
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Go(func() {
		for !stop.Load() {
			if err := g.Export(context.Background(), &metricdata.ResourceMetrics{}); err != nil {
				t.Errorf("Export = %v, want nil: the failure is handled here", err)
				return
			}
			failed.Add(1)
		}
	})
	// more waits for that many more exports to have failed.
	more := func(n int64) {
		t.Helper()
		want := failed.Load() + n
		for end := time.Now().Add(time.Minute); failed.Load() < want; time.Sleep(time.Millisecond) {
			if time.Now().After(end) {
				stop.Store(true)
				t.Fatalf("%d exports failed in a minute, want %d", failed.Load(), want)
			}
		}
	}
	more(3)
	if err := tel.build(resource.Empty(), m.r); err != nil {
		stop.Store(true)
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	more(3)
	stop.Store(true)
	wg.Wait()
	if got, want := int64(collect(t, m).Value("aries.telemetry.dropped", "signal", "metrics")), failed.Load(); got != want {
		t.Errorf("aries.telemetry.dropped = %d after %d exports failed, some of them before the instruments were made; want every one counted", got, want)
	}
}

// A collector that is down, and one that accepts and never answers, hold
// neither a recording nor a stop.
func TestDeadCollector(t *testing.T) {
	// Serial: counts the process's goroutines (noLeak).
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	_ = closed.Close()
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = silent.Close() }) // accepts in the backlog, and never reads or answers

	for _, tc := range []struct{ name, addr string }{{"closed port", closedAddr}, {"silent listener", silent.Addr().String()}} {
		t.Run(tc.name, func(t *testing.T) {
			noLeak(t)
			log := &syncBuffer{}
			// An export to the listener that never answers lasts its
			// timeout, a minute here, and the first starts 20 ms in: a
			// recording that waited for an export would wait that long.
			const exportTimeout = time.Minute
			ms := fmt.Sprint(exportTimeout.Milliseconds())
			tel, err := New(t.Context(), Config{Logger: logTo(log), Ops: testOps, PerProject: true, Getenv: env{
				"OTEL_EXPORTER_OTLP_ENDPOINT": "http://" + tc.addr, "OTEL_METRIC_EXPORT_INTERVAL": "20",
				"OTEL_METRIC_EXPORT_TIMEOUT": ms, "OTEL_EXPORTER_OTLP_TIMEOUT": ms,
			}.get})
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			for range 20000 {
				tel.Record(RequestDuration, 0.01, "show", ResultOK)
				tel.Count(ProjectRequests, projA, ClassOK)
			}
			// Half the wait for one export: far more than the recordings
			// take by themselves, a second on a machine that is slow, and
			// less than any of them would take if it waited.
			if d := time.Since(start); d > exportTimeout/2 {
				t.Errorf("40000 recordings took %s with the collector dead and exports that last %s, want them not to wait for it", d, exportTimeout)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			start = time.Now()
			_ = tel.Shutdown(ctx)
			// The same bound: a stop that waited for the export under way
			// instead of its own deadline would take the rest of the
			// minute.
			if d := time.Since(start); d > exportTimeout/2 {
				t.Errorf("Shutdown took %s with a 1s deadline and an export that lasts %s", d, exportTimeout)
			}
			if n := strings.Count(log.String(), "export failed"); n > 1 {
				t.Errorf("%d export failures logged, want at most 1:\n%s", n, log)
			}
		})
	}
}

// failing is an exporter whose every export fails with err.
type failing struct {
	sdkmetric.Exporter
	err error
}

func (f failing) Export(context.Context, *metricdata.ResourceMetrics) error { return f.err }

// An export cut short because the daemon is stopping is not a failure of
// the collector's: it is neither logged nor counted. Any other is both.
func TestExportCanceledAtStop(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		err     error
		dropped float64
	}{
		{"canceled", fmt.Errorf("post: %w", context.Canceled), 0},
		{"a deadline", fmt.Errorf("post: %w", context.DeadlineExceeded), 1},
		{"refused", errors.New("connection refused"), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := &syncBuffer{}
			tel, m, err := NewManual(Config{Logger: logTo(log), Ops: testOps})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
			g := &guarded{Exporter: failing{err: tc.err}, t: tel}
			if err := g.Export(t.Context(), &metricdata.ResourceMetrics{}); err != nil {
				t.Errorf("Export = %v, want nil: the failure is handled here", err)
			}
			if got := collect(t, m).Value("aries.telemetry.dropped", "signal", "metrics"); got != tc.dropped {
				t.Errorf("aries.telemetry.dropped = %v, want %v", got, tc.dropped)
			}
			if logged := strings.Contains(log.String(), "export failed"); logged != (tc.dropped > 0) {
				t.Errorf("export failure logged = %t, want %t:\n%s", logged, tc.dropped > 0, log)
			}
		})
	}
}

// Only a model the caller admitted is a value of the model attribute:
// any other, a hostile string or an admitted one past MaxModels
// included, is (other), so the series stay bounded.
func TestUsageTokensModelsAreAdmitted(t *testing.T) {
	t.Parallel()
	tel, m, err := NewManual(Config{PerProject: false})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	for _, bad := range []string{"", ModelOther, "has space", "bad\nline", strings.Repeat("a", 129)} {
		if tel.AdmitModel(bad) {
			t.Errorf("AdmitModel(%q) = true, want false", bad)
		}
	}
	if !tel.AdmitModel("example-large") || !tel.AdmitModel("example-large") {
		t.Fatal("AdmitModel(example-large) = false, want true, twice")
	}
	tel.Record(UsageTokens, 5, "", TokenInput, "example-large")
	tel.Record(UsageTokens, 7, "", TokenInput, "not-admitted")
	tel.Record(UsageTokens, 1, "", "bogus-kind", "example-large")
	snap := collect(t, m)
	if v := snap.Sum("aries.usage.tokens", "kind", TokenInput, "model", "example-large"); v != 5 {
		t.Errorf("tokens of example-large = %v, want 5", v)
	}
	if v := snap.Sum("aries.usage.tokens", "kind", TokenInput, "model", ModelOther); v != 7 {
		t.Errorf("tokens of a model not admitted = %v, want 7 under %s", v, ModelOther)
	}
	if v := snap.Sum("aries.usage.tokens", "kind", Other); v != 1 {
		t.Errorf("tokens of an unknown kind = %v, want 1 under %s", v, Other)
	}
	for i := range MaxModels + 10 {
		tel.AdmitModel(fmt.Sprintf("model-%d", i))
	}
	if tel.AdmitModel("one-too-many") {
		t.Errorf("AdmitModel past MaxModels = true, want false")
	}
}
