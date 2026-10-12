package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/server"
	"github.com/ariesworx/starfix/internal/store"
	"github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/ssh"
)

// Invented projects.
const (
	adopted = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	second  = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

// site is one server's settings for the tests of a server of many
// projects: its project database, its control database, and a config
// file naming dana as the server admin.
type site struct {
	dsn       string // the adopted project's database
	db        string // its name
	controlDB string
	cfg       string // the config file
	keysFile  string // the file the daemon writes the store's keys to
	env       map[string]string
}

// newSite makes a project database and a control database, both
// granted, and a config file for them. extra is appended to the file.
func newSite(t *testing.T, extra string) *site {
	t.Helper()
	s := &site{dsn: newDSN(t)}
	s.db = dbName(t, s.dsn)
	s.controlDB = dbName(t, newDSN(t))
	s.cfg = filepath.Join(t.TempDir(), "starfixd.yaml")
	body := fmt.Sprintf("project: %s\nprefix: acme\naccount: acme-2026\ncontrol_db: %s\nadmins: [dana]\n%s", adopted, s.controlDB, extra)
	if err := os.WriteFile(s.cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// The keys file and the hand-maintained one are the site's own, never
	// those of whoever runs the tests.
	sshDir := filepath.Join(t.TempDir(), ".ssh")
	if err := os.Mkdir(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	s.keysFile = filepath.Join(sshDir, "starfix_keys")
	s.env = map[string]string{server.EnvDSN: s.dsn, server.EnvKeysFile: s.keysFile,
		server.EnvAuthorizedKeys: filepath.Join(sshDir, "authorized_keys")}
	return s
}

func dbName(t *testing.T, dsn string) string {
	t.Helper()
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.DBName
}

// run runs an admin command in process with the site's config file and
// environment.
func (s *site) run(t *testing.T, stdin string, cmd func(context.Context, adminEnv, []string) error, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	env := adminEnv{stdin: strings.NewReader(stdin), stdout: &out, stderr: &errb, getenv: func(k string) string { return s.env[k] }}
	// Flags come before a command's file, and after `control`'s init.
	at := 0
	if len(args) > 0 && args[0] == "init" {
		at = 1
	}
	err := cmd(t.Context(), env, slices.Insert(slices.Clone(args), at, "--config", s.cfg))
	return result{out.String(), errb.String(), err}
}

// settings resolves the site's settings as serve does.
func (s *site) settings(t *testing.T) server.Settings {
	t.Helper()
	out, err := server.ResolveSettings(server.Settings{}, s.cfg, func(k string) string { return s.env[k] })
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// control opens the site's control store as dana would administer it.
func (s *site) control(t *testing.T) *store.Control {
	t.Helper()
	dsn, err := s.settings(t).ControlDSN()
	if err != nil {
		t.Fatal(err)
	}
	c, err := store.OpenControl(t.Context(), dsn, store.ControlOptions{Admins: []string{"dana"}, CommitInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// pubKey returns an invented public key in authorized_keys form, the
// same for the same seed, and its fingerprint.
func pubKey(t *testing.T, seed string) (line, fingerprint string) {
	t.Helper()
	sum := sha256.Sum256([]byte(seed))
	pub, err := ssh.NewPublicKey(ed25519.NewKeyFromSeed(sum[:]).Public())
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), ssh.FingerprintSHA256(pub)
}

// forced is an authorized_keys line as the server guide writes one.
func forced(t *testing.T, principal, seed string) string {
	t.Helper()
	key, _ := pubKey(t, seed)
	return fmt.Sprintf(`restrict,command="/usr/local/bin/starfixd stdio --principal %s" %s %s@example.com`, principal, key, principal)
}

func writeKeys(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "authorized_keys")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// serveSite runs the daemon the site configures on a socket of its own
// until the test ends, and returns the socket.
func serveSite(t *testing.T, s *site) string {
	t.Helper()
	d, err := openDaemon(t.Context(), s.settings(t), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("open daemon: %v", err)
	}
	// Not t.TempDir, whose path can be too long for a unix socket.
	dir, err := os.MkdirTemp("", "sfc")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "run", "d.sock")
	l, err := server.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.srv.Serve(ctx, l) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
		if err := d.Close(); err != nil {
			t.Errorf("close daemon: %v", err)
		}
		_ = os.RemoveAll(dir)
	})
	return sock
}

// hello connects to a daemon's socket as the bridge would for principal
// and returns the welcome, which may be a refusal.
func hello(t *testing.T, sock, principal, project string) *proto.Frame {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	enc := proto.NewEncoder(c)
	// A daemon that refuses may answer and close before it has read
	// both frames, and the write of the second then fails: what it
	// answered is still there to read, so a failed write only counts
	// when there is no answer.
	var werr error
	for _, f := range []*proto.Frame{
		{T: proto.FrameBridge, Principal: principal},
		{T: proto.FrameHello, Proto: proto.Proto, Project: project, Session: "s-" + principal, Machine: "laptop"},
	} {
		if werr = enc.Encode(f); werr != nil {
			break
		}
	}
	w, err := proto.NewDecoder(c).Decode()
	if err != nil {
		t.Fatalf("handshake as %s: %v", principal, errors.Join(werr, err))
	}
	return w
}

func TestControlInit(t *testing.T) {
	s := newSite(t, "")
	aliceKey, aliceFP := pubKey(t, "alice-1")
	plain, _ := pubKey(t, "nobody")
	keys := writeKeys(t,
		"# the starfix account's keys", // 1
		forced(t, "alice", "alice-1"),  // 2
		"",                             // 3
		`command="starfixd stdio --principal=alice",no-pty `+mustKeyOnly(t, "alice-2"), // 4: another form of the command
		forced(t, "bob", "bob-1"),                                              // 5
		forced(t, "dana", "dana-1"),                                            // 6: the config's admin
		plain+" a key with no forced command",                                  // 7
		`restrict,command="/usr/bin/rsync --server" `+mustKeyOnly(t, "backup"), // 8
		"this is not a key at all",                                             // 9
		forced(t, "import", "reserved"),                                        // 10
		forced(t, "Bob", "upper"),                                              // 11
		`restrict,command="/usr/local/bin/starfixd stdio --principal bob" `+aliceKey+" stolen",   // 12: alice's key again, for bob
		`restrict,command="/usr/local/bin/starfixd stdio --principal" `+mustKeyOnly(t, "noname"), // 13
	)

	got := s.run(t, "", controlCmd, "init", "--name", "acme", "--authorized-keys", keys)
	if got.err != nil {
		t.Fatalf("control init: %v\n%s", got.err, got.stderr)
	}
	wantOut := fmt.Sprintf("project acme (%s) adopted from database %s: active\n", adopted, s.db) +
		"principals: 3 added, 0 already there; keys: 4 added, 0 already there; members: 2 added, 0 already there; 7 lines skipped\n" +
		"4 keys in the store are also lines of " + keys + ": sshd reads that file too, so `sfx admin key remove` does not keep such a key out until its line is deleted too\n"
	if got.stdout != wantOut {
		t.Errorf("stdout = %q\nwant     %q", got.stdout, wantOut)
	}
	for _, want := range []string{
		"authorized_keys line 7: no `starfixd stdio --principal NAME` forced command; skipped\n",
		"authorized_keys line 8: no `starfixd stdio --principal NAME` forced command; skipped\n",
		"authorized_keys line 9: not a public key line; skipped\n",
		"authorized_keys line 10: principal \"import\" is a name the server uses itself; skipped\n",
		"authorized_keys line 11: principal \"Bob\" must be",
		"authorized_keys line 12: key " + aliceFP + " already belongs to another principal; skipped\n",
		"authorized_keys line 13: no `starfixd stdio --principal NAME` forced command; skipped\n",
	} {
		if !strings.Contains(got.stderr, want) {
			t.Errorf("stderr has no %q:\n%s", want, got.stderr)
		}
	}
	// No line is echoed: neither a key nor the text that was no key.
	for _, secret := range []string{"AAAA", "this is not a key", "rsync"} {
		if strings.Contains(got.stderr+got.stdout, secret) {
			t.Errorf("output repeats part of a line (%q):\n%s%s", secret, got.stdout, got.stderr)
		}
	}

	c := s.control(t)
	p, err := c.Project(t.Context(), adopted)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "acme" || p.Prefix != "acme" || p.Account != "acme-2026" || p.DBName != s.db || p.State != store.ProjectActive ||
		p.CreatedBy != store.OperatorPrincipal {
		t.Errorf("adopted project = %+v; want the config's prefix and account, the DSN's database, active, created by the operator", p)
	}
	for name, want := range map[string]struct {
		keys int
		role store.Role
	}{"alice": {2, store.RoleMember}, "bob": {1, store.RoleMember}, "dana": {1, ""}} {
		ks, err := c.Keys(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		role, _, err := c.Role(t.Context(), name, adopted)
		if err != nil {
			t.Fatal(err)
		}
		if len(ks) != want.keys || role != want.role {
			t.Errorf("%s has %d keys and role %q in the project; want %d and %q", name, len(ks), role, want.keys, want.role)
		}
	}
	if who, key, ok := c.AuthorizedKey(aliceFP); !ok || who != "alice" || key != aliceKey {
		t.Errorf("AuthorizedKey(alice's) = %q, %q, %v; want alice and the key without options or comment", who, key, ok)
	}
	evs, err := c.Events(t.Context(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// project.add, project.activate, 3 principals, 4 keys, 2 members.
	if len(evs) != 11 {
		t.Errorf("%d events recorded, want 11", len(evs))
	}
	for _, e := range evs {
		if e.Actor.Principal != store.OperatorPrincipal || !strings.HasPrefix(e.Actor.Session, "control-init-") {
			t.Errorf("event %s on %s by %+v, want the operator's control-init session", e.Op, e.Target, e.Actor)
		}
	}

	t.Run("again changes nothing", func(t *testing.T) {
		got := s.run(t, "", controlCmd, "init", "--name", "acme", "--authorized-keys", keys)
		want := fmt.Sprintf("project acme (%s) adopted from database %s: active\n", adopted, s.db) +
			"principals: 0 added, 3 already there; keys: 0 added, 4 already there; members: 0 added, 2 already there; 7 lines skipped\n" +
			"4 keys in the store are also lines of " + keys + ": sshd reads that file too, so `sfx admin key remove` does not keep such a key out until its line is deleted too\n"
		if got.err != nil || got.stdout != want {
			t.Errorf("second run = %q, %v\nwant       %q", got.stdout, got.err, want)
		}
		c := s.control(t)
		if after, err := c.Events(t.Context(), 0, 0); err != nil || len(after) != len(evs) {
			t.Errorf("after a second run there are %d events (%v), want %d as before", len(after), err, len(evs))
		}
	})

	t.Run("a new line is added, and roles and states are kept", func(t *testing.T) {
		c := s.control(t)
		dana := store.Actor{Principal: "dana", Session: "s", Machine: "m"}
		if _, err := c.SetMember(t.Context(), dana, "alice", adopted, store.RoleAdmin); err != nil {
			t.Fatal(err)
		}
		if _, err := c.ArchiveProject(t.Context(), dana, adopted); err != nil {
			t.Fatal(err)
		}
		more := writeKeys(t, forced(t, "alice", "alice-1"), forced(t, "carol", "carol-1"))
		got := s.run(t, "", controlCmd, "init", "--name", "acme", "--authorized-keys", more)
		want := fmt.Sprintf("project acme (%s) adopted from database %s: archived\n", adopted, s.db) +
			"principals: 1 added, 1 already there; keys: 1 added, 1 already there; members: 1 added, 1 already there; 0 lines skipped\n" +
			"2 keys in the store are also lines of " + more + ": sshd reads that file too, so `sfx admin key remove` does not keep such a key out until its line is deleted too\n"
		if got.err != nil || got.stdout != want || got.stderr != "" {
			t.Errorf("run with a new line = %q, %v, stderr %q\nwant %q", got.stdout, got.err, got.stderr, want)
		}
		c = s.control(t)
		if role, _, err := c.Role(t.Context(), "alice", adopted); err != nil || role != store.RoleAdmin {
			t.Errorf("alice's role after a rerun = %q, %v; want admin kept", role, err)
		}
	})

	t.Run("other settings for the same project are refused", func(t *testing.T) {
		got := s.run(t, "", controlCmd, "init", "--name", "renamed", "--authorized-keys", keys)
		if got.err == nil || !strings.Contains(got.err.Error(), "already exists with another name") || strings.Count(got.err.Error(), "fix: ") != 1 {
			t.Errorf("control init with another name = %v, want a refusal with one fix", got.err)
		}
	})
}

// TestControlInitLimits checks that the limits: settings reach the
// control store: a line past a cap is skipped, and says which.
func TestControlInitLimits(t *testing.T) {
	s := newSite(t, "limits:\n  principals: 2\n  keys_per_principal: 1\n")
	keys := writeKeys(t,
		forced(t, "alice", "alice-1"),
		forced(t, "alice", "alice-2"), // 2: her second key, past keys_per_principal
		forced(t, "bob", "bob-1"),
		forced(t, "carol", "carol-1"), // 4: a third principal, past principals
	)
	got := s.run(t, "", controlCmd, "init", "--name", "acme", "--authorized-keys", keys)
	if got.err != nil {
		t.Fatalf("control init: %v\n%s", got.err, got.stderr)
	}
	// The two keys that were stored are counted as still in the file; the
	// two lines skipped are not.
	if want := "principals: 2 added, 0 already there; keys: 2 added, 0 already there; members: 2 added, 0 already there; 2 lines skipped\n" +
		"2 keys in the store are also lines of " + keys + ": sshd reads that file too, so `sfx admin key remove` does not keep such a key out until its line is deleted too\n"; !strings.HasSuffix(got.stdout, want) {
		t.Errorf("stdout = %q, want it to end %q", got.stdout, want)
	}
	for _, want := range []string{
		"authorized_keys line 2: a principal keeps at most 1 keys; skipped\n",
		"authorized_keys line 4: the server keeps at most 2 principals; skipped\n",
	} {
		if !strings.Contains(got.stderr, want) {
			t.Errorf("stderr has no %q:\n%s", want, got.stderr)
		}
	}
}

// mustKeyOnly is a key line with no comment.
func mustKeyOnly(t *testing.T, seed string) string {
	t.Helper()
	k, _ := pubKey(t, seed)
	return k
}

func TestControlInitRefusals(t *testing.T) {
	keys := writeKeys(t, forced(t, "alice", "alice-1"))
	tests := []struct {
		name  string
		edit  func(t *testing.T, s *site)
		args  []string
		usage bool
		want  []string
	}{
		{name: "no command", args: []string{}, usage: true, want: []string{"control takes one command: init"}},
		{name: "unknown command", args: []string{"reset"}, usage: true, want: []string{"control takes one command: init"}},
		{name: "no name", args: []string{"init", "--authorized-keys", keys}, usage: true, want: []string{"needs --name"}},
		{name: "arguments", args: []string{"init", "--name", "acme", "extra"}, usage: true, want: []string{"takes no arguments"}},
		{name: "name not a code name", args: []string{"init", "--name", "Acme Corp", "--authorized-keys", keys},
			want: []string{`project name "Acme Corp" must be a code name`, "fix: "}},
		{name: "no control_db", args: []string{"init", "--name", "acme", "--authorized-keys", keys},
			edit: func(t *testing.T, s *site) { rewrite(t, s, "project: "+adopted+"\n") },
			want: []string{"no control database; fix: set control_db:"}},
		{name: "no project", args: []string{"init", "--name", "acme", "--authorized-keys", keys},
			edit: func(t *testing.T, s *site) { rewrite(t, s, "control_db: "+s.controlDB+"\n") },
			want: []string{"no project to adopt; fix: set project:"}},
		{name: "no authorized_keys file", args: []string{"init", "--name", "acme", "--authorized-keys", filepath.Join(t.TempDir(), "none")},
			want: []string{"read authorized_keys:", "fix: give the file sshd reads for the starfix account with --authorized-keys FILE"}},
		{name: "control database never created", args: []string{"init", "--name", "acme", "--authorized-keys", keys},
			edit: func(t *testing.T, s *site) {
				rewrite(t, s, "project: "+adopted+"\ncontrol_db: starfix_control_missing\n")
			},
			want: []string{"open the control database starfix_control_missing:", "fix: ", "as Dolt's root",
				"CREATE DATABASE IF NOT EXISTS starfix_control_missing;",
				"GRANT ALL ON starfix_control_missing.* TO 'starfix'@'localhost';"}},
		{name: "control database is a project's", args: []string{"init", "--name", "acme", "--authorized-keys", keys},
			edit: func(t *testing.T, s *site) {
				// Another project's database, migrated as one.
				other := newDSN(t)
				st, err := store.Open(t.Context(), other, store.Options{CommitInterval: -1})
				if err != nil {
					t.Fatal(err)
				}
				if err := st.Close(); err != nil {
					t.Fatal(err)
				}
				rewrite(t, s, "project: "+adopted+"\ncontrol_db: "+dbName(t, other)+"\n")
			},
			want: []string{"open the control database", "does not hold the control store", "fix: check control_db:"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newSite(t, "")
			if tc.edit != nil {
				tc.edit(t, s)
			}
			got := s.run(t, "", controlCmd, tc.args...)
			if got.err == nil {
				t.Fatalf("control %v succeeded: %q", tc.args, got.stdout)
			}
			for _, want := range tc.want {
				if !strings.Contains(got.err.Error(), want) {
					t.Errorf("control %v = %v\nwant it to contain %q", tc.args, got.err, want)
				}
			}
			if _, usage := errors.AsType[usageError](got.err); usage != tc.usage {
				t.Errorf("control %v = %v; usage error %v, want %v", tc.args, got.err, usage, tc.usage)
			}
			if !tc.usage && strings.Count(got.err.Error(), "fix: ") != 1 {
				t.Errorf("refusal %q has %d fixes, want one", got.err, strings.Count(got.err.Error(), "fix: "))
			}
			if got.stdout != "" {
				t.Errorf("a refused run printed %q", got.stdout)
			}
		})
	}
}

// rewrite replaces the site's config file.
func rewrite(t *testing.T, s *site, body string) {
	t.Helper()
	if err := os.WriteFile(s.cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestServeManyProjects runs the daemon as serve builds it, from the
// settings alone.
func TestServeManyProjects(t *testing.T) {
	s := newSite(t, "")

	// Before control init there is nothing to serve.
	_, err := openDaemon(t.Context(), s.settings(t), slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "holds no active project") ||
		!strings.Contains(err.Error(), "fix: adopt this server's database as the first with `starfixd control init --name NAME`") {
		t.Fatalf("serve before control init = %v, want a refusal naming control init", err)
	}

	keys := writeKeys(t, forced(t, "alice", "alice-1"), forced(t, "dana", "dana-1"))
	if got := s.run(t, "", controlCmd, "init", "--name", "acme", "--authorized-keys", keys); got.err != nil {
		t.Fatalf("control init: %v\n%s", got.err, got.stderr)
	}
	sock := serveSite(t, s)
	tests := []struct {
		name, principal, project string
		code                     proto.Code
	}{
		{"an imported member", "alice", adopted, ""},
		{"the config's admin, a member of nothing", "dana", adopted, ""},
		{"a principal never imported", "bob", adopted, proto.CodeForbidden},
		{"a project not on the server", "alice", second, proto.CodeNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := hello(t, sock, tc.principal, tc.project)
			switch {
			case tc.code == "" && w.Err != nil:
				t.Errorf("hello(%s, %s) refused: %v", tc.principal, tc.project, w.Err)
			case tc.code != "" && (w.Err == nil || w.Err.Code != tc.code || w.Err.Fix == ""):
				t.Errorf("hello(%s, %s) = %+v, want refusal %s with a fix", tc.principal, tc.project, w.Err, tc.code)
			}
		})
	}
	// The upgrade's health check still passes, for the config's project.
	if v, err := server.Probe(t.Context(), sock, adopted, "v0.0.0"); err != nil || v == "" {
		t.Errorf("Probe = %q, %v; want the daemon's version", v, err)
	}
}

// TestServeWritesTheKeysFile checks that the daemon serve builds writes
// the control store's keys where the settings say before it can listen,
// that a server of one project writes nothing, and that a daemon that
// cannot write the file does not start, and says what to change.
func TestServeWritesTheKeysFile(t *testing.T) {
	s := newSite(t, "bridge: /opt/starfix/bin/starfixd\n")
	keys := writeKeys(t, forced(t, "alice", "alice-1"), forced(t, "dana", "dana-1"))
	if got := s.run(t, "", controlCmd, "init", "--name", "acme", "--authorized-keys", keys); got.err != nil {
		t.Fatalf("control init: %v\n%s", got.err, got.stderr)
	}
	if _, err := os.Lstat(s.keysFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("control init left a keys file (%v); only the daemon writes it", err)
	}
	d, err := openDaemon(t.Context(), s.settings(t), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("open daemon: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	aliceKey, aliceFP := pubKey(t, "alice-1")
	danaKey, danaFP := pubKey(t, "dana-1")
	want := "restrict,command=\"/opt/starfix/bin/starfixd stdio --principal alice --key " + aliceFP + "\" " + aliceKey + "\n" +
		"restrict,command=\"/opt/starfix/bin/starfixd stdio --principal dana --key " + danaFP + "\" " + danaKey + "\n"
	b, err := os.ReadFile(s.keysFile)
	if err != nil {
		t.Fatalf("the daemon opened and wrote no keys file: %v", err)
	}
	if header, lines, _ := strings.Cut(string(b), "\n"); !strings.HasPrefix(header, "# Generated by starfixd") || lines != want {
		t.Errorf("keys file =\n%s\nwant a comment that starfixd generates it, then\n%s", b, want)
	}
	if fi, err := os.Stat(s.keysFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("keys file mode = %v (%v), want 0600", fi.Mode().Perm(), err)
	}

	t.Run("a directory that is not there", func(t *testing.T) {
		s.env[server.EnvKeysFile] = filepath.Join(t.TempDir(), "missing", "starfix_keys")
		defer func() { s.env[server.EnvKeysFile] = s.keysFile }()
		_, err := openDaemon(t.Context(), s.settings(t), slog.New(slog.DiscardHandler))
		if err == nil || !strings.Contains(err.Error(), "write the keys file "+s.env[server.EnvKeysFile]) || !strings.Contains(err.Error(), "; fix: create it as the user starfixd runs as") {
			t.Errorf("openDaemon with no directory for the keys file = %v; want a refusal naming the file, with a fix", err)
		}
	})

	t.Run("a server of one project", func(t *testing.T) {
		one := newSite(t, "")
		rewrite(t, one, "project: "+adopted+"\nadmins: [dana]\n")
		d, err := openDaemon(t.Context(), one.settings(t), slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(one.keysFile); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a server of one project wrote a keys file (%v)", err)
		}
	})
}

// TestKeyFiles checks where the daemon puts the keys file: an absolute
// setting is taken as it is, and a relative one is under the home
// directory of the user it runs as.
func TestKeyFiles(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "keys")
	keys, hand, err := keyFiles(server.Settings{KeysFile: abs, AuthorizedKeys: abs + ".hand"})
	if err != nil || keys != abs || hand != abs+".hand" {
		t.Errorf("keyFiles(absolute) = %q, %q, %v; want them unchanged", keys, hand, err)
	}
	u, err := user.Current()
	if err != nil {
		t.Skipf("no current user: %v", err)
	}
	keys, hand, err = keyFiles(server.Settings{KeysFile: server.DefaultKeysFile, AuthorizedKeys: server.DefaultAuthorizedKeys})
	if err != nil || keys != filepath.Join(u.HomeDir, ".ssh", "starfix_keys") || hand != filepath.Join(u.HomeDir, ".ssh", "authorized_keys") {
		t.Errorf("keyFiles(defaults) = %q, %q, %v; want them under %s/.ssh", keys, hand, err, u.HomeDir)
	}
}

// TestServeOneProjectOpensNoControl checks that a server whose settings
// name no control database is the server it was: it opens its one
// store, no control store, and leaves no control table anywhere.
func TestServeOneProjectOpensNoControl(t *testing.T) {
	s := newSite(t, "")
	rewrite(t, s, "project: "+adopted+"\nadmins: [dana]\n")
	d, err := openDaemon(t.Context(), s.settings(t), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if d.ctl != nil || d.st == nil {
		t.Errorf("a server of one project opened a control store (%v) or no store (%v)", d.ctl, d.st)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	sock := serveSite(t, s)
	// Anyone with a key line is let in, as before: no membership exists.
	if w := hello(t, sock, "anyone", adopted); w.Err != nil {
		t.Errorf("hello on a server of one project refused: %v", w.Err)
	}
	db, err := sql.Open("mysql", dolt.RootDSN(""))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM information_schema.tables
  WHERE table_schema IN (?, ?) AND table_name = 'control_migrations'`, s.db, s.controlDB).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d control_migrations tables exist after serving one project, want none", n)
	}
}

// addSecond adds a second active project in a database of its own,
// through the control store, as `sfx admin project add` will.
func addSecond(t *testing.T, s *site) store.Project {
	t.Helper()
	c := s.control(t)
	dana := store.Actor{Principal: "dana", Session: "s", Machine: "m"}
	if _, err := c.AddProject(t.Context(), dana, store.NewProject{ID: second, Name: "other", Prefix: "oth", Account: "internal",
		DBName: dbName(t, newDSN(t))}); err != nil {
		t.Fatal(err)
	}
	p, err := c.ActivateProject(t.Context(), dana, second)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestImportExportProject(t *testing.T) {
	s := newSite(t, "")
	if got := s.run(t, "", controlCmd, "init", "--name", "acme", "--authorized-keys", writeKeys(t)); got.err != nil {
		t.Fatalf("control init: %v", got.err)
	}
	addSecond(t, s)

	// With no --project, the config file's.
	if got := s.run(t, "", importBD, backlog); got.err != nil || !strings.HasPrefix(got.stdout, "imported 10 issues (10 created)") {
		t.Fatalf("import-bd = %q, %v\n%s", got.stdout, got.err, got.stderr)
	}
	exported := func(args ...string) int {
		t.Helper()
		got := s.run(t, "", exportBD, args...)
		if got.err != nil {
			t.Fatalf("export-bd %v: %v", args, got.err)
		}
		return strings.Count(got.stdout, "\n")
	}
	if n := exported(); n != 10 {
		t.Errorf("export-bd of the config's project wrote %d lines, want 10", n)
	}
	if n := exported("--project", adopted); n != 10 {
		t.Errorf("export-bd --project of the same wrote %d lines, want 10", n)
	}
	if n := exported("--project", second); n != 0 {
		t.Errorf("export-bd --project of the second project wrote %d lines, want 0: nothing was imported into it", n)
	}
	// Into the second: ids keep the export's own, and its database alone
	// changes.
	if got := s.run(t, "", importBD, "--project", second, backlog); got.err != nil {
		t.Fatalf("import-bd --project: %v\n%s", got.err, got.stderr)
	}
	if n := exported("--project", second); n != 10 {
		t.Errorf("after import-bd --project, the second project exports %d lines, want 10", n)
	}

	for _, tc := range []struct {
		name string
		id   string
		want string
	}{
		{"unknown project", "cccccccc-cccc-4ccc-8ccc-cccccccccccc", "is not one of this server's; fix: "},
		{"not a UUID", "acme", "is not one of this server's; fix: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := s.run(t, "", exportBD, "--project", tc.id)
			if got.err == nil || !strings.Contains(got.err.Error(), tc.want) || got.stdout != "" {
				t.Errorf("export-bd --project %s = %q, %v; want an error containing %q", tc.id, got.stdout, got.err, tc.want)
			}
		})
	}
	t.Run("a server of one project takes no --project", func(t *testing.T) {
		rewrite(t, s, "project: "+adopted+"\n")
		got := s.run(t, "", exportBD, "--project", adopted)
		if _, usage := errors.AsType[usageError](got.err); !usage || !strings.Contains(got.err.Error(), "no control_db") {
			t.Errorf("export-bd --project without control_db = %v, want a usage error", got.err)
		}
		if n := exported(); n != 10 {
			t.Errorf("export-bd without control_db wrote %d lines, want the DSN's 10", n)
		}
	})
}

// TestUpgradeBackupManyProjects runs the real backup on a server of
// many projects: every database the new binary may migrate is tagged.
func TestUpgradeBackupManyProjects(t *testing.T) {
	s := newSite(t, "")
	if got := s.run(t, "", controlCmd, "init", "--name", "acme", "--authorized-keys", writeKeys(t)); got.err != nil {
		t.Fatalf("control init: %v", got.err)
	}
	other := addSecond(t, s)
	// One still being created has no database to tag, and stops nothing.
	c := s.control(t)
	if _, err := c.AddProject(t.Context(), store.Actor{Principal: "dana", Session: "s", Machine: "m"}, store.NewProject{
		ID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", Name: "later", Prefix: "lat", Account: "internal", DBName: "not_made_yet"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	env := adminEnv{stdin: strings.NewReader(""), stdout: io.Discard, stderr: io.Discard, getenv: func(k string) string { return s.env[k] }}
	tag, err := backupStore(t.Context(), env, server.Settings{}, s.cfg, "starfix-v0.2.0")
	if err != nil || tag != "starfix-v0.2.0" {
		t.Fatalf("backupStore = %q, %v; want the one tag", tag, err)
	}
	db, err := sql.Open("mysql", dolt.RootDSN(""))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, name := range []string{s.controlDB, s.db, other.DBName} {
		var n int
		// name is one of the test's own database names.
		if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM `"+name+"`.dolt_tags WHERE tag_name = ?", "starfix-v0.2.0").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("database %s has %d tags named starfix-v0.2.0, want 1", name, n)
		}
	}

	// A project whose database is gone stops the upgrade before the swap.
	if _, err := db.ExecContext(t.Context(), "DROP DATABASE `"+other.DBName+"`"); err != nil {
		t.Fatal(err)
	}
	_, err = backupStore(t.Context(), env, server.Settings{}, s.cfg, "starfix-v0.2.1")
	if err == nil || !strings.Contains(err.Error(), "project other (database "+other.DBName+")") {
		t.Errorf("backupStore with a project's database gone = %v, want an error naming the project", err)
	}
}

// tables returns how many tables named table the database db holds.
func tables(t *testing.T, db, table string) int {
	t.Helper()
	root, err := sql.Open("mysql", dolt.RootDSN(""))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	var n int
	if err := root.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = ? AND table_name = ?`,
		db, table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestServeMigratesEveryProject starts the daemon on a server whose
// second project has a database no starfixd ever opened: it is migrated
// before the daemon listens, with no connection to it, as every active
// project's is after an upgrade. An archived project's is left alone.
func TestServeMigratesEveryProject(t *testing.T) {
	s := newSite(t, "")
	if got := s.run(t, "", controlCmd, "init", "--name", "acme", "--authorized-keys", writeKeys(t)); got.err != nil {
		t.Fatalf("control init: %v", got.err)
	}
	other := addSecond(t, s)
	c := s.control(t)
	dana := store.Actor{Principal: "dana", Session: "s", Machine: "m"}
	const third = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	archived := dbName(t, newDSN(t))
	if _, err := c.AddProject(t.Context(), dana, store.NewProject{ID: third, Name: "shelved", Prefix: "sh", Account: "internal", DBName: archived}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ArchiveProject(t.Context(), dana, third); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if n := tables(t, other.DBName, "issues"); n != 0 {
		t.Fatalf("the second project's database has an issues table before any starfixd opened it")
	}

	sock := serveSite(t, s)
	if n := tables(t, other.DBName, "issues"); n != 1 {
		t.Errorf("the second project's database has %d issues tables once the daemon serves, want it migrated at start", n)
	}
	if n := tables(t, archived, "issues"); n != 0 {
		t.Errorf("the archived project's database was migrated at start; it is migrated when it is activated")
	}
	if v, err := server.Probe(t.Context(), sock, adopted, "v0.0.0"); err != nil || v == "" {
		t.Errorf("Probe = %q, %v; want the daemon's version", v, err)
	}
}

// TestServeEndsClaimsOfArchivedProjects starts the daemon on a server
// whose archived project still holds a claim, as a daemon that stopped
// between an archive's control write and its release leaves it: the
// claim has ended before the daemon listens, with no connection to the
// project and without activating it (decision 24).
func TestServeEndsClaimsOfArchivedProjects(t *testing.T) {
	s := newSite(t, "")
	if got := s.run(t, "", controlCmd, "init", "--name", "acme", "--authorized-keys", writeKeys(t)); got.err != nil {
		t.Fatalf("control init: %v", got.err)
	}
	other := addSecond(t, s)
	dsn, err := server.DatabaseDSN(s.dsn, other.DBName)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.Context(), dsn, store.Options{CommitInterval: -1, Prefix: other.Prefix})
	if err != nil {
		t.Fatal(err)
	}
	alice := store.Actor{Principal: "alice", Session: "s-a", Machine: "m"}
	is, err := st.CreateIssue(t.Context(), alice, store.NewIssue{Title: "held as the daemon stopped"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.StartIssue(t.Context(), alice, is.ID, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	c := s.control(t)
	if _, err := c.ArchiveProject(t.Context(), store.Actor{Principal: "dana", Session: "s", Machine: "m"}, second); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	serveSite(t, s)
	root, err := sql.Open("mysql", dolt.RootDSN(other.DBName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	var held, released int
	if err := root.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM claims WHERE principal IS NOT NULL`).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if err := root.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM events WHERE op = 'claim.release' AND principal = 'starfixd'`).Scan(&released); err != nil {
		t.Fatal(err)
	}
	if held != 0 || released != 1 {
		t.Errorf("once the daemon serves, the archived project holds %d claims and %d releases by the server; want 0 and 1", held, released)
	}
}

// siteRig is an upgrade of the daemon a site configures: the release,
// the binary and systemd are the rig's fakes, and the health check is
// the real one, against the site's running daemon.
func siteRig(t *testing.T, s *site, sock string) *upgradeRig {
	t.Helper()
	r := newRig(t, "v0.2.0", "v0.3.0", "")
	body := fmt.Sprintf("project: %s\ncontrol_db: %s\nadmins: [dana]\nsystemd_unit: starfixd\nsocket: %s\n", adopted, s.controlDB, sock)
	if err := os.WriteFile(r.cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	r.dsn = s.dsn
	r.d.healthWait, r.d.healthEvery = 0, time.Millisecond
	r.d.probe = func(ctx context.Context, socket, project string) (string, error) {
		if _, err := server.Probe(ctx, socket, project, "v0.3.0"); err != nil {
			return "", err
		}
		return r.running, nil // the test daemon is one build; the rig says which release it stands for
	}
	return r
}

// TestUpgradeChecksEveryProject runs `starfixd upgrade` against a real
// daemon of many projects. A project whose database cannot be opened
// fails the health check, so the upgrade puts the old binary back; and
// the check does not need the config file's project to be active.
func TestUpgradeChecksEveryProject(t *testing.T) {
	dana := store.Actor{Principal: "dana", Session: "s", Machine: "m"}
	t.Run("a project that cannot be opened fails the upgrade", func(t *testing.T) {
		s := newSite(t, "")
		if got := s.run(t, "", controlCmd, "init", "--name", "acme", "--authorized-keys", writeKeys(t)); got.err != nil {
			t.Fatalf("control init: %v", got.err)
		}
		c := s.control(t)
		if _, err := c.AddProject(t.Context(), dana, store.NewProject{ID: second, Name: "broken", Prefix: "br", Account: "internal",
			DBName: "not_made_yet"}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.ActivateProject(t.Context(), dana, second); err != nil {
			t.Fatal(err)
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		sock := serveSite(t, s)
		// The project that opened is served all the same.
		if w := hello(t, sock, "dana", adopted); w.Err != nil {
			t.Errorf("hello to the healthy project, beside a broken one: %v", w.Err)
		}
		r := siteRig(t, s, sock)
		_, err := r.run(t)
		if err == nil || !strings.Contains(err.Error(), "starfixd v0.3.0 is unhealthy") ||
			!strings.Contains(err.Error(), "could not open the database of an active project: broken (database not_made_yet)") {
			t.Fatalf("upgrade with a project that cannot be opened = %v\nwant it unhealthy, naming the project and its database", err)
		}
		if r.exeHolds(t) != "v0.2.0" || len(r.restarts) != 2 {
			t.Errorf("after the failed upgrade the binary is %q after %d restarts; want v0.2.0 put back and restarted", r.exeHolds(t), len(r.restarts))
		}
	})
	t.Run("the config file's project archived", func(t *testing.T) {
		s := newSite(t, "")
		if got := s.run(t, "", controlCmd, "init", "--name", "acme", "--authorized-keys", writeKeys(t)); got.err != nil {
			t.Fatalf("control init: %v", got.err)
		}
		addSecond(t, s)
		c := s.control(t)
		if _, err := c.ArchiveProject(t.Context(), dana, adopted); err != nil {
			t.Fatal(err)
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		sock := serveSite(t, s)
		if w := hello(t, sock, "dana", adopted); w.Err == nil || w.Err.Code != proto.CodeNotFound {
			t.Fatalf("hello to the archived project = %+v, want not found", w.Err)
		}
		r := siteRig(t, s, sock)
		out, err := r.run(t)
		if err != nil || !strings.Contains(out, "starfixd upgraded v0.2.0 -> v0.3.0") || !strings.Contains(out, "restarted and healthy") {
			t.Errorf("upgrade with the config file's project archived = %q, %v; want it healthy", out, err)
		}
	})
}
