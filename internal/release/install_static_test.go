package release_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ariesworx/starfix/internal/release"
	"github.com/ariesworx/starfix/internal/release/releasetest"
)

// fakeBinary is an sfx or starfixd that reports tag.
func fakeBinary(tag string) []byte {
	return fmt.Appendf(nil, "#!/bin/sh\necho 'starfix %s (protocol 1)'\n", tag)
}

// newInstallTree is a static tree of sfx for linux/amd64 at v1.2.3,
// v1.9.0 and v1.10.0, which is the newest though not the last in the
// alphabet, and the prerelease v1.11.0-rc.1. It returns the tree and its
// public key as a PEM.
func newInstallTree(t *testing.T) (*releasetest.Tree, string) {
	t.Helper()
	tr := releasetest.NewTree(t)
	for _, tag := range []string{"v1.2.3", "v1.9.0", "v1.10.0", "v1.11.0-rc.1"} {
		tr.AddBinary(t, tag, "sfx", "linux", "amd64", fakeBinary(tag))
	}
	der, err := x509.MarshalPKIXPublicKey(tr.Public)
	if err != nil {
		t.Fatal(err)
	}
	return tr, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// installEnv is the environment install.sh runs in: fake first on PATH,
// or alone on it when only is set.
func installEnv(t *testing.T, fake string, only bool, env ...string) []string {
	t.Helper()
	path := fake
	if !only {
		path += string(os.PathListSeparator) + os.Getenv("PATH")
	}
	return append([]string{
		"PATH=" + path,
		"HOME=" + t.TempDir(),
		"SHELL=/bin/zsh",
		"TMPDIR=" + t.TempDir(),
	}, env...)
}

// linuxAMD64 writes a uname into dir that reports Linux on x86_64.
func linuxAMD64(t *testing.T, dir string) {
	t.Helper()
	script(t, dir, "uname", "case $1 in -s) echo Linux ;; -m) echo x86_64 ;; *) exit 1 ;; esac\n")
}

// skipWithoutInstallTools skips a test where install.sh cannot run.
func skipWithoutInstallTools(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is for Linux and macOS")
	}
	for _, tool := range []string{"sh", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s: %v", tool, err)
		}
	}
}

// install.sh reads a static tree as sfx does: over loopback HTTP, from a
// directory, and from a file:// URL, and trusts the host with nothing but
// the choice of a tag.
func TestInstallScriptStatic(t *testing.T) {
	t.Parallel()
	skipWithoutInstallTools(t)
	if _, err := exec.LookPath("curl"); err != nil {
		if _, err := exec.LookPath("wget"); err != nil {
			t.Skip("neither curl nor wget")
		}
	}
	canVerify := opensslEd25519(t)

	// elsewhere stands for wherever a redirect points. Nothing may ask it
	// for anything.
	var asked atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		_, _ = w.Write([]byte(`{"format": 1, "releases": [{"tag": "v1.2.3"}]}`))
	}))
	t.Cleanup(elsewhere.Close)
	t.Cleanup(func() { // after every case, which run in parallel
		if n := asked.Load(); n != 0 {
			t.Errorf("install.sh followed a redirect: %d requests reached the other host", n)
		}
	})
	redirect := func(suffix string) func(*testing.T, *releasetest.Tree) {
		return func(_ *testing.T, tr *releasetest.Tree) {
			tr.Handle(func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.HasSuffix(r.URL.Path, suffix) {
					return false
				}
				http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusFound) //nolint:gosec // the test sends its own client to its own server
				return true
			})
		}
	}

	const (
		overHTTP = iota
		fromDir
		fromFileURL
	)
	tests := []struct {
		name string
		via  int
		// location replaces the tree's own, for the refusals of one.
		location string
		args     []string
		env      []string
		edit     func(t *testing.T, tr *releasetest.Tree)
		needSig  bool     // needs a working openssl
		want     string   // the tag installed; "" when the install must fail
		wantOut  []string // in the output
		noOut    []string // not in the output
	}{
		{name: "latest over http", want: "v1.10.0"},
		{name: "latest from a directory", via: fromDir, want: "v1.10.0"},
		{name: "latest from a file URL", via: fromFileURL, want: "v1.10.0"},
		{name: "pinned version", args: []string{"--version", "v1.9.0"}, want: "v1.9.0"},
		{name: "pinned prerelease", args: []string{"--version", "1.11.0-rc.1"}, want: "v1.11.0-rc.1"},
		{name: "pinned version needs no index", args: []string{"--version", "v1.9.0"}, via: fromDir,
			edit: func(_ *testing.T, tr *releasetest.Tree) { tr.Delete(release.IndexName) }, want: "v1.9.0"},
		{name: "index on one line",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.SetIndex(`{"format":1,"releases":[{"tag":"v1.10.0","prerelease":false},{"tag":"v1.9.0","prerelease":false},{"tag":"v1.11.0-rc.1","prerelease":true}]}`)
			}, want: "v1.10.0"},
		{name: "the variable wins over STARFIX_BASE_URL", env: []string{"STARFIX_BASE_URL=http://127.0.0.1:1/nothing"}, want: "v1.10.0"},

		// A fresh install has nothing older to compare with: an index of
		// old releases installs an old, signed release.
		{name: "index names only an older signed release",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "v1.2.3"}]}`)
			},
			want: "v1.2.3"},
		{name: "an older signed release in a newer one's directory",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.Move("v1.2.3", "v9.9.9")
				tr.Set("v9.9.9/sfx_9.9.9_linux_amd64.tar.gz", tr.File("v1.2.3/sfx_1.2.3_linux_amd64.tar.gz"))
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "v9.9.9"}]}`)
			},
			wantOut: []string{"sfx_9.9.9_linux_amd64.tar.gz is not listed in v9.9.9's checksums.txt", "fix: "}},
		{name: "index names a tag that climbs out of the tree",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "v1.2.3"}, {"tag": "../../v1.2.3"}]}`)
			},
			wantOut: []string{"index.json names a tag that is not a release version", "fix: "}},
		{name: "index names a URL",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "https://evil.example.com/v1.2.3"}]}`)
			},
			wantOut: []string{"index.json names a tag that is not a release version"}, noOut: []string{"evil.example.com"}},
		{name: "index names a huge tag",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "v1.2.3-` + strings.Repeat("a", 100_000) + `"}]}`)
			},
			wantOut: []string{"index.json names a tag that is not a release version"}, noOut: []string{"aaaaaaaa"}},
		{name: "huge index",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "v1.2.3"}]}` + strings.Repeat(" ", release.MaxIndexBytes))
			},
			wantOut: []string{"index.json is larger than 1048576 bytes", "fix: "}},
		{name: "malformed index",
			edit:    func(_ *testing.T, tr *releasetest.Tree) { tr.SetIndex("<html>Service unavailable</html>") },
			wantOut: []string{"index.json lists no release", "fix: "}},
		{name: "only prereleases",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "v1.11.0-rc.1", "prerelease": true}]}`)
			},
			wantOut: []string{"index.json lists no release"}},
		{name: "prerelease served as the latest release",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "v1.9.0", "prerelease": false}, {"tag": "v1.11.0-rc.1", "prerelease": false}]}`)
			},
			want: "v1.9.0"},
		{name: "index redirects elsewhere", edit: redirect("/index.json"),
			wantOut: []string{"cannot read", "index.json", "fix: "}},
		{name: "checksums redirect elsewhere", edit: redirect("/checksums.txt"),
			wantOut: []string{"cannot download", "checksums.txt"}},
		{name: "archive redirects elsewhere", edit: redirect(".tar.gz"),
			wantOut: []string{"cannot download", "sfx_1.10.0_linux_amd64.tar.gz"}},
		{name: "archive changed after signing",
			edit: func(t *testing.T, tr *releasetest.Tree) {
				tr.Set("v1.10.0/sfx_1.10.0_linux_amd64.tar.gz", releasetest.TarGz(t, "sfx", []byte("evil")))
			},
			wantOut: []string{"checksum mismatch for sfx_1.10.0_linux_amd64.tar.gz", "nothing was installed"}},
		{name: "signed by the host's own key", needSig: true,
			edit: func(t *testing.T, tr *releasetest.Tree) {
				_, other, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				tr.Set("v1.10.0/sfx_1.10.0_linux_amd64.tar.gz", releasetest.TarGz(t, "sfx", []byte("evil")))
				tr.Key = other
				tr.Sign("v1.10.0")
			},
			wantOut: []string{"the signature on v1.10.0's checksums.txt does not verify"}},
		{name: "no signature",
			edit:    func(_ *testing.T, tr *releasetest.Tree) { tr.Delete("v1.10.0/checksums.txt.sig") },
			wantOut: []string{"cannot download", "checksums.txt.sig"}},

		{name: "plain http to a host", location: "http://releases.example.com/starfix",
			wantOut: []string{"STARFIX_RELEASES is not an https URL", "fix: "}},
		{name: "http to a name that says loopback", location: "http://127.0.0.1.example.com/starfix",
			wantOut: []string{"STARFIX_RELEASES is not an https URL"}},
		{name: "a relative directory", location: "mirror/starfix",
			wantOut: []string{"STARFIX_RELEASES is not an https URL", "absolute path"}},
		{name: "a password in the URL", location: "https://alice:hunter2@releases.example.com/starfix", //nolint:gosec // an invented password, which the refusal must not repeat
			wantOut: []string{"STARFIX_RELEASES must not hold a user name, a password, a query or a fragment"}, noOut: []string{"hunter2"}},
		{name: "a file URL on another host", location: "file://fileserver.example.com/mirror",
			wantOut: []string{"STARFIX_RELEASES is not an https URL"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.needSig && !canVerify {
				t.Skip("openssl cannot verify Ed25519 here")
			}
			tr, pubPEM := newInstallTree(t)
			if tc.edit != nil {
				tc.edit(t, tr)
			}
			location := tr.URL
			if tc.via != overHTTP {
				mirror := t.TempDir()
				tr.WriteDir(t, mirror)
				location = mirror
				if tc.via == fromFileURL {
					location = fileURL(mirror)
				}
			}
			if tc.location != "" {
				location = tc.location
			}
			dir := filepath.Join(t.TempDir(), "bin")
			fake := t.TempDir()
			linuxAMD64(t, fake)
			env := installEnv(t, fake, false, append(tc.env,
				"STARFIX_DIR="+dir, "STARFIX_PUBKEY_PEM="+pubPEM, "STARFIX_RELEASES="+location)...)
			cmd := exec.Command("sh", append([]string{installScript}, tc.args...)...) //nolint:gosec // the test's own arguments
			cmd.Env = env
			b, err := cmd.CombinedOutput()
			out := string(b)

			for _, w := range tc.wantOut {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%.2000s", w, out)
				}
			}
			for _, w := range tc.noOut {
				if strings.Contains(out, w) {
					t.Errorf("output repeats %q, which the host or the variable supplied:\n%.2000s", w, out)
				}
			}
			bin := filepath.Join(dir, "sfx")
			got, readErr := os.ReadFile(bin) //nolint:gosec // test temp dir
			if tc.want == "" {
				if err == nil {
					t.Errorf("install.sh succeeded, want it to fail:\n%.2000s", out)
				}
				if readErr == nil {
					t.Errorf("a failed install left %s holding %q", bin, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("install.sh failed: %v\n%.2000s", err, out)
			}
			if want := fakeBinary(tc.want); string(got) != string(want) {
				t.Errorf("installed %q, want release %s's sfx, %q", got, tc.want, want)
			}
			if want := "installed starfix " + tc.want + " (protocol 1) to "; !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		})
	}
}

// With wget and no curl, install.sh reads a static tree the same way and
// follows no redirect either.
func TestInstallScriptStaticWget(t *testing.T) {
	t.Parallel()
	skipWithoutInstallTools(t)
	// A PATH of links to the tools install.sh uses, wget among them and
	// curl not.
	bin := t.TempDir()
	tools := []string{"sh", "wget", "tar", "gzip", "awk", "sed", "grep", "sort", "tr", "wc", "tail", "head", "mktemp",
		"rm", "cp", "mv", "chmod", "mkdir", "cat", "openssl"}
	for _, tool := range tools {
		p, err := exec.LookPath(tool)
		if err != nil {
			if tool == "openssl" {
				continue
			}
			t.Skipf("no %s: %v", tool, err)
		}
		if err := os.Symlink(p, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	sum := false
	for _, tool := range []string{"sha256sum", "shasum"} {
		if p, err := exec.LookPath(tool); err == nil {
			sum = true
			if err := os.Symlink(p, filepath.Join(bin, tool)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !sum {
		t.Skip("neither sha256sum nor shasum")
	}
	// shasum is a Perl script: it needs its interpreter.
	if p, err := exec.LookPath("perl"); err == nil {
		if err := os.Symlink(p, filepath.Join(bin, "perl")); err != nil {
			t.Fatal(err)
		}
	}
	linuxAMD64(t, bin)

	var asked atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		_, _ = w.Write([]byte(`{"format": 1, "releases": [{"tag": "v1.2.3"}]}`))
	}))
	t.Cleanup(elsewhere.Close)

	for _, tc := range []struct {
		name     string
		redirect bool
		wantOut  string
	}{
		{name: "latest", wantOut: "installed starfix v1.10.0 (protocol 1) to "},
		{name: "index redirects elsewhere", redirect: true, wantOut: "cannot read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, pubPEM := newInstallTree(t)
			if tc.redirect {
				tr.Handle(func(w http.ResponseWriter, r *http.Request) bool {
					http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusFound) //nolint:gosec // the test sends its own client to its own server
					return true
				})
			}
			dir := filepath.Join(t.TempDir(), "bin")
			cmd := exec.Command(filepath.Join(bin, "sh"), installScript) //nolint:gosec // the test's own link to sh
			cmd.Env = installEnv(t, bin, true, "STARFIX_DIR="+dir, "STARFIX_PUBKEY_PEM="+pubPEM, "STARFIX_RELEASES="+tr.URL)
			b, err := cmd.CombinedOutput()
			if (err == nil) == tc.redirect || !strings.Contains(string(b), tc.wantOut) {
				t.Errorf("install.sh: %v, want output holding %q:\n%s", err, tc.wantOut, b)
			}
			if n := asked.Load(); n != 0 {
				t.Errorf("install.sh followed a redirect: %d requests reached the other host", n)
			}
		})
	}
}
