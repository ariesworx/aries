package logs

import (
	"flag"
	"os"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/names.golden from the code")

// names lists, one a line, every event and field name a line may have.
func names() []string {
	var out []string
	for _, e := range Events() {
		out = append(out, "event "+string(e))
	}
	for _, k := range Keys() {
		out = append(out, "field "+k)
	}
	slices.Sort(out)
	return out
}

// TestNamesKept holds the names of log events and fields to the promise
// of docs/server.md, Names that are kept: from v0.5.0 on none is renamed or
// removed, and only additions are made. testdata/names.golden
// records every name; a new one is recorded with -update, after which
// it is kept too.
func TestNamesKept(t *testing.T) {
	t.Parallel()
	const golden = "testdata/names.golden"
	got := names()
	if *update {
		if err := os.WriteFile(golden, []byte(strings.Join(got, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read %s: %v; run go test -run TestNamesKept -update to make it", golden, err)
	}
	kept := strings.Split(strings.TrimSpace(string(b)), "\n")
	for _, k := range kept {
		if !slices.Contains(got, k) {
			t.Errorf("%q is gone: a name is kept from v0.5.0 on (docs/server.md, Names that are kept)", k)
		}
	}
	for _, g := range got {
		if !slices.Contains(kept, g) {
			t.Errorf("%q is new: record it with go test -run TestNamesKept -update, and it is kept from then on", g)
		}
	}
}
