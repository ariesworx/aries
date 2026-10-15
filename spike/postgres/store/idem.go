package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ariesworx/starfix/internal/lazyregexp"
)

// Idempotency (design §7, §8, §18.5): a write may carry a client key:
// create, finish, comment, handoff, remember, forget, hours.log and
// hours.delete, and since protocol 6 update, label.add, label.rm,
// dep.add, accept and usage, so that the outbox can queue them. The
// operation's last event records the key, a hash of the request and the
// result. The same principal sending the same key again within the
// store's window (Options.IdemWindow) gets that result back and nothing
// is written; the same key with a different request, or after the
// window, is refused with an *IdemError. Keys are never deleted: the
// unique index on (principal, idem_key) keeps an expired key from being
// reused, so a key is applied once however late it comes back, and the
// window decides only whether its result is still given. That is also
// why the window needs no pruning, and no migration of its own: the key
// lives on an event, which is history and kept anyway.
//
// An operation that changes nothing (a label the issue has, an item
// already ticked, records all stored before) records no event, and so
// keeps no key: sent again, it is decided again on the state then, as
// a request without a key is.

// DefaultIdemWindow is how long a key replays its request when the
// store is given no window: 7 days, how long an outbox entry may wait
// and still be delivered without a person (design §14, decision 41).
const DefaultIdemWindow = 7 * 24 * time.Hour

// IdemWindow returns how long a key gives back its request's result.
func (s *Store) IdemWindow() time.Duration { return s.opts.IdemWindow }

// IdemPattern is what an idempotency key may look like: 1-64 letters,
// digits and ._:-, starting with a letter or digit.
var IdemPattern = lazyregexp.New(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

// idemStamp is an operation's key, request hash and, once the closure
// sets it, its result; flush writes them on the operation's last event.
type idemStamp struct {
	key, args string
	result    json.RawMessage
}

// validIdem refuses, with ErrInvalid, a key that is set and does not
// match IdemPattern.
func validIdem(key string) error {
	if key != "" && !IdemPattern.MatchString(key) {
		return fmt.Errorf("%w: idempotency key must be 1-64 letters, digits and ._:-, starting with a letter or digit", ErrInvalid)
	}
	return nil
}

// idemHash identifies a request: the operation and its arguments.
func idemHash(op string, args any) (string, error) {
	b, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("hash request: %w", err)
	}
	h := sha256.New()
	h.Write([]byte(op))
	h.Write([]byte{0})
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// replay looks key up for w's principal. When the key's request was
// already applied it sets *out to that request's result, exactly as
// stored, and reports true. Otherwise it arms w to stamp this operation
// with key and reports false. An empty key does nothing. op and args
// identify the request.
func replay[T any](ctx context.Context, w *wtx, key, op string, args any, out *T) (bool, error) {
	w.idem = nil
	if key == "" {
		return false, nil
	}
	hash, err := idemHash(op, args)
	if err != nil {
		return false, err
	}
	var at time.Time
	var prev sql.NullString
	var result []byte
	err = w.tx.QueryRowContext(ctx, `SELECT at, idem_args, idem_result FROM events WHERE principal = ? AND idem_key = ?`,
		w.actor.Principal, key).Scan(&at, &prev, &result)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		w.idem = &idemStamp{key: key, args: hash}
		return false, nil
	case err != nil:
		return false, fmt.Errorf("idempotency lookup: %w", err)
	case w.now.Sub(at) > w.window:
		return false, &IdemError{Key: key, Expired: true, Used: at, Window: w.window}
	case prev.String != hash || len(result) == 0:
		return false, &IdemError{Key: key}
	}
	// Decode into a zero value, not into *out: a failed earlier attempt of
	// the same write may have filled *out, and a field the stored result
	// leaves out would keep what that attempt put there.
	var v T
	if err := json.Unmarshal(result, &v); err != nil {
		return false, fmt.Errorf("idempotency result: %w", err)
	}
	*out = v
	return true, nil
}

// settleChanged is settle for an operation that may change nothing: one
// that recorded no event has nowhere to keep its key, and is left
// without one (see the package's note on idempotency above).
func (w *wtx) settleChanged(result any) error {
	if len(w.pending) == 0 {
		w.idem = nil
		return nil
	}
	return w.settle(result)
}

// settle records the operation's result for its idempotency stamp. It
// does nothing for an operation without a key.
func (w *wtx) settle(result any) error {
	if w.idem == nil {
		return nil
	}
	b, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode idempotency result: %w", err)
	}
	w.idem.result = b
	return nil
}
