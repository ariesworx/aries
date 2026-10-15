package offline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ariesworx/aries/internal/client"
	"github.com/ariesworx/aries/internal/outbox"
	"github.com/ariesworx/aries/internal/proto"
)

// EnvNoQueue is the variable that, set to anything but "", "0" or "false", makes a write that
// does not reach the server fail as it did before there was an outbox:
// `sfx --no-queue` for a script or a CI job that must know.
const EnvNoQueue = "STARFIX_NO_QUEUE"

// NoQueue reports whether the environment turns queueing off.
func NoQueue(getenv func(string) string) bool {
	switch getenv(EnvNoQueue) {
	case "", "0", "false":
		return false
	}
	return true
}

// Client is one process's way to its outboxes: the checkout it runs in,
// its environment, and what it calls itself. The zero value is not
// usable: Dir, GOOS, Getenv and UserHomeDir must be set.
type Client struct {
	// Dir is where the search for .starfix.yaml starts.
	Dir string
	// GOOS, Getenv and UserHomeDir find the data directory ([DataDir])
	// and the keys the process would offer ([client.Offered]).
	GOOS        string
	Getenv      func(string) string
	UserHomeDir func() (string, error)
	// Now is the client's clock; time.Now when nil.
	Now func() time.Time
	// Session and Machine are who makes the writes this client keeps.
	Session, Machine string
	// Outbox bounds the outboxes; the zero value takes the defaults.
	Outbox outbox.Options
}

// Local returns what this machine keeps for the server and project of
// the checkout's config, which it returns too. The config is read on
// every call, as a dial reads it.
func (c *Client) Local() (*Local, *client.Config, error) {
	cfg, err := client.LoadConfig(c.Dir)
	if err != nil {
		return nil, nil, err
	}
	root, err := DataDir(c.GOOS, c.Getenv, c.UserHomeDir)
	if err != nil {
		return nil, nil, err
	}
	return Find(root, cfg.Pinned(), cfg.Project, Options{Now: c.Now, Outbox: c.Outbox, Address: Address(cfg)}), cfg, nil
}

// Whose returns the principal this process's writes belong to while the
// server cannot say ([Local.Principal], by the keys it would offer and
// whether one is configured), or "" when this machine does not know.
func (c *Client) Whose(l *Local, cfg *client.Config) string {
	path, _, err := cfg.KeyFile(c.Getenv, c.UserHomeDir)
	if err != nil {
		return ""
	}
	keys, err := client.Offered(cfg, c.Getenv, c.UserHomeDir)
	if err != nil {
		return ""
	}
	offered := make([]string, len(keys))
	for i, k := range keys {
		offered[i] = k.Fingerprint
	}
	who, _ := l.Principal(offered, path != "")
	return who
}

// Connected notes, after a handshake, who the server took the
// connection's key for. Failing to note it is not the caller's failure:
// it costs a later write its place in the outbox, which that write's
// refusal then says.
//
// It returns one line, with its fix, when the server took the key for
// someone else than it did before and that principal's outbox holds
// entries: this connection does not send them (doc.go, "Whose"), and a
// person should hear of it. Otherwise it returns "".
func (c *Client) Connected(conn Conn) (moved string) {
	l, _, err := c.Local()
	if err != nil {
		return ""
	}
	l.Touch()
	k, ok := conn.(interface{ Key() client.KeyInfo })
	if !ok || conn.Principal() == "" {
		return ""
	}
	was, _ := l.Reached(conn.Principal(), k.Key().Fingerprint, k.Key().Configured(), speaks(conn))
	if was == "" {
		return ""
	}
	n, err := l.Waiting(was)
	if err != nil || n.Pending+n.Refused == 0 {
		return ""
	}
	return fmt.Sprintf("outbox: the server now takes this key for %s, not %s, so %s queued as %s %s not sent with it; "+
		"fix: sign in with a key of %s's to send them, or run `sfx outbox --principal %s` to see or drop them",
		conn.Principal(), was, writes(n.Pending+n.Refused), was, was2(n.Pending+n.Refused), was, was)
}

// Missed notes that the server was not reached just now, for `sfx
// outbox` to show.
func (c *Client) Missed() {
	if l, _, err := c.Local(); err == nil {
		l.Missed()
	}
}

// Ended notes err, a dial's refusal, when it is the server's word that a
// principal's access to this project has ended ([client.AccessEnded]):
// what that principal queued for it is refused with the server's
// refusal ([Local.Ended]). It returns one line saying so, or "" when
// err is no such word or nothing waited. A failure to note it leaves
// the entries waiting, as they were.
func (c *Client) Ended(err error) string {
	who, ok := client.AccessEnded(err)
	if !ok {
		return ""
	}
	pe, _ := errors.AsType[*proto.Error](err) // AccessEnded says it is one
	l, _, lerr := c.Local()
	if lerr != nil {
		return ""
	}
	n, _ := l.Ended(who, pe)
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("outbox: %s queued as %s %s refused with it, and kept: `sfx outbox` lists %s, to retry once access is back or drop",
		writes(n), who, was(n), them(n))
}

// CacheOf is the cache of principal, the principal of a connection the
// server let in, for what it reads to be kept; nil when this machine has
// nowhere to keep it.
func (c *Client) CacheOf(principal string) *Cache {
	l, _, err := c.Local()
	if err != nil || principal == "" {
		return nil
	}
	return l.Cache(principal)
}

// Cached is the cache of the principal this process's requests belong to
// while the server cannot say ([Client.Whose]), by the same rule as an
// outbox's, so that one principal's copy is never shown to another; nil
// when this machine does not know whose they are.
func (c *Client) Cached() *Cache {
	l, cfg, err := c.Local()
	if err != nil {
		return nil
	}
	who := c.Whose(l, cfg)
	if who == "" {
		return nil
	}
	return l.Cache(who)
}

// Forget removes, when err is the server's word that a principal's access
// to this project has ended ([client.AccessEnded]), that principal's
// cache of the project from this machine (design §18.6). It returns one
// line saying so, or "" when err is no such word or nothing was cached.
func (c *Client) Forget(err error) string {
	who, ok := client.AccessEnded(err)
	if !ok {
		return ""
	}
	l, _, lerr := c.Local()
	if lerr != nil {
		return ""
	}
	n, _ := l.Cache(who).Forget()
	if n == 0 {
		return ""
	}
	answers := "answers"
	if n == 1 {
		answers = "answer"
	}
	return fmt.Sprintf("cache: %d %s %s read from this project %s deleted from this machine", n, answers, who, was(n))
}

// LastReached is when this machine last reached the server, or the zero
// time when it has no record of it.
func (c *Client) LastReached() time.Time {
	l, _, err := c.Local()
	if err != nil {
		return time.Time{}
	}
	return l.LastReached()
}

func them(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// Keep queues the write op with args, which did not reach the server;
// reach is the error that says so. principal is who the connection it
// was lost on belonged to, or "" when there was none, and the keys this
// process would offer decide ([Client.Whose]). A refusal is as
// [Local.Keep]'s.
func (c *Client) Keep(principal, op string, args any, reach error) (Kept, error) {
	b, err := json.Marshal(args)
	if err != nil {
		return Kept{}, fmt.Errorf("queue %s: %w", op, err)
	}
	l, cfg, err := c.Local()
	if err != nil {
		pe, ok := errors.AsType[*proto.Error](err)
		if !ok {
			pe = proto.Errf(proto.CodeUnavailable, rerun, err.Error())
		}
		was := reach.Error()
		if rp, ok := errors.AsType[*proto.Error](reach); ok {
			was = rp.Message
		}
		return Kept{}, &notQueued{pe: proto.Errf(pe.Code, pe.Fix, fmt.Sprintf("%s, and this %s was not queued: %s", was, op, pe.Message))}
	}
	if principal == "" {
		principal = c.Whose(l, cfg)
	}
	return l.Keep(principal, outbox.Entry{Op: op, Args: b, Session: c.Session, Machine: c.Machine}, reach)
}

// Replay sends what waits for conn's principal on conn ([Local.Replay]).
func (c *Client) Replay(ctx context.Context, conn Conn) (outbox.Report, error) {
	l, _, err := c.Local()
	if err != nil {
		return outbox.Report{}, err
	}
	return l.Replay(ctx, conn)
}

// Notice is the one line a session's start says of the outbox of conn's
// principal, or "" when nothing waits in it: how many writes wait for
// the server, and how many it refused, which only a person settles.
func (c *Client) Notice(conn Conn) string {
	return c.NoticeFor(conn.Principal())
}

// NoticeFor is [Client.Notice] for principal's outbox, when there is no
// connection to say whose it is.
func (c *Client) NoticeFor(principal string) string {
	l, _, err := c.Local()
	if err != nil {
		return ""
	}
	n, err := l.Waiting(principal)
	if err != nil {
		return ""
	}
	return notice(n)
}

// Rekeyed is the line a session's start says of what this machine keeps
// for the project under host keys the server no longer has
// ([Local.Rekeyed]), whoever queued it, or "" when nothing is kept so.
// None of it is sent by itself, so only a person settles it.
func (c *Client) Rekeyed() string {
	l, _, err := c.Local()
	if err != nil {
		return ""
	}
	formers, err := l.Rekeyed()
	if err != nil {
		return ""
	}
	total := 0
	for _, f := range formers {
		for _, p := range f.Local.Principals() {
			if n, err := f.Local.Waiting(p); err == nil {
				total += n.Pending + n.Refused
			}
		}
	}
	if total == 0 {
		return ""
	}
	itself := "are not sent by themselves"
	if total == 1 {
		itself = "is not sent by itself"
	}
	return fmt.Sprintf("outbox: %s queued under the server's old host key %s: tell the user to run `sfx outbox`", writes(total), itself)
}

func notice(n Count) string {
	const made = " made while the server was away "
	switch {
	case n.Refused > 0 && n.Pending > 0:
		return fmt.Sprintf("outbox: %s%s%s refused and %d wait to be sent: tell the user to run `sfx outbox`",
			writes(n.Refused), made, was(n.Refused), n.Pending)
	case n.Refused > 0:
		return fmt.Sprintf("outbox: %s%s%s refused: tell the user to run `sfx outbox`", writes(n.Refused), made, was(n.Refused))
	case n.Pending == 1:
		return "outbox: 1 write" + made + "waits to be sent, which sfx does by itself"
	case n.Pending > 0:
		return fmt.Sprintf("outbox: %d writes%swait to be sent, which sfx does by itself", n.Pending, made)
	}
	return ""
}

func writes(n int) string {
	if n == 1 {
		return "1 write"
	}
	return fmt.Sprintf("%d writes", n)
}

// was2 is "is" or "are" for n things.
func was2(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

func was(n int) string {
	if n == 1 {
		return "was"
	}
	return "were"
}
