package trust

import "testing"

const gha = "https://token.actions.githubusercontent.com"

func TestCheck(t *testing.T) {
	t.Parallel()
	ok := Rule{Name: "r", Principal: "ci-example", Issuer: gha, Audience: "aries.example.com",
		Claims: map[string]string{"repository_id": "123456789", "ref": "refs/heads/main"}}
	tests := []struct {
		name   string
		mutate func(*Rule)
		ok     bool
	}{
		{"good", func(*Rule) {}, true},
		{"no claims", func(r *Rule) { r.Claims = nil }, false},
		{"only sub", func(r *Rule) { r.Claims = map[string]string{"sub": "repo:example/app:ref:refs/heads/main"} }, false},
		{"sub and an id", func(r *Rule) { r.Claims = map[string]string{"sub": "x", "repository_id": "1"} }, true},
		{"a pattern", func(r *Rule) { r.Claims = map[string]string{"repository_id": "1", "ref": "refs/heads/*"} }, false},
		{"no audience", func(r *Rule) { r.Audience = "" }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := ok
			r.Claims = map[string]string{"repository_id": "123456789", "ref": "refs/heads/main"}
			tc.mutate(&r)
			if err := r.Check(); (err == nil) != tc.ok {
				t.Errorf("Check(%s) = %v, want ok %v", tc.name, err, tc.ok)
			}
		})
	}
}

func TestMatch(t *testing.T) {
	t.Parallel()
	rules := []Rule{
		{Name: "main", Principal: "ci-example", Issuer: gha, Audience: "aries.example.com",
			Claims: map[string]string{"repository_id": "123456789", "ref": "refs/heads/main"}},
		{Name: "release", Principal: "release-example", Issuer: gha, Audience: "aries.example.com",
			Claims: map[string]string{"repository_id": "123456789", "environment": "release"}},
	}
	aud := []string{"aries.example.com"}
	tests := []struct {
		name   string
		iss    string
		aud    []string
		claims map[string]any
		want   string // "" for a refusal
	}{
		{"main", gha, aud, map[string]any{"repository_id": "123456789", "ref": "refs/heads/main"}, "ci-example"},
		{"a fork's branch", gha, aud, map[string]any{"repository_id": "987654321", "ref": "refs/heads/main"}, ""},
		{"another branch", gha, aud, map[string]any{"repository_id": "123456789", "ref": "refs/heads/dev"}, ""},
		{"id as a number", gha, aud, map[string]any{"repository_id": 123456789.0, "ref": "refs/heads/main"}, ""},
		{"another audience", gha, []string{"other.example.com"}, map[string]any{"repository_id": "123456789", "ref": "refs/heads/main"}, ""},
		{"another issuer", "https://evil.example.com", aud, map[string]any{"repository_id": "123456789", "ref": "refs/heads/main"}, ""},
		{"two principals", gha, aud, map[string]any{"repository_id": "123456789", "ref": "refs/heads/main", "environment": "release"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, _, err := Match(rules, tc.iss, tc.aud, tc.claims)
			if got != tc.want || (tc.want == "") != (err != nil) {
				t.Errorf("Match(%s) = %q, %v, want %q", tc.name, got, err, tc.want)
			}
		})
	}
}
