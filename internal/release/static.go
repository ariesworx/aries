package release

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// EnvReleases names the environment variable that says where sfx,
// starfixd and install.sh read releases from, in place of
// [DefaultSource]: a location [Open] accepts.
const EnvReleases = "STARFIX_RELEASES"

// SourceGitHub is the location that names GitHub's API for [Repo].
const SourceGitHub = "github"

// DefaultSource is where releases are read from when nothing says
// otherwise: [SourceGitHub], or the base URL of a static tree.
//
// It changes to the release bucket's URL in the release that precedes
// making the repository private (RELEASING.md, "Before the repository is
// made private"). DEFAULT_RELEASES in install.sh changes with it;
// TestDefaultSource fails until the two agree.
const DefaultSource = SourceGitHub

// tree says where a static tree is. Exactly one of base and dir is set.
type tree struct {
	// base is the tree's URL, without a trailing slash.
	base string
	// dir is the tree's directory.
	dir string
}

// static is the source that reads c's static tree: index.json at its
// root, and each release's files in a directory named by its tag.
type static struct{ c *Client }

// Open returns a Client that reads releases from location and verifies
// them with v. The location is "" for [DefaultSource], [SourceGitHub],
// or a static tree: an https URL, a file:// URL, or the absolute path of
// a directory.
//
// A static tree is transport and is not trusted: a release from it is
// accepted on the same signature and checksum as one from GitHub.
func Open(location string, v Verifier) (*Client, error) {
	if location == "" {
		location = DefaultSource
	}
	c := &Client{Verifier: v}
	if location == SourceGitHub {
		return c, nil
	}
	c.static = &tree{}
	// A path first: C:\mirror would otherwise parse as a URL of scheme c.
	if filepath.IsAbs(location) {
		c.static.dir = filepath.Clean(location)
		return c, nil
	}
	// The refusals below never repeat the location, which may hold a
	// credential.
	u, err := url.Parse(location)
	if err != nil {
		return nil, errNotLocation
	}
	switch {
	case u.User != nil:
		return nil, errors.New("release location must not hold a user name or password")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return nil, errors.New("release location must not hold a query or a fragment")
	case u.Scheme == "file":
		if u.Host != "" && u.Host != "localhost" {
			return nil, errors.New("release location is a file:// URL that names a host; it must name a directory on this machine (file:///path)")
		}
		p := filepath.FromSlash(u.Path)
		// file:///C:/mirror parses to /C:/mirror.
		if rest := strings.TrimPrefix(p, string(filepath.Separator)); filepath.VolumeName(rest) != "" {
			p = rest
		}
		if !filepath.IsAbs(p) {
			return nil, errors.New("release location is a file:// URL without an absolute path (file:///path)")
		}
		c.static.dir = filepath.Clean(p)
		return c, nil
	case u.Scheme != "https" && u.Scheme != "http":
		return nil, errNotLocation
	case u.Hostname() == "":
		return nil, errors.New("release location names no host")
	case u.Scheme == "http" && !loopback(u.Hostname()):
		// Plain HTTP reaches only this machine: tests, and a mirror
		// served from a local directory.
		return nil, errNotLocation
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	c.static.base = u.String()
	return c, nil
}

// errNotLocation refuses a location of no accepted kind.
var errNotLocation = errors.New("release location is not an https URL, a file:// URL or the absolute path of a directory " +
	"(plain http is accepted only to a loopback address)")

// loopback reports whether host is a loopback address written as an
// address. A name is not taken at its word, "localhost" included.
func loopback(host string) bool {
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// noRedirect is the redirect policy of a static tree: none is followed,
// so every request goes to the host the location names, over the scheme
// it names. A redirect is an answer that is not 200.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Location says where the Client reads releases from, for a message: the
// GitHub API's root, or the static tree's URL or directory.
func (c *Client) Location() string {
	switch {
	case c.static == nil:
		return c.base()
	case c.static.dir != "":
		return c.static.dir
	}
	return c.static.base
}

// latest reads index.json and returns the newest release it lists that
// is not a prerelease.
func (s static) latest(ctx context.Context) (*Release, error) {
	b, err := s.read(ctx, IndexName, MaxIndexBytes)
	if err != nil {
		return nil, err
	}
	x, err := ParseIndex(b)
	if err != nil {
		return nil, err
	}
	tag, ok := x.Latest()
	if !ok {
		return nil, fmt.Errorf("%s lists no release: %w", IndexName, ErrNotFound)
	}
	return &Release{Tag: tag}, nil
}

// byTag reports the release tagged tag when the tree holds its
// checksums.txt. It does not read index.json, so a mirror of one release
// needs none.
func (s static) byTag(ctx context.Context, tag string) (*Release, error) {
	rel := &Release{Tag: tag}
	if _, err := s.file(ctx, rel, ChecksumsName, maxChecksums); err != nil {
		return nil, fmt.Errorf("release %s: %w", tag, err)
	}
	return rel, nil
}

// has knows nothing until it asks: a static tree lists no files.
func (s static) has(*Release, string) error { return nil }

// file reads name from the directory of rel's tag. It is the one place a
// tag becomes part of a URL or a path, so it is where the tag is checked.
func (s static) file(ctx context.Context, rel *Release, name string, limit int64) ([]byte, error) {
	if !ValidTag(rel.Tag) {
		return nil, fmt.Errorf("release tag %q is not of the form vMAJOR.MINOR.PATCH", clip(rel.Tag))
	}
	b, err := s.read(ctx, rel.Tag+"/"+name, limit)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", name, err)
	}
	return b, nil
}

// read returns the file at the slash-separated path p under the tree's
// root, refusing more than limit bytes. A missing file wraps ErrNotFound.
func (s static) read(ctx context.Context, p string, limit int64) ([]byte, error) {
	t := s.c.static
	if t.dir == "" {
		hc := *s.c.http()
		hc.CheckRedirect = noRedirect
		return get(ctx, &hc, t.base+"/"+p, limit, "")
	}
	// A Root keeps the read inside the directory, whatever links it holds.
	root, err := os.OpenRoot(t.dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(filepath.FromSlash(p))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", filepath.Join(t.dir, filepath.FromSlash(p)), ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s: larger than %d bytes", f.Name(), limit)
	}
	return b, nil
}
