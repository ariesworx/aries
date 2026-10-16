package client

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A linked worktree (git worktree add) is its own top level, so a
// config kept untracked in the main worktree is found from it, read
// there and never above it; the project's files still go in the
// worktree (Checkout).
func TestLoadConfigWorktree(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	body := "project: " + testProject + "\nserver:\n  host: aries.example.com\n  host_key: " + testFpr + "\n"
	tests := []struct {
		name string
		// repo is how the repository is made: "main" for a main
		// worktree at main with a linked one at wt, "bare" for a bare
		// repository at bare/.git with a linked worktree at wt, ""
		// for no git at all.
		repo string
		// dirs are made and configs written, relative to a temp top,
		// after the repository; run is where the search starts.
		dirs, configs  []string
		run            string
		root, checkout string // root "" means not found
	}{
		{name: "from the main worktree", repo: "main", configs: []string{"main"}, run: "wt", root: "main", checkout: "wt"},
		{name: "the worktree's own wins", repo: "main", configs: []string{"main", "wt"}, run: "wt", root: "wt", checkout: "wt"},
		{name: "one between wins", repo: "main", dirs: []string{"wt/a/b"}, configs: []string{"main", "wt/a"}, run: "wt/a/b", root: "wt/a", checkout: "wt/a"},
		{name: "nested in the worktree", repo: "main", dirs: []string{"wt/a/b"}, configs: []string{"main"}, run: "wt/a/b", root: "main", checkout: "wt"},
		{name: "main worktree has none", repo: "main", run: "wt"},
		{name: "only the main worktree's top", repo: "main", dirs: []string{"main/sub"}, configs: []string{"main/sub"}, run: "wt"},
		{name: "never above the main worktree", repo: "main", configs: []string{"."}, run: "wt"},
		{name: "a bare repository has no main worktree", repo: "bare", configs: []string{"bare"}, run: "wt"},
		{name: "no git", dirs: []string{"plain/a"}, configs: []string{"plain"}, run: "plain/a", root: "plain", checkout: "plain"},
		{name: "no git, none", dirs: []string{"plain/a"}, run: "plain/a"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			top, err := filepath.EvalSymlinks(t.TempDir()) // git records resolved paths
			if err != nil {
				t.Fatal(err)
			}
			switch tc.repo {
			case "main":
				main := filepath.Join(top, "main")
				gitIn(t, top, "init", "-q", "-b", "main", main)
				gitIn(t, main, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "--allow-empty", "-m", "init")
				gitIn(t, main, "worktree", "add", "-q", "-b", "wt", filepath.Join(top, "wt"))
			case "bare":
				src := filepath.Join(top, "src")
				gitIn(t, top, "init", "-q", "-b", "main", src)
				gitIn(t, src, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "--allow-empty", "-m", "init")
				bare := filepath.Join(top, "bare", ".git")
				gitIn(t, top, "clone", "-q", "--bare", src, bare)
				gitIn(t, bare, "worktree", "add", "-q", "-b", "wt", filepath.Join(top, "wt"))
			}
			for _, d := range tc.dirs {
				if err := os.MkdirAll(filepath.Join(top, d), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for _, c := range tc.configs {
				if err := os.WriteFile(filepath.Join(top, c, ConfigFile), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			c, err := loadConfig(filepath.Join(top, tc.run), filepath.Join(top, "home"))
			if tc.root == "" {
				if !errors.Is(err, ErrNoConfig) {
					t.Fatalf("loadConfig(%s) = %+v, %v; want not found", tc.run, c, err)
				}
				return
			}
			if err != nil || c.Root != filepath.Join(top, tc.root) || c.Checkout != filepath.Join(top, tc.checkout) {
				t.Fatalf("loadConfig(%s) = %+v, %v; want root %s, checkout %s", tc.run, c, err, tc.root, tc.checkout)
			}
		})
	}
}

// gitIn runs git in dir, failing the test if it fails.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // test fixture
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
