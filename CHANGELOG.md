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

## v0.5.0 (unreleased)

- The first release as Aries, and a fresh start: new names for every
  binary, file, variable and path, a new release signing key and a new
  license (AGPL-3.0-only). Breaking: nothing carries over from v0.4.0.
- Postgres replaces Dolt; the schema and the migrations begin again.
- Many projects on one server, HTTPS as the primary transport (SSH off
  unless turned on), sign-in through an identity provider, service
  principals, gates, dates and locks, work offline, and observability.
- The protocol is settled at 8 and spoken alone.
- Notes: [docs/releases/v0.5.0.md](docs/releases/v0.5.0.md) (a draft until the cut).

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
