package outbox

// Tests of the locking rules with real processes: the test binary starts
// itself again, as internal/iaptest does, several times over on one
// directory. Nothing here waits on a clock. A parent and its children
// take turns through pipes: a child prints a line when it has reached a
// point, and reads a line when it must not go on until told. A child that
// must die at a known point blocks there until the parent kills it, and
// the parent has reaped it, and so knows its locks are gone, before it
// looks at what was left.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ariesworx/starfix/internal/proto"
)

// A child is told what to be through its environment.
const (
	childEnv = "STARFIX_OUTBOX_TEST_CHILD" // the mode; empty in a test binary that is not a child
	dirEnv   = "STARFIX_OUTBOX_TEST_DIR"   // the outbox's directory
	idEnv    = "STARFIX_OUTBOX_TEST_ID"    // which child this is
	nEnv     = "STARFIX_OUTBOX_TEST_N"     // how many entries, or which one
	logEnv   = "STARFIX_OUTBOX_TEST_LOG"   // the stand-in server's log of what it was sent
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(childEnv); mode != "" {
		os.Exit(childMain(mode))
	}
	os.Exit(m.Run())
}

// childMain runs one child and returns its exit code. It uses the
// process's own streams, which internal code otherwise never touches,
// because they are how its parent follows it.
func childMain(mode string) int {
	in := bufio.NewReader(os.Stdin)
	if err := runChild(mode, in); err != nil {
		fmt.Fprintf(os.Stderr, "child %s: %v\n", mode, err)
		return 1
	}
	return 0
}

// say tells the parent the child has reached a point.
func say(format string, args ...any) { _, _ = fmt.Fprintf(os.Stdout, format+"\n", args...) }

// hear waits for the parent's word to go on. It fails when the parent is
// gone, so no child outlives its test.
func hear(in *bufio.Reader) error {
	if _, err := in.ReadString('\n'); err != nil {
		return fmt.Errorf("the parent went away: %w", err)
	}
	return nil
}

func childKey(id, i int) string { return fmt.Sprintf("p%d-%d", id, i) }

func childEntry(id, i int) Entry {
	return Entry{
		Op:      proto.OpHandoff,
		Args:    fmt.Appendf(nil, `{"id":"sfx-1","note":"%s","idem":"%s"}`, strings.Repeat("handoff text ", 40), childKey(id, i)),
		Session: "s-" + strconv.Itoa(id),
	}
}

// logSend is the stand-in server: it notes each entry it is sent, in one
// write, in the file every replayer of a test shares.
func logSend(path string, e Entry) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600) //nolint:gosec // the test's own file
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(f, "%d\n", e.Seq)
	return errors.Join(err, f.Close())
}

func runChild(mode string, in *bufio.Reader) error {
	dir := os.Getenv(dirEnv)
	id, _ := strconv.Atoi(os.Getenv(idEnv))
	n, _ := strconv.Atoi(os.Getenv(nEnv))
	o, err := Open(dir, Options{})
	if err != nil {
		return err
	}
	switch mode {
	case "append":
		// Appends n entries once told to, saying so after each, and
		// stops halfway until told again.
		if err := hear(in); err != nil {
			return err
		}
		for i := range n {
			if i == n/2 {
				say("half")
				if err := hear(in); err != nil {
					return err
				}
			}
			if _, err := o.Append(childEntry(id, i)); err != nil {
				return err
			}
			say("appended")
		}
		return nil

	case "torn":
		// Dies as a writer killed between two writes of one entry
		// would: holding the append lock, with the first half of a line
		// in the file.
		lk, err := acquire(o.path(appendLockFile), true)
		if err != nil {
			return err
		}
		defer func() { _ = lk.release() }()
		f, err := os.OpenFile(o.path(outboxFile), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600) //nolint:gosec // the test's own file
		if err != nil {
			return err
		}
		if _, err := f.WriteString(`{"seq":3,"op":"handoff","args":{"id":"sfx-1","note":"the first half of`); err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
		say("torn")
		return hear(in) // until killed

	case "replay-hold":
		// Replays, and stops inside its first delivery until told to go
		// on.
		held := false
		rep, err := o.Replay(context.Background(), func(context.Context, Entry) (Outcome, error) {
			if !held {
				held = true
				say("sending")
				if err := hear(in); err != nil {
					return Outcome{}, err
				}
			}
			return Outcome{}, nil
		})
		if err != nil {
			return err
		}
		say("done %d", rep.Delivered)
		return nil

	case "replay-try":
		// Tries to replay once and says what came of it.
		sent := 0
		rep, err := o.Replay(context.Background(), func(context.Context, Entry) (Outcome, error) {
			sent++
			return Outcome{}, nil
		})
		if err != nil {
			return err
		}
		say("busy=%t sent=%d", rep.Busy, sent)
		return nil

	case "replay-die":
		// Replays to the shared log, and stops after the server has the
		// n-th entry and before its answer is noted, until killed.
		sent := 0
		_, err := o.Replay(context.Background(), func(_ context.Context, e Entry) (Outcome, error) {
			if err := logSend(os.Getenv(logEnv), e); err != nil {
				return Outcome{}, err
			}
			if sent++; sent == n {
				say("mid")
				return Outcome{}, hear(in) // until killed
			}
			return Outcome{}, nil
		})
		return err
	}
	return fmt.Errorf("unknown mode %q", mode)
}

// child is a running child and the pipes to it.
type child struct {
	t      *testing.T
	cmd    *exec.Cmd
	in     *os.File
	out    *bufio.Reader
	stderr *bytes.Buffer
	waited bool
}

// startChild starts the test binary again as a child in mode on dir. The
// child is killed when the test ends, if it is still there.
func startChild(t *testing.T, mode, dir string, env ...string) *child {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("the test binary cannot find itself to start again: %v", err)
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	c := &child{t: t, in: inW, out: bufio.NewReader(outR), stderr: &bytes.Buffer{}}
	c.cmd = exec.Command(exe) //nolint:gosec // the test binary itself
	// Under -race a process waits a second as it exits, for reports
	// still being written; a child has one goroutine, and a race it did
	// find still fails it.
	c.cmd.Env = append(os.Environ(), append(env, childEnv+"="+mode, dirEnv+"="+dir, "GORACE=atexit_sleep_ms=0")...)
	c.cmd.Stdin, c.cmd.Stdout, c.cmd.Stderr = inR, outW, c.stderr
	if err := c.cmd.Start(); err != nil {
		t.Skipf("this system cannot start the test binary again: %v", err)
	}
	// The child has its ends now. Closing ours of them is what makes a
	// read of out end when the child does.
	_ = inR.Close()
	_ = outW.Close()
	t.Cleanup(func() {
		if !c.waited {
			_ = c.cmd.Process.Kill()
			_ = c.cmd.Wait()
		}
		_ = inW.Close()
		_ = outR.Close()
	})
	return c
}

// line reads the child's next line, and "" once the child is gone.
func (c *child) line() string {
	s, _ := c.out.ReadString('\n')
	return strings.TrimSuffix(s, "\n")
}

// expect waits for the child to say want.
func (c *child) expect(want string) {
	c.t.Helper()
	if got := c.line(); got != want {
		c.kill() // its stderr is whole only once it is gone
		c.t.Fatalf("the child said %q, want %q; its stderr:\n%s", got, want, c.stderr)
	}
}

// tell lets the child go on.
func (c *child) tell() {
	c.t.Helper()
	if _, err := c.in.WriteString("go\n"); err != nil {
		c.t.Fatalf("telling the child to go on: %v", err)
	}
}

// exit waits for the child to end by itself, which it must do without
// error.
func (c *child) exit() {
	c.t.Helper()
	c.waited = true
	if err := c.cmd.Wait(); err != nil {
		c.t.Fatalf("the child ended with %v; its stderr:\n%s", err, c.stderr)
	}
}

// kill ends the child where it stands, and returns once the system has
// reaped it: its descriptors are closed, and its locks with them.
func (c *child) kill() {
	c.t.Helper()
	c.waited = true
	_ = c.cmd.Process.Kill() // an error means it had ended by itself
	_ = c.cmd.Wait()         // "signal: killed"
}

func openTest(t *testing.T) *Outbox {
	t.Helper()
	o, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	return o
}

func env(name string, v int) string { return name + "=" + strconv.Itoa(v) }

// rawLines returns the outbox file's lines, and whether it ends in a
// newline.
func rawLines(t *testing.T, o *Outbox) (lines [][]byte, whole bool) {
	t.Helper()
	b, err := os.ReadFile(o.path(outboxFile))
	if err != nil {
		t.Fatal(err)
	}
	whole = bytes.HasSuffix(b, []byte("\n"))
	return bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n")), whole
}

func TestProcessesAppendTogether(t *testing.T) {
	const procs, each = 10, 10
	o := openTest(t)
	children := make([]*child, procs)
	for id := range children {
		children[id] = startChild(t, "append", o.dir, env(idEnv, id), env(nEnv, each))
	}
	// All of them are running before any is told to begin.
	for _, c := range children {
		c.tell()
	}
	var wg sync.WaitGroup
	for _, c := range children {
		wg.Go(func() {
			for line := c.line(); line != ""; line = c.line() {
				if line == "half" {
					c.tell()
				}
			}
		})
	}
	wg.Wait()
	for _, c := range children {
		c.exit()
	}

	// Nothing interleaved: every line of the file is one whole entry.
	lines, whole := rawLines(t, o)
	if !whole || len(lines) != procs*each {
		t.Errorf("the outbox has %d lines (ending in a newline: %t), want %d whole ones", len(lines), whole, procs*each)
	}
	for i, line := range lines {
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil || e.Seq != uint64(i+1) {
			t.Fatalf("line %d of the outbox is not entry %d: %v\n%s", i+1, i+1, err, line)
		}
	}
	// Nothing lost: every append that returned is there once, and each
	// process's entries are in the order it wrote them.
	l, err := o.List()
	if err != nil {
		t.Fatalf("List() = %v", err)
	}
	if len(l.Pending) != procs*each {
		t.Fatalf("List() has %d pending, want %d", len(l.Pending), procs*each)
	}
	next := make(map[string]int)
	for _, e := range l.Pending {
		if want := childKey(sessionID(t, e), next[e.Session]); e.Idem != want {
			t.Errorf("entry %d of %s has key %s, want %s: an entry is missing, doubled or out of order", e.Seq, e.Session, e.Idem, want)
		}
		next[e.Session]++
	}
	if len(next) != procs {
		t.Errorf("entries of %d processes, want %d", len(next), procs)
	}
}

func sessionID(t *testing.T, e Entry) int {
	t.Helper()
	id, err := strconv.Atoi(strings.TrimPrefix(e.Session, "s-"))
	if err != nil {
		t.Fatalf("entry %d has session %q", e.Seq, e.Session)
	}
	return id
}

func TestWriterKilledMidAppend(t *testing.T) {
	o := openTest(t)
	for i := range 2 {
		if _, err := o.Append(childEntry(0, i)); err != nil {
			t.Fatalf("Append() = %v", err)
		}
	}
	c := startChild(t, "torn", o.dir)
	c.expect("torn")
	c.kill()

	// The next reader takes the whole lines and not the fragment.
	l, err := o.List()
	if err != nil {
		t.Fatalf("List() after a writer died mid-line = %v", err)
	}
	if got := entrySeqs(l.Pending); !slices.Equal(got, []uint64{1, 2}) {
		t.Errorf("List() after a writer died mid-line = entries %v, want [1 2]", got)
	}
	// The next writer is not held back by the dead one's lock, and ends
	// the fragment before it writes, so its own entry is a line.
	e, err := o.Append(childEntry(0, 2))
	if err != nil {
		t.Fatalf("Append() after a writer died mid-line = %v", err)
	}
	if e.Seq != 3 {
		t.Errorf("Append() after a writer died mid-line took seq %d, want 3", e.Seq)
	}
	lines, whole := rawLines(t, o)
	if !whole || len(lines) != 4 || json.Valid(lines[2]) || !json.Valid(lines[3]) {
		t.Errorf("the outbox has %d lines (ending in a newline: %t), want 4: two entries, the fragment alone on the third, and the new entry", len(lines), whole)
	}
	var sent []uint64
	rep, err := o.Replay(t.Context(), func(_ context.Context, e Entry) (Outcome, error) {
		sent = append(sent, e.Seq)
		return Outcome{}, nil
	})
	if err != nil || rep.Delivered != 3 || !slices.Equal(sent, []uint64{1, 2, 3}) {
		t.Errorf("Replay() = %+v, %v, sent %v, want [1 2 3] delivered", rep, err, sent)
	}
}

func entrySeqs(es []Entry) []uint64 {
	out := make([]uint64, len(es))
	for i, e := range es {
		out[i] = e.Seq
	}
	return out
}

func TestOneProcessReplaysAtATime(t *testing.T) {
	o := openTest(t)
	for i := range 3 {
		if _, err := o.Append(childEntry(0, i)); err != nil {
			t.Fatalf("Append() = %v", err)
		}
	}
	holder := startChild(t, "replay-hold", o.dir)
	holder.expect("sending")

	// While it is inside a delivery, another process is turned away at
	// once, and so is this one.
	other := startChild(t, "replay-try", o.dir)
	other.expect("busy=true sent=0")
	other.exit()
	rep, err := o.Replay(t.Context(), func(context.Context, Entry) (Outcome, error) {
		t.Error("Replay() sent an entry while another process was replaying")
		return Outcome{}, nil
	})
	if err != nil || !rep.Busy {
		t.Errorf("Replay() while another process replays = %+v, %v, want Busy", rep, err)
	}
	if err := o.Drop(1); !errors.Is(err, ErrBusy) {
		t.Errorf("Drop(1) while another process replays = %v, want ErrBusy", err)
	}
	// Appending is not held back by a replay.
	if _, err := o.Append(childEntry(0, 3)); err != nil {
		t.Errorf("Append() while another process replays = %v", err)
	}

	holder.tell()
	holder.expect("done 4")
	holder.exit()
	rep, err = o.Replay(t.Context(), func(context.Context, Entry) (Outcome, error) {
		t.Error("Replay() sent an entry the other process had delivered")
		return Outcome{}, nil
	})
	if err != nil || rep.Busy || rep.Pending != 0 {
		t.Errorf("Replay() after the other process finished = %+v, %v, want nothing to do", rep, err)
	}
}

func TestReplayerKilledMidReplay(t *testing.T) {
	const entries, dieAt = 6, 3
	o := openTest(t)
	for i := range entries {
		if _, err := o.Append(childEntry(0, i)); err != nil {
			t.Fatalf("Append() = %v", err)
		}
	}
	log := o.dir + "-server.log"
	c := startChild(t, "replay-die", o.dir, env(nEnv, dieAt), logEnv+"="+log)
	c.expect("mid")
	c.kill()

	// The dead replayer's lock does not hold the next one back.
	rep, err := o.Replay(t.Context(), func(_ context.Context, e Entry) (Outcome, error) {
		return Outcome{}, logSend(log, e)
	})
	if err != nil || rep.Busy || rep.Pending != 0 {
		t.Fatalf("Replay() after a replayer was killed = %+v, %v, want it to run to the end", rep, err)
	}
	b, err := os.ReadFile(log) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	var got []uint64
	for s := range strings.FieldsSeq(string(b)) {
		seq, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			t.Fatalf("the server's log has %q", s)
		}
		got = append(got, seq)
	}
	// At-least-once: the entry whose answer was never noted is sent
	// again, and nothing is skipped or sent out of order.
	if want := []uint64{1, 2, 3, 3, 4, 5, 6}; !slices.Equal(got, want) {
		t.Errorf("the server was sent %v, want %v: entry %d twice and every other once, in order", got, want, dieAt)
	}
}

func TestCompactionWhileProcessesAppend(t *testing.T) {
	const procs, each, every, batch = 8, 10, 7, 5
	o := openTest(t)
	delivered := make(map[string]int)
	compactions := 0
	// replay delivers batch entries at most and then loses the server,
	// so that each call ends, and compacts, with appenders still going,
	// instead of one call following them to the end.
	errGone := errors.New("the server went away")
	replay := func(limit int) Report {
		t.Helper()
		rep, err := o.Replay(t.Context(), func(_ context.Context, e Entry) (Outcome, error) {
			if limit--; limit < 0 {
				return Outcome{}, errGone
			}
			delivered[e.Idem]++
			return Outcome{}, nil
		})
		if err != nil || rep.Busy {
			t.Fatalf("Replay() = %+v, %v", rep, err)
		}
		if rep.Delivered > 0 {
			compactions++ // a replay that delivered anything compacts
		}
		return rep
	}

	children := make([]*child, procs)
	said := make(chan string)
	var wg sync.WaitGroup
	for id := range children {
		c := startChild(t, "append", o.dir, env(idEnv, id), env(nEnv, each))
		children[id] = c
		wg.Go(func() {
			for line := c.line(); line != ""; line = c.line() {
				select {
				case said <- line:
				case <-t.Context().Done(): // the test failed and nobody listens
					return
				}
			}
		})
	}
	go func() { wg.Wait(); close(said) }()
	for _, c := range children {
		c.tell()
	}

	// The appenders run freely, and this process replays, and so
	// compacts, after every few appends it hears of: it has then heard
	// of more appends than it has delivered, so each replay delivers
	// something. When all of them have stopped halfway it compacts once
	// more, so every one of them then appends to an outbox that was
	// replaced since it last wrote.
	appended, halves := 0, 0
	for line := range said {
		switch line {
		case "appended":
			if appended++; appended%every == 0 {
				replay(batch)
			}
		case "half":
			if halves++; halves == procs {
				replay(batch)
				for _, c := range children {
					c.tell()
				}
			}
		}
	}
	for _, c := range children {
		c.exit()
	}
	if rep := replay(procs * each); rep.Pending != 0 || rep.Stopped != nil {
		t.Errorf("the last Replay() = %+v, want nothing left", rep)
	}

	if appended != procs*each {
		t.Fatalf("the children reported %d appends, want %d", appended, procs*each)
	}
	if want := procs * each / every; compactions < want {
		t.Errorf("the outbox was compacted %d times while the appenders ran, want %d at least", compactions, want)
	}
	for id := range procs {
		for i := range each {
			if n := delivered[childKey(id, i)]; n != 1 {
				t.Errorf("entry %s was delivered %d times, want once", childKey(id, i), n)
			}
		}
	}
	if len(delivered) != procs*each {
		t.Errorf("%d different entries were delivered, want %d", len(delivered), procs*each)
	}
	l, err := o.List()
	if err != nil || len(l.Pending) != 0 || len(l.Refused) != 0 {
		t.Errorf("List() at the end = %d pending, %d refused, %v, want nothing left", len(l.Pending), len(l.Refused), err)
	}
}
