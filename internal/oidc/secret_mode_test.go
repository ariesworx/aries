package oidc

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A client secret that group or others can read is refused at start,
// with the fix chmod 600, in each mode that leaves a bit open to them.
func TestOpenRefusesLooseSecretFile(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not checked on Windows")
	}
	for _, mode := range []os.FileMode{0o640, 0o604} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "secret")
			if err := os.WriteFile(path, []byte("s\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil { //nolint:gosec // the mode under test
				t.Fatal(err)
			}
			s := Settings{Name: "corp", Kind: KindOIDC, Issuer: "https://idp.example.com/realms/example", ClientID: "aries", ClientSecretFile: path}
			if _, err := Open(s, Options{}); err == nil || !strings.Contains(err.Error(), "chmod 600") {
				t.Errorf("Open with a secret of mode %04o = %v, want a refusal with the fix chmod 600", mode, err)
			}
		})
	}
}
