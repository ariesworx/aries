package dispatch

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ariesworx/aries/internal/version"
)

// NoPostgresNote is the note a brief carries when the dispatcher found no
// Postgres for the run: the run starts anyway, since most issues need
// none, and is told what it cannot prove.
const NoPostgresNote = "The dispatcher found no Postgres " + version.PostgresMajor + " on this machine, so none is on the run's PATH: " +
	"a test that needs Postgres fails here, so the store, server and e2e tests cannot run."

// FindPostgres returns the directory of the pinned Postgres's programs, as
// scripts/install-postgres.sh would print it, or "" when there is none:
// the packaged one on Linux (ARIES_POSTGRES_PACKAGED or
// /usr/lib/postgresql, then MAJOR/bin), else the one built into the cache
// directory (ARIES_POSTGRES_DIR, or $XDG_CACHE_HOME/aries, or
// ~/.cache/aries, then postgres-VERSION/bin). It builds and downloads
// nothing; environ is the process's environment.
func FindPostgres(environ []string) string {
	get := func(k string) string {
		for _, kv := range environ {
			if v, ok := strings.CutPrefix(kv, k+"="); ok {
				return v
			}
		}
		return ""
	}
	var dirs []string
	if runtime.GOOS == "linux" {
		root := get("ARIES_POSTGRES_PACKAGED")
		if root == "" {
			root = "/usr/lib/postgresql"
		}
		dirs = append(dirs, filepath.Join(root, version.PostgresMajor, "bin"))
	}
	cache := get("ARIES_POSTGRES_DIR")
	if cache == "" {
		if x := get("XDG_CACHE_HOME"); x != "" {
			cache = filepath.Join(x, "aries")
		} else if h := get("HOME"); h != "" {
			cache = filepath.Join(h, ".cache", "aries")
		}
	}
	if cache != "" {
		dirs = append(dirs, filepath.Join(cache, "postgres-"+version.Postgres, "bin"))
	}
	for _, d := range dirs {
		if runnable(filepath.Join(d, "postgres")) && runnable(filepath.Join(d, "initdb")) {
			return d
		}
	}
	return ""
}

func runnable(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0
}
