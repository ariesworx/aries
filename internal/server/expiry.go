package server

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
	"github.com/ariesworx/aries/internal/telemetry"
)

// Keys' ends (identity.md §4, transport.md §11.3; slice S1). A key of
// the control store may have an end, after which it authenticates no
// one. Who enforces it:
//
//   - sshd, by the expiry-time of the key's line in the keys file, when
//     a connection authenticates, on the wall clock (keyLines);
//   - the daemon, at the handshake (keyHolds) and in the HTTPS key proof
//     (keyOwner), on the store's clock, which is never behind the wall
//     clock, so the daemon refuses at or before the moment sshd would;
//   - the daemon again, for a connection already open, which sshd never
//     looks at twice: at the key's end it takes the line out of the keys
//     file and closes the connections the key signed in (decision 59).
//
// The last runs on a timer set for the earliest end, and at least once
// per ReapInterval, so that a key added by `ariesd control init` beside
// the daemon is seen too.

// keyExpired is why connections are closed at their key's end.
const keyExpired = "key expired"

// keyEnds is what the daemon knows of keys' ends.
type keyEnds struct {
	// wake tells expireLoop that a key was added, which may end before
	// the end it waits for.
	wake chan struct{}
	// told holds the keys whose end has been logged, until they are
	// pruned; guarded by mu.
	mu   sync.Mutex
	told map[string]bool
}

// keysChanged wakes the timer of keys' ends, after a key was added.
func (s *Server) keysChanged() {
	select {
	case s.ends.wake <- struct{}{}:
	default: // already woken
	}
}

// expireLoop closes the connections of each key at its end until ctx
// is done: at once, then at the earliest end still to come, at a key
// added (keysChanged), and every ReapInterval at most. Its timer runs on
// the monotonic clock; how long it waits comes from the store's.
func (s *Server) expireLoop(ctx context.Context) {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.ends.wake:
		}
		wait := s.cfg.ReapInterval
		if next := s.ExpireKeys(ctx); !next.IsZero() {
			wait = min(wait, next.Sub(s.cfg.Control.Now()))
		}
		t.Reset(max(wait, 0))
	}
}

// ExpireKeys ends, on the store's clock, the keys whose end has passed:
// it writes the keys file without their lines, closes the connections
// they signed in, over SSH and over HTTPS, and logs each key once. It
// returns the earliest end still to come, or the zero time for none. A
// server of one project has no keys of its own, and does nothing.
func (s *Server) ExpireKeys(ctx context.Context) (next time.Time) {
	if s.cfg.Control == nil {
		return time.Time{}
	}
	ended, next := s.cfg.Control.KeyEnds()
	byKey := make(map[string]bool, len(ended))
	for _, k := range ended {
		byKey[k.Fingerprint] = true
	}
	if len(ended) > 0 {
		// The file first, so that sshd lets the key in no more once its
		// connections close. A failure is logged, and tried again.
		s.syncKeys(ctx)
	}
	s.mu.Lock()
	var closing []net.Conn
	per := map[string]int{}
	for c, o := range s.conns {
		if o.principal != "" && byKey[o.key] {
			closing = append(closing, c)
			per[o.key]++
		}
	}
	s.mu.Unlock()
	// Closing a connection ends its handler, which frees what it held.
	for _, c := range closing {
		_ = c.Close()
	}
	if n := len(closing); n > 0 {
		s.cfg.Telemetry.Record(telemetry.ConnectionsDropped, float64(n), telemetry.DropExpired)
		s.cfg.Logger.InfoContext(ctx, "connections dropped", "event", logs.ConnDropped, "reason", keyExpired, "by", store.ReaperActor.Principal,
			"connections", n, "own", false)
	}
	s.ends.mu.Lock()
	defer s.ends.mu.Unlock()
	for _, k := range ended {
		if s.ends.told[k.Fingerprint] {
			continue
		}
		s.ends.told[k.Fingerprint] = true
		s.cfg.Logger.InfoContext(ctx, "key reached its end", "event", logs.KeyExpired, "fingerprint", k.Fingerprint, "subject", k.Principal,
			"at", *k.ExpiresAt, "connections", per[k.Fingerprint])
	}
	for fp := range s.ends.told {
		if !byKey[fp] { // pruned
			delete(s.ends.told, fp)
		}
	}
	return next
}

// keyEnded is the refusal of a connection by a key of principal's that
// ended at end.
func keyEnded(principal string, end time.Time) *proto.Error {
	return proto.Errf(proto.CodeAuth,
		fmt.Sprintf("ask an admin for a key that has not ended: `aries admin key add %s FILE.pub`, with --expires for an end", principal),
		fmt.Sprintf("the key that signed in ended at %s", end.UTC().Format(time.RFC3339)))
}

// endedOffer refuses a key proof none of whose keys the server takes
// when one of them is a key of the store that has ended, and returns nil
// otherwise.
func (s *Server) endedOffer(keys []ssh.PublicKey) *proto.Error {
	if s.cfg.Control == nil {
		return nil
	}
	for _, k := range keys {
		if p, end, ok := s.cfg.Control.EndedKey(ssh.FingerprintSHA256(k)); ok {
			return keyEnded(p, end)
		}
	}
	return nil
}

// pruneKeys deletes the keys that ended more than store.KeyPruneAfter
// ago, and logs how many.
func (s *Server) pruneKeys(ctx context.Context) {
	if s.cfg.Control == nil {
		return
	}
	n, err := s.cfg.Control.PruneKeys(ctx)
	switch {
	case err != nil:
		if ctx.Err() == nil {
			s.cfg.Logger.ErrorContext(ctx, "prune keys that ended", "event", logs.ReaperPruneFailed, "err", err)
		}
	case n > 0:
		s.cfg.Logger.InfoContext(ctx, "pruned", "event", logs.ReaperPruned, "keys", n)
	}
}
