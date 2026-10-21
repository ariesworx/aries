// Package guard is aries guard (docs/design/guard.md): Claude Code's
// PreToolUse hook over one permission policy, the repository's
// .aries/guard.yaml, for headless runs and Desktop sessions alike.
//
// It reads a Bash command as sh with its own parser (Parse), through
// wrappers and compound commands, checks every simple command in it
// against the policy, and answers allow, deny with a fix, ask, or
// nothing. It runs nothing, opens no connection and starts no process:
// the policy it reads is a snapshot taken at the session's start
// (snapshot.go), and the one file it writes is the count of denials.
package guard

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/ariesworx/aries/internal/lazyregexp"
)

// File is the policy's path from the repository's top.
const File = ".aries/guard.yaml"

// Builtin is the Source of the rules that apply whatever the file says.
const Builtin = "built in"

// Version is the policy file's version this binary reads.
const Version = 1

// EnvRule is what the policy says of a variable set in front of a
// command: allowed with any value (Any), or only with the session's own
// value, and otherwise denied with Fix.
type EnvRule struct {
	Fix string
	Any bool
}

// Rule is one deny rule. Words is a run: rule's words (the program
// first); Paths a path: rule's patterns. Flags, Refs and Config narrow a
// run: rule to commands holding one of those flags, pushing to one of
// those refs, or setting one of those git config keys.
type Rule struct {
	// N is the rule's place in the file's deny list, from 1; 0 for a
	// built-in rule.
	N      int
	Words  []string
	Flags  []string
	Refs   []string
	Config []string
	Paths  []string
	Fix    string
}

// ID names the rule in a denial and in the denial count: "deny N", or
// "builtin".
func (r Rule) ID() string {
	if r.N == 0 {
		return "builtin"
	}
	return fmt.Sprintf("deny %d", r.N)
}

// Policy is a loaded policy file, or the built-in rules alone.
type Policy struct {
	// Source is File, or Builtin for the rules that apply without one.
	Source string
	Env    map[string]EnvRule
	// Wrappers are programs that run the command after their options.
	Wrappers []string
	// Options are, per program, its options that take a value.
	Options map[string][]string
	// Tools are the tool names allowed outright, for the dispatcher's
	// --allowedTools; the guard decides only Bash and the file tools.
	Tools []string
	// Allow are the run: allow rules, as words.
	Allow [][]string
	Deny  []Rule
}

// builtinOptions are the options that take a value for the programs the
// built-in rules name, so that `git -C . push` is read as a push.
var builtinOptions = map[string][]string{
	"git": {"-C", "-c", "--git-dir", "--work-tree", "--namespace", "--config-env"},
	"gh":  {"-R", "--repo"},
}

// builtinDeny are the rules that apply with no policy file, and with
// one, whatever it says (docs/design/guard.md §3, decision 4).
var builtinDeny = []Rule{
	{Words: []string{"gh", "pr", "merge"}, Fix: "merges are the coordinator's: hand off with the PR's link"},
	{Words: []string{"gh", "api"}, Fix: "gh api is not allowed: use gh pr view, gh pr checks or gh run view"},
	{Words: []string{"git", "push"}, Flags: []string{"--force", "-f", "--force-with-lease", "--force-if-includes", "--mirror", "--delete", "-d"},
		Fix: "push without force to your own branch: git push -u origin HEAD"},
	{Words: []string{"git", "push"}, Refs: []string{"main"}, Fix: "push to your branch, never main: git push -u origin HEAD"},
}

// BuiltinPolicy is the policy of a repository with no policy file: the
// built-in denies, and nothing allowed.
func BuiltinPolicy() *Policy {
	return &Policy{Source: Builtin, Options: builtinOptions}
}

// The file's shape, decoded strictly.
type (
	file struct {
		Version  int                 `yaml:"version"`
		Env      map[string]envRule  `yaml:"env"`
		Wrappers []string            `yaml:"wrappers"`
		Options  map[string][]string `yaml:"options"`
		Allow    []allowRule         `yaml:"allow"`
		Deny     []denyRule          `yaml:"deny"`
	}
	envRule struct {
		Fix string `yaml:"fix"`
		Any bool   `yaml:"any"`
	}
	allowRule struct {
		Tools []string `yaml:"tools"`
		Run   []string `yaml:"run"`
	}
	denyRule struct {
		Run    string   `yaml:"run"`
		Flags  []string `yaml:"flags"`
		Refs   []string `yaml:"refs"`
		Config []string `yaml:"config"`
		Path   []string `yaml:"path"`
		Fix    string   `yaml:"fix"`
	}
)

// unknownField is what the YAML module says of a key the file's shape
// does not have.
var unknownField = lazyregexp.New(`line (\d+): field (\S+) not found in type`)

// Load reads a policy file. An unknown key, a rule with no fix, a rule
// that is not one thing or another, or a version this binary does not
// read is an error of one line saying where.
func Load(b []byte) (*Policy, error) {
	var f file
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, yamlError(err)
	}
	switch {
	case f.Version == 0:
		return nil, errors.New("no version: the file begins `version: 1`")
	case f.Version != Version:
		return nil, fmt.Errorf("version %d: this aries reads version %d; upgrade aries", f.Version, Version)
	}
	p := &Policy{Source: File, Env: map[string]EnvRule{}, Options: map[string][]string{}}
	for name, e := range f.Env {
		if (e.Fix == "") == !e.Any {
			return nil, fmt.Errorf("env %s: either fix or any: true", name)
		}
		p.Env[name] = EnvRule(e)
	}
	for _, w := range f.Wrappers {
		if strings.TrimSpace(w) == "" {
			return nil, errors.New("wrappers: an empty name")
		}
		p.Wrappers = append(p.Wrappers, strings.TrimSpace(w))
	}
	for prog, opts := range builtinOptions {
		p.Options[prog] = append(p.Options[prog], opts...)
	}
	for prog, opts := range f.Options {
		p.Options[prog] = append(p.Options[prog], opts...)
	}
	for i, a := range f.Allow {
		if (len(a.Tools) == 0) == (len(a.Run) == 0) {
			return nil, fmt.Errorf("allow %d: either tools or run", i+1)
		}
		p.Tools = append(p.Tools, a.Tools...)
		for _, r := range a.Run {
			words := strings.Fields(r)
			if len(words) == 0 {
				return nil, fmt.Errorf("allow %d: an empty run", i+1)
			}
			p.Allow = append(p.Allow, words)
		}
	}
	for i, d := range f.Deny {
		r, err := denyFrom(i+1, d)
		if err != nil {
			return nil, err
		}
		p.Deny = append(p.Deny, r)
	}
	return p, nil
}

// yamlError is the YAML module's error in one line, an unknown key
// named as such.
func yamlError(err error) error {
	msg := err.Error()
	if m := unknownField.FindStringSubmatch(msg); m != nil {
		return fmt.Errorf("line %s: unknown key %q", m[1], m[2])
	}
	msg = strings.TrimPrefix(msg, "yaml: ")
	msg = strings.Join(strings.Fields(msg), " ")
	return errors.New(msg)
}

func denyFrom(n int, d denyRule) (Rule, error) {
	r := Rule{N: n, Words: strings.Fields(d.Run), Flags: d.Flags, Refs: d.Refs, Config: d.Config, Paths: d.Path, Fix: strings.TrimSpace(d.Fix)}
	where := fmt.Sprintf("deny %d", n)
	switch {
	case (d.Run == "") == (len(d.Path) == 0):
		return r, errors.New(where + ": either run or path")
	case d.Run != "" && len(r.Words) == 0:
		return r, errors.New(where + ": run is empty")
	case r.Fix == "":
		return r, errors.New(where + ": no fix: every denial says what to do instead")
	case d.Run == "" && len(d.Flags)+len(d.Refs)+len(d.Config) > 0:
		return r, errors.New(where + ": flags, refs and config need run")
	case len(d.Refs) > 0 && strings.Join(r.Words, " ") != "git push":
		return r, errors.New(where + ": refs are for git push")
	case len(d.Config) > 0 && strings.Join(r.Words, " ") != "git":
		return r, errors.New(where + ": config is for git")
	}
	return r, nil
}

// matchPath reports whether rel, a slash-separated path from the
// repository's top, matches pattern as gitignore reads it: a pattern
// with no slash but a trailing one matches a name at any depth, ** any
// number of directories, and a trailing slash or /** everything under
// a directory.
func matchPath(pattern, rel string) bool {
	anchored := strings.Contains(strings.TrimSuffix(pattern, "/"), "/")
	if strings.HasSuffix(pattern, "/") {
		pattern += "**"
	}
	pattern = strings.TrimPrefix(pattern, "/")
	if !anchored {
		pattern = "**/" + pattern
	}
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
