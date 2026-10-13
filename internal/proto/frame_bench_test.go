package proto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

// The benchmarks of this file are a frame's cost on the wire, apart
// from everything a request does with it (docs/design/profile-starfixd.md):
// what the daemon pays to read a request and to write its answer.
//
//	go test -run '^$' -bench 'Frame' -benchmem ./internal/proto/

// rewind is a reader that gives the same bytes again without end, so
// that a Decoder can be asked for a frame b.N times.
type rewind struct {
	line []byte
	at   int
}

func (r *rewind) Read(p []byte) (int, error) {
	n := copy(p, r.line[r.at:])
	if r.at += n; r.at == len(r.line) {
		r.at = 0
	}
	return n, nil
}

// benchLine is the line of a request for op with args.
func benchLine(tb testing.TB, op string, args any) []byte {
	tb.Helper()
	f, err := Request(7, op, args)
	if err != nil {
		tb.Fatal(err)
	}
	var buf bytes.Buffer
	if err := NewEncoder(&buf).Encode(f); err != nil {
		tb.Fatal(err)
	}
	return buf.Bytes()
}

// BenchmarkFrameDecode reads one request frame: a show, a create with a
// body of 4 KiB, and a create with four texts of 64 KiB, the largest an
// issue takes. A frame's arguments are copied out of the line once
// here, and decoded a second time by the server into the op's type.
func BenchmarkFrameDecode(b *testing.B) {
	text := strings.Repeat("A line of the report, as a person writes one.\n", 1500)[:65535]
	for _, bm := range []struct {
		name string
		line []byte
	}{
		{"show", benchLine(b, OpShow, ShowArgs{ID: "sf-abcd2345"})},
		{"create-4KiB", benchLine(b, OpCreate, CreateArgs{Title: "The parser drops the lease", Body: text[:4096]})},
		{"create-4x64KiB", benchLine(b, OpCreate, CreateArgs{Title: "The parser drops the lease", Body: text, Design: text, Acceptance: text, Notes: text})},
	} {
		b.Run(bm.name, func(b *testing.B) {
			dec := NewDecoder(&rewind{line: bm.line})
			b.SetBytes(int64(len(bm.line)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := dec.Decode(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkFrameEncode builds and writes one answer: a write's id and
// revision, a list of 50 summaries, and an issue with four texts of
// 64 KiB. The result is encoded once into the frame, and the frame
// encoded again around it.
func BenchmarkFrameEncode(b *testing.B) {
	text := strings.Repeat("A line of the report, as a person writes one.\n", 1500)[:65535]
	list := ListResult{}
	for i := range 50 {
		list.Issues = append(list.Issues, Summary{ID: fmt.Sprintf("sf-abcd%04d", i), Status: "open", Priority: i % 4,
			Title: fmt.Sprintf("The parser drops the lease after the socket restarts, case %d", i)})
	}
	for _, bm := range []struct {
		name   string
		result any
	}{
		{"write", WriteResult{ID: "sf-abcd2345", Rev: 7}},
		{"list-50", list},
		{"show-4x64KiB", ShowResult{Issue: Issue{ID: "sf-abcd2345", Title: "The parser drops the lease", Body: text, Design: text,
			Acceptance: text, Notes: text, Status: "open", Type: "task"}}},
	} {
		b.Run(bm.name, func(b *testing.B) {
			enc := NewEncoder(io.Discard)
			size, err := json.Marshal(bm.result)
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(size)))
			b.ReportAllocs()
			for b.Loop() {
				f, err := Response(7, bm.result, nil)
				if err != nil {
					b.Fatal(err)
				}
				if err := enc.Encode(f); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkFrameTraceContext parses the trace context a request frame
// may carry, which the server does for every request.
func BenchmarkFrameTraceContext(b *testing.B) {
	for _, bm := range []struct{ name, traceparent string }{
		{"none", ""},
		{"valid", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
	} {
		b.Run(bm.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, _ = ParseTraceContext(bm.traceparent, "")
			}
		})
	}
}
