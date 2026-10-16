# Bearings: an agent orchestrator on Aries

Status: **draft specification**, open for review. Companion to
[aries.md](aries.md). Extended 16 Oct 2026 with §3.15 to §3.24
(organizations and portfolios, the dark factory, the wingman, control
through a leads' MCP and a web UI, governance, budgets, the model
gateway, and what they need from Aries), on the maintainer's decisions 21
to 30 (§7); the rest of what they say is proposed. No Bearings code exists.

Bearings runs and supervises many AI coding agents across sessions and machines.
Aries holds all state: issues, claims, leases, memory, events and costs.
Bearings holds none. It is the deterministic Go layer that starts agents, keeps
their claims honest, routes work to them and lands their output safely.

The name follows aries: a navigator fixes a position from several star
bearings, as Bearings lands one change from many agents.

The design draws on Gas Town and its successor Gas City, which proved the idea
and exposed its failure modes: claims that never release, reapers that kill live
agents, LLM supervisors that burn tokens, merges on red CI, and agents running
with every permission on the host. Each component below names the failure it
removes.

## 1. Principles

1. **Go supervises; LLMs judge.** Spawning, reaping, routing, budgeting and
   merging are deterministic Go reacting to events. A model is called only for
   a question that needs judgment, such as "why is this agent stuck?".
2. **No state outside Postgres.** Every fact lives in Aries or, for what
   is Bearings' own and binds no one else, in a Bearings schema beside it
   with its own single writer and event log (§3.23). A `bearingsd`
   restart loses nothing and adopts its running children; a standby
   ends running agents with handoffs. *Changed 16 Oct 2026 after review
   round 1: this read "Stateless. Every fact lives in Aries."*
3. **Event-driven, never polling.** Bearings subscribes to the Aries event
   stream (`evt` frames pushed over the connection to ariesd: the HTTPS
   WebSocket, or SSH where an operator turns it on; aries.md decision 86).
   No status loops, no per-operation CLI processes.
4. **Identity is the principal plus the session**, as in Aries, from the
   grant's or key's scope and never requested by the client. Never a
   path, a working directory or an environment variable.
5. **Safe by default; autonomous by policy.** A new project starts in
   pull-request mode with a human gate. An organization that turns on
   dark-factory mode (§3.16, decision 25) lets the merge train merge to the
   protected main branch once verification passes: green CI and the
   required checks, the issue's acceptance criteria verified, a review by
   a reviewer principal that is not the writer, and the project's policy
   gates. What holds in every mode: no merge on red, never a force-push,
   only the merge train holds push credentials, agents hold no secrets,
   release signing stays with people (AGENTS.md rule 3), and every merge
   can be reverted from the event log. Agents run in their harness's
   allowlist mode; unrestricted permissions only inside a sandbox.
   *Changed 16 Oct 2026: this read "Pull-request mode with a human gate. No
   merge without green CI, never a force-push, never a push to a
   protected branch"; decision 25 made the human gate the default a
   policy may replace.*
6. **Provider allowlist.** Bearings launches only providers and models an
   operator has allowed, and refuses everything else at config load.
7. **One static binary; Go on the server and in every helper.** The web
   UI (§3.20) is the one exception: its front end is Astro, TypeScript,
   React, Vite and Tailwind (decision 24), built to static files that
   `bearingsd` embeds and serves, so nothing but Go runs on a server or a
   developer's machine, and Node is a build-time dependency only, pinned
   and locked (§3.20, Build and supply chain). No hard dependency on tmux.
   *Changed 16 Oct 2026: this read "One static binary, Go only. No Node,
   Python or TypeScript components"; decision 24 changed it for the UI's
   front end alone.*
8. **Terse.** Same output rules as aries: failures print, passes only with
   `-v`, one summary line, `--json` for machines.

## 2. Architecture

```
  operator: bearings CLI · TUI      leads: the leads' MCP (§3.19) · web UI (§3.20)
                 │                                   │ HTTPS, Bearings' own origin
 ┌─────────────── bearingsd (one per machine; server parts marked *) ───────────────┐
 │ supervisor ─ patrol ─ dispatcher ─ governor ─ merge train*                        │
 │ wingman* (§3.17) · control API* (§3.18) · model gateway* (§3.24)                 │
 │ agent CLIs (Claude Code, Codex, Gemini), one process group and one git           │
 │ worktree each, optionally sandboxed; model calls through the gateway             │
 └──────┬───────────────────────────────────────────────────────────────────────────┘
        │ aries client: MCP for agents; RPC + event push for bearingsd,
        │ one connection per principal (the control API: per signed-in person)
        ▼
     ariesd ─── Postgres
```

*Changed 16 Oct 2026: the diagram said Dolt and an HTML status page.
Dolt became Postgres (aries.md decision 79), HTTPS the primary transport
(decision 86), and the wingman, the leads' MCP, the web UI and the model
gateway were added (decisions 22, 24, 26, 29 and 30). The starred parts
run only in the server `bearingsd` (§2, Deployment order), never on a
developer's machine.*

- `bearingsd` runs on every machine that runs agents, because it owns their
  processes and worktrees: a developer's laptop, a headless server, or both.
  Several can share one Aries server; that is the multi-machine story, and it
  mostly falls out of Aries's single authority and leases.
- **Deployment order.** Developer machines first, until Bearings is proven:
  supervise agents on the developer's machine under their own logins. Then a
  server `bearingsd` that keeps working when no developer is online. The merge
  train (§3.5) holds push credentials and the branch lock, so it runs in
  exactly one place, on that server.
- Agents talk to Aries through its MCP server. Bearings never types into an
  agent's terminal.
- `bearings` is the operator CLI. It talks to the local `bearingsd` over a unix
  socket.

## 3. Components

Each row names the Aries primitive it builds on and the Gas Town failure it
addresses.

| # | Component | Uses | Removes |
|---|---|---|---|
| 1 | **Supervisor with lease coupling** | claims, epochs, leases, agents registry | claims stuck `in_progress`; leases reading expired on live agents; orphaned processes |
| 2 | **Typed agent state** | agents registry, events | usage limits indistinguishable from hangs |
| 3 | **Event-driven patrol** | event push, reaper, gates | LLM patrol loops, heartbeat drift, idle token burn |
| 4 | **Conflict log; reservations later** | events; later reservations | rework from overlapping work, unmeasured |
| 5 | **Safe merge train** | locks, gates, events | merges on red CI, force-push recovery |
| 6 | **Structured handoff** | handoffs, scoped memory, prime | lost reasoning when a session ends |
| 7 | **Budget and concurrency governor** | semaphores, cost tracking (Aries §12.1) | rate-limit exhaustion, runaway spend |
| 8 | **MCP inbox instead of keystrokes** | inbox, event push, `aries setup` | send-keys fragility, provider-specific hooks |
| 9 | **Dispatch policy** | ready, claims, labels | first-come dispatch, manual assignment |
| 10 | **Formula runner** | molecules, idempotency keys, gates | duplicate workflow instances |
| 11 | **Observability** | events, agents, digest | many stores, polling dashboards |
| 12 | **Sandboxed workers** | principal and session mapping | host-wide credential access |
| 13 | **Epics** | parent links, gates, events, memory, cost | half-landed epics, stale bases, sibling drift |
| 14 | **Organizations and portfolios** (§3.15) | control store memberships and roles, cross-project edges (aries.md §20.7) | work across projects coordinated in chat and memory; no layer above the project (enterprise review L4) |
| 15 | **The dark factory** (§3.16) | gates, locks, events, the kill switch, `review` gates, `rollback`, escalations | approval at every step; no bound on autonomous work; no way to stop it or reconstruct it |
| 16 | **The wingman** (§3.17) | events, conflicts and proposals (aries.md §18.7), files to issues, cross-project edges, cost | conflicts found at merge; overlapping intent nobody saw; a portfolio's schedule risk nobody holds |
| 17 | **Control: one API, two surfaces, policy as code** (§3.18, §3.19, §3.20) | scopes, control roles, confirmations, event push | leads who cannot steer the factory; a UI and an MCP that drift apart; bounds one click away from those they bound |
| 18 | **Governance and budgets** (§3.21, §3.22) | the audit trail, envelopes, usage, prices and plans | an audit trail nobody can read whole; cost reported and never controlled (enterprise review §4, §5) |
| 19 | **Credentials and the model gateway** (§3.24) | usage with a source, the `gateway` scope, the Bearings schema | provider keys in agents' hands; models policy did not allow; self-reported counts |

### 3.1 Supervisor with lease coupling

- Spawn each agent CLI as a child with `Setpgid`, in its own git worktree, with
  a pty only when the harness needs one.
- While the PID is alive, renew the claim lease. Liveness is the process, not
  the model's willingness to call a heartbeat tool.
- On exit: if the agent finished, the issue is already closed. Otherwise
  release the claim, or mark it `stalled` with a handoff (§3.6) when work is in
  the worktree.
- On `bearingsd` restart, adopt live children by PID and session ID; reap the
  rest. Teardown kills the whole process group.
- A fenced epoch on every claim means a recycled agent with the same name can
  never act on a stale claim.

### 3.2 Typed agent state

`working`, `idle`, `blocked{reason, until}`, `exiting`, `stalled`.

- Derived from exit codes and the harness's structured output per provider,
  never from scraping terminal text.
- `blocked{usage_limit, until}` pauses the agent and its claims without being
  reaped, and resumes it at `until`.
- State changes are Aries events, so every observer sees the same truth.

### 3.3 Event-driven patrol

Go handlers on the event stream replace LLM watchdog roles:

| Event | Handler |
|---|---|
| lease expired | reclaim, or mark stalled with a recovery packet |
| session exited | apply §3.1 exit rules |
| gate opened (CI green, human approval) | advance the formula or merge train |
| budget threshold | pause dispatch; notify through the inbox |
| agent stalled longer than N | ask a model once for a diagnosis; post it to the inbox |

### 3.4 Conflicts: worktrees first, reservations later

Every agent works in its own worktree and branch, and conflicts are resolved at
the pull request by rebasing. That is the safety net, and the first stages
rely on it alone.

- **Log every conflict.** When a branch conflicts, `bearingsd` records an event
  with the issues and files involved, and the rework it cost in tokens.
- **Reservations are deferred.** If the log shows rework is expensive, add
  reservations: before dispatch, reserve the paths an issue will touch, and hold
  overlapping issues until the first merges. Sources, in order: paths in the
  issue's `design` field; files similar past issues touched (Aries's
  files-to-issues index); reserve on first write. No model call unless data
  shows it is needed.

### 3.5 Safe merge train

- An Aries lock (FIFO, leased) guards each target branch.
- Batch ready branches, test the batch tip, bisect on red, and eject the
  culprit back to its issue with the failure attached.
- Gates come from the Aries gate runner: required CI checks and, by default,
  human approval.
- **Queue only until the server runs.** The train orders, tests and opens pull
  requests; a human merges. Merging by the train waits for the server
  `bearingsd`, is then opt-in per project, and still requires green CI. The
  one exception is an epic branch: from B3 the train may merge a child with
  green CI into `epic/*`, never `main` (§3.13). Never force-push.
- **Merging to `main` in dark-factory mode** (§3.16, decision 25; changed
  16 Oct 2026, when this said "never push to a protected branch"). With the
  project's policy at level D, the server `bearingsd`'s train merges a
  branch to `main` when every verification passed: CI and the host's
  required checks (the `ci` gates of aries.md §20.3), the issue's
  acceptance checklist verified against the change by a reviewer principal
  (§3.16, Separation of duties), that reviewer's `review` gate, and the
  project's policy gates. It merges through the host's branch protection
  with a bot identity that the protection admits and that cannot bypass it
  or change workflows (§3.14), never around it. A merge that turns `main`
  red is reverted by the train (§3.16, Rollback). Below level D the human
  gate stays.
- Only the merge train holds push credentials. Agents never do. The
  train's principal (`merger-PROJECT`) writes no code and reviews none.

### 3.6 Structured handoff

When a session ends or its context fills, the supervisor asks the agent for a
`handoff` record (state, next step, branch, worktree, open questions), or writes
a minimal one itself from git and the event log. The next claimant receives it
in `prime` with the pinned and relevant memories. Provider-neutral; no session
forking.

### 3.7 Budget and concurrency governor

- Server-side semaphores cap active agents per developer, project and provider.
- Spend budgets in tokens or dollars per issue, per formula run and per day,
  using Aries's token capture and price table (§12.1). Subscription plans use
  the amortized cost.
- At a threshold the governor stops dispatching, lets running agents finish
  their current step, and posts to the inbox.

### 3.7.1 Accounting split

Aries owns the accounting: token usage per issue, session and `account`, the
price table, list-price and amortized cost, `aries cost`, `aries log` and
the cost reports (Aries §12.1; the MCP `cost` tool was removed by
decision 88). Bearings keeps no ledger. It does two
things:

- **Capture.** Because it launches each agent, Bearings reads the harness's
  usage output at the end of each turn or session and reports it to Aries
  against the claimed issue. This covers harnesses whose hooks cannot report
  usage.
- **Enforce.** The governor reads cost back from Aries to apply budgets.

The numbers are the same whether work ran under Bearings or by hand.

### 3.7.2 Plans and credentials

Bearings never calls a model API itself. It launches the providers' own CLIs
under whatever login they have, an API key or a subscription plan. Judgment
calls (§3.3) go through the same CLI in headless mode.

*Changed 16 Oct 2026 (§3.24, proposed): with the model gateway, the
providers' CLIs call the provider **through** Bearings, which holds the
key and forwards the call; Bearings still asks no model anything for
itself except the judgment calls and the wingman's, which go the same
way. The reason: a model allowlist and a budget can be enforced only on
the call, not inferred from a transcript afterward, and a key the agent
never holds cannot leak from it. Subscription logins stay as this section
has them, outside the gateway.*

- Parallel agents on one subscription share its usage limits. A limit is the
  `blocked{usage_limit, until}` state (§3.2), not a failure, and the governor
  caps concurrency per provider.
- Unattended servers need a login on that host. Check each provider's terms
  for unattended and parallel use; API keys are the usual fit for fleets.

### 3.8 MCP inbox instead of keystrokes

Nudges, mail and operator instructions go to the Aries inbox. The agent reads
them through its MCP `inbox` tool, or a session-start or prompt-submit hook that
reads the local Aries cache. tmux, if used at all, is a viewer.

### 3.9 Dispatch policy

- `ready --claim` with work-in-progress limits per agent and per pool.
- Label affinity: a label can require a provider or model.
- Priority aging so old low-priority work eventually runs.
- Steal a claim only after its lease expires.
- Pools scale from ready-queue depth, within the governor's caps.

### 3.10 Formula runner

TOML workflow templates instantiated as Aries molecules with idempotency keys,
so a retried start never duplicates a run. Steps advance on events. A step can
require a gate: CI, a human approval, or a named reviewer.

### 3.11 Observability

- `bearings who`: agents, state, claim, lease remaining, spend.
- `bearings board`: live TUI (bubbletea) over the event stream.
- The web UI (§3.20) is where people who do not run a CLI see the same
  truth. *Changed 16 Oct 2026: this read "`bearingsd --http`: a
  server-rendered status page (`html/template`), no JavaScript framework";
  decision 24 replaces the status page with the web UI, and B4 loses it
  (§6).*
- The Aries `digest` covers "what happened in the last N hours".
- OpenTelemetry traces and metrics, optional.

### 3.12 Sandboxed workers

Run each agent in a container with only its worktree mounted and a network
allowlist. Containers, not bubblewrap, so one design runs on macOS (most
developers, through the local container runtime's VM) and Linux (servers). Inject a session-scoped credential that ariesd maps to
(principal, session). No ambient `~/.ssh`, no git push rights. Unrestricted
agent permissions are allowed only here.

### 3.13 Epics: distributed work on one goal

An epic is an Aries issue of type `epic` whose children point to it with
`parent_id`. Bearings never dispatches the epic itself; it runs the children,
possibly on several machines at once, and lands them as one change. That is the
hardest coordination problem Bearings has, because siblings share a goal, often
share files, and depend on each other's code, not just each other's status.

**What goes wrong without a design**

| Failure | Cause |
|---|---|
| `main` holds half an epic | Each child lands on its own |
| A child builds on code that isn't there | Its blocker is *closed* but not yet *merged* where the child branches from |
| Siblings disagree on an interface | Each agent decides alone; nothing carries the decision to the others |
| Conflicts pile up late | Siblings touch the same area in parallel |
| Scope and spend creep | Agents file new children mid-epic and Bearings dispatches them |
| One failing child stalls the epic silently | Retries loop; siblings keep building on a broken base |
| The epic branch drifts from `main` | Long-lived branch, no one merges `main` back |

**Design**

1. **Plan, then approve.** A planning step (a person, or an agent at a person's
   request) splits the epic into children sized for one session each, with
   dependencies and an epic-level `design` (interfaces, conventions, out of
   scope). Dispatch starts only after a human approves the plan through an
   Aries gate. Parallelism comes from the dependency graph, so a poor split
   costs more than any scheduler can win back.
2. **One integration branch per epic.** The merge train creates
   `epic/<id>` from `main`. Children branch from its tip and land on it through
   the train (queue, test, then merge into the epic branch, since that branch is
   not protected). Only the final epic pull request to `main` goes to a human.
   Small epics can opt into `integration = "trunk"`: each child opens its own
   pull request to `main`, as for standalone issues.
3. **"Landed", not "closed", unblocks.** A child's dependents become dispatchable
   when it has *landed* on the epic branch, not when its agent closes it. The
   merge train records a `landed {issue, branch, sha}` event in Aries, and the
   dispatcher requires every blocker to be landed. Workers on any machine fetch
   the epic branch before creating a worktree, so they always start from code
   that contains their blockers.
4. **Shared epic context.** `prime` for a child includes the epic's `design`, the
   epic's decision log, and short handoffs from landed siblings. When an agent
   makes a decision others must follow (an interface, a name, a schema), it
   records it with the Aries MCP tools as an epic decision; Bearings pushes it
   to the inboxes of running siblings, whose next prompt sees it.
5. **Lower parallelism, measured conflicts.** Siblings collide more than
   unrelated issues, so each epic has its own cap (`max_parallel`, default 3)
   under the governor's global caps. The conflict log (§3.4) is grouped by
   epic; epics are where reservations would pay off first, if the log shows
   they are needed.
6. **Discovered work waits.** A child an agent files during the epic is created
   `deferred` with `discovered-from`. It is dispatched only after a human
   accepts it. A per-epic policy (such as "under N points and under budget")
   may replace the human later, once spend forecasts are trustworthy. Bugs
   outside the epic go to the backlog, not into the epic.
7. **Budget for the whole epic.** The governor caps spend across all children
   (`per_epic_usd`), forecasts remaining cost from children done so far, and
   stops dispatching when the forecast exceeds the cap, posting to the inbox.
8. **Failure stops the line, not the fleet.** A child that fails CI on the
   epic branch is ejected by the train and retried at most twice, with the
   failure attached to its handoff. Then it is marked `blocked{needs_human}`.
   Its dependents wait; independent siblings keep running.
9. **Keep the branch fresh.** When `main` moves, the train merges `main` into
   `epic/<id>` (never a rebase or force-push). A conflict there becomes a child
   issue at the head of the queue, and dispatch on that epic pauses until it
   lands.
10. **Close-out.** When every child has landed and the epic branch is green,
    Bearings opens the epic pull request to `main` and marks the epic
    close-eligible. The epic closes when that pull request merges. Canceling an
    epic stops its agents, releases claims and keeps every branch.

**What Aries needs** (beyond stage 3): a `landed` event and "blockers landed"
in the ready query used for dispatch (stage 6, with gates); epic decisions as
epic-tagged memory (stage 4); `epic status` and close-eligible (bd parity);
`cost --by epic` (stage 4, already designed).

**Distributed by construction.** Nothing in this design depends on where an
agent runs. Claims, the landed event, decisions and budgets live in Aries;
code moves only through the epic branch on the git remote. A laptop and a
server can work the same epic, and a child resumed on another machine starts
from its handoff and the branch.

**In dark-factory mode** (§3.16) items 1, 6 and 10 change: a planning agent
plans and the plan's gate is a `review` gate a reviewer principal passes
within policy, a human gate only where policy says so; discovered children
under the policy's size and budget are dispatched without a person
(decision 8's "per-epic policy" is that policy); and the epic pull request
to `main` merges through the train after verification (§3.5). Decisions 8
and 9 stand below level D.

### 3.14 Unattended operation

Work continues while no developer is online, such as overnight or over a
weekend, but no LLM leads it (decision 11). *Changed 16 Oct 2026: decision
25 narrows this section to levels A to C of §3.16. At level D the autonomy
window becomes the policy's standing envelope, the pre-approved queue
becomes the intent people set (goals, acceptance criteria, priorities), the
wingman plans and acts within policy while no person is present, and
merges into `main` follow §3.5. What stays in every mode: a server the team
controls, API keys, the bot identity, the circuit breakers, and parking for
a person where policy does not say otherwise.*

- **Autonomy window.** A person opens a window from the CLI with a scope (a
  project, epic or label set), a budget, a pull-request cap and an expiry of
  at most 72 hours. Workers may start work only inside an open window.
  Expiry is the dead-man switch; closing the window is the kill switch.
  ariesd stores and enforces it, so a crashed or misbehaving `bearingsd`
  cannot extend it.
- **Pre-approved queue.** Before leaving, people approve the work in scope.
  Stateless workers pull ready issues from it with leases. A model plans only
  while a person is present; unattended, nothing adds work to the queue.
- **Where it runs.** On a server the team controls (decision 12). Hosted
  agent sandboxes generally reach the internet only through HTTP proxies or
  domain allowlists, and Aries speaks SSH. *Note, 16 Oct 2026: written
  when SSH was the only transport. HTTPS (aries.md decision 86) removes
  the network objection; decision 12 stands on who controls the host.*
- **Credentials.** Provider API keys only, never a subscription login
  (decision 14). Pushes use a bot identity that cannot bypass branch rules or
  change workflows, with short-lived tokens checked before a window opens.
- **Merges.** Into `epic/*` with green CI when the window opts in (decision
  16); never into `main` (decision 17).
- **Park for a person** rather than guess: any change that deletes or weakens
  a test (decision 18), any change to `go.mod` or `go.sum` (decision 19), and
  any refusal the worker cannot resolve. Parked work waits in the inbox and
  the morning digest.
- **Circuit breakers.** The governor stops dispatch on repeated failures,
  error-rate spikes, provider rate limits or outages, and budget exhaustion,
  and never retries into a storm.
- **Aries needs:** the window itself; narrower default rights, so a worker
  cannot edit or close work outside its claim; distinct agent identities
  instead of many sessions under one developer's key; a person's claim that
  lapses during a window parks instead of becoming ready; and a claim epoch
  that fences git pushes, so two workers cannot push one branch. *16 Oct
  2026: the window is folded into §3.16's envelopes and pauses (an
  envelope plus an expiry that pauses the scope), and the rights and
  identities into §3.16's narrow worker rights and §3.23.*

### 3.15 Organizations and portfolios

Proposed 16 Oct 2026, on decision 21: coordination "across projects,
portfolios, and entire organizations". Aries has a server of many
projects and nothing above a project (enterprise review L4). Revised
after review round 1 (findings 11, 25, 27). Nothing here is built.

**The hierarchy.** Organization > portfolio > project, a tree: a project
is in at most one portfolio, a portfolio in one organization. Work two
portfolios share is a cross-project edge (aries.md §20.7, decision 74),
not dual membership. A portfolio is owned by its leads and their wingman
(decision 22); the organization by its leads and the org-level wingman.

**One organization per server (decision 31).** The control store, its
log, the audit stream and `admins:` are a server's; making the server the
organization gives the top of the tree for nothing: server admins are the
organization's admins and the control log its audit trail. Coordination
across servers is federation, which §5 rejects. Several organizations
would need an `organizations` table and a third admin tier.

**Rows of the control store** (one migration, described). Each written
through `Control.write` with its event (rules 5 and 7), each event in the
audit stream as an access change, which it is.

| Table | Columns | Events |
|---|---|---|
| `portfolios` | `name` (handle, a project's pattern), `display_name`, `state` (`active`, `archived`), `created_by`, `created_at`, `rev` | `portfolio.add`, `portfolio.archive` |
| `portfolio_projects` | `portfolio`, `project` (unique), `accepted_by` | `portfolio.project.offer`, `portfolio.project.add`, `portfolio.project.remove` |
| `scope_roles` | `principal`, `scope_kind` (`org`, `portfolio`, `project`), `scope`, `role` | `scope.role.set`, `scope.role.remove` |

**Roles.** Two kinds, kept apart (finding 27): *membership* grants reads;
*control roles* grant decisions (§3.18). A role at a scope holds at the
scopes below it. Admin tiers stay two (decision 14).

| Role | Kind | May |
|---|---|---|
| Server admin (`admins:`) | — | As aries.md §15.3, and the organization's admin: portfolios, roles at any scope, the non-human principals of §3.17 |
| `portfolio_admin` | control | Offer projects to the portfolio (below); set roles within it other than `portfolio_admin`; act as project admin in its projects by computed reach **only once the project's own admin accepted the offer** |
| `member` (org or portfolio) | membership | Read the boards of the scope's projects (`list`, `show`, `history`, `blocked`, `ready` as reads); write to a project only as its member |
| `lead`, `budget_owner`, `policy_owner`, `auditor` | control | §3.18 |
| `wingman` | non-human | §3.17; never a human role |

**Adding a project to a portfolio is two-step** (finding 11): a
`portfolio_admin` offers, and an admin of the project accepts
(`aries admin portfolio project accept`), or a server admin does both,
as decision 36's two-step has it. Until acceptance the portfolio admin's
reach does not cover the project, so no one gains a project by owning a
portfolio.

**Ops,** CLI only (rule 12): `aries admin portfolio add|archive|show|list`,
`portfolio project offer|accept|remove`, `aries admin role set|remove
PRINCIPAL ROLE --org|--portfolio P|--project P`; the bounded fan-out reads
`portfolio.board` and `org.board` over the open stores the caller may
read (one project per connection stays; finding 14).

**Limits** (rule 18, named by decision 88's rule): `portfolios_per_server`
100, `projects_per_portfolio` 50, `roles_per_scope` 200,
`projects_per_board` 50.

### 3.16 The dark factory

Proposed 16 Oct 2026, on decision 25: "We want to enable bearings to run
projects and portfolios with minimal human involvement during execution.
We want to create a true dark factory for organizations." The
maintainer's figure: "aries helps them find their way in the dark". Aries
is the shared map and the record, the issues, memory, event log, claims
and gates the agents navigate by; Bearings takes bearings from it and
steers. Revised after review round 1 (findings 3 to 7, 9, 12, 26, 29,
32, 33).

**What people do.** Set intent before execution (goals, specifications,
acceptance criteria, priorities), set envelopes and policy (§3.18), handle
what escalates, review after the fact and roll back. They do not approve
each action: at level D nothing waits for a person unless policy or a
hard limit says so.

**Autonomy levels,** set per scope in policy, a child never looser than
its parent, any scope settable back to A or B at once, and demoted by the
breakers by themselves.

| Level | Bearings and the wingman | For |
|---|---|---|
| A, analyst | Read, report, comment | Onboarding; what a breaker trips to |
| B, proposer | Propose (§3.17); nothing applies by itself | Collecting the acceptance data C needs |
| C, delegated | Apply the proposal classes policy turns on; the rest stay proposals | The path into D; the broad default once dark-factory mode is on |
| D, autonomous execution | Plan, dispatch, write, review, merge and resolve inside hard limits and the envelope | Dark-factory mode |

**Promotion is measured,** by a structural policy change (§3.18) citing
what Bearings reports from the logs over a window (28 days by default):
acceptance rate by proposal kind at B, rework rate (ejected branches and
reopened issues over merged), escaped defects (bugs with
`discovered-from` a merged change), rollback rate. Policy names
thresholds under which Bearings refuses to apply a promotion.

**Level D's preconditions** (finding 4, 12): a per-project
**protected-paths** list in policy covering build, test and CI
definitions, deploy files, `go.mod` and its like, and the policy files,
a change to which parks for a person; required checks run from `main`'s
workflow definitions, not the branch's (the host's setting, which
Bearings verifies at policy apply and the compliance report shows);
"weakens a test" defined deterministically as the test count or the
coverage of changed packages falling; a policy statement that **a merge
to `main` does not deploy**, or a `ci` deploy gate a person or policy
names (decision 35); an epic whose intent came through a lead's assistant
(§3.19) keeps a human plan gate; and `sandbox.mode = "container"` for
the server `bearingsd` (finding 3).

**Hard limits: never autonomous** (proposed; each needs the maintainer's
confirmation, §8 Q6). A person's act from the CLI, a policy pull request
or a tier-2 confirmation (§3.18); never a worker's, the wingman's or a
lead's assistant's alone:

1. Access and membership: principals, keys, trust rules, roles at any
   scope (`aries admin`; no leads' tool in the first slices, §8 Q4).
2. Raising a budget beyond the scope's envelope.
3. Release signing (AGENTS.md rule 3).
4. Deploying to production, unless policy names the environment and a
   gate.
5. Changing structural policy, the bounds and these limits included.
6. Deleting data: a project, a principal, events, memories, usage.
7. Anything outside the organization: another server, another company's
   repository, a provider or model not allowed, a payment.

ariesd enforces 1, 2, 5 and 6 by scope and role (§3.23); Bearings
enforces 3, 4 and 7 by holding no such credential and refusing at config
load (principle 6).

**Threat model: a compromised `bearingsd`** (finding 3). Assume an
attacker runs as `bearingsd`'s user on the server. What they get: the
core's own grant (dispatch, claims, proposals at its level), the BFF
sessions in memory, and the ability to start agents. What they do not
get, by construction: provider keys (a separate process and user,
§3.24); the push credential (the merger is a separate principal in a
separate process, and the host's protection requires a review from a
second GitHub App, §3.5); people's host tokens (none are held; policy
pull requests are opened by a bot naming the lead, approved on the
host); a tier-2 act (ariesd checks the person's re-authenticated
confirmation itself, §3.18); a change to the bounds (`policy.apply`
validated by ariesd under its own scope and second signer, §3.18); an
act outside the pause (ariesd enforces it). Agents never run as
`bearingsd`'s user: containers at the server (§3.12), which also stops a
same-uid agent reading its files. The core's own grant is held to least
privilege as well (decision 34): what dispatch and supervision need, and
no more.

**Safety machinery.** Deterministic Go on the event stream (principle 1);
no model is in the loop that stops things.

- **Kill switch.** `aries pause SCOPE [--halt] [--reason]` writes a
  `pauses` row in the control store (`pause.set`), pushed to every
  connection. ariesd then refuses `start` and `lock.acquire` by any
  principal of the scope except the exemptions below, and `bearingsd`
  stops dispatch within one event push. **Drain** (default): leases keep
  renewing while an agent finishes its step and writes a handoff (§3.6);
  a merge already pushed completes; nothing new is pushed. **Exempt from
  a drain** (finding 5): the merger's rollback ops (the branch lock, the
  revert, `rollback`) and `ci` reports of `landed`, so a red `main` can
  still be repaired and a breaker still reset. `--halt` kills process
  groups and ends claims explicitly with `claim.release` and a handoff
  from git. **Who may pause** (finding 26): a person holding any role at
  that scope or above, from any surface, tier 1, never refused for
  budget or limit; pauses are rate-limited per person (3 an hour) with
  an alert. **Resume** is tier 2 by a lead of the scope. **Break-glass**
  (finding 6): `bearings halt` over the local unix socket stops dispatch
  and the gateway when ariesd cannot be reached, and records the stop to
  Aries once it can; provider-side key revocation is the last resort,
  and the guide says where it is.
- **Circuit breakers.** The governor keeps per scope, over a sliding
  window from its own store's clock (finding 19): error rate, spend rate
  against the envelope's burn, rework rate, rollback rate, and whether
  `main` is red. A threshold from policy trips: the scope drains and
  drops to A for the cooldown, an escalation opens, `breaker.trip`
  records measure, value and threshold (numbers only). `breaker.reset`
  restores the level only if the measure is under the threshold; a
  second trip in a day needs a person's resume.
- **Rollback of a merge that breaks `main`** (finding 33). The merger
  records `merge.intent` (sha, pull request) before every push (finding
  13) and watches `main`'s `landed` gate. **Two** `landed` failures, or
  one with no other change on `main` since, within policy's window
  revert the merge: a revert commit by the merger through the host's
  protection, never a force-push; `rollback` names merge, revert and
  check; the issue reopens with the failure in its handoff; merges pause
  until green. A change policy marks irreversible (a migration, a deploy
  file; the protected paths) parks at D instead of merging, since a
  revert does not undo it. A revert the host refuses opens an escalation.
  A lead may roll back after the fact (tier 2) with the diff shown first.
- **Escalation.** A row of the Bearings schema (finding 14, 25), not an
  issue: `kind` (budget, breaker, conflict the wingman may not resolve,
  hard limit touched, policy question, review disagreement, lint),
  evidence refs, timeout and fallback from policy, and the deciders:
  **the holders of the deciding control role at the scope, resolved at
  decision time**, so no named approver list goes stale. Told through
  the inbox, the leads' `exceptions` and the digest. **No answer** within
  the timeout applies the fallback, always the safe one: hold (default)
  or drain; never proceed. After N unanswered (3 by default) the scope
  pauses; lint-raised escalations do not count toward N (finding 26).
  Cross-portfolio escalations go to the organization's leads.
- **An audit trail that reconstructs every decision.** Every act carries
  principal, session, surface (§3.17), the policy commit in force and,
  where a model judged, the proposal or judgment id, whose row holds the
  evidence refs (ids the Go side checked exist), provider, model, and the
  transcript's hash. **Transcripts** (finding 29) are the harness's, not
  the gateway's: `bearingsd` keeps them encrypted in its own store under
  Bearings' retention, readable by `auditor`s alone, after the secrets
  lint; the gateway keeps no body (§3.24). Text goes to no span (rule 2).
- **Budget envelopes,** §3.22.
- **Separation of duties, with custody split** (finding 3, 10). Writer,
  reviewer and merger are three principals, and policy refuses a scope
  where two are one: **one `worker-*` principal per worker** (finding 9;
  a session-bound grant minted at launch, so a steered worker cannot
  touch another's issue), `reviewer-*` (scope `review`, passes `review`
  gates only on issues it neither holds nor wrote, under its own GitHub
  App whose review the host's protection requires), `merger-PROJECT`
  (scope `merge`, in a process of its own under its own user with the
  push credential, writes no code). Only server admins create these
  principals, their keys and trust rules (finding 10). The scope is
  fixed by the trust rule or grant, never requested by the client, and
  Bearings' role scopes work over HTTPS only (SSH-only servers get
  levels A and B; §8 Q9). The wingman is a fourth principal. Separation
  among *people* (§3.18) is advisory and reported, not enforced.
- **Prompt injection, now that a steered agent can act.** Structural
  defenses: every text an agent reads is data, marked and passed through
  `safetext`; what an agent may do is its grant's scope at dispatch in
  ariesd, with **narrow worker rights** (finding 9, §3.23): a worker
  writes only its claimed issue, guarded by principal and claim epoch,
  comments anywhere, changes labels and edges only on its own issue, and
  writes no project memory except as a proposal; a steered reviewer can
  pass a gate it should fail, and a steered wingman apply a class policy
  allows, which is why the three are separate, the gate is never the
  writer's, protected paths park, and merges are reversible. An evidence
  ref that does not resolve voids a proposal. A lint, in the secrets
  lint's shape, flags text addressing agents in the imperative and
  escalates instead of acting. Tier 2 (§3.18) keeps a lead's assistant
  from a high-impact act.

**The autonomy window is folded in** (finding 32, decision 8 of
aries.md): a window is an envelope plus an expiry, and the expiry pauses
the scope; §3.14's text stands as the description of levels A to C.

### 3.17 The wingman

Proposed 16 Oct 2026, on decision 22: "The portfolio is owned by a team of
humans and their agentic AI wingman." The wingman helps resolve conflicts
between developers' and agents' work (git conflicts at the pull request,
Aries field conflicts of aries.md §18.7, overlapping intent found by path
overlap and the files-to-issues index) and helps leads coordinate a
portfolio (cross-project dependencies, priority, schedule risk, who is
blocked on whom). Below C it proposes; at C and D it acts within policy.
Revised after review round 1 (findings 8, 10, 23, 24).

**Identity and rights.** A service principal (`kind: service`, identity.md
§7) owned by a lead, with the role `wingman` at its scope, signed in by
workload token exchange (transport.md §7) under a trust rule that fixes
the scope `wingman` (finding 24: the scope is on the rule, never
requested). It never runs under a person's credential and holds no key.
**It runs as Go, not as a harness** (finding 23): the Go side gathers the
evidence, calls the gateway (§3.24) for a structured judgment (a JSON
answer to a fixed question, no tools, no shell), and the Go side files
the proposal or applies the class; there is no wingman tool list and
`aries mcp` is untouched. Its rights: reads across its scope (which
exceeds a service principal's member rights of decision 85 and
identity.md §7.1, so it is a narrowed decision, §7); `proposal.*`; at C
and D the ops a class allows, each guarded in the project as any write;
never an admin op, a human gate, a `review` or `merge` act, membership or
a payment. It never accepts its own proposal; a class it applies is
recorded as applied by policy with the class and commit.

**Surfaces.** Each grant's scope names the surface; ariesd records it on
every event and in the audit stream (ids only, finding 18) and dispatches
by it. Decision 42 and rule 12 bind interactive agents unchanged; the
wingman's authority is its role.

| Surface | Who | Scope | May |
|---|---|---|---|
| `cli` | A person in a terminal | `cli` | Every op the role allows (decision 68) |
| `lead` | A person through the leads' MCP | `lead` | Control ops by control role; tier 2 after a re-authenticated confirmation; never admin ops or a key's value |
| `web` | A person in the browser | `web` | The same control ops at their tier; the confirmation page; the secret input |
| `mcp` | A worker through `aries mcp` | `mcp` | The 24 agent tools, narrow rights; no `conflict.resolve`, no admin (rule 12) |
| `wingman` | The wingman | `wingman` | `proposal.*`, cross-scope reads, class ops at C and D |
| `review`, `merge`, `policy`, `gateway`, `ci` | Reviewer, merger, policy applier, gateway, CI | Each its own | `review` gates; locks, `merge.intent`, `landed`, `rollback`; `policy.apply`; `usage` with `source: gateway`; `ci` gates |

`conflict.resolve` from scope `mcp` is refused `forbidden` with a fix to
propose; from `wingman` it is allowed only while the scope's level and
class, as `policy.apply` set them in ariesd, permit.

**Triggers: Go detects, the model judges.** Each is a deterministic
condition on the event stream with a debounce and a budget check; then
the gateway is asked for a judgment with the evidence gathered.

| Event stream shows | The model judges | Kind |
|---|---|---|
| `conflict.park` (aries.md §18.7) | Which intent to keep, or a merge of the texts | `conflict` (`conflict.propose`, decision 42) |
| Bearings' conflict log: two branches conflict at the pull request (§3.4) | Which rebases, onto what, in what order | `merge_order` |
| Two held issues with overlapping paths, in a project or across the portfolio | Real overlap or coincidence | `overlap` |
| A `blocks` chain crosses a project; `due_at` near with the chain unlanded; a mirrored `dep.target` changes | Is the date at risk; what moves it | `schedule_risk`, `priority` |
| Two issues' text name each other with no edge | Does a dependency exist, which way | `dependency` |
| A lease expired twice; a holder silent past the window; `blocked{needs_human}` unowned | Who or which pool takes it, or release | `reassign` |
| Overlap while a claim is taken (G5, when built) | Whether to reserve | `reservation` |

**Proposals** are rows of the Bearings schema (finding 14): `id`, `kind`,
scope, `by`, `subjects` (`PROJECT/ISSUE`, 10 at most), `evidence` (ids
checked to exist, 50 at most), `change` (the structured change the kind
allows; never free text applied), `reason` (2 KiB, `safetext`), `state`
(`pending`, `accepted`, `rejected`, `applied`, `withdrawn`, `expired`),
decider, `policy_commit`. Accepting (a lead, tier 1 below D and tier 2 at
D, finding 4) or applying (the wingman at C and D) runs the kind's change
in the project as the actor through its writer and guards, with
`proposal: ID` on the project event; `conflict` proposals stay §18.7's
(`conflict.propose`, `aries resolve --accept`). Proposals expire after a
policy window. Statistics by kind and window (proposed, accepted,
rejected, applied, reverted, time to decision) feed promotion and are
what the next prompt is tuned on; a class reverted past a threshold is
demoted with an escalation.

**Across boundaries, by proposal.** Into another portfolio's work the
wingman only proposes, whatever its level; that portfolio's leads or
wingman decide under their policy. Clashes between portfolios escalate to
the organization's leads with the org-level wingman proposing.

**Analysis of developers' work** (decision 23: "an admin decision and not
an individual developer decision"). `analysis` is structural policy at
the organization or portfolio, `off` by default, a child never looser.
**Defined narrowly** (finding 23): per-person aggregates and reports
(hours, usage, throughput by principal). The wingman's coordination
triggers read issues, claims, paths and events regardless, since they
are the project's record and not a person's measure. Turning analysis on
writes `setting.set` to the audit stream; `prime` and `aries whoami`
print one line saying so and what is read (some jurisdictions require
notice; counsel decides the rest). The wingman never ranks, scores or
compares people, and names one only as a claim's holder or a gate's
approver. Whether issue and comment text may go to third-party
providers in its prompts, and whether the secrets lint runs first, is
§8 Q13; the design assumes the lint runs and the scope's provider
allowlist decides the rest.

**Budget and provider.** A line of the portfolio's envelope with its own
caps in policy (`wingman.per_day_usd`, `max_proposals_per_day`,
`per_proposal_usd`); the governor stops triggers at the cap, and Aries's
own budgets bound the principal at `start` when built. Provider and model
from the allowlist (principle 6), through the gateway.

### 3.18 Control: one API, two surfaces, policy as code

Proposed 16 Oct 2026, on decisions 26, 27 and 29. Decision 29: "I would
prefer to give human leads the option to control, authenticated and
authorized of course, the ability to control bearings through the MCP and
UI in that order of preference." Decision 27 holds in its narrower
sense: developers and product managers do not do day-to-day work on
individual issues through the UI; Bearings and the wingman execute.
Revised after review round 1 (findings 1, 7, 8, 19, 27).

**One Go control API behind both surfaces.** `internal/control` in the
server `bearingsd` holds every control as one function with one
authorization check; the leads' MCP tools and the UI's JSON endpoints
are thin callers. A table names each control with its tier, roles and
scope kind; a test holds both surfaces to it. The MCP gets every control
first, the UI the same controls in §6's order.

**Control roles,** at organization, portfolio or project scope, holding
downward; decisions belong to them alone, membership only reads
(finding 27).

| Role | May |
|---|---|
| `lead` | Pause and resume; decide escalations; set intent and priorities; change a scope's level and classes within the organization's bounds; roll back; decide proposals; reallocate within an envelope |
| `budget_owner` | Set and adjust envelopes within the parent's; one-time extensions within the override; the spent-envelope action within policy; draft organization envelopes |
| `policy_owner` | Draft structural policy; the required reviewer of it on the host (CODEOWNERS) |
| `auditor` | Read everything at the scope, the audit trail, reports, policy history, transcripts; write nothing |

Separation among people (§8 Q15): recommended as a policy rule at the
organization only, that a `budget_owner` who extended an envelope is not
the sole decider of a budget escalation it funds; advisory below, shown
by the compliance report.

**Tiers, and the confirmation the model cannot complete** (finding 1).

| Tier | Runs | Examples |
|---|---|---|
| 1 | From the call | Reads; the pause; deciding an escalation not marked sensitive; proposals below D; intent and priorities below D; reallocation within an envelope; drafting a structural change |
| 2 | After the person's re-authenticated confirmation | Resume; rollback; adjusting or extending an envelope; raising a level; sensitive or budget escalations; overriding a fallback; at D, `intent` that creates or re-scopes an epic, and `proposal_decide` |

Over MCP a tier-2 call applies nothing and returns an **opaque id**, never
a code the model could use. The person completes it on a surface that
re-authenticates: the UI's confirmation page after a fresh sign-in at
the provider (`prompt=login`, `auth_time` within 5 minutes; decision 85),
or `aries confirm ID`, which forces a fresh sign-in the same way. **ariesd
enforces it** (finding 1, 19): a tier-2 op is refused unless a
`confirm.done` by the same principal exists for it, single use, within
10 minutes on the store's clock, with the grant's `auth_time` checked.
In the UI a tier-1 control is the person's click, tier 2 a dialog and the
fresh sign-in. Shell-capable assistants (Claude Code) are **unsupported
for tier 2** until this is built and measured, since they could run
`aries confirm` with the person's `cli` token (decision 33). MCP elicitation may
accompany a confirmation and never replaces it.

**Operational controls and structural policy** (§8 Q14):

| | Operational: direct, recorded as events | Structural: policy as code, by reviewed pull request |
|---|---|---|
| What | Pause and resume; escalations; reallocation; envelopes within the parent's; extensions within the override; a scope's level and classes within bounds; priorities; intent | Hard limits and their configuration; the bounds on autonomy and promotion thresholds; separation-of-duties rules; the organization's envelopes and override; protected paths; the analysis setting; retention; breaker thresholds; gates per project; reviewer constraints |
| How | A control op as the person, with its event and the commit in force | The person describes it; Bearings writes the files and opens a pull request **as a bot naming the lead** (no person's host token is held, finding 3); `policy_owner`s review on the host; the merge applies |

Everything direct, files only exported, would put every bound one tier-2
call from the person it bounds; everything by pull request would make a
pause wait for a merge. The split is recommended.

**The policy repository,** `aries-policy` or a directory of the
organization's choosing: `org.toml`, `portfolios/NAME.toml`,
`projects/NAME.toml`, CODEOWNERS, branch protection. **Applying is
hardened** (finding 7): a principal `policy-apply` of its own with scope
`policy`, never the merger's; its trust rule pins `ref`, `event_name`
(`push` to the configured branch) and `job_workflow_ref`; **ariesd
validates every `policy.apply` itself**, refusing a child looser than its
parent whatever the repository's own test says, and a change to hard
limits or bounds only with a configured second signer (a second
principal's `policy.cosign` within the window); the event records the
commit, tree hash, who merged and the host's recorded approvers. The
operational state lives in Aries's control store (pauses, levels within
bounds, adjusted envelopes); `aries policy export SCOPE` writes the
effective policy, structure and state together.

**Emergency pause without a pull request:** operational, tier 1, from
any surface (§3.16); only resuming waits; a breaker trips the same state.

### 3.19 The leads' MCP

Proposed 16 Oct 2026, on decisions 26 and 29. Decision 26: "We would want
to expose bearings to its human leads primarily through an MCP." A lead
works from an assistant client against a remote MCP endpoint the server
`bearingsd` serves at the leads' origin (`https://bearings.example.com/mcp`)
over decision 86's HTTPS with OAuth (transport.md §9, measured for
connectors). Rule 12 bounds worker agents' tools; this is a second server
with a list of its own, governed by the lead's control role. Revised
after review round 1 (findings 1, 2, 4, 35).

**Acting as the person at ariesd** (finding 2). The client signs in at
Aries's authorization server with the client `bearings-leads`; `bearingsd`
**exchanges** that token at Aries's token endpoint (RFC 8693, on behalf
of the same grant) for a DPoP-bound token with audience ariesd and scope
`lead`, bound to a key `bearingsd` holds for that grant, and opens the
person's connection with it. No token is passed through (transport.md
§9.3), and the exchange is a requirement on Aries (§3.23). Serving the
leads' MCP from ariesd's own remote MCP machinery was rejected: ariesd
stays small (finding 14).

**Exposure profiles** (finding 2). A connector from claude.ai or Desktop
connects from Anthropic's cloud (transport.md §9.1), so with connectors
the control plane is internet-facing; a connector's token is bearer for
an hour. **Local clients** (Claude Code, a desktop client speaking MCP
over the user's network) work on the `private` and `airgapped` profiles;
**connectors only on the `internet` profile**, with `/mcp` and the UI on
separate listeners so the secret input (§3.24) is never on the
internet-facing one. Local clients come first (decision 32).

**Tiers** are §3.18's. The model never sees a completable secret.
`"untrusted": true` on text fields is guidance to the model, not a
control (finding 35); the controls are the tiers, the caps and
`safetext`. Results cap text, pass it through `safetext` as `--json`
does, and give ids and titles where a body is not needed.

**Tools, first list** (about 24). Tier 2 marked; D-only tier 2 in
brackets.

| Tool | Tier | Does |
|---|---|---|
| `status`, `board`, `who`, `history`, `digest` | 1 | Reads at a scope |
| `exceptions` | 1 | Open escalations with kind, age, timeout, fallback, evidence |
| `decide` | 2 | Decide an escalation (tier 1 for non-sensitive kinds waits on §8 Q17) |
| `proposals` | 1 | List the wingman's proposals |
| `proposal_decide` | 1 [2] | Accept or reject one |
| `pause` | 1 | The kill switch at a scope, drain or halt |
| `resume` | 2 | Clear a pause |
| `rollback` | 2 | Revert a merged change; the diff in the UI |
| `level` | 1 / 2 | A scope's level and classes within bounds; 2 to raise |
| `intent` | 1 [2] | Create or edit an issue or epic: goal, specification, acceptance criteria, due, priority |
| `prioritize` | 1 | Priorities across a scope's ready work |
| `budget`, `reallocate` | 1 | Envelopes, burn, forecast; move headroom within the share |
| `budget_adjust` | 2 | An envelope within its parent's; an extension within the override |
| `provider_keys` | 1 | Keys at a scope by name, state, expiry; register by secret-store path; never a value |
| `policy`, `policy_draft` | 1 | The policy in force and its diff; open a structural pull request |
| `audit`, `report` | 1 | Events by time, actor, surface, kind; compliance and feedback reports |
| `confirmations` | 1 | Open confirmations the person owes, by id and age; no codes |

**Budgets.** Schema about 5,500 estimated tokens, pinned by a test;
results 3,000 tokens, `report` and `audit` 4,000 with paging, `digest`
1,500; constants in the leads' server's `budget.go`.

### 3.20 The web UI

Proposed 16 Oct 2026, on decisions 24, 27 and 29. Decision 24: "the
backend code should be Go for the most part (e.g. APIs and endpoints) but
the frontend UI for bearings should be a mix of
Astro/TypeScript/React/Vite/Tailwinds." Revised after review round 1
(findings 2, 3, 28).

**The second surface for control, the first for seeing.** The same
controls as the leads' MCP through `internal/control`, in §6's order,
with §3.18's tiers for a browser (a click; a dialog and fresh sign-in).
First for what a chat client renders badly: live boards (project,
portfolio, organization); spend, burn and forecast against envelopes;
reallocations with reasons; breakers and pauses; policy versions and
diffs; the audit trail, compliance reports and exceptions; review after
the fact with a change's diff before a rollback; the confirmation page;
the secret input for a provider key (§3.24). No form for day-to-day work
on individual issues (decision 27). Served to leads, budget owners,
policy owners, auditors, members; a developer sees their projects'
boards.

**Where it lives.** `internal/web` in the server `bearingsd`, at the
leads' origin on its own listener (finding 2), never in ariesd
(transport.md §4.5: no page of ariesd renders user content).

**Acting as the signed-in person.** A back end for the front end (BFF):
the browser signs in at Aries's authorization server with `bearings-web`
as the client (a static client, one redirect at the UI's origin);
`bearingsd` holds the person's DPoP-bound token (scope `web`) in its own
store, keyed by a `__Host-` cookie that is `httpOnly`, `Secure`,
`SameSite=Strict` and carries a random session id only; one connection
to ariesd per signed-in person. "Tokens only in headers, never cookies"
holds: the token is in a header on the hop to ariesd. The alternative, a
token in the browser with DPoP from WebCrypto and ariesd open to a
browser origin, was rejected for the XSS exposure and the origin it
would open on ariesd; §8 Q19 asks the maintainer to confirm. **Sessions**
(finding 28, rule 18): idle 30 minutes, absolute the grant's day, rotated
on each re-authentication, logout revokes the grant at Aries; caps
`sessions_per_person` 10 and `sse_per_person` 10.

**Security.** CSRF: the `__Host-` cookie, `SameSite=Strict`, a required
custom header on every non-`GET`, and `Sec-Fetch-Site`/`Origin` checked
against the UI's origin alone. CSP on every response: `default-src
'none'; script-src 'self'; style-src 'self'; img-src 'self' data:;
font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri
'none'; form-action 'self'`; Astro's inline hydration scripts must be
either disabled or allowed by hash, **to be verified in U0** (finding
28), and a test fails the build on an inline script the policy does not
cover; `Strict-Transport-Security`, `nosniff`, `Referrer-Policy:
no-referrer`. No third-party script, font, style or beacon at runtime.
XSS: React renders text as text; `dangerouslySetInnerHTML` and `set:html`
forbidden by lint; no Markdown rendering in the first slices; links not
clickable. `safetext` in the browser: JSON written as `aries --json` does
(`\uXXXX` for DEL, C1, bidi), escapes shown visibly, every text cell
`unicode-bidi: isolate` with `dir="auto"`; truncation in the UI, never
the data. The secret input: one field, `autocomplete="off"`, posted once
over TLS to `bearingsd`, which hands the value to the gateway process
(§3.24) and keeps nothing; the response names the key and fingerprint.

**Live updates, never polling:** `bearingsd` holds the event stream on
each person's connection and pushes over server-sent events
(`/api/events`, `Last-Event-ID`, HTTP/2), with `internal/board`'s
debounce.

**Telemetry:** the Go API's spans and metrics as aries.md §16 and rule 2
have them; front-end telemetry none in the first slices, and if ever,
route names and durations to the UI's origin only, off by default.

**Accessibility and terseness:** WCAG 2.2 AA (keyboard, visible focus, a
word for every state, scaling text, reduced motion honored, a polite live
region per debounce); principle 8's rules where they apply: failures
first, one-line summaries, the CLI's words for states and codes, every
refusal with its `fix:`, the `--json` shapes behind the API.

**Build, release and supply chain** (finding 28, 30). `web/` with
`package.json`, an exact lockfile, exact versions, a short list (Astro,
React, Vite, Tailwind, TypeScript); Node pinned in `mise.toml` and
`.nvmrc`; `npm ci --ignore-scripts`, **to be verified** against esbuild's
and Tailwind's native packages in U0, else a documented allowlist of
install scripts. **The Go-only build is the default:** the embedded
assets are behind a build tag `web` that the release workflow sets, so
`go build ./...` and the gate need no Node and no empty `embed`
directory; a release-build test fails if the tag is set and the assets
are missing. The gate's `web` step (`tsc --noEmit`, ESLint with the
no-raw-HTML rules, unit tests, the build) runs when `web/` changed; `npm
audit`, which needs the network and is not deterministic, runs as a
scheduled workflow, not in the gate. Assets are built in the release
workflow and embedded, inside what the signed checksum covers; the
release records the lockfile's hash and Node's version, and builds twice
to compare. A bill of materials of the front end joins the release
(enterprise review E15).

### 3.21 Governance

Proposed 16 Oct 2026, on decision 28: "The webUI and bearings must also
provide governance and budget management." Governance and budgets are
administration, set through policy pull requests, control ops and the
CLI, watched in the UI and the leads' MCP. Each part names the gap of
enterprise review §4 it closes. Revised after review round 1 (findings
16, 17, 18).

| Part | Where | Closes |
|---|---|---|
| Structural policy as code with its review trail; operational controls as events, each with surface and role (§3.18) | The policy repository and `policy.apply`; the control logs | "Policy on what an agent may do: absent beyond claims"; the autonomy window "decided, not designed" |
| Control roles, `auditor` read-only (§3.18) | `scope_roles`, checked at every control op | "Read-only, auditor, finance or approver roles: absent" |
| Separation of duties with split custody (§3.16) | Principals, scopes, processes; a policy test | "Separation of duties: absent" |
| Hard limits (§3.16) | ariesd by scope and role; Bearings by holding no credential | The same, and the autonomy window's bound |
| The audit trail with the surface (§3.16, §3.17) | Every event: principal, session, surface, policy commit, proposal id | "Reading the log beyond one issue; the access log: absent" |
| `aries audit SCOPE --since --until [--actor] [--surface] [--kind] [--export jsonl]`, `--head`; the leads' `audit` | A read across the logs the caller may read; export | "Exporting the audit trail: designed in part" |
| Tamper evidence (finding 17) | Each event carries the hash of its predecessor over a **canonical encoding** (the event's columns in a fixed order, JSON with sorted keys, no whitespace, RFC 8785); `aries audit --head` returns a **signed checkpoint** (the head hash, the count, store time) for an external verifier to pull, so ariesd makes no outbound call; a verifier for exported JSONL ships with `aries` | "Tamper evidence: absent" |
| The audit stream (finding 18) | Stays decision 30's: access changes, plus **ids-only** lines for control acts (pause, level, envelope, escalation decided, policy applied); no `reason` text; every new event name registered in `internal/logs` and kept from 1.0 (decision 88) | A SIEM reads control acts as it reads access changes |
| Compliance reports (`aries report compliance SCOPE PERIOD`, the leads' `report`, the UI) | From the logs: what ran under which policy commit; overrides, who and why; escalations, decisions, time to decision, fallbacks applied; breaker trips; rollbacks; separation-of-duties checks per merge; where one person held two roles; analysis history | "Show us the audit log for last quarter" |
| Retention (finding 16) | Structural policy; periods never shorter than the compliance period or the idempotency and conflict windows; pruning only behind a signed checkpoint that records the head hash, as bookkeeping with one `retention.prune` event of counts; transcripts and Bearings' own rows under the same rule in the Bearings schema | "Retention, deletion, erasure: absent" (deletion of a project or person stays hard limit 6, a person's act) |
| The analysis setting (§3.17) | Structural policy, audited | Notice and works-agreement obligations (aries.md §17.7) |

Erasure, residency and encryption at rest stay where aries.md §17.7 and
the deployment guides have them.

### 3.22 Budget management

Proposed 16 Oct 2026, on decision 28. Enterprise review §5: cost is
reported and never controlled, and enforcement belongs in the authority
every agent passes through. Revised after review round 1 (findings 20,
21, 36).

**Hierarchical envelopes.** Organization > portfolio > project > issue or
session, in dollars, tokens, held time or concurrent sessions (the last
two the server measures itself), for a period. A child's envelopes sum to
no more than its parent's; ariesd refuses otherwise at `policy.apply`
and `budget.adjust`. The organization's envelopes are structural; below
them a `budget_owner` adjusts within the parent's as a tier-2 control
(`budget.adjust`), and extends once within the override; above that it
is a pull request.

**Enforced in real time:** the governor at dispatch and renewal, the
gateway on every model call (§3.24), and ariesd at `start` for agents
Bearings did not launch (enterprise review E3, §3.23). Spend is read
from Aries's usage, prices and plans; each report says its `source`,
set by ariesd from the reporting scope, never by the client (finding 20).

**When an envelope is spent,** the action is structural policy per scope,
applied mid-session by the gateway and at dispatch by the governor:
`pause` (drain and escalate), `degrade` (dispatch and route only to the
cheaper allowed models, and escalate; **mid-session a degraded request
is answered by the cheaper model, which a harness tolerates since the
response shape is the provider's**, to be verified in K1, finding 36),
`escalate` (hold new work), `continue` (go on and alert). Each is
`budget.exhausted` with scope, measure and action; the spend-rate breaker
trips before the envelope when burn says it will be reached.

**Dynamic reallocation within an envelope,** after K1's exact metering:
Bearings moves headroom between children of one parent toward
higher-priority work and to issues or sessions that need more, up to
`reallocation.max_share` (20% of the parent by default), never above the
parent, never into a paused or tripped scope. Each move is
`budget.reallocate` (from, to, amount, measure, the priority or forecast
that justified it). Above the share, or across portfolios, a proposal or
an escalation.

**A generic cost intake.** Aries has usage per issue, session and
account, prices by model and date, plans with amortized cost, hours.
Self-hosted models "still have a cost", as do GPU-hours and flat rates.
What it lacks, proposed as **1.x additions that leave `admin price` and
`admin plan` as they are** (finding 21): `unit` (`token`, `gpu_hour`,
`request`, `flat`) and `period` on prices; `units` with a unit on usage;
a provider that is a cluster or host; `aries usage report` for a job's
metering; and server-level price and plan tables that a project's
override, in place of moving them.

**Forecasts and burn rate** per scope and period from the log: spend to
date, burn over the window, the forecast to period end, an epic's
remaining cost from children done (§3.13 item 7); in the UI, the leads'
`budget` and `aries cost --forecast`.

**Cost attributed to the outcome.** `landed` and `rollback` (§3.5, §3.16)
join cost to a merged change, an epic's close and a reopened issue, so
"what a closed bug costs with this model, and how often it is reopened"
is one query; the leads' `report cost` and the UI show cost per outcome
by type, label, provider and model.

**Are budgets optional? Recommended: envelopes optional, the accounting
not.** A scope without an envelope spends uncapped and the report says
so; dark-factory mode requires an envelope at every level, which the
policy test and `bearingsd` refuse to run without.

### 3.23 What Bearings needs from Aries

Revised after review round 1 (finding 14): **ariesd keeps only what must
bind agents Bearings did not launch, or must survive a compromised
`bearingsd`.** Everything else lives in a **Bearings schema** in the same
Postgres, written by `bearingsd`'s single writer with its own event log
(rules 5 and 7 by analogy, with the writer's lock of `store/fence.go`;
decision 31). Nothing here is written into aries.md.

**In ariesd** (each op a protocol change under rule 10, in 1.x, §6):

| Requirement | Shape |
|---|---|
| Hierarchy roles and reads (§3.15) | Control migration: `portfolios`, `portfolio_projects`, `scope_roles`; the index computes reach; `aries admin portfolio`, `role`; bounded fan-out `portfolio.board`, `org.board` (no portfolio-scoped connection; one project per session stays) |
| Scopes and the surface | Grants and trust rules carry a fixed scope (`lead`, `web`, `wingman`, `review`, `merge`, `policy`, `gateway`); a `surface` column on both event logs; dispatch by scope per op; `conflict.resolve` refused for `mcp`, allowed for `wingman` by the level and class set by `policy.apply` |
| Token exchange for `lead` (§3.19) | RFC 8693 at `/oauth/token`: the leads' MCP token for a DPoP-bound token, audience ariesd, scope `lead`, same grant |
| Narrow worker rights (§3.16) | `wtx.guard` by principal **and claim epoch**; a `worker` kind may write only its claimed issue's fields, labels and edges, comment anywhere, and create no project memory; one grant per worker session |
| Pauses (§3.16) | Control table `pauses`; `pause.set`, `pause.clear`; refusals at `start` and `lock.acquire` by scope, with the `merge` rollback ops and `ci` `landed` exempt; renewals allowed during a drain; pushed as an `evt` |
| Confirmations' check (§3.18) | Control table `confirmations`: `confirm.open` (scope `lead`, `web`), `confirm.done` (scope `web` or `cli` with a fresh `auth_time`); a tier-2 op refused without one by the same principal, single use, 10 minutes on `wtx.now` |
| Review gates (§3.16) | Gate kind `review`, passed by scope `review` principals the gate names, refused for a principal that held the issue's claim or authored its branch's commits |
| Policy in force (§3.18) | `policy.apply` and `policy.cosign` (scope `policy`): commit, tree hash, approvers, and the levels, bounds, envelopes and hard limits by scope; ariesd validates monotonicity and the second signer; every event records the commit |
| Envelopes at `start` (§3.22) | Enterprise review E3 as designed there, read from `policy.apply` and `budget.adjust`; `usage.source` set from the reporting scope; gateway usage keyed apart from client reports, batched |
| Outcome ops (§3.16) | `merge.intent`, `rollback` (scope `merge`); `landed` as G1 has it |
| Audit (§3.21) | `audit` read across logs by time, actor, surface, kind; `prev_hash` over the canonical encoding; `audit --head` signed checkpoints; retention pruning behind a checkpoint |
| Time (§3.18) | Every window that authorizes an act checked on `wtx.now` and the grant's `auth_time` |
| Names (§3.17) | `provider_key.*` for key metadata events if the metadata is ever mirrored; the ops are Bearings' (`bearings keys`), and `keys`/`key.add` stay the SSH keys' |

**In the Bearings schema** (`bearingsd`'s writer, its event log, its
clock): proposals and their statistics; escalations, timeouts and
fallbacks; breaker windows and rate-limit counters; policy-commit
tracking and the pull requests opened; provider-key metadata (names,
scopes, fingerprints, states, expiries; never values, §3.24); gateway
session tokens (an HMAC over the session id under a persisted key, so a
restart loses none, finding 13); BFF sessions; transcripts, encrypted;
Bearings' retention; `merge.intent` mirrored for reconcile with the git
host at start.

**Principle 2 restated** (finding 13): *no state outside Postgres*. Every
fact lives in Aries or in the Bearings schema; a restart of `bearingsd`
loses nothing and adopts its children; failover to a standby is defined
as **running agents end with handoffs** (a standby cannot adopt another
host's process groups) and the train fenced by an Aries lock, so two
daemons never push one branch.

### 3.24 Model credentials and the model gateway

Proposed 16 Oct 2026, a requirement with a design and not yet a decision
(§8 Q16). The maintainer asked: "Are we able to do AI API key management
in bearings so we can ensure we use the right models and allocate the
correct costs?" Yes, if Bearings stands between every agent and every
provider: §3.7.2 is amended accordingly. Revised after review round 1
(findings 3, 6, 13, 20 to 22, 24, 29 to 31, 36).

**A process of its own** (finding 3): `bearings-gateway`, under its own
user on the server, holds the secret-store client and the keys' values in
memory and serves the proxy; the core `bearingsd` holds neither, and
asks the gateway over a local socket to mint a session token. Its
principal has scope `gateway` by its own trust rule (finding 24: one
platform identity and one process per role; one `bearingsd` principal
minting per-role grants would concentrate the trust the split removes).

**Credential custody.**
- Keys **by reference to a secret store**: a Go interface (`Secrets`:
  get, put, rotate, revoke, list by scope) with a 0600-file adapter and a
  Vault adapter on the standard library, and the cloud secret managers
  (AWS, Google, Azure) as **nested modules** (finding 30) so their SDKs
  never enter the main `go.mod`; each a justified dependency.
- A key's value is never in Aries, the policy repository, `argv`, a log,
  a span or a metric (rule 2). The Bearings schema holds name, scope,
  provider, state, fingerprint and expiry; the store holds the value.
- **Scoped** to an organization, portfolio or project, and the guide tells
  operators to match the provider-side workspace or project, so the
  provider's bill splits as the envelopes do.
- **Rotation, expiry, revocation.** Expiry in policy (90 days default),
  warned at a threshold; rotation switches on the next request;
  revocation stops every session on the key within one request, so
  **revoking is a kill switch** beside the pause.
- **Registered by `bearings keys add NAME --scope S --provider P`** over
  `bearingsd`'s unix socket, reading the value from a file or standard
  input (finding 21, 22: never through ariesd, never an argument), by the
  UI's secret input (§3.20), or by naming a secret-store path, the one
  part the leads' `provider_keys` tool may handle. Never through the
  leads' MCP, because the lead's model would see it.

**The gateway.** A small Go proxy on `net/http`: a reverse proxy with
streaming passthrough and three small usage parsers; no provider SDK.
- **A session token, never a provider key.** Minted at launch (§3.1),
  bound to session, issue, principal, the scope's model allowlist and
  the remaining envelope; derived by HMAC from a persisted key (finding
  13); **valid only while the claim's lease, as last renewed, is live**
  (finding 6), so it ends with the session, the claim or the pause.
- **Harnesses point at it** by their base-URL settings, from the
  documentation as read:

  | Harness | Base URL | Credential | Status |
  |---|---|---|---|
  | Claude Code | `ANTHROPIC_BASE_URL` | `apiKeyHelper` (a command printing the credential) or `ANTHROPIC_AUTH_TOKEN` | Documented; refresh on 401 **unverified** |
  | Codex CLI | `model_providers.NAME.base_url` with `env_key`; `OPENAI_BASE_URL` | The named variable | **Unverified** against a current release |
  | Gemini CLI | `GOOGLE_GEMINI_BASE_URL` | `GEMINI_API_KEY` | **Unverified**; the OpenAI-compatible route is the fallback |

  K1 and K2 verify each against the release they ship with.
- **Every request's model is checked** against the allowlist and refused
  with a fix in the provider's error shape; unknown models refused.
- **Metering** from the provider's response (streaming's final usage
  event included), reported to Aries **as the gateway's own principal
  and scope** so ariesd sets `source: gateway` (finding 20), **batched**
  (one report per session per minute, or at session end) under
  `usage_per_day` and the write rate, keyed apart from client reports;
  transcript capture stays for sessions outside the gateway.
- **Envelopes mid-session** by the spent-envelope action (§3.22).
- **Rate limits** per provider and key, from policy.
- **Self-hosted models** behind OpenAI-compatible endpoints routed the
  same way, cost from a configured rate per token or GPU-hour.
- **Fails closed** (finding 6): after `gateway.aries_timeout` (30 s) with
  no word from ariesd it refuses new calls with a fix; running responses
  complete. `bearings halt` stops it locally.

**Enforcement.** In sandboxed workers (§3.12) egress is allowed to the
gateway alone, which is why the server requires containers (§3.16);
unsandboxed workers are governed only as far as the harness honors its
settings, and the record says `gateway: enforced` or `configured`.
**Subscription logins** cannot pass through (the CLI signs in itself):
they stay as §3.7.2 has them, metered from transcripts, and policy may
forbid them in a scope (`providers.subscriptions = false`), as decision
14 does for unattended runs.

**Risks, stated.** It sees prompts and responses and **stores and logs
neither**: no body in any log line or span, counts, ids, model names and
codes alone, held by a test as `internal/logs`' tests hold ariesd's. It
is a high-value target: its own user, its own AppArmor profile (finding
36), no shell, keys readable by it alone, session tokens worthless off
it. It is a single point of failure: when it is down, sessions see
connection errors, the supervisor marks them `blocked{gateway, until}`
(§3.2) and resumes when `/readyz` answers; nothing falls back to a
direct key; availability is one active and a standby behind a load
balancer, as transport.md §13 has it. **An organization's existing
gateway** stands in through an adapter (`Gateway`: mint a session
credential, resolve a model, read usage), with Bearings still minting
session tokens and reporting usage from what it returns; the built-in
gateway is the recommended default, since it is what makes metering and
envelopes exact and what every install has.

**Testing** (finding 31): `internal/gatewaytest`, a fake provider that
answers with configurable usage and streaming; a fake harness binary
that calls the gateway and exits with typed states; a fake git host with
branch protection, required reviews and a merge queue; and a fake clock,
as `pgtest` and `iaptest` stand in today. The gateway is the
deterministic seam for the wingman's and the reviewer's judgments in the
soaks: the fake answers the structured question from a fixture.

**Operability** (finding 36): `bearingsd` and the gateway get metric and
log names under the names-kept promise (decision 88), an AppArmor profile
each in `deploy/apparmor`, a status report over the unix socket, and a
limits table in the server guide; the design's own model spend (the
wingman's judgments, reviews) is bounded by its envelope line and
estimated in W0 from the trigger rates the soak shows.

**Aries needs** (§3.23): `usage.source` from the reporting scope; the
`gateway` scope; the `unit` on prices as a 1.x addition. Key metadata
stays in the Bearings schema.

## 4. Configuration

One TOML file per project, `bearings.toml`, plus a per-machine file for local
limits. Sketch:

```toml
[providers]
allow = ["claude-code", "codex", "gemini"]   # anything else is refused

[pools.default]
provider = "claude-code"
max = 4
labels = ["backend", "docs"]

[budget]
per_issue_usd = 20
per_day_usd = 150

[merge]
mode = "pr"            # "pr" (default) or "train"

[epics]
integration = "branch" # "branch" (epic/<id>, default) or "trunk"
max_parallel = 3
per_epic_usd = 300
discovered = "hold"    # "hold" (human accepts) or a policy name
require = ["ci", "human"]

[sandbox]
mode = "none"          # "none" or "container"; "container" required on the server (§3.16)

[policy]               # server bearingsd only (§3.18); proposed
repo = "git@git.example.com:platform/aries-policy.git"
branch = "main"        # Bearings applies the merged commit, nothing else

[wingman]              # server bearingsd only (§3.17); proposed
principal = "wingman-platform"   # a service principal of Aries, never a person's
provider = "claude-code"         # must be in providers.allow; called through the gateway

[gateway]              # bearings-gateway, a process of its own (§3.24); proposed
listen = "127.0.0.1:8444"
secrets = "vault"      # "file", "vault", or a nested-module adapter
aries_timeout = "30s"  # fail closed after this long without ariesd

[leads]                # the leads' MCP endpoint (§3.19); proposed
listen = ":8443"
origin = "https://bearings.example.com"
profile = "private"    # "private" | "airgapped" (local clients) | "internet" (connectors too)

[web]                  # the web UI, its own listener at the same origin (§3.20); proposed
listen = ":8445"
```

*Changed 16 Oct 2026.* This file is a machine's: which providers it may
launch, its pools, its local caps, where the policy repository is, the
secret store and the gateway's listener (§3.24). Structural policy (the
hard limits, the bounds on autonomy, the organization's envelopes,
separation-of-duties rules, the analysis setting, thresholds) is in the
policy repository (§3.18), changed by pull request and applied from the
merged commit; operational state (pauses, a scope's current level within
bounds, adjusted envelopes) is in Aries's control store, set by leads
through the MCP or the UI; the `[budget]` and `[epics]` tables above
become a machine's caps under the envelopes, never above them.

## 5. What Bearings does not build

| Not built | Why |
|---|---|
| LLM supervisor hierarchy (Mayor, Deacon, Witness, Dogs as agents) | Token cost and drift; Go patrol does it (§3.3) |
| A mandatory planning agent as the only interface | Optional. The CLI, inbox and digest are the interface |
| Identity from path, cwd or environment variables | Forgeable; identity is principal plus session |
| tmux as IPC or a hard dependency | Fragile; the inbox replaces it |
| Mail stored as issues | Aries has an inbox |
| Federation, reputation, ledgers | Conflicts with one authority |
| Release workflows that push upstream with user credentials | Only the merge train pushes, and only to unprotected branches |
| Autonomous merge to main **by default** | A new project starts in PR mode with a human gate; an organization's policy turns on merging to `main` after verification (§3.16, decision 25). *Changed 16 Oct 2026: the row read "Autonomous merge to main"* |
| Unrestricted permissions by default | Only inside a sandbox |
| Per-operation CLI execs, polling dashboards | Source of orphans and connection exhaustion |
| A non-Go server, daemon, helper or runtime | Go only. *Changed 16 Oct 2026: the row read "Non-Go UI or helpers". The web UI's front end is TypeScript (decision 24, §3.20), built to static files that Go embeds and serves; nothing that runs on a server is non-Go, and Node runs only at build time* |
| A wingman that administers access, raises a budget past its envelope, changes policy, deletes data or signs a release | Hard limits (§3.16); a person does these, from the CLI or a policy pull request |
| A web UI for day-to-day work on individual issues (create, edit, assign, reprioritize one task, resolve) | Decision 27: no click-ops for developers' and product managers' daily work. Leads control Bearings through the MCP first and the UI second (decision 29); the work itself is done by agents, the wingman and the CLI (§3.18 to §3.20) |
| A provider key in Aries's database, the policy repository, a tool result or a log | §3.24: keys live in a secret store by reference in a gateway process of its own, are registered by `bearings keys add` or the UI's secret input, and never pass through ariesd or the leads' MCP |
| A wingman with a shell | §3.17: the wingman is Go asking the gateway structured questions; a harness session would give the most privileged host a shell |
| Admin or resolve tools in `aries mcp` for worker agents | Rule 12 and decision 42 bind agent tools; the wingman's and the leads' authority comes from their principals' roles and scopes, on surfaces of their own (§3.17, §3.19) |
| Federation of organizations across servers | One organization per server is recommended (§3.15); coordination across servers is federation, rejected above |
| Many providers on day one | Start with Claude Code, Codex and Gemini |

## 6. Plan

Bearings stages depend on Aries stages ([aries.md §13](aries.md#13-plan)).

| Stage | Delivers | Needs Aries |
|---|---|---|
| B0 | Supervisor (§3.1), typed state (§3.2), `bearings who`, provider allowlist, one provider (Claude Code) | 3 (leases, agents, inbox, event push) |
| B1 | Event-driven patrol (§3.3), handoff (§3.6), inbox delivery (§3.8), conflict log (§3.4), Codex and Gemini | 3 |
| B2 | Dispatch policy and pools (§3.9), governor (§3.7), live board (§3.11) | 3–4 (cost) |
| B3 | Merge train in queue mode, merging only into `epic/*` (§3.5), formula runner (§3.10), epics (§3.13) | 6 (locks, gates, molecules) |
| B4 | Sandboxed workers (§3.12), OpenTelemetry. *Changed 16 Oct 2026: the HTML status page moved to the web UI (U0)* | 3 |
| B5 | Server `bearingsd` running unattended inside autonomy windows (§3.14); merge train may merge into `epic/*` (opt-in) | 3 plus the autonomy window, scoped rights and git fencing (§3.14) |
| — | Reservations (§3.4), only if the conflict log shows rework is expensive | 6 (reservations) |
| — | Gas City shim: an `exec:` beads provider backed by Aries, so Gas City users can try it | 1 (issues, deps, ready) |

B5 is built right after B1, ahead of B2–B4 (decision 13): unattended work
is the goal, and B5 needs only the Aries primitives in §3.14.

Gate for B0: a dozen agents across two machines run for a day with no stuck
claims, no orphaned processes and no reaped live agents.

**Added 16 Oct 2026, reordered after review round 1** (finding 15; §3.15
to §3.24, all proposed). **Bearings is 1.x work.** None of it joins
protocol 8 or v1.0.0; each slice that adds an op bumps the protocol as
rule 10 and decision 78 say. The Aries prerequisites are split into what
v1.0.0 already holds and what 1.x adds, and sized. Order: the minimal
policy apply, then levels, then the wingman at A and B, then the leads'
MCP with reads and the pause, then the machinery, then control, then D.

| Prerequisite | Where | Size |
|---|---|---|
| HTTPS, tokens, scopes `cli`/`mcp`/`ci`, service principals, remote MCP, G0 to G2 | v1.0.0 (decisions 81, 83, 86, 87) | Done before any Bearings slice |
| G4 cross-project edges and issue edges | 1.x, aries.md §20.10 | About 2 weeks |
| A1: hierarchy tables, `scope_roles`, reads; the surface on events; fixed scopes on grants and rules | 1.x | About 2 weeks |
| A2: `pauses`; `review` gates; narrow worker rights with epoch guard; `merge.intent`, `rollback` | 1.x | About 2 weeks |
| A3: `policy.apply` and `cosign` with validation; `confirmations` and the tier-2 check; token exchange for `lead` | 1.x | About 2 weeks |
| A4: envelopes at `start` (E3), `usage.source`, batched gateway usage; `audit`, `prev_hash`, checkpoints; retention behind a checkpoint; `unit` on prices | 1.x | About 3 weeks |

| Slice | Delivers | Needs | Gate |
|---|---|---|---|
| H0 | Portfolios in `bearings` and the TUI: the two-step project add, boards, cross-project chains (§3.15) | G4, A1 | A member reads three projects' boards with membership in none; a write is refused with a fix; a portfolio admin gains nothing over a project until its admin accepts |
| P0 | The Bearings schema with its writer and event log; minimal policy as code: the repository, `policy-apply`, levels and bounds applied and validated by ariesd, `aries policy export` (§3.18, §3.23) | A1, A3 | A child looser than its parent is refused by ariesd whatever the repository says; a change to bounds without a cosign is refused |
| K0 | `bearings-gateway` as a process, custody: the `Secrets` interface, file and Vault adapters, `bearings keys`, rotation, expiry, revocation (§3.24) | B0, B5 | A key's value appears in no row, file, argument, log, span or tool result, by test; revocation stops every session within one request |
| K1 | The gateway: session tokens bound to leases, Claude Code pointed at it, the allowlist, batched metering with `source: gateway`, rate limits, fail-closed, sandbox egress, `bearings halt` (§3.24) | K0, A4 | Cost by issue equals the provider's usage; a disallowed model is refused with a fix; a sandboxed worker cannot reach the provider; with ariesd gone the gateway refuses within 30 s and `bearings halt` stops dispatch; no body in logs or spans |
| W0 | Wingman level A as Go with structured judgments through the gateway; the analysis setting, narrowly defined, and its notice (§3.17) | P0, K1 | Triggers run with analysis off; a per-person aggregate appears only with it on; a trigger fires once per debounce within its cap |
| W1 | Level B: proposals of every kind in the Bearings schema, `aries proposals`, acceptance in the CLI, the statistics (§3.17) | W0 | A steered wingman produces at most a proposal; an unresolvable evidence ref voids it; acceptance applies as the accepter and is refused where they lack rights |
| M0 | The leads' MCP, reads and the pause: token exchange, `status`, `board`, `who`, `history`, `digest`, `exceptions`, `proposals`, `budget`, `policy`, `audit`, `report`, `confirmations`, `pause`; the control API and roles; exposure profiles (§3.18, §3.19) | A1, A3, P0 | Every tool acts as the lead with surface `lead`; a passed-through token is refused; the pause drains within one event push while the merger's rollback and `landed` still pass; a person without a role at the scope cannot pause it |
| D0 | The machinery at level B: breakers, rollback after two failures with `merge.intent`, escalations in the Bearings schema with role-resolved deciders, envelopes by governor and gateway, split custody (reviewer App, merger process), narrow worker rights, protected paths, the lint, containers required (§3.16) | A2, A4, K1, B2, B3 | A soak with a red merge, a spend spike, an unanswered escalation, an injected issue, ariesd down and `bearingsd` partitioned: the merge is reverted, the scope drains, the fallback holds, the lint escalates, the gateway fails closed, and the log reconstructs each |
| M1 | Tier 2: opaque confirmations completed on a re-authenticated surface, `resume`, `rollback`, `level`, `budget_adjust`, `decide`, `proposal_decide`, `intent`, `prioritize`, `reallocate`, `provider_keys` by path, `policy_draft` as a bot naming the lead; `aries confirm` (§3.18, §3.19) | A3, D0 | A tier-2 call from the assistant changes nothing; an `aries confirm` without a fresh sign-in is refused by ariesd; the completed act applies once, as the same person |
| U0 | The web UI for seeing: BFF sign-in, sessions, live boards by SSE, exceptions, audit, policy diffs, the confirmation page; the `web` build tag and the gate's step; the CSP and `npm ci` verifications (§3.20) | M0 | A board follows the log with no polling; CSP, CSRF and `safetext` by test; the release carries the assets and `go build` without Node passes |
| U1 | Controls in the UI, tier 1 then tier 2 with the fresh sign-in: pause, decide, proposals, intent, priorities, reallocate, level, budgets, rollback with the diff, the secret input, policy drafts | M1, U0 | The same control API; the record shows surface `web`; a tier-2 click without a fresh sign-in is refused |
| U2 | Governance and budget views: envelopes, burn, forecast, reallocations with reasons, compliance reports, retention, transcripts for auditors (§3.21, §3.22) | A4, U0 | The compliance report for a period reconstructs every escalation, override, trip and rollback from the logs alone; a verifier checks an export against a checkpoint |
| D1 | Level C class by class by policy pull request citing W1's statistics; dynamic reallocation (§3.22) | D0, K1 | Applied proposals are reverted by one command each; a class past the revert threshold is demoted |
| D2 | Level D in one project an organization chooses: planning agents, `review` gates, merges to `main` after verification, discovered work within policy, the D preconditions (§3.5, §3.13, §3.16) | D1, M1 | A week with no person acting during execution: every merge verified and reversible, every escalation answered or held, spend inside the envelope, the compliance report complete |

Reservations (§3.4, G5) stay conditional. Decision 13's order (B5 after
B1) stands; D0 to D2 come after B2's governor and B3's train.

## 7. Decisions

| # | Decision |
|---|---|
| 1 | Developer machines first; a server `bearingsd` follows once proven, for work while no developer is online (§2). Decision 13 moves it to right after B1. |
| 2 | The merge train only queues, tests and opens pull requests until the server runs (§3.5). |
| 3 | Sandboxes are containers, on macOS and Linux (§3.12). |
| 4 | Build the Gas City shim (§6). |
| 5 | Worktrees and pull requests handle conflicts; log them, and defer reservations until the log justifies them (§3.4). |
| 6 | Bearings lives in this repository as `cmd/bearings` and `cmd/bearingsd`, in the same Go module, released under one tag with Aries, so it never ships against a protocol Aries does not speak. *Whether it holds with the web build and the cloud adapters is §8 Q11.* |
| 7 | Epics integrate on an `epic/<id>` branch by default; small epics may opt into `integration = "trunk"` (§3.13). |
| 8 | A human accepts discovered children; a per-epic policy may come later, once spend forecasts are trustworthy (§3.13). |
| 9 | A person or a planning agent may plan an epic; dispatch always waits for a human to approve the plan (§3.13). |
| 10 | From B3 the merge train may merge children with green CI into `epic/*`, never `main`, before the server runs; `main` still waits for a human. This narrows decision 2 (§3.5, §3.13). |
| 11 | No LLM leads unattended work. Unattended, stateless workers pull a queue people approved, inside an autonomy window; a model plans only while a person is present (§3.14). |
| 12 | Unattended workers run on a server the team controls, not in hosted sandboxes (§3.14). |
| 13 | B5 (server `bearingsd`) follows B1, ahead of B2–B4 (§6). |
| 14 | Unattended runs use provider API keys only, never subscription logins (§3.14). |
| 15 | An autonomy window lasts at most 72 hours (§3.14). |
| 16 | A window may opt in to merging green children into `epic/*` (§3.14). |
| 17 | Unattended work never merges into `main` (§3.14). |
| 18 | A change that deletes or weakens a test parks for a person (§3.14). |
| 19 | A change to `go.mod` or `go.sum` parks for a person (§3.14). |
| 20 | An HTTPS transport for hosted sandboxes waits; SSH stays the only transport for now. *Superseded 15 Oct 2026 by aries.md decision 86: HTTPS is the primary transport, SSH second and off by default.* |

**Maintainer, 16 Oct 2026** (§3.15 to §3.24). His words are quoted; each
row says what the design makes of them.

| # | Decision |
|---|---|
| 21 | Coordination is possible "across projects, portfolios, and entire organizations." The hierarchy is organization > portfolio > project, as rows of the control store with roles above the project (§3.15). One organization per server is the proposal, not the decision (§8 Q1). |
| 22 | "The portfolio is owned by a team of humans and their agentic AI wingman." The wingman is a service principal with a role of its own at the portfolio (and the organization); it never runs under a person's credential, never accepts its own proposal, and holds none of the writer, reviewer or merger roles (§3.17, §3.16). |
| 23 | Whether developers' work may be analyzed is "an admin decision and not an individual developer decision": the `analysis` setting, structural policy, audited, with notice; defined narrowly as per-person aggregates; the wingman never ranks people (§3.17). |
| 24 | "the backend code should be Go for the most part (e.g. APIs and endpoints) but the frontend UI for bearings should be a mix of Astro/TypeScript/React/Vite/Tailwinds." Principle 7 and §5's row change for the UI's front end alone: static files built at release time behind a `web` build tag, embedded in `bearingsd`, no Node at runtime (§3.20). |
| 25 | "We want to enable bearings to run projects and portfolios with minimal human involvement during execution. We want to create a true dark factory for organizations." People set intent and policy and handle exceptions; during execution Bearings and the wingman act within policy, up to merging to `main` after verification. Levels A to D; promotion measured; the machinery of §3.16. Narrows decisions 2, 8, 9, 10, 11, 17, 18 and 19 (table below); principle 5 and §3.5 amended. The figure for Aries's part: "aries helps them find their way in the dark." |
| 26 | "We would want to expose bearings to its human leads primarily through an MCP." A leads' MCP server, separate from `aries mcp`, over HTTPS with OAuth, acting as the signed-in lead by token exchange, tiered so that a steered assistant cannot take a high-impact act without the person's re-authenticated confirmation (§3.19). *Cost the review found (§8 Q2, Q3): connectors make the control plane internet-facing, and shell-capable clients cannot use tier 2 yet.* |
| 27 | "I would not want a team of developers or product managers to engage in click-ops with the bearings UI for day-to-day operations and work." Read, after decision 29, in its narrower sense: no forms in the UI for the daily work on individual issues; execution is Bearings' and the wingman's; control of Bearings is a lead's, on both surfaces (§3.18, §3.20). |
| 28 | "The webUI and bearings must also provide governance and budget management." Governance: policy as code with its review trail, separation of duties with split custody, hard limits, the audit trail with the surface, tamper evidence, compliance reports, retention, the analysis setting (§3.21). Budgets: hierarchical envelopes enforced by governor, gateway and ariesd, the spent-envelope action as a setting, reallocation within an envelope as events, a generic cost intake, forecasts, cost per outcome; envelopes optional except in dark-factory mode (§3.22). |
| 29 | "I would prefer to give human leads the option to control, authenticated and authorized of course, the ability to control bearings through the MCP and UI in that order of preference." One Go control API behind both surfaces; control roles `lead`, `budget_owner`, `policy_owner`, `auditor`; operational controls direct as events, structural policy by a pull request a bot opens naming the lead; confirmation tiers on both surfaces (§3.18). Supersedes the observe-only reading of decision 27. |
| 30 | A requirement, not yet a decision: "Are we able to do AI API key management in bearings so we can ensure we use the right models and allocate the correct costs?" Answered with custody in a secret store by reference, in a gateway process of its own, and a model gateway that mints lease-bound session tokens, checks every call against the allowlist, meters it and enforces envelopes mid-session (§3.24); §3.7.2 amended. The decision is §8 Q16. |

**Maintainer, 16 Oct 2026, on the questions marked first in §8.** His
words are quoted.

| # | Decision |
|---|---|
| 31 | "Keep ariesd light and move Bearings into a separate schema in the same database, one server per organization but with the capability to shard that server if necessary for write/read-only replicas." Bearings keeps its state in a `bearings` schema of the server's Postgres, with its own single writer and event log, and ariesd keeps only §3.23's rows (§8 Q1). One organization per server (§3.15). The deployment may scale out: read-only replicas serve reads that tolerate lag (boards, digests, the UI's views, the wingman's fan-out reads, reports), while every read a write depends on (a compare-and-swap's rev, the claim guard, envelopes at `start`, confirmations, policy validation) stays on the primary; and schemas may be placed on more than one Postgres primary, each schema with one writer still (rule 5). Neither is designed yet: §8 Q25. |
| 32 | "Local clients first." The leads' MCP serves local clients on private networks first; claude.ai and Desktop connectors wait for the `internet` profile with separate listeners (§3.19, §8 Q2). |
| 33 | On shell-capable lead clients: "Ok with the recommendation." They get reads and tier 1 only until ariesd checks the re-authenticated confirmation (M1) (§3.18, §8 Q3). |
| 34 | "Split custody and execute least privilege with security in mind." A compromised `bearingsd` is not game over: the gateway and keys, the merger and its push credential, and the reviewer's App each sit apart from it under a user of their own, no leads' host tokens are held, and ariesd re-checks the acts §3.23 lists. Each principal and process holds the least it needs (§3.16, §8 Q5). |
| 35 | "Merging to main does not deploy. It depends on the user's own CI/CD pipeline configuration." Bearings never deploys and treats a merge to `main` as not a deploy. Since an organization's own pipeline may deploy on merge, level D requires the organization's policy to state whether it does; where it does, a deploy gate the organization owns is required (§3.16, §8 Q6). Which of the seven hard limits stand stays open in Q6. |

**Aries decisions and AGENTS.md rules this design narrows** (review
finding 8). Neither aries.md nor AGENTS.md is edited here; each row is a
question in §8, and the rule 12 amendment is planned as its own pull
request once the maintainer answers.

| Narrowed | How | §8 |
|---|---|---|
| AGENTS.md rule 12 (no admin over MCP; keys, settings, prices, plans CLI-only) | The leads' MCP has `provider_keys` by path, `budget_adjust` and `level`, on a surface of its own with its own scope and role; `aries mcp` is unchanged | Q7 |
| Decision 6 (one module, one tag) | The web build and the cloud secret-store adapters | Q11 |
| Decision 8 of aries.md (autonomy window) | Folded into an envelope plus an expiry that pauses the scope (§3.16) | Q6 (the fold needs confirming) |
| Decision 12 (no admin tool over MCP, no web page) | A web UI and a leads' MCP for control, not for `aries admin` | Q7 |
| Decision 42 (a person approves conflict resolutions) | The wingman resolves at C and D within a class policy turned on | Q7 |
| Decision 68 (human gates passed from `cli` only; `inPerson()` in `store/gates.go`) | `decide` and gate passing from `lead` and `web` at tier 2 | Q17 |
| Decision 85 and identity.md §7.1 (a service principal has a member's rights) | The wingman reads across a portfolio; the merger, reviewer, gateway and policy applier have scopes of their own | Q7, Q14 |
| Decision 88 (names kept from 1.0) | New ops, events, metrics and limits only; `keys`, `key.add`, `admin price` and `admin plan` untouched; prices and plans at the server as 1.x additions | — |
| Bearings decisions 2, 8, 9, 10, 11, 17, 18, 19 | Stand below level D; at D replaced by policy with the limits of §3.16 | Q3, Q6 |

## 8. Open questions

Added 16 Oct 2026, each with the recommendation and what it costs.
Those marked **first** are what the rest of the design waits on; those
marked "(review, round 1)" came from the architecture review.

1. *Answered (decision 31).* One organization per server, with
   Bearings in a schema of its own, and replicas or more than one
   primary when needed (Q25).
2. *Answered (decision 32).* Local clients first.
3. *Answered (decision 33).* Reads and tier 1 only until M1.
4. **Access and membership changes:** CLI only in the first slices, or
   tier-2 tools on the leads' surfaces too? Recommended: CLI only.
   *(Review, round 1, Q14 merged:)* **who may create reviewer, merger,
   wingman and gateway principals and their trust rules?** Recommended:
   server admins only (§3.16).
5. *Answered (decision 34).* Split custody, least privilege.
6. *Deploys answered (decision 35):* a merge to `main` is not a deploy;
   the organization's pipeline decides, so D requires its policy to say
   whether that pipeline deploys on merge, and a deploy gate where it
   does. **Still open: which hard limits** of §3.16's seven does the
   maintainer confirm? Bearings itself never deploys, which makes the
   fourth (production deploys) a limit on what policy may let Bearings
   trigger, not on what it holds.
7. **first** **Should Aries decisions 12, 42, 68 and 85 be narrowed, and
   AGENTS.md rule 12 edited** as §7's table has it? (review, round 1)
   Recommended: yes, in one pull request to aries.md and AGENTS.md after
   this design is accepted. Cost: rule 12's text names surfaces and
   scopes, not "MCP".
8. **Do the Bearings prerequisites go into v1.0.0 (protocol 8, delaying
   it) or into 1.x with protocol bumps?** (review, round 1) Recommended:
   1.x, as §6 has it. Cost: none to 1.0.
9. **Must Bearings' separation of duties work on SSH-only and air-gapped
   servers,** where no token carries a scope? (review, round 1)
   Recommended: levels A and B over SSH; C and D need HTTPS. Cost: an
   SSH-only site gets no dark factory, which §3.16 says.
10. **One principal per agent pool, or one per worker?** (review, round 1)
    Recommended: one per worker, as a session-bound grant (§3.16, §3.23).
    Cost: a grant per launch, and the control index grows with them.
11. **Should the host's merge queue and required reviews by separate
    GitHub Apps replace the custom train for merges to `main`?** (review,
    round 1) Recommended: yes for `main` (the train orders and gates; the
    host merges through its queue, as enterprise review §8 argued), the
    train's own merging kept for `epic/*`. Cost: a host feature per
    platform. **And does decision 6 still hold** with the `web` build tag
    and nested modules for the cloud adapters? Recommended: yes, with
    those two.
12. **What is the required time to stop the factory when ariesd is down,
    and is a local break-glass halt wanted?** (review, round 1)
    Recommended: 30 seconds (the gateway's fail-closed timeout) and yes
    (`bearings halt`, §3.16).
13. **May issue and comment text go to third-party model providers in
    wingman and judgment prompts, and does it need the secrets lint
    first?** (review, round 1) Recommended: yes within the scope's
    provider allowlist, and yes, the lint first (§3.17).
14. **Policy: the split of §3.18** against everything direct or
    everything by pull request? Recommended: the split. **And the
    repository against the control store** for structural policy?
    Recommended: the repository.
15. **Separation of duties among people:** a policy rule at the
    organization only, advisory below? Recommended: yes (§3.18).
16. **Model credentials and the gateway (§3.24):** build it, as a process
    of its own, with the built-in gateway the default and an adapter for
    an organization's own; file and Vault adapters first; may policy
    forbid subscription logins? Recommended: yes to each.
17. **Scope `web` and `lead` and human gates:** may an approver pass a
    gate, or decide a non-sensitive escalation, from the UI or the leads'
    MCP (decision 68)? Recommended: at tier 2 on both. Cost: decision
    68's "only from `cli`" widens.
18. **Which level C classes first,** and the promotion thresholds?
    Recommended: `dependency` where both issues name each other and
    `overlap` notices, at 90% acceptance over 50 proposals; `conflict`
    last.
19. **The UI's token: BFF, or in the browser with ariesd open to a
    browser origin?** Recommended: BFF (§3.20).
20. **The leads' first tool list and budgets** (§3.19): is about 5,500
    schema tokens right for chat clients, and which tools go in M0?
21. **Direct budget edits in the UI** beyond a `budget_owner`'s tier-2
    adjustments within the parent's? Recommended: no.
22. **Are budgets optional?** Recommended: envelopes optional, the
    accounting not, an envelope at every level a condition of
    dark-factory mode (§3.22).
23. **Tamper evidence:** a hash chain with signed checkpoints an external
    verifier pulls (§3.21), or Postgres's own means? Recommended: the
    chain.
24. **The wingman as Go with structured judgments** (§3.17), not a
    harness session with tools? Recommended: Go; a harness session would
    give the most privileged host a shell and the largest injection
    surface. Cost: judgments are bounded to fixed questions.
25. **Replicas and more than one primary** (decision 31). Which store
    reads move to a replica, and how a reader there knows its lag;
    whether `store.DB` becomes one pool per Postgres primary, with the
    control schema on one and project and `bearings` schemas placed
    across them; how a project's schema moves between primaries with
    the writer's lock (`store/fence.go`) held; and what `pgtest` and the
    soak need to run it. Recommended: replicas first, for the UI and the
    wingman's reads, since aries.md §14's standby (decision 79) already
    replicates; more than one primary only once one is measured to be
    the limit. Cost: an ariesd change, so it is an Aries design and a
    1.x release, not Bearings'.
