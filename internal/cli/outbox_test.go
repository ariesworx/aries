package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/internal/offline"
	"github.com/ariesworx/aries/internal/outbox"
	"github.com/ariesworx/aries/internal/proto"
)

const (
	awayProject = "6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f"
	awayHostKey = "SHA256:et6CqkKsyU2BxzA7Ws+V18rXrtM9Gj6dk1/m0C5TqvU"
)

// away is a checkout whose server cannot be reached: its address hangs
// up on every caller before SSH begins, as a tunnel does, or an sshd
// that turns the address away. It has a key file, a data directory of
// its own and a clock the test moves.
type away struct {
	repo, data string
	key        string // the key's fingerprint
	env        map[string]string
	now        time.Time
	stdin      string
}

func newAway(t *testing.T) *away {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			nc, err := l.Accept()
			if err != nil {
				return
			}
			_ = nc.Close()
		}
	}()
	t.Cleanup(func() { _ = l.Close(); <-done })

	a := &away{repo: t.TempDir(), data: t.TempDir(), now: time.Date(2026, 10, 13, 14, 2, 0, 0, time.UTC)}
	// The data directory must be the user's alone.
	if err := os.Chmod(a.data, 0o700); err != nil { //nolint:gosec // a directory: 0700 is owner-only
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
	if err := os.WriteFile(filepath.Join(a.repo, "id_ed25519"), pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	a.key = ssh.FingerprintSHA256(sp)
	cfg := fmt.Sprintf("project: %s\nserver:\n  host: 127.0.0.1\n  port: %d\n  host_key: %s\nkey: id_ed25519\n",
		awayProject, l.Addr().(*net.TCPAddr).Port, awayHostKey)
	if err := os.WriteFile(filepath.Join(a.repo, ".aries.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	a.env = map[string]string{offline.EnvDataDir: a.data}
	return a
}

// local is what the checkout's machine keeps for its server and project.
func (a *away) local() *offline.Local {
	return offline.Open(a.data, awayHostKey, awayProject, offline.Options{Now: func() time.Time { return a.now }})
}

// reachedAs has the server take the checkout's key for principal, as a
// handshake before the outage did.
func (a *away) reachedAs(t *testing.T, principal string) {
	t.Helper()
	if _, err := a.local().Reached(principal, a.key, true, proto.Proto); err != nil {
		t.Fatal(err)
	}
}

func (a *away) run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(t.Context(), append([]string{"-C", a.repo}, args...), Env{Stdin: strings.NewReader(a.stdin), Stdout: &out, Stderr: &errb,
		Getenv: func(k string) string { return a.env[k] }, Hostname: func() (string, error) { return "laptop", nil },
		Now: func() time.Time { return a.now }, Version: "v0.3.0", GOOS: "linux"})
	return code, out.String(), errb.String()
}

// pending lists what waits in principal's outbox, as "op target".
func (a *away) pending(t *testing.T, principal string) []string {
	t.Helper()
	ob, ok, err := a.local().Existing(principal)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return nil
	}
	list, err := ob.List()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range list.Pending {
		out = append(out, strings.TrimSpace(e.Op+" "+offline.Target(e)))
	}
	return out
}

// TestQueuedWrite pins what a person and a script see when a write does
// not reach the server and is queued: one line, or with --json one
// document that says "queued": true and never an id and a rev as if it
// had been written (design §18.2, §18.8), and exit code 0.
func TestQueuedWrite(t *testing.T) {
	t.Parallel()
	const since = "queued (server unreachable since 2026-10-13 14:02 UTC): "
	const sent = "; aries sends it when the server answers (`aries outbox` lists what waits)"
	for _, tc := range []struct {
		name string
		args []string
		text string
		doc  map[string]any
	}{
		{"handoff", []string{"handoff", "sf-a1b2", "half", "done", "--next", "the tests"},
			since + "entry 1, handoff sf-a1b2" + sent + "\n",
			map[string]any{"queued": true, "entry": 1.0, "op": "handoff", "issue": "sf-a1b2", "since": "2026-10-13T14:02:00Z"}},
		{"comment", []string{"comment", "sf-a1b2", "a", "note"},
			since + "entry 1, comment sf-a1b2" + sent + "\n",
			map[string]any{"queued": true, "entry": 1.0, "op": "comment", "issue": "sf-a1b2", "since": "2026-10-13T14:02:00Z"}},
		{"finish", []string{"finish", "sf-a1b2", "--reason", "done", "--discovered", "more to do"},
			since + "entry 1, finish sf-a1b2" + sent + "\n",
			map[string]any{"queued": true, "entry": 1.0, "op": "finish", "issue": "sf-a1b2", "since": "2026-10-13T14:02:00Z"}},
		{"hours", []string{"log", "1.5h", "sf-a1b2"},
			since + "entry 1, hours.log sf-a1b2" + sent + "\n",
			map[string]any{"queued": true, "entry": 1.0, "op": "hours.log", "issue": "sf-a1b2", "since": "2026-10-13T14:02:00Z"}},
		{"remember", []string{"remember", "style", "tabs,", "not", "spaces"},
			since + "entry 1, remember style" + sent + "\n",
			map[string]any{"queued": true, "entry": 1.0, "op": "remember", "key": "style", "since": "2026-10-13T14:02:00Z"}},
		{"update, with its rev", []string{"update", "sf-a1b2", "--rev", "3", "--title", "renamed"},
			since + "entry 1, update sf-a1b2" + sent + "\n",
			map[string]any{"queued": true, "entry": 1.0, "op": "update", "issue": "sf-a1b2", "since": "2026-10-13T14:02:00Z"}},
		{"dep add", []string{"dep", "add", "sf-a1b2", "sf-c3d4"},
			since + "entry 1, dep.add sf-a1b2" + sent + "\n",
			map[string]any{"queued": true, "entry": 1.0, "op": "dep.add", "issue": "sf-a1b2", "since": "2026-10-13T14:02:00Z"}},
		{"label rm", []string{"label", "rm", "sf-a1b2", "old"},
			since + "entry 1, label.rm sf-a1b2" + sent + "\n",
			map[string]any{"queued": true, "entry": 1.0, "op": "label.rm", "issue": "sf-a1b2", "since": "2026-10-13T14:02:00Z"}},
		{"accept", []string{"accept", "sf-a1b2", "1,2"},
			since + "entry 1, accept sf-a1b2" + sent + "\n",
			map[string]any{"queued": true, "entry": 1.0, "op": "accept", "issue": "sf-a1b2", "since": "2026-10-13T14:02:00Z"}},
		{"create has no id yet", []string{"create", "A", "new", "issue"},
			since + "entry 1, create" + sent + "; the issue has no id until then, so nothing queued can refer to it\n",
			map[string]any{"queued": true, "entry": 1.0, "op": "create", "since": "2026-10-13T14:02:00Z",
				"note": "the issue has no id until it is sent, so nothing queued can refer to it"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAway(t)
			a.reachedAs(t, "alice")
			code, out, errb := a.run(t, tc.args...)
			if code != ExitOK || out != tc.text || errb != "" {
				t.Errorf("aries %s = exit %d\nstdout %q\nwant   %q\nstderr %q, want none", strings.Join(tc.args, " "), code, out, tc.text, errb)
			}
			if got := a.pending(t, "alice"); len(got) != 1 {
				t.Errorf("alice's outbox holds %v, want the one write", got)
			}

			b := newAway(t)
			b.reachedAs(t, "alice")
			code, out, errb = b.run(t, append(tc.args, "--json")...)
			var doc map[string]any
			if err := json.Unmarshal([]byte(out), &doc); err != nil || strings.Count(out, "\n") != 1 {
				t.Fatalf("aries %s --json printed %q, want one JSON document: %v", strings.Join(tc.args, " "), out, err)
			}
			if code != ExitOK || errb != "" || fmt.Sprint(doc) != fmt.Sprint(tc.doc) {
				t.Errorf("aries %s --json = exit %d, %v\nwant %v\nstderr %q, want none", strings.Join(tc.args, " "), code, doc, tc.doc, errb)
			}
			for _, k := range []string{"id", "rev", "created"} {
				if _, ok := doc[k]; ok {
					t.Errorf("aries %s --json has %q, as if the write had been made: %v", strings.Join(tc.args, " "), k, doc)
				}
			}
		})
	}
}

// The outage began when its first write was queued; later ones say the
// same time, and take the next numbers.
func TestQueuedWritesNumberAndDate(t *testing.T) {
	t.Parallel()
	a := newAway(t)
	a.reachedAs(t, "alice")
	a.run(t, "comment", "sf-a", "one")
	a.now = a.now.Add(7 * time.Minute)
	_, out, _ := a.run(t, "comment", "sf-b", "two")
	if want := "queued (server unreachable since 2026-10-13 14:02 UTC): entry 2, comment sf-b;"; !strings.HasPrefix(out, want) {
		t.Errorf("the second queued write printed %q, want it to begin %q", out, want)
	}
}

// TestNoQueue pins --no-queue and ARIES_NO_QUEUE: the write fails as
// it did before there was an outbox, with exit code 1, and nothing is
// kept.
func TestNoQueue(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		env  string
	}{
		{"the flag, before the command", []string{"--no-queue", "handoff", "sf-a", "note"}, ""},
		{"the flag, after the command", []string{"handoff", "sf-a", "note", "--no-queue"}, ""},
		{"the variable", []string{"handoff", "sf-a", "note"}, "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAway(t)
			a.reachedAs(t, "alice")
			a.env[offline.EnvNoQueue] = tc.env
			code, out, errb := a.run(t, tc.args...)
			if code != ExitFailure || out != "" || !strings.HasPrefix(errb, "aries: 127.0.0.1:") ||
				!strings.Contains(errb, "closed the connection before the ssh handshake") ||
				!strings.Contains(errb, "\nfix: wait a minute") || strings.Contains(errb, "queue") {
				t.Errorf("exit %d, stdout %q, stderr %q; want exit %d and the failure to connect, as without an outbox", code, out, errb, ExitFailure)
			}
			if got := a.pending(t, "alice"); len(got) != 0 {
				t.Errorf("alice's outbox holds %v, want nothing", got)
			}
			code, out, _ = a.run(t, append(tc.args, "--json")...)
			var doc map[string]jsonError
			if err := json.Unmarshal([]byte(out), &doc); err != nil || code != ExitFailure || doc["error"].Code != proto.CodeUnavailable {
				t.Errorf("with --json: exit %d, %q (%v); want one document with the unavailable error", code, out, err)
			}
		})
	}
}

// A write the outbox does not queue fails as it always has, and its fix
// says that it was not queued and why. A read says nothing of queues.
func TestUnqueuedWriteSaysSo(t *testing.T) {
	t.Parallel()
	a := newAway(t)
	a.reachedAs(t, "alice")
	for _, tc := range []struct {
		args []string
		why  string
	}{
		{[]string{"start", "sf-a"}, "it takes a claim, and no claim is made without the server"},
		{[]string{"close", "sf-a"}, "it ends something on a state the client cannot see"},
		{[]string{"dep", "rm", "sf-a", "sf-b"}, "it undoes something on a state the client cannot see"},
		{[]string{"forget", "style"}, "it undoes something on a state the client cannot see"},
		{[]string{"list"}, ""},
		{[]string{"show", "sf-a"}, ""},
	} {
		code, out, errb := a.run(t, tc.args...)
		if code != ExitFailure || out != "" || !strings.HasPrefix(errb, "aries: 127.0.0.1:") ||
			!strings.Contains(errb, "closed the connection before the ssh handshake") {
			t.Errorf("aries %s = exit %d, stdout %q, stderr %q; want the failure to connect", strings.Join(tc.args, " "), code, out, errb)
		}
		want := "; this was not queued for later, since " + tc.why + ": run it again when the server answers\n"
		if tc.why == "" {
			if strings.Contains(errb, "queue") {
				t.Errorf("aries %s, a read, speaks of queues: %q", strings.Join(tc.args, " "), errb)
			}
		} else if !strings.HasSuffix(errb, want) || strings.Count(errb, "\n") != 2 {
			t.Errorf("aries %s printed %q, want two lines, the fix ending %q", strings.Join(tc.args, " "), errb, want)
		}
	}
	if got := a.pending(t, "alice"); len(got) != 0 {
		t.Errorf("alice's outbox holds %v, want nothing", got)
	}
}

// Several labels of one command are queued as a write each: a line for
// each, or with --json one document that lists them.
func TestQueuedLabels(t *testing.T) {
	t.Parallel()
	a := newAway(t)
	a.reachedAs(t, "alice")
	code, out, errb := a.run(t, "label", "add", "sf-a", "x", "y")
	if code != ExitOK || strings.Count(out, "\n") != 2 || !strings.Contains(out, "entry 1, label.add sf-a;") ||
		!strings.Contains(out, "entry 2, label.add sf-a;") || errb != "" {
		t.Errorf("aries label add with two labels = exit %d, stdout %q, stderr %q; want a queued line for each", code, out, errb)
	}
	code, out, _ = a.run(t, "label", "add", "sf-a", "z", "w", "--json")
	var doc struct {
		Queued  bool
		Entries []struct{ Entry int }
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || code != ExitOK || strings.Count(out, "\n") != 1 ||
		!doc.Queued || len(doc.Entries) != 2 || doc.Entries[0].Entry != 3 || doc.Entries[1].Entry != 4 {
		t.Errorf("aries label add --json with two labels = exit %d, %q (%v); want one document listing entries 3 and 4", code, out, err)
	}
	if got := a.pending(t, "alice"); len(got) != 4 {
		t.Errorf("alice's outbox holds %v, want the four labels", got)
	}
}

// An update without --rev reads the issue's rev first, which needs the
// server: it is not queued, and the fix says to give --rev.
func TestUpdateWithoutRevIsNotQueued(t *testing.T) {
	t.Parallel()
	a := newAway(t)
	a.reachedAs(t, "alice")
	code, out, errb := a.run(t, "update", "sf-a", "--title", "t")
	if code != ExitFailure || out != "" ||
		!strings.HasSuffix(errb, "; this update was not queued, since it needs the issue's rev, which only the server knows: give --rev N, the rev you based it on, to queue it\n") {
		t.Errorf("aries update without --rev = exit %d, stdout %q, stderr %q; want it to fail, saying to give --rev", code, out, errb)
	}
	if got := a.pending(t, "alice"); len(got) != 0 {
		t.Errorf("alice's outbox holds %v, want nothing", got)
	}
}

// A machine that never reached the server with this key cannot tell
// whose write it is: nothing is queued, and the refusal says so.
func TestNothingQueuedBeforeTheFirstHandshake(t *testing.T) {
	t.Parallel()
	a := newAway(t)
	code, out, errb := a.run(t, "handoff", "sf-a", "note")
	if code != ExitFailure || out != "" || !strings.Contains(errb, "and this handoff was not queued: this machine has never reached the server with this key") ||
		!strings.Contains(errb, "\nfix: run it again when the server answers; ") {
		t.Errorf("exit %d, stdout %q, stderr %q; want exit %d and the refusal with its fix", code, out, errb, ExitFailure)
	}
	code, out, _ = a.run(t, "handoff", "sf-a", "note", "--json")
	var doc map[string]map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil || code != ExitFailure || doc["error"]["code"] != "unavailable" || doc["queued"] != nil {
		t.Errorf("with --json: exit %d, %q (%v); want one document with the error", code, out, err)
	}
}

// What the outbox itself refuses is a refusal with a fix (rule 11), and
// exit code 1.
func TestOutboxRefusalsReachThePerson(t *testing.T) {
	t.Parallel()
	a := newAway(t)
	a.reachedAs(t, "alice")
	code, out, errb := a.run(t, "comment", "sf-a", "the", "token", "is", "ghp_0123456789abcdefghijABCDEFGHIJ012345")
	if code != ExitFailure || out != "" || !strings.Contains(errb, "this comment was not queued") ||
		!strings.Contains(errb, "\nfix: "+proto.FixSecret) || strings.Contains(errb, "ghp_") {
		t.Errorf("a comment with a token: exit %d, stdout %q, stderr %q; want the refusal, its fix, and not the token", code, out, errb)
	}
}

// refusing answers a replayed write as the server would one about an
// issue that is gone.
type refusing struct{ who string }

func (c refusing) Principal() string { return c.who }
func (refusing) Err() error          { return nil }
func (refusing) Call(context.Context, string, any, any) error {
	return proto.Errf(proto.CodeNotFound, "find the id with `aries list`", "issue sf-gone not found")
}

// outboxWith queues three writes as alice, the first of which the server
// then refuses, with the clock moving between them.
func outboxWith(t *testing.T) *away {
	t.Helper()
	a := newAway(t)
	a.reachedAs(t, "alice")
	a.run(t, "comment", "sf-gone", "too late")
	if _, err := a.local().Replay(t.Context(), refusing{"alice"}); err != nil {
		t.Fatal(err)
	}
	a.now = a.now.Add(10 * time.Minute)
	a.run(t, "handoff", "sf-a1b2", "half done")
	a.now = a.now.Add(20 * time.Minute)
	a.run(t, "create", "Something new")
	a.now = a.now.Add(5 * time.Minute)
	return a
}

func TestOutboxList(t *testing.T) {
	t.Parallel()
	a := outboxWith(t)
	code, out, errb := a.run(t, "outbox")
	want := "alice's outbox: 2 waiting, 1 refused\n" +
		"server unreachable since 2026-10-13 14:12 UTC; last tried 2026-10-13 14:32 UTC\n" +
		"waiting:\n" +
		"  N  OP       ON       AGE  FROM\n" +
		"  2  handoff  sf-a1b2  25m  cli on laptop\n" +
		"  3  create   -        5m   cli on laptop\n" +
		"refused:\n" +
		"  1  comment  sf-gone  35m  cli on laptop\n" +
		"     not_found: issue sf-gone not found\n" +
		"     fix: find the id with `aries list`\n" +
		"`aries outbox retry N` sends a refused entry again; `aries outbox drop N` removes an entry for good\n"
	if code != ExitOK || out != want || errb != "" {
		t.Errorf("aries outbox = exit %d\n%s\nwant\n%s\nstderr %q", code, out, want, errb)
	}

	code, out, _ = a.run(t, "outbox", "--json")
	var doc struct {
		Principal string
		Pending   []struct {
			Entry         int
			Op, Issue     string
			QueuedAt      time.Time `json:"queued_at"`
			Session, Host string
		}
		Refused []struct {
			Entry                    int
			Op, Issue, Code, Message string
			Fix                      string
			RefusedAt                time.Time `json:"refused_at"`
		}
		UnreachableSince time.Time `json:"unreachable_since"`
		LastNotReached   time.Time `json:"last_not_reached"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || strings.Count(out, "\n") != 1 || code != ExitOK {
		t.Fatalf("aries outbox --json = exit %d, %q, want one JSON document: %v", code, out, err)
	}
	if doc.Principal != "alice" || len(doc.Pending) != 2 || len(doc.Refused) != 1 || doc.Pending[0].Entry != 2 || doc.Pending[0].Op != "handoff" ||
		doc.Pending[0].Issue != "sf-a1b2" || doc.Pending[1].Issue != "" || doc.Refused[0].Code != "not_found" || doc.Refused[0].Fix == "" ||
		doc.Refused[0].RefusedAt.IsZero() || !doc.UnreachableSince.Equal(time.Date(2026, 10, 13, 14, 12, 0, 0, time.UTC)) || doc.LastNotReached.IsZero() {
		t.Errorf("aries outbox --json = %+v", doc)
	}
}

func TestOutboxListWhenNothingWaits(t *testing.T) {
	t.Parallel()
	a := newAway(t)
	if code, out, errb := a.run(t, "outbox"); code != ExitOK || errb != "" ||
		out != "nothing is queued: this machine has not reached the server with this key, so it queues nothing\n" {
		t.Errorf("aries outbox on a new machine = exit %d, %q, %q", code, out, errb)
	}
	a.reachedAs(t, "alice")
	if code, out, errb := a.run(t, "outbox"); code != ExitOK || errb != "" ||
		out != "alice's outbox is empty\nno failure to reach the server is recorded\n" {
		t.Errorf("aries outbox with nothing queued = exit %d, %q, %q", code, out, errb)
	}
	var doc map[string]any
	_, out, _ := a.run(t, "outbox", "--json")
	if err := json.Unmarshal([]byte(out), &doc); err != nil || doc["principal"] != "alice" || fmt.Sprint(doc["pending"]) != "[]" || fmt.Sprint(doc["refused"]) != "[]" {
		t.Errorf("aries outbox --json with nothing queued = %q (%v); want empty lists, not null", out, err)
	}
}

// Text in an entry and in a refusal is other people's: it is escaped.
func TestOutboxListEscapes(t *testing.T) {
	t.Parallel()
	a := newAway(t)
	a.reachedAs(t, "alice")
	a.run(t, "comment", "sf-\x1b[2Jx", "a note")
	_, out, _ := a.run(t, "outbox")
	if strings.ContainsRune(out, 0x1b) || !strings.Contains(out, `sf-\x1b[2Jx`) {
		t.Errorf("aries outbox printed an escape character from an entry: %q", out)
	}
}

func TestOutboxRetryAndDrop(t *testing.T) {
	t.Parallel()
	a := outboxWith(t)
	for _, tc := range []struct {
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{[]string{"outbox", "retry", "2"}, ExitFailure, "",
			"aries: no refused entry 2 in alice's outbox\nfix: `aries outbox` lists the entries; only a refused one is retried, and one that waits is sent by itself\n"},
		{[]string{"outbox", "drop", "9"}, ExitFailure, "", "aries: no entry 9 in alice's outbox\nfix: `aries outbox` lists the entries and their numbers\n"},
		{[]string{"outbox", "retry"}, ExitUsage, "", "aries: retry needs an entry's number\nfix: usage: aries outbox [retry N | drop N | stats] [--principal P] [--host-key FINGERPRINT]\n"},
		{[]string{"outbox", "drop", "x"}, ExitUsage, "", "aries: \"x\" is not an entry's number\nfix: usage: aries outbox [retry N | drop N | stats] [--principal P] [--host-key FINGERPRINT]\n"},
		{[]string{"outbox", "send"}, ExitUsage, "", "aries: unknown outbox action \"send\"\nfix: usage: aries outbox [retry N | drop N | stats] [--principal P] [--host-key FINGERPRINT]\n"},
		{[]string{"outbox", "retry", "1"}, ExitOK, "entry 1 (comment sf-gone) waits again: the next aries command that reaches the server sends it\n", ""},
		{[]string{"outbox", "drop", "3"}, ExitOK, "dropped entry 3 (create): it will not be sent\n", ""},
		{[]string{"outbox", "drop", "2", "--json"}, ExitOK, `{"entry":2,"state":"dropped"}` + "\n", ""},
	} {
		code, out, errb := a.run(t, tc.args...)
		if code != tc.code || out != tc.stdout || errb != tc.stderr {
			t.Errorf("aries %s = exit %d\nstdout %q\nwant   %q\nstderr %q\nwant   %q", strings.Join(tc.args, " "), code, out, tc.stdout, errb, tc.stderr)
		}
	}
	if got := a.pending(t, "alice"); len(got) != 1 || got[0] != "comment sf-gone" {
		t.Errorf("alice's outbox holds %v pending, want only the retried comment", got)
	}
	st, err := mustOutbox(t, a, "alice").Stats()
	if err != nil || st.Ops[proto.OpCreate].Dropped != 1 || st.Ops[proto.OpHandoff].Dropped != 1 {
		t.Errorf("Stats = %+v, %v; want the two drops counted", st, err)
	}
}

func mustOutbox(t *testing.T, a *away, principal string) *outbox.Outbox {
	t.Helper()
	ob, ok, err := a.local().Existing(principal)
	if err != nil || !ok {
		t.Fatalf("no outbox for %s: %v", principal, err)
	}
	return ob
}

func TestOutboxStats(t *testing.T) {
	t.Parallel()
	a := outboxWith(t)
	code, out, errb := a.run(t, "outbox", "stats")
	want := "alice's outbox, since it was made:\n" +
		"OP       QUEUED  DELIVERED  PARKED  REFUSED  STALE  DROPPED  MEAN WAIT  LONGEST\n" +
		"comment  1       0          0       1        0      0        -          -\n" +
		"create   1       0          0       0        0      0        -          -\n" +
		"handoff  1       0          0       0        0      0        -          -\n" +
		"outages: 1 over, 0m in all, the longest 0m; one in progress since 2026-10-13 14:12 UTC\n"
	if code != ExitOK || out != want || errb != "" {
		t.Errorf("aries outbox stats = exit %d\n%s\nwant\n%s\nstderr %q", code, out, want, errb)
	}
	code, out, _ = a.run(t, "outbox", "stats", "--json")
	var doc struct {
		Principal string
		outbox.Stats
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || strings.Count(out, "\n") != 1 || code != ExitOK {
		t.Fatalf("aries outbox stats --json = exit %d, %q, want one JSON document: %v", code, out, err)
	}
	if doc.Principal != "alice" || doc.Ops[proto.OpComment].Refused != 1 || doc.Ops[proto.OpHandoff].Queued != 1 || doc.Outages.Count != 1 || doc.Outages.Since.IsZero() {
		t.Errorf("aries outbox stats --json = %+v", doc)
	}
}

// One principal's outbox says when another's, on the same machine, has
// entries that wait, and --principal shows it: a key the server has
// since given to someone else leaves entries this key no longer sends.
func TestOutboxOfAnotherPrincipal(t *testing.T) {
	t.Parallel()
	a := newAway(t)
	a.reachedAs(t, "alice-agent")
	a.run(t, "comment", "sf-a", "from the agent")
	a.reachedAs(t, "alice") // the server now takes the key for alice
	_, out, _ := a.run(t, "outbox")
	want := "alice's outbox is empty\n" +
		"server last not reached 2026-10-13 14:02 UTC\n" +
		"alice-agent's outbox on this machine: 1 waiting, 0 refused; aries sends it on a connection as alice-agent (`aries outbox --principal alice-agent` lists it)\n"
	if out != want {
		t.Errorf("aries outbox =\n%s\nwant\n%s", out, want)
	}
	if _, out, _ := a.run(t, "outbox", "--principal", "alice-agent"); !strings.HasPrefix(out, "alice-agent's outbox: 1 waiting, 0 refused\n") {
		t.Errorf("aries outbox --principal alice-agent = %q", out)
	}
	if code, out, _ := a.run(t, "outbox", "drop", "1", "--principal", "alice-agent"); code != ExitOK || !strings.HasPrefix(out, "dropped entry 1 (comment sf-a)") {
		t.Errorf("aries outbox drop 1 --principal alice-agent = exit %d, %q", code, out)
	}
}

// The hooks do as they always did: nothing is queued, nothing is noted
// on the disk, and nothing new is said (CLAUDE.md, AGENTS.md).
func TestHooksQueueNothing(t *testing.T) {
	t.Parallel()
	a := newAway(t)
	a.reachedAs(t, "alice")
	before := snapshot(t, a.data)
	a.stdin = `{"session_id":"cc-1","hook_event_name":"SessionStart"}`
	code, out, errb := a.run(t, "prime", "--hook")
	if code != ExitOK || errb != "" || !strings.Contains(out, "aries: prime failed: ") || strings.Contains(out, "queue") {
		t.Errorf("prime --hook = exit %d, %q, %q; want its one note, and no word of queues", code, out, errb)
	}
	a.stdin = `{"session_id":"cc-1","hook_event_name":"Stop","transcript_path":"` + filepath.Join(a.repo, "none.jsonl") + `"}`
	if code, out, errb := a.run(t, "usage", "--hook"); code != ExitOK || out != "" || strings.Contains(errb, "queue") {
		t.Errorf("usage --hook = exit %d, %q, %q; want silence", code, out, errb)
	}
	if after := snapshot(t, a.data); !maps.Equal(after, before) {
		t.Errorf("the hooks changed the data directory:\n%s\nwas\n%s", after, before)
	}
}
