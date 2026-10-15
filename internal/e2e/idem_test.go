package e2e

// Slice F2 (design §18.5, §18.10): protocol 6's keys on the ops an
// outbox may queue, the idempotency window, and stale; through the CLI,
// SSH and a real daemon. A client of 7 stepping down to a server of 6
// is in conflicts_test.go; it no longer speaks 5.

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/cli"
	"github.com/ariesworx/aries/internal/proto"
)

// TestReplayAfterADayAppliesOnce is F2's gate, its first half: "A replay
// after 24 hours applies once." Writes protocol 6 gives a key, queued
// while the server is away, arrive once a day later, which protocol 5's
// 24 hours would have refused; and one whose answer was lost after the
// server applied it, sent again a day later with its key, is not applied
// again.
func TestReplayAfterADayAppliesOnce(t *testing.T) {
	t.Parallel()
	clk := &clock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	w := newWorld(t, daemonOpts{now: clk.now})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	alice.now, bob.now = clk.now, clk.now
	id := strings.TrimSpace(alice.ok("create", "Changed while away", "--label", "old", "--acceptance", "- one\n- two"))
	blocker := strings.TrimSpace(alice.ok("create", "Blocker"))
	rev := decode[proto.ShowResult](t, alice.ok("show", id, "--json")).Issue.Rev

	// Applied, and its answer lost: queued with the key it was sent with.
	w.dropReplyTo(proto.OpLabelAdd)
	if out := alice.ok("label", "add", id, "lost"); !strings.HasPrefix(out, "queued (") {
		t.Fatalf("a label whose answer was lost printed %q, want it queued", out)
	}
	w.stop()
	for _, args := range [][]string{
		{"update", id, "--rev", strconv.FormatInt(rev, 10), "--title", "Renamed while away"},
		{"label", "rm", id, "old"},
		{"dep", "add", id, blocker},
		{"accept", id, "1"},
	} {
		if out := alice.ok(args...); !strings.HasPrefix(out, "queued (") {
			t.Fatalf("sfx %s with the server away printed %q, want it queued", strings.Join(args, " "), out)
		}
	}
	clk.add(25 * time.Hour)
	w.start()
	if r := alice.run("v0.2.0", "ready"); r.code != cli.ExitOK || r.stderr != "sfx: outbox: delivered 5\n" {
		t.Fatalf("the first command a day later = exit %d, stderr %q; want the five writes delivered", r.code, r.stderr)
	}
	check := func(when string) {
		t.Helper()
		s := decode[proto.ShowResult](t, bob.ok("show", id, "--json"))
		if s.Issue.Title != "Renamed while away" || strings.Join(s.Issue.Labels, ",") != "lost" {
			t.Errorf("%s: the issue is %q with labels %v; want it renamed, old removed and lost added", when, s.Issue.Title, s.Issue.Labels)
		}
		ops, _ := bob.history(id)
		for op, want := range map[string]int{"issue.update": 1, "label.add": 1, "label.remove": 1, "dep.add": 1, "acceptance.tick": 1} {
			if n := tally(ops, op); n != want {
				t.Errorf("%s: history has %d %s, want %d: %v", when, n, op, want, ops)
			}
		}
	}
	check("after the replay")
	// Nothing goes twice: not the next command, nor after a restart.
	if r := alice.run("v0.2.0", "ready"); r.stderr != "" {
		t.Errorf("the next command said %q, want nothing left to send", r.stderr)
	}
	if err := w.restartDaemon(); err != nil {
		t.Fatal(err)
	}
	w.dropAll()
	check("after a restart")
}

// TestReplayPastTheWindowIsKept is F2's gate, its second half: "one past
// the window is refused and kept." An entry that waited longer than the
// window is not sent, and waits for a person as stale; and a write the
// server applied, whose answer was lost, sent again by hand past the
// window, is refused stale by the server, saying it was applied then,
// and kept: never applied twice.
func TestReplayPastTheWindowIsKept(t *testing.T) {
	t.Parallel()
	clk := &clock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	w := newWorld(t, daemonOpts{now: clk.now})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	alice.now, bob.now = clk.now, clk.now
	id := strings.TrimSpace(alice.ok("create", "Commented on long ago"))

	w.dropReplyTo(proto.OpComment)
	alice.ok("comment", id, "applied, its answer lost")
	w.stop()
	alice.ok("label", "add", id, "never-sent")
	clk.add(8 * 24 * time.Hour)
	w.start()

	if r := alice.run("v0.2.0", "ready"); r.code != cli.ExitOK || r.stderr != "sfx: outbox: stale 2 (see `sfx outbox`)\n" {
		t.Fatalf("the first command eight days later = exit %d, stderr %q; want both entries found stale", r.code, r.stderr)
	}
	if n := bob.comments(id); n != 1 {
		t.Errorf("the issue has %d comments, want the one the server applied", n)
	}
	out := alice.ok("outbox")
	for _, want := range []string{
		"alice's outbox: 0 waiting, 2 refused\n",
		"     stale: it waited 192h0m0s, and an entry is sent for 168h0m0s\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("sfx outbox lacks %q:\n%s", want, out)
		}
	}
	// Retried by hand: the label, never sent, applies; the comment's key
	// is the server's from eight days ago, and is refused stale.
	alice.ok("outbox", "retry", "1")
	alice.ok("outbox", "retry", "2")
	if r := alice.run("v0.2.0", "ready"); r.stderr != "sfx: outbox: delivered 1, refused 1 (see `sfx outbox`)\n" {
		t.Errorf("the command after the retries said %q, want the label delivered and the comment refused", r.stderr)
	}
	out = alice.ok("outbox")
	for _, want := range []string{
		"alice's outbox: 0 waiting, 1 refused\n",
		"  1  comment  " + id,
		"longer ago than the 7d a key's result is kept: its request was applied then",
		"     fix: nothing to do if that was this request; if the change is still wanted, make it again by hand",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("sfx outbox after the retries lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "     stale: ") {
		t.Errorf("the comment is not refused as stale:\n%s", out)
	}
	if n := bob.comments(id); n != 1 {
		t.Errorf("the issue has %d comments, want the one: a key is applied once, however late", n)
	}
	if s := decode[proto.ShowResult](t, bob.ok("show", id, "--json")); strings.Join(s.Issue.Labels, ",") != "never-sent" {
		t.Errorf("labels = %v, want the retried one", s.Issue.Labels)
	}
}

// firstRefused is the number of the first refused entry `sfx outbox`
// lists in out.
func firstRefused(t *testing.T, out string) string {
	t.Helper()
	_, after, ok := strings.Cut(out, "refused:\n")
	if !ok {
		t.Fatalf("no refused entries in:\n%s", out)
	}
	for _, line := range strings.Split(after, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] != "N" {
			return f[0]
		}
	}
	t.Fatalf("no refused entry in:\n%s", out)
	return ""
}
