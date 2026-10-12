// Command starfixd is the starfix server.
//
//	starfixd serve [--dev [--allow-unsafe-dolt]] [--config FILE] [--dsn DSN] [--socket PATH] [--project UUID] [--prefix P] [--log-level L] [--log-format F]
//	starfixd stdio --principal NAME [--key FINGERPRINT] [--config FILE] [--socket PATH]
//	starfixd control init --name NAME [--authorized-keys FILE] [--config FILE] [--dsn DSN]
//	starfixd import-bd [--config FILE] [--dsn DSN] [--project UUID] [--principal NAME] [--dry-run] [--json] FILE|-
//	starfixd export-bd [--config FILE] [--dsn DSN] [--project UUID] [-o FILE]
//	starfixd upgrade [--check] [--to vX.Y.Z] [--rollback] [--restart] [--config FILE] [--dsn DSN] [--socket PATH]
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
// daemon when a systemd unit is configured or --restart is given.
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
	"syscall"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/server"
	"github.com/ariesworx/starfix/internal/store"
	"github.com/ariesworx/starfix/internal/version"
)

const usage = `usage:
  starfixd serve [--dev [--allow-unsafe-dolt]] [--config FILE] [--dsn DSN] [--socket PATH] [--project UUID] [--prefix P] [--log-level L] [--log-format F]
  starfixd stdio --principal NAME [--key FINGERPRINT] [--config FILE] [--socket PATH]
  starfixd control init --name NAME [--authorized-keys FILE] [--config FILE] [--dsn DSN]
  starfixd import-bd [--config FILE] [--dsn DSN] [--project UUID] [--principal NAME] [--dry-run] [--json] FILE|-
  starfixd export-bd [--config FILE] [--dsn DSN] [--project UUID] [-o FILE]
  starfixd upgrade [--check] [--to vX.Y.Z] [--rollback] [--restart] [--config FILE] [--dsn DSN] [--socket PATH]
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
	fmt.Fprintf(os.Stderr, "starfixd: %v\n", err)
	if _, ok := errors.AsType[usageError](err); ok {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	os.Exit(1)
}

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
	case "version", "--version":
		fmt.Printf("starfixd %s (protocol %d-%d)\n", version.Version, proto.ProtoMin, proto.ProtoMax)
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
	var cfgPath string
	var dev bool
	if err := flags("serve", args, &fl, &cfgPath, func(fs *flag.FlagSet) {
		devFlags(fs, &dev, &fl.AllowUnsafeDolt)
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
	d, err := openDaemon(ctx, s, log)
	if err != nil {
		return err
	}
	l, err := server.Listen(s.Socket)
	if err != nil {
		return errors.Join(err, d.Close())
	}
	if d.ctl != nil {
		log.Info("starfixd serving", "version", version.Version, "socket", s.Socket, "control_db", s.ControlDB,
			"projects", d.ctl.ActiveProjects(), "admins", s.Admins)
	} else {
		log.Info("starfixd serving", "version", version.Version, "socket", s.Socket, "project", s.Project, "admins", s.Admins)
	}
	err = d.srv.Serve(ctx, l)
	return errors.Join(err, d.Close())
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
	cfg := server.Config{Version: version.Version, Latest: s.Latest, Logger: log, Limits: s.Limits, Account: s.Account}
	opts := store.Options{Logger: log, AllowUnsafeAccount: s.AllowUnsafeDolt, Limits: s.Limits.Limits}
	if s.DSN == "" {
		return nil, errors.New("no database DSN; fix: set $STARFIXD_DSN or dsn: in the config file")
	}
	if s.ControlDB == "" {
		if s.Project == "" {
			return nil, errors.New("no project; fix: set --project, $STARFIXD_PROJECT or project: in the config file")
		}
		opts.Prefix, opts.Admins = s.Prefix, s.Admins
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
	ctl, err := openControl(ctx, s, store.ControlOptions{Logger: log})
	if err != nil {
		return nil, err
	}
	if ctl.ActiveProjects() == 0 {
		return nil, errors.Join(fmt.Errorf("the control database %s holds no active project; fix: adopt this server's database as the first with `starfixd control init --name NAME`",
			s.ControlDB), ctl.Close())
	}
	cfg.Control, cfg.Open, cfg.ProjectDBs = ctl, server.ProjectOpener(s.DSN, ctl, opts), s.ProjectDBs
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
		return nil, errors.Join(fmt.Errorf("%w; fix: check that dolt sql-server is running", err), ctl.Close())
	}
	// Every claim in an archived project ends before the daemon listens
	// (decision 24): one left by a daemon that stopped between an
	// archive and its release would otherwise wait for the project to
	// be activated. An archived project's database is opened as it is
	// and never migrated here; activating the project does that.
	asIs := opts
	asIs.NoMigrate = true
	if err := srv.ReleaseArchived(ctx, server.ProjectOpener(s.DSN, ctl, asIs)); err != nil {
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
		u, err := user.Current()
		if err == nil && !filepath.IsAbs(u.HomeDir) {
			err = errors.New("the account has no home directory")
		}
		if err != nil {
			return "", "", fmt.Errorf("find the home directory of the user starfixd runs as: %w; fix: set keys_file: and authorized_keys: to absolute paths in the config file", err)
		}
		home = u.HomeDir
	}
	keysFile, authorizedKeys = s.KeyFiles(home)
	return keysFile, authorizedKeys, nil
}

// stdio runs `starfixd stdio`, the forced command: it bridges this
// process's stdin and stdout to the daemon's socket.
func stdio(ctx context.Context, args []string) error {
	var fl server.Settings
	var cfgPath, principal, key string
	if err := flags("stdio", args, &fl, &cfgPath, func(fs *flag.FlagSet) {
		fs.StringVar(&principal, "principal", "", "principal this key authenticates")
		fs.StringVar(&key, "key", "", "SHA256 fingerprint of the key, when its line is one starfixd wrote to its keys file")
	}); err != nil {
		return err
	}
	if principal == "" {
		return usageError("stdio needs --principal")
	}
	s, err := server.ResolveSettings(fl, cfgPath, os.Getenv)
	if err != nil {
		return err
	}
	return server.Bridge(ctx, s.Socket, principal, key, os.Stdin, os.Stdout)
}
