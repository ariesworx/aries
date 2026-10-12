package store

import "testing"

// TestControlAdministersProjectOf pins who may see a principal's keys as
// an admin of a project it belongs to (decision 36), read from memory.
func TestControlAdministersProjectOf(t *testing.T) {
	c := accessWorld(t)
	tests := []struct {
		actor, principal string
		want             bool
	}{
		{"alice", "bob", true},    // bob is in a, which alice administers
		{"alice", "carol", true},  // carol is in a too, though also in b
		{"erin", "carol", true},   // and erin administers b
		{"erin", "bob", false},    // bob is not in b
		{"alice", "frank", false}, // in no project
		{"alice", "erin", false},  // an admin of another project
		{"bob", "hal", false},     // a member administers nothing
		{"alice", "dana", true},   // a server admin who is a member of a
		{"alice", "sam", false},   // a server admin in no project
		{"alice", "nobody", false},
		{"nobody", "bob", false},
	}
	for _, tc := range tests {
		if got := c.AdministersProjectOf(tc.actor, tc.principal); got != tc.want {
			t.Errorf("AdministersProjectOf(%s, %s) = %t, want %t", tc.actor, tc.principal, got, tc.want)
		}
	}
	ctx := t.Context()
	// An archived project is served to no one, and its admin sees nothing
	// by it; a disabled admin administers nothing.
	if _, err := c.ArchiveProject(ctx, dana, projB); err != nil {
		t.Fatal(err)
	}
	if c.AdministersProjectOf("erin", "carol") {
		t.Error("AdministersProjectOf(erin, carol) with b archived = true, want false")
	}
	if _, err := c.DisablePrincipal(ctx, dana, "alice"); err != nil {
		t.Fatal(err)
	}
	if c.AdministersProjectOf("alice", "bob") {
		t.Error("AdministersProjectOf(alice, bob) with alice disabled = true, want false")
	}
}

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
