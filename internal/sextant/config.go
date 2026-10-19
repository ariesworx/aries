package sextant

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/client"
	"github.com/ariesworx/aries/internal/lazyregexp"
	"github.com/ariesworx/aries/internal/telemetry"
	"go.yaml.in/yaml/v3"
)

// Defaults and bounds of the config (docs/sextant.md has the table).
const (
	DefaultInterval       = 60 * time.Second
	MinInterval           = 10 * time.Second
	DefaultWindow         = 24 * time.Hour
	MinWindow             = time.Minute
	MaxWindow             = 31 * 24 * time.Hour
	DefaultListen         = "127.0.0.1:9466"
	DefaultProjects       = telemetry.DefaultSextantProjects
	MaxProjects           = 500
	DefaultModels         = telemetry.DefaultSextantModels
	MaxModels             = 500
	DefaultRequestTimeout = 30 * time.Second
	MinRequestTimeout     = time.Second
	MaxRequestTimeout     = 2 * time.Minute
)

// Config is sextant.yaml.
type Config struct {
	// Server names the server as .aries.yaml's server: does: url (and
	// ca) for HTTPS, or host, port, user and host_key(s) for SSH.
	Server yaml.Node `yaml:"server"`
	// Key is the key file Sextant signs in with, absolute or relative to
	// the config's directory; the only key it offers.
	Key string `yaml:"key"`
	// Workload signs Sextant in as a service principal with its
	// platform's token, in place of a key.
	Workload *Workload `yaml:"workload"`
	// DataDir keeps a workload's sign-in.
	DataDir string `yaml:"data_dir"`
	// Projects are the ids of the projects Sextant reads.
	Projects []string `yaml:"projects"`
	// Interval is how often every project is read.
	Interval time.Duration `yaml:"interval"`
	// Window is how far back the cost of a project is summed.
	Window time.Duration `yaml:"window"`
	// Listen is the loopback address of /metrics, /healthz and /readyz;
	// "off" serves none.
	Listen string `yaml:"listen"`
	Limits Limits `yaml:"limits"`

	// dir is the config's directory; clients are the projects' configs.
	dir     string
	clients map[string]*client.Config
}

// Workload is a service principal's sign-in (docs/ci.md, transport.md
// §7): its platform and the server's provider for it.
type Workload struct {
	Kind     string `yaml:"kind"`
	Provider string `yaml:"provider"`
}

// Limits bound what Sextant asks of the server and exports.
type Limits struct {
	// Projects caps the projects listed, and the project label.
	Projects int `yaml:"projects"`
	// Models caps the model label: the models with the largest cost are
	// named, the rest summed into (other).
	Models int `yaml:"models"`
	// RequestTimeout bounds a connection's start and each request.
	RequestTimeout time.Duration `yaml:"request_timeout"`
}

// ListenOff is the Listen that serves nothing.
const ListenOff = "off"

var uuidRE = lazyregexp.New(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Load reads the config file at path. A file others may write is
// refused: it names the key Sextant offers and the server it trusts.
func Load(path string) (*Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("sextant: %w", err)
	}
	b, err := os.ReadFile(abs) //nolint:gosec // the operator's config, named on the command line
	if err != nil {
		return nil, fmt.Errorf("sextant: read the config: %w; fix: name sextant.yaml with -config (docs/sextant.md)", err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("sextant: %w", err)
		}
		if fi.Mode().Perm()&0o022 != 0 {
			return nil, fmt.Errorf("sextant: %s is mode %04o, which others can write; fix: chmod go-w %s", abs, fi.Mode().Perm(), abs)
		}
	}
	return Parse(b, filepath.Dir(abs))
}

// Parse reads a config whose relative paths start at dir, fills in the
// defaults and checks every value. Each refusal names a fix.
func Parse(b []byte, dir string) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, refuse("%v", "correct sextant.yaml (docs/sextant.md lists its keys)", err)
	}
	c.dir = dir
	c.defaults()
	if err := c.check(); err != nil {
		return nil, err
	}
	return &c, nil
}

// refuse is a refusal of the config with its fix.
func refuse(format, fix string, args ...any) error {
	return fmt.Errorf("sextant: "+format+"; fix: "+fix, args...)
}

func (c *Config) defaults() {
	if c.Interval == 0 {
		c.Interval = DefaultInterval
	}
	if c.Window == 0 {
		c.Window = DefaultWindow
	}
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.Limits.Projects == 0 {
		c.Limits.Projects = DefaultProjects
	}
	if c.Limits.Models == 0 {
		c.Limits.Models = DefaultModels
	}
	if c.Limits.RequestTimeout == 0 {
		c.Limits.RequestTimeout = DefaultRequestTimeout
	}
}

func (c *Config) check() error {
	l := c.Limits
	switch {
	case c.Server.Kind == 0:
		return refuse("server: is not set", "name the server under server: as .aries.yaml does: url: for HTTPS, or host: and host_key: for SSH")
	case len(c.Projects) == 0:
		return refuse("projects: lists no project", "list the ids of the projects to read under projects:, as each one's .aries.yaml names it")
	case l.Projects < 1 || l.Projects > MaxProjects:
		return refuse("limits.projects: %d is not from 1 to %d", "set limits.projects within that", l.Projects, MaxProjects)
	case len(c.Projects) > l.Projects:
		return refuse("projects: lists %d projects, more than limits.projects (%d)", "list fewer, or raise limits.projects", len(c.Projects), l.Projects)
	case l.Models < 1 || l.Models > MaxModels:
		return refuse("limits.models: %d is not from 1 to %d", "set limits.models within that", l.Models, MaxModels)
	case l.RequestTimeout < MinRequestTimeout || l.RequestTimeout > MaxRequestTimeout:
		return refuse("limits.request_timeout: %s is not from %s to %s", "set limits.request_timeout within that", l.RequestTimeout, MinRequestTimeout, MaxRequestTimeout)
	case c.Interval < MinInterval:
		return refuse("interval: %s is shorter than %s", "set interval: to %s or longer; the default is %s", c.Interval, MinInterval, MinInterval, DefaultInterval)
	case c.Window < MinWindow:
		return refuse("window: %s is shorter than %s", "set window: to %s or longer; the default is %s", c.Window, MinWindow, MinWindow, DefaultWindow)
	case c.Window > MaxWindow:
		return refuse("window: %s is longer than %s", "set window: to %s or shorter", c.Window, MaxWindow, MaxWindow)
	}
	if c.Listen != ListenOff {
		if err := telemetry.CheckAddr(c.Listen); err != nil {
			return refuse("listen: %v", "set listen: to a loopback address and a port, such as %s, or to off", err, DefaultListen)
		}
	}
	for i, p := range c.Projects {
		if !uuidRE.MatchString(p) {
			return refuse("projects: %q is not a project's lowercase UUID", "copy the id from the project's .aries.yaml (project:)", p)
		}
		if slices.Contains(c.Projects[:i], p) {
			return refuse("projects: %s is listed twice", "list each project once", p)
		}
	}
	if err := c.checkSignIn(); err != nil {
		return err
	}
	c.clients = make(map[string]*client.Config, len(c.Projects))
	for _, p := range c.Projects {
		cc, err := c.client(p)
		if err != nil {
			return err
		}
		c.clients[p] = cc
	}
	if c.Workload != nil && c.clients[c.Projects[0]].Server.URL == "" {
		return refuse("workload: signs in over HTTPS, and server: names no url:", "set server.url, or sign in with key: over SSH")
	}
	return nil
}

func (c *Config) checkSignIn() error {
	switch {
	case c.Key == "" && c.Workload == nil:
		return refuse("neither key: nor workload: is set", "set key: to Sextant's key file, or workload: to sign in as a service principal (docs/sextant.md, Signing in)")
	case c.Key != "" && c.Workload != nil:
		return refuse("both key: and workload: are set", "keep one: key: signs in by key, workload: as a service principal")
	case c.Workload != nil && c.DataDir == "":
		return refuse("workload: is set and data_dir: is not set", "set data_dir: to a directory of Sextant's own, mode 0700, where its sign-in is kept")
	case c.Workload != nil && !slices.Contains(client.WorkloadKinds, c.Workload.Kind):
		return refuse("workload.kind %q is not one of %s", "set workload.kind to the platform Sextant runs on", c.Workload.Kind, strings.Join(client.WorkloadKinds, ", "))
	}
	return nil
}

// client makes project's .aries.yaml from server:, which client.Config
// checks as it checks any.
func (c *Config) client(project string) (*client.Config, error) {
	b, err := yaml.Marshal(map[string]any{"project": project, "server": &c.Server})
	if err != nil {
		return nil, refuse("server: %v", "correct server: in sextant.yaml", err)
	}
	cc, err := client.ParseConfig(b)
	if err != nil {
		return nil, refuse("%v", "correct server: in sextant.yaml, as .aries.yaml's server: would be (docs/aries.md)", err)
	}
	if len(cc.Unknown) > 0 {
		return nil, refuse("%s is not a key sextant knows", "remove it from sextant.yaml", cc.Unknown[0])
	}
	// A CA's file is read from the config's directory, as .aries.yaml's
	// is from its repository's.
	cc.Root, cc.Checkout = c.dir, c.dir
	return cc, nil
}

// Client is project's connection config.
func (c *Config) Client(project string) (*client.Config, error) {
	cc, ok := c.clients[project]
	if !ok {
		return nil, fmt.Errorf("sextant: project %s is not in the config", project)
	}
	return cc, nil
}

// KeyFile is the absolute path of the key Sextant offers, or "".
func (c *Config) KeyFile() string {
	if c.Key == "" || filepath.IsAbs(c.Key) {
		return c.Key
	}
	return filepath.Join(c.dir, c.Key)
}
