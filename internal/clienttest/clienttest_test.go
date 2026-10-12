package clienttest

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryConnectingPackageIsIsolated reads the module's tests: a
// package whose tests can make sfx choose a key (they import the client,
// or something that connects with it) must call Isolate, which is what
// keeps them from the developer's ~/.ssh and ssh-agent.
func TestEveryConnectingPackageIsIsolated(t *testing.T) {
	const module = `"github.com/ariesworx/starfix/internal/`
	connects := []string{module + `client"`, module + `cli"`, module + `mcpserver"`, module + `board"`}
	needs, has := map[string]string{}, map[string]bool{}
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "spike" || name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path) //nolint:gosec // this module's own tests
		if err != nil {
			return err
		}
		src, dir := string(b), filepath.Dir(path)
		// A package's own tests count too: client, cli, mcpserver, board.
		own := `"github.com/ariesworx/starfix/` + filepath.ToSlash(strings.TrimPrefix(dir, root+string(filepath.Separator))) + `"`
		for _, imp := range connects {
			if strings.Contains(src, imp) || own == imp {
				needs[dir] = path
			}
		}
		if strings.Contains(src, "func TestMain(") && strings.Contains(src, "clienttest.Isolate()") {
			has[dir] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(needs) < 5 {
		t.Fatalf("found %d packages whose tests connect (%v), want at least client, cli, mcpserver, board and e2e: the search is broken", len(needs), needs)
	}
	for dir, file := range needs {
		if !has[dir] {
			t.Errorf("%s has tests that can sign in (%s) and no TestMain that calls clienttest.Isolate: they could offer the developer's own keys", dir, filepath.Base(file))
		}
	}
}

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
