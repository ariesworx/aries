package e2e

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/cli"
	"github.com/ariesworx/starfix/internal/client"
)

// TestWhoami runs `sfx whoami` through SSH and the daemon: it names the
// principal the server took the connection for and the key that signed,
// in text and with --json, and follows STARFIX_KEY to another identity
// (design §14, decision 35).
func TestWhoami(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	alice.env = map[string]string{"STARFIX_SESSION": "s-alice"}
	aliceKey := filepath.Join(alice.repo, "id_ed25519")

	want := fmt.Sprintf("principal  alice\nproject    %s\nserver     starfix@127.0.0.1:%d, host key %s\nsession    s-alice\nkey        %s, from the file %s, which key: in .starfix.yaml names\n",
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
		got.Key != (client.KeyInfo{Fingerprint: alice.fingerprint(), Source: "file", Path: aliceKey, By: "key: in .starfix.yaml"}) {
		t.Errorf("sfx whoami --json = %+v", got)
	}

	// In alice's checkout, with STARFIX_KEY naming another key, sfx is
	// whoever that key is: here bob. The checkout's own key: is not
	// offered.
	bobKey := filepath.Join(bob.repo, "id_ed25519")
	alice.env = map[string]string{"STARFIX_SESSION": "s-alice", client.EnvKeyFile: bobKey}
	got = decode[doc](t, alice.ok("whoami", "--json"))
	if got.Principal != "bob" || got.Key != (client.KeyInfo{Fingerprint: bob.fingerprint(), Source: "file", Path: bobKey, By: client.EnvKeyFile}) {
		t.Errorf("sfx whoami --json with %s naming bob's key = %+v, want bob and his key", client.EnvKeyFile, got)
	}

	// prime, which an agent reads, names the principal and the key too.
	alice.env = map[string]string{"STARFIX_SESSION": "s-alice"}
	if got := alice.ok("prime"); !strings.Contains(got, "you are alice, session s-alice\nsigned in with key "+alice.fingerprint()+" from "+aliceKey+"\n") {
		t.Errorf("sfx prime does not say who and by which key:\n%s", got)
	}

	// STARFIX_KEY is a name some deployments gave a secret that holds a
	// private key's text. sfx never reads it: set to a key, nothing any
	// command prints holds any of it, whether the command succeeds, is
	// refused by the server or cannot sign in, in text or with --json.
	const material = "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW\nQyNTUxOQAAACBzZWNyZXQtc2VjcmV0LXNlY3JldC1zZWNyZXQtc2VjcmV0AAAA\n-----END OPENSSH PRIVATE KEY-----\n"
	stranger := w.newUser("", "") // a key the server does not know
	for _, u := range []*user{alice, stranger} {
		for _, env := range []map[string]string{{client.EnvNotRead: material}, {client.EnvKeyFile: material}} {
			u.env = env
			for _, args := range [][]string{{"whoami"}, {"whoami", "--json"}, {"ready"}, {"prime"}, {"show", "sf-nosuchid", "--json"}, {"admin", "key", "list", "alice"}} {
				r := u.run("v0.2.0", args...)
				for _, part := range []string{"b3BlbnNzaC1rZXktdjE", "QyNTUxOQAAACBz", "PRIVATE KEY"} {
					if strings.Contains(r.stdout+r.stderr, part) {
						t.Errorf("sfx %s with %v holding a private key printed part of it (%q):\n%s%s", strings.Join(args, " "), slices.Collect(maps.Keys(env)), part, r.stdout, r.stderr)
					}
				}
				if _, bad := env[client.EnvKeyFile]; bad && (r.code != cli.ExitFailure || !strings.Contains(r.stdout+r.stderr, "never the key itself")) {
					t.Errorf("sfx %s with %s holding a key: exit %d, want it refused with a fix that says the variable takes a path\n%s%s",
						strings.Join(args, " "), client.EnvKeyFile, r.code, r.stdout, r.stderr)
				}
			}
		}
		u.env = nil
	}
	alice.env = map[string]string{"STARFIX_SESSION": "s-alice"}

	if r := alice.run("v0.2.0", "whoami", "now"); r.code != cli.ExitUsage {
		t.Errorf("sfx whoami now: exit %d, want a usage error\n%s", r.code, r.stderr)
	}
}
