package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/internal/cli"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/server"
	"github.com/ariesworx/aries/internal/store"
)

// keysWorld is a server of many projects whose sshd reads the keys file
// ariesd writes, before the authorized_keys file kept by hand, as an
// sshd with the AuthorizedKeysFile of the server guide does. dana and
// sam are its server admins.
func keysWorld(t *testing.T, lim server.Limits) *world {
	t.Helper()
	if lim.ControlWriteBurst == 0 {
		lim.ControlWriteBurst = 500
	}
	return newWorld(t, daemonOpts{many: true, admins: []string{"dana", "sam"}, keys: true, limits: lim})
}

// pub is the path of u's public key file, and fingerprint its key's.
func (u *user) pub() string { return u.pubFile }

func (u *user) fingerprint() string {
	t := u.w.t
	t.Helper()
	b, err := os.ReadFile(u.pub())
	if err != nil {
		t.Fatal(err)
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey(b)
	if err != nil {
		t.Fatal(err)
	}
	return ssh.FingerprintSHA256(key)
}

// controlEvents returns the events the control store has recorded for a
// request, each as its op, its target and who it is recorded as. A
// clock.step is not one of them: the daemon records it by itself when
// the machine's clock is set forward, and a machine that was suspended
// for some seconds while the test ran sets it forward when it wakes.
func (w *world) controlEvents() []string {
	w.t.Helper()
	evs, err := w.ctl.Events(w.t.Context(), 0, 1000)
	if err != nil {
		w.t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		if e.Op == store.OpClockStep {
			continue
		}
		out = append(out, fmt.Sprintf("%s %s by %s", e.Op, e.Target, e.Actor.Principal))
	}
	return out
}

// TestKeysFromTheStore is the whole path of this stage's keys: an admin
// adds a person and their key with `aries admin`, with no line in
// authorized_keys; the person connects with that key, because ariesd
// wrote it to the file sshd reads; the admin removes the key; the
// person's open connection closes and the next is refused.
func TestKeysFromTheStore(t *testing.T) {
	t.Parallel()
	w := keysWorld(t, server.Limits{})
	dana := w.newUser("dana", "") // the first admin: a line in authorized_keys
	bob := w.newUser("", "")      // no line anywhere

	// Before anyone adds it, bob's key lets nobody in.
	if r := bob.run("v0.2.0", "list"); r.code != cli.ExitFailure || !strings.Contains(r.stderr, "send your public key") {
		t.Fatalf("a key no one added: exit %d, stderr %q; want an auth refusal", r.code, r.stderr)
	}

	fp := bob.fingerprint()
	if got := dana.ok("admin", "principal", "add", "bob", "--key", bob.pub()); got != "principal bob added, a member of project first\nkey "+fp+" added to bob\n" {
		t.Errorf("principal add --key: %q", got)
	}
	// bob works at once, as bob, with a key authorized_keys never held.
	id := strings.TrimSpace(bob.ok("create", "bob's first issue"))
	if got := decode[proto.ShowResult](t, bob.ok("show", id, "--json")); got.Issue.CreatedBy != "bob" {
		t.Errorf("the issue was created by %q, want bob: the store's line names the principal", got.Issue.CreatedBy)
	}
	bw := bob.watch()

	// The key removed: the connection it signed in closes, and the key
	// lets nobody in again.
	if got := dana.ok("admin", "key", "remove", fp); got != "key "+fp+" removed from bob\n1 connection it had signed in was closed\n" {
		t.Errorf("key remove: %q", got)
	}
	bw.closedByServer(t, "bob")
	if r := bob.run("v0.2.0", "list"); r.code != cli.ExitFailure || !strings.Contains(r.stderr, "send your public key") {
		t.Errorf("bob's next command after his key was removed: exit %d, stderr %q; want an auth refusal", r.code, r.stderr)
	}

	// Added again from standard input, it works again; a disabled
	// principal's key is not served, and an enabled one's is.
	dana.stdin = readFile(t, bob.pub())
	if got := decode[proto.KeyResult](t, dana.ok("admin", "key", "add", "bob", "-", "--json")); got !=
		(proto.KeyResult{ID: fp, Principal: "bob", Type: "ssh-ed25519", Comment: "someone@example.com", Change: "added"}) {
		t.Errorf("key add from standard input --json: %+v", got)
	}
	bob.ok("list")
	bw = bob.watch()
	dana.ok("admin", "principal", "disable", "bob")
	bw.closedByServer(t, "bob, disabled,")
	if r := bob.run("v0.2.0", "list"); r.code != cli.ExitFailure || !strings.Contains(r.stderr, "send your public key") {
		t.Errorf("a disabled principal's key: exit %d, stderr %q; want an auth refusal: the keys file has no line for it", r.code, r.stderr)
	}
	dana.ok("admin", "principal", "enable", "bob")
	bob.ok("list")

	// A member of nothing authenticates and is refused by the project.
	carol := w.newUser("", "")
	dana.ok("admin", "principal", "add", "carol", "--no-project", "--key", carol.pub())
	if e := carol.refused(proto.CodeForbidden, "list"); !strings.Contains(e.Message, "carol is not a member of project") {
		t.Errorf("a principal with a key and no membership: %+v", e)
	}
}

// TestStaleKeysFileIsNotAHole states the property the keys file rests
// on: the file is a cache, and the store decides. A key that was removed
// from the store, or whose principal was disabled, and that is still in
// the file sshd reads, gets past sshd and is refused by the daemon at
// the handshake. The file is made stale twice: by a daemon that cannot
// write it, which also shows what the admin and `status` are told, and
// by hand.
//
// sshd reads the keys file before the authorized_keys file kept by hand
// (decision 26), so a stale line also shadows the hand-kept line of the
// same key: frank's removed key, which that file would let in, is
// refused while the stale line stands, and let in by the hand-kept line
// once the keys file is written again. Stale, the file is never more
// permissive than the store, and for a key in both it is stricter.
func TestStaleKeysFileIsNotAHole(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("needs a directory this user cannot write to")
	}
	w := keysWorld(t, server.Limits{})
	dana := w.newUser("dana", "")
	bob, carol := w.newUser("", ""), w.newUser("", "") // keys only the store holds
	dana.ok("admin", "principal", "add", "bob", "--key", bob.pub())
	dana.ok("admin", "principal", "add", "carol", "--key", carol.pub())
	frank := w.newUser("frank", "") // a line of authorized_keys, and a key of the store
	w.member("frank", project, store.RoleMember)
	dana.ok("admin", "key", "add", "frank", frank.pub())
	bob.ok("list")
	carol.ok("list")
	frank.ok("list")
	fresh := readFile(t, w.keysFile)
	sshdLetsIn := func(u *user) bool {
		t.Helper()
		pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(readFile(t, u.pub())))
		if err != nil {
			t.Fatal(err)
		}
		return w.fileLine(pub) != nil
	}

	// The daemon can no longer replace its file.
	dir := filepath.Dir(w.keysFile)
	if err := os.Chmod(dir, 0o500); err != nil { //nolint:gosec // a directory the daemon cannot write to
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // back to what it was
	const stale = "aries: warning: the change is recorded, but ariesd could not write the keys file sshd reads, so sshd may still be reading the old one: "
	r := dana.run("v0.2.0", "admin", "key", "remove", bob.fingerprint())
	if r.code != cli.ExitOK || r.stdout != "key "+bob.fingerprint()+" removed from bob\n" || !strings.HasPrefix(r.stderr, stale) {
		t.Fatalf("key remove with the keys file unwritable: exit %d\nstdout %q\nstderr %q\nwant the key removed, and a warning that sshd may read the old file", r.code, r.stdout, r.stderr)
	}
	r = dana.run("v0.2.0", "admin", "principal", "disable", "carol")
	if r.code != cli.ExitOK || r.stdout != "carol: disabled\n" || !strings.HasPrefix(r.stderr, stale) {
		t.Errorf("principal disable with the keys file unwritable: exit %d\nstdout %q\nstderr %q\nwant her disabled, and the warning", r.code, r.stdout, r.stderr)
	}
	r = dana.run("v0.2.0", "admin", "key", "remove", frank.fingerprint())
	if r.code != cli.ExitOK || !strings.HasPrefix(r.stderr, "aries: warning: sshd will still accept key "+frank.fingerprint()) ||
		!strings.Contains(r.stderr, "; and the change is recorded, but ariesd could not write the keys file sshd reads") {
		t.Errorf("key remove of a key also in authorized_keys, with the keys file unwritable: exit %d\nstderr %q\nwant both warnings in the one line", r.code, r.stderr)
	}
	if got := readFile(t, w.keysFile); got != fresh {
		t.Fatalf("the keys file changed though its directory is read-only:\n%s", got)
	}
	if !sshdLetsIn(bob) || !sshdLetsIn(carol) || !sshdLetsIn(frank) {
		t.Fatal("the stale keys file no longer lets the three keys past sshd; the test needs it to")
	}
	// frank's key is still a line of authorized_keys, and sshd does not
	// reach it: the stale line comes first, and names a key the store no
	// longer holds.
	if e := frank.refused(proto.CodeAuth, "list"); e.Message != "the key that signed in is no longer one the server holds for frank" {
		t.Errorf("a removed key that a stale keys file and authorized_keys both hold: %+v; want the daemon's auth refusal", e)
	}
	// Past sshd, and no further.
	if e := bob.refused(proto.CodeAuth, "list"); e.Message != "the key that signed in is no longer one the server holds for bob" || e.Fix == "" {
		t.Errorf("a removed key that a stale keys file still holds: %+v; want the daemon's auth refusal, with a fix", e)
	}
	if e := carol.refused(proto.CodeForbidden, "list"); e.Message != "carol is disabled on this server" {
		t.Errorf("a disabled principal's key that a stale keys file still holds: %+v; want the daemon's refusal", e)
	}
	st := decode[proto.StatusResult](t, dana.ok("admin", "status", "--json"))
	if k := st.Server.KeysFile; k == nil || !k.Failed || k.Path != w.keysFile {
		t.Errorf("status --json of the keys file after a failed write = %+v, want it failed", k)
	}
	if text := dana.ok("admin", "status"); !strings.Contains(text, "keys file "+w.keysFile+": THE LAST WRITE FAILED, so sshd may be reading an older list") {
		t.Errorf("status does not say the keys file is out of date:\n%s", text)
	}

	// Writable again: the next change is written, and sshd refuses both.
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // back to what it was
		t.Fatal(err)
	}
	if r := dana.run("v0.2.0", "admin", "principal", "add", "erin", "--key", w.newUser("", "").pub()); r.code != cli.ExitOK || r.stderr != "" {
		t.Fatalf("principal add once the keys file can be written: exit %d, stderr %q; want no warning", r.code, r.stderr)
	}
	if sshdLetsIn(bob) || sshdLetsIn(carol) || sshdLetsIn(frank) {
		t.Error("the keys file, written again, still has a line for a removed key or a disabled principal's")
	}
	// With no line in the keys file, frank's key reaches its line of
	// authorized_keys, as the removal's warning said it would.
	frank.ok("list")
	if st := decode[proto.StatusResult](t, dana.ok("admin", "status", "--json")); st.Server.KeysFile.Failed || st.Server.KeysFile.Keys != 1 {
		t.Errorf("status of the keys file once written again = %+v; want it current, with erin's key", st.Server.KeysFile)
	}

	// The old file put back by hand, as a restored backup would be: the
	// same two keys get past sshd again, and the daemon refuses them.
	if err := os.WriteFile(w.keysFile, []byte(fresh), 0o600); err != nil {
		t.Fatal(err)
	}
	if !sshdLetsIn(bob) {
		t.Fatal("the keys file written by hand does not let bob's key past sshd; the test needs it to")
	}
	if e := bob.refused(proto.CodeAuth, "list"); e.Message != "the key that signed in is no longer one the server holds for bob" {
		t.Errorf("a removed key in a keys file written by hand: %+v; want the daemon's auth refusal", e)
	}
	if e := carol.refused(proto.CodeForbidden, "list"); e.Message != "carol is disabled on this server" {
		t.Errorf("a disabled principal's key in a keys file written by hand: %+v; want the daemon's refusal", e)
	}
	if e := frank.refused(proto.CodeAuth, "list"); e.Message != "the key that signed in is no longer one the server holds for frank" {
		t.Errorf("a removed key in a keys file written by hand, and in authorized_keys: %+v; want the daemon's auth refusal", e)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestAdminKeyCommands runs `aries admin key` and `principal add --key`
// through SSH and the daemon, in text and --json, as those who may and
// those who may not.
func TestAdminKeyCommands(t *testing.T) {
	t.Parallel()
	w := keysWorld(t, server.Limits{Control: store.ControlLimits{Keys: 3}})
	w.addProject(second, "second", "two")
	w.member("alice", project, store.RoleAdmin) // reaches bob, who is in her project only
	w.member("erin", second, store.RoleAdmin)
	w.member("carol", project, store.RoleMember)
	w.member("carol", second, store.RoleMember)
	if _, err := w.ctl.AddPrincipal(t.Context(), sam, "dana", ""); err != nil { // a server admin with a row
		t.Fatal(err)
	}
	dana, alice, erin, carol := w.newUser("dana", ""), w.newUser("alice", ""), w.newUserIn("erin", "", second), w.newUser("carol", "")
	laptop, desktop, phone, fourth := w.newUser("", ""), w.newUser("", ""), w.newUser("", ""), w.newUser("", "")

	// principal add --key, more than once, in --json.
	added := decode[proto.PrincipalAddResult](t, alice.ok("admin", "principal", "add", "bob", "--key", laptop.pub(), "--key", desktop.pub(), "--json"))
	// Keys added in one write are as old as each other, and are listed by
	// fingerprint.
	together := slices.Sorted(slices.Values([]string{laptop.fingerprint(), desktop.fingerprint()}))
	if added.ID != "bob" || added.Project != project || !slices.Equal(added.Keys, together) {
		t.Errorf("principal add --key --key --json: %+v", added)
	}
	bob := laptop

	// key add: from a file, relative to where the command runs; again;
	// and in --json.
	rel, err := filepath.Rel(filepath.Join(alice.repo, "src"), phone.pub())
	if err != nil {
		t.Fatal(err)
	}
	if got := alice.ok("admin", "key", "add", "bob", rel); got != "key "+phone.fingerprint()+" (ssh-ed25519, someone@example.com) added to bob\n" {
		t.Errorf("key add from a relative path: %q", got)
	}
	if got := dana.ok("admin", "key", "add", "bob", phone.pub()); got != "bob already has key "+phone.fingerprint()+" (ssh-ed25519, someone@example.com)\n" {
		t.Errorf("key add of a key bob has: %q", got)
	}
	if got := decode[proto.KeyResult](t, dana.ok("--json", "admin", "key", "add", "bob", phone.pub())); got.Change != "unchanged" || got.ID != phone.fingerprint() {
		t.Errorf("key add of a key bob has, --json: %+v", got)
	}

	// key list, by each who may: a server admin, an admin with reach,
	// and bob himself.
	for _, tc := range []struct {
		who string
		u   *user
	}{{"a server admin", dana}, {"an admin with reach", alice}, {"bob himself", bob}} {
		out := tc.u.ok("admin", "key", "list", "bob")
		for _, want := range []string{"keys of bob\n", "FINGERPRINT", "TYPE", "ADDED BY",
			laptop.fingerprint() + "  ssh-ed25519  someone@example.com  alice", phone.fingerprint() + "  ssh-ed25519  someone@example.com  alice"} {
			if !strings.Contains(out, want) {
				t.Errorf("key list bob for %s lacks %q:\n%s", tc.who, want, out)
			}
		}
		ks := decode[proto.KeysResult](t, tc.u.ok("admin", "key", "list", "bob", "--json"))
		var fps []string
		for _, k := range ks.Keys {
			fps = append(fps, k.Fingerprint)
		}
		if ks.Principal != "bob" || !slices.Equal(fps, append(slices.Clone(together), phone.fingerprint())) {
			t.Errorf("key list bob --json for %s = %+v", tc.who, ks)
		}
	}
	if got := dana.ok("admin", "key", "list", "alice"); got != "no keys recorded for alice\n" {
		t.Errorf("key list of a principal with no keys: %q", got)
	}

	// The refusals: who may not, the cap, a key another has, one's own.
	before := w.controlEvents()
	const ownFix = "ask another admin to run it: a server admin, or a project admin who administers every project you belong to; the server's operator can also change authorized_keys on the server"
	listFix := func(who string) string {
		return "ask a server admin, or an admin of a project the principal belongs to; `aries admin key list " + who + "` lists your own"
	}
	for _, tc := range []struct {
		name string
		u    *user
		args []string
		code proto.Code
		msg  string
		fix  string
	}{
		{"a member lists another's keys", carol, []string{"admin", "key", "list", "bob"}, proto.CodeForbidden,
			"carol may not list the keys of bob: it is for the principal, for an admin of a project it belongs to, and for server admins", listFix("carol")},
		{"an admin of another project lists them", erin, []string{"admin", "key", "list", "bob"}, proto.CodeForbidden,
			"erin may not list the keys of bob: it is for the principal, for an admin of a project it belongs to, and for server admins", listFix("erin")},
		// An admin of a project the principal is in lists them, with or
		// without reach (decision 36): alice and carol, below the table.
		{"an admin lists the keys of someone the server has never heard of", alice, []string{"admin", "key", "list", "nobody"}, proto.CodeForbidden,
			"alice may not list the keys of nobody: it is for the principal, for an admin of a project it belongs to, and for server admins", listFix("alice")},
		{"a member adds a key to another", carol, []string{"admin", "key", "add", "bob", fourth.pub()}, proto.CodeForbidden,
			"carol may not add a key to bob: it is for project admins and server admins",
			"ask a server admin to run it, or a project admin who administers every project the principal belongs to"},
		{"an admin without reach adds one", alice, []string{"admin", "key", "add", "carol", fourth.pub()}, proto.CodeForbidden,
			"alice may not add a key to carol: carol does not exist, or belongs to a project alice does not administer, or to none",
			"check the name, or ask a server admin to run it; a project admin may only while every project the principal belongs to is one they administer"},
		{"an admin without reach removes one", erin, []string{"admin", "key", "remove", laptop.fingerprint()}, proto.CodeForbidden,
			"erin may not remove that key: it is no key of a principal erin may manage",
			"check the fingerprint with `aries admin key list NAME`, or ask a server admin to run it"},
		{"a member adds their own", bob, []string{"admin", "key", "add", "bob", fourth.pub()}, proto.CodeForbidden,
			"bob may not add a key to bob: nobody adds a key to their own account", ownFix},
		{"a project admin adds their own", alice, []string{"admin", "key", "add", "alice", fourth.pub()}, proto.CodeForbidden,
			"alice may not add a key to alice: nobody adds a key to their own account", ownFix},
		{"a server admin adds their own", dana, []string{"admin", "key", "add", "dana", fourth.pub()}, proto.CodeForbidden,
			"dana may not add a key to dana: nobody adds a key to their own account", ownFix},
		{"a key past the cap", dana, []string{"admin", "key", "add", "bob", fourth.pub()}, proto.CodeInvalid,
			"a principal keeps at most 3 keys",
			"remove a key the principal no longer uses with `aries admin key remove FINGERPRINT`, or ask the server's operator to raise keys_per_principal under limits: in ariesd's config"},
		{"a key another principal has", dana, []string{"admin", "key", "add", "carol", phone.pub()}, proto.CodeExists,
			"key " + phone.fingerprint() + " already belongs to another principal",
			"use another key: a key belongs to one principal, and an agent or a machine that needs less access gets a key and a principal of its own"},
		{"a principal that does not exist", dana, []string{"admin", "key", "add", "nobody", fourth.pub()}, proto.CodeNotFound,
			"principal nobody not found", "check the name; a server admin lists them with `aries admin principal list`"},
		{"a fingerprint no key has", dana, []string{"admin", "key", "remove", fourth.fingerprint()}, proto.CodeNotFound,
			"key " + fourth.fingerprint() + " not found", "list a principal's keys, with their fingerprints, with `aries admin key list NAME`"},
		// The private key beside the .pub file: refused here, and nothing
		// sent.
		{"a private key file", dana, []string{"admin", "key", "add", "bob", filepath.Join(fourth.repo, "id_ed25519")}, proto.CodeInvalid,
			filepath.Join(fourth.repo, "id_ed25519") + " holds a private key, which never leaves this machine",
			"give the public key instead: " + filepath.Join(fourth.repo, "id_ed25519") + ".pub"},
		{"a private key file, with a new principal", dana, []string{"admin", "principal", "add", "hal", "--key", filepath.Join(fourth.repo, "id_ed25519")}, proto.CodeInvalid,
			filepath.Join(fourth.repo, "id_ed25519") + " holds a private key, which never leaves this machine",
			"give the public key instead: " + filepath.Join(fourth.repo, "id_ed25519") + ".pub"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if e := tc.u.refused(tc.code, tc.args...); e.Message != tc.msg || e.Fix != tc.fix {
				t.Errorf("aries %s\n got %q\n     fix %q\nwant %q\n     fix %q", strings.Join(tc.args, " "), e.Message, e.Fix, tc.msg, tc.fix)
			}
		})
	}
	if after := w.controlEvents(); len(after) != len(before) {
		t.Errorf("the refusals recorded %d events in the control store, want none: %q", len(after)-len(before), after[len(before):])
	}
	if _, err := w.ctl.Principal(t.Context(), "hal"); err == nil {
		t.Error("principal add with a private key file added the principal")
	}
	// alice administers a project carol is in, and has no reach over her:
	// she may list carol's keys, which sign in to her project too.
	if got := alice.ok("admin", "key", "list", "carol"); !strings.Contains(got, "carol") {
		t.Errorf("key list carol by an admin of one of her projects:\n%s", got)
	}

	// key remove, by an admin with reach, in text and --json.
	if got := alice.ok("admin", "key", "remove", desktop.fingerprint()); got != "key "+desktop.fingerprint()+" removed from bob\n" {
		t.Errorf("key remove: %q", got)
	}
	if got := decode[proto.KeyResult](t, dana.ok("admin", "key", "remove", phone.fingerprint(), "--json")); got !=
		(proto.KeyResult{ID: phone.fingerprint(), Principal: "bob", Type: "ssh-ed25519", Comment: "someone@example.com"}) {
		t.Errorf("key remove --json: %+v", got)
	}
	// Another server admin changes a server admin's keys.
	if got := w.newUser("sam", "").ok("admin", "key", "add", "dana", fourth.pub()); !strings.HasSuffix(got, " added to dana\n") {
		t.Errorf("key add to a server admin by another: %q", got)
	}

	// Every key change is on the control store's record, by who made it.
	evs, err := w.ctl.Events(t.Context(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var by []string
	for _, e := range evs {
		if e.Op == store.OpKeyAdd || e.Op == store.OpKeyRemove {
			by = append(by, string(e.Op)+" by "+e.Actor.Principal)
		}
	}
	if want := []string{"key.add by alice", "key.add by alice", "key.add by alice", "key.remove by alice", "key.remove by dana", "key.add by sam"}; !slices.Equal(by, want) {
		t.Errorf("key events = %v, want %v", by, want)
	}
}

// sam is the second server admin of keysWorld.
var sam = store.Actor{Principal: "sam", Session: "e2e", Machine: "server"}

// TestDisabledWins pins decision 20 end to end: a server admin whose
// principal is disabled is refused, config file or not, until enabled,
// and the last enabled server admin cannot be disabled.
func TestDisabledWins(t *testing.T) {
	t.Parallel()
	w := keysWorld(t, server.Limits{})
	dana, sam := w.newUser("dana", ""), w.newUser("sam", "")
	// sam has no row, and is a server admin all the same.
	if got := sam.ok("admin", "principal", "add", "dana", "--no-project"); got != "principal dana added, in no project\n" {
		t.Errorf("principal add of a server admin: %q", got)
	}
	dana.ok("admin", "principal", "add", "bob")
	dw := dana.watch()

	if got := sam.ok("admin", "principal", "disable", "dana"); got != "dana: disabled\n" {
		t.Errorf("principal disable of a server admin: %q", got)
	}
	dw.closedByServer(t, "dana, a disabled server admin,")
	for _, args := range [][]string{{"list"}, {"admin", "principal", "list"}, {"admin", "principal", "enable", "dana"}} {
		e := dana.refused(proto.CodeForbidden, args...)
		if e.Message != "dana is disabled on this server" || !strings.Contains(e.Fix, "`aries admin principal enable dana`") {
			t.Errorf("aries %s by a disabled server admin: %+v", strings.Join(args, " "), e)
		}
	}
	sam.ok("admin", "principal", "enable", "dana")
	dana.ok("admin", "principal", "list")

	// dana disables sam, once sam has a row; then she is the last.
	dana.ok("admin", "principal", "add", "sam", "--no-project")
	dana.ok("admin", "principal", "disable", "sam")
	e := dana.refused(proto.CodeInvalid, "admin", "principal", "disable", "dana")
	if e.Message != "dana is the last server admin who is not disabled; without one, nobody could enable anyone from here" ||
		e.Fix != "enable another server admin first with `aries admin principal enable NAME`, or ask the server's operator to name another under admins: in ariesd's config" {
		t.Errorf("disabling the last enabled server admin: %+v", e)
	}
	dana.ok("list")
}

// TestLosingAccessReleasesClaims pins decision 21 end to end: the claim
// of a member who is removed ends at once, on the record, and the issue
// is ready for the next person.
func TestLosingAccessReleasesClaims(t *testing.T) {
	t.Parallel()
	w := keysWorld(t, server.Limits{})
	w.member("alice", project, store.RoleAdmin)
	w.member("bob", project, store.RoleMember)
	dana, alice, bob := w.newUser("dana", ""), w.newUser("alice", ""), w.newUser("bob", "")
	id := strings.TrimSpace(bob.ok("create", "bob's work"))
	kept := strings.TrimSpace(alice.ok("create", "alice's work"))
	bob.ok("start", id)
	alice.ok("start", kept)

	// A demotion releases nothing.
	if got := dana.ok("admin", "member", "add", "alice", "--role", "member"); got != "alice is now member in project first\n" {
		t.Errorf("a demotion: %q", got)
	}
	if got := dana.ok("admin", "member", "remove", "bob"); got != "bob removed from project first\n1 claim bob held was released\n" {
		t.Errorf("member remove of someone holding an issue: %q", got)
	}
	sh := decode[proto.ShowResult](t, dana.ok("show", id, "--json"))
	if sh.Claim != nil || sh.Issue.Status != "open" || sh.Issue.Assignee != "" {
		t.Errorf("bob's issue after his removal: claim %+v, status %s, assignee %q; want it open and free", sh.Claim, sh.Issue.Status, sh.Issue.Assignee)
	}
	if out := alice.ok("ready"); !strings.Contains(out, id) {
		t.Errorf("bob's issue is not ready for alice:\n%s", out)
	}
	if sh := decode[proto.ShowResult](t, dana.ok("show", kept, "--json")); sh.Claim == nil || sh.Claim.By != "alice" {
		t.Errorf("alice's claim after her demotion and bob's removal = %+v, want hers still", sh.Claim)
	}
	hist := dana.ok("history", id)
	if !strings.Contains(hist, "claim.release") || !strings.Contains(hist, "dana") {
		t.Errorf("the issue's history lacks the release by dana:\n%s", hist)
	}
	// In --json, the count is in the one document.
	bob2 := strings.TrimSpace(alice.ok("create", "more work"))
	dana.ok("admin", "member", "add", "bob")
	bob.ok("start", bob2)
	if got := decode[proto.PrincipalResult](t, dana.ok("--json", "admin", "principal", "disable", "bob")); got.State != "disabled" || got.Released != 1 {
		t.Errorf("principal disable --json of someone holding an issue: %+v, want 1 released", got)
	}
}

// A server of one project refuses the key commands, and says how a
// server opts in.
func TestAdminKeyOnAServerOfOneProject(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{admins: []string{"dana"}})
	d := w.newUser("dana", "")
	for _, args := range [][]string{
		{"admin", "key", "list", "bob"}, {"admin", "key", "add", "bob", d.pub()},
		{"admin", "key", "remove", "SHA256:et6CqkKsyU2BxzA7Ws+V18rXrtM9Gj6dk1/m0C5TqvU"},
	} {
		if e := d.refused(proto.CodeInvalid, args...); !strings.Contains(e.Message, "this server holds one project") ||
			!strings.Contains(e.Fix, "setting control_schema (docs/server.md, Serving many projects, Opt in)") {
			t.Errorf("aries %s on a server of one project: %+v", strings.Join(args, " "), e)
		}
	}
}
