package outbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// CodeStale is the code of a [Refusal] this package made itself: the
// entry waited longer than MaxAge, and was not sent. It is the code a
// server gives a key older than its window, too (proto.CodeStale),
// which a replayed entry may also be refused with.
const CodeStale = string(proto.CodeStale)

// maxText bounds the code, the message and the fix kept of a refusal, in
// bytes each.
const maxText = 1024

// Outcome is the server's answer to a replayed entry. The zero Outcome
// is an entry the server applied.
type Outcome struct {
	// Refused says the server answered with an error, which Code,
	// Message and Fix are.
	Refused bool
	Code    string
	Message string
	Fix     string
	// Parked says the server applied the entry in part and parked the
	// rest as conflicts for a person to resolve (design §18.7); Kept,
	// that it applied none of it, because the issue had moved on while it
	// waited, and kept it as a comment (§18.6, §18.7). Either is delivered
	// all the same: the server has it.
	Parked, Kept bool
}

// Report is what one call of Replay did.
type Report struct {
	// Busy says another replay was going on, in this process or another,
	// and this call did nothing.
	Busy bool
	// Delivered, Refused and Stale count the entries that left the
	// pending set.
	Delivered, Refused, Stale int
	// Parked counts the delivered entries that parked conflicts, and
	// Kept those kept as a comment.
	Parked, Kept int
	// Pending counts the entries still waiting to be sent.
	Pending int
	// Stopped is why replay ended with entries still pending: the error
	// send returned, or the context's. It is nil when nothing was left.
	Stopped error
}

// Replay delivers the pending entries to send, in order and one at a
// time, and notes each answer.
//
// send returns the server's answer, or an error when the server was not
// reached. An entry leaves the pending set once the server has answered
// it, whatever the answer: a refusal goes to the refused list, which is
// never sent again by itself. An error from send stops the replay with
// that entry still pending, and the next replay begins with it, with the
// same key. An entry that has waited longer than MaxAge is not sent: it
// goes to the refused list with [CodeStale].
//
// Delivery is at-least-once. A replayer that dies after the server has
// an entry and before the answer is noted leaves it to be sent again,
// which its idempotency key makes harmless.
//
// One replay runs at a time on a directory. When another is running
// Replay returns at once with Report.Busy. It never waits long itself:
// the whole call ends within ReplayBudget and each send within
// SendTimeout, by the deadline of the context send is given, and by
// ctx's.
//
// The error is a failure of the local files; the Report is right as far
// as the replay got. An unreachable server is Report.Stopped, not an
// error.
func (o *Outbox) Replay(ctx context.Context, send func(context.Context, Entry) (Outcome, error)) (Report, error) {
	// No lease: the kernel frees the lock when its holder dies, and a
	// lease that can be taken over is what lets two replayers run at
	// once (spike/outbox/RESULTS.md, sections 1 and 4).
	rl, err := acquire(o.path(replayLockFile), false)
	if err != nil {
		return Report{}, fmt.Errorf("replay outbox: %w", err)
	}
	if rl == nil {
		return Report{Busy: true}, nil
	}
	defer func() { _ = rl.release() }()

	ctx, cancel := context.WithTimeout(ctx, o.opts.ReplayBudget)
	defer cancel()

	var (
		rep    Report
		counts []func(*Stats)
	)
	// Each round takes the entries pending now, so what other sessions
	// queue while this one replays goes out in the same call.
rounds:
	for {
		var v *view
		if err := o.locked(func() (err error) { v, err = o.load(); return err }); err != nil {
			return rep, fmt.Errorf("replay outbox: %w", err)
		}
		sent := 0
		for _, e := range v.entries {
			if !v.pending(e.Seq) {
				continue
			}
			if err := ctx.Err(); err != nil {
				rep.Stopped = err
				break rounds
			}
			sent++
			queued := e.At
			if a, again := v.state[e.Seq]; again {
				queued = a.At // sent again by hand: it waits anew from then
			}
			if waited := o.now().Sub(queued); waited > o.opts.MaxAge {
				err := o.record(answer{
					Seq: e.Seq, State: stateRefused, Code: CodeStale, At: o.now(),
					Message: fmt.Sprintf("it waited %s, and an entry is sent for %s", waited.Round(time.Second), o.opts.MaxAge),
					Fix:     "retry it if it is still wanted, or drop it",
				})
				if err != nil {
					return rep, fmt.Errorf("replay outbox: %w", err)
				}
				rep.Stale++
				counts = append(counts, func(s *Stats) { s.stale(e.Op) })
				continue
			}

			sctx, stop := context.WithTimeout(ctx, o.opts.SendTimeout)
			out, err := send(sctx, e)
			stop()
			if err != nil {
				rep.Stopped = err
				break rounds
			}
			a := answer{Seq: e.Seq, State: stateDelivered, At: o.now()}
			if out.Refused {
				a.State, a.Code, a.Message, a.Fix = stateRefused, out.Code, out.Message, out.Fix
			}
			if err := o.record(a); err != nil {
				return rep, fmt.Errorf("replay outbox: %w", err)
			}
			if out.Refused {
				rep.Refused++
				counts = append(counts, func(s *Stats) { s.refused(e.Op, a.At) })
			} else {
				rep.Delivered++
				if out.Parked {
					rep.Parked++
				}
				if out.Kept {
					rep.Kept++
				}
				parked := out.Parked || out.Kept
				counts = append(counts, func(s *Stats) { s.delivered(e.Op, a.At, a.At.Sub(e.At), parked) })
			}
		}
		if sent == 0 {
			break
		}
	}

	rep.Pending, err = o.settle(counts)
	if err != nil {
		return rep, fmt.Errorf("replay outbox: %w", err)
	}
	return rep, nil
}

// RefuseAll moves every pending entry to the refused list with the
// server's answer out, sending none, and returns how many it moved. It
// is for an answer that holds for every entry alike: the server refused
// the connection they would be sent on, and will go on refusing it until
// a person acts (design §18.3, "Who it runs as"). An entry already
// refused keeps its own answer. Like a person's decision, it does not
// wait for a replay in progress, which was let in and decides for
// itself: it is [ErrBusy] then.
func (o *Outbox) RefuseAll(out Outcome) (int, error) {
	rl, err := acquire(o.path(replayLockFile), false)
	if err != nil {
		return 0, fmt.Errorf("refuse outbox: %w", err)
	}
	if rl == nil {
		return 0, fmt.Errorf("refuse outbox: %w: a replay is going on", ErrBusy)
	}
	defer func() { _ = rl.release() }()

	var v *view
	if err := o.locked(func() (err error) { v, err = o.load(); return err }); err != nil {
		return 0, fmt.Errorf("refuse outbox: %w", err)
	}
	var counts []func(*Stats)
	for _, e := range v.entries {
		if !v.pending(e.Seq) {
			continue
		}
		a := answer{Seq: e.Seq, State: stateRefused, Code: out.Code, Message: out.Message, Fix: out.Fix, At: o.now()}
		if err := o.record(a); err != nil {
			return len(counts), fmt.Errorf("refuse outbox: %w", err)
		}
		counts = append(counts, func(s *Stats) { s.refused(e.Op, a.At) })
	}
	if len(counts) == 0 {
		return 0, nil
	}
	if _, err := o.settle(counts); err != nil {
		return len(counts), fmt.Errorf("refuse outbox: %w", err)
	}
	return len(counts), nil
}

// Retry makes the refused entry seq pending again, to be sent by the
// next Replay with the key it always had. It waits anew from now, so a
// stale entry is sent. It is [ErrNotFound] when no refused entry has
// that number, and [ErrBusy] while a replay is going on.
func (o *Outbox) Retry(seq uint64) error {
	if err := o.change(seq, statePending); err != nil {
		return fmt.Errorf("retry entry %d: %w", seq, err)
	}
	return nil
}

// Drop removes the entry seq, pending or refused, for good. It is
// [ErrNotFound] when no entry that waits has that number, and [ErrBusy]
// while a replay is going on.
func (o *Outbox) Drop(seq uint64) error {
	if err := o.change(seq, stateDropped); err != nil {
		return fmt.Errorf("drop entry %d: %w", seq, err)
	}
	return nil
}

// change notes a person's decision on the entry seq. The answered file
// is written under the replay lock only, so a decision waits its turn as
// a replay does: it does not wait, and says the outbox is busy.
func (o *Outbox) change(seq uint64, state string) error {
	rl, err := acquire(o.path(replayLockFile), false)
	if err != nil {
		return err
	}
	if rl == nil {
		return fmt.Errorf("%w: a replay is going on", ErrBusy)
	}
	defer func() { _ = rl.release() }()

	var v *view
	if err := o.locked(func() (err error) { v, err = o.load(); return err }); err != nil {
		return err
	}
	var op string
	for _, e := range v.entries {
		if e.Seq == seq && (v.refused(seq) || state == stateDropped && v.pending(seq)) {
			op = e.Op
		}
	}
	if op == "" {
		if state == statePending {
			return fmt.Errorf("%w among the refused", ErrNotFound)
		}
		return ErrNotFound
	}
	if err := o.record(answer{Seq: seq, State: state, At: o.now()}); err != nil {
		return err
	}
	var counts []func(*Stats)
	if state == stateDropped {
		counts = append(counts, func(s *Stats) { s.dropped(op) })
	}
	_, err = o.settle(counts)
	return err
}

// record notes what became of an entry, in one write. The caller holds
// the replay lock. It is not synced: an answer lost with the power is an
// entry delivered again with its key, and a sync for each answer made
// replay 2.6 times as slow (spike/outbox/RESULTS.md, section 4).
func (o *Outbox) record(a answer) error {
	a.Code, a.Message, a.Fix = clip(a.Code), clip(a.Message), clip(a.Fix)
	line, err := json.Marshal(a)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	f, err := os.OpenFile(o.path(answeredFile), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600) //nolint:gosec // the outbox's own file
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err == nil && st.Size() > 0 {
		var last [1]byte
		if _, err = f.ReadAt(last[:], st.Size()-1); err == nil && last[0] != '\n' {
			line = append([]byte{'\n'}, line...) // end a line a dead replayer left
		}
	}
	if err == nil {
		_, err = f.Write(line)
	}
	return errors.Join(err, f.Close())
}

// clip cuts s to maxText bytes at most, between characters.
func clip(s string) string {
	if len(s) <= maxText {
		return s
	}
	end := 0
	for i := range s {
		if i > maxText {
			break
		}
		end = i
	}
	return s[:end]
}

// settle ends a replay or a person's decision: holding the replay lock,
// it takes the append lock, adds counts to the stats, and compacts. It
// returns how many entries are still pending.
func (o *Outbox) settle(counts []func(*Stats)) (pending int, err error) {
	err = o.locked(func() error {
		if len(counts) > 0 {
			o.count(func(s *Stats) {
				for _, c := range counts {
					c(s)
				}
			})
		}
		v, err := o.load()
		if err != nil {
			return err
		}
		for _, e := range v.entries {
			if v.pending(e.Seq) {
				pending++
			}
		}
		if err := o.compact(v); err != nil {
			return fmt.Errorf("compact: %w", err)
		}
		return nil
	})
	return pending, err
}

// compact rewrites the two files without the entries that are gone and
// without the answers that no longer say anything, when there are any.
// The caller holds the replay lock and the append lock, taken in that
// order. No appender has the outbox open, since each opens it under the
// append lock, and no reader has either file open, for the same reason;
// so each rename is over a file nothing has open.
//
// The outbox is replaced first. A death between the two renames then
// leaves answers whose entries are gone, which mean nothing, since a
// sequence number is never used again; the other order would leave
// entries whose answers are gone, all to be delivered again
// (spike/outbox/RESULTS.md, section 5). The first line of a compacted
// outbox keeps the lowest number not yet used, which the entries that
// were taken out would otherwise have told.
//
// If a rename fails, nothing is lost: the file it would have replaced is
// as it was, and the next replay tries again.
func (o *Outbox) compact(v *view) error {
	var out, answers bytes.Buffer
	fmt.Fprintf(&out, "{\"next\":%d}\n", v.next)
	kept, said := 0, 0
	for i, e := range v.entries {
		if !v.pending(e.Seq) && !v.refused(e.Seq) {
			continue
		}
		kept++
		out.Write(v.lines[i])
		out.WriteByte('\n')
		if a, ok := v.state[e.Seq]; ok {
			b, err := json.Marshal(a)
			if err != nil {
				return err
			}
			said++
			answers.Write(b)
			answers.WriteByte('\n')
		}
	}
	if kept == len(v.entries) && v.bad == 0 && !v.torn && said == v.answers {
		return nil // nothing to take out
	}
	if err := replace(o.path(outboxFile), out.Bytes(), true); err != nil {
		return err
	}
	if err := replace(o.path(answeredFile), answers.Bytes(), true); err != nil {
		return err
	}
	return syncDir(o.dir)
}

// replace writes data to a temporary file beside path and renames it
// over path. The caller holds the lock under which path is written, so
// the one temporary name is its alone, and one a dead holder left is
// written over.
func replace(path string, data []byte, sync bool) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // beside the outbox's own file
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil && sync {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err == nil {
		err = os.Rename(tmp, path) //nolint:gosec // as above
	}
	if err != nil {
		_ = os.Remove(tmp) //nolint:gosec // as above
	}
	return err
}
