package e2e

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/ariesworx/starfix/internal/cli"
	"github.com/ariesworx/starfix/internal/client"
)

// TestWhoami runs `sfx whoami` through SSH and the daemon: it names the
// principal the server took the connection for and the key that signed,
// in text and with --json, and follows STARFIX_KEY to another identity
// (design §14, decision 35).
func TestWhoami(t *testing.T) {
	w := newWorld(t, daemonOpts{})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	alice.env = map[string]string{"STARFIX_SESSION": "s-alice"}
	aliceKey := filepath.Join(alice.repo, "id_ed25519")

	want := fmt.Sprintf("principal  alice\nproject    %s\nserver     starfix@127.0.0.1:%d, host key %s\nsession    s-alice\nkey        %s, from the file %s\n",
		project, w.addr.Port, w.hostFpr, alice.fingerprint(), aliceKey)
	if got := alice.ok("whoami"); got != want {
		t.Errorf("sfx whoami =\n%swant\n%s", got, want)
	}
	type doc struct {
		Principal string `json:"principal"`
		Project   string `json:"project"`
		Server    struct {
			Host    string `json:"host"`
			Port    int    `json:"port"`
			User    string `json:"user"`
			HostKey string `json:"host_key"`
		} `json:"server"`
		Session string         `json:"session"`
		Key     client.KeyInfo `json:"key"`
	}
	got := decode[doc](t, alice.ok("whoami", "--json"))
	if got.Principal != "alice" || got.Project != project || got.Server.Host != "127.0.0.1" || got.Server.Port != w.addr.Port ||
		got.Server.User != "starfix" || got.Server.HostKey != w.hostFpr || got.Session != "s-alice" ||
		got.Key != (client.KeyInfo{Fingerprint: alice.fingerprint(), Source: "file", Path: aliceKey}) {
		t.Errorf("sfx whoami --json = %+v", got)
	}

	// In alice's checkout, with STARFIX_KEY naming another key, sfx is
	// whoever that key is: here bob. The checkout's own key: is not
	// offered.
	bobKey := filepath.Join(bob.repo, "id_ed25519")
	alice.env = map[string]string{"STARFIX_SESSION": "s-alice", client.EnvKey: bobKey}
	got = decode[doc](t, alice.ok("whoami", "--json"))
	if got.Principal != "bob" || got.Key != (client.KeyInfo{Fingerprint: bob.fingerprint(), Source: "file", Path: bobKey}) {
		t.Errorf("sfx whoami --json with %s naming bob's key = %+v, want bob and his key", client.EnvKey, got)
	}

	if r := alice.run("v0.2.0", "whoami", "now"); r.code != cli.ExitUsage {
		t.Errorf("sfx whoami now: exit %d, want a usage error\n%s", r.code, r.stderr)
	}
}
