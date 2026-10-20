package dispatch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Claude launches headless Claude Code runs under the lock RunsLock
// (dispatcher.md §5, §10, §11):
//
//	aries -C WORKTREE lock run dispatch/runs --capacity N --for 10m -- \
//	  claude -p --session-id SESSION --model MODEL --output-format json \
//	  --permission-mode dontAsk --allowedTools … --disallowedTools …
//
// without --wait, so a full lock is refused and the run never starts.
// The brief is the run's stdin; its output goes to the run's log file.
// Each run is a process group of its own, which outlives the dispatcher.
type Claude struct {
	// Aries is the aries binary, which takes the lock; Claude is the
	// harness's binary ("claude" on PATH when empty).
	Aries, Claude string
	// MaxBudgetUSD, when set, is each run's --max-budget-usd.
	MaxBudgetUSD string
	// Allowed and Denied are its tool lists; DefaultAllowed and
	// DefaultDenied when nil.
	Allowed, Denied []string
}

// DefaultAllowed is what a run may do without asking, which is all it
// may do under dontAsk: read and edit its worktree, the aries tools, git,
// Go and the repository's gate, and gh to open and read its PR. A brief
// that asks for more than this grants is refused by the harness, which is
// what the lessons of 19 October 2026 warned of.
var DefaultAllowed = []string{
	"Read", "Edit", "Write", "Glob", "Grep", "TodoWrite", "mcp__aries",
	"Bash(git:*)", "Bash(go:*)", "Bash(gofmt:*)", "Bash(golangci-lint:*)", "Bash(shellcheck:*)",
	"Bash(scripts/gate.sh:*)", "Bash(scripts/install-postgres.sh:*)",
	"Bash(gh pr create:*)", "Bash(gh pr view:*)", "Bash(gh pr checks:*)", "Bash(gh pr list:*)", "Bash(gh run view:*)",
	"Bash(ls:*)", "Bash(cat:*)", "Bash(head:*)", "Bash(tail:*)", "Bash(wc:*)", "Bash(grep:*)", "Bash(find:*)",
	"Bash(sed -n:*)", "Bash(mkdir:*)", "Bash(diff:*)",
}

// DefaultDenied is what a run may never do, whatever it is allowed
// (dispatcher.md §11): merge, call gh's API, force a push or push to
// main, or edit the harness's settings, the MCP config or the release
// keys. Branch protection is the backstop.
var DefaultDenied = []string{
	"Bash(gh pr merge:*)", "Bash(gh api:*)",
	"Bash(git push --force:*)", "Bash(git push -f:*)", "Bash(git push --force-with-lease:*)",
	"Bash(git push origin main:*)", "Bash(git push origin HEAD:main:*)", "Bash(git push origin +*)",
	"Edit(.claude/**)", "Write(.claude/**)", "Edit(.mcp.json)", "Write(.mcp.json)",
	"Edit(internal/release/keys.go)", "Write(internal/release/keys.go)",
}

// Argv is the command line of the run sp describes. It holds no brief
// and no key.
func (c *Claude) Argv(sp Spec) []string {
	claude, allowed, denied := c.Claude, c.Allowed, c.Denied
	if claude == "" {
		claude = "claude"
	}
	if allowed == nil {
		allowed = DefaultAllowed
	}
	if denied == nil {
		denied = DefaultDenied
	}
	argv := []string{c.Aries, "-C", sp.Dir, "lock", "run", RunsLock,
		"--capacity", strconv.Itoa(sp.Capacity), "--for", sp.Lease.String(), "--",
		claude, "-p", "--session-id", sp.Session, "--model", sp.Model,
		"--output-format", "json", "--permission-mode", "dontAsk"}
	if c.MaxBudgetUSD != "" {
		argv = append(argv, "--max-budget-usd", c.MaxBudgetUSD)
	}
	argv = append(append(argv, "--allowedTools"), allowed...)
	return append(append(argv, "--disallowedTools"), denied...)
}

// Launch starts the run in its worktree, in a process group of its own,
// with the brief on stdin through a file that is gone once the run has
// it, and its output appended to sp.Log, 0600.
func (c *Claude) Launch(_ context.Context, sp Spec) (Process, error) {
	if err := os.MkdirAll(filepath.Dir(sp.Log), 0o700); err != nil {
		return nil, err
	}
	out, err := os.OpenFile(sp.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	in, err := briefFile(filepath.Dir(sp.Log), sp.Brief)
	if err != nil {
		_ = out.Close()
		return nil, err
	}
	defer func() { _ = in.Close() }()
	argv := c.Argv(sp)
	// Not CommandContext: the run outlives the dispatcher's context, and
	// is stopped by signal alone.
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // aries and the harness, with arguments built here
	cmd.Dir, cmd.Env = sp.Dir, sp.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, out
	ownGroup(cmd)
	if err := cmd.Start(); err != nil {
		_ = out.Close()
		return nil, err
	}
	p := &proc{pid: cmd.Process.Pid, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		_ = out.Close()
		p.exit = Classify(exitCode(err), tail(sp.Log, 64<<10))
		close(p.done)
	}()
	return p, nil
}

// briefFile writes the brief to a file of its own in dir, 0600, removes
// its name and returns it open at its start: the run reads it as stdin,
// and nothing is left on disk.
func briefFile(dir, brief string) (*os.File, error) {
	f, err := os.CreateTemp(dir, ".brief-*")
	if err != nil {
		return nil, err
	}
	_ = os.Remove(f.Name())
	if _, err := io.WriteString(f, brief); err != nil {
		_ = f.Close()
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// Adopt finds pid running with session in its command line, and watches
// it until it ends; how it ended is unknown.
func (c *Claude) Adopt(pid int, session string) (Process, bool) {
	if !alive(pid, session) {
		return nil, false
	}
	p := &proc{pid: pid, done: make(chan struct{}), exit: Exit{Code: -1, Outcome: OutcomeUnknown}}
	go func() {
		for alive(pid, session) {
			time.Sleep(5 * time.Second)
		}
		close(p.done)
	}()
	return p, true
}

// proc is a run's process group.
type proc struct {
	pid  int
	done chan struct{}
	exit Exit
}

func (p *proc) PID() int              { return p.pid }
func (p *proc) Done() <-chan struct{} { return p.done }
func (p *proc) Exit() Exit            { return p.exit }
func (p *proc) Interrupt() error      { return signalGroup(p.pid, false) }
func (p *proc) Terminate() error      { return signalGroup(p.pid, true) }

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		if c := ee.ExitCode(); c >= 0 {
			return c
		}
	}
	return 1
}

// tail is the last n bytes of the file at path.
func tail(path string, n int64) []byte {
	f, err := os.Open(path) //nolint:gosec // the run's own log, in the dispatcher's directory
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err == nil && fi.Size() > n {
		_, _ = f.Seek(-n, io.SeekEnd)
	}
	b, _ := io.ReadAll(f)
	return b
}

// authWords are what an authentication or billing failure says: every
// later run would fail the same way.
var authWords = []string{"invalid api key", "authentication", "/login", "oauth", "401", "unauthorized",
	"credit balance is too low", "x-api-key"}

// Classify says how a run ended from its exit code and output: Claude
// Code's JSON result, when it printed one, else what aries printed
// before the harness started.
func Classify(code int, out []byte) Exit {
	type result struct {
		Type    string `json:"type"`
		IsError bool   `json:"is_error"`
		Result  string `json:"result"`
	}
	var res result
	found := false
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var r result
		if json.Unmarshal(line, &r) == nil && r.Type == "result" {
			res, found = r, true
		}
	}
	switch {
	case found && !res.IsError && code == 0:
		return Exit{Code: code, Outcome: OutcomeOK}
	case found && authish(res.Result):
		return Exit{Code: code, Outcome: OutcomeAuth}
	case found:
		return Exit{Code: code, Outcome: OutcomeFailed}
	case authish(string(out)):
		return Exit{Code: code, Outcome: OutcomeAuth}
	case code == 0:
		return Exit{Code: code, Outcome: OutcomeOK}
	case refusedByAries(out):
		return Exit{Code: code, Outcome: OutcomeRefused}
	}
	return Exit{Code: code, Outcome: OutcomeFailed}
}

func authish(s string) bool {
	s = strings.ToLower(s)
	for _, w := range authWords {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// refusedByAries reports whether aries refused before the harness ran:
// its output is aries's lines alone, and no lock was lost mid-run.
func refusedByAries(out []byte) bool {
	s := strings.TrimSpace(string(out))
	if s == "" || strings.Contains(s, "lost lock") {
		return false
	}
	for line := range strings.SplitSeq(s, "\n") {
		if !strings.HasPrefix(line, "aries: ") && !strings.HasPrefix(line, "fix: ") && !strings.HasPrefix(line, "req: ") {
			return false
		}
	}
	return true
}
