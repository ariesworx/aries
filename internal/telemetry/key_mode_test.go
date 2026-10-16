package telemetry

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A stand-in key that group or others can read is a damaged key whose
// fixes include chmod 600, in each mode that leaves a bit open to them.
func TestReadKeyRefusesLooseModes(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not checked on Windows")
	}
	const key = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	for _, mode := range []os.FileMode{0o640, 0o604} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "principals.key")
			if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil { //nolint:gosec // the mode under test
				t.Fatal(err)
			}
			_, prob := readKey(path)
			if prob == nil || !prob.damaged || !strings.Contains(strings.Join(prob.fixes, " "), "chmod 600") {
				t.Errorf("readKey with a key of mode %04o = %+v, want a damaged key with the fix chmod 600", mode, prob)
			}
		})
	}
}
