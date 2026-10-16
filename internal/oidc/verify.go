// Package oidc is ariesd's one verifier of tokens from identity
// providers (docs/design/identity.md §6.4, transport.md §8): an issuer
// and an audience pinned in the server's settings, the issuer's signing
// keys fetched from a configured URL and cached, or read from a key set
// file with nothing fetched at all (the offline mode of transport.md
// §8.2), and the claims every token is held to. What differs between
// providers is settings, not code ([Settings], [Provider]); nothing here
// names a provider's host.
//
// It is the standard library and internal/jose alone (rule 4).
package oidc

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ariesworx/aries/internal/jose"
)

// Limits on what is read from an issuer, or from a key set file.
const (
	// MaxDocument bounds a discovery document, a key set and a token
	// endpoint's answer.
	MaxDocument = 64 << 10
	// MaxKeys bounds the keys of a set.
	MaxKeys = 32
	// FetchTimeout bounds one request to an issuer.
	FetchTimeout = 10 * time.Second
)

// The kinds of refusal, which every error of Verify wraps one of.
var (
	// ErrInvalidToken: the token is not one the provider issued for this
	// server and this sign-in, or it has expired.
	ErrInvalidToken = errors.New("the token does not verify")
	// ErrStale: the person last authenticated at the provider too long
	// ago (auth_time), or the token does not say when.
	ErrStale = errors.New("the sign-in at the provider is not fresh")
	// ErrUnavailable: the provider's keys could not be had: a fetch
	// failed, or the token names a key the set or the file does not hold.
	ErrUnavailable = errors.New("the provider's keys are not available")
)

// VerifierConfig is what a token is held to, besides its signature.
type VerifierConfig struct {
	// Issuer is the exact iss.
	Issuer string
	// Audience is what aud must include: the server's client id at the
	// provider.
	Audience string
	// IDToken holds the token to OpenID Connect's rule for an ID token as
	// well: with several audiences, azp must be Audience.
	IDToken bool
	// Skew is allowed between the provider's clock and the server's.
	Skew time.Duration
	// MaxAge bounds how old a token may be at all, from its iat: it is
	// spent at once, so an old one is a replay or a stolen one. Zero
	// does not bound it.
	MaxAge time.Duration
	// Refetch is the least time between two reads of the key set, from
	// its URL or its file, so that a stranger with a made-up key id
	// cannot make the server fetch at will.
	Refetch time.Duration
	// Now is the server's clock: the store's (rule 19).
	Now func() time.Time
}

// The sources of a verifier's keys, as Status names them.
const (
	SourceFetch = "fetch"
	SourceFile  = "file"
)

// KeyStatus is where a verifier's keys came from and when, for the
// server's status report.
type KeyStatus struct {
	// Source is SourceFetch or SourceFile.
	Source string
	// KeyIDs are the ids of the signing keys held, in order.
	KeyIDs []string
	// ReadAt is when the keys were last fetched or read, on the server's
	// clock; zero before the first.
	ReadAt time.Time
	// Err is why the last fetch or read failed, or empty.
	Err string
}

// Verifier checks the tokens of one issuer. It is safe for concurrent
// use.
type Verifier struct {
	cfg VerifierConfig
	// Exactly one of url and file is set.
	url    string
	client *http.Client
	file   string

	mu      sync.Mutex
	keys    map[string]crypto.PublicKey
	readAt  time.Time
	tried   time.Time // the last read, whether or not it succeeded
	lastErr error
	// reading is closed when the read under way ends, and nil when none
	// is: a read is not made under mu, which would hold every token and
	// the status report behind a provider for up to FetchTimeout.
	reading chan struct{}
}

// NewFetchVerifier returns a verifier whose keys are fetched from
// jwksURL with c, at the first token and again for a key id it does not
// know, at most once each cfg.Refetch.
func NewFetchVerifier(cfg VerifierConfig, jwksURL string, c *http.Client) *Verifier {
	return &Verifier{cfg: cfg, url: jwksURL, client: c}
}

// NewFileVerifier returns a verifier whose keys are read from the key
// set file at path, which the operator supplies on a network that cannot
// reach the issuer's (transport.md §8.2). It fetches nothing, ever: a
// token signed by a key not in the file is refused, unavailable, until
// the operator replaces the file, which is read again at the first such
// token at least cfg.Refetch after the last read. The file must hold a
// signing key.
func NewFileVerifier(cfg VerifierConfig, path string) (*Verifier, error) {
	v := &Verifier{cfg: cfg, file: path}
	keys, err := readKeyFile(path)
	if err != nil {
		return nil, err
	}
	v.keys, v.readAt, v.tried = keys, cfg.Now(), cfg.Now()
	return v, nil
}

// readKeyFile reads the signing keys of the key set file at path.
func readKeyFile(path string) (map[string]crypto.PublicKey, error) {
	f, err := os.Open(path) //nolint:gosec // the operator's jwks_file
	if err != nil {
		return nil, fmt.Errorf("jwks_file: %w", err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, MaxDocument+1))
	if err != nil {
		return nil, fmt.Errorf("jwks_file %s: %w", path, err)
	}
	keys, err := parseKeys(b)
	if err != nil {
		return nil, fmt.Errorf("jwks_file %s: %w", path, err)
	}
	return keys, nil
}

// parseKeys reads a key set and returns its signing keys by id.
func parseKeys(b []byte) (map[string]crypto.PublicKey, error) {
	if len(b) > MaxDocument {
		return nil, fmt.Errorf("the key set is over %d bytes", MaxDocument)
	}
	var set jose.Set
	if err := json.Unmarshal(b, &set); err != nil {
		return nil, fmt.Errorf("the key set is not JSON: %w", err)
	}
	if len(set.Keys) > MaxKeys {
		return nil, fmt.Errorf("the key set has %d keys, over %d", len(set.Keys), MaxKeys)
	}
	keys := map[string]crypto.PublicKey{}
	for _, k := range set.Keys {
		pub, err := k.PublicKey()
		if err != nil || k.Kid == "" {
			continue // an encryption key, or one not used for signing here
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return nil, errors.New("the key set holds no signing key with an id")
	}
	return keys, nil
}

// Status reports where the verifier's keys came from and when.
func (v *Verifier) Status() KeyStatus {
	v.mu.Lock()
	defer v.mu.Unlock()
	s := KeyStatus{Source: SourceFetch, ReadAt: v.readAt}
	if v.file != "" {
		s.Source = SourceFile
	}
	for id := range v.keys {
		s.KeyIDs = append(s.KeyIDs, id)
	}
	slices.Sort(s.KeyIDs)
	if v.lastErr != nil {
		s.Err = v.lastErr.Error()
	}
	return s
}

// key returns the signing key kid names, reading the set again when kid
// is not known and the last read is at least Refetch ago. A token that
// comes during a read waits for it, so that a rotated key is read once.
func (v *Verifier) key(ctx context.Context, kid string) (crypto.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for v.reading != nil {
		if k, ok := v.keys[kid]; ok {
			return k, nil
		}
		reading := v.reading
		v.mu.Unlock()
		select {
		case <-reading:
		case <-ctx.Done():
			v.mu.Lock()
			return nil, fmt.Errorf("%w: waiting for the key set: %v", ErrUnavailable, ctx.Err())
		}
		v.mu.Lock()
	}
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	now := v.cfg.Now()
	where := "the issuer's key set"
	if v.file != "" {
		where = "the key set file " + v.file
	}
	if !v.tried.IsZero() && now.Sub(v.tried) < v.cfg.Refetch {
		return nil, fmt.Errorf("%w: no key %q in %s, read %s ago", ErrUnavailable, kid, where, now.Sub(v.tried).Round(time.Second))
	}
	v.tried = now
	reading := make(chan struct{})
	v.reading = reading
	v.mu.Unlock()
	var keys map[string]crypto.PublicKey
	var err error
	if v.file != "" {
		keys, err = readKeyFile(v.file)
	} else {
		keys, err = v.fetch(ctx)
	}
	v.mu.Lock()
	v.reading = nil
	close(reading)
	v.lastErr = err
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	v.keys, v.readAt = keys, now
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	if v.file != "" {
		return nil, fmt.Errorf("%w: no key %q in the key set file %s; the operator replaces it when the provider rotates its keys", ErrUnavailable, kid, v.file)
	}
	return nil, fmt.Errorf("%w: no key %q in %s", ErrUnavailable, kid, where)
}

func (v *Verifier) fetch(ctx context.Context) (map[string]crypto.PublicKey, error) {
	b, err := get(ctx, v.client, v.url)
	if err != nil {
		return nil, fmt.Errorf("fetch the key set: %w", err)
	}
	return parseKeys(b)
}

// Claims are the registered claims every token is held to, and all of
// them as they came.
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

// String is a claim's value when it is a string, and empty otherwise.
func (c *Claims) String(name string) string {
	s, _ := c.All[name].(string)
	return s
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
	// Nonce, when set, must equal the token's nonce: the one the server
	// sent with the sign-in.
	Nonce string
	// AuthAge, when set, bounds how long ago the person last
	// authenticated at the provider (auth_time), which prompt=login
	// makes the time of this sign-in.
	AuthAge time.Duration
}

// Verify checks raw: its form, its signature under the issuer's key, the
// issuer, the audience, its times on the verifier's clock, and what want
// asks. It returns the claims only when all of that holds; a refusal
// wraps ErrInvalidToken, ErrStale or ErrUnavailable.
func (v *Verifier) Verify(ctx context.Context, raw string, want Expect) (*Claims, error) {
	j, err := jose.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	key, err := v.key(ctx, j.Header.Kid)
	if err != nil {
		return nil, err
	}
	if err := j.Verify(key); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	var c Claims
	if err := json.Unmarshal(j.Payload, &c); err != nil {
		return nil, fmt.Errorf("%w: claims: %v", ErrInvalidToken, err)
	}
	if err := json.Unmarshal(j.Payload, &c.All); err != nil {
		return nil, fmt.Errorf("%w: claims: %v", ErrInvalidToken, err)
	}
	if err := v.check(&c, want); err != nil {
		return nil, err
	}
	return &c, nil
}

func (v *Verifier) check(c *Claims, want Expect) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrInvalidToken}, a...)...)
	}
	if c.Iss != v.cfg.Issuer {
		return bad("issuer %q, not %q", c.Iss, v.cfg.Issuer)
	}
	if !slices.Contains(c.Aud, v.cfg.Audience) {
		return bad("audience %q does not include %q", []string(c.Aud), v.cfg.Audience)
	}
	// With several audiences, the party it was issued to must be ours
	// (OpenID Connect Core §3.1.3.7, 4 and 5).
	if v.cfg.IDToken && len(c.Aud) > 1 && c.Azp != v.cfg.Audience {
		return bad("authorized party %q, not %q", c.Azp, v.cfg.Audience)
	}
	now, skew := v.cfg.Now(), v.cfg.Skew
	at := func(s int64) string { return time.Unix(s, 0).UTC().Format(time.RFC3339) }
	switch {
	case c.Exp == 0:
		return bad("no exp")
	case c.Iat == 0:
		return bad("no iat")
	case now.After(time.Unix(c.Exp, 0).Add(skew)):
		return bad("expired at %s", at(c.Exp))
	case c.Nbf != 0 && now.Add(skew).Before(time.Unix(c.Nbf, 0)):
		return bad("not valid until %s (nbf)", at(c.Nbf))
	case now.Add(skew).Before(time.Unix(c.Iat, 0)):
		return bad("issued in the future (iat %s); the provider's clock and the server's disagree", at(c.Iat))
	case v.cfg.MaxAge > 0 && now.Sub(time.Unix(c.Iat, 0)) > v.cfg.MaxAge+skew:
		return bad("issued %s ago, over %s", now.Sub(time.Unix(c.Iat, 0)).Round(time.Second), v.cfg.MaxAge)
	}
	if want.Nonce != "" && c.Nonce != want.Nonce {
		return bad("the nonce is not this sign-in's")
	}
	if want.AuthAge > 0 {
		if c.AuthTime == 0 {
			return fmt.Errorf("%w: the token has no auth_time", ErrStale)
		}
		if age := now.Sub(time.Unix(c.AuthTime, 0)); age > want.AuthAge+skew {
			return fmt.Errorf("%w: last authenticated at the provider %s ago, over %s", ErrStale, age.Round(time.Second), want.AuthAge)
		}
	}
	return nil
}

// HTTPClient returns the client a provider's requests go through: no
// redirect followed, FetchTimeout per request, TLS 1.2 at least, and,
// when caFile is set, the certificate authorities in it alone, for a
// provider whose certificate is from an internal authority.
func HTTPClient(caFile string) (*http.Client, error) {
	if caFile == "" {
		return noRedirects(nil), nil
	}
	pem, err := os.ReadFile(caFile) //nolint:gosec // the operator's ca_file
	if err != nil {
		return nil, fmt.Errorf("ca_file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("ca_file %s holds no certificate", caFile)
	}
	return noRedirects(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}), nil
}

func noRedirects(tc *tls.Config) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if tc == nil {
		tc = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	tr.TLSClientConfig = tc
	// The environment's proxy, HTTPS_PROXY and NO_PROXY, is the
	// operator's, for a site whose egress goes through one.
	tr.Proxy = http.ProxyFromEnvironment
	return &http.Client{Transport: tr, Timeout: FetchTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("a provider's redirect is not followed")
	}}
}

// get reads the body of a GET of u, at most MaxDocument bytes.
func get(ctx context.Context, c *http.Client, u string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return readBody(resp, u)
}

// readBody reads resp's body, refusing a status other than 200 and a
// body over MaxDocument.
func readBody(resp *http.Response, u string) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxDocument+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxDocument {
		return nil, fmt.Errorf("%s answered over %d bytes", redact(u), MaxDocument)
	}
	if resp.StatusCode != http.StatusOK {
		return b, &StatusError{URL: redact(u), Status: resp.Status, Body: b}
	}
	return b, nil
}

// StatusError is a provider's answer with a status other than 200.
type StatusError struct {
	URL, Status string
	Body        []byte
}

func (e *StatusError) Error() string { return fmt.Sprintf("%s answered %s", e.URL, e.Status) }

// redact is u without its query, which may carry what is not for a log.
func redact(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return "the provider"
	}
	p.RawQuery, p.Fragment, p.User = "", "", nil
	return p.String()
}

// localHost reports whether host is reached on this machine by name: a
// loopback address, localhost, or a name under .localhost (RFC 6761),
// where plain http is allowed for a test provider.
func localHost(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	return h == "127.0.0.1" || h == "::1"
}
