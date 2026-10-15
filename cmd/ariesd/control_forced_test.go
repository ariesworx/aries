package main

import (
	"strings"
	"testing"
)

// TestControlInitReadsAForcedCommandAsStdioDoes pins that `control init`
// and `ariesd stdio` agree on whose a line is. The flag parser takes
// the last --principal of a command; the importer once took the first,
// so for such a line the store would have given the key to one
// principal while sshd signed it in as another (security review of
// stage 3a, a note read from the code).
func TestControlInitReadsAForcedCommandAsStdioDoes(t *testing.T) {
	t.Parallel()
	s := newSite(t, "")
	key := func(seed string) string { return mustKeyOnly(t, seed) }
	keys := writeKeys(t,
		forced(t, "dana", "dana-1"), // 1
		`restrict,command="/usr/local/bin/ariesd stdio --principal alice --principal bob" `+key("twice"),            // 2: stdio runs as bob
		`restrict,command="/usr/local/bin/ariesd stdio --principal=carol --socket /run/s.sock" `+key("with-socket"), // 3
		`restrict,command="/usr/local/bin/ariesd stdio --principal erin extra" `+key("extra"),                       // 4: stdio refuses an argument
		`restrict,command="/usr/local/bin/ariesd stdio --no-such-flag --principal frank" `+key("unknown-flag"),      // 5: and an unknown flag
	)
	got := s.run(t, "", controlCmd, "init", "--name", "acme", "--authorized-keys", keys)
	if got.err != nil {
		t.Fatalf("control init: %v\n%s", got.err, got.stderr)
	}
	c := s.control(t)
	for name, want := range map[string]bool{"bob": true, "carol": true, "alice": false, "erin": false, "frank": false} {
		_, err := c.Principal(t.Context(), name)
		if exists := err == nil; exists != want {
			t.Errorf("after control init, principal %s exists = %t, want %t (%v)", name, exists, want, err)
		}
	}
	for _, n := range []string{"4", "5"} {
		if want := "authorized_keys line " + n + ": no `ariesd stdio --principal NAME` forced command; skipped\n"; !strings.Contains(got.stderr, want) {
			t.Errorf("stderr has no %q:\n%s", want, got.stderr)
		}
	}
}
