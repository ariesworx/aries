# Review: Aries's domains, a context map and a glossary

Review, 23 October 2026, for issue aries-q76xfbku. The maintainer asked
whether domain-driven design would make Aries easier to reason about and
understand. This is a review, not a rewrite: it names the bounded
contexts as the code has them today, maps how they depend on each
other, gives a glossary of the language with every term that means two
things, lists where the boundaries leak, and ranks what is worth doing.
It is written against `main` at 8bba8f14; a line number is that
commit's. It changes no code. **Decided, 23 October 2026:** the
maintainer accepted the recommendation on all ten questions, with the
glossary's test from the start and `area:` labels only after the
glossary (§8).

It follows [cli-review.md](cli-review.md) and
[factory-review.md](factory-review.md) in form: findings with their
citations (§2 to §5), then what DDD does and does not fit here (§6),
ranked recommendations (§7) and questions for the maintainer, each with
this review's recommendation and the maintainer's decision (§8).

**The short answer.** DDD's strategic half pays for itself here: names
for the contexts, a map of how they talk, and one meaning per word. The
code already has most of the shape (the control store, the agent
factory's packages behind narrow interfaces, `proto` as a published
language), and what it lacks is the naming and a handful of rules held
twice. DDD's tactical half (aggregates as objects, repositories, an
in-process event bus, a package per context) does not fit a store whose
model is SQL run by one writer in one transaction per operation, with
the event log as the truth; adopting it would add layers without
removing a bug. §7 recommends eight changes, the first four small, and
recommends against a domain-modeler agent.

## 1. What was read

- [README.md](../../README.md), [docs/design/aries.md](aries.md) §1
  (principles), §2 (architecture), §3 (data model) and §14
  (decisions), [AGENTS.md](../../AGENTS.md)'s layout and rules,
  [docs/concepts.md](../concepts.md), [docs/agents.md](../agents.md),
  and the package doc comments of every package under `internal/`.
- Sampled where a boundary looked blurred, not read whole:
  `internal/store` (71 files, 26,066 lines, every file's context
  classified), `internal/server` (every file classified, the router,
  `stores.go`, `scopes.go`, `limits.go`, `settings.go`), `internal/proto`
  (op constants by file), `internal/cli` and `internal/mcpserver`
  (rules held on both sides), `internal/outbox` and `internal/offline`
  (which ops queue and which reads are cached), and the agent factory's
  packages' imports.
- Counts of methods, ops and event kinds were taken by reading the
  declarations, not by a tool; they are within one or two.

## 2. The bounded contexts as they are

A context here is a part of the system with its own words, its own
rules and one writer of its state. Nine are visible. Five are the
domain (what Aries is for); four are generic (what any server of this
kind needs). The ops column is from `internal/proto`'s constants (86 ops
in all: the router's `handlers`, `internal/server/dispatch.go:53-110`,
56 ops, and `adminHandlers`, `internal/server/admin.go:39-70`, 30).

### 2.1 Work tracking (core)

| | |
|---|---|
| **Holds** | Issues, dependencies, labels, comments, claims, handoffs, acceptance items, the project's workflow, gates (`human`, `ci`) and their checks, `landed`, review |
| **Packages** | `internal/store`: `issues.go`, `graph.go`, `notes.go`, `list.go`, `show.go`, `similar.go`, `handoffs.go`, `work.go`, `claims.go`, `acceptance.go`, `gates.go`, `ci_gates.go`, `review.go`, `workflow.go`, `landed.go`, `paths.go`, `import.go`; `internal/server/dispatch.go` (handlers inline), `gates.go`, `workflow.go`; `internal/cli` `commands.go`, `work.go`, `gate.go`, `land.go`, `workflow.go`; `internal/mcpserver` `tools.go`, `closeout.go`, `gate.go`; `internal/gitx` (branch and trailer); `internal/bdimport` |
| **Aggregates** | The **issue**, with its claim, acceptance items and handoffs: one row with a `rev`, changed only by compare-and-swap (`casUpdate`, `internal/store/issues.go:667`). The **gate**, an issue of type `gate` with a side table (decision 66). The **workflow**, versioned by its `workflow.set` events (`internal/store/workflow.go:33`) |
| **Invariants** | Only the claim's holder or an admin changes a held issue (`wtx.guard`, `internal/store/authz.go:66`). A claim is fenced by an epoch that only rises (`internal/store/claims.go:44`). An issue closes only with every item ticked or waived, no holding gate, every required check passed and review given (`internal/store/issues.go:889-904`). A gate is passed by a person from the session `cli` or by CI from `ci` (`internal/store/gates.go:373-389`). Ready and blocked are derived, never stored (`internal/store/graph.go:342`) |
| **Who writes** | Agents and people through `start`, `update`, `handoff`, `finish`, `close`; CI and the pull job through `gate pass --check` (the closing pass, `internal/store/landed.go:283`); the operator through `ariesd import-bd` |
| **Ops** | 31 (`internal/proto/ops.go:14-37` in part, `gates.go:10-19`, `workflow.go:20-21`) |

### 2.2 Coordination (supporting)

| | |
|---|---|
| **Holds** | The agents registry (who is at work), inboxes, the live watch, `who`, `digest`, locks, announcements |
| **Packages** | `internal/store` `agents.go`, `inbox.go`, `digest.go`, `dates.go`, `locks.go`, `announce.go`, `pages.go` (in part); `internal/server` `agents.go`, `inbox.go`, `digest.go`, `locks.go`, `announce.go`; `internal/board` (`aries tui`); `internal/cli` `lock.go`, `inbox.go`, `who.go`, `digest.go` |
| **Aggregates** | The **lock** (a mutex or semaphore with a first-in-first-out queue and waiter leases, `internal/store/locks.go:1-30`); the **inbox item** (append, then acknowledged) |
| **Invariants** | A lock grants at most its capacity, in order, and its epoch rises per grant. Every inbox item is written in the same transaction as the change it announces, by one helper (`wtx.notify`, `internal/store/inbox.go:105`). Registry touches and acks are bookkeeping, not history (`wtx.quiet`, rule 7) |
| **Who writes** | Every other context writes the inbox (§3); locks are taken by people, scripts, CI and the dispatcher, never by an agent through MCP (decision 70) |
| **Ops** | 10 (inbox, ack, watch, digest, who, and the five lock ops, `internal/proto/locks.go:10-14`) |

### 2.3 Memory (supporting)

| | |
|---|---|
| **Holds** | Memories with a key, a body, tags, a scope (`project`, `team`, `user`), pins and an optional issue link |
| **Packages** | `internal/store/memory.go` (1,092 lines); `internal/server/memory.go`; `internal/cli/memory.go`; `internal/mcpserver/memory.go`; `internal/secretscan` |
| **Aggregate** | The **memory**, keyed by (scope, key), with a `rev` |
| **Invariants** | No text that looks like a credential (`internal/store/memory.go:294`); caps per scope, with the oldest forgotten ones deleted to make room through `wtx.touch` (rule 7) |
| **Who writes** | Agents and people |
| **Ops** | 4 (`internal/proto/memory.go:10-13`) |

### 2.4 Identity and access (generic, in its own store)

| | |
|---|---|
| **Holds** | Projects, principals (`person`, `service`), public keys, memberships with roles, identities and invitations, sign-ins (grants), trust rules for workload tokens |
| **Packages** | `internal/store/control*.go` (the control store, a schema of its own); `internal/server` `admin.go`, `keys.go`, `expiry.go`, `conns.go`, `identities.go`, `signin.go`, `signinpending.go`, `exchange.go`, `trust.go`, `scopes.go`, `project.go`; `internal/oidc`, `internal/jose`, `internal/dpop`, `internal/keyproof`; `internal/cli` `admin*.go`, `login.go`, `whoami.go` |
| **Aggregates** | The **principal** with its keys and memberships; the **project**; the **sign-in**; the **trust rule** |
| **Invariants** | Disabled wins (decision 20). Nobody adds a key to their own account (decisions 19, 38). Losing access ends claims at once (decision 21). Each op has its token scopes (`internal/server/scopes.go:13-41`) |
| **Who writes** | Admins, from the CLI only (rule 12); `ariesd control init`; a sign-in at the provider |
| **Ops** | 29 (`internal/proto/admin.go:21-47`, `signin.go:25-26`, `trust.go:11-15`) |

This is the cleanest boundary in the code: a schema of its own, a type
of its own (`Control`, `internal/store/control.go:85`) and an interface
split by area on the server's side (`internal/server/stores.go:182-300`).

### 2.5 Offline and conflicts (supporting)

| | |
|---|---|
| **Holds** | The outbox of writes made away from the server, the cache of what a session read, replay, and conflicts parked on the server for a person |
| **Packages** | `internal/outbox`, `internal/offline`; `internal/store` `conflicts.go`, `late.go`, `idem.go`; `internal/server/conflicts.go`; `internal/cli` `outbox.go`, `cache.go`, `conflicts.go`; `internal/mcpserver` `offline.go`, `cache.go` |
| **Aggregates** | The **outbox entry** (an op with its idempotency key); the **conflict** (a replayed update's fields against the state at its rev) |
| **Invariants** | Only the ops in `queued` are queued (`internal/outbox/entry.go:40-53`), each with an idempotency key; no claim is made offline (decision 39); a conflict is decided by a person (decision 42) |
| **Who writes** | `aries` and `aries mcp` on each machine; the server parks conflicts |
| **Ops** | 3 (`internal/proto/conflicts.go:11-13`) |

### 2.6 Cost (supporting)

| | |
|---|---|
| **Holds** | Usage records, prices (with tiers), subscription plans, logged hours, the attribution of usage to issues, cost by outcome |
| **Packages** | `internal/store` `usage.go`, `cost.go`, `price.go`, `plan.go`, `amortize.go`, `attribution.go` (1,212 lines, the store's largest), `cost_outcome.go`, `hours.go`, `issue_account.go`; `internal/server` `cost.go`, `usage.go`, `hours.go`; `internal/capture`; `internal/cli` `cost.go`, `hours.go`, `usage.go`, `capture.go`; `internal/sextant` |
| **Aggregates** | The **usage record** (append-only), the **price**, the **plan** |
| **Invariants** | Which issue a usage record belongs to, and what it cost, is computed at read time from claim history (`internal/store/attribution.go:1-55`); prices and plans are set by admins only |
| **Who writes** | Harness hooks (`aries usage --hook`), people (`hours`), admins (prices, plans); Sextant only reads |
| **Ops** | 9 (`internal/proto/ops.go`, `cost.go:19-21`, `hours.go:18-22`) |

### 2.7 The agent factory (core, client side)

| | |
|---|---|
| **Holds** | The dispatcher, briefs and their template, the guard, the merge rule, outcome records, landing from the main checkout, the pull job, harness setup |
| **Packages** | `internal/dispatch`, `internal/brief`, `internal/guard`, `internal/merges`, `internal/outcome`, `internal/gitx`, `internal/credential`, `internal/agentsetup`, `internal/agenttel`; `internal/cli` `dispatch.go`, `guard.go`, `prs.go`, `outcome.go`, `land.go`, `pulljob.go`, `setup*.go` |
| **Aggregates** | None of its own on the server. Its records are text on issues (`brief:`, `outcome:`, `lesson:` comments) and files in the repository (`.aries/brief.txt`, `.aries/guard.yaml`, `.aries/merges.yaml`) |
| **Invariants** | It reaches the server only through ops: the dispatcher by running `aries --json --no-queue` (`internal/dispatch/exec.go:50-51`), `outcome` and `merges` through a `Store` interface the CLI fills (`internal/cli/outcome.go:208-286`, `internal/cli/prs.go:88`). None of its packages imports `internal/store` or `internal/server` |
| **Who writes** | The coordinator, the dispatcher, workers' sessions, the maintainer's pull job |
| **Ops** | None of its own: it is a client of work tracking, coordination and cost |

### 2.8 Transport and protocol (generic)

`internal/proto` (the frames, ops, `*Args` and `*Result`, typed errors),
`internal/client`, `internal/wsconn`, and the server's `https.go`,
`bridge.go`, `tls.go`, `listen.go` and connection loop
(`internal/server/server.go`). Its invariants are rule 10's: an op or
argument field is a protocol change, and the server refuses unknown
fields. It is the open host service every other context is reached
through.

### 2.9 Operations and observability (generic)

`internal/telemetry`, `internal/logs`, `internal/sdnotify`,
`internal/statustext`, `internal/release`, `internal/version`, the
server's `status.go`, `audit.go`, `observe.go`, `settings.go`, and the
store's infrastructure (`store.go`, `db.go`, `fence.go`, `clock.go`,
`migrate.go`, `errors.go`). Its invariants are rules 2 and 19: no text
in telemetry, time from the store.

## 3. Context map

```text
                      ┌──────────────────────────┐
                      │ Agent factory (client)   │ dispatch, brief, guard,
                      │ no store, no server      │ merges, outcome, land
                      └────────────┬─────────────┘
                 aries subprocess, │ proto ops, text records
                 internal/client   │ (brief:, outcome:, lesson:, Aries:)
                                   ▼
┌───────────────┐  proto ops  ┌─────────────────────────────────────────┐
│ Offline       │────────────▶│ Transport and protocol (open host)      │
│ outbox, cache │  replay     │ proto = published language              │
└──────┬────────┘             └───────┬───────────────────────┬─────────┘
       │ conflicts parked             │ project store         │ control store
       ▼                              ▼                       ▼
┌──────────────────────────────────────────────┐   ┌──────────────────────┐
│ one project schema, one writer, one tx per op│   │ Identity and access  │
│  Work tracking ──notify──▶ Coordination      │◀──│ (control schema)     │
│      ▲    │ claims, events                   │   │ IsAdmin, EndClaims,  │
│      │    ▼                                  │   │ HeldClaims (SQL)     │
│  Memory    Cost (reads claims, issues,       │   └──────────────────────┘
│  (issue    gates, events at read time)       │
│   link)                                      │◀── Sextant: ops prices, cost
└──────────────────────────────────────────────┘
```

| From | To | Through | Relationship |
|---|---|---|---|
| Every client context | Work, memory, cost, coordination | `proto` ops over one connection | Open host service with a published language (`internal/proto`) |
| Work tracking | Coordination | `wtx.notify` (`internal/store/inbox.go:105`) from 20 call sites in 12 files; the live watch (`internal/store/store.go:903-908`) | Same transaction; Work is upstream |
| Cost | Work tracking | Direct SQL over `claims` (`internal/store/attribution.go:262`), claim events (`:230`, `:377`, `:417`), `issues`, `acceptance_state` and `gates` (`internal/store/cost_outcome.go:90`, `:257-258`) | Conformist read model: cost reads work's tables as they are |
| Identity | Work tracking | `Options.IsAdmin` answered from the control store (`internal/server/project.go:95-97`); `EndClaims` when access ends; `HeldClaims` reading every project's `claims`, `lock_holders` and `lock_waiters` in one query (`internal/store/control_admin.go:82-90`) | Customer and supplier, with one shared-SQL read across schemas |
| Identity | Coordination | `AnnounceSignIn` (`internal/store/announce.go:77`) | Notification |
| Offline | Work tracking | Replay of queued ops; conflicts park through the issue's `applyPatch` and `guard` (`internal/store/conflicts.go:417`, `:472`) | Offline is downstream of the protocol; on the server it shares work's transaction |
| Memory | Work tracking | An optional issue link (`internal/store/memory.go:473`) | Reference only |
| Agent factory | Work, coordination, cost | `aries` as a subprocess (`internal/dispatch/exec.go:50-51`, `:168`); `outcome.Store` and `merges.Store` filled by the CLI over `internal/client` | Anti-corruption layer: the factory's interfaces name only what it reads |
| CI and the pull job | Work tracking | `gate.pass` and `gate.fail` from the session `ci` or `cli` (`internal/store/landed.go:283`, called from `ci_gates.go:383`) | Customer of work's rules |
| Sextant | Cost | The ops `prices` and `cost`, as `aries` sends them (`internal/sextant`) | Downstream conformist, read only |
| MCP | Every context agents use | `internal/mcpserver/errors.go` rewords each refusal as the next tool call | Anti-corruption layer for agents |

Two things the map shows. First, the domain contexts other than
identity share one schema, one writer and one transaction per op: a
`finish` touches the issue, its claim, its items, its handoff, the
inbox and the event log at once. That is deliberate (rules 5 to 7) and
is why the store is one package (§6). Second, the event log is the
integration point in time rather than in code: cost, digest, conflicts,
landing and the outcome record all read events written by work
tracking (§5, L6), and nothing lists those events in one place.

## 4. Glossary

One definition each, as the code uses the word. Where the code and a
guide disagree, the code's meaning is given and the guide is listed in
§5.

| Term | Definition |
|---|---|
| **Acceptance item** | A line of an issue's checklist, ticked or waived with a reason; `close` and `finish` refuse while one is open (`internal/store/acceptance.go:33-40`) |
| **Actor** | The (principal, session, machine) a write names (`internal/store/doc.go`) |
| **Agent** | A session in the registry (`internal/store/agents.go:14`); see §4.1 |
| **Attribution** | The read-time rule that gives a usage record to the issue its session held (`internal/store/attribution.go:1-55`) |
| **Brief** | The text a worker's session is started with, rendered from `.aries/brief.txt`, and recorded as a comment whose first line is `brief:` (`internal/brief`) |
| **Check** | A named requirement a `ci` gate stands for, such as `landed`, passed by a report (`internal/proto/gates.go:22-26`) |
| **Claim** | An issue's current lease to one holder, under an epoch (`internal/store/claims.go:44`) |
| **Close** | Ending an issue; refused while an item, gate, check or review waits |
| **Conflict** | A replayed offline update whose fields met another's, parked for a person (`internal/proto/conflicts.go:1-5`) |
| **Coordinator** | The session or person that starts others' sessions and queues merges (memory `dispatch`) |
| **Dispatcher** | The client-side loop that keeps headless runs busy (`internal/dispatch`, decision 96) |
| **Epoch** | A counter that rises with each new holder of a claim or a lock |
| **Event** | A row of a project's (or the control store's) append-only log, written in the same transaction as its change (rule 7) |
| **Finish** | `close` plus `handoff` plus ticks and waivers plus discovered work, in one write |
| **Gate** | An issue of type `gate`, kind `human` or `ci`, that holds the issues it blocks (decision 66) |
| **Guard** | The PreToolUse hook that checks a harness's commands against `.aries/guard.yaml` (`internal/guard`) |
| **Handoff** | A note with a state (`done`, `partial`, `blocked`) left on an issue for whoever continues (`internal/store/handoffs.go:19-26`) |
| **Inbox item** | A notice addressed to a principal: a claim lost, a handoff, a mention, an assignment, a conflict, a gate or lock change, a sign-in |
| **Landed** | The check a merged commit's issue passes when `aries land` or CI reports it; and the close reason the closing pass then records (`internal/store/landed.go:47`) |
| **Lesson** | A note about how the factory works, recorded as a `lesson:` comment at handoff (memory `lessons`; decision 101's field is not built) |
| **Lock** | A named mutex or semaphore with a first-in-first-out queue, per project |
| **Memory** | A keyed note that outlasts sessions, in a scope |
| **Merge rule** | The classifier that says which pull requests may be queued without a person (`internal/merges`) |
| **Outbox** | The queue of writes made while the server could not be reached |
| **Outcome record** | What an issue's run led to, computed by rule and posted as an `outcome:` comment (`internal/outcome`) |
| **Plan** | A subscription plan over which usage is amortized (`internal/store/plan.go:1-4`) |
| **Price** | A model's rates, by prompt-length tier (`internal/store/price.go`) |
| **Principal** | Who a key or sign-in authenticates: a `person` or a `service` (`internal/store/control_principals.go:36-42`) |
| **Project** | A tenant of a server: a schema of its own, recorded in the control store (`internal/store/control_projects.go:14-19`) |
| **Ready** | Open, with no open blocker or open child; derived at read time (`internal/store/graph.go:342`) |
| **Review** | The workflow's review gate on an issue (`internal/proto/workflow.go:51`) |
| **Scope** | Who a memory reaches: `project`, `team` or `user` (`internal/store/memory.go:30-40`) |
| **Session** | One run of an agent, or a person's terminal (`cli`) or CI (`ci`), named by `ARIES_SESSION` or the harness's id (`internal/client/session.go:17`) |
| **Sign-in** | A grant from an identity provider, bound to a key, with an end (`internal/store/control_signin.go:16-21`) |
| **Trust rule** | The rule by which a service principal signs in with its platform's token (`internal/store/control_trust.go:1`) |
| **Usage record** | Tokens a session spent, with model and time (`internal/store/usage.go`) |
| **Workflow** | A project's flow: default items by type, required checks and review, applied from `.aries/workflow.yaml` (`internal/proto/workflow.go:13-21`) |

### 4.1 Words with more than one meaning

Each of these sends a person or an agent who searches for the word to
the wrong code at least some of the time.

| Word | Meanings | Citations |
|---|---|---|
| **session** | (a) an Aries session; (b) the reserved sessions `cli` and `ci`; (c) Claude Code's session id, reused as (a); (d) Claude Desktop's `local_…` id, which no claim carries; (e) an SSH or TLS session | `internal/client/session.go:17`; `internal/proto/ops.go:587-595`; `docs/agents.md:683-686`; `docs/design/dispatcher.md:254`, `internal/cli/dispatch.go:474`; `docs/design/transport.md:764` |
| **key** | An SSH public key; a memory key; an idempotency key; the dispatcher's API key; a key file; the sshd keys file | `docs/concepts.md:11-16`, `:569`; `internal/proto/idem.go:8-11`; `internal/dispatch/dispatcher.go:413`; `docs/cli.md:43`; `docs/server.md:1359` |
| **scope** | A memory's reach; a token's scope (`cli`, `mcp`, `ci`) checked per op; a lesson's `factory` or `project` | `internal/store/memory.go:30-40`; `internal/proto/signin.go:5-19`, `internal/server/scopes.go:13-41`; `docs/design/lessons.md:137` |
| **project** | A server's tenant (a schema); the memory scope `project`; the repository that holds `.aries/` | `internal/store/control_projects.go:14-19`; `internal/store/memory.go:36-38`; `docs/concepts.md:392-394` |
| **identity** | An account at an identity provider; an agent's own principal ("agent identity"); SSH's default identities | `internal/store/control_identities.go:64`; `docs/concepts.md:10`, `docs/agents.md:720`; `docs/cli.md:221` |
| **agent** | A registry row (a session); a harness (`aries setup AGENT`); a subagent spec (`go-engineer`); the outcome record's kind, derived from a session that is neither `cli` nor `ci` | `internal/store/agents.go:14`, `:39`; `AGENTS.md:489-492`; `internal/outcome/capture.go:222-230` |
| **hold** | A lock grant; a session holding a claim; a gate holding issues; the merge rule holding a pull request | `docs/concepts.md:232-233`; `internal/store/agents.go:29`; `internal/store/gates.go:82`; `docs/cli.md:605-606` |
| **gate** | A gate issue; the contributor gate script; a stage's exit criterion | `internal/proto/gates.go:22-26`; `scripts/gate.sh:2`; `docs/design/aries.md:573` |
| **check** | A ci gate's check; `aries workflow check`; `setup --check`; the upgrade's health check | `docs/concepts.md:152-156`, `:437`, `:38`; `docs/agents.md:111` |
| **workflow** | The project's workflow setting; `aries workflow`; the memory `workflow`; a GitHub Actions workflow; "an issue's workflow state" (its status) | `internal/proto/workflow.go:13-21`; `internal/cli/cli.go:160`; `docs/design/factory-review.md:50`; `docs/ci.md:80`; `internal/store/types.go:32` |
| **dispatch** | The server's routing of an op to its handler; the client-side dispatcher; the memory `dispatch`; the lock `dispatch/runs` | `internal/server/dispatch.go:28`, `:40`; `internal/dispatch/doc.go:1-4` (the dispatcher), `:9` (the lock) |
| **status** | An issue's status; the server's status report; `aries dispatch status` | `internal/store/types.go:32-41`; `internal/server/status.go:16`; `internal/cli/cli.go:163` |
| **blocked** | The status `blocked`, a note anyone may change; derived blocked (an open blocker or child); the handoff state `blocked` | `docs/concepts.md:97`; `internal/store/graph.go:410-414`; `internal/store/handoffs.go:26` |
| **plan** | A subscription plan; the stage plan; a plan an agent submits to a gate | `internal/store/plan.go:1-4`; `docs/design/aries.md:571`; `docs/concepts.md:109` |
| **tier** | A price's prompt-length tier; the test tiers | `internal/proto/cost.go:49-55`; `AGENTS.md:155` |
| **conflict** | A parked offline conflict; the error code `conflict` for a stale rev or a held claim; a git merge conflict | `internal/proto/conflicts.go:1-5`; `internal/proto/errors.go:10-11`, `internal/store/errors.go:122`; `internal/outcome/record.go:113-116` |
| **outcome** | The outcome record's `landed`, `closed`, `pr-closed`; cost's groups `landed`, `closed`, `open`; a gate's `passed` or `waived` | `internal/outcome/record.go:33-37`; `internal/store/cost_outcome.go:22-31`; `internal/proto/gates.go:100` |
| **landed** | Cost: the last close gave the reason `landed`. The outcome record: that, or the `landed` check passed | `internal/store/cost_outcome.go:22-27`; `internal/outcome/capture.go:119` |
| **event** | The project's event log; the control store's; a log line's `event` name; Claude Code's own OpenTelemetry events | `internal/store/types.go:284`; `internal/server/audit.go:1-5`; `internal/logs/events.go:5-10`; `internal/agenttel` |
| **trust** | A trust rule; "untrusted" text in results; `proxy: trusted:` addresses | `internal/store/control_trust.go:1`; `internal/mcpserver/doc.go:32`; `docs/server.md:828` |
| **notice** | A rate-limited inbox item; a line in `prime`'s output | `docs/server.md:2959`; `docs/cli.md:50` |

### 4.2 Two words for one thing

| Words | Thing | Citations |
|---|---|---|
| **claim** and **lease** | An issue's current holding; "Claim is an issue's current lease" | `internal/store/claims.go:44`; lease is also used for locks (`internal/store/locks.go:1-30`) |
| **sign-in** and **grant** | One sign-in | `internal/store/control_signin.go:16-21` (`Grant`); the event `signin.grant` |
| **agent** and **session** | A row of the registry | `internal/store/agents.go:14` |
| **principal kind** and **session kind** | `person`, `service`, `agent`: the code derives the kind from the session, the design calls it the principal's | `internal/outcome/capture.go:222-230`; `docs/design/outcome-record.md:68` |
| **schema** and **database** | A project's storage, after decision 79: the docs mostly say schema, a few lines still say database | `docs/server.md:1525`; `docs/concepts.md:588` |
| **acceptance**, **criteria**, **items**, **checklist** | Acceptance items | `docs/design/aries.md:481`; `internal/store/acceptance.go:33` |

## 5. Where the boundaries leak

Each finding has a severity: **high** where a rule is held twice and can
drift, **medium** where a person or agent will look in the wrong place,
**low** where it is untidy and harmless.

### 5.1 In the code

**L1. `internal/store` spans seven contexts and does not say so
(medium).** 71 files and 26,066 lines; `Store` (`internal/store/store.go:111`)
has 122 exported methods, about 64 of them work tracking, 12 cost, 12
coordination, 8 memory, 8 offline and 18 infrastructure. The package
doc (`internal/store/doc.go:6-22`) lists what the store holds in one
sentence and names no areas; it omits gates, workflow, locks,
conflicts, paths and attribution. Several files mix contexts:
`dates.go` holds digest queries (`:96`, `:160`, `:208`), `attribution.go`
holds the digest's usage and hours (`:983`, `:1077`, `:1108`),
`clock.go` extends claim leases and locks (`:466-483`), `limits.go`
prunes the registry and inbox (`:259`), `observe.go` reads claims and
landings (`:303-411`), `stats.go` holds the control store's counts
(`:112`, `:148`). The package boundary is the transaction boundary,
which is right (§6); what is missing is the map inside it.

**L2. The rule for when an issue may close is held twice (high).**
`close` reads `holdingGates`, `acceptanceItems`, `checkWaits` and
`reviewWaits` (`internal/store/issues.go:889-904`); the closing pass
reads the same four in another order (`internal/store/landed.go:312-324`),
and `holdingGates` is called again from `graph.go:97`, `work.go:124` and
`issues.go:582`. This is the one invariant of the core that has two
copies: a new kind of wait (an approval, a second review) has to be
added to both, and a test of one does not cover the other.

**L3. Cost reads work tracking's tables and events directly, and
defines `landed` its own way (medium).** `attribution.go` reads `claims`
(`:262`) and claim events (`:230`, `:377`, `:417`); `cost_outcome.go`
reads `issues` (`:90`), close and reopen events (`:111-114`),
`acceptance_state` and `gates` (`:257-258`). Reading at read time is
principle 3 and is fine. The leak is that cost's `landed` (the last
close's reason, `internal/store/cost_outcome.go:22-27`) and the outcome
record's (that, or the `landed` check passed, `internal/outcome/capture.go:119`)
differ, so `aries cost --group-by outcome` and `aries outcome` can count
the same issue differently.

**L4. The event vocabulary has no home (medium).** The event log is the
truth (principle 3), but its 67 kinds are declared across 17 files: 10
core ops at `internal/store/types.go:116-127`, the rest beside their
writers (`acceptance.go:45-47`, `claims.go:64-67`, `gates.go:65-69`,
`locks.go:80-85`, `memory.go:143-149`, `conflicts.go:36-38`, the seven
`control_*.go` files, and others). Ten files read events of kinds
another context writes (`attribution.go`, `cost_outcome.go`,
`conflicts.go`, `landed.go`, `digest.go`, `late.go`, `workflow.go`,
`clock.go`, `pages.go`, `observe.go`). A reader who wants "what can
happen to an issue" has no list to read.

**L5. Inbox kinds are declared in four places, and the guides list a
subset (low to medium).** The kinds live in `internal/store/inbox.go:38-52`,
`gates.go:72-78`, `locks.go:88-91` and `announce.go:20`. The table in
`docs/concepts.md:529-536` and the MCP instructions
(`internal/mcpserver/mcpserver.go:89`) list claim, handoff, mention,
assignment, conflict and sign-in, not the gate and lock kinds (which
`docs/concepts.md:142-147` and `:249-252` do describe). One sink is
right; one list is missing.

**L6. The server's router and its work handlers are one file, and its
interfaces put locks under gates (medium).** The routing switch is in
`internal/server/server.go:1063-1070`; whether an op goes to the
control store is decided by a list in `limits.go:335-357` (`adminOp`,
`controlWrites`). `dispatch.go` holds the router's table and work
tracking's handlers inline (`:123-716`), while memory, cost, gates,
locks and coordination each have a file. In `stores.go`, `Work`
includes `AnnounceSignIn` (`:87`) and `Gates` includes the five lock
methods (`:135-139`). `settings.go` is one struct across every context
(`:35-150`), which is right for a settings file, and `mapErr`
(`errors.go:75`) maps every context's errors, which is right for one
wire.

**L7. Session rules are enforced in two layers (medium).** Who may pass
a gate (the session `cli` or `ci`) is the store's
(`internal/store/gates.go:373-389`), as is who may set the workflow
(`internal/store/workflow.go:143-148`); but "only the session `cli`
renews all of a principal's claims" is the server's
(`internal/server/dispatch.go:667-673`). Admin checks are in both the
server (`serverOnly`, `internal/server/admin.go:122-127`, and ad hoc at
`admin.go:742`, `:917`, `status.go:56-61`, `trail.go:41-78`) and the
control store (`internal/store/control_authz.go:236`, `:246`). A person
asking "where is the rule that stops an agent doing X" has to look in
both.

**L8. `internal/proto` holds some domain rules (low).** It parses the
workflow file (`internal/proto/workflow.go:128-279`), money
(`cost.go:221-286`) and dates (`gates.go:161`), and defines the
reserved sessions (`ops.go:587`); 14 files of `internal/store` import
it. In DDD terms `proto` is the shared kernel and the published
language, and holding the workflow's grammar there lets the client
check a file before it is sent (`aries workflow check`). It is fine as
long as it is said; the package doc does not say it.

**L9. The offline lists are hand-kept, and nothing holds them to the
protocol (high).** Which ops queue is `queued` and `never` in
`internal/outbox/entry.go:40-86`; which reads are cached or refused is
`internal/offline/cache.go:121-161`; and `internal/mcpserver` keeps a
third copy as per-tool `cached:` and `refused:` flags
(`internal/mcpserver/mcpserver.go:524-528`, `tools.go:616-726`). An op in
neither of the outbox's lists is silently "not queueable, with no
reason". Their tests (`internal/outbox/outbox_test.go:131`,
`unqueued_test.go:12`, `internal/offline/cache_test.go:398`) are lists
too. The op scopes, by contrast, are held to every op in `proto` by a
test that parses the package (`internal/server/scopes_test.go:20-45`).

**L10. The CLI and the MCP server each hold the same agent-facing rules
(medium).** Eight rules are written on both sides: retrying a finish or
handoff without paths (`internal/cli/work.go:187-198`;
`internal/mcpserver/tools.go:653-659`, `:675-681`); an update queued
only with its rev (`internal/cli/commands.go:592-604`;
`internal/mcpserver/tools.go:1015-1016`, worded differently); the
cache's refusal (`internal/cli/cache.go:82`; `internal/mcpserver/errors.go:38`);
why a write was not queued (`internal/cli/outbox.go:145-158`;
`internal/mcpserver/offline.go:166-171`); what a queued write prints
(`internal/cli/outbox.go:199-236`; `internal/mcpserver/offline.go:109`);
the close-out (`internal/cli/commands.go:629`;
`internal/mcpserver/closeout.go:32`); gate state
(`internal/cli/gate.go:270-372`; `internal/mcpserver/gate.go:59-81`);
and error to fix (`internal/cli/cli.go:462-505`;
`internal/mcpserver/errors.go:23-70`). Some of this is presentation and
belongs on each side; the rules under it (when to retry, when to queue)
do not. And the CLI's connection type lives in the MCP package
(`RepoConn`, `DialRepo`, `internal/mcpserver/repo.go:13`, `:41`), so
`internal/cli` imports `internal/mcpserver`.

**L11. The factory's text records are parsed in more than one place
(low to medium).** The `brief:` line is written by `internal/brief`
(`brief.go:312`) and parsed by `brief.ParseComment` (`brief.go:324-345`,
prefix `brief:`) and again by `merges.lastBrief`
(`internal/merges/issue.go:97-108`, prefix `brief: ` with a space).
Landing is spelled as a literal check name in `internal/cli/land.go:140`
and `internal/cli/gate.go:359`, and the default branch is resolved
twice, almost line for line (`internal/cli/land.go:86-100`,
`internal/cli/pulljob.go:87-100`). The adapter `merges` reads through
is defined in `internal/cli/outcome.go:208-286` and reused by
`prs.go:88`. The `Aries:` trailer, by contrast, has one parser
(`internal/gitx/gitx.go:97`) and three callers.

**L12. Small dependencies across contexts (low).** The daemon imports
`internal/gitx` to read an issue id from a branch
(`internal/server/gates.go:74`); `internal/credential` imports
`internal/offline` and `internal/client` for the data directory and an
ownership check (`internal/credential/file.go:31-62`). Each is one
function, and each is a hint that a helper lives in the wrong package.

### 5.2 In the docs

**D1. The design's principles and architecture still describe the
first draft (medium).** Principle 8 says "No git hooks"
(`docs/design/aries.md:25`) and §12's table says "A merged PR closes
the issue through the GitHub webhook. No git hooks are installed"
(`docs/design/aries.md:479`); decision 67 has no webhooks, and `aries
setup post-merge` offers git's post-merge hook for `aries land`
(`docs/cli.md:458`, `:688`). The architecture figure still shows
SQLite, GitHub webhooks and Dolt (`docs/design/aries.md:34`, `:44-45`).
As-built notes record each change below the text, as rule 17 asks, but
the figure is the first thing a new reader sees.

**D2. The README's status is a release behind (low).** It says "v0.6.0
is the current release" (`README.md:17`) and lists v0.7.0 as "In
progress on `main`" (`README.md:264`); v0.7.0 is tagged, and
`CHANGELOG.md:14` dates it 22 October 2026.

**D3. AGENTS.md's layout has no row for `internal/outcome` (low).** Every
other package under `internal/` has one; agents read that table to find
where a rule lives.

**D4. The memory scopes are described three ways (medium).**
`docs/concepts.md:577` says `project` reaches "Everyone on the server";
`internal/store/memory.go:36` says the project's members;
`docs/concepts.md:589-591` says `team` and `project` reach the same
people today, the project's, while `internal/store/memory.go:34` says
`team` is "everyone on the server". `docs/concepts.md:588` also says a
project's memories are "in its own database"; since decision 79 it is a
schema (`docs/server.md:1525`).

**D5. Principal kinds (medium).** `docs/concepts.md:10` says a principal
is "a person or an agent identity"; `docs/concepts.md:17` and
`internal/store/control_principals.go:40-41` say person or service.
`docs/design/outcome-record.md:68` calls agent, person and service the
"principal kind"; the code derives them from the session
(`internal/outcome/capture.go:222-230`).

**D6. The `acceptance` error code (low).** Its comment says it means
items are open (`internal/proto/errors.go:18-20`); the server also
returns it for required checks and review gates
(`internal/server/errors.go:466-496`), as `docs/concepts.md:450` and
`:476` say.

**D7. The session `cli` (low).** `docs/agents.md:676` says one per
machine; `internal/proto/ops.go:587-588` says one per principal and
machine.

**D8. An old variable name (low).** `docs/design/aries.md:161` names
`CLAUDE_SESSION_ID`, which `internal/client/session.go:14` calls the
older spelling of `CLAUDE_CODE_SESSION_ID`.

## 6. Where DDD helps here, and where it does not fit

**Where it helps.**

- **Names for the contexts.** AGENTS.md's layout table is the de facto
  context map, but it is ordered by package and its `internal/store`
  and `internal/server` rows are paragraphs that list every area. An
  agent briefed on "a cost change" today greps; with the contexts named,
  the brief can say which context and which invariants apply.
- **One meaning per word.** §4.1 has 21 words with two or more
  meanings. Most are harmless to a person who knows the code; to an
  agent, "session", "dispatch", "scope", "landed" and "check" each lead
  a search to the wrong file, and "landed" already gives two counts
  (L3).
- **Naming the layers that already translate.** `outcome.Store`,
  `merges.Store` and `internal/mcpserver/errors.go` are anti-corruption
  layers in all but name; `proto` is a published language. Saying so in
  their package docs tells the next author what may and may not cross.
- **Invariants stated once.** L2 and L9 are the two places where a
  rule has copies that can drift. DDD's habit of putting an invariant
  beside the thing it protects is what fixes them.

**Where it does not fit.**

- **Aggregates as objects with repositories.** The store's model is SQL
  in one transaction per operation on a single writer (rules 5 to 8).
  An aggregate exists in DDD to bound what one transaction must lock;
  here the single writer already serializes every write in a project,
  and `finish` legitimately changes the issue, its claim, its items, its
  handoff, the inbox and the event log at once. Splitting those into
  aggregates would force either several transactions (losing rule 5's
  atomicity) or a unit of work that is `wtx` under another name.
- **Domain events as an in-process bus.** The event log is already the
  truth and is written in the same transaction (rule 7); derived views
  are computed at read time (principle 3). A bus beside it would be a
  second source of truth.
- **A package per context.** Every domain context shares `wtx` and the
  writer. Moving `memory.go` or `cost.go` to their own packages would
  mean exporting `wtx`'s internals (`exec`, `event`, `notify`, `guard`,
  `now`, `lim`) or wrapping them, which widens the store's surface to
  make it look narrower. The control store shows the split that does
  pay: a different schema, so a different writer.
- **Rich domain models in the client.** The CLI is first: most of its
  code is parsing and printing, and the domain rules it holds (L10, L11)
  should move to one shared place, not into model types.

## 7. Recommendations, ranked

Ranked by what each makes easier to reason about against what it
costs. None needs a protocol change. The first four are small.

**R1. Fix the docs that disagree (D1 to D8).** One docs pull request:
redraw §2's figure as built (or add an as-built figure above it and
mark the old one as the first draft's), amend principle 8 to "no git
hooks unless asked", fix the README's status, add AGENTS.md's row for
`internal/outcome`, and make concepts.md's scopes, principal kinds and
`cli` session match the code. *Cost:* an hour or two, `go test
./scripts/`. *Briefs:* none. *AGENTS.md:* one new row.

**R2. One close-readiness function (L2).** A single `closeWaits` (gates,
items, checks, review) called by `close`, `finish` and the closing
pass, with one table-driven test over every kind of wait. *Cost:* a
small store change with its tests; no behavior change. *Briefs and
AGENTS.md:* none; rule 5's text on the closing pass can cite it.

**R3. Hold the offline lists to the protocol by a test (L9).** As
`scopes_test.go` does: parse `internal/proto`'s ops, and fail unless
every write op is in `queued` or in `never` with a reason, and every
read is cached or refused. Derive `internal/mcpserver`'s per-tool flags
from `internal/offline` rather than keeping a third copy. *Cost:* small;
one test and a refactor of the flags. *AGENTS.md:* the outbox row says
the test holds it.

**R4. A glossary in the user guide, and one meaning per word in prose.**
Put §4's table in `docs/concepts.md` (or a `docs/glossary.md` it links),
and in the docs qualify the overloaded words: "Aries session" and
"Desktop session id", "token scope" and "memory scope", "server
project" and "repository", "gate script" for `scripts/gate.sh`. Define
`landed` once and make `cost_outcome.go` and `internal/outcome` agree
(L3). Rename only what is internal: the server's op routing (`dispatch`
to `route`, `internal/server/dispatch.go`), so that "dispatch" means the
dispatcher. No wire or command name changes: from v0.5.0 they are kept
(rule 2, rule 10). *Cost:* small for the docs; small for the rename;
small for `landed`; a test fails when a glossary term no longer
appears in the code (decided, §8 question 2). *Briefs:* the template can name the glossary as the
meaning of its words, one line. *AGENTS.md:* a pointer from "What this
is".

**R5. Name the contexts inside the store and the server without moving
code (L1, L4, L5, L6).** Give `internal/store/doc.go` an "Areas" section
that maps each file to its context and names the invariants each
holds; collect the event kinds in one file grouped by context
(`events.go` exists and is 67 lines), and the inbox kinds in one list;
split `internal/server/stores.go`'s `Locks` out of `Gates` and
`AnnounceSignIn` out of `Work`; move work tracking's handlers out of
`dispatch.go` into files like the other contexts'. *Cost:* medium,
mechanical, no behavior change; the event and inbox constants keep
their values (rule 2's names kept). *AGENTS.md:* the `internal/store`
and `internal/server` rows shrink to a sentence and a pointer to the
package docs' areas.

**R6. Group AGENTS.md's layout by context.** Keep every row, under
headings: work tracking, coordination, memory, identity, offline, cost,
the agent factory, transport, operations, tooling and tests. Add the
context map (§3) to `docs/design/aries.md` as an as-built note under
§2. *Cost:* small to medium; `internal/agentspec` and the docs tests
are unaffected unless they parse the table (they do not today).
*Briefs:* a brief can name the context an issue touches, and the
dispatcher can do it from a label (`area:work`, `area:cost`), which the
`dim:value` labels already allow (`docs/design/aries.md` §3), tried
for one wave once R1 and R4's glossary have landed (decided, §8
question 9).

**R7. One home for the rules the CLI and MCP share (L10, L11).** Move
`RepoConn` and `DialRepo` from `internal/mcpserver` to
`internal/client`; move the retry-without-paths and queue-with-rev
rules, and the cache's and outbox's refusal reasons, to
`internal/offline` or `proto`, leaving each side its wording; have
`merges` read the `brief:` line with `brief.ParseComment`; give `land`
and `pull-job` one default-branch helper in `internal/gitx`, and the
check name `landed` one constant. *Cost:* medium; several packages'
tests. *Briefs:* none.

**R8. Settle where session and admin rules are enforced (L7).** Either
move "only `cli` renews all claims" into the store beside the gate's
session rule, or state in `internal/server/doc.go` which rules the
server enforces (scopes, the handshake, rate limits) and which the
store does (everything about who may change what). *Cost:* small for
the doc, small to medium for the move. *Briefs:* none.

**Not recommended:** splitting `internal/store` into a package per
context; aggregate types or repositories over the SQL; an in-process
domain event bus; a database per context beyond the control store's
schema. §6 says why.

**A domain-modeler agent spec: no.** AGENTS.md's rule is that a spec
adds working method only and repository facts belong in AGENTS.md
("Agent specifications"). The facts a domain modeler would bring are
§2 to §4 of this document, which belong in the guides and the package
docs (R4 to R6), where every agent reads them. The method it would add
("name the context, use the glossary, keep an invariant in one place")
is three lines that fit in `go-engineer`'s review checklist. A standing
modeler would be asked to review few changes (most issues sit inside
one context) and would pull toward the tactical patterns §6 rejects.
This review is better repeated as a one-off when Bearings starts, since
Bearings adds contexts (runs, budgets across providers, trains) of its
own.

## 8. Questions for the maintainer

Each has this review's recommendation and, under it, the maintainer's
decision of 23 October 2026: "Accept your recommendations for all 10
domain-review questions (glossary test now; area labels after the
glossary)."

1. **Should the contexts be named in the docs only, or in the code
   too?** *Recommendation:* in the guides, AGENTS.md and the package
   docs (R4 to R6), and in file layout only where it costs nothing; no
   package split (§6).
   *Decided:* as recommended.
2. **Where should the glossary live?** *Recommendation:* a section of
   `docs/concepts.md`, which people and agents already read, with a
   test later that holds its terms to the code if it drifts.
   *Decided:* a section of `docs/concepts.md`, with the test from the
   start: it fails when a glossary term no longer appears in the code.
3. **Which overloaded words are renamed in code, and which only
   qualified in prose?** *Recommendation:* rename only the server's op
   routing (`dispatch` to `route`), which is internal; qualify the rest
   in prose; rename nothing on the wire or in a command.
   *Decided:* as recommended.
4. **Are locks part of work tracking or of coordination?**
   *Recommendation:* coordination: they hold no issue, and decision 70
   keeps them away from agents. Split them from `Gates` in
   `stores.go` (R5).
   *Decided:* as recommended.
5. **Which `landed` is the product's: the close reason, or the check
   passed?** *Recommendation:* the check passed, as the outcome record
   has it, since an issue can be closed by hand after its check passed;
   make cost's group follow (R4).
   *Decided:* as recommended.
6. **Should the close-readiness rule (R2) and the offline-list test
   (R3) be done next, before the rest?** *Recommendation:* yes: they are
   the two places where a rule can drift today, and each is a small
   issue.
   *Decided:* as recommended.
7. **Is `internal/proto` allowed to hold domain parsing (the workflow
   file, money, dates)?** *Recommendation:* yes, as the published
   language both sides need; say so in its package doc.
   *Decided:* as recommended.
8. **Should a domain-modeler agent spec be added?** *Recommendation:*
   no; add three lines of method to `go-engineer` instead, and repeat
   this review when Bearings starts.
   *Decided:* as recommended: no domain-modeler spec; three lines of
   method in `go-engineer`'s three files (Claude Code's, Codex's and
   Gemini CLI's), and this review repeated when Bearings starts.
9. **Should issues carry an `area:` label naming their context, for
   briefs and the dispatcher?** *Recommendation:* try it for one wave
   of issues and read the outcome records before making it a workflow
   default.
   *Decided:* tried for one wave, and only after R1 and R4's glossary
   have landed.
10. **Should the docs fixes (R1) go in one pull request now, separate
    from the code changes?** *Recommendation:* yes, as one docs issue;
    it needs no decision beyond this one.
    *Decided:* as recommended.
