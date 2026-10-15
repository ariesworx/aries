// Command releaseindex lays a release out for a static file host, the
// layout sfx upgrade, starfixd upgrade and install.sh read (RELEASING.md).
// It is not part of sfx or starfixd.
//
//	releaseindex [--published TIME] TAG SRC DEST
//
// SRC holds one release as the release workflow leaves it: the archives,
// checksums.txt and checksums.txt.sig. The command copies them to
// DEST/TAG/ and adds TAG to DEST/index.json, which it writes the same,
// byte for byte, for the same releases. DEST is then what the host
// serves; copy it there with any tool that copies files.
//
// It signs nothing and needs no secret. Before it writes anything it
// checks what a client would check, so that a tree is never published
// that clients would refuse: the signature on checksums.txt, against the
// public keys in keys.go, and every archive's sha256 against
// checksums.txt.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/release"
)

const usage = `usage:
  releaseindex [--published TIME] TAG SRC DEST

  TAG   the release's tag, vMAJOR.MINOR.PATCH
  SRC   the directory holding its archives, checksums.txt and checksums.txt.sig
  DEST  the root of the static tree; made when it does not exist

  --published TIME  when the release was published, as RFC 3339
                    (default: the time index.json already gives it, or now)`

// env is what run needs from the process, so tests can supply their own.
type env struct {
	stdout, stderr io.Writer
	// now is the published time of a release that has none.
	now func() time.Time
	// keys returns the trusted public keys. Default release.Keys.
	keys func() ([]ed25519.PublicKey, error)
}

// usageError is a mistake in the command line: run prints the usage after
// it and exits 2.
type usageError string

func (e usageError) Error() string { return string(e) }

func main() {
	os.Exit(run(os.Args[1:], env{stdout: os.Stdout, stderr: os.Stderr, now: time.Now, keys: release.Keys}))
}

// run runs the command in args and returns the exit code: 0 on success,
// 1 on failure and 2 on a usage error.
func run(args []string, e env) int {
	err := layout(args, e)
	if err == nil {
		return 0
	}
	_, _ = fmt.Fprintf(e.stderr, "releaseindex: %v\n", err)
	if _, ok := errors.AsType[usageError](err); ok {
		_, _ = fmt.Fprintln(e.stderr, usage)
		return 2
	}
	return 1
}

// file is one file of the release: where it is now, and its sha256.
type file struct {
	name string
	sum  []byte
}

// layout checks the release in SRC, then copies it into DEST and lists it
// in DEST's index. Nothing is written until everything has been checked.
func layout(args []string, e env) error {
	fl := flag.NewFlagSet("releaseindex", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	at := fl.String("published", "", "when the release was published, as RFC 3339")
	if err := fl.Parse(args); err != nil {
		return usageError(err.Error())
	}
	if fl.NArg() != 3 {
		return usageError("want TAG, SRC and DEST")
	}
	tag, src, dest := fl.Arg(0), fl.Arg(1), fl.Arg(2)
	if !release.ValidTag(tag) {
		return usageError(fmt.Sprintf("tag %q is not of the form vMAJOR.MINOR.PATCH", tag))
	}
	var published time.Time
	if *at != "" {
		var err error
		if published, err = time.Parse(time.RFC3339, *at); err != nil {
			return usageError(fmt.Sprintf("--published %q is not an RFC 3339 time, such as 2026-01-02T03:04:05Z", *at))
		}
	}

	files, err := check(tag, src, e)
	if err != nil {
		return err
	}
	dir := filepath.Join(dest, tag)
	var missing []file
	for _, f := range files {
		switch got, err := sum(filepath.Join(dir, f.name)); {
		case errors.Is(err, fs.ErrNotExist):
			missing = append(missing, f)
		case err != nil:
			return err
		case !bytes.Equal(got, f.sum):
			return fmt.Errorf("%s already holds a different %s, and a published release does not change; fix: remove %s if this release was never published, or tag a new one",
				dir, f.name, dir)
		}
	}
	index, err := indexWith(dest, tag, published, e.now)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // a tree anyone may read
		return err
	}
	for _, f := range missing {
		if err := copyFile(filepath.Join(src, f.name), filepath.Join(dir, f.name)); err != nil {
			return err
		}
	}
	// The index last: it never names a release whose files are not there.
	if err := writeFile(filepath.Join(dest, release.IndexName), index); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(e.stdout, "%s: %d files (%d new); %s lists %s\n", dir, len(files), len(missing), filepath.Join(dest, release.IndexName), tag)
	return nil
}

// check verifies the release in src as a client would, and returns its
// files: every archive checksums.txt lists, then checksums.txt and its
// signature.
func check(tag, src string, e env) ([]file, error) {
	sums, err := os.ReadFile(filepath.Join(src, release.ChecksumsName)) //nolint:gosec // the maintainer names the directory
	if err != nil {
		return nil, fmt.Errorf("%w; fix: name the directory that holds the release's files", err)
	}
	sig, err := os.ReadFile(filepath.Join(src, release.SignatureName)) //nolint:gosec // the maintainer names the directory
	if err != nil {
		return nil, fmt.Errorf("%w; fix: sign %s first (releasekey sign); this command signs nothing", err, release.ChecksumsName)
	}
	keys, err := e.keys()
	if err != nil {
		return nil, err
	}
	if err := (release.Ed25519{Keys: keys}).Verify(context.Background(), sums, sig, tag); err != nil {
		return nil, fmt.Errorf("%s: %w; fix: clients would refuse this release, so it is not laid out; sign it with a key in internal/release/keys.go",
			filepath.Join(src, release.SignatureName), err)
	}

	// An archive of this release: <bin>_<version>_<os>_<arch> and nothing
	// that could name another directory.
	archive := regexp.MustCompile(`^[a-z0-9]+_` + regexp.QuoteMeta(strings.TrimPrefix(tag, "v")) + `_[a-z0-9]+_[a-z0-9]+\.(tar\.gz|zip)$`)
	var files []file
	for line := range strings.Lines(string(sums)) {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) != 2 {
			return nil, fmt.Errorf("%s: %q is not a sha256 and a file name", release.ChecksumsName, strings.TrimSpace(line))
		}
		name := strings.TrimPrefix(f[1], "*")
		if !archive.MatchString(name) {
			return nil, fmt.Errorf("%s lists %q, which is not a file of release %s; fix: name the tag these files were built for", release.ChecksumsName, name, tag)
		}
		want, err := hex.DecodeString(f[0])
		if err != nil || len(want) != sha256.Size {
			return nil, fmt.Errorf("%s: bad sha256 for %s", release.ChecksumsName, name)
		}
		got, err := sum(filepath.Join(src, name))
		if err != nil {
			return nil, fmt.Errorf("%w; fix: %s lists it, so the release is not whole without it", err, release.ChecksumsName)
		}
		if !bytes.Equal(got, want) {
			return nil, fmt.Errorf("%s: sha256 is %x, and %s lists %x", name, got, release.ChecksumsName, want)
		}
		files = append(files, file{name: name, sum: got})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%s lists no archive", filepath.Join(src, release.ChecksumsName))
	}
	for _, f := range []struct {
		name string
		body []byte
	}{{release.ChecksumsName, sums}, {release.SignatureName, sig}} {
		h := sha256.Sum256(f.body)
		files = append(files, file{name: f.name, sum: h[:]})
	}
	return files, nil
}

// indexWith returns dest's index.json with tag added: published at the
// time given, or else at the time the index already has for it, or else
// now. A release laid out twice therefore keeps its time.
func indexWith(dest, tag string, published time.Time, now func() time.Time) ([]byte, error) {
	x := &release.Index{}
	path := filepath.Join(dest, release.IndexName)
	switch b, err := os.ReadFile(path); { //nolint:gosec // the maintainer names the tree
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if x, err = release.ParseIndex(b); err != nil {
			return nil, fmt.Errorf("%s: %w; fix: restore the tree's index before adding to it", path, err)
		}
	}
	if published.IsZero() {
		published = now()
		for _, r := range x.Releases {
			if r.Tag != tag {
				continue
			}
			var err error
			if published, err = time.Parse(time.RFC3339, r.Published); err != nil {
				return nil, fmt.Errorf("%s: release %s: published %q is not an RFC 3339 time; fix: pass --published", path, tag, r.Published)
			}
		}
	}
	if err := x.Put(tag, published); err != nil {
		return nil, err
	}
	return x.Encode()
}

// sum returns the sha256 of the file at path.
func sum(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // the maintainer names the directories
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return h.Sum(nil), nil
}

// copyFile copies src to dst through a temporary file beside dst, so dst
// is whole or absent.
func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // the maintainer names the directories
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	return replace(dst, func(w io.Writer) error {
		_, err := io.Copy(w, in)
		return err
	})
}

// writeFile writes b to path, whole or not at all.
func writeFile(path string, b []byte) error {
	return replace(path, func(w io.Writer) error {
		_, err := w.Write(b)
		return err
	})
}

// replace writes path through a temporary file beside it, mode 0644.
func replace(path string, write func(io.Writer) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	err = errors.Join(write(tmp), tmp.Sync(), tmp.Close(), os.Chmod(tmp.Name(), 0o644)) //nolint:gosec // a tree anyone may read
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name()) //nolint:gosec // the temporary file replace just made
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
