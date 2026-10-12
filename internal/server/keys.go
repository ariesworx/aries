package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// Keys served from the control store (design §15.2). sshd asks, for each
// key anyone offers and before it has authenticated them, which
// principal the key belongs to: it runs `starfixd keys TYPE BASE64`
// (AuthorizedKeysCommand), which is AuthorizedKey below, and that asks
// the daemon on its socket with a keys frame, which lookup answers from
// the control store's memory.

const (
	// lookupTimeout bounds the daemon's answer to one key lookup, and
	// keyLookupTimeout the whole question as `starfixd keys` asks it.
	// sshd waits on the command with a login in progress, so both are
	// short: past them sshd falls back to the authorized_keys file.
	lookupTimeout    = 2 * time.Second
	keyLookupTimeout = 3 * time.Second
	// lookupLogKey is what lookups are sampled under in the log. It has a
	// space, so it is no principal's name.
	lookupLogKey = "key lookup"
)

// DefaultBridge is the path `starfixd keys` names in the forced command
// of the line it prints, as docs/server.md installs it.
const DefaultBridge = "/usr/local/bin/starfixd"

// bridgePattern is what the path of the starfixd binary in a forced
// command may look like: absolute, with nothing that needs quoting in an
// authorized_keys option or in the shell sshd runs the command with.
var bridgePattern = regexp.MustCompile(`^(/[A-Za-z0-9._+-]+)+$`)

// lookup answers one key lookup on c: a keys frame naming the principal
// and the stored key for fingerprint, or an empty one when no active
// principal has that key, the fingerprint is malformed, or this is a
// server of one project. It reads memory only, takes no connection
// slot and opens no store, since anyone who can reach sshd can cause
// it. Past Limits.KeyLookups in flight the connection is closed
// unanswered, which the asker reads as no key.
func (s *Server) lookup(c net.Conn, enc *proto.Encoder, fingerprint string) {
	log, lim := s.cfg.Logger, s.cfg.Limits.KeyLookups
	defer s.lookups.Add(-1)
	if n := s.lookups.Add(1); n > int64(lim) {
		s.refusals.log(log, lookupLogKey, s.now(), slog.LevelDebug, "key lookup refused", "reason", "key_lookups limit", "limit", lim)
		return
	}
	out := &proto.Frame{T: proto.FrameKeys}
	valid := store.ValidFingerprint(fingerprint)
	if s.cfg.Control != nil && valid {
		if p, key, ok := s.cfg.Control.AuthorizedKey(fingerprint); ok {
			out.Principal, out.PublicKey = p, key
		}
	}
	_ = c.SetWriteDeadline(time.Now().Add(lookupTimeout))
	err := enc.Encode(out)
	// Never the key, and a fingerprint only once it is known to be one:
	// what was asked is a stranger's text.
	attrs := []any{"found", out.Principal != ""}
	if valid {
		attrs = append(attrs, "fingerprint", fingerprint)
	}
	if out.Principal != "" {
		attrs = append(attrs, "principal", out.Principal)
	}
	if err != nil {
		attrs = append(attrs, "err", err)
	}
	s.refusals.log(log, lookupLogKey, s.now(), slog.LevelDebug, "key lookup", attrs...)
}

// AuthorizedKey is `starfixd keys TYPE BASE64`, sshd's
// AuthorizedKeysCommand: it asks the daemon at socket which principal
// the public key of type keyType and base64 blob belongs to, and returns
// the one authorized_keys line that lets it in, without a newline:
//
//	restrict,command="BRIDGE stdio --principal NAME --key FINGERPRINT" TYPE BASE64
//
// bridge is the path of the starfixd binary the forced command runs
// (DefaultBridge). For anything else it returns an error and no line: a
// key no active principal has, a server of one project, a daemon that is
// down, too busy or from before key lookups, arguments that are not one
// public key, and an answer it does not trust. The caller prints
// nothing then, and sshd goes on to the authorized_keys file. No error
// repeats the arguments, which are a stranger's.
//
// The line is built from nothing the stranger chose. The fingerprint is
// computed here from the key sshd parsed, never taken from sshd's %f,
// whose form depends on its configuration. The principal is the
// daemon's, and must be a principal name that is not reserved. The key
// printed is the one the store holds, and must be the key that was
// asked about.
func AuthorizedKey(ctx context.Context, socket, bridge, keyType, blob string) (string, error) {
	if !bridgePattern.MatchString(bridge) {
		return "", errors.New("the path of starfixd for the forced command must be absolute, of letters, digits and ._+-")
	}
	// sshd gives the key's own type and base64 (%t and %k), so anything
	// else is refused: the base64 must be this key's and nothing more.
	offered, err := publicKey(keyType + " " + blob)
	if err != nil || offered.Type() != keyType || base64.StdEncoding.EncodeToString(offered.Marshal()) != blob {
		return "", errors.New("the arguments are not a public key's type and base64")
	}
	fingerprint := ssh.FingerprintSHA256(offered)

	ctx, cancel := context.WithTimeout(ctx, keyLookupTimeout)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return "", fmt.Errorf("connect to starfixd at %s: %w", socket, err)
	}
	defer func() { _ = c.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	if err := proto.NewEncoder(c).Encode(&proto.Frame{T: proto.FrameKeys, Key: fingerprint}); err != nil {
		return "", err
	}
	f, err := proto.NewDecoder(c).Decode()
	if err != nil {
		return "", fmt.Errorf("read starfixd's answer: %w", err)
	}
	switch {
	case f.T != proto.FrameKeys:
		return "", fmt.Errorf("starfixd answered with a %q frame", f.T)
	case f.Principal == "" && f.PublicKey == "":
		return "", errors.New("no active principal has this key")
	case !PrincipalPattern.MatchString(f.Principal) || store.Reserved(f.Principal):
		return "", errors.New("starfixd named an invalid or reserved principal")
	}
	stored, err := publicKey(f.PublicKey)
	if err != nil || ssh.FingerprintSHA256(stored) != fingerprint {
		return "", errors.New("starfixd answered with another key")
	}
	return fmt.Sprintf(`restrict,command="%s stdio --principal %s --key %s" %s`,
		bridge, f.Principal, fingerprint, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(stored)))), nil
}

// publicKey parses line as one public key, TYPE BASE64, with no options
// and no certificate. Its error never repeats the line.
func publicKey(line string) (ssh.PublicKey, error) {
	if strings.ContainsAny(line, "\r\n") {
		return nil, errors.New("not one line")
	}
	pub, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil || len(options) > 0 || len(rest) > 0 {
		return nil, errors.New("not a public key")
	}
	if _, cert := pub.(*ssh.Certificate); cert {
		return nil, errors.New("a certificate, not a public key")
	}
	return pub, nil
}

// accessActor is who the daemon's own writes are recorded as when it
// ends the claims of holders it finds without access (Sweep): the
// server, as the reaper's are, in a session of its own.
var accessActor = store.Actor{Principal: store.ReaperActor.Principal, Session: "access", Machine: "server"}

// Why a claim ended with its holder's access (decision 21), as the
// claim.release event and the holder's inbox item give it.
const (
	lostMembership = "membership removed"
	lostDisabled   = "principal disabled"
	lostArchived   = "project archived"
)

// lost says why principal has no access to project now, or "" when it
// has: the project is not active, the principal is disabled, or it is
// neither a member nor a server admin.
func (s *Server) lost(principal, project string) string {
	acc := s.cfg.Control.Access(principal, project)
	switch {
	case !acc.Active:
		return lostArchived
	case acc.Disabled:
		return lostDisabled
	case acc.Role == "":
		return lostMembership
	}
	return ""
}

// Sweep ends, in every open store, the claims held by principals who no
// longer have access to that store's project (decision 21), as the
// reaper loop does on each tick and a store does as it opens. An admin's
// change ends the claims in the stores open when it commits, under the
// admin's name (release); a store closed then has no one to end them,
// so they end here when it next opens, before its first request, under
// the server's name. It is also what ends them when the daemon stopped
// between the change and the release. A server of one project has
// nothing to sweep.
func (s *Server) Sweep(ctx context.Context) {
	if s.stores == nil {
		return
	}
	s.stores.each(func(pr *served) { s.sweep(ctx, pr) })
}

// sweep is Sweep for one project's store.
func (s *Server) sweep(ctx context.Context, pr *served) {
	if s.cfg.Control == nil {
		return
	}
	s.endClaims(ctx, pr, accessActor, func(holder string) string { return s.lost(holder, pr.id) })
}

// release ends, as by, the claims principal holds in project's store
// for reason, and returns how many: in every open store when project is
// empty, and of every holder when principal is. It looks only at stores
// that are open; Sweep ends the rest when their stores open.
//
// The caller has committed the change that ended the access. A store
// that fails here is logged, and swept on the reaper's next tick.
func (s *Server) release(ctx context.Context, by store.Actor, project, principal, reason string) int {
	n := 0
	s.stores.eachOf(project, func(pr *served) {
		n += s.endClaims(ctx, pr, by, func(holder string) string {
			if principal == "" || holder == principal {
				return reason
			}
			return ""
		})
	})
	return n
}

// endClaims ends the claims in pr that why gives a reason for, as by,
// logs each, and returns how many.
func (s *Server) endClaims(ctx context.Context, pr *served, by store.Actor, why func(holder string) string) int {
	ectx, cancel := context.WithTimeout(ctx, pr.timeout)
	defer cancel()
	cs, err := pr.store.EndClaims(ectx, by, why)
	if err != nil {
		if ctx.Err() == nil {
			pr.log.Error("end the claims of holders without access", "err", err)
		}
		return 0
	}
	for _, c := range cs {
		pr.log.Info("claim released: access ended", "issue", c.Issue, "principal", c.Holder.Principal,
			"session", c.Holder.Session, "epoch", c.Epoch, "by", by.Principal, "reason", why(c.Holder.Principal))
	}
	return len(cs)
}
