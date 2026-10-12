package server

import (
	"net"
	"testing"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// A claim that ends with its project's archive is counted once, as a
// release for that reason, however the archive reaches it (decision 24):
// in a store that was open, in one the archive had to open, and at the
// daemon's start in one an interrupted archive left behind. The store a
// start opens as it is, without migrating, is no measured opening.
func TestMetricsOfArchivedClaims(t *testing.T) {
	tel, mr := measured(t, true)
	m := adminWorldTel(t, Limits{}, tel)
	const projD = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	m.project(t, projC, "carol")
	m.project(t, projD, "carol")
	d := mustDialTo(t, m.Server, dana, projA)

	// B's store is open when it is archived.
	e := mustDialTo(t, m.Server, erin, projB)
	held(e, "erin's")
	mustAsk[proto.ProjectResult](d, proto.OpProjectArchive, proto.ProjectArgs{Project: projB})
	e.hangUp()
	check(t, snapshot(t, mr), []want{
		{"starfix.claims", []string{"event", "take", "reason", "none"}, 1},
		{"starfix.claims", []string{"event", "release", "reason", "archived"}, 1},
		{"starfix.store.open.duration", []string{"migrated", "true"}, 2}, // A's and B's
	})

	// C's is closed: the archive opens it, which is an opening like any
	// other, and ends the claim there.
	c := mustDialTo(t, m.Server, carol, projC)
	held(c, "carol's in C")
	c.hangUp()
	m.stores.closeAll() // C's; not A's, which dana's connection holds
	mustAsk[proto.ProjectResult](d, proto.OpProjectArchive, proto.ProjectArgs{Project: projC})
	check(t, snapshot(t, mr), []want{
		{"starfix.claims", []string{"event", "take", "reason", "none"}, 2},
		{"starfix.claims", []string{"event", "release", "reason", "archived"}, 2},
		{"starfix.store.open.duration", []string{"migrated", "true"}, 3},  // and C's first
		{"starfix.store.open.duration", []string{"migrated", "false"}, 1}, // C's again, for the archive
	})

	// D's archive reached the control store and no further, as when a
	// daemon stops between the two; the next start ends the claim.
	c = mustDialTo(t, m.Server, carol, projD)
	held(c, "carol's in D")
	c.hangUp()
	d.hangUp()
	if _, err := m.ctl.ArchiveProject(t.Context(), dana, projD); err != nil {
		t.Fatal(err)
	}
	m.stores.closeAll()
	before := snapshot(t, mr)
	if err := m.ReleaseArchived(t.Context(), m.asIs()); err != nil {
		t.Fatal(err)
	}
	after := snapshot(t, mr)
	check(t, after, []want{
		{"starfix.claims", []string{"event", "take", "reason", "none"}, 3},
		{"starfix.claims", []string{"event", "release", "reason", "archived"}, 3},
		{"starfix.claims", []string{"event", "release", "reason", "none"}, 0},
		{"starfix.claims", []string{"event", "release", "reason", "other"}, 0},
		{"starfix.claims", []string{"event", "expire", "reason", "none"}, 0},
	})
	if b, a := before.Observations("starfix.store.open.duration"), after.Observations("starfix.store.open.duration"); a != b {
		t.Errorf("starfix.store.open.duration went from %d to %d observations at ReleaseArchived, which opens a store as it is and beside the registry: want none added", b, a)
	}
	// Run again, as at the next start: nothing is left to end or count.
	if err := m.ReleaseArchived(t.Context(), m.asIs()); err != nil {
		t.Fatal(err)
	}
	check(t, snapshot(t, mr), []want{{"starfix.claims", []string{"event", "release", "reason", "archived"}, 3}})
}

// The names the daemon's own commands connect under are held to the
// protocol as their use requires: the upgrade's probe, which sends no
// request, is welcomed whatever it speaks, and `starfixd status`, which
// sends one, only when it speaks this daemon's protocol.
func TestStatusPrincipalSpeaksTheProtocol(t *testing.T) {
	tel, mr := measured(t, true)
	s := newServerTel(t, Limits{}, tel)
	hello := func(principal string, p int) *proto.Frame {
		t.Helper()
		srv, cli := net.Pipe()
		c := &conn{t: t, enc: proto.NewEncoder(cli), dec: proto.NewDecoder(cli), nc: cli}
		t.Cleanup(c.hangUp)
		c.wg.Go(func() { s.handle(t.Context(), srv); _ = srv.Close() })
		c.wg.Go(func() {
			_ = c.enc.Encode(&proto.Frame{T: proto.FrameBridge, Principal: principal})
			_ = c.enc.Encode(&proto.Frame{T: proto.FrameHello, Proto: p, Project: project})
		})
		return c.read()
	}
	for _, tc := range []struct {
		name      string
		principal string
		proto     int
		code      proto.Code // empty: welcomed
	}{
		{"status, this protocol", StatusPrincipal, proto.Proto, ""},
		{"status, an older protocol", StatusPrincipal, proto.ProtoMin - 1, proto.CodeVersion},
		{"status, a newer protocol", StatusPrincipal, proto.ProtoMax + 1, proto.CodeVersion},
		{"status, no protocol", StatusPrincipal, 0, proto.CodeVersion},
		{"the probe, an older protocol", ProbePrincipal, proto.ProtoMin - 1, ""},
		{"the probe, a newer protocol", ProbePrincipal, proto.ProtoMax + 1, ""},
	} {
		w := hello(tc.principal, tc.proto)
		switch {
		case tc.code == "" && w.Err != nil:
			t.Errorf("%s: refused: %v", tc.name, w.Err)
		case tc.code != "" && (w.Err == nil || w.Err.Code != tc.code || w.Err.Fix == ""):
			t.Errorf("%s: welcome error %v, want %s with a fix", tc.name, w.Err, tc.code)
		}
	}
	// Every other reserved name is still refused before the hello.
	for _, name := range store.ReservedPrincipals {
		if name == StatusPrincipal || name == ProbePrincipal {
			continue
		}
		srv, cli := net.Pipe()
		done := make(chan struct{})
		go func() { defer close(done); s.handle(t.Context(), srv); _ = srv.Close() }()
		if err := proto.NewEncoder(cli).Encode(&proto.Frame{T: proto.FrameBridge, Principal: name}); err != nil {
			t.Fatal(err)
		}
		if f, err := proto.NewDecoder(cli).Decode(); err == nil {
			t.Errorf("a bridge frame as %s was answered with a %q frame, want the connection closed", name, f.T)
		}
		_ = cli.Close()
		<-done
	}
	check(t, snapshot(t, mr), []want{
		{"starfix.handshakes", []string{"result", "ok"}, 3},
		{"starfix.handshakes", []string{"result", "version"}, 3},
		{"starfix.handshakes", []string{"result", "failed"}, float64(len(store.ReservedPrincipals) - 2)},
	})
}

// The openings a daemon makes before it listens, which are where an
// upgrade's migrations run, are measured as any opening is.
func TestMetricsOfOpenProjects(t *testing.T) {
	tel, mr := measured(t, true)
	m := newMultiTel(t, Limits{}, nil, tel, dana.Principal)
	m.project(t, projA, "alice")
	m.project(t, projB, "alice")
	if err := m.OpenProjects(t.Context()); err != nil {
		t.Fatal(err)
	}
	check(t, snapshot(t, mr), []want{
		{"starfix.store.open.duration", []string{"migrated", "true"}, 2},
		{"starfix.store.open.duration", []string{"migrated", "false"}, 0},
		{"starfix.stores.open", nil, 0},
	})
	if err := m.OpenProjects(t.Context()); err != nil {
		t.Fatal(err)
	}
	check(t, snapshot(t, mr), []want{
		{"starfix.store.open.duration", []string{"migrated", "true"}, 2},
		{"starfix.store.open.duration", []string{"migrated", "false"}, 2},
	})
}
