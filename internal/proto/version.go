package proto

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/ariesworx/aries/internal/lazyregexp"
)

// Protocol versions. A client speaks Proto; a server accepts any client in
// [ProtoMin, ProtoMax].
//
// v0.5.0 settled the protocol at 8, spoken alone (design §11, decisions
// 78 and 88): every release before it spoke at most 4, and no release
// spoke 5 to 7, so a client or server of any other version is refused at
// the handshake with code version, and no code steps down to another.
//
// From v0.5.0 on (decision 93), principle 9 holds: a change raises
// Proto and ProtoMax and keeps ProtoMin at the version before, so that
// a new server still serves the clients people already have (AGENTS.md, rule
// 10). TestReleasedClients pins which clients are refused, and what each
// is told.
//
// Protocol 11 is the release after v0.7.0's: the op brief records the
// brief a session is started with (BriefArgs, design §22), which a
// protocol 10 server refuses as an unknown op and a protocol 10 client
// never sends. It moves ProtoMin to 10, v0.7.0's.
//
// Protocol 10 was v0.7.0's: price.set takes tier, the rates of a model
// priced by prompt length (PriceSetArgs), which a protocol 9 server
// refuses as an unknown field and a protocol 9 client never sends. It
// moves ProtoMin to 9, v0.6.0's: v0.5.0's clients, and the key proof they
// sign (refuse_key_proof_v1), are no longer served.
//
// Protocol 9 was v0.6.0's, the first bump since v0.5.0; a release raises
// the protocol once, and later changes before it ships join it. Two
// changes make it: handoff takes ticked and waived (HandoffArgs), which
// a protocol 8 server refuses as unknown fields and a protocol 8 client
// never sends; and the HTTPS key proof is bound to the TLS session where
// ariesd terminates TLS (internal/keyproof), its client signing the v2
// transcript alone. The server still takes protocol 8's v1 proof, and so
// 8's clients, until ProtoMin moves to 9; https: refuse_key_proof_v1
// refuses them sooner.
//
// A refused client is told which side to upgrade (CheckProto). A client
// older than the server prints the message and fix the server sends, as
// they are, so those words are all a released client can be told; a
// client newer than the server writes its own. `ariesd upgrade` is run
// by the old binary, whose health check then speaks the old protocol to
// the new daemon: the daemon answers that check whatever protocol it
// speaks (server.ProbePrincipal), or no upgrade could cross a raised
// minimum.
//
// The bridge frame is not part of this protocol: it passes between two
// ariesd processes on the server (frame.go), and carries no version.
// It is read leniently, so a bridge from before a binary swap works
// against the daemon after it, and the reverse.
const (
	Proto    = 11
	ProtoMin = 10
	ProtoMax = 11
)

// CheckProto returns nil when the client protocol version p is in
// [lo, hi], and otherwise a CodeVersion error that tells the person which
// side to upgrade. A range that is empty, or starts below 1, refuses every
// client.
//
// The server runs it on a hello and sends the error in its welcome; the
// client runs it on the welcome's range. A client below the range is
// therefore told by the server, in words a released client prints as
// they are: its fix names `aries upgrade`, which asks the releases and not
// the server, so it works for a client the server refuses; the install
// script, for one too old to have it; and the agent to restart, since
// the `aries mcp` an agent's harness started is the old binary until then.
func CheckProto(p, lo, hi int, serverVersion string) *Error {
	switch {
	case lo < 1 || hi < lo:
		return Errf(CodeVersion, "ask the server admin to check ariesd's protocol range",
			fmt.Sprintf("server advertises an empty protocol range [%d,%d]", lo, hi))
	case p < lo:
		return Errf(CodeVersion, "upgrade this client: run `aries upgrade`, or install the latest release (docs/install.md), then restart any agent that runs `aries mcp`",
			fmt.Sprintf("aries speaks protocol %d; the server (%s) needs %s", p, orDev(serverVersion), protoRange(lo, hi)))
	case p > hi:
		return Errf(CodeVersion, "ask the server admin to run `ariesd upgrade`, or install the aries release that matches the server",
			fmt.Sprintf("aries speaks protocol %d; the server (%s) is older and accepts %s", p, orDev(serverVersion), protoRange(lo, hi)))
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
		return fmt.Sprintf("aries %s is older than the server (%s); run `aries upgrade`", client, server)
	}
	return ""
}
