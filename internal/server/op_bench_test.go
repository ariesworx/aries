package server

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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/logs"
	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
	"github.com/ariesworx/starfix/internal/telemetry"
)

// The benchmarks of this file measure a request end to end in the
// daemon: a frame read from a unix socket, decoded, dispatched to a
// real store on a real Dolt, and its answer encoded and written
// (docs/design/profile-starfixd.md). The client side is a socket and
// two byte slices: it builds each request before the clock starts and
// reads the answer into a buffer it keeps, so what -benchmem counts is
// the daemon's.
//
//	go test -run '^$' -bench 'Op/off' -benchmem ./internal/server/
//	go test -run '^$' -bench 'OpStatements' -benchtime 20x ./internal/server/
//
// They are measurements to rerun, not tests: nothing is asserted of a
// time or a count.

// benchTelemetry names what the daemon's telemetry does in a benchmark:
// nothing, metrics offered for pull, those and spans exported to a
// collector that answers, or those and the log exported too. debug is
// off with the log at debug, which writes a line for every request.
var benchTelemetry = []string{"off", "metrics", "traces", "logs", "debug"}

// benchWorld is a server of one project on a database the benchmark
// seeded, as a daemon would run it: its store measured and traced, and
// its log in the journal's format, written nowhere.
type benchWorld struct {
	s   *Server
	st  *store.Store
	sql *sqlCounter // set when the world counts its statements
	l   net.Listener
	// shown is the issue show, comments and history read, with a long
	// body, acceptance items, labels, dependencies and comments. held
	// are the issues alice's session holds.
	shown string
	held  []string
	n     atomic.Int64 // makes each written title and key new
}

// seeded is what seedProject left in a database.
type seeded struct {
	dsn   string
	shown string
	held  []string
}

// benchLimits lets one principal hold the connections and make the
// writes a benchmark needs.
var benchLimits = Limits{Conns: 4096, ConnsPerPrincipal: 4096, Handshakes: 4096, WriteRate: 1e6, WriteBurst: 1 << 20}

// seedProject fills a new database with a small project, through the
// store: 60 open issues, 10 of them blocked, 40 closed ones, an issue
// with a long body to show, 30 memories and two claims of alice's.
func seedProject(b *testing.B) seeded {
	b.Helper()
	dsn := newDatabase(b)
	st, err := store.Open(b.Context(), dsn, store.Options{Prefix: "sf", CommitInterval: -1, Admins: []string{dana.Principal}})
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			b.Errorf("close the seeding store: %v", err)
		}
	}()
	s, err := New(Config{Store: st, Project: project, ReapInterval: -1, Limits: benchLimits, PeerCheck: func(net.Conn) error { return nil }})
	if err != nil {
		b.Fatal(err)
	}
	do := func(a store.Actor, op string, args, into any) {
		b.Helper()
		raw, err := json.Marshal(args)
		if err != nil {
			b.Fatal(err)
		}
		res, perr := s.Dispatch(b.Context(), a, op, raw)
		if perr != nil {
			b.Fatalf("seed %s: %v", op, perr)
		}
		if into == nil {
			return
		}
		out, err := json.Marshal(res)
		if err == nil {
			err = json.Unmarshal(out, into)
		}
		if err != nil {
			b.Fatal(err)
		}
	}
	create := func(args proto.CreateArgs) string {
		var res proto.CreateResult
		do(bob, proto.OpCreate, args, &res)
		return res.ID
	}
	topics := []string{"parser", "lease", "reaper", "inbox", "digest", "socket", "bridge", "telemetry", "migration", "registry"}
	var open []string
	for i := range 60 {
		prio := i % 4
		open = append(open, create(proto.CreateArgs{
			Title: fmt.Sprintf("Handle the %s when the %s restarts, case %d", topics[i%10], topics[(i+3)%10], i),
			Body:  strings.Repeat("What happens, and what should. ", 8), Priority: &prio, Labels: []string{"area-" + topics[i%10]},
		}))
	}
	for i := range 10 {
		do(bob, proto.OpDepAdd, proto.DepArgs{From: open[i], To: open[i+10]}, nil)
	}
	for i := range 40 {
		id := create(proto.CreateArgs{Title: fmt.Sprintf("The %s drops the %s after a restart, case %d", topics[i%10], topics[(i+5)%10], i)})
		do(bob, proto.OpClose, proto.CloseArgs{ID: id, Reason: "done"}, nil)
	}
	shown := create(proto.CreateArgs{
		Title: "Parser drops the lease when the socket restarts", Body: strings.Repeat("A line of the report, as a person writes one.\n", 45),
		Acceptance: "- [ ] the lease survives\n- [ ] the parser reconnects\n- [ ] a test covers it\n- [ ] the guide says so\n- [ ] the soak passes\n",
		Labels:     []string{"area-parser", "bug", "p1"},
	})
	do(bob, proto.OpDepAdd, proto.DepArgs{From: shown, To: open[20]}, nil)
	do(bob, proto.OpDepAdd, proto.DepArgs{From: shown, To: open[21]}, nil)
	for i := range 3 {
		do(bob, proto.OpComment, proto.CommentArgs{ID: shown, Body: fmt.Sprintf("Comment %d: %s", i, strings.Repeat("seen again. ", 10))}, nil)
	}
	for i := range 30 {
		pinned := i%10 == 0
		tags := []string{topics[i%10]}
		do(bob, proto.OpRemember, proto.RememberArgs{Key: fmt.Sprintf("note-%02d", i), Tags: &tags, Pinned: &pinned,
			Body: fmt.Sprintf("The %s needs the %s restarted first. %s", topics[i%10], topics[(i+1)%10], strings.Repeat("More of why. ", 12))}, nil)
	}
	held := []string{open[30], open[31]}
	for _, id := range held {
		do(alice, proto.OpStart, proto.StartArgs{ID: id}, nil)
	}
	return seeded{dsn: dsn, shown: shown, held: held}
}

// benchCollector is a collector that takes every export and answers it,
// so that spans and log records leave the daemon as they would with one
// that works.
func benchCollector(b *testing.B) string {
	b.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	b.Cleanup(srv.Close)
	return srv.URL
}

// benchTel makes the daemon's telemetry and logger for mode, one of
// benchTelemetry.
func benchTel(b *testing.B, mode string) (*telemetry.Telemetry, *slog.Logger) {
	b.Helper()
	level := "info"
	env := map[string]string{}
	cfg := telemetry.Config{Version: "v0.2.0", PerProject: true, Ops: Ops(), KeyFile: filepath.Join(b.TempDir(), "principals.key")}
	switch mode {
	case "off":
	case "debug":
		level = "debug"
	case "metrics":
		cfg.Pull = true
	case "traces", "logs":
		cfg.Pull = true
		env["OTEL_EXPORTER_OTLP_ENDPOINT"] = benchCollector(b)
		env["OTEL_METRICS_EXPORTER"] = "none"
		if mode == "logs" {
			env["OTEL_LOGS_EXPORTER"] = "otlp"
		}
	default:
		b.Fatalf("telemetry mode %q is not one of %v", mode, benchTelemetry)
	}
	log, err := NewLogger(io.Discard, level, "json")
	if err != nil {
		b.Fatal(err)
	}
	if mode == "off" || mode == "debug" {
		return nil, log
	}
	cfg.Getenv = func(k string) string { return env[k] }
	cfg.Logger = log
	tel, err := telemetry.New(b.Context(), cfg)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tel.Shutdown(ctx)
	})
	if h := tel.LogHandler(); h != nil {
		tap := log.Handler().(*logs.Handler)
		tap.Tap(h)
		b.Cleanup(func() { tap.Tap(nil) })
	}
	return tel, log
}

// newBenchWorld serves sd's database with telemetry in mode, counting
// the store's statements when counted is set.
func newBenchWorld(b *testing.B, sd seeded, mode string, counted bool) *benchWorld {
	b.Helper()
	w := &benchWorld{shown: sd.shown, held: sd.held}
	dsn := sd.dsn
	if counted {
		dsn, w.sql = countSQL(b, dsn)
	}
	tel, log := benchTel(b, mode)
	var err error
	w.st, err = store.Open(b.Context(), dsn, store.Options{Prefix: "sf", CommitInterval: -1, Admins: []string{dana.Principal}, Logger: log,
		Observer: StoreObserver(tel, project), Tracer: StoreTracer(tel, project)})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := w.st.Close(); err != nil {
			b.Errorf("close the store: %v", err)
		}
	})
	w.s, err = New(Config{Store: w.st, Project: project, Version: "v0.2.0", ReapInterval: -1, Limits: benchLimits, Logger: log, Telemetry: tel,
		PeerCheck: func(net.Conn) error { return nil }})
	if err != nil {
		b.Fatal(err)
	}
	w.l = benchListen(b)
	return w
}

// benchListen listens on a unix socket in a directory of its own, short
// enough for a socket's path, removed when the benchmark ends.
func benchListen(b *testing.B) net.Listener {
	b.Helper()
	dir, err := os.MkdirTemp("", "sfxb")
	if err != nil {
		b.Fatal(err)
	}
	l, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		_ = os.RemoveAll(dir)
		b.Fatal(err)
	}
	b.Cleanup(func() {
		_ = l.Close()
		_ = os.RemoveAll(dir)
	})
	return l
}

// benchConn is the client's end of a connection to the daemon: a socket
// and the buffers it reuses.
type benchConn struct {
	tb   testing.TB
	c    net.Conn
	in   []byte
	id   uint64
	done chan struct{} // closed when the daemon's handler has returned
}

// benchPair returns the two ends of a new connection over l.
func benchPair(tb testing.TB, l net.Listener) (client, server net.Conn) {
	tb.Helper()
	client, err := net.Dial("unix", l.Addr().String())
	if err != nil {
		tb.Fatal(err)
	}
	server, err = l.Accept()
	if err != nil {
		_ = client.Close()
		tb.Fatal(err)
	}
	return client, server
}

// open connects to the daemon and hands the daemon's end to a handler,
// without a handshake.
func (w *benchWorld) open(tb testing.TB) *benchConn {
	tb.Helper()
	client, server := benchPair(tb, w.l)
	c := &benchConn{tb: tb, c: client, in: make([]byte, 0, 64<<10), done: make(chan struct{})}
	go func() {
		defer close(c.done)
		defer func() { _ = server.Close() }()
		w.s.handle(context.Background(), server)
	}()
	return c
}

// benchHello is the two frames that begin a's connection, as one write.
func benchHello(tb testing.TB, a store.Actor, proj string) []byte {
	tb.Helper()
	var buf bytes.Buffer
	enc := proto.NewEncoder(&buf)
	for _, f := range []*proto.Frame{
		{T: proto.FrameBridge, Principal: a.Principal},
		{T: proto.FrameHello, Proto: proto.Proto, Project: proj, Session: a.Session, Machine: a.Machine},
	} {
		if err := enc.Encode(f); err != nil {
			tb.Fatal(err)
		}
	}
	return buf.Bytes()
}

// dial connects as a and completes the handshake. The connection is
// closed, and its handler waited for, when the benchmark ends.
func (w *benchWorld) dial(tb testing.TB, a store.Actor) *benchConn {
	tb.Helper()
	c := w.open(tb)
	tb.Cleanup(c.close)
	if line := c.roundTrip(benchHello(tb, a, project)); !bytes.Contains(line, []byte(`"t":"welcome"`)) || bytes.Contains(line, []byte(`"err":`)) {
		tb.Fatalf("handshake as %s: %s", a.Principal, line)
	}
	return c
}

// close hangs up and waits for the daemon's handler.
func (c *benchConn) close() {
	_ = c.c.Close()
	<-c.done
}

// frame is the request for op with args, as the bytes of its line.
func (c *benchConn) frame(op string, args any) []byte {
	c.tb.Helper()
	c.id++
	f, err := proto.Request(c.id, op, args)
	if err != nil {
		c.tb.Fatal(err)
	}
	line, err := json.Marshal(f)
	if err != nil {
		c.tb.Fatal(err)
	}
	return append(line, '\n')
}

// roundTrip writes req and returns the line that answers it, which is
// valid until the next call. It allocates only when an answer outgrows
// the buffer.
func (c *benchConn) roundTrip(req []byte) []byte {
	c.tb.Helper()
	if _, err := c.c.Write(req); err != nil {
		c.tb.Fatalf("write: %v", err)
	}
	c.in = c.in[:0]
	for {
		if len(c.in) == cap(c.in) {
			c.in = append(c.in, 0)[:len(c.in)]
		}
		n, err := c.c.Read(c.in[len(c.in):cap(c.in)])
		c.in = c.in[:len(c.in)+n]
		if n > 0 && c.in[len(c.in)-1] == '\n' {
			return c.in
		}
		if err != nil {
			c.tb.Fatalf("read: %v (after %q)", err, c.in)
		}
	}
}

// result returns the JSON of the result in line, the answer to a
// request, and fails the benchmark if the request was refused.
func (c *benchConn) result(line []byte) []byte {
	c.tb.Helper()
	rest, ok := bytes.CutPrefix(line, []byte(`{"t":"res","id":`))
	for ok && len(rest) > 0 && rest[0] >= '0' && rest[0] <= '9' {
		rest = rest[1:]
	}
	if rest, ok = bytes.CutPrefix(rest, []byte(`,"ok":`)); !ok {
		c.tb.Fatalf("refused: %s", line)
	}
	return rest[:len(rest)-2] // less the frame's brace and the newline
}

// call sends op and decodes its result into into, if set. It is for the
// requests a benchmark makes with its clock stopped.
func (c *benchConn) call(op string, args, into any) {
	c.tb.Helper()
	res := c.result(c.roundTrip(c.frame(op, args)))
	if into == nil {
		return
	}
	if err := json.Unmarshal(res, into); err != nil {
		c.tb.Fatalf("%s: %v in %s", op, err, res)
	}
}

// benchRun is one benchmark's run of an op.
type benchRun struct {
	b   *testing.B
	w   *benchWorld
	c   *benchConn
	sql sqlCount // what the measured requests sent Dolt
}

// do sends req, measured: it is what the benchmark times and counts.
func (r *benchRun) do(req []byte) []byte {
	if r.w.sql == nil {
		return r.c.result(r.c.roundTrip(req))
	}
	before := r.w.sql.read()
	line := r.c.result(r.c.roundTrip(req))
	r.sql = r.sql.add(r.w.sql.read().sub(before))
	return line
}

// each runs fn, measured, for every iteration; fn's requests are do's.
func (r *benchRun) each(fn func()) {
	for r.b.Loop() {
		fn()
	}
}

// fixed sends the same request every iteration.
func (r *benchRun) fixed(op string, args any) {
	req := r.c.frame(op, args)
	r.each(func() { r.do(req) })
}

// prepared runs prepare with the clock stopped before each iteration,
// and sends the request it returns measured.
func (r *benchRun) prepared(prepare func() []byte, after func(res []byte)) {
	r.each(func() {
		r.b.StopTimer()
		req := prepare()
		r.b.StartTimer()
		res := r.do(req)
		if after != nil {
			r.b.StopTimer()
			after(res)
			r.b.StartTimer()
		}
	})
}

// newIssue creates an issue, unmeasured, and returns its id.
func (r *benchRun) newIssue() string {
	var res proto.CreateResult
	r.c.call(proto.OpCreate, proto.CreateArgs{Title: fmt.Sprintf("Benchmark the %d of the widget reader", r.w.n.Add(1))}, &res)
	return res.ID
}

// started creates an issue and starts it, unmeasured.
func (r *benchRun) started() (id string, epoch int64) {
	id = r.newIssue()
	var res proto.StartResult
	r.c.call(proto.OpStart, proto.StartArgs{ID: id}, &res)
	return id, res.Claim.Epoch
}

// benchOps are the requests measured, in the order reported. Each is
// one request to the daemon, but prime, which is the four an agent's
// session sends when it starts.
var benchOps = []struct {
	name string
	run  func(r *benchRun)
}{
	{"show", func(r *benchRun) { r.fixed(proto.OpShow, proto.ShowArgs{ID: r.w.shown}) }},
	{"list", func(r *benchRun) { r.fixed(proto.OpList, proto.ListArgs{}) }},
	{"ready", func(r *benchRun) { r.fixed(proto.OpReady, proto.LimitArgs{}) }},
	{"blocked", func(r *benchRun) { r.fixed(proto.OpBlocked, proto.LimitArgs{}) }},
	{"digest", func(r *benchRun) { r.fixed(proto.OpDigest, proto.DigestArgs{}) }},
	{"who", func(r *benchRun) { r.fixed(proto.OpWho, proto.WhoArgs{}) }},
	{"inbox", func(r *benchRun) { r.fixed(proto.OpInbox, proto.InboxArgs{}) }},
	{"claims", func(r *benchRun) { r.fixed(proto.OpClaims, proto.LimitArgs{}) }},
	{"comments", func(r *benchRun) { r.fixed(proto.OpComments, proto.PageArgs{ID: r.w.shown}) }},
	{"history", func(r *benchRun) { r.fixed(proto.OpHistory, proto.PageArgs{ID: r.w.shown}) }},
	{"recall", func(r *benchRun) { r.fixed(proto.OpRecall, proto.RecallArgs{Text: "parser restarted"}) }},
	{"prime", func(r *benchRun) {
		reqs := [][]byte{
			r.c.frame(proto.OpList, proto.ListArgs{Status: []string{"in_progress"}, Assignee: alice.Principal}),
			r.c.frame(proto.OpReady, proto.LimitArgs{Limit: 5}),
			r.c.frame(proto.OpInbox, proto.InboxArgs{Limit: 5}),
			r.c.frame(proto.OpRecall, proto.RecallArgs{Prime: true, Limit: 5}),
		}
		r.each(func() {
			for _, req := range reqs {
				r.do(req)
			}
		})
	}},
	{"refused", func(r *benchRun) {
		// A request the store refuses: the error's mapping and its line
		// in the log, which is sampled.
		req := r.c.frame(proto.OpShow, proto.ShowArgs{ID: "sf-aaaaaaaa"})
		r.each(func() {
			var before sqlCount
			if r.w.sql != nil {
				before = r.w.sql.read()
			}
			line := r.c.roundTrip(req)
			if !bytes.Contains(line, []byte(`"c":"not_found"`)) {
				r.b.Fatalf("show of no issue was not refused as not found: %s", line)
			}
			if r.w.sql != nil {
				r.sql = r.sql.add(r.w.sql.read().sub(before))
			}
		})
	}},
	{"create", func(r *benchRun) {
		body := strings.Repeat("What happens, and what should. ", 16)
		r.prepared(func() []byte {
			return r.c.frame(proto.OpCreate, proto.CreateArgs{Title: fmt.Sprintf("Benchmark the %d of the widget reader", r.w.n.Add(1)), Body: body})
		}, nil)
	}},
	{"update", func(r *benchRun) {
		id := r.newIssue()
		var rev int64 = 1
		r.prepared(func() []byte {
			title := fmt.Sprintf("Benchmark the %d of the widget writer", r.w.n.Add(1))
			return r.c.frame(proto.OpUpdate, proto.UpdateArgs{ID: id, Rev: rev, Title: &title})
		}, func(res []byte) {
			var out proto.WriteResult
			if err := json.Unmarshal(res, &out); err != nil {
				r.b.Fatal(err)
			}
			rev = out.Rev
		})
	}},
	{"comment", func(r *benchRun) {
		r.fixed(proto.OpComment, proto.CommentArgs{ID: r.w.shown, Body: strings.Repeat("Seen again on the build machine. ", 6)})
	}},
	{"start", func(r *benchRun) {
		var id string
		r.prepared(func() []byte {
			id = r.newIssue()
			return r.c.frame(proto.OpStart, proto.StartArgs{ID: id})
		}, func([]byte) { r.c.call(proto.OpFinish, proto.FinishArgs{ID: id}, nil) })
	}},
	{"start-next", func(r *benchRun) {
		// Start with no id: the top ready issue, a new one of priority 0,
		// is ranked and taken; it is finished again, unmeasured.
		r.prepared(func() []byte {
			var res proto.CreateResult
			p := 0
			r.c.call(proto.OpCreate, proto.CreateArgs{Title: fmt.Sprintf("Benchmark the %d of the widget reader", r.w.n.Add(1)),
				Priority: &p}, &res)
			return r.c.frame(proto.OpStart, proto.StartArgs{})
		}, func(res []byte) {
			var got proto.StartResult
			if err := json.Unmarshal(res, &got); err != nil {
				r.b.Fatal(err)
			}
			r.c.call(proto.OpFinish, proto.FinishArgs{ID: got.Issue.ID}, nil)
		})
	}},
	{"finish", func(r *benchRun) {
		r.prepared(func() []byte {
			id, epoch := r.started()
			return r.c.frame(proto.OpFinish, proto.FinishArgs{ID: id, Epoch: epoch, Reason: "done", Handoff: "Done; nothing left."})
		}, nil)
	}},
	{"handoff", func(r *benchRun) {
		r.prepared(func() []byte {
			id, epoch := r.started()
			return r.c.frame(proto.OpHandoff, proto.HandoffArgs{ID: id, Epoch: epoch, Note: "Half done; the parser is next.", Release: true})
		}, nil)
	}},
	{"renew", func(r *benchRun) { r.fixed(proto.OpRenew, proto.RenewArgs{}) }},
	{"usage", func(r *benchRun) {
		in, out := int64(1200), int64(340)
		at := time.Now().UTC().Add(-time.Minute)
		r.prepared(func() []byte {
			return r.c.frame(proto.OpUsage, proto.UsageArgs{Records: []proto.UsageRecord{{Harness: "claude-code",
				RequestID: "req-" + strconv.FormatInt(r.w.n.Add(1), 10), Model: "claude-opus-4", At: at, Granularity: "request",
				Tokens: proto.Tokens{Input: &in, Output: &out}}}})
		}, nil)
	}},
	{"remember", func(r *benchRun) {
		body := strings.Repeat("Why the reader waits for the writer. ", 8)
		var key string
		r.prepared(func() []byte {
			key = "bench-" + strconv.FormatInt(r.w.n.Add(1), 10)
			return r.c.frame(proto.OpRemember, proto.RememberArgs{Key: key, Body: body})
		}, func([]byte) { r.c.call(proto.OpForget, proto.ForgetArgs{Key: key}, nil) })
	}},
}

// report adds what a counted run sent Dolt to the benchmark's result.
func (r *benchRun) report() {
	if r.w.sql == nil {
		return
	}
	n := float64(r.b.N)
	r.b.ReportMetric(float64(r.sql.Statements)/n, "statements/op")
	r.b.ReportMetric(float64(r.sql.RoundTrips)/n, "roundtrips/op")
	r.b.ReportMetric(float64(r.sql.Prepared)/n, "prepares/op")
	r.b.ReportMetric(float64(r.sql.Commits)/n, "commits/op")
	r.b.ReportMetric(float64(r.sql.Waited.Microseconds())/n/1000, "dolt-ms/op")
	r.b.ReportMetric(float64(r.sql.CommitWaited.Microseconds())/n/1000, "commit-ms/op")
}

// BenchmarkOp is each op's cost in the daemon, by what its
// telemetry does: time, bytes and allocations for one request, from the
// frame read off the socket to the answer written to it. The time is
// Dolt's for the most part; the bytes and allocations are the daemon's.
func BenchmarkOp(b *testing.B) {
	for _, mode := range benchTelemetry {
		b.Run(mode, func(b *testing.B) {
			// A project of its own for each mode: the writes measured in
			// one would otherwise be the next one's history to read.
			w := newBenchWorld(b, seedProject(b), mode, false)
			c := w.dial(b, alice)
			for _, op := range benchOps {
				b.Run(op.name, func(b *testing.B) {
					c.tb = b
					r := &benchRun{b: b, w: w, c: c}
					b.ReportAllocs()
					op.run(r)
				})
			}
		})
	}
}

// BenchmarkOpStatements is each op's SQL: the statements it runs, the
// round trips to Dolt they cost, and how long the store waited on Dolt
// for them, of which how long for its COMMIT; all read off the wire by
// a proxy (sqlCounter). Set dolt-ms/op beside ns/op for the share of a
// request that is the wait for Dolt. Its times include the proxy's two
// extra hops, so read BenchmarkOp for an op's time alone.
func BenchmarkOpStatements(b *testing.B) {
	sd := seedProject(b)
	w := newBenchWorld(b, sd, "off", true)
	c := w.dial(b, alice)
	for _, op := range benchOps {
		b.Run(op.name, func(b *testing.B) {
			c.tb = b
			r := &benchRun{b: b, w: w, c: c}
			op.run(r)
			r.report()
		})
	}
	b.Run("handshake", func(b *testing.B) {
		greeting := benchHello(b, store.Actor{Principal: "carol", Session: "s-c", Machine: "laptop-c"}, project)
		var sent sqlCount
		for b.Loop() {
			before := w.sql.read()
			hc := w.open(b)
			hc.roundTrip(greeting)
			hc.close()
			sent = sent.add(w.sql.read().sub(before))
		}
		r := &benchRun{b: b, w: w, sql: sent}
		r.report()
	})
}

// BenchmarkHandshake is a connection from its first frame to its
// welcome, and its end: the bridge frame and the hello decoded, the
// welcome written, the session noted in the registry (a write), and the
// handler's return. The client's side of it, a dial and an accept, is
// in the counts too.
func BenchmarkHandshake(b *testing.B) {
	sd := seedProject(b)
	for _, mode := range []string{"off", "traces"} {
		b.Run(mode, func(b *testing.B) {
			w := newBenchWorld(b, sd, mode, false)
			greeting := benchHello(b, store.Actor{Principal: "carol", Session: "s-c", Machine: "laptop-c"}, project)
			b.ReportAllocs()
			for b.Loop() {
				c := w.open(b)
				c.roundTrip(greeting)
				c.close()
			}
		})
	}
}

// BenchmarkPush is a write whose event is pushed to n connections that
// watch the project's events, as `sfx tui` and `sfx watch` do: the
// comment's own cost, and each watcher's frame encoded and written.
func BenchmarkPush(b *testing.B) {
	sd := seedProject(b)
	w := newBenchWorld(b, sd, "off", false)
	writer := w.dial(b, alice)
	for _, n := range []int{0, 1, 10, 100} {
		b.Run(fmt.Sprintf("watchers=%d", n), func(b *testing.B) {
			var got atomic.Int64
			var wg sync.WaitGroup
			var watchers []*benchConn
			for i := range n {
				wc := w.dial(b, store.Actor{Principal: "bob", Session: fmt.Sprintf("s-w%d", i), Machine: "laptop-b"})
				wc.call(proto.OpWatch, proto.WatchArgs{Events: true}, nil)
				watchers = append(watchers, wc)
				wg.Go(func() {
					buf := make([]byte, 16<<10)
					for {
						k, err := wc.c.Read(buf)
						got.Add(int64(bytes.Count(buf[:k], []byte{'\n'})))
						if err != nil {
							return
						}
					}
				})
			}
			writer.tb = b
			req := writer.frame(proto.OpComment, proto.CommentArgs{ID: w.shown, Body: "Seen again on the build machine."})
			b.ReportAllocs()
			var sent int64
			for b.Loop() {
				writer.result(writer.roundTrip(req))
				sent += int64(n)
				for got.Load() < sent {
					time.Sleep(20 * time.Microsecond)
				}
			}
			b.StopTimer()
			for _, wc := range watchers {
				wc.close()
			}
			wg.Wait()
		})
	}
}

// BenchmarkOpStatementsBatch counts the SQL of requests that carry
// several things, at two sizes each, so that the difference says what
// one more label, usage record, discovered issue or held claim costs:
// which ops run a statement for each item.
func BenchmarkOpStatementsBatch(b *testing.B) {
	w := newBenchWorld(b, seedProject(b), "off", true)
	c := w.dial(b, alice)
	run := func(name string, fn func(r *benchRun)) {
		b.Run(name, func(b *testing.B) {
			c.tb = b
			r := &benchRun{b: b, w: w, c: c}
			fn(r)
			r.report()
		})
	}
	for _, n := range []int{1, 21} {
		run(fmt.Sprintf("create/labels=%d", n), func(r *benchRun) {
			labels := make([]string, n)
			for i := range labels {
				labels[i] = "label-" + strconv.Itoa(i)
			}
			r.prepared(func() []byte {
				return r.c.frame(proto.OpCreate, proto.CreateArgs{Title: fmt.Sprintf("Benchmark the %d of the widget reader", r.w.n.Add(1)), Labels: labels})
			}, nil)
		})
	}
	for _, n := range []int{1, 101} {
		run(fmt.Sprintf("usage/records=%d", n), func(r *benchRun) {
			in, out := int64(1200), int64(340)
			at := time.Now().UTC().Add(-time.Minute)
			r.prepared(func() []byte {
				recs := make([]proto.UsageRecord, n)
				for i := range recs {
					recs[i] = proto.UsageRecord{Harness: "claude-code", RequestID: "batch-" + strconv.FormatInt(r.w.n.Add(1), 10),
						Model: "claude-opus-4", At: at, Granularity: "request", Tokens: proto.Tokens{Input: &in, Output: &out}}
				}
				return r.c.frame(proto.OpUsage, proto.UsageArgs{Records: recs})
			}, nil)
		})
	}
	for _, n := range []int{0, 5} {
		run(fmt.Sprintf("finish/discovered=%d", n), func(r *benchRun) {
			found := make([]proto.Discovered, n)
			for i := range found {
				found[i].Title = "Found while at it, part " + strconv.Itoa(i)
			}
			r.prepared(func() []byte {
				id, epoch := r.started()
				return r.c.frame(proto.OpFinish, proto.FinishArgs{ID: id, Epoch: epoch, Discovered: found})
			}, nil)
		})
	}
	// Alice holds two claims from the seeding; ten more for the second
	// size, which stay held.
	for _, n := range []int{2, 12} {
		run(fmt.Sprintf("renew/claims=%d", n), func(r *benchRun) {
			var res proto.ClaimsResult
			r.c.call(proto.OpRenew, proto.RenewArgs{}, &res)
			for len(res.Claims) < n {
				r.started()
				r.c.call(proto.OpRenew, proto.RenewArgs{}, &res)
			}
			r.fixed(proto.OpRenew, proto.RenewArgs{})
		})
	}
}
