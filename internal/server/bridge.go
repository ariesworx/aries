package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

// Bridge is `starfixd stdio`, the forced command of every developer key. It
// connects to the daemon's socket, sends a bridge frame naming principal
// (fixed by sshd from the key that authenticated), then copies in to the
// daemon and the daemon to out until the daemon closes the connection.
//
// key is the fingerprint of the key that authenticated, when the line
// that let it in came from the control store (the daemon writes it into
// the forced command of each line of its keys file), and empty for a
// line of the hand-maintained authorized_keys file. The daemon refuses
// the handshake unless the store holds that key for principal, and
// closes the connection when the key is removed.
//
// When the daemon cannot be reached, Bridge writes a refusing welcome to
// out, so the client reports a typed error rather than a dropped session.
// An invalid or reserved principal, and a key that is no fingerprint,
// are refused the same way, before connecting.
func Bridge(ctx context.Context, socket, principal, key string, in io.Reader, out io.Writer) error {
	if !PrincipalPattern.MatchString(principal) || store.Reserved(principal) {
		e := proto.Errf(proto.CodeAuth, "the server admin should fix this key's authorized_keys line",
			"this key's forced command names an invalid or reserved principal")
		return errors.Join(e, refuse(out, e))
	}
	if key != "" && !store.ValidFingerprint(key) {
		e := proto.Errf(proto.CodeAuth, "the server admin should fix this key's authorized_keys line: --key takes the key's SHA256: fingerprint, or is left out",
			"this key's forced command names a key that is not a fingerprint")
		return errors.Join(e, refuse(out, e))
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		e := proto.Errf(proto.CodeUnavailable, "ask the server admin to start it (`systemctl start starfixd`)",
			"starfixd is not running on the server")
		return errors.Join(fmt.Errorf("connect to %s: %w", socket, err), refuse(out, e))
	}
	defer func() { _ = c.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	if err := proto.NewEncoder(c).Encode(&proto.Frame{T: proto.FrameBridge, Principal: principal, Key: key}); err != nil {
		return err
	}
	// Not waited for: Bridge returns when the daemon closes the
	// connection, and the stdio process exits with it, ending a read
	// still blocked on in.
	go func() {
		_, _ = io.Copy(c, in)
		if uc, ok := c.(*net.UnixConn); ok {
			_ = uc.CloseWrite()
		}
	}()
	if _, err := io.Copy(out, c); err != nil && ctx.Err() == nil {
		return fmt.Errorf("bridge: %w", err)
	}
	return nil
}

// refuse writes a welcome carrying e, which the client reads in place of
// the daemon's.
func refuse(out io.Writer, e *proto.Error) error {
	return proto.NewEncoder(out).Encode(&proto.Frame{T: proto.FrameWelcome, Err: e})
}
