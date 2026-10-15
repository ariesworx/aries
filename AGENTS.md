# Agent instructions

Canonical instructions for any AI coding agent working in this repository
(Claude Code, Codex, Gemini CLI, Cursor, GitHub Copilot and others). Codex,
Cursor and most other agents read this file directly. `CLAUDE.md` and
`GEMINI.md` import it, and `.github/copilot-instructions.md` points to it.
Put shared guidance here, not there.

## What this is

Aries is a public, AGPL-3.0-only Go module: an issue tracker, shared memory
and coordination layer for AI coding agents, stored in Dolt. `aries` is the
client (a CLI for people and an MCP server for agents); `ariesd` is the
server daemon and the sshd forced-command bridge. Bearings, the orchestrator,
is a design only ([docs/design/bearings.md](docs/design/bearings.md)); no
Bearings code exists yet.

Read [README.md](README.md) for the overview, the roadmap and the map of
the guides in `docs/`, and [docs/design/aries.md](docs/design/aries.md)
for the principles and architecture. The README's status line says which
stage is in progress.

## Layout

| Path | Holds |
|---|---|
| `cmd/aries` | `aries` entry point: builds a `cli.Env` from the process and calls `cli.Run` |
| `cmd/ariesd` | `ariesd` entry point: `serve` (which, on a server of many projects, also writes the keys file sshd reads), `stdio`, `upgrade`, `status` (the running daemon's report, over its socket), and the admin `control init`, `import-bd` and `export-bd`. `serve` also wires the telemetry, the loopback listener and systemd's notifications (`runDaemon`) |
| `internal/store` | Typed store over Dolt: single writer, CAS updates, event log, ready and blocked queries; and conflicts (`conflicts.go`, design §18.7): a replayed update compared field by field with the state at its rev, what others changed parked for a person to resolve, and agents' proposals. Also `Control` (`control*.go`), the same machinery over a second database for what is the server's: projects, principals, their public keys and memberships with roles, and the in-memory index the daemon reads at each handshake and each project store asks for its admins. `stats.go` holds what `aries admin status` reads from memory: a store's write counters, and the control index's counts. `observe.go` holds the two hooks the daemon measures and traces a store through, `Observer` and `Tracer`; the store imports nothing of telemetry |
| `internal/store/migrations`, `internal/store/controlmigrations` | Embedded schema migrations for a project's database and for the control database, two separate sets: `NNNN_name.sql`, each numbered 1..n without gaps |
| `internal/proto` | Wire protocol: NDJSON frames, handshake, `Op` constants, `*Args` and `*Result` types, typed errors; and the bridge frame, which passes between two `ariesd` processes on the server, carries no version and only ever gains fields |
| `internal/server` | Daemon: socket, peer check, sshd bridge, dispatch of ops to the connection's project's store, error mapping, settings; with `control_db`, the membership check at the handshake, the registry of open project stores (`registry.go`), the `aries admin` ops over the control store (`admin.go`), the closing of connections whose access ended (`conns.go`), keys for sshd (`keys.go`): the `authorized_keys`-format file it writes from the control store (`Server.WriteKeys`), the hand-maintained `authorized_keys` it only reads, and the ending of claims whose holder lost access; and the report `aries admin status` reads (`status.go`) |
| `internal/client` | In-process SSH client with a pinned host key, over TCP or a Google Cloud IAP tunnel; which key it offers (`ARIES_KEY_FILE`, or `key:`, and then no other; with neither, the default identities of `~/.ssh` and then ssh-agent's keys); `.aries.yaml` discovery, and the one printed for a new project; session ids |
| `internal/safetext` | Unsafe characters (controls, bidi): the store's validation, bd import cleaning, and the escaping of everything clients print |
| `internal/lazyregexp` | Regular expressions in package variables, compiled on first use, so that a process does not pay at start for those it never uses: sshd starts an `ariesd stdio` for every connection. Use it, not `regexp.MustCompile`, for a package variable of a package `ariesd` links; `TestInitAllocations` (`cmd/ariesd`) bounds what initialization allocates |
| `internal/logs` | The schema of ariesd's log (design §16.4), on the standard library alone: every field a line may have, with whether it may leave the machine (`schema.go`), the stable name of every event (`events.go`), a request's id, and the `slog.Handler` that puts a request's id and its span's ids on each line from the line's context and hands every line to a tap, which is how lines are exported. Its tests hold every log call of the daemon's packages, and the tables of docs/server.md, to the lists |
| `internal/telemetry` | ariesd's metrics, traces and exported log records, and the only importer of the OpenTelemetry SDK, its exporters and the Prometheus client (design §16): the instruments and their fixed attribute values (`instruments.go`), spans with their fixed names, keys and values (`trace.go`, whose rule is that a span carries ids, codes and small numbers and never text), how a person appears on a span and the key of the stand-ins (`people.go`), log records over OTLP with only the fields a span could carry (`logs.go`), OTLP/HTTP push of all three from the `OTEL_*` variables, the Prometheus handler, the loopback listener with `/metrics`, `/healthz` and `/readyz`, and `Manual`, which reads the instruments, the spans and the exported log records in process for tests. A nil `*Telemetry` does nothing. Its tests fail if `cmd/aries` links any of the SDK or another package imports it |
| `internal/doltmetrics` | Dolt's own figures for the status report (design §16.5), with the standard library alone: a reader of Dolt's Prometheus endpoint that connects to loopback only, follows no redirect and gives up after 5 seconds, and a parser of its own for the six series ariesd shows, which trusts nothing in the body and has a fuzz target. `internal/server` reads it on a timer (`doltwatch.go`) and sets the collector's cycles against its stores' writes |
| `internal/sdnotify` | systemd's notify socket with the standard library: ready, stopping and the watchdog; a no-op off Linux and without `NOTIFY_SOCKET` |
| `internal/statustext` | The status report as text, shared by `aries admin status` and `ariesd status` |
| `internal/secretscan` | Text that looks like a credential (private keys, service and bearer tokens, JWTs, passwords in URLs and commands, secret assignments, long random tokens): the store's secrets lint for memories |
| `internal/cli` | `aries` commands, `aries admin` and `aries outbox` among them; writes only to the `Env` it is given. `outbox.go` has what a queued write prints, `--no-queue`, and the replay on a new connection; `cache.go`, what is kept on the way past, what a command answered from the cache prints, the reads refused offline, and `aries cache` |
| `internal/board` | `aries tui`, the live board: the model and its golden-tested frames, `Live` (event push, debounced rereads, reconnects), and the terminal loop over `golang.org/x/term` |
| `internal/capture` | Token usage read from a harness's local records (Claude Code transcripts) for `aries usage --hook`, with per-file offsets in a 0600 state file |
| `internal/outbox` | The queue of writes made while the server cannot be reached (design §18.3, §18.4; stage 5, slice F1), as a library that knows nothing of connections: `internal/offline` is its one caller. Plain files in a 0700 directory its caller names: an NDJSON outbox synced on each append, the answers, and counts of what was queued and for how long. Which ops are queued (`entry.go`), the secrets lint before an entry is written, caps on entries, bytes and age, replay by one process at a time, the refused list with retry and drop. The locks are `flock` and `LockFileEx` through `syscall`, as F0 measured them; `doc.go` has the rules, why each is there, and what has not run on Windows. Its tests run real processes, some of them killed, against one directory |
| `internal/offline` | What `aries` and `aries mcp` do with a write that did not reach the server (design §18.3, As built): which errors say so is `client.Unreachable`; here are the data directory on each system and `ARIES_DATA_DIR`, the check that it is the user's own, the record of who the server took each key for (`seen.json`) and the rule from it to a principal's outbox, each refusal to queue with its code and fix, the replay of one principal's outbox on that principal's connection, and `prime`'s notice. And the cache of what a principal's sessions read (`cache.go`, design §18.6, As built for F3): which answers are kept and how they are keyed, the files and their caps, which reads are refused offline, and when the server was last reached. `doc.go` has the rules; nothing in it has run on Windows |
| `internal/mcpserver` | MCP tools, `prime`, token budgets, agent-facing error rewording; and the trace context a tool call brings in its request's metadata, which its requests then carry. With an outbox (`Options.Offline`), a queueable tool call that does not reach the server is queued and answered in one line (`offline.go`), and each new connection sends what waits first. With a cache (`Options.Cache`, `cache.go`), what a tool call reads is kept, and with the server away `prime`, `show`, `inbox` and `digest` answer from it, saying so, while `ready`, `blocked`, `list` and `who` are refused |
| `internal/agentsetup` | `aries setup AGENT` and `--all`: per harness, the MCP config, pointer block, SessionStart hook (and Claude Code's usage hooks) and the hook's output format; per-project entries in a desktop app's config (`desktop.go`); `ARIES_KEY_FILE` in an entry of the person's own files (`--key-file`), never a project's |
| `internal/clienttest` | `Isolate`, which keeps a test binary from the developer's `~/.ssh`, ssh-agent, key variables and outbox (it sets `ARIES_DATA_DIR`); every package whose tests connect calls it from `TestMain`. `IsolateGit`, which keeps the git a test binary starts from the developer's git configuration. Its tests read the module's tests: each is parallel or says why not |
| `internal/gitx` | Branch names from issues, issue ids from branches and `Aries:` trailers |
| `internal/bdimport` | bd JSONL import and export |
| `internal/release` | Release download, signature and checksum checks, binary swap. A release is read from one of two sources behind one interface: GitHub's API (`release.go`), or a static tree on any HTTPS file host or in a directory (`static.go`, with `index.json` in `index.go`), chosen by `DefaultSource`, `ARIES_RELEASES` and `release.Open`. The source is transport and is never trusted: `Fetch` accepts a release on its signature and checksum alone, a static tree's tags are checked before they reach a URL, and no redirect from one is followed. `releasetest` serves a fake of each source |
| `internal/tools/releasekey`, `internal/tools/releaseindex` | The signing tool; and the tool that lays a release's files out as a static tree and writes its `index.json`, which signs nothing and needs no secret ([RELEASING.md](RELEASING.md#publishing-to-a-static-host)) |
| `internal/version` | Build version, and `Dolt`, the pinned Dolt release, with the SHA-256 of its archives (`dolt.sha256`); its tests keep every copy of a tool pin in step |
| `internal/dolttest` | Starts a throwaway `dolt sql-server` for tests, grants its locked-down account databases by name, and drops a test's database when the test is done with it. `Pool` is a test binary's servers: each test holds one alone while it runs, so tests run in parallel |
| `internal/iaptest` | A fake `gcloud` for tests of the IAP transport: the test binary, re-executed from `PATH` |
| `internal/e2e` | End-to-end tests: CLI and MCP through an in-process SSH server to a real daemon and store. The SSH server can read the keys file the daemon writes, before its own keys, as sshd reads its `AuthorizedKeysFile`. `TestSoak` (`soak*_test.go`) is the gate of stages 3 and 3a ([CONTRIBUTING.md](CONTRIBUTING.md#the-soak-test)); with `ARIES_SOAK_TELEMETRY` it runs with the daemon's telemetry on, and holds the instruments, the spans and the audit stream to the event logs or gives the daemon a collector that is not there (`soak_telemetry_test.go`); with `ARIES_SOAK_PARTITION` it is the gate of stage 5, cutting agents off from the server for minutes at a time and holding every write they queued to their outboxes and the event logs (`soak_partition_test.go`) |
| `deploy/docker` | The Docker test server ([docs/deploy/docker.md](docs/deploy/docker.md)): image, entrypoint, compose file and `smoke.sh`, which drives it end to end and is not part of CI. It mirrors docs/server.md's setup steps; `internal/version` keeps its Dolt at the pin. Telemetry is off unless asked for: `ARIES_TELEMETRY=1` turns ariesd's on, its log export included, and the compose profiles `telemetry` and `prometheus` run a Collector and a Prometheus, pinned by digest, in the server's network namespace, since what they read answers on loopback alone; `ARIES_SMOKE_TELEMETRY=1` has `smoke.sh` check them |
| `deploy/observability` | Examples that are tested, for operators (design §16.7): an OpenTelemetry Collector's configuration for metrics, traces and logs, alert rules in Prometheus's format, a Prometheus configuration that loads them, and one dashboard. Vendor-neutral (rule 1). Tests in `internal/telemetry` (`harness_test.go`) fail if a rule or a query names a metric, a label or a value ariesd does not have, so change them with the instruments |
| `deploy/apparmor` | AppArmor profiles for `ariesd` on Ubuntu, and their tunables. A change to what `ariesd` reads, writes, connects to or runs changes them too |
| `scripts` | For contributors, in POSIX shell: `gate.sh` runs the gate (Commands, below), and `install-dolt.sh` installs the pinned Dolt, verified, under the user's cache directory. `leftovers.sh` runs a command, the tests usually, with a temporary directory of its own and fails if it leaves a file there or a process running; the gate's tests run under it. The tests beside them run all three against stand-ins; and `TestNoOldName` fails if the path or the text of any file in the repository names the project by a former name, with no exception |
| `plugins/aries-ops`, `.claude-plugin/marketplace.json` | The operations skill, a Claude Code plugin, and the marketplace file that lists it ([Operate a server with Claude Code](docs/server.md#operate-a-server-with-claude-code)): `SKILL.md` holds the workflow, the safety rules and a map of `references/*.md`, one per area, each step citing the guide's section it follows. It runs on an operator's workstation and drives the installer, `aries admin` and `ariesd` over the operator's SSH; it never handles a secret. Anything specific to the database belongs in `references/database.md`, which until the move to Postgres lands holds no steps and links the guide's current ones. `scripts/opsskill_test.go` holds its commands, flags and citations to the code and the guides, and its manifests and frontmatter to what Claude Code reads. It has no version: an operator adds the marketplace at a release's tag |
| `mise.toml` | Go, golangci-lint and shellcheck versions for contributors who use mise; optional |
| `install-server.sh` | The server installer: does docs/server.md's setup, for a server of many projects, on a fresh Ubuntu 24.04 LTS or 26.04 LTS machine, as root, each step checking before it acts; `--dry-run` changes nothing. It installs the binaries through `install.sh`, holds no release key, and holds the Dolt pin, which `internal/version` keeps in step. `scripts/installserver_test.go` fails when its steps, or the text of a file it writes, differ from the guide's ([With the installer](docs/server.md#with-the-installer)) |
| `install.sh` | The one-line installer for `aries` and `ariesd`, from GitHub or from a static tree (`ARIES_RELEASES`); `internal/release/install_test.go` and `install_static_test.go` run it against each, and tests check that its key is in `keys.go` and its `DEFAULT_RELEASES` is `release.DefaultSource` |
| `spike/outbox` | Stage 5 (slice F0) measurements of the local store of design §18.4 under many processes: file locks, the outbox, replay, compaction and the cache. Kept as evidence; not maintained and excluded from lint |
| `spike/postgres` | Measurements of the store on Postgres beside Dolt, a module of its own with a copy of the store at the time, kept as the evidence for decision 79 (design §14). Not maintained and excluded from lint |
| `docs` | User guides: concepts, `aries`, agents, CI gates and their workflows, running a server, deployment examples, install, moving from bd, the security model |
| `docs/design` | Design documents |
| `.claude/agents`, `.codex/agents`, `.gemini/agents` | Agent specifications (below); `internal/agentspec` keeps them in step |

## Commands

`scripts/gate.sh` runs the gate: the commands below, which are what CI
runs (`.github/workflows/ci.yml`), in this order. Run it before opening a
pull request, and report what its summary says a run on your machine did
not cover. This block is the list of record; a test in `scripts/` fails
when the script and the block differ, so change them together, and
`ci.yml` with them.

```sh
gofmt -l .                       # must print nothing
go vet ./...
golangci-lint run ./...          # CI pins v2.14.0; config in .golangci.yml
go mod verify                    # module cache still matches go.sum
git ls-files -co --exclude-standard -z "*.sh" | xargs -0 shellcheck
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
GOOS=darwin GOARCH=amd64 go vet ./...          # aries must build and vet
GOOS=darwin GOARCH=amd64 go build ./cmd/...    # on every developer OS
GOOS=windows GOARCH=amd64 go vet ./...
GOOS=windows GOARCH=amd64 go build ./cmd/...
ARIES_REQUIRE_DOLT=1 scripts/leftovers.sh go test -race -shuffle=on ./...
```

The script stops at the first failure (`--keep-going` does not), and
away from Linux it also vets, builds and lints for Linux, which CI does
natively. The tests run under `scripts/leftovers.sh`, which fails a run
that passed but left a file in its temporary directory (or in
`GOTMPDIR`, where `go test` makes each `t.TempDir` when it is set) or a
process running, and names the test that probably did.
`scripts/gate.sh --quick` is for the loop between edits and is
not the gate: it skips `go mod verify`, govulncheck and the builds for
other systems, and runs the tests without `-race` or `-shuffle`.

**Tools.** Go is at the `go` line in `go.mod`, at its newest patch
release, as CI installs it: on an older one govulncheck reports standard
library findings that are not the change's, which the script says.
golangci-lint is at the version CI pins. `mise.toml` pins both for
contributors who use mise, and tests in `internal/version` keep it equal
to `go.mod` and `ci.yml`.

CI also runs the tests named `TestCheckPeer…` (`internal/server`) under
`sudo`, because `TestCheckPeerOtherUID` and
`TestCheckPeerKeepsStatusFromOtherUsers` start a socket peer as another
user; they skip when not root. It builds the package's test binary as
its own user and runs only that binary as root, under
`scripts/leftovers.sh`, and the step fails unless both tests passed,
none was skipped and they left nothing. `scripts/gate.sh` prints
the command where it could not run them, and a test in `scripts/` keeps
the two alike.

Around those commands, in `ci.yml` and not in the gate a contributor
runs: every job of every workflow has a `timeout-minutes` (a job that
calls a workflow cannot, so `release.yml`'s `gate` relies on those of
`ci.yml`). `lint` and `test` each restore a Go build cache of their
own, and one module cache between them, keyed by the Go version and the
hash of `go.sum` with no fallback; only a push to `main` saves one, a
pull request and the merge queue's run of one only read them, and any
other run, the release's gate among them, uses none. The tests' temporary files, Dolt's databases
among them, are in memory when the runner's `/dev/shm` is fit for them
(`TESTS_TMPFS`), and otherwise in `/tmp` with a warning;
`scripts/leftovers.sh` makes its directory under the one that step
chose. `GOTMPDIR`, and with it the test binaries and each `t.TempDir`,
stays on disk, in `/tmp`.
[docs/design/ci-analysis.md](docs/design/ci-analysis.md) has the
measurements behind each.

CI also checks the syntax of the AppArmor profiles in `deploy/apparmor`
with `apparmor_parser -Q -K`, under Ubuntu 24.04's parser (in `lint`) and
26.04's (`apparmor-26-04`), which needs Linux with AppArmor and is not
in the gate above; it does not check that `ariesd` works when confined
([deploy/apparmor/README.md](deploy/apparmor/README.md)).
`deploy/docker/smoke.sh` is not part of the gate, and CI does not run it.
The `release` workflow runs this whole gate (`ci.yml`, as a reusable
workflow) on the tagged commit before it builds anything
([RELEASING.md](RELEASING.md)).

**Dolt.** The store, server, bdimport, `cmd/ariesd` and e2e tests start a
real `dolt sql-server` through `internal/dolttest`. They need `dolt` on
`PATH`, at the version in `version.Dolt` (`internal/version`; tests keep
CI's `DOLT_VERSION`, the Docker test server's and the checksums in
`internal/version/dolt.sha256` equal to it, so change them together as
[RELEASING.md](RELEASING.md#changing-the-dolt-pin) says, and
`TestDoltVersion` checks the `dolt` on `PATH`). `scripts/install-dolt.sh`
downloads that release for Linux or macOS, verifies its SHA-256 against
the pinned one, installs it under the user's cache directory and prints
the directory to put on `PATH`; `scripts/gate.sh` uses it by itself when
the `dolt` on `PATH` is another version. Without
`dolt` those tests skip with a message, and the rest still run; a skipped
store test proves nothing, so install Dolt before changing the store,
server or protocol. With `ARIES_REQUIRE_DOLT=1`, as CI sets it, a missing
`dolt` fails those tests instead of skipping them. Tests that need `git`
skip without it too. A server `dolttest` starts does not outlive the
process that started it, on Linux and macOS: a test binary that is killed
(a `go test` timeout, a canceled run, a stopped agent) runs no deferred
`Stop`, so each launch has a watcher, a `/bin/sh` that kills dolt and
removes its files from the directory once that process is gone. Start
Dolt for a test through `dolttest`, never directly. A package's tests
take their servers from a `dolttest.Pool`: a test holds one, alone,
while it runs, and the pool runs one for every two processors, four at
most. Dolt holds every database
in memory, about 14 MB with Aries's schema, and opens each new one
more slowly the more it holds. So a test's database is dropped when the
test ends (the store's and `cmd/ariesd`'s `newDSN`, the server's
`newDatabase`, bdimport's `newStore` and the e2e world's, over
`dolttest.Pool`): get a test's database from those, not from
`NewDatabase` directly.

**Tests.** A test begins with `t.Parallel()`, or with a comment
`// Serial:` and the reason it cannot run beside another; a test in
`internal/clienttest` fails until each has one. A test run leaves
nothing behind, and `scripts/leftovers.sh` checks that.
[CONTRIBUTING.md](CONTRIBUTING.md#writing-a-test) says what makes a test
serial and where a test's files, processes and databases go, and how to
run the tests [on a slow machine](CONTRIBUTING.md#tests-on-a-slow-machine)
and what a test that passes there does not do.

## Rules

1. **Nothing organization-specific** in code, tests or docs: no real host,
   fingerprint, path, person or client name. Use `example.com`, `alice`,
   `bob` and invented UUIDs (design principle 10).
2. **No secret in argv, logs, output or the repository.** `ariesd` refuses
   a DSN password on the command line, and a config file holding one must be
   mode 0600. Follow that pattern for anything new. **And no text in
   telemetry:** a span carries ids, codes and small numbers, never an
   issue's or a memory's text, a comment, a key, a DSN, a token or an
   error's message (design §16.4). `internal/telemetry` enforces it for
   whatever a caller passes, so start spans through it and add a key
   only with a rule for its values. A log record exported over OTLP is
   held to the same: a log call's message is a string literal, its
   first field is `"event"` with a name from `internal/logs`, and a new
   field is added to `internal/logs/schema.go` with its kind, which
   says whether it may be exported; tests fail otherwise. Log with the
   request's context (`log.ErrorContext(ctx, …)`), which is what puts
   `req` and the trace ids on the line.
3. **Agents never handle release signing keys.** Releases are signed by the
   maintainer's gated `release` environment ([RELEASING.md](RELEASING.md)).
   Do not edit `internal/release/keys.go` or the release workflow's signing
   steps unless the maintainer asks.
4. **Keep the binaries small and auditable.** Standard library first; a new
   dependency needs a stated reason in the pull request. Release builds are
   `CGO_ENABLED=0`, `-trimpath`, `-ldflags "-s -w"`, so code must build
   without cgo. Pin GitHub Actions by commit SHA with a version comment.
   The OpenTelemetry SDK is `ariesd`'s alone (design §16.2, decision
   28): only `internal/telemetry` imports it, its exporters or the
   Prometheus client, `aries` links none of it, and tests enforce both. Use
   that package's interface from anywhere else.
5. **Store writes go through `Store.write`**, on the store's single writer
   connection, one SQL transaction per operation. There is one store, and
   so one writer, for each database: each project's, and the control
   database's (`Control.write`). Do not open another writer on a
   database or write to Dolt from anywhere else; the daemon's registry
   never opens a project's store beside one still closing. A write that changes an
   issue (its fields, status, handoffs or acceptance) first calls
   `wtx.guard` with the issue's claim (`store/authz.go`), so only the
   holder's principal or an admin may change an issue someone holds.
   `ImportIssue`, which only the operator's `ariesd import-bd` reaches,
   is exempt: it writes an issue whoever holds it, and ends the claim,
   telling the holder, when it leaves the issue anything but in progress
   with the holder (closed, open, blocked or reassigned). `ReapClaims`
   and `EndClaims`, which only the daemon reaches, are exempt too: they
   end a claim whose lease ran out, or whose holder lost access to the
   project, and return its issue to open. The control
   store's equivalent is `ControlOptions.Operator`, which only `ariesd
   control init` sets: it lets the reserved principal `import` write as a
   server admin. Who is an admin of a project's issues is `Store.IsAdmin`:
   `Options.Admins`, and in a server of many projects `Options.IsAdmin`,
   which the daemon answers from the control store.
6. **Every `UPDATE` sets `write_id = ?`** to a value unique to that write.
   Dolt detects conflicts per cell, not per row, so `rev = rev + 1` alone
   lets two concurrent writers both succeed silently (stage 0,
   summarized in `spike/postgres/RESULTS.md`). `wtx.exec` refuses an `UPDATE` without it.
   `SELECT ... FOR UPDATE` does not lock in Dolt; do not use it.
7. **Every mutation appends an event** with `wtx.event`, in the same
   transaction. `writeOnce` refuses a transaction that changed rows but
   recorded no event. Only bookkeeping that is not history skips the
   event, by setting `wtx.quiet`: lease renewals, registry touches, inbox
   acks and pruning. Within a write that also makes history, a
   statement that is bookkeeping runs through `wtx.touch` instead, which
   exempts only that statement: refreshing the time of an issue's paths
   already recorded (a client resends what git shows every few minutes;
   only a new path is history, and it records an `issue.paths` event),
   and deleting a principal's oldest forgotten memories to make room for
   a new one under `memories_per_scope`. The
   event log is the truth; ready, blocked and digest are derived from it
   and never stored. The event of a write that changes an issue's row is
   recorded with `wtx.issueEvent` (or `wtx.revEvent` for a create), which
   puts the rev the write left on it: a replayed update reads the state
   at the rev it was based on from those, and parks what changed since
   (`store/conflicts.go`).
8. **Write closures must be safe to rerun.** A write that loses to a
   concurrent transaction is retried from the start.
9. **Migrations are append-only**, in both sets. Add the next
   `NNNN_name.sql`; never edit, renumber or delete one that has merged. They only go forward: an older
   `ariesd` refuses a database a newer one migrated (`ErrSchemaTooNew`).
10. **The protocol is versioned.** Frames are NDJSON (`internal/proto`). The
    server decodes `*Args` with `DisallowUnknownFields`, so a new argument
    field, or a new op, is refused by an older server. Treat either as a
    protocol change: raise `proto.Proto` and `ProtoMax`, and keep `ProtoMin`
    at the previous version, so new servers still accept old clients
    (design principle 9). The server must then work when an old client
    omits the new field. After the bump, a new client against an old
    server is refused at the handshake with code `version` (exit code 3)
    and told to have the server upgraded, rather than failing on its
    first request. Clients ignore unknown result fields and frame
    types, so adding those needs no bump. A version no release has
    carried takes further changes without another bump; the protocol is
    settled at the next release (design decision 78). Keep the protocol
    number out of routine output.
11. **Every refusal says what to do next.** Return a `proto.Error` with a
    typed `Code`, a message naming the cause and a `Fix` naming the next
    action (`proto.Errf(code, fix, msg)`). Match `internal/server/errors.go`.
    The CLI prints the message and a `fix:` line; `internal/mcpserver`
    rewords the fix as the agent's next tool call (`errors.go`).
12. **MCP is for agents and stays small.** No admin tools over MCP: import,
    export, setup, upgrade, settings, prices, plans, projects, principals,
    members, keys and the server's status are CLI only, and so are passing,
    waiving and rearming a gate, a person's act from the session `cli`, and
    reporting a ci gate's check, a CI job's from the session `ci` (agents
    may add and list gates). An agent refused for want of access is told
    to ask a person, never to run an admin command
    (`internal/mcpserver/errors.go`).
    Identity is implicit: the principal comes from the SSH key and the
    session from the environment, never from a tool argument. A trace
    context is the request's metadata (`_meta`), never an argument. Results are capped
    (`budget.go`: 2,000 tokens, prime and digest 1,500) and the whole tool
    schema set at about 3,000 estimated tokens (decision 76); the tests enforce both and
    pin the tool list. Keep schema descriptions terse.
13. **Output stays terse.** Writes return `{id, rev}`; lists return the
    compact summary. `aries --json` prints exactly one JSON document, errors
    included. Exit codes: 0 ok, 1 failure, 2 usage, 3 protocol refused.
14. **Platforms.** `aries` runs on Linux, macOS and Windows, so every change
    must build for all three. `ariesd serve` is Linux-only, because only
    Linux can check the socket peer's user (`peer_linux.go`); `--dev`
    overrides that on a single-user machine.
15. **Go style.** `gofmt` and `goimports`, table-driven tests, errors
    wrapped with `%w`. Library code in `internal/` never touches
    `os.Stdout`, `os.Stderr` or `os.Stdin`; it uses the writers it is given
    (`cli.Env`). Only `main` packages wire up the process's streams. Panic
    only on a programming error that tests catch, and say so in a comment.
16. **American English** in code, comments and docs: "behavior", "color",
    "organization", "license" (noun and verb), "-ize".
17. **Keep the docs true.** A change in behavior updates the doc that
    describes it in the same pull request: the guide in `docs/` that
    covers it ([CONTRIBUTING.md](CONTRIBUTING.md) lists which),
    `RELEASING.md`, or the design doc. Where the build departs from the
    design, add to that section's dated **As built** note in
    `docs/design/aries.md` rather than rewriting the plan.
18. **Bound what a client can grow.** Anything a principal can add to
    without limit (rows, items, connections, writes) gets a cap in
    `store.Limits`, `store.ControlLimits` or `server.Limits`, with a
    default, a `limits:` setting, a table-driven test and a row in the
    Limits table in `docs/server.md`.
    A refusal past a cap is `invalid` or `busy` with a `Fix`. Tests run
    against a locked-down Dolt account (`dolttest`), as `ariesd` does.
19. **Time comes from the store.** The server's clock is relied on and
    not trusted to be right (design §14, decision 37). Anything the
    store writes or compares takes its time from `wtx.now` or
    `Store.now`, never from `time.Now`: that time does not go backward,
    and does not jump forward until the store has added the jump to
    every live lease (`store/clock.go`). A write takes its time once it
    holds the writer, and settles a jump it finds there first
    (`writeOnce`), so nothing that begins after a jump is dated before
    it; a write path that takes its time anywhere else breaks that. A
    daemon gives every store it
    opens the one `store.Clock` of its process. A test that builds
    history at several times starts its clock before the earliest and
    only moves it forward. `time.Now` is for measuring how long
    something took, and for the server's own timers, which run on its
    monotonic reading.

## Agent specifications

`go-engineer` is a test-first Go engineer and reviewer for changes in this
repository, in each harness's format: `.claude/agents/go-engineer.md`
(Claude Code), `.codex/agents/go-engineer.toml` (Codex) and
`.gemini/agents/go-engineer.md` (Gemini CLI). Delegate Go work and reviews
to it. The three files share one prompt, name and description; edit all
three together, and `internal/agentspec` fails until they match. A spec
adds working method only; repository facts and rules belong here.

## Git

- `main` is protected: squash merges through a pull request and the merge
  queue, which runs `lint` and `test` on each pull request as it would
  merge (the `merge_group` event in `ci.yml`), so a branch need not be
  brought up to date by hand.
- Branch with a type prefix: `feature/`, `fix/`, `maintenance/`, `docs/` or
  `refactor/`, then lowercase kebab-case.
- Title the pull request the same way: `fix: …`, `feature: …`.
- Commit subjects are imperative; the body says why. Note "No new
  dependencies" or justify the one you added.
- A pull request's description follows
  `.github/pull_request_template.md`: why, what, tests, the gate with
  what was not run, docs, dependencies.
- **Stacked pull requests** (one whose base is another's branch). `main`
  takes squash merges, so the base's commits never reach `main` as
  themselves:
  - Before the base's branch is deleted, retarget the stacked pull
    request to `main` (`gh pr edit N --base main`). Deleting a base
    branch with `gh` or the API closes the pull requests stacked on it,
    and a closed pull request cannot be retargeted.
  - Then bring the stacked branch up to date. If it was never pushed,
    replay its own commits onto `main`: `git rebase --onto origin/main
    OLD`, where OLD is the last commit of the base that the branch was
    built on (`git merge-base BASE-BRANCH HEAD`). If it was pushed,
    merge instead, keeping the branch's side wherever the squash
    conflicts with the commits it replaced: `git merge -X ours
    origin/main`. Then check the result: `git diff HEAD^1 HEAD` is what
    the merge changed, and must be only what `main` gained;
    `git diff origin/main HEAD` is the pull request, and must be only
    the branch's own work.
- Do not push, open pull requests or merge unless asked.
