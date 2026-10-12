package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/starfix/internal/server"
	"github.com/ariesworx/starfix/internal/store"
)

// keyArgs returns the key pubKey gives seed as sshd's %t and %k.
func keyArgs(t *testing.T, seed string) (typ, blob string) {
	t.Helper()
	sum := sha256.Sum256([]byte(seed))
	pub, err := ssh.NewPublicKey(ed25519.NewKeyFromSeed(sum[:]).Public())
	if err != nil {
		t.Fatal(err)
	}
	return pub.Type(), base64.StdEncoding.EncodeToString(pub.Marshal())
}

// TestKeysCommand runs `starfixd keys` as sshd does: one exact line for
// a key of an active principal, and for everything else nothing on
// standard output, with success all the same.
func TestKeysCommand(t *testing.T) {
	s := newSite(t, "")
	keys := writeKeys(t, forced(t, "alice", "alice-1"), forced(t, "bob", "bob-1"), forced(t, "dana", "dana-1"))
	if got := s.run(t, "", controlCmd, "init", "--name", "acme", "--authorized-keys", keys); got.err != nil {
		t.Fatalf("control init: %v\n%s", got.err, got.stderr)
	}
	// Before the daemon starts: it reads another process's writes only
	// every 30 seconds.
	dana := store.Actor{Principal: "dana", Session: "s", Machine: "m"}
	if _, err := s.control(t).DisablePrincipal(t.Context(), dana, "bob"); err != nil {
		t.Fatal(err)
	}
	sock := serveSite(t, s)
	typ, blob := keyArgs(t, "alice-1")
	_, fp := pubKey(t, "alice-1")

	got := s.run(t, "", keysCmd, "--socket", sock, typ, blob)
	want := `restrict,command="/usr/local/bin/starfixd stdio --principal alice --key ` + fp + `" ssh-ed25519 ` + blob + "\n"
	if got.err != nil || got.stdout != want || got.stderr != "" {
		t.Fatalf("starfixd keys for alice's key: error %v\nstdout %q\nwant   %q\nstderr %q", got.err, got.stdout, want, got.stderr)
	}
	// It is the line sshd reads: one key, restricted, with the forced
	// command that names the principal and the key.
	_, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(got.stdout))
	if wantOptions := []string{"restrict", `command="/usr/local/bin/starfixd stdio --principal alice --key ` + fp + `"`}; err != nil ||
		!slices.Equal(options, wantOptions) || len(rest) != 0 {
		t.Errorf("the line parses to options %q, rest %q, %v; want exactly %q", options, rest, err, wantOptions)
	}
	// And the forced command is one `control init` would read back.
	if p, ok := forcedPrincipal(options); !ok || p != "alice" {
		t.Errorf("forcedPrincipal of the line = %q, %v; want alice", p, ok)
	}
	if got := s.run(t, "", keysCmd, "--socket", sock, "--bridge", "/opt/starfix/bin/starfixd", typ, blob); got.err != nil ||
		!strings.HasPrefix(got.stdout, `restrict,command="/opt/starfix/bin/starfixd stdio --principal alice --key `) {
		t.Errorf("starfixd keys --bridge: error %v, stdout %q", got.err, got.stdout)
	}

	bobType, bobBlob := keyArgs(t, "bob-1")
	_, strangers := keyArgs(t, "a stranger")
	one := newSite(t, "")
	rewrite(t, one, "project: "+adopted+"\nadmins: [dana]\n")
	oneSock := serveSite(t, one)
	nothing := []struct {
		name string
		site *site
		args []string
	}{
		{"a key no one has", s, []string{"--socket", sock, typ, strangers}},
		{"a key of a disabled principal", s, []string{"--socket", sock, bobType, bobBlob}},
		{"no daemon", s, []string{"--socket", filepath.Join(filepath.Dir(sock), "none.sock"), typ, blob}},
		{"a server of one project", one, []string{"--socket", oneSock, typ, blob}},
		{"no arguments", s, []string{"--socket", sock}},
		{"one argument", s, []string{"--socket", sock, typ}},
		{"three arguments", s, []string{"--socket", sock, typ, blob, "alice@example.com"}},
		{"an unknown flag", s, []string{"--socket", sock, "--principal", "dana", typ, blob}},
		{"a type that is not the key's", s, []string{"--socket", sock, "ssh-rsa", blob}},
		{"a key that is no base64", s, []string{"--socket", sock, typ, `x" ssh-ed25519 ` + strangers}},
		{"a bridge path with a quote", s, []string{"--socket", sock, "--bridge", `/bin/sh" x`, typ, blob}},
	}
	for _, tc := range nothing {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.site.run(t, "", keysCmd, tc.args...)
			if got.err != nil || got.stdout != "" {
				t.Fatalf("starfixd keys = error %v, stdout %q; want success and nothing printed", got.err, got.stdout)
			}
			if !strings.HasPrefix(got.stderr, "starfixd keys: no line: ") || strings.Count(got.stderr, "\n") != 1 {
				t.Errorf("stderr %q, want one line saying why", got.stderr)
			}
			for _, secret := range []string{blob, strangers, bobBlob} {
				if strings.Contains(got.stderr, secret) {
					t.Errorf("stderr repeats the key: %q", got.stderr)
				}
			}
		})
	}
	// A config file that cannot be read gives nothing either.
	bad := adminEnv{stdin: strings.NewReader(""), stdout: &strings.Builder{}, stderr: &strings.Builder{}, getenv: func(string) string { return "" }}
	if err := keysCmd(t.Context(), bad, []string{"--config", filepath.Join(t.TempDir(), "none.yaml"), typ, blob}); err != nil ||
		bad.stdout.(*strings.Builder).Len() != 0 {
		t.Errorf("starfixd keys with no config file = %v, stdout %q; want success and nothing printed", err, bad.stdout)
	}
}

// TestKeysCommandThroughRun checks the command is wired into the
// program, and exits 0 for a key it has no line for.
func TestKeysCommandThroughRun(t *testing.T) {
	if _, err := os.Stat(server.DefaultConfigFile); err == nil {
		t.Skip("this machine has a starfixd config file, which the command would read")
	}
	if err := run(t.Context(), []string{"keys", "--socket", filepath.Join(t.TempDir(), "none.sock"), "ssh-ed25519", "AAAA"}); err != nil {
		t.Errorf("run(keys ...) = %v, want nil: sshd must fall back to authorized_keys", err)
	}
}
