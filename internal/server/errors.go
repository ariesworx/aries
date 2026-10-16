package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

// command is the CLI spelling of an op, for fix lines.
func command(op string) string {
	switch op {
	case proto.OpAck:
		return "inbox" // aries inbox --ack
	case proto.OpPriceSet:
		return "admin price set"
	case proto.OpPrices:
		return "admin price list"
	case proto.OpHoursLog, proto.OpHoursDelete, proto.OpHours:
		return "hours"
	case proto.OpPlanSet:
		return "admin plan set"
	case proto.OpPlans:
		return "admin plan list"
	case proto.OpProjects:
		return "admin project list"
	case proto.OpPrincipals:
		return "admin principal list"
	case proto.OpMemberSet:
		return "admin member add"
	case proto.OpMembers:
		return "admin member list"
	case proto.OpKeys:
		return "admin key list"
	case proto.OpInvites:
		return "admin invite list"
	case proto.OpIdentities:
		return "admin identity list"
	case proto.OpTrustRules:
		return "admin trust list"
	case proto.OpPropose:
		return "conflicts" // proposing is an agent's, over MCP; aries conflicts shows them
	case proto.OpResolve:
		return "resolve"
	}
	if adminOp(op) {
		return "admin " + strings.ReplaceAll(op, ".", " ")
	}
	return strings.ReplaceAll(op, ".", " ")
}

// mapErr turns a store error into a typed protocol error whose message
// names the cause and whose fix names the next action. id and rev are the
// request's target and expected revision, when it has them.
func (pr *served) mapErr(ctx context.Context, op, id string, rev int64, err error) *proto.Error {
	pr.observeLimit(op, err)
	text := err.Error()
	var stale *store.StaleEpochError
	var idem *store.IdemError
	var unmet *store.AcceptanceError
	var forbidden *store.ForbiddenError
	var state *store.StateError
	var batch *store.UsageBatchError
	var priceCap *store.PriceLimitError
	var planCap *store.PlanLimitError
	var hoursCap *store.HoursLimitError
	var dayFull *store.HoursDayError
	var access *store.AccessError
	var taken *store.TakenError
	var invited *store.InvitedError
	var controlCap *store.ControlLimitError
	var lastAdmin *store.LastAdminError
	var conflictCap *store.ConflictLimitError
	var gate *store.GateError
	if perr := lockErr(err); perr != nil {
		return perr
	}
	if served, ok := errors.AsType[*store.ServedError](err); ok {
		// The writer's lock was lost while its connection was down, and
		// another process took the schema: this daemon writes nothing to
		// it until its store is closed.
		return proto.Errf(proto.CodeUnavailable,
			fmt.Sprintf("ask the server's operator to stop whichever ariesd should not serve it (Postgres's pg_stat_activity shows each one's %q connection), then retry", store.FenceApplication),
			fmt.Sprintf("another process on the same database now serves this project's schema, %s, so this server writes nothing to it", served.Schema))
	}
	switch {
	case errors.As(err, &gate):
		return gateErr(gate)

	case errors.As(err, &access):
		return accessErr(access)

	case errors.As(err, &taken):
		return takenErr(taken)

	case errors.As(err, &invited):
		return proto.Errf(proto.CodeExists,
			fmt.Sprintf("remove that invitation first with `aries admin invite remove %d`, or invite another address", invited.ID),
			invited.Error())

	case errors.As(err, &controlCap):
		fix := fmt.Sprintf("ask the server's operator to raise %s under limits: in ariesd's config", controlCap.Limit)
		switch controlCap.Limit {
		case "keys_per_principal":
			fix = "remove a key the principal no longer uses with `aries admin key remove FINGERPRINT`, or " + fix
		case "principals_per_creator":
			fix = "ask a server admin to add the principal, or the server's operator to raise principals_per_creator under limits: in ariesd's config"
		case "invites_per_server":
			fix = "remove an invitation no longer wanted (`aries admin invite list`, then `aries admin invite remove ID`), or " + fix
		case "trust_rules_per_server", "trust_rules_per_principal":
			fix = "remove a trust rule no longer used (`aries admin trust list`, then `aries admin trust remove NAME`), or " + fix
		}
		return proto.Errf(proto.CodeInvalid, fix, strings.TrimPrefix(text, store.ErrInvalid.Error()+": "))

	case errors.As(err, &lastAdmin):
		return proto.Errf(proto.CodeInvalid,
			"enable another server admin first with `aries admin principal enable NAME`, or ask the server's operator to name another under admins: in ariesd's config",
			fmt.Sprintf("%s is the last server admin who is not disabled; without one, nobody could enable anyone from here", lastAdmin.Principal))

	case errors.Is(err, store.ErrInvalid) && op == proto.OpKeyAdd:
		return proto.Errf(proto.CodeInvalid, "give the public key file, such as ~/.ssh/id_ed25519.pub: `aries admin key add NAME FILE`",
			strings.TrimPrefix(text, store.ErrInvalid.Error()+": "))

	case errors.Is(err, store.ErrNotFound) && adminOp(op):
		return proto.Errf(proto.CodeNotFound, adminNotFoundFix(op),
			strings.Replace(text, ": "+store.ErrNotFound.Error(), " not found", 1))

	case errors.Is(err, store.ErrInvalid) && adminOp(op):
		return proto.Errf(proto.CodeInvalid, fmt.Sprintf("correct it and retry; `aries %s -h` shows the usage", command(op)),
			strings.TrimPrefix(text, store.ErrInvalid.Error()+": "))

	case errors.Is(err, store.ErrNotFound) && (op == proto.OpConflicts || op == proto.OpPropose || op == proto.OpResolve):
		return proto.Errf(proto.CodeNotFound, proto.FixFindConflict+" with `aries conflicts`",
			strings.Replace(text, ": "+store.ErrNotFound.Error(), " not found", 1))

	case errors.As(err, &conflictCap):
		return proto.Errf(proto.CodeInvalid,
			fmt.Sprintf("a person resolves the open conflicts first (`aries conflicts`, then `aries resolve N`), and then `aries outbox retry` sends it again; or ask the server admin to raise %s under limits: in ariesd's config", conflictCap.Limit),
			strings.TrimPrefix(text, store.ErrInvalid.Error()+": "))

	case errors.Is(err, store.ErrNotFound) && op == proto.OpHoursDelete:
		return proto.Errf(proto.CodeNotFound, "find the entry id with `aries hours --issue ID` or `aries hours --by PRINCIPAL`",
			strings.Replace(text, ": "+store.ErrNotFound.Error(), " not found", 1))

	case errors.Is(err, store.ErrNotFound):
		return proto.Errf(proto.CodeNotFound, "find the id with `aries list`",
			strings.Replace(text, ": "+store.ErrNotFound.Error(), " not found", 1))

	case errors.As(err, &forbidden):
		return forbiddenErr(forbidden)

	case errors.As(err, &dayFull):
		left := int64((store.MaxHoursEntry - dayFull.Logged) / time.Second)
		return proto.Errf(proto.CodeInvalid, fmt.Sprintf("you can log at most %s more on %s; undo an entry with `aries hours --undo ID` to change it",
			proto.Hours(left), dayFull.On.Format(time.DateOnly)), text)

	case errors.As(err, &stale):
		msg := fmt.Sprintf("your claim on %s (epoch %d) was lost; it is now epoch %d", stale.ID, stale.Epoch, stale.Current)
		if stale.By.Principal != "" {
			msg += fmt.Sprintf(", held by %s/%s", stale.By.Principal, stale.By.Session)
		}
		return proto.Errf(proto.CodeConflict,
			fmt.Sprintf("stop work on it; keep anything useful as a comment (`aries comment %s`), or start it again if it is free", stale.ID), msg)

	case errors.As(err, &idem) && idem.Expired:
		return proto.Errf(proto.CodeStale,
			"nothing to do if that was this request; if the change is still wanted, make it again by hand, which sends a new key (`aries outbox` shows what a queued write was)",
			text)

	case errors.As(err, &idem):
		return proto.Errf(proto.CodeConflict,
			fmt.Sprintf("send the request with a new idempotency key instead of %q; aries makes one per command", idem.Key), text)

	case errors.As(err, &unmet):
		return unmetErr(op, unmet)

	case errors.Is(err, store.ErrConflict):
		return pr.conflict(ctx, id, rev)

	case errors.Is(err, store.ErrExists):
		return proto.Errf(proto.CodeExists, "omit --id to have one generated, or read it with `aries show "+id+"`",
			strings.Replace(text, ": "+store.ErrExists.Error(), " already exists", 1))

	case errors.Is(err, store.ErrCycle):
		return proto.Errf(proto.CodeCycle, "remove an edge with `aries dep remove`, or choose another parent",
			strings.TrimSuffix(text, ": "+store.ErrCycle.Error())+", so this would make a cycle")

	case errors.Is(err, store.ErrStatusInProgress):
		return proto.Errf(proto.CodeInvalid, fmt.Sprintf(proto.FixStart+" `aries start %s`, which claims it", id),
			"status in_progress is set only by start")

	case errors.Is(err, store.ErrBusy):
		fix := "retry later"
		switch op {
		case proto.OpGateAdd:
			fix = "pass or waive an open gate first (`aries gate list`), or ask the server admin to raise open_gates_per_project under limits: in ariesd's config"
		case proto.OpUsage:
			fix = "retry later: the cap counts the last 24 hours; the server admin can raise usage_per_day under limits: in ariesd's config"
		}
		return proto.Errf(proto.CodeBusy, fix, strings.TrimPrefix(text, store.ErrBusy.Error()+": "))

	case errors.As(err, &batch):
		return proto.Errf(proto.CodeInvalid, fmt.Sprintf("send at most %d records per call; nothing from the batch was stored", batch.Max),
			strings.TrimPrefix(text, store.ErrInvalid.Error()+": "))

	case errors.As(err, &priceCap):
		return proto.Errf(proto.CodeInvalid,
			"replace an existing price (the same model and --from) instead, or ask the server admin to raise prices_per_project under limits: in ariesd's config",
			strings.TrimPrefix(text, store.ErrInvalid.Error()+": "))

	case errors.As(err, &planCap):
		return proto.Errf(proto.CodeInvalid,
			"replace an existing plan row (the same name and --from) instead, or ask the server admin to raise plans_per_project under limits: in ariesd's config",
			strings.TrimPrefix(text, store.ErrInvalid.Error()+": "))

	case errors.As(err, &hoursCap):
		return proto.Errf(proto.CodeInvalid,
			"combine entries: undo some with `aries hours --undo ENTRY` (`aries hours --by "+hoursCap.Principal+
				"` lists them) and log their sum, or ask the server admin to raise hours_per_day under limits: in ariesd's config",
			strings.TrimPrefix(text, store.ErrInvalid.Error()+": "))

	case errors.Is(err, store.ErrInvalid) && op == proto.OpUsage:
		return proto.Errf(proto.CodeInvalid, "correct or drop that record and send the batch again; nothing from the batch was stored",
			strings.TrimPrefix(text, store.ErrInvalid.Error()+": "))

	case errors.Is(err, store.ErrInvalid):
		if errors.As(err, &state) {
			msg := state.Error()
			switch state.Reason {
			case store.StateClaimed:
				return proto.Errf(proto.CodeInvalid,
					fmt.Sprintf(proto.FixRelease+" with `aries handoff %s --release` first; only the holder or an admin can", id),
					strings.TrimPrefix(msg, store.ErrInvalid.Error()+": "))
			case store.StateClosed:
				return proto.Errf(proto.CodeInvalid, fmt.Sprintf(proto.FixReopen+" it with `aries reopen %s`", id),
					strings.TrimSuffix(msg, "; reopen it first"))
			case store.StateAlreadyClosed, store.StateNotClosed:
				return proto.Errf(proto.CodeInvalid, proto.FixNothing, msg)
			}
		}
		return proto.Errf(proto.CodeInvalid, fmt.Sprintf("correct it and retry; `aries %s -h` lists the options", command(op)), text)

	case errors.Is(err, store.ErrSchemaTooNew):
		pr.log.ErrorContext(ctx, "schema too new", "event", logs.StoreSchemaTooNew, "op", op, "err", err)
		return proto.Errf(proto.CodeUnavailable, "the server admin should run the ariesd release that migrated it",
			"the database was migrated by a newer ariesd")

	case errors.Is(err, context.DeadlineExceeded):
		return proto.Errf(proto.CodeUnavailable, "retry; if it persists, the server admin should check the ariesd log",
			fmt.Sprintf("the server timed out on %s", op))
	}
	pr.log.ErrorContext(ctx, "request failed", "event", logs.RequestFailed, "op", op, "err", err)
	return proto.Errf(proto.CodeUnavailable, "retry; if it persists, the server admin should check the ariesd log",
		fmt.Sprintf("the server could not complete %s", op))
}

// accessPhrase says what an AccessError refused, in the words of the
// `aries admin` command, naming only what the request named.
func accessPhrase(e *store.AccessError) string {
	in := ""
	if e.Project != "" {
		in = " in project " + e.Project
	}
	switch string(e.Op) {
	case proto.OpProjectAdd:
		return "add a project"
	case proto.OpProjectArchive:
		return "archive project " + e.Project
	case proto.OpProjectActivate:
		return "activate project " + e.Project
	case proto.OpPrincipalAdd:
		return "add principal " + e.Principal + in
	case proto.OpPrincipals:
		return "list the principals"
	case proto.OpPrincipalDisable:
		return "disable " + e.Principal
	case proto.OpPrincipalEnable:
		return "enable " + e.Principal
	case proto.OpMemberSet:
		return "set the role of " + e.Principal + in
	case proto.OpMemberRemove:
		return "remove " + e.Principal + " from project " + e.Project
	case proto.OpMembers:
		return "list the members of project " + e.Project
	case proto.OpKeyAdd:
		return "add a key to " + e.Principal
	case proto.OpKeyRemove:
		if e.Principal == "" {
			return "remove that key"
		}
		return "remove a key of " + e.Principal
	case proto.OpPrincipalSet:
		return "set " + e.Principal
	case string(store.OpIdentityInvite):
		return "invite an identity for " + e.Principal
	case string(store.OpIdentityRemove):
		return "remove an identity"
	case proto.OpInvites:
		return "list the invitations"
	case string(store.OpSignInEnd):
		return "end a sign-in of " + e.Principal
	case proto.OpTrustAdd:
		if e.Principal == "" {
			return "add a trust rule"
		}
		return "add a trust rule for " + e.Principal
	case proto.OpTrustSet:
		if e.Principal == "" {
			return "change a trust rule"
		}
		return "change a trust rule of " + e.Principal
	case proto.OpTrustRemove:
		if e.Principal == "" {
			return "remove a trust rule"
		}
		return "remove a trust rule of " + e.Principal
	}
	return strings.TrimSpace(string(e.Op) + " " + e.Principal + in)
}

// accessErr refuses a change to the control store the caller may not
// make (store.AccessError): forbidden, with the rule that applied and a
// fix that names who can.
func accessErr(e *store.AccessError) *proto.Error {
	msg := fmt.Sprintf("%s may not %s: ", e.Actor, accessPhrase(e))
	switch e.Reason {
	case store.AccessAdmin:
		if e.Project != "" {
			return proto.Errf(proto.CodeForbidden, "ask one of the project's admins, or a server admin, to run it",
				msg+"it is for the project's admins and server admins")
		}
		return proto.Errf(proto.CodeForbidden,
			"ask a server admin to run it, or a project admin who administers every project the principal belongs to",
			msg+"it is for project admins and server admins")
	case store.AccessOwnKeys:
		return proto.Errf(proto.CodeForbidden,
			"ask another admin to run it: a server admin, or a project admin who administers every project you belong to; the server's operator can also change authorized_keys on the server",
			msg+"nobody adds a key to their own account")
	case store.AccessOwnTrust:
		return proto.Errf(proto.CodeForbidden,
			"ask another server admin to run it: a trust rule lets whoever runs the jobs it admits sign in as the principal",
			msg+"nobody manages a trust rule of their own principal")
	case store.AccessOwnIdentity:
		return proto.Errf(proto.CodeForbidden,
			"ask another server admin to run it: an invitation lets whoever holds the address sign in as the principal",
			msg+"nobody invites an identity onto their own principal")
	case store.AccessReach:
		if e.Principal == "" {
			// A key named by its fingerprint. The refusal is the same
			// for a key that does not exist, and says nothing of whose.
			return proto.Errf(proto.CodeForbidden,
				"check the fingerprint with `aries admin key list NAME`, or ask a server admin to run it",
				msg+fmt.Sprintf("it is no key of a principal %s may manage", e.Actor))
		}
		// The same for a name no one has as for a principal out of reach.
		return proto.Errf(proto.CodeForbidden,
			"check the name, or ask a server admin to run it; a project admin may only while every project the principal belongs to is one they administer",
			msg+fmt.Sprintf("%s does not exist, or belongs to a project %s does not administer, or to none", e.Principal, e.Actor))
	case store.AccessKey:
		// The same whoever has the key, and it does not say that anyone
		// does.
		return proto.Errf(proto.CodeForbidden,
			fmt.Sprintf("make a new key for %s and add that, or ask a server admin to run it", e.Principal),
			fmt.Sprintf("%s may not add that key to %s: it is no key %s may give a principal", e.Actor, e.Principal, e.Actor))
	case store.AccessServerAdminPrincipal:
		return proto.Errf(proto.CodeForbidden, "ask a server admin to run it",
			msg+e.Principal+" is a server admin")
	case store.AccessUnassigned:
		return proto.Errf(proto.CodeForbidden,
			fmt.Sprintf("ask a server admin to add %s to the project; a project admin adds only a principal that already belongs to a project, or one they created", e.Principal),
			msg+fmt.Sprintf("%s belongs to no project and %s did not create it", e.Principal, e.Actor))
	}
	return proto.Errf(proto.CodeForbidden, "ask a server admin (admins: in ariesd's config) to run it", msg+"it is for server admins")
}

// takenErr refuses a new project or principal whose name is in use, or a
// key another principal has, without saying whose.
func takenErr(e *store.TakenError) *proto.Error {
	switch e.Kind {
	case store.TakenProjectName:
		return proto.Errf(proto.CodeExists, "choose another name; `aries admin project list` shows the projects you can see",
			fmt.Sprintf("a project named %q already exists", e.Value))
	case store.TakenPrincipal:
		return proto.Errf(proto.CodeExists,
			fmt.Sprintf("choose another name, or if %s is the person you mean, add them with `aries admin member add %s`", e.Value, e.Value),
			fmt.Sprintf("a principal named %q already exists", e.Value))
	case store.TakenKey:
		return proto.Errf(proto.CodeExists, "use another key: a key belongs to one principal, and an agent or a machine that needs less access gets a key and a principal of its own",
			fmt.Sprintf("key %s already belongs to another principal", e.Value))
	}
	return proto.Errf(proto.CodeConflict, "retry; if it persists, the server admin should check the ariesd log", e.Error())
}

// adminNotFoundFix is the next step when an admin op names a project,
// principal or membership that does not exist.
func adminNotFoundFix(op string) string {
	switch op {
	case proto.OpMemberRemove:
		return "see who is a member with `aries admin member list`"
	case proto.OpMemberSet:
		return "add the principal first with `aries admin principal add NAME`"
	case proto.OpPrincipalDisable, proto.OpPrincipalEnable, proto.OpKeyAdd, proto.OpKeys,
		proto.OpPrincipalSet, proto.OpPrincipalShow, proto.OpInviteAdd, proto.OpIdentities:
		return "check the name; a server admin lists them with `aries admin principal list`"
	case proto.OpKeyRemove:
		return "list a principal's keys, with their fingerprints, with `aries admin key list NAME`"
	case proto.OpInviteRemove:
		return "list the invitations, with their ids, with `aries admin invite list`"
	case proto.OpIdentityRemove:
		return "list a principal's identities, with their ids, with `aries admin identity list NAME`"
	case proto.OpSignInEnd:
		return "list a principal's sign-ins, with their ids, with `aries admin signin list NAME`"
	case proto.OpSignIns, proto.OpTrustRules:
		return "check the name; a server admin lists them with `aries admin principal list`"
	case proto.OpTrustAdd:
		return "check --principal; a server admin lists the principals with `aries admin principal list`"
	case proto.OpTrustShow, proto.OpTrustSet, proto.OpTrustRemove:
		return "list the trust rules with `aries admin trust list`"
	}
	return "list the projects with `aries admin project list`"
}

// unmetErr refuses a close or finish with acceptance items open, naming
// the command that ticks them; an admin may also force a close. An
// update that drops open items is told to settle them first.
func unmetErr(op string, e *store.AcceptanceError) *proto.Error {
	nums := make([]string, len(e.Open))
	for i, n := range e.Open {
		nums[i] = strconv.Itoa(n)
	}
	if e.Dropped {
		return proto.Errf(proto.CodeAcceptance,
			fmt.Sprintf("tick them with `aries accept %s %s`, or waive each with `aries accept %s N --waive REASON`, "+proto.FixEditText,
				e.ID, strings.Join(nums, " "), e.ID), e.Error())
	}
	fix := fmt.Sprintf("tick what is met with `aries accept %s %s`, or waive an item with `aries accept %s N --waive REASON`",
		e.ID, strings.Join(nums, " "), e.ID)
	if op == proto.OpClose {
		fix += "; an aries admin can close it anyway with --force, on the record"
	}
	return proto.Errf(proto.CodeAcceptance, fix, e.Error())
}

// forbiddenErr refuses a change to an issue another principal holds,
// naming the holder and the ways forward, or an admin-only action.
func forbiddenErr(e *store.ForbiddenError) *proto.Error {
	if e.Action != "" && e.ID == "" {
		return proto.Errf(proto.CodeForbidden, "ask an aries admin (admins: in ariesd's config) to run it",
			fmt.Sprintf("%s is for aries admins", e.Action))
	}
	if e.Action != "" {
		return proto.Errf(proto.CodeForbidden,
			fmt.Sprintf("tick or waive the open items with `aries accept %s N`, or ask an aries admin to run it", e.ID),
			fmt.Sprintf("%s is for aries admins", e.Action))
	}
	h := e.Holder
	return proto.Errf(proto.CodeForbidden,
		fmt.Sprintf(proto.FixAsk+"%s to hand it off (`aries handoff %s --release`), wait for the lease to run out, or ask an aries admin; `aries comment %s` works on any issue",
			h.Principal, e.ID, e.ID),
		fmt.Sprintf("%s is held by %s (session %s) until %s; only the holder or an admin may change it",
			e.ID, h.Principal, h.Session, e.Until.UTC().Format(time.RFC3339)))
}

// conflict explains a failed compare-and-swap: who moved the issue to
// which revision, or that concurrent writers kept winning.
func (pr *served) conflict(ctx context.Context, id string, rev int64) *proto.Error {
	reread := fmt.Sprintf(proto.FixReread+" with `aries show %s`", id)
	if id == "" || rev < 1 {
		return proto.Errf(proto.CodeConflict, "retry", "the change lost to concurrent writes")
	}
	cur, err := pr.store.GetIssue(ctx, store.IssueID(id))
	if err != nil || int64(cur.Rev) == rev {
		return proto.Errf(proto.CodeConflict, "retry", fmt.Sprintf("%s kept changing under concurrent writes", id))
	}
	// Only issue.* events move rev; labels, deps and comments do not.
	by := ""
	if page, err := pr.store.HistoryPage(ctx, store.IssueID(id), "", 50); err == nil {
		for _, ev := range slices.Backward(page.Events) {
			if strings.HasPrefix(string(ev.Op), "issue.") {
				by = " by " + ev.Actor.Principal
				break
			}
		}
	}
	return proto.Errf(proto.CodeConflict, reread,
		fmt.Sprintf("%s changed since rev %d (now rev %d%s)", id, rev, cur.Rev, by))
}

// held explains a refused start: another session of the caller's own
// principal holds the issue, which --take overrides, or another principal
// does, and the next ready issue is named instead (design §12 item 5), as
// the caller a's own ready would rank it. Only start returns a HeldError,
// so start calls held rather than mapErr, which has no caller.
func (pr *served) held(ctx context.Context, a store.Actor, h *store.HeldError) *proto.Error {
	if h.Own {
		return proto.Errf(proto.CodeConflict,
			fmt.Sprintf(proto.FixTakeOver+" with `aries start %s --take` only if that session has stopped; it loses the claim", h.ID),
			fmt.Sprintf("%s is held by your session %s", h.ID, h.Session))
	}
	msg := fmt.Sprintf("%s is in progress by %s", h.ID, h.By)
	next, err := pr.store.Ready(ctx, a, 1)
	if err != nil || len(next) == 0 {
		return proto.Errf(proto.CodeConflict, fmt.Sprintf(proto.FixLeaveIt+" %s; nothing else is ready, so see `aries blocked`", h.By),
			msg+"; nothing else is ready")
	}
	return proto.Errf(proto.CodeConflict, fmt.Sprintf(proto.FixTakeNext+" with `aries start %s`", next[0].ID),
		fmt.Sprintf("%s; next ready: %s", msg, next[0].ID))
}
