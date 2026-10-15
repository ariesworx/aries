package safetext

import (
	"io"
	"strings"
	"testing"
)

// Benchmarks of the escaping every client applies to what it prints
// (docs/design/profile-aries.md). Run with:
//
//	go test -run '^$' -bench Client -benchmem ./internal/safetext/

const (
	benchClean  = "Reconciler loses the first of two overlapping writes (case 42)"
	benchUnsafe = "Reconciler \x1b[2Jloses the first\u202e of two overlapping writes"
)

// BenchmarkClientLine is one single-line field escaped: text that needs
// nothing, which is the usual case and returns its argument, and text
// that holds an escape sequence and a bidi override.
func BenchmarkClientLine(b *testing.B) {
	for _, tc := range []struct{ name, s string }{{"clean", benchClean}, {"unsafe", benchUnsafe}} {
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(tc.s)))
			b.ReportAllocs()
			for b.Loop() {
				Line(tc.s)
			}
		})
	}
}

// BenchmarkClientJSON is a --json document checked, and escaped when it
// must be: a page of 500 issues, about 60 KiB.
func BenchmarkClientJSON(b *testing.B) {
	clean := []byte(`{"issues":[` + strings.TrimSuffix(strings.Repeat(`{"id":"sf-00000042","title":"`+benchClean+`","status":"open","priority":2},`, 500), ",") + `]}`)
	unsafe := []byte(strings.Replace(string(clean), "loses", "loses\u202e", 1))
	for _, tc := range []struct {
		name string
		doc  []byte
	}{{"clean", clean}, {"unsafe", unsafe}} {
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(tc.doc)))
			b.ReportAllocs()
			for b.Loop() {
				JSON(tc.doc)
			}
		})
	}
}

// BenchmarkClientWriter is text written through a Writer in the pieces a
// printer makes: a line at a time, and a few bytes at a time as
// text/tabwriter writes a table's cells and padding.
func BenchmarkClientWriter(b *testing.B) {
	line := []byte("sf-00000042  P2  open  " + benchClean + "\n")
	for _, tc := range []struct {
		name  string
		piece int
	}{{"line", len(line)}, {"cell", 12}} {
		b.Run(tc.name, func(b *testing.B) {
			w := NewWriter(io.Discard)
			b.SetBytes(int64(len(line)))
			b.ReportAllocs()
			for b.Loop() {
				for i := 0; i < len(line); i += tc.piece {
					if _, err := w.Write(line[i:min(i+tc.piece, len(line))]); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
