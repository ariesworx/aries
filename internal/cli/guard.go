package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ariesworx/aries/internal/guard"
	"github.com/ariesworx/aries/internal/offline"
	"github.com/ariesworx/aries/internal/proto"
)

const guardUsage = "guard --hook | --check [--policy FILE] [--headless] [COMMAND...]"

// cmdGuard is aries guard (docs/design/guard.md): Claude Code's
// PreToolUse hook over the repository's policy, and the check of the
// policy and of commands against it.
func cmdGuard(_ context.Context, r *runner, args []string) error {
	fs := r.newFlags("guard")
	var hook, check, headless bool
	var policy string
	fs.BoolVar(&hook, "hook", false, "run as Claude Code's PreToolUse hook: the call's JSON on stdin, the decision as JSON; never fails")
	fs.BoolVar(&check, "check", false, "load the policy, and say whether each COMMAND is allowed; exits 1 when one is not")
	fs.StringVar(&policy, "policy", "", "with --check: the policy `FILE` (default: .aries/guard.yaml at the repository's top)")
	fs.BoolVar(&headless, "headless", false, "with --check: decide as for a headless run, which denies a command it cannot read")
	pos, err := parse(fs, args, guardUsage)
	if err != nil {
		return err
	}
	switch {
	case hook == check:
		return usagef(guardUsage, "guard needs --hook or --check")
	case hook && len(pos) > 0:
		return usagef(guardUsage, "guard --hook takes no arguments")
	case hook:
		r.guardHook()
		return nil
	}
	return r.guardCheck(policy, headless, pos)
}

// guardHook decides the call on standard input. It always exits 0: what
// it cannot do, it says in its output (guard.Hook.Run).
func (r *runner) guardHook() {
	data, err := offline.DataDir(r.env.GOOS, r.env.Getenv, r.env.UserHomeDir)
	if err != nil {
		data = ""
	}
	stdin := r.env.Stdin
	if f, ok := stdin.(*os.File); ok {
		if st, err := f.Stat(); err != nil || st.Mode()&os.ModeCharDevice != 0 {
			stdin = strings.NewReader("") // a person at a terminal: not left waiting
		}
	}
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	if out := (guard.Hook{Stdin: stdin, Getenv: r.env.Getenv, DataDir: data}).Run(); out != nil {
		r.emit(json.RawMessage(out))
	}
}

// checked is one command's verdict, as --check prints it.
type checked struct {
	Command string `json:"command"`
	Answer  string `json:"answer"`
	Rule    string `json:"rule,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

func (r *runner) guardCheck(policy string, headless bool, cmds []string) error {
	cwd, err := filepath.Abs(r.dir)
	if err != nil {
		return err
	}
	top := guard.Top(cwd)
	if policy == "" {
		if top == "" {
			return proto.Errf(proto.CodeInvalid, "run it in a repository, or name the file with --policy", cwd+" is not in a git repository")
		}
		policy = filepath.Join(top, guard.File)
	}
	b, err := os.ReadFile(policy) //nolint:gosec // the policy file the person named
	if errors.Is(err, os.ErrNotExist) {
		return proto.Errf(proto.CodeInvalid, "add "+guard.File+": docs/design/guard.md §4 has a starting one",
			policy+": no policy file")
	}
	if err != nil {
		return err
	}
	p, err := guard.Load(b)
	if err != nil {
		return proto.Errf(proto.CodeInvalid, "correct the file and run aries guard --check again", policy+": "+err.Error())
	}
	if len(cmds) == 0 {
		if r.json {
			r.emit(map[string]any{"policy": policy, "allow": len(p.Allow), "deny": len(p.Deny), "tools": len(p.Tools)})
			return nil
		}
		_, _ = fmt.Fprintf(r.env.Stdout, "ok: %s: %d allow rules, %d deny rules, %d tools\n", policy, len(p.Allow), len(p.Deny), len(p.Tools))
		return nil
	}
	mode := guard.Desktop
	if headless {
		mode = guard.Headless
	}
	var out []checked
	refused := 0
	for _, c := range cmds {
		v := p.Decide(guard.Call{Tool: "Bash", Command: c, Cwd: cwd, Top: top, Getenv: r.env.Getenv}, mode)
		answer := string(v.Answer)
		if v.Answer == guard.None {
			answer = "none"
		}
		if v.Answer != guard.Allow {
			refused++
		}
		out = append(out, checked{Command: c, Answer: answer, Rule: v.Rule, Reason: v.Reason})
	}
	var refusal error
	if refused > 0 {
		refusal = proto.Errf(proto.CodeInvalid, "write the command in an allowed form, or change "+guard.File+" by pull request",
			fmt.Sprintf("%d of %d commands are not allowed", refused, len(cmds)))
	}
	if r.json {
		if refusal != nil {
			pe, _ := errors.AsType[*proto.Error](refusal)
			return &refusedWith{err: pe, key: "commands", value: out}
		}
		r.emit(map[string]any{"policy": policy, "commands": out})
		return nil
	}
	for _, c := range out {
		label := c.Answer
		switch {
		case c.Answer == "none":
			label = "no decision"
		case c.Rule != "":
			label += " (" + c.Rule + ")"
		}
		_, _ = fmt.Fprintf(r.env.Stdout, "%s: %s\n", label, esc(c.Command))
		for line := range strings.SplitSeq(c.Reason, "\n") {
			if line != "" {
				_, _ = fmt.Fprintf(r.env.Stdout, "  %s\n", line)
			}
		}
	}
	return refusal
}
