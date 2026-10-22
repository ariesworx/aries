package merges

import (
	"errors"
	"path"
	"strings"
)

// Match reports whether rel, a slash-separated path from the
// repository's top, matches pattern. A pattern is always read from the
// top: ** is any number of directories, none included, and every other
// segment is a path.Match pattern of one name.
func Match(pattern, rel string) bool {
	return matchSegs(strings.Split(pattern, "/"), strings.Split(rel, "/"))
}

func matchSegs(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(segs); i++ {
				if matchSegs(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], segs[0]); err != nil || !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}

// checkPattern refuses a pattern Match cannot read as written.
func checkPattern(p string) error {
	switch {
	case p == "":
		return errors.New("an empty pattern")
	case strings.HasPrefix(p, "/"):
		return errors.New("a pattern is read from the repository's top: drop the leading /")
	case strings.HasSuffix(p, "/"):
		return errors.New("a pattern names files: write dir/** for a directory")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return errors.New("a pattern has an empty, . or .. segment")
		}
		if seg == "**" {
			continue
		}
		if strings.Contains(seg, "**") {
			return errors.New("** must be a whole segment")
		}
		if _, err := path.Match(seg, ""); err != nil {
			return errors.New("a malformed pattern")
		}
	}
	return nil
}

// cleanPath reports whether p is a path the classifier can classify: a
// slash-separated path from the repository's top, already clean.
func cleanPath(p string) bool {
	return p != "" && !strings.HasPrefix(p, "/") && path.Clean(p) == p && p != "." &&
		!strings.HasPrefix(p, "../") && p != ".." && !strings.ContainsAny(p, "\x00\n\r\\\"\t")
}
