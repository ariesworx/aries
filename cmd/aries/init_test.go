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

// What every aries process allocates before main runs, at most. Every
// command pays it, hooks included, and a developer's sessions run
// thousands of them a day (docs/design/profile-aries.md, proposal 2):
// Aries's own packages allocated 446 KB of 711 KB, nearly all of it
// regular expressions compiled into package variables, before they were
// compiled on first use (internal/lazyregexp). The rest is the
// dependencies'.
const (
	maxOwnInitBytes = 32 << 10
	maxInitBytes    = 1 << 20
)

// TestInitAllocations runs the aries binary, built as a release is, with
// the runtime's trace of package initialization, and holds what the
// packages allocate to the bounds above. Allocations are counted, not
// memory measured: they are the same on every run and every machine, and
// they are what the change controls. cmd/ariesd's test of the same name
// does this for ariesd.
func TestInitAllocations(t *testing.T) {
	t.Parallel()
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go is not on PATH: the test builds aries")
	}
	exe := filepath.Join(t.TempDir(), "aries")
	build := exec.CommandContext(t.Context(), gobin, "build", "-trimpath", "-ldflags", "-s -w", "-o", exe, ".") //nolint:gosec // the test's own package
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	// --version reads no file and connects to nothing: what it allocates
	// is initialization and a line of output.
	run := exec.CommandContext(t.Context(), exe, "--version") //nolint:gosec // the binary just built
	run.Env = []string{"GODEBUG=inittrace=1", "HOME=" + t.TempDir()}
	var trace bytes.Buffer
	run.Stderr = &trace
	if err := run.Run(); err != nil {
		t.Fatalf("aries --version: %v\n%s", err, trace.String())
	}

	own, all, pkgs := initBytes(t, trace.String())
	if pkgs == 0 {
		t.Fatalf("aries printed no init trace lines:\n%s", trace.String())
	}
	if total := sum(own); total > maxOwnInitBytes {
		t.Errorf("aries's packages allocate %d bytes as aries initializes, want at most %d; by package: %v\n"+
			"Compile a package-level regular expression on first use (internal/lazyregexp), and build tables when first needed",
			total, maxOwnInitBytes, own)
	}
	if all > maxInitBytes {
		t.Errorf("aries's %d packages allocate %d bytes as it initializes, want at most %d: every command and hook pays them",
			pkgs, all, maxInitBytes)
	}
}

// initBytes reads GODEBUG=inittrace=1 output: the bytes each of
// Aries's own packages allocated, the bytes all packages did, and how
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
		if strings.HasPrefix(f[1], "github.com/ariesworx/aries/") && n > 0 {
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
