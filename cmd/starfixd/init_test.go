package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// What every starfixd process allocates before main runs, at most. sshd
// starts a `starfixd stdio` for each connection, and what initialization
// allocates is most of that bridge's own memory
// (docs/design/profile-starfixd.md, proposal 2): starfix's own packages
// allocated 1.5 MB of 2.2 MB, nearly all of it regular expressions
// compiled into package variables, before they were compiled on first use
// (internal/lazyregexp). The rest is the dependencies'.
const (
	maxOwnInitBytes = 32 << 10
	maxInitBytes    = 1 << 20
)

// TestInitAllocations runs the starfixd binary, built as a release is,
// with the runtime's trace of package initialization, and holds what the
// packages allocate to the bounds above. Allocations are counted, not
// memory measured: they are the same on every run and every machine, and
// they are what the change controls.
func TestInitAllocations(t *testing.T) {
	t.Parallel()
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go is not on PATH: the test builds starfixd")
	}
	exe := filepath.Join(t.TempDir(), "starfixd")
	build := exec.CommandContext(t.Context(), gobin, "build", "-trimpath", "-ldflags", "-s -w", "-o", exe, ".") //nolint:gosec // the test's own package
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	// The socket is never reached: initialization is over before stdio
	// runs, and its refusal is beside the point.
	run := exec.CommandContext(t.Context(), exe, "stdio", "--socket", filepath.Join(t.TempDir(), "none"), "--principal", "alice") //nolint:gosec // the binary just built
	run.Env = append(os.Environ(), "GODEBUG=inittrace=1")
	var trace bytes.Buffer
	run.Stderr = &trace
	_ = run.Run()

	own, all, pkgs := initBytes(t, trace.String())
	if pkgs == 0 {
		t.Fatalf("starfixd printed no init trace lines:\n%s", trace.String())
	}
	if total := sum(own); total > maxOwnInitBytes {
		t.Errorf("starfix's packages allocate %d bytes as starfixd initializes, want at most %d; by package: %v\n"+
			"Compile a package-level regular expression on first use (internal/lazyregexp), and build tables when first needed",
			total, maxOwnInitBytes, own)
	}
	if all > maxInitBytes {
		t.Errorf("starfixd's %d packages allocate %d bytes as it initializes, want at most %d: every `starfixd stdio`, one for each connection, holds them",
			pkgs, all, maxInitBytes)
	}
}

// initBytes reads GODEBUG=inittrace=1 output: the bytes each of
// starfix's own packages allocated, the bytes all packages did, and how
// many packages it read. A line is
//
//	init PACKAGE @T ms, T ms clock, BYTES bytes, ALLOCS allocs
func initBytes(t *testing.T, trace string) (own map[string]int, all, pkgs int) {
	t.Helper()
	own = map[string]int{}
	for line := range strings.Lines(trace) {
		f := strings.Fields(line)
		if len(f) < 9 || f[0] != "init" || f[len(f)-3] != "bytes," {
			continue
		}
		n, err := strconv.Atoi(f[len(f)-4])
		if err != nil {
			t.Fatalf("init trace line %q: %v", line, err)
		}
		pkgs++
		all += n
		if strings.HasPrefix(f[1], "github.com/ariesworx/starfix/") && n > 0 {
			own[f[1]] = n
		}
	}
	return own, all, pkgs
}

func sum(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}
