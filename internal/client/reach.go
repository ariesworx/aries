package client

import (
	"errors"

	"github.com/ariesworx/starfix/internal/proto"
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
//     failed or timed out, the SSH handshake did, sshd would not open a
//     session or run the bridge, or the bridge closed before its welcome;
//   - the bridge or the daemon answered the handshake unavailable: the
//     daemon is not running, or its database does not answer;
//   - the connection was lost while a call waited for its answer, in
//     which case the server may have applied the request;
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
