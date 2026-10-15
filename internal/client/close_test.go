package client

import (
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/internal/proto"
)

// fullConn is the client's end of a connection whose peer, once full,
// takes nothing more: each write from then on waits until the
// connection is closed, as one does when the server reads nothing and
// the buffers between are full.
type fullConn struct {
	net.Conn
	held   chan struct{} // receives when a write starts to wait
	closed chan struct{} // closed by Close

	mu   sync.Mutex
	full bool
	once sync.Once
}

func (c *fullConn) fill() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.full = true
}

func (c *fullConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	full := c.full
	c.mu.Unlock()
	if !full {
		return c.Conn.Write(p)
	}
	select {
	case c.held <- struct{}{}:
	default:
	}
	<-c.closed
	return 0, net.ErrClosed
}

func (c *fullConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// bridgedConn returns a Conn past its welcome, over SSH on loopback TCP,
// and the client's end of that connection.
func bridgedConn(t *testing.T) (*Conn, *fullConn) {
	t.Helper()
	signer, err := ssh.NewSignerFromKey(newKeyPair(t).priv)
	if err != nil {
		t.Fatal(err)
	}
	sc := &ssh.ServerConfig{NoClientAuth: true}
	sc.AddHostKey(signer)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	cli, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := l.Accept()
	if err != nil {
		_ = cli.Close()
		t.Fatal(err)
	}
	near := &fullConn{Conn: cli, held: make(chan struct{}, 1), closed: make(chan struct{})}
	var wg sync.WaitGroup
	t.Cleanup(func() { _ = near.Close(); _ = srv.Close(); wg.Wait() })
	wg.Go(func() {
		conn, chans, reqs, err := ssh.NewServerConn(srv, sc)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		wg.Go(func() { ssh.DiscardRequests(reqs) })
		open, ok := <-chans
		if !ok {
			return
		}
		wg.Go(func() {
			for o := range chans {
				_ = o.Reject(ssh.Prohibited, "one session")
			}
		})
		ch, creqs, err := open.Accept()
		if err != nil {
			return
		}
		req, ok := <-creqs
		if !ok {
			return
		}
		_ = req.Reply(true, nil)
		wg.Go(func() { ssh.DiscardRequests(creqs) })
		if f, err := proto.NewDecoder(ch).Decode(); err != nil || f.T != proto.FrameHello {
			return
		}
		_ = proto.NewEncoder(ch).Encode(&proto.Frame{T: proto.FrameWelcome, Proto: proto.Proto, Min: proto.ProtoMin, Max: proto.ProtoMax, Session: "s-1"})
		_ = conn.Wait()
	})
	nc := &transport{Conn: near}
	//nolint:gosec // G106: both ends are the test's
	cc, chans, reqs, err := ssh.NewClientConn(nc, "the server", &ssh.ClientConfig{User: "starfix", HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	if err != nil {
		t.Fatal(err)
	}
	client := ssh.NewClient(cc, chans, reqs)
	c, err := start(t.Context(), client, nc, "the server", &Config{Project: testProject}, Options{Timeout: patience}, proto.Proto)
	if err != nil {
		_ = client.Close()
		t.Fatal(err)
	}
	return c, near
}

// Close returns, and closes the connection, when the server takes
// nothing more: ending the session writes to the server, and those
// writes wait behind one that is already waiting, so Close stops
// waiting for them after a moment. The call whose write was waiting
// ends with it.
func TestCloseWhileTheServerTakesNothing(t *testing.T) {
	// Serial: counts the process's goroutines (settled).
	settled(t)
	c, near := bridgedConn(t)
	c.closeWait = 10 * time.Millisecond
	near.fill()
	called := make(chan error, 1)
	go func() { called <- c.Call(t.Context(), proto.OpReady, proto.LimitArgs{}, nil) }()
	await(t, near.held, "the request had not been written")
	closed := make(chan error, 1)
	start := time.Now()
	go func() { closed <- c.Close() }()
	if err := await(t, closed, "Close had not returned on a server that takes nothing"); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	if took := time.Since(start); took < c.closeWait {
		t.Errorf("Close returned after %s, before its wait of %s: it did not try to end the session first", took, c.closeWait)
	}
	await(t, c.Done(), "the reader had not stopped after Close")
	if err := await(t, called, "the call had not returned after Close"); err == nil {
		t.Error("Call during Close = nil, want an error")
	}
}
