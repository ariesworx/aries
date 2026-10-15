// Package e2e runs the Aries CLI and MCP server against ariesd through
// a real SSH handshake: an in-process SSH server that authenticates keys,
// maps each to a principal and enforces forced-command semantics by running
// the stdio bridge whatever the client asks for, in front of a daemon on a
// temporary unix socket and a real Postgres store.
//
// A world is one daemon and its SSH server, and a user is a developer with
// a key and a repository whose .aries.yaml points at them. Each test
// makes its own world, whose cleanup stops both and waits for their
// goroutines. A test takes a Postgres server of its own from the
// package's pool; without postgres on PATH the tests skip, unless
// ARIES_REQUIRE_POSTGRES is set.
package e2e

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/internal/cli"
	"github.com/ariesworx/aries/internal/client"
	"github.com/ariesworx/aries/internal/clienttest"
	"github.com/ariesworx/aries/internal/iaptest"
	"github.com/ariesworx/aries/internal/mcpserver"
	"github.com/ariesworx/aries/internal/offline"
	"github.com/ariesworx/aries/internal/pgtest"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/server"
	"github.com/ariesworx/aries/internal/store"
	"github.com/ariesworx/aries/internal/telemetry"
)

// pgs are the package's Postgres servers: a test has one to itself while
// it runs, so the tests can run in parallel (pgtest.Pool).
var pgs = pgtest.NewPool("aries-e2e-")

func TestMain(m *testing.M) {
	iaptest.Main()
	// No test here, the soak included, reads the real ~/.ssh or
	// ssh-agent: each user's key is in its own checkout. Nor does the git
	// the tests start read the user's git configuration.
	restore := clienttest.Isolate()
	restoreGit := clienttest.IsolateGit()
	code := run(m)
	restoreGit()
	restore()
	os.Exit(code)
}

func run(m *testing.M) int {
	defer func() { _ = pgs.Close() }()
	pgs.Warm()
	return m.Run()
}

const project = "6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f"

// patience is how long a test waits for something that must happen. It
// turns a hang into a failure that says what did not happen, and nothing
// is asserted of it, so it is long: `aries watch` took more than twenty
// seconds to start where the tests shared a processor and the database
// was short of memory.
const patience = time.Minute

// world is one daemon behind one SSH server.
type world struct {
	t *testing.T
	// pg is the Postgres server the world's test holds, and no other test
	// while it runs.
	pg      *pgtest.Server
	socket  string
	addr    *net.TCPAddr
	hostFpr string
	// hostKey is the SSH server's host key, which rekey replaces.
	hostKey *swappedKey
	keysMu  sync.Mutex        // guards keys, and writes of handFile
	keys    map[string]string // authorized key fingerprint → principal
	// keysFile is the file the daemon writes the control store's keys to
	// and the SSH server reads, and handFile the authorized_keys file kept
	// by hand, which holds a line for each of keys and which the daemon
	// only reads (daemonOpts.keys).
	keysFile, handFile string
	// hand holds handFile's lines, by fingerprint.
	hand map[string]string

	// fixedClock says the store runs on the test's clock (daemonOpts.now).
	fixedClock bool
	connsMu    sync.Mutex            // guards conns and dropOp
	conns      map[net.Conn]struct{} // open SSH connections
	// dsn is the store's database, for tests that plant rows the store
	// would refuse (data from before a validation, or a hostile writer).
	dsn string
	// made are the databases the world made (newDatabase), which it drops
	// when its test ends.
	madeMu sync.Mutex
	made   []string
	// dropOp, when set, names an op whose next response is never
	// delivered: the connection drops after the server has answered
	// (dropReplyTo).
	dropOp string
	// silentOp, when set, names an op whose next response is never
	// sent, on a connection that stays open (silentAfter). connsMu
	// guards it.
	silentOp string

	// opts are the daemon's options, kept so restartDaemon starts its
	// successor alike; daemonMu guards stopDaemon, which stops the
	// daemon running now.
	opts       daemonOpts
	daemonMu   sync.Mutex
	stopDaemon func()

	// controlDSN is the control database of a server of many projects
	// (daemonOpts.many), and ctl the daemon's own control store, through
	// which tests add projects and members until `aries admin` can.
	controlDSN string
	ctl        *store.Control
	// block is the daemon's block of database names for new projects
	// (daemonOpts.dbs).
	block server.ProjectDBs
	// acceptHistory is the next daemon's --accept-clock-behind: the
	// history it may start behind (server.Config.AcceptHistory). The
	// caller holds daemonMu.
	acceptHistory time.Time
}

// daemonOpts configures a world's daemon; the zero value takes the
// defaults.
type daemonOpts struct {
	// protoMin and protoMax are the protocol versions the server accepts.
	protoMin, protoMax int
	// noDaemon starts only the SSH server, so the bridge finds no daemon.
	noDaemon bool
	// latest is the newest release the server knows of.
	latest string
	// now, if set, is the store's clock. The reaper and the agents'
	// renewals are then off, so claims expire only when the test says.
	now func() time.Time
	// clock, if set, is the machine's two clocks, in place of now: the
	// wall clock and the monotonic time elapsed, from one reading. Each
	// daemon makes its process's store.Clock from it, as serve does, so
	// its stores and the server see the clock's steps.
	clock func() (time.Time, time.Duration)
	// admins are the store's admins.
	admins []string
	// limits are the daemon's limits, the store's included.
	limits server.Limits
	// reap, if set, is how often the reaper runs, on the store's clock
	// even when now sets it. Unset, it runs every 30s, or never with now.
	reap time.Duration
	// logger, if set, receives the daemon's and the store's log.
	logger *slog.Logger
	// telemetry, if set, measures and traces the daemon and its stores,
	// as `ariesd serve` wires them.
	telemetry *telemetry.Telemetry
	// many makes the daemon a server of many projects: the world's
	// database is adopted as project, in a control store (world.ctl)
	// whose server admins are admins. No one else may connect until a
	// test makes them a member.
	many bool
	// dbs is how many schema names the daemon may take for new projects
	// (world.block), of which the first usable are free: the superuser
	// makes each one past them a schema first, which the daemon's role
	// may then not use. Zero is no block.
	dbs, usable int
	// keys makes the daemon write its keys file and the SSH server read
	// it, as an sshd does whose AuthorizedKeysFile names that file and
	// then the hand-kept authorized_keys (world.keys), the order of
	// decision 26: a key with a line in the first is let in as its
	// forced command says, with its fingerprint, and one with none is
	// looked up in the second.
	keys bool
}

// newWorld starts a daemon on a new database, unless o.noDaemon, and the
// SSH server in front of it, and stops both when t ends.
func newWorld(t *testing.T, o daemonOpts) *world {
	t.Helper()
	// Taken before any cleanup is registered, so that the test gives the
	// server up last, once its databases are dropped.
	srv, err := pgs.Server(t)
	if errors.Is(err, pgtest.ErrNoPostgres) {
		t.Skip("postgres is not on PATH: run scripts/install-postgres.sh to run the end-to-end tests")
	}
	if err != nil {
		t.Fatalf("postgres: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	base, err := os.MkdirTemp("", "sfe") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	w := &world{t: t, pg: srv, socket: filepath.Join(base, "run", "ariesd.sock"), keys: map[string]string{},
		conns: map[net.Conn]struct{}{}, opts: o, hand: map[string]string{}}
	// Registered before anything is opened on a database, so that it runs
	// after the daemon has stopped and the test's own connections closed.
	t.Cleanup(w.dropDatabases)
	if o.keys {
		dir := filepath.Join(base, ".ssh")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		w.keysFile, w.handFile = filepath.Join(dir, "aries_keys"), filepath.Join(dir, "authorized_keys")
	}

	if o.now != nil && o.reap == 0 {
		w.fixedClock, w.opts.reap = true, -1
	}
	if !o.noDaemon {
		dsn, err := w.newDatabase(ctx)
		if err != nil {
			t.Fatal(err)
		}
		w.dsn = dsn
		if err := w.startDaemon(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			w.daemonMu.Lock()
			defer w.daemonMu.Unlock()
			w.stopDaemon()
		})
	}
	w.startSSH(ctx)
	return w
}

// newDatabase makes a database on the world's Postgres, which the world
// drops when its test ends.
func (w *world) newDatabase(ctx context.Context) (string, error) {
	dsn, err := w.pg.NewDatabase(ctx)
	if err != nil {
		return "", err
	}
	w.madeMu.Lock()
	defer w.madeMu.Unlock()
	w.made = append(w.made, dsn)
	return dsn, nil
}

// dropDatabases drops the databases newDatabase made.
func (w *world) dropDatabases() {
	w.madeMu.Lock()
	defer w.madeMu.Unlock()
	for _, dsn := range w.made {
		if err := w.pg.DropDatabase(dsn); err != nil {
			w.t.Errorf("drop the world's database: %v", err)
		}
	}
}

// startDaemon opens the store on the world's database and serves it on
// the world's socket until stopDaemon is called. The caller holds
// daemonMu, or is newWorld before anything else can run.
func (w *world) startDaemon() error {
	o := w.opts
	if o.many {
		return w.startMany()
	}
	ctx, cancel := context.WithCancel(context.Background())
	clk := w.processClock()
	st, err := store.Open(ctx, w.dsn, store.Options{Prefix: "sf", Now: o.now, Clock: clk, Admins: o.admins,
		Limits: o.limits.Limits, Logger: o.logger, IdemWindow: time.Duration(o.limits.IdemWindow), AwayMax: time.Duration(o.limits.AwayMax),
		Observer: server.StoreObserver(o.telemetry, project), Tracer: server.StoreTracer(o.telemetry, project)})
	if err != nil {
		cancel()
		return err
	}
	srv, err := server.New(server.Config{Store: st, Project: project, Version: "v0.2.0",
		ProtoMin: o.protoMin, ProtoMax: o.protoMax, Latest: o.latest, ReapInterval: o.reap, Limits: o.limits,
		Logger: o.logger, Clock: clk, AcceptHistory: w.acceptHistory, Telemetry: o.telemetry})
	if err != nil {
		cancel()
		return errors.Join(err, st.Close())
	}
	l, err := server.Listen(w.socket)
	if err != nil {
		cancel()
		return errors.Join(err, st.Close())
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, l) }()
	w.stopDaemon = func() {
		cancel()
		if err := <-done; err != nil {
			w.t.Errorf("serve: %v", err)
		}
		if err := st.Close(); err != nil {
			w.t.Errorf("close store: %v", err)
		}
	}
	return nil
}

// processClock returns the clock of a daemon about to start: a new
// store.Clock on the machine's clocks (daemonOpts.clock), as each
// process makes one, or nil, when the stores make their own from now.
func (w *world) processClock() *store.Clock {
	if w.opts.clock == nil {
		return nil
	}
	return store.NewClock(store.ClockOptions{Read: w.opts.clock})
}

// adopt makes the control store, in a schema beside the world's, and
// records the world's schema in it as project, as the operator's
// `ariesd control init` does before the daemon first serves many
// projects.
func (w *world) adopt(ctx context.Context) error {
	dsn, err := store.SchemaDSN(w.dsn, "aries_control")
	if err != nil {
		return err
	}
	d, err := store.ParseDSN(w.dsn)
	if err != nil {
		return err
	}
	ctl, err := store.OpenControl(ctx, dsn, store.ControlOptions{Operator: true})
	if err != nil {
		return err
	}
	operator := store.Actor{Principal: store.OperatorPrincipal, Session: "e2e", Machine: "server"}
	_, err = ctl.AddProject(ctx, operator, store.NewProject{ID: project, Name: "first", Prefix: "sf", Account: "internal",
		DBName: d.Schema})
	if err == nil {
		_, err = ctl.ActivateProject(ctx, operator, project)
	}
	if err = errors.Join(err, ctl.Close()); err != nil {
		return err
	}
	w.controlDSN = dsn
	if w.opts.dbs > 0 {
		w.block = server.ProjectDBs{Prefix: "aries_p", Count: w.opts.dbs}
		for i := w.opts.usable + 1; i <= w.opts.dbs; i++ {
			if err := w.asSuperuser(ctx, "CREATE SCHEMA "+w.block.Name(i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// asSuperuser runs each statement on the world's database as its
// server's superuser.
func (w *world) asSuperuser(ctx context.Context, stmts ...string) error {
	d, err := store.ParseDSN(w.dsn)
	if err != nil {
		return err
	}
	db, err := sql.Open("pgx", w.pg.RootDSN(d.Database))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	return nil
}

// startMany is startDaemon for a server of many projects: at the first
// start it adopts the world's database as project, and then it serves
// every project the control store holds.
func (w *world) startMany() error {
	o := w.opts
	ctx, cancel := context.WithCancel(context.Background())
	if w.controlDSN == "" {
		if err := w.adopt(ctx); err != nil {
			cancel()
			return err
		}
	}
	clk := w.processClock()
	ctl, err := store.OpenControl(ctx, w.controlDSN, store.ControlOptions{Admins: o.admins, Now: o.now, Clock: clk,
		Limits: o.limits.Control, Logger: o.logger, Tracer: server.StoreTracer(o.telemetry, "")})
	if err != nil {
		cancel()
		return err
	}
	fail := func(err error) error {
		cancel()
		return errors.Join(err, ctl.Close())
	}
	srv, err := server.New(server.Config{Control: ctl, Version: "v0.2.0", ProjectDBs: w.block,
		Open:     server.ProjectOpener(w.dsn, ctl, store.Options{Now: o.now, Clock: clk, Limits: o.limits.Limits, Logger: o.logger, IdemWindow: time.Duration(o.limits.IdemWindow), AwayMax: time.Duration(o.limits.AwayMax)}, o.telemetry),
		ProtoMin: o.protoMin, ProtoMax: o.protoMax, Latest: o.latest, ReapInterval: o.reap, Limits: o.limits, Logger: o.logger,
		KeysFile: w.keysFile, AuthorizedKeys: w.handFile, Clock: clk, AcceptHistory: w.acceptHistory, Telemetry: o.telemetry})
	if err != nil {
		return fail(err)
	}
	// As `ariesd serve` does before it listens.
	if err := srv.WriteKeys(); err != nil {
		return fail(err)
	}
	// Every active project's schema is opened once before the daemon
	// listens, which is also where it checks its clock against each
	// one's history and may refuse to start (*server.ClockBehindError).
	if err := srv.OpenProjects(ctx); err != nil {
		return fail(err)
	}
	if err := srv.ReleaseArchived(ctx, server.ProjectOpener(w.dsn, ctl,
		store.Options{Now: o.now, Clock: clk, Limits: o.limits.Limits, Logger: o.logger, NoMigrate: true}, o.telemetry)); err != nil {
		return fail(err)
	}
	l, err := server.Listen(w.socket)
	if err != nil {
		return fail(err)
	}
	w.ctl = ctl
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, l) }()
	w.stopDaemon = func() {
		cancel()
		if err := <-done; err != nil {
			w.t.Errorf("serve: %v", err)
		}
		if err := ctl.Close(); err != nil {
			w.t.Errorf("close control store: %v", err)
		}
	}
	return nil
}

// stop stops the daemon and leaves the SSH server up, so that the bridge
// finds no daemon: the server cannot be reached until start.
func (w *world) stop() {
	w.daemonMu.Lock()
	defer w.daemonMu.Unlock()
	w.stopDaemon()
	w.stopDaemon = func() {} // until a new daemon starts, nothing runs to stop
}

// start starts a daemon on the world's database and socket after stop.
func (w *world) start() {
	w.t.Helper()
	w.daemonMu.Lock()
	defer w.daemonMu.Unlock()
	if err := w.startDaemon(); err != nil {
		w.t.Fatal(err)
	}
}

// restartDaemon stops the daemon, closing every connection to it, and
// starts another on the same database and socket, as an operator's
// restart would.
func (w *world) restartDaemon() error {
	w.daemonMu.Lock()
	defer w.daemonMu.Unlock()
	w.stopDaemon()
	w.stopDaemon = func() {} // until a new daemon starts, nothing runs to stop
	return w.startDaemon()
}

// startSSH runs the in-process sshd. Like OpenSSH with
// restrict,command="ariesd stdio --principal NAME", it maps the key to a
// principal, refuses pty and forwarding, and runs the bridge for any exec
// or shell request, ignoring the command the client sent. With
// daemonOpts.keys a key is first looked up in the keys file the daemon
// writes, as OpenSSH reads the first file of its AuthorizedKeysFile
// (fileLine), and only then among its own keys, the second file.
func (w *world) startSSH(ctx context.Context) {
	t := w.t
	t.Helper()
	w.hostKey = &swappedKey{}
	w.rekey()
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			// sshd takes the first line that matches, in the order of its
			// AuthorizedKeysFile: the daemon's file, then the one kept by
			// hand (decision 26). A stale line of the first therefore
			// shadows the second's line for the same key.
			if perms := w.fileLine(key); perms != nil {
				return perms, nil
			}
			w.keysMu.Lock()
			p, ok := w.keys[ssh.FingerprintSHA256(key)]
			w.keysMu.Unlock()
			if ok {
				return &ssh.Permissions{Extensions: map[string]string{"principal": p}}, nil
			}
			return nil, errors.New("unknown key")
		},
	}
	cfg.AddHostKey(w.hostKey)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	w.addr = l.Addr().(*net.TCPAddr)
	var wg sync.WaitGroup
	t.Cleanup(func() { _ = l.Close(); wg.Wait() })
	wg.Go(func() {
		for {
			nc, err := l.Accept()
			if err != nil {
				return
			}
			wg.Go(func() { w.serveSSH(ctx, nc, cfg) })
		}
	})
}

// swappedKey is a host key that rekey replaces while the SSH server
// runs, as rebuilding a server gives it a new one.
type swappedKey struct {
	mu sync.Mutex
	s  ssh.Signer
}

func (k *swappedKey) signer() ssh.Signer {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.s
}

func (k *swappedKey) PublicKey() ssh.PublicKey { return k.signer().PublicKey() }

func (k *swappedKey) Sign(r io.Reader, data []byte) (*ssh.Signature, error) {
	return k.signer().Sign(r, data)
}

// rekey gives the SSH server a new host key, as rebuilding the server
// does, and makes it the one new users pin. What users have pinned is
// unchanged.
func (w *world) rekey() {
	w.t.Helper()
	w.useHostKey(newHostKey(w.t))
}

// newHostKey makes a host key, for a server to present later.
func newHostKey(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// useHostKey has the SSH server present s from its next connection, as
// an operator who switches sshd to another host key does, and makes it
// the one new users pin.
func (w *world) useHostKey(s ssh.Signer) {
	w.hostKey.mu.Lock()
	w.hostKey.s = s
	w.hostKey.mu.Unlock()
	w.hostFpr = ssh.FingerprintSHA256(s.PublicKey())
}

// fileLine looks key up in the keys file the daemon writes, and reads
// its line as sshd would: the first line whose key is the one offered,
// whose forced command gives the principal and the key's fingerprint,
// which the bridge is then run with. It returns nil when the world has
// no keys file, or the file no line for the key. A line that is not what
// the daemon may write fails the test.
func (w *world) fileLine(key ssh.PublicKey) *ssh.Permissions {
	if w.keysFile == "" {
		return nil
	}
	b, err := os.ReadFile(w.keysFile)
	if err != nil {
		return nil
	}
	for line := range bytes.Lines(b) {
		if text := bytes.TrimSpace(line); len(text) == 0 || text[0] == '#' {
			continue
		}
		named, comment, options, rest, err := ssh.ParseAuthorizedKey(line)
		if err != nil || comment != "" || len(rest) != 0 || len(options) != 2 || options[0] != "restrict" {
			w.t.Errorf("the keys file holds a line sshd would not take as one restricted key: %q (%v)", line, err)
			continue
		}
		if !bytes.Equal(named.Marshal(), key.Marshal()) {
			continue
		}
		cmd, ok := strings.CutPrefix(options[1], `command="`)
		f := strings.Fields(strings.TrimSuffix(cmd, `"`))
		if !ok || len(f) != 6 || f[0] != server.DefaultBridge || f[1] != "stdio" || f[2] != "--principal" || f[4] != "--key" || f[5] != ssh.FingerprintSHA256(key) {
			w.t.Errorf("the keys file forces %q for a key, want %s stdio --principal NAME --key and its fingerprint", options[1], server.DefaultBridge)
			return nil
		}
		return &ssh.Permissions{Extensions: map[string]string{"principal": f[3], "key": f[5]}}
	}
	return nil
}

// byHand gives key a line in the authorized_keys file kept by hand,
// forcing the bridge for principal, or takes its line out when principal
// is "": the SSH server reads world.keys, and the daemon the file.
func (w *world) byHand(key ssh.PublicKey, principal string) {
	w.t.Helper()
	w.keysMu.Lock()
	defer w.keysMu.Unlock()
	fp := ssh.FingerprintSHA256(key)
	if principal == "" {
		delete(w.keys, fp)
		delete(w.hand, fp)
	} else {
		w.keys[fp] = principal
		w.hand[fp] = fmt.Sprintf("restrict,command=\"%s stdio --principal %s\" %s %s@example.com\n", server.DefaultBridge, principal,
			strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))), principal)
	}
	if w.handFile == "" {
		return
	}
	var b strings.Builder
	b.WriteString("# kept by hand: the first admin, and whoever was here before the store\n")
	for _, k := range slices.Sorted(maps.Keys(w.hand)) {
		b.WriteString(w.hand[k])
	}
	if err := os.WriteFile(w.handFile, []byte(b.String()), 0o600); err != nil {
		w.t.Fatal(err)
	}
}

// dropReplyTo arranges for the next request of op to be applied by the
// server and its response lost: the SSH connection drops instead of
// delivering it, as a network failure at the worst moment would.
func (w *world) dropReplyTo(op string) {
	w.connsMu.Lock()
	defer w.connsMu.Unlock()
	w.dropOp = op
}

// silentAfter arranges for the next request of op to be applied by the
// server and never answered, on a connection that stays open: a server,
// or a path to it, that stops answering. The client's own bound on the
// request is then what ends it.
func (w *world) silentAfter(op string) {
	w.connsMu.Lock()
	defer w.connsMu.Unlock()
	w.silentOp = op
}

// tap watches one SSH connection's frames for dropReplyTo and
// silentAfter: reads note the id of the request, writes drop the
// connection at its response, or leave the response out.
type tap struct {
	w    *world
	nc   net.Conn
	in   io.Reader
	out  io.Writer
	mu   sync.Mutex // guards drop, rbuf and wbuf
	drop map[uint64]bool
	// silent are the requests whose responses are left out.
	silent map[uint64]bool
	rbuf   []byte // partial request line
	wbuf   []byte // partial response line
}

func (t *tap) Read(p []byte) (int, error) {
	n, err := t.in.Read(p)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rbuf = append(t.rbuf, p[:n]...)
	for {
		i := bytes.IndexByte(t.rbuf, '\n')
		if i < 0 {
			break
		}
		var f proto.Frame
		if json.Unmarshal(t.rbuf[:i], &f) == nil && f.T == proto.FrameReq {
			t.w.connsMu.Lock()
			if f.Op == t.w.dropOp {
				t.w.dropOp = ""
				t.drop[f.ID] = true
			}
			if f.Op == t.w.silentOp {
				t.w.silentOp = ""
				t.silent[f.ID] = true
			}
			t.w.connsMu.Unlock()
		}
		t.rbuf = t.rbuf[i+1:]
	}
	return n, err
}

func (t *tap) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.wbuf = append(t.wbuf, p...)
	for {
		i := bytes.IndexByte(t.wbuf, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := t.wbuf[:i+1]
		var f proto.Frame
		if json.Unmarshal(line, &f) == nil && f.T == proto.FrameRes && t.drop[f.ID] {
			_ = t.nc.Close()
			return 0, errors.New("e2e: response dropped")
		}
		if f.T == proto.FrameRes && t.silent[f.ID] {
			t.wbuf = t.wbuf[i+1:]
			continue
		}
		if _, err := t.out.Write(line); err != nil {
			return 0, err
		}
		t.wbuf = t.wbuf[i+1:]
	}
}

// dropAll closes every open SSH connection, as a network drop would.
func (w *world) dropAll() {
	w.connsMu.Lock()
	defer w.connsMu.Unlock()
	for c := range w.conns {
		_ = c.Close()
	}
}

// serveSSH serves one SSH connection until it closes, then waits for its
// sessions. A session runs the bridge on its first exec or shell request;
// other channel types and requests are refused.
func (w *world) serveSSH(ctx context.Context, nc net.Conn, cfg *ssh.ServerConfig) {
	w.connsMu.Lock()
	w.conns[nc] = struct{}{}
	w.connsMu.Unlock()
	defer func() {
		w.connsMu.Lock()
		delete(w.conns, nc)
		w.connsMu.Unlock()
		_ = nc.Close()
	}()
	sc, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		return
	}
	defer func() { _ = sc.Close() }()
	go ssh.DiscardRequests(reqs)
	principal := sc.Permissions.Extensions["principal"]
	var wg sync.WaitGroup
	defer wg.Wait()
	for nch := range chans {
		if nch.ChannelType() != "session" {
			_ = nch.Reject(ssh.Prohibited, "restricted")
			continue
		}
		ch, creqs, err := nch.Accept()
		if err != nil {
			return
		}
		wg.Go(func() {
			defer func() { _ = ch.Close() }()
			for req := range creqs {
				switch req.Type {
				case "exec", "shell":
					_ = req.Reply(true, nil)
					status := uint32(0)
					tp := &tap{w: w, nc: nc, in: ch, out: ch, drop: map[uint64]bool{}, silent: map[uint64]bool{}}
					if err := server.Bridge(ctx, w.socket, principal, sc.Permissions.Extensions["key"], tp, tp); err != nil {
						_, _ = fmt.Fprintf(ch.Stderr(), "ariesd: %v\n", err)
						status = 1
					}
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
					return
				default: // pty-req, env, x11, agent forwarding: refused, as with restrict
					_ = req.Reply(false, nil)
				}
			}
		})
	}
}

// user is a developer: a key, and a repository whose .aries.yaml points at
// the world's server.
type user struct {
	w    *world
	repo string
	env  map[string]string
	// now, if set, is the CLI's clock, in the user's time zone; unset,
	// the CLI's default.
	now func() time.Time
	// stdin, if set, is what the CLI reads from standard input.
	stdin string
	// terminal says a person is at the CLI's input and output, so that it
	// may ask them something.
	terminal bool
	// pubFile is the user's public key file, for `aries admin key add`.
	pubFile string
	// queues gives the user's `aries mcp` an outbox, as cmdMCP does.
	queues bool
	// key is the user's public key, for a test that has the server take
	// it for another principal (world.byHand).
	key ssh.PublicKey
	// after, if set, is the clock `aries lock` waits on, for a test whose
	// store's clock runs fast; unset, the CLI's default.
	after func(time.Duration) <-chan time.Time
}

// newUser makes a user with a new key, which the server maps to principal
// ("" leaves the key unknown to it). The user's .aries.yaml pins the
// server's host key, or hostFpr if it is set.
func (w *world) newUser(principal, hostFpr string) *user {
	w.t.Helper()
	return w.newUserIn(principal, hostFpr, project)
}

// newUserIn is newUser for a repository whose .aries.yaml names
// project proj.
func (w *world) newUserIn(principal, hostFpr, proj string) *user {
	t := w.t
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	if principal != "" {
		w.byHand(signer.PublicKey(), principal)
	}
	block, err := ssh.MarshalPrivateKey(priv, principal+"@example.com")
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "id_ed25519"), pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	// The public key, as ssh-keygen leaves one: outside the repository,
	// so that git sees nothing new in it.
	pubFile := filepath.Join(t.TempDir(), "id_ed25519.pub")
	pub := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) + " " + cmp.Or(principal, "someone") + "@example.com\n"
	if err := os.WriteFile(pubFile, []byte(pub), 0o600); err != nil {
		t.Fatal(err)
	}
	if hostFpr == "" {
		hostFpr = w.hostFpr
	}
	cfg := fmt.Sprintf("project: %s\nserver:\n  host: 127.0.0.1\n  port: %d\n  user: aries\n  host_key: %s\nkey: id_ed25519\n",
		proj, w.addr.Port, hostFpr)
	if err := os.WriteFile(filepath.Join(repo, ".aries.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	// What the user queues while the server is away goes to a data
	// directory of their own, as on their own machine: never to that of
	// whoever runs the tests.
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil { //nolint:gosec // a directory: 0700 is owner-only
		t.Fatal(err)
	}
	return &user{w: w, repo: repo, env: map[string]string{offline.EnvDataDir: data}, pubFile: pubFile, key: signer.PublicKey()}
}

// result is one CLI run's exit code and output.
type result struct {
	code           int
	stdout, stderr string
}

// run invokes the CLI as this user, from a subdirectory of the repository.
func (u *user) run(version string, args ...string) result {
	u.w.t.Helper()
	sub := filepath.Join(u.repo, "src")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		u.w.t.Fatal(err)
	}
	var out, errb bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	code := u.runTo(ctx, &out, &errb, version, args...)
	return result{code: code, stdout: out.String(), stderr: errb.String()}
}

// runTo invokes the CLI as this user until ctx ends, writing to the
// writers given.
func (u *user) runTo(ctx context.Context, stdout, stderr io.Writer, version string, args ...string) int {
	sub := filepath.Join(u.repo, "src")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		u.w.t.Error(err)
		return -1
	}
	return cli.Run(ctx, append([]string{"-C", sub}, args...), cli.Env{
		Stdin: strings.NewReader(cmp.Or(u.stdin, "from stdin\n")), Stdout: stdout, Stderr: stderr,
		Getenv:   func(k string) string { return u.env[k] }, // no SSH_AUTH_SOCK: the key file is used
		Hostname: func() (string, error) { return "laptop-test", nil },
		Version:  version,
		Now:      u.now,
		Terminal: func() bool { return u.terminal },
		// What `aries lock run` gives the command it runs: a PATH to find it
		// by, and nothing else of the test's environment.
		Environ: func() []string { return []string{"PATH=" + os.Getenv("PATH")} },
		After:   u.after,
	})
}

// ok runs the CLI as client v0.2.0 and returns its standard output,
// failing the test unless it exits 0.
func (u *user) ok(args ...string) string {
	u.w.t.Helper()
	r := u.run("v0.2.0", args...)
	if r.code != 0 {
		u.w.t.Fatalf("aries %s: exit %d\n%s%s", strings.Join(args, " "), r.code, r.stdout, r.stderr)
	}
	return r.stdout
}

// decode unmarshals s as a T, failing the test if it cannot.
func decode[T any](t *testing.T, s string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return v
}

func TestCRUDRoundTrip(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{})
	alice := w.newUser("alice", "")

	a := decode[proto.WriteResult](t, alice.ok("create", "Ship", "the", "thing", "-p", "1", "--label", "area:api", "--json"))
	if a.Rev != 1 || !strings.HasPrefix(a.ID, "sf-") {
		t.Fatalf("create: %+v", a)
	}
	b := strings.TrimSpace(alice.ok("create", "Write the docs", "--body", "-", "-t", "chore"))
	alice.ok("dep", "add", a.ID, b)

	if out := alice.ok("ready"); !strings.Contains(out, b) || strings.Contains(out, a.ID) {
		t.Fatalf("ready:\n%s", out)
	}
	if out := alice.ok("blocked"); !strings.Contains(out, a.ID) || !strings.Contains(out, "blocked by "+b) {
		t.Fatalf("blocked:\n%s", out)
	}
	if got := alice.ok("update", a.ID, "--title", "Ship it", "--assignee", "alice"); got != a.ID+" rev 2\n" {
		t.Fatalf("update: %q", got)
	}
	show := alice.ok("show", a.ID)
	for _, want := range []string{a.ID + "  P1  open  task  rev 2", "Ship it", "assignee: alice", "labels: area:api", "depends on: " + b + " (blocks)"} {
		if !strings.Contains(show, want) {
			t.Fatalf("show lacks %q:\n%s", want, show)
		}
	}
	if s := decode[proto.ShowResult](t, alice.ok("show", b, "--json")); s.Issue.Body != "from stdin" || s.Issue.Type != "chore" {
		t.Fatalf("show --json: %+v", s.Issue)
	}

	alice.ok("label", "add", b, "docs", "small")
	alice.ok("label", "rm", b, "small")
	if l := decode[proto.ListResult](t, alice.ok("list", "--label", "docs", "--json")); len(l.Issues) != 1 || l.Issues[0].ID != b {
		t.Fatalf("list --label: %+v", l)
	}
	cid := strings.TrimSpace(alice.ok("comment", b, "looks", "good"))
	if out := alice.ok("comments", b); !strings.Contains(out, "alice") || !strings.Contains(out, "looks good") {
		t.Fatalf("comments:\n%s", out)
	}
	if c := decode[proto.CommentsResult](t, alice.ok("comments", b, "--json")); len(c.Comments) != 1 || c.Comments[0].ID != cid {
		t.Fatalf("comments --json: %+v", c)
	}

	alice.ok("close", b, "--reason", "done")
	if out := alice.ok("list"); strings.Contains(out, b) {
		t.Fatalf("closed issue in default list:\n%s", out)
	}
	if out := alice.ok("list", "--all"); !strings.Contains(out, b) {
		t.Fatalf("closed issue missing from list --all:\n%s", out)
	}
	if out := alice.ok("ready"); !strings.Contains(out, a.ID) {
		t.Fatalf("unblocked issue not ready:\n%s", out)
	}
	alice.ok("reopen", b)
	alice.ok("dep", "rm", a.ID, b)

	h := decode[proto.HistoryResult](t, alice.ok("history", b, "--json"))
	var ops []string
	for _, e := range h.Events {
		ops = append(ops, e.Op)
		if e.Principal != "alice" || e.Machine != "laptop-test" || e.Session != client.CLISession {
			t.Fatalf("event actor: %+v", e)
		}
	}
	if got := strings.Join(ops, " "); got != "issue.create label.add label.add label.remove comment.add issue.close issue.reopen" {
		t.Fatalf("history: %s", got)
	}
	if out := alice.ok("history", a.ID); !strings.Contains(out, "issue.update") || !strings.Contains(out, "assignee, title") {
		t.Fatalf("history:\n%s", out)
	}
}

func TestSessionFromEnvironment(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{})
	alice := w.newUser("alice", "")
	alice.env["CLAUDE_SESSION_ID"] = "claude-123"
	id := strings.TrimSpace(alice.ok("create", "one"))
	alice.env["ARIES_SESSION"] = "aries-456"
	alice.ok("comment", id, "two")
	h := decode[proto.HistoryResult](t, alice.ok("history", id, "--json"))
	if len(h.Events) != 2 || h.Events[0].Session != "claude-123" || h.Events[1].Session != "aries-456" {
		t.Fatalf("sessions: %+v", h.Events)
	}
}

func TestConflict(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	id := strings.TrimSpace(alice.ok("create", "contested"))
	bob.ok("update", id, "--rev", "1", "-p", "0")

	r := alice.run("v0.2.0", "update", id, "--rev", "1", "--title", "mine")
	want := fmt.Sprintf("aries: %s changed since rev 1 (now rev 2 by bob)\nfix: re-read with `aries show %s`\n", id, id)
	if r.code != cli.ExitFailure || r.stderr != want || r.stdout != "" {
		t.Fatalf("exit %d\nstderr %q\nwant   %q", r.code, r.stderr, want)
	}
	r = alice.run("v0.2.0", "update", id, "--rev", "1", "--title", "mine", "--json")
	e := decode[map[string]proto.Error](t, r.stdout)["error"]
	if r.code != cli.ExitFailure || e.Code != proto.CodeConflict || !strings.Contains(e.Message, "now rev 2 by bob") {
		t.Fatalf("json conflict: exit %d %+v", r.code, e)
	}
	// Without --rev the CLI reads the current revision first.
	alice.ok("update", id, "--title", "mine")
}

func TestRefusals(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{})
	other := w.newUser("", "") // a key the server does not know
	pinnedWrong := w.newUser("carol", "SHA256:et6CqkKsyU2BxzA7Ws+V18rXrtM9Gj6dk1/m0C5TqvU")

	tests := []struct {
		name string
		u    *user
		args []string
		code int
		want []string
	}{
		{name: "wrong host key", u: pinnedWrong, args: []string{"ready"}, code: cli.ExitFailure,
			want: []string{"host key mismatch for 127.0.0.1", w.hostFpr, "fix: if the server's host key was changed"}},
		{name: "unknown key", u: other, args: []string{"ready"}, code: cli.ExitFailure,
			want: []string{"accepted no key: aries offered SHA256:", "fix: send your public key"}},
		{name: "not found", u: w.newUser("dave", ""), args: []string{"show", "sf-zzzzzzzz"}, code: cli.ExitFailure,
			want: []string{"aries: issue sf-zzzzzzzz not found\nfix: find the id with `aries list`"}},
		{name: "usage", u: other, args: []string{"show"}, code: cli.ExitUsage,
			want: []string{"show needs exactly one issue id", "fix: usage: aries show ID"}},
		{name: "unknown command", u: other, args: []string{"explode"}, code: cli.ExitUsage,
			want: []string{`unknown command "explode"`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.u.run("v0.2.0", tc.args...)
			if r.code != tc.code {
				t.Fatalf("exit %d, want %d\n%s", r.code, tc.code, r.stderr)
			}
			for _, s := range tc.want {
				if !strings.Contains(r.stderr, s) {
					t.Fatalf("stderr lacks %q:\n%s", s, r.stderr)
				}
			}
		})
	}
}

func TestProtocolRangeRefusal(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{protoMin: proto.Proto + 1, protoMax: proto.Proto + 2})
	alice := w.newUser("alice", "")
	r := alice.run("v0.2.0", "ready")
	if r.code != cli.ExitVersion {
		t.Fatalf("exit %d, want %d\n%s", r.code, cli.ExitVersion, r.stderr)
	}
	want := fmt.Sprintf("aries: aries speaks protocol %d; the server (v0.2.0) needs protocols %d to %d\n", proto.Proto, proto.Proto+1, proto.Proto+2) +
		"fix: upgrade this client: run `aries upgrade`, or install the latest release (docs/install.md), then restart any agent that runs `aries mcp`\n"
	if r.stderr != want {
		t.Fatalf("stderr %q\nwant   %q", r.stderr, want)
	}
	r = alice.run("v0.2.0", "--json", "ready")
	if e := decode[map[string]proto.Error](t, r.stdout)["error"]; r.code != cli.ExitVersion || e.Code != proto.CodeVersion {
		t.Fatalf("json: exit %d %+v", r.code, e)
	}
}

func TestOlderClientWarns(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{})
	alice := w.newUser("alice", "")
	r := alice.run("v0.1.0", "ready")
	if r.code != 0 || r.stderr != "aries: aries v0.1.0 is older than the server (v0.2.0); run `aries upgrade`\n" {
		t.Fatalf("exit %d stderr %q", r.code, r.stderr)
	}
	if r := alice.run("v0.2.0", "ready"); r.stderr != "" {
		t.Fatalf("same version warned: %q", r.stderr)
	}
}

// A key in .aries.yaml that this aries does not know, such as one a
// newer aries reads, does not stop it: a command warns once, on standard
// error, and goes on, and prime tells the agent.
func TestUnknownConfigKeyWarns(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{})
	alice := w.newUser("alice", "")
	cfg := filepath.Join(alice.repo, ".aries.yaml")
	b, err := os.ReadFile(cfg) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, append(b, "transport: https\n"...), 0o600); err != nil { //nolint:gosec // the test's own temp repository
		t.Fatal(err)
	}
	const want = "aries: warning: .aries.yaml has keys this aries does not know, ignored: transport; " +
		"fix: run `aries upgrade` if a newer aries reads them, else remove them\n"
	if r := alice.run("v0.2.0", "create", "First"); r.code != 0 || r.stderr != want {
		t.Errorf("aries create with an unknown key: exit %d, stderr %q; want 0, %q", r.code, r.stderr, want)
	}
	if out := alice.ok("prime"); !strings.Contains(out, "notice: .aries.yaml has keys this aries does not know, ignored: transport;") {
		t.Errorf("aries prime with an unknown key lacks the notice:\n%s", out)
	}
	p := decode[mcpserver.Prime](t, alice.mcp("v0.2.0").ok("prime", nil))
	if !slices.ContainsFunc(p.Notices, func(n string) bool { return strings.HasPrefix(n, ".aries.yaml has keys") }) {
		t.Errorf("the prime tool with an unknown key: notices %q, want the config's warning", p.Notices)
	}
}

// A restart whose new daemon fails to start leaves nothing to stop: the
// world's cleanup must not wait on the daemon the restart already
// stopped.
func TestRestartDaemonFails(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{})
	// The listener refuses a socket directory others can read.
	if err := os.Chmod(filepath.Dir(w.socket), 0o755); err != nil { //nolint:gosec // the mode the listener must refuse
		t.Fatal(err)
	}
	if err := w.restartDaemon(); err == nil || !strings.Contains(err.Error(), "chmod 700") {
		t.Fatalf("restartDaemon with an open socket directory = %v, want its refusal", err)
	}
	stop, stopped := w.stopDaemon, make(chan struct{})
	go func() {
		defer close(stopped)
		stop()
	}()
	select {
	case <-stopped:
	case <-time.After(patience):
		w.stopDaemon = func() {} // let the cleanup finish; the goroutine above stays blocked
		t.Fatal("stopDaemon after a failed restart did not return: it waits again on the daemon the restart stopped")
	}
}

func TestDaemonDown(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{noDaemon: true})
	r := w.newUser("alice", "").run("v0.2.0", "ready")
	if r.code != cli.ExitFailure || !strings.Contains(r.stderr, "ariesd is not running on the server") ||
		!strings.Contains(r.stderr, "fix: ask the server admin to start it") {
		t.Fatalf("exit %d\n%s", r.code, r.stderr)
	}
}
