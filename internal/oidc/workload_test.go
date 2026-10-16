package oidc

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/oidc/oidctest"
)

const (
	serverAudience = "https://aries.example.com"
	jobKey         = "thumbprint-of-the-jobs-key-0000000000000000"
)

// keyFile writes iss's key set to a file, as an operator would copy it.
func keyFile(t *testing.T, iss *oidctest.Issuer) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "jwks.json")
	if err := os.WriteFile(p, iss.JWKS(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// workloadToken is a token of iss for aud, issued at now, with claims.
func workloadToken(iss *oidctest.Issuer, aud string, now time.Time, claims map[string]any) string {
	c := map[string]any{"iss": iss.URL, "aud": aud, "sub": "repo:example/app:ref:refs/heads/main", "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"jti": "00000000-0000-4000-8000-000000000001"}
	maps.Copy(c, claims)
	return iss.Sign(c)
}

func TestOpenWorkload(t *testing.T) {
	t.Parallel()
	iss := oidctest.New(t)
	file := keyFile(t, iss)
	for _, tc := range []struct {
		name string
		s    WorkloadSettings
		want string // in the refusal; "" for none
	}{
		{"github-actions", WorkloadSettings{Name: "github-actions", Kind: KindGitHubActions, Issuer: iss.URL}, ""},
		{"azure", WorkloadSettings{Name: "azure", Kind: KindAzure, Issuer: iss.URL, Audience: "api://aries.example.com"}, ""},
		{"a key set file", WorkloadSettings{Name: "gcp", Kind: KindGoogle, Issuer: iss.URL, Discovery: DiscoveryStatic, JWKSFile: file}, ""},
		{"a name", WorkloadSettings{Name: "GitHub", Kind: KindGitHubActions, Issuer: iss.URL}, "lowercase letter"},
		{"a kind", WorkloadSettings{Name: "k8s", Kind: "kubernetes", Issuer: iss.URL}, "azure, github-actions, google"},
		{"no issuer", WorkloadSettings{Name: "gha", Kind: KindGitHubActions}, "issuer is required"},
		{"plain http", WorkloadSettings{Name: "gha", Kind: KindGitHubActions, Issuer: "http://issuer.example.com"}, "not https"},
		{"an audience of its own", WorkloadSettings{Name: "gha", Kind: KindGitHubActions, Issuer: iss.URL, Audience: "x"}, "bound to its key"},
		{"azure without an audience", WorkloadSettings{Name: "azure", Kind: KindAzure, Issuer: iss.URL}, "Application ID URI"},
		{"static with no keys", WorkloadSettings{Name: "gha", Kind: KindGitHubActions, Issuer: iss.URL, Discovery: DiscoveryStatic}, "one of them"},
		{"fetch with a key file", WorkloadSettings{Name: "gha", Kind: KindGitHubActions, Issuer: iss.URL, JWKSFile: file}, "discovery: static"},
		{"a discovery", WorkloadSettings{Name: "gha", Kind: KindGitHubActions, Issuer: iss.URL, Discovery: "maybe"}, "not fetch or static"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := OpenWorkload(tc.s, serverAudience, options(newClock()))
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Errorf("OpenWorkload(%s) = %v, want %q", tc.name, err, tc.want)
			}
		})
	}
}

func TestWorkloadVerify(t *testing.T) {
	t.Parallel()
	iss := oidctest.New(t)
	clk := newClock()
	o := options(clk)
	o.MaxAge = 2 * time.Minute
	gha, err := OpenWorkload(WorkloadSettings{Name: "github-actions", Kind: KindGitHubActions, Issuer: iss.URL, CAFile: iss.CAFile}, serverAudience, o)
	if err != nil {
		t.Fatal(err)
	}
	if !gha.Bound() || gha.Audience() != serverAudience || gha.Kind() != KindGitHubActions || gha.Issuer() != iss.URL {
		t.Errorf("gha = bound %t, audience %q", gha.Bound(), gha.Audience())
	}
	claims := map[string]any{"repository_id": "123456789", "repository_owner_id": "111111111"}
	bound := BoundAudience(serverAudience, jobKey)
	c, err := gha.Verify(t.Context(), workloadToken(iss, bound, clk.Now(), claims), jobKey)
	if err != nil || c.String("repository_id") != "123456789" {
		t.Fatalf("Verify(a token bound to the job's key) = %+v, %v", c, err)
	}
	if n := iss.Discovery.Load(); n != 1 {
		t.Errorf("discovery read %d times, want once", n)
	}
	for _, tc := range []struct {
		name string
		tok  string
		jkt  string
	}{
		{"the audience unbound", workloadToken(iss, serverAudience, clk.Now(), claims), jobKey},
		{"bound to another key", workloadToken(iss, BoundAudience(serverAudience, "another"), clk.Now(), claims), jobKey},
		{"presented with another key", workloadToken(iss, bound, clk.Now(), claims), "another"},
		{"older than two minutes", workloadToken(iss, bound, clk.Now().Add(-3*time.Minute), claims), jobKey},
		{"another issuer", func() string {
			return iss.Sign(map[string]any{"iss": "https://issuer.example.com", "aud": bound, "iat": clk.Now().Unix(), "exp": clk.Now().Add(time.Minute).Unix()})
		}(), jobKey},
	} {
		if _, err := gha.Verify(t.Context(), tc.tok, tc.jkt); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("Verify(%s) = %v, want ErrInvalidToken", tc.name, err)
		}
	}

	// Azure's audience is fixed, and bound to nothing.
	az, err := OpenWorkload(WorkloadSettings{Name: "azure", Kind: KindAzure, Issuer: iss.URL, Audience: "api://aries.example.com",
		Discovery: DiscoveryStatic, JWKSFile: keyFile(t, iss)}, serverAudience, o)
	if err != nil {
		t.Fatal(err)
	}
	if az.Bound() {
		t.Error("azure is bound")
	}
	if _, err := az.Verify(t.Context(), workloadToken(iss, "api://aries.example.com", clk.Now(), nil), jobKey); err != nil {
		t.Errorf("Verify(azure) = %v", err)
	}
	if _, err := az.Verify(t.Context(), workloadToken(iss, serverAudience, clk.Now(), nil), jobKey); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Verify(azure, the server's audience) = %v, want ErrInvalidToken", err)
	}
	if s := az.Status(); s.Name != "azure" || s.Discovery != DiscoveryStatic || !s.Discovered || s.Keys.Source != SourceFile {
		t.Errorf("Status = %+v", s)
	}
}

// TestWorkloadUnreachable: an issuer that cannot be read refuses as
// unavailable, and is not asked again within Refetch.
func TestWorkloadUnreachable(t *testing.T) {
	t.Parallel()
	clk := newClock()
	w, err := OpenWorkload(WorkloadSettings{Name: "gha", Kind: KindGitHubActions, Issuer: "https://127.0.0.1:1"}, serverAudience, options(clk))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Verify(t.Context(), "a.b.c", jobKey); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Verify(an issuer down) = %v, want ErrUnavailable", err)
	}
	if _, err := w.Verify(t.Context(), "a.b.c", jobKey); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "ago") {
		t.Errorf("Verify(again at once) = %v, want unavailable without asking again", err)
	}
	if s := w.Status(); s.Discovered || s.Err == "" {
		t.Errorf("Status = %+v, want not discovered, with the error", s)
	}
}

func TestWorkloadKinds(t *testing.T) {
	t.Parallel()
	for kind, want := range map[string][]string{
		KindGitHubActions: {"repository_owner_id", "repository_id"},
		KindGoogle:        {"sub", "email"},
		KindAzure:         {"tid", "oid"},
	} {
		if got := RequiredClaims(kind); !slices.Equal(got, want) {
			t.Errorf("RequiredClaims(%s) = %v, want %v", kind, got, want)
		}
		if len(LaunchClaims(kind)) == 0 {
			t.Errorf("LaunchClaims(%s) is empty", kind)
		}
		for _, c := range RequiredClaims(kind) {
			if c == "sub" && len(RequiredClaims(kind)) == 1 {
				t.Errorf("%s requires sub alone", kind)
			}
		}
	}
	if RequiredClaims("kubernetes") != nil {
		t.Error("RequiredClaims(a kind that is not built) is not nil")
	}
}

func TestFlatClaims(t *testing.T) {
	t.Parallel()
	got := FlatClaims(map[string]any{
		"repository_id": "123456789",
		"run_number":    7.0,
		"admin":         true,
		"google":        map[string]any{"compute_engine": map[string]any{"project_id": "example-project", "zone": "us-east1-b"}},
		"kubernetes.io": map[string]any{"namespace": "ci", "serviceaccount": map[string]any{"uid": "0000"}},
		"a.b":           "flat",
		"a":             map[string]any{"b": "nested"},
		"deep":          map[string]any{"one": map[string]any{"two": map[string]any{"three": map[string]any{"four": "too deep"}}}},
		"groups":        []any{"x"},
	})
	want := map[string]string{
		"repository_id":                    "123456789",
		"google.compute_engine.project_id": "example-project",
		"google.compute_engine.zone":       "us-east1-b",
		"kubernetes.io.namespace":          "ci",
		"kubernetes.io.serviceaccount.uid": "0000",
	}
	if !maps.Equal(got, want) {
		t.Errorf("FlatClaims = %v, want %v", got, want)
	}
	many := map[string]any{}
	for i := range 300 {
		many[strings.Repeat("c", 1+i%40)+string(rune('a'+i%26))+strings.Repeat("x", i/26)] = "v"
	}
	if n := len(FlatClaims(many)); n > maxFlatClaims {
		t.Errorf("FlatClaims(300 claims) kept %d, over %d", n, maxFlatClaims)
	}
}

func TestPeekIssuer(t *testing.T) {
	t.Parallel()
	iss := oidctest.New(t)
	if got, err := PeekIssuer(iss.Sign(map[string]any{"iss": iss.URL})); err != nil || got != iss.URL {
		t.Errorf("PeekIssuer = %q, %v", got, err)
	}
	for _, raw := range []string{"", "a.b.c", iss.Sign(map[string]any{"sub": "x"})} {
		if _, err := PeekIssuer(raw); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("PeekIssuer(%q) = %v, want ErrInvalidToken", raw, err)
		}
	}
}
