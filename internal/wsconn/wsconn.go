// Package wsconn is the part of WebSocket (RFC 6455) that carries
// Aries's NDJSON frames over HTTPS, on the standard library alone
// (design decision 87, transport.md §5.3): the opening handshake on
// either side, text and binary messages, fragments, ping and pong, and
// the close. It has no extensions (no compression), no subprotocols and
// no HTTP/2 (RFC 8441): one connection is one HTTP/1.1 upgrade.
//
// It reads what a peer on the internet sends, so it trusts none of it:
// a frame's length is checked against the bound before anything is
// allocated, masking is enforced for each side, control frames are
// short and whole, a text message must be UTF-8, a length must be
// written in its shortest form, and a message's fragments may not be
// interleaved with more than a few control frames. Any of these fails
// the connection with a close frame that says why, and every later Read
// fails the same way. FuzzRead holds it to that.
package wsconn

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6455's accept key is SHA-1 by definition; it is no security check
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Opcodes (RFC 6455 §5.2).
const (
	opContinue = 0x0
	opText     = 0x1
	opBinary   = 0x2
	opClose    = 0x8
	opPing     = 0x9
	opPong     = 0xA
)

// Close codes (RFC 6455 §7.4.1) the package sends.
const (
	closeNormal   = 1000
	closeProtocol = 1002
	closeInvalid  = 1007
	closeTooBig   = 1009
)

// DefaultMax bounds a message when the caller gives no bound: 4 MiB,
// proto.MaxFrame, one frame of Aries's protocol with its newline.
const DefaultMax = 4 << 20

// maxControl is how many control frames may come between the fragments
// of one message. A peer that answers a ping sends one; more than this
// is a peer that keeps a message open to make the reader work.
const maxControl = 64

// closeWait bounds the close frame Close writes, so that a peer that
// reads nothing cannot hold a Close.
const closeWait = time.Second

// guid is RFC 6455's, appended to the client's key for the accept key.
const guid = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

var (
	// ErrTooLarge is a message past the connection's bound.
	ErrTooLarge = errors.New("websocket message too large")
	// ErrProtocol is a peer that broke RFC 6455: the error that wraps it
	// says how.
	ErrProtocol = errors.New("websocket protocol error")
)

// protocolError is ErrProtocol with what broke it.
func protocolError(why string) error { return fmt.Errorf("%w: %s", ErrProtocol, why) }

// Conn is one WebSocket connection. Read is for one goroutine; Write,
// Ping and Close may be called from any, and at once.
type Conn struct {
	nc     net.Conn // nil only in tests that read and write buffers
	r      *bufio.Reader
	w      io.Writer
	server bool // a server reads masked frames and writes unmasked ones
	max    int
	// maskKey makes the key of each frame a client writes.
	maskKey func() [4]byte

	// readErr is why Read failed; every later Read returns it.
	readErr error

	wmu    sync.Mutex // serializes frames on the wire
	closed atomic.Bool
	once   sync.Once // closes nc once
}

// newConn is a Conn over r and w, the stream of nc; max zero takes
// DefaultMax.
func newConn(nc net.Conn, r *bufio.Reader, w io.Writer, server bool, max int) *Conn {
	if max <= 0 {
		max = DefaultMax
	}
	return &Conn{nc: nc, r: r, w: w, server: server, max: max, maskKey: randomKey}
}

// randomKey is a client's masking key: random, as RFC 6455 §5.3 asks,
// so that a proxy cannot be made to see a chosen byte string.
func randomKey() [4]byte {
	var k [4]byte
	_, _ = rand.Read(k[:]) // crypto/rand.Read never fails
	return k
}

// acceptKey is the Sec-WebSocket-Accept a server answers key with.
func acceptKey(key string) string {
	h := sha1.Sum([]byte(key + guid)) //nolint:gosec // see the import
	return base64.StdEncoding.EncodeToString(h[:])
}

// Accept completes the opening handshake of r, an HTTP/1.1 GET with
// Upgrade: websocket, version 13 and a key of 16 bytes, and takes over
// its connection, with no deadline. A request that is not one is
// answered 400 and refused with an error. max bounds a message; zero
// takes DefaultMax.
func Accept(w http.ResponseWriter, r *http.Request, max int) (*Conn, error) {
	key := r.Header.Get("Sec-WebSocket-Key")
	raw, kerr := base64.StdEncoding.DecodeString(key)
	if r.ProtoMajor != 1 || !r.ProtoAtLeast(1, 1) || r.Method != http.MethodGet ||
		!headerHas(r.Header, "Connection", "upgrade") || !headerHas(r.Header, "Upgrade", "websocket") ||
		r.Header.Get("Sec-WebSocket-Version") != "13" || kerr != nil || len(raw) != 16 {
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "a WebSocket upgrade, version 13, over HTTP/1.1 is required here", http.StatusBadRequest)
		return nil, errors.New("not a websocket upgrade")
	}
	nc, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "cannot upgrade this connection", http.StatusInternalServerError)
		return nil, fmt.Errorf("hijack: %w", err)
	}
	_ = nc.SetDeadline(time.Time{})
	res := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + acceptKey(key) + "\r\n\r\n"
	if _, err := brw.WriteString(res); err != nil {
		_ = nc.Close()
		return nil, fmt.Errorf("upgrade: %w", err)
	}
	if err := brw.Flush(); err != nil {
		_ = nc.Close()
		return nil, fmt.Errorf("upgrade: %w", err)
	}
	return newConn(nc, brw.Reader, nc, true, max), nil
}

// headerHas reports whether the comma-separated header name holds token,
// in any case.
func headerHas(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for t := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// maxRefusal bounds what Dial keeps of the body of a refused upgrade.
const maxRefusal = 16 << 10

// Dial opens a WebSocket to a wss:// URL (or ws://, which tests and a
// server behind no TLS use), sending header with the upgrade, and
// returns the server's answer to the upgrade too. A refused upgrade is
// an error, and the answer, with up to 16 KiB of its body, is returned
// for the caller to report. cfg is the TLS configuration for wss; nil
// takes the system's roots. max bounds a message; zero takes
// DefaultMax. ctx bounds the dial and the upgrade, not the connection.
func Dial(ctx context.Context, rawURL string, cfg *tls.Config, header http.Header, max int) (*Conn, *http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, err
	}
	var scheme, port string
	switch u.Scheme {
	case "wss":
		scheme, port = "https", "443"
	case "ws":
		scheme, port = "http", "80"
	default:
		return nil, nil, fmt.Errorf("scheme %q is not ws or wss", u.Scheme)
	}
	addr := u.Host
	if u.Port() == "" {
		addr = net.JoinHostPort(u.Hostname(), port)
	}
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	if scheme == "https" {
		c := &tls.Config{MinVersion: tls.VersionTLS12}
		if cfg != nil {
			c = cfg.Clone()
		}
		if c.ServerName == "" {
			c.ServerName = u.Hostname()
		}
		c.NextProtos = []string{"http/1.1"} // an upgrade is HTTP/1.1's
		tc := tls.Client(nc, c)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = nc.Close()
			return nil, nil, &TLSError{Err: err}
		}
		nc = tc
	}
	stop := context.AfterFunc(ctx, func() { _ = nc.Close() })
	defer stop()
	var raw [16]byte
	_, _ = rand.Read(raw[:]) // crypto/rand.Read never fails
	key := base64.StdEncoding.EncodeToString(raw[:])
	target := &url.URL{Scheme: scheme, Host: u.Host, Path: u.Path, RawPath: u.RawPath, RawQuery: u.RawQuery}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		_ = nc.Close()
		return nil, nil, err
	}
	for k, vs := range header {
		req.Header[k] = vs
	}
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", key)
	req.Header.Set("Sec-WebSocket-Version", "13")
	if err := req.Write(nc); err != nil {
		_ = nc.Close()
		return nil, nil, fmt.Errorf("upgrade: %w", err)
	}
	br := bufio.NewReader(nc)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = nc.Close()
		return nil, nil, fmt.Errorf("upgrade: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxRefusal))
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(strings.NewReader(string(body)))
		_ = nc.Close()
		return nil, resp, fmt.Errorf("upgrade refused: %s", resp.Status)
	}
	var why string
	switch {
	case !headerHas(resp.Header, "Upgrade", "websocket"):
		why = "without Upgrade: websocket"
	case !headerHas(resp.Header, "Connection", "upgrade"):
		why = "without Connection: Upgrade"
	case resp.Header.Get("Sec-WebSocket-Accept") != acceptKey(key):
		why = "with the wrong Sec-WebSocket-Accept"
	case resp.Header.Get("Sec-WebSocket-Extensions") != "":
		why = "with an extension that was not asked for"
	case resp.Header.Get("Sec-WebSocket-Protocol") != "":
		why = "with a subprotocol that was not asked for"
	}
	if why != "" {
		_ = nc.Close()
		return nil, resp, fmt.Errorf("upgrade answered %s", why)
	}
	return newConn(nc, br, nc, false, max), resp, nil
}

// TLSError is a TLS handshake that failed in Dial: a certificate that
// does not verify among the causes, which a caller tells apart from a
// network that failed.
type TLSError struct{ Err error }

func (e *TLSError) Error() string { return "tls: " + e.Err.Error() }

func (e *TLSError) Unwrap() error { return e.Err }

// Read returns the next text or binary message, answering pings and
// skipping pongs on the way. It returns io.EOF when the peer closed the
// connection, with a close frame or between messages, and
// io.ErrUnexpectedEOF when the stream ended inside a frame. A peer that
// breaks the protocol is sent a close frame that says why. Once Read has
// failed it fails again with the same error.
func (c *Conn) Read() ([]byte, error) {
	if c.readErr != nil {
		return nil, c.readErr
	}
	msg, err := c.read()
	if err != nil {
		c.readErr = err
		code := 0
		switch {
		case errors.Is(err, ErrTooLarge):
			code = closeTooBig
		case errors.Is(err, errInvalidText):
			code = closeInvalid
		case errors.Is(err, ErrProtocol):
			code = closeProtocol
		}
		if code != 0 {
			_ = c.writeFrame(opClose, closePayload(code))
			c.closed.Store(true)
		}
		return nil, err
	}
	return msg, nil
}

// errInvalidText is a text message, or a close's reason, that is not
// UTF-8.
var errInvalidText = protocolError("text that is not UTF-8")

// read is Read before its error is kept.
func (c *Conn) read() ([]byte, error) {
	var msg []byte
	started, text, control := false, false, 0
	for {
		fin, op, payload, err := c.readFrame(started)
		if err != nil {
			return nil, err
		}
		switch op {
		case opPing, opPong, opClose:
			if started {
				if control++; control > maxControl {
					return nil, protocolError("too many control frames inside a message")
				}
			}
			switch op {
			case opPing:
				if err := c.writeFrame(opPong, payload); err != nil {
					return nil, err
				}
			case opClose:
				code, err := closeCode(payload)
				if err != nil {
					return nil, err
				}
				var echo []byte
				if code != 0 {
					echo = closePayload(code)
				}
				_ = c.writeFrame(opClose, echo)
				c.closed.Store(true)
				return nil, io.EOF
			}
			continue
		case opText, opBinary:
			if started {
				return nil, protocolError("a new message began inside a fragmented one")
			}
			started, text = true, op == opText
		case opContinue:
			if !started {
				return nil, protocolError("a continuation frame with no message")
			}
		}
		if len(msg)+len(payload) > c.max {
			return nil, ErrTooLarge
		}
		msg = append(msg, payload...)
		if fin {
			if text && !utf8.Valid(msg) {
				return nil, errInvalidText
			}
			if msg == nil {
				msg = []byte{}
			}
			return msg, nil
		}
	}
}

// closeCode reads a close frame's payload: its status code, or zero for
// none, and refuses a payload of one byte, a code no endpoint may send
// (RFC 6455 §7.4), or a reason that is not UTF-8.
func closeCode(p []byte) (int, error) {
	switch len(p) {
	case 0:
		return 0, nil
	case 1:
		return 0, protocolError("a close frame of one byte")
	}
	code := int(binary.BigEndian.Uint16(p))
	switch {
	case code < 1000, code >= 1004 && code <= 1006, code >= 1015 && code < 3000, code >= 5000:
		return 0, protocolError(fmt.Sprintf("close code %d", code))
	case !utf8.Valid(p[2:]):
		return 0, errInvalidText
	}
	return code, nil
}

// closePayload is a close frame's payload with code and no reason.
func closePayload(code int) []byte { return binary.BigEndian.AppendUint16(nil, uint16(code)) } //nolint:gosec // close codes are below 5000

// readFrame reads one frame and unmasks its payload. inMessage says a
// fragmented message is open.
func (c *Conn) readFrame(inMessage bool) (fin bool, op byte, payload []byte, err error) {
	var h [2]byte
	if _, err := io.ReadFull(c.r, h[:]); err != nil {
		if errors.Is(err, io.EOF) && !inMessage {
			return false, 0, nil, io.EOF // the peer is gone between messages
		}
		return false, 0, nil, unexpected(err)
	}
	fin, op = h[0]&0x80 != 0, h[0]&0x0F
	if h[0]&0x70 != 0 {
		return false, 0, nil, protocolError("reserved bits set")
	}
	switch op {
	case opContinue, opText, opBinary:
	case opClose, opPing, opPong:
		if !fin {
			return false, 0, nil, protocolError("a fragmented control frame")
		}
	default:
		return false, 0, nil, protocolError(fmt.Sprintf("unknown opcode %#x", op))
	}
	masked := h[1]&0x80 != 0
	if masked != c.server {
		return false, 0, nil, protocolError("frame masking is wrong for this side")
	}
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(c.r, b[:]); err != nil {
			return false, 0, nil, unexpected(err)
		}
		if n = uint64(binary.BigEndian.Uint16(b[:])); n < 126 {
			return false, 0, nil, protocolError("a length not in its shortest form")
		}
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(c.r, b[:]); err != nil {
			return false, 0, nil, unexpected(err)
		}
		if n = binary.BigEndian.Uint64(b[:]); n>>63 != 0 || n < 1<<16 {
			return false, 0, nil, protocolError("a 64-bit length past 2^63 or not in its shortest form")
		}
	}
	if op >= opClose && n > 125 {
		return false, 0, nil, protocolError("a control frame over 125 bytes")
	}
	if n > uint64(c.max) { //nolint:gosec // max is positive
		return false, 0, nil, ErrTooLarge
	}
	var key [4]byte
	if masked {
		if _, err := io.ReadFull(c.r, key[:]); err != nil {
			return false, 0, nil, unexpected(err)
		}
	}
	payload = make([]byte, n)
	if _, err := io.ReadFull(c.r, payload); err != nil {
		return false, 0, nil, unexpected(err)
	}
	if masked {
		for i := range payload {
			payload[i] ^= key[i%4]
		}
	}
	return fin, op, payload, nil
}

// unexpected makes an end of stream inside a frame io.ErrUnexpectedEOF,
// so that io.EOF stays the peer's leaving between messages.
func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// Write sends msg as one text message, in one frame.
func (c *Conn) Write(msg []byte) error { return c.writeFrame(opText, msg) }

// Ping sends a ping, which the peer answers with a pong that Read
// skips. A server pings a connection with nothing to say, so that the
// proxies on its way keep it open.
func (c *Conn) Ping() error { return c.writeFrame(opPing, nil) }

// writeFrame writes one frame, whole. After a close frame, nothing more
// is written.
func (c *Conn) writeFrame(op byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.writeLocked(op, payload)
}

// writeLocked is writeFrame with c.wmu held.
func (c *Conn) writeLocked(op byte, payload []byte) error {
	if c.closed.Load() {
		return net.ErrClosed
	}
	n := len(payload)
	buf := make([]byte, 0, 14+n)
	buf = append(buf, 0x80|op)
	m := byte(0)
	if !c.server {
		m = 0x80
	}
	switch {
	case n < 126:
		buf = append(buf, m|byte(n))
	case n < 1<<16:
		buf = append(buf, m|126)
		buf = binary.BigEndian.AppendUint16(buf, uint16(n))
	default:
		buf = append(buf, m|127)
		buf = binary.BigEndian.AppendUint64(buf, uint64(n))
	}
	if c.server {
		buf = append(buf, payload...)
	} else {
		key := c.maskKey()
		buf = append(buf, key[:]...)
		for i, b := range payload {
			buf = append(buf, b^key[i%4])
		}
	}
	if op == opClose {
		c.closed.Store(true)
	}
	if _, err := c.w.Write(buf); err != nil {
		return fmt.Errorf("websocket write: %w", err)
	}
	return nil
}

// Close sends a close frame, unless one was sent or a write is in
// progress, and closes the connection. It does not wait on a write
// stuck behind a peer that reads nothing: that write then fails. It may
// be called more than once.
func (c *Conn) Close() error {
	if c.wmu.TryLock() {
		if !c.closed.Load() {
			if c.nc != nil {
				_ = c.nc.SetWriteDeadline(time.Now().Add(closeWait))
			}
			_ = c.writeLocked(opClose, closePayload(closeNormal))
		}
		c.wmu.Unlock()
	}
	c.closed.Store(true)
	var err error
	c.once.Do(func() {
		if c.nc != nil {
			err = c.nc.Close()
		}
	})
	return err
}

// SetReadDeadline, SetWriteDeadline and SetDeadline are the underlying
// connection's. A read that times out inside a frame leaves the stream
// out of step, so Read fails from then on.
func (c *Conn) SetReadDeadline(t time.Time) error  { return c.nc.SetReadDeadline(t) }
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.nc.SetWriteDeadline(t) }
func (c *Conn) SetDeadline(t time.Time) error      { return c.nc.SetDeadline(t) }

// LocalAddr and RemoteAddr are the underlying connection's.
func (c *Conn) LocalAddr() net.Addr  { return c.nc.LocalAddr() }
func (c *Conn) RemoteAddr() net.Addr { return c.nc.RemoteAddr() }

// Stream is c as a byte stream: what is written is sent as one message
// for each Write, and what is read is the messages' bytes, one after
// another. Aries's frames are lines, and its encoder writes each in one
// Write, so each message carries one frame; a reader does not rely on
// it. Its deadlines and Close are c's.
func Stream(c *Conn) net.Conn { return &stream{Conn: c} }

// stream is Stream's net.Conn.
type stream struct {
	*Conn
	rest []byte // what is left of the message being read
}

func (s *stream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(s.rest) == 0 {
		m, err := s.Conn.Read()
		if err != nil {
			return 0, err
		}
		s.rest = m
	}
	n := copy(p, s.rest)
	s.rest = s.rest[n:]
	return n, nil
}

func (s *stream) Write(p []byte) (int, error) {
	if err := s.Conn.Write(p); err != nil {
		return 0, err
	}
	return len(p), nil
}
