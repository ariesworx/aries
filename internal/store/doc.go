// Package store is ariesd's issue store: a typed API over one schema of
// a Postgres database, and the only code that writes to it.
//
// [Open] checks the server's version and the database role, applies the
// embedded migrations (versioned in schema_migrations) and returns a
// [Store], which is safe
// for concurrent use. Every write names its [Actor]: the principal,
// session and machine that made it. The store holds issues ([Issue],
// keyed by [IssueID]) with their dependencies ([Dep]), labels and
// comments ([Comment]); claims, which lease an issue to one session
// ([Claim]); the agents registry ([Agent]); inbox items ([InboxItem]),
// pushed to each [Watch] as they commit, with every issue event for a
// watch that asks; handoff notes ([Handoff]);
// acceptance checklists ([AcceptanceItem]); scoped memories ([Memory]),
// with no text that looks like a credential (internal/secretscan); token
// usage records ([UsageRecord]), model prices ([Price]), people's logged
// hours ([HoursEntry]) and subscription plans ([Plan]); and the event log
// ([Event]).
// Ready, Blocked, Digest, acceptance items, which issue a usage record
// belongs to and what it cost, at list price and amortized over plans
// ([Store.CostReport]), are computed at read time, with recursive CTEs where they follow the graph; nothing derived
// is stored.
//
// [OpenControl] returns a [Control], the store of what belongs to the
// server rather than to one project, in a schema of its own: projects
// ([Project]), principals ([Principal]), their public SSH keys
// ([PrincipalKey]), memberships with roles ([Member]), and the
// identities people sign in with and invitations to them ([Identity]).
// It is built on a Store, so everything below about errors and
// concurrency holds for it; its own migrations are in controlmigrations,
// and design §15 and docs/design/identity.md §3 describe it.
//
// The design is docs/design/aries.md: §3 for the data model, §6 for
// memory, §7 for claims, the registry, inboxes, handoffs, idempotency and authorization,
// §8 for how concurrent changes resolve, and §12.1 for tokens and cost;
// decision 79 (§14) says why the database is Postgres.
//
// # Errors
//
// A refusal wraps one of the sentinel errors, such as [ErrNotFound],
// [ErrConflict] or [ErrInvalid]; test for them with [errors.Is]. An error
// that wraps none is a failure, such as a lost database connection or a
// canceled context. Refusals that carry detail are typed and wrap a
// sentinel: [*HeldError], [*IdemError], [*StaleEpochError] and
// [*MemoryConflictError] wrap ErrConflict, [*ForbiddenError] wraps
// [ErrForbidden], and [*AcceptanceError], [*SecretError] and
// [*MemoryLimitError] wrap ErrInvalid; read their fields with
// [errors.As]. The control store adds [*AccessError], which wraps
// ErrForbidden, [*TakenError] and [*InvitedError], which wrap
// ErrConflict, and [*ControlLimitError] and [*IdentityError], which wrap
// ErrInvalid. [Open] refuses an unsafe database role with an
// [*UnsafeAccountError], and a server older than Postgres 16 with a
// [*ServerVersionError].
//
// # Authorization
//
// While a live claim holds an issue, only the holder's principal and the
// admins ([Options.Admins], and whoever [Options.IsAdmin] says is one
// when asked) may update, close, reopen, finish, hand off or accept it. Anyone else is refused with a [*ForbiddenError]
// before anything is written. Comments, labels and dependencies stay open
// to everyone. An admin's change to an issue another principal holds is
// recorded as an [OpAdminOverride] event ahead of the change's own events.
//
// The control store authorizes each write inside its transaction: a
// server admin ([ControlOptions.Admins]) may make every change, a
// project admin some, and only over principals whose every project they
// administer ([Control.AddKey]). Its reads are not guarded. The daemon
// asks [Control.Access], which reads memory, whom to let into a project
// and who its admins are.
//
// # Concurrency
//
// Writes run on one connection, so within a process they are serialized
// and the event sequence is gapless without contention. Reads use a
// separate pool and see committed transactions.
//
// Every write runs in one SQL transaction, and each change in it that is
// history appends an event in the same transaction (principal, session,
// machine, op, before and after). Bookkeeping that is not history records
// none: lease renewals, registry touches, inbox acks and [Store.Prune].
//
// Transactions run at repeatable read, so each sees one snapshot. An
// UPDATE of a mutable row names the rev it read (WHERE rev = $n) and
// raises it: Postgres locks the row for the first writer, and a second
// writer of the same row waits for that lock and then fails with a
// serialization failure (40001), so the update is an exact
// compare-and-swap. Two writers that insert the same key, such as the
// event log's next seq or an issue's first claim row, collide on the
// unique index, and the second fails with a unique violation (23505)
// once the first commits. A write that loses either way, or to a
// deadlock (40P01), is rerun from the start, up to [Options.MaxAttempts],
// and rereads what it read, so a stale expected rev surfaces as
// [ErrConflict] and a key taken meanwhile as the refusal it means. This
// covers a second process on the same database, such as an admin tool.
//
// A migration runs in one transaction with the row that records it, as
// Postgres's DDL is transactional: one that fails leaves the schema as it
// was. What is committed is the only history there is beside the event
// log; the operator's backups keep earlier states.
package store
