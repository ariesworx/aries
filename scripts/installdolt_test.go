package scripts_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
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
)

// installDoltScript is the script beside this test.
const installDoltScript = "install-dolt.sh"

// fakeDolt is the dolt of the test's archives: it answers "dolt version".
func fakeDolt(version string) string {
	return "#!/bin/sh\necho 'dolt version " + version + "'\n"
}

// doltArchive packs dolt as Dolt's releases do: PLATFORM/bin/dolt in a
// gzipped tar.
func doltArchive(t *testing.T, platform, dolt string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: "dolt-" + platform + "/bin/dolt", Mode: 0o755, Size: int64(len(dolt))}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(dolt)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// doltCheckout is a copy of the script beside a dolt.sha256 of the
// test's own, laid out as the repository is, and a fake uname.
type doltCheckout struct {
	script, sums, fake, cache string
}

func newDoltCheckout(t *testing.T, sums, unameS, unameM string) doltCheckout {
	t.Helper()
	root := t.TempDir()
	c := doltCheckout{
		script: filepath.Join(root, "scripts", "install-dolt.sh"),
		sums:   filepath.Join(root, "internal", "version", "dolt.sha256"),
		fake:   filepath.Join(root, "fake"),
		cache:  filepath.Join(root, "cache"),
	}
	src, err := os.ReadFile(installDoltScript)
	if err != nil {
		t.Fatal(err)
	}
	uname := fmt.Sprintf("#!/bin/sh\ncase $1 in -s) echo %s ;; -m) echo %s ;; *) exit 1 ;; esac\n", unameS, unameM)
	for path, body := range map[string]string{
		c.script:                         string(src),
		c.sums:                           sums,
		filepath.Join(c.fake, "uname"):   uname,
		filepath.Join(root, "home", "x"): "",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil { //nolint:gosec // executable fixtures in a test directory
			t.Fatal(err)
		}
	}
	return c
}

// run runs the script against the release at url and returns what it
// printed to stdout and to stderr, and whether it succeeded.
func (c doltCheckout) run(t *testing.T, url string) (stdout, stderr string, ok bool) {
	t.Helper()
	cmd := exec.Command("sh", c.script) //nolint:gosec // the repository's script, copied by the test
	cmd.Env = []string{
		"PATH=" + c.fake + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + filepath.Join(filepath.Dir(c.fake), "home"),
		"XDG_CACHE_HOME=" + c.cache,
		"TMPDIR=" + t.TempDir(),
		"STARFIX_DOLT_BASE_URL=" + url,
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err == nil
}

func TestInstallDoltScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("scripts/install-dolt.sh is for Linux and macOS")
	}
	for _, tool := range []string{"sh", "tar", "awk"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s: %v", tool, err)
		}
	}
	if _, err := exec.LookPath("curl"); err != nil {
		if _, err := exec.LookPath("wget"); err != nil {
			t.Skip("neither curl nor wget")
		}
	}

	const v = "9.8.7"
	linux := doltArchive(t, "linux-amd64", fakeDolt(v))
	darwin := doltArchive(t, "darwin-arm64", fakeDolt(v))
	sum := func(name string, archive []byte) string {
		return fmt.Sprintf("# a comment\n%x  v%s/%s\n", sha256.Sum256(archive), v, name)
	}
	const linuxName, darwinName = "dolt-linux-amd64.tar.gz", "dolt-darwin-arm64.tar.gz"

	tests := []struct {
		name           string
		unameS, unameM string
		sums           string
		before         string // a dolt already in the cache directory
		ok             bool
		requests       int32
		want           []string // in stderr
	}{
		{name: "linux amd64", unameS: "Linux", unameM: "x86_64", sums: sum(linuxName, linux),
			ok: true, requests: 1, want: []string{"installed Dolt 9.8.7", `export PATH="`}},
		{name: "darwin arm64", unameS: "Darwin", unameM: "arm64", sums: sum(linuxName, linux) + sum(darwinName, darwin),
			ok: true, requests: 1, want: []string{"installed Dolt 9.8.7"}},
		{name: "already installed", unameS: "Linux", unameM: "x86_64", sums: sum(linuxName, linux), before: fakeDolt(v),
			ok: true, requests: 0, want: []string{"Dolt 9.8.7 is already in "}},
		{name: "checksum mismatch", unameS: "Linux", unameM: "x86_64", sums: sum(linuxName, darwin),
			requests: 1, want: []string{"install-dolt.sh: checksum mismatch for dolt-linux-amd64.tar.gz", "nothing was installed", "fix: "}},
		{name: "no pin for the platform", unameS: "Linux", unameM: "aarch64", sums: sum(linuxName, linux),
			want: []string{"install-dolt.sh: no checksum is pinned for v9.8.7/dolt-linux-arm64.tar.gz", "fix: "}},
		{name: "archive missing from the release", unameS: "Darwin", unameM: "x86_64", sums: sum("dolt-darwin-amd64.tar.gz", darwin),
			requests: 1, want: []string{"install-dolt.sh: cannot download ", "fix: "}},
		{name: "another dolt in the directory", unameS: "Linux", unameM: "x86_64", sums: sum(linuxName, linux), before: fakeDolt("1.0.0"),
			want: []string{"is not Dolt 9.8.7", "fix: remove "}},
		{name: "unsupported OS", unameS: "Plan9", unameM: "x86_64", sums: sum(linuxName, linux),
			want: []string{"install-dolt.sh: unsupported OS Plan9", "fix: "}},
		{name: "no release in the checksum file", unameS: "Linux", unameM: "x86_64", sums: "# nothing\n",
			want: []string{"names no release", "fix: "}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				switch r.URL.Path {
				case "/v" + v + "/" + linuxName:
					_, _ = w.Write(linux)
				case "/v" + v + "/" + darwinName:
					_, _ = w.Write(darwin)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			c := newDoltCheckout(t, tc.sums, tc.unameS, tc.unameM)
			bin := filepath.Join(c.cache, "starfix", "dolt-"+v, "bin")
			dolt := filepath.Join(bin, "dolt")
			if tc.before != "" {
				if err := os.MkdirAll(bin, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(dolt, []byte(tc.before), 0o700); err != nil { //nolint:gosec // an executable fixture
					t.Fatal(err)
				}
			}

			stdout, stderr, ok := c.run(t, srv.URL)
			if ok != tc.ok {
				t.Fatalf("succeeded = %v, want %v; stderr:\n%s", ok, tc.ok, stderr)
			}
			for _, w := range tc.want {
				if !strings.Contains(stderr, w) {
					t.Errorf("stderr lacks %q:\n%s", w, stderr)
				}
			}
			if got := requests.Load(); got > tc.requests {
				t.Errorf("made %d requests, want at most %d", got, tc.requests)
			}
			got, err := os.ReadFile(dolt) //nolint:gosec // test temp dir
			if !tc.ok {
				if stdout != "" {
					t.Errorf("a failed run printed %q to stdout, want nothing", stdout)
				}
				if string(got) != tc.before {
					t.Errorf("a failed run left %q at %s, want %q", got, dolt, tc.before)
				}
				if entries, _ := os.ReadDir(filepath.Join(c.cache, "starfix")); tc.before == "" && len(entries) != 0 {
					t.Errorf("a failed run left %d entries in the cache directory", len(entries))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			// Stdout is the directory alone, so PATH="$(install-dolt.sh):$PATH" works.
			if stdout != bin+"\n" {
				t.Errorf("stdout = %q, want %q", stdout, bin+"\n")
			}
			if string(got) != fakeDolt(v) {
				t.Errorf("installed %q, want the archive's dolt", got)
			}
			if fi, err := os.Stat(dolt); err != nil || fi.Mode().Perm()&0o100 == 0 {
				t.Errorf("%s is not executable: %v, %v", dolt, fi, err)
			}
			if entries, _ := os.ReadDir(filepath.Join(c.cache, "starfix")); len(entries) != 1 {
				t.Errorf("the cache directory holds %d entries, want only dolt-%s", len(entries), v)
			}
		})
	}
}
