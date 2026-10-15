package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/spike/identity/oidc"
	"github.com/ariesworx/aries/spike/identity/trust"
)

// cmdWorkload stands in for a CI job signing in (docs/design/identity.md §7):
// Keycloak's client-credentials grant plays the platform that gives the
// job a token (GitHub Actions, Google Cloud, Azure), with the claims of a
// GitHub Actions token made up by mappers. The token goes through the
// same verifier as a person's, with the server's own name as audience,
// and a trust rule maps it to a service principal.
func cmdWorkload(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("workload", flag.ExitOnError)
	issuer := fs.String("issuer", "http://127.0.0.1:18080/realms/"+realm, "the platform's issuer")
	client := fs.String("client", "ci-example", "the workload's client id at the stand-in platform")
	audience := fs.String("audience", "aries.example.com", "the audience the server pins for workloads")
	state := fs.String("state", "", "the spike's state directory")
	_ = fs.Parse(args)
	if *state == "" {
		return errors.New("-state is required")
	}
	secret, err := readSecret(filepath.Join(*state, *client+".secret"))
	if err != nil {
		return err
	}
	hc := &http.Client{Timeout: 15 * time.Second}
	d, err := oidc.Discover(ctx, hc, *issuer)
	if err != nil {
		return err
	}

	// The platform's side: the job asks for its token.
	var out struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := postForm(ctx, hc, d.TokenEndpoint, url.Values{
		"grant_type": {"client_credentials"}, "client_id": {*client}, "client_secret": {secret},
	}, &out); err != nil {
		return fmt.Errorf("client credentials: %w", err)
	}
	fmt.Printf("workload: token from the stand-in platform, expires in %ds, ID token too: %v\n", out.ExpiresIn, out.IDToken != "")
	describe(out.AccessToken, "")

	// The server's side.
	v := oidc.New(oidc.Config{Issuer: *issuer, Audience: *audience, Skew: 30 * time.Second, MaxAge: 10 * time.Minute, Refetch: time.Minute},
		d, hc, time.Now)
	c, err := v.Verify(ctx, out.AccessToken, oidc.Expect{})
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	fmt.Printf("workload: verified: iss %s, aud %v, azp %q, repository_id %v, ref %v, run_id %v, actor %v\n",
		c.Iss, []string(c.Aud), c.Azp, c.All["repository_id"], c.All["ref"], c.All["run_id"], c.All["actor"])

	rules := []trust.Rule{
		{Name: "app-main", Principal: "ci-example", Issuer: *issuer, Audience: *audience,
			Claims: map[string]string{"repository_id": "123456789", "ref": "refs/heads/main"}},
		{Name: "other-repo", Principal: "ci-other", Issuer: *issuer, Audience: *audience,
			Claims: map[string]string{"repository_id": "555555555"}},
	}
	for _, r := range rules {
		if err := r.Check(); err != nil {
			return err
		}
	}
	bad := trust.Rule{Name: "sub-only", Principal: "ci-example", Issuer: *issuer, Audience: *audience,
		Claims: map[string]string{"sub": c.Sub}}
	fmt.Printf("workload: a rule that pins only sub: %v\n", bad.Check())

	principal, rule, err := trust.Match(rules, c.Iss, c.Aud, c.All)
	if err != nil {
		return fmt.Errorf("trust: %w", err)
	}
	pubRaw, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	pub, _ := ssh.NewPublicKey(pubRaw)
	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	fmt.Printf("workload: rule %s maps it to %s; key %s for one hour: %s\n", rule, principal, ssh.FingerprintSHA256(pub), shorten(keyLine("/usr/local/bin/ariesd", principal, pub, exp)))

	// The same token, checked as a person's ID token for aries-cli, and
	// against a rule for another repository only.
	people := oidc.New(oidc.Config{Issuer: *issuer, Audience: "aries-cli", IDToken: true, Skew: 30 * time.Second, MaxAge: 10 * time.Minute, Refetch: time.Minute},
		d, hc, time.Now)
	_, err = people.Verify(ctx, out.AccessToken, oidc.Expect{})
	fmt.Printf("workload: the token at the people's verifier: %v\n", err)
	_, _, err = trust.Match(rules[1:], c.Iss, c.Aud, c.All)
	fmt.Printf("workload: against only the other repository's rule: %v\n", err)
	return nil
}
