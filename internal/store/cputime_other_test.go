//go:build !linux && !darwin

package store

import (
	"testing"
	"time"
)

// onProcessor returns the clock, where the processor time of a thread is
// not read: a test that compares how long work took then counts what the
// machine gave to something else, as it did everywhere before.
func onProcessor(*testing.T) func() time.Duration {
	start := time.Now()
	return func() time.Duration { return time.Since(start) }
}
