package server

import (
	"go/build"
	"slices"
	"strings"
	"testing"
)

// The database driver is the store's (decision 79, slice P0): the
// daemon reaches its databases through internal/store alone, so that
// changing the backend changes no package above it.

// driver is the module of the database driver: any of its packages.
const driver = "github.com/jackc/pgx/v5"

// importsDriver reports whether imports holds a package of driver.
func importsDriver(imports []string) bool {
	return slices.ContainsFunc(imports, func(p string) bool { return p == driver || strings.HasPrefix(p, driver+"/") })
}

// TestOnlyTheStoreImportsTheDriver fails if the daemon's packages import
// the database driver themselves, on any system they are built for.
// Their tests may: they reach past the store to stage what the store
// would refuse. It reads the packages' files with go/build, which runs
// no go command and so fetches no module.
func TestOnlyTheStoreImportsTheDriver(t *testing.T) {
	t.Parallel()
	guarded := []string{".", "../../cmd/ariesd"}
	// The store does import it, which shows the test reads what it
	// should.
	witness := "../store"
	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			t.Parallel()
			ctx := build.Default
			ctx.GOOS, ctx.GOARCH, ctx.CgoEnabled = goos, "amd64", false
			imports := func(dir string) []string {
				t.Helper()
				p, err := ctx.ImportDir(dir, 0)
				if err != nil {
					t.Fatalf("read the package in %s (GOOS=%s): %v", dir, goos, err)
				}
				return p.Imports
			}
			for _, dir := range guarded {
				if importsDriver(imports(dir)) {
					t.Errorf("the package in %s imports %s (GOOS=%s): only internal/store may; add what it needs to the store", dir, driver, goos)
				}
			}
			if !importsDriver(imports(witness)) {
				t.Errorf("internal/store does not import %s (GOOS=%s): change driver to the store's", driver, goos)
			}
		})
	}
}
