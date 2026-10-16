package server

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
	"github.com/ariesworx/aries/internal/telemetry"
)

// adminWorldTel is adminWorld on a server measured by tel.
func adminWorldTel(t *testing.T, lim Limits, tel *telemetry.Telemetry) *multi {
	t.Helper()
	m := newMultiTel(t, lim, nil, tel, dana.Principal)
	m.project(t, projA, "alice=admin", "bob", "carol")
	m.project(t, projB, "erin=admin", "carol")
	if _, err := m.ctl.AddKey(t.Context(), dana, "carol", keyLine(t, "carol")); err != nil {
		t.Fatal(err)
	}
	m.refresh(t.Context())
	return m
}

// Each way the server ends a connection is counted under its reason,
// and a claim that ends with its holder's access under that reason too.
func TestMetricsOfDrops(t *testing.T) {
	t.Parallel()
	tel, mr := measured(t, true)
	m := adminWorldTel(t, Limits{}, tel)
	d := mustDialTo(t, m.Server, dana, projA)

	// A key removed: the connection it let in is closed.
	carols := fingerprintOf(t, "carol")
	w, keyed, closed := dialKey(t, m.Server, carol, projA, carols)
	if closed || w.Err != nil {
		t.Fatalf("carol's handshake by her key: closed %t, %+v", closed, w)
	}
	// The welcome is written before the server tracks the connection;
	// once a request is answered it does, and the removal finds it.
	mustAsk[proto.WhoResult](keyed, proto.OpWho, proto.WhoArgs{})
	if rm := mustAsk[proto.KeyResult](d, proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: carols}); rm.Dropped != 1 {
		t.Fatalf("key.remove dropped %d connections, want carol's 1", rm.Dropped)
	}
	if !keyed.dropped() {
		t.Fatal("carol's connection was not closed")
	}

	// A principal disabled: both of bob's connections, and his claim.
	b1, b2 := mustDialTo(t, m.Server, bob, projA), mustDialTo(t, m.Server, store.Actor{Principal: "bob", Session: "s-b2", Machine: "desk"}, projA)
	is := mustAsk[proto.CreateResult](b1, proto.OpCreate, proto.CreateArgs{Title: "bob's"})
	mustAsk[proto.StartResult](b1, proto.OpStart, proto.StartArgs{ID: is.ID})
	mustAsk[proto.WhoResult](b2, proto.OpWho, proto.WhoArgs{}) // tracked, as above
	mustAsk[proto.PrincipalResult](d, proto.OpPrincipalDisable, proto.PrincipalArgs{Name: "bob"})
	if !b1.dropped() || !b2.dropped() {
		t.Fatal("bob's connections were not closed")
	}

	// A project archived: everyone's connections to it, and their claims.
	e := mustDialTo(t, m.Server, erin, projB)
	mustAsk[proto.WhoResult](e, proto.OpWho, proto.WhoArgs{}) // tracked, as above
	c := mustDialTo(t, m.Server, carol, projB)
	held := mustAsk[proto.CreateResult](c, proto.OpCreate, proto.CreateArgs{Title: "carol's"})
	mustAsk[proto.StartResult](c, proto.OpStart, proto.StartArgs{ID: held.ID})
	mustAsk[proto.ProjectResult](d, proto.OpProjectArchive, proto.ProjectArgs{Project: projB})
	if !e.dropped() || !c.dropped() {
		t.Fatal("the connections to the archived project were not closed")
	}
	for _, conn := range []*conn{keyed, b1, b2, e, c} {
		conn.hangUp()
	}

	check(t, snapshot(t, mr), []want{
		{"aries.connections.dropped", []string{"reason", "key"}, 1},
		{"aries.connections.dropped", []string{"reason", "disabled"}, 2},
		{"aries.connections.dropped", []string{"reason", "archived"}, 2},
		{"aries.connections.dropped", []string{"reason", "membership"}, 0},
		{"aries.connections.dropped", []string{"reason", "other"}, 0},
		{"aries.claims", []string{"event", "take", "reason", "none"}, 2},
		{"aries.claims", []string{"event", "release", "reason", "disabled"}, 1},
		{"aries.claims", []string{"event", "release", "reason", "archived"}, 1},
		{"aries.claims", []string{"event", "release", "reason", "other"}, 0},
		{"aries.control.writes", []string{"op", "key.remove", "result", "ok"}, 1},
		{"aries.control.writes", []string{"op", "principal.disable", "result", "ok"}, 1},
		{"aries.control.writes", []string{"op", "project.archive", "result", "ok"}, 1},
		{"aries.connections", []string{"project", projA}, 1}, // dana's
		{"aries.connections", []string{"project", projB}, 0},
	})
}

// An idle connection the server closes is counted as such.
func TestMetricsOfIdleConnections(t *testing.T) {
	t.Parallel()
	tel, mr := measured(t, true)
	s := newServerTel(t, Limits{}, tel)
	// Once past is set, every read deadline the server sets is already
	// gone.
	var past atomic.Bool
	s.deadline = func() time.Time {
		if past.Load() {
			return time.Now().Add(-time.Hour)
		}
		return time.Now()
	}
	c := dialPipe(t, s, alice)
	mustAsk[proto.CreateResult](c, proto.OpCreate, proto.CreateArgs{Title: "work"})
	past.Store(true)
	// The server may already be waiting for a request under a deadline
	// set before past was: this one wakes it, and its next wait idles
	// out. If it had not set the deadline yet, it idles out at once and
	// never reads the request. Either way the note follows.
	c.send(proto.OpList, proto.ListArgs{})
	f := c.read()
	if f.T == proto.FrameRes && f.Err == nil {
		f = c.read()
	}
	if f.Err == nil || f.Err.Code != proto.CodeUnavailable {
		t.Fatalf("an idle connection got %+v, want the note that it idled out", f)
	}
	c.hangUp()
	check(t, snapshot(t, mr), []want{
		{"aries.connections.dropped", []string{"reason", "idle"}, 1},
		{"aries.connections", []string{"project", project}, 0},
	})
}

// A refusal at one of the store's caps that has an error of its own is
// counted under the limit's name.
func TestMetricsOfStoreLimits(t *testing.T) {
	t.Parallel()
	tel, mr := measured(t, true)
	lim := Limits{}
	lim.Control.Keys = 1
	lim.Prices = 1
	m := adminWorldTel(t, lim, tel)
	d := mustDialTo(t, m.Server, dana, projA)
	// carol has her one key already.
	if _, perr := ask[proto.KeyResult](d, proto.OpKeyAdd, proto.KeyAddArgs{Name: "carol", Key: keyLine(t, "carol-second")}); perr == nil || perr.Code != proto.CodeInvalid {
		t.Fatalf("a second key with keys_per_principal: 1 = %v, want invalid", perr)
	}
	price := func(model string) *proto.Error {
		_, perr := ask[proto.WriteResult](d, proto.OpPriceSet, proto.PriceSetArgs{Model: model, From: "2026-01-01", Rates: testRates})
		return perr
	}
	if perr := price("model-a"); perr != nil {
		t.Fatalf("the first price: %v", perr)
	}
	if perr := price("model-b"); perr == nil || perr.Code != proto.CodeInvalid {
		t.Fatalf("a second price with prices: 1 = %v, want invalid", perr)
	}
	check(t, snapshot(t, mr), []want{
		{"aries.limit.refusals", []string{"limit", "keys_per_principal"}, 1},
		{"aries.limit.refusals", []string{"limit", "prices_per_project"}, 1},
		{"aries.limit.refusals", []string{"limit", "other"}, 0},
		{"aries.control.writes", []string{"op", "key.add", "result", "invalid"}, 1},
	})
}
