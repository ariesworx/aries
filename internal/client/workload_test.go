package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
)

// aJWT reads as a compact JWT, which is all aries checks.
const aJWT = "eyJhbGciOiJFUzI1NiJ9.eyJpc3MiOiJ4In0.c2ln"

func envOf(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestPlatformTokenGitHub(t *testing.T) {
	t.Parallel()
	var got *http.Request
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		_ = json.NewEncoder(w).Encode(map[string]string{"value": aJWT})
	}))
	t.Cleanup(srv.Close)
	env := WorkloadEnv{Client: srv.Client(), Getenv: envOf(map[string]string{
		"ACTIONS_ID_TOKEN_REQUEST_URL": srv.URL + "/token?api-version=2.0", "ACTIONS_ID_TOKEN_REQUEST_TOKEN": "runtime-token"})}
	tok, err := PlatformToken(t.Context(), WorkloadGitHubActions, "https://aries.example.com/n/jkt", env)
	if err != nil || tok != aJWT {
		t.Fatalf("PlatformToken(github-actions) = %q, %v", tok, err)
	}
	if got.URL.Query().Get("audience") != "https://aries.example.com/n/jkt" || got.URL.Query().Get("api-version") != "2.0" ||
		got.Header.Get("Authorization") != "bearer runtime-token" {
		t.Errorf("the request = %s %v, want the audience added to the URL's query, and the runtime's token as a bearer", got.URL, got.Header)
	}

	// No id-token permission: the variables are missing.
	_, err = PlatformToken(t.Context(), WorkloadGitHubActions, "x", WorkloadEnv{Getenv: envOf(nil)})
	if pe, ok := err.(*proto.Error); !ok || pe.Code != proto.CodeInvalid || !strings.Contains(pe.Fix, "id-token: write") {
		t.Errorf("PlatformToken(no permission) = %v, want a fix naming id-token: write", err)
	}
	// Plain http to another machine is refused.
	insecure := map[string]string{"ACTIONS_ID_TOKEN_REQUEST_URL": "http://token.example.com/x", "ACTIONS_ID_TOKEN_REQUEST_TOKEN": "t"} //nolint:gosec // a test's stand-in, no credential
	_, err = PlatformToken(t.Context(), WorkloadGitHubActions, "x", WorkloadEnv{Getenv: envOf(insecure)})
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("PlatformToken(http) = %v, want refused", err)
	}
}

func TestPlatformTokenGoogle(t *testing.T) {
	t.Parallel()
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		_, _ = w.Write([]byte(aJWT + "\n"))
	}))
	t.Cleanup(srv.Close)
	env := WorkloadEnv{Getenv: envOf(map[string]string{"GCE_METADATA_HOST": strings.TrimPrefix(srv.URL, "http://")})}
	tok, err := PlatformToken(t.Context(), WorkloadGoogle, "https://aries.example.com/n/jkt", env)
	if err != nil || tok != aJWT {
		t.Fatalf("PlatformToken(google) = %q, %v", tok, err)
	}
	if got.URL.Path != googleIdentityPath || got.URL.Query().Get("audience") != "https://aries.example.com/n/jkt" ||
		got.URL.Query().Get("format") != "full" || got.Header.Get("Metadata-Flavor") != "Google" {
		t.Errorf("the request = %s %v", got.URL, got.Header)
	}
}

func TestPlatformTokenAzure(t *testing.T) {
	t.Parallel()
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": aJWT, "token_type": "Bearer"})
	}))
	t.Cleanup(srv.Close)
	env := WorkloadEnv{Getenv: envOf(map[string]string{"IDENTITY_ENDPOINT": srv.URL + "/msi/token", "IDENTITY_HEADER": "app-service-secret"})}
	tok, err := PlatformToken(t.Context(), WorkloadAzure, "api://aries.example.com", env)
	if err != nil || tok != aJWT {
		t.Fatalf("PlatformToken(azure) = %q, %v", tok, err)
	}
	if got.URL.Query().Get("resource") != "api://aries.example.com" || got.Header.Get("X-IDENTITY-HEADER") != "app-service-secret" {
		t.Errorf("the request = %s %v", got.URL, got.Header)
	}
}

func TestPlatformTokenRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"a refusal", http.StatusForbidden, `{"value":"x"}`},
		{"not JSON", http.StatusOK, `token`},
		{"not a JWT", http.StatusOK, `{"value":"not a token"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			env := WorkloadEnv{Client: srv.Client(), Getenv: envOf(map[string]string{
				"ACTIONS_ID_TOKEN_REQUEST_URL": srv.URL + "/t?sig=secret", "ACTIONS_ID_TOKEN_REQUEST_TOKEN": "t"})}
			_, err := PlatformToken(t.Context(), WorkloadGitHubActions, "a", env)
			if pe, ok := err.(*proto.Error); !ok || pe.Code != proto.CodeUnavailable || pe.Fix == "" || strings.Contains(pe.Message, "secret") {
				t.Errorf("PlatformToken(%s) = %v, want unavailable, with a fix, and no query", tc.name, err)
			}
		})
	}
	if _, err := PlatformToken(t.Context(), "kubernetes", "a", WorkloadEnv{Getenv: envOf(nil)}); err == nil || !strings.Contains(err.Error(), "no workload kind") {
		t.Errorf("PlatformToken(kubernetes) = %v", err)
	}
}

func TestChooseWorkload(t *testing.T) {
	t.Parallel()
	gha := WorkloadInfo{Name: "github-actions", Kind: WorkloadGitHubActions, Audience: "https://aries.example.com", Bound: true}
	ghes := WorkloadInfo{Name: "ghes", Kind: WorkloadGitHubActions, Audience: "https://aries.example.com", Bound: true}
	az := WorkloadInfo{Name: "azure", Kind: WorkloadAzure, Audience: "api://aries.example.com"}
	for _, tc := range []struct {
		name     string
		have     []WorkloadInfo
		kind     string
		provider string
		want     string // a provider's name, or what the refusal names
		ok       bool
	}{
		{"the one of its kind", []WorkloadInfo{gha, az}, WorkloadAzure, "", "azure", true},
		{"named", []WorkloadInfo{gha, ghes}, WorkloadGitHubActions, "ghes", "ghes", true},
		{"two of a kind", []WorkloadInfo{gha, ghes}, WorkloadGitHubActions, "", "--provider", false},
		{"none of the kind", []WorkloadInfo{gha}, WorkloadGoogle, "", "github-actions (github-actions)", false},
		{"none at all", nil, WorkloadGoogle, "", "workload: providers", false},
	} {
		w, err := chooseWorkload(&ServerInfo{Workloads: tc.have}, tc.kind, tc.provider)
		if tc.ok && (err != nil || w.Name != tc.want) || !tc.ok && (err == nil || !strings.Contains(err.Error()+err.(*proto.Error).Fix, tc.want)) {
			t.Errorf("chooseWorkload(%s) = %+v, %v; want %q", tc.name, w, err, tc.want)
		}
	}
}
