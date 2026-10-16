package outbox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
)

// Benchmarks of the outbox as a client would use it
// (docs/design/profile-aries.md). Nothing in aries calls the outbox yet, so
// these are the costs it brings when it does. Run with:
//
//	go test -run '^$' -bench Client -benchmem ./internal/outbox/

// benchEntry is a comment of ordinary length, queued.
func benchEntry(b *testing.B, i int) Entry {
	b.Helper()
	args, err := json.Marshal(proto.CommentArgs{ID: "ar-00000042", Idem: fmt.Sprintf("bench-%d", i),
		Body: strings.Repeat("The second write wins, which loses the first controller's change. ", 4)})
	if err != nil {
		b.Fatal(err)
	}
	return Entry{Op: proto.OpComment, Args: args, Session: "20000000-0000-4000-8000-000000000001", Machine: "laptop"}
}

// BenchmarkClientAppend is one entry queued in an outbox that already
// holds waiting entries: the lock, the whole file read to count them,
// the secrets lint, one write and its sync. entry-bytes is what one
// entry takes in the file.
func BenchmarkClientAppend(b *testing.B) {
	for _, waiting := range []int{0, 100, 900} {
		b.Run(fmt.Sprintf("waiting=%d", waiting), func(b *testing.B) {
			var o *Outbox
			var n int
			fresh := func() {
				dir := b.TempDir()
				var err error
				if o, err = Open(dir, Options{MaxEntries: 1 << 30, MaxBytes: 1 << 40}); err != nil {
					b.Fatal(err)
				}
				for range waiting {
					n++
					if _, err := o.Append(benchEntry(b, n)); err != nil {
						b.Fatal(err)
					}
				}
			}
			fresh()
			b.ReportAllocs()
			for i := 0; b.Loop(); i++ {
				// Keep the file near its size: start again every 50.
				if i%50 == 49 {
					b.StopTimer()
					fresh()
					b.StartTimer()
				}
				n++
				if _, err := o.Append(benchEntry(b, n)); err != nil {
					b.Fatal(err)
				}
			}
			if fi, err := os.Stat(filepath.Join(o.dir, outboxFile)); err == nil && waiting > 0 {
				b.ReportMetric(float64(fi.Size())/float64(waiting+b.N%50+1), "entry-bytes")
			}
		})
	}
}

// BenchmarkClientList is the outbox read for a person, or to see whether
// anything waits.
func BenchmarkClientList(b *testing.B) {
	for _, waiting := range []int{0, 100, 900} {
		b.Run(fmt.Sprintf("waiting=%d", waiting), func(b *testing.B) {
			o, err := Open(b.TempDir(), Options{})
			if err != nil {
				b.Fatal(err)
			}
			for i := range waiting {
				if _, err := o.Append(benchEntry(b, i)); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			for b.Loop() {
				if _, err := o.List(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
