// Package lazyregexp holds regular expressions in package variables
// without compiling them as the program starts, after the package of the
// same name in the Go distribution (cmd/go).
//
// Every starfixd process initializes every package the binary links, and
// sshd starts one, `starfixd stdio`, for each connection. Compiled at
// initialization, starfix's own expressions were most of what such a
// process allocated before main, and so most of its memory
// (docs/design/profile-starfixd.md, proposal 2). A Regexp compiles on
// first use instead, so a process pays only for those it uses.
package lazyregexp

import (
	"os"
	"regexp"
	"strings"
	"sync"
)

// Regexp is a regular expression compiled the first time it is used. It
// is safe for concurrent use, as a *regexp.Regexp is.
type Regexp struct {
	expr string
	once sync.Once
	re   *regexp.Regexp
}

// New returns the regular expression expr, which is compiled when first
// used, and then panics if it does not compile, as regexp.MustCompile
// does. In a test binary it is compiled at once, so a mistake fails the
// tests of the package that declares it.
func New(expr string) *Regexp {
	r := &Regexp{expr: expr}
	if inTest {
		r.compiled()
	}
	return r
}

// inTest says whether this is a test binary, which go test names *.test.
var inTest = len(os.Args) > 0 && strings.HasSuffix(strings.TrimSuffix(os.Args[0], ".exe"), ".test")

func (r *Regexp) compiled() *regexp.Regexp {
	r.once.Do(func() { r.re = regexp.MustCompile(r.expr) })
	return r.re
}

// String returns the source text of the expression.
func (r *Regexp) String() string { return r.expr }

// MatchString is [regexp.Regexp.MatchString].
func (r *Regexp) MatchString(s string) bool { return r.compiled().MatchString(s) }

// FindStringIndex is [regexp.Regexp.FindStringIndex].
func (r *Regexp) FindStringIndex(s string) []int { return r.compiled().FindStringIndex(s) }

// FindStringSubmatch is [regexp.Regexp.FindStringSubmatch].
func (r *Regexp) FindStringSubmatch(s string) []string { return r.compiled().FindStringSubmatch(s) }

// FindAllStringIndex is [regexp.Regexp.FindAllStringIndex].
func (r *Regexp) FindAllStringIndex(s string, n int) [][]int {
	return r.compiled().FindAllStringIndex(s, n)
}

// FindAllStringSubmatch is [regexp.Regexp.FindAllStringSubmatch].
func (r *Regexp) FindAllStringSubmatch(s string, n int) [][]string {
	return r.compiled().FindAllStringSubmatch(s, n)
}

// FindAllStringSubmatchIndex is [regexp.Regexp.FindAllStringSubmatchIndex].
func (r *Regexp) FindAllStringSubmatchIndex(s string, n int) [][]int {
	return r.compiled().FindAllStringSubmatchIndex(s, n)
}
