package client

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/safetext"
)

// Options describe this client to the server.
type Options struct {
	// Version is the client's build version.
	Version string
	// Session is the client session id; empty lets the server assign one.
	Session string
	// Machine is this machine's name.
	Machine string
	// Harness names the agent harness, or is empty (HarnessFromEnv).
	Harness string
	// Trace is the caller's W3C trace context, sent on every request of
	// the connection whose call brings none of its own ([WithTrace]), or
	// the zero value for none (TraceFromEnv).
	Trace proto.TraceContext
	// Getenv reads SSH_AUTH_SOCK and EnvKeyFile. Default os.Getenv.
	Getenv func(string) string
	// UserHomeDir finds the home directory, whose .ssh holds the default
	// identities and which "~/" in EnvKeyFile means. Default
	// os.UserHomeDir, which reads HOME, or USERPROFILE on Windows: the
	// process's environment, which no file of a repository sets.
	UserHomeDir func() (string, error)
	// Timeout bounds connecting and the handshake. Default 15s.
	Timeout time.Duration
	// OnPush receives the events the server pushes (after a watch call),
	// in order, on the connection's reader goroutine. It must return
	// quickly and must not call the Conn: the next response waits for it.
	OnPush func(proto.Push)
}

// Conn is one session with starfixd. Calls are serialized. A reader
// goroutine reads every frame the server sends: responses go to the call
// waiting for them, pushed events to Options.OnPush, and frame types it
// does not know are skipped.
type Conn struct {
	// Welcome is the server's handshake reply.
	Welcome proto.Frame
	// key is the key that signed in (Key), and skipped the default
	// identity files that could not be used (SkippedKeys).
	key     KeyInfo
	skipped []string

	ssh    *ssh.Client
	sess   *ssh.Session
	stdin  io.WriteCloser
	enc    *proto.Encoder
	dec    *proto.Decoder
	stderr *capBuffer
	// kill closes the transport, ending the reader and any call.
	kill   func() error
	onPush func(proto.Push)
	// trace goes on every request whose call has none of its own
	// (Options.Trace).
	trace proto.TraceContext

	res     chan *proto.Frame // responses, from the reader to Call
	done    chan struct{}     // closed when the reader stops
	readErr error             // why the reader stopped; set before done closes
	closed  chan struct{}     // closed by Close
	once    sync.Once

	mu     sync.Mutex // held for each Call, which serializes them; guards nextID and broken
	nextID uint64
	broken error // why the Conn can no longer be used; nil while it can
}

// newConn returns a Conn over enc and dec, before its handshake.
func newConn(enc *proto.Encoder, dec *proto.Decoder, kill func() error, onPush func(proto.Push)) *Conn {
	return &Conn{enc: enc, dec: dec, kill: kill, onPush: onPush, stderr: &capBuffer{max: 4096},
		res: make(chan *proto.Frame, 1), done: make(chan struct{}), closed: make(chan struct{})}
}

// run starts the reader. Before it, the handshake reads frames itself.
func (c *Conn) run() { go c.read() }

// read delivers frames until the stream ends.
func (c *Conn) read() {
	defer close(c.done)
	for {
		f, err := c.dec.Decode()
		if err != nil {
			c.readErr = err
			return
		}
		switch f.T {
		case proto.FrameRes:
			select {
			case c.res <- f:
			case <-c.closed:
				return
			}
		case proto.FrameEvent:
			if p, err := proto.DecodePush(f); err == nil && c.onPush != nil {
				c.onPush(p)
			}
		}
	}
}

// Done is closed once the connection has ended, by Close or by the
// server or network.
func (c *Conn) Done() <-chan struct{} { return c.done }

// RemoteCommand is what the client asks sshd to run. The key's forced
// command overrides it anyway.
const RemoteCommand = "starfixd stdio"

// Dial connects to the server named in cfg, verifies its pinned host key,
// authenticates, and completes the version handshake, all within
// opts.Timeout. A refusal comes back as a *proto.Error. The caller must
// Close the Conn.
func Dial(ctx context.Context, cfg *Config, opts Options) (*Conn, error) {
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	if opts.UserHomeDir == nil {
		opts.UserHomeDir = os.UserHomeDir
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	id, err := loadIdentity(cfg, opts.Getenv, opts.UserHomeDir)
	if err != nil {
		return nil, err
	}
	defer id.close()

	addr := net.JoinHostPort(cfg.Server.Host, strconv.Itoa(cfg.Server.Port))
	var hostKeyErr *proto.Error
	sc := &ssh.ClientConfig{
		User:              cfg.Server.User,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(id.signers...)},
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if got := ssh.FingerprintSHA256(key); got != cfg.Server.HostKey {
				hostKeyErr = proto.Errf(proto.CodeAuth,
					"if the server was rebuilt, confirm its new key with the admin out of band, then update server.host_key; otherwise do not connect",
					fmt.Sprintf("host key mismatch for %s: the server presented %s, but %s pins %s", cfg.Server.Host, got, ConfigFile, cfg.Server.HostKey))
				return hostKeyErr
			}
			return nil
		},
		Timeout: opts.Timeout,
	}
	nc, err := dialTransport(ctx, cfg, addr)
	if err != nil {
		return nil, err
	}
	// Closing the transport bounds the SSH handshake for both transports;
	// the IAP tunnel's pipes have no portable deadlines.
	stop := context.AfterFunc(ctx, func() { _ = nc.Close() })
	defer stop()
	cc, chans, reqs, err := ssh.NewClientConn(nc, addr, sc)
	if err != nil {
		_ = nc.Close()
		if hostKeyErr != nil {
			return nil, hostKeyErr
		}
		if e := id.refusedKeys(addr, err); e != nil {
			return nil, e
		}
		var timeout time.Duration
		if ctx.Err() != nil {
			timeout = opts.Timeout
		}
		if t, ok := nc.(*iapConn); ok {
			return nil, t.failed(err, timeout)
		}
		if timeout > 0 {
			return nil, proto.Errf(proto.CodeUnavailable, "check server.host and server.port in "+ConfigFile+", and your network",
				fmt.Sprintf("timed out after %s in the ssh handshake with %s", timeout, addr))
		}
		return nil, proto.Errf(proto.CodeUnavailable, "check server.host and server.port in "+ConfigFile,
			fmt.Sprintf("ssh handshake with %s failed: %v", addr, err))
	}
	client := ssh.NewClient(cc, chans, reqs)
	c, err := start(ctx, client, cfg, opts)
	if err != nil {
		return nil, errors.Join(err, client.Close())
	}
	c.key, c.skipped = id.used(), id.skipped
	return c, nil
}

// Key says which key signed this connection in.
func (c *Conn) Key() KeyInfo { return c.key }

// SkippedKeys says which default identity files sfx found and could not
// use for this connection, and why each.
func (c *Conn) SkippedKeys() []string { return c.skipped }

// dialTransport opens the byte stream SSH runs over: a TCP connection to
// addr, or, when cfg sets server.iap, a Google Cloud IAP tunnel.
func dialTransport(ctx context.Context, cfg *Config, addr string) (net.Conn, error) {
	if cfg.Server.IAP != nil {
		return dialIAP(cfg)
	}
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, proto.Errf(proto.CodeUnavailable, "check server.host and server.port in "+ConfigFile+", and your network",
			fmt.Sprintf("cannot reach %s: %v", addr, err))
	}
	return nc, nil
}

// start opens the SSH session that runs the bridge and completes the
// handshake on it, within ctx. On success the Conn's reader is running;
// on failure the caller closes client.
func start(ctx context.Context, client *ssh.Client, cfg *Config, opts Options) (*Conn, error) {
	sess, err := client.NewSession()
	if err != nil {
		return nil, proto.Errf(proto.CodeUnavailable, "retry; if it persists, ask the server admin", fmt.Sprintf("open ssh session: %v", err))
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("ssh stdin: %w", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ssh stdout: %w", err)
	}
	c := newConn(proto.NewEncoder(stdin), proto.NewDecoder(stdout), client.Close, opts.OnPush)
	c.ssh, c.sess, c.stdin, c.trace = client, sess, stdin, opts.Trace
	sess.Stderr = c.stderr
	if err := sess.Start(RemoteCommand); err != nil {
		return nil, proto.Errf(proto.CodeUnavailable, "check that this key's authorized_keys line forces `starfixd stdio --principal NAME`",
			fmt.Sprintf("the server would not start starfixd: %v", err))
	}
	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stop()
	if err := c.enc.Encode(&proto.Frame{T: proto.FrameHello, Version: opts.Version, Proto: proto.Proto,
		Project: cfg.Project, Session: opts.Session, Machine: opts.Machine, Harness: opts.Harness}); err != nil {
		// The bridge may have refused and closed before reading the hello;
		// its refusal is still waiting to be read.
		if w, rerr := c.next(); rerr == nil && w.T == proto.FrameWelcome && w.Err != nil {
			return nil, w.Err
		}
		return nil, c.lost(err)
	}
	w, err := c.next()
	if err != nil {
		return nil, c.lost(err)
	}
	if w.T != proto.FrameWelcome {
		return nil, proto.Errf(proto.CodeVersion, "upgrade starfix to match the server",
			fmt.Sprintf("expected a welcome from the server, got a %q frame", w.T))
	}
	if w.Err != nil {
		return nil, w.Err
	}
	if e := proto.CheckProto(proto.Proto, w.Min, w.Max, w.Version); e != nil {
		return nil, e
	}
	c.Welcome = *w
	c.run()
	return c, nil
}

// next reads, during the handshake, the next frame the client
// understands, skipping event frames and frame types added by newer
// servers.
func (c *Conn) next() (*proto.Frame, error) {
	for {
		f, err := c.dec.Decode()
		if err != nil {
			return nil, err
		}
		switch f.T {
		case proto.FrameWelcome, proto.FrameRes:
			return f, nil
		}
	}
}

// lost explains a connection that ended unexpectedly, quoting what the
// server printed on stderr.
func (c *Conn) lost(err error) *proto.Error {
	msg := "the server closed the connection"
	if !errors.Is(err, io.EOF) {
		msg = fmt.Sprintf("connection to the server failed: %v", err)
	}
	msg += quoteStderr(c.stderr.String(), "server")
	return proto.Errf(proto.CodeUnavailable,
		"retry; if it persists, check that this key's authorized_keys line forces `starfixd stdio --principal NAME` and that starfixd is running", msg)
}

// Session is the session id the server recorded for this connection.
func (c *Conn) Session() string { return c.Welcome.Session }

// Principal is who the server says this key belongs to. It is empty with a
// server older than the welcome's principal field.
func (c *Conn) Principal() string { return c.Welcome.Principal }

// Err reports why the connection can no longer be used, or nil while it
// can. A refused request does not break the connection; a lost one does,
// even while no call is running.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.broken == nil {
		select {
		case <-c.done:
			c.broken = c.lost(c.readErr)
		default:
		}
	}
	return c.broken
}

// Notices returns the one-line version notices for an agent or person: this
// client older than the server, and the server older than the latest
// release it knows of.
func (c *Conn) Notices(clientVersion string) []string {
	var out []string
	for _, n := range []string{
		proto.OlderClientWarning(clientVersion, c.Welcome.Version),
		proto.OlderServerWarning(c.Welcome.Version, c.Welcome.Latest),
	} {
		if n != "" {
			out = append(out, n)
		}
	}
	return out
}

// Warning returns a one-line notice for the person when this client is
// older than the server, or "".
func (c *Conn) Warning(clientVersion string) string {
	return proto.OlderClientWarning(clientVersion, c.Welcome.Version)
}

// Call sends one request and decodes its result into result (which may be
// nil). A server refusal is returned as a *proto.Error and leaves the
// Conn usable. Calls are serialized. Cancelling ctx closes the connection.
// A cancelled or lost connection, or an error frame that answers no
// request (as when the server closes an idle connection), breaks the Conn:
// later calls fail at once, and Err says why.
func (c *Conn) Call(ctx context.Context, op string, args, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.broken != nil {
		return c.broken
	}
	c.nextID++
	id := c.nextID
	f, err := proto.Request(id, op, args)
	if err != nil {
		return err
	}
	// The call's own trace context, when its caller gave one, and the
	// connection's otherwise.
	tc := c.trace
	if own, ok := TraceFrom(ctx); ok {
		tc = own
	}
	f.Traceparent, f.Tracestate = tc.Traceparent(), tc.State
	stop := context.AfterFunc(ctx, func() { _ = c.kill() })
	defer stop()
	if err := c.enc.Encode(f); err != nil {
		c.broken = c.lost(err)
		return c.broken
	}
	for {
		var r *proto.Frame
		select {
		case r = <-c.res:
		case <-c.done:
			// The reader may have queued this call's answer just before
			// the stream ended.
			select {
			case r = <-c.res:
			default:
				if ctx.Err() != nil {
					c.broken = fmt.Errorf("%s: %w", op, ctx.Err())
				} else {
					c.broken = c.lost(c.readErr)
				}
				return c.broken
			}
		}
		if r.ID != id && r.ID != 0 {
			continue // an answer to an earlier, abandoned call
		}
		if r.Err != nil {
			if r.ID == 0 {
				c.broken = r.Err
			}
			return r.Err
		}
		if result == nil {
			return nil
		}
		if err := json.Unmarshal(r.OK, result); err != nil {
			return fmt.Errorf("decode %s result: %w", op, err)
		}
		return nil
	}
}

// Close ends the session and closes the connection, which ends a call in
// progress and the reader. It may be called more than once.
func (c *Conn) Close() error {
	c.once.Do(func() { close(c.closed) })
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if c.sess != nil {
		_ = c.sess.Close()
	}
	if err := c.kill(); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) {
		return fmt.Errorf("close: %w", err)
	}
	return nil
}

// KeyInfo says which key signed a connection in: its SHA-256
// fingerprint, whether a key file signed ("file") or ssh-agent did
// ("agent"), the file that named the key, when one did, and what chose
// it: EnvKeyFile, key: in the config file, "default identity" for a file
// of ~/.ssh with one of ssh's default names, or "ssh-agent".
type KeyInfo struct {
	Fingerprint string `json:"fingerprint"`
	Source      string `json:"source"`
	Path        string `json:"path,omitempty"`
	By          string `json:"chosen_by,omitempty"`
}

// Configured reports whether the key is the one EnvKeyFile or key: in
// the config file names, and so was the only key offered, rather than
// one found among the default identities and ssh-agent's keys.
func (k KeyInfo) Configured() bool { return k.By != "" && k.By != byDefault && k.By != byAgent }

// Ways a key is chosen when none is configured (KeyInfo.By).
const (
	byDefault = "default identity"
	byAgent   = "ssh-agent"
)

// maxOffered is the most keys sfx offers a server when none is
// configured. sshd counts every key it refuses and hangs up at
// MaxAuthTries, six by default, so five leaves the server able to
// answer; a refusal then says which keys were left out.
const maxOffered = 5

// defaultIdentities are the files of ~/.ssh that sfx tries when no key
// is configured, in the order of ssh_config(5)'s default for
// IdentityFile. The _sk names are keys held on a hardware token: their
// files hold a handle, not a key sfx can sign with, so they are used
// only through ssh-agent. A certificate beside a key (NAME-cert.pub) is
// not read: certificates are not supported yet (design §17), and
// nothing here stands in their way.
var defaultIdentities = []string{"id_rsa", "id_ecdsa", "id_ecdsa_sk", "id_ed25519", "id_ed25519_sk"}

// identity is the keys a connection offers the server, in order, and
// which of them signed.
type identity struct {
	signers []ssh.Signer
	keys    []KeyInfo // what each signer is, by index
	// from names what configured the one key (EnvKeyFile, or key: in the
	// config file), and is empty when none is configured.
	from string
	// skipped says, for each default identity file that could not be
	// used, which file and why; left holds the keys found and not
	// offered, past maxOffered.
	skipped []string
	left    []KeyInfo
	// notRead says the variable sfx never reads (EnvNotRead) is set.
	notRead bool
	// signed is the index of the signer that last signed, or -1. The
	// server asks for a signature only from a key it accepts.
	signed atomic.Int32
	// closeAgent closes the agent connection, if one is open.
	closeAgent func()
}

// close ends the agent connection. The caller calls it once the SSH
// handshake is done.
func (id *identity) close() {
	if id.closeAgent != nil {
		id.closeAgent()
	}
}

// used returns the key that signed, or the zero KeyInfo when none did.
func (id *identity) used() KeyInfo {
	if i := int(id.signed.Load()); i >= 0 && i < len(id.keys) {
		return id.keys[i]
	}
	return KeyInfo{}
}

// add appends a key to offer.
func (id *identity) add(s ssh.Signer, source, path, by string) {
	i := int32(len(id.signers)) //nolint:gosec // a handful of keys
	id.signers = append(id.signers, noteSigner(s, func() { id.signed.Store(i) }))
	id.keys = append(id.keys, KeyInfo{Fingerprint: ssh.FingerprintSHA256(s.PublicKey()), Source: source, Path: path, By: by})
}

// has reports whether pub is among the keys to offer.
func (id *identity) has(pub ssh.PublicKey) bool {
	return slices.ContainsFunc(id.signers, func(s ssh.Signer) bool { return bytes.Equal(s.PublicKey().Marshal(), pub.Marshal()) })
}

// offered lists the keys offered, each with where it came from.
func (id *identity) offered() string {
	var out []string
	for _, k := range id.keys {
		out = append(out, k.Fingerprint+" ("+cmp.Or(k.Path, byAgent)+")")
	}
	return strings.Join(out, ", ")
}

// noting is a signer that calls note before it signs. The three forms
// keep what the wrapped signer can do: ssh picks the signature algorithm
// by which of these interfaces a signer has.
type (
	noting struct {
		ssh.Signer
		note func()
	}
	notingAlgorithm struct {
		ssh.AlgorithmSigner
		note func()
	}
	notingMulti struct {
		ssh.MultiAlgorithmSigner
		note func()
	}
)

func (n noting) Sign(rand io.Reader, data []byte) (*ssh.Signature, error) {
	n.note()
	return n.Signer.Sign(rand, data)
}

func (n notingAlgorithm) Sign(rand io.Reader, data []byte) (*ssh.Signature, error) {
	n.note()
	return n.AlgorithmSigner.Sign(rand, data)
}

func (n notingAlgorithm) SignWithAlgorithm(rand io.Reader, data []byte, algorithm string) (*ssh.Signature, error) {
	n.note()
	return n.AlgorithmSigner.SignWithAlgorithm(rand, data, algorithm)
}

func (n notingMulti) Sign(rand io.Reader, data []byte) (*ssh.Signature, error) {
	n.note()
	return n.MultiAlgorithmSigner.Sign(rand, data)
}

func (n notingMulti) SignWithAlgorithm(rand io.Reader, data []byte, algorithm string) (*ssh.Signature, error) {
	n.note()
	return n.MultiAlgorithmSigner.SignWithAlgorithm(rand, data, algorithm)
}

// noteSigner wraps s so that note runs whenever it signs.
func noteSigner(s ssh.Signer, note func()) ssh.Signer {
	switch s := s.(type) {
	case ssh.MultiAlgorithmSigner:
		return notingMulti{s, note}
	case ssh.AlgorithmSigner:
		return notingAlgorithm{s, note}
	}
	return noting{s, note}
}

// loadIdentity decides which keys to offer (design §14, decision 35).
//
// A configured key (Config.KeyFile) is the only key offered, as with
// OpenSSH's IdentitiesOnly: whatever ssh-agent holds, the server is
// shown that key and no other, so a process given a narrow principal's
// key cannot sign in as whoever's agent socket it inherited. The file
// signs when it is a private key sfx can read. ssh-agent signs for that
// one key, found by its public half, when the file is
// passphrase-protected, when it is the public key (a .pub file), and
// when the private key is missing and its .pub is there, which is how
// OpenSSH treats an IdentityFile whose key is in the agent.
//
// With no key configured, sfx does as ssh does: the default identity
// files of ~/.ssh (defaultIdentities), in order, then the keys
// ssh-agent holds that are not among them, each key once, and
// maxOffered at most. A default file that needs a passphrase, or a
// hardware token, is used through the agent when the agent holds its
// key, and is otherwise skipped, with the reason kept for whoami and
// for a refusal. Nothing is exclusive here, so the first key the server
// knows decides who the connection is: which is why whoami and prime
// say which key that was.
//
// home finds the home directory. EnvNotRead is never read: only
// whether it is set is noted, for a refusal.
func loadIdentity(cfg *Config, getenv func(string) string, home func() (string, error)) (*identity, error) {
	path, from, err := cfg.KeyFile(getenv, home)
	if err != nil {
		return nil, err
	}
	id := &identity{notRead: getenv(EnvNotRead) != ""}
	id.signed.Store(-1)
	if path == "" {
		return id.unconfigured(getenv, home)
	}
	id.from = from
	signer, pub, why, perr := readKeyFile(path, from)
	if perr != nil {
		return nil, id.noted(perr)
	}
	if signer != nil {
		id.add(signer, "file", path, from)
		return id, nil
	}
	// Only the key's public half is to hand: the agent signs, with that
	// key alone.
	missing := id.noted(proto.Errf(proto.CodeAuth, "add it to ssh-agent: `ssh-add "+strings.TrimSuffix(path, ".pub")+"`",
		fmt.Sprintf("key file %s %s, and ssh-agent does not hold it (%s)", path, why, ssh.FingerprintSHA256(pub))))
	for _, s := range id.agentSigners(getenv) {
		if bytes.Equal(s.PublicKey().Marshal(), pub.Marshal()) {
			id.add(s, "agent", path, from)
			return id, nil
		}
	}
	id.close()
	return nil, missing
}

// agentSigners returns the keys ssh-agent holds, in its order, and
// keeps the connection for signing; none when there is no agent, or it
// cannot list.
func (id *identity) agentSigners(getenv func(string) string) []ssh.Signer {
	ac, err := dialAgent(getenv)
	if err != nil {
		return nil
	}
	id.closeAgent = func() { _ = ac.Close() }
	ss, _ := agent.NewClient(ac).Signers() // an agent that cannot list holds nothing to sign with
	return ss
}

// noted adds to a refusal that EnvNotRead is set, when it is: someone
// who set it meant sfx to use a key, and should learn that it did not.
// The variable's value is never read, so it cannot be repeated.
func (id *identity) noted(e *proto.Error) *proto.Error {
	if !id.notRead {
		return e
	}
	return proto.Errf(e.Code, e.Fix, fmt.Sprintf("%s; %s is set, and sfx does not read it: %s names a key file, by its path", e.Message, EnvNotRead, EnvKeyFile))
}

// unconfigured gathers the keys to offer when none is configured.
func (id *identity) unconfigured(getenv func(string) string, home func() (string, error)) (*identity, error) {
	held := id.agentSigners(getenv)
	inAgent := func(pub ssh.PublicKey) ssh.Signer {
		for _, s := range held {
			if bytes.Equal(s.PublicKey().Marshal(), pub.Marshal()) {
				return s
			}
		}
		return nil
	}
	// No home directory: there are no default identities to try.
	if dir, err := home(); err == nil && dir != "" {
		for _, name := range defaultIdentities {
			path := filepath.Join(dir, ".ssh", name)
			signer, pub, why := readDefaultIdentity(path)
			switch {
			case signer != nil:
				if !id.has(signer.PublicKey()) {
					id.add(signer, "file", path, byDefault)
				}
			case pub != nil && inAgent(pub) != nil:
				if !id.has(pub) {
					id.add(inAgent(pub), "agent", path, byDefault)
				}
			case pub != nil:
				id.skipped = append(id.skipped, fmt.Sprintf("%s: %s, and ssh-agent does not hold %s", path, why, map[bool]string{true: "it", false: "its key"}[strings.HasSuffix(name, "_sk")]))
			case why != "":
				id.skipped = append(id.skipped, path+": "+why)
			}
		}
	}
	for _, s := range held {
		if !id.has(s.PublicKey()) {
			id.add(s, "agent", "", byAgent)
		}
	}
	if len(id.signers) == 0 {
		id.close()
		msg := "no SSH key available"
		if len(id.skipped) > 0 {
			msg += "; skipped " + strings.Join(id.skipped, "; ")
		}
		return nil, id.noted(proto.Errf(proto.CodeAuth,
			fmt.Sprintf("name your key file with %s in your environment, keep it under a default name such as ~/.ssh/id_ed25519, or start ssh-agent and `ssh-add` it", EnvKeyFile),
			msg))
	}
	if len(id.signers) > maxOffered {
		id.left = id.keys[maxOffered:]
		id.signers, id.keys = id.signers[:maxOffered], id.keys[:maxOffered]
	}
	return id, nil
}

// readDefaultIdentity reads one default identity file. It returns a
// signer when the file is a private key that needs no passphrase; the
// key's public half and why the file cannot sign, when ssh-agent may
// hold the key; why alone, when the file is there and of no use; and
// nothing when there is no such file, which is the usual case and is not
// reported. It never repeats what a file holds.
func readDefaultIdentity(path string) (signer ssh.Signer, pub ssh.PublicKey, why string) {
	b, err := os.ReadFile(path) //nolint:gosec // a default identity of the user's own ~/.ssh
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil, ""
	case err != nil:
		cause := err
		if pe, ok := errors.AsType[*fs.PathError](err); ok {
			cause = pe.Err
		}
		return nil, nil, fmt.Sprintf("it cannot be read (%v)", cause)
	}
	beside := func() ssh.PublicKey {
		pb, err := os.ReadFile(path + ".pub") //nolint:gosec // beside a default identity
		if err != nil {
			return nil
		}
		k, _, _, _, _ := ssh.ParseAuthorizedKey(pb)
		return k
	}
	if strings.HasSuffix(path, "_sk") {
		const hardware = "it is a hardware-backed key, which sfx uses only through ssh-agent"
		if pub = beside(); pub == nil {
			return nil, nil, hardware + ", and it has no .pub beside it to ask ssh-agent by"
		}
		return nil, pub, hardware
	}
	s, err := ssh.ParsePrivateKey(b)
	if pm, locked := errors.AsType[*ssh.PassphraseMissingError](err); locked {
		if pub = pm.PublicKey; pub == nil {
			pub = beside()
		}
		if pub == nil {
			return nil, nil, "it is passphrase-protected, and has no .pub beside it to ask ssh-agent by"
		}
		return nil, pub, "it is passphrase-protected"
	}
	if err != nil {
		return nil, nil, "it is not a private key sfx can read"
	}
	return s, nil, ""
}

// readKeyFile reads the configured key file. It returns a signer when
// the file is a private key that needs no passphrase; otherwise the
// key's public half and why the file itself cannot sign; or a refusal.
// from is what named the file. It never repeats what the file holds.
func readKeyFile(path, from string) (signer ssh.Signer, pub ssh.PublicKey, why string, refusal *proto.Error) {
	unreadable := func(err error) *proto.Error {
		// The cause alone: the path is named already.
		cause := err
		if pe, ok := errors.AsType[*fs.PathError](err); ok {
			cause = pe.Err
		}
		return proto.Errf(proto.CodeAuth,
			fmt.Sprintf("fix key: in %s, or set %s to your key file, or remove both to use ssh-agent", ConfigFile, EnvKeyFile),
			fmt.Sprintf("cannot read key file %s, which %s names: %v", path, from, cause))
	}
	b, err := os.ReadFile(path) //nolint:gosec // the configured key file
	if errors.Is(err, fs.ErrNotExist) {
		// As OpenSSH does for an IdentityFile whose key is in the agent.
		pb, perr := os.ReadFile(path + ".pub") //nolint:gosec // beside the configured key file
		if perr != nil {
			return nil, nil, "", unreadable(err)
		}
		if k, _, _, _, kerr := ssh.ParseAuthorizedKey(pb); kerr == nil {
			return nil, k, "is not there, only " + filepath.Base(path) + ".pub, the public half", nil
		}
		return nil, nil, "", unreadable(err)
	}
	if err != nil {
		return nil, nil, "", unreadable(err)
	}
	if k, _, _, _, kerr := ssh.ParseAuthorizedKey(b); kerr == nil {
		return nil, k, "holds only the public half", nil
	}
	s, err := ssh.ParsePrivateKey(b)
	if pm, locked := errors.AsType[*ssh.PassphraseMissingError](err); locked {
		// An OpenSSH key file carries its public half unencrypted; an
		// older format needs the .pub beside it.
		pub := pm.PublicKey
		if pub == nil {
			if pb, perr := os.ReadFile(path + ".pub"); perr == nil { //nolint:gosec // beside the configured key file
				pub, _, _, _, _ = ssh.ParseAuthorizedKey(pb)
			}
		}
		if pub == nil {
			return nil, nil, "", proto.Errf(proto.CodeAuth,
				fmt.Sprintf("put its public key beside it as %s.pub and add the key to ssh-agent: `ssh-add %s`", path, path),
				fmt.Sprintf("key file %s is passphrase-protected, and its public half is not known, so ssh-agent cannot be asked for it", path))
		}
		return nil, pub, "is passphrase-protected", nil
	}
	if err != nil {
		return nil, nil, "", proto.Errf(proto.CodeAuth,
			fmt.Sprintf("point %s at an OpenSSH private key or its .pub file, or remove it to use ssh-agent", from),
			fmt.Sprintf("cannot parse key file %s: %v", path, err))
	}
	return s, nil, "", nil
}

// refusedKeys words the server's refusal of every key offered, or of so
// many that it hung up, and returns nil when err is neither. ssh reports
// both only as text: it has no error value to match with errors.Is. The
// refusal lists the keys offered, by fingerprint and where each came
// from, says which were left out or skipped, and names EnvKeyFile.
func (id *identity) refusedKeys(addr string, err error) *proto.Error {
	text := strings.ToLower(err.Error())
	hungUp := strings.Contains(text, "too many authentication failures")
	if !hungUp && !strings.Contains(text, "unable to authenticate") {
		return nil
	}
	if id.from != "" {
		return id.noted(proto.Errf(proto.CodeAuth,
			fmt.Sprintf("send your public key, the .pub of that file, to the starfix admin, who adds it with `sfx admin key add NAME FILE.pub`; while %s names a key, sfx offers no other, so unset it to offer your default identities and ssh-agent's keys instead", id.from),
			fmt.Sprintf("%s accepted no key: sfx offered %s, the one key %s names (%s)", addr, id.keys[0].Fingerprint, id.from, id.keys[0].Path)))
	}
	msg := fmt.Sprintf("%s accepted none of the %d keys sfx offered: %s", addr, len(id.keys), id.offered())
	if len(id.keys) == 1 {
		msg = fmt.Sprintf("%s accepted no key: sfx offered the one key it found, %s", addr, id.offered())
	}
	if hungUp {
		// A server whose MaxAuthTries is below sshd's default.
		msg = fmt.Sprintf("%s closed the connection after too many refused keys: sfx had %d to offer (%s), more than this server tries", addr, len(id.keys), id.offered())
	}
	if n := len(id.left); n > 0 {
		msg += fmt.Sprintf("; %d more keys that ssh-agent holds were not offered, since sshd hangs up after a few refused keys", n)
	}
	if len(id.skipped) > 0 {
		msg += "; skipped " + strings.Join(id.skipped, "; ")
	}
	fix := fmt.Sprintf("send your public key (`ssh-add -L` lists ssh-agent's) to the starfix admin, who adds it with `sfx admin key add NAME FILE.pub`, or name your key file with %s; once a key is accepted, `sfx whoami` says which key and principal you are", EnvKeyFile)
	if len(id.left) > 0 || hungUp {
		fix = fmt.Sprintf("name your key, so that sfx offers it alone: set %s in your environment to its file, the private key or its .pub; `ssh-add -L` lists what ssh-agent holds", EnvKeyFile)
	}
	return id.noted(proto.Errf(proto.CodeAuth, fix, msg))
}

// quoteStderr is " (who said: …)" quoting what a program printed on
// stderr, escaped, on one line; or "" when it printed nothing.
func quoteStderr(stderr, who string) string {
	s := strings.TrimSpace(stderr)
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = safetext.Line(strings.TrimSuffix(l, "\r"))
	}
	return " (" + who + " said: " + strings.Join(lines, " | ") + ")"
}

// capBuffer keeps the first max bytes written to it, or with tail the
// last max.
type capBuffer struct {
	mu   sync.Mutex
	max  int
	tail bool
	b    []byte
}

func (b *capBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	switch {
	case b.tail:
		p = p[max(0, len(p)-b.max):]
		b.b = append(b.b, p...)
		b.b = b.b[max(0, len(b.b)-b.max):]
	case len(b.b) < b.max:
		b.b = append(b.b, p[:min(b.max-len(b.b), len(p))]...)
	}
	return n, nil
}

func (b *capBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.b)
}
