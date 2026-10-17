package client

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
)

// A private key file that group or others may read is not signed with,
// as OpenSSH refuses it (aries-rpp7gz6g): named by ARIES_KEY_FILE it is
// refused, with the fix chmod 600; as a default identity it is skipped,
// saying why, and the other keys are offered. A file of the public half
// alone is not checked, nor is a key of mode 0600.
func TestLooseKeyFileRefused(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not checked on Windows")
	}
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604} {
		t.Run(fmt.Sprintf("configured %04o", mode), func(t *testing.T) {
			t.Parallel()
			k := newKeyPair(t)
			path := k.write(t, t.TempDir(), "id_ed25519", "", false)
			if err := os.Chmod(path, mode); err != nil { //nolint:gosec // the mode under test
				t.Fatal(err)
			}
			env := map[string]string{EnvKeyFile: path}
			id, err := loadIdentity(&Config{Root: t.TempDir()}, func(k string) string { return env[k] }, func() (string, error) { return t.TempDir(), nil })
			if err == nil {
				id.close()
				t.Fatalf("a key file of mode %04o was used", mode)
			}
			pe, ok := errors.AsType[*proto.Error](err)
			if !ok || pe.Code != proto.CodeAuth || !strings.Contains(pe.Fix, "chmod 600 "+path) || !strings.Contains(pe.Message, "other users") {
				t.Errorf("loadIdentity with a key of mode %04o = %v, want an auth refusal saying others can read it, with the fix chmod 600 %s", mode, err, path)
			}
			if strings.Contains(pe.Message+pe.Fix, "PRIVATE KEY") {
				t.Errorf("the refusal shows the key: %v", err)
			}
		})
	}

	t.Run("configured 0600", func(t *testing.T) {
		t.Parallel()
		k := newKeyPair(t)
		env := map[string]string{EnvKeyFile: k.write(t, t.TempDir(), "id_ed25519", "", false)}
		id, err := loadIdentity(&Config{Root: t.TempDir()}, func(k string) string { return env[k] }, func() (string, error) { return t.TempDir(), nil })
		if err != nil {
			t.Fatalf("a key of mode 0600: %v", err)
		}
		id.close()
	})

	t.Run("a public half others can read", func(t *testing.T) {
		t.Parallel()
		k := newKeyPair(t)
		pub := k.writePub(t, t.TempDir(), "id_ed25519.pub")
		if err := os.Chmod(pub, 0o644); err != nil { //nolint:gosec // a public key
			t.Fatal(err)
		}
		env := map[string]string{EnvKeyFile: pub, "SSH_AUTH_SOCK": agentWith(t, k)}
		id, err := loadIdentity(&Config{Root: t.TempDir()}, func(k string) string { return env[k] }, func() (string, error) { return t.TempDir(), nil })
		if err != nil {
			t.Fatalf("a .pub of mode 0644 whose key ssh-agent holds: %v", err)
		}
		id.close()
	})

	t.Run("a default identity", func(t *testing.T) {
		t.Parallel()
		loose, kept := newKeyPair(t), newKeyPair(t)
		home := t.TempDir()
		dir := sshDir(t, home)
		path := loose.write(t, dir, "id_ed25519", "", false)
		if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // the mode under test
			t.Fatal(err)
		}
		kept.write(t, dir, "id_ecdsa", "", false)
		id, err := loadIdentity(&Config{Root: t.TempDir()}, func(string) string { return "" }, func() (string, error) { return home, nil })
		if err != nil {
			t.Fatal(err)
		}
		defer id.close()
		var offered []string
		for _, k := range id.keys {
			offered = append(offered, k.Fingerprint)
		}
		if !slices.Equal(offered, []string{kept.fingerprint()}) {
			t.Errorf("offers %v, want only the key of mode 0600, %s", offered, kept.fingerprint())
		}
		if len(id.skipped) != 1 || !strings.Contains(id.skipped[0], path) || !strings.Contains(id.skipped[0], "chmod 600") {
			t.Errorf("skipped %q, want %s skipped, saying chmod 600", id.skipped, path)
		}
	})
}
