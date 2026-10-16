//go:build !darwin

package pgtest

// Postgres on Linux takes its semaphores from the shared memory it maps,
// not from System V, so a server leaves none behind and there are none
// to find.

func semaphoreSets(string, int) []semSet { return nil }

func semaphoreSetExists(semSet) bool { return false }

func freeSemaphores(string, int) {}
