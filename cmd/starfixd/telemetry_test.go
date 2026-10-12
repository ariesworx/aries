package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/server"
)

// logBuffer is a log sink read while the daemon still writes to it.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// noLeak fails the test if, once its cleanups have run, more goroutines
// are running than when it was called, waiting for them to settle.
func noLeak(t *testing.T) {
	t.Helper()
	before := runtime.NumGoroutine()
	t.Cleanup(func() {
		deadline := time.Now().Add(15 * time.Second)
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

// freeAddr returns a loopback address with a port that was free a moment
// ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

// httpGet fetches url on a connection of its own, and returns the status
// and body, or 0 when nothing answers.
func httpGet(url string) (int, string) {
	tr := &http.Transport{DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Get(url) //nolint:noctx // a test's own loopback URL
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// daemon runs runDaemon for the site, as serve does, on a socket of its
// own and with env as its environment. stop ends it and returns what it
// returned.
type running struct {
	sock string
	log  *logBuffer
	stop func() error
}

func runSite(t *testing.T, s *site, env map[string]string) *running {
	t.Helper()
	// Not t.TempDir, whose path can be too long for a unix socket.
	dir, err := os.MkdirTemp("", "sft")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	set := s.settings(t)
	set.Socket = filepath.Join(dir, "run", "d.sock")
	r := &running{sock: set.Socket, log: &logBuffer{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runDaemon(ctx, set, slog.New(slog.NewTextHandler(r.log, nil)), func(k string) string { return env[k] })
	}()
	var once sync.Once
	var stopErr error
	r.stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case stopErr = <-done:
			case <-time.After(30 * time.Second):
				stopErr = fmt.Errorf("runDaemon did not return 30s after its context ended")
			}
		})
		return stopErr
	}
	t.Cleanup(func() {
		if err := r.stop(); err != nil {
			t.Errorf("runDaemon = %v, want nil", err)
		}
	})
	return r
}

// status runs `starfixd status` against the daemon on sock.
func status(t *testing.T, s *site, sock string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	env := adminEnv{stdin: strings.NewReader(""), stdout: &out, stderr: &errb, getenv: func(k string) string { return s.env[k] }}
	err := statusCmd(t.Context(), env, append([]string{"--config", s.cfg, "--socket", sock}, args...))
	return result{out.String(), errb.String(), err}
}

const secretToken = "Bearer-s3cr3t-T0KEN-fedcba9876543210"

// The daemon as serve runs it, with both ways out for metrics: it pushes
// to the collector the environment names, serves /metrics, /healthz and
// /readyz on the loopback listener, answers `starfixd status`, logs no
// token, and stops cleanly, leaving nothing running.
func TestRunDaemonWithTelemetry(t *testing.T) {
	noLeak(t)
	var mu sync.Mutex
	var exports [][]byte
	var auth string
	col := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		exports, auth = append(exports, b), r.Header.Get("Authorization")
		mu.Unlock()
		// A collector that refuses, and repeats the credential it was sent.
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "no access for "+r.Header.Get("Authorization")) //nolint:gosec // a fake collector, repeating what it was sent on purpose
	}))
	t.Cleanup(col.Close)
	exported := func(name string) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, b := range exports {
			if bytes.Contains(b, []byte(name)) {
				return true
			}
		}
		return false
	}

	s := newSite(t, "")
	addr := freeAddr(t)
	rewrite(t, s, "project: "+adopted+"\nadmins: [dana]\ntelemetry:\n  listen: "+addr+"\nlimits:\n  telemetry_requests: 4\n")
	d := runSite(t, s, map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": col.URL, "OTEL_METRIC_EXPORT_INTERVAL": "50",
		"OTEL_EXPORTER_OTLP_HEADERS": "Authorization=" + secretToken,
	})
	base := "http://" + addr

	// The listener answers before the daemon is ready, and says which.
	waitFor(t, "/healthz to answer", func() bool { code, _ := httpGet(base + "/healthz"); return code == 200 })
	waitFor(t, "/readyz to answer ready", func() bool {
		code, body := httpGet(base + "/readyz")
		if code == 503 && !strings.HasPrefix(body, "not ready: ") {
			t.Errorf("/readyz = 503 %q, want one line saying what is not ready", body)
		}
		return code == 200
	})
	if w := hello(t, d.sock, "alice", adopted); w.Err != nil {
		t.Fatalf("hello: %v", w.Err)
	}
	if w := hello(t, d.sock, "alice", second); w.Err == nil {
		t.Fatalf("hello for a project the server does not serve was welcomed")
	}

	_, text := httpGet(base + "/metrics")
	for _, want := range []string{
		`starfix_handshakes_total{result="ok"} 1`,
		`starfix_handshakes_total{result="not_found"} 1`,
		"starfix_stores_open 1",
		`starfix_telemetry_dropped_total{signal="metrics"}`,
		"go_goroutine_count",
		`service_name="starfixd"`,
	} {
		waitFor(t, "/metrics to carry "+want, func() bool {
			_, text = httpGet(base + "/metrics")
			return strings.Contains(text, want)
		})
	}
	if strings.Contains(text, "alice") || strings.Contains(text, second) {
		t.Errorf("/metrics names a principal, or a project the server does not serve:\n%s", text)
	}
	if code, _ := httpGet(base + "/debug/vars"); code != 404 {
		t.Errorf("GET /debug/vars = %d, want 404", code)
	}
	waitFor(t, "an export of the handshakes to reach the collector", func() bool { return exported("starfix.handshakes") })
	mu.Lock()
	sent := auth
	mu.Unlock()
	if sent != secretToken {
		t.Errorf("the collector was sent Authorization %q, want the token from the environment", sent)
	}

	// `starfixd status`, as text and as one JSON document.
	res := status(t, s, d.sock)
	// How many connections it counts depends on whether the daemon has
	// yet seen the hellos above hang up, so the count is not checked.
	if res.err != nil || !strings.HasPrefix(res.stdout, "starfixd ") || !strings.Contains(res.stdout, "stores 1 of 1, connections ") ||
		strings.Contains(res.stdout, "1970-01-01") ||
		!strings.Contains(res.stdout, "PROJECT") || !strings.Contains(res.stdout, adopted) {
		t.Errorf("starfixd status = %v, printed:\n%s", res.err, res.stdout)
	}
	res = status(t, s, d.sock, "--json")
	var doc proto.StatusResult
	if err := json.Unmarshal([]byte(res.stdout), &doc); res.err != nil || err != nil || strings.Count(res.stdout, "\n") != 1 {
		t.Fatalf("starfixd status --json = %v, %v; want exactly one JSON document, got %q", res.err, err, res.stdout)
	}
	if doc.Server == nil || doc.Server.OpenStores != 1 || len(doc.Projects) != 1 || doc.Projects[0].ID != adopted || !doc.Projects[0].Open {
		t.Errorf("starfixd status --json = %+v, want the server and its one project, open", doc)
	}

	if err := d.stop(); err != nil {
		t.Fatalf("runDaemon = %v, want nil", err)
	}
	if code, _ := httpGet(base + "/healthz"); code != 0 {
		t.Errorf("GET /healthz after the stop = %d, want the listener closed", code)
	}
	out := d.log.String()
	for _, want := range []string{`msg="telemetry on"`, "telemetry listener serving", "a metrics export failed", "starfixd serving"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, secretToken) || strings.Contains(out, "s3cr3t") {
		t.Errorf("the log holds the token:\n%s", out)
	}
	if n := strings.Count(out, "a metrics export failed"); n != 1 {
		t.Errorf("%d export failures logged, want 1:\n%s", n, out)
	}

	// With no daemon, status says so, and with --json as one document.
	res = status(t, s, d.sock)
	if res.err == nil || !strings.Contains(res.err.Error(), "fix: ") || res.stdout != "" {
		t.Errorf("starfixd status with no daemon = %v, printed %q; want an error with a fix and nothing printed", res.err, res.stdout)
	}
	res = status(t, s, d.sock, "--json")
	var failed map[string]proto.Error
	if err := json.Unmarshal([]byte(res.stdout), &failed); err != nil || failed["error"].Code != proto.CodeUnavailable ||
		res.err != exitError(1) || strings.Count(res.stdout, "\n") != 1 {
		t.Errorf("starfixd status --json with no daemon = %v, printed %q; want one {\"error\"} document and exit 1", res.err, res.stdout)
	}
}

// With nothing configured the daemon is the daemon it was: no listener,
// no export, no line about telemetry, and no goroutine left behind.
func TestRunDaemonTelemetryOffByDefault(t *testing.T) {
	noLeak(t)
	s := newSite(t, "")
	rewrite(t, s, "project: "+adopted+"\nadmins: [dana]\n")
	d := runSite(t, s, nil)
	waitFor(t, "the daemon to answer its socket", func() bool {
		_, err := server.Probe(t.Context(), d.sock, adopted, "test")
		return err == nil
	})
	if err := d.stop(); err != nil {
		t.Fatalf("runDaemon = %v, want nil", err)
	}
	if out := d.log.String(); strings.Contains(out, "telemetry") || strings.Contains(out, "systemd") {
		t.Errorf("a daemon with nothing configured logged about telemetry or systemd:\n%s", out)
	}
}

// OTEL_SDK_DISABLED turns the metrics off and leaves the health checks:
// the listener answers /healthz and /readyz, and /metrics is 404.
func TestRunDaemonSDKDisabled(t *testing.T) {
	noLeak(t)
	s := newSite(t, "")
	addr := freeAddr(t)
	rewrite(t, s, "project: "+adopted+"\nadmins: [dana]\ntelemetry:\n  listen: "+addr+"\n")
	d := runSite(t, s, map[string]string{"OTEL_SDK_DISABLED": "true", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:1"})
	base := "http://" + addr
	waitFor(t, "/readyz to answer ready", func() bool { code, _ := httpGet(base + "/readyz"); return code == 200 })
	if code, _ := httpGet(base + "/healthz"); code != 200 {
		t.Errorf("GET /healthz = %d, want 200", code)
	}
	if code, _ := httpGet(base + "/metrics"); code != 404 {
		t.Errorf("GET /metrics with OTEL_SDK_DISABLED = %d, want 404", code)
	}
	if err := d.stop(); err != nil {
		t.Fatalf("runDaemon = %v, want nil", err)
	}
	out := d.log.String()
	if !strings.Contains(out, "telemetry is off: OTEL_SDK_DISABLED is true") || !strings.Contains(out, "metrics are off") ||
		strings.Contains(out, "export failed") {
		t.Errorf("want the log to say telemetry is off, and no export tried:\n%s", out)
	}
}

// A listener that cannot be bound fails the start, with a fix, before
// the database is touched.
func TestRunDaemonListenerFailsTheStart(t *testing.T) {
	noLeak(t)
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	set := server.Settings{DSN: "nobody@tcp(127.0.0.1:1)/nowhere", Project: adopted, Socket: filepath.Join(t.TempDir(), "d.sock")}
	set.Telemetry.Listen = held.Addr().String()
	set.Limits = server.DefaultLimits
	err = runDaemon(t.Context(), set, slog.New(slog.DiscardHandler), func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "telemetry listen address") || !strings.Contains(err.Error(), "fix: free the port") {
		t.Errorf("runDaemon with the telemetry port taken = %v, want a refusal naming the address and a fix", err)
	}
	// A variable it cannot honor fails the start too.
	set.Telemetry.Listen = ""
	err = runDaemon(t.Context(), set, slog.New(slog.DiscardHandler), func(k string) string {
		return map[string]string{"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4317"}[k]
	})
	if err == nil || !strings.Contains(err.Error(), "OTLP over HTTP only") || !strings.Contains(err.Error(), "fix: ") {
		t.Errorf("runDaemon with OTEL_EXPORTER_OTLP_PROTOCOL=grpc = %v, want a refusal with a fix", err)
	}
}

func TestStatusUsage(t *testing.T) {
	for _, args := range [][]string{{"extra"}, {"--project", "x"}, {"-n", "3"}} {
		var out bytes.Buffer
		err := statusCmd(t.Context(), adminEnv{stdout: &out, stderr: io.Discard, getenv: func(string) string { return "" }}, args)
		if _, ok := err.(usageError); !ok || out.Len() != 0 { //nolint:errorlint // the command returns it unwrapped
			t.Errorf("starfixd status %v = %v, printed %q; want a usage error and nothing printed", args, err, out.String())
		}
	}
}

// Whoever cannot read the daemon's config file is told who can run the
// command, as text and as the one JSON document.
func TestStatusUnreadableConfig(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a file this user cannot read")
	}
	cfg := filepath.Join(t.TempDir(), "starfixd.yaml")
	if err := os.WriteFile(cfg, []byte("socket: /nonexistent/d.sock\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	env := func(out io.Writer) adminEnv {
		return adminEnv{stdout: out, stderr: io.Discard, getenv: func(string) string { return "" }}
	}
	var out bytes.Buffer
	err := statusCmd(t.Context(), env(&out), []string{"--config", cfg})
	if err == nil || !strings.Contains(err.Error(), "permission denied") || !strings.Contains(err.Error(), "fix: run it as the user starfixd runs as") || out.Len() != 0 {
		t.Errorf("starfixd status with an unreadable config = %v, printed %q; want the refusal with a fix, and nothing printed", err, out.String())
	}
	out.Reset()
	err = statusCmd(t.Context(), env(&out), []string{"--json", "--config", cfg})
	var doc map[string]proto.Error
	if jerr := json.Unmarshal(out.Bytes(), &doc); jerr != nil || err != exitError(1) || !strings.Contains(doc["error"].Message, "fix: run it as the user starfixd runs as") {
		t.Errorf("starfixd status --json with an unreadable config = %v, printed %q; want one {\"error\"} document naming the fix", err, out.String())
	}
}
