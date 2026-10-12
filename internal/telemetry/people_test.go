package telemetry

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/proto"
)

var (
	principalStandIn = regexp.MustCompile(`^p_[0-9a-f]{16}$`)
	sessionStandIn   = regexp.MustCompile(`^s_[0-9a-f]{16}$`)
)

// who returns the principal and session a span carries after SetWho.
func who(t *testing.T, tel *Telemetry, m *Manual, principal, session string) (string, string) {
	t.Helper()
	m.ResetSpans()
	_, sp := tel.StartRequest(t.Context(), "show", proto.TraceContext{})
	sp.SetWho(principal, session)
	sp.End()
	got := one(t, m.Spans(), "show")
	return got.Attrs["principal"], got.Attrs["session"]
}

// telemetry: principals: decides how a person appears on a span.
func TestPrincipalsSetting(t *testing.T) {
	t.Run("pseudonym is the default", func(t *testing.T) {
		for _, mode := range []string{"", PrincipalsPseudonym} {
			tel, m := traced(t, Config{Principals: mode})
			p, s := who(t, tel, m, "alice", "s-abcdefgh")
			if !principalStandIn.MatchString(p) || !sessionStandIn.MatchString(s) {
				t.Fatalf("principals %q: span carries principal %q and session %q; want p_ and s_ with 16 hex digits each", mode, p, s)
			}
			if p2, s2 := who(t, tel, m, "alice", "s-abcdefgh"); p2 != p || s2 != s {
				t.Errorf("the same person is %q, %q and then %q, %q: a stand-in must be stable", p, s, p2, s2)
			}
			if p2, s2 := who(t, tel, m, "bob", "s-other"); p2 == p || s2 == s {
				t.Errorf("bob's stand-ins are alice's: %q, %q", p2, s2)
			}
			// A session whose id is a principal's name is not that
			// principal's stand-in with another prefix.
			if _, s2 := who(t, tel, m, "alice", "alice"); s2[2:] == p[2:] {
				t.Errorf("the session %q and the principal %q share their digits", s2, p)
			}
			if got, ok := tel.StandIn("alice"); !ok || got != p {
				t.Errorf("StandIn(alice) = %q, %t; want the span's %q", got, ok, p)
			}
		}
	})
	t.Run("name", func(t *testing.T) {
		tel, m := traced(t, Config{Principals: PrincipalsName})
		if p, s := who(t, tel, m, "alice", "s-abcdefgh"); p != "alice" || s != "s-abcdefgh" {
			t.Errorf("span carries principal %q and session %q, want the names", p, s)
		}
		// A session id is the client's text. One that is not a plain id
		// is left out rather than sent as it came.
		for _, session := range []string{"two words", "line\nbreak", "café", strings.Repeat("x", 256), "\x1b[2K"} {
			if p, s := who(t, tel, m, "alice", session); p != "alice" || s != "" {
				t.Errorf("session %q: span carries principal %q and session %q, want the principal and no session", session, p, s)
			}
		}
	})
	t.Run("omit", func(t *testing.T) {
		tel, m := traced(t, Config{Principals: PrincipalsOmit})
		if p, s := who(t, tel, m, "alice", "s-abcdefgh"); p != "" || s != "" {
			t.Errorf("span carries principal %q and session %q, want neither", p, s)
		}
	})
	t.Run("anything else is refused", func(t *testing.T) {
		for _, mode := range []string{"hash", "Name", "none", "true"} {
			_, _, err := NewManual(Config{Principals: mode})
			if err == nil || !strings.Contains(err.Error(), "fix: ") || !strings.Contains(err.Error(), "pseudonym, name or omit") {
				t.Errorf("NewManual(principals %q) = %v, want a refusal naming the three values and a fix", mode, err)
			}
			if ValidPrincipals(mode) {
				t.Errorf("ValidPrincipals(%q) = true", mode)
			}
		}
	})
}

// The key the stand-ins are made with is generated on first use, kept in
// a 0600 file, and read back at the next start. Deleting it makes every
// earlier stand-in unlinkable: a new key gives different ones.
func TestStandInKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "principals.key")
	open := func(t *testing.T) *Telemetry {
		t.Helper()
		tel, _ := traced(t, Config{KeyFile: path})
		return tel
	}
	first := open(t)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the key file was not made: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("the key file is mode %04o, want 0600", fi.Mode().Perm())
	}
	text, err := os.ReadFile(path) //nolint:gosec // the test's own temporary file
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}\n$`).Match(text) {
		t.Errorf("the key file does not hold 64 hex digits and a newline (%d bytes)", len(text))
	}
	alice, _ := first.StandIn("alice")

	if again, _ := open(t).StandIn("alice"); again != alice {
		t.Errorf("after a restart alice is %q, was %q: the key must be read back", again, alice)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(text) { //nolint:gosec // the test's own temporary file
		t.Errorf("a restart rewrote the key file (err %v)", err)
	}

	// A Telemetry that traces nothing still reads the key, to resolve
	// what was made with it, and writes nothing.
	off, err := New(t.Context(), Config{KeyFile: path, Ops: testOps})
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"bob", "alice", "carol"}
	if name, ok := off.Whois(alice, slices.Values(names)); !ok || name != "alice" {
		t.Errorf("Whois(%s) with traces off = %q, %t; want alice", alice, name, ok)
	}
	if name, ok := off.Whois("p_0123456789abcdef", slices.Values(names)); ok {
		t.Errorf("Whois of a stand-in that is nobody's = %q, true", name)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	fresh := open(t)
	renamed, ok := fresh.StandIn("alice")
	if !ok || renamed == alice || !principalStandIn.MatchString(renamed) {
		t.Errorf("after the key file was deleted alice is %q (was %q), want another stand-in", renamed, alice)
	}
	if name, ok := fresh.Whois(alice, slices.Values(names)); ok {
		t.Errorf("the old stand-in still resolves, to %q, under a new key", name)
	}
	if name, ok := fresh.Whois(renamed, slices.Values(names)); !ok || name != "alice" {
		t.Errorf("Whois(the new stand-in) = %q, %t; want alice", name, ok)
	}
}

// A key file that cannot be trusted or made stops a start that needs
// it, with a fix, and never repeats the key.
func TestStandInKeyRefusals(t *testing.T) {
	const key = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, path string)
		want    string
	}{
		{"readable by others", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(key+"\n"), 0o644); err != nil { //nolint:gosec // the mode under test
				t.Fatal(err)
			}
		}, "chmod 600"},
		{"not a key", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(key[:40]+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "does not hold a key"},
		{"a directory", func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}, "not a regular file"},
		{"no directory to make it in", func(t *testing.T, path string) {
			if err := os.Remove(filepath.Dir(path)); err != nil {
				t.Fatal(err)
			}
		}, "ReadWritePaths"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "telemetry", "principals.key")
			if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			tc.prepare(t, path)
			_, _, err := NewManual(Config{KeyFile: path})
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "fix: ") {
				t.Fatalf("NewManual = %v, want a refusal containing %q and a fix", err, tc.want)
			}
			if strings.Contains(err.Error(), key[:40]) {
				t.Errorf("the refusal repeats the key: %v", err)
			}
			// With names in place of stand-ins no key is needed, and the
			// file is not looked at.
			log := &syncBuffer{}
			tel, _, err := NewManual(Config{KeyFile: path, Principals: PrincipalsName, Logger: logTo(log)})
			if err != nil {
				t.Fatalf("NewManual(principals name) = %v, want it to start: no key is needed", err)
			}
			_ = tel.Shutdown(t.Context())
			if strings.Contains(log.String(), key[:40]) {
				t.Errorf("the log repeats the key:\n%s", log)
			}
		})
	}
}

func TestValidStandIn(t *testing.T) {
	for s, want := range map[string]bool{
		"p_0123456789abcdef":  true,
		"p_0123456789ABCDEF":  false,
		"p_0123456789abcde":   false,
		"p_0123456789abcdef0": false,
		"s_0123456789abcdef":  false,
		"0123456789abcdef":    false,
		"p_0123456789abcdeg":  false,
		"alice":               false,
		"":                    false,
	} {
		if got := ValidStandIn(s); got != want {
			t.Errorf("ValidStandIn(%q) = %t, want %t", s, got, want)
		}
	}
}
