package release_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ariesworx/starfix/internal/release"
	"github.com/ariesworx/starfix/internal/release/releasetest"
)

// newTree is a tree of three releases: v1.1.0, v1.2.0 with sfx for linux
// and windows, and the prerelease v1.3.0-rc.1.
func newTree(t *testing.T) *releasetest.Tree {
	t.Helper()
	tr := releasetest.NewTree(t)
	tr.AddBinary(t, "v1.1.0", "sfx", "linux", "amd64", []byte("sfx v1.1.0"))
	tr.AddBinary(t, "v1.2.0", "sfx", "linux", "amd64", []byte("sfx v1.2.0"))
	tr.AddBinary(t, "v1.2.0", "sfx", "windows", "arm64", []byte("sfx.exe v1.2.0"))
	tr.AddBinary(t, "v1.3.0-rc.1", "sfx", "linux", "amd64", []byte("sfx v1.3.0-rc.1"))
	return tr
}

// fileURL is the file:// URL of the absolute path dir.
func fileURL(dir string) string {
	p := filepath.ToSlash(dir)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // C:/x on Windows
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

// The static source finds and fetches releases over loopback HTTP, from a
// directory and from a file:// URL, with the same checks over each.
func TestStatic(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	dir := t.TempDir()
	tr.WriteDir(t, dir)
	for _, src := range []struct{ name, location string }{
		{"http", tr.URL},
		{"http with a trailing slash", tr.URL + "/"},
		{"directory", dir},
		{"file URL", fileURL(dir)},
	} {
		t.Run(src.name, func(t *testing.T) {
			c, err := release.Open(src.location, tr.Verifier())
			if err != nil {
				t.Fatalf("Open(%q) = %v", src.location, err)
			}
			rel, err := c.Latest(t.Context())
			if err != nil || rel.Tag != "v1.2.0" {
				t.Fatalf("Latest() = %+v, %v, want v1.2.0: the newest that is no prerelease", rel, err)
			}
			for _, f := range []struct{ goos, goarch, want string }{
				{"linux", "amd64", "sfx v1.2.0"},
				{"windows", "arm64", "sfx.exe v1.2.0"},
			} {
				got, err := c.Fetch(t.Context(), rel, "sfx", f.goos, f.goarch)
				if err != nil || string(got) != f.want {
					t.Errorf("Fetch(v1.2.0, sfx, %s, %s) = %q, %v, want %q", f.goos, f.goarch, got, err, f.want)
				}
			}
			if _, err := c.Fetch(t.Context(), rel, "sfx", "darwin", "arm64"); !errors.Is(err, release.ErrNotFound) {
				t.Errorf("Fetch(v1.2.0, sfx, darwin, arm64) = %v, want ErrNotFound: the release has no such build", err)
			}

			pre, err := c.ByTag(t.Context(), "v1.3.0-rc.1")
			if err != nil || pre.Tag != "v1.3.0-rc.1" {
				t.Fatalf("ByTag(v1.3.0-rc.1) = %+v, %v", pre, err)
			}
			if got, err := c.Fetch(t.Context(), pre, "sfx", "linux", "amd64"); err != nil || string(got) != "sfx v1.3.0-rc.1" {
				t.Errorf("Fetch(v1.3.0-rc.1) = %q, %v", got, err)
			}
			if _, err := c.ByTag(t.Context(), "v9.9.9"); !errors.Is(err, release.ErrNotFound) {
				t.Errorf("ByTag(v9.9.9) = %v, want ErrNotFound", err)
			}
			if _, err := c.ByTag(t.Context(), "v1.2.0/../v1.1.0"); err == nil || !strings.Contains(err.Error(), "not of the form") {
				t.Errorf("ByTag(v1.2.0/../v1.1.0) = %v, want a refusal", err)
			}
		})
	}
}

// ByTag does not need the index: a mirror may hold one release and an
// index that is missing or broken.
func TestStaticByTagWithoutIndex(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	tr.Delete(release.IndexName)
	c := tr.Client(t, tr.Verifier())
	rel, err := c.ByTag(t.Context(), "v1.1.0")
	if err != nil {
		t.Fatalf("ByTag(v1.1.0) with no index.json = %v", err)
	}
	if got, err := c.Fetch(t.Context(), rel, "sfx", "linux", "amd64"); err != nil || string(got) != "sfx v1.1.0" {
		t.Errorf("Fetch(v1.1.0) = %q, %v", got, err)
	}
	if _, err := c.Latest(t.Context()); !errors.Is(err, release.ErrNotFound) {
		t.Errorf("Latest() with no index.json = %v, want ErrNotFound", err)
	}
}

func TestOpen(t *testing.T) {
	t.Parallel()
	abs := t.TempDir()
	tests := []struct {
		name     string
		location string
		want     string // Location(); "" when refused
		refusal  string // in the error
	}{
		{name: "github by name", location: "github", want: "https://api.github.com"},
		{name: "https", location: "https://releases.example.com/starfix", want: "https://releases.example.com/starfix"},
		{name: "https with a trailing slash", location: "https://releases.example.com/starfix/", want: "https://releases.example.com/starfix"},
		{name: "https with a port", location: "https://releases.example.com:8443", want: "https://releases.example.com:8443"},
		{name: "http to loopback", location: "http://127.0.0.1:8080/mirror", want: "http://127.0.0.1:8080/mirror"},
		{name: "http to IPv6 loopback", location: "http://[::1]:8080", want: "http://[::1]:8080"},
		{name: "directory", location: abs, want: abs},
		{name: "file URL", location: fileURL(abs), want: abs},
		{name: "http to a host", location: "http://releases.example.com/starfix", refusal: "https"},
		{name: "http to a name that says loopback", location: "http://127.0.0.1.example.com/", refusal: "https"},
		{name: "http to localhost by name", location: "http://localhost:8080/", refusal: "https"},
		{name: "another scheme", location: "ftp://releases.example.com/starfix", refusal: "https"},
		{name: "no scheme", location: "releases.example.com/starfix", refusal: "https"},
		{name: "relative directory", location: "mirror/starfix", refusal: "absolute"},
		{name: "https with no host", location: "https:///starfix", refusal: "host"},
		{name: "a password in the URL", location: "https://alice:hunter2@releases.example.com/", refusal: "user name or password"}, //nolint:gosec // an invented password, which the refusal must not repeat
		{name: "a query", location: "https://releases.example.com/?token=hunter2", refusal: "query"},
		{name: "a fragment", location: "https://releases.example.com/#x", refusal: "query"},
		{name: "file URL on another host", location: "file://fileserver.example.com/mirror", refusal: "host"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := release.Open(tc.location, nil)
			if tc.refusal != "" {
				if err == nil {
					t.Fatalf("Open(%q) accepted, reading from %s", tc.location, c.Location())
				}
				if !strings.Contains(err.Error(), tc.refusal) {
					t.Errorf("Open(%q) = %v, want an error holding %q", tc.location, err, tc.refusal)
				}
				// A location may hold a credential; the refusal never repeats it.
				if strings.Contains(err.Error(), "hunter2") {
					t.Errorf("Open(%q) = %v, which repeats the secret", tc.location, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Open(%q) = %v", tc.location, err)
			}
			if got := c.Location(); got != tc.want {
				t.Errorf("Open(%q).Location() = %q, want %q", tc.location, got, tc.want)
			}
		})
	}
}

// TestDefaultSource pins the one line that says where releases come
// from. Changing release.DefaultSource to the bucket's URL moves sfx and
// starfixd; install.sh has its own copy, which must say the same.
func TestDefaultSource(t *testing.T) {
	t.Parallel()
	def, err := release.Open(release.DefaultSource, nil)
	if err != nil {
		t.Fatalf("Open(DefaultSource = %q) = %v", release.DefaultSource, err)
	}
	unset, err := release.Open("", nil)
	if err != nil {
		t.Fatalf(`Open("") = %v`, err)
	}
	if unset.Location() != def.Location() {
		t.Errorf(`Open("").Location() = %q, want DefaultSource's, %q`, unset.Location(), def.Location())
	}
	if release.DefaultSource != release.SourceGitHub && !strings.HasPrefix(release.DefaultSource, "https://") {
		t.Errorf("DefaultSource = %q, want %q or an https URL: a build must not default to a file or to plain http",
			release.DefaultSource, release.SourceGitHub)
	}

	src, err := os.ReadFile(installScript)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^DEFAULT_RELEASES=(\S+)$`).FindSubmatch(src)
	if m == nil {
		t.Fatal("install.sh has no DEFAULT_RELEASES= line")
	}
	if got := string(m[1]); got != release.DefaultSource {
		t.Errorf("install.sh DEFAULT_RELEASES=%s, want release.DefaultSource, %s", got, release.DefaultSource)
	}
}

// What a hostile or broken host can do with a static tree. The host is
// transport: in every case the client refuses, or is handed a release
// that the release key signed and that it asked for.
func TestStaticHostileHost(t *testing.T) {
	t.Parallel()
	// elsewhere stands for wherever a redirect points. Nothing may ask it
	// for anything.
	var asked atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		_, _ = w.Write([]byte(`{"format": 1, "releases": []}`))
	}))
	t.Cleanup(elsewhere.Close)
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

	tests := []struct {
		name string
		edit func(t *testing.T, tr *releasetest.Tree)
		// tag is the release asked for by tag; "" asks for the latest.
		tag string
		// wantLatest is the tag Latest returns; "" when Latest must fail.
		wantLatest string
		// wantErr and wantText are what the failing step returns.
		wantErr  error
		wantText string
		// only lists the only paths the host may have been asked for.
		only []string
	}{
		{name: "index names only an older signed release",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "v1.1.0"}]}`)
			},
			// It is a real release, and Latest says so. Whether to install
			// it is the caller's rule: never one older than what runs.
			wantLatest: "v1.1.0"},
		{name: "an older signed release in a newer one's directory",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.Move("v1.1.0", "v9.9.9")
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "v9.9.9"}]}`)
			},
			wantLatest: "v9.9.9", wantErr: release.ErrNotFound, wantText: "does not list sfx_9.9.9_linux_amd64.tar.gz"},
		{name: "an older release's archive under the new name too",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.Move("v1.1.0", "v9.9.9")
				tr.Set("v9.9.9/sfx_9.9.9_linux_amd64.tar.gz", tr.File("v1.1.0/sfx_1.1.0_linux_amd64.tar.gz"))
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "v9.9.9"}]}`)
			},
			wantLatest: "v9.9.9", wantErr: release.ErrNotFound, wantText: "does not list"},
		{name: "index names a tag that climbs out of the tree",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "../../v1.2.0"}]}`)
			},
			wantText: "not of the form", only: []string{"/index.json"}},
		{name: "index names a URL",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "https://evil.example.com/v1.2.0"}]}`)
			},
			wantText: "not of the form", only: []string{"/index.json"}},
		{name: "index names a huge tag",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "v1.2.0-` + strings.Repeat("a", 500_000) + `"}]}`)
			},
			wantText: "not of the form", only: []string{"/index.json"}},
		{name: "huge index",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "v1.2.0"}]}` + strings.Repeat(" ", release.MaxIndexBytes))
			},
			wantText: "larger than", only: []string{"/index.json"}},
		{name: "index that never ends",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.Handle(func(w http.ResponseWriter, r *http.Request) bool {
					if r.URL.Path != "/index.json" {
						return false
					}
					chunk := []byte(strings.Repeat(" ", 64<<10))
					for range 64 { // 4 MiB, four times the cap
						if _, err := w.Write(chunk); err != nil {
							break
						}
					}
					return true
				})
			},
			wantText: "larger than", only: []string{"/index.json"}},
		{name: "malformed index",
			edit:     func(_ *testing.T, tr *releasetest.Tree) { tr.SetIndex(`{"format": 1, "releases": [`) },
			wantText: "index.json", only: []string{"/index.json"}},
		{name: "an error page as the index",
			edit:     func(_ *testing.T, tr *releasetest.Tree) { tr.SetIndex("<html>Service unavailable</html>") },
			wantText: "index.json", only: []string{"/index.json"}},
		{name: "prerelease served as the latest release",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "v1.2.0"}, {"tag": "v1.3.0-rc.1", "prerelease": false}]}`)
			},
			wantText: "prerelease", only: []string{"/index.json"}},
		{name: "prerelease listed as one is passed over",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "v1.2.0"}, {"tag": "v1.3.0-rc.1", "prerelease": true}]}`)
			},
			wantLatest: "v1.2.0"},
		{name: "index redirects elsewhere", edit: redirect("/index.json"),
			wantText: "redirect", only: []string{"/index.json"}},
		{name: "checksums redirect elsewhere", edit: redirect("/checksums.txt"),
			wantLatest: "v1.2.0", wantText: "redirect"},
		{name: "signature redirects elsewhere", edit: redirect("/checksums.txt.sig"),
			wantLatest: "v1.2.0", wantText: "redirect"},
		{name: "archive redirects elsewhere", edit: redirect(".tar.gz"),
			wantLatest: "v1.2.0", wantText: "redirect"},
		{name: "release by tag redirects elsewhere", edit: redirect("/checksums.txt"), tag: "v1.2.0",
			wantText: "redirect", only: []string{"/v1.2.0/checksums.txt"}},
		{name: "archive changed after signing",
			edit: func(t *testing.T, tr *releasetest.Tree) {
				tr.Set("v1.2.0/sfx_1.2.0_linux_amd64.tar.gz", releasetest.TarGz(t, "sfx", []byte("evil")))
			},
			wantLatest: "v1.2.0", wantErr: release.ErrMismatch},
		{name: "checksums changed to match a changed archive",
			edit: func(t *testing.T, tr *releasetest.Tree) {
				sig := tr.File("v1.2.0/checksums.txt.sig")
				tr.Set("v1.2.0/sfx_1.2.0_linux_amd64.tar.gz", releasetest.TarGz(t, "sfx", []byte("evil")))
				tr.Sign("v1.2.0")
				tr.Set("v1.2.0/checksums.txt.sig", sig)
			},
			wantLatest: "v1.2.0", wantErr: release.ErrSignature},
		{name: "signed by the host's own key",
			edit: func(t *testing.T, tr *releasetest.Tree) {
				_, other, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				tr.Set("v1.2.0/sfx_1.2.0_linux_amd64.tar.gz", releasetest.TarGz(t, "sfx", []byte("evil")))
				tr.Key = other
				tr.Sign("v1.2.0")
			},
			wantLatest: "v1.2.0", wantErr: release.ErrSignature},
		{name: "no signature",
			edit:       func(_ *testing.T, tr *releasetest.Tree) { tr.Delete("v1.2.0/checksums.txt.sig") },
			wantLatest: "v1.2.0", wantErr: release.ErrSignature, wantText: "is not signed"},
		{name: "huge checksums",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.Set("v1.2.0/checksums.txt", []byte(strings.Repeat("0", 1<<20)))
			},
			wantLatest: "v1.2.0", wantText: "larger than"},
		{name: "the host fails",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.Handle(func(w http.ResponseWriter, _ *http.Request) bool {
					http.Error(w, "boom", http.StatusInternalServerError)
					return true
				})
			},
			wantText: "HTTP 500", only: []string{"/index.json"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTree(t)
			pub := tr.Public // the key the client trusts, whatever the host signs with later
			tc.edit(t, tr)
			c := tr.Client(t, release.Ed25519{Keys: []ed25519.PublicKey{pub}})
			asked.Store(0)
			t.Cleanup(func() {
				if n := asked.Load(); n != 0 {
					t.Errorf("the client followed a redirect: %d requests reached the other host", n)
				}
				if tc.only == nil {
					return
				}
				if got := tr.Requests(); !slices.Equal(got, tc.only) {
					t.Errorf("the host was asked for %q, want only %q", got, tc.only)
				}
			})

			var rel *release.Release
			var err error
			if tc.tag != "" {
				rel, err = c.ByTag(t.Context(), tc.tag)
			} else {
				rel, err = c.Latest(t.Context())
			}
			if tc.wantLatest == "" {
				if err == nil {
					t.Fatalf("release = %+v, want an error holding %q", rel, tc.wantText)
				}
				checkErr(t, err, tc.wantErr, tc.wantText)
				return
			}
			if err != nil || rel.Tag != tc.wantLatest {
				t.Fatalf("Latest() = %+v, %v, want %s", rel, err, tc.wantLatest)
			}
			got, err := c.Fetch(t.Context(), rel, "sfx", "linux", "amd64")
			if tc.wantErr == nil && tc.wantText == "" {
				if want := "sfx " + rel.Tag; err != nil || string(got) != want {
					t.Errorf("Fetch(%s) = %q, %v, want %q", rel.Tag, got, err, want)
				}
				return
			}
			if err == nil {
				t.Fatalf("Fetch(%s) = %q, want an error", rel.Tag, got)
			}
			checkErr(t, err, tc.wantErr, tc.wantText)
		})
	}
}

// checkErr fails unless err is want, when want is set, and holds text.
func checkErr(t *testing.T, err, want error, text string) {
	t.Helper()
	if want != nil && !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
	if !strings.Contains(err.Error(), text) {
		t.Errorf("err = %v, want it to hold %q", err, text)
	}
	if len(err.Error()) > 600 {
		t.Errorf("the error is %d bytes long; it repeats what the host sent", len(err.Error()))
	}
}

// A mirror in a directory is read through the directory alone: a tag
// directory that is a link to somewhere outside it is not followed.
func TestStaticDirectoryConfined(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("making a symbolic link needs a privilege on Windows")
	}
	tr := newTree(t)
	outside := t.TempDir()
	tr.WriteDir(t, outside)
	mirror := t.TempDir()
	if err := os.WriteFile(filepath.Join(mirror, release.IndexName), tr.File(release.IndexName), 0o644); err != nil { //nolint:gosec // a public file
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "v1.2.0"), filepath.Join(mirror, "v1.2.0")); err != nil {
		t.Fatal(err)
	}
	c, err := release.Open(mirror, tr.Verifier())
	if err != nil {
		t.Fatal(err)
	}
	rel, err := c.Latest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := c.Fetch(t.Context(), rel, "sfx", "linux", "amd64"); err == nil {
		t.Errorf("Fetch through a link out of the mirror = %q, want an error", got)
	}
}
