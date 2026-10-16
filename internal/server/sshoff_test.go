package server

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

// With SSH off (transport.md §11.1), a bridge frame from `ariesd stdio`
// is refused unavailable, with a fix that names the setting and the
// installer's flag, and `ariesd status` still gets in: it connects to
// the socket as the daemon's own user, not through sshd.
func TestNoSSHRefusesBridge(t *testing.T) {
	t.Parallel()
	s, err := New(Config{Store: &fakeStore{}, Project: project, Version: "v0.2.0", ReapInterval: -1, PeerCheck: anyPeer, NoSSH: true})
	if err != nil {
		t.Fatal(err)
	}
	hello := &proto.Frame{T: proto.FrameHello, Proto: proto.Proto, Project: project, Session: "s", Machine: "m"}
	f, err := handshake(t, s, &proto.Frame{T: proto.FrameBridge, Principal: "alice"}, hello)
	if err != nil {
		t.Fatalf("handshake through the bridge with SSH off: %v", err)
	}
	if f.Err == nil || f.Err.Code != proto.CodeUnavailable || !strings.Contains(f.Err.Message, "SSH is off") ||
		!strings.Contains(f.Err.Fix, "transports: [https, ssh]") || !strings.Contains(f.Err.Fix, "--enable-ssh") {
		t.Errorf("a bridge frame with SSH off = %+v, want unavailable, SSH is off, and a fix naming transports and --enable-ssh", f.Err)
	}
	f, err = handshake(t, s, &proto.Frame{T: proto.FrameBridge, Principal: StatusPrincipal}, hello)
	if err != nil || f.Err != nil || f.Principal != StatusPrincipal {
		t.Errorf("ariesd status with SSH off = %+v, %v; want welcomed", f, err)
	}
}

// With SSH off the daemon writes no keys file, removes one it wrote
// before, and leaves alone a file there it did not write.
func TestNoSSHKeysFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		before string // the file's content before the daemon starts; "" for none
		after  string // its content after; "" for no file
	}{
		{name: "none", before: "", after: ""},
		{name: "one ariesd wrote", before: keysFileHeader + "restrict,command=\"/usr/local/bin/ariesd stdio --principal carol --key SHA256:x\" ssh-ed25519 AAAA\n", after: ""},
		{name: "one ariesd did not write", before: "# the operator's\n", after: "# the operator's\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newMultiWith(t, Limits{}, nil, nil, func(c *Config) {
				c.NoSSH = true
				if tc.before != "" {
					if err := os.WriteFile(c.KeysFile, []byte(tc.before), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}, dana.Principal)
			m.project(t, projA, "carol")
			if _, err := m.ctl.AddKey(t.Context(), dana, "carol", keyLine(t, "carol")); err != nil {
				t.Fatal(err)
			}
			m.refresh(t.Context())
			b, err := os.ReadFile(m.keysFile)
			switch {
			case tc.after == "" && !errors.Is(err, fs.ErrNotExist):
				t.Errorf("the keys file with SSH off, from %s: %q, %v; want no file", tc.name, b, err)
			case tc.after != "" && string(b) != tc.after:
				t.Errorf("the keys file with SSH off, from %s: %q, %v; want %q", tc.name, b, err, tc.after)
			}
			if k := m.keysStatus(); k != nil {
				t.Errorf("the keys file's status with SSH off = %+v, want none: there is no keys file to show", k)
			}
		})
	}
}

// A key added or removed with SSH off is the store's, and signs in over
// HTTPS; the admin is told that sshd does not read it.
func TestNoSSHKeyNotice(t *testing.T) {
	t.Parallel()
	m := newMultiWith(t, Limits{}, nil, nil, func(c *Config) { c.NoSSH = true }, dana.Principal)
	m.project(t, projA, "alice=admin", "carol")
	d := mustHandOff(t, m.Server, dana, projA)
	add := mustAsk[proto.KeyResult](d, proto.OpKeyAdd, proto.KeyAddArgs{Name: "carol", Key: keyLine(t, "carol")})
	if !strings.Contains(add.Warning, "SSH is off on this server") || !strings.Contains(add.Warning, "HTTPS") {
		t.Errorf("key.add with SSH off: warning %q, want it to say SSH is off and the key signs in over HTTPS", add.Warning)
	}
	rm := mustAsk[proto.KeyResult](d, proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: add.Fingerprint})
	if rm.Fingerprint != add.Fingerprint || strings.Contains(rm.Warning, "keys file") {
		t.Errorf("key.remove with SSH off = %+v, want the key removed and no word of the keys file", rm)
	}
}

// mustHandOff connects a to project proj as the HTTPS listener does
// once a key proof checks out: the bridge frame is the listener's, not
// read from the connection. It fails the test unless a is welcomed.
func mustHandOff(t *testing.T, s *Server, a store.Actor, proj string) *conn {
	t.Helper()
	srv, cli := net.Pipe()
	c := &conn{t: t, enc: proto.NewEncoder(cli), dec: proto.NewDecoder(cli), nc: cli}
	t.Cleanup(c.hangUp)
	c.wg.Go(func() {
		defer func() { _ = srv.Close() }()
		s.serveConn(t.Context(), srv, &proto.Frame{T: proto.FrameBridge, Principal: a.Principal}, nil)
	})
	c.wg.Go(func() {
		_ = c.enc.Encode(&proto.Frame{T: proto.FrameHello, Proto: proto.Proto, Project: proj, Session: a.Session, Machine: a.Machine})
	})
	if w := c.read(); w.T != proto.FrameWelcome || w.Err != nil {
		t.Fatalf("%s to project %s, handed off: welcome %+v, error %v", a.Principal, proj, w, w.Err)
	}
	return c
}

// With SSH on, the default, the bridge is served as it always was.
func TestSSHOnServesBridge(t *testing.T) {
	t.Parallel()
	s := newFakeServer(t, &fakeStore{})
	hello := &proto.Frame{T: proto.FrameHello, Proto: proto.Proto, Project: project, Session: "s", Machine: "m"}
	f, err := handshake(t, s, &proto.Frame{T: proto.FrameBridge, Principal: "alice"}, hello)
	if err != nil || f.Err != nil || f.Principal != "alice" {
		t.Errorf("a bridge frame with SSH on = %+v, %v; want alice welcomed", f, err)
	}
}
