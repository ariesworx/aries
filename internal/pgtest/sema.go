package pgtest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// semSet is a System V semaphore set: its key and its id.
type semSet struct {
	key int32
	id  int
}

// semaphoresFile names the file in a server's directory that lists its
// semaphore sets, "ID KEY" on each line with the key as ipcs prints it,
// for the watcher: it cannot tell a set's owner itself, and frees those
// whose id still has the key.
const semaphoresFile = "semaphores"

// recordSemaphores writes the semaphore sets of the server, whose
// postmaster is pid, to its directory for the watcher, or removes the
// file if it has none.
func (s *Server) recordSemaphores(pid int) error {
	path := filepath.Join(s.dir, semaphoresFile)
	sets := semaphoreSets(s.data(), pid)
	if len(sets) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("pgtest: %w", err)
		}
		return nil
	}
	var b strings.Builder
	for _, set := range sets {
		fmt.Fprintf(&b, "%d 0x%08x\n", set.id, uint32(set.key)) //nolint:gosec // the key's bits, as ipcs prints them
	}
	// A line cut short names no set the watcher would find.
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("pgtest: %w", err)
	}
	return nil
}
