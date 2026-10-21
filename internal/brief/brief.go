// Package brief is the brief a worker gets for an issue: the project's
// template of fixed steps (.aries/brief.txt), its rendering for each
// harness, and the record of a brief that the dispatcher and a
// coordinator post and `aries outcome` reads back (docs/design/
// dispatcher.md §7, As built: the brief template). The format, the
// rendering, the record's first line and its hash are stated here once,
// so that every writer and reader of a brief agrees on them.
package brief

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ariesworx/aries/internal/proto"
)

// File is where a project keeps its template, from the top of its
// checkout, beside .aries/workflow.yaml.
const File = ".aries/brief.txt"

// The harnesses a brief is rendered for, as the record's harness= names
// them.
const (
	// Headless is a `claude -p` run the dispatcher started.
	Headless = "claude-code"
	// Desktop is a Claude Desktop session a coordinator started.
	Desktop = "claude-code-desktop"
)

// The lines around a Desktop brief's hand-written section.
const (
	HandBegin = "----- hand-written for this session (by the coordinator) -----"
	HandEnd   = "----- end of hand-written -----"
)

// tags maps a line's tag to the harness that keeps it.
var tags = map[string]string{"[headless]": Headless, "[desktop]": Desktop}

// slots are the names a template may fill.
var slots = []string{"id", "session", "worktree", "branch", "issue", "handwritten"}

const fix = "fix " + File + " as docs/design/dispatcher.md §7 (As built: the brief template) says"

// Template is a parsed template: its version and its lines after the
// header, each already normalized.
type Template struct {
	Version int
	lines   []string
	sum     string
}

// Name is the template's version as a brief's record names it.
func (t *Template) Name() string { return "brief@" + strconv.Itoa(t.Version) }

// Sum is the SHA-256 of the template's text after its header, normalized,
// which the repository's test holds to its version.
func (t *Template) Sum() string { return t.sum }

// Load reads and parses the template at the top of a checkout. A
// missing or malformed file is refused: no built-in copy stands in.
func Load(top string) (*Template, error) {
	data, err := os.ReadFile(filepath.Join(top, File)) //nolint:gosec // the project's template, at the top of the checkout the command runs in
	if errors.Is(err, fs.ErrNotExist) {
		return nil, proto.Errf(proto.CodeInvalid, "write "+File+" at the top of the checkout, as docs/design/dispatcher.md §7 (As built: the brief template) says",
			"no brief template: "+File+" is missing")
	}
	if err != nil {
		return nil, fmt.Errorf("brief template: %w", err)
	}
	t, err := Parse(data)
	if pe, ok := errors.AsType[*proto.Error](err); ok {
		return nil, proto.Errf(pe.Code, pe.Fix, File+": "+pe.Message)
	}
	return t, err
}

// Parse reads a template: comment lines (#) dropped, then `version: N`,
// a blank line and the body. CRLF and trailing space are dropped, and a
// mistyped slot or tag is refused here rather than reaching a model.
func Parse(data []byte) (*Template, error) {
	if !utf8.Valid(data) {
		return nil, proto.Errf(proto.CodeInvalid, fix, "the template is not UTF-8")
	}
	var lines []string
	for l := range strings.SplitSeq(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		l = strings.TrimRight(l, " \t\r")
		if !strings.HasPrefix(l, "#") {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "version:") {
		return nil, proto.Errf(proto.CodeInvalid, fix, "the template has no version: line first")
	}
	v, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(lines[0], "version:")))
	if err != nil || v < 1 {
		return nil, proto.Errf(proto.CodeInvalid, fix, "the template's version: is not a positive integer")
	}
	if len(lines) < 2 || lines[1] != "" {
		return nil, proto.Errf(proto.CodeInvalid, fix, "the template needs a blank line after its version: line")
	}
	body := lines[2:]
	for len(body) > 0 && body[len(body)-1] == "" {
		body = body[:len(body)-1]
	}
	if len(body) == 0 {
		return nil, proto.Errf(proto.CodeInvalid, fix, "the template's body is empty")
	}
	for _, l := range body {
		if tag, _, ok := strings.Cut(l, " "); ok && strings.HasPrefix(tag, "[") && strings.HasSuffix(tag, "]") && tags[tag] == "" {
			return nil, proto.Errf(proto.CodeInvalid, fix, "unknown tag "+tag+"; use [headless] or [desktop]")
		}
		for _, name := range slotNames(l) {
			if !slices.Contains(slots, name) {
				return nil, proto.Errf(proto.CodeInvalid, fix, "unknown slot {"+name+"}; the slots are {"+strings.Join(slots, "}, {")+"}")
			}
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(body, "\n") + "\n"))
	return &Template{Version: v, lines: body, sum: hex.EncodeToString(sum[:])}, nil
}

// slotNames are the {word} tokens of a line whose word is lowercase
// letters alone.
func slotNames(l string) []string {
	var out []string
	for {
		i := strings.IndexByte(l, '{')
		if i < 0 {
			return out
		}
		l = l[i+1:]
		j := strings.IndexByte(l, '}')
		if j < 0 {
			return out
		}
		if isWord(l[:j]) {
			out = append(out, l[:j])
			l = l[j+1:]
		}
	}
}

func isWord(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

// Input is what a brief is rendered from: the issue as start or show
// returned it, the harness, and what the starter knows of the session.
type Input struct {
	Harness  string
	Issue    proto.Issue
	Items    []proto.AcceptanceItem
	Handoff  *proto.Handoff
	Session  string
	Worktree string
	Branch   string
	// Note is a Desktop brief's hand-written section; empty is "(none)".
	Note string
}

// Render is the brief for in: the lines of in's harness, untagged, with
// each slot filled once, so that a value holding a slot's name stays as
// it is. A line that is only a slot rendering empty is dropped, runs of
// blank lines are one, and the text ends in one newline: the same
// template and input give the same bytes.
func (t *Template) Render(in Input) string {
	vals := map[string]string{
		"id": in.Issue.ID, "session": in.Session, "worktree": in.Worktree, "branch": in.Branch,
		"issue": quote(in), "handwritten": hand(in),
	}
	var out []string
	for _, l := range t.lines {
		if tag, rest, ok := strings.Cut(l, " "); ok && tags[tag] != "" {
			if tags[tag] != in.Harness {
				continue
			}
			l = rest
		}
		if name, ok := strings.CutPrefix(l, "{"); ok && strings.HasSuffix(name, "}") && vals[strings.TrimSuffix(name, "}")] == "" && isWord(strings.TrimSuffix(name, "}")) {
			continue
		}
		l = fill(l, vals)
		for s := range strings.SplitSeq(l, "\n") {
			s = strings.TrimRight(s, " \t")
			if s == "" && (len(out) == 0 || out[len(out)-1] == "") {
				continue
			}
			out = append(out, s)
		}
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n") + "\n"
}

// fill replaces each {slot} of l with its value, in one pass.
func fill(l string, vals map[string]string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(l, '{')
		if i < 0 {
			b.WriteString(l)
			return b.String()
		}
		j := strings.IndexByte(l[i:], '}')
		if j < 0 {
			b.WriteString(l)
			return b.String()
		}
		name := l[i+1 : i+j]
		b.WriteString(l[:i])
		if v, ok := vals[name]; ok && isWord(name) {
			b.WriteString(v)
		} else {
			b.WriteString(l[i : i+j+1])
		}
		l = l[i+j+1:]
	}
}

// quote is the issue quoted as data: its fields, open items and last
// handoff between marked lines.
func quote(in Input) string {
	var b strings.Builder
	is := in.Issue
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	w("----- issue %s (data) -----\n", is.ID)
	w("id: %s\ntype: %s\npriority: %d\ntitle: %s\n", is.ID, is.Type, is.Priority, is.Title)
	if is.Body != "" {
		w("body:\n%s\n", strings.TrimRight(is.Body, "\n"))
	}
	if is.Notes != "" {
		w("notes:\n%s\n", strings.TrimRight(is.Notes, "\n"))
	}
	var open []proto.AcceptanceItem
	for _, it := range in.Items {
		if it.State == "" || it.State == "open" {
			open = append(open, it)
		}
	}
	if len(open) > 0 {
		w("open acceptance items:\n")
		for _, it := range open {
			w("  %d. %s\n", it.N, it.Text)
		}
	}
	if h := in.Handoff; h != nil {
		state := h.State
		if state == "" {
			state = "none"
		}
		w("last handoff (state %s, by %s):\n%s\n", state, h.Author, strings.TrimRight(h.Body, "\n"))
	}
	w("----- end of issue %s -----", is.ID)
	return b.String()
}

// hand is a Desktop brief's hand-written section; a headless brief has
// none.
func hand(in Input) string {
	if in.Harness != Desktop {
		return ""
	}
	note := strings.Trim(in.Note, "\n")
	if strings.TrimSpace(note) == "" {
		note = "(none)"
	}
	return HandBegin + "\n" + note + "\n" + HandEnd
}

// Record is a brief's first line: who ran it, on what, under which
// session and template (the memory `briefs`).
type Record struct {
	Harness, Model, Effort, Session, Template string
}

// Line is the record's first line, space-separated key=value fields. A
// value has no space in it: one is replaced by "-", and an empty value
// is "none", so the line parses field by field.
func (r Record) Line() string {
	v := func(s string) string {
		s = strings.Join(strings.Fields(s), "-")
		if s == "" {
			return "none"
		}
		return s
	}
	return fmt.Sprintf("brief: harness=%s model=%s effort=%s session=%s template=%s",
		v(r.Harness), v(r.Model), v(r.Effort), v(r.Session), v(r.Template))
}

// Comment is the comment that records a brief: the record's line, a
// blank line, then the brief verbatim.
func Comment(r Record, brief string) string {
	return r.Line() + "\n\n" + brief
}

// ParseComment reads a `brief:` comment: its record, from the fields of
// its first line it knows, and the brief after it, trimmed.
func ParseComment(body string) (Record, string, bool) {
	first, rest, _ := strings.Cut(body, "\n")
	fields, ok := strings.CutPrefix(first, "brief:")
	if !ok {
		return Record{}, "", false
	}
	var r Record
	for _, f := range strings.Fields(fields) {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		switch k {
		case "harness":
			r.Harness = v
		case "model":
			r.Model = v
		case "effort":
			r.Effort = v
		case "session":
			r.Session = v
		case "template":
			r.Template = v
		}
	}
	return r, strings.TrimSpace(rest), true
}

// Hash is a brief's SHA-256, of its text with surrounding space trimmed:
// the one rule by which the dispatcher's log and the outcome record name
// a brief.
func Hash(brief string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(brief)))
	return hex.EncodeToString(sum[:])
}
