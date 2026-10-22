package merges

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"

	"github.com/ariesworx/aries/internal/lazyregexp"
)

// skipCall is a test's skip as a diff adds it: t.Skip, b.SkipNow,
// tb.Skipf and their kin.
var skipCall = lazyregexp.New(`\.Skip(f|Now)?\(`)

// maxDiffLine is the longest line ParseDiff reads; a longer one fails the
// read, which holds the pull request.
const maxDiffLine = 1 << 20

// ParseDiff reads a unified diff as git writes it (`gh pr diff`): each
// file's paths, both sides of a rename or copy, the lines it loses and
// the t.Skip calls it gains. A path git quoted is unquoted; one that
// cannot be is kept quoted, which the classifier cannot classify and so
// holds.
func ParseDiff(b []byte) ([]Change, error) {
	var out []Change
	var cur *Change
	inHunk := false
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), maxDiffLine)
	for sc.Scan() {
		line := sc.Text()
		if rest, ok := strings.CutPrefix(line, "diff --git "); ok {
			out = append(out, Change{})
			cur, inHunk = &out[len(out)-1], false
			cur.Old, cur.Path = headerPaths(rest)
			continue
		}
		if cur == nil {
			continue
		}
		if inHunk {
			switch {
			case strings.HasPrefix(line, "-"):
				cur.Removed++
			case strings.HasPrefix(line, "+") && skipCall.MatchString(line):
				cur.Skips++
			case strings.HasPrefix(line, " "), strings.HasPrefix(line, "\\"), strings.HasPrefix(line, "@@"), line == "":
			default:
				inHunk = false
			}
			if inHunk {
				continue
			}
		}
		switch {
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case strings.HasPrefix(line, "rename from "), strings.HasPrefix(line, "copy from "):
			_, p, _ := strings.Cut(line, " from ")
			cur.Old = unquote(p)
		case strings.HasPrefix(line, "rename to "), strings.HasPrefix(line, "copy to "):
			_, p, _ := strings.Cut(line, " to ")
			cur.Path = unquote(p)
		case strings.HasPrefix(line, "--- "):
			if p := side(line[4:], "a/"); p != "" {
				cur.Old = p
			}
		case strings.HasPrefix(line, "+++ "):
			if p := side(line[4:], "b/"); p != "" {
				cur.Path = p
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		c := &out[i]
		if c.Path == "" {
			c.Path, c.Old = c.Old, ""
		}
		if c.Old == c.Path {
			c.Old = ""
		}
	}
	return out, nil
}

// side is a ---/+++ line's path without its a/ or b/, or "" for
// /dev/null.
func side(s, prefix string) string {
	s = strings.TrimSuffix(s, "\t")
	if s == "/dev/null" {
		return ""
	}
	p := unquote(s)
	if q, ok := strings.CutPrefix(p, prefix); ok {
		return q
	}
	return p
}

// headerPaths reads "a/X b/Y" from a diff --git line. Only an unquoted
// header with both sides alike can be split for certain; otherwise the
// ---, +++ and rename lines that follow name the paths, and with none of
// them the header is kept whole, quoted, so that it holds.
func headerPaths(rest string) (old, path string) {
	if strings.HasPrefix(rest, `"`) {
		return "", strconv.Quote(rest)
	}
	if n := len(rest); n%2 == 1 {
		a, b := rest[:n/2], rest[n/2+1:]
		if strings.HasPrefix(a, "a/") && strings.HasPrefix(b, "b/") && a[2:] == b[2:] {
			return "", a[2:]
		}
	}
	return "", strconv.Quote(rest)
}

// unquote reads a path git quoted (C-style, in double quotes), or
// returns it as is; a quoted path that does not unquote stays quoted.
func unquote(s string) string {
	if !strings.HasPrefix(s, `"`) {
		return s
	}
	if u, err := strconv.Unquote(s); err == nil {
		return u
	}
	return s
}
