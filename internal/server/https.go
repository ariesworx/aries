package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/internal/keyproof"
	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
	"github.com/ariesworx/aries/internal/telemetry"
	"github.com/ariesworx/aries/internal/wsconn"
)

// HTTPS configures the HTTPS listener (transport.md §4): the transport a
// fresh server serves. Its paths are two, and nothing else is served
// (everything else is 404):
//
//   - GET /.well-known/aries, what a client may know before it signs in:
//     the server's version, the transports on, and how one signs in. No
//     principal and no secret.
//   - GET /v1/connect, a WebSocket (internal/wsconn) that carries the
//     protocol's frames. Before its hello, the connection proves it holds
//     a key the server knows (internal/keyproof); the server then hands
//     it, in this process, to the handler the socket's connections have,
//     with the principal and the key in place of a bridge frame. From the
//     hello on it is served as any connection is: the session, its
//     watchers, the limits per principal and the close when access ends
//     are the same code.
//
// Nothing the request says of where it came from is trusted: not Host,
// not X-Forwarded-*. A key proof names the server's URL as the client
// has it, which must be one of URLs.
type HTTPS struct {
	// Listener accepts the listener's TCP connections. Serve closes it
	// when it returns.
	Listener net.Listener
	// TLS is the listener's TLS: the certificate, from files or ACME,
	// and TLS 1.2 or 1.3 at least. HTTP/2 is offered beside HTTP/1.1, for
	// everything but the WebSocket, which is HTTP/1.1's. Nil serves plain
	// HTTP behind a proxy that terminates TLS, which Trusted must name.
	TLS *tls.Config
	// Trusted are the addresses that may connect when TLS is nil: the
	// proxy's. A connection from any other is closed unanswered. The
	// limits per address then do not apply, since every connection is
	// the proxy's.
	Trusted []netip.Prefix
	// URLs are the server's public URLs, https://HOST[:PORT][/PATH]:
	// what clients have in server.url, and what a key proof must name.
	// At least one. A proxy that serves Aries under a path strips it.
	URLs []string
	// Mode names the TLS mode for the status report and the log: acme,
	// files or proxy. Empty says files with TLS, and proxy without.
	Mode string
	// NotAfter, when set, says when the certificate served ends, for the
	// status report; the zero time when that is not known.
	NotAfter func() time.Time
	// Ping is how often the server pings a WebSocket, so that the proxies
	// on its way keep a connection with nothing to say: 25 seconds by
	// default, under the 60 seconds proxies commonly allow. idle_timeout
	// counts frames, not pings.
	Ping time.Duration
	// Keys finds whose a public key is: the principal, and the key's
	// fingerprint when the control store holds the key, which the
	// connection is then checked against as one from the keys file is.
	// Nil looks where sshd would ([Server.keyOwner]); a test's world sets
	// its own, as its SSH server has.
	Keys func(ssh.PublicKey) (principal, fingerprint string, ok bool)
}

// DefaultPing is how often an HTTPS connection is pinged by default.
const DefaultPing = 25 * time.Second

// maxProofMessage bounds a message of the key proof: an offer of
// keyproof.MaxKeys keys of RSA 16384 at most fits.
const maxProofMessage = 64 << 10

// shutdownWait bounds how long the plain requests in flight have when
// the listener stops.
const shutdownWait = 5 * time.Second

// check refuses a listener New could not serve safely, and normalizes
// URLs.
func (h *HTTPS) check() error {
	switch {
	case h.Listener == nil:
		return errors.New("the HTTPS listener has no listener")
	case len(h.URLs) == 0:
		return errors.New("the HTTPS listener has no URL; fix: set https: public_urls: to the URLs clients use")
	case h.TLS == nil && len(h.Trusted) == 0:
		return errors.New("an HTTPS listener without TLS, behind a proxy, needs the proxy's addresses; fix: list them under trusted in https: tls: proxy: in ariesd's config")
	}
	for i, u := range h.URLs {
		n, err := keyproof.NormalizeURL(u)
		if err != nil {
			return fmt.Errorf("https: public_urls: %w", err)
		}
		h.URLs[i] = n
	}
	if h.Ping <= 0 {
		h.Ping = DefaultPing
	}
	if h.Mode == "" {
		h.Mode = map[bool]string{true: "files", false: "proxy"}[h.TLS != nil]
	}
	return nil
}

// web is the listener as Serve runs it: the server, and the WebSockets
// it has taken over from net/http, which net/http no longer tracks.
type web struct {
	s   *Server
	h   *HTTPS
	ctx context.Context

	mu      sync.Mutex
	closing bool
	conns   map[*wsconn.Conn]struct{}
	wg      sync.WaitGroup
}

// serveHTTPS serves the HTTPS listener until ctx is done, then stops
// taking connections, gives the plain requests in flight shutdownWait,
// closes every WebSocket and waits for its handler. It returns an error
// only when the listener failed.
func (s *Server) serveHTTPS(ctx context.Context) error {
	h := s.cfg.HTTPS
	lim := s.cfg.Limits
	w := &web{s: s, h: h, ctx: ctx, conns: map[*wsconn.Conn]struct{}{}}
	l := &limitedListener{Listener: h.Listener, total: lim.HTTPSConns, perAddr: lim.HTTPSConnsPerAddr, trusted: h.Trusted, tel: s.cfg.Telemetry,
		refused: func(addr netip.Addr, reason string) { w.refused(ctx, addr, reason) }}
	srv := &http.Server{
		Handler:           w.handler(),
		ReadHeaderTimeout: time.Duration(lim.HTTPSHeaderTimeout),
		MaxHeaderBytes:    lim.HTTPSHeaderBytes,
		IdleTimeout:       2 * time.Minute,
		TLSConfig:         h.TLS,
		// What net/http would print is a scanner's failed handshake or a
		// client's malformed request: noise, with text that is the
		// peer's, so none of it reaches the log.
		ErrorLog:    log.New(io.Discard, "", 0),
		BaseContext: func(net.Listener) context.Context { return ctx },
		ConnState:   func(c net.Conn, st http.ConnState) { countTLSFailure(s.cfg.Telemetry, c, st) },
	}
	errc := make(chan error, 1)
	go func() {
		if h.TLS != nil {
			errc <- srv.ServeTLS(l, "", "")
			return
		}
		errc <- srv.Serve(l)
	}()
	s.cfg.Logger.InfoContext(ctx, "HTTPS listener serving", "event", logs.HTTPSListening, "addr", h.Listener.Addr().String(),
		"mode", h.Mode, "urls", strings.Join(h.URLs, " "))
	var failed error
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), shutdownWait)
		_ = srv.Shutdown(sctx)
		cancel()
		_ = srv.Close()
		<-errc
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			failed = fmt.Errorf("the HTTPS listener stopped: %w", err)
			s.cfg.Logger.ErrorContext(ctx, "HTTPS listener stopped", "event", logs.HTTPSStopped, "err", err)
		}
		_ = srv.Close()
	}
	// net/http closes a listener it has started on; one it stopped
	// before it started on is closed here.
	_ = h.Listener.Close()
	w.closeAll()
	return failed
}

// countTLSFailure counts a connection net/http closed before its TLS
// handshake completed: a client that does not trust the certificate, a
// scanner, plain HTTP to the port. Nothing of the peer's is recorded,
// as nothing of it is logged (ErrorLog). A connection net/http hands
// over (a WebSocket) completed its handshake, and is never closed here.
func countTLSFailure(tel *telemetry.Telemetry, c net.Conn, st http.ConnState) {
	if tc, ok := c.(*tls.Conn); ok && st == http.StateClosed && !tc.ConnectionState().HandshakeComplete {
		tel.Count(telemetry.HTTPSTLSFailures)
	}
}

// handler routes the listener's two paths, and answers 404 for every
// other; every response says to use HTTPS only (HSTS), and a request's
// body is bounded.
func (w *web) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/aries", w.wellKnown)
	mux.HandleFunc("GET "+keyproof.ConnectPath, w.connect)
	if w.s.auth != nil {
		w.s.auth.routes(mux)
	}
	mux.HandleFunc("/", func(rw http.ResponseWriter, _ *http.Request) {
		writeRefusal(rw, http.StatusNotFound, proto.Errf(proto.CodeNotFound, "aries connects to "+keyproof.ConnectPath+"; check server.url in .aries.yaml",
			"nothing is served at this path"))
	})
	body := int64(w.s.cfg.Limits.HTTPSBodyBytes)
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Strict-Transport-Security", "max-age=31536000")
		rw.Header().Set("X-Content-Type-Options", "nosniff")
		rw.Header().Set("Cache-Control", "no-store")
		r.Body = http.MaxBytesReader(rw, r.Body, body)
		mux.ServeHTTP(rw, r)
	})
}

// WellKnown is what GET /.well-known/aries answers: the server's
// version, the transports on, the ways to sign in ("key", the proof by
// an SSH key, "signin", a sign-in at a provider, and "workload", a
// workload's token exchanged), the providers, and the workload
// providers.
type WellKnown struct {
	Version    string         `json:"version"`
	Transports []string       `json:"transports"`
	Auth       []string       `json:"auth"`
	Providers  []WellProvider `json:"providers,omitempty"`
	Workloads  []WellWorkload `json:"workloads,omitempty"`
}

// WellWorkload is a workload provider a service principal may sign in
// from: its name and kind, the audience a job asks its platform for,
// and whether the audience is bound to the job's key, as
// AUDIENCE/n/THUMBPRINT.
type WellWorkload struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Audience string `json:"audience"`
	Bound    bool   `json:"bound"`
}

// WellProvider is a provider a person may sign in at: its name in the
// server's settings, and its kind.
type WellProvider struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// wellKnown answers GET /.well-known/aries.
func (w *web) wellKnown(rw http.ResponseWriter, _ *http.Request) {
	out := WellKnown{Version: w.s.cfg.Version, Transports: w.s.transports(), Auth: []string{"key"}}
	if a := w.s.auth; a != nil && a.signsInPeople() {
		out.Auth = append(out.Auth, "signin")
		for _, name := range a.names {
			out.Providers = append(out.Providers, WellProvider{Name: name, Kind: a.providers[name].Kind()})
		}
	}
	if a := w.s.auth; a != nil && a.workload != nil {
		out.Auth = append(out.Auth, "workload")
		for _, p := range a.workload.Providers {
			out.Workloads = append(out.Workloads, WellWorkload{Name: p.Name(), Kind: p.Kind(), Audience: p.Audience(), Bound: p.Bound()})
		}
	}
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(out)
}

// transports names the transports the server serves, HTTPS first.
func (s *Server) transports() []string {
	var out []string
	if s.cfg.HTTPS != nil {
		out = append(out, TransportHTTPS)
	}
	if !s.cfg.NoSSH {
		out = append(out, TransportSSH)
	}
	return out
}

// writeRefusal answers with status and e, as {"error": …} in the form
// the protocol gives a refusal, which aries prints as it prints a
// refused welcome.
func writeRefusal(rw http.ResponseWriter, status int, e *proto.Error) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(struct {
		Error *proto.Error `json:"error"`
	}{e})
}

// connect serves GET /v1/connect: it refuses a browser and an address
// past its failures, upgrades, runs the key proof, and hands the
// connection to the server.
func (w *web) connect(rw http.ResponseWriter, r *http.Request) {
	addr := peerAddr(r.RemoteAddr)
	behind := w.h.TLS == nil
	tel := w.s.cfg.Telemetry
	// A page in a person's browser could open this WebSocket; a browser
	// always says the page's origin, and aries never sends one. No
	// cookie or header a browser holds signs in here, so this is
	// defense in depth.
	if r.Header.Get("Origin") != "" {
		tel.Count(telemetry.HTTPSUpgrades, telemetry.UpgradeRefused, telemetry.UpgradeOrigin)
		w.refused(r.Context(), addr, "origin")
		writeRefusal(rw, http.StatusForbidden, proto.Errf(proto.CodeForbidden,
			"connect with aries, which sends no Origin; a browser cannot connect here",
			"a request from a browser's page is not served"))
		return
	}
	if !behind {
		if ok, wait := w.s.authFails.has(addr.String(), w.s.now()); !ok {
			tel.Count(telemetry.HTTPSUpgrades, telemetry.UpgradeRefused, telemetry.UpgradeAuthFailures)
			tel.Count(telemetry.LimitRefusals, telemetry.LimitAuthFailures)
			w.refused(r.Context(), addr, "auth_failures")
			writeRefusal(rw, http.StatusTooManyRequests, proto.Errf(proto.CodeBusy,
				fmt.Sprintf("wait %s and retry with a key the server knows; `aries whoami` says which key aries offers", wait),
				"too many refused sign-ins from this address"))
			return
		}
	}
	// A sign-in's token, with its DPoP proof, is checked before the
	// upgrade (transport.md §5.4), and refused with a 401 aries reads as
	// it reads a refused welcome; a connection that brings none proves a
	// key after the upgrade.
	var in *signedIn
	var given *proto.Frame
	if r.Header.Get("Authorization") != "" {
		var perr *proto.Error
		var reason string
		if w.s.auth == nil {
			perr, reason = proto.Errf(proto.CodeAuth, "sign in with a key: unset the sign-in with `aries logout`, or ask the server's operator to set up login:",
				"this server signs no one in at a provider"), telemetry.TokenNoLogin
		} else {
			given, in, perr, reason = w.s.auth.admit(r)
		}
		// Counted before the refusal or the upgrade is answered, as a
		// key proof is.
		if perr != nil {
			tel.Count(telemetry.HTTPSTokens, reason)
			tel.Count(telemetry.HTTPSUpgrades, telemetry.UpgradeRefused, telemetry.UpgradeToken)
			w.refused(r.Context(), addr, reason)
			if !behind && perr.Code == proto.CodeAuth {
				w.s.authFails.take(addr.String(), w.s.now())
			}
			rw.Header().Set("WWW-Authenticate", `DPoP algs="ES256", error="invalid_token"`)
			status := http.StatusUnauthorized
			if perr.Code == proto.CodeBusy {
				status = http.StatusServiceUnavailable
			}
			writeRefusal(rw, status, perr)
			return
		}
		tel.Count(telemetry.HTTPSTokens, telemetry.ResultOK)
	}
	lim := w.s.cfg.Limits
	c, err := wsconn.Accept(rw, r, lim.WSMessageBytes)
	if err != nil {
		tel.Count(telemetry.HTTPSUpgrades, telemetry.UpgradeRefused, telemetry.UpgradeBadRequest)
		return // Accept answered 400
	}
	tel.Count(telemetry.HTTPSUpgrades, telemetry.ResultOK, telemetry.ReasonNone)
	if !w.track(c) {
		_ = c.Close()
		return
	}
	defer w.untrack(c)
	var perr *proto.Error
	var reason string
	if in == nil {
		given, perr, reason = w.prove(c)
		// Counted before the refusal or the welcome is written, so that a
		// client that has read either finds it counted.
		switch {
		case perr != nil:
			tel.Count(telemetry.HTTPSKeyProofs, reason)
		case given == nil:
			tel.Count(telemetry.HTTPSKeyProofs, telemetry.ResultFailed)
		default:
			tel.Count(telemetry.HTTPSKeyProofs, telemetry.ResultOK)
		}
	}
	if perr != nil {
		w.refused(r.Context(), addr, reason)
		if reason == "unknown_key" || reason == "bad_proof" || reason == "url" {
			if !behind {
				w.s.authFails.take(addr.String(), w.s.now())
			}
		}
		b, _ := json.Marshal(&proto.Frame{T: proto.FrameWelcome, Version: w.s.cfg.Version, Min: w.s.cfg.ProtoMin, Max: w.s.cfg.ProtoMax, Err: perr})
		_ = c.Write(append(b, '\n'))
		drain(c)
		_ = c.Close()
		return
	}
	if given == nil {
		_ = c.Close() // the client left, or said nothing in time
		return
	}
	done := make(chan struct{})
	var pings sync.WaitGroup
	pings.Go(func() {
		t := time.NewTicker(w.h.Ping)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if c.Ping() != nil {
					return
				}
			}
		}
	})
	w.s.serveConn(w.ctx, wsconn.Stream(c), given, in)
	drain(c)
	// Closing first ends a ping stuck behind a peer that reads nothing.
	_ = c.Close()
	close(done)
	pings.Wait()
}

// prove runs the key proof on c within upgrade_timeout. It returns the
// bridge frame the proof makes, or a refusal with a word for why, or
// neither when the client left or said nothing in time.
func (w *web) prove(c *wsconn.Conn) (*proto.Frame, *proto.Error, string) {
	_ = c.SetDeadline(time.Now().Add(time.Duration(w.s.cfg.Limits.UpgradeTimeout)))
	m, err := c.Read()
	if err != nil {
		return nil, nil, ""
	}
	if len(m) > maxProofMessage {
		return nil, proto.Errf(proto.CodeInvalid, proto.FixUpgrade+" aries to match the server", "the key offer is too large"), "invalid"
	}
	offer, err := keyproof.ParseOffer(m)
	if err != nil {
		return nil, proto.Errf(proto.CodeInvalid, proto.FixUpgrade+" aries to match the server: over HTTPS it proves its key before the hello",
			fmt.Sprintf("expected the key proof: %v", err)), "invalid"
	}
	if !slices.Contains(w.h.URLs, offer.URL) {
		return nil, proto.Errf(proto.CodeAuth, fmt.Sprintf("set server.url in .aries.yaml to %s", w.h.URLs[0]),
			fmt.Sprintf("this server is not %s, so a key proof for it is refused", offer.URL)), "url"
	}
	key, principal, fingerprint := -1, "", ""
	for i, k := range offer.Keys {
		if p, fp, ok := w.s.keyOwner(k); ok {
			key, principal, fingerprint = i, p, fp
			break
		}
	}
	// A disabled principal's key is known, and refused as theirs once
	// proved: not as an unknown key, which the address is charged for.
	disabled := false
	for i := 0; key < 0 && i < len(offer.Keys); i++ {
		if p, ok := w.s.disabledOwner(offer.Keys[i]); ok {
			key, principal, disabled = i, p, true
		}
	}
	if key < 0 {
		// A key past its end proves nothing (decision 89), and is told so.
		if perr := w.s.endedOffer(offer.Keys); perr != nil {
			return nil, perr, telemetry.ProofKeyEnded
		}
		return nil, proto.Errf(proto.CodeAuth,
			"send your public key to the aries admin, who adds it with `aries admin key add NAME FILE.pub`; `aries whoami` lists the keys aries offers",
			fmt.Sprintf("the server knows none of the keys offered (%d)", len(offer.Keys))), "unknown_key"
	}
	nonce := keyproof.NewNonce()
	if err := c.Write(keyproof.NewChallenge(key, nonce)); err != nil {
		return nil, nil, ""
	}
	if m, err = c.Read(); err != nil {
		return nil, nil, ""
	}
	sig, err := keyproof.ParseProof(m)
	if err != nil {
		return nil, proto.Errf(proto.CodeInvalid, proto.FixUpgrade+" aries to match the server", fmt.Sprintf("expected the key proof: %v", err)), "invalid"
	}
	if err := keyproof.Verify(offer.Keys[key], offer.URL, nonce, sig); err != nil {
		return nil, proto.Errf(proto.CodeAuth, "check that the key file aries reads is the private half of the key it offers; `aries whoami` names it",
			"the key proof does not verify"), "bad_proof"
	}
	if disabled {
		return nil, disabledErr(principal), telemetry.ProofDisabled
	}
	_ = c.SetDeadline(time.Time{})
	return &proto.Frame{T: proto.FrameBridge, Principal: principal, Key: fingerprint}, nil, ""
}

// keyOwner finds whose pub is as sshd would take it (decision 26): by
// the control store's key, unless the hand-maintained authorized_keys
// file gives the key to another principal; else by a line of that file
// that forces the bridge. The fingerprint is returned only for the
// store's key, which the handshake checks and whose removal closes the
// connection, as for a line of the keys file. A reserved principal, or
// a name no principal may have, is no one's.
func (s *Server) keyOwner(pub ssh.PublicKey) (principal, fingerprint string, ok bool) {
	if s.cfg.HTTPS != nil && s.cfg.HTTPS.Keys != nil {
		return s.cfg.HTTPS.Keys(pub)
	}
	valid := func(p string) bool { return PrincipalPattern.MatchString(p) && !store.Reserved(p) }
	fp := ssh.FingerprintSHA256(pub)
	if s.cfg.Control != nil {
		owner, byHand := s.handOwner(fp)
		if p, key, ok := s.cfg.Control.AuthorizedKey(fp); ok && (!byHand || owner == p) && valid(p) {
			if stored, err := publicKey(key); err == nil && bytes.Equal(stored.Marshal(), pub.Marshal()) {
				return p, fp, true
			}
		}
	}
	if p, ok := s.handBridgeOwner(fp); ok && valid(p) {
		return p, "", true
	}
	return "", "", false
}

// disabledOwner finds the disabled principal whose key of the control
// store pub is, as keyOwner finds an active one: unless the
// hand-maintained authorized_keys file gives the key to another
// principal. It asks the control store whether or not HTTPS.Keys is
// set: sshd, which that mirrors, has no line for such a key at all.
func (s *Server) disabledOwner(pub ssh.PublicKey) (principal string, ok bool) {
	if s.cfg.Control == nil {
		return "", false
	}
	fp := ssh.FingerprintSHA256(pub)
	p, key, ok := s.cfg.Control.DisabledKey(fp)
	if !ok {
		return "", false
	}
	if owner, byHand := s.handOwner(fp); byHand && owner != p {
		return "", false
	}
	if stored, err := publicKey(key); err != nil || !bytes.Equal(stored.Marshal(), pub.Marshal()) {
		return "", false
	}
	return p, true
}

// refused logs a connection refused before its hello, sampled per
// address, and counts it.
func (w *web) refused(ctx context.Context, addr netip.Addr, reason string) {
	w.s.refusals.line(ctx, w.s.cfg.Logger, "https:"+addr.String(), w.s.now(), slog.LevelInfo, logs.HTTPSRefused,
		"HTTPS connection refused", "reason", reason, "addr", addr.String())
}

// track adds c to the WebSockets the listener holds, unless it is
// stopping.
func (w *web) track(c *wsconn.Conn) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closing {
		return false
	}
	w.conns[c] = struct{}{}
	w.wg.Add(1)
	return true
}

// untrack removes c once its handler is done.
func (w *web) untrack(c *wsconn.Conn) {
	w.mu.Lock()
	delete(w.conns, c)
	w.mu.Unlock()
	w.wg.Done()
}

// closeAll closes every WebSocket and waits for their handlers. track
// takes none after it.
func (w *web) closeAll() {
	w.mu.Lock()
	w.closing = true
	for c := range w.conns {
		_ = c.Close()
	}
	w.mu.Unlock()
	w.wg.Wait()
}

// peerAddr is the address of a request's peer, without its port.
func peerAddr(remote string) netip.Addr {
	ap, err := netip.ParseAddrPort(remote)
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr().Unmap()
}

// limitedListener is the HTTPS listener's own: it closes, unanswered, a
// connection past the total or its address's share, and, behind a
// proxy, one from an address it does not trust. tel counts the
// connections it holds, and those it refuses.
type limitedListener struct {
	net.Listener
	total, perAddr int
	trusted        []netip.Prefix
	refused        func(addr netip.Addr, reason string)
	tel            *telemetry.Telemetry

	mu sync.Mutex
	n  int
	by map[netip.Addr]int
}

func (l *limitedListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		addr := peerAddr(c.RemoteAddr().String())
		if reason := l.enter(addr); reason != "" {
			_ = c.Close()
			l.count(reason)
			l.refused(addr, reason)
			continue
		}
		l.tel.Record(telemetry.HTTPSConnections, 1)
		return &limitedConn{Conn: c, l: l, addr: addr}, nil
	}
}

// count counts a connection enter refused for reason, and the limit it
// was past by name.
func (l *limitedListener) count(reason string) {
	switch reason {
	case "untrusted":
		l.tel.Count(telemetry.HTTPSConnectionsRefused, telemetry.ListenerUntrusted)
		return
	case "conns":
		l.tel.Count(telemetry.LimitRefusals, telemetry.LimitHTTPSConns)
	case "conns_per_addr":
		l.tel.Count(telemetry.LimitRefusals, telemetry.LimitHTTPSConnsPerAddr)
	}
	l.tel.Count(telemetry.HTTPSConnectionsRefused, telemetry.ListenerLimit)
}

// enter counts a connection from addr, or says why it is refused.
func (l *limitedListener) enter(addr netip.Addr) string {
	proxied := len(l.trusted) > 0
	if proxied && !slices.ContainsFunc(l.trusted, func(p netip.Prefix) bool { return p.Contains(addr) }) {
		return "untrusted"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.n >= l.total:
		return "conns"
	case !proxied && l.by[addr] >= l.perAddr:
		return "conns_per_addr"
	}
	if l.by == nil {
		l.by = map[netip.Addr]int{}
	}
	l.n++
	l.by[addr]++
	return ""
}

// leave uncounts a connection enter counted.
func (l *limitedListener) leave(addr netip.Addr) {
	l.tel.Record(telemetry.HTTPSConnections, -1)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n--
	if l.by[addr]--; l.by[addr] <= 0 {
		delete(l.by, addr)
	}
}

// limitedConn is a connection limitedListener counted, until it closes.
type limitedConn struct {
	net.Conn
	l    *limitedListener
	addr netip.Addr
	once sync.Once
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.l.leave(c.addr) })
	return err
}

// drainTime bounds how long drain reads what a peer sent before a close.
const drainTime = time.Second

// drain reads and drops what the peer has sent and the server has not
// read, for at most drainTime, before c is closed. A refusal is often
// written before the client's next message is read (the handshake
// refuses a disabled principal before it reads the hello), and a socket
// closed with unread data sends a reset, which can discard the refusal
// before the client reads it. Read, the close is an orderly one.
func drain(c *wsconn.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(drainTime))
	for {
		if _, err := c.Read(); err != nil {
			return
		}
	}
}
