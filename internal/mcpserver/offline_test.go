package mcpserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/internal/client"
	"github.com/ariesworx/aries/internal/offline"
	"github.com/ariesworx/aries/internal/outbox"
	"github.com/ariesworx/aries/internal/proto"
)

// errDown is what a dial returns while the server is away.
var errDown = proto.Errf(proto.CodeUnavailable, "check server.host and server.port in .aries.yaml, and your network",
	"cannot reach 127.0.0.1:2222: connection refused")

// outage is a server that comes and goes: while down, a dial fails as
// unreachable; while up, it hands out the next of its connections.
type outage struct {
	mu    sync.Mutex
	down  bool
	conns []*fakeConn
	n     int
}

func (o *outage) dial(context.Context) (Conn, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.down || o.n >= len(o.conns) {
		return nil, errDown
	}
	o.n++
	return o.conns[o.n-1], nil
}

func (o *outage) set(down bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.down = down
}

// away is an MCP session with an outbox: a checkout with a key the
// server takes for alice, a data directory of its own, and the server
// that comes and goes.
type away struct {
	cs  *mcp.ClientSession
	srv *Server
	off *offline.Client
	net *outage
	key string
}

// checkout makes a checkout with a key file and a data directory of its
// own, and returns the way to its outboxes and the key's fingerprint.
func checkout(t *testing.T) (*offline.Client, string) {
	t.Helper()
	repo, data := t.TempDir(), t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil { //nolint:gosec // a directory: 0700 is owner-only
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "id_ed25519"), pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	cfg := "project: 6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f\nserver:\n  host: 127.0.0.1\n  port: 2222\n" +
		"  host_key: SHA256:et6CqkKsyU2BxzA7Ws+V18rXrtM9Gj6dk1/m0C5TqvU\nkey: id_ed25519\n"
	if err := os.WriteFile(filepath.Join(repo, client.ConfigFile), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{offline.EnvDataDir: data}
	home := t.TempDir()
	return &offline.Client{Dir: repo, GOOS: "linux", Getenv: func(k string) string { return env[k] },
		UserHomeDir: func() (string, error) { return home, nil }, Session: "s-test", Machine: "laptop"}, ssh.FingerprintSHA256(sp)
}

func newAway(t *testing.T, conns ...*fakeConn) *away {
	t.Helper()
	a := &away{net: &outage{conns: conns}}
	a.off, a.key = checkout(t)
	for _, c := range conns {
		c.who, c.key = "alice", a.key
	}
	var err error
	a.srv = New(Options{Version: "v0.2.0", Dial: a.net.dial, Offline: a.off, Cache: a.off, RenewEvery: -1})
	t.Cleanup(func() { _ = a.srv.Close() })
	st, ct := mcp.NewInMemoryTransports()
	if _, err := a.srv.MCP().Connect(t.Context(), st, nil); err != nil {
		t.Fatal(err)
	}
	a.cs, err = mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.cs.Close() })
	return a
}

// list is what waits in alice's outbox.
func (a *away) list(t *testing.T) outbox.Listing {
	t.Helper()
	l, _, err := a.off.Local()
	if err != nil {
		t.Fatal(err)
	}
	ob, ok, err := l.Existing("alice")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return outbox.Listing{}
	}
	list, err := ob.List()
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// drop breaks the connection now open, as a network failure does.
func drop(c *fakeConn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lost = errLost
}

const queuedLine = "queued: aries cannot reach its server, so this %s waits on this machine and is sent when the server answers. Carry on, and do not repeat it."

// TestQueuedToolCalls pins what an agent is told when a write does not
// reach the server and is queued: one terse line that says so and that
// it should carry on, which is not an error; and the write is then in
// the outbox, with the key it would have been sent with.
func TestQueuedToolCalls(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		tool string
		args map[string]any
		op   string
		want string
	}{
		{"finish", map[string]any{"id": "ar-1", "reason": "done", "handoff": "all tests pass"}, proto.OpFinish, fmt.Sprintf(queuedLine, "finish")},
		{"handoff", map[string]any{"id": "ar-1", "note": "half done"}, proto.OpHandoff, fmt.Sprintf(queuedLine, "handoff")},
		{"comment", map[string]any{"id": "ar-1", "body": "decided on tabs"}, proto.OpComment, fmt.Sprintf(queuedLine, "comment")},
		{"remember", map[string]any{"key": "style", "body": "tabs"}, proto.OpRemember, fmt.Sprintf(queuedLine, "remember")},
		{"create", map[string]any{"title": "Something new"}, proto.OpCreate,
			fmt.Sprintf(queuedLine, "create") + " The issue has no id until then, so nothing can refer to it."},
		{"update", map[string]any{"id": "ar-1", "rev": 2, "title": "renamed"}, proto.OpUpdate, fmt.Sprintf(queuedLine, "update")},
		{"dep", map[string]any{"action": "add", "id": "ar-1", "depends_on": "ar-2"}, proto.OpDepAdd, fmt.Sprintf(queuedLine, "dep.add")},
		{"label", map[string]any{"action": "rm", "id": "ar-1", "labels": []string{"old"}}, proto.OpLabelRm, fmt.Sprintf(queuedLine, "label.rm")},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			a := newAway(t, &fakeConn{reply: okReply})
			if res := callTool(t, a.cs, "ready", nil); res.IsError {
				t.Fatalf("ready before the outage: %s", text(t, res))
			}
			drop(a.net.conns[0])
			a.net.set(true)
			res := callTool(t, a.cs, tc.tool, tc.args)
			if got := text(t, res); res.IsError || got != tc.want {
				t.Errorf("%s while the server is away = %q (error %v)\nwant %q, and no error", tc.tool, got, res.IsError, tc.want)
			}
			if n := Tokens([]byte(text(t, res))); n > 80 {
				t.Errorf("the queued line is %d tokens, want it terse", n)
			}
			var doc struct {
				Queued bool
				Op     string
				Entry  int
				ID     string
				Rev    int
			}
			b, err := json.Marshal(res.StructuredContent)
			if err != nil || json.Unmarshal(b, &doc) != nil || !doc.Queued || doc.Op != tc.op || doc.Entry != 1 || doc.ID != "" || doc.Rev != 0 {
				t.Errorf("%s: structured content %s, want queued: true with the op and the entry, and no id or rev (%v)", tc.tool, b, err)
			}
			list := a.list(t)
			if len(list.Pending) != 1 || list.Pending[0].Op != tc.op || !strings.HasPrefix(list.Pending[0].Idem, "mcp-") ||
				list.Pending[0].Session != "s-test" || list.Pending[0].Machine != "laptop" {
				t.Errorf("%s: the outbox holds %+v, want the one %s with its key, session and machine", tc.tool, list.Pending, tc.op)
			}
		})
	}
}

// Proto says the fake speaks this build's protocol, as a server of this
// build does.
func (f *fakeConn) Proto() int { return proto.Proto }

// A label call of several labels that cannot reach the server queues a
// write for each, in order, each with a key of its own, and says so in
// one line; an update without a rev is not queued, since only the
// server knows the rev, and the agent is told to give one.
func TestQueuedSeveralAndUnqueuedUpdate(t *testing.T) {
	t.Parallel()
	a := newAway(t, &fakeConn{reply: okReply})
	if res := callTool(t, a.cs, "ready", nil); res.IsError {
		t.Fatalf("ready before the outage: %s", text(t, res))
	}
	drop(a.net.conns[0])
	a.net.set(true)
	res := callTool(t, a.cs, "label", map[string]any{"action": "add", "id": "ar-1", "labels": []string{"a", "b", "c"}})
	want := "queued: aries cannot reach its server, so these 3 writes (label.add) wait on this machine and are sent when the server answers. Carry on, and do not repeat them."
	if got := text(t, res); res.IsError || got != want {
		t.Errorf("label of three while the server is away = %q (error %v)\nwant %q", got, res.IsError, want)
	}
	list := a.list(t)
	var labels, keys []string
	for _, e := range list.Pending {
		var args proto.LabelArgs
		if err := json.Unmarshal(e.Args, &args); err != nil || e.Op != proto.OpLabelAdd {
			t.Fatalf("the outbox holds %s %s (%v), want label.add", e.Op, e.Args, err)
		}
		labels, keys = append(labels, args.Label), append(keys, e.Idem)
	}
	if !slices.Equal(labels, []string{"a", "b", "c"}) || len(slices.Compact(slices.Sorted(slices.Values(keys)))) != 3 {
		t.Errorf("the outbox holds labels %v with keys %v; want a, b and c, each with a key of its own", labels, keys)
	}

	res = callTool(t, a.cs, "update", map[string]any{"id": "ar-1", "title": "renamed"})
	if got := text(t, res); !res.IsError || !strings.Contains(got,
		"this was not queued (an update is kept for later only with the rev it was based on, which only the server knows: give rev)") {
		t.Errorf("update without a rev while the server is away = %q (error %v); want it not queued, saying to give the rev", got, res.IsError)
	}
	if n := len(a.list(t).Pending); n != 3 {
		t.Errorf("the outbox holds %d entries after the update, want the 3 labels alone", n)
	}
}

// A write lost with its connection, after the server may have applied
// it, is queued with the key it was sent with: that is what makes
// sending it again safe.
func TestQueuedAfterADropKeepsItsKey(t *testing.T) {
	t.Parallel()
	var sent []string
	first := &fakeConn{reply: func(_ string, args any) (any, error) {
		if a, ok := args.(proto.CommentArgs); ok {
			sent = append(sent, a.Idem)
			return nil, errLost
		}
		return proto.Empty{}, nil
	}}
	a := newAway(t, first)
	if res := callTool(t, a.cs, "ready", nil); res.IsError {
		t.Fatal(text(t, res))
	}
	a.net.set(true) // the retry's dial finds the server gone
	res := callTool(t, a.cs, "comment", map[string]any{"id": "ar-1", "body": "hi"})
	if res.IsError || !strings.HasPrefix(text(t, res), "queued: ") {
		t.Fatalf("comment on a connection that drops = %q (error %v), want it queued", text(t, res), res.IsError)
	}
	list := a.list(t)
	if len(sent) != 1 || len(list.Pending) != 1 || list.Pending[0].Idem != sent[0] {
		t.Errorf("the comment went out with key %v and waits with %+v; want the same key", sent, list.Pending)
	}
}

// What is never queued fails as it did, and tells the agent that it was
// not queued and to carry on with what it holds: no claim is taken
// without the server (decision 39).
func TestUnqueuedToolCalls(t *testing.T) {
	t.Parallel()
	a := newAway(t)
	a.net.set(true)
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"start", nil, "it takes a claim, and no claim is made without the server"},
		{"close", map[string]any{"id": "ar-1"}, "it ends something on a state the client cannot see"},
	} {
		res := callTool(t, a.cs, tc.tool, tc.args)
		want := "unavailable: cannot reach 127.0.0.1:2222: connection refused\n" +
			"fix: aries cannot reach its server, and this was not queued (" + tc.want + "): carry on with the work you hold and call it again later; if it still fails, tell the user"
		if got := text(t, res); !res.IsError || got != want {
			t.Errorf("%s while the server is away = %q (error %v)\nwant %q", tc.tool, got, res.IsError, want)
		}
	}
	if res := callTool(t, a.cs, "ready", nil); !res.IsError || strings.Contains(text(t, res), "queued") {
		t.Errorf("ready, a read, while the server is away = %q; want the error it always gave", text(t, res))
	}
	if list := a.list(t); len(list.Pending) != 0 {
		t.Errorf("the outbox holds %+v, want nothing", list.Pending)
	}
}

// The agent is never asked to manage the outbox (rule 12): what the
// outbox refuses is reworded as its next step, or handed to the user.
func TestOutboxRefusalsForAnAgent(t *testing.T) {
	t.Parallel()
	t.Run("this machine never reached the server", func(t *testing.T) {
		a := newAway(t)
		a.net.set(true)
		res := callTool(t, a.cs, "comment", map[string]any{"id": "ar-1", "body": "hi"})
		want := "unavailable: cannot reach 127.0.0.1:2222: connection refused, and this comment was not queued: " +
			"this machine has never reached the server with this key, so aries cannot tell whose write it is\n" +
			"fix: aries cannot reach its server and could not keep this for later: keep the text, call it again when a call succeeds, and tell the user"
		if got := text(t, res); !res.IsError || got != want {
			t.Errorf("comment = %q (error %v)\nwant %q", got, res.IsError, want)
		}
	})
	t.Run("a secret", func(t *testing.T) {
		a := newAway(t, &fakeConn{reply: okReply})
		callTool(t, a.cs, "ready", nil)
		drop(a.net.conns[0])
		a.net.set(true)
		//nolint:gosec // G101: an invented token, which the outbox must refuse
		res := callTool(t, a.cs, "comment", map[string]any{"id": "ar-1", "body": "the token is ghp_0123456789abcdefghijABCDEFGHIJ012345"})
		got := text(t, res)
		if !res.IsError || !strings.HasPrefix(got, "invalid: ") || strings.Contains(got, "ghp_") || strings.Contains(got, "aries outbox") ||
			!strings.HasSuffix(got, "\nfix: never store a secret: remember the rest, naming where the secret is kept (a vault path or an environment variable) instead") {
			t.Errorf("comment with a token = %q (error %v); want the refusal, the next step, and neither the token nor a command", got, res.IsError)
		}
	})
}

// On the next connection what waits is sent first, in order, before the
// request that opened the connection; and a refusal among it reaches a
// person through prime's notices.
func TestQueuedWritesGoFirstOnTheNextConnection(t *testing.T) {
	t.Parallel()
	second := &fakeConn{reply: func(op string, args any) (any, error) {
		if b, _ := json.Marshal(args); op == proto.OpComment && strings.Contains(string(b), "ar-gone") {
			return nil, proto.Errf(proto.CodeNotFound, "find the id with `aries list`", "issue ar-gone not found")
		}
		return proto.Empty{}, nil
	}}
	a := newAway(t, &fakeConn{reply: okReply}, second)
	callTool(t, a.cs, "ready", nil)
	drop(a.net.conns[0])
	a.net.set(true)
	for _, id := range []string{"ar-1", "ar-gone", "ar-2"} {
		if res := callTool(t, a.cs, "comment", map[string]any{"id": id, "body": "hi"}); res.IsError {
			t.Fatalf("comment while away: %s", text(t, res))
		}
	}
	a.net.set(false)
	res := callTool(t, a.cs, "prime", nil)
	if res.IsError {
		t.Fatalf("prime when the server is back: %s", text(t, res))
	}
	if got, want := second.ops(), []string{proto.OpComment, proto.OpComment, proto.OpComment}; len(got) < 4 || !slices.Equal(got[:3], want) {
		t.Errorf("the new connection carried %v, want the three comments before prime's own requests", second.ops())
	}
	var p Prime
	if err := json.Unmarshal([]byte(text(t, res)), &p); err != nil {
		t.Fatal(err)
	}
	want := "outbox: 1 write made while the server was away was refused: tell the user to run `aries outbox`"
	if !slices.Contains(p.Notices, want) {
		t.Errorf("prime's notices are %q, want among them %q", p.Notices, want)
	}
	if list := a.list(t); len(list.Pending) != 0 || len(list.Refused) != 1 {
		t.Errorf("the outbox holds %d pending and %d refused, want 0 and 1", len(list.Pending), len(list.Refused))
	}
}

// A session that makes no tool call still sends what waits: the renewal
// tick connects when something does.
func TestRenewSendsWhatWaits(t *testing.T) {
	t.Parallel()
	second := &fakeConn{reply: okReply}
	a := newAway(t, &fakeConn{reply: okReply}, second)
	callTool(t, a.cs, "ready", nil)
	drop(a.net.conns[0])
	a.net.set(true)
	callTool(t, a.cs, "handoff", map[string]any{"id": "ar-1", "note": "half done"})
	a.srv.Renew(t.Context()) // still away: nothing to do, and no harm
	if len(a.list(t).Pending) != 1 {
		t.Fatal("the handoff left the outbox while the server was away")
	}
	a.net.set(false)
	a.srv.Renew(t.Context())
	if got := second.ops(); len(got) == 0 || got[0] != proto.OpHandoff {
		t.Errorf("after a renewal with the server back, the connection carried %v, want the handoff first", got)
	}
	if n := len(a.list(t).Pending); n != 0 {
		t.Errorf("%d entries still wait after the renewal", n)
	}
}

// A connection whose replay found another process replaying the same
// outbox (busy) sends what waits at the next renewal, on the connection
// it has: otherwise a session that stays connected never sends it, and,
// if the other process stops, it waits until the connection drops. The
// partition soak found it: an outbox waited minutes with none sent.
func TestRenewSendsWhatABusyReplayLeft(t *testing.T) {
	t.Parallel()
	conn := &fakeConn{reply: okReply}
	a := newAway(t, &fakeConn{reply: okReply}, conn)
	callTool(t, a.cs, "ready", nil)
	drop(a.net.conns[0])
	a.net.set(true)
	callTool(t, a.cs, "handoff", map[string]any{"id": "ar-1", "note": "half done"})
	l, _, err := a.off.Local()
	if err != nil {
		t.Fatal(err)
	}
	ob, _, err := l.Existing("alice")
	if err != nil {
		t.Fatal(err)
	}
	// Another process replays the outbox while the session reconnects:
	// the session's replay is busy, and sends nothing.
	held, release := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = ob.Replay(context.Background(), func(context.Context, outbox.Entry) (outbox.Outcome, error) {
			close(held)
			<-release
			return outbox.Outcome{}, errLost // not reached: the entry stays
		})
	}()
	<-held
	a.net.set(false)
	callTool(t, a.cs, "ready", nil)
	close(release)
	<-done
	if n := len(a.list(t).Pending); n != 1 {
		t.Fatalf("%d entries wait after the busy replay, want the handoff", n)
	}
	a.srv.Renew(t.Context())
	if got := conn.ops(); !slices.Contains(got, proto.OpHandoff) {
		t.Errorf("the connection carried %v, want the handoff sent at the renewal", got)
	}
	if n := len(a.list(t).Pending); n != 0 {
		t.Errorf("%d entries still wait after the renewal", n)
	}
}

// Without an outbox the server does as it always did.
func TestNoOutboxNoQueue(t *testing.T) {
	t.Parallel()
	o := &outage{down: true}
	s := New(Options{Version: "v0.2.0", Dial: o.dial, RenewEvery: -1})
	t.Cleanup(func() { _ = s.Close() })
	st, ct := mcp.NewInMemoryTransports()
	if _, err := s.MCP().Connect(t.Context(), st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	res := callTool(t, cs, "comment", map[string]any{"id": "ar-1", "body": "hi"})
	if got := text(t, res); !res.IsError || !strings.Contains(got, "fix: tell the user aries cannot connect") {
		t.Errorf("comment with no outbox = %q (error %v), want the dial error", got, res.IsError)
	}
}
