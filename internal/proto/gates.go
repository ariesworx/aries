package proto

import (
	"fmt"
	"time"
)

// Gates and dates (design §20.3; stage 6, slice G0; protocol 8).
const (
	OpGateAdd   = "gate.add"   // GateAddArgs → Gate
	OpGatePass  = "gate.pass"  // GateArgs → Gate
	OpGateWaive = "gate.waive" // GateArgs → Gate
	OpGateRearm = "gate.rearm" // GateArgs → Gate
	OpGates     = "gates"      // GatesArgs → GatesResult
	OpDates     = "dates"      // DatesArgs → DatesResult
)

// GateAddArgs adds a human gate that holds issue Blocks back until one of
// Approvers passes it; with none, the project's admins other than its
// creator. Title defaults to "Approve " and the held issue's id. Idem,
// an idempotency key, makes a retry return the first result.
type GateAddArgs struct {
	Blocks    string   `json:"blocks"`
	Title     string   `json:"title,omitempty"`
	Approvers []string `json:"approvers,omitempty"`
	Idem      string   `json:"idem,omitempty"`
}

// GateArgs passes, waives or rearms gate ID, with a reason, which a waive
// needs. The server takes these only from CLISession, a person's own
// terminal.
type GateArgs struct {
	ID     string `json:"id"`
	Reason string `json:"reason,omitempty"`
}

// GatesArgs lists open gates, or with All every gate, oldest first, at
// most Limit (0 takes 50; at most 500).
type GatesArgs struct {
	All   bool `json:"all,omitempty"`
	Limit int  `json:"limit,omitempty"`
}

// Gate is a gate. Approvers is empty when the project's admins pass it.
// Outcome is empty while it is open, else passed or waived, By and At
// saying who and when. Holds are the issues that depend on it directly;
// a gate on an epic holds its descendants too.
type Gate struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Kind      string     `json:"kind"`
	Approvers []string   `json:"approvers"`
	Outcome   string     `json:"outcome,omitempty"`
	By        string     `json:"by,omitempty"`
	At        *time.Time `json:"at,omitempty"`
	Reason    string     `json:"reason,omitempty"`
	CreatedBy string     `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
	Holds     []string   `json:"holds,omitempty"`
}

// GatesResult lists gates.
type GatesResult struct {
	Gates []Gate `json:"gates"`
}

// DatesArgs reads what dates on issues say now: the issues that became
// ready since Since (as DigestArgs.Since; empty takes 24h) because their
// defer_until passed, those due within the server's due_soon, and those
// overdue.
type DatesArgs struct {
	Since string `json:"since,omitempty"`
}

// DatesResult is dates' result. An item's At is its date (defer_until
// for Ready, the due date for the rest) and By its assignee. Totals
// count everything matched; the lists hold the first few.
type DatesResult struct {
	Since     time.Time    `json:"since"`
	Until     time.Time    `json:"until"`
	Ready     []DigestItem `json:"ready,omitempty"`
	DueSoon   []DigestItem `json:"due_soon,omitempty"`
	Overdue   []DigestItem `json:"overdue,omitempty"`
	Totals    DateTotals   `json:"totals"`
	Truncated bool         `json:"truncated,omitempty"`
}

// DateTotals counts each of DatesResult's lists in full.
type DateTotals struct {
	Ready   int `json:"ready,omitempty"`
	DueSoon int `json:"due_soon,omitempty"`
	Overdue int `json:"overdue,omitempty"`
}

// ParseDate reads a date an issue's defer_until or due is set to: a time
// in RFC 3339, or a date (2006-01-02), which is midnight UTC. Empty is
// the zero time, which clears the date. A time before 1970 is refused:
// the zero time among them, which would clear it.
func ParseDate(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		if t, err = time.Parse(time.DateOnly, s); err != nil {
			return time.Time{}, fmt.Errorf("%q is not a date (2006-01-02) or a time (RFC 3339, 2006-01-02T15:04:05Z)", s)
		}
	}
	if t.Year() < 1970 {
		return time.Time{}, fmt.Errorf("%q is before 1970", s)
	}
	return t.UTC(), nil
}
