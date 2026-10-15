// Package oidc is the one verifier of the design (docs/design/identity.md §6.4): an
// issuer and an audience pinned in the server's settings, the issuer's
// signing keys fetched from its discovery document and cached, and the
// claims every token is held to. People's ID tokens (Keycloak, Google,
// Entra ID, Okta) and workload tokens (GitHub Actions, Google Cloud,
// Azure) go through the same Verify; what differs per provider is
// settings, not code.
package oidc

import (
	"context"
	"crypto"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ariesworx/aries/spike/identity/jose"
)

// Limits on what is read from an issuer.
const (
	maxDoc       = 64 << 10
	maxKeys      = 32
	fetchTimeout = 10 * time.Second
)

// Discovery is what the verifier and the client take from an issuer's
// /.well-known/openid-configuration.
type Discovery struct {
	Issuer                      string   `json:"issuer"`
	AuthorizationEndpoint       string   `json:"authorization_endpoint"`
	TokenEndpoint               string   `json:"token_endpoint"`
	DeviceAuthorizationEndpoint string   `json:"device_authorization_endpoint"`
	JWKSURI                     string   `json:"jwks_uri"`
	CodeChallengeMethods        []string `json:"code_challenge_methods_supported"`
}

// Discover reads issuer's discovery document. Its issuer must equal
// issuer exactly (OpenID Connect Discovery §4.3), and every endpoint must
// be https, or http on a loopback address for tests.
func Discover(ctx context.Context, c *http.Client, issuer string) (*Discovery, error) {
	var d Discovery
	if err := getJSON(ctx, c, strings.TrimSuffix(issuer, "/")+"/.well-known/openid-configuration", &d); err != nil {
		return nil, fmt.Errorf("discover %s: %w", issuer, err)
	}
	if d.Issuer != issuer {
		return nil, fmt.Errorf("discover %s: the document names issuer %q", issuer, d.Issuer)
	}
	for _, u := range []string{d.AuthorizationEndpoint, d.TokenEndpoint, d.JWKSURI} {
		if err := secureURL(u); err != nil {
			return nil, fmt.Errorf("discover %s: %w", issuer, err)
		}
	}
	return &d, nil
}

// secureURL refuses anything but https, except http to a loopback host.
func secureURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("endpoint %q: %w", raw, err)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1") {
		return nil
	}
	return fmt.Errorf("endpoint %q is not https", raw)
}

func getJSON(ctx context.Context, c *http.Client, u string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxDoc+1))
	if err != nil {
		return err
	}
	if len(b) > maxDoc {
		return fmt.Errorf("GET %s: over %d bytes", u, maxDoc)
	}
	return json.Unmarshal(b, v)
}

// Config is one provider's entry in the server's settings.
type Config struct {
	Issuer   string
	Audience string // the client id for people; the server's own name for workloads
	// IDToken holds the token to OpenID Connect's rules for an ID token
	// as well: with several audiences, azp must be ours. A workload's
	// token is not an ID token for a client of ours, and its azp, where
	// it has one, names the workload.
	IDToken bool
	// Skew is allowed between the issuer's clock and the store's.
	Skew time.Duration
	// MaxAge bounds how old a token may be at all, from its iat: a token
	// is spent at once, so an old one is a replay or a stolen one.
	MaxAge time.Duration
	// Refetch is the least time between two fetches of the key set, so
	// that a stranger with a made-up kid cannot make the server fetch at
	// will.
	Refetch time.Duration
}

// Verifier checks tokens of one issuer.
type Verifier struct {
	cfg    Config
	client *http.Client
	now    func() time.Time
	jwks   string

	mu      sync.Mutex
	keys    map[string]crypto.PublicKey
	static  bool // keys came from a file and are never fetched
	fetched time.Time
	// Fetches counts key set fetches, for the spike's report.
	Fetches int
}

// New makes a verifier from a discovery document already read. now is
// the store's clock in ariesd (rule 19).
func New(cfg Config, d *Discovery, c *http.Client, now func() time.Time) *Verifier {
	return &Verifier{cfg: cfg, client: c, now: now, jwks: d.JWKSURI}
}

// NewStatic makes a verifier whose signing keys are set, as read from a
// key set file the operator supplies on a network that cannot reach the
// issuer's (transport.md, the air-gapped profile). It fetches nothing,
// ever: a token signed by a key not in set is refused until the operator
// replaces the file.
func NewStatic(cfg Config, set jose.Set, now func() time.Time) (*Verifier, error) {
	keys := signingKeys(set)
	if len(keys) == 0 {
		return nil, errors.New("the key set file holds no signing key")
	}
	return &Verifier{cfg: cfg, now: now, keys: keys, static: true}, nil
}

func signingKeys(set jose.Set) map[string]crypto.PublicKey {
	keys := make(map[string]crypto.PublicKey)
	for _, k := range set.Keys {
		pub, err := k.PublicKey()
		if err != nil || k.Kid == "" {
			continue // an encryption key, or one of a kind not used for signing here
		}
		keys[k.Kid] = pub
	}
	return keys
}

// key returns the signing key kid names, fetching the set when it is not
// known and the last fetch is at least Refetch ago.
func (v *Verifier) key(ctx context.Context, kid string) (crypto.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	if v.static {
		return nil, fmt.Errorf("no key %q in the key set file; the operator replaces it when the issuer rotates its keys", kid)
	}
	if !v.fetched.IsZero() && v.now().Sub(v.fetched) < v.cfg.Refetch {
		return nil, fmt.Errorf("no key %q, and the key set was fetched %s ago", kid, v.now().Sub(v.fetched).Round(time.Second))
	}
	var set jose.Set
	v.fetched = v.now()
	v.Fetches++
	if err := getJSON(ctx, v.client, v.jwks, &set); err != nil {
		return nil, fmt.Errorf("fetch the key set: %w", err)
	}
	if len(set.Keys) > maxKeys {
		return nil, fmt.Errorf("the key set has %d keys, over %d", len(set.Keys), maxKeys)
	}
	keys := signingKeys(set)
	v.keys = keys
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("no key %q in the issuer's key set", kid)
}

// Claims are the registered claims every token is held to, and the rest
// as they came.
type Claims struct {
	Iss      string   `json:"iss"`
	Sub      string   `json:"sub"`
	Aud      Audience `json:"aud"`
	Azp      string   `json:"azp"`
	Exp      int64    `json:"exp"`
	Nbf      int64    `json:"nbf"`
	Iat      int64    `json:"iat"`
	AuthTime int64    `json:"auth_time"`
	Nonce    string   `json:"nonce"`
	All      map[string]any
}

// Audience is aud, which is a string or an array of strings.
type Audience []string

// UnmarshalJSON takes either form.
func (a *Audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = Audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New("aud is neither a string nor an array of strings")
	}
	*a = many
	return nil
}

// Expect is what the caller knows a particular token must say.
type Expect struct {
	// Nonce, when set, must equal the token's nonce. A person's sign-in
	// always sets it; a workload token has none.
	Nonce string
	// AuthAge, when set, bounds how long ago the person last
	// authenticated at the provider (auth_time), for decision 61's fresh
	// sign-in.
	AuthAge time.Duration
}

// Verify checks raw: its form, its signature under the issuer's key, the
// issuer, the audience, its times on the verifier's clock, and what want
// asks. It returns the claims only when all of that holds.
func (v *Verifier) Verify(ctx context.Context, raw string, want Expect) (*Claims, error) {
	j, err := jose.Parse(raw)
	if err != nil {
		return nil, err
	}
	key, err := v.key(ctx, j.Header.Kid)
	if err != nil {
		return nil, err
	}
	if err := j.Verify(key); err != nil {
		return nil, err
	}
	var c Claims
	if err := json.Unmarshal(j.Payload, &c); err != nil {
		return nil, fmt.Errorf("claims: %w", err)
	}
	if err := json.Unmarshal(j.Payload, &c.All); err != nil {
		return nil, fmt.Errorf("claims: %w", err)
	}
	if c.Iss != v.cfg.Issuer {
		return nil, fmt.Errorf("issuer %q, want %q", c.Iss, v.cfg.Issuer)
	}
	if !slices.Contains(c.Aud, v.cfg.Audience) {
		return nil, fmt.Errorf("audience %q does not include %q", []string(c.Aud), v.cfg.Audience)
	}
	// With more than one audience, the party it was issued to must be
	// ours (OpenID Connect Core §3.1.3.7, 4 and 5).
	if v.cfg.IDToken && len(c.Aud) > 1 && c.Azp != "" && c.Azp != v.cfg.Audience {
		return nil, fmt.Errorf("authorized party %q, want %q", c.Azp, v.cfg.Audience)
	}
	now := v.now()
	skew := v.cfg.Skew
	switch {
	case c.Exp == 0 || c.Iat == 0:
		return nil, errors.New("no exp or no iat")
	case now.After(time.Unix(c.Exp, 0).Add(skew)):
		return nil, fmt.Errorf("expired at %s", time.Unix(c.Exp, 0).UTC().Format(time.RFC3339))
	case c.Nbf != 0 && now.Add(skew).Before(time.Unix(c.Nbf, 0)):
		return nil, errors.New("not yet valid (nbf)")
	case now.Add(skew).Before(time.Unix(c.Iat, 0)):
		return nil, errors.New("issued in the future (iat)")
	case v.cfg.MaxAge > 0 && now.Sub(time.Unix(c.Iat, 0)) > v.cfg.MaxAge:
		return nil, fmt.Errorf("issued %s ago, over %s", now.Sub(time.Unix(c.Iat, 0)).Round(time.Second), v.cfg.MaxAge)
	}
	if want.Nonce != "" && c.Nonce != want.Nonce {
		return nil, errors.New("nonce does not match: the token was not issued for this key and challenge")
	}
	if want.AuthAge > 0 {
		if c.AuthTime == 0 {
			return nil, errors.New("no auth_time, and a fresh sign-in is required")
		}
		if now.Sub(time.Unix(c.AuthTime, 0)) > want.AuthAge+skew {
			return nil, fmt.Errorf("last authenticated %s ago, over %s", now.Sub(time.Unix(c.AuthTime, 0)).Round(time.Second), want.AuthAge)
		}
	}
	return &c, nil
}
