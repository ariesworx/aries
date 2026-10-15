package client

// What Dial does when the server accepts the key and then stops
// answering, or closes: between the SSH handshake and the welcome.

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/internal/proto"
)

// patience turns a hang into a failure that says what did not happen. It
// is not a time anything here is expected to take.
const patience = time.Minute

// settled fails the test if, once its cleanups have run, more goroutines
// are running than when it was called. It waits for them to settle: a
// closed connection's goroutines take a moment to return.
func settled(t *testing.T) {
	t.Helper()
	before := runtime.NumGoroutine()
	t.Cleanup(func() {
		deadline := time.Now().Add(patience)
		for {
			n := runtime.NumGoroutine()
			if n <= before {
				return
			}
			if time.Now().After(deadline) {
				buf := make([]byte, 1<<20)
				t.Errorf("%d goroutines running, %d before the test:\n%s", n, before, buf[:runtime.Stack(buf, true)])
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

// until is a context whose deadline passes when the test says so, so that
// a test of what Dial does out of time waits for no clock.
type until struct {
	context.Context
	done chan struct{}
}

// newUntil returns the context and what makes its deadline pass.
func newUntil(t *testing.T) (context.Context, func()) {
	t.Helper()
	u := &until{Context: t.Context(), done: make(chan struct{})}
	return u, sync.OnceFunc(func() { close(u.done) })
}

func (u *until) Done() <-chan struct{} { return u.done }

func (u *until) Err() error {
	select {
	case <-u.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// quiet is how far a stand-in server goes with a connection whose key it
// accepted before it stops answering.
type quiet int

const (
	closesAtOnce   quiet = iota // closes the connection
	ignoresSession              // never answers the request to open a session
	ignoresExec                 // opens the session, and never answers the request to run ariesd
	ignoresHello                // runs it, reads the hello and never answers
)

// quietSSHD is a server that accepts any key and then goes quiet. It
// sends on reached when it holds what it will not answer, and on hungUp
// when the client's end of that connection has closed.
type quietSSHD struct {
	port    int
	hostKey string
	reached chan struct{}
	hungUp  chan struct{}
}

func newQuietSSHD(t *testing.T, how quiet) *quietSSHD {
	t.Helper()
	host := newKeyPair(t)
	signer, err := ssh.NewSignerFromKey(host.priv)
	if err != nil {
		t.Fatal(err)
	}
	sc := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil }}
	sc.AddHostKey(signer)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := &quietSSHD{port: l.Addr().(*net.TCPAddr).Port, hostKey: host.fingerprint(),
		reached: make(chan struct{}, 1), hungUp: make(chan struct{}, 1)}
	var wg sync.WaitGroup
	t.Cleanup(func() { _ = l.Close(); wg.Wait() })
	wg.Go(func() {
		for {
			nc, err := l.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer func() { _ = nc.Close() }()
				conn, chans, reqs, err := ssh.NewServerConn(nc, sc)
				if err != nil {
					return
				}
				wg.Go(func() { ssh.DiscardRequests(reqs) })
				d.serve(conn, chans, how)
			})
		}
	})
	return d
}

// serve goes as far as how says, then waits for the client to hang up.
func (d *quietSSHD) serve(conn *ssh.ServerConn, chans <-chan ssh.NewChannel, how quiet) {
	if how == closesAtOnce {
		_ = conn.Close()
		return
	}
	defer func() {
		_ = conn.Wait() // returns once the connection has ended
		d.hungUp <- struct{}{}
	}()
	open, ok := <-chans
	if !ok {
		return
	}
	if how == ignoresSession {
		d.reached <- struct{}{}
		return
	}
	ch, reqs, err := open.Accept()
	if err != nil {
		return
	}
	req, ok := <-reqs
	if !ok {
		return
	}
	if how == ignoresExec {
		d.reached <- struct{}{}
		return
	}
	_ = req.Reply(true, nil)
	if f, err := proto.NewDecoder(ch).Decode(); err != nil || f.T != proto.FrameHello {
		return
	}
	d.reached <- struct{}{}
}

// addr is the server's address, as a message names it.
func (d *quietSSHD) addr() string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(d.port)) }

// dial runs Dial against d on a goroutine of its own, as a checkout with
// a key of its own and no ssh-agent, and returns where its error goes.
func (d *quietSSHD) dial(ctx context.Context, t *testing.T) <-chan error {
	t.Helper()
	cfg := &Config{Project: testProject, Key: "id", Root: t.TempDir()}
	cfg.Server.Host, cfg.Server.Port, cfg.Server.User, cfg.Server.HostKey = "127.0.0.1", d.port, "aries", d.hostKey
	newKeyPair(t).write(t, cfg.Root, "id", "", false)
	result := make(chan error, 1)
	go func() {
		c, err := Dial(ctx, cfg, Options{Timeout: patience, Getenv: func(string) string { return "" },
			UserHomeDir: func() (string, error) { return cfg.Root, nil }})
		if err == nil {
			_ = c.Close()
		}
		result <- err
	}()
	return result
}

// await receives from c, or stops the test saying what did not happen.
func await[T any](t *testing.T, c <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(patience):
		buf := make([]byte, 1<<20)
		t.Fatalf("after %s, %s\n%s", patience, what, buf[:runtime.Stack(buf, true)])
		panic("unreachable")
	}
}

// A server that accepts the key and then stops answering holds Dial only
// until its time is up, or its context is canceled: Dial then says which
// step it was waiting for, and leaves no connection and no goroutine.
func TestDialGivesUpOnAQuietServer(t *testing.T) {
	// Serial: counts the process's goroutines (settled).
	tests := []struct {
		name   string
		how    quiet
		cancel bool   // the caller cancels, rather than the time running out
		want   string // the message, with the server's address for %s
	}{
		{name: "the session is never opened", how: ignoresSession,
			want: "timed out after 1m0s waiting for %s to open an ssh session"},
		{name: "ariesd is never started", how: ignoresExec,
			want: "timed out after 1m0s waiting for %s to start ariesd"},
		{name: "the hello is never answered", how: ignoresHello,
			want: "timed out after 1m0s waiting for %s to answer the hello"},
		{name: "the caller cancels", how: ignoresHello, cancel: true,
			want: "stopped waiting for %s to answer the hello: context canceled"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := newQuietSSHD(t, tc.how)
			settled(t)
			ctx, end := newUntil(t)
			if tc.cancel {
				ctx, end = context.WithCancel(t.Context())
			}
			defer end()
			result := d.dial(ctx, t)
			await(t, d.reached, "the server never got what it was to leave unanswered")
			end()
			err := await(t, result, "Dial had not returned, though its context had ended")
			pe, ok := errors.AsType[*proto.Error](err)
			if !ok || pe.Code != proto.CodeUnavailable {
				t.Fatalf("Dial = %v, want an unavailable *proto.Error", err)
			}
			if want := strings.Replace(tc.want, "%s", d.addr(), 1); pe.Message != want {
				t.Errorf("Dial message = %q, want %q", pe.Message, want)
			}
			if !strings.Contains(pe.Fix, "retry") || !strings.Contains(pe.Fix, "ariesd status") {
				t.Errorf("Dial fix = %q, want it to say to retry, and what the admin checks", pe.Fix)
			}
			if !Unreachable(err) {
				t.Errorf("Unreachable(%v) = false, want true: a later attempt may get through", err)
			}
			await(t, d.hungUp, "the server still had the client's connection open after Dial returned")
		})
	}
}

// A server that closes the connection the moment it has accepted the key
// is refused at once, however the close and the opening of the session
// fall: CI once waited ten minutes on this.
func TestDialToAServerThatClosesAtOnce(t *testing.T) {
	// Serial: counts the process's goroutines (settled).
	d := newQuietSSHD(t, closesAtOnce)
	settled(t)
	for i := range 50 {
		err := await(t, d.dial(t.Context(), t), "Dial had not returned from a server that closed the connection")
		if pe, ok := errors.AsType[*proto.Error](err); !ok || pe.Code != proto.CodeUnavailable || pe.Message != closedEarly || pe.Fix == "" {
			t.Fatalf("Dial %d = %v, want an unavailable *proto.Error with a fix, saying %q", i, err, closedEarly)
		}
	}
}

// closedEarly is what Dial says of a connection that ended once the
// server had accepted the key, before a session was open on it.
const closedEarly = "open ssh session: the server closed the connection once it had accepted the key"

// stallConn is the client's end of a connection. Once armed, its next
// write waits to be released, and then reports that it was written, as a
// write does that the system took before the connection ended.
type stallConn struct {
	net.Conn
	held    chan struct{} // closed when a write is being held
	release chan struct{} // closed to let it return
	closed  chan struct{} // closed by Close

	mu    sync.Mutex
	armed bool
	once  sync.Once
}

func (c *stallConn) arm() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed = true
}

func (c *stallConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	hold := c.armed
	c.armed = false
	c.mu.Unlock()
	if !hold {
		return c.Conn.Write(p)
	}
	close(c.held)
	<-c.release
	return len(p), nil
}

func (c *stallConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// sshPair returns an SSH client past authentication, the transport
// under it, which runs over a stallConn, and the server's end of the
// connection.
func sshPair(t *testing.T) (*ssh.Client, *transport, *stallConn, net.Conn) {
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
	t.Cleanup(func() { _ = cli.Close() })
	srv, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	served := make(chan error, 1)
	go func() {
		_, _, _, err := ssh.NewServerConn(srv, sc)
		served <- err
	}()
	stall := &stallConn{Conn: cli, held: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	nc := &transport{Conn: stall}
	//nolint:gosec // G106: both ends are the test's
	cc, chans, reqs, err := ssh.NewClientConn(nc, "the server", &ssh.ClientConfig{User: "aries", HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	if err != nil {
		t.Fatal(err)
	}
	if err := await(t, served, "the server had not finished the SSH handshake"); err != nil {
		t.Fatal(err)
	}
	return ssh.NewClient(cc, chans, reqs), nc, stall, srv
}

// A session opened on a connection that has just ended is refused at
// once. ssh, left to open it, waits forever for an answer that nothing
// is left to read, whatever is closed afterward (golang/go#82009): it
// has closed the channels it knew, and has not yet noted that writes
// fail. The test holds ssh in that state, by holding a write of its own
// while the server hangs up, and opens the session the moment the write
// returns. Without the transport's check, about every second run of this
// never returned, and every run on one processor.
func TestNoSessionIsOpenedOnAConnectionThatEnded(t *testing.T) {
	// Serial: counts the process's goroutines (settled).
	settled(t)
	for i := range 20 {
		client, nc, stall, srv := sshPair(t)
		result := make(chan error, 1)
		go func() {
			stall.arm()
			// Held in the transport's write, this keeps ssh from noting
			// that the connection is over.
			_, _, _ = client.SendRequest("keepalive@example.com", false, nil)
			_, err := start(t.Context(), client, nc, "the server", &Config{Project: testProject}, Options{}, proto.Proto)
			result <- err
		}()
		await(t, stall.held, "ssh had not written the request")
		_ = srv.Close()
		// ssh closes the connection after it has closed its channels.
		await(t, stall.closed, "ssh had not seen the server hang up")
		close(stall.release)
		err := await(t, result, "the session's opening had not returned, on a connection that had ended")
		if pe, ok := errors.AsType[*proto.Error](err); !ok || pe.Code != proto.CodeUnavailable || pe.Message != closedEarly {
			t.Fatalf("start %d = %v, want an unavailable *proto.Error saying %q", i, err, closedEarly)
		}
		_ = client.Close()
	}
}

// A read that fails while a channel is being opened is kept back until
// the request to open it is written, the open is over, or the transport
// is closed; and none is opened once a read has failed.
func TestTransportKeepsBackAFailedRead(t *testing.T) {
	// Serial: a synctest bubble's virtual time takes no real time worth sharing.
	tests := []struct {
		name    string
		release func(*transport)
	}{
		{name: "a write", release: func(nc *transport) { _, _ = nc.Write([]byte("open")) }},
		{name: "the open's end", release: (*transport).opened},
		{name: "Close", release: func(nc *transport) { _ = nc.Close() }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cli, srv := net.Pipe()
				defer func() { _ = cli.Close() }()
				nc := &transport{Conn: cli}
				if !nc.open() {
					t.Fatal("open() = false on a connection that has not failed")
				}
				_ = srv.Close()
				read := make(chan error, 1)
				go func() {
					_, err := nc.Read(make([]byte, 1))
					read <- err
				}()
				synctest.Wait()
				select {
				case err := <-read:
					t.Fatalf("Read returned %v while a channel was being opened", err)
				default:
				}
				tc.release(nc)
				synctest.Wait()
				select {
				case err := <-read:
					if !errors.Is(err, io.EOF) {
						t.Errorf("Read = %v, want io.EOF", err)
					}
				default:
					t.Fatal("Read had not returned after " + tc.name)
				}
				if nc.open() {
					t.Error("open() = true after a read failed")
				}
			})
		})
	}
	t.Run("closed before any open", func(t *testing.T) {
		cli, srv := net.Pipe()
		defer func() { _ = srv.Close() }()
		nc := &transport{Conn: cli}
		_ = nc.Close()
		if nc.open() {
			t.Error("open() = true after Close")
		}
	})
}
