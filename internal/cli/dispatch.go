package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ariesworx/aries/internal/brief"
	"github.com/ariesworx/aries/internal/client"
	"github.com/ariesworx/aries/internal/credential"
	"github.com/ariesworx/aries/internal/dispatch"
	"github.com/ariesworx/aries/internal/gitx"
	"github.com/ariesworx/aries/internal/guard"
	"github.com/ariesworx/aries/internal/offline"
	"github.com/ariesworx/aries/internal/proto"
)

const dispatchUsage = "dispatch run [--dry-run] [--once] [--capacity N] [--cap TOKENS] [--max-budget-usd USD] [--models M[,M]] [--labels L[,L]] [--clear]" +
	" | dispatch status | dispatch stop [--now] | dispatch key set|remove" +
	" | dispatch brief ID [--harness claude-code-desktop|claude-code] [--note TEXT] [--session S] [--worktree DIR] [--record --model M [--effort E]]"

// cmdDispatch is the client-side dispatcher (docs/design/dispatcher.md,
// decision 96): internal and experimental, removed when Bearings' B0
// takes over its loop, which lives in internal/dispatch. Here is only its
// wiring to the process. CLI only (rule 12).
func cmdDispatch(ctx context.Context, r *runner, args []string) error {
	args = r.leading(args)
	if len(args) == 0 {
		return usagef(dispatchUsage, "dispatch needs run, status, stop, key or brief")
	}
	switch args[0] {
	case "run":
		return dispatchRun(ctx, r, args[1:])
	case "status":
		return dispatchStatus(r, args[1:])
	case "stop":
		return dispatchStop(r, args[1:])
	case "key":
		return dispatchKey(ctx, r, args[1:])
	case "brief":
		return dispatchBrief(ctx, r, args[1:])
	}
	return usagef(dispatchUsage, "unknown dispatch action %q", args[0])
}

// dispatchPaths are the dispatcher's files for the project of the
// repository r runs in.
func (r *runner) dispatchPaths() (dispatch.Paths, error) {
	cfg, err := client.LoadConfig(r.dir)
	if err != nil {
		return dispatch.Paths{}, err
	}
	data, err := offline.DataDir(r.env.GOOS, r.env.Getenv, r.env.UserHomeDir)
	if err != nil {
		return dispatch.Paths{}, err
	}
	return dispatch.PathsFor(data, cfg.Project), nil
}

// refuseOffUnix refuses the loop on Windows (rule 14): runs are process
// groups, stopped by signal.
func (r *runner) refuseOffUnix() error {
	if r.env.GOOS == "windows" {
		return proto.Errf(proto.CodeInvalid, "run aries dispatch on macOS or Linux",
			"aries dispatch runs only on macOS and Linux: it starts runs as process groups and stops them by signal, which Windows has not")
	}
	return nil
}

func dispatchRun(ctx context.Context, r *runner, args []string) error {
	fs := r.newFlags("dispatch run")
	cfg := dispatch.DefaultConfig()
	fs.BoolVar(&cfg.DryRun, "dry-run", false, "say what it would start, skip and queue, and why; start nothing")
	once := fs.Bool("once", false, "wake once, then exit; runs started keep running")
	fs.IntVar(&cfg.Capacity, "capacity", cfg.Capacity, "the lock "+dispatch.RunsLock+"'s capacity: runs at once, of every dispatcher")
	fs.Int64Var(&cfg.OpusCap, "cap", cfg.OpusCap, "a UTC day's Opus output tokens past which it starts nothing")
	budget := fs.String("max-budget-usd", "", "each run's --max-budget-usd")
	models := fs.String("models", "", "start only issues whose model label is one of these (haiku,sonnet,opus)")
	labels := fs.String("labels", "", "start only issues carrying all of these labels")
	clearStop := fs.Bool("clear", false, "clear the stop CI failures or an auth failure left, and the hold on issues its runs failed on")
	pos, err := parse(fs, args, dispatchUsage)
	if err != nil {
		return err
	}
	switch {
	case len(pos) > 0:
		return usagef(dispatchUsage, "dispatch run takes no arguments")
	case r.json:
		return usagef(dispatchUsage, "dispatch run prints a line for each decision; --json is for dispatch status")
	case cfg.Capacity < 1 || cfg.Capacity > 16:
		return usagef(dispatchUsage, "--capacity must be 1 to 16")
	case cfg.OpusCap < 1:
		return usagef(dispatchUsage, "--cap must be at least 1")
	}
	if cfg.Filter, err = dispatch.ParseFilter(*models, *labels); err != nil {
		var pe *proto.Error
		if errors.As(err, &pe) {
			return usagef(dispatchUsage, "%s; fix: %s", pe.Message, pe.Fix)
		}
		return err
	}
	if err := r.refuseOffUnix(); err != nil {
		return err
	}
	paths, err := r.dispatchPaths()
	if err != nil {
		return err
	}
	top, err := dispatch.RepoTop(ctx, r.dir)
	if err != nil {
		return proto.Errf(proto.CodeInvalid, "run it in a git checkout of the project", "dispatch run: "+err.Error())
	}
	tmpl, err := brief.Load(top)
	if err != nil {
		return err
	}
	if err := checkPolicy(top); err != nil {
		return err
	}
	base, err := gitx.DefaultBranch(ctx, top)
	if err != nil || base == "" {
		return proto.Errf(proto.CodeInvalid, "fetch origin, so its default branch is known", "dispatch run: no default branch found")
	}
	aries, err := r.env.Executable()
	if err != nil {
		return err
	}
	key := ""
	if !cfg.DryRun {
		if key, err = r.dispatchKey(ctx); err != nil {
			return err
		}
	}
	state, err := dispatch.LoadState(paths.State)
	if err != nil {
		return err
	}
	save := func(s *dispatch.State) error { return s.Write(paths.State) }
	if !cfg.DryRun {
		if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
			return err
		}
		unlock, err := dispatch.LockInstance(paths.Lock)
		if err != nil {
			return err
		}
		defer unlock()
		_ = os.Remove(paths.Stop) // a stop asked of a dispatcher before this one
		if *clearStop {
			state.Clear()
		}
		state.PID, state.Since, state.DryRun = os.Getpid(), r.env.Now().UTC(), false
		if err := save(state); err != nil {
			return err
		}
		defer func() {
			state.PID = 0
			_ = save(state)
			_ = os.Remove(paths.Stop)
		}()
	}
	// A dry run opens no log file: it writes nothing, not even an empty file.
	logf := io.Discard
	if !cfg.DryRun {
		f, err := openLog(paths)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		logf = f
	}
	env := r.env.Environ()
	d := &dispatch.Dispatcher{
		Cfg: cfg,
		Tracker: &dispatch.Exec{Aries: aries, Dir: top, Environ: env,
			Session: "dispatch-" + randomHex(4)},
		Launcher:     &dispatch.Claude{Aries: aries, MaxBudgetUSD: *budget},
		Git:          &dispatch.ExecGit{Top: top, Remote: "origin", Base: base},
		Forge:        &dispatch.GH{Dir: top},
		Out:          r.env.Stdout,
		Log:          logf,
		Now:          r.env.Now,
		NewSession:   newUUID,
		Worktrees:    filepath.Join(top, ".aries-dispatch", "worktrees"),
		RunLogs:      paths.Runs,
		Key:          key,
		Environ:      env,
		AgentOTLP:    envValue(env, "ARIES_AGENT_OTLP_ENDPOINT"),
		FindPostgres: func() string { return dispatch.FindPostgres(env) },
		State:        state,
		Save:         save,
		StopRequest:  func() string { return dispatch.ReadStop(paths.Stop) },
		Template:     tmpl,
	}
	if cfg.DryRun {
		r.notef("dry run: it starts, claims and writes nothing")
	}
	if *once {
		if !cfg.DryRun {
			d.Adopt(ctx)
		}
		return d.Wake(ctx, dispatch.WakeStart)
	}
	return d.Loop(ctx)
}

// checkPolicy refuses a repository whose checkout has no policy for the
// guard, or one it cannot load (docs/design/guard.md §8, decision 4).
// Each run reads its own snapshot of the default branch's, taken at its
// launch; this says at the start what would stop every run.
func checkPolicy(top string) error {
	b, err := os.ReadFile(filepath.Join(top, guard.File)) //nolint:gosec // the repository's own policy file
	if errors.Is(err, os.ErrNotExist) {
		return proto.Errf(proto.CodeInvalid, "add "+guard.File+" by pull request: docs/design/guard.md §4 has a starting one",
			"dispatch run: "+guard.File+" is missing: runs take their permissions from it, so none starts without it")
	}
	if err != nil {
		return err
	}
	if _, err := guard.Load(b); err != nil {
		return proto.Errf(proto.CodeInvalid, "correct it by pull request, and check it with aries guard --check",
			"dispatch run: "+guard.File+": "+err.Error())
	}
	return nil
}

// openLog opens the dispatcher's log for appending, 0600 in its 0700
// directory.
func openLog(paths dispatch.Paths) (*os.File, error) {
	if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(paths.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// envValue is the value of name in env (NAME=value strings), or "".
func envValue(env []string, name string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v
		}
	}
	return ""
}

// dispatchKey reads the API key from the credential store, and says
// which store it read.
func (r *runner) dispatchKey(ctx context.Context) (string, error) {
	store, err := r.credentials(ctx)
	if err != nil {
		return "", err
	}
	key, ok, err := store.Get(ctx, dispatch.KeyName)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", proto.Errf(proto.CodeInvalid, "keep one with `aries dispatch key set`, its key on standard input",
			"no API key is kept for aries dispatch in "+store.Name())
	}
	r.notef("the API key was read from %s", store.Name())
	return key, nil
}

// credentials opens the store the API key is kept in.
func (r *runner) credentials(ctx context.Context) (credential.Store, error) {
	if r.env.Credentials != nil {
		return r.env.Credentials(ctx)
	}
	return credential.Open(ctx, credential.Options{GOOS: r.env.GOOS, Getenv: r.env.Getenv, Home: r.env.UserHomeDir})
}

func dispatchKey(ctx context.Context, r *runner, args []string) error {
	fs := r.newFlags("dispatch key")
	pos, err := parse(fs, args, dispatchUsage)
	if err != nil {
		return err
	}
	if len(pos) != 1 || (pos[0] != "set" && pos[0] != "remove") {
		return usagef(dispatchUsage, "dispatch key takes set or remove")
	}
	store, err := r.credentials(ctx)
	if err != nil {
		return err
	}
	if pos[0] == "remove" {
		if err := store.Remove(ctx, dispatch.KeyName); err != nil {
			return err
		}
		return r.say(map[string]string{"removed": store.Name()}, "removed the API key from %s", store.Name())
	}
	if r.env.Terminal() {
		return proto.Errf(proto.CodeInvalid, "pipe the key in, from a password manager or a file of your own: `aries dispatch key set < FILE`",
			"aries dispatch key set reads the key from standard input, so that it never shows on the screen")
	}
	b, err := io.ReadAll(io.LimitReader(r.env.Stdin, 4096))
	if err != nil {
		return err
	}
	key := strings.TrimSpace(string(b))
	if key == "" || strings.ContainsAny(key, " \t\r\n") {
		return proto.Errf(proto.CodeInvalid, "give the key alone, on one line, on standard input",
			"standard input holds no key, or more than one word")
	}
	if err := store.Set(ctx, dispatch.KeyName, key); err != nil {
		return err
	}
	return r.say(map[string]string{"kept": store.Name()}, "kept the API key in %s", store.Name())
}

// say prints doc with --json, else a line.
func (r *runner) say(doc any, format string, a ...any) error {
	if r.json {
		r.emit(doc)
		return nil
	}
	_, _ = fmt.Fprintf(r.env.Stdout, format+"\n", a...)
	return nil
}

func dispatchStop(r *runner, args []string) error {
	fs := r.newFlags("dispatch stop")
	now := fs.Bool("now", false, "also interrupt its runs (SIGINT, then SIGTERM after 30s)")
	pos, err := parse(fs, args, dispatchUsage)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef(dispatchUsage, "dispatch stop takes no arguments")
	}
	paths, err := r.dispatchPaths()
	if err != nil {
		return err
	}
	if !dispatch.Running(paths.Lock) {
		return r.say(map[string]bool{"running": false},
			"no dispatcher runs for this project on this machine; to stop every dispatcher, take the lock: `aries lock acquire %s --for 24h`", dispatch.PauseLock)
	}
	mode, does := dispatch.StopDrain, "it starts nothing more, lets its runs end, then exits"
	if *now {
		mode, does = dispatch.StopNow, "it interrupts its runs, then exits"
	}
	if err := dispatch.WriteStop(paths.Stop, mode); err != nil {
		return err
	}
	return r.say(map[string]any{"running": true, "stop": mode}, "asked the dispatcher to stop: %s", does)
}

func dispatchStatus(r *runner, args []string) error {
	fs := r.newFlags("dispatch status")
	pos, err := parse(fs, args, dispatchUsage)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef(dispatchUsage, "dispatch status takes no arguments")
	}
	paths, err := r.dispatchPaths()
	if err != nil {
		return err
	}
	s, err := dispatch.LoadState(paths.State)
	if err != nil {
		return err
	}
	running, stop := dispatch.Running(paths.Lock), dispatch.ReadStop(paths.Stop)
	if r.json {
		r.emit(struct {
			Running bool            `json:"running"`
			Stop    string          `json:"stop,omitempty"`
			State   *dispatch.State `json:"state"`
		}{running, stop, s})
		return nil
	}
	printDispatchStatus(r.env.Stdout, s, running, stop)
	return nil
}

// printDispatchStatus prints what the state file holds (dispatcher.md
// §12): whether it runs, what holds it, the budget, the CI failures and
// the runs.
func printDispatchStatus(w io.Writer, s *dispatch.State, running bool, stop string) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format+"\n", a...) }
	switch {
	case running && stop != "":
		p("dispatcher: running (pid %d, since %s), asked to stop (%s)", s.PID, when(s.Since), stop)
	case running:
		p("dispatcher: running (pid %d, since %s)", s.PID, when(s.Since))
	default:
		p("dispatcher: not running")
	}
	if !s.WokeAt.IsZero() {
		p("last wake: %s, %s", esc(s.Wake), when(s.WokeAt))
	}
	if f := (dispatch.Filter{Models: s.Models, Labels: s.Labels}); running && f.Active() {
		p("scope: %s", esc(f.String()))
	}
	if s.Hold != "" {
		p("hold: %s: %s", esc(s.Hold), esc(s.HoldWhy))
	}
	if s.Budget.Day != "" {
		p("budget: %d of %d Opus output tokens on %s (read %s)", s.Budget.Output, s.Budget.Cap, esc(s.Budget.Day), when(s.Budget.At))
	}
	p("CI failures in a row: %d", s.Failures)
	if s.Stopped != "" {
		p("stopped: %s, until `aries dispatch run --clear`", esc(s.Stopped))
	}
	if len(s.Runs) == 0 {
		p("runs: none")
		return
	}
	live := 0
	for _, run := range s.Runs {
		if run.Alive() {
			live++
		}
	}
	p("runs: %d live, %d ended", live, len(s.Runs)-live)
	for _, run := range s.Runs {
		state := "live"
		switch {
		case run.Alive():
		case run.Live():
			// Unended, and no process: it ended while no dispatcher watched.
			state = "ended unknown"
		default:
			state = "ended " + run.Outcome
		}
		line := fmt.Sprintf("  %s  %s  %s  session %s  pid %d  started %s", esc(run.Issue), esc(run.Model), esc(state),
			esc(run.Session), run.PID, when(run.Started))
		if run.PR != "" {
			line += "  " + esc(run.PR)
		}
		if run.CI != "" {
			line += "  ci " + esc(run.CI)
		}
		switch {
		case run.Cleaned:
			line += "  cleaned"
		case run.Left:
			line += "  worktree left: " + esc(run.Worktree)
		default:
			line += "  " + esc(run.Worktree)
		}
		p("%s", line)
	}
}

// newUUID returns a random (version 4) UUID, as Claude Code's
// --session-id takes.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// dispatchBrief prints the brief for an issue, rendered from the
// project's template as the dispatcher renders its runs' (dispatcher.md
// §7, As built: the brief template), for a coordinator to send to a
// session: a Desktop brief by default, with its hand-written section.
// It reads the issue and writes nothing. With --record it prints the
// whole brief: comment, its first line as the memory `briefs` fixes it.
func dispatchBrief(ctx context.Context, r *runner, args []string) error {
	fs := r.newFlags("dispatch brief")
	harness := fs.String("harness", brief.Desktop, "the harness the brief is for: "+brief.Desktop+" (a Desktop session) or "+brief.Headless+" (a headless run)")
	note := fs.String("note", "", "a Desktop brief's hand-written section: what this session needs that the issue and the steps do not say")
	session := fs.String("session", "", "the session the brief is for, as the record names it: the desktop's local_… id, or a headless run's session")
	worktree := fs.String("worktree", "", "a headless brief's worktree")
	record := fs.Bool("record", false, "print the whole brief: comment, its first line from --model, --effort and --session")
	model := fs.String("model", "", "the record's full model id, such as claude-sonnet-5-5")
	effort := fs.String("effort", "default", "the record's effort: low, medium, high, xhigh, max or default")
	pos, err := parse(fs, args, dispatchUsage)
	if err != nil {
		return err
	}
	switch {
	case len(pos) != 1:
		return usagef(dispatchUsage, "dispatch brief takes one issue id")
	case *harness != brief.Desktop && *harness != brief.Headless:
		return usagef(dispatchUsage, "--harness must be %s or %s", brief.Desktop, brief.Headless)
	case *note != "" && *harness != brief.Desktop:
		return usagef(dispatchUsage, "--note is for a Desktop brief; a headless brief has no hand-written section")
	case *record && (*model == "" || *session == ""):
		return usagef(dispatchUsage, "--record needs --model and --session, which its first line names")
	}
	top, err := dispatch.RepoTop(ctx, r.dir)
	if err != nil {
		return proto.Errf(proto.CodeInvalid, "run it in a git checkout of the project", "dispatch brief: "+err.Error())
	}
	tmpl, err := brief.Load(top)
	if err != nil {
		return err
	}
	var show proto.ShowResult
	if err := r.call(ctx, proto.OpShow, proto.ShowArgs{ID: pos[0], Full: true}, &show); err != nil {
		return err
	}
	is := show.Issue
	text := tmpl.Render(brief.Input{Harness: *harness, Issue: is, Items: show.Items, Handoff: show.Handoff,
		Session: *session, Worktree: *worktree, Branch: gitx.Branch(is.Type, is.ID, is.Title), Note: *note})
	out := text
	if *record {
		out = brief.Comment(brief.Record{Harness: *harness, Model: *model, Effort: *effort, Session: *session, Template: tmpl.Name()}, text)
	}
	if r.json {
		r.emit(struct {
			Issue    string `json:"issue"`
			Harness  string `json:"harness"`
			Template string `json:"template"`
			SHA256   string `json:"sha256"`
			Brief    string `json:"brief"`
			Comment  string `json:"comment,omitempty"`
		}{is.ID, *harness, tmpl.Name(), brief.Hash(text), text, map[bool]string{true: out}[*record]})
		return nil
	}
	_, _ = io.WriteString(r.env.Stdout, out)
	return nil
}
