package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/server"
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
	// key is where the daemon keeps the key of principals' stand-ins: a
	// directory of the test's, made as the guide has the operator make
	// it, and never the home directory of whoever runs the tests.
	key string
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
	if !filepath.IsAbs(set.Telemetry.PrincipalsKey) {
		set.Telemetry.PrincipalsKey = filepath.Join(dir, "telemetry", "principals.key")
		if err := os.Mkdir(filepath.Dir(set.Telemetry.PrincipalsKey), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	r := &running{sock: set.Socket, log: &logBuffer{}, key: set.Telemetry.PrincipalsKey}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		// The logger serve makes, so that the lines are what a daemon
		// writes and can be exported.
		log, err := server.NewLogger(r.log, "info", "text")
		if err != nil {
			done <- err
			return
		}
		done <- runDaemon(ctx, set, log, func(k string) string { return env[k] })
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

// The daemon as serve runs it, with both ways out for metrics and with
// traces: it pushes both to the collector the environment names, makes
// the key of principals' stand-ins, serves /metrics, /healthz and
// /readyz on the loopback listener, answers `starfixd status`, logs no
// token, and stops cleanly, leaving nothing running.
func TestRunDaemonWithTelemetry(t *testing.T) {
	// Serial: counts the process's goroutines (noLeak).
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
		"OTEL_EXPORTER_OTLP_ENDPOINT": col.URL, "OTEL_METRIC_EXPORT_INTERVAL": "50", "OTEL_BSP_SCHEDULE_DELAY": "50",
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
		`starfix_telemetry_dropped_total{signal="traces"}`,
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
	// The handshakes' spans went too, under a stand-in for alice, made
	// with a key the daemon keeps where it was told to, for itself alone.
	waitFor(t, "an export of the handshake's span to reach the collector", func() bool { return exported("handshake") && exported("p_") })
	if exported("alice") {
		t.Errorf("an export names alice, with telemetry: principals: at its default")
	}
	if fi, err := os.Stat(d.key); err != nil || fi.Mode().Perm() != 0o600 || fi.Size() != 65 {
		t.Errorf("the stand-in key %s: %v, %v; want a file of mode 0600 holding 64 hex digits", d.key, fi, err)
	}
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
	for _, want := range []string{`msg="telemetry on"`, " traces=" + col.URL + "/v1/traces ", " sampler=parentbased_always_on ", " principals=pseudonym ",
		"telemetry listener serving", "a metrics export failed", "a trace export failed", "starfixd serving"} {
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
	if n := strings.Count(out, "a trace export failed"); n != 1 {
		t.Errorf("%d trace export failures logged, want 1:\n%s", n, out)
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
	// Serial: counts the process's goroutines (noLeak).
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
	// It made no key: nothing it exports needs one.
	if _, err := os.Stat(d.key); !os.IsNotExist(err) {
		t.Errorf("a daemon that exports no trace made the stand-in key %s (stat: %v)", d.key, err)
	}
}

// The key of principals' stand-ins is made only when traces are
// exported with stand-ins. A daemon that cannot make it starts and
// serves all the same (design §16: telemetry never fails the server):
// its spans say nothing about who, and its log and its status say why
// and what to do.
func TestRunDaemonStandInKey(t *testing.T) {
	// Serial: counts the process's goroutines (noLeak).
	traces := map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://127.0.0.1:1/v1/traces", "OTEL_BSP_SCHEDULE_DELAY": "60000"}
	t.Run("nowhere to make it", func(t *testing.T) {
		noLeak(t)
		var mu sync.Mutex
		var posted []byte
		col := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			posted = append(posted, b...)
			mu.Unlock()
		}))
		t.Cleanup(col.Close)
		exported := func(re string) bool {
			mu.Lock()
			defer mu.Unlock()
			return regexp.MustCompile(re).Match(posted)
		}
		key := filepath.Join(t.TempDir(), "telemetry", "principals.key")
		s := newSite(t, "")
		rewrite(t, s, "project: "+adopted+"\nadmins: [dana]\ntelemetry:\n  principals_key: "+key+"\n")
		// The guide's one variable, and nothing else done.
		d := runSite(t, s, map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": col.URL, "OTEL_BSP_SCHEDULE_DELAY": "50", "OTEL_METRICS_EXPORTER": "none"})
		waitFor(t, "the daemon to answer its socket", func() bool {
			_, err := server.Probe(t.Context(), d.sock, adopted, "test")
			return err == nil
		})
		if w := hello(t, d.sock, "alice", adopted); w.Err != nil {
			t.Fatalf("hello: %v", w.Err)
		}
		waitFor(t, "the handshake's span to reach the collector", func() bool { return exported("handshake") })
		if exported(`[ps]_[0-9a-f]{16}`) || exported("alice") {
			t.Errorf("with the key unavailable an export names someone, by a stand-in or by name")
		}
		res := status(t, s, d.sock)
		if res.err != nil || !strings.Contains(res.stdout, "TRACES SAY NOTHING ABOUT WHO: the key for stand-ins, "+key+", is unavailable: it cannot be made: no such file or directory") {
			t.Errorf("starfixd status = %v, and does not say the key is unavailable:\n%s", res.err, res.stdout)
		}
		if err := d.stop(); err != nil {
			t.Fatalf("runDaemon = %v, want nil", err)
		}
		out := d.log.String()
		for _, want := range []string{`level=ERROR msg="telemetry: spans say nothing about who`, `msg="the key's file" event=telemetry.key_unavailable path=` + key,
			`msg=cause event=telemetry.key_unavailable cause="it cannot be made: no such file or directory`,
			`msg=fix event=telemetry.key_unavailable fix="make its directory`, "ReadWritePaths=" + filepath.Dir(key), "principals: omit", "OTEL_TRACES_EXPORTER=none", "starfixd serving"} {
			if !strings.Contains(out, want) {
				t.Errorf("the log lacks %q:\n%s", want, out)
			}
		}
		if _, err := os.Stat(filepath.Dir(key)); !os.IsNotExist(err) {
			t.Errorf("the daemon made the key's directory itself (stat: %v)", err)
		}
	})
	for _, tc := range []struct {
		name, principals string
		env              map[string]string
		wantKey          bool
	}{
		{"traces with stand-ins", "", traces, true},
		{"traces with names", "name", traces, false},
		{"traces with nobody", "omit", traces, false},
		{"metrics alone", "", map[string]string{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://127.0.0.1:1/v1/metrics"}, false},
		{"traces set to none", "", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:1", "OTEL_TRACES_EXPORTER": "none"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noLeak(t)
			s := newSite(t, "")
			body := "project: " + adopted + "\nadmins: [dana]\n"
			if tc.principals != "" {
				body += "telemetry:\n  principals: " + tc.principals + "\n"
			}
			rewrite(t, s, body)
			d := runSite(t, s, tc.env)
			waitFor(t, "the daemon to answer its socket", func() bool {
				_, err := server.Probe(t.Context(), d.sock, adopted, "test")
				return err == nil
			})
			if _, err := os.Stat(d.key); (err == nil) != tc.wantKey {
				t.Errorf("the stand-in key exists: %t (stat: %v), want %t", err == nil, err, tc.wantKey)
			}
			if err := d.stop(); err != nil {
				t.Fatalf("runDaemon = %v, want nil", err)
			}
		})
	}
}

// OTEL_SDK_DISABLED turns the metrics off and leaves the health checks:
// the listener answers /healthz and /readyz, and /metrics is 404.
func TestRunDaemonSDKDisabled(t *testing.T) {
	// Serial: counts the process's goroutines (noLeak).
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
	// Serial: counts the process's goroutines (noLeak).
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
	t.Parallel()
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
	t.Parallel()
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

// A refusal is printed with its fix on a line of its own, since
// `systemctl status` cuts a long line and the fix is at its end.
func TestRefusalLines(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  string
		want []string
	}{
		{"no fix here", []string{"no fix here"}},
		{"the cause; fix: do this", []string{"the cause", "fix: do this"}},
		{"the cause; fix: do this; fix: or that", []string{"the cause", "fix: do this", "fix: or that"}},
	} {
		if got := refusalLines(errors.New(tc.err)); !slices.Equal(got, tc.want) {
			t.Errorf("refusalLines(%q) = %q, want %q", tc.err, got, tc.want)
		}
	}
	// The start refusals of the OTEL_* variables, as they are printed.
	set := server.Settings{DSN: "nobody@tcp(127.0.0.1:1)/nowhere", Project: adopted, Socket: filepath.Join(t.TempDir(), "d.sock")}
	set.Limits = server.DefaultLimits
	for _, env := range []map[string]string{
		{"OTEL_TRACES_SAMPLER": "jaeger_remote"},
		{"OTEL_TRACES_SAMPLER": "traceidratio", "OTEL_TRACES_SAMPLER_ARG": "half"},
		{"OTEL_TRACES_EXPORTER": "zipkin"},
		{"OTEL_BSP_MAX_QUEUE_SIZE": "0"},
		{"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "grpc", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4317"},
		{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "collector:4318"},
		{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318", "OTEL_EXPORTER_OTLP_TRACES_HEADERS": "nonsense"},
		{"OTEL_LOGS_EXPORTER": "console"},
		{"OTEL_LOGS_EXPORTER": "otlp"},
		{"OTEL_LOGS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318", "OTEL_BLRP_MAX_QUEUE_SIZE": "0"},
		{"OTEL_LOGS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL": "grpc", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4317"},
	} {
		err := runDaemon(t.Context(), set, slog.New(slog.DiscardHandler), func(k string) string { return env[k] })
		if err == nil {
			t.Errorf("runDaemon with %v started, want a refusal", env)
			continue
		}
		lines := refusalLines(err)
		if len(lines) < 2 || !strings.HasPrefix(lines[len(lines)-1], "fix: ") {
			t.Errorf("the refusal for %v is not a cause and a fix on lines of their own: %q", env, lines)
		}
		for _, l := range lines {
			if len(l) > 200 {
				t.Errorf("a line of %d bytes in the refusal for %v, which `systemctl status` would cut:\n%s", len(l), env, l)
			}
		}
	}
}
