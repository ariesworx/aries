package clienttest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsolate(t *testing.T) {
	t.Setenv("HOME", "/somebody/else")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/somebody.sock")
	t.Setenv("STARFIX_KEY_FILE", "/somebody/else/.ssh/id_ed25519")
	restore := Isolate()
	home, err := os.UserHomeDir()
	if err != nil || home == "/somebody/else" {
		t.Fatalf("os.UserHomeDir() under Isolate = %q, %v; want a directory of the test's own", home, err)
	}
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
		t.Errorf("the isolated home holds %v (%v), want it empty", entries, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".ssh")); !os.IsNotExist(err) {
		t.Errorf("the isolated home has a .ssh: %v", err)
	}
	for _, k := range []string{"SSH_AUTH_SOCK", "STARFIX_KEY_FILE", "STARFIX_KEY"} {
		if v, ok := os.LookupEnv(k); ok {
			t.Errorf("%s = %q under Isolate, want it unset", k, v)
		}
	}
	restore()
	if got := os.Getenv("HOME"); got != "/somebody/else" {
		t.Errorf("HOME after restore = %q, want what it was", got)
	}
	if got := os.Getenv("SSH_AUTH_SOCK"); got != "/tmp/somebody.sock" {
		t.Errorf("SSH_AUTH_SOCK after restore = %q, want what it was", got)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("the isolated home is still there after restore: %v", err)
	}
}
