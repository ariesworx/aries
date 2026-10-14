package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/cli"
	"github.com/ariesworx/starfix/internal/mcpserver"
	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// ops returns the ops of an issue's events, oldest first, and the
// sessions that made them.
func (u *user) history(id string) (ops, sessions []string) {
	u.w.t.Helper()
	h := decode[proto.HistoryResult](u.w.t, u.ok("history", id, "--json"))
	for _, e := range h.Events {
		ops, sessions = append(ops, e.Op), append(sessions, e.Session)
	}
	return ops, sessions
}

// tally is how many of xs are x.
func tally(xs []string, x string) int {
	n := 0
	for _, v := range xs {
		if v == x {
			n++
		}
	}
	return n
}

// titled counts the project's issues with this title, closed ones too.
func (u *user) titled(title string) int {
	u.w.t.Helper()
	n := 0
	for _, is := range decode[proto.ListResult](u.w.t, u.ok("list", "--all", "--json")).Issues {
		if is.Title == title {
			n++
		}
	}
	return n
}

// queuedDoc is what `sfx --json` prints for a write that was queued.
type queuedDoc struct {
	Queued bool
	Entry  int
	Op     string
	ID     string
	Rev    int
}

// TestOutageWritesArriveOnceCLI is slice F1's gate, for a person at the
// CLI (design §18.10): a handoff, a comment, a new issue and the finish
// of a held issue, written while the server is away, each arrive once
// when it is back; and what is never queued fails as it always did.
func TestOutageWritesArriveOnceCLI(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{})
	alice := w.newUser("alice", "")
	held := strings.TrimSpace(alice.ok("create", "Held through the outage"))
	other := strings.TrimSpace(alice.ok("create", "Another issue"))
	alice.ok("start", held)

	w.stop()
	for i, args := range [][]string{
		{"handoff", held, "half done", "--next", "the tests"},
		{"comment", other, "written while away"},
		{"create", "Found while away"},
		{"finish", held, "--reason", "done", "--handoff", "all of it"},
	} {
		r := alice.run("v0.2.0", append(args, "--json")...)
		doc := decode[queuedDoc](t, r.stdout)
		if r.code != cli.ExitOK || !doc.Queued || doc.Entry != i+1 || doc.ID != "" || doc.Rev != 0 || r.stderr != "" {
			t.Fatalf("sfx %s with the server away = exit %d, %q, stderr %q; want it queued as entry %d", strings.Join(args, " "), r.code, r.stdout, r.stderr, i+1)
		}
	}
	if out := alice.ok("comment", other, "a second note"); !strings.HasPrefix(out, "queued (server unreachable since ") ||
		!strings.Contains(out, "): entry 5, comment "+other+";") {
		t.Errorf("a queued comment printed %q", out)
	}
	// Nothing that takes a claim, or ends something, is queued.
	for _, args := range [][]string{{"start", other}, {"close", other}} {
		r := alice.run("v0.2.0", args...)
		if r.code != cli.ExitFailure || !strings.Contains(r.stderr, "sfx: starfixd is not running on the server\n") ||
			!strings.Contains(r.stderr, "this was not queued for later, since ") {
			t.Errorf("sfx %s with the server away = exit %d, %q; want it to fail, saying it was not queued", strings.Join(args, " "), r.code, r.stderr)
		}
	}
	// --no-queue: a script that must know.
	if r := alice.run("v0.2.0", "--no-queue", "comment", other, "must arrive now"); r.code != cli.ExitFailure || r.stdout != "" ||
		r.stderr != "sfx: starfixd is not running on the server\nfix: ask the server admin to start it (`systemctl start starfixd`)\n" {
		t.Errorf("sfx --no-queue comment with the server away = exit %d, %q, %q; want the failure as without an outbox", r.code, r.stdout, r.stderr)
	}
	if out := alice.ok("outbox"); !strings.HasPrefix(out, "alice's outbox: 5 waiting, 0 refused\nserver unreachable since ") {
		t.Errorf("sfx outbox with the server away:\n%s", out)
	}

	w.start()
	r := alice.run("v0.2.0", "ready")
	if r.code != cli.ExitOK || r.stderr != "sfx: outbox: delivered 5\n" {
		t.Fatalf("the first command with the server back = exit %d, stderr %q; want the five writes delivered", r.code, r.stderr)
	}
	check := func(when string) {
		t.Helper()
		ops, _ := alice.history(held)
		// A handoff is recorded as a comment.
		if tally(ops, "comment.add") != 2 || tally(ops, "issue.close") != 1 {
			t.Errorf("%s: the held issue's history is %v; want the handoff, the finish's handoff and one close", when, ops)
		}
		cs := decode[proto.CommentsResult](t, alice.ok("comments", other, "--json"))
		if len(cs.Comments) != 2 || cs.Comments[0].Body != "written while away" || cs.Comments[1].Body != "a second note" {
			t.Errorf("%s: comments = %+v; want the two queued ones, once each and in order", when, cs.Comments)
		}
		if n := alice.titled("Found while away"); n != 1 {
			t.Errorf("%s: %d issues were created from the queued create, want 1", when, n)
		}
	}
	check("after the replay")
	// Nothing is sent twice: not by the next command, and not after the
	// server restarts and every connection drops.
	if r := alice.run("v0.2.0", "ready"); r.stderr != "" {
		t.Errorf("the next command said %q, want nothing left to deliver", r.stderr)
	}
	if err := w.restartDaemon(); err != nil {
		t.Fatal(err)
	}
	w.dropAll()
	check("after a restart")
	var doc struct{ Pending, Refused []any }
	if err := json.Unmarshal([]byte(alice.ok("outbox", "--json")), &doc); err != nil || len(doc.Pending)+len(doc.Refused) != 0 {
		t.Errorf("sfx outbox --json after delivery holds %+v (%v), want nothing", doc, err)
	}
	if out := alice.ok("outbox", "stats"); !strings.Contains(out, "outages: 1 over") || !strings.Contains(out, "none in progress") {
		t.Errorf("sfx outbox stats after delivery:\n%s", out)
	}
}

// TestOutageWritesArriveOnceMCP is the same gate for an agent: each
// queued tool call says it was queued and to carry on, and the session's
// next call, with the server back, sends them first.
func TestOutageWritesArriveOnceMCP(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{})
	alice := w.newUser("alice", "")
	alice.queues = true
	alice.env["CLAUDE_CODE_SESSION_ID"] = "cc-e2e"
	other := strings.TrimSpace(alice.ok("create", "Another issue"))
	ag := alice.mcp("v0.2.0")
	held := decode[mcpserver.Started](t, ag.ok("start", nil)).ID
	if held == other {
		other = strings.TrimSpace(alice.ok("create", "Yet another issue"))
	}

	w.stop()
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"handoff", map[string]any{"id": held, "note": "half done", "next": "the tests"}},
		{"comment", map[string]any{"id": other, "body": "written while away"}},
		{"create", map[string]any{"title": "Found while away"}},
		{"finish", map[string]any{"id": held, "reason": "done", "handoff": "all of it"}},
	} {
		out, isErr := ag.call(c.tool, c.args)
		if isErr || !strings.HasPrefix(out, "queued: starfix cannot reach its server, so this "+c.tool+" waits on this machine") ||
			!strings.Contains(out, "Carry on, and do not repeat it.") {
			t.Fatalf("%s with the server away = %q (error %v); want the one line that says it was queued", c.tool, out, isErr)
		}
	}
	if out, isErr := ag.call("start", map[string]any{"id": other}); !isErr || !strings.Contains(out, "this was not queued (it takes a claim, and no claim is made without the server)") {
		t.Errorf("start with the server away = %q (error %v); want it to fail, saying it was not queued", out, isErr)
	}

	w.start()
	p := decode[mcpserver.Prime](t, ag.ok("prime", nil))
	for _, n := range p.Notices {
		if strings.Contains(n, "outbox") {
			t.Errorf("prime with everything delivered has the notice %q", n)
		}
	}
	ops, sessions := alice.history(held)
	if tally(ops, "comment.add") != 2 || tally(ops, "issue.close") != 1 {
		t.Errorf("the held issue's history is %v; want the handoff, the finish's handoff and one close", ops)
	}
	// The session of a replayed write is that of the connection that
	// delivered it, which here is the session that made it.
	if n := tally(sessions, "cc-e2e"); n != len(sessions) {
		t.Errorf("the held issue's events are by sessions %v, want all by cc-e2e", sessions)
	}
	cs := decode[proto.CommentsResult](t, alice.ok("comments", other, "--json"))
	if len(cs.Comments) != 1 || cs.Comments[0].Body != "written while away" {
		t.Errorf("comments = %+v; want the queued one, once", cs.Comments)
	}
	if n := alice.titled("Found while away"); n != 1 {
		t.Errorf("%d issues were created from the queued create, want 1", n)
	}
}

// A write the server applied, whose answer was lost with the connection,
// is queued with the key it was sent with; replayed, it applies once.
func TestLostAnswerThenReplayAppliesOnce(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{})
	alice := w.newUser("alice", "")
	id := strings.TrimSpace(alice.ok("create", "Answered twice"))
	alice.ok("start", id)

	w.dropReplyTo(proto.OpComment)
	if out := alice.ok("comment", id, "noted once"); !strings.HasPrefix(out, "queued (") {
		t.Fatalf("a comment whose answer was lost printed %q, want it queued", out)
	}
	w.dropReplyTo(proto.OpFinish)
	if out := alice.ok("finish", id, "--reason", "done", "--discovered", "Follow-up"); !strings.HasPrefix(out, "queued (") {
		t.Fatalf("a finish whose answer was lost printed %q, want it queued", out)
	}
	// The server has both already. The finish's connection sent the
	// comment again before the finish, and the next command sends the
	// finish again: the server answers each with what it did the first
	// time.
	if r := alice.run("v0.2.0", "ready"); r.code != cli.ExitOK || r.stderr != "sfx: outbox: delivered 1\n" {
		t.Fatalf("the next command = exit %d, stderr %q; want the finish delivered", r.code, r.stderr)
	}
	if out := alice.ok("outbox"); !strings.HasPrefix(out, "alice's outbox is empty\n") {
		t.Errorf("sfx outbox after both were sent again:\n%s", out)
	}
	cs := decode[proto.CommentsResult](t, alice.ok("comments", id, "--json"))
	if len(cs.Comments) != 1 {
		t.Errorf("comments = %+v, want the one", cs.Comments)
	}
	if ops, _ := alice.history(id); tally(ops, "issue.close") != 1 {
		t.Errorf("history = %v, want one close", ops)
	}
	if n := alice.titled("Follow-up"); n != 1 {
		t.Errorf("%d issues were discovered by the finish, want 1", n)
	}
}

// A queued write the server refuses when it arrives goes to the refused
// list, with the server's code, message and fix; the person hears of it
// from the command that delivered it and from prime, and retries or
// drops it with `sfx outbox`.
func TestRefusedEntries(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{admins: []string{"dana"}})
	alice, dana := w.newUser("alice", ""), w.newUser("dana", "")
	id := strings.TrimSpace(alice.ok("create", "Closed under her"))
	alice.ok("start", id)
	dana.ok("ready") // dana's key is known to her machine before the outage

	w.stop()
	alice.ok("finish", id, "--reason", "done")
	alice.ok("comment", "sf-zzzzzzzz", "on an issue that is not there")
	w.start()
	dana.ok("close", id, "--force", "--reason", "an admin closed it meanwhile")

	r := alice.run("v0.2.0", "ready")
	if r.code != cli.ExitOK || r.stderr != "sfx: outbox: refused 2 (see `sfx outbox`)\n" {
		t.Fatalf("the first command with the server back = exit %d, stderr %q; want both writes refused", r.code, r.stderr)
	}
	out := alice.ok("outbox")
	for _, want := range []string{
		"alice's outbox: 0 waiting, 2 refused\n",
		"  1  finish   " + id,
		"     invalid: ",
		"  2  comment  sf-zzzzzzzz",
		"     not_found: issue sf-zzzzzzzz not found\n     fix: find the id with `sfx list`\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("sfx outbox lacks %q:\n%s", want, out)
		}
	}
	if p := alice.ok("prime"); !strings.Contains(p, "notice: outbox: 2 writes made while the server was away were refused: tell the user to run `sfx outbox`\n") {
		t.Errorf("prime does not say what was refused:\n%s", p)
	}
	// A refused entry is not sent again by itself.
	if r := alice.run("v0.2.0", "ready"); r.stderr != "" {
		t.Errorf("the next command said %q, want nothing sent", r.stderr)
	}
	// Retried, it is sent once more, and refused again; dropped, it is gone.
	alice.ok("outbox", "retry", "2")
	if r := alice.run("v0.2.0", "ready"); r.stderr != "sfx: outbox: refused 1 (see `sfx outbox`)\n" {
		t.Errorf("the command after a retry said %q, want the comment refused again", r.stderr)
	}
	alice.ok("outbox", "drop", "2")
	alice.ok("outbox", "drop", "1")
	if out := alice.ok("outbox"); !strings.HasPrefix(out, "alice's outbox is empty\n") {
		t.Errorf("sfx outbox after the drops:\n%s", out)
	}
	if ops, _ := alice.history(id); tally(ops, "issue.close") != 1 {
		t.Errorf("history = %v, want the admin's one close", ops)
	}
}

// What one session queued is sent by whichever connection of the same
// principal comes next, and the server records that connection's session
// on it: protocol 5 carries no other. Another principal's connection
// sends none of it.
func TestAnotherSessionDeliversTheOutbox(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	alice.queues = true
	alice.env["CLAUDE_CODE_SESSION_ID"] = "cc-e2e"
	ag := alice.mcp("v0.2.0")
	alice.ok("create", "Held by the agent")
	id := decode[mcpserver.Started](t, ag.ok("start", nil)).ID
	bob.ok("ready")

	w.stop()
	if out, isErr := ag.call("finish", map[string]any{"id": id, "reason": "done", "handoff": "all of it"}); isErr {
		t.Fatalf("finish with the server away: %s", out)
	}
	w.start()
	if r := bob.run("v0.2.0", "ready"); r.stderr != "" {
		t.Errorf("bob's command said %q; alice's outbox is not his to send", r.stderr)
	}
	if s := decode[proto.ShowResult](t, bob.ok("show", id, "--json")); s.Issue.Status != "in_progress" {
		t.Errorf("after bob's command the issue is %s, want it still in progress", s.Issue.Status)
	}
	// alice's own CLI, another session of hers, delivers it.
	delete(alice.env, "CLAUDE_CODE_SESSION_ID")
	if r := alice.run("v0.2.0", "ready"); r.stderr != "sfx: outbox: delivered 1\n" {
		t.Fatalf("alice's command said %q, want the finish delivered", r.stderr)
	}
	ops, sessions := alice.history(id)
	if tally(ops, "issue.close") != 1 || sessions[len(sessions)-1] != "cli" {
		t.Errorf("history = %v by sessions %v; want one close, recorded under the session that delivered it", ops, sessions)
	}
	// To the server, then, another session of alice's closed the issue
	// the agent's session held: that session is told it lost the claim.
	alice.env["CLAUDE_CODE_SESSION_ID"] = "cc-e2e"
	lost := false
	for _, it := range decode[proto.InboxResult](t, alice.ok("inbox", "--all", "--json")).Items {
		if it.Kind == "claim.lost" && it.Issue == id && strings.Contains(it.Body, "closed by alice/cli") {
			lost = true
		}
	}
	if !lost {
		t.Errorf("the session that made the finish has no claim.lost item for %s saying alice/cli closed it", id)
	}
}

// comments counts an issue's comments, read as another user: a command
// of the author's own would send what waits in the author's outbox.
func (u *user) comments(id string) int {
	u.w.t.Helper()
	return len(decode[proto.CommentsResult](u.w.t, u.ok("comments", id, "--json")).Comments)
}

// When the server gives a key to another principal, what the first one
// queued with it is not sent on the key's connections: the entries stay,
// the person is told once, in one line with its fix, and `sfx outbox`
// goes on listing them, where --principal reaches them.
func TestKeyMovedLeavesTheOutbox(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	id := strings.TrimSpace(alice.ok("create", "Commented on while away"))
	w.stop()
	alice.ok("comment", id, "queued as alice")
	w.start()
	w.byHand(alice.key, "alicia") // the server now takes alice's key for alicia

	r := alice.run("v0.2.0", "ready")
	want := "sfx: outbox: the server now takes this key for alicia, not alice, so 1 write queued as alice is not sent with it; " +
		"fix: sign in with a key of alice's to send them, or run `sfx outbox --principal alice` to see or drop them\n"
	if r.code != cli.ExitOK || r.stderr != want {
		t.Errorf("the first command as alicia = exit %d, stderr %q\nwant %q", r.code, r.stderr, want)
	}
	for range 2 {
		if r := alice.run("v0.2.0", "ready"); r.code != cli.ExitOK || r.stderr != "" {
			t.Errorf("a later command as alicia = exit %d, stderr %q; want it said once, and nothing sent", r.code, r.stderr)
		}
	}
	if n := bob.comments(id); n != 0 {
		t.Errorf("the issue has %d comments, want none: alice's write is not alicia's to send", n)
	}
	out := alice.ok("outbox")
	if !strings.HasPrefix(out, "alicia's outbox is empty\n") || !strings.Contains(out,
		"alice's outbox on this machine: 1 waiting, 0 refused; sfx sends it on a connection as alice (`sfx outbox --principal alice` lists it)\n") {
		t.Errorf("sfx outbox as alicia:\n%s", out)
	}
	if out := alice.ok("outbox", "--principal", "alice"); !strings.HasPrefix(out, "alice's outbox: 1 waiting, 0 refused\n") ||
		!strings.Contains(out, "  1  comment  "+id) {
		t.Errorf("sfx outbox --principal alice:\n%s", out)
	}
	// The key is alice's again: her connection sends what she queued.
	w.byHand(alice.key, "alice")
	if r := alice.run("v0.2.0", "ready"); r.stderr != "sfx: outbox: delivered 1\n" {
		t.Errorf("the first command as alice again said %q, want her write delivered", r.stderr)
	}
	if n := bob.comments(id); n != 1 {
		t.Errorf("the issue has %d comments, want alice's one", n)
	}
}

// When the server refuses a principal's access to the project at the
// handshake, what that principal queued for it is refused as forbidden,
// with the server's message and fix, and kept for a person (design
// §18.3, "Who it runs as"): it is not left to go stale. A membership
// ended and a principal disabled each prove it. A project archived
// does not: it is refused as not served, which a project made active
// again mends, so what waits for it goes on waiting. Access given back,
// a person's retry sends the entry.
func TestAccessEndedRefusesTheOutbox(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{many: true, admins: []string{"dana"}})
	w.member("alice", project, store.RoleMember)
	w.member("bob", project, store.RoleMember)
	w.member("carol", project, store.RoleMember)
	alice, bob, carol := w.newUser("alice", ""), w.newUser("bob", ""), w.newUser("carol", "")
	id := strings.TrimSpace(alice.ok("create", "Commented on while away"))
	bob.ok("ready")
	carol.ok("ready")

	w.stop()
	alice.ok("comment", id, "queued by alice")
	alice.ok("comment", id, "queued by alice again")
	bob.ok("comment", id, "queued by bob")
	carol.ok("comment", id, "queued by carol")
	w.start()
	ctx := t.Context()
	if _, err := w.ctl.RemoveMember(ctx, dana, "alice", project); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ctl.DisablePrincipal(ctx, dana, "bob"); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		u        *user
		name     string
		n        string
		msg, fix string
	}{
		{alice, "alice", "2 writes", `alice is not a member of project "` + project + `"`,
			"ask a server admin, or one of the project's admins, to add you: `sfx admin member add alice`, run in this repository"},
		{bob, "bob", "1 write", "bob is disabled on this server",
			"ask a server admin, or a project admin who administers every project you belong to, to run `sfx admin principal enable bob`"},
	} {
		r := tc.u.run("v0.2.0", "ready")
		if r.code != cli.ExitFailure || !strings.Contains(r.stderr, "sfx: "+tc.msg+"\n") ||
			!strings.Contains(r.stderr, "sfx: outbox: "+tc.n+" queued as "+tc.name) {
			t.Errorf("%s's first command after access ended = exit %d, stderr:\n%s\nwant the refusal, and the outbox's line", tc.name, r.code, r.stderr)
		}
		out := tc.u.ok("outbox")
		for _, want := range []string{
			tc.name + "'s outbox: 0 waiting, ",
			"     forbidden: " + tc.msg + "\n     fix: " + tc.fix + "\n",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("sfx outbox as %s lacks %q:\n%s", tc.name, want, out)
			}
		}
	}

	// A project archived is not the end of anyone's access.
	if _, err := w.ctl.ArchiveProject(ctx, dana, project); err != nil {
		t.Fatal(err)
	}
	if r := carol.run("v0.2.0", "ready"); r.code != cli.ExitFailure || !strings.Contains(r.stderr, "is not served by this server") ||
		strings.Contains(r.stderr, "outbox") {
		t.Errorf("carol's command in an archived project = exit %d, stderr:\n%s\nwant it refused, and her outbox left alone", r.code, r.stderr)
	}
	if out := carol.ok("outbox"); !strings.HasPrefix(out, "carol's outbox: 1 waiting, 0 refused\n") {
		t.Errorf("sfx outbox as carol, the project archived:\n%s", out)
	}
	if _, err := w.ctl.ActivateProject(ctx, dana, project); err != nil {
		t.Fatal(err)
	}
	if r := carol.run("v0.2.0", "ready"); r.code != cli.ExitOK || r.stderr != "sfx: outbox: delivered 1\n" {
		t.Errorf("carol's command, the project active again = exit %d, stderr %q; want her write delivered", r.code, r.stderr)
	}

	// alice is a member again: nothing goes by itself, and a retry sends.
	w.member("alice", project, store.RoleMember)
	if r := alice.run("v0.2.0", "ready"); r.code != cli.ExitOK || r.stderr != "" {
		t.Errorf("alice's command as a member again = exit %d, stderr %q; want nothing sent by itself", r.code, r.stderr)
	}
	alice.ok("outbox", "retry", "1")
	if r := alice.run("v0.2.0", "ready"); r.stderr != "sfx: outbox: delivered 1\n" {
		t.Errorf("alice's command after a retry said %q, want the entry delivered", r.stderr)
	}
	if n := carol.comments(id); n != 2 {
		t.Errorf("the issue has %d comments, want carol's and alice's retried one", n)
	}

	// An agent's session: its tool call is refused, and tells it to ask
	// a person; what it queued waits for that person, refused.
	w.member("erin", project, store.RoleMember)
	erin := w.newUser("erin", "")
	erin.queues = true
	ag := erin.mcp("v0.2.0")
	ag.ok("ready", nil)
	w.stop()
	if out, isErr := ag.call("comment", map[string]any{"id": id, "body": "queued by erin's agent"}); isErr {
		t.Fatalf("comment with the server away = %q, want it queued", out)
	}
	w.start()
	if _, err := w.ctl.RemoveMember(ctx, dana, "erin", project); err != nil {
		t.Fatal(err)
	}
	if out, isErr := ag.call("ready", nil); !isErr || !strings.Contains(out, "erin is not a member of project") {
		t.Errorf("the agent's call after its access ended = %q (error %v); want the refusal", out, isErr)
	}
	if out := erin.ok("outbox"); !strings.HasPrefix(out, "erin's outbox: 0 waiting, 1 refused\n") ||
		!strings.Contains(out, "     forbidden: erin is not a member of project") {
		t.Errorf("sfx outbox as erin, after her agent's call was refused:\n%s", out)
	}
}

// configure rewrites the user's .starfix.yaml, as a person who edits it
// does: from becomes to.
func (u *user) configure(from, to string) {
	u.w.t.Helper()
	p := filepath.Join(u.repo, ".starfix.yaml")
	b, err := os.ReadFile(p) //nolint:gosec // the test's own file
	if err != nil {
		u.w.t.Fatal(err)
	}
	if !strings.Contains(string(b), from) {
		u.w.t.Fatalf("%s has no %q:\n%s", p, from, b)
	}
	if err := os.WriteFile(p, []byte(strings.ReplaceAll(string(b), from, to)), 0o600); err != nil { //nolint:gosec // the test's own file
		u.w.t.Fatal(err)
	}
}

// A server rebuilt with a new host key leaves what was queued for it
// under the old key. Nothing of it is sent by itself, even once the
// person has pinned the new key: a changed host key is for a person to
// look at. `sfx outbox` and prime say it is there; a person moves an
// entry to the current server's outbox, which then sends it, but only
// when the checkout names the server at the address it was queued for;
// or drops it.
func TestRekeyedServerKeepsTheOutbox(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	id := strings.TrimSpace(alice.ok("create", "Commented on before the rebuild"))
	w.stop()
	alice.ok("comment", id, "first, before the rebuild")
	alice.ok("comment", id, "second, before the rebuild")
	w.start()
	old := w.hostFpr
	w.rekey()

	// The pin no longer matches: refused, and nothing else happens.
	if r := alice.run("v0.2.0", "ready"); r.code != cli.ExitFailure || !strings.Contains(r.stderr, "host key mismatch") || strings.Contains(r.stderr, "outbox") {
		t.Fatalf("a command against the rebuilt server = exit %d, stderr:\n%s\nwant the host key refused", r.code, r.stderr)
	}
	// The people confirm the new key and pin it.
	alice.configure(old, w.hostFpr)
	bob.configure(old, w.hostFpr)
	if r := alice.run("v0.2.0", "ready"); r.code != cli.ExitOK || r.stderr != "" {
		t.Errorf("the first command with the new key pinned = exit %d, stderr %q; want nothing sent by itself", r.code, r.stderr)
	}
	if n := bob.comments(id); n != 0 {
		t.Errorf("the issue has %d comments, want none sent by themselves", n)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", w.addr.Port)
	out := alice.ok("outbox")
	for _, want := range []string{
		"alice's outbox is empty\n",
		"under the server's old host key " + old + ", at " + addr + ": alice's outbox, 2 waiting, 0 refused\n",
		"not sent by themselves, since the server's host key changed: `sfx outbox --host-key " + old + "` lists them\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("sfx outbox lacks %q:\n%s", want, out)
		}
	}
	if p := alice.ok("prime"); !strings.Contains(p,
		"notice: outbox: 2 writes queued under the server's old host key are not sent by themselves: tell the user to run `sfx outbox`\n") {
		t.Errorf("prime does not say what waits under the old key:\n%s", p)
	}
	out = alice.ok("outbox", "--host-key", old)
	for _, want := range []string{
		"alice's outbox under the server's old host key " + old + ", at " + addr + ": 2 waiting, 0 refused\n",
		"  1  comment  " + id,
		"  2  comment  " + id,
		"`sfx outbox retry N --host-key " + old + "` moves entry N to this server's outbox, which sends it; `sfx outbox drop N --host-key " + old + "` removes it for good\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("sfx outbox --host-key lacks %q:\n%s", want, out)
		}
	}

	// Named at another address, the server is not known to be the same.
	alice.configure("host: 127.0.0.1", "host: localhost")
	r := alice.run("v0.2.0", "outbox", "retry", "1", "--host-key", old)
	if r.code != cli.ExitFailure || !strings.Contains(r.stderr, "entry 1 was queued for the server at "+addr+", and this checkout names localhost:") {
		t.Errorf("a retry with the server named at another address = exit %d, stderr:\n%s\nwant it refused", r.code, r.stderr)
	}
	alice.configure("host: localhost", "host: 127.0.0.1")
	if r := alice.run("v0.2.0", "outbox", "retry", "9", "--host-key", old); r.code != cli.ExitFailure || !strings.Contains(r.stderr, "no entry 9") {
		t.Errorf("a retry of an entry that is not there = exit %d, stderr:\n%s", r.code, r.stderr)
	}
	if r := alice.run("v0.2.0", "outbox", "--host-key", w.hostFpr); r.code != cli.ExitUsage {
		t.Errorf("sfx outbox --host-key with the current key = exit %d, stderr:\n%s\nwant a usage error", r.code, r.stderr)
	}

	if out := alice.ok("outbox", "retry", "1", "--host-key", old); !strings.HasPrefix(out,
		"entry 1 (comment "+id+") of the outbox under the old host key is entry 1 of this server's now: the next sfx command that reaches the server sends it\n") {
		t.Errorf("sfx outbox retry --host-key said:\n%s", out)
	}
	alice.ok("outbox", "drop", "2", "--host-key", old)
	if r := alice.run("v0.2.0", "ready"); r.stderr != "sfx: outbox: delivered 1\n" {
		t.Errorf("the command after the retry said %q, want the entry delivered", r.stderr)
	}
	cs := decode[proto.CommentsResult](t, bob.ok("comments", id, "--json"))
	if len(cs.Comments) != 1 || cs.Comments[0].Body != "first, before the rebuild" {
		t.Errorf("comments = %+v, want the one retried", cs.Comments)
	}
	if out := alice.ok("outbox"); strings.Contains(out, "old host key") {
		t.Errorf("sfx outbox with the old outbox emptied still names it:\n%s", out)
	}
}

// An entry that waited longer than the age cap is not sent: it moves to
// the refused list as stale, with a fix, and a person's retry sends it.
func TestStaleEntryWaitsForAPerson(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	clock := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	alice.now = func() time.Time { return clock }
	id := strings.TrimSpace(alice.ok("create", "Commented on long ago"))
	w.stop()
	alice.ok("comment", id, "written eight days ago")
	w.start()
	clock = clock.Add(8 * 24 * time.Hour) // alice's machine was away for more than the cap

	if r := alice.run("v0.2.0", "ready"); r.code != cli.ExitOK || r.stderr != "sfx: outbox: stale 1 (see `sfx outbox`)\n" {
		t.Fatalf("the first command eight days later = exit %d, stderr %q; want the entry found stale", r.code, r.stderr)
	}
	if n := bob.comments(id); n != 0 {
		t.Errorf("the issue has %d comments, want none: a stale entry is not sent", n)
	}
	out := alice.ok("outbox")
	for _, want := range []string{
		"alice's outbox: 0 waiting, 1 refused\n",
		"  1  comment  " + id,
		"     stale: it waited 192h0m0s, and an entry is sent for 168h0m0s\n     fix: retry it if it is still wanted, or drop it\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("sfx outbox lacks %q:\n%s", want, out)
		}
	}
	if r := alice.run("v0.2.0", "ready"); r.stderr != "" {
		t.Errorf("the next command said %q, want the stale entry left alone", r.stderr)
	}
	alice.ok("outbox", "retry", "1")
	if r := alice.run("v0.2.0", "ready"); r.stderr != "sfx: outbox: delivered 1\n" {
		t.Errorf("the command after the retry said %q, want the entry delivered", r.stderr)
	}
	if n := bob.comments(id); n != 1 {
		t.Errorf("the issue has %d comments, want the one", n)
	}
}

// The replay budget, end to end for the CLI: the server takes the first
// queued write and never answers it. The command that found the server
// back is not held by that: when the budget runs out it closes the
// connection, as a canceled call does, connects again and runs. Both
// writes stay pending, and the next command sends them: the first again,
// with its key, which the server applies once.
//
// This waits out the outbox's real budget, five seconds: a connection to
// a real server is not in a synctest bubble. It synchronizes on the
// client giving up, never on a sleep, so a slow machine only makes it
// slower.
func TestReplayBudgetCLI(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	first := strings.TrimSpace(alice.ok("create", "Never answered"))
	second := strings.TrimSpace(alice.ok("create", "Behind it"))
	w.stop()
	alice.ok("comment", first, "applied, and not answered")
	alice.ok("comment", second, "waits behind it")
	w.start()

	w.silentAfter(proto.OpComment)
	r := alice.run("v0.2.0", "ready")
	if r.code != cli.ExitOK || !strings.Contains(r.stdout, first) || r.stderr != "" {
		t.Fatalf("the command that found a server that does not answer = exit %d\nstdout %q\nstderr %q\nwant its own request answered", r.code, r.stdout, r.stderr)
	}
	if got, want := [2]int{bob.comments(first), bob.comments(second)}, [2]int{1, 0}; got != want {
		t.Errorf("the issues have %v comments, want %v: the server applied the first write and saw nothing of the second", got, want)
	}
	if out := alice.ok("outbox"); !strings.HasPrefix(out, "alice's outbox: 2 waiting, 0 refused\n") {
		t.Errorf("sfx outbox after the budget ran out:\n%s", out)
	}
	if r := alice.run("v0.2.0", "ready"); r.code != cli.ExitOK || r.stderr != "sfx: outbox: delivered 2\n" {
		t.Errorf("the next command = exit %d, stderr %q; want both writes delivered", r.code, r.stderr)
	}
	if got, want := [2]int{bob.comments(first), bob.comments(second)}, [2]int{1, 1}; got != want {
		t.Errorf("the issues have %v comments, want %v: each write once", got, want)
	}
}
