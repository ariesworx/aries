package client

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/internal/proto"
)

// Benchmarks of what every connecting sfx command does before its
// request (docs/design/profile-sfx.md): find and read .starfix.yaml, load
// the keys to offer, dial and shake hands; and of one call. Run with:
//
//	go test -run '^$' -bench Client -benchmem ./internal/client/
//
// BenchmarkClientDial and BenchmarkClientCall talk to an SSH server in
// this process, so their allocations are both ends'; the client's alone
// are in the document, measured from the outside.

const benchProject = "6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f"

// benchKeyFile writes key to dir/name as OpenSSH writes one, and returns
// the path.
func benchKeyFile(b *testing.B, dir, name string, key crypto.PrivateKey) string {
	b.Helper()
	block, err := ssh.MarshalPrivateKey(key, "alice@example.com")
	if err != nil {
		b.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		b.Fatal(err)
	}
	return path
}

func benchEd25519(b *testing.B) ed25519.PrivateKey {
	b.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		b.Fatal(err)
	}
	return key
}

// benchRepo makes a repository whose .starfix.yaml names a server at
// port with host key fingerprint hostKey, and returns a directory three
// levels inside it.
func benchRepo(b *testing.B, port int, hostKey, key string) string {
	b.Helper()
	root := b.TempDir()
	deep := filepath.Join(root, "internal", "store", "migrations")
	if err := os.MkdirAll(deep, 0o700); err != nil {
		b.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		b.Fatal(err)
	}
	cfg := fmt.Sprintf("project: %s\nserver:\n  host: 127.0.0.1\n  port: %d\n  user: starfix\n  host_key: %s\n", benchProject, port, hostKey)
	if key != "" {
		cfg += "key: " + key + "\n"
	}
	if err := os.WriteFile(filepath.Join(root, ConfigFile), []byte(cfg), 0o600); err != nil {
		b.Fatal(err)
	}
	return deep
}

const benchHostKey = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// BenchmarkClientLoadConfig is the search for .starfix.yaml from three
// directories below the repository's top, the checks on the file, and
// its parsing and validation.
func BenchmarkClientLoadConfig(b *testing.B) {
	dir := benchRepo(b, 22, benchHostKey, "")
	b.ReportAllocs()
	for b.Loop() {
		if _, err := LoadConfig(dir); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkClientLoadConfigNone is the search that finds nothing, which
// `prime --hook` and `usage --hook` make in every repository that is not
// a starfix one.
func BenchmarkClientLoadConfigNone(b *testing.B) {
	dir := benchRepo(b, 22, benchHostKey, "")
	if err := os.Remove(filepath.Join(dir, "..", "..", "..", ConfigFile)); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := LoadConfig(dir); err == nil {
			b.Fatal("found a config")
		}
	}
}

// BenchmarkClientIdentity is loading the keys to offer: one configured
// key file of each kind, and the default identities of a home that has
// an RSA, an ECDSA and an Ed25519 key, every one of which is read and
// parsed before the server has said which it knows.
func BenchmarkClientIdentity(b *testing.B) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		b.Fatal(err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		b.Fatal(err)
	}
	edKey := benchEd25519(b)
	none := func(string) string { return "" }

	for _, tc := range []struct {
		name string
		key  crypto.PrivateKey
	}{{"configured-ed25519", edKey}, {"configured-ecdsa", ecKey}, {"configured-rsa3072", rsaKey}} {
		b.Run(tc.name, func(b *testing.B) {
			path := benchKeyFile(b, b.TempDir(), "key", tc.key)
			getenv := func(k string) string {
				if k == EnvKeyFile {
					return path
				}
				return ""
			}
			home := func() (string, error) { return "", os.ErrNotExist }
			b.ReportAllocs()
			for b.Loop() {
				id, err := loadIdentity(&Config{}, getenv, home)
				if err != nil {
					b.Fatal(err)
				}
				id.close()
			}
		})
	}
	b.Run("default-rsa-ecdsa-ed25519", func(b *testing.B) {
		dir := b.TempDir()
		if err := os.Mkdir(filepath.Join(dir, ".ssh"), 0o700); err != nil {
			b.Fatal(err)
		}
		benchKeyFile(b, filepath.Join(dir, ".ssh"), "id_rsa", rsaKey)
		benchKeyFile(b, filepath.Join(dir, ".ssh"), "id_ecdsa", ecKey)
		benchKeyFile(b, filepath.Join(dir, ".ssh"), "id_ed25519", edKey)
		home := func() (string, error) { return dir, nil }
		b.ReportAllocs()
		for b.Loop() {
			id, err := loadIdentity(&Config{}, none, home)
			if err != nil {
				b.Fatal(err)
			}
			id.close()
		}
	})
}

// benchSSHD is an SSH server that accepts one key and, for any exec,
// answers the hello and then every request with an empty result, or for
// list with issues.
type benchSSHD struct {
	port    int
	hostFpr string
	issues  proto.ListResult
}

func newBenchSSHD(b *testing.B, accept ssh.PublicKey) *benchSSHD {
	b.Helper()
	host, err := ssh.NewSignerFromKey(benchEd25519(b))
	if err != nil {
		b.Fatal(err)
	}
	want := string(accept.Marshal())
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if string(key.Marshal()) != want {
			return nil, fmt.Errorf("unknown key")
		}
		return nil, nil
	}}
	cfg.AddHostKey(host)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	s := &benchSSHD{port: l.Addr().(*net.TCPAddr).Port, hostFpr: ssh.FingerprintSHA256(host.PublicKey())}
	for i := range 500 {
		s.issues.Issues = append(s.issues.Issues, proto.Summary{ID: fmt.Sprintf("sf-%08d", i), Status: "open", Priority: i % 5,
			Title: fmt.Sprintf("Reconciler loses the first of two overlapping writes (case %d)", i)})
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	conns := map[net.Conn]struct{}{}
	b.Cleanup(func() {
		_ = l.Close()
		mu.Lock()
		for nc := range conns {
			_ = nc.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	wg.Go(func() {
		for {
			nc, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns[nc] = struct{}{}
			mu.Unlock()
			wg.Go(func() {
				s.serve(nc, cfg)
				mu.Lock()
				delete(conns, nc)
				mu.Unlock()
			})
		}
	})
	return s
}

func (s *benchSSHD) serve(nc net.Conn, cfg *ssh.ServerConfig) {
	defer func() { _ = nc.Close() }()
	sc, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		return
	}
	defer func() { _ = sc.Close() }()
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		ch, creqs, err := nch.Accept()
		if err != nil {
			return
		}
		for req := range creqs {
			if req.Type != "exec" {
				_ = req.Reply(false, nil)
				continue
			}
			_ = req.Reply(true, nil)
			s.bridge(ch)
			_ = ch.Close()
			break
		}
	}
}

// bridge speaks the protocol on one session until it ends.
func (s *benchSSHD) bridge(ch ssh.Channel) {
	enc, dec := proto.NewEncoder(ch), proto.NewDecoder(ch)
	for {
		f, err := dec.Decode()
		if err != nil {
			return
		}
		var res *proto.Frame
		switch {
		case f.T == proto.FrameHello:
			res = &proto.Frame{T: proto.FrameWelcome, Version: "v0.2.0", Min: proto.ProtoMin, Max: proto.ProtoMax,
				Session: f.Session, Principal: "alice"}
		case f.Op == proto.OpList:
			res, _ = proto.Response(f.ID, s.issues, nil)
		default:
			res, _ = proto.Response(f.ID, proto.Empty{}, nil)
		}
		if enc.Encode(res) != nil {
			return
		}
	}
}

// benchDialed returns the config and options of a client whose key the
// server accepts.
func benchDialed(b *testing.B) (*Config, Options) {
	b.Helper()
	key := benchEd25519(b)
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		b.Fatal(err)
	}
	s := newBenchSSHD(b, signer.PublicKey())
	dir := benchRepo(b, s.port, s.hostFpr, "id_ed25519")
	cfg, err := LoadConfig(dir)
	if err != nil {
		b.Fatal(err)
	}
	benchKeyFile(b, cfg.Root, "id_ed25519", key)
	return cfg, Options{Version: "v0.2.0", Session: "s-bench", Machine: "laptop",
		Getenv:      func(string) string { return "" },
		UserHomeDir: func() (string, error) { return "", os.ErrNotExist }}
}

// BenchmarkClientDial is one connection, from the key file to the
// welcome, and its close: TCP to this machine, the SSH key exchange, the
// pinned host key check, public key authentication, the session and the
// version handshake.
func BenchmarkClientDial(b *testing.B) {
	cfg, opts := benchDialed(b)
	ctx := b.Context()
	b.ReportAllocs()
	for b.Loop() {
		c, err := Dial(ctx, cfg, opts)
		if err != nil {
			b.Fatal(err)
		}
		if err := c.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkClientCall is one request and its decoded result on an open
// connection: an empty result, and a page of 500 issues.
func BenchmarkClientCall(b *testing.B) {
	cfg, opts := benchDialed(b)
	ctx := b.Context()
	c, err := Dial(ctx, cfg, opts)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = c.Close() })
	b.Run("empty", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var out proto.Empty
			if err := c.Call(ctx, proto.OpDepAdd, proto.Empty{}, &out); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("list-500", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var out proto.ListResult
			if err := c.Call(ctx, proto.OpList, proto.ListArgs{Limit: 500}, &out); err != nil {
				b.Fatal(err)
			}
		}
	})
}
