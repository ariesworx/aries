package ws

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// echo serves one connection: every message it reads it writes back.
func echo(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(w, r)
		if err != nil {
			return // Accept has answered with an error status
		}
		defer c.Close()
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

func dial(t *testing.T, srv *httptest.Server) *Conn {
	t.Helper()
	cfg := srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	c, _, err := Dial(t.Context(), "wss"+strings.TrimPrefix(srv.URL, "https")+"/", cfg, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
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
			t.Fatalf("Write(%d bytes): %v", size, err)
		}
		got, err := c.Read()
		if err != nil {
			t.Fatalf("Read after Write(%d bytes): %v", size, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("echo of %d bytes = %d bytes, want the same", size, len(got))
		}
	}
}

func TestReadAfterCloseIsEOF(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(w, r)
		if err != nil {
			return
		}
		_ = c.Close()
	}))
	t.Cleanup(srv.Close)
	c := dial(t, srv)
	if _, err := c.Read(); !errors.Is(err, io.EOF) {
		t.Errorf("Read after the server closed = %v, want io.EOF", err)
	}
}

func TestAcceptRefusesPlainRequest(t *testing.T) {
	t.Parallel()
	srv := echo(t)
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("GET without Upgrade: status %d, want 400", resp.StatusCode)
	}
}

// frames builds a raw stream of frames, as a peer would send them.
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
		b.Write([]byte{byte(n >> 8), byte(n)})
	default:
		b.WriteByte(m | 127)
		for i := 7; i >= 0; i-- {
			b.WriteByte(byte(n >> (8 * i)))
		}
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

func TestReadFrames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		server   bool // reading as the server, which requires masked frames
		stream   []byte
		want     string
		wantErr  bool
		maxBytes int
	}{
		{name: "one text frame", stream: frame(true, opText, false, []byte("hi")), want: "hi"},
		{name: "fragments", stream: append(frame(false, opText, false, []byte("a")), frame(true, opContinue, false, []byte("b"))...), want: "ab"},
		{name: "ping between fragments", stream: append(append(frame(false, opText, false, []byte("a")), frame(true, opPing, false, []byte("p"))...), frame(true, opContinue, false, []byte("b"))...), want: "ab"},
		{name: "masked, read by the server", server: true, stream: frame(true, opText, true, []byte("hi")), want: "hi"},
		{name: "unmasked, read by the server", server: true, stream: frame(true, opText, false, []byte("hi")), wantErr: true},
		{name: "masked, read by the client", stream: frame(true, opText, true, []byte("hi")), wantErr: true},
		{name: "over the limit", stream: frame(true, opText, false, bytes.Repeat([]byte{'x'}, 200)), maxBytes: 100, wantErr: true},
		{name: "fragments over the limit", stream: append(frame(false, opText, false, bytes.Repeat([]byte{'x'}, 60)), frame(true, opContinue, false, bytes.Repeat([]byte{'x'}, 60))...), maxBytes: 100, wantErr: true},
		{name: "continuation first", stream: frame(true, opContinue, false, []byte("a")), wantErr: true},
		{name: "fragmented ping", stream: frame(false, opPing, false, []byte("a")), wantErr: true},
		{name: "reserved bits", stream: append([]byte{0x80 | 0x40 | opText, 0}, nil...), wantErr: true},
		{name: "unknown opcode", stream: frame(true, 0x3, false, nil), wantErr: true},
		{name: "truncated", stream: frame(true, opText, false, []byte("hello"))[:4], wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &Conn{r: bufio.NewReader(bytes.NewReader(tc.stream)), w: io.Discard, server: tc.server, max: tc.maxBytes}
			got, err := c.Read()
			if tc.wantErr {
				if err == nil {
					t.Errorf("Read(%s) = %q, want an error", tc.name, got)
				}
				return
			}
			if err != nil || string(got) != tc.want {
				t.Errorf("Read(%s) = %q, %v, want %q", tc.name, got, err, tc.want)
			}
		})
	}
}

func TestDialRefusesWrongAccept(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Upgrade", "websocket")
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Sec-WebSocket-Accept", "wrong")
		w.WriteHeader(http.StatusSwitchingProtocols)
	}))
	t.Cleanup(srv.Close)
	cfg := &tls.Config{RootCAs: srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}
	if _, _, err := Dial(t.Context(), "wss"+strings.TrimPrefix(srv.URL, "https"), cfg, nil); err == nil {
		t.Error("Dial to a server with a wrong Sec-WebSocket-Accept succeeded, want an error")
	}
}

func FuzzRead(f *testing.F) {
	f.Add(frame(true, opText, false, []byte("hi")), false)
	f.Add(frame(true, opText, true, []byte("hi")), true)
	f.Add(append(frame(false, opText, false, []byte("a")), frame(true, opContinue, false, []byte("b"))...), false)
	f.Add(frame(true, opClose, false, []byte{3, 232}), false)
	f.Fuzz(func(t *testing.T, stream []byte, server bool) {
		c := &Conn{r: bufio.NewReader(bytes.NewReader(stream)), w: io.Discard, server: server, max: 1 << 16}
		for range 8 {
			if _, err := c.Read(); err != nil {
				return
			}
		}
	})
}
