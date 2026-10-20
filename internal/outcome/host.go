package outcome

import (
	"context"
	"time"
)

// Host is a CI host as the capture reads it (docs/design/outcome-record.md
// §5): the changes (pull requests, merge requests) of an issue, and for
// one of them its commits, CI runs and review counts. GitHub, through
// `gh`, is the first implementation (github.go); another forge or CI
// system is another implementation, and the record does not change.
//
// Decision 67 holds: a host is read by the client with the person's or
// the machine's own credentials, never by ariesd.
type Host interface {
	// Name is the host's name in a record's change and ci: "github".
	Name() string
	// Changes returns the changes whose branch is one of branches or is
	// the issue's by name (gitx.BranchFor), oldest first.
	Changes(ctx context.Context, issue string, branches []string) ([]Change, error)
	// Closed returns the changes closed without a merge since then, for
	// the capture of pr-closed outcomes over a window.
	Closed(ctx context.Context, since time.Time) ([]Change, error)
	// Detail reads one change's commits, runs and reviews, and fetches
	// its commits into the repository so that git can merge them.
	Detail(ctx context.Context, c Change) (Detail, error)
}

// Change is a pull request or its host's equivalent. Merged and Closed
// are zero while it is open; a change merged is also closed.
type Change struct {
	ID      string
	URL     string
	Base    string
	Branch  string
	Opened  time.Time
	Merged  time.Time
	Closed  time.Time
	Unmerge bool // closed without a merge
}

// Detail is what a host says of one change.
type Detail struct {
	// Commits are the change's commits, oldest first.
	Commits []Commit
	// Runs are its CI runs, each attempt of each workflow its own entry,
	// on its own commits (Queue false) and in the merge queue (Queue
	// true).
	Runs []Run
	// ChangesRequested counts reviews that requested changes; Comments
	// the review comments. Who wrote them is never read (decision 97).
	ChangesRequested int
	Comments         int
	// Rewritten says the change's branch was force-pushed, so its
	// catch-up merges may be gone (§6).
	Rewritten bool
	// Fetched says the commits are in the local repository, for
	// gitx.MergeConflicts; FetchErr says why not.
	Fetched  bool
	FetchErr error
}

// Commit is one commit of a change: its parents, first parent first, and
// its committer's time.
type Commit struct {
	SHA     string
	Parents []string
	At      time.Time
}

// Conclusions a host maps its runs' results to.
const (
	Pass      = "pass"      // succeeded, or was skipped or neutral
	Fail      = "fail"      // failed
	Cancelled = "cancelled" // cancelled, by a person or by a newer push
	Infra     = "infra"     // timed out, or never started
	Pending   = "pending"   // not finished
)

// Run is one attempt of one workflow on one commit.
type Run struct {
	ID         string
	Workflow   string
	Attempt    int
	SHA        string
	Queue      bool
	Conclusion string
	Started    time.Time
	Finished   time.Time
	// FailedJobs names the attempt's failed jobs, from the workflow's
	// definition; read for failed attempts only.
	FailedJobs []string
}
