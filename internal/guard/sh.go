package guard

import (
	"strings"
)

// The parser's limits (docs/design/guard.md §5): a longer command, or one
// nested deeper, cannot be read.
const (
	maxSource = 64 << 10
	maxDepth  = 8
)

// Word is one word of a command after quote removal. Known says its text
// is what the shell passes: false when an unquoted or double-quoted
// expansion ($VAR, $(…), `…`), a glob, a brace expansion or a tilde could
// make it something else, and then Text is only what the source held.
type Word struct {
	Text  string
	Known bool
}

// Assign is a NAME=value in front of a command, or a command of its own.
type Assign struct {
	Name  string
	Value Word
}

// Redir is a redirection. Write says it opens Target for writing (>,
// >>, >|, &>, &>>, <>, >&FILE); a duplication of a descriptor (2>&1)
// and a here-document have no Target to check.
type Redir struct {
	Op     string
	Target Word
	Write  bool
}

// Command is one simple command: the assignments in front of it, its
// words (the program first) and its redirections. A command of
// assignments or redirections alone has no words.
type Command struct {
	Assigns []Assign
	Words   []Word
	Redirs  []Redir
}

// Script is what Parse read of a command line: every simple command in
// it, those in pipelines, lists, compound commands and command and
// process substitutions included, each substitution before the command
// that holds it. Unreadable, when not empty, is why some part of the
// line could not be read; Commands then holds only what was.
type Script struct {
	Commands   []Command
	Unreadable string
}

// Parse reads src as POSIX sh, with bash's [[ … ]], |&, &>, <<<, $'…' and
// <( … ). It runs nothing and expands nothing; what it is not sure of,
// it calls unreadable rather than guess.
func Parse(src string) Script {
	p := &parser{src: src}
	if len(src) > maxSource {
		p.fail("the command is longer than 64 KiB")
		return p.out
	}
	if strings.IndexByte(src, 0) >= 0 {
		p.fail("a NUL byte")
		return p.out
	}
	p.list(0, false)
	if p.out.Unreadable == "" && p.pos < len(p.src) {
		p.fail("unexpected " + quoteChar(p.src[p.pos]))
	}
	if p.out.Unreadable == "" && len(p.pending) > 0 {
		p.fail("a here-document has no end")
	}
	return p.out
}

// heredoc is a here-document whose body is still to be read, at the
// next newline.
type heredoc struct {
	delim string
	tabs  bool // <<-: leading tabs are stripped
	raw   bool // the delimiter was quoted: no expansion in the body
}

type parser struct {
	src     string
	pos     int
	out     Script
	pending []heredoc
}

func (p *parser) fail(why string) {
	if p.out.Unreadable == "" {
		p.out.Unreadable = why
	}
	p.pos = len(p.src)
}

func (p *parser) failed() bool { return p.out.Unreadable != "" }

func quoteChar(c byte) string { return "`" + string(c) + "`" }

func (p *parser) peek(s string) bool { return strings.HasPrefix(p.src[p.pos:], s) }

// blanks skips spaces, tabs and line continuations.
func (p *parser) blanks() {
	for p.pos < len(p.src) {
		switch {
		case p.src[p.pos] == ' ' || p.src[p.pos] == '\t':
			p.pos++
		case p.peek("\\\n"):
			p.pos += 2
		default:
			return
		}
	}
}

// comment skips a comment to the end of its line.
func (p *parser) comment() {
	for p.pos < len(p.src) && p.src[p.pos] != '\n' {
		p.pos++
	}
}

// newline consumes a newline and the bodies of the here-documents its
// line began.
func (p *parser) newline() {
	p.pos++
	pending := p.pending
	p.pending = nil
	for _, h := range pending {
		p.heredocBody(h)
		if p.failed() {
			return
		}
	}
}

func (p *parser) heredocBody(h heredoc) {
	for {
		if p.pos >= len(p.src) {
			p.fail("a here-document has no end")
			return
		}
		line := p.src[p.pos:]
		if end := strings.IndexByte(line, '\n'); end >= 0 {
			line = line[:end]
			p.pos += end + 1
		} else {
			p.pos = len(p.src)
		}
		if h.tabs {
			line = strings.TrimLeft(line, "\t")
		}
		if line == h.delim {
			return
		}
		if !h.raw && (strings.Contains(line, "$(") || strings.Contains(line, "`") || strings.Contains(line, "${")) {
			p.fail("a here-document runs a command substitution")
			return
		}
	}
}

// list reads commands joined by operators and newlines until the end, or
// a `)` when inParen; depth is the nesting so far.
func (p *parser) list(depth int, inParen bool) {
	if depth > maxDepth {
		p.fail("the command is nested more than eight deep")
		return
	}
	needCmd := false // after &&, || or |: a command must follow
	prev := false    // a command came last: &&, || or | may follow
	for !p.failed() {
		p.blanks()
		if p.pos >= len(p.src) {
			break
		}
		c := p.src[p.pos]
		switch {
		case c == '#':
			p.comment()
		case c == '\n':
			p.newline()
		case c == ')':
			if !inParen {
				p.fail("unexpected `)`")
				return
			}
			if needCmd {
				p.fail("an operator has no command after it")
			}
			return
		case p.peek(";;"):
			p.fail("`;;` outside case")
			return
		case p.peek("&&") || p.peek("||") || p.peek("|&"):
			if needCmd || !prev {
				p.fail("an operator has no command before it")
				return
			}
			p.pos += 2
			needCmd = true
			continue
		case c == '|':
			if needCmd || !prev {
				p.fail("an operator has no command before it")
				return
			}
			p.pos++
			needCmd = true
			continue
		case c == ';' || (c == '&' && !p.peek("&>")):
			p.pos++
		default:
			at := p.pos
			p.command(depth)
			if p.pos == at && !p.failed() {
				p.fail("unexpected " + quoteChar(c))
			}
			needCmd, prev = false, true
			continue
		}
		prev = false
		if needCmd && !p.failed() && c != '\n' && c != '#' {
			p.fail("an operator has no command after it")
		}
	}
	if needCmd && !p.failed() {
		p.fail("an operator has no command after it")
	}
}

// reserved words that open or continue a compound command, after which
// a command follows; and those that end one.
var (
	openers = map[string]bool{"if": true, "then": true, "else": true, "elif": true, "do": true,
		"while": true, "until": true, "!": true, "{": true}
	closers = map[string]bool{"fi": true, "done": true, "}": true, "esac": true}
	refused = map[string]string{
		"case": "case", "select": "select", "function": "a function", "coproc": "coproc",
	}
)

// command reads one command at the parser's position: a compound
// command's opening word, a subshell, [[ … ]] or a simple command.
func (p *parser) command(depth int) {
	if p.peek("(") {
		if p.peek("((") {
			p.fail("arithmetic")
			return
		}
		p.pos++
		p.list(depth+1, true)
		if !p.closeParen() {
			return
		}
		p.simple(depth, nil, true)
		return
	}
	if c := p.src[p.pos]; c == '<' || c == '>' || c == '&' {
		p.simple(depth, nil, false)
		return
	}
	start, seen := p.pos, len(p.out.Commands)
	w, plain, ok := p.word(depth)
	if !ok {
		return
	}
	p.out.Commands = p.out.Commands[:seen] // read again below, by simple
	if plain {
		switch {
		case openers[w.Text]:
			return // a command follows, which list reads
		case closers[w.Text]:
			p.simple(depth, nil, true) // redirections of the compound command
			return
		case refused[w.Text] != "":
			p.fail(refused[w.Text] + " is not read")
			return
		case w.Text == "for":
			p.forLoop(depth)
			return
		case w.Text == "[[":
			p.test(depth)
			return
		}
	}
	p.pos = start
	p.simple(depth, nil, false)
}

// forLoop reads `for NAME [in WORDS]` up to its separator; the body
// follows as commands after `do`.
func (p *parser) forLoop(depth int) {
	p.blanks()
	if p.peek("((") {
		p.fail("arithmetic")
		return
	}
	if _, plain, ok := p.word(depth); !ok || !plain {
		p.fail("a for loop without a name")
		return
	}
	p.blanks()
	if !p.peek("in") || (len(p.src) > p.pos+2 && !isBreak(p.src[p.pos+2])) {
		return
	}
	p.pos += 2
	for !p.failed() {
		p.blanks()
		if p.pos >= len(p.src) || p.src[p.pos] == ';' || p.src[p.pos] == '\n' {
			return
		}
		if isMeta(p.src[p.pos]) {
			p.fail("unexpected " + quoteChar(p.src[p.pos]) + " in a for loop")
			return
		}
		if _, _, ok := p.word(depth); !ok {
			return
		}
	}
}

// test reads the words of [[ … ]] after its opening, where && || < > (
// ) and ! are the test's own and run nothing.
func (p *parser) test(depth int) {
	cmd := Command{Words: []Word{{Text: "[[", Known: true}}}
	for !p.failed() {
		p.blanks()
		if p.pos >= len(p.src) || p.src[p.pos] == '\n' {
			p.fail("[[ without ]]")
			return
		}
		if p.peek("]]") && (p.pos+2 == len(p.src) || isBreak(p.src[p.pos+2])) {
			p.pos += 2
			cmd.Words = append(cmd.Words, Word{Text: "]]", Known: true})
			break
		}
		if op := p.testOp(); op != "" {
			p.pos += len(op)
			cmd.Words = append(cmd.Words, Word{Text: op, Known: true})
			continue
		}
		if w, _, ok := p.word(depth); ok {
			cmd.Words = append(cmd.Words, w)
		}
	}
	if !p.failed() {
		p.out.Commands = append(p.out.Commands, cmd)
	}
}

// testOp is the operator of [[ … ]] at the parser's position, if any.
func (p *parser) testOp() string {
	for _, op := range []string{"&&", "||", "<", ">", "(", ")", "!"} {
		if p.peek(op) {
			return op
		}
	}
	return ""
}

// closeParen consumes the `)` a list stopped at, or fails when the input
// ended first.
func (p *parser) closeParen() bool {
	if p.failed() {
		return false
	}
	if p.pos >= len(p.src) || p.src[p.pos] != ')' {
		p.fail("a `(` is not closed")
		return false
	}
	p.pos++
	return true
}

// isMeta reports whether c ends an unquoted word.
func isMeta(c byte) bool {
	switch c {
	case ' ', '\t', '\n', ';', '&', '|', '(', ')', '<', '>':
		return true
	}
	return false
}

// isBreak reports whether c may follow a reserved word.
func isBreak(c byte) bool { return isMeta(c) }

// simple reads a simple command's assignments, words and redirections.
// With redirsOnly, it reads only redirections: those after a compound
// command, which apply to it.
func (p *parser) simple(depth int, cmd *Command, redirsOnly bool) {
	if cmd == nil {
		cmd = &Command{}
	}
	for !p.failed() {
		p.blanks()
		if p.pos >= len(p.src) {
			break
		}
		c := p.src[p.pos]
		if (c == '<' || c == '>') && p.pos+1 < len(p.src) && p.src[p.pos+1] == '(' {
			if redirsOnly {
				p.fail("unexpected process substitution")
				return
			}
			p.pos += 2
			p.list(depth+1, true)
			if !p.closeParen() {
				return
			}
			cmd.Words = append(cmd.Words, Word{})
			continue
		}
		if p.redirect(depth, cmd) {
			continue
		}
		if p.failed() {
			return
		}
		if c == '(' {
			p.fail("a function or an unexpected `(`")
			return
		}
		if c == '#' || isMeta(c) {
			break
		}
		if redirsOnly {
			p.fail("unexpected word after a compound command")
			return
		}
		start := p.pos
		w, _, ok := p.word(depth)
		if !ok {
			return
		}
		if len(cmd.Words) == 0 {
			if name, ok := assignName(p.src[start:p.pos]); ok {
				v := w
				v.Text = w.Text[len(name)+1:]
				name = strings.TrimSuffix(name, "+")
				cmd.Assigns = append(cmd.Assigns, Assign{Name: name, Value: v})
				continue
			}
		}
		cmd.Words = append(cmd.Words, w)
	}
	if len(cmd.Assigns)+len(cmd.Words)+len(cmd.Redirs) > 0 {
		p.out.Commands = append(p.out.Commands, *cmd)
	}
}

// assignName is the NAME (or NAME+) of raw when raw, as written, begins
// NAME= or NAME+= with NAME unquoted.
func assignName(raw string) (string, bool) {
	i := 0
	for i < len(raw) && (raw[i] == '_' || raw[i] >= 'a' && raw[i] <= 'z' || raw[i] >= 'A' && raw[i] <= 'Z' || i > 0 && raw[i] >= '0' && raw[i] <= '9') {
		i++
	}
	if i == 0 {
		return "", false
	}
	if strings.HasPrefix(raw[i:], "=") {
		return raw[:i], true
	}
	if strings.HasPrefix(raw[i:], "+=") {
		return raw[:i+1], true
	}
	return "", false
}

// redirOps are the redirection operators, longest first.
var redirOps = []string{"&>>", "<<<", "<<-", "&>", ">>", ">|", ">&", "<<", "<>", "<&", ">", "<"}

// redirect reads a redirection at the parser's position, with the
// descriptor number in front of it if any, into cmd. It reports whether
// there was one.
func (p *parser) redirect(depth int, cmd *Command) bool {
	i := p.pos
	for i < len(p.src) && p.src[i] >= '0' && p.src[i] <= '9' {
		i++
	}
	op := ""
	for _, o := range redirOps {
		if strings.HasPrefix(p.src[i:], o) {
			op = o
			break
		}
	}
	if op == "" || (i > p.pos && op[0] == '&') {
		return false
	}
	p.pos = i + len(op)
	p.blanks()
	if p.pos >= len(p.src) || isMeta(p.src[p.pos]) {
		p.fail("a redirection without a file")
		return false
	}
	if op == "<<" || op == "<<-" {
		start := p.pos
		w, _, ok := p.word(depth)
		if !ok {
			return false
		}
		raw := p.src[start:p.pos]
		if !w.Known && !strings.ContainsAny(raw, `'"\`) {
			p.fail("a here-document's delimiter is not plain")
			return false
		}
		p.pending = append(p.pending, heredoc{delim: w.Text, tabs: op == "<<-", raw: strings.ContainsAny(raw, `'"\`)})
		cmd.Redirs = append(cmd.Redirs, Redir{Op: op})
		return true
	}
	w, _, ok := p.word(depth)
	if !ok {
		return false
	}
	r := Redir{Op: op, Target: w}
	switch op {
	case ">", ">>", ">|", "&>", "&>>", "<>":
		r.Write = true
	case ">&":
		r.Write = !w.Known || !isDup(w.Text)
	}
	cmd.Redirs = append(cmd.Redirs, r)
	return true
}

// isDup reports whether s, after >& or <&, names a descriptor or `-`.
func isDup(s string) bool {
	if s == "-" {
		return true
	}
	s = strings.TrimSuffix(s, "-")
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// word reads one word at the parser's position, removing quotes as the
// shell would, and the commands in its substitutions into the script.
// plain says the word was written with no quoting or expansion at all,
// which a reserved word must be.
func (p *parser) word(depth int) (w Word, plain, ok bool) {
	var b strings.Builder
	known, plain := true, true
	start := p.pos
	if p.pos < len(p.src) && p.src[p.pos] == '~' {
		known = false
	}
	for p.pos < len(p.src) && !p.failed() {
		c := p.src[p.pos]
		if isMeta(c) {
			break
		}
		switch c {
		case 0:
			p.fail("a NUL byte")
		case '\\':
			plain = false
			switch {
			case p.peek("\\\n"):
				p.pos += 2
			case p.pos+1 < len(p.src):
				b.WriteByte(p.src[p.pos+1])
				p.pos += 2
			default:
				b.WriteByte('\\')
				p.pos++
			}
		case '\'':
			plain = false
			end := strings.IndexByte(p.src[p.pos+1:], '\'')
			if end < 0 {
				p.fail("a single quote is not closed")
				break
			}
			b.WriteString(p.src[p.pos+1 : p.pos+1+end])
			p.pos += end + 2
		case '"':
			plain = false
			p.pos++
			if !p.doubleQuoted(depth, &b) {
				known = false
			}
		case '$':
			plain = false
			switch {
			case p.peek("$'"):
				if !p.ansiC(&b) {
					known = false
				}
			case p.peek("$\""):
				p.pos += 2
				if !p.doubleQuoted(depth, &b) {
					known = false
				}
			default:
				if p.dollar(depth, &b) {
					known = false
				}
			}
		case '`':
			plain = false
			known = false
			p.backquote(depth)
		case '*', '?':
			known = false
			b.WriteByte(c)
			p.pos++
		case '[':
			if strings.ContainsRune(p.rest(), ']') {
				known = false
			}
			b.WriteByte(c)
			p.pos++
		case '{':
			if rest := p.rest(); strings.ContainsRune(rest, '}') && (p.pos != start || rest != "{}") {
				known = false
			}
			b.WriteByte(c)
			p.pos++
		default:
			b.WriteByte(c)
			p.pos++
		}
	}
	if p.failed() {
		return Word{}, false, false
	}
	if p.pos == start {
		p.fail("an empty word")
		return Word{}, false, false
	}
	return Word{Text: b.String(), Known: known}, plain, true
}

// rest is the unquoted remainder of the word at the parser's position,
// up to its first quote, expansion or end.
func (p *parser) rest() string {
	i := p.pos
	for i < len(p.src) && !isMeta(p.src[i]) && !strings.ContainsRune(`'"\$`+"`", rune(p.src[i])) {
		i++
	}
	return p.src[p.pos:i]
}

// doubleQuoted reads the inside of "…" after its opening quote into b,
// and reports whether its text is known.
func (p *parser) doubleQuoted(depth int, b *strings.Builder) bool {
	known := true
	for p.pos < len(p.src) && !p.failed() {
		c := p.src[p.pos]
		switch c {
		case '"':
			p.pos++
			return known
		case '\\':
			if p.pos+1 < len(p.src) && strings.IndexByte("$`\"\\\n", p.src[p.pos+1]) >= 0 {
				if p.src[p.pos+1] != '\n' {
					b.WriteByte(p.src[p.pos+1])
				}
				p.pos += 2
				continue
			}
			b.WriteByte('\\')
			p.pos++
		case '$':
			if p.dollar(depth, b) {
				known = false
			}
		case '`':
			known = false
			p.backquote(depth)
		case 0:
			p.fail("a NUL byte")
		default:
			b.WriteByte(c)
			p.pos++
		}
	}
	p.fail("a double quote is not closed")
	return false
}

// dollar reads what follows a `$`. It reports whether it was an
// expansion; a lone `$` is the character itself, written to b.
func (p *parser) dollar(depth int, b *strings.Builder) bool {
	p.pos++ // the $
	if p.pos >= len(p.src) {
		b.WriteByte('$')
		return false
	}
	c := p.src[p.pos]
	switch {
	case c == '(':
		if p.peek("((") {
			p.fail("arithmetic")
			return true
		}
		p.pos++
		p.list(depth+1, true)
		p.closeParen()
		return true
	case c == '{':
		end := strings.IndexByte(p.src[p.pos:], '}')
		if end < 0 {
			p.fail("a ${ is not closed")
			return true
		}
		inner := p.src[p.pos+1 : p.pos+end]
		if strings.ContainsAny(inner, "$`'\"\\{") {
			p.fail("a parameter expansion holds an expansion or quotes")
			return true
		}
		p.pos += end + 1
		return true
	case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
		for p.pos < len(p.src) && (p.src[p.pos] == '_' || p.src[p.pos] >= 'a' && p.src[p.pos] <= 'z' ||
			p.src[p.pos] >= 'A' && p.src[p.pos] <= 'Z' || p.src[p.pos] >= '0' && p.src[p.pos] <= '9') {
			p.pos++
		}
		return true
	case c >= '0' && c <= '9' || strings.IndexByte("@*#?$!-", c) >= 0:
		p.pos++
		return true
	}
	b.WriteByte('$')
	return false
}

// ansiC reads $'…' into b, and reports whether it decoded every escape.
func (p *parser) ansiC(b *strings.Builder) bool {
	p.pos += 2
	known := true
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch c {
		case '\'':
			p.pos++
			return known
		case '\\':
			if p.pos+1 >= len(p.src) {
				p.fail("a $' is not closed")
				return false
			}
			e := p.src[p.pos+1]
			p.pos += 2
			if r, ok := ansiEscapes[e]; ok {
				b.WriteByte(r)
			} else {
				known = false // \x, \u, \c and octal: not decoded
			}
		default:
			b.WriteByte(c)
			p.pos++
		}
	}
	p.fail("a $' is not closed")
	return false
}

var ansiEscapes = map[byte]byte{'a': '\a', 'b': '\b', 'e': 0x1b, 'E': 0x1b, 'f': '\f', 'n': '\n',
	'r': '\r', 't': '\t', 'v': '\v', '\\': '\\', '\'': '\'', '"': '"', '?': '?'}

// backquote reads `…` and parses what it holds as a script of its own.
func (p *parser) backquote(depth int) {
	p.pos++
	var b strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '`' {
			p.pos++
			if depth+1 > maxDepth {
				p.fail("the command is nested more than eight deep")
				return
			}
			inner := &parser{src: b.String()}
			inner.list(depth+1, false)
			if inner.out.Unreadable == "" && len(inner.pending) > 0 {
				inner.fail("a here-document has no end")
			}
			p.out.Commands = append(p.out.Commands, inner.out.Commands...)
			if inner.out.Unreadable != "" {
				p.fail(inner.out.Unreadable)
			}
			return
		}
		if c == '\\' && p.pos+1 < len(p.src) && strings.IndexByte("`\\$", p.src[p.pos+1]) >= 0 {
			b.WriteByte(p.src[p.pos+1])
			p.pos += 2
			continue
		}
		b.WriteByte(c)
		p.pos++
	}
	p.fail("a backquote is not closed")
}
