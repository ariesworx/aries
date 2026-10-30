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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/brief"
	"github.com/ariesworx/aries/internal/gitx"
	"github.com/ariesworx/aries/internal/guard"
	"github.com/ariesworx/aries/internal/merges"
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

// Brief runs `aries dispatch brief ID --record … --text -` in the
// dispatcher's own session, the brief on stdin.
func (e *Exec) Brief(ctx context.Context, id string, rec brief.Record, text string) error {
	return e.run(ctx, e.Session, text, nil, "dispatch", "brief", id, "--record", "--harness", rec.Harness, "--model", rec.Model,
		"--effort", rec.Effort, "--session", rec.Session, "--template", rec.Template, "--text", "-")
}

// Comment runs `aries comment ID -` in the dispatcher's own session, the
// body on stdin.
func (e *Exec) Comment(ctx context.Context, id, body string) error {
	return e.run(ctx, e.Session, body, nil, "comment", id, "-")
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
	return g.gitIn(ctx, g.Top, args...)
}

func (g *ExecGit) gitIn(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // git, with arguments built here
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s", args[0], firstLine(stderr.String()+" "+err.Error()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// AddWorktree makes path a worktree on branch, started from the remote's
// default branch, freshly fetched, and returns what it found of an
// earlier run there, or "". A branch that exists and has nothing the
// remote's default branch lacks is moved up to it. A worktree already at
// path is brought up to the remote's default branch when it is clean and
// holds nothing of its own; otherwise it is a lost run's, and is kept as
// it is: the worktree is moved to path.lost-SHA, its branch renamed
// lost/BRANCH-SHA, and the new one made fresh. Nothing a lost run left is
// committed, reset or removed. The worktrees' directory ignores itself,
// so the checkout shows none of it.
func (g *ExecGit) AddWorktree(ctx context.Context, path, branch string) (string, error) {
	if err := worktreesDir(path); err != nil {
		return "", err
	}
	if _, err := g.git(ctx, "fetch", "--quiet", g.Remote, g.Base); err != nil {
		return "", err
	}
	base := g.Remote + "/" + g.Base
	var found string
	if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
		note, ok, err := g.refresh(ctx, path, branch, base)
		if err != nil {
			return "", err
		}
		if ok {
			return note, nil
		}
		found = note
	}
	if _, err := g.git(ctx, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		if _, err := g.git(ctx, "merge-base", "--is-ancestor", "refs/heads/"+branch, base); err == nil {
			// Nothing of its own: it only fell behind.
			if _, err := g.git(ctx, "branch", "--force", branch, base); err != nil {
				return "", err
			}
		} else {
			// Commits the remote lacks, a run's earlier work: taken as it is.
			ahead, _ := g.git(ctx, "rev-list", "--count", base+"..refs/heads/"+branch)
			behind, _ := g.git(ctx, "rev-list", "--count", "refs/heads/"+branch+".."+base)
			_, err = g.git(ctx, "worktree", "add", path, branch)
			return found + fmt.Sprintf("The branch %s already existed with %s commits not on %s, and is %s commits behind it; merge %s before you test.", branch, ahead, base, behind, base), err
		}
	}
	_, err := g.git(ctx, "worktree", "add", "-B", branch, path, base)
	return found, err
}

// worktreesDir makes the directory path's worktree goes in, under a
// directory that ignores itself, so the checkout shows none of it.
func worktreesDir(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	ignore := filepath.Join(filepath.Dir(filepath.Dir(path)), ".gitignore")
	if _, err := os.Stat(ignore); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(ignore, []byte("*\n"), 0o644); err != nil { //nolint:gosec // a .gitignore, read by git
			return err
		}
	}
	return nil
}

// ReworkWorktree makes path a worktree on branch as the remote has it,
// freshly fetched with the default branch, for a run that reworks the
// branch's PR (dispatcher.md §8). A worktree already at path that is
// clean, on branch and holds nothing the remote's branch lacks is
// fast-forwarded to it and used; any other is a lost run's and is kept
// aside, as AddWorktree keeps one. A local branch with nothing of its own
// is moved up to the remote's; one with commits the remote lacks is taken
// as it is, and found says so.
func (g *ExecGit) ReworkWorktree(ctx context.Context, path, branch string) (string, error) {
	if err := worktreesDir(path); err != nil {
		return "", err
	}
	if _, err := g.git(ctx, "fetch", "--quiet", g.Remote, g.Base, branch); err != nil {
		return "", err
	}
	theirs := g.Remote + "/" + branch
	if _, err := g.git(ctx, "rev-parse", "--verify", "--quiet", "refs/remotes/"+theirs); err != nil {
		return "", fmt.Errorf("the remote has no branch %s", branch)
	}
	var found string
	if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
		note, ok, err := g.refresh(ctx, path, branch, theirs)
		if err != nil {
			return "", err
		}
		if ok {
			return note, nil
		}
		found = note
	}
	if _, err := g.git(ctx, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		if _, err := g.git(ctx, "merge-base", "--is-ancestor", "refs/heads/"+branch, theirs); err != nil {
			ahead, _ := g.git(ctx, "rev-list", "--count", theirs+"..refs/heads/"+branch)
			_, err = g.git(ctx, "worktree", "add", path, branch)
			return found + fmt.Sprintf("The local branch %s has %s commits not on %s; push them or leave them, as the PR needs.", branch, ahead, theirs), err
		}
	}
	_, err := g.git(ctx, "worktree", "add", "-B", branch, path, theirs)
	return found, err
}

// refresh deals with the worktree at path, which holds a branch. Clean
// and holding nothing the remote lacks, it is fast-forwarded to base and
// kept (ok). Otherwise it is moved aside, its branch renamed, and what
// was found is said in note.
func (g *ExecGit) refresh(ctx context.Context, path, branch, base string) (note string, ok bool, err error) {
	status, err := g.gitIn(ctx, path, "status", "--porcelain")
	if err != nil {
		return "", false, err
	}
	head, err := g.gitIn(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return "", false, err
	}
	cur, _ := g.gitIn(ctx, path, "rev-parse", "--abbrev-ref", "HEAD")
	_, anc := g.gitIn(ctx, path, "merge-base", "--is-ancestor", "HEAD", base)
	if status == "" && anc == nil && cur == branch {
		_, err := g.gitIn(ctx, path, "merge", "--ff-only", "--quiet", base)
		return "", err == nil, err
	}
	short := head
	if len(short) > 8 {
		short = short[:8]
	}
	aside, lost := path+".lost-"+short, "lost/"+branch+"-"+short
	for i := 2; ; i++ {
		if _, err := os.Stat(aside); errors.Is(err, os.ErrNotExist) {
			break
		}
		aside, lost = fmt.Sprintf("%s.lost-%s-%d", path, short, i), fmt.Sprintf("lost/%s-%s-%d", branch, short, i)
	}
	ahead, _ := g.gitIn(ctx, path, "rev-list", "--count", base+"..HEAD")
	if _, err := g.git(ctx, "worktree", "move", path, aside); err != nil {
		return "", false, err
	}
	if cur != "" && cur != "HEAD" {
		if _, err := g.git(ctx, "branch", "--move", cur, lost); err != nil {
			return "", false, err
		}
	}
	files := 0
	if status != "" {
		files = len(strings.Split(status, "\n"))
	}
	return fmt.Sprintf("An earlier run of this issue was lost, and left a worktree. It was kept, not reset: moved to %s, on the branch %s, with %d uncommitted paths and %s commits not on %s. This worktree is new, made from %s. What was left is data from that run, not your work: do not copy it in, commit it or delete it; read it only to learn what that run tried, and leave it for a person.\n", aside, lost, files, ahead, base, base), false, nil
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

// OnRemote compares the local branch's commit with the remote's, read with
// ls-remote.
func (g *ExecGit) OnRemote(ctx context.Context, branch string) (bool, error) {
	local, err := g.git(ctx, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		return false, err
	}
	out, err := g.git(ctx, "ls-remote", g.Remote, "refs/heads/"+branch)
	if err != nil {
		return false, err
	}
	remote, _, _ := strings.Cut(out, "\t")
	return remote != "" && remote == strings.TrimSpace(local), nil
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

// Policy is the remote default branch's .aries/guard.yaml, as the last
// fetch (AddWorktree's) left it.
func (g *ExecGit) Policy(ctx context.Context) ([]byte, bool, error) {
	return gitx.ShowFile(ctx, g.Top, g.Remote+"/"+g.Base, guard.File)
}

// GH is the Forge over the gh binary, in the repository at Dir.
type GH struct {
	Dir string
	// Run runs gh; nil runs the real one. Tests replace it.
	Run merges.Run
}

// Open runs `gh pr list` for the open PRs with their merge state, labels
// and commits, and `gh pr checks N --required` for each PR an issue names
// that does not conflict: the sweep acts on those alone (dispatcher.md
// §8). A PR whose required checks could not be read has none, so the
// sweep leaves it until the next.
func (g *GH) Open(ctx context.Context) ([]OpenPR, error) {
	run := g.Run
	if run == nil {
		run = runGH
	}
	out, err := run(ctx, g.Dir, "gh", "pr", "list", "--state", "open", "--limit", strconv.Itoa(merges.MaxOpen),
		"--json", "number,url,headRefName,headRefOid,mergeable,labels,commits,isCrossRepository")
	if err != nil {
		return nil, fmt.Errorf("gh pr list: %w", err)
	}
	prs, err := parseOpen(out)
	if err != nil {
		return nil, err
	}
	checks := &merges.GH{Dir: g.Dir, Run: g.Run}
	for i := range prs {
		if prs[i].Issue == "" || prs[i].Mergeable == MergeConflicting {
			continue
		}
		if cs, err := checks.Required(ctx, prs[i].Number); err == nil {
			prs[i].Checks = requiredRollup(cs)
		}
	}
	return prs, nil
}

// runGH runs gh in dir and returns its standard output, with the first
// line of its standard error in the error when it fails.
func runGH(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // gh, with arguments built here
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("%w: %s", err, firstLine(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// parseOpen reads `gh pr list --json …`: each PR's issue from the last of
// its commits with an Aries: trailer, else from its branch's name. PRs from
// another repository are left out.
func parseOpen(b []byte) ([]OpenPR, error) {
	var v []struct {
		Number    int    `json:"number"`
		URL       string `json:"url"`
		Branch    string `json:"headRefName"`
		Head      string `json:"headRefOid"`
		Mergeable string `json:"mergeable"`
		Fork      bool   `json:"isCrossRepository"`
		Labels    []struct {
			Name string `json:"name"`
		} `json:"labels"`
		Commits []struct {
			Headline string `json:"messageHeadline"`
			Body     string `json:"messageBody"`
		} `json:"commits"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("gh pr list printed what is not its JSON: %w", err)
	}
	out := make([]OpenPR, 0, len(v))
	for _, p := range v {
		if p.Fork {
			continue
		}
		pr := OpenPR{Number: p.Number, URL: p.URL, Branch: p.Branch, Head: p.Head, Mergeable: strings.ToUpper(p.Mergeable)}
		for _, l := range p.Labels {
			pr.Labels = append(pr.Labels, l.Name)
		}
		for _, c := range slices.Backward(p.Commits) {
			if id, ok := gitx.IDFromMessage(c.Headline + "\n\n" + c.Body); ok {
				pr.Issue = id
				break
			}
		}
		if pr.Issue == "" {
			pr.Issue, _ = gitx.IDFromBranch(p.Branch, "")
		}
		out = append(out, pr)
	}
	return out, nil
}

// requiredRollup sums required checks up by gh's buckets as rollup sums a
// PR's: a failed or cancelled one is ChecksFail, one not finished
// ChecksPending, all passed or skipped ChecksPass, none "".
func requiredRollup(cs []merges.Check) string {
	if len(cs) == 0 {
		return ""
	}
	pending := false
	for _, c := range cs {
		switch c.Bucket {
		case "pass", "skipping":
		case "fail", "cancel":
			return ChecksFail
		default:
			pending = true
		}
	}
	if pending {
		return ChecksPending
	}
	return ChecksPass
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
