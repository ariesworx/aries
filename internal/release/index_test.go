package release_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/release"
)

// goldenIndex is index.json exactly as Encode writes it. install.sh reads
// this format too, so a change here is a change there.
const goldenIndex = `{
  "format": 1,
  "releases": [
    {"tag": "v0.9.0", "prerelease": false, "published": "2026-01-02T03:04:05Z"},
    {"tag": "v0.10.0-rc.1", "prerelease": true, "published": "2026-02-02T03:04:05Z"},
    {"tag": "v0.10.0", "prerelease": false, "published": "2026-03-02T03:04:05Z"}
  ]
}
`

func TestIndexEncode(t *testing.T) {
	t.Parallel()
	var x release.Index
	// Added out of order, and from another time zone: the file is the same.
	for _, e := range []struct{ tag, at string }{
		{"v0.10.0", "2026-03-02T03:04:05Z"},
		{"v0.9.0", "2026-01-02T04:04:05+01:00"},
		{"v0.10.0-rc.1", "2026-02-02T03:04:05Z"},
	} {
		at, err := time.Parse(time.RFC3339, e.at)
		if err != nil {
			t.Fatal(err)
		}
		if err := x.Put(e.tag, at); err != nil {
			t.Fatalf("Put(%s) = %v", e.tag, err)
		}
	}
	got, err := x.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != goldenIndex {
		t.Errorf("Encode() =\n%s\nwant\n%s", got, goldenIndex)
	}

	// What Encode writes, ParseIndex reads back to the same bytes.
	back, err := release.ParseIndex(got)
	if err != nil {
		t.Fatalf("ParseIndex(Encode()) = %v", err)
	}
	again, err := back.Encode()
	if err != nil || string(again) != goldenIndex {
		t.Errorf("Encode() after ParseIndex =\n%s (%v)\nwant\n%s", again, err, goldenIndex)
	}

	// Putting a tag again replaces its entry.
	if err := back.Put("v0.9.0", time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if n := len(back.Releases); n != 3 || back.Releases[0].Published != "2026-01-03T00:00:00Z" {
		t.Errorf("after Put of a listed tag: %d entries, first %+v; want 3 and the new time", n, back.Releases[0])
	}
	if err := back.Put("v1.0.0/../x", time.Now()); err == nil {
		t.Error("Put accepted a tag that is not a release version")
	}
}

func TestIndexLatest(t *testing.T) {
	t.Parallel()
	entry := func(tag string, pre bool) string {
		return fmt.Sprintf(`{"tag": %q, "prerelease": %v, "published": "2026-01-02T03:04:05Z"}`, tag, pre)
	}
	index := func(entries ...string) string {
		return `{"format": 1, "releases": [` + strings.Join(entries, ",") + `]}`
	}
	tests := []struct {
		name string
		in   string
		want string // "" for none
	}{
		{name: "newest last", in: index(entry("v0.2.0", false), entry("v0.3.0", false)), want: "v0.3.0"},
		{name: "order in the file does not matter", in: index(entry("v0.3.0", false), entry("v0.2.0", false)), want: "v0.3.0"},
		{name: "by number, not by text", in: index(entry("v0.10.0", false), entry("v0.9.0", false)), want: "v0.10.0"},
		{name: "a prerelease is never the latest", in: index(entry("v0.3.0", false), entry("v0.4.0-rc.1", true)), want: "v0.3.0"},
		{name: "only prereleases", in: index(entry("v0.4.0-rc.1", true)), want: ""},
		{name: "empty", in: index(), want: ""},
		{name: "a listed tag twice", in: index(entry("v0.3.0", false), entry("v0.3.0", false)), want: "v0.3.0"},
		{name: "fields this build does not know are ignored",
			in:   `{"format": 1, "note": "x", "releases": [{"tag": "v0.3.0", "prerelease": false, "published": "2026-01-02T03:04:05Z", "yanked": false}]}`,
			want: "v0.3.0"},
		{name: "no published time", in: `{"format": 1, "releases": [{"tag": "v0.3.0"}]}`, want: "v0.3.0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			x, err := release.ParseIndex([]byte(tc.in))
			if err != nil {
				t.Fatalf("ParseIndex(%s) = %v", tc.in, err)
			}
			got, ok := x.Latest()
			if got != tc.want || ok != (tc.want != "") {
				t.Errorf("ParseIndex(%s).Latest() = %q, %v, want %q", tc.in, got, ok, tc.want)
			}
		})
	}
}

// An index is read from a host that is not trusted. Whatever it holds
// that is not a list of release tags is refused whole, before any tag is
// put in a URL.
func TestParseIndexRefuses(t *testing.T) {
	t.Parallel()
	one := func(tag string) string {
		return fmt.Sprintf(`{"format": 1, "releases": [{"tag": %q, "prerelease": false}]}`, tag)
	}
	many := func(n int) string {
		var b strings.Builder
		b.WriteString(`{"format":1,"releases":[`)
		for i := range n {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"tag":"v0.0.%d"}`, i)
		}
		b.WriteString(`]}`)
		return b.String()
	}
	tests := []struct {
		name string
		in   string
		want string // in the error
	}{
		{name: "empty file", in: "", want: "index.json"},
		{name: "not JSON", in: "<html>not found</html>", want: "index.json"},
		{name: "a list, not an object", in: `[{"tag": "v0.3.0"}]`, want: "index.json"},
		{name: "two documents", in: one("v0.3.0") + one("v0.4.0"), want: "index.json"},
		{name: "no format", in: `{"releases": [{"tag": "v0.3.0"}]}`, want: "format 0"},
		{name: "a later format", in: `{"format": 2, "releases": [{"tag": "v0.3.0"}]}`, want: "format 2"},
		{name: "tag climbs out of the tree", in: one("../../etc/passwd"), want: "not of the form"},
		{name: "tag with a path after a version", in: one("v1.0.0/../../x"), want: "not of the form"},
		{name: "tag is a URL", in: one("https://evil.example.com/v1.0.0"), want: "not of the form"},
		{name: "tag with a query", in: one("v1.0.0?x=1"), want: "not of the form"},
		{name: "tag with a newline", in: one("v1.0.0\nv2.0.0"), want: "not of the form"},
		{name: "tag with a leading zero", in: one("v01.0.0"), want: "not of the form"},
		{name: "empty tag", in: one(""), want: "not of the form"},
		{name: "latest", in: one("latest"), want: "not of the form"},
		{name: "tag of 20 digits", in: one("v" + strings.Repeat("9", 20) + ".0.0"), want: "not of the form"},
		{name: "huge tag", in: one("v1.0.0-" + strings.Repeat("a", 100_000)), want: "not of the form"},
		{name: "one bad tag among good ones",
			in:   `{"format": 1, "releases": [{"tag": "v0.3.0"}, {"tag": "v0.4.0/.."}]}`,
			want: "not of the form"},
		{name: "a prerelease said to be a release",
			in:   `{"format": 1, "releases": [{"tag": "v0.4.0-rc.1", "prerelease": false}]}`,
			want: "prerelease"},
		{name: "a release said to be a prerelease",
			in:   `{"format": 1, "releases": [{"tag": "v0.4.0", "prerelease": true}]}`,
			want: "prerelease"},
		{name: "more entries than the cap", in: many(release.MaxIndexEntries + 1), want: "more than"},
		{name: "more bytes than the cap", in: one("v0.3.0") + strings.Repeat(" ", release.MaxIndexBytes), want: "larger than"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			x, err := release.ParseIndex([]byte(tc.in))
			if err == nil {
				t.Fatalf("ParseIndex(%.80q) = %+v, want an error", tc.in, x)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ParseIndex(%.80q) = %v, want an error holding %q", tc.in, err, tc.want)
			}
			if len(err.Error()) > 400 {
				t.Errorf("ParseIndex(%.80q): the error is %d bytes long; it repeats what the host sent", tc.in, len(err.Error()))
			}
		})
	}

	// At the cap, an index is read.
	x, err := release.ParseIndex([]byte(many(release.MaxIndexEntries)))
	if err != nil || len(x.Releases) != release.MaxIndexEntries {
		t.Errorf("ParseIndex(%d entries) = %v, want it read", release.MaxIndexEntries, err)
	}
}

// Encode writes nothing ParseIndex would refuse.
func TestIndexEncodeRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		x    release.Index
	}{
		{name: "a tag that is not a version", x: release.Index{Releases: []release.IndexEntry{{Tag: "v1", Published: "2026-01-02T03:04:05Z"}}}},
		{name: "a time that is not RFC 3339", x: release.Index{Releases: []release.IndexEntry{{Tag: "v1.0.0", Published: "yesterday"}}}},
		{name: "a prerelease said to be a release", x: release.Index{Releases: []release.IndexEntry{{Tag: "v1.0.0-rc.1", Published: "2026-01-02T03:04:05Z"}}}},
		{name: "more entries than the cap", x: release.Index{Releases: make([]release.IndexEntry, release.MaxIndexEntries+1)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if b, err := tc.x.Encode(); err == nil {
				t.Errorf("Encode() = %.80q, want an error", b)
			}
		})
	}
}

func TestValidTag(t *testing.T) {
	t.Parallel()
	tests := []struct {
		tag  string
		want bool
	}{
		{"v0.3.0", true},
		{"v10.20.30", true},
		{"v0.3.0-rc.1", true},
		{"v123456789.0.0", true},
		{"v1234567890.0.0", false}, // ten digits: more than any version orders
		{"0.3.0", false},
		{"v0.3", false},
		{"v0.3.0+build", false},
		{"v0.3.0-", false},
		{"v0.3.0-rc_1", false},
		{"v0.3.0-" + strings.Repeat("a", 33), false},
		{"v0.3.0/", false},
		{"v0.3.0/..", false},
		{"../v0.3.0", false},
		{"v0.3.0\x00", false},
		{"v0.3.0 ", false},
		{"V0.3.0", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := release.ValidTag(tc.tag); got != tc.want {
			t.Errorf("ValidTag(%q) = %v, want %v", tc.tag, got, tc.want)
		}
	}
}

// FuzzParseIndex holds what a caller relies on: an index that is read
// names only valid tags, within the cap, and its latest release is one
// of them and no prerelease.
func FuzzParseIndex(f *testing.F) {
	f.Add([]byte(goldenIndex))
	f.Add([]byte(`{"format":1,"releases":[]}`))
	f.Add([]byte(`{"format":1,"releases":[{"tag":"../x"}]}`))
	f.Add([]byte(`{"format":1,"releases":[{"tag":"v1.0.0","published":"x"}]}`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, b []byte) {
		x, err := release.ParseIndex(b)
		if err != nil {
			return
		}
		if len(x.Releases) > release.MaxIndexEntries {
			t.Fatalf("ParseIndex(%q) read %d entries, over the cap", b, len(x.Releases))
		}
		for _, e := range x.Releases {
			if !release.ValidTag(e.Tag) {
				t.Fatalf("ParseIndex(%q) read tag %q", b, e.Tag)
			}
		}
		if tag, ok := x.Latest(); ok && (!release.ValidTag(tag) || strings.Contains(tag, "-")) {
			t.Fatalf("ParseIndex(%q).Latest() = %q", b, tag)
		}
	})
}
