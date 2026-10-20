package outcome

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/gitx"
)

// Exec runs a command in dir and returns its standard output.
type Exec func(ctx context.Context, dir, name string, args ...string) ([]byte, error)

// GitHub is the host GitHub, read through `gh` with whatever account gh
// is signed in to, in the repository Dir (decision 67: the client reads
// the host, never ariesd). It is the only GitHub-specific part of the
// capture; another host implements Host the same way.
type GitHub struct {
	Dir string
	// Remote is the git remote the change's ref is fetched from;
	// "origin" when empty.
	Remote string
	// Workflows, when set, are the only workflows whose runs count as
	// attempts, by name; empty counts every workflow a change runs.
	Workflows []string
	// Exec runs gh; Fetch fetches a ref. Tests replace them; nil runs the
	// real ones.
	Exec  Exec
	Fetch func(ctx context.Context, dir, remote, refspec string) error

	prs   []ghPR
	queue []ghRun
}

// maxPRs is the most pull requests one capture lists; maxRuns the most
// runs it lists for a branch or the merge queue.
const (
	maxPRs  = 500
	maxRuns = 500
)

// Name is the host's name in a record.
func (g *GitHub) Name() string { return "github" }

type ghPR struct {
	Number      int        `json:"number"`
	URL         string     `json:"url"`
	BaseRefName string     `json:"baseRefName"`
	HeadRefName string     `json:"headRefName"`
	State       string     `json:"state"`
	CreatedAt   time.Time  `json:"createdAt"`
	MergedAt    *time.Time `json:"mergedAt"`
	ClosedAt    *time.Time `json:"closedAt"`
}

type ghRun struct {
	DatabaseID   int64     `json:"databaseId"`
	Attempt      int       `json:"attempt"`
	Conclusion   string    `json:"conclusion"`
	Status       string    `json:"status"`
	HeadSha      string    `json:"headSha"`
	HeadBranch   string    `json:"headBranch"`
	Event        string    `json:"event"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
	WorkflowName string    `json:"workflowName"`
}

const runFields = "databaseId,attempt,conclusion,status,headSha,headBranch,event,createdAt,updatedAt,workflowName"

func (g *GitHub) gh(ctx context.Context, out any, args ...string) error {
	run := g.Exec
	if run == nil {
		run = execCommand
	}
	b, err := run(ctx, g.Dir, "gh", args...)
	if err != nil {
		return fmt.Errorf("gh %s: %w", strings.Join(args[:min(2, len(args))], " "), err)
	}
	return decodeAll(b, out)
}

// decodeAll decodes b into out; `gh api --paginate` writes one JSON
// array per page, which are appended into one slice.
func decodeAll(b []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	var pages []json.RawMessage
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return fmt.Errorf("decode gh's output: %w", err)
		}
		pages = append(pages, raw)
	}
	switch {
	case len(pages) == 0:
		return errors.New("gh printed nothing")
	case len(pages) == 1:
		return json.Unmarshal(pages[0], out)
	}
	var all []json.RawMessage
	for _, p := range pages {
		var items []json.RawMessage
		if err := json.Unmarshal(p, &items); err != nil {
			return fmt.Errorf("decode gh's pages: %w", err)
		}
		all = append(all, items...)
	}
	joined, err := json.Marshal(all)
	if err != nil {
		return err
	}
	return json.Unmarshal(joined, out)
}

func execCommand(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // fixed binary, explicit argv, no shell
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			first, _, _ := strings.Cut(msg, "\n")
			return nil, fmt.Errorf("%w: %s", err, first)
		}
		return nil, err
	}
	return out, nil
}

func (g *GitHub) list(ctx context.Context) ([]ghPR, error) {
	if g.prs != nil {
		return g.prs, nil
	}
	var prs []ghPR
	if err := g.gh(ctx, &prs, "pr", "list", "--state", "all", "--limit", strconv.Itoa(maxPRs),
		"--json", "number,url,baseRefName,headRefName,state,createdAt,mergedAt,closedAt"); err != nil {
		return nil, err
	}
	slices.SortFunc(prs, func(a, b ghPR) int { return a.CreatedAt.Compare(b.CreatedAt) })
	g.prs = prs
	if g.prs == nil {
		g.prs = []ghPR{}
	}
	return g.prs, nil
}

func change(p ghPR) Change {
	c := Change{ID: strconv.Itoa(p.Number), URL: p.URL, Base: p.BaseRefName, Branch: p.HeadRefName, Opened: p.CreatedAt}
	if p.MergedAt != nil {
		c.Merged = *p.MergedAt
	}
	if p.ClosedAt != nil {
		c.Closed = *p.ClosedAt
	}
	c.Unmerge = p.State == "CLOSED" && p.MergedAt == nil
	return c
}

// Changes returns the pull requests whose head branch is the issue's by
// name or one of branches, among the repository's latest maxPRs.
func (g *GitHub) Changes(ctx context.Context, issue string, branches []string) ([]Change, error) {
	prs, err := g.list(ctx)
	if err != nil {
		return nil, err
	}
	var out []Change
	for _, p := range prs {
		if gitx.BranchFor(p.HeadRefName, issue) || slices.Contains(branches, p.HeadRefName) {
			out = append(out, change(p))
		}
	}
	return out, nil
}

// Closed returns the pull requests closed unmerged since then.
func (g *GitHub) Closed(ctx context.Context, since time.Time) ([]Change, error) {
	prs, err := g.list(ctx)
	if err != nil {
		return nil, err
	}
	var out []Change
	for _, p := range prs {
		if c := change(p); c.Unmerge && !c.Closed.Before(since) {
			out = append(out, c)
		}
	}
	return out, nil
}

// conclusion maps a run's status and conclusion to a Run's.
func conclusion(status, concl string) string {
	if status != "" && status != "completed" {
		return Pending
	}
	switch concl {
	case "success", "skipped", "neutral":
		return Pass
	case "failure":
		return Fail
	case "cancelled":
		return Cancelled
	case "":
		return Pending
	default: // timed_out, startup_failure, action_required, stale
		return Infra
	}
}

// Detail reads the pull request's commits, runs (each earlier attempt of
// a rerun, and the merge queue's for it), reviews, review comments and
// force pushes, and fetches its head ref, which outlives its branch.
func (g *GitHub) Detail(ctx context.Context, c Change) (Detail, error) {
	var d Detail
	n := c.ID
	var commits []struct {
		SHA     string `json:"sha"`
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
		Commit struct {
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	if err := g.gh(ctx, &commits, "api", "--paginate", "repos/{owner}/{repo}/pulls/"+n+"/commits"); err != nil {
		return Detail{}, err
	}
	for _, cm := range commits {
		x := Commit{SHA: cm.SHA, At: cm.Commit.Committer.Date}
		for _, p := range cm.Parents {
			x.Parents = append(x.Parents, p.SHA)
		}
		d.Commits = append(d.Commits, x)
	}

	var runs []ghRun
	if err := g.gh(ctx, &runs, "run", "list", "--branch", c.Branch, "--limit", strconv.Itoa(maxRuns), "--json", runFields); err != nil {
		return Detail{}, err
	}
	if g.queue == nil {
		var q []ghRun
		if err := g.gh(ctx, &q, "run", "list", "--event", "merge_group", "--limit", strconv.Itoa(maxRuns), "--json", runFields); err != nil {
			return Detail{}, err
		}
		g.queue = append([]ghRun{}, q...)
	}
	mark := "/pr-" + n + "-"
	for _, q := range g.queue {
		if strings.Contains(q.HeadBranch, mark) {
			runs = append(runs, q)
		}
	}
	for _, r := range runs {
		queue := r.Event == "merge_group"
		if !queue && r.Event != "pull_request" && r.Event != "pull_request_target" {
			continue
		}
		if len(g.Workflows) > 0 && !slices.Contains(g.Workflows, r.WorkflowName) {
			continue
		}
		id := strconv.FormatInt(r.DatabaseID, 10)
		for k := 1; k < r.Attempt; k++ {
			var a struct {
				Status     string    `json:"status"`
				Conclusion string    `json:"conclusion"`
				Started    time.Time `json:"run_started_at"`
				Updated    time.Time `json:"updated_at"`
			}
			if err := g.gh(ctx, &a, "api", "repos/{owner}/{repo}/actions/runs/"+id+"/attempts/"+strconv.Itoa(k)); err != nil {
				return Detail{}, err
			}
			d.Runs = append(d.Runs, Run{ID: id, Workflow: r.WorkflowName, Attempt: k, SHA: r.HeadSha, Queue: queue,
				Conclusion: conclusion(a.Status, a.Conclusion), Started: a.Started, Finished: a.Updated})
		}
		d.Runs = append(d.Runs, Run{ID: id, Workflow: r.WorkflowName, Attempt: max(r.Attempt, 1), SHA: r.HeadSha, Queue: queue,
			Conclusion: conclusion(r.Status, r.Conclusion), Started: r.CreatedAt, Finished: r.UpdatedAt})
	}
	for i := range d.Runs {
		r := &d.Runs[i]
		if r.Conclusion != Fail {
			continue
		}
		var v struct {
			Jobs []struct {
				Name       string `json:"name"`
				Conclusion string `json:"conclusion"`
			} `json:"jobs"`
		}
		if err := g.gh(ctx, &v, "run", "view", r.ID, "--attempt", strconv.Itoa(r.Attempt), "--json", "jobs"); err != nil {
			return Detail{}, err
		}
		for _, j := range v.Jobs {
			if j.Conclusion == "failure" || j.Conclusion == "timed_out" {
				r.FailedJobs = append(r.FailedJobs, j.Name)
			}
		}
	}

	var reviews []struct {
		State string `json:"state"`
	}
	if err := g.gh(ctx, &reviews, "api", "--paginate", "repos/{owner}/{repo}/pulls/"+n+"/reviews"); err != nil {
		return Detail{}, err
	}
	for _, r := range reviews {
		if r.State == "CHANGES_REQUESTED" {
			d.ChangesRequested++
		}
	}
	var comments []json.RawMessage
	if err := g.gh(ctx, &comments, "api", "--paginate", "repos/{owner}/{repo}/pulls/"+n+"/comments"); err != nil {
		return Detail{}, err
	}
	d.Comments = len(comments)
	var timeline []struct {
		Event string `json:"event"`
	}
	if err := g.gh(ctx, &timeline, "api", "--paginate", "repos/{owner}/{repo}/issues/"+n+"/timeline"); err != nil {
		return Detail{}, err
	}
	d.Rewritten = slices.ContainsFunc(timeline, func(e struct {
		Event string `json:"event"`
	}) bool {
		return e.Event == "head_ref_force_pushed"
	})

	fetch := g.Fetch
	if fetch == nil {
		fetch = gitx.Fetch
	}
	remote := g.Remote
	if remote == "" {
		remote = "origin"
	}
	if err := fetch(ctx, g.Dir, remote, "refs/pull/"+n+"/head"); err != nil {
		d.FetchErr = err
	} else {
		d.Fetched = true
	}
	return d, nil
}
