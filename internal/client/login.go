package client

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/dpop"
	"github.com/ariesworx/aries/internal/keyproof"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/safetext"
)

// `aries login` by the server's own authorization server (transport.md
// §6.2): aries sends the browser to the server's /oauth/authorize, with
// its own PKCE challenge and state and a loopback redirect; the server
// signs the person in at the provider and redirects the browser back to
// the loopback address with a code of its own; aries exchanges the code,
// with its verifier and a DPoP proof by a key it has just made, for a
// token bound to that key. The provider never sees aries.

// ClientID is aries's client id at the server's authorization server.
const ClientID = "aries-cli"

// The scopes `aries login` asks for: every op of a person at a
// terminal, and the agent's tools of `aries mcp` (transport.md §6.8).
var loginScopes = []string{"cli", "mcp"}

// loginTimeout bounds each request of a sign-in to the server.
const loginTimeout = 15 * time.Second

// maxAnswer bounds what aries reads of the server's answers here.
const maxAnswer = 64 << 10

// ServerInfo is what /.well-known/aries says before anyone signs in.
type ServerInfo struct {
	Version    string         `json:"version"`
	Transports []string       `json:"transports"`
	Auth       []string       `json:"auth"`
	Providers  []ProviderInfo `json:"providers"`
	Workloads  []WorkloadInfo `json:"workloads"`
}

// ProviderInfo names a provider a person may sign in at.
type ProviderInfo struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// httpClient reaches the server named in cfg: the system's roots and
// server.ca, TLS 1.2 at least, no redirect followed, and no proxy, as the
// WebSocket has none.
func httpClient(cfg *Config) (*http.Client, error) {
	roots, err := cfg.Roots()
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	tr.Proxy = nil
	return &http.Client{Transport: tr, Timeout: loginTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}, nil
}

// serverURL is cfg's server URL as the server writes its own.
func serverURL(cfg *Config) (string, error) {
	if cfg.Server.URL == "" {
		return "", proto.Errf(proto.CodeInvalid, "set server.url in "+ConfigFile+" to the server's https:// URL; sign-in is over HTTPS",
			ConfigFile+" names no server.url")
	}
	u, err := keyproof.NormalizeURL(cfg.Server.URL)
	if err != nil {
		return "", proto.Errf(proto.CodeInvalid, "set server.url in "+ConfigFile+" to the server's https:// URL", "server.url: "+err.Error())
	}
	return u, nil
}

// FetchServerInfo reads the server's /.well-known/aries.
func FetchServerInfo(ctx context.Context, cfg *Config) (*ServerInfo, error) {
	base, err := serverURL(cfg)
	if err != nil {
		return nil, err
	}
	c, err := httpClient(cfg)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/.well-known/aries", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, unreached(base, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	var info ServerInfo
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &info) != nil {
		return nil, proto.Errf(proto.CodeUnavailable, "check server.url in "+ConfigFile+"; if it is right, ask the server's operator to check ariesd",
			fmt.Sprintf("%s did not answer as an Aries server (%s)", base, resp.Status))
	}
	return &info, nil
}

// unreached words a request that did not reach the server.
func unreached(base string, err error) error {
	return proto.Errf(proto.CodeUnavailable, "check server.url in "+ConfigFile+", and your network", fmt.Sprintf("cannot reach %s: %v", base, err))
}

// Login is one sign-in in progress: what aries keeps between sending
// the browser off and the browser coming back.
type Login struct {
	// URL is where the browser goes: the server's authorization
	// endpoint. It carries the PKCE challenge, never the verifier.
	URL string

	cfg      *Config
	base     string
	provider string
	redirect string
	state    string
	verifier string
}

// StartLogin begins a sign-in at the provider of cfg's server named
// provider, whose browser comes back to redirect, a loopback address
// this aries listens on.
func StartLogin(cfg *Config, provider, redirect string) (*Login, error) {
	base, err := serverURL(cfg)
	if err != nil {
		return nil, err
	}
	l := &Login{cfg: cfg, base: base, provider: provider, redirect: redirect, state: rand.Text(), verifier: rand.Text() + rand.Text()}
	sum := sha256.Sum256([]byte(l.verifier))
	v := url.Values{
		"response_type": {"code"}, "client_id": {ClientID}, "redirect_uri": {redirect}, "state": {l.state},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
		"scope": {strings.Join(loginScopes, " ")}, "resource": {base}, "provider": {provider},
	}
	l.URL = base + "/oauth/authorize?" + v.Encode()
	return l, nil
}

// Finish takes the address the browser came back to, checks that it is
// this sign-in's and this server's, and exchanges its code, with a proof
// by a new DPoP key, for the sign-in, which the caller keeps
// (SaveSignIn). machine names this machine to the server, as a hello
// does, and flow how the browser came back: loopback, or url, its
// address pasted.
func (l *Login) Finish(ctx context.Context, address, machine, flow string) (*SignIn, error) {
	q, err := l.callback(address)
	if err != nil {
		return nil, err
	}
	key, err := dpop.NewKey()
	if err != nil {
		return nil, err
	}
	tokenURL := l.base + "/oauth/token"
	proof, err := dpop.Proof(key, http.MethodPost, tokenURL, "", time.Now())
	if err != nil {
		return nil, err
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {q.Get("code")}, "redirect_uri": {l.redirect},
		"client_id": {ClientID}, "code_verifier": {l.verifier}, "machine": {machine}, "flow": {flow}}
	var answer struct {
		AccessToken string    `json:"access_token"`
		TokenType   string    `json:"token_type"`
		ExpiresAt   time.Time `json:"expires_at"`
		Scope       string    `json:"scope"`
		Principal   string    `json:"principal"`
		Provider    string    `json:"provider"`
		Grant       string    `json:"grant"`
	}
	if err := l.post(ctx, tokenURL, form, proof, &answer); err != nil {
		return nil, err
	}
	if answer.AccessToken == "" || !strings.EqualFold(answer.TokenType, "DPoP") || answer.ExpiresAt.IsZero() {
		return nil, proto.Errf(proto.CodeVersion, proto.FixUpgrade+" aries to match the server", "the server's answer to the sign-in is not one this aries reads")
	}
	return &SignIn{Server: l.cfg.Server.URL, Token: answer.AccessToken, ExpiresAt: answer.ExpiresAt.UTC(), Scopes: strings.Fields(answer.Scope),
		Principal: answer.Principal, Provider: answer.Provider, Grant: answer.Grant, Key: key}, nil
}

// callback checks address, the loopback redirect the browser came back
// to, and returns its query: this sign-in's state, this server as the
// issuer (RFC 9207, against a mix-up of servers), and a code; or the
// refusal it carries.
func (l *Login) callback(address string) (url.Values, error) {
	again := "run `aries login` again, and paste the address the browser shows once it has signed you in"
	u, err := url.Parse(strings.TrimSpace(address))
	if err != nil {
		return nil, proto.Errf(proto.CodeInvalid, again, "what was given is not an address")
	}
	want, _ := url.Parse(l.redirect)
	if u.Scheme != want.Scheme || u.Host != want.Host || u.Path != want.Path {
		return nil, proto.Errf(proto.CodeInvalid, again, fmt.Sprintf("the address is not %s, where this sign-in returns", l.redirect))
	}
	q := u.Query()
	if q.Get("state") != l.state {
		return nil, proto.Errf(proto.CodeInvalid, "paste the address from this sign-in's browser tab, or run `aries login` again",
			"the address is from another sign-in")
	}
	switch iss := q.Get("iss"); {
	case iss == "":
		return nil, proto.Errf(proto.CodeAuth, again, "the address names no server it came from (iss)")
	case iss != l.base:
		return nil, proto.Errf(proto.CodeAuth, "do not use this address; run `aries login` again, and tell the server's admin if it recurs",
			fmt.Sprintf("the address says it came from %s, not %s", safetext.Line(iss), l.base))
	}
	if e := q.Get("error"); e != "" {
		msg := q.Get("error_description")
		if msg == "" {
			msg = "the server refused the sign-in: " + e
		}
		return nil, proto.Errf(proto.CodeForbidden, again, msg)
	}
	if q.Get("code") == "" {
		return nil, proto.Errf(proto.CodeInvalid, again, "the address carries no code")
	}
	return q, nil
}

// post sends form to u with a DPoP proof, and decodes a 200's JSON into
// out. A refusal of the server's, in OAuth's form with Aries's code and
// fix beside it, comes back as a *proto.Error.
func (l *Login) post(ctx context.Context, u string, form url.Values, proof string, out any) error {
	return postForm(ctx, l.cfg, l.base, u, form, proof, out)
}

func postForm(ctx context.Context, cfg *Config, base, u string, form url.Values, proof string, out any) error {
	c, err := httpClient(cfg)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if proof != "" {
		req.Header.Set("DPoP", proof)
	}
	resp, err := c.Do(req)
	if err != nil {
		return unreached(base, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if resp.StatusCode == http.StatusOK {
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(b, out); err != nil {
			return proto.Errf(proto.CodeVersion, proto.FixUpgrade+" aries to match the server", "the server's answer is not one this aries reads")
		}
		return nil
	}
	return oauthRefusal(base, resp.Status, b)
}

// oauthRefusal is a refusal of the server's authorization endpoints: an
// OAuth error, with the protocol's code and fix beside it.
func oauthRefusal(base, status string, b []byte) error {
	var e struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
		Code        string `json:"code"`
		Fix         string `json:"fix"`
	}
	if json.Unmarshal(b, &e) != nil || e.Error == "" {
		return proto.Errf(proto.CodeUnavailable, "retry; if it persists, ask the server's operator to check ariesd and any proxy in front of it",
			fmt.Sprintf("%s answered %s", base, status))
	}
	code := proto.Code(e.Code)
	if code == "" {
		code = proto.CodeAuth
	}
	msg := cmp.Or(e.Description, "the server refused the sign-in: "+e.Error)
	fix := cmp.Or(e.Fix, "run `aries login` again")
	return &proto.Error{Code: code, Message: msg, Fix: fix}
}

// Logout asks cfg's server to end s, and its connections, at once. The
// caller removes it from this machine whatever the answer
// (RemoveSignIn).
func Logout(ctx context.Context, cfg *Config, s *SignIn) error {
	base, err := serverURL(cfg)
	if err != nil {
		return err
	}
	return postForm(ctx, cfg, base, base+"/oauth/revoke", url.Values{"token": {s.Token}, "client_id": {ClientID}}, "", nil)
}
