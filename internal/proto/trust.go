package proto

import "time"

// Trust rules (docs/design/identity.md §7, slice N5): which tokens of a
// workload provider, a CI system's or a cloud's, sign in as which
// service principal, for `aries admin trust`. A sign-in by one is made
// by token exchange at the server's /oauth/token, over HTTPS, not by an
// op; it holds the scope ci.
const (
	OpTrustAdd    = "trust.add"    // TrustAddArgs → TrustResult; server admins
	OpTrustRules  = "trust_rules"  // TrustRulesArgs → TrustRulesResult; server admins, and a service's owner
	OpTrustShow   = "trust.show"   // TrustArgs → TrustResult; server admins, and the service's owner
	OpTrustSet    = "trust.set"    // TrustSetArgs → TrustResult; server admins
	OpTrustRemove = "trust.remove" // TrustArgs → TrustResult; server admins
)

// The states of a trust rule.
const (
	TrustActive   = "active"
	TrustDisabled = "disabled"
)

// TrustRule is a trust rule: a workload provider of the server's
// settings, and claims its tokens must each carry with exactly that
// value, for the service Principal. TokenLifetime is how long a sign-in
// by it lasts, as a duration such as 1h or 90m (ParseDuration).
type TrustRule struct {
	Name          string            `json:"name"`
	Principal     string            `json:"principal"`
	Provider      string            `json:"provider"`
	Claims        map[string]string `json:"claims"`
	TokenLifetime string            `json:"token_lifetime"`
	State         string            `json:"state"`
	CreatedBy     string            `json:"created_by"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
	Rev           int64             `json:"rev"`
}

// TrustAddArgs adds a trust rule. TokenLifetime, a duration such as 1h,
// defaults to the server's workload: token_lifetime.
type TrustAddArgs struct {
	Name          string            `json:"name"`
	Principal     string            `json:"principal"`
	Provider      string            `json:"provider"`
	Claims        map[string]string `json:"claims"`
	TokenLifetime string            `json:"token_lifetime,omitempty"`
}

// TrustArgs names a trust rule.
type TrustArgs struct {
	Name string `json:"name"`
}

// TrustSetArgs changes a trust rule: Claims, when set, replace its
// claims; TokenLifetime, when set, its lifetime; State, when set,
// enables or disables it. Changing the claims or disabling it ends the
// sign-ins it gave.
type TrustSetArgs struct {
	Name          string            `json:"name"`
	Claims        map[string]string `json:"claims,omitempty"`
	TokenLifetime string            `json:"token_lifetime,omitempty"`
	State         string            `json:"state,omitempty"`
}

// TrustResult is a trust rule after a write, or as trust.show reads it.
// Change is "added", "changed", "removed", or empty when nothing was
// written; Ended counts the sign-ins the write ended, and Required names
// the claims the provider's kind requires.
type TrustResult struct {
	TrustRule
	Change   string   `json:"change,omitempty"`
	Ended    int      `json:"ended,omitempty"`
	Required []string `json:"required,omitempty"`
}

// TrustRulesArgs lists the trust rules of the service Principal, or every
// rule on the server when it is empty, which only a server admin may.
type TrustRulesArgs struct {
	Principal string `json:"principal,omitempty"`
}

// TrustRulesResult lists trust rules by name.
type TrustRulesResult struct {
	Rules []TrustRule `json:"rules"`
}
