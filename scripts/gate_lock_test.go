package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// standInAries is a stand-in for `aries lock run NAME [--capacity N]
// [--wait] [--for D] -- CMD...`: it records the call, keeps the lock as
// one directory per slot under slots/, refuses with "busy" when the lock
// is full and --wait is not given, waits when it is, and releases its
// slot when the command ends or it is interrupted or terminated.
const standInAries = `#!/bin/sh
root=$(dirname "$0")/..
echo "aries $*" >>"$root/calls"
[ "$1 $2" = "lock run" ] || { echo "stand-in: unexpected $*" >&2; exit 2; }
name=$3
shift 3
cap=1 wait=0
while [ $# -gt 0 ] && [ "$1" != -- ]; do
	case $1 in
	--capacity) cap=$2; shift ;;
	--for) shift ;;
	--wait) wait=1 ;;
	esac
	shift
done
shift
mkdir -p "$root/slots"
slot=
while [ -z "$slot" ]; do
	i=1
	while [ "$i" -le "$cap" ]; do
		if mkdir "$root/slots/$i" 2>/dev/null; then slot=$i; break; fi
		i=$((i + 1))
	done
	[ -n "$slot" ] && break
	[ "$wait" = 1 ] || { echo "aries: $name is busy" >&2; exit 1; }
	sleep 0.1
done
release() { rmdir "$root/slots/$slot"; }
"$@" &
child=$!
# As aries lock run does: ask the command to stop, kill it if it does not.
trap 'kill -TERM "$child" 2>/dev/null; (sleep 1; kill -KILL "$child" 2>/dev/null) & wait "$child" 2>/dev/null; release; exit 130' INT TERM
wait "$child"
rc=$?
release
exit "$rc"
`

// hostName is the machine's name as gate.sh takes it.
func hostName(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("hostname").Output()
	if err != nil {
		t.Skipf("no hostname: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func lockTree(t *testing.T) gateTree {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("scripts/gate.sh is for a POSIX shell")
	}
	g := newGateTree(t)
	g.write(t, "fakebin/aries", standInAries)
	return g
}

// slotsHeld returns how many slots of the stand-in's lock are held.
func (g gateTree) slotsHeld(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(g.root, "slots"))
	if err != nil {
		return 0
	}
	return len(ents)
}

func (g gateTree) started(t *testing.T) int {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(g.root, "started.*"))
	if err != nil {
		t.Fatal(err)
	}
	return len(m)
}

// start runs gate.sh without waiting for it.
func (g gateTree) start(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sh", append([]string{filepath.Join(g.root, "scripts", "gate.sh")}, args...)...) //nolint:gosec // the repository's script, copied by the test
	cmd.Env = append([]string{
		"PATH=" + g.bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + g.root, "TMPDIR=" + t.TempDir(),
	}, g.env...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestGatePgtestLock(t *testing.T) {
	t.Parallel()
	host := hostName(t)
	lock := "aries lock run pgtest/" + host + " --capacity 2 --wait -- sh -c "
	const quickTest = "ARIES_REQUIRE_POSTGRES=1 scripts/leftovers.sh go test ./..."
	for _, tc := range []struct {
		name  string
		args  []string
		env   []string
		calls []string
	}{
		{name: "the flag locks the test step", args: []string{"--quick", "--pgtest-lock"},
			calls: []string{"gofmt -l .", "go vet ./...", "golangci-lint run ./...", "git ls-files -co --exclude-standard -z *.sh", "shellcheck install.sh",
				lock + quickTest, "go test ./..."}},
		{name: "the variable locks it too", args: []string{"--quick"}, env: []string{"ARIES_GATE_PGTEST_LOCK=1"},
			calls: []string{"gofmt -l .", "go vet ./...", "golangci-lint run ./...", "git ls-files -co --exclude-standard -z *.sh", "shellcheck install.sh",
				lock + quickTest, "go test ./..."}},
		{name: "without it nothing changes", args: []string{"--quick"},
			calls: []string{"gofmt -l .", "go vet ./...", "golangci-lint run ./...", "git ls-files -co --exclude-standard -z *.sh", "shellcheck install.sh", "go test ./..."}},
		{name: "a variable that is not 1 does not lock", args: []string{"--quick"}, env: []string{"ARIES_GATE_PGTEST_LOCK=0"},
			calls: []string{"gofmt -l .", "go vet ./...", "golangci-lint run ./...", "git ls-files -co --exclude-standard -z *.sh", "shellcheck install.sh", "go test ./..."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := lockTree(t)
			g.env = tc.env
			out, status, calls := g.run(t, tc.args...)
			if status != 0 {
				t.Errorf("exited %d:\n%s", status, out)
			}
			if !slices.Equal(calls, tc.calls) {
				t.Errorf("ran\n  %s\nwant\n  %s", strings.Join(calls, "\n  "), strings.Join(tc.calls, "\n  "))
			}
			if n := g.slotsHeld(t); n != 0 {
				t.Errorf("%d slots still held", n)
			}
		})
	}

	t.Run("both whole-suite steps of the gate are locked", func(t *testing.T) {
		t.Parallel()
		g := lockTree(t)
		_, status, calls := g.run(t, "--pgtest-lock")
		if status != 0 {
			t.Fatalf("exited %d", status)
		}
		var locked []string
		for _, c := range calls {
			if strings.HasPrefix(c, "aries ") {
				locked = append(locked, c)
			}
		}
		want := []string{
			lock + "ARIES_REQUIRE_POSTGRES=1 scripts/leftovers.sh go test -race -shuffle=on ./...",
			lock + "ARIES_REQUIRE_POSTGRES=1 ARIES_E2E_TRANSPORT=https scripts/leftovers.sh go test -race -shuffle=on ./internal/e2e/",
		}
		if !slices.Equal(locked, want) {
			t.Errorf("locked\n  %s\nwant\n  %s", strings.Join(locked, "\n  "), strings.Join(want, "\n  "))
		}
	})

	t.Run("a failing test releases the lock and fails the gate", func(t *testing.T) {
		t.Parallel()
		g := lockTree(t)
		g.write(t, "fail", "go test")
		out, status, _ := g.run(t, "--quick", "--pgtest-lock")
		if status != 1 || !strings.Contains(out, "it broke") {
			t.Errorf("exited %d, want 1 with the test's output:\n%s", status, out)
		}
		if n := g.slotsHeld(t); n != 0 {
			t.Errorf("%d slots still held after the failure", n)
		}
	})

	t.Run("a full lock is waited for, not failed", func(t *testing.T) {
		t.Parallel()
		g := lockTree(t)
		g.write(t, "block", "")
		var runs []*exec.Cmd
		for range 3 {
			runs = append(runs, g.start(t, "--quick", "--pgtest-lock"))
		}
		waitFor(t, "two runs to hold the lock", func() bool { return g.started(t) == 2 })
		time.Sleep(time.Second) // the third has had its chance
		if n, held := g.started(t), g.slotsHeld(t); n != 2 || held != 2 {
			t.Errorf("%d test runs started and %d slots held while two hold the lock, want 2 and 2", n, held)
		}
		if err := os.Remove(filepath.Join(g.root, "block")); err != nil {
			t.Fatal(err)
		}
		for i, c := range runs {
			if err := c.Wait(); err != nil {
				t.Errorf("run %d: %v", i, err)
			}
		}
		if n := g.slotsHeld(t); n != 0 {
			t.Errorf("%d slots still held at the end", n)
		}
	})

	t.Run("an interrupted gate releases the lock", func(t *testing.T) {
		t.Parallel()
		g := lockTree(t)
		g.write(t, "block", "")
		c := g.start(t, "--quick", "--pgtest-lock")
		waitFor(t, "the run to hold the lock", func() bool { return g.started(t) == 1 && g.slotsHeld(t) == 1 })
		if err := c.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		if err := c.Wait(); err == nil {
			t.Error("the terminated gate exited 0")
		}
		if n := g.slotsHeld(t); n != 0 {
			t.Errorf("%d slots still held after the interrupt", n)
		}
		_ = os.Remove(filepath.Join(g.root, "block"))
	})

	t.Run("asked for without aries on PATH", func(t *testing.T) {
		t.Parallel()
		g := newGateTree(t)                                  // no stand-in
		g.env = []string{"PATH=" + g.bin + ":/usr/bin:/bin"} // the system's tools and no aries
		out, status, _ := g.run(t, "--quick", "--pgtest-lock")
		if status != 2 || !strings.Contains(out, "aries is not installed") || !strings.Contains(out, "fix: ") {
			t.Errorf("exited %d, want 2 naming aries:\n%s", status, out)
		}
	})
}
