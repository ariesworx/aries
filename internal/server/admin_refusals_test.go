package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/proto"
)

// TestRefusalsDoNotSayWhatExists pins that what a project admin is
// refused does not tell them which principals and keys the server holds
// outside their projects (security review of stage 3a, F-6): a name no
// one has is refused as one out of reach is, and a key some principal
// has is refused the same way whoever has it, without saying that anyone
// does. A server admin, who may list everything, is still told which.
func TestRefusalsDoNotSayWhatExists(t *testing.T) {
	m := adminWorld(t, Limits{}) // carol, in a and b, has a key; frank is in no project
	ctx := t.Context()
	if _, err := m.ctl.AddPrincipal(ctx, dana, "hal", projA); err != nil { // within alice's reach
		t.Fatal(err)
	}
	for _, name := range []string{"hal", "erin"} {
		if _, err := m.ctl.AddKey(ctx, dana, name, keyLine(t, name)); err != nil {
			t.Fatal(err)
		}
	}
	a := mustDialTo(t, m.Server, alice, projA)
	// refusal is what c is told, with the name the request gave replaced,
	// so that two requests can be compared.
	refusal := func(t *testing.T, c *conn, op string, args any, name string) proto.Error {
		t.Helper()
		_, perr := ask[json.RawMessage](c, op, args)
		if perr == nil {
			t.Fatalf("%s %+v was allowed, want a refusal", op, args)
		}
		return proto.Error{Code: perr.Code, Message: strings.ReplaceAll(perr.Message, name, "NAME"), Fix: strings.ReplaceAll(perr.Fix, name, "NAME")}
	}
	names := func(op string, args func(name string) any) func(*testing.T) {
		return func(t *testing.T) {
			absent := refusal(t, a, op, args("zed"), "zed")
			if absent.Code != proto.CodeForbidden || absent.Fix == "" {
				t.Errorf("%s of a name no one has = %+v, want forbidden with a fix", op, absent)
			}
			for _, name := range []string{"frank", "carol", "erin"} { // in no project; in a and b; in b only
				if got := refusal(t, a, op, args(name), name); got != absent {
					t.Errorf("%s %s, who exists out of alice's reach = %+v\nof a name no one has = %+v\nwant the same refusal", op, name, got, absent)
				}
			}
		}
	}
	t.Run("key.add names", names(proto.OpKeyAdd, func(name string) any { return proto.KeyAddArgs{Name: name, Key: keyLine(t, "fresh")} }))
	t.Run("principal.disable names", names(proto.OpPrincipalDisable, func(name string) any { return proto.PrincipalArgs{Name: name} }))
	t.Run("principal.enable names", names(proto.OpPrincipalEnable, func(name string) any { return proto.PrincipalArgs{Name: name} }))

	t.Run("a server admin with and without a row", func(t *testing.T) {
		args := proto.KeyAddArgs{Name: "dana", Key: keyLine(t, "fresh")}
		before := refusal(t, a, proto.OpKeyAdd, args, "dana")
		if _, err := m.ctl.AddPrincipal(ctx, dana, "dana", ""); err != nil {
			t.Fatal(err)
		}
		if after := refusal(t, a, proto.OpKeyAdd, args, "dana"); after != before {
			t.Errorf("key.add dana once dana has a row = %+v\nbefore = %+v\nwant the same refusal", after, before)
		}
	})

	t.Run("key.add of a key someone has", func(t *testing.T) {
		elsewhere := refusal(t, a, proto.OpKeyAdd, proto.KeyAddArgs{Name: "bob", Key: keyLine(t, "carol")}, "bob") // carol is out of reach
		within := refusal(t, a, proto.OpKeyAdd, proto.KeyAddArgs{Name: "bob", Key: keyLine(t, "hal")}, "bob")      // hal is within it
		if elsewhere != within || elsewhere.Code != proto.CodeForbidden || elsewhere.Fix == "" {
			t.Errorf("key.add of a key of someone out of reach = %+v\nof someone within reach = %+v\nwant one forbidden refusal with a fix", elsewhere, within)
		}
		if said := elsewhere.Message + elsewhere.Fix; strings.Contains(said, "belongs to another") || strings.Contains(said, fingerprintOf(t, "carol")) {
			t.Errorf("the refusal says the key is someone's: %+v", elsewhere)
		}
		// A key that the operator's authorized_keys gives to another
		// principal (F-5) is refused in the same words: a project admin
		// cannot tell that file from the store, or either from the other.
		typ, blob, handFP, _ := testKey(t, "dana by hand")
		m.writeByHand(t, `restrict,command="/usr/local/bin/starfixd stdio --principal dana" `+typ+" "+blob+" dana@example.com\n")
		byHand := refusal(t, a, proto.OpKeyAdd, proto.KeyAddArgs{Name: "bob", Key: typ + " " + blob}, "bob")
		if byHand != elsewhere {
			t.Errorf("key.add of a key that authorized_keys gives to another principal = %+v\nof a key the store holds for one = %+v\nwant the same refusal", byHand, elsewhere)
		}
		if said := byHand.Message + byHand.Fix; strings.Contains(said, "authorized_keys") || strings.Contains(said, handFP) {
			t.Errorf("the refusal tells a project admin of the server's authorized_keys file: %+v", byHand)
		}
		if who, _, ok := m.ctl.AuthorizedKey(handFP); ok {
			t.Errorf("the store took the hand-maintained key, for %q", who)
		}
		// To a principal out of reach, a new key and someone's key are
		// refused alike.
		fresh := refusal(t, a, proto.OpKeyAdd, proto.KeyAddArgs{Name: "carol", Key: keyLine(t, "fresh")}, "carol")
		taken := refusal(t, a, proto.OpKeyAdd, proto.KeyAddArgs{Name: "carol", Key: keyLine(t, "erin")}, "carol")
		if fresh != taken {
			t.Errorf("key.add carol of a new key = %+v\nof erin's key = %+v\nwant the same refusal", fresh, taken)
		}
		withKey := refusal(t, a, proto.OpPrincipalAdd, proto.PrincipalAddArgs{Name: "gus", Keys: []string{keyLine(t, "carol")}}, "gus")
		if withKey.Code != proto.CodeForbidden || strings.Contains(withKey.Message, "belongs to another") {
			t.Errorf("principal.add with carol's key = %+v, want forbidden, not saying whose the key is", withKey)
		}
		if _, err := m.ctl.Principal(ctx, "gus"); err == nil {
			t.Error("the refused principal.add left gus behind")
		}
	})

	t.Run("a server admin is told", func(t *testing.T) {
		d := mustDialTo(t, m.Server, dana, projA)
		for _, tc := range []struct {
			name string
			op   string
			args any
			code proto.Code
			msg  string
		}{
			{"key.add to no one", proto.OpKeyAdd, proto.KeyAddArgs{Name: "zed", Key: keyLine(t, "fresh")}, proto.CodeNotFound, "principal zed not found"},
			{"disable no one", proto.OpPrincipalDisable, proto.PrincipalArgs{Name: "zed"}, proto.CodeNotFound, "principal zed not found"},
			{"enable no one", proto.OpPrincipalEnable, proto.PrincipalArgs{Name: "zed"}, proto.CodeNotFound, "principal zed not found"},
			{"key.add of carol's key", proto.OpKeyAdd, proto.KeyAddArgs{Name: "bob", Key: keyLine(t, "carol")}, proto.CodeExists,
				"key " + fingerprintOf(t, "carol") + " already belongs to another principal"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, perr := ask[json.RawMessage](d, tc.op, tc.args)
				if perr == nil || perr.Code != tc.code || perr.Message != tc.msg || perr.Fix == "" {
					t.Errorf("%s by a server admin = %+v, want %s: %q and a fix", tc.op, perr, tc.code, tc.msg)
				}
			})
		}
	})
}
