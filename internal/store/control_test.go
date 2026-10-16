package store

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"
)

// The control tests' people. dana and sam are the server admins
// (openControl); sam never has a principal row.
var (
	sam   = Actor{Principal: "sam", Session: "sess-s", Machine: "laptop-s"}
	erin  = Actor{Principal: "erin", Session: "sess-e", Machine: "laptop-e"}
	frank = Actor{Principal: "frank", Session: "sess-f", Machine: "laptop-f"}
	ivy   = Actor{Principal: "ivy", Session: "sess-i", Machine: "laptop-i"}
)

// Invented project ids.
const (
	projA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	projB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	projC = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
)

func openControl(t *testing.T, dsn string, opts ControlOptions) *Control {
	t.Helper()
	if opts.Admins == nil {
		opts.Admins = []string{"dana", "sam"}
	}
	d, err := ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	c, err := testDB(t, dsn).OpenControl(t.Context(), d.Schema, opts)
	if err != nil {
		t.Fatalf("open control: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("close control: %v", err)
		}
	})
	return c
}

func newControl(t *testing.T) *Control {
	t.Helper()
	return openControl(t, newDSN(t), ControlOptions{})
}

// testKey returns a public key line, without a comment, and its
// fingerprint. The same seed gives the same key.
func testKey(t *testing.T, seed string) (line, fingerprint string) {
	t.Helper()
	sum := sha256.Sum256([]byte(seed))
	pub, err := ssh.NewPublicKey(ed25519.NewKeyFromSeed(sum[:]).Public())
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), ssh.FingerprintSHA256(pub)
}

// testCertificate returns an OpenSSH user certificate in authorized_keys
// form: a key signed by an authority, which is not a plain public key.
func testCertificate(t *testing.T) string {
	t.Helper()
	sum := sha256.Sum256([]byte("authority"))
	ca, err := ssh.NewSignerFromKey(ed25519.NewKeyFromSeed(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	sum = sha256.Sum256([]byte("certified"))
	pub, err := ssh.NewPublicKey(ed25519.NewKeyFromSeed(sum[:]).Public())
	if err != nil {
		t.Fatal(err)
	}
	cert := &ssh.Certificate{Key: pub, CertType: ssh.UserCert, KeyId: "alice", ValidBefore: ssh.CertTimeInfinity}
	if err := cert.SignCert(rand.Reader, ca); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(cert)))
}

// newProject is the project the tests add under id: named for its last
// character, as project a, b or c.
func newProject(id string) NewProject {
	n := id[len(id)-1:]
	return NewProject{ID: id, Name: "proj-" + n, Prefix: "p" + n, Account: "internal", Schema: "aries_p" + n}
}

func mustProject(t *testing.T, c *Control, id string) Project {
	t.Helper()
	if _, err := c.AddProject(t.Context(), dana, newProject(id)); err != nil {
		t.Fatalf("add project %s: %v", id, err)
	}
	p, err := c.ActivateProject(t.Context(), dana, id)
	if err != nil {
		t.Fatalf("activate project %s: %v", id, err)
	}
	return p
}

// mustPrincipal adds a principal as dana, with a role in each project
// given as "PROJECT=ROLE".
func mustPrincipal(t *testing.T, c *Control, name string, memberships ...string) {
	t.Helper()
	if _, err := c.AddPrincipal(t.Context(), dana, name, ""); err != nil {
		t.Fatalf("add principal %s: %v", name, err)
	}
	for _, m := range memberships {
		project, role, _ := strings.Cut(m, "=")
		if _, err := c.SetMember(t.Context(), dana, name, project, Role(role)); err != nil {
			t.Fatalf("set member %s %s: %v", name, m, err)
		}
	}
}

// mustKey adds the key seed gives to principal, as dana; dana's own are
// added by sam, since nobody changes their own keys (decision 19).
func mustKey(t *testing.T, c *Control, principal, seed string) PrincipalKey {
	t.Helper()
	line, _ := testKey(t, seed)
	by := dana
	if principal == by.Principal {
		by = sam
	}
	k, err := c.AddKey(t.Context(), by, principal, line)
	if err != nil {
		t.Fatalf("add key %s to %s: %v", seed, principal, err)
	}
	return k
}

func controlSeq(t *testing.T, c *Control) int64 {
	t.Helper()
	n, err := lastEventSeq(t.Context(), c.s.r)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// ev is an event a test expects: its op, target and actor, and a
// substring of its after state.
type ev struct {
	op     Op
	target string
	by     Actor
	after  string
}

// wantEvents checks that exactly want were recorded after seq.
func wantEvents(t *testing.T, c *Control, seq int64, want ...ev) {
	t.Helper()
	got, err := c.Events(t.Context(), seq, 0)
	if err != nil {
		t.Fatalf("Events(%d): %v", seq, err)
	}
	if len(got) != len(want) {
		t.Errorf("recorded %d events after %d, want %d: %+v", len(got), seq, len(want), got)
		return
	}
	for i, w := range want {
		g := got[i]
		if g.Op != w.op || g.Target != w.target || g.Actor != w.by || !strings.Contains(string(g.After), w.after) {
			t.Errorf("event %d = %s on %q by %+v, after %s; want %s on %q by %+v, after containing %q",
				g.Seq, g.Op, g.Target, g.Actor, g.After, w.op, w.target, w.by, w.after)
		}
	}
}

func TestControlProjects(t *testing.T) {
	t.Parallel()
	c := newControl(t)
	ctx := t.Context()
	in := newProject(projA)

	seq := controlSeq(t, c)
	p, err := c.AddProject(ctx, dana, in)
	if err != nil {
		t.Fatalf("AddProject(%+v): %v", in, err)
	}
	if p.ID != in.ID || p.Name != in.Name || p.Prefix != in.Prefix || p.Account != in.Account || p.Schema != in.Schema ||
		p.State != ProjectCreating || p.CreatedBy != "dana" || p.Rev != 1 || p.CreatedAt.IsZero() {
		t.Errorf("AddProject(%+v) = %+v, want those fields, state creating, by dana, rev 1", in, p)
	}
	wantEvents(t, c, seq, ev{OpProjectAdd, projA, dana, `"schema":"aries_pa"`})

	// A creation that failed halfway is run again: the same request, by
	// any server admin, returns the row and records nothing.
	seq = controlSeq(t, c)
	again, err := c.AddProject(ctx, sam, in)
	if err != nil || again != p {
		t.Errorf("AddProject again = %+v, %v; want the first row %+v", again, err, p)
	}
	wantEvents(t, c, seq)

	taken := []struct {
		name string
		in   NewProject
		kind TakenKind
	}{
		{"same id, other name", NewProject{projA, "other", in.Prefix, in.Account, in.Schema}, TakenProjectID},
		{"same id, other prefix", NewProject{projA, in.Name, "zz", in.Account, in.Schema}, TakenProjectID},
		{"same id, other account", NewProject{projA, in.Name, in.Prefix, "acme", in.Schema}, TakenProjectID},
		{"same id, other database", NewProject{projA, in.Name, in.Prefix, in.Account, "aries_other"}, TakenProjectID},
		{"other id, same name", NewProject{projB, in.Name, "pb", "internal", "aries_pb"}, TakenProjectName},
		{"other id, same schema", NewProject{projB, "proj-b", "pb", "internal", in.Schema}, TakenSchema},
	}
	for _, tc := range taken {
		t.Run(tc.name, func(t *testing.T) {
			seq := controlSeq(t, c)
			_, err := c.AddProject(ctx, dana, tc.in)
			var te *TakenError
			if !errors.As(err, &te) || te.Kind != tc.kind || !errors.Is(err, ErrConflict) {
				t.Errorf("AddProject(%+v) = %v, want a *TakenError of kind %q wrapping ErrConflict", tc.in, err, tc.kind)
			}
			wantEvents(t, c, seq)
		})
	}

	seq = controlSeq(t, c)
	p, err = c.ActivateProject(ctx, dana, projA)
	if err != nil || p.State != ProjectActive || p.Rev != 2 {
		t.Errorf("ActivateProject = %+v, %v; want active at rev 2", p, err)
	}
	if again, err := c.ActivateProject(ctx, dana, projA); err != nil || again != p {
		t.Errorf("ActivateProject again = %+v, %v; want %+v unchanged", again, err, p)
	}
	if again, err := c.AddProject(ctx, dana, in); err != nil || again != p {
		t.Errorf("AddProject of an active project = %+v, %v; want %+v unchanged", again, err, p)
	}
	wantEvents(t, c, seq, ev{OpProjectActivate, projA, dana, `"state":"active"`})

	if got, err := c.Project(ctx, projA); err != nil || got != p {
		t.Errorf("Project(%s) = %+v, %v; want %+v", projA, got, err, p)
	}

	seq = controlSeq(t, c)
	p, err = c.ArchiveProject(ctx, dana, projA)
	if err != nil || p.State != ProjectArchived || p.Rev != 3 {
		t.Errorf("ArchiveProject = %+v, %v; want archived at rev 3", p, err)
	}
	if again, err := c.ArchiveProject(ctx, dana, projA); err != nil || again != p {
		t.Errorf("ArchiveProject again = %+v, %v; want %+v unchanged", again, err, p)
	}
	wantEvents(t, c, seq, ev{OpProjectArchive, projA, dana, `"state":"archived"`})

	// A server admin activates an archived project again; the event's
	// before state says it was archived.
	seq = controlSeq(t, c)
	p, err = c.ActivateProject(ctx, sam, projA)
	if err != nil || p.State != ProjectActive || p.Rev != 4 {
		t.Errorf("ActivateProject of an archived project = %+v, %v; want active at rev 4", p, err)
	}
	wantEvents(t, c, seq, ev{OpProjectActivate, projA, sam, `"state":"active"`})
	if evs, err := c.Events(ctx, seq, 0); err != nil || len(evs) != 1 || !strings.Contains(string(evs[0].Before), `"state":"archived"`) {
		t.Errorf("the reactivation's event = %+v, %v; want a before state that is archived", evs, err)
	}
	if _, err := c.ActivateProject(ctx, alice, projA); !errors.Is(err, ErrForbidden) {
		t.Errorf("ActivateProject by someone who is no server admin = %v, want ErrForbidden", err)
	}

	// A project never activated can be archived, which abandons it.
	if _, err := c.AddProject(ctx, dana, newProject(projB)); err != nil {
		t.Fatal(err)
	}
	if b, err := c.ArchiveProject(ctx, dana, projB); err != nil || b.State != ProjectArchived {
		t.Errorf("ArchiveProject of a creating project = %+v, %v; want archived", b, err)
	}

	for name, run := range map[string]func() error{
		"Project":         func() error { _, err := c.Project(ctx, projC); return err },
		"ActivateProject": func() error { _, err := c.ActivateProject(ctx, dana, projC); return err },
		"ArchiveProject":  func() error { _, err := c.ArchiveProject(ctx, dana, projC); return err },
	} {
		if err := run(); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s(%s) = %v, want ErrNotFound", name, projC, err)
		}
	}

	all, err := c.Projects(ctx)
	if err != nil || len(all) != 2 || all[0].ID != projA || all[1].ID != projB {
		t.Errorf("Projects() = %+v, %v; want a then b, by name", all, err)
	}
}

func TestControlProjectValidation(t *testing.T) {
	t.Parallel()
	c := newControl(t)
	ok := newProject(projA)
	with := func(f func(*NewProject)) NewProject {
		p := ok
		f(&p)
		return p
	}
	cases := []struct {
		name string
		in   NewProject
	}{
		{"empty id", with(func(p *NewProject) { p.ID = "" })},
		{"uppercase id", with(func(p *NewProject) { p.ID = strings.ToUpper(projA) })},
		{"id not a uuid", with(func(p *NewProject) { p.ID = "proj-a" })},
		{"empty name", with(func(p *NewProject) { p.Name = "" })},
		{"name with a space", with(func(p *NewProject) { p.Name = "proj a" })},
		{"uppercase name", with(func(p *NewProject) { p.Name = "Proj" })},
		{"name too long", with(func(p *NewProject) { p.Name = strings.Repeat("a", 65) })},
		{"name with a control character", with(func(p *NewProject) { p.Name = "proj\x1b" })},
		{"empty prefix", with(func(p *NewProject) { p.Prefix = "" })},
		{"prefix starts with a digit", with(func(p *NewProject) { p.Prefix = "1p" })},
		{"prefix too long", with(func(p *NewProject) { p.Prefix = strings.Repeat("p", 33) })},
		{"empty account", with(func(p *NewProject) { p.Account = "" })},
		{"account with an underscore", with(func(p *NewProject) { p.Account = "a_b" })},
		{"empty database", with(func(p *NewProject) { p.Schema = "" })},
		{"database starts with a digit", with(func(p *NewProject) { p.Schema = "1db" })},
		{"database with a hyphen", with(func(p *NewProject) { p.Schema = "aries-p1" })},
		{"database with a backtick", with(func(p *NewProject) { p.Schema = "a`; DROP DATABASE x; --" })},
		{"uppercase database", with(func(p *NewProject) { p.Schema = "Aries" })},
		{"database too long", with(func(p *NewProject) { p.Schema = strings.Repeat("d", 64) })},
		{"database Postgres keeps for its own", with(func(p *NewProject) { p.Schema = "pg_aries" })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.AddProject(t.Context(), dana, tc.in); !errors.Is(err, ErrInvalid) {
				t.Errorf("AddProject(%+v) = %v, want ErrInvalid", tc.in, err)
			}
		})
	}
	wantEvents(t, c, 0)
	longest := NewProject{ID: projA, Name: strings.Repeat("n", 64), Prefix: strings.Repeat("p", 32),
		Account: strings.Repeat("a", 64), Schema: "d" + strings.Repeat("_", 62)}
	if _, err := c.AddProject(t.Context(), dana, longest); err != nil {
		t.Errorf("AddProject with every field at its longest: %v", err)
	}
	for _, id := range []string{"", "nope", strings.ToUpper(projA)} {
		if _, err := c.Project(t.Context(), id); !errors.Is(err, ErrInvalid) {
			t.Errorf("Project(%q) = %v, want ErrInvalid", id, err)
		}
	}
}

func TestControlPrincipals(t *testing.T) {
	t.Parallel()
	c := newControl(t)
	ctx := t.Context()
	mustProject(t, c, projA)

	seq := controlSeq(t, c)
	p, err := c.AddPrincipal(ctx, dana, "alice", "")
	if err != nil || p.Name != "alice" || p.State != PrincipalActive || p.CreatedBy != "dana" || p.Rev != 1 {
		t.Errorf("AddPrincipal(alice) = %+v, %v; want active, by dana, rev 1", p, err)
	}
	wantEvents(t, c, seq, ev{OpPrincipalAdd, "alice", dana, `"state":"active"`})

	// With a project, the principal starts as its member, in one write.
	seq = controlSeq(t, c)
	if _, err := c.AddPrincipal(ctx, dana, "bob", projA); err != nil {
		t.Fatalf("AddPrincipal(bob, %s): %v", projA, err)
	}
	if r, ok, err := c.Role(ctx, "bob", projA); err != nil || !ok || r != RoleMember {
		t.Errorf("Role(bob, a) = %q, %v, %v; want member", r, ok, err)
	}
	wantEvents(t, c, seq, ev{OpPrincipalAdd, "bob", dana, `"name":"bob"`}, ev{OpMemberSet, projA + "/bob", dana, `"role":"member"`})

	seq = controlSeq(t, c)
	_, err = c.AddPrincipal(ctx, dana, "alice", "")
	var te *TakenError
	if !errors.As(err, &te) || te.Kind != TakenPrincipal || !errors.Is(err, ErrConflict) {
		t.Errorf("AddPrincipal(alice) again = %v, want a *TakenError for the principal name", err)
	}
	if _, err := c.AddPrincipal(ctx, dana, "zed", projC); !errors.Is(err, ErrNotFound) {
		t.Errorf("AddPrincipal into an unknown project = %v, want ErrNotFound", err)
	}
	if _, err := c.Principal(ctx, "zed"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Principal(zed) after a refused add = %v, want ErrNotFound: the add wrote nothing", err)
	}
	wantEvents(t, c, seq)

	seq = controlSeq(t, c)
	p, err = c.DisablePrincipal(ctx, dana, "alice")
	if err != nil || p.State != PrincipalDisabled || p.Rev != 2 {
		t.Errorf("DisablePrincipal(alice) = %+v, %v; want disabled at rev 2", p, err)
	}
	if again, err := c.DisablePrincipal(ctx, dana, "alice"); err != nil || again != p {
		t.Errorf("DisablePrincipal again = %+v, %v; want %+v unchanged", again, err, p)
	}
	wantEvents(t, c, seq, ev{OpPrincipalDisable, "alice", dana, `"state":"disabled"`})

	seq = controlSeq(t, c)
	p, err = c.EnablePrincipal(ctx, dana, "alice")
	if err != nil || p.State != PrincipalActive || p.Rev != 3 {
		t.Errorf("EnablePrincipal(alice) = %+v, %v; want active at rev 3", p, err)
	}
	if again, err := c.EnablePrincipal(ctx, dana, "alice"); err != nil || again != p {
		t.Errorf("EnablePrincipal again = %+v, %v; want %+v unchanged", again, err, p)
	}
	wantEvents(t, c, seq, ev{OpPrincipalEnable, "alice", dana, `"state":"active"`})

	if got, err := c.Principal(ctx, "alice"); err != nil || got != p {
		t.Errorf("Principal(alice) = %+v, %v; want %+v", got, err, p)
	}
	for name, run := range map[string]func() error{
		"DisablePrincipal": func() error { _, err := c.DisablePrincipal(ctx, dana, "zed"); return err },
		"EnablePrincipal":  func() error { _, err := c.EnablePrincipal(ctx, dana, "zed"); return err },
	} {
		if err := run(); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s(zed) = %v, want ErrNotFound", name, err)
		}
	}
	all, err := c.Principals(ctx)
	if err != nil || len(all) != 2 || all[0].Name != "alice" || all[1].Name != "bob" {
		t.Errorf("Principals() = %+v, %v; want alice then bob", all, err)
	}
}

func TestControlPrincipalNames(t *testing.T) {
	t.Parallel()
	c := newControl(t)
	bad := []string{"", "Alice", "9lives", "al ice", "alice\n", "al\x1bice", "-alice", strings.Repeat("a", 65), "al/ice", "álice"}
	bad = append(bad, ReservedPrincipals...)
	for _, name := range bad {
		if _, err := c.AddPrincipal(t.Context(), dana, name, ""); !errors.Is(err, ErrInvalid) {
			t.Errorf("AddPrincipal(%q) = %v, want ErrInvalid", name, err)
		}
		if _, err := c.Principal(t.Context(), name); !errors.Is(err, ErrInvalid) {
			t.Errorf("Principal(%q) = %v, want ErrInvalid", name, err)
		}
	}
	wantEvents(t, c, 0)
	for _, name := range []string{"a", "alice.smith", "ci-bot_2", strings.Repeat("a", 64)} {
		if _, err := c.AddPrincipal(t.Context(), dana, name, ""); err != nil {
			t.Errorf("AddPrincipal(%q): %v", name, err)
		}
	}
}

func TestControlKeys(t *testing.T) {
	t.Parallel()
	c := newControl(t)
	ctx := t.Context()
	mustPrincipal(t, c, "alice")
	mustPrincipal(t, c, "bob")
	line, fp := testKey(t, "alice-1")

	// The caller's spacing and comment do not reach the stored key.
	seq := controlSeq(t, c)
	k, err := c.AddKey(ctx, dana, "alice", "  "+strings.Replace(line, " ", "\t ", 1)+"   alice@laptop work \r\n")
	if err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	if k.Fingerprint != fp || k.Principal != "alice" || k.Key != line || k.Comment != "alice@laptop work" || k.AddedBy != "dana" || k.AddedAt.IsZero() {
		t.Errorf("AddKey = %+v; want fingerprint %s, key %q, comment %q, by dana", k, fp, line, "alice@laptop work")
	}
	wantEvents(t, c, seq, ev{OpKeyAdd, fp, dana, `"principal":"alice"`})

	// The same key again, whatever its comment, is the key already there.
	seq = controlSeq(t, c)
	if again, err := c.AddKey(ctx, dana, "alice", line+" another comment"); err != nil || again != k {
		t.Errorf("AddKey again = %+v, %v; want %+v unchanged", again, err, k)
	}
	_, err = c.AddKey(ctx, dana, "bob", line)
	var te *TakenError
	if !errors.As(err, &te) || te.Kind != TakenKey || !errors.Is(err, ErrConflict) {
		t.Errorf("AddKey of alice's key to bob = %v, want a *TakenError for the key", err)
	}
	if err != nil && strings.Contains(err.Error(), "alice") {
		t.Errorf("AddKey of alice's key to bob = %q, which names the principal who has it", err)
	}
	if _, err := c.AddKey(ctx, dana, "zed", line); !errors.Is(err, ErrNotFound) {
		t.Errorf("AddKey to an unknown principal = %v, want ErrNotFound", err)
	}
	wantEvents(t, c, seq)

	k2 := mustKey(t, c, "alice", "alice-2")
	keys, err := c.Keys(ctx, "alice")
	if err != nil || !slices.Equal(keys, []PrincipalKey{k, k2}) {
		t.Errorf("Keys(alice) = %+v, %v; want %+v then %+v", keys, err, k, k2)
	}
	if keys, err := c.Keys(ctx, "bob"); err != nil || len(keys) != 0 {
		t.Errorf("Keys(bob) = %+v, %v; want none", keys, err)
	}

	seq = controlSeq(t, c)
	gone, err := c.RemoveKey(ctx, dana, fp)
	if err != nil || gone != k {
		t.Errorf("RemoveKey(%s) = %+v, %v; want the key removed, %+v", fp, gone, err, k)
	}
	wantEvents(t, c, seq, ev{OpKeyRemove, fp, dana, ""})
	if _, err := c.RemoveKey(ctx, dana, fp); !errors.Is(err, ErrNotFound) {
		t.Errorf("RemoveKey again = %v, want ErrNotFound", err)
	}
	if _, err := c.RemoveKey(ctx, dana, "not a fingerprint"); !errors.Is(err, ErrInvalid) {
		t.Errorf("RemoveKey of no fingerprint = %v, want ErrInvalid", err)
	}
	// A removed key is free for another principal.
	if _, err := c.AddKey(ctx, dana, "bob", line); err != nil {
		t.Errorf("AddKey of a removed key to bob: %v", err)
	}
}

func TestControlKeyParsing(t *testing.T) {
	t.Parallel()
	c := newControl(t)
	mustPrincipal(t, c, "alice")
	line, _ := testKey(t, "k")
	other, _ := testKey(t, "other")
	typ, b64, _ := strings.Cut(line, " ")
	private := "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW\n-----END OPENSSH PRIVATE KEY-----\n"
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"blank", " \n"},
		{"garbage", "not a key at all"},
		{"type only", typ},
		{"base64 that is no key", typ + " AAAAB3NzaC1yc2E="},
		{"options", `command="sh" ` + line},
		{"restrict option", "restrict " + line},
		{"type that is not the key's", "ssh-rsa " + b64},
		{"two keys", line + "\n" + other},
		{"key then a comment line", line + "\n# trailing"},
		{"comment line then key", "# leading\n" + line},
		{"carriage return inside", line + " a\rb"},
		{"private key", private},
		{"private key on one line", strings.ReplaceAll(private, "\n", " ")},
		{"comment with an escape", line + " alice\x1b[2J"},
		{"comment with a nul", line + " alice\x00"},
		{"comment with a bidi override", line + " alice\u202e"},
		{"comment not utf-8", line + " alice\xff"},
		{"comment too long", line + " " + strings.Repeat("c", 256)},
		{"line too long", line + " " + strings.Repeat("c", 20000)},
	}
	cases = append(cases, struct {
		name string
		in   string
	}{"certificate", testCertificate(t)})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.AddKey(t.Context(), dana, "alice", tc.in)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("AddKey(%q) = %v, want ErrInvalid", tc.in, err)
			}
			// A refusal never repeats what it was given: it may be a secret.
			if strings.Contains(err.Error(), "b3BlbnNz") || strings.Contains(err.Error(), "cccc") {
				t.Errorf("AddKey(%q) = %q, which repeats its input", tc.in, err)
			}
		})
	}
	if keys, err := c.Keys(t.Context(), "alice"); err != nil || len(keys) != 0 {
		t.Errorf("Keys(alice) after refusals = %+v, %v; want none", keys, err)
	}
	for _, okLine := range []string{line, line + "\n", line + "\r\n", line + " " + strings.Repeat("c", 255)} {
		k, err := c.AddKey(t.Context(), dana, "alice", okLine)
		if err != nil || k.Key != line {
			t.Errorf("AddKey(%q) = %+v, %v; want key %q", okLine, k, err, line)
		}
	}
}

// TestAuthorizedKey checks the index sshd's lookup reads: it answers
// only for a key of an active principal, follows every write, and is
// loaded when the store opens.
func TestAuthorizedKey(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	c := openControl(t, dsn, ControlOptions{})
	ctx := t.Context()
	mustPrincipal(t, c, "alice")
	mustPrincipal(t, c, "bob")
	line, fp := testKey(t, "alice-1")
	_, unknown := testKey(t, "nobody")
	bobLine, bobFP := testKey(t, "bob-1")

	check := func(c *Control, when, fingerprint, wantPrincipal, wantKey string) {
		t.Helper()
		p, k, ok := c.AuthorizedKey(fingerprint)
		if p != wantPrincipal || k != wantKey || ok != (wantPrincipal != "") {
			t.Errorf("%s: AuthorizedKey(%s) = %q, %q, %v; want %q, %q, %v", when, fingerprint, p, k, ok, wantPrincipal, wantKey, wantPrincipal != "")
		}
	}
	check(c, "before any key", fp, "", "")
	if _, err := c.AddKey(ctx, dana, "alice", line+" a comment"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddKey(ctx, dana, "bob", bobLine); err != nil {
		t.Fatal(err)
	}
	check(c, "added", fp, "alice", line)
	check(c, "unknown key", unknown, "", "")
	check(c, "no fingerprint", "", "", "")
	check(c, "a second principal", bobFP, "bob", bobLine)

	if _, err := c.DisablePrincipal(ctx, dana, "alice"); err != nil {
		t.Fatal(err)
	}
	check(c, "principal disabled", fp, "", "")
	check(c, "another principal disabled", bobFP, "bob", bobLine)

	// A second store on the database loads the same answers.
	c2 := openControl(t, dsn, ControlOptions{})
	check(c2, "reopened, disabled", fp, "", "")
	check(c2, "reopened", bobFP, "bob", bobLine)

	if _, err := c.EnablePrincipal(ctx, dana, "alice"); err != nil {
		t.Fatal(err)
	}
	check(c, "principal enabled", fp, "alice", line)
	if _, err := c.RemoveKey(ctx, dana, fp); err != nil {
		t.Fatal(err)
	}
	check(c, "removed", fp, "", "")
	if _, err := c.AddKey(ctx, dana, "bob", line); err != nil {
		t.Fatal(err)
	}
	check(c, "added to another principal", fp, "bob", line)

	// A refused write leaves the index alone.
	if _, err := c.RemoveKey(ctx, alice, bobFP); !errors.Is(err, ErrForbidden) {
		t.Fatalf("RemoveKey by alice = %v, want ErrForbidden", err)
	}
	check(c, "a refused removal", bobFP, "bob", bobLine)

	// With the store closed nothing can reach the database, and the
	// lookup answers as before: it reads memory only.
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	check(c, "database closed", bobFP, "bob", bobLine)
	check(c, "database closed, unknown key", unknown, "", "")
}

// TestAuthorizedKeyConcurrent reads the index while writes change it;
// the race detector checks the locking.
func TestAuthorizedKeyConcurrent(t *testing.T) {
	t.Parallel()
	c := newControl(t)
	mustPrincipal(t, c, "alice")
	line, fp := testKey(t, "alice-1")
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				if p, k, ok := c.AuthorizedKey(fp); ok && (p != "alice" || k != line) {
					t.Errorf("AuthorizedKey(%s) = %q, %q; want alice's key or nothing", fp, p, k)
				}
			}
		}
	}()
	for range 5 {
		if _, err := c.AddKey(t.Context(), dana, "alice", line); err != nil {
			t.Errorf("AddKey: %v", err)
		}
		if _, err := c.DisablePrincipal(t.Context(), dana, "alice"); err != nil {
			t.Errorf("DisablePrincipal: %v", err)
		}
		if _, err := c.EnablePrincipal(t.Context(), dana, "alice"); err != nil {
			t.Errorf("EnablePrincipal: %v", err)
		}
		if _, err := c.RemoveKey(t.Context(), dana, fp); err != nil {
			t.Errorf("RemoveKey: %v", err)
		}
	}
	close(stop)
	<-done
}

// TestAuthorizedKeyAfterFailedWrite: a write that fails, rather than
// being refused, may have committed all the same, so the index is read
// again from the database instead of trusting what it held.
func TestAuthorizedKeyAfterFailedWrite(t *testing.T) {
	t.Parallel()
	c := newControl(t)
	mustPrincipal(t, c, "alice")
	_, fp := testKey(t, "alice-1")
	mustKey(t, c, "alice", "alice-1")
	// Another process removes the key behind this one's back.
	if _, err := c.s.w.ExecContext(t.Context(), `DELETE FROM principal_keys WHERE fingerprint = $1`, fp); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := c.AuthorizedKey(fp); !ok {
		t.Fatal("the index noticed a change made outside the store; this test cannot show the reload")
	}
	boom := errors.New("the connection dropped")
	c.s.beforeCommit = func(context.Context) error { return boom }
	if _, err := c.AddPrincipal(t.Context(), dana, "bob", ""); !errors.Is(err, boom) {
		t.Fatalf("AddPrincipal with a failing commit = %v, want %v", err, boom)
	}
	c.s.beforeCommit = nil
	if p, _, ok := c.AuthorizedKey(fp); ok {
		t.Errorf("AuthorizedKey(%s) after a failed write = %q, true; want the index reloaded, and the key gone", fp, p)
	}
}

func TestControlMembers(t *testing.T) {
	t.Parallel()
	c := newControl(t)
	ctx := t.Context()
	mustProject(t, c, projA)
	mustProject(t, c, projB)
	mustPrincipal(t, c, "alice")
	mustPrincipal(t, c, "bob")
	target := projA + "/alice"

	seq := controlSeq(t, c)
	m, err := c.SetMember(ctx, dana, "alice", projA, RoleMember)
	if err != nil || m.Principal != "alice" || m.Project != projA || m.Role != RoleMember || m.AddedBy != "dana" || m.Rev != 1 {
		t.Errorf("SetMember(alice, a, member) = %+v, %v; want a member, by dana, rev 1", m, err)
	}
	wantEvents(t, c, seq, ev{OpMemberSet, target, dana, `"role":"member"`})

	seq = controlSeq(t, c)
	if again, err := c.SetMember(ctx, dana, "alice", projA, RoleMember); err != nil || again != m {
		t.Errorf("SetMember again = %+v, %v; want %+v unchanged", again, err, m)
	}
	wantEvents(t, c, seq)

	m, err = c.SetMember(ctx, sam, "alice", projA, RoleAdmin)
	if err != nil || m.Role != RoleAdmin || m.Rev != 2 || m.AddedBy != "dana" {
		t.Errorf("SetMember(alice, a, admin) = %+v, %v; want admin at rev 2, still added by dana", m, err)
	}
	wantEvents(t, c, seq, ev{OpMemberSet, target, sam, `"role":"admin"`})
	if evs, err := c.Events(ctx, seq, 0); err != nil || len(evs) != 1 || !strings.Contains(string(evs[0].Before), `"role":"member"`) {
		t.Errorf("the role change's event = %+v, %v; want a before state with the old role", evs, err)
	}

	for _, tc := range []struct {
		principal, project string
		want               Role
		ok                 bool
	}{
		{"alice", projA, RoleAdmin, true},
		{"alice", projB, "", false},
		{"bob", projA, "", false},
		{"zed", projA, "", false},
		{"alice", projC, "", false},
	} {
		if r, ok, err := c.Role(ctx, tc.principal, tc.project); err != nil || r != tc.want || ok != tc.ok {
			t.Errorf("Role(%s, %s) = %q, %v, %v; want %q, %v", tc.principal, tc.project, r, ok, err, tc.want, tc.ok)
		}
	}

	if _, err := c.SetMember(ctx, dana, "alice", projB, RoleMember); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetMember(ctx, dana, "bob", projA, RoleMember); err != nil {
		t.Fatal(err)
	}
	ms, err := c.Members(ctx, projA)
	if err != nil || len(ms) != 2 || ms[0] != m || ms[1].Principal != "bob" {
		t.Errorf("Members(a) = %+v, %v; want alice (%+v) then bob", ms, err, m)
	}
	of, err := c.ProjectsOf(ctx, "alice")
	if err != nil || len(of) != 2 || of[0] != m || of[1].Project != projB || of[1].Role != RoleMember {
		t.Errorf("ProjectsOf(alice) = %+v, %v; want a as admin then b as member", of, err)
	}
	if of, err := c.ProjectsOf(ctx, "zed"); err != nil || len(of) != 0 {
		t.Errorf("ProjectsOf(zed) = %+v, %v; want none", of, err)
	}

	refused := []struct {
		name string
		run  func() error
		want error
	}{
		{"unknown principal", func() error { _, err := c.SetMember(ctx, dana, "zed", projA, RoleMember); return err }, ErrNotFound},
		{"unknown project", func() error { _, err := c.SetMember(ctx, dana, "bob", projC, RoleMember); return err }, ErrNotFound},
		{"no role", func() error { _, err := c.SetMember(ctx, dana, "bob", projA, ""); return err }, ErrInvalid},
		{"unknown role", func() error { _, err := c.SetMember(ctx, dana, "bob", projA, "owner"); return err }, ErrInvalid},
		{"bad principal", func() error { _, err := c.SetMember(ctx, dana, "Bob", projA, RoleMember); return err }, ErrInvalid},
		{"bad project", func() error { _, err := c.SetMember(ctx, dana, "bob", "a", RoleMember); return err }, ErrInvalid},
		{"remove a non-member", func() error { _, err := c.RemoveMember(ctx, dana, "bob", projB); return err }, ErrNotFound},
		{"remove from an unknown project", func() error { _, err := c.RemoveMember(ctx, dana, "bob", projC); return err }, ErrNotFound},
	}
	seq = controlSeq(t, c)
	for _, tc := range refused {
		if err := tc.run(); !errors.Is(err, tc.want) {
			t.Errorf("%s = %v, want %v", tc.name, err, tc.want)
		}
	}
	wantEvents(t, c, seq)

	// Only an active principal is given a role; one that has a role and
	// is disabled keeps it, and can be removed.
	if _, err := c.DisablePrincipal(ctx, dana, "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetMember(ctx, dana, "bob", projB, RoleMember); !errors.Is(err, ErrInvalid) {
		t.Errorf("SetMember of a disabled principal = %v, want ErrInvalid", err)
	}
	seq = controlSeq(t, c)
	gone, err := c.RemoveMember(ctx, dana, "bob", projA)
	if err != nil || gone.Principal != "bob" || gone.Project != projA || gone.Role != RoleMember {
		t.Errorf("RemoveMember(bob, a) = %+v, %v; want the membership removed", gone, err)
	}
	wantEvents(t, c, seq, ev{OpMemberRemove, projA + "/bob", dana, ""})
	if _, ok, err := c.Role(ctx, "bob", projA); err != nil || ok {
		t.Errorf("Role(bob, a) after removal = %v, %v; want none", ok, err)
	}
}

func TestControlAdmins(t *testing.T) {
	t.Parallel()
	c := newControl(t)
	ctx := t.Context()
	mustProject(t, c, projA)
	mustProject(t, c, projB)
	mustPrincipal(t, c, "alice", projA+"=admin", projB+"=member")
	mustPrincipal(t, c, "bob", projA+"=member")
	for _, tc := range []struct {
		principal    string
		server, a, b bool
	}{
		{"dana", true, true, true},
		{"sam", true, true, true},
		{"alice", false, true, false},
		{"bob", false, false, false},
		{"zed", false, false, false},
	} {
		if got := c.IsServerAdmin(tc.principal); got != tc.server {
			t.Errorf("IsServerAdmin(%s) = %v, want %v", tc.principal, got, tc.server)
		}
		for project, want := range map[string]bool{projA: tc.a, projB: tc.b} {
			if got, err := c.IsAdminOf(ctx, tc.principal, project); err != nil || got != want {
				t.Errorf("IsAdminOf(%s, %s) = %v, %v; want %v", tc.principal, project, got, err, want)
			}
		}
	}
	// A disabled principal administers nothing.
	if _, err := c.DisablePrincipal(ctx, dana, "alice"); err != nil {
		t.Fatal(err)
	}
	if got, err := c.IsAdminOf(ctx, "alice", projA); err != nil || got {
		t.Errorf("IsAdminOf(alice, a) once disabled = %v, %v; want false", got, err)
	}
}

func TestOpenControl(t *testing.T) {
	t.Parallel()
	t.Run("refuses bad options", func(t *testing.T) {
		dsn := newDSN(t)
		for name, opts := range map[string]ControlOptions{
			"reserved admin":  {Admins: []string{ReaperActor.Principal}},
			"admin no name":   {Admins: []string{"Dana"}},
			"negative limit":  {Limits: ControlLimits{Projects: -1}},
			"negative limit2": {Limits: ControlLimits{Keys: -1}},
			"short invites":   {InviteTTL: MinInviteTTL - time.Second},
			"long invites":    {InviteTTL: MaxInviteTTL + time.Second},
		} {
			if _, err := OpenControl(t.Context(), dsn, opts); !errors.Is(err, ErrInvalid) {
				t.Errorf("OpenControl with %s = %v, want ErrInvalid", name, err)
			}
		}
		if _, err := OpenControl(t.Context(), withoutDatabase(t, dsn), ControlOptions{}); !errors.Is(err, ErrInvalid) {
			t.Errorf("OpenControl with no database = %v, want ErrInvalid", err)
		}
	})

	// The account check is the project store's: the superuser is refused.
	t.Run("refuses an unsafe account", func(t *testing.T) {
		dsn := newDSN(t)
		_, err := OpenControl(t.Context(), superuserDSN(t, dsn), ControlOptions{})
		var ua *UnsafeAccountError
		if !errors.As(err, &ua) {
			t.Fatalf("OpenControl as the superuser = %v, want an *UnsafeAccountError", err)
		}
		c, err := OpenControl(t.Context(), superuserDSN(t, dsn), ControlOptions{AllowUnsafeAccount: true})
		if err != nil {
			t.Fatalf("OpenControl as the superuser with AllowUnsafeAccount: %v", err)
		}
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})

	// A database this code migrated opens again, as the least-privileged
	// account, with its rows; and a newer one is refused.
	t.Run("reopens", func(t *testing.T) {
		dsn := newDSN(t)
		c := openControl(t, dsn, ControlOptions{})
		mustProject(t, c, projA)
		if n, err := migrate(t.Context(), c.s.w, c.s.now(), controlSchema, true); err != nil || n != 0 {
			t.Errorf("second migrate ran %d (err %v), want 0", n, err)
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		c = openControl(t, dsn, ControlOptions{})
		if p, err := c.Project(t.Context(), projA); err != nil || p.State != ProjectActive {
			t.Errorf("Project(a) after reopening = %+v, %v; want it active", p, err)
		}
		if _, err := c.s.w.ExecContext(t.Context(), `INSERT INTO control_migrations VALUES (99, 'future', NOW())`); err != nil {
			t.Fatal(err)
		}
		if _, err := testDB(t, dsn).OpenControl(t.Context(), DefaultSchema, ControlOptions{}); !errors.Is(err, ErrSchemaTooNew) {
			t.Errorf("OpenControl of a newer schema = %v, want ErrSchemaTooNew", err)
		}
	})
}

// TestControlAndProjectDatabases: each kind of store refuses the other's
// database before changing it, so two DSNs swapped in a config file do
// not run one schema's migrations over the other's tables.
func TestControlAndProjectDatabases(t *testing.T) {
	t.Parallel()
	tables := func(t *testing.T, dsn string) []string {
		t.Helper()
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		rows, err := db.QueryContext(t.Context(), `SELECT tablename FROM pg_tables WHERE schemaname = current_schema()`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		var out []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
			out = append(out, n)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		slices.Sort(out)
		return out
	}

	t.Run("project store on the control database", func(t *testing.T) {
		dsn := newDSN(t)
		c := openControl(t, dsn, ControlOptions{})
		mustPrincipal(t, c, "alice")
		before := tables(t, dsn)
		_, err := testDB(t, dsn).Open(t.Context(), DefaultSchema, Options{})
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "control_migrations") {
			t.Errorf("Open on the control database = %v, want ErrInvalid naming control_migrations", err)
		}
		if after := tables(t, dsn); !slices.Equal(after, before) {
			t.Errorf("tables after the refused Open = %v, want %v unchanged", after, before)
		}
		if _, err := c.AddPrincipal(t.Context(), dana, "bob", ""); err != nil {
			t.Errorf("the control store after the refused Open: %v", err)
		}
	})

	t.Run("control store on a project database", func(t *testing.T) {
		dsn := newDSN(t)
		s := openStore(t, dsn, Options{})
		mustCreate(t, s, NewIssue{})
		before := tables(t, dsn)
		_, err := testDB(t, dsn).OpenControl(t.Context(), DefaultSchema, ControlOptions{})
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "schema_migrations") {
			t.Errorf("OpenControl on a project database = %v, want ErrInvalid naming schema_migrations", err)
		}
		if after := tables(t, dsn); !slices.Equal(after, before) {
			t.Errorf("tables after the refused OpenControl = %v, want %v unchanged", after, before)
		}
		mustCreate(t, s, NewIssue{})
	})

	// A project's store and the control store work side by side on one
	// Postgres server, each in its own database.
	t.Run("side by side", func(t *testing.T) {
		s := newStore(t)
		c := newControl(t)
		mustPrincipal(t, c, "alice")
		is := mustCreate(t, s, NewIssue{Title: "beside the control store"})
		if got, err := s.GetIssue(t.Context(), is.ID); err != nil || got.Title != is.Title {
			t.Errorf("GetIssue beside a control store = %+v, %v", got, err)
		}
		if n := lastSeq(t, s); n != 1 {
			t.Errorf("the project's last event seq = %d, want 1: the control store's events are its own", n)
		}
	})
}

// TestControlEventStates: a key's events hold the whole key, so the log
// alone says which key was let in and which was taken out; and rows and
// events carry the store's clock.
func TestControlEventStates(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 11, 9, 30, 0, 123456000, time.UTC)
	c := openControl(t, newDSN(t), ControlOptions{Now: func() time.Time { return at }})
	mustPrincipal(t, c, "alice")
	k := mustKey(t, c, "alice", "alice-1")
	if _, err := c.RemoveKey(t.Context(), dana, k.Fingerprint); err != nil {
		t.Fatal(err)
	}
	evs, err := c.Events(t.Context(), 0, 0)
	if err != nil || len(evs) != 3 {
		t.Fatalf("Events = %+v, %v; want 3", evs, err)
	}
	var added, removed PrincipalKey
	if err := json.Unmarshal(evs[1].After, &added); err != nil || added != k {
		t.Errorf("key.add after state = %s (%v), want %+v", evs[1].After, err, k)
	}
	if err := json.Unmarshal(evs[2].Before, &removed); err != nil || removed != k || evs[2].After != nil {
		t.Errorf("key.remove states = %s, %s (%v); want before %+v and no after", evs[2].Before, evs[2].After, err, k)
	}
	if !k.AddedAt.Equal(at) || !evs[1].At.Equal(at) || !c.Now().Equal(at) {
		t.Errorf("key added at %v, its event at %v, Now() %v; want all %v", k.AddedAt, evs[1].At, c.Now(), at)
	}
}

// FuzzParseKey: whatever the line, parseKey does not panic, and what it
// accepts is safe to store and to write into an authorized_keys line: a
// type and base64 that parse back to the same key, and a comment of one
// safe line.
func FuzzParseKey(f *testing.F) {
	sum := sha256.Sum256([]byte("fuzz"))
	pub, err := ssh.NewPublicKey(ed25519.NewKeyFromSeed(sum[:]).Public())
	if err != nil {
		f.Fatal(err)
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
	for _, seed := range []string{line, line + " alice@laptop\n", "restrict " + line, `command="sh" ` + line, line + "\n" + line,
		"# comment\n" + line, line + " a\x1bb", "ssh-rsa AAAA", "", "\n", "-----BEGIN OPENSSH PRIVATE KEY-----"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		fp, key, comment, err := parseKey(in)
		if err != nil {
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("parseKey(%q) = %v, want an error wrapping ErrInvalid", in, err)
			}
			return
		}
		if !fingerprintPattern.MatchString(fp) {
			t.Errorf("parseKey(%q) fingerprint = %q, want the SHA256: form", in, fp)
		}
		if fields := strings.Split(key, " "); len(fields) != 2 || strings.ContainsAny(key, "\t\r\n\"") || !utf8.ValidString(key) {
			t.Errorf("parseKey(%q) key = %q, want a type, one space and base64", in, key)
		}
		again, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(key))
		if err != nil || len(options) != 0 || len(rest) != 0 || ssh.FingerprintSHA256(again) != fp {
			t.Errorf("parseKey(%q) key = %q, which parses back as %v with options %q and rest %q; want the key of %s", in, key, err, options, rest, fp)
		}
		if checkLine("comment", comment, maxKeyComment, false) != nil {
			t.Errorf("parseKey(%q) comment = %q, want one safe line of at most %d bytes", in, comment, maxKeyComment)
		}
	})
}

// TestControlConcurrentWriters: several stores on one control database,
// as several processes would be, write at once. A write that loses is
// rerun from the start (rule 8), so every one lands, once, on a gapless
// event log, and each store's index has what it wrote.
func TestControlConcurrentWriters(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	cs := make([]*Control, raceStores)
	for i := range cs {
		cs[i] = openControl(t, dsn, ControlOptions{MaxAttempts: 50})
	}
	type writer struct{ name, line, fp string }
	ws := make([]writer, raceWriters)
	for w := range ws {
		ws[w].name = fmt.Sprintf("agent%d", w)
		ws[w].line, ws[w].fp = testKey(t, ws[w].name)
	}
	errs := make([]error, raceWriters)
	var wg sync.WaitGroup
	for w := range raceWriters {
		wg.Go(func() {
			c := cs[w%raceStores]
			if _, errs[w] = c.AddPrincipal(t.Context(), dana, ws[w].name, ""); errs[w] == nil {
				_, errs[w] = c.AddKey(t.Context(), dana, ws[w].name, ws[w].line)
			}
		})
	}
	wg.Wait()
	for w, err := range errs {
		if err != nil {
			t.Errorf("writer %d: %v, want its principal and key to land", w, err)
		}
	}
	evs, err := cs[0].Events(t.Context(), 0, 0)
	if err != nil || len(evs) != 2*raceWriters {
		t.Fatalf("Events = %d, %v; want %d", len(evs), err, 2*raceWriters)
	}
	for i, e := range evs {
		if e.Seq != int64(i+1) {
			t.Errorf("event %d has seq %d, want a gapless log", i, e.Seq)
		}
	}
	ps, err := cs[0].Principals(t.Context())
	if err != nil || len(ps) != raceWriters {
		t.Errorf("Principals() = %+v, %v; want the %d writers, once each", ps, err, raceWriters)
	}
	for w := range raceWriters {
		if p, _, ok := cs[w%raceStores].AuthorizedKey(ws[w].fp); !ok || p != ws[w].name {
			t.Errorf("AuthorizedKey(%s's key) on the store that added it = %q, %v", ws[w].name, p, ok)
		}
	}
}

// withoutDatabase is dsn naming no database.
func withoutDatabase(t *testing.T, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = ""
	return u.String()
}
