package server

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ariesworx/aries/internal/dpop"
	"github.com/ariesworx/aries/internal/keyproof"
	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/oidc"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/safetext"
	"github.com/ariesworx/aries/internal/store"
	"github.com/ariesworx/aries/internal/telemetry"
)

// The server is its own authorization server (transport.md §6): a
// person signs in at a provider the operator configured, through the
// server, and the server issues a token of its own, opaque, bound to a
// key the client holds (DPoP, RFC 9449), for the day (decision 58).
//
//  1. aries opens the browser at GET /oauth/authorize, with its own PKCE
//     challenge, state and loopback redirect.
//  2. The server remembers the request and sends the browser on to the
//     provider, with its own PKCE, state, nonce, prompt=login and
//     max_age, and its one callback.
//  3. At GET /auth/callback the server exchanges the provider's code
//     itself (decision 85), verifies the ID token (internal/oidc), and
//     sends the browser back to aries's loopback address with a code of
//     its own, the client's state, and iss (RFC 9207). A refusal rides
//     the code, so that aries hears it, with its fix, at step 4.
//  4. At POST /oauth/token aries spends that code with its PKCE
//     verifier and a DPoP proof by a key it has just made. The store
//     finds the person, or binds the invitation the verified address
//     names (store.Control.SignIn), and grants the sign-in; the token
//     goes back once, and only its hash is kept.
//
// The token is then presented on /v1/connect, with a DPoP proof, in
// place of the key proof. POST /oauth/revoke ends it, and closes its
// connections at once (decision 59).

// Login is the server's sign-in: its providers and the life of what it
// issues. It needs Control, whose identities say who signs in, and
// HTTPS.
type Login struct {
	// Providers are the providers people sign in at, by name.
	Providers []*oidc.Provider
	// TokenLifetime is how long a sign-in lasts (decision 58). Zero takes
	// DefaultTokenLifetime.
	TokenLifetime time.Duration
	// FreshAuth bounds how long before the sign-in the person last
	// authenticated at the provider (auth_time), and is sent as max_age.
	// Zero takes DefaultFreshAuth.
	FreshAuth time.Duration
	// PruneAfter is how long a sign-in that ended is kept, for `aries
	// admin signin list`, before it is deleted. Zero takes
	// DefaultSignInKeep.
	PruneAfter time.Duration
}

// The defaults and bounds of Login.
const (
	DefaultTokenLifetime = 24 * time.Hour
	MaxTokenLifetime     = 7 * 24 * time.Hour
	DefaultFreshAuth     = 10 * time.Minute
	DefaultSignInKeep    = time.Hour
	// signInWait is how long a sign-in started at the provider may take
	// to come back.
	signInWait = 10 * time.Minute
	// codeLife is how long the server's code waits for aries to spend
	// it.
	codeLife = time.Minute
)

// ClientCLI is aries's client id: a public client, with a loopback
// redirect on any port and DPoP required.
const ClientCLI = "aries-cli"

// The paths of the authorization server.
const (
	pathAuthorize = "/oauth/authorize"
	pathCallback  = "/auth/callback"
	pathToken     = "/oauth/token" //nolint:gosec // a path, not a credential
	pathRevoke    = "/oauth/revoke"
	pathASMeta    = "/.well-known/oauth-authorization-server"
)

// authServer is the sign-in's state in the daemon's memory: sign-ins in
// progress and the codes not yet spent (signInHolds), bounded in all
// (Limits.SignInsPending) and per address (Limits.SignInsPendingPerAddr)
// and lost at a restart, which costs a person a second `aries login`;
// and the DPoP proofs seen, bounded
// (Limits.DPoPProofs). Several daemons in front of one database would
// each hold their own, so that a proof replayed to another daemon within
// its window would pass (transport.md §13).
type authServer struct {
	s         *Server
	login     Login
	providers map[string]*oidc.Provider
	names     []string // the providers' names, in the settings' order
	proofs    *dpop.Verifier

	mu   sync.Mutex
	held *signInHolds // guarded by mu
}

// pendingSignIn is a sign-in sent to the provider and not yet back.
type pendingSignIn struct {
	provider *oidc.Provider
	// The client's request.
	redirect, challenge, state string
	scopes                     []string
	// The server's own request to the provider.
	verifier, nonce string
	until           time.Time
}

// issuedCode is the server's code, waiting for aries to spend it: who
// the provider said signed in, or why the sign-in was refused.
type issuedCode struct {
	redirect, challenge string
	scopes              []string
	provider            string
	id                  *oidc.Identity
	refused             *proto.Error
	until               time.Time
}

// newAuthServer makes the authorization server of login.
func newAuthServer(s *Server, login Login) (*authServer, error) {
	login.TokenLifetime = cmp.Or(login.TokenLifetime, DefaultTokenLifetime)
	login.FreshAuth = cmp.Or(login.FreshAuth, DefaultFreshAuth)
	login.PruneAfter = cmp.Or(login.PruneAfter, DefaultSignInKeep)
	switch {
	case len(login.Providers) == 0:
		return nil, errors.New("login: no providers; fix: list the providers people sign in at under login: providers")
	case login.TokenLifetime > MaxTokenLifetime:
		return nil, fmt.Errorf("login: token_lifetime %s is over %s", proto.Span(login.TokenLifetime), proto.Span(MaxTokenLifetime))
	}
	perAddr := s.cfg.Limits.SignInsPendingPerAddr
	if s.cfg.HTTPS.TLS == nil {
		perAddr = 0 // behind a proxy, every request is the proxy's
	}
	a := &authServer{s: s, login: login, providers: map[string]*oidc.Provider{}, proofs: dpop.NewVerifier(s.cfg.Limits.DPoPProofs),
		held: newSignInHolds(s.cfg.Limits.SignInsPending, perAddr)}
	for _, p := range login.Providers {
		if _, dup := a.providers[p.Name()]; dup {
			return nil, fmt.Errorf("login: two providers are named %s", p.Name())
		}
		a.providers[p.Name()] = p
		a.names = append(a.names, p.Name())
	}
	return a, nil
}

// HasProvider reports whether the server signs people in at a provider
// named name; with no login: providers, any name is allowed, since
// nothing signs in.
func (s *Server) hasProvider(name string) bool {
	if s.auth == nil {
		return true
	}
	_, ok := s.auth.providers[name]
	return ok
}

// routes adds the authorization server's paths to mux.
func (a *authServer) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+pathASMeta, a.metadata)
	mux.HandleFunc("GET "+pathAuthorize, a.authorize)
	mux.HandleFunc("GET "+pathCallback, a.callback)
	mux.HandleFunc("POST "+pathToken, a.token)
	mux.HandleFunc("POST "+pathRevoke, a.revoke)
}

// base is the server's first public URL: its issuer, and where the
// provider sends the browser back.
func (a *authServer) base() string { return a.s.cfg.HTTPS.URLs[0] }

// at is each of the server's URLs with path: what a DPoP proof may name.
func (a *authServer) at(path string) []string {
	out := make([]string, len(a.s.cfg.HTTPS.URLs))
	for i, u := range a.s.cfg.HTTPS.URLs {
		out[i] = u + path
	}
	return out
}

// now is the store's time (rule 19).
func (a *authServer) now() time.Time { return a.s.cfg.Control.Now() }

// metadata answers RFC 8414's document.
func (a *authServer) metadata(rw http.ResponseWriter, _ *http.Request) {
	b := a.base()
	writeJSON(rw, http.StatusOK, map[string]any{
		"issuer": b, "authorization_endpoint": b + pathAuthorize, "token_endpoint": b + pathToken, "revocation_endpoint": b + pathRevoke,
		"response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code"},
		"code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none"},
		"dpop_signing_alg_values_supported": []string{"ES256"}, "scopes_supported": []string{proto.ScopeCLI, proto.ScopeMCP},
		"authorization_response_iss_parameter_supported": true,
	})
}

func writeJSON(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(v)
}

// signInPage answers a browser with a short page of plain text: the only
// pages the sign-in shows are its refusals before a redirect is known
// to be the client's. Nothing in it is the request's.
func signInPage(rw http.ResponseWriter, status int, text string) {
	rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
	rw.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	rw.Header().Set("X-Frame-Options", "DENY")
	rw.WriteHeader(status)
	_, _ = fmt.Fprintln(rw, text)
}

// loopback reports whether raw is a loopback redirect aries may name:
// http to 127.0.0.1 or [::1], on any port (RFC 8252 §7.3), with a path
// and no query or fragment.
func loopback(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.RawQuery != "" || u.Fragment != "" || u.User != nil || u.Port() == "" {
		return false
	}
	ip, err := netip.ParseAddr(u.Hostname())
	return err == nil && ip.IsLoopback()
}

// redirectWith sends the browser to redirect with v and the server's
// issuer, as RFC 6749 §4.1.2 and RFC 9207 have it.
func (a *authServer) redirectWith(rw http.ResponseWriter, r *http.Request, redirect string, v url.Values) {
	v.Set("iss", a.base())
	http.Redirect(rw, r, redirect+"?"+v.Encode(), http.StatusFound) //nolint:gosec // redirect is a loopback address authorize checked, or the one a sign-in in progress holds
}

// authorize begins a sign-in: it checks aries's request, remembers it,
// and sends the browser on to the provider with the server's own PKCE,
// state, nonce and prompt=login (decision 85). A request whose client
// or redirect is not one is answered with a page, never a redirect
// (RFC 9700 §4.11); any other refusal goes back to the redirect.
func (a *authServer) authorize(rw http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect := q.Get("redirect_uri")
	if q.Get("client_id") != ClientCLI || !loopback(redirect) {
		a.refused(r, "", "client")
		signInPage(rw, http.StatusBadRequest, "This sign-in was not started by aries: the client or its redirect is not one this server knows. Run `aries login` again.")
		return
	}
	back := func(code, why string) {
		a.refused(r, "", code)
		a.redirectWith(rw, r, redirect, url.Values{"error": {code}, "error_description": {why}, "state": {q.Get("state")}})
	}
	// Anyone may start a sign-in, and each is held for a while, so an
	// address is held to its share, and one that keeps asking past it
	// is refused as one that keeps failing to sign in is. Behind a
	// proxy every request is the proxy's, so neither applies.
	addr := peerAddr(r.RemoteAddr).String()
	if a.held.perAddr == 0 {
		addr = ""
	} else if ok, wait := a.s.authFails.has(addr, a.s.now()); !ok {
		a.s.cfg.Telemetry.Count(telemetry.LimitRefusals, telemetry.LimitAuthFailures)
		back("temporarily_unavailable", fmt.Sprintf("too many refused sign-ins from this address; wait %s", wait))
		return
	}
	scopes := strings.Fields(cmp.Or(q.Get("scope"), proto.ScopeCLI+" "+proto.ScopeMCP))
	state := q.Get("state")
	switch {
	case q.Get("response_type") != "code":
		back("unsupported_response_type", "the server answers response_type code alone")
		return
	case q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43:
		back("invalid_request", "PKCE with S256 is required")
		return
	case state == "" || len(state) > 512 || !safetext.ValidLine(state):
		back("invalid_request", "a state of at most 512 printable characters is required")
		return
	case slices.ContainsFunc(scopes, func(s string) bool { return s != proto.ScopeCLI && s != proto.ScopeMCP }):
		back("invalid_scope", "aries signs in for the scopes cli and mcp")
		return
	case q.Get("resource") != "" && !slices.Contains(a.s.cfg.HTTPS.URLs, q.Get("resource")):
		back("invalid_target", "the resource is not this server")
		return
	}
	name := q.Get("provider")
	if name == "" && len(a.names) == 1 {
		name = a.names[0]
	}
	p := a.providers[name]
	if p == nil {
		back("invalid_request", fmt.Sprintf("this server signs people in at %s; choose one with `aries login --provider NAME`", strings.Join(a.names, ", ")))
		return
	}
	ps := &pendingSignIn{provider: p, redirect: redirect, challenge: q.Get("code_challenge"), state: state, scopes: scopes,
		verifier: rand.Text() + rand.Text(), nonce: rand.Text()}
	up := rand.Text()
	if !a.holdPending(addr, up, ps) {
		a.s.cfg.Telemetry.Count(telemetry.LimitRefusals, telemetry.LimitSignInsPendingPerAddr)
		a.s.authFails.take(addr, a.s.now())
		back("temporarily_unavailable", fmt.Sprintf("this address has %d sign-ins in progress, the most the server holds from one address; "+
			"finish one in its browser tab, or wait for one to lapse, %d minutes after it began",
			a.held.perAddr, int(signInWait/time.Minute)))
		return
	}
	sum := sha256.Sum256([]byte(ps.verifier))
	to, err := p.AuthorizeURL(r.Context(), oidc.AuthRequest{State: up, Nonce: ps.nonce, Challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
		Redirect: a.base() + pathCallback, MaxAge: a.login.FreshAuth})
	if err != nil {
		a.mu.Lock()
		a.held.takePending(up)
		a.mu.Unlock()
		a.providerFailed(r.Context(), p, err)
		back("temporarily_unavailable", fmt.Sprintf("the server cannot reach the provider %s; retry, and if it persists tell the server's admin", p.Name()))
		return
	}
	http.Redirect(rw, r, to, http.StatusFound)
}

// holdPending holds ps, started from addr, under the server's state at
// the provider, up, unless addr holds Limits.SignInsPendingPerAddr
// already. Its time is taken under mu, so that each kind signInHolds holds
// is added in the order it lapses.
func (a *authServer) holdPending(addr, up string, ps *pendingSignIn) bool {
	a.mu.Lock()
	now := a.now()
	ps.until = now.Add(signInWait)
	held, evicted := a.held.addPending(now, addr, up, ps)
	a.mu.Unlock()
	if evicted {
		a.s.cfg.Telemetry.Count(telemetry.LimitRefusals, telemetry.LimitSignInsPending)
	}
	return held
}

// holdCode holds ic, for the sign-in addr started, under the code's hash.
func (a *authServer) holdCode(addr, hash string, ic *issuedCode) {
	a.mu.Lock()
	now := a.now()
	ic.until = now.Add(codeLife)
	evicted := a.held.addCode(now, addr, hash, ic)
	a.mu.Unlock()
	if evicted {
		a.s.cfg.Telemetry.Count(telemetry.LimitRefusals, telemetry.LimitSignInsPending)
	}
}

// prune lets go of what expired, and of the DPoP proofs whose window
// passed, and deletes the sign-ins that ended before Login.PruneAfter.
func (a *authServer) prune(ctx context.Context) {
	now := a.now()
	a.mu.Lock()
	a.held.expire(now)
	a.mu.Unlock()
	a.proofs.Prune(now)
	if n, err := a.s.cfg.Control.PruneSignIns(ctx, now.Add(-a.login.PruneAfter)); err != nil {
		a.s.cfg.Logger.ErrorContext(ctx, "prune sign-ins", "event", logs.ReaperPruneFailed, "err", err)
	} else if n > 0 {
		a.s.cfg.Logger.InfoContext(ctx, "pruned sign-ins that ended", "event", logs.ReaperPruned, "signins", n)
	}
}

// callback is where the provider sends the browser back. The server
// exchanges the provider's code itself, verifies the ID token, and sends
// the browser back to aries with a code of its own, which carries who
// signed in or why not.
func (a *authServer) callback(rw http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	a.mu.Lock()
	ps, addr := a.held.takePending(q.Get("state"))
	a.mu.Unlock()
	if ps == nil || !a.now().Before(ps.until) {
		a.refused(r, "", "unknown_state")
		signInPage(rw, http.StatusBadRequest, "This sign-in is not one the server is waiting for: it took longer than 10 minutes, "+
			"or the server let it go to make room for newer ones. Run `aries login` again.")
		return
	}
	ic := &issuedCode{redirect: ps.redirect, challenge: ps.challenge, scopes: ps.scopes, provider: ps.provider.Name()}
	if e := q.Get("error"); e != "" {
		ic.refused = proto.Errf(proto.CodeForbidden, "run `aries login` again; if the provider refuses you again, ask its administrator",
			fmt.Sprintf("the provider %s refused the sign-in (%s)", ps.provider.Name(), safeWord(e)))
	} else {
		ic.id, ic.refused = a.verify(r.Context(), ps, q.Get("code"))
	}
	if ic.refused != nil {
		a.refusedSignIn(r.Context(), ps.provider.Name(), ic.refused)
	}
	code := rand.Text() + rand.Text()
	a.holdCode(addr, hashOf(code), ic)
	a.redirectWith(rw, r, ps.redirect, url.Values{"code": {code}, "state": {ps.state}})
}

// verify exchanges the provider's code and checks its ID token, or says
// why the sign-in is refused.
func (a *authServer) verify(ctx context.Context, ps *pendingSignIn, code string) (*oidc.Identity, *proto.Error) {
	p := ps.provider
	raw, err := p.Exchange(ctx, code, ps.verifier, a.base()+pathCallback)
	if err == nil {
		var id *oidc.Identity
		if id, err = p.Check(ctx, raw, oidc.Expect{Nonce: ps.nonce, AuthAge: a.login.FreshAuth}); err == nil {
			return id, nil
		}
	}
	switch {
	case errors.Is(err, oidc.ErrStale):
		return nil, proto.Errf(proto.CodeAuth, "run `aries login` again; the provider will ask you to sign in",
			fmt.Sprintf("the sign-in at %s is not fresh: %v", p.Name(), errors.Unwrap(err)))
	case errors.Is(err, oidc.ErrNotAllowed):
		return nil, proto.Errf(proto.CodeForbidden, "sign in with the account the server's settings ask for, or ask the server's admin",
			fmt.Sprintf("%s: %v", p.Name(), err))
	case errors.Is(err, oidc.ErrUnavailable):
		a.providerFailed(ctx, p, err)
		return nil, proto.Errf(proto.CodeUnavailable, "retry; if it persists, the server's admin checks the provider in `aries admin status`",
			fmt.Sprintf("the server could not check the sign-in at %s", p.Name()))
	}
	return nil, proto.Errf(proto.CodeAuth, "run `aries login` again", fmt.Sprintf("the sign-in at %s did not verify", p.Name()))
}

// safeWord is s if it is an OAuth error code's characters, and "an
// error" otherwise: what a provider sends back is not repeated.
func safeWord(s string) string {
	if s == "" || len(s) > 64 || strings.Trim(s, "abcdefghijklmnopqrstuvwxyz_") != "" {
		return "an error"
	}
	return s
}

// hashOf is the SHA-256 of a code or token, in hex: what is kept of it.
func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// oauthError answers the token or revocation endpoint's refusal: OAuth's
// error and description, and the protocol's code and fix beside them,
// which aries prints as it prints any refusal.
func oauthError(rw http.ResponseWriter, status int, oauthCode string, e *proto.Error) {
	writeJSON(rw, status, map[string]string{"error": oauthCode, "error_description": e.Message, "code": string(e.Code), "fix": e.Fix})
}

// token is the token endpoint: the server's code spent, with aries's
// PKCE verifier and a DPoP proof, for a token bound to the proof's key.
func (a *authServer) token(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Pragma", "no-cache")
	if ok, wait := a.heard(r); !ok {
		a.s.cfg.Telemetry.Count(telemetry.LimitRefusals, telemetry.LimitAuthFailures)
		a.refused(r, "", "auth_failures")
		oauthError(rw, http.StatusTooManyRequests, "invalid_request", proto.Errf(proto.CodeBusy,
			fmt.Sprintf("wait %s and run `aries login` again", wait), "too many refused sign-ins from this address"))
		return
	}
	if err := r.ParseForm(); err != nil {
		oauthError(rw, http.StatusBadRequest, "invalid_request", proto.Errf(proto.CodeInvalid, proto.FixUpgrade+" aries", "the request is not a form"))
		return
	}
	f := r.PostForm
	if f.Get("grant_type") != "authorization_code" {
		oauthError(rw, http.StatusBadRequest, "unsupported_grant_type", proto.Errf(proto.CodeInvalid, proto.FixUpgrade+" aries",
			"the server exchanges an authorization code alone"))
		return
	}
	hash := hashOf(f.Get("code"))
	a.mu.Lock()
	ic := a.held.takeCode(hash) // a code is spent once, whatever comes of it
	a.mu.Unlock()
	now := a.now()
	// The proof is checked here and remembered only once the code is
	// shown to be live and this client's: anyone may sign a proof with a
	// key of their own, and the proofs remembered are bounded, so that
	// remembering one before would let anyone fill them (issue #160).
	proof, err := dpop.Check(r.Header.Get("DPoP"), http.MethodPost, a.at(pathToken), "", now)
	if err != nil {
		e, _ := errors.AsType[*dpop.Error](err)
		a.refused(r, "", "dpop_"+string(e.Reason))
		a.failed(r)
		oauthError(rw, http.StatusBadRequest, "invalid_dpop_proof", proto.Errf(proto.CodeAuth, "run `aries login` again; check this machine's clock", err.Error()))
		return
	}
	if ic == nil || !now.Before(ic.until) || f.Get("client_id") != ClientCLI || f.Get("redirect_uri") != ic.redirect {
		a.refused(r, "", "code")
		a.failed(r)
		oauthError(rw, http.StatusBadRequest, "invalid_grant", proto.Errf(proto.CodeAuth, "run `aries login` again",
			"the sign-in's code is unknown, spent, older than a minute, or for another client or redirect"))
		return
	}
	sum := sha256.Sum256([]byte(f.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != ic.challenge {
		a.refused(r, ic.provider, "pkce")
		a.failed(r)
		oauthError(rw, http.StatusBadRequest, "invalid_grant", proto.Errf(proto.CodeAuth, "run `aries login` again", "the PKCE verifier is not the sign-in's"))
		return
	}
	if ic.refused != nil {
		oauthError(rw, http.StatusForbidden, "access_denied", ic.refused)
		return
	}
	if err := a.proofs.Spend(proof, now); err != nil {
		e, _ := errors.AsType[*dpop.Error](err)
		a.refused(r, ic.provider, "dpop_"+string(e.Reason))
		if e.Reason == dpop.ReasonBusy {
			a.s.cfg.Telemetry.Count(telemetry.LimitRefusals, telemetry.LimitDPoPProofs)
			oauthError(rw, http.StatusServiceUnavailable, "temporarily_unavailable", proto.Errf(proto.CodeBusy, "run `aries login` again in a minute", err.Error()))
			return
		}
		a.failed(r)
		oauthError(rw, http.StatusBadRequest, "invalid_dpop_proof", proto.Errf(proto.CodeAuth, "run `aries login` again", err.Error()))
		return
	}
	a.grant(rw, r, ic, proof.JKT)
}

// grant has the store record the sign-in, and answers with its token.
func (a *authServer) grant(rw http.ResponseWriter, r *http.Request, ic *issuedCode, jkt string) {
	f := r.PostForm
	machine := cmp.Or(f.Get("machine"), "unknown")
	if !validActorPart(machine) || len(machine) > 255 {
		machine = "unknown"
	}
	flow := store.FlowLoopback
	if f.Get("flow") == store.FlowURL {
		flow = store.FlowURL
	}
	tok := rand.Text() + rand.Text() // 256 random bits and more, base32
	g, err := a.s.cfg.Control.SignIn(r.Context(), store.SignIn{Provider: ic.provider, Issuer: ic.id.Issuer, Subject: ic.id.Subject,
		Address: ic.id.Address, Verified: ic.id.Verified, ClientID: ClientCLI, Scopes: ic.scopes, JKT: jkt, Resource: a.base(),
		Flow: flow, Machine: machine, Lifetime: a.login.TokenLifetime, TokenHash: hashOf(tok)})
	if err != nil {
		e := a.signInErr(ic.provider, err)
		a.refusedSignIn(r.Context(), ic.provider, e)
		status := http.StatusForbidden
		if e.Code == proto.CodeUnavailable || e.Code == proto.CodeBusy {
			status = http.StatusServiceUnavailable
		}
		oauthError(rw, status, "access_denied", e)
		return
	}
	a.s.Audit(r.Context())
	a.s.cfg.Logger.InfoContext(r.Context(), "signed in", "event", logs.SignInGranted, "principal", g.Principal, "provider", g.Provider,
		"grant", g.ID, "flow", g.Flow)
	a.s.announceSignIn(r.Context(), g) // before the token is answered, so before it can be used
	writeJSON(rw, http.StatusOK, map[string]any{
		"access_token": tok, "token_type": "DPoP", "expires_in": int(g.EndsAt.Sub(a.now()).Seconds()),
		"expires_at": g.EndsAt.Format(time.RFC3339), "scope": strings.Join(g.Scopes, " "),
		"principal": g.Principal, "provider": g.Provider, "grant": g.ID,
	})
}

// signInErr words the store's refusal of a sign-in at provider, for the
// person signing in: their own address, which no log line holds.
func (a *authServer) signInErr(provider string, err error) *proto.Error {
	if se, ok := errors.AsType[*store.SignInError](err); ok {
		switch se.Reason {
		case store.SignInLapsed:
			return proto.Errf(proto.CodeForbidden, "ask an admin of your project for a new invitation: `aries admin invite add NAME ADDRESS --provider "+provider+"`",
				se.Error())
		case store.SignInUnverified:
			return proto.Errf(proto.CodeForbidden, "verify the address at the provider, or sign in with the account whose address was invited", se.Error())
		case store.SignInRebound:
			return proto.Errf(proto.CodeAuth, "sign in with the account you used before, or ask an admin", se.Error())
		case store.SignInDisabled:
			return disabledErr(se.Principal)
		}
		return proto.Errf(proto.CodeForbidden, "ask an admin of your project to invite this address: `aries admin invite add NAME ADDRESS --provider "+provider+"`",
			se.Error())
	}
	if ce, ok := errors.AsType[*store.ControlLimitError](err); ok {
		a.s.cfg.Telemetry.Count(telemetry.LimitRefusals, telemetry.LimitGrants)
		return proto.Errf(proto.CodeBusy, "run `aries logout` on a machine you no longer use, or ask an admin to end a sign-in with `aries admin signin end ID`",
			strings.TrimPrefix(ce.Error(), store.ErrInvalid.Error()+": "))
	}
	a.s.cfg.Logger.Error("record a sign-in", "event", logs.RequestFailed, "provider", provider, "err", err)
	return proto.Errf(proto.CodeUnavailable, "retry; if it persists, the server's admin should check the ariesd log", "the server could not record the sign-in")
}

// revoke is the revocation endpoint (RFC 7009): the token's sign-in
// ends, and its connections close at once (decision 59). It answers 200
// whether or not the token was known.
func (a *authServer) revoke(rw http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthError(rw, http.StatusBadRequest, "invalid_request", proto.Errf(proto.CodeInvalid, proto.FixUpgrade+" aries", "the request is not a form"))
		return
	}
	g, ok := a.s.cfg.Control.TokenGrant(hashOf(r.PostForm.Get("token")))
	if !ok {
		rw.WriteHeader(http.StatusOK)
		return
	}
	actor := store.Actor{Principal: g.Principal, Session: "signin", Machine: g.Machine}
	if _, changed, err := a.s.cfg.Control.EndSignIn(r.Context(), actor, g.ID, store.EndLogout); err != nil {
		a.s.cfg.Logger.ErrorContext(r.Context(), "end a sign-in", "event", logs.RequestFailed, "grant", g.ID, "err", err)
		oauthError(rw, http.StatusServiceUnavailable, "temporarily_unavailable",
			proto.Errf(proto.CodeUnavailable, "retry `aries logout`", "the server could not end the sign-in"))
		return
	} else if changed {
		a.s.Audit(r.Context())
		a.s.endGrant(r.Context(), g.ID, store.EndLogout)
	}
	rw.WriteHeader(http.StatusOK)
}

// signedIn is the sign-in a connection presented: what its session is
// held to.
type signedIn struct {
	grant    string
	hash     string
	identity int64
	provider string
	scopes   []string
	ends     time.Time
}

// admit checks the token and DPoP proof of an upgrade to /v1/connect,
// and returns the sign-in, or a refusal and a word for the log.
func (a *authServer) admit(r *http.Request) (*proto.Frame, *signedIn, *proto.Error, string) {
	again := proto.FixSignIn
	scheme, tok, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	switch {
	case !strings.EqualFold(scheme, "DPoP"):
		return nil, nil, proto.Errf(proto.CodeAuth, proto.FixUpgrade+" aries, then "+again,
			"a token here is presented as DPoP, with a proof by its key"), telemetry.TokenScheme
	case tok == "" || len(tok) > 256:
		return nil, nil, proto.Errf(proto.CodeAuth, again, "the sign-in's token is missing"), telemetry.TokenMissing
	}
	g, ok := a.s.cfg.Control.TokenGrant(hashOf(tok))
	if !ok {
		return nil, nil, proto.Errf(proto.CodeAuth, again, "the sign-in is unknown to this server, or was ended"), telemetry.TokenUnknown
	}
	now := a.now()
	if !g.Live(now) {
		return nil, nil, proto.Errf(proto.CodeAuth, again, fmt.Sprintf("your sign-in ended at %s", g.EndsAt.Format(time.RFC3339))), telemetry.TokenEnded
	}
	if g.JKT == "" {
		return nil, nil, proto.Errf(proto.CodeAuth, again, "this sign-in's token is not for aries"), telemetry.TokenBearer
	}
	// The grant's key goes into the check, so that a proof by another
	// key is refused before it is remembered (issue #160).
	if err := a.proofs.Verify(r.Header.Get("DPoP"), http.MethodGet, a.at(keyproof.ConnectPath), tok, g.JKT, now); err != nil {
		e, _ := errors.AsType[*dpop.Error](err)
		switch e.Reason {
		case dpop.ReasonKey:
			return nil, nil, proto.Errf(proto.CodeAuth, again+"; the token and its key on this machine no longer match",
				"the DPoP proof is signed by another key than the sign-in's"), telemetry.DPoPKey
		case dpop.ReasonBusy:
			a.s.cfg.Telemetry.Count(telemetry.LimitRefusals, telemetry.LimitDPoPProofs)
			return nil, nil, proto.Errf(proto.CodeBusy, "retry in a minute", err.Error()), telemetry.DPoPBusy
		}
		return nil, nil, proto.Errf(proto.CodeAuth, again+"; the token and its key on this machine no longer match, or this machine's clock is off",
			err.Error()), "dpop_" + string(e.Reason)
	}
	return &proto.Frame{T: proto.FrameBridge, Principal: g.Principal},
		&signedIn{grant: g.ID, hash: hashOf(tok), identity: g.Identity, provider: g.Provider, scopes: g.Scopes, ends: g.EndsAt}, nil, ""
}

// heard reports whether r's address is still heard at the token endpoint
// (Limits.AuthFailureRate), as at /v1/connect, or else how long it must
// wait. Behind a proxy every request is the proxy's, and the limit
// does not apply.
func (a *authServer) heard(r *http.Request) (bool, time.Duration) {
	if a.s.cfg.HTTPS.TLS == nil {
		return true, 0
	}
	return a.s.authFails.has(peerAddr(r.RemoteAddr).String(), a.s.now())
}

// failed counts a refusal at the token endpoint against r's address, as
// a refused key proof is counted, unless behind a proxy.
func (a *authServer) failed(r *http.Request) {
	if a.s.cfg.HTTPS.TLS != nil {
		a.s.authFails.take(peerAddr(r.RemoteAddr).String(), a.s.now())
	}
}

// refused logs a refusal of the authorization server, sampled per
// address, with no token, code or address in it.
func (a *authServer) refused(r *http.Request, provider, reason string) {
	addr := peerAddr(r.RemoteAddr)
	a.s.refusals.line(r.Context(), a.s.cfg.Logger, "signin:"+addr.String(), a.s.now(), slog.LevelInfo, logs.SignInRefused,
		"sign-in refused", "reason", reason, "provider", provider, "addr", addr.String())
}

// refusedSignIn logs a sign-in refused after the provider, by its code.
func (a *authServer) refusedSignIn(ctx context.Context, provider string, e *proto.Error) {
	a.s.cfg.Logger.InfoContext(ctx, "sign-in refused", "event", logs.SignInRefused, "reason", string(e.Code), "provider", provider)
}

// providerFailed logs a provider that could not be reached or read.
func (a *authServer) providerFailed(ctx context.Context, p *oidc.Provider, err error) {
	a.s.refusals.line(ctx, a.s.cfg.Logger, "provider:"+p.Name(), a.s.now(), slog.LevelWarn, logs.SignInProviderFailed,
		"the provider could not be reached or read", "provider", p.Name(), "err", err)
}

// endGrant closes the connections of sign-in id, which ended for
// reason, each with a last frame that says so (decision 59).
func (s *Server) endGrant(ctx context.Context, id, reason string) {
	e := proto.Errf(proto.CodeAuth, proto.FixSignIn+" to sign in again", "your sign-in ended ("+strings.ReplaceAll(reason, "_", " ")+")")
	n := s.closeSessions(func(o owner) bool { return o.grant == id }, e)
	s.cfg.Logger.InfoContext(ctx, "sign-in ended", "event", logs.SignInEnded, "grant", id, "reason", reason, "connections", n)
}

// sweepSignIns closes the connections whose sign-in is no longer live
// in the control store's index, each with a last frame that says so, as
// the reaper does on each tick. A logout, signin.end or identity.remove
// closes them at once when its write succeeds; one whose write failed
// after it committed has ended the sign-in all the same, the index read
// again, and its connections close here.
func (s *Server) sweepSignIns(ctx context.Context) {
	ctl := s.cfg.Control
	if ctl == nil {
		return
	}
	now := ctl.Now()
	e := proto.Errf(proto.CodeAuth, proto.FixSignIn+" to sign in again", "your sign-in ended")
	n := s.closeSessions(func(o owner) bool {
		if o.grant == "" {
			return false
		}
		g, ok := ctl.TokenGrant(o.sess.signedIn.hash)
		return !ok || g.ID != o.grant || !g.Live(now)
	}, e)
	if n > 0 {
		s.cfg.Logger.InfoContext(ctx, "sign-in ended", "event", logs.SignInEnded, "reason", "swept", "connections", n)
	}
}

// wireSignIn is g on the wire, at now.
func wireSignIn(g store.Grant, now time.Time) proto.SignIn {
	out := proto.SignIn{ID: g.ID, Principal: g.Principal, Provider: g.Provider, Client: g.ClientID, Scopes: g.Scopes, Flow: g.Flow,
		Machine: g.Machine, State: proto.SignInEnded, CreatedAt: g.CreatedAt, ExpiresAt: g.EndsAt, EndReason: g.EndReason}
	if !g.RevokedAt.IsZero() {
		at := g.RevokedAt
		out.EndedAt = &at
	}
	if g.Live(now) {
		out.State = proto.SignInLive
	}
	return out
}

// signIns lists a principal's sign-ins: for a server admin, and for the
// principal itself. Anyone else is refused the same way whether or not
// the principal exists.
func signIns(ctx context.Context, s *Server, sess *session, in proto.SignInsArgs) (any, *proto.Error) {
	const op = proto.OpSignIns
	ctl, actor := s.cfg.Control, sess.actor.Principal
	admin := ctl.IsServerAdmin(actor)
	if !admin && actor != in.Name {
		return nil, proto.Errf(proto.CodeForbidden,
			"ask a server admin; `aries admin signin list "+actor+"` lists your own",
			fmt.Sprintf("%s may not list the sign-ins of %s: it is for the principal and for server admins", actor, in.Name))
	}
	gs, err := ctl.Grants(ctx, in.Name)
	if err == nil && admin && len(gs) == 0 {
		// A server admin is told when there is no such principal.
		_, err = ctl.Principal(ctx, in.Name)
	}
	if err != nil {
		return nil, s.ctlErr(ctx, sess, op, err)
	}
	now := ctl.Now()
	out := proto.SignInsResult{Principal: in.Name, SignIns: make([]proto.SignIn, 0, len(gs))}
	for _, g := range gs {
		out.SignIns = append(out.SignIns, wireSignIn(g, now))
	}
	return out, nil
}

// signInEnd ends a sign-in now, its holder's or, for a server admin,
// anyone's, and closes its connections (transport.md §6.7). The session
// that asks, if it is one of them, closes once it is answered.
func signInEnd(ctx context.Context, s *Server, sess *session, in proto.SignInEndArgs) (any, *proto.Error) {
	const op = proto.OpSignInEnd
	g, changed, err := s.cfg.Control.EndSignIn(ctx, sess.actor, in.ID, store.EndRevoked)
	if err != nil {
		return nil, s.ctlErr(ctx, sess, op, err)
	}
	out := proto.SignInEndResult{SignIn: wireSignIn(g, s.cfg.Control.Now())}
	if !changed {
		return out, nil
	}
	out.Change = "ended"
	e := proto.Errf(proto.CodeAuth, proto.FixSignIn+" to sign in again", "your sign-in ended (revoked)")
	out.Connections = s.closeSessions(func(o owner) bool { return o.grant == g.ID && o.sess != sess }, e)
	if in := sess.signedIn; in != nil && in.grant == g.ID {
		sess.ended = true
		out.Connections++
	}
	s.cfg.Logger.InfoContext(ctx, "sign-in ended", "event", logs.SignInEnded, "grant", g.ID, "reason", store.EndRevoked, "connections", out.Connections)
	return out, nil
}
