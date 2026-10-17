package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
)

// A line of the hand-kept authorized_keys that sshd would only take
// under a condition the key proof cannot check (from=, the address it
// comes from; expiry-time, its end; cert-authority, the key is a CA's;
// verify-required, the FIDO key's user verification) forces the bridge
// for no one over HTTPS: dropping the condition would let in what sshd
// keeps out. Its key is refused as an unknown one. A line that names
// only the command, as the guide writes it, is still taken.
func TestHTTPSHandLineConditions(t *testing.T) {
	t.Parallel()
	line := func(owner, options string) string {
		typ, blob, _, _ := testKey(t, owner)
		return fmt.Sprintf(`%s,command="/usr/local/bin/ariesd stdio --principal %s" %s %s`, options, owner, typ, blob)
	}
	cases := []struct{ owner, options string }{
		{"carol", `from="10.0.0.0/8"`},
		{"erin", `restrict,expiry-time="20200101000000Z"`},
		{"frank", `cert-authority,principals="frank"`},
		{"grace", `verify-required`},
		{"heidi", `restrict,FROM="10.0.0.0/8"`},
	}
	lines := []string{bridgeLine(t, "alice", "alice")}
	for _, tc := range cases {
		lines = append(lines, line(tc.owner, tc.options))
	}
	cfg := Config{AuthorizedKeys: handFile(t, lines...)}
	u, pool := listenHTTPS(t, &cfg, nil)
	s, _ := newServerConfig(t, cfg, nil)
	serving(t, s)

	for _, tc := range cases {
		// In turn: the test holds a Postgres server (CONTRIBUTING.md).
		t.Run(tc.options, func(t *testing.T) {
			c, _, err := wsDial(t, u, pool, nil)
			if err != nil {
				t.Fatal(err)
			}
			f := prove(t, c, u, signerOf(t, tc.owner), nil)
			if f == nil {
				w := hello(t, c)
				t.Fatalf("a line with %s let %s in over HTTPS: %+v", tc.options, tc.owner, w)
			}
			if f.Err.Code != proto.CodeAuth || !strings.Contains(f.Err.Message, "none of the keys") {
				t.Errorf("a line with %s: refused %+v; want the unknown key's refusal", tc.options, f.Err)
			}
		})
	}
	c, _, err := wsDial(t, u, pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f := prove(t, c, u, signerOf(t, "alice"), nil); f != nil {
		t.Fatalf("alice's plain bridge line: %v", f.Err)
	}
	if w := hello(t, c); w.Err != nil || w.Principal != "alice" {
		t.Errorf("alice by her plain bridge line = %+v", w)
	}
}

func TestConditionalOption(t *testing.T) {
	t.Parallel()
	for o, want := range map[string]bool{
		`from="10.0.0.0/8"`: true, `From="*.example.com"`: true, `expiry-time="20261116170000Z"`: true,
		"cert-authority": true, "CERT-AUTHORITY": true, "verify-required": true,
		"restrict": false, "no-pty": false, `command="/usr/local/bin/ariesd stdio --principal alice"`: false,
		`principals="alice"`: false, "no-touch-required": false, `environment="FROM=x"`: false,
	} {
		if got := conditionalOption(o); got != want {
			t.Errorf("conditionalOption(%s) = %t, want %t", o, got, want)
		}
	}
}
