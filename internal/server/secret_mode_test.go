package server

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A TLS key that group or others can read is refused at start, with
// the fix chmod 600, in each mode that leaves a bit open to them.
func TestTLSKeyRefusesLooseModes(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not checked on Windows")
	}
	for _, mode := range []os.FileMode{0o640, 0o604} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			cert, key := writeCert(t, dir, "aries.example.com", time.Now().Add(time.Hour))
			if err := os.Chmod(key, mode); err != nil { //nolint:gosec // the mode under test
				t.Fatal(err)
			}
			c := &certFiles{cert: cert, key: key}
			if _, _, err := c.readPair(); err == nil || !strings.Contains(err.Error(), "chmod 600") {
				t.Errorf("readPair with a key of mode %04o = %v, want a refusal with the fix chmod 600", mode, err)
			}
		})
	}
}

// A config file whose DSN holds a password is refused when group or
// others can read it, with the fix chmod 600, in each loose mode.
func TestConfigPasswordRefusesLooseModes(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not checked on Windows")
	}
	for _, mode := range []os.FileMode{0o640, 0o604} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "ariesd.yaml")
			if err := os.WriteFile(path, []byte("dsn: postgres://aries:pw@127.0.0.1:5432/aries\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil { //nolint:gosec // the mode under test
				t.Fatal(err)
			}
			if _, err := loadSettings(path, true); err == nil || !strings.Contains(err.Error(), "chmod 600") {
				t.Errorf("loadSettings with a password in mode %04o = %v, want a refusal with the fix chmod 600", mode, err)
			}
		})
	}
}
