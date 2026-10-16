package e2e

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/ariesworx/aries/internal/outbox"
	"github.com/ariesworx/aries/internal/proto"
)

// The sessions of one principal share an outbox, and a replay sends what
// another session queues while it runs, then drops it from the outbox
// once answered. A cut session told it queued entry N reads N's key back
// from the outbox; without the principal's outbox lock a sibling's
// replay could deliver N first, leaving no key to read, which the
// partition check reported as a write "told it was queued, but there was
// no entry N in its outbox".
func TestOutboxKeyReadBeforeASiblingDelivers(t *testing.T) {
	t.Parallel()
	ob, err := outbox.Open(t.TempDir(), outbox.Options{})
	if err != nil {
		t.Fatal(err)
	}
	locks := &outboxLocks{}
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	// The sibling: replays without pause, each delivery under the lock,
	// as tracedConn sends a replayed request.
	wg.Go(func() {
		for ctx.Err() == nil {
			_, _ = ob.Replay(ctx, func(context.Context, outbox.Entry) (outbox.Outcome, error) {
				defer locks.forReplay("bob")()
				return outbox.Outcome{}, nil
			})
		}
	})
	defer wg.Wait()
	defer cancel()
	for i := range 300 {
		unlock := locks.forQueue("bob")
		e, err := ob.Append(outbox.Entry{Op: proto.OpComment,
			Args: fmt.Appendf(nil, `{"id":"aries-1","body":"note %d","idem":"k-%d"}`, i, i)})
		key := entryKeyIn(ob, e.Seq)
		unlock()
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf("k-%d", i); key != want {
			t.Fatalf("the key of entry %d, read back at once = %q, want %q: a sibling delivered it first", e.Seq, key, want)
		}
	}
}
