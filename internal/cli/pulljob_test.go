package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=T", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput() //nolint:gosec // test fixture
	if err != nil {
		t.Fatalf("git %s: %v\n%s", args[0], err, out)
	}
	return strings.TrimSpace(string(out))
}

// What a run of the job does to the checkout, whether or not the server
// answers: with none (this repository has no .aries.yaml) each run ends
// in its land failing, which the job logs, and the git side is what is
// held here: it pulls only on the default branch, with no change to a
// tracked file, and as a fast-forward.
func TestPullJobRunGit(t *testing.T) {
	t.Parallel()
	origin, repo, data := gitRepo(t), gitRepo(t), t.TempDir()
	if err := os.WriteFile(filepath.Join(origin, "f.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, origin, "add", "f.txt")
	gitTest(t, origin, "commit", "-q", "-m", "init")
	gitTest(t, repo, "remote", "add", "origin", origin)
	gitTest(t, repo, "fetch", "-q", "origin")
	gitTest(t, repo, "reset", "-q", "--hard", "origin/main")

	run := func() string {
		t.Helper()
		var out, errb bytes.Buffer
		Run(t.Context(), []string{"-C", repo, "pull-job", "--run"}, Env{Stdout: &out, Stderr: &errb, Version: "v0.3.0",
			Getenv:      func(k string) string { return map[string]string{"ARIES_DATA_DIR": data, "CI": "true"}[k] },
			UserHomeDir: func() (string, error) { return t.TempDir(), nil }})
		return out.String() + errb.String()
	}
	head := func() string { return gitTest(t, repo, "rev-parse", "HEAD") }

	first := head()
	second := commitIn(t, origin, "second")
	if run(); head() != second {
		t.Fatalf("a clean checkout on main did not fast-forward: HEAD %s, want %s", head(), first)
	}

	third := commitIn(t, origin, "third")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if run(); head() != second {
		t.Errorf("a dirty checkout was pulled: HEAD %s, want %s", head(), second)
	}
	gitTest(t, repo, "checkout", "-q", "f.txt")

	own := commitIn(t, repo, "mine")
	if run(); head() != own {
		t.Errorf("a diverged checkout was changed: HEAD %s, want %s", head(), own)
	}
	gitTest(t, repo, "reset", "-q", "--hard", second)

	gitTest(t, repo, "switch", "-q", "-c", "feature/x")
	if out := run(); head() != second || !strings.Contains(out, "skipped: the checkout is on feature/x, not main") {
		t.Errorf("off the default branch: HEAD %s, output %q; want nothing done", head(), out)
	}
	gitTest(t, repo, "switch", "-q", "main")
	if run(); head() != third {
		t.Errorf("back on main it did not pull: HEAD %s, want %s", head(), third)
	}

	log, err := os.ReadFile(filepath.Join(data, pullJobLog)) //nolint:gosec // the test's data directory
	if err != nil || strings.Count(string(log), "\n") != 5 {
		t.Errorf("%s = %q, %v; want one line for each of 5 runs", pullJobLog, log, err)
	}
}

func commitIn(t *testing.T, dir, msg string) string {
	t.Helper()
	gitTest(t, dir, "commit", "-q", "--allow-empty", "-m", msg)
	return gitTest(t, dir, "rev-parse", "HEAD")
}
