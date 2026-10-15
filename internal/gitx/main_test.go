package gitx

import (
	"os"
	"testing"

	"github.com/ariesworx/aries/internal/clienttest"
)

// TestMain keeps the git these tests start from the configuration of
// whoever runs them (clienttest), for the whole process: a test that set
// the environment itself could not run beside another.
func TestMain(m *testing.M) {
	restore := clienttest.IsolateGit()
	code := m.Run()
	restore()
	os.Exit(code)
}
