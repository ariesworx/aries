package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
	"github.com/ariesworx/aries/internal/telemetry"
)

// BenchmarkRetained says what the daemon holds for each thing a client
// can make it hold (docs/design/profile-ariesd.md): the live heap,
// the goroutine stacks and the goroutines that one more connection,
// watcher, open store or map entry adds, read after a collection. The
// figures are the daemon's side alone: what the benchmark's own end of
// a socket holds is measured first and taken off.
//
//	go test -run '^$' -bench 'Retained' -benchtime 1x ./internal/server/
//
// One iteration is one reading, so -benchtime 1x is enough; more
// iterations average.

// benchHeld is a reading of what the process holds.
type benchHeld struct {
	heap, stacks float64
	goroutines   float64
}

// holding reads what the process holds now, after two collections: the
// second frees what the first one's finalizers released.
func holding() benchHeld {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return benchHeld{heap: float64(m.HeapAlloc), stacks: float64(m.StackInuse), goroutines: float64(runtime.NumGoroutine())}
}

func (a benchHeld) sub(b benchHeld) benchHeld {
	return benchHeld{a.heap - b.heap, a.stacks - b.stacks, a.goroutines - b.goroutines}
}

func (a benchHeld) add(b benchHeld) benchHeld {
	return benchHeld{a.heap + b.heap, a.stacks + b.stacks, a.goroutines + b.goroutines}
}

// report gives sum, the total of several readings of units things, as
// what one thing holds.
func (a benchHeld) report(b *testing.B, units int, unit string) {
	n := float64(b.N) * float64(units)
	b.ReportMetric(a.heap/n, "heap-B/"+unit)
	b.ReportMetric(a.stacks/n, "stack-B/"+unit)
	b.ReportMetric(a.goroutines/n, "goroutines/"+unit)
	b.ReportMetric(0, "ns/op") // the time is the setup's, and says nothing
}

// quiet lets go of the benchmark's own read buffers, so that a reading
// holds the daemon's memory and not the client's.
func quiet(conns []*benchConn) {
	for _, c := range conns {
		c.in = nil
	}
	settle()
}

// settle waits for the goroutines a step started and has no more use
// for, such as each connection's note of its session, to end.
func settle() {
	for range 50 {
		n := runtime.NumGoroutine()
		time.Sleep(10 * time.Millisecond)
		if runtime.NumGoroutine() == n {
			return
		}
	}
}

// actorN is the nth session of principal.
func actorN(principal string, n int) store.Actor {
	return store.Actor{Principal: principal, Session: "s-" + strconv.Itoa(n), Machine: "laptop"}
}

func BenchmarkRetained(b *testing.B) {
	sd := seedProject(b)
	w := newBenchWorld(b, sd, "off", false)

	// The benchmark's own end of a connection: the two sockets, and no
	// handler. Every connection figure below is what it holds beyond
	// this.
	var pair benchHeld
	{
		const k = 400
		before := holding()
		var conns []net.Conn
		for range k {
			c, s := benchPair(b, w.l)
			conns = append(conns, c, s)
		}
		got := holding().sub(before)
		for _, c := range conns {
			_ = c.Close()
		}
		pair = benchHeld{got.heap / k, got.stacks / k, got.goroutines / k}
	}
	less := func(got benchHeld, k int) benchHeld {
		return got.sub(benchHeld{pair.heap * float64(k), pair.stacks * float64(k), pair.goroutines * float64(k)})
	}
	closeAll := func(conns []*benchConn) {
		for _, c := range conns {
			c.close()
		}
	}

	b.Run("conn/idle", func(b *testing.B) {
		// Past its handshake, having asked once, and waiting.
		const k = 400
		var sum benchHeld
		for b.Loop() {
			before := holding()
			var conns []*benchConn
			for i := range k {
				c := w.open(b)
				c.roundTrip(benchHello(b, actorN("alice", i), project))
				c.call(proto.OpInbox, proto.InboxArgs{Limit: 1}, nil)
				conns = append(conns, c)
			}
			quiet(conns)
			sum = sum.add(less(holding().sub(before), k))
			closeAll(conns)
		}
		sum.report(b, k, "conn")
	})

	b.Run("conn/after-4MiB-frame", func(b *testing.B) {
		// The same, after one request just under the largest frame,
		// which the decoder gathers in a buffer it does not keep.
		const k = 8
		big := []byte(`{"t":"req","id":1,"op":"comment","a":{"id":"` + w.shown + `","body":"` + strings.Repeat("x", proto.MaxFrame-200) + `"}}` + "\n")
		var sum benchHeld
		for b.Loop() {
			before := holding()
			var conns []*benchConn
			for i := range k {
				c := w.open(b)
				c.roundTrip(benchHello(b, actorN("alice", i), project))
				if line := c.roundTrip(big); !bytes.Contains(line, []byte(`"err":`)) {
					b.Fatalf("a comment of 4 MiB was taken: %.200s", line)
				}
				conns = append(conns, c)
			}
			quiet(conns)
			sum = sum.add(less(holding().sub(before), k))
			closeAll(conns)
		}
		sum.report(b, k, "conn")
	})

	b.Run("conn/before-hello", func(b *testing.B) {
		// The bridge frame sent, and no hello: what a socket holds for
		// the ten seconds the hello may take.
		const k = 200
		bridge := []byte(`{"t":"bridge","pr":"alice"}` + "\n")
		var sum benchHeld
		for b.Loop() {
			before := holding()
			var conns []*benchConn
			for range k {
				c := w.open(b)
				if _, err := c.c.Write(bridge); err != nil {
					b.Fatal(err)
				}
				conns = append(conns, c)
			}
			until := time.Now().Add(5 * time.Second)
			for w.s.hellos.count("alice") < k && time.Now().Before(until) {
				time.Sleep(time.Millisecond)
			}
			quiet(conns)
			sum = sum.add(less(holding().sub(before), k))
			closeAll(conns)
		}
		sum.report(b, k, "conn")
	})

	b.Run("conn/before-hello-4MiB", func(b *testing.B) {
		// The same, with a hello that never ends: just under 4 MiB and
		// no newline, which the decoder holds while it waits.
		const k = 8
		bridge := []byte(`{"t":"bridge","pr":"alice"}` + "\n")
		endless := bytes.Repeat([]byte("x"), proto.MaxFrame-4096)
		var sum benchHeld
		for b.Loop() {
			before := holding()
			var conns []*benchConn
			for range k {
				c := w.open(b)
				if _, err := c.c.Write(bridge); err != nil {
					b.Fatal(err)
				}
				if _, err := c.c.Write(endless); err != nil {
					b.Fatal(err)
				}
				conns = append(conns, c)
			}
			time.Sleep(200 * time.Millisecond) // the daemon reads what the sockets hold
			quiet(conns)
			sum = sum.add(less(holding().sub(before), k))
			closeAll(conns)
		}
		sum.report(b, k, "conn")
	})

	b.Run("watcher", func(b *testing.B) {
		// What a watch of the project's events adds to its connection:
		// the subscription and the goroutine that pushes.
		const k = 400
		var sum benchHeld
		for b.Loop() {
			var conns []*benchConn
			for i := range k {
				c := w.open(b)
				c.roundTrip(benchHello(b, actorN("alice", i), project))
				c.call(proto.OpInbox, proto.InboxArgs{Limit: 1}, nil)
				conns = append(conns, c)
			}
			quiet(conns)
			before := holding()
			for _, c := range conns {
				c.call(proto.OpWatch, proto.WatchArgs{Events: true}, nil)
			}
			quiet(conns)
			sum = sum.add(holding().sub(before))
			closeAll(conns)
		}
		sum.report(b, k, "watcher")
	})

	b.Run("limiter-keys", func(b *testing.B) {
		// One entry of the write buckets, of the control-write buckets
		// and of the refusal sampler: a principal in a project that has
		// written, and been refused, once. Nothing removes a bucket.
		const k = 100000
		var sum benchHeld
		for b.Loop() {
			now := time.Now()
			before := holding()
			var writes buckets
			writes.rate, writes.burst = 10, 100
			for i := range k {
				writes.take(limitKey(fmt.Sprintf("00000000-0000-4000-8000-%012d", i%500), "principal-"+strconv.Itoa(i)), now)
			}
			sum = sum.add(holding().sub(before))
			runtime.KeepAlive(&writes)
		}
		sum.report(b, k, "key")
	})

	b.Run("telemetry-project", func(b *testing.B) {
		// The series one more project adds to the instruments that name
		// a project, with metrics on.
		const k = 500
		var sum benchHeld
		for b.Loop() {
			tel, err := telemetry.New(b.Context(), telemetry.Config{Pull: true, PerProject: true, Ops: Ops()})
			if err != nil {
				b.Fatal(err)
			}
			start := time.Now()
			before := holding()
			for i := range k {
				id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
				for _, class := range []string{telemetry.ClassOK, telemetry.ClassRefused, telemetry.ClassError} {
					tel.Count(telemetry.ProjectRequests, id, class)
				}
				tel.Record(telemetry.Connections, 1, id)
				tel.Since(telemetry.StoreWriteDuration, start, id)
				tel.Count(telemetry.StoreWriteRetries, id)
			}
			sum = sum.add(holding().sub(before))
			_ = tel.Shutdown(context.Background())
		}
		sum.report(b, k, "project")
	})
}

// BenchmarkRetainedTelemetry says what the daemon's telemetry holds: the
// heap a server gains from being made, and answering 6,000 requests
// that each end a span and log a line, with telemetry off, with metrics
// on, with spans exported to a collector that never answers, and with
// the log exported to it too. A collector that never answers is the
// worst a queue meets: it fills to its bound and stays full.
func BenchmarkRetainedTelemetry(b *testing.B) {
	sd := seedProject(b)
	for _, mode := range []string{"off", "metrics", "traces-stuck", "logs-stuck"} {
		b.Run(mode, func(b *testing.B) {
			var sum benchHeld
			for b.Loop() {
				before := holding()
				stop := benchStuck(b, sd, mode)
				sum = sum.add(holding().sub(before))
				stop()
			}
			sum.report(b, 1, "daemon")
		})
	}
}

// benchStuck serves sd with telemetry in mode, sends it 6,000 requests
// and returns with everything still held; the func it returns lets go.
func benchStuck(b *testing.B, sd seeded, mode string) (stop func()) {
	b.Helper()
	release := make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	collector := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	env := map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": collector.URL, "OTEL_METRICS_EXPORTER": "none"}
	cfg := telemetry.Config{Version: "v0.2.0", PerProject: true, Ops: Ops(), Pull: mode != "off",
		KeyFile: b.TempDir() + "/principals.key"}
	// At debug every request logs a line, which is what an exported log
	// carries at its busiest.
	log, err := NewLogger(io.Discard, "debug", "json")
	if err != nil {
		b.Fatal(err)
	}
	switch mode {
	case "off", "metrics":
		env = nil
	case "logs-stuck":
		env["OTEL_LOGS_EXPORTER"] = "otlp"
	}
	var tel *telemetry.Telemetry
	if mode != "off" {
		cfg.Getenv = func(k string) string { return env[k] }
		cfg.Logger = log
		if tel, err = telemetry.New(b.Context(), cfg); err != nil {
			b.Fatal(err)
		}
		if h := tel.LogHandler(); h != nil {
			log.Handler().(*logs.Handler).Tap(h)
		}
	}
	st, err := store.Open(b.Context(), sd.dsn, store.Options{Prefix: "ar", Admins: []string{dana.Principal}, Logger: log,
		Observer: StoreObserver(tel, project), Tracer: StoreTracer(tel, project)})
	if err != nil {
		b.Fatal(err)
	}
	s, err := New(Config{Store: st, Project: project, Version: "v0.2.0", ReapInterval: -1, Limits: benchLimits, Logger: log, Telemetry: tel,
		PeerCheck: func(net.Conn) error { return nil }})
	if err != nil {
		b.Fatal(err)
	}
	w := &benchWorld{s: s, st: st, l: benchListen(b), shown: sd.shown}
	c := w.open(b)
	c.roundTrip(benchHello(b, alice, project))
	req := c.frame(proto.OpInbox, proto.InboxArgs{Limit: 1})
	for range 6000 {
		c.result(c.roundTrip(req))
	}
	return func() {
		c.close()
		free()
		if h, ok := log.Handler().(*logs.Handler); ok {
			h.Tap(nil)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = tel.Shutdown(ctx)
		cancel()
		collector.Close()
		if err := st.Close(); err != nil {
			b.Errorf("close the store: %v", err)
		}
	}
}

// benchMulti is a server of many projects for a benchmark, with dana its
// server admin and alice a member of every project it is given.
type benchMulti struct {
	s   *Server
	ctl *store.Control
	l   net.Listener
	ids []string
}

// newBenchMulti starts a server of n projects, each in a schema of its
// own that holds three issues with texts of 64 KiB.
func newBenchMulti(b *testing.B, n int) *benchMulti {
	b.Helper()
	ctx := b.Context()
	db := openDB(b, newDatabase(b), benchLimits)
	ctl, err := db.OpenControl(ctx, store.DefaultSchema, store.ControlOptions{Admins: []string{dana.Principal}})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = ctl.Close() })
	m := &benchMulti{ctl: ctl, l: benchListen(b)}
	if _, err := ctl.AddPrincipal(ctx, dana, alice.Principal, ""); err != nil {
		b.Fatal(err)
	}
	long := strings.Repeat("A line of the report, as a person writes one.\n", 1400)
	for i := range n {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		if _, err := ctl.AddProject(ctx, dana, store.NewProject{ID: id, Name: "proj-" + strconv.Itoa(i), Prefix: "p" + strconv.Itoa(i),
			Account: "acct", Schema: "aries_p" + strconv.Itoa(i)}); err != nil {
			b.Fatal(err)
		}
		if err := ctl.CreateSchema(ctx, dana, id); err != nil {
			b.Fatal(err)
		}
		if _, err := ctl.ActivateProject(ctx, dana, id); err != nil {
			b.Fatal(err)
		}
		if _, err := ctl.SetMember(ctx, dana, alice.Principal, id, store.RoleMember); err != nil {
			b.Fatal(err)
		}
		m.ids = append(m.ids, id)
	}
	open := ProjectOpener(db, ctl, store.Options{}, nil)
	m.s, err = New(Config{Control: ctl, Postgres: db, Version: "v0.2.0", ReapInterval: -1, Limits: benchLimits, Open: open,
		PeerCheck: func(net.Conn) error { return nil }})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(m.s.stores.closeAll)
	// The long issues, through each project's own store, closed again
	// so that the benchmark opens them itself.
	for _, id := range m.ids {
		p, err := ctl.Project(ctx, id)
		if err != nil {
			b.Fatal(err)
		}
		st, err := open(ctx, p)
		if err != nil {
			b.Fatal(err)
		}
		for i := range 3 {
			if _, err := st.CreateIssue(ctx, alice, store.NewIssue{Title: "A long report " + strconv.Itoa(i), Body: long, Design: long, Notes: long}); err != nil {
				b.Fatal(err)
			}
		}
		if err := st.Close(); err != nil {
			b.Fatal(err)
		}
	}
	return m
}

// dial connects a to project id and completes the handshake.
func (m *benchMulti) dial(b *testing.B, a store.Actor, id string) *benchConn {
	b.Helper()
	client, server := benchPair(b, m.l)
	c := &benchConn{tb: b, c: client, in: make([]byte, 0, 64<<10), done: make(chan struct{})}
	go func() {
		defer close(c.done)
		defer func() { _ = server.Close() }()
		m.s.handle(context.Background(), server)
	}()
	if line := c.roundTrip(benchHello(b, a, id)); bytes.Contains(line, []byte(`"err":`)) {
		b.Fatalf("handshake to %s: %s", id, line)
	}
	return c
}

// BenchmarkRetainedStores says what an open project store holds in a
// server of many projects: just opened, by the handshake of its first
// connection, which is in the figure (BenchmarkRetained's conn/idle is
// what to take off); and after eight reads at once of a page of long
// issues, which leaves each of the store's eight readers with the
// buffer that page needed.
func BenchmarkRetainedStores(b *testing.B) {
	const k = 4
	m := newBenchMulti(b, k)
	ctx := b.Context()
	var opened, read benchHeld
	for b.Loop() {
		before := holding()
		var conns []*benchConn
		for _, id := range m.ids {
			c := m.dial(b, alice, id)
			c.call(proto.OpInbox, proto.InboxArgs{Limit: 1}, nil)
			conns = append(conns, c)
		}
		quiet(conns)
		opened = opened.add(holding().sub(before))
		m.s.stores.atOnce(func(pr *served) {
			var wg sync.WaitGroup
			for range 8 {
				wg.Go(func() {
					for range 4 {
						if _, err := realStore(pr.store).List(ctx, store.Filter{}); err != nil {
							b.Error(err)
						}
					}
				})
			}
			wg.Wait()
		})
		settle()
		read = read.add(holding().sub(before))
		for _, c := range conns {
			c.close()
		}
		m.s.stores.closeAll()
	}
	n := float64(b.N) * k
	b.ReportMetric(opened.heap/n, "heap-B/store-opened")
	b.ReportMetric(read.heap/n, "heap-B/store-read")
	b.ReportMetric(opened.goroutines/n, "goroutines/store-opened")
	b.ReportMetric(read.goroutines/n, "goroutines/store-read")
	b.ReportMetric(opened.stacks/n, "stack-B/store-opened")
	b.ReportMetric(read.stacks/n, "stack-B/store-read")
	b.ReportMetric(0, "ns/op")
}

// BenchmarkStatus is the status report of a server of many projects, as
// `aries admin status` asks for it, and the handshake that opens no
// store: the control index's answers, and the registry's.
func BenchmarkStatus(b *testing.B) {
	m := newBenchMulti(b, 2)
	c := m.dial(b, dana, m.ids[0])
	b.Cleanup(c.close)
	b.Run("status", func(b *testing.B) {
		c.tb = b
		req := c.frame(proto.OpStatus, proto.StatusArgs{})
		b.ReportAllocs()
		for b.Loop() {
			c.result(c.roundTrip(req))
		}
	})
	b.Run("handshake", func(b *testing.B) {
		greeting := benchHello(b, actorN("alice", 1), m.ids[0])
		b.ReportAllocs()
		for b.Loop() {
			client, server := benchPair(b, m.l)
			hc := &benchConn{tb: b, c: client, in: make([]byte, 0, 4096), done: make(chan struct{})}
			go func() {
				defer close(hc.done)
				defer func() { _ = server.Close() }()
				m.s.handle(context.Background(), server)
			}()
			hc.roundTrip(greeting)
			hc.close()
		}
	})
}
