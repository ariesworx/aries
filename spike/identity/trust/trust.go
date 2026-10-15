// Package trust maps a verified workload token to a service principal
// (docs/design/identity.md §7.2). A rule names an issuer, the audience the token must
// carry, and claims that must equal given values; a token gets the
// principal of the one rule it matches. A rule that pins only the
// subject is refused, because a subject's form differs by platform and on
// some it is built from names that can be renamed or recreated.
package trust

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Rule is one trust rule, as `aries admin trust add` would write it.
type Rule struct {
	Name      string
	Principal string
	Issuer    string
	Audience  string
	// Claims must each equal the token's claim of that name, compared
	// as strings; a claim that is absent or not a string never matches.
	Claims map[string]string
}

// maxClaims bounds the claims of one rule.
const maxClaims = 16

// Check refuses a rule that could match more than its author meant.
func (r Rule) Check() error {
	switch {
	case r.Name == "" || r.Principal == "":
		return errors.New("a rule needs a name and a principal")
	case r.Issuer == "" || r.Audience == "":
		return fmt.Errorf("rule %s: an issuer and an audience are required", r.Name)
	case len(r.Claims) == 0:
		return fmt.Errorf("rule %s: no claims: it would admit every token the issuer signs for this audience", r.Name)
	case len(r.Claims) > maxClaims:
		return fmt.Errorf("rule %s: %d claims, over %d", r.Name, len(r.Claims), maxClaims)
	}
	if _, onlySub := r.Claims["sub"]; onlySub && len(r.Claims) == 1 {
		return fmt.Errorf("rule %s: pins only sub; pin the platform's immutable ids (such as repository_id) as well", r.Name)
	}
	for k, v := range r.Claims {
		if k == "" || v == "" || strings.ContainsAny(v, "*?") {
			return fmt.Errorf("rule %s: claim %q: a value is matched exactly, and may not be empty or a pattern", r.Name, k)
		}
	}
	return nil
}

// Match returns the principal of the one rule that iss, aud and claims
// satisfy. No match is a refusal; two rules for different principals
// matching the same token is a refusal too, since the server must not
// choose between them.
func Match(rules []Rule, iss string, aud []string, claims map[string]any) (principal, rule string, err error) {
	var hits []Rule
	for _, r := range rules {
		if r.Issuer != iss || !slices.Contains(aud, r.Audience) {
			continue
		}
		if matches(r.Claims, claims) {
			hits = append(hits, r)
		}
	}
	if len(hits) == 0 {
		return "", "", errors.New("no trust rule matches this token")
	}
	principals := map[string]bool{}
	for _, h := range hits {
		principals[h.Principal] = true
	}
	if len(principals) > 1 {
		return "", "", fmt.Errorf("rules for %d principals match this token: %s", len(principals), strings.Join(slices.Sorted(maps.Keys(principals)), ", "))
	}
	return hits[0].Principal, hits[0].Name, nil
}

func matches(want map[string]string, claims map[string]any) bool {
	for k, v := range want {
		got, ok := claims[k].(string)
		if !ok || got != v {
			return false
		}
	}
	return true
}
