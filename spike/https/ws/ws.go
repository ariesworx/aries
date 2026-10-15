// Package ws is the part of WebSocket (RFC 6455) that carries Aries's
// NDJSON frames over HTTPS, on the standard library alone: the opening
// handshake on either side, messages of text or binary, fragments,
// ping and pong, and close. It has no extensions (no compression), no
// subprotocols and no HTTP/2 (RFC 8441): one connection is one
// HTTP/1.1 upgrade. It is the spike's answer to the dependency question
// of docs/design/transport.md: what a library would bring is this file.
package ws

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
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
	"time"
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

// DefaultMax bounds a message, as proto.MaxFrame bounds a frame.
const DefaultMax = 4 << 20

const guid = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// ErrTooLarge means a message exceeded the connection's bound.
var ErrTooLarge = errors.New("websocket message too large")

// Conn is one WebSocket connection. Read is for one goroutine; Write may
// be called from several.
type Conn struct {
	nc     net.Conn
	r      *bufio.Reader
	w      io.Writer
	server bool // a server reads masked frames and writes unmasked ones
	max    int

	wmu    sync.Mutex
	closed bool
}

func accept(key string) string {
	h := sha1.Sum([]byte(key + guid))
	return base64.StdEncoding.EncodeToString(h[:])
}

// Accept completes the opening handshake of r, an HTTP/1.1 GET with
// Upgrade: websocket, and takes over its connection. On a request that
// is not one, it answers 400 and returns an error.
func Accept(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if r.Method != http.MethodGet || !headerHas(r.Header, "Connection", "upgrade") ||
		!strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		r.Header.Get("Sec-WebSocket-Version") != "13" || key == "" {
		http.Error(w, "a WebSocket upgrade is required here", http.StatusBadRequest)
		return nil, errors.New("not a websocket upgrade")
	}
	nc, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "cannot upgrade this connection", http.StatusInternalServerError)
		return nil, fmt.Errorf("hijack: %w", err)
	}
	_ = nc.SetDeadline(time.Time{})
	res := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept(key) + "\r\n\r\n"
	if _, err := brw.WriteString(res); err != nil {
		nc.Close()
		return nil, err
	}
	if err := brw.Flush(); err != nil {
		nc.Close()
		return nil, err
	}
	return &Conn{nc: nc, r: brw.Reader, w: nc, server: true, max: DefaultMax}, nil
}

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

// Dial opens a WebSocket to a wss:// (or ws://) URL, sending header with
// the upgrade (an Authorization and a DPoP proof, in the spike). It
// returns the server's response to the upgrade, which on a refusal
// carries the status and body for the caller to report.
func Dial(ctx context.Context, rawURL string, cfg *tls.Config, header http.Header) (*Conn, *http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, err
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), map[string]string{"wss": "443", "ws": "80"}[u.Scheme])
	}
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, nil, err
	}
	switch u.Scheme {
	case "wss":
		c := cfg.Clone()
		if c.ServerName == "" {
			c.ServerName = u.Hostname()
		}
		c.NextProtos = []string{"http/1.1"} // an upgrade is HTTP/1.1's
		tc := tls.Client(nc, c)
		if err := tc.HandshakeContext(ctx); err != nil {
			nc.Close()
			return nil, nil, fmt.Errorf("tls: %w", err)
		}
		nc = tc
	case "ws":
	default:
		nc.Close()
		return nil, nil, fmt.Errorf("scheme %q is not ws or wss", u.Scheme)
	}
	stop := context.AfterFunc(ctx, func() { nc.Close() })
	defer stop()
	var nonce [16]byte
	_, _ = rand.Read(nonce[:])
	key := base64.StdEncoding.EncodeToString(nonce[:])
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, (&url.URL{Scheme: map[string]string{"wss": "https", "ws": "http"}[u.Scheme], Host: u.Host, Path: u.Path, RawQuery: u.RawQuery}).String(), nil)
	if err != nil {
		nc.Close()
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
		nc.Close()
		return nil, nil, err
	}
	br := bufio.NewReader(nc)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		nc.Close()
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		resp.Body = io.NopCloser(strings.NewReader(string(body)))
		nc.Close()
		return nil, resp, fmt.Errorf("upgrade refused: %s", resp.Status)
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != accept(key) {
		nc.Close()
		return nil, resp, errors.New("upgrade answered with the wrong Sec-WebSocket-Accept")
	}
	return &Conn{nc: nc, r: br, w: nc, max: DefaultMax}, resp, nil
}

// Read returns the next text or binary message, answering pings and
// skipping pongs on the way. It returns io.EOF when the peer closed.
func (c *Conn) Read() ([]byte, error) {
	var msg []byte
	started := false
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case opPing:
			if err := c.writeFrame(opPong, payload); err != nil {
				return nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			_ = c.writeFrame(opClose, payload) // echo, then end
			return nil, io.EOF
		case opText, opBinary:
			if started {
				return nil, errors.New("a new message began inside a fragmented one")
			}
			started = true
		case opContinue:
			if !started {
				return nil, errors.New("a continuation frame with no message")
			}
		}
		if c.max > 0 && len(msg)+len(payload) > c.max {
			return nil, ErrTooLarge
		}
		msg = append(msg, payload...)
		if fin {
			if msg == nil {
				msg = []byte{}
			}
			return msg, nil
		}
	}
}

func (c *Conn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var h [2]byte
	if _, err := io.ReadFull(c.r, h[:]); err != nil {
		return false, 0, nil, err // io.EOF here is a peer gone between frames
	}
	fin, op = h[0]&0x80 != 0, h[0]&0x0F
	if h[0]&0x70 != 0 {
		return false, 0, nil, errors.New("reserved bits set")
	}
	switch op {
	case opContinue, opText, opBinary:
	case opClose, opPing, opPong:
		if !fin {
			return false, 0, nil, errors.New("a fragmented control frame")
		}
	default:
		return false, 0, nil, fmt.Errorf("unknown opcode %#x", op)
	}
	masked := h[1]&0x80 != 0
	if masked != c.server {
		return false, 0, nil, errors.New("frame masking is wrong for this side")
	}
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(c.r, b[:]); err != nil {
			return false, 0, nil, unexpected(err)
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(c.r, b[:]); err != nil {
			return false, 0, nil, unexpected(err)
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if op >= opClose && n > 125 {
		return false, 0, nil, errors.New("a control frame over 125 bytes")
	}
	if c.max > 0 && n > uint64(c.max) {
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

// unexpected turns an end of stream inside a frame into an error that
// is not io.EOF, which Read keeps for a clean close.
func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// Write sends one text message.
func (c *Conn) Write(msg []byte) error { return c.writeFrame(opText, msg) }

// Ping sends a ping, which a peer answers with a pong that Read skips.
// A server pings an idle connection so that proxies keep it open.
func (c *Conn) Ping() error { return c.writeFrame(opPing, nil) }

func (c *Conn) writeFrame(op byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	n := len(payload)
	hdr := make([]byte, 0, 14)
	hdr = append(hdr, 0x80|op)
	m := byte(0)
	if !c.server {
		m = 0x80
	}
	switch {
	case n < 126:
		hdr = append(hdr, m|byte(n))
	case n < 1<<16:
		hdr = append(hdr, m|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, m|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	buf := make([]byte, 0, len(hdr)+4+n)
	buf = append(buf, hdr...)
	if c.server {
		buf = append(buf, payload...)
	} else {
		var key [4]byte
		_, _ = rand.Read(key[:])
		buf = append(buf, key[:]...)
		for i, b := range payload {
			buf = append(buf, b^key[i%4])
		}
	}
	_, err := c.w.Write(buf)
	if op == opClose {
		c.closed = true
	}
	return err
}

// Close sends a close frame, if none was sent, and closes the connection.
func (c *Conn) Close() error {
	_ = c.writeFrame(opClose, []byte{0x03, 0xE8}) // 1000, normal closure
	if c.nc == nil {
		return nil
	}
	return c.nc.Close()
}

// SetReadDeadline is the underlying connection's.
func (c *Conn) SetReadDeadline(t time.Time) error { return c.nc.SetReadDeadline(t) }
