package capture

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// Benchmarks of reading a Claude Code transcript for `aries usage --hook`
// (docs/design/profile-aries.md). Run with:
//
//	go test -run '^$' -bench Client -benchmem ./internal/capture/

const benchSession = "20000000-0000-4000-8000-000000000001"

// benchTranscript is a transcript of n responses: for each, two assistant
// lines and a user line carrying a tool result of tool bytes.
func benchTranscript(n, tool int) []byte {
	var b bytes.Buffer
	result := strings.Repeat("x", tool)
	for i := range n {
		for _, out := range []int{40, 400 + i%300} {
			fmt.Fprintf(&b, `{"type":"assistant","sessionId":%q,"timestamp":"2026-10-13T09:%02d:%02d.000Z","version":"2.1.30",`+
				`"message":{"id":"msg_%012d","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"The reconciler applies the difference."}],`+
				`"usage":{"input_tokens":12,"output_tokens":%d,"cache_creation_input_tokens":900,"cache_read_input_tokens":80000,"cache_creation":{"ephemeral_1h_input_tokens":900}}}}`+"\n",
				benchSession, i/60%60, i%60, i, out)
		}
		fmt.Fprintf(&b, `{"type":"user","sessionId":%q,"message":{"role":"user","content":[{"type":"tool_result","content":%q}]}}`+"\n", benchSession, result)
	}
	return b.Bytes()
}

// BenchmarkClientReadClaude is one read of a transcript from its start:
// what the first hook run of a session pays for the whole file, and each
// later run for the lines added since. Every line is decoded, the tool
// results included, to learn its type.
func BenchmarkClientReadClaude(b *testing.B) {
	for _, tc := range []struct {
		name    string
		n, tool int
	}{{"turn-5x4KiB", 5, 4 << 10}, {"session-300x4KiB", 300, 4 << 10}, {"session-300x100KiB", 300, 100 << 10}} {
		b.Run(tc.name, func(b *testing.B) {
			doc := benchTranscript(tc.n, tc.tool)
			b.SetBytes(int64(len(doc)))
			b.ReportAllocs()
			for b.Loop() {
				fr, err := readClaude(bytes.NewReader(doc), benchSession, true)
				if err != nil {
					b.Fatal(err)
				}
				if len(fr.recs) != tc.n {
					b.Fatalf("read %d records, want %d", len(fr.recs), tc.n)
				}
			}
		})
	}
}
