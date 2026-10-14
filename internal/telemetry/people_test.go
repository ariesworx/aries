package telemetry

import (
	"context"
	"errors"
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
	t.Parallel()
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
	// Serial: telemetry.New sets the SDK's error handler, which is the process's (setSDKLogging).
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

// A key that cannot be read or made never stops a start (design §16:
// telemetry must not fail the server). While it is unavailable a span
// says nothing about who, exactly as with omit: no in-memory key, and
// never the name. The log says so once, at error level, with the cause
// on one short line and each fix on its own; the state says why; and
// once the cause is gone a retry starts the stand-ins and says so once.
func TestStandInKeyUnavailable(t *testing.T) {
	t.Parallel()
	const key = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, path string)
		// repair removes the cause, as the fix says to.
		repair  func(t *testing.T, path string)
		cause   string
		damaged bool
		fix     string
	}{
		{"no directory to make it in", func(t *testing.T, path string) {
			if err := os.Remove(filepath.Dir(path)); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, path string) {
			if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
		}, "no such file or directory", false, "ReadWritePaths="},
		{"a directory it may not write in", func(t *testing.T, path string) {
			if os.Geteuid() == 0 {
				t.Skip("root writes anywhere")
			}
			if err := os.Chmod(filepath.Dir(path), 0o500); err != nil { //nolint:gosec // the mode under test
				t.Fatal(err)
			}
		}, func(t *testing.T, path string) {
			if err := os.Chmod(filepath.Dir(path), 0o700); err != nil { //nolint:gosec // a directory
				t.Fatal(err)
			}
		}, "permission denied", false, "ReadWritePaths="},
		{"readable by others", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(key+"\n"), 0o644); err != nil { //nolint:gosec // the mode under test
				t.Fatal(err)
			}
		}, func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}, "others can read it", true, "delete it"},
		{"not a key", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(key[:40]+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}, "does not hold a key", true, "delete it"},
		{"a directory", func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}, "not a regular file", true, "move it away"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "telemetry", "principals.key")
			if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			tc.prepare(t, path)
			log := &syncBuffer{}
			tel, m, err := NewManual(Config{KeyFile: path, Logger: logTo(log), Ops: testOps})
			if err != nil {
				t.Fatalf("NewManual = %v; a key that is unavailable must not stop a start", err)
			}
			t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
			if p, s := who(t, tel, m, "alice", "s-abcdefgh"); p != "" || s != "" {
				t.Errorf("with the key unavailable a span carries principal %q and session %q, want nothing about who", p, s)
			}
			if got, ok := tel.StandIn("alice"); ok {
				t.Errorf("StandIn = %q with the key unavailable: no key may be made up", got)
			}
			st := tel.People()
			if st.Mode != PrincipalsPseudonym || st.StandIns || !strings.Contains(st.Cause, tc.cause) || st.Damaged != tc.damaged || st.KeyFile != path {
				t.Errorf("People() = %+v; want pseudonym, no stand-ins, a cause containing %q, damaged %t", st, tc.cause, tc.damaged)
			}
			out := log.String()
			if n := strings.Count(out, "level=ERROR msg=\"telemetry: spans say nothing about who"); n != 1 {
				t.Errorf("%d error lines saying spans say nothing about who, want 1:\n%s", n, out)
			}
			for _, want := range []string{tc.cause, "msg=cause event=telemetry.key_unavailable cause=", "msg=fix event=telemetry.key_unavailable fix=",
				tc.fix, "principals: omit", "OTEL_TRACES_EXPORTER=none"} {
				if !strings.Contains(out, want) {
					t.Errorf("the log lacks %q:\n%s", want, out)
				}
			}
			for line := range strings.Lines(out) {
				// Short enough for `systemctl status` to show whole. Measured
				// with the key where a server keeps it, so that the bound does
				// not move with the length of the temporary directory's name:
				// 262 bytes is what 320 allowed under Linux's /tmp.
				if n := len(strings.ReplaceAll(line, dir, "/var/lib/starfix")); n > 262 {
					t.Errorf("a log line of %d bytes with the key under /var/lib/starfix; the cause and each fix must be a short line of its own:\n%s", n, line)
				}
			}
			if strings.Contains(out, key[:40]) || strings.Contains(st.Cause, key[:40]) {
				t.Errorf("the log or the state repeats the key:\n%s", out)
			}

			// Looked at again and still unavailable: nothing more is said.
			tel.RetryKey()
			if n := strings.Count(log.String(), "spans say nothing about who"); n != 1 {
				t.Errorf("%d lines after a retry that found the same cause, want 1", n)
			}
			// The cause removed: the next look makes or reads the key.
			tc.repair(t, path)
			tel.RetryKey()
			tel.RetryKey()
			p, s := who(t, tel, m, "alice", "s-abcdefgh")
			if !principalStandIn.MatchString(p) || !sessionStandIn.MatchString(s) {
				t.Errorf("after the fix a span carries principal %q and session %q, want stand-ins", p, s)
			}
			if st := tel.People(); !st.StandIns || st.Cause != "" || st.Damaged {
				t.Errorf("People() after the fix = %+v, want stand-ins and no cause", st)
			}
			if n := strings.Count(log.String(), "spans name people by stand-ins from now on"); n != 1 {
				t.Errorf("%d lines saying the stand-ins began, want 1:\n%s", n, log)
			}
			if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
				t.Errorf("the key file after the fix: %v, %v; want mode 0600", fi, err)
			}
		})
	}
}

// With names in place of stand-ins, or nothing, no key is needed: a key
// file that could not be used is not looked at as a fault, and nothing
// is said of it at error level.
func TestNoKeyNeeded(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{PrincipalsName, PrincipalsOmit} {
		path := filepath.Join(t.TempDir(), "missing", "principals.key")
		log := &syncBuffer{}
		tel, m, err := NewManual(Config{KeyFile: path, Principals: mode, Logger: logTo(log), Ops: testOps})
		if err != nil {
			t.Fatalf("NewManual(principals %s) = %v", mode, err)
		}
		t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
		p, _ := who(t, tel, m, "alice", "s-abcdefgh")
		if (p == "alice") != (mode == PrincipalsName) {
			t.Errorf("principals %s: span carries principal %q", mode, p)
		}
		tel.RetryKey()
		if st := tel.People(); st.Mode != mode || st.StandIns || st.Cause != "" {
			t.Errorf("principals %s: People() = %+v, want no cause: no key is wanted", mode, st)
		}
		if strings.Contains(log.String(), "level=ERROR") {
			t.Errorf("principals %s: an error was logged:\n%s", mode, log)
		}
		if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
			t.Errorf("principals %s: the key's directory was made", mode)
		}
	}
}

// The daemon's Telemetry never makes a key up: with no file named for
// it, the key is unavailable, for the reason it was given.
func TestNoKeyFileNamed(t *testing.T) {
	// Serial: telemetry.New sets the SDK's error handler, which is the process's (setSDKLogging).
	log := &syncBuffer{}
	tel, err := New(t.Context(), Config{Logger: logTo(log), Ops: testOps, KeyFileErr: errors.New("the account has no home directory"),
		Getenv: env{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://127.0.0.1:1/v1/traces", "OTEL_BSP_SCHEDULE_DELAY": "60000"}.get})
	if err != nil {
		t.Fatalf("New = %v, want a start", err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	if _, ok := tel.StandIn("alice"); ok {
		t.Errorf("a stand-in was made with no key file")
	}
	if st := tel.People(); st.StandIns || !strings.Contains(st.Cause, "the account has no home directory") {
		t.Errorf("People() = %+v, want the reason no file is named", st)
	}
	if !strings.Contains(log.String(), "principals_key") {
		t.Errorf("the log does not say to set telemetry: principals_key:\n%s", log)
	}
}

func TestValidStandIn(t *testing.T) {
	t.Parallel()
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
