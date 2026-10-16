package cli

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/safetext"
)

// Benchmarks of the command line apart from the connection
// (docs/design/profile-aries.md). Run with:
//
//	go test -run '^$' -bench Client -benchmem ./internal/cli/

// BenchmarkClientRun is a command that never connects, from Run to its
// exit code: what is left of such a command once the process has started
// and its packages are initialized.
func BenchmarkClientRun(b *testing.B) {
	for _, args := range [][]string{{"--version"}, {"--help"}, {"help", "list"}, {"nosuchcommand"}} {
		b.Run(strings.Join(args, " "), func(b *testing.B) {
			env := Env{Stdout: io.Discard, Stderr: io.Discard, Getenv: func(string) string { return "" }, Version: "v0.2.0"}
			ctx := b.Context()
			b.ReportAllocs()
			for b.Loop() {
				Run(ctx, args, env)
			}
		})
	}
}

// benchCountingWriter counts the writes that reach it, each of which is a
// system call when it is a terminal or a pipe.
type benchCountingWriter struct{ writes, bytes int }

func (w *benchCountingWriter) Write(p []byte) (int, error) {
	w.writes++
	w.bytes += len(p)
	return len(p), nil
}

// BenchmarkClientPrintSummaries is the text of `aries list` and `aries
// ready`: the table of issues written through the escaping writer that
// Run puts in front of standard output. writes/op is how many writes
// reach standard output.
func BenchmarkClientPrintSummaries(b *testing.B) {
	for _, n := range []int{10, 50, 500} {
		b.Run(fmt.Sprintf("issues=%d", n), func(b *testing.B) {
			issues := make([]proto.Summary, n)
			for i := range issues {
				issues[i] = proto.Summary{ID: fmt.Sprintf("ar-%08d", i), Status: "open", Priority: i % 5,
					Title: fmt.Sprintf("Reconciler loses the first of two overlapping writes (case %d)", i)}
			}
			var out benchCountingWriter
			b.ReportAllocs()
			for b.Loop() {
				w := safetext.NewWriter(&out)
				printSummaries(w, issues, nil)
				if err := w.Flush(); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(out.writes)/float64(b.N), "writes/op")
			b.ReportMetric(float64(out.bytes)/float64(b.N), "out-bytes/op")
		})
	}
}
