package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/pgtest"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
	"github.com/ariesworx/aries/internal/telemetry"
)

// measured makes a Telemetry whose instruments a test reads in process.
func measured(t *testing.T, perProject bool) (*telemetry.Telemetry, *telemetry.Manual) {
	t.Helper()
	tel, m, err := telemetry.NewManual(telemetry.Config{Version: "v0.2.0", PerProject: perProject, Ops: Ops()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	return tel, m
}

func snapshot(t *testing.T, m *telemetry.Manual) telemetry.Snapshot {
	t.Helper()
	snap, err := m.Collect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// newServerTel is newServerWith on a server and a store measured and
// traced by tel, as `ariesd serve` builds them for one project.
func newServerTel(t *testing.T, lim Limits, tel *telemetry.Telemetry) *Server {
	t.Helper()
	dsn := newDatabase(t)
	st, err := store.Open(t.Context(), dsn, store.Options{Prefix: "ar", Admins: []string{dana.Principal},
		Limits: lim.Limits, Observer: StoreObserver(tel, project), Tracer: StoreTracer(tel, project)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s, err := New(Config{Store: st, Project: project, Version: "v0.2.0", PeerCheck: func(net.Conn) error { return nil },
		Limits: lim, Telemetry: tel, ReapInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// want is one series a test expects: its value, or for a histogram how
// many observations it has.
type want struct {
	name  string
	attrs []string // key, value, key, value
	value float64
}

func check(t *testing.T, snap telemetry.Snapshot, wants []want) {
	t.Helper()
	for _, w := range wants {
		p, ok := snap.Find(w.name, w.attrs...)
		got := p.Value
		if strings.HasSuffix(w.name, ".duration") {
			got = float64(p.Count)
		}
		if w.value == 0 {
			if ok && got != 0 {
				t.Errorf("%s%v = %v, want no such series, or zero", w.name, w.attrs, got)
			}
			continue
		}
		if !ok || got != w.value {
			t.Errorf("%s%v = %v (series found: %t), want %v", w.name, w.attrs, got, ok, w.value)
		}
	}
	if t.Failed() {
		t.Logf("collected:\n%s", ariesOnly(snap))
	}
}

// ariesOnly is snap without the Go runtime's metrics.
func ariesOnly(snap telemetry.Snapshot) telemetry.Snapshot {
	return slices.DeleteFunc(slices.Clone(snap), func(p telemetry.Point) bool { return !strings.HasPrefix(p.Name, "aries.") })
}

// A known sequence of requests through a server of one project gives
// exactly the counts it should, and each histogram its observations.
func TestMetricsOfRequests(t *testing.T) {
	t.Parallel()
	tel, m := measured(t, true)
	s := newServerTel(t, Limits{}, tel)
	c := dialPipe(t, s, alice)
	a := mustAsk[proto.CreateResult](c, proto.OpCreate, proto.CreateArgs{Title: "first"})
	mustAsk[proto.CreateResult](c, proto.OpCreate, proto.CreateArgs{Title: "second"})
	mustAsk[proto.ListResult](c, proto.OpList, proto.ListArgs{})
	if _, perr := ask[proto.ShowResult](c, proto.OpShow, proto.ShowArgs{ID: "ar-none"}); perr == nil || perr.Code != proto.CodeNotFound {
		t.Fatalf("show of an unknown issue = %v, want not_found", perr)
	}
	if _, perr := ask[proto.WriteResult](c, proto.OpCreate, proto.CreateArgs{}); perr == nil || perr.Code != proto.CodeInvalid {
		t.Fatalf("create with no title = %v, want invalid", perr)
	}
	if _, perr := ask[proto.WriteResult](c, "no.such.op", struct{}{}); perr == nil || perr.Code != proto.CodeInvalid {
		t.Fatalf("an unknown op = %v, want invalid", perr)
	}
	// An admin op on a server of one project is refused, and is no write
	// to a control store it does not have... but it is counted as asked.
	if _, perr := ask[proto.WriteResult](c, proto.OpProjectArchive, proto.ProjectArgs{Project: "x"}); perr == nil || perr.Code != proto.CodeInvalid {
		t.Fatalf("project.archive on a server of one project = %v, want invalid", perr)
	}
	mustAsk[proto.StartResult](c, proto.OpStart, proto.StartArgs{ID: a.ID})
	mustAsk[proto.FinishResult](c, proto.OpFinish, proto.FinishArgs{ID: a.ID, Reason: "done"})

	open := snapshot(t, m)
	check(t, open, []want{
		{"aries.handshakes", []string{"result", "ok"}, 1},
		{"aries.connections", []string{"project", project}, 1},
		{"aries.request.duration", []string{"op", "create", "result", "ok"}, 2},
		{"aries.request.duration", []string{"op", "create", "result", "invalid"}, 1},
		{"aries.request.duration", []string{"op", "list", "result", "ok"}, 1},
		{"aries.request.duration", []string{"op", "show", "result", "not_found"}, 1},
		{"aries.request.duration", []string{"op", "unknown", "result", "invalid"}, 1},
		{"aries.request.duration", []string{"op", "project.archive", "result", "invalid"}, 1},
		{"aries.request.duration", []string{"op", "start", "result", "ok"}, 1},
		{"aries.request.duration", []string{"op", "finish", "result", "ok"}, 1},
		{"aries.project.requests", []string{"project", project, "result_class", "ok"}, 5},
		{"aries.project.requests", []string{"project", project, "result_class", "refused"}, 4},
		{"aries.project.requests", []string{"project", project, "result_class", "error"}, 0},
		{"aries.control.writes", []string{"op", "project.archive", "result", "invalid"}, 1},
		{"aries.claims", []string{"event", "take", "reason", "none"}, 1},
		{"aries.claims", []string{"event", "release", "reason", "none"}, 1},
		{"aries.claims", []string{"event", "expire", "reason", "none"}, 0},
		{"aries.stores.open", nil, 1},
	})
	// The handshake's presence touch, two creates, a start and a finish
	// each changed a row; what the request did beyond that is the
	// store's business, so the count has a floor, not a value.
	if n := open.Observations("aries.store.write.duration", "project", project); n < 4 {
		t.Errorf("aries.store.write.duration has %d observations, want at least the 4 writes that committed", n)
	}
	if n, st := open.Observations("aries.store.write.duration"), s.cfg.Store.WriteStats().Samples; float64(n) != float64(st) {
		t.Errorf("aries.store.write.duration has %d observations and status counts %d writes: they are one measurement", n, st)
	}
	if total := open.Observations("aries.request.duration"); total != 9 {
		t.Errorf("aries.request.duration has %d observations in all, want the 9 requests", total)
	}

	c.hangUp()
	check(t, snapshot(t, m), []want{{"aries.connections", []string{"project", project}, 0}})
}

// Handshakes are counted by how they end.
func TestMetricsOfHandshakes(t *testing.T) {
	t.Parallel()
	tel, m := measured(t, true)
	s := newServerTel(t, Limits{ConnsPerPrincipal: 1}, tel)
	other := "99999999-9999-4999-8999-999999999999"
	for _, tc := range []struct {
		name   string
		frames []*proto.Frame
	}{
		{"ok", []*proto.Frame{{T: proto.FrameBridge, Principal: "alice"}, {T: proto.FrameHello, Proto: proto.Proto, Project: project, Session: "s1", Machine: "m"}}},
		{"at the principal's cap", []*proto.Frame{{T: proto.FrameBridge, Principal: "alice"}, {T: proto.FrameHello, Proto: proto.Proto, Project: project, Session: "s2", Machine: "m"}}},
		{"another project", []*proto.Frame{{T: proto.FrameBridge, Principal: "bob"}, {T: proto.FrameHello, Proto: proto.Proto, Project: other}}},
		{"a hostile project", []*proto.Frame{{T: proto.FrameBridge, Principal: "bob"}, {T: proto.FrameHello, Proto: proto.Proto, Project: "x\n\u202e" + strings.Repeat("y", 300)}}},
		{"a protocol too new", []*proto.Frame{{T: proto.FrameBridge, Principal: "bob"}, {T: proto.FrameHello, Proto: proto.ProtoMax + 1, Project: project}}},
		{"no hello", []*proto.Frame{{T: proto.FrameBridge, Principal: "bob"}, {T: proto.FrameReq, Op: proto.OpList}}},
		{"no bridge frame", []*proto.Frame{{T: proto.FrameHello, Proto: proto.Proto, Project: project}}},
		{"a reserved principal", []*proto.Frame{{T: proto.FrameBridge, Principal: store.ReaperActor.Principal}}},
	} {
		srv, cli := net.Pipe()
		var wg sync.WaitGroup
		wg.Go(func() { s.handle(t.Context(), srv); _ = srv.Close() })
		enc, dec := proto.NewEncoder(cli), proto.NewDecoder(cli)
		wg.Go(func() {
			for _, f := range tc.frames {
				if enc.Encode(f) != nil {
					return
				}
			}
		})
		_ = cli.SetReadDeadline(time.Now().Add(patience))
		_, _ = dec.Decode() // the welcome, a refusal, or the close
		if tc.name == "ok" {
			t.Cleanup(func() { _ = cli.Close(); wg.Wait() }) // held, so the next is at the cap
			continue
		}
		_ = cli.Close()
		wg.Wait()
	}
	snap := snapshot(t, m)
	check(t, snap, []want{
		{"aries.handshakes", []string{"result", "ok"}, 1},
		{"aries.handshakes", []string{"result", "busy"}, 1},
		{"aries.handshakes", []string{"result", "not_found"}, 2},
		{"aries.handshakes", []string{"result", "version"}, 1},
		{"aries.handshakes", []string{"result", "invalid"}, 1},
		{"aries.handshakes", []string{"result", "failed"}, 2},
		{"aries.limit.refusals", []string{"limit", "conns_per_principal"}, 1},
		{"aries.limit.refusals", []string{"limit", "conns_per_server"}, 0},
	})
	if got := snap.Sum("aries.handshakes"); got != 8 {
		t.Errorf("aries.handshakes counts %v in all, want the 8 handshakes", got)
	}
}

// Whatever a client sends, the series stay within the lists in the
// code: unknown ops are one value, a project the server does not serve
// is never a value, and nothing of the client's own is an attribute.
func TestMetricsStayBounded(t *testing.T) {
	t.Parallel()
	tel, m := measured(t, true)
	s := newServerTel(t, Limits{WriteBurst: 100000, WriteRate: 100000}, tel)
	hostile := []string{"", "x", "create ", "CREATE", "create\n", "../etc", "alice", "s-a", "laptop-a", `{"op":"x"}`, "\u202eetaerc", strings.Repeat("o", 2000)}
	for i := range 200 {
		hostile = append(hostile, fmt.Sprintf("op-%d", i), fmt.Sprintf("%08x-aaaa-4aaa-8aaa-aaaaaaaaaaaa", i))
	}
	c := dialPipe(t, s, alice)
	for _, op := range hostile {
		if _, perr := ask[proto.WriteResult](c, op, struct{}{}); perr == nil || perr.Code != proto.CodeInvalid {
			t.Fatalf("op %.20q = %v, want invalid", op, perr)
		}
		// The same strings as an issue id, a session and a project.
		_, _ = ask[proto.ShowResult](c, proto.OpShow, proto.ShowArgs{ID: op})
	}
	for i, proj := range hostile[:40] {
		w, _ := dialTo(t, s, store.Actor{Principal: "bob", Session: fmt.Sprintf("s-%d-%s", i, proj), Machine: proj}, proj)
		if w.Err == nil {
			t.Fatalf("a hello for project %.20q was welcomed", proj)
		}
	}
	snap := ariesOnly(snapshot(t, m))
	check(t, snap, []want{
		{"aries.request.duration", []string{"op", "unknown", "result", "invalid"}, float64(len(hostile))},
	})
	limits := map[string]int{
		"aries.request.duration":    3, // unknown/invalid, show/not_found, show/invalid
		"aries.project.requests":    1,
		"aries.handshakes":          3, // ok, not_found, invalid
		"aries.connections":         1,
		"aries.connections.dropped": 0,
	}
	for name, most := range limits {
		if n := len(snap.Series(name)); n > most {
			t.Errorf("%s has %d series after %d hostile requests, want at most %d:\n%s", name, n, 2*len(hostile), most, snap.Series(name))
		}
	}
	if len(snap) > 40 {
		t.Errorf("%d series in all, want a handful:\n%s", len(snap), snap)
	}
	for _, p := range snap {
		for k, v := range p.Attrs {
			if k == "project" && v != project {
				t.Errorf("%s names project %q, which the server does not serve", p.Name, v)
			}
			if slices.Contains(hostile, v) || strings.Contains(v, "alice") || strings.Contains(v, "laptop") || strings.HasPrefix(v, "s-") || strings.HasPrefix(v, "ar-") {
				t.Errorf("%s has %s=%q, which a client chose", p.Name, k, v)
			}
		}
	}
}

// The write limits are counted by name, and their refusals as busy.
func TestMetricsOfLimits(t *testing.T) {
	t.Parallel()
	tel, m := measured(t, true)
	s := newServerTel(t, Limits{WriteBurst: 2, WriteRate: 0.001, Conns: 2, ConnsPerPrincipal: 2}, tel)
	c := dialPipe(t, s, alice)
	for i := range 2 {
		mustAsk[proto.CreateResult](c, proto.OpCreate, proto.CreateArgs{Title: fmt.Sprintf("w%d", i)})
	}
	for range 3 {
		if _, perr := ask[proto.WriteResult](c, proto.OpCreate, proto.CreateArgs{Title: "over"}); perr == nil || perr.Code != proto.CodeBusy {
			t.Fatalf("a write past the bucket = %v, want busy", perr)
		}
	}
	// The server counts a handshake after writing its answer, which the
	// client may read first; it serves the first request only after
	// counting, so an answered request means bob's is counted.
	mustAsk[proto.ListResult](dialPipe(t, s, bob), proto.OpReady, proto.LimitArgs{})
	w, refused := handshakeAs(t, s, dana)
	if w.Err == nil || w.Err.Code != proto.CodeBusy {
		t.Fatalf("a third connection with conns: 2 = %+v, want busy", w.Err)
	}
	// The handshake is counted once the server has written its answer,
	// which the client may read first: wait for the handler.
	refused.hangUp()
	check(t, snapshot(t, m), []want{
		{"aries.limit.refusals", []string{"limit", "writes_per_second"}, 3},
		{"aries.limit.refusals", []string{"limit", "conns_per_server"}, 1},
		{"aries.request.duration", []string{"op", "create", "result", "busy"}, 3},
		{"aries.request.duration", []string{"op", "create", "result", "ok"}, 2},
		{"aries.handshakes", []string{"result", "busy"}, 1},
		{"aries.handshakes", []string{"result", "ok"}, 2},
	})
}

// A server of many projects: the instruments that name a project, the
// control writes, the reasons connections and claims end, the stores
// and the keys file.
func TestMetricsOfManyProjects(t *testing.T) {
	t.Parallel()
	tel, m := measured(t, true)
	ck := newClock()
	mu := newMultiTel(t, Limits{}, ck, tel, dana.Principal)
	mu.project(t, projA, "alice", "bob")
	mu.project(t, projB, "alice")
	mu.refresh(t.Context())

	a := mustDialTo(t, mu.Server, alice, projA)
	b := mustDialTo(t, mu.Server, alice, projB)
	bobs := mustDialTo(t, mu.Server, bob, projA)
	d := mustDialTo(t, mu.Server, dana, projA)
	is := mustAsk[proto.CreateResult](bobs, proto.OpCreate, proto.CreateArgs{Title: "bob's"})
	mustAsk[proto.StartResult](bobs, proto.OpStart, proto.StartArgs{ID: is.ID})
	mustAsk[proto.ListResult](a, proto.OpList, proto.ListArgs{})
	mustAsk[proto.ListResult](b, proto.OpList, proto.ListArgs{})
	mustAsk[proto.ListResult](b, proto.OpList, proto.ListArgs{})

	// alice is no admin: her access change is refused, and counted so.
	if _, perr := ask[proto.MemberRemoveResult](a, proto.OpMemberRemove, proto.MemberArgs{Name: "bob"}); perr == nil || perr.Code != proto.CodeForbidden {
		t.Fatalf("member.remove by a member = %v, want forbidden", perr)
	}
	// dana's ends bob's access: his connection is dropped, his claim
	// released, each for that reason.
	rm := mustAsk[proto.MemberRemoveResult](d, proto.OpMemberRemove, proto.MemberArgs{Name: "bob"})
	if rm.Released != 1 {
		t.Fatalf("member.remove released %d claims, want bob's 1", rm.Released)
	}
	if !bobs.dropped() {
		t.Fatal("bob's connection was not closed")
	}
	bobs.hangUp()
	mustAsk[proto.KeyResult](d, proto.OpKeyAdd, proto.KeyAddArgs{Name: "alice", Key: keyLine(t, "alice")})

	snap := snapshot(t, m)
	check(t, snap, []want{
		{"aries.handshakes", []string{"result", "ok"}, 4},
		{"aries.connections", []string{"project", projA}, 2},
		{"aries.connections", []string{"project", projB}, 1},
		{"aries.project.requests", []string{"project", projA, "result_class", "ok"}, 5},
		{"aries.project.requests", []string{"project", projA, "result_class", "refused"}, 1},
		{"aries.project.requests", []string{"project", projB, "result_class", "ok"}, 2},
		{"aries.control.writes", []string{"op", "member.remove", "result", "forbidden"}, 1},
		{"aries.control.writes", []string{"op", "member.remove", "result", "ok"}, 1},
		{"aries.control.writes", []string{"op", "key.add", "result", "ok"}, 1},
		{"aries.connections.dropped", []string{"reason", "membership"}, 1},
		{"aries.claims", []string{"event", "take", "reason", "none"}, 1},
		{"aries.claims", []string{"event", "release", "reason", "membership"}, 1},
		{"aries.store.open.duration", []string{"migrated", "true"}, 2},
		{"aries.store.open.duration", []string{"migrated", "false"}, 0},
		{"aries.stores.open", nil, 2},
		{"aries.keys_file.writes", []string{"result", "ok"}, 2}, // the start's, and the key added
		{"aries.keys_file.writes", []string{"result", "failed"}, 0},
	})
	if at := snap.Value("aries.keys_file.last_write.time"); time.Since(time.Unix(int64(at), 0)) > time.Hour {
		t.Errorf("aries.keys_file.last_write.time = %v, want a time just now", at)
	}
	// The pool every store shares: between requests it holds what it
	// opened, none in use, and nobody waited for one.
	if n := snap.Value("aries.db.pool.conns"); n < 1 || n > float64(DefaultLimits.DBConns) {
		t.Errorf("aries.db.pool.conns = %v, want the pool's, 1 to %d", n, DefaultLimits.DBConns)
	}
	check(t, snap, []want{{"aries.db.pool.in_use", nil, 0}, {"aries.db.pool.waits", nil, 0}})
	if n := snap.Observations("aries.store.write.duration", "project", projA); n < 2 {
		t.Errorf("aries.store.write.duration{project=A} has %d observations, want bob's writes", n)
	}
	// Each project's own series: B took no write beyond presence.
	if nA, nB := snap.Observations("aries.store.write.duration", "project", projA), snap.Observations("aries.store.write.duration", "project", projB); nB >= nA {
		t.Errorf("aries.store.write.duration: project B has %d observations, A %d; want B's fewer", nB, nA)
	}

	// A store closed for idleness and opened again has nothing to migrate.
	a.hangUp()
	b.hangUp()
	d.hangUp()
	ck.advance(time.Hour)
	mu.CloseIdleStores()
	check(t, snapshot(t, m), []want{{"aries.stores.open", nil, 0}, {"aries.connections", []string{"project", projA}, 0}})
	mustDialTo(t, mu.Server, alice, projB)
	check(t, snapshot(t, m), []want{
		{"aries.stores.open", nil, 1},
		{"aries.store.open.duration", []string{"migrated", "false"}, 1},
		{"aries.store.open.duration", []string{"migrated", "true"}, 2},
	})
}

// With per_project off, no instrument names a project, and the numbers
// are the same.
func TestMetricsWithoutProjects(t *testing.T) {
	t.Parallel()
	tel, m := measured(t, false)
	mu := newMultiTel(t, Limits{}, nil, tel, dana.Principal)
	mu.project(t, projA, "alice")
	mu.project(t, projB, "alice")
	a := mustDialTo(t, mu.Server, alice, projA)
	b := mustDialTo(t, mu.Server, alice, projB)
	mustAsk[proto.CreateResult](a, proto.OpCreate, proto.CreateArgs{Title: "in A"})
	mustAsk[proto.CreateResult](b, proto.OpCreate, proto.CreateArgs{Title: "in B"})
	snap := ariesOnly(snapshot(t, m))
	for _, p := range snap {
		if _, ok := p.Attrs["project"]; ok {
			t.Errorf("%s has a project attribute with per_project off: %v", p.Name, p.Attrs)
		}
		for _, v := range p.Attrs {
			if v == projA || v == projB {
				t.Errorf("%s names a project in %v with per_project off", p.Name, p.Attrs)
			}
		}
	}
	check(t, snap, []want{
		{"aries.connections", nil, 2},
		{"aries.project.requests", []string{"result_class", "ok"}, 2},
	})
	if n := snap.Observations("aries.store.write.duration"); n < 2 {
		t.Errorf("aries.store.write.duration has %d observations, want both projects' writes in the one series", n)
	}
	if n := len(snap.Series("aries.store.write.duration")); n != 1 {
		t.Errorf("aries.store.write.duration has %d series with per_project off, want 1", n)
	}
}

// A write of the keys file that fails is counted, and /readyz says so
// until one succeeds.
func TestReadyFollowsTheKeysFile(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory this user cannot write to")
	}
	tel, m := measured(t, true)
	mu := newMultiTel(t, Limits{}, nil, tel, dana.Principal)
	mu.project(t, projA, "alice")
	mu.listening.Store(true) // as while Serve accepts
	if why := mu.Ready(t.Context()); why != "" {
		t.Fatalf("Ready() = %q, want ready", why)
	}
	dir := filepath.Dir(mu.keysFile)
	if err := os.Chmod(dir, 0o500); err != nil { //nolint:gosec // a directory the server cannot write to
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // back to what it was
	if _, err := mu.ctl.AddKey(t.Context(), dana, "alice", keyLine(t, "alice")); err != nil {
		t.Fatal(err)
	}
	mu.refresh(t.Context()) // a reaper tick: the write fails
	mu.refresh(t.Context()) // and is tried again
	if why := mu.Ready(t.Context()); why != "the last write of the keys file failed" {
		t.Errorf("Ready() with the keys file unwritable = %q, want it to name the keys file", why)
	}
	check(t, snapshot(t, m), []want{{"aries.keys_file.writes", []string{"result", "failed"}, 2}})

	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // writable again
		t.Fatal(err)
	}
	mu.refresh(t.Context())
	if why := mu.Ready(t.Context()); why != "" {
		t.Errorf("Ready() after the keys file was written = %q, want ready", why)
	}
	check(t, snapshot(t, m), []want{
		{"aries.keys_file.writes", []string{"result", "failed"}, 2},
		{"aries.keys_file.writes", []string{"result", "ok"}, 2}, // the start's, and this one
	})
}

// /readyz is ready only while the daemon listens and the database
// answers; the check of the database is made once and stands for
// dbCheckEvery, however often it is asked.
func TestReadyFollowsTheDatabase(t *testing.T) {
	// Serial: bounds elapsed time, which a test running beside it would stretch.
	// A server of the test's own, and not the pool's, since it is stopped.
	dir, err := os.MkdirTemp("", "sfready")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	own, err := pgtest.Start(ctx, dir)
	if errors.Is(err, pgtest.ErrNoPostgres) {
		t.Skip("postgres is not on PATH: run scripts/install-postgres.sh to run the server tests")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = own.Stop() })
	dsn, err := own.NewDatabase(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, dsn, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := &syncBuffer{}
	s, err := New(Config{Store: st, Project: project, PeerCheck: func(net.Conn) error { return nil }, ReapInterval: -1,
		Logger: slogTo(log)})
	if err != nil {
		t.Fatal(err)
	}
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	s.db.now = func() time.Time { return time.Unix(0, now.Load()) }
	later := func() { now.Add(int64(dbCheckEvery)) }

	if why := s.Ready(ctx); why != "ariesd is not listening on its socket" {
		t.Errorf("Ready() before Serve = %q, want it to say the daemon is not listening", why)
	}
	l := newFakeListener()
	sctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Serve(sctx, l)
	}()
	t.Cleanup(func() { stop(); <-done })
	waitFor(t, "the server to listen", func() bool { return s.listening.Load() })
	if why := s.Ready(ctx); why != "" {
		t.Fatalf("Ready() while serving = %q, want ready", why)
	}

	if err := own.Stop(); err != nil {
		t.Fatal(err)
	}
	// The last answer stands until it is dbCheckEvery old: asking does
	// not reach the database.
	for range 50 {
		if why := s.Ready(ctx); why != "" {
			t.Fatalf("Ready() just after the database stopped = %q, want the cached answer", why)
		}
	}
	later()
	start := time.Now()
	if why := s.Ready(ctx); why != "the last check of the database failed" {
		t.Errorf("Ready() with the database stopped = %q, want it to name the database", why)
	}
	if d := time.Since(start); d > dbCheckTimeout+3*time.Second {
		t.Errorf("the check of a stopped database took %s, want it bounded by %s", d, dbCheckTimeout)
	}
	if !strings.Contains(log.String(), "the check of the database failed") {
		t.Errorf("the failed check was not logged:\n%s", log)
	}

	// The database back on the same socket: ready again at the next check.
	if err := own.Restart(ctx); err != nil {
		t.Fatalf("restart postgres: %v", err)
	}
	if why := s.Ready(ctx); why != "the last check of the database failed" {
		t.Errorf("Ready() just after the database came back = %q, want the cached answer", why)
	}
	waitFor(t, "the daemon to be ready again", func() bool {
		later()
		return s.Ready(ctx) == ""
	})

	stop()
	<-done
	if why := s.Ready(ctx); why != "ariesd is not listening on its socket" {
		t.Errorf("Ready() after Serve returned = %q, want it to say the daemon is not listening", why)
	}
}

// waitFor polls ok until it holds, failing the test after 30 seconds.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The reaper's ticks are measured, and the heartbeat comes from its
// goroutine: often between ticks, and not at all once it has stopped.
func TestReaperMetricsAndHeartbeat(t *testing.T) {
	t.Parallel()
	tel, m := measured(t, true)
	s := newServerTel(t, Limits{}, tel)
	s.cfg.ReapInterval = 20 * time.Millisecond
	var beats atomic.Int64
	s.cfg.Heartbeat, s.cfg.HeartbeatEvery = func() { beats.Add(1) }, 5*time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, newFakeListener()) }()
	waitFor(t, "two ticks of the reaper and some heartbeats", func() bool {
		snap := snapshot(t, m)
		return snap.Observations("aries.reaper.duration") >= 2 && beats.Load() >= 3
	})
	snap := snapshot(t, m)
	if at := snap.Value("aries.reaper.last_run.time"); time.Since(time.Unix(int64(at), 0)) > time.Minute {
		t.Errorf("aries.reaper.last_run.time = %v, want a time just now", at)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Serve has returned, so the reaper's goroutine has: nothing is left
	// to send a heartbeat.
	n := beats.Load()
	runtime.Gosched()
	if got := beats.Load(); got != n {
		t.Errorf("%d heartbeats after the reaper stopped, want none", got-n)
	}
}

func slogTo(w io.Writer) *slog.Logger {
	return slog.New(logs.NewHandler(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}), LogIDs))
}

// `ariesd status` gets the report a server admin gets, over the
// daemon's socket and with no key, and nothing else.
func TestLocalStatus(t *testing.T) {
	t.Parallel()
	sockDir := func(t *testing.T) string {
		t.Helper()
		// Not t.TempDir, whose path can be too long for a unix socket.
		dir, err := os.MkdirTemp("", "sfs")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		return filepath.Join(dir, "run", "d.sock")
	}
	serve := func(t *testing.T, s *Server) string {
		t.Helper()
		sock := sockDir(t)
		l, err := Listen(sock)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- s.Serve(ctx, l) }()
		t.Cleanup(func() {
			cancel()
			if err := <-done; err != nil {
				t.Error(err)
			}
		})
		return sock
	}

	t.Run("a server of many projects", func(t *testing.T) {
		m := adminWorld(t, Limits{})
		a := mustDialTo(t, m.Server, alice, projA)
		mustAsk[proto.CreateResult](a, proto.OpCreate, proto.CreateArgs{Title: "a"})
		d := mustDialTo(t, m.Server, dana, projA)
		over := mustAsk[proto.StatusResult](d, proto.OpStatus, proto.StatusArgs{})
		sock := serve(t, m.Server)
		got, err := Status(t.Context(), sock, "v0.2.0")
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if got.Server == nil || got.Server.Version != "v0.2.0" || got.Server.OpenStores != 1 || got.Server.Conns != 2 ||
			got.Server.KeysFile == nil || got.Server.Projects["active"] != 2 {
			t.Errorf("Status().Server = %+v, want the server as an admin sees it: one store open, alice's and dana's connections", got.Server)
		}
		names := func(r proto.StatusResult) []string {
			var out []string
			for _, p := range r.Projects {
				out = append(out, fmt.Sprintf("%s/%s/open=%t/conns=%d", p.Name, p.State, p.Open, p.Conns))
			}
			return out
		}
		if !slices.Equal(names(got), names(over)) || len(got.Projects) != 2 {
			t.Errorf("Status() lists %v, and `aries admin status` for a server admin lists %v: want the same two", names(got), names(over))
		}
		// The status connection is no session: it holds no slot and is in
		// no project's who.
		if n := m.connections(StatusPrincipal, ""); n != 0 {
			t.Errorf("the status connection is tracked as a session")
		}
	})

	t.Run("every page of a long list", func(t *testing.T) {
		m := newMulti(t, Limits{}, nil)
		m.project(t, projA)
		m.project(t, projB)
		m.project(t, projC)
		srv, cli := net.Pipe()
		c := &conn{t: t, enc: proto.NewEncoder(cli), dec: proto.NewDecoder(cli), nc: cli}
		t.Cleanup(c.hangUp)
		c.wg.Go(func() { m.handle(t.Context(), srv); _ = srv.Close() }) // as Serve closes it
		c.wg.Go(func() {
			_ = c.enc.Encode(&proto.Frame{T: proto.FrameBridge, Principal: StatusPrincipal})
			_ = c.enc.Encode(&proto.Frame{T: proto.FrameHello, Proto: proto.Proto})
		})
		if w := c.read(); w.Err != nil || w.Principal != StatusPrincipal {
			t.Fatalf("welcome = %+v, want one for %s", w, StatusPrincipal)
		}
		page := mustAsk[proto.StatusResult](c, proto.OpStatus, proto.StatusArgs{AdminListArgs: proto.AdminListArgs{Limit: 2}})
		if len(page.Projects) != 2 || page.More != 1 || page.Server == nil {
			t.Errorf("the first page = %d projects and %d more, want 2 and 1, with the server", len(page.Projects), page.More)
		}
		// Anything but status is refused, and the connection closed.
		if _, perr := ask[proto.ListResult](c, proto.OpList, proto.ListArgs{}); perr == nil || perr.Code != proto.CodeInvalid || perr.Fix == "" {
			t.Errorf("list on a status connection = %v, want invalid with a fix", perr)
		}
		if !c.dropped() {
			t.Errorf("the status connection stayed open after a request that is not status")
		}
	})

	t.Run("a server of one project", func(t *testing.T) {
		s := newServer(t)
		dialPipe(t, s, alice)
		got, err := Status(t.Context(), serve(t, s), "v0.2.0")
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if got.Server == nil || got.Server.OpenStores != 1 || got.Server.OpenStoresMax != 1 || got.Server.Conns != 1 || got.Server.KeysFile != nil {
			t.Errorf("Status().Server = %+v, want one store of one, one connection and no keys file", got.Server)
		}
		if len(got.Projects) != 1 || got.Projects[0].ID != project || !got.Projects[0].Open || got.Projects[0].Conns != 1 || got.Projects[0].Writes == nil {
			t.Fatalf("Status().Projects = %+v, want the one project, open, with alice's connection", got.Projects)
		}
		if used := got.Projects[0].LastUsed; used == nil || time.Since(*used) > time.Hour {
			t.Errorf("Status().Projects[0].LastUsed = %v, want when the project was opened or last answered, not the zero time", used)
		}
	})

	t.Run("a peer that is not the daemon's user", func(t *testing.T) {
		s := newServer(t)
		s.cfg.PeerCheck = func(net.Conn) error { return errors.New("peer uid 1001 is not the daemon's uid 999") }
		_, err := Status(t.Context(), serve(t, s), "v0.2.0")
		if err == nil || !strings.Contains(err.Error(), "fix: run it as the user ariesd runs as") {
			t.Errorf("Status() from a refused peer = %v, want a refusal with a fix", err)
		}
	})

	t.Run("no daemon", func(t *testing.T) {
		_, err := Status(t.Context(), sockDir(t), "v0.2.0")
		if err == nil || !strings.Contains(err.Error(), "fix: ") {
			t.Errorf("Status() with no daemon = %v, want an error with a fix", err)
		}
	})

	// The report is bounded, so this is a daemon that is itself stuck or
	// went away: the error says where to look.
	t.Run("a daemon that welcomes and then does not answer", func(t *testing.T) {
		sock := fakeDaemon(t, func(*proto.Frame) *proto.Frame {
			return &proto.Frame{T: proto.FrameWelcome, Principal: StatusPrincipal}
		})
		_, err := Status(t.Context(), sock, "v0.2.0")
		if err == nil || !strings.Contains(err.Error(), "the daemon did not answer") || !strings.Contains(err.Error(), "fix: see ariesd's log") {
			t.Errorf("Status() from a daemon that does not answer = %v, want an error that says so, with a fix", err)
		}
	})

	// No SSH key reaches it: the bridge refuses the name to every key.
	t.Run("the bridge refuses the name", func(t *testing.T) {
		var out strings.Builder
		err := Bridge(t.Context(), sockDir(t), StatusPrincipal, "", strings.NewReader(""), &out)
		if err == nil || !strings.Contains(out.String(), `"c":"auth"`) {
			t.Errorf("Bridge as %s = %v, wrote %q; want an auth refusal", StatusPrincipal, err, out.String())
		}
		if !store.Reserved(StatusPrincipal) {
			t.Errorf("%s is not a reserved principal", StatusPrincipal)
		}
	})
}

// Ops lists every op the server answers, once.
func TestOps(t *testing.T) {
	t.Parallel()
	ops := Ops()
	if !slices.IsSorted(ops) || len(slices.Compact(slices.Clone(ops))) != len(ops) {
		t.Errorf("Ops() is not sorted and unique: %v", ops)
	}
	for op := range maps.Keys(handlers) {
		if !slices.Contains(ops, op) {
			t.Errorf("Ops() lacks %q", op)
		}
	}
	for op := range controlWrites {
		if !slices.Contains(ops, op) {
			t.Errorf("Ops() lacks the control write %q", op)
		}
	}
}
