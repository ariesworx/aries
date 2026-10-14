package offline

import (
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
	if was, err := l.Reached("", keyA, true); err != nil || was != "" || who(l, true, keyA) != "" {
		t.Errorf("a handshake that names no principal was recorded (%v)", err)
	}
	// alice signs in with no key configured, and her agent with its own.
	if _, err := l.Reached("alice", keyA, false); err != nil {
		t.Fatalf("Reached = %v", err)
	}
	if _, err := l.Reached("alice-agent", keyB, true); err != nil {
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
	was, err := l.Reached("bob", keyA, false)
	if err != nil || was != "alice" || who(l, false, keyA) != "bob" {
		t.Errorf("Reached(bob) = %q, %v, and Principal = %q; want alice, who the key was, and then bob", was, err, who(l, false, keyA))
	}
	if was, err := l.Reached("bob", keyA, false); err != nil || was != "" {
		t.Errorf("a second handshake as bob = %q, %v; want no change", was, err)
	}

	// The record is the server, the project, and for each key its
	// fingerprint, the principal and when: nothing else.
	b, err := os.ReadFile(filepath.Join(l.dir, seenFile))
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`{"v":1,"server":%q,"project":%q,"keys":{%q:{"principal":"bob","at":"2026-10-13T15:02:00Z"},%q:{"principal":"alice-agent","at":"2026-10-13T14:02:00Z"}},"unconfigured":%q}`+"\n",
		hostKey, project, keyA, keyB, keyA)
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
			if _, err := l.Reached("alice", keyA, true); err != nil {
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
				_, err = l.Reached("bob", keyB, true)
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
	if _, err := l.Reached("alice", keyA, true); err != nil {
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
}

func (f *fakeConn) Principal() string { return f.who }
func (f *fakeConn) Err() error        { return f.lost }

func (f *fakeConn) Call(_ context.Context, op string, args, _ any) error {
	b, err := json.Marshal(args)
	if err != nil {
		return err
	}
	var a struct {
		ID, Key string
		Paths   []string
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	name := op + " " + a.ID + a.Key
	if op == proto.OpHandoff && len(a.Paths) == 0 {
		name += " -paths"
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
	if want := []string{"handoff sf-a", "handoff sf-a -paths"}; !slices.Equal(c.calls, want) {
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
