# Aries

[![ci](https://github.com/ariesworx/aries/actions/workflows/ci.yml/badge.svg)](https://github.com/ariesworx/aries/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-AGPL--3.0--only-blue.svg)](LICENSE)

**An issue tracker, shared memory and coordination layer for AI coding
agents.** Agents claim work, hand it off and see who else is working,
across sessions, machines and models. They reach Aries through MCP
(Model Context Protocol), the standard way coding agents call tools;
people use the same tracker from the command line. One small Go server
keeps the data in [Postgres](https://www.postgresql.org).

Aries is for teams, and single developers, who run several agents on
one codebase and need them to agree on who does what, what is ready,
what was learned and what has really landed.

> **Status: stage 5 of 7 done; v0.5.0 is next.** Not ready for
> production use yet.
>
> - Works today: issues, claims, handoffs, the inbox, the live board,
>   memory, token cost, people's hours, gates, locks, agent setup, and
>   import from bd (beads).
> - Built and passing its gates in the test stack: many projects on one
>   server, observability, work while the server cannot be reached, and
>   a project's workflow, where *closed* means *landed*. None of the
>   offline work has run on Windows.
> - HTTPS is the default transport; SSH is off until an operator turns
>   it on. v0.5.0 authenticates people and services by keys
>   ([decision 91](docs/design/aries.md#14-decisions)). Sign-in at a
>   company's identity provider and CI sign-in by workload token are on
>   `main`, off unless configured, and not yet security-reviewed.
>
> The [roadmap](#roadmap) has the detail.

## Quick start

**Join a project that has a server:**

1. [Install `aries`](#install).
2. Send your public SSH key, such as `~/.ssh/id_ed25519.pub`, to the
   server's admin. `aries` uses the key `ssh` would: a default file in
   `~/.ssh`, or a key in ssh-agent; set `ARIES_KEY_FILE` to use another
   ([Your key is your identity](docs/cli.md#your-key-is-your-identity)).
   Where the server signs people in at the company's identity provider,
   run `aries login` instead ([Sign in](docs/cli.md#sign-in)).
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
   `start` and `finish` for each issue.
   [Agents](docs/agents.md#set-up-an-agent) lists every agent and the
   one step each person still takes on their own machine.

**Run your own server:** start with the [Local](docs/deploy/local.md)
example, or follow [Running a server](docs/server.md) on any Linux
machine. On Ubuntu 24.04 LTS or 26.04 LTS, one script,
`install-server.sh`, does the setup
([With the installer](docs/server.md#with-the-installer)).

## Install

On Linux or macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/ariesworx/aries/main/install.sh | sh
```

The script checks the release's checksum, and its signature when
OpenSSL 3 is installed, then installs `aries` into `~/.local/bin`. It
never uses sudo. On Windows, download the zip from the
[releases page](https://github.com/ariesworx/aries/releases).
[Installing Aries](docs/install.md) covers the options, the server
binary, building from source and verifying a download by hand.

## Why Aries

- **Claims can't go stale.** An agent claims an issue with a lease. If
  the agent dies, the lease lapses within 15 minutes and the issue is
  ready again. An agent that lost its claim cannot close work someone
  else has since taken.
- **One authority.** All changes go through one server, which puts them
  in order and records each as an event. There are no database merges.
  An edit made offline that meets another is parked for a person to
  resolve, never merged silently.
- **Closed means landed.** A project's workflow adds the acceptance
  items every issue must meet, and can hold an issue open until CI, or
  `aries land`, reports that its change merged.
- **Identity comes from keys the server knows.** Over HTTPS, `ariesd`
  checks a proof by your key; over SSH, sshd checks it. A client cannot
  claim to be someone else.
- **Built for context windows.** A write returns only the issue's id and
  new revision (`{id, rev}`), and no result passes about 2,000 tokens.
- **Failures say what to do.** Every error comes with a `fix:` line
  naming the next step, and each exit code and error code has one
  documented meaning.

## What you can do with it

**Plan and track work**

| You can | With | Read more |
|---|---|---|
| Track issues with types, priorities, labels, parents, dependencies and dates, and see what is ready or blocked | `create`, `ready`, `blocked`, `dep` | [Issues](docs/concepts.md#issues), [Ready and blocked](docs/concepts.md#dependencies-ready-and-blocked), [Dates](docs/concepts.md#dates) |
| Claim an issue, then close it with a handoff note, filing what you found on the way | `start`, `finish` | [Claims](docs/concepts.md#claims-and-leases), [Start and finish](docs/concepts.md#a-session-start-and-finish) |
| Check work against its acceptance items | `accept`, `finish --tick` | [Acceptance criteria](docs/concepts.md#acceptance-criteria) |
| Set the acceptance items and required checks each issue type gets | `admin workflow`, `workflow check` | [The workflow](docs/concepts.md#the-workflow) |
| Hold work until a person approves or CI reports a check | `gate` | [Gates](docs/concepts.md#gates), [CI gates](docs/ci.md) |
| Report what merged, from a machine, for a server CI cannot reach | `land` | [A server CI cannot reach](docs/ci.md#a-server-ci-cannot-reach) |
| Hold a lock or a semaphore around a command | `lock run` | [Locks](docs/concepts.md#locks) |

**Work together**

| You can | With | Read more |
|---|---|---|
| Hand work between people and agents | `handoff --to` | [Handoffs](docs/concepts.md#handoffs) |
| Get mentions, handoffs, assignments and lost claims in an inbox the server pushes | `inbox`, `watch` | [The inbox](docs/concepts.md#the-inbox) |
| See every session at work and what it holds | `who` | [Who is at work](docs/concepts.md#who-is-at-work) |
| Watch ready, held and blocked work change live in a terminal | `tui` | [The live board](docs/cli.md#the-live-board) |
| Summarize a day or a week for a standup | `digest` | [The digest](docs/concepts.md#the-digest) |
| Keep what the next session needs to know, for the project, the team or yourself | `remember`, `recall`, `memories`, `pin` | [Memory](docs/concepts.md#memory) |
| Keep working while the server cannot be reached: writes queue and are sent once, later | `outbox`, `cache`, `resolve`, `away` | [When the server cannot be reached](docs/cli.md#when-the-server-cannot-be-reached) |

**Measure time and cost**

| You can | With | Read more |
|---|---|---|
| See the time and tokens each issue took, by account (tokens from Claude Code so far) | `show`, `digest`, `usage --hook` | [Accounts, time and tokens](docs/concepts.md#accounts-time-and-tokens) |
| Report token cost at list price, and amortized over subscription plans, by account, issue, epic, person or model | `cost`, `admin price`, `admin plan` | [Cost](docs/concepts.md#cost), [Prices](docs/server.md#prices), [Plans](docs/server.md#plans) |
| Log the hours people spend on an issue | `hours` | [Human hours](docs/concepts.md#human-hours) |

**Set up and run**

| You can | With | Read more |
|---|---|---|
| Set up Claude Code, Codex, Gemini CLI, Cursor, VS Code, Junie, JetBrains AI Assistant or Claude Desktop | `setup` | [Agents](docs/agents.md) |
| Serve many projects from one server, and manage projects, people, keys and memberships from any machine | `admin project`, `admin principal`, `admin key`, `admin member` | [Serving many projects](docs/server.md#serving-many-projects) |
| Watch the server: metrics, traces, logs, health and a status report | `admin status`, `ariesd status` | [Observability](docs/server.md#observability) |
| Reach a server that has no public address, through Google Cloud IAP | `iap:` in `.aries.yaml` | [Servers without a public IP](docs/cli.md#servers-without-a-public-ip-google-cloud-iap) |
| Move a bd (beads) backlog over, keeping its IDs | `ariesd import-bd` | [Moving from bd](docs/migrate-from-bd.md) |
| Upgrade clients and the server from signed releases | `upgrade` | [Upgrade and roll back](docs/install.md#upgrade-and-roll-back) |

[Using `aries`](docs/cli.md#commands) lists every command.

## How it works

```text
 agent ──MCP──┐                                      the server
              ├─▶ aries ══ HTTPS (WebSocket) ═══════▶ ariesd serve ─▶ Postgres
 person ─CLI──┘     ╚═══ SSH, when turned on ══▶ sshd ─▶ ariesd stdio ─┘
                                                        (unix socket, 0700)
```

| Part | What it does | Runs on |
|---|---|---|
| `aries` | The client: a command line for people, and an MCP server for agents | Linux, macOS, Windows |
| `ariesd serve` | Owns the data: applies every change in order, records each as an event in the same transaction, and stores it in Postgres | Linux |
| HTTPS (default) | `aries` proves it holds a key the server knows; the server takes that key's principal, the name it knows you by | |
| SSH (when turned on) | sshd checks the key, and each key's line in `authorized_keys` runs one fixed command, `ariesd stdio --principal NAME` | |

Either way the key, not the client, decides who you are. Messages are
newline-delimited JSON, and each connection opens with a version
handshake. [Design](docs/design/aries.md) has the architecture, and
[Security model](docs/security-model.md) what each part trusts.

## Deployment examples

| Example | For | Server | Cost a month |
|---|---|---|---|
| [Local](docs/deploy/local.md) | One developer and their agents | Your own Linux machine, WSL2 or a Linux VM | $0 |
| [Team](docs/deploy/team.md) | A team with agents on laptops, in cloud sandboxes and in CI | A small cloud VM, reached over SSH | About $19 |
| [Enterprise](docs/deploy/enterprise.md) | A company that keeps the tracker off the internet | A private VM behind Google Cloud IAP, with backups nothing on it can delete | About $65 to $114 |

Costs are Google Cloud list prices for us-central1, read in October
2026. [Deployment examples](docs/deploy/README.md) compares the three,
and [the Docker test server](docs/deploy/docker.md) runs one locally for
development.

## Documentation

**Using Aries**

| Guide | Covers |
|---|---|
| [How Aries works](docs/concepts.md) | Identity, issues, dependencies, gates, locks, claims and leases, the workflow, handoffs, the inbox, memory, time and cost |
| [Using `aries`](docs/cli.md) | Connecting a repository, signing in, every command, working offline, exit codes |
| [Agents](docs/agents.md) | Setting agents up, session-start and usage hooks, MCP tools, sessions |
| [CI gates](docs/ci.md) | Holding work until CI reports a check, landing, `aries land`, and workflows for GitHub Actions and GitLab CI, with what they cost |
| [Moving from bd](docs/migrate-from-bd.md) | Importing a beads backlog, and exporting it back |

**Running a server**

| Guide | Covers |
|---|---|
| [Running a server](docs/server.md) | Setup step by step, principals, admins, sign-in, settings, limits, observability, upgrades, backups, troubleshooting |
| [Operating with Claude Code](docs/server.md#operate-a-server-with-claude-code) | The `aries-ops` plugin: deploy and run a server by talking to Claude, with every change shown and approved first |
| [Deployment examples](docs/deploy/README.md) | Local, team and enterprise deployments, with costs, and a Docker test server |
| [Installing Aries](docs/install.md) | Install options, manual downloads, verifying releases, upgrades |
| [Upgrading to 0.5](docs/upgrade-to-0.5.md) | From v0.4.0 to v0.5.0: reinstalling `aries`, what to remove, and setting up a new server (a draft until v0.5.0 ships) |
| [Release notes](docs/releases/v0.5.0.md) | What v0.5.0 adds, breaks, removes and supports (a draft until it ships) |
| [Changelog](CHANGELOG.md) | One entry per release, newest first, each linking its full notes |
| [Security model](docs/security-model.md) | Identity, host keys, the database role, untrusted text, releases, CI checks |

**Building Aries**

| Guide | Covers |
|---|---|
| [Design documents](docs/design/README.md) | Principles, architecture, decisions, each stage's design, Bearings, and the research and reviews behind them |
| [Contributing](CONTRIBUTING.md) | From a clone to a passing gate, trying a change against a real server, opening a pull request |
| [Releasing](RELEASING.md) | How releases are built, signed and published |
| [AGENTS.md](AGENTS.md) | The rules every contributor follows, person or agent |

## Roadmap

| Stage | Delivers | State |
|---|---|---|
| 0 | Concurrency spike of the first store, Dolt | Done |
| 1 | Store, server, version handshake, issue CLI, bd import | Done |
| 2 | MCP server, `start` and `finish`, `digest`, `prime`, `upgrade`, agent setup | Done |
| 3 | Claims with leases, the agents registry, the inbox, event push, handoffs, the live board, token capture | Built, and its soak passes. Left: token capture for Codex and Gemini CLI, held until their formats can be checked against real files |
| 3a | Many projects on one server; keys and memberships in the store; `aries admin` | Built; its gate passes in the test stack and on an Ubuntu 24.04 VM under systemd. Not yet run on a cloud provider's machine ([§15](docs/design/aries.md#15-projects-and-access)) |
| 3b | Observability: OpenTelemetry metrics, traces and logs, health endpoints, an audit stream, and tested Collector, alert and dashboard examples | Built ([§16](docs/design/aries.md#16-observability)). Not yet verified against a hosted backend or real traffic |
| 4 | Team and personal memory; prices, plans, hours and `aries cost` | Done |
| 5 | Working without the server: outbox, cache, conflicts, `aries away` | Built, and its partition soak passes in the test stack ([§18](docs/design/aries.md#18-working-without-the-server-the-stage-5-design)). Not run on Windows or on a real network partition; claims made offline are not built |
| 6 | Gates, dates, locks, formulas, swarm, cross-project work | In progress: human and CI gates, dates and locks (slices G0 to G2) are built; G3 to G5 come in 1.x ([§20](docs/design/aries.md#20-coordination-across-agents-and-projects-the-stage-6-design)) |
| 7 | Scheduled digests, GitHub sync, compaction, vectors | Not started |

Beside the stages, **a project's workflow**
([§21](docs/design/aries.md#21-a-projects-workflow-the-flow-aries-enforces)):
default acceptance items (W0), close means landed (W1) and `aries land`
(W2) are built; review and approval, close-out lines and a server-wide
default (W3 to W5) are not.

**Releases.** Releases are minor versions, v0.5.0, v0.6.0 and on, until
v1.0.0, which is held as long as possible
([decision 93](docs/design/aries.md#14-decisions)).

- **v0.5.0, next:** the rename to Aries, Postgres in place of Dolt, many
  projects, observability, working offline, gates and locks, HTTPS as
  the primary transport, authentication by keys, and a hardening pass.
  Left before it ships: ACME against a real CA, and the security review
  of the HTTPS listener. The [release notes](docs/releases/v0.5.0.md)
  have the full list.
- **From v0.5.0 on:** a release that changes the protocol keeps
  accepting the one before it, and the names a dashboard or an alert
  relies on are kept.
- **Before v1.0.0:** the project workflow setting and organization
  memory.
- **In 1.x:** sign-in against a real identity provider, with its
  security review; a remote MCP endpoint; GitHub's sign-in; a Claude
  Desktop extension; and stage 6's G3 to G5
  ([Coming in 1.x](docs/releases/v0.5.0.md#coming-in-1x)).

**Bearings**, an orchestrator that runs and supervises many agents on
top of Aries, is a design only; its stages B0 to B5 are planned in the
[Bearings plan](docs/design/bearings.md#6-plan).

## Names

Aries takes its name from the first point of Aries, the point on the
celestial sphere from which celestial navigation measures the sky. With
the almanac's figures for it, a clock and a sextant, a navigator fixes
their position with no signal from anyone else. Aries gives AI coding
agents the same: one shared reference, on any machine and with any
model, to fix where their work stands and chart the next step.

- **Aries** is the coordination server and its client, `ariesd` and
  `aries`.
- **Bearings** is the orchestrator planned on top of it, a design only.
- **Sextant** is reserved for future observability: metrics,
  monitoring, tracing, and cost and usage reporting. It is a name only.

## Contributing

[CONTRIBUTING.md](CONTRIBUTING.md) gets you from a clone to a passing
gate (`scripts/gate.sh` runs what CI runs) and to a pull request.
[AGENTS.md](AGENTS.md) holds the rules every contributor follows,
person or agent.

## Relationship to beads

Aries is an independent project, inspired by and able to import from
[beads](https://github.com/gastownhall/beads) (`bd`). It is not part of,
or endorsed by, the beads or Dolt projects.

## License

AGPL-3.0-only. See [LICENSE](LICENSE). Copyright (C) 2026 Ariesworx LLC.

Contributions are accepted under a contributor license agreement, as
[CONTRIBUTING.md](CONTRIBUTING.md#license-and-contributor-agreement)
says.
