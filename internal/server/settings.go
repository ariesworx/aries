package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/ariesworx/starfix/internal/store"
	"github.com/go-sql-driver/mysql"
	"go.yaml.in/yaml/v3"
)

// DefaultConfigFile is read when it exists and no --config is given.
const DefaultConfigFile = "/etc/starfix/starfixd.yaml"

// Settings configure the starfixd commands: serve, stdio, upgrade, and the
// admin commands control init, import-bd and export-bd.
type Settings struct {
	// DSN is the Dolt database (go-sql-driver/mysql form). It may hold a
	// password, so it never comes from a command-line flag with one.
	DSN string `yaml:"dsn"`
	// Socket is the daemon's unix socket. Default DefaultSocket.
	Socket string `yaml:"socket"`
	// ControlDB names the control database, on the same Dolt server and
	// account as DSN, and makes this a server of many projects (design
	// §15): each project's database is then named by its row there, and
	// DSN's, Project, Prefix and Account describe the project `starfixd
	// control init` adopts, which the admin commands act on by default.
	// Empty, the default, is a server of the one project in DSN.
	ControlDB string `yaml:"control_db"`
	// ProjectDBs is the block of database names granted to DSN's account
	// for the projects `sfx admin project add` creates. It needs
	// ControlDB.
	ProjectDBs ProjectDBs `yaml:"project_dbs"`
	// KeysFile is the file a server of many projects writes the control
	// database's keys to, in authorized_keys form, for sshd to read
	// (design §15.2). AuthorizedKeys is the file of that form the
	// operator maintains by hand, which the server reads to say which
	// keys are in both, and never writes. A relative path is under the
	// home directory of the user starfixd runs as ([Settings.KeyFiles]).
	// Defaults DefaultKeysFile and DefaultAuthorizedKeys. A server of
	// one project uses neither.
	KeysFile       string `yaml:"keys_file"`
	AuthorizedKeys string `yaml:"authorized_keys"`
	// Bridge is the path of starfixd in the forced command of each line
	// of KeysFile. Default DefaultBridge.
	Bridge string `yaml:"bridge"`
	// Project is the UUID of the project served.
	Project string `yaml:"project"`
	// Prefix is the issue-ID prefix for generated IDs. Default "sf".
	Prefix string `yaml:"prefix"`
	// Latest is the latest release, set by hand on air-gapped servers.
	Latest string `yaml:"latest"`
	// SystemdUnit is the unit `starfixd upgrade` restarts. Empty means
	// upgrade installs the binary and leaves the restart to the admin,
	// unless --restart is given.
	SystemdUnit string `yaml:"systemd_unit"`
	// LogLevel is the least severe level logged: debug, info, warn or
	// error. Default info; debug adds a line per request.
	LogLevel string `yaml:"log_level"`
	// LogFormat is text (slog's key=value) or json. Default text.
	LogFormat string `yaml:"log_format"`
	// Admins are the principals who may change issues others hold and
	// force a close (decision D2). They are set here, or in
	// STARFIXD_ADMINS as a comma-separated list, and nowhere else:
	// nothing over the protocol reads or changes them. None may be
	// reserved (store.ReservedPrincipals).
	Admins []string `yaml:"admins"`
	// AllowUnsafeDolt opens the store even when its Dolt account is
	// unsafe (store.Options.AllowUnsafeAccount, S-2). Only a command-line
	// flag sets it, and only with --dev (decision D3); no config file or
	// environment variable can.
	AllowUnsafeDolt bool `yaml:"-"`
	// Limits bound what one principal can make the server do; see
	// [Limits]. Zero fields take the defaults.
	Limits Limits `yaml:"limits"`
	// Account is the project's default account (design §12.1): what an
	// issue reports time and tokens against when neither it nor an
	// ancestor sets one. A code name: lowercase letters, digits and inner
	// hyphens. Default DefaultAccount.
	Account string `yaml:"account"`
}

// DefaultAccount is the account an issue reports against when nothing
// sets one.
const DefaultAccount = "internal"

// UnitPattern is what a systemd unit name may look like. It cannot start
// with "-", so it never reaches systemctl as an option.
var UnitPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._:-]{0,127}$`)

// Environment variables read by ResolveSettings.
const (
	EnvDSN     = "STARFIXD_DSN"
	EnvSocket  = "STARFIXD_SOCKET"
	EnvProject = "STARFIXD_PROJECT"
	EnvConfig  = "STARFIXD_CONFIG"
	// EnvLogLevel and EnvLogFormat set LogLevel and LogFormat.
	EnvLogLevel  = "STARFIXD_LOG_LEVEL"
	EnvLogFormat = "STARFIXD_LOG_FORMAT"
	// EnvAdmins sets Admins, comma-separated.
	EnvAdmins = "STARFIXD_ADMINS"
	// EnvControlDB sets ControlDB.
	EnvControlDB = "STARFIXD_CONTROL_DB"
	// EnvKeysFile, EnvAuthorizedKeys and EnvBridge set KeysFile,
	// AuthorizedKeys and Bridge.
	EnvKeysFile       = "STARFIXD_KEYS_FILE"
	EnvAuthorizedKeys = "STARFIXD_AUTHORIZED_KEYS"
	EnvBridge         = "STARFIXD_BRIDGE"
)

// ResolveSettings merges, highest precedence first: flags, the environment,
// the config file, and defaults. configPath "" means STARFIXD_CONFIG, else
// DefaultConfigFile if it exists; a named file must exist. Limits and
// ProjectDBs come from the file only, Admins from the file unless STARFIXD_ADMINS replaces
// them, and AllowUnsafeDolt from flags only. It refuses a password in
// flags.DSN, which the process list would show; a config file with an
// unknown key, or with a password and any access for group or others; an
// admin name that is invalid or reserved; an invalid limit; an account
// that is not a code name; a control database that is not a database
// name, or is the DSN's own; a block of project database names that is
// invalid, set without a control database, or holds its name; a systemd
// unit that is not a unit name; a bridge path that would need quoting in
// a forced command; a keys file that is the authorized_keys file; and
// an unknown log level or format.
func ResolveSettings(flags Settings, configPath string, getenv func(string) string) (Settings, error) {
	if flags.DSN != "" {
		cfg, err := mysql.ParseDSN(flags.DSN)
		if err != nil {
			return Settings{}, fmt.Errorf("--dsn: %w", err)
		}
		if cfg.Passwd != "" {
			return Settings{}, fmt.Errorf("--dsn holds a password, which would show in the process list; fix: set %s or dsn: in the config file instead", EnvDSN)
		}
	}
	required := true
	if configPath == "" {
		configPath = getenv(EnvConfig)
	}
	if configPath == "" {
		configPath, required = DefaultConfigFile, false
	}
	file, err := loadSettings(configPath, required)
	if err != nil {
		return Settings{}, err
	}
	env := Settings{DSN: getenv(EnvDSN), Socket: getenv(EnvSocket), Project: getenv(EnvProject),
		LogLevel: getenv(EnvLogLevel), LogFormat: getenv(EnvLogFormat), ControlDB: getenv(EnvControlDB),
		KeysFile: getenv(EnvKeysFile), AuthorizedKeys: getenv(EnvAuthorizedKeys), Bridge: getenv(EnvBridge)}
	out := Settings{Socket: DefaultSocket, Prefix: "sf", LogLevel: "info", LogFormat: "text", Account: DefaultAccount,
		KeysFile: DefaultKeysFile, AuthorizedKeys: DefaultAuthorizedKeys, Bridge: DefaultBridge}
	for _, s := range []Settings{file, env, flags} {
		for _, f := range []struct {
			dst *string
			v   string
		}{
			{&out.DSN, s.DSN}, {&out.Socket, s.Socket}, {&out.Project, s.Project},
			{&out.Prefix, s.Prefix}, {&out.Latest, s.Latest}, {&out.SystemdUnit, s.SystemdUnit},
			{&out.LogLevel, s.LogLevel}, {&out.LogFormat, s.LogFormat}, {&out.Account, s.Account},
			{&out.ControlDB, s.ControlDB}, {&out.KeysFile, s.KeysFile}, {&out.AuthorizedKeys, s.AuthorizedKeys},
			{&out.Bridge, s.Bridge},
		} {
			if f.v != "" {
				*f.dst = f.v
			}
		}
	}
	out.AllowUnsafeDolt = flags.AllowUnsafeDolt
	out.Limits = file.Limits
	out.ProjectDBs = file.ProjectDBs
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
		return Settings{}, fmt.Errorf("bridge %q cannot be written into a forced command; fix: set bridge: or %s to the absolute path of starfixd, of letters, digits and ._+-, such as %s",
			out.Bridge, EnvBridge, DefaultBridge)
	}
	if filepath.Clean(out.KeysFile) == filepath.Clean(out.AuthorizedKeys) {
		return Settings{}, fmt.Errorf("keys_file %q is the authorized_keys file, which starfixd reads and never writes; fix: set keys_file: or %s to a file of its own beside it, such as %s",
			out.KeysFile, EnvKeysFile, DefaultKeysFile)
	}
	if out.SystemdUnit != "" && !UnitPattern.MatchString(out.SystemdUnit) {
		return Settings{}, fmt.Errorf("systemd_unit %q is not a unit name; fix: set it to the service's name, for example starfixd.service", out.SystemdUnit)
	}
	if _, err := NewLogger(io.Discard, out.LogLevel, out.LogFormat); err != nil {
		return Settings{}, err
	}
	return out, nil
}

// checkControlDB refuses a control database that is not a database name,
// or that is the database the DSN names: that one holds a project.
func (s Settings) checkControlDB() error {
	if s.ControlDB == "" {
		return nil
	}
	if !store.ValidDBName(s.ControlDB) {
		return fmt.Errorf("control_db %q is not a database name; fix: set control_db: or %s to a lowercase letter, then lowercase letters, digits and underscores, such as starfix_control",
			s.ControlDB, EnvControlDB)
	}
	if cfg, err := mysql.ParseDSN(s.DSN); err == nil && cfg.DBName == s.ControlDB {
		return fmt.Errorf("control_db %q is the database in the DSN, which holds a project; fix: set control_db: to a database of its own, such as starfix_control",
			s.ControlDB)
	}
	return nil
}

// checkProjectDBs refuses a block of project database names that is not
// valid, that is set on a server of one project, or that holds the
// control database's name. It may hold the DSN's database: that one is
// the adopted project's, and a name a project has is never given again.
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
		return fmt.Errorf("project_dbs holds %s, the control database; fix: choose a prefix of its own, such as starfix_p", s.ControlDB)
	}
	return nil
}

// KeyFiles returns the absolute paths of the keys file and of the
// hand-maintained authorized_keys file: a relative setting is under
// home, the home directory of the user starfixd runs as, whose
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

// ControlDSN returns the DSN of the control database (ControlDB), which
// must be set.
func (s Settings) ControlDSN() (string, error) { return DatabaseDSN(s.DSN, s.ControlDB) }

// NewLogger returns a logger writing to w at level (debug, info, warn or
// error) in format (text or json).
func NewLogger(w io.Writer, level, format string) (*slog.Logger, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil || !slices.Contains([]string{"debug", "info", "warn", "error"}, level) {
		return nil, fmt.Errorf("log level %q is not debug, info, warn or error; fix: set log_level: or %s to one of them", level, EnvLogLevel)
	}
	opts := &slog.HandlerOptions{Level: l}
	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	}
	return nil, fmt.Errorf("log format %q is not text or json; fix: set log_format: or %s to text or json", format, EnvLogFormat)
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
		if cfg, err := mysql.ParseDSN(s.DSN); err == nil && cfg.Passwd != "" {
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
