package board

import (
	"os"
	"testing"

	"github.com/ariesworx/aries/internal/clienttest"
)

// TestMain keeps every test here away from the real home directory and
// ssh-agent (clienttest).
func TestMain(m *testing.M) {
	restore := clienttest.Isolate()
	code := m.Run()
	restore()
	os.Exit(code)
}
