package release

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// IndexName is the file at the root of a static release tree that lists
// its releases.
const IndexName = "index.json"

// IndexFormat is the format of index.json this build reads and writes.
// The format only ever gains fields, which older builds ignore; the number
// changes only when older builds must stop reading the file.
const IndexFormat = 1

// The caps on an index.json: it is refused when larger, and the tool that
// writes it refuses to write one a build would not read.
const (
	MaxIndexBytes   = 1 << 20
	MaxIndexEntries = 4096
)

// Index is index.json: the releases a static tree holds. Nothing in it is
// trusted. It only says which tag to ask for; the release behind the tag
// is accepted by its signature and checksum, like any other.
type Index struct {
	Format   int          `json:"format"`
	Releases []IndexEntry `json:"releases"`
}

// IndexEntry is one release in an [Index].
type IndexEntry struct {
	// Tag is the release's tag, which is also its directory in the tree.
	Tag string `json:"tag"`
	// Prerelease is whether Tag has a prerelease part (v1.2.3-rc.1). It
	// repeats the tag for readers that do not parse versions; an index
	// where the two disagree is refused.
	Prerelease bool `json:"prerelease"`
	// Published is when the release was added, in UTC as RFC 3339. It is
	// for people: no build orders releases by it.
	Published string `json:"published"`
}

// ValidTag reports whether tag is a release tag that may name a directory
// of a static tree: it matches [TagPattern] and is a version
// proto.CompareVersions orders, so at most 64 bytes with numbers of at
// most 9 digits.
func ValidTag(tag string) bool {
	return TagPattern.MatchString(tag) && proto.ValidVersion(tag)
}

// prerelease reports whether the valid tag has a prerelease part.
func prerelease(tag string) bool {
	return strings.Contains(tag, "-")
}

// compareTags orders two valid tags by semantic versioning precedence.
func compareTags(a, b string) int {
	c, _ := proto.CompareVersions(a, b)
	return c
}

// ParseIndex reads an index.json, and refuses it whole when it is over a
// cap, of another format, or names anything that is not a release tag:
// a caller may put every tag it returns in a URL or a file name.
func ParseIndex(b []byte) (*Index, error) {
	if len(b) > MaxIndexBytes {
		return nil, fmt.Errorf("%s: larger than %d bytes", IndexName, MaxIndexBytes)
	}
	var x Index
	if err := json.Unmarshal(b, &x); err != nil {
		return nil, fmt.Errorf("%s: %w", IndexName, err)
	}
	if x.Format != IndexFormat {
		return nil, fmt.Errorf("%s: format %d, and this build reads format %d", IndexName, x.Format, IndexFormat)
	}
	if err := x.check(); err != nil {
		return nil, err
	}
	return &x, nil
}

// check refuses an index over the cap on entries, or with an entry whose
// tag is not a release tag or whose prerelease flag contradicts its tag.
func (x *Index) check() error {
	if len(x.Releases) > MaxIndexEntries {
		return fmt.Errorf("%s: more than %d releases", IndexName, MaxIndexEntries)
	}
	for i, e := range x.Releases {
		if !ValidTag(e.Tag) {
			return fmt.Errorf("%s: release %d: tag %q is not of the form vMAJOR.MINOR.PATCH", IndexName, i+1, clip(e.Tag))
		}
		if e.Prerelease != prerelease(e.Tag) {
			return fmt.Errorf("%s: release %s: prerelease is %v, which its tag contradicts", IndexName, e.Tag, e.Prerelease)
		}
	}
	return nil
}

// Latest returns the tag of the newest release that is not a prerelease,
// by version and not by position or time. ok is false when there is none.
func (x *Index) Latest() (tag string, ok bool) {
	for _, e := range x.Releases {
		if prerelease(e.Tag) {
			continue
		}
		if !ok || compareTags(e.Tag, tag) > 0 {
			tag, ok = e.Tag, true
		}
	}
	return tag, ok
}

// Put adds the release tagged tag, published at the given time, or
// replaces its entry, and keeps the entries in version order.
func (x *Index) Put(tag string, published time.Time) error {
	if !ValidTag(tag) {
		return fmt.Errorf("release tag %q is not of the form vMAJOR.MINOR.PATCH", clip(tag))
	}
	e := IndexEntry{Tag: tag, Prerelease: prerelease(tag), Published: published.UTC().Format(time.RFC3339)}
	x.Releases = slices.DeleteFunc(x.Releases, func(o IndexEntry) bool { return o.Tag == tag })
	x.Releases = append(x.Releases, e)
	slices.SortStableFunc(x.Releases, func(a, b IndexEntry) int { return compareTags(a.Tag, b.Tag) })
	x.Format = IndexFormat
	return nil
}

// Encode returns the bytes of index.json: one release to a line, oldest
// first, the same bytes for the same releases in any order. It refuses
// to write what [ParseIndex] would refuse to read, or a published time
// that is not RFC 3339.
func (x *Index) Encode() ([]byte, error) {
	if err := x.check(); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "{\n  \"format\": %d,\n  \"releases\": [", IndexFormat)
	sorted := slices.SortedStableFunc(slices.Values(x.Releases), func(a, b IndexEntry) int { return compareTags(a.Tag, b.Tag) })
	for i, e := range sorted {
		if _, err := time.Parse(time.RFC3339, e.Published); err != nil {
			return nil, fmt.Errorf("%s: release %s: published %q is not an RFC 3339 time", IndexName, e.Tag, clip(e.Published))
		}
		if i > 0 {
			b.WriteByte(',')
		}
		// A valid tag and an RFC 3339 time hold nothing JSON escapes.
		fmt.Fprintf(&b, "\n    {\"tag\": %q, \"prerelease\": %v, \"published\": %q}", e.Tag, e.Prerelease, e.Published)
	}
	if len(sorted) > 0 {
		b.WriteString("\n  ")
	}
	b.WriteString("]\n}\n")
	if b.Len() > MaxIndexBytes {
		return nil, fmt.Errorf("%s: larger than %d bytes", IndexName, MaxIndexBytes)
	}
	return b.Bytes(), nil
}

// clip shortens a string from an untrusted file for an error message.
func clip(s string) string {
	const limit = 80
	if len(s) <= limit {
		return s
	}
	return strings.ToValidUTF8(s[:limit], "") + "..."
}
