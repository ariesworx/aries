package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// Every command is under one heading of aries help, once, and every name
// there is a command's.
func TestHelpGroups(t *testing.T) {
	t.Parallel()
	seen := map[string]string{}
	for _, g := range helpGroups {
		for _, l := range g.commands {
			if lookup(l.name) == nil || aliases[l.name] != "" {
				t.Errorf("heading %s lists %q, which is no command", g.title, l.name)
			}
			if was, ok := seen[l.name]; ok {
				t.Errorf("%s is under %s and %s", l.name, was, g.title)
			}
			seen[l.name] = g.title
		}
	}
	for _, c := range commands {
		if _, ok := seen[c.name]; !ok {
			t.Errorf("%s is under no heading of aries help", c.name)
		}
	}
	_, out, _ := runCLI(t, "help")
	for _, want := range []string{"Issues:", "Coordination:", "Memory:", "Cost and usage:", "Gates:", "Offline and cache:", "Setup:", "Admin:"} {
		if !strings.Contains(out, "\n"+want+"\n") {
			t.Errorf("aries help has no heading %q:\n%s", want, out)
		}
	}
}

// Help fits a terminal 80 columns wide, and is written to stdout alone.
func TestHelpFits(t *testing.T) {
	t.Parallel()
	runs := [][]string{{"help"}, {"--help"}}
	for _, c := range commands {
		runs = append(runs, []string{"help", c.name}, []string{c.name, "--help"})
	}
	runs = append(runs, []string{"help", "admin", "member"}, []string{"gate", "add", "-h"}, []string{"lock", "run", "-h"})
	for _, args := range runs {
		_, out, errb := runCLI(t, args...)
		if errb != "" {
			t.Errorf("aries %s wrote to stderr: %q", strings.Join(args, " "), errb)
		}
		for line := range strings.SplitSeq(out, "\n") {
			if n := utf8.RuneCountInString(line); n > helpWidth {
				t.Errorf("aries %s: a line of %d columns: %q", strings.Join(args, " "), n, line)
			}
		}
	}
}

// help, -h and --help exit 0 with the help on stdout; bare aries prints
// the help and exits 2, and so does a mistake.
func TestHelpExitCodes(t *testing.T) {
	t.Parallel()
	empty := t.TempDir()
	tests := []struct {
		name string
		args []string
		code int
		// stdout and stderr are what each holds, unwrapped; "" holds anything.
		stdout, stderr []string
		// noStdout says stdout is empty.
		noStdout bool
	}{
		{name: "bare aries", args: nil, code: ExitUsage, stdout: []string{"Issues:", "Admin:"}},
		{name: "help", args: []string{"help"}, code: ExitOK, stdout: []string{"Issues:", "aries help COMMAND"}},
		{name: "-h", args: []string{"-h"}, code: ExitOK, stdout: []string{"Issues:"}},
		{name: "help of a command", args: []string{"help", "start"}, code: ExitOK,
			stdout: []string{"usage: aries start [ID] [--for DURATION] [--take] [--new-branch | --worktree DIR]",
				"flags: --for DURATION how long the claim lasts", "--new-branch", "examples: # Claim the top ready issue aries start"}},
		{name: "--help after a command", args: []string{"start", "--help"}, code: ExitOK,
			stdout: []string{"usage: aries start [ID]", "flags: --for DURATION", "examples:"}},
		{name: "-h among a command's arguments", args: []string{"-C", empty, "start", "ar-a1b2c3d4", "-h"}, code: ExitOK,
			stdout: []string{"usage: aries start [ID]", "examples: # Claim the top ready issue aries start #"}},
		{name: "help of a subcommand", args: []string{"help", "admin", "member"}, code: ExitOK,
			stdout: []string{"usage: aries admin member add NAME", "--role member|admin", "examples: # Make bob an admin of the project"}},
		{name: "--help after a subcommand", args: []string{"gate", "add", "--help"}, code: ExitOK,
			stdout: []string{"flags: --approver P", "--blocks ID", "aries gate add --blocks ar-a1b2c3d4 --approver alice"}},
		{name: "--help of a command that dispatches", args: []string{"gate", "--help"}, code: ExitOK,
			stdout: []string{"usage: aries gate add --blocks ID", "aries gate pass|rearm ID", "examples:"}},
		{name: "two names of one flag are one line", args: []string{"help", "list"}, code: ExitOK,
			stdout: []string{"-t, --type TYPE types", "-l, --label L labels, all required"}},
		{name: "--json and --no-queue are the top help's", args: []string{"help", "ready"}, code: ExitOK,
			stdout: []string{"flags: -n N how many examples:"}},
		{name: "help of an alias", args: []string{"help", "log"}, code: ExitOK, stdout: []string{"usage: aries hours DURATION ID"}},
		{name: "a mistake is still one", args: []string{"ready", "--colour"}, code: ExitUsage, noStdout: true,
			stderr: []string{"flag provided but not defined: -colour", "fix: usage: aries ready [-n N]"}},
		{name: "-h after -- is an argument", args: []string{"-C", empty, "lock", "run", "deploy", "--", "make", "-h"}, code: ExitFailure,
			noStdout: true, stderr: []string{".aries.yaml not found"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, out, errb := runCLI(t, tc.args...)
			if code != tc.code || tc.noStdout && out != "" {
				t.Fatalf("aries %s: exit %d (want %d)\nstdout %q\nstderr %q", strings.Join(tc.args, " "), code, tc.code, out, errb)
			}
			for _, want := range tc.stdout {
				if !strings.Contains(unwrap(out), want) {
					t.Errorf("aries %s: stdout lacks %q:\n%s", strings.Join(tc.args, " "), want, out)
				}
			}
			for _, want := range tc.stderr {
				if !strings.Contains(errb, want) {
					t.Errorf("aries %s: stderr lacks %q: %q", strings.Join(tc.args, " "), want, errb)
				}
			}
		})
	}
}

// Every command has two or three examples, each a command line its flag
// set takes: run with -h, it gets as far as the help of the command, or
// subcommand, it names, past every flag it gives. Examples name only
// example.com, alice and bob (rule 1).
func TestHelpExamples(t *testing.T) {
	t.Parallel()
	for _, c := range commands {
		if n := len(examples[c.name]); n < 2 || n > 3 {
			t.Errorf("%s has %d examples; want two or three", c.name, n)
		}
	}
	for name := range examples {
		if lookup(name) == nil {
			t.Errorf("examples of %q, which is no command", name)
		}
	}
	people := []string{"--to", "--assignee", "--by", "--approver", "--principal", "--owner"}
	for name, exs := range examples {
		for _, ex := range exs {
			words := shellWords(t, ex.line)
			if len(words) < 2 || words[0] != "aries" {
				t.Errorf("%s: example %q is not an aries command line", name, ex.line)
				continue
			}
			if len("  "+ex.line) > helpWidth || len("  # "+ex.does) > helpWidth {
				t.Errorf("%s: example %q is wider than %d columns", name, ex.line, helpWidth)
			}
			for i, w := range words {
				if strings.Contains(w, "://") && !strings.Contains(w, "example.com") {
					t.Errorf("%s: example %q names a host that is not example.com", name, ex.line)
				}
				if i > 0 && slices.Contains(people, words[i-1]) && w != "alice" && w != "bob" {
					t.Errorf("%s: example %q names %q; examples name alice and bob", name, ex.line, w)
				}
			}
			checkExample(t, name, words[1:])
		}
	}
}

// checkExample runs an example's arguments, with -h before any "--", as
// commandHelp runs them, and fails unless they reached a flag set's -h.
func checkExample(t *testing.T, name string, args []string) {
	t.Helper()
	line := strings.Join(args, " ")
	var out, errb bytes.Buffer
	r := &runner{env: Env{Stdout: &out, Stderr: &errb, Getenv: func(string) string { return "" }}.withDefaults(), dir: t.TempDir()}
	r.wrapOutput()
	args, err := r.globals(args)
	if err != nil || len(args) == 0 {
		t.Errorf("example aries %s: %v", line, err)
		return
	}
	c := lookup(args[0])
	if c == nil || c.name != name {
		t.Errorf("example aries %s is not of %s", line, name)
		return
	}
	rest := args[1:]
	at := slices.Index(rest, "--")
	if at < 0 {
		at = len(rest)
	}
	rest = slices.Concat(rest[:at], []string{"-h"}, rest[at:])
	r.helping = true
	err = c.run(t.Context(), r, rest)
	if ue, ok := errors.AsType[*usageError](err); !ok || ue.flags == nil {
		t.Errorf("example aries %s does not parse: %v", line, err)
	}
}

// shellWords splits an example as a POSIX shell would: at spaces, with
// "double quotes" around a word that has them. Examples use no other
// quoting.
func shellWords(t *testing.T, line string) []string {
	t.Helper()
	var words []string
	var cur strings.Builder
	quoted, in := false, false
	for _, ch := range line {
		switch {
		case ch == '"':
			quoted, in = !quoted, true
		case ch == ' ' && !quoted:
			if in {
				words, in = append(words, cur.String()), false
				cur.Reset()
			}
		default:
			cur.WriteRune(ch)
			in = true
		}
	}
	if quoted {
		t.Fatalf("example %q has an unclosed quote", line)
	}
	if in {
		words = append(words, cur.String())
	}
	return words
}

func TestSplitUsage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		usage, name string
		want        []string
	}{
		{"ready [-n N]", "ready", []string{"ready [-n N]"}},
		{"hours DURATION ID | hours --undo ENTRY", "hours", []string{"hours DURATION ID", "hours --undo ENTRY"}},
		{"admin plan list | set NAME | admin status", "admin", []string{"admin plan list | set NAME", "admin status"}},
		{"dep add|remove FROM TO", "dep", []string{"dep add|remove FROM TO"}},
	}
	for _, tc := range tests {
		if got := splitUsage(tc.usage, tc.name); !slices.Equal(got, tc.want) {
			t.Errorf("splitUsage(%q) = %q, want %q", tc.usage, got, tc.want)
		}
	}
}

func TestUsageUnits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		usage string
		want  []string
	}{
		{"start [ID] [--new-branch | --worktree DIR]", []string{"start", "[ID]", "[--new-branch | --worktree DIR]"}},
		{"setup [--global [--key-file PATH]] AGENT", []string{"setup", "[--global [--key-file PATH]]", "AGENT"}},
		{"label add|remove ID LABEL...", []string{"label", "add|remove", "ID", "LABEL..."}},
	}
	for _, tc := range tests {
		if got := usageUnits(tc.usage); !slices.Equal(got, tc.want) {
			t.Errorf("usageUnits(%q) = %q, want %q", tc.usage, got, tc.want)
		}
	}
}

// docs/cli.md quotes `aries help start` as aries prints it (rule 17).
func TestDocsQuoteHelp(t *testing.T) {
	t.Parallel()
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "cli.md"))
	if err != nil {
		t.Fatal(err)
	}
	const prompt = "$ aries help start\n"
	_, quoted, ok := strings.Cut(string(doc), prompt)
	if !ok {
		t.Fatalf("docs/cli.md does not quote %q", prompt)
	}
	quoted, _, _ = strings.Cut(quoted, "```")
	if _, out, _ := runCLI(t, "help", "start"); quoted != out {
		t.Errorf("docs/cli.md quotes aries help start as\n%s\nand it prints\n%s", quoted, out)
	}
}
