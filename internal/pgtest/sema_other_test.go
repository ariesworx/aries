//go:build unix && !darwin

package pgtest

import "testing"

// semSet is a System V semaphore set. Postgres on Linux takes its
// semaphores from the shared memory it maps, not from System V, so
// there are none to find.
type semSet struct{ key, id int }

func semaphoreSets(*testing.T, string, int) []semSet { return nil }

func semaphoreSetExists(semSet) bool { return false }
