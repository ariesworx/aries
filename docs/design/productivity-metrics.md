# Productivity metrics for agentic coding: what Aries measures, and what it could

**Research, 17 October 2026 (aries-a3fxwzfk).** It decides nothing and
changes no code. It surveys the metrics people use or propose for
software delivery and for coding agents, maps each to what Aries records,
weighs each for use and for how it misleads, and recommends a first set.
Three pieces of this ground are already filed and are not repeated here
except where they meet the rest: the work dashboard (aries-w5kyz3hj:
issues in work, tokens over time, start-to-close time), tool calls by
tool (aries-vvqvogrh, a design) and tokens by model and kind
(aries-fiuo35z4). [sextant.md](sextant.md) proposes an exporter for
work metrics after v1.0.0; §2 says how this research relates to it.

## 1. In short

- **Aries already holds most of what the delivery literature asks for**,
  because its event log records every start, claim, handoff, gate report,
  close and reopen with the store's time (rule 19), and since W1
  (aries.md §21.15) a close of an issue whose workflow requires `landed`
  means merged. What it lacks is not data but **derivations**: no metric
  or report turns those events into rates and durations.
- **The first set recommended (§6)** is five pieces of work beyond the
  three filed: counters of outcomes (closes by how they closed, reopens
  by why), the durations of the stages where work waits (queue, review
  and landing, gates), cost per landed issue with its rework, the live
  claims beside the ready queue (the idle time between waves), and, as a
  question added to the tool-usage design, human interventions counted
  from the harness's transcripts. And once aries-2qx4mlm2 records each
  session's brief, model and effort, outcomes compared by them (M36).
- **Out of reach**, and staying so: deployment and incident metrics
  (DORA's deploy frequency and time to restore need a deploy and an
  incident, which Aries never sees), code churn and duplication (git
  content), developer satisfaction (a survey), PR review comments (the
  host's), and "time saved" (self-report).
- **The main hazards** are those of any productivity measure, sharpened
  by agents: counts of items of unequal size, durations that reward
  splitting work, a held claim read as work, usage that no issue was
  held for, and agents that work under their person's key (decision 8),
  so that "who" is a person, a session or neither. §5 lists them; each
  metric's entry says which apply.

## 2. Where a metric can live in Aries

A metric here has one of three homes. Which one decides what it may
carry.

| Home | What it is | May carry | May not | Example |
|---|---|---|---|---|
| **ariesd's telemetry** (aries.md §16) | An instrument recorded by the store's `Observer` after a write commits, exported over OTLP and `/metrics` | Bounded attributes from fixed lists (`project`, `type`, a reason from a fixed list, `kind`, a priced `model`) and numbers | An issue id, a principal, a session, any text (rule 2; series bounded, rule 18's spirit) | `aries.usage.tokens`; the filed `aries.issue.cycle.duration` |
| **The store's reports, on read** | What `aries cost`, `digest`, `show` and `dates` compute from the log and tables when asked, with the reader's access | Per issue, per epic, per account, per person; dollars from prices on read (§12.1) | Nothing beyond the member's own access | `aries cost --group-by issue`; digest's unattributed usage |
| **Sextant** ([sextant.md](sextant.md), after v1.0.0) | An exporter that polls those reports and writes series | Accounts and models capped, people only by the operator's choice (decision 32) | Text; a price in a counter | `sextant.cost.usd` |

Two rules follow, and the recommendations keep both:

- **A rate or a duration across a project is telemetry; a value for one
  issue is a report.** Prometheus cannot hold an issue id as a label, and
  rule 2 forbids text. So a histogram says how long issues of type `bug`
  waited for landing this week; `show` and `cost` say how long this one
  did and what it cost.
- **Money is computed on read, never counted.** A price set later changes
  every past cost (aries.md §12.1; sextant.md recommendation 2), so
  telemetry counts tokens and the report prices them.

The filed work dashboard (aries-w5kyz3hj) puts cycle time and issue
counts in ariesd's telemetry, as the maintainer asked on 17 October
2026, rather than waiting for Sextant. This research follows it: the
recommended instruments are of the same kind, recorded at the commit of
the write that ends the interval, and Sextant can later add what needs
a principal or an account.

## 3. What Aries records

Every metric below is derived from these. "Store time" is the event's
`at`, which never goes backward (rule 19).

| Source | What it holds | Metrics it feeds |
|---|---|---|
| `events` (`issue.create`, `issue.update`, `issue.close`, `issue.reopen`) | Each change of an issue, with principal, session, machine and time; a close's `reason` (`landed` from the closing pass, free text otherwise) and `acceptance_overridden` or `checks_overridden` when forced; a reopen's `reason` (`check failed` from a revert) | Throughput, lead time, reopen and revert rates, forced closes |
| `events` (`claim.take`, `claim.release`, `claim.expire`, `claim.lapsed`, `claim.away`), `claims` | When a session held an issue, and how each hold ended | Cycle time, held time, queue time, abandoned work, live work in progress |
| `events` (`issue.handoff`), `handoffs` | Each handoff with its state (`done`, `partial`, `blocked`), next step, branch, recipient | Awaiting landing, blocked handoffs, handoffs per issue |
| `events` (`gate.add`, `gate.pass`, `gate.fail`, `gate.waive`, `gate.rearm`), `gates` | Human and ci gates, their checks, reports with commit, and who | First-pass rate of a check, gate wait, review wait |
| `acceptance_state`, `events` (`acceptance.tick`, `.waive`, `.untick`) | Items ticked, waived with a reason, unticked by a revert | Waiver rate |
| `deps` (`discovered-from`, `blocks`, …) | Discovered work and what blocks what | Discovered work, blocked time |
| `token_usage`, `prices`, `plans`, attribution (`store/attribution.go`) | Tokens by kind, model and harness per request or turn; issues a session held at the time, split by time; `(unattributed)` where none was held | Tokens and cost per issue, per landed issue, rework cost, unattributed share |
| `hours` (`aries log`) | A person's logged time on an issue | Human hours per issue |
| `conflicts`, `locks`, `lock_*` | Offline edits parked; lock queues | Coordination friction |
| `agents` (`who`) | Sessions seen and when | Active sessions |
| `internal/telemetry/instruments.go` | `aries.claims` {`event`, `reason`}, `aries.gates` {`kind`, `event`}, `aries.usage.tokens` {`project`, `kind`, `model`}, `aries.lock.wait.duration`, `aries.project.requests` {`result_class`}, and the server's own | The only work metrics exported today |
| `deploy/observability` | The server dashboard (claims, gates, tokens by model), requests, traces and logs | No flow or outcome panel yet |

What Aries does **not** record: pull requests, review comments and CI runs
on the host (only what a job or `aries land` reports as a gate); deploys
and incidents; the code itself, beyond the paths in `issue_paths`; what a
person typed to an agent; an issue's size or estimate (there is no such
field).

## 4. The metrics

Each entry gives its definition, where it comes from, its status in
Aries (**today**, **filed**, **small change** or **out of reach**), and
its caveats, among them how it is gamed or misread. The table is the
index; the entries follow by family.

| # | Metric | Family | Status | Worth it |
|---|---|---|---|---|
| M1 | Landing frequency (deployment frequency) | DORA | small change | high |
| M2 | Lead time for changes | DORA | partly today; deploy end out of reach | medium |
| M3 | Change failure rate (revert rate) | DORA | small change | high |
| M4 | Time to restore service | DORA | out of reach | — |
| M5 | Rework rate | DORA (2024 on) | small change | high |
| M6 | Cycle time | Flow | filed (aries-w5kyz3hj) | high |
| M7 | Lead time, created to closed | Flow | today in the log; small change | medium |
| M8 | Queue time, created to first start | Flow | small change | medium |
| M9 | Awaiting landing (review and merge wait) | Flow | small change | high |
| M10 | Gate wait | Flow | small change | high |
| M11 | Throughput | Flow, SPACE performance | partly filed; small change | high, with care |
| M12 | Work in progress | Flow | filed; small change for live claims | high |
| M13 | Flow efficiency | Flow | small change, in a report | medium |
| M14 | Idle time between waves | Agent operations | small change | high |
| M15 | Reopen rate | Quality | small change | high |
| M16 | First-pass rate of a check | Quality | today where CI reports per change; out of reach here | medium |
| M17 | Waiver and override rate | Governance | small change | high |
| M18 | Escaped defects | Quality | today only if linked by hand | low |
| M19 | Abandoned work | Quality | today, partly | medium |
| M20 | Code churn and duplication | Code quality | out of reach | — |
| M21 | Tokens and cost per issue | Cost | today | high |
| M22 | Cost per landed issue | Cost | small change | high |
| M23 | Rework cost | Cost | small change | medium |
| M24 | Unattributed usage | Cost | today | high |
| M25 | Tokens by model and kind; cache reads | Cost | filed (aries-fiuo35z4) | high |
| M26 | Human hours per issue | Cost | today, if logged | low |
| M27 | Human interventions | Agent operations | out of reach in Aries; small change in capture | high |
| M28 | Review load | SPACE communication | partly by proxy | medium |
| M29 | Coordination friction | SPACE communication | today, partly | low |
| M30 | Handoffs per issue, blocked handoffs | SPACE communication | today in the log | low |
| M31 | Satisfaction and well-being | SPACE | out of reach | — |
| M32 | Activity counts (commits, lines, PRs, tool calls) | SPACE activity | partly filed (aries-vvqvogrh) | low |
| M33 | Task horizon and autonomy | Agent capability | out of reach | — |
| M34 | Suggestions and lines accepted | Harness analytics | out of scope | — |
| M35 | Time saved | AI frameworks | out of reach | — |
| M36 | Outcomes by brief, model and effort | Agent operations | filed in part (aries-2qx4mlm2); then a small change, in a report | high, with care |

### 4.1 Delivery: DORA

DORA's four keys (deployment frequency, lead time for changes, change
failure rate, time to restore) measure throughput and stability of
delivery to production; its 2024 report added rework rate, unplanned
deployments made to fix production, and its 2025 report on AI-assisted
development found AI adoption now goes with higher throughput but still
with more instability. Aries sees merges, not deploys, so each key maps
at best to the merge.

**M1 Landing frequency.**
*Definition:* issues closed as landed per day, by project and type; the
nearest Aries has to deployment frequency, and equal to it where the
default branch deploys continuously.
*Sources:* `issue.close` events with `reason: landed` (W1's closing
pass). `aries.gates{kind="ci",event="passed"}` counts every ci pass,
`landed` and other checks together, and cannot tell a closing pass.
*Status:* small change: a counter of closes by reason (§6, F1).
*Caveats:* only issues whose workflow requires `landed` close this way;
an issue closed by hand, or of a type with no required check, is not
counted, so compare the landed share of closes with it (M17). Splitting
an issue in two doubles it. Merge is not release.

**M2 Lead time for changes.**
*Definition:* in DORA, from a change's first commit to its running in
production.
*Sources:* Aries knows the issue's first start (`claim.take`) and its
landing (the closing pass's time), which is the cycle time (M6). The
first commit's time is in git, read by `aries land` but not sent; the
deploy end is not in Aries.
*Status:* the start-to-landed half is filed as M6; the deploy end is out
of reach.
*Caveats:* as M6.

**M3 Change failure rate (revert rate).**
*Definition:* the share of landed issues later reopened because their
`landed` check failed, that is, reverted.
*Sources:* `issue.reopen` with `reason: check failed` over M1's landed
closes in the same window.
*Status:* small change: the reopen counter by reason (F1).
*Caveats:* a revert counts only when someone reports `landed` failed
(`aries gate fail`, or a CI job's report); a fix-forward PR under a new
issue is not a revert and is not counted here, which is M5's ground.
Low counts make the ratio noisy for a small project; show the counts
beside the ratio.

**M4 Time to restore service.**
*Status:* out of reach: Aries records no incident and no deploy. An
operator who files incidents as bugs with a label could read their
create-to-landed time from M7, but nothing in Aries says an issue is an
incident, and adding that is outside a tracker for agents.

**M5 Rework rate.**
*Definition:* the share of closed issues that needed more work after
closing: reopened for any reason, or followed by a bug linked to them.
*Sources:* `issue.reopen` events of any reason (M15), and `deps` of type
`discovered-from` from a bug to a closed issue (M18).
*Status:* the reopen half is a small change (F1); the linked-bug half
depends on people linking (M18).
*Caveats:* DORA's rework is about unplanned deployments, which Aries
cannot see; this is the issue-level analogue and should be named so. A
team that reopens rather than filing anew raises it; one that files anew
hides it.

### 4.2 Flow

The flow metrics (cycle and lead time, throughput or flow velocity, work
in progress or flow load, flow efficiency, and flow distribution by
type) come from lean practice and are tied by Little's law: average time
in the system equals average work in progress over average throughput.
Aries's log gives each directly.

**M6 Cycle time.**
*Definition:* from an issue's first start to its close.
*Status:* filed, aries-w5kyz3hj (`aries.issue.cycle.duration`, by
`project` and `type`).
*Caveats:* a short cycle time rewards splitting work and starting late;
read it by type, with throughput. Since W1 the close of a governed issue
is its landing, so cycle time includes review and merge wait (M9), which
for this project was over a third of the elapsed time on 16 October
(aries-5llpbo64's baseline: 37% waiting for a merge decision). Report
the median and 90th percentile, not the mean, which one stale issue
moves.

**M7 Lead time, created to closed.**
*Definition:* from `created_at` to `closed_at`.
*Sources:* the `issues` row, and the create and close events.
*Status:* today in the store; a histogram is a small change, but it is
mostly backlog age, so it is a report, not an instrument (not in the
first set).
*Caveats:* dominated by how long an issue sat before anyone wanted it;
priority and deferral (`defer_until`) are choices, not delays.

**M8 Queue time, created to first start.**
*Definition:* from an issue's create, or its `defer_until` if later, to
its first `claim.take`.
*Sources:* events; `ready` is derived and never stored (rule 7), so the
moment an issue became ready (its last blocker closed) is computed from
its `blocks` deps' close events, and is an approximation when deps
changed.
*Status:* small change (F2), recorded at the first start.
*Caveats:* for a team with a coordinator, high queue time with free
slots is idle time (M14); with no free slots it is a full queue, which is
a different finding. Read beside M12.

**M9 Awaiting landing.**
*Definition:* from the handoff with state `done` to the close as landed:
the time a finished change waits for review, CI and merge.
*Sources:* the `issue.handoff` event and W1's closing pass; `dates` and
`digest` already list issues awaiting landing now (`Landing`, §21.15),
longest first.
*Status:* the current list is today; the duration of each completed wait
is a small change (F2), recorded at the closing pass.
*Caveats:* the clock starts when the agent says done, which it may say
early; a `done` handoff followed by more work restarts nothing. On a
server CI cannot reach, it ends when a person runs `aries land`, so it
includes that person's delay (§21.8), which is the point.

**M10 Gate wait.**
*Definition:* from a gate's add (or rearm) to its pass or waiver, by kind
(`human`, `ci`).
*Sources:* gate events.
*Status:* small change (F2). `aries.gates` counts gate changes but not
their durations.
*Caveats:* a gate made by a first report (§21.5) starts at that report,
not at the issue's start. Human gates are the measurable part of review
load (M28) until W3's review profile exists.

**M11 Throughput.**
*Definition:* issues closed per day or week, by type.
*Sources:* `issue.close` events.
*Status:* aries-w5kyz3hj's dashboard has "closes per day", from the
cycle histogram's count, which misses an issue closed without a start;
the counter by reason (F1) counts every close.
*Caveats:* the most gamed metric there is: items differ in size, Aries
has no size, and agents file issues cheaply. Never compare people or
models by it. Read it by type, beside cycle time and the landed share,
and as a trend for one project.

**M12 Work in progress.**
*Definition:* issues in progress at a moment; and, apart, live claims,
the issues a session holds now.
*Sources:* `issues.status`, and `claims` not expired.
*Status:* issues by status is filed (aries-w5kyz3hj's `aries.issues`).
Live claims are a small change (F4): since W1, an issue awaiting landing
is `in_progress` with no claim, so status alone overstates the work being
done.
*Caveats:* a claim held while its session waits on a background run, or
sits idle in an app, reads as work; claims expire on their lease, so a
dead session's claim stays live until the reaper ends it.

**M13 Flow efficiency.**
*Definition:* held time over cycle time: the share of an issue's life
someone was working on it.
*Sources:* the time held is already computed for usage attribution and
shown by `show` ("held 3m"); cycle time from M6.
*Status:* small change, as a field of the report F3 builds; not an
instrument, since it is per issue.
*Caveats:* held is not working (M12's caveat); an agent that keeps its
claim through review raises it falsely, and the closing pass's release
makes the honest path the default.

**M14 Idle time between waves.**
*Definition:* time with fewer live claims than the slots a coordinator
runs (four, by the memory `dispatch`) while issues are ready; total
hours by day.
*Sources:* live claims over time (M12) and the ready count over time.
aries-5llpbo64 built this by hand for 16 October from GitHub and the log,
and found most idle time was waiting for merge or for the next wave.
*Status:* small change (F4): a gauge of live claims and one of ready
issues, so a dashboard draws both against the slot count. The slot
count is the coordinator's setting, not Aries's, and is drawn as a line.
*Caveats:* the ready query is a recursive CTE; a gauge read at every
scrape costs one per open project, which F4 must measure or bound. Idle
by design (a person reviewing) is not waste; M9 and M10 say which wait it
was.

### 4.3 Quality and outcomes

**M15 Reopen rate.**
*Definition:* reopens over closes in a window, by reason: a revert
(`check failed`) or a person's or agent's reopen.
*Sources:* `issue.reopen` events.
*Status:* small change (F1).
*Caveats:* discouraging reopens makes people file anew instead; read it
with M18. A reopen to add one forgotten line is not a defect.

**M16 First-pass rate of a check.**
*Definition:* the share of issues whose first report of a required check
was a pass.
*Sources:* `gate.pass` and `gate.fail` events per subject and check.
*Status:* where CI reports a check per change (a `build` check from the
PR's job, §20.4), today in the log, and a small change in a report.
In this project it is out of reach: CI cannot reach the dogfood server,
`build` is no required check (§21.15, W0), and `landed` is reported after
merge, so its first report is nearly always a pass and the rate means
nothing. The host's CI history holds the real first-pass rate.
*Caveats:* agents that run the gate locally before pushing raise it,
which is the behavior wanted; one that disables a test raises it too.
Not in the first set until a project reports per-change checks.

**M17 Waiver and override rate.**
*Definition:* acceptance items waived, and closes forced past items or
checks (`close --force`), over closes.
*Sources:* `acceptance.waive` events with their reasons;
`acceptance_overridden` and `checks_overridden` on `issue.close`.
*Status:* small change: the close counter's reason (F1) separates forced
closes; waivers are counted in the report (F3).
*Caveats:* a governance signal, not a quality score: a waiver with a good
reason is the system working. A rising rate says the workflow's defaults
do not fit the work, or that they are being dodged; the reasons say
which.

**M18 Escaped defects.**
*Definition:* bugs filed against work already closed.
*Sources:* a `discovered-from` dep from the bug to the closed issue.
*Status:* today, only when someone links them; `finish` links the work it
discovers, but a bug found later is linked only by hand.
*Caveats:* undercounts by however often people do not link. Low value
until linking is habitual; not in the first set.

**M19 Abandoned work.**
*Definition:* claims that ended by expiry or lapse rather than release or
close; and handoffs `done` that never land (M9's open list, older than a
threshold), the analogue of a pull request not merged within 30 days.
*Sources:* `aries.claims{event="expire"}` today; `claim.lapsed` events;
`Landing` in `dates` and `digest`.
*Status:* today, partly: the counter and the list exist, a rate does not.
*Caveats:* a session closed by its person ends its claims by expiry; an
expired claim is not lost work if the next session picked it up.

**M20 Code churn and duplication.**
*Definition:* lines rewritten or reverted within a short time of being
written, and duplicated blocks; studies of AI-assisted code report both
rising.
*Status:* out of reach: they are analyses of the code's content, which
Aries does not read (it keeps paths, and `gitx` reads commit messages).
A git analysis tool beside Aries can join its results to issues by the
`Aries:` trailer.

### 4.4 Cost and efficiency

**M21 Tokens and cost per issue.**
*Definition:* tokens by kind and their list cost attributed to an issue.
*Sources:* `token_usage` and attribution, priced on read; `aries cost
--group-by issue|epic|account|model|person`, `show`'s usage line,
`digest`.
*Status:* today.
*Caveats:* attribution splits a session's usage evenly across the issues
it held at the time and by time held for turn and session records, so a
coordinator holding many issues smears its cost; usage while nothing was
held is unattributed (M24). With no prices set, cost reads zero and
"unpriced", which must never be read as free. Tokens alone are a vanity
measure: a cheaper model that needs three tries costs more per landed
change (M22).

**M22 Cost per landed issue.**
*Definition:* the cost attributed to issues that closed as landed in the
window, over their count, by type; with the cost of issues closed
otherwise and of issues still open shown beside it.
*Sources:* M21's attribution joined to M1's closes.
*Status:* small change (F3), in the cost report.
*Caveats:* the denominator is items of unequal size (M11). An issue's
cost is all its sessions', including failed tries, which is the honest
figure. Comparing models by it needs the same kinds of issue on each, and
months of data (enterprise review L5).

**M23 Rework cost.**
*Definition:* the cost attributed to an issue after its first close:
what reopens and reverts cost.
*Sources:* attribution split at the issue's first `issue.close`.
*Status:* small change (F3).
*Caveats:* as M15.

**M24 Unattributed usage.**
*Definition:* usage, and its cost, while the session held no issue, as a
share of all usage.
*Sources:* attribution's `(unattributed)`; `digest`'s usage line.
*Status:* today.
*Caveats:* it is the error bar on every per-issue cost: aries-hkwmq25r
found most of a coordinator's usage unattributed on 16 October. A high
share says sessions work without starting an issue (planning, review,
coordination), which may be correct; it should be read as overhead, not
dropped.

**M25 Tokens by model and kind; cache reads.**
*Status:* filed, aries-fiuo35z4 (tokens by model and kind, the one-hour
cache write its own kind). The cache read share, cache reads over all
input-side tokens, is a derived panel of those series and needs nothing
more.
*Caveats:* a high cache read share lowers cost per token, and a long,
bloated context raises it too; it is an efficiency signal, not a goal.

**M26 Human hours per issue.**
*Definition:* time people logged on an issue.
*Sources:* `hours` (`aries log`), in `aries cost`.
*Status:* today, if people log.
*Caveats:* self-reported and rarely logged; never use it for the "hours
saved" of M35.

### 4.5 People and collaboration: SPACE

SPACE (satisfaction and well-being, performance, activity, communication
and collaboration, efficiency and flow) argues that productivity has many
dimensions, no single metric captures it, and a chosen few should span
them at the individual, team and system levels. Performance and
efficiency are M1 to M17 above. The rest:

**M27 Human interventions.**
*Definition:* the times a person steered an agent on an issue: a prompt
after the first, an interruption, a correction, an answer to the agent's
question, a permission granted; per issue and per session. Published
measurements of agent autonomy count interventions per session and the
share of tasks finished without one.
*Sources:* the harness's transcripts, which `internal/capture` already
reads for usage, hold each user turn and interruption; they never leave
the machine beyond counts (security-model.md, "Transcripts stay on your
machine"). In Aries, the acts a person makes in person (session `cli`) on
an issue an agent held, such as a gate pass, a waiver, a forced close or
a reopen, are a partial proxy.
*Status:* out of reach in the store today; a small change in capture, of
the same shape as tool counts (aries-vvqvogrh), so it is added to that
design as a question rather than filed apart (F5).
*Caveats:* fewer interventions is not better by itself: experienced
users approve more automatically and interrupt more, and an agent left
alone on the wrong path is worse than one corrected. Read it with
outcomes (M3, M15). Counts only: never a prompt's text (rule 2).

**M28 Review load.**
*Definition:* the review work agents' output asks of people: changes
awaiting review, their age, and review comments per change.
*Sources:* review comments are on the host and out of reach. In Aries,
M9 (awaiting landing: count and age) and M10 for human gates, and with
W3's review profile, review gates per approver.
*Status:* partly, by proxy; F2 adds the durations.
*Caveats:* this is where agent productivity is spent: the person who
merges is the bottleneck, and the dashboard should show it so. Per
approver it is a person metric (§5).

**M29 Coordination friction.**
*Definition:* lock waits, offline edits parked as conflicts, claims
refused because another holds the issue.
*Sources:* `aries.lock.wait.duration` and `aries.locks` today; the
`conflicts` table (no instrument); refusals by code in
`aries.request.duration`'s `result`.
*Status:* today, partly; a conflicts counter is a small change not
recommended now.
*Caveats:* low numbers may mean little parallel work, not good
coordination.

**M30 Handoffs per issue, blocked handoffs.**
*Sources:* `issue.handoff` events and `handoffs.state`.
*Status:* today in the log; not exported.
*Caveats:* many handoffs may be a long issue worked well across sessions;
a `blocked` handoff is the useful signal, and `digest` already lists it.

**M31 Satisfaction and well-being.**
*Status:* out of reach: a survey of people, which a tracker should not
run. Agents have no satisfaction to measure; their people do.

**M32 Activity counts.**
*Definition:* commits, lines, PRs opened, tool calls, requests.
*Status:* tool calls are filed (aries-vvqvogrh); requests by op are
today (`aries.project.requests`, `aries.request.duration`); commits and
lines are out of reach and not wanted.
*Caveats:* SPACE's activity dimension, and the easiest to inflate: an
agent produces activity at no cost. Useful only to explain cost and
behavior (which tools an expensive issue used), never as output.

### 4.6 Agent capability, and the vendors' measures

**M33 Task horizon and autonomy.** Research measures the length of task,
in human time, an agent finishes at a given success rate, and finds it
doubling every several months. *Status:* out of reach: Aries has no
estimate of an issue's human time and should not invent one. A proxy is
cycle time by type of issues closed as landed with no in-person act
(M27), which says how much of the work agents now carry alone.

**M34 Suggestions and lines accepted.** Harness analytics report edits
accepted, lines accepted and the share of merged pull requests with
agent-written code. *Status:* out of scope: the harness's own, read from
its dashboards; Aries adds nothing by copying them. The share of merged
PRs is closer to M1 for a project where every issue is agents' work.

**M35 Time saved.** AI measurement frameworks lead with time saved per
developer per week, or hours of human-equivalent work agents did.
*Status:* out of reach: it is self-reported, and a randomized trial in
2025 found experienced developers using AI tools took 19% longer while
believing they were 20% faster. Aries should not offer a number it
cannot measure.

### 4.7 Briefs and their outcomes

**M36 Outcomes by brief, model and effort.**
*Definition:* for each session that started an issue, the brief it was
started with, the model and effort it ran on, set against what followed:
whether the issue landed, was reverted or reopened (M3, M15), the
first-pass rate of its checks where CI reports them (M16), its tokens
and cost (M21), its time held and cycle time (M6, M13), and the
interventions it needed (M27). Read across many sessions, it says which
briefs, and which model and effort for which type of issue, lead to work
that lands at the least cost, so that briefs can be refined and shared.
*Sources:* the brief, model and effort are to be recorded on the issue,
as an event joined to the claim's session id, by aries-2qx4mlm2. Today a
brief lives only in the harness's local transcript, which is deleted
after a time, and the model only on the usage records. Every outcome
above is joined to the session through its `claim.take` and attribution.
*Status:* the recording is filed (aries-2qx4mlm2), not filed again here;
once it lands, the comparison is a small change in a report: F3's cost by
outcome grouped by the starting session's model and effort, and `show`
and `history` putting each brief beside its issue's outcome. It belongs
in a report, never in telemetry: a brief is text (rule 2), and model and
effort may be attributes only if bounded as `aries.usage.tokens` bounds
models by prices.
*Caveats:* confounded by difficulty: whoever chose the model and effort
chose them for the issue, so a strong model given the hard issues looks
costly and a weak one given the easy ones looks good; compare within a
type, and treat it as evidence for a person to read, not a ranking.
Samples are small: one brief is one issue. An issue worked by several
sessions has several briefs, and its outcome is shared among them. A
brief tuned to the measure (short briefs for cheap issues) improves the
number, not the work.

## 5. Cautions that apply to all

1. **A measure that becomes a target stops measuring.** Agents optimize
   what they are scored on faster than people do. No metric here belongs
   in an agent's prompt or `prime` as a goal; they are for the people who
   run the agents.
2. **Items differ in size.** Throughput (M11), cost per landed issue (M22)
   and every rate over closes count items, and Aries has no size. Read
   them by type, as trends for one project, never across projects,
   people or models without the same kind of work.
3. **Durations reward splitting.** Cycle time halves when an issue is cut
   in two. Pair each duration with throughput and the landed share.
4. **Held is not working.** A claim stays live while its session waits on
   CI, idles in an app or is dead until its lease runs out (M12, M13,
   M14).
5. **Who is ambiguous.** Until agents are principals of their own
   (decision 8; enterprise review E2), an agent works under its person's
   key, so per-principal metrics merge a person with their agents, and
   sessions are client-chosen ids. Per-person metrics stay off by
   default, as sextant.md §4 and decision 32 have it; a stand-in hides a
   name, not a person.
6. **Unattributed usage is overhead, not noise** (M24): per-issue cost
   leaves it out, and the dashboard shows it beside.
7. **Unpriced is not free** (M21): every cost panel says when a model has
   no price.
8. **No text in telemetry** (rule 2): every attribute recommended here is
   from a fixed list; reasons that are free text in the store (a close's
   `reason`) are mapped to fixed values, everything else `other`.
9. **Small numbers.** A project closes tens of issues a week; a rate of
   reverts over a week is a handful of events. Show counts beside every
   ratio.

## 6. The recommended first set

Beyond the three filed (aries-w5kyz3hj, aries-vvqvogrh, aries-fiuo35z4),
these answer the questions a person running agents asks most: is work
landing, where does it wait, what does a landed change cost, and are the
slots full. Each is filed under the v0.6.0 epic (aries-cwloautj) with its
acceptance items.

| | Work | Metrics | Home | Issue |
|---|---|---|---|---|
| F1 | Outcome counters: `aries.issues.closed` {`project`, `type`, `reason`: `landed`, `forced`, `other`} and `aries.issues.reopened` {`project`, `type`, `reason`: `check_failed`, `other`}, recorded through the `Observer` after the commit; panels for landing frequency, revert and reopen rates and the forced share in the work dashboard | M1, M3, M5, M11, M15, M17 | telemetry | aries-bxhvim3i |
| F2 | Stage durations: histograms `aries.issue.queue.duration` (create or defer to first start, recorded at the first start), `aries.issue.landing.duration` (`done` handoff to the closing pass) by `project` and `type`, and `aries.gate.wait.duration` by `kind` (add or rearm to pass or waiver) | M8, M9, M10, M28 | telemetry | aries-6ypz4szb |
| F3 | Cost per outcome: `aries cost` groups an issue's cost by how it ended (landed, closed otherwise, open, unattributed) with count and cost per landed issue, the part spent after a first close (rework), held time over cycle time, and waivers, by type | M13, M17, M22, M23, M24 | report | aries-sbikbf3g |
| F4 | Live work against the queue: gauges `aries.claims.held` (live claims) and `aries.issues.ready` by `project`, the second at a measured and bounded cost per scrape; a panel of both against a slot count drawn as a constant | M12, M14 | telemetry | aries-cggv4jom |
| F5 | Human interventions: counts of user turns, interruptions and permission answers per session from the transcripts capture reads, added to the tool-usage design as a question | M27 | capture, report | comment on aries-vvqvogrh |
| F6 | Outcomes by brief, model and effort: the brief, model and effort recorded on the issue a session starts; then F3's report grouped by them | M36 | store, report | aries-2qx4mlm2 (filed apart), then F3 |

Order: F1 and F2 after aries-w5kyz3hj, which makes the dashboard and the
first close-time instrument they follow; F3 needs no telemetry and can go
first; F4 measures before it builds.

**Protocol.** F1, F2 and F4 are telemetry: no op and no argument. F3 adds
result fields to `cost`, which needs no bump, but if it takes a new
`group_by` value, an older server refuses it as invalid, which rule 10
treats as a change of the protocol after v0.5.0; F3 says which it chose.

**Names.** Every new instrument, attribute and value is recorded in
`internal/telemetry/testdata/names.golden` and in docs/server.md's "What
is measured", and kept from then on (rule 2).

## 7. Out of reach, and why

| Metric | Why |
|---|---|
| Deployment frequency, time to restore (M1's deploy end, M4) | Aries sees merges reported as `landed`, never a deploy or an incident |
| Lead time from first commit (M2) | The first commit's time is in git; `aries land` reads commits but sends only the landing |
| PR review comments, PR acceptance (M28, M19) | On the host; Aries has the handoff and the landing, not the pull request |
| Code churn, duplication (M20) | Content of the code, which Aries does not read |
| Satisfaction (M31) | A survey of people |
| Task horizon (M33), time saved (M35) | Need an estimate of human time per task, which Aries has not and should not guess |
| Suggestions and lines accepted (M34) | The harness's own analytics |

## 8. Questions for the maintainer

1. **Where these live.** aries-w5kyz3hj put work metrics in ariesd's
   telemetry; sextant.md recommended a separate exporter after v1.0.0.
   This research follows the first for bounded, project-level series
   (F1, F2, F4) and keeps per-issue values in reports (F3). Is that the
   line? *Recommended:* yes; Sextant then adds accounts and people.
2. **Close reasons as values.** F1 maps a close's free-text reason to
   `landed`, `forced` or `other`. Should a reason of a fixed list become
   part of `close` (as `landed` and `check failed` already are), so that
   `other` can be split later? *Recommended:* not now.
3. **Human interventions.** Should capture count user turns and
   interruptions from transcripts (F5), counts only, under the same
   privacy line as tool calls? *Recommended:* yes, in the same design and
   protocol change.

## References

- DORA, *Accelerate State of DevOps Report 2024* (rework rate as a fifth
  metric) and *State of AI-assisted Software Development 2025*,
  [dora.dev](https://dora.dev/research/).
- *The SPACE of Developer Productivity*, ACM Queue, 2021,
  [queue.acm.org](https://queue.acm.org/detail.cfm?id=3454124).
- The Flow Framework's flow metrics (velocity, time, load, efficiency,
  distribution), as summarized in
  [Planview's guide](https://blog.planview.com/flow-load/).
- METR, *Measuring the Impact of Early-2025 AI on Experienced
  Open-Source Developer Productivity*, 2025, and *Measuring AI Ability to
  Complete Long Tasks*, 2025,
  [arxiv.org/abs/2503.14499](https://arxiv.org/abs/2503.14499).
- Anthropic, *Measuring agent autonomy* (interventions per session),
  [anthropic.com](https://www.anthropic.com/research/measuring-agent-autonomy).
- Claude Code's analytics (accept rate, lines accepted, PRs with agent
  code), [code.claude.com](https://code.claude.com/docs/en/analytics).
- GitClear, *AI Copilot Code Quality 2025* (churn and duplication),
  [gitclear.com](https://www.gitclear.com/ai_assistant_code_quality_2025_research).
- DX, the Core 4 and the AI Measurement Framework (utilization, impact,
  cost), [getdx.com](https://getdx.com/research/measuring-ai-code-assistants-and-agents/).
- Studies of agents' pull requests and their acceptance rates by agent
  and task, for example
  [arxiv.org/abs/2602.02345](https://arxiv.org/abs/2602.02345v1).
