# Aries

[![ci](https://github.com/ariesworx/aries/actions/workflows/ci.yml/badge.svg)](https://github.com/ariesworx/aries/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-AGPL--3.0--only-blue.svg)](LICENSE)

Aries takes its name from the first point of Aries: the point on the
celestial sphere where the Sun crosses the celestial equator northward at
the March equinox. Celestial navigation measures the sky from it. A
star's sidereal hour angle is its angle westward from the first point of
Aries, and the almanac gives that point's hour angle for every second of
the year. With those figures, a clock and a sextant sight of two or
three stars, anyone anywhere on the globe can fix their position and
chart a course, with no signal from anyone else. Aries does the same for
AI coding agents: one shared reference that every agent, on any machine
and with any model, measures its work against (who holds what, what is
ready, what was learned), so that each can fix where it stands and
chart its next step.

**An issue tracker built for AI coding agents.** Agents claim work, hand
it off and see who else is working, across sessions and machines. They
reach Aries through MCP (Model Context Protocol), the standard way
coding agents call tools, and people use the same tracker from the
command line. One small Go server keeps the data in
[Postgres](https://www.postgresql.org).

> **Status: stage 5 of 7 done** (stage 3 waits only on token capture
> for Codex and Gemini CLI, a follow-up; stage 3a, many projects on one
> server, is built and waits on checks on real machines; stage 5 has
> not run on Windows). Issues, claims, handoffs, the inbox,
> a live board, scoped memory, token cost (at list price and amortized
> over plans), people's hours, agent setup and importing from bd (beads)
> work today. Stage 5, working without the server, is built and passes
> its gate in the test stack: the writes made while the server cannot
> be reached are kept on the machine and sent later, once each, for up
> to 7 days; a cache shows a session the work it holds while the server
> is away, saying so; a write that arrives after others changed the
> issue is parked as a conflict for a person, or kept as a comment; and
> `aries away` holds a person's claims through a planned absence. None of
> it has run on Windows. The store now runs on Postgres (slice P1 of
> the move from Dolt); the server guide, the installer and the Docker
> test server still set up Dolt until slice P4, so no server can be set
> up from them meanwhile. Not ready for production use
> yet; see the [roadmap](#roadmap).

## Why Aries

When several agents work on one codebase, they have to agree on who does
what. Aries makes that safe:

- **Claims can't go stale.** An agent claims an issue with a lease. If the
  agent dies, the lease lapses within 15 minutes and the issue is ready
  again. An agent that lost its claim cannot close work someone else has
  since taken.
- **One authority.** All changes go through one server, which puts them
  in order. There are no database merges and no conflicts to resolve.
- **Identity comes from SSH keys.** The server's sshd decides who you are.
  A client cannot claim to be someone else.
- **Built for context windows.** Results are compact: a write returns
  only the issue's id and new revision (`{id, rev}`), and no result passes
  about 2,000 tokens.
- **Failures say what to do.** Errors come with a `fix:` line naming the
  next step, and each exit code and error code has one documented
  meaning.

## What you can do with it

| You can | With | Read more |
|---|---|---|
| Track issues with types, priorities, labels, parents and dependencies, and see what is ready or blocked | `create`, `ready`, `blocked`, `dep` | [Issues](docs/concepts.md#issues), [ready and blocked](docs/concepts.md#dependencies-ready-and-blocked) |
| Claim an issue, then close it with a handoff note, filing what you found on the way | `start`, `finish` | [Claims](docs/concepts.md#claims-and-leases), [start and finish](docs/concepts.md#a-session-start-and-finish) |
| Check work against its acceptance criteria | `accept`, `finish --tick` | [Acceptance criteria](docs/concepts.md#acceptance-criteria) |
| Hand work between people and agents | `handoff --to` | [Handoffs](docs/concepts.md#handoffs) |
| Get mentions, handoffs, assignments and lost claims in an inbox the server pushes | `inbox`, `watch` | [The inbox](docs/concepts.md#the-inbox) |
| See every session at work and what it holds | `who` | [Who is at work](docs/concepts.md#who-is-at-work) |
| Watch ready, held and blocked work, and every change as it happens, in a terminal | `tui` | [The live board](docs/cli.md#the-live-board) |
| Summarize a day or a week for a standup | `digest` | [The digest](docs/concepts.md#the-digest) |
| Keep what the next session needs to know, for the project, the team or yourself alone | `remember`, `recall`, `memories`, `pin` | [Memory](docs/concepts.md#memory) |
| See the time and tokens each issue took, by account (tokens from Claude Code so far) | `show`, `digest`, `usage --hook` | [Accounts, time and tokens](docs/concepts.md#accounts-time-and-tokens) |
| Report what the tokens cost at list price, and amortized over subscription plans, by account, issue, epic, person or model | `cost`, `admin price`, `admin plan` | [Cost](docs/concepts.md#cost), [Prices](docs/server.md#prices), [Plans](docs/server.md#plans) |
| Log the hours people spend on an issue | `hours` | [Human hours](docs/concepts.md#human-hours) |
| Serve many projects from one server, and manage projects, people, keys and memberships from any machine | `admin project`, `admin principal`, `admin key`, `admin member`, `admin status` | [Serving many projects](docs/server.md#serving-many-projects) |
| Set up Claude Code, Codex, Gemini CLI, Cursor, VS Code, Junie, JetBrains AI Assistant or Claude Desktop | `setup` | [Agents](docs/agents.md) |
| Move a bd (beads) backlog over, keeping its IDs | `ariesd import-bd` | [Moving from bd](docs/migrate-from-bd.md) |
| Reach a server that has no public address, through Google Cloud IAP | `iap:` in `.aries.yaml` | [Using `aries`](docs/cli.md#servers-without-a-public-ip-google-cloud-iap) |
| Upgrade clients and the server from signed releases | `upgrade` | [Installing Aries](docs/install.md#upgrade-and-roll-back) |

Planned: an offline cache and conflict handling, and **Bearings**, an orchestrator that runs
and supervises many agents on top of Aries
([design](docs/design/bearings.md)).

## How it works

```text
 agent ──MCP──┐                             the server
              ├─▶ aries ══ SSH ══▶ sshd ─▶ ariesd stdio --principal NAME
 person ─CLI──┘   (pinned host key)            │ unix socket, 0700
                                               ▼
                                         ariesd serve ─▶ Postgres
```

- **`aries`** is the client: a command line for people, and an MCP server
  for agents. It runs on Linux, macOS and Windows. The name is short for
  Aries.
- **sshd** checks the key. Each key's line in `authorized_keys` runs one
  fixed command, `ariesd stdio --principal NAME`, so the key, not the
  client, decides your principal: the name the server knows you by.
- **`ariesd serve`** owns the data. It applies every change in order,
  records each as an event in the same transaction, and stores it in Postgres.
  It runs on Linux.
- Messages are newline-delimited JSON. Each connection opens with a
  version handshake.

## Deployment examples

| Example | For | Server | Cost a month |
|---|---|---|---|
| [Local](docs/deploy/local.md) | One developer and their agents | Your own Linux machine, WSL2 or a Linux VM | $0 |
| [Team](docs/deploy/team.md) | A team with agents on laptops, in cloud sandboxes and in CI | A small cloud VM, reached over SSH | About $19 |
| [Enterprise](docs/deploy/enterprise.md) | A company that keeps the tracker off the internet | A private VM behind Google Cloud IAP, with backups nothing on it can delete | About $65 to $114 |

Costs are Google Cloud list prices for us-central1, read in October 2026.
[Deployment examples](docs/deploy/README.md) compares the three.

## Quick start

**To join a project that has a server:**

1. [Install `aries`](#install).
2. Send your public SSH key, such as `~/.ssh/id_ed25519.pub`, to the
   server's admin. `aries` signs in with the key `ssh` would use: a default
   file in `~/.ssh`, or a key in ssh-agent. To use another, set
   `ARIES_KEY_FILE` to its path
   ([Your key is your identity](docs/cli.md#your-key-is-your-identity)).
3. In the repository, which has a committed `.aries.yaml`, check the
   connection:

   ```sh
   aries ready
   ```

4. If nobody has set your agent up in this repository yet, do it and
   commit what setup writes. For Claude Code:

   ```sh
   aries setup claude-code --write
   ```

   The files tell agents to call `prime` when a session starts, then
   `start` and `finish` for each issue. [Agents](docs/agents.md#set-up-an-agent)
   lists every agent, and the one step each person still takes on their
   own machine, such as approving the MCP server.

**To run your own server,** start with the [Local](docs/deploy/local.md)
example, or follow [Running a server](docs/server.md) on any Linux
machine.

## Install

On Linux or macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/ariesworx/aries/main/install.sh | sh
```

The script checks the release's checksum, and its signature when
OpenSSL 3 is installed, then installs `aries` into `~/.local/bin`. It never
uses sudo. On Windows, download the zip
from the [releases page](https://github.com/ariesworx/aries/releases).
[Installing Aries](docs/install.md) covers the options, the server
binary, building from source and verifying a download by hand. A server
on Ubuntu 24.04 LTS or 26.04 LTS is set up by one script, `install-server.sh`
([With the installer](docs/server.md#with-the-installer)).

## Documentation

| Guide | Covers |
|---|---|
| [How Aries works](docs/concepts.md) | Identity, issues, dependencies, claims and leases, handoffs, the inbox, the digest |
| [Using `aries`](docs/cli.md) | Connecting a repository, every command, exit codes |
| [Agents](docs/agents.md) | Setting agents up, session-start hooks, MCP tools, sessions |
| [CI gates](docs/ci.md) | Holding work until CI reports a check, landing, and workflows for GitHub Actions and GitLab CI, with what they cost |
| [Running a server](docs/server.md) | Setup step by step, principals, admins, settings, limits, metrics and health, upgrades, backups, troubleshooting |
| [Operating with Claude Code](docs/server.md#operate-a-server-with-claude-code) | The `aries-ops` plugin: deploy and run a server by talking to Claude, with every change shown and approved first |
| [Deployment examples](docs/deploy/README.md) | Local, team and enterprise deployments, with costs, and a Docker test server |
| [Installing Aries](docs/install.md) | Install options, manual downloads, verifying releases, upgrades |
| [Upgrading to 1.0](docs/upgrade-to-1.0.md) | From v0.4.0 to v1.0.0: reinstalling `aries`, what to remove, and setting up a new server (a draft until v1.0.0 ships) |
| [Release notes](docs/releases/v1.0.0.md) | What v1.0.0 adds, breaks, removes and supports, with what is still planned marked (a draft until it ships) |
| [Moving from bd](docs/migrate-from-bd.md) | Importing a beads backlog, and exporting it back |
| [Security model](docs/security-model.md) | Identity, host keys, the Dolt account, untrusted text, releases, CI checks |
| [Design documents](docs/design/README.md) | Principles, architecture, data model, the Dolt spike, Bearings |
| [Contributing](CONTRIBUTING.md) | From a clone to a passing gate, trying a change against a real server, opening a pull request, troubleshooting |
| [Releasing](RELEASING.md) | How releases are built, signed and published |

## Roadmap

| Stage | Delivers | State |
|---|---|---|
| 0 | Dolt concurrency spike | Done |
| 1 | Store, server, SSH transport, version handshake, issue CLI, bd import | Done |
| 2 | MCP server, `start`/`finish`, `digest`, `prime`, `upgrade`, agent setup | Done |
| 3 | Claims with leases, agents registry, inbox, event push, handoff, files to issues, live board, token capture | In progress. Built: claims, agents registry, inbox, event push, structured handoffs, idempotency, files to issues, acceptance checklists, similar closed issues, the live board, accounts, time and token reporting, and token capture for Claude Code. The stage's gate, a multi-session soak test, passes. Left: token capture for Codex and Gemini CLI, held until the maintainer uses them and their formats can be checked against real files |
| 3a | Many projects on one server, each with its own database and writer; keys and project memberships in the store; server and project admins; `aries admin` | Built, and its gate passes in the test stack, against a real sshd in Docker, and under systemd on an Ubuntu 24.04 virtual machine; not yet checked on a cloud provider's machine ([§15](docs/design/aries.md#15-projects-and-access)). Built: the control store; a server that holds many projects when `control_schema` is set, with membership checked at connection, project admins, and `ariesd control init` to adopt the existing project and import `authorized_keys`; `aries admin` to create, archive and activate projects and to manage principals, members and keys from anywhere, with connections closed and claims released when access ends; keys written from the store to a file sshd reads, so adding a person needs no shell on the server; `aries admin status` to watch the server and each project; and the soak test across two projects ([Serving many projects](docs/server.md#serving-many-projects)). To verify: the setup on a machine of each cloud provider, which only the Docker test server, that virtual machine (arm64) and a stand-in for a provider's login agent have run; and Dolt's memory with several projects on Linux. The supported server platform is Ubuntu LTS, with optional AppArmor profiles for ariesd ([Confine ariesd with AppArmor](docs/server.md#confine-ariesd-with-apparmor)); systems that run SELinux are not supported yet |
| 3b | Observability: OpenTelemetry traces and metrics from `ariesd`, a loopback endpoint for metrics and health, a log schema and audit stream, Dolt's metrics, and a Collector harness | Built ([§16](docs/design/aries.md#16-observability)): metrics, pushed over OTLP/HTTP and offered to a Prometheus scraper; traces, pushed over OTLP/HTTP, from the caller's trace context through the request into the store, with people named by a stand-in ([Traces](docs/server.md#traces)); the loopback listener with `/metrics`, `/healthz` and `/readyz`; readiness and a watchdog for systemd; `ariesd status`; Dolt's own metrics, turned on in the guide and the Docker image, with its memory, connections, uptime and collector in `aries admin status` ([Observability](docs/server.md#observability)); and a log schema with stable event names, a request id on every line of a request and in its refusal, the audit stream of access changes, and the same lines over OTLP as an option ([Logs](docs/server.md#logs)); and the harness: a Collector's configuration, alert rules and a dashboard as tested examples ([deploy/observability](deploy/observability)), Compose profiles that run a Collector and a Prometheus beside the Docker test server ([Telemetry](docs/deploy/docker.md#telemetry)), a smoke test of what reaches them in all three signals, and a soak that holds the instruments, the spans and the audit stream to the event logs. Metrics, traces and the listener were run under systemd with the AppArmor profiles enforced, on an Ubuntu 24.04 virtual machine. Not yet verified: the log schema and logs over OTLP under systemd and AppArmor; the harness anywhere but Docker on a Mac (arm64), so not on amd64, not with a Collector as a unit on a server, and not against a hosted backend; the dashboard in a Grafana; and the alert thresholds against real traffic |
| 4 | Team and personal memory with tags; prices and `aries cost` | Done. Built: scoped, tagged memory with pins and issue links (`remember`, `recall`, `forget`, `memories`, `pin`), ranked in `prime` and `start`, private user scope, a secrets lint, and import of bd's memories; prices (`aries admin prices`), `aries cost` and the MCP `cost` tool, and the cost in `show` and `digest`; human hours (`aries log`) in `show`, `digest` and `aries cost`; subscription plans (`aries admin plans`) and their amortized cost in `aries cost` |
| 5 | Offline cache, outbox, conflict resolution | Built, and its gate, the partition tests, passes in the test stack ([§18](docs/design/aries.md#18-working-without-the-server-the-stage-5-design)): the outbox for `create`, `comment`, `handoff`, `remember`, logged hours and `finish`, and since protocol 6 for `update`, labels, `dep add`, `accept` and token usage, sent once each when the server answers, within a window of 7 days (`idem_window`); `aries outbox` and the refused list ([When the server cannot be reached](docs/cli.md#when-the-server-cannot-be-reached)); the cache of a session's own context; since protocol 7 conflicts: a replayed update parks the fields others changed, for a person to resolve with `aries resolve`, and an agent may propose ([Conflicts](docs/cli.md#conflicts)); `aries away` within the server's `away_max`; and since protocol 8 the late `finish`: one that arrives after its lease lapsed applies if nobody took the issue, and is kept as a comment if somebody did. Not run: any of it on Windows, or against a real network that partitions rather than the soak's cut dials. Not built: claims made offline, and a full local copy of the project (decisions 39 and 40) |
| 6 | Locks, gates, formulas, swarm, cross-project | In progress: slices G0, G1 and G2 are built: human gates (`aries gate`; agents may add one, a person passes it), dates on an issue (`--defer-until`, `--due`, listed by `digest` and `prime`), CI gates, which a CI job's `aries gate pass` or `fail` reports by its branch, with `landed` telling closed from landed ([CI gates](docs/ci.md)), and locks: leased, fenced, first-in-first-out mutexes and semaphores that a script holds around a command with `aries lock run` ([Locks](docs/concepts.md#locks); [§20](docs/design/aries.md#20-coordination-across-agents-and-projects-the-stage-6-design)) |
| 7 | Scheduled digests, GitHub sync, compaction, vectors | |

**v1.0.0.** The next release is v1.0.0. It holds the rename to Aries;
the move from Dolt to Postgres, slices P0, P1, P2, P4 and P5 of
[decision 79](docs/design/aries.md#14-decisions); stage 6's slices G0
to G2; sign-in through your identity provider (Google Workspace,
Microsoft Entra ID, Okta, Keycloak or GitHub), in the browser or by a
URL, beside SSH keys; service principals that sign in with their
platform's workload identity; a Claude Desktop extension (decision 83);
and a hardening pass: a review of every public name, flag, variable and
setting, the protocol settled at one version (decision
78), and the open follow-ups. Stage 6's G3 to G5 come in 1.x releases
as compatible additions. Of the move, P0 and P1 are built: the store,
the daemon and every test run on Postgres alone, a schema for each
project. Until P2 and P4 land, the status report has no figures of the
database's own, and the server guide, the installer, the AppArmor
profile and the Docker test server still describe Dolt.

The Bearings stages (B0 to B5) start once Aries stage 3 lands; see the
[Bearings plan](docs/design/bearings.md#6-plan).

## Names

**Aries** is the coordination server and its client, `ariesd` and
`aries`. **Bearings** is the orchestrator planned on top of it, a design
only. **Sextant** is reserved for future observability: metrics,
monitoring, tracing, and cost and usage reporting. It is a name only;
nothing moves under it now.

## Contributing

[CONTRIBUTING.md](CONTRIBUTING.md) gets you from a clone to a passing
gate (`scripts/gate.sh` runs what CI runs) and to a pull request. [AGENTS.md](AGENTS.md) holds the rules every
contributor follows, person or agent.

## Relationship to beads

Aries is an independent project, inspired by and able to import from
[beads](https://github.com/gastownhall/beads) (`bd`). It is not part of, or
endorsed by, the beads or Dolt projects.

## License

AGPL-3.0-only. See [LICENSE](LICENSE). Copyright (C) 2026 Ariesworx LLC.

Contributions are accepted under a contributor license agreement, as
[CONTRIBUTING.md](CONTRIBUTING.md#license-and-contributor-agreement)
says.
