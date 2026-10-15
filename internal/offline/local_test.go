package offline

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/outbox"
	"github.com/ariesworx/starfix/internal/proto"
)

const (
	hostKey = "SHA256:et6CqkKsyU2BxzA7Ws+V18rXrtM9Gj6dk1/m0C5TqvU"
	project = "6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f"
	keyA    = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	keyB    = "SHA256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	keyX    = "SHA256:XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"
)

var t0 = time.Date(2026, 10, 13, 14, 2, 0, 0, time.UTC)

// clock is a clock a test moves by hand.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newLocal(t *testing.T, o outbox.Options) (*Local, *clock) {
	t.Helper()
	c := &clock{t: t0}
	return Open(ownDir(t), hostKey, project, Options{Now: c.now, Outbox: o}), c
}

// ownDir is a new directory that is the test's alone, as sfx's data
// directory must be.
func ownDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // a directory: 0700 is owner-only
		t.Fatal(err)
	}
	return dir
}

func TestDataDir(t *testing.T) {
	t.Parallel()
	home := func() (string, error) { return filepath.FromSlash("/home/alice"), nil }
	noHome := func() (string, error) { return "", errors.New("no home") }
	abs := func(p string) string {
		a, err := filepath.Abs(filepath.FromSlash(p))
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	for _, tc := range []struct {
		name string
		goos string
		env  map[string]string
		home func() (string, error)
		want string // "" for an error
	}{
		{"the override wins", "linux", map[string]string{EnvDataDir: abs("/data/sfx"), "XDG_DATA_HOME": abs("/xdg")}, home, abs("/data/sfx")},
		{"a relative override is refused", "linux", map[string]string{EnvDataDir: "data"}, home, ""},
		{"linux: XDG_DATA_HOME", "linux", map[string]string{"XDG_DATA_HOME": abs("/xdg")}, home, filepath.Join(abs("/xdg"), "starfix")},
		{"linux: a relative XDG_DATA_HOME is ignored", "linux", map[string]string{"XDG_DATA_HOME": "xdg"}, home, filepath.FromSlash("/home/alice/.local/share/starfix")},
		{"linux: the home directory", "linux", nil, home, filepath.FromSlash("/home/alice/.local/share/starfix")},
		{"linux: no home", "linux", nil, noHome, ""},
		{"macOS", "darwin", nil, home, filepath.FromSlash("/home/alice/Library/Application Support/starfix")},
		{"Windows: local, never roaming", "windows", map[string]string{"LocalAppData": abs("/local"), "AppData": abs("/roaming")}, home, filepath.Join(abs("/local"), "starfix")},
		{"Windows: without LocalAppData", "windows", map[string]string{"AppData": abs("/roaming")}, home, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DataDir(tc.goos, func(k string) string { return tc.env[k] }, tc.home)
			if (err != nil) != (tc.want == "") || got != tc.want {
				t.Errorf("DataDir(%s, %v) = %q, %v; want %q", tc.goos, tc.env, got, err, tc.want)
			}
			if err != nil {
				if pe, ok := errors.AsType[*proto.Error](err); !ok || !strings.Contains(pe.Fix, EnvDataDir) {
					t.Errorf("DataDir(%s, %v) = %v, want a refusal whose fix names %s", tc.goos, tc.env, err, EnvDataDir)
				}
			}
		})
	}
}

// TestPrincipalRule pins whose outbox a write goes to while the server
// cannot say. With a key configured, it is the principal the server last
// took that key for. With none, it is the principal of the key the
// server accepted the last time none was configured, and only while
// that key is still among those sfx would offer. Otherwise it is
// nobody's, and nothing is queued.
func TestPrincipalRule(t *testing.T) {
	t.Parallel()
	l, clk := newLocal(t, outbox.Options{})
	who := func(l *Local, configured bool, offered ...string) string {
		p, ok := l.Principal(offered, configured)
		if ok != (p != "") {
			t.Errorf("Principal(%v, %v) = %q, %v", offered, configured, p, ok)
		}
		return p
	}
	if got := who(l, true, keyA) + who(l, false, keyA); got != "" {
		t.Errorf("before any handshake, Principal = %q, want none", got)
	}
	if was, err := l.Reached("", keyA, true, proto.Proto); err != nil || was != "" || who(l, true, keyA) != "" {
		t.Errorf("a handshake that names no principal was recorded (%v)", err)
	}
	// alice signs in with no key configured, and her agent with its own.
	if _, err := l.Reached("alice", keyA, false, proto.Proto); err != nil {
		t.Fatalf("Reached = %v", err)
	}
	if _, err := l.Reached("alice-agent", keyB, true, proto.Proto); err != nil {
		t.Fatalf("Reached = %v", err)
	}
	for _, tc := range []struct {
		name       string
		configured bool
		offered    []string
		want       string
	}{
		{"the configured key", true, []string{keyB}, "alice-agent"},
		{"a configured key the server took when none was configured", true, []string{keyA}, "alice"},
		{"a configured key the server never took", true, []string{keyX}, ""},
		{"none configured: the key accepted last time, still offered", false, []string{keyX, keyA}, "alice"},
		{"none configured: that key is offered no more", false, []string{keyX, keyB}, ""},
		{"none configured: a key accepted only when it was configured does not decide", false, []string{keyB}, ""},
		{"no key", false, nil, ""},
	} {
		if got := who(l, tc.configured, tc.offered...); got != tc.want {
			t.Errorf("%s: Principal(%v) = %q, want %q", tc.name, tc.offered, got, tc.want)
		}
	}
	if got := l.Principals(); !slices.Equal(got, []string{"alice", "alice-agent"}) {
		t.Errorf("Principals = %v, want alice and alice-agent", got)
	}

	// What one server and project said holds for no other.
	for name, other := range map[string]*Local{
		"another server":  Open(l.root, strings.Replace(hostKey, "et6", "zzz", 1), project, Options{}),
		"another project": Open(l.root, hostKey, strings.Replace(project, "6f1c", "0000", 1), Options{}),
	} {
		if got := who(other, true, keyA); got != "" {
			t.Errorf("%s: Principal = %q, want none", name, got)
		}
	}

	// The server may give a key to another principal: the last handshake
	// wins, and says who the key was before.
	clk.t = t0.Add(time.Hour)
	was, err := l.Reached("bob", keyA, false, proto.Proto)
	if err != nil || was != "alice" || who(l, false, keyA) != "bob" {
		t.Errorf("Reached(bob) = %q, %v, and Principal = %q; want alice, who the key was, and then bob", was, err, who(l, false, keyA))
	}
	if was, err := l.Reached("bob", keyA, false, proto.Proto); err != nil || was != "" {
		t.Errorf("a second handshake as bob = %q, %v; want no change", was, err)
	}

	// The record is the server, the project, and for each key its
	// fingerprint, the principal and when, and the protocol the server
	// spoke: nothing else.
	b, err := os.ReadFile(filepath.Join(l.dir, seenFile))
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`{"v":1,"server":%q,"project":%q,"keys":{%q:{"principal":"bob","at":"2026-10-13T15:02:00Z"},%q:{"principal":"alice-agent","at":"2026-10-13T14:02:00Z"}},"unconfigured":%q,"proto":%d}`+"\n",
		hostKey, project, keyA, keyB, keyA, proto.Proto)
	if string(b) != want {
		t.Errorf("%s holds\n%s\nwant\n%s", seenFile, b, want)
	}
}

// TestDirectoryMustBeTheUsersOwn pins that a data directory others can
// read or write is refused, with a fix, and never chmodded: sfx did not
// make it that way, and what it would keep there is a person's text.
func TestDirectoryMustBeTheUsersOwn(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no mode bits; its ACLs are not checked")
	}
	for _, tc := range []struct {
		name string
		mode os.FileMode
		dir  func(l *Local) string
	}{
		{"the data directory, readable by the group", 0o750, func(l *Local) string { return l.root }},
		{"the data directory, writable by others", 0o707, func(l *Local) string { return l.root }},
		{"the server's directory", 0o755, func(l *Local) string { return l.dir }},
		{"the outbox", 0o755, func(l *Local) string { return l.outboxDir("alice") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := newLocal(t, outbox.Options{})
			if _, err := l.Reached("alice", keyA, true, proto.Proto); err != nil {
				t.Fatal(err)
			}
			if _, err := l.Keep("alice", entry(proto.OpComment, "sf-a"), errReach); err != nil {
				t.Fatal(err)
			}
			dir := tc.dir(l)
			if err := os.Chmod(dir, tc.mode); err != nil { //nolint:gosec // the mode sfx must refuse
				t.Fatal(err)
			}
			check := func(what string, err error) {
				t.Helper()
				pe, ok := errors.AsType[*proto.Error](err)
				if !ok || pe.Code != proto.CodeUnavailable || !strings.Contains(pe.Message, dir) ||
					!strings.Contains(pe.Fix, "chmod 700 "+dir) || !strings.Contains(pe.Fix, EnvDataDir) {
					t.Errorf("%s = %v, want a refusal that names %s and a fix to chmod it or set %s", what, err, dir, EnvDataDir)
				}
			}
			_, err := l.Keep("alice", entry(proto.OpComment, "sf-b"), errReach)
			check("Keep", err)
			if !errors.Is(err, ErrNotQueued) {
				t.Errorf("Keep = %v, want ErrNotQueued", err)
			}
			_, err = l.Replay(t.Context(), &fakeConn{who: "alice"})
			check("Replay", err)
			_, err = l.Waiting("alice")
			check("Waiting", err)
			if dir != l.outboxDir("alice") {
				_, err = l.Reached("bob", keyB, true, proto.Proto)
				check("Reached", err)
				if p, ok := l.Principal([]string{keyA}, true); ok {
					t.Errorf("Principal read a record others could have written: %q", p)
				}
			}
			if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != tc.mode {
				t.Errorf("%s now has mode %v (%v), want it left at %o", dir, st.Mode().Perm(), err, tc.mode)
			}
		})
	}
}
func TestLocalFilesAreTheOwnersAlone(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no mode bits")
	}
	l, _ := newLocal(t, outbox.Options{})
	if _, err := l.Reached("alice", keyA, true, proto.Proto); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Keep("alice", entry(proto.OpComment, "sf-a"), errReach); err != nil {
		t.Fatal(err)
	}
	err := filepath.WalkDir(l.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		st, err := d.Info()
		if err != nil {
			return err
		}
		want := os.FileMode(0o600)
		if d.IsDir() {
			want = 0o700
		}
		if path != l.root && st.Mode().Perm() != want {
			t.Errorf("%s has mode %o, want %o", path, st.Mode().Perm(), want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A principal's name is the server's text: it names no file.
func TestOutboxDirectoryPerPrincipal(t *testing.T) {
	t.Parallel()
	l, _ := newLocal(t, outbox.Options{})
	dirs := map[string]string{}
	for _, p := range []string{"alice", "bob", "../../etc", "CON", "Alice"} {
		d := l.outboxDir(p)
		if rel, err := filepath.Rel(l.dir, d); err != nil || strings.Contains(rel, "..") || strings.ContainsAny(filepath.Base(d), "/\\.") {
			t.Errorf("outboxDir(%q) = %q, which is not a plain name under %q", p, d, l.dir)
		}
		if strings.Contains(strings.ToLower(filepath.Base(d)), strings.ToLower(strings.Trim(p, "./"))) {
			t.Errorf("outboxDir(%q) = %q carries the principal's name", p, d)
		}
		for q, e := range dirs {
			if e == d {
				t.Errorf("outboxDir(%q) = outboxDir(%q) = %q", p, q, d)
			}
		}
		dirs[p] = d
	}
	if l.outboxDir("alice") != dirs["alice"] {
		t.Error("outboxDir(alice) changed between calls")
	}
}

var errReach = proto.Errf(proto.CodeUnavailable, "check server.host and server.port in .starfix.yaml, and your network",
	"cannot reach 127.0.0.1:2222: connection refused")

func entry(op, id string) outbox.Entry {
	args := map[string]any{"id": id, "body": "the note", "idem": proto.NewIdem("cli")}
	if op == proto.OpRemember {
		args = map[string]any{"key": id, "body": "the note", "idem": proto.NewIdem("cli")}
	}
	b, err := json.Marshal(args)
	if err != nil {
		panic(err) // a programming error in the test
	}
	return outbox.Entry{Op: op, Args: b, Session: "cli", Machine: "laptop"}
}

func TestKeep(t *testing.T) {
	t.Parallel()
	l, clk := newLocal(t, outbox.Options{})
	first, err := l.Keep("alice", entry(proto.OpHandoff, "sf-a"), errReach)
	if err != nil {
		t.Fatalf("Keep = %v", err)
	}
	clk.t = t0.Add(5 * time.Minute)
	second, err := l.Keep("alice", entry(proto.OpComment, "sf-b"), errReach)
	if err != nil {
		t.Fatalf("Keep = %v", err)
	}
	if first.Entry.Seq != 1 || second.Entry.Seq != 2 || first.Entry.Session != "cli" || first.Entry.Machine != "laptop" || first.Entry.Idem == "" {
		t.Errorf("Keep stored %+v then %+v; want entries 1 and 2 with their session, machine and key", first.Entry, second.Entry)
	}
	if !first.Since.Equal(t0) || !second.Since.Equal(t0) || second.Principal != "alice" {
		t.Errorf("Keep = since %s then %s, want both %s, when the outage began", first.Since, second.Since, t0)
	}
	if got := l.LastMissed(); !got.Equal(clk.t) {
		t.Errorf("LastMissed = %s, want %s, when the server was last not reached", got, clk.t)
	}
	n, err := l.Waiting("alice")
	if err != nil || n.Pending != 2 || n.Refused != 0 {
		t.Errorf("Waiting(alice) = %+v, %v; want 2 pending", n, err)
	}
	if n, err := l.Waiting("bob"); err != nil || n != (Count{}) {
		t.Errorf("Waiting(bob) = %+v, %v; want nothing, and no error", n, err)
	}
	if _, err := os.Stat(l.outboxDir("bob")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("asking what waits for bob made his outbox (%v)", err)
	}
}

// TestKeepRefusals pins each way a write that did not reach the server
// is not queued either: its code, and a fix that names the next action
// (rule 11). The message keeps why the server was not reached.
func TestKeepRefusals(t *testing.T) {
	t.Parallel()
	secret := entry(proto.OpHandoff, "sf-a")
	secret.Args = json.RawMessage(`{"id":"sf-a","note":"the token is ghp_0123456789abcdefghijABCDEFGHIJ012345","idem":"cli-1"}`)
	big := entry(proto.OpHandoff, "sf-a")
	big.Args = json.RawMessage(fmt.Sprintf(`{"id":"sf-a","note":%q,"idem":"cli-1"}`, strings.Repeat("word ", 400)))
	blocked := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		local func(t *testing.T) *Local
		who   string
		e     outbox.Entry
		code  proto.Code
		is    error
		msg   string
		fix   string
	}{
		{name: "nobody: this machine never reached the server with this key", who: "", e: entry(proto.OpHandoff, "sf-a"),
			code: proto.CodeUnavailable, msg: "this machine has never reached the server with this key",
			fix: "run it again when the server answers"},
		{name: "full", e: entry(proto.OpComment, "sf-a"), who: "alice",
			local: func(t *testing.T) *Local {
				l, _ := newLocal(t, outbox.Options{MaxEntries: 1})
				if _, err := l.Keep("alice", entry(proto.OpComment, "sf-a"), errReach); err != nil {
					t.Fatal(err)
				}
				return l
			},
			code: proto.CodeBusy, is: outbox.ErrFull, msg: "1 entries wait, which is the cap", fix: "`sfx outbox drop N`"},
		{name: "too large", e: big, who: "alice",
			local: func(t *testing.T) *Local { l, _ := newLocal(t, outbox.Options{MaxEntryBytes: 1024}); return l },
			code:  proto.CodeInvalid, is: outbox.ErrTooLarge, msg: "the cap is 1024", fix: "shorten its text"},
		{name: "a secret", e: secret, who: "alice", code: proto.CodeInvalid, is: outbox.ErrSecret, msg: "the outbox keeps no secret",
			fix: proto.FixSecret},
		{name: "the directory cannot be made", e: entry(proto.OpComment, "sf-a"), who: "alice",
			local: func(*testing.T) *Local { return Open(blocked, hostKey, project, Options{}) },
			code:  proto.CodeUnavailable, msg: "a-file", fix: EnvDataDir},
		{name: "an op that is never queued", e: entry(proto.OpStart, "sf-a"), who: "alice", code: proto.CodeUnavailable,
			is: outbox.ErrNotQueueable, msg: "takes a claim", fix: "run it again when the server answers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := newLocal(t, outbox.Options{})
			if tc.local != nil {
				l = tc.local(t)
			}
			_, err := l.Keep(tc.who, tc.e, errReach)
			pe, ok := errors.AsType[*proto.Error](err)
			if !ok {
				t.Fatalf("Keep = %v, want a refusal", err)
			}
			if pe.Code != tc.code || !strings.Contains(pe.Message, tc.msg) || !strings.Contains(pe.Fix, tc.fix) {
				t.Errorf("Keep = %s: %q, fix %q\nwant %s, a message holding %q and a fix holding %q", pe.Code, pe.Message, pe.Fix, tc.code, tc.msg, tc.fix)
			}
			if !strings.Contains(pe.Message, errReach.Message) || !strings.Contains(pe.Message, "was not queued") {
				t.Errorf("Keep's message %q does not say why the server was not reached and that nothing was queued", pe.Message)
			}
			if !errors.Is(err, ErrNotQueued) || tc.is != nil && !errors.Is(err, tc.is) {
				t.Errorf("Keep = %v, want ErrNotQueued and %v", err, tc.is)
			}
			if strings.Contains(pe.Message+pe.Fix, "ghp_") {
				t.Errorf("the refusal repeats the secret: %q, fix %q", pe.Message, pe.Fix)
			}
		})
	}
}

// fakeConn answers a replayed entry by the issue it names.
type fakeConn struct {
	who     string
	answers map[string]error // by the entry's id or key
	lost    error
	calls   []string // "op id", and "op id -paths" for a call without paths
	// speaks is the connection's protocol; 0 is this build's.
	speaks int
	// parks are the issues on which a replayed update parks a field.
	parks map[string]bool
	// keeps are the issues on which a replayed finish or handoff is
	// kept as a comment.
	keeps map[string]bool
}

func (f *fakeConn) Principal() string { return f.who }
func (f *fakeConn) Err() error        { return f.lost }
func (f *fakeConn) Proto() int        { return cmp.Or(f.speaks, proto.Proto) }

func (f *fakeConn) Call(_ context.Context, op string, args, out any) error {
	b, err := json.Marshal(proto.ForProtocol(f.Proto(), op, args))
	if err != nil {
		return err
	}
	var a struct {
		ID, Key string
		Paths   []string
		Park    bool
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	name := op + " " + a.ID + a.Key
	if op == proto.OpHandoff && len(a.Paths) == 0 {
		name += " -paths"
	}
	if a.Park {
		name += " park"
		if res, ok := out.(*proto.UpdateResult); ok && f.parks[a.ID] {
			res.Parked = []proto.ParkedField{{Field: "title", Conflict: 1}}
		}
		if res, ok := out.(*proto.FinishResult); ok && f.keeps[a.ID] {
			res.Comment = "c-" + a.ID
		}
		if res, ok := out.(*proto.HandoffResult); ok && f.keeps[a.ID] {
			res.Comment = "c-" + a.ID
		}
	}
	f.calls = append(f.calls, name)
	err = f.answers[a.ID+a.Key]
	switch {
	case errors.Is(err, errDropped):
		f.lost = err
	case errors.Is(err, errBadPaths) && len(a.Paths) == 0:
		return nil // only the paths were wrong
	}
	return err
}

var (
	errDropped  = proto.Errf(proto.CodeUnavailable, "retry", "the server closed the connection")
	errBadPaths = proto.Errf(proto.CodeInvalid, "correct it and retry", "a path is not relative to the repository")
)

// TestReplay pins what becomes of a replayed entry by the server's
// answer: applied, it is gone; refused, it waits for a person with the
// server's code, message and fix; and when the server was not reached,
// or said to wait, it stays where it was, with everything after it.
func TestReplay(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		answers   map[string]error
		delivered int
		refused   []string // "id code"
		pending   int
		calls     []string
		stopped   bool
	}{
		{name: "all applied", delivered: 3, calls: []string{"comment sf-a", "comment sf-b", "remember style"}},
		{name: "one refused, the rest go on",
			answers:   map[string]error{"sf-a": proto.Errf(proto.CodeNotFound, "find the id with `sfx list`", "issue sf-a not found")},
			delivered: 2, refused: []string{"sf-a not_found"}, calls: []string{"comment sf-a", "comment sf-b", "remember style"}},
		{name: "forbidden is a refusal, kept", answers: map[string]error{"sf-b": proto.Errf(proto.CodeForbidden, "ask bob", "bob holds sf-b")},
			delivered: 2, refused: []string{"sf-b forbidden"}, calls: []string{"comment sf-a", "comment sf-b", "remember style"}},
		{name: "unavailable stops the replay",
			answers:   map[string]error{"sf-b": proto.Errf(proto.CodeUnavailable, "retry", "the server could not complete comment")},
			delivered: 1, pending: 2, calls: []string{"comment sf-a", "comment sf-b"}, stopped: true},
		{name: "busy stops the replay", answers: map[string]error{"sf-a": proto.Errf(proto.CodeBusy, "wait 2s and retry", "too many writes")},
			pending: 3, calls: []string{"comment sf-a"}, stopped: true},
		{name: "a dropped connection stops the replay", answers: map[string]error{"sf-a": errDropped},
			pending: 3, calls: []string{"comment sf-a"}, stopped: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := newLocal(t, outbox.Options{})
			for _, e := range []outbox.Entry{entry(proto.OpComment, "sf-a"), entry(proto.OpComment, "sf-b"), entry(proto.OpRemember, "style")} {
				if _, err := l.Keep("alice", e, errReach); err != nil {
					t.Fatal(err)
				}
			}
			c := &fakeConn{who: "alice", answers: tc.answers}
			rep, err := l.Replay(t.Context(), c)
			if err != nil {
				t.Fatalf("Replay = %v", err)
			}
			if rep.Delivered != tc.delivered || rep.Refused != len(tc.refused) || rep.Pending != tc.pending || (rep.Stopped != nil) != tc.stopped {
				t.Errorf("Replay = %+v; want %d delivered, %d refused, %d pending, stopped %v", rep, tc.delivered, len(tc.refused), tc.pending, tc.stopped)
			}
			if !slices.Equal(c.calls, tc.calls) {
				t.Errorf("Replay sent %v, want %v", c.calls, tc.calls)
			}
			ob, _, err := l.Existing("alice")
			if err != nil {
				t.Fatal(err)
			}
			list, err := ob.List()
			if err != nil {
				t.Fatal(err)
			}
			var refused []string
			for _, r := range list.Refused {
				if r.Message == "" || r.Fix == "" {
					t.Errorf("refused entry %d keeps no message or no fix: %+v", r.Entry.Seq, r)
				}
				refused = append(refused, Target(r.Entry)+" "+r.Code)
			}
			if !slices.Equal(refused, tc.refused) || len(list.Pending) != tc.pending {
				t.Errorf("after Replay: refused %v and %d pending; want %v and %d", refused, len(list.Pending), tc.refused, tc.pending)
			}
		})
	}
}

// Paths are a hint read from git. A finish or a handoff the server
// refuses as invalid with them is sent once more without, as it is when
// it is not replayed (proto.RetryWithoutPaths).
func TestReplayRetriesWithoutPaths(t *testing.T) {
	t.Parallel()
	l, _ := newLocal(t, outbox.Options{})
	e := outbox.Entry{Op: proto.OpHandoff, Args: json.RawMessage(`{"id":"sf-a","note":"n","idem":"cli-1","paths":["../x"]}`)}
	if _, err := l.Keep("alice", e, errReach); err != nil {
		t.Fatal(err)
	}
	c := &fakeConn{who: "alice", answers: map[string]error{"sf-a": errBadPaths}}
	rep, err := l.Replay(t.Context(), c)
	if err != nil || rep.Delivered != 1 || rep.Refused != 0 {
		t.Errorf("Replay = %+v, %v; want the handoff delivered", rep, err)
	}
	if want := []string{"handoff sf-a park", "handoff sf-a -paths park"}; !slices.Equal(c.calls, want) {
		t.Errorf("Replay sent %v, want %v", c.calls, want)
	}
}

// An outbox is delivered only on a connection the server took for its
// principal: another principal's stays where it is, unsent.
func TestReplayLeavesAnotherPrincipalsOutbox(t *testing.T) {
	t.Parallel()
	l, _ := newLocal(t, outbox.Options{})
	if _, err := l.Keep("alice", entry(proto.OpComment, "sf-a"), errReach); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"bob", ""} {
		c := &fakeConn{who: who}
		rep, err := l.Replay(t.Context(), c)
		if err != nil || rep != (outbox.Report{}) || len(c.calls) != 0 {
			t.Errorf("Replay as %q = %+v, %v, having sent %v; want nothing sent", who, rep, err, c.calls)
		}
	}
	if _, err := os.Stat(l.outboxDir("bob")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a replay as bob, who queued nothing, made his outbox (%v)", err)
	}
	if n, err := l.Waiting("alice"); err != nil || n.Pending != 1 {
		t.Errorf("Waiting(alice) = %+v, %v; want her entry still pending", n, err)
	}
	c := &fakeConn{who: "alice"}
	if rep, err := l.Replay(t.Context(), c); err != nil || rep.Delivered != 1 {
		t.Errorf("Replay as alice = %+v, %v; want her entry delivered", rep, err)
	}
}

// TestEnded: when the server refuses a principal's access to the
// project, what waits in that principal's outbox is refused with the
// server's code, message and fix, and kept for a person. Another
// principal's outbox is not touched, and nobody's outbox is made.
func TestEnded(t *testing.T) {
	t.Parallel()
	l, _ := newLocal(t, outbox.Options{})
	for _, who := range []string{"alice", "alice", "bob"} {
		if _, err := l.Keep(who, entry(proto.OpComment, "sf-a"), errReach); err != nil {
			t.Fatal(err)
		}
	}
	ended := proto.Errf(proto.CodeForbidden, "ask a server admin to run `sfx admin principal enable alice`", "alice is disabled on this server")
	if n, err := l.Ended("alice", ended); err != nil || n != 2 {
		t.Fatalf("Ended(alice) = %d, %v; want her 2 entries refused", n, err)
	}
	ob, _, err := l.Existing("alice")
	if err != nil {
		t.Fatal(err)
	}
	list, err := ob.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Pending) != 0 || len(list.Refused) != 2 {
		t.Fatalf("alice's outbox after Ended: %d pending, %d refused; want 0 and 2", len(list.Pending), len(list.Refused))
	}
	for _, r := range list.Refused {
		if r.Code != string(proto.CodeForbidden) || r.Message != ended.Message || r.Fix != ended.Fix {
			t.Errorf("entry %d is refused as %s: %q, fix %q; want the server's refusal %+v", r.Entry.Seq, r.Code, r.Message, r.Fix, ended)
		}
	}
	if n, err := l.Waiting("bob"); err != nil || n.Pending != 1 || n.Refused != 0 {
		t.Errorf("Waiting(bob) = %+v, %v; want his entry still pending", n, err)
	}
	if n, err := l.Ended("carol", ended); err != nil || n != 0 {
		t.Errorf("Ended(carol), who queued nothing = %d, %v; want 0, nil", n, err)
	}
	if _, err := os.Stat(l.outboxDir("carol")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Ended(carol) made her outbox (%v)", err)
	}
}

// update is an update entry of issue id, with its key.
func update(id string) outbox.Entry {
	b, err := json.Marshal(proto.UpdateArgs{ID: id, Rev: 3, Title: ptr("renamed"), Idem: proto.NewIdem("cli")})
	if err != nil {
		panic(err) // a programming error in the test
	}
	return outbox.Entry{Op: proto.OpUpdate, Args: b, Session: "cli", Machine: "laptop"}
}

func ptr[T any](v T) *T { return &v }

// TestKeepByProtocol: a write is queued only when the server spoke a
// protocol, at the last handshake, that takes its op's key (design
// §18.5): an update not for a server of 5, or one this machine has not
// heard the protocol of, which is taken to be the oldest; a comment for
// both.
func TestKeepByProtocol(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		protocol int
		queued   bool
	}{
		{"a server of protocol 6", 6, true},
		{"a server of protocol 5", 5, false},
		{"a record from before protocols were noted", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l, _ := newLocal(t, outbox.Options{})
			if _, err := l.Reached("alice", keyA, true, tc.protocol); err != nil {
				t.Fatal(err)
			}
			if _, err := l.Keep("alice", entry(proto.OpComment, "sf-a"), errReach); err != nil {
				t.Errorf("Keep(comment) = %v, want it queued whatever the protocol", err)
			}
			_, err := l.Keep("alice", update("sf-a"), errReach)
			if tc.queued != (err == nil) {
				t.Fatalf("Keep(update) = %v, want queued %v", err, tc.queued)
			}
			if tc.queued {
				return
			}
			pe, ok := errors.AsType[*proto.Error](err)
			if !ok || !errors.Is(err, ErrNotQueued) || pe.Code != proto.CodeUnavailable ||
				!strings.Contains(pe.Message, "and this update was not queued: the server spoke protocol 5 when last reached, which takes no idempotency key for update") ||
				!strings.HasPrefix(pe.Fix, rerun) {
				t.Errorf("Keep(update) = %+v; want unavailable, not queued, saying why, with the fix to run it again", err)
			}
		})
	}
}

// TestReplayParks: a replayed update asks the server to compare it
// (park, protocol 7), and one the server parked in part is delivered and
// counted as parked; to a server of 6 it goes as a plain update.
func TestReplayParks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		speaks int
		calls  []string
		parked int
	}{
		{"a server of 7", 7, []string{"update sf-a park", "update sf-b park", "comment sf-c"}, 1},
		{"a server of 6", 6, []string{"update sf-a", "update sf-b", "comment sf-c"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l, _ := newLocal(t, outbox.Options{})
			if _, err := l.Reached("alice", keyA, true, tc.speaks); err != nil {
				t.Fatal(err)
			}
			for _, e := range []outbox.Entry{update("sf-a"), update("sf-b"), entry(proto.OpComment, "sf-c")} {
				if _, err := l.Keep("alice", e, errReach); err != nil {
					t.Fatal(err)
				}
			}
			c := &fakeConn{who: "alice", speaks: tc.speaks, parks: map[string]bool{"sf-a": true}}
			rep, err := l.Replay(t.Context(), c)
			if err != nil || rep.Delivered != 3 || rep.Parked != tc.parked || !slices.Equal(c.calls, tc.calls) {
				t.Errorf("Replay = %+v, %v, sending %v; want 3 delivered, %d parked, sending %v", rep, err, c.calls, tc.parked, tc.calls)
			}
		})
	}
}

// TestReplayKeepsLate: a replayed finish or handoff asks the server to
// keep it as a comment where the issue moved on since its claim (park,
// protocol 8), and one it kept is delivered and counted as kept; to a
// server of 7 they go as they are, and one the server refuses waits in
// the refused list.
func TestReplayKeepsLate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		speaks int
		calls  []string
		kept   int
	}{
		{"a server of 8", 8, []string{"finish sf-a park", "handoff sf-b -paths park", "finish sf-c park"}, 2},
		{"a server of 7", 7, []string{"finish sf-a", "handoff sf-b -paths", "finish sf-c"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l, _ := newLocal(t, outbox.Options{})
			if _, err := l.Reached("alice", keyA, true, tc.speaks); err != nil {
				t.Fatal(err)
			}
			for _, e := range []outbox.Entry{entry(proto.OpFinish, "sf-a"), entry(proto.OpHandoff, "sf-b"), entry(proto.OpFinish, "sf-c")} {
				if _, err := l.Keep("alice", e, errReach); err != nil {
					t.Fatal(err)
				}
			}
			c := &fakeConn{who: "alice", speaks: tc.speaks, keeps: map[string]bool{"sf-a": true, "sf-b": true}}
			rep, err := l.Replay(t.Context(), c)
			if err != nil || rep.Delivered != 3 || rep.Kept != tc.kept || rep.Parked != 0 || !slices.Equal(c.calls, tc.calls) {
				t.Errorf("Replay = %+v, %v, sending %v; want 3 delivered, %d kept, sending %v", rep, err, c.calls, tc.kept, tc.calls)
			}
		})
	}
}

// TestReplayByProtocol: an entry whose op the connection's protocol
// takes no key for is not sent, since a server that cannot apply it once
// must not be sent it twice; it is refused with code version and kept
// for a person, and what is behind it is sent.
func TestReplayByProtocol(t *testing.T) {
	t.Parallel()
	l, _ := newLocal(t, outbox.Options{})
	if _, err := l.Reached("alice", keyA, true, proto.Proto); err != nil {
		t.Fatal(err)
	}
	for _, e := range []outbox.Entry{update("sf-a"), entry(proto.OpComment, "sf-b")} {
		if _, err := l.Keep("alice", e, errReach); err != nil {
			t.Fatal(err)
		}
	}
	c := &fakeConn{who: "alice", speaks: 5}
	rep, err := l.Replay(t.Context(), c)
	if err != nil || rep.Delivered != 1 || rep.Refused != 1 || !slices.Equal(c.calls, []string{"comment sf-b"}) {
		t.Fatalf("Replay on a connection of protocol 5 = %+v, %v, sending %v; want the comment delivered and the update refused unsent", rep, err, c.calls)
	}
	ob, _, err := l.Existing("alice")
	if err != nil {
		t.Fatal(err)
	}
	list, err := ob.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Refused) != 1 {
		t.Fatalf("refused = %+v, want the update", list.Refused)
	}
	r := list.Refused[0]
	if r.Code != string(proto.CodeVersion) || !strings.Contains(r.Message, "the server speaks protocol 5, which takes no idempotency key for update") ||
		!strings.Contains(r.Fix, "`sfx outbox retry 1`") {
		t.Errorf("the update is refused as %s: %q, fix %q; want version, saying why, with a fix", r.Code, r.Message, r.Fix)
	}
	if err := ob.Retry(r.Entry.Seq); err != nil {
		t.Fatal(err)
	}
	c = &fakeConn{who: "alice"}
	if rep, err := l.Replay(t.Context(), c); err != nil || rep.Delivered != 1 || !slices.Equal(c.calls, []string{"update sf-a park"}) {
		t.Errorf("Replay on a connection of this build, after a retry = %+v, %v, sending %v; want the update delivered", rep, err, c.calls)
	}
}

// oldKey and otherKey are host keys a server had before it was rebuilt.
const (
	oldKey   = "SHA256:oldoldoldoldoldoldoldoldoldoldoldoldoldoldo"
	otherKey = "SHA256:otherotherotherotherotherotherotherotherot"
)

// queuedAt makes the Local of a server and project at addr under root,
// as a handshake leaves it, and queues for alice there.
func queuedAt(t *testing.T, root, key, proj, addr string, n int) *Local {
	t.Helper()
	l := Open(root, key, proj, Options{Now: func() time.Time { return t0 }, Address: addr})
	if _, err := l.Reached("alice", keyA, true, proto.Proto); err != nil {
		t.Fatal(err)
	}
	for range n {
		if _, err := l.Keep("alice", entry(proto.OpComment, "sf-a"), errReach); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

// TestRekeyed: what this machine keeps for the same project under a
// host key the server no longer has is found, with the address it was
// kept for; what it keeps for another project, or for the current key,
// is not.
func TestRekeyed(t *testing.T) {
	t.Parallel()
	root := ownDir(t)
	const here, there = "example.com:22", "example.org:22"
	queuedAt(t, root, oldKey, project, here, 2)
	queuedAt(t, root, otherKey, project, there, 1)
	queuedAt(t, root, oldKey, strings.Replace(project, "6f1c", "0000", 1), here, 1)
	cur := queuedAt(t, root, hostKey, project, here, 0)
	// A directory left by an sfx from before addresses were noted.
	unnoted := Open(root, keyX, project, Options{Now: func() time.Time { return t0 }})
	if _, err := unnoted.Reached("alice", keyA, true, 0); err != nil {
		t.Fatal(err)
	}

	got, err := cur.Rekeyed()
	if err != nil {
		t.Fatalf("Rekeyed() = %v", err)
	}
	type former struct{ key, addr string }
	var have []former
	for _, f := range got {
		have = append(have, former{f.HostKey, f.Address})
		if f.Local.Dir() != Open(root, f.HostKey, project, Options{}).Dir() {
			t.Errorf("the Local of %s is %s, want that server and project's", f.HostKey, f.Local.Dir())
		}
	}
	want := []former{{oldKey, here}, {otherKey, there}, {keyX, ""}}
	slices.SortFunc(want, func(a, b former) int { return strings.Compare(a.key, b.key) })
	if !slices.Equal(have, want) {
		t.Errorf("Rekeyed() = %v, want %v", have, want)
	}
}

// TestAdopt: an entry kept for the project under the server's old host
// key moves, when a person says so, to the outbox of the same principal
// for the current key, with its op, arguments and key, to be sent
// there; only when the server is at the address it was kept for.
func TestAdopt(t *testing.T) {
	t.Parallel()
	root := ownDir(t)
	const here = "example.com:22"
	old := queuedAt(t, root, oldKey, project, here, 2)
	moved := queuedAt(t, root, otherKey, project, "example.org:22", 1)
	cur := queuedAt(t, root, hostKey, project, here, 0)

	ob, _, err := old.Existing("alice")
	if err != nil {
		t.Fatal(err)
	}
	before, err := ob.List()
	if err != nil {
		t.Fatal(err)
	}
	e, err := cur.Adopt(old, "alice", 2)
	if err != nil {
		t.Fatalf("Adopt(entry 2) = %v", err)
	}
	if e.Seq != 1 || e.Op != before.Pending[1].Op || e.Idem != before.Pending[1].Idem || string(e.Args) != string(before.Pending[1].Args) {
		t.Errorf("Adopt stored %+v; want entry 2 as it was (%+v), as the first of the current outbox", e, before.Pending[1])
	}
	if n, err := cur.Waiting("alice"); err != nil || n.Pending != 1 {
		t.Errorf("the current outbox holds %+v, %v; want the adopted entry waiting", n, err)
	}
	if n, err := old.Waiting("alice"); err != nil || n.Pending != 1 {
		t.Errorf("the old outbox holds %+v, %v; want only entry 1 left", n, err)
	}

	for _, tc := range []struct {
		name string
		from *Local
		seq  uint64
		want error
	}{
		{"an entry that is not there", old, 2, ErrNoEntry},
		{"another address", moved, 1, ErrOtherAddress},
	} {
		if _, err := cur.Adopt(tc.from, "alice", tc.seq); !errors.Is(err, tc.want) {
			t.Errorf("%s: Adopt = %v, want %v", tc.name, err, tc.want)
		}
	}
	if n, err := moved.Waiting("alice"); err != nil || n.Pending != 1 {
		t.Errorf("the outbox at another address holds %+v, %v; want its entry left", n, err)
	}
}

func TestTarget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ op, args, want string }{
		{proto.OpComment, `{"id":"sf-a","body":"x"}`, "sf-a"},
		{proto.OpFinish, `{"id":"sf-b"}`, "sf-b"},
		{proto.OpRemember, `{"key":"style","scope":"me","body":"x"}`, "style"},
		{proto.OpCreate, `{"title":"x"}`, ""},
		{proto.OpCreate, `{"id":"sf-mine","title":"x"}`, "sf-mine"},
		{proto.OpComment, `not json`, ""},
	} {
		if got := Target(outbox.Entry{Op: tc.op, Args: json.RawMessage(tc.args)}); got != tc.want {
			t.Errorf("Target(%s %s) = %q, want %q", tc.op, tc.args, got, tc.want)
		}
	}
}
