package guard

import (
	"strings"
)

// optSpec is a wrapper's options: those that take no value, those that
// take one (written apart, attached to a short option, or after = for a
// long one), and how many operands come before the command it runs.
type optSpec struct {
	flags, values []string
	operands      int
	// numeric allows -N (nice -10).
	numeric bool
}

// wrappers are the programs that run the command after their options
// (docs/design/guard.md §5). The shells, env, find, watch, flock and
// xargs have rules of their own below.
var wrappers = map[string]optSpec{
	"command": {flags: []string{"-p"}},
	"builtin": {},
	"exec":    {flags: []string{"-c", "-l"}, values: []string{"-a"}},
	"nohup":   {},
	"noglob":  {},
	"time":    {flags: []string{"-p", "-v", "--portability", "--verbose", "-a", "--append", "-q", "--quiet"}, values: []string{"-o", "-f", "--output", "--format"}},
	"timeout": {flags: []string{"--preserve-status", "--foreground", "-v", "--verbose"}, values: []string{"-s", "--signal", "-k", "--kill-after"}, operands: 1},
	"nice":    {values: []string{"-n", "--adjustment"}, numeric: true},
	"stdbuf":  {values: []string{"-i", "-o", "-e", "--input", "--output", "--error"}},
	"ionice":  {flags: []string{"-t", "--ignore"}, values: []string{"-c", "-n", "--class", "--classdata"}},
	"setsid":  {flags: []string{"-c", "-f", "-w", "--ctty", "--fork", "--wait"}},
	"xargs": {flags: []string{"-0", "-r", "-t", "-p", "-x", "-o", "--null", "--no-run-if-empty", "--verbose", "--interactive", "--exit", "--open-tty"},
		values: []string{"-a", "-d", "-E", "-I", "-L", "-n", "-P", "-s", "--arg-file", "--delimiter", "--eof", "--max-lines", "--max-args", "--max-procs", "--max-chars", "--process-slot-var", "--replace"}},
	"flock": {flags: []string{"-s", "-x", "-u", "-n", "-o", "-F", "--shared", "--exclusive", "--unlock", "--nonblock", "--nb", "--close", "--verbose", "--no-fork"},
		values: []string{"-w", "-E", "--timeout", "--wait", "--conflict-exit-code"}, operands: 1},
	"watch": {flags: []string{"-d", "-t", "-b", "-e", "-g", "-c", "-x", "-p", "-r", "-w", "--differences", "--no-title", "--beep", "--errexit", "--chgexit", "--color", "--exec", "--precise", "--no-rerun", "--no-wrap"},
		values: []string{"-n", "-q", "--interval", "--equexit"}},
	"env": {flags: []string{"-i", "-", "-0", "-v", "--ignore-environment", "--null", "--debug"},
		values: []string{"-u", "-C", "-S", "--unset", "--chdir", "--split-string"}},
}

// shells run the string after -c, or a script file.
var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "ash": true}

// opt is one option a wrapper was given, with its value if it takes one.
type opt struct {
	name  string
	value Word
}

// split reads a wrapper's options from args, and returns them, its
// operands and the command it runs. why says why they could not be read.
func (s optSpec) split(args []Word) (opts []opt, operands, rest []Word, why string) {
	contains := func(xs []string, x string) bool {
		for _, y := range xs {
			if y == x {
				return true
			}
		}
		return false
	}
	i := 0
	for i < len(args) {
		a := args[i]
		if !a.Known {
			return nil, nil, nil, "a wrapper's option is not known before it runs"
		}
		t := a.Text
		if t == "--" {
			i++
			break
		}
		if !strings.HasPrefix(t, "-") || t == "-" && !contains(s.flags, "-") {
			break
		}
		name, val, eq := strings.Cut(t, "=")
		switch {
		case contains(s.flags, t):
			opts = append(opts, opt{name: t})
			i++
		case contains(s.values, t):
			if i+1 >= len(args) {
				return nil, nil, nil, "a wrapper's option has no value"
			}
			opts = append(opts, opt{name: t, value: args[i+1]})
			i += 2
		case eq && strings.HasPrefix(t, "--") && contains(s.values, name):
			opts = append(opts, opt{name: name, value: Word{Text: val, Known: true}})
			i++
		case len(t) > 2 && t[1] != '-' && contains(s.values, t[:2]):
			opts = append(opts, opt{name: t[:2], value: Word{Text: t[2:], Known: true}})
			i++
		case s.numeric && len(t) > 1 && strings.Trim(t[1:], "0123456789") == "":
			i++
		default:
			return nil, nil, nil, "a wrapper's option it does not know"
		}
	}
	if len(args)-i < s.operands {
		return nil, nil, nil, "a wrapper has no command to run"
	}
	return opts, args[i : i+s.operands], args[i+s.operands:], ""
}

func has(opts []opt, names ...string) (Word, bool) {
	for _, o := range opts {
		for _, n := range names {
			if o.name == n {
				return o.value, true
			}
		}
	}
	return Word{}, false
}

// wrapper checks what a wrapper runs. isWrapper says prog is one; then
// allowed says whether what it runs is allowed (the wrapper alone allows
// nothing).
func (ch *checker) wrapper(name, prog string, args []Word, tail bool, depth int) (allowed, isWrapper bool) {
	for _, w := range ch.p.Wrappers {
		if ch.progMatches(w, prog, true) {
			i := 0
			for i < len(args) && args[i].Known && strings.HasPrefix(args[i].Text, "-") {
				i++
			}
			if i < len(args) && !args[i].Known {
				ch.unread("a wrapper's option is not known before it runs")
				return false, true
			}
			if i == len(args) {
				return false, true
			}
			return ch.words(args[i:], tail, depth+1), true
		}
	}
	if shells[name] {
		return ch.shell(args, depth), true
	}
	spec, ok := wrappers[name]
	if !ok {
		return false, false
	}
	opts, _, rest, why := spec.split(args)
	if why != "" {
		ch.unread(why)
		return false, true
	}
	switch name {
	case "command":
		if len(rest) > 0 && rest[0].Known && (rest[0].Text == "-v" || rest[0].Text == "-V") {
			return false, true // runs nothing
		}
	case "ionice":
		if _, pids := has(opts, "-p", "-P", "-u"); pids {
			return false, true
		}
	case "env":
		return ch.env(opts, rest, tail, depth), true
	case "flock":
		if cmd, ok := has(opts, "-c", "--command"); ok {
			return ch.script(cmd, depth+1), true
		}
		if len(rest) > 0 && rest[0].Known && (rest[0].Text == "-c" || rest[0].Text == "--command") {
			if len(rest) < 2 {
				ch.unread("flock -c has no command")
				return false, true
			}
			return ch.script(rest[1], depth+1), true
		}
	case "watch":
		if _, exec := has(opts, "-x", "--exec"); !exec {
			var b strings.Builder
			for i, w := range rest {
				if !w.Known {
					ch.unread("the command watch runs is not known before it runs")
					return false, true
				}
				if i > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(w.Text)
			}
			return ch.script(Word{Text: b.String(), Known: true}, depth+1), true
		}
	case "xargs":
		if len(rest) == 0 {
			return false, true // echo
		}
		return ch.words(rest, true, depth+1), true
	}
	if len(rest) == 0 {
		return false, true
	}
	return ch.words(rest, tail, depth+1), true
}

// env checks env's assignments and the command it runs; -S's string is
// read as a command.
func (ch *checker) env(opts []opt, rest []Word, tail bool, depth int) bool {
	ok := true
	if _, cd := has(opts, "-C", "--chdir"); cd {
		ch.dirChanged = true
	}
	if s, split := has(opts, "-S", "--split-string"); split {
		if len(rest) > 0 {
			ch.unread("env -S with arguments after its string")
			return false
		}
		return ch.script(s, depth+1)
	}
	for len(rest) > 0 && rest[0].Known {
		name, ok2 := assignName(rest[0].Text)
		if !ok2 || strings.HasSuffix(name, "+") {
			break
		}
		if !ch.assign(Assign{Name: name, Value: Word{Text: rest[0].Text[len(name)+1:], Known: true}}) {
			ok = false
		}
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return false // prints the environment
	}
	return ch.words(rest, tail, depth+1) && ok
}

// shell checks what sh, bash, zsh, dash or ksh runs: the string after
// -c, or a script file with its arguments. A shell that reads its
// commands from standard input cannot be read.
func (ch *checker) shell(args []Word, depth int) bool {
	cmode, stdin := false, false
	i := 0
loop:
	for i < len(args) {
		a := args[i]
		if !a.Known {
			ch.unread("a shell's option is not known before it runs")
			return false
		}
		t := a.Text
		switch {
		case t == "--" || t == "-":
			i++
			break loop
		case t == "--rcfile" || t == "--init-file":
			i += 2
		case strings.HasPrefix(t, "--"):
			switch t {
			case "--login", "--norc", "--noprofile", "--posix", "--restricted", "--noediting", "--verbose":
				i++
			default:
				ch.unread("a shell's option it does not know")
				return false
			}
		case len(t) > 1 && (t[0] == '-' || t[0] == '+'):
			i++
			for _, f := range t[1:] {
				switch f {
				case 'c':
					cmode = true
				case 's', 'i':
					stdin = true
				case 'o', 'O':
					i++ // the option's name
				}
			}
		default:
			break loop
		}
	}
	switch {
	case cmode && i < len(args):
		return ch.script(args[i], depth+1)
	case cmode, stdin, i >= len(args):
		ch.unread("a shell reads its commands from standard input")
		return false
	}
	// A script file: checked as the program it is.
	file := args[i]
	if file.Known && !strings.Contains(file.Text, "/") {
		file.Text = "./" + file.Text
	}
	return ch.words(append([]Word{file}, args[i+1:]...), false, depth+1)
}
