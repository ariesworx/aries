package guard

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

// maxInput bounds the hook's input; a command past the parser's limit is
// unreadable anyway.
const maxInput = 1 << 20

// Hook is one run of `aries guard --hook`: Claude Code's PreToolUse
// input on Stdin, its decision as JSON from Run.
type Hook struct {
	Stdin  io.Reader
	Getenv func(string) string
	// DataDir is aries's data directory, where snapshots and the count of
	// denials are kept; "" when there is none.
	DataDir string
	Now     func() time.Time
}

// hookInput is the part of Claude Code's PreToolUse input the guard
// reads (Hooks reference, "PreToolUse input").
type hookInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Command      string `json:"command"`
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	} `json:"tool_input"`
}

// hookOutput is the decision (Hooks reference, "PreToolUse decision
// control" and "JSON output").
type hookOutput struct {
	Specific      *specific `json:"hookSpecificOutput,omitempty"`
	SystemMessage string    `json:"systemMessage,omitempty"`
}

type specific struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
}

// Run decides the call on Stdin and returns the JSON to print, or nil
// to print nothing (no decision, or not in a git repository). It never
// fails: when it cannot read its policy or its input, a Desktop session
// gets the built-in denies and a line saying so, and a headless run gets
// a denial (docs/design/guard.md §6).
func (h Hook) Run() []byte {
	headless := h.Getenv(EnvPolicy) != ""
	var in hookInput
	b, err := io.ReadAll(io.LimitReader(h.Stdin, maxInput+1))
	switch {
	case err != nil:
	case len(b) > maxInput:
		err = errors.New("the hook's input is larger than 1 MiB")
	default:
		if err = json.Unmarshal(b, &in); err != nil {
			err = errors.New("the hook's input is not Claude Code's PreToolUse JSON")
		}
	}
	if err != nil {
		if headless {
			return h.out(failClosed(err), "")
		}
		return h.out(Verdict{}, "aries guard: policy not applied: "+err.Error())
	}
	top := Top(in.Cwd)
	if top == "" {
		return nil
	}
	pol, mode, protected, perr := h.policy(in.SessionID, headless)
	c := Call{Tool: in.ToolName, Command: in.ToolInput.Command, FilePath: in.ToolInput.FilePath,
		Cwd: in.Cwd, Top: top, Getenv: h.Getenv, Protected: protected}
	if c.FilePath == "" {
		c.FilePath = in.ToolInput.NotebookPath
	}
	var v Verdict
	msg := ""
	switch {
	case perr != nil && mode == Headless:
		v = failClosed(perr)
	case perr != nil:
		v = BuiltinPolicy().Decide(c, Desktop)
		msg = "aries guard: policy not applied: " + perr.Error()
	default:
		v = pol.Decide(c, mode)
	}
	if v.Answer == Deny && h.DataDir != "" {
		count(h.DataDir, h.now(), in.SessionID, mode, in.ToolName, v.Rule)
	}
	return h.out(v, msg)
}

// policy reads the session's snapshot: the one EnvPolicy names, else the
// session's under the data directory. It returns the directories and
// files no call may write, and the session's mode, which is headless
// when EnvPolicy is set even if its snapshot cannot be read.
func (h Hook) policy(session string, headless bool) (*Policy, Mode, []string, error) {
	var protected []string
	if h.DataDir != "" {
		protected = append(protected, SnapshotDir(h.DataDir))
	}
	mode := Desktop
	if headless {
		mode = Headless
	}
	path := h.Getenv(EnvPolicy)
	if path != "" {
		protected = append(protected, path)
	} else {
		if h.DataDir == "" {
			return nil, mode, protected, errors.New("aries has no data directory to keep the session's snapshot in")
		}
		var err error
		if path, err = SnapshotPath(h.DataDir, session); err != nil {
			return nil, mode, protected, err
		}
	}
	b, err := os.ReadFile(path) //nolint:gosec // the session's own snapshot, in aries's data directory or where the dispatcher put it
	if errors.Is(err, os.ErrNotExist) {
		return nil, mode, protected, errors.New("no snapshot of the policy for this session: start a new session, whose `aries prime --hook` takes one")
	}
	if err != nil {
		return nil, mode, protected, errors.New("the session's snapshot of the policy cannot be read")
	}
	p, m, err := ReadSnapshot(b)
	if headless {
		m = Headless
	}
	return p, m, protected, err
}

func (h Hook) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// failClosed is a headless run's denial when the guard cannot decide.
func failClosed(err error) Verdict {
	return Verdict{Answer: Deny, Rule: "unavailable",
		Reason: "aries guard: the guard could not read its policy, so nothing runs: " + err.Error() + ".\n" +
			"fix: stop, and hand off saying the guard could not read its policy; the dispatcher's log has more."}
}

// out renders a verdict and a message for the person as the hook's
// JSON, or nil when there is neither.
func (h Hook) out(v Verdict, msg string) []byte {
	o := hookOutput{SystemMessage: msg}
	if v.Answer != None {
		o.Specific = &specific{HookEventName: "PreToolUse", PermissionDecision: string(v.Answer), PermissionDecisionReason: v.Reason}
	}
	if o.Specific == nil && msg == "" {
		return nil
	}
	b, _ := json.Marshal(o) // plain strings: cannot fail
	return b
}

// Top is the top of the git repository dir is in: the nearest
// directory up from it holding .git, a directory or a worktree's file,
// with its symlinks resolved; "" outside one.
func Top(dir string) string {
	if dir == "" || !filepath.IsAbs(dir) {
		return ""
	}
	dir = resolve(filepath.Clean(dir), 0)
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		up := filepath.Dir(dir)
		if up == dir {
			return ""
		}
		dir = up
	}
}

// maxCount bounds the count of denials; past it the file is moved to
// denials.jsonl.1, replacing the one before.
const maxCount = 1 << 20

// count appends one line to the count of denials: the time, the session,
// its mode, the tool and the rule, never the command, a path or a value
// (docs/design/guard.md §7). A failure is ignored.
func count(dataDir string, now time.Time, session string, m Mode, tool, rule string) {
	if len(session) > 128 {
		session = session[:128]
	}
	line, err := json.Marshal(struct {
		At      string `json:"at"`
		Session string `json:"session"`
		Mode    string `json:"mode"`
		Tool    string `json:"tool"`
		Rule    string `json:"rule"`
	}{now.UTC().Format(time.RFC3339), session, m.String(), tool, rule})
	if err != nil {
		return
	}
	dir := SnapshotDir(dataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	f := filepath.Join(dir, "denials.jsonl")
	if fi, err := os.Stat(f); err == nil && fi.Size() >= maxCount {
		_ = os.Rename(f, f+".1")
	}
	w, err := os.OpenFile(f, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // aries's own data directory
	if err != nil {
		return
	}
	_, _ = w.Write(append(line, '\n'))
	_ = w.Close()
}
