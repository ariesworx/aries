package store

import (
	"errors"
	"slices"
	"testing"
)

// TestProposeMember pins decision 36 in the store: a new membership for
// a principal that has any key is not made until the keys are accepted.
// ProposeMember answers with them and writes nothing; SetMember, which
// accepts, adds. Who may is decided first, so a refusal shows no key.
func TestProposeMember(t *testing.T) {
	t.Parallel()
	c := newControl(t)
	ctx := t.Context()
	mustProject(t, c, projA)
	mustProject(t, c, projB)
	mustPrincipal(t, c, "alice", projA+"=admin")
	mustPrincipal(t, c, "erin", projB+"=admin")
	mustPrincipal(t, c, "victor", projA+"=member")
	mustPrincipal(t, c, "bob", projA+"=member") // no key
	mustPrincipal(t, c, "frank")                // in no project, with a key
	planted, _ := testKey(t, "alices-second")
	k1, err := c.AddKey(ctx, alice, "victor", planted) // within alice's reach
	if err != nil {
		t.Fatal(err)
	}
	k2 := mustKey(t, c, "victor", "victors-own")
	mustKey(t, c, "frank", "franks")

	fingerprints := func(ks []PrincipalKey) []string {
		var out []string
		for _, k := range ks {
			out = append(out, k.Fingerprint)
		}
		slices.Sort(out)
		return out
	}
	want := []string{k1.Fingerprint, k2.Fingerprint}
	slices.Sort(want)

	t.Run("a principal with keys is not added until they are accepted", func(t *testing.T) {
		for _, by := range []Actor{erin, dana} { // a project admin, and a server admin alike
			seq := controlSeq(t, c)
			_, err := c.ProposeMember(ctx, by, "victor", projB, RoleMember)
			ck, ok := errors.AsType[*CarriedKeysError](err)
			if !ok || !errors.Is(err, ErrConflict) {
				t.Fatalf("ProposeMember(victor, b) by %s = %v, want a *CarriedKeysError wrapping ErrConflict", by.Principal, err)
			}
			if got := fingerprints(ck.Keys); ck.Principal != "victor" || ck.Project != projB || !slices.Equal(got, want) {
				t.Errorf("ProposeMember(victor, b) by %s carries %v for %s in %s, want victor's keys %v", by.Principal, got, ck.Principal, ck.Project, want)
			}
			for _, k := range ck.Keys {
				if k.AddedBy == "" || k.AddedAt.IsZero() || k.Key == "" {
					t.Errorf("carried key %+v, want who added it, when, and the key", k)
				}
			}
			wantEvents(t, c, seq)
			if _, member, _ := c.Role(ctx, "victor", projB); member {
				t.Fatalf("victor is a member of b after ProposeMember by %s", by.Principal)
			}
			if got := c.Access("victor", projB).Role; got != "" {
				t.Errorf("Access(victor, b).Role = %q after ProposeMember, want none", got)
			}
		}
	})

	t.Run("who may is decided before any key is shown", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			by        Actor
			principal string
			project   string
		}{
			{"a plain member", bob, "victor", projA},
			{"an admin of another project", alice, "victor", projB},
			{"an unassigned principal someone else created", alice, "frank", projA},
		} {
			_, err := c.ProposeMember(ctx, tc.by, tc.principal, tc.project, RoleMember)
			if _, shown := errors.AsType[*CarriedKeysError](err); shown || !errors.Is(err, ErrForbidden) {
				t.Errorf("%s: ProposeMember(%s) by %s = %v, want ErrForbidden and no keys", tc.name, tc.principal, tc.by.Principal, err)
			}
		}
		if _, err := c.ProposeMember(ctx, erin, "nobody", projB, RoleMember); !errors.Is(err, ErrNotFound) {
			t.Errorf("ProposeMember(nobody) = %v, want ErrNotFound", err)
		}
	})

	t.Run("what needs no acceptance", func(t *testing.T) {
		m, err := c.ProposeMember(ctx, erin, "bob", projB, RoleMember)
		if err != nil || m.Principal != "bob" || m.Role != RoleMember {
			t.Errorf("ProposeMember(bob, b), who has no key = %+v, %v; want him added", m, err)
		}
		// A role change of someone already a member, keys or not.
		m, err = c.ProposeMember(ctx, alice, "victor", projA, RoleAdmin)
		if err != nil || m.Role != RoleAdmin {
			t.Errorf("ProposeMember(victor, a, admin), already a member = %+v, %v; want the role changed", m, err)
		}
		if m, err = c.ProposeMember(ctx, alice, "victor", projA, RoleAdmin); err != nil || m.Role != RoleAdmin {
			t.Errorf("ProposeMember(victor, a, admin) again = %+v, %v; want it unchanged", m, err)
		}
	})

	t.Run("accepted", func(t *testing.T) {
		seq := controlSeq(t, c)
		m, err := c.SetMember(ctx, erin, "victor", projB, RoleMember)
		if err != nil || m.Principal != "victor" || m.Project != projB {
			t.Fatalf("SetMember(victor, b) by erin = %+v, %v", m, err)
		}
		wantEvents(t, c, seq, ev{OpMemberSet, projB + "/victor", erin, `"role":"member"`})
	})
}
