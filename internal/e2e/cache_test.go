package e2e

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ariesworx/starfix/internal/cli"
	"github.com/ariesworx/starfix/internal/mcpserver"
	"github.com/ariesworx/starfix/internal/offline"
	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

const cachedPrefix = "cached: starfix cannot reach its server, so this is this machine's copy from "

// hookContext runs prime --hook as u and returns the context it gives
// the harness, failing the test unless it exits 0 with one JSON
// document and nothing on stderr, as the hook contract says.
func (u *user) hookContext() string {
	u.w.t.Helper()
	r := u.run("v0.2.0", "prime", "--hook")
	var doc struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil || r.code != cli.ExitOK || r.stderr != "" || strings.Count(r.stdout, "\n") != 1 {
		u.w.t.Fatalf("sfx prime --hook = exit %d, %q (%v), stderr %q; want one JSON document and nothing else", r.code, r.stdout, err, r.stderr)
	}
	return doc.HookSpecificOutput.AdditionalContext
}

// TestCacheGateCLI is slice F3's gate for a person (design §18.10):
// with the daemon stopped, show and prime of the work alice holds, and
// prime --hook, answer from her machine's copy and say so; ready is
// refused with when the server was last reached and a fix; another
// principal on the same machine is never shown her copy; and once the
// server is back it is asked again.
func TestCacheGateCLI(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	// bob signs in from alice's machine: the same data directory.
	bob.env[offline.EnvDataDir] = alice.env[offline.EnvDataDir]
	id := strings.TrimSpace(alice.ok("create", "Held through the outage", "--body", "the plan"))
	alice.ok("start", id)
	alice.ok("prime")
	bob.ok("ready")

	w.stop()
	r := alice.run("v0.2.0", "show", id)
	if r.code != cli.ExitOK || !strings.Contains(r.stdout, "Held through the outage\n") || !strings.Contains(r.stdout, "the plan") ||
		!strings.HasPrefix(r.stderr, "sfx: "+cachedPrefix) || strings.Count(r.stderr, "\n") != 1 {
		t.Errorf("sfx show of held work with the server down = exit %d\nstdout %q\nstderr %q\nwant the issue, and one line on stderr that it is cached", r.code, r.stdout, r.stderr)
	}
	r = alice.run("v0.2.0", "show", id, "--json")
	var doc struct {
		Cached   bool
		CachedAt time.Time `json:"cached_at"`
		Issue    proto.Issue
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil || r.code != cli.ExitOK || !doc.Cached || doc.CachedAt.IsZero() || doc.Issue.ID != id {
		t.Errorf("sfx show --json with the server down = exit %d, %q (%v); want the issue with cached: true and when", r.code, r.stdout, err)
	}
	r = alice.run("v0.2.0", "prime")
	if r.code != cli.ExitOK || !strings.Contains(r.stdout, "you are alice") || !strings.Contains(r.stdout, "notice: "+cachedPrefix) ||
		!strings.Contains(r.stdout, "in progress:\n  "+id+" P2 \"Held through the outage\"\n") || !strings.HasPrefix(r.stderr, "sfx: "+cachedPrefix) {
		t.Errorf("sfx prime with the server down = exit %d\nstdout:\n%s\nstderr %q\nwant the cached prime with the held issue, saying it is cached", r.code, r.stdout, r.stderr)
	}
	if ctx := alice.hookContext(); !strings.Contains(ctx, "notice: "+cachedPrefix) || !strings.Contains(ctx, id) {
		t.Errorf("sfx prime --hook with the server down gave the context:\n%s\nwant the cached prime, saying it is cached", ctx)
	}
	r = alice.run("v0.2.0", "ready")
	if r.code != cli.ExitFailure || !strings.Contains(r.stderr, "sfx: starfixd is not running on the server; ready is not answered from this machine's copy, since it says what others are doing now (server last reached ") ||
		!strings.Contains(r.stderr, "; meanwhile `sfx prime` and `sfx show ID` answer from this machine's copy of what you read\n") {
		t.Errorf("sfx ready with the server down = exit %d, stderr:\n%s\nwant it refused, saying when the server was last reached, with a fix", r.code, r.stderr)
	}
	// bob, on the same machine, is never shown alice's copy.
	for _, args := range [][]string{{"show", id}, {"prime"}} {
		if r := bob.run("v0.2.0", args...); r.code != cli.ExitFailure || strings.Contains(r.stdout, "Held through") || strings.Contains(r.stderr, "cached") {
			t.Errorf("bob's sfx %s on alice's machine with the server down = exit %d, %q, %q; want her copy kept from him", strings.Join(args, " "), r.code, r.stdout, r.stderr)
		}
	}
	if ctx := bob.hookContext(); !strings.HasPrefix(ctx, "starfix: prime failed: ") || strings.Contains(ctx, id) {
		t.Errorf("bob's sfx prime --hook on alice's machine gave:\n%s\nwant the failure's note, and nothing of alice's", ctx)
	}

	// Back: the server is asked, and nothing says cached.
	w.start()
	alice.ok("update", id, "--title", "Renamed after the outage")
	if r := alice.run("v0.2.0", "show", id); r.code != cli.ExitOK || r.stderr != "" || !strings.Contains(r.stdout, "Renamed after the outage") {
		t.Errorf("sfx show with the server back = exit %d\nstdout %q\nstderr %q\nwant the server's answer", r.code, r.stdout, r.stderr)
	}
}

// callAll runs a tool and returns all of its texts and whether it was an
// error.
func (a *agent) callAll(name string, args map[string]any) ([]string, bool) {
	a.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := a.cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		a.t.Fatalf("%s: %v", name, err)
	}
	var out []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			out = append(out, tc.Text)
		}
	}
	return out, res.IsError
}

// TestCacheGateMCP is the same gate for an agent: prime and show of the
// issue it holds answer from the cache, each saying so in one terse
// line; ready is refused with a fix to carry on; and the SessionStart
// hook of the next session, through the CLI, finds the prime the agent
// read.
func TestCacheGateMCP(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{})
	alice := w.newUser("alice", "")
	alice.env["CLAUDE_CODE_SESSION_ID"] = "cc-e2e"
	alice.ok("create", "Held by the agent")
	ag := alice.mcp("v0.2.0")
	ag.ok("prime", nil)
	id := decode[mcpserver.Started](t, ag.ok("start", nil)).ID
	ag.ok("prime", nil) // now with the issue in progress

	w.stop()
	p := decode[mcpserver.Prime](t, ag.ok("prime", nil))
	if len(p.Notices) == 0 || !strings.HasPrefix(p.Notices[0], cachedPrefix) || len(p.Working) != 1 || p.Working[0].ID != id || len(p.Ready) != 0 {
		t.Errorf("prime with the server down = %+v; want the issue held, no ready ones, and a first notice that it is cached", p)
	}
	texts, isErr := ag.callAll("show", map[string]any{"id": id})
	if isErr || len(texts) != 2 || decode[mcpserver.Issue](t, texts[0]).Title != "Held by the agent" || !strings.HasPrefix(texts[1], cachedPrefix) {
		t.Errorf("show of the held issue with the server down = %q (error %v); want the issue and one line that it is cached", texts, isErr)
	}
	if out, isErr := ag.call("ready", nil); !isErr || !strings.Contains(out, "is not answered from this machine's copy") ||
		!strings.Contains(out, "fix: carry on with the work you hold") {
		t.Errorf("ready with the server down = %q (error %v); want it refused, with a fix to carry on", out, isErr)
	}
	if ctx := alice.hookContext(); !strings.Contains(ctx, "notice: "+cachedPrefix) || !strings.Contains(ctx, id) {
		t.Errorf("the next session's prime --hook with the server down gave:\n%s\nwant the prime the agent read, saying it is cached", ctx)
	}
}

// TestAccessEndedDeletesTheCache: the first handshake refused as access
// ended deletes what the principal read of the project from the
// machine, says so, and nothing of it is shown afterward (design §18.6).
func TestAccessEndedDeletesTheCache(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{many: true, admins: []string{"dana"}})
	w.member("alice", project, store.RoleMember)
	alice := w.newUser("alice", "")
	id := strings.TrimSpace(alice.ok("create", "Read before access ended"))
	alice.ok("show", id)
	alice.ok("prime")
	if out := alice.ok("cache"); !strings.HasPrefix(out, "alice's cache: 2 answers, ") {
		t.Fatalf("sfx cache before access ended:\n%s", out)
	}
	if _, err := w.ctl.RemoveMember(t.Context(), dana, "alice", project); err != nil {
		t.Fatal(err)
	}
	r := alice.run("v0.2.0", "ready")
	if r.code != cli.ExitFailure || !strings.Contains(r.stderr, "sfx: cache: 2 answers alice read from this project were deleted from this machine\n") {
		t.Errorf("the first command after access ended = exit %d, stderr:\n%s\nwant the cache deleted, and a line that says so", r.code, r.stderr)
	}
	if out := alice.ok("cache"); out != "this machine keeps nothing read from the server for this project\n" {
		t.Errorf("sfx cache after access ended:\n%s", out)
	}
	w.stop()
	if r := alice.run("v0.2.0", "show", id); r.code != cli.ExitFailure || strings.Contains(r.stdout, "Read before") {
		t.Errorf("sfx show with the server down, after access ended = exit %d, %q; want nothing shown", r.code, r.stdout)
	}
}
