package proto

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

// Benchmarks of the frame codec as the client uses it
// (docs/design/profile-aries.md). Run with:
//
//	go test -run '^$' -bench Client -benchmem ./internal/proto/

// clientBenchFrame returns one encoded res frame, newline included, whose
// result is a list of issue summaries of about size bytes.
func clientBenchFrame(b *testing.B, size int) []byte {
	b.Helper()
	var res ListResult
	for i, n := 0, 0; n < size; i++ {
		s := Summary{ID: fmt.Sprintf("sf-%08d", i), Status: "open", Priority: i % 5,
			Title: fmt.Sprintf("Reconciler loses the first of two overlapping writes (case %d)", i)}
		res.Issues = append(res.Issues, s)
		n += len(s.ID) + len(s.Title) + 50
	}
	f, err := Response(1, res, nil)
	if err != nil {
		b.Fatal(err)
	}
	line, err := json.Marshal(f)
	if err != nil {
		b.Fatal(err)
	}
	return append(line, '\n')
}

// clientBenchLoop is a stream that repeats one frame for ever.
type clientBenchLoop struct {
	frame []byte
	off   int
}

func (l *clientBenchLoop) Read(p []byte) (int, error) {
	n := copy(p, l.frame[l.off:])
	l.off = (l.off + n) % len(l.frame)
	return n, nil
}

var clientBenchSizes = []struct {
	name string
	size int
}{{"300B", 300}, {"8KiB", 8 << 10}, {"64KiB", 64 << 10}, {"1MiB", 1 << 20}, {"3.9MiB", MaxFrame - 100<<10}}

// BenchmarkClientDecode is one frame read by a Decoder that has already
// read frames of that size: the frame's own cost, the copy of its result
// in Frame.OK, and for a frame over 4 KiB the buffer it is gathered in.
func BenchmarkClientDecode(b *testing.B) {
	for _, tc := range clientBenchSizes {
		b.Run(tc.name, func(b *testing.B) {
			frame := clientBenchFrame(b, tc.size)
			d := NewDecoder(&clientBenchLoop{frame: frame})
			if _, err := d.Decode(); err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(frame)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := d.Decode(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkClientDecodeFirst is the first frame of a connection: a new
// Decoder and its 4 KiB buffer, and the frame.
func BenchmarkClientDecodeFirst(b *testing.B) {
	for _, tc := range clientBenchSizes {
		b.Run(tc.name, func(b *testing.B) {
			frame := clientBenchFrame(b, tc.size)
			b.SetBytes(int64(len(frame)))
			b.ReportAllocs()
			for b.Loop() {
				d := NewDecoder(&clientBenchLoop{frame: frame})
				if _, err := d.Decode(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkClientDecodeResult is what a call does with a frame once it
// has it: the result decoded into its type, which copies every string a
// second time.
func BenchmarkClientDecodeResult(b *testing.B) {
	for _, tc := range clientBenchSizes {
		b.Run(tc.name, func(b *testing.B) {
			frame := clientBenchFrame(b, tc.size)
			f, err := NewDecoder(strings.NewReader(string(frame))).Decode()
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(f.OK)))
			b.ReportAllocs()
			for b.Loop() {
				var res ListResult
				if err := json.Unmarshal(f.OK, &res); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkClientEncodeRequest is one request frame built and written.
func BenchmarkClientEncodeRequest(b *testing.B) {
	enc := NewEncoder(io.Discard)
	args := ListArgs{Status: []string{"open", "in_progress", "blocked", "deferred"}, Limit: 50}
	b.ReportAllocs()
	for b.Loop() {
		f, err := Request(7, OpList, args)
		if err != nil {
			b.Fatal(err)
		}
		if err := enc.Encode(f); err != nil {
			b.Fatal(err)
		}
	}
}
