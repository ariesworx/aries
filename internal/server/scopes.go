package server

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/ariesworx/aries/internal/proto"
)

// A connection that signed in with a token holds the token's scopes
// (transport.md §6.8); one that came by a key, over SSH or by the key
// proof, holds none, and is limited only by its principal's role, as
// before tokens. The scope cli allows every op. opScopes says, for every
// op, which other scopes allow it: mcp, the ops behind the MCP tools,
// and ci, a member's ops from a CI job. An op missing from the table is
// allowed to cli alone, and TestEveryOpHasScopes fails until it is added.

// opScopes is, for every op, the scopes besides cli whose tokens may
// send it. It is made at its first use, not as ariesd starts: sshd
// starts an `ariesd stdio` for every connection, which never reads it
// (TestInitAllocations).
var opScopes = sync.OnceValue(func() map[string][]string {
	// none, ciOnly and both name the scopes besides cli an op is open to.
	none, ciOnly, both := []string{}, []string{proto.ScopeCI}, []string{proto.ScopeMCP, proto.ScopeCI}
	return map[string][]string{
		// Issues: an agent's and a CI job's work alike.
		proto.OpCreate: both, proto.OpShow: both, proto.OpList: both, proto.OpReady: both, proto.OpBlocked: both,
		proto.OpUpdate: both, proto.OpClose: both, proto.OpReopen: both, proto.OpDepAdd: both, proto.OpDepRm: both,
		proto.OpLabelAdd: both, proto.OpLabelRm: both, proto.OpComment: both, proto.OpComments: both, proto.OpHistory: both,
		proto.OpStart: both, proto.OpFinish: both, proto.OpHandoff: both, proto.OpDigest: both, proto.OpRenew: both,
		proto.OpWho: both, proto.OpDates: both, proto.OpWatch: both,
		proto.OpAccept: ciOnly, proto.OpClaims: ciOnly, proto.OpUsage: ciOnly, proto.OpPin: ciOnly,
		// Memory and the inbox.
		proto.OpRemember: both, proto.OpRecall: both, proto.OpForget: both, proto.OpInbox: both, proto.OpAck: both,
		// Conflicts: an agent proposes; a person resolves.
		proto.OpConflicts: both, proto.OpPropose: both, proto.OpResolve: none,
		// Gates: anyone adds and lists; a CI job reports a ci gate's check
		// from the session ci, and the store refuses it a person's gate; a
		// waive is a person's alone (decision 68).
		proto.OpGateAdd: both, proto.OpGates: both,
		proto.OpGatePass: ciOnly, proto.OpGateFail: ciOnly, proto.OpGateRearm: ciOnly, proto.OpGateWaive: none,
		// Time and cost.
		proto.OpCost: both, proto.OpHours: ciOnly, proto.OpHoursLog: ciOnly, proto.OpHoursDelete: ciOnly,
		proto.OpPrices: ciOnly, proto.OpPlans: ciOnly, proto.OpPriceSet: none, proto.OpPlanSet: none,
		// Locks: over the CLI, never an agent's (decision 70); a CI job holds
		// one around a deploy.
		proto.OpLockAcquire: ciOnly, proto.OpLockRenew: ciOnly, proto.OpLockRelease: ciOnly, proto.OpLockLeave: ciOnly, proto.OpLocks: ciOnly,
		// The workflow: any member reads it, prime among them, and a CI job
		// checks it against the repository's file; a project admin sets
		// it, at a terminal (design §21.3).
		proto.OpWorkflow: both, proto.OpWorkflowSet: none,
		// Access and the server: a person's, at a terminal (rule 12).
		proto.OpProjects: none, proto.OpProjectAdd: none, proto.OpProjectArchive: none, proto.OpProjectActivate: none,
		proto.OpPrincipals: none, proto.OpPrincipalAdd: none, proto.OpPrincipalDisable: none, proto.OpPrincipalEnable: none,
		proto.OpPrincipalWhois: none, proto.OpMembers: none, proto.OpMemberSet: none, proto.OpMemberRemove: none,
		proto.OpKeys: none, proto.OpKeyAdd: none, proto.OpKeyRemove: none, proto.OpStatus: none,
		proto.OpPrincipalSet: none, proto.OpPrincipalShow: none, proto.OpInviteAdd: none, proto.OpInvites: none,
		proto.OpInviteRemove: none, proto.OpIdentities: none, proto.OpIdentityRemove: none,
		proto.OpSignIns: none, proto.OpSignInEnd: none,
		proto.OpTrustAdd: none, proto.OpTrustRules: none, proto.OpTrustShow: none, proto.OpTrustSet: none, proto.OpTrustRemove: none,
	}
})

// mayCall refuses op to a connection that signed in with in, unless
// one of its scopes allows it. A nil in, a connection that came by a
// key, may send every op.
func mayCall(in *signedIn, op string) *proto.Error {
	if in == nil || slices.Contains(in.scopes, proto.ScopeCLI) {
		return nil
	}
	allowed, known := opScopes()[op]
	for _, s := range in.scopes {
		if known && slices.Contains(allowed, s) {
			return nil
		}
	}
	return proto.Errf(proto.CodeForbidden,
		"ask a person to run it with `aries` at their terminal, signed in with `aries login`",
		fmt.Sprintf("%s is not allowed to a sign-in of scope %s", op, strings.Join(in.scopes, " ")))
}

// mayTakeSession refuses a session a connection that signed in with in
// may not take: the session cli, a person's terminal, only with the
// scope cli, and the session ci, a CI job's, only with cli or ci. A nil
// in, a connection that came by a key, takes any.
func mayTakeSession(in *signedIn, session string) *proto.Error {
	if in == nil || slices.Contains(in.scopes, proto.ScopeCLI) {
		return nil
	}
	switch {
	case session == proto.CLISession:
	case session == proto.CISession && !slices.Contains(in.scopes, proto.ScopeCI):
	default:
		return nil
	}
	return proto.Errf(proto.CodeForbidden,
		"run aries with a session of its own (ARIES_SESSION, or the harness's), or sign in at a terminal with `aries login` for the session cli",
		fmt.Sprintf("the session %s is not open to a sign-in of scope %s", session, strings.Join(in.scopes, " ")))
}
