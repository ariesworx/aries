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
)

const realm = "aries-spike"

// cmdSetup makes the spike's realm through Keycloak's admin API: an ES256
// signing key beside the default RS256 one, two people, a public client
// for `aries login` and a confidential one standing in for a workload.
// Passwords and the client secret are made here, written to 0600 files
// in -state, and sent only in request bodies.
func cmdSetup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	kc := fs.String("kc", "http://127.0.0.1:18080", "Keycloak's base URL")
	adminFile := fs.String("admin-pass-file", "", "file holding the bootstrap admin's password")
	state := fs.String("state", "", "directory for the generated passwords and secret (0700)")
	_ = fs.Parse(args)
	if *adminFile == "" || *state == "" {
		return errors.New("-admin-pass-file and -state are required")
	}
	adminPass, err := readSecret(*adminFile)
	if err != nil {
		return err
	}
	tok, err := adminToken(ctx, *kc, adminPass)
	if err != nil {
		return err
	}
	a := admin{base: strings.TrimSuffix(*kc, "/") + "/admin/realms", token: tok}

	alicePass, bobPass, ciSecret := random(), random(), random()
	for name, v := range map[string]string{"alice.pass": alicePass, "bob.pass": bobPass, "ci-example.secret": ciSecret} {
		if err := os.WriteFile(filepath.Join(*state, name), []byte(v), 0o600); err != nil {
			return err
		}
	}

	steps := []struct {
		what string
		path string
		body any
	}{
		{"realm", "", map[string]any{
			"id": realm, "realm": realm, "enabled": true,
			"accessTokenLifespan":      300, // five minutes: a token is spent at once
			"oauth2DeviceCodeLifespan": 600, "oauth2DevicePollingInterval": 2,
		}},
		{"ES256 key", "/" + realm + "/components", map[string]any{
			"name": "ecdsa-p256", "providerId": "ecdsa-generated", "providerType": "org.keycloak.keys.KeyProvider",
			"parentId": realm, "config": map[string][]string{"priority": {"100"}, "ecdsaEllipticCurveKey": {"P-256"}},
		}},
		{"user alice", "/" + realm + "/users", person("alice", "Alice", alicePass)},
		{"user bob", "/" + realm + "/users", person("bob", "Bob", bobPass)},
		{"client aries-cli", "/" + realm + "/clients", map[string]any{
			"clientId": "aries-cli", "name": "Aries (spike)", "publicClient": true, "protocol": "openid-connect",
			"standardFlowEnabled": true, "directAccessGrantsEnabled": false, "implicitFlowEnabled": false,
			"serviceAccountsEnabled": false, "consentRequired": false,
			// Loopback on any port (RFC 8252 §7.3), and the provider's own
			// page that shows the code, for the paste path.
			"redirectUris": []string{"http://127.0.0.1/*", "urn:ietf:wg:oauth:2.0:oob"},
			"attributes": map[string]string{
				"pkce.code.challenge.method":                "S256",
				"oauth2.device.authorization.grant.enabled": "true",
				"id.token.signed.response.alg":              "ES256",
				"use.refresh.tokens":                        "false",
			},
		}},
		{"client ci-example", "/" + realm + "/clients", map[string]any{
			"clientId": "ci-example", "name": "A CI workload (spike)", "publicClient": false, "secret": ciSecret,
			"protocol": "openid-connect", "standardFlowEnabled": false, "directAccessGrantsEnabled": false,
			"serviceAccountsEnabled": true,
			"attributes":             map[string]string{"use.refresh.tokens": "false"},
			// What a GitHub Actions token carries, made up: the server's
			// name as audience, and the repository's id and ref.
			"protocolMappers": []map[string]any{
				mapper("aud", "oidc-audience-mapper", map[string]string{
					"included.custom.audience": "aries.example.com", "access.token.claim": "true", "id.token.claim": "false",
				}),
				hardcoded("repository_id", "123456789"),
				hardcoded("repository", "example/app"),
				hardcoded("ref", "refs/heads/main"),
				hardcoded("workflow_ref", "example/app/.github/workflows/ci.yml@refs/heads/main"),
				hardcoded("run_id", "8123"),
				hardcoded("actor", "bob"),
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
		"access.token.claim": "true", "id.token.claim": "false", "introspection.token.claim": "true",
	})
}

func random() string { return rand.Text() }

func readSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func adminToken(ctx context.Context, kc, pass string) (string, error) {
	form := url.Values{"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {"admin"}, "password": {pass}}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := postForm(ctx, http.DefaultClient, strings.TrimSuffix(kc, "/")+"/realms/master/protocol/openid-connect/token", form, &out); err != nil {
		return "", fmt.Errorf("admin sign-in: %w", err)
	}
	return out.AccessToken, nil
}

type admin struct{ base, token string }

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
	resp, err := http.DefaultClient.Do(req)
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

// postForm posts form and decodes a JSON answer into out. An error answer
// is returned with OAuth's error and description, which carry no secret.
func postForm(ctx context.Context, c *http.Client, u string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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
		var e oauthError
		_ = json.Unmarshal(b, &e)
		e.Status = resp.StatusCode
		return &e
	}
	return json.Unmarshal(b, out)
}

type oauthError struct {
	Status      int
	Code        string `json:"error"`
	Description string `json:"error_description"`
}

func (e *oauthError) Error() string {
	return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Description)
}
