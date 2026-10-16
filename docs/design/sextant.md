# Sextant v0: the work agents do, measured

**Design, 16 October 2026, proposed.** It decides nothing and nothing in
it is built. Sextant is the name decision 80 reserved for observability:
metrics, monitoring, tracing, cost and usage reporting. This is its
first slice, to start after v1.0.0 (decision 90).

## 1. Why

`ariesd`'s telemetry (aries.md §16) describes the server's health:
requests, connections, writes, claims taken, sign-ins, the audit
stream. A team that runs agents asks other questions:

- What did the agents spend this week, by account, by model and by
  epic? Which issues got expensive?
- How much work closes, how long it takes from ready to closed, and
  where it waits: blocked, at a gate, or handed off and not taken?
- Is a person's plan paying for itself, and whose hours went where?

Aries already answers each of these, but only when someone asks:
`aries cost` (list price, amortized plans and hours, aries.md §12.1),
`digest` (closed, newly ready, due, tokens and cost in a window),
`ready`, `blocked`, gates and `who`. Nothing turns them into series over
time that a dashboard draws and an alert watches. That is what a team
pays for: governance and cost control of agent work (the product goal),
not a tracker's request latency.

§16 cannot simply grow to hold this. Its metrics carry no principal,
issue or other value a client chooses (rule 18's bound on series), and
its spans carry ids and codes, never text (rule 2). Work metrics need
accounts, models and, carefully, people. So Sextant is a separate
layer, with rules of its own.

## 2. What v0 is

**An exporter that reads Aries through its public API and writes
metrics.** It is a binary, `sextant`, that runs beside `ariesd` (or
anywhere that can reach it), signs in as a service principal, polls the
ops `aries` already sends (`cost`, `digest`, `ready`, `blocked`, `who`,
gates), and exports the results over OTLP and on a Prometheus endpoint.
A dashboard and alert rules in `deploy/observability` read them, and the
Docker test server's `observe` profile shows them.

Why this shape (recommendations 1 and 2, §6):

- **No change to `ariesd`, the store or the protocol for v0.** It works
  against a v1.0.0 server as shipped, so it can start the day 1.0 is
  tagged and cannot destabilize it.
- **Access is Aries's own.** It reads with a principal's rights, in the
  projects it is a member of, through the same checks as a person. It
  never touches the database (rule 5 keeps one writer; reading the
  database directly would bypass access as well).
- **`ariesd` stays small** (rule 4). The OpenTelemetry SDK is already
  `ariesd`'s alone; Sextant links its own and touches neither binary.
- **It is a product boundary.** A separate binary can be licensed,
  priced and released apart from Aries (question 1, §7).

What v0 does not do: store history of its own (the metrics backend
keeps it), invoice, change anything in Aries, read issue text, or
replace `aries cost` (which stays exact and on demand; Sextant samples).

## 3. The metrics

Every series is per project. Values are bounded as §16's are: a label's
values come from a fixed list or are capped, the rest summed into
`(other)`, as `cost` already does for groups.

| Metric | Kind | Labels | From |
|---|---|---|---|
| `sextant.cost.usd` | gauge, for the window | `project`, `account`, `model` | `cost --group-by account` and `model`, list price |
| `sextant.cost.amortized.usd` | gauge, for the window | `project`, `account` | `cost`, plans amortized |
| `sextant.tokens` | gauge, for the window | `project`, `model`, `kind` (input, output, cache_write, cache_read) | `cost --group-by model` |
| `sextant.hours` | gauge, for the window | `project`, `account` | `cost`'s logged hours |
| `sextant.cost.unpriced.tokens` | gauge | `project`, `model` | tokens with no price, so a missing price is visible, never $0 |
| `sextant.issues` | gauge | `project`, `status` (open, in_progress, blocked, closed), `type` | `list` counts |
| `sextant.issues.ready` | gauge | `project` | `ready` |
| `sextant.issues.closed` | counter | `project`, `type` | `digest`'s closed |
| `sextant.issue.cycle` | histogram, hours | `project`, `type` | an issue's time from its first claim to its close, from `digest`'s closed and `show` |
| `sextant.gates.waiting` | gauge | `project`, `kind` (human, ci) | gates not passed; the oldest's age as `sextant.gates.oldest.age` |
| `sextant.sessions.active` | gauge | `project` | `who` |
| `sextant.scrape.errors` | counter | `project`, `op`, `code` | its own failures, so a silent exporter is an alert |

- **The window.** Cost, tokens and hours are reported for a rolling
  window (24 hours by default, and the calendar month for plans), not
  as monotonic counters: `cost` is computed on read from prices that
  can change (§12.1), so a counter would drift when a price is set
  afterwards. A gauge over a window recomputes the truth each poll.
- **Accounts and models are capped**, 20 each by default (`(other)`
  beyond), since both are free text in Aries.
- **Polling** every 60 seconds by default, one project after another,
  each op within its result budget; a project's polls cost what one
  person running `aries cost` and `digest` costs.

## 4. People

The most valuable view, spend and hours by person or by agent session,
is the most sensitive, and §16's answer (decision 32) is the starting
point: a stand-in, never a name, unless the operator chooses otherwise.

- **Off by default.** No metric has a person label unless
  `people: pseudonym` or `people: name` is set.
- **`pseudonym`** gives `person` as the same `p_…` stand-in `ariesd`
  uses, so a trace and a metric agree; Sextant asks `ariesd` for it
  rather than holding the key (a new read op, so 1.x, recommendation 4).
  Until then, v0 offers only `name` or nothing.
- **Bounded**: people are capped like accounts (50 by default).
- **A stand-in hides a name, not a person**: the guide says so, as §16
  does.

## 5. Slices

| Slice | Delivers | Needs |
|---|---|---|
| X0 | `cmd/sextant`: config, sign-in as a service principal (N5) or by key, the poll loop, OTLP and Prometheus out, `sextant.scrape.errors`, limits | v1.0.0 |
| X1 | Cost, tokens, hours, unpriced tokens, by account and model | X0 |
| X2 | Flow: issues by status, ready, closed, gates waiting, sessions, cycle time | X0 |
| X3 | A dashboard and alert rules (spend over a budget per account, unpriced tokens, a gate older than a day, nothing closed in a week, the exporter failing), tested as §16.7's are, in the `observe` profile | X1, X2 |
| X4 | People, pseudonymous, once `ariesd` can give a stand-in to a principal with the right | a 1.x protocol change |
| X5 | Packaging: a release of its own, an installer line, a systemd unit and AppArmor profile, as `ariesd` has | X0 |

About three weeks of one engineer for X0 to X3.

## 6. Recommendations

1. **A separate binary over the public API**, not instruments in
   `ariesd` and not a reader of the database. *Cost:* polling is coarser
   than events (60 seconds), and v0 can measure only what the ops
   already return.
2. **Gauges over a window for money**, not counters, because cost is
   computed on read. *Cost:* a backend's `increase()` over cost is
   wrong; the dashboard uses the gauge as is, and the guide says why.
3. **No person label by default**, and pseudonyms only once `ariesd`
   can serve them. *Cost:* per-person spend waits for X4 unless an
   operator chooses names.
4. **New ops only after v1.0.0, each with rule 10's bump:** a stand-in
   for a principal, and later, if polling proves too coarse, an event
   feed (`events since`) so Sextant can follow the log instead of
   sampling it.
5. **Start after v1.0.0 ships**, as decision 90 orders, and develop it
   against the dogfood server, whose `observe` profile already runs the
   backend.

## 7. Questions for the maintainer

1. **License and repository.** Aries is AGPL-3.0-only with a CLA that
   lets Ariesworx LLC license it otherwise (decision 82). Is Sextant
   (a) in this repository under the AGPL, (b) in this repository under
   another license, or (c) in a repository of its own, licensed
   commercially? *Recommended:* (c), if Sextant is what is sold, so that
   Aries stays a complete open tool and the boundary between them is
   the public API, which is what v0 already uses.
2. **People.** Should per-person spend exist at all in v0, by name,
   before pseudonyms (X4)? *Recommended:* no.
3. **Hosted or self-hosted.** Is the product a binary a customer runs
   beside `ariesd`, a hosted service that reads many customers' servers,
   or both? v0 is the binary either way; a hosted service changes what
   leaves a customer's network, and needs its own design.
4. **Budgets.** Should a budget per account be data in Aries (so an
   agent can see it, and `start` could warn), or only an alert rule in
   Sextant? *Recommended:* an alert rule in v0; budgets in Aries are a
   separate 1.x design.
