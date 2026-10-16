package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ariesworx/aries/internal/agentsetup"
	"github.com/ariesworx/aries/internal/client"
	"github.com/ariesworx/aries/internal/proto"
)

// cmdSetup prints, or with --write makes, an agent's Aries setup: the
// MCP registration, the pointer block in its instruction file and, where
// the harness has them, the SessionStart hook and Claude Code's usage
// hooks. With --all it does so for
// every agent. It edits the project's own files unless --global says the
// person's home ones may be touched.
func cmdSetup(_ context.Context, r *runner, args []string) error {
	const usage = "setup AGENT|--all [--write|--check|--remove] [--global [--key-file PATH]] [--command PATH]"
	fs := r.newFlags("setup")
	write := fs.Bool("write", false, "edit the files")
	check := fs.Bool("check", false, "fail unless everything is in place")
	remove := fs.Bool("remove", false, "take aries out of the files")
	global := fs.Bool("global", false, "the user's files in the home directory, not the project's")
	all := fs.Bool("all", false, "every supported agent")
	entry := agentsetup.DefaultEntry
	fs.StringVar(&entry.Command, "command", entry.Command, "how the agent runs aries")
	keyFile := fs.String("key-file", "", "the key file the agent's aries signs in with (ARIES_KEY_FILE in its entry); only for your own files: --global, or a desktop app")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if set(fs, "key-file") {
		if *remove {
			return usagef(usage, "--remove takes no --key-file: it takes the entry out, and the key file's path with it")
		}
		if entry.KeyFile, err = r.keyFileOption(*keyFile); err != nil {
			return err
		}
	}
	names := pos
	switch {
	case *all && len(pos) > 0:
		return usagef(usage, "--all takes no agent")
	case *all:
		// A desktop app's config is the person's, outside the
		// repository, and pins this checkout: it is set up by name only.
		names = nil
		for _, n := range agentsetup.Names() {
			if !agentsetup.Agents[n].Desktop {
				names = append(names, n)
			}
		}
	case len(pos) != 1:
		return usagef(usage, "setup needs one agent, or --all: %s", strings.Join(agentsetup.Names(), ", "))
	}
	n := 0
	for _, b := range []bool{*write, *check, *remove} {
		if b {
			n++
		}
	}
	if n > 1 {
		return usagef(usage, "give at most one of --write, --check and --remove")
	}
	mode := setupMode{write: *write, check: *check, remove: *remove}
	if entry.Command == "" || strings.ContainsAny(entry.Command, "\x00\n") {
		return usagef(usage, "--command must be a program name or path")
	}
	var plans []setupPlan
	for _, name := range names {
		agent, ok := agentsetup.Agents[name]
		if !ok {
			return usagef(usage, "unknown agent %q; supported: %s", name, strings.Join(agentsetup.Names(), ", "))
		}
		if agent.Desktop {
			command := ""
			if set(fs, "command") {
				command = entry.Command
			}
			return r.setupDesktop(agent, command, entry.KeyFile, mode)
		}
		p := setupPlan{Agent: agent}
		if *global && agent.Global == "" {
			if !*all {
				return proto.Errf(proto.CodeInvalid, agent.NoGlobal, noGlobal(agent))
			}
			p.skipped = agent.NoGlobal
		}
		plans = append(plans, p)
	}

	if entry.KeyFile != "" && !*global {
		// A project's files are committed, and a path on one person's
		// machine does not belong in them.
		return proto.Errf(proto.CodeInvalid,
			fmt.Sprintf("add --global to write it into your own files, or set %s in the environment the agent starts in, such as your shell's profile", agentsetup.KeyFileEnv),
			"--key-file is only for your own files: the project's are committed and shared, and a key file's path is yours alone")
	}
	root, err := r.setupRoot(*global)
	if err != nil {
		return err
	}
	again := "aries setup " + names[0]
	if *all {
		again = "aries setup --all"
	}
	if *global {
		again += " --global"
	}
	base := fileBase{dir: root, user: *global, home: root}
	// Read each file once: two parts may share one (Gemini's settings,
	// AGENTS.md for Codex and Junie), and each sees the last one's edit.
	// Printing reads nothing.
	disk := map[string]*diskFile{}
	var order []*diskFile
	for i := range plans {
		p := &plans[i]
		if p.skipped != "" {
			continue
		}
		for _, t := range p.Targets(*global) {
			path := filepath.Join(root, filepath.FromSlash(t.Path))
			d, ok := disk[path]
			if !ok {
				d = &diskFile{path: path, base: base}
				if mode != (setupMode{}) {
					if err := d.read(); err != nil {
						return err
					}
				}
				disk[path] = d
				order = append(order, d)
			}
			f := setupFile{Target: t, disk: d, rel: t.Path}
			if *global {
				f.rel = path
			}
			p.files = append(p.files, f)
		}
	}
	r.keyFileNotes(plans, entry, mode)
	return r.applySetup(plans, entry, mode, *all, *global, again, order)
}

// keyFileOption checks --key-file and returns the path to write: the
// file's absolute path, with "~/" resolved. What was given is not
// trusted to be a path, and is never repeated when it is plainly a key.
func (r *runner) keyFileOption(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	path, _, err := (&client.Config{}).KeyFile(func(k string) string {
		if k == client.EnvKeyFile {
			return value
		}
		return ""
	}, r.env.UserHomeDir)
	if pe, ok := errors.AsType[*proto.Error](err); ok {
		return "", proto.Errf(proto.CodeInvalid, "give --key-file the path of your key file, such as ~/.ssh/id_ed25519: it takes the file's path, never the key itself",
			strings.Replace(pe.Message, client.EnvKeyFile, "--key-file", 1))
	}
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() {
		return "", proto.Errf(proto.CodeInvalid, "give --key-file the path of the key file the agent is to sign in with",
			fmt.Sprintf("--key-file %s is not a file", path))
	}
	return path, nil
}

// keyFileNotes says, on stderr, what setup's files hold of the key file
// beyond what was asked: an entry that sets one when --key-file was not
// given, since the entry is replaced whole and --check compares it
// whole; and, when one is written, that an agent's hooks do not see it.
func (r *runner) keyFileNotes(plans []setupPlan, entry agentsetup.Entry, mode setupMode) {
	if mode.remove {
		return
	}
	for _, p := range plans {
		hooks := false
		for _, f := range p.files {
			hooks = hooks || f.Kind == agentsetup.KindHook
			if f.Kind != agentsetup.KindMCP || mode == (setupMode{}) {
				continue
			}
			had := p.KeyFile(f.disk.orig, entry)
			switch {
			case had == "" || had == entry.KeyFile:
			case mode.write:
				_, _ = fmt.Fprintf(r.env.Stderr, "aries: note: %s set %s=%s for %s, and this run replaces the entry: pass --key-file to keep or change it\n",
					esc(f.rel), agentsetup.KeyFileEnv, esc(had), p.Name)
			default:
				_, _ = fmt.Fprintf(r.env.Stderr, "aries: note: %s sets %s=%s for %s: pass --key-file %s to check for it\n",
					esc(f.rel), agentsetup.KeyFileEnv, esc(had), p.Name, esc(had))
			}
		}
		if entry.KeyFile != "" && hooks && mode.write {
			_, _ = fmt.Fprintf(r.env.Stderr, "aries: note: %s's hooks run with its own environment, not the MCP entry's: set %s there too (your shell's profile), or the hooks sign in as whoever your default keys are; `aries prime` names the principal and the key\n",
				p.Title, agentsetup.KeyFileEnv)
		}
	}
}

// setupMode is what setup was asked to do; none set means print.
type setupMode struct{ write, check, remove bool }

// applySetup checks, prints or edits the plans' files, read already,
// and reports. again is the command that would set them up; order
// holds each file once, in the order read.
func (r *runner) applySetup(plans []setupPlan, entry agentsetup.Entry, mode setupMode, all, global bool, again string, order []*diskFile) error {
	var err error
	switch {
	case mode.check:
		var missing []string
		for _, p := range plans {
			var m []string
			for _, f := range p.files {
				if f.Registered(f.disk.orig, entry) {
					continue
				}
				if hooks := f.MissingHooks(f.disk.orig, entry); f.Kind == agentsetup.KindHook && len(hooks) > 0 {
					m = append(m, f.rel+" ("+hookList(hooks)+")")
					continue
				}
				m = append(m, f.rel)
			}
			if len(m) > 0 {
				missing = append(missing, p.Title+" in "+strings.Join(m, ", "))
			}
		}
		if len(missing) > 0 {
			return proto.Errf(proto.CodeNotFound, "run `"+again+" --write`",
				"aries is not set up for "+strings.Join(missing, "; "))
		}
		r.report(plans, all, "registered")
		return nil
	case !mode.write && !mode.remove:
		r.printPlans(plans, entry, all, global)
		return nil
	}
	// Work out every edit before writing any, so a file that cannot be
	// parsed leaves all of them as they were.
	for i := range plans {
		files := plans[i].files
		for j := range files {
			f := &files[j]
			var out []byte
			if mode.remove {
				out, f.res, err = f.Remove(f.disk.cur, entry)
			} else {
				out, f.res, err = f.Apply(f.disk.cur, entry)
			}
			if err != nil {
				return proto.Errf(proto.CodeInvalid, "fix the file by hand, or move it aside and rerun",
					fmt.Sprintf("cannot edit %s: %v", f.rel, err))
			}
			if f.res == agentsetup.Unchanged {
				continue
			}
			f.disk.cur = out
			// An emptied pointer or hook file was Aries's alone; an
			// emptied MCP config is kept, as the person's file.
			f.disk.drop = len(out) == 0 && f.res == agentsetup.Removed && f.Kind != agentsetup.KindMCP
		}
	}
	for _, d := range order {
		switch {
		case string(d.cur) == string(d.orig):
			continue
		case d.drop:
			err = removeConfig(d.real)
		default:
			err = d.base.writeConfig(d.path, d.real, d.cur, d.mode)
		}
		if err != nil {
			return err
		}
	}
	for i := range plans {
		p := &plans[i]
		for _, f := range p.files {
			switch {
			case f.res != agentsetup.Added:
			case !global:
				p.note = p.Steps(entry)
			case f.Kind == agentsetup.KindHook:
				p.note = globalHookNote
			}
		}
	}
	r.report(plans, all, "")
	return nil
}

// hookList names hook events: "SessionStart hook", "Stop and
// SessionEnd hooks", "A, B and C hooks".
func hookList(events []string) string {
	if len(events) < 2 {
		return strings.Join(events, "") + " hook"
	}
	return strings.Join(events[:len(events)-1], ", ") + " and " + events[len(events)-1] + " hooks"
}

// globalHookNote is said after setup adds or prints hooks in the home
// directory. They run prime and usage, which dial the server named by the
// .aries.yaml of whatever repository the agent opens (C-14 in the
// security review): a cloned repository chooses the host. Per-project
// hooks have the same reach but are part of the repository the person
// chose to set up.
const globalHookNote = "These hooks run in every repository with a .aries.yaml that you open with the agent, and connect to the server that file names; Claude Code's usage hooks send it the session's token counts. Turn them off before opening a repository you do not trust, or drop --global and set the hooks up per project."

// noGlobal is the message refusing --global for a, whose fix is
// a.NoGlobal.
func noGlobal(a agentsetup.Agent) string {
	if a.ManualMCP() {
		return a.Title + " keeps its MCP servers in the IDE's settings, not a file setup edits"
	}
	return a.Title + " keeps its user MCP config in a per-platform profile that setup does not edit"
}

// setupPlan is one agent's files and what setup did to them.
type setupPlan struct {
	agentsetup.Agent
	files []setupFile
	// skipped is the fix for an agent --all --global passes over.
	skipped string
	// note is said after the agent's results.
	note string
}

// shared reports whether another of the agent's parts is in f's file.
func (p setupPlan) shared(f setupFile) bool {
	for _, g := range p.files {
		if g.disk == f.disk && g.Kind != f.Kind {
			return true
		}
	}
	return false
}

// diskFile is one file on disk: as read, and with the edits so far.
type diskFile struct {
	// path is the file as setup names it; real is where it is read
	// and written, once base has checked the way there.
	path, real string
	// base is how the file may be reached.
	base fileBase
	// orig is the file as read, empty if it does not exist; cur is orig
	// with the edits so far.
	orig, cur []byte
	// mode is the file's permission bits, or a new file's.
	mode fs.FileMode
	// drop: the last edit emptied a file that held only Aries's part.
	drop bool
}

// read checks the way to the file and reads it.
func (d *diskFile) read() error {
	var err error
	if d.real, err = d.base.resolve(d.path); err != nil {
		return err
	}
	if d.orig, d.mode, err = d.base.readConfig(d.real); err != nil {
		return err
	}
	d.cur = d.orig
	return nil
}

// setupFile is one target and the file it lives in.
type setupFile struct {
	agentsetup.Target
	disk *diskFile
	// rel is the path printed for the file: relative to the repository
	// root, or absolute for the person's own files.
	rel string
	// res is what the edit did to the target.
	res agentsetup.Result
}

// result is fixed, if set, else what the edit did.
func (f setupFile) result(fixed string) string {
	if fixed != "" {
		return fixed
	}
	return f.res.String()
}

// report prints each file's result, or fixed for all of them: a line per
// file for one agent, a line per agent for --all.
func (r *runner) report(plans []setupPlan, all bool, fixed string) {
	type line struct {
		Path   string `json:"path"`
		Kind   string `json:"kind"`
		Result string `json:"result"`
	}
	type agentDoc struct {
		Agent   string `json:"agent"`
		Files   []line `json:"files"`
		Note    string `json:"note,omitempty"`
		Skipped string `json:"skipped,omitempty"`
	}
	docs := make([]agentDoc, 0, len(plans))
	var b strings.Builder
	for _, p := range plans {
		d := agentDoc{Agent: p.Name, Files: []line{}, Note: p.note}
		if p.skipped != "" {
			d.Skipped = p.skipped
			fmt.Fprintf(&b, "%s: skipped; fix: %s\n", p.Name, p.skipped)
			docs = append(docs, d)
			continue
		}
		var parts []string
		for _, f := range p.files {
			res := f.result(fixed)
			d.Files = append(d.Files, line{f.rel, f.Kind.String(), res})
			label := f.rel
			if p.shared(f) {
				label += " (" + f.Kind.String() + ")" // Gemini's settings hold two parts
			}
			if all {
				parts = append(parts, label+" "+res)
			} else {
				fmt.Fprintf(&b, "%s: %s\n", label, res)
			}
		}
		if all {
			fmt.Fprintf(&b, "%s: %s\n", p.Name, strings.Join(parts, ", "))
		}
		if p.note != "" {
			note := strings.TrimSuffix(p.note, "\n")
			if all {
				note = "  " + strings.ReplaceAll(note, "\n", "\n  ")
			}
			b.WriteString(note + "\n")
		}
		docs = append(docs, d)
	}
	switch {
	case r.json && all:
		r.emit(struct {
			Agents []agentDoc `json:"agents"`
		}{docs})
	case r.json:
		r.emit(struct {
			Files []line `json:"files"`
		}{docs[0].Files})
	default:
		_, _ = io.WriteString(r.env.Stdout, b.String())
	}
}

// printPlans prints the edits setup would make, as snippets a person can
// also add by hand.
func (r *runner) printPlans(plans []setupPlan, entry agentsetup.Entry, all, global bool) {
	if r.json {
		type snip struct {
			Path    string `json:"path"`
			Kind    string `json:"kind"`
			Snippet string `json:"snippet"`
		}
		type agentDoc struct {
			Agent   string `json:"agent"`
			Files   []snip `json:"files"`
			Skipped string `json:"skipped,omitempty"`
		}
		docs := make([]agentDoc, 0, len(plans))
		for _, p := range plans {
			d := agentDoc{Agent: p.Name, Files: []snip{}, Skipped: p.skipped}
			for _, f := range p.files {
				d.Files = append(d.Files, snip{f.rel, f.Kind.String(), f.Snippet(entry)})
			}
			docs = append(docs, d)
		}
		if all {
			r.emit(struct {
				Agents []agentDoc `json:"agents"`
			}{docs})
		} else {
			r.emit(struct {
				Files []snip `json:"files"`
			}{docs[0].Files})
		}
		return
	}
	for i, p := range plans {
		if i > 0 {
			_, _ = io.WriteString(r.env.Stdout, "\n")
		}
		r.printSnippets(p, entry, all, global)
	}
}

// printSnippets prints one agent's snippets, each under a comment line
// saying where it goes.
func (r *runner) printSnippets(plan setupPlan, entry agentsetup.Entry, all, global bool) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(r.env.Stdout, format, a...) }
	agent := plan.Agent
	if plan.skipped != "" {
		p("# %s: skipped; fix: %s\n", agent.Title, plan.skipped)
		return
	}
	again := "aries setup " + agent.Name
	if all {
		again = "aries setup --all"
	}
	if global {
		again += " --global"
	}
	p("# %s: run `%s --write` to make these edits, or make them by hand\n", agent.Title, again)
	if agent.ManualMCP() && !global {
		p("\n# MCP server: %s\n%s", agent.Note, agent.Snippet(entry))
	}
	for _, f := range plan.files {
		switch f.Kind {
		case agentsetup.KindMCP:
			p("\n# MCP server: add to %s\n%s", f.rel, f.Snippet(entry))
			if agent.Note != "" && !global {
				p("# %s\n", agent.Note)
			}
		case agentsetup.KindPointer:
			p("\n# pointer: add to %s\n%s", f.rel, f.Snippet(entry))
		case agentsetup.KindHook:
			p("\n# %s: merge into %s\n%s", hookList(f.MissingHooks(nil, entry)), f.rel, f.Snippet(entry))
			if global {
				p("# %s\n", globalHookNote)
			}
		}
	}
	if agent.SessionEnv != "" {
		p("\n# session id: read from %s\n", agent.SessionEnv)
	} else {
		p("\n# session id: %s gives aries mcp none; aries mcp picks its own per process (m-…) unless ARIES_SESSION is set\n", agent.Title)
	}
}

// setupRoot is the directory the targets are relative to: the directory
// holding .aries.yaml, or a linked worktree's own top level when the file
// found is the main worktree's (Config.Checkout), or, with global, the
// home directory.
// For a repository, it warns when this machine lacks a program needed to
// reach the server (gcloud, for IAP).
func (r *runner) setupRoot(global bool) (string, error) {
	if global {
		home, err := r.env.UserHomeDir()
		if err != nil || home == "" {
			return "", proto.Errf(proto.CodeInvalid, "set HOME, or drop --global", "cannot find the home directory")
		}
		return home, nil
	}
	cfg, err := client.LoadConfig(r.dir)
	if err != nil {
		if _, ok := errors.AsType[*proto.Error](err); ok {
			return "", err
		}
		return "", proto.Errf(proto.CodeInvalid, "correct "+client.ConfigFile+"; see docs/cli.md, Connect a repository", err.Error())
	}
	// The agent can be set up regardless, but it will not connect.
	if pe, ok := errors.AsType[*proto.Error](cfg.CheckTransport()); ok {
		_, _ = fmt.Fprintf(r.env.Stderr, "aries: %s\nfix: %s\n", esc(pe.Message), esc(pe.Fix))
	}
	return cfg.Checkout, nil
}
