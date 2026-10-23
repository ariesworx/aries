package dispatch

import (
	"context"
	"time"

	"github.com/ariesworx/aries/internal/brief"
	"github.com/ariesworx/aries/internal/proto"
)

// Tracker is what the loop asks of aries. [Exec] runs the aries binary;
// tests stand in for it. An error the server returned is a *proto.Error.
type Tracker interface {
	// Ready lists up to n ready issues, in ready's order.
	Ready(ctx context.Context, n int) ([]proto.Summary, error)
	// Show reads one issue in full.
	Show(ctx context.Context, id string) (proto.ShowResult, error)
	// Start claims id under session for lease. A claim another session
	// holds is refused with proto.CodeConflict.
	Start(ctx context.Context, id, session string, lease time.Duration) (Started, error)
	// Handoff leaves note on id from session, releasing the claim when
	// release is set.
	Handoff(ctx context.Context, id, session, note string, release bool) error
	// Brief records text as the brief of the run rec names, on id, from
	// the dispatcher's own session (design §22): a session never briefs
	// itself.
	Brief(ctx context.Context, id string, rec brief.Record, text string) error
	// OpusOutput is the Opus output tokens of the UTC day that holds day.
	OpusOutput(ctx context.Context, day time.Time) (int64, error)
	// Paused reports whether anyone holds PauseLock.
	Paused(ctx context.Context) (bool, error)
	// Slots is how many hold RunsLock now.
	Slots(ctx context.Context) (int, error)
	// Watch calls wake with a source (WakeInbox, WakeEvent, WakeResync)
	// for each inbox item and issue event the server pushes, until ctx
	// ends or the watch does.
	Watch(ctx context.Context, wake func(source string)) error
}

// Started is a claimed issue, as start returns it, with its branch's
// name.
type Started struct {
	Issue   proto.Issue            `json:"issue"`
	Handoff *proto.Handoff         `json:"handoff,omitempty"`
	Claim   *proto.Claim           `json:"claim,omitempty"`
	Items   []proto.AcceptanceItem `json:"items,omitempty"`
	Branch  string                 `json:"branch"`
}

// Launcher starts headless runs under the lock RunsLock. [Claude] starts
// Claude Code; tests stand in for it.
type Launcher interface {
	// Launch starts the run sp describes, in its own process group.
	Launch(ctx context.Context, sp Spec) (Process, error)
	// Adopt finds a run a dispatcher before this one started: pid, still
	// running in session.
	Adopt(pid int, session string) (Process, bool)
}

// Spec is one run to launch.
type Spec struct {
	Issue, Session, Model string
	// Dir is the worktree it runs in, on Branch.
	Dir, Branch string
	// Brief is given on stdin, never in argv or the environment.
	Brief string
	// Env is its whole environment, with ARIES_SESSION and the key.
	Env []string
	// Log is the file its output goes to.
	Log string
	// Capacity and Lease are the lock's.
	Capacity int
	Lease    time.Duration
	// Policy is the run's snapshot of the policy, which Env names to the
	// guard (guard.EnvPolicy); Allowed and Denied are Claude Code's tool
	// lists rendered from it.
	Policy          string
	Allowed, Denied []string
}

// Process is a launched or adopted run.
type Process interface {
	PID() int
	// Done is closed when the run has ended; Exit then says how.
	Done() <-chan struct{}
	Exit() Exit
	// Interrupt sends its process group SIGINT; Terminate, SIGTERM.
	Interrupt() error
	Terminate() error
}

// Exit is how a run ended: its exit code and what it means.
type Exit struct {
	Code    int
	Outcome string
}

// Git makes and removes the dispatcher's worktrees, and reads the
// default branch's ref. [ExecGit] runs git.
type Git interface {
	// AddWorktree makes a worktree at path on branch, from the remote's
	// default branch. What an earlier run left there or on the branch is
	// kept aside, never committed, and described in found for the brief.
	AddWorktree(ctx context.Context, path, branch string) (found string, err error)
	// RemoveWorktree removes it without --force: one with changes is
	// refused.
	RemoveWorktree(ctx context.Context, path string) error
	// DeleteBranch deletes a local branch whose PR merged.
	DeleteBranch(ctx context.Context, branch string) error
	// OnRemote reports whether the remote has branch at the commit the
	// local branch is at: deleting the local one loses nothing.
	OnRemote(ctx context.Context, branch string) (bool, error)
	// RemoteHead is the remote default branch's commit.
	RemoteHead(ctx context.Context) (string, error)
	// Policy is the remote default branch's .aries/guard.yaml as of the
	// last fetch, and whether it has one.
	Policy(ctx context.Context) (b []byte, present bool, err error)
}

// Checks are a PR's checks, summed up.
const (
	ChecksPass    = "pass"
	ChecksFail    = "fail"
	ChecksPending = "pending"
)

// PR is a pull request, as the forge reports it.
type PR struct {
	Number int
	URL    string
	// State is OPEN, MERGED or CLOSED.
	State string
	// Checks is ChecksPass, ChecksFail, ChecksPending, or "" with none.
	Checks string
	// Head is the commit the PR's branch is at, which its checks ran on.
	Head string
}

// Forge finds a branch's pull request. [GH] runs gh.
type Forge interface {
	// PR returns branch's PR, or nil when it has none.
	PR(ctx context.Context, branch string) (*PR, error)
}
