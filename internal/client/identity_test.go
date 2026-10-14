package client

// Which keys sfx offers a server (design §14, decision 35; security
// review of stage 3a, F-1). Every key and agent here is made for the
// test: nothing reads the environment's SSH_AUTH_SOCK.

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/ariesworx/starfix/internal/proto"
)

// keyPair is a key made for a test.
type keyPair struct {
	priv ed25519.PrivateKey
	pub  ssh.PublicKey
}

func (k keyPair) fingerprint() string { return ssh.FingerprintSHA256(k.pub) }

func newKeyPair(t *testing.T) keyPair {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	return keyPair{priv, pub}
}

// write writes the private key as dir/name, encrypted when passphrase is
// set, and returns its path. pub also writes name.pub beside it.
func (k keyPair) write(t *testing.T, dir, name, passphrase string, pub bool) string {
	t.Helper()
	var block *pem.Block
	var err error
	if passphrase != "" {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(k.priv, "", []byte(passphrase))
	} else {
		block, err = ssh.MarshalPrivateKey(k.priv, "")
	}
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	if pub {
		k.writePub(t, dir, name+".pub")
	}
	return p
}

func (k keyPair) writePub(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, ssh.MarshalAuthorizedKey(k.pub), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// agentWith runs an in-process ssh-agent holding keys, in order, on a
// socket of its own, and returns the socket.
func agentWith(t *testing.T, keys ...keyPair) string {
	t.Helper()
	kr := agent.NewKeyring()
	for _, k := range keys {
		if err := kr.Add(agent.AddedKey{PrivateKey: k.priv}); err != nil {
			t.Fatal(err)
		}
	}
	dir, err := os.MkdirTemp("", "sfa") // short: a socket's path is limited
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

// fakeSSHD stands in for the server's sshd. It lets in the keys of
// principals, as authorized_keys lines would, writes down every key a
// client offers, in order, and reports on signedIn the principal each
// connection authenticated as, and refuses the session it then asks for.
// Like OpenSSH, it hangs up after six refused keys.
type fakeSSHD struct {
	root     string // the repository: where .starfix.yaml would be
	home     string // the home directory of whoever runs sfx: empty, unless a test fills it
	port     int
	hostKey  string
	signedIn chan string

	mu      sync.Mutex
	offered []string // fingerprints, in the order first offered
}

func newFakeSSHD(t *testing.T, principals map[string]keyPair) *fakeSSHD {
	t.Helper()
	host := newKeyPair(t)
	signer, err := ssh.NewSignerFromKey(host.priv)
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeSSHD{root: t.TempDir(), home: t.TempDir(), hostKey: host.fingerprint(), signedIn: make(chan string, 4)}
	//nolint:gosec // G408: a test stand-in; it records every key offered on purpose
	sc := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		fp := ssh.FingerprintSHA256(key)
		d.mu.Lock()
		if !slices.Contains(d.offered, fp) {
			d.offered = append(d.offered, fp)
		}
		d.mu.Unlock()
		for name, k := range principals {
			if k.fingerprint() == fp {
				return &ssh.Permissions{Extensions: map[string]string{"principal": name}}, nil
			}
		}
		return nil, errors.New("no such key")
	}}
	sc.AddHostKey(signer)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d.port = l.Addr().(*net.TCPAddr).Port
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
				conn, chans, reqs, err := ssh.NewServerConn(nc, sc)
				if err != nil {
					return
				}
				d.signedIn <- conn.Permissions.Extensions["principal"]
				// It runs no starfixd, and says so as sshd would: it refuses
				// the session, and the client hangs up. Closing the
				// connection here instead would leave the client's error to
				// how the close fell against its request.
				wg.Go(func() { ssh.DiscardRequests(reqs) })
				for ch := range chans {
					_ = ch.Reject(ssh.Prohibited, "no starfixd here")
				}
			})
		}
	})
	return d
}

// dial runs Dial against d as a checkout whose .starfix.yaml has key as
// its `key:`, with env as the environment. The stand-in runs no
// starfixd, so a Dial that authenticates still fails afterward: who is
// the principal sshd took the connection for, or "" when it let none in.
func (d *fakeSSHD) dial(t *testing.T, key string, env map[string]string) (who string, offered []string, err error) {
	t.Helper()
	cfg := &Config{Project: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Key: key, Root: d.root}
	cfg.Server.Host, cfg.Server.Port, cfg.Server.User, cfg.Server.HostKey = "127.0.0.1", d.port, "starfix", d.hostKey
	d.mu.Lock()
	d.offered = nil
	d.mu.Unlock()
	c, err := Dial(t.Context(), cfg, Options{Timeout: 10 * time.Second, Getenv: func(k string) string { return env[k] },
		UserHomeDir: func() (string, error) { return d.home, nil }})
	if err == nil {
		_ = c.Close()
		t.Fatal("Dial succeeded against a stand-in that runs no starfixd")
	}
	select {
	case who = <-d.signedIn:
	default:
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return who, slices.Clone(d.offered), err
}

// TestConfiguredKeyIsTheOnlyKeyOffered is the review's scenario: the
// config names the key of a narrow principal, sandbox, and ssh-agent
// holds the developer's own, a server admin's, which the server also
// knows. sfx once offered the agent's keys first and signed in as the
// admin.
func TestConfiguredKeyIsTheOnlyKeyOffered(t *testing.T) {
	t.Parallel()
	admin, sandbox := newKeyPair(t), newKeyPair(t)
	d := newFakeSSHD(t, map[string]keyPair{"dana-the-admin": admin, "sandbox": sandbox})
	sandbox.write(t, d.root, "id_sandbox", "", false)
	who, offered, _ := d.dial(t, "id_sandbox", map[string]string{"SSH_AUTH_SOCK": agentWith(t, admin)})
	if who != "sandbox" {
		t.Errorf("with key: naming sandbox's key and the admin's key in ssh-agent, sfx signed in as %q, want sandbox", who)
	}
	if !slices.Equal(offered, []string{sandbox.fingerprint()}) {
		t.Errorf("sfx offered %v, want the configured key alone, %s", offered, sandbox.fingerprint())
	}
}

// TestWhichKeysAreOffered pins, for each way a key can be configured,
// which keys reach the server and who signs.
func TestWhichKeysAreOffered(t *testing.T) {
	t.Parallel()
	mine, other, third := newKeyPair(t), newKeyPair(t), newKeyPair(t)
	var crowd []keyPair
	for range 6 { // with OpenSSH's six tries, a seventh key is never tried
		crowd = append(crowd, newKeyPair(t))
	}
	d := newFakeSSHD(t, map[string]keyPair{"me": mine, "other": other})
	outside := t.TempDir() // not the repository
	mine.write(t, d.root, "plain", "", false)
	mine.write(t, d.root, "locked", "secret", true)
	mine.write(t, d.root, "locked-alone", "secret", false) // the file's own header has the public half
	mine.writePub(t, d.root, "only.pub")                   // the private half is elsewhere, or in a token
	third.write(t, d.root, "unknown", "", false)
	envKey := mine.write(t, outside, "id_env", "", false)
	envPub := mine.writePub(t, outside, "id_agent.pub")
	both := agentWith(t, other, mine)

	tests := []struct {
		name    string
		key     string            // key: in .starfix.yaml
		env     map[string]string // the environment
		who     string            // the principal signed in; "" is refused
		offered []keyPair
	}{
		{name: "no key configured: ssh-agent's keys, in its order", env: map[string]string{"SSH_AUTH_SOCK": both},
			who: "other", offered: []keyPair{other}},
		{name: "a key file, with other keys in ssh-agent", key: "plain", env: map[string]string{"SSH_AUTH_SOCK": both},
			who: "me", offered: []keyPair{mine}},
		{name: "a key file, with no agent", key: "plain", who: "me", offered: []keyPair{mine}},
		{name: "a key file, behind six other keys in ssh-agent", key: "plain", env: map[string]string{"SSH_AUTH_SOCK": agentWith(t, crowd...)},
			who: "me", offered: []keyPair{mine}},
		{name: "a passphrase-protected file: ssh-agent signs with that key alone", key: "locked", env: map[string]string{"SSH_AUTH_SOCK": both},
			who: "me", offered: []keyPair{mine}},
		{name: "the same, with no .pub beside it", key: "locked-alone", env: map[string]string{"SSH_AUTH_SOCK": both},
			who: "me", offered: []keyPair{mine}},
		{name: "a .pub file: ssh-agent signs with that key alone", key: "only.pub", env: map[string]string{"SSH_AUTH_SOCK": both},
			who: "me", offered: []keyPair{mine}},
		{name: "a private key that is missing, with its .pub there", key: "only", env: map[string]string{"SSH_AUTH_SOCK": both},
			who: "me", offered: []keyPair{mine}},
		{name: "STARFIX_KEY, a file anywhere, wins over key:", key: "unknown", env: map[string]string{EnvKeyFile: envKey, "SSH_AUTH_SOCK": both},
			who: "me", offered: []keyPair{mine}},
		{name: "STARFIX_KEY naming a .pub", env: map[string]string{EnvKeyFile: envPub, "SSH_AUTH_SOCK": both},
			who: "me", offered: []keyPair{mine}},
		{name: "a configured key the server does not know, with one it knows in ssh-agent", key: "unknown", env: map[string]string{"SSH_AUTH_SOCK": both},
			who: "", offered: []keyPair{third}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			who, offered, err := d.dial(t, tc.key, tc.env)
			var want []string
			for _, k := range tc.offered {
				want = append(want, k.fingerprint())
			}
			if who != tc.who || !slices.Equal(offered, want) {
				t.Errorf("signed in as %q having offered %v\nwant %q having offered %v\n(Dial: %v)", who, offered, tc.who, want, err)
			}
		})
	}
}

// TestKeyRefusals pins what sfx says when it has no key to offer, or the
// server takes none: each refusal names the cause and the next action,
// and names keys by fingerprint only.
func TestKeyRefusals(t *testing.T) {
	t.Parallel()
	mine, stranger := newKeyPair(t), newKeyPair(t)
	var crowd []keyPair
	for range 7 {
		crowd = append(crowd, newKeyPair(t))
	}
	d := newFakeSSHD(t, map[string]keyPair{"me": mine})
	outside := t.TempDir()
	stranger.write(t, d.root, "unknown", "", false)
	mine.write(t, d.root, "locked", "secret", true)
	mine.writePub(t, d.root, "only.pub")
	if err := os.WriteFile(filepath.Join(d.root, "garbage"), []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	elsewhere := mine.write(t, outside, "id_mine", "", false)
	if err := os.Symlink(elsewhere, filepath.Join(d.root, "link")); err != nil {
		t.Fatal(err)
	}
	none := agentWith(t, stranger)
	many := agentWith(t, append(slices.Clone(crowd), mine)...) // the server knows the eighth

	tests := []struct {
		name    string
		key     string
		env     map[string]string
		code    proto.Code
		msg     []string // each is in the message
		fix     []string // each is in the fix
		offered int      // keys that reached the server
	}{
		{name: "the configured key is not one the server knows", key: "unknown", env: map[string]string{"SSH_AUTH_SOCK": agentWith(t, mine)},
			code: proto.CodeAuth, msg: []string{"accepted no key", stranger.fingerprint(), "key: in .starfix.yaml"},
			fix: []string{"sfx admin key add", "offers no other"}, offered: 1},
		{name: "none of ssh-agent's keys is known", env: map[string]string{"SSH_AUTH_SOCK": none},
			code: proto.CodeAuth, msg: []string{"accepted no key", "the one key it found", stranger.fingerprint() + " (ssh-agent)"},
			fix: []string{"ssh-add -L", "sfx admin key add", EnvKeyFile}, offered: 1},
		{name: "ssh-agent holds more keys than sshd tries", env: map[string]string{"SSH_AUTH_SOCK": many},
			code: proto.CodeAuth, msg: []string{"accepted none of the 5 keys sfx offered", crowd[0].fingerprint(), "3 more keys that ssh-agent holds were not offered"},
			fix: []string{EnvKeyFile, "ssh-add -L"}, offered: 5},
		{name: "no key and no agent", code: proto.CodeAuth, msg: []string{"no SSH key available"}, fix: []string{"ssh-add", EnvKeyFile}},
		{name: "a passphrase-protected key that ssh-agent does not hold", key: "locked", env: map[string]string{"SSH_AUTH_SOCK": none},
			code: proto.CodeAuth, msg: []string{"passphrase-protected", "ssh-agent does not hold it", mine.fingerprint()}, fix: []string{"ssh-add " + filepath.Join(d.root, "locked")}},
		{name: "a passphrase-protected key and no agent", key: "locked",
			code: proto.CodeAuth, msg: []string{"passphrase-protected", "ssh-agent does not hold it"}, fix: []string{"ssh-add"}},
		{name: "a .pub whose key ssh-agent does not hold", key: "only.pub", env: map[string]string{"SSH_AUTH_SOCK": none},
			code: proto.CodeAuth, msg: []string{"only the public half", "ssh-agent does not hold it"}, fix: []string{"ssh-add"}},
		{name: "a key file that is not there", key: "missing", code: proto.CodeAuth, msg: []string{"cannot read key file"}, fix: []string{"key:", EnvKeyFile}},
		{name: "a key path that is a directory", key: ".", code: proto.CodeAuth, msg: []string{"cannot read key file"}, fix: []string{"key:"}},
		{name: "a file that is no key", key: "garbage", code: proto.CodeAuth, msg: []string{"cannot parse key file"}, fix: []string{"OpenSSH"}},
		{name: "STARFIX_KEY naming a file that is not there", env: map[string]string{EnvKeyFile: filepath.Join(outside, "nope")},
			code: proto.CodeAuth, msg: []string{"cannot read key file"}, fix: []string{EnvKeyFile}},
		// What a committed file may name: a file inside the repository.
		{name: "key: with an absolute path", key: elsewhere, env: map[string]string{"SSH_AUTH_SOCK": none},
			code: proto.CodeInvalid, msg: []string{"outside the repository"}, fix: []string{EnvKeyFile + "=" + elsewhere}},
		{name: "key: under the home directory", key: "~/.ssh/id_ed25519", env: map[string]string{"SSH_AUTH_SOCK": none},
			code: proto.CodeInvalid, msg: []string{"outside the repository"}, fix: []string{EnvKeyFile + "=~/.ssh/id_ed25519"}},
		{name: "key: climbing out of the repository", key: "../" + filepath.Base(outside) + "/id_mine",
			code: proto.CodeInvalid, msg: []string{"outside the repository"}, fix: []string{EnvKeyFile}},
		{name: "key: naming a link that leads out of the repository", key: "link",
			code: proto.CodeInvalid, msg: []string{"outside the repository"}, fix: []string{EnvKeyFile}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			who, offered, err := d.dial(t, tc.key, tc.env)
			pe, ok := errors.AsType[*proto.Error](err)
			if !ok {
				t.Fatalf("Dial = %v, want a *proto.Error", err)
			}
			if who != "" || pe.Code != tc.code || len(offered) != tc.offered {
				t.Errorf("signed in as %q, offered %d keys, refused with %s\nwant no sign-in, %d keys offered and %s\n%v\nfix: %s",
					who, len(offered), pe.Code, tc.offered, tc.code, pe.Message, pe.Fix)
			}
			for _, want := range tc.msg {
				if !strings.Contains(pe.Message, want) {
					t.Errorf("message %q lacks %q", pe.Message, want)
				}
			}
			for _, want := range tc.fix {
				if !strings.Contains(pe.Fix, want) {
					t.Errorf("fix %q lacks %q", pe.Fix, want)
				}
			}
			if said := pe.Message + pe.Fix; strings.Contains(said, "PRIVATE KEY") || strings.Contains(said, "server.host") {
				t.Errorf("the refusal shows key material, or blames the address: %v\nfix: %s", pe.Message, pe.Fix)
			}
		})
	}
}

// TestKeyPathStaysInTheRepository pins the rule for a committed key:
// where the path may lead, whatever it looks like.
func TestKeyPathStaysInTheRepository(t *testing.T) {
	t.Parallel()
	root, outside := t.TempDir(), t.TempDir()
	for _, dir := range []string{filepath.Join(root, "keys"), filepath.Join(outside, "keys")} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(outside, "keys"), filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "keys"), filepath.Join(root, "inside")); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		key  string
		want string // the path, relative to root; "" is refused
	}{
		{"keys/agent", "keys/agent"},
		{"./keys/agent", "keys/agent"},
		{"keys/../keys/agent", "keys/agent"},
		{"not/there/yet", "not/there/yet"},
		{"inside/agent", "inside/agent"}, // a link that stays inside
		{"../agent", ""},
		{"keys/../../agent", ""},
		{"linked/agent", ""}, // a directory link that leads out
		{filepath.Join(outside, "keys", "agent"), ""},
		{filepath.Join(root, "keys", "agent"), ""}, // absolute, even to the inside: it names this machine
		{"~/.ssh/id_ed25519", ""},
		{"~", ""},
	}
	for _, tc := range tests {
		c := &Config{Key: tc.key, Root: root}
		got, err := c.KeyPath()
		if tc.want == "" {
			if pe, ok := errors.AsType[*proto.Error](err); !ok || pe.Code != proto.CodeInvalid || !strings.Contains(pe.Fix, EnvKeyFile) {
				t.Errorf("KeyPath(%q) = %q, %v; want it refused as invalid, with a fix naming %s", tc.key, got, err, EnvKeyFile)
			}
			continue
		}
		if want := filepath.Join(root, tc.want); err != nil || got != want {
			t.Errorf("KeyPath(%q) = %q, %v; want %q", tc.key, got, err, want)
		}
	}
	if got, err := (&Config{Root: root}).KeyPath(); got != "" || err != nil {
		t.Errorf("KeyPath with no key = %q, %v; want none", got, err)
	}
}

// TestKeyOfAConnection pins what a connection says of the key that
// signed it in, for `sfx whoami`.
func TestKeyOfAConnection(t *testing.T) {
	t.Parallel()
	mine, other := newKeyPair(t), newKeyPair(t)
	dir := t.TempDir()
	file := mine.write(t, dir, "id", "", false)
	pub := mine.writePub(t, dir, "agent.pub")
	sock := agentWith(t, other, mine)
	tests := []struct {
		name string
		env  map[string]string
		want KeyInfo
	}{
		{"a key file", map[string]string{EnvKeyFile: file, "SSH_AUTH_SOCK": sock}, KeyInfo{Fingerprint: mine.fingerprint(), Source: "file", Path: file, By: EnvKeyFile}},
		{"ssh-agent, for a configured key", map[string]string{EnvKeyFile: pub, "SSH_AUTH_SOCK": sock}, KeyInfo{Fingerprint: mine.fingerprint(), Source: "agent", Path: pub, By: EnvKeyFile}},
		{"ssh-agent, the second of its keys", map[string]string{"SSH_AUTH_SOCK": sock}, KeyInfo{Fingerprint: mine.fingerprint(), Source: "agent", By: "ssh-agent"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id, err := loadIdentity(&Config{Root: dir}, func(k string) string { return tc.env[k] }, func() (string, error) { return t.TempDir(), nil })
			if err != nil {
				t.Fatal(err)
			}
			defer id.close()
			// The server asks for a signature only from a key it accepts:
			// here, mine.
			for _, s := range id.signers {
				if ssh.FingerprintSHA256(s.PublicKey()) != mine.fingerprint() {
					continue
				}
				if _, err := s.Sign(rand.Reader, []byte("session")); err != nil {
					t.Fatal(err)
				}
			}
			if got := id.used(); got != tc.want {
				t.Errorf("the key that signed = %+v, want %+v", got, tc.want)
			}
		})
	}
	if got := fmt.Sprint((&identity{}).used()); got != fmt.Sprint(KeyInfo{}) {
		t.Errorf("the key of an identity that signed nothing = %s, want none", got)
	}
}
