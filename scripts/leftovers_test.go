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
// processes may wait on ($HOLD).
func runLeftovers(t *testing.T, base, hold, settle, shell string) (stdout, stderr string, status int) {
	t.Helper()
	cmd := exec.Command("sh", leftoversScript, "sh", "-c", shell) //nolint:gosec // the repository's script, and the test's own commands
	cmd.Env = append(os.Environ(), "TMPDIR="+base, "HOLD="+hold, "STARFIX_LEFTOVERS_WAIT="+settle)
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
				"fix: "}},
		{name: "directories left behind are counted and named", shell: `mkdir -p "$TMPDIR/TestOne123/001" "$TMPDIR/sfe456" && echo x >"$TMPDIR/TestOne123/001/f"`,
			status: 1, want: []string{"left 2 entries", "TestOne123", "sfe456"}},
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
			stdout, stderr, status := runLeftovers(t, base, hold, tc.settle, tc.shell)
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
			// Whatever the command left, the script's own directory is gone.
			entries, err := os.ReadDir(base)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				t.Errorf("the script left %s in the temporary directory it was given", filepath.Join(base, e.Name()))
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
