package telemetry

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/ariesworx/aries/internal/clienttest"
	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/proto"
)

// Invented ids, in the shapes an exported record takes.
const (
	logProject     = "0190a8a0-0000-7000-8000-00000000000a"
	logFingerprint = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	logReq         = "00000000000000aa"
)

// logged makes a Telemetry whose exported log records a test reads in
// process, and the logger that feeds it as ariesd's does.
func logged(t *testing.T, cfg Config) (*Telemetry, *Manual, *slog.Logger) {
	t.Helper()
	tel, m := traced(t, cfg)
	h := logs.NewHandler(slog.DiscardHandler, IDs)
	h.Tap(tel.LogHandler())
	return tel, m, slog.New(journal{h})
}

// journal is a handler that takes every level, as a journal at debug.
type journal struct{ *logs.Handler }

func (journal) Enabled(context.Context, slog.Level) bool { return true }

func (j journal) WithAttrs(as []slog.Attr) slog.Handler {
	return journal{j.Handler.WithAttrs(as).(*logs.Handler)}
}

// oneLog returns the only record exported, failing the test otherwise.
func oneLog(t *testing.T, m *Manual) LogData {
	t.Helper()
	recs := m.Logs()
	if len(recs) != 1 {
		t.Fatalf("%d log records exported, want 1: %+v", len(recs), recs)
	}
	return recs[0]
}

// An exported record says who as telemetry: principals: allows, exactly
// as a span does (decision 32): stand-ins by default, names when asked,
// nothing with omit, and nothing, never a name, when the key for
// stand-ins is unavailable. Every field that names a person follows it.
func TestExportedLogsFollowPrincipals(t *testing.T) {
	t.Parallel()
	noKey := filepath.Join(t.TempDir(), "no such directory", "principals.key")
	for _, tc := range []struct {
		name string
		cfg  Config
		// want is what the record carries of each field: "" for absent,
		// "p_" or "s_" for a stand-in.
		want map[string]string
	}{
		{"pseudonym, the default", Config{},
			map[string]string{"principal": "p_", "session": "s_", "by": "p_", "subject": "p_", "admin": "p_", "created_by": "p_", "added_by": "p_"}},
		{"name", Config{Principals: PrincipalsName},
			map[string]string{"principal": "alice", "session": "s-abcdefgh", "by": "dana", "subject": "bob", "admin": "dana", "created_by": "erin", "added_by": "frank"}},
		{"omit", Config{Principals: PrincipalsOmit},
			map[string]string{"principal": "", "session": "", "by": "", "subject": "", "admin": "", "created_by": "", "added_by": ""}},
		{"pseudonym with no key", Config{KeyFile: noKey},
			map[string]string{"principal": "", "session": "", "by": "", "subject": "", "admin": "", "created_by": "", "added_by": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tel, m, log := logged(t, tc.cfg)
			if tc.cfg.KeyFile != "" && tel.People().StandIns {
				t.Fatalf("the test's key file %s was made: it tests nothing", tc.cfg.KeyFile)
			}
			log.With("principal", "alice", "session", "s-abcdefgh").Info("an access change", "event", logs.AuditMemberSet,
				"by", "dana", "subject", "bob", "admin", "dana", "created_by", "erin", "added_by", "frank",
				"target", "proj/bob", "err", "bob is not alice", "machine", "alice-laptop")
			got := oneLog(t, m)
			for k, want := range tc.want {
				v := got.Attrs[k]
				switch want {
				case "p_", "s_":
					if !strings.HasPrefix(v, want) || len(v) != 18 {
						t.Errorf("%s = %q, want a stand-in: %s and 16 hex digits", k, v, want)
					}
				default:
					if v != want {
						t.Errorf("%s = %q, want %q", k, v, want)
					}
				}
			}
			// Text stays in the journal whatever the setting, and under
			// every setting but name no name leaves in any field.
			for _, k := range []string{"target", "err", "machine"} {
				if v, ok := got.Attrs[k]; ok {
					t.Errorf("the record carries %s = %q: text is never exported", k, v)
				}
			}
			if tc.cfg.Principals != PrincipalsName {
				all := got.Body + fmt.Sprint(got.Attrs)
				for _, name := range []string{"alice", "bob", "dana", "erin", "frank", "s-abcdefgh"} {
					if strings.Contains(all, name) {
						t.Errorf("the record holds %q: %+v", name, got)
					}
				}
			}
			// The same person is the same stand-in as on a span.
			if want := tel.who.principal("alice"); got.Attrs["principal"] != want {
				t.Errorf("principal = %q, and a span would say %q", got.Attrs["principal"], want)
			}
		})
	}
}

// What a record may carry is decided here, field by field (THE RULE of
// trace.go, for logs): each id only in its own shape, words and numbers
// as they are, and anything else not at all.
func TestExportedLogFieldsAreVetted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		key   string
		value any
		want  string // "" for a field that is dropped
	}{
		{"a project's id", "project", logProject, logProject},
		{"a project that is no UUID", "project", "PLANTED text", ""},
		{"an op the server knows", "op", "show", "show"},
		{"an op it does not", "op", "PLANTED.op", "unknown"},
		{"a code", "code", "invalid", "invalid"},
		{"a code that is none", "code", "PLANTED", "other"},
		{"a request's id", "req", logReq, logReq},
		{"a request's id of another shape", "req", "PLANTED", ""},
		{"an issue's id", "issue", "ar-a1b2", "ar-a1b2"},
		{"an issue's id with a child number", "issue", "ar-a1b2.3", "ar-a1b2.3"},
		{"an issue's id that is none", "issue", "PLANTED text", ""},
		{"a fingerprint", "fingerprint", logFingerprint, logFingerprint},
		{"a fingerprint that is a key", "fingerprint", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA", ""},
		{"a number", "dur_ms", int64(12), "12"},
		{"a count", "connections", 3, "3"},
		{"a boolean", "audit", true, "true"},
		{"a duration", "after", 90 * time.Second, "1m30s"},
		{"a time", "at", time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC), "2026-10-12T09:30:00Z"},
		{"a word", "reason", "membership", "membership"},
		{"a few words", "reason", "membership removed", "membership removed"},
		{"a word that is a sentence", "reason", "PLANTED text, with punctuation", ""},
		{"a word that is a path", "reason", "/home/PLANTED", ""},
		{"a word that is too long", "reason", strings.Repeat("a", 65), ""},
		{"an error", "err", "refused PLANTED text", ""},
		{"a name", "name", "acme", ""},
		{"a field of no schema", "anything_else", "PLANTED", ""},
		{"a trace id is the span's, not a field", "trace_id", remoteTrace, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, m, log := logged(t, Config{})
			log.Info("a line", "event", logs.RequestRefused, tc.key, tc.value)
			got := oneLog(t, m)
			if v, ok := got.Attrs[tc.key]; v != tc.want || ok != (tc.want != "") {
				t.Errorf("exported %s = %q (present %t), want %q", tc.key, v, ok, tc.want)
			}
			if strings.Contains(got.Body+fmt.Sprint(got.Attrs), "PLANTED") {
				t.Errorf("the record holds the planted text: %+v", got)
			}
		})
	}
}

// A record has the line's message, level and event, and the ids of the
// span it was logged under, from the span itself.
func TestExportedLogShape(t *testing.T) {
	t.Parallel()
	tel, m, log := logged(t, Config{})
	ctx, sp := tel.StartRequest(t.Context(), "show", proto.TraceContext{})
	traceID, spanID, _ := sp.IDs()
	log.WarnContext(logs.WithReq(ctx, logReq), "request", "event", logs.RequestRefused, "op", "show", "code", "not_found")
	sp.End()
	got := oneLog(t, m)
	want := LogData{Body: "request", Event: string(logs.RequestRefused), Severity: "WARN", TraceID: traceID, SpanID: spanID,
		Attrs: map[string]string{"event": string(logs.RequestRefused), "op": "show", "code": "not_found", "req": logReq}}
	if got.Body != want.Body || got.Event != want.Event || got.Severity != want.Severity || got.TraceID != want.TraceID ||
		got.SpanID != want.SpanID || !maps.Equal(got.Attrs, want.Attrs) {
		t.Errorf("exported record = %+v, want %+v", got, want)
	}

	m.ResetLogs()
	log.Error("a line of no event")
	log.Info("a line of an event that is not listed", "event", "PLANTED.event")
	for _, r := range m.Logs() {
		if r.Event != "" || r.Attrs["event"] != "" {
			t.Errorf("record %+v names an event that is not of the list", r)
		}
	}
	if n := len(m.Logs()); n != 2 {
		t.Errorf("%d records exported, want 2", n)
	}
}

// Logs go out over OTLP only when OTEL_LOGS_EXPORTER asks for it
// (design §16.2: the exporter is experimental upstream), to /v1/logs
// under the endpoint, with the resource of the other signals.
func TestLogPush(t *testing.T) {
	// Serial: counts the process's goroutines (noLeak).
	noLeak(t)
	col := newCollector(t, http.StatusOK, nil)
	out := &syncBuffer{}
	tel, err := New(t.Context(), Config{Version: "v1", Logger: logTo(out), Ops: testOps, KeyFile: filepath.Join(t.TempDir(), "principals.key"),
		Getenv: env{
			"OTEL_EXPORTER_OTLP_ENDPOINT": col.URL, "OTEL_EXPORTER_OTLP_LOGS_HEADERS": "x-test=one%20two", "OTEL_LOGS_EXPORTER": "otlp",
			"OTEL_METRICS_EXPORTER": "none", "OTEL_TRACES_EXPORTER": "none", "OTEL_BLRP_SCHEDULE_DELAY": "60000",
		}.get})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !tel.Logging() || tel.Tracing() || tel.Enabled() {
		t.Fatalf("Logging() = %t, Tracing() = %t, Enabled() = %t; want logs alone", tel.Logging(), tel.Tracing(), tel.Enabled())
	}
	h := logs.NewHandler(slog.DiscardHandler, IDs)
	h.Tap(tel.LogHandler())
	slog.New(journal{h}).Info("claim expired", "event", logs.ClaimExpired, "principal", "alice", "session", "s-abcdefgh", "issue", "ar-a1b2")
	sctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := tel.Shutdown(sctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	if col.count() != 1 {
		t.Fatalf("%d exports reached the collector after the stop, want the 1 that flushes", col.count())
	}
	col.mu.Lock()
	body, hdr, path := col.bodies[0], col.headers[0], col.paths[0]
	col.mu.Unlock()
	if path != "/v1/logs" {
		t.Errorf("export posted to %q, want /v1/logs", path)
	}
	if got := hdr.Get("x-test"); got != "one two" {
		t.Errorf("export header x-test = %q, want %q", got, "one two")
	}
	for _, s := range []string{"claim expired", "claim.expired", "ar-a1b2", "service.name", "ariesd", "service.instance.id", "p_", "s_"} {
		if !bytes.Contains(body, []byte(s)) {
			t.Errorf("the export does not carry %q", s)
		}
	}
	for _, s := range []string{"alice", "s-abcdefgh"} {
		if bytes.Contains(body, []byte(s)) {
			t.Errorf("the export carries %q with telemetry.principals at its default, pseudonym", s)
		}
	}
	if o := out.String(); !strings.Contains(o, `msg="telemetry on" event=telemetry.on `) || !strings.Contains(o, "logs="+col.URL+"/v1/logs") {
		t.Errorf("the start-up line does not say where logs go:\n%s", o)
	}
}

func TestReadEnvLogs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		env     env
		want    string // the endpoint logs go to; "" for none
		wantErr string
	}{
		{"nothing set", env{}, "", ""},
		{"an endpoint alone exports no logs", env{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318"}, "", ""},
		{"asked for, under the general endpoint", env{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318", "OTEL_LOGS_EXPORTER": "otlp"},
			"http://127.0.0.1:4318/v1/logs", ""},
		{"asked for, with an endpoint of its own", env{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "http://127.0.0.1:4318/logs", "OTEL_LOGS_EXPORTER": "otlp"},
			"http://127.0.0.1:4318/logs", ""},
		{"its own endpoint without being asked for", env{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "http://127.0.0.1:4318/logs"}, "", ""},
		{"none", env{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318", "OTEL_LOGS_EXPORTER": "none"}, "", ""},
		{"asked for with nowhere to send", env{"OTEL_LOGS_EXPORTER": "otlp"}, "", "names a collector"},
		{"another exporter", env{"OTEL_LOGS_EXPORTER": "console"}, "", "otlp only"},
		{"gRPC", env{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4317", "OTEL_LOGS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL": "grpc"},
			"", "OTLP over HTTP only"},
		{"a queue past the cap", env{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318", "OTEL_LOGS_EXPORTER": "otlp", "OTEL_BLRP_MAX_QUEUE_SIZE": "65537"},
			"", "OTEL_BLRP_MAX_QUEUE_SIZE"},
		{"a batch past its queue", env{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318", "OTEL_LOGS_EXPORTER": "otlp",
			"OTEL_BLRP_MAX_QUEUE_SIZE": "10", "OTEL_BLRP_MAX_EXPORT_BATCH_SIZE": "11"}, "", "OTEL_BLRP_MAX_EXPORT_BATCH_SIZE"},
		{"a delay that is no number", env{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318", "OTEL_LOGS_EXPORTER": "otlp", "OTEL_BLRP_SCHEDULE_DELAY": "soon"},
			"", "OTEL_BLRP_SCHEDULE_DELAY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := readEnv(tc.env.get)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "fix: ") {
					t.Fatalf("readEnv error = %v, want one naming %q with a fix", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("readEnv: %v", err)
			}
			if c.logs.endpoint != tc.want {
				t.Errorf("logs go to %q, want %q", c.logs.endpoint, tc.want)
			}
		})
	}
	c, err := readEnv(env{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318", "OTEL_LOGS_EXPORTER": "otlp"}.get)
	if err != nil {
		t.Fatal(err)
	}
	if c.logs.queue != 2048 || c.logs.batch != 512 || c.logs.delay != time.Second || c.logs.exportTimeout != 30*time.Second {
		t.Errorf("logs are batched as %+v, want the specification's defaults: 2048, 512, 1s, 30s", c.logs)
	}
}

// With nothing asking for logs there is no exporter, no goroutine and no
// key file made, whatever else is exported.
func TestLogsOffByDefault(t *testing.T) {
	// Serial: counts the process's goroutines (noLeak).
	noLeak(t)
	col := newCollector(t, http.StatusOK, nil)
	key := filepath.Join(t.TempDir(), "principals.key")
	tel, err := New(t.Context(), Config{Ops: testOps, KeyFile: key,
		Getenv: env{"OTEL_EXPORTER_OTLP_ENDPOINT": col.URL, "OTEL_TRACES_EXPORTER": "none", "OTEL_METRIC_EXPORT_INTERVAL": "60000"}.get})
	if err != nil {
		t.Fatal(err)
	}
	if tel.Logging() || tel.LogHandler() != nil {
		t.Errorf("Logging() = %t and a handler %v with OTEL_LOGS_EXPORTER unset, want neither", tel.Logging(), tel.LogHandler())
	}
	var none *Telemetry
	if none.Logging() || none.LogHandler() != nil {
		t.Errorf("a nil Telemetry exports logs")
	}
	if err := tel.Shutdown(t.Context()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	col.mu.Lock()
	defer col.mu.Unlock()
	for _, p := range col.paths {
		if p == "/v1/logs" {
			t.Errorf("an export of logs reached the collector with OTEL_LOGS_EXPORTER unset")
		}
	}
}

// held is a log exporter that holds each export until released, and
// counts the records it was given.
type held struct {
	release chan struct{}
	mu      sync.Mutex
	got     int
	fail    bool
}

func (b *held) Export(ctx context.Context, recs []sdklog.Record) error {
	select {
	case <-b.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail {
		return fmt.Errorf("refused")
	}
	b.got += len(recs)
	return nil
}

func (*held) Shutdown(context.Context) error   { return nil }
func (*held) ForceFlush(context.Context) error { return nil }

// batched replaces tel's log export with a batch over exp.
func batched(t *testing.T, tel *Telemetry, exp sdklog.Exporter, le logEnv) *slog.Logger {
	t.Helper()
	tel.logging(resource.Empty(), tel.logBatcher(exp, le))
	h := logs.NewHandler(slog.DiscardHandler, IDs)
	h.Tap(tel.LogHandler())
	return slog.New(journal{h})
}

// Every record is exported or counted as dropped, exactly, on
// aries.telemetry.dropped{signal="logs"}.
func TestDroppedLogsAreCounted(t *testing.T) {
	// Serial: counts the process's goroutines (noLeak).
	for _, tc := range []struct {
		name string
		fail bool
	}{{"a slow exporter", false}, {"a slow exporter that then refuses", true}} {
		t.Run(tc.name, func(t *testing.T) {
			noLeak(t)
			tel, m := traced(t, Config{})
			exp := &held{release: make(chan struct{}), fail: tc.fail}
			log := batched(t, tel, exp, logEnv{queue: 50, batch: 10, delay: time.Millisecond, exportTimeout: 30 * time.Second})
			const lines = 500
			start := time.Now()
			for range lines {
				log.Info("a line", "event", logs.RequestDone)
			}
			if d := time.Since(start); d > 5*time.Second {
				t.Errorf("%d lines took %s with the exporter held: logging waited for it", lines, d)
			}
			close(exp.release)
			if err := tel.stopLogs(t.Context()); err != nil {
				t.Fatalf("stopLogs: %v", err)
			}
			dropped := int(collect(t, m).Value("aries.telemetry.dropped", "signal", "logs"))
			if tc.fail {
				if dropped != lines {
					t.Errorf("dropped = %d with an exporter that refuses, want all %d", dropped, lines)
				}
				return
			}
			exp.mu.Lock()
			got := exp.got
			exp.mu.Unlock()
			if got+dropped != lines {
				t.Errorf("%d exported and %d dropped of %d lines: every one must be one or the other", got, dropped, lines)
			}
			if dropped < lines-50-10-10 || dropped == 0 {
				t.Errorf("dropped = %d of %d with a queue of 50 and an exporter that was held: the queue is not bounded", dropped, lines)
			}
		})
	}
}

// A stop whose deadline passes while an export hangs cuts it short,
// counts what it abandoned, and leaves nothing running.
func TestStopCutsShortASlowLogExport(t *testing.T) {
	// Serial: counts the process's goroutines (noLeak).
	noLeak(t)
	tel, m := traced(t, Config{})
	exp := &held{release: make(chan struct{})} // never released
	log := batched(t, tel, exp, logEnv{queue: 100, batch: 10, delay: time.Millisecond, exportTimeout: time.Minute})
	const lines = 35
	for range lines {
		log.Info("a line", "event", logs.RequestDone)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := tel.stopLogs(ctx)
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the stop took %s with a deadline of 200ms and an export that never returns", d)
	}
	if err == nil {
		t.Errorf("stopLogs = nil, want it to say the deadline passed")
	}
	if dropped := int(collect(t, m).Value("aries.telemetry.dropped", "signal", "logs")); dropped != lines {
		t.Errorf("dropped = %d, want the %d records the stop abandoned", dropped, lines)
	}
}

// A collector that is down, and one that accepts and never answers, hold
// neither a log line nor a stop; the queue is bounded, what it could not
// take is counted, and the failure is logged once, not once a line: the
// warning is itself a line that is exported, and must not feed itself.
func TestDeadCollectorLogs(t *testing.T) {
	// Serial: counts the process's goroutines (noLeak).
	closedAddr := clienttest.DeadPort(t)
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = silent.Close() })

	const lines = 20000
	for _, tc := range []struct{ name, addr string }{{"closed port", closedAddr}, {"silent listener", silent.Addr().String()}} {
		t.Run(tc.name, func(t *testing.T) {
			noLeak(t)
			out := &syncBuffer{}
			h := logs.NewHandler(slog.NewTextHandler(out, nil), IDs)
			log := slog.New(h)
			tel, err := New(t.Context(), Config{Logger: log, Ops: testOps, Pull: true, KeyFile: filepath.Join(t.TempDir(), "principals.key"), Getenv: env{
				"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "http://" + tc.addr + "/v1/logs", "OTEL_LOGS_EXPORTER": "otlp",
				"OTEL_BLRP_SCHEDULE_DELAY": "20", "OTEL_BLRP_EXPORT_TIMEOUT": "150", "OTEL_EXPORTER_OTLP_TIMEOUT": "150",
				"OTEL_BLRP_MAX_QUEUE_SIZE": "256", "OTEL_BLRP_MAX_EXPORT_BATCH_SIZE": "64",
			}.get})
			if err != nil {
				t.Fatal(err)
			}
			h.Tap(tel.LogHandler())
			quiet := slog.New(journal{logs.NewHandler(slog.DiscardHandler, IDs)})
			quiet.Handler().(journal).Tap(tel.LogHandler())
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			start := time.Now()
			for range lines {
				quiet.Info("claim expired", "event", logs.ClaimExpired, "principal", "alice", "issue", "ar-a1b2")
			}
			// Lines that waited for the export of their batch would wait
			// its timeout, 150 ms, once for each batch of 64: about 47
			// seconds over these. Half of that is the bound. The lines
			// themselves take well under a second, and took five where
			// the test had a quarter of a slow processor.
			const waited = lines / 64 * 150 * time.Millisecond
			if d := time.Since(start); d > waited/2 {
				t.Errorf("%d lines took %s with the collector dead, want them not to wait for it: waiting for each batch's export would take %s", lines, d, waited)
			}
			runtime.GC()
			runtime.ReadMemStats(&after)
			if after.HeapAlloc > before.HeapAlloc+32<<20 {
				t.Errorf("the heap grew from %d to %d MiB over %d lines with the collector dead: the queue is not bounded",
					before.HeapAlloc>>20, after.HeapAlloc>>20, lines)
			}
			t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			start = time.Now()
			_ = tel.stopLogs(ctx)
			if d := time.Since(start); d > 4*time.Second {
				t.Errorf("stopping the logs took %s with a 1s deadline", d)
			}
			if n := strings.Count(out.String(), "a log export failed"); n != 1 {
				t.Errorf("%d log export failures logged, want exactly 1:\n%s", n, out)
			}
			text := promText(t, tel)
			var dropped int
			if _, err := fmt.Sscanf(grep(text, `aries_telemetry_dropped_total{signal="logs"}`), `aries_telemetry_dropped_total{signal="logs"} %d`, &dropped); err != nil {
				t.Fatalf("no count of dropped log records on /metrics: %v\n%s", err, grep(text, "dropped"))
			}
			// The lines, and the few of the daemon's own that were
			// exported with them: the start-up line and the warning.
			if dropped < lines || dropped > lines+4 {
				t.Errorf("aries.telemetry.dropped{signal=logs} = %d, want every one of the %d lines", dropped, lines)
			}
		})
	}
}

// What exporting a line costs the goroutine that logged it.
func BenchmarkExportedLog(b *testing.B) {
	for _, on := range []bool{false, true} {
		b.Run(fmt.Sprintf("export=%t", on), func(b *testing.B) {
			tel, _, err := NewManual(Config{Ops: testOps})
			if err != nil {
				b.Fatal(err)
			}
			h := logs.NewHandler(slog.DiscardHandler, IDs)
			if on {
				tel.logging(resource.Empty(), tel.logBatcher(nowhere{}, logEnv{queue: 2048, batch: 512, delay: time.Second, exportTimeout: time.Second}))
				h.Tap(tel.LogHandler())
			}
			log := slog.New(journal{h}).With("project", logProject, "principal", "alice", "session", "s-abcdefgh")
			ctx := logs.WithReq(b.Context(), logReq)
			b.ReportAllocs()
			for b.Loop() {
				log.InfoContext(ctx, "request", "event", logs.RequestRefused, "op", "show", "dur_ms", 3, "code", "not_found")
			}
			_ = tel.Shutdown(context.Background())
		})
	}
}

// nowhere is a log exporter that keeps nothing.
type nowhere struct{}

func (nowhere) Export(context.Context, []sdklog.Record) error { return nil }
func (nowhere) Shutdown(context.Context) error                { return nil }
func (nowhere) ForceFlush(context.Context) error              { return nil }
