//go:build unix

package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/dolttest"
	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/statustext"
	"github.com/ariesworx/starfix/internal/store"
)

// The status report while Dolt does not answer. Dolt is stopped with
// SIGSTOP, which a test can only do where there are signals: it still
// holds its sockets, so a connection to it waits and is not refused, as
// with a Dolt that is swapping or stuck in its collector.

// statusBound is how long a status report may take whatever Dolt is
// doing: the bound on its reads, and a second for everything else.
const statusBound = statusReadTimeout + time.Second

// unanswered is what the report says of a figure whose read ran out of
// time.
const unanswered = "database did not answer in 2s"

// freeze stops d, the whole process group, until thaw is called or the
// test ends, whichever is first: a stopped Dolt must not outlive the
// test. ping is a read of d, which freeze checks goes unanswered.
func freeze(t *testing.T, d *dolttest.Server, ping func(context.Context) error) (thaw func()) {
	t.Helper()
	pid := d.PID()
	thaw = sync.OnceFunc(func() {
		if err := syscall.Kill(-pid, syscall.SIGCONT); err != nil {
			t.Errorf("continue dolt (process group %d): %v", pid, err)
		}
	})
	t.Cleanup(thaw)
	if err := syscall.Kill(-pid, syscall.SIGSTOP); err != nil {
		t.Fatalf("stop dolt (process group %d): %v", pid, err)
	}
	// kill returns before every thread of the group has stopped, on
	// Linux by long enough for a ping to be answered: wait for one that
	// is not.
	for end := time.Now().Add(5 * time.Second); ; {
		ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
		err := ping(ctx)
		cancel()
		if err != nil {
			return thaw
		}
		if time.Now().After(end) {
			t.Fatal("dolt still answered a ping 5s after SIGSTOP")
		}
	}
}

// timed runs report and returns how long it took. Should it take twice
// statusBound, Dolt is continued, so that a report that waits for Dolt
// fails the test instead of hanging it.
func timed(report func() (proto.StatusResult, *proto.Error), thaw func()) (proto.StatusResult, *proto.Error, time.Duration) {
	net := time.AfterFunc(2*statusBound, thaw)
	defer net.Stop()
	start := time.Now()
	res, perr := report()
	return res, perr, time.Since(start)
}

// printed is a report as the two commands print it: the text, and the
// JSON of --json.
func printed(t *testing.T, res proto.StatusResult) (text, doc string) {
	t.Helper()
	var w strings.Builder
	statustext.Print(&w, res)
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	return w.String(), string(b)
}

// With Dolt frozen, both ways to the report answer within the bound,
// however many stores are open: the sessions, the one figure read from
// the database, are unknown and say why, and everything the daemon
// holds in memory is still there.
func TestStatusWithDoltFrozen(t *testing.T) {
	m := adminWorld(t, Limits{})
	d := mustDialTo(t, m.Server, dana, projA)
	a := mustDialTo(t, m.Server, alice, projA)
	mustDialTo(t, m.Server, erin, projB).alive()
	mustAsk[proto.CreateResult](a, proto.OpCreate, proto.CreateArgs{Title: "before the freeze"})
	local := func() (proto.StatusResult, *proto.Error) {
		out, perr := m.localStatus(t.Context(), proto.StatusArgs{})
		res, _ := out.(proto.StatusResult)
		return res, perr
	}
	tests := []struct {
		name   string
		report func() (proto.StatusResult, *proto.Error)
	}{
		{"starfixd status", local},
		{"sfx admin status on an open connection", func() (proto.StatusResult, *proto.Error) {
			return ask[proto.StatusResult](d, proto.OpStatus, proto.StatusArgs{})
		}},
	}

	// The control database is in the same Dolt, and frozen with it.
	thaw := freeze(t, dolt, m.ctl.Ping)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, perr, took := timed(tc.report, thaw)
			if perr != nil {
				t.Fatalf("status with Dolt frozen = %v, want a report", perr)
			}
			// Two stores are open: one after the other, their reads
			// would take twice statusReadTimeout.
			if took > statusBound {
				t.Errorf("status with Dolt frozen took %s, want at most %s", took.Round(time.Millisecond), statusBound)
			}
			if sv := res.Server; sv == nil || sv.Version != "v0.2.0" || sv.OpenStores != 2 || sv.Conns != 3 || sv.Projects["active"] != 2 || sv.KeysFile == nil {
				t.Errorf("status with Dolt frozen has the server as %s; want its version, 2 open stores, 3 connections, 2 active projects and the keys file", js(sv))
			}
			ps := byName(res)
			for name, conns := range map[string]int{"proj-a": 2, "proj-b": 1} {
				p := ps[name]
				if p.Sessions != nil || p.SessionsUnknown != unanswered {
					t.Errorf("%s sessions = %s, unknown %q; want no figure, and %q", name, js(p.Sessions), p.SessionsUnknown, unanswered)
				}
				if !p.Open || p.Conns != conns || p.Writes == nil || p.Writes.LastMinute == 0 || p.LastUsed == nil {
					t.Errorf("%s = %s; want it open with %d connections, its writes and when it was last used", name, js(p), conns)
				}
			}
			text, doc := printed(t, res)
			for _, want := range []string{"starfixd v0.2.0, ", "stores 2 of 64, connections 3 of ", "\nproj-a ", "  unknown  ",
				"\nsessions: unknown for 2 projects (" + unanswered + ")"} {
				if !strings.Contains(text, want) {
					t.Errorf("the status with Dolt frozen lacks %q:\n%s", want, text)
				}
			}
			if !json.Valid([]byte(doc)) || strings.Count(doc, `"sessions_unknown":"`+unanswered+`"`) != 2 || strings.Contains(doc, `"sessions":`) {
				t.Errorf("the JSON with Dolt frozen = %s\nwant one document, with sessions_unknown for both projects and no sessions", doc)
			}
		})
	}

	thaw()
	waitUntil(t, "dolt answers again", func() bool {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		return m.ctl.Ping(ctx) == nil
	})
	res, perr := local()
	if perr != nil {
		t.Fatalf("status once Dolt answers again = %v", perr)
	}
	for name, want := range map[string]int{"proj-a": 2, "proj-b": 1} {
		if p := byName(res)[name]; p.Sessions == nil || *p.Sessions != want || p.SessionsUnknown != "" {
			t.Errorf("%s sessions once Dolt answers again = %s, unknown %q; want %d", name, js(p.Sessions), p.SessionsUnknown, want)
		}
	}
}

// One store whose database does not answer costs the report its own
// figure and no other's.
func TestStatusOneStoreSlow(t *testing.T) {
	// A Dolt of its own for proj-b, started before the server so that it
	// is stopped after the server's stores have closed. Not t.TempDir,
	// whose path can be too long for dolt's socket.
	dir, err := os.MkdirTemp("", "sfz")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	slow, err := dolttest.Start(t.Context(), dir)
	if errors.Is(err, dolttest.ErrNoDolt) {
		t.Skip("dolt is not on PATH: install dolt to run the server tests")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := slow.Stop(); err != nil {
			t.Error(err)
		}
	})
	dsn, err := slow.NewDatabase(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	m := adminWorld(t, Limits{})
	var theirs *store.Store
	open := m.cfg.Open
	m.cfg.Open = func(ctx context.Context, p store.Project) (*store.Store, error) {
		if p.ID != projB {
			return open(ctx, p)
		}
		st, err := store.Open(ctx, dsn, store.Options{CommitInterval: -1, Prefix: p.Prefix})
		theirs = st
		return st, err
	}
	mustDialTo(t, m.Server, alice, projA).alive()
	mustDialTo(t, m.Server, erin, projB).alive()

	thaw := freeze(t, slow, theirs.Ping)
	res, perr, took := timed(func() (proto.StatusResult, *proto.Error) {
		out, perr := m.localStatus(t.Context(), proto.StatusArgs{})
		res, _ := out.(proto.StatusResult)
		return res, perr
	}, thaw)
	if perr != nil {
		t.Fatalf("status with one store's Dolt frozen = %v, want a report", perr)
	}
	if took > statusBound {
		t.Errorf("status with one store's Dolt frozen took %s, want at most %s", took.Round(time.Millisecond), statusBound)
	}
	ps := byName(res)
	if p := ps["proj-a"]; p.Sessions == nil || *p.Sessions != 1 || p.SessionsUnknown != "" {
		t.Errorf("proj-a sessions = %s, unknown %q; want 1, alice's: its own database answers", js(p.Sessions), p.SessionsUnknown)
	}
	if p := ps["proj-b"]; p.Sessions != nil || p.SessionsUnknown != unanswered || !p.Open || p.Writes == nil {
		t.Errorf("proj-b = %s; want it open with its writes, and its sessions unknown: %q", js(p), unanswered)
	}
	text, _ := printed(t, res)
	if want := "\nsessions: unknown for 1 project (" + unanswered + ")"; !strings.Contains(text, want) {
		t.Errorf("the status with one store's Dolt frozen lacks %q:\n%s", want, text)
	}
	thaw() // before the stores close: a store's last act is a commit
}
