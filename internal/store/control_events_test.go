package store

import "testing"

// TestControlLastEvent pins the read `starfixd control init` decides by
// when it is run again: the newest event on a key, a membership or a
// principal says whether an admin removed it since.
func TestControlLastEvent(t *testing.T) {
	c := newControl(t)
	ctx := t.Context()
	mustProject(t, c, projA)
	mustPrincipal(t, c, "alice", projA+"=admin")
	mustPrincipal(t, c, "bob", projA+"=member")
	k := mustKey(t, c, "bob", "bob-1")
	membership := projA + "/bob"

	last := func(target string) (Op, string) {
		t.Helper()
		e, ok, err := c.LastEvent(ctx, target)
		if err != nil {
			t.Fatalf("LastEvent(%s): %v", target, err)
		}
		if !ok {
			return "", ""
		}
		if e.Target != target || e.At.IsZero() {
			t.Errorf("LastEvent(%s) = %+v, want an event on that target with its time", target, e)
		}
		return e.Op, e.Actor.Principal
	}
	want := func(target string, op Op, by string) {
		t.Helper()
		if gotOp, gotBy := last(target); gotOp != op || gotBy != by {
			t.Errorf("LastEvent(%s) = %s by %q, want %s by %q", target, gotOp, gotBy, op, by)
		}
	}
	want(k.Fingerprint, OpKeyAdd, "dana")
	want(membership, OpMemberSet, "dana")
	want("bob", OpPrincipalAdd, "dana")
	want("nobody", "", "")

	if _, err := c.RemoveKey(ctx, alice, k.Fingerprint); err != nil { // bob is within alice's reach
		t.Fatal(err)
	}
	if _, err := c.RemoveMember(ctx, alice, "bob", projA); err != nil {
		t.Fatal(err)
	}
	want(k.Fingerprint, OpKeyRemove, "alice")
	want(membership, OpMemberRemove, "alice")

	// A later add by an admin is the newest again.
	if _, err := c.SetMember(ctx, dana, "bob", projA, RoleMember); err != nil {
		t.Fatal(err)
	}
	mustKey(t, c, "bob", "bob-1")
	want(k.Fingerprint, OpKeyAdd, "dana")
	want(membership, OpMemberSet, "dana")

	if _, err := c.DisablePrincipal(ctx, dana, "bob"); err != nil {
		t.Fatal(err)
	}
	want("bob", OpPrincipalDisable, "dana")
}
