# Sextant v0: the work agents do, measured

**Design, 16 October 2026; X0 built, 19 October 2026.** Sextant is the
name decision 80 reserved for observability: metrics, monitoring,
tracing, cost and usage reporting. This is its first slice. The
maintainer answered §7 on 19 October 2026 (decision 98), and decided
the same day that it starts before v1.0.0 (decision 100), not after it
as decision 90 had it. X0 is built (§5, As built); X1 to X5 are not.
[docs/sextant.md](../sextant.md) is the guide to running it.

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
  against a released server as shipped (v0.5.0 on), so it cannot
  destabilize it, and it need not wait for v1.0.0 (decision 100).
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
| X0 | `cmd/sextant`: config, sign-in as a service principal (N5) or by key, the poll loop, OTLP and Prometheus out, `sextant.scrape.errors`, limits | Nothing: it starts before v1.0.0 (decision 100). Built |
| X1 | Cost, tokens, hours, unpriced tokens, by account and model | X0 |
| X2 | Flow: issues by status, ready, closed, gates waiting, sessions, cycle time | X0 |
| X3 | A dashboard and alert rules (spend over a budget per account, unpriced tokens, a gate older than a day, nothing closed in a week, the exporter failing), tested as §16.7's are, in the `observe` profile | X1, X2 |
| X4 | People, pseudonymous, once `ariesd` can give a stand-in to a principal with the right | a 1.x protocol change |
| X5 | Packaging: a release of its own, an installer line, a systemd unit and AppArmor profile, as `ariesd` has | X0 |

About three weeks of one engineer for X0 to X3.

**As built (X0, 19 Oct 2026).** `cmd/sextant` and `internal/sextant`
(the config, the poller and the run loop), with its exporters in
`internal/telemetry` (`sextant.go`). Where the build fills in or
departs from §2 and §3:

- **Its exporters are `internal/telemetry`'s**, not an SDK of its own:
  a meter provider of Sextant's, apart from ariesd's and holding none of
  its instruments, built from the same OTLP and Prometheus code, the
  same `OTEL_*` variables (with `service.name` `sextant`) and the same
  loopback listener. So the package stays the only one that imports
  the SDK (decision 28), and Sextant links a subset of what `ariesd`
  links, with no module of its own: `internal/depcheck` records its
  link set (`links-sextant.golden`), holds it to `ariesd`'s scope in
  `deps.allow`, and fails if `aries` links any package of Sextant's or
  `internal/telemetry`.
- **No new op.** A poll sends `prices` and `cost`, by model, for the
  window, with `limit` at the model cap: the ops `aries` sends, with a
  member's rights. A sign-in as a service principal is `aries login
  --workload`'s, kept in Sextant's `data_dir` and renewed two minutes
  before it ends; a key is the only one Sextant offers, never
  ssh-agent's or a home's.
- **X0 exports one figure of cost**, `sextant.cost.usd` by `project`
  and `model`, so that a poll is tested end to end against a real
  daemon (`internal/e2e`, `sextant_test.go`), with
  `sextant.scrape.errors` by `project`, `op` (`connect`, `prices`,
  `cost`) and `code` (the protocol's codes, or `other`). §3's
  `account` label on it is X1's: `cost` groups by one key, so cost by
  account and model needs either two series or a grouping by both,
  which X1 decides.
- **A model is a label only if the server has a price for it** (the
  `prices` op), among the `limits.models` largest; any other is
  `(other)`, as decision 92 has it for `aries.usage.tokens`. The label
  values are checked again where they are recorded: a project's id
  must have a UUID's shape and a model `proto.UsageModel`'s, and every
  value past a cap is `other` or `(other)`.
- **A project that cannot be read has no cost series** until it is
  read again, rather than its last figure: a failed poll clears it.
- **Limits** are the config's (`docs/sextant.md` has the table):
  projects (50, at most 500), models (20, at most 500), the request
  timeout (30s, 1s to 2m), the interval (60s, at least 10s) and the
  window (24h, 1m to 31 days). Nothing a client adds grows Sextant: it
  holds one connection per configured project and one set of series
  per project and model.
- **X1 and X3 (21 Oct 2026)** decide §3's grouping: `cost` groups by
  one key, so cost by account and by model are two series,
  `sextant.account.cost.usd` (`project`, `account`) beside
  `sextant.cost.usd` (`project`, `model`), with `sextant.hours`
  (`project`, `account`), `sextant.tokens` (`project`, `model`, `kind`)
  and `sextant.cost.unpriced.tokens` (`project`, `model`, the tokens of
  a group the server marks unpriced or of a model it has no price for).
  A poll sends `cost` twice, by model and by account. Accounts are
  capped at 20 (`telemetry.DefaultSextantAccounts`, not yet a config
  setting; a poll asks for `limits.models` groups) and must be shaped
  like a code name, or are `(other)`. `sextant.cost.amortized.usd` is
  not built. X3's rules and dashboard are
  `deploy/observability/alerts-sextant.yaml` and
  `dashboard-sextant.json`, tested in `internal/telemetry`
  (`harness_sextant_test.go`); the rules for a gate older than a day and
  nothing closed in a week wait for X2, and the `observe` profile does
  not run Sextant.
- **Not built:** a release, an installer line, a systemd unit and an
  AppArmor profile (X5); `deploy/observability`'s dashboard and rules
  (X3); and the Docker test server's `observe` profile does not run it.

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
   backend. *Superseded 19 Oct 2026 by decision 100:* it starts before
   v1.0.0, against a released server.

## 7. Questions for the maintainer

1. **License and repository.** Aries is AGPL-3.0-only with a CLA that
   lets Ariesworx LLC license it otherwise (decision 82). Is Sextant
   (a) in this repository under the AGPL, (b) in this repository under
   another license, or (c) in a repository of its own, licensed
   commercially? *Recommended:* (c), if Sextant is what is sold, so that
   Aries stays a complete open tool and the boundary between them is
   the public API, which is what v0 already uses. *Answered 19 Oct
   2026:* (a), in this repository under the AGPL. The CLA still lets
   Ariesworx LLC license it otherwise. Sextant is a binary of its own,
   and `aries` links none of it (rule 4).
2. **People.** Should per-person spend exist at all in v0, by name,
   before pseudonyms (X4)? *Recommended:* no. *Answered 19 Oct 2026:*
   no.
3. **Hosted or self-hosted.** Is the product a binary a customer runs
   beside `ariesd`, a hosted service that reads many customers' servers,
   or both? v0 is the binary either way; a hosted service changes what
   leaves a customer's network, and needs its own design. *Answered 19
   Oct 2026:* the binary, against one `ariesd`, proved out before it
   reads several servers or becomes a hosted service.
4. **Budgets.** Should a budget per account be data in Aries (so an
   agent can see it, and `start` could warn), or only an alert rule in
   Sextant? *Recommended:* an alert rule in v0; budgets in Aries are a
   separate 1.x design. *Answered 19 Oct 2026:* as recommended. Hard
   stops on spend are set at each model provider (enterprise review,
   question 5), and later managed in one place by Bearings' gateway
   (bearings.md §8, question 16).
