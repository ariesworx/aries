package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/mcpserver"
	"github.com/ariesworx/aries/internal/proto"
)

// keptAs seeds principal's cache as connected reads would have, at the
// checkout's clock: a show of ar-a1b2, the inbox, a digest and prime.
func (a *away) keptAs(t *testing.T, principal string) {
	t.Helper()
	c := a.local().Cache(principal)
	for _, k := range []struct {
		op           string
		args, result any
	}{
		{proto.OpShow, proto.ShowArgs{ID: "ar-a1b2", Full: true}, proto.ShowResult{Issue: proto.Issue{ID: "ar-a1b2", Title: "Held work",
			Status: "in_progress", Type: "task", Rev: 4, Body: "the plan"}}},
		{proto.OpInbox, proto.InboxArgs{}, proto.InboxResult{Unread: 1, Items: []proto.InboxItem{{ID: 7, Kind: "handoff", Issue: "ar-a1b2", From: "bob", Body: "over to you"}}}},
		{proto.OpDigest, proto.DigestArgs{}, proto.DigestResult{Totals: proto.DigestTotals{Closed: 3}}},
	} {
		if err := c.Keep(k.op, k.args, k.result); err != nil {
			t.Fatal(err)
		}
	}
	p := &mcpserver.Prime{Project: "demo", You: principal, Session: "old", Working: []proto.Summary{{ID: "ar-a1b2", Title: "Held work", Status: "in_progress"}},
		Ready: []proto.Summary{{ID: "ar-c3d4", Title: "Up next"}}}
	if err := mcpserver.KeepPrime(c, p); err != nil {
		t.Fatal(err)
	}
}

const cachedLine = "aries: cached: aries cannot reach its server, so this is this machine's copy from 2026-10-13 14:02 UTC (3h old), and may be out of date\n"

// TestCachedReads is slice F3's gate for a person (design §18.10): with
// the server away, show of held work, prime, inbox and digest print this
// machine's copy, and say on stderr that it is one and how old; --json
// says "cached": true and when; what was never read fails as before.
func TestCachedReads(t *testing.T) {
	t.Parallel()
	a := newAway(t)
	a.reachedAs(t, "alice")
	a.keptAs(t, "alice")
	a.now = a.now.Add(3 * time.Hour)

	for _, tc := range []struct {
		args []string
		want string // in stdout
	}{
		{[]string{"show", "ar-a1b2"}, "ar-a1b2  P0  in_progress  task  rev 4\nHeld work\n"},
		{[]string{"inbox"}, "over to you"},
		{[]string{"digest"}, "closed"},
	} {
		code, out, errb := a.run(t, tc.args...)
		if code != ExitOK || !strings.Contains(out, tc.want) || errb != cachedLine {
			t.Errorf("aries %s with the server away = exit %d\nstdout %q, want it to hold %q\nstderr %q\nwant   %q",
				strings.Join(tc.args, " "), code, out, tc.want, errb, cachedLine)
		}
	}

	code, out, errb := a.run(t, "show", "ar-a1b2", "--json")
	var doc struct {
		Cached   bool      `json:"cached"`
		CachedAt time.Time `json:"cached_at"`
		Issue    proto.Issue
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || code != ExitOK || errb != "" || !doc.Cached ||
		!doc.CachedAt.Equal(time.Date(2026, 10, 13, 14, 2, 0, 0, time.UTC)) || doc.Issue.Title != "Held work" {
		t.Errorf("aries show --json with the server away = exit %d, %q (%v), stderr %q; want the issue with cached: true and when", code, out, err, errb)
	}

	code, out, errb = a.run(t, "prime")
	for _, want := range []string{
		"aries: project demo, you are alice, session cli\n",
		"notice: " + strings.TrimPrefix(strings.TrimSuffix(cachedLine, "\n"), "aries: ") + "\n",
		"next: carry on with the work in progress; nothing can be started until the server answers",
		"in progress:\n  ar-a1b2 P0 \"Held work\"\n",
		"ready:\n  (not shown while the server cannot be reached)\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("aries prime with the server away lacks %q:\n%s", want, out)
		}
	}
	if code != ExitOK || errb != cachedLine || strings.Contains(out, "ar-c3d4") {
		t.Errorf("aries prime with the server away = exit %d, stderr %q; want the cached prime without its ready issues, and the line on stderr", code, errb)
	}

	// What was never read fails as it did.
	if code, _, errb := a.run(t, "show", "ar-zzzz"); code != ExitFailure || !strings.HasPrefix(errb, "aries: ") || strings.Contains(errb, "cached") {
		t.Errorf("aries show of an issue never read = exit %d, %q; want the failure as before", code, errb)
	}
	if code, _, errb := a.run(t, "inbox", "--all"); code != ExitFailure || strings.Contains(errb, "cached") {
		t.Errorf("aries inbox --all, never read = exit %d, %q; want the failure as before", code, errb)
	}
}

// prime --hook with the server away exits 0 and prints one JSON document,
// as the hook contract says, now with the cached context in it.
func TestPrimeHookFromCache(t *testing.T) {
	t.Parallel()
	a := newAway(t)
	a.reachedAs(t, "alice")
	a.keptAs(t, "alice")
	a.now = a.now.Add(3 * time.Hour)
	a.run(t, "comment", "ar-a1b2", "queued meanwhile")
	code, out, errb := a.run(t, "prime", "--hook")
	var doc struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || code != ExitOK || errb != "" || strings.Count(out, "\n") != 1 {
		t.Fatalf("aries prime --hook with the server away = exit %d, %q (%v), stderr %q; want one JSON document and nothing else", code, out, err, errb)
	}
	ctx := doc.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ctx, "notice: cached: aries cannot reach its server") || !strings.Contains(ctx, `ar-a1b2 P0 "Held work"`) ||
		!strings.Contains(ctx, "notice: outbox: 1 write made while the server was away waits to be sent") {
		t.Errorf("the hook's context with the server away:\n%s\nwant the cached prime, saying it is cached, with the outbox's notice", ctx)
	}
	// Nothing cached: the one-line note, as before.
	b := newAway(t)
	b.reachedAs(t, "alice")
	code, out, _ = b.run(t, "prime", "--hook")
	if err := json.Unmarshal([]byte(out), &doc); err != nil || code != ExitOK || !strings.HasPrefix(doc.HookSpecificOutput.AdditionalContext, "aries: prime failed: ") {
		t.Errorf("aries prime --hook with nothing cached = exit %d, %q (%v); want the failure's note", code, out, err)
	}
}

// ready, blocked, list and who are refused offline, saying when the
// server was last reached, with a fix; and one principal's copy is never
// shown to another.
func TestCacheRefusals(t *testing.T) {
	t.Parallel()
	a := newAway(t)
	a.reachedAs(t, "alice")
	a.local().Touch()
	a.keptAs(t, "alice")
	for _, cmd := range []string{"ready", "blocked", "list", "who"} {
		code, _, errb := a.run(t, cmd)
		if code != ExitFailure || !strings.Contains(errb, "; "+cmd+" is not answered from this machine's copy, since it says what others are doing now (server last reached 2026-10-13 14:02 UTC)\n") ||
			!strings.Contains(errb, "; meanwhile `aries prime` and `aries show ID` answer from this machine's copy of what you read\n") {
			t.Errorf("aries %s with the server away = exit %d, stderr:\n%s\nwant it refused, saying when the server was last reached, with a fix", cmd, code, errb)
		}
	}
	// The server took this machine's key for bob since: alice's copy is
	// not his.
	a.reachedAs(t, "bob")
	for _, args := range [][]string{{"show", "ar-a1b2"}, {"prime"}, {"inbox"}} {
		if code, out, errb := a.run(t, args...); code != ExitFailure || strings.Contains(out, "Held work") || strings.Contains(errb, "cached") {
			t.Errorf("aries %s as bob = exit %d, %q, %q; want alice's copy kept from him", strings.Join(args, " "), code, out, errb)
		}
	}
}

// aries cache says what this machine keeps, and aries cache clear deletes
// it, every principal's, and leaves the outbox.
func TestCacheCommand(t *testing.T) {
	t.Parallel()
	a := newAway(t)
	a.reachedAs(t, "alice")
	a.keptAs(t, "alice")
	a.keptAs(t, "bob")
	a.run(t, "comment", "ar-a1b2", "kept for later")
	code, out, _ := a.run(t, "cache")
	for _, want := range []string{
		"alice's cache: 4 answers, ",
		"bob's cache: 4 answers, ",
		", kept from 2026-10-13 14:02 UTC to 2026-10-13 14:02 UTC\n",
		"`aries cache clear` deletes them from this machine\n",
	} {
		if code != ExitOK || !strings.Contains(out, want) {
			t.Errorf("aries cache = exit %d, lacks %q:\n%s", code, want, out)
		}
	}
	if code, out, _ := a.run(t, "cache", "clear"); code != ExitOK || out != "deleted 8 cached answers from this machine\n" {
		t.Errorf("aries cache clear = exit %d, %q", code, out)
	}
	if code, out, _ := a.run(t, "cache", "--json"); code != ExitOK || !strings.HasPrefix(out, `{"caches":[]`) {
		t.Errorf("aries cache --json after clear = exit %d, %q; want no caches", code, out)
	}
	if got := a.pending(t, "alice"); len(got) != 1 {
		t.Errorf("alice's outbox after aries cache clear holds %v, want her comment still there", got)
	}
	if code, _, _ := a.run(t, "cache", "drop"); code != ExitUsage {
		t.Errorf("aries cache drop = exit %d, want a usage error", code)
	}
}
