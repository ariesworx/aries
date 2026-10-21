package guard

import (
	"bytes"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// show renders a script's commands for comparison: one string per simple
// command, its words joined by "|", an unknown word as "?", assignments
// as NAME=value before them and write redirections after them as ">path".
func show(s Script) []string {
	var out []string
	for _, c := range s.Commands {
		var parts []string
		for _, a := range c.Assigns {
			v := a.Value.Text
			if !a.Value.Known {
				v = "?"
			}
			parts = append(parts, a.Name+"="+v)
		}
		for _, w := range c.Words {
			if w.Known {
				parts = append(parts, w.Text)
			} else {
				parts = append(parts, "?")
			}
		}
		for _, r := range c.Redirs {
			if !r.Write {
				continue
			}
			if r.Target.Known {
				parts = append(parts, ">"+r.Target.Text)
			} else {
				parts = append(parts, ">?")
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	return out
}

func TestParse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		src  string
		want []string
	}{
		{"go test ./...", []string{"go|test|./..."}},
		{"  go   vet\t./... ", []string{"go|vet|./..."}},
		{"git 'push' origin main", []string{"git|push|origin|main"}},
		{`git "push" o\ x`, []string{"git|push|o x"}},
		{`echo "a\"b\\c\$d\e"`, []string{`echo|a"b\c$d\e`}},
		{"echo $'a\\tb\\'c'", []string{"echo|a\tb'c"}},
		{"echo $\"x\"", []string{"echo|x"}},
		{"echo a\\\nb", []string{"echo|ab"}},
		{"a && b || c; d | e |& f & g\nh", []string{"a", "b", "c", "d", "e", "f", "g", "h"}},
		{"go test ./... | tail -20", []string{"go|test|./...", "tail|-20"}},
		{"cd internal/dispatch && go test ./...", []string{"cd|internal/dispatch", "go|test|./..."}},
		{"FOO=bar gh pr merge 1", []string{"FOO=bar|gh|pr|merge|1"}},
		{"GOTOOLCHAIN=local GOFLAGS=-mod=readonly go test -race ./...", []string{"GOTOOLCHAIN=local|GOFLAGS=-mod=readonly|go|test|-race|./..."}},
		{"x=`gh api user`", []string{"gh|api|user", "x=?"}},
		{"echo $(gh pr merge 1)", []string{"gh|pr|merge|1", "echo|?"}},
		{`echo "in $(gh api x) quotes"`, []string{"gh|api|x", "echo|?"}},
		{"echo $(a $(b))", []string{"b", "a|?", "echo|?"}},
		{"(cd x && gh pr merge 1)", []string{"cd|x", "gh|pr|merge|1"}},
		{"{ a; b; }", []string{"a", "b"}},
		{"if true; then gh pr merge 1; else c; fi", []string{"true", "gh|pr|merge|1", "c"}},
		{"for i in 1 $(d); do gh api x; done", []string{"d", "gh|api|x"}},
		{"while a; do b; done > out.txt", []string{"a", "b", ">out.txt"}},
		{"! a", []string{"a"}},
		{"diff <(a) <(b)", []string{"a", "b", "diff|?|?"}},
		{"echo x > .mcp.json", []string{"echo|x|>.mcp.json"}},
		{"echo x >> a 2>&1 >&2 2>/dev/null &> b <c", []string{"echo|x|>a|>/dev/null|>b"}},
		{"echo x 2> e.txt", []string{"echo|x|>e.txt"}},
		{"echo x >| f", []string{"echo|x|>f"}},
		{"echo x > $F", []string{"echo|x|>?"}},
		{"echo 2>x", []string{"echo|>x"}},
		{"echo a2>x", []string{"echo|a2|>x"}},
		{"ls *.go", []string{"ls|?"}},
		{"ls '*.go'", []string{"ls|*.go"}},
		{"echo {a,b}", []string{"echo|?"}},
		{"find . -exec rm {} ;", []string{"find|.|-exec|rm|{}"}},
		{"find . -exec rm {} \\;", []string{"find|.|-exec|rm|{}|;"}},
		{"echo ~/x", []string{"echo|?"}},
		{"echo a~b", []string{"echo|a~b"}},
		{"echo $HOME ${X} $1", []string{"echo|?|?|?"}},
		{"echo cost: $", []string{"echo|cost:|$"}},
		{"[ -f x ] && b", []string{"[|-f|x|]", "b"}},
		{"[[ -f x && $y < 3 ]] || b", []string{"[[|-f|x|&&|?|<|3|]]", "b"}},
		{"echo a # gh pr merge 1\nb", []string{"echo|a", "b"}},
		{"echo a#b", []string{"echo|a#b"}},
		{"cat <<'EOF'\n$(gh pr merge 1)\nEOF\necho done", []string{"cat", "echo|done"}},
		{"git commit -m \"$(cat <<'EOF'\nsubject\n\nbody\nEOF\n)\"", []string{"cat", "git|commit|-m|?"}},
		{"cat <<-EOF\n\tplain $x\n\tEOF\n", []string{"cat"}},
		{"cat <<<\"$(a)\"", []string{"a", "cat"}},
		{"x=1", []string{"x=1"}},
		{"echo if then fi", []string{"echo|if|then|fi"}},
		{"", nil},
		{"# just a comment", nil},
	} {
		s := Parse(tc.src)
		if s.Unreadable != "" {
			t.Errorf("Parse(%q): unreadable: %s", tc.src, s.Unreadable)
			continue
		}
		if got := show(s); !slices.Equal(got, tc.want) {
			t.Errorf("Parse(%q) = %q; want %q", tc.src, got, tc.want)
		}
	}
}

// What the parser is not sure of, it calls unreadable, whatever the rest
// of the command holds.
func TestParseUnreadable(t *testing.T) {
	t.Parallel()
	for _, src := range []string{
		"echo 'open",
		`echo "open`,
		"echo $(open",
		"echo `open",
		"(open",
		"close)",
		"echo $((1+2))",
		"((x++))",
		"echo ${x:-$(gh api user)}",
		"f() { gh pr merge 1; }",
		"function f { a; }",
		"case x in a) b;; esac",
		"select x in a; do b; done",
		"coproc a",
		"for ((i=0;i<3;i++)); do a; done",
		"cat <<EOF\nno end",
		"cat <<EOF\n$(gh pr merge 1)\nEOF\n",
		"cat <<EOF\n`gh api x`\nEOF\n",
		"a &&",
		"&& a",
		"a | | b",
		"echo @(x)",
		strings.Repeat("$(", 9) + "a" + strings.Repeat(")", 9),
		strings.Repeat("(", 9) + "a" + strings.Repeat(")", 9),
		"echo " + strings.Repeat("x", maxSource),
	} {
		if s := Parse(src); s.Unreadable == "" {
			t.Errorf("Parse(%.60q) = %q; want unreadable", src, show(s))
		}
	}
}

// The parser's words for simple commands of literal words are the words
// /bin/sh passes to the program, checked by running printf in its place.
func TestParseAgainstSh(t *testing.T) {
	t.Parallel()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	for _, args := range []string{
		`'a b' c\ d "e\"f" g\\h`,
		`"a\$b" 'c$d' e\$f`,
		`"x\ny" 'x\y' x\y`,
		`a\` + "\n" + `b`,
		`"" '' x""y`,
		`"a\` + "\n" + `b"`,
		`{} \; ';' "|" '&&'`,
		`a#b c=d -e=f`,
		`"$"`,
		`'a'"'"'b'`,
		`"don't"`,
		`a=b`,
	} {
		s := Parse("printf %s\\\\0 " + args)
		if s.Unreadable != "" || len(s.Commands) != 1 {
			t.Errorf("Parse(%q): unreadable %q or %d commands", args, s.Unreadable, len(s.Commands))
			continue
		}
		var want []string
		for _, w := range s.Commands[0].Words[2:] {
			want = append(want, w.Text)
		}
		out, err := exec.Command(sh, "-c", "printf '%s\\0' "+args).Output() //nolint:gosec // the test's own words
		if err != nil {
			t.Fatalf("sh: %v", err)
		}
		got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
		if len(out) == 0 {
			got = nil
		}
		if !slices.Equal(got, want) {
			t.Errorf("words of %q: sh %q, Parse %q", args, got, want)
		}
	}
}

// quote renders word in one of the shell's quoting styles.
func quote(word string, style byte) string {
	switch style % 4 {
	case 0:
		return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
	case 1:
		var b strings.Builder
		b.WriteByte('"')
		for _, r := range word {
			if strings.ContainsRune("\"\\$`", r) {
				b.WriteByte('\\')
			}
			b.WriteRune(r)
		}
		b.WriteByte('"')
		return b.String()
	case 2:
		var b strings.Builder
		for _, r := range word {
			if r == '\n' {
				b.WriteString(`"` + "\n" + `"`)
				continue
			}
			b.WriteByte('\\')
			b.WriteRune(r)
		}
		if word == "" {
			return "''"
		}
		return b.String()
	default:
		return "$'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`, "\n", `\n`, "\t", `\t`).Replace(word) + "'"
	}
}

// FuzzParse holds the parser to two things for any input: it ends
// without panicking, and when it calls a command readable, rendering
// its words back in quotes reads as the same words. And for words it is
// given, quoted in any of the shell's styles, it reads back those words
// or calls the command unreadable: never other words.
func FuzzParse(f *testing.F) {
	f.Add("git push origin main", "push\x00origin main", uint8(0))
	f.Add("echo $(gh api x) `y` \"$z\" <(w) > f", "a'b\x00c\"d", uint8(1))
	f.Add("if a; then b; fi; for i in 1; do c; done", "$x\x00`y`", uint8(2))
	f.Add("cat <<'EOF'\n$(x)\nEOF\n", "\\\x00\n", uint8(3))
	f.Add("[[ a < b ]] && { c; } || (d)", "{a,b}\x00*", uint8(5))
	f.Fuzz(func(t *testing.T, src, words string, styles uint8) {
		s := Parse(src)
		if s.Unreadable == "" {
			for _, c := range s.Commands {
				if !allKnown(c.Words) || len(c.Words) == 0 {
					continue
				}
				var b strings.Builder
				for i, w := range c.Words {
					if i > 0 {
						b.WriteByte(' ')
					}
					b.WriteString(quote(w.Text, 0))
				}
				again := Parse(b.String())
				if again.Unreadable != "" || len(again.Commands) != 1 || !sameWords(again.Commands[0].Words, c.Words) {
					t.Fatalf("Parse(%q) read %q, which rendered as %q reads as %q (%s)", src, show(Script{Commands: []Command{c}}), b.String(), show(again), again.Unreadable)
				}
			}
		}
		if !utf8.ValidString(words) || strings.ContainsRune(words, 0x7f) {
			return
		}
		ws := strings.Split(words, "\x00")
		var b strings.Builder
		b.WriteString("prog")
		for i, w := range ws {
			if strings.ContainsRune(w, 0) {
				return
			}
			b.WriteByte(' ')
			b.WriteString(quote(w, styles>>(i%4*2)))
		}
		got := Parse(b.String())
		if got.Unreadable != "" {
			return
		}
		if len(got.Commands) != 1 {
			t.Fatalf("%q quoted as %q reads as %d commands", ws, b.String(), len(got.Commands))
		}
		var have []string
		for _, w := range got.Commands[0].Words[1:] {
			if !w.Known {
				return // unknown is not wrong: it is never read as a word
			}
			have = append(have, w.Text)
		}
		if !slices.Equal(have, ws) {
			t.Fatalf("%q quoted as %q reads as %q", ws, b.String(), have)
		}
	})
}

func allKnown(ws []Word) bool {
	for _, w := range ws {
		if !w.Known {
			return false
		}
	}
	return true
}

func sameWords(a, b []Word) bool {
	return slices.EqualFunc(a, b, func(x, y Word) bool { return x.Known == y.Known && x.Text == y.Text })
}

// A command's words never hold a NUL from its text: sh cannot pass one.
func TestParseNUL(t *testing.T) {
	t.Parallel()
	if s := Parse("echo a\x00b"); s.Unreadable == "" && bytes.ContainsRune([]byte(show(s)[0]), 0) {
		t.Errorf("NUL read into a word: %q", show(s))
	}
}
