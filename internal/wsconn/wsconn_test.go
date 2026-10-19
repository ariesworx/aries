package wsconn

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

// echo serves WebSocket connections that write back every message they
// read, until the peer closes.
func echo(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(w, r, 0)
		if err != nil {
			return // Accept has answered with an error status
		}
		defer func() { _ = c.Close() }()
		for {
			m, err := c.Read()
			if err != nil {
				return
			}
			if err := c.Write(m); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// wss is srv's URL with the WebSocket scheme.
func wss(srv *httptest.Server) string { return "wss" + strings.TrimPrefix(srv.URL, "https") + "/" }

// tlsConfig trusts srv's certificate.
func tlsConfig(srv *httptest.Server) *tls.Config {
	return srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
}

func dial(t *testing.T, srv *httptest.Server) *Conn {
	t.Helper()
	c, _, err := Dial(t.Context(), wss(srv), tlsConfig(srv), nil, 0)
	if err != nil {
		t.Fatalf("Dial(%s) = %v", wss(srv), err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	c := dial(t, echo(t))
	for _, size := range []int{0, 1, 125, 126, 65535, 65536, 1 << 20} {
		want := bytes.Repeat([]byte{'x'}, size)
		if err := c.Write(want); err != nil {
			t.Fatalf("Write(%d bytes) = %v", size, err)
		}
		got, err := c.Read()
		if err != nil {
			t.Fatalf("Read after Write(%d bytes) = %v", size, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("echo of %d bytes = %d bytes, want the same", size, len(got))
		}
	}
}

func TestReadAfterCloseIsEOF(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := Accept(w, r, 0); err == nil {
			_ = c.Close()
		}
	}))
	t.Cleanup(srv.Close)
	c := dial(t, srv)
	if _, err := c.Read(); !errors.Is(err, io.EOF) {
		t.Errorf("Read after the server closed = %v, want io.EOF", err)
	}
}

func TestAcceptRefuses(t *testing.T) {
	t.Parallel()
	srv := echo(t)
	good := func() *http.Request {
		r, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
		r.Header.Set("Upgrade", "websocket")
		r.Header.Set("Connection", "keep-alive, Upgrade")
		r.Header.Set("Sec-WebSocket-Version", "13")
		r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		return r
	}
	tests := []struct {
		name string
		edit func(*http.Request)
	}{
		{name: "no upgrade", edit: func(r *http.Request) { r.Header.Del("Upgrade") }},
		{name: "POST", edit: func(r *http.Request) { r.Method = http.MethodPost }},
		{name: "version 8", edit: func(r *http.Request) { r.Header.Set("Sec-WebSocket-Version", "8") }},
		{name: "no key", edit: func(r *http.Request) { r.Header.Del("Sec-WebSocket-Key") }},
		{name: "a key of 8 bytes", edit: func(r *http.Request) { r.Header.Set("Sec-WebSocket-Key", "AAAAAAAAAAA=") }},
		{name: "a key that is not base64", edit: func(r *http.Request) { r.Header.Set("Sec-WebSocket-Key", "not base64 at all!!!!!!!") }},
		{name: "connection without upgrade", edit: func(r *http.Request) { r.Header.Set("Connection", "keep-alive") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := good()
			tc.edit(r)
			resp, err := srv.Client().Do(r)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("an upgrade with %s: status %d, want 400", tc.name, resp.StatusCode)
			}
		})
	}
}

// An upgrade is HTTP/1.1's: a request over HTTP/2 is refused, whatever
// it carries.
func TestAcceptRefusesHTTP2(t *testing.T) {
	t.Parallel()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := Accept(w, r, 0); err == nil {
			_ = c.Close()
		}
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	r, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	r.Header.Set("Sec-WebSocket-Version", "13")
	r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	resp, err := srv.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.ProtoMajor != 2 || resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an upgrade over %s: status %d, want HTTP/2 and 400", resp.Proto, resp.StatusCode)
	}
}

// frame builds one raw frame, as a peer would send it.
func frame(fin bool, op byte, mask bool, payload []byte) []byte {
	var b bytes.Buffer
	first := op
	if fin {
		first |= 0x80
	}
	b.WriteByte(first)
	m := byte(0)
	if mask {
		m = 0x80
	}
	switch n := len(payload); {
	case n < 126:
		b.WriteByte(m | byte(n))
	case n < 1<<16:
		b.WriteByte(m | 126)
		b.Write(binary.BigEndian.AppendUint16(nil, uint16(n)))
	default:
		b.WriteByte(m | 127)
		b.Write(binary.BigEndian.AppendUint64(nil, uint64(n)))
	}
	p := append([]byte(nil), payload...)
	if mask {
		key := []byte{1, 2, 3, 4}
		b.Write(key)
		for i := range p {
			p[i] ^= key[i%4]
		}
	}
	b.Write(p)
	return b.Bytes()
}

// frames joins raw frames into one stream.
func frames(fs ...[]byte) []byte { return bytes.Join(fs, nil) }

// reader is a Conn reading stream, as the server when server is set,
// writing what it answers to out.
func reader(stream []byte, server bool, maxBytes int, out io.Writer) *Conn {
	if out == nil {
		out = io.Discard
	}
	return newConn(nil, bufio.NewReader(bytes.NewReader(stream)), out, server, maxBytes)
}

func TestReadFrames(t *testing.T) {
	t.Parallel()
	pings := make([][]byte, 0, maxControl+2)
	pings = append(pings, frame(false, opText, false, []byte("a")))
	for range maxControl + 1 {
		pings = append(pings, frame(true, opPing, false, nil))
	}
	tests := []struct {
		name     string
		server   bool // reading as the server, which requires masked frames
		stream   []byte
		maxBytes int
		want     string
		wantErr  error // matched with errors.Is; nil with want set means success
	}{
		{name: "one text frame", stream: frame(true, opText, false, []byte("hi")), want: "hi"},
		{name: "one binary frame", stream: frame(true, opBinary, false, []byte{0xff}), want: "\xff"},
		{name: "fragments", stream: frames(frame(false, opText, false, []byte("a")), frame(true, opContinue, false, []byte("b"))), want: "ab"},
		{name: "ping between fragments", stream: frames(frame(false, opText, false, []byte("a")), frame(true, opPing, false, []byte("p")), frame(true, opContinue, false, []byte("b"))), want: "ab"},
		{name: "pong skipped", stream: frames(frame(true, opPong, false, nil), frame(true, opText, false, []byte("x"))), want: "x"},
		{name: "masked, read by the server", server: true, stream: frame(true, opText, true, []byte("hi")), want: "hi"},
		{name: "an empty message", stream: frame(true, opText, false, nil), want: ""},
		{name: "unmasked, read by the server", server: true, stream: frame(true, opText, false, []byte("hi")), wantErr: ErrProtocol},
		{name: "masked, read by the client", stream: frame(true, opText, true, []byte("hi")), wantErr: ErrProtocol},
		{name: "over the limit", stream: frame(true, opText, false, bytes.Repeat([]byte{'x'}, 200)), maxBytes: 100, wantErr: ErrTooLarge},
		{name: "fragments over the limit", stream: frames(frame(false, opText, false, bytes.Repeat([]byte{'x'}, 60)), frame(true, opContinue, false, bytes.Repeat([]byte{'x'}, 60))), maxBytes: 100, wantErr: ErrTooLarge},
		{name: "a length past 2^63", stream: []byte{0x81, 127, 0x80, 0, 0, 0, 0, 0, 0, 0}, wantErr: ErrProtocol},
		{name: "a 16-bit length that fits in 7", stream: []byte{0x81, 126, 0, 5, 'h', 'e', 'l', 'l', 'o'}, wantErr: ErrProtocol},
		{name: "a 64-bit length that fits in 16", stream: frames([]byte{0x81, 127, 0, 0, 0, 0, 0, 0, 0, 5}, []byte("hello")), wantErr: ErrProtocol},
		{name: "continuation first", stream: frame(true, opContinue, false, []byte("a")), wantErr: ErrProtocol},
		{name: "a message inside a message", stream: frames(frame(false, opText, false, []byte("a")), frame(true, opText, false, []byte("b"))), wantErr: ErrProtocol},
		{name: "fragmented ping", stream: frame(false, opPing, false, []byte("a")), wantErr: ErrProtocol},
		{name: "a ping over 125 bytes", stream: frame(true, opPing, false, bytes.Repeat([]byte{'p'}, 126)), wantErr: ErrProtocol},
		{name: "reserved bits", stream: []byte{0x80 | 0x40 | opText, 0}, wantErr: ErrProtocol},
		{name: "unknown opcode", stream: frame(true, 0x3, false, nil), wantErr: ErrProtocol},
		{name: "text that is not UTF-8", stream: frame(true, opText, false, []byte{0xff, 0xfe}), wantErr: ErrProtocol},
		{name: "text that is not UTF-8 across fragments", stream: frames(frame(false, opText, false, []byte{0xe2, 0x82}), frame(true, opContinue, false, []byte{'x'})), wantErr: ErrProtocol},
		{name: "a close with one byte", stream: frame(true, opClose, false, []byte{3}), wantErr: ErrProtocol},
		{name: "a close with code 1005", stream: frame(true, opClose, false, []byte{0x03, 0xed}), wantErr: ErrProtocol},
		{name: "a close with a reason not UTF-8", stream: frame(true, opClose, false, []byte{0x03, 0xe8, 0xff}), wantErr: ErrProtocol},
		{name: "a close", stream: frame(true, opClose, false, []byte{0x03, 0xe8}), wantErr: io.EOF},
		{name: "a close with no code", stream: frame(true, opClose, false, nil), wantErr: io.EOF},
		{name: "a flood of pings in one message", stream: frames(pings...), wantErr: ErrProtocol},
		{name: "truncated header", stream: frame(true, opText, false, []byte("hello"))[:1], wantErr: io.ErrUnexpectedEOF},
		{name: "truncated payload", stream: frame(true, opText, false, []byte("hello"))[:4], wantErr: io.ErrUnexpectedEOF},
		{name: "nothing: the peer gone between messages", stream: nil, wantErr: io.EOF},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := reader(tc.stream, tc.server, tc.maxBytes, nil).Read()
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("Read(%s) = %q, %v; want %v", tc.name, got, err, tc.wantErr)
				}
				return
			}
			if err != nil || string(got) != tc.want {
				t.Errorf("Read(%s) = %q, %v; want %q", tc.name, got, err, tc.want)
			}
		})
	}
}

// A read that failed keeps failing: the stream is out of step with its
// frames, so nothing after the failure can be trusted.
func TestReadErrorSticks(t *testing.T) {
	t.Parallel()
	c := reader(frames(frame(true, 0x3, false, nil), frame(true, opText, false, []byte("after"))), false, 0, nil)
	_, first := c.Read()
	got, second := c.Read()
	if first == nil || !errors.Is(second, ErrProtocol) {
		t.Errorf("Read after a failed Read = %q, %v; want the first error again (%v)", got, second, first)
	}
}

// What the reader answers: a pong with the ping's payload, the close
// echoed, and a close with a code that says why for a stream it refuses.
func TestReadAnswers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		stream []byte
		max    int
		want   []byte // the frames written back
	}{
		{name: "ping", stream: frames(frame(true, opPing, false, []byte("p")), frame(true, opText, false, []byte("x"))), want: frame(true, opPong, true, []byte("p"))},
		{name: "close", stream: frame(true, opClose, false, []byte{0x03, 0xe8}), want: frame(true, opClose, true, []byte{0x03, 0xe8})},
		{name: "too large", stream: frame(true, opText, false, bytes.Repeat([]byte{'x'}, 20)), max: 10, want: frame(true, opClose, true, []byte{0x03, 0xf1})},
		{name: "protocol error", stream: frame(true, 0x3, false, nil), want: frame(true, opClose, true, []byte{0x03, 0xea})},
		{name: "invalid text", stream: frame(true, opText, false, []byte{0xff}), want: frame(true, opClose, true, []byte{0x03, 0xef})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			c := reader(tc.stream, false, tc.max, &out)
			c.maskKey = func() [4]byte { return [4]byte{1, 2, 3, 4} } // the key frame masks with
			_, _ = c.Read()
			if !bytes.Equal(out.Bytes(), tc.want) {
				t.Errorf("Read(%s) answered % x, want % x", tc.name, out.Bytes(), tc.want)
			}
		})
	}
}

func TestWriteFrames(t *testing.T) {
	t.Parallel()
	for _, size := range []int{0, 125, 126, 65535, 65536} {
		var out bytes.Buffer
		c := newConn(nil, nil, &out, true, 0)
		msg := bytes.Repeat([]byte{'y'}, size)
		if err := c.Write(msg); err != nil {
			t.Fatalf("Write(%d bytes) = %v", size, err)
		}
		if want := frame(true, opText, false, msg); !bytes.Equal(out.Bytes(), want) {
			t.Errorf("Write(%d bytes) as the server wrote % x…, want % x…", size, head(out.Bytes()), head(want))
		}
	}
}

func head(b []byte) []byte { return b[:min(len(b), 12)] }

func TestWriteAfterClose(t *testing.T) {
	t.Parallel()
	c := dial(t, echo(t))
	_ = c.Close()
	if err := c.Write([]byte("late")); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Write after Close = %v, want net.ErrClosed", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("a second Close = %v, want nil", err)
	}
}

// Close does not wait on a writer stuck behind a peer that reads
// nothing: the server closes a connection to stop its pusher.
func TestCloseUnblocksWriter(t *testing.T) {
	t.Parallel()
	stuck := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(w, r, 0)
		if err != nil {
			return
		}
		<-stuck // reads nothing until the test is done
		_ = c.Close()
	}))
	t.Cleanup(func() { close(stuck); srv.Close() })
	c := dial(t, srv)
	wrote := make(chan error, 1)
	go func() {
		big := bytes.Repeat([]byte{'z'}, 1<<20)
		for {
			if err := c.Write(big); err != nil {
				wrote <- err
				return
			}
		}
	}()
	closed := make(chan struct{})
	go func() { _ = c.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return while a Write was stuck")
	}
	if err := <-wrote; err == nil {
		t.Error("the stuck Write returned nil after Close, want an error")
	}
}

func TestDialRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{name: "wrong accept", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Upgrade", "websocket")
			w.Header().Set("Connection", "Upgrade")
			w.Header().Set("Sec-WebSocket-Accept", "wrong")
			w.WriteHeader(http.StatusSwitchingProtocols)
		}, want: "Sec-WebSocket-Accept"},
		{name: "no upgrade header", handler: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Connection", "Upgrade")
			w.Header().Set("Sec-WebSocket-Accept", acceptKey(r.Header.Get("Sec-WebSocket-Key")))
			w.WriteHeader(http.StatusSwitchingProtocols)
		}, want: "Upgrade"},
		{name: "an extension", handler: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Upgrade", "websocket")
			w.Header().Set("Connection", "Upgrade")
			w.Header().Set("Sec-WebSocket-Accept", acceptKey(r.Header.Get("Sec-WebSocket-Key")))
			w.Header().Set("Sec-WebSocket-Extensions", "permessage-deflate")
			w.WriteHeader(http.StatusSwitchingProtocols)
		}, want: "extension"},
		{name: "refused", handler: func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"code":"auth"}`, http.StatusUnauthorized)
		}, want: "401"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewTLSServer(tc.handler)
			t.Cleanup(srv.Close)
			_, resp, err := Dial(t.Context(), wss(srv), tlsConfig(srv), nil, 0)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Dial to a server that answers with %s = %v, want an error naming %s", tc.name, err, tc.want)
			}
			if tc.name == "refused" {
				if resp == nil || resp.StatusCode != http.StatusUnauthorized {
					t.Fatalf("Dial refused: response %v, want the 401", resp)
				}
				body, _ := io.ReadAll(resp.Body)
				if !strings.Contains(string(body), `"auth"`) {
					t.Errorf("Dial refused: body %q, want the server's", body)
				}
			}
		})
	}
}

func TestDialSendsHeader(t *testing.T) {
	t.Parallel()
	got := make(chan string, 1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("X-Test") + " " + r.Host + " " + r.URL.RequestURI()
		if c, err := Accept(w, r, 0); err == nil {
			_ = c.Close()
		}
	}))
	t.Cleanup(srv.Close)
	u := wss(srv) + "v1/connect?a=b"
	c, _, err := Dial(t.Context(), u, tlsConfig(srv), http.Header{"X-Test": {"yes"}}, 0)
	if err != nil {
		t.Fatalf("Dial(%s) = %v", u, err)
	}
	_ = c.Close()
	want := "yes " + strings.TrimPrefix(srv.URL, "https://") + " /v1/connect?a=b"
	if g := <-got; g != want {
		t.Errorf("the server saw %q, want %q", g, want)
	}
}

func TestDialScheme(t *testing.T) {
	t.Parallel()
	if _, _, err := Dial(t.Context(), "https://example.com/", nil, nil, 0); err == nil || !strings.Contains(err.Error(), "scheme") {
		t.Errorf("Dial(https://…) = %v, want a refusal of the scheme", err)
	}
}

// A server pings, and the client's Read goes on past it to the message;
// the server's Read goes on past the pong to the next.
func TestPing(t *testing.T) {
	t.Parallel()
	got := make(chan string, 1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(w, r, 0)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		if c.Ping() != nil || c.Write([]byte("after the ping")) != nil {
			return
		}
		m, _ := c.Read() // past the pong
		got <- string(m)
	}))
	t.Cleanup(srv.Close)
	c := dial(t, srv)
	if m, err := c.Read(); err != nil || string(m) != "after the ping" {
		t.Fatalf("Read after a ping = %q, %v; want the message", m, err)
	}
	if err := c.Write([]byte("after the pong")); err != nil {
		t.Fatal(err)
	}
	if m := <-got; m != "after the pong" {
		t.Errorf("the server's Read after the pong = %q, want the message", m)
	}
}

func TestPingFrame(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := newConn(nil, nil, &out, true, 0).Ping(); err != nil {
		t.Fatal(err)
	}
	if want := frame(true, opPing, false, nil); !bytes.Equal(out.Bytes(), want) {
		t.Errorf("Ping wrote % x, want % x", out.Bytes(), want)
	}
}

func TestStream(t *testing.T) {
	t.Parallel()
	c := dial(t, echo(t))
	s := Stream(c)
	if _, err := s.Write([]byte("one line\n")); err != nil {
		t.Fatalf("Stream.Write = %v", err)
	}
	if _, err := s.Write([]byte("another line\n")); err != nil {
		t.Fatalf("Stream.Write = %v", err)
	}
	r := bufio.NewReader(s)
	for _, want := range []string{"one line\n", "another line\n"} {
		got, err := r.ReadString('\n')
		if err != nil || got != want {
			t.Errorf("a line read through Stream = %q, %v; want %q", got, err, want)
		}
	}
	small := make([]byte, 3)
	if _, err := s.Write([]byte("abcdefg")); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for len(got) < 7 {
		n, err := s.Read(small)
		if err != nil {
			t.Fatalf("Stream.Read = %v", err)
		}
		got = append(got, small[:n]...)
	}
	if string(got) != "abcdefg" {
		t.Errorf("a message read 3 bytes at a time = %q, want abcdefg", got)
	}
	if err := s.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	var ne net.Error
	if _, err := s.Read(small); !errors.As(err, &ne) || !ne.Timeout() {
		t.Errorf("Stream.Read past its deadline = %v, want a timeout", err)
	}
	if s.LocalAddr() == nil || s.RemoteAddr() == nil {
		t.Error("Stream has no addresses")
	}
}

// FuzzRead feeds a reader on either side a stream of anything: it must
// fail or deliver a message within its bound, and never panic or
// allocate past the bound.
func FuzzRead(f *testing.F) {
	f.Add(frame(true, opText, false, []byte("hi")), false)
	f.Add(frame(true, opText, true, []byte("hi")), true)
	f.Add(frames(frame(false, opText, false, []byte("a")), frame(true, opContinue, false, []byte("b"))), false)
	f.Add(frames(frame(false, opText, true, []byte("a")), frame(true, opPing, true, nil), frame(true, opContinue, true, []byte("b"))), true)
	f.Add(frame(true, opClose, false, []byte{3, 232}), false)
	f.Add(frame(true, opBinary, false, bytes.Repeat([]byte{0}, 300)), false)
	f.Add([]byte{0x81, 127, 0, 0, 0, 0, 0, 0, 1, 0}, false)
	const limit = 1 << 12
	f.Fuzz(func(t *testing.T, stream []byte, server bool) {
		c := reader(stream, server, limit, io.Discard)
		for range 8 {
			m, err := c.Read()
			if err != nil {
				return
			}
			if len(m) > limit {
				t.Fatalf("Read returned %d bytes, past the bound of %d", len(m), limit)
			}
		}
	})
}

// A message larger than the connection's bound is refused before it is
// written, so a write never allocates past the bound.
func TestWriteRefusesPastTheBound(t *testing.T) {
	t.Parallel()
	a, b := net.Pipe()
	defer func() { _ = a.Close(); _ = b.Close() }()
	c := newConn(a, bufio.NewReader(a), io.Discard, true, 16)
	if err := c.Write(make([]byte, 17)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Write of 17 bytes on a 16-byte bound = %v, want ErrTooLarge", err)
	}
}

// A frame's payload is allocated as its bytes arrive, not as its header
// declares: a peer that names a message of the bound and sends a few
// bytes makes the reader hold a few bytes, not the bound.
func TestReadAllocatesWhatArrives(t *testing.T) {
	// Serial: it reads the process's allocation counter, which a parallel
	// test's allocations would add to.
	const declared = 4 << 20
	head := []byte{0x80 | opBinary, 0x80 | 127}
	head = binary.BigEndian.AppendUint64(head, declared)
	head = append(head, 1, 2, 3, 4)                          // the masking key
	stream := append(head, bytes.Repeat([]byte{'x'}, 16)...) // then the peer stops
	c := reader(stream, true, declared, nil)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := c.Read()
	runtime.ReadMemStats(&after)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Read = %v, want io.ErrUnexpectedEOF", err)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 1<<20 {
		t.Errorf("reading a frame that declared %d bytes and sent 16 allocated %d bytes; want what arrived, under 1 MiB", declared, got)
	}
}

// SetMax lowers the bound of the messages read after it, and raises it
// again.
func TestSetMax(t *testing.T) {
	t.Parallel()
	big := bytes.Repeat([]byte{'a'}, 100)
	c := reader(frames(frame(true, opText, true, big), frame(true, opText, true, big)), true, 0, nil)
	c.SetMax(99)
	if _, err := c.Read(); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Read past SetMax(99) = %v, want ErrTooLarge", err)
	}
	d := reader(frames(frame(true, opText, true, big)), true, 0, nil)
	d.SetMax(99)
	d.SetMax(100)
	if m, err := d.Read(); err != nil || len(m) != 100 {
		t.Fatalf("Read after SetMax(100) = %d bytes, %v; want 100", len(m), err)
	}
}

// A WebSocket dialed over TLS has the session's state, which a caller
// binds a proof to; one over plain HTTP, or read from buffers, has
// none.
func TestTLSState(t *testing.T) {
	t.Parallel()
	c := dial(t, echo(t))
	if cs := c.TLS(); cs == nil || !cs.HandshakeComplete {
		t.Fatalf("TLS() of a wss connection = %+v, want its completed handshake", cs)
	}
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := Accept(w, r, 0); err == nil {
			_ = c.Close()
		}
	}))
	t.Cleanup(plain.Close)
	pc, _, err := Dial(t.Context(), "ws"+strings.TrimPrefix(plain.URL, "http")+"/", nil, nil, 0)
	if err != nil {
		t.Fatalf("Dial(ws) = %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	if cs := pc.TLS(); cs != nil {
		t.Errorf("TLS() of a ws connection = %+v, want nil", cs)
	}
	if cs := (&Conn{}).TLS(); cs != nil {
		t.Errorf("TLS() of a Conn with no connection = %+v, want nil", cs)
	}
}

// SetMax runs on Read's goroutine while Write and Close run on others,
// as the server's key proof does when ariesd stops: the race detector
// fails a bound that both sides touch. SetMax bounds reads alone, so a
// write keeps the bound the connection was made with.
func TestSetMaxBesideWriteAndClose(t *testing.T) {
	t.Parallel()
	c := newConn(nil, bufio.NewReader(bytes.NewReader(nil)), io.Discard, true, 64)
	start := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-start
		for range 100 {
			c.SetMax(16)
			c.SetMax(0)
		}
	}()
	go func() { <-start; _ = c.Write(make([]byte, 32)) }()
	go func() { <-start; _ = c.Close() }()
	close(start)
	<-done

	d := newConn(nil, nil, io.Discard, true, 64)
	d.SetMax(16)
	if err := d.Write(make([]byte, 32)); err != nil {
		t.Errorf("Write of 32 bytes after SetMax(16) on a 64-byte connection = %v, want nil", err)
	}
	if err := d.Write(make([]byte, 65)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("Write of 65 bytes on a 64-byte connection = %v, want ErrTooLarge", err)
	}
}

// With a write timeout, a pong to a peer that reads nothing fails, and
// the connection is closed, instead of Read's goroutine waiting on it
// for ever.
func TestWriteTimeoutEndsStalledPong(t *testing.T) {
	t.Parallel()
	a, b := net.Pipe() // unbuffered: a write waits for a read
	defer func() { _ = a.Close(); _ = b.Close() }()
	c := newConn(a, bufio.NewReader(a), a, true, 0)
	c.SetWriteTimeout(100 * time.Millisecond)
	go func() { _, _ = b.Write(frame(true, opPing, true, nil)) }() // then reads nothing
	done := make(chan error, 1)
	go func() { _, err := c.Read(); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Read = nil after a pong that could not be written")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Read still waits on a pong to a peer that reads nothing")
	}
	if err := c.Write([]byte("x")); err == nil {
		t.Error("Write after the stalled pong = nil, want an error: the connection is closed")
	}
}
