package store

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/safetext"
	"github.com/ariesworx/aries/internal/secretscan"
)

// The benchmarks of this file say how the store's reads and writes grow
// with a project (docs/design/profile-ariesd.md): each runs on a
// database of 100, 1,000 and 10,000 issues, half of them closed, and of
// 50,000 when ARIES_BENCH_LARGE is set. The time is mostly Postgres's;
// the bytes and allocations are the store's and its driver's.
//
//	go test -run '^$' -bench 'Scale' -benchmem ./internal/store/
//	go test -run '^$' -bench 'Scale/issues=10000/' -benchmem ./internal/store/
//
// They are measurements to rerun, not tests: nothing is asserted of a
// time or a count.

// benchSizes are the project sizes BenchmarkScale runs at.
func benchSizes() []int {
	sizes := []int{100, 1000, 10000}
	if os.Getenv("ARIES_BENCH_LARGE") != "" {
		sizes = append(sizes, 50000)
	}
	return sizes
}

// benchTopics are the words the seeded titles are made of, so that the
// similar-title scorer finds titles that overlap.
var benchTopics = []string{"parser", "lease", "reaper", "inbox", "digest", "socket", "bridge", "telemetry", "migration", "registry",
	"cursor", "handoff", "claim", "label", "prefix", "worktree", "session", "sampler", "bucket", "watcher"}

// benchProject is what seedScale left in a database.
type benchProject struct {
	// open and closed are the ids of the open and the closed issues, and
	// long one issue whose four texts are each bodyLen bytes.
	open, closed []IssueID
	long         IssueID
}

// seedScale fills s's database with n issues, in rows written as a
// migration would write them, since n creates through the store would
// take minutes: half closed, a twentieth deferred and the rest open,
// each with a label and a body of bodyLen bytes; a tenth of the open
// ones blocked by another; one create event for each, spread over the
// last 30 days; and 200 memories. Then, through the store, 20 issues
// that another session holds with paths, so that Ready ranks by them.
func seedScale(b *testing.B, s *Store, n, bodyLen int) benchProject {
	b.Helper()
	ctx := b.Context()
	now := s.now()
	body := strings.Repeat("A line of the report, as a person writes one.\n", bodyLen/46+1)[:bodyLen]
	var p benchProject
	var blocked [][2]string
	const chunk = 500
	for lo := 0; lo < n; lo += chunk {
		hi := min(lo+chunk, n)
		var issues, labels, events []string
		var ip, lp, ep params
		for i := lo; i < hi; i++ {
			id := fmt.Sprintf("tst-b%07d", i)
			title := fmt.Sprintf("The %s drops the %s after the %s restarts, case %d",
				benchTopics[i%20], benchTopics[(i/20+7)%20], benchTopics[(i/400+13)%20], i)
			status, at := "open", now.Add(-time.Duration(n-i)*30*24*time.Hour/time.Duration(n))
			var closedAt any
			switch {
			case i%2 == 0:
				status, closedAt = "closed", at.Add(time.Minute)
				p.closed = append(p.closed, IssueID(id))
			case i%20 == 1:
				status = "deferred"
			default:
				p.open = append(p.open, IssueID(id))
				if i%10 == 3 && len(p.open) > 1 {
					blocked = append(blocked, [2]string{id, string(p.open[len(p.open)-2])})
				}
			}
			issues = append(issues, "("+ip.add(id, title, body)+", '', '', '', "+ip.add(status, i%4)+", 'task', 'bob', "+ip.add(at, at, closedAt)+", 1)")
			labels = append(labels, "("+lp.add(id, "area-"+benchTopics[i%20], at)+")")
			events = append(events, "("+ep.add(i+1, at)+", 'bob', 'sess-b', 'laptop-b', 'issue.create', "+ep.add(id, `{"status":"open"}`)+")")
		}
		err := s.write(ctx, bob, func(w *wtx) error {
			w.quiet = true
			if _, err := w.exec(ctx, `INSERT INTO issues (id, title, body, design, acceptance, notes, status, priority, type,
  created_by, created_at, updated_at, closed_at, rev) VALUES `+strings.Join(issues, ","), ip.args...); err != nil {
				return err
			}
			if _, err := w.exec(ctx, `INSERT INTO labels (issue_id, label, created_at) VALUES `+strings.Join(labels, ","), lp.args...); err != nil {
				return err
			}
			_, err := w.exec(ctx, `INSERT INTO events (seq, at, principal, session, machine, op, target, after_state) VALUES `+
				strings.Join(events, ","), ep.args...)
			return err
		})
		if err != nil {
			b.Fatalf("seed issues %d to %d: %v", lo, hi, err)
		}
	}
	for lo := 0; lo < len(blocked); lo += chunk {
		var rows []string
		var p params
		for _, e := range blocked[lo:min(lo+chunk, len(blocked))] {
			rows = append(rows, "("+p.add(e[0], e[1])+", 'blocks', 'bob', "+p.add(now)+", 1)")
		}
		if err := s.write(ctx, bob, func(w *wtx) error {
			w.quiet = true
			_, err := w.exec(ctx, `INSERT INTO deps (from_id, to_id, type, created_by, created_at, rev) VALUES `+
				strings.Join(rows, ","), p.args...)
			return err
		}); err != nil {
			b.Fatalf("seed deps: %v", err)
		}
	}
	var rows []string
	var mp params
	for i := range 200 {
		rows = append(rows, "("+mp.add(fmt.Sprintf("m%07d", i))+", 'project', '', "+mp.add(fmt.Sprintf("note-%03d", i),
			fmt.Sprintf("The %s needs the %s restarted first. %s", benchTopics[i%20], benchTopics[(i+1)%20], strings.Repeat("More of why. ", 20)),
			i%20 == 0)+", 'bob', 'bob', "+mp.add(now, now)+", 1)")
	}
	if err := s.write(ctx, bob, func(w *wtx) error {
		w.quiet = true
		_, err := w.exec(ctx, `INSERT INTO memories (id, scope, owner, mem_key, body, pinned, author, updated_by, created_at, updated_at, rev)
  VALUES `+strings.Join(rows, ","), mp.args...)
		return err
	}); err != nil {
		b.Fatalf("seed memories: %v", err)
	}
	long := strings.Repeat("A line of the report, as a person writes one.\n", maxText/46+1)[:maxText-1]
	is, err := s.CreateIssue(ctx, bob, NewIssue{Title: "The parser drops the lease after the socket restarts", Body: long, Design: long,
		Notes: long, Acceptance: "- [ ] the lease survives\n- [ ] the parser reconnects\n- [ ] a test covers it\n" + long[:maxText-200]})
	if err != nil {
		b.Fatalf("seed the long issue: %v", err)
	}
	p.long = is.ID
	for i := range 20 {
		held, err := s.CreateIssue(ctx, bob, NewIssue{Title: fmt.Sprintf("Held work %d on the %s", i, benchTopics[i]),
			Paths: []string{fmt.Sprintf("internal/%s/", benchTopics[i]), fmt.Sprintf("docs/%s.md", benchTopics[i])}})
		if err != nil {
			b.Fatalf("seed a held issue: %v", err)
		}
		holder := Actor{Principal: "carol", Session: "sess-c" + strconv.Itoa(i), Machine: "laptop-c"}
		if _, _, err := s.StartIssue(ctx, holder, held.ID, DefaultLease, false); err != nil {
			b.Fatalf("seed a claim: %v", err)
		}
	}
	return p
}

// BenchmarkScale is each of the store's reads and writes at several
// project sizes, with bodies of 400 bytes.
func BenchmarkScale(b *testing.B) {
	for _, n := range benchSizes() {
		b.Run(fmt.Sprintf("issues=%d", n), func(b *testing.B) {
			hold := &writerHold{}
			s := openStore(b, newDSN(b), Options{Tracer: hold})
			p := seedScale(b, s, n, 400)
			benchStore(b, s, p, hold)
		})
	}
}

// writerHold is a Tracer that adds up how long writes hold the store's
// writer: each attempt's time, less its wait for the writer.
type writerHold struct{ ns atomic.Int64 }

func (h *writerHold) Begin(ctx context.Context, stage Stage, _ int) (context.Context, func(Outcome)) {
	start := time.Now()
	switch stage {
	case StageAttempt:
		return ctx, func(Outcome) { h.ns.Add(int64(time.Since(start))) }
	case StageWait:
		return ctx, func(Outcome) { h.ns.Add(-int64(time.Since(start))) }
	}
	return ctx, untraced
}

// held returns the time held so far.
func (h *writerHold) held() time.Duration { return time.Duration(h.ns.Load()) }

// BenchmarkLongBodies is the reads that return issues, on 1,000 issues
// whose bodies are each 32 KiB: list, ready and blocked answer with a
// summary of each issue, and read only that; the export reads the whole
// row (list/full).
func BenchmarkLongBodies(b *testing.B) {
	s := openStore(b, newDSN(b), Options{})
	p := seedScale(b, s, 1000, 32<<10)
	ctx := b.Context()
	for _, limit := range []int{50, 500} {
		b.Run(fmt.Sprintf("list/limit=%d", limit), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := s.ListSummaries(ctx, Filter{Limit: limit}); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("list/full/limit=%d", limit), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := s.List(ctx, Filter{Limit: limit}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	b.Run("ready/limit=10", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := s.Ready(ctx, alice, 10); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("blocked/limit=50", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := s.Blocked(ctx, 50); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("get/long", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := s.GetIssue(ctx, p.long); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("update/long", func(b *testing.B) {
		// One of four 64 KiB texts replaced: the event holds the before
		// and the after of the whole issue, compacted.
		is, err := s.GetIssue(ctx, p.long)
		if err != nil {
			b.Fatal(err)
		}
		texts := [2]string{strings.Repeat("x", maxText-1), strings.Repeat("y", maxText-1)}
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			i++
			if is, err = s.UpdateIssue(ctx, bob, is.ID, is.Rev, IssuePatch{Notes: &texts[i%2]}); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// benchStore runs the store's reads and writes on s, which holds p, and
// whose writes hold tells the time of.
func benchStore(b *testing.B, s *Store, p benchProject, hold *writerHold) {
	ctx := b.Context()
	some := p.open[len(p.open)/2]
	reads := []struct {
		name string
		run  func() error
	}{
		{"get", func() error { _, err := s.GetIssue(ctx, some); return err }},
		{"list", func() error { _, err := s.ListSummaries(ctx, Filter{}); return err }},
		{"list/in_progress", func() error {
			_, err := s.ListSummaries(ctx, Filter{Statuses: []Status{StatusInProgress}, Assignee: "carol"})
			return err
		}},
		{"list/full", func() error { _, err := s.List(ctx, Filter{}); return err }},
		{"ready", func() error { _, err := s.Ready(ctx, alice, 10); return err }},
		{"ready-check", func() error {
			// What start with no id asks under the writer of the issue it
			// ranked first on a read.
			_, err := readyNow(ctx, s.r, some, s.now())
			return err
		}},
		{"blocked", func() error { _, err := s.Blocked(ctx, 50); return err }},
		{"digest", func() error { _, err := s.Digest(ctx, DigestFilter{Window: 24 * time.Hour}); return err }},
		{"similar/warm", func() error {
			_, err := s.SimilarClosed(ctx, "The parser drops the lease after a restart", "", MaxSimilar)
			return err
		}},
		{"similar/cold", func() error {
			// As after a close or a reopen: the titles are read again.
			s.similar.invalidate()
			_, err := s.SimilarClosed(ctx, "The parser drops the lease after a restart", "", MaxSimilar)
			return err
		}},
		{"recall", func() error { _, _, err := s.Recall(ctx, alice, MemoryQuery{Text: "restarted"}); return err }},
		{"prime", func() error { _, err := s.PrimeMemories(ctx, alice, 5); return err }},
		{"who", func() error { _, _, err := s.WhoPage(ctx, 0, 0); return err }},
		{"claims", func() error { _, _, err := s.ActiveClaims(ctx, 0); return err }},
	}
	for _, r := range reads {
		b.Run(r.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if err := r.run(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	body := strings.Repeat("What happens, and what should. ", 16)
	n := 0
	b.Run("create", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			n++
			if _, err := s.CreateIssue(ctx, alice, NewIssue{Title: fmt.Sprintf("Benchmark the %d of the widget reader", n), Body: body}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("comment", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := s.AddComment(ctx, alice, some, "Seen again on the build machine.", ""); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("update", func(b *testing.B) {
		is, err := s.GetIssue(ctx, some)
		if err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		for b.Loop() {
			n++
			title := fmt.Sprintf("Benchmark the %d of the widget writer", n)
			if is, err = s.UpdateIssue(ctx, alice, is.ID, is.Rev, IssuePatch{Title: &title}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("start-next", func(b *testing.B) {
		// Start with no id takes the best ready issue, by Ready's
		// ranking. Each is released again, unmeasured. writer-ms/op is
		// how long the start held the project's one writer.
		b.ReportAllocs()
		var held time.Duration
		for b.Loop() {
			before := hold.held()
			is, c, err := s.StartIssue(ctx, alice, "", DefaultLease, false)
			if err != nil {
				b.Fatal(err)
			}
			held += hold.held() - before
			b.StopTimer()
			if _, err := s.HandoffIssue(ctx, alice, is.ID, c.Epoch, HandoffNote{Note: "Not now."}, true, "", nil); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
		}
		b.ReportMetric(float64(held)/float64(b.N)/float64(time.Millisecond), "writer-ms/op")
	})
	b.Run("close", func(b *testing.B) {
		// A close makes the cached closed titles stale.
		b.ReportAllocs()
		for b.Loop() {
			b.StopTimer()
			n++
			is, err := s.CreateIssue(ctx, alice, NewIssue{Title: fmt.Sprintf("Benchmark the %d of the widget closer", n)})
			if err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			if _, err := s.CloseIssue(ctx, alice, is.ID, is.Rev, "done"); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkGo is the store's work that never reaches Postgres, on inputs at
// their limits: the validation of a 64 KiB text, the secrets lint of a
// 4 KiB memory, the tokens of a title, the scoring of 2,000 closed
// titles, the parsing of an acceptance checklist of 200 items, the JSON
// of an event's before and after for an issue with four 64 KiB texts,
// and a write's id.
func BenchmarkGo(b *testing.B) {
	text := strings.Repeat("A line of the report, as a person writes one.\n", maxText/46+1)[:maxText]
	memory := strings.Repeat("The reader waits for the writer, and says so in the log. ", DefaultLimits.MemoryBody/57+1)[:DefaultLimits.MemoryBody]
	checklist := strings.Repeat("- [ ] the lease survives a restart of the socket\n", DefaultLimits.AcceptanceItems)
	title := "The parser drops the lease after the socket restarts, case 1234"
	closed := make([]closedTitle, SimilarScan)
	for i := range closed {
		closed[i].Title = fmt.Sprintf("The %s drops the %s after the %s restarts, case %d",
			benchTopics[i%20], benchTopics[(i/20+7)%20], benchTopics[(i/400+13)%20], i)
		closed[i].tokens = titleTokens(closed[i].Title)
	}
	long := Issue{ID: "tst-long", Title: title, Body: text, Design: text, Acceptance: text, Notes: text, Status: StatusOpen, Type: "task"}
	for _, bm := range []struct {
		name string
		size int // bytes of input, for MB/s; 0 for none
		run  func()
	}{
		{"safetext/64KiB", len(text), func() { _ = safetext.ValidText(text) }},
		{"checkText/64KiB", len(text), func() { _ = checkText("body", text, maxText, false) }},
		{"secretscan/4KiB", len(memory), func() { _, _ = secretscan.Find(memory) }},
		{"titleTokens", len(title), func() { _ = titleTokens(title) }},
		{"similar/score-2000", 0, func() {
			want := titleTokens(title)
			for _, t := range closed {
				_, _ = overlap(want, t.tokens)
			}
		}},
		{"parseAcceptance/200", len(checklist), func() { _ = parseAcceptance(checklist) }},
		{"eventState/issue-4x64KiB", 4 * len(text), func() {
			if _, err := jsonOrNull(long); err != nil {
				b.Fatal(err)
			}
		}},
		{"eventState/issue-small", 0, func() {
			if _, err := jsonOrNull(Issue{ID: "tst-small", Title: title, Body: text[:400], Status: StatusOpen, Type: "task"}); err != nil {
				b.Fatal(err)
			}
		}},
		{"idemHash/issue-small", 0, func() {
			if _, err := idemHash("create", NewIssue{Title: title, Body: text[:400]}); err != nil {
				b.Fatal(err)
			}
		}},
		{"writeID", 0, func() { _ = randomInt63() }},
	} {
		b.Run(bm.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(bm.size))
			for b.Loop() {
				bm.run()
			}
		})
	}
}

// BenchmarkSimilarCache says what the similar-title cache holds: the
// heap a store gains when it has read the SimilarScan most recently
// closed titles, reported as bytes for each title.
func BenchmarkSimilarCache(b *testing.B) {
	s := openStore(b, newDSN(b), Options{})
	seedScale(b, s, 2*SimilarScan, 100)
	ctx := b.Context()
	var held float64
	for b.Loop() {
		s.similar.mu.Lock()
		s.similar.rows, s.similar.built = nil, 0
		s.similar.mu.Unlock()
		before := heapNow()
		if _, err := s.SimilarClosed(ctx, "The parser drops the lease", "", MaxSimilar); err != nil {
			b.Fatal(err)
		}
		held += float64(heapNow() - before)
	}
	s.similar.mu.Lock()
	titles := len(s.similar.rows)
	s.similar.mu.Unlock()
	b.ReportMetric(held/float64(b.N), "heap-B/cache")
	b.ReportMetric(held/float64(b.N)/float64(max(titles, 1)), "heap-B/title")
	b.ReportMetric(float64(titles), "titles")
}

// BenchmarkCommit takes a write apart: a transaction that changes
// nothing, one that updates a row and records no event, one that
// inserts a row and records no event, one that only appends an event,
// and an update with its event, which is what a small write is. The
// time is Postgres's; the benchmark says which statement of a write
// the commit is paying for.
func BenchmarkCommit(b *testing.B) {
	s := openStore(b, newDSN(b), Options{})
	ctx := b.Context()
	is, err := s.CreateIssue(ctx, alice, NewIssue{Title: "issue"})
	if err != nil {
		b.Fatal(err)
	}
	n := 0
	update := func(w *wtx) error {
		_, err := w.exec(ctx, `UPDATE issues SET rev = rev + 1 WHERE id = $1`, string(is.ID))
		return err
	}
	insert := func(w *wtx) error {
		n++
		_, err := w.exec(ctx, `INSERT INTO labels (issue_id, label, created_at) VALUES ($1, $2, $3)`, string(is.ID), fmt.Sprintf("label-%d", n), w.now)
		return err
	}
	event := func(w *wtx) error { return w.event(ctx, OpIssueUpdate, string(is.ID), nil, map[string]int{"n": n}) }
	for _, bm := range []struct {
		name  string
		quiet bool
		steps []func(*wtx) error
	}{
		{"nothing", true, nil},
		{"update", true, []func(*wtx) error{update}},
		{"insert", true, []func(*wtx) error{insert}},
		{"event", false, []func(*wtx) error{event}},
		{"update+event", false, []func(*wtx) error{update, event}},
	} {
		b.Run(bm.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				err := s.write(ctx, alice, func(w *wtx) error {
					w.quiet = bm.quiet
					for _, step := range bm.steps {
						if err := step(w); err != nil {
							return err
						}
					}
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// heapNow is the bytes of live heap objects after a collection: two,
// so that what the first one's finalizers freed is counted as free.
func heapNow() int64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc) //nolint:gosec // a heap is far below 2^63 bytes
}
