package proto

import "time"

// Briefs (design §22; protocol 11).
const (
	OpBrief = "brief" // BriefArgs → BriefResult; the CLI alone, never a sign-in of scope mcp or ci
)

// BriefArgs records the brief a session is started with on issue ID, by
// whoever starts it: the harness, model and effort it runs on, the
// session as the starter names it (a Desktop session's local_… id, or a
// headless run's Aries session), the template it was rendered from
// (brief@N, or workflow@REV), and the brief's text verbatim. Each field
// but the text is one word. Effort defaults to "default". The recorder's
// principal and session are the connection's. Idem, an idempotency key,
// makes a retry return the first result.
type BriefArgs struct {
	ID       string `json:"id"`
	Harness  string `json:"harness"`
	Model    string `json:"model"`
	Effort   string `json:"effort,omitempty"`
	Session  string `json:"session,omitempty"`
	Template string `json:"template"`
	Text     string `json:"text"`
	Idem     string `json:"idem,omitempty"`
}

// BriefResult names the brief's comment and its SHA-256, of the text
// trimmed, and the claim it was joined to at once, if it was.
type BriefResult struct {
	ID     string      `json:"id"`
	SHA256 string      `json:"sha256"`
	State  string      `json:"state"`
	Claim  *BriefClaim `json:"claim,omitempty"`
}

// BriefClaim is the claim a brief is joined to: the session that started
// the issue with it, and the claim's epoch.
type BriefClaim struct {
	By      string    `json:"by"`
	Session string    `json:"session"`
	Epoch   int64     `json:"epoch"`
	At      time.Time `json:"at"`
}

// Brief is a recorded brief, as show returns it. By and BySession
// recorded it; State is pending, until a start joins it to Claim
// (bound). Text, the brief trimmed, is returned with Full; the comment
// holds it whole.
type Brief struct {
	Comment   string      `json:"comment"`
	By        string      `json:"by"`
	BySession string      `json:"by_session"`
	Harness   string      `json:"harness"`
	Model     string      `json:"model"`
	Effort    string      `json:"effort"`
	Session   string      `json:"session,omitempty"`
	Template  string      `json:"template"`
	SHA256    string      `json:"sha256"`
	At        time.Time   `json:"at"`
	State     string      `json:"state"`
	Claim     *BriefClaim `json:"claim,omitempty"`
	Text      string      `json:"text,omitempty"`
}
