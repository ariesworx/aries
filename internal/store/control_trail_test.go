package store

import (
	"errors"
	"slices"
	"testing"
)

// TestControlTrail pins the reads that say what one principal left
// behind in the control store: the keys it added, the principals it
// created and the memberships it added (security review of stage 3a,
// F-2). Disabling an account ends none of them, so an admin has to be
// able to find them.
func TestControlTrail(t *testing.T) {
	t.Parallel()
	c := accessWorld(t) // everything in it is dana's work, but dana's own key, which sam added
	ctx := t.Context()
	hals, _ := testKey(t, "hal-from-alice")
	planted, err := c.AddKey(ctx, alice, "hal", hals) // hal is in a only: within alice's reach
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddPrincipal(ctx, alice, "gina", projA); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetMember(ctx, erin, "bob", projB, RoleMember); err != nil {
		t.Fatal(err)
	}

	keysOf := func(by string) []string {
		t.Helper()
		ks, err := c.KeysAddedBy(ctx, by)
		if err != nil {
			t.Fatalf("KeysAddedBy(%s): %v", by, err)
		}
		var out []string
		for _, k := range ks {
			if k.AddedBy != by || k.Key == "" || k.AddedAt.IsZero() {
				t.Errorf("KeysAddedBy(%s) holds %+v", by, k)
			}
			out = append(out, k.Principal)
		}
		slices.Sort(out)
		return out
	}
	created := func(by string) []string {
		t.Helper()
		ps, err := c.PrincipalsCreatedBy(ctx, by)
		if err != nil {
			t.Fatalf("PrincipalsCreatedBy(%s): %v", by, err)
		}
		var out []string
		for _, p := range ps {
			if p.CreatedBy != by {
				t.Errorf("PrincipalsCreatedBy(%s) holds %+v", by, p)
			}
			out = append(out, p.Name)
		}
		return out
	}
	added := func(by string) []string {
		t.Helper()
		ms, err := c.MembersAddedBy(ctx, by)
		if err != nil {
			t.Fatalf("MembersAddedBy(%s): %v", by, err)
		}
		var out []string
		for _, m := range ms {
			if m.AddedBy != by {
				t.Errorf("MembersAddedBy(%s) holds %+v", by, m)
			}
			out = append(out, m.Principal+" in "+m.Project[:1])
		}
		slices.Sort(out)
		return out
	}

	tests := []struct {
		by                   string
		keys, made, memberOf []string
	}{
		{"alice", []string{"hal"}, []string{"gina"}, []string{"gina in a"}},
		{"erin", nil, nil, []string{"bob in b"}},
		{"sam", []string{"dana"}, nil, nil},
		{"dana", []string{"bob", "carol", "frank", "hal"}, []string{"alice", "bob", "carol", "dana", "erin", "frank", "hal", "ivy"},
			[]string{"alice in a", "bob in a", "carol in a", "carol in b", "dana in a", "erin in b", "hal in a", "ivy in a", "ivy in b"}},
		{"nobody", nil, nil, nil},
		// What `ariesd control init` wrote is under the operator's name.
		{OperatorPrincipal, nil, nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.by, func(t *testing.T) {
			if got := keysOf(tc.by); !slices.Equal(got, tc.keys) {
				t.Errorf("KeysAddedBy(%s) are keys of %v, want %v", tc.by, got, tc.keys)
			}
			if got := created(tc.by); !slices.Equal(got, tc.made) {
				t.Errorf("PrincipalsCreatedBy(%s) = %v, want %v, by name", tc.by, got, tc.made)
			}
			if got := added(tc.by); !slices.Equal(got, tc.memberOf) {
				t.Errorf("MembersAddedBy(%s) = %v, want %v", tc.by, got, tc.memberOf)
			}
		})
	}
	// A key that was removed is no longer one that signs in, and is not
	// listed.
	if _, err := c.RemoveKey(ctx, dana, planted.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if got := keysOf("alice"); got != nil {
		t.Errorf("KeysAddedBy(alice) after her key was removed = %v, want none", got)
	}
	for _, bad := range []string{"", "Not A Name", "a\nb"} {
		if _, err := c.KeysAddedBy(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("KeysAddedBy(%q) = %v, want ErrInvalid", bad, err)
		}
		if _, err := c.PrincipalsCreatedBy(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("PrincipalsCreatedBy(%q) = %v, want ErrInvalid", bad, err)
		}
		if _, err := c.MembersAddedBy(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("MembersAddedBy(%q) = %v, want ErrInvalid", bad, err)
		}
	}
}
