package e2e

import (
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/cli"
	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// TestAdminTrail runs, through SSH and the daemon, the commands that
// show what an account left behind (security review of stage 3a, F-2):
// `principal disable` says what the disabled account added, and the
// three lists it names show it.
func TestAdminTrail(t *testing.T) {
	t.Parallel()
	w := adminWorld(t, 0, 0)
	w.member("alice", project, store.RoleAdmin)
	dana, alice := w.newUser("dana", ""), w.newUser("alice", "")
	ginas := w.newUser("", "") // a key the server has not seen
	alice.ok("admin", "principal", "add", "gina", "--key", ginas.pub())

	// A server admin disables alice, and is told what she left.
	r := dana.run("v0.2.0", "admin", "principal", "disable", "alice")
	const warning = "sfx: warning: alice added 1 key that is still in the store, created 1 principal and added 1 membership, and disabling alice undoes none of them: " +
		"review them with `sfx admin key list --added-by alice`, `sfx admin principal list --created-by alice` and `sfx admin member list --added-by alice`\n"
	if r.code != cli.ExitOK || r.stdout != "alice: disabled\n" || r.stderr != warning {
		t.Errorf("principal disable of an admin who added things: exit %d\nstdout %q\nstderr %q\nwant    %q", r.code, r.stdout, r.stderr, warning)
	}
	dana.ok("admin", "principal", "enable", "alice")
	got := decode[proto.PrincipalResult](t, dana.ok("--json", "admin", "principal", "disable", "alice"))
	if got.KeysAdded != 1 || got.PrincipalsCreated != 1 || got.MembersAdded != 1 || !strings.Contains(got.Warning, "--added-by alice") {
		t.Errorf("principal disable --json: %+v, want the three counts and the warning in the one document", got)
	}

	// The three lists.
	keys := dana.ok("admin", "key", "list", "--added-by", "alice")
	if !strings.HasPrefix(keys, "keys added by alice\nFINGERPRINT ") || !strings.Contains(keys, "\n"+ginas.fingerprint()+"  gina ") {
		t.Errorf("key list --added-by alice:\n%s", keys)
	}
	if k := decode[proto.KeysResult](t, dana.ok("admin", "key", "list", "--added-by", "alice", "--json")); k.AddedBy != "alice" || len(k.Keys) != 1 ||
		k.Keys[0].Principal != "gina" || k.Keys[0].Fingerprint != ginas.fingerprint() {
		t.Errorf("key list --added-by alice --json: %+v", k)
	}
	if got := dana.ok("admin", "principal", "list", "--created-by", "alice"); !strings.Contains(got, "\ngina ") || strings.Contains(got, "\nalice ") {
		t.Errorf("principal list --created-by alice:\n%s", got)
	}
	members := dana.ok("admin", "member", "list", "--added-by", "alice")
	if !strings.HasPrefix(members, "memberships added by alice\nPROJECT ") || !strings.Contains(members, "\nfirst    gina ") {
		t.Errorf("member list --added-by alice:\n%s", members)
	}
	if got := dana.ok("admin", "key", "list", "--added-by", "gina"); got != "no keys added by gina\n" {
		t.Errorf("key list --added-by someone who added none: %q", got)
	}
	// Nothing was removed: gina's key is still hers.
	if got := dana.ok("admin", "key", "list", "gina"); !strings.Contains(got, ginas.fingerprint()) {
		t.Errorf("gina's keys after alice was disabled:\n%s", got)
	}
	// A plain member is shown none of it.
	w.member("bob", project, store.RoleMember)
	if e := w.newUser("bob", "").refused(proto.CodeForbidden, "admin", "key", "list", "--added-by", "alice"); !strings.Contains(e.Message, "bob may not list the keys alice added") {
		t.Errorf("key list --added-by by a member: %+v", e)
	}
}
