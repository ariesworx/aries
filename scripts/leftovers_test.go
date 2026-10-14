package scripts_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// leftoversScript is the script beside this test.
const leftoversScript = "leftovers.sh"

// runLeftovers runs the script on a command given as shell text, with
// base as the temporary directory it makes its own under, and returns
// what it printed and its exit status. settle is how long it waits for
// processes to end, in seconds, and hold a named pipe the command's
// processes may wait on ($HOLD). env adds to the environment, which has
// no GOTMPDIR or GITHUB_ACTIONS of the test's own: go test passes on its
// GOTMPDIR, and CI sets both.
func runLeftovers(t *testing.T, base, hold, settle, shell string, env ...string) (stdout, stderr string, status int) {
	t.Helper()
	cmd := exec.Command("sh", leftoversScript, "sh", "-c", shell) //nolint:gosec // the repository's script, and the test's own commands
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GOTMPDIR=") && !strings.HasPrefix(kv, "GITHUB_ACTIONS=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "TMPDIR="+base, "HOLD="+hold, "STARFIX_LEFTOVERS_WAIT="+settle)
	cmd.Env = append(cmd.Env, env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		status = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run %s: %v", leftoversScript, err)
	}
	return out.String(), errb.String(), status
}

// TestLeftoversScript runs scripts/leftovers.sh on commands that leave
// nothing, a file, a directory and a process, and on ones that fail.
func TestLeftoversScript(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("scripts/leftovers.sh is for a POSIX shell")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("no sh: %v", err)
	}
	tests := []struct {
		name   string
		shell  string // the command, which sees the directory as $TMPDIR
		settle string
		linux  bool // only where a process's environment can be read (/proc)
		letGo  bool // the process is let go while the script runs, not after
		gotmp  bool // the script is given a GOTMPDIR, $GOBASE
		github bool // as in GitHub Actions (GITHUB_ACTIONS=true)
		status int
		stdout string   // exactly
		want   []string // in stderr
		not    []string // not in stderr
	}{
		{name: "a command that leaves nothing", shell: `d=$(mktemp -d) && echo made >"$d/f" && rm -r "$d" && echo out`,
			stdout: "out\n", not: []string{"left"}},
		{name: "the command's output and status pass through", shell: `echo out; echo err >&2; exit 0`,
			stdout: "out\n", want: []string{"err\n"}},
		{name: "a file left behind fails a command that passed", shell: `echo x >"$TMPDIR/forgotten.sock"`,
			status: 1, want: []string{"leftovers.sh: the command left 1 entry under its temporary directory", "forgotten.sock",
				"fix: "}, not: []string{"::error", ": t.TempDir", ": os.MkdirTemp"}},
		// A directory's name says which test made it: t.TempDir names it
		// for the test, and the short ones are os.MkdirTemp's prefixes.
		{name: "directories left behind are counted and named, with whose they are", shell: `mkdir -p "$TMPDIR/TestOne123/001" "$TMPDIR/sfe456" "$TMPDIR/starfix-test-home-789" && echo x >"$TMPDIR/TestOne123/001/f"`,
			status: 1, want: []string{"left 3 entries", "  TestOne123 (", " KB): t.TempDir of TestOne\n", "  sfe456 (", " KB): os.MkdirTemp(\"\", \"sfe\")\n",
				"  starfix-test-home-789 (", " KB): os.MkdirTemp(\"\", \"starfix-test-home-\")\n"}},
		{name: "a test's directory left by a failing command is named too", shell: `mkdir "$TMPDIR/TestDiedToo1"; exit 4`,
			status: 4, want: []string{"TestDiedToo1 (", " KB): t.TempDir of TestDiedToo\n"}},
		{name: "with GOTMPDIR, the command is given one of its own under it", gotmp: true,
			shell: `case $GOTMPDIR in "$GOBASE"/sfl.*) ;; *) echo "GOTMPDIR is $GOTMPDIR" >&2; exit 9 ;; esac; [ -d "$GOTMPDIR" ] && [ "$GOTMPDIR" != "$TMPDIR" ]`,
			not:   []string{"left"}},
		{name: "without GOTMPDIR, the command has none", shell: `[ -z "${GOTMPDIR+set}" ]`, not: []string{"left"}},
		// go test makes t.TempDir under GOTMPDIR when it is set, as CI sets it.
		{name: "a file left in GOTMPDIR fails a command that passed", gotmp: true, shell: `mkdir "$GOTMPDIR/TestGo77"`,
			status: 1, want: []string{"leftovers.sh: the command left 1 entry under its GOTMPDIR", "  TestGo77 (", " KB): t.TempDir of TestGo\n", "fix: "},
			not: []string{"under its temporary directory"}},
		{name: "in GitHub Actions, what was left is an annotation too", github: true, shell: `echo x >"$TMPDIR/TestAnnotated5"`,
			status: 1, want: []string{"::error title=leftovers.sh::the command left 1 entry under its temporary directory:%0A  TestAnnotated5 (", " KB): t.TempDir of TestAnnotated%0Afix: "}},
		{name: "in GitHub Actions, nothing left is no annotation", github: true, shell: `exit 0`, not: []string{"::error"}},
		{name: "a failing command keeps its status, and what it left is said", shell: `mkdir "$TMPDIR/TestDied9"; exit 7`,
			status: 7, want: []string{"TestDied9", "the command failed"}, not: []string{"fix: "}},
		{name: "a failing command that leaves nothing says nothing", shell: `exit 3`, status: 3, not: []string{"leftovers.sh"}},
		// These leave a process that waits to be let go ($HOLD, a named
		// pipe of the test's), which the test does once the script has
		// reported, so that nothing here depends on how long anything
		// takes and the test leaves no process.
		// Its command line names the directory, as a dolt sql-server's does.
		{name: "a process left running fails a command that passed",
			shell:  `sh -c 'read -r x <"$HOLD"' "$TMPDIR/job" >/dev/null 2>&1 &`,
			settle: "0", status: 1, want: []string{"leftovers.sh: the command left 1 process running", "/job"}, not: []string{"under its temporary directory"}},
		{name: "a process that names the directory only in its environment", linux: true,
			shell:  `sh -c 'read -r x <"$HOLD"' >/dev/null 2>&1 &`,
			settle: "0", status: 1, want: []string{"leftovers.sh: the command left 1 process running", "read -r x"}},
		{name: "a process says which entry it runs from",
			shell:  `mkdir "$TMPDIR/TestJob31"; sh -c 'read -r x <"$HOLD"' "$TMPDIR/TestJob31/job" >/dev/null 2>&1 &`,
			settle: "0", status: 1, want: []string{"the command left 1 process running", "/TestJob31/job\n      in TestJob31: t.TempDir of TestJob\n"}},
		{name: "a process that ends while the script waits is not one left", letGo: true,
			shell:  `sh -c 'read -r x <"$HOLD"' "$TMPDIR/job" >/dev/null 2>&1 &`,
			settle: "60", not: []string{"left"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.linux && runtime.GOOS != "linux" {
				t.Skip("a process's environment is read from /proc")
			}
			base := t.TempDir()
			hold := filepath.Join(t.TempDir(), "hold")
			if out, err := exec.CommandContext(t.Context(), "mkfifo", hold).CombinedOutput(); err != nil { //nolint:gosec // a path in the test's own directory
				t.Skipf("mkfifo: %v: %s", err, out)
			}
			// Writing to the pipe waits for the process that reads it, and
			// lets it go; the context ends the wait where there is none.
			release := func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				_ = exec.CommandContext(ctx, "sh", "-c", `echo go >"$1"`, "sh", hold).Run() //nolint:gosec // a constant script; the path is an argument
			}
			if tc.letGo {
				var wg sync.WaitGroup
				wg.Go(release)
				defer wg.Wait()
			} else if strings.Contains(tc.shell, "$HOLD") {
				defer release()
			}
			var env []string
			gobase := ""
			if tc.gotmp {
				gobase = t.TempDir()
				env = append(env, "GOTMPDIR="+gobase, "GOBASE="+gobase)
			}
			if tc.github {
				env = append(env, "GITHUB_ACTIONS=true")
			}
			stdout, stderr, status := runLeftovers(t, base, hold, tc.settle, tc.shell, env...)
			if status != tc.status {
				t.Errorf("exit status %d, want %d\nstderr:\n%s", status, tc.status, stderr)
			}
			if stdout != tc.stdout {
				t.Errorf("stdout = %q, want %q", stdout, tc.stdout)
			}
			for _, want := range tc.want {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr lacks %q:\n%s", want, stderr)
				}
			}
			for _, not := range tc.not {
				if strings.Contains(stderr, not) {
					t.Errorf("stderr has %q:\n%s", not, stderr)
				}
			}
			// Whatever the command left, the script's own directories are gone.
			for _, dir := range []string{base, gobase} {
				if dir == "" {
					continue
				}
				entries, err := os.ReadDir(dir)
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range entries {
					t.Errorf("the script left %s in the temporary directory it was given", filepath.Join(dir, e.Name()))
				}
			}
		})
	}

	t.Run("no command is a usage error", func(t *testing.T) {
		t.Parallel()
		cmd := exec.Command("sh", leftoversScript)
		cmd.Env = append(os.Environ(), "TMPDIR="+t.TempDir())
		out, err := cmd.CombinedOutput()
		if ee, ok := errors.AsType[*exec.ExitError](err); !ok || ee.ExitCode() != 2 || !strings.Contains(string(out), "usage: leftovers.sh") {
			t.Errorf("leftovers.sh with no command = %v, %q; want exit status 2 and the usage", err, out)
		}
	})
}
