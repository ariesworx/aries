package pgtest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A server that cannot get System V shared memory or semaphores says the
// machine is out of them, and what to do, instead of only that it exited.
func TestStartFailureNamesTheMachinesLimit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, log string
		wantIPC   bool
	}{
		{"shared memory", `2026-10-17 10:00:00.000 PDT [4242] FATAL:  could not create shared memory segment: No space left on device
2026-10-17 10:00:00.000 PDT [4242] DETAIL:  Failed system call was shmget(key=5432001, size=56, 03600).
2026-10-17 10:00:00.000 PDT [4242] HINT:  This error does *not* mean that you have run out of disk space.  It occurs when either the system limit for the maximum number of shared memory identifiers (SHMMNI) is exceeded.`, true},
		{"semaphores", `2026-10-17 10:00:00.000 PDT [4242] FATAL:  could not create semaphores: No space left on device
2026-10-17 10:00:00.000 PDT [4242] DETAIL:  Failed system call was semget(5432002, 17, 03600).`, true},
		{"initdb", "creating shared memory ... \ninitdb: error: ... FATAL:  could not create shared memory segment: No space left on device\nDETAIL:  Failed system call was shmget(", true},
		{"a disk that is full", "FATAL:  could not write to file \"pg_wal/xlogtemp.1\": No space left on device", false},
		{"another failure", "FATAL:  data directory \"x\" has invalid permissions", false},
		{"nothing", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "server.log")
			if err := os.WriteFile(path, []byte(tc.log), 0o600); err != nil {
				t.Fatal(err)
			}
			err := startFailure(errors.New("exit status 1"), path)
			if got := errors.Is(err, ErrOutOfIPC); got != tc.wantIPC {
				t.Fatalf("errors.Is(err, ErrOutOfIPC) = %t, want %t: %v", got, tc.wantIPC, err)
			}
			msg := err.Error()
			if tc.wantIPC {
				for _, want := range []string{"System V", "ipcs", "shmmni", "fewer"} {
					if !strings.Contains(msg, want) {
						t.Errorf("the error does not say %q:\n%s", want, msg)
					}
				}
			}
			if tc.log != "" && !strings.Contains(msg, strings.Split(tc.log, "\n")[0]) {
				t.Errorf("the error does not show what the server logged:\n%s", msg)
			}
		})
	}
	// A log that is not there is no reason to lose the exit.
	if err := startFailure(errors.New("exit status 1"), filepath.Join(t.TempDir(), "none.log")); err == nil || !strings.Contains(err.Error(), "exit status 1") {
		t.Errorf("startFailure with no log = %v, want the exit", err)
	}
}
