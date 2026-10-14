package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/doltmetrics"
	"github.com/ariesworx/starfix/internal/dolttest"
	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/statustext"
)

// fakeDolt stands in for Dolt's metrics endpoint: an HTTP server on
// loopback whose answer a test sets, and which counts what it is asked.
type fakeDolt struct {
	url  string
	hits atomic.Int64
	h    atomic.Pointer[http.HandlerFunc]
}

func newFakeDolt(t *testing.T) *fakeDolt {
	t.Helper()
	f := &fakeDolt{}
	f.answer(http.NotFound)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		(*f.h.Load())(w, r)
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL + "/metrics"
	return f
}

func (f *fakeDolt) answer(h http.HandlerFunc) { f.h.Store(&h) }

// serve answers 200 with body from now on.
func (f *fakeDolt) serve(body string) {
	f.answer(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
}

// doltStarted is when the Dolt of doltBody started.
const doltStarted = 1791835308

// doltBody is what a Dolt 2.4.2 that started at doltStarted, holds 4
// connections and 59670528 bytes, and runs under a limit of 1500 MiB
// serves after cycles cycles of its collector.
func doltBody(cycles int) string {
	return fmt.Sprintf("# HELP dss_dolt_version The version\ndss_dolt_version 3.3816578e+07\ndss_concurrent_connections 4\n"+
		"process_resident_memory_bytes 5.9670528e+07\nprocess_start_time_seconds %d\ngo_gc_gomemlimit_bytes 1.572864e+09\n"+
		"go_gc_duration_seconds{quantile=\"0.5\"} 3.7e-05\ngo_gc_duration_seconds_count %d\n", doltStarted, cycles)
}

// watchDolt has s read Dolt's metrics from url, when a test calls
// ReadDolt, and by a clock the test moves: it returns the function that
// moves it.
func watchDolt(t *testing.T, s *Server, url string) (advance func(time.Duration)) {
	t.Helper()
	r, err := doltmetrics.New(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	s.cfg.DoltMetrics = r
	var now atomic.Int64
	now.Store(time.Unix(doltStarted+92, 0).UnixNano())
	s.doltm.now = func() time.Time { return time.Unix(0, now.Load()).UTC() }
	return func(d time.Duration) { now.Add(int64(d)) }
}

// localReport is the status report `starfixd status` gets, with its text
// and its JSON as the two commands print them.
func localReport(t *testing.T, s *Server) (res proto.StatusResult, text, doc string) {
	t.Helper()
	out, perr := s.localStatus(t.Context(), proto.StatusArgs{})
	if perr != nil {
		t.Fatalf("status: %v", perr)
	}
	res = out.(proto.StatusResult)
	var w bytes.Buffer
	statustext.Print(&w, res)
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	return res, w.String(), string(b)
}

// writeIssues writes n issues, one write each.
func writeIssues(t *testing.T, s *Server, n int) {
	t.Helper()
	for i := range n {
		mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: fmt.Sprintf("issue %d", i)})
	}
}

// The status report carries what the daemon last read of Dolt's own
// metrics, in text and in JSON, and the collector's cycles against its
// own writes once it has read twice.
func TestDoltStatus(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	f := newFakeDolt(t)
	advance := watchDolt(t, s, f.url)
	ctx := t.Context()

	res, text, doc := localReport(t, s)
	if d := res.Server.Dolt; d == nil || !d.Unknown || d.ReadAt != nil {
		t.Fatalf("Dolt before the first read = %s, want unknown and never read", js(d))
	}
	if want := "Dolt: unknown: starfixd has not read Dolt's metrics since it started; see starfixd's log\n"; !strings.Contains(text, want) {
		t.Errorf("the status before the first read lacks %q:\n%s", want, text)
	}
	if want := `"dolt":{"unknown":true}`; !strings.Contains(doc, want) {
		t.Errorf("the JSON before the first read lacks %s:\n%s", want, doc)
	}

	f.serve(doltBody(3))
	s.ReadDolt(ctx)
	_, text, doc = localReport(t, s)
	if want := "goroutines\nDolt 2.4.2, up 1m 32s, 4 connections, 57 MiB resident, memory limit 1500 MiB\nprojects: "; !strings.Contains(text, want) {
		t.Errorf("the status after one read lacks %q:\n%s", want, text)
	}
	if want := `"dolt":{"read_at":"2026-10-12T20:03:20Z","version":"2.4.2","started_at":"2026-10-12T20:01:48Z","uptime_seconds":92,` +
		`"resident_bytes":59670528,"conns":4,"mem_limit_bytes":1572864000}`; !strings.Contains(doc, want) {
		t.Errorf("the JSON after one read lacks %s:\n%s", want, doc)
	}

	writeIssues(t, s, 3)
	advance(time.Minute)
	f.serve(doltBody(5))
	s.ReadDolt(ctx)
	_, text, doc = localReport(t, s)
	if want := "Dolt 2.4.2, up 2m 32s, 4 connections, 57 MiB resident, memory limit 1500 MiB\n" +
		"Dolt's collector: 2 cycles for 3 writes in 1m: too few writes to judge the memory limit by (it takes 500)\n"; !strings.Contains(text, want) {
		t.Errorf("the status after two reads lacks %q:\n%s", want, text)
	}
	if want := `"gc":{"window_seconds":60,"cycles":2,"writes":3,"cycles_per_100_writes":66.66666666666667}`; !strings.Contains(doc, want) {
		t.Errorf("the JSON after two reads lacks %s:\n%s", want, doc)
	}
	if n := f.hits.Load(); n != 2 {
		t.Errorf("Dolt's endpoint was asked %d times for two reads and three status reports, want 2: a report reads nothing", n)
	}
}

// The window is the latest ten minutes: older readings leave it. A Dolt
// that restarted, which its start time and its counter both say, starts
// the window again.
func TestDoltWindow(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	f := newFakeDolt(t)
	advance := watchDolt(t, s, f.url)
	ctx := t.Context()
	// gc is the collector's part of the report, as its JSON.
	gc := func() string {
		res, _, _ := localReport(t, s)
		return js(res.Server.Dolt.GC)
	}
	// A reading a minute for a quarter of an hour: two cycles and one
	// write in each.
	for i := range 16 {
		f.serve(doltBody(100 + 2*i))
		s.ReadDolt(ctx)
		if got := gc(); i == 0 && got != "null" {
			t.Fatalf("the collector's figures after one reading = %s, want none", got)
		}
		if got, want := gc(), `{"window_seconds":240,"cycles":8,"writes":4,"cycles_per_100_writes":200}`; i == 4 && got != want {
			t.Errorf("after 4 minutes: %s, want %s", got, want)
		}
		writeIssues(t, s, 1)
		advance(time.Minute)
	}
	// One more reading, and the window is the ten minutes before it.
	f.serve(doltBody(132))
	s.ReadDolt(ctx)
	if got, want := gc(), `{"window_seconds":600,"cycles":20,"writes":10,"cycles_per_100_writes":200}`; got != want {
		t.Errorf("after 16 minutes: %s, want %s", got, want)
	}

	// Dolt restarts: its counter starts again and its start time moves.
	restarted := strings.ReplaceAll(doltBody(1), fmt.Sprint(doltStarted), fmt.Sprint(doltStarted+990))
	advance(time.Minute)
	f.serve(restarted)
	s.ReadDolt(ctx)
	if got := gc(); got != "null" {
		t.Errorf("the collector's figures just after Dolt restarted = %s, want none: its counter began again", got)
	}
	writeIssues(t, s, 2)
	advance(15 * time.Second)
	f.serve(strings.ReplaceAll(restarted, "seconds_count 1", "seconds_count 4"))
	s.ReadDolt(ctx)
	if got, want := gc(), `{"window_seconds":15,"cycles":3,"writes":2,"cycles_per_100_writes":150}`; got != want {
		t.Errorf("after the restart: %s, want %s", got, want)
	}
	// A counter that went back is a restart too, though the start time
	// is the same to the second.
	advance(15 * time.Second)
	f.serve(strings.ReplaceAll(restarted, "seconds_count 1", "seconds_count 2"))
	s.ReadDolt(ctx)
	if got := gc(); got != "null" {
		t.Errorf("the collector's figures after its counter went back = %s, want none", got)
	}
	// And so is a start time Dolt no longer gives.
	advance(15 * time.Second)
	f.serve(strings.ReplaceAll(restarted, "seconds_count 1", "seconds_count 3"))
	s.ReadDolt(ctx)
	advance(15 * time.Second)
	f.serve("go_gc_duration_seconds_count 9\n")
	s.ReadDolt(ctx)
	if got := gc(); got != "null" {
		t.Errorf("the collector's figures after Dolt's start time went missing = %s, want none", got)
	}
}

// js prints a part of a status report as its JSON, so that a failure
// shows the figures and not the addresses of those that may be absent.
func js(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return err.Error()
	}
	return string(b)
}

// Whatever Dolt's endpoint does, the status says "unknown" with the last
// read that succeeded, the daemon stays ready and serves, and a read
// that works again brings the figures back.
func TestDoltUnknown(t *testing.T) {
	// Serial: bounds elapsed time, which a test running beside it would stretch.
	s := newServer(t)
	s.listening.Store(true) // as while Serve accepts
	log := &syncBuffer{}
	s.cfg.Logger = slogTo(log)
	f := newFakeDolt(t)
	advance := watchDolt(t, s, f.url)
	ctx := t.Context()
	var elsewhere atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhere.Add(1)
		_, _ = w.Write([]byte(doltBody(9)))
	}))
	t.Cleanup(target.Close)

	f.serve(doltBody(3))
	s.ReadDolt(ctx)
	// A short deadline for the reads that fail, the last of which waits it
	// out, and for those only: the reads that must succeed, before and
	// after, have the reader's own, which a slow machine does not run out.
	s.doltm.timeout = 300 * time.Millisecond
	const readAt = "2026-10-12 20:03 UTC"
	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
	}{
		{"an error", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, doltBody(9), http.StatusInternalServerError)
		}},
		{"garbage", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("\x00\xff<html>not metrics")) }},
		{"another program's metrics", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("go_goroutines 28\n")) }},
		{"a redirect", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/metrics", http.StatusFound)
		}},
		{"a body without end", func(w http.ResponseWriter, r *http.Request) {
			line := []byte(strings.Repeat("# padding\n", 1000))
			for r.Context().Err() == nil {
				if _, err := w.Write(line); err != nil {
					return
				}
			}
		}},
		{"no answer", func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.answer(tc.h)
			advance(15 * time.Second)
			start := time.Now()
			s.ReadDolt(ctx)
			if took := time.Since(start); took > 3*time.Second {
				t.Errorf("the read took %s, want it bounded by its deadline of %s", took, s.doltm.timeout)
			}
			res, text, doc := localReport(t, s)
			if got, want := js(res.Server.Dolt), `{"unknown":true,"read_at":"2026-10-12T20:03:20Z"}`; got != want {
				t.Errorf("Dolt = %s, want %s: unknown, and no figure", got, want)
			}
			if want := "Dolt: unknown: starfixd could not read Dolt's metrics, and last could at " + readAt + "; see starfixd's log\n"; !strings.Contains(text, want) {
				t.Errorf("the status lacks %q:\n%s", want, text)
			}
			if want := `"dolt":{"unknown":true,"read_at":"2026-10-12T20:03:20Z"}`; !strings.Contains(doc, want) {
				t.Errorf("the JSON lacks %s:\n%s", want, doc)
			}
			if why := s.Ready(ctx); why != "" {
				t.Errorf("Ready() = %q, want ready: Dolt's metrics are not what /readyz checks", why)
			}
			writeIssues(t, s, 1)
		})
	}
	if n := elsewhere.Load(); n != 0 {
		t.Errorf("the redirect's target was asked %d times, want 0", n)
	}
	// The failure has a name an alert can match (design §16.4).
	if n := strings.Count(log.String(), "unknown until a read succeeds\" event=dolt_metrics.read_failed url="); n != 1 {
		t.Errorf("%d dolt_metrics.read_failed lines logged for six failures in a row, want 1:\n%s", n, log)
	}
	if strings.Contains(log.String(), "level=ERROR") {
		t.Errorf("a failed read was logged as an error of starfixd's:\n%s", log)
	}

	advance(15 * time.Second)
	f.serve(doltBody(7))
	s.doltm.timeout = 0
	s.ReadDolt(ctx)
	res, text, _ := localReport(t, s)
	// The readings before the failures still stand: 105 seconds, the six
	// writes made meanwhile, and four cycles.
	if d := res.Server.Dolt; d.Unknown || d.GC == nil || d.GC.WindowSeconds != 105 || d.GC.Cycles != 4 || d.GC.Writes != 6 {
		t.Errorf("Dolt after a read that worked again = %s, want 4 cycles and 6 writes in 105s", js(d))
	}
	if !strings.Contains(text, "Dolt 2.4.2, up 3m 17s, ") {
		t.Errorf("the status after a read that worked again lacks Dolt's figures:\n%s", text)
	}
	if n := strings.Count(log.String(), `msg="reading Dolt's metrics again" event=dolt_metrics.read_recovered url=`); n != 1 {
		t.Errorf("%d lines say the reads work again, want 1:\n%s", n, log)
	}
}

// A Dolt that cannot be read is logged when it starts, every ten minutes
// while it lasts with the number of failures, and when it ends: never
// once a read.
func TestDoltUnknownLogIsSampled(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	log := &syncBuffer{}
	s.cfg.Logger = slogTo(log)
	// Nothing listens here.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + l.Addr().String() + "/metrics"
	_ = l.Close()
	advance := watchDolt(t, s, url)
	ctx := t.Context()
	count := func(msg string) int { return strings.Count(log.String(), msg) }

	for range 39 { // 15 seconds apart: 9 minutes 45 seconds
		s.ReadDolt(ctx)
		advance(15 * time.Second)
	}
	if n := count("could not read Dolt's metrics"); n != 1 {
		t.Fatalf("%d lines for 39 failures in under ten minutes, want 1:\n%s", n, log)
	}
	if !strings.Contains(log.String(), "level=WARN") || !strings.Contains(log.String(), "connection refused") || !strings.Contains(log.String(), url) {
		t.Errorf("the line is not a warning naming the URL and the cause:\n%s", log)
	}
	for range 40 { // ten minutes more
		s.ReadDolt(ctx)
		advance(15 * time.Second)
	}
	if n := count("could not read Dolt's metrics"); n != 2 {
		t.Errorf("%d lines after twenty minutes of failures, want 2:\n%s", n, log)
	}
	if !strings.Contains(log.String(), "failures=41") {
		t.Errorf("the second line does not count the failures so far:\n%s", log)
	}
	res, _, _ := localReport(t, s)
	if got, want := js(res.Server.Dolt), `{"unknown":true}`; got != want {
		t.Errorf("Dolt = %s, want %s: unknown, and never read", got, want)
	}
}

// With no URL the daemon reads nothing and its status says nothing of
// Dolt.
func TestDoltNotWatched(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	s.ReadDolt(t.Context())
	res, text, doc := localReport(t, s)
	if res.Server.Dolt != nil || strings.Contains(text, "Dolt") || strings.Contains(doc, "dolt") {
		t.Errorf("the status names Dolt with no URL to read it from:\n%s\n%s", text, doc)
	}
}

// Serve reads on a timer, at once and then every DoltMetricsEvery, and
// stops with it, even while a read hangs. Nothing is left running.
func TestDoltTimer(t *testing.T) {
	// Serial: counts the process's goroutines (noLeak).
	for _, tc := range []struct {
		name  string
		hangs bool
	}{{"an endpoint that answers", false}, {"an endpoint that never answers", true}} {
		t.Run(tc.name, func(t *testing.T) {
			noLeak(t)
			s := newServer(t)
			f := newFakeDolt(t)
			f.serve(doltBody(3))
			if tc.hangs {
				f.answer(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
			}
			watchDolt(t, s, f.url)
			s.cfg.DoltMetricsEvery = 10 * time.Millisecond
			// A read's own deadline is far off, so a Serve that returns
			// while one hangs has ended it, not waited it out.
			s.doltm.timeout = time.Hour
			ctx, stop := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- s.Serve(ctx, newFakeListener()) }()
			want := int64(4)
			if tc.hangs {
				want = 1
			}
			waitFor(t, "the timer's reads", func() bool { return f.hits.Load() >= want })
			if !tc.hangs {
				waitFor(t, "a status with Dolt's figures", func() bool {
					res, _, _ := localReport(t, s)
					return !res.Server.Dolt.Unknown && res.Server.Dolt.GC != nil
				})
			}
			stop()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Serve = %v", err)
				}
			case <-time.After(patience):
				t.Fatalf("Serve did not return within %s of its stop, with a read of Dolt's metrics under way", patience)
			}
			at := f.hits.Load()
			time.Sleep(50 * time.Millisecond)
			if n := f.hits.Load(); n != at {
				t.Errorf("Dolt's endpoint was asked %d more times after Serve returned", n-at)
			}
		})
	}
}

// On a server of many projects the writes are every open store's. A
// store that closes takes nothing back from the count, and one that
// opens again adds from zero. Only a server admin is shown Dolt: it is
// the server's, not a project's.
func TestDoltManyProjects(t *testing.T) {
	t.Parallel()
	m := newMulti(t, Limits{OpenStores: 8}, nil)
	m.project(t, projA, "alice=admin")
	m.project(t, projB, "bob")
	f := newFakeDolt(t)
	advance := watchDolt(t, m.Server, f.url)
	ctx := t.Context()
	read := func(cycles int) int64 {
		t.Helper()
		advance(15 * time.Second)
		f.serve(doltBody(cycles))
		m.ReadDolt(ctx)
		res, _, _ := localReport(t, m.Server)
		if res.Server.Dolt.GC == nil {
			return -1
		}
		return res.Server.Dolt.GC.Writes
	}
	a, b := mustDialTo(t, m.Server, alice, projA), mustDialTo(t, m.Server, bob, projB)
	if !a.alive() || !b.alive() {
		t.Fatal("a connection is not open")
	}
	read(1)
	for i := range 2 {
		mustAsk[proto.CreateResult](a, proto.OpCreate, proto.CreateArgs{Title: fmt.Sprintf("a %d", i)})
	}
	for i := range 3 {
		mustAsk[proto.CreateResult](b, proto.OpCreate, proto.CreateArgs{Title: fmt.Sprintf("b %d", i)})
	}
	if got := read(2); got != 5 {
		t.Fatalf("writes in the window = %d, want the 5 made in two projects", got)
	}
	// Project b's store closes; a's is written to.
	b.hangUp()
	waitFor(t, "project b's store to be unused", func() bool {
		m.stores.closeAll()
		return m.OpenStores() == 1
	})
	mustAsk[proto.CreateResult](a, proto.OpCreate, proto.CreateArgs{Title: "a more"})
	after := read(3)
	if after < 6 {
		t.Errorf("writes in the window after a store closed = %d, want at least the 6 counted so far", after)
	}
	// It opens again, counting from zero.
	b = mustDialTo(t, m.Server, bob, projB)
	mustAsk[proto.CreateResult](b, proto.OpCreate, proto.CreateArgs{Title: "b again"})
	if got := read(4); got < after+1 {
		t.Errorf("writes in the window after the store opened again = %d, want more than the %d before", got, after)
	}

	res := mustAsk[proto.StatusResult](a, proto.OpStatus, proto.StatusArgs{})
	if res.Server != nil || len(res.Projects) != 1 {
		t.Errorf("a project admin's status = server %s and %d projects; want no server part, and so nothing of Dolt", js(res.Server), len(res.Projects))
	}
	d := mustDialTo(t, m.Server, dana, projA)
	if res := mustAsk[proto.StatusResult](d, proto.OpStatus, proto.StatusArgs{}); res.Server == nil || res.Server.Dolt == nil || res.Server.Dolt.Version != "2.4.2" {
		t.Errorf("a server admin's status = %s, want Dolt's figures", js(res.Server))
	}
}

// A real Dolt, read through its own endpoint: the figures are its own,
// and the writes set against its collector are the ones this server
// made.
func TestDoltStatusRealDolt(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	dolt := doltOf(t)
	if dolt.MetricsURL() == "" {
		t.Fatal("the tests' Dolt has no metrics endpoint")
	}
	r, err := doltmetrics.New(t.Context(), dolt.MetricsURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	s.cfg.DoltMetrics = r
	ctx := t.Context()
	version, err := dolttest.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	s.ReadDolt(ctx)
	const writes = 40
	writeIssues(t, s, writes)
	s.ReadDolt(ctx)
	res, text, _ := localReport(t, s)
	d := res.Server.Dolt
	if d == nil || d.Unknown {
		t.Fatalf("Dolt = %s, want its figures:\n%s", js(d), text)
	}
	if d.Version != version {
		t.Errorf("version %q, want %q, the dolt on PATH", d.Version, version)
	}
	if d.ResidentBytes == nil || *d.ResidentBytes < 10<<20 {
		t.Errorf("resident memory in %s, want Dolt's own, tens of mebibytes at least", js(d))
	}
	// This store's connections at least; other tests' too.
	if d.Conns == nil || *d.Conns < 1 {
		t.Errorf("connections in %s, want this store's at least", js(d))
	}
	if d.MemLimitBytes == nil || *d.MemLimitBytes != 0 {
		t.Errorf("memory limit in %s, want 0: the tests' Dolt runs without one", js(d))
	}
	if d.StartedAt == nil || d.StartedAt.After(began) || d.UptimeSeconds == nil || *d.UptimeSeconds < 0 || d.ReadAt == nil || d.ReadAt.Before(began) {
		t.Errorf("%s; want a start before this test, at %s, and a read during it", js(d), began.UTC().Format(time.RFC3339))
	}
	g := d.GC
	if g == nil || g.Writes != writes || g.Per100Writes == nil || *g.Per100Writes != 100*float64(g.Cycles)/writes {
		t.Errorf("collector = %s, want %d writes and its cycles for each 100 of them", js(g), writes)
	}
	if !strings.Contains(text, "Dolt "+version+", up ") || !strings.Contains(text, "no memory limit\nDolt's collector: ") {
		t.Errorf("the status lacks Dolt's lines:\n%s", text)
	}
}
