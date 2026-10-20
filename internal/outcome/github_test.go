package outcome

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGH answers gh's commands from recorded output, by argv; a command
// it has no answer for fails the test. Nothing reaches GitHub.
type fakeGH struct {
	t       *testing.T
	answers map[string]string
	mu      sync.Mutex
	calls   []string
}

func (f *fakeGH) exec(_ context.Context, dir, name string, args ...string) ([]byte, error) {
	key := strings.Join(args, " ")
	f.mu.Lock()
	f.calls = append(f.calls, key)
	f.mu.Unlock()
	if name != "gh" || dir != "/repo" {
		f.t.Errorf("ran %s in %s", name, dir)
	}
	out, ok := f.answers[key]
	if !ok {
		f.t.Errorf("no recorded answer for gh %s", key)
		return nil, errors.New("exit status 1")
	}
	return []byte(out), nil
}

const prList = "pr list --state all --limit 500 --json number,url,baseRefName,headRefName,state,createdAt,mergedAt,closedAt"

func ghFixture(t *testing.T) (*GitHub, *fakeGH) {
	f := &fakeGH{t: t, answers: map[string]string{
		prList: `[
 {"number":272,"url":"https://example.com/acme/widgets/pull/272","baseRefName":"main","headRefName":"fix/ar-c3d4-other","state":"CLOSED","createdAt":"2026-10-19T13:00:00Z","mergedAt":null,"closedAt":"2026-10-19T15:00:00Z"},
 {"number":271,"url":"https://example.com/acme/widgets/pull/271","baseRefName":"main","headRefName":"feature/ar-a1b2-x","state":"MERGED","createdAt":"2026-10-19T12:19:00Z","mergedAt":"2026-10-19T13:40:00Z","closedAt":"2026-10-19T13:40:00Z"},
 {"number":270,"url":"https://example.com/acme/widgets/pull/270","baseRefName":"main","headRefName":"docs/handed-off-name","state":"OPEN","createdAt":"2026-10-19T11:00:00Z","mergedAt":null,"closedAt":null}
]`,
		// Two pages, as --paginate prints them.
		"api --paginate repos/{owner}/{repo}/pulls/271/commits": `[
 {"sha":"c1","parents":[{"sha":"b0"}],"commit":{"committer":{"date":"2026-10-19T12:20:00Z"}}},
 {"sha":"c2","parents":[{"sha":"c1"}],"commit":{"committer":{"date":"2026-10-19T12:30:00Z"}}}]
[{"sha":"c3","parents":[{"sha":"c2"},{"sha":"m1"}],"commit":{"committer":{"date":"2026-10-19T13:00:00Z"}}}]`,
		"run list --branch feature/ar-a1b2-x --limit 500 --json " + runFields: `[
 {"databaseId":11,"attempt":2,"conclusion":"success","status":"completed","headSha":"c1","headBranch":"feature/ar-a1b2-x","event":"pull_request","createdAt":"2026-10-19T12:25:00Z","updatedAt":"2026-10-19T12:28:00Z","workflowName":"ci"},
 {"databaseId":12,"attempt":1,"conclusion":"success","status":"completed","headSha":"c1","headBranch":"feature/ar-a1b2-x","event":"pull_request_target","createdAt":"2026-10-19T12:21:00Z","updatedAt":"2026-10-19T12:21:30Z","workflowName":"cla"},
 {"databaseId":13,"attempt":1,"conclusion":"","status":"in_progress","headSha":"c3","headBranch":"feature/ar-a1b2-x","event":"pull_request","createdAt":"2026-10-19T13:01:00Z","updatedAt":"2026-10-19T13:02:00Z","workflowName":"ci"},
 {"databaseId":14,"attempt":1,"conclusion":"success","status":"completed","headSha":"c2","headBranch":"feature/ar-a1b2-x","event":"push","createdAt":"2026-10-19T12:31:00Z","updatedAt":"2026-10-19T12:32:00Z","workflowName":"ci"}
]`,
		"run list --event merge_group --limit 500 --json " + runFields: `[
 {"databaseId":21,"attempt":1,"conclusion":"timed_out","status":"completed","headSha":"q1","headBranch":"gh-readonly-queue/main/pr-271-c3","event":"merge_group","createdAt":"2026-10-19T13:10:00Z","updatedAt":"2026-10-19T13:30:00Z","workflowName":"ci"},
 {"databaseId":22,"attempt":1,"conclusion":"failure","status":"completed","headSha":"q9","headBranch":"gh-readonly-queue/main/pr-2710-x","event":"merge_group","createdAt":"2026-10-19T13:10:00Z","updatedAt":"2026-10-19T13:30:00Z","workflowName":"ci"}
]`,
		"api repos/{owner}/{repo}/actions/runs/11/attempts/1":   `{"status":"completed","conclusion":"failure","run_started_at":"2026-10-19T12:21:00Z","updated_at":"2026-10-19T12:24:00Z"}`,
		"run view 11 --attempt 1 --json jobs":                   `{"jobs":[{"name":"lint","conclusion":"success"},{"name":"test","conclusion":"failure"}]}`,
		"api --paginate repos/{owner}/{repo}/pulls/271/reviews": `[{"state":"COMMENTED"},{"state":"CHANGES_REQUESTED"},{"state":"APPROVED"}]`,
		"api --paginate repos/{owner}/{repo}/pulls/271/comments": `[{"id":1},{"id":2}]
[{"id":3}]`,
		"api --paginate repos/{owner}/{repo}/issues/271/timeline": `[{"event":"committed"},{"event":"reviewed"}]`,
	}}
	g := &GitHub{Dir: "/repo", Exec: f.exec, Fetch: func(_ context.Context, dir, remote, ref string) error {
		if dir != "/repo" || remote != "origin" || ref != "refs/pull/271/head" {
			return fmt.Errorf("fetched %s %s in %s", remote, ref, dir)
		}
		return nil
	}}
	return g, f
}

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// The changes of an issue are its branch's by name and those handoffs
// named; closed ones are listed from the same read.
func TestGitHubChanges(t *testing.T) {
	t.Parallel()
	g, f := ghFixture(t)
	got, err := g.Changes(t.Context(), "ar-a1b2", []string{"docs/handed-off-name"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Change{
		{ID: "270", URL: "https://example.com/acme/widgets/pull/270", Base: "main", Branch: "docs/handed-off-name", Opened: utc("2026-10-19T11:00:00Z")},
		{ID: "271", URL: "https://example.com/acme/widgets/pull/271", Base: "main", Branch: "feature/ar-a1b2-x",
			Opened: utc("2026-10-19T12:19:00Z"), Merged: utc("2026-10-19T13:40:00Z"), Closed: utc("2026-10-19T13:40:00Z")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Changes =\n%+v\nwant\n%+v", got, want)
	}
	closed, err := g.Closed(t.Context(), utc("2026-10-19T00:00:00Z"))
	if err != nil || len(closed) != 1 || closed[0].ID != "272" || !closed[0].Unmerge {
		t.Errorf("Closed = %+v, %v", closed, err)
	}
	if len(f.calls) != 1 {
		t.Errorf("the pull requests were listed %d times, want once", len(f.calls))
	}
}

// Detail maps gh's runs, reruns, jobs, reviews and timeline to a host's
// detail: a rerun's earlier attempt is its own run, a push is no
// attempt, and the queue's runs are the change's alone.
func TestGitHubDetail(t *testing.T) {
	t.Parallel()
	g, _ := ghFixture(t)
	ch := Change{ID: "271", Branch: "feature/ar-a1b2-x"}
	d, err := g.Detail(t.Context(), ch)
	if err != nil {
		t.Fatal(err)
	}
	want := Detail{
		Commits: []Commit{
			{SHA: "c1", Parents: []string{"b0"}, At: utc("2026-10-19T12:20:00Z")},
			{SHA: "c2", Parents: []string{"c1"}, At: utc("2026-10-19T12:30:00Z")},
			{SHA: "c3", Parents: []string{"c2", "m1"}, At: utc("2026-10-19T13:00:00Z")},
		},
		Runs: []Run{
			{ID: "11", Workflow: "ci", Attempt: 1, SHA: "c1", Conclusion: Fail, Started: utc("2026-10-19T12:21:00Z"),
				Finished: utc("2026-10-19T12:24:00Z"), FailedJobs: []string{"test"}},
			{ID: "11", Workflow: "ci", Attempt: 2, SHA: "c1", Conclusion: Pass, Started: utc("2026-10-19T12:25:00Z"), Finished: utc("2026-10-19T12:28:00Z")},
			{ID: "12", Workflow: "cla", Attempt: 1, SHA: "c1", Conclusion: Pass, Started: utc("2026-10-19T12:21:00Z"), Finished: utc("2026-10-19T12:21:30Z")},
			{ID: "13", Workflow: "ci", Attempt: 1, SHA: "c3", Conclusion: Pending, Started: utc("2026-10-19T13:01:00Z"), Finished: utc("2026-10-19T13:02:00Z")},
			{ID: "21", Workflow: "ci", Attempt: 1, SHA: "q1", Queue: true, Conclusion: Infra, Started: utc("2026-10-19T13:10:00Z"), Finished: utc("2026-10-19T13:30:00Z")},
		},
		ChangesRequested: 1, Comments: 3, Fetched: true,
	}
	if !reflect.DeepEqual(d, want) {
		t.Errorf("Detail =\n%+v\nwant\n%+v", d, want)
	}
	ci := ciOf(g.Name(), d.Runs, d.Commits, utc("2026-10-19T13:40:00Z"))
	if ci.Attempts != 2 || ci.QueueAttempts != 1 || len(ci.Failures) != 2 ||
		ci.Failures[0].Cause != CauseFlake || ci.Failures[1].Cause != CauseInfra {
		t.Errorf("ci = %+v", ci)
	}

	g.Workflows = []string{"cla"}
	g.queue = nil
	d, err = g.Detail(t.Context(), ch)
	if err != nil || len(d.Runs) != 1 || d.Runs[0].Workflow != "cla" {
		t.Errorf("with --workflow cla: %+v, %v", d.Runs, err)
	}
}

// A force push marks the change rewritten; a fetch that fails leaves
// the commits unfetched with its error; a failing gh fails Detail.
func TestGitHubDetailRewritten(t *testing.T) {
	t.Parallel()
	g, f := ghFixture(t)
	f.answers["api --paginate repos/{owner}/{repo}/issues/271/timeline"] = `[{"event":"head_ref_force_pushed"}]`
	g.Fetch = func(context.Context, string, string, string) error { return errors.New("no such ref") }
	d, err := g.Detail(t.Context(), Change{ID: "271", Branch: "feature/ar-a1b2-x"})
	if err != nil || !d.Rewritten || d.Fetched || d.FetchErr == nil {
		t.Errorf("Detail = rewritten %v fetched %v fetch error %v; %v", d.Rewritten, d.Fetched, d.FetchErr, err)
	}

	g2 := &GitHub{Dir: "/repo", Exec: func(context.Context, string, string, ...string) ([]byte, error) {
		return nil, errors.New("exit status 4: gh auth login")
	}}
	if _, err := g2.Changes(t.Context(), "ar-a1b2", nil); err == nil || !strings.Contains(err.Error(), "gh auth login") {
		t.Errorf("Changes with gh signed out: %v", err)
	}
}

func TestConclusion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ status, concl, want string }{
		{"completed", "success", Pass}, {"completed", "skipped", Pass}, {"completed", "neutral", Pass},
		{"completed", "failure", Fail}, {"completed", "cancelled", Cancelled},
		{"completed", "timed_out", Infra}, {"completed", "startup_failure", Infra},
		{"in_progress", "", Pending}, {"queued", "", Pending}, {"completed", "", Pending},
	} {
		if got := conclusion(tc.status, tc.concl); got != tc.want {
			t.Errorf("conclusion(%q, %q) = %q, want %q", tc.status, tc.concl, got, tc.want)
		}
	}
}
