package client

// What Dial makes of the server's answer to its hello.

import (
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/starfix/internal/proto"
)

// welcomer is a stand-in for sshd and the daemon behind it: it accepts
// any key, and answers each session's hello with what answer returns
// for it. A welcome without an error keeps the session open until the
// client closes it; a refusal closes it, as the daemon does.
type welcomer struct {
	port    int
	hostKey string
	answer  func(hello *proto.Frame) *proto.Frame

	mu     sync.Mutex
	hellos []proto.Frame // every hello, in the order they came
	conns  int           // the SSH connections made
}

func newWelcomer(t *testing.T, answer func(hello *proto.Frame) *proto.Frame) *welcomer {
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
	w := &welcomer{port: l.Addr().(*net.TCPAddr).Port, hostKey: host.fingerprint(), answer: answer}
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
				w.mu.Lock()
				w.conns++
				w.mu.Unlock()
				wg.Go(func() { ssh.DiscardRequests(reqs) })
				var sessions sync.WaitGroup
				for open := range chans {
					ch, creqs, err := open.Accept()
					if err != nil {
						break
					}
					sessions.Go(func() { w.session(ch, creqs) })
				}
				sessions.Wait()
				_ = conn.Close()
			})
		}
	})
	return w
}

// session runs one session: the request to run starfixd, the hello and
// its answer.
func (w *welcomer) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer func() { _ = ch.Close() }()
	req, ok := <-reqs
	if !ok {
		return
	}
	_ = req.Reply(true, nil)
	go ssh.DiscardRequests(reqs)
	dec, enc := proto.NewDecoder(ch), proto.NewEncoder(ch)
	hello, err := dec.Decode()
	if err != nil || hello.T != proto.FrameHello {
		return
	}
	w.mu.Lock()
	w.hellos = append(w.hellos, *hello)
	w.mu.Unlock()
	welcome := w.answer(hello)
	if err := enc.Encode(welcome); err != nil || welcome.Err != nil {
		return
	}
	for {
		if _, err := dec.Decode(); err != nil {
			return // the client closed it
		}
	}
}

// dial runs Dial against w, as a checkout with a key of its own and no
// ssh-agent.
func (w *welcomer) dial(t *testing.T) (*Conn, error) {
	t.Helper()
	return w.dialPinned(t, w.hostKey)
}

// dialPinned is dial with the config pinning hostKeys, in host_keys.
func (w *welcomer) dialPinned(t *testing.T, hostKeys ...string) (*Conn, error) {
	t.Helper()
	cfg := &Config{Project: testProject, Key: "id", Root: t.TempDir()}
	cfg.Server.Host, cfg.Server.Port, cfg.Server.User, cfg.Server.HostKeys = "127.0.0.1", w.port, "starfix", hostKeys
	newKeyPair(t).write(t, cfg.Root, "id", "", false)
	c, err := Dial(t.Context(), cfg, Options{Timeout: patience, Getenv: noEnv,
		UserHomeDir: func() (string, error) { return cfg.Root, nil }})
	if err == nil {
		t.Cleanup(func() { _ = c.Close() })
	}
	return c, err
}

// protos are the protocol versions of the hellos w was sent, in order,
// and the SSH connections they came on.
func (w *welcomer) protos() ([]int, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []int
	for _, h := range w.hellos {
		out = append(out, h.Proto)
	}
	return out, w.conns
}

// server is a daemon that accepts protocols lo to hi, and refuses any
// other as a daemon does, with its range.
func server(lo, hi int) func(*proto.Frame) *proto.Frame {
	return func(hello *proto.Frame) *proto.Frame {
		w := welcome(lo, hi)
		if e := proto.CheckProto(hello.Proto, lo, hi, w.Version); e != nil {
			w.Session, w.Principal, w.Err = "", "", e
		}
		return w
	}
}

// TestDialStepsDown pins the version a client of this build speaks
// (design §18.5): protocol 6 to a server that accepts it; to a server of
// 5 alone, 5, saying hello again on the same SSH connection once the
// server's refusal has named its range; and nothing to a server that
// accepts neither, which refuses it with code version as before.
func TestDialStepsDown(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		lo, hi int
		want   int   // the protocol spoken, 0 when refused
		hellos []int // the hellos' protocols
	}{
		{"a server of 5 and 6", 5, 6, 6, []int{6}},
		{"a server of 6 and 7", 6, 7, 6, []int{6}},
		{"a server of 5", 5, 5, 5, []int{6, 5}},
		{"a server of 7 and 8", 7, 8, 0, []int{6}},
		{"a server of 3 and 4", 3, 4, 0, []int{6}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWelcomer(t, server(tc.lo, tc.hi))
			c, err := w.dial(t)
			hellos, conns := w.protos()
			switch {
			case tc.want == 0:
				if pe, ok := errors.AsType[*proto.Error](err); !ok || pe.Code != proto.CodeVersion {
					t.Errorf("Dial to %s = %v, want refused with code version", tc.name, err)
				}
			case err != nil:
				t.Fatalf("Dial to %s = %v, want it welcomed", tc.name, err)
			case c.Proto() != tc.want:
				t.Errorf("Dial to %s speaks protocol %d, want %d", tc.name, c.Proto(), tc.want)
			}
			if !slices.Equal(hellos, tc.hellos) || conns != 1 {
				t.Errorf("Dial to %s said hello speaking %v on %d SSH connections; want %v on 1", tc.name, hellos, conns, tc.hellos)
			}
		})
	}
}

// TestCallSpeaksTheConnectionsProtocol: on a connection of protocol 5,
// an op whose key protocol 6 added goes without it, which a server of 5
// would refuse as an unknown argument; on 6 it goes with it.
func TestCallSpeaksTheConnectionsProtocol(t *testing.T) {
	t.Parallel()
	for _, v := range []int{5, 6} {
		c, srv := pipeConn(t, nil)
		c.proto = v
		sent := make(chan string, 1)
		go func() {
			f := srv.req()
			if f == nil {
				sent <- ""
				return
			}
			sent <- string(f.Args)
			srv.send(&proto.Frame{T: proto.FrameRes, ID: f.ID, OK: []byte(`{"id":"sf-1","rev":2}`)})
		}()
		if err := c.Call(t.Context(), proto.OpUpdate, proto.UpdateArgs{ID: "sf-1", Rev: 1, Idem: "cli-1"}, nil); err != nil {
			t.Fatalf("Call at protocol %d = %v", v, err)
		}
		if got := <-sent; strings.Contains(got, `"idem"`) != (v >= 6) || !strings.Contains(got, `"rev":1`) {
			t.Errorf("update at protocol %d was sent as %s; want it whole, and its key sent only at 6", v, got)
		}
	}
}

// welcome is a welcome frame from a server of protocols lo to hi.
func welcome(lo, hi int) *proto.Frame {
	return &proto.Frame{T: proto.FrameWelcome, Version: "v0.2.0", Min: lo, Max: hi, Session: "s1", Principal: "alice"}
}

// TestAccessEnded pins what proves to a client that a principal's
// access to the project has ended, so that what it queued is refused
// and kept for a person rather than left to go stale (design §18.3): a
// refusal of the handshake coded forbidden that names the principal. A
// forbidden answer to a request is about that request; a refusal that
// names nobody, as an older server's does, proves nothing about whose
// access it was; and every other code, unavailable above all, is not
// the end of anyone's access.
func TestAccessEnded(t *testing.T) {
	t.Parallel()
	refusal := func(code proto.Code, principal string) func(*proto.Frame) *proto.Frame {
		return func(*proto.Frame) *proto.Frame {
			w := welcome(proto.ProtoMin, proto.ProtoMax)
			w.Session, w.Principal = "", principal
			w.Err = proto.Errf(code, "ask a person", "alice is not a member of project \"p\"")
			return w
		}
	}
	for _, tc := range []struct {
		name   string
		answer func(*proto.Frame) *proto.Frame
		want   string // "" is not proof
	}{
		{"forbidden, naming the principal", refusal(proto.CodeForbidden, "alice"), "alice"},
		{"forbidden, naming nobody", refusal(proto.CodeForbidden, ""), ""},
		{"not_found: a project not served, archived or never there", refusal(proto.CodeNotFound, "alice"), ""},
		{"unavailable", refusal(proto.CodeUnavailable, "alice"), ""},
		{"busy", refusal(proto.CodeBusy, "alice"), ""},
		{"auth: a key the server no longer holds", refusal(proto.CodeAuth, "alice"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := newWelcomer(t, tc.answer).dial(t)
			if err == nil {
				t.Fatal("Dial was welcomed, want it refused")
			}
			if pe, ok := errors.AsType[*proto.Error](err); !ok || pe.Message != "alice is not a member of project \"p\"" {
				t.Errorf("Dial's error %v is not the server's refusal as a *proto.Error", err)
			}
			who, ended := AccessEnded(err)
			if who != tc.want || ended != (tc.want != "") {
				t.Errorf("AccessEnded(%v) = %q, %v; want %q, %v", err, who, ended, tc.want, tc.want != "")
			}
			// An error is kept for later or refuses access, never both.
			if ended && Unreachable(err) {
				t.Errorf("AccessEnded(%v) and Unreachable(%v) are both true", err, err)
			}
		})
	}
	t.Run("a request refused as forbidden", func(t *testing.T) {
		t.Parallel()
		err := proto.Errf(proto.CodeForbidden, "ask the holder", "sf-1 is held by bob")
		if who, ended := AccessEnded(err); ended {
			t.Errorf("AccessEnded(%v) = %q, true; want false: it refuses one request", err, who)
		}
	})
}
