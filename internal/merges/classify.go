package merges

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// PR is what was read of one open pull request. An Err field says that
// part could not be read; the classifier holds on it.
type PR struct {
	Number int
	URL    string
	Title  string
	Base   string
	Branch string
	Head   string
	Draft  bool
	// Err is why the pull request itself could not be read.
	Err string
	// Checks are its required checks on the head commit.
	Checks    []Check
	ChecksErr string
	// Files are its changed paths against its base.
	Files    []Change
	FilesErr string
	Issue    Issue
}

// Check is one required check and its bucket as gh names it: pass, fail,
// pending, skipping or cancel.
type Check struct {
	Name   string `json:"name"`
	Bucket string `json:"bucket"`
}

// Change is one changed path: Old is the path it had before a rename or
// copy. Removed counts the lines it loses and Skips the t.Skip calls it
// gains, which the tests condition reads for a test file.
type Change struct {
	Path    string
	Old     string
	Removed int
	Skips   int
}

// Issue is the pull request's issue, as the branch names it.
type Issue struct {
	ID string
	// Err is why the issue could not be read.
	Err  string
	Epic Epic
	// Brief is the fields of the issue's last `brief:` comment, after
	// "brief: ", or "" with none.
	Brief string
}

// Epic says whether the issue sits under an approved epic, and if not,
// why not; Detail names the epic when it is.
type Epic struct {
	Approved bool
	Detail   string
}

// Context is what the classifier needs beside the rule and the pull
// request.
type Context struct {
	// Base is the default branch: only a pull request onto it is queued.
	Base string
	// RulesPath is the rule's file, from the repository's top, when it is
	// in the repository and not at File; the floor holds it too.
	RulesPath string
}

// Verdict is the rule's answer for one pull request.
type Verdict struct {
	Number int      `json:"number"`
	URL    string   `json:"url,omitempty"`
	Issue  string   `json:"issue,omitempty"`
	Queue  bool     `json:"queue"`
	Class  string   `json:"class,omitempty"`
	Opener Opened   `json:"opener"`
	Holds  []Reason `json:"holds,omitempty"`
}

// Opened is what is known of the opener: never verified, until briefs
// are stored (decision 5).
type Opened struct {
	Verified bool   `json:"verified"`
	Brief    string `json:"brief,omitempty"`
}

// Reason is one condition that held a pull request: of a class, or of
// the pull request whatever the class (Class empty).
type Reason struct {
	Class     string `json:"class,omitempty"`
	Condition string `json:"condition"`
	Detail    string `json:"detail"`
}

func (r Reason) String() string {
	s := r.Condition + ": " + r.Detail
	if r.Class != "" {
		s = r.Class + " " + s
	}
	return s
}

// String is the verdict in one line, without the number: "queue CLASS",
// or "hold" and its reasons.
func (v Verdict) String() string {
	if v.Queue {
		return "queue " + v.Class
	}
	s := make([]string, len(v.Holds))
	for i, h := range v.Holds {
		s[i] = h.String()
	}
	return "hold " + strings.Join(s, "; ")
}

// Classify is the rule's verdict on pr. It holds unless one class of r
// passes pr and nothing holds it whatever the class: an unreadable pull
// request, no rule, a draft, a base other than the default branch, or a
// path of the floor. A held pull request names, for each class, the
// first condition that failed, in the order checks, epic, paths, tests,
// opener.
func Classify(r *Rules, c Context, pr PR) Verdict {
	v := Verdict{Number: pr.Number, URL: pr.URL, Issue: pr.Issue.ID, Opener: Opened{Brief: pr.Issue.Brief}}
	hold := func(cond, detail string) { v.Holds = append(v.Holds, Reason{Condition: cond, Detail: detail}) }
	if pr.Err != "" {
		hold("read", pr.Err)
		return v
	}
	if r == nil {
		hold("rule", "no rule file ("+File+" on the default branch, or --rules FILE)")
		return v
	}
	if pr.Draft {
		hold("draft", "a draft is a hold")
	}
	if c.Base == "" || pr.Base != c.Base {
		hold("base", fmt.Sprintf("%s, not %s", cmpOr(pr.Base, "unknown"), cmpOr(c.Base, "a known default branch")))
	}
	for _, p := range paths(pr.Files) {
		if !cleanPath(p) {
			hold("paths", strconv.Quote(p)+" cannot be classified")
		} else if role, ok := floorOf(r, c, p); ok {
			hold("floor", p+" ("+role+")")
		}
	}
	if len(v.Holds) > 0 {
		return v
	}
	for _, cl := range r.Classes {
		if why, ok := cl.fails(pr); ok {
			why.Class = cl.Name
			v.Holds = append(v.Holds, why)
			continue
		}
		v.Queue, v.Class, v.Holds = true, cl.Name, nil
		return v
	}
	return v
}

func cmpOr(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

// paths are every path the change touches, both sides of a rename, once
// each, in order.
func paths(cs []Change) []string {
	var out []string
	for _, c := range cs {
		for _, p := range []string{c.Old, c.Path} {
			if p != "" && !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	return out
}

func floorOf(r *Rules, c Context, p string) (string, bool) {
	for _, a := range Floor {
		for _, pat := range a.Paths {
			if Match(pat, p) {
				return a.Role, true
			}
		}
	}
	if c.RulesPath != "" && p == c.RulesPath {
		return "the merge rule", true
	}
	for _, pat := range r.Floor {
		if Match(pat, p) {
			return "the rule's floor", true
		}
	}
	return "", false
}

// fails returns the first condition of c that pr fails.
func (c Class) fails(pr PR) (Reason, bool) {
	if d, ok := checksFail(pr); ok {
		return Reason{Condition: "checks", Detail: d}, true
	}
	if c.Epic != "" {
		if d, ok := epicFails(pr.Issue); ok {
			return Reason{Condition: "epic", Detail: d}, true
		}
	}
	if d, ok := c.pathsFail(pr); ok {
		return Reason{Condition: "paths", Detail: d}, true
	}
	if c.Tests != "" {
		if d, ok := testsFail(pr.Files); ok {
			return Reason{Condition: "tests", Detail: d}, true
		}
	}
	if c.Opener != nil {
		brief := "no brief: comment"
		if pr.Issue.Brief != "" {
			brief = "brief " + pr.Issue.Brief
		}
		return Reason{Condition: "opener", Detail: "unverified (" + brief + ")"}, true
	}
	return Reason{}, false
}

func checksFail(pr PR) (string, bool) {
	if pr.ChecksErr != "" {
		return "unreadable: " + pr.ChecksErr, true
	}
	if len(pr.Checks) == 0 {
		return "no required check reported", true
	}
	var bad []string
	for _, ch := range pr.Checks {
		if ch.Bucket != "pass" {
			bad = append(bad, ch.Name+" "+cmpOr(ch.Bucket, "unknown"))
		}
	}
	return strings.Join(bad, ", "), len(bad) > 0
}

func epicFails(is Issue) (string, bool) {
	switch {
	case is.ID == "":
		return "no issue: the branch names none", true
	case is.Err != "":
		return "issue " + is.ID + " unreadable: " + is.Err, true
	case !is.Epic.Approved:
		return cmpOr(is.Epic.Detail, "no approved epic above "+is.ID), true
	}
	return "", false
}

func (c Class) pathsFail(pr PR) (string, bool) {
	if pr.FilesErr != "" {
		return "unreadable: " + pr.FilesErr, true
	}
	ps := paths(pr.Files)
	if len(ps) == 0 {
		return "no changed path read", true
	}
	var bad []string
	for _, p := range ps {
		switch {
		case matchAny(c.Deny, p):
			bad = append(bad, p+" (denied)")
		case len(c.Allow) > 0 && (!matchAny(c.Allow, p) || matchAny(c.Except, p)):
			bad = append(bad, p+" (not allowed)")
		}
	}
	return strings.Join(bad, ", "), len(bad) > 0
}

func matchAny(pats []string, p string) bool {
	return slices.ContainsFunc(pats, func(pat string) bool { return Match(pat, p) })
}

// testsFail holds a test file that loses lines or gains a t.Skip: paths
// cannot see a test weakened (dispatcher.md §8).
func testsFail(cs []Change) (string, bool) {
	var bad []string
	for _, c := range cs {
		if !strings.HasSuffix(c.Path, "_test.go") && !strings.HasSuffix(c.Old, "_test.go") {
			continue
		}
		name := cmpOr(c.Path, c.Old)
		switch {
		case c.Removed > 0:
			bad = append(bad, fmt.Sprintf("%s loses %d lines", name, c.Removed))
		case c.Skips > 0:
			bad = append(bad, name+" adds t.Skip")
		}
	}
	return strings.Join(bad, ", "), len(bad) > 0
}
