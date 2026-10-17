package server

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/keyproof"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/wsconn"
)

// Until its key is proved, a WebSocket's messages are held to the key
// proof's own bound, not ws_message_bytes: a peer that has proved
// nothing cannot make the server read, or hold, a message of 4 MiB.
// Once proved, the connection takes messages up to ws_message_bytes.
func TestHTTPSProofMessageBound(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T) *wsconn.Conn {
		cfg := Config{AuthorizedKeys: handFile(t, bridgeLine(t, "alice", "alice"))}
		listenHTTPS(t, &cfg, func(h *HTTPS) {
			h.TLS, h.Trusted, h.URLs = nil, []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, []string{"https://aries.example.com"}
		})
		addr := cfg.HTTPS.Listener.Addr().String()
		s, _ := newServerConfig(t, cfg, nil)
		serving(t, s)
		c, _, err := wsconn.Dial(t.Context(), "ws://"+addr+keyproof.ConnectPath, nil, nil, 0)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}

	t.Run("before the proof", func(t *testing.T) {
		t.Parallel()
		c := setup(t)
		if err := c.Write([]byte(strings.Repeat("x", maxProofMessage+1))); err != nil {
			t.Fatalf("write: %v", err)
		}
		if m, err := c.Read(); err == nil {
			t.Fatalf("a message of %d bytes before the proof was read and answered %q; want the connection closed", maxProofMessage+1, m)
		}
	})

	t.Run("after the proof", func(t *testing.T) {
		t.Parallel()
		c := setup(t)
		if f := prove(t, c, "https://aries.example.com", signerOf(t, "alice"), nil); f != nil {
			t.Fatalf("alice's proof: %v", f.Err)
		}
		if w := hello(t, c); w.Err != nil {
			t.Fatalf("hello: %v", w.Err)
		}
		req, err := json.Marshal(proto.Frame{T: proto.FrameReq, ID: 1, Op: proto.OpShow,
			Args: json.RawMessage(`{"id":"` + strings.Repeat("x", 70<<10) + `"}`)})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Write(append(req, '\n')); err != nil {
			t.Fatalf("write: %v", err)
		}
		m, err := c.Read()
		if err != nil {
			t.Fatalf("a request of %d bytes after the proof closed the connection: %v; want an answer", len(req), err)
		}
		var f proto.Frame
		if err := json.Unmarshal(m, &f); err != nil || f.ID != 1 {
			t.Fatalf("answer %q: %v; want the answer to request 1", m, err)
		}
	})
}
