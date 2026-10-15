package e2e

import (
	"fmt"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/internal/cli"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/server"
	"github.com/ariesworx/aries/internal/store"
)

// TestAdminStatus runs `sfx admin status` through SSH and the daemon as
// each kind of caller, in text and as JSON.
func TestAdminStatus(t *testing.T) {
	t.Parallel()
	w := keysWorld(t, server.Limits{OpenStores: 7})
	const second = "9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"
	w.addProject(second, "second", "sb")
	dana := w.newUser("dana", "")
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	w.member("alice", project, store.RoleAdmin)
	w.member("bob", project, store.RoleMember)
	bob.ok("create", "an issue")
	bw := bob.watch()

	res := decode[proto.StatusResult](t, dana.ok("admin", "status", "--json"))
	if res.Server == nil || res.Server.Version != "v0.2.0" || res.Server.OpenStores != 1 || res.Server.OpenStoresMax != 7 ||
		res.Server.Projects["active"] != 2 || res.Server.Principals["active"] != 2 {
		t.Errorf("status --json for a server admin: server %+v; want v0.2.0, 1 of 7 stores, 2 active projects and 2 active principals", res.Server)
	}
	if len(res.Projects) != 2 || res.Projects[0].Name != "first" || res.Projects[1].Name != "second" {
		t.Fatalf("status --json lists %+v, want first and second", res.Projects)
	}
	if k := res.Server.KeysFile; k == nil || k.Path != w.keysFile || k.Keys != 0 || k.Failed || k.WrittenAt == nil {
		t.Errorf("status --json of the keys file = %+v; want %s, written, with no key of the store yet", k, w.keysFile)
	}
	// bob's watch and dana's own connection, both by authorized_keys.
	first, other := res.Projects[0], res.Projects[1]
	if !first.Open || first.Conns != 2 || first.FileConns != 2 || first.Sessions == nil || *first.Sessions < 2 || first.Writes == nil ||
		first.Writes.LastMinute < 1 || first.Writes.P50Micros <= 0 || first.LastUsed == nil {
		t.Errorf("status of the first project = %+v (writes %+v); want it open, with two connections by the file, sessions and writes", first, first.Writes)
	}
	if other.Open || other.Conns != 0 || other.Sessions != nil || other.Writes != nil {
		t.Errorf("status of a project no one has connected to = %+v; want a closed store", other)
	}

	text := dana.ok("admin", "status")
	if strings.Contains(text, "protocol") {
		t.Errorf("status names the protocol, which only a mismatch needs:\n%s", text)
	}
	for _, want := range []string{"starfixd v0.2.0, up ", "stores 1 of 7, connections 2 of 1024\nkeys file " + w.keysFile + ": 0 keys, written ",
		"projects: 2 active; principals: 2 active\n", "PROJECT  STATE   STORE   CONNS  BY HAND  SESSIONS  WRITES/MIN  P50",
		"\nsecond   active  closed  0      -        -         -           -", "\nBY HAND: connections sshd let in by the server's hand-maintained authorized_keys file"} {
		if !strings.Contains(text, want) {
			t.Errorf("status lacks %q:\n%s", want, text)
		}
	}
	if got := dana.ok("admin", "status", "-n", "1", "--after", "first"); !strings.Contains(got, "\nsecond ") || strings.Contains(got, "\nfirst ") {
		t.Errorf("status --after first:\n%s", got)
	}

	// A project admin sees their project and nothing of the server.
	own := decode[proto.StatusResult](t, alice.ok("admin", "status", "--json"))
	if own.Server != nil || len(own.Projects) != 1 || own.Projects[0].Name != "first" {
		t.Errorf("status for a project admin = %+v; want the first project alone, and no server", own)
	}
	if got := alice.ok("admin", "status"); strings.Contains(got, "starfixd") || strings.Contains(got, "second") || !strings.HasPrefix(got, "PROJECT ") {
		t.Errorf("status for a project admin:\n%s", got)
	}
	// Anyone else is refused, and told whom to ask.
	if e := bob.refused(proto.CodeForbidden, "admin", "status"); !strings.Contains(e.Message, "bob may not see the server's status") {
		t.Errorf("status for a member: %+v", e)
	}
	bw.stillOpen(t, "bob")

	// A server of one project has nothing to report.
	one := newWorld(t, daemonOpts{admins: []string{"dana"}})
	if e := one.newUser("dana", "").refused(proto.CodeInvalid, "admin", "status"); !strings.Contains(e.Message, "this server holds one project") {
		t.Errorf("status on a server of one project: %+v", e)
	}
}

// TestKeyStillInTheFile is the state `starfixd control init` leaves: a
// key in the store and in the authorized_keys file kept by hand. sshd
// reads the keys file starfixd writes first (decision 26), so the key
// signs in by the store's line, which names it: removing it from the
// store closes its connections. It does not keep the key out, because
// sshd then finds the other line, which names no key; `key list` and
// `key remove` say so, by the key, because the daemon reads that file.
// Once the line is deleted there, the store governs the key.
func TestKeyStillInTheFile(t *testing.T) {
	t.Parallel()
	w := keysWorld(t, server.Limits{})
	dana := w.newUser("dana", "")
	bob := w.newUser("bob", "") // a line of authorized_keys
	w.member("bob", project, store.RoleMember)
	fp := bob.fingerprint()
	dana.ok("admin", "key", "add", "bob", bob.pub()) // and now in the store, as an import leaves it

	byStore := bob.watch()
	const mark = "ALSO IN authorized_keys: the key is also a line of the server's hand-maintained authorized_keys file, which sshd reads after the store's keys; " +
		"`sfx admin key remove` closes the key's connections, and sshd still accepts it by that line until the line is deleted on the server\n"
	const count = "1 open connection of bob came in by a line of the server's hand-maintained authorized_keys file; removing a key from the store does not close it\n"
	// The connection came in by the store's line: nothing is counted as
	// let in by the other file.
	got := dana.ok("admin", "key", "list", "bob")
	if !strings.Contains(got, "  ALSO IN\n"+fp+"  ssh-ed25519  bob@example.com  dana  ") || !strings.HasSuffix(got, "  authorized_keys\n"+mark) {
		t.Errorf("key list of a key also in authorized_keys, with a connection the store's line let in:\n%s\nwant the key marked, what the mark means, and no connection counted as by hand", got)
	}
	ks := decode[proto.KeysResult](t, dana.ok("admin", "key", "list", "bob", "--json"))
	if len(ks.Keys) != 1 || !ks.Keys[0].InAuthorizedKeys || ks.FileConns != 0 {
		t.Errorf("key list --json: %+v, want the key marked and no connection by authorized_keys", ks)
	}

	// The removal closes the connection, and says that the key still
	// gets in.
	r := dana.run("v0.2.0", "admin", "key", "remove", fp)
	const warning = "sfx: warning: sshd will still accept key %s: the connections it had signed in were closed, but it is also a line of the server's authorized_keys file, " +
		"which starfixd never edits and sshd reads after starfixd's keys file. Its next connection comes in by that line, as the principal the line names, and names no key. " +
		"Ask the server's operator to delete the line (docs/server.md, Keys from the store); until then, disabling the principal that line names is what keeps the key out\n"
	if want := fmt.Sprintf(warning, fp); r.code != cli.ExitOK || r.stdout != "key "+fp+" removed from bob\n1 connection it had signed in was closed\n" || r.stderr != want {
		t.Errorf("key remove of a key also in authorized_keys: exit %d\nstdout %q\nstderr %q\nwant one connection closed and stderr %q", r.code, r.stdout, r.stderr, want)
	}
	byStore.closedByServer(t, "bob")
	bob.ok("list") // not revoked: sshd takes the line of authorized_keys
	byHand := bob.watch()
	if got := dana.ok("admin", "key", "list", "bob"); got != "no keys recorded for bob\n"+count {
		t.Errorf("key list once the key is out of the store, with a connection the other file let in: %q", got)
	}
	// In the store again, the key's next connection is the store's; the
	// one that came in by hand names no key, and a removal leaves it.
	dana.ok("admin", "key", "add", "bob", bob.pub())
	if got := decode[proto.KeyResult](t, dana.ok("admin", "key", "remove", fp, "--json")); !got.InAuthorizedKeys || got.Dropped != 0 ||
		!strings.HasPrefix(got.Warning, "sshd will still accept key "+fp) {
		t.Errorf("key remove --json: %+v, want the mark and the warning in the one document, and nothing closed", got)
	}

	// The operator deletes the line, as the warning asks. The key is the
	// store's alone: its connections name it, and its removal revokes it.
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(readFile(t, bob.pub())))
	if err != nil {
		t.Fatal(err)
	}
	w.byHand(pub, "")
	dana.ok("admin", "key", "add", "bob", bob.pub())
	if got := dana.ok("admin", "key", "list", "bob"); strings.Contains(got, "ALSO IN") {
		t.Errorf("key list still marks the key after its line was deleted from authorized_keys:\n%s", got)
	}
	byStore = bob.watch()
	r = dana.run("v0.2.0", "admin", "key", "remove", fp)
	if r.code != cli.ExitOK || r.stdout != "key "+fp+" removed from bob\n1 connection it had signed in was closed\n" || r.stderr != "" {
		t.Errorf("key remove once the line is deleted: exit %d\nstdout %q\nstderr %q\nwant the key removed, one connection closed and no warning", r.code, r.stdout, r.stderr)
	}
	byStore.closedByServer(t, "bob")
	if r := bob.run("v0.2.0", "list"); r.code != cli.ExitFailure || !strings.Contains(r.stderr, "send your public key") {
		t.Errorf("bob's next command, his key in neither file: exit %d, stderr %q; want an auth refusal", r.code, r.stderr)
	}
	// Through two removals: it names no key, and sshd had let it in
	// before its line went. Only disabling bob closes it.
	byHand.stillOpen(t, "bob")
}
