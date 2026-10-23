# Changelog

One entry per release, newest first: its date, what matters most, and a
link to its full notes in [docs/releases](docs/releases). The entry is
written when the release is cut ([RELEASING.md](RELEASING.md#cutting-a-release)),
and `scripts/changelog_test.go` fails if a tag has no entry or no notes
file, if the entries are out of order, or if a link does not resolve.

Until v1.0.0, which is held as long as possible, every release is a
minor version (decision 93). Entries for v0.1.0 to v0.4.0 were written
afterward, from the notes GitHub generated for each release; the project
had another name then, and its notes say Aries and `aries` today.

## v0.7.0 (unreleased)

- Protocol 10, accepting 9: v0.6.0's `aries` keeps working against a
  v0.7.0 server, v0.5.0's is refused, and `aries` v0.7.0 needs a v0.7.0
  server; upgrade `ariesd` first. Migration 3 adds a price's tier.
- Prices by prompt length (Haiku 5.5 over 100,000 tokens); ariesd
  instruments for flow, closes, waits and the ready queue; Sextant's
  cost by account, with alert rules and a dashboard.
- Experimental, CLI only: `aries dispatch`, `aries outcome` and `aries
  guard`, a policy hook every Claude Code session can run. Also `aries
  prs merges` (read only), `aries setup pull-job` and a reworked help,
  which deprecates `admin principal add --key` for `--pubkey`.
- Breaking: `aries dispatch run` needs `.aries/guard.yaml`.
- Notes: [docs/releases/v0.7.0.md](docs/releases/v0.7.0.md).

## v0.6.0 (2026-10-20)

- Protocol 9, accepting 8: v0.5.0's `aries` keeps working against a
  v0.6.0 server, and `aries` v0.6.0 needs one; upgrade `ariesd` first.
  No schema change.
- The HTTPS key proof is bound to the TLS session where `ariesd`
  terminates TLS (finding K-2), and `handoff` settles acceptance items.
- Cost per outcome, the gates that hold an issue in `blocked` and
  `show`, `aries watch --events`, and Sextant X0, built from source.
- Breaking: `cache_write` in `aries.usage.tokens` is now the
  five-minute cache write alone, beside `cache_write_1h`.
- Notes: [docs/releases/v0.6.0.md](docs/releases/v0.6.0.md).

## v0.5.0 (2026-10-17)

- The first release as Aries, and a fresh start: new names for every
  binary, file, variable and path, a new release signing key and a new
  license (AGPL-3.0-only). Breaking: nothing carries over from v0.4.0;
  reinstall `aries` and set up a new server.
- Postgres replaces Dolt; the schema and the migrations begin again.
- Many projects on one server, HTTPS as the primary transport (SSH off
  unless turned on), keys that end, gates, dates and locks, work
  offline, and observability. Sign-in, tokens and service principals
  ship off unless configured, reviewed in 1.x.
- A project's workflow: default acceptance items, closed means landed,
  `aries land`, review and approval gates and a close-out checklist.
- The protocol is settled at 8 and spoken alone; from v0.5.0 a protocol
  change keeps accepting the version before.
- Notes: [docs/releases/v0.5.0.md](docs/releases/v0.5.0.md).

## v0.4.0 (2026-10-09)

- Scoped memory with `recall`, `prime` and a secrets lint.
- Prices, `aries cost` and the `cost` tool; `aries log` hours and
  subscription plans with amortized cost.
- Notes: [docs/releases/v0.4.0.md](docs/releases/v0.4.0.md).

## v0.3.0 (2026-10-09)

- Token usage stored and attributed to issues and accounts, captured
  from Claude Code with `aries usage --hook`.
- Files tied to issues, and the live board, `aries tui`.
- A multi-session soak test, and `claim.release` events.
- Notes: [docs/releases/v0.3.0.md](docs/releases/v0.3.0.md).

## v0.2.2 (2026-10-08)

- Seventeen minor findings from the Go review fixed; a test-only fix
  that keeps Dolt's metrics sender from outliving test servers.
- Notes: [docs/releases/v0.2.2.md](docs/releases/v0.2.2.md).

## v0.2.1 (2026-10-08)

- Seven bugs found in the Go review fixed.
- The README split into guides, with deployment examples.
- Notes: [docs/releases/v0.2.1.md](docs/releases/v0.2.1.md).

## v0.2.0 (2026-10-07)

- Claims with leases, epochs and a server reaper; the agents registry
  and `who`; the inbox, server push and structured handoffs.
- Idempotency keys, acceptance checklists and similar issues; `setup
  --all` and SessionStart hooks for every harness; servers behind
  Google Cloud IAP; a one-line install script.
- Security fixes: other principals' text treated as untrusted data,
  only the holder or an admin may change a held issue, and limits plus
  a Dolt account check in the server.
- Notes: [docs/releases/v0.2.0.md](docs/releases/v0.2.0.md).

## v0.1.0 (2026-10-07)

- The first release: the design, the issue store on Dolt, the server
  with its SSH transport, the issue CLI, bd import and export, the MCP
  server, `prime`, `start`, `finish`, `handoff`, `digest`, and signed
  releases with `upgrade`.
- Notes: [docs/releases/v0.1.0.md](docs/releases/v0.1.0.md).
