package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/brief"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/version"
)

// A dry run judges every ready issue and says what it would start, skip
// (with the rule) and queue, and claims, makes, launches, posts and saves
// nothing.
func TestDryRunStartsNothing(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.tracker.add("ar-1", "feature", "model:opus")
	w.tracker.add("ar-2", "bug", "model:sonnet", "human")
	w.tracker.add("ar-3", "epic", "model:opus")
	w.tracker.add("ar-4", "task")
	w.tracker.add("ar-5", "task", "model:haiku")
	w.tracker.add("ar-6", "task", "model:opus")
	w.slots.held = 2 // two of four slots are others'
	cfg := DefaultConfig()
	cfg.DryRun = true
	d, l := w.dispatcher(cfg)
	if err := d.Wake(t.Context(), WakeStart); err != nil {
		t.Fatal(err)
	}
	if got := w.calls.all(); len(got) != 0 {
		t.Errorf("a dry run asked the stand-ins %q", got)
	}
	if len(l.sessions()) != 0 || w.saved.Load() != 0 || len(d.State.Runs) != 0 {
		t.Errorf("a dry run launched %d, saved %d times, recorded %d runs", len(l.sessions()), w.saved.Load(), len(d.State.Runs))
	}
	out := w.out.String()
	for _, want := range []string{
		"wake start",
		"would start ar-1 (model opus)",
		"skip ar-2 [human]: ",
		"skip ar-3 [epic]: ",
		"skip ar-4 [no-model]: ",
		"would start ar-5 (model haiku)",
		"queue ar-6 (model opus) [slots]: ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry run's output lacks %q:\n%s", want, out)
		}
	}
	// The log is a record of what happened, and a dry run makes nothing
	// happen: it writes no line, not even a marked one.
	if w.log.String() != "" {
		t.Errorf("a dry run wrote to the log:\n%s", w.log.String())
	}
}

// A filter reaches the issues past the first twenty of ready, and the
// dry run says how many ready issues it judged.
func TestDryRunJudgesEveryReadyIssue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		total   int
		matchAt int // 1-based place of the one Haiku issue, 0 for none
		limit   int // 0 takes the default
		want    []string
		not     []string
	}{
		{name: "match on the first page", total: 25, matchAt: 3,
			want: []string{"would start ar-3 (model haiku)", "judged 25 ready issues"}},
		{name: "match on the second page", total: 25, matchAt: 23,
			want: []string{"would start ar-23 (model haiku)", "judged 25 ready issues"}},
		{name: "no match", total: 25,
			want: []string{"judged 25 ready issues"}, not: []string{"would start"}},
		{name: "no ready issue", total: 0, want: []string{"judged 0 ready issues"}},
		{name: "past the limit one wake reads", total: 25, matchAt: 23, limit: 20,
			want: []string{"judged 20 ready issues (the most a wake reads)"}, not: []string{"would start"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			for i := 1; i <= tc.total; i++ {
				label := "model:sonnet"
				if i == tc.matchAt {
					label = "model:haiku"
				}
				w.tracker.add(fmt.Sprintf("ar-%d", i), "task", label)
			}
			cfg := DefaultConfig()
			cfg.DryRun = true
			cfg.Filter = Filter{Models: []string{"haiku"}}
			if tc.limit > 0 {
				cfg.ReadyLimit = tc.limit
			}
			d, _ := w.dispatcher(cfg)
			if err := d.Wake(t.Context(), WakeStart); err != nil {
				t.Fatal(err)
			}
			out := w.out.String()
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("the output lacks %q:\n%s", want, out)
				}
			}
			for _, not := range tc.not {
				if strings.Contains(out, not) {
					t.Errorf("the output has %q:\n%s", not, out)
				}
			}
		})
	}
}

// A dry run says what holds it, and judges nothing it could not start.
func TestDryRunSaysWhatHolds(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.tracker.add("ar-1", "feature", "model:opus")
	w.tracker.opus = DefaultConfig().OpusCap + 5
	cfg := DefaultConfig()
	cfg.DryRun = true
	d, _ := w.dispatcher(cfg)
	if err := d.Wake(t.Context(), WakeTimer); err != nil {
		t.Fatal(err)
	}
	out := w.out.String()
	if !strings.Contains(out, "hold [budget]: ") || strings.Contains(out, "would start") {
		t.Errorf("past the cap, the dry run printed:\n%s", out)
	}
}

// A run's environment carries the Postgres the dispatcher found first on
// PATH, and ARIES_REQUIRE_POSTGRES=1, so a test that needs Postgres fails
// instead of skipping (aries-xphid6sx).
func TestRunEnvCarriesPostgres(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.tracker.add("ar-1", "feature", "model:opus")
	d, l := w.dispatcher(DefaultConfig())
	d.FindPostgres = func() string { return "/opt/pg/bin" }
	if err := d.Wake(t.Context(), WakeStart); err != nil {
		t.Fatal(err)
	}
	sp := l.spec(l.sessions()[0])
	for _, want := range []string{"PATH=/opt/pg/bin" + string(os.PathListSeparator) + "/usr/bin", "ARIES_REQUIRE_POSTGRES=1"} {
		if !slices.Contains(sp.Env, want) {
			t.Errorf("the run's environment lacks %s:\n%s", want, strings.Join(sp.Env, "\n"))
		}
	}
	if strings.Contains(sp.Brief, NoPostgresNote) {
		t.Errorf("the brief carries the no-Postgres note though Postgres was found")
	}
}

// With no Postgres the run still starts, with a note in its brief and
// ARIES_REQUIRE_POSTGRES=1 and its PATH unchanged, and the dispatcher says
// so.
func TestRunWithoutPostgresStartsWithANote(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.tracker.add("ar-1", "feature", "model:opus")
	d, l := w.dispatcher(DefaultConfig())
	d.FindPostgres = func() string { return "" }
	if err := d.Wake(t.Context(), WakeStart); err != nil {
		t.Fatal(err)
	}
	sessions := l.sessions()
	if len(sessions) != 1 {
		t.Fatalf("launched %d runs; want 1", len(sessions))
	}
	sp := l.spec(sessions[0])
	if !strings.Contains(sp.Brief, NoPostgresNote) {
		t.Errorf("the brief lacks the note:\n%s", sp.Brief)
	}
	for _, want := range []string{"PATH=/usr/bin", "ARIES_REQUIRE_POSTGRES=1"} {
		if !slices.Contains(sp.Env, want) {
			t.Errorf("the run's environment lacks %s", want)
		}
	}
	if !strings.Contains(w.out.String(), "no Postgres found for ar-1") {
		t.Errorf("the dispatcher did not say so:\n%s", w.out.String())
	}
}

// FindPostgres looks where scripts/install-postgres.sh puts it.
func TestFindPostgres(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := []string{"HOME=" + home, "ARIES_POSTGRES_PACKAGED=" + filepath.Join(home, "none")}
	if got := FindPostgres(env); got != "" {
		t.Fatalf("found %q in an empty home", got)
	}
	bin := filepath.Join(home, ".cache", "aries", "postgres-"+version.Postgres, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"postgres", "initdb"} {
		if err := os.WriteFile(filepath.Join(bin, p), []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // a stand-in program must be executable
			t.Fatal(err)
		}
	}
	if got := FindPostgres(env); got != bin {
		t.Errorf("FindPostgres = %q; want %q", got, bin)
	}
}

// The dispatcher claims an issue under the run's session before it makes
// the worktree, launches the run in it and posts the brief (design §4,
// §7). The run gets the session and the key in its environment, and the
// brief on stdin, not in its environment or argv.
func TestStartClaimsBeforeLaunch(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.tracker.add("ar-1", "feature", "model:opus")
	d, l := w.dispatcher(DefaultConfig())
	if err := d.Wake(t.Context(), WakeStart); err != nil {
		t.Fatal(err)
	}
	sessions := l.sessions()
	if len(sessions) != 1 {
		t.Fatalf("launched %d runs; want 1: %q", len(sessions), w.calls.all())
	}
	s := sessions[0]
	start, add, launch, posted := w.calls.index("start ar-1 "+s+" 10m0s"), w.calls.index("worktree add "), w.calls.index("launch ar-1 "+s), w.calls.index("comment ar-1 "+s)
	if start < 0 || add < start || launch < add || posted < launch {
		t.Fatalf("want start, worktree, launch, brief in order; got %q", w.calls.all())
	}
	sp := l.spec(s)
	if sp.Model != "claude-opus-5-5" || sp.Dir != "/repo/.aries-dispatch/worktrees/ar-1" || sp.Branch != "feature/ar-1-title" {
		t.Errorf("spec = %+v", sp)
	}
	env := strings.Join(sp.Env, "\n")
	for _, want := range []string{"ARIES_SESSION=" + s, "ANTHROPIC_API_KEY=sk-test-not-a-key", "PATH=/usr/bin",
		"GOTOOLCHAIN=local", "GOFLAGS=-mod=readonly"} {
		if !slices.Contains(sp.Env, want) {
			t.Errorf("the run's environment lacks %s:\n%s", want, env)
		}
	}
	for _, gone := range []string{"ARIES_SESSION=outer", "outer-key", "CLAUDE_CODE_SESSION_ID", "CLAUDECODE"} {
		if strings.Contains(env, gone) {
			t.Errorf("the run's environment keeps %s:\n%s", gone, env)
		}
	}
	if strings.Contains(env, "Body of") || !strings.Contains(sp.Brief, "Body of ar-1") {
		t.Errorf("the brief is not on stdin alone")
	}
	comment := w.tracker.comments["ar-1"][0]
	first, rest, _ := strings.Cut(comment, "\n")
	if first != "brief: harness=claude-code model=claude-opus-5-5 effort=default session="+s+" template="+d.Template.Name() || strings.TrimPrefix(rest, "\n") != sp.Brief {
		t.Errorf("the brief: comment is\n%s\nfor the brief\n%s", comment, sp.Brief)
	}
	// The comment parses as aries outcome reads it, with every field
	// filled (aries-pk72yzeh), and the log's hash is the record's.
	rec, text, ok := brief.ParseComment(comment)
	if !ok || rec != (brief.Record{Harness: "claude-code", Model: "claude-opus-5-5", Effort: "default", Session: s, Template: d.Template.Name()}) {
		t.Errorf("the comment parses to %+v", rec)
	}
	var logged struct {
		Event string `json:"event"`
		Brief string `json:"brief_sha256"`
	}
	for l := range strings.SplitSeq(w.log.String(), "\n") {
		if json.Unmarshal([]byte(l), &logged) == nil && logged.Event == "start" {
			break
		}
	}
	if logged.Brief == "" || logged.Brief != brief.Hash(text) {
		t.Errorf("the log's brief hash %q is not the record's %q", logged.Brief, brief.Hash(text))
	}
	if strings.Contains(comment, "sk-test") {
		t.Error("the brief: comment holds the key")
	}
	if len(d.State.Runs) != 1 || d.State.Runs[0].Session != s || d.State.Runs[0].PID == 0 || w.saved.Load() == 0 {
		t.Errorf("state = %+v, saved %d", d.State.Runs, w.saved.Load())
	}
}

// A second dispatcher that read the issue before the first claimed it is
// refused the claim and moves on to the next issue; no issue is launched
// twice.
func TestSecondDispatcherRefused(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.tracker.add("ar-1", "feature", "model:opus")
	w.tracker.add("ar-2", "feature", "model:opus")
	d1, l1 := w.dispatcher(DefaultConfig())
	if err := d1.Wake(t.Context(), WakeStart); err != nil {
		t.Fatal(err)
	}
	w.tracker.add("ar-3", "feature", "model:opus")
	w.tracker.mu.Lock()
	w.tracker.stale = true
	w.tracker.mu.Unlock()
	// Ready and show read before the first dispatcher's starts.
	d2, l2 := w.dispatcher(DefaultConfig())
	d2.Tracker = staleReady{w.tracker, []string{"ar-1", "ar-2", "ar-3"}}
	if err := d2.Wake(t.Context(), WakeStart); err != nil {
		t.Fatal(err)
	}
	if n := len(l1.sessions()); n != 2 {
		t.Errorf("the first dispatcher launched %d; want 2", n)
	}
	if got := l2.sessions(); len(got) != 1 || l2.spec(got[0]).Issue != "ar-3" {
		t.Errorf("the second dispatcher launched %q; want ar-3 alone", got)
	}
	for _, id := range []string{"ar-1", "ar-2", "ar-3"} {
		if n := w.calls.count("launch " + id + " "); n != 1 {
			t.Errorf("%s launched %d times", id, n)
		}
	}
	out := w.out.String()
	if !strings.Contains(out, "skip ar-1 [held]: ") || !strings.Contains(out, "skip ar-2 [held]: ") {
		t.Errorf("the second dispatcher did not say it was refused:\n%s", out)
	}
}

// staleReady is a tracker whose ready is the list given, as read before
// another dispatcher's claims.
type staleReady struct {
	*fakeTracker
	ids []string
}

func (s staleReady) Ready(context.Context, int) ([]proto.Summary, error) {
	var out []proto.Summary
	for _, id := range s.ids {
		out = append(out, proto.Summary{ID: id, Status: "open"})
	}
	return out, nil
}

// Two dispatchers waking at once, over and over, never launch one issue
// twice, never run more than the lock's capacity between them, and never
// leave a claim behind a launch the lock refused.
func TestConcurrentDispatchers(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for i := range 12 {
		w.tracker.add(fmt.Sprintf("ar-%02d", i), "task", "model:sonnet")
	}
	w.tracker.mu.Lock()
	w.tracker.stale = true
	w.tracker.mu.Unlock()
	var ds []*Dispatcher
	var ls []*fakeLauncher
	for range 2 {
		d, l := w.dispatcher(DefaultConfig())
		ds, ls = append(ds, d), append(ls, l)
	}
	ctx := t.Context()
	var wg sync.WaitGroup
	for _, d := range ds {
		wg.Go(func() {
			for range 5 {
				if err := d.Wake(ctx, WakeTimer); err != nil {
					t.Error(err)
				}
				d.reapReady(ctx)
			}
		})
	}
	wg.Wait()
	for _, d := range ds {
		d.reapReady(ctx)
	}
	launched := map[string]int{}
	live := 0
	for _, l := range ls {
		for _, s := range l.sessions() {
			sp := l.spec(s)
			if p := l.proc(s); p.slots != nil && p.live() {
				launched[sp.Issue]++
				live++
				if got := w.tracker.claim(sp.Issue); got != s {
					t.Errorf("%s runs in session %s but %q holds its claim", sp.Issue, s, got)
				}
			}
		}
	}
	for id, n := range launched {
		if n > 1 {
			t.Errorf("%s runs %d times", id, n)
		}
	}
	if w.slots.max > w.slots.capacity || live != w.slots.capacity {
		t.Errorf("at most %d ran at once and %d run now; want %d at most and now", w.slots.max, live, w.slots.capacity)
	}
	w.tracker.mu.Lock()
	defer w.tracker.mu.Unlock()
	if len(w.tracker.claims) != live {
		t.Errorf("%d claims for %d live runs: %v", len(w.tracker.claims), live, w.tracker.claims)
	}
}

// A launch that fails releases the claim with a handoff saying why
// (design §4), and the next issue is tried.
func TestLaunchFailureReleases(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(*world, *fakeLauncher)
		why   string
	}{
		{name: "worktree", setup: func(w *world, _ *fakeLauncher) { w.git.failAdd = errors.New("fatal: a branch named x already exists") }, why: "worktree"},
		{name: "launch", setup: func(_ *world, l *fakeLauncher) { l.fail = errors.New("exec: claude: not found") }, why: "launch"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.tracker.add("ar-1", "feature", "model:opus")
			d, l := w.dispatcher(DefaultConfig())
			tc.setup(w, l)
			if err := d.Wake(t.Context(), WakeStart); err != nil {
				t.Fatal(err)
			}
			if w.tracker.claim("ar-1") != "" {
				t.Errorf("the claim was kept: %q", w.calls.all())
			}
			h := w.calls.index("handoff ar-1 ")
			if h < 0 || !strings.Contains(w.calls.all()[h], "release=true") || !strings.Contains(w.calls.all()[h], tc.why) {
				t.Errorf("no releasing handoff saying %s: %q", tc.why, w.calls.all())
			}
			if !strings.Contains(w.out.String(), "skip ar-1 [launch]: ") {
				t.Errorf("output:\n%s", w.out.String())
			}
			if len(d.State.Runs) != 0 {
				t.Errorf("a run that never started is recorded: %+v", d.State.Runs[0])
			}
		})
	}
}

// A launch the lock refused (another dispatcher took the slot) ends the
// run at once; its claim is released, and it counts toward no stop.
func TestLockRefusedReleases(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.tracker.add("ar-1", "feature", "model:opus")
	d, l := w.dispatcher(DefaultConfig())
	w.slots.capacity = 0 // full by the time the run asks
	d.Tracker = freeSlots{w.tracker}
	if err := d.Wake(t.Context(), WakeStart); err != nil {
		t.Fatal(err)
	}
	if len(l.sessions()) != 1 {
		t.Fatalf("launched %d", len(l.sessions()))
	}
	d.reapReady(t.Context())
	r := d.State.Runs[0]
	if r.Outcome != OutcomeRefused || r.Ended.IsZero() {
		t.Errorf("run = %+v", r)
	}
	if w.tracker.claim("ar-1") != "" || d.State.Failures != 0 || d.State.Stopped != "" {
		t.Errorf("claim %q, failures %d, stopped %q", w.tracker.claim("ar-1"), d.State.Failures, d.State.Stopped)
	}
}

// freeSlots reports every slot free, as a lock list read before another
// dispatcher's launch would.
type freeSlots struct{ *fakeTracker }

func (freeSlots) Slots(context.Context) (int, error) { return 0, nil }

// Past the day's cap the dispatcher starts nothing, says so once, and
// lets running runs end (design §6).
func TestBudgetStop(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.tracker.add("ar-1", "feature", "model:opus")
	d, l := w.dispatcher(DefaultConfig())
	if err := d.Wake(t.Context(), WakeStart); err != nil {
		t.Fatal(err)
	}
	w.tracker.add("ar-2", "feature", "model:opus")
	w.tracker.opus = DefaultConfig().OpusCap
	for range 2 {
		if err := d.Wake(t.Context(), WakeTimer); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(l.sessions()); n != 1 {
		t.Errorf("launched %d past the cap; want only the first", n)
	}
	if w.calls.count("interrupt ") != 0 {
		t.Error("the budget stop interrupted a run")
	}
	if n := strings.Count(w.log.String(), `"event":"stop"`); n != 1 {
		t.Errorf("the stop was logged %d times; want once:\n%s", n, w.log.String())
	}
	if d.State.Budget.Output != DefaultConfig().OpusCap || d.State.Budget.Day != "2026-10-20" {
		t.Errorf("budget = %+v", d.State.Budget)
	}
	// A new UTC day starts again.
	w.now = w.now.Add(24 * time.Hour)
	w.tracker.opus = 0
	if err := d.Wake(t.Context(), WakeTimer); err != nil {
		t.Fatal(err)
	}
	if n := len(l.sessions()); n != 2 {
		t.Errorf("launched %d on the next day; want 2", n)
	}
}

// Two CI failures in a row on PRs its runs opened stop it; a green one
// between resets the count. Each PR's result counts once.
func TestTwoFailureStop(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		checks  []string
		stopped bool
	}{
		{name: "two in a row", checks: []string{ChecksFail, ChecksFail}, stopped: true},
		{name: "a green between", checks: []string{ChecksFail, ChecksPass, ChecksFail}, stopped: false},
		{name: "pending counts nothing", checks: []string{ChecksFail, ChecksPending}, stopped: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			for i := range tc.checks {
				w.tracker.add(fmt.Sprintf("ar-%d", i), "task", "model:opus")
			}
			d, l := w.dispatcher(DefaultConfig())
			ctx := t.Context()
			if err := d.Wake(ctx, WakeStart); err != nil {
				t.Fatal(err)
			}
			for i, c := range tc.checks {
				r := d.State.Runs[i]
				l.proc(r.Session).end(Exit{Code: 0, Outcome: OutcomeOK})
				d.reapReady(ctx)
				w.forge.set(r.Branch, PR{Number: 10 + i, URL: fmt.Sprintf("https://example.com/pr/%d", 10+i), State: "OPEN", Checks: c})
				if err := d.Wake(ctx, WakeTimer); err != nil {
					t.Fatal(err)
				}
				// Read again: a result counts once.
				if err := d.Wake(ctx, WakeTimer); err != nil {
					t.Fatal(err)
				}
			}
			if got := d.State.Stopped == HoldFailures || d.State.Failures >= 2; got != tc.stopped {
				t.Fatalf("stopped = %v (failures %d); want %v", got, d.State.Failures, tc.stopped)
			}
			w.tracker.add("ar-next", "task", "model:opus")
			before := len(l.sessions())
			if err := d.Wake(ctx, WakeTimer); err != nil {
				t.Fatal(err)
			}
			if started := len(l.sessions()) > before; started == tc.stopped {
				t.Errorf("started more: %v; stopped %v\n%s", started, tc.stopped, w.out.String())
			}
		})
	}
}

// The first run that fails for auth stops the dispatcher at once (design
// §6): every later run would fail the same way.
func TestAuthStop(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.tracker.add("ar-1", "feature", "model:opus")
	d, l := w.dispatcher(DefaultConfig())
	ctx := t.Context()
	if err := d.Wake(ctx, WakeStart); err != nil {
		t.Fatal(err)
	}
	r := d.State.Runs[0]
	l.proc(r.Session).end(Exit{Code: 1, Outcome: OutcomeAuth})
	d.reapReady(ctx)
	if d.State.Stopped != HoldAuth {
		t.Fatalf("stopped = %q", d.State.Stopped)
	}
	if w.tracker.claim("ar-1") != "" {
		t.Error("the claim of a run that failed for auth was kept")
	}
	w.tracker.add("ar-2", "feature", "model:opus")
	if err := d.Wake(ctx, WakeTimer); err != nil {
		t.Fatal(err)
	}
	if len(l.sessions()) != 1 || !strings.Contains(w.out.String(), "hold [auth]: ") {
		t.Errorf("after an auth failure:\n%s", w.out.String())
	}
}

// A run that ends without letting its claim go has it released with a
// handoff that says how it ended; one that handed off is left alone.
func TestRunEndReleasesClaim(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		exit     Exit
		handoff  bool // the run handed off itself, releasing the claim
		released bool
	}{
		{name: "failed holding the claim", exit: Exit{Code: 1, Outcome: OutcomeFailed}, released: true},
		{name: "ok holding the claim", exit: Exit{Code: 0, Outcome: OutcomeOK}, released: false},
		{name: "failed after its handoff", exit: Exit{Code: 1, Outcome: OutcomeFailed}, handoff: true, released: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.tracker.add("ar-1", "feature", "model:opus")
			d, l := w.dispatcher(DefaultConfig())
			ctx := t.Context()
			if err := d.Wake(ctx, WakeStart); err != nil {
				t.Fatal(err)
			}
			r := d.State.Runs[0]
			if tc.handoff {
				_ = w.tracker.Handoff(ctx, "ar-1", r.Session, "PR opened", true)
			}
			before := w.calls.count("handoff ")
			l.proc(r.Session).end(tc.exit)
			d.reapReady(ctx)
			got := w.calls.count("handoff ") > before
			if got != tc.released {
				t.Errorf("released = %v; want %v: %q", got, tc.released, w.calls.all())
			}
			if r.Ended.IsZero() || r.Outcome != tc.exit.Outcome || r.Code != tc.exit.Code {
				t.Errorf("run = %+v", r)
			}
		})
	}
}

// A run past its wall-clock limit is interrupted, and terminated if it
// has not ended after the grace (design §6). A stop --now does the same
// to every run at once.
func TestRunLimitAndStopNow(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.tracker.add("ar-1", "feature", "model:opus")
	w.tracker.add("ar-2", "feature", "model:opus")
	cfg := DefaultConfig()
	cfg.Capacity = 1
	w.slots.capacity = 1
	d, l := w.dispatcher(cfg)
	ctx := t.Context()
	if err := d.Wake(ctx, WakeStart); err != nil {
		t.Fatal(err)
	}
	r := d.State.Runs[0]
	d.enforce(ctx)
	if w.calls.count("interrupt ") != 0 {
		t.Fatal("interrupted a run within its limit")
	}
	w.now = w.now.Add(cfg.RunLimit + time.Second)
	d.enforce(ctx)
	d.enforce(ctx)
	if w.calls.count("interrupt "+r.Session) != 1 || w.calls.count("terminate ") != 0 {
		t.Fatalf("at the limit: %q", w.calls.all())
	}
	w.now = w.now.Add(cfg.StopGrace + time.Second)
	d.enforce(ctx)
	if w.calls.count("terminate "+r.Session) != 1 {
		t.Fatalf("after the grace: %q", w.calls.all())
	}
	d.reapReady(ctx)
	if r.Outcome != OutcomeTimeout {
		t.Errorf("outcome = %q; want %q", r.Outcome, OutcomeTimeout)
	}

	// stop --now: interrupt every live run, start nothing.
	if err := d.Wake(ctx, WakeTimer); err != nil {
		t.Fatal(err)
	}
	r2 := d.State.Runs[1]
	l.proc(r2.Session).onInterrupt = &Exit{Code: 130, Outcome: OutcomeFailed}
	w.stop = StopNow
	d.enforce(ctx)
	d.reapReady(ctx)
	if w.calls.count("interrupt "+r2.Session) != 1 || r2.Outcome != OutcomeStopped {
		t.Errorf("stop --now: run %+v, calls %q", r2, w.calls.all())
	}
	if !d.Done() {
		t.Error("with a stop asked and no run live, the dispatcher is not done")
	}
}

// stop (without --now) starts nothing more and lets runs end: the
// dispatcher is done once the last has.
func TestStopDrain(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.tracker.add("ar-1", "feature", "model:opus")
	d, l := w.dispatcher(DefaultConfig())
	ctx := t.Context()
	if err := d.Wake(ctx, WakeStart); err != nil {
		t.Fatal(err)
	}
	w.tracker.add("ar-2", "feature", "model:opus")
	w.stop = StopDrain
	if err := d.Wake(ctx, WakeTimer); err != nil {
		t.Fatal(err)
	}
	d.enforce(ctx)
	if len(l.sessions()) != 1 || w.calls.count("interrupt ") != 0 || d.Done() {
		t.Fatalf("draining: launched %d, %q, done %v", len(l.sessions()), w.calls.all(), d.Done())
	}
	l.proc(d.State.Runs[0].Session).end(Exit{Outcome: OutcomeOK})
	d.reapReady(ctx)
	if !d.Done() {
		t.Error("drained, the dispatcher is not done")
	}
}

// The pause lock stops every dispatcher starting work (design §12), and
// a server it cannot reach starts nothing either.
func TestPauseAndUnreachable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		set  func(*fakeTracker)
		rule string
	}{
		{name: "pause", set: func(f *fakeTracker) { f.paused = true }, rule: HoldPause},
		{name: "unreachable", set: func(f *fakeTracker) { f.down = true }, rule: HoldUnreachable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.tracker.add("ar-1", "feature", "model:opus")
			tc.set(w.tracker)
			d, l := w.dispatcher(DefaultConfig())
			if err := d.Wake(t.Context(), WakeStart); err != nil {
				t.Fatal(err)
			}
			if len(l.sessions()) != 0 || !strings.Contains(w.out.String(), "hold ["+tc.rule+"]: ") {
				t.Errorf("launched %d:\n%s", len(l.sessions()), w.out.String())
			}
		})
	}
}

// Cleanup on close (design §9): only an ended run of its own whose issue
// is closed; the worktree without --force, the branch only when its PR
// merged; a dirty worktree is left and reported.
func TestCleanup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		status   string
		pr       *PR
		dirty    bool
		live     bool
		removed  bool
		branch   bool
		reported string
	}{
		{name: "closed and merged", status: "closed", pr: &PR{State: "MERGED"}, removed: true, branch: true},
		{name: "closed, PR not merged", status: "closed", pr: &PR{State: "CLOSED"}, removed: true},
		{name: "closed, no PR", status: "closed", removed: true},
		{name: "open", status: "in_progress", pr: &PR{State: "MERGED"}},
		{name: "a live run", status: "closed", pr: &PR{State: "MERGED"}, live: true},
		{name: "dirty", status: "closed", pr: &PR{State: "MERGED"}, dirty: true, reported: "left"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.tracker.add("ar-1", "feature", "model:opus")
			d, l := w.dispatcher(DefaultConfig())
			ctx := t.Context()
			if err := d.Wake(ctx, WakeStart); err != nil {
				t.Fatal(err)
			}
			r := d.State.Runs[0]
			if !tc.live {
				l.proc(r.Session).end(Exit{Outcome: OutcomeOK})
				d.reapReady(ctx)
			}
			w.tracker.set("ar-1", func(sr *proto.ShowResult) { sr.Issue.Status = tc.status })
			if tc.pr != nil {
				w.forge.set(r.Branch, *tc.pr)
			}
			w.git.dirty[r.Worktree] = tc.dirty
			if err := d.Wake(ctx, WakeTimer); err != nil {
				t.Fatal(err)
			}
			removed := w.calls.count("worktree remove "+r.Worktree) > 0 && !tc.dirty
			if removed != tc.removed {
				t.Errorf("removed = %v; want %v: %q", removed, tc.removed, w.calls.all())
			}
			if got := w.calls.count("branch delete "+r.Branch) > 0; got != tc.branch {
				t.Errorf("branch deleted = %v; want %v", got, tc.branch)
			}
			if r.Cleaned != tc.removed {
				t.Errorf("cleaned = %v", r.Cleaned)
			}
			if tc.reported != "" && !strings.Contains(w.out.String(), tc.reported) {
				t.Errorf("not reported:\n%s", w.out.String())
			}
			// Once cleaned, never again.
			n := w.calls.count("worktree remove ")
			if err := d.Wake(ctx, WakeTimer); err != nil {
				t.Fatal(err)
			}
			if tc.removed && w.calls.count("worktree remove ") != n {
				t.Error("cleaned twice")
			}
		})
	}
}

// Cleanup never touches a worktree outside its own directory, whatever
// the state file says.
func TestCleanupOnlyItsOwn(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.tracker.add("ar-1", "feature", "model:opus")
	w.tracker.set("ar-1", func(sr *proto.ShowResult) { sr.Issue.Status = "closed" })
	d, _ := w.dispatcher(DefaultConfig())
	d.State.Runs = []*Run{{Issue: "ar-1", Session: "s", Worktree: "/repo/.claude/worktrees/ar-1", Branch: "b",
		Started: w.now, Ended: w.now, Outcome: OutcomeOK}}
	if err := d.Wake(t.Context(), WakeTimer); err != nil {
		t.Fatal(err)
	}
	if w.calls.count("worktree remove ") != 0 {
		t.Errorf("removed a foreign worktree: %q", w.calls.all())
	}
}

// On restart it adopts runs whose process still runs in their session and
// marks the rest ended (design §9); an adopted run's issue is not started
// again.
func TestAdopt(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.tracker.add("ar-1", "feature", "model:opus")
	w.tracker.add("ar-2", "feature", "model:opus")
	d, l := w.dispatcher(DefaultConfig())
	d.State.Runs = []*Run{
		{Issue: "ar-1", Session: "s1", PID: 41, Worktree: "/repo/.aries-dispatch/worktrees/ar-1", Started: w.now},
		{Issue: "ar-2", Session: "s2", PID: 42, Worktree: "/repo/.aries-dispatch/worktrees/ar-2", Started: w.now},
	}
	w.tracker.claims["ar-1"] = "s1"
	l.alive["s1"] = 41
	d.Adopt(t.Context())
	if !d.State.Runs[0].Ended.IsZero() || d.State.Runs[1].Ended.IsZero() || d.State.Runs[1].Outcome != OutcomeLost {
		t.Fatalf("runs = %+v %+v", d.State.Runs[0], d.State.Runs[1])
	}
	w.tracker.set("ar-1", func(sr *proto.ShowResult) { sr.Issue.Status = "open" })
	w.tracker.claims = map[string]string{}
	if err := d.Wake(t.Context(), WakeStart); err != nil {
		t.Fatal(err)
	}
	if n := w.calls.count("launch ar-1 "); n != 0 {
		t.Errorf("started ar-1 again while its adopted run lives")
	}
	if n := w.calls.count("launch ar-2 "); n != 1 {
		t.Errorf("did not start ar-2, whose run ended: %q", w.calls.all())
	}
}
