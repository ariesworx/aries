package client

// Review artifacts for docs/design/security-review-stage-3a.md, finding
// F-1. Each test demonstrates the finding against the code as it is: it
// skips with a message that states it, or fails when
// STARFIX_REVIEW_FAIL=1, and passes once it no longer reproduces.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/ariesworx/starfix/internal/proto"
)

func reviewFinding(t *testing.T, id, text string) {
	t.Helper()
	if os.Getenv("STARFIX_REVIEW_FAIL") != "" {
		t.Errorf("SECURITY REVIEW %s reproduced: %s", id, text)
		return
	}
	t.Skipf("SECURITY REVIEW %s reproduced: %s", id, text)
}

// reviewKey makes a key pair.
func reviewKey(t *testing.T) (ed25519.PrivateKey, ssh.PublicKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	return priv, pub
}

// reviewAgent runs an ssh-agent holding keys, in order, and returns its
// socket.
func reviewAgent(t *testing.T, keys ...ed25519.PrivateKey) string {
	t.Helper()
	kr := agent.NewKeyring()
	for _, k := range keys {
		if err := kr.Add(agent.AddedKey{PrivateKey: k}); err != nil {
			t.Fatal(err)
		}
	}
	dir, err := os.MkdirTemp("", "sfa")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "agent.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() { _ = l.Close(); wg.Wait() })
	wg.Go(func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			wg.Go(func() { _ = agent.ServeAgent(kr, c); _ = c.Close() })
		}
	})
	return sock
}

// reviewSSHD is a stand-in for the server's sshd: it lets in the keys of
// principals, as authorized_keys lines would, and reports on signedIn
// the principal each connection authenticated as, then closes it. offered
// counts the keys a client put to it.
type reviewSSHD struct {
	cfg      *Config
	signedIn chan string
	mu       sync.Mutex
	offered  map[string]bool
}

func newReviewSSHD(t *testing.T, principals map[string]ssh.PublicKey, keyFile string) *reviewSSHD {
	t.Helper()
	hostPriv, hostPub := reviewKey(t)
	host, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	d := &reviewSSHD{signedIn: make(chan string, 4), offered: map[string]bool{}}
	//nolint:gosec // G408: a test stand-in; it records every key offered on purpose
	sc := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		fp := ssh.FingerprintSHA256(key)
		d.mu.Lock()
		d.offered[fp] = true
		d.mu.Unlock()
		for name, k := range principals {
			if ssh.FingerprintSHA256(k) == fp {
				return &ssh.Permissions{Extensions: map[string]string{"principal": name}}, nil
			}
		}
		return nil, errors.New("no such key")
	}}
	sc.AddHostKey(host)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
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
				_ = nc.SetDeadline(time.Now().Add(10 * time.Second))
				conn, _, _, err := ssh.NewServerConn(nc, sc)
				if err != nil {
					return
				}
				d.signedIn <- conn.Permissions.Extensions["principal"]
				_ = conn.Close()
			})
		}
	})
	yaml := fmt.Sprintf("project: aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa\nserver:\n  host: 127.0.0.1\n  port: %d\n  host_key: %s\nkey: %s\n",
		l.Addr().(*net.TCPAddr).Port, ssh.FingerprintSHA256(hostPub), keyFile)
	if d.cfg, err = ParseConfig([]byte(yaml)); err != nil {
		t.Fatal(err)
	}
	return d
}

// reviewKeyFile writes priv as an OpenSSH private key file.
func reviewKeyFile(t *testing.T, priv ed25519.PrivateKey) string {
	t.Helper()
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "id_sandbox")
	if err := os.WriteFile(p, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// F-1 (a): .starfix.yaml names a key, the key of a narrow principal
// (`sandbox`), and ssh-agent holds the developer's own, a server
// admin's. sfx offers the agent's keys first, so the server lets it in
// as the admin, and nothing sfx prints says which principal it is.
func TestReviewF1AgentKeyWinsOverConfiguredKey(t *testing.T) {
	adminPriv, adminPub := reviewKey(t)
	sandboxPriv, sandboxPub := reviewKey(t)
	d := newReviewSSHD(t, map[string]ssh.PublicKey{"dana-the-admin": adminPub, "sandbox": sandboxPub}, reviewKeyFile(t, sandboxPriv))
	sock := reviewAgent(t, adminPriv)

	_, _ = Dial(context.Background(), d.cfg, Options{Timeout: 5 * time.Second, Getenv: func(k string) string {
		if k == "SSH_AUTH_SOCK" {
			return sock
		}
		return ""
	}})
	select {
	case who := <-d.signedIn:
		if who == "sandbox" {
			return
		}
		reviewFinding(t, "F-1", "with `key:` in .starfix.yaml naming the sandbox principal's key and the developer's key in ssh-agent, sfx signed in as "+who+
			": the agent's keys are offered before the configured one (internal/client/conn.go, signers)")
	case <-time.After(10 * time.Second):
		t.Fatal("no key signed in")
	}
}

// F-1 (b): with six other keys in ssh-agent, the configured key is the
// seventh offered, sshd's default MaxAuthTries is six, and the sign-in
// fails with a refusal that points at the host and port.
func TestReviewF1AgentKeysCrowdOutConfiguredKey(t *testing.T) {
	sandboxPriv, sandboxPub := reviewKey(t)
	var others []ed25519.PrivateKey
	for range 6 {
		k, _ := reviewKey(t)
		others = append(others, k)
	}
	d := newReviewSSHD(t, map[string]ssh.PublicKey{"sandbox": sandboxPub}, reviewKeyFile(t, sandboxPriv))
	sock := reviewAgent(t, others...)
	_, err := Dial(context.Background(), d.cfg, Options{Timeout: 5 * time.Second, Getenv: func(k string) string {
		if k == "SSH_AUTH_SOCK" {
			return sock
		}
		return ""
	}})
	select {
	case who := <-d.signedIn:
		t.Logf("signed in as %s", who)
		return
	default:
	}
	pe, ok := errors.AsType[*proto.Error](err)
	if !ok {
		t.Fatalf("Dial = %v, want a typed refusal", err)
	}
	d.mu.Lock()
	n := len(d.offered)
	d.mu.Unlock()
	if !strings.Contains(pe.Fix, "server.host") {
		t.Logf("refusal: %v; fix: %s", pe, pe.Fix)
		return
	}
	reviewFinding(t, "F-1", fmt.Sprintf("the key .starfix.yaml names was never offered (%d keys offered, all ssh-agent's); the refusal is %q with the fix %q",
		n, pe.Message, pe.Fix))
}

// F-1 (c): what a committed .starfix.yaml chooses. It names the server
// and pins its host key, so a cloned repository's file sends every sfx
// command, and a hook set up with --global, to a server of its author's
// choosing; that server is then offered every public key in ssh-agent,
// and the key file the same file names, which may be any path on the
// machine.
func TestReviewF1ClonedConfigIsOfferedEveryKey(t *testing.T) {
	a, _ := reviewKey(t)
	b, _ := reviewKey(t)
	mine, _ := reviewKey(t)
	// The "server" is the repository author's: it accepts nothing and
	// writes down what it is offered.
	d := newReviewSSHD(t, nil, reviewKeyFile(t, mine))
	sock := reviewAgent(t, a, b)
	_, err := Dial(context.Background(), d.cfg, Options{Timeout: 5 * time.Second, Getenv: func(k string) string {
		if k == "SSH_AUTH_SOCK" {
			return sock
		}
		return ""
	}})
	if err == nil {
		t.Fatal("a server that accepts no key let the client in")
	}
	d.mu.Lock()
	n := len(d.offered)
	d.mu.Unlock()
	if n < 3 {
		t.Logf("%d keys offered", n)
		return
	}
	reviewFinding(t, "F-1", fmt.Sprintf("a server named by .starfix.yaml alone was offered %d public keys: both of ssh-agent's and the file `key:` names", n))
}
