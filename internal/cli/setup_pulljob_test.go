package cli

import (
	"bytes"
	"context"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files")

// The job each system gets is a function of its spec: its files, and the
// commands that start it, are held to a golden file per system.
func TestPullJobGolden(t *testing.T) {
	t.Parallel()
	for _, goos := range []string{"darwin", "linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			t.Parallel()
			j := jobSpec{GOOS: goos, Exe: "/usr/local/bin/aries", Top: "/home/alice/code/proj", Branch: "main", Remote: "origin",
				KeyFile: "/home/alice/.ssh/id_work", Path: "/usr/local/bin:/usr/bin", Every: 15 * time.Minute, Home: "/home/alice"}
			j.Name = jobName(j.Top)
			var b strings.Builder
			for _, f := range j.files() {
				b.WriteString("=== " + f.Path + "\n" + f.Body)
			}
			if goos == "windows" {
				b.WriteString("=== task\n" + j.winCommand() + "\n")
			}
			b.WriteString("=== start\n")
			for _, c := range j.create() {
				b.WriteString(words(c) + "\n")
			}
			b.WriteString("=== stop\n")
			for _, c := range j.stop() {
				b.WriteString(words(c) + "\n")
			}
			path := filepath.Join("testdata", "pulljob_"+goos+".golden")
			if *updateGolden {
				if err := os.MkdirAll("testdata", 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path) //nolint:gosec // a golden file of this package
			if err != nil {
				t.Fatalf("%v: run the test with -update", err)
			}
			if string(want) != b.String() {
				t.Errorf("the %s job differs from %s:\n%s\nrun the test with -update if the change is meant", goos, path, b.String())
			}
		})
	}
}

func slicesValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// programs records the commands setup starts a scheduler with.
type programs struct {
	mu   sync.Mutex
	cmds []string
}

func (p *programs) run(_ context.Context, name string, args ...string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cmds = append(p.cmds, strings.Join(append([]string{name}, args...), " "))
	return nil
}

func pullJobSetup(t *testing.T, goos, home, repo string, p *programs, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	env := Env{Stdout: &out, Stderr: &errb, Getenv: func(string) string { return "" }, Version: "v0.3.0", GOOS: goos,
		UserHomeDir: func() (string, error) { return home, nil },
		Executable:  func() (string, error) { return "/opt/aries/bin/aries", nil }}
	if p != nil {
		env.RunProgram = p.run
	}
	code := Run(t.Context(), append([]string{"-C", repo}, args...), env)
	return code, out.String(), errb.String()
}

// `aries setup pull-job` prints by default and writes only with --write,
// checks what it wrote, and takes out only its own files.
func TestSetupPullJob(t *testing.T) {
	t.Parallel()
	repo := gitRepo(t)
	for _, args := range [][]string{
		{"remote", "add", "origin", "https://example.com/proj.git"},
		{"-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil { //nolint:gosec // test fixture
			t.Fatalf("git %s: %v\n%s", args[0], err, out)
		}
	}
	for _, tc := range []struct {
		goos  string
		files []string
		start string
	}{
		{"darwin", []string{"Library/LaunchAgents"}, "launchctl load -w"},
		{"linux", []string{".config/systemd/user"}, "systemctl --user enable --now"},
		{"windows", nil, "schtasks /Create"},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			p := &programs{}

			code, out, errb := pullJobSetup(t, tc.goos, home, repo, p, "setup", "pull-job", "--every", "30m")
			if code != ExitOK || !strings.Contains(out, "pull-job") || !strings.Contains(out, "every 30m0s") || len(p.cmds) != 0 || len(snapshot(t, home)) != 0 {
				t.Fatalf("print = exit %d\n%s%s\ncommands %v, files %v; want it printed and nothing written or run", code, out, errb, p.cmds, snapshot(t, home))
			}
			if code, _, _ := pullJobSetup(t, tc.goos, home, repo, p, "setup", "pull-job", "--check"); code != ExitFailure && len(tc.files) > 0 {
				t.Errorf("--check with no job = exit %d, want 1", code)
			}
			if code, _, errb := pullJobSetup(t, tc.goos, home, repo, p, "setup", "pull-job", "--every", "10s"); code != ExitUsage {
				t.Errorf("--every 10s = exit %d\n%s\nwant a usage error", code, errb)
			}

			if code, out, errb = pullJobSetup(t, tc.goos, home, repo, p, "setup", "pull-job", "--write"); code != ExitOK {
				t.Fatalf("--write = exit %d\n%s%s", code, out, errb)
			}
			files := snapshot(t, home)
			if (len(tc.files) == 0) != (len(files) == 0) {
				t.Fatalf("files after --write: %v", files)
			}
			for path, body := range files {
				if !strings.Contains(body, pullJobMark) {
					t.Errorf("%s = %q, want aries's mark", path, body)
				}
			}
			if all := strings.Join(slicesValues(files), "\n"); len(files) > 0 && !strings.Contains(all, "--run") {
				t.Errorf("the files %q do not run `pull-job --run`", all)
			}
			if len(p.cmds) == 0 || !strings.Contains(strings.Join(p.cmds, "\n"), tc.start) {
				t.Errorf("commands run = %v, want one with %q", p.cmds, tc.start)
			}
			if tc.goos == "windows" {
				return // Task Scheduler keeps the task: nothing on disk to compare
			}
			if code, _, errb := pullJobSetup(t, tc.goos, home, repo, p, "setup", "pull-job", "--check"); code != ExitOK {
				t.Errorf("--check with the job = exit %d\n%s", code, errb)
			}
			for path := range files {
				if err := os.WriteFile(path, []byte("# someone else's\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				break
			}
			if code, _, _ := pullJobSetup(t, tc.goos, home, repo, p, "setup", "pull-job", "--write"); code != ExitFailure {
				t.Errorf("--write over someone else's file = exit %d, want it refused", code)
			}
			if code, _, _ := pullJobSetup(t, tc.goos, home, repo, p, "setup", "pull-job", "--remove"); code != ExitFailure {
				t.Errorf("--remove of someone else's file = exit %d, want it refused", code)
			}
			if got := snapshot(t, home); len(got) != len(files) {
				t.Errorf("a refusal changed the files: %v", got)
			}
			for path := range files {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			pullJobSetup(t, tc.goos, home, repo, p, "setup", "pull-job", "--write")
			if code, _, errb := pullJobSetup(t, tc.goos, home, repo, p, "setup", "pull-job", "--remove"); code != ExitOK || len(snapshot(t, home)) != 0 {
				t.Errorf("--remove = exit %d\n%s\nfiles %v, want none", code, errb, snapshot(t, home))
			}
		})
	}
}
