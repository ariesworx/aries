package releasetest

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/release"
)

// Tree is a static release tree, the layout RELEASING.md describes,
// served over loopback HTTP: index.json at the root, and each release's
// archives, checksums.txt and checksums.txt.sig in a directory named by
// its tag. Every release is signed with Key, one key made for the Tree.
//
// A test plays a hostile or broken host by changing what is served, file
// by file ([Tree.Set], [Tree.Delete], [Tree.Move], [Tree.SetIndex]), or by
// answering requests itself ([Tree.Handle]). Its methods are safe for
// concurrent use, since what asks the tree may be another process.
type Tree struct {
	// URL is the tree's base, for [release.Open].
	URL string
	// Key signs each release when it is next signed; Public is the public
	// half of the key the Tree was made with.
	Key    ed25519.PrivateKey
	Public ed25519.PublicKey

	mu     sync.Mutex // guards the fields below
	files  map[string][]byte
	handle func(w http.ResponseWriter, r *http.Request) bool
	seen   []string
}

// NewTree starts an empty tree: no releases, and an index.json listing
// none.
func NewTree(t testing.TB) *Tree {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tr := &Tree{files: map[string][]byte{}, Key: priv, Public: pub}
	tr.index(t)
	srv := httptest.NewServer(http.HandlerFunc(tr.serve))
	t.Cleanup(srv.Close)
	tr.URL = srv.URL
	return tr
}

// AddBinary adds the archive of binary bin, holding body, for goos/goarch
// to release tag, which it makes and lists in index.json when new. It
// writes the release's checksums.txt and signature again, and returns
// the archive's name.
func (tr *Tree) AddBinary(t testing.TB, tag, bin, goos, goarch string, body []byte) string {
	t.Helper()
	name := release.ArchiveName(bin, tag, goos, goarch)
	if goos == "windows" {
		tr.Set(tag+"/"+name, Zip(t, bin+".exe", body))
	} else {
		tr.Set(tag+"/"+name, TarGz(t, bin, body))
	}
	tr.Sign(tag)
	tr.index(t)
	return name
}

// Set serves body at path p under the base ("v1.2.0/checksums.txt").
func (tr *Tree) Set(p string, body []byte) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.files[p] = body
}

// Delete stops serving path p.
func (tr *Tree) Delete(p string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	delete(tr.files, p)
}

// File returns what is served at path p, or nil.
func (tr *Tree) File(p string) []byte {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.files[p]
}

// SetIndex serves body as index.json.
func (tr *Tree) SetIndex(body string) {
	tr.Set(release.IndexName, []byte(body))
}

// Handle has h see each request first and report whether it answered.
func (tr *Tree) Handle(h func(w http.ResponseWriter, r *http.Request) bool) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.handle = h
}

// Sign writes release tag's checksums.txt from its archives as they are
// now, and signs it with Key.
func (tr *Tree) Sign(tag string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	var b strings.Builder
	for _, p := range slices.Sorted(maps.Keys(tr.files)) {
		name := path.Base(p)
		if path.Dir(p) != tag || name == release.ChecksumsName || name == release.SignatureName {
			continue
		}
		fmt.Fprintf(&b, "%x  %s\n", sha256.Sum256(tr.files[p]), name)
	}
	tr.files[tag+"/"+release.ChecksumsName] = []byte(b.String())
	tr.files[tag+"/"+release.SignatureName] = release.EncodeSignature(ed25519.Sign(tr.Key, []byte(b.String())))
}

// Move serves release from's files, as they are, in the directory of
// release to as well: what a host does to pass one signed release off as
// another.
func (tr *Tree) Move(from, to string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for p, b := range maps.Clone(tr.files) {
		if path.Dir(p) == from {
			tr.files[to+"/"+path.Base(p)] = b
		}
	}
}

// index writes index.json from the releases the tree holds.
func (tr *Tree) index(t testing.TB) {
	t.Helper()
	tr.mu.Lock()
	defer tr.mu.Unlock()
	var x release.Index
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for p := range tr.files {
		if path.Base(p) != release.ChecksumsName {
			continue
		}
		if err := x.Put(path.Dir(p), at); err != nil {
			t.Fatal(err)
		}
	}
	b, err := x.Encode()
	if err != nil {
		t.Fatal(err)
	}
	tr.files[release.IndexName] = b
}

// Client returns a release client for the tree, verifying with v.
func (tr *Tree) Client(t testing.TB, v release.Verifier) *release.Client {
	t.Helper()
	c, err := release.Open(tr.URL, v)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Verifier is the real Ed25519 verifier, trusting only the key the Tree
// was made with.
func (tr *Tree) Verifier() release.Ed25519 {
	return release.Ed25519{Keys: []ed25519.PublicKey{tr.Public}}
}

// WriteDir writes the tree's files into dir, as a mirror kept in a
// directory.
func (tr *Tree) WriteDir(t testing.TB, dir string) {
	t.Helper()
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for p, b := range tr.files {
		f := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil { //nolint:gosec // a test's directory
			t.Fatal(err)
		}
		if err := os.WriteFile(f, b, 0o644); err != nil { //nolint:gosec // public release files
			t.Fatal(err)
		}
	}
}

// Requests returns the paths asked for so far, in order.
func (tr *Tree) Requests() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return slices.Clone(tr.seen)
}

func (tr *Tree) serve(w http.ResponseWriter, r *http.Request) {
	tr.mu.Lock()
	tr.seen = append(tr.seen, r.URL.Path)
	handle := tr.handle
	b, ok := tr.files[strings.TrimPrefix(r.URL.Path, "/")]
	tr.mu.Unlock()
	if handle != nil && handle(w, r) {
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(b)
}
