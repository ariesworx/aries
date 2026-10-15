// Command starfixd is the starfix server.
//
//	starfixd serve [--dev [--allow-unsafe-dolt]] [--config FILE] [--dsn DSN] [--socket PATH] [--project UUID] [--prefix P] [--log-level L] [--log-format F]
//	starfixd stdio --principal NAME [--key FINGERPRINT] [--config FILE] [--socket PATH]
//	starfixd control init --name NAME [--authorized-keys FILE] [--restore-removed-access] [--config FILE] [--dsn DSN]
//	starfixd import-bd [--config FILE] [--dsn DSN] [--project UUID] [--principal NAME] [--dry-run] [--json] FILE|-
//	starfixd export-bd [--config FILE] [--dsn DSN] [--project UUID] [-o FILE]
//	starfixd upgrade [--check] [--to vX.Y.Z] [--rollback] [--restart] [--config FILE] [--dsn DSN] [--socket PATH]
//	starfixd status [--json] [--config FILE] [--socket PATH]
//	starfixd version
//
// serve is the long-running daemon: it owns the Dolt store and serves the
// protocol on a unix socket. stdio is the forced command in each developer
// key's authorized_keys line; it bridges the SSH session to that socket.
// import-bd and export-bd are admin commands run on the server: they open
// the store directly with the daemon's DSN settings and move a backlog in
// and out as bd's JSONL. With control_db set the server holds many
// projects, each in a database of its own: control init adopts the
// existing database as the first and imports authorized_keys,
// import-bd and export-bd take --project, and serve writes the keys the
// control database holds to a file sshd reads (keys_file) and opens
// every active project's database once before it listens, which
// migrates it. upgrade replaces this binary with a verified
// release, after tagging the database, and restarts and health-checks the
// daemon when a systemd unit is configured or --restart is given. status
// asks the running daemon, over its socket and as its own user, for the
// report `sfx admin status` gives a server admin.
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
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ariesworx/starfix/internal/doltmetrics"
	"github.com/ariesworx/starfix/internal/logs"
	"github.com/ariesworx/starfix/internal/sdnotify"
	"github.com/ariesworx/starfix/internal/server"
	"github.com/ariesworx/starfix/internal/store"
	"github.com/ariesworx/starfix/internal/telemetry"
	"github.com/ariesworx/starfix/internal/version"
)

const usage = `usage:
  starfixd serve [--dev [--allow-unsafe-dolt]] [--config FILE] [--dsn DSN] [--socket PATH] [--project UUID] [--prefix P] [--log-level L] [--log-format F] [--accept-clock-behind TIME]
  starfixd stdio --principal NAME [--key FINGERPRINT] [--config FILE] [--socket PATH]
  starfixd control init --name NAME [--authorized-keys FILE] [--restore-removed-access] [--config FILE] [--dsn DSN]
  starfixd import-bd [--config FILE] [--dsn DSN] [--project UUID] [--principal NAME] [--dry-run] [--json] FILE|-
  starfixd export-bd [--config FILE] [--dsn DSN] [--project UUID] [-o FILE]
  starfixd upgrade [--check] [--to vX.Y.Z] [--rollback] [--restart] [--config FILE] [--dsn DSN] [--socket PATH]
  starfixd status [--json] [--config FILE] [--socket PATH]
  starfixd version

The database DSN comes from --dsn (no password allowed there), $STARFIXD_DSN,
or dsn: in the config file (default /etc/starfix/starfixd.yaml, mode 0600).
Admins, who may change issues others hold and force a close, are listed
under admins: in the config file or in $STARFIXD_ADMINS (comma-separated);
serve reads them at start.

With control_db: (or $STARFIXD_CONTROL_DB) set, the server holds many
projects, and who may reach each is in that database. Run control init
once to adopt the existing database as the first project; admins then
manage projects, principals, members and keys from anywhere with sfx admin.
project_dbs: names the databases granted for new projects. serve writes
the keys the control database holds to keys_file: (default
~/.ssh/starfix_keys of the user it runs as), which sshd reads with
AuthorizedKeysFile before the authorized_keys you maintain by hand.

control init may be run again, to import new lines of authorized_keys. It
leaves alone what admins did since, and reports each:
  a key removed with            sfx admin key remove
  a membership removed with     sfx admin member remove
  a principal disabled with     sfx admin principal disable
--restore-removed-access overrides those admins: it puts all three back
for every principal the file names, and records each as an event.

serve refuses to start, with exit status 75, when the server's clock is
behind the latest event of a database it serves by more than
clock_behind_tolerance: (default 2m): check the clock, correct it and
start again. --accept-clock-behind TIME starts once all the same, for a
history no later than TIME, which the refusal prints; clock_behind: warn
(or $STARFIXD_CLOCK_BEHIND) starts with a warning every time.

serve exports nothing and opens no port unless configured to: metrics and
traces are pushed over OTLP/HTTP to $OTEL_EXPORTER_OTLP_ENDPOINT when it
is set, and telemetry: listen: (a loopback HOST:PORT, or
$STARFIXD_TELEMETRY_LISTEN) serves /metrics, /healthz and /readyz to this
machine. Traces name people by a stand-in (telemetry: principals:
pseudonym, name or omit), made with a key serve keeps in telemetry:
principals_key: (default telemetry/principals.key in its home directory).
Until serve can make that file its traces say nothing about who, and its
log and its status say why; $OTEL_TRACES_EXPORTER=none sends metrics
alone. status prints what the running daemon knows of itself and its
projects; run it as the user starfixd runs as. With telemetry:
dolt_metrics_url: (an http:// URL on a loopback address, or
$STARFIXD_TELEMETRY_DOLT_METRICS_URL) set to Dolt's own metrics endpoint,
serve reads it every 15 seconds and status shows Dolt's memory,
connections, uptime and collector.

Every command that opens the store refuses a Dolt account that is root,
holds rights beyond its database, or a server whose secure_file_priv is
empty; --dev --allow-unsafe-dolt allows it on a developer's own machine.`

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
			fmt.Fprintf(os.Stderr, "starfixd: %s\n", l)
		}
		os.Exit(exitClockBehind)
	}
	// The cause, then each fix, on a line of its own: `systemctl status`
	// cuts a long line, and the fix is at its end.
	for _, l := range refusalLines(err) {
		fmt.Fprintf(os.Stderr, "starfixd: %s\n", l)
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
	case "version", "--version":
		fmt.Printf("starfixd %s\n", version.Version)
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

// serve runs `starfixd serve`, the daemon, until ctx is done.
func serve(ctx context.Context, args []string) error {
	var fl server.Settings
	var cfgPath, accept string
	var dev bool
	if err := flags("serve", args, &fl, &cfgPath, func(fs *flag.FlagSet) {
		devFlags(fs, &dev, &fl.AllowUnsafeDolt)
		fs.StringVar(&accept, "accept-clock-behind", "", "start once although the clock is behind a history no later than this time (RFC 3339)")
		fs.StringVar(&fl.DSN, "dsn", "", "Dolt DSN (no password)")
		fs.StringVar(&fl.Project, "project", "", "project UUID")
		fs.StringVar(&fl.Prefix, "prefix", "", "issue-ID prefix")
		fs.StringVar(&fl.LogLevel, "log-level", "", "debug, info, warn or error (default info)")
		fs.StringVar(&fl.LogFormat, "log-format", "", "text or json (default text)")
	}); err != nil {
		return err
	}
	if err := checkDev(dev, fl.AllowUnsafeDolt); err != nil {
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
	// Dolt's own metrics, for the status report (design §16.5). A URL
	// that is not on loopback fails the start here, with its fix: a host
	// that is a name is resolved now.
	var dolt *doltmetrics.Reader
	if u := s.Telemetry.DoltMetricsURL; u != "" {
		if dolt, err = doltmetrics.New(ctx, u); err != nil {
			return err
		}
		defer dolt.Close()
		log.Info("reading Dolt's metrics for the status report", "event", logs.DoltMetricsReading, "url", dolt.URL(), "every", server.DefaultDoltMetricsEvery)
	}
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
			err := tel.Serve(tctx, tl, telemetry.ServeOptions{Requests: s.Limits.TelemetryRequests, Logger: log,
				Ready: func(ctx context.Context) string {
					if p := srv.Load(); p != nil {
						return p.Ready(ctx)
					}
					return "starfixd is starting"
				}})
			if err != nil {
				log.Error("the telemetry listener stopped; metrics and health are not served until starfixd restarts", "event", logs.TelemetryListenerStopped, "err", err)
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
	note := sdnotify.New(getenv, os.Getpid())
	d, err := openDaemonWith(ctx, s, log, hooks{tel: tel, dolt: dolt, heartbeatEvery: note.WatchdogEvery(), heartbeat: func() {
		if err := note.Watchdog(); err != nil {
			log.Warn("could not feed systemd's watchdog", "event", logs.SystemdNotify, "err", err)
		}
	}})
	if err != nil {
		return err
	}
	l, err := server.Listen(s.Socket)
	if err != nil {
		return errors.Join(err, d.Close())
	}
	srv.Store(d.srv)
	if d.ctl != nil {
		log.Info("starfixd serving", "event", logs.DaemonServing, "version", version.Version, "socket", s.Socket, "control_db", s.ControlDB,
			"projects", d.ctl.ActiveProjects(), "admins", s.Admins)
	} else {
		log.Info("starfixd serving", "event", logs.DaemonServing, "version", version.Version, "socket", s.Socket, "project", s.Project, "admins", s.Admins)
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

// hooks are what runDaemon adds to the daemon openDaemonWith builds.
// The zero value adds nothing.
type hooks struct {
	// tel measures the server and its stores.
	tel *telemetry.Telemetry
	// dolt reads Dolt's own metrics for the status report; nil reads
	// none.
	dolt *doltmetrics.Reader
	// heartbeat is called every heartbeatEvery from the server's reaper,
	// for systemd's watchdog; zero calls nothing.
	heartbeat      func()
	heartbeatEvery time.Duration
}

// daemon is what serve runs: the server, and what it must close when
// the server has stopped.
type daemon struct {
	srv *server.Server
	// st is the store of a server of one project, and ctl the control
	// store of a server of many (control_db). Exactly one is set.
	st  *store.Store
	ctl *store.Control
}

// Close closes the daemon's store or control store. The server closes
// the project stores it opened itself.
func (d *daemon) Close() error {
	if d.ctl != nil {
		return d.ctl.Close()
	}
	return d.st.Close()
}

// openDaemon opens what s configures: the store of the one project in
// the DSN, or, with control_db set, the control store of a server of
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
	cfg := server.Config{Version: version.Version, Latest: s.Latest, Logger: log, Limits: s.Limits, Account: s.Account,
		Telemetry: h.tel, Heartbeat: h.heartbeat, HeartbeatEvery: h.heartbeatEvery, DoltMetrics: h.dolt, NoAudit: !s.Telemetry.Audit(),
		Clock: clk, ClockBehind: s.ClockBehind, BehindTolerance: time.Duration(s.ClockBehindTolerance), AcceptHistory: s.AcceptClockBehind}
	opts := store.Options{Logger: log, AllowUnsafeAccount: s.AllowUnsafeDolt, Limits: s.Limits.Limits,
		Clock: clk, ClientAhead: time.Duration(s.ClockClientTolerance), IdemWindow: time.Duration(s.Limits.IdemWindow), AwayMax: time.Duration(s.Limits.AwayMax)}
	if s.DSN == "" {
		return nil, errors.New("no database DSN; fix: set $STARFIXD_DSN or dsn: in the config file")
	}
	if s.ControlDB == "" {
		if s.Project == "" {
			return nil, errors.New("no project; fix: set --project, $STARFIXD_PROJECT or project: in the config file")
		}
		opts.Prefix, opts.Admins = s.Prefix, s.Admins
		opts.Observer = server.StoreObserver(h.tel, s.Project)
		opts.Tracer = server.StoreTracer(h.tel, s.Project)
		st, err := store.Open(ctx, s.DSN, opts)
		if err != nil {
			return nil, fmt.Errorf("open store: %w", err)
		}
		cfg.Store, cfg.Project = st, s.Project
		srv, err := server.New(cfg)
		if err != nil {
			return nil, errors.Join(err, st.Close())
		}
		return &daemon{srv: srv, st: st}, nil
	}
	ctl, err := openControl(ctx, s, store.ControlOptions{Logger: log, Clock: clk, Tracer: server.StoreTracer(h.tel, "")})
	if err != nil {
		return nil, err
	}
	if ctl.ActiveProjects() == 0 {
		return nil, errors.Join(fmt.Errorf("the control database %s holds no active project; fix: adopt this server's database as the first with `starfixd control init --name NAME`",
			s.ControlDB), ctl.Close())
	}
	warnOfAdmins(ctx, ctl, s.Admins, log)
	cfg.Control, cfg.Open, cfg.ProjectDBs = ctl, server.ProjectOpener(s.DSN, ctl, opts, h.tel), s.ProjectDBs
	cfg.Bridge = s.Bridge
	if cfg.KeysFile, cfg.AuthorizedKeys, err = keyFiles(s); err != nil {
		return nil, errors.Join(err, ctl.Close())
	}
	srv, err := server.New(cfg)
	if err != nil {
		return nil, errors.Join(err, ctl.Close())
	}
	// sshd reads the keys from this file, so it holds what the store
	// does before anyone can connect, or the daemon does not start.
	if err := srv.WriteKeys(); err != nil {
		return nil, errors.Join(err, ctl.Close())
	}
	// Every active project's database is opened, and so migrated,
	// before the daemon listens: after an upgrade, the health check
	// then answers for all of them. One that fails is logged and held
	// against that check; the others are served.
	if err := srv.OpenProjects(ctx); err != nil {
		if _, ok := errors.AsType[*server.ClockBehindError](err); ok {
			return nil, errors.Join(err, ctl.Close()) // it names its own fix
		}
		return nil, errors.Join(fmt.Errorf("%w; fix: check that dolt sql-server is running", err), ctl.Close())
	}
	// Every claim in an archived project ends before the daemon listens
	// (decision 24): one left by a daemon that stopped between an
	// archive and its release would otherwise wait for the project to
	// be activated. An archived project's database is opened as it is
	// and never migrated here; activating the project does that.
	asIs := opts
	asIs.NoMigrate = true
	if err := srv.ReleaseArchived(ctx, server.ProjectOpener(s.DSN, ctl, asIs, h.tel)); err != nil {
		return nil, errors.Join(fmt.Errorf("%w; fix: check that dolt sql-server is running", err), ctl.Close())
	}
	return &daemon{srv: srv, ctl: ctl}, nil
}

// keyFiles returns the absolute paths of the keys file a server of many
// projects writes and of the authorized_keys file it reads. A relative
// setting is under the home directory of the user starfixd runs as,
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

// accountHome returns the home directory of the user starfixd runs as.
func accountHome() (string, error) {
	u, err := user.Current()
	if err == nil && !filepath.IsAbs(u.HomeDir) {
		err = errors.New("the account has no home directory")
	}
	if err != nil {
		return "", fmt.Errorf("find the home directory of the user starfixd runs as: %w", err)
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
				"fix", "check with "+a+" that every key `sfx admin key list "+a+"` shows is theirs, or take the name out of admins:")
		}
		keys, err := ctl.Keys(ctx, a)
		if err != nil {
			log.ErrorContext(ctx, "check the configured server admins", "event", logs.AdminCheck, "admin", a, "err", err)
			continue
		}
		for _, k := range keys {
			if !trusted(k.AddedBy) {
				log.WarnContext(ctx, "a configured server admin was set up by someone who is no server admin", "event", logs.AdminWarning, "admin", a, "fingerprint", k.Fingerprint, "added_by", k.AddedBy,
					"fix", "check with "+a+" that the key is theirs, or have another server admin run `sfx admin key remove "+k.Fingerprint+"`")
			}
		}
	}
}

// stdio runs `starfixd stdio`, the forced command: it bridges this
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

// stdioArgs parses the arguments of `starfixd stdio`. `control init`
// reads the forced command of an authorized_keys line with it too, so
// that the two cannot disagree on whose a line is.
func stdioArgs(args []string) (fl server.Settings, cfgPath, principal, key string, err error) {
	if err := flags("stdio", args, &fl, &cfgPath, func(fs *flag.FlagSet) {
		fs.StringVar(&principal, "principal", "", "principal this key authenticates")
		fs.StringVar(&key, "key", "", "SHA256 fingerprint of the key, when its line is one starfixd wrote to its keys file")
	}); err != nil {
		return fl, "", "", "", err
	}
	if principal == "" {
		return fl, "", "", "", usageError("stdio needs --principal")
	}
	return fl, cfgPath, principal, key, nil
}
