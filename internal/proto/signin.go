package proto

import "time"

// The scopes of a token the server issues at a sign-in (transport.md
// §6.8). The server checks them at dispatch, by op, and at the hello,
// by session.
const (
	// ScopeCLI is a person at a terminal: every op the principal's role
	// allows, the session cli among them (admin ops, deciding a gate,
	// locks).
	ScopeCLI = "cli"
	// ScopeMCP is an agent's tools: the ops behind them, never an admin
	// op, never deciding a gate, never the session cli.
	ScopeMCP = "mcp"
	// ScopeCI is a CI job's: a member's ops, and reporting a ci gate's
	// check from the session ci.
	ScopeCI = "ci"
)

// A person's sign-ins at the server's providers (transport.md §6.7),
// for `aries admin signin`. A sign-in is opened by `aries login` over
// HTTPS, not by an op.
const (
	OpSignIns   = "signins"    // SignInsArgs → SignInsResult; server admins, and the principal
	OpSignInEnd = "signin.end" // SignInEndArgs → SignInEndResult; server admins, and the principal
)

// SignInsArgs lists the sign-ins of the principal Name: live, and those
// that ended within the server's login: prune_after.
type SignInsArgs struct {
	Name string `json:"name"`
}

// SignInsResult lists a principal's sign-ins, oldest first.
type SignInsResult struct {
	Principal string   `json:"principal"`
	SignIns   []SignIn `json:"signins"`
}

// The states of a sign-in.
const (
	SignInLive  = "live"
	SignInEnded = "ended"
)

// SignIn is one sign-in: never its token or key. ExpiresAt is its end;
// EndedAt and EndReason say when and why it ended before it (logout,
// revoked, identity_removed, principal_disabled). State is live or ended, by the server's
// clock.
type SignIn struct {
	ID        string     `json:"id"`
	Principal string     `json:"principal"`
	Provider  string     `json:"provider,omitempty"`
	Client    string     `json:"client"`
	Scopes    []string   `json:"scopes"`
	Flow      string     `json:"flow"`
	Machine   string     `json:"machine"`
	State     string     `json:"state"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	EndReason string     `json:"end_reason,omitempty"`
}

// SignInEndArgs ends sign-in ID now, as `aries admin signin end ID`
// does: its tokens are deleted, and its connections closed.
type SignInEndArgs struct {
	ID string `json:"id"`
}

// SignInEndResult is the sign-in ended, and how many connections it
// closed. Change is "ended", or empty when it had ended already.
type SignInEndResult struct {
	SignIn
	Connections int    `json:"connections"`
	Change      string `json:"change,omitempty"`
}
