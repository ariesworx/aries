package oidc

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// readSecret checks the mode of the file it opened, so a symlink to a
// loose file is refused and one to a 0600 file is read.
func TestReadSecretChecksOpenFile(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not checked on Windows")
	}
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	loose := filepath.Join(dir, "loose")
	for p, mode := range map[string]os.FileMode{good: 0o600, loose: 0o644} {
		if err := os.WriteFile(p, []byte("s3\nmore\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil { //nolint:gosec // the mode under test
			t.Fatal(err)
		}
	}
	for _, p := range []string{good, filepath.Join(dir, "linkgood")} {
		if p != good {
			if err := os.Symlink(good, p); err != nil {
				t.Fatal(err)
			}
		}
		if got, err := readSecret(p); err != nil || got != "s3" {
			t.Errorf("readSecret(%s) = %q, %v; want s3", p, got, err)
		}
	}
	link := filepath.Join(dir, "linkloose")
	if err := os.Symlink(loose, link); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{loose, link} {
		if _, err := readSecret(p); err == nil || !strings.Contains(err.Error(), "chmod 600") {
			t.Errorf("readSecret(%s) = %v, want a refusal with the fix chmod 600", p, err)
		}
	}
}

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
