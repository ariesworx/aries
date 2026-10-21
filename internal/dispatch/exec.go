package dispatch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// Exec is the Tracker over the aries binary: each call runs
// `aries -C DIR --json --no-queue …`, with ARIES_SESSION set to the
// session it acts in, and reads the one JSON document it prints. A write
// that cannot reach the server fails rather than waiting in the outbox,
// and an answer from this machine's cache counts as the server not
// reached: the dispatcher never acts on a stale copy.
type Exec struct {
	// Aries is the aries binary; Dir the repository it runs in.
	Aries, Dir string
	// Environ is the environment it runs aries in, before ARIES_SESSION.
	Environ []string
	// Session is the dispatcher's own session, for what it reads.
	Session string
}

// errorDoc is a refusal as `aries --json` prints it.
type errorDoc struct {
	Error *struct {
		Code    proto.Code `json:"code"`
		Message string     `json:"message"`
		Fix     string     `json:"fix"`
	} `json:"error"`
	Cached bool `json:"cached"`
}

// run runs aries with args as session, stdin given, and decodes its
// document into out.
func (e *Exec) run(ctx context.Context, session, stdin string, out any, args ...string) error {
	full := append([]string{"-C", e.Dir, "--json", "--no-queue"}, args...)
	cmd := exec.CommandContext(ctx, e.Aries, full...) //nolint:gosec // aries itself, with arguments the dispatcher builds
	cmd.Env = withEnv(e.Environ, "ARIES_SESSION", session)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	var doc errorDoc
	if err := json.Unmarshal(stdout.Bytes(), &doc); err == nil {
		switch {
		case doc.Error != nil:
			return &proto.Error{Code: doc.Error.Code, Message: doc.Error.Message, Fix: doc.Error.Fix}
		case doc.Cached:
			return proto.Errf(proto.CodeUnavailable, "check that the server can be reached",
				"aries "+args[0]+" answered from this machine's cache: the server was not reached")
		}
	}
	if runErr != nil {
		return fmt.Errorf("aries %s: %w: %s", args[0], runErr, firstLine(stderr.String()))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
		return fmt.Errorf("aries %s printed what is not its JSON: %w", args[0], err)
	}
	return nil
}

// Ready runs `aries ready -n N`.
func (e *Exec) Ready(ctx context.Context, n int) ([]proto.Summary, error) {
	var out proto.ListResult
	err := e.run(ctx, e.Session, "", &out, "ready", "-n", strconv.Itoa(n))
	return out.Issues, err
}

// Show runs `aries show ID`.
func (e *Exec) Show(ctx context.Context, id string) (proto.ShowResult, error) {
	var out proto.ShowResult
	err := e.run(ctx, e.Session, "", &out, "show", id)
	return out, err
}

// Start runs `aries start ID --for LEASE` in session.
func (e *Exec) Start(ctx context.Context, id, session string, lease time.Duration) (Started, error) {
	var out Started
	err := e.run(ctx, session, "", &out, "start", id, "--for", lease.String())
	return out, err
}

// Handoff runs `aries handoff ID - [--release]` in session, the note on
// stdin.
func (e *Exec) Handoff(ctx context.Context, id, session, note string, release bool) error {
	args := []string{"handoff", id, "-"}
	if release {
		args = append(args, "--release")
	}
	return e.run(ctx, session, note, nil, args...)
}

// Comment runs `aries comment ID -` in session, the body on stdin.
func (e *Exec) Comment(ctx context.Context, id, session, body string) error {
	return e.run(ctx, session, body, nil, "comment", id, "-")
}

// OpusOutput runs `aries cost` by model for the UTC day that holds day,
// and sums its Opus models' output.
func (e *Exec) OpusOutput(ctx context.Context, day time.Time) (int64, error) {
	since := day.UTC().Truncate(24 * time.Hour)
	var out proto.CostResult
	if err := e.run(ctx, e.Session, "", &out, "cost", "--since", since.Format(time.DateOnly),
		"--until", since.Add(24*time.Hour).Format(time.DateOnly), "--group-by", "model"); err != nil {
		return 0, err
	}
	return opusOutput(out), nil
}

// opusOutput sums the output tokens of every Opus model in a report by
// model.
func opusOutput(r proto.CostResult) int64 {
	var n int64
	for _, g := range r.Groups {
		if strings.Contains(strings.ToLower(g.Key), "opus") && g.Output != nil {
			n += *g.Output
		}
	}
	return n
}

// Paused reports whether anyone holds PauseLock.
func (e *Exec) Paused(ctx context.Context) (bool, error) {
	n, err := e.holders(ctx, PauseLock)
	return n > 0, err
}

// Slots counts who holds RunsLock.
func (e *Exec) Slots(ctx context.Context) (int, error) {
	return e.holders(ctx, RunsLock)
}

// holders counts who holds lock name.
func (e *Exec) holders(ctx context.Context, name string) (int, error) {
	var out proto.LocksResult
	if err := e.run(ctx, e.Session, "", &out, "lock", "list", name); err != nil {
		return 0, err
	}
	n := 0
	for _, l := range out.Locks {
		if l.Name == name {
			n += len(l.Holders)
		}
	}
	return n, nil
}

// Watch runs `aries --json watch --events` and calls wake for each line
// it prints, until it ends.
func (e *Exec) Watch(ctx context.Context, wake func(string)) error {
	cmd := exec.CommandContext(ctx, e.Aries, "-C", e.Dir, "--json", "watch", "--events") //nolint:gosec // aries itself
	cmd.Env = withEnv(e.Environ, "ARIES_SESSION", e.Session)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var line struct {
			Op string `json:"op"`
		}
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue
		}
		switch line.Op {
		case "inbox":
			wake(WakeInbox)
		case "event":
			wake(WakeEvent)
		case "resync":
			wake(WakeResync)
		}
	}
	return cmd.Wait()
}

// withEnv is env with key set to value, in place of any it had.
func withEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k != key {
			out = append(out, kv)
		}
	}
	if value != "" {
		out = append(out, key+"="+value)
	}
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// ExecGit is Git over the git binary, for the repository whose main
// worktree's top is Top, its remote Remote and default branch Base.
type ExecGit struct {
	Top, Remote, Base string
}

func (g *ExecGit) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", g.Top}, args...)...) //nolint:gosec // git, with arguments built here
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s", args[0], firstLine(stderr.String()+" "+err.Error()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// AddWorktree makes path a worktree on branch: from the remote's default
// branch, freshly fetched, when the branch is new; on the branch when it
// exists. A worktree already at path is used as it is. The worktrees'
// directory ignores itself, so the checkout shows none of it.
func (g *ExecGit) AddWorktree(ctx context.Context, path, branch string) error {
	if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	ignore := filepath.Join(filepath.Dir(filepath.Dir(path)), ".gitignore")
	if _, err := os.Stat(ignore); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(ignore, []byte("*\n"), 0o644); err != nil { //nolint:gosec // a .gitignore, read by git
			return err
		}
	}
	if _, err := g.git(ctx, "fetch", "--quiet", g.Remote, g.Base); err != nil {
		return err
	}
	if _, err := g.git(ctx, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		_, err = g.git(ctx, "worktree", "add", path, branch)
		return err
	}
	_, err := g.git(ctx, "worktree", "add", "-b", branch, path, g.Remote+"/"+g.Base)
	return err
}

// RemoveWorktree runs `git worktree remove PATH`, without --force.
func (g *ExecGit) RemoveWorktree(ctx context.Context, path string) error {
	_, err := g.git(ctx, "worktree", "remove", path)
	return err
}

// DeleteBranch runs `git branch -D BRANCH`.
func (g *ExecGit) DeleteBranch(ctx context.Context, branch string) error {
	_, err := g.git(ctx, "branch", "-D", branch)
	return err
}

// RemoteHead reads the remote default branch's commit with ls-remote.
func (g *ExecGit) RemoteHead(ctx context.Context) (string, error) {
	out, err := g.git(ctx, "ls-remote", g.Remote, "refs/heads/"+g.Base)
	if err != nil {
		return "", err
	}
	head, _, _ := strings.Cut(out, "\t")
	return head, nil
}

// GH is the Forge over the gh binary, in the repository at Dir.
type GH struct {
	Dir string
}

// PR runs `gh pr view BRANCH --json number,url,state,statusCheckRollup`.
func (g *GH) PR(ctx context.Context, branch string) (*PR, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", branch, "--json", "number,url,state,headRefOid,statusCheckRollup") //nolint:gosec // gh, with a branch start named
	cmd.Dir = g.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if strings.Contains(stderr.String(), "no pull requests found") {
			return nil, nil
		}
		return nil, fmt.Errorf("gh pr view: %s", firstLine(stderr.String()+" "+err.Error()))
	}
	return parsePR(stdout.Bytes())
}

// ghCheck is one entry of gh's statusCheckRollup: a check run (status
// and conclusion) or a commit status (state).
type ghCheck struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}

func parsePR(b []byte) (*PR, error) {
	var v struct {
		Number int       `json:"number"`
		URL    string    `json:"url"`
		State  string    `json:"state"`
		Head   string    `json:"headRefOid"`
		Checks []ghCheck `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("gh pr view printed what is not its JSON: %w", err)
	}
	return &PR{Number: v.Number, URL: v.URL, State: v.State, Head: v.Head, Checks: rollup(v.Checks)}, nil
}

// rollup sums a PR's checks up: a failure of any is ChecksFail; one not
// finished, ChecksPending; all passed or skipped, ChecksPass; none, "".
func rollup(checks []ghCheck) string {
	if len(checks) == 0 {
		return ""
	}
	pending := false
	for _, c := range checks {
		result := c.Conclusion
		if c.State != "" {
			result = c.State
		}
		switch strings.ToUpper(result) {
		case "SUCCESS", "NEUTRAL", "SKIPPED":
		case "FAILURE", "ERROR", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE", "STALE":
			return ChecksFail
		default: // "", PENDING, EXPECTED, or a check run not COMPLETED
			pending = true
		}
	}
	if pending {
		return ChecksPending
	}
	return ChecksPass
}

// RepoTop returns the top of the main worktree of the repository holding
// dir, from any of its linked worktrees: the parent of git's common
// directory. The dispatcher's worktrees go under it.
func RepoTop(ctx context.Context, dir string) (string, error) {
	g := &ExecGit{Top: dir}
	common, err := g.git(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return filepath.Dir(common), nil
}
