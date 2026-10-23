package offline

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// The cache's default bounds (doc.go, "The cache"), for each principal's
// cache of a server and project.
const (
	DefaultCacheEntries    = 500
	DefaultCacheBytes      = 16 << 20
	DefaultCacheEntryBytes = 1 << 20
	// DefaultCacheAge is the outbox's age cap, and the server's default
	// idempotency window: past it, what a session read is no longer worth
	// working from.
	DefaultCacheAge = 7 * 24 * time.Hour
)

// CacheOptions bound each principal's cache. A field left zero takes its
// default.
type CacheOptions struct {
	// MaxEntries and MaxBytes bound the answers kept, and their size on
	// disk; past either the oldest go first.
	MaxEntries int
	MaxBytes   int64
	// MaxEntryBytes bounds one answer: a larger one is not kept.
	MaxEntryBytes int
	// MaxAge is how long an answer is kept and served.
	MaxAge time.Duration
}

func (o CacheOptions) withDefaults() CacheOptions {
	if o.MaxEntries <= 0 {
		o.MaxEntries = DefaultCacheEntries
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultCacheBytes
	}
	if o.MaxEntryBytes <= 0 {
		o.MaxEntryBytes = DefaultCacheEntryBytes
	}
	if o.MaxAge <= 0 {
		o.MaxAge = DefaultCacheAge
	}
	return o
}

// PrimeKey is the key a session's prime is kept under, whole.
const PrimeKey = "prime"

// cacheVersion is the version of an entry's format; an entry of another
// is read as absent. 2 is the protocol's names as v0.5.0 settled them
// (decision 88): an answer kept under 1 has the names before.
const cacheVersion = 2

// The names in a cache directory beside its entries.
const (
	// cachePrincipal holds the name of the principal the cache is for,
	// which the directory's name, a hash, does not say.
	cachePrincipal = "principal"
	// cacheTemp begins the name of an entry being written.
	cacheTemp = ".tmp-"
	// cacheExt ends the name of an entry.
	cacheExt = ".json"
)

// reachedFile holds when the server was last reached, as text.
const reachedFile = "reached"

// cacheEntry is one file of a cache: an answer, the key it answers, and
// when it was kept, by the client's clock.
type cacheEntry struct {
	V     int             `json:"v"`
	Key   string          `json:"key"`
	At    time.Time       `json:"at"`
	Value json.RawMessage `json:"value"`
}

// Cache is what one principal's sessions read from one server and
// project, kept on this machine to be shown while the server cannot be
// reached (design §18.6). It holds no file open, and is safe for use by
// several goroutines and processes. A Cache for no principal keeps and
// answers nothing.
type Cache struct {
	l         *Local
	principal string
	dir       string
	opts      CacheOptions
}

// Cache returns principal's cache. It makes nothing until something is
// kept.
func (l *Local) Cache(principal string) *Cache {
	return &Cache{l: l, principal: principal, dir: l.cacheDir(principal), opts: l.opts.Cache.withDefaults()}
}

// Principal is whose cache it is.
func (c *Cache) Principal() string { return c.principal }

// cacheDir is principal's cache: named for the principal, as an outbox
// is, so it is never another principal's answer.
func (l *Local) cacheDir(principal string) string {
	return filepath.Join(l.dir, "cache-"+hashed("principal", principal))
}

// file is the entry of key.
func (c *Cache) file(key string) string {
	return filepath.Join(c.dir, hashed("cache", key)+cacheExt)
}

// cacheKey is the key op's answer to args is kept under, when op is one
// the cache answers: show by the issue alone, whichever text it was
// asked for, and inbox and digest by all their arguments.
func cacheKey(op string, args any) (string, bool) {
	if !AnsweredOffline(op) {
		return "", false
	}
	if op == proto.OpShow {
		var a struct {
			ID string `json:"id"`
		}
		if !convert(args, &a) || a.ID == "" {
			return "", false
		}
		return op + " " + a.ID, true
	}
	b, err := json.Marshal(args)
	if err != nil {
		return "", false
	}
	return op + " " + string(b), true
}

// convert decodes from's JSON into to.
func convert(from, to any) bool {
	b, err := json.Marshal(from)
	return err == nil && json.Unmarshal(b, to) == nil
}

// What becomes of each read of the protocol while the server cannot be
// reached (design §18.6): it is answered from the cache
// (AnsweredOffline), refused offline (RefusedOffline), or not kept, with
// why (unkept). The writes' are internal/outbox's (Queueable and
// Unqueued), and a test holds them all to every op of internal/proto.
// They are switches, not maps, so that aries allocates nothing for them
// as it starts.

// AnsweredOffline reports whether op is a read the cache answers while
// the server cannot be reached, when it holds the answer: show, inbox
// and digest, what a session was given to go on with the work it has
// (design §18.6). start's answer is kept too, as a show of the issue it
// took (Keep), and prime's whole, under PrimeKey, by its callers.
func AnsweredOffline(op string) bool {
	switch op {
	case proto.OpShow, proto.OpInbox, proto.OpDigest:
		return true
	}
	return false
}

// RefusedOffline reports whether op is a read that is refused while the
// server cannot be reached, rather than answered from the cache: an
// answer about what everyone is doing now, which from a copy would be
// wrong in the way that has two sessions take the same work (design
// §18.6).
func RefusedOffline(op string) bool {
	switch op {
	case proto.OpReady, proto.OpBlocked, proto.OpList, proto.OpWho, proto.OpClaims, proto.OpGates, proto.OpDates, proto.OpLocks:
		return true
	}
	return false
}

// unkept reports why the answers of op, a read neither answered nor
// refused offline, are not kept, or "" for any other op. While the
// server cannot be reached such a read fails as any request does.
func unkept(op string) string {
	switch op {
	case proto.OpComments:
		return "an issue's comments are not what a session was given; show has its last handoff"
	case proto.OpHistory:
		return "an issue's history is not what a session was given; show has its state"
	case proto.OpRecall:
		return "a search needs the server's memories; prime and start bring the relevant ones"
	case proto.OpConflicts:
		return "conflicts are resolved on the server's state; show has an issue's open ones"
	case proto.OpWatch:
		return "it asks for what the server pushes, which no copy can send"
	case proto.OpWorkflow:
		return "the project's workflow is not what a session was given"
	case proto.OpPrices, proto.OpCost, proto.OpHours, proto.OpPlans:
		return "the project's accounts are not what a session was given"
	case proto.OpProjects, proto.OpPrincipals, proto.OpPrincipalShow, proto.OpPrincipalWhois, proto.OpMembers, proto.OpKeys,
		proto.OpInvites, proto.OpIdentities, proto.OpSignIns, proto.OpTrustRules, proto.OpTrustShow:
		// What the server holds of access stays off people's machines
		// (§18.6, "What a cache means for access").
		return "it is an admin's read of the server's access"
	case proto.OpStatus:
		return "it is the server's report of itself now"
	}
	return ""
}

// Keep keeps result, the server's answer to op with args, when op is one
// whose answers the cache keeps (design §18.6): show; start, kept as a
// show of the issue it took; inbox; and digest. Anything else it
// ignores. An answer larger than the cap on one is not kept, and the
// answer it would have replaced is removed, since it is now out of date.
func (c *Cache) Keep(op string, args, result any) error {
	if op == proto.OpStart {
		var r proto.StartResult
		if !convert(result, &r) || r.Issue.ID == "" {
			return nil
		}
		op, args = proto.OpShow, proto.ShowArgs{ID: r.Issue.ID}
		result = proto.ShowResult{Issue: r.Issue, Claim: r.Claim, Handoff: r.Handoff, Items: r.Items}
	}
	key, ok := cacheKey(op, args)
	if !ok {
		return nil
	}
	return c.Put(key, result)
}

// Answer decodes into result the kept answer to op with args, when op is
// one the cache answers (show, inbox and digest), and returns when it
// was kept.
func (c *Cache) Answer(op string, args, result any) (time.Time, bool) {
	key, ok := cacheKey(op, args)
	if !ok {
		return time.Time{}, false
	}
	return c.Get(key, result)
}

// Put keeps v under key, written whole to a temporary file and renamed
// into place, then removes what is past the caps.
func (c *Cache) Put(key string, v any) error {
	if c.principal == "" {
		return nil
	}
	value, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("cache %s: %w", key, err)
	}
	now := c.l.opts.Now()
	b, err := json.Marshal(cacheEntry{V: cacheVersion, Key: key, At: now.UTC(), Value: value})
	if err != nil {
		return fmt.Errorf("cache %s: %w", key, err)
	}
	if err := c.ready(); err != nil {
		return err
	}
	if len(b) > c.opts.MaxEntryBytes {
		if err := os.Remove(c.file(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	f, err := os.CreateTemp(c.dir, cacheTemp+"*") // mode 0600
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	err = errors.Join(err, f.Close())
	if err == nil {
		// The entry's age is its file's: what prune reads without opening
		// each one.
		err = os.Chtimes(f.Name(), now, now)
	}
	if err == nil {
		err = os.Rename(f.Name(), c.file(key))
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return err
	}
	return c.prune(now)
}

// ready checks the directories down to the cache's and makes those that
// are not there, with the file that names the cache's principal.
func (c *Cache) ready() error {
	if err := c.l.ready(true, ""); err != nil {
		return err
	}
	if err := private(c.dir); err != nil {
		return err
	}
	if err := os.Mkdir(c.dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if err := private(c.dir); err != nil { // made by someone else meanwhile
		return err
	}
	owner := filepath.Join(c.dir, cachePrincipal)
	if _, err := os.Stat(owner); errors.Is(err, fs.ErrNotExist) {
		return os.WriteFile(owner, []byte(c.principal+"\n"), 0o600)
	}
	return nil
}

// Get decodes into v the entry kept under key, and returns when it was
// kept. An entry that is not there, is not whole, is of another version
// or key, is older than the cap on age, or is in a directory that is not
// the user's alone, is absent.
func (c *Cache) Get(key string, v any) (time.Time, bool) {
	if c.principal == "" || private(c.l.root, c.l.dir, c.dir) != nil {
		return time.Time{}, false
	}
	f, err := os.Open(c.file(key))
	if err != nil {
		return time.Time{}, false
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, int64(c.opts.MaxEntryBytes)+1))
	if err != nil || len(b) > c.opts.MaxEntryBytes {
		return time.Time{}, false
	}
	var e cacheEntry
	if json.Unmarshal(b, &e) != nil || e.V != cacheVersion || e.Key != key || e.At.IsZero() {
		return time.Time{}, false
	}
	if c.l.opts.Now().Sub(e.At) > c.opts.MaxAge || json.Unmarshal(e.Value, v) != nil {
		return time.Time{}, false
	}
	return e.At, true
}

// staleTemp is how old a temporary file must be before prune takes it
// for one a writer that died left.
const staleTemp = time.Minute

// prune removes the entries past the cap on age, then the oldest until
// the rest are within the caps on entries and bytes, and temporary
// files left by writers that died. Several processes may prune at once:
// a file another removed first is not an error.
func (c *Cache) prune(now time.Time) error {
	ents, err := os.ReadDir(c.dir)
	if err != nil {
		return err
	}
	type kept struct {
		name string
		mod  time.Time
		size int64
	}
	var files []kept
	var errs []error
	remove := func(name string) {
		if err := os.Remove(filepath.Join(c.dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	for _, d := range ents {
		info, err := d.Info()
		if err != nil {
			continue // removed meanwhile
		}
		name, age := d.Name(), now.Sub(info.ModTime())
		switch {
		case strings.HasPrefix(name, cacheTemp):
			if age > staleTemp {
				remove(name)
			}
		case !strings.HasSuffix(name, cacheExt):
		case age > c.opts.MaxAge:
			remove(name)
		default:
			files = append(files, kept{name, info.ModTime(), info.Size()})
		}
	}
	slices.SortFunc(files, func(a, b kept) int { return b.mod.Compare(a.mod) })
	var total int64
	for i, f := range files {
		total += f.size
		if i >= c.opts.MaxEntries || total > c.opts.MaxBytes {
			remove(f.name)
		}
	}
	return errors.Join(errs...)
}

// Forget removes the cache, and returns how many answers it held.
func (c *Cache) Forget() (int, error) {
	if err := private(c.l.root, c.l.dir); err != nil {
		return 0, err
	}
	st, err := c.l.cacheStat(c.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return st.Entries, os.RemoveAll(c.dir)
}

// CacheStat is what one principal's cache holds.
type CacheStat struct {
	Principal string
	Entries   int
	Bytes     int64
	// Oldest and Newest are when the oldest and newest answers were kept.
	Oldest, Newest time.Time
}

// cacheStat counts what the cache in dir holds.
func (l *Local) cacheStat(dir string) (CacheStat, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return CacheStat{}, err
	}
	var st CacheStat
	if b, err := os.ReadFile(filepath.Join(dir, cachePrincipal)); err == nil { //nolint:gosec // aries's own file, in a directory checked to be the user's
		st.Principal = strings.TrimSpace(string(b))
	}
	for _, d := range ents {
		info, err := d.Info()
		if err != nil || !strings.HasSuffix(d.Name(), cacheExt) || strings.HasPrefix(d.Name(), cacheTemp) {
			continue
		}
		st.Entries++
		st.Bytes += info.Size()
		if st.Oldest.IsZero() || info.ModTime().Before(st.Oldest) {
			st.Oldest = info.ModTime()
		}
		if info.ModTime().After(st.Newest) {
			st.Newest = info.ModTime()
		}
	}
	return st, nil
}

// cacheDirs are the caches of this server and project, of every
// principal, that are the user's alone.
func (l *Local) cacheDirs() ([]string, error) {
	if err := private(l.root, l.dir); err != nil {
		return nil, err
	}
	dirs, err := filepath.Glob(filepath.Join(l.dir, "cache-*")) // the pattern is well formed
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range dirs {
		if st, err := os.Lstat(d); err == nil && st.IsDir() && private(d) == nil {
			out = append(out, d)
		}
	}
	return out, nil
}

// Caches says what each principal's cache of this server and project
// holds, in the order of their names.
func (l *Local) Caches() ([]CacheStat, error) {
	dirs, err := l.cacheDirs()
	if err != nil {
		return nil, err
	}
	var out []CacheStat
	for _, d := range dirs {
		if st, err := l.cacheStat(d); err == nil {
			out = append(out, st)
		}
	}
	slices.SortFunc(out, func(a, b CacheStat) int { return strings.Compare(a.Principal, b.Principal) })
	return out, nil
}

// ClearCaches removes the cache of every principal of this server and
// project from this machine, and returns how many answers they held.
// Outboxes are left alone: what they hold exists nowhere else.
func (l *Local) ClearCaches() (int, error) {
	dirs, err := l.cacheDirs()
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, d := range dirs {
		if st, err := l.cacheStat(d); err == nil {
			n += st.Entries
		}
		errs = append(errs, os.RemoveAll(d))
	}
	return n, errors.Join(errs...)
}

// Touch notes that the server was reached just now, at most once a
// minute. A failure to note it is dropped, as Missed's is.
func (l *Local) Touch() {
	last, now := l.LastReached(), l.opts.Now()
	if !last.IsZero() && now.Sub(last) < missEvery && !now.Before(last) {
		return
	}
	_ = l.replace(reachedFile, []byte(now.UTC().Format(time.RFC3339)+"\n"))
}

// LastReached is when the server was last reached, to within a minute,
// or the zero time when this machine has no record of it.
func (l *Local) LastReached() time.Time {
	if l.ready(false, "") != nil {
		return time.Time{}
	}
	b, err := os.ReadFile(filepath.Join(l.dir, reachedFile))
	if err != nil {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, strings.TrimSpace(string(b))) // not a time: never
	return t
}

// LastReachedLine says when the server was last reached from this
// machine, for a refusal of a read that is not answered offline.
func LastReachedLine(t time.Time) string {
	if t.IsZero() {
		return "this machine has no record of reaching the server"
	}
	return "server last reached " + t.UTC().Format("2006-01-02 15:04 UTC")
}

// CachedLine is the one line that says an answer is this machine's copy,
// kept at at, and how old it is at now.
func CachedLine(at, now time.Time) string {
	return fmt.Sprintf("cached: aries cannot reach its server, so this is this machine's copy from %s (%s old), and may be out of date",
		at.UTC().Format("2006-01-02 15:04 UTC"), proto.Span(max(now.Sub(at), 0)))
}
