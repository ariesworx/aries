package scripts_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

// markdownTarget finds the destination of an inline markdown link or
// image: the URL inside ](…). Link text is not parsed; a nested image
// link ([![alt](img)](url)) yields both targets, which is what we want.
// Titles after the URL (](url "title")) are not used in this repository.
var markdownTarget = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// atxHeading matches an ATX heading outside a fenced code block.
var atxHeading = regexp.MustCompile(`^(#{1,6})[ \t]+(.+?)[ \t]*#*[ \t]*$`)

// TestMarkdownLinks holds every relative markdown link and heading
// anchor in tracked *.md files to a path that exists and, for #slug
// fragments on .md targets, a GitHub-style heading slug in that file.
//
// It skips http(s), mailto, and other scheme URLs so a docs-tier run
// does not need the network; external URL checks can be a nightly
// later. Fenced code blocks (``` / ~~~) and inline code are ignored so
// examples that look like links do not fail the test.
//
// Limitations: reference-style links ([text][id]) are not checked
// (none appear in this repository); indented code blocks are not
// skipped; HTML <a href> is not checked. No allowlist: every relative
// target must resolve inside the repository.
func TestMarkdownLinks(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	cmd := exec.CommandContext(t.Context(), "git", "ls-files", "-z", "*.md")
	cmd.Dir = ".."
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	files := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if len(files) < 10 {
		t.Fatalf("git ls-files *.md listed %d files; want the docs tree", len(files))
	}

	content := make(map[string][]byte, len(files))
	slugs := make(map[string]map[string]int, len(files))
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join("..", filepath.FromSlash(f))) //nolint:gosec // a file git tracks in this repository
		if err != nil {
			t.Errorf("read %s: %v", f, err)
			continue
		}
		content[f] = b
		slugs[f] = headingSlugs(b)
	}

	var nFiles, nLinks int
	for _, f := range files {
		b := content[f]
		if b == nil {
			continue
		}
		nFiles++
		dir := filepath.ToSlash(filepath.Dir(f))
		if dir == "." {
			dir = ""
		}
		for _, hit := range markdownLinks(b) {
			target := hit.target
			if isExternalMarkdownURL(target) {
				continue
			}
			nLinks++
			path, frag, hasFrag := strings.Cut(target, "#")
			var dest string
			switch path {
			case "":
				dest = f
			default:
				joined := path
				if dir != "" {
					joined = dir + "/" + path
				}
				clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(joined)))
				if clean == ".." || strings.HasPrefix(clean, "../") {
					t.Errorf("%s:%d: %s leaves the repository", f, hit.line, target)
					continue
				}
				if !filepath.IsLocal(filepath.FromSlash(clean)) {
					t.Errorf("%s:%d: %s is not a local path", f, hit.line, target)
					continue
				}
				full := filepath.Join("..", filepath.FromSlash(clean))
				if _, err := os.Stat(full); err != nil { //nolint:gosec // clean is IsLocal, under the repository root
					t.Errorf("%s:%d: %s does not resolve (%s)", f, hit.line, target, clean)
					continue
				}
				dest = clean
			}
			if !hasFrag || frag == "" {
				continue
			}
			if !strings.HasSuffix(dest, ".md") {
				continue
			}
			if _, ok := slugs[dest][frag]; !ok {
				t.Errorf("%s:%d: %s: no heading #%s in %s", f, hit.line, target, frag, dest)
			}
		}
	}
	if nFiles == 0 || nLinks == 0 {
		t.Fatalf("scanned %d markdown files and %d ](…) targets; want both non-zero", nFiles, nLinks)
	}
	t.Logf("checked %d relative targets across %d markdown files", nLinks, nFiles)
}

type mdLink struct {
	line   int
	target string
}

// markdownLinks returns each ](target) outside fenced and inline code.
func markdownLinks(b []byte) []mdLink {
	var out []mdLink
	inFence := false
	var fenceMark byte
	for i, line := range bytes.Split(b, []byte("\n")) {
		s := bytes.TrimLeft(line, " \t")
		if !inFence {
			if len(s) >= 3 && (s[0] == '`' || s[0] == '~') {
				m := s[0]
				n := 0
				for n < len(s) && s[n] == m {
					n++
				}
				if n >= 3 {
					inFence = true
					fenceMark = m
					continue
				}
			}
		} else {
			if len(s) >= 3 && s[0] == fenceMark {
				n := 0
				for n < len(s) && s[n] == fenceMark {
					n++
				}
				if n >= 3 {
					inFence = false
				}
			}
			continue
		}
		for _, m := range markdownTarget.FindAllSubmatch(stripInlineCode(line), -1) {
			out = append(out, mdLink{line: i + 1, target: string(m[1])})
		}
	}
	return out
}

// headingSlugs maps GitHub-style heading slugs to the 1-based line of
// the first heading that produced each slug. Duplicate headings get
// the -1, -2, … suffixes GitHub adds.
func headingSlugs(b []byte) map[string]int {
	out := map[string]int{}
	counts := map[string]int{}
	inFence := false
	var fenceMark byte
	for i, line := range bytes.Split(b, []byte("\n")) {
		s := bytes.TrimLeft(line, " \t")
		if !inFence {
			if len(s) >= 3 && (s[0] == '`' || s[0] == '~') {
				m := s[0]
				n := 0
				for n < len(s) && s[n] == m {
					n++
				}
				if n >= 3 {
					inFence = true
					fenceMark = m
					continue
				}
			}
		} else {
			if len(s) >= 3 && s[0] == fenceMark {
				n := 0
				for n < len(s) && s[n] == fenceMark {
					n++
				}
				if n >= 3 {
					inFence = false
				}
			}
			continue
		}
		m := atxHeading.FindSubmatch(line)
		if m == nil {
			continue
		}
		base := githubHeadingSlug(string(m[2]))
		n := counts[base]
		counts[base] = n + 1
		slug := base
		if n > 0 {
			slug = fmt.Sprintf("%s-%d", base, n)
		}
		if _, ok := out[slug]; !ok {
			out[slug] = i + 1
		}
	}
	return out
}

// githubHeadingSlug is the GitHub Flavored Markdown heading id: the
// heading text lowercased, punctuation stripped (letters, digits,
// spaces and hyphens kept; markdown emphasis and link markup removed),
// spaces turned into hyphens. Duplicate handling is in headingSlugs.
func githubHeadingSlug(text string) string {
	text = stripMDEmphasis(text)
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || r == '-':
			b.WriteRune(r)
		}
	}
	return strings.ReplaceAll(strings.TrimSpace(b.String()), " ", "-")
}

// stripMDEmphasis removes the markdown that GitHub does not keep in a
// heading slug: backticks, emphasis markers, and [text](url) → text.
func stripMDEmphasis(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		switch s[i] {
		case '`':
			if j := strings.IndexByte(s[i+1:], '`'); j >= 0 {
				b.WriteString(s[i+1 : i+1+j])
				i += j + 2
				continue
			}
		case '[':
			if end := strings.IndexByte(s[i:], ']'); end > 0 {
				rest := s[i+end+1:]
				if strings.HasPrefix(rest, "(") {
					if endParen := strings.IndexByte(rest, ')'); endParen >= 0 {
						b.WriteString(s[i+1 : i+end])
						i += end + 1 + endParen + 1
						continue
					}
				}
			}
		case '*', '_', '~':
			i++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// stripInlineCode replaces `…` spans with spaces so a ](…) inside them
// is not treated as a link. Unclosed backticks are left alone.
func stripInlineCode(line []byte) []byte {
	if !bytes.Contains(line, []byte("`")) {
		return line
	}
	out := make([]byte, len(line))
	copy(out, line)
	for i := 0; i < len(out); {
		if out[i] != '`' {
			i++
			continue
		}
		j := bytes.IndexByte(out[i+1:], '`')
		if j < 0 {
			break
		}
		for k := i; k <= i+1+j; k++ {
			out[k] = ' '
		}
		i += j + 2
	}
	return out
}

func isExternalMarkdownURL(target string) bool {
	if i := strings.Index(target, "://"); i > 0 {
		scheme := target[:i]
		for _, r := range scheme {
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '+' && r != '-' && r != '.' {
				return false
			}
		}
		return scheme != ""
	}
	return strings.HasPrefix(strings.ToLower(target), "mailto:")
}

func TestGithubHeadingSlug(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"With the installer", "with-the-installer"},
		{"What's Changed", "whats-changed"},
		{"10.3 Refusals (rule 11)", "103-refusals-rule-11"},
		{"The `Aries:` trailer", "the-aries-trailer"},
		{"As built", "as-built"},
		{"1. Install Postgres", "1-install-postgres"},
		{"Operate a server with Claude Code", "operate-a-server-with-claude-code"},
	}
	for _, tc := range cases {
		if got := githubHeadingSlug(tc.in); got != tc.want {
			t.Errorf("githubHeadingSlug(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// Duplicate headings get -1, -2 as on GitHub.
	body := []byte("# Same\n\n## Same\n\n## Same\n")
	got := headingSlugs(body)
	for _, slug := range []string{"same", "same-1", "same-2"} {
		if _, ok := got[slug]; !ok {
			t.Errorf("headingSlugs missing %q: %v", slug, got)
		}
	}
}
