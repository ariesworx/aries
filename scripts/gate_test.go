// Package scripts_test tests the contributor scripts beside it: that
// gate.sh runs the commands AGENTS.md lists, what it reports, and what
// install-dolt.sh installs and refuses.
package scripts_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// read returns a file of the repository, named from its root.
func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", filepath.FromSlash(path))) //nolint:gosec // a file of this repository, named by the test
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// agentsCommands returns the commands in the code block of AGENTS.md's
// Commands section, without their comments.
func agentsCommands(t *testing.T) []string {
	t.Helper()
	_, rest, ok := strings.Cut(read(t, "AGENTS.md"), "\n## Commands\n")
	if !ok {
		t.Fatal("AGENTS.md has no Commands section")
	}
	_, rest, ok = strings.Cut(rest, "```sh\n")
	block, _, closed := strings.Cut(rest, "```")
	if !ok || !closed {
		t.Fatal("AGENTS.md's Commands section has no sh code block")
	}
	var cmds []string
	for line := range strings.Lines(block) {
		cmd, _, _ := strings.Cut(line, "  #")
		if cmd = strings.TrimSpace(cmd); cmd != "" && !strings.HasPrefix(cmd, "#") {
			cmds = append(cmds, cmd)
		}
	}
	return cmds
}

// gateCommands returns the commands gate.sh runs as the gate: its "both"
// and "gate" lines, in order.
func gateCommands(t *testing.T) []string {
	t.Helper()
	var cmds []string
	for _, m := range regexp.MustCompile(`(?m)^(?:both|gate) '([^']+)'`).FindAllStringSubmatch(read(t, "scripts/gate.sh"), -1) {
		cmds = append(cmds, m[1])
	}
	return cmds
}

// TestGateMatchesAgents keeps the gate a person runs and the gate an
// agent reads the same list, in the same order.
func TestGateMatchesAgents(t *testing.T) {
	got, want := gateCommands(t), agentsCommands(t)
	if len(want) == 0 {
		t.Fatal("AGENTS.md's Commands block lists no command")
	}
	if !slices.Equal(got, want) {
		t.Errorf("scripts/gate.sh runs\n  %s\nAGENTS.md's Commands block lists\n  %s\nchange both together, and .github/workflows/ci.yml with them",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// TestGateMatchesCI checks the commands CI words exactly as the gate
// does. The others it runs through an action (golangci-lint, which also
// covers gofmt and go vet), in a loop (the builds for other systems) or
// with a coverage profile (the tests).
func TestGateMatchesCI(t *testing.T) {
	ci := read(t, ".github/workflows/ci.yml")
	cmds := gateCommands(t)
	for _, prefix := range []string{"go mod verify", "git ls-files", "go run golang.org/x/vuln/cmd/govulncheck@"} {
		i := slices.IndexFunc(cmds, func(c string) bool { return strings.HasPrefix(c, prefix) })
		if i < 0 {
			t.Errorf("scripts/gate.sh has no command starting %q", prefix)
			continue
		}
		if !strings.Contains(ci, cmds[i]) {
			t.Errorf("ci.yml does not run %q, which scripts/gate.sh does; change both together", cmds[i])
		}
	}
}

// TestGovulncheckPin keeps every govulncheck in the repository at one
// version: the gate's, CI's, the weekly scan's and the release build's.
func TestGovulncheckPin(t *testing.T) {
	pin := regexp.MustCompile(`golang\.org/x/vuln/cmd/govulncheck@(v[0-9.]+)`)
	var want, first string
	for _, path := range []string{
		".github/workflows/ci.yml", ".github/workflows/vuln.yml", ".github/workflows/release.yml",
		"AGENTS.md", "scripts/gate.sh",
	} {
		ms := pin.FindAllStringSubmatch(read(t, path), -1)
		if len(ms) == 0 {
			t.Errorf("%s runs no pinned govulncheck", path)
		}
		for _, m := range ms {
			if want == "" {
				want, first = m[1], path
			}
			if m[1] != want {
				t.Errorf("%s pins govulncheck %s, %s pins %s; change them together", path, m[1], first, want)
			}
		}
	}
}

// Canned govulncheck reports, in the format of v1.8.0.
const (
	vulnStdlib = `=== Symbol Results ===

Vulnerability #1: GO-2026-0001
    An invented finding.
  More info: https://pkg.go.dev/vuln/GO-2026-0001
  Standard library
    Found in: net/http@go1.27.1
    Fixed in: net/http@go1.27.2

Vulnerability #2: GO-2026-0002
    Another.
  Standard library
    Found in: crypto/tls@go1.27.1
    Fixed in: crypto/tls@go1.27.3

Your code is affected by 2 vulnerabilities from the Go standard library.
`
	vulnModule = vulnStdlib + `
Vulnerability #3: GO-2026-0003
    One in a dependency.
  Module: example.com/dep
    Found in: example.com/dep@v1.0.0
    Fixed in: example.com/dep@v1.0.1
`
	vulnUnfixed = `Vulnerability #1: GO-2026-0004
  Standard library
    Found in: net/http@go1.27.1
    Fixed in: N/A
`
	vulnNextRelease = `Vulnerability #1: GO-2026-0005
  Standard library
    Found in: net/http@go1.27.1
    Fixed in: net/http@go1.28.0
`
)

// gateTree is a stand-in checkout holding gate.sh and the files it
// reads, with every tool it runs replaced by a fake that records its
// arguments.
type gateTree struct {
	root, bin string
}

// newGateTree builds the checkout. Each fake appends "NAME ARGS" to
// calls, and fails when that line contains the text in the file "fail".
func newGateTree(t *testing.T) gateTree {
	t.Helper()
	g := gateTree{root: t.TempDir()}
	g.bin = filepath.Join(g.root, "fakebin")
	gate := read(t, "scripts/gate.sh")
	const record = `#!/bin/sh
root=$(dirname "$0")/..
echo "$(basename "$0") $*" >>"$root/calls"
`
	const failing = `if [ -s "$root/fail" ] && echo "$(basename "$0") $*" | grep -qF "$(cat "$root/fail")"; then echo "it broke"; exit 1; fi
`
	files := map[string]string{
		"scripts/gate.sh":              gate,
		"go.mod":                       "module example.com/m\n\ngo 1.27\n",
		".github/workflows/ci.yml":     "      - uses: golangci/golangci-lint-action@0000 # v9\n        with:\n          version: v2.14.0\n",
		"internal/version/dolt.sha256": "# pins\n" + strings.Repeat("0", 64) + "  v9.8.7/dolt-linux-amd64.tar.gz\n",
		// The Dolt the fake install-dolt.sh supplies when PATH's is wrong.
		"scripts/install-dolt.sh": record + `mkdir -p "$root/cache" && printf '#!/bin/sh\necho "dolt version 9.8.7"\n' >"$root/cache/dolt" && chmod +x "$root/cache/dolt" && (cd "$root/cache" && pwd)` + "\n",
		"fakebin/go": record + `case "$*" in
"env GOVERSION") echo go1.27.1 ;;
"env GOOS") cat "$root/goos" ;;
"env GOARCH") echo amd64 ;;
*govulncheck*) cat "$root/vuln"; [ ! -s "$root/vuln" ] || exit 3 ;;
esac
` + failing,
		"fakebin/gofmt":         record + `cat "$root/unformatted"` + "\n",
		"fakebin/golangci-lint": record + `case "$*" in "version --short") cat "$root/lint" ;; esac` + "\n" + failing,
		"fakebin/shellcheck":    record + failing,
		"fakebin/git":           record + `printf 'install.sh\0'` + "\n",
		"fakebin/dolt":          record + `echo "dolt version $(cat "$root/dolt")"` + "\n",
		"goos":                  "linux\n",
		"lint":                  "2.14.0\n",
		"dolt":                  "9.8.7\n",
		"vuln":                  "",
		"unformatted":           "",
		"fail":                  "",
	}
	for path, body := range files {
		g.write(t, path, body)
	}
	return g
}

func (g gateTree) write(t *testing.T, path, body string) {
	t.Helper()
	full := filepath.Join(g.root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o700); err != nil { //nolint:gosec // executable fixtures in a test directory
		t.Fatal(err)
	}
}

// run runs gate.sh and returns its output, exit status and the tools it
// called, without the calls that only ask a tool about itself.
func (g gateTree) run(t *testing.T, args ...string) (out string, status int, calls []string) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{filepath.Join(g.root, "scripts", "gate.sh")}, args...)...) //nolint:gosec // the repository's script, copied by the test
	cmd.Env = []string{
		"PATH=" + g.bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + g.root,
		"TMPDIR=" + t.TempDir(),
	}
	b, err := cmd.CombinedOutput()
	if err != nil {
		ee, ok := err.(*exec.ExitError) //nolint:errorlint // exec returns it unwrapped
		if !ok {
			t.Fatalf("gate.sh: %v", err)
		}
		status = ee.ExitCode()
	}
	log, _ := os.ReadFile(filepath.Join(g.root, "calls")) //nolint:gosec // test temp dir
	for line := range strings.Lines(string(log)) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "go env ") || line == "golangci-lint version --short" || line == "dolt version" {
			continue
		}
		calls = append(calls, line)
	}
	return string(b), status, calls
}

func TestGateScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("scripts/gate.sh is for a POSIX shell")
	}
	for _, tool := range []string{"sh", "awk", "sed", "paste", "xargs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s: %v", tool, err)
		}
	}
	const (
		vet    = "go vet ./..."
		lint   = "golangci-lint run ./..."
		verify = "go mod verify"
		shell  = "shellcheck install.sh"
		vuln   = "go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./..."
		build  = "go build ./cmd/..."
		test   = "go test -race -shuffle=on ./..."
	)
	// The fakes record arguments, not environment, so the commands for
	// another system look like the native ones.
	full := []string{"gofmt -l .", vet, lint, verify, "git ls-files -co --exclude-standard -z *.sh", shell, vuln, vet, build, vet, build, test}
	tests := []struct {
		name   string
		args   []string
		files  map[string]string // replacing the stand-in checkout's
		status int
		calls  []string // nil: not checked
		want   []string // in the output
		not    []string // not in the output
	}{
		{name: "everything passes", calls: full,
			want: []string{"gate: go1.27.1 linux/amd64, golangci-lint 2.14.0, Dolt 9.8.7 (on PATH)",
				"gofmt -l . ... ok (", "gate: passed in ", ": 11 of 11 commands",
				"Not covered by this run:", "TestCheckPeerOtherUID", "deploy/docker/smoke.sh", "The soak test ran its short form"},
			not: []string{"FAILED", "golangci-lint here is", "govulncheck:", "CI runs them on linux/amd64"}},
		{name: "stops at the first failure", files: map[string]string{"unformatted": "cmd/sfx/main.go\n"}, status: 1,
			calls: []string{"gofmt -l ."},
			want: []string{"gofmt -l . ... FAILED (", "    cmd/sfx/main.go", "fix: these files are not formatted",
				"gate: FAILED in ", ": 0 of 11 commands passed", "  failed:  gofmt -l .", "  not run: go vet ./...",
				"  not run: STARFIX_REQUIRE_DOLT=1 go test -race -shuffle=on ./...", "--keep-going runs every command"},
			not: []string{"gate: passed"}},
		{name: "keep going runs the rest", args: []string{"--keep-going"}, files: map[string]string{"fail": "golangci-lint run"}, status: 1,
			calls: full,
			want:  []string{"golangci-lint run ./... ... FAILED (", "    it broke", ": 10 of 11 commands passed", "  failed:  golangci-lint run ./..."},
			not:   []string{"not run:"}},
		{name: "a failing test fails the gate", files: map[string]string{"fail": "go test"}, status: 1,
			want: []string{"STARFIX_REQUIRE_DOLT=1 go test -race -shuffle=on ./... ... FAILED (", ": 10 of 11 commands passed"}},
		{name: "standard library findings a newer Go fixes", files: map[string]string{"vuln": vulnStdlib}, calls: full,
			want: []string{vuln + " ... needs Go 1.27.3 (", "gate: passed in ", "except govulncheck, which needs a newer Go: 10 of 11 commands passed",
				"govulncheck: every finding is in the standard library of go1.27.1 and is fixed by Go 1.27.3",
				"not the change", "nothing was found in the module's dependencies"},
			not: []string{"FAILED"}},
		{name: "a finding in a dependency fails", files: map[string]string{"vuln": vulnModule}, status: 1,
			want: []string{vuln + " ... FAILED (", "Found in: example.com/dep@v1.0.0", "gate: FAILED"},
			not:  []string{"needs Go", "not the change"}},
		{name: "a standard library finding with no fix fails", files: map[string]string{"vuln": vulnUnfixed}, status: 1,
			want: []string{vuln + " ... FAILED ("}, not: []string{"needs Go"}},
		{name: "a finding fixed only in the next Go fails", files: map[string]string{"vuln": vulnNextRelease}, status: 1,
			want: []string{vuln + " ... FAILED ("}, not: []string{"needs Go"}},
		{name: "quick is not the gate", args: []string{"--quick"},
			calls: []string{"gofmt -l .", vet, lint, "git ls-files -co --exclude-standard -z *.sh", shell, "go test ./..."},
			want: []string{"quick (not the gate): go1.27.1", "quick (not the gate): passed in ", ": 5 of 5 commands",
				"--quick skipped these commands of the gate", "go mod verify; go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...; GOOS=darwin",
				"STARFIX_REQUIRE_DOLT=1 go test -race -shuffle=on ./.... Run scripts/gate.sh before you open a pull request"},
			not: []string{"\ngate: "}},
		{name: "another dolt on PATH", files: map[string]string{"dolt": "1.2.3\n"},
			calls: append([]string{"install-dolt.sh"}, full...),
			want:  []string{", Dolt 9.8.7 (from ", "/cache; on PATH: dolt version 1.2.3)", "gate: passed"}},
		{name: "another golangci-lint", files: map[string]string{"lint": "2.13.2\n"}, calls: full,
			want: []string{"golangci-lint 2.13.2,", "gate: passed", "golangci-lint here is 2.13.2 and CI pins 2.14.0"}},
		{name: "not on Linux", files: map[string]string{"goos": "darwin\n"},
			calls: slices.Concat(full[:11], []string{vet, build, lint, test}),
			want: []string{"GOOS=linux GOARCH=amd64 golangci-lint run ./... ... ok", ": 14 of 14 commands",
				"Tests ran on darwin/amd64; CI runs them on linux/amd64", "TestCheckPeer*"},
			not: []string{"TestCheckPeerOtherUID"}},
		{name: "smoke", args: []string{"--smoke"}, files: map[string]string{"deploy/docker/smoke.sh": "#!/bin/sh\necho smoke ok\n"},
			want: []string{"deploy/docker/smoke.sh ... ok (", ": 12 of 12 commands"}, not: []string{"was not run; CI does not run it either"}},
		{name: "unknown option", args: []string{"--nope"}, status: 2, calls: []string{},
			want: []string{"gate.sh: unknown option --nope", "fix: "}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := newGateTree(t)
			for path, body := range tc.files {
				g.write(t, path, body)
			}
			out, status, calls := g.run(t, tc.args...)
			if status != tc.status {
				t.Errorf("gate.sh %v exited %d, want %d", tc.args, status, tc.status)
			}
			if tc.calls != nil && !slices.Equal(calls, tc.calls) {
				t.Errorf("gate.sh %v ran\n  %s\nwant\n  %s", tc.args, strings.Join(calls, "\n  "), strings.Join(tc.calls, "\n  "))
			}
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q", w)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(out, n) {
					t.Errorf("output has %q", n)
				}
			}
			if t.Failed() {
				t.Logf("output of gate.sh %s:\n%s", fmt.Sprint(tc.args), out)
			}
		})
	}
}
