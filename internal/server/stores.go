package server

import (
	"context"
	"time"

	"github.com/ariesworx/aries/internal/store"
)

// What the server asks of its stores, by area (decision 79, slice P0).
// The server holds a project's store as a [Store] and the control store
// as a [Control], so that its own logic (dispatch, error mapping,
// authorization at the handshake, limits, the registry) can be tested
// against a fake, and so that the store's insides can change without
// the server. [*store.Store] and [*store.Control] are the
// implementations; the domain types, errors and the clock stay in
// package store, which both sides share.
//
// Opening a store is not here: that is the caller's ([Config.Open],
// [ProjectOpener]), and so is what only cmd/ariesd asks of a store it
// opened itself, such as a tag before an upgrade.

// Store is a project's store, as the server uses it.
type Store interface {
	Issues
	Graph
	Work
	Memory
	Cost
	Gates
	Conflicts
	Upkeep
}

// Issues is what the server does with one issue: its row, labels,
// comments, history and acceptance criteria.
type Issues interface {
	CreateIssue(ctx context.Context, actor store.Actor, in store.NewIssue) (store.Issue, error)
	GetIssue(ctx context.Context, id store.IssueID) (store.Issue, error)
	ShowIssue(ctx context.Context, actor store.Actor, id store.IssueID, n int) (store.IssueView, error)
	UpdateIssue(ctx context.Context, actor store.Actor, id store.IssueID, expected store.Rev, patch store.IssuePatch) (store.Issue, error)
	CloseIssue(ctx context.Context, actor store.Actor, id store.IssueID, expected store.Rev, reason string) (store.Issue, error)
	ForceClose(ctx context.Context, actor store.Actor, id store.IssueID, expected store.Rev, reason string) (store.Issue, error)
	ReopenIssue(ctx context.Context, actor store.Actor, id store.IssueID, expected store.Rev) (store.Issue, error)
	AddLabel(ctx context.Context, actor store.Actor, id store.IssueID, label, idem string) error
	RemoveLabel(ctx context.Context, actor store.Actor, id store.IssueID, label, idem string) error
	AddComment(ctx context.Context, actor store.Actor, id store.IssueID, body, idem string) (store.Comment, error)
	CommentsPage(ctx context.Context, id store.IssueID, before store.Cursor, limit int) (store.CommentPage, error)
	HistoryPage(ctx context.Context, id store.IssueID, before store.Cursor, limit int) (store.EventPage, error)
	AcceptanceItems(ctx context.Context, id store.IssueID) ([]store.AcceptanceItem, error)
	Accept(ctx context.Context, actor store.Actor, id store.IssueID, a store.Acceptance) ([]store.AcceptanceItem, error)
	SimilarClosed(ctx context.Context, title string, exclude store.IssueID, limit int) ([]store.SimilarIssue, error)
}

// Graph is the dependencies between issues, and what is read across
// issues: lists, the ready and blocked queues, the digest and dates.
type Graph interface {
	AddDep(ctx context.Context, actor store.Actor, from, to store.IssueID, typ store.DepType, idem string) error
	RemoveDep(ctx context.Context, actor store.Actor, from, to store.IssueID, typ store.DepType) error
	List(ctx context.Context, f store.Filter) (store.IssuePage, error)
	ListSummaries(ctx context.Context, f store.Filter) (store.SummaryPage, error)
	Ready(ctx context.Context, actor store.Actor, limit int) ([]store.ReadyIssue, error)
	Blocked(ctx context.Context, limit int) ([]store.BlockedIssue, error)
	Digest(ctx context.Context, f store.DigestFilter) (store.Digest, error)
	Dates(ctx context.Context, f store.DatesFilter) (store.Dates, error)
}

// Work is claims and handoffs, who is at work, and each principal's
// inbox and the events pushed to a watching session.
type Work interface {
	StartIssue(ctx context.Context, actor store.Actor, id store.IssueID, lease time.Duration, take bool) (store.Issue, store.Claim, error)
	FinishIssue(ctx context.Context, actor store.Actor, id store.IssueID, epoch int64, f store.Finish) (store.Issue, []store.IssueID, error)
	HandoffIssue(ctx context.Context, actor store.Actor, id store.IssueID, epoch int64, h store.HandoffNote, release bool, idem string, paths []string) (store.Issue, error)
	ReplayFinish(ctx context.Context, actor store.Actor, id store.IssueID, epoch int64, f store.Finish) (store.Finished, error)
	ReplayHandoff(ctx context.Context, actor store.Actor, id store.IssueID, epoch int64, h store.HandoffNote, release bool, idem string, paths []string) (store.HandedOff, error)
	LastHandoff(ctx context.Context, id store.IssueID) (*store.Handoff, error)
	RenewClaims(ctx context.Context, actor store.Actor, lease time.Duration, allSessions bool, paths map[store.IssueID][]string) ([]store.Claim, error)
	ActiveClaims(ctx context.Context, limit int) ([]store.Claim, int, error)
	AwayMax() time.Duration
	TouchAgent(ctx context.Context, actor store.Actor, harness string) error
	WhoPage(ctx context.Context, since time.Duration, limit int) ([]store.Agent, int, error)
	SessionsSeen(ctx context.Context, since time.Duration) (int, error)
	Inbox(ctx context.Context, actor store.Actor, all bool, limit int) (store.InboxPage, error)
	AckInbox(ctx context.Context, actor store.Actor, ids []int64, all bool) (int, error)
	Watch(principal, session string) *store.Watch
	WatchEvents(principal, session string) *store.Watch
}

// Memory is what principals remember for each other.
type Memory interface {
	Remember(ctx context.Context, actor store.Actor, in store.NewMemory) (store.Memory, error)
	Recall(ctx context.Context, actor store.Actor, q store.MemoryQuery) ([]store.Memory, int, error)
	Forget(ctx context.Context, actor store.Actor, scope store.Scope, key string, rev store.Rev, idem string) (store.Memory, error)
	PinMemory(ctx context.Context, actor store.Actor, scope store.Scope, key string, pinned bool) (store.Memory, error)
	PrimeMemories(ctx context.Context, actor store.Actor, limit int) ([]store.Memory, error)
	IssueMemories(ctx context.Context, actor store.Actor, id store.IssueID, limit int) ([]store.Memory, error)
}

// Cost is token usage and its prices and plans, and the hours people
// log.
type Cost interface {
	AddUsage(ctx context.Context, actor store.Actor, recs []store.UsageRecord, idem string) (store.UsageAdded, error)
	CostReport(ctx context.Context, f store.CostFilter) (store.CostReport, error)
	SetPrice(ctx context.Context, actor store.Actor, model string, from time.Time, r store.Rates) (store.PriceChange, error)
	Prices(ctx context.Context) ([]store.Price, error)
	SetPlan(ctx context.Context, actor store.Actor, p store.NewPlan) (store.PlanChange, error)
	Plans(ctx context.Context) ([]store.Plan, error)
	LogHours(ctx context.Context, actor store.Actor, in store.NewHours) (store.HoursEntry, error)
	Hours(ctx context.Context, f store.HoursFilter) (store.HoursList, error)
	DeleteHours(ctx context.Context, actor store.Actor, id, idem string) (store.HoursEntry, error)
}

// Gates is gates and the checks that pass or fail them, and locks.
type Gates interface {
	AddGate(ctx context.Context, actor store.Actor, in store.NewGate) (store.Gate, error)
	GateOf(ctx context.Context, id store.IssueID) (*store.Gate, error)
	Gates(ctx context.Context, f store.GateFilter) ([]store.Gate, error)
	GatesOf(ctx context.Context, ids []store.IssueID) ([]store.Gate, error)
	GatesHolding(ctx context.Context, id store.IssueID) ([]store.Gate, error)
	PassGate(ctx context.Context, actor store.Actor, id store.IssueID, reason string) (store.Gate, error)
	WaiveGate(ctx context.Context, actor store.Actor, id store.IssueID, reason string) (store.Gate, error)
	RearmGate(ctx context.Context, actor store.Actor, id store.IssueID, reason string) (store.Gate, error)
	PassCheck(ctx context.Context, actor store.Actor, r store.Report) (store.Gate, error)
	FailCheck(ctx context.Context, actor store.Actor, r store.Report) (store.Gate, error)
	ChecksOf(ctx context.Context, id store.IssueID) ([]store.Gate, error)
	AcquireLock(ctx context.Context, actor store.Actor, in store.LockAcquire) (store.LockState, error)
	RenewLock(ctx context.Context, actor store.Actor, name string, epoch int64, lease time.Duration) (store.LockState, error)
	ReleaseLock(ctx context.Context, actor store.Actor, name string, epoch int64) error
	LeaveLock(ctx context.Context, actor store.Actor, name string) (store.LockLeft, error)
	Locks(ctx context.Context, name string) ([]store.Lock, error)
}

// Conflicts is the replayed update of an issue, and the conflicts it
// parks for a person to resolve.
type Conflicts interface {
	MergeIssue(ctx context.Context, actor store.Actor, id store.IssueID, base store.Rev, patch store.IssuePatch) (store.Merged, error)
	Conflicts(ctx context.Context, f store.ConflictFilter) ([]store.Conflict, bool, error)
	GetConflict(ctx context.Context, id int64) (store.Conflict, error)
	OpenConflicts(ctx context.Context, principal string) (int, error)
	ResolveConflict(ctx context.Context, actor store.Actor, id int64, choice store.Choice, text string) (store.Issue, error)
	ProposeResolution(ctx context.Context, actor store.Actor, id int64, choice store.Choice, text, why string) (store.Conflict, error)
}

// Upkeep is what the daemon does to a store beside its requests: the
// reaper's ends of lapsed claims and locks, the ends of those whose
// holders lost access, pruning, the health check and the figures of the
// status report, the clock, and closing it.
type Upkeep interface {
	ReapClaims(ctx context.Context) ([]store.Claim, error)
	EndClaims(ctx context.Context, actor store.Actor, why func(principal string) string) ([]store.Claim, error)
	ReapLocks(ctx context.Context) ([]store.LockEnd, error)
	EndLocks(ctx context.Context, actor store.Actor, why func(principal string) string) ([]store.LockEnd, error)
	Prune(ctx context.Context, agentKeep, inboxKeep time.Duration) (store.Pruned, error)
	Ping(ctx context.Context) error
	Writes() int
	WriteStats() store.WriteStats
	Migrated() int
	ClockState() store.ClockState
	Now() time.Time
	Prefix() string
	Close() error
}

// Control is the control store of a server of many projects, as the
// server uses it.
type Control interface {
	Index
	ProjectAdmin
	PrincipalAdmin
	ControlUpkeep
}

// Index is what the control store answers from memory: who may reach
// which project, read at each handshake, and the keys sshd is given.
type Index interface {
	Access(principal, project string) store.Access
	Standing(principal string) (known, disabled bool)
	IsServerAdmin(principal string) bool
	Admins() []string
	AdministersProjectOf(actor, principal string) bool
	ReachGained(principal, left string) []string
	AuthorizedKey(fingerprint string) (principal, key string, ok bool)
	AuthorizedKeys() []store.PrincipalKey
	KeysElsewhere(owner func(fingerprint string) (principal string, ok bool))
	ProjectNamed(name string) (store.Project, bool)
	KnownProjects() []store.Project
	ActiveProjects() int
	Counts() store.ControlCounts
}

// ProjectAdmin is the projects of `aries admin project`, and their
// databases.
type ProjectAdmin interface {
	AddProject(ctx context.Context, actor store.Actor, in store.NewProject) (store.Project, error)
	CreateDatabase(ctx context.Context, actor store.Actor, project string) error
	ActivateProject(ctx context.Context, actor store.Actor, id string) (store.Project, error)
	ArchiveProject(ctx context.Context, actor store.Actor, id string) (store.Project, error)
	Project(ctx context.Context, id string) (store.Project, error)
	Projects(ctx context.Context) ([]store.Project, error)
	HeldClaims(ctx context.Context, db string) (int, error)
	Account(ctx context.Context) (string, error)
}

// PrincipalAdmin is the principals, keys and members of `aries admin`.
type PrincipalAdmin interface {
	AddPrincipal(ctx context.Context, actor store.Actor, name, project string, keys ...string) (store.Principal, error)
	DisablePrincipal(ctx context.Context, actor store.Actor, name string) (store.Principal, error)
	EnablePrincipal(ctx context.Context, actor store.Actor, name string) (store.Principal, error)
	Principal(ctx context.Context, name string) (store.Principal, error)
	Principals(ctx context.Context) ([]store.Principal, error)
	PrincipalsCreatedBy(ctx context.Context, creator string) ([]store.Principal, error)
	AddKey(ctx context.Context, actor store.Actor, principal, line string) (store.PrincipalKey, error)
	RemoveKey(ctx context.Context, actor store.Actor, fingerprint string) (store.PrincipalKey, error)
	Keys(ctx context.Context, principal string) ([]store.PrincipalKey, error)
	KeysAddedBy(ctx context.Context, adder string) ([]store.PrincipalKey, error)
	SetMember(ctx context.Context, actor store.Actor, principal, project string, role store.Role) (store.Member, error)
	ProposeMember(ctx context.Context, actor store.Actor, principal, project string, role store.Role) (store.Member, error)
	RemoveMember(ctx context.Context, actor store.Actor, principal, project string) (store.Member, error)
	Members(ctx context.Context, project string) ([]store.Member, error)
	ProjectsOf(ctx context.Context, principal string) ([]store.Member, error)
	MembersAddedBy(ctx context.Context, adder string) ([]store.Member, error)
	Role(ctx context.Context, principal, project string) (role store.Role, ok bool, err error)
}

// ControlUpkeep is what the daemon does to the control store beside its
// requests: rereading it, checking that its database answers, reading
// its event log for the audit stream, and the clock. The caller that
// opened it closes it.
type ControlUpkeep interface {
	Reload(ctx context.Context) error
	Ping(ctx context.Context) error
	Events(ctx context.Context, after int64, limit int) ([]store.Event, error)
	LastEventSeq(ctx context.Context) (int64, error)
	ClockState() store.ClockState
	SettleClock(ctx context.Context) error
}

var (
	_ Store   = (*store.Store)(nil)
	_ Control = (*store.Control)(nil)
)
