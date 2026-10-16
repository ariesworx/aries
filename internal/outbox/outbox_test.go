package outbox_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/ariesworx/aries/internal/outbox"
	"github.com/ariesworx/aries/internal/proto"
)

// start is where every test's clock begins.
var start = time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)

// clock is a client's clock that a test moves.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: start} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// open returns an outbox in a new directory, on clk when it is not nil.
func open(t *testing.T, opts outbox.Options, clk *clock) (*outbox.Outbox, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "outbox")
	if clk != nil {
		opts.Now = clk.Now
	}
	o, err := outbox.Open(dir, opts)
	if err != nil {
		t.Fatalf("Open(%s) = %v", dir, err)
	}
	return o, dir
}

// handoff is a handoff entry whose key and note are of i.
func handoff(i int) outbox.Entry {
	return outbox.Entry{
		Op:      proto.OpHandoff,
		Args:    fmt.Appendf(nil, `{"id":"aries-1","epoch":4,"note":"note %d","idem":"t-%d"}`, i, i),
		Session: "s-1",
		Machine: "m-1",
	}
}

func mustAppend(t *testing.T, o *outbox.Outbox, e outbox.Entry) outbox.Entry {
	t.Helper()
	got, err := o.Append(e)
	if err != nil {
		t.Fatalf("Append(%s %s) = %v", e.Op, e.Args, err)
	}
	return got
}

func mustList(t *testing.T, o *outbox.Outbox) outbox.Listing {
	t.Helper()
	l, err := o.List()
	if err != nil {
		t.Fatalf("List() = %v", err)
	}
	return l
}

// server is a stand-in for the server: it notes what it was sent, and
// answers as answer says, delivered when answer is nil.
type server struct {
	sent   []uint64
	keys   []string
	answer func(outbox.Entry) (outbox.Outcome, error)
}

func (s *server) send(_ context.Context, e outbox.Entry) (outbox.Outcome, error) {
	s.sent = append(s.sent, e.Seq)
	s.keys = append(s.keys, e.Idem)
	if s.answer == nil {
		return outbox.Outcome{}, nil
	}
	return s.answer(e)
}

func mustReplay(t *testing.T, o *outbox.Outbox, s *server) outbox.Report {
	t.Helper()
	rep, err := o.Replay(t.Context(), s.send)
	if err != nil {
		t.Fatalf("Replay() = %v", err)
	}
	return rep
}

func seqs(es []outbox.Entry) []uint64 {
	out := make([]uint64, len(es))
	for i, e := range es {
		out[i] = e.Seq
	}
	return out
}

func refusedSeqs(rs []outbox.Refusal) []uint64 {
	out := make([]uint64, len(rs))
	for i, r := range rs {
		out[i] = r.Entry.Seq
	}
	return out
}

func TestQueueable(t *testing.T) {
	t.Parallel()
	// Design §18.3.s table, each of which takes an idempotency key.
	queued := []string{proto.OpCreate, proto.OpComment, proto.OpHandoff, proto.OpRemember, proto.OpHoursLog, proto.OpFinish,
		proto.OpUpdate, proto.OpLabelAdd, proto.OpLabelRm, proto.OpDepAdd, proto.OpAccept, proto.OpUsage}
	for _, op := range queued {
		if !outbox.Queueable(op) {
			t.Errorf("Queueable(%q) = false, want true", op)
		}
	}
	never := []string{
		proto.OpStart, proto.OpClose, proto.OpReopen, proto.OpForget, proto.OpDepRm, proto.OpHoursDelete, proto.OpRenew,
		proto.OpProjectAdd, proto.OpKeyAdd, proto.OpShow, proto.OpPin, "",
	}
	for _, op := range never {
		if outbox.Queueable(op) {
			t.Errorf("Queueable(%q) = true, want false", op)
		}
	}
}

func TestAppendRefuses(t *testing.T) {
	t.Parallel()
	const secret = "hunter2-Xq9-zebra" //nolint:gosec // invented, for the lint to find
	tests := []struct {
		name string
		e    outbox.Entry
		opts outbox.Options
		want error
	}{
		{name: "start takes a claim", e: outbox.Entry{Op: proto.OpStart, Args: []byte(`{"id":"aries-1","idem":"k"}`)}, want: outbox.ErrNotQueueable},
		{name: "close ends something", e: outbox.Entry{Op: proto.OpClose, Args: []byte(`{"id":"aries-1","idem":"k"}`)}, want: outbox.ErrNotQueueable},
		{name: "an update without its key", e: outbox.Entry{Op: proto.OpUpdate, Args: []byte(`{"id":"aries-1","rev":2}`)}, want: outbox.ErrNoKey},
		{name: "usage without its key", e: outbox.Entry{Op: proto.OpUsage, Args: []byte(`{"records":[]}`)}, want: outbox.ErrNoKey},
		{name: "an admin op", e: outbox.Entry{Op: proto.OpMemberSet, Args: []byte(`{"idem":"k"}`)}, want: outbox.ErrNotQueueable},
		{name: "renew", e: outbox.Entry{Op: proto.OpRenew, Args: []byte(`{"idem":"k"}`)}, want: outbox.ErrNotQueueable},
		{name: "an unknown op", e: outbox.Entry{Op: "frobnicate", Args: []byte(`{"idem":"k"}`)}, want: outbox.ErrNotQueueable},
		{name: "no key", e: outbox.Entry{Op: proto.OpHandoff, Args: []byte(`{"id":"aries-1","note":"n"}`)}, want: outbox.ErrNoKey},
		{name: "an empty key", e: outbox.Entry{Op: proto.OpComment, Args: []byte(`{"id":"aries-1","body":"b","idem":""}`)}, want: outbox.ErrNoKey},
		{name: "a key that is not text", e: outbox.Entry{Op: proto.OpComment, Args: []byte(`{"id":"aries-1","body":"b","idem":7}`)}, want: outbox.ErrInvalid},
		{name: "a key that is not the arguments'", e: outbox.Entry{Op: proto.OpComment, Idem: "other", Args: []byte(`{"id":"aries-1","body":"b","idem":"k"}`)}, want: outbox.ErrInvalid},
		{name: "no arguments", e: outbox.Entry{Op: proto.OpComment}, want: outbox.ErrInvalid},
		{name: "arguments that are not an object", e: outbox.Entry{Op: proto.OpComment, Args: []byte(`["idem"]`)}, want: outbox.ErrInvalid},
		{name: "arguments that are not JSON", e: outbox.Entry{Op: proto.OpComment, Args: []byte(`{"idem":"k"`)}, want: outbox.ErrInvalid},
		{name: "two documents", e: outbox.Entry{Op: proto.OpComment, Args: []byte(`{"idem":"k"}{"idem":"k"}`)}, want: outbox.ErrInvalid},
		{name: "a password in a note", e: outbox.Entry{Op: proto.OpHandoff, Args: []byte(`{"id":"aries-1","note":"db password=` + secret + `","idem":"k"}`)}, want: outbox.ErrSecret},
		{name: "a private key in a memory", e: outbox.Entry{Op: proto.OpRemember, Args: []byte(`{"key":"deploy","body":"-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXk\n-----END OPENSSH PRIVATE KEY-----","idem":"k"}`)}, want: outbox.ErrSecret},
		{name: "a secret in discovered work", e: outbox.Entry{Op: proto.OpFinish, Args: []byte(`{"id":"aries-1","discovered":[{"title":"rotate api_key=` + secret + `"}],"idem":"k"}`)}, want: outbox.ErrSecret},
		{name: "a secret in a waiver", e: outbox.Entry{Op: proto.OpFinish, Args: []byte(`{"id":"aries-1","waived":{"2":"token: ` + secret + `"},"idem":"k"}`)}, want: outbox.ErrSecret},
		{
			name: "an entry past the size cap",
			e:    outbox.Entry{Op: proto.OpComment, Args: []byte(`{"id":"aries-1","body":"` + strings.Repeat("word ", 80) + `","idem":"k"}`)},
			opts: outbox.Options{MaxEntryBytes: 300},
			want: outbox.ErrTooLarge,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o, _ := open(t, tc.opts, nil)
			_, err := o.Append(tc.e)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Append(%s %s) = %v, want %v", tc.e.Op, tc.e.Args, err, tc.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("Append(%s) error %q repeats the secret", tc.e.Op, err)
			}
			if l := mustList(t, o); len(l.Pending) != 0 {
				t.Errorf("after the refusal, List() has %d pending, want 0", len(l.Pending))
			}
		})
	}
}

func TestAppendQueues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		op, args    string
		wantIdem    string
		rev, epoch  int64
		wantOneLine string // the arguments as stored, when not args itself
	}{
		{op: proto.OpCreate, args: `{"idem":"cli-1","title":"Fix <the> parser & \"quotes\"","labels":["a","b"]}`, wantIdem: "cli-1"},
		{op: proto.OpComment, args: `{"id":"aries-1","body":"line one\nline two é","idem":"cli-2"}`, wantIdem: "cli-2"},
		{op: proto.OpHandoff, args: `{"id":"aries-1","epoch":4,"note":"n","idem":"mcp-3"}`, wantIdem: "mcp-3", epoch: 4},
		{op: proto.OpRemember, args: `{"key":"build","body":"use make","rev":7,"idem":"mcp-4"}`, wantIdem: "mcp-4", rev: 7},
		{op: proto.OpHoursLog, args: `{"id":"aries-1","seconds":3600,"idem":"cli-5"}`, wantIdem: "cli-5"},
		{op: proto.OpFinish, args: `{"id":"aries-1","epoch":9,"handoff":"done","waived":{"2":"not needed"},"idem":"mcp-6"}`, wantIdem: "mcp-6", epoch: 9},
		{
			op: proto.OpComment, args: "{\n  \"id\": \"aries-1\",\n  \"body\": \"b\",\n  \"idem\": \"cli-7\"\n}", wantIdem: "cli-7",
			wantOneLine: `{"id":"aries-1","body":"b","idem":"cli-7"}`,
		},
	}
	clk := newClock()
	o, _ := open(t, outbox.Options{}, clk)
	for i, tc := range tests {
		clk.Advance(time.Minute)
		got, err := o.Append(outbox.Entry{Op: tc.op, Args: []byte(tc.args), Session: "s-1", Machine: "m-1"})
		if err != nil {
			t.Fatalf("Append(%s %s) = %v", tc.op, tc.args, err)
		}
		wantArgs := tc.args
		if tc.wantOneLine != "" {
			wantArgs = tc.wantOneLine
		}
		want := outbox.Entry{
			Seq: uint64(i + 1), Op: tc.op, Args: []byte(wantArgs), Idem: tc.wantIdem, Rev: tc.rev, Epoch: tc.epoch,
			Session: "s-1", Machine: "m-1", At: clk.Now(),
		}
		if !sameEntry(got, want) {
			t.Errorf("Append(%s %s) =\n %+v, want\n %+v", tc.op, tc.args, show(got), show(want))
		}
		l := mustList(t, o)
		if len(l.Pending) != i+1 {
			t.Fatalf("after %d appends, List() has %d pending", i+1, len(l.Pending))
		}
		if stored := l.Pending[i]; !sameEntry(stored, want) {
			t.Errorf("List() entry %d =\n %+v, want\n %+v", i+1, show(stored), show(want))
		}
	}
}

func sameEntry(a, b outbox.Entry) bool {
	return a.Seq == b.Seq && a.Op == b.Op && bytes.Equal(a.Args, b.Args) && a.Idem == b.Idem && a.Rev == b.Rev &&
		a.Epoch == b.Epoch && a.Session == b.Session && a.Machine == b.Machine && a.At.Equal(b.At)
}

func show(e outbox.Entry) string {
	return fmt.Sprintf("{seq %d op %s args %s idem %s rev %d epoch %d session %s machine %s at %s}",
		e.Seq, e.Op, e.Args, e.Idem, e.Rev, e.Epoch, e.Session, e.Machine, e.At.Format(time.RFC3339Nano))
}

func TestAppendAcceptsNewIdem(t *testing.T) {
	t.Parallel()
	// The secrets lint reads every string of the arguments, the key
	// among them, so a key the client makes must not look like a secret.
	o, _ := open(t, outbox.Options{}, nil)
	for _, prefix := range []string{"cli", "mcp"} {
		args := fmt.Appendf(nil, `{"id":"aries-1","body":"b","idem":%q}`, proto.NewIdem(prefix))
		if _, err := o.Append(outbox.Entry{Op: proto.OpComment, Args: args}); err != nil {
			t.Errorf("Append(comment %s) = %v, want it queued", args, err)
		}
	}
}

func TestFilesArePrivate(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no permission bits; the directory's access list is the user's data directory's")
	}
	o, dir := open(t, outbox.Options{}, nil)
	mustAppend(t, o, handoff(1))
	mustAppend(t, o, handoff(2))
	mustReplay(t, o, &server{answer: func(e outbox.Entry) (outbox.Outcome, error) {
		return outbox.Outcome{Refused: e.Seq == 1, Code: "conflict"}, nil
	}})
	if _, err := o.Stats(); err != nil {
		t.Fatalf("Stats() = %v", err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o700 {
		t.Errorf("directory mode = %o, want 700", got)
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 5 {
		t.Errorf("the directory holds %d files, want the outbox, the answers, the counts and two locks", len(files))
	}
	for _, f := range files {
		info, err := f.Info()
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %o, want 600", f.Name(), got)
		}
	}
}

func TestOpenTightensDirectory(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no permission bits")
	}
	dir := filepath.Join(t.TempDir(), "outbox")
	if err := os.Mkdir(dir, 0o755); err != nil { //nolint:gosec // the mode Open must tighten
		t.Fatal(err)
	}
	if _, err := outbox.Open(dir, outbox.Options{}); err != nil {
		t.Fatalf("Open(%s) = %v", dir, err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o700 {
		t.Errorf("after Open, directory mode = %o, want 700", got)
	}
}

func TestCapEntries(t *testing.T) {
	t.Parallel()
	deliver := func(outbox.Entry) (outbox.Outcome, error) { return outbox.Outcome{}, nil }
	refuse := func(outbox.Entry) (outbox.Outcome, error) {
		return outbox.Outcome{Refused: true, Code: "forbidden"}, nil
	}
	tests := []struct {
		name    string
		max     int
		queue   int
		answer  func(outbox.Entry) (outbox.Outcome, error) // replayed after queue appends, when not nil
		wantErr error
	}{
		{name: "below the cap", max: 3, queue: 2},
		{name: "at the cap", max: 3, queue: 3, wantErr: outbox.ErrFull},
		{name: "a cap of one", max: 1, queue: 1, wantErr: outbox.ErrFull},
		{name: "refused entries still count", max: 3, queue: 3, answer: refuse, wantErr: outbox.ErrFull},
		{name: "delivered entries make room", max: 3, queue: 3, answer: deliver},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o, _ := open(t, outbox.Options{MaxEntries: tc.max}, nil)
			for i := range tc.queue {
				mustAppend(t, o, handoff(i))
			}
			if tc.answer != nil {
				mustReplay(t, o, &server{answer: tc.answer})
			}
			_, err := o.Append(handoff(tc.queue))
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("with a cap of %d and %d queued, Append() = %v, want %v", tc.max, tc.queue, err, tc.wantErr)
			}
		})
	}
	t.Run("the default", func(t *testing.T) {
		if outbox.DefaultMaxEntries != 1000 {
			t.Errorf("DefaultMaxEntries = %d, want 1000", outbox.DefaultMaxEntries)
		}
	})
}

func TestCapBytes(t *testing.T) {
	t.Parallel()
	// Every handoff(i) with one digit is a line of the same length,
	// on one clock.
	probe, dir := open(t, outbox.Options{}, newClock())
	mustAppend(t, probe, handoff(1))
	st, err := os.Stat(filepath.Join(dir, "outbox.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	line := st.Size()

	tests := []struct {
		name     string
		max      int64
		wantFits int
	}{
		{name: "room for none", max: line - 1, wantFits: 0},
		{name: "room for exactly one", max: line, wantFits: 1},
		{name: "a byte short of two", max: 2*line - 1, wantFits: 1},
		{name: "room for exactly three", max: 3 * line, wantFits: 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o, _ := open(t, outbox.Options{MaxBytes: tc.max}, newClock())
			fits := 0
			for i := 1; i <= 5; i++ {
				_, err := o.Append(handoff(i))
				if err == nil {
					fits++
					continue
				}
				if !errors.Is(err, outbox.ErrFull) {
					t.Fatalf("Append() %d with a cap of %d bytes = %v, want ErrFull", i, tc.max, err)
				}
				break
			}
			if fits != tc.wantFits {
				t.Errorf("with a cap of %d bytes and lines of %d, %d entries fit, want %d", tc.max, line, fits, tc.wantFits)
			}
			if l := mustList(t, o); len(l.Pending) != tc.wantFits {
				t.Errorf("List() has %d pending, want %d", len(l.Pending), tc.wantFits)
			}
		})
	}
}

func TestCapAge(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		maxAge    time.Duration // zero for the default
		waited    time.Duration
		wantStale bool
	}{
		{name: "new", waited: 0},
		{name: "a day and a second, well within the default", waited: 24*time.Hour + time.Second},
		{name: "a minute short of the default", waited: 7*24*time.Hour - time.Minute},
		{name: "at the default, the server's window", waited: 7 * 24 * time.Hour},
		{name: "past the default", waited: 7*24*time.Hour + time.Nanosecond, wantStale: true},
		{name: "past a shorter cap", maxAge: time.Hour, waited: time.Hour + time.Second, wantStale: true},
		{name: "within a longer cap", maxAge: 30 * 24 * time.Hour, waited: 20 * 24 * time.Hour},
		{name: "a clock that went back", waited: -time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clk := newClock()
			o, _ := open(t, outbox.Options{MaxAge: tc.maxAge}, clk)
			mustAppend(t, o, handoff(1))
			clk.Advance(tc.waited)
			var s server
			rep := mustReplay(t, o, &s)
			l := mustList(t, o)
			if !tc.wantStale {
				if rep.Delivered != 1 || rep.Stale != 0 || len(s.sent) != 1 || len(l.Refused) != 0 {
					t.Errorf("after %s, Replay() = %+v with %d sent and %d refused, want it delivered", tc.waited, rep, len(s.sent), len(l.Refused))
				}
				return
			}
			if rep.Stale != 1 || rep.Delivered != 0 || len(s.sent) != 0 {
				t.Fatalf("after %s, Replay() = %+v with %d sent, want it stale and not sent", tc.waited, rep, len(s.sent))
			}
			if len(l.Pending) != 0 || len(l.Refused) != 1 {
				t.Fatalf("List() = %d pending, %d refused, want the stale entry refused", len(l.Pending), len(l.Refused))
			}
			r := l.Refused[0]
			if r.Code != outbox.CodeStale || r.Message == "" || r.Fix == "" || !r.At.Equal(clk.Now()) {
				t.Errorf("refusal = code %q message %q fix %q at %s, want code %q with a message and a fix, at %s",
					r.Code, r.Message, r.Fix, r.At, outbox.CodeStale, clk.Now())
			}
		})
	}
}

func TestRetryOfStaleEntryIsSent(t *testing.T) {
	t.Parallel()
	clk := newClock()
	o, _ := open(t, outbox.Options{}, clk)
	e := mustAppend(t, o, handoff(1))
	clk.Advance(outbox.DefaultMaxAge + time.Hour)
	var s server
	if rep := mustReplay(t, o, &s); rep.Stale != 1 {
		t.Fatalf("Replay() = %+v, want one stale", rep)
	}
	if err := o.Retry(e.Seq); err != nil {
		t.Fatalf("Retry(%d) = %v", e.Seq, err)
	}
	clk.Advance(time.Hour)
	rep := mustReplay(t, o, &s)
	if rep.Delivered != 1 || !slices.Equal(s.keys, []string{"t-1"}) {
		t.Errorf("after Retry, Replay() = %+v and sent keys %v, want it delivered with its first key", rep, s.keys)
	}
	if l := mustList(t, o); len(l.Pending) != 0 || len(l.Refused) != 0 {
		t.Errorf("after delivery List() = %d pending, %d refused, want none", len(l.Pending), len(l.Refused))
	}
}

func TestListSkipsDamage(t *testing.T) {
	t.Parallel()
	o, dir := open(t, outbox.Options{}, nil)
	mustAppend(t, o, handoff(1))
	path := filepath.Join(dir, "outbox.ndjson")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	// A line that is not JSON, lines that are JSON and no entry, and the
	// first half of an entry with no newline, as a writer killed between
	// two writes leaves it.
	damage := "not json\n" + `{"seq":0}` + "\n" + `[1,2]` + "\n" + `{"seq":9,"op":"handoff","args":{"id":"aries-1","note":"torn`
	if _, err := f.WriteString(damage); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if got := seqs(mustList(t, o).Pending); !slices.Equal(got, []uint64{1}) {
		t.Errorf("List() of a damaged outbox = entries %v, want [1]", got)
	}
	e := mustAppend(t, o, handoff(2))
	if e.Seq != 2 {
		t.Errorf("Append() after a torn line took seq %d, want 2", e.Seq)
	}
	if got := seqs(mustList(t, o).Pending); !slices.Equal(got, []uint64{1, 2}) {
		t.Errorf("List() after appending to a torn outbox = entries %v, want [1 2]", got)
	}
	var s server
	if rep := mustReplay(t, o, &s); rep.Delivered != 2 || !slices.Equal(s.sent, []uint64{1, 2}) {
		t.Errorf("Replay() = %+v, sent %v, want [1 2] delivered", rep, s.sent)
	}
}

func TestReplayDeliversInOrder(t *testing.T) {
	t.Parallel()
	clk := newClock()
	o, _ := open(t, outbox.Options{}, clk)
	for i := 1; i <= 5; i++ {
		mustAppend(t, o, handoff(i))
	}
	clk.Advance(time.Minute)
	s := &server{answer: func(e outbox.Entry) (outbox.Outcome, error) {
		if e.Seq == 2 {
			return outbox.Outcome{Refused: true, Code: "conflict", Message: "aries-1 is held by bob", Fix: "ask bob"}, nil
		}
		return outbox.Outcome{}, nil
	}}
	rep := mustReplay(t, o, s)
	if want := (outbox.Report{Delivered: 4, Refused: 1}); rep != want {
		t.Errorf("Replay() = %+v, want %+v", rep, want)
	}
	if !slices.Equal(s.sent, []uint64{1, 2, 3, 4, 5}) {
		t.Errorf("Replay() sent %v, want 1 to 5 in order", s.sent)
	}
	if !slices.Equal(s.keys, []string{"t-1", "t-2", "t-3", "t-4", "t-5"}) {
		t.Errorf("Replay() sent keys %v, want each entry's own", s.keys)
	}
	l := mustList(t, o)
	if len(l.Pending) != 0 || len(l.Refused) != 1 {
		t.Fatalf("List() = %d pending, %d refused, want 0 and 1", len(l.Pending), len(l.Refused))
	}
	r := l.Refused[0]
	if r.Entry.Seq != 2 || r.Entry.Idem != "t-2" || r.Code != "conflict" || r.Message != "aries-1 is held by bob" || r.Fix != "ask bob" || !r.At.Equal(clk.Now()) {
		t.Errorf("refusal = %+v, want entry 2 with the server's code, message and fix at %s", r, clk.Now())
	}

	// A refusal is never retried by itself.
	again := &server{}
	if rep := mustReplay(t, o, again); len(again.sent) != 0 || rep != (outbox.Report{}) {
		t.Errorf("a second Replay() = %+v and sent %v, want nothing sent", rep, again.sent)
	}
}

func TestReplayStopsWhenServerIsNotReached(t *testing.T) {
	t.Parallel()
	o, _ := open(t, outbox.Options{}, nil)
	for i := 1; i <= 5; i++ {
		mustAppend(t, o, handoff(i))
	}
	errDown := errors.New("connection refused")
	s := &server{answer: func(e outbox.Entry) (outbox.Outcome, error) {
		if e.Seq == 3 {
			return outbox.Outcome{}, errDown
		}
		return outbox.Outcome{}, nil
	}}
	rep := mustReplay(t, o, s)
	if rep.Delivered != 2 || rep.Pending != 3 || !errors.Is(rep.Stopped, errDown) {
		t.Errorf("Replay() = %+v, want 2 delivered, 3 pending, stopped by %v", rep, errDown)
	}
	if !slices.Equal(s.sent, []uint64{1, 2, 3}) {
		t.Errorf("Replay() sent %v, want it to stop at 3", s.sent)
	}
	if got := seqs(mustList(t, o).Pending); !slices.Equal(got, []uint64{3, 4, 5}) {
		t.Errorf("List() pending = %v, want [3 4 5]", got)
	}

	// The connection is back: replay resumes at the same entry, with
	// the same key.
	back := &server{}
	rep = mustReplay(t, o, back)
	if rep.Delivered != 3 || rep.Pending != 0 || rep.Stopped != nil {
		t.Errorf("the next Replay() = %+v, want 3 delivered and nothing left", rep)
	}
	if !slices.Equal(back.keys, []string{"t-3", "t-4", "t-5"}) {
		t.Errorf("the next Replay() sent keys %v, want t-3 to t-5", back.keys)
	}
}

func TestReplayHonorsCancellation(t *testing.T) {
	t.Parallel()
	o, _ := open(t, outbox.Options{}, nil)
	for i := 1; i <= 3; i++ {
		mustAppend(t, o, handoff(i))
	}
	ctx, cancel := context.WithCancel(t.Context())
	s := &server{answer: func(e outbox.Entry) (outbox.Outcome, error) {
		if e.Seq == 1 {
			cancel() // the caller gives up while the first is being answered
		}
		return outbox.Outcome{}, nil
	}}
	rep, err := o.Replay(ctx, s.send)
	if err != nil {
		t.Fatalf("Replay() = %v", err)
	}
	if rep.Delivered != 1 || rep.Pending != 2 || !errors.Is(rep.Stopped, context.Canceled) {
		t.Errorf("Replay() = %+v, want the answered entry recorded, 2 pending, stopped by the context", rep)
	}
	if !slices.Equal(s.sent, []uint64{1}) {
		t.Errorf("Replay() sent %v, want only the first", s.sent)
	}
}

func TestReplayBoundsOneSend(t *testing.T) {
	// Serial: a synctest bubble's virtual time takes no real time worth sharing.
	synctest.Test(t, func(t *testing.T) {
		o, _ := open(t, outbox.Options{SendTimeout: 3 * time.Second, ReplayBudget: time.Minute}, nil)
		mustAppend(t, o, handoff(1))
		mustAppend(t, o, handoff(2))
		began := time.Now()
		calls := 0
		rep, err := o.Replay(t.Context(), func(ctx context.Context, _ outbox.Entry) (outbox.Outcome, error) {
			calls++
			<-ctx.Done() // a server that never answers
			return outbox.Outcome{}, ctx.Err()
		})
		if err != nil {
			t.Fatalf("Replay() = %v", err)
		}
		if got := time.Since(began); got != 3*time.Second {
			t.Errorf("Replay() against a server that never answers took %s, want the send timeout of 3s", got)
		}
		if calls != 1 || rep.Pending != 2 || !errors.Is(rep.Stopped, context.DeadlineExceeded) {
			t.Errorf("Replay() = %+v after %d sends, want 1 send, 2 pending, stopped by the deadline", rep, calls)
		}
	})
}

func TestReplayBoundsTheCall(t *testing.T) {
	// Serial: a synctest bubble's virtual time takes no real time worth sharing.
	synctest.Test(t, func(t *testing.T) {
		o, _ := open(t, outbox.Options{SendTimeout: time.Minute, ReplayBudget: 5 * time.Second}, nil)
		for i := 1; i <= 4; i++ {
			mustAppend(t, o, handoff(i))
		}
		began := time.Now()
		var sent []uint64
		rep, err := o.Replay(t.Context(), func(ctx context.Context, e outbox.Entry) (outbox.Outcome, error) {
			sent = append(sent, e.Seq)
			select { // a server that takes 2s to answer
			case <-time.After(2 * time.Second):
				return outbox.Outcome{}, nil
			case <-ctx.Done():
				return outbox.Outcome{}, ctx.Err()
			}
		})
		if err != nil {
			t.Fatalf("Replay() = %v", err)
		}
		if got := time.Since(began); got != 5*time.Second {
			t.Errorf("Replay() took %s, want its budget of 5s", got)
		}
		if rep.Delivered != 2 || rep.Pending != 2 || !errors.Is(rep.Stopped, context.DeadlineExceeded) {
			t.Errorf("Replay() = %+v, want 2 delivered, 2 pending, stopped by the deadline", rep)
		}
		if !slices.Equal(sent, []uint64{1, 2, 3}) {
			t.Errorf("Replay() sent %v, want [1 2 3], the third cut short", sent)
		}
	})
}

func TestReplaySendsWhatWasQueuedMeanwhile(t *testing.T) {
	t.Parallel()
	o, _ := open(t, outbox.Options{}, nil)
	mustAppend(t, o, handoff(1))
	s := &server{}
	s.answer = func(e outbox.Entry) (outbox.Outcome, error) {
		if e.Seq == 1 {
			mustAppend(t, o, handoff(2)) // another session queues while this one replays
		}
		return outbox.Outcome{}, nil
	}
	rep := mustReplay(t, o, s)
	if rep.Delivered != 2 || rep.Pending != 0 || !slices.Equal(s.sent, []uint64{1, 2}) {
		t.Errorf("Replay() = %+v, sent %v, want both delivered", rep, s.sent)
	}
}

func TestOneReplayAtATimeInOneProcess(t *testing.T) {
	t.Parallel()
	// aries mcp replays in the background of the process that serves tool
	// calls, so a second handle in the same process must be kept out.
	o, dir := open(t, outbox.Options{}, nil)
	e := mustAppend(t, o, handoff(1))
	other, err := outbox.Open(dir, outbox.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s := &server{}
	s.answer = func(outbox.Entry) (outbox.Outcome, error) {
		for _, h := range []*outbox.Outbox{o, other} {
			inner, err := h.Replay(t.Context(), func(context.Context, outbox.Entry) (outbox.Outcome, error) {
				t.Error("a second Replay() sent an entry while the first was replaying")
				return outbox.Outcome{}, nil
			})
			if err != nil || !inner.Busy {
				t.Errorf("a second Replay() = %+v, %v, want Busy", inner, err)
			}
			if err := h.Drop(e.Seq); !errors.Is(err, outbox.ErrBusy) {
				t.Errorf("Drop() during a replay = %v, want ErrBusy", err)
			}
			if err := h.Retry(e.Seq); !errors.Is(err, outbox.ErrBusy) {
				t.Errorf("Retry() during a replay = %v, want ErrBusy", err)
			}
		}
		return outbox.Outcome{}, nil
	}
	if rep := mustReplay(t, o, s); rep.Delivered != 1 || rep.Busy {
		t.Errorf("Replay() = %+v, want 1 delivered", rep)
	}
}

func TestRetryAndDrop(t *testing.T) {
	t.Parallel()
	refuseOdd := func(e outbox.Entry) (outbox.Outcome, error) {
		return outbox.Outcome{Refused: e.Seq%2 == 1, Code: "forbidden", Message: "no access"}, nil
	}
	down := func(outbox.Entry) (outbox.Outcome, error) { return outbox.Outcome{}, errors.New("down") }

	// Entries 1 and 3 are refused, 2 is delivered, 4 and 5 are pending.
	setup := func(t *testing.T) *outbox.Outbox {
		o, _ := open(t, outbox.Options{}, nil)
		for i := 1; i <= 3; i++ {
			mustAppend(t, o, handoff(i))
		}
		mustReplay(t, o, &server{answer: refuseOdd})
		mustAppend(t, o, handoff(4))
		mustAppend(t, o, handoff(5))
		mustReplay(t, o, &server{answer: down})
		return o
	}
	tests := []struct {
		name        string
		do          func(*outbox.Outbox) error
		wantErr     error
		wantPending []uint64
		wantRefused []uint64
	}{
		{name: "retry a refused entry", do: func(o *outbox.Outbox) error { return o.Retry(3) }, wantPending: []uint64{3, 4, 5}, wantRefused: []uint64{1}},
		{name: "retry a pending entry", do: func(o *outbox.Outbox) error { return o.Retry(4) }, wantErr: outbox.ErrNotFound, wantPending: []uint64{4, 5}, wantRefused: []uint64{1, 3}},
		{name: "retry a delivered entry", do: func(o *outbox.Outbox) error { return o.Retry(2) }, wantErr: outbox.ErrNotFound, wantPending: []uint64{4, 5}, wantRefused: []uint64{1, 3}},
		{name: "retry an entry that never was", do: func(o *outbox.Outbox) error { return o.Retry(99) }, wantErr: outbox.ErrNotFound, wantPending: []uint64{4, 5}, wantRefused: []uint64{1, 3}},
		{name: "drop a refused entry", do: func(o *outbox.Outbox) error { return o.Drop(1) }, wantPending: []uint64{4, 5}, wantRefused: []uint64{3}},
		{name: "drop a pending entry", do: func(o *outbox.Outbox) error { return o.Drop(4) }, wantPending: []uint64{5}, wantRefused: []uint64{1, 3}},
		{name: "drop a delivered entry", do: func(o *outbox.Outbox) error { return o.Drop(2) }, wantErr: outbox.ErrNotFound, wantPending: []uint64{4, 5}, wantRefused: []uint64{1, 3}},
		{name: "drop an entry that never was", do: func(o *outbox.Outbox) error { return o.Drop(0) }, wantErr: outbox.ErrNotFound, wantPending: []uint64{4, 5}, wantRefused: []uint64{1, 3}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := setup(t)
			if err := tc.do(o); !errors.Is(err, tc.wantErr) {
				t.Fatalf("got %v, want %v", err, tc.wantErr)
			}
			l := mustList(t, o)
			if got := seqs(l.Pending); !slices.Equal(got, tc.wantPending) {
				t.Errorf("pending = %v, want %v", got, tc.wantPending)
			}
			if got := refusedSeqs(l.Refused); !slices.Equal(got, tc.wantRefused) {
				t.Errorf("refused = %v, want %v", got, tc.wantRefused)
			}
			// What is pending is sent, in order; what is not, is not.
			var s server
			mustReplay(t, o, &s)
			if !slices.Equal(s.sent, tc.wantPending) {
				t.Errorf("the next Replay() sent %v, want %v", s.sent, tc.wantPending)
			}
		})
	}
}

// TestRefuseAll: when the server refuses the connection the entries
// would go on, for good (a principal's access ended), every pending
// entry moves to the refused list with that answer, at once, and is
// counted refused; one already refused keeps its own answer. Nothing is
// sent, nothing is lost, and a person's retry sends one again.
func TestRefuseAll(t *testing.T) {
	t.Parallel()
	clk := newClock()
	o, _ := open(t, outbox.Options{}, clk)
	for i := 1; i <= 3; i++ {
		mustAppend(t, o, handoff(i))
	}
	mustReplay(t, o, &server{answer: func(e outbox.Entry) (outbox.Outcome, error) {
		if e.Seq == 1 {
			return outbox.Outcome{Refused: true, Code: "not_found", Message: "issue aries-1 not found"}, nil
		}
		return outbox.Outcome{}, errors.New("down")
	}})
	clk.Advance(time.Hour)
	ended := outbox.Outcome{Refused: true, Code: "forbidden", Message: "alice is disabled on this server", Fix: "ask a server admin"}
	n, err := o.RefuseAll(ended)
	if err != nil || n != 2 {
		t.Fatalf("RefuseAll(%+v) = %d, %v; want the 2 pending entries refused", ended, n, err)
	}
	l := mustList(t, o)
	if len(l.Pending) != 0 || !slices.Equal(refusedSeqs(l.Refused), []uint64{1, 2, 3}) {
		t.Fatalf("after RefuseAll: pending %v, refused %v; want none pending and 1, 2 and 3 refused", seqs(l.Pending), refusedSeqs(l.Refused))
	}
	for _, r := range l.Refused {
		want, at := ended, start.Add(time.Hour)
		if r.Entry.Seq == 1 {
			want, at = outbox.Outcome{Refused: true, Code: "not_found", Message: "issue aries-1 not found"}, start
		}
		if r.Code != want.Code || r.Message != want.Message || r.Fix != want.Fix || !r.At.Equal(at) {
			t.Errorf("entry %d is refused as %s: %q, fix %q at %v; want %s: %q, fix %q at %v",
				r.Entry.Seq, r.Code, r.Message, r.Fix, r.At, want.Code, want.Message, want.Fix, at)
		}
	}
	st, err := o.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Ops[proto.OpHandoff].Refused; got != 3 {
		t.Errorf("Stats() counts %d handoffs refused, want 3", got)
	}
	if n, err := o.RefuseAll(ended); err != nil || n != 0 {
		t.Errorf("RefuseAll() with nothing pending = %d, %v; want 0, nil", n, err)
	}
	var s server
	if mustReplay(t, o, &s); len(s.sent) != 0 {
		t.Errorf("Replay() after RefuseAll sent %v, want nothing", s.sent)
	}
	if err := o.Retry(2); err != nil {
		t.Fatal(err)
	}
	if mustReplay(t, o, &s); !slices.Equal(s.sent, []uint64{2}) {
		t.Errorf("Replay() after Retry(2) sent %v, want [2]", s.sent)
	}
}

// TestRefuseAllWaitsForNoReplay: a replay in progress is on a
// connection that was let in, so it decides; RefuseAll says it is busy.
func TestRefuseAllWaitsForNoReplay(t *testing.T) {
	t.Parallel()
	o, _ := open(t, outbox.Options{}, nil)
	mustAppend(t, o, handoff(1))
	s := &server{answer: func(outbox.Entry) (outbox.Outcome, error) {
		if _, err := o.RefuseAll(outbox.Outcome{Refused: true, Code: "forbidden"}); !errors.Is(err, outbox.ErrBusy) {
			t.Errorf("RefuseAll() during a replay = %v, want ErrBusy", err)
		}
		return outbox.Outcome{}, nil
	}}
	if rep := mustReplay(t, o, s); rep.Delivered != 1 {
		t.Errorf("Replay() = %+v, want the entry delivered", rep)
	}
}

func TestSequenceSurvivesCompaction(t *testing.T) {
	t.Parallel()
	o, dir := open(t, outbox.Options{}, nil)
	for i := 1; i <= 20; i++ {
		mustAppend(t, o, handoff(i))
	}
	one, err := os.Stat(filepath.Join(dir, "outbox.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if rep := mustReplay(t, o, &server{}); rep.Delivered != 20 {
		t.Fatalf("Replay() = %+v, want 20 delivered", rep)
	}
	for _, name := range []string{"outbox.ndjson", "answered.ndjson"} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if st.Size() > one.Size()/20 {
			t.Errorf("after everything was delivered %s is %d bytes, want it compacted to less than one entry's %d", name, st.Size(), one.Size()/20)
		}
	}

	// A number is never used twice: an answer left from before the
	// compaction must not be taken for the new entry's.
	e := mustAppend(t, o, handoff(21))
	if e.Seq != 21 {
		t.Errorf("Append() after compaction took seq %d, want 21", e.Seq)
	}
	var s server
	if rep := mustReplay(t, o, &s); rep.Delivered != 1 || !slices.Equal(s.sent, []uint64{21}) {
		t.Errorf("Replay() = %+v, sent %v, want entry 21 delivered", rep, s.sent)
	}
}

func TestCompactionKeepsRefusals(t *testing.T) {
	t.Parallel()
	o, _ := open(t, outbox.Options{}, nil)
	for i := 1; i <= 4; i++ {
		mustAppend(t, o, handoff(i))
	}
	mustReplay(t, o, &server{answer: func(e outbox.Entry) (outbox.Outcome, error) {
		return outbox.Outcome{Refused: e.Seq == 2, Code: "invalid", Message: "m", Fix: "f"}, nil
	}})
	// Retry and refuse it again, several times: each round leaves the
	// one refusal, with its latest reason.
	for round := range 3 {
		if err := o.Retry(2); err != nil {
			t.Fatalf("Retry(2) = %v", err)
		}
		mustReplay(t, o, &server{answer: func(outbox.Entry) (outbox.Outcome, error) {
			return outbox.Outcome{Refused: true, Code: "invalid", Message: fmt.Sprintf("round %d", round)}, nil
		}})
		l := mustList(t, o)
		if len(l.Pending) != 0 || len(l.Refused) != 1 || l.Refused[0].Message != fmt.Sprintf("round %d", round) {
			t.Fatalf("round %d: List() = %+v, want the one refusal with this round's message", round, l)
		}
	}
}

func TestRefusalTextIsBounded(t *testing.T) {
	t.Parallel()
	o, _ := open(t, outbox.Options{}, nil)
	mustAppend(t, o, handoff(1))
	long := "a" + strings.Repeat("é", 100_000) // the cap falls inside a character
	mustReplay(t, o, &server{answer: func(outbox.Entry) (outbox.Outcome, error) {
		return outbox.Outcome{Refused: true, Code: "invalid", Message: long, Fix: long}, nil
	}})
	r := mustList(t, o).Refused[0]
	for name, s := range map[string]string{"message": r.Message, "fix": r.Fix} {
		if len(s) > 1024 || len(s) < 1000 || !strings.HasPrefix(long, s) || !utf8.ValidString(s) {
			t.Errorf("the refusal's %s is %d bytes, want a prefix of the server's of whole characters, 1024 bytes at most", name, len(s))
		}
	}
}

func TestFailedCompactionLosesNothing(t *testing.T) {
	t.Parallel()
	// A compaction that cannot replace a file, as one on Windows could
	// not while another program has it open, leaves both files as they
	// were: what was delivered stays delivered, and what waits, waits.
	o, dir := open(t, outbox.Options{}, nil)
	for i := 1; i <= 3; i++ {
		mustAppend(t, o, handoff(i))
	}
	block := filepath.Join(dir, "outbox.ndjson.tmp")
	if err := os.Mkdir(block, 0o700); err != nil {
		t.Fatal(err)
	}
	s := &server{answer: func(e outbox.Entry) (outbox.Outcome, error) {
		return outbox.Outcome{Refused: e.Seq == 2, Code: "conflict"}, nil
	}}
	rep, err := o.Replay(t.Context(), s.send)
	if err == nil {
		t.Errorf("Replay() whose compaction cannot write = nil error, want it reported")
	}
	if rep.Delivered != 2 || rep.Refused != 1 {
		t.Errorf("Replay() whose compaction failed = %+v, want its 2 deliveries and 1 refusal to stand", rep)
	}
	mustAppend(t, o, handoff(4))
	l := mustList(t, o)
	if got := seqs(l.Pending); !slices.Equal(got, []uint64{4}) {
		t.Errorf("pending = %v, want [4]", got)
	}
	if got := refusedSeqs(l.Refused); !slices.Equal(got, []uint64{2}) {
		t.Errorf("refused = %v, want [2]", got)
	}

	// Once the file can be replaced, the next replay compacts.
	if err := os.Remove(block); err != nil {
		t.Fatal(err)
	}
	again := &server{}
	if rep := mustReplay(t, o, again); rep.Delivered != 1 || !slices.Equal(again.sent, []uint64{4}) {
		t.Errorf("the next Replay() = %+v, sent %v, want entry 4 delivered and nothing sent twice", rep, again.sent)
	}
	if got := refusedSeqs(mustList(t, o).Refused); !slices.Equal(got, []uint64{2}) {
		t.Errorf("refused after compaction = %v, want [2]", got)
	}
}

// FuzzFiles reads an outbox whose two files hold anything at all: files
// a crash, another program or a person left. Whatever is in them, the
// outbox lists, queues and replays without error, a new entry takes a
// number no listed entry has, and everything pending is sent.
func FuzzFiles(f *testing.F) {
	entry := `{"seq":1,"op":"handoff","args":{"id":"aries-1","note":"n","idem":"k-1"},"idem":"k-1","at":"2026-10-13T09:00:00Z"}` + "\n"
	second := `{"seq":2,"op":"comment","args":{"id":"aries-1","body":"b","idem":"k-2"},"idem":"k-2","at":"2026-10-13T09:00:00Z"}` + "\n"
	f.Add([]byte(""), []byte(""))
	f.Add([]byte(entry+second), []byte(""))
	f.Add([]byte(entry+second), []byte(`{"seq":1,"state":"delivered","at":"2026-10-13T09:01:00Z"}`+"\n"))
	f.Add([]byte(entry+second), []byte(`{"seq":2,"state":"refused","code":"conflict","at":"2026-10-13T09:01:00Z"}`+"\n"+`{"seq":2,"state":"pending","at":"2026-10-13T09:02:00Z"}`+"\n"))
	f.Add([]byte(`{"next":41}`+"\n"+entry), []byte(`{"seq":7,"state":"dropped"}`+"\n"+`{"seq":1,"state":"nonsense"}`+"\n"))
	f.Add([]byte(entry+entry+`{"seq":3,"op":"handoff","args":{"id":"aries-1","note":"torn`), []byte(`{"seq":1,"state":"refu`))
	f.Add([]byte("not json\n[1]\n{}\nnull\n"+`{"seq":18446744073709551615,"op":"handoff","args":{},"idem":"k"}`+"\n"), []byte("\n\n{}\n"))
	f.Add([]byte(`{"next":18446744073709551615}`+"\n"+`{"seq":4611686018427387904,"op":"x","args":1,"idem":"k"}`+"\n"), []byte(`{"seq":-1}`))
	f.Fuzz(func(t *testing.T, box, answered []byte) {
		// No entry is too old, whatever time it gives.
		o, dir := open(t, outbox.Options{MaxAge: math.MaxInt64}, newClock())
		for name, data := range map[string][]byte{"outbox.ndjson": box, "answered.ndjson": answered} {
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		before, err := o.List()
		if err != nil {
			t.Fatalf("List() = %v", err)
		}
		e, err := o.Append(handoff(1))
		if errors.Is(err, outbox.ErrFull) {
			return // the files held a cap's worth, or said the numbers ran out
		}
		if err != nil {
			t.Fatalf("Append() = %v", err)
		}
		for _, old := range append(seqs(before.Pending), refusedSeqs(before.Refused)...) {
			if old == 0 || old >= e.Seq {
				t.Errorf("an entry numbered %d was listed, and the new entry took %d", old, e.Seq)
			}
		}
		var s server
		rep, err := o.Replay(t.Context(), s.send)
		if err != nil {
			t.Fatalf("Replay() = %v", err)
		}
		if want := len(before.Pending) + 1; rep.Delivered != want || rep.Pending != 0 || rep.Stopped != nil {
			t.Errorf("Replay() = %+v, want the %d pending entries delivered", rep, want)
		}
		if !slices.Contains(s.sent, e.Seq) {
			t.Errorf("Replay() sent %v, without the new entry %d", s.sent, e.Seq)
		}
		after, err := o.List()
		if err != nil || len(after.Pending) != 0 || len(after.Refused) != len(before.Refused) {
			t.Errorf("List() after the replay = %d pending, %d refused, %v, want 0 and the %d refused before",
				len(after.Pending), len(after.Refused), err, len(before.Refused))
		}
	})
}
