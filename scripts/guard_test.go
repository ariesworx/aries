package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/guard"
)

// briefCommands are the commands the brief template tells a worker to
// run; each must stay in the template as written.
var briefCommands = []string{
	"go test -p 2 ./...",
	"scripts/gate.sh --quick --pgtest-lock",
}

// testTierCommands are the commands AGENTS.md's Test tiers and
// CONTRIBUTING.md's short soak name.
var testTierCommands = []string{
	"go test ./scripts/",
	"scripts/gate.sh --quick",
	"go test -race -count=3 -run TestX ./internal/store/",
	"go test -race -run 'TestSoak$' ./internal/e2e",
	"deploy/docker/smoke.sh",
}

// Every command a worker is told to run is one the repository's policy
// allows in a headless run, in the environment the dispatcher gives it,
// so that a change to the brief template, AGENTS.md or
// .aries/guard.yaml that would deny a run fails here first
// (docs/design/guard.md §8, the preflight).
func TestGuardAllowsWorkersCommands(t *testing.T) {
	t.Parallel()
	top, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	top = guard.Top(top)
	b, err := os.ReadFile(filepath.Join(top, guard.File)) //nolint:gosec // the repository's own policy
	if err != nil {
		t.Fatal(err)
	}
	p, err := guard.Load(b)
	if err != nil {
		t.Fatalf("%s: %v", guard.File, err)
	}
	brief := read(t, ".aries/brief.txt")
	for _, c := range briefCommands {
		if !strings.Contains(brief, "`"+c+"`") {
			t.Errorf(".aries/brief.txt no longer says `%s`: update briefCommands", c)
		}
	}
	agents := read(t, "AGENTS.md")
	for _, c := range []string{"go test ./scripts/", "scripts/gate.sh --quick", "-count=3", "deploy/docker/smoke.sh"} {
		if !strings.Contains(agents, c) {
			t.Errorf("AGENTS.md's Test tiers no longer say %q: update testTierCommands", c)
		}
	}
	env := map[string]string{"GOTOOLCHAIN": "local", "GOFLAGS": "-mod=readonly"}
	cmds := append(append(append([]string{}, briefCommands...), testTierCommands...), agentsCommands(t)...)
	for _, c := range cmds {
		v := p.Decide(guard.Call{Tool: "Bash", Command: c, Cwd: top, Top: top, Getenv: func(k string) string { return env[k] }}, guard.Headless)
		if v.Answer != guard.Allow {
			t.Errorf("%s does not allow %q in a headless run: %s (%s) %s", guard.File, c, v.Answer, v.Rule, v.Reason)
		}
	}
}
