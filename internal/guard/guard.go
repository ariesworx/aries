package guard

import (
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Mode is the kind of session a call comes from.
type Mode int

const (
	// Desktop is a session a person can see: a command the guard cannot
	// read is asked of them, and a guard that cannot read its policy
	// fails open.
	Desktop Mode = iota
	// Headless is a run nobody watches (aries dispatch): an unreadable
	// command is denied, and so is every call when the policy cannot be
	// read.
	Headless
)

func (m Mode) String() string {
	if m == Headless {
		return "headless"
	}
	return "desktop"
}

// Answer is the guard's decision on a call: Claude Code's
// permissionDecision, or None for no decision.
type Answer string

// The answers (docs/design/guard.md §3).
const (
	None  Answer = ""
	Allow Answer = "allow"
	Deny  Answer = "deny"
	Ask   Answer = "ask"
)

// Verdict is a decision with what it rests on. Rule is the rule's ID,
// "env NAME", or "unreadable"; Reason is what the agent reads, which
// names the rule and never the command's arguments.
type Verdict struct {
	Answer Answer
	Rule   string
	Reason string
}

// Call is one tool call to decide.
type Call struct {
	// Tool is Claude Code's tool name: Bash, Edit, Write, MultiEdit or
	// NotebookEdit. Any other is no decision.
	Tool string
	// Command is Bash's command; FilePath the file tools' absolute path.
	Command  string
	FilePath string
	// Cwd is the session's directory, and Top the top of the repository
	// it is in, both absolute, Top with its symlinks resolved.
	Cwd, Top string
	// Getenv reads the session's environment, for the env: rules.
	Getenv func(string) string
	// Protected are directories nothing may write to: the guard's
	// snapshots.
	Protected []string
}

// The last line of every denial (docs/design/guard.md §7).
const refusedOnly = "Only this command was refused; other commands, Bash included, still run."

// unreadableFix is the fix of a command the guard cannot read.
const unreadableFix = "write it as plain commands, one program each"

// Decide answers a call under the policy; a nil policy is the built-in
// rules alone. It runs nothing and reads only the file system's links.
func (p *Policy) Decide(c Call, m Mode) Verdict {
	if p == nil {
		p = BuiltinPolicy()
	}
	switch c.Tool {
	case "Bash":
		return p.decideBash(c, m)
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		ch := &checker{p: p, c: c}
		if r, pat, ok := ch.pathRule(c.FilePath); ok {
			return denial(p, r, "writing a file that matches `"+pat+"`", false)
		}
	}
	return Verdict{}
}

func (p *Policy) decideBash(c Call, m Mode) Verdict {
	s := Parse(c.Command)
	ch := &checker{p: p, c: c}
	allowed := len(s.Commands) > 0
	for _, cmd := range s.Commands {
		if !ch.command(cmd) {
			allowed = false
		}
	}
	if s.Unreadable != "" {
		ch.unread(s.Unreadable)
	}
	switch {
	case ch.denied != nil:
		return *ch.denied
	case p.Source == Builtin:
		return Verdict{}
	case ch.unreadable != "":
		reason := "aries guard: this command cannot be read: " + ch.unreadable + " (unreadable).\nfix: " + unreadableFix + ".\n"
		if m == Headless {
			return Verdict{Answer: Deny, Rule: "unreadable", Reason: reason + refusedOnly}
		}
		return Verdict{Answer: Ask, Rule: "unreadable", Reason: reason + "Allow it only once you have read all of it."}
	case allowed:
		return Verdict{Answer: Allow}
	}
	return Verdict{}
}

// denial is a deny verdict on rule r, whose subject is what the reason
// says was not allowed. uncertain says the command matched the rule's
// words but an argument the rule reads is not known.
func denial(p *Policy, r Rule, subject string, uncertain bool) Verdict {
	where := Builtin
	if r.N > 0 {
		where = p.Source + ", " + r.ID()
	}
	fix := strings.TrimSuffix(r.Fix, ".")
	text := subject + " is not allowed here"
	if uncertain {
		text = subject + " cannot be ruled out: an argument is not known before the command runs"
		fix = "write the arguments out in the command; " + fix
	}
	return Verdict{Answer: Deny, Rule: r.ID(),
		Reason: "aries guard: " + text + " (" + where + ").\nfix: " + fix + ".\n" + refusedOnly}
}

// checker checks a script's simple commands, keeping the first denial
// and the first reason a part could not be read.
type checker struct {
	p          *Policy
	c          Call
	denied     *Verdict
	unreadable string
	// dirChanged says a cd came before: a relative path no longer names
	// what it did from the session's directory.
	dirChanged bool
}

func (ch *checker) deny(v Verdict) {
	if ch.denied == nil {
		ch.denied = &v
	}
}

func (ch *checker) unread(why string) {
	if ch.unreadable == "" {
		ch.unreadable = why
	}
}

// command checks one simple command and reports whether every part of
// it is allowed.
func (ch *checker) command(cmd Command) bool {
	ok := true
	for _, r := range cmd.Redirs {
		if r.Write && !ch.writes(r.Target) {
			ok = false
		}
	}
	for _, a := range cmd.Assigns {
		if !ch.assign(a) {
			ok = false
		}
	}
	return ch.words(cmd.Words, false, 0) && ok
}

// assign checks NAME=value against the policy's env: rules.
func (ch *checker) assign(a Assign) bool {
	e, listed := ch.p.Env[a.Name]
	switch {
	case !listed:
		return false
	case e.Any:
		return true
	case a.Value.Known && ch.c.Getenv != nil && a.Value.Text == ch.c.Getenv(a.Name):
		return true
	}
	ch.deny(Verdict{Answer: Deny, Rule: "env " + a.Name,
		Reason: "aries guard: setting `" + a.Name + "=…` in front of a command is not allowed here (" + ch.p.Source + ", env " + a.Name + ").\nfix: " +
			strings.TrimSuffix(e.Fix, ".") + ".\n" + refusedOnly})
	return false
}

// writes checks a file a command writes, and reports whether writing it
// is allowed: a file in the repository no rule denies, or a device that
// is not a file.
func (ch *checker) writes(target Word) bool {
	if !target.Known {
		ch.unread("a file it writes is not known before it runs")
		return false
	}
	t := target.Text
	switch t {
	case "/dev/null", "/dev/stdout", "/dev/stderr":
		return true
	}
	if !filepath.IsAbs(t) && ch.dirChanged {
		ch.unread("it writes to a relative path after cd")
		return false
	}
	abs := t
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(ch.c.Cwd, abs)
	}
	if r, pat, ok := ch.pathRule(abs); ok {
		ch.deny(denial(ch.p, r, "writing a file that matches `"+pat+"`", false))
		return false
	}
	_, inside := relTo(ch.c.Top, resolve(abs, 0))
	return inside
}

// The built-in path rules.
var (
	policyRule   = Rule{Paths: []string{File}, Fix: "the policy is the maintainer's: say what it needs in your handoff"}
	snapshotRule = Rule{Fix: "the guard's snapshot of the policy is not to be changed: say what it needs in your handoff"}
)

// pathRule finds the deny rule, if any, for writing the file at abs, and
// the pattern it matched.
func (ch *checker) pathRule(abs string) (Rule, string, bool) {
	if abs == "" {
		return Rule{}, "", false
	}
	file := resolve(filepath.Clean(abs), 0)
	for _, d := range ch.c.Protected {
		d = resolve(filepath.Clean(d), 0)
		if _, in := relTo(d, file); in || d == file {
			return snapshotRule, "the guard's snapshots", true
		}
	}
	rel, in := relTo(ch.c.Top, file)
	if !in {
		return Rule{}, "", false
	}
	for _, r := range ch.p.Deny {
		for _, pat := range r.Paths {
			if matchPath(pat, rel) {
				return r, pat, true
			}
		}
	}
	if rel == File {
		return policyRule, File, true
	}
	return Rule{}, "", false
}

// relTo is p's slash-separated path from top, when it is inside top.
func relTo(top, p string) (string, bool) {
	if top == "" {
		return "", false
	}
	rel, err := filepath.Rel(top, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// resolve is p with its symlinks resolved, as far as the file system
// holds them: a file not yet written is resolved through its directory.
func resolve(p string, depth int) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	if depth > 40 {
		return p
	}
	if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if t, err := os.Readlink(p); err == nil {
			if !filepath.IsAbs(t) {
				t = filepath.Join(filepath.Dir(p), t)
			}
			return resolve(filepath.Clean(t), depth+1)
		}
	}
	dir := filepath.Dir(p)
	if dir == p {
		return p
	}
	return filepath.Join(resolve(dir, depth+1), filepath.Base(p))
}

// words checks a command's words, the program first: the deny rules,
// then what a wrapper runs, then the allow rules. tail says unknown
// arguments follow the words (xargs, find -exec). It reports whether the
// command is allowed.
func (ch *checker) words(ws []Word, tail bool, depth int) bool {
	if len(ws) == 0 {
		return true
	}
	if depth > maxDepth {
		ch.unread("it is wrapped more than eight deep")
		return false
	}
	prog := ws[0]
	if !prog.Known {
		ch.unread("its program is named by a variable or a pattern")
		return false
	}
	name := path.Base(filepath.ToSlash(prog.Text))
	args := ws[1:]
	switch name {
	case "eval", "source", ".":
		ch.unread(name + " runs text the guard cannot see")
		return false
	case "cd", "pushd", "popd":
		defer func() { ch.dirChanged = true }()
	case "tee":
		for _, a := range args {
			if a.Known && strings.HasPrefix(a.Text, "-") {
				continue
			}
			ch.writes(a)
		}
	}
	ch.denyRules(prog.Text, args, tail)
	if w, ok := ch.wrapper(name, prog.Text, args, tail, depth); ok {
		return w
	}
	ok := ch.allowRules(prog.Text, args)
	if name == "find" {
		for _, inner := range findActions(args) {
			if !ch.words(inner, false, depth+1) {
				ok = false
			}
		}
	}
	return ok
}

// script checks a string a wrapper runs as a shell command.
func (ch *checker) script(src Word, depth int) bool {
	if !src.Known {
		ch.unread("the command a shell runs is not known before it runs")
		return false
	}
	if depth > maxDepth {
		ch.unread("it is wrapped more than eight deep")
		return false
	}
	s := Parse(src.Text)
	if s.Unreadable != "" {
		ch.unread(s.Unreadable)
	}
	ok := len(s.Commands) > 0 && s.Unreadable == ""
	for _, cmd := range s.Commands {
		for _, r := range cmd.Redirs {
			if r.Write && !ch.writes(r.Target) {
				ok = false
			}
		}
		for _, a := range cmd.Assigns {
			if !ch.assign(a) {
				ok = false
			}
		}
		if !ch.words(cmd.Words, false, depth+1) {
			ok = false
		}
	}
	return ok
}

// progMatches reports whether a rule's program, rule, names the
// command's program, prog. For a deny rule (loose) a name matches any
// path ending in it, so /usr/bin/gh is gh; for an allow rule only the
// bare name does. A rule that names a path matches the program's path
// from the repository's top.
func (ch *checker) progMatches(rule, prog string, loose bool) bool {
	if strings.Contains(rule, "/") {
		if !strings.Contains(prog, "/") || (!loose && ch.dirChanged && !filepath.IsAbs(prog)) {
			return false
		}
		abs := prog
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(ch.c.Cwd, abs)
		}
		rel, ok := relTo(ch.c.Top, filepath.Clean(abs))
		if loose && path.Base(rule) == path.Base(filepath.ToSlash(prog)) {
			return true
		}
		return ok && rel == path.Clean(rule)
	}
	if loose {
		return path.Base(filepath.ToSlash(prog)) == rule
	}
	return prog == rule
}

// match is how a rule's words met a command's.
type match int

const (
	noMatch match = iota
	matched
	unsure // an argument the rule reads is not known
)

// matchWords matches a rule's words after the program against args,
// skipping the program's options (those in the policy's options: with
// their values) before each word that is not itself an option. It
// returns the arguments after the words.
func (ch *checker) matchWords(prog string, rule []string, args []Word, tail bool) (match, []Word) {
	withValue := ch.p.Options[prog]
	if withValue == nil {
		withValue = builtinOptions[prog]
	}
	i := 0
	for _, rw := range rule {
		if !strings.HasPrefix(rw, "-") {
			for i < len(args) && args[i].Known && strings.HasPrefix(args[i].Text, "-") && args[i].Text != "-" {
				if takesValue(withValue, args[i].Text) {
					i++
				}
				i++
			}
		}
		if i >= len(args) {
			if tail {
				return unsure, nil
			}
			return noMatch, nil
		}
		if !args[i].Known {
			return unsure, nil
		}
		if args[i].Text != rw {
			return noMatch, nil
		}
		i++
	}
	return matched, args[i:]
}

// takesValue reports whether opt is one of withValue written apart from
// its value: `-C dir`, not `-Cdir` or `--git-dir=x`.
func takesValue(withValue []string, opt string) bool {
	return slices.Contains(withValue, opt)
}

func (ch *checker) denyRules(prog string, args []Word, tail bool) {
	name := path.Base(filepath.ToSlash(prog))
	for _, rules := range [][]Rule{ch.p.Deny, builtinDeny} {
		for _, r := range rules {
			if len(r.Words) == 0 || !ch.progMatches(r.Words[0], prog, true) {
				continue
			}
			m, rest := ch.matchWords(name, r.Words[1:], args, tail)
			if m == matched {
				switch {
				case len(r.Flags) > 0:
					m = flagsMatch(r, rest, tail)
				case len(r.Refs) > 0:
					m = refsMatch(r, rest, tail)
				case len(r.Config) > 0:
					m = ch.configMatch(r, args)
				}
			}
			if m != noMatch {
				ch.deny(denial(ch.p, r, ruleSubject(r), m == unsure))
				return
			}
		}
	}
}

// ruleSubject is what a denial says the rule refuses, in the rule's own
// words.
func ruleSubject(r Rule) string {
	s := "`" + strings.Join(r.Words, " ") + "`"
	switch {
	case len(r.Flags) > 0:
		s += " with " + strings.Join(r.Flags, ", ")
	case len(r.Refs) > 0:
		s += " to " + strings.Join(r.Refs, ", ")
	case len(r.Config) > 0:
		s += " with -c " + strings.Join(r.Config, ", ")
	}
	return s
}

// flagsMatch reports whether rest holds one of the rule's flags: exactly,
// as --flag=value, or a short flag among others (-fu holds -f). For git
// push, a +refspec is --force.
func flagsMatch(r Rule, rest []Word, tail bool) match {
	push := strings.Join(r.Words, " ") == "git push" && slices.Contains(r.Flags, "--force")
	m := noMatch
	for _, a := range rest {
		if !a.Known {
			m = unsure
			continue
		}
		t := a.Text
		if push && strings.HasPrefix(t, "+") {
			return matched
		}
		for _, f := range r.Flags {
			switch {
			case t == f,
				strings.HasPrefix(f, "--") && strings.HasPrefix(t, f+"="),
				len(f) == 2 && f[0] == '-' && f[1] != '-' && len(t) > 2 && t[0] == '-' && t[1] != '-' && strings.IndexByte(t[1:], f[1]) >= 0:
				return matched
			}
		}
	}
	if tail {
		return unsure
	}
	return m
}

// pushValues are git push's options that take a value apart from it.
var pushValues = []string{"-o", "--push-option", "--receive-pack", "--exec", "--repo"}

// refsMatch reports whether git push's arguments push to one of the
// rule's refs: a refspec whose destination is the ref, with or without
// refs/heads/ and +, or a deletion of it.
func refsMatch(r Rule, rest []Word, tail bool) match {
	m := noMatch
	repoSet := false
	positional := 0
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if !a.Known {
			m = unsure
			positional++
			continue
		}
		t := a.Text
		if strings.HasPrefix(t, "-") && t != "-" {
			if strings.HasPrefix(t, "--repo") {
				repoSet = true
			}
			if slices.Contains(pushValues, t) {
				i++
			}
			continue
		}
		positional++
		if positional == 1 && !repoSet {
			continue // the repository
		}
		dst := strings.TrimPrefix(t, "+")
		if _, after, ok := strings.Cut(dst, ":"); ok {
			dst = after
		}
		dst = strings.TrimPrefix(dst, "refs/heads/")
		if slices.Contains(r.Refs, dst) {
			return matched
		}
	}
	if tail {
		return unsure
	}
	return m
}

// configMatch reports whether git's options before its subcommand set a
// config key the rule names (git -c alias.x=…), keys compared as git
// does, without case.
func (ch *checker) configMatch(r Rule, args []Word) match {
	withValue := ch.p.Options["git"]
	keyOf := func(w Word) (string, bool) {
		if !w.Known {
			return "", false
		}
		k, _, _ := strings.Cut(w.Text, "=")
		return strings.ToLower(k), true
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !a.Known {
			return unsure
		}
		t := a.Text
		if !strings.HasPrefix(t, "-") {
			return noMatch // the subcommand
		}
		var key string
		switch {
		case t == "-c" || t == "--config-env":
			if i+1 >= len(args) {
				return noMatch
			}
			k, ok := keyOf(args[i+1])
			if !ok {
				return unsure
			}
			key = k
			i++
		case strings.HasPrefix(t, "-c"):
			key, _ = keyOf(Word{Text: t[2:], Known: true})
		case strings.HasPrefix(t, "--config-env="):
			key, _ = keyOf(Word{Text: strings.TrimPrefix(t, "--config-env="), Known: true})
		case takesValue(withValue, t):
			i++
			continue
		default:
			continue
		}
		for _, pat := range r.Config {
			if ok, _ := path.Match(strings.ToLower(pat), key); ok {
				return matched
			}
		}
	}
	return noMatch
}

// allowRules reports whether an allow rule's words match the command.
func (ch *checker) allowRules(prog string, args []Word) bool {
	name := path.Base(filepath.ToSlash(prog))
	for _, r := range ch.p.Allow {
		if !ch.progMatches(r[0], prog, false) {
			continue
		}
		if m, _ := ch.matchWords(name, r[1:], args, false); m == matched {
			return true
		}
	}
	return false
}

// findActions are the commands find's -exec, -execdir, -ok and -okdir
// run, each up to its ; or +, with {} an unknown argument.
func findActions(args []Word) [][]Word {
	var out [][]Word
	for i := 0; i < len(args); i++ {
		if !args[i].Known || !slices.Contains([]string{"-exec", "-execdir", "-ok", "-okdir"}, args[i].Text) {
			continue
		}
		var cmd []Word
		for i++; i < len(args); i++ {
			if args[i].Known && (args[i].Text == ";" || args[i].Text == "+") {
				break
			}
			if args[i].Known && args[i].Text == "{}" {
				cmd = append(cmd, Word{Text: "{}"})
				continue
			}
			cmd = append(cmd, args[i])
		}
		out = append(out, cmd)
	}
	return out
}
