package proto

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Frame types.
//
// FrameBridge is a connection's first frame. It comes from another
// ariesd process on the server, never from a client: the bridge
// (`ariesd stdio`), ahead of a client's hello. It has no version. The
// binary is swapped while old bridges run, so either side may be the
// older: fields are only ever added to it, a reader ignores a field it
// does not know, and a missing one means what it meant before it existed
// (a bridge frame without k is a key line of the authorized_keys file).
const (
	FrameBridge  = "bridge"
	FrameHello   = "hello"
	FrameWelcome = "welcome"
	FrameReq     = "req"
	FrameRes     = "res"
	FrameEvent   = "evt"
)

// MaxFrame bounds one encoded frame, newline included.
const MaxFrame = 4 << 20

// ErrFrameTooLarge means a frame exceeded MaxFrame.
var ErrFrameTooLarge = errors.New("frame larger than 4 MiB")

// Frame is one line on the wire. Which fields are set depends on T; see the
// package documentation for the key names.
type Frame struct {
	T  string `json:"t"`
	ID uint64 `json:"id,omitempty"`

	Op   string          `json:"op,omitempty"`
	Args json.RawMessage `json:"a,omitempty"`
	OK   json.RawMessage `json:"ok,omitempty"`
	Err  *Error          `json:"err,omitempty"`

	Version string `json:"v,omitempty"`
	Proto   int    `json:"p,omitempty"`
	Min     int    `json:"min,omitempty"`
	Max     int    `json:"max,omitempty"`
	Project string `json:"proj,omitempty"`
	Session string `json:"s,omitempty"`
	Machine string `json:"m,omitempty"`
	Latest  string `json:"latest,omitempty"`
	// Harness names the agent harness a hello comes from (an `aries setup`
	// agent name), or is empty. Servers that predate it ignore it.
	Harness string `json:"h,omitempty"`

	Principal string `json:"pr,omitempty"`
	// Key is, on a bridge frame, the SHA-256 fingerprint ("SHA256:…") of
	// the key that authenticated, when its line came from the control
	// store: the daemon writes it into the forced command of each line of
	// the keys file sshd reads.
	Key string `json:"k,omitempty"`

	// E is an evt frame's payload, such as the inbox item it pushes.
	E json.RawMessage `json:"e,omitempty"`

	// Traceparent and Tracestate are, on a req frame, the caller's W3C
	// trace context: the traceparent value and, optionally, the
	// tracestate. They are per request, since one connection of `aries mcp`
	// serves many agent turns. The server reads them with
	// ParseTraceContext, which ignores what is malformed: a bad trace
	// context never fails a request.
	Traceparent string `json:"tp,omitempty"`
	Tracestate  string `json:"ts,omitempty"`
}

// Encoder writes frames. It is safe for concurrent use.
type Encoder struct {
	mu sync.Mutex // held for each write, so frames never interleave
	w  io.Writer
}

// NewEncoder returns an Encoder writing to w.
func NewEncoder(w io.Writer) *Encoder { return &Encoder{w: w} }

// Encode writes f as one line, in a single Write. A frame larger than
// MaxFrame is not written, and the error wraps [ErrFrameTooLarge].
func (e *Encoder) Encode(f *Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("encode %s frame: %w", f.T, err)
	}
	if len(b)+1 > MaxFrame {
		return fmt.Errorf("encode %s frame: %w", f.T, ErrFrameTooLarge)
	}
	b = append(b, '\n')
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.w.Write(b); err != nil {
		return fmt.Errorf("write %s frame: %w", f.T, err)
	}
	return nil
}

// Decoder reads frames. It is not safe for concurrent use.
//
// It keeps a small buffer, and gathers a frame too long for it in one of
// its own that it lets go of once the frame is decoded: a server holds a
// Decoder for each connection and aries mcp one for hours, and a buffer
// kept at the size of the largest frame seen would be held as long
// (docs/design/profile-ariesd.md, proposal 1).
type Decoder struct {
	r   *bufio.Reader
	err error // the error that ended the stream; every later call returns it
}

// readSize is a Decoder's buffer: larger than nearly every frame.
const readSize = 4 << 10

// NewDecoder returns a Decoder reading from r.
func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{r: bufio.NewReaderSize(r, readSize)}
}

// line returns the next line without its newline and one carriage return
// before it, and a last line the stream ends without a newline, as
// bufio.Scanner with ScanLines and a MaxFrame buffer did: a line must fit
// in MaxFrame bytes with its newline, or, at the end of the stream, with
// room for one. Any error but a line that is not a frame is final, and
// every later call returns it.
func (d *Decoder) line() ([]byte, error) {
	if d.err != nil {
		return nil, d.err
	}
	b, err := d.r.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		b, err = d.gather(b)
	}
	if len(b) > MaxFrame || len(b) == MaxFrame && err != nil {
		d.err = ErrFrameTooLarge
		return nil, d.err
	}
	switch err {
	case nil:
		b = b[:len(b)-1]
	case io.EOF: // as bufio.Scanner had it: only io.EOF itself, unwrapped, is a clean end
		d.err = io.EOF
	default:
		d.err = fmt.Errorf("read frame: %w", err)
	}
	if err != nil && len(b) == 0 {
		return nil, d.err
	}
	return bytes.TrimSuffix(b, []byte{'\r'}), nil
}

// gather reads the rest of a line that began with b and is longer than
// the Decoder's buffer, into a buffer of its own that doubles as it
// fills, to MaxFrame at most, so that a large frame costs a few copies
// rather than one for each 4 KiB. It stops at MaxFrame bytes, newline or
// none, and returns ReadSlice's last error: bufio.ErrBufferFull if it
// stopped there.
func (d *Decoder) gather(b []byte) ([]byte, error) {
	long := append(make([]byte, 0, 2*len(b)), b...)
	for {
		b, err := d.r.ReadSlice('\n')
		if len(long)+len(b) > cap(long) {
			// Doubled exactly: append's own growth rounds up past it.
			grown := make([]byte, len(long), max(len(long)+len(b), min(2*cap(long), MaxFrame)))
			copy(grown, long)
			long = grown
		}
		long = append(long, b...)
		if !errors.Is(err, bufio.ErrBufferFull) || len(long) >= MaxFrame {
			return long, err
		}
	}
}

// Decode reads the next frame. It returns [io.EOF] at a clean end of
// stream, and [ErrFrameTooLarge] for a line longer than MaxFrame. A line
// that is not JSON, or has no frame type, is an error, and the next call
// reads the line after it; any other error is final.
func (d *Decoder) Decode() (*Frame, error) {
	b, err := d.line()
	if err != nil {
		return nil, err
	}
	var f Frame
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("decode frame: %w", err)
	}
	if f.T == "" {
		return nil, errors.New("decode frame: no frame type")
	}
	return &f, nil
}

// Request builds a req frame with args encoded.
func Request(id uint64, op string, args any) (*Frame, error) {
	a, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("encode %s args: %w", op, err)
	}
	return &Frame{T: FrameReq, ID: id, Op: op, Args: a}, nil
}

// Response builds a res frame: an error when e is non-nil, else the result.
func Response(id uint64, result any, e *Error) (*Frame, error) {
	if e != nil {
		return &Frame{T: FrameRes, ID: id, Err: e}, nil
	}
	ok, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode result: %w", err)
	}
	return &Frame{T: FrameRes, ID: id, OK: ok}, nil
}
