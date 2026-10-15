package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/release"
	"github.com/ariesworx/aries/internal/release/releasetest"
)

// result is what one run returned and printed.
type result struct {
	code           int
	stdout, stderr string
}

// fixture is a release key and the directories of one run after another.
type fixture struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
	dest string
	now  time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{pub: pub, priv: priv, dest: filepath.Join(t.TempDir(), "tree"),
		now: time.Date(2026, 5, 6, 7, 8, 9, 123, time.UTC)}
}

func (f *fixture) run(args ...string) result {
	var out, errb bytes.Buffer
	code := run(args, env{
		stdout: &out, stderr: &errb,
		now:  func() time.Time { return f.now },
		keys: func() ([]ed25519.PublicKey, error) { return []ed25519.PublicKey{f.pub}, nil },
	})
	return result{code, out.String(), errb.String()}
}

// dist writes what the release workflow's dist directory holds for tag: an
// aries archive for linux and for windows, an ariesd archive, checksums.txt
// and its signature. It returns the directory.
func (f *fixture) dist(t *testing.T, tag string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string][]byte{
		release.ArchiveName("aries", tag, "linux", "amd64"):   releasetest.TarGz(t, "aries", []byte("aries "+tag)),
		release.ArchiveName("aries", tag, "windows", "amd64"): releasetest.Zip(t, "aries.exe", []byte("aries.exe "+tag)),
		release.ArchiveName("ariesd", tag, "linux", "arm64"):  releasetest.TarGz(t, "ariesd", []byte("ariesd "+tag)),
	}
	var sums strings.Builder
	for _, name := range slices.Sorted(maps.Keys(files)) {
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(files[name]), name)
	}
	files[release.ChecksumsName] = []byte(sums.String())
	files[release.SignatureName] = release.EncodeSignature(ed25519.Sign(f.priv, []byte(sums.String())))
	for name, b := range files {
		put(t, filepath.Join(dir, name), b)
	}
	return dir
}

func put(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o644); err != nil { //nolint:gosec // public release files
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test temp dir
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// tree lists every file under dir, by slash-separated path.
func tree(t *testing.T, dir string) []string {
	t.Helper()
	var got []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		got = append(got, filepath.ToSlash(rel))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	return got
}

// The tool lays releases out as the static source reads them, and what
// it writes is what a client then installs from.
func TestLayout(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if r := f.run("--published", "2026-01-02T03:04:05Z", "v0.9.0", f.dist(t, "v0.9.0"), f.dest); r.code != 0 {
		t.Fatalf("first release: %+v", r)
	}
	if r := f.run("--published", "2026-03-02T03:04:05Z", "v0.10.0", f.dist(t, "v0.10.0"), f.dest); r.code != 0 {
		t.Fatalf("second release: %+v", r)
	}
	// A prerelease, and an older release added late: the index stays in
	// version order.
	if r := f.run("--published", "2026-02-02T03:04:05Z", "v0.10.0-rc.1", f.dist(t, "v0.10.0-rc.1"), f.dest); r.code != 0 {
		t.Fatalf("prerelease: %+v", r)
	}

	want := []string{"index.json"}
	for _, tag := range []string{"v0.10.0", "v0.10.0-rc.1", "v0.9.0"} {
		v := strings.TrimPrefix(tag, "v")
		want = append(want, tag+"/checksums.txt", tag+"/checksums.txt.sig",
			tag+"/aries_"+v+"_linux_amd64.tar.gz", tag+"/aries_"+v+"_windows_amd64.zip", tag+"/ariesd_"+v+"_linux_arm64.tar.gz")
	}
	slices.Sort(want)
	if got := tree(t, f.dest); !slices.Equal(got, want) {
		t.Errorf("the tree holds\n%q\nwant\n%q", got, want)
	}
	const wantIndex = `{
  "format": 1,
  "releases": [
    {"tag": "v0.9.0", "prerelease": false, "published": "2026-01-02T03:04:05Z"},
    {"tag": "v0.10.0-rc.1", "prerelease": true, "published": "2026-02-02T03:04:05Z"},
    {"tag": "v0.10.0", "prerelease": false, "published": "2026-03-02T03:04:05Z"}
  ]
}
`
	if got := readFile(t, filepath.Join(f.dest, "index.json")); got != wantIndex {
		t.Errorf("index.json =\n%s\nwant\n%s", got, wantIndex)
	}

	// The client reads the tree the tool wrote.
	c, err := release.Open(f.dest, release.Ed25519{Keys: []ed25519.PublicKey{f.pub}})
	if err != nil {
		t.Fatal(err)
	}
	rel, err := c.Latest(t.Context())
	if err != nil || rel.Tag != "v0.10.0" {
		t.Fatalf("Latest() of the tree = %+v, %v, want v0.10.0", rel, err)
	}
	if bin, err := c.Fetch(t.Context(), rel, "aries", "windows", "amd64"); err != nil || string(bin) != "aries.exe v0.10.0" {
		t.Errorf("Fetch(v0.10.0, aries, windows, amd64) = %q, %v", bin, err)
	}
}

// The same release laid out again changes nothing: not a byte of the
// index, whatever the clock says, and no file.
func TestLayoutIsRepeatable(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dist := f.dist(t, "v0.9.0")
	if r := f.run("v0.9.0", dist, f.dest); r.code != 0 {
		t.Fatalf("first run: %+v", r)
	}
	index := readFile(t, filepath.Join(f.dest, "index.json"))
	if want := `"published": "2026-05-06T07:08:09Z"`; !strings.Contains(index, want) {
		t.Errorf("with no --published, index.json =\n%s\nwant the clock's time in UTC to the second, %s", index, want)
	}
	f.now = f.now.Add(48 * time.Hour)
	if r := f.run("v0.9.0", dist, f.dest); r.code != 0 {
		t.Fatalf("second run: %+v", r)
	}
	if got := readFile(t, filepath.Join(f.dest, "index.json")); got != index {
		t.Errorf("a second run changed index.json to\n%s\nfrom\n%s", got, index)
	}
	// Two trees built from the same inputs are the same bytes.
	other := *f
	other.dest = filepath.Join(t.TempDir(), "tree")
	if r := other.run("--published", "2026-05-06T07:08:09Z", "v0.9.0", dist, other.dest); r.code != 0 {
		t.Fatalf("other tree: %+v", r)
	}
	if got := readFile(t, filepath.Join(other.dest, "index.json")); got != index {
		t.Errorf("another tree's index.json =\n%s\nwant\n%s", got, index)
	}
}

func TestLayoutRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		tag  string
		// edit changes the dist directory of v0.9.0, or the tree.
		edit func(t *testing.T, f *fixture, dist string)
		args []string // in place of TAG SRC DEST
		code int
		want string
	}{
		{name: "archive changed after checksums", tag: "v0.9.0", code: 1, want: "aries_0.9.0_linux_amd64.tar.gz: sha256",
			edit: func(t *testing.T, _ *fixture, dist string) {
				put(t, filepath.Join(dist, "aries_0.9.0_linux_amd64.tar.gz"), []byte("changed"))
			}},
		{name: "archive missing", tag: "v0.9.0", code: 1, want: "aries_0.9.0_windows_amd64.zip",
			edit: func(t *testing.T, _ *fixture, dist string) {
				if err := os.Remove(filepath.Join(dist, "aries_0.9.0_windows_amd64.zip")); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "signature missing", tag: "v0.9.0", code: 1, want: "checksums.txt.sig",
			edit: func(t *testing.T, _ *fixture, dist string) {
				if err := os.Remove(filepath.Join(dist, "checksums.txt.sig")); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "signed by a key no build trusts", tag: "v0.9.0", code: 1, want: "signature does not verify",
			edit: func(t *testing.T, _ *fixture, dist string) {
				_, other, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				sums, err := os.ReadFile(filepath.Join(dist, "checksums.txt")) //nolint:gosec // test temp dir
				if err != nil {
					t.Fatal(err)
				}
				put(t, filepath.Join(dist, "checksums.txt.sig"), release.EncodeSignature(ed25519.Sign(other, sums)))
			}},
		{name: "another release's files", tag: "v0.9.1", code: 1, want: "is not a file of release v0.9.1"},
		{name: "checksums list a path", tag: "v0.9.0", code: 1, want: "is not a file of release v0.9.0",
			edit: func(t *testing.T, f *fixture, dist string) {
				sums := fmt.Sprintf("%x  ../aries_0.9.0_linux_amd64.tar.gz\n", sha256.Sum256(nil))
				put(t, filepath.Join(dist, "checksums.txt"), []byte(sums))
				put(t, filepath.Join(dist, "checksums.txt.sig"), release.EncodeSignature(ed25519.Sign(f.priv, []byte(sums))))
			}},
		{name: "empty checksums", tag: "v0.9.0", code: 1, want: "lists no archive",
			edit: func(t *testing.T, f *fixture, dist string) {
				put(t, filepath.Join(dist, "checksums.txt"), nil)
				put(t, filepath.Join(dist, "checksums.txt.sig"), release.EncodeSignature(ed25519.Sign(f.priv, nil)))
			}},
		{name: "the tree holds this release with other bytes", tag: "v0.9.0", code: 1, want: "already holds",
			edit: func(t *testing.T, f *fixture, _ string) {
				if err := os.MkdirAll(filepath.Join(f.dest, "v0.9.0"), 0o755); err != nil { //nolint:gosec // a public tree
					t.Fatal(err)
				}
				put(t, filepath.Join(f.dest, "v0.9.0", "checksums.txt"), []byte("other"))
			}},
		{name: "the tree's index is not one", tag: "v0.9.0", code: 1, want: "index.json",
			edit: func(t *testing.T, f *fixture, _ string) {
				if err := os.MkdirAll(f.dest, 0o755); err != nil { //nolint:gosec // a public tree
					t.Fatal(err)
				}
				put(t, filepath.Join(f.dest, "index.json"), []byte(`{"format": 7}`))
			}},
		{name: "tag is not a version", tag: "latest", code: 2, want: "not of the form vMAJOR.MINOR.PATCH"},
		{name: "tag is a path", tag: "v0.9.0/..", code: 2, want: "not of the form vMAJOR.MINOR.PATCH"},
		{name: "published is not a time", args: []string{"--published", "yesterday", "v0.9.0", "src", "dest"}, code: 2, want: "RFC 3339"},
		{name: "too few arguments", args: []string{"v0.9.0"}, code: 2, want: "usage:"},
		{name: "unknown flag", args: []string{"--sign", "v0.9.0", "src", "dest"}, code: 2, want: "usage:"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			dist := f.dist(t, "v0.9.0")
			if tc.edit != nil {
				tc.edit(t, f, dist)
			}
			before := []string(nil)
			if _, err := os.Stat(f.dest); err == nil {
				before = tree(t, f.dest)
			}
			args := tc.args
			if args == nil {
				args = []string{tc.tag, dist, f.dest}
			}
			r := f.run(args...)
			if r.code != tc.code || !strings.Contains(r.stderr, tc.want) {
				t.Errorf("run(%q) = exit %d, stderr %q; want exit %d and stderr holding %q", args, r.code, r.stderr, tc.code, tc.want)
			}
			// A refusal writes nothing: no half-laid-out release.
			after := []string(nil)
			if _, err := os.Stat(f.dest); err == nil {
				after = tree(t, f.dest)
			}
			if !slices.Equal(before, after) {
				t.Errorf("a refused run changed the tree from %q to %q", before, after)
			}
		})
	}
}
