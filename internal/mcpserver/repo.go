package mcpserver

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/ariesworx/aries/internal/client"
	"github.com/ariesworx/aries/internal/proto"
)

// RepoConn is a *client.Conn that knows its repository.
type RepoConn struct {
	*client.Conn
	name string
	// config is the warning of the repository's config, or "".
	config string
}

// Project is the repository directory's name: more use to an agent than
// the project UUID.
func (c *RepoConn) Project() string { return c.name }

// ConfigWarning is one line naming the keys of .aries.yaml this aries
// ignored, with its fix, or "" ([client.Config.Warning]).
func (c *RepoConn) ConfigWarning() string { return c.config }

// Notices are the connection's version notices and then the config's
// warning, for prime.
func (c *RepoConn) Notices(clientVersion string) []string {
	n := c.Conn.Notices(clientVersion)
	if c.config != "" {
		n = append(n, c.config)
	}
	return n
}

// DialRepo connects to the server named by the .aries.yaml at or above
// dir. The config is read on every dial, so a fix to it takes effect on
// the next tool call without restarting the agent.
func DialRepo(ctx context.Context, dir string, opts client.Options) (*RepoConn, error) {
	cfg, err := client.LoadConfig(dir)
	if err != nil {
		if _, ok := errors.AsType[*proto.Error](err); ok {
			return nil, err
		}
		return nil, proto.Errf(proto.CodeInvalid, "correct "+client.ConfigFile+"; see docs/cli.md, Connect a repository", err.Error())
	}
	c, err := client.Dial(ctx, cfg, opts)
	if err != nil {
		return nil, err
	}
	return &RepoConn{Conn: c, name: filepath.Base(cfg.Root), config: cfg.Warning()}, nil
}
