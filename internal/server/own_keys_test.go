package server

import (
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

// TestOwnKeyRemoval pins decision 38 in the daemon: a person removes a
// key of their own account, and never the one their connection signed
// in with, so that nobody locks themselves out by accident.
func TestOwnKeyRemoval(t *testing.T) {
	t.Parallel()
	m := newMultiAdmins(t, Limits{}, nil, "dana", "sam")
	m.project(t, projA, "alice=admin", "bob")
	ctx := t.Context()
	if _, err := m.ctl.AddPrincipal(ctx, sam, "dana", ""); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bob", "alice", "dana"} {
		for _, n := range []string{"-1", "-2", "-3"} {
			if _, err := m.ctl.AddKey(ctx, sam, name, keyLine(t, name+n)); err != nil {
				t.Fatal(err)
			}
		}
	}
	m.refresh(ctx)
	events := func() int {
		t.Helper()
		evs, err := m.ctl.Events(ctx, 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		return len(evs)
	}
	for _, who := range []store.Actor{bob, alice, dana} { // a member, a project admin, a server admin
		me := who.Principal
		t.Run(me, func(t *testing.T) {
			inUse, other, third := fingerprintOf(t, me+"-1"), fingerprintOf(t, me+"-2"), fingerprintOf(t, me+"-3")
			w, c, closed := dialKey(t, m.Server, who, projA, inUse)
			if closed || w.Err != nil {
				t.Fatalf("%s's handshake with their first key = %+v (closed %v)", me, w, closed)
			}
			second := store.Actor{Principal: me, Session: "s-other-" + me, Machine: "phone"}
			w2, viaOther, closed := dialKey(t, m.Server, second, projA, other)
			if closed || w2.Err != nil {
				t.Fatalf("%s's handshake with their second key = %+v (closed %v)", me, w2, closed)
			}

			// Adding one's own is refused as before.
			if _, perr := ask[proto.KeyResult](c, proto.OpKeyAdd, proto.KeyAddArgs{Name: me, Key: keyLine(t, me+"-new")}); perr == nil ||
				perr.Code != proto.CodeForbidden || !strings.Contains(perr.Message, "nobody adds a key to their own account") {
				t.Errorf("key.add %s by %s = %v, want forbidden: nobody adds a key to their own account", me, me, perr)
			}
			// The key this connection signed in with is not removed by it.
			before := events()
			_, perr := ask[proto.KeyResult](c, proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: inUse})
			wantFix := "sign in with another of your keys and remove this one from there, or ask an admin to; `sfx whoami` shows the key in use and `sfx admin key list " + me + "` the others"
			if perr == nil || perr.Code != proto.CodeInvalid || perr.Fix != wantFix ||
				perr.Message != "key "+inUse+" is the key this connection signed in with, and removing it here would lock "+me+" out" {
				t.Errorf("key.remove of the key in use by %s = %+v\nwant invalid, naming the fingerprint, and the fix %q", me, perr, wantFix)
			}
			if after := events(); after != before {
				t.Errorf("the refused removal recorded %d events, want none", after-before)
			}
			if p, _, ok := m.ctl.AuthorizedKey(inUse); !ok || p != me {
				t.Errorf("the key in use is no longer %s's after the refusal", me)
			}
			// Another of one's own keys is: its connections close, and this
			// one stays.
			res, perr := ask[proto.KeyResult](c, proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: other})
			if perr != nil || res.ID != other || res.Principal != me || res.Dropped != 1 || res.Warning != "" {
				t.Fatalf("key.remove of %s's other key by %s = %+v, %v; want it removed, 1 connection dropped and no warning", me, me, res, perr)
			}
			if !viaOther.dropped() {
				t.Errorf("the connection %s's removed key had signed in is still open", me)
			}
			if !c.alive() {
				t.Errorf("%s's own connection was closed by removing another of their keys", me)
			}
			if _, _, ok := m.ctl.AuthorizedKey(other); ok {
				t.Errorf("the store still holds the key %s removed", me)
			}
			// A connection that came in by the hand-maintained file names no
			// key, so the daemon cannot tell which it used: the removal is
			// allowed, and says that it could not check.
			byFile := mustDialTo(t, m.Server, store.Actor{Principal: me, Session: "s-file-" + me, Machine: "desk"}, projA)
			res, perr = ask[proto.KeyResult](byFile, proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: third})
			if perr != nil || res.ID != third || !strings.Contains(res.Warning, "starfixd could not check that key "+third+" is not the one this connection signed in with") {
				t.Errorf("key.remove of %s's own key on a connection that named no key = %+v, %v; want it removed, with a warning that it could not be checked", me, res, perr)
			}
		})
	}

	t.Run("a key that is not one's own", func(t *testing.T) {
		c := mustDialTo(t, m.Server, bob, projA)
		before := events()
		theirs, none := refusalOf(t, c, fingerprintOf(t, "alice-1")), refusalOf(t, c, fingerprintOf(t, "nobody"))
		// Each refusal names its own request, and is otherwise the same.
		if theirs.Req == "" || theirs.Req == none.Req {
			t.Errorf("the refusals carry the request ids %q and %q, want one each", theirs.Req, none.Req)
		}
		theirs.Req, none.Req = "", ""
		if theirs != none || theirs.Code != proto.CodeForbidden {
			t.Errorf("key.remove by bob of alice's key = %+v\nof a key no one has = %+v\nwant the same forbidden refusal", theirs, none)
		}
		if after := events(); after != before {
			t.Errorf("the refusals recorded %d events, want none", after-before)
		}
		// An admin with reach removes bob's key, the one he signs in with
		// included: it is not that admin's connection's.
		w, viaBob, closed := dialKey(t, m.Server, bob, projA, fingerprintOf(t, "bob-1"))
		if closed || w.Err != nil {
			t.Fatal("bob's handshake with his first key failed")
		}
		a := mustDialTo(t, m.Server, alice, projA)
		if res, perr := ask[proto.KeyResult](a, proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: fingerprintOf(t, "bob-1")}); perr != nil || res.Dropped != 1 {
			t.Errorf("key.remove of bob's key by alice, who has reach = %+v, %v; want it removed and his connection dropped", res, perr)
		}
		if !viaBob.dropped() {
			t.Error("bob's connection by the key an admin removed is still open")
		}
	})
}

// refusalOf is what c is told when it removes the key fp.
func refusalOf(t *testing.T, c *conn, fp string) proto.Error {
	t.Helper()
	_, perr := ask[proto.KeyResult](c, proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: fp})
	if perr == nil {
		t.Fatalf("key.remove %s was allowed, want a refusal", fp)
	}
	return *perr
}
