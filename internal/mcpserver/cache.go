package mcpserver

import (
	"context"
	"time"

	"github.com/ariesworx/starfix/internal/client"
	"github.com/ariesworx/starfix/internal/offline"
)

// keeper is a Conn that keeps each answer the cache keeps
// ([offline.Cache.Keep]) in the cache of the connection's principal, on
// the way past (design §18.6).
type keeper struct {
	Conn
	cache *offline.Cache
}

func (k keeper) Call(ctx context.Context, op string, args, result any) error {
	err := k.Conn.Call(ctx, op, args, result)
	if err == nil && result != nil {
		_ = k.cache.Keep(op, args, result) // a cache that cannot be written costs a later answer, not this one
	}
	return err
}

// Key is the key that signed the connection in, when it says.
func (k keeper) Key() client.KeyInfo {
	if c, ok := k.Conn.(interface{ Key() client.KeyInfo }); ok {
		return c.Key()
	}
	return client.KeyInfo{}
}

// fromCache stands in for the connection a tool call could not get: it
// answers each request the cache answers ([offline.Cache.Answer]) and
// fails every other with err, the error that said the server was not
// reached. Running a tool against it yields the tool's answer from this
// machine's copy, when the copy has all it asks.
type fromCache struct {
	cache              *offline.Cache
	err                error
	principal, session string
	// at is when the oldest answer it gave was kept.
	at time.Time
	// told: the tool said itself that its answer is cached (prime, in its
	// notices), so no line is added.
	told bool
}

func (f *fromCache) Call(_ context.Context, op string, args, result any) error {
	at, ok := f.cache.Answer(op, args, result)
	if !ok {
		return f.err
	}
	if f.at.IsZero() || at.Before(f.at) {
		f.at = at
	}
	return nil
}

func (f *fromCache) Err() error              { return nil }
func (f *fromCache) Close() error            { return nil }
func (f *fromCache) Principal() string       { return f.principal }
func (f *fromCache) Session() string         { return f.session }
func (f *fromCache) Notices(string) []string { return nil }
func (f *fromCache) Project() string         { return "" }

// cached returns the stand-in that answers from the cache of the
// principal this session's requests belong to, or nil when this machine
// does not know whose they are, or keeps no cache.
func (s *Server) cached(err error) *fromCache {
	if s.opts.Cache == nil {
		return nil
	}
	cache := s.opts.Cache.Cached()
	if cache == nil {
		return nil
	}
	return &fromCache{cache: cache, err: err, principal: cache.Principal(), session: s.opts.Cache.Session}
}

// keeping wraps c so that what it answers is kept, when the session has
// a cache.
func (s *Server) keeping(c Conn) Conn {
	if s.opts.Cache == nil {
		return c
	}
	cache := s.opts.Cache.CacheOf(c.Principal())
	if cache == nil {
		return c
	}
	return keeper{Conn: c, cache: cache}
}

// offlineRead is a read that is refused while the server cannot be
// reached, rather than answered from the cache (offline.RefusedOffline),
// and when the server was last reached.
type offlineRead struct {
	err  error
	tool string
	last time.Time
}

func (e *offlineRead) Error() string { return e.err.Error() }
func (e *offlineRead) Unwrap() error { return e.err }

// lastReached says when the server was last reached from this machine,
// for a refusal.
func lastReached(t time.Time) string {
	if t.IsZero() {
		return "this machine has no record of reaching the server"
	}
	return "server last reached " + t.UTC().Format("2006-01-02 15:04 UTC")
}
