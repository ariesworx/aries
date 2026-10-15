package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ariesworx/aries/internal/client"
	"github.com/ariesworx/aries/internal/mcpserver"
	"github.com/ariesworx/aries/internal/offline"
	"github.com/ariesworx/aries/internal/proto"
)

// keep keeps result, c's answer to op with args, in the cache of c's
// principal, when the cache keeps such answers (design §18.6). A cache
// that cannot be written costs a later answer, not this one.
func (r *runner) keep(c *mcpserver.RepoConn, op string, args, result any) {
	if cache := r.away().CacheOf(c.Principal()); cache != nil {
		_ = cache.Keep(op, args, result)
	}
}

// answered decodes into result this machine's copy of the answer to op
// with args, when err says the server was not reached, the command
// answers from the cache, and the cache of the principal the offered
// keys belong to has the answer. It reports whether it did.
func (r *runner) answered(op string, args, result any, err error) bool {
	if !r.cacheable || !client.Unreachable(err) {
		return false
	}
	cache := r.away().Cached()
	if cache == nil {
		return false
	}
	at, ok := cache.Answer(op, args, result)
	if ok {
		r.fromCache(at)
	}
	return ok
}

// fromCache notes that the command's answer is this machine's copy,
// kept at at: on Stderr in one line, which leaves Stdout the command's
// own output, or in the one JSON document with --json (emit). A hook
// says so in its own output.
func (r *runner) fromCache(at time.Time) {
	r.cachedAt = at
	if !r.json && !r.hook {
		_, _ = fmt.Fprintf(r.env.Stderr, "aries: %s\n", offline.CachedLine(at, r.env.Now()))
	}
}

// withCached adds "cached": true and when to doc, a JSON object, when
// the answer is from the cache.
func (r *runner) withCached(doc []byte) []byte {
	if r.cachedAt.IsZero() || len(doc) < 2 || doc[0] != '{' {
		return doc
	}
	at, err := r.cachedAt.UTC().MarshalJSON()
	if err != nil {
		return doc
	}
	fields := append(append([]byte(`{"cached":true,"cached_at":`), at...), ',')
	if bytes.Equal(doc, []byte("{}")) {
		fields[len(fields)-1] = '}'
		return fields
	}
	return append(fields, doc[1:]...)
}

// refusedOffline adds to err, when it says the server was not reached
// and op is a read never answered from the cache, why it is not, when
// the server was last reached and what answers meanwhile.
func (r *runner) refusedOffline(op string, err error) error {
	if !client.Unreachable(err) || !offline.RefusedOffline(op) {
		return err
	}
	pe, _ := errors.AsType[*proto.Error](err) // Unreachable says it is one
	e := *pe
	e.Message += fmt.Sprintf("; %s is not answered from this machine's copy, since it says what others are doing now (%s)",
		op, offline.LastReachedLine(r.away().LastReached()))
	if e.Fix != "" {
		e.Fix += "; "
	}
	e.Fix += "meanwhile `aries prime` and `aries show ID` answer from this machine's copy of what you read"
	return &e
}

// prime connects and builds prime, and keeps it for the connection's
// principal. When the server cannot be reached it is this machine's
// copy instead (mcpserver.CachedPrime), when there is one.
func (r *runner) prime(ctx context.Context) (*mcpserver.Prime, error) {
	c, err := r.connect(ctx)
	if err == nil {
		var p *mcpserver.Prime
		if p, err = mcpserver.BuildPrime(ctx, c, r.env.Version, r.outboxNotices(c)...); err == nil {
			_ = mcpserver.KeepPrime(r.away().CacheOf(c.Principal()), p) // costs a later prime, not this one
			return p, nil
		}
	}
	if !client.Unreachable(err) {
		return nil, err
	}
	cache := r.away().Cached()
	if cache == nil {
		return nil, err
	}
	var notices []string
	if n := r.away().NoticeFor(cache.Principal()); n != "" {
		notices = append(notices, n)
	}
	p, at, ok := mcpserver.CachedPrime(cache, r.clientOptions().Session, r.env.Now(), notices...)
	if !ok {
		return nil, err
	}
	r.fromCache(at)
	return p, nil
}

const cacheUsage = "cache [clear]"

// cacheDoc is one principal's cache in `aries cache --json`.
type cacheDoc struct {
	Principal string    `json:"principal"`
	Answers   int       `json:"answers"`
	Bytes     int64     `json:"bytes"`
	Oldest    time.Time `json:"oldest"`
	Newest    time.Time `json:"newest"`
}

// cmdCache says what this machine keeps of what was read from the
// server for this project, or with clear deletes it, every principal's.
// It never connects.
func cmdCache(_ context.Context, r *runner, args []string) error {
	fs := r.newFlags("cache")
	pos, err := parse(fs, args, cacheUsage)
	if err != nil {
		return err
	}
	clearing := len(pos) == 1 && pos[0] == "clear"
	if len(pos) > 0 && !clearing {
		return usagef(cacheUsage, "unknown cache action %q", pos[0])
	}
	l, _, err := r.away().Local()
	if err != nil {
		return err
	}
	if clearing {
		n, err := l.ClearCaches()
		if err != nil {
			return outboxFailed(l, err)
		}
		if r.json {
			r.emit(struct {
				Deleted int `json:"deleted"`
			}{n})
			return nil
		}
		_, _ = fmt.Fprintf(r.env.Stdout, "deleted %d cached answer%s from this machine\n", n, plural(n))
		return nil
	}
	stats, err := l.Caches()
	if err != nil {
		return outboxFailed(l, err)
	}
	doc := struct {
		Caches      []cacheDoc `json:"caches"`
		LastReached time.Time  `json:"last_reached,omitzero"`
	}{Caches: []cacheDoc{}, LastReached: l.LastReached().UTC()}
	for _, st := range stats {
		doc.Caches = append(doc.Caches, cacheDoc{Principal: st.Principal, Answers: st.Entries, Bytes: st.Bytes,
			Oldest: st.Oldest.UTC(), Newest: st.Newest.UTC()})
	}
	if r.json {
		r.emit(doc)
		return nil
	}
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(r.env.Stdout, format, a...) }
	if len(doc.Caches) == 0 {
		p("this machine keeps nothing read from the server for this project\n")
		return nil
	}
	for _, c := range doc.Caches {
		p("%s's cache: %d answer%s, %d KiB, kept from %s to %s\n", esc(c.Principal), c.Answers, plural(c.Answers),
			(c.Bytes+1023)/1024, when(c.Oldest), when(c.Newest))
	}
	p("%s\n", offline.LastReachedLine(doc.LastReached))
	p("they are shown only while the server cannot be reached; `aries cache clear` deletes them from this machine\n")
	return nil
}
