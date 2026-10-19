package server

import (
	"crypto/tls"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/keyproof"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/wsconn"
)

// A signed-in client that pings and then stops reading cannot hold its
// connection, or its slot, past ws_write_timeout: the server's pong
// stalls once the client's end is full, and the write gives up.
func TestHTTPSStalledReaderIsClosed(t *testing.T) {
	t.Parallel()
	cfg := Config{AuthorizedKeys: handFile(t, bridgeLine(t, "alice", "alice"))}
	cfg.Limits = Limits{ConnsPerPrincipal: 1, WSWriteTimeout: Duration(300 * time.Millisecond)}
	u, pool := listenHTTPS(t, &cfg, func(h *HTTPS) { h.Ping = time.Hour })
	s, _ := newServerConfig(t, cfg, nil)
	serving(t, s)
	c, _, err := wsDial(t, u, pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if f := prove(t, c, u, signerOf(t, "alice"), nil); f != nil {
		t.Fatalf("alice's proof: %v", f.Err)
	}
	if w := hello(t, c); w.Err != nil {
		t.Fatalf("welcome: %v", w.Err)
	}
	attempt := func() bool {
		c2, _, err := wsconn.Dial(t.Context(), keyproof.ConnectURL(u), &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}, nil, 0)
		if err != nil {
			return false
		}
		defer func() { _ = c2.Close() }()
		if f := prove(t, c2, u, signerOf(t, "alice"), nil); f != nil {
			return false
		}
		return hello(t, c2).Err == nil
	}
	if attempt() {
		t.Fatal("a second connection of alice's was taken at conns_per_principal 1")
	}
	// Ask for answers until the server's writes stall: this client
	// never reads, so the answers and pongs fill both ends. The ping
	// is the finding's trigger, the requests supply the volume a
	// loopback buffer takes.
	go func() {
		st := wsconn.Stream(c)
		enc := proto.NewEncoder(st)
		for id := uint64(1); c.Ping() == nil; id++ {
			req, _ := proto.Request(id, proto.OpReady, proto.LimitArgs{})
			if enc.Encode(req) != nil {
				return
			}
		}
	}()
	// The slot is freed once the server gives up on the stalled write.
	deadline := time.Now().Add(patience)
	for {
		if attempt() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the slot of a client that pings and stops reading was not freed")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
