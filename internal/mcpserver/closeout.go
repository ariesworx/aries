package mcpserver

import (
	"fmt"
	"strings"

	"github.com/ariesworx/aries/internal/proto"
)

// The close-out checklist (design §21.7). A workflow profile's closeout
// lines are what the harness does when work ends: shown, never tracked.
// finish, handoff (state done) and close (of an epic) answer them after
// the result line, as a short list in a block of their own, so that the
// result stays the one JSON document it was.

// closeoutNoteLen cuts the close-out note of an epic in digest, in bytes.
const closeoutNoteLen = 300

// closeoutReserve is what a result leaves for the inbox notice that may
// follow it, in tokens.
const closeoutReserve = 40

// trailer is a result that adds a block of text after its JSON.
type trailer interface{ trailer() string }

// closeoutBlock renders lines as the block after result: "closeout:" and
// a line each, cut by whole lines, the last of them a count of those
// left out, so that result and block together stay within
// [MaxResultTokens] with room for the inbox notice. The result is never
// cut for the list: the list is what gives way first, and the first line
// always stays.
func closeoutBlock(result any, lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	room := MaxResultTokens - closeoutReserve - size(result)
	render := func(k int) string {
		var b strings.Builder
		b.WriteString("closeout:")
		for _, l := range lines[:k] {
			b.WriteString("\n- " + l)
		}
		if k < len(lines) {
			fmt.Fprintf(&b, "\n- … %d more", len(lines)-k)
		}
		return b.String()
	}
	k := fitPrefix(len(lines), 1, func(k int) bool { return Tokens([]byte(render(k))) <= room })
	return render(k)
}

// Finished is finish's result: the issue's revision, the issues filed,
// and, after it, the close-out lines of its profile.
type Finished struct {
	ID      string   `json:"id"`
	Rev     int64    `json:"rev"`
	Created []string `json:"created,omitempty"`
	Lapsed  bool     `json:"lapsed,omitempty"`
	Comment string   `json:"comment,omitempty"`

	closeout []string
}

func finished(r proto.FinishResult) Finished {
	return Finished{ID: r.ID, Rev: r.Rev, Created: r.Created, Lapsed: r.Lapsed, Comment: r.Comment, closeout: r.Closeout}
}

func (f Finished) trailer() string { return closeoutBlock(f, f.closeout) }

// Handed is handoff's result, with the close-out lines of a handoff that
// is done.
type Handed struct {
	ID  string `json:"id"`
	Rev int64  `json:"rev"`

	closeout []string
}

func (h Handed) trailer() string { return closeoutBlock(h, h.closeout) }

// Closed is close's result, with the close-out lines of an epic it
// closed.
type Closed struct {
	ID  string `json:"id"`
	Rev int64  `json:"rev"`

	closeout []string
}

func (c Closed) trailer() string { return closeoutBlock(c, c.closeout) }
