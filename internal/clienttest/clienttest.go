// Package clienttest keeps a test binary that starts the Aries client
// away from the keys and the outbox of whoever runs it, and one that
// starts git away from their git configuration.
//
// With no key configured, the client offers the default identity files
// of the home directory's .ssh and then ssh-agent's keys. A test that
// reached either would pass on a machine that happens to have keys
// loaded and fail on one that has none, and would offer a person's real
// keys to a test server. And a write that does not reach its server is
// kept in an outbox under the user's data directory (internal/offline),
// where a test's writes would wait beside a person's own. So every
// package whose tests dial calls [Isolate] from TestMain, once, before
// any test runs.
//
// Both functions set the environment of the process, in TestMain, and
// not of one test: a test that sets it (t.Setenv) cannot run beside
// another.
package clienttest

import (
	"fmt"
	"net"
	"os"
	"testing"
)

// Isolate gives the process an empty home directory of its own and no
// ssh-agent, and clears the variables that name a key, until the
// returned func is called. A test that wants an agent or a home with
// keys in it makes its own and passes it to the client it starts
// (client.Options.Getenv and UserHomeDir, or cli.Env's).
//
// It panics when it cannot make the directory: it runs in TestMain,
// where there is no test to fail.
func Isolate() (restore func()) {
	home, err := os.MkdirTemp("", "aries-test-home-")
	if err != nil {
		panic(fmt.Sprintf("clienttest: %v", err))
	}
	data, err := os.MkdirTemp("", "aries-test-data-")
	if err != nil {
		panic(fmt.Sprintf("clienttest: %v", err))
	}
	// HOME is what os.UserHomeDir reads on Unix, and USERPROFILE on
	// Windows. XDG_DATA_HOME would name a person's data directory to a
	// test that hands aries an environment without ARIES_DATA_DIR and
	// the process's own home.
	unset := setenv(map[string]string{"HOME": home, "USERPROFILE": home, "SSH_AUTH_SOCK": "", "ARIES_KEY_FILE": "", "ARIES_KEY": "",
		"ARIES_DATA_DIR": data, "ARIES_NO_QUEUE": "", "XDG_DATA_HOME": ""})
	return func() {
		unset()
		_ = os.RemoveAll(home)
		_ = os.RemoveAll(data)
	}
}

// IsolateGit keeps the git that the process's tests start from every
// configuration but a repository's own, until the returned func is
// called: the one in the home directory of whoever runs the tests, which
// may sign commits, rewrite URLs or name another default branch, and the
// machine's. A package whose tests make repositories calls it from
// TestMain.
func IsolateGit() (restore func()) {
	return setenv(map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1"})
}

// setenv sets each variable of set in the process's environment, or
// unsets it where the value is empty, and returns a func that puts back
// what was there.
func setenv(set map[string]string) (restore func()) {
	old := map[string]*string{}
	for k, v := range set {
		if cur, ok := os.LookupEnv(k); ok {
			old[k] = &cur
		} else {
			old[k] = nil
		}
		if v == "" {
			_ = os.Unsetenv(k)
		} else {
			_ = os.Setenv(k, v)
		}
	}
	return func() {
		for k, v := range old {
			if v == nil {
				_ = os.Unsetenv(k)
			} else {
				_ = os.Setenv(k, *v)
			}
		}
	}
}

// SilentPort returns the address of a loopback listener that accepts
// connections into its backlog and never reads or answers, until t ends.
//
// Close it before the test checks for leaked goroutines, which a cleanup
// registered after that check does: an exporter's HTTP transport keeps a
// connection it dialed after its request gave up, in the idle pool for
// 90 seconds, and its two goroutines with it. Closing the listener
// resets the connections waiting in its backlog, which ends them.
func SilentPort(t testing.TB) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("clienttest: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l.Addr().String()
}
