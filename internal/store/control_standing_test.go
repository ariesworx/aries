package store

import "testing"

// TestControlStanding pins what the daemon may decide about a principal
// from its bridge frame alone, before a hello names a project.
func TestControlStanding(t *testing.T) {
	c := newControl(t) // server admins dana and sam
	ctx := t.Context()
	mustProject(t, c, projA)
	mustPrincipal(t, c, "alice", projA+"=member")
	mustPrincipal(t, c, "frank") // in no project
	mustPrincipal(t, c, "gone", projA+"=member")
	mustPrincipal(t, c, "sam") // a server admin with a row
	for _, name := range []string{"gone", "sam"} {
		if _, err := c.DisablePrincipal(ctx, dana, name); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name            string
		principal       string
		known, disabled bool
	}{
		{"a member", "alice", true, false},
		{"in no project", "frank", true, false},
		{"disabled", "gone", true, true},
		{"a server admin with no row", "dana", true, false},
		{"a disabled server admin", "sam", true, true},
		{"no row", "ivy", false, false},
		{"not a name", "Not A Name", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if known, disabled := c.Standing(tc.principal); known != tc.known || disabled != tc.disabled {
				t.Errorf("Standing(%q) = known %v, disabled %v; want known %v, disabled %v", tc.principal, known, disabled, tc.known, tc.disabled)
			}
		})
	}
	// It follows the store's writes at once.
	if _, err := c.EnablePrincipal(ctx, dana, "gone"); err != nil {
		t.Fatal(err)
	}
	if known, disabled := c.Standing("gone"); !known || disabled {
		t.Errorf("Standing(gone) once enabled = known %v, disabled %v; want known and not disabled", known, disabled)
	}
}
