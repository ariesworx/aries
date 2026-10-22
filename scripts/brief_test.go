package scripts_test

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/brief"
	"github.com/ariesworx/aries/internal/proto"
)

var updateBrief = flag.Bool("update", false, "record the brief template's new version in testdata/brief-versions.golden")

const briefGolden = "testdata/brief-versions.golden"

// The brief template's wording ships by review and with a raised
// version (docs/design/dispatcher.md §7, As built: the brief template).
// The golden holds each version's SHA-256, one line each, oldest first:
// a change of text under a version already recorded fails, as does a
// version not above every recorded one; -update appends a new version.
func TestBriefTemplate(t *testing.T) {
	t.Parallel()
	tp, err := brief.Load("..")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(briefGolden)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	recorded := map[int]string{}
	last := 0
	for i, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if l == "" {
			continue
		}
		name, sum, ok := strings.Cut(l, " ")
		v, err := strconv.Atoi(strings.TrimPrefix(name, "brief@"))
		if !ok || err != nil || !strings.HasPrefix(name, "brief@") || len(sum) != 64 {
			t.Fatalf("%s:%d: want \"brief@N SHA256\", got %q", briefGolden, i+1, l)
		}
		if v <= last {
			t.Fatalf("%s:%d: brief@%d follows brief@%d; versions only rise", briefGolden, i+1, v, last)
		}
		recorded[v], last = sum, v
	}
	switch sum, ok := recorded[tp.Version]; {
	case ok && sum == tp.Sum():
	case ok:
		t.Fatalf("%s changed but its version did not: raise its version: to %d, then run go test ./scripts/ -run TestBriefTemplate -update", brief.File, last+1)
	case tp.Version <= last:
		t.Fatalf("%s is at version %d, not above the last recorded, %d: raise it to %d", brief.File, tp.Version, last, last+1)
	case !*updateBrief:
		t.Fatalf("%s's version %d is not recorded: run go test ./scripts/ -run TestBriefTemplate -update", brief.File, tp.Version)
	default:
		line := fmt.Sprintf("%s %s\n", tp.Name(), tp.Sum())
		if err := os.MkdirAll(filepath.Dir(briefGolden), 0o750); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(briefGolden, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // a file of this repository
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(line); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// The repository's template renders for both harnesses with every slot
// filled and none of its tags left.
func TestBriefTemplateRenders(t *testing.T) {
	t.Parallel()
	tp, err := brief.Load("..")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{brief.Headless, brief.Desktop} {
		got := tp.Render(brief.Input{Harness: h, Session: "s-1", Worktree: "/wt", Branch: "fix/x-1-y",
			Issue: proto.Issue{ID: "x-1", Type: "bug", Title: "A title"}})
		for _, no := range []string{"{", "[headless]", "[desktop]", "\n\n\n"} {
			if strings.Contains(got, no) {
				t.Errorf("%s brief holds %q:\n%s", h, no, got)
			}
		}
		for _, want := range []string{"x-1", "`workflow`", "`handoff`", "Aries: x-1", "never merge",
			"leave the CI item and the review and merge item open", "never end a turn while a command runs"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s brief lacks %q:\n%s", h, want, got)
			}
		}
	}
}
