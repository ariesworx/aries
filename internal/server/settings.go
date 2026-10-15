package server

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/lazyregexp"
	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
	"github.com/ariesworx/aries/internal/telemetry"
	"go.yaml.in/yaml/v3"
)

// DefaultConfigFile is read when it exists and no --config is given.
const DefaultConfigFile = "/etc/aries/ariesd.yaml"

// Settings configure the ariesd commands: serve, stdio, upgrade, and the
// admin commands control init, import-bd and export-bd.
type Settings struct {
	// DSN is the Postgres database, as a postgres:// URL whose
	// search_path names the schema of the one project (public when it
	// names none; store.ParseDSN). It may hold a password, so it never
	// comes from a command-line flag with one; the PG* environment
	// variables and the role's .pgpass are read too.
	DSN string `yaml:"dsn"`
	// Socket is the daemon's unix socket. Default DefaultSocket.
	Socket string `yaml:"socket"`
	// ControlDB names the control store's schema, in DSN's database and
	// as its role, and makes this a server of many projects (design
	// §15): each project's schema is then named by its row there, and
	// DSN's, Project, Prefix and Account describe the project `ariesd
	// control init` adopts, which the admin commands act on by default.
	// Empty, the default, is a server of the one project in DSN. (The
	// name is from when each was a database of its own.)
	ControlDB string `yaml:"control_db"`
	// ProjectDBs is the block of schema names for the projects `aries
	// admin project add` creates, which DSN's role makes in its
	// database. It needs ControlDB.
	ProjectDBs ProjectDBs `yaml:"project_dbs"`
	// KeysFile is the file a server of many projects writes the control
	// database's keys to, in authorized_keys form, for sshd to read
	// (design §15.2). AuthorizedKeys is the file of that form the
	// operator maintains by hand, which the server reads to say which
	// keys are in both, and never writes. A relative path is under the
	// home directory of the user ariesd runs as ([Settings.KeyFiles]).
	// Defaults DefaultKeysFile and DefaultAuthorizedKeys. A server of
	// one project uses neither.
	KeysFile       string `yaml:"keys_file"`
	AuthorizedKeys string `yaml:"authorized_keys"`
	// Bridge is the path of ariesd in the forced command of each line
	// of KeysFile. Default DefaultBridge.
	Bridge string `yaml:"bridge"`
	// Project is the UUID of the project served.
	Project string `yaml:"project"`
	// Prefix is the issue-ID prefix for generated IDs. Default "sf".
	Prefix string `yaml:"prefix"`
	// Latest is the latest release, set by hand on air-gapped servers.
	Latest string `yaml:"latest"`
	// SystemdUnit is the unit `ariesd upgrade` restarts. Empty means
	// upgrade installs the binary and leaves the restart to the admin,
	// unless --restart is given.
	SystemdUnit string `yaml:"systemd_unit"`
	// Releases is where `ariesd upgrade` reads releases from, when the
	// environment's ARIES_RELEASES does not say: a location
	// release.Open accepts. Empty means the default compiled in. Upgrade
	// checks it; the daemon never reads it.
	Releases string `yaml:"releases"`
	// LogLevel is the least severe level logged: debug, info, warn or
	// error. Default info; debug adds a line per request.
	LogLevel string `yaml:"log_level"`
	// LogFormat is text (slog's key=value) or json. Default text.
	LogFormat string `yaml:"log_format"`
	// Admins are the principals who may change issues others hold and
	// force a close (decision D2). They are set here, or in
	// ARIESD_ADMINS as a comma-separated list, and nowhere else:
	// nothing over the protocol reads or changes them. None may be
	// reserved (store.ReservedPrincipals).
	Admins []string `yaml:"admins"`
	// AllowUnsafeAccount opens the store even when its database role is
	// unsafe (store.Options.AllowUnsafeAccount, S-2). Only a command-line
	// flag sets it, and only with --dev (decision D3); no config file or
	// environment variable can.
	AllowUnsafeAccount bool `yaml:"-"`
	// Limits bound what one principal can make the server do; see
	// [Limits]. Zero fields take the defaults.
	Limits Limits `yaml:"limits"`
	// Account is the project's default account (design §12.1): what an
	// issue reports time and tokens against when neither it nor an
	// ancestor sets one. A code name: lowercase letters, digits and inner
	// hyphens. Default DefaultAccount.
	Account string `yaml:"account"`
	// ClockBehind is what serve does when it finds its clock behind the
	// history of a database it serves by more than ClockBehindTolerance
	// (design §14, decision 37): ClockStop, the default, refuses to
	// start, and ClockWarn starts with a warning that repeats.
	// ClockBehindTolerance defaults to DefaultBehindTolerance.
	ClockBehind          string    `yaml:"clock_behind"`
	ClockBehindTolerance Tolerance `yaml:"clock_behind_tolerance"`
	// ClockStepTolerance is how far elapsed wall-clock time and elapsed
	// monotonic time may differ before the daemon calls it a step of the
	// clock. Default store.DefaultStepTolerance.
	ClockStepTolerance Tolerance `yaml:"clock_step_tolerance"`
	// ClockClientTolerance is how far past the server's clock a time a
	// client supplies, a usage record's, may be. Default
	// store.DefaultClientAhead.
	ClockClientTolerance Tolerance `yaml:"clock_client_tolerance"`
	// AcceptClockBehind lets serve start once although its clock is
	// behind a history no later than this (Config.AcceptHistory). Only a
	// command-line flag sets it: an answer to one refusal must not be
	// left in a file to answer the next.
	AcceptClockBehind time.Time `yaml:"-"`
	// Telemetry is what is Aries's own of the daemon's telemetry
	// (design §16.3); where metrics are pushed is in the standard OTEL_*
	// environment variables, not here.
	Telemetry TelemetrySettings `yaml:"telemetry"`
}

// Tolerance is a YAML duration that must be more than zero: a tolerance
// of nothing would call every reading of the clock wrong. Left out, it
// takes its default.
type Tolerance time.Duration

// UnmarshalText reads a duration in proto.ParseDuration's form, which
// refuses one that is not positive.
func (t *Tolerance) UnmarshalText(b []byte) error {
	v, err := proto.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("a clock tolerance of %q is not a duration of more than zero; fix: set it to one such as 5s or 1m, or leave it out for the default", b)
	}
	*t = Tolerance(v)
	return nil
}

// TelemetrySettings are the telemetry: settings of ariesd's config.
type TelemetrySettings struct {
	// Listen is the loopback address, HOST:PORT, of the listener that
	// serves /metrics, /healthz and /readyz. Empty, the default, opens no
	// listener. An address that is not loopback is refused: the listener
	// has no authentication.
	Listen string `yaml:"listen"`
	// PerProject says whether metrics name the project on the
	// instruments that have that attribute. Nil, the default, is on;
	// false leaves the project off every instrument, for a server with
	// many projects.
	PerProject *bool `yaml:"per_project"`
	// Principals is how a person appears on a span and on a log record
	// exported over OTLP (design §16.4, decision 32):
	// telemetry.PrincipalsPseudonym, the default, a stable stand-in for
	// the principal's name and for the session's id; PrincipalsName, the
	// names themselves; or PrincipalsOmit, nothing. It does not change
	// the daemon's log on stderr, nor the audit stream there.
	Principals string `yaml:"principals"`
	// PrincipalsKey is the file that holds the key the stand-ins are
	// made with. ariesd makes it, mode 0600, the first time it exports
	// traces with stand-ins. A relative path is under the home directory
	// of the user ariesd runs as ([Settings.PrincipalsKey]). Default
	// DefaultPrincipalsKey.
	PrincipalsKey string `yaml:"principals_key"`
	// AuditLog says whether each event of the control store's log that
	// changes who has access to what is also written as a log line with
	// audit=true (design §16.4, decision 30). Nil, the default, is on.
	// The event log is the record whatever this says.
	AuditLog *bool `yaml:"audit_log"`
}

// DefaultPrincipalsKey is where the stand-in key is kept, under the home
// directory of the user ariesd runs as: a directory of its own, so
// that the unit can let the daemon write there and nowhere else in the
// home, where authorized_keys is.
const DefaultPrincipalsKey = "telemetry/principals.key"

// ByProject reports whether metrics name the project: unless
// per_project is false.
func (t TelemetrySettings) ByProject() bool { return t.PerProject == nil || *t.PerProject }

// Audit reports whether the audit stream is written: unless audit_log
// is false.
func (t TelemetrySettings) Audit() bool { return t.AuditLog == nil || *t.AuditLog }

// DefaultAccount is the account an issue reports against when nothing
// sets one.
const DefaultAccount = "internal"

// UnitPattern is what a systemd unit name may look like. It cannot start
// with "-", so it never reaches systemctl as an option.
var UnitPattern = lazyregexp.New(`^[A-Za-z0-9][A-Za-z0-9@._:-]{0,127}$`)

// Environment variables read by ResolveSettings.
const (
	EnvDSN     = "ARIESD_DSN"
	EnvSocket  = "ARIESD_SOCKET"
	EnvProject = "ARIESD_PROJECT"
	EnvConfig  = "ARIESD_CONFIG"
	// EnvLogLevel and EnvLogFormat set LogLevel and LogFormat.
	EnvLogLevel  = "ARIESD_LOG_LEVEL"
	EnvLogFormat = "ARIESD_LOG_FORMAT"
	// EnvAdmins sets Admins, comma-separated.
	EnvAdmins = "ARIESD_ADMINS"
	// EnvControlDB sets ControlDB.
	EnvControlDB = "ARIESD_CONTROL_DB"
	// EnvKeysFile, EnvAuthorizedKeys and EnvBridge set KeysFile,
	// AuthorizedKeys and Bridge.
	EnvKeysFile       = "ARIESD_KEYS_FILE"
	EnvAuthorizedKeys = "ARIESD_AUTHORIZED_KEYS"
	EnvBridge         = "ARIESD_BRIDGE"
	// EnvClockBehind sets ClockBehind.
	EnvClockBehind = "ARIESD_CLOCK_BEHIND"
	// EnvTelemetryListen sets Telemetry.Listen.
	EnvTelemetryListen = "ARIESD_TELEMETRY_LISTEN"
)

// ResolveSettings merges, highest precedence first: flags, the environment,
// the config file, and defaults. configPath "" means ARIESD_CONFIG, else
// DefaultConfigFile if it exists; a named file must exist. Limits and
// ProjectDBs come from the file only, Admins from the file unless ARIESD_ADMINS replaces
// them, Telemetry from the file with its listen address from
// ARIESD_TELEMETRY_LISTEN when that is set, and AllowUnsafeAccount from
// flags only. It refuses a password in
// flags.DSN, which the process list would show; a config file with an
// unknown key, or with a password and any access for group or others; an
// admin name that is invalid or reserved; an invalid limit; an account
// that is not a code name; a control schema that is not a schema name,
// or is the DSN's own; a block of project schema names that is invalid,
// set without a control schema, or holds its name; a systemd
// unit that is not a unit name; a bridge path that would need quoting in
// a forced command; a keys file that is the authorized_keys file; a
// telemetry listen address that is not HOST:PORT or is an IP address
// that is not loopback (a host name is resolved, and checked, when the
// daemon starts); a telemetry principals that is not pseudonym, name or
// omit; a clock_behind that is not stop or warn; a clock tolerance out of its
// range; and an unknown log level or format. The clock tolerances come
// from the file only.
func ResolveSettings(flags Settings, configPath string, getenv func(string) string) (Settings, error) {
	if flags.DSN != "" {
		d, err := store.ParseDSN(flags.DSN)
		if err != nil {
			return Settings{}, fmt.Errorf("--dsn: %w", err)
		}
		if d.Password {
			return Settings{}, fmt.Errorf("--dsn holds a password, which would show in the process list; fix: set %s or dsn: in the config file instead", EnvDSN)
		}
	}
	file, err := configFile(configPath, getenv)
	if err != nil {
		return Settings{}, err
	}
	env := Settings{DSN: getenv(EnvDSN), Socket: getenv(EnvSocket), Project: getenv(EnvProject),
		LogLevel: getenv(EnvLogLevel), LogFormat: getenv(EnvLogFormat), ControlDB: getenv(EnvControlDB),
		KeysFile: getenv(EnvKeysFile), AuthorizedKeys: getenv(EnvAuthorizedKeys), Bridge: getenv(EnvBridge),
		ClockBehind: getenv(EnvClockBehind)}
	out := Settings{Socket: DefaultSocket, Prefix: "sf", LogLevel: "info", LogFormat: "text", Account: DefaultAccount,
		KeysFile: DefaultKeysFile, AuthorizedKeys: DefaultAuthorizedKeys, Bridge: DefaultBridge, ClockBehind: ClockStop}
	for _, s := range []Settings{file, env, flags} {
		for _, f := range []struct {
			dst *string
			v   string
		}{
			{&out.DSN, s.DSN}, {&out.Socket, s.Socket}, {&out.Project, s.Project},
			{&out.Prefix, s.Prefix}, {&out.Latest, s.Latest}, {&out.SystemdUnit, s.SystemdUnit},
			{&out.Releases, s.Releases},
			{&out.LogLevel, s.LogLevel}, {&out.LogFormat, s.LogFormat}, {&out.Account, s.Account},
			{&out.ControlDB, s.ControlDB}, {&out.KeysFile, s.KeysFile}, {&out.AuthorizedKeys, s.AuthorizedKeys},
			{&out.Bridge, s.Bridge}, {&out.ClockBehind, s.ClockBehind},
		} {
			if f.v != "" {
				*f.dst = f.v
			}
		}
	}
	out.AllowUnsafeAccount = flags.AllowUnsafeAccount
	out.AcceptClockBehind = flags.AcceptClockBehind
	if err := out.resolveClock(file); err != nil {
		return Settings{}, err
	}
	out.Limits = file.Limits
	out.ProjectDBs = file.ProjectDBs
	out.Telemetry = file.Telemetry
	if v := getenv(EnvTelemetryListen); v != "" {
		out.Telemetry.Listen = v
	}
	if out.Telemetry.Listen != "" {
		if err := telemetry.CheckAddr(out.Telemetry.Listen); err != nil {
			return Settings{}, err
		}
	}
	if out.Telemetry.Principals == "" {
		out.Telemetry.Principals = telemetry.PrincipalsPseudonym
	}
	if !telemetry.ValidPrincipals(out.Telemetry.Principals) {
		return Settings{}, fmt.Errorf("telemetry: principals: %q is not %s, %s or %s; fix: set it to one of them, or leave it out for %s, a stable stand-in for each name",
			out.Telemetry.Principals, telemetry.PrincipalsPseudonym, telemetry.PrincipalsName, telemetry.PrincipalsOmit, telemetry.PrincipalsPseudonym)
	}
	if out.Telemetry.PrincipalsKey == "" {
		out.Telemetry.PrincipalsKey = DefaultPrincipalsKey
	}
	out.Admins = file.Admins
	if v := getenv(EnvAdmins); v != "" {
		out.Admins = nil
		for a := range strings.SplitSeq(v, ",") {
			if a = strings.TrimSpace(a); a != "" {
				out.Admins = append(out.Admins, a)
			}
		}
	}
	for _, a := range out.Admins {
		if !PrincipalPattern.MatchString(a) || store.Reserved(a) {
			return Settings{}, fmt.Errorf("admin %q is not a principal name, or is reserved for the server; fix: list the admins' principal names under admins: or in %s", a, EnvAdmins)
		}
	}
	if err := out.Limits.Validate(); err != nil {
		return Settings{}, err
	}
	out.Limits = out.Limits.WithDefaults()
	if !store.ValidAccount(out.Account) {
		return Settings{}, fmt.Errorf("account %q is not a code name; fix: set account: to 1-64 lowercase letters, digits and inner hyphens, such as %s", out.Account, DefaultAccount)
	}
	if err := out.checkControlDB(); err != nil {
		return Settings{}, err
	}
	if err := out.checkProjectDBs(); err != nil {
		return Settings{}, err
	}
	if !ValidBridge(out.Bridge) {
		return Settings{}, fmt.Errorf("bridge %q cannot be written into a forced command; fix: set bridge: or %s to the absolute path of ariesd, of letters, digits and ._+-, such as %s",
			out.Bridge, EnvBridge, DefaultBridge)
	}
	if filepath.Clean(out.KeysFile) == filepath.Clean(out.AuthorizedKeys) {
		return Settings{}, fmt.Errorf("keys_file %q is the authorized_keys file, which ariesd reads and never writes; fix: set keys_file: or %s to a file of its own beside it, such as %s",
			out.KeysFile, EnvKeysFile, DefaultKeysFile)
	}
	if out.SystemdUnit != "" && !UnitPattern.MatchString(out.SystemdUnit) {
		return Settings{}, fmt.Errorf("systemd_unit %q is not a unit name; fix: set it to the service's name, for example ariesd.service", out.SystemdUnit)
	}
	if _, err := NewLogger(io.Discard, out.LogLevel, out.LogFormat); err != nil {
		return Settings{}, err
	}
	return out, nil
}

// resolveClock takes the clock tolerances from file, each with its
// default, and refuses a clock_behind that is not stop or warn and a
// tolerance past its bound. A bound is refused, not warned about: a
// tolerance of days turns its check off without saying so, and
// clock_behind: warn is the way to start whatever the clock says.
func (s *Settings) resolveClock(file Settings) error {
	if s.ClockBehind != ClockStop && s.ClockBehind != ClockWarn {
		return fmt.Errorf("clock_behind %q is not %s or %s; fix: set clock_behind: or %s to %s, to refuse to start when the clock is behind the server's own history, or to %s, to start with a warning",
			s.ClockBehind, ClockStop, ClockWarn, EnvClockBehind, ClockStop, ClockWarn)
	}
	for _, f := range []struct {
		name     string
		dst      *Tolerance
		v        Tolerance
		def, max time.Duration
	}{
		{"clock_behind_tolerance", &s.ClockBehindTolerance, file.ClockBehindTolerance, DefaultBehindTolerance, maxBehindTolerance},
		{"clock_step_tolerance", &s.ClockStepTolerance, file.ClockStepTolerance, store.DefaultStepTolerance, maxStepTolerance},
		{"clock_client_tolerance", &s.ClockClientTolerance, file.ClockClientTolerance, store.DefaultClientAhead, store.MaxClientAhead},
	} {
		*f.dst = cmp.Or(f.v, Tolerance(f.def))
		if d := time.Duration(*f.dst); d < minClockTolerance || d > f.max {
			return fmt.Errorf("%s is %s; it must be from %s to %s; fix: set it within that range in the config file, or leave it out for the default, %s",
				f.name, d, minClockTolerance, f.max, f.def)
		}
	}
	return nil
}

// checkControlDB refuses a control schema that is not a schema name, or
// that is the schema the DSN names: that one holds a project.
func (s Settings) checkControlDB() error {
	if s.ControlDB == "" {
		return nil
	}
	if !store.ValidDBName(s.ControlDB) {
		return fmt.Errorf("control_db %q is not a schema name; fix: set control_db: or %s to a lowercase letter, then lowercase letters, digits and underscores, not starting pg_, such as aries_control",
			s.ControlDB, EnvControlDB)
	}
	if d, err := store.ParseDSN(s.DSN); err == nil && d.Schema == s.ControlDB {
		return fmt.Errorf("control_db %q is the schema the DSN names, which holds a project; fix: set control_db: to a schema of its own, such as aries_control",
			s.ControlDB)
	}
	return nil
}

// checkProjectDBs refuses a block of project schema names that is not
// valid, that is set on a server of one project, or that holds the
// control store's name. It may hold the DSN's schema: that one is the
// adopted project's, and a name a project has is never given again.
func (s Settings) checkProjectDBs() error {
	b := s.ProjectDBs
	if err := b.Validate(); err != nil {
		return err
	}
	switch {
	case b == ProjectDBs{}:
		return nil
	case s.ControlDB == "":
		return errors.New("project_dbs is set without control_db; fix: set control_db: to serve many projects, or remove project_dbs: (docs/server.md, Serving many projects)")
	case b.holds(s.ControlDB):
		return fmt.Errorf("project_dbs holds %s, the control store's schema; fix: choose a prefix of its own, such as aries_p", s.ControlDB)
	}
	return nil
}

// KeyFiles returns the absolute paths of the keys file and of the
// hand-maintained authorized_keys file: a relative setting is under
// home, the home directory of the user ariesd runs as, whose
// authorized_keys sshd reads.
func (s Settings) KeyFiles(home string) (keysFile, authorizedKeys string) {
	abs := func(p string) string {
		if filepath.IsAbs(p) {
			return filepath.Clean(p)
		}
		return filepath.Join(home, p)
	}
	return abs(s.KeysFile), abs(s.AuthorizedKeys)
}

// PrincipalsKey returns the absolute path of the stand-in key's file
// (telemetry: principals_key:): a relative setting is under home, the
// home directory of the user ariesd runs as.
func (s Settings) PrincipalsKey(home string) string {
	if p := s.Telemetry.PrincipalsKey; filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(home, s.Telemetry.PrincipalsKey)
}

// ControlDSN returns the DSN of the control store's schema (ControlDB),
// which must be set.
func (s Settings) ControlDSN() (string, error) { return SchemaDSN(s.DSN, s.ControlDB) }

// NewLogger returns ariesd's logger, writing to w at level (debug,
// info, warn or error) in format (text or json). Its handler is a
// [*logs.Handler]: a line logged with a request's context has the
// request's id and the ids of its span, and the lines can be given to a
// second handler ([logs.Handler.Tap]), which is how they are exported.
func NewLogger(w io.Writer, level, format string) (*slog.Logger, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil || !slices.Contains([]string{"debug", "info", "warn", "error"}, level) {
		return nil, fmt.Errorf("log level %q is not debug, info, warn or error; fix: set log_level: or %s to one of them", level, EnvLogLevel)
	}
	opts := &slog.HandlerOptions{Level: l}
	switch format {
	case "text":
		return slog.New(logs.NewHandler(slog.NewTextHandler(w, opts), LogIDs)), nil
	case "json":
		return slog.New(logs.NewHandler(slog.NewJSONHandler(w, opts), LogIDs)), nil
	}
	return nil, fmt.Errorf("log format %q is not text or json; fix: set log_format: or %s to text or json", format, EnvLogFormat)
}

// ResolveSocket returns the daemon's socket for `ariesd stdio`, from
// where ResolveSettings takes it: flag, then $ARIESD_SOCKET, then
// socket: in the config file, then DefaultSocket. It reads the file as
// ResolveSettings does, and refuses what that refuses of the file itself
// (an unknown key, a password in a file others can read), but checks no
// other setting. The bridge uses none of them, and sshd starts one for
// every connection: checking them compiled regular expressions that each
// bridge then held (docs/design/profile-ariesd.md, proposal 2).
func ResolveSocket(flag, configPath string, getenv func(string) string) (string, error) {
	file, err := configFile(configPath, getenv)
	if err != nil {
		return "", err
	}
	for _, s := range []string{flag, getenv(EnvSocket), file.Socket} {
		if s != "" {
			return s, nil
		}
	}
	return DefaultSocket, nil
}

// configFile reads the config file at configPath, or else at
// $ARIESD_CONFIG, either of which must exist, or else at
// DefaultConfigFile, which may not.
func configFile(configPath string, getenv func(string) string) (Settings, error) {
	required := true
	if configPath == "" {
		configPath = getenv(EnvConfig)
	}
	if configPath == "" {
		configPath, required = DefaultConfigFile, false
	}
	return loadSettings(configPath, required)
}

// loadSettings reads the config file at path, refusing unknown keys. A
// missing file is not an error unless required. A file whose DSN holds a
// password is refused if its group or others have any access to it.
func loadSettings(path string, required bool) (Settings, error) {
	var s Settings
	b, err := os.ReadFile(path) //nolint:gosec // the admin names the config file
	if errors.Is(err, fs.ErrNotExist) && !required {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("config: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil && !errors.Is(err, io.EOF) {
		return s, fmt.Errorf("config %s: %w", path, err)
	}
	if s.DSN != "" {
		if d, err := store.ParseDSN(s.DSN); err == nil && d.Password {
			fi, err := os.Stat(path)
			if err != nil {
				return s, fmt.Errorf("config: %w", err)
			}
			if fi.Mode().Perm()&0o077 != 0 {
				return s, fmt.Errorf("config %s holds a database password but is mode %04o; fix: chmod 600 %s", path, fi.Mode().Perm(), path)
			}
		}
	}
	return s, nil
}
