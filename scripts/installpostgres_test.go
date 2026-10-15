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

// installPostgresScript is the script beside this test.
const installPostgresScript = "install-postgres.sh"

// fakePostgres is the postgres of the test's archives and packages: it
// answers --version as the release, or a distribution's build, does.
func fakePostgres(version string) string {
	return "#!/bin/sh\necho 'postgres (PostgreSQL) " + version + "'\n"
}

// postgresArchive packs a source tree of release version as the
// PostgreSQL project's archives are laid out, postgresql-VERSION/ in a
// gzipped tar, whose configure, make and make install stand in for the
// real build: configure records --prefix and writes a Makefile, and make
// install puts postgres, which answers --version as postgres says,
// under $(DESTDIR)PREFIX/bin. With fail set, configure fails as one
// that finds no compiler does.
func postgresArchive(t *testing.T, version, postgres string, fail bool) []byte {
	t.Helper()
	configure := `#!/bin/sh
for a; do case $a in --prefix=*) printf '%s\n' "${a#--prefix=}" >prefix ;; esac; done
printf 'all:\n\t@true\ninstall:\n\tsh ./install-fake.sh "$(DESTDIR)"\n' >Makefile
`
	if fail {
		configure = "#!/bin/sh\necho 'configure: error: no acceptable C compiler found in $PATH'\nexit 1\n"
	}
	files := []struct {
		name, body string
	}{
		{"configure", configure},
		{"install-fake.sh", `p=$(cat prefix) && mkdir -p "$1$p/bin" && cp postgres "$1$p/bin/postgres" && chmod 755 "$1$p/bin/postgres"` + "\n"},
		{"postgres", postgres},
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		hdr := &tar.Header{Name: "postgresql-" + version + "/" + f.name, Mode: 0o755, Size: int64(len(f.body))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// postgresCheckout is a copy of the script beside a postgres.sha256 of
// the test's own, laid out as the repository is, a fake uname and cc,
// and a directory standing for /usr/lib/postgresql.
type postgresCheckout struct {
	script, sums, fake, cache, packaged string
}

func newPostgresCheckout(t *testing.T, sums, unameS string) postgresCheckout {
	t.Helper()
	root := t.TempDir()
	c := postgresCheckout{
		script:   filepath.Join(root, "scripts", "install-postgres.sh"),
		sums:     filepath.Join(root, "internal", "version", "postgres.sha256"),
		fake:     filepath.Join(root, "fake"),
		cache:    filepath.Join(root, "cache"),
		packaged: filepath.Join(root, "usr-lib-postgresql"),
	}
	src, err := os.ReadFile(installPostgresScript)
	if err != nil {
		t.Fatal(err)
	}
	uname := fmt.Sprintf("#!/bin/sh\ncase $1 in -s) echo %s ;; -m) echo x86_64 ;; *) exit 1 ;; esac\n", unameS)
	for path, body := range map[string]string{
		c.script:                         string(src),
		c.sums:                           sums,
		filepath.Join(c.fake, "uname"):   uname,
		filepath.Join(c.fake, "cc"):      "#!/bin/sh\nexit 0\n",
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

// run runs the script against the archives at url and returns what it
// printed to stdout and to stderr, and whether it succeeded.
func (c postgresCheckout) run(t *testing.T, url string) (stdout, stderr string, ok bool) {
	t.Helper()
	cmd := exec.Command("sh", c.script) //nolint:gosec // the repository's script, copied by the test
	cmd.Env = []string{
		"PATH=" + c.fake + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + filepath.Join(filepath.Dir(c.fake), "home"),
		"XDG_CACHE_HOME=" + c.cache,
		"TMPDIR=" + t.TempDir(),
		"ARIES_POSTGRES_BASE_URL=" + url,
		"ARIES_POSTGRES_PACKAGED=" + c.packaged,
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err == nil
}

func TestInstallPostgresScript(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("scripts/install-postgres.sh is for Linux and macOS")
	}
	for _, tool := range []string{"sh", "tar", "gzip", "awk", "make"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s: %v", tool, err)
		}
	}
	if _, err := exec.LookPath("curl"); err != nil {
		if _, err := exec.LookPath("wget"); err != nil {
			t.Skip("neither curl nor wget")
		}
	}

	const v, name = "9.8", "postgresql-9.8.tar.gz"
	good := postgresArchive(t, v, fakePostgres(v), false)
	broken := postgresArchive(t, v, fakePostgres(v), true)
	sum := func(archive []byte) string {
		return fmt.Sprintf("# a comment\n%x  v%s/%s\n", sha256.Sum256(archive), v, name)
	}

	tests := []struct {
		name     string
		unameS   string
		sums     string
		archive  []byte
		packaged string // a packaged postgres of major version 9, if any
		before   string // a postgres already in the cache directory
		ok       bool
		built    bool // the bin directory printed is the cache's, not the package's
		requests int32
		want     []string // in stderr
	}{
		{name: "macOS builds", unameS: "Darwin", sums: sum(good), archive: good,
			ok: true, built: true, requests: 1, want: []string{"building Postgres 9.8", "installed Postgres 9.8", `export PATH="`}},
		{name: "linux uses the package", unameS: "Linux", sums: sum(good), archive: good, packaged: fakePostgres("9.3 (Ubuntu 9.3-1.pgdg24.04+1)"),
			ok: true, want: []string{"using the packaged Postgres 9.3"}},
		{name: "linux builds without the package", unameS: "Linux", sums: sum(good), archive: good,
			ok: true, built: true, requests: 1, want: []string{"installed Postgres 9.8"}},
		{name: "linux builds beside another major's package", unameS: "Linux", sums: sum(good), archive: good, packaged: fakePostgres("10.1"),
			ok: true, built: true, requests: 1, want: []string{"installed Postgres 9.8"}},
		{name: "already installed", unameS: "Darwin", sums: sum(good), archive: good, before: fakePostgres(v),
			ok: true, built: true, want: []string{"Postgres 9.8 is already in "}},
		{name: "checksum mismatch", unameS: "Darwin", sums: sum(broken), archive: good,
			requests: 1, want: []string{"install-postgres.sh: checksum mismatch for postgresql-9.8.tar.gz", "nothing was installed", "fix: "}},
		{name: "build fails", unameS: "Darwin", sums: sum(broken), archive: broken,
			requests: 1, want: []string{"install-postgres.sh: building Postgres 9.8 failed; nothing was installed", "build.log", "fix: "}},
		{name: "archive missing", unameS: "Darwin", sums: sum(good),
			requests: 1, want: []string{"install-postgres.sh: cannot download ", "fix: "}},
		{name: "another postgres in the directory", unameS: "Darwin", sums: sum(good), archive: good, before: fakePostgres("1.0"),
			want: []string{"is not Postgres 9.8", "fix: remove "}},
		{name: "unsupported OS", unameS: "Plan9", sums: sum(good), archive: good,
			want: []string{"install-postgres.sh: unsupported OS Plan9", "fix: "}},
		{name: "no release in the checksum file", unameS: "Darwin", sums: "# nothing\n", archive: good,
			want: []string{"names no release", "fix: "}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path == "/v"+v+"/"+name && tc.archive != nil {
					_, _ = w.Write(tc.archive)
					return
				}
				http.NotFound(w, r)
			}))
			t.Cleanup(srv.Close)
			c := newPostgresCheckout(t, tc.sums, tc.unameS)
			bin := filepath.Join(c.cache, "aries", "postgres-"+v, "bin")
			postgres := filepath.Join(bin, "postgres")
			for dir, body := range map[string]string{bin: tc.before, filepath.Join(c.packaged, "9", "bin"): tc.packaged} {
				if body == "" {
					continue
				}
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "postgres"), []byte(body), 0o700); err != nil { //nolint:gosec // an executable fixture
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
			got, err := os.ReadFile(postgres) //nolint:gosec // test temp dir
			if !tc.ok {
				if stdout != "" {
					t.Errorf("a failed run printed %q to stdout, want nothing", stdout)
				}
				if string(got) != tc.before {
					t.Errorf("a failed run left %q at %s, want %q", got, postgres, tc.before)
				}
				// A failed build keeps its log, and nothing else.
				entries, _ := os.ReadDir(filepath.Join(c.cache, "aries"))
				for _, e := range entries {
					if tc.before == "" && !strings.HasSuffix(e.Name(), ".build.log") {
						t.Errorf("a failed run left %s in the cache directory", e.Name())
					}
				}
				return
			}
			if !tc.built {
				// Stdout is the directory alone, so PATH="$(install-postgres.sh):$PATH" works.
				if want := filepath.Join(c.packaged, "9", "bin") + "\n"; stdout != want {
					t.Errorf("stdout = %q, want the package's %q", stdout, want)
				}
				if entries, _ := os.ReadDir(filepath.Join(c.cache, "aries")); len(entries) != 0 {
					t.Errorf("using the package left %d entries in the cache directory, want none", len(entries))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if stdout != bin+"\n" {
				t.Errorf("stdout = %q, want %q", stdout, bin+"\n")
			}
			if string(got) != fakePostgres(v) {
				t.Errorf("installed %q, want the archive's postgres", got)
			}
			if fi, err := os.Stat(postgres); err != nil || fi.Mode().Perm()&0o100 == 0 {
				t.Errorf("%s is not executable: %v, %v", postgres, fi, err)
			}
			if entries, _ := os.ReadDir(filepath.Join(c.cache, "aries")); len(entries) != 1 {
				t.Errorf("the cache directory holds %d entries, want only postgres-%s", len(entries), v)
			}
		})
	}
}
