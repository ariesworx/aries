// Package oidctest is a stand-in OpenID Connect provider for tests, in
// process and on loopback: a discovery document, a key set, an
// authorization endpoint that signs in whoever the test says without a
// page, and a token endpoint that checks the client's secret and PKCE
// and returns an ID token signed ES256. It is what the verifier's tests,
// the server's sign-in tests and the end-to-end sign-in run against;
// Keycloak, in the Docker test server, is the real one (docs/deploy/docker.md).
package oidctest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/jose"
)

// ClientID and Secret are the server's registration at the stand-in.
const (
	ClientID = "aries-server"
	Secret   = "stand-in-client-secret"
)

// Person is who the authorization endpoint signs in.
type Person struct {
	Subject  string
	Email    string
	Verified bool
	// Extra claims, such as hd or tid.
	Extra map[string]any
}

// Issuer is a running stand-in. Its fields may be changed between
// sign-ins, under the test's own ordering.
type Issuer struct {
	// URL is the issuer: https://127.0.0.1:PORT.
	URL string
	// CAFile is a PEM file of the certificate the issuer serves, in the
	// test's temporary directory, for a provider's ca_file.
	CAFile string
	// Discovery, Keys and Tokens count the requests each endpoint took.
	Discovery, Keys, Tokens atomic.Int64

	srv *httptest.Server
	key *ecdsa.PrivateKey
	kid string

	mu sync.Mutex
	// person is who the next authorization signs in.
	person Person
	// authAge is how long ago the person last authenticated, for
	// auth_time.
	authAge time.Duration
	// now is the issuer's clock.
	now   func() time.Time
	codes map[string]grant
	// answerIss is the iss its authorization responses carry, or none.
	answerIss string
}

type grant struct {
	nonce, challenge, redirect string
	person                     Person
	authTime                   time.Time
}

// Option changes a stand-in New starts.
type Option func(*options)

type options struct{ path string }

// Path serves the issuer under path, as Entra's is under its tenant and
// version: https://127.0.0.1:PORT/PATH.
func Path(path string) Option { return func(o *options) { o.path = path } }

// New starts a stand-in that signs alice@example.com in, and stops it
// when the test ends.
func New(t testing.TB, opts ...Option) *Issuer {
	t.Helper()
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	i := &Issuer{key: key, kid: "stand-in-1", codes: map[string]grant{}, now: time.Now,
		person: Person{Subject: "00000000-0000-4000-8000-00000000a11c", Email: "alice@example.com", Verified: true}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+o.path+"/.well-known/openid-configuration", i.discovery)
	mux.HandleFunc("GET "+o.path+"/keys", i.keys)
	mux.HandleFunc("GET "+o.path+"/authorize", i.authorize)
	mux.HandleFunc("POST "+o.path+"/token", i.token)
	i.srv = httptest.NewUnstartedServer(mux)
	// A test that refuses the certificate makes a handshake fail on
	// purpose; that is not for the test's output.
	i.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	i.srv.StartTLS()
	t.Cleanup(i.srv.Close)
	i.URL = i.srv.URL + o.path
	i.CAFile = filepath.Join(t.TempDir(), "issuer-ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: i.srv.Certificate().Raw})
	if err := os.WriteFile(i.CAFile, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return i
}

// SignIn sets who the next authorizations sign in.
func (i *Issuer) SignIn(p Person) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.person = p
}

// AuthAge sets how long before each token the person last
// authenticated: its auth_time.
func (i *Issuer) AuthAge(d time.Duration) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.authAge = d
}

// AnswerIssuer sets the iss the authorization endpoint's redirects carry
// (RFC 9207): the issuer's URL, as a provider that supports it sends, or
// another, as a provider in a mix-up would. Empty, the default, sends
// none.
func (i *Issuer) AnswerIssuer(iss string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.answerIss = iss
}

// Clock sets the issuer's clock, for iat, exp and auth_time.
func (i *Issuer) Clock(now func() time.Time) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.now = now
}

// Client trusts the issuer's certificate, for a test's browser.
func (i *Issuer) Client() *http.Client { return i.srv.Client() }

// JWKS is the issuer's key set, as a file of it holds it.
func (i *Issuer) JWKS() []byte { return i.JWKSAs(i.kid) }

// JWKSAs is the issuer's key set with its key named kid: the set a
// provider publishes after a rotation, when its new key is this one.
func (i *Issuer) JWKSAs(kid string) []byte {
	j, err := jose.PublicJWK(&i.key.PublicKey, kid)
	if err != nil {
		panic(err) // a P-256 key always has a JWK
	}
	j.Use, j.Alg = "sig", jose.AlgES256
	b, _ := json.Marshal(jose.Set{Keys: []jose.JWK{j}})
	return b
}

// Sign signs claims as the issuer does an ID token.
func (i *Issuer) Sign(claims map[string]any) string { return i.SignAs(i.kid, claims) }

// SignAs signs claims with the issuer's key, naming it kid.
func (i *Issuer) SignAs(kid string, claims map[string]any) string {
	tok, err := jose.Sign(i.key, jose.Header{Kid: kid, Typ: "JWT"}, claims)
	if err != nil {
		panic(err) // the key is a P-256 key, and the claims a map
	}
	return tok
}

// IDToken is the ID token the issuer gives for p, with nonce, at its
// clock's now.
func (i *Issuer) IDToken(p Person, nonce string) string {
	i.mu.Lock()
	now, age := i.now(), i.authAge
	i.mu.Unlock()
	return i.Sign(i.claims(p, nonce, now, now.Add(-age)))
}

func (i *Issuer) claims(p Person, nonce string, now, authTime time.Time) map[string]any {
	c := map[string]any{
		"iss": i.URL, "sub": p.Subject, "aud": ClientID, "azp": ClientID,
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(), "auth_time": authTime.Unix(),
		"email": p.Email, "email_verified": p.Verified,
	}
	if nonce != "" {
		c["nonce"] = nonce
	}
	for k, v := range p.Extra {
		c[k] = v
	}
	return c
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (i *Issuer) discovery(w http.ResponseWriter, _ *http.Request) {
	i.Discovery.Add(1)
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer": i.URL, "authorization_endpoint": i.URL + "/authorize", "token_endpoint": i.URL + "/token",
		"jwks_uri": i.URL + "/keys", "code_challenge_methods_supported": []string{"S256"},
	})
}

func (i *Issuer) keys(w http.ResponseWriter, _ *http.Request) {
	i.Keys.Add(1)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(i.JWKS())
}

// authorize signs the configured person in at once, as a provider with
// a session open would after prompt=login's page, and redirects back.
func (i *Issuer) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != ClientID || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	code := rand.Text()
	i.mu.Lock()
	i.codes[code] = grant{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri"),
		person: i.person, authTime: i.now().Add(-i.authAge)}
	answerIss := i.answerIss
	i.mu.Unlock()
	v := url.Values{"code": {code}, "state": {q.Get("state")}}
	if answerIss != "" {
		v.Set("iss", answerIss)
	}
	http.Redirect(w, r, q.Get("redirect_uri")+"?"+v.Encode(), http.StatusFound) //nolint:gosec // a test stand-in, redirecting where its test asks
}

// token exchanges a code once, for the client with its secret and the
// PKCE verifier the authorization's challenge was made from.
func (i *Issuer) token(w http.ResponseWriter, r *http.Request) {
	i.Tokens.Add(1)
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	f := r.PostForm
	i.mu.Lock()
	g, ok := i.codes[f.Get("code")]
	delete(i.codes, f.Get("code"))
	now := i.now()
	i.mu.Unlock()
	sum := sha256.Sum256([]byte(f.Get("code_verifier")))
	switch {
	case f.Get("client_id") != ClientID || f.Get("client_secret") != Secret:
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
	case !ok || f.Get("grant_type") != "authorization_code" || f.Get("redirect_uri") != g.redirect:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": "code not valid"})
	case base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": "PKCE verification failed"})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"id_token": i.Sign(i.claims(g.person, g.nonce, now, g.authTime)),
			"access_token": rand.Text(), "token_type": "Bearer", "expires_in": 300})
	}
}
