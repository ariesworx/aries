package proto

// Code classifies an error for programs. People read Message and Fix.
type Code string

// Error codes.
const (
	// CodeNotFound: the issue (or other target) does not exist.
	CodeNotFound Code = "not_found"
	// CodeConflict: the target changed since the revision the caller read.
	CodeConflict Code = "conflict"
	// CodeExists: an issue with that ID already exists.
	CodeExists Code = "exists"
	// CodeCycle: the change would create a dependency cycle.
	CodeCycle Code = "cycle"
	// CodeInvalid: the request failed validation.
	CodeInvalid Code = "invalid"
	// CodeAcceptance: close or finish was refused because acceptance
	// items are neither ticked nor waived, or an update would drop such
	// items from the text.
	CodeAcceptance Code = "acceptance"
	// CodeForbidden: the caller may not make this change: another
	// principal holds the issue, or the change is for admins.
	CodeForbidden Code = "forbidden"
	// CodeUnavailable: the server, or the daemon behind it, cannot serve
	// the request now.
	CodeUnavailable Code = "unavailable"
	// CodeVersion: the client's protocol version is outside the server's
	// range.
	CodeVersion Code = "version"
	// CodeAuth: SSH authentication failed, or the server's host key does
	// not match the pinned one.
	CodeAuth Code = "auth"
	// CodeBusy: the caller reached one of the server's limits on
	// connections or writes; the fix says how long to wait.
	CodeBusy Code = "busy"
	// CodeStale: the request's idempotency key was used longer ago than
	// the server keeps a key's result: its request was applied then, and
	// this one is not. An
	// outbox gives the same code to an entry it kept past that age and
	// did not send.
	CodeStale Code = "stale"
)

// Error is a typed, one-line error. Message names the cause; Fix names the
// next action (design §12 item 5).
type Error struct {
	Code    Code   `json:"c"`
	Message string `json:"m"`
	Fix     string `json:"fix,omitempty"`
	// Req is the id the server gave the request it refuses: every line
	// of the server's log about the request has it, so a report that
	// quotes it finds them (design §16.4). It is a result field: a
	// server that predates it sends none, and a client that does
	// ignores it.
	Req string `json:"req,omitempty"`
}

// Error returns Message, followed by "; " and Fix when there is one.
func (e *Error) Error() string {
	if e.Fix == "" {
		return e.Message
	}
	return e.Message + "; " + e.Fix
}

// Errf returns an Error with code, fix and msg. Despite its name it does
// not format: callers build msg, and fix, with [fmt.Sprintf].
func Errf(code Code, fix, msg string) *Error {
	return &Error{Code: code, Message: msg, Fix: fix}
}

// Fix phrases. aries mcp tells some refusals apart by their code and the
// phrase their fix opens with, and turns each into an agent's next step
// (internal/mcpserver). The server writes those fixes with these phrases,
// so the two cannot drift apart. They are wire text that released clients
// match too, so keep each as it is. FixNothing is a whole fix, and
// FixEditText the end of one.
const (
	// FixSeeBlocked: not_found, nothing is ready to start.
	FixSeeBlocked = "see what holds work back"
	// FixTakeNext: conflict, another principal holds the issue; the next
	// ready one is named.
	FixTakeNext = "take that one"
	// FixTakeOver: conflict, another session of the caller's principal
	// holds the issue.
	FixTakeOver = "take it over"
	// FixLeaveIt: conflict, another principal holds the issue, and nothing
	// else is ready.
	FixLeaveIt = "leave it to"
	// FixReread: conflict, the issue changed since the caller's rev.
	FixReread = "re-read"
	// FixReopen: invalid, the issue is closed.
	FixReopen = "reopen"
	// FixStart: invalid, update cannot set in_progress; start does.
	FixStart = "take it with"
	// FixRelease: invalid, a status or assignee change to a claimed issue.
	FixRelease = "finish it, or let it go"
	// FixNothing: invalid, the whole fix when the issue is already as
	// asked.
	FixNothing = "nothing to do"
	// FixUpgrade: invalid, the request does not fit this server's
	// protocol, such as an unknown operation or argument.
	FixUpgrade = "upgrade"
	// FixEditText: acceptance, the end of the fix when an edit drops
	// acceptance items still open.
	FixEditText = "then edit the text"
	// FixAsk: forbidden, another principal holds the issue, whom the fix
	// asks.
	FixAsk = "ask "
	// FixRecall: conflict, a memory exists or changed since the caller's
	// rev; the fix names the stored rev.
	FixRecall = "recall it"
	// FixFindMemory: not_found, no memory has the key in the scope.
	FixFindMemory = "find the key"
	// FixFindConflict: not_found, no conflict has the number.
	FixFindConflict = "find the conflict's number"
	// FixSecret: invalid, a memory looks like it holds a secret.
	FixSecret = "remove the secret"
	// FixForgetSome: invalid, the caller holds as many memories in the
	// scope as the server allows.
	FixForgetSome = "forget memories"
	// FixGate: invalid or forbidden, a gate holds the issue, or the
	// change is a gate's approvers' or a person's to make; the fix names
	// who.
	FixGate = "wait for"
	// FixGateAdd: invalid, an issue is made a gate only by gate add.
	FixGateAdd = "add a gate with"
	// FixLand: acceptance, a check the issue's workflow requires has not
	// passed (design §21.5): the work is handed off done, and the
	// check's pass closes it.
	FixLand = "hand it off"
	// FixReview: acceptance, a review the issue's workflow requires has
	// not passed (design §21.6): a person passes its gate at their own
	// terminal, and then the issue can be finished.
	FixReview = "ask a person to review"
	// FixSignIn: auth, the caller's sign-in ended, was ended, or does
	// not check out: the person signs in again, at a terminal.
	FixSignIn = "run `aries login`"
)
