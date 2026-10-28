package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ariesworx/aries/internal/jose"
)

// Workload providers (docs/design/identity.md §7, transport.md §7,
// slice N5): the platforms whose tokens a service principal signs in
// with, a CI job's or a cloud workload's. A workload's token is verified
// by the same verifier as a person's, with the workload provider's
// issuer and the server's workload audience; what differs between
// platforms is which claims a trust rule must pin, which say which run
// it was, and whether the audience can be bound to the job's key.

// Kinds of workload provider (identity.md §7.2). Kubernetes, which the
// design leaves optional and untested, is not built.
const (
	// KindGitHubActions is GitHub Actions' OIDC token, which a job asks
	// for with the audience it chooses.
	KindGitHubActions = "github-actions"
	// KindGoogle is a Google Cloud service account's ID token, from the
	// metadata server, with the audience it chooses.
	KindGoogle = "google"
	// KindAzure is an Azure managed identity's token for the app
	// registration that stands for the server: its audience is fixed.
	KindAzure = "azure"
)

// workloadKind is what Aries knows of a platform's tokens.
type workloadKind struct {
	// required are the claims every rule must pin: immutable ids, never
	// a name that can be renamed or recreated, never sub alone.
	required []string
	// launch are the claims that say which run of which job a token was
	// for, which the sign-in's event records (identity.md §7.5).
	launch []string
	// bound says the job chooses the audience, so that it can be bound
	// to the job's key: AUDIENCE/n/THUMBPRINT.
	bound bool
}

var workloadKinds = map[string]workloadKind{
	KindGitHubActions: {required: []string{"repository_owner_id", "repository_id"},
		launch: []string{"run_id", "run_attempt", "workflow_ref", "actor_id", "event_name", "ref"}, bound: true},
	KindGoogle: {required: []string{"sub", "email"},
		launch: []string{"google.compute_engine.project_id", "google.compute_engine.instance_id"}, bound: true},
	KindAzure: {required: []string{"tid", "oid"}, launch: []string{"xms_mirid"}},
}

// WorkloadKinds are the kinds of workload provider, in order.
func WorkloadKinds() []string { return slices.Sorted(maps.Keys(workloadKinds)) }

// RequiredClaims are the claims a trust rule at a provider of kind must
// pin, or nil for a kind that is not one.
func RequiredClaims(kind string) []string { return slices.Clone(workloadKinds[kind].required) }

// LaunchClaims are the claims of kind's tokens that say which run of
// which job it was.
func LaunchClaims(kind string) []string { return slices.Clone(workloadKinds[kind].launch) }

// BoundAudience is audience bound to the key whose JWK thumbprint is
// jkt: what a job of a platform that lets it choose asks for, so that
// its token is good for nothing without that key.
func BoundAudience(audience, jkt string) string { return audience + "/n/" + jkt }

// WorkloadSettings are one provider's entry under workload: providers:
// in ariesd's config.
type WorkloadSettings struct {
	// Name is how a trust rule names it, as a provider of login: is
	// named.
	Name string `yaml:"name"`
	// Kind is KindGitHubActions, KindGoogle or KindAzure.
	Kind string `yaml:"kind"`
	// Issuer is the exact iss of its tokens.
	Issuer string `yaml:"issuer"`
	// Audience is what its tokens are for. Required for azure, where it
	// is the Application ID URI of the app registration that stands for
	// the server; refused for the others, whose audience is the
	// server's (workload: audience), bound to the job's key.
	Audience string `yaml:"audience"`
	// Discovery is DiscoveryFetch (the default), which reads the
	// issuer's discovery document for its key set's URL, or
	// DiscoveryStatic, with JWKSURL or JWKSFile.
	Discovery string `yaml:"discovery"`
	JWKSURL   string `yaml:"jwks_url"`
	JWKSFile  string `yaml:"jwks_file"`
	// CAFile holds the certificate authorities trusted for the issuer's
	// endpoints, in place of the system's.
	CAFile string `yaml:"ca_file"`
}

// Workload is one configured workload provider: the verifier of its
// tokens. It is safe for concurrent use.
type Workload struct {
	s        WorkloadSettings
	kind     workloadKind
	audience string
	cfg      VerifierConfig
	client   *http.Client

	mu       sync.Mutex
	verifier *Verifier // nil until discovered, with DiscoveryFetch
	tried    time.Time
	lastErr  error
}

// OpenWorkload checks s and reads its files. audience is the server's
// workload audience, which a provider of a kind that chooses its own
// audience is held to. It fetches nothing: with DiscoveryFetch the
// discovery document is read at the first token.
func OpenWorkload(s WorkloadSettings, audience string, o Options) (*Workload, error) {
	fail := func(format string, a ...any) (*Workload, error) {
		return nil, fmt.Errorf("workload: provider %s: "+format, append([]any{s.Name}, a...)...)
	}
	if !namePattern.MatchString(s.Name) {
		return nil, fmt.Errorf("workload: provider name %q must be a lowercase letter, then up to 31 lowercase letters, digits and hyphens", s.Name)
	}
	k, ok := workloadKinds[s.Kind]
	if !ok {
		return fail("kind %q is not one of %s", s.Kind, strings.Join(WorkloadKinds(), ", "))
	}
	if s.Issuer == "" {
		return fail("issuer is required: the exact iss of the platform's tokens")
	}
	if err := secureURL(s.Issuer); err != nil {
		return fail("issuer: %v", err)
	}
	w := &Workload{s: s, kind: k, audience: audience}
	switch {
	case k.bound && s.Audience != "":
		return fail("audience is set, and a %s job asks for the server's own audience (workload: audience), bound to its key; remove it", s.Kind)
	case !k.bound && s.Audience == "":
		return fail("audience is required for %s: the Application ID URI of the app registration that stands for the server", s.Kind)
	case !k.bound:
		w.audience = s.Audience
	case audience == "":
		return fail("the server has no workload audience")
	}
	var err error
	if w.client, err = HTTPClient(s.CAFile); err != nil {
		return fail("%w", err) // the caller tells a file it may not read
	}
	w.cfg = VerifierConfig{Issuer: s.Issuer, Audience: w.audience, Skew: o.Skew, MaxAge: o.MaxAge, Refetch: o.Refetch, Now: o.Now}
	switch s.Discovery {
	case "", DiscoveryFetch:
		if s.JWKSURL != "" || s.JWKSFile != "" {
			return fail("jwks_url and jwks_file are set with discovery: static, which writes them out; with discovery: fetch they come from the issuer")
		}
	case DiscoveryStatic:
		if (s.JWKSFile == "") == (s.JWKSURL == "") {
			return fail("with discovery: static, give the keys as jwks_file or jwks_url, one of them")
		}
		if s.JWKSFile != "" {
			if w.verifier, err = NewFileVerifier(w.cfg, s.JWKSFile); err != nil {
				return fail("%w", err)
			}
		} else {
			if err := secureURL(s.JWKSURL); err != nil {
				return fail("jwks_url: %v", err)
			}
			w.verifier = NewFetchVerifier(w.cfg, s.JWKSURL, w.client)
		}
	default:
		return fail("discovery %q is not fetch or static", s.Discovery)
	}
	return w, nil
}

// Name is the provider's name in the settings.
func (w *Workload) Name() string { return w.s.Name }

// Kind is the provider's kind.
func (w *Workload) Kind() string { return w.s.Kind }

// Issuer is the exact iss of the provider's tokens.
func (w *Workload) Issuer() string { return w.s.Issuer }

// Audience is what the provider's tokens are for, before any binding.
func (w *Workload) Audience() string { return w.audience }

// Bound reports whether the provider's tokens are bound to the job's
// key by their audience. A token that is not is spent once, and taken
// only if issued since the daemon started (identity.md §3.5).
func (w *Workload) Bound() bool { return w.kind.bound }

// verifierFor returns the verifier, reading the discovery document the
// first time, and again after a failure at most once each Refetch.
func (w *Workload) verifierFor(ctx context.Context) (*Verifier, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.verifier != nil {
		return w.verifier, nil
	}
	now := w.cfg.Now()
	if !w.tried.IsZero() && now.Sub(w.tried) < w.cfg.Refetch {
		return nil, fmt.Errorf("%w: the discovery document could not be read %s ago: %v", ErrUnavailable, now.Sub(w.tried).Round(time.Second), w.lastErr)
	}
	w.tried = now
	jwks, err := w.discover(ctx)
	w.lastErr = err
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	w.verifier = NewFetchVerifier(w.cfg, jwks, w.client)
	return w.verifier, nil
}

// discover reads the issuer's discovery document for its key set's URL.
func (w *Workload) discover(ctx context.Context) (string, error) {
	b, err := get(ctx, w.client, strings.TrimSuffix(w.s.Issuer, "/")+"/.well-known/openid-configuration")
	if err != nil {
		return "", fmt.Errorf("read the discovery document: %w", err)
	}
	var e endpoints
	if err := json.Unmarshal(b, &e); err != nil {
		return "", fmt.Errorf("the discovery document is not JSON: %w", err)
	}
	if e.Issuer != w.s.Issuer {
		return "", fmt.Errorf("the discovery document names issuer %q, not %q", e.Issuer, w.s.Issuer)
	}
	if err := secureURL(e.JWKSURI); err != nil {
		return "", fmt.Errorf("the discovery document's jwks_uri %v", err)
	}
	return e.JWKSURI, nil
}

// Verify checks raw, a token of the provider's, and returns its claims:
// its signature, issuer, times and age, and its audience, bound to the
// key whose thumbprint is jkt where the kind binds it. A refusal wraps
// ErrInvalidToken or ErrUnavailable.
func (w *Workload) Verify(ctx context.Context, raw, jkt string) (*Claims, error) {
	v, err := w.verifierFor(ctx)
	if err != nil {
		return nil, err
	}
	want := Expect{}
	if w.kind.bound {
		want.Audience = BoundAudience(w.audience, jkt)
	}
	return v.Verify(ctx, raw, want)
}

// Status reports the provider's keys, as a provider of login: does.
func (w *Workload) Status() Status {
	w.mu.Lock()
	s := Status{Name: w.s.Name, Kind: w.s.Kind, Issuer: w.s.Issuer, Discovery: DiscoveryFetch, Discovered: w.verifier != nil}
	if w.s.Discovery == DiscoveryStatic {
		s.Discovery = DiscoveryStatic
	}
	if w.lastErr != nil {
		s.Err = w.lastErr.Error()
	}
	v := w.verifier
	w.mu.Unlock()
	if v != nil {
		s.Keys = v.Status()
	} else {
		s.Keys.Source = SourceFetch
	}
	return s
}

// PeekIssuer returns the iss of raw, a compact JWS, without verifying
// anything: which provider's verifier to give it to. Nothing else of what
// it returns is to be trusted.
func PeekIssuer(raw string) (string, error) {
	j, err := jose.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	var c struct {
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(j.Payload, &c); err != nil || c.Iss == "" {
		return "", fmt.Errorf("%w: the token names no issuer", ErrInvalidToken)
	}
	return c.Iss, nil
}

// The bounds of FlatClaims.
const (
	maxClaimDepth = 4
	maxFlatClaims = 256
)

// FlatClaims are the claims of all whose values are strings, each by its
// name, and a nested one by the path to it with dots, as
// google.compute_engine.project_id: what a trust rule's claims are
// matched against. A name that two paths would both give, such as a
// claim named a.b beside a claim a holding b, is left out, so that
// neither is taken for the other; past 4 levels or 256 claims, the rest
// is left out too.
func FlatClaims(all map[string]any) map[string]string {
	out := map[string]string{}
	twice := map[string]bool{}
	var walk func(prefix string, m map[string]any, depth int)
	walk = func(prefix string, m map[string]any, depth int) {
		for _, k := range slices.Sorted(maps.Keys(m)) {
			name := prefix + k
			switch v := m[k].(type) {
			case string:
				if _, dup := out[name]; dup || twice[name] {
					delete(out, name)
					twice[name] = true
					continue
				}
				if len(out) < maxFlatClaims {
					out[name] = v
				}
			case map[string]any:
				if depth < maxClaimDepth {
					walk(name+".", v, depth+1)
				}
			}
		}
	}
	walk("", all, 1)
	return out
}
