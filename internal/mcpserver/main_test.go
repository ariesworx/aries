package mcpserver

import (
	"os"
	"testing"

	"github.com/ariesworx/starfix/internal/clienttest"
)

// TestMain keeps every test here away from the real home directory and
// ssh-agent, and the git the tests start from the user's git
// configuration (clienttest).
func TestMain(m *testing.M) {
	restore := clienttest.Isolate()
	restoreGit := clienttest.IsolateGit()
	code := m.Run()
	restoreGit()
	restore()
	os.Exit(code)
}
