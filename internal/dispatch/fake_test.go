package dispatch

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/brief"
	"github.com/ariesworx/aries/internal/proto"
)

// calls is the ordered record of what the stand-ins were asked, shared
// between them, so a test can say what happened before what.
type calls struct {
	mu   sync.Mutex
	list []string
}

func (c *calls) add(format string, a ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.list = append(c.list, fmt.Sprintf(format, a...))
}

func (c *calls) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.list)
}

// index is the position of the first call that starts with prefix, or -1.
func (c *calls) index(prefix string) int {
	for i, s := range c.all() {
		if strings.HasPrefix(s, prefix) {
			return i
		}
	}
	return -1
}

func (c *calls) count(prefix string) int {
	n := 0
	for _, s := range c.all() {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}

// slots stands in for the server's lock dispatch/runs, shared by the
// launchers of every dispatcher in a test, as the server's lock is: a
// launch past its capacity is refused, as `aries lock run` without
// --wait refuses a full lock.
type slots struct {
	mu        sync.Mutex
	held, max int
	capacity  int
}

func (s *slots) take() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held >= s.capacity {
		return false
	}
	s.held++
	s.max = max(s.max, s.held)
	return true
}

func (s *slots) give() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held--
}

func (s *slots) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held
}

// fakeTracker stands in for aries: one project's issues, claims, comments
// and handoffs, its Opus output and its locks.
type fakeTracker struct {
	mu       sync.Mutex
	calls    *calls
	slots    *slots
	order    []string
	issues   map[string]proto.ShowResult
	claims   map[string]string // issue → session
	comments map[string][]string
	opus     int64
	paused   bool
	// lockReads counts the reads of the two locks.
	lockReads atomic.Int64
	// stale makes Show leave out claims, as a show read before another
	// dispatcher's start would.
	stale bool
	// down makes every read fail, as when the server cannot be reached.
	down bool
}

func newTracker(c *calls, s *slots) *fakeTracker {
	return &fakeTracker{calls: c, slots: s, issues: map[string]proto.ShowResult{},
		claims: map[string]string{}, comments: map[string][]string{}}
}

// add puts an issue at the end of ready.
func (f *fakeTracker) add(id, typ string, labels ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, id)
	f.issues[id] = proto.ShowResult{Issue: proto.Issue{ID: id, Title: "Title of " + id, Body: "Body of " + id,
		Type: typ, Status: "open", Priority: 2, Labels: labels},
		Items: []proto.AcceptanceItem{{N: 1, Text: "Tests pass"}, {N: 2, Text: "Docs", State: "ticked"}}}
}

func (f *fakeTracker) set(id string, fn func(*proto.ShowResult)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sr := f.issues[id]
	fn(&sr)
	f.issues[id] = sr
}

func (f *fakeTracker) claim(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claims[id]
}

var errDown = proto.Errf(proto.CodeUnavailable, "retry", "the server cannot be reached")

// Ready answers up to n issues (the server's cap, 500, when n is 0 or
// more), as the server's ready does.
func (f *fakeTracker) Ready(_ context.Context, n int) ([]proto.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, errDown
	}
	var out []proto.Summary
	for _, id := range f.order {
		sr := f.issues[id]
		if sr.Issue.Status == "open" && f.claims[id] == "" {
			out = append(out, proto.Summary{ID: id, Title: sr.Issue.Title, Status: "open", Priority: sr.Issue.Priority})
		}
	}
	if n <= 0 || n > 500 {
		n = 500
	}
	return out[:min(n, len(out))], nil
}

func (f *fakeTracker) Show(_ context.Context, id string) (proto.ShowResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return proto.ShowResult{}, errDown
	}
	sr, ok := f.issues[id]
	if !ok {
		return sr, proto.Errf(proto.CodeNotFound, "check the id", id+" not found")
	}
	switch s := f.claims[id]; {
	case s != "" && f.stale:
		sr.Issue.Status = "open" // as it was before the claim
	case s != "":
		sr.Claim = &proto.Claim{Issue: id, By: "alice", Session: s, ExpiresAt: time.Now().Add(time.Hour)}
	}
	return sr, nil
}

func (f *fakeTracker) Start(_ context.Context, id, session string, lease time.Duration) (Started, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.add("start %s %s %s", id, session, lease)
	if f.down {
		return Started{}, errDown
	}
	if s := f.claims[id]; s != "" && s != session {
		return Started{}, proto.Errf(proto.CodeConflict, "leave it", id+" is held by your session "+s)
	}
	f.claims[id] = session
	sr := f.issues[id]
	sr.Issue.Status = "in_progress"
	f.issues[id] = sr
	return Started{Issue: sr.Issue, Items: sr.Items, Handoff: sr.Handoff, Branch: "feature/" + id + "-title"}, nil
}

func (f *fakeTracker) Handoff(_ context.Context, id, session, note string, release bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.add("handoff %s %s release=%v %s", id, session, release, note)
	if release && f.claims[id] == session {
		delete(f.claims, id)
		sr := f.issues[id]
		if sr.Issue.Status == "in_progress" {
			sr.Issue.Status = "open"
		}
		f.issues[id] = sr
	}
	return nil
}

func (f *fakeTracker) Comment(_ context.Context, id, session, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.add("comment %s %s", id, session)
	f.comments[id] = append(f.comments[id], body)
	return nil
}

func (f *fakeTracker) OpusOutput(context.Context, time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return 0, errDown
	}
	return f.opus, nil
}

func (f *fakeTracker) Paused(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lockReads.Add(1)
	return f.paused, nil
}

func (f *fakeTracker) setPaused(p bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = p
}

func (f *fakeTracker) Slots(context.Context) (int, error) {
	f.lockReads.Add(1)
	return f.slots.count(), nil
}

func (f *fakeTracker) Watch(ctx context.Context, _ func(string)) error {
	<-ctx.Done()
	return nil
}

// fakeLauncher stands in for the headless runner under the lock: each
// launch takes a slot, or is refused when none is free, and its process
// ends when the test says.
type fakeLauncher struct {
	mu    sync.Mutex
	calls *calls
	slots *slots
	procs map[string]*fakeProc
	specs map[string]Spec
	fail  error
	// alive are the pids Adopt finds still running, by session.
	alive map[string]int
}

func newLauncher(c *calls, s *slots) *fakeLauncher {
	return &fakeLauncher{calls: c, slots: s, procs: map[string]*fakeProc{}, specs: map[string]Spec{}, alive: map[string]int{}}
}

func (l *fakeLauncher) Launch(_ context.Context, sp Spec) (Process, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls.add("launch %s %s", sp.Issue, sp.Session)
	if l.fail != nil {
		return nil, l.fail
	}
	p := newProc(1000+len(l.procs), sp.Session, l.calls)
	if l.slots.take() {
		p.slots = l.slots
	} else {
		p.end(Exit{Code: 1, Outcome: OutcomeRefused})
	}
	l.procs[sp.Session] = p
	l.specs[sp.Session] = sp
	return p, nil
}

func (l *fakeLauncher) Adopt(pid int, session string) (Process, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.alive[session] != pid {
		return nil, false
	}
	p := newProc(pid, session, l.calls)
	l.procs[session] = p
	return p, true
}

func (l *fakeLauncher) proc(session string) *fakeProc {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.procs[session]
}

func (l *fakeLauncher) spec(session string) Spec {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.specs[session]
}

// sessions are the sessions launched, in no order.
func (l *fakeLauncher) sessions() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for s := range l.specs {
		out = append(out, s)
	}
	return out
}

type fakeProc struct {
	pid     int
	session string
	calls   *calls
	done    chan struct{}
	once    sync.Once
	exit    Exit
	// slots is set when the run took a slot, which it gives back when it
	// ends.
	slots *slots
	// onInterrupt, if set, is how the run ends when interrupted.
	onInterrupt *Exit
}

func newProc(pid int, session string, c *calls) *fakeProc {
	return &fakeProc{pid: pid, session: session, calls: c, done: make(chan struct{})}
}

func (p *fakeProc) PID() int              { return p.pid }
func (p *fakeProc) Done() <-chan struct{} { return p.done }
func (p *fakeProc) Exit() Exit            { return p.exit }

func (p *fakeProc) live() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// end ends the run as e says, once.
func (p *fakeProc) end(e Exit) {
	p.once.Do(func() {
		p.exit = e
		if p.slots != nil {
			p.slots.give()
		}
		close(p.done)
	})
}

func (p *fakeProc) Interrupt() error {
	p.calls.add("interrupt %s", p.session)
	if p.onInterrupt != nil {
		p.end(*p.onInterrupt)
	}
	return nil
}

func (p *fakeProc) Terminate() error {
	p.calls.add("terminate %s", p.session)
	p.end(Exit{Code: 143, Outcome: OutcomeFailed})
	return nil
}

// fakeGit stands in for git: worktrees and branches, by name.
type fakeGit struct {
	mu        sync.Mutex
	calls     *calls
	head      string
	dirty     map[string]bool
	failAdd   error
	worktrees map[string]string // path → branch
}

func newGit(c *calls) *fakeGit {
	return &fakeGit{calls: c, head: "c0ffee", dirty: map[string]bool{}, worktrees: map[string]string{}}
}

func (g *fakeGit) AddWorktree(_ context.Context, path, branch string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls.add("worktree add %s %s", path, branch)
	if g.failAdd != nil {
		return g.failAdd
	}
	g.worktrees[path] = branch
	return nil
}

func (g *fakeGit) RemoveWorktree(_ context.Context, path string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls.add("worktree remove %s", path)
	if g.dirty[path] {
		return fmt.Errorf("'%s' contains modified or untracked files, use --force to delete it", path)
	}
	delete(g.worktrees, path)
	return nil
}

func (g *fakeGit) DeleteBranch(_ context.Context, branch string) error {
	g.calls.add("branch delete %s", branch)
	return nil
}

func (g *fakeGit) RemoteHead(context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.head, nil
}

// fakeForge stands in for gh: each branch's PR.
type fakeForge struct {
	mu  sync.Mutex
	prs map[string]*PR
}

func (f *fakeForge) PR(_ context.Context, branch string) (*PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if pr := f.prs[branch]; pr != nil {
		c := *pr
		return &c, nil
	}
	return nil, nil
}

func (f *fakeForge) set(branch string, pr PR) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.prs == nil {
		f.prs = map[string]*PR{}
	}
	f.prs[branch] = &pr
}

// world is one test's stand-ins and a dispatcher over them.
type world struct {
	t       *testing.T
	calls   *calls
	slots   *slots
	tracker *fakeTracker
	git     *fakeGit
	forge   *fakeForge
	out     *syncBuffer
	log     *syncBuffer
	now     time.Time
	stop    string
	saved   atomic.Int64
}

func newWorld(t *testing.T) *world {
	t.Helper()
	c := &calls{}
	s := &slots{capacity: 4}
	return &world{t: t, calls: c, slots: s, tracker: newTracker(c, s), git: newGit(c), forge: &fakeForge{},
		out: &syncBuffer{}, log: &syncBuffer{}, now: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
}

// dispatcher returns a dispatcher over w's stand-ins, with a launcher of
// its own sharing w's slots, as a second dispatcher on another machine
// has.
func (w *world) dispatcher(cfg Config) (*Dispatcher, *fakeLauncher) {
	l := newLauncher(w.calls, w.slots)
	n := 0
	d := &Dispatcher{
		Cfg: cfg, Tracker: w.tracker, Launcher: l, Git: w.git, Forge: w.forge,
		Out: w.out, Log: w.log, Now: func() time.Time { return w.now },
		Worktrees: "/repo/.aries-dispatch/worktrees", RunLogs: "/data/runs", Key: "sk-test-not-a-key",
		Environ:     []string{"PATH=/usr/bin", "ARIES_SESSION=outer", "ANTHROPIC_API_KEY=outer-key", "CLAUDE_CODE_SESSION_ID=outer", "CLAUDECODE=1", "GOFLAGS=-mod=mod"},
		State:       &State{},
		Save:        func(*State) error { w.saved.Add(1); return nil },
		StopRequest: func() string { return w.stop },
		Template:    repoTemplate(w.t),
	}
	d.NewSession = func() string {
		n++
		return fmt.Sprintf("%p-%d", d, n)
	}
	return d, l
}

// syncBuffer is a bytes.Buffer safe for the goroutines of a dispatcher.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// repoTemplate is the repository's own brief template, so that the
// dispatcher's tests run the text its runs get.
func repoTemplate(t *testing.T) *brief.Template {
	t.Helper()
	tp, err := brief.Load(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return tp
}
