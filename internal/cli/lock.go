package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ariesworx/aries/internal/client"
	"github.com/ariesworx/aries/internal/proto"
)

const lockUsage = "lock acquire NAME [--capacity N] [--for DURATION] [--wait]" +
	" | lock renew NAME EPOCH [--for DURATION] | lock release NAME EPOCH | lock leave NAME | lock list [NAME]" +
	" | lock run NAME [--capacity N] [--for DURATION] [--wait] -- CMD [ARG]..."

// Lock timings (design §20.5). A place in a queue lapses two minutes after
// its last renewal, so a waiter renews it twice as often as it must. A
// hold is renewed at a third of its lease, and after a renewal that did
// not reach the server, a sixth.
const (
	placeRenewEvery = 30 * time.Second
	// stopGrace is how long `lock run` gives the command to stop after an
	// interrupt before it kills it.
	stopGrace = 10 * time.Second
)

// cmdLock takes, renews, releases and lists locks, and runs a command
// under one (design §20.5; decision 70). Nothing of it is queued while
// the server cannot be reached: a script fails before its critical
// section, not inside it.
func cmdLock(ctx context.Context, r *runner, args []string) error {
	if len(args) == 0 {
		return usagef(lockUsage, "lock needs acquire, renew, release, leave, list or run")
	}
	action, rest := args[0], args[1:]
	switch action {
	case "acquire":
		return lockAcquire(ctx, r, rest)
	case "renew":
		return lockRenew(ctx, r, rest)
	case "release":
		return lockRelease(ctx, r, rest)
	case "leave":
		return lockLeave(ctx, r, rest)
	case "list", "ls":
		return lockList(ctx, r, rest)
	case "run":
		return lockRun(ctx, r, rest)
	}
	return usagef(lockUsage, "unknown lock action %q", action)
}

// acquireFlags are the flags acquire and run share.
type acquireFlags struct {
	capacity int
	lease    string
	wait     bool
}

func (f *acquireFlags) register(fs interface {
	IntVar(*int, string, int, string)
	StringVar(*string, string, string, string)
	BoolVar(*bool, string, bool, string)
}) {
	fs.IntVar(&f.capacity, "capacity", 0, "how many may hold it at once, set by its first acquire (default 1)")
	fs.StringVar(&f.lease, "for", "", "the hold's lease, renewed while it is held (default 15m; 1m to 24h)")
	fs.BoolVar(&f.wait, "wait", false, "wait in its queue rather than fail when it is held")
}

// leaseOf is the lease f asks for, as a duration: the default when none.
func (f *acquireFlags) leaseOf() (time.Duration, error) {
	if f.lease == "" {
		return 15 * time.Minute, nil
	}
	d, err := proto.ParseDuration(f.lease)
	if err != nil {
		return 0, usagef(lockUsage, "--for %q: %v", f.lease, err)
	}
	return d, nil
}

func lockAcquire(ctx context.Context, r *runner, args []string) error {
	fs := r.newFlags("lock acquire")
	var f acquireFlags
	f.register(fs)
	pos, err := parse(fs, args, lockUsage)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef(lockUsage, "lock acquire takes one lock name")
	}
	if _, err := f.leaseOf(); err != nil {
		return err
	}
	held, err := r.takeLock(ctx, pos[0], f, f.wait)
	if err != nil {
		return err
	}
	if r.json {
		r.emit(held)
		return nil
	}
	_, _ = fmt.Fprintf(r.env.Stdout, "%d\n", held.Epoch)
	_, _ = fmt.Fprintf(r.env.Stderr, "aries: you hold %s at epoch %d until %s; renew it with `aries lock renew %s %d`, release it with `aries lock release %s %d`\n",
		esc(held.Name), held.Epoch, when(held.ExpiresAt), esc(held.Name), held.Epoch, esc(held.Name), held.Epoch)
	return nil
}

// takeLock acquires lock name as f asks. With f.wait it waits in the
// queue until granted, renewing its place, and gives the place up if ctx
// ends first. With watch, its connections watch the inbox, so that a
// grant is heard at once, and the item that told of it is then marked
// read.
func (r *runner) takeLock(ctx context.Context, name string, f acquireFlags, watch bool) (proto.LockResult, error) {
	granted := make(chan int64, 1)
	if watch {
		r.onPush = func(p proto.Push) {
			if p.Item != nil && p.Item.Kind == lockGranted {
				select {
				case granted <- p.Item.ID:
				default:
				}
			}
		}
	}
	var told int64
	defer func() {
		if told != 0 {
			_ = r.lockCall(ctx, proto.OpAck, proto.AckArgs{IDs: []int64{told}}, nil) // only tidies the inbox
		}
	}()
	in := proto.LockAcquireArgs{Name: name, Capacity: f.capacity, Lease: f.lease, Wait: f.wait}
	var out proto.LockResult
	if err := r.lockCall(ctx, proto.OpLockAcquire, in, &out); err != nil {
		return out, err
	}
	for !out.Held {
		if !r.json {
			_, _ = fmt.Fprintf(r.env.Stderr, "aries: waiting for %s, place %d in its queue\n", esc(name), out.Position)
		}
		select {
		case <-ctx.Done():
			r.giveUp(ctx, name)
			return out, proto.Errf(proto.CodeUnavailable, "run it again to wait again",
				fmt.Sprintf("interrupted while waiting for lock %s; its place was given up", name))
		case told = <-granted:
		case <-r.after(placeRenewEvery):
		}
		var next proto.LockResult
		err := r.lockCall(ctx, proto.OpLockRenew, proto.LockRenewArgs{Name: name}, &next)
		if pe, ok := errors.AsType[*proto.Error](err); ok && pe.Code == proto.CodeConflict {
			// The place lapsed, as it does after two minutes away from
			// the server: queue again, at the end.
			err = r.lockCall(ctx, proto.OpLockAcquire, in, &next)
		}
		switch {
		case err == nil:
			out = next
		case client.Unreachable(err) && ctx.Err() == nil:
			// Keep trying: the place lasts two minutes past the last
			// renewal, and is taken again when it lapsed meanwhile.
		default:
			r.giveUp(ctx, name)
			return out, err
		}
	}
	return out, nil
}

// lockGranted is the kind of inbox item that tells a waiter of its grant.
const lockGranted = "lock.granted"

// giveUp leaves lock name's queue, or releases the hold if the place was
// granted meanwhile, on a context of its own: ctx may have ended.
func (r *runner) giveUp(ctx context.Context, name string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	var left proto.LockLeaveResult
	if err := r.lockCall(cctx, proto.OpLockLeave, proto.LockLeaveArgs{Name: name}, &left); err != nil || left.Epoch == 0 {
		return
	}
	_ = r.lockCall(cctx, proto.OpLockRelease, proto.LockReleaseArgs{Name: name, Epoch: left.Epoch}, nil)
}

// lockCall is call for a command that holds or waits for a lock, which
// may outlive a connection: a connection that ended is dropped, so the
// next call dials again, and on a new one the command watches its inbox
// when it has something to hear (onPush), which also keeps the
// connection from idling out.
func (r *runner) lockCall(ctx context.Context, op string, args, result any) error {
	if r.conn != nil && r.conn.Err() != nil {
		_ = r.conn.Close()
		r.conn = nil
	}
	if r.conn == nil && r.onPush != nil {
		c, err := r.connect(ctx)
		if err != nil {
			return r.unqueued(op, err)
		}
		_ = c.Call(ctx, proto.OpWatch, proto.WatchArgs{}, nil) // only sooner news of a grant; renewals do without
	}
	return r.call(ctx, op, args, result)
}

// after is the clock lock waits on (Env.After).
func (r *runner) after(d time.Duration) <-chan time.Time { return r.env.After(d) }

// nameEpoch reads NAME EPOCH.
func nameEpoch(cmd string, pos []string) (string, int64, error) {
	if len(pos) != 2 {
		return "", 0, usagef(lockUsage, "lock %s takes a lock name and the epoch it is held at", cmd)
	}
	epoch, err := strconv.ParseInt(pos[1], 10, 64)
	if err != nil || epoch < 1 {
		return "", 0, usagef(lockUsage, "lock %s: the epoch %q is not a number from 1; `aries lock acquire` printed it", cmd, pos[1])
	}
	return pos[0], epoch, nil
}

func lockRenew(ctx context.Context, r *runner, args []string) error {
	fs := r.newFlags("lock renew")
	var lease string
	fs.StringVar(&lease, "for", "", "the lease from now (default: the one it was acquired for)")
	pos, err := parse(fs, args, lockUsage)
	if err != nil {
		return err
	}
	name, epoch, err := nameEpoch("renew", pos)
	if err != nil {
		return err
	}
	var out proto.LockResult
	if err := r.call(ctx, proto.OpLockRenew, proto.LockRenewArgs{Name: name, Epoch: epoch, Lease: lease}, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	_, _ = fmt.Fprintf(r.env.Stdout, "%s epoch %d until %s\n", esc(out.Name), out.Epoch, when(out.ExpiresAt))
	return nil
}

func lockRelease(ctx context.Context, r *runner, args []string) error {
	fs := r.newFlags("lock release")
	pos, err := parse(fs, args, lockUsage)
	if err != nil {
		return err
	}
	name, epoch, err := nameEpoch("release", pos)
	if err != nil {
		return err
	}
	var out proto.LockReleaseResult
	if err := r.call(ctx, proto.OpLockRelease, proto.LockReleaseArgs{Name: name, Epoch: epoch}, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	_, _ = fmt.Fprintf(r.env.Stdout, "released %s epoch %d\n", esc(out.Name), out.Epoch)
	return nil
}

func lockLeave(ctx context.Context, r *runner, args []string) error {
	fs := r.newFlags("lock leave")
	pos, err := parse(fs, args, lockUsage)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef(lockUsage, "lock leave takes one lock name")
	}
	var out proto.LockLeaveResult
	if err := r.call(ctx, proto.OpLockLeave, proto.LockLeaveArgs{Name: pos[0]}, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	switch {
	case out.Left:
		_, _ = fmt.Fprintf(r.env.Stdout, "left the queue of %s\n", esc(out.Name))
	case out.Epoch > 0:
		_, _ = fmt.Fprintf(r.env.Stdout, "you hold %s at epoch %d: release it with `aries lock release %s %d`\n", esc(out.Name), out.Epoch, esc(out.Name), out.Epoch)
	default:
		_, _ = fmt.Fprintf(r.env.Stdout, "you have no place in the queue of %s\n", esc(out.Name))
	}
	return nil
}

func lockList(ctx context.Context, r *runner, args []string) error {
	fs := r.newFlags("lock list")
	pos, err := parse(fs, args, lockUsage)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return usagef(lockUsage, "lock list takes at most one lock name")
	}
	var in proto.LocksArgs
	if len(pos) == 1 {
		in.Name = pos[0]
	}
	var out proto.LocksResult
	if err := r.call(ctx, proto.OpLocks, in, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	printLocks(r.env.Stdout, out.Locks)
	return nil
}

// printLocks prints a line for each lock: its name, how full it is, who
// holds it at which epoch until when, and who waits, in order.
func printLocks(w io.Writer, locks []proto.Lock) {
	if len(locks) == 0 {
		_, _ = fmt.Fprintln(w, "no locks held")
		return
	}
	var b bytes.Buffer
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, l := range locks {
		holders := make([]string, 0, len(l.Holders))
		for _, h := range l.Holders {
			holders = append(holders, fmt.Sprintf("%s/%s epoch %d until %s", esc(h.By), esc(h.Session), h.Epoch, when(h.ExpiresAt)))
		}
		line := fmt.Sprintf("%s\t%d/%d\t%s", esc(l.Name), len(l.Holders), l.Capacity, strings.Join(holders, ", "))
		if len(l.Waiters) > 0 {
			waiters := make([]string, 0, len(l.Waiters))
			for _, p := range l.Waiters {
				waiters = append(waiters, esc(p.By)+"/"+esc(p.Session))
			}
			line += "\twaiting: " + strings.Join(waiters, ", ")
		}
		_, _ = fmt.Fprintln(tw, line)
	}
	_ = tw.Flush()
	_, _ = w.Write(b.Bytes())
}

// lockRun runs a command under lock NAME: it takes the lock, waiting in
// its queue with --wait, runs the command with ARIES_LOCK_EPOCH set to
// the hold's epoch, renews the hold while the command runs, releases it
// when the command exits, and exits with the command's exit code.
//
// An interrupt (ctrl-c, SIGTERM) is passed to the command, which is
// killed if it has not stopped within stopGrace; the hold is released
// once it has, and aries exits 130 if the command died of it. Interrupted
// while waiting, it gives up its place. A hold that is lost, because the
// server says it ended (its lease ran out while the server could not be
// reached, or the principal lost access), stops the command the same
// way, and aries exits 1: what the lock guards is no longer guarded.
//
// aries killed outright releases nothing: the hold lasts until its lease
// runs out, at most --for after the last renewal, and the reaper then
// grants it to the next waiter. The command, if it outlives aries, runs on
// without the lock; a resource that checks the epoch refuses it once the
// lock is granted again.
//
// It takes a session of its own, so that two runs in one terminal are
// two holders, not one.
func lockRun(ctx context.Context, r *runner, args []string) error {
	var cmdline []string
	for i, a := range args {
		if a == "--" {
			args, cmdline = args[:i], args[i+1:]
			break
		}
	}
	fs := r.newFlags("lock run")
	var f acquireFlags
	f.register(fs)
	pos, err := parse(fs, args, lockUsage)
	if err != nil {
		return err
	}
	switch {
	case len(pos) != 1:
		return usagef(lockUsage, "lock run takes one lock name, then -- and the command")
	case len(cmdline) == 0:
		return usagef(lockUsage, "lock run needs the command to run, after --")
	case r.json:
		return usagef(lockUsage, "lock run prints what the command prints; --json is for the other lock commands")
	}
	lease, err := f.leaseOf()
	if err != nil {
		return err
	}
	name := pos[0]
	r.session = client.DefaultSession(r.env.Getenv) + ".lock-" + randomHex(4)
	held, err := r.takeLock(ctx, name, f, true)
	if err != nil {
		return err
	}
	cmd := exec.Command(cmdline[0], cmdline[1:]...) //nolint:gosec // the command is the person's, run as they asked
	cmd.Dir = r.dir
	cmd.Env = append(r.env.Environ(), fmt.Sprintf("%s=%d", proto.LockEpochEnv, held.Epoch))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = r.env.Stdin, r.rawOut, r.rawErr
	if err := cmd.Start(); err != nil {
		r.releaseLock(ctx, name, held.Epoch)
		return proto.Errf(proto.CodeInvalid, "check the command's name and that it is on PATH",
			fmt.Sprintf("start %s: %v", cmdline[0], err))
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	code, lost := r.holdWhile(ctx, name, held.Epoch, lease, cmd, done)
	if !lost {
		r.releaseLock(ctx, name, held.Epoch)
	}
	if code != 0 {
		return exitError{code: code}
	}
	return nil
}

// holdWhile renews the hold of name at epoch for lease until the command
// ends, and returns its exit code. When ctx ends, or the hold is lost, it
// stops the command; lost reports the latter, and the code is then 1.
func (r *runner) holdWhile(ctx context.Context, name string, epoch int64, lease time.Duration, cmd *exec.Cmd, done <-chan error) (code int, lost bool) {
	every := lease / 3
	for {
		select {
		case err := <-done:
			return exitCodeOf(err, false), false
		case <-ctx.Done():
			return exitCodeOf(r.stop(cmd, done), true), false
		case <-r.after(every):
		}
		var out proto.LockResult
		err := r.lockCall(ctx, proto.OpLockRenew, proto.LockRenewArgs{Name: name, Epoch: epoch}, &out)
		switch pe, ok := errors.AsType[*proto.Error](err); {
		case err == nil:
			every = lease / 3
		case ok && pe.Code == proto.CodeConflict:
			_, _ = fmt.Fprintf(r.env.Stderr, "aries: lost lock %s: %s; stopping the command\n", esc(name), esc(pe.Message))
			_ = r.stop(cmd, done)
			return 1, true
		case ctx.Err() == nil:
			// Not reached, or refused for another reason: try again
			// sooner, while the lease lasts. The server ends the hold if
			// the lease runs out first, and the next renewal hears so.
			every = lease / 6
		}
	}
}

// stop asks the command to stop, kills it if it has not within stopGrace,
// and returns how it ended.
func (r *runner) stop(cmd *exec.Cmd, done <-chan error) error {
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		_ = cmd.Process.Kill() // Windows has no interrupt to send
	}
	select {
	case err := <-done:
		return err
	case <-r.after(stopGrace):
		_ = cmd.Process.Kill()
		return <-done
	}
}

// exitCodeOf is the exit code of a command that ended with err: its own,
// or 130 for one an interrupt ended, or 1 for one that did not exit by
// itself otherwise.
func exitCodeOf(err error, interrupted bool) int {
	if err == nil {
		return 0
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		if c := ee.ExitCode(); c >= 0 {
			return c
		}
	}
	if interrupted {
		return 130
	}
	return 1
}

// releaseLock releases the hold of name at epoch, on a context of its own
// since ctx may have ended, and says so if it could not.
func (r *runner) releaseLock(ctx context.Context, name string, epoch int64) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	err := r.lockCall(cctx, proto.OpLockRelease, proto.LockReleaseArgs{Name: name, Epoch: epoch}, nil)
	if err == nil {
		return
	}
	msg := err.Error()
	if pe, ok := errors.AsType[*proto.Error](err); ok {
		msg = pe.Message
	}
	_, _ = fmt.Fprintf(r.env.Stderr, "aries: could not release lock %s at epoch %d: %s; it ends with its lease\n", esc(name), epoch, esc(msg))
}

// exitError is a command's exit code, which Run returns as its own and
// prints nothing for.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// randomHex returns n random bytes in hex.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return hex.EncodeToString(b)
}
