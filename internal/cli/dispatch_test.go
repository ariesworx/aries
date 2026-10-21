package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/credential"
	"github.com/ariesworx/aries/internal/dispatch"
)

// memStore is a credential store in memory.
type memStore struct {
	mu      sync.Mutex
	secrets map[string]string
}

func (m *memStore) Kind() credential.Kind { return credential.KindFile }
func (m *memStore) Name() string          { return "test store" }
func (m *memStore) Get(_ context.Context, name string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.secrets[name]
	return s, ok, nil
}
func (m *memStore) Set(_ context.Context, name, secret string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.secrets[name] = secret
	return nil
}
func (m *memStore) Remove(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.secrets, name)
	return nil
}

// dispatchEnv is an Env for the dispatch commands: a data directory of
// the test's own, the store given, and stdin.
type dispatchEnv struct {
	data, dir string
	store     *memStore
	stdin     string
	terminal  bool
	goos      string
	aries     string
	environ   []string
}

func newDispatchEnv(t *testing.T) *dispatchEnv {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".aries.yaml"), "project: 0f6e5c1a-1111-4222-8333-444455556666\nserver:\n  url: https://aries.example.com\n")
	return &dispatchEnv{data: t.TempDir(), dir: dir, store: &memStore{secrets: map[string]string{}}}
}

func (e *dispatchEnv) run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	env := Env{Stdout: &out, Stderr: &errb, Stdin: strings.NewReader(e.stdin), Version: "v0.6.0",
		Getenv: func(k string) string {
			if k == "ARIES_DATA_DIR" {
				return e.data
			}
			return ""
		},
		Terminal:    func() bool { return e.terminal },
		GOOS:        e.goos,
		Credentials: func(context.Context) (credential.Store, error) { return e.store, nil },
	}
	if e.aries != "" {
		env.Executable = func() (string, error) { return e.aries, nil }
	}
	if e.environ != nil {
		env.Environ = func() []string { return e.environ }
	}
	code := Run(t.Context(), append([]string{"-C", e.dir}, args...), env)
	return code, out.String(), errb.String()
}

// The loop refuses on Windows with a fix, while the command still builds
// there (rule 14).
func TestDispatchRefusesOnWindows(t *testing.T) {
	t.Parallel()
	e := newDispatchEnv(t)
	e.goos = "windows"
	code, _, errb := e.run(t, "dispatch", "run", "--dry-run", "--once")
	if code != ExitFailure || !strings.Contains(errb, "only on macOS and Linux") || !strings.Contains(errb, "fix: run aries dispatch on macOS or Linux") {
		t.Errorf("exit %d, stderr %q", code, errb)
	}
}

// Mistakes on the command line are usage errors.
func TestDispatchUsage(t *testing.T) {
	t.Parallel()
	e := newDispatchEnv(t)
	for _, args := range [][]string{
		{"dispatch"}, {"dispatch", "go"}, {"dispatch", "run", "--json"}, {"dispatch", "run", "--capacity", "0"}, {"dispatch", "run", "--models", "gpt"}, {"dispatch", "run", "--labels", "a,,b"},
		{"dispatch", "run", "extra"}, {"dispatch", "key"}, {"dispatch", "key", "show"},
		{"dispatch", "brief"}, {"dispatch", "brief", "ar-1", "ar-2"}, {"dispatch", "brief", "ar-1", "--harness", "codex"},
		{"dispatch", "brief", "ar-1", "--harness", "claude-code", "--note", "x"}, {"dispatch", "brief", "ar-1", "--record", "--model", "m"},
	} {
		// With --json the refusal is the one document on stdout.
		if code, out, errb := e.run(t, args...); code != ExitUsage || !strings.Contains(out+errb, "usage: aries dispatch") {
			t.Errorf("aries %s: exit %d, %q %q", strings.Join(args, " "), code, out, errb)
		}
	}
}

// The help says it is experimental (dispatcher.md §12).
func TestDispatchHelpSaysExperimental(t *testing.T) {
	t.Parallel()
	if out := helpText(t, "dispatch"); !strings.Contains(out, "experimental") {
		t.Errorf("aries help dispatch:\n%s", out)
	}
	if out := helpText(t); !strings.Contains(out, "dispatch experimental") {
		t.Errorf("aries help:\n%s", out)
	}
}

// key set reads the key from stdin alone, keeps it in the store and
// never prints it; a terminal at stdin is refused, so a key typed would
// never show; remove removes it.
func TestDispatchKey(t *testing.T) {
	t.Parallel()
	e := newDispatchEnv(t)
	e.terminal = true
	if code, _, errb := e.run(t, "dispatch", "key", "set"); code != ExitFailure || !strings.Contains(errb, "never shows on the screen") {
		t.Errorf("from a terminal: exit %d, %q", code, errb)
	}
	e.terminal = false
	e.stdin = "two words\n"
	if code, _, _ := e.run(t, "dispatch", "key", "set"); code != ExitFailure {
		t.Error("two words were kept as a key")
	}
	e.stdin = "sk-test-not-a-key\n"
	code, out, errb := e.run(t, "dispatch", "key", "set")
	if code != ExitOK || !strings.Contains(out, "kept the API key in test store") {
		t.Fatalf("exit %d, %q %q", code, out, errb)
	}
	if strings.Contains(out+errb, "sk-test") {
		t.Error("the key was printed")
	}
	if got := e.store.secrets[dispatch.KeyName]; got != "sk-test-not-a-key" {
		t.Errorf("kept %q", got)
	}
	if code, out, _ := e.run(t, "dispatch", "key", "remove"); code != ExitOK || !strings.Contains(out, "removed") || len(e.store.secrets) != 0 {
		t.Errorf("remove: exit %d, %q, %v", code, out, e.store.secrets)
	}
}

// status reads the state file alone; stop with no dispatcher running
// says so and asks nothing of one.
func TestDispatchStatusAndStop(t *testing.T) {
	t.Parallel()
	e := newDispatchEnv(t)
	code, out, _ := e.run(t, "dispatch", "status")
	if code != ExitOK || !strings.Contains(out, "dispatcher: not running") || !strings.Contains(out, "runs: none") {
		t.Errorf("no state: exit %d\n%s", code, out)
	}
	paths := dispatch.PathsFor(e.data, "0f6e5c1a-1111-4222-8333-444455556666")
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	// PID 7 names no process of this session: the run is unended in the
	// state, but its process is gone, so status says it ended unknown.
	s := &dispatch.State{Failures: 1, Hold: dispatch.HoldBudget, HoldWhy: "past the cap",
		Budget: dispatch.Budget{Day: "2026-10-20", Output: 1400000, Cap: 1300000, At: at},
		Runs: []*dispatch.Run{
			{Issue: "ar-1", Session: "s-1", Model: "opus", PID: 7, Started: at, Worktree: "/w/ar-1"},
			{Issue: "ar-2", Session: "s-2", Model: "sonnet", PID: 8, Started: at, Ended: at, Outcome: dispatch.OutcomeOK,
				PR: "https://example.com/pr/9", CI: dispatch.ChecksPass, Cleaned: true},
		}}
	if err := s.Write(paths.State); err != nil {
		t.Fatal(err)
	}
	_, out, _ = e.run(t, "dispatch", "status")
	for _, want := range []string{"hold: budget: past the cap", "budget: 1400000 of 1300000 Opus output tokens on 2026-10-20",
		"CI failures in a row: 1", "runs: 0 live, 2 ended", "ar-1  opus  ended unknown  session s-1  pid 7", "ar-2  sonnet  ended ok",
		"https://example.com/pr/9  ci pass  cleaned"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	// The filters show only while a dispatcher runs.
	if strings.Contains(out, "scope:") {
		t.Errorf("status shows a scope with no filters:\n%s", out)
	}
	var sb strings.Builder
	f := dispatch.Filter{Models: []string{"haiku"}, Labels: []string{"trial"}}
	printDispatchStatus(&sb, &dispatch.State{PID: 9, Models: f.Models, Labels: f.Labels}, true, "")
	if !strings.Contains(sb.String(), "scope: only models haiku; only labels trial") {
		t.Errorf("a running dispatcher's status lacks its scope:\n%s", sb.String())
	}
	sb.Reset()
	printDispatchStatus(&sb, &dispatch.State{Models: f.Models}, false, "")
	if strings.Contains(sb.String(), "scope:") {
		t.Errorf("a stopped dispatcher's status shows a scope:\n%s", sb.String())
	}
	_, out, _ = e.run(t, "--json", "dispatch", "status")
	var doc struct {
		Running bool            `json:"running"`
		State   *dispatch.State `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || doc.Running || len(doc.State.Runs) != 2 {
		t.Errorf("--json: %v %+v\n%s", err, doc, out)
	}
	code, out, _ = e.run(t, "dispatch", "stop", "--now")
	if code != ExitOK || !strings.Contains(out, "no dispatcher runs") || !strings.Contains(out, dispatch.PauseLock) {
		t.Errorf("stop: exit %d, %q", code, out)
	}
	if _, err := os.Stat(paths.Stop); err == nil {
		t.Error("stop left a request for a dispatcher that does not run")
	}
}

// standinAries is a shell script that answers as aries would for a
// dry run's reads, recording each command line, and refuses anything
// else.
const standinAries = `#!/bin/sh
while [ $# -gt 0 ]; do
  case "$1" in
    -C) shift 2 ;;
    --json|--no-queue) shift ;;
    *) break ;;
  esac
done
echo "$*" >> "$RECORD"
case "$1 $2" in
  "ready "*) echo '{"issues":[{"id":"ar-1","title":"a","status":"open","priority":1},{"id":"ar-2","title":"b","status":"open","priority":2}]}' ;;
  "show ar-1") echo '{"issue":{"id":"ar-1","title":"a","status":"open","type":"feature","priority":1,"labels":["model:opus"]}}' ;;
  "show ar-2") echo '{"issue":{"id":"ar-2","title":"b","status":"open","type":"bug","priority":2,"labels":["human","model:opus"]}}' ;;
  "cost "*) echo '{"by":"model","groups":[{"key":"claude-opus-5-5","output":12,"cost_usd":"0"}],"total":{"key":"","cost_usd":"0"}}' ;;
  "lock list") echo '{"locks":[]}' ;;
  *) echo '{"error":{"code":"invalid","message":"the stand-in does not do that"}}'; exit 1 ;;
esac
`

// A dry run, end to end through aries's wiring: it reads ready, each
// issue, the cost, the pause lock and the slots, says what it would
// start and skip and why, and claims, makes, writes and starts nothing,
// with no key.
func TestDispatchDryRun(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the loop does not run on Windows")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	e := newDispatchEnv(t)
	for _, args := range [][]string{{"init", "--quiet", "--initial-branch=main"},
		{"-c", "user.name=alice", "-c", "user.email=alice@example.com", "commit", "--quiet", "--allow-empty", "-m", "first"}} {
		if out, err := exec.Command("git", append([]string{"-C", e.dir}, args...)...).CombinedOutput(); err != nil { //nolint:gosec // git, with the test's arguments
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// Without the brief template it refuses before it reads anything.
	if code, out, errb := e.run(t, "dispatch", "run", "--dry-run", "--once"); code != ExitFailure || !strings.Contains(out+errb, ".aries/brief.txt is missing") {
		t.Errorf("no template: exit %d, %q %q", code, out, errb)
	}
	tmpl, err := os.ReadFile(filepath.Join("..", "..", ".aries", "brief.txt"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(e.dir, ".aries", "brief.txt"), string(tmpl))
	bin := t.TempDir()
	e.aries = filepath.Join(bin, "aries")
	if err := os.WriteFile(e.aries, []byte(standinAries), 0o700); err != nil { //nolint:gosec // a script the test runs
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "record")
	e.environ = []string{"PATH=" + os.Getenv("PATH"), "RECORD=" + record}
	code, out, errb := e.run(t, "dispatch", "run", "--dry-run", "--once")
	if code != ExitOK {
		t.Fatalf("exit %d\nstdout %s\nstderr %s", code, out, errb)
	}
	for _, want := range []string{"wake start", "would start ar-1 (model opus)", "skip ar-2 [human]: labeled human"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	// The filters skip with a rule of their own, and are named.
	code, out, errb = e.run(t, "dispatch", "run", "--dry-run", "--once", "--models", "haiku,sonnet")
	if code != ExitOK || !strings.Contains(out, "only models haiku,sonnet") || !strings.Contains(out, "skip ar-1 [only-models]: ") || strings.Contains(out, "would start") {
		t.Errorf("--models: exit %d\nstdout %s\nstderr %s", code, out, errb)
	}
	code, out, errb = e.run(t, "dispatch", "run", "--dry-run", "--once", "--labels", "trial")
	if code != ExitOK || !strings.Contains(out, "skip ar-1 [only-labels]: ") {
		t.Errorf("--labels: exit %d\nstdout %s\nstderr %s", code, out, errb)
	}
	code, out, errb = e.run(t, "dispatch", "run", "--dry-run", "--once", "--models", "gpt")
	if code != ExitUsage || !strings.Contains(errb+out, "unknown model") || !strings.Contains(errb+out, "fix: use one of haiku, opus, sonnet") {
		t.Errorf("--models gpt: exit %d, %q %q", code, out, errb)
	}
	b, err := os.ReadFile(record) //nolint:gosec // the stand-in's record, in the test's directory
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		cmd, _, _ := strings.Cut(line, " ")
		if cmd != "ready" && cmd != "show" && cmd != "cost" && cmd != "lock" {
			t.Errorf("a dry run ran aries %s", line)
		}
		if strings.HasPrefix(line, "lock ") && !strings.HasPrefix(line, "lock list ") {
			t.Errorf("a dry run ran aries %s", line)
		}
	}
	paths := dispatch.PathsFor(e.data, "0f6e5c1a-1111-4222-8333-444455556666")
	if _, err := os.Stat(paths.State); err == nil {
		t.Error("a dry run wrote the state file")
	}
	if _, err := os.Stat(paths.Log); err == nil {
		t.Error("a dry run created the log")
	}
	if _, err := os.Stat(filepath.Join(e.dir, ".aries-dispatch")); err == nil {
		t.Error("a dry run made the worktrees' directory")
	}
	// Without --dry-run and with no key kept, it refuses, naming the fix.
	code, _, errb = e.run(t, "dispatch", "run", "--once")
	if code != ExitFailure || !strings.Contains(errb, "fix: keep one with `aries dispatch key set`") {
		t.Errorf("with no key: exit %d, %q", code, errb)
	}
}
