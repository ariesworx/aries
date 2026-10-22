package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ariesworx/aries/internal/lazyregexp"
)

// Kinds of provider (transport.md §8.1). GitHub's adapter, which has no
// ID token, is slice N6's.
const (
	// KindOIDC is any OpenID Connect provider: Keycloak, AD FS, Okta,
	// Google Workspace.
	KindOIDC = "oidc"
	// KindEntra is Microsoft Entra ID, in any cloud, with its authority
	// configured and no default.
	KindEntra = "entra"
)

// The ways a provider's endpoints and keys are found (transport.md
// §8.2).
const (
	// DiscoveryFetch reads the discovery document and the key set from
	// the issuer's own URLs.
	DiscoveryFetch = "fetch"
	// DiscoveryStatic discovers nothing: every endpoint is in the
	// settings, and the keys come from jwks_file, with nothing fetched,
	// or from jwks_url.
	DiscoveryStatic = "static"
)

// ErrNotAllowed refuses a token that verifies but whose person the
// provider's settings do not let in: another domain or tenant, or a
// required claim with another value.
var ErrNotAllowed = errors.New("the provider's settings do not let this account in")

// Settings are one provider's entry under login: providers: in ariesd's
// config. A secret is a path to a 0600 file, never a value (rule 2).
type Settings struct {
	// Name is how invitations and `aries login --provider` name it: a
	// lowercase letter, then up to 31 lowercase letters, digits and
	// hyphens.
	Name string `yaml:"name"`
	// Kind is KindOIDC or KindEntra.
	Kind string `yaml:"kind"`
	// Issuer is the exact iss of its tokens. Required for oidc, and for
	// entra with TokenVersion 1; derived for entra version 2.
	Issuer string `yaml:"issuer"`
	// ClientID is the server's registration at the provider: the
	// audience of its ID tokens.
	ClientID string `yaml:"client_id"`
	// ClientSecretFile holds the registration's secret, mode 0600. Empty
	// exchanges a code as a public client, with PKCE alone.
	ClientSecretFile string `yaml:"client_secret_file"`
	// Authority, Tenant and TokenVersion are Entra's: the cloud's sign-in
	// host (no default), the tenant's id, and the version of token the
	// app registration issues (1 or 2).
	Authority    string `yaml:"authority"`
	Tenant       string `yaml:"tenant"`
	TokenVersion int    `yaml:"token_version"`
	// Discovery is DiscoveryFetch (the default) or DiscoveryStatic.
	Discovery string `yaml:"discovery"`
	// AuthorizationEndpoint and TokenEndpoint are required with
	// DiscoveryStatic, and refused without it.
	AuthorizationEndpoint string `yaml:"authorization_endpoint"`
	TokenEndpoint         string `yaml:"token_endpoint"`
	// JWKSURL or JWKSFile, exactly one, with DiscoveryStatic: where the
	// signing keys are fetched from, or the file they are read from with
	// nothing fetched.
	JWKSURL  string `yaml:"jwks_url"`
	JWKSFile string `yaml:"jwks_file"`
	// CAFile holds the certificate authorities trusted for the
	// provider's endpoints, in place of the system's, for a provider
	// whose certificate is from an internal authority.
	CAFile string `yaml:"ca_file"`
	// Require are claims a token must carry with exactly these values:
	// hd for a Google Workspace domain, for instance.
	Require map[string]string `yaml:"require"`
	// Domains, when set, are the domains a token's address must be at,
	// an address the provider says is verified.
	Domains []string `yaml:"domains"`
	// PromptLogin sends prompt=login, so that the provider asks the
	// person to sign in again whatever session the browser holds. The
	// default is true; Google, which has no prompt=login, takes false,
	// and max_age alone.
	PromptLogin *bool `yaml:"prompt_login"`
}

// namePattern is a provider's name, as the control store checks it in
// an invitation.
var namePattern = lazyregexp.New(`^[a-z][a-z0-9-]{0,31}$`)

// domainPattern is a domain of Settings.Domains.
var domainPattern = lazyregexp.New(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// Options are the server's, for every provider.
type Options struct {
	// Now is the server's clock: the store's (rule 19).
	Now func() time.Time
	// Skew, MaxAge and Refetch are VerifierConfig's.
	Skew, MaxAge, Refetch time.Duration
}

// Provider is one configured provider: where to send a person to sign
// in, how to exchange the code the provider returns, and the verifier
// of its ID tokens. It is safe for concurrent use.
type Provider struct {
	s      Settings
	opts   Options
	issuer string
	secret string
	client *http.Client

	mu        sync.Mutex
	endpoints *endpoints // nil until discovered, with DiscoveryFetch
	verifier  *Verifier
	tried     time.Time // the last discovery tried
	lastErr   error
}

type endpoints struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// Open checks s and reads its files: the client secret, the CA file and
// the key set file. It fetches nothing: with DiscoveryFetch the
// discovery document is read at the first sign-in, so that a provider
// that cannot be reached at the daemon's start delays nothing.
func Open(s Settings, o Options) (*Provider, error) {
	fail := func(format string, a ...any) (*Provider, error) {
		return nil, fmt.Errorf("login: provider %s: "+format, append([]any{s.Name}, a...)...)
	}
	if !namePattern.MatchString(s.Name) {
		return nil, fmt.Errorf("login: provider name %q must be a lowercase letter, then up to 31 lowercase letters, digits and hyphens", s.Name)
	}
	iss, err := s.issuer()
	if err != nil {
		return fail("%v", err)
	}
	if s.ClientID == "" {
		return fail("client_id is required: the server's registration at the provider")
	}
	for _, d := range s.Domains {
		if !domainPattern.MatchString(strings.ToLower(d)) {
			return fail("domains: %q is not a domain, such as example.com", d)
		}
	}
	p := &Provider{s: s, opts: o, issuer: iss}
	if s.ClientSecretFile != "" {
		if p.secret, err = readSecret(s.ClientSecretFile); err != nil {
			return fail("%v", err)
		}
	}
	if p.client, err = HTTPClient(s.CAFile); err != nil {
		return fail("%v", err)
	}
	vc := VerifierConfig{Issuer: iss, Audience: s.ClientID, IDToken: true, Skew: o.Skew, MaxAge: o.MaxAge, Refetch: o.Refetch, Now: o.Now}
	switch s.Discovery {
	case "", DiscoveryFetch:
		if s.AuthorizationEndpoint != "" || s.TokenEndpoint != "" || s.JWKSURL != "" || s.JWKSFile != "" {
			return fail("endpoints, jwks_url and jwks_file are set with discovery: static, which writes them out; with discovery: fetch they come from the issuer")
		}
	case DiscoveryStatic:
		if s.AuthorizationEndpoint == "" || s.TokenEndpoint == "" {
			return fail("with discovery: static, authorization_endpoint and token_endpoint are required, since nothing is discovered")
		}
		if (s.JWKSFile == "") == (s.JWKSURL == "") {
			return fail("with discovery: static, give the keys as jwks_file or jwks_url, one of them")
		}
		for _, u := range []string{s.AuthorizationEndpoint, s.TokenEndpoint, s.JWKSURL} {
			if err := secureURL(u); u != "" && err != nil {
				return fail("%v", err)
			}
		}
		p.endpoints = &endpoints{Issuer: iss, AuthorizationEndpoint: s.AuthorizationEndpoint, TokenEndpoint: s.TokenEndpoint, JWKSURI: s.JWKSURL}
		if s.JWKSFile != "" {
			if p.verifier, err = NewFileVerifier(vc, s.JWKSFile); err != nil {
				return fail("%v", err)
			}
		} else {
			p.verifier = NewFetchVerifier(vc, s.JWKSURL, p.client)
		}
	default:
		return fail("discovery %q is not fetch or static", s.Discovery)
	}
	return p, nil
}

// issuer returns the exact iss s's tokens carry, or why s cannot have
// one.
func (s Settings) issuer() (string, error) {
	switch s.Kind {
	case KindOIDC:
		if s.Issuer == "" {
			return "", errors.New("issuer is required: the exact iss of the provider's tokens")
		}
		return s.Issuer, secureURL(s.Issuer)
	case KindEntra:
		if s.Authority == "" {
			return "", errors.New("authority is required for entra, and has no default: the sign-in host of the tenant's cloud, as its portal names it")
		}
		if err := secureURL(s.Authority); err != nil {
			return "", fmt.Errorf("authority: %w", err)
		}
		switch strings.ToLower(s.Tenant) {
		case "":
			return "", errors.New("tenant is required for entra: the tenant's id")
		case "common", "organizations", "consumers":
			return "", fmt.Errorf("tenant %q signs in accounts of any tenant; give the tenant's own id", s.Tenant)
		}
		switch s.TokenVersion {
		case 2:
			if s.Issuer != "" {
				return s.Issuer, secureURL(s.Issuer)
			}
			return strings.TrimSuffix(s.Authority, "/") + "/" + s.Tenant + "/v2.0", nil
		case 1:
			if s.Issuer == "" {
				return "", errors.New("issuer is required for entra with token_version 1: copy it from the tenant's v1.0 discovery document")
			}
			return s.Issuer, secureURL(s.Issuer)
		}
		return "", errors.New("token_version must be 1 or 2, as the app registration's manifest sets it")
	case "github":
		return "", errors.New("kind github is not built yet (transport.md §15, slice N6); use an OpenID Connect provider")
	}
	return "", fmt.Errorf("kind %q is not oidc or entra", s.Kind)
}

// secureURL refuses anything but https, except http to a host on this
// machine (localHost), for a test provider.
func secureURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%q is not a URL", raw)
	}
	if u.Scheme == "https" || u.Scheme == "http" && localHost(u.Hostname()) {
		return nil
	}
	return fmt.Errorf("%q is not https", raw)
}

// readSecret reads a client secret from path, a file only its owner may
// read (rule 2), and returns its first line.
func readSecret(path string) (string, error) {
	// Open once and check the open file's mode, so the file checked is the
	// file read even if the path is swapped meanwhile.
	f, err := os.Open(path) //nolint:gosec // the operator's client_secret_file
	if err != nil {
		return "", fmt.Errorf("client_secret_file: %w", err)
	}
	defer f.Close() //nolint:errcheck // read only
	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("client_secret_file: %w", err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("client_secret_file %s is mode %#o; it holds a secret, so make it 0600: chmod 600 %s", path, fi.Mode().Perm(), path)
	}
	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return "", fmt.Errorf("client_secret_file: %w", err)
	}
	secret, _, _ := strings.Cut(string(b), "\n")
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return "", fmt.Errorf("client_secret_file %s is empty", path)
	}
	return secret, nil
}

// Name is the provider's name in the settings.
func (p *Provider) Name() string { return p.s.Name }

// Kind is the provider's kind: KindOIDC or KindEntra.
func (p *Provider) Kind() string { return p.s.Kind }

// Issuer is the exact iss of the provider's tokens.
func (p *Provider) Issuer() string { return p.issuer }

// AnswerIssuer is the issuer of the provider's authorization server: what
// its discovery document names, and the iss its authorization responses
// carry when it sends one (RFC 9207). It is Issuer, except for Entra's
// version 1 tokens, whose issuer is not the v2.0 endpoint's that signs
// the person in.
func (p *Provider) AnswerIssuer() string {
	if p.s.Kind == KindEntra && p.s.TokenVersion == 1 {
		return strings.TrimSuffix(p.s.Authority, "/") + "/" + p.s.Tenant + "/v2.0"
	}
	return p.issuer
}

// discoveryURL is where the discovery document is read: the issuer's,
// or for Entra the v2.0 endpoint's of the tenant.
func (p *Provider) discoveryURL() string {
	if p.s.Kind == KindEntra {
		return strings.TrimSuffix(p.s.Authority, "/") + "/" + p.s.Tenant + "/v2.0/.well-known/openid-configuration"
	}
	return strings.TrimSuffix(p.issuer, "/") + "/.well-known/openid-configuration"
}

// discover returns the provider's endpoints, reading the discovery
// document the first time, and again after a failure at most once each
// Refetch.
func (p *Provider) discover(ctx context.Context) (*endpoints, *Verifier, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.endpoints != nil {
		return p.endpoints, p.verifier, nil
	}
	now := p.opts.Now()
	if !p.tried.IsZero() && now.Sub(p.tried) < p.opts.Refetch {
		return nil, nil, fmt.Errorf("%w: the discovery document could not be read %s ago: %v", ErrUnavailable, now.Sub(p.tried).Round(time.Second), p.lastErr)
	}
	p.tried = now
	e, err := p.fetchDiscovery(ctx)
	p.lastErr = err
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	p.endpoints = e
	p.verifier = NewFetchVerifier(VerifierConfig{Issuer: p.issuer, Audience: p.s.ClientID, IDToken: true,
		Skew: p.opts.Skew, MaxAge: p.opts.MaxAge, Refetch: p.opts.Refetch, Now: p.opts.Now}, e.JWKSURI, p.client)
	return p.endpoints, p.verifier, nil
}

func (p *Provider) fetchDiscovery(ctx context.Context) (*endpoints, error) {
	b, err := get(ctx, p.client, p.discoveryURL())
	if err != nil {
		return nil, fmt.Errorf("read the discovery document: %w", err)
	}
	var e endpoints
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, fmt.Errorf("the discovery document is not JSON: %w", err)
	}
	// The document must name the issuer it was read from (OpenID Connect
	// Discovery §4.3). Entra's v1.0 tokens carry another issuer than its
	// v2.0 document, which is read for the endpoints alone.
	if want := p.AnswerIssuer(); e.Issuer != want {
		return nil, fmt.Errorf("the discovery document names issuer %q, not %q", e.Issuer, want)
	}
	for _, u := range []string{e.AuthorizationEndpoint, e.TokenEndpoint, e.JWKSURI} {
		if err := secureURL(u); err != nil {
			return nil, fmt.Errorf("the discovery document's endpoint %v", err)
		}
	}
	return &e, nil
}

// AuthRequest is one sign-in's request to the provider: the server's
// own state, nonce and PKCE challenge, never the client's (decision 85).
type AuthRequest struct {
	State, Nonce string
	// Challenge is the S256 PKCE challenge of the verifier the server
	// keeps.
	Challenge string
	// Redirect is the server's callback, registered at the provider.
	Redirect string
	// MaxAge, when set, is sent as max_age.
	MaxAge time.Duration
}

// AuthorizeURL is where the person's browser is sent to sign in.
func (p *Provider) AuthorizeURL(ctx context.Context, a AuthRequest) (string, error) {
	e, _, err := p.discover(ctx)
	if err != nil {
		return "", err
	}
	v := url.Values{
		"response_type": {"code"}, "client_id": {p.s.ClientID}, "redirect_uri": {a.Redirect}, "scope": {"openid email"},
		"state": {a.State}, "nonce": {a.Nonce}, "code_challenge": {a.Challenge}, "code_challenge_method": {"S256"},
	}
	if p.s.PromptLogin == nil || *p.s.PromptLogin {
		v.Set("prompt", "login")
	}
	if a.MaxAge > 0 {
		v.Set("max_age", strconv.Itoa(int(a.MaxAge.Seconds())))
	}
	sep := "?"
	if strings.Contains(e.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	return e.AuthorizationEndpoint + sep + v.Encode(), nil
}

// Exchange spends code at the provider's token endpoint, with the PKCE
// verifier and the redirect of the request it answers, and the server's
// client secret, and returns the ID token. A refusal by the provider
// wraps ErrInvalidToken; a provider that cannot be reached, or answers
// with no ID token, ErrUnavailable.
func (p *Provider) Exchange(ctx context.Context, code, verifier, redirect string) (string, error) {
	e, _, err := p.discover(ctx)
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect},
		"client_id": {p.s.ClientID}, "code_verifier": {verifier}}
	if p.secret != "" {
		form.Set("client_secret", p.secret)
	}
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: the token endpoint: %v", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := readBody(resp, e.TokenEndpoint)
	var answer struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(b, &answer)
	switch {
	case answer.Error != "":
		// The provider's own word for why, such as invalid_grant: a code,
		// never a secret.
		return "", fmt.Errorf("%w: the provider refused the code: %s", ErrInvalidToken, safeWord(answer.Error))
	case err != nil:
		return "", fmt.Errorf("%w: the token endpoint: %v", ErrUnavailable, err)
	case answer.IDToken == "":
		return "", fmt.Errorf("%w: the token endpoint answered with no ID token; check that the registration has the openid scope", ErrUnavailable)
	}
	return answer.IDToken, nil
}

// safeWord is s if it is an OAuth error code's characters, and "an
// error" otherwise.
func safeWord(s string) string {
	if len(s) > 64 || strings.Trim(s, "abcdefghijklmnopqrstuvwxyz_") != "" {
		return "an error"
	}
	return s
}

// Identity is who a provider's token says signed in.
type Identity struct {
	// Issuer and Subject are what an identity is bound to and matched by.
	Issuer, Subject string
	// Address is the token's email, and Verified whether the provider
	// says it is the person's: only a verified address binds an
	// invitation (identity.md §3.2).
	Address  string
	Verified bool
	// AuthTime is when the person last authenticated at the provider,
	// or zero when the token does not say.
	AuthTime time.Time
}

// Check verifies an ID token of the provider and returns who it names.
// A refusal wraps ErrInvalidToken, ErrStale, ErrUnavailable or
// ErrNotAllowed.
func (p *Provider) Check(ctx context.Context, raw string, want Expect) (*Identity, error) {
	_, v, err := p.discover(ctx)
	if err != nil {
		return nil, err
	}
	c, err := v.Verify(ctx, raw, want)
	if err != nil {
		return nil, err
	}
	id := &Identity{Issuer: p.issuer, Subject: c.Sub, Address: c.String("email")}
	if c.AuthTime != 0 {
		id.AuthTime = time.Unix(c.AuthTime, 0).UTC()
	}
	switch v := c.All["email_verified"].(type) {
	case bool:
		id.Verified = v
	case string: // some providers send it as a string
		id.Verified = v == "true"
	}
	if p.s.Kind == KindEntra {
		// Entra's sub differs per app registration; oid is the person's
		// in the tenant. tid must be the configured tenant, and ver the
		// configured version. An address is verified only by xms_edov:
		// a single-tenant app sees unverified addresses otherwise.
		if tid := c.String("tid"); tid != p.s.Tenant {
			return nil, fmt.Errorf("%w: tid %q, not the tenant %q", ErrInvalidToken, tid, p.s.Tenant)
		}
		if ver := c.String("ver"); ver != fmt.Sprintf("%d.0", p.s.TokenVersion) {
			return nil, fmt.Errorf("%w: ver %q, not %d.0", ErrInvalidToken, ver, p.s.TokenVersion)
		}
		id.Subject = c.String("oid")
		if id.Subject == "" {
			return nil, fmt.Errorf("%w: no oid in the token", ErrInvalidToken)
		}
		edov, _ := c.All["xms_edov"].(bool)
		id.Verified = edov
	}
	if id.Subject == "" {
		return nil, fmt.Errorf("%w: no subject in the token", ErrInvalidToken)
	}
	for name, value := range p.s.Require {
		if got := c.String(name); got != value {
			return nil, fmt.Errorf("%w: the provider's settings require %s %q, and the token has %q", ErrNotAllowed, name, value, got)
		}
	}
	if len(p.s.Domains) > 0 {
		// An address the provider does not vouch for may be any domain's.
		if !id.Verified {
			return nil, fmt.Errorf("%w: the address is not verified, and domains takes only a verified one", ErrNotAllowed)
		}
		_, domain, _ := strings.Cut(id.Address, "@")
		if domain == "" || !slices.ContainsFunc(p.s.Domains, func(d string) bool { return strings.EqualFold(d, domain) }) {
			return nil, fmt.Errorf("%w: the address is not at %s", ErrNotAllowed, strings.Join(p.s.Domains, " or "))
		}
	}
	return id, nil
}

// Status is a provider as the server's status report shows it.
type Status struct {
	Name, Kind, Issuer string
	// Discovery is DiscoveryFetch or DiscoveryStatic.
	Discovery string
	// Discovered reports whether the endpoints are known: always with
	// DiscoveryStatic, and after the first read of the document with
	// DiscoveryFetch.
	Discovered bool
	// Keys are the verifier's, once there is one.
	Keys KeyStatus
	// Err is why discovery last failed, or empty.
	Err string
}

// Status reports the provider's endpoints and keys.
func (p *Provider) Status() Status {
	p.mu.Lock()
	s := Status{Name: p.s.Name, Kind: p.s.Kind, Issuer: p.issuer, Discovery: DiscoveryFetch, Discovered: p.endpoints != nil}
	if p.s.Discovery == DiscoveryStatic {
		s.Discovery = DiscoveryStatic
	}
	if p.lastErr != nil {
		s.Err = p.lastErr.Error()
	}
	v := p.verifier
	p.mu.Unlock()
	if v != nil {
		s.Keys = v.Status()
	} else {
		s.Keys.Source = SourceFetch
	}
	return s
}
