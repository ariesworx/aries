package server

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ariesworx/starfix/internal/doltmetrics"
	"github.com/ariesworx/starfix/internal/lazyregexp"
	"github.com/ariesworx/starfix/internal/logs"
	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/safetext"
	"github.com/ariesworx/starfix/internal/store"
	"github.com/ariesworx/starfix/internal/telemetry"
)

// Config configures a Server.
type Config struct {
	// Store is the issue store of a server of one project. Required,
	// unless Control is set.
	Store *store.Store
	// Project is the UUID a server of one project serves; a hello naming
	// another is refused. Required with Store.
	Project string
	// Control, when set, makes this a server of many projects (design
	// §15): a hello's project is looked up in it, the principal must be
	// a server admin or an active member of that project, and each
	// project's store is opened with Open on its first connection and
	// closed when idle (Limits.OpenStores, Limits.StoreIdle). Store and
	// Project are then left unset. The caller closes Control after Serve
	// returns.
	Control *store.Control
	// Open opens the store of one of Control's projects; the server
	// closes it. Required with Control. See [ProjectOpener].
	Open func(context.Context, store.Project) (*store.Store, error)
	// Version is the server's build version, reported in the handshake.
	Version string
	// ProtoMin and ProtoMax bound the client protocol versions accepted.
	// Zero takes proto.ProtoMin and proto.ProtoMax.
	ProtoMin, ProtoMax int
	// Latest is the latest release the server knows of; may be empty.
	Latest string
	// Logger receives the daemon's log: each request at debug, refusals
	// sampled per principal (Limits.RefusalLogs), and reaping, pruning
	// and store failures. Nil discards.
	Logger *slog.Logger
	// PeerCheck vets each connection before its bridge frame is trusted.
	// Nil uses CheckPeer.
	PeerCheck func(net.Conn) error
	// RequestTimeout bounds one request, and each reap, prune and
	// presence update. Default 60s.
	RequestTimeout time.Duration
	// ReapInterval is how often expired claims are ended. Default 30s;
	// negative turns the reaper off (tests call Reap and Prune).
	ReapInterval time.Duration
	// Limits bound connections, idling, writes and refusal logging per
	// principal; zero fields take DefaultLimits. Its store limits are the
	// store's to apply (store.Options.Limits), and only shown here.
	Limits Limits
	// Account is the project's default account, shown for an issue that
	// neither sets one nor inherits one. Empty takes DefaultAccount. With
	// Control each project's own account is used instead.
	Account string
	// ProjectDBs is the block of database names a new project's database
	// is taken from (Control only). The zero value has none, and adding
	// a project is refused.
	ProjectDBs ProjectDBs
	// KeysFile is the absolute path of the file the server writes the
	// control store's keys to, in authorized_keys form, for sshd to read
	// (Control only; see [Server.WriteKeys]). Empty writes none.
	KeysFile string
	// AuthorizedKeys is the absolute path of the authorized_keys file the
	// operator maintains by hand, which sshd reads beside KeysFile. The
	// server only reads it, to say which of the store's keys are in it
	// too. Empty reads none. It is never KeysFile.
	AuthorizedKeys string
	// Bridge is the path of starfixd in the forced command of each line
	// of KeysFile ([ValidBridge]). Empty takes DefaultBridge.
	Bridge string
	// Clock is the process's clock, the one its stores were opened with
	// (store.Options.Clock): the server reports its steps. Nil makes one
	// of its own, which sees the steps but shares them with no store.
	Clock *store.Clock
	// ClockBehind is what the server does at start when its clock is
	// behind the history of a database it serves by more than
	// BehindTolerance: ClockStop, the default, or ClockWarn (clock.go).
	// BehindTolerance defaults to DefaultBehindTolerance.
	ClockBehind     string
	BehindTolerance time.Duration
	// AcceptHistory, when set, lets a server whose ClockBehind is
	// ClockStop start although its clock is behind its history, as long
	// as that history is no later than this: the operator's answer, for
	// one start, to a [*ClockBehindError], which names the time to give.
	AcceptHistory time.Time
	// NoAudit turns off the audit stream: the log lines with audit=true
	// that copy each access change of Control's event log (audit.go).
	// The zero value writes them.
	NoAudit bool
	// Telemetry records the daemon's measurements (design §16.4). Nil
	// records none. A store the caller opens itself is measured when it
	// is opened with [StoreObserver].
	Telemetry *telemetry.Telemetry
	// Heartbeat, when set, is called every HeartbeatEvery by the reaper's
	// goroutine, between its ticks, so it stops being called when that
	// goroutine hangs: what feeds systemd's watchdog. It needs a
	// ReapInterval above zero.
	Heartbeat      func()
	HeartbeatEvery time.Duration
	// DoltMetrics, when set, reads Dolt's own metrics endpoint for the
	// status report (doltwatch.go): Serve reads it every
	// DoltMetricsEvery, which defaults to DefaultDoltMetricsEvery;
	// negative turns the timer off (tests call ReadDolt). Nil reads
	// nothing, and the report says nothing of Dolt. The caller closes it
	// after Serve returns.
	DoltMetrics      *doltmetrics.Reader
	DoltMetricsEvery time.Duration
}

// Server serves the protocol. Create it with New.
type Server struct {
	cfg Config

	mu sync.Mutex
	// conns holds the open connections, and for each one past its
	// handshake whose it is; guarded by mu.
	conns map[net.Conn]owner
	wg    sync.WaitGroup // the reaper and each connection's handler

	// slots counts connections past the handshake (Limits.Conns and
	// ConnsPerPrincipal); writes is each principal's write bucket, and
	// ctlWrites its bucket for writes to the control store; refusals
	// samples refusal log lines.
	slots conns
	// hellos counts each principal's connections between the bridge
	// frame and the hello (Limits.Handshakes).
	hellos    waiting
	writes    buckets
	ctlWrites buckets
	refusals  sampler
	// keys is the state of the keys file (Config.KeysFile).
	keys keysFile
	// adding serializes `project add`, so that two do not take the same
	// database name.
	adding sync.Mutex
	// archiving serializes `project archive` from its check that the
	// project is not the last active one to its control write, so that
	// two archives sent together cannot each see the other's project
	// still active and leave the server with none.
	archiving sync.Mutex
	// unopened holds the active projects whose databases the last
	// OpenProjects could not open, by id, until one opens or is
	// archived; guarded by mu. While it holds any, the upgrade's health
	// probe is refused.
	unopened map[string]store.Project
	// now is the clock for the limits above, and for how long a project
	// store has been idle; tests replace it.
	now func() time.Time
	// started is when the server was made, for its uptime.
	started time.Time
	// clock is what the server has seen of Config.Clock (clock.go).
	clock clockWatch
	// listening is set while Serve accepts connections, and dolt is the
	// last check of Dolt: what /readyz answers from (Ready).
	listening atomic.Bool
	dolt      doltCheck
	// doltm is what was last read of Dolt's own metrics
	// (Config.DoltMetrics), for the status report.
	doltm doltWatch
	// audited is how far the audit stream has read the control store's
	// log (audit.go).
	audited auditor

	// single is the project of a server of one (Config.Store); stores
	// holds the open projects of a server of many (Config.Control).
	// Exactly one is set.
	single *served
	stores *registry
}

var (
	// PrincipalPattern is what a principal name may look like; the store
	// holds the one definition, which mentions and handoffs also use.
	PrincipalPattern = store.PrincipalPattern
	// uuidPattern is a lowercase UUID, as Config.Project must be.
	uuidPattern = lazyregexp.New(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// validActorPart reports whether a session or machine name is 1-255 bytes
// on one line, without control or bidirectional characters (safetext), as
// the store requires of every actor.
func validActorPart(s string) bool { return len(s) <= 255 && s != "" && safetext.ValidLine(s) }

// New checks cfg and returns a Server.
func New(cfg Config) (*Server, error) {
	multi := cfg.Control != nil
	switch {
	case multi && (cfg.Store != nil || cfg.Open == nil):
		return nil, errors.New("server: a control store needs Open, and no Store")
	case !multi && cfg.Store == nil:
		return nil, errors.New("server: no store")
	case !multi && !uuidPattern.MatchString(cfg.Project):
		return nil, fmt.Errorf("server: project %q is not a lowercase UUID", cfg.Project)
	}
	if cfg.ProtoMin == 0 {
		cfg.ProtoMin = proto.ProtoMin
	}
	if cfg.ProtoMax == 0 {
		cfg.ProtoMax = proto.ProtoMax
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.PeerCheck == nil {
		cfg.PeerCheck = CheckPeer
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 60 * time.Second
	}
	if cfg.ReapInterval == 0 {
		cfg.ReapInterval = 30 * time.Second
	}
	if cfg.DoltMetricsEvery == 0 {
		cfg.DoltMetricsEvery = DefaultDoltMetricsEvery
	}
	if err := cfg.Limits.Validate(); err != nil {
		return nil, fmt.Errorf("server: %w", err)
	}
	if cfg.Account == "" {
		cfg.Account = DefaultAccount
	}
	if !store.ValidAccount(cfg.Account) {
		return nil, fmt.Errorf("server: account %q is not a code name", cfg.Account)
	}
	cfg.Limits = cfg.Limits.WithDefaults()
	if err := cfg.ProjectDBs.Validate(); err != nil {
		return nil, fmt.Errorf("server: %w", err)
	}
	if cfg.Bridge == "" {
		cfg.Bridge = DefaultBridge
	}
	if cfg.Clock == nil {
		cfg.Clock = store.NewClock(store.ClockOptions{})
	}
	if cfg.ClockBehind == "" {
		cfg.ClockBehind = ClockStop
	}
	if cfg.BehindTolerance <= 0 {
		cfg.BehindTolerance = DefaultBehindTolerance
	}
	switch {
	case cfg.ClockBehind != ClockStop && cfg.ClockBehind != ClockWarn:
		return nil, fmt.Errorf("server: clock_behind %q is not %s or %s", cfg.ClockBehind, ClockStop, ClockWarn)
	case !ValidBridge(cfg.Bridge):
		return nil, fmt.Errorf("server: the path of starfixd for the forced command, %q, must be absolute, of letters, digits and ._+-", cfg.Bridge)
	case cfg.KeysFile != "" && (!multi || !filepath.IsAbs(cfg.KeysFile)):
		return nil, fmt.Errorf("server: a keys file needs a control store and an absolute path, not %q", cfg.KeysFile)
	case cfg.AuthorizedKeys != "" && !filepath.IsAbs(cfg.AuthorizedKeys):
		return nil, fmt.Errorf("server: the authorized_keys file needs an absolute path, not %q", cfg.AuthorizedKeys)
	case cfg.KeysFile != "" && filepath.Clean(cfg.KeysFile) == filepath.Clean(cfg.AuthorizedKeys):
		return nil, fmt.Errorf("server: the keys file %s is the authorized_keys file, which starfixd never writes", cfg.KeysFile)
	}
	s := &Server{cfg: cfg, conns: map[net.Conn]owner{}, now: time.Now, started: time.Now(), unopened: map[string]store.Project{}}
	s.writes = buckets{rate: cfg.Limits.WriteRate, burst: float64(cfg.Limits.WriteBurst)}
	s.ctlWrites = buckets{rate: cfg.Limits.ControlWriteRate, burst: float64(cfg.Limits.ControlWriteBurst)}
	s.refusals = sampler{n: cfg.Limits.RefusalLogs}
	s.slots.tel = cfg.Telemetry
	if !multi {
		s.single = &served{id: cfg.Project, store: cfg.Store, account: cfg.Account, log: cfg.Logger, timeout: cfg.RequestTimeout, tel: cfg.Telemetry}
		s.single.used.Store(s.now().UnixNano()) // opened now, as openProject notes for a project of many
		cfg.Telemetry.Watch(telemetry.StoresOpen, func() float64 { return float64(s.OpenStores()) })
		if err := s.startClock([]*behindLog{s.startBehind(s.single.logName(), cfg.Store.ClockState())}); err != nil {
			return nil, err
		}
		return s, nil
	}
	cfg.Control.KeysElsewhere(s.handOwner)
	s.stores = &registry{open: s.openProject, max: cfg.Limits.OpenStores, idle: time.Duration(cfg.Limits.StoreIdle),
		now: func() time.Time { return s.now() }, entries: map[string]*entry{}}
	// An archive ends every claim in the project while requests may
	// still be in flight on the connections it closes, and one of those
	// may take a claim after. No tick sweeps a closed store, so an
	// archived project's is swept as it closes (decision 21). Each
	// write is bounded by RequestTimeout.
	s.stores.retiring = func(pr *served) { s.sweep(context.Background(), pr) }
	cfg.Telemetry.Watch(telemetry.StoresOpen, func() float64 { return float64(s.OpenStores()) })
	// The audit stream starts at the newest event there is now.
	s.auditStart(context.Background())
	return s, nil
}

// openProject opens p's store for the registry. Before the store serves
// a request it ends the claims that lapsed and prunes what aged while it
// was closed: a closed store has no reaper.
func (s *Server) openProject(ctx context.Context, p store.Project) (*served, error) {
	st, err := s.openStore(ctx, p)
	if err != nil {
		return nil, err
	}
	pr := &served{id: p.ID, db: p.DBName, store: st, account: p.Account, log: s.cfg.Logger.With("project", p.ID), timeout: s.cfg.RequestTimeout,
		tel: s.cfg.Telemetry}
	pr.used.Store(s.now().UnixNano())
	pr.log.InfoContext(ctx, "project store opened", "event", logs.ProjectOpened, "name", p.Name, "database", p.DBName)
	s.mu.Lock()
	delete(s.unopened, p.ID)
	s.mu.Unlock()
	pr.reap(ctx)
	s.sweep(ctx, pr)
	pr.prune(ctx, s.cfg.Limits)
	return pr, nil
}

// openStore opens p's store with Config.Open, within RequestTimeout. It
// is a store.open span, with the store's migrations beneath it, and one
// observation of starfix.store.open.duration when it succeeds.
func (s *Server) openStore(ctx context.Context, p store.Project) (*store.Store, error) {
	ctx, span := s.cfg.Telemetry.Start(ctx, telemetry.SpanStoreOpen)
	defer span.End()
	span.Set(telemetry.KeyProject, p.ID)
	octx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	start := time.Now()
	st, err := s.cfg.Open(octx, p)
	if err != nil {
		span.Fail(telemetry.OutcomeError)
		return nil, err
	}
	migrated := strconv.FormatBool(st.Migrated() > 0)
	span.Set(telemetry.KeyMigrated, migrated)
	s.cfg.Telemetry.Since(telemetry.StoreOpenDuration, start, migrated)
	return st, nil
}

// each runs fn for the server's project, or for every project whose
// store is open.
func (s *Server) each(fn func(*served)) {
	if s.single != nil {
		fn(s.single)
		return
	}
	s.stores.each(fn)
}

// eachAtOnce is each with fn run for every open store at the same time.
func (s *Server) eachAtOnce(fn func(*served)) {
	if s.single != nil {
		fn(s.single)
		return
	}
	s.stores.atOnce(fn)
}

// CloseIdleStores closes the store of every project that has had no
// connection for Limits.StoreIdle, as the reaper does on each tick. It
// gives back connections to Dolt and nothing else. A server of one
// project has nothing to close.
func (s *Server) CloseIdleStores() {
	if s.stores != nil {
		s.stores.closeIdle()
	}
}

// OpenStores returns how many project stores a server of many projects
// holds open, and 1 for a server of one.
func (s *Server) OpenStores() int {
	if s.stores == nil {
		return 1
	}
	return s.stores.size()
}

// refresh reads the control store's index again, so that what the
// operator's `starfixd control init` wrote beside the daemon counts
// without a restart, and writes the keys file if it no longer holds what
// the index does: after such a change, after a write that failed, or
// after someone edited the file.
func (s *Server) refresh(ctx context.Context) {
	if s.cfg.Control == nil {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	rctx, span := s.cfg.Telemetry.Start(rctx, telemetry.SpanControlReload)
	if err := s.cfg.Control.Reload(rctx); err != nil {
		span.Fail(telemetry.OutcomeError)
		if ctx.Err() == nil {
			s.cfg.Logger.ErrorContext(rctx, "reload the control store", "event", logs.ControlReload, "err", err)
		}
	}
	span.End()
	s.syncKeys(ctx)
}

// Serve accepts connections on l until ctx is done, then closes l and every
// open connection, waits for their handlers and the reaper to return,
// closes the project stores it opened (Config.Control), and returns nil. An Accept error that may pass, such as running out of file
// descriptors, is logged at warn and retried after a pause that doubles
// from 5ms to at most a second. Any other Accept error ends Serve the same
// way, and Serve returns it.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	closed := make(chan struct{})
	context.AfterFunc(ctx, func() {
		defer close(closed)
		_ = l.Close()
		s.mu.Lock()
		for c := range s.conns {
			_ = c.Close()
		}
		s.mu.Unlock()
	})
	// However Serve returns, cancel closes l and the connections, which
	// ends the handlers, and stops the reaper; only then can they be
	// waited for.
	defer func() {
		cancel()
		<-closed
		s.wg.Wait()
		if s.stores != nil {
			s.stores.closeAll()
		}
	}()
	if s.cfg.ReapInterval > 0 {
		s.wg.Go(func() { s.reapLoop(ctx) })
	}
	if s.cfg.DoltMetrics != nil && s.cfg.DoltMetricsEvery > 0 {
		s.wg.Go(func() { s.doltLoop(ctx) })
	}
	s.listening.Store(true)
	defer s.listening.Store(false)
	var pause time.Duration
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if !transient(err) {
				return fmt.Errorf("accept: %w", err)
			}
			pause = min(max(2*pause, 5*time.Millisecond), time.Second)
			s.cfg.Logger.Warn("accept failed; retrying", "event", logs.ConnAccept, "err", err, "after", pause)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(pause):
			}
			continue
		}
		pause = 0
		s.mu.Lock()
		if ctx.Err() != nil {
			// Accepted as Serve stops: the loop that closes the connections
			// may have run already, and would never close this one.
			s.mu.Unlock()
			_ = c.Close()
			return nil
		}
		// At twice the cap, the connection that has waited longest
		// without a hello is closed to make room: whoever arrives now is
		// more likely to be someone at work than one that has said
		// nothing for longest. The cap itself is refused cleanly, in the
		// handshake. Connections past their hello number at most Conns,
		// so one without is always there to close.
		closing := c
		if len(s.conns) < 2*s.cfg.Limits.Conns {
			closing = nil
		} else if oldest := s.oldestWaiting(); oldest != nil {
			closing = oldest
			delete(s.conns, oldest)
		}
		if closing != c {
			s.conns[c] = owner{since: time.Now()}
		}
		s.mu.Unlock()
		if closing != nil {
			s.refusals.line(ctx, s.cfg.Logger, "", s.now(), slog.LevelWarn, logs.ConnCapRefused, "closed a connection without a hello", "reason", "connection limit", "oldest", closing != c)
			s.cfg.Telemetry.Count(telemetry.LimitRefusals, telemetry.LimitSockets)
			s.cfg.Telemetry.Count(telemetry.ConnectionsDropped, telemetry.DropLimit)
			_ = closing.Close() // an older one's handler ends with it, and frees what it held
		}
		if closing == c {
			continue
		}
		s.wg.Go(func() {
			defer func() {
				s.mu.Lock()
				delete(s.conns, c)
				s.mu.Unlock()
				_ = c.Close()
			}()
			s.handle(ctx, c)
		})
	}
}

// transient reports whether an Accept error may pass: the process or the
// system is out of file descriptors or memory, or a connection went away
// while it waited to be accepted. net.Error's Temporary, which net/http
// asks, is deprecated, so the errors are named.
func transient(err error) bool {
	for _, errno := range []syscall.Errno{syscall.EMFILE, syscall.ENFILE, syscall.ENOBUFS, syscall.ENOMEM,
		syscall.ECONNABORTED, syscall.ECONNRESET} {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}

// pruneEvery is how often the reaper also prunes the registry and the
// inbox.
const pruneEvery = 10 * time.Minute

// reapLoop ends expired claims every ReapInterval until ctx is done. It
// prunes on the first tick, then every pruneEvery. A server of many
// projects also closes its idle stores, rereads the control store,
// writes the keys file if it is out of date, and ends the claims of
// holders who lost their access (Sweep), on each tick.
func (s *Server) reapLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.ReapInterval)
	defer t.Stop()
	// beat stays nil, and so never fires, with no heartbeat to send.
	var beat <-chan time.Time
	if s.cfg.Heartbeat != nil && s.cfg.HeartbeatEvery > 0 {
		bt := time.NewTicker(s.cfg.HeartbeatEvery)
		defer bt.Stop()
		beat = bt.C
	}
	var pruned time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-beat:
			s.cfg.Heartbeat()
		case <-t.C:
			prune := time.Since(pruned) >= pruneEvery
			s.tick(ctx, prune)
			if prune {
				pruned = time.Now()
			}
		}
	}
}

// tick is one tick of the reaper: it looks at the clock, rereads the
// control store and writes the keys file if it is out of date, closes
// idle stores, ends expired claims and those of holders who lost their
// access, and with prune set also prunes. The tick is one trace: the
// reaper's span, with the parts that have a span of their own beneath
// it, so a slow tick shows which part was slow.
func (s *Server) tick(ctx context.Context, prune bool) {
	start := time.Now()
	ctx, span := s.cfg.Telemetry.Start(ctx, telemetry.SpanReaper)
	// A key for stand-ins that could not be read or made at start is
	// looked for again: making its directory is all an operator does.
	s.cfg.Telemetry.RetryKey()
	s.CheckClock(ctx)
	s.refresh(ctx)
	// What was written to the control store beside the daemon reaches
	// the audit stream here.
	s.Audit(ctx)
	s.CloseIdleStores()
	s.Reap(ctx)
	s.Sweep(ctx)
	if prune {
		s.Prune(ctx)
	}
	span.End()
	s.cfg.Telemetry.Since(telemetry.ReaperDuration, start)
	s.cfg.Telemetry.Record(telemetry.ReaperLastRun, float64(time.Now().Unix()))
}

// Prune deletes registry rows and read inbox items older than the limits
// keep them (S-6, S-7), in every open store, and logs what it removed.
func (s *Server) Prune(ctx context.Context) {
	s.each(func(pr *served) { pr.prune(ctx, s.cfg.Limits) })
}

// Reap ends expired claims now, in every open store, and logs each.
func (s *Server) Reap(ctx context.Context) {
	s.each(func(pr *served) { pr.reap(ctx) })
}

// Handshake timeouts: the bridge frame comes at once, from `starfixd
// stdio` on this machine; the hello follows the SSH session start.
//
// helloTimeout is how long a connection that has said who it is may hold
// a socket, and a place among its principal's Limits.Handshakes, without
// saying what it wants. It was 30 seconds, which nothing needed. The
// bridge starts when sshd answers the client's exec request, and the
// client (client.start) writes its hello as soon as that answer reaches
// it, so the hello trails the bridge frame by one round trip. The client
// gives its whole connection 15 seconds (client.Options.Timeout, which
// no caller raises), and the TCP connect, key exchange, authentication,
// channel and exec before the hello cost at least five round trips: a
// client whose round trip is over 2.5 seconds has given up before its
// hello could arrive. Ten seconds is four times that, so it closes no
// connection a client is still waiting on, through a slow tunnel
// included.
const (
	bridgeTimeout = 5 * time.Second
	helloTimeout  = 10 * time.Second
)

// staleKeysFrame is the type of the first frame `starfixd keys` sent,
// when sshd asked the daemon about each key through that command. The
// command is gone (design §14, decision 22) and was never in a release.
// A root-owned copy of an unreleased build may still be some sshd's
// AuthorizedKeysCommand while its server is upgraded, so its question is
// expected: the connection is closed without an answer, which that copy
// reads as no key, and nothing is logged above debug.
const staleKeysFrame = "keys"

// session is one connection's state, from the handshake on.
type session struct {
	// actor is the principal from the bridge frame, and the session and
	// machine from the hello.
	actor store.Actor
	// harness is what the hello said the client runs under, unchecked.
	harness string
	// enc is shared by the request loop and the pusher; only the request
	// loop reads dec.
	enc *proto.Encoder
	dec *proto.Decoder

	// push is the running watch, if the client sent watch. Only the
	// request loop sets it; pushMu orders pushed frames against the
	// loop's flush before each response.
	push   *pusher
	pushMu sync.Mutex
	// slot is set when the session holds a connection slot (s.slots).
	slot bool
	// conn is the session's connection. ended is set, by the request
	// loop alone, when a request of its own ended its access: the
	// connection closes once the reply is sent.
	conn  net.Conn
	ended bool
	// key is what the session's connection slot, write bucket and
	// refusal log lines are counted under (limitKey).
	key string
	// fingerprint is the bridge frame's key: the fingerprint of the key
	// that authenticated, when sshd took its line from the keys file, and
	// empty for a line of the hand-maintained authorized_keys file.
	fingerprint string
	// proto is the protocol version the hello gave, once it was read; a
	// span names it.
	proto int
	// proj is the project the hello named. held is its entry in the
	// registry of a server of many projects, set while the session
	// counts as one of the store's connections.
	proj *served
	held *entry
	// statusBy is when the status report being answered stops waiting
	// on Dolt: the request loop sets it before each status request.
	statusBy time.Time
}

// watching reports whether sess's watch is running, which exempts it
// from the idle timeout.
func (sess *session) watching() bool {
	p := sess.push
	if p == nil {
		return false
	}
	sess.pushMu.Lock()
	defer sess.pushMu.Unlock()
	return !p.ended
}

// handle serves one connection: it checks the peer and runs the
// handshake, then answers requests in order until the client leaves, the
// connection idles out, a read or write fails, or ctx is done. The
// connection's pusher, if it watched, has ended when handle returns. The
// caller closes c.
func (s *Server) handle(ctx context.Context, c net.Conn) {
	log := s.cfg.Logger
	if err := s.cfg.PeerCheck(c); err != nil {
		s.refusals.line(ctx, log, "", s.now(), slog.LevelWarn, logs.ConnPeerRefused, "refused socket peer", "err", err)
		return
	}
	// The handshake is a trace of its own: a hello brings no trace
	// context. Opening the project's store, when this connection is the
	// one that does, and noting the session in the registry show
	// beneath it. The span ends before the first request is read, and
	// so before the note has been made, which runs beside that read;
	// the deferred End is for the returns before that, and does nothing
	// once the span has ended.
	hctx, hspan := s.cfg.Telemetry.StartServer(ctx, telemetry.SpanHandshake)
	defer hspan.End()
	sess, err := s.handshake(hctx, c)
	s.observeHandshake(hspan, sess, err)
	if sess != nil && sess.slot {
		defer s.slots.release(sess.key)
	}
	if sess != nil && sess.held != nil {
		defer s.stores.release(sess.held)
	}
	if err != nil {
		key := ""
		if sess != nil {
			key = sess.key
		}
		s.refusals.line(hctx, log, key, s.now(), slog.LevelInfo, logs.HandshakeFailed, "handshake failed", "err", err)
		return
	}
	if sess == nil {
		return // a stale key lookup, closed unanswered
	}
	if sess.proj == nil {
		// The health probe of a server of many projects: the welcome was
		// its answer. `starfixd status` asks on.
		hspan.End()
		s.serveStatus(ctx, sess)
		return
	}
	principal, key := sess.actor.Principal, sess.key
	if s.stores != nil {
		log = log.With("project", sess.proj.id)
	}
	log = log.With("principal", principal, "session", sess.actor.Session, "machine", sess.actor.Machine)
	log.DebugContext(hctx, "connected", "event", logs.ConnOpened)
	defer func() {
		_ = c.Close() // unblocks a pusher stuck writing to a client that stopped reading
		s.unwatch(sess)
	}()
	defer s.untrack(c)
	if !s.track(c, sess) {
		log.InfoContext(hctx, "connection closed: access ended during the handshake", "event", logs.ConnAccessEnded)
		s.cfg.Telemetry.Count(telemetry.ConnectionsDropped, telemetry.DropAccess)
		return
	}
	// The session is noted for who beside the read of the first request,
	// not before it: the note waits on Dolt for as long as a request
	// may, and each request waits for it only as long as it may itself.
	noted := s.present(hctx, log, sess)
	defer noted.end()
	hspan.End()
	idle := time.Duration(s.cfg.Limits.IdleTimeout)
	for {
		if sess.watching() {
			_ = c.SetReadDeadline(time.Time{})
		} else {
			_ = c.SetReadDeadline(s.now().Add(idle))
		}
		f, err := sess.dec.Decode()
		if err != nil {
			var ne net.Error
			switch {
			case errors.Is(err, io.EOF) || ctx.Err() != nil:
			case errors.As(err, &ne) && ne.Timeout():
				log.Debug("idle connection closed", "event", logs.ConnIdleClosed, "after", idle)
				s.cfg.Telemetry.Count(telemetry.ConnectionsDropped, telemetry.DropIdle)
				_ = s.send(sess, 0, nil, proto.Errf(proto.CodeUnavailable, "reconnect; starfix does so on the next command",
					fmt.Sprintf("connection closed after %s idle", proto.Span(idle))))
			default:
				s.refusals.line(ctx, log, key, s.now(), slog.LevelInfo, logs.ConnReadFailed, "read failed", "err", err)
				if errors.Is(err, proto.ErrFrameTooLarge) {
					_ = s.send(sess, 0, nil, proto.Errf(proto.CodeInvalid, "send large text in smaller parts",
						"request larger than 4 MiB"))
				}
			}
			return
		}
		if f.T != proto.FrameReq {
			_ = s.send(sess, f.ID, nil, proto.Errf(proto.CodeInvalid, proto.FixUpgrade+" starfix to match the server",
				fmt.Sprintf("unexpected %q frame after the handshake", f.T)))
			return
		}
		start := time.Now()
		sess.proj.used.Store(s.now().UnixNano())
		// The request's id is on every line logged for it, here and in
		// the store's hooks, and in the refusal the client is sent, so
		// that a person's report finds the server's lines.
		req := logs.NewReq()
		rctx, cancel := context.WithTimeout(logs.WithReq(ctx, req), s.cfg.RequestTimeout)
		// The caller's trace context rides the request's context. One
		// that does not parse is ignored: it never fails the request,
		// and nothing of it is logged.
		tc, traced := proto.ParseTraceContext(f.Traceparent, f.Tracestate)
		if traced {
			rctx = withTrace(rctx, tc)
		}
		// The request's server span, named for its op: a child of the
		// caller's span when the frame brought one, and the parent of
		// whatever the store does for the request. It ends beside
		// observeRequest below, which measures the same stretch. With
		// traces off it is the zero Span, and costs nothing.
		rctx, span := s.cfg.Telemetry.StartRequest(rctx, f.Op, tc)
		var res any
		var perr *proto.Error
		ok, wait := true, time.Duration(0)
		switch {
		case controlWrites[f.Op]:
			// The control store is the whole server's, so its bucket is
			// the principal's, whichever project the connection is to.
			if ok, wait = s.ctlWrites.take(principal, s.now()); !ok {
				perr = s.busyControlWrites(principal, wait)
			}
		case !readOps[f.Op]:
			if ok, wait = s.writes.take(key, s.now()); !ok {
				perr = s.busyWrites(principal, wait)
			}
		}
		switch {
		case !ok:
		case f.Op == proto.OpStatus:
			// The report counts the sessions seen, its asker's among
			// them, so it too comes after the note; but it waits on
			// Dolt for statusReadTimeout in all, and its own reads have
			// what the wait for the note leaves of that.
			sess.statusBy = time.Now().Add(statusReadTimeout)
			wctx, done := context.WithDeadline(rctx, sess.statusBy)
			noted.ensure(wctx)
			done()
		default:
			// Every other op reads or writes a store, the project's or
			// the control store, and comes after the note of its
			// session, however long that takes of the request's time.
			noted.ensure(rctx)
		}
		switch {
		case !ok:
		case f.Op == proto.OpWatch:
			res, perr = s.watch(rctx, sess, f.Args)
		case adminOp(f.Op):
			res, perr = s.admin(rctx, sess, f.Op, f.Args)
		default:
			res, perr = sess.proj.dispatch(rctx, sess.actor, f.Op, f.Args)
		}
		cancel()
		if ok && controlWrites[f.Op] {
			// The change has committed, or was refused: its line of the
			// audit stream, if it made an event, precedes its answer.
			s.Audit(ctx)
		}
		if perr != nil {
			// A copy: a refusal may be a value shared between requests.
			named := *perr
			named.Req = req
			perr = &named
		}
		s.observeRequest(sess, f.Op, perr, start)
		s.endRequest(span, sess, perr)
		if p := sess.push; p != nil {
			s.flush(sess, p) // what was committed before this answer goes first
		}
		// The line's req and the ids of the request's span, sampled or
		// not, come from rctx through the log's handler; with traces off
		// the ids are the caller's, hex and checked, and span_id the
		// span that sent the request (LogIDs).
		ms := time.Since(start).Milliseconds()
		if perr != nil {
			// A refusal is worth seeing at the default level, sampled per
			// principal (S-20); a success is one line per request, so only
			// at debug.
			s.refusals.line(rctx, log, key, s.now(), slog.LevelInfo, logs.RequestRefused, "request", "op", f.Op, "dur_ms", ms, "code", string(perr.Code))
		} else {
			log.DebugContext(rctx, "request", "event", logs.RequestDone, "op", f.Op, "dur_ms", ms)
		}
		if err := s.send(sess, f.ID, res, perr); err != nil {
			log.InfoContext(rctx, "write failed", "event", logs.ConnWriteFailed, "err", err)
			return
		}
		if sess.ended {
			log.InfoContext(rctx, "connection closed: the request ended its own access", "event", logs.ConnAccessEnded)
			return
		}
	}
}

// send writes the response to request id: perr when it is set, else res.
// A result that cannot be encoded is answered with unavailable, and one
// larger than proto.MaxFrame with invalid, so the client gets an answer
// rather than a dropped connection. The error is the write's.
func (s *Server) send(sess *session, id uint64, res any, perr *proto.Error) error {
	f, err := proto.Response(id, res, perr)
	if err != nil {
		f, _ = proto.Response(id, nil, proto.Errf(proto.CodeUnavailable, "retry; if it persists, report it", "server could not encode the result"))
	}
	err = sess.enc.Encode(f)
	if errors.Is(err, proto.ErrFrameTooLarge) {
		// Paged reads keep replies well under the limit; should one still
		// pass it, refuse the request rather than drop the connection.
		f, _ = proto.Response(id, nil, proto.Errf(proto.CodeInvalid, "ask for less: a smaller --limit, or page with --before",
			"the result is larger than 4 MiB"))
		err = sess.enc.Encode(f)
	}
	return err
}

// busyWrites refuses a write over principal's rate, saying when to retry.
func (s *Server) busyWrites(principal string, wait time.Duration) *proto.Error {
	l := s.cfg.Limits
	s.cfg.Telemetry.Count(telemetry.LimitRefusals, telemetry.LimitWriteRate)
	return proto.Errf(proto.CodeBusy, fmt.Sprintf("wait %s and retry", wait),
		fmt.Sprintf("%s is over the write limit (%g a second, bursts of %d)", principal, l.WriteRate, l.WriteBurst))
}

// busyControlWrites refuses a write to the control store over
// principal's rate, saying when to retry.
func (s *Server) busyControlWrites(principal string, wait time.Duration) *proto.Error {
	l := s.cfg.Limits
	s.cfg.Telemetry.Count(telemetry.LimitRefusals, telemetry.LimitControlWriteRate)
	return proto.Errf(proto.CodeBusy,
		fmt.Sprintf("wait %s and retry; the server's operator can raise control_write_rate and control_write_burst under limits: in starfixd's config", wait),
		fmt.Sprintf("%s is over the limit on access changes (%g a second, bursts of %d)", principal, l.ControlWriteRate, l.ControlWriteBurst))
}

// handshake reads the bridge frame and the client's hello, and answers with
// a welcome, or a refusal and an error. A connection whose first frame is
// a keys frame is closed unanswered, and nothing is returned
// (staleKeysFrame). On success the session holds a
// connection slot (sess.slot) and, in a server of many projects, counts
// as a connection of its project's store (sess.held). A refusal after a
// valid bridge frame also returns the session, so the caller can say
// whose it was and free what it holds, and so do a hello that did not
// come and a welcome that could not be written; other failures return
// nil. ctx bounds the opening of a project's store.
//
// Two things are decided on the bridge frame alone, before the hello is
// waited for (standing, Limits.Handshakes): a server of many projects
// refuses a principal it would let into no project, and any server
// refuses one with too many connections that have not said hello. What
// depends on the project, membership and the key that signed in, is
// decided at the hello (admit, keyHolds).
func (s *Server) handshake(ctx context.Context, c net.Conn) (*session, error) {
	sess := &session{conn: c, enc: proto.NewEncoder(c), dec: proto.NewDecoder(c)}
	_ = c.SetReadDeadline(time.Now().Add(bridgeTimeout))
	f, err := sess.dec.Decode()
	if err != nil {
		return nil, fmt.Errorf("bridge frame: %w", err)
	}
	if f.T == staleKeysFrame {
		s.cfg.Logger.DebugContext(ctx, "key lookup closed unanswered: keys reach sshd by the keys file now", "event", logs.ConnKeyLookup)
		return nil, nil
	}
	// The upgrade's probe and `starfixd status`, alone of the reserved
	// names, are welcomed: they connect here directly, and Bridge refuses
	// their names to every key.
	if f.T != proto.FrameBridge || !PrincipalPattern.MatchString(f.Principal) ||
		(store.Reserved(f.Principal) && f.Principal != ProbePrincipal && f.Principal != StatusPrincipal) {
		return nil, fmt.Errorf("first frame is %q, not a bridge frame with a valid, unreserved principal", f.T)
	}
	// A line of the hand-maintained authorized_keys file names none.
	if f.Key != "" && !store.ValidFingerprint(f.Key) {
		return nil, errors.New("the bridge frame's key is not a SHA256 fingerprint")
	}
	sess.actor.Principal = f.Principal
	sess.key = f.Principal
	sess.fingerprint = f.Key

	w := &proto.Frame{T: proto.FrameWelcome, Version: s.cfg.Version, Min: s.cfg.ProtoMin, Max: s.cfg.ProtoMax, Latest: s.cfg.Latest}
	// A refusal past the bridge frame returns sess, so the caller knows
	// whose it is.
	refuse := func(e *proto.Error) (*session, error) {
		w.Err = e
		return sess, errors.Join(e, sess.enc.Encode(w))
	}
	// From here the connection is somebody's, so it is refused or
	// counted as theirs before it is waited on. The upgrade's probe and
	// `starfixd status` are the daemon's own, and neither.
	own := f.Principal == ProbePrincipal || f.Principal == StatusPrincipal
	if !own {
		if e := s.standing(f.Principal); e != nil {
			return refuse(e)
		}
		if most := s.cfg.Limits.Handshakes; !s.hellos.enter(f.Principal, most) {
			s.cfg.Telemetry.Count(telemetry.LimitRefusals, telemetry.LimitHandshakes)
			return refuse(proto.Errf(proto.CodeBusy,
				"wait for your other starfix sessions to finish connecting, or close them, and retry; the server's operator can raise handshakes_per_principal under limits: in starfixd's config",
				fmt.Sprintf("%s has %d connections that have not said hello, the most a principal may (%d)", f.Principal, most, most)))
		}
	}
	_ = c.SetReadDeadline(time.Now().Add(helloTimeout))
	f, err = sess.dec.Decode()
	if !own {
		s.hellos.leave(sess.actor.Principal)
	}
	if err != nil {
		return sess, fmt.Errorf("hello: %w", err)
	}
	_ = c.SetReadDeadline(time.Time{})
	s.greeted(c)
	if f.T != proto.FrameHello {
		return refuse(proto.Errf(proto.CodeInvalid, proto.FixUpgrade+" starfix to match the server",
			fmt.Sprintf("expected a hello, got a %q frame", f.T)))
	}
	// The health check of `starfixd upgrade` is run by the binary being
	// replaced, so it speaks that binary's protocol. It asks only whether
	// this daemon answers and sends no request, so it is welcomed whatever
	// it speaks: otherwise no upgrade could cross a raised ProtoMin, since
	// the old binary would take the refusal for a daemon that failed, and
	// put itself back. Only the daemon's own user can be the probe.
	if sess.actor.Principal != ProbePrincipal {
		if e := proto.CheckProto(f.Proto, s.cfg.ProtoMin, s.cfg.ProtoMax, s.cfg.Version); e != nil {
			return refuse(e)
		}
		sess.proto = f.Proto
	}
	if sess.actor.Principal == StatusPrincipal {
		// `starfixd status` asks about the server, whichever project it
		// names, and holds no connection slot (serveStatus). Unlike the
		// probe it sends a request and reads a result, so it has passed
		// the protocol check above: a `starfixd status` of another
		// version is refused with `version`, as any client is.
		w.Session, w.Principal = statusSession, StatusPrincipal
		return sess, sess.enc.Encode(w)
	}
	var row store.Project
	if s.stores == nil {
		if f.Project != s.cfg.Project {
			return refuse(notServed(f.Project))
		}
		sess.proj = s.single
	} else {
		if sess.actor.Principal == ProbePrincipal {
			// The upgrade's health check asks about the server, not
			// about a project: whichever it names, archived or not.
			if e := s.unopenedErr(); e != nil {
				return refuse(e)
			}
			w.Session, w.Principal = "upgrade-probe", ProbePrincipal
			return sess, sess.enc.Encode(w)
		}
		acc, e := s.admit(sess.actor.Principal, f.Project)
		if e != nil {
			return refuse(e)
		}
		if e := s.keyHolds(sess.actor.Principal, sess.fingerprint); e != nil {
			return refuse(e)
		}
		row = acc.Project
		// Each project has its own allowance of a principal's
		// connections, writes and refusal lines.
		sess.key = limitKey(row.ID, sess.actor.Principal)
	}
	sess.actor.Session = f.Session
	if sess.actor.Session == "" {
		sess.actor.Session = newSessionID()
	}
	sess.actor.Machine = f.Machine
	if sess.actor.Machine == "" {
		sess.actor.Machine = "unknown"
	}
	if !validActorPart(sess.actor.Session) || !validActorPart(sess.actor.Machine) {
		return refuse(proto.Errf(proto.CodeInvalid, "set STARFIX_SESSION to 1-255 printable characters",
			"session and machine must be 1-255 printable characters, without control or bidirectional characters"))
	}
	sess.harness = f.Harness
	if ok, why := s.slots.acquire(sess.key, s.cfg.Limits.Conns, s.cfg.Limits.ConnsPerPrincipal); !ok {
		return refuse(proto.Errf(proto.CodeBusy, "close other starfix sessions, or wait for them to end, and retry", why))
	}
	sess.slot = true
	if s.stores != nil {
		if e := s.silentDolt(ctx, sess.key, row); e != nil {
			return refuse(e)
		}
		held, err := s.stores.acquire(ctx, row)
		switch {
		case errors.Is(err, errStoresFull):
			s.cfg.Telemetry.Count(telemetry.LimitRefusals, telemetry.LimitOpenStores)
			return refuse(proto.Errf(proto.CodeBusy,
				"retry in a minute; the server admin can raise open_stores under limits: in starfixd's config",
				fmt.Sprintf("the server has its limit of %d projects open, each with a connection", s.cfg.Limits.OpenStores)))
		case err != nil:
			if ctx.Err() == nil {
				s.cfg.Logger.ErrorContext(ctx, "open project store", "event", logs.ProjectOpenFailed, "project", row.ID, "database", row.DBName, "err", err)
			}
			return refuse(proto.Errf(proto.CodeUnavailable, "retry; if it persists, the server admin should check the starfixd log",
				fmt.Sprintf("the server could not open project %q", row.ID)))
		}
		sess.held, sess.proj = held, held.pr
	}
	w.Session = sess.actor.Session
	w.Principal = sess.actor.Principal
	if err := sess.enc.Encode(w); err != nil {
		return sess, err // sess holds a slot, which the caller frees
	}
	return sess, nil
}

// silentDolt refuses a handshake that would open p's store while Dolt
// does not answer a ping within doltCheckTimeout, and returns nil when
// the store is open or being opened already, or Dolt answers. Opening a
// store waits on Dolt for as long as a request may (openStore), and the
// connection that waits is as likely an admin's, come to ask the
// server's status, which opens no store: it is told now what is wrong,
// and where the status can still be read. The ping is this handshake's
// own, not the one /readyz shares (checkDolt), whose answer may be five
// seconds old. key is what the log line is sampled under.
func (s *Server) silentDolt(ctx context.Context, key string, p store.Project) *proto.Error {
	if s.stores.has(p.ID) {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, doltCheckTimeout)
	defer cancel()
	err := s.cfg.Control.Ping(cctx)
	if err == nil || ctx.Err() != nil {
		return nil // Dolt answers, or the daemon is stopping, which the opening says
	}
	s.refusals.line(ctx, s.cfg.Logger, key, s.now(), slog.LevelError, logs.DoltCheckFailed, "the check of Dolt failed; project store not opened",
		"project", p.ID, "database", p.DBName, "err", err)
	return proto.Errf(proto.CodeUnavailable,
		"retry in a minute; if it persists, the server's operator should run `starfixd status` on the server, which answers without the database, and see to Dolt (docs/server.md, Troubleshooting)",
		fmt.Sprintf("the server's database does not answer, so project %q was not opened", p.ID))
}

// notServed refuses a hello for a project the server does not serve.
func notServed(project string) *proto.Error {
	return proto.Errf(proto.CodeNotFound, "check `project` in .starfix.yaml, or the server address",
		fmt.Sprintf("project %q is not served by this server", project))
}

// disabledErr refuses a principal whose row is disabled.
func disabledErr(principal string) *proto.Error {
	return proto.Errf(proto.CodeForbidden,
		fmt.Sprintf("ask a server admin, or a project admin who administers every project you belong to, to run `sfx admin principal enable %s`", principal),
		fmt.Sprintf("%s is disabled on this server", principal))
}

// standing refuses, on the bridge frame alone, a principal that a server
// of many projects lets into no project: one whose row is disabled, a
// server admin's included (decision 20), and one that has no row and is
// no server admin in the config file. Its key is still a line sshd
// accepts, so without this it could hold connections open until each
// hello timed out. A server of one project has no control store, and
// refuses no one here.
func (s *Server) standing(principal string) *proto.Error {
	if s.cfg.Control == nil {
		return nil
	}
	switch known, disabled := s.cfg.Control.Standing(principal); {
	case disabled:
		return disabledErr(principal)
	case !known:
		return proto.Errf(proto.CodeForbidden,
			fmt.Sprintf("ask a server admin, or an admin of the project, to add you: `sfx admin principal add %s`, run in the project's repository", principal),
			fmt.Sprintf("%s is not a principal of this server", principal))
	}
	return nil
}

// admit decides a handshake in a server of many projects, from the
// control store's memory. A project that is unknown, archived or still
// being created is not served to anyone. An active one is served to a
// server admin and to an active principal who is a member of it; anyone
// else is forbidden, whether they are disabled, in other projects only,
// or unknown to the store. A disabled principal is refused even when
// the config file names it a server admin (decision 20). The upgrade's
// health probe never comes here: the handshake answers it first.
func (s *Server) admit(principal, project string) (store.Access, *proto.Error) {
	acc := s.cfg.Control.Access(principal, project)
	if !acc.Active {
		return acc, notServed(project)
	}
	if acc.Disabled {
		return acc, disabledErr(principal)
	}
	if acc.Role == "" {
		return acc, proto.Errf(proto.CodeForbidden,
			fmt.Sprintf("ask a server admin, or one of the project's admins, to add you: `sfx admin member add %s`, run in this repository", principal),
			fmt.Sprintf("%s is not a member of project %q", principal, project))
	}
	return acc, nil
}

// limitKey is what a principal's connections, writes and refusal log
// lines are counted under in a server of many projects: the project and
// the principal, so that each project has its own allowance. A server of
// one project counts under the principal alone.
func limitKey(project, principal string) string { return project + "/" + principal }

// keyPrincipal returns the principal a limit key names.
func keyPrincipal(key string) string {
	if _, p, ok := strings.Cut(key, "/"); ok {
		return p
	}
	return key
}

var sessionEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// newSessionID returns a session id for a client that sent none: "s-" and
// 16 lowercase base32 characters, 80 random bits.
func newSessionID() string {
	var b [10]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	return "s-" + sessionEncoding.EncodeToString(b[:])
}
