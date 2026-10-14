package telemetry

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// The server guide's table of instruments is the code's: every
// instrument has a row with its name, its name on /metrics and its
// attributes, and the guide lists every value an attribute may take.
func TestGuideListsEveryInstrument(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("../../docs/server.md")
	if err != nil {
		t.Fatal(err)
	}
	guide := string(b)
	rows := map[string]string{}
	for line := range strings.Lines(guide) {
		if cells := strings.Split(line, "|"); len(cells) >= 5 && strings.HasPrefix(strings.TrimSpace(cells[1]), "`starfix.") {
			rows[strings.Trim(strings.TrimSpace(cells[1]), "`")] = line
		}
	}
	for _, d := range Instruments() {
		row, ok := rows[d.Name]
		if !ok {
			t.Errorf("docs/server.md has no row for %s in its table of instruments", d.Name)
			continue
		}
		cells := strings.Split(row, "|")
		if got := strings.Trim(strings.TrimSpace(cells[2]), "`"); got != d.Prometheus {
			t.Errorf("docs/server.md: %s is %q on /metrics, the code says %q", d.Name, got, d.Prometheus)
		}
		if !strings.Contains(cells[3], strings.Split(string(d.Kind), " ")[0]) {
			t.Errorf("docs/server.md: %s is described as %q, the code says %s", d.Name, strings.TrimSpace(cells[3]), d.Kind)
		}
		var keys []string
		for _, k := range strings.Split(cells[4], ",") {
			if k = strings.Trim(strings.TrimSpace(k), "`"); k != "none" && k != "" {
				keys = append(keys, k)
			}
		}
		if !slices.Equal(keys, d.Keys) {
			t.Errorf("docs/server.md: %s has attributes %v, the code says %v", d.Name, keys, d.Keys)
		}
		delete(rows, d.Name)
	}
	for name := range rows {
		t.Errorf("docs/server.md lists the instrument %s, which the code does not have", name)
	}
	// Every listed value of every attribute is in the guide, in backticks.
	for _, sp := range specs {
		for _, a := range sp.attrs {
			for _, v := range a.values {
				if !strings.Contains(guide, "`"+v+"`") {
					t.Errorf("docs/server.md does not list %q, a value of %s's attribute %s", v, sp.name, a.key)
				}
			}
		}
	}
	for _, bound := range []string{"5, 10, 25, 50, 100, 250 and", "500 ms, then 1, 2.5, 5 and 10 s"} {
		if !strings.Contains(guide, bound) {
			t.Errorf("docs/server.md does not give the histogram buckets as %q; Buckets is %v", bound, Buckets)
		}
	}
}

// The server guide's Traces section is the code's: every span has a row
// with its name, every attribute key a span may have has a row with
// what it holds, and every outcome, every sampler and every value of
// telemetry: principals: is named.
func TestGuideListsEverySpan(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("../../docs/server.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(b), "\n### Traces\n")
	if !ok {
		t.Fatal("docs/server.md has no section headed ### Traces")
	}
	section, _, _ = strings.Cut(section, "\n### ")
	// The first cell of each table row, which may name several things.
	rows := map[string]bool{}
	for line := range strings.Lines(section) {
		if cells := strings.Split(line, "|"); len(cells) >= 3 {
			for name := range strings.SplitSeq(cells[1], ",") {
				rows[strings.Trim(strings.TrimSpace(name), "`")] = true
			}
		}
	}
	for _, name := range Spans() {
		if !rows[name] {
			t.Errorf("docs/server.md, Traces: no row for the span %s", name)
		}
	}
	for _, key := range SpanKeys() {
		if !rows[key] {
			t.Errorf("docs/server.md, Traces: no row for the attribute %s", key)
		}
	}
	for _, word := range slices.Concat(outcomes, []string{Unknown, Other, PrincipalsPseudonym, PrincipalsName, PrincipalsOmit,
		samplerAlwaysOn, samplerAlwaysOff, samplerRatio, samplerParentOn, samplerParentOff, samplerParentRatio}) {
		if !strings.Contains(section, "`"+word+"`") {
			t.Errorf("docs/server.md, Traces does not name `%s`", word)
		}
	}
}
