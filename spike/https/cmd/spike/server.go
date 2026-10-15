package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ariesworx/aries/spike/https/dpop"
	"github.com/ariesworx/aries/spike/https/provider"
	"github.com/ariesworx/aries/spike/identity/jose"
	"github.com/ariesworx/aries/spike/identity/oidc"
	"github.com/ariesworx/aries/spike/identity/trust"
)

// The stand-in ariesd: one HTTPS listener carrying the pipes (pipes.go),
// the authorization server that issues Aries's own tokens after a
// sign-in at the provider (this file), and the remote MCP endpoint
// (mcp.go). Its state is in memory; ariesd's would be in the control
// store (transport.md §5).

// Scopes of an Aries token (transport.md §5.5).
const (
	scopeCLI = "cli" // aries in a terminal: every op the principal may do, the session cli
	scopeMCP = "mcp" // an agent's tools only: no admin, no gate passing
	scopeCI  = "ci"  // a service principal's job
)

// builtinCLI is `aries`'s client id: a public client, loopback redirect
// on any port, DPoP required.
const builtinCLI = "aries-cli"

type grant struct {
	id        string // short, for the session and the log; not a secret
	principal string
	scopes    []string
	jkt       string // DPoP key thumbprint, or "" for a bearer grant
	resource  string // the audience: the server's base, or base+"/mcp"
	flow      string // loopback, workload, card
	end       time.Time
}

type token struct {
	g       *grant
	exp     time.Time
	refresh bool
}

type pending struct {
	clientID, redirect, challenge, state, resource string
	upVerifier, upNonce                            string
	until                                          time.Time
}

type issued struct {
	g                             *grant
	clientID, redirect, challenge string
	until                         time.Time
}

type client struct {
	redirects []string
	public    bool
}

type server struct {
	base     string   // https://aries.internal:8443
	publics  []string // base, and the URLs proxies present it at
	prov     *provider.Provider
	secret   string
	workload *oidc.Verifier
	rules    []trust.Rule
	invites  map[string]string // verified address → principal
	cards    map[string]string // certificate CN → principal
	crl      *x509.RevocationList
	day      time.Duration // a sign-in's life (decision 58)
	mcpLife  time.Duration // a connector's access token
	dp       *dpop.Verifier

	mu      sync.Mutex
	tokens  map[string]*token // sha256(token) → token
	pend    map[string]*pending
	codes   map[string]*issued
	clients map[string]*client
	live    map[string]map[*live]struct{} // grant id → open connections
}

// live is one open pipe connection, closed when its grant ends.
type live struct{ cancel context.CancelCauseFunc }

var errSignInEnded = errors.New("sign-in ended")

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8443", "listen address")
	base := fs.String("base", "https://aries.internal:8443", "the server's public URL")
	also := fs.String("also", "", "other public URLs the server is reached at (a proxy's), comma-separated")
	cert := fs.String("cert", "", "server certificate (PEM)")
	key := fs.String("key", "", "server key (PEM)")
	ca := fs.String("ca", "", "the internal CA: trusted for the provider and for client certificates")
	crlFile := fs.String("crl", "", "the CA's CRL, checked for client certificates")
	issuer := fs.String("issuer", "https://keycloak.internal:8443/realms/"+realm, "the provider's issuer")
	secretFile := fs.String("client-secret-file", "", "the server's client secret at the provider")
	jwksFile := fs.String("jwks-file", "", "the provider's key set, supplied by the operator; with it, no key is fetched")
	day := fs.Duration("day", 24*time.Hour, "a sign-in's life")
	mcpLife := fs.Duration("mcp-access", time.Hour, "a connector's access token's life")
	_ = fs.Parse(args)
	secret, err := readSecret(*secretFile)
	if err != nil {
		return err
	}
	pcfg := provider.Config{Name: "keycloak", Kind: provider.KindOIDC, Issuer: *issuer, ClientID: "aries-server", CAFile: *ca}
	if *jwksFile != "" {
		// No discovery and no key fetch: every endpoint is written in the
		// settings, as an air-gapped operator writes them.
		pcfg.JWKSFile = *jwksFile
		pcfg.AuthorizationEndpoint = *issuer + "/protocol/openid-connect/auth"
		pcfg.TokenEndpoint = *issuer + "/protocol/openid-connect/token"
	}
	prov, err := provider.Open(ctx, pcfg, time.Now)
	if err != nil {
		return err
	}
	// The workload "platform" is the same Keycloak, its client-credentials
	// tokens standing in for GitHub Actions' (identity.md §7): the same
	// issuer and keys, the server's own URL as the audience.
	vc := oidc.Config{Issuer: *issuer, Audience: *base, Skew: 30 * time.Second, MaxAge: 10 * time.Minute, Refetch: time.Minute}
	wv := oidc.New(vc, &prov.Endpoints, prov.Client, time.Now)
	if *jwksFile != "" {
		b, err := os.ReadFile(*jwksFile)
		if err != nil {
			return err
		}
		var set jose.Set
		if err := json.Unmarshal(b, &set); err != nil {
			return err
		}
		if wv, err = oidc.NewStatic(vc, set, time.Now); err != nil {
			return err
		}
	}
	s := &server{
		base: *base, publics: publicURLs(*base, *also), prov: prov, secret: secret, workload: wv,
		rules: []trust.Rule{{Name: "ci-example-main", Principal: "ci-example", Issuer: *issuer, Audience: *base,
			Claims: map[string]string{"repository_owner_id": "111111111", "repository_id": "123456789", "ref": "refs/heads/main"}}},
		invites: map[string]string{"alice@example.com": "alice"},
		cards:   map[string]string{"alice": "alice", "mallory": "mallory"},
		day:     *day, mcpLife: *mcpLife,
		dp:     dpop.NewVerifier(100000, 2*dpop.Skew),
		tokens: map[string]*token{}, pend: map[string]*pending{}, codes: map[string]*issued{},
		clients: map[string]*client{builtinCLI: {public: true}},
		live:    map[string]map[*live]struct{}{},
	}
	if *crlFile != "" {
		b, err := os.ReadFile(*crlFile)
		if err != nil {
			return err
		}
		blk, _ := pem.Decode(b)
		if blk == nil {
			return errors.New("crl: not PEM")
		}
		if s.crl, err = x509.ParseRevocationList(blk.Bytes); err != nil {
			return err
		}
	}
	cas, err := pool(*ca)
	if err != nil {
		return err
	}
	pair, err := tls.LoadX509KeyPair(*cert, *key)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	s.routes(mux)
	var protos http.Protocols
	protos.SetHTTP1(true)
	protos.SetHTTP2(true)
	hs := &http.Server{
		Addr:    *addr,
		Handler: mux,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12,
			ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: cas,
		},
		Protocols:         &protos,
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    16 << 10,
		IdleTimeout:       2 * time.Minute,
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	log.Printf("serve: listening on %s as %s; provider %s; keys from %s", *addr, *base, *issuer,
		map[bool]string{true: "a key set file (nothing fetched)", false: "the provider's key set URL"}[*jwksFile != ""])
	if err := hs.ServeTLS(ln, "", ""); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func publicURLs(base, also string) []string {
	out := []string{base}
	for u := range strings.SplitSeq(also, ",") {
		if u != "" {
			out = append(out, u)
		}
	}
	return out
}

func (s *server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.asMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", s.prm)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.prm)
	mux.HandleFunc("POST /oauth/register", s.register)
	mux.HandleFunc("GET /oauth/authorize", s.authorize)
	mux.HandleFunc("GET /auth/callback", s.callback)
	mux.HandleFunc("POST /oauth/token", s.token)
	mux.HandleFunc("POST /oauth/revoke", s.revoke)
	mux.HandleFunc("GET /v1/connect", s.connectWS)
	mux.HandleFunc("POST /v1/stream", s.connectStream)
	mux.HandleFunc("POST /v1/rpc", s.rpc)
	mux.HandleFunc("GET /v1/events", s.events)
	mux.Handle("/mcp", s.mcpHandler())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthErr(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

// asMetadata is RFC 8414's document. Aries is its own authorization
// server: its issuer is its own URL, and it signs people in by sending
// them on to the provider (transport.md §5.1).
func (s *server) asMetadata(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"issuer":                                         s.base,
		"authorization_endpoint":                         s.base + "/oauth/authorize",
		"token_endpoint":                                 s.base + "/oauth/token",
		"registration_endpoint":                          s.base + "/oauth/register",
		"revocation_endpoint":                            s.base + "/oauth/revoke",
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token", "urn:ietf:params:oauth:grant-type:token-exchange"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"dpop_signing_alg_values_supported":              []string{"ES256"},
		"scopes_supported":                               []string{scopeCLI, scopeMCP},
		"authorization_response_iss_parameter_supported": true,
	})
}

// prm is RFC 9728's protected resource metadata for /mcp.
func (s *server) prm(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"resource":                 s.base + "/mcp",
		"authorization_servers":    []string{s.base},
		"scopes_supported":         []string{scopeMCP},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "Aries (spike)",
	})
}

func loopback(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		return false
	}
	h := u.Hostname()
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

// register is dynamic client registration (RFC 7591), deprecated in
// MCP 2026-07-28 for Client ID Metadata Documents but what Claude falls
// back to when those are not advertised. Redirects are loopback, or
// https; at most 1,000 clients (rule 18).
func (s *server) register(w http.ResponseWriter, r *http.Request) {
	var m struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&m); err != nil || len(m.RedirectURIs) == 0 || len(m.RedirectURIs) > 8 {
		oauthErr(w, 400, "invalid_client_metadata", "redirect_uris: 1 to 8 are required")
		return
	}
	for _, u := range m.RedirectURIs {
		pu, err := url.Parse(u)
		if err != nil || !(loopback(u) || pu.Scheme == "https") || pu.Fragment != "" {
			oauthErr(w, 400, "invalid_redirect_uri", "a redirect must be loopback http or https")
			return
		}
	}
	id := "dcr-" + rand.Text()[:16]
	s.mu.Lock()
	if len(s.clients) > 1000 {
		s.mu.Unlock()
		oauthErr(w, 503, "temporarily_unavailable", "too many registered clients")
		return
	}
	s.clients[id] = &client{redirects: m.RedirectURIs, public: true}
	s.mu.Unlock()
	log.Printf("register: client %s (%q), %d redirects", id, m.ClientName, len(m.RedirectURIs))
	writeJSON(w, 201, map[string]any{"client_id": id, "redirect_uris": m.RedirectURIs, "token_endpoint_auth_method": "none",
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}})
}

func (s *server) redirectOK(c *client, clientID, redirect string) bool {
	if clientID == builtinCLI {
		return loopback(redirect)
	}
	for _, r := range c.redirects {
		if r == redirect {
			return true
		}
		// RFC 8252 §7.3: a loopback redirect matches on any port.
		if loopback(r) && loopback(redirect) {
			a, _ := url.Parse(r)
			b, _ := url.Parse(redirect)
			if a.Hostname() == b.Hostname() && a.Path == b.Path {
				return true
			}
		}
	}
	return false
}

// authorize begins a sign-in: it checks the client's request and sends
// the browser on to the provider, with the server's own PKCE, nonce and
// prompt=login (decision 85). Nothing of the client's reaches the
// provider.
func (s *server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clientID, redirect := q.Get("client_id"), q.Get("redirect_uri")
	s.mu.Lock()
	c := s.clients[clientID]
	s.mu.Unlock()
	if c == nil || !s.redirectOK(c, clientID, redirect) {
		// Never redirect to an unchecked address (RFC 9700 §4.11).
		http.Error(w, "unknown client or redirect", http.StatusBadRequest)
		return
	}
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43 {
		http.Error(w, "response_type=code and PKCE with S256 are required", http.StatusBadRequest)
		return
	}
	resource := q.Get("resource")
	if resource != "" && resource != s.base && resource != s.base+"/mcp" {
		http.Error(w, "unknown resource", http.StatusBadRequest)
		return
	}
	upState, upVerifier, upNonce := rand.Text(), rand.Text()+rand.Text(), rand.Text()
	s.mu.Lock()
	if len(s.pend) > 10000 {
		s.mu.Unlock()
		http.Error(w, "busy: too many sign-ins in progress; retry in a minute", http.StatusServiceUnavailable)
		return
	}
	s.pend[upState] = &pending{clientID: clientID, redirect: redirect, challenge: q.Get("code_challenge"), state: q.Get("state"),
		resource: resource, upVerifier: upVerifier, upNonce: upNonce, until: time.Now().Add(10 * time.Minute)}
	s.mu.Unlock()
	ch := sha256.Sum256([]byte(upVerifier))
	v := url.Values{
		"response_type": {"code"}, "client_id": {"aries-server"}, "redirect_uri": {s.base + "/auth/callback"},
		"scope": {"openid email profile"}, "state": {upState}, "nonce": {upNonce},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(ch[:])}, "code_challenge_method": {"S256"},
		"prompt": {"login"}, "max_age": {"600"},
	}
	http.Redirect(w, r, s.prov.Endpoints.AuthorizationEndpoint+"?"+v.Encode(), http.StatusFound)
}

// callback is where the provider sends the browser back. The server
// exchanges the code itself (decision 85), checks the ID token, finds the
// invited principal, and sends the browser on to the client with an
// Aries code.
func (s *server) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.mu.Lock()
	p := s.pend[q.Get("state")]
	delete(s.pend, q.Get("state"))
	s.mu.Unlock()
	if p == nil || time.Now().After(p.until) {
		http.Error(w, "this sign-in is unknown or has expired; run aries login again", http.StatusBadRequest)
		return
	}
	if e := q.Get("error"); e != "" {
		http.Error(w, "the provider refused the sign-in: "+e, http.StatusForbidden)
		return
	}
	var tr struct {
		IDToken string `json:"id_token"`
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {q.Get("code")}, "redirect_uri": {s.base + "/auth/callback"},
		"client_id": {"aries-server"}, "client_secret": {s.secret}, "code_verifier": {p.upVerifier}}
	if err := postForm(r.Context(), s.prov.Client, s.prov.Endpoints.TokenEndpoint, form, nil, &tr); err != nil {
		http.Error(w, "the code exchange with the provider failed", http.StatusBadGateway)
		log.Printf("callback: code exchange: %v", err)
		return
	}
	id, err := s.prov.Check(r.Context(), tr.IDToken, oidc.Expect{Nonce: p.upNonce, AuthAge: 10 * time.Minute})
	if err != nil {
		http.Error(w, "the sign-in could not be verified", http.StatusForbidden)
		log.Printf("callback: id token refused: %v", err)
		return
	}
	email, _ := id.Claims.All["email"].(string)
	verified, _ := id.Claims.All["email_verified"].(bool)
	principal := s.invites[strings.ToLower(email)]
	if !verified || principal == "" {
		http.Error(w, "no invitation for this account; ask an admin of your project to invite it", http.StatusForbidden)
		log.Printf("callback: refused %s: no invitation", id.Subject)
		return
	}
	scopes, resource := []string{scopeMCP}, s.base+"/mcp"
	if p.clientID == builtinCLI {
		scopes, resource = []string{scopeCLI, scopeMCP}, s.base
	}
	g := &grant{id: rand.Text()[:10], principal: principal, scopes: scopes, resource: resource, flow: "loopback", end: time.Now().Add(s.day)}
	code := rand.Text()
	s.mu.Lock()
	s.codes[hash(code)] = &issued{g: g, clientID: p.clientID, redirect: p.redirect, challenge: p.challenge, until: time.Now().Add(time.Minute)}
	s.mu.Unlock()
	log.Printf("callback: %s signed in (issuer %s, subject %s), grant %s, for %s, ends %s", principal, id.Issuer, id.Subject, g.id, p.clientID, g.end.UTC().Format(time.RFC3339))
	v := url.Values{"code": {code}, "state": {p.state}, "iss": {s.base}}
	http.Redirect(w, r, p.redirect+"?"+v.Encode(), http.StatusFound)
}

// token is the token endpoint: the code exchanged for an Aries token,
// a refresh, or a workload's token exchanged (RFC 8693).
func (s *server) token(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthErr(w, 400, "invalid_request", "form")
		return
	}
	jkt := ""
	if proof := r.Header.Get("DPoP"); proof != "" {
		var err error
		if jkt, err = s.dp.Verify(proof, http.MethodPost, s.base+"/oauth/token", "", time.Now()); err != nil {
			oauthErr(w, 400, "invalid_dpop_proof", err.Error())
			return
		}
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.exchangeCode(w, r, jkt)
	case "refresh_token":
		s.refresh(w, r, jkt)
	case "urn:ietf:params:oauth:grant-type:token-exchange":
		s.exchangeWorkload(w, r, jkt)
	default:
		oauthErr(w, 400, "unsupported_grant_type", "")
	}
}

func (s *server) exchangeCode(w http.ResponseWriter, r *http.Request, jkt string) {
	f := r.PostForm
	s.mu.Lock()
	c := s.codes[hash(f.Get("code"))]
	delete(s.codes, hash(f.Get("code"))) // single use
	s.mu.Unlock()
	if c == nil || time.Now().After(c.until) || c.clientID != f.Get("client_id") || c.redirect != f.Get("redirect_uri") {
		oauthErr(w, 400, "invalid_grant", "code unknown, used, expired, or for another client or redirect")
		return
	}
	ch := sha256.Sum256([]byte(f.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(ch[:]) != c.challenge {
		oauthErr(w, 400, "invalid_grant", "PKCE verification failed")
		return
	}
	g := c.g
	if c.clientID == builtinCLI {
		if jkt == "" {
			oauthErr(w, 400, "invalid_dpop_proof", "aries's tokens are bound to its key: send a DPoP proof")
			return
		}
		g.jkt = jkt
		// One access token for the day, bound to the key; no refresh
		// token, so nothing renews the day (decision 58).
		s.issue(w, g, g.end, false)
		return
	}
	g.jkt = jkt // a connector that offers DPoP gets a bound token; Claude's do not (transport.md §7)
	s.issue(w, g, minTime(time.Now().Add(s.mcpLife), g.end), true)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (s *server) issue(w http.ResponseWriter, g *grant, exp time.Time, withRefresh bool) {
	at := rand.Text() + rand.Text()
	out := map[string]any{"access_token": at, "token_type": map[bool]string{true: "DPoP", false: "Bearer"}[g.jkt != ""],
		"expires_in": int(time.Until(exp).Seconds()), "scope": strings.Join(g.scopes, " ")}
	s.mu.Lock()
	s.tokens[hash(at)] = &token{g: g, exp: exp}
	if withRefresh {
		rt := rand.Text() + rand.Text()
		// The refresh token rotates and never outlives the sign-in: a
		// connector stays signed in for the day, not past it.
		s.tokens[hash(rt)] = &token{g: g, exp: g.end, refresh: true}
		out["refresh_token"] = rt
	}
	s.mu.Unlock()
	writeJSON(w, 200, out)
}

func (s *server) refresh(w http.ResponseWriter, r *http.Request, jkt string) {
	h := hash(r.PostForm.Get("refresh_token"))
	s.mu.Lock()
	t := s.tokens[h]
	delete(s.tokens, h)
	s.mu.Unlock()
	if t == nil || !t.refresh || time.Now().After(t.exp) || (t.g.jkt != "" && t.g.jkt != jkt) {
		oauthErr(w, 400, "invalid_grant", "refresh token unknown, used, or past the end of the sign-in")
		return
	}
	log.Printf("refresh: grant %s of %s renewed for an hour, within its end %s", t.g.id, t.g.principal, t.g.end.UTC().Format(time.RFC3339))
	s.issue(w, t.g, minTime(time.Now().Add(s.mcpLife), t.g.end), true)
}

// exchangeWorkload takes a workload platform's token (RFC 8693) and, if
// one trust rule matches, gives the rule's service principal a token for
// an hour (identity.md §7, carried over).
func (s *server) exchangeWorkload(w http.ResponseWriter, r *http.Request, jkt string) {
	f := r.PostForm
	if f.Get("subject_token_type") != "urn:ietf:params:oauth:token-type:jwt" && f.Get("subject_token_type") != "urn:ietf:params:oauth:token-type:id_token" {
		oauthErr(w, 400, "invalid_request", "subject_token_type must be jwt or id_token")
		return
	}
	c, err := s.workload.Verify(r.Context(), f.Get("subject_token"), oidc.Expect{})
	if err != nil {
		oauthErr(w, 400, "invalid_grant", "the workload token was refused: "+err.Error())
		return
	}
	principal, rule, err := trust.Match(s.rules, c.Iss, c.Aud, c.All)
	if err != nil {
		oauthErr(w, 403, "access_denied", err.Error())
		return
	}
	g := &grant{id: rand.Text()[:10], principal: principal, scopes: []string{scopeCI}, jkt: jkt, resource: s.base, flow: "workload", end: time.Now().Add(time.Hour)}
	log.Printf("token-exchange: %s by rule %s (run %v), grant %s", principal, rule, c.All["run_id"], g.id)
	s.issue(w, g, g.end, false)
}

// revoke ends a token's grant, and closes its connections (decision 59).
func (s *server) revoke(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	s.mu.Lock()
	t := s.tokens[hash(r.PostForm.Get("token"))]
	s.mu.Unlock()
	if t != nil {
		s.endGrant(t.g, "revoked")
	}
	w.WriteHeader(200) // RFC 7009: 200 whether or not the token was known
}

func (s *server) endGrant(g *grant, why string) {
	s.mu.Lock()
	for k, t := range s.tokens {
		if t.g == g {
			delete(s.tokens, k)
		}
	}
	conns := s.live[g.id]
	delete(s.live, g.id)
	s.mu.Unlock()
	for l := range conns {
		l.cancel(fmt.Errorf("%w: %s", errSignInEnded, why))
	}
	log.Printf("grant %s of %s ended (%s), %d connections closed", g.id, g.principal, why, len(conns))
}

// authErr is a refusal at a resource, with what RFC 6750 and RFC 9449
// say to put in WWW-Authenticate.
type authErr struct {
	status int
	msg    string
}

func (e *authErr) Error() string { return e.msg }

func (s *server) refuse(w http.ResponseWriter, e *authErr, mcp bool) {
	if mcp {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource/mcp", scope="mcp", error="invalid_token"`, s.base))
	} else {
		w.Header().Set("WWW-Authenticate", `DPoP algs="ES256", error="invalid_token"`)
	}
	http.Error(w, e.msg, e.status)
}

// authenticate finds the grant behind a request to the pipes: a DPoP-bound
// token with its proof, a bearer token for a grant with no key, or a
// client certificate from the internal CA that the CRL does not revoke
// (a smart card, transport.md §8.3).
func (s *server) authenticate(r *http.Request, resource string) (*grant, *authErr) {
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 && r.Header.Get("Authorization") == "" {
		return s.card(r.TLS.PeerCertificates[0])
	}
	scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || tok == "" {
		return nil, &authErr{401, "sign in first: run aries login"}
	}
	s.mu.Lock()
	t := s.tokens[hash(tok)]
	s.mu.Unlock()
	switch {
	case t == nil || t.refresh:
		return nil, &authErr{401, "the token is unknown or was revoked: run aries login"}
	case time.Now().After(t.exp):
		return nil, &authErr{401, "the token expired: run aries login"}
	case time.Now().After(t.g.end):
		return nil, &authErr{401, "your sign-in ended: run aries login"}
	case t.g.resource != resource:
		return nil, &authErr{401, "the token is for another resource"}
	}
	if t.g.jkt == "" {
		if !strings.EqualFold(scheme, "Bearer") {
			return nil, &authErr{401, "a bearer grant is presented as Bearer"}
		}
		return t.g, nil
	}
	if !strings.EqualFold(scheme, "DPoP") {
		return nil, &authErr{401, "this token is bound to a key: present it with a DPoP proof"}
	}
	// htu is held to the server's configured public URLs, never to the
	// Host header, which a proxy may rewrite (nginx does, on a location
	// that sets any header of its own: RESULTS.md, section 3).
	var jkt string
	var errs []error
	for _, pub := range s.publics {
		var err error
		if jkt, err = s.dp.Verify(r.Header.Get("DPoP"), r.Method, pub+r.URL.Path, tok, time.Now()); err == nil {
			errs = nil
			break
		}
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		// The first URL's refusal: the others' only add that htu differs.
		return nil, &authErr{401, errs[0].Error()}
	}
	if jkt != t.g.jkt {
		return nil, &authErr{401, "the DPoP proof is signed by another key than the token is bound to"}
	}
	return t.g, nil
}

func (s *server) card(c *x509.Certificate) (*grant, *authErr) {
	if s.crl != nil && slices.ContainsFunc(s.crl.RevokedCertificateEntries, func(e x509.RevocationListEntry) bool {
		return e.SerialNumber.Cmp(c.SerialNumber) == 0
	}) {
		return nil, &authErr{401, "this certificate is revoked"}
	}
	p := s.cards[c.Subject.CommonName]
	if p == "" {
		return nil, &authErr{403, "no principal for this certificate"}
	}
	return &grant{id: "card-" + hash(string(c.Raw))[:8], principal: p, scopes: []string{scopeCLI, scopeMCP}, resource: s.base, flow: "card", end: c.NotAfter}, nil
}

// watch registers a pipe connection under its grant, and ends it at the
// grant's end or when the grant is revoked.
func (s *server) watch(ctx context.Context, g *grant) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	l := &live{cancel: cancel}
	s.mu.Lock()
	if s.live[g.id] == nil {
		s.live[g.id] = map[*live]struct{}{}
	}
	s.live[g.id][l] = struct{}{}
	s.mu.Unlock()
	timer := time.AfterFunc(time.Until(g.end), func() { cancel(fmt.Errorf("%w at %s", errSignInEnded, g.end.UTC().Format(time.RFC3339))) })
	return ctx, func() {
		timer.Stop()
		s.mu.Lock()
		delete(s.live[g.id], l)
		s.mu.Unlock()
		cancel(nil)
	}
}
