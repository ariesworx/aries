package scripts_test

import (
	"os"
	"testing"

	"github.com/ariesworx/aries/internal/clienttest"
)

// TestMain keeps every test here away from the real home directory and
// ssh-agent (clienttest): the operations skill's test runs sfx's help
// in process, and sfx is a client that could sign in.
func TestMain(m *testing.M) {
	restore := clienttest.Isolate()
	code := m.Run()
	restore()
	os.Exit(code)
}
