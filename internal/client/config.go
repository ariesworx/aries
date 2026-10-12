package client

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/ariesworx/starfix/internal/proto"
)

// ConfigFile is the committed, per-repository configuration. It holds no
// secrets.
const ConfigFile = ".starfix.yaml"

// Config is .starfix.yaml.
type Config struct {
	// Project is the project's UUID.
	Project string `yaml:"project"`
	Server  struct {
		// Host is the server's host name or address. Through IAP it is
		// not dialed, and names the instance unless IAP.Instance does.
		Host string `yaml:"host"`
		// Port defaults to 22.
		Port int `yaml:"port"`
		// User is the SSH account starfixd runs as. Default "starfix".
		User string `yaml:"user"`
		// HostKey is the server's pinned ED25519 fingerprint, SHA256:….
		HostKey string `yaml:"host_key"`
		// IAP, when set, reaches the server through Google Cloud IAP TCP
		// forwarding instead of dialing Host directly.
		IAP *IAP `yaml:"iap"`
	} `yaml:"server"`
	// Key is an optional key file inside the repository, relative to its
	// root (KeyPath). It is then the only key offered; without it, and
	// without EnvKeyFile, ssh-agent's keys are.
	Key string `yaml:"key"`

	// Root is the directory holding the config file (not read from YAML).
	Root string `yaml:"-"`
}

// IAP names the Compute Engine instance that Dial reaches through
// Identity-Aware Proxy TCP forwarding, by running `gcloud compute
// start-iap-tunnel`. Each field becomes an argument to gcloud, so each is
// held to Google Cloud's own naming rules.
//
// There is deliberately no general proxy command: .starfix.yaml is
// committed, so a command in it would run whatever a cloned repository
// says. IAP is the one transport supported this way; another would be
// added the same way, as validated names rather than a command line.
type IAP struct {
	// Project is the Google Cloud project id.
	Project string `yaml:"project"`
	// Zone is the instance's zone, such as us-central1-a.
	Zone string `yaml:"zone"`
	// Instance is the instance name. Default: server.host.
	Instance string `yaml:"instance"`
}

var (
	gcpProjectRE  = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	gcpZoneRE     = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]+-[a-z]$`)
	gcpInstanceRE = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
)

var (
	uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	hostRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.:-]{0,252}$`)
	userRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	fprRE  = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)
)

// ErrNoConfig matches, with errors.Is, LoadConfig's error when neither
// the directory nor any parent holds ConfigFile. The error is also a
// *proto.Error for errors.As.
var ErrNoConfig = errors.New(ConfigFile + " not found")

// noConfigError is LoadConfig's error when it finds no config: a
// *proto.Error through Unwrap, which also matches ErrNoConfig.
type noConfigError struct{ pe *proto.Error }

func (e noConfigError) Error() string { return e.pe.Error() }

func (e noConfigError) Unwrap() error { return e.pe }

func (noConfigError) Is(target error) bool { return target == ErrNoConfig }

// LoadConfig finds .starfix.yaml in dir or the nearest parent that has
// one. The search stops at the repository's top level (the first
// directory holding .git, a directory or a worktree's file) and at the
// home directory, never above them: a config in a shared parent such as
// /tmp, or a drive root on Windows, must not capture a checkout that has
// none of its own. The file found must be a regular file; on Unix it
// must also be the user's own and writable by no one else, since it
// names the server every command and hook connects to. Windows has no
// owner or mode check here (its ACLs are not Unix bits), so there the
// boundary is the only guard.
func LoadConfig(dir string) (*Config, error) {
	home, _ := os.UserHomeDir() // no home: the search stops at .git or the root
	return loadConfig(dir, home)
}

// loadConfig is LoadConfig with the home directory as a parameter, so
// tests can place it; "" sets no home boundary.
func loadConfig(dir, home string) (*Config, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var homeInfo fs.FileInfo
	if home != "" {
		homeInfo, _ = os.Stat(home) // a missing home bounds nothing
	}
	for {
		path := filepath.Join(dir, ConfigFile)
		fi, err := os.Stat(path)
		if err == nil {
			if err := checkConfigFile(path, fi); err != nil {
				return nil, err
			}
			b, err := os.ReadFile(path) //nolint:gosec // the repository's own config, checked above
			if err != nil {
				return nil, fmt.Errorf("config: %w", err)
			}
			c, err := ParseConfig(b)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			c.Root = dir
			return c, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("config: %w", err)
		}
		parent := filepath.Dir(dir)
		if parent == dir || isRepoTop(dir) || sameDir(dir, homeInfo) {
			return nil, noConfigError{proto.Errf(proto.CodeInvalid,
				"run starfix inside a repository that has one, or pass -C DIR; the search stops at the repository's top level and at your home directory",
				ConfigFile+" not found here or in any parent directory")}
		}
		dir = parent
	}
}

// isRepoTop reports whether dir holds .git: a directory, or the file a
// worktree or submodule has.
func isRepoTop(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// sameDir reports whether dir is the directory home describes.
func sameDir(dir string, home fs.FileInfo) bool {
	if home == nil {
		return false
	}
	fi, err := os.Stat(dir)
	return err == nil && os.SameFile(fi, home)
}

// checkConfigFile refuses a config that is not a regular file or that
// someone else could have written.
func checkConfigFile(path string, fi fs.FileInfo) error {
	if !fi.Mode().IsRegular() {
		return proto.Errf(proto.CodeInvalid, "replace it with the project's "+ConfigFile+" file",
			path+" is not a regular file")
	}
	if !OwnedByUser(fi) {
		return proto.Errf(proto.CodeInvalid,
			"check where it came from; if it is your project's, copy it into your checkout as your own file, else delete it",
			path+" is owned by another user; starfix reads only your own "+ConfigFile)
	}
	if groupOrWorldWritable(fi) {
		return proto.Errf(proto.CodeInvalid, "run `chmod go-w "+path+"`",
			fmt.Sprintf("%s is writable by other users (mode %04o)", path, fi.Mode().Perm()))
	}
	return nil
}

// ParseConfig reads and validates a config, filling defaults. Unknown keys
// are refused.
func ParseConfig(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if c.Server.Port == 0 {
		c.Server.Port = 22
	}
	if c.Server.User == "" {
		c.Server.User = "starfix"
	}
	if c.Server.IAP != nil && c.Server.IAP.Instance == "" {
		c.Server.IAP.Instance = c.Server.Host
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	switch {
	case !uuidRE.MatchString(c.Project):
		return errors.New("project must be the project's lowercase UUID")
	case !hostRE.MatchString(c.Server.Host):
		return fmt.Errorf("server.host %q is not a host name", c.Server.Host)
	case c.Server.Port < 1 || c.Server.Port > 65535:
		return fmt.Errorf("server.port %d is out of range", c.Server.Port)
	case !userRE.MatchString(c.Server.User):
		return fmt.Errorf("server.user %q is not a Unix account name", c.Server.User)
	case !fprRE.MatchString(c.Server.HostKey):
		return errors.New("server.host_key must be the server's ED25519 fingerprint (SHA256:…, from `ssh-keyscan -t ed25519 HOST | ssh-keygen -lf -`)")
	case notAPath(c.Key) != "":
		return errors.New("key is not a file path")
	}
	if iap := c.Server.IAP; iap != nil {
		switch {
		case !gcpProjectRE.MatchString(iap.Project):
			return fmt.Errorf("server.iap.project %q is not a Google Cloud project id", iap.Project)
		case !gcpZoneRE.MatchString(iap.Zone):
			return fmt.Errorf("server.iap.zone %q is not a Compute Engine zone, such as us-central1-a", iap.Zone)
		case !gcpInstanceRE.MatchString(iap.Instance):
			return fmt.Errorf("server.iap.instance %q is not a Compute Engine instance name (it defaults to server.host)", iap.Instance)
		}
	}
	return nil
}

// ForProject returns the .starfix.yaml of another project on the same
// server: this config's server block with project in place of its own.
// It is what an admin commits to a new project's repository, so it holds
// only what a committed config may: never Key, which is a path on this
// machine, and no value this config left to its default.
func (c *Config) ForProject(project string) ([]byte, error) {
	if !uuidRE.MatchString(project) {
		return nil, errors.New("project must be the project's lowercase UUID")
	}
	type iap struct {
		Project  string `yaml:"project"`
		Zone     string `yaml:"zone"`
		Instance string `yaml:"instance,omitempty"`
	}
	var out struct {
		Project string `yaml:"project"`
		Server  struct {
			Host    string `yaml:"host"`
			Port    int    `yaml:"port,omitempty"`
			User    string `yaml:"user,omitempty"`
			HostKey string `yaml:"host_key"`
			IAP     *iap   `yaml:"iap,omitempty"`
		} `yaml:"server"`
	}
	out.Project, out.Server.Host, out.Server.HostKey = project, c.Server.Host, c.Server.HostKey
	if c.Server.Port != 22 {
		out.Server.Port = c.Server.Port
	}
	if c.Server.User != "starfix" {
		out.Server.User = c.Server.User
	}
	if p := c.Server.IAP; p != nil {
		out.Server.IAP = &iap{Project: p.Project, Zone: p.Zone}
		if p.Instance != c.Server.Host {
			out.Server.IAP.Instance = p.Instance
		}
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(out); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return b.Bytes(), nil
}

// EnvKeyFile is the environment variable that names the key file sfx
// offers, by any path. It wins over Key, and is how one person chooses a
// key without editing the committed file. It holds a path, as ssh's
// IdentityFile does, never a key.
const EnvKeyFile = "STARFIX_KEY_FILE"

// EnvNotRead is a variable sfx never reads. Deployments used the name
// for a secret holding a private key's text, which they piped to
// ssh-add, so its value is never a path, and never printed: a refusal
// may say only that the variable is set and not read.
const EnvNotRead = "STARFIX_KEY"

// maxKeyPath bounds a path that names a key file: PATH_MAX on Linux.
const maxKeyPath = 4096

// notAPath says why s cannot be the path of a key file, or returns "".
// What is checked for is a key put where a path belongs, which a
// refusal must not repeat: more than one line, the armor of a private
// key or the marker inside an OpenSSH one (plain, and as its base64
// begins), and more bytes than a path has.
func notAPath(s string) string {
	switch {
	case strings.ContainsAny(s, "\r\n\x00"):
		return "it spans more than one line"
	case strings.Contains(s, "-----BEGIN"), strings.Contains(s, "openssh-key-v1"), strings.Contains(s, "b3BlbnNzaC1rZXktdjE"):
		return "it holds key material"
	case len(s) > maxKeyPath:
		return "it is longer than a path can be"
	}
	return ""
}

// KeyFile returns the key file sfx is to offer, and no other key with it
// (design §14, decision 35), and what named it, for a refusal:
// EnvKeyFile's value, any path on the machine ("~/" is the home
// directory home gives, and a relative path is relative to the working
// directory), or else [Config.KeyPath]. It returns "" when neither names
// one: the default identities and ssh-agent's keys are then offered.
//
// EnvKeyFile's value is not trusted to be a path: one that is plainly a
// key is refused with CodeInvalid, and never repeated.
func (c *Config) KeyFile(getenv func(string) string, home func() (string, error)) (path, from string, err error) {
	k := getenv(EnvKeyFile)
	if k == "" {
		path, err = c.KeyPath()
		return path, "key: in " + ConfigFile, err
	}
	if why := notAPath(k); why != "" {
		return "", EnvKeyFile, proto.Errf(proto.CodeInvalid,
			fmt.Sprintf("set %s to the path of your key file, such as ~/.ssh/id_ed25519: it takes the file's path, never the key itself", EnvKeyFile),
			fmt.Sprintf("%s does not hold the path of a key file: %s", EnvKeyFile, why))
	}
	if k == "~" || strings.HasPrefix(k, "~/") {
		dir, err := home()
		if err != nil {
			return "", EnvKeyFile, fmt.Errorf("%s: %w", EnvKeyFile, err)
		}
		return filepath.Join(dir, strings.TrimPrefix(k, "~")), EnvKeyFile, nil
	}
	path, err = filepath.Abs(k)
	if err != nil {
		return "", EnvKeyFile, fmt.Errorf("%s: %w", EnvKeyFile, err)
	}
	return path, EnvKeyFile, nil
}

// KeyPath resolves Key, a path relative to the repository root, and
// returns "" when no key is set.
//
// The config file is committed, so in a cloned repository someone else
// chose Key. It may therefore name only a file inside the repository: a
// relative path that, with symbolic links resolved, stays under Root.
// Otherwise a cloned file could make sfx read a key anywhere on the
// machine and show its public half to the server the same file names.
// An absolute path, one under "~", and one that climbs or links out are
// refused with CodeInvalid; a key elsewhere is named with EnvKeyFile. The
// file need not exist: reading it is the caller's.
func (c *Config) KeyPath() (string, error) {
	k := c.Key
	if k == "" {
		return "", nil
	}
	outside := proto.Errf(proto.CodeInvalid,
		fmt.Sprintf("set %s=%s in your environment, and delete the line `key: %s` from %s", EnvKeyFile, k, k, ConfigFile),
		fmt.Sprintf("key: in %s names %s, outside the repository; the file is committed, so it may name only a key inside the repository", ConfigFile, k))
	if filepath.IsAbs(k) || k == "~" || strings.HasPrefix(k, "~/") || filepath.VolumeName(k) != "" {
		return "", outside
	}
	path := filepath.Join(c.Root, k)
	root, err := filepath.EvalSymlinks(cmp.Or(c.Root, "."))
	if err != nil {
		return "", fmt.Errorf("key: %w", err)
	}
	resolved, err := resolveExisting(path)
	if err != nil {
		return "", fmt.Errorf("key: %w", err)
	}
	if rel, err := filepath.Rel(root, resolved); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", outside
	}
	return path, nil
}

// resolveExisting is filepath.EvalSymlinks for a path whose last
// elements may not exist yet: the links of the part that does exist are
// resolved, and the rest is kept as it is.
func resolveExisting(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rest := ""
	for {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			return filepath.Join(resolved, rest), nil
		}
		parent := filepath.Dir(path)
		if !errors.Is(err, fs.ErrNotExist) || parent == path {
			return "", err
		}
		rest, path = filepath.Join(filepath.Base(path), rest), parent
	}
}
