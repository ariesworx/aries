//go:build unix

package dolttest

import (
	"fmt"
	"os"
	"time"
)

// endWithParent ends this process, after calling stop, once the process
// that started it is gone. It returns at once, and looks every second.
//
// It is for a test binary, whose parent is go test. When go test is
// killed alone (a harness that stops an agent, a kill of the one process
// id), its test binaries are no one's children and no one's to stop: they
// ran on to the end of their tests, for minutes, with their Dolt servers.
// A process whose parent is gone is given another, so the parent's
// process id changes, which is the sign.
func endWithParent(stop func()) {
	parent := os.Getppid()
	go func() {
		for range time.Tick(time.Second) {
			if os.Getppid() == parent {
				continue
			}
			stop()
			// Nothing may be reading: the parent held the other end.
			fmt.Fprintln(os.Stderr, "dolttest: the process that started this one is gone (go test was killed?): stopped the Dolt servers, and ending")
			os.Exit(1)
		}
	}()
}
