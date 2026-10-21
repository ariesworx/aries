# Agent instructions

Canonical instructions for any AI coding agent working in this repository
(Claude Code, Codex, Gemini CLI, Cursor, GitHub Copilot and others). Codex,
Cursor and most other agents read this file directly. `CLAUDE.md` and
`GEMINI.md` import it, and `.github/copilot-instructions.md` points to it.
Put shared guidance here, not there.

## What this is

Aries is a public, AGPL-3.0-only Go module: an issue tracker, shared memory
and coordination layer for AI coding agents, stored in Postgres. `aries` is the
client (a CLI for people and an MCP server for agents); `ariesd` is the
server daemon and the sshd forced-command bridge. Bearings, the orchestrator,
is a design only ([docs/design/bearings.md](docs/design/bearings.md)); no
Bearings code exists yet.

Read [README.md](README.md) for the overview, the roadmap and the map of
the guides in `docs/`, and [docs/design/aries.md](docs/design/aries.md)
for the principles and architecture. The README's status line says which
stage is in progress.

<!-- aries:begin -->
## Issue tracking: aries
Track work with the `aries` MCP tools; never shell out to `aries` for it.
Call `prime` when a session starts, unless its output is already in context; then `start` takes an issue and `finish` closes it with a handoff note.
<!-- aries:end -->

This repository is tracked in Aries by those who run a server for it.
`.mcp.json`, `.claude/settings.json` and the block above are what
`aries setup claude-code` writes, with the block here rather than in
`CLAUDE.md`, which imports this file: `aries setup claude-code --check`
therefore reports `CLAUDE.md`'s pointer missing, and `--write` would add
a second. They need `aries` on `PATH` and a `.aries.yaml`, which this
repository does not commit: each contributor keeps their own, untracked,
at the top of their checkout, and a worktree reads the main worktree's.
Without one, the hooks print nothing and the `aries` tools refuse,
saying that no `.aries.yaml` was found: work without them. A
contributor who keeps the backlog on the Docker test server rebuilds it
as [docs/deploy/docker.md](docs/deploy/docker.md#rebuild-a-server-you-work-on)
says: a dump first, the same variables, and the CA checked after.

## Layout

| Path | Holds |
|---|---|
| `cmd/aries` | `aries` entry point: builds a `cli.Env` from the process and calls `cli.Run` |
| `cmd/sextant` | `sextant` entry point: hands `sextant.Command` the process's streams, environment and version. A binary of its own, which `aries` links nothing of (decision 98) |
| `cmd/ariesd` | `ariesd` entry point: `serve` (which, on a server of many projects, also writes the keys file sshd reads), `stdio`, `upgrade`, `status` (the running daemon's report, over its socket), and the admin `control init`, `import-bd` and `export-bd`. `serve` also wires the telemetry, the loopback listener and systemd's notifications (`runDaemon`) |
| `internal/store` | Typed store over Postgres, a schema for each project: single writer, CAS updates, event log, ready and blocked queries; and conflicts (`conflicts.go`, design §18.7): a replayed update compared field by field with the state at its rev, what others changed parked for a person to resolve, and agents' proposals. Also `Control` (`control*.go`), the same machinery over a schema of its own for what is the server's: projects, principals with their kinds, their public keys and memberships with roles, their identities and invitations (`control_identities.go`, identity.md §3), and the in-memory index the daemon reads at each handshake and each project store asks for its admins; sign-ins (`control_signin.go`) and the trust rules by which a service principal signs in with its platform's token (`control_trust.go`, identity.md §7). `stats.go` holds what `aries admin status` reads from memory: a store's write counters, and the control index's counts. `observe.go` holds the two hooks the daemon measures and traces a store through, `Observer` and `Tracer`; the store imports nothing of telemetry. `db.go` holds `DB`, the server's one pool of connections, which every store's writer and readers take a connection from for each transaction, each set to the store's schema; `fence.go` the writer's lock of each schema, and `db_stats.go` Postgres's own figures and the schemas' sizes for the status report, read with ariesd's role. `ProjectSchema` (`control_projects.go`) is the rule a new project's schema is named by. `postgres.go` checks the server's version (16 or later) and the schema at open, `account.go` refuses a role that may do more than its own database (`--allow-unsafe-account` in development), and `errors.go` says which SQLSTATEs are retried. `dsn.go` holds what the daemon knows of a DSN and the SQL that makes and hands over a schema, so that the store alone imports the database driver, pgx (`internal/server`'s `boundary_test.go` holds it and `cmd/ariesd` to that) |
| `internal/store/migrations`, `internal/store/controlmigrations` | Embedded schema migrations for a project's schema and for the control store's, two separate sets in Postgres's dialect: `NNNN_name.sql`, each numbered 1..n without gaps. Both began again at `0001_init.sql` when the store moved from Dolt (decision 79) |
| `internal/proto` | Wire protocol: NDJSON frames, handshake, `Op` constants, `*Args` and `*Result` types, typed errors; and the bridge frame, which passes between two `ariesd` processes on the server, carries no version and only ever gains fields |
| `internal/server` | Daemon: socket, peer check, sshd bridge, the HTTPS listener (`https.go`: `/.well-known/aries`, the WebSocket upgrade on `/v1/connect`, the key proof, its caps and the hand-off of each connection to `Server.handle`; `signin.go`: the authorization server of `login: providers`, its `/oauth/*` paths, tokens and their check with DPoP on the upgrade, and the close at a sign-in's end; `exchange.go`: a workload's token exchanged at `/oauth/token` (RFC 8693) for a sign-in of scope `ci`, by `workload: providers`, and `trust.go`, the `trust.*` ops; `scopes.go`: each op's scopes, which a test holds to every op; `tls.go`: certificates from files, ACME or a proxy) and the `transports:` setting, with SSH off by default, dispatch of ops to the connection's project's store, error mapping, settings; with `control_schema`, the membership check at the handshake, the registry of open project stores (`registry.go`), the `aries admin` ops over the control store (`admin.go`), the closing of connections whose access ended (`conns.go`), keys for sshd (`keys.go`): the `authorized_keys`-format file it writes from the control store (`Server.WriteKeys`), the hand-maintained `authorized_keys` it only reads, and the ending of claims whose holder lost access; keys' ends (`expiry.go`): the timer that closes a key's connections at its end, and the refusal of a key past it; and the report `aries admin status` reads (`status.go`), with Postgres's own figures through `Postgres` (`stores.go`), which `*store.DB` satisfies. It holds its stores through interfaces by area (`stores.go`): `Store` for a project's and `Control` for the control store, which `*store.Store` and `*store.Control` satisfy; a test of the server's own logic, not of the database, uses the fakes in `fake_test.go` |
| `internal/client` | The connection to the server: over HTTPS by `server.url` (a WebSocket, with the system's roots and `server.ca`), presenting a sign-in kept in the data directory (`signin.go`; `login.go` has `aries login`'s steps) or else proving a key, or over in-process SSH with a pinned host key, over TCP or a Google Cloud IAP tunnel, chosen in `transport.go` (`ARIES_TRANSPORT=ssh` pins SSH); which key it offers (`ARIES_KEY_FILE`, or `key:`, and then no other; with neither, the default identities of `~/.ssh` and then ssh-agent's keys); `.aries.yaml` discovery, and the one printed for a new project; session ids |
| `internal/wsconn` | WebSocket (RFC 6455) on the standard library: the upgrade on each side, one message per NDJSON frame, caps on a message's size, pings, and `Stream`, a `net.Conn` over it; fuzzed (`FuzzRead`) |
| `internal/jose` | Compact JWS on the standard library: RS256 and ES256 alone (`none`, HMAC and `crit` refused), JWKs and their thumbprints; fuzzed (`FuzzParse`) |
| `internal/oidc` | The verifier of identity providers' tokens (transport.md §8): a provider's settings (`login: providers`, kinds `oidc` and `entra`, the latter with no default authority), discovery read at the first sign-in or every endpoint written out, keys fetched from a configured URL and cached or read from a key set file with nothing fetched, the authorization request and the server-side code exchange; and workload providers (`workload.go`): GitHub Actions', Google Cloud's and Azure's tokens, each kind's required claims, and the audience bound to a job's DPoP key. Nothing in it names a provider's host. `oidctest` is a stand-in provider in process for tests; fuzzed (`FuzzVerify`) |
| `internal/dpop` | DPoP (RFC 9449) with ES256: aries's proofs, and the server's check of one against its configured URLs, with the proofs seen remembered, bounded, for their window; fuzzed (`FuzzVerify`) |
| `internal/keyproof` | The key proof of a connection over HTTPS (docs/server.md, How aries signs in over HTTPS): the offer, the challenge and the signed transcript, bound to the server's configured URL, never the request's `Host`; fuzzed |
| `internal/safetext` | Unsafe characters (controls, bidi): the store's validation, bd import cleaning, and the escaping of everything clients print |
| `internal/lazyregexp` | Regular expressions in package variables, compiled on first use, so that a process does not pay at start for those it never uses: sshd starts an `ariesd stdio` for every connection. Use it, not `regexp.MustCompile`, for a package variable of a package `ariesd` links; `TestInitAllocations` (`cmd/ariesd`) bounds what initialization allocates |
| `internal/logs` | The schema of ariesd's log (design §16.4), on the standard library alone: every field a line may have, with whether it may leave the machine (`schema.go`), the stable name of every event (`events.go`), a request's id, and the `slog.Handler` that puts a request's id and its span's ids on each line from the line's context and hands every line to a tap, which is how lines are exported. Its tests hold every log call of the daemon's packages, and the tables of docs/server.md, to the lists |
| `internal/telemetry` | ariesd's metrics, traces and exported log records, and the only importer of the OpenTelemetry SDK, its exporters and the Prometheus client (design §16): the instruments and their fixed attribute values (`instruments.go`), spans with their fixed names, keys and values (`trace.go`, whose rule is that a span carries ids, codes and small numbers and never text), how a person appears on a span and the key of the stand-ins (`people.go`), log records over OTLP with only the fields a span could carry (`logs.go`), OTLP/HTTP push of all three from the `OTEL_*` variables, the Prometheus handler, the loopback listener with `/metrics`, `/healthz` and `/readyz`, and `Manual`, which reads the instruments, the spans and the exported log records in process for tests. A nil `*Telemetry` does nothing. `sextant.go` holds Sextant's exporters and instruments, on a meter provider of their own that holds none of ariesd's. Its tests fail if `cmd/aries` links any of the SDK or another package imports it |
| `internal/sextant` | Sextant X0 (docs/design/sextant.md, docs/sextant.md): `sextant.yaml` and its limits (`config.go`), the poller (`poll.go`), which reads each project through the ops `aries` sends (`prices`, `cost`) by key or as a service principal's workload sign-in, keeps a connection to each, counts each failure and clears the project's figures, the run loop with the listener (`run.go`), and the command (`cmd.go`). It exports through `internal/telemetry`'s `Sextant` and imports nothing of the SDK |
| `internal/sdnotify` | systemd's notify socket with the standard library: ready, stopping and the watchdog; a no-op off Linux and without `NOTIFY_SOCKET` |
| `internal/statustext` | The status report as text, shared by `aries admin status` and `ariesd status` |
| `internal/secretscan` | Text that looks like a credential (private keys, service and bearer tokens, JWTs, passwords in URLs and commands, secret assignments, long random tokens): the store's secrets lint for memories |
| `internal/cli` | `aries` commands, `aries admin`, `aries outbox` and `aries login` among them; writes only to the `Env` it is given. `outbox.go` has what a queued write prints, `--no-queue`, and the replay on a new connection; `cache.go`, what is kept on the way past, what a command answered from the cache prints, the reads refused offline, and `aries cache` |
| `internal/board` | `aries tui`, the live board: the model and its golden-tested frames, `Live` (event push, debounced rereads, reconnects), and the terminal loop over `golang.org/x/term` |
| `internal/credential` | A secret of aries's, the dispatcher's API key first (dispatcher.md §11): the macOS keychain through `security`, the Secret Service through `secret-tool` where one answers and otherwise a 0600 file of the user's in the data directory, and Windows Credential Manager, which refuses with a fix until it is built. The secret reaches commands on stdin alone: one function starts them and refuses an argv that carries it, and stderr is redacted before an error quotes it. `aries dispatch key` keeps the key in it, and `aries dispatch run` reads it |
| `internal/dispatch` | The client-side dispatcher's loop (docs/design/dispatcher.md, decision 96), internal and experimental, a package of its own so that Bearings' B0 replaces it whole; `internal/cli`'s `dispatch.go` only wires it to the process. Picking by the memory `dispatch`'s rules (`select.go`), the budget, CI-failure and auth stops (`stops.go`), briefs built from the issue (`brief.go`), the loop with claim before launch, reconcile, cleanup and the kill switch (`dispatcher.go`), its state file (`state.go`), and its parts over programs (`exec.go`: aries with `--json --no-queue` in the run's session, git, gh; `claude.go`: `aries lock run dispatch/runs -- claude -p`). macOS and Linux (`proc_unix.go`); it builds elsewhere and does not run. Its tests stand in for aries, git, gh and the runner, the test binary standing in for aries; none starts a harness |
| `internal/brief` | The brief a worker gets for an issue (docs/design/dispatcher.md §7, As built: the brief template): the project's template, `.aries/brief.txt`, read and checked (`Load`, `Parse`), its byte-stable rendering for a headless run or a Desktop session with the issue quoted as data, and the `brief:` record's first line, its parse and its hash, which the dispatcher, `aries dispatch brief` and `aries outcome` share. The repository's template is held to its version by `scripts/brief_test.go` |
| `internal/capture` | Token usage read from a harness's local records (Claude Code transcripts) for `aries usage --hook`, with per-file offsets in a 0600 state file |
| `internal/outbox` | The queue of writes made while the server cannot be reached (design §18.3, §18.4; stage 5, slice F1), as a library that knows nothing of connections: `internal/offline` is its one caller. Plain files in a 0700 directory its caller names: an NDJSON outbox synced on each append, the answers, and counts of what was queued and for how long. Which ops are queued (`entry.go`), the secrets lint before an entry is written, caps on entries, bytes and age, replay by one process at a time, the refused list with retry and drop. The locks are `flock` and `LockFileEx` through `syscall`, as F0 measured them; `doc.go` has the rules, why each is there, and what has not run on Windows. Its tests run real processes, some of them killed, against one directory |
| `internal/offline` | What `aries` and `aries mcp` do with a write that did not reach the server (design §18.3, As built): which errors say so is `client.Unreachable`; here are the data directory on each system and `ARIES_DATA_DIR`, the check that it is the user's own, the record of who the server took each key for (`seen.json`) and the rule from it to a principal's outbox, each refusal to queue with its code and fix, the replay of one principal's outbox on that principal's connection, and `prime`'s notice. And the cache of what a principal's sessions read (`cache.go`, design §18.6, As built for F3): which answers are kept and how they are keyed, the files and their caps, which reads are refused offline, and when the server was last reached. `doc.go` has the rules; nothing in it has run on Windows |
| `internal/mcpserver` | MCP tools, `prime`, token budgets, agent-facing error rewording; and the trace context a tool call brings in its request's metadata, which its requests then carry. With an outbox (`Options.Offline`), a queueable tool call that does not reach the server is queued and answered in one line (`offline.go`), and each new connection sends what waits first. With a cache (`Options.Cache`, `cache.go`), what a tool call reads is kept, and with the server away `prime`, `show`, `inbox` and `digest` answer from it, saying so, while `ready`, `blocked`, `list` and `who` are refused |
| `internal/agentsetup` | `aries setup AGENT` and `--all`: per harness, the MCP config, pointer block, SessionStart hook (and Claude Code's usage hooks) and the hook's output format; per-project entries in a desktop app's config (`desktop.go`); `ARIES_KEY_FILE` in an entry of the person's own files (`--key-file`), never a project's |
| `internal/clienttest` | `Isolate`, which keeps a test binary from the developer's `~/.ssh`, ssh-agent, key variables and outbox (it sets `ARIES_DATA_DIR`); every package whose tests connect calls it from `TestMain`. `IsolateGit`, which keeps the git a test binary starts from the developer's git configuration. `DeadPort`, the address of a loopback port that resets every connection and that no other test can take, for tests of a collector that is down (a closed port is handed to another test's listener). `SilentPort`, a listener that accepts and never answers and is closed when the test ends. Its tests read the module's tests: each is parallel or says why not |
| `internal/gitx` | Branch names from issues, issue ids from branches and `Aries:` trailers; what a commit says of its issue (`IssueOfMessage`), the default branch, the commits since one (`Since`) and a hook's path, for `aries land` |
| `internal/bdimport` | bd JSONL import and export, over the `Source` and `Target` it needs of a project's store |
| `internal/release` | Release download, signature and checksum checks, binary swap. A release is read from one of two sources behind one interface: GitHub's API (`release.go`), or a static tree on any HTTPS file host or in a directory (`static.go`, with `index.json` in `index.go`), chosen by `DefaultSource`, `ARIES_RELEASES` and `release.Open`. The source is transport and is never trusted: `Fetch` accepts a release on its signature and checksum alone, a static tree's tags are checked before they reach a URL, and no redirect from one is followed. `releasetest` serves a fake of each source |
| `internal/depcheck` | Tests only, of the dependency policy (rule 4) against `deps.allow` at the root: `go.mod`'s and the tools module's direct modules both ways, each binary's linked modules on the systems the release builds against `testdata/links-aries.golden` and `links-ariesd.golden` (`-update`), and Sextant's, held to ariesd's scope, against `links-sextant.golden`, with no package of Sextant's in `aries`, and each module's scope, every workflow's `uses:` (by owner/repo, pinned by commit with a version comment; `./` is exempt), and no `go run` or `go install` of `pkg@version` in a workflow or script; every workflow job has a `timeout-minutes`; and `TestCooldown`, which `.github/workflows/deps.yml` runs with `ARIES_DEPS_BASE` set to a pull request's base (it skips without it): a module version the branch takes was published 14 days before, 30 for a new major version, unless the pull request is labeled `security-fix` |
| `internal/tools/pin` | A module of its own, never built into anything, whose `go.mod` and `go.sum` pin the gate's Go tools: govulncheck, as a `tool` line. Not `internal/tools/go.mod`, which would take `releasekey` and `releaseindex` out of the main module. golangci-lint is not here: its Action installs it by version |
| `internal/tools/releasekey`, `internal/tools/releaseindex` | The signing tool; and the tool that lays a release's files out as a static tree and writes its `index.json`, which signs nothing and needs no secret ([RELEASING.md](RELEASING.md#publishing-to-a-static-host)) |
| `internal/tools/cla` | The contributor license agreement check that `cla.yml` runs from the base branch (decision 94): who must sign a pull request, its one comment, and signatures recorded on the `cla-signatures` branch, through GitHub's API with the workflow's token alone. Tested against a fake API in memory |
| `internal/tools/trailer` | The `Aries:` trailer check that `trailer.yml` runs on pull requests (CONTRIBUTING.md, "The `Aries:` trailer"): the commits from the event's base to its head, merges left out, each read by `internal/gitx`'s `IssueOfMessage` as `aries land` reads it; it fails unless one names an issue and all name the same, and the label `no-issue` exempts a pull request. Reads git and the event file only. Tested with commits it is given and with a real repository |
| `internal/version` | Build version, and `Postgres`, the pinned Postgres release, its major version and the oldest the store opens, with the SHA-256 of its source archive (`postgres.sha256`); its tests keep every copy of a tool pin in step |
| `internal/pgtest` | Starts a throwaway Postgres for tests, in a temporary directory with a unix socket and no TCP port, with a superuser only it uses and the locked-down role `aries` the tests connect as, and drops a test's database when the test is done with it. `Pool` is a test binary's servers: each test holds one alone while it runs, so tests run in parallel |
| `internal/iaptest` | A fake `gcloud` for tests of the IAP transport: the test binary, re-executed from `PATH` |
| `internal/e2e` | End-to-end tests: CLI and MCP through an in-process SSH server to a real daemon and store, or with `ARIES_E2E_TRANSPORT=https` over the daemon's HTTPS listener (`https_test.go`; the gate runs both). The SSH server can read the keys file the daemon writes, before its own keys, as sshd reads its `AuthorizedKeysFile`. `TestSoak` (`soak*_test.go`) is the gate of stages 3 and 3a ([CONTRIBUTING.md](CONTRIBUTING.md#the-soak-test)); with `ARIES_SOAK_TELEMETRY` it runs with the daemon's telemetry on, and holds the instruments, the spans and the audit stream to the event logs or gives the daemon a collector that is not there (`soak_telemetry_test.go`); with `ARIES_SOAK_PARTITION` it is the gate of stage 5, cutting agents off from the server for minutes at a time and holding every write they queued to their outboxes and the event logs (`soak_partition_test.go`) |
| `deploy/docker` | The Docker test server ([docs/deploy/docker.md](docs/deploy/docker.md)): image, entrypoint, compose file and `smoke.sh`, which drives it end to end and is not part of the gate (`.github/workflows/smoke.yml` runs it in every mode nightly on `main`): over HTTPS with a test CA by default, and over SSH too with `ARIES_SSH=1` and `ARIES_SMOKE_SSH=1`. It mirrors docs/server.md's setup steps, with ariesd's Postgres in a container of its own (the official image, pinned by digest, its major version held to `version.PostgresMajor` by a test in `scripts`), reached as a Postgres on another machine, with a generated password kept in 0600 files. Telemetry is off unless asked for: `ARIES_TELEMETRY=1` turns ariesd's on, its log export included, and the compose profiles `telemetry` and `prometheus` run a Collector and a Prometheus, pinned by digest, in the server's network namespace, since what they read answers on loopback alone; `ARIES_SMOKE_TELEMETRY=1` has `smoke.sh` check them. The profile `identity` runs Keycloak, pinned by digest, as a local identity provider for developing and testing sign-in, also on a network with no route out: `identity/identity.sh` makes its realm, users and clients idempotently, with generated passwords and secrets kept 0600 on its volume, and `ARIES_SMOKE_IDENTITY=1` has `smoke.sh` check it. The profile `proxy` runs nginx, pinned by digest, in front of ariesd (`ARIES_PROXY=1`), and `ARIES_SMOKE_PROXY=1` runs all of `smoke.sh` through it. A test fixture, never production |
| `deploy/observability` | Examples that are tested, for operators (design §16.7): an OpenTelemetry Collector's configuration for metrics, traces and logs, alert rules in Prometheus's format, a Prometheus configuration that loads them, and three Grafana dashboards (the server, requests by op, traces and logs) with their provisioning for the Docker test server's `observe` profile. Vendor-neutral (rule 1). Tests in `internal/telemetry` (`harness_test.go`) fail if a rule or a query names a metric, a label or a value ariesd does not have, so change them with the instruments |
| `deploy/apparmor` | AppArmor profiles for `ariesd` on Ubuntu, and their tunables. A change to what `ariesd` reads, writes, connects to or runs changes them too |
| `scripts` | For contributors, in POSIX shell: `gate.sh` runs the gate (Commands, below), and `install-postgres.sh` gives the packaged Postgres of the pinned major version on Linux, or else builds the pinned release, verified, under the user's cache directory. `leftovers.sh` runs a command, the tests usually, with a temporary directory of its own and fails if it leaves a file there or a process running; the gate's tests run under it. The tests beside them run all three against stand-ins; and `TestNoOldName` fails if the path or the text of any file in the repository names the project by a former name, with no exception |
| `plugins/aries-ops`, `.claude-plugin/marketplace.json` | The operations skill, a Claude Code plugin, and the marketplace file that lists it ([Operate a server with Claude Code](docs/server.md#operate-a-server-with-claude-code)): `SKILL.md` holds the workflow, the safety rules and a map of `references/*.md`, one per area, each step citing the guide's section it follows. It runs on an operator's workstation and drives the installer, `aries admin` and `ariesd` over the operator's SSH; it never handles a secret. Anything specific to the database belongs in `references/database.md`, which holds the Postgres steps, each citing the guide's section. `scripts/opsskill_test.go` holds its commands, flags and citations to the code and the guides, and its manifests and frontmatter to what Claude Code reads. It has no version: an operator adds the marketplace at a release's tag |
| `mise.toml` | Go, golangci-lint and shellcheck versions for contributors who use mise; optional |
| `install-server.sh` | The server installer: does docs/server.md's setup, for a server of many projects, on a fresh Ubuntu 24.04 LTS or 26.04 LTS machine, as root, each step checking before it acts; `--dry-run` changes nothing. It installs the binaries through `install.sh`, holds no release key, and installs Postgres from the PostgreSQL project's apt repository, checking its key's fingerprint, or uses a Postgres already on the machine, or one `--dsn` names. `scripts/installserver_test.go` fails when its steps, the text of a file it writes or the SQL it runs differ from the guide's ([With the installer](docs/server.md#with-the-installer)), or its Postgres pins from `internal/version` and CI's |
| `install.sh` | The one-line installer for `aries` and `ariesd`, from GitHub or from a static tree (`ARIES_RELEASES`); `internal/release/install_test.go` and `install_static_test.go` run it against each, and tests check that its key is in `keys.go` and its `DEFAULT_RELEASES` is `release.DefaultSource` |
| `spike/outbox` | Stage 5 (slice F0) measurements of the local store of design §18.4 under many processes: file locks, the outbox, replay, compaction and the cache. Kept as evidence; not maintained and excluded from lint |
| `spike/postgres` | Measurements of the store on Postgres beside Dolt, a module of its own with a copy of the store at the time, kept as the evidence for decision 79 (design §14). Not maintained, excluded from lint, and no longer builds: it imports `internal/dolttest`, which is gone |
| `spike/identity` | Sign-in and service principals against a local Keycloak and a real sshd (decision 83, `docs/design/identity.md`): the loopback, URL and device flows, a verifier of ID tokens on the standard library, trust rules for workload tokens, and how sshd reads `expiry-time`. A module of its own, kept as evidence; not maintained and excluded from lint |
| `spike/https` | HTTPS as the transport (decision 86, `docs/design/transport.md`), run on a Docker network with no route out: a WebSocket on the standard library carrying `internal/proto`'s frames, DPoP-bound tokens from an authorization server that signs people in through Keycloak, workload token exchange, a remote MCP endpoint, an Entra provider with a configured authority, and the pipes measured through nginx and against SSH. A module of its own that imports `internal/proto` and `spike/identity`, kept as evidence; not maintained and excluded from lint |
| `docs` | User guides: concepts, `aries`, agents, CI gates and their workflows, running a server, deployment examples, install, moving from bd, the security model |
| `docs/design` | Design documents |
| `.claude/agents`, `.codex/agents`, `.gemini/agents` | Agent specifications (below); `internal/agentspec` keeps them in step |

## Commands

`scripts/gate.sh` runs the gate: the commands below, which are what CI
runs (`.github/workflows/ci.yml`), in this order. CI runs it on every
pull request, and the merge queue merges nothing it has not passed. On
your machine, run what the change calls for (**Test tiers**, below),
not the whole gate every time, and say in the pull request what you ran
and what you left to CI. This block is the list of record; a test in `scripts/` fails
when the script and the block differ, so change them together, and
`ci.yml` with them.

```sh
gofmt -l .                       # must print nothing
go vet ./...
golangci-lint run ./...          # CI pins v2.14.0; config in .golangci.yml
go mod verify && go -C internal/tools/pin mod verify      # module cache still matches go.sum
go mod tidy -diff && go -C internal/tools/pin mod tidy -diff  # go.mod and go.sum are tidy
git ls-files -co --exclude-standard -z "*.sh" | xargs -0 shellcheck
go tool -modfile=internal/tools/pin/go.mod govulncheck ./...  # pinned by go.sum
GOOS=darwin GOARCH=amd64 go vet ./...          # aries must build and vet
GOOS=darwin GOARCH=amd64 go build ./cmd/...    # on every developer OS
GOOS=windows GOARCH=amd64 go vet ./...
GOOS=windows GOARCH=amd64 go build ./cmd/...
ARIES_REQUIRE_POSTGRES=1 scripts/leftovers.sh go test -race -shuffle=on ./...
ARIES_REQUIRE_POSTGRES=1 ARIES_E2E_TRANSPORT=https scripts/leftovers.sh go test -race -shuffle=on ./internal/e2e/
```

With `--pgtest-lock` (or `ARIES_GATE_PGTEST_LOCK=1`), the script runs
each whole-suite `go test` line above under the Aries lock `pgtest/HOST`
(HOST the machine's name, capacity 2, waiting rather than failing), so
that agents in parallel on one machine do not run out of System V shared
memory; it needs `aries` on `PATH`, and without the flag nothing changes
([CONTRIBUTING.md](CONTRIBUTING.md#agents-testing-in-parallel-on-one-machine)).
The script stops at the first failure (`--keep-going` does not), and
away from Linux it also vets, builds and lints for Linux, which CI does
natively. The tests run under `scripts/leftovers.sh`, which fails a run
that passed but left a file in its temporary directory (or in
`GOTMPDIR`, where `go test` makes each `t.TempDir` when it is set) or a
process running, and names the test that probably did.
`scripts/gate.sh --quick` is for the loop between edits and is
not the gate: it skips `go mod verify`, `go mod tidy -diff` (which may
download modules), govulncheck and the builds for other systems, and
runs the tests without `-race` or `-shuffle`. The script sets
`GOTOOLCHAIN=local` and `GOFLAGS=-mod=readonly`, as CI has them.

**Test tiers.** Before a pull request, run what the change touches;
CI does the rest ([CONTRIBUTING.md](CONTRIBUTING.md#what-to-run-before-a-pull-request)
has the table):

- Docs, design and comments: `go test ./scripts/`, which holds the docs
  to the code.
- Code: the touched packages' tests with `-race`, and
  `scripts/gate.sh --quick`.
- Concurrency (claims, locks, leases, the outbox and its replay, the
  clock, the pool and retries): also `-count=3` on the touched tests,
  and one short soak.
- `deploy/docker`, the installer, AppArmor, `ariesd`'s start or the
  transport: also one run of `deploy/docker/smoke.sh` in the modes the
  change touches.
- A release candidate: all of it, with the long and partition soaks,
  `smoke.sh` in every mode, and the installer on a real machine.

Don't rerun a whole suite after a rebase whose conflicts were in docs
only: CI and the merge queue run it.

**Tools.** Go is at the `toolchain` line in `go.mod`, which CI installs
and the release is built with: on an older one govulncheck reports
standard library findings that are not the change's, which the script
says. A Go patch release moves that line, `mise.toml`, the tools
module's `go.mod` and the Docker image's together, and tests in
`internal/version` keep them equal. golangci-lint is at the version CI
pins, which `mise.toml` pins too. govulncheck is pinned by the tools
module, `internal/tools/pin`, whose `go.mod` and `go.sum` hold it and
nothing else; the gate runs it with `go tool -modfile=`. No tool is run
as `go run …@version` (`internal/depcheck`).

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
hashes of `go.sum` and the tools module's with no fallback; only a push to `main` saves one, a
pull request and the merge queue's run of one only read them, and any
other run, the release's gate among them, uses none. The tests' temporary files, Postgres's data
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
`deploy/docker/smoke.sh` is not part of the gate, and no pull request runs
it; `.github/workflows/smoke.yml` runs it in every mode every night on
`main`, and by hand, as a check that is not required.
The `release` workflow runs this whole gate (`ci.yml`, as a reusable
workflow) on the tagged commit before it builds anything
([RELEASING.md](RELEASING.md)).

**Postgres.** The store, server, bdimport, `cmd/ariesd` and e2e tests
start a real Postgres through `internal/pgtest`. They need `postgres`
and `initdb` on `PATH`, at the major version in `version.PostgresMajor`
(`internal/version`, which also holds `version.Postgres`, the pinned
release, and `postgres.sha256`, the SHA-256 of its source archive; tests
keep CI's `POSTGRES_MAJOR`, CONTRIBUTING.md and the release notes in
step, so change them together as
[RELEASING.md](RELEASING.md#changing-the-postgres-pin) says, and
`TestPostgresVersion` checks the `postgres` on `PATH`).
`scripts/install-postgres.sh` prints the directory of the packaged
Postgres of that major version on Linux when it is installed
(`/usr/lib/postgresql/MAJOR/bin`, from the PostgreSQL project's apt
repository, as CI installs it); otherwise, and always on macOS, it
downloads the pinned source archive, verifies its SHA-256, builds it
under the user's cache directory and prints that directory to put on
`PATH`. `scripts/gate.sh` uses it by itself when the `postgres` on
`PATH` is another major version. Without `postgres` those tests skip
with a message, and the rest still run; a skipped store test proves
nothing, so install Postgres before changing the store, server or
protocol. With `ARIES_REQUIRE_POSTGRES=1`, as CI sets it, a missing
`postgres` fails those tests instead of skipping them. Tests that need
`git` skip without it too. A server `pgtest` starts has its own data
directory and listens on a unix socket in it, with no TCP port; it runs
with `fsync` off, which loses nothing a test needs, since its data is
thrown away. It does not outlive the process that started it, on Linux
and macOS: a test binary that is killed (a `go test` timeout, a canceled
run, a stopped agent) runs no deferred `Stop`, so each launch has a
watcher, a `/bin/sh` that shuts the server down (an immediate shutdown,
which frees its semaphores and shared memory, and a kill if that does
not end it), removes its files and frees what a killed server left of
its semaphores and shared memory once that process is gone; while it
lives, the process frees them itself if the server is killed. Postgres refuses to run as
root, and so does `pgtest`. Start Postgres for a test through
`pgtest`, never directly. A package's tests take their servers from a
`pgtest.Pool`: a test holds one, alone, while it runs, and the pool runs
one for every two processors, four at most. A test's database is
dropped when the test ends (the store's and `cmd/ariesd`'s `newDSN`,
the server's `newDatabase`, bdimport's `newStore` and the e2e world's,
over `pgtest.Pool`): get a test's database from those, not from
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
   `bob` and invented UUIDs (design principle 10). The one exception is
   the copyright holder, Ariesworx LLC, named in the README's license
   notice (design decision 82).
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
   says whether it may be exported; tests fail otherwise. From v0.5.0 on,
   no event, field, instrument, attribute value or span is renamed or
   removed (docs/server.md, Names that are kept): a new one
   is recorded in its package's `testdata/names.golden` with `-update`,
   and `TestNamesKept` fails if a recorded one goes. Log with the
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

   **Admitting a dependency.** Every module `go.mod` requires directly,
   every tool the gate runs and every Action has a line in `deps.allow`:
   its scope (`aries`, `ariesd`, `both`, `tools` or `actions`), license,
   date and reason. A Go module is added only when the standard library
   would not do (the pull request says what was tried); its line is in
   the same pull request; its license is Apache-2.0, BSD, ISC, MIT or
   MPL-2.0 if a binary links it; it comes through the default `GOPROXY`
   and `GOSUMDB`; its name is the one its documentation gives, checked by
   a person; and a person reviews it. A tool is pinned by a `go.sum` (the
   tools module), an Action by commit. An agent may propose a dependency,
   saying where it learned the name, and never adds one, nor its
   `deps.allow` line. `internal/depcheck` holds `go.mod`, the tools
   module, the workflows and what each binary links
   (`testdata/links-*.golden`) to the file. **Cooldown:** a version is
   taken no sooner than 14 days after it was published (30 for a new
   major version, which `.github/dependabot.yml` waits for Go modules and
   a reviewer for Actions and images), for modules, tools
   and Actions alike, unless it fixes a vulnerability govulncheck or an
   advisory names; a security fix skips the wait, never the review. Go's
   own patch releases are taken within the week (docs/design/supply-chain.md,
   §7.2, §7.3).
5. **Store writes go through `Store.write`**, on the store's single writer,
   one SQL transaction per operation, on a connection it takes from the
   daemon's one pool (`store.DB`). There is one store, and so one writer,
   for each schema: each project's, and the control store's
   (`Control.write`). Do not open another writer on a schema or write to
   Postgres from anywhere else; the daemon's registry never opens a
   project's store beside one still closing, and between processes the
   writer's lock (`store/fence.go`), an advisory lock each write checks,
   refuses a second daemon, or `ariesd import-bd`, a schema another
   serves (`*store.ServedError`). Only `ariesd control init` writes the
   control store beside the daemon, without the lock; a store opened
   `ReadOnly` takes none and writes nothing. A write that changes an
   issue (its fields, status, handoffs or acceptance) first calls
   `wtx.guard` with the issue's claim (`store/authz.go`), so only the
   holder's principal or an admin may change an issue someone holds.
   `ImportIssue`, which only the operator's `ariesd import-bd` reaches,
   is exempt: it writes an issue whoever holds it, and ends the claim,
   telling the holder, when it leaves the issue anything but in progress
   with the holder (closed, open, blocked or reassigned). `ReapClaims`
   and `EndClaims`, which only the daemon reaches, are exempt too: they
   end a claim whose lease ran out, or whose holder lost access to the
   project, and return its issue to open. So is what a report of a
   check an issue's workflow requires does to that issue
   (`store/landed.go`): the closing pass, which closes it and ends the
   claim, the ticks and unticks of the items bound to the check, and a
   revert's reopen, each by the principal the gate names. The control
   store's equivalent is `ControlOptions.Operator`, which only `ariesd
   control init` sets: it lets the reserved principal `import` write as a
   server admin. Who is an admin of a project's issues is `Store.IsAdmin`:
   `Options.Admins`, and in a server of many projects `Options.IsAdmin`,
   which the daemon answers from the control store.
6. **An update names the rev it read.** A compare-and-swap is
   `UPDATE … SET …, rev = rev + 1 WHERE id = $n AND rev = $m`: Postgres
   locks the row, so of two writers the second, at the store's
   repeatable read, fails with a serialization error and is retried
   from the start (rule 8), and at read committed would match no row
   (`TestCASNeedsNoWriteID`). Queries are written with Postgres's `$n`
   placeholders, numbered in each query; there is no layer that
   rewrites them. A write retries SQLSTATE 40001 and 40P01, and 23505
   where a unique key was taken by a concurrent writer; a duplicate the
   caller asked for is its own error (`store/errors.go`).
7. **Every mutation appends an event** with `wtx.event`, in the same
   transaction. `writeOnce` refuses a transaction that changed rows but
   recorded no event. Only bookkeeping that is not history skips the
   event, by setting `wtx.quiet`: lease renewals, registry touches, inbox
   acks, pruning, and a sign-in's announcement in a project's inbox,
   whose history is the control store's `signin.grant`. Within a write that also makes history, a
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
   `NNNN_name.sql`; never edit, renumber or delete one that has merged.
   Each runs in one transaction with the row that records it. They only go forward: an older
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
    types, so adding those needs no bump. The protocol is settled at 8
    for v0.5.0, spoken alone (`Proto = ProtoMin = ProtoMax = 8`; design
    decisions 78 and 88): until v0.5.0 ships, a change joins 8 without a
    bump, and from v0.5.0 on the bump above applies to every change,
    since `aries upgrade` installs v0.5.0 and a released client must
    keep working against a newer server (decision 93). From v0.5.0 on,
    the protocol is raised once per release: the first change after a
    release raises `Proto` and `ProtoMax`, and later changes before that
    release ships join it, with `ProtoMin` at the last released version.
    Protocol 10 is v0.7.0's (`Proto = ProtoMax = 10`, `ProtoMin = 9`):
    `price.set` takes `tier`. Protocol 9 was v0.6.0's:
    `handoff` takes `ticked` and `waived`, and the HTTPS key proof is
    bound to the TLS session. v1.0.0 is held
    as long as possible, and the releases before it are minor versions.
    There is no step-down: a client or server of another version is
    refused at the handshake. Keep the protocol number out of routine
    output.
11. **Every refusal says what to do next.** Return a `proto.Error` with a
    typed `Code`, a message naming the cause and a `Fix` naming the next
    action (`proto.Errf(code, fix, msg)`). Match `internal/server/errors.go`.
    The CLI prints the message and a `fix:` line; `internal/mcpserver`
    rewords the fix as the agent's next tool call (`errors.go`).
12. **MCP is for agents and stays small.** No admin tools over MCP: import,
    export, setup, upgrade, settings, prices, plans, projects, principals,
    members, keys, invitations, identities and the server's status are CLI only, and so are passing,
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
    as a locked-down Postgres role (`pgtest`), as `ariesd` does.
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
