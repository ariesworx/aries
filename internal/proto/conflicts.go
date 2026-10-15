package proto

import "time"

// Conflicts (protocol 7, design §18.7): the fields of replayed updates
// that someone else changed since the rev the update was based on,
// parked for a person to resolve. An agent may propose a resolution; a
// person resolves, or accepts the proposal (decision 42). There is no
// resolve tool over MCP.
const (
	OpConflicts = "conflicts"        // ConflictsArgs → ConflictsResult
	OpPropose   = "conflict.propose" // ProposeArgs → Empty
	OpResolve   = "resolve"          // ResolveArgs → WriteResult
)

// ProtoConflicts is the first protocol with conflicts: update's park,
// and the ops above.
const ProtoConflicts = 7

// ProtoLate is the first protocol in which a finish or handoff replayed
// from an outbox carries park, and is kept as a comment rather than
// refused when the issue moved on since its claim (design §18.6).
const ProtoLate = 8

// MaxShowConflicts bounds the conflicts show returns with an issue.
const MaxShowConflicts = 10

// The choices of a resolution. ChoiceAccept resolves as the conflict's
// proposal says, and is not itself a proposal.
const (
	ChoiceMine   = "mine"
	ChoiceTheirs = "theirs"
	ChoiceText   = "text"
	ChoiceAccept = "accept"
)

// ConflictsArgs lists open conflicts: of Issue when it is set, at most
// Limit (0 takes 20, at most 100), after the conflict After; or the one
// conflict ID, open or resolved, with its sides whole.
type ConflictsArgs struct {
	Issue string `json:"issue,omitempty"`
	ID    int64  `json:"id,omitempty"`
	Limit int    `json:"limit,omitempty"`
	After int64  `json:"after,omitempty"`
}

// Conflict is one parked field: Base is its value at BaseRev, Mine the
// replayed update's, which By made, and Theirs the server's when it was
// parked, which TheirsBy made at TheirsAt. In a list a side longer than
// a show's long text is cut, and Truncated set; asked for by ID, the
// sides are whole. Advice is the server's recommendation, computed when
// read, and never applied by itself.
type Conflict struct {
	ID         int64      `json:"id"`
	Issue      string     `json:"issue"`
	Field      string     `json:"field"`
	BaseRev    int64      `json:"base_rev"`
	Base       string     `json:"base"`
	Mine       string     `json:"mine"`
	Theirs     string     `json:"theirs"`
	By         string     `json:"by"`
	Session    string     `json:"session,omitempty"`
	TheirsBy   string     `json:"theirs_by"`
	TheirsAt   time.Time  `json:"theirs_at"`
	ParkedAt   time.Time  `json:"parked_at"`
	Proposal   *Proposal  `json:"proposal,omitempty"`
	Resolution string     `json:"resolution,omitempty"`
	ResolvedBy string     `json:"resolved_by,omitempty"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	Advice     string     `json:"advice,omitempty"`
	Truncated  bool       `json:"truncated,omitempty"`
}

// Proposal is a suggested resolution of a conflict: mine, theirs, or
// text, with why, by whom and when.
type Proposal struct {
	Choice string    `json:"choice"`
	Text   string    `json:"text,omitempty"`
	Why    string    `json:"why,omitempty"`
	By     string    `json:"by"`
	At     time.Time `json:"at"`
}

// ConflictsResult is a page of conflicts, oldest first. Next, when set,
// is the After of the next page.
type ConflictsResult struct {
	Conflicts []Conflict `json:"conflicts"`
	Next      int64      `json:"next,omitempty"`
}

// ProposeArgs proposes how to resolve conflict ID: Choice mine, theirs
// or text, Text with text alone, and Why. A later proposal replaces it;
// none changes the issue until a person accepts it.
type ProposeArgs struct {
	ID     int64  `json:"id"`
	Choice string `json:"choice"`
	Text   string `json:"text,omitempty"`
	Why    string `json:"why,omitempty"`
}

// ResolveArgs resolves conflict ID: Choice mine, theirs, text (with
// Text), or accept, which resolves as its proposal says.
type ResolveArgs struct {
	ID     int64  `json:"id"`
	Choice string `json:"choice"`
	Text   string `json:"text,omitempty"`
}
