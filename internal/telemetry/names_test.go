package telemetry

import (
	"flag"
	"os"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/names.golden from the code")

// names lists, one a line, every name ariesd's metrics and traces
// report: each instrument with its Prometheus name, each attribute value
// an instrument lists, each span and each key a span carries.
func names() []string {
	var out []string
	for _, d := range Instruments() {
		out = append(out, "instrument "+d.Name+" "+d.Prometheus)
	}
	for _, sp := range specs {
		for _, a := range sp.attrs {
			out = append(out, "attribute "+sp.name+" "+a.key)
			for _, v := range a.values {
				out = append(out, "value "+sp.name+" "+a.key+" "+v)
			}
		}
	}
	for _, s := range Spans() {
		out = append(out, "span "+s)
	}
	for _, k := range SpanKeys() {
		out = append(out, "span-key "+k)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// TestNamesKept holds the names of metrics, attribute values, spans and
// span keys to the promise of docs/server.md, Names that are kept: from
// v0.5.0 on none is renamed or removed, and only additions are made. testdata/names.golden records every name; a new one is recorded
// with -update, after which it is kept too.
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
