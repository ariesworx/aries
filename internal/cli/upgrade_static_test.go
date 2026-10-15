package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ariesworx/aries/internal/release"
	"github.com/ariesworx/aries/internal/release/releasetest"
)

// staticUpgrade is an installed sfx and a static release tree, which sfx
// finds through $STARFIX_RELEASES and nothing else: Upgrader.Releases is
// not set.
type staticUpgrade struct {
	tree *releasetest.Tree
	exe  string
	env  Env
	// vars is the environment sfx sees.
	vars map[string]string
}

// newStaticUpgrade installs "sfx <current>" and serves a tree of the
// releases given, each with an sfx for linux/amd64 holding "sfx <tag>".
func newStaticUpgrade(t *testing.T, current string, tags ...string) *staticUpgrade {
	t.Helper()
	f := &staticUpgrade{tree: releasetest.NewTree(t), exe: filepath.Join(t.TempDir(), "sfx"), vars: map[string]string{}}
	for _, tag := range tags {
		f.tree.AddBinary(t, tag, "sfx", "linux", "amd64", []byte("sfx "+tag))
	}
	if err := os.WriteFile(f.exe, []byte("sfx "+current), 0o755); err != nil { //nolint:gosec // an executable fixture
		t.Fatal(err)
	}
	f.vars[release.EnvReleases] = f.tree.URL
	f.env = Env{Getenv: func(k string) string { return f.vars[k] }, Version: current, Upgrader: &Upgrader{
		Verifier: f.tree.Verifier(),
		Exe:      f.exe,
		GOOS:     "linux", GOARCH: "amd64",
		Check: func(_ context.Context, p, tag string) error {
			if b, err := os.ReadFile(p); err != nil || string(b) != "sfx "+tag { //nolint:gosec // test temp dir
				return errors.New("staged binary is not the release's")
			}
			return nil
		},
	}}
	return f
}

func (f *staticUpgrade) run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	env := f.env
	env.Stdout, env.Stderr = &out, &errb
	code = Run(t.Context(), args, env)
	return code, out.String(), errb.String()
}

func (f *staticUpgrade) exeHolds(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(f.exe)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// sfx upgrade, start to finish, from a static tree named by
// $STARFIX_RELEASES: over loopback HTTP, from a directory, and from a
// file:// URL.
func TestUpgradeFromStaticTree(t *testing.T) {
	t.Parallel()
	for _, via := range []string{"http", "directory", "file URL"} {
		t.Run(via, func(t *testing.T) {
			f := newStaticUpgrade(t, "v0.2.0", "v0.2.0", "v0.3.0", "v0.4.0-rc.1")
			if via != "http" {
				dir := t.TempDir()
				f.tree.WriteDir(t, dir)
				f.vars[release.EnvReleases] = dir
				if via == "file URL" {
					f.vars[release.EnvReleases] = "file://" + filepath.ToSlash(dir)
				}
			}
			code, out, errb := f.run(t, "upgrade", "--check")
			if want := "starfix v0.2.0, latest v0.3.0: run sfx upgrade\n"; code != ExitOK || out != want {
				t.Fatalf("upgrade --check: exit %d, stdout %q, stderr %q; want %q", code, out, errb, want)
			}
			code, out, errb = f.run(t, "upgrade")
			if code != ExitOK || !strings.Contains(out, "upgraded v0.2.0 -> v0.3.0") {
				t.Fatalf("upgrade: exit %d, stdout %q, stderr %q", code, out, errb)
			}
			if got := f.exeHolds(t); got != "sfx v0.3.0" {
				t.Fatalf("after upgrade the binary holds %q, want sfx v0.3.0", got)
			}
			f.env.Version = "v0.3.0"
			if code, out, _ := f.run(t, "upgrade"); code != ExitOK || out != "starfix v0.3.0 is up to date\n" {
				t.Errorf("second upgrade: exit %d, stdout %q", code, out)
			}
			if code, _, errb := f.run(t, "upgrade", "--rollback"); code != ExitOK || f.exeHolds(t) != "sfx v0.2.0" {
				t.Errorf("rollback: exit %d, stderr %q, binary %q", code, errb, f.exeHolds(t))
			}
		})
	}
}

// Which source sfx reads is $STARFIX_RELEASES, and a value it cannot use
// is refused before anything is asked of anyone.
func TestUpgradeSourceSelection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		location string
		args     []string
		code     int
		stderr   []string
		noStderr string
	}{
		{name: "plain http to a host", location: "http://releases.example.com/starfix", args: []string{"upgrade"},
			code: ExitFailure, stderr: []string{"STARFIX_RELEASES", "not an https URL", "fix: "}},
		{name: "a relative directory", location: "mirror", args: []string{"upgrade", "--check"},
			code: ExitFailure, stderr: []string{"STARFIX_RELEASES", "absolute path", "fix: "}},
		{name: "a password in the URL", location: "https://alice:hunter2@releases.example.com/", args: []string{"upgrade"}, //nolint:gosec // an invented password, which the refusal must not repeat
			code: ExitFailure, stderr: []string{"STARFIX_RELEASES", "user name or password", "fix: "}, noStderr: "hunter2"},
		{name: "a directory that is not there", location: filepath.Join(t.TempDir(), "nothing"), args: []string{"upgrade"},
			code: ExitFailure, stderr: []string{"cannot read the latest starfix release", "nothing", "fix: "}},
		// Rollback reads no release, so a bad location does not stop it.
		{name: "rollback does not read the source", location: "http://releases.example.com/starfix", args: []string{"upgrade", "--rollback"},
			code: ExitFailure, stderr: []string{"no previous binary"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newStaticUpgrade(t, "v0.2.0", "v0.3.0")
			f.vars[release.EnvReleases] = tc.location
			code, out, errb := f.run(t, tc.args...)
			if code != tc.code {
				t.Errorf("exit %d, want %d; stdout %q, stderr %q", code, tc.code, out, errb)
			}
			for _, w := range tc.stderr {
				if !strings.Contains(errb, w) {
					t.Errorf("stderr %q lacks %q", errb, w)
				}
			}
			if tc.noStderr != "" && strings.Contains(errb, tc.noStderr) {
				t.Errorf("stderr %q repeats %q", errb, tc.noStderr)
			}
			if got := f.exeHolds(t); got != "sfx v0.2.0" {
				t.Errorf("the binary holds %q after a refusal", got)
			}
			if got := f.tree.Requests(); len(got) != 0 {
				t.Errorf("the tree was asked for %q, though the variable named something else", got)
			}
		})
	}
}

// A test tree is signed with a test key. Without Upgrader.Verifier, sfx
// checks it against the keys built into the binary, and refuses it.
func TestUpgradeStaticTreeAgainstBuiltInKeys(t *testing.T) {
	t.Parallel()
	f := newStaticUpgrade(t, "v0.2.0", "v0.3.0")
	f.env.Upgrader.Verifier = nil
	code, _, errb := f.run(t, "upgrade")
	if code != ExitFailure || !strings.Contains(errb, "failed verification") {
		t.Errorf("exit %d, stderr %q, want a release no built-in key signed refused", code, errb)
	}
	if got := f.exeHolds(t); got != "sfx v0.2.0" {
		t.Errorf("the binary holds %q after a refusal", got)
	}
}

// What a hostile or broken release host gets out of sfx upgrade: an
// error, or nothing. In no case is the binary replaced.
func TestUpgradeHostileStaticHost(t *testing.T) {
	t.Parallel()
	var asked atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		_, _ = w.Write([]byte(`{"format": 1, "releases": [{"tag": "v0.9.0"}]}`))
	}))
	t.Cleanup(elsewhere.Close)

	tests := []struct {
		name   string
		edit   func(t *testing.T, tr *releasetest.Tree)
		code   int
		stdout string
		stderr string
	}{
		{name: "index names only an older signed release",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "v0.2.0"}]}`)
			},
			code: ExitOK, stdout: "starfix v0.3.0 is up to date\n"},
		{name: "an older signed release in a newer one's directory",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.Move("v0.2.0", "v0.9.0")
				tr.Set("v0.9.0/sfx_0.9.0_linux_amd64.tar.gz", tr.File("v0.2.0/sfx_0.2.0_linux_amd64.tar.gz"))
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "v0.9.0"}]}`)
			},
			code: ExitFailure, stderr: "does not list sfx_0.9.0_linux_amd64.tar.gz"},
		{name: "index names a tag that climbs out of the tree",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "../v0.4.0"}]}`)
			},
			code: ExitFailure, stderr: "not of the form vMAJOR.MINOR.PATCH"},
		{name: "index names a URL",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "` + elsewhere.URL + `/v0.9.0"}]}`)
			},
			code: ExitFailure, stderr: "not of the form vMAJOR.MINOR.PATCH"},
		{name: "huge index",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "v0.4.0"}]}` + strings.Repeat(" ", release.MaxIndexBytes))
			},
			code: ExitFailure, stderr: "larger than"},
		{name: "malformed index",
			edit: func(_ *testing.T, tr *releasetest.Tree) { tr.SetIndex(`{"format": 1, "releases": [{"tag": `) },
			code: ExitFailure, stderr: "index.json"},
		{name: "prerelease served as the latest release",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "v0.5.0-rc.1", "prerelease": false}]}`)
			},
			code: ExitFailure, stderr: "prerelease"},
		{name: "prerelease is the only newer release",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "v0.3.0"}, {"tag": "v0.5.0-rc.1", "prerelease": true}]}`)
			},
			code: ExitOK, stdout: "starfix v0.3.0 is up to date\n"},
		{name: "index redirects elsewhere",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.Handle(func(w http.ResponseWriter, r *http.Request) bool {
					http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusFound) //nolint:gosec // the test sends its own client to its own server
					return true
				})
			},
			code: ExitFailure, stderr: "a redirect, and none is followed"},
		{name: "archive changed after signing",
			edit: func(t *testing.T, tr *releasetest.Tree) {
				tr.Set("v0.4.0/sfx_0.4.0_linux_amd64.tar.gz", releasetest.TarGz(t, "sfx", []byte("evil")))
			},
			code: ExitFailure, stderr: "checksum mismatch"},
		{name: "archive and checksums changed, signature kept",
			edit: func(t *testing.T, tr *releasetest.Tree) {
				sig := tr.File("v0.4.0/checksums.txt.sig")
				tr.Set("v0.4.0/sfx_0.4.0_linux_amd64.tar.gz", releasetest.TarGz(t, "sfx", []byte("sfx v0.4.0")))
				tr.Set("v0.4.0/extra_0.4.0_linux_amd64.tar.gz", []byte("x"))
				tr.Sign("v0.4.0")
				tr.Set("v0.4.0/checksums.txt.sig", sig)
			},
			code: ExitFailure, stderr: "failed verification"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newStaticUpgrade(t, "v0.3.0", "v0.2.0", "v0.3.0", "v0.4.0", "v0.5.0-rc.1")
			tc.edit(t, f.tree)
			asked.Store(0)
			code, out, errb := f.run(t, "upgrade")
			if code != tc.code || out != tc.stdout || !strings.Contains(errb, tc.stderr) {
				t.Errorf("exit %d (want %d), stdout %q (want %q), stderr %q (want it to hold %q)",
					code, tc.code, out, tc.stdout, errb, tc.stderr)
			}
			if tc.code != ExitOK && !strings.Contains(errb, "fix: ") {
				t.Errorf("stderr %q has no fix", errb)
			}
			if len(errb) > 1000 {
				t.Errorf("stderr is %d bytes long; it repeats what the host sent", len(errb))
			}
			if got := f.exeHolds(t); got != "sfx v0.3.0" {
				t.Errorf("the binary holds %q, want it untouched", got)
			}
			if _, err := os.Stat(f.exe + ".prev"); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a refused upgrade left a .prev: %v", err)
			}
			if n := asked.Load(); n != 0 {
				t.Errorf("sfx followed a redirect: %d requests reached the other host", n)
			}
		})
	}
}
