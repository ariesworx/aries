package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/clienttest"
	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/telemetry"
)

// slogKeys are the fields slog itself gives a line.
var slogKeys = []string{"time", "level", "msg"}

// clientIDs are the fields of the journal that hold what the client
// called itself or asked for: its session, its machine, its harness and
// the op it sent. They are ids the journal records as they came, and
// are not exported (session) or exported only as a known op.
var clientIDs = []string{logs.KeySession, "machine", "harness", logs.KeyOp}

var reqShape = regexp.MustCompile(`^[0-9a-f]{16}$`)

// soakedLog runs the soak of spans_text_test.go, which puts plantedText
// wherever a person or an agent writes, against a server of many
// projects that logs in JSON at debug and exports its log records to the
// Manual, as ariesd does with OTEL_LOGS_EXPORTER=otlp. It returns the
// journal's lines and the server.
func soakedLog(t *testing.T, cfg telemetry.Config) (*multi, *telemetry.Manual, []jsonLine) {
	t.Helper()
	tel, man := spanned(t, cfg)
	m := newMultiTel(t, Limits{WriteRate: 100000, WriteBurst: 100000, ControlWriteRate: 100000, ControlWriteBurst: 100000, RefusalLogs: 100000}, nil, tel, dana.Principal)
	out := jsonLogged(t, m)
	m.cfg.Logger.Handler().(*logs.Handler).Tap(tel.LogHandler())
	m.project(t, projA, "alice=admin", "bob", "carol")
	m.project(t, projB, "erin=admin", "carol")
	if _, err := m.ctl.AddKey(t.Context(), dana, "carol", keyLine(t, "carol")); err != nil {
		t.Fatal(err)
	}
	m.refresh(t.Context())
	soakServer(t, m)
	// An access change by an admin, a claim that lapses, and a tick.
	d := mustDialTo(t, m.Server, dana, projA)
	mustAsk[proto.PrincipalAddResult](d, proto.OpPrincipalAdd, proto.PrincipalAddArgs{Name: "hal", Project: projA, Keys: []string{keyLine(t, "hal")}})
	d.hangUp()
	m.tick(t.Context(), true)
	return m, man, jsonLines(t, out.String())
}

// Every line ariesd writes follows the schema (design §16.4): it
// names an event of the list, every field is one of the schema's, and a
// line about a request carries the request's id and its trace's ids.
func TestLogLinesFollowTheSchema(t *testing.T) {
	t.Parallel()
	_, _, lines := soakedLog(t, telemetry.Config{})
	if len(lines) < 100 {
		t.Fatalf("%d log lines: the run logged too little to test", len(lines))
	}
	seen := map[string]int{}
	for _, l := range lines {
		ev := l.str(logs.KeyEvent)
		seen[ev]++
		if !logs.IsEvent(ev) {
			t.Errorf("a line names the event %q, which is not in internal/logs: %v", ev, l)
		}
		for k := range l {
			if !logs.IsKey(k) && !slices.Contains(slogKeys, k) {
				t.Errorf("a %s line has the field %q, which is not in internal/logs: %v", ev, k, l)
			}
		}
		switch logs.Event(ev) {
		case logs.RequestDone, logs.RequestRefused, logs.RequestFailed, logs.RequestPart:
			if !reqShape.MatchString(l.str(logs.KeyReq)) || len(l.str(logs.KeyTraceID)) != 32 || len(l.str(logs.KeySpanID)) != 16 {
				t.Errorf("a %s line lacks the request's id or its trace's ids: %v", ev, l)
			}
			if l.str(logs.KeyPrincipal) == "" || l.str(logs.KeyProject) == "" {
				t.Errorf("a %s line does not say whose request it was, or in which project: %v", ev, l)
			}
		}
		switch logs.Event(ev) {
		case logs.RequestDone, logs.RequestRefused:
			if _, ok := l[logs.KeyDurMS].(float64); !ok || l.str(logs.KeyOp) == "" {
				t.Errorf("a %s line lacks op or dur_ms: %v", ev, l)
			}
			if (l.str(logs.KeyCode) != "") != (logs.Event(ev) == logs.RequestRefused) {
				t.Errorf("a %s line has the code %q: a refusal has its code, and nothing else has one", ev, l.str(logs.KeyCode))
			}
		case logs.ReaperPruned, logs.ClaimExpired:
			// The reaper's work: its span's ids, and no request's.
			if l.str(logs.KeyReq) != "" {
				t.Errorf("a %s line carries a request's id: %v", ev, l)
			}
		}
	}
	for _, ev := range []logs.Event{logs.RequestDone, logs.RequestRefused, logs.ConnOpened, logs.ProjectOpened, logs.KeysFileWritten,
		logs.AuditPrincipalAdd, logs.AuditKeyAdd, logs.AuditMemberSet, logs.AuditProjectAdd} {
		if seen[string(ev)] == 0 {
			t.Errorf("no %s line was logged: the run did not cover it", ev)
		}
	}
}

// No log line holds what a person or an agent wrote, or a secret (rule
// 2): not an issue's title or body, a memory's text, a comment, a
// handoff, a key's text or a DSN. Requests with a recognizable text in
// every field that takes text, served and refused, leave it in no field
// of the journal but the ids the client gave itself.
func TestLogLinesCarryNoText(t *testing.T) {
	t.Parallel()
	m, _, lines := soakedLog(t, telemetry.Config{})
	dsnSecrets := secretsOfDSN(t, m.dsn)
	for _, l := range lines {
		for k, v := range l {
			if slices.Contains(clientIDs, k) {
				continue
			}
			s := fmt.Sprint(v)
			if holds(k) || holds(s) {
				t.Errorf("the log holds the client's text in %s: %v", k, l)
			}
			for _, bad := range append([]string{"ssh-ed25519 AAAA", "PRIVATE KEY", "postgres://"}, dsnSecrets...) {
				if strings.Contains(s, bad) {
					t.Errorf("the log holds %q, of a key or a DSN, in %s: %v", bad, k, l)
				}
			}
		}
	}
}

// secretsOfDSN is what of dsn no log line may repeat: the DSN itself,
// and its password when it has one.
func secretsOfDSN(t *testing.T, dsn string) []string {
	t.Helper()
	out := []string{dsn}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if pass, ok := u.User.Password(); ok && pass != "" {
		out = append(out, pass)
	}
	return out
}

// The same, read where it leaves the machine (design §16.4): a record
// exported over OTLP holds none of the text, in no field at all, and
// with telemetry: principals: at its default none of the names either;
// with the stand-in key unavailable it says nothing about who.
func TestExportedLogsCarryNoText(t *testing.T) {
	t.Parallel()
	names := []string{"alice", "dana", "bob", "carol", "erin", "hal", "laptop", "desk", "s-a", "s-d"}
	for _, tc := range []struct {
		name string
		cfg  telemetry.Config
		// who is what a record carries of a principal: a stand-in, the
		// name, or nothing.
		who string
	}{
		{"pseudonym, the default", telemetry.Config{}, "p_"},
		{"omit", telemetry.Config{Principals: telemetry.PrincipalsOmit}, ""},
		{"pseudonym with the key unavailable", telemetry.Config{KeyFile: filepath.Join(t.TempDir(), "no such directory", "principals.key")}, ""},
		{"name", telemetry.Config{Principals: telemetry.PrincipalsName}, "name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, man, lines := soakedLog(t, tc.cfg)
			recs := man.Logs()
			// The same records: one for each line of the journal.
			if len(recs) != len(lines) || len(recs) < 100 {
				t.Fatalf("%d records exported for %d lines of the journal, want one each", len(recs), len(lines))
			}
			events, withWho := map[string]int{}, 0
			for _, r := range recs {
				events[r.Event]++
				attrs := maps.Clone(r.Attrs)
				if tc.who == "name" {
					// With name the session's id is exported as the
					// client gave it, as on a span: it is an id.
					delete(attrs, logs.KeySession)
				}
				all := r.Body + " " + r.Event + " " + fmt.Sprint(attrs)
				if holds(all) {
					t.Errorf("an exported record holds the client's text: %+v", r)
				}
				for _, bad := range append([]string{"ssh-ed25519", "PRIVATE KEY", "postgres://"}, secretsOfDSN(t, m.dsn)...) {
					if strings.Contains(all, bad) {
						t.Errorf("an exported record holds %q, of a key or a DSN: %+v", bad, r)
					}
				}
				for k := range r.Attrs {
					if kind := logs.KindOf(k); kind == logs.KindLocal || kind == logs.KindTrace {
						t.Errorf("an exported record has the field %s, which stays in the journal: %+v", k, r)
					}
				}
				if tc.who != "name" {
					for _, name := range names {
						if regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`).MatchString(all) {
							t.Errorf("an exported record holds %q with principals %q: %+v", name, tc.cfg.Principals, r)
						}
					}
				}
				p := r.Attrs[logs.KeyPrincipal]
				switch {
				case p == "":
				case tc.who == "p_" && telemetry.ValidStandIn(p), tc.who == "name" && slices.Contains(names, p):
					withWho++
				default:
					t.Errorf("an exported record says principal = %q, want %q: %+v", p, tc.who, r)
				}
			}
			if (withWho > 0) != (tc.who != "") {
				t.Errorf("%d records say who, with principals %q", withWho, tc.cfg.Principals)
			}
			for _, ev := range []logs.Event{logs.RequestDone, logs.RequestRefused, logs.AuditPrincipalAdd, logs.AuditKeyAdd, logs.ProjectOpened} {
				if events[string(ev)] == 0 {
					t.Errorf("no %s record was exported: the run did not cover it", ev)
				}
			}
		})
	}
}

// And the bytes posted to a collector, as TestExportedSpansCarryNoText
// reads the spans': none of the text and none of the names.
func TestPostedLogsCarryNoText(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var posted [][]byte
	col := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		posted = append(posted, b)
		mu.Unlock()
	}))
	t.Cleanup(col.Close)
	env := map[string]string{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": col.URL + "/v1/logs", "OTEL_LOGS_EXPORTER": "otlp", "OTEL_BLRP_SCHEDULE_DELAY": "50"}
	tel, err := telemetry.New(t.Context(), telemetry.Config{Version: "v0.2.0", PerProject: true, Ops: Ops(), Getenv: func(k string) string { return env[k] },
		KeyFile: filepath.Join(t.TempDir(), "principals.key")})
	if err != nil {
		t.Fatal(err)
	}
	world := adminWorldTel(t, Limits{WriteRate: 100000, WriteBurst: 100000, ControlWriteRate: 100000, ControlWriteBurst: 100000}, tel)
	jsonLogged(t, world)
	world.cfg.Logger.Handler().(*logs.Handler).Tap(tel.LogHandler())
	soakServer(t, world)
	world.tick(t.Context(), true)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := tel.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	mu.Lock()
	all := bytes.Join(posted, nil)
	mu.Unlock()
	for _, want := range []string{"request.refused", "request.done", "conn.opened", "ariesd", projA, "p_", "s_"} {
		if !bytes.Contains(all, []byte(want)) {
			t.Fatalf("the %d bytes exported do not hold %q: the test reads nothing", len(all), want)
		}
	}
	if holds(string(all)) {
		i := strings.Index(strings.ToLower(string(all)), "planted-zq9x")
		t.Errorf("the exported log records hold the client's text: …%q…", all[max(0, i-80):min(len(all), i+80)])
	}
	for _, name := range []string{"alice", "dana", "laptop", "desk"} {
		if bytes.Contains(all, []byte(name)) {
			t.Errorf("the exported log records hold %q with telemetry: principals: at its default", name)
		}
	}
}

// A request's id is in the refusal the client is sent and on every line
// the server logged for the request, so that a person's report finds
// the server's lines (design §16.4).
func TestRequestID(t *testing.T) {
	t.Parallel()
	m := newMulti(t, Limits{}, nil)
	out := jsonLogged(t, m)
	m.project(t, projA, "alice=admin")
	m.refresh(t.Context())
	c := mustDialTo(t, m.Server, alice, projA)

	_, first := ask[proto.ShowResult](c, proto.OpShow, proto.ShowArgs{ID: "pa-none"})
	_, second := ask[proto.ShowResult](c, proto.OpShow, proto.ShowArgs{ID: "pa-none"})
	if first == nil || second == nil {
		t.Fatal("show of an unknown issue succeeded")
	}
	if !reqShape.MatchString(first.Req) || !reqShape.MatchString(second.Req) || first.Req == second.Req {
		t.Fatalf("two refusals carry the request ids %q and %q, want an id each of 16 hex digits", first.Req, second.Req)
	}
	mustAsk[proto.ListResult](c, proto.OpList, proto.ListArgs{})
	c.hangUp()

	byReq := map[string][]jsonLine{}
	for _, l := range jsonLines(t, out.String()) {
		if r := l.str(logs.KeyReq); r != "" {
			byReq[r] = append(byReq[r], l)
		}
	}
	for _, perr := range []*proto.Error{first, second} {
		ls := byReq[perr.Req]
		if len(ls) != 1 || ls[0].str(logs.KeyEvent) != string(logs.RequestRefused) || ls[0].str(logs.KeyOp) != proto.OpShow ||
			ls[0].str(logs.KeyCode) != string(perr.Code) || ls[0].str(logs.KeyPrincipal) != "alice" {
			t.Errorf("the lines with req %s = %v, want the one refusal of alice's show, with its code %s", perr.Req, ls, perr.Code)
		}
	}
	// A request that succeeded has an id too, on its line at debug.
	if len(byReq) != 3 {
		t.Errorf("%d request ids in the log, want one for each of the 3 requests: %v", len(byReq), byReq)
	}
}

// Every line a request causes carries its id, not only the request's
// own: a part of the answer that could not be read is logged with it.
func TestRequestIDOnEveryLineOfARequest(t *testing.T) {
	t.Parallel()
	out := &syncBuffer{}
	log, err := NewLogger(out, "debug", "json")
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(t)
	s.cfg.Logger = log
	s.single.log = log
	c := dialPipe(t, s, alice)
	is := mustAsk[proto.CreateResult](c, proto.OpCreate, proto.CreateArgs{Title: "an issue"})
	// The store is closed under the server: the next request fails in
	// the store, and the server logs why.
	if err := s.cfg.Store.Close(); err != nil {
		t.Fatal(err)
	}
	_, perr := ask[proto.ShowResult](c, proto.OpShow, proto.ShowArgs{ID: is.ID})
	if perr == nil || perr.Code != proto.CodeUnavailable || !reqShape.MatchString(perr.Req) {
		t.Fatalf("show with the store closed = %+v, want unavailable with the request's id", perr)
	}
	var events []string
	for _, l := range jsonLines(t, out.String()) {
		if l.str(logs.KeyReq) == perr.Req {
			events = append(events, l.str(logs.KeyEvent))
		}
	}
	slices.Sort(events)
	if want := []string{string(logs.RequestFailed), string(logs.RequestRefused)}; !slices.Equal(events, want) {
		t.Errorf("the events logged with req %s = %v, want %v: the failure's own line, and the request's", perr.Req, events, want)
	}
}

// OTLP logs to a dead collector change nothing a client can see, as
// TestDeadCollector shows of metrics and traces (design §16.6): the
// requests are no slower, the failure is logged once, and what could not
// be exported is counted, which the telemetry package's own test of a
// dead collector reads to the record.
//
// As there, the server with the dead collector takes turns, round by
// round, with one whose logs are not exported, so that a machine busy
// for part of the test slows both alike.
func TestDeadCollectorLogs(t *testing.T) {
	// Serial: counts the process's goroutines (noLeak).
	const rounds = 40
	// side is one server and a connection to it, with its log.
	type side struct {
		c   *conn
		tel *telemetry.Telemetry
		out *syncBuffer
	}
	start := func(t *testing.T, endpoint string) side {
		t.Helper()
		out := &syncBuffer{}
		log, err := NewLogger(out, "debug", "text")
		if err != nil {
			t.Fatal(err)
		}
		env := map[string]string{}
		if endpoint != "" {
			env = map[string]string{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "http://" + endpoint + "/v1/logs", "OTEL_LOGS_EXPORTER": "otlp",
				"OTEL_EXPORTER_OTLP_TIMEOUT": "150", "OTEL_BLRP_SCHEDULE_DELAY": "20", "OTEL_BLRP_EXPORT_TIMEOUT": "150",
				"OTEL_BLRP_MAX_QUEUE_SIZE": "64", "OTEL_BLRP_MAX_EXPORT_BATCH_SIZE": "16"}
		}
		tel, err := telemetry.New(t.Context(), telemetry.Config{Version: "v0.2.0", PerProject: true, Ops: Ops(), Logger: log,
			KeyFile: filepath.Join(t.TempDir(), "principals.key"), Getenv: func(k string) string { return env[k] }})
		if err != nil {
			t.Fatal(err)
		}
		if on := endpoint != ""; tel.Logging() != on {
			t.Fatalf("logs exported = %t with endpoint %q", tel.Logging(), endpoint)
		}
		log.Handler().(*logs.Handler).Tap(tel.LogHandler())
		s := newServerTel(t, Limits{WriteBurst: 100000, WriteRate: 100000}, tel)
		s.cfg.Logger, s.single.log = log, log
		c := dialPipe(t, s, alice)
		mix(t, c, 3) // the connections and caches, before any timing
		return side{c: c, tel: tel, out: out}
	}
	stop := func(t *testing.T, s side) {
		t.Helper()
		s.c.hangUp()
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		at := time.Now()
		_ = s.tel.Shutdown(ctx)
		if d := time.Since(at); d > 5*time.Second {
			t.Errorf("stopping telemetry took %s with a 1s deadline", d)
		}
	}
	closed, silent := deadCollectors(t)
	for _, tc := range []struct{ name, endpoint string }{{"a closed port", closed}, {"a listener that never answers", silent}} {
		t.Run(tc.name, func(t *testing.T) {
			noLeak(t)
			addr := tc.endpoint
			if addr == "" {
				addr = clienttest.SilentPort(t)
			}
			plain, dead := start(t, ""), start(t, addr)
			var off, took time.Duration
			for i := range rounds {
				at := time.Now()
				round(t, plain.c, i)
				mid := time.Now()
				round(t, dead.c, i)
				off, took = off+mid.Sub(at), took+time.Since(mid)
			}
			stop(t, plain)
			stop(t, dead)
			out := dead.out.String()
			if limit := 4*off + 5*time.Second; took > limit {
				t.Errorf("%d rounds of requests took %s, and %s with telemetry off: want no more than %s", rounds, took, off, limit)
			}
			if n := strings.Count(out, "a log export failed"); n != 1 {
				t.Errorf("%d log export failures logged, want exactly 1 for a collector that stays dead:\n%s", n, out)
			}
			if strings.Contains(out, "level=ERROR") {
				t.Errorf("an error was logged:\n%s", out)
			}
		})
	}
}

// deadCollectors returns the address of a port nothing listens on, and
// "" for a listener that accepts connections and never answers, which a
// subtest makes with clienttest.SilentPort.
func deadCollectors(t *testing.T) (closed, silent string) {
	t.Helper()
	c, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed = c.Addr().String()
	_ = c.Close()
	// The silent listener is made by the subtest that uses it, empty here,
	// so that it is closed before the leak check.
	return closed, ""
}
