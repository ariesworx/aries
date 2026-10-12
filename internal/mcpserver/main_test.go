package mcpserver

import (
	"os"
	"testing"

	"github.com/ariesworx/starfix/internal/clienttest"
)

// TestMain keeps every test here away from the real home directory and
// ssh-agent (clienttest).
func TestMain(m *testing.M) {
	restore := clienttest.Isolate()
	code := m.Run()
	restore()
	os.Exit(code)
}
