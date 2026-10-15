package proto

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"
)

// scanDecoder is the Decoder as it was before it stopped keeping its
// largest buffer (docs/design/profile-ariesd.md, proposal 1): a
// bufio.Scanner that starts at 64 KiB and doubles to MaxFrame. The tests
// below hold Decoder to it, frame for frame and error for error.
type scanDecoder struct{ s *bufio.Scanner }

func newScanDecoder(r io.Reader) *scanDecoder {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64<<10), MaxFrame)
	return &scanDecoder{s: s}
}

func (d *scanDecoder) Decode() (*Frame, error) {
	if !d.s.Scan() {
		err := d.s.Err()
		switch {
		case err == nil:
			return nil, io.EOF
		case errors.Is(err, bufio.ErrTooLong):
			return nil, ErrFrameTooLarge
		default:
			return nil, fmt.Errorf("read frame: %w", err)
		}
	}
	var f Frame
	if err := json.Unmarshal(d.s.Bytes(), &f); err != nil {
		return nil, fmt.Errorf("decode frame: %w", err)
	}
	if f.T == "" {
		return nil, errors.New("decode frame: no frame type")
	}
	return &f, nil
}

// decoded is what one Decode call gave, in a form two calls can be
// compared by.
type decoded struct {
	frame string // as JSON
	err   string
	eof   bool
	large bool
}

func (d decoded) String() string {
	return fmt.Sprintf("{frame %.100s, err %q, EOF %v, too large %v}", d.frame, d.err, d.eof, d.large)
}

func newDecoded(f *Frame, err error) decoded {
	var d decoded
	if f != nil {
		b, _ := json.Marshal(f)
		d.frame = string(b)
	}
	if err != nil {
		d.err, d.eof, d.large = err.Error(), errors.Is(err, io.EOF), errors.Is(err, ErrFrameTooLarge)
	}
	return d
}

// decodeAll reads frames until a final error: io.EOF, ErrFrameTooLarge,
// or a read error. A line that does not decode is not final, as Decode
// documents. After io.EOF or a read error it calls Decode once more,
// which must give the same error again.
//
// Not after ErrFrameTooLarge: the Scanner then gave the first MaxFrame
// bytes of the line that was too long as the next frame, a call the
// documentation says is not made and no caller makes. Decoder gives
// ErrFrameTooLarge again (TestDecoderTooLargeIsFinal).
func decodeAll(dec interface{ Decode() (*Frame, error) }) []decoded {
	var out []decoded
	for range 1 << 16 {
		d := newDecoded(dec.Decode())
		out = append(out, d)
		if d.large {
			return out
		}
		if d.eof || strings.HasPrefix(d.err, "read frame") {
			break
		}
	}
	return append(out, newDecoded(dec.Decode()))
}

// sameAsScanner fails t unless Decoder and the Scanner it replaced read
// the same frames and errors from the stream that reader makes of in.
func sameAsScanner(t *testing.T, in []byte, reader func(io.Reader) io.Reader) {
	t.Helper()
	got := decodeAll(NewDecoder(reader(bytes.NewReader(in))))
	want := decodeAll(newScanDecoder(reader(bytes.NewReader(in))))
	if len(got) != len(want) {
		t.Fatalf("Decode of %.80q: %d results, want %d:\n got %v\nwant %v", in, len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("Decode of %.80q, call %d = %v, want %v", in, i, got[i], want[i])
		}
	}
}

// readers are the ways a stream reaches the decoder: as it is, a byte at
// a time, in halves, in pieces of 7, with its end arriving with its last
// bytes, and broken after them. Not iotest.TimeoutReader: it fails the
// second Read, and which bytes that leaves read depends on the size of
// the decoder's buffer rather than on the stream, which a real deadline
// does not.
var readers = []struct {
	name string
	wrap func(io.Reader) io.Reader
}{
	{"whole", func(r io.Reader) io.Reader { return r }},
	{"bytes", iotest.OneByteReader},
	{"halves", iotest.HalfReader},
	{"pieces", func(r io.Reader) io.Reader { return &pieces{r: r, n: 7} }},
	{"data and EOF", iotest.DataErrReader},
	{"broken", func(r io.Reader) io.Reader {
		return io.MultiReader(r, iotest.ErrReader(errors.New("connection reset")))
	}},
}

// pieces reads at most n bytes a call.
type pieces struct {
	r io.Reader
	n int
}

func (p *pieces) Read(b []byte) (int, error) { return p.r.Read(b[:min(len(b), p.n)]) }

// Decoder reads what the Scanner it replaced read, at the frame size
// limit and around it, with carriage returns, blank lines, bad lines,
// and a stream that ends or breaks in the middle of a line.
func TestDecoderMatchesScanner(t *testing.T) {
	t.Parallel()
	// line is a frame whose line is n bytes long, newline not counted.
	line := func(n int) string {
		head, tail := `{"t":"req","a":"`, `"}`
		return head + strings.Repeat("x", n-len(head)-len(tail)) + tail
	}
	small := `{"t":"req","id":1,"op":"show","a":{"id":"sf-abcd1234"}}`
	tests := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"one frame", small + "\n"},
		{"two frames", small + "\n" + small + "\n"},
		{"no final newline", small},
		{"carriage return", small + "\r\n" + small + "\r"},
		{"two carriage returns", small + "\r\r\n"},
		{"blank lines", "\n\n" + small + "\n\r\n"},
		{"not json", "nope\n" + small + "\n"},
		{"no type", `{"id":1}` + "\n" + small + "\n"},
		{"only a carriage return", "\r"},
		{"4 KiB", line(4<<10) + "\n" + small + "\n"},
		{"just over 4 KiB", line(4<<10+1) + "\n" + small + "\n"},
		{"64 KiB", line(64<<10) + "\n" + small + "\n"},
		{"1 MiB", line(1<<20) + "\n" + small + "\n"},
		{"largest", line(MaxFrame-1) + "\n" + small + "\n"},
		{"largest with a carriage return", line(MaxFrame-2) + "\r\n" + small + "\n"},
		{"largest at the end", line(MaxFrame - 1)},
		{"one over", line(MaxFrame) + "\n" + small + "\n"},
		{"one over at the end", line(MaxFrame)},
		{"one over with a carriage return", line(MaxFrame-1) + "\r\n" + small + "\n"},
		{"far over", small + "\n" + line(MaxFrame+100<<10) + "\n" + small + "\n"},
		{"large after small", small + "\n" + line(3<<20) + "\n" + small + "\n" + line(MaxFrame-1) + "\n"},
	}
	for _, tc := range tests {
		for _, r := range readers {
			if len(tc.in) > 64<<10 && (r.name == "bytes" || r.name == "pieces") {
				continue // the same reads as halves, a byte or 7 at a time
			}
			if tc.name == "one over at the end" && r.name == "data and EOF" {
				// A last line of MaxFrame bytes and no newline is too
				// large, as the Scanner had it for every reader but one
				// that returns the line's last bytes together with
				// io.EOF: the Scanner then took the line whole.
				continue
			}
			t.Run(tc.name+"/"+r.name, func(t *testing.T) {
				t.Parallel()
				sameAsScanner(t, []byte(tc.in), r.wrap)
			})
		}
	}
}

// A frame too large ends the stream: every later call says so again,
// and none returns part of the line as a frame.
func TestDecoderTooLargeIsFinal(t *testing.T) {
	t.Parallel()
	head, tail := `{"t":"req","a":"`, `"}`
	for _, n := range []int{MaxFrame, MaxFrame + 1, MaxFrame + 100<<10} {
		in := head + strings.Repeat("x", n-len(head)-len(tail)) + tail + "\n" + `{"t":"req"}` + "\n"
		dec := NewDecoder(strings.NewReader(in))
		for i := range 3 {
			if f, err := dec.Decode(); f != nil || !errors.Is(err, ErrFrameTooLarge) {
				t.Errorf("Decode %d after a %d-byte line = (a frame: %v), %v, want ErrFrameTooLarge", i, n, f != nil, err)
			}
		}
	}
}

// FuzzDecoder holds Decoder to the Scanner it replaced on any stream,
// read whole or in pieces of any size.
func FuzzDecoder(f *testing.F) {
	f.Add([]byte(`{"t":"req","id":1,"op":"show","a":{"id":"sf-abcd1234"}}`+"\n"), uint8(0))
	f.Add([]byte("{\"t\":\"hello\"}\r\n\n{\"t\":\"res\",\"ok\":{}}"), uint8(3))
	f.Add([]byte("nope\n{\"id\":1}\n\r\n{\"t\":\"evt\"}\n"), uint8(1))
	f.Add([]byte(`{"t":"res","err":{"code":"conflict"}}`+"\n"+`{"t":`), uint8(5))
	f.Fuzz(func(t *testing.T, in []byte, n uint8) {
		wrap := func(r io.Reader) io.Reader { return r }
		if n > 0 {
			wrap = func(r io.Reader) io.Reader { return &pieces{r: r, n: int(n)} }
		}
		sameAsScanner(t, in, wrap)
	})
}

// A Decoder keeps 4 KiB to read with, and gives back what it grew to
// read a larger frame once that frame is decoded: a server holds one for
// each connection, and aries mcp one for as long as it runs
// (docs/design/profile-ariesd.md, proposal 1; profile-aries.md,
// proposal 5).
func TestDecoderRetains(t *testing.T) {
	// Serial: it measures the heap, which a test running beside it would move.
	const decoders = 16
	const most = 8 << 10 // a decoder's own bytes, at most
	small := []byte(`{"t":"req","id":1,"op":"show","a":{"id":"sf-abcd1234"}}` + "\n")
	large := []byte(`{"t":"res","id":2,"ok":"` + strings.Repeat("x", 256<<10) + `"}` + "\n")

	retained := func(read [][]byte) uint64 {
		t.Helper()
		stream := bytes.Join(read, nil) // shared, and so not counted
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		decs := make([]*Decoder, decoders)
		for i := range decs {
			decs[i] = NewDecoder(bytes.NewReader(stream))
			for range read {
				if _, err := decs[i].Decode(); err != nil {
					t.Fatalf("Decode: %v", err)
				}
			}
		}
		runtime.GC()
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(decs)
		runtime.KeepAlive(stream)
		if after.HeapAlloc < before.HeapAlloc {
			return 0
		}
		return (after.HeapAlloc - before.HeapAlloc) / decoders
	}
	if got := retained([][]byte{small}); got > most {
		t.Errorf("a Decoder that read a %d-byte frame holds %d bytes, want at most %d", len(small), got, most)
	}
	if got := retained([][]byte{small, large, small}); got > most {
		t.Errorf("a Decoder that read a %d-byte frame holds %d bytes, want at most %d: it keeps the buffer it grew", len(large), got, most)
	}
}
