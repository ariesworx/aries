package outbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/secretscan"
)

// Entry is one queued operation.
type Entry struct {
	// Seq is the order of replay. Append assigns it, and a number is
	// never used twice in one outbox.
	Seq uint64 `json:"seq"`
	// Op is the operation, one of proto's Op constants.
	Op string `json:"op"`
	// Args are the operation's arguments as they would have been sent,
	// idempotency key included, on one line.
	Args json.RawMessage `json:"args"`
	// Idem is the idempotency key in Args. Append reads it from there;
	// a caller that sets it must set the same.
	Idem string `json:"idem"`
	// Rev and Epoch are the revision and the claim epoch the operation
	// was based on, where Args have them. Append reads them from there.
	Rev   int64 `json:"rev,omitempty"`
	Epoch int64 `json:"epoch,omitempty"`
	// Session and Machine are who made it.
	Session string `json:"session,omitempty"`
	Machine string `json:"machine,omitempty"`
	// At is the client's time when it was queued. Append sets it.
	At time.Time `json:"at"`
}

// queued are the ops the outbox queues: the ones whose replay cannot do
// harm nobody asked for (design §18.3), each of which carries an
// idempotency key. The first six have one in protocol 5, and the rest
// from protocol 6 (proto.Keyed): the caller sends an entry only to a
// server that takes its key.
var queued = map[string]bool{
	proto.OpCreate:   true,
	proto.OpComment:  true,
	proto.OpHandoff:  true,
	proto.OpRemember: true,
	proto.OpHoursLog: true,
	proto.OpFinish:   true,
	proto.OpUpdate:   true,
	proto.OpLabelAdd: true,
	proto.OpLabelRm:  true,
	proto.OpDepAdd:   true,
	proto.OpAccept:   true,
	proto.OpUsage:    true,
}

// never are the writes that are not queued by design, each with why.
var never = map[string]string{
	proto.OpStart:       "it takes a claim, and no claim is made without the server",
	proto.OpClose:       "it ends something on a state the client cannot see",
	proto.OpReopen:      "it undoes something on a state the client cannot see",
	proto.OpForget:      "it undoes something on a state the client cannot see",
	proto.OpDepRm:       "it undoes something on a state the client cannot see",
	proto.OpHoursDelete: "it undoes something on a state the client cannot see",
	proto.OpRenew:       "renewing a lease is the client's own chore",
	proto.OpResolve:     "it decides between two changes on a state the client cannot see",
	proto.OpPropose:     "it decides between two changes on a state the client cannot see",
	// Gates (design §20.3, §20.9): never queued. Adding one later could
	// hold back work someone started meanwhile, and a pass, waive or
	// rearm is a person's decision at the moment they make it. A CI
	// job's report is the job's, which ends: one sent later would report
	// a run nobody is watching, so the job fails loudly instead.
	proto.OpGateAdd:   "it holds work back on a state the client cannot see",
	proto.OpGatePass:  "it is a person's decision on a state the client cannot see",
	proto.OpGateWaive: "it is a person's decision on a state the client cannot see",
	proto.OpGateRearm: "it is a person's decision on a state the client cannot see",
	proto.OpGateFail:  "it is a CI job's report, which a later run would not make",
}

// Queueable reports whether op is one the outbox queues.
func Queueable(op string) bool { return queued[op] }

// Unqueued reports why op, a write, is not queued, for a caller that
// could not reach the server with it. It reports false for an op the
// outbox queues and for one it knows nothing of, a read among them,
// which has nothing to queue.
func Unqueued(op string) (why string, ok bool) {
	if w, ok := never[op]; ok {
		return w, true
	}
	return "", false
}

// notQueueable is the refusal of an op the outbox does not queue.
func notQueueable(op string) error {
	why, ok := Unqueued(op)
	if !ok {
		why = "only writes that are safe to send again later are queued"
	}
	return fmt.Errorf("%w: %q is not queued while the server cannot be reached: %s", ErrNotQueueable, op, why)
}

// prepare checks e and fills in what Append derives: the key, the
// revision and the epoch from the arguments, and the time.
func (o *Outbox) prepare(e Entry) (Entry, error) {
	if !queued[e.Op] {
		return Entry{}, notQueueable(e.Op)
	}
	args := bytes.TrimSpace(e.Args)
	if len(args) == 0 || args[0] != '{' {
		return Entry{}, fmt.Errorf("%w: the arguments of %s must be a JSON object", ErrInvalid, e.Op)
	}
	var head struct {
		Idem  string `json:"idem"`
		Rev   int64  `json:"rev"`
		Epoch int64  `json:"epoch"`
	}
	if err := json.Unmarshal(args, &head); err != nil {
		return Entry{}, fmt.Errorf("%w: the arguments of %s: %v", ErrInvalid, e.Op, err)
	}
	if head.Idem == "" {
		return Entry{}, fmt.Errorf("%w: %s is replayed at least once, so it is not queued without one", ErrNoKey, e.Op)
	}
	if e.Idem != "" && e.Idem != head.Idem {
		return Entry{}, fmt.Errorf("%w: the key of the %s entry is not the one in its arguments", ErrInvalid, e.Op)
	}
	if kind, found := findSecret(args); found {
		return Entry{}, fmt.Errorf("%w: the arguments of %s hold a %s, and the outbox keeps no secret", ErrSecret, e.Op, kind)
	}
	// Stored without the white space between tokens, a newline among it,
	// and otherwise byte for byte.
	var one bytes.Buffer
	if err := json.Compact(&one, args); err != nil {
		return Entry{}, fmt.Errorf("%w: the arguments of %s: %v", ErrInvalid, e.Op, err)
	}
	e.Seq, e.Args = 0, one.Bytes()
	e.Idem, e.Rev, e.Epoch = head.Idem, head.Rev, head.Epoch
	e.At = o.now()
	return e, nil
}

// findSecret runs the secrets lint over every string in args, names and
// values alike. args is valid JSON.
func findSecret(args []byte) (secretscan.Kind, bool) {
	dec := json.NewDecoder(bytes.NewReader(args))
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", false // the end
		}
		if s, ok := tok.(string); ok {
			if f, found := secretscan.Find(s); found {
				return f.Kind, true
			}
		}
	}
}

// encode returns e as one line, newline included. The arguments are
// written as they are but for the white space between their tokens.
func encode(e Entry) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	line := buf.Bytes()
	if bytes.IndexByte(line, '\n') != len(line)-1 {
		// A newline inside a line would make two of it. JSON has none
		// outside its white space, which encoding removes.
		return nil, fmt.Errorf("%w: the entry does not fit one line", ErrInvalid)
	}
	return line, nil
}

// maxSeq is past every sequence number an entry may have, so that one
// more never wraps around. No outbox gets there by appending: only a
// file that something else wrote can say so.
const maxSeq = 1 << 62

// fileLine is a line of the outbox file: an entry, or the line a
// compaction leaves first, which holds only the lowest sequence number
// not yet used.
type fileLine struct {
	Entry
	Next uint64 `json:"next,omitempty"`
}

// scan is what a reader finds in an outbox file.
type scan struct {
	entries []Entry
	lines   [][]byte // each entry's line as it is in the file, without its newline
	next    uint64   // the lowest sequence number not yet used
	bad     int      // whole lines that are not an entry: skipped
	torn    bool     // bytes after the last newline: a line a dead writer did not finish
	size    int
}

// scanOutbox reads an outbox file as a reader that trusts nothing in it:
// a line that does not parse is skipped and counted, and bytes after the
// last newline are not a line (spike/outbox/RESULTS.md, section 3).
func scanOutbox(data []byte) scan {
	s := scan{next: 1, size: len(data)}
	for len(data) > 0 {
		line, rest, whole := bytes.Cut(data, []byte{'\n'})
		if !whole {
			s.torn = true
			break
		}
		data = rest
		var l fileLine
		if json.Unmarshal(line, &l) != nil || l.Seq >= maxSeq || l.Next > maxSeq {
			s.bad++
			continue
		}
		switch {
		case l.Op == "" && l.Next > 0:
			s.next = max(s.next, l.Next)
		case l.Seq > 0 && l.Op != "" && l.Idem != "" && len(l.Args) > 0:
			s.entries = append(s.entries, l.Entry)
			s.lines = append(s.lines, line)
			s.next = max(s.next, l.Seq+1)
		default:
			s.bad++
		}
	}
	return s
}

// The states an answer gives an entry. An entry with no answer is
// pending.
const (
	stateDelivered = "delivered" // the server applied it; it is gone
	stateRefused   = "refused"   // the server refused it, or it went stale; kept for a person
	stateDropped   = "dropped"   // a person dropped it; it is gone
	statePending   = "pending"   // a person asked for a refused entry to be sent again
)

// answer is a line of the answered file: what became of the entry Seq.
// A later line for the same entry replaces an earlier one.
type answer struct {
	Seq     uint64    `json:"seq"`
	State   string    `json:"state"`
	Code    string    `json:"code,omitempty"`
	Message string    `json:"message,omitempty"`
	Fix     string    `json:"fix,omitempty"`
	At      time.Time `json:"at"`
}

// scanAnswers reads an answered file as scanOutbox reads an outbox, and
// returns the last answer for each entry and how many it read.
func scanAnswers(data []byte) (map[uint64]answer, int) {
	last := make(map[uint64]answer)
	n := 0
	for len(data) > 0 {
		line, rest, whole := bytes.Cut(data, []byte{'\n'})
		if !whole {
			break
		}
		data = rest
		var a answer
		if json.Unmarshal(line, &a) != nil || a.Seq == 0 || a.Seq >= maxSeq {
			continue
		}
		switch a.State {
		case stateDelivered, stateRefused, stateDropped, statePending:
			last[a.Seq] = a
			n++
		}
	}
	return last, n
}
