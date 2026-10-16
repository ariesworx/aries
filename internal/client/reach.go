package client

import (
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/ariesworx/aries/internal/proto"
)

// Unreachable reports whether err, from [Dial] or [Conn.Call], says the
// server was not reached, or was reached and could not serve: the one
// case in which a write is kept on this machine and sent later
// (internal/outbox, design §18.3).
//
// It is true for a refusal coded [proto.CodeUnavailable], which is what
// every one of these is:
//
//   - Dial could not get through: the TCP connection or the IAP tunnel
//     failed or timed out, the SSH handshake did (the server's closing
//     the connection before it among them, [ClosedBeforeHandshake]),
//     sshd would not open a session or run the bridge, or the bridge
//     closed before its welcome;
//   - the bridge or the daemon answered the handshake unavailable: the
//     daemon is not running, or its database does not answer;
//   - the connection was lost while a call waited for its answer, or the
//     server did not answer within [RequestTimeout], in which case the
//     server may have applied the request;
//   - the server answered a request unavailable: it failed or timed out.
//
// It is false for everything else, which fails as it always has: a
// refusal of this client or this person (auth, which a host key that
// does not match its pin and a key the server does not know both are;
// version; forbidden), of the request (invalid, not_found, conflict and
// the rest), the server's rate limits (busy, whose fix is to wait and
// retry), an error that is no refusal (a canceled call, a result that
// does not decode), and a program this machine lacks to reach the
// server at all, which no later attempt from it would get past.
func Unreachable(err error) bool {
	if _, setup := errors.AsType[*setupError](err); setup {
		return false
	}
	pe, ok := errors.AsType[*proto.Error](err)
	return ok && pe.Code == proto.CodeUnavailable
}

// setupError is a refusal that says this machine lacks something it
// needs to reach the server by the transport its config names: gcloud,
// for a server behind IAP. It is a *proto.Error through Unwrap, and
// never [Unreachable].
type setupError struct{ pe *proto.Error }

func (e *setupError) Error() string { return e.pe.Error() }

func (e *setupError) Unwrap() error { return e.pe }

// ClosedBeforeHandshake reports whether err, from [Dial], says the server
// accepted the TCP connection and then closed or reset it before the SSH
// handshake was done, without a word: before its greeting, or in the key
// exchange, before it had shown its host key. sshd does that to an
// address it holds back for a while after failed or dropped connections
// from it (PerSourcePenalties, OpenSSH 9.8 and later), and past
// MaxStartups; a firewall or a proxy on the way can do it too.
//
// Such an error is also [Unreachable]: nothing was sent, and what holds
// the client back passes, so a write is queued. Every attempt meanwhile
// is refused, and may lengthen sshd's penalty, so a caller that dials by
// itself waits before it dials again.
func ClosedBeforeHandshake(err error) bool {
	_, ok := errors.AsType[*hungUpError](err)
	return ok
}

// hungUpError is the refusal ClosedBeforeHandshake reports. It is a
// *proto.Error through Unwrap, coded unavailable.
type hungUpError struct{ pe *proto.Error }

func (e *hungUpError) Error() string { return e.pe.Error() }

func (e *hungUpError) Unwrap() error { return e.pe }

// hungUp is the refusal of a connection to addr that the server ended
// with err before the SSH handshake was done.
func hungUp(addr string, err error) *hungUpError {
	return &hungUpError{pe: proto.Errf(proto.CodeUnavailable,
		"wait a minute, then run it once more, not in a loop: each attempt can lengthen sshd's penalty; "+
			"check that `aries whoami` signs in with the key the server knows ("+EnvKeyFile+" names it); "+
			"if it persists, ask the server's operator to look for \"penalty\" in sshd's log",
		fmt.Sprintf("%s closed the connection before the ssh handshake (%v): sshd does this for a while to an address with failed or dropped connections "+
			"(PerSourcePenalties, OpenSSH 9.8 and later), or past MaxStartups; so can a firewall or proxy",
			addr, err))}
}

// isHangUp reports whether err is the far end closing or resetting the
// connection.
func isHangUp(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return slices.ContainsFunc(resets, func(r error) bool { return errors.Is(err, r) })
}

// AccessEnded reports whether err, from [Dial], is the server's word
// that principal's access to the project has ended: a refusal of the
// handshake coded [proto.CodeForbidden] that names whom it refuses. A
// principal disabled, one the server no longer knows, and one who is no
// longer a member of the project are each refused so, and stay refused
// until a person changes that; what they queued for the project can then
// only be sent by a person's decision (design §18.3).
//
// Nothing else is proof. A forbidden answer to a request refuses that
// request. A refusal that names nobody, as one from a server older than
// the field does, does not say whose access ended. A project that is not
// served (archived, being made, or never there) is not_found, which a
// project made active again mends; a key the server no longer holds is
// auth, and the principal may have others; and unavailable and busy are
// the server's own state, not anyone's access.
func AccessEnded(err error) (principal string, ok bool) {
	r, ok := errors.AsType[*refusal](err)
	if !ok || r.pe.Code != proto.CodeForbidden || r.principal == "" {
		return "", false
	}
	return r.principal, true
}

// refusal is the server's refusal of a handshake: a *proto.Error through
// Unwrap, with the principal the refusal names, if any.
type refusal struct {
	pe        *proto.Error
	principal string
}

func (e *refusal) Error() string { return e.pe.Error() }

func (e *refusal) Unwrap() error { return e.pe }

// Offered returns the keys [Dial] would offer the server named in cfg,
// in order, without connecting: the one configured key, or the default
// identities and then ssh-agent's (design §14, decision 35). It fails as
// Dial does when there is no key to offer.
func Offered(cfg *Config, getenv func(string) string, home func() (string, error)) ([]KeyInfo, error) {
	id, err := loadIdentity(cfg, getenv, home)
	if err != nil {
		return nil, err
	}
	id.close()
	return id.keys, nil
}
