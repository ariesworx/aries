package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repo makes a repository's top in a temporary directory, with the
// starting policy file, and returns it with the policy loaded.
func repo(t testing.TB) (string, *Policy) {
	t.Helper()
	top, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(top, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join("..", "..", File))
	if err != nil {
		t.Fatal(err)
	}
	p, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	return top, p
}

// session is the environment of the sessions the tests stand for: the
// one the dispatcher gives its runs.
func session(name string) string {
	return map[string]string{"GOTOOLCHAIN": "local", "GOFLAGS": "-mod=readonly"}[name]
}

func bash(top, cmd string) Call {
	return Call{Tool: "Bash", Command: cmd, Cwd: top, Top: top, Getenv: session}
}

// Every command form the brief template and AGENTS.md's Commands and
// Test tiers issue is allowed (docs/design/guard.md §5).
func TestDecideAllows(t *testing.T) {
	t.Parallel()
	top, p := repo(t)
	for _, cmd := range []string{
		"go test -race ./internal/dispatch/",
		"go test -p 2 ./...",
		"go test ./scripts/",
		"go test -count=3 -run TestX ./internal/store/",
		"go test -race -run 'TestSoak$' ./internal/e2e",
		"ARIES_REQUIRE_POSTGRES=1 ARIES_SOAK=10m go test -race -timeout 45m -run 'TestSoak$' -v ./internal/e2e/",
		"GOTOOLCHAIN=local GOFLAGS=-mod=readonly go test -race ./...",
		"ARIES_REQUIRE_POSTGRES=1 scripts/leftovers.sh go test -race -shuffle=on ./...",
		"ARIES_REQUIRE_POSTGRES=1 ARIES_E2E_TRANSPORT=https scripts/leftovers.sh go test -race -shuffle=on ./internal/e2e/",
		"scripts/gate.sh --quick --pgtest-lock",
		"./scripts/gate.sh --quick",
		"gofmt -l .",
		"go vet ./...",
		"golangci-lint run ./...",
		"go mod verify && go -C internal/tools/pin mod verify",
		"go mod tidy -diff && go -C internal/tools/pin mod tidy -diff",
		`git ls-files -co --exclude-standard -z "*.sh" | xargs -0 shellcheck`,
		"go tool -modfile=internal/tools/pin/go.mod govulncheck ./...",
		"GOOS=darwin GOARCH=amd64 go vet ./...",
		"GOOS=windows GOARCH=amd64 go build ./cmd/...",
		"git fetch origin && git checkout -b fix/x origin/main",
		"git add -A && git commit -m x",
		"git commit -m \"$(cat <<'EOF'\nsubject\n\nbody\nEOF\n)\"",
		"git push -u origin HEAD",
		"git push origin fix/x",
		"git merge origin/main",
		"gh pr create --title x --body-file b.md",
		"gh pr view 12 && gh pr checks 12",
		"cd internal/dispatch && go test ./...",
		"go test ./... | tail -20",
		"go test ./... 2>&1 | tail -5",
		"go test ./... > out.txt 2>/dev/null",
		"sed -n 1,20p x.go",
		"find . -name '*.go'",
		"ls internal; wc -l x.go",
	} {
		if v := p.Decide(bash(top, cmd), Headless); v.Answer != Allow {
			t.Errorf("%q: %s (%s): %s; want allow", cmd, v.Answer, v.Rule, v.Reason)
		}
	}
}

// Every wrapped, prefixed or compound form of a denied command is denied
// with the rule's fix.
func TestDecideDenies(t *testing.T) {
	t.Parallel()
	top, p := repo(t)
	for _, tc := range []struct{ cmd, rule string }{
		{"GOFLAGS=-mod=mod go test ./...", "env GOFLAGS"},
		{"GOTOOLCHAIN=go1.20 go build", "env GOTOOLCHAIN"},
		{"gh pr merge 1", "deny 1"},
		{"gh pr merge --squash 1", "deny 1"},
		{"FOO=bar gh pr merge 1", "deny 1"},
		{"/usr/local/bin/gh pr merge 1", "deny 1"},
		{"gh -R o/r pr merge 1", "deny 1"},
		{"gh --repo=o/r pr merge 1", "deny 1"},
		{"scripts/leftovers.sh gh pr merge 1", "deny 1"},
		{"env gh pr merge 1", "deny 1"},
		{"env -i -u BAR FOO=1 gh pr merge 1", "deny 1"},
		{"env -S 'gh pr merge 1'", "deny 1"},
		{"command gh pr merge 1", "deny 1"},
		{"exec gh pr merge 1", "deny 1"},
		{"time -p gh pr merge 1", "deny 1"},
		{"timeout 5 gh api user", "deny 2"},
		{"timeout -s KILL --kill-after=1 5 gh api user", "deny 2"},
		{"nohup gh pr merge 1 &", "deny 1"},
		{"nice -n 5 gh pr merge 1", "deny 1"},
		{"stdbuf -oL gh pr merge 1", "deny 1"},
		{"setsid gh api x", "deny 2"},
		{"flock /tmp/l gh api x", "deny 2"},
		{"flock -w 3 /tmp/l -c 'gh api x'", "deny 2"},
		{"watch -n 5 gh api x", "deny 2"},
		{"xargs -n1 gh pr merge < ids", "deny 1"},
		{"echo 1 | xargs gh pr merge", "deny 1"},
		{"sh -c 'gh pr merge 1'", "deny 1"},
		{`bash -lc "gh api user"`, "deny 2"},
		{"zsh -ec 'true; gh api y'", "deny 2"},
		{"bash scripts/x.sh && gh api y", "deny 2"},
		{"echo $(gh pr merge 1)", "deny 1"},
		{"x=`gh api user`", "deny 2"},
		{"(cd x && gh pr merge 1)", "deny 1"},
		{"git status && gh pr merge 12", "deny 1"},
		{"cd /tmp && gh pr merge 1", "deny 1"},
		{"if true; then gh pr merge 1; fi", "deny 1"},
		{"for i in 1; do gh api x; done", "deny 2"},
		{"while true; do gh api x; done", "deny 2"},
		{"diff <(gh api x) y", "deny 2"},
		{"find . -name x -exec gh pr merge 1 \\;", "deny 6"},
		{"find . -delete", "deny 6"},
		{"find . -name '*.tmp' -exec rm {} +", "deny 6"},
		{"echo main | xargs git push origin", "deny 3"},
		{"git -C . push origin main", "deny 4"},
		{"git 'push' origin main", "deny 4"},
		{"git push origin HEAD:refs/heads/main", "deny 4"},
		{"git push origin HEAD:main", "deny 4"},
		{"git push origin :main", "deny 4"},
		{"git push origin main:main", "deny 4"},
		{"git push origin +fix/x", "deny 3"},
		{"git push --force origin HEAD", "deny 3"},
		{"git push -fu origin HEAD", "deny 3"},
		{"git push --force-with-lease=fix/x origin fix/x", "deny 3"},
		{"git push origin --delete fix/x", "deny 3"},
		{"git push origin $BRANCH", "deny 3"},
		{"git -c alias.m='!gh pr merge 1' m", "deny 5"},
		{"git -c ALIAS.m=x m", "deny 5"},
		{"sed -i s/a/b/ x.go", "deny 7"},
		{"sed -ni p x.go", "deny 7"},
		{"sudo go test ./...", "deny 8"},
		{"gh $X pr merge 1", "deny 1"},
		{"echo x > .mcp.json", "deny 11"},
		{"cat x >> .claude/settings.json", "deny 11"},
		{"go test ./... | tee .aries/guard.yaml", "deny 11"},
		{"echo x > internal/release/keys.go", "deny 11"},
	} {
		v := p.Decide(bash(top, tc.cmd), Desktop)
		if v.Answer != Deny || v.Rule != tc.rule {
			t.Errorf("%q: %s (%s): %s; want deny (%s)", tc.cmd, v.Answer, v.Rule, v.Reason, tc.rule)
			continue
		}
		checkReason(t, tc.cmd, v.Reason)
	}
}

// checkReason holds a denial's reason to docs/design/guard.md §7: where
// the rule is, a fix, that only this call was refused, and none of the
// command's own arguments.
func checkReason(t *testing.T, cmd, reason string) {
	t.Helper()
	if !strings.HasPrefix(reason, "aries guard: ") || !strings.Contains(reason, "\nfix: ") ||
		!strings.HasSuffix(reason, "Only this command was refused; other commands, Bash included, still run.") {
		t.Errorf("%q: reason %q is not in the form of guard.md §7", cmd, reason)
	}
	for _, secret := range []string{"-mod=mod", "go1.20", "o/r", "KILL", "/tmp/l", "x.go", "fix/x", "$BRANCH", "$X", "ids"} {
		if strings.Contains(cmd, secret) && strings.Contains(reason, secret) {
			t.Errorf("%q: reason quotes the command's %q: %q", cmd, secret, reason)
		}
	}
}

// A command the guard cannot read is denied in a headless run, and
// asked of the person in a Desktop session.
func TestDecideUnreadable(t *testing.T) {
	t.Parallel()
	top, p := repo(t)
	for _, cmd := range []string{
		`eval "gh pr merge 1"`,
		"$CMD pr merge 1",
		"sh <<EOF\ngh pr merge 1\nEOF\n",
		"echo gh pr merge 1 | bash",
		"source x.sh",
		". x.sh",
		"case x in a) gh api x;; esac",
		"go test ./... > $OUT",
		"g* push origin main",
		"echo 'open",
		"timeout",
		"env -Z gh",
		"go test && cd /tmp && echo x > y",
		strings.Repeat("sh -c ", 10) + "x",
	} {
		v := p.Decide(bash(top, cmd), Headless)
		if v.Answer != Deny || v.Rule != "unreadable" || !strings.Contains(v.Reason, "write it as plain commands") {
			t.Errorf("headless %q: %s (%s): %s; want deny (unreadable)", cmd, v.Answer, v.Rule, v.Reason)
		}
		if v := p.Decide(bash(top, cmd), Desktop); v.Answer != Ask {
			t.Errorf("Desktop %q: %s (%s): %s; want ask", cmd, v.Answer, v.Rule, v.Reason)
		}
	}
}

// What the policy neither allows nor denies is left to Claude Code's
// own rules: no decision.
func TestDecideNone(t *testing.T) {
	t.Parallel()
	top, p := repo(t)
	for _, cmd := range []string{
		"echo hi",
		"curl example.com",
		"go test ./... && curl example.com",
		"gh pr close 12",
		"rm -rf x",
		"mise exec -- go test ./...",
		"go test ./... > /tmp/out",
		"PATH=/x:$PATH go test ./...",
		"cd /tmp && ./scripts/gate.sh",
		"x/git status",
		"/usr/bin/git status",
		"timeout 5 curl example.com",
		"scripts/leftovers.sh",
	} {
		if v := p.Decide(bash(top, cmd), Headless); v.Answer != None {
			t.Errorf("%q: %s (%s): %s; want no decision", cmd, v.Answer, v.Rule, v.Reason)
		}
	}
}

// With no policy file the built-in denies apply, and nothing else is
// decided (decision 4).
func TestDecideBuiltin(t *testing.T) {
	t.Parallel()
	top, _ := repo(t)
	p := BuiltinPolicy()
	for _, tc := range []struct {
		cmd    string
		answer Answer
	}{
		{"gh pr merge 1", Deny},
		{"sh -c 'gh api x'", Deny},
		{"git push --force origin HEAD", Deny},
		{"git -C . push origin main", Deny},
		{"go test ./...", None},
		{"eval x", None},
		{"echo x > .mcp.json", None},
		{"echo x > .aries/guard.yaml", Deny},
	} {
		v := p.Decide(bash(top, tc.cmd), Headless)
		if v.Answer != tc.answer {
			t.Errorf("%q: %s (%s): %s; want %s", tc.cmd, v.Answer, v.Rule, v.Reason, tc.answer)
		}
		if v.Answer == Deny && v.Rule != "builtin" {
			t.Errorf("%q: rule %q; want builtin", tc.cmd, v.Rule)
		}
	}
	// The file's own rules come first; the built-in ones hold under them.
	top2, file := repo(t)
	if v := file.Decide(bash(top2, "gh pr merge 1"), Desktop); v.Rule != "deny 1" {
		t.Errorf("with the file, gh pr merge: %s", v.Rule)
	}
	empty, err := Load([]byte("version: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if v := empty.Decide(bash(top2, "git push origin main"), Desktop); v.Answer != Deny || v.Rule != "builtin" {
		t.Errorf("an empty file, git push origin main: %s (%s)", v.Answer, v.Rule)
	}
}

// The file tools are checked by path, after symlinks, from the
// repository's top; the snapshot directory and the policy file are
// never written.
func TestDecideFiles(t *testing.T) {
	t.Parallel()
	top, p := repo(t)
	snaps := filepath.Join(t.TempDir(), "guard")
	if err := os.Symlink(filepath.Join(top, ".mcp.json"), filepath.Join(top, "link")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		tool, path string
		policy     *Policy
		answer     Answer
		rule       string
	}{
		{"Edit", ".claude/settings.json", p, Deny, "deny 11"},
		{"Write", ".mcp.json", p, Deny, "deny 11"},
		{"Edit", ".aries/guard.yaml", p, Deny, "deny 11"},
		{"Write", "internal/release/keys.go", p, Deny, "deny 11"},
		{"Edit", "link", p, Deny, "deny 11"},
		{"Write", "new/dir/.mcp.json", p, Deny, "deny 11"},
		{"Edit", "internal/guard/guard.go", p, None, ""},
		{"Write", "/elsewhere/x", p, None, ""},
		{"Write", filepath.Join(snaps, "s.yaml"), p, Deny, "builtin"},
		{"Edit", ".aries/guard.yaml", BuiltinPolicy(), Deny, "builtin"},
		{"Edit", ".mcp.json", BuiltinPolicy(), None, ""},
		{"NotebookEdit", ".claude/x.ipynb", p, Deny, "deny 11"},
	} {
		path := tc.path
		if !filepath.IsAbs(path) {
			path = filepath.Join(top, path)
		}
		c := Call{Tool: tc.tool, FilePath: path, Cwd: top, Top: top, Getenv: session, Protected: []string{snaps}}
		v := tc.policy.Decide(c, Headless)
		if v.Answer != tc.answer || v.Rule != tc.rule {
			t.Errorf("%s %s: %s (%s): %s; want %s (%s)", tc.tool, tc.path, v.Answer, v.Rule, v.Reason, tc.answer, tc.rule)
		}
	}
}

// BenchmarkDecide is the guard's work for one call, a command of about
// 1 KiB against the starting file, the file parsed afresh as the hook
// does (docs/design/guard.md §11).
func BenchmarkDecide(b *testing.B) {
	top, _ := repo(b)
	raw, err := os.ReadFile(filepath.Join("..", "..", File))
	if err != nil {
		b.Fatal(err)
	}
	cmd := strings.Repeat("go test -race -run 'TestX$' ./internal/store/ && ", 20) + "git push -u origin HEAD"
	b.ReportAllocs()
	for b.Loop() {
		p, err := Load(raw)
		if err != nil {
			b.Fatal(err)
		}
		if v := p.Decide(bash(top, cmd), Headless); v.Answer != Allow {
			b.Fatal(v)
		}
	}
}

// The guard's own work for one call stays small: its allocations are
// bounded, which a wall clock in a parallel suite could not hold
// steadily (BenchmarkDecide records the time; docs/design/guard.md §11
// and §14 the budget and what the binary measured).
func TestDecideAllocations(t *testing.T) {
	// Serial: testing.AllocsPerRun refuses to run in a parallel test.
	top, _ := repo(t)
	raw, err := os.ReadFile(filepath.Join("..", "..", File))
	if err != nil {
		t.Fatal(err)
	}
	cmd := strings.Repeat("go test -race -run 'TestX$' ./internal/store/ && ", 20) + "git push -u origin HEAD"
	allocs := testing.AllocsPerRun(20, func() {
		p, err := Load(raw)
		if err != nil {
			t.Fatal(err)
		}
		_ = p.Decide(bash(top, cmd), Headless)
	})
	if allocs > 4000 {
		t.Errorf("a decision on a 1 KiB command allocates %v times; it did about 1,800", allocs)
	}
}

// FuzzDecide holds the guard to its rule that one denied part denies the
// whole: a command denied alone is denied after another command, and
// nothing panics.
func FuzzDecide(f *testing.F) {
	top, p := repo(f)
	for _, s := range []string{"gh pr merge 1", "sh -c 'gh api x'", "git -C . push origin main", "eval x",
		"find . -exec rm {} +", "echo x > .mcp.json", "GOFLAGS=x go test", "xargs git push o"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, cmd string) {
		v := p.Decide(bash(top, cmd), Headless)
		if v.Answer != Deny {
			return
		}
		if w := p.Decide(bash(top, "go vet ./...\n"+cmd), Headless); w.Answer != Deny {
			t.Fatalf("%q is denied (%s), but after go vet: %s", cmd, v.Rule, w.Answer)
		}
	})
}
