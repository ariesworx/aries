package e2e

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// lockedWriter is a buffer a command's output and a test's reads may
// share.
type lockedWriter struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *lockedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// lockEvents lists the ops of the events on lock name in the world's
// database, oldest first, space-separated.
func (w *world) lockEvents(t *testing.T, name string) string {
	t.Helper()
	evs, err := readEvents(t.Context(), openCheckerDB(t, w.dsn), 0)
	if err != nil {
		t.Fatal(err)
	}
	var ops []string
	for _, e := range evs {
		if e.target == "lock:"+name {
			ops = append(ops, e.op)
		}
	}
	return strings.Join(ops, " ")
}

// locksOf is what `aries lock list --json` says.
func locksOf(u *user) []proto.Lock {
	u.w.t.Helper()
	return decode[proto.LocksResult](u.w.t, u.ok("lock", "list", "--json")).Locks
}

// The lock commands, through the CLI: an acquire prints the epoch, a
// second is refused with the holder named and the way to wait, a stale
// release is refused, the list shows the holder, and a release frees it.
func TestLockCommands(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	if got := strings.TrimSpace(alice.ok("lock", "acquire", "deploy/prod", "--for", "30m")); got != "1" {
		t.Fatalf("lock acquire printed %q, want the epoch 1", got)
	}
	r := bob.run("v0.2.0", "lock", "acquire", "deploy/prod")
	if r.code != 1 || !strings.Contains(r.stderr, "held by alice") || !strings.Contains(r.stderr, "--wait") {
		t.Errorf("a second acquire = exit %d, %q; want 1, naming alice and --wait", r.code, r.stderr)
	}
	if ls := locksOf(bob); len(ls) != 1 || ls[0].Name != "deploy/prod" || len(ls[0].Holders) != 1 || ls[0].Holders[0].By != "alice" {
		t.Errorf("lock list = %+v, want deploy/prod held by alice", ls)
	}
	if r := alice.run("v0.2.0", "lock", "release", "deploy/prod", "2"); r.code != 1 || !strings.Contains(r.stderr, "epoch 2") {
		t.Errorf("a release of epoch 2 = exit %d, %q; want 1, naming the epoch", r.code, r.stderr)
	}
	alice.ok("lock", "release", "deploy/prod", "1")
	if ls := locksOf(bob); len(ls) != 0 {
		t.Errorf("lock list after the release = %+v, want none", ls)
	}
	if r := bob.run("v0.2.0", "lock", "acquire", "Deploy"); r.code != 1 || !strings.Contains(r.stderr, "lowercase") {
		t.Errorf("an acquire of a bad name = exit %d, %q; want 1 with the rule", r.code, r.stderr)
	}
}

// aries lock refuses at once when the server cannot be reached, and queues
// nothing: a script fails before its critical section.
func TestLockOffline(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{})
	alice := w.newUser("alice", "")
	w.stop()
	for _, args := range [][]string{
		{"lock", "acquire", "deploy"},
		{"lock", "run", "deploy", "--", "true"},
		{"lock", "release", "deploy", "1"},
	} {
		r := alice.run("v0.2.0", args...)
		if r.code != 1 || !strings.Contains(r.stderr, "not queued") {
			t.Errorf("aries %s offline = exit %d, %q; want 1, saying it was not queued", strings.Join(args, " "), r.code, r.stderr)
		}
	}
	if r := alice.run("v0.2.0", "lock", "list"); r.code != 1 || !strings.Contains(r.stderr, "not answered from this machine's copy") {
		t.Errorf("aries lock list offline = exit %d, %q; want 1, saying it is not answered from the cache", r.code, r.stderr)
	}
	if r := alice.run("v0.2.0", "outbox", "--json"); !strings.Contains(r.stdout, `"pending":[]`) && !strings.Contains(r.stdout, `"entries":[]`) &&
		strings.Contains(r.stdout, "lock") {
		t.Errorf("the outbox after offline lock commands = %s, want nothing queued", r.stdout)
	}
}

// aries lock run runs the command under the lock, with the epoch in its
// environment, passes its exit code through, and releases the lock when
// it exits.
func TestLockRunExitCode(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{})
	alice := w.newUser("alice", "")
	for _, tc := range []struct {
		script string
		code   int
	}{
		{`test "$ARIES_LOCK_EPOCH" = 1 && echo held`, 0},
		{`test "$ARIES_LOCK_EPOCH" = 2 && exit 7`, 7},
		{`exit 3`, 3},
	} {
		r := alice.run("v0.2.0", "lock", "run", "deploy", "--", "sh", "-c", tc.script)
		if r.code != tc.code {
			t.Errorf("lock run -- sh -c %q = exit %d (%s%s), want %d", tc.script, r.code, r.stdout, r.stderr, tc.code)
		}
		if ls := locksOf(alice); len(ls) != 0 {
			t.Errorf("after lock run -- sh -c %q the locks are %+v, want none: released on exit", tc.script, ls)
		}
	}
	if r := alice.run("v0.2.0", "lock", "run", "deploy", "--", "no-such-command-zq9x"); r.code != 1 || !strings.Contains(r.stderr, "no-such-command-zq9x") {
		t.Errorf("lock run of a command that is not there = exit %d, %q; want 1 naming it", r.code, r.stderr)
	}
	if ls := locksOf(alice); len(ls) != 0 {
		t.Errorf("after a command that did not start the locks are %+v, want none", ls)
	}
}

// An interrupt reaches the command, and the lock is released once it
// has stopped; aries exits 130, as the command died of it.
func TestLockRunInterrupt(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	ctx, interrupt := context.WithCancel(context.Background())
	defer interrupt()
	var out, errb lockedWriter
	code := make(chan int, 1)
	go func() {
		code <- alice.runTo(ctx, &out, &errb, "v0.2.0", "lock", "run", "deploy", "--", "sh", "-c", "echo started; exec sleep 30")
	}()
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(out.String(), "started") {
		if time.Now().After(deadline) {
			t.Fatalf("the command did not start: %q %q", out.String(), errb.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ls := locksOf(bob); len(ls) != 1 || ls[0].Holders[0].By != "alice" || !strings.HasPrefix(ls[0].Holders[0].Session, "cli.lock-") {
		t.Errorf("locks while the command runs = %+v, want deploy held by alice in a session of the run's own", ls)
	}
	interrupt()
	select {
	case c := <-code:
		if c != 130 {
			t.Errorf("lock run interrupted = exit %d (%q), want 130", c, errb.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("lock run did not return after the interrupt")
	}
	if ls := locksOf(bob); len(ls) != 0 {
		t.Errorf("after the interrupt the locks are %+v, want none: released", ls)
	}
}

// Two runs contend for a lock: the second waits in the queue, keeping
// its place, while the first holds the lock past its lease by renewing
// it; the second runs once the first exits, at the next epoch. The
// store's clock runs 60 times faster than real time, and so do the
// CLI's waits.
func TestLockRunWaitsAndRenews(t *testing.T) {
	t.Parallel()
	const speed = 60
	clk := &vclock{base: time.Now().UTC().Truncate(time.Second), start: time.Now(), speed: speed}
	w := newWorld(t, daemonOpts{now: clk.now, clock: clk.read, reap: 50 * time.Millisecond})
	fast := func(d time.Duration) <-chan time.Time { return time.After(d / speed) }
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	alice.after, bob.after = fast, fast

	// alice holds the lock for 3 seconds, three minutes on the store's
	// clock, under a lease of one: the renewals keep it hers.
	first := make(chan result, 1)
	go func() {
		first <- alice.run("v0.2.0", "lock", "run", "deploy", "--for", "1m", "--", "sh", "-c", "sleep 3; echo first $ARIES_LOCK_EPOCH")
	}()
	deadline := time.Now().Add(20 * time.Second)
	for len(locksOf(bob)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("alice's run never took the lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	second := bob.run("v0.2.0", "lock", "run", "deploy", "--wait", "--", "sh", "-c", "echo second $ARIES_LOCK_EPOCH")
	a := <-first
	if a.code != 0 || strings.TrimSpace(a.stdout) != "first 1" {
		t.Errorf("alice's run = exit %d, %q %q; want 0 and first 1", a.code, a.stdout, a.stderr)
	}
	if second.code != 0 || strings.TrimSpace(second.stdout) != "second 2" || !strings.Contains(second.stderr, "waiting for deploy") {
		t.Errorf("bob's run = exit %d, %q %q; want 0 and second 2, after waiting", second.code, second.stdout, second.stderr)
	}
	evs := w.lockEvents(t, "deploy")
	if want := "lock.acquire lock.wait lock.release lock.grant lock.release"; evs != want {
		t.Errorf("events on lock:deploy = %s, want %s: no hold lapsed, and bob was granted alice's", evs, want)
	}
}
