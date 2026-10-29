// Command ariesd is the Aries server.
//
//	ariesd serve [--dev [--allow-unsafe-account]] [--config FILE] [--dsn DSN] [--socket PATH] [--project UUID] [--prefix P] [--log-level L] [--log-format F]
//	ariesd stdio --principal NAME [--key FINGERPRINT] [--config FILE] [--socket PATH]
//	ariesd control init --name NAME [--authorized-keys FILE] [--restore-removed-access] [--config FILE] [--dsn DSN]
//	ariesd control invite --provider P [--config FILE] [--dsn DSN] NAME ADDRESS
//	ariesd import-bd [--config FILE] [--dsn DSN] [--project UUID] [--principal NAME] [--dry-run] [--json] FILE|-
//	ariesd export-bd [--config FILE] [--dsn DSN] [--project UUID] [-o FILE]
//	ariesd upgrade [--check] [--to vX.Y.Z] [--rollback] [--restart] [--dump FILE] [--config FILE] [--dsn DSN] [--socket PATH]
//	ariesd migrations
//	ariesd status [--json] [--config FILE] [--socket PATH]
//	ariesd version
//
// serve is the long-running daemon: it owns the Postgres store and serves
// the protocol on a unix socket. stdio is the forced command in each developer
// key's authorized_keys line; it bridges the SSH session to that socket.
// import-bd and export-bd are admin commands run on the server: they open
// the store directly with the daemon's DSN settings and move a backlog in
// and out as bd's JSONL. With control_schema set the server holds many
// projects, each in a schema of its own: control init adopts the
// existing schema as the first and imports authorized_keys, control
// invite invites an address for a principal as the operator (how a
// lone server admin gets an identity), import-bd
// and export-bd take --project, and serve writes the keys the control
// store holds to a file sshd reads (keys_file) and opens every active
// project's schema once before it listens, which migrates it. upgrade
// replaces this binary with a verified release, and restarts and
// health-checks the daemon when a systemd unit is configured or
// --restart is given; when the release would migrate a schema, it first
// takes a dump of the database, or checks the fresh one --dump names,
// and refuses without one. A migration that fails leaves the schema as
// it was; one that succeeds is undone only from that dump. migrations
// lists the migrations this binary carries, which upgrade asks of the
// release it is about to install. status
// asks the running daemon, over its socket and as its own user, for the
// report `aries admin status` gives a server admin.
//
// serve exports metrics and traces when it is configured to
// (docs/server.md, Observability): pushed over OTLP/HTTP to the endpoint
// the standard OTEL_* environment variables name, and the metrics
// offered to a scraper, with /healthz and /readyz, on the loopback
// listener telemetry: listen: sets. Its traces name people by a
// stand-in unless telemetry: principals: says otherwise, made with a
// key it keeps in a file of its own. Under systemd with Type=notify it
// says when it is ready, and feeds the unit's watchdog.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/sdnotify"
	"github.com/ariesworx/aries/internal/server"
	"github.com/ariesworx/aries/internal/store"
	"github.com/ariesworx/aries/internal/telemetry"
	"github.com/ariesworx/aries/internal/version"
)

const usage = `usage:
  ariesd serve [--dev [--allow-unsafe-account]] [--config FILE] [--dsn DSN] [--socket PATH] [--project UUID] [--prefix P] [--log-level L] [--log-format F] [--accept-clock-behind TIME]
  ariesd stdio --principal NAME [--key FINGERPRINT] [--config FILE] [--socket PATH]
  ariesd control init --name NAME [--authorized-keys FILE] [--restore-removed-access] [--config FILE] [--dsn DSN]
  ariesd control invite --provider P [--config FILE] [--dsn DSN] NAME ADDRESS
  ariesd import-bd [--config FILE] [--dsn DSN] [--project UUID] [--principal NAME] [--dry-run] [--json] FILE|-
  ariesd export-bd [--config FILE] [--dsn DSN] [--project UUID] [-o FILE]
  ariesd upgrade [--check] [--to vX.Y.Z] [--rollback] [--restart] [--dump FILE] [--config FILE] [--dsn DSN] [--socket PATH]
  ariesd migrations
  ariesd status [--json] [--config FILE] [--socket PATH]
  ariesd version

The database DSN, a postgres:// URL, comes from --dsn (no password
allowed there), $ARIESD_DSN, or dsn: in the config file (default
/etc/aries/ariesd.yaml, mode 0600); a password is better in the role's
.pgpass or $PGPASSWORD.
Admins, who may change issues others hold and force a close, are listed
under admins: in the config file or in $ARIESD_ADMINS (comma-separated);
serve reads them at start.

With control_schema: (or $ARIESD_CONTROL_SCHEMA) set, the server holds many
projects, each a schema of the DSN's database, and who may reach each is
in the control_schema: schema, which control init makes. Run control init
once to adopt the DSN's schema as the first project; admins then
manage projects, principals, members and keys from anywhere with aries admin;
each new project gets the schema project_NAME. serve writes
the keys the control store holds to keys_file: (default
~/.ssh/aries_keys of the user it runs as), which sshd reads with
AuthorizedKeysFile before the authorized_keys you maintain by hand.

control init may be run again, to import new lines of authorized_keys. It
leaves alone what admins did since, and reports each:
  a key removed with            aries admin key remove
  a membership removed with     aries admin member remove
  a principal disabled with     aries admin principal disable
--restore-removed-access overrides those admins: it puts all three back
for every principal the file names, and records each as an event.

control invite invites ADDRESS at the login: provider P for the principal
NAME, as aries admin invite add does; NAME's first aries login there binds
it. Nobody invites onto their own principal, so it is how a server whose
only admin is one person gives that person an identity. Like control init,
it writes as the operator and runs beside serve.

serve refuses to start, with exit status 75, when the server's clock is
behind the latest event of a project it serves by more than
clock_behind_tolerance: (default 2m): check the clock, correct it and
start again. --accept-clock-behind TIME starts once all the same, for a
history no later than TIME, which the refusal prints; clock_behind: warn
(or $ARIESD_CLOCK_BEHIND) starts with a warning every time.

serve exports nothing and opens no port unless configured to: metrics and
traces are pushed over OTLP/HTTP to $OTEL_EXPORTER_OTLP_ENDPOINT when it
is set, and telemetry: listen: (a loopback HOST:PORT, or
$ARIESD_TELEMETRY_LISTEN) serves /metrics, /healthz and /readyz to this
machine. Traces name people by a stand-in (telemetry: principals:
pseudonym, name or omit), made with a key serve keeps in telemetry:
principals_key: (default telemetry/principals.key in its home directory).
Until serve can make that file its traces say nothing about who, and its
log and its status say why; $OTEL_TRACES_EXPORTER=none sends metrics
alone. status prints what the running daemon knows of itself and its
projects; run it as the user ariesd runs as.

Every command that opens the store refuses a server older than Postgres
16, and a database role that is a superuser or holds rights beyond its
database (CREATEROLE, CREATEDB, REPLICATION, BYPASSRLS, or the roles that
reach the server's files); --dev --allow-unsafe-account allows such a
role on a developer's own machine.`

// usageError is a mistake on the command line: main prints it with the
// usage and exits 2.
type usageError string

func (e usageError) Error() string { return string(e) }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:])
	stop()
	if err == nil {
		os.Exit(0)
	}
	// A bare exitError means the command already said what went wrong.
	if code, ok := err.(exitError); ok { //nolint:errorlint // only the unwrapped value is silent
		os.Exit(int(code))
	}
	if behind, ok := errors.AsType[*server.ClockBehindError](err); ok {
		// A line for each part, the last things in the journal: the unit
		// does not start a daemon again that exits with this status.
		for _, l := range behind.Lines() {
			fmt.Fprintf(os.Stderr, "ariesd: %s\n", l)
		}
		os.Exit(exitClockBehind)
	}
	// The cause, then each fix, on a line of its own: `systemctl status`
	// cuts a long line, and the fix is at its end.
	for _, l := range refusalLines(err) {
		fmt.Fprintf(os.Stderr, "ariesd: %s\n", l)
	}
	if _, ok := errors.AsType[usageError](err); ok {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	os.Exit(1)
}

// exitClockBehind is serve's exit status when it refuses to start
// because the clock is behind the server's own history. It is a status
// of its own so that the systemd unit can leave the daemon stopped
// (RestartPreventExitStatus=75): restarting cannot correct a clock, and
// the refusal should be the last thing in the journal, not one line of a
// loop.
const exitClockBehind = 75

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError("no command")
	}
	switch args[0] {
	case "serve":
		return serve(ctx, args[1:])
	case "stdio":
		return stdio(ctx, args[1:])
	case "control":
		return controlCmd(ctx, osEnv(), args[1:])
	case "import-bd":
		return importBD(ctx, osEnv(), args[1:])
	case "export-bd":
		return exportBD(ctx, osEnv(), args[1:])
	case "upgrade":
		return upgradeCmd(ctx, osEnv(), osUpgradeDeps(), args[1:])
	case "status":
		return statusCmd(ctx, osEnv(), args[1:])
	case "migrations":
		return migrationsCmd(osEnv(), args[1:])
	case "version", "--version":
		fmt.Printf("ariesd %s\n", version.Version)
		return nil
	case "help", "-h", "--help":
		fmt.Println(usage)
		return nil
	}
	return usageError(fmt.Sprintf("unknown command %q", args[0]))
}

// flags parses serve's or stdio's flags: --config into cfg, --socket into
// s, and those extra registers. Positional arguments are refused.
func flags(name string, args []string, s *server.Settings, cfg *string, extra func(*flag.FlagSet)) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(cfg, "config", "", "config file")
	fs.StringVar(&s.Socket, "socket", "", "unix socket path")
	extra(fs)
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error())
	}
	if fs.NArg() > 0 {
		return usageError(fmt.Sprintf("%s takes no arguments", name))
	}
	return nil
}

// serve runs `ariesd serve`, the daemon, until ctx is done.
func serve(ctx context.Context, args []string) error {
	var fl server.Settings
	var cfgPath, accept string
	var dev bool
	if err := flags("serve", args, &fl, &cfgPath, func(fs *flag.FlagSet) {
		devFlags(fs, &dev, &fl.AllowUnsafeAccount)
		fs.StringVar(&accept, "accept-clock-behind", "", "start once although the clock is behind a history no later than this time (RFC 3339)")
		fs.StringVar(&fl.DSN, "dsn", "", "Postgres DSN, a postgres:// URL (no password)")
		fs.StringVar(&fl.Project, "project", "", "project UUID")
		fs.StringVar(&fl.Prefix, "prefix", "", "issue-ID prefix")
		fs.StringVar(&fl.LogLevel, "log-level", "", "debug, info, warn or error (default info)")
		fs.StringVar(&fl.LogFormat, "log-format", "", "text or json (default text)")
	}); err != nil {
		return err
	}
	if err := checkDev(dev, fl.AllowUnsafeAccount); err != nil {
		return err
	}
	if accept != "" {
		t, err := time.Parse(time.RFC3339, accept)
		if err != nil {
			return usageError(fmt.Sprintf("--accept-clock-behind %q is not a time such as 2026-10-12T09:30:00Z; give the time the refusal to start printed", accept))
		}
		fl.AcceptClockBehind = t
	}
	if err := server.RequirePeerCheck(dev); err != nil {
		return err
	}
	s, err := server.ResolveSettings(fl, cfgPath, os.Getenv)
	if err != nil {
		return err
	}
	log, err := server.NewLogger(os.Stderr, s.LogLevel, s.LogFormat)
	if err != nil {
		return err
	}
	return runDaemon(ctx, s, log, os.Getenv)
}

// telemetryStop bounds the last export when the daemon stops, so a
// collector that is down or silent does not hold the stop.
const telemetryStop = 5 * time.Second

// runDaemon runs the daemon s configures until ctx is done: its
// telemetry, the loopback listener if one is set, the store or stores,
// and the socket. getenv looks up the OTEL_* variables and systemd's.
//
// The loopback listener is opened first, so that a monitor can watch a
// start that takes a while, as one that migrates many projects does:
// /healthz answers at once, and /readyz says the daemon is starting
// until it listens on its socket. A listener that cannot be bound fails
// the start.
func runDaemon(ctx context.Context, s server.Settings, log *slog.Logger, getenv func(string) string) error {
	if err := s.CheckServe(); err != nil {
		return err
	}
	// A stand-in key that cannot be found, read or made never stops the
	// daemon: its spans then say nothing about who, and its log and its
	// status say why (telemetry.Config.KeyFile).
	keyFile, keyErr := principalsKey(s)
	tel, err := telemetry.New(ctx, telemetry.Config{Version: version.Version, Getenv: getenv, Logger: log,
		Pull: s.Telemetry.Listen != "", PerProject: s.Telemetry.ByProject(), Ops: server.Ops(), Projects: s.Limits.Control.Projects,
		Principals: s.Telemetry.Principals, KeyFile: keyFile, KeyFileErr: keyErr})
	if err != nil {
		return err
	}
	// With OTEL_LOGS_EXPORTER=otlp every line the journal gets from here
	// on is also exported (design §16.4). The journal never depends on
	// it: the export is taken off before telemetry stops, so the lines
	// of the stop itself are the journal's alone.
	tap, _ := log.Handler().(*logs.Handler)
	if h := tel.LogHandler(); h != nil && tap != nil {
		tap.Tap(h)
	}
	defer func() {
		if tap != nil {
			tap.Tap(nil)
		}
		sctx, cancel := context.WithTimeout(context.Background(), telemetryStop)
		defer cancel()
		if err := tel.Shutdown(sctx); err != nil {
			log.Warn("telemetry did not stop cleanly", "event", logs.TelemetryStopFailed, "err", err)
		}
	}()
	// srv is the server once it is built; /readyz asks it.
	var srv atomic.Pointer[server.Server]
	if s.Telemetry.Listen != "" {
		tl, err := telemetry.Listen(ctx, s.Telemetry.Listen)
		if err != nil {
			return err
		}
		tctx, stop := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			err := tel.Serve(tctx, tl, telemetry.ServeOptions{Requests: s.Telemetry.MaxRequests, Logger: log,
				Ready: func(ctx context.Context) string {
					if p := srv.Load(); p != nil {
						return p.Ready(ctx)
					}
					return "ariesd is starting"
				}})
			if err != nil {
				log.Error("the telemetry listener stopped; metrics and health are not served until ariesd restarts", "event", logs.TelemetryListenerStopped, "err", err)
			}
		}()
		defer func() {
			stop()
			<-done
		}()
		if tel.Enabled() {
			log.Info("telemetry listener serving /metrics, /healthz and /readyz", "event", logs.TelemetryListening, "addr", tl.Addr().String())
		} else {
			log.Info("telemetry listener serving /healthz and /readyz; metrics are off", "event", logs.TelemetryListening, "addr", tl.Addr().String())
		}
	}
	// The HTTPS listener is bound before the database is opened: a port
	// that is taken, or a certificate that cannot be served, fails the
	// start at once. The server closes it from here on.
	web, err := listenHTTPS(ctx, s, log, tel)
	if err != nil {
		return err
	}
	note := sdnotify.New(getenv, os.Getpid())
	d, err := openDaemonWith(ctx, s, log, hooks{tel: tel, https: web, heartbeatEvery: note.WatchdogEvery(), heartbeat: func() {
		if err := note.Watchdog(); err != nil {
			log.Warn("could not feed systemd's watchdog", "event", logs.SystemdNotify, "err", err)
		}
	}})
	if err != nil {
		if web != nil {
			_ = web.Listener.Close()
		}
		return err
	}
	l, err := server.Listen(s.Socket)
	if err != nil {
		if web != nil {
			_ = web.Listener.Close()
		}
		return errors.Join(err, d.Close())
	}
	srv.Store(d.srv)
	transports := strings.Join(s.Transports, " ")
	if d.ctl != nil {
		log.Info("ariesd serving", "event", logs.DaemonServing, "version", version.Version, "socket", s.Socket, "control_schema", s.ControlSchema,
			"projects", d.ctl.ActiveProjects(), "admins", s.Admins, "transports", transports)
	} else {
		log.Info("ariesd serving", "event", logs.DaemonServing, "version", version.Version, "socket", s.Socket, "project", s.Project, "admins", s.Admins,
			"transports", transports)
	}
	// The socket takes connections from here on, so a unit with
	// Type=notify is started now, and told when the stop begins.
	if err := note.Ready(); err != nil {
		log.Warn("could not tell systemd the daemon is ready", "event", logs.SystemdNotify, "err", err)
	}
	stopping := context.AfterFunc(ctx, func() { _ = note.Stopping() })
	defer stopping()
	err = d.srv.Serve(ctx, l)
	return errors.Join(err, d.Close())
}

// refusalLines is err as the lines main prints: what went wrong, then
// each "fix: " the error names, a line each.
func refusalLines(err error) []string {
	parts := strings.Split(err.Error(), "; fix: ")
	for i := 1; i < len(parts); i++ {
		parts[i] = "fix: " + parts[i]
	}
	return parts
}

// listenHTTPS binds the HTTPS listener s configures, with its TLS, or
// returns nil when s serves no HTTPS. CheckServe has passed. tel counts
// the certificate's reloads.
func listenHTTPS(ctx context.Context, s server.Settings, log *slog.Logger, tel *telemetry.Telemetry) (*server.HTTPS, error) {
	if !s.HTTPSOn() {
		return nil, nil
	}
	lt, err := s.HTTPS.TLSConfig(log, tel)
	if err != nil {
		return nil, err
	}
	trusted, err := s.HTTPS.Trusted()
	if err != nil {
		return nil, err
	}
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", s.HTTPS.Listen)
	if err != nil {
		return nil, fmt.Errorf("https: listen on %s: %w; fix: free the port, or set https: listen: to another, and when it is below 1024 give ariesd CAP_NET_BIND_SERVICE (docs/server.md, The HTTPS listener)", s.HTTPS.Listen, err)
	}
	return &server.HTTPS{Listener: l, TLS: lt.TLS, Trusted: trusted, URLs: s.HTTPS.PublicURLs, Mode: lt.Mode, NotAfter: lt.NotAfter,
		RefuseKeyProofV1: s.HTTPS.RefuseKeyProofV1}, nil
}

// hooks are what runDaemon adds to the daemon openDaemonWith builds.
// The zero value adds nothing.
type hooks struct {
	// tel measures the server and its stores.
	tel *telemetry.Telemetry
	// https is the HTTPS listener the server serves, when HTTPS is on.
	https *server.HTTPS
	// heartbeat is called every heartbeatEvery from the server's reaper,
	// for systemd's watchdog; zero calls nothing.
	heartbeat      func()
	heartbeatEvery time.Duration
}

// daemon is what serve runs: the server, and what it must close when
// the server has stopped.
type daemon struct {
	srv *server.Server
	// db is the pool every store is opened from. st is the store of a
	// server of one project, and ctl the control store of a server of
	// many (control_schema). Exactly one is set.
	db  *store.DB
	st  *store.Store
	ctl *store.Control
}

// Close closes the daemon's store or control store, and then its pool.
// The server closes the project stores it opened itself.
func (d *daemon) Close() error {
	var err error
	if d.ctl != nil {
		err = d.ctl.Close()
	} else {
		err = d.st.Close()
	}
	return errors.Join(err, d.db.Close())
}

// openDaemon opens what s configures: the store of the one project in
// the DSN, or, with control_schema set, the control store of a server of
// many projects, whose stores the server opens as they are asked for,
// after opening each active project's once to migrate it. A server of
// one project opens no control store.
func openDaemon(ctx context.Context, s server.Settings, log *slog.Logger) (*daemon, error) {
	return openDaemonWith(ctx, s, log, hooks{})
}

// openDaemonWith is openDaemon with telemetry and a heartbeat.
func openDaemonWith(ctx context.Context, s server.Settings, log *slog.Logger, h hooks) (*daemon, error) {
	// One clock for the process: every store reads it, so each learns
	// of a step whichever noticed it, and the server reports them.
	clk := store.NewClock(store.ClockOptions{StepTolerance: time.Duration(s.ClockStepTolerance)})
	cfg := server.Config{Version: version.Version, Logger: log, Limits: s.Limits, Account: s.Account,
		ConflictAge: time.Duration(s.ConflictAge), DueSoon: time.Duration(s.DueSoon),
		Telemetry: h.tel, Heartbeat: h.heartbeat, HeartbeatEvery: h.heartbeatEvery, NoAudit: !s.Telemetry.Audit(),
		Clock: clk, ClockBehind: s.ClockBehind, BehindTolerance: time.Duration(s.ClockBehindTolerance), AcceptHistory: s.AcceptClockBehind,
		NoSSH: !s.SSH(), HTTPS: h.https}
	opts := store.Options{Logger: log, AllowUnsafeAccount: s.AllowUnsafeAccount, Limits: s.Limits.Limits, Readers: s.Limits.DBConnsPerProject,
		Clock: clk, ClientAhead: time.Duration(s.ClockClientTolerance), IdemWindow: time.Duration(s.Limits.IdemWindow), AwayMax: time.Duration(s.Limits.AwayMax)}
	if s.DSN == "" {
		return nil, errors.New("no database DSN; fix: set $ARIESD_DSN or dsn: in the config file")
	}
	if s.ControlSchema == "" && s.Project == "" {
		return nil, errors.New("no project; fix: set --project, $ARIESD_PROJECT or project: in the config file")
	}
	// One pool for the server, which every store shares (store.DB).
	db, err := store.OpenDB(ctx, s.DSN, store.DBOptions{Conns: s.Limits.DBConns})
	if err != nil {
		return nil, fmt.Errorf("open the database: %w; fix: check the DSN and that Postgres is running", err)
	}
	cfg.Postgres = db
	if s.ControlSchema == "" {
		d, err := daemonOfOne(ctx, s, db, cfg, opts, h)
		if err != nil {
			return nil, errors.Join(err, db.Close())
		}
		return d, nil
	}
	ctl, err := openControl(ctx, s, db, store.ControlOptions{Logger: log, Clock: clk, Tracer: server.StoreTracer(h.tel, ""),
		Readers: s.Limits.DBConnsPerProject})
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	srv, err := daemonOfMany(ctx, s, db, ctl, cfg, opts, h, log)
	if err != nil {
		return nil, errors.Join(err, ctl.Close(), db.Close())
	}
	return &daemon{srv: srv, db: db, ctl: ctl}, nil
}

// daemonOfOne opens the store of the one project in s's DSN, from db,
// and the server of it.
func daemonOfOne(ctx context.Context, s server.Settings, db *store.DB, cfg server.Config, opts store.Options, h hooks) (*daemon, error) {
	d, err := store.ParseDSN(s.DSN)
	if err != nil {
		return nil, err
	}
	opts.Prefix, opts.Admins = s.Prefix, s.Admins
	opts.Observer = server.StoreObserver(h.tel, opts.Logger, s.Project)
	opts.Tracer = server.StoreTracer(h.tel, s.Project)
	if h.https != nil {
		// A server of one project has no store of keys: a key proof over
		// HTTPS is checked against the lines of authorized_keys that
		// force the bridge, as sshd checks the key.
		if _, cfg.AuthorizedKeys, err = keyFiles(s); err != nil {
			return nil, err
		}
	}
	st, err := db.Open(ctx, d.Schema, opts)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	cfg.Store, cfg.Project = st, s.Project
	srv, err := server.New(cfg)
	if err != nil {
		return nil, errors.Join(err, st.Close())
	}
	return &daemon{srv: srv, db: db, st: st}, nil
}

// daemonOfMany makes the server of the projects ctl holds, whose stores
// it opens from db, and readies it: the keys file written, every active
// project's schema opened, and the claims of the archived ones ended.
func daemonOfMany(ctx context.Context, s server.Settings, db *store.DB, ctl *store.Control, cfg server.Config, opts store.Options, h hooks, log *slog.Logger) (*server.Server, error) {
	if ctl.ActiveProjects() == 0 {
		return nil, fmt.Errorf("the control store %s holds no active project; fix: adopt this server's project as the first with `ariesd control init --name NAME`",
			s.ControlSchema)
	}
	warnOfAdmins(ctx, ctl, s.Admins, log)
	cfg.Control, cfg.Open = ctl, server.ProjectOpener(db, ctl, opts, h.tel)
	cfg.Bridge = s.Bridge
	var err error
	if cfg.KeysFile, cfg.AuthorizedKeys, err = keyFiles(s); err != nil {
		return nil, err
	}
	// The providers' files are read now, and nothing is fetched: a
	// provider's discovery waits for the first sign-in.
	if cfg.Login, err = server.OpenLogin(s.Login, ctl.Now); err != nil {
		return nil, err
	}
	audience := ""
	if cfg.HTTPS != nil && len(cfg.HTTPS.URLs) > 0 {
		audience = cfg.HTTPS.URLs[0]
	}
	if cfg.Workload, err = server.OpenWorkload(s.Workload, s.Login, audience, ctl.Now); err != nil {
		return nil, err
	}
	srv, err := server.New(cfg)
	if err != nil {
		return nil, err
	}
	// sshd reads the keys from this file, so it holds what the store
	// does before anyone can connect, or the daemon does not start.
	if err := srv.WriteKeys(); err != nil {
		return nil, err
	}
	// Every active project's schema is opened, and so migrated,
	// before the daemon listens: after an upgrade, the health check
	// then answers for all of them. One that fails is logged and held
	// against that check; the others are served.
	if err := srv.OpenProjects(ctx); err != nil {
		if _, ok := errors.AsType[*server.ClockBehindError](err); ok {
			return nil, err // it names its own fix
		}
		return nil, fmt.Errorf("%w; fix: check that Postgres is running", err)
	}
	// Every claim in an archived project ends before the daemon listens
	// (decision 24): one left by a daemon that stopped between an
	// archive and its release would otherwise wait for the project to
	// be activated. An archived project's schema is opened as it is
	// and never migrated here; activating the project does that.
	asIs := opts
	asIs.NoMigrate = true
	if err := srv.ReleaseArchived(ctx, server.ProjectOpener(db, ctl, asIs, h.tel)); err != nil {
		return nil, fmt.Errorf("%w; fix: check that Postgres is running", err)
	}
	return srv, nil
}

// keyFiles returns the absolute paths of the keys file a server of many
// projects writes and of the authorized_keys file it reads. A relative
// setting is under the home directory of the user ariesd runs as,
// taken from the account database, not from $HOME, which a daemon
// started by hand may have inherited from someone else.
func keyFiles(s server.Settings) (keysFile, authorizedKeys string, err error) {
	home := ""
	if !filepath.IsAbs(s.KeysFile) || !filepath.IsAbs(s.AuthorizedKeys) {
		if home, err = accountHome(); err != nil {
			return "", "", fmt.Errorf("%w; fix: set keys_file: and authorized_keys: to absolute paths in the config file", err)
		}
	}
	keysFile, authorizedKeys = s.KeyFiles(home)
	return keysFile, authorizedKeys, nil
}

// principalsKey returns the absolute path of the file that holds the
// key principals' stand-ins are made with (telemetry: principals_key:),
// which is under the account's home directory when the setting is
// relative, as it is by default.
func principalsKey(s server.Settings) (string, error) {
	home := ""
	if !filepath.IsAbs(s.Telemetry.PrincipalsKey) {
		var err error
		if home, err = accountHome(); err != nil {
			return "", fmt.Errorf("no file is named for it: %w", err)
		}
	}
	return s.PrincipalsKey(home), nil
}

// accountHome returns the home directory of the user ariesd runs as.
func accountHome() (string, error) {
	u, err := user.Current()
	if err == nil && !filepath.IsAbs(u.HomeDir) {
		err = errors.New("the account has no home directory")
	}
	if err != nil {
		return "", fmt.Errorf("find the home directory of the user ariesd runs as: %w", err)
	}
	return u.HomeDir, nil
}

// warnOfAdmins logs, for each server admin the config file names, a row
// or a key in the control store that neither a server admin nor the
// operator put there. A project admin may create a principal of any
// free name with a key of their own, and add keys to anyone within
// their reach; naming such a principal under admins: later makes that
// key a server admin's. The daemon starts all the same: the operator
// chose the names, and is told what to check.
func warnOfAdmins(ctx context.Context, ctl *store.Control, admins []string, log *slog.Logger) {
	trusted := func(who string) bool { return who == store.OperatorPrincipal || slices.Contains(admins, who) }
	for _, a := range admins {
		p, err := ctl.Principal(ctx, a)
		if errors.Is(err, store.ErrNotFound) {
			continue // no row, so no key in the store either
		}
		if err != nil {
			log.ErrorContext(ctx, "check the configured server admins", "event", logs.AdminCheck, "admin", a, "err", err)
			continue
		}
		if !trusted(p.CreatedBy) {
			log.WarnContext(ctx, "a configured server admin was set up by someone who is no server admin", "event", logs.AdminWarning, "admin", a, "created_by", p.CreatedBy,
				"fix", "check with "+a+" that every key `aries admin key list "+a+"` shows is theirs, or take the name out of admins:")
		}
		keys, err := ctl.Keys(ctx, a)
		if err != nil {
			log.ErrorContext(ctx, "check the configured server admins", "event", logs.AdminCheck, "admin", a, "err", err)
			continue
		}
		for _, k := range keys {
			if !trusted(k.AddedBy) {
				log.WarnContext(ctx, "a configured server admin was set up by someone who is no server admin", "event", logs.AdminWarning, "admin", a, "fingerprint", k.Fingerprint, "added_by", k.AddedBy,
					"fix", "check with "+a+" that the key is theirs, or have another server admin run `aries admin key remove "+k.Fingerprint+"`")
			}
		}
	}
}

// stdio runs `ariesd stdio`, the forced command: it bridges this
// process's stdin and stdout to the daemon's socket. sshd starts one for
// every connection, so it reads only the socket from the settings.
func stdio(ctx context.Context, args []string) error {
	fl, cfgPath, principal, key, err := stdioArgs(args)
	if err != nil {
		return err
	}
	socket, err := server.ResolveSocket(fl.Socket, cfgPath, os.Getenv)
	if err != nil {
		return err
	}
	return server.Bridge(ctx, socket, principal, key, os.Stdin, os.Stdout)
}

// stdioArgs parses the arguments of `ariesd stdio`. `control init`
// reads the forced command of an authorized_keys line with it too, so
// that the two cannot disagree on whose a line is.
func stdioArgs(args []string) (fl server.Settings, cfgPath, principal, key string, err error) {
	if err := flags("stdio", args, &fl, &cfgPath, func(fs *flag.FlagSet) {
		fs.StringVar(&principal, "principal", "", "principal this key authenticates")
		fs.StringVar(&key, "key", "", "SHA256 fingerprint of the key, when its line is one ariesd wrote to its keys file")
	}); err != nil {
		return fl, "", "", "", err
	}
	if principal == "" {
		return fl, "", "", "", usageError("stdio needs --principal")
	}
	return fl, cfgPath, principal, key, nil
}
