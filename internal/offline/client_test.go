package offline

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/starfix/internal/client"
	"github.com/ariesworx/starfix/internal/clienttest"
	"github.com/ariesworx/starfix/internal/outbox"
	"github.com/ariesworx/starfix/internal/proto"
)

// TestMain keeps every test here away from the real home directory,
// ssh-agent and data directory (clienttest).
func TestMain(m *testing.M) {
	restore := clienttest.Isolate()
	code := m.Run()
	restore()
	os.Exit(code)
}

// repo is a checkout whose .starfix.yaml names a server nothing runs,
// with two key files: `key:` names the first.
type repo struct {
	dir        string
	mine, also string // the keys' fingerprints
}

func newRepo(t *testing.T) repo {
	t.Helper()
	r := repo{dir: t.TempDir()}
	write := func(name string) string {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		block, err := ssh.MarshalPrivateKey(priv, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r.dir, name), pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
		sp, err := ssh.NewPublicKey(pub)
		if err != nil {
			t.Fatal(err)
		}
		return ssh.FingerprintSHA256(sp)
	}
	r.mine, r.also = write("id_mine"), write("id_also")
	cfg := fmt.Sprintf("project: %s\nserver:\n  host: 127.0.0.1\n  port: 2222\n  host_key: %s\nkey: id_mine\n", project, hostKey)
	if err := os.WriteFile(filepath.Join(r.dir, client.ConfigFile), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return r
}

// newClient returns a Client in r with a data directory of its own and
// env as its environment.
func (r repo) newClient(t *testing.T, env map[string]string) *Client {
	t.Helper()
	all := map[string]string{EnvDataDir: ownDir(t)}
	for k, v := range env {
		all[k] = v
	}
	return &Client{Dir: r.dir, GOOS: "linux", Getenv: func(k string) string { return all[k] },
		UserHomeDir: func() (string, error) { return t.TempDir(), nil },
		Now:         func() time.Time { return t0 }, Session: "cli", Machine: "laptop"}
}

// keyedConn is a connection that says which key signed it in.
type keyedConn struct {
	fakeConn
	key string
}

func (c *keyedConn) Key() client.KeyInfo {
	return client.KeyInfo{Fingerprint: c.key, Source: "file", By: "key: in " + client.ConfigFile}
}

// A key the server gives to another principal no longer sends what the
// first one queued with it: the entries stay, and the person is told
// once, in one line with a fix.
func TestConnectedSaysWhenAKeyMoved(t *testing.T) {
	t.Parallel()
	r := newRepo(t)
	c := r.newClient(t, nil)
	if got := c.Connected(&keyedConn{fakeConn: fakeConn{who: "alice"}, key: r.mine}); got != "" {
		t.Errorf("Connected at the first handshake = %q, want nothing to say", got)
	}
	args := proto.CommentArgs{ID: "sf-a", Body: "a note", Idem: proto.NewIdem("cli")}
	if _, err := c.Keep("", proto.OpComment, args, errReach); err != nil {
		t.Fatal(err)
	}
	bob := &keyedConn{fakeConn: fakeConn{who: "bob"}, key: r.mine}
	want := "outbox: the server now takes this key for bob, not alice, so 1 write queued as alice is not sent with it; " +
		"fix: sign in with a key of alice's to send them, or run `sfx outbox --principal alice` to see or drop them"
	if got := c.Connected(bob); got != want {
		t.Errorf("Connected after the key moved = %q\nwant %q", got, want)
	}
	if rep, err := c.Replay(t.Context(), bob); err != nil || rep.Delivered != 0 || len(bob.calls) != 0 {
		t.Errorf("Replay as bob = %+v, %v, sending %v; want nothing of alice's sent", rep, err, bob.calls)
	}
	if got := c.Connected(bob); got != "" {
		t.Errorf("Connected again as bob = %q, want it said once", got)
	}
	l, _, err := c.Local()
	if err != nil {
		t.Fatal(err)
	}
	if n, err := l.Waiting("alice"); err != nil || n.Pending != 1 {
		t.Errorf("Waiting(alice) = %+v, %v; want her entry left where it was", n, err)
	}
}

// TestClientKeepsForWhoeverTheKeyWas pins the rule from the keys sfx
// would offer to an outbox: nothing is queued until the server has taken
// one of them for someone, and the write then goes to that principal's
// outbox, whichever key the environment names now.
func TestClientKeepsForWhoeverTheKeyWas(t *testing.T) {
	t.Parallel()
	r := newRepo(t)
	args := proto.CommentArgs{ID: "sf-a", Body: "a note", Idem: proto.NewIdem("cli")}

	c := r.newClient(t, nil)
	_, err := c.Keep("", proto.OpComment, args, errReach)
	if pe, ok := errors.AsType[*proto.Error](err); !ok || !errors.Is(err, ErrNotQueued) || !strings.Contains(pe.Message, "never reached the server with this key") {
		t.Fatalf("Keep before any handshake = %v, want the refusal that says this machine never reached the server", err)
	}

	c.Connected(&keyedConn{fakeConn: fakeConn{who: "alice"}, key: r.mine})
	kept, err := c.Keep("", proto.OpComment, args, errReach)
	if err != nil || kept.Principal != "alice" || kept.Entry.Session != "cli" || kept.Entry.Machine != "laptop" {
		t.Fatalf("Keep after alice's handshake = %+v, %v; want it in alice's outbox, with the session and machine", kept, err)
	}

	// Another key, which the server never took: nobody's write.
	also := filepath.Join(r.dir, "id_also")
	other := &Client{}
	*other = *c // the same machine and data directory
	other.Getenv = withEnv(c.Getenv, client.EnvKeyFile, also)
	if _, err := other.Keep("", proto.OpComment, args, errReach); !errors.Is(err, ErrNotQueued) {
		t.Errorf("Keep with a key the server never took = %v, want a refusal", err)
	}
	// The server took it for the agent's principal: its writes go to that
	// outbox and not alice's, and alice's key still means alice.
	other.Connected(&keyedConn{fakeConn: fakeConn{who: "alice-agent"}, key: r.also})
	if kept, err := other.Keep("", proto.OpComment, args, errReach); err != nil || kept.Principal != "alice-agent" {
		t.Errorf("Keep with the agent's key = %+v, %v; want it in alice-agent's outbox", kept, err)
	}
	if kept, err := c.Keep("", proto.OpComment, args, errReach); err != nil || kept.Principal != "alice" {
		t.Errorf("Keep with alice's key = %+v, %v; want it in alice's outbox", kept, err)
	}

	// Each connection delivers its own principal's outbox and no other.
	agent := &keyedConn{fakeConn: fakeConn{who: "alice-agent"}, key: r.also}
	if rep, err := other.Replay(t.Context(), agent); err != nil || rep.Delivered != 1 || len(agent.calls) != 1 {
		t.Errorf("Replay as alice-agent = %+v, %v, sending %v; want its one entry", rep, err, agent.calls)
	}
	l, _, err := c.Local()
	if err != nil {
		t.Fatal(err)
	}
	if n, err := l.Waiting("alice"); err != nil || n.Pending != 2 {
		t.Errorf("Waiting(alice) after the agent's replay = %+v, %v; want her 2 entries untouched", n, err)
	}
}

// A write lost with its connection belongs to that connection's
// principal, which the server said: no guess is made.
func TestClientKeepsForTheConnectionsPrincipal(t *testing.T) {
	t.Parallel()
	c := newRepo(t).newClient(t, nil)
	kept, err := c.Keep("bob", proto.OpHandoff, proto.HandoffArgs{ID: "sf-a", Note: "n", Idem: proto.NewIdem("cli")}, errReach)
	if err != nil || kept.Principal != "bob" || kept.Entry.Seq != 1 {
		t.Errorf("Keep(bob) = %+v, %v; want entry 1 of bob's outbox", kept, err)
	}
}

func TestClientKeepWithoutADataDirectory(t *testing.T) {
	t.Parallel()
	c := newRepo(t).newClient(t, map[string]string{EnvDataDir: "relative"})
	_, err := c.Keep("alice", proto.OpComment, proto.CommentArgs{ID: "sf-a", Body: "b", Idem: "cli-1"}, errReach)
	pe, ok := errors.AsType[*proto.Error](err)
	if !ok || !errors.Is(err, ErrNotQueued) || pe.Code != proto.CodeUnavailable || !strings.Contains(pe.Fix, EnvDataDir) ||
		!strings.Contains(pe.Message, "was not queued") {
		t.Errorf("Keep without a data directory = %v, want a refusal whose fix names %s", err, EnvDataDir)
	}
}

func TestNotice(t *testing.T) {
	t.Parallel()
	c := newRepo(t).newClient(t, nil)
	c.Outbox = outbox.Options{}
	conn := &fakeConn{who: "alice", answers: map[string]error{
		"sf-gone": proto.Errf(proto.CodeNotFound, "find the id with `sfx list`", "issue sf-gone not found"),
		"sf-slow": proto.Errf(proto.CodeUnavailable, "retry", "the server could not complete comment"),
	}}
	if got := c.Notice(conn); got != "" {
		t.Errorf("Notice with nothing queued = %q, want none", got)
	}
	keep := func(id string) {
		t.Helper()
		if _, err := c.Keep("alice", proto.OpComment, proto.CommentArgs{ID: id, Body: "b", Idem: proto.NewIdem("cli")}, errReach); err != nil {
			t.Fatal(err)
		}
	}
	keep("sf-gone")
	if got, want := c.Notice(conn), "outbox: 1 write made while the server was away waits to be sent, which sfx does by itself"; got != want {
		t.Errorf("Notice = %q, want %q", got, want)
	}
	keep("sf-slow")
	keep("sf-b")
	if _, err := c.Replay(t.Context(), conn); err != nil {
		t.Fatal(err)
	}
	want := "outbox: 1 write made while the server was away was refused and 2 wait to be sent: tell the user to run `sfx outbox`"
	if got := c.Notice(conn); got != want {
		t.Errorf("Notice = %q, want %q", got, want)
	}
	if got := c.Notice(&fakeConn{who: "bob"}); got != "" {
		t.Errorf("Notice for bob = %q, want none: the outbox is alice's", got)
	}
}

func TestNoQueue(t *testing.T) {
	t.Parallel()
	for v, want := range map[string]bool{"": false, "0": false, "false": false, "1": true, "true": true, "yes": true} {
		if got := NoQueue(func(k string) string { return map[string]string{EnvNoQueue: v}[k] }); got != want {
			t.Errorf("NoQueue with %s=%q = %v, want %v", EnvNoQueue, v, got, want)
		}
	}
}

func withEnv(getenv func(string) string, key, value string) func(string) string {
	return func(k string) string {
		if k == key {
			return value
		}
		return getenv(k)
	}
}
