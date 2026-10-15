package cli

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
)

// countingWriter counts the writes that reach it.
type countingWriter struct {
	bytes.Buffer
	writes int
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.Buffer.Write(p)
}

// The issue table reaches its writer in one write, whatever its length:
// written cell by cell, `sfx list -n 500` was 4,000 writes to standard
// output (docs/design/profile-sfx.md, proposal 6).
func TestPrintSummariesOneWrite(t *testing.T) {
	t.Parallel()
	for _, n := range []int{1, 10, 500} {
		issues := make([]proto.Summary, n)
		for i := range issues {
			issues[i] = proto.Summary{ID: fmt.Sprintf("sf-%08d", i), Status: "open", Priority: i % 5, Title: fmt.Sprintf("Case %d", i)}
		}
		var w countingWriter
		printSummaries(&w, issues, nil)
		if w.writes != 1 {
			t.Errorf("printSummaries of %d issues made %d writes, want 1", n, w.writes)
		}
	}
}

// The table's text: columns padded to the widest cell, an extra column
// where the caller adds one, and titles with an escape sequence and with
// characters wider than a byte, as text/tabwriter and the escaping lay
// them out today.
func TestPrintSummariesText(t *testing.T) {
	t.Parallel()
	issues := []proto.Summary{
		{ID: "sf-abcd1234", Status: "open", Priority: 1, Title: "The parser drops the lease"},
		{ID: "sf-ef56", Status: "in_progress", Priority: 0, Title: "Résumé \x1b[31mred\x1b[0m"},
		{ID: "sf-abcd1234.1", Status: "blocked", Priority: 3, Title: "Wide 漢字 title"},
	}
	var w bytes.Buffer
	printSummaries(&w, issues, func(i int) string { return fmt.Sprintf("\t(%d)", i) })
	want := "sf-abcd1234    P1  open         The parser drops the lease  (0)\n" +
		"sf-ef56        P0  in_progress  Résumé \\x1b[31mred\\x1b[0m   (1)\n" +
		"sf-abcd1234.1  P3  blocked      Wide 漢字 title               (2)\n"
	if got := w.String(); got != want {
		t.Errorf("printSummaries =\n%s\nwant\n%s", got, want)
	}
}
