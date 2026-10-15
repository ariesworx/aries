// Package logs is the schema of starfixd's log (design §16.4): the
// names of a line's fields, the stable name of each event a line
// reports, the id of a request, and the handler that puts a request's id
// and its trace's ids on every line logged for it.
//
// The names are a contract with whoever alerts on the log or reads it
// with a program: a field or an event is added here, with a row in
// docs/server.md, and never renamed. Tests hold every log call of the
// daemon's packages to these lists.
//
// The package is the standard library alone, so that the store may
// import it.
package logs

import "slices"

// The fields a line may have beside slog's own time, level and msg. The
// first group is the schema of design §16.4, which every line shares;
// the rest say what one event or a few are about.
const (
	// KeyEvent is the stable name of what the line reports: one of the
	// [Event] values. Every line has it, as the first of the fields the
	// line's own call gives; the fields of the logger it was logged
	// through (a connection's project, principal and session) come
	// before it.
	KeyEvent = "event"
	// KeyProject is a project's id.
	KeyProject = "project"
	// KeyPrincipal and KeySession say whose connection or request the
	// line is about, and on an audit line who made the change.
	KeyPrincipal = "principal"
	KeySession   = "session"
	// KeyOp is a request's op, and on an audit line the event's.
	KeyOp = "op"
	// KeyCode is a refusal's code.
	KeyCode = "code"
	// KeyDurMS is how long a request took, in milliseconds.
	KeyDurMS = "dur_ms"
	// KeyReq is the request's id ([NewReq]): on every line logged for
	// the request, and in the refusal the client is sent.
	KeyReq = "req"
	// KeyTraceID and KeySpanID are the ids of the span the line was
	// logged under.
	KeyTraceID = "trace_id"
	KeySpanID  = "span_id"
	// KeyAudit is true on a line of the audit stream, and absent from
	// every other line.
	KeyAudit = "audit"

	// KeyErr is an error's text.
	KeyErr = "err"
	// KeySeq and KeyAt are, on an audit line, the event's number in the
	// control store's log and its time there.
	KeySeq = "seq"
	KeyAt  = "at"
	// KeyTarget is what an audit line's event changed, as the event log
	// names it. KeySubject is the principal it changed, KeyFingerprint a
	// key's fingerprint, and KeyRole a membership's role.
	KeyTarget      = "target"
	KeySubject     = "subject"
	KeyFingerprint = "fingerprint"
	KeyRole        = "role"
	// KeyBy is the principal whose change caused what the line reports.
	KeyBy = "by"
	// KeyIssue is an issue's id, and KeyEpoch a claim's.
	KeyIssue = "issue"
	KeyEpoch = "epoch"
	// KeyReason is a fixed word for why.
	KeyReason = "reason"
	// KeyDetail is text that is not the daemon's own: what the database
	// driver or the SDK said.
	KeyDetail = "detail"
	// KeyCause and KeyFix are why something is wrong and what to do
	// about it, and KeyEffect what follows from it meanwhile.
	KeyCause  = "cause"
	KeyFix    = "fix"
	KeyEffect = "effect"
)

// Kind is what a field holds, which decides whether it may leave the
// machine in a log record exported over OTLP. The journal has every
// field as it was logged.
type Kind int

// The kinds of field.
const (
	// KindLocal is anything else: an error's text, a path, a name. It stays
	// in the journal and is never exported, since nothing vets what it
	// quotes.
	KindLocal Kind = iota
	// KindWord is a word or a few from a fixed list in the code, or a
	// number, a boolean, a time or a duration: never text a client
	// chose.
	KindWord
	// KindPrincipal is a principal's name, and KindSession a session's id. They
	// are exported as telemetry: principals: allows.
	KindPrincipal
	KindSession
	// The rest are ids, each exported only in its own shape: a project's
	// UUID, an op the server knows, a refusal's code, a request's id, an
	// issue's id, a key's fingerprint.
	KindProject
	KindOp
	KindCode
	KindReq
	KindIssue
	KindFingerprint
	// KindTrace is the ids of a span. An exported record has them from the
	// span itself, so the fields are not repeated on it.
	KindTrace
)

// keys is every field a line may have, with its kind.
var keys = map[string]Kind{
	KeyEvent: KindWord, KeyProject: KindProject, KeyPrincipal: KindPrincipal, KeySession: KindSession, KeyOp: KindOp, KeyCode: KindCode,
	KeyDurMS: KindWord, KeyReq: KindReq, KeyTraceID: KindTrace, KeySpanID: KindTrace, KeyAudit: KindWord,

	KeyErr: KindLocal, KeySeq: KindWord, KeyAt: KindWord, KeyTarget: KindLocal, KeySubject: KindPrincipal, KeyFingerprint: KindFingerprint, KeyRole: KindWord,
	KeyBy: KindPrincipal, KeyIssue: KindIssue, KeyEpoch: KindWord, KeyReason: KindWord, KeyDetail: KindLocal, KeyCause: KindLocal, KeyFix: KindLocal, KeyEffect: KindLocal,

	// A connection, and what was done to it.
	"machine": KindLocal, "harness": KindLocal, "after": KindWord, "oldest": KindWord, "connections": KindWord, "own": KindWord,
	"lines": KindWord, "per_minute": KindWord,
	// A project and its store.
	"name": KindLocal, "database": KindLocal, "resumed": KindWord, "agents": KindWord, "inbox": KindWord, "account": KindLocal, "problems": KindLocal,
	// The keys file.
	"path": KindLocal, "keys": KindWord,
	// The clock.
	"log": KindLocal, "clock": KindWord, "history": KindWord, "behind": KindWord, "since": KindWord, "was_behind_since": KindWord, "direction": KindWord,
	"step": KindWord, "from": KindWord, "to": KindWord, "steps": KindWord, "extended": KindWord, "claims": KindWord, "forward_since_start": KindWord, "backward_since_start": KindWord,
	// Locks: a lock's name is text a person chose, so it stays in the
	// journal (design §20.9); how many is a number.
	"lock": KindLocal, "locks": KindWord, "waiting": KindWord,
	// Dolt's metrics, and the check of Dolt.
	"url": KindLocal, "failures": KindWord, "after_failures": KindWord, "every": KindWord,
	// The daemon's start, and the admins its config file names.
	"version": KindLocal, "socket": KindLocal, "control_db": KindLocal, "projects": KindWord, "admins": KindLocal, "addr": KindLocal,
	"admin": KindPrincipal, "created_by": KindPrincipal, "added_by": KindPrincipal,
	// Telemetry.
	"push": KindLocal, "push_interval": KindWord, "push_headers": KindWord, "pull": KindWord, "per_project": KindWord, "traces": KindLocal,
	"trace_headers": KindWord, "sampler": KindLocal, "principals": KindWord, "logs": KindLocal, "log_headers": KindWord, "service": KindLocal,
	"failed": KindWord, "failures_not_logged": KindWord, "signal": KindWord,
}

// Keys returns every field a line may have, sorted.
func Keys() []string {
	out := make([]string, 0, len(keys))
	for k := range keys {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// KindOf returns the kind of the field named key, and [KindLocal] for a
// name that is no field's: what is not known to be safe stays in the
// journal.
func KindOf(key string) Kind { return keys[key] }

// IsKey reports whether key is a field a line may have.
func IsKey(key string) bool {
	_, ok := keys[key]
	return ok
}
