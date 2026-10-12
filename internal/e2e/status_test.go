package e2e

import (
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/cli"
	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/server"
	"github.com/ariesworx/starfix/internal/store"
)

// TestAdminStatus runs `sfx admin status` through SSH and the daemon as
// each kind of caller, in text and as JSON.
func TestAdminStatus(t *testing.T) {
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
	for _, want := range []string{"starfixd v0.2.0, protocol ", "stores 1 of 7, connections 2 of 1024, key lookups 0 of 64\n",
		"projects: 2 active; principals: 2 active\n", "PROJECT  STATE   STORE   CONNS  BY FILE  SESSIONS  WRITES/MIN  P50",
		"\nsecond   active  closed  0      -        -         -           -", "\nBY FILE: connections sshd let in by the server's authorized_keys file"} {
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
// key in the store and in authorized_keys. Removing it from the store
// closes the connections the store's line let in, and the file still
// lets its owner in; `key remove`, `key list` and `status` say what the
// daemon can see of that.
func TestKeyStillInTheFile(t *testing.T) {
	w := keysWorld(t, server.Limits{})
	dana := w.newUser("dana", "")
	bob := w.newUser("bob", "") // a line of authorized_keys
	w.member("bob", project, store.RoleMember)
	fp := bob.fingerprint()
	dana.ok("admin", "key", "add", "bob", bob.pub()) // and now in the store, as an import leaves it

	// sshd asks the store first, so bob's connection names the key.
	byStore := bob.watch()
	if got := dana.ok("admin", "key", "list", "bob"); strings.Contains(got, "authorized_keys") {
		t.Errorf("key list with no connection by the file mentions it:\n%s", got)
	}
	r := dana.run("v0.2.0", "admin", "key", "remove", fp)
	if r.code != cli.ExitOK || r.stdout != "key "+fp+" removed from bob\n1 connection it had signed in was closed\n" || r.stderr != "" {
		t.Errorf("key remove with every connection by the store: exit %d\nstdout %q\nstderr %q\nwant the key removed, one connection closed and no warning", r.code, r.stdout, r.stderr)
	}
	byStore.closedByServer(t, "bob")

	// The store no longer has the key; the file does, and lets bob in.
	bob.ok("list")
	byFile := bob.watch()
	if got := dana.ok("admin", "key", "list", "bob"); got != "no keys recorded for bob\n"+
		"1 open connection of bob came in by the server's authorized_keys file, not by a key listed here; removing a key does not close it\n" {
		t.Errorf("key list for a principal connected by the file: %q", got)
	}
	if got := decode[proto.KeysResult](t, dana.ok("admin", "key", "list", "bob", "--json")); got.FileConns != 1 {
		t.Errorf("key list --json: %+v, want 1 connection by the file", got)
	}

	// The key added and removed again: the admin is warned this time.
	dana.ok("admin", "key", "add", "bob", bob.pub())
	r = dana.run("v0.2.0", "admin", "key", "remove", fp)
	const warning = "sfx: warning: bob still has 1 open connection that sshd let in by the server's authorized_keys file, not by a key from the store. " +
		"It stays open, and starfixd cannot tell which key it used: if the key just removed has a line in that file, it still lets bob in. " +
		"Delete the line on the server; `sfx admin principal disable bob` closes every connection bob has\n"
	if r.code != cli.ExitOK || r.stdout != "key "+fp+" removed from bob\n" || r.stderr != warning {
		t.Errorf("key remove with a connection by the file: exit %d\nstdout %q\nstderr %q\nwant stderr %q", r.code, r.stdout, r.stderr, warning)
	}
	dana.ok("admin", "key", "add", "bob", bob.pub())
	if got := decode[proto.KeyResult](t, dana.ok("admin", "key", "remove", fp, "--json")); got.FileConns != 1 || !strings.HasPrefix(got.Warning, "bob still has 1 open connection") {
		t.Errorf("key remove --json: %+v, want the count and the warning in the one document", got)
	}
	byFile.stillOpen(t, "bob")
}
