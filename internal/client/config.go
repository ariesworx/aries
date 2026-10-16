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
	"reflect"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/ariesworx/aries/internal/keyproof"
	"github.com/ariesworx/aries/internal/lazyregexp"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/safetext"
)

// ConfigFile is the committed, per-repository configuration. It holds no
// secrets.
const ConfigFile = ".aries.yaml"

// Config is .aries.yaml.
type Config struct {
	// Project is the project's UUID.
	Project string `yaml:"project"`
	Server  struct {
		// URL is the server's https:// URL: HTTPS, the transport aries
		// uses when it is set (Transport). The fields after CA are SSH's,
		// needed only when the server is reached by SSH.
		URL string `yaml:"url"`
		// CA is a certificate authority, for a server whose certificate
		// the system's roots do not trust (a site's own CA): its
		// certificate in PEM, or a PEM file inside the repository, by a
		// path relative to its root. It is added to the system's roots
		// for this server, never in their place. A CA's certificate is
		// public, so it may be committed.
		CA string `yaml:"ca"`
		// Host is the server's host name or address. Through IAP it is
		// not dialed, and names the instance unless IAP.Instance does.
		Host string `yaml:"host"`
		// Port defaults to 22.
		Port int `yaml:"port"`
		// User is the SSH account ariesd runs as. Default "aries".
		User string `yaml:"user"`
		// HostKey is the server's pinned ED25519 fingerprint, SHA256:…,
		// when one key is pinned. Pinned is what the client checks.
		HostKey string `yaml:"host_key"`
		// HostKeys are the fingerprints pinned when there are several,
		// in place of HostKey: the server is taken for itself when it
		// presents any one of them, so a new host key is listed before
		// the server presents it, and the old one is removed after.
		HostKeys []string `yaml:"host_keys"`
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
	// Checkout is the directory the project's files belong in, such as
	// those aries setup writes: Root, unless the config was found in the
	// main worktree from a linked one, when it is the linked worktree's
	// top level (not read from YAML).
	Checkout string `yaml:"-"`
	// Unknown are the keys of the file this aries does not know, as
	// dotted paths in the file's order, which ParseConfig ignored.
	Unknown []string `yaml:"-"`
}

// warnKeys and warnKeyLen bound Warning's line, which a file of any
// number of keys, of any length, must not stretch past what prime keeps
// of a notice: it names warnKeys keys, each cut to warnKeyLen bytes.
const (
	warnKeys   = 5
	warnKeyLen = 24
)

// Warning is one line naming the keys of the file this aries ignored,
// with what to do, or "" when it knew them all. The keys are the
// repository's text, escaped.
func (c *Config) Warning() string {
	if len(c.Unknown) == 0 {
		return ""
	}
	n := min(len(c.Unknown), warnKeys)
	keys := make([]string, n)
	for i, k := range c.Unknown[:n] {
		k = safetext.Line(k)
		if len(k) > warnKeyLen {
			k = strings.ToValidUTF8(k[:warnKeyLen], "") + "…"
		}
		keys[i] = k
	}
	list := strings.Join(keys, ", ")
	if more := len(c.Unknown) - n; more > 0 {
		list += fmt.Sprintf(" and %d more", more)
	}
	return fmt.Sprintf("%s has keys this aries does not know, ignored: %s; fix: run `aries upgrade` if a newer aries reads them, else remove them",
		ConfigFile, list)
}

// IAP names the Compute Engine instance that Dial reaches through
// Identity-Aware Proxy TCP forwarding, by running `gcloud compute
// start-iap-tunnel`. Each field becomes an argument to gcloud, so each is
// held to Google Cloud's own naming rules.
//
// There is deliberately no general proxy command: .aries.yaml is
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
	gcpProjectRE  = lazyregexp.New(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	gcpZoneRE     = lazyregexp.New(`^[a-z]+-[a-z]+[0-9]+-[a-z]$`)
	gcpInstanceRE = lazyregexp.New(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
)

var (
	uuidRE = lazyregexp.New(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	hostRE = lazyregexp.New(`^[A-Za-z0-9][A-Za-z0-9.:-]{0,252}$`)
	userRE = lazyregexp.New(`^[a-z_][a-z0-9_-]{0,31}$`)
	fprRE  = lazyregexp.New(`^SHA256:[A-Za-z0-9+/]{43}$`)
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

// LoadConfig finds .aries.yaml in dir or the nearest parent that has
// one. The search stops at the repository's top level (the first
// directory holding .git, a directory or a worktree's file) and at the
// home directory, never above them: a config in a shared parent such as
// /tmp, or a drive root on Windows, must not capture a checkout that has
// none of its own. A linked worktree (git worktree add) is its own top
// level; when the search ends at one, it reads the config at the top of
// the repository's main worktree, if there is one (mainWorktree), and
// only there, so a config kept untracked in the main checkout serves
// every worktree. The file found must be a regular file; on Unix it
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
	notFound := noConfigError{proto.Errf(proto.CodeInvalid,
		"run aries inside a repository that has one, or pass -C DIR; the search stops at the repository's top level and at your home directory, and a linked worktree reads the main worktree's",
		ConfigFile+" not found here or in any parent directory")}
	for {
		c, err := readConfigIn(dir)
		if c != nil || err != nil {
			return c, err
		}
		parent := filepath.Dir(dir)
		if sameDir(dir, homeInfo) || parent == dir {
			return nil, notFound
		}
		if isRepoTop(dir) {
			main := mainWorktree(dir)
			if main == "" || sameDir(main, homeInfo) {
				return nil, notFound
			}
			c, err := readConfigIn(main)
			if c == nil && err == nil {
				return nil, notFound
			}
			if c != nil {
				c.Checkout = dir
			}
			return c, err
		}
		dir = parent
	}
}

// readConfigIn reads and checks dir's config, returning nil and no
// error when dir has none.
func readConfigIn(dir string) (*Config, error) {
	path := filepath.Join(dir, ConfigFile)
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
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
	c.Root, c.Checkout = dir, dir
	return c, nil
}

// mainWorktree returns the top level of the main worktree of the
// repository whose linked worktree's top is dir, or "" when dir is not a
// linked worktree's top or the repository has no main worktree. It reads
// git's files rather than running git: dir/.git is a file naming the
// worktree's git directory ("gitdir: PATH"), whose commondir file names
// the repository's common directory, the main worktree's .git. A
// submodule's .git file names a git directory with no commondir, and a
// bare repository's common directory is no worktree's .git (core.bare),
// so neither has one.
func mainWorktree(dir string) string {
	gitdir, ok := readGitPath(filepath.Join(dir, ".git"), "gitdir: ", dir)
	if !ok {
		return ""
	}
	common, ok := readGitPath(filepath.Join(gitdir, "commondir"), "", gitdir)
	if !ok || filepath.Base(common) != ".git" {
		return ""
	}
	main := filepath.Dir(common)
	// The common directory must be main's own .git, a directory.
	cfi, err := os.Stat(common)
	if err != nil || !cfi.IsDir() {
		return ""
	}
	gfi, err := os.Lstat(filepath.Join(main, ".git"))
	if err != nil || !os.SameFile(cfi, gfi) || isBare(common) {
		return ""
	}
	return main
}

// readGitPath reads a one-line file of git's holding a path after
// prefix, resolving a relative path against base.
func readGitPath(path, prefix, base string) (string, bool) {
	b, err := os.ReadFile(path) //nolint:gosec // git's own file in the user's checkout
	if err != nil || len(b) > 4096 {
		return "", false
	}
	line, _, _ := strings.Cut(string(b), "\n")
	p, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), prefix)
	if !ok || p == "" {
		return "", false
	}
	p = filepath.FromSlash(p)
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	return filepath.Clean(p), true
}

// isBare reports whether the git directory's config sets core.bare.
func isBare(gitDir string) bool {
	b, err := os.ReadFile(filepath.Join(gitDir, "config")) //nolint:gosec // git's own file in the user's checkout
	if err != nil {
		return false
	}
	section := ""
	for line := range strings.Lines(string(b)) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			section = strings.ToLower(strings.Trim(line, "[] \t"))
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok && section == "core" && strings.EqualFold(strings.TrimSpace(k), "bare") {
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "true", "yes", "on", "1":
				return true
			}
		}
	}
	return false
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
			path+" is owned by another user; aries reads only your own "+ConfigFile)
	}
	if groupOrWorldWritable(fi) {
		return proto.Errf(proto.CodeInvalid, "run `chmod go-w "+path+"`",
			fmt.Sprintf("%s is writable by other users (mode %04o)", path, fi.Mode().Perm()))
	}
	return nil
}

// ParseConfig reads and validates a config, filling defaults. A key it
// does not know is ignored and listed in Unknown: the file is committed
// and read by every teammate's aries, of whatever version, so a key a
// newer one reads must not stop an older one.
func ParseConfig(b []byte) (*Config, error) {
	var doc yaml.Node
	if err := yaml.NewDecoder(bytes.NewReader(b)).Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	var c Config
	if err := doc.Decode(&c); err != nil {
		if listedHostKey(b) {
			return nil, errors.New("server.host_key holds one fingerprint: list several under host_keys instead")
		}
		return nil, err
	}
	c.Unknown = unknownKeys(&doc, reflect.TypeFor[Config](), "")
	if c.Server.Port == 0 {
		c.Server.Port = 22
	}
	if c.Server.User == "" {
		c.Server.User = "aries"
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
	case notAPath(c.Key) != "":
		return errors.New("key is not a file path")
	case c.Server.URL == "" && !c.sshNamed():
		return errors.New("server.url, the server's https:// URL, is not set, nor server.host and server.host_key for SSH; set one of them")
	}
	if c.Server.URL != "" {
		u, err := keyproof.NormalizeURL(c.Server.URL)
		if err != nil {
			return fmt.Errorf("server.url: %w", err)
		}
		c.Server.URL = u
	}
	if err := c.validateCA(); err != nil {
		return err
	}
	if c.Server.URL != "" && !c.sshNamed() {
		return nil
	}
	return c.validateSSH()
}

// sshNamed reports whether the config names the server for SSH.
func (c *Config) sshNamed() bool {
	s := c.Server
	return s.Host != "" || s.HostKey != "" || len(s.HostKeys) > 0 || s.IAP != nil
}

// validateSSH checks the server's SSH fields.
func (c *Config) validateSSH() error {
	switch {
	case !hostRE.MatchString(c.Server.Host):
		return fmt.Errorf("server.host %q is not a host name", c.Server.Host)
	case c.Server.Port < 1 || c.Server.Port > 65535:
		return fmt.Errorf("server.port %d is out of range", c.Server.Port)
	case !userRE.MatchString(c.Server.User):
		return fmt.Errorf("server.user %q is not a Unix account name", c.Server.User)
	}
	if err := c.validateHostKeys(); err != nil {
		return err
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

// unknownKeys lists the keys of the mapping n that no field of the
// struct t names, as dotted paths after prefix, with those of the
// mappings under the fields that are structs. An unknown key holding a
// mapping is one key.
func unknownKeys(n *yaml.Node, t reflect.Type, prefix string) []string {
	if n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
		n = n.Content[0]
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}
	var out []string
	for i := 0; i+1 < len(n.Content); i += 2 {
		key := n.Content[i].Value
		ft, ok := yamlField(t, key)
		if !ok {
			out = append(out, prefix+key)
			continue
		}
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct {
			out = append(out, unknownKeys(n.Content[i+1], ft, prefix+key+".")...)
		}
	}
	return out
}

// yamlField is the type of the field of the struct t that the YAML key
// name decodes into.
func yamlField(t reflect.Type, name string) (reflect.Type, bool) {
	for f := range t.Fields() {
		tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if tag == name && tag != "-" {
			return f.Type, true
		}
	}
	return nil, false
}

// listedHostKey reports whether b gives host_key a list, which YAML's
// own refusal of it does not explain.
func listedHostKey(b []byte) bool {
	var probe struct {
		Server struct {
			HostKey yaml.Node `yaml:"host_key"`
		} `yaml:"server"`
	}
	return yaml.Unmarshal(b, &probe) == nil && probe.Server.HostKey.Kind == yaml.SequenceNode
}

// fingerprintHow says what a pinned host key is and where it comes
// from, for a refusal.
const fingerprintHow = "the server's ED25519 fingerprint (SHA256:…, from `ssh-keyscan -t ed25519 HOST | ssh-keygen -lf -`)"

// validateHostKeys checks the pin: host_key, or host_keys, never both,
// since a config that sets both says two things and nothing tells which
// it means; each a fingerprint, and none listed twice.
func (c *Config) validateHostKeys() error {
	s := &c.Server
	switch {
	case s.HostKey != "" && len(s.HostKeys) > 0:
		return errors.New("server.host_key and server.host_keys are both set: list every key the server may present under host_keys, and delete host_key")
	case len(s.HostKeys) == 0:
		if !fprRE.MatchString(s.HostKey) {
			return errors.New("server.host_key must be " + fingerprintHow + "; host_keys, a list of them, pins several")
		}
		return nil
	}
	for i, k := range s.HostKeys {
		if !fprRE.MatchString(k) {
			return fmt.Errorf("server.host_keys[%d] must be %s", i, fingerprintHow)
		}
		if slices.Contains(s.HostKeys[:i], k) {
			return fmt.Errorf("server.host_keys lists %s twice: delete one", k)
		}
	}
	return nil
}

// Pinned returns the fingerprints of the server's host keys that the
// config pins, in its order: host_keys, or host_key's one. A dial takes
// the server for itself when it presents any one of them.
func (c *Config) Pinned() []string {
	if len(c.Server.HostKeys) > 0 {
		return c.Server.HostKeys
	}
	return []string{c.Server.HostKey}
}

// andList joins items for a sentence: "a", "a and b", "a, b and c".
func andList(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// ForProject returns the .aries.yaml of another project on the same
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
			URL      string   `yaml:"url,omitempty"`
			CA       string   `yaml:"ca,omitempty"`
			Host     string   `yaml:"host,omitempty"`
			Port     int      `yaml:"port,omitempty"`
			User     string   `yaml:"user,omitempty"`
			HostKey  string   `yaml:"host_key,omitempty"`
			HostKeys []string `yaml:"host_keys,omitempty"`
			IAP      *iap     `yaml:"iap,omitempty"`
		} `yaml:"server"`
	}
	out.Project, out.Server.URL, out.Server.CA = project, c.Server.URL, c.Server.CA
	if c.sshNamed() {
		out.Server.Host = c.Server.Host
		// One key is written as every config before the list had it,
		// which an aries that knows no list reads too.
		if pinned := c.Pinned(); len(pinned) == 1 {
			out.Server.HostKey = pinned[0]
		} else {
			out.Server.HostKeys = pinned
		}
		if c.Server.Port != 22 {
			out.Server.Port = c.Server.Port
		}
		if c.Server.User != "aries" {
			out.Server.User = c.Server.User
		}
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

// EnvKeyFile is the environment variable that names the key file aries
// offers, by any path. It wins over Key, and is how one person chooses a
// key without editing the committed file. It holds a path, as ssh's
// IdentityFile does, never a key.
const EnvKeyFile = "ARIES_KEY_FILE"

// EnvNotRead is a variable aries never reads. Deployments used the name
// for a secret holding a private key's text, which they piped to
// ssh-add, so its value is never a path, and never printed: a refusal
// may say only that the variable is set and not read.
const EnvNotRead = "ARIES_KEY"

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

// KeyFile returns the key file aries is to offer, and no other key with it
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
// Otherwise a cloned file could make aries read a key anywhere on the
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
