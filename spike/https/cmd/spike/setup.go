package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const realm = "aries-spike"

// cmdSetup makes the realm through Keycloak's admin API, over HTTPS with
// the internal CA: two people, a confidential client that is the Aries
// server's own registration (a web application with one redirect to the
// server, not a loopback one: transport.md §4.2), and a client standing
// in for a CI workload. Passwords and secrets go to 0600 files in -state
// and only in request bodies.
func cmdSetup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	kc := fs.String("kc", "https://keycloak.internal:8443", "Keycloak's base URL")
	ca := fs.String("ca", "", "the internal CA's certificate")
	adminFile := fs.String("admin-pass-file", "", "file holding the bootstrap admin's password")
	state := fs.String("state", "", "directory for the generated passwords and secrets")
	aries := fs.String("aries", "https://aries.internal:8443", "the Aries servers' public URLs, comma-separated; the first is the workload audience")
	_ = fs.Parse(args)
	if *adminFile == "" || *state == "" || *ca == "" {
		return errors.New("-ca, -admin-pass-file and -state are required")
	}
	hc, err := httpClient(*ca)
	if err != nil {
		return err
	}
	adminPass, err := readSecret(*adminFile)
	if err != nil {
		return err
	}
	tok, err := adminToken(ctx, hc, *kc, adminPass)
	if err != nil {
		return err
	}
	a := admin{c: hc, base: strings.TrimSuffix(*kc, "/") + "/admin/realms", token: tok}
	alicePass, bobPass, serverSecret, ciSecret := rand.Text(), rand.Text(), rand.Text(), rand.Text()
	for name, v := range map[string]string{"alice.pass": alicePass, "bob.pass": bobPass, "aries-server.secret": serverSecret, "ci-example.secret": ciSecret} {
		if err := os.WriteFile(filepath.Join(*state, name), []byte(v), 0o600); err != nil {
			return err
		}
	}
	steps := []struct {
		what, path string
		body       any
	}{
		{"realm", "", map[string]any{"id": realm, "realm": realm, "enabled": true, "accessTokenLifespan": 300}},
		{"ES256 key", "/" + realm + "/components", map[string]any{
			"name": "ecdsa-p256", "providerId": "ecdsa-generated", "providerType": "org.keycloak.keys.KeyProvider",
			"parentId": realm, "config": map[string][]string{"priority": {"100"}, "ecdsaEllipticCurveKey": {"P-256"}},
		}},
		{"user alice", "/" + realm + "/users", person("alice", "Alice", alicePass)},
		{"user bob", "/" + realm + "/users", person("bob", "Bob", bobPass)},
		{"client aries-server", "/" + realm + "/clients", map[string]any{
			"clientId": "aries-server", "name": "Aries server (spike)", "publicClient": false, "secret": serverSecret,
			"protocol": "openid-connect", "standardFlowEnabled": true, "directAccessGrantsEnabled": false,
			"implicitFlowEnabled": false, "serviceAccountsEnabled": false, "consentRequired": false,
			"redirectUris": callbacks(*aries),
			"attributes": map[string]string{
				"pkce.code.challenge.method":   "S256",
				"id.token.signed.response.alg": "ES256",
				"use.refresh.tokens":           "false",
			},
		}},
		{"client ci-example", "/" + realm + "/clients", map[string]any{
			"clientId": "ci-example", "name": "A CI workload (spike)", "publicClient": false, "secret": ciSecret,
			"protocol": "openid-connect", "standardFlowEnabled": false, "serviceAccountsEnabled": true,
			"attributes": map[string]string{"use.refresh.tokens": "false"},
			"protocolMappers": []map[string]any{
				mapper("aud", "oidc-audience-mapper", map[string]string{"included.custom.audience": strings.Split(*aries, ",")[0], "access.token.claim": "true", "id.token.claim": "false"}),
				hardcoded("repository_owner_id", "111111111"),
				hardcoded("repository_id", "123456789"),
				hardcoded("ref", "refs/heads/main"),
				hardcoded("run_id", "8123"),
			},
		}},
	}
	for _, s := range steps {
		if err := a.post(ctx, s.path, s.body); err != nil {
			return fmt.Errorf("%s: %w", s.what, err)
		}
		fmt.Printf("setup: %s made\n", s.what)
	}
	return nil
}

// cmdJWKS writes the realm's key set to a file, standing in for the
// operator who exports it from the provider and copies it to the
// server, so that the server fetches no keys.
func cmdJWKS(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("jwks", flag.ExitOnError)
	issuer := fs.String("issuer", "https://keycloak.internal:8443/realms/"+realm, "the issuer")
	ca := fs.String("ca", "", "the internal CA's certificate")
	out := fs.String("out", "", "file to write")
	_ = fs.Parse(args)
	hc, err := httpClient(*ca)
	if err != nil {
		return err
	}
	resp, err := hc.Get(*issuer + "/protocol/openid-connect/certs")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		return err
	}
	var set struct {
		Keys []struct{ Kid, Alg, Use string }
	}
	_ = json.Unmarshal(b, &set)
	fmt.Printf("jwks: %d keys written to %s\n", len(set.Keys), filepath.Base(*out))
	return nil
}

// callbacks are the servers' redirects: each server is its own web
// application at the provider, with one exact redirect.
func callbacks(bases string) []string {
	var out []string
	for b := range strings.SplitSeq(bases, ",") {
		out = append(out, b+"/auth/callback")
	}
	return out
}

func person(user, first, pass string) map[string]any {
	return map[string]any{
		"username": user, "email": user + "@example.com", "emailVerified": true, "enabled": true,
		"firstName": first, "lastName": "Example",
		"credentials": []map[string]any{{"type": "password", "value": pass, "temporary": false}},
	}
}

func mapper(name, kind string, cfg map[string]string) map[string]any {
	return map[string]any{"name": name, "protocol": "openid-connect", "protocolMapper": kind, "config": cfg}
}

func hardcoded(claim, value string) map[string]any {
	return mapper(claim, "oidc-hardcoded-claim-mapper", map[string]string{
		"claim.name": claim, "claim.value": value, "jsonType.label": "String",
		"access.token.claim": "true", "id.token.claim": "false",
	})
}

func readSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// httpClient trusts the internal CA only and follows no redirect.
func httpClient(caFile string) (*http.Client, error) {
	cfg, err := clientTLS(caFile, "")
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Timeout:       15 * time.Second,
		Transport:     &http.Transport{TLSClientConfig: cfg, ForceAttemptHTTP2: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

func adminToken(ctx context.Context, c *http.Client, kc, pass string) (string, error) {
	form := url.Values{"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {"admin"}, "password": {pass}}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := postForm(ctx, c, strings.TrimSuffix(kc, "/")+"/realms/master/protocol/openid-connect/token", form, nil, &out); err != nil {
		return "", fmt.Errorf("admin sign-in: %w", err)
	}
	return out.AccessToken, nil
}

type admin struct {
	c           *http.Client
	base, token string
}

func (a admin) post(ctx context.Context, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("POST %s: %s: %s", path, resp.Status, msg)
	}
	return nil
}

// postForm posts form, with extra headers, and decodes a JSON answer.
// An error answer is returned with OAuth's error and description.
func postForm(ctx context.Context, c *http.Client, u string, form url.Values, hdr http.Header, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		e := oauthError{Status: resp.StatusCode}
		_ = json.Unmarshal(b, &e)
		return &e
	}
	return json.Unmarshal(b, out)
}

type oauthError struct {
	Status      int
	Code        string `json:"error"`
	Description string `json:"error_description"`
}

func (e *oauthError) Error() string { return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Description) }
