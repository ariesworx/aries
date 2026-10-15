//go:build unix

package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/pgtest"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/statustext"
	"github.com/ariesworx/aries/internal/store"
)

// The status report while Postgres does not answer. Postgres is stopped with
// SIGSTOP, which a test can only do where there are signals: it still
// holds its sockets, so a connection to it waits and is not refused, as
// with a Postgres that is swapping or stuck in its collector.

// statusBound is how long a status report may take whatever Postgres is
// doing: the bound on its reads, and a second for everything else.
const statusBound = statusReadTimeout + time.Second

// unanswered is what the report says of a figure whose read ran out of
// time.
const unanswered = "database did not answer in 2s"

// freeze stops d, every process of it, until thaw is called or the test
// ends, whichever is first: a stopped Postgres must not outlive the
// test. ping is a read of d, which freeze checks goes unanswered.
func freeze(t *testing.T, d *pgtest.Server, ping func(context.Context) error) (thaw func()) {
	t.Helper()
	thaw = sync.OnceFunc(func() {
		if err := d.Signal(syscall.SIGCONT); err != nil {
			t.Errorf("continue postgres: %v", err)
		}
	})
	t.Cleanup(thaw)
	if err := d.Signal(syscall.SIGSTOP); err != nil {
		t.Fatalf("stop postgres: %v", err)
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
			t.Fatal("postgres still answered a ping 5s after SIGSTOP")
		}
	}
}

// pace starts a wait of d, and returns late, which waits for it to end
// and reports how much longer than d it took on this machine. Something
// that must end at a deadline of d is held to its bound and late() more.
//
// The bounds here are on a deadline the server sets itself, and a
// machine that stops the test's process for seconds, or leaves it no
// processor, fires the server's timer as late as it fires this one: a
// report that took eight seconds for that, as one did under load, says
// nothing of the server. A server that waits for Postgres is late when this
// wait was not, and fails as before.
func pace(d time.Duration) (late func() time.Duration) {
	start := time.Now()
	ended := make(chan time.Duration, 1)
	time.AfterFunc(d, func() { ended <- time.Since(start) - d })
	return sync.OnceValue(func() time.Duration { return max(0, <-ended) })
}

// timed runs report and returns how long it took, and how late this
// machine was for a wait of statusReadTimeout begun with it (pace).
// Should the report take twice statusBound, Postgres is continued, so that a
// report that waits for Postgres fails the test instead of hanging it.
func timed(report func() (proto.StatusResult, *proto.Error), thaw func()) (res proto.StatusResult, perr *proto.Error, took, late time.Duration) {
	net := time.AfterFunc(2*statusBound, thaw)
	defer net.Stop()
	behind := pace(statusReadTimeout)
	start := time.Now()
	res, perr = report()
	return res, perr, time.Since(start), behind()
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

// With Postgres frozen, both ways to the report answer within the bound,
// however many stores are open: the sessions, the one figure read from
// the database, are unknown and say why, and everything the daemon
// holds in memory is still there.
func TestStatusWithPostgresFrozen(t *testing.T) {
	// Serial: bounds elapsed time (pace), which a test running beside it would stretch.
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
		{"ariesd status", local},
		{"aries admin status on an open connection", func() (proto.StatusResult, *proto.Error) {
			return ask[proto.StatusResult](d, proto.OpStatus, proto.StatusArgs{})
		}},
	}

	// The control database is in the same Postgres, and frozen with it.
	thaw := freeze(t, pgOf(t), m.ctl.Ping)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, perr, took, late := timed(tc.report, thaw)
			if perr != nil {
				t.Fatalf("status with Postgres frozen = %v, want a report", perr)
			}
			// Two stores are open: one after the other, their reads
			// would take twice statusReadTimeout.
			if took > statusBound+late {
				t.Errorf("status with Postgres frozen took %s, want at most %s, and the %s this machine was late by", took.Round(time.Millisecond), statusBound, late.Round(time.Millisecond))
			}
			if sv := res.Server; sv == nil || sv.Version != "v0.2.0" || sv.OpenStores != 2 || sv.Conns != 3 || sv.Projects["active"] != 2 || sv.KeysFile == nil {
				t.Errorf("status with Postgres frozen has the server as %s; want its version, 2 open stores, 3 connections, 2 active projects and the keys file", js(sv))
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
			for _, want := range []string{"ariesd v0.2.0, ", "stores 2 of 64, connections 3 of ", "\nproj-a ", "  unknown  ",
				"\nsessions: unknown for 2 projects (" + unanswered + ")"} {
				if !strings.Contains(text, want) {
					t.Errorf("the status with Postgres frozen lacks %q:\n%s", want, text)
				}
			}
			if !json.Valid([]byte(doc)) || strings.Count(doc, `"sessions_unknown":"`+unanswered+`"`) != 2 || strings.Contains(doc, `"sessions":`) {
				t.Errorf("the JSON with Postgres frozen = %s\nwant one document, with sessions_unknown for both projects and no sessions", doc)
			}
		})
	}

	thaw()
	waitUntil(t, "postgres answers again", func() bool {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		return m.ctl.Ping(ctx) == nil
	})
	res, perr := local()
	if perr != nil {
		t.Fatalf("status once Postgres answers again = %v", perr)
	}
	for name, want := range map[string]int{"proj-a": 2, "proj-b": 1} {
		if p := byName(res)[name]; p.Sessions == nil || *p.Sessions != want || p.SessionsUnknown != "" {
			t.Errorf("%s sessions once Postgres answers again = %s, unknown %q; want %d", name, js(p.Sessions), p.SessionsUnknown, want)
		}
	}
}

// `aries` connects anew for each command, so what an admin's `aries admin
// status` meets while Postgres does not answer is a new connection. To a
// project whose store is open it is welcomed and answered within the
// status bound. To one whose store is closed it is refused as quickly,
// told that the database does not answer and to run `ariesd status`
// on the server, and no store is opened. Any other request is refused
// when it runs out of time, as on an older connection, and once Postgres
// answers again the session is in who.
func TestNewConnectionWithPostgresFrozen(t *testing.T) {
	// Serial: bounds elapsed time (pace), which a test running beside it would stretch.
	// Long enough for a request once Postgres answers again on a busy
	// machine, and short enough to wait out here.
	const timeout = 4 * time.Second
	m := adminWorld(t, Limits{})
	m.cfg.RequestTimeout = timeout
	if !mustDialTo(t, m.Server, dana, projA).alive() { // project a's store is open, b's closed
		t.Fatal("project a's first connection does not answer")
	}
	// Another session of a's, as each command of `aries` outside a
	// harness is not, but as each test connection here must be.
	another := func(a store.Actor, suffix string) store.Actor {
		a.Session += suffix
		return a
	}

	thaw := freeze(t, pgOf(t), m.ctl.Ping)

	t.Run("status, store open", func(t *testing.T) {
		late, start := pace(statusReadTimeout), time.Now()
		c := mustDialTo(t, m.Server, another(dana, "-open"), projA)
		res, perr := ask[proto.StatusResult](c, proto.OpStatus, proto.StatusArgs{})
		took := time.Since(start)
		t.Logf("new connection, store open: status after %s", took.Round(time.Millisecond))
		if perr != nil {
			t.Fatalf("status on a new connection with Postgres frozen = %v, want a report", perr)
		}
		if took > statusBound+late() {
			t.Errorf("status on a new connection with Postgres frozen took %s, want at most %s, and the %s this machine was late by", took.Round(time.Millisecond), statusBound, late().Round(time.Millisecond))
		}
		ps := byName(res)
		if a, b := ps["proj-a"], ps["proj-b"]; res.Server == nil || !a.Open || a.SessionsUnknown != unanswered || b.Open {
			t.Errorf("status on a new connection with Postgres frozen = %s; want the server, proj-a open with its sessions unknown (%q), and proj-b closed", js(res), unanswered)
		}
	})

	t.Run("status, store closed", func(t *testing.T) {
		late, start := pace(statusReadTimeout), time.Now()
		w, _ := dialTo(t, m.Server, another(dana, "-closed"), projB)
		took := time.Since(start)
		t.Logf("new connection, store closed: refused after %s", took.Round(time.Millisecond))
		if took > statusBound+late() {
			t.Errorf("the answer to a hello for a closed store with Postgres frozen took %s, want at most %s, and the %s this machine was late by", took.Round(time.Millisecond), statusBound, late().Round(time.Millisecond))
		}
		if e := w.Err; e == nil || e.Code != proto.CodeUnavailable || !strings.Contains(e.Message, "database does not answer") ||
			!strings.Contains(e.Message, projB) || !strings.Contains(e.Fix, "`ariesd status` on the server") {
			t.Errorf("welcome for a closed store with Postgres frozen has error %+v; want unavailable, saying the database does not answer and naming the project, with a fix naming `ariesd status` on the server", e)
		}
		if n := m.opened(projB); n != 0 {
			t.Errorf("project b's store was opened %d times with Postgres frozen, want 0", n)
		}
		if got := m.OpenStores(); got != 1 {
			t.Errorf("open stores = %d, want 1: nothing is left of the refused opening", got)
		}
		if log := m.log.String(); !strings.Contains(log, "event=database.check_failed") {
			t.Errorf("the log does not say the check of the database failed:\n%s", log)
		}
	})

	t.Run("another request, store open", func(t *testing.T) {
		late, start := pace(timeout), time.Now()
		c := mustDialTo(t, m.Server, alice, projA)
		// "At once" is given the whole of statusBound: the welcome waits
		// for nothing, and a second of it is the machine's.
		if took := time.Since(start); took > statusBound {
			t.Errorf("the welcome with Postgres frozen took %s, want it at once", took.Round(time.Millisecond))
		}
		_, perr := ask[proto.ListResult](c, proto.OpList, proto.ListArgs{})
		took := time.Since(start)
		t.Logf("new connection, store open: list refused after %s, of a request timeout of %s", took.Round(time.Millisecond), timeout)
		if perr == nil || perr.Code != proto.CodeUnavailable || perr.Message != "the server timed out on list" || !strings.HasPrefix(perr.Fix, "retry") {
			t.Errorf("list on a new connection with Postgres frozen = %+v; want unavailable, timed out, with a fix to retry", perr)
		}
		if took < timeout || took > timeout+statusBound+late() {
			t.Errorf("list on a new connection with Postgres frozen was refused after %s; want the request timeout, %s, and at most %s more, and the %s this machine was late by", took.Round(time.Millisecond), timeout, statusBound, late().Round(time.Millisecond))
		}

		thaw()
		waitUntil(t, "postgres answers again", func() bool {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			return m.ctl.Ping(ctx) == nil
		})
		who := mustAsk[proto.WhoResult](c, proto.OpWho, proto.WhoArgs{})
		var sessions []string
		for _, g := range who.Agents {
			sessions = append(sessions, g.Principal+"/"+g.Session)
		}
		if want := alice.Principal + "/" + alice.Session; !slices.Contains(sessions, want) {
			t.Errorf("who once Postgres answers again lists %v, want %s among them", sessions, want)
		}
	})
}

// One store whose database does not answer costs the report its own
// figure and no other's.
func TestStatusOneStoreSlow(t *testing.T) {
	// Serial: bounds elapsed time (pace), which a test running beside it would stretch.
	// A Postgres of its own for proj-b, started before the server so that it
	// is stopped after the server's stores have closed. Not t.TempDir,
	// whose path can be too long for the server's socket.
	dir, err := os.MkdirTemp("", "sfz")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	slow, err := pgtest.Start(t.Context(), dir)
	if errors.Is(err, pgtest.ErrNoPostgres) {
		t.Skip("postgres is not on PATH: run scripts/install-postgres.sh to run the server tests")
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
	m.cfg.Open = func(ctx context.Context, p store.Project) (Store, error) {
		if p.ID != projB {
			return open(ctx, p)
		}
		st, err := store.Open(ctx, dsn, store.Options{Prefix: p.Prefix})
		theirs = st
		return st, err
	}
	mustDialTo(t, m.Server, alice, projA).alive()
	mustDialTo(t, m.Server, erin, projB).alive()

	thaw := freeze(t, slow, theirs.Ping)
	res, perr, took, late := timed(func() (proto.StatusResult, *proto.Error) {
		out, perr := m.localStatus(t.Context(), proto.StatusArgs{})
		res, _ := out.(proto.StatusResult)
		return res, perr
	}, thaw)
	if perr != nil {
		t.Fatalf("status with one store's Postgres frozen = %v, want a report", perr)
	}
	if took > statusBound+late {
		t.Errorf("status with one store's Postgres frozen took %s, want at most %s, and the %s this machine was late by", took.Round(time.Millisecond), statusBound, late.Round(time.Millisecond))
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
		t.Errorf("the status with one store's Postgres frozen lacks %q:\n%s", want, text)
	}
	thaw() // before the stores close: a store's last act is a commit
}
