# starfix against its goal: a gap analysis

12 Oct 2026. Status: **a review, for the maintainer to act on**. It decides nothing. It reads `main` at commit `ea749e5` and judges the product against the goal the maintainer stated the same day:

> a tool that an enterprise or team can use to help organize work and coordinate AI agents using a variety of foundational and open-source models to develop a product, maintain a product, and provide the team and enterprise with adequate governance, cost control, and feedback/improvement recommendations to continually refine and improve how work gets coordinated across a team and enterprise leveraging AI agents.

Scope: the client (`sfx`), the server (`starfixd`) and Bearings, which is a design with no code ([bearings.md](bearings.md)).

**How to read the classifications.** Every capability below is one of:

| Class | Means |
|---|---|
| **Built, tested** | The code and a test for it were found; the file or the test is named. The tests were read, not run, for this review |
| **Built, unverified** | The code exists; what has not been tested or run is said |
| **Designed** | A section of a design document describes it; no code |
| **Decided** | A numbered decision records it; no design says how |
| **Absent** | Not in the code, the designs or the decisions. The search made is said |

A design is never counted as a capability. "Starfix §n" is [starfix.md](starfix.md); "Bearings §n" is [bearings.md](bearings.md); "the server guide" is [docs/server.md](../server.md).

## Summary

**The judgment.** starfix today is a careful, well-tested coordination kernel for coding agents: issues, fenced claims with leases, handoffs, an inbox, an event log that is the truth, scoped memory and cost reporting. It is not yet the product the goal describes. Of the goal's six clauses, one is solid (coordinating agents on shared work), two are partial (organizing work; developing and maintaining a product), and three are mostly not there (governance beyond access, cost *control*, and feedback that recommends anything).

- **(a) For a team:** close. A team of terminal users whose agents run in one vendor's harness could pilot it now, with the README's own warning that it is not production-ready. What separates it from "usable by a team" is weeks of work, most of it already recorded: checking the many-projects setup on real machines, monitoring, token capture for more than one harness, and a first report that tells the team something about how its work went.
- **(b) For an enterprise:** far. Roughly two to three stages of work and eight or so product decisions. The first three reasons an enterprise would say no are identity (hand-managed SSH keys, no single sign-on, an agent indistinguishable from its person), the absence of anything for a person who does not live in a terminal, and the absence of any control over spend. None of the three is blocked by the architecture; each is blocked by a decision that has been deferred.

**The findings that matter most.**

1. **Cost is reported, never controlled.** There is no budget, cap, alert or forecast anywhere in the code or in starfix's own design; the only design for one is the Bearings governor (Bearings §3.7), which would govern only the agents Bearings launches and takes its limits from a file in the repository. Budgets belong in `starfixd`, checked at `start`, so they hold for every agent however it was launched.
2. **"A variety of models" is true for coordination and false for cost.** Setup, hooks and MCP cover eight harnesses, with tests. Token capture reads one vendor's transcript format and nothing else (`internal/capture/claude.go`), and there is no way for any other harness, wrapper or self-hosted model to report its own counts: `sfx usage` accepts only `--hook`. The wire protocol is already neutral; a generic intake is days of work and should come before any second vendor-specific reader.
3. **Feedback and improvement, the clause that ends the goal, has no design at all.** `digest` lists what happened in a window. Nothing computes a rate, a trend or a recommendation, and Bearings adds a conflict log and nothing more. The event log already holds every fact a first version needs (reopens, lapsed claims, waived acceptance items, overrides, handoff states, time and tokens per issue).
4. **The audit trail is complete but cannot be read, exported or verified by an auditor.** Every mutation is an event in its transaction, which is rare and valuable. But only `history ID` reads events, one issue at a time; no command reads the control database's log of access changes at all (`Control.Events` has no caller outside the store); nothing exports the log; and nothing makes it tamper-evident to anyone with the database account.
5. **An agent is not a principal.** An agent on a person's machine is that person: same key, same rights, including `sfx admin` if the person is an admin. Decision 8 says agents get distinct identities and narrower rights; no design says how, and Bearings B5 (unattended work) depends on it.
6. **Bearings predates stage 3a and has its stages in a risky order.** It was written for one project per server, puts unattended operation (B5) ahead of the budget governor (B2), names an HTTP status page and GitHub webhooks that contradict "starfixd opens no port another machine can reach", and rebuilds a merge queue that hosting platforms already provide. It is the cheapest thing to change, and it should be revised before any code.
7. **Nothing exists for a manager, product owner or executive.** The only views are a terminal board and CLI reports, each of one project. A web view was dropped (Starfix §12, item 7) and decision 12 rules out a web page for administration. A static report file needs no port and would not reverse either decision.

**What to do first** is in [What to do](#what-to-do): four items before a team, about a dozen before an enterprise, and a list of things to cut.

## Capability table

Evidence is a file, a test or a section. "Searched" names the patterns looked for in `internal/` and `cmd/` (non-test Go) and in `docs/`.

### 1. Organizing work

| Capability | Class | Evidence |
|---|---|---|
| Issues with type, priority, status, labels, parent, comments | Built, tested | `internal/store/issues.go`, `types.go:37-62`; `issues_test.go` |
| Dependencies, ready and blocked computed at read time, cycle refusal | Built, tested | `internal/store/graph.go`, `graph_test.go` |
| Acceptance checklist that blocks close, with waivers on the record | Built, tested | `internal/store/acceptance.go`, `acceptance_test.go`; migration 0009 |
| Epics as a type with children; cost by epic | Built, tested | `store/cost.go:152` (`CostByEpic`); `list --parent` |
| Epic status, close-eligible, graph output | Designed | Starfix §4 (Port); no `epic` or `graph` command in `internal/cli/cli.go` |
| Custom statuses and types | Designed | Starfix §4; the five statuses and five types are constants (`types.go`) |
| Due dates, deferral, estimates | Absent as a feature | `due_at` and `defer_until` exist on the wire for import (`proto/ops.go:80-81`); no CLI flag sets them (searched `due`, `defer` in `internal/cli`); `estimated_minutes` is dropped on import ([migrate-from-bd.md](../migrate-from-bd.md)) |
| Text search over issues | Designed | Starfix §4 and §5 list `search`; no `search` op in `internal/proto` and `ListArgs` has no text filter (`proto/ops.go:341`) |
| Scoped memory with a secrets lint | Built, tested | `internal/store/memory.go`, `internal/secretscan`; migration 0013 |
| `team` memory reaching beyond one project | Absent | Each project's memories are in its own database (Starfix §15.1, As built); [concepts.md](../concepts.md#memory) says "will reach further in a later release" |
| Many projects on one server | Built, unverified | `internal/store/control*.go`, `internal/server/registry.go`; `TestSoak` across two projects. Not run under systemd or on a cloud VM (Starfix §15.4, last note) |
| Cross-project dependencies | Designed | Starfix §13 stage 6, §15.5 item 5 |
| Portfolios, programs, roadmaps, milestones, priorities above a project | Absent | Searched `portfolio`, `program`, `roadmap`, `milestone`, `release` in designs and code |
| A view across projects (ready, digest, cost) | Absent | Every op is served by one project's store; `sfx admin status` shows operational figures only |
| A view outside a terminal | Absent | Web view dropped (Starfix §12 item 7); `bearingsd --http` is Designed (Bearings §3.11) |
| Import and export | Built, tested, one format | `internal/bdimport`; `starfixd import-bd`, `export-bd` run on the server only |
| Sync with other trackers | Designed, stage 7 | Starfix §4 ("Port, later"), §13 |

### 2. Coordinating AI agents across models

| Capability | Class | Evidence |
|---|---|---|
| MCP server, 24 tools, capped results and schema budget | Built, tested | `internal/mcpserver/tools.go`, `budget.go`; `TestTools` |
| Setup (MCP config, pointer, session-start hook) for Claude Code, Codex, Gemini CLI, Cursor, VS Code, Junie; rule file only for JetBrains AI Assistant; a config entry for Claude Desktop | Built, tested as file edits; unverified against the live products | `internal/agentsetup/agentsetup.go:87-125`; `TestHookFormats`, `TestTargets`, `hardening_test.go`. Starfix §5 says the trust wording for three harnesses is "from general knowledge" and one frontmatter key is unconfirmed |
| Token capture | Built, tested, **one harness** | `internal/capture/claude.go`; `capture_test.go`, `internal/e2e/usage_test.go` |
| Token capture for any other harness | Designed for two (held, README stage 3); Absent for the rest | README roadmap; `agentsetup.UsageHooks()` is Claude Code's alone (`TestUsageHooksAreClaudeCodes`) |
| A harness-neutral way to report usage (a file, stdin, an API) | Absent | `sfx usage` refuses without `--hook` (`internal/cli/capture.go:39`). The `usage` op itself is neutral: model matches `^[A-Za-z0-9][A-Za-z0-9_.:/+@-]{0,127}$` (`proto/ops.go:289`), which admits open-model names |
| One session id across hook and MCP | Built for one harness; a known split for the rest | Starfix §5 "Limitation, for the security review: session ids"; "No fix is built" |
| Claims with leases, epochs, fencing, a server reaper | Built, tested | `internal/store/claims.go`, `authz.go:66` (`wtx.guard`); `claims_test.go`, `TestSoak` |
| Handoffs, inbox with push, presence (`who`) | Built, tested | migrations 0005 to 0007; `inbox_test.go`, `internal/e2e/who_test.go` |
| File overlap ranking from git | Built, tested | `internal/store/paths.go`, `internal/gitx/changes.go` |
| An agent identity distinct from its person | Decided | Decision 8; Bearings §3.14 "Starfix needs". Today "agents on someone's machine use that person's key" (server guide, Principals) |
| Hundreds of sessions per developer | Built, unverified at that scale | The soak runs 5 principals × 5 sessions (`soak_test.go:101`). Defaults cap a principal at 32 connections a project (`server/limits.go:70`) |
| Locks, reservations, gates, WIP limits, label affinity | Designed, stage 6 | Starfix §7, §13; searched `reservation`, `OpLock`, `gates` |
| Starting, supervising and routing agents | Designed | Bearings §3.1 to §3.9 |

### 3. Developing and maintaining a product

| Capability | Class | Evidence |
|---|---|---|
| Branch name from an issue; `start --branch`, `--worktree` | Built, tested | `internal/gitx/gitx.go:80`, `:199`, `:216` |
| Issue id from a branch or a `Starfix:` trailer | Built, tested, used only for file paths | `gitx.go:104`, `:123`; used by `changes.go` |
| Linking commits and pull requests to issues; a merge closing an issue | Designed | Starfix §12 item 2; no webhook or pull-request code (searched `webhook`, `pull.?request`) |
| CI results, review state, releases, deployments, incidents | Absent in starfix; partly Designed in Bearings | Bearings §3.5 (gates from "required CI checks"), §3.13 (`landed` event) |
| Planning and decomposition | Designed, as a human-approved step | Bearings §3.13 item 1; `finish --discovered` is the only built decomposition |
| Similar closed issues at create | Built, tested | `internal/store/similar.go:108`; `similar_test.go` |

### 4. Governance

| Capability | Class | Evidence |
|---|---|---|
| Identity from an SSH key, pinned host key, forced command | Built, tested | `internal/client`, `internal/server/bridge.go`; `internal/e2e` |
| Keys, principals, memberships managed remotely; access that ends closes connections and releases claims | Built, tested; unverified on real machines | `internal/store/control_*.go`, `internal/server/admin.go`; `TestStaleKeysFileIsNotAHole`, `TestArchiveSweepsAsTheStoreCloses`, the soak's access checks |
| Single sign-on, short-lived credentials, directory provisioning | Investigated, undecided | Starfix §15.6 |
| Roles | Built, tested, three: server admin, project admin, member | `store/control_members.go:17-18`; `control_authz_test.go` |
| Read-only, auditor, finance or approver roles; separation of duties | Absent | Searched `role`, `viewer`, `read.?only` in `internal/store`; one rule approaches it: nobody changes their own keys (decision 19) |
| Event log: every mutation, in its transaction | Built, tested | `internal/store/events.go`; `writeOnce` refuses a write with no event (AGENTS.md rule 7); the soak replays the log against the tables |
| Reading the log beyond one issue; reading the access log | Absent | `history` takes an issue (`proto/ops.go:27`); `Control.Events` (`store/control.go:142`) has no caller outside the store |
| Exporting the audit trail | Designed in part | Starfix §16.4 audit stream: access changes copied to the journal. Project events are not in it |
| Tamper evidence | Absent | No hash chain or signature on events (read `events.go`). Dolt's commit history is content-addressed, but whoever holds the database account or the disk can rewrite it; Starfix §2 calls it "a second audit trail" available to admins by `AS OF`, which no command exposes |
| Autonomy window, parking, push fencing | Decided | Decision 8; Bearings §3.14. Searched `autonomy`, `unattended`: no code, and no section of starfix.md designs it |
| Approvals and human gates | Designed, stage 6 | Starfix §3 `gates`; Bearings §3.10, §3.13 |
| Policy on what an agent may do | Absent beyond claims | An agent has every right its principal has, MCP's missing admin tools aside; "narrower default rights" is Decided (decision 8) |
| Secrets lint | Built, tested, memories only | `store/memory.go:293`, `bdimport/memory.go:48`; Starfix §6: "applies to memories only so far". The security model says notes and comments "are not checked" |
| Retention, deletion, erasure of a person's data | Absent; investigated | The event log and usage rows are "never pruned" (server guide, Storage growth); no issue delete (searched `purge`, `DELETE FROM issues`); Starfix §15.6 "Hiding who a person is" |
| Data residency, encryption at rest | Left to the operator | [deploy/enterprise.md](../deploy/enterprise.md) mentions customer-managed keys as a cloud option |
| Per-principal limits on growth | Built, tested | `internal/server/limits.go`, `internal/store/limits.go`; `limits_test.go` |

### 5. Cost control

| Capability | Class | Evidence |
|---|---|---|
| Usage records, attribution to issues by claim history | Built, tested | `internal/store/usage.go`, `attribution.go`; `attribution_test.go` |
| Prices by model and date; list-price cost, exact arithmetic | Built, tested | `internal/store/price.go`, `cost.go`; `cost_test.go` |
| Subscription plans and amortized cost | Built, tested | `internal/store/plan.go`, `amortize.go`; `plan_test.go` |
| Human hours | Built, tested | `internal/store/hours.go`; `hours_test.go` |
| Reports by account, issue, epic, person, model | Built, tested | `store/cost.go:150-154` |
| Reports by harness, session, label, team, or across projects | Absent | The five groupings above are the whole list; prices and plans are tables of each project's database (migrations 0014, 0015) |
| Budgets, caps, alerts, forecasts, stopping work | Absent in starfix; Designed in Bearings | Searched `budget`: only token and path budgets. Bearings §3.7, §3.13 item 7 |
| Chargeback | Partial | `account` code names per issue, inherited (Built, tested: `issue_account.go`); no export, no account hierarchy, hours "carry no money" |
| Cost per outcome; comparing models for a class of work | Absent | Cost by issue and by model exist separately; nothing joins them with issue type, label or result |
| Proof against false reports | Absent, by stated design | Security model: "usage is for reporting, not billing proof" |

### 6. Feedback and improvement

| Capability | Class | Evidence |
|---|---|---|
| A window's activity as structured data | Built, tested | `internal/store/digest.go:114`; `digest_test.go` |
| Stalled work (48 hours without an event) | Built, tested | `digest.go:19` |
| Cycle time, rework, lapse, override or waiver rates | Absent | The events exist (`issue.reopen`, `claim.expire`, `acceptance.waive`, `admin.override`, `claim.take`); searched `digest.go` for each: none is counted |
| Trends over time; comparison across people, harnesses, models, projects | Absent | |
| Recommendations | Absent | Not in starfix.md or bearings.md (searched `recommend`, `retro`, `improve`); the one recommendation designed is per sync conflict (Starfix §8) |
| Conflict log | Designed | Bearings §3.4 |
| Memory as carried learning | Built, tested | As §1; nothing prompts an agent or a person to record a lesson when work goes badly |

### 7. Operability

| Capability | Class | Evidence |
|---|---|---|
| Step-by-step server guide, systemd units, three deployment examples | Built, partly unverified | The server guide; the many-projects path has not run under systemd or on a provider's machine |
| Signed releases, verified upgrade and rollback | Built, tested | `internal/release`; `cmd/starfixd/upgrade_test.go` |
| Protocol compatibility across released clients | Built, tested | `TestReleasedClients` (`proto_test.go:161`); protocol 2 to 6 |
| Backup | Documented, not automated or tested | A cron line for `dolt dump` (server guide); no test restores a dump (searched `dolt dump` in `internal/`, `deploy/`, `cmd/`: none) |
| High availability, failover, multi-region | Absent | "No high availability" ([deploy/README.md](../deploy/README.md)); searched `replica`, `standby`, `failover` in `docs/` |
| Metrics, traces, health checks | Designed; first slice in flight | Starfix §16 |
| `sfx admin status` | Built, tested | `internal/server/status.go`; `status_test.go` |
| Scale measurements | Built, on a laptop | [spike/dolt](https://github.com/ariesworx/aries/blob/fbbbf4655665845db81a72d2bbd1845472fd1c5a/spike/dolt/RESULTS.md) (4 vCPU Linux, about 250 writes a second); [spike/multidb](https://github.com/ariesworx/aries/blob/fbbbf4655665845db81a72d2bbd1845472fd1c5a/spike/multidb/RESULTS.md) (macOS; lists four things not measured) |
| SELinux; AppArmor profile | Unsupported; Decided | Decision 23 |

### 8. Bearings

| Capability | Class | Evidence |
|---|---|---|
| All of it | Designed | No `cmd/bearings`, no Go file mentions it |
| The starfix primitives B0 and B1 need (leases, registry, inbox, event push) | Built, tested | As §2 |
| The primitives B2 to B5 need (semaphores, locks, gates, molecules, autonomy window, agent identities, push fencing, `landed`) | Designed or Decided | Starfix §13 stage 6; decision 8 |

### 9. Adoption

| Capability | Class | Evidence |
|---|---|---|
| One-line install, no sudo; a Docker test server | Built, tested | `install.sh`, `internal/release/install_test.go`; `deploy/docker/smoke.sh` |
| Migration from one predecessor tool | Built, tested | `internal/bdimport` |
| Migration from the trackers enterprises run | Absent | |
| Apache-2.0 | Decided and done | `LICENSE`; decision 4 |
| A vulnerability disclosure policy, a support statement, an SLA | Absent | No `SECURITY.md` at the repository root |

## 1. Organizing work

**What is there.** A sound issue model: types, priorities, a parent link, seven dependency types of which two hold work back, acceptance criteria as a checklist that `close` enforces, and `ready` and `blocked` derived at read time so they cannot drift. Memory in three scopes, with compare-and-swap and a secrets lint. Since stage 3a a server holds many projects, each in its own database, with membership checked at connection. The one view for people is `sfx tui`, a read-only live board of a single project, and `sfx digest`, a window's activity as data.

**What is weak.**
- **A project is an island.** One database, one writer and one event log per project is a good isolation boundary and a hard reporting boundary. Nothing reads across projects: not `ready`, not `digest`, not `cost`, not memory. `team` scope is stored but reaches exactly what `project` reaches. A principal in six projects runs six commands in six checkouts, because the project comes from the repository's `.starfix.yaml`.
- **The model stops at the epic.** There is no layer between "server" and "project" (a team, a department, a portfolio) and none above the epic inside a project (a milestone, a release, a roadmap item). Priority is 0 to 4 on an issue; nothing sets priority across projects.
- **Planning fields are missing or unreachable.** `due_at` and `defer_until` exist on the wire and survive an import, but no command sets them. There is no estimate. An import drops `estimated_minutes` with a warning.
- **Finding things.** `list` filters by status, type, priority, assignee, parent and label. There is no text search, though the design lists one twice, and Dolt's full-text index was measured unreliable (`TestDoltFulltextIsUnreliable`), so search needs the Go scorer the similar-issues feature already has, or a different plan.
- **Import and export are one format, run on the server.** `import-bd` and `export-bd` are `starfixd` commands, so they need a shell on the server and cover one predecessor tool.

**What is missing.** Anything a manager, a product owner or an executive can open. That is a stated position, not an oversight: the web view was dropped so that `starfixd` opens no port. But the goal names "the team and enterprise" as the audience for governance, cost and feedback, and those readers do not run `sfx cost --by epic` in a checkout.

**Why it matters.** "Organize work" for a team is met. For an enterprise the unit of organization is not the repository, and the people who decide priority and budget will never see the tool. The cheapest honest fix does not need a port: a report that `sfx` writes as a static file (HTML or CSV) from `digest`, `cost` and the metrics of §6, per project and, for a server admin, across projects. Whether the product also needs a live web view is an open question for the maintainer, not something the code can settle.

## 2. Coordinating AI agents across models

**What is there.** This is the strongest part of the product. Claims are leases with a fenced epoch; the server's clock and reaper end them; `wtx.guard` enforces who may change a held issue in one place; every claim ends with an event of its own; and a soak test replays the event log against the tables under dropped answers, dropped connections and daemon restarts. The harness does the chores (renewal, session identity, file paths from git), so the model never heartbeats. MCP is the neutral interface, and the tool set is small on purpose.

**How neutral is it, claim by claim.**

| Claim in [agents.md](../agents.md) | Finding |
|---|---|
| Setup writes the MCP config, a pointer and a hook for seven agents, and a config entry for Claude Desktop | True as file edits, with table tests per harness (`TestHookFormats`, `TestTargets`, `TestJunieMCP`, `TestJetBrainsRule`) and hardening tests for links, duplicate keys and TOML. **Not verified against the running products**: the design says so for the trust steps of three harnesses and for one rule-file key. A harness that changes its config format breaks setup silently until someone notices |
| "Only Claude Code's [tokens] are captured so far" | True, and the limit is deeper than "so far" suggests. Capture is a reader of one vendor's transcript files. Every other harness reports held time only, so its cost is `unpriced` or missing, and an amortized plan is split only across the principals who did report tokens |
| Session ids: one harness sets one; the rest get `m-…` | True. For the five other harnesses with a hook, the hook's session and the MCP session are different ids, so presence and audit split in two (Starfix §5 names this a limitation with no fix built) |
| The harness name comes from `STARFIX_HARNESS` | True, and it is recorded on usage rows and in the registry, but no report groups by it, so "which harness did this work" cannot be asked |

**Open-source and self-hosted models.** Whatever harness runs them, coordination works if the harness speaks MCP over stdio. Cost does not: there is no reader for their usage and no way to hand `sfx` a count. Prices are per million tokens by model name, compared byte for byte with no aliasing, and none ships, so every deployment types its own. A self-hosted model has no list price at all; today the only way to represent its cost is a zero price or a "plan" whose fee is the hardware, which works arithmetically and is documented nowhere.

**Agent identity.** A principal is a key. An agent on a developer's machine holds the developer's key, so it *is* the developer: in every event, in every claim, in `cost --by person`, and in what it may do, `sfx admin` included if the developer is an admin (the security model says so and advises a separate principal for an agent that works on its own). Two agents of one person differ only by session id, which the client chooses and which "grants nothing". An agent sharing a person's key can also name itself session `cli` to reach the seven-day lease (Starfix §7, authorization note). Decision 8 promises distinct agent identities and narrower rights; nothing designs them.

**"1 to 100s of sessions per developer."** The mechanisms hold in principle; the defaults and the evidence do not reach that far:

| What binds, in order | Default | Note |
|---|---|---|
| `conns_per_principal` | 32 a project | Each `sfx mcp` holds one connection while it watches its inbox. The 33rd concurrent agent session of one developer is refused with `busy`. A setting, but the default contradicts the stated requirement |
| `sessions_per_principal` | 256 | Registry rows; past it the least recently seen drops out of `who`. Leases are unaffected |
| `conns` | 1,024 for the server | 100 developers with 20 sessions each exceeds it |
| Processes on the server | none | Inferred, not measured: each connection is an sshd session plus a `starfixd stdio` bridge process. Thousands of connections are thousands of process pairs on one machine |
| The single writer per project | measured about 250 writes a second on 4 vCPUs | Renewals, registry touches and usage calls all count as writes (`readOps`, `server/limits.go:154`). Several hundred idle sessions cost a few writes a second, which is fine; the write bucket of 10 a second per principal would bind for one developer at roughly 600 sessions |
| Evidence | 5 principals × 5 sessions | The soak has never run at 100 sessions a principal or 100 principals |

**What is missing.** A generic usage intake; a fix for the split session; an agent principal type; a load test at the stated scale, with defaults set from its result.

**Why it matters.** The goal says "a variety of foundational and open-source models". Today the product coordinates any of them and can account for one vendor's.

## 3. Developing and maintaining a product

**What starfix knows about today.** An issue, who holds it, what was handed off, which files its commits touched, what it cost. That is the ticket and the session. From git it knows only paths, read on the client: `gitx` builds a branch name, can check it out or make a worktree, and parses an issue id from a branch or a `Starfix:` trailer.

**What it does not know.** Whether a commit exists, whether a pull request is open, whether it was reviewed, whether CI passed, whether the change merged, was released, was deployed or was reverted. Closing an issue is an agent's assertion plus ticked acceptance items, which are also the agent's assertion. An agent that closes work that does not build is, to starfix, indistinguishable from one that closes work that shipped. This is the largest gap between "issue tracker for agents" and "develop a product".

| Part of the lifecycle | starfix | Bearings |
|---|---|---|
| Planning, decomposition | `finish --discovered`; epics by hand | A planning step a human approves (§3.13); otherwise out of scope by decision 11 |
| Branch and worktree | Built on the client | Owns worktrees per agent (§3.1) |
| Commit and pull request linkage | Designed (§12 item 2), not built | The merge train opens pull requests (§3.5) |
| Review | Absent | A gate "a named reviewer" (§3.10) |
| CI result | Absent; gates are stage 6 | Required checks gate the train (§3.5) |
| Merge and "landed" | Absent | The `landed` event (§3.13), which starfix must add |
| Release, deployment | Absent | Explicitly not built (§5) |
| Incidents, maintenance, recurring work | Absent: no incident type, no recurrence, no link from an alert or an external ticket | Absent |
| Documentation | Absent as a concept | Absent |

**What is weak.** "Maintain a product" is not addressed by either design. Maintenance work arrives from outside the tracker: an alert, a customer report, a dependency advisory, a failed scheduled job. Nothing creates an issue from an outside event, and stage 7's tracker sync is the only planned path in.

**A conflict to settle before building any of it.** Starfix §2 and §7 give the gate runner "GitHub webhooks". A webhook needs an inbound HTTP listener reachable from another machine. Decision 9 keeps SSH as the only transport and decision 29 allows one listener, on loopback only. Either gates are fed by something that polls or pushes from inside (the CI job itself running `sfx`, which the Team example already equips with a key, or `bearingsd` asking the hosting platform), or a decision is reversed. The first is the smaller change and is harness- and platform-neutral: a CI job that runs `sfx` to record a result against an issue needs no webhook and no platform-specific code in the server.

**Why it matters.** Without the result of the work (merged, green, reverted), §5 cannot compute cost per outcome and §6 cannot tell rework from progress.

## 4. Governance

**What is there, and it is good.** The principal comes from the key and nothing the client says. Rights are checked inside the write's transaction. Access that ends takes effect at once: connections close and claims are released, each with an event. Nobody changes their own keys. A disabled principal is refused everywhere, a config-file admin included. Text from others is treated as hostile to terminals and to agents. Limits bound what any principal can grow. A real security review ran in batches and its findings are recorded with their fixes. For a project of this age that is unusual discipline.

**Where it stops for an enterprise.**

| Asked by a security or compliance team, roughly in this order | Today |
|---|---|
| Does it use our single sign-on and second factor? | No. Keys are added by an admin, one per machine, and never expire. §15.6 tried SSH certificates from a sign-on login against a real sshd and found they fit the keys-file mechanism; it is an investigation, not a decision |
| When someone leaves, how fast is access gone? | When an admin runs `principal disable`, at once. Until then, never. Nothing follows a directory |
| Can we tell a person from their agents? | No (§2 above) |
| Who can read what? | Every member of a project reads every issue, comment, memory outside `user` scope, everyone's hours and notes, and all cost. There is no read-only role and no narrower one |
| Can the person who sets prices and plans be someone who cannot override work? | No. Project admin is one role: members, keys within reach, overrides, forced close, prices, plans, undoing others' hours |
| Show us the audit log for last quarter | Per issue, with `history`. The access log (who was added, which key, by whom) is in the control database and no command reads it. There is no export. §16's audit stream (in flight) will copy access changes to the journal; it does not cover project events |
| How do we know the log was not altered? | You do not. Clients cannot edit events; anyone with the Dolt account or the disk can. Nothing chains or signs events, and nothing anchors them off the machine. The enterprise example's write-once backup bucket is the nearest thing, at a day's granularity |
| What may an agent do with no person watching? | Whatever its principal may. The autonomy window is decided (decision 8) and described in Bearings §3.14; no design in starfix.md says what the window is as data, who may open one, or how `start` consults it |
| Where are approvals recorded? | Nowhere. Gates are stage 6. A waived acceptance item and an admin override are on the record; an approval is not a thing |
| Can a secret end up in it? | In a memory, the lint refuses it. In an issue body, a comment, a handoff note or an hours note, nothing checks, and the event log keeps it forever |
| Retention and erasure? | Events and usage rows are never pruned; there is no issue delete; a principal's name is text in every event of every project. §15.6 sets out three depths of answer and chooses none |
| Where is the data, and is it encrypted at rest? | Wherever the operator puts the one VM; the disk's encryption is the cloud's |

**What I would not change.** MCP carries no admin tools (rule 12). The event log is the truth. One authority. These are the properties an enterprise would pay for.

**Why it matters.** "Adequate governance" for a team is met by what is built. For an enterprise the kernel is right and the perimeter is missing: identity federation, role granularity, an audit trail an auditor can take away, and a policy on unattended agents that the server enforces.

## 5. Cost control

**What is there.** Reporting that is more careful than most: counts come from the harness and never from the model; a record is keyed by the harness's request id so resending stores it once; attribution is computed on read from the claim history, in whole tokens that sum exactly; prices are dated so history is never rewritten; cost is exact decimal arithmetic; a model with no price says `unpriced` and never `$0.00`; subscriptions are amortized across the work in proportion to tokens. Hours sit beside tokens.

**Can it control anything?** No. Each row below was searched for in the code and in both designs.

| Control | starfix | Design |
|---|---|---|
| A budget for a project, account, epic, person or issue | Absent | Bearings §3.7: per issue, per formula run, per day; §3.13: per epic |
| A cap that stops work | Absent | Bearings: the governor "stops dispatching" |
| An alert at a threshold | Absent | Bearings §3.3: "notify through the inbox". Starfix §16 ships no cost metric or alert |
| A forecast | Absent | Bearings §3.13 item 7, for an epic, from children done so far |
| Per-team limits | Absent; there is no team | None |
| Chargeback to cost centers | `account` code names; the map to real cost centers "lives outside starfix" | None beyond that |
| Cost per outcome | Absent | None |
| Choosing a cheaper model for a class of work | Absent | Bearings §3.9: "a label can require a provider or model", set by hand |

**The design puts enforcement in the wrong place.** Bearings §3.7.1 says "the numbers are the same whether work ran under Bearings or by hand". The *limits* are not: the governor enforces a budget only by declining to dispatch, so an agent a developer starts by hand, or one started by any other orchestrator, is never stopped. And the budget's value lives in `bearings.toml`, a file in the repository, so whoever can merge a change sets the spending limit. An enterprise wants the limit set by someone with authority, stored where the access rules are, and enforced by the authority every agent already passes through. That is `starfixd` at `start` (and at renewal, for a running session): the same place the autonomy window must be enforced, for the same reason the design gives there ("so a crashed or misbehaving `bearingsd` cannot extend it").

**Other weaknesses.**
- **Prices and plans are per project.** They are tables of each project's database, set by that project's admins. A server of 100 projects has 100 price tables to keep equal, and a plan that pays for one person's work in three projects is recorded three times and amortized three times over.
- **No report crosses projects,** so the enterprise's total is a spreadsheet someone assembles.
- **Counts are self-reported.** A principal can report false counts for itself. That is acceptable for showback and stated honestly; it rules out chargeback that anyone disputes, and it means a hard budget can be evaded by not reporting. A budget should therefore also be expressible in held time and in number of concurrent sessions, which the server measures itself.
- **Nothing connects cost to result.** Cost by issue and cost by model both exist. "What does a closed bug cost with this model against that one, and how often is it reopened" needs issue type, model and outcome in one query, and the outcome is not known (§3).

**Why it matters.** "Cost control" is in the goal by name. What exists is cost accounting, done well. Control is perhaps three weeks of work in `starfixd` if it is designed there, and it is the feature a budget holder will ask to see first.

## 6. Feedback and improvement recommendations

**What exists.** `digest`: eight lists and totals for a window, with held time, tokens, cost and hours. "Stalled" is the one judgment in it (in progress, no event for 48 hours). Similar closed issues at `create` and `show`, by title tokens. Memory, which carries what someone chose to write down. That is all, and none of it measures how work went or suggests anything. Bearings adds a conflict log (§3.4) so that one later decision, whether to build reservations, can be made on evidence; that instinct is right and is applied once.

**This is the thinnest area, and the data for a first version is already in the log.** A first, honest version computes rates, compares them with the same project's own past, and says what it saw. It runs no model, like `digest`, and leaves any narrative to the agent or person who asked.

| Measure | From events that exist today |
|---|---|
| Cycle time: create to close, first `claim.take` to close | `issue.create`, `claim.take`, `issue.close` |
| Rework: share of closed issues reopened, and how soon | `issue.reopen` after `issue.close` |
| Claims that lapsed: share of takes ending in `claim.expire` rather than a release, by harness | `claim.take`, `claim.expire`, `claim.release`; the registry's harness |
| Takeovers and overrides | `claim.take` with a before state; `admin.override` |
| Acceptance honesty: items waived, closes forced, issues closed with no acceptance text | `acceptance.waive`, `issue.close` with `acceptance_overridden` |
| Handoff quality: handoffs with no `next`, with state `blocked`; time from a handoff to the next take | `comment.add` of kind `handoff` and its fields |
| Discovery: discovered issues per closed issue; how many are ever closed | `dep.add` of type `discovered-from` |
| Collisions: issues taken while overlapping a held one | `issue.paths` and claims |
| Cost and time per closed issue, by type, label, harness and model; unattributed share | `token_usage`, attribution, `issue.close` |
| Queue health: age of ready work by priority; blocked for how long, by what | Issue state and `dep` events |

**From measurement to recommendation.** Keep the first rules few, explainable and falsifiable, each naming its evidence:
- "Issues labeled X are reopened three times as often as the rest; five examples: …" (rework by label).
- "40% of tokens this month were held by no issue" (agents working outside claims; fix the workflow or the pointer).
- "Claims from harness H lapse at four times the rate of others" (a setup or renewal problem).
- "Issues with no acceptance text cost twice as much and are reopened more" (ask for criteria at `create`).
- "Model A closes `bug` issues at a third of model B's cost with the same reopen rate" (a routing suggestion, the cheaper-model question of §5).

Each needs a minimum sample before it speaks, or it will mislead a small team. That is a design point, not a detail.

**What it needs that does not exist.** The outcome of the work (§3): without "merged" and "reverted", rework is only "reopened". A report across projects for the enterprise reading. And somewhere for a recommendation to go: an inbox item for a project admin, a line in `prime`, a pinned `team` memory proposed for a person to accept. The last closes the loop the goal describes: what was learned changes what the next session is told.

**Why it matters.** The goal's last clause is the product's reason to exist above a tracker: "continually refine and improve how work gets coordinated". Today no part of the product, built or designed, does this.

## 7. Operability for a platform team

**What is there.** A server guide that is unusually precise (each step ends with a check; no password on a command line; the Dolt account is verified least-privileged at every open). Three worked deployments with costs. Signed releases, `starfixd upgrade` with a database tag first, a health check and an automatic restore of the old binary. Protocol compatibility back to every released client, pinned by a test. `sfx admin status`. Honest notes on what was and was not run on a real machine.

**What is weak.**
- **One machine, by design.** "No high availability" is stated in every deployment. What an outage costs: agents keep editing code and cannot claim, finish, hand off, read memory or report usage. Inferred from the design, not tested here: an outage longer than the 15-minute agent lease ends every agent's claim when the server returns, because a store reaps at open before its first request; each agent must then `start` its issue again and may find it taken. Stage 5 (offline, provisional claims) would soften this and is two stages away.
- **Recovery is manual and unrehearsed.** Backup is a cron line; restore is five shell commands. No test restores a dump, and the upgrade's own tag "is not a copy outside Dolt". A dump holds current rows including the event log, not Dolt's commit history.
- **What HA would take on Dolt.** Inferred, not measured: a standby needs the database's own replication to a second machine, a way to move the daemon and the host key (every `.starfix.yaml` pins it), and fencing so that two daemons never both write, since the whole design rests on one writer per project. The server-side quotas and write statistics are "per daemon, in memory" and would restart from zero, which is harmless. [database.md](database.md) keeps a relational fallback "behind the same interface"; nothing exercises that interface with a second database, so it is a hope, not an option.
- **Scale is measured on small data on a laptop.** About 100 MiB of Dolt memory for a project of 300 issues; memory follows data written and is not given back; a memory limit halved it at four times the write latency. Not measured, by the spike's own list: Linux, a restart, concurrent projects, larger projects. An enterprise's first sizing question ("how big a machine for 200 projects of 5,000 issues") has no answer yet, and the event log and usage rows only grow.
- **Monitoring** is a log and a status command until §16 lands. §16 is thorough and, once built, would move this section's finding from weak to adequate.
- **Platforms.** Ubuntu LTS only; systems with SELinux unsupported (decision 23); private access is built for one cloud's tunnel and is "a VPN" elsewhere.
- **Deployment docs lag the code.** All three examples, the enterprise one included, describe a one-project server with hand-edited key lines ("delete their key lines" for offboarding). The many-projects path, which is what an enterprise would run, is opt-in and carries a warning.

**Multi-region.** Absent and not designed. One authority with one writer per project makes it a deliberate non-goal; the honest enterprise answer is a server per region or business unit, which then needs the cross-server reporting that does not exist even across projects.

**Why it matters.** A platform team will accept one small machine for a pilot. To run it for hundreds of people it needs a stated recovery time and recovery point, a restore it has seen work, sizing guidance from Linux, and alerts.

## 8. Bearings

**On its own terms the design is disciplined.** "Go supervises; LLMs judge", statelessness, events instead of polling, identity from the principal, a provider allowlist, pull requests with a human gate by default, and no LLM leading unattended work are the right lessons from the systems it cites. Each component names the failure it removes. The epic design (§3.13) is the most thought-through part: "landed, not closed, unblocks" is correct and is exactly the outcome signal §3 and §6 of this review find missing.

**What it assumes that stage 3a changed.** The design is dated before projects and access existed.
- **One project.** `bearings.toml` is "one TOML file per project", and a `bearingsd` is "one per machine" that "can share one starfix server". Nothing says how one daemon serves several projects, which membership it holds in each, or how per-developer caps apply across them.
- **Who a `bearingsd` is.** It needs a principal. On a laptop it is the developer (and so are its agents, §2). On a server it is unstated: a member of each project? A project admin, so it can release others' claims? Its rights decide what a compromised worker can do, and stage 3a's rules (a project admin's reach, nobody changes their own keys) now constrain the answer.
- **"A session-scoped credential that starfixd maps to (principal, session)"** (§3.12). Stage 3a made a key name a principal and nothing else, kept in a control store that only admins write, with the handshake refusing a key the store does not hold. A per-session credential is a new kind of thing in that model: who mints it, how it expires, how it is revoked, whether it is a principal. This is the same unanswered question as decision 8's "distinct agent identities" and as §15.6's certificates, and one design should answer all three. Short-lived certificates with the agent's identity in them would fit the mechanism §15.6 already tried.
- **Protocol versions.** Decision 6 releases Bearings under one tag with starfix "so it never ships against a protocol starfix does not speak". Since then the server accepts protocols 2 to 6 and a client may be older than the server. A `bearingsd` on a laptop will meet an older or newer `starfixd`; it needs the same handshake discipline as `sfx`, not an assumption of lockstep.
- **"Server-side semaphores"** for the governor (§3.7) are stage 6 locks. The plan table says B2 needs starfix stages "3–4".

**What is underspecified or risky.**
1. **Stage order.** Decision 13 builds B5 (a server running agents unattended) right after B1 and ahead of B2, which holds the governor and its budgets. B5's own text requires "a budget" on every autonomy window and circuit breakers in "the governor". As ordered, unattended work arrives before the thing that bounds its spend. If budgets move into `starfixd` (§5), the order can stand; otherwise it should not.
2. **Budget and window enforcement** are split: the window in `starfixd`, the budget in `bearingsd`. They are the same control and belong together, in the server.
3. **The merge train** (§3.5) batches, tests the tip, bisects on red and ejects culprits. That is a merge queue, which code hosting platforms provide and which is hard to build well. It also makes Bearings the only holder of push credentials, a high-value secret on a machine that runs agents.
4. **Ports and webhooks.** `bearingsd --http` serves a status page (§3.11) and gates open on "CI green" (§3.3). Who learns that CI is green, and how, is not said; starfix's design says webhooks, which its decisions do not allow (§3 of this review).
5. **Harness neutrality.** Typed state comes "from exit codes and the harness's structured output per provider" (§3.2), and capture "reads the harness's usage output" (§3.7.1). Both are per-vendor adapters, and the plan names three vendors' CLIs. Nothing defines the adapter's contract, so an open-source harness cannot be added without changing Bearings, and the allowlist names harnesses, not models.
6. **Judgment calls** "go through the same CLI in headless mode" (§3.7.2). What they may read and what bounds their cost is not said.
7. **Unattended credentials** (§3.14): provider API keys and a bot identity with short-lived tokens live on the server `bearingsd`. Where they are stored, who can read them, and how they stay out of an agent's environment is the core of the sandbox design and is one paragraph.
8. **The unattended parking rules** name `go.mod` and `go.sum` (decisions 18 and 19). They are this repository's rules written into a general tool; they should be a per-project list of protected paths.
9. **Dependencies and duplication.** `bearings board` is specified with a TUI library, while `sfx tui` exists, hand-written to avoid one (rule 4). Two boards over the same event stream is one too many.

**What I would cut or defer.**
- **The merge train's batching and bisecting.** Use the hosting platform's merge queue and required checks. Keep only what starfix needs: the `landed` event, recorded by whatever observes the merge.
- **The formula runner** (§3.10) until gates exist and a workflow has been run by hand often enough to know its shape.
- **The Gas City shim** (decision 4). It serves another tool's users, not the goal.
- **`bearings board` and the HTTP status page.** Extend `sfx tui` and the static report.
- **Reservations** stay deferred, as designed.

**What must be decided before any code.**
1. What an agent identity is, and how one is minted, scoped, expired and revoked (with decision 8 and §15.6).
2. Where budgets and autonomy windows live and are enforced (this review says `starfixd`).
3. How `starfixd` learns that work landed and that CI passed, without a public listener.
4. The harness adapter's contract, with a generic adapter for a harness the project has never seen.
5. How one `bearingsd` relates to many projects and what its principal may do in each.
6. Whether Bearings is needed for the goal's first version at all. The harnesses increasingly start and supervise their own sub-agents. The part nobody else provides is the part that needs no supervisor: shared claims, budgets, policy and the record. A thinner first Bearings, B0 and B1 as a per-machine supervisor that couples process liveness to leases and nothing else, would prove the idea at a fraction of the design.

## 9. Adoption

**A team of five, in an afternoon.** Close to true today. One small Linux VM following the server guide (an hour, most of it Dolt and sshd), one install line per person, `sfx setup --all --write` once per repository, and a key line per person. What slows the afternoon: Dolt is an unfamiliar database to install, pin and back up; only an Ubuntu LTS server is supported; the first `sfx cost` shows no cost until someone finds and types prices; and only one harness reports tokens. What helps: no account system, no browser, no port to open, nothing in the working tree, and errors that say what to do.

**An enterprise, to approve it.**

| They will ask for | Today |
|---|---|
| A security review of their own | Well prepared: a written security model, a recorded internal review, signed releases with provenance, static analysis and vulnerability scanning in CI, a small dependency list. No published disclosure policy (`SECURITY.md`), no software bill of materials |
| Single sign-on and lifecycle from the directory | No (§4) |
| An SLA, a recovery time, a support contact | None; pre-1.0 and says so; one maintainer is implied by the release process (inferred) |
| A migration path from their tracker | One predecessor tool only |
| A way for non-engineers to see it | No |
| Their operating system and cloud | One distribution; private access built for one cloud |
| A license | Apache-2.0: no obstacle, and a real advantage in procurement |
| Proof of scale | A ten-minute soak with 25 sessions on a laptop |

**Where the design choices help and where they will be the reason for "no".**

| Choice | Helps | Hurts |
|---|---|---|
| SSH as the only transport | No new port, no certificates to run, identity from infrastructure a team already has, a forced command that makes impersonation structural. Excellent for a team | Enterprise proxies and hosted agent sandboxes often allow only HTTPS (Bearings §3.14 says so itself). Single sign-on by OpenID Connect needs HTTPS (§15.6). Windows and locked-down laptops may not have an agent holding keys. Security teams have mature tooling for HTTPS traffic and little for SSH |
| Dolt | Versioned history for free, a real second record | A database almost no platform team has run: no in-house skills, no managed offering to buy (inferred), memory behavior that surprised its own measurement, full-text search measured unreliable, a pinned version that must move in step with starfix. The fallback is unexercised |
| No web interface | Small, auditable, no port | The single largest adoption barrier outside engineering |
| Terminal-first client | Fits agents and the engineers who run them | Fits nobody else named in the goal |
| One authority, one machine | Simple, correct, cheap ($19 to $114 a month) | No availability story |
| MCP for agents | The neutral standard; small schemas | None found |

None of the "hurts" entries argues for abandoning a choice. Each argues for a second option beside it, chosen deliberately: a second transport, a read-only view, a standby.

## What to do

Sizes are rough: **days** (under a week), **weeks** (one to four), **a stage** (comparable to stage 3a). "Recorded" means a decision or plan already says it; "New" means this review proposes it.

### Before calling this usable by a team

| # | Item | Solves | Size | Depends on | New or recorded |
|---|---|---|---|---|---|
| T1 | Run the many-projects setup under systemd and on at least one provider's VM; measure Dolt's memory on Linux across a restart | The README's own "not yet checked on real machines" | Days | Nothing | Recorded (Starfix §15.4, §15.5 item 2) |
| T2 | Observability slices O1 and O3: metrics, health, the log schema | A team cannot tell that the server is unhealthy before agents fail | Weeks | In flight | Recorded (§16) |
| T3 | A harness-neutral usage intake: `sfx usage` reading records from standard input or a file in the wire's own shape, documented so any harness, wrapper or gateway can report | "A variety of models" for cost; unblocks self-hosted models without a reader per vendor | Days | Nothing: the op exists | New |
| T4 | One session id for hook and MCP in every harness | Presence and audit split in two for the five harnesses that have a hook and set no session id in the environment | Days | A trust analysis the design already asks for | Recorded as a limitation; no decision |
| T5 | A first `review` report: the measures of §6 for one project, each against the project's own previous window, with a minimum sample | The goal's last clause has nothing behind it | Weeks | Nothing: the events exist | New |
| T6 | Text search on `list`, reusing the similar-issues tokenizer | A backlog past a few hundred issues cannot be searched | Days | Nothing | Recorded (Starfix §4) |
| T7 | A restore drill in the Docker smoke test: dump, wipe, restore, compare event counts | Backups nobody has restored | Days | Nothing | New |
| T8 | A price file an admin can load in one command, and a documented way to price a self-hosted model | The first cost report is empty; no price ships and none should be hard-coded | Days | Nothing | New |
| T9 | Set defaults from the stated requirement: raise `conns_per_principal`, and run the soak once at 100 sessions for one principal | The default refuses the 33rd session of a developer the requirement says may have hundreds | Days | Nothing | New |

### Before putting it in front of an enterprise

| # | Item | Solves | Size | Depends on | New or recorded |
|---|---|---|---|---|---|
| E1 | Decide and build identity federation: short-lived SSH certificates from the company's sign-on as a second way in, with membership able to follow a group | Manual keys that never expire; leavers; no second factor | A stage | A decision on §15.6 | Investigated, undecided |
| E2 | Design agent identities in starfix.md: an agent principal owned by a person or a service, with narrower default rights, visible in events and reports | An agent is its person; unattended work cannot be scoped | Weeks | E1's mechanism, ideally | Decided (decision 8), not designed |
| E3 | Budgets in `starfixd`: a budget on a project, account, epic or principal, in dollars, tokens, held time or concurrent sessions; refused or parked at `start`; an inbox item and a metric at thresholds | Cost control, for every agent however launched | Weeks | Prices at the server (E4) | New; replaces the enforcement point of Bearings §3.7 |
| E4 | Prices, plans and cost reports at the server: one price table, plans that span projects, `cost` across the projects a caller may see, grouping by harness and label | 100 price tables; no enterprise total | Weeks | The control store | New |
| E5 | An audit trail an auditor can take: a read of the event log by time and actor for project and control logs, an export, and tamper evidence (each event carrying a hash of its predecessor, with the head recorded somewhere the server cannot rewrite) | "Show us last quarter" and "prove it was not altered" | Weeks | O3 covers part of the access log | New; partly in flight |
| E6 | Roles beyond two: read-only, and prices-and-plans apart from overrides | Everyone reads everything; one admin role does all | Weeks | The control store | New |
| E7 | A report for people who do not use a terminal: `sfx` writes a static file per project and across projects from digest, cost and review | Managers, product owners and finance see nothing | Weeks | T5, E4 | New; needs no port and reverses no decision |
| E8 | Outcome signals: an op by which CI or a merge records "landed", "checks passed" or "reverted" against an issue, sent by a job running `sfx` | Closed is an assertion; rework and cost per outcome cannot be measured | Weeks | A decision on webhooks versus push from CI | Designed in part (Bearings §3.13 `landed`; Starfix §12 item 2) |
| E9 | The secrets lint on issue text, comments, handoffs and hours notes, as a warning or a refusal by setting | A credential pasted into a comment is kept forever | Days | Nothing | Recorded as a gap (Starfix §6) |
| E10 | A recovery statement and a standby: stated recovery time and point, a scripted rebuild, then a warm standby on the database's replication with fencing | No availability story | A stage | Linux measurements (T1) | New |
| E11 | Retention and erasure: opaque principal handles with a display name in the control store (§15.6, depth 2); a retention setting for usage rows | A person's name in every log forever | Weeks | A decision on §15.6 | Investigated, undecided |
| E12 | A load test at enterprise shape (hundreds of principals, thousands of connections, dozens of active projects) on Linux, with sizing guidance from it | No evidence past 25 sessions | Weeks | T1, T2 | New |
| E13 | Autonomy window designed in starfix.md beside E3: scope, budget, expiry, who may open one, what `start` checks | Unattended agents have no server-side bound | Weeks | E2, E3 | Decided (decision 8), not designed |
| E14 | A second transport, or a written reason there will not be one | Proxies, hosted sandboxes, sign-on by OpenID Connect | A stage | A product decision | Deferred (decisions 9, 20) |
| E15 | Adoption paperwork: a disclosure policy, a bill of materials with each release, a support statement, the mandatory-access-control profiles | Procurement checklists | Days to weeks | In flight in part | Partly recorded (decision 23) |
| E16 | Rewrite the enterprise deployment example for a many-projects server with keys from the store | The example describes the setup an enterprise would not use | Days | T1 | New |

### Later

| # | Item | Why later |
|---|---|---|
| L1 | Bearings B0 and B1, after the six decisions of §8 | The cheapest time to change it is before code |
| L2 | Gates and locks (stage 6), fed by E8 | Approvals and merge coordination need outcome signals first |
| L3 | Sync with other trackers (stage 7); import from them | Large; an enterprise pilot can start with one team's new work |
| L4 | A layer above the project (teams, portfolios), cross-project dependencies | Wait for the reports of E4 and E7 to show what grouping people ask for |
| L5 | Recommendations that route work (a cheaper model for a class of issue) | Needs T5, E8 and months of data |
| L6 | Offline cache and conflict parking (stage 5) | See below |
| L7 | A live web view | Only if E7's static report proves insufficient |

### Cut or stop

| Stop | Why |
|---|---|
| **Treating parity with the predecessor tool as the roadmap.** Starfix §4 ports every feature: nineteen dependency types, molecules, formulas, wisps, swarm, compaction | The goal has changed since §4 was written. None of these is named by the goal, and stage 6 and 7 are full of them while budgets, audit and feedback have no stage |
| **Stage 5 (offline cache, outbox, conflict parking) as the next stage.** The README says it "comes next" | It is the largest remaining stage, adds a second store and a merge model to a design whose first principle is one authority, and serves the case of a developer with no network, which agents calling hosted models rarely are. A standby (E10) addresses server outages more directly. Postpone it behind the enterprise band and revisit |
| **Adding harnesses to `sfx setup`.** Eight are covered | Each is file edits checked against documentation read on one day. T3 and a documented manual setup serve the long tail better than a ninth entry nobody can verify |
| **A second vendor-specific transcript reader before the generic intake** | The reader is the expensive, fragile way; build the neutral door first |
| **Vectors and embeddings (Starfix §10)** | No finding in this review is solved by them |
| **In Bearings:** the merge train's batching and bisecting, the formula runner, the Gas City shim, the second board and the HTTP status page | §8 |
| **B5 ahead of the governor** | Unattended work before budgets, unless E3 lands first |
| **Refining amortization further** | It is already more exact than its inputs, which are self-reported |

## Open questions for the maintainer

1. **Who is the buyer and who are the readers?** If "team and enterprise" includes managers, product owners and finance, the product needs a view for them; if it means engineering teams inside an enterprise, a static report may be all it ever needs. Decision 12 and the dropped web view assume the second.
2. **Is SSH-only a principle or a stage?** Decisions 9 and 20 say "for now". Identity federation by certificate (E1) keeps it; sign-on by OpenID Connect, hosted sandboxes and webhooks do not.
3. **Is Bearings part of the first product that meets the goal?** Or is the first product starfix with budgets, policy, audit and feedback, coordinating agents that harnesses and people start?
4. **What is an agent, as an identity?** One per harness process, one per role, one per pool? Who is accountable for it, and whose budget does it draw on?
5. **What should happen when a budget is spent?** Refuse the next `start`, park running work for a person, or only alert? Who may raise it? Does a subscription's amortized cost count against it, or only list price?
6. **What does a self-hosted model cost?** Zero, a plan whose fee is the hardware, or compute time at a rate? The answer decides whether "cheaper model" comparisons mean anything.
7. **What is the unit above a project?** One server per enterprise with projects as repositories, a team layer between, or several servers? This decides where cross-project reports, shared memory and shared prices live.
8. **Is the predecessor tool's feature list still a commitment** (§4, "You asked to port every feature"), now that the goal is stated differently?
9. **Must the audit trail satisfy a named standard?** That decides how much of E5 is needed and whether the database's own history can be called an audit trail at all.
10. **Is a person's name in the event log forever acceptable** to the first enterprise that would run this? If not, §15.6 depth 2 should be built before data accumulates under real names.
11. **Is the relational fallback still on the table?** If an enterprise's platform team refuses to run Dolt, is there an answer, and should the store interface be kept honest by a test against a second database?
12. **Who operates and supports it?** Self-hosted only, or also offered as a service? An SLA needs an answer to this before E10 is sized.

## What was not reviewed, and confidence

**Not reviewed.**
- **No test was run.** Tests are cited as evidence that a capability has one; they were located and in several cases read, not executed. CI's state on `main` was not checked.
- **The in-flight branches**: the observability slice, the protocol reset, the AppArmor profile, the contributor-setup rework and the security review. Where one would change a finding it is said (§4 audit, §7 monitoring, E15). The protocol reset could change every protocol number cited here.
- **Code read in part.** The store's write path, authorization, claims, digest, cost, the control store's rules, the server's limits and the agent setup table were read. `internal/board`, `internal/client`, `internal/release`, `internal/bdimport` and most of `internal/cli` were checked for what they offer, not reviewed line by line. This is a product review, not a code or security review.
- **Guides read in part.** Both design documents, the README, concepts, agents, security model, the command reference, the enterprise and team examples, the deployment overview, database.md and both spike results were read whole or nearly. In the server guide, the opt-in steps, "Keys from the store" and "Managing access" were taken from the design's As built notes rather than read in full; the install guide, the local and Docker examples, CONTRIBUTING.md and RELEASING.md were skimmed.
- **The running products.** No harness was started; setup's output was not checked against any live agent.
- **Anything on a real server.** No deployment was built.

**Confidence.**

| Section | Confidence | Why |
|---|---|---|
| 1. Organizing work | High | Absences were each searched for in code and designs |
| 2. Coordinating agents | High on what is built; medium on scale | The scale findings are arithmetic on defaults and one inference about processes, not a measurement |
| 3. Develop and maintain | High | The absence of any CI, review or merge knowledge is easy to verify |
| 4. Governance | High on mechanisms; medium on what a compliance team would ask | The question list is judgment, not a standard |
| 5. Cost control | High | Searched and read end to end |
| 6. Feedback | High that it is absent; medium on the proposed first version | The measures are checked against event names that exist; their usefulness is untested |
| 7. Operability | Medium | Outage behavior and what a standby needs are inferred; nothing was run |
| 8. Bearings | Medium-high | A design read against another design; the recommendations to cut are judgment |
| 9. Adoption | Medium | What enterprises ask for is general knowledge, stated as such |
