//go:build linux || darwin

package store

import (
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// onProcessor returns a clock that reads how long the calling goroutine
// has had a processor, for a test that compares how long work took and
// must not count the time the machine gave to something else. The
// goroutine is kept on one thread until t ends, since the time read is
// the thread's.
//
// The process's time (getrusage) will not do: it counts the collector's
// threads too, and on Linux it is brought up to date only now and then,
// so a tenth of a millisecond of work has read as a hundredth.
func onProcessor(t *testing.T) func() time.Duration {
	t.Helper()
	runtime.LockOSThread()
	t.Cleanup(runtime.UnlockOSThread)
	return func() time.Duration {
		var ts unix.Timespec
		if err := unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &ts); err != nil {
			t.Fatalf("clock_gettime(CLOCK_THREAD_CPUTIME_ID): %v", err)
		}
		return time.Duration(ts.Nano())
	}
}
