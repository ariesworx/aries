package offline

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/outbox"
	"github.com/ariesworx/starfix/internal/proto"
)

func newCache(t *testing.T, o CacheOptions) (*Local, *clock) {
	t.Helper()
	c := &clock{t: t0}
	return Open(ownDir(t), hostKey, project, Options{Now: c.now, Cache: o}), c
}

func shown(id, title string) proto.ShowResult {
	return proto.ShowResult{Issue: proto.Issue{ID: id, Title: title, Status: "in_progress", Rev: 3}}
}

// keep is Keep, failing the test on an error.
func keep(t *testing.T, c *Cache, op string, args, result any) {
	t.Helper()
	if err := c.Keep(op, args, result); err != nil {
		t.Fatalf("Keep(%s, %+v) = %v", op, args, err)
	}
}

// What §18.6 keeps is answered from the cache, by op and arguments, with
// when it was kept; what it refuses offline is never kept at all.
func TestCacheKeepsWhatASessionRead(t *testing.T) {
	t.Parallel()
	l, clk := newCache(t, CacheOptions{})
	c := l.Cache("alice")
	keep(t, c, proto.OpShow, proto.ShowArgs{ID: "sf-a", Full: true}, shown("sf-a", "Shown"))
	clk.t = t0.Add(time.Minute)
	keep(t, c, proto.OpStart, proto.StartArgs{Lease: "15m"}, proto.StartResult{
		Issue:   proto.Issue{ID: "sf-b", Title: "Started", Status: "in_progress"},
		Handoff: &proto.Handoff{Comment: proto.Comment{Body: "half done"}},
		Items:   []proto.AcceptanceItem{{N: 1, Text: "tests pass"}},
		Claim:   &proto.Claim{ID: "sf-b", By: "alice", Epoch: 2},
	})
	keep(t, c, proto.OpInbox, proto.InboxArgs{Limit: 20}, proto.InboxResult{Unread: 4})
	keep(t, c, proto.OpDigest, proto.DigestArgs{Since: "24h"}, proto.DigestResult{Totals: proto.DigestTotals{Closed: 2}})
	for _, op := range []string{proto.OpReady, proto.OpList, proto.OpBlocked, proto.OpWho, proto.OpClaims, proto.OpComments} {
		keep(t, c, op, proto.LimitArgs{Limit: 5}, proto.ListResult{Issues: []proto.Summary{{ID: "sf-x"}}})
	}

	var s proto.ShowResult
	if at, ok := c.Answer(proto.OpShow, proto.ShowArgs{ID: "sf-a"}, &s); !ok || !at.Equal(t0) || s.Issue.Title != "Shown" {
		t.Errorf("Answer(show sf-a) = %+v, %v, %v; want what show returned, kept at %v", s, at, ok, t0)
	}
	s = proto.ShowResult{}
	if at, ok := c.Answer(proto.OpShow, proto.ShowArgs{ID: "sf-b", Full: true}, &s); !ok || !at.Equal(t0.Add(time.Minute)) ||
		s.Issue.Title != "Started" || s.Handoff == nil || s.Handoff.Body != "half done" || len(s.Items) != 1 || s.Claim == nil || s.Claim.Epoch != 2 {
		t.Errorf("Answer(show sf-b) = %+v, %v, %v; want what start returned, as show has it", s, at, ok)
	}
	var in proto.InboxResult
	if _, ok := c.Answer(proto.OpInbox, proto.InboxArgs{Limit: 20}, &in); !ok || in.Unread != 4 {
		t.Errorf("Answer(inbox, limit 20) = %+v, %v; want the inbox kept", in, ok)
	}
	if _, ok := c.Answer(proto.OpInbox, proto.InboxArgs{All: true}, &in); ok {
		t.Error("Answer(inbox --all) found an answer to other arguments")
	}
	var d proto.DigestResult
	if _, ok := c.Answer(proto.OpDigest, proto.DigestArgs{Since: "24h"}, &d); !ok || d.Totals.Closed != 2 {
		t.Errorf("Answer(digest 24h) = %+v, %v; want the digest kept", d, ok)
	}
	if _, ok := c.Answer(proto.OpDigest, proto.DigestArgs{Since: "7d"}, &d); ok {
		t.Error("Answer(digest 7d) found an answer to other arguments")
	}
	for _, op := range []string{proto.OpReady, proto.OpList, proto.OpBlocked, proto.OpWho, proto.OpClaims, proto.OpComments, proto.OpStart} {
		var r proto.ListResult
		if _, ok := c.Answer(op, proto.LimitArgs{Limit: 5}, &r); ok {
			t.Errorf("Answer(%s) = %+v; want nothing: it is not answered offline", op, r)
		}
	}
	if st := cacheStats(t, l)["alice"]; st.Entries != 4 {
		t.Errorf("alice's cache holds %d entries, want 4: show twice, inbox and digest", st.Entries)
	}
}

func cacheStats(t *testing.T, l *Local) map[string]CacheStat {
	t.Helper()
	all, err := l.Caches()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]CacheStat{}
	for _, st := range all {
		out[st.Principal] = st
	}
	return out
}

// One principal's cache is never another's answer, and nobody's write
// makes one.
func TestCacheIsThePrincipalsOwn(t *testing.T) {
	t.Parallel()
	l, _ := newCache(t, CacheOptions{})
	keep(t, l.Cache("alice"), proto.OpShow, proto.ShowArgs{ID: "sf-a"}, shown("sf-a", "Alice's"))
	var s proto.ShowResult
	if _, ok := l.Cache("bob").Answer(proto.OpShow, proto.ShowArgs{ID: "sf-a"}, &s); ok {
		t.Errorf("bob's cache answered with alice's %+v", s)
	}
	keep(t, l.Cache(""), proto.OpShow, proto.ShowArgs{ID: "sf-b"}, shown("sf-b", "Nobody's"))
	if _, ok := l.Cache("").Answer(proto.OpShow, proto.ShowArgs{ID: "sf-b"}, &s); ok {
		t.Errorf("the cache of nobody answered %+v", s)
	}
	if _, err := os.Stat(l.cacheDir("")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a cache was made for nobody: %v", err)
	}
}

// A file that is not a whole entry of this key is read as absent: torn,
// not JSON, of another version, or of another key.
func TestCacheReadsAnUnreadableFileAsAbsent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		spoil func(t *testing.T, file string, b []byte)
	}{
		{"cut short", func(t *testing.T, file string, b []byte) { write(t, file, b[:len(b)/2]) }},
		{"empty", func(t *testing.T, file string, _ []byte) { write(t, file, nil) }},
		{"not JSON", func(t *testing.T, file string, _ []byte) { write(t, file, []byte("\x00\x01garbage")) }},
		{"another version", func(t *testing.T, file string, b []byte) {
			write(t, file, []byte(strings.Replace(string(b), `"v":1`, `"v":99`, 1)))
		}},
		{"another key's", func(t *testing.T, file string, b []byte) {
			write(t, file, []byte(strings.Replace(string(b), `"key":"show sf-a"`, `"key":"show sf-z"`, 1)))
		}},
		{"a value that does not decode", func(t *testing.T, file string, b []byte) {
			i := strings.Index(string(b), `"value":`)
			write(t, file, []byte(string(b[:i])+`"value":"a string"}`))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l, _ := newCache(t, CacheOptions{})
			c := l.Cache("alice")
			keep(t, c, proto.OpShow, proto.ShowArgs{ID: "sf-a"}, shown("sf-a", "Whole"))
			file := c.file("show sf-a")
			b, err := os.ReadFile(file) //nolint:gosec // the test's own file
			if err != nil {
				t.Fatal(err)
			}
			tc.spoil(t, file, b)
			var s proto.ShowResult
			if _, ok := c.Answer(proto.OpShow, proto.ShowArgs{ID: "sf-a"}, &s); ok {
				t.Errorf("a file %s answered %+v; want it read as absent", tc.name, s)
			}
		})
	}
}

func write(t *testing.T, file string, b []byte) {
	t.Helper()
	if err := os.WriteFile(file, b, 0o600); err != nil { //nolint:gosec // the test's own file
		t.Fatal(err)
	}
}

// Each entry is written whole and renamed into place: nothing of a write
// is left behind, and a temporary file a writer that died left is
// removed once it is old.
func TestCacheWritesWhole(t *testing.T) {
	t.Parallel()
	l, clk := newCache(t, CacheOptions{})
	c := l.Cache("alice")
	dead := filepath.Join(l.cacheDir("alice"), ".tmp-dead")
	keep(t, c, proto.OpShow, proto.ShowArgs{ID: "sf-a"}, shown("sf-a", "One"))
	write(t, dead, []byte(`{"v":1`))
	if err := os.Chtimes(dead, t0, t0); err != nil {
		t.Fatal(err)
	}
	clk.t = t0.Add(10 * time.Minute)
	keep(t, c, proto.OpShow, proto.ShowArgs{ID: "sf-b"}, shown("sf-b", "Two"))
	names, err := filepath.Glob(filepath.Join(l.cacheDir("alice"), "*"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, n := range names {
		got = append(got, filepath.Base(n))
	}
	if len(got) != 3 || !strings.Contains(strings.Join(got, " "), "principal") {
		t.Errorf("the cache directory holds %v; want two entries and the principal's name, and no temporary file", got)
	}
}

// The caps: entries, bytes and age, with the oldest going first, and an
// answer too large to keep not kept, nor the one before it served.
func TestCacheCaps(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", 2000)
	for _, tc := range []struct {
		name string
		opts CacheOptions
		// keep runs at one minute apart, from t0; then the clock is at
		// after.
		keep  []proto.ShowResult
		after time.Duration
		want  []string // the ids answered
	}{
		{"entries", CacheOptions{MaxEntries: 2},
			[]proto.ShowResult{shown("sf-a", ""), shown("sf-b", ""), shown("sf-c", "")}, 3 * time.Minute, []string{"sf-b", "sf-c"}},
		{"bytes", CacheOptions{MaxBytes: 5000},
			[]proto.ShowResult{shown("sf-a", big), shown("sf-b", big), shown("sf-c", big)}, 3 * time.Minute, []string{"sf-b", "sf-c"}},
		{"an entry's bytes", CacheOptions{MaxEntryBytes: 1000},
			[]proto.ShowResult{shown("sf-a", ""), shown("sf-a", big), shown("sf-b", "")}, 3 * time.Minute, []string{"sf-b"}},
		{"age", CacheOptions{MaxAge: time.Hour},
			[]proto.ShowResult{shown("sf-a", ""), shown("sf-b", ""), shown("sf-c", "")}, time.Hour + 90*time.Second, []string{"sf-c"}},
		{"defaults", CacheOptions{},
			[]proto.ShowResult{shown("sf-a", big), shown("sf-b", ""), shown("sf-c", "")}, 6 * 24 * time.Hour, []string{"sf-a", "sf-b", "sf-c"}},
		{"the default age", CacheOptions{},
			[]proto.ShowResult{shown("sf-a", ""), shown("sf-b", "")}, DefaultCacheAge + 30*time.Second, []string{"sf-b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l, clk := newCache(t, tc.opts)
			c := l.Cache("alice")
			for i, s := range tc.keep {
				clk.t = t0.Add(time.Duration(i) * time.Minute)
				keep(t, c, proto.OpShow, proto.ShowArgs{ID: s.Issue.ID}, s)
			}
			clk.t = t0.Add(tc.after)
			var got []string
			for _, id := range []string{"sf-a", "sf-b", "sf-c"} {
				var s proto.ShowResult
				if _, ok := c.Answer(proto.OpShow, proto.ShowArgs{ID: id}, &s); ok {
					got = append(got, s.Issue.ID)
				}
			}
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("answered %v, want %v", got, tc.want)
			}
			// A write prunes what is past the caps, so the files on disk
			// are only what can still be answered.
			keep(t, c, proto.OpInbox, proto.InboxArgs{}, proto.InboxResult{})
			want := len(tc.want) + 1
			if tc.opts.MaxEntries > 0 {
				want = min(want, tc.opts.MaxEntries)
			}
			if st := cacheStats(t, l)["alice"]; st.Entries != want {
				t.Errorf("after a write the cache holds %d entries, want %d", st.Entries, want)
			}
		})
	}
}

// The cache is refused, read and written, in a directory that is not the
// user's alone, and its mode is left as it was.
func TestCacheDirectoryMustBeTheUsersOwn(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no mode bits; its ACLs are not checked")
	}
	for _, tc := range []struct {
		name string
		dir  func(l *Local) string
	}{
		{"the data directory", func(l *Local) string { return l.root }},
		{"the server's directory", func(l *Local) string { return l.dir }},
		{"the cache", func(l *Local) string { return l.cacheDir("alice") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l, _ := newCache(t, CacheOptions{})
			c := l.Cache("alice")
			keep(t, c, proto.OpShow, proto.ShowArgs{ID: "sf-a"}, shown("sf-a", "Mine"))
			dir := tc.dir(l)
			if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // the mode sfx must refuse
				t.Fatal(err)
			}
			var s proto.ShowResult
			if _, ok := c.Answer(proto.OpShow, proto.ShowArgs{ID: "sf-a"}, &s); ok {
				t.Errorf("Answer read %+v from %s, which others can read and write", s, dir)
			}
			err := c.Keep(proto.OpShow, proto.ShowArgs{ID: "sf-b"}, shown("sf-b", "Mine too"))
			if pe, ok := errors.AsType[*proto.Error](err); !ok || !strings.Contains(pe.Message, dir) || !strings.Contains(pe.Fix, "chmod 700 "+dir) {
				t.Errorf("Keep = %v, want a refusal that names %s with a fix", err, dir)
			}
			if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o755 {
				t.Errorf("%s now has mode %v (%v), want it left", dir, st.Mode().Perm(), err)
			}
		})
	}
}

// Clearing removes every principal's cache of the project on this
// machine, and Forget one principal's; neither touches an outbox.
func TestCacheClear(t *testing.T) {
	t.Parallel()
	l, _ := newCache(t, CacheOptions{})
	keep(t, l.Cache("alice"), proto.OpShow, proto.ShowArgs{ID: "sf-a"}, shown("sf-a", ""))
	keep(t, l.Cache("alice"), proto.OpShow, proto.ShowArgs{ID: "sf-b"}, shown("sf-b", ""))
	keep(t, l.Cache("bob"), proto.OpShow, proto.ShowArgs{ID: "sf-a"}, shown("sf-a", ""))
	if _, err := l.Reached("alice", keyA, true, proto.Proto); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Keep("alice", entry(proto.OpComment, "sf-a"), errReach); err != nil {
		t.Fatal(err)
	}
	if n, err := l.Cache("alice").Forget(); err != nil || n != 2 {
		t.Errorf("Forget(alice) = %d, %v; want her 2 entries removed", n, err)
	}
	st := cacheStats(t, l)
	if _, ok := st["alice"]; ok || st["bob"].Entries != 1 {
		t.Errorf("after Forget(alice) the caches are %+v; want bob's alone", st)
	}
	if n, err := l.ClearCaches(); err != nil || n != 1 {
		t.Errorf("ClearCaches = %d, %v; want bob's 1 entry removed", n, err)
	}
	if st := cacheStats(t, l); len(st) != 0 {
		t.Errorf("after ClearCaches the caches are %+v, want none", st)
	}
	if n, err := l.Waiting("alice"); err != nil || n.Pending != 1 {
		t.Errorf("Waiting(alice) = %+v, %v; want her outbox left alone", n, err)
	}
	if n, err := l.ClearCaches(); err != nil || n != 0 {
		t.Errorf("ClearCaches with nothing cached = %d, %v", n, err)
	}
}

// The cache's files are the owner's alone, as the outbox's are.
func TestCacheFilesAreTheOwnersAlone(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no mode bits")
	}
	l, _ := newCache(t, CacheOptions{})
	keep(t, l.Cache("alice"), proto.OpShow, proto.ShowArgs{ID: "sf-a"}, shown("sf-a", ""))
	if err := l.Cache("alice").Put(PrimeKey, map[string]string{"you": "alice"}); err != nil {
		t.Fatal(err)
	}
	err := filepath.WalkDir(l.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		st, err := d.Info()
		if err != nil {
			return err
		}
		want := os.FileMode(0o600)
		if d.IsDir() {
			want = 0o700
		}
		if path != l.root && st.Mode().Perm() != want {
			t.Errorf("%s has mode %o, want %o", path, st.Mode().Perm(), want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// When the server was last reached is noted at most once a minute, and
// read back to the minute.
func TestLastReached(t *testing.T) {
	t.Parallel()
	l, clk := newLocal(t, outbox.Options{})
	if got := l.LastReached(); !got.IsZero() {
		t.Errorf("LastReached before any handshake = %v, want the zero time", got)
	}
	l.Touch()
	clk.t = t0.Add(30 * time.Second)
	l.Touch()
	if got := l.LastReached(); !got.Equal(t0) {
		t.Errorf("LastReached = %v, want %v: noted at most once a minute", got, t0)
	}
	clk.t = t0.Add(2 * time.Minute)
	l.Touch()
	if got := l.LastReached(); !got.Equal(clk.t) {
		t.Errorf("LastReached = %v, want %v", got, clk.t)
	}
}

func TestCachedLine(t *testing.T) {
	t.Parallel()
	got := CachedLine(t0, t0.Add(3*time.Hour+5*time.Minute))
	want := "cached: starfix cannot reach its server, so this is this machine's copy from 2026-10-13 14:02 UTC (3h old), and may be out of date"
	if got != want {
		t.Errorf("CachedLine = %q\nwant %q", got, want)
	}
}

// The reads that say what holds work back now are refused offline, as
// ready is; a copy of them would be wrong in the way that matters.
func TestRefusedOffline(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		op   string
		want bool
	}{
		{proto.OpReady, true}, {proto.OpBlocked, true}, {proto.OpList, true}, {proto.OpWho, true}, {proto.OpClaims, true},
		{proto.OpGates, true}, {proto.OpDates, true},
		{proto.OpShow, false}, {proto.OpDigest, false}, {proto.OpInbox, false},
	} {
		if got := RefusedOffline(tc.op); got != tc.want {
			t.Errorf("RefusedOffline(%q) = %v, want %v", tc.op, got, tc.want)
		}
	}
}
