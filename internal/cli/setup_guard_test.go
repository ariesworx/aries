package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// standIn writes an executable shell script named name in a directory of
// its own, standing in for an aries, and returns its path.
func standIn(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil { //nolint:gosec // a stand-in the test runs
		t.Fatal(err)
	}
	return path
}

// setupRun runs aries with the process's RunProgram, as cmd/aries does,
// so that setup runs the program it would write into the guard's hook.
func setupRun(t *testing.T, home string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(t.Context(), args, Env{Stdout: &out, Stderr: &errb,
		Getenv: func(string) string { return "" }, Version: "v0.3.0",
		UserHomeDir: func() (string, error) { return home, nil }, RunProgram: RunProgram})
	return code, out.String(), errb.String()
}

// setup --write runs the aries it writes into Claude Code's guard hook as
// the hook runs it, and refuses one that cannot run it, with a fix, before
// writing any file: an aries from before `guard` exits 2 for the unknown
// command, which Claude Code reads as a block (docs/design/guard.md §6).
// Printing, --check, --remove and a harness without the guard run nothing.
func TestSetupRefusesGuardItCannotRun(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("stands in for aries with POSIX shell scripts")
	}
	old := standIn(t, "aries", `echo 'aries: unknown command "guard"' >&2; echo 'fix: run `+"`aries help`"+` for the commands' >&2; exit 2`)
	current := standIn(t, "aries", `[ "$1 $2" = "guard --hook" ] || exit 1; exit 0`)
	missing := filepath.Join(t.TempDir(), "aries")
	tests := []struct {
		name, command string
		args          []string
		refused       bool
	}{
		{name: "predates guard", command: old, args: []string{"claude-code", "--write"}, refused: true},
		{name: "every agent", command: old, args: []string{"--all", "--write"}, refused: true},
		{name: "not there", command: missing, args: []string{"claude-code", "--write"}, refused: true},
		{name: "runs the guard", command: current, args: []string{"claude-code", "--write"}},
		{name: "print", command: old, args: []string{"claude-code"}},
		{name: "remove", command: old, args: []string{"claude-code", "--remove"}},
		{name: "no guard", command: old, args: []string{"codex", "--write"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root, sub := repoWithConfig(t)
			args := append([]string{"-C", sub, "setup", "--command", tc.command}, tc.args...)
			code, out, errb := setupRun(t, t.TempDir(), args...)
			if !tc.refused {
				if code != ExitOK {
					t.Errorf("aries %s = exit %d\n%s%s; want 0", strings.Join(args, " "), code, out, errb)
				}
				return
			}
			if code != ExitFailure || !strings.Contains(errb, "cannot run `guard --hook`") ||
				!strings.Contains(errb, "fix: upgrade it (aries upgrade), or name an aries that has guard with --command") {
				t.Errorf("aries %s = exit %d\n%s%s; want exit 1 saying it cannot run the guard, with the fix", strings.Join(args, " "), code, out, errb)
			}
			if files := snapshot(t, root); len(files) != 1 {
				t.Errorf("aries %s wrote files though it refused: %v", strings.Join(args, " "), files)
			}
		})
	}
}
