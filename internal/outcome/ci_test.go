package outcome

import (
	"reflect"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

// at is t0 plus m minutes.
func at(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }

// run is one workflow attempt that started at minute m and took a minute.
func run(id, sha string, attempt, m int, concl string, jobs ...string) Run {
	return Run{ID: id, Workflow: "ci", Attempt: attempt, SHA: sha, Conclusion: concl,
		Started: at(m), Finished: at(m + 1), FailedJobs: jobs}
}

func queued(r Run) Run { r.Queue = true; return r }

// The change's commits for the cause rules: c3 merges the default branch
// (its second parent, m1, is not the change's) into c2.
var ciCommits = []Commit{
	{SHA: "c1", Parents: []string{"b0"}, At: at(0)},
	{SHA: "c2", Parents: []string{"c1"}, At: at(10)},
	{SHA: "c3", Parents: []string{"c2", "m1"}, At: at(20)},
	{SHA: "c4", Parents: []string{"c3"}, At: at(30)},
}

// Each failure's cause is decided by rule from the sequence of runs
// alone (docs/design/outcome-record.md §5).
func TestCICauses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		runs  []Run
		until time.Time
		want  CI
	}{
		{"first pass", []Run{run("1", "c1", 1, 1, Pass)}, at(100),
			CI{Host: "github", Attempts: 1, FirstPass: true, Failures: []Failure{}}},
		{"flake: a rerun of the same commit passed", []Run{
			run("1", "c1", 1, 1, Fail, "test"),
			run("1", "c1", 2, 3, Pass),
		}, at(100), CI{Host: "github", Attempts: 2, Failures: []Failure{
			{Run: "ci:1", Attempt: 1, SHA: "c1", Event: "change", Jobs: []string{"test"}, Cause: "flake", At: at(2)},
		}}},
		{"change: a later commit passed", []Run{
			run("1", "c1", 1, 1, Fail, "lint"),
			run("2", "c2", 1, 11, Pass),
		}, at(100), CI{Host: "github", Attempts: 2, Failures: []Failure{
			{Run: "ci:1", Attempt: 1, SHA: "c1", Event: "change", Jobs: []string{"lint"}, Cause: "change", At: at(2)},
		}}},
		{"a pass on the same commit after another commit is not a flake", []Run{
			run("1", "c1", 1, 1, Fail, "test"),
			run("2", "c2", 1, 11, Fail, "test"),
			run("1", "c1", 2, 12, Pass),
		}, at(100), CI{Host: "github", Attempts: 3, Failures: []Failure{
			{Run: "ci:1", Attempt: 1, SHA: "c1", Event: "change", Jobs: []string{"test"}, Cause: "unknown", At: at(2)},
			{Run: "ci:2", Attempt: 1, SHA: "c2", Event: "change", Jobs: []string{"test"}, Cause: "unknown", At: at(12)},
		}}},
		{"base: the catch-up merge broke a commit that had passed", []Run{
			run("2", "c2", 1, 11, Pass),
			run("3", "c3", 1, 21, Fail, "test"),
			run("4", "c4", 1, 31, Pass),
		}, at(100), CI{Host: "github", Attempts: 3, Failures: []Failure{
			{Run: "ci:3", Attempt: 1, SHA: "c3", Event: "change", Jobs: []string{"test"}, Cause: "base", At: at(22)},
		}}},
		{"queue: the merge queue failed while the change passed", []Run{
			run("4", "c4", 1, 31, Pass),
			queued(run("9", "q1", 1, 40, Fail, "test")),
			queued(run("10", "q2", 1, 50, Pass)),
		}, at(100), CI{Host: "github", Attempts: 1, QueueAttempts: 2, Failures: []Failure{
			{Run: "ci:9", Attempt: 1, SHA: "q1", Event: "queue", Jobs: []string{"test"}, Cause: "queue", At: at(41)},
		}}},
		{"infra: timed out", []Run{
			run("1", "c1", 1, 1, Infra),
			run("1", "c1", 2, 3, Pass),
		}, at(100), CI{Host: "github", Attempts: 2, Failures: []Failure{
			{Run: "ci:1", Attempt: 1, SHA: "c1", Event: "change", Cause: "infra", At: at(2)},
		}}},
		{"a run cancelled by a newer push is no attempt", []Run{
			run("1", "c1", 1, 1, Cancelled),
			run("2", "c2", 1, 11, Pass),
		}, at(100), CI{Host: "github", Attempts: 1, FirstPass: true, Failures: []Failure{}}},
		{"unknown: nothing passed after", []Run{
			run("1", "c1", 1, 1, Fail, "test"),
		}, at(100), CI{Host: "github", Attempts: 1, Failures: []Failure{
			{Run: "ci:1", Attempt: 1, SHA: "c1", Event: "change", Jobs: []string{"test"}, Cause: "unknown", At: at(2)},
		}}},
		{"workflows on one commit are one attempt; pending and later runs left out", []Run{
			run("1", "c1", 1, 1, Pass),
			{ID: "5", Workflow: "lint", Attempt: 1, SHA: "c1", Conclusion: Fail, Started: at(1), Finished: at(4), FailedJobs: []string{"lint"}},
			run("2", "c2", 1, 11, Pending),
			run("3", "c4", 1, 200, Pass),
		}, at(100), CI{Host: "github", Attempts: 1, Failures: []Failure{
			{Run: "ci:5", Attempt: 1, SHA: "c1", Event: "change", Jobs: []string{"lint"}, Cause: "unknown", At: at(4)},
		}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ciOf("github", tc.runs, ciCommits, tc.until)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ciOf =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}

// The catch-up merges are the commits whose second parent is not the
// change's own.
func TestCatchUps(t *testing.T) {
	t.Parallel()
	got := catchUps(ciCommits)
	want := []Commit{ciCommits[2]}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("catchUps = %+v, want %+v", got, want)
	}
	inner := append(ciCommits[:2:2], Commit{SHA: "c5", Parents: []string{"c2", "c1"}})
	if got := catchUps(inner); len(got) != 0 {
		t.Errorf("a merge of the change's own commits is a catch-up: %+v", got)
	}
}
