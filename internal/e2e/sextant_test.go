package e2e

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/sextant"
	"github.com/ariesworx/aries/internal/telemetry"
)

// exporter is Sextant against the world's daemon: its telemetry, served
// on a loopback port, and its poller, which the test drives a round at
// a time.
type exporter struct {
	t      *testing.T
	poller *sextant.Poller
	url    string
}

// newExporter starts Sextant from the config text yaml, written beside
// u's .aries.yaml, whose server: stanza it can copy.
func newExporter(t *testing.T, u *user, yaml string) *exporter {
	t.Helper()
	path := filepath.Join(u.repo, "sextant.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := sextant.Load(path)
	if err != nil {
		t.Fatalf("sextant.Load: %v", err)
	}
	tel, err := telemetry.NewSextant(t.Context(), telemetry.SextantConfig{Version: "v0.2.0", Pull: true,
		Projects: cfg.Limits.Projects, Models: cfg.Limits.Models})
	if err != nil {
		t.Fatalf("telemetry.NewSextant: %v", err)
	}
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- tel.Serve(ctx, l, telemetry.ServeOptions{}) }()
	p := sextant.NewPoller(cfg, tel, sextant.PollerEnv{Version: "v0.2.0", Getenv: u.getenv, Machine: "sextant-test"})
	t.Cleanup(func() {
		p.Close()
		cancel()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
		_ = tel.Shutdown(context.Background())
	})
	return &exporter{t: t, poller: p, url: "http://" + l.Addr().String() + "/metrics"}
}

// metrics scrapes the exporter, as Prometheus would: each series's line,
// by its name and labels, to its value.
func (e *exporter) metrics() map[string]float64 {
	e.t.Helper()
	req, err := http.NewRequestWithContext(e.t.Context(), http.MethodGet, e.url, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("scrape: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		e.t.Fatalf("scrape: %s\n%s", resp.Status, b)
	}
	out := map[string]float64{}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err != nil {
			e.t.Fatalf("scrape: %q: %v", line, err)
		}
		out[line[:i]] = v
	}
	return out
}

// errors sums sextant_scrape_errors_total over the series whose labels
// hold every one of labels.
func errorsWith(m map[string]float64, labels ...string) float64 {
	var sum float64
next:
	for k, v := range m {
		if !strings.HasPrefix(k, "sextant_scrape_errors_total{") {
			continue
		}
		for _, l := range labels {
			if !strings.Contains(k, l) {
				continue next
			}
		}
		sum += v
	}
	return sum
}

// getenv is the user's environment, as the CLI reads it.
func (u *user) getenv(k string) string { return u.env[k] }

// Sextant X0 against a real daemon (sextant.md §5): signed in by key as
// a member, it reads the project's cost by model over its window and
// exports it; a model the server has no price for is (other), never a
// label of its own. A poll that cannot reach the server counts
// sextant.scrape.errors and clears the project's figures, rather than
// leaving an old one; and the next poll after the daemon is back reads
// them again.
func TestSextantExportsCost(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{admins: []string{"dana"}})
	alice, dana, sx := w.newUser("alice", ""), w.newUser("dana", ""), w.newUser("sextant", "")
	alice.env["ARIES_SESSION"] = "s-agent"
	task := strings.TrimSpace(alice.ok("create", "Do the work"))
	alice.ok("start", task)
	dana.ok("admin", "price", "set", "example-large", "--from", "2026-01-01",
		"--input", "4", "--output", "20", "--cache-write", "5", "--cache-write-1h", "8", "--cache-read", "0.4")
	in, out, cw, cw1h, cr, unknown := int64(1_000_000), int64(100_000), int64(200_000), int64(50_000), int64(3_000_000), int64(42)
	at := time.Now().UTC()
	alice.rawDial("s-agent").usageCall(1, []proto.UsageRecord{
		// 4 + 2 + 0.15×5 + 0.05×8 + 3×0.4 = 8.35 USD
		{Harness: "claude-code", RequestID: "msg_01", Model: "example-large", At: at, Granularity: "request",
			Tokens: proto.Tokens{Input: &in, Output: &out, CacheWrite: &cw, CacheWrite1h: &cw1h, CacheRead: &cr}},
		{Harness: "claude-code", RequestID: "msg_02", Model: "example-unpriced", At: at, Granularity: "request",
			Tokens: proto.Tokens{Input: &unknown}},
	})

	e := newExporter(t, sx, sx.sextantServer()+"key: id_ed25519\nprojects: ["+project+"]\nwindow: 1h\n")
	if err := e.poller.Poll(t.Context()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	m := e.metrics()
	large := `sextant_cost_usd{model="example-large",project="` + project + `"}`
	if got, ok := m[large]; !ok || got != 8.35 {
		t.Errorf("%s = %v (present %t), want 8.35; scraped:\n%v", large, got, ok, m)
	}
	other := `sextant_cost_usd{model="(other)",project="` + project + `"}`
	if got, ok := m[other]; !ok || got != 0 {
		t.Errorf("%s = %v (present %t), want 0: the unpriced model's tokens cost nothing at list price", other, got, ok)
	}
	for k := range m {
		if strings.Contains(k, "example-unpriced") {
			t.Errorf("an unpriced model is a label: %s", k)
		}
	}
	if n := errorsWith(m); n != 0 {
		t.Errorf("sextant.scrape.errors = %v after a good poll", n)
	}

	// The daemon goes away: the open connection fails at its first
	// request, and the next poll cannot connect.
	w.stop()
	if err := e.poller.Poll(t.Context()); err == nil {
		t.Fatalf("Poll with the daemon stopped = nil, want its failure")
	}
	if err := e.poller.Poll(t.Context()); err == nil {
		t.Fatalf("second Poll with the daemon stopped = nil, want its failure")
	}
	m = e.metrics()
	if n := errorsWith(m, `project="`+project+`"`); n != 2 {
		t.Errorf("sextant.scrape.errors for the project = %v, want 2; scraped:\n%v", n, m)
	}
	if n := errorsWith(m, `op="connect"`); n < 1 {
		t.Errorf("no scrape error counted at connect; scraped:\n%v", m)
	}
	if _, ok := m[large]; ok {
		t.Errorf("%s is still exported while the server cannot be read", large)
	}

	w.start()
	if err := e.poller.Poll(t.Context()); err != nil {
		t.Fatalf("Poll after the daemon is back: %v", err)
	}
	if got := e.metrics()[large]; got != 8.35 {
		t.Errorf("%s = %v after the daemon is back, want 8.35", large, got)
	}
}

// A principal that is no member is refused, and the refusal is counted
// by its code.
func TestSextantNotAMember(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{})
	nobody := w.newUser("", "")
	e := newExporter(t, nobody, nobody.sextantServer()+"key: id_ed25519\nprojects: ["+project+"]\n")
	if err := e.poller.Poll(t.Context()); err == nil {
		t.Fatalf("Poll with a key the server does not know = nil")
	}
	if n := errorsWith(e.metrics(), `op="connect"`, `code="auth"`); n != 1 {
		t.Errorf("sextant.scrape.errors{op=connect,code=auth} = %v, want 1; scraped:\n%v", n, e.metrics())
	}
}

// sextantServer is the server: stanza of u's .aries.yaml, which
// sextant.yaml takes as it is.
func (u *user) sextantServer() string {
	b, err := os.ReadFile(filepath.Join(u.repo, ".aries.yaml"))
	if err != nil {
		u.w.t.Fatal(err)
	}
	_, rest, _ := strings.Cut(string(b), "\nserver:\n")
	server, _, _ := strings.Cut(rest, "\nkey:")
	return "server:\n" + server + "\n"
}

// Sextant signs in as a service principal with its platform's token, no
// key of its own (transport.md §7): the sign-in is kept in its data
// directory and presented on each connection, and read access is the
// principal's, as a member.
func TestSextantSignsInAsAService(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{many: true, keys: true, https: true, workload: true, admins: []string{"dana"}})
	dana := w.newUser("dana", "")
	dana.ok("admin", "principal", "add", "alice")
	dana.ok("admin", "principal", "add", "sextant-example", "--kind", "service", "--owner", "alice")
	dana.ok("admin", "member", "add", "sextant-example")
	dana.ok("admin", "trust", "add", "sextant-example-main", "--principal", "sextant-example", "--provider", "github-actions",
		"--claim", "repository_owner_id=111111111", "--claim", "repository_id=123456789", "--claim", "ref=refs/heads/main")
	dana.ok("admin", "price", "set", "example-large", "--from", "2026-01-01", "--input", "4", "--output", "20",
		"--cache-write", "0", "--cache-write-1h", "0", "--cache-read", "0")
	in := int64(1_000_000)
	dana.rawDial("s-dana").usageCall(1, []proto.UsageRecord{{Harness: "claude-code", RequestID: "msg_01", Model: "example-large",
		At: time.Now().UTC(), Granularity: "request", Tokens: proto.Tokens{Input: &in}}})

	gh := w.newActions(t, map[string]any{"repository_owner_id": "111111111", "repository_id": "123456789", "ref": "refs/heads/main",
		"repository": "example/ops", "run_id": "1001", "run_attempt": "1", "event_name": "push", "workflow_ref": "example/ops/.github/workflows/sextant.yml@refs/heads/main"})
	sx := w.newUser("", "")
	gh.job(sx, "main")
	data := sx.env["ARIES_DATA_DIR"]
	e := newExporter(t, sx, sx.sextantServer()+"workload:\n  kind: github-actions\n  provider: github-actions\ndata_dir: "+data+"\nprojects: ["+project+"]\nwindow: 1h\n")
	if err := e.poller.Poll(t.Context()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	large := `sextant_cost_usd{model="example-large",project="` + project + `"}`
	if got := e.metrics()[large]; got != 4 {
		t.Errorf("%s = %v, want 4; scraped:\n%v", large, got, e.metrics())
	}
	if out := dana.ok("admin", "signin", "list", "sextant-example"); !strings.Contains(out, "github-actions (rule sextant-example-main)") {
		t.Errorf("aries admin signin list sextant-example =\n%s", out)
	}
	// The next poll presents the same sign-in; it signs in again only
	// near its end.
	if err := e.poller.Poll(t.Context()); err != nil {
		t.Fatalf("second Poll: %v", err)
	}
	if out := dana.ok("admin", "signin", "list", "sextant-example"); strings.Count(out, "live") != 1 {
		t.Errorf("after two polls, aries admin signin list sextant-example =\n%s\nwant one live sign-in", out)
	}
}
