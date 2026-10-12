// Package clienttest keeps a test binary that starts the starfix client
// away from the keys of whoever runs it.
//
// With no key configured, the client offers the default identity files
// of the home directory's .ssh and then ssh-agent's keys. A test that
// reached either would pass on a machine that happens to have keys
// loaded and fail on one that has none, and would offer a person's real
// keys to a test server. So every package whose tests dial calls
// [Isolate] from TestMain, once, before any test runs.
package clienttest

import (
	"fmt"
	"os"
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
	home, err := os.MkdirTemp("", "starfix-test-home-")
	if err != nil {
		panic(fmt.Sprintf("clienttest: %v", err))
	}
	// HOME is what os.UserHomeDir reads on Unix, and USERPROFILE on
	// Windows.
	set := map[string]string{"HOME": home, "USERPROFILE": home, "SSH_AUTH_SOCK": "", "STARFIX_KEY_FILE": "", "STARFIX_KEY": ""}
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
		_ = os.RemoveAll(home)
	}
}
