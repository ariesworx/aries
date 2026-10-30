package dispatch

import (
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// sweepWorld is a world whose issue ar-1 was handed off done by a run of
// the dispatcher's own, which ended, leaving PR 7 open on its branch.
func sweepWorld(t *testing.T) (*world, *Dispatcher, *fakeLauncher, *Run) {
	t.Helper()
	w := newWorld(t)
	w.tracker.add("ar-1", "feature", "model:sonnet")
	w.tracker.set("ar-1", func(sr *proto.ShowResult) {
		sr.Issue.Status = "in_progress"
		sr.Handoff = &proto.Handoff{HandoffFields: proto.HandoffFields{State: "done", Branch: "feature/ar-1-title"}}
	})
	d, l := w.dispatcher(DefaultConfig())
	run := &Run{Issue: "ar-1", Session: "old-session", Harness: HarnessClaudeCode, Model: "claude-sonnet-5-5",
		Worktree: "/repo/.aries-dispatch/worktrees/ar-1", Branch: "feature/ar-1-title", PID: 99,
		Started: w.now.Add(-2 * time.Hour), Ended: w.now.Add(-time.Hour), Outcome: OutcomeOK,
		PR: "https://example.com/pr/7", CI: ChecksPass, Head: "aaa111"}
	d.State.Runs = []*Run{run}
	w.git.head = "main2"
	return w, d, l, run
}

func conflicted(head string) OpenPR {
	return OpenPR{Number: 7, URL: "https://example.com/pr/7", Branch: "feature/ar-1-title", Head: head,
		Issue: "ar-1", Mergeable: MergeConflicting, Checks: ChecksPass}
}

// A PR of its own run that conflicts after a merge is resumed in the
// run's worktree, in the run's session, under a claim taken first, with a
// brief to merge the default branch, and a rework: comment naming the
// merge. The same PR at the same head is not taken again.
func TestSweepResumesItsOwnRun(t *testing.T) {
	t.Parallel()
	w, d, l, old := sweepWorld(t)
	w.forge.open = []OpenPR{conflicted("aaa111")}
	if err := d.Wake(t.Context(), WakeBranch); err != nil {
		t.Fatal(err)
	}
	start, launch := w.calls.index("start ar-1 old-session"), w.calls.index("launch ar-1 old-session")
	if start < 0 || launch < 0 || start > launch {
		t.Fatalf("want the claim in the old session before the resume; calls %q", w.calls.all())
	}
	sp := l.spec("old-session")
	if !sp.Resume || sp.Dir != old.Worktree || sp.Branch != old.Branch || sp.Model != old.Model {
		t.Errorf("the resume's spec = %+v", sp)
	}
	for _, want := range []string{"https://example.com/pr/7", "conflicts", "merge origin/main", "main2", "start", "ar-1"} {
		if !strings.Contains(sp.Brief, want) {
			t.Errorf("the rework brief lacks %q:\n%s", want, sp.Brief)
		}
	}
	if w.calls.count("worktree add") != 0 || w.calls.count("worktree rework") != 0 {
		t.Errorf("a resume made a worktree: %q", w.calls.all())
	}
	cs := w.tracker.comments["ar-1"]
	var rework string
	for _, c := range cs {
		if strings.HasPrefix(c, "rework:") {
			rework = c
		}
	}
	for _, want := range []string{"pr=7", "kind=conflict", "main=main2", "head=aaa111", "action=resume", "session=old-session"} {
		if !strings.Contains(rework, want) {
			t.Errorf("the rework comment lacks %q: %q", want, rework)
		}
	}
	if len(d.State.Runs) != 2 || d.State.Runs[1].Rework != ReworkConflict || !d.State.Runs[1].Live() {
		t.Fatalf("runs = %+v", d.State.Runs)
	}
	if !old.Cleaned || !old.BranchDone {
		t.Errorf("the run resumed still owns its worktree: %+v", old)
	}
	if !strings.Contains(w.out.String(), "sweep: resume ar-1") {
		t.Errorf("the output:\n%s", w.out.String())
	}

	// Its rework ends without pushing: the PR is named once, not resumed again.
	l.proc("old-session").end(Exit{Outcome: OutcomeOK})
	w.git.head = "main3"
	for range 2 {
		if err := d.Wake(t.Context(), WakeBranch); err != nil {
			t.Fatal(err)
		}
	}
	if n := w.calls.count("launch "); n != 1 {
		t.Errorf("%d launches; want the one resume", n)
	}
	if n := strings.Count(w.out.String(), "sweep: name ar-1 PR 7 [rework-left]"); n != 1 {
		t.Errorf("named %d times; want once:\n%s", n, w.out.String())
	}
}

// A PR whose required checks failed after its run's had passed is sent
// back as a conflict is; one whose run counted the failure as its own is
// not, since that is the CI stop's (dispatcher.md §6).
func TestSweepFailedChecks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		runCI   string
		runHead string
		resumed bool
	}{
		{name: "failed after it passed", runCI: ChecksPass, runHead: "aaa111", resumed: true},
		{name: "failed on a new head", runCI: ChecksPass, runHead: "zzz999", resumed: true},
		{name: "its own failure", runCI: ChecksFail, runHead: "aaa111"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w, d, _, old := sweepWorld(t)
			old.CI, old.Head = tc.runCI, tc.runHead
			pr := conflicted("aaa111")
			pr.Mergeable, pr.Checks = MergeClean, ChecksFail
			w.forge.open = []OpenPR{pr}
			if err := d.Wake(t.Context(), WakeBranch); err != nil {
				t.Fatal(err)
			}
			if got := w.calls.count("launch ar-1 old-session") == 1; got != tc.resumed {
				t.Fatalf("resumed = %v; want %v; calls %q", got, tc.resumed, w.calls.all())
			}
			if tc.resumed && d.State.Runs[1].Rework != ReworkChecks {
				t.Errorf("the rework's kind = %q", d.State.Runs[1].Rework)
			}
			if !tc.resumed && !strings.Contains(w.out.String(), "[own-ci]") {
				t.Errorf("its own failure was not named:\n%s", w.out.String())
			}
		})
	}
}

// A PR a live session holds is left, and named only if it is still broken
// at the next sweep.
func TestSweepLeavesAHeldPR(t *testing.T) {
	t.Parallel()
	w, d, _, _ := sweepWorld(t)
	w.tracker.claims["ar-1"] = "someone-else"
	w.forge.open = []OpenPR{conflicted("aaa111")}
	if err := d.Wake(t.Context(), WakeStart); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(w.out.String(), "sweep: name") {
		t.Errorf("named at the first sweep:\n%s", w.out.String())
	}
	w.git.head = "main3"
	if err := d.Wake(t.Context(), WakeBranch); err != nil {
		t.Fatal(err)
	}
	if w.calls.count("launch ") != 0 || w.calls.count("start ") != 0 {
		t.Errorf("a held PR was taken: %q", w.calls.all())
	}
	if !strings.Contains(w.out.String(), "sweep: name ar-1 PR 7 [held]") {
		t.Errorf("not named at the second sweep:\n%s", w.out.String())
	}
}

// A PR no one holds and no run of its own opened is reworked only with
// the label dispatch:rework: then a new run is claimed and started on the
// PR's branch, in a worktree made from it; without the label it is named.
func TestSweepNewRunOnAPRNoOneHolds(t *testing.T) {
	t.Parallel()
	for _, labeled := range []bool{true, false} {
		name := "unlabeled"
		if labeled {
			name = "labeled"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.tracker.add("ar-2", "bug", "model:haiku")
			w.tracker.set("ar-2", func(sr *proto.ShowResult) { sr.Issue.Status = "in_progress" })
			d, l := w.dispatcher(DefaultConfig())
			pr := OpenPR{Number: 9, URL: "https://example.com/pr/9", Branch: "fix/ar-2-bug", Head: "bbb222",
				Issue: "ar-2", Mergeable: MergeConflicting}
			if labeled {
				pr.Labels = []string{ReworkLabel}
			}
			w.forge.open = []OpenPR{pr}
			if err := d.Wake(t.Context(), WakeStart); err != nil {
				t.Fatal(err)
			}
			if !labeled {
				if w.calls.count("start ar-2") != 0 || !strings.Contains(w.out.String(), "sweep: name ar-2 PR 9 [not-ours]") {
					t.Errorf("an unlabeled PR: calls %q\n%s", w.calls.all(), w.out.String())
				}
				return
			}
			ss := l.sessions()
			if len(ss) != 1 {
				t.Fatalf("launched %d runs; want 1: %q", len(ss), w.calls.all())
			}
			sp := l.spec(ss[0])
			if sp.Resume || sp.Branch != "fix/ar-2-bug" || sp.Model != ModelID("haiku") {
				t.Errorf("the new run's spec = %+v", sp)
			}
			start, wt, launch := w.calls.index("start ar-2 "+ss[0]), w.calls.index("worktree rework "), w.calls.index("launch ar-2")
			if start < 0 || wt < start || launch < wt {
				t.Errorf("want claim, worktree from the branch, launch: %q", w.calls.all())
			}
			for _, want := range []string{"PR 9", "https://example.com/pr/9", "merge origin/main"} {
				if !strings.Contains(sp.Brief, want) {
					t.Errorf("the brief lacks %q:\n%s", want, sp.Brief)
				}
			}
			if w.calls.count("brief ar-2") != 1 {
				t.Errorf("the brief was not recorded: %q", w.calls.all())
			}
			if r := d.State.Runs[0]; r.Branch != "fix/ar-2-bug" || r.Rework != ReworkConflict {
				t.Errorf("the run = %+v", r)
			}
		})
	}
}

// Without a free slot, or while something holds starting, the sweep acts
// on nothing and is due again at the next wake of any source.
func TestSweepWaitsForAHold(t *testing.T) {
	t.Parallel()
	w, d, _, _ := sweepWorld(t)
	w.tracker.paused = true
	w.forge.open = []OpenPR{conflicted("aaa111")}
	if err := d.Wake(t.Context(), WakeBranch); err != nil {
		t.Fatal(err)
	}
	if w.calls.count("launch ") != 0 || !d.State.Sweep.Due {
		t.Fatalf("a paused sweep launched, or is not due: %q, due %v", w.calls.all(), d.State.Sweep.Due)
	}
	w.tracker.setPaused(false)
	if err := d.Wake(t.Context(), WakeTimer); err != nil {
		t.Fatal(err)
	}
	if w.calls.count("launch ar-1 old-session") != 1 || d.State.Sweep.Due {
		t.Errorf("the due sweep did not resume: %q, due %v", w.calls.all(), d.State.Sweep.Due)
	}
}

// The sweep reads the open PRs at the start and when the default branch
// moved, or when one is due; not at a wake for an inbox item. A dry run
// says what it would do and does none of it.
func TestSweepWhenAndDryRun(t *testing.T) {
	t.Parallel()
	w, d, _, _ := sweepWorld(t)
	w.forge.open = []OpenPR{conflicted("aaa111")}
	if err := d.Wake(t.Context(), WakeInbox); err != nil {
		t.Fatal(err)
	}
	if n := w.forge.opens.Load(); n != 0 {
		t.Errorf("an inbox wake swept %d times", n)
	}

	w2, d2, _, _ := sweepWorld(t)
	d2.Cfg.DryRun = true
	w2.forge.open = []OpenPR{conflicted("aaa111"),
		{Number: 8, Branch: "docs/x", Mergeable: MergeClean, Checks: ChecksPass},
		{Number: 10, Branch: "feature/y", Mergeable: MergeConflicting}}
	if err := d2.Wake(t.Context(), WakeStart); err != nil {
		t.Fatal(err)
	}
	if got := w2.calls.all(); len(got) != 0 {
		t.Errorf("a dry sweep asked the stand-ins %q", got)
	}
	out := w2.out.String()
	for _, want := range []string{"sweep: would resume ar-1 PR 7", "sweep: name PR 10 [no-issue]"} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry sweep's output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "PR 8") {
		t.Errorf("a green PR was named:\n%s", out)
	}
}

// A mergeable state the forge has not computed yet makes the next wake
// sweep again.
func TestSweepUnknownMergeStateIsDue(t *testing.T) {
	t.Parallel()
	w, d, _, _ := sweepWorld(t)
	pr := conflicted("aaa111")
	pr.Mergeable = MergeUnknown
	w.forge.open = []OpenPR{pr}
	if err := d.Wake(t.Context(), WakeBranch); err != nil {
		t.Fatal(err)
	}
	if !d.State.Sweep.Due || w.calls.count("launch ") != 0 {
		t.Errorf("due %v, calls %q", d.State.Sweep.Due, w.calls.all())
	}
}
