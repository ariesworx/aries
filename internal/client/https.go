package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/internal/dpop"
	"github.com/ariesworx/aries/internal/keyproof"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/wsconn"
)

// dialHTTPS is Dial over HTTPS (transport.md §5): a WebSocket to the
// server's /v1/connect, verified by the system's roots and server.ca,
// then the proof of one of id's keys (internal/keyproof), then the
// hello and the welcome as over SSH. The frames from the hello on are
// the protocol's, unchanged. ctx bounds it all.
//
// What fails is told apart as transport.md §5.6 has it: a server not
// reached (DNS, a refused or reset connection, a TLS handshake that did
// not finish, a proxy's 502, 503 or 504, no answer in time) is
// unavailable, and so [Unreachable]; a certificate that does not
// verify, a key the server does not know, and the server's own refusal
// are not, and neither is a 4xx a proxy answers the upgrade with.
func dialHTTPS(ctx context.Context, cfg *Config, opts Options, id *identity) (*Conn, error) {
	u := cfg.Server.URL
	far := hostOf(u)
	ws, err := openWS(ctx, cfg, opts, nil)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = ws.Close() })
	defer stop()
	fail := func(err error) (*Conn, error) {
		_ = ws.Close()
		return nil, err
	}
	keys := make([]ssh.PublicKey, len(id.signers))
	for i, s := range id.signers {
		keys[i] = s.PublicKey()
	}
	if err := ws.Write(keyproof.NewOffer(u, keys)); err != nil {
		return fail(gone(ctx, far, "take the key offer", err, opts))
	}
	m, err := ws.Read()
	if err != nil {
		return fail(gone(ctx, far, "answer the key offer", err, opts))
	}
	if w := welcomeIn(m); w != nil {
		if w.Err.Code == proto.CodeAuth && strings.HasPrefix(w.Err.Message, noKeyKnown) {
			return fail(id.noneAccepted(far, false))
		}
		return fail(&refusal{pe: w.Err, principal: w.Principal})
	}
	ch, err := keyproof.ParseChallenge(m)
	if err != nil || ch.Key >= len(id.signers) {
		return fail(proto.Errf(proto.CodeVersion, proto.FixUpgrade+" aries to match the server", fmt.Sprintf("the server answered the key offer with what this aries does not read (%v)", err)))
	}
	sig, err := keyproof.Sign(id.signers[ch.Key], u, ch.Nonce)
	if err != nil {
		return fail(proto.Errf(proto.CodeAuth, fmt.Sprintf("name another key with %s", EnvKeyFile), fmt.Sprintf("the key %s could not sign the server's challenge: %v", id.keys[ch.Key].Fingerprint, err)))
	}
	if err := ws.Write(keyproof.NewProof(sig)); err != nil {
		return fail(gone(ctx, far, "take the key proof", err, opts))
	}
	return greet(ctx, ws, cfg, opts)
}

// dialSignedIn is Dial over HTTPS with a sign-in (transport.md §5.4):
// the token and a DPoP proof by its key go on the upgrade, in place of
// the key proof, and the hello follows at once. The proof names the
// server's URL as .aries.yaml has it, which the server checks against
// its own, never against Host.
func dialSignedIn(ctx context.Context, cfg *Config, opts Options, s *SignIn) (*Conn, error) {
	base, err := serverURL(cfg)
	if err != nil {
		return nil, err
	}
	proof, err := dpop.Proof(s.Key, http.MethodGet, base+keyproof.ConnectPath, s.Token, opts.Now())
	if err != nil {
		return nil, err
	}
	ws, err := openWS(ctx, cfg, opts, http.Header{"Authorization": {"DPoP " + s.Token}, "DPoP": {proof}})
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = ws.Close() })
	defer stop()
	c, err := greet(ctx, ws, cfg, opts)
	if err != nil {
		return nil, err
	}
	shown := *s
	shown.Token, shown.Key = "", nil
	c.signIn = &shown
	return c, nil
}

// openWS opens the WebSocket to cfg's server with header added to the
// upgrade, verified by the system's roots and server.ca.
func openWS(ctx context.Context, cfg *Config, opts Options, header http.Header) (*wsconn.Conn, error) {
	roots, err := cfg.Roots()
	if err != nil {
		return nil, err
	}
	u := cfg.Server.URL
	h := http.Header{"User-Agent": {"aries/" + opts.Version}}
	for k, v := range header {
		h[k] = v
	}
	ws, resp, err := wsconn.Dial(ctx, keyproof.ConnectURL(u), &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, h, 0)
	if err != nil {
		return nil, upgradeFailed(ctx, u, hostOf(u), resp, err, opts)
	}
	return ws, nil
}

// gone words a WebSocket that ended while the server was to do step.
func gone(ctx context.Context, far, step string, err error, opts Options) error {
	if ctx.Err() != nil {
		return httpsGaveUp(ctx, far, step, opts)
	}
	return proto.Errf(proto.CodeUnavailable, "retry; if it persists, ask the server's operator to check ariesd, and any proxy in front of it",
		fmt.Sprintf("the connection to %s ended while it was to %s: %v", far, step, err))
}

// greet sends the hello on ws and reads the welcome, as over SSH. The
// frames from the hello on are the protocol's, unchanged.
func greet(ctx context.Context, ws *wsconn.Conn, cfg *Config, opts Options) (*Conn, error) {
	far := hostOf(cfg.Server.URL)
	fail := func(err error) (*Conn, error) {
		_ = ws.Close()
		return nil, err
	}
	s := wsconn.Stream(ws)
	c := newConn(proto.NewEncoder(s), proto.NewDecoder(s), ws.Close, opts.OnPush)
	c.trace, c.overHTTPS = opts.Trace, true
	if err := c.enc.Encode(&proto.Frame{T: proto.FrameHello, Version: opts.Version, Proto: proto.Proto,
		Project: cfg.Project, Session: opts.Session, Machine: opts.Machine, Harness: opts.Harness}); err != nil {
		// The server may have refused and closed before reading the
		// hello, as the bridge does over SSH: its refusal is still
		// waiting to be read.
		if w, rerr := c.next(); rerr == nil && w.T == proto.FrameWelcome && w.Err != nil {
			return fail(&refusal{pe: w.Err, principal: w.Principal})
		}
		return fail(gone(ctx, far, "take the hello", err, opts))
	}
	w, err := c.next()
	if err != nil {
		return fail(gone(ctx, far, "answer the hello", err, opts))
	}
	switch {
	case w.T != proto.FrameWelcome:
		return fail(proto.Errf(proto.CodeVersion, "upgrade aries to match the server", fmt.Sprintf("expected a welcome from the server, got a %q frame", w.T)))
	case w.Err != nil:
		return fail(&refusal{pe: w.Err, principal: w.Principal})
	}
	if e := proto.CheckProto(proto.Proto, w.Min, w.Max, w.Version); e != nil {
		return fail(e)
	}
	c.Welcome = *w
	c.run()
	return c, nil
}

// signInHint adds to a refusal of every key offered over HTTPS that
// signing in is the other way in, and when the sign-in kept here ended.
func signInHint(err error, via string, ended *SignIn) error {
	pe, ok := errors.AsType[*proto.Error](err)
	if via != TransportHTTPS || !ok || pe.Code != proto.CodeAuth {
		return err
	}
	out := *pe
	if ended != nil {
		out.Message = fmt.Sprintf("your sign-in as %s ended at %s, and %s", ended.Principal, ended.ExpiresAt.Format(time.RFC3339), out.Message)
		out.Fix = proto.FixSignIn + " to sign in again; or " + out.Fix
		return &out
	}
	out.Fix += "; or, where the server signs people in at a provider, run `aries login`"
	return &out
}

// noKeyKnown is the message of the server's refusal of every key a
// proof offered (internal/server), which aries words with the keys it
// offered, as it does sshd's.
const noKeyKnown = "the server knows none of the keys offered"

// welcomeIn is m as a welcome frame that refuses, or nil.
func welcomeIn(m []byte) *proto.Frame {
	var f proto.Frame
	if json.Unmarshal(m, &f) != nil || f.T != proto.FrameWelcome || f.Err == nil {
		return nil
	}
	return &f
}

// hostOf is the host and port of a server's URL, for a message.
func hostOf(serverURL string) string {
	u, err := url.Parse(serverURL)
	if err != nil {
		return serverURL
	}
	if u.Port() == "" {
		return net.JoinHostPort(u.Hostname(), "443")
	}
	return u.Host
}

// httpsGaveUp is the refusal of a dial over HTTPS whose ctx ended while
// it waited for far to do step.
func httpsGaveUp(ctx context.Context, far, step string, opts Options) *proto.Error {
	msg := fmt.Sprintf("timed out after %s waiting for %s to %s", opts.Timeout, far, step)
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		msg = fmt.Sprintf("stopped waiting for %s to %s: %v", far, step, ctx.Err())
	}
	return proto.Errf(proto.CodeUnavailable, "retry; if it persists, ask the server's operator to check ariesd, where `ariesd status` reports on it, and any proxy in front of it", msg)
}

// upgradeFailed words a WebSocket that could not be opened: the
// connection or TLS failed, or the upgrade was answered with something
// else than 101, by the server or by a proxy on the way.
func upgradeFailed(ctx context.Context, serverURL, far string, resp *http.Response, err error, opts Options) error {
	if resp != nil {
		return refusedUpgrade(serverURL, far, resp)
	}
	if tlsErr, ok := errors.AsType[*wsconn.TLSError](err); ok && untrusted(tlsErr) {
		// A configuration error, or someone between: never queued.
		return proto.Errf(proto.CodeAuth,
			"ask the server's operator for the site's CA and put it in server.ca in "+ConfigFile+"; if the server's certificate should verify as it is, do not connect: something may stand between you and the server",
			fmt.Sprintf("the TLS certificate of %s does not verify: %v", far, tlsErr.Err))
	}
	if ctx.Err() != nil {
		return httpsGaveUp(ctx, far, "connect", opts)
	}
	return proto.Errf(proto.CodeUnavailable, "check server.url in "+ConfigFile+", and your network",
		fmt.Sprintf("cannot reach %s: %v", far, err))
}

// untrusted reports whether a TLS failure is the server's certificate
// not verifying, rather than the handshake not finishing.
func untrusted(err *wsconn.TLSError) bool {
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var host x509.HostnameError
	var verify *tls.CertificateVerificationError
	return errors.As(err, &unknown) || errors.As(err, &invalid) || errors.As(err, &host) || errors.As(err, &verify)
}

// refusedUpgrade words an upgrade answered with resp: the server's own
// refusal, a proto.Error in JSON, as it is; a proxy's 502, 503 or 504 as
// the server not reached; and any other status as what it says of the
// way to the server, which no later attempt from here gets past.
func refusedUpgrade(serverURL, far string, resp *http.Response) error {
	var body struct {
		Error *proto.Error `json:"error"`
	}
	b, _ := io.ReadAll(resp.Body) // wsconn kept 16 KiB of it at most
	if json.Unmarshal(b, &body) == nil && body.Error != nil && body.Error.Code != "" {
		return body.Error
	}
	switch resp.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return proto.Errf(proto.CodeUnavailable, "retry in a minute; if it persists, ask the server's operator to check ariesd and the proxy in front of it",
			fmt.Sprintf("%s answered %s: ariesd, or the proxy before it, is not serving", far, resp.Status))
	case http.StatusNotFound:
		return &setupError{pe: proto.Errf(proto.CodeInvalid, "check server.url in "+ConfigFile+": it is the server's URL, with no path but one a proxy serves Aries under",
			fmt.Sprintf("%s answered 404 Not Found: %s has no %s", far, serverURL, keyproof.ConnectPath))}
	}
	return &setupError{pe: proto.Errf(proto.CodeUnavailable,
		"the proxy between you and the server must pass WebSocket upgrades on "+keyproof.ConnectPath+" (docs/server.md, Behind a proxy); ask the server's operator, or use SSH where the server has it (ARIES_TRANSPORT=ssh)",
		fmt.Sprintf("%s answered the WebSocket upgrade with %s", far, resp.Status))}
}
