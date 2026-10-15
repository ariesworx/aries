package e2e

import (
	"fmt"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/internal/cli"
)

// TestHostKeyRotation is the operator's rotation of the server's host
// key as docs/server.md has it: the new key is added to .starfix.yaml
// beside the old one, the server's sshd is switched to it, and the old
// key is removed from the file. A checkout that pins both connects
// before and after the switch, and what it queued in between is sent,
// with nothing said of an old host key; a checkout that pins the old key
// alone is refused after the switch, naming the key it was offered.
func TestHostKeyRotation(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{})
	alice, bob, carol := w.newUser("alice", ""), w.newUser("bob", ""), w.newUser("carol", "")
	id := strings.TrimSpace(alice.ok("create", "Commented on across a rotation"))
	old, next := w.hostFpr, newHostKey(t)
	nextFpr := ssh.FingerprintSHA256(next.PublicKey())

	// 1. The new key is committed beside the old, and pulled.
	for _, u := range []*user{alice, bob} {
		u.configure("host_key: "+old, "host_keys: ["+old+", "+nextFpr+"]")
	}
	want := fmt.Sprintf("server     starfix@127.0.0.1:%d, host keys %s and %s\n", w.addr.Port, old, nextFpr)
	if got := alice.ok("whoami"); !strings.Contains(got, want) {
		t.Errorf("sfx whoami pinning two keys =\n%swant it to say\n%s", got, want)
	}
	type whoami struct {
		Server struct {
			HostKey  string   `json:"host_key"`
			HostKeys []string `json:"host_keys"`
		} `json:"server"`
	}
	doc := decode[whoami](t, alice.ok("whoami", "--json"))
	if doc.Server.HostKey != "" || len(doc.Server.HostKeys) != 2 || doc.Server.HostKeys[0] != old || doc.Server.HostKeys[1] != nextFpr {
		t.Errorf("sfx whoami --json pinning two keys has server %+v, want host_keys [%s %s] and no host_key", doc.Server, old, nextFpr)
	}

	// 2. While the server is away, alice queues a write; it comes back
	// presenting the new key.
	w.stop()
	alice.ok("comment", id, "queued before the switch")
	w.useHostKey(next)
	w.start()
	if r := alice.run("v0.2.0", "ready"); r.code != cli.ExitOK || r.stderr != "sfx: outbox: delivered 1\n" {
		t.Errorf("alice's first command after the switch = exit %d, stderr %q; want her queued write delivered", r.code, r.stderr)
	}
	r := carol.run("v0.2.0", "ready")
	if wantMsg := "host key mismatch for 127.0.0.1: the server presented " + nextFpr + ", but .starfix.yaml pins " + old; r.code != cli.ExitFailure ||
		!strings.Contains(r.stderr, wantMsg) || !strings.Contains(r.stderr, "server.host_keys") {
		t.Errorf("carol, pinning the old key alone, after the switch = exit %d, stderr:\n%s\nwant refused with %q", r.code, r.stderr, wantMsg)
	}

	// 3. The old key is removed from the file.
	alice.configure("host_keys: ["+old+", "+nextFpr+"]", "host_key: "+nextFpr)
	w.stop()
	alice.ok("comment", id, "queued after the old key was removed")
	w.start()
	if r := alice.run("v0.2.0", "ready"); r.code != cli.ExitOK || r.stderr != "sfx: outbox: delivered 1\n" {
		t.Errorf("alice's first command pinning the new key alone = exit %d, stderr %q; want her queued write delivered", r.code, r.stderr)
	}
	if out := alice.ok("outbox"); strings.Contains(out, "old host key") {
		t.Errorf("sfx outbox after the rotation speaks of an old host key:\n%s", out)
	}
	if n := bob.comments(id); n != 2 {
		t.Errorf("the issue has %d comments, want the 2 alice queued, each sent once", n)
	}
}
