package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/brief"
	"github.com/ariesworx/aries/internal/proto"
)

// Dispatcher is the loop over its parts. Set every exported field but
// those marked optional; one Dispatcher serves one project from one
// goroutine.
type Dispatcher struct {
	Cfg      Config
	Tracker  Tracker
	Launcher Launcher
	Git      Git
	// Forge is optional: without it no PR is read, so CI failures are not
	// counted and no branch is deleted.
	Forge Forge
	// Out takes a line for a person per decision; Log one JSON object per
	// decision, ids, numbers and paths only (dispatcher.md §12).
	Out, Log io.Writer
	Now      func() time.Time
	// NewSession returns a new session id for a run (a UUID, as the
	// harness's --session-id takes).
	NewSession func() string
	// Worktrees is the directory its worktrees go in, one per issue;
	// RunLogs the directory of each run's output.
	Worktrees, RunLogs string
	// Key is the API key given to each run in its environment; Environ
	// the environment runs start from.
	Key     string
	Environ []string
	// State is read and changed in place; Save writes it.
	State *State
	Save  func(*State) error
	// StopRequest returns the stop asked for: StopDrain, StopNow or "".
	StopRequest func() string
	// Template is the project's brief template, read once at start
	// (brief.Load), so that every run of one dispatcher follows one
	// version.
	Template *brief.Template

	procs map[string]Process
	// interrupts are the runs interrupted, by session.
	interrupts map[string]*interrupt
}

// interrupt is why and when a run was interrupted, and whether it has
// been terminated since.
type interrupt struct {
	outcome    string
	at         time.Time
	terminated bool
}

func (d *Dispatcher) init() {
	if d.procs == nil {
		d.procs = map[string]Process{}
		d.interrupts = map[string]*interrupt{}
	}
}

// record is one line of the log.
type record struct {
	At       time.Time `json:"at"`
	Event    string    `json:"event"`
	Source   string    `json:"source,omitempty"`
	Issue    string    `json:"issue,omitempty"`
	Session  string    `json:"session,omitempty"`
	Model    string    `json:"model,omitempty"`
	Action   string    `json:"action,omitempty"`
	Rule     string    `json:"rule,omitempty"`
	PID      int       `json:"pid,omitempty"`
	Code     int       `json:"code,omitempty"`
	Outcome  string    `json:"outcome,omitempty"`
	Path     string    `json:"path,omitempty"`
	Branch   string    `json:"branch,omitempty"`
	PR       string    `json:"pr,omitempty"`
	Checks   string    `json:"checks,omitempty"`
	Failures int       `json:"ci_failures,omitempty"`
	Output   int64     `json:"output,omitempty"`
	Cap      int64     `json:"cap,omitempty"`
	Brief    string    `json:"brief_sha256,omitempty"`
}

// log appends one record to the log, unless it is a dry run: a dry run
// makes nothing happen, so it writes no line (docs/cli.md, dispatcher.md).
func (d *Dispatcher) log(r record) {
	if d.Cfg.DryRun {
		return
	}
	r.At = d.Now().UTC()
	b, err := json.Marshal(r)
	if err == nil {
		_, _ = fmt.Fprintf(d.Log, "%s\n", b)
	}
}

func (d *Dispatcher) say(format string, a ...any) {
	_, _ = fmt.Fprintf(d.Out, format+"\n", a...)
}

func (d *Dispatcher) save() {
	if d.Cfg.DryRun {
		return
	}
	if err := d.Save(d.State); err != nil {
		d.say("could not save the state: %v", err)
	}
}

// Wake is one pass of the loop: it reconciles its runs with Aries and
// git, cleaning up after those whose issue closed, then starts runs while
// a slot is free and nothing holds it. A dry run reads and judges, and
// says what it would do; it claims, makes, launches and writes nothing.
func (d *Dispatcher) Wake(ctx context.Context, source string) error {
	d.init()
	d.say("wake %s", source)
	d.log(record{Event: "wake", Source: source})
	d.State.Wake, d.State.WokeAt = source, d.Now().UTC()
	d.State.Models, d.State.Labels = d.Cfg.Filter.Models, d.Cfg.Filter.Labels
	if d.Cfg.Filter.Active() {
		d.say("%s", d.Cfg.Filter)
	}
	if !d.Cfg.DryRun {
		d.reapReady(ctx)
		d.reconcile(ctx)
	}
	d.fill(ctx)
	d.save()
	return ctx.Err()
}

// fill starts runs while it may (dispatcher.md §2, §4, §6).
func (d *Dispatcher) fill(ctx context.Context) {
	now := d.Now().UTC()
	in := Inputs{Stop: d.stopRequest()}
	// What holds it without reading aries first; then with what it reads.
	rule, whyHeld := Hold(d.Cfg, d.State, Inputs{Stop: in.Stop, Free: 1})
	if rule == "" {
		var err error
		in, err = d.inputs(ctx, in, now)
		if err != nil {
			rule, whyHeld = HoldUnreachable, "aries could not be read: "+errText(err)
		} else {
			rule, whyHeld = Hold(d.Cfg, d.State, in)
		}
	}
	if rule != "" && (rule != HoldSlots || !d.Cfg.DryRun) {
		d.held(rule, whyHeld)
		return
	}
	d.held("", "")
	ready, err := d.Tracker.Ready(ctx, d.Cfg.ReadyLimit)
	if err != nil {
		d.held(HoldUnreachable, "aries could not be read: "+errText(err))
		return
	}
	d.say("judged %d ready issues%s", len(ready), d.cappedNote(len(ready)))
	free := in.Free
	for _, s := range ready {
		if ctx.Err() != nil {
			return
		}
		dec := d.judge(ctx, s.ID, now)
		if dec.Action == ActStart && free <= 0 {
			dec = Decision{Action: ActQueue, Issue: dec.Issue, Model: dec.Model, Rule: RuleSlots, Why: why[RuleSlots]}
		}
		switch {
		case dec.Action != ActStart:
		case d.Cfg.DryRun:
			free--
		default:
			if dec = d.start(ctx, dec); dec.Action == ActStart {
				free--
			}
		}
		d.decided(dec)
	}
}

// cappedNote says that n ready issues is all one wake reads, when it is.
func (d *Dispatcher) cappedNote(n int) string {
	if n > 0 && n >= d.Cfg.ReadyLimit {
		return " (the most a wake reads)"
	}
	return ""
}

// inputs reads the day's Opus output, the pause lock and the free slots.
func (d *Dispatcher) inputs(ctx context.Context, in Inputs, now time.Time) (Inputs, error) {
	opus, err := d.Tracker.OpusOutput(ctx, now)
	if err != nil {
		return in, err
	}
	d.State.Budget = Budget{Day: now.Format(time.DateOnly), Output: opus, Cap: d.Cfg.OpusCap, At: now}
	paused, err := d.Tracker.Paused(ctx)
	if err != nil {
		return in, err
	}
	held, err := d.Tracker.Slots(ctx)
	if err != nil {
		return in, err
	}
	in.OpusOutput, in.Paused, in.Free = opus, paused, d.Cfg.Capacity-held
	return in, nil
}

// held records what holds starting: a lasting hold is logged as a stop
// once, when it begins; any other at each wake.
func (d *Dispatcher) held(rule, whyHeld string) {
	was := d.State.Hold
	d.State.Hold, d.State.HoldWhy = rule, whyHeld
	if rule == "" {
		return
	}
	d.say("hold [%s]: %s", rule, whyHeld)
	ev := "hold"
	if lasting[rule] {
		if was == rule {
			return
		}
		ev = "stop"
	}
	d.log(record{Event: ev, Rule: rule, Output: d.State.Budget.Output, Cap: d.State.Budget.Cap, Failures: d.State.Failures})
}

// judge reads one ready issue and applies the rules to it.
func (d *Dispatcher) judge(ctx context.Context, id string, now time.Time) Decision {
	if d.failedBefore(id) {
		return Decision{Action: ActSkip, Issue: id, Rule: RuleFailedBefore, Why: why[RuleFailedBefore]}
	}
	sr, err := d.Tracker.Show(ctx, id)
	if err != nil {
		return Decision{Action: ActSkip, Issue: id, Rule: RuleUnreadable, Why: why[RuleUnreadable] + ": " + errText(err)}
	}
	var pr *PR
	if h := sr.Handoff; h != nil && h.Branch != "" && d.Forge != nil && (h.State == "partial" || h.State == "blocked") {
		if pr, err = d.Forge.PR(ctx, h.Branch); err != nil {
			return Decision{Action: ActSkip, Issue: id, Rule: RuleUnreadable, Why: why[RuleUnreadable] + ": " + errText(err)}
		}
	}
	return d.Cfg.Filter.Apply(sr, Judge(sr, d.running, pr, now))
}

// decided says and logs a decision.
func (d *Dispatcher) decided(dec Decision) {
	switch dec.Action {
	case ActStart:
		if d.Cfg.DryRun {
			d.say("would start %s (model %s)", dec.Issue, dec.Model)
		}
	case ActQueue:
		d.say("queue %s (model %s) [%s]: %s", dec.Issue, dec.Model, dec.Rule, dec.Why)
	default:
		d.say("skip %s [%s]: %s", dec.Issue, dec.Rule, dec.Why)
	}
	if dec.Action == ActStart && !d.Cfg.DryRun {
		return // start logged it, with the session
	}
	d.log(record{Event: "pick", Action: string(dec.Action), Issue: dec.Issue, Model: dec.Model, Rule: dec.Rule})
}

// running reports whether one of its runs works id.
func (d *Dispatcher) running(id string) bool {
	for _, r := range d.State.Runs {
		if r.Issue == id && r.Live() {
			return true
		}
	}
	return false
}

// failedBefore reports whether a run of its own on id ended in a way a
// person should look at before another starts.
func (d *Dispatcher) failedBefore(id string) bool {
	for _, r := range d.State.Runs {
		if r.Issue == id && !r.Live() && !r.Cleared {
			switch r.Outcome {
			case OutcomeOK, OutcomeRefused, OutcomeLost, OutcomeUnknown:
			default:
				return true
			}
		}
	}
	return false
}

// start claims the issue under a new session, makes its worktree,
// launches the run and posts its brief, in that order (dispatcher.md §4,
// §7). A claim another took first skips the issue; any later failure
// releases the claim with a handoff saying why.
func (d *Dispatcher) start(ctx context.Context, dec Decision) Decision {
	id, session := dec.Issue, d.NewSession()
	skip := func(rule, whyNot string) Decision {
		return Decision{Action: ActSkip, Issue: id, Model: dec.Model, Rule: rule, Why: whyNot}
	}
	if d.Template == nil {
		return skip(RuleLaunch, "no brief template was read; "+brief.File+" is needed")
	}
	st, err := d.Tracker.Start(ctx, id, session, d.Cfg.Lease)
	if err != nil {
		if pe, ok := errors.AsType[*proto.Error](err); ok && pe.Code == proto.CodeConflict {
			return skip(RuleHeld, why[RuleHeld])
		}
		return skip(RuleLaunch, "the claim failed: "+errText(err))
	}
	dir := filepath.Join(d.Worktrees, id)
	found, err := d.Git.AddWorktree(ctx, dir, st.Branch)
	if err != nil {
		d.release(ctx, id, session, "aries dispatch could not make the run's worktree: "+err.Error())
		return skip(RuleLaunch, "the worktree failed: "+err.Error())
	}
	text := Brief(d.Template, st, session, dir, found)
	model := ModelID(dec.Model)
	sp := Spec{Issue: id, Session: session, Model: model, Dir: dir, Branch: st.Branch, Brief: text,
		Env: d.runEnv(session), Log: filepath.Join(d.RunLogs, session+".log"), Capacity: d.Cfg.Capacity, Lease: d.Cfg.Lease}
	p, err := d.Launcher.Launch(ctx, sp)
	if err != nil {
		_ = d.Git.RemoveWorktree(ctx, dir) // made for this run alone, and empty
		d.release(ctx, id, session, "aries dispatch could not launch the run: "+err.Error())
		return skip(RuleLaunch, "the launch failed: "+err.Error())
	}
	run := &Run{Issue: id, Session: session, Harness: HarnessClaudeCode, Model: model, Worktree: dir,
		Branch: st.Branch, PID: p.PID(), Started: d.Now().UTC()}
	d.State.Runs = append(d.State.Runs, run)
	d.procs[session] = p
	d.save()
	rec := brief.Record{Harness: HarnessClaudeCode, Model: model, Effort: EffortDefault, Session: session, Template: d.Template.Name()}
	if err := d.Tracker.Comment(ctx, id, session, brief.Comment(rec, text)); err != nil {
		d.say("could not post the brief on %s: %s", id, errText(err))
	}
	d.say("start %s (model %s, session %s, pid %d)", id, model, session, run.PID)
	d.log(record{Event: "start", Issue: id, Session: session, Model: model, PID: run.PID, Path: dir, Branch: st.Branch, Brief: brief.Hash(text)})
	return dec
}

// runEnv is a run's environment: the dispatcher's, without a session or
// a key of its own or the marks of a Claude Code session around it, with
// the run's session and the key, and the Go variables the gate sets
// (AGENTS.md, Commands), so that no command needs them written in front:
// an allow rule does not match a command that starts with an assignment.
func (d *Dispatcher) runEnv(session string) []string {
	var env []string
	for _, kv := range d.Environ {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case k == "ARIES_SESSION", k == "ANTHROPIC_API_KEY", k == proto.LockEpochEnv, k == "CLAUDECODE",
			strings.HasPrefix(k, "CLAUDE_CODE_"):
			continue
		}
		env = append(env, kv)
	}
	env = withEnv(env, "GOTOOLCHAIN", "local")
	env = withEnv(env, "GOFLAGS", "-mod=readonly")
	env = append(env, "ARIES_SESSION="+session)
	if d.Key != "" {
		env = append(env, "ANTHROPIC_API_KEY="+d.Key)
	}
	return env
}

// release lets id's claim go, from session, with note.
func (d *Dispatcher) release(ctx context.Context, id, session, note string) {
	if err := d.Tracker.Handoff(ctx, id, session, note, true); err != nil {
		d.say("could not release %s: %s; its claim lapses with its lease", id, errText(err))
	}
	d.log(record{Event: "release", Issue: id, Session: session})
}

// reapReady handles the runs that have ended since it last looked, and
// returns how many.
func (d *Dispatcher) reapReady(ctx context.Context) int {
	d.init()
	n := 0
	for _, r := range d.State.Runs {
		p := d.procs[r.Session]
		if p == nil || !r.Live() {
			continue
		}
		select {
		case <-p.Done():
			d.ended(ctx, r, p.Exit())
			n++
		default:
		}
	}
	if n > 0 {
		d.save()
	}
	return n
}

// ended records how a run ended. An auth failure stops the dispatcher; a
// run refused before it started has its worktree removed; a run that did
// not end well and still holds its claim has it released.
func (d *Dispatcher) ended(ctx context.Context, r *Run, e Exit) {
	outcome := e.Outcome
	if in := d.interrupts[r.Session]; in != nil {
		outcome = in.outcome
	}
	delete(d.procs, r.Session)
	delete(d.interrupts, r.Session)
	r.Ended, r.Outcome, r.Code = d.Now().UTC(), outcome, e.Code
	d.say("exit %s (session %s): %s, code %d", r.Issue, r.Session, outcome, e.Code)
	d.log(record{Event: "exit", Issue: r.Issue, Session: r.Session, PID: r.PID, Outcome: outcome, Code: e.Code})
	RecordExit(d.State, outcome)
	switch outcome {
	case OutcomeOK:
		return
	case OutcomeRefused:
		// The harness never started: the claim and the worktree are the
		// dispatcher's alone.
		if err := d.Git.RemoveWorktree(ctx, r.Worktree); err == nil {
			r.Cleaned = true
		}
		d.release(ctx, r.Issue, r.Session, "The lock "+RunsLock+" refused the run before it started; aries dispatch released the claim and tries again later.")
		return
	}
	sr, err := d.Tracker.Show(ctx, r.Issue)
	if err == nil && sr.Claim != nil && sr.Claim.Session == r.Session {
		d.release(ctx, r.Issue, r.Session, fmt.Sprintf("The run ended (%s, exit code %d) without letting its claim go; aries dispatch released it.", outcome, e.Code))
	}
}

// reconcile reads each ended run's PR, counting its CI result once per PR
// and commit, and cleans up after the runs whose issue closed (dispatcher.md
// §6, §9).
func (d *Dispatcher) reconcile(ctx context.Context) {
	for _, r := range d.State.Runs {
		if r.Live() || r.Cleaned {
			continue
		}
		var pr *PR
		if r.CI == "" {
			pr = d.pr(ctx, r)
		}
		if pr != nil {
			r.PR = pr.URL
			if pr.Checks == ChecksPass || pr.Checks == ChecksFail {
				r.CI, r.Head = pr.Checks, pr.Head
				if !d.State.Counted(r) {
					tripped := RecordCI(d.Cfg, d.State, pr.Checks == ChecksPass)
					if tripped {
						d.held(Hold(d.Cfg, d.State, Inputs{Free: 1}))
					}
				}
				d.log(record{Event: "ci", Issue: r.Issue, Session: r.Session, PR: r.PR, Checks: r.CI, Failures: d.State.Failures})
			}
		}
		d.cleanup(ctx, r, pr)
	}
	d.prune()
}

// pr reads the run's PR, or nil when it has none or it could not be read:
// it is read again at the next wake.
func (d *Dispatcher) pr(ctx context.Context, r *Run) *PR {
	if d.Forge == nil || r.Branch == "" {
		return nil
	}
	pr, err := d.Forge.PR(ctx, r.Branch)
	if err != nil {
		return nil
	}
	return pr
}

// cleanup removes the worktree of an ended run of its own whose issue is
// closed, without --force, and its branch only when its PR merged
// (dispatcher.md §9).
func (d *Dispatcher) cleanup(ctx context.Context, r *Run, pr *PR) {
	if !d.owns(r.Worktree) {
		return
	}
	sr, err := d.Tracker.Show(ctx, r.Issue)
	if err != nil || sr.Issue.Status != "closed" {
		return
	}
	if err := d.Git.RemoveWorktree(ctx, r.Worktree); err != nil {
		if !r.Left {
			d.say("left %s's worktree %s: %v", r.Issue, r.Worktree, err)
			d.log(record{Event: "left", Issue: r.Issue, Path: r.Worktree, Branch: r.Branch})
		}
		r.Left = true
		return
	}
	r.Cleaned, r.Left = true, false
	deleted := ""
	if pr == nil {
		pr = d.pr(ctx, r)
	}
	if pr != nil && pr.State == "MERGED" {
		if err := d.Git.DeleteBranch(ctx, r.Branch); err == nil {
			deleted = r.Branch
		}
	}
	d.say("removed %s's worktree %s", r.Issue, r.Worktree)
	d.log(record{Event: "removed", Issue: r.Issue, Session: r.Session, Path: r.Worktree, Branch: deleted})
}

// owns reports whether path is a worktree under its own directory.
func (d *Dispatcher) owns(path string) bool {
	rel, err := filepath.Rel(d.Worktrees, path)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

// enforce interrupts a run past its limit, and every run on a stop
// --now, and terminates one that has not ended within the grace.
func (d *Dispatcher) enforce(context.Context) {
	d.init()
	now, stop := d.Now(), d.stopRequest()
	for _, r := range d.State.Runs {
		p := d.procs[r.Session]
		if p == nil || !r.Live() {
			continue
		}
		in := d.interrupts[r.Session]
		switch {
		case in == nil && stop == StopNow:
			d.interrupt(r, p, OutcomeStopped, now)
		case in == nil && now.Sub(r.Started) >= d.Cfg.RunLimit:
			d.interrupt(r, p, OutcomeTimeout, now)
		case in != nil && !in.terminated && now.Sub(in.at) >= d.Cfg.StopGrace:
			in.terminated = true
			_ = p.Terminate()
			d.log(record{Event: "terminate", Issue: r.Issue, Session: r.Session, PID: r.PID})
		}
	}
}

func (d *Dispatcher) interrupt(r *Run, p Process, outcome string, now time.Time) {
	d.interrupts[r.Session] = &interrupt{outcome: outcome, at: now}
	_ = p.Interrupt()
	d.say("interrupt %s (session %s): %s", r.Issue, r.Session, outcome)
	d.log(record{Event: "interrupt", Issue: r.Issue, Session: r.Session, PID: r.PID, Outcome: outcome})
}

// Adopt takes back the runs a dispatcher before it started whose process
// still runs in their session, and marks the rest ended: their claims
// lapse by themselves (dispatcher.md §9).
func (d *Dispatcher) Adopt(ctx context.Context) {
	d.init()
	for _, r := range d.State.Runs {
		if !r.Live() {
			continue
		}
		if p, ok := d.Launcher.Adopt(r.PID, r.Session); ok {
			d.procs[r.Session] = p
			d.say("adopted %s (session %s, pid %d)", r.Issue, r.Session, r.PID)
			d.log(record{Event: "adopt", Issue: r.Issue, Session: r.Session, PID: r.PID})
			continue
		}
		r.Ended, r.Outcome = d.Now().UTC(), OutcomeLost
		d.log(record{Event: "exit", Issue: r.Issue, Session: r.Session, PID: r.PID, Outcome: OutcomeLost})
	}
	d.prune()
	d.save()
	_ = ctx
}

// keepRuns is how long a cleaned run stays in the state, to be seen in
// status and to keep its issue from starting again soon.
const keepRuns = 7 * 24 * time.Hour

// prune drops cleaned runs that ended more than keepRuns ago.
func (d *Dispatcher) prune() {
	cutoff := d.Now().Add(-keepRuns)
	kept := d.State.Runs[:0]
	for _, r := range d.State.Runs {
		if r.Cleaned && !r.Live() && r.Ended.Before(cutoff) {
			continue
		}
		kept = append(kept, r)
	}
	d.State.Runs = kept
}

// Live is how many of its runs are live.
func (d *Dispatcher) Live() int {
	n := 0
	for _, r := range d.State.Runs {
		if r.Live() {
			n++
		}
	}
	return n
}

// Done reports whether a stop was asked and no run is live: the loop
// ends.
func (d *Dispatcher) Done() bool {
	return d.stopRequest() != "" && d.Live() == 0
}

func (d *Dispatcher) stopRequest() string {
	if d.StopRequest == nil {
		return ""
	}
	return d.StopRequest()
}

// Loop runs until ctx ends or a stop asked for has been carried out. It
// adopts runs a dispatcher before it left, wakes once, then on each
// inbox item or issue event (at most every Debounce), each run's exit,
// each move of the default branch, a freed pause or slot found by
// the lock poll while it holds for one, and the fallback timer. Ended by ctx,
// it leaves its runs running: they renew their own claims and slots, and
// the next dispatcher adopts them.
func (d *Dispatcher) Loop(ctx context.Context) error {
	d.init()
	pushed := make(chan string, 1)
	go d.watch(ctx, func(source string) {
		select {
		case pushed <- source:
		default:
		}
	})
	if !d.Cfg.DryRun {
		d.Adopt(ctx)
	}
	head, _ := d.Git.RemoteHead(ctx)
	if err := d.Wake(ctx, WakeStart); err != nil {
		return d.left(err)
	}
	fallback := time.NewTimer(d.Cfg.Fallback)
	defer fallback.Stop()
	branch := time.NewTicker(d.Cfg.BranchPoll)
	defer branch.Stop()
	tick := time.NewTicker(d.Cfg.Tick)
	defer tick.Stop()
	lockPoll := time.NewTicker(d.Cfg.LockPoll)
	defer lockPoll.Stop()
	last, pending := d.Now(), ""
	wake := func(source string) error {
		pending, last = "", d.Now()
		fallback.Reset(d.Cfg.Fallback)
		return d.Wake(ctx, source)
	}
	for {
		var err error
		select {
		case <-ctx.Done():
			return d.left(nil)
		case source := <-pushed:
			if d.Now().Sub(last) < d.Cfg.Debounce {
				pending = source
				continue
			}
			err = wake(source)
		case <-fallback.C:
			err = wake(WakeTimer)
		case <-branch.C:
			h, herr := d.Git.RemoteHead(ctx)
			if herr != nil || h == head {
				continue
			}
			head = h
			d.log(record{Event: "branch"})
			err = wake(WakeBranch)
		case <-lockPoll.C:
			if d.lockFreed(ctx) {
				err = wake(WakeLock)
			}
		case <-tick.C:
			d.enforce(ctx)
			switch {
			case d.reapReady(ctx) > 0:
				err = wake(WakeExit)
			case pending != "" && d.Now().Sub(last) >= d.Cfg.Debounce:
				err = wake(pending)
			}
			if d.Done() {
				d.say("stopped: no run is live")
				d.log(record{Event: "done"})
				return nil
			}
		}
		if err != nil {
			return d.left(err)
		}
	}
}

// lockFreed reports whether a loop holding for the pause or for a slot may
// start a run now. Neither a release of dispatch/pause nor a slot freed
// by another machine's run, or by a lease that lapsed, reaches the watch
// (lock events are not pushed to it, and a lapse is no event until a
// write finds it), so the loop reads the two locks each LockPoll while it
// holds for them, and at no other time.
func (d *Dispatcher) lockFreed(ctx context.Context) bool {
	if d.State.Hold != HoldPause && d.State.Hold != HoldSlots {
		return false
	}
	paused, err := d.Tracker.Paused(ctx)
	if err != nil || paused {
		return false
	}
	held, err := d.Tracker.Slots(ctx)
	return err == nil && held < d.Cfg.Capacity
}

// left says what a loop that ends by ctx leaves running.
func (d *Dispatcher) left(err error) error {
	if errors.Is(err, context.Canceled) || err == nil {
		n := 0
		for _, r := range d.State.Runs {
			if r.Alive() {
				n++
			}
		}
		if n > 0 {
			d.say("interrupted: %d runs keep running; the next aries dispatch run adopts them", n)
		}
		d.log(record{Event: "exit-loop"})
		return nil
	}
	return err
}

// watch keeps a watch open, opening it again a while after it ends.
func (d *Dispatcher) watch(ctx context.Context, wake func(string)) {
	for ctx.Err() == nil {
		if err := d.Tracker.Watch(ctx, wake); err != nil && ctx.Err() == nil {
			d.log(record{Event: "watch-ended"})
		}
		wake(WakeResync)
		select {
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
		}
	}
}

// errText is an error's message, the server's own for a *proto.Error.
func errText(err error) string {
	if pe, ok := errors.AsType[*proto.Error](err); ok {
		return pe.Message
	}
	return err.Error()
}
