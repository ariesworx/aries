// Package provider turns one identity provider's settings into a
// verifier, for a server that may be on the internet or on a network
// with no route out (transport.md, the air-gapped profile).
//
// Nothing here names a provider's host. An Entra ID provider takes its
// authority (the commercial cloud's, Azure Government's, or one that
// exists only inside a disconnected cloud), its tenant and its token
// version from the settings, and has no default authority. Keys come
// from the configured issuer's key set, through the operator's CA file
// where the issuer's certificate is from an internal authority, or from
// a key set file with every endpoint written in the settings, in which
// case nothing is fetched at all.
package provider

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ariesworx/aries/spike/identity/jose"
	"github.com/ariesworx/aries/spike/identity/oidc"
)

// Kinds of provider.
const (
	KindOIDC  = "oidc"  // any OpenID Connect provider: Keycloak, AD FS, Okta, Google
	KindEntra = "entra" // Microsoft Entra ID, in any cloud
)

// Config is one provider's entry in ariesd.yaml.
type Config struct {
	Name     string
	Kind     string
	ClientID string // the audience of its ID tokens: Aries's registration there

	// Issuer is the exact iss. Required for oidc, and for entra with
	// TokenVersion 1, whose issuer differs by cloud and is read from the
	// tenant's own discovery document by the operator.
	Issuer string
	// Authority, Tenant and TokenVersion are Entra's: the issuer of a
	// v2.0 token is Authority/Tenant/v2.0.
	Authority    string
	Tenant       string
	TokenVersion int

	// CAFile holds the certificate authorities trusted for this
	// provider's endpoints, for an issuer whose certificate is from an
	// internal authority. Empty: the system's.
	CAFile string
	// JWKSFile, with AuthorizationEndpoint and TokenEndpoint, replaces
	// discovery: nothing is fetched to check a token.
	JWKSFile              string
	AuthorizationEndpoint string
	TokenEndpoint         string
}

// Provider checks one provider's ID tokens.
type Provider struct {
	cfg       Config
	issuer    string
	verifier  *oidc.Verifier
	Endpoints oidc.Discovery
	// Client reaches the provider's token endpoint, with the operator's
	// CA file where one is set.
	Client *http.Client
}

// Identity is who a token says signed in: the issuer and the subject
// that identity.md §3.2 binds an invitation to.
type Identity struct {
	Issuer  string
	Subject string
	Claims  *oidc.Claims
}

func httpsURL(raw, what string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s %q is not a URL", what, raw)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("%s %q is not https", what, raw)
	}
	return nil
}

// issuerOf is the iss a provider's tokens must carry exactly.
func (c Config) issuerOf() (string, error) {
	switch c.Kind {
	case KindOIDC:
		if c.Issuer == "" {
			return "", fmt.Errorf("provider %s: issuer is required", c.Name)
		}
		return c.Issuer, httpsURL(c.Issuer, "issuer")
	case KindEntra:
		if c.Authority == "" {
			return "", fmt.Errorf("provider %s: authority is required for entra, and has no default: the cloud's sign-in host, such as the one the tenant's portal names", c.Name)
		}
		if err := httpsURL(c.Authority, "authority"); err != nil {
			return "", fmt.Errorf("provider %s: %w", c.Name, err)
		}
		if c.Tenant == "" {
			return "", fmt.Errorf("provider %s: tenant is required for entra (its id; common and organizations are refused)", c.Name)
		}
		switch c.TokenVersion {
		case 2:
			if c.Issuer != "" {
				return c.Issuer, httpsURL(c.Issuer, "issuer")
			}
			return strings.TrimSuffix(c.Authority, "/") + "/" + c.Tenant + "/v2.0", nil
		case 1:
			if c.Issuer == "" {
				return "", fmt.Errorf("provider %s: issuer is required for entra token_version 1; copy it from the tenant's v1.0 discovery document", c.Name)
			}
			return c.Issuer, httpsURL(c.Issuer, "issuer")
		default:
			return "", fmt.Errorf("provider %s: token_version must be 1 or 2, as the app registration's manifest sets it", c.Name)
		}
	}
	return "", fmt.Errorf("provider %s: kind %q is not oidc or entra", c.Name, c.Kind)
}

// Open reads the settings, the CA file and the key set file, and, unless
// a key set file is given, the issuer's discovery document through the
// configured issuer URL only.
func Open(ctx context.Context, cfg Config, now func() time.Time) (*Provider, error) {
	iss, err := cfg.issuerOf()
	if err != nil {
		return nil, err
	}
	if cfg.ClientID == "" {
		return nil, fmt.Errorf("provider %s: client_id is required", cfg.Name)
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("redirects are not followed")
	}}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("provider %s: ca_file: %w", cfg.Name, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("provider %s: ca_file %s holds no certificate", cfg.Name, cfg.CAFile)
		}
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	}
	vc := oidc.Config{Issuer: iss, Audience: cfg.ClientID, IDToken: true, Skew: 30 * time.Second, MaxAge: 10 * time.Minute, Refetch: time.Minute}
	p := &Provider{cfg: cfg, issuer: iss, Client: client}
	if cfg.JWKSFile != "" {
		if cfg.AuthorizationEndpoint == "" || cfg.TokenEndpoint == "" {
			return nil, fmt.Errorf("provider %s: with jwks_file, authorization_endpoint and token_endpoint are required, since nothing is discovered", cfg.Name)
		}
		b, err := os.ReadFile(cfg.JWKSFile)
		if err != nil {
			return nil, fmt.Errorf("provider %s: jwks_file: %w", cfg.Name, err)
		}
		var set jose.Set
		if err := json.Unmarshal(b, &set); err != nil {
			return nil, fmt.Errorf("provider %s: jwks_file: %w", cfg.Name, err)
		}
		if p.verifier, err = oidc.NewStatic(vc, set, now); err != nil {
			return nil, fmt.Errorf("provider %s: %w", cfg.Name, err)
		}
		p.Endpoints = oidc.Discovery{Issuer: iss, AuthorizationEndpoint: cfg.AuthorizationEndpoint, TokenEndpoint: cfg.TokenEndpoint}
		return p, nil
	}
	d, err := oidc.Discover(ctx, client, iss)
	if err != nil {
		return nil, fmt.Errorf("provider %s: %w", cfg.Name, err)
	}
	p.Endpoints = *d
	p.verifier = oidc.New(vc, d, client, now)
	return p, nil
}

// Check verifies an ID token and returns who it names.
func (p *Provider) Check(ctx context.Context, raw string, want oidc.Expect) (*Identity, error) {
	c, err := p.verifier.Verify(ctx, raw, want)
	if err != nil {
		return nil, err
	}
	sub := c.Sub
	if p.cfg.Kind == KindEntra {
		// Entra's sub differs per app registration; oid is the person's
		// in the tenant. tid must be the configured tenant, and ver the
		// configured version.
		if tid, _ := c.All["tid"].(string); tid != p.cfg.Tenant {
			return nil, fmt.Errorf("tid %q, want %q", tid, p.cfg.Tenant)
		}
		if ver, _ := c.All["ver"].(string); ver != fmt.Sprintf("%d.0", p.cfg.TokenVersion) {
			return nil, fmt.Errorf("ver %q, want %d.0", ver, p.cfg.TokenVersion)
		}
		oid, _ := c.All["oid"].(string)
		if oid == "" {
			return nil, errors.New("no oid in the token")
		}
		sub = oid
	}
	if sub == "" {
		return nil, errors.New("no subject in the token")
	}
	return &Identity{Issuer: p.issuer, Subject: sub, Claims: c}, nil
}
