package mcpserver

import (
	"errors"
	"strconv"
	"strings"

	"github.com/ariesworx/aries/internal/client"
	"github.com/ariesworx/aries/internal/offline"
	"github.com/ariesworx/aries/internal/outbox"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/safetext"
)

// explain rephrases an error for an agent (design §12 item 5). The server's
// code and message are kept; its fix, written for a person at a shell
// (`aries show …`), becomes the agent's next step in terms of tools.
// Fixes only a person can carry out (keys, config, upgrades) are handed to
// the user, quoted: the server chose that text, so it is relayed as data
// for the user, never as the agent's instruction (C-4). The message is
// the server's too, escaped onto one line so it cannot add a fix line.
func explain(err error) toolErr {
	var lost *lostError
	var dial *dialError
	var unq *unqueuedError
	var pe *proto.Error
	errors.As(err, &pe)
	e := toolErr{Code: string(proto.CodeUnavailable), Message: err.Error()}
	if pe != nil {
		e.Code, e.Message = string(pe.Code), pe.Message
	}
	e.Message = safetext.Line(e.Message)
	var off *offlineRead
	switch {
	case errors.As(err, &off):
		// What everyone is doing now, from a copy, is wrong in the way
		// that has two sessions take the same work (design §18.6).
		e.Message += "; " + off.tool + " is not answered from this machine's copy, since it says what others are doing now (" + offline.LastReachedLine(off.last) + ")"
		e.Fix = "carry on with the work you hold (prime and show answer from this machine's copy) and call " + off.tool +
			" again later; if it still fails, tell the user"
	case client.ClosedBeforeHandshake(err):
		// sshd turns an address away for a while after failed or
		// dropped connections, and every attempt meanwhile can lengthen
		// that: the person must know, and the agent must not loop.
		e.Fix = hangUpFix()
		if errors.As(err, &unq) {
			e.Fix = "this was not queued (" + unq.why + "); " + e.Fix
		}
		e.Fix += personFix(pe)
	case errors.As(err, &unq):
		// No claim is taken, and nothing undone, without the server
		// (design §14, decision 39): the agent goes on with what it holds.
		e.Fix = "aries cannot reach its server, and this was not queued (" + unq.why +
			"): carry on with the work you hold and call it again later; if it still fails, tell the user"
	case errors.Is(err, offline.ErrNotQueued) && errors.Is(err, outbox.ErrSecret):
		e.Fix = nextStep(pe)
	case errors.Is(err, offline.ErrNotQueued) && errors.Is(err, outbox.ErrTooLarge):
		e.Fix = "aries cannot reach its server, and this is too long to keep for later: shorten the text and call it again"
	case errors.Is(err, offline.ErrNotQueued):
		// The outbox is the person's to manage, never the agent's (rule
		// 12): the person's fix is in `aries outbox`, which says the same.
		e.Fix = "aries cannot reach its server and could not keep this for later: keep the text, call it again when a call succeeds, and tell the user"
	case errors.As(err, &lost) && lost.retried:
		e.Fix = "aries reconnected and the retry failed too; try again later, and if it persists tell the user" + personFix(pe)
	case errors.As(err, &lost):
		e.Fix = "the connection dropped and the next call reconnects; this write may have applied, so check with show before repeating it"
	case proto.AtConnLimit(err):
		// Only the user can free a connection. The agent learns what
		// waiting costs: its claims lapse, and busy is never queued.
		e.Fix = "the user's other aries sessions hold every connection the server allows them, so this session cannot reach it until one closes. " +
			"Tell the user now: they can close some, or ask the server's operator to raise conns_per_principal. " +
			"Until then, issues you hold go back to open unless renewed within their " + Lease + " lease, and writes are not kept for later; never run an admin command yourself"
	case pe != nil && pe.Code == proto.CodeAuth && strings.HasPrefix(pe.Fix, proto.FixSignIn):
		// The user's sign-in ended, or never was. prompt=login has the
		// provider ask the person again, which an agent driving their
		// browser must not do for them (identity.md §6.7).
		e.Fix = signInFix
	case errors.As(err, &dial) && pe != nil && pe.Code == proto.CodeForbidden:
		// The principal is no member of the project, or was removed or
		// disabled while connected. The server's fix names the admin's
		// command; an agent may hold an admin's key, so it is told
		// outright to leave that to a person.
		e.Fix = "you have no access to this project: stop and tell the user, who must ask one of its admins; never run an admin command yourself" + personFix(pe)
	case errors.As(err, &dial):
		if pe == nil || pe.Fix == "" {
			e.Fix = "tell the user aries cannot connect: check " + client.ConfigFile + " and the network"
		} else {
			e.Fix = "tell the user aries cannot connect" + personFix(pe)
		}
	case pe == nil:
		e.Fix = "retry once; if it fails again, tell the user"
	default:
		e.Fix = nextStep(pe)
	}
	return e
}

// signInFix is the agent's next step when the user's sign-in at the
// server has ended: the person signs in again, at a terminal.
const signInFix = "the user's sign-in at the server has ended: tell the user to run `aries login` in a terminal; never run it yourself, since signing in is the person's act"

// hangUpFix is the agent's next step when the server closed the
// connection before the SSH handshake.
func hangUpFix() string {
	return "the server hung up before signing in, as sshd does for a while after failed connections: " +
		"do not call aries tools again in a loop, since each attempt can lengthen the wait (aries itself waits " + hangUpWait.String() + " before it dials again); " +
		"carry on with the work you hold and tell the user"
}

// nextStep is the agent's next step after a server refusal. It tells
// refusals apart by their code and the proto.Fix phrase their fix opens
// with (FixNothing is a whole fix, FixEditText an ending). internal/server
// writes those fixes with the same phrases, and its TestFixPhrases fails
// if a fix loses its phrase.
func nextStep(pe *proto.Error) string {
	switch pe.Code {
	case proto.CodeNotFound:
		switch {
		case strings.HasPrefix(pe.Fix, proto.FixSeeBlocked):
			return "nothing is ready: call blocked to see why, or create an issue"
		case strings.HasPrefix(pe.Fix, proto.FixFindMemory):
			return "call recall to find the key and its scope; remember without rev creates it"
		case strings.HasPrefix(pe.Fix, proto.FixFindConflict):
			return "call show on the issue to list its open conflicts and their numbers"
		}
		return "find the id with list or ready"
	case proto.CodeConflict:
		switch {
		case strings.HasPrefix(pe.Fix, proto.FixTakeNext):
			return "call start with the next ready id named above"
		case strings.HasPrefix(pe.Fix, proto.FixTakeOver):
			return "another session of yours holds it: call start with take: true only if the user says that session has stopped; otherwise pick other work"
		case strings.HasPrefix(pe.Fix, proto.FixLeaveIt):
			return "someone else holds this issue: pick other work with start, or tell the user if it must move"
		case strings.HasPrefix(pe.Fix, proto.FixReread):
			return "call show for the current rev, then retry with that rev if your change still applies"
		case strings.HasPrefix(pe.Fix, proto.FixRecall):
			return "call recall with the key and scope for its body and rev, merge your change into it, then remember again with that rev"
		}
		return "retry"
	case proto.CodeExists:
		return "omit id to have one generated, or call show on the existing issue"
	case proto.CodeCycle:
		if strings.HasPrefix(pe.Fix, proto.FixNothing) {
			return "nothing to do: a parent waits on its open children, and is not ready until they close"
		}
		return "remove an edge with dep (action rm), or choose another parent"
	case proto.CodeInvalid:
		switch {
		case strings.HasPrefix(pe.Fix, proto.FixReopen):
			return "call reopen first"
		case strings.HasPrefix(pe.Fix, proto.FixStart):
			return "call start to take it; update cannot set in_progress"
		case strings.HasPrefix(pe.Fix, proto.FixRelease):
			return "it is claimed: call finish or handoff with release: true first if it is yours; otherwise leave it"
		case pe.Fix == proto.FixNothing:
			return "nothing to do"
		case strings.HasPrefix(pe.Fix, proto.FixUpgrade):
			return "tell the user" + personFix(pe)
		case strings.HasPrefix(pe.Fix, proto.FixSecret):
			return "never store a secret: remember the rest, naming where the secret is kept (a vault path or an environment variable) instead"
		case strings.HasPrefix(pe.Fix, proto.FixForgetSome):
			return "call forget on memories that no longer hold, or tell the user" + personFix(pe)
		case strings.HasPrefix(pe.Fix, proto.FixGate):
			return "a gate holds this until a person passes it: do not work around it; pick other work with ready, or tell the user it waits for the approvers named above"
		case strings.HasPrefix(pe.Fix, proto.FixGateAdd):
			return "call gate with action add to hold an issue back; create and update do not make gates"
		}
		return "correct the arguments and retry"
	case proto.CodeAcceptance:
		if strings.HasSuffix(pe.Fix, proto.FixEditText) {
			return "the edit drops open acceptance items: keep them in the text, or tell the user they must be ticked or waived first"
		}
		if strings.HasPrefix(pe.Fix, proto.FixReview) {
			return "do not call finish again now: a review is a person's act at their own terminal; tell the user the issue waits for the approvers named above, hand off with state: done if you stop, and never run aries gate yourself"
		}
		if strings.HasPrefix(pe.Fix, proto.FixLand) {
			return "do not call finish again now: call handoff with state: done and the pull request's link in the note; the check's pass closes the issue, or, if items are still open then, your inbox says so and finish with them ticked closes it"
		}
		return "call finish with ticked: [numbers met] and waived: {number: reason} for the rest; show lists the items"
	case proto.CodeForbidden:
		if strings.HasPrefix(pe.Fix, proto.FixGate) {
			return "passing a gate is a person's act at their own terminal: tell the user it waits for the approvers named above; never run aries gate yourself"
		}
		if strings.HasPrefix(pe.Fix, proto.FixAsk) {
			return "someone else holds this issue: do not change it; leave a comment, pick other work with start, or tell the user"
		}
		return "this is for an aries admin: tell the user"
	case proto.CodeUnavailable:
		return "retry once; if it fails again, tell the user" + personFix(pe) + requestID(pe)
	case proto.CodeBusy:
		return "the server is limiting your requests: wait a few seconds, then retry once; if it is refused again, tell the user" + personFix(pe)
	}
	return "tell the user" + personFix(pe)
}

// requestID has the agent pass on the id the server gave a request it
// failed: the server's log has the cause under it. The id is the
// server's text, so it is repeated only in the shape the server makes
// it, 16 hex digits, which can say nothing else.
func requestID(pe *proto.Error) string {
	if len(pe.Req) != 16 || strings.Trim(pe.Req, "0123456789abcdef") != "" {
		return ""
	}
	return "; give the user the request id " + pe.Req
}

// personFix relays the server's fix for the user, quoted.
func personFix(pe *proto.Error) string {
	if pe == nil || pe.Fix == "" {
		return ": aries cannot reach its server"
	}
	return ", quoting the server: " + strconv.Quote(pe.Fix)
}
