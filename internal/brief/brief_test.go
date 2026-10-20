package brief

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
)

const sample = "# a comment\n" +
	"version: 3\n" +
	"\n" +
	"[headless] Headless on {id}, session {session}.\n" +
	"[desktop] Desktop on {id}.\n" +
	"{issue}\n" +
	"\n" +
	"{handwritten}\n" +
	"\n" +
	"1. Work in {worktree} on {branch}.   \n" +
	"[desktop] 2. Turn on Auto-fix.\n" +
	"[headless] 2. Say the sweep takes over.\n" +
	"\n\n"

func TestParse(t *testing.T) {
	t.Parallel()
	tp, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if tp.Version != 3 || tp.Name() != "brief@3" {
		t.Errorf("version %d, name %q", tp.Version, tp.Name())
	}
	// The hash is of the text after the header, so line endings and
	// trailing space do not change it, and the version line is not in it.
	crlf, err := Parse([]byte(strings.ReplaceAll(sample, "\n", "\r\n")))
	if err != nil {
		t.Fatal(err)
	}
	if crlf.Sum() != tp.Sum() || len(tp.Sum()) != 64 {
		t.Errorf("sums %q and %q", crlf.Sum(), tp.Sum())
	}
	other, _ := Parse([]byte(strings.Replace(sample, "Auto-fix", "Autofix", 1)))
	if other.Sum() == tp.Sum() {
		t.Error("a change of wording keeps the sum")
	}
	bumped, _ := Parse([]byte(strings.Replace(sample, "version: 3", "version: 4", 1)))
	if bumped.Sum() != tp.Sum() {
		t.Error("the version line changes the sum")
	}
}

func TestParseRefuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, text, want string }{
		{"empty", "", "version"},
		{"no version", "# c\nhello {id}\n", "version"},
		{"bad version", "version: one\n\nx\n", "version"},
		{"zero version", "version: 0\n\nx\n", "version"},
		{"no blank line", "version: 1\nx\n", "blank line"},
		{"no body", "version: 1\n\n", "empty"},
		{"unknown slot", "version: 1\n\nhello {name}\n", "{name}"},
		{"unknown tag", "version: 1\n\n[codex] hello\n", "[codex]"},
		{"not utf-8", "version: 1\n\n\xff\n", "UTF-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tc.text))
			var pe *proto.Error
			if !errors.As(err, &pe) || pe.Code != proto.CodeInvalid || pe.Fix == "" || !strings.Contains(pe.Message, tc.want) {
				t.Fatalf("got %v, want an invalid error naming %q with a fix", err, tc.want)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	t.Parallel()
	top := t.TempDir()
	_, err := Load(top)
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeInvalid || !strings.Contains(pe.Message, File) || !strings.Contains(pe.Fix, "dispatcher.md") {
		t.Fatalf("a missing template: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(top, ".aries"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(top, File), []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}
	tp, err := Load(top)
	if err != nil || tp.Version != 3 {
		t.Fatalf("load: %v, %v", tp, err)
	}
	if err := os.WriteFile(filepath.Join(top, File), []byte("version: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(top); err == nil || !strings.Contains(err.Error(), File) {
		t.Fatalf("a malformed template: %v", err)
	}
}

func TestRender(t *testing.T) {
	t.Parallel()
	tp, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	in := Input{
		Issue:    proto.Issue{ID: "ar-1", Type: "bug", Priority: 2, Title: "Export {id} to CSV", Body: "Body of ar-1\n"},
		Items:    []proto.AcceptanceItem{{N: 1, Text: "Exports", State: "open"}, {N: 2, Text: "Done already", State: "met"}},
		Session:  "s-1",
		Worktree: "/repo/wt/ar-1",
		Branch:   "fix/ar-1-export",
	}
	headless := in
	headless.Harness = Headless
	want := "Headless on ar-1, session s-1.\n" +
		"----- issue ar-1 (data) -----\n" +
		"id: ar-1\ntype: bug\npriority: 2\ntitle: Export {id} to CSV\nbody:\nBody of ar-1\n" +
		"open acceptance items:\n  1. Exports\n" +
		"----- end of issue ar-1 -----\n" +
		"\n" +
		"1. Work in /repo/wt/ar-1 on fix/ar-1-export.\n" +
		"2. Say the sweep takes over.\n"
	got := tp.Render(headless)
	if got != want {
		t.Errorf("headless:\n%s\nwant:\n%s", got, want)
	}
	if tp.Render(headless) != got {
		t.Error("two renderings of one input differ")
	}

	desktop := in
	desktop.Harness = Desktop
	got = tp.Render(desktop)
	for _, w := range []string{"Desktop on ar-1.\n", HandBegin + "\n(none)\n" + HandEnd + "\n", "2. Turn on Auto-fix.\n"} {
		if !strings.Contains(got, w) {
			t.Errorf("desktop lacks %q:\n%s", w, got)
		}
	}
	for _, no := range []string{"Headless", "sweep", "[desktop]"} {
		if strings.Contains(got, no) {
			t.Errorf("desktop holds %q:\n%s", no, got)
		}
	}
	desktop.Note = "Leave the permission list alone.\n\n"
	if got := tp.Render(desktop); !strings.Contains(got, HandBegin+"\nLeave the permission list alone.\n"+HandEnd+"\n") {
		t.Errorf("the note is not in the hand-written section:\n%s", got)
	}
}

func TestRecord(t *testing.T) {
	t.Parallel()
	r := Record{Harness: Headless, Model: "claude-haiku-5-5", Effort: "default", Session: "s-1", Template: "brief@1"}
	if got := r.Line(); got != "brief: harness=claude-code model=claude-haiku-5-5 effort=default session=s-1 template=brief@1" {
		t.Errorf("line %q", got)
	}
	text := "the brief\n"
	c := Comment(r, text)
	back, rest, ok := ParseComment(c)
	if !ok || back != r || rest != "the brief" {
		t.Errorf("parsed %+v %q %v", back, rest, ok)
	}
	if Hash(text) != Hash(rest) || Hash(text) != Hash("  the brief  \n\n") || len(Hash(text)) != 64 {
		t.Error("the hash is not of the trimmed brief")
	}
	// A value with a space would split; it is replaced, so the line
	// still parses field by field.
	odd := Record{Harness: "claude code", Model: "m", Effort: "", Session: "s 2", Template: "t"}
	back, _, _ = ParseComment(Comment(odd, "x"))
	if back.Harness != "claude-code" || back.Session != "s-2" || back.Effort != "none" {
		t.Errorf("odd values: %+v", back)
	}
	if _, _, ok := ParseComment("lesson: x"); ok {
		t.Error("a lesson parsed as a brief")
	}
}
