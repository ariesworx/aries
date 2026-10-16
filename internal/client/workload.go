package client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/dpop"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/safetext"
)

// `aries login --workload KIND` (docs/design/identity.md §7.4,
// transport.md §7): a CI job or a cloud workload signs in as a service
// principal with the token its platform already gives it, and no secret
// stored anywhere. aries makes a DPoP key, asks the platform for a token
// for the server's audience, bound to that key where the platform lets
// the job choose its audience, and exchanges it at the server's token
// endpoint (RFC 8693) for a sign-in of scope ci, kept as `aries login`
// keeps a person's.

// The workload kinds aries fetches a token for, as the server's
// workload providers name them.
const (
	WorkloadGitHubActions = "github-actions"
	WorkloadGoogle        = "google"
	WorkloadAzure         = "azure"
)

// WorkloadKinds are the kinds aries fetches a token for, in order.
var WorkloadKinds = []string{WorkloadAzure, WorkloadGitHubActions, WorkloadGoogle}

// WorkloadInfo is a workload provider of the server, as
// /.well-known/aries names it.
type WorkloadInfo struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Audience string `json:"audience"`
	Bound    bool   `json:"bound"`
}

// The token exchange's names (RFC 8693).
const (
	grantTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange" //nolint:gosec // a URN, not a credential
	tokenTypeJWT       = "urn:ietf:params:oauth:token-type:jwt"            //nolint:gosec // a URN, not a credential
)

// The platforms' endpoints. A metadata server is reached on the
// machine's own link, never through a proxy.
const (
	googleMetadataHost = "metadata.google.internal"
	googleIdentityPath = "/computeMetadata/v1/instance/service-accounts/default/identity"
	azureIMDS          = "http://169.254.169.254/metadata/identity/oauth2/token"
)

// WorkloadEnv is what a workload's sign-in reads of the process: its
// environment, and the HTTP client the platform's token is fetched with,
// nil for the defaults (for a test's stand-in platform).
type WorkloadEnv struct {
	Getenv func(string) string
	Client *http.Client
}

// chooseWorkload is the server's workload provider of kind, named
// provider when it is set.
func chooseWorkload(info *ServerInfo, kind, provider string) (WorkloadInfo, error) {
	var names []string
	var found []WorkloadInfo
	for _, w := range info.Workloads {
		names = append(names, w.Name+" ("+w.Kind+")")
		if w.Kind == kind && (provider == "" || w.Name == provider) {
			found = append(found, w)
		}
	}
	switch {
	case len(info.Workloads) == 0:
		return WorkloadInfo{}, proto.Errf(proto.CodeInvalid,
			"ask the server's operator to set up the platform under workload: providers (docs/server.md, Service principals)",
			"this server signs in no service principal by a workload's token")
	case len(found) == 0:
		return WorkloadInfo{}, proto.Errf(proto.CodeInvalid, "give --workload the kind of one of them, and --provider its name if several share a kind",
			fmt.Sprintf("the server has no workload provider of kind %s%s; it has %s", kind, map[bool]string{true: " named " + provider, false: ""}[provider != ""],
				strings.Join(names, ", ")))
	case len(found) > 1:
		return WorkloadInfo{}, proto.Errf(proto.CodeInvalid, "choose one with --provider NAME",
			fmt.Sprintf("the server has several workload providers of kind %s: %s", kind, strings.Join(names, ", ")))
	}
	return found[0], nil
}

// WorkloadLogin signs this workload in at cfg's server as the service
// principal a trust rule names, by the token of kind's platform, and
// returns the sign-in, which the caller keeps (SaveSignIn). provider
// chooses among the server's providers of kind, when it has several;
// machine names this machine to the server.
func WorkloadLogin(ctx context.Context, cfg *Config, info *ServerInfo, kind, provider, machine string, env WorkloadEnv) (*SignIn, error) {
	base, err := serverURL(cfg)
	if err != nil {
		return nil, err
	}
	w, err := chooseWorkload(info, kind, provider)
	if err != nil {
		return nil, err
	}
	key, err := dpop.NewKey()
	if err != nil {
		return nil, err
	}
	audience := w.Audience
	if w.Bound {
		audience += "/n/" + dpop.Thumbprint(&key.PublicKey)
	}
	subject, err := PlatformToken(ctx, kind, audience, env)
	if err != nil {
		return nil, err
	}
	tokenURL := base + "/oauth/token"
	proof, err := dpop.Proof(key, http.MethodPost, tokenURL, "", time.Now())
	if err != nil {
		return nil, err
	}
	form := url.Values{"grant_type": {grantTokenExchange}, "subject_token": {subject}, "subject_token_type": {tokenTypeJWT},
		"resource": {base}, "scope": {"ci"}, "machine": {machine}}
	var answer struct {
		AccessToken string    `json:"access_token"`
		TokenType   string    `json:"token_type"`
		ExpiresAt   time.Time `json:"expires_at"`
		Scope       string    `json:"scope"`
		Principal   string    `json:"principal"`
		Provider    string    `json:"provider"`
		Grant       string    `json:"grant"`
		TrustRule   string    `json:"trust_rule"`
	}
	if err := postForm(ctx, cfg, base, tokenURL, form, proof, &answer); err != nil {
		return nil, err
	}
	if answer.AccessToken == "" || !strings.EqualFold(answer.TokenType, "DPoP") || answer.ExpiresAt.IsZero() {
		return nil, proto.Errf(proto.CodeVersion, proto.FixUpgrade+" aries to match the server", "the server's answer to the sign-in is not one this aries reads")
	}
	return &SignIn{Server: cfg.Server.URL, Token: answer.AccessToken, ExpiresAt: answer.ExpiresAt.UTC(), Scopes: strings.Fields(answer.Scope),
		Principal: answer.Principal, Provider: answer.Provider, Grant: answer.Grant, TrustRule: answer.TrustRule, Key: key}, nil
}

// PlatformToken asks kind's platform for a token for audience: GitHub
// Actions' OIDC token endpoint, a Google Cloud metadata server's
// identity, or an Azure managed identity's token.
func PlatformToken(ctx context.Context, kind, audience string, env WorkloadEnv) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()
	switch kind {
	case WorkloadGitHubActions:
		return githubToken(ctx, audience, env)
	case WorkloadGoogle:
		return googleToken(ctx, audience, env)
	case WorkloadAzure:
		return azureToken(ctx, audience, env)
	}
	return "", proto.Errf(proto.CodeInvalid, "give --workload one of "+strings.Join(WorkloadKinds, ", "), fmt.Sprintf("%q is no workload kind aries knows", kind))
}

// githubToken asks GitHub Actions for the job's OIDC token, which the
// workflow's permissions must allow.
func githubToken(ctx context.Context, audience string, env WorkloadEnv) (string, error) {
	raw, bearer := env.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL"), env.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	if raw == "" || bearer == "" {
		return "", proto.Errf(proto.CodeInvalid, "give the job `permissions: id-token: write` in the workflow (docs/ci.md)",
			"GitHub Actions gave this job no OIDC token: ACTIONS_ID_TOKEN_REQUEST_URL is not set")
	}
	u, err := url.Parse(raw)
	secure := err == nil && (u.Scheme == "https" || u.Scheme == "http" && localHost(u.Hostname()))
	if !secure {
		return "", proto.Errf(proto.CodeInvalid, "run aries in a GitHub Actions job, which sets it", "ACTIONS_ID_TOKEN_REQUEST_URL is not an https URL")
	}
	q := u.Query()
	q.Set("audience", audience)
	u.RawQuery = q.Encode()
	var answer struct {
		Value string `json:"value"`
	}
	if err := platformGet(ctx, env, true, u.String(), http.Header{"Authorization": {"bearer " + bearer}}, &answer, "GitHub Actions"); err != nil {
		return "", err
	}
	return platformJWT(answer.Value, "GitHub Actions")
}

// googleToken asks the metadata server of Compute Engine, GKE or Cloud
// Run for the service account's ID token, with every claim (format
// full). GCE_METADATA_HOST names another metadata server, as Google's
// own libraries take it.
func googleToken(ctx context.Context, audience string, env WorkloadEnv) (string, error) {
	host := env.Getenv("GCE_METADATA_HOST")
	if host == "" {
		host = googleMetadataHost
	}
	u := "http://" + host + googleIdentityPath + "?" + url.Values{"audience": {audience}, "format": {"full"}}.Encode()
	var raw string
	if err := platformGet(ctx, env, false, u, http.Header{"Metadata-Flavor": {"Google"}}, &raw, "the Google Cloud metadata server"); err != nil {
		return "", err
	}
	return platformJWT(raw, "the Google Cloud metadata server")
}

// azureToken asks for an Azure managed identity's token for audience,
// the app registration that stands for the server: from the instance
// metadata service of a virtual machine, or from the endpoint App
// Service, Functions and Container Apps name in IDENTITY_ENDPOINT, with
// IDENTITY_HEADER.
func azureToken(ctx context.Context, audience string, env WorkloadEnv) (string, error) {
	u, h := azureIMDS+"?"+url.Values{"api-version": {"2018-02-01"}, "resource": {audience}}.Encode(), http.Header{"Metadata": {"true"}}
	if ep, secret := env.Getenv("IDENTITY_ENDPOINT"), env.Getenv("IDENTITY_HEADER"); ep != "" && secret != "" {
		u, h = ep+"?"+url.Values{"api-version": {"2019-08-01"}, "resource": {audience}}.Encode(), http.Header{"X-IDENTITY-HEADER": {secret}}
	}
	var answer struct {
		AccessToken string `json:"access_token"`
	}
	if err := platformGet(ctx, env, false, u, h, &answer, "the Azure managed identity endpoint"); err != nil {
		return "", err
	}
	return platformJWT(answer.AccessToken, "the Azure managed identity endpoint")
}

// platformGet reads u, with header h, as JSON into out, or as a string
// when out is a *string. A metadata server is reached directly; GitHub's
// endpoint through the environment's proxy, when proxied.
func platformGet(ctx context.Context, env WorkloadEnv, proxied bool, u string, h http.Header, out any, what string) error {
	c := env.Client
	if c == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		tr.Proxy = nil
		if proxied {
			tr.Proxy = http.ProxyFromEnvironment
		}
		c = &http.Client{Transport: tr, Timeout: loginTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	for k, v := range h {
		req.Header[k] = v
	}
	resp, err := c.Do(req)
	if err != nil {
		return proto.Errf(proto.CodeUnavailable, "run aries where the platform gives the workload a token, and retry",
			fmt.Sprintf("cannot reach %s: %v", what, redactQuery(err)))
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if resp.StatusCode != http.StatusOK {
		return proto.Errf(proto.CodeUnavailable, "check that the workload has an identity the platform issues tokens for, and retry",
			fmt.Sprintf("%s answered %s", what, resp.Status))
	}
	if s, ok := out.(*string); ok {
		*s = strings.TrimSpace(string(b))
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return proto.Errf(proto.CodeUnavailable, "retry; if it persists, the platform's answer has changed and aries needs an upgrade",
			fmt.Sprintf("%s's answer is not JSON aries reads", what))
	}
	return nil
}

// platformJWT is tok if it reads as a compact JWT, which is all aries
// checks of it: the server verifies it.
func platformJWT(tok, what string) (string, error) {
	if strings.Count(tok, ".") != 2 || len(tok) > 16<<10 || !safetext.ValidLine(tok) || strings.ContainsAny(tok, " \t") {
		return "", proto.Errf(proto.CodeUnavailable, "retry; if it persists, the platform's answer has changed and aries needs an upgrade",
			fmt.Sprintf("%s gave no token aries reads", what))
	}
	return tok, nil
}

// redactQuery is err with any URL's query left out: a platform's URL may
// carry what is not for a terminal or a log.
func redactQuery(err error) string {
	if ue, ok := err.(*url.Error); ok {
		if u, perr := url.Parse(ue.URL); perr == nil {
			u.RawQuery = ""
			return fmt.Sprintf("%s %s: %v", ue.Op, u, ue.Err)
		}
	}
	return err.Error()
}

// localHost reports whether host is this machine, where plain http is
// allowed for a test's stand-in.
func localHost(host string) bool {
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}
