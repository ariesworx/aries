package server

import (
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// The database driver is the store's (decision 79, slice P0): the
// daemon reaches its databases through internal/store alone, so that
// changing the backend changes no package above it. This test asks the
// go tool, which `go test` puts on PATH, what the packages import.

const (
	module = "github.com/ariesworx/aries"
	driver = "github.com/go-sql-driver/mysql"
)

// TestOnlyTheStoreImportsTheDriver fails if the daemon's packages import
// the database driver themselves, on any system they are built for.
// Their tests may: they reach past the store to stage what the store
// would refuse.
func TestOnlyTheStoreImportsTheDriver(t *testing.T) {
	t.Parallel()
	guarded := []string{module + "/internal/server", module + "/cmd/ariesd"}
	// The store does import it, which shows the test reads what it
	// should.
	witness := module + "/internal/store"
	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			t.Parallel()
			imports := listImports(t, goos, append(guarded, witness)...)
			for _, p := range guarded {
				got, ok := imports[p]
				if !ok {
					t.Fatalf("go list did not list %s", p)
				}
				if slices.Contains(got, driver) {
					t.Errorf("%s imports %s (GOOS=%s): only internal/store may; add what it needs to the store", p, driver, goos)
				}
			}
			if !slices.Contains(imports[witness], driver) {
				t.Errorf("%s does not import %s (GOOS=%s): change driver to the store's", witness, driver, goos)
			}
		})
	}
}

// listImports runs `go list` at the module's root for goos, and returns
// each package's imports, its tests' left out.
func listImports(t *testing.T, goos string, pkgs ...string) map[string][]string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "go", append([]string{"list", "-json=ImportPath,Imports"}, pkgs...)...) //nolint:gosec // fixed arguments
	cmd.Dir = "../.."
	cmd.Env = append(cmd.Environ(), "GOOS="+goos, "GOARCH=amd64", "CGO_ENABLED=0")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list (GOOS=%s): %v\n%s", goos, err, stderr.String())
	}
	imports := map[string][]string{}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for {
		var p struct {
			ImportPath string
			Imports    []string
		}
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("go list: decode: %v", err)
		}
		imports[p.ImportPath] = p.Imports
	}
	return imports
}
