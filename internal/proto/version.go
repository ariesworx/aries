package proto

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/ariesworx/starfix/internal/lazyregexp"
)

// Protocol versions. A client speaks Proto; a server accepts any client in
// [ProtoMin, ProtoMax]. Principle 9 keeps the range one version back and
// one forward once there is more than one: a change to the protocol
// raises Proto and ProtoMax and leaves ProtoMin at the version before, so
// that a new server still serves the clients people already have
// (AGENTS.md, rule 10).
//
// This build speaks protocols 7 and 8, and accepts both. Making 5 the
// minimum was a reset, made once, on 12 Oct 2026 (design §14, decision
// 25): nobody ran starfix in production yet and it was under active
// development, so the maintainer chose to refuse every released client
// (v0.2.x to v0.4.x, protocols 2 to 4) rather than go on carrying them.
// It is not a change to the rule above, which holds for every bump after
// it: 6 made Proto and ProtoMax 6 and left ProtoMin at 5.
// TestReleasedClients pins which releases are refused, and what each is
// told.
//
// A refused client is told which side to upgrade (CheckProto). A client
// older than the server prints the message and fix the server sends, as
// they are, so those words are all a released client can be told; a
// client newer than the server writes its own. `starfixd upgrade` is run
// by the old binary, whose health check then speaks the old protocol to
// the new daemon: the daemon answers that check whatever protocol it
// speaks (server.ProbePrincipal), or no upgrade could cross a raised
// minimum.
//
// What each version added, kept as history:
//
// 1 (v0.1.0) is the first protocol.
//
// 2 (v0.2.x) added claims: start's lease and claim, the epoch on finish
// and handoff, show's claim, renew, and who (the agents registry); then
// the inbox (inbox, ack, watch and the evt frames it pushes) and the
// handoff fields on finish and handoff; then idempotency keys on
// comment, finish and handoff, the acceptance checklist (accept,
// finish's ticked and waived, close's force) and similar closed issues in
// create's and show's results; then start's take and the forbidden code
// (the authorization review).
//
// 3 (v0.3.0) added token capture (design §12.1): the usage op, and
// account on create and update; then files to issues (design §12 item
// 3): paths on renew, finish and handoff, declared paths on create and
// update, ready's overlaps and show's files; then the live board's
// reads: watch's events, the event push (EvEvent) and the claims op.
//
// 4 (v0.4.0) added memory (design §6): the remember, recall, forget and
// pin ops, and start's memories; then prices and cost reports (design
// §12.1): the price.set, prices and cost ops, and show's and digest's
// cost; then human hours and subscription plans: the hours.log,
// hours.delete, hours, plan.set and plans ops, and the logged time and
// amortized cost in show, digest and cost.
//
// 5 (not yet released) adds access management on a server of many
// projects (design §15, admin.go): the project.add, projects,
// project.archive, project.activate, principal.add, principals,
// principal.disable, principal.enable, member.set, members and
// member.remove ops; keys in the control store (design §15.2): the
// key.add, keys and key.remove ops and principal.add's keys; and the
// status op (design §15.4, `sfx admin status`). Results carry released
// (on the ops that end access), warning, in_authorized_keys, file_conns
// and status's keys_file, clock, dolt and traces. It also adds the trace
// context on a request
// frame, tp and ts (decision 27): frames are read leniently, so the
// fields needed no version of their own, and were added while 5 was
// unreleased so that every protocol 5 server reads them.
//
// Within each version, everything after its first addition joined it
// before it shipped in a release, so without another bump. That is the
// practice for a version no release speaks: an addition joins it. For a
// few days main called the key ops and status protocol 6, a bump made so
// that a client with the key commands was refused at the handshake by a
// server built from main before them. No release ever spoke 5 or 6, and
// the maintainer wanted as few versions as possible at this stage, so 6
// was folded into 5 (decision 25): a server built from main between the
// admin ops and the fold refuses what it does not know as an unknown
// operation or argument, with code invalid, and serves everything else.
// The keys frame, with which an sshd command asked the daemon about a
// key, came and went in the same days (decision 22) and was never client
// protocol; a daemon closes a connection that opens with one,
// unanswered.
//
// The security review of stage 3a (design §14, decisions 35, 36 and 38)
// joined 5 the same way, while no release spoke it. member.set's
// arguments gained accept_keys, and its result lost keys, a list of
// fingerprints, and gained carried, the keys with who added each, and
// the change "pending". For the lists of what one account left behind,
// keys gained the arguments added_by, limit and after, and name became
// optional beside them; principals gained created_by; and members gained
// added_by. Their results gained fields (a key's principal, a member's
// project and project_name, keys' added_by and more), and
// principal.disable's gained keys_added, principals_created and
// members_added. A server built from main before these refuses the new
// arguments as unknown, with code invalid, and adds a principal without
// showing its keys, as it did; a client built from main before them
// never sends accept_keys, so against this server it cannot add a
// principal who has keys, and reads "pending" as no change.
//
// Traces (design §16.4, decision 32) joined 5 the same way, while no
// release spoke it: the principal.whois op, which resolves the stand-in
// a server's spans carry in place of a principal's name, and the status
// result's traces, which says how people appear in those spans. A server
// built from main before it refuses the op as unknown, with code
// invalid, and leaves the field out. The
// trace context a tool call brings `sfx mcp` rides tp and ts as the
// environment's does, so the wire did not change for it.
//
// 6 (not yet released; stage 5, slice F2, design §18.5) adds an
// idempotency key, idem, to the arguments of update, label.add,
// label.rm, dep.add, accept and usage, so that an outbox can queue them
// (ProtoKeys, Keyed); and the code stale, for a key used longer ago than
// the server's window, idem_window under limits:, 7 days by default. It
// is the first bump made by the rule above: ProtoMin stays at 5. A
// server of 6 serves a client of 5, which sends no such key, and sends
// it conflict where it would send a client of 6 stale, as protocol 5
// did for a key past its 24 hours. A client of 6 speaks 5 to a server of
// 5: refused at its first hello with code version and that server's
// range, it says hello again speaking 5 (Speak), on the same SSH
// connection; it then sends those six ops without their key
// (ForProtocol), and its outbox neither queues them nor sends them
// (Keyed), since a server of 5 cannot apply them once. Though no release
// speaks 5 or 6, 6 was not folded into 5, as additions to an unreleased
// version were: the design decided a version of its own (§18.5, §18.9),
// and the outbox of slice F1, built on 5, has entries on disk that a
// client of 6 must still send to a server of 5.
//
// 7 (not yet released; stage 5, slice F4, design §18.7) adds conflicts:
// park on update, which a replayed update sends so that the server
// compares each field with the state at its rev and parks what someone
// else changed, instead of refusing it as stale; and the ops conflicts,
// conflict.propose and resolve. Its results gained fields, which need no
// version: update's parked, dropped and comment, show's conflicts,
// inbox's conflicts, and digest's conflicts. ProtoMin moved to 6, by the
// rule above, so a client of 5, which no release is, is refused. A
// client of 7 speaks 6 to a server of 6 (Speak) and sends update without
// park (ForProtocol): a stale update it replays there is refused as
// conflict, as before, and waits in the outbox's refused list. A server
// of 5 alone is too old for it, and refuses it with code version.
//
// 8 (not yet released; stage 5, slice F5, design §18.6) adds park to
// finish and handoff, which an outbox sends when it replays them: a
// finish or handoff made under a claim whose lease ran out before it
// arrived still applies if nobody took the issue since, and is
// otherwise kept as a comment, with a finish's discovered issues
// created, rather than refused. Without park the server fences them as
// it always did. Their results gained lapsed and comment, which need no
// version. ProtoMin moved to 7, by the rule above, so a client of 6,
// which no release is, is refused. A client of 8 speaks 7 to a server of
// 7 (Speak) and sends finish and handoff without park there
// (ForProtocol): one that arrives after the issue moved on is refused,
// as before, and waits in the outbox's refused list. A server of 6 alone
// is too old for it, and refuses it with code version.
//
// Stage 6's first slice, G0 (design §20.3), joined 8 the same way, while
// no release spoke it: on 15 Oct 2026 the maintainer set aside decision
// 77's bump for each slice for work no release has spoken, so additions
// join the current unreleased version, and at the next release the
// protocol is settled to a single version above any released one and the
// step-down code for the versions in between removed. G0 added the ops
// gate.add, gate.pass, gate.waive, gate.rearm, gates and dates, and the
// arguments defer_until and due on create and update. Its results gained
// fields, which need no version: show's gate and gates, blocked's gates,
// list's and ready's due_at and defer_until, and digest's ready,
// due_soon and overdue. A server built from main before G0 refuses the
// new ops and arguments as unknown, with code invalid and a fix that
// says to upgrade, and `sfx mcp`'s prime goes without dates there.
//
// The bridge frame is not part of this protocol: it passes between two
// starfixd processes on the server (frame.go), and carries no version.
// It is read leniently, so a bridge from before a binary swap works
// against the daemon after it, and the reverse.
const (
	Proto    = 8
	ProtoMin = 7
	ProtoMax = 8
)

// Speak returns the protocol this build speaks to a server that accepts
// lo to hi, as its welcome or its refusal of a hello says: the newest of
// ProtoMin to Proto that the server accepts, and false when it accepts
// none of them. A client says hello speaking Proto; refused with code
// version by a server that accepts an older one of its own, it says
// hello again speaking that (design §18.5, principle 9).
func Speak(lo, hi int) (int, bool) {
	v := min(Proto, hi)
	if v < max(ProtoMin, lo) {
		return 0, false
	}
	return v, true
}

// CheckProto returns nil when the client protocol version p is in
// [lo, hi], and otherwise a CodeVersion error that tells the person which
// side to upgrade. A range that is empty, or starts below 1, refuses every
// client.
//
// The server runs it on a hello and sends the error in its welcome; the
// client runs it on the welcome's range. A client below the range is
// therefore told by the server, in words a released client prints as
// they are: its fix names `sfx upgrade`, which asks the releases and not
// the server, so it works for a client the server refuses; the install
// script, for one too old to have it; and the agent to restart, since
// the `sfx mcp` an agent's harness started is the old binary until then.
func CheckProto(p, lo, hi int, serverVersion string) *Error {
	switch {
	case lo < 1 || hi < lo:
		return Errf(CodeVersion, "ask the server admin to check starfixd's protocol range",
			fmt.Sprintf("server advertises an empty protocol range [%d,%d]", lo, hi))
	case p < lo:
		return Errf(CodeVersion, "upgrade this client: run `sfx upgrade`, or install the latest release (docs/install.md), then restart any agent that runs `sfx mcp`",
			fmt.Sprintf("starfix speaks protocol %d; the server (%s) needs %s", p, orDev(serverVersion), protoRange(lo, hi)))
	case p > hi:
		return Errf(CodeVersion, "ask the server admin to run `starfixd upgrade`, or install the starfix release that matches the server",
			fmt.Sprintf("starfix speaks protocol %d; the server (%s) is older and accepts %s", p, orDev(serverVersion), protoRange(lo, hi)))
	}
	return nil
}

// protoRange names the versions lo to hi in a message.
func protoRange(lo, hi int) string {
	if lo == hi {
		return fmt.Sprintf("protocol %d", lo)
	}
	return fmt.Sprintf("protocols %d to %d", lo, hi)
}

// orDev names a server version in a message. A string that is neither a
// version nor "dev" came from a server that may be hostile, so it is never
// echoed (C-5).
func orDev(v string) string {
	if v == "dev" || ValidVersion(v) {
		return v
	}
	return "unknown version"
}

// maxVersion bounds a version string. Release tags are far shorter
// (release.TagPattern); the bound keeps a server-sent string from growing a
// message.
const maxVersion = 64

// versionPattern is a semantic version 2.0 with a "v" prefix: numeric
// identifiers without leading zeros, prerelease and build identifiers
// non-empty and drawn from [0-9A-Za-z-]. Anything else, including control
// characters, spaces and non-ASCII, is not a version.
var versionPattern = lazyregexp.New(`^v(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})` +
	`(?:-((?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?` +
	`(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

// ValidVersion reports whether s is a release version that CompareVersions
// orders: vMAJOR.MINOR.PATCH, an optional -prerelease and +build, at most
// 64 bytes. Callers treat any other string from a server as unknown.
func ValidVersion(s string) bool {
	_, ok := parseVersion(s)
	return ok
}

// CompareVersions compares two release versions of the form
// vMAJOR.MINOR.PATCH, with an optional -prerelease that sorts before the
// release and +build metadata that is ignored, by semantic versioning 2.0
// precedence. ok is false when either is not a valid version (for example
// "dev", or a hostile string from a server); callers then stay quiet.
func CompareVersions(a, b string) (cmp int, ok bool) {
	pa, oka := parseVersion(a)
	pb, okb := parseVersion(b)
	if !oka || !okb {
		return 0, false
	}
	for i := range 3 {
		if c := cmpInt(pa.n[i], pb.n[i]); c != 0 {
			return c, true
		}
	}
	switch {
	case len(pa.pre) == 0 && len(pb.pre) == 0:
		return 0, true
	case len(pa.pre) == 0:
		return 1, true
	case len(pb.pre) == 0:
		return -1, true
	}
	for i := range min(len(pa.pre), len(pb.pre)) {
		if c := cmpIdent(pa.pre[i], pb.pre[i]); c != 0 {
			return c, true
		}
	}
	return cmpInt(len(pa.pre), len(pb.pre)), true
}

// cmpIdent orders two prerelease identifiers: numeric ones numerically and
// below alphanumeric ones, which compare in ASCII order. The pattern bounds
// a numeric identifier only by maxVersion, so numbers compare by length
// first (no leading zeros) rather than by parsing.
func cmpIdent(a, b string) int {
	na, nb := isNumeric(a), isNumeric(b)
	switch {
	case na && nb:
		if c := cmpInt(len(a), len(b)); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	case na:
		return -1
	case nb:
		return 1
	}
	return strings.Compare(a, b)
}

func isNumeric(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// semver is a parsed version: major, minor and patch, and the prerelease
// identifiers. Build metadata is dropped, since it does not affect
// precedence.
type semver struct {
	n   [3]int
	pre []string
}

// parseVersion parses s, reporting false unless it is a version as
// ValidVersion defines one.
func parseVersion(s string) (semver, bool) {
	var v semver
	if len(s) > maxVersion {
		return v, false
	}
	m := versionPattern.FindStringSubmatch(s)
	if m == nil {
		return v, false
	}
	for i := range 3 {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return v, false
		}
		v.n[i] = n
	}
	if m[4] != "" {
		v.pre = strings.Split(m[4], ".")
	}
	return v, true
}

// OlderClientWarning returns the one-line warning for a client older than
// its server, or "" when there is nothing to say.
func OlderClientWarning(client, server string) string {
	if c, ok := CompareVersions(client, server); ok && c < 0 {
		return fmt.Sprintf("starfix %s is older than the server (%s); run `sfx upgrade`", client, server)
	}
	return ""
}

// OlderServerWarning returns the one-line warning for a server older than
// the latest release it knows of, or "".
func OlderServerWarning(server, latest string) string {
	if c, ok := CompareVersions(server, latest); ok && c < 0 {
		return fmt.Sprintf("the server runs starfixd %s; %s is out: ask the admin to run `starfixd upgrade`", server, latest)
	}
	return ""
}
