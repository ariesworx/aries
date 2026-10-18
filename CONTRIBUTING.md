# Contributing to Aries

Aries is an issue tracker and coordination layer for AI coding agents,
written in Go and stored in Postgres ([README](README.md)). This page gets
you from a fresh clone to a pull request. The rules every change follows
and the map of the code are in [AGENTS.md](AGENTS.md); they apply to
people and agents alike, so read them before your first change.

## License and contributor agreement

Aries is licensed under the GNU Affero General Public License, version 3
only ([LICENSE](LICENSE)). A contribution is accepted under a contributor
license agreement, which grants the maintainer the right to offer it
under other terms too, so that Aries can also be licensed commercially
(design decision 82). The agreement is [CLA.md](CLA.md), in effect from
15 October 2026 (decision 84). On your first pull request the `cla`
check posts it and fails until you sign, by commenting the sentence it
gives you; it then passes on that and every later pull request. To
contribute on behalf of a company, contact Ariesworx first (section 6.3
of the agreement).

## From a clone to a passing gate

The gate is the set of checks CI runs on every pull request.
`scripts/gate.sh` runs the same commands on your machine.

With [mise](https://mise.jdx.dev), which reads the tool versions from
[mise.toml](mise.toml):

```sh
git clone https://github.com/ariesworx/aries && cd aries
mise install        # Go, golangci-lint and shellcheck
scripts/gate.sh     # finds or builds Postgres, then runs what CI runs
```

Without mise, which is optional, install the tools below yourself, then
run `scripts/gate.sh`.

| Tool | Version | Used for |
|---|---|---|
| Go | the release the `toolchain` line in [go.mod](go.mod) names; the gate runs the Go on your `PATH` (`GOTOOLCHAIN=local`) and says when it is another | everything |
| golangci-lint | the `version:` CI pins in [ci.yml](.github/workflows/ci.yml), and mise.toml with it | lint, with the config in `.golangci.yml` |
| shellcheck | any recent; mise.toml names one | every `*.sh` in the repository |
| git | any recent | the gate, and tests of branch and worktree creation |
| Postgres | the major version `PostgresMajor` in [internal/version](internal/version/version.go), 18 | the store, server, bd import and end-to-end tests. `scripts/install-postgres.sh` finds or builds it |
| Docker | Docker Desktop, OrbStack, or Docker Engine 25 with Compose v2 | only the [Docker test server](#try-your-change-against-a-real-server) |

**Postgres** must be at the pinned major version; another one fails
`TestPostgresVersion`. The tests start servers of their own, each in a
temporary directory with a unix socket and no TCP port, so a Postgres
you already run is never touched, and none needs to be running.

- **On Linux,** install the PostgreSQL project's package
  `postgresql-18` from [its apt repository](https://wiki.postgresql.org/wiki/Apt),
  as CI does; the tests use its programs in
  `/usr/lib/postgresql/18/bin`. The package makes and starts a cluster
  of its own, which the tests do not need: put
  `create_main_cluster = false` in
  `/etc/postgresql-common/createcluster.conf` before installing it, or
  stop that cluster.
- **On macOS,** and on Linux without the package,
  `scripts/install-postgres.sh` builds Postgres 18.6 from the official
  source archive, after checking its SHA-256 against
  [internal/version/postgres.sha256](internal/version/postgres.sha256),
  into your cache directory (`~/.cache/aries/postgres-18.6`), once. It
  needs `make` and a C compiler (on macOS, `xcode-select --install`),
  takes a few minutes, and never builds over a Postgres you installed.

`scripts/install-postgres.sh` prints the directory of the programs,
whichever it found, and the gate runs it for you when the `postgres` on
your `PATH` is another major version. To run `go test` yourself, put
that directory first on your `PATH`:

```sh
export PATH="$(scripts/install-postgres.sh):$PATH"
go test ./...
```

Postgres refuses to run as root, and so the tests that need it fail as
root. Tests that need Postgres or git skip without them, and a skipped
store test proves nothing; `ARIES_REQUIRE_POSTGRES=1`, which the gate and
CI set, makes a missing Postgres a failure. [Commands](AGENTS.md#commands) lists the
gate command by command.

| Command | Does |
|---|---|
| `scripts/gate.sh` | The gate. Stops at the first failure, prints one line for each command, and ends with what the run did not cover |
| `scripts/gate.sh --keep-going` | The same, running every command whatever fails |
| `scripts/gate.sh --smoke` | The gate, then the [Docker smoke test](#try-your-change-against-a-real-server) |
| `scripts/gate.sh --quick` | Not the gate; for the loop between edits. Format, vet, lint, shellcheck and the tests without the race detector, so unchanged packages come from Go's test cache. It skips `go mod verify`, `go mod tidy -diff`, govulncheck and the builds for other systems |

**On Windows,** build and test `aries` natively with Go (`go build
./cmd/aries`, `go test ./...`). The scripts are for a POSIX shell, and
`ariesd serve` runs on Linux only, so run the scripts, the gate and
the server in WSL, where everything on this page works as on Linux.

## Try your change against a real server

The [Docker test server](docs/deploy/docker.md) runs sshd and `ariesd`
in one container, built from your checkout, beside Postgres in another,
set up as
[Running a server](docs/server.md) sets up a machine. From the
repository root, with an SSH key in ssh-agent:

```sh
export COMPOSE_FILE=deploy/docker/compose.yaml
export ARIES_KEYS="alice $(cat ~/.ssh/id_ed25519.pub)"
docker compose up -d --build --wait
docker compose logs --no-log-prefix aries | grep -A 13 'test server ready' | tail -n 14
```

The last command prints the `.aries.yaml` to save in a scratch
repository. [Connect a scratch repository](docs/deploy/docker.md#connect-a-scratch-repository)
goes on from there.

It is a server of [many projects](docs/server.md#serving-many-projects),
and `alice` is its admin, so `aries admin` works against it with nothing
more to set up: `aries admin project add web` makes a second project,
`aries admin principal add bob --key FILE` adds a person with no restart,
and `aries admin status` shows the server.

`deploy/docker/smoke.sh` builds the image and `aries` from the checkout,
starts its own server on port 2223, drives `aries` through the main
commands and through `aries admin` end to end, and removes everything it
made. CI does not run it, so run it after changing the server, the
protocol, `deploy/docker` or the setup steps in docs/server.md:

```sh
deploy/docker/smoke.sh
```

After changing telemetry, `deploy/observability` or the compose file's
profiles, run it with the harness too, which checks what reaches a
real Collector and Prometheus
([With the telemetry harness](docs/deploy/docker.md#with-the-telemetry-harness)):

```sh
ARIES_SMOKE_TELEMETRY=1 deploy/docker/smoke.sh
```

Without Docker, the [Local](docs/deploy/local.md) example sets a server
up in a Linux VM you can throw away.

## Open a pull request

1. Branch, title and commit as [Git](AGENTS.md#git) says: a branch such
   as `fix/lease-renewal`, a title such as `fix: …`.
2. Write the test first, and see it fail
   ([go-engineer](.claude/agents/go-engineer.md) describes the loop).
3. Run what the change calls for ([What to run before a pull
   request](#what-to-run-before-a-pull-request)). CI runs the whole gate
   on the pull request, and the merge queue requires it. Say in the pull
   request what you ran and what you left to CI: a check that was not
   run is reported as not run, never as passing.
4. Update the docs the change makes untrue, in the same pull request
   (the table below).
5. Fill in the [template](.github/pull_request_template.md): why, what,
   tests, the gate, docs, and dependencies: "None", or for each one
   added its reason and its line in [deps.allow](deps.allow). A
   dependency is admitted as [rule 4](AGENTS.md#rules) says, and a
   person reviews it; `go test ./internal/depcheck/` fails until
   `deps.allow` lists it, and asks for `-update` when what `aries` or
   `ariesd` links changes.

| You changed | Update |
|---|---|
| An `aries` command or flag, `aries admin` among them | [docs/cli.md](docs/cli.md) |
| Agent setup, hooks or MCP tools | [docs/agents.md](docs/agents.md) |
| CI gates, what a CI job runs, or the example workflows | [docs/ci.md](docs/ci.md), whose workflows pin each action by commit |
| What is queued while the server cannot be reached, what a queued write prints, `aries outbox`, or where the outbox is kept and whose it is | [When the server cannot be reached](docs/cli.md#when-the-server-cannot-be-reached) in docs/cli.md, [the same](docs/agents.md#when-the-server-cannot-be-reached) in docs/agents.md for what an agent is told, [The outbox](docs/security-model.md#the-outbox) in the security model, and `doc.go` of `internal/offline` and `internal/outbox`. Keep saying that none of it has run on Windows until it has |
| What an issue, claim, handoff, memory or cost is or does | [docs/concepts.md](docs/concepts.md) |
| A server setting or `ariesd` command | [Settings](docs/server.md#settings) and [ariesd commands](docs/server.md#ariesd-commands) in docs/server.md |
| A cap on what a client can grow (rule 18) | The [Limits](docs/server.md#limits) table in docs/server.md |
| A metric: an instrument, an attribute or a value an attribute may take | `internal/telemetry/instruments.go` and the tables of [What is measured](docs/server.md#what-is-measured) together; tests in `internal/telemetry` fail until they agree, and until every attribute's values come from a list in the code (rule 18) |
| A span: its name, an attribute or what a span may carry | `internal/telemetry/trace.go`, whose rule is that a span carries ids, codes and small numbers and never text, and the tables of [Traces](docs/server.md#traces) together; tests in `internal/telemetry` hold the two to each other, and `TestSpansCarryNoText` in `internal/server` needs a new op's arguments listed. [Traces](docs/security-model.md#traces) in the security model says what leaves the machine |
| A metric's name, an attribute or one of its values that an alert rule or the dashboard reads; a scrape job's name or address | `deploy/observability`, whose files tests in `internal/telemetry` hold to the instruments (`harness_test.go`), and [A collector, alert rules and a dashboard](docs/server.md#a-collector-alert-rules-and-a-dashboard) in docs/server.md. A new value of the `signal` attribute needs a rule on `aries_telemetry_dropped_total` for it. Then the smoke test with `ARIES_SMOKE_TELEMETRY=1`, and the soak with `ARIES_SOAK_TELEMETRY=check`; to look at it in Grafana, the Docker test server's `observe` profile ([Observe](docs/deploy/docker.md#observe)) |
| A log line: its event, a field, or what of it is exported | `internal/logs` (`events.go`, `schema.go`) and the tables of [Logs](docs/server.md#logs) together; tests in `internal/logs` read every log call of the daemon's packages and fail until each names a listed event first, under a message that is a string literal, with fields the schema lists, and until the guide lists every event and field. A field's kind there decides whether a record exported over OTLP may carry it ([Logs over OTLP](docs/server.md#logs-over-otlp), and [Logs](docs/security-model.md#logs) in the security model); `TestLogLinesCarryNoText` and `TestExportedLogsCarryNoText` in `internal/server` search both for marked text |
| The telemetry listener, a health check, an `OTEL_*` variable ariesd reads, or `ariesd status` | [Observability](docs/server.md#observability) in docs/server.md, and [The telemetry listener](docs/security-model.md#the-telemetry-listener) for what it reveals |
| What the store checks of Postgres when it opens (its version, the role's rights, the schema), or which SQLSTATEs a write retries | `internal/store` (`postgres.go`, `account.go`, `errors.go`), rules 6 and 8 in [AGENTS.md](AGENTS.md#rules), and the [design's As built note for the port](docs/design/aries.md) |
| The systemd unit, or what ariesd tells systemd | The unit and its table in [step 5](docs/server.md#5-run-ariesd-under-systemd), and a run under a real systemd ([What was run, and where](docs/server.md#what-was-run-and-where)): the tests reach only a socket that stands in for systemd's |
| Projects, principals, members, keys, the control store or the keys file | [Serving many projects](docs/server.md#serving-many-projects) in docs/server.md, and [Who can authenticate](docs/security-model.md#who-can-authenticate) |
| A message an operator sees when something is wrong | [Troubleshooting](docs/server.md#troubleshooting) in docs/server.md |
| A setup step in docs/server.md, or the Docker test server | [docs/deploy/docker.md](docs/deploy/docker.md), `deploy/docker`, and `smoke.sh` with its count of checks; and `install-server.sh` with the table of [With the installer](docs/server.md#with-the-installer), which `scripts/installserver_test.go` holds to the guide's steps and files |
| An `aries` or `ariesd` command or flag, a step or a heading of the guides, or a change to what operators run | The operations skill, `plugins/aries-ops` ([Operate a server with Claude Code](docs/server.md#operate-a-server-with-claude-code)): `scripts/opsskill_test.go` fails until every command, flag and section it names exists. Its steps follow the guides, so a changed step changes its reference file too; anything specific to the database belongs in `references/database.md` alone, which holds no steps until the move to Postgres lands |
| A file `ariesd` reads or writes, a socket or address it connects to, or a program it runs | `deploy/apparmor`, and a run on a machine with AppArmor ([deploy/apparmor/README.md](deploy/apparmor/README.md)) |
| What a deployment needs or costs | The example under [docs/deploy](docs/deploy/README.md) |
| How `aries` or `ariesd` is installed or upgraded, or `install.sh` | [docs/install.md](docs/install.md) |
| bd import or export | [docs/migrate-from-bd.md](docs/migrate-from-bd.md) |
| A security property, or a check CI makes | [docs/security-model.md](docs/security-model.md) |
| The protocol (rule 10) | The version in `internal/proto`, and what the change is in the pull request |
| The release process, or the Postgres pin | [RELEASING.md](RELEASING.md) |
| Anything a user or an operator of the next release will notice: a feature, a break, a removal, a security property, a supported platform | The next release's notes in [docs/releases](docs/releases/v0.5.0.md); a planned item that lands drops its **(planned)** mark there and in [Upgrading to 0.5](docs/upgrade-to-0.5.md) |
| A command of the gate | `scripts/gate.sh`, [Commands](AGENTS.md#commands) and `ci.yml` together; a test fails until the first two agree |
| A release's entry in the changelog | [CHANGELOG.md](CHANGELOG.md), written at the cut ([RELEASING.md](RELEASING.md#cutting-a-release)), never by an ordinary pull request |
| A tool's version | [Changing the Postgres pin](RELEASING.md#changing-the-postgres-pin) for Postgres; `ci.yml` and `mise.toml` for golangci-lint; `go.mod` and `mise.toml` for Go. Tests in `internal/version` fail until they agree |
| The layout of the code, a rule, or how agents should work | [AGENTS.md](AGENTS.md); the three [agent specifications](AGENTS.md#agent-specifications) together |
| The stage a feature is at | The status line and roadmap in [README.md](README.md) |
| Something that departs from a design | A dated **As built** note in [docs/design](docs/design/README.md) |

### What to run before a pull request

The full gate takes minutes and CI runs it on every pull request, so on
your machine run what the change can break, and leave the rest to CI.
Each tier includes the ones above it.

| The change touches | Run | Leave to CI |
|---|---|---|
| Docs, design or comments | `go test ./scripts/` (it holds the docs to the code and the guides to the scripts) | everything else |
| Code | The touched packages' tests with `-race`, then `scripts/gate.sh --quick` | the whole suite, lint, govulncheck, the builds for other systems |
| Concurrency: claims, locks, leases, the outbox and its replay, the clock, the database pool and retries | Also `-count=3` on the touched tests, and one short soak (`go test -race -run 'TestSoak$' ./internal/e2e`); with the outbox or replay, one run in partition mode too | |
| `deploy/docker`, `install-server.sh`, AppArmor, `ariesd`'s start, or the transport | Also one run of `deploy/docker/smoke.sh`, in the modes the change touches (`ARIES_SMOKE_TELEMETRY=1`, `ARIES_SMOKE_IDENTITY=1`, `ARIES_SMOKE_PROXY=1`) | |
| A release candidate | The whole gate, the long soak and its partition and telemetry modes (the [`soak` workflow](#run-the-soaks-on-github)), `smoke.sh` in every mode, and `install-server.sh` on a real machine of each supported release ([RELEASING.md](RELEASING.md)) | |

A test that is new or changed is still seen failing before its fix, in
every tier. After a rebase whose conflicts were in docs alone, don't
rerun the suite: CI and the merge queue run it on the result.

### Stacked pull requests

A stacked pull request has another pull request's branch as its base.
`main` takes squash merges, so the base's commits never reach `main` as
themselves, and two things go wrong unless you do this in order:

1. **Retarget before the base branch is deleted.** When the base pull
   request has merged, run `gh pr edit N --base main` on the stacked
   one, and only then delete the base's branch. Deleting it with `gh` or
   the API closes the pull requests stacked on it, and a closed pull
   request cannot be retargeted.
2. **Bring the stacked branch up to date.** A plain `git merge
   origin/main` conflicts wherever the branch changed what the base had
   changed.
   - If the branch was never pushed, replay its own commits onto `main`:

     ```sh
     git rebase --onto origin/main "$(git merge-base BASE-BRANCH HEAD)"
     ```

   - If it was pushed, merge, keeping the branch's side of each
     conflict, and check the result:

     ```sh
     git merge -X ours origin/main
     git diff HEAD^1 HEAD          # what the merge changed: only what main gained
     git diff origin/main HEAD     # the pull request: only the branch's own work
     ```

     If the second diff undoes something the base's review changed after
     you branched, the merge kept your older side of it: take `main`'s
     version of those lines.

## Writing a test

### Tests run in parallel

A test begins with `t.Parallel()`, so that it runs beside the other
tests of its package, as many at once as `go test -parallel` allows (the
number of processors). One that cannot begins with a comment that says
why:

```go
func TestStartWithoutPostgres(t *testing.T) {
	// Serial: sets the process's environment (t.Setenv).
```

`TestEveryTestIsParallelOrSaysWhyNot` (`internal/clienttest`) fails until
every test has one or the other. `go test` runs a package's serial tests
first, one at a time, and its parallel tests only once the last of those
has ended, so a serial test never runs beside another test: it may
change what the process shares, if it puts it back (`t.Cleanup`).

| A test is serial when it | Because |
|---|---|
| Sets the environment or the working directory (`t.Setenv`, `t.Chdir`, `iaptest.Install`) | They are the process's, and `t.Setenv` panics in a parallel test |
| Replaces a package-level variable (`pickPort`, `usageScanRows`) | Every test reads it |
| Counts what the process holds: goroutines (`noLeak`, the soak's `checkLeaks`) | A test beside it adds its own |
| Bounds or compares elapsed time (`pace`, the dead collector's turns, the division of usage by holds) | A test beside it takes the processor: the table under [Tests on a slow machine](#tests-on-a-slow-machine) says how such a test is written, and it stays serial |
| Calls `testing.AllocsPerRun` | It panics beside a parallel test |
| Runs a program it has just written (`release.VersionCheck` on a script) | A process that another test starts at that moment holds the new file open for an instant, and running it then fails with `text file busy` |
| Runs in a `synctest` bubble | Its time is virtual: it takes no real time, so nothing would be gained |

What does not make a test serial, because each test has its own: a
database and the Postgres server under it (below), a directory
(`t.TempDir()`), a port (listen on `127.0.0.1:0` and read the port the
kernel gave), a clock (`Options.Now`), a logger, a home directory and
the keys in it (`cli.Env`, `client.Options`).

What a whole package needs is set once, in `TestMain`, and never by a
test: the empty home directory and the absence of ssh-agent
(`clienttest.Isolate`), git that reads no configuration but the
repository's (`clienttest.IsolateGit`), and the Postgres servers.

Subtests run in turn within their test, which is usually what a table
wants: its rows often share what the test set up. A subtest that calls
`t.Parallel()` runs after its test's function has returned, so what it
uses must be closed by `t.Cleanup`, not by `defer`.

**Postgres.** A package's tests take their databases from a
`pgtest.Pool` (`newDSN`, `newDatabase`, `newStore`, `newWorld`, and
`pgOf` for the server itself). A test and its subtests hold one Postgres
server, and no other test has it meanwhile: the pool starts a server
when a test asks and none is free, up to one for every two processors
and four at most (`pgtest.PoolLimit`), and hands it on when the test
ends. So a test may stop its Postgres, restart it, stop its processes
(`Server.Signal`) or drop a database in it without disturbing another.
A test that takes a server and then runs subtests runs them in turn,
never with `t.Parallel()`: a parallel subtest waits for a free slot
after its test's function returns, still holding the server, and when
every slot is a test waiting for a server the run hangs until `go
test` times out. CI's four processors give two servers and four slots,
so two such tests were enough.
Postgres itself could serve the tests from one server; they hold one
each so that a test that stops its server stops no other test.

### Tests that leave nothing behind

A test run leaves no file, process, database or listening port, when it
passes and when a test fails.

| A test's | Go |
|---|---|
| Files | Under `t.TempDir()`. Where a unix socket's path would be too long there (104 bytes on macOS), under `os.MkdirTemp("", "aries")` with a short prefix, removed by `t.Cleanup`. Never a fixed path, the repository, or the home directory, which `clienttest.Isolate` replaces with an empty one for the process |
| Processes | Are stopped and waited for in `t.Cleanup`. Postgres is started through `pgtest` only: a server it starts is shut down, its directory removed and its semaphores and shared memory freed, when the test binary is gone however it went, and a test binary with a pool ends when its `go test` is gone |
| Databases | Are dropped when the test ends (`newDSN` and the others, above), and the pool drops whatever else the test left on its server, such as a database the daemon made for a new project |
| Ports and sockets | Are closed in `t.Cleanup`; the port is the kernel's choice |
| Shell scripts' files | `mktemp -d "${TMPDIR:-/tmp}/name.XXXXXX"`, with the template: `mktemp -d` alone ignores `TMPDIR` on macOS, so a test that gives a script a temporary directory would not get its files back |

A test binary that is killed, panics or runs out of time runs no
cleanup. Its Postgres servers go, by their watchers; the directories of the
tests that were running stay (`TestName123` and the short ones), and the
empty home directory. `scripts/leftovers.sh` covers that, and checks the
rest:

```sh
scripts/leftovers.sh go test ./...
```

It gives the command a temporary directory of its own, under the
`TMPDIR` it is given, reports what the command left in it and which
processes it started are still running, and removes the directory
whatever happened. When `GOTMPDIR` is set it does the same with a
`GOTMPDIR` of the command's own beneath it: `go test` then makes each
`t.TempDir` there, not under `TMPDIR`. A command that passed and left
something fails; a command that failed keeps its status, and what it
left is listed. Each entry is named with what probably made it,
`t.TempDir of TestName` (a subtest's name loses its slash) or
`os.MkdirTemp("", "sfe")`, and so is each process whose command line
names an entry. A process found only by its environment, such as a
`sleep` a test started and did not stop, is listed with its command
alone. With `GITHUB_ACTIONS=true` the report is an error annotation too.

The gate's tests run under it, in `scripts/gate.sh`,
[Commands](AGENTS.md#commands) and `ci.yml`, and so does CI's peer check
as root, since only root could remove what it left.

## What a local run cannot tell you

`scripts/gate.sh` ends with this list for the machine it ran on. CI
tests on linux/amd64 alone; for macOS and Windows it vets and builds
`aries` and `ariesd`, and runs no test.

| On | A passing gate does not show |
|---|---|
| Linux | `TestCheckPeerOtherUID` and `TestCheckPeerKeepsStatusFromOtherUsers` (`internal/server`), which start a socket peer as another user and so need root. CI builds the package's test binary and runs that under `sudo`, so that `go` never runs as root; the gate's summary prints the command |
| macOS | Anything about the tests on Linux. The code only Linux builds, the socket peer check behind `ariesd serve` (`internal/server/peer_linux.go`), is vetted, built and linted there but its tests, `TestCheckPeer*`, cannot run. A test that depends on the platform can pass on one system and fail on the other. On Apple silicon the amd64 builds are compiled and vetted, never run |
| Windows, natively | The scripts, which need a POSIX shell, and the tests marked for Unix: those of the Google Cloud IAP transport, and of refusing a transcript that is a FIFO. Use WSL for the gate |
| Any | The Docker smoke test, unless you pass `--smoke`; the long soak test, below; and, with a Go older than go.mod's `toolchain` line, govulncheck's verdict on the standard library, which the gate reports apart from failures |

So a pull request says where its gate ran, and CI's run is the one that
counts for Linux.

### Tests on a slow machine

CI's runner is slower than a laptop and busier, and a test that assumes
otherwise passes here and fails there, on someone else's pull request. To
run the tests as a slow machine runs them, give a Linux container part
of a processor:

```sh
docker run --rm --cpus 1 --memory 4g -e GOMAXPROCS=4 \
  -e ARIES_REQUIRE_POSTGRES=1 -e GOFLAGS=-buildvcs=false \
  -v "$PWD":/src:ro -w /src golang:1.27-trixie sh -c '
    apt-get update -q && apt-get install -yq postgresql-common &&
    /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh -y &&
    echo "create_main_cluster = false" >/etc/postgresql-common/createcluster.conf &&
    apt-get install -yq postgresql-18 && useradd -m tester &&
    su tester -c "umask 022 && PATH=/usr/lib/postgresql/18/bin:/usr/local/go/bin:\$PATH go test -race -shuffle=on -p 4 ./..."'
```

`GOMAXPROCS=4` and `-p 4` are what CI's four processors give: four
packages at once, each with four threads, here on one processor between
them. `--cpus 0.5` is slower again, and `--cpus 2` with a busy loop or
two beside the tests (`while :; do :; done &`) is a machine that is busy
rather than slow, which finds other faults. Mount a volume at
`/go/pkg/mod` and another as `GOCACHE` to keep the build between runs. A
test is run many times with `-run NAME -count N`. The tests run as a
user of their own, since Postgres refuses root, so `TestCheckPeer…`
skips there as it does on your machine; the umask is CI's, since a
user's own on Debian lets the group write, which the keys file's tests
refuse. `TestNoOldName` fails in a worktree there: its `.git` names a
path outside the container.

CI keeps the tests' temporary files, Postgres's data among them, in
memory (`TESTS_TMPFS` in [ci.yml](.github/workflows/ci.yml)), so its
tests do not wait for a disk and that command's do. For CI's way, add
`--tmpfs /dev/shm:exec,size=3g -e TMPDIR=/dev/shm -e GOTMPDIR=/tmp`
and a gigabyte of `--memory`: the files were a quarter of a gigabyte at
their most. Docker's own `/dev/shm` is 64 MB and runs no program, which
the tests need of their temporary directory.

A test that passes there:

| Does not | Does instead |
|---|---|
| Give up after a few seconds on something that must happen | Waits for the event: a channel, an answer, a condition it polls. Its deadline is there to turn a hang into a failure that says what did not happen, and is a minute (`patience` in `internal/server` and `internal/e2e`), not ten seconds |
| Go on at an answer when it needs what follows the answer | Waits for that too. A server answers before it has let go of the connection: the listener's tests read each connection to its close (`try` in `internal/telemetry`) |
| Take the first of something that repeats to be the one it caused | Waits for the one that holds what it did: the first export of a 50 ms interval may come before the test's recordings (`collector.carrying`) |
| Give work that must succeed the short deadline it gives work that must run out of time | Keeps the short deadline for the cases that wait it out |
| Assert that something took less than a fixed time | Bounds it by what the fault would cost with the timeouts the test set (half an export's timeout, for a recording that must not wait for the export); or times the same work beside it, in turns, so that the machine slows both alike (the dead collector's requests); or, for work of a few milliseconds, compares the fastest of many runs, which is the one the machine did not interrupt (the division of usage by holds); or, for a deadline the server sets itself, adds how late the machine was for a wait of the same length begun with it (`pace`, for the status report's two seconds) |
| Count on a minute of the real clock not passing, or on the clock not being set | Gives the store a clock (`Options.Now`, `newServerClock`, the e2e world's `now`): `notices_per_minute` is a minute of the store's time, and "held 0m", "0m ago" and "seen just now" are true for a minute of it. A machine that was suspended sets its clock forward when it wakes, which a daemon on the real clock records as a `clock.step` |
| Leave its database on its Postgres server | Takes it from `newDSN` or `newDatabase`, which drop it ([Tests that leave nothing behind](#tests-that-leave-nothing-behind)) |
| Run beside others when it bounds time or counts what the process holds | Stays serial, and says why ([Tests run in parallel](#tests-run-in-parallel)) |
| Compare, by the clock, work that the machine may interrupt every time | Compares the time the work had a processor (`onProcessor` in `internal/store`): on half a processor shared by four packages' tests, not one division in two hundred went uninterrupted |
| Take two answers given at different times to say the same of time | Leaves out what moves between them: a refusal for a rate tells of a wait that is a second shorter a second later (`refused` in `internal/e2e`) |

The slowest packages are `internal/server`, `internal/store` and
`internal/e2e`. With four packages at once on one processor, and the
machine otherwise idle, they took about three minutes, two and two and
a half. On half a processor they took ten minutes, eight to nine and
six and a half to eight: `go test` gives a package ten minutes, so pass
`-timeout 20m` there, as before the tests ran in parallel, when they
took nine and a half, eight and a half and eight. Most of
`internal/server`'s time on such a machine is its serial tests, the dead
collector's above all, which take their turns with the machine. These
times were measured when the tests ran on Dolt, and have not been
measured again on Postgres, which is much faster: on one laptop
(16 processors), `go test -race ./internal/store/` took 21 seconds on
Postgres and 257 on Dolt.

Tests in parallel are for a machine with processors to spare: on four
the suite takes a fifth less time than it took with the tests in turn,
and on one, with `GOMAXPROCS=4`, a quarter more, since what runs at
once only takes the processor from the rest. On a small machine, run
the tests in turn, as they ran before: `go test -parallel 1 ./...` (with
`-race -shuffle=on` for the gate's command), which starts one Postgres
server for a package and runs one test at a time. On one processor that
took the time it took before, 198 s against 205.

## Troubleshooting

| You see | Cause and fix |
|---|---|
| `No version is set for shim: go` | mise manages your Go and no version is set where you ran it. Run `go` inside the checkout, where mise.toml sets one, after `mise install` |
| `gate.sh: golangci-lint is not installed, or not on PATH` (or another tool) | Install the tool in the table above, or run `mise install` |
| `TestPostgresVersion`: `Version() = "17.6", want Postgres 18 (version.PostgresMajor)` | The `postgres` on your `PATH` is another major version. `export PATH="$(scripts/install-postgres.sh):$PATH"`; the gate does this itself |
| `pgtest: ARIES_REQUIRE_POSTGRES is set but postgres and initdb are not on PATH` | The same fix |
| `pgtest: Postgres refuses to run as root` | Run the tests as another user ([Tests on a slow machine](#tests-on-a-slow-machine) shows one way in a container) |
| `install-postgres.sh: checksum mismatch for postgresql-….tar.gz` | The download is not the archive that was pinned. Nothing was installed. Run it again; if it persists, do not use the download, and report it |
| `install-postgres.sh: building Postgres … failed` | It names its build log, kept in the cache directory. Most often a tool is missing: `make`, a C compiler, or zlib's headers (`zlib1g-dev` on Debian and Ubuntu) |
| `could not create shared memory segment: No space left on device`, on macOS | Test servers killed with no chance to clean up left System V shared memory behind, and macOS allows 32 segments. `ipcs -m -p` lists them; remove those that are yours and whose creator (`CPID`) is gone with `ipcrm -m ID`. `pgtest` frees a server's segment however it ends. Servers that are alive count too: each holds one, so about eight test runs at once (a pool runs up to four servers each) fill the 32. `pgtest` then fails with "the machine is out of System V shared memory or semaphores" and this fix, not only "exited early"; run fewer test binaries at once. It never changes `kern.sysv.shmmni` |
| `could not create semaphores: No space left on device`, on macOS | Test servers left their System V semaphores behind: about 400 each, in sets of 17, and macOS allows 87,381 (`sysctl kern.sysv.semmns`). `pgtest` frees a server's sets however it ends, by the creator's process id Postgres keeps in each set, so these come from a `pgtest` from before it did, or from another program's Postgres. While no Postgres of yours runs (`pgrep -x postgres` prints nothing), `ipcs -s -a` lists them, yours with mode `--ra-------` and 17 in `NSEMS`; remove each with `ipcrm -s ID`. A server starting on another data directory does not reclaim them |
| The gate's summary says `golangci-lint here is 2.13.2 and CI pins 2.14.0` | Your linter is another version, so CI may find what yours did not. Install the pinned one: `mise install`, or from [golangci-lint's releases](https://github.com/golangci/golangci-lint/releases) |
| govulncheck: `Your code is affected by 9 vulnerabilities from the Go standard library`; the gate prints `needs Go 1.27.2` | Your Go is older than go.mod's `toolchain` line, which CI installs. The findings are not your change's. Install that release (`mise install`) and run the gate again |
| govulncheck fails and the gate prints `FAILED` | A finding in a dependency, one no Go release fixes yet, or one fixed only by a Go newer than the `toolchain` line (the gate's `fix:` line says so): a real failure, which CI will report too. Read the finding's `Fixed in:` line and update the module, or move the `toolchain` line with `mise.toml`, `internal/tools/pin/go.mod` and the Docker image |
| `docker build`: `exec: "docker-credential-desktop": executable file not found in $PATH` | Docker Desktop's helper programs are not on your `PATH`: `export PATH="$HOME/.docker/bin:$PATH"` ([Docker test server](docs/deploy/docker.md#troubleshooting)) |
| A test passes here and fails in CI, or the reverse | [What a local run cannot tell you](#what-a-local-run-cannot-tell-you). `-shuffle=on` prints its seed; `go test -shuffle=SEED` repeats the order. One that fails only where the machine is slow: [Tests on a slow machine](#tests-on-a-slow-machine) |
| A pull request closed when its base branch was deleted | It cannot be retargeted or reopened onto `main`. Open a new one from the same branch, and next time follow [Stacked pull requests](#stacked-pull-requests) |
| `git merge origin/main` conflicts in files you did not mean to change | The branch was stacked on one that was squash-merged: [Stacked pull requests](#stacked-pull-requests), step 2 |

## The soak test

`TestSoak` in `internal/e2e` is the gate of stages 3 and 3a, and in its
partition mode (below) of stage 5. A short run
of about 12 seconds of work, 23 in all, is part of `go test`. Run a long
one before changing claims, the reaper, the event log, pushes, the write
path, the control store, or what happens when access ends:

```sh
ARIES_REQUIRE_POSTGRES=1 ARIES_SOAK=10m go test -race -timeout 45m -run 'TestSoak$' -v ./internal/e2e/
```

The run prints its seed first and with any failure, followed by the
events of each issue involved. `ARIES_SOAK_SEED=<seed>` repeats a
run's choices; the interleaving of sessions still differs. With `-v` the
report lists each op's count, outcome and p50 and p99 latency, the
refusals by code, each project's events and claims, the control log, what
the admin changed, and what the chaos did. Latency is reported, not
asserted: only a request with no answer in 60 seconds fails the run.

A 10-minute run under `-race` holds about 4.5 GiB at its peak, 3 GiB of
it the test binary's, and wants its four processors to itself: in a
container, give it 6 GiB and run long soaks one at a time. Three at
once in 8 GiB were killed for memory or starved past the locks' bound
of real time ([design §14](docs/design/aries.md#14-decisions), As built
for P5).

With `ARIES_E2E_TRANSPORT=https` the run, like the rest of
`internal/e2e`, connects over the daemon's HTTPS listener in place of
the in-process SSH server. Run it so once before changing the listener,
`internal/wsconn` or the key proof.

### Run the soaks on GitHub

The `soak` workflow (`.github/workflows/soak.yml`) runs the long soaks
on GitHub's Linux runners, on amd64 (`ubuntu-24.04`) and arm64
(`ubuntu-24.04-arm`): over SSH, over HTTPS, in partition mode and with
telemetry checked (below), eight jobs in all, each under `-race`,
`ARIES_REQUIRE_POSTGRES=1` and `scripts/leftovers.sh`, with Postgres and
Go installed as CI installs them (`.github/actions`). It runs only when
a person starts it, never on a pull request or a push. Start it from
the Actions tab, or:

```sh
gh workflow run soak.yml --ref BRANCH -f duration=10m
```

`-f seed=N` replays a seed in every job. Each job's summary has its
seed, its request count and its result. A release candidate runs it on
the commit to be tagged ([RELEASING.md](RELEASING.md)). A runner has 4
processors and 16 GiB, enough for one long soak at a time, and runs one.

The `p5-machine` workflow (`.github/workflows/p5-machine.yml`) is
the other check that needs a real machine: on the same two runners,
which are fresh virtual machines with systemd and AppArmor, it builds
the binaries, runs `install-server.sh --profile private --apparmor` as
root with a certificate from a throwaway CA (and again, to see it
change nothing), checks that `ariesd` runs under `ariesd-serve` in
enforce mode, drives it with `aries` over HTTPS, and fails on any
AppArmor denial of an `ariesd` profile in the journal. It also runs
only by hand, from `main`: `gh workflow run p5-machine.yml`. Run it
after a change to the installer, to `deploy/apparmor` or to what
`ariesd` reads, writes, connects to or runs.

### What it does

Five principals with five sessions each work the backlogs of two
projects on one server at once, through the in-process SSH server, a
real daemon and Postgres, while the test drops answers and connections,
restarts the daemon, and lets sessions vanish or stall past their
leases. It checks claims, fencing, the event log, idempotent retries,
the inbox and event pushes, token usage, rule 18's caps, and that the
daemon leaks nothing: for each project, from that project's own event
log and tables.

It also moves the server's clock (`soak_clock_test.go`): forward by 5 to
30 minutes on the store's clock, more than most leases have left, and
back by seconds, while the sessions work; and back by 10 to 15 minutes
at each restart, which the daemon must refuse (`clock_behind: stop`)
until the clock is corrected or that history accepted for one start. A
claim with lease left when the clock jumps forward must not be reaped
for it: read from the project's log, on the store's time, a store's
time follows the jump only after its `clock.step`, and a claim reaped
after one that found its lease live has a lease extended by the jump
(`jumpFaults`). A lease that ran out before the store settled the jump
is not one. A jump made while the clock is behind the stores' time is
checked too, as far as a log can say: a store whose time ran slow gives
its leases less than the jump, by an amount only it knows, so a claim
reaped then is held to what its log's `clock.step` events say they gave
it. Every other check holds unchanged, event time that never decreases
among them.

The run keeps its own reckoning of the stores' time (`vclock.now`), by
the rule the store follows. It is never behind that time and is often
ahead of it: by as long as the daemon took to stop and start after a
restart with the clock set back, which is minutes at the soak's speed on
a loaded machine. So no check may fail a
run for the difference between that reckoning and a time in a log. A
check that asks what the server knew when it answered goes by the order
of a log's events; one that places an event of one log against another
log goes by the two logs' times, which one server's stores share, with
no allowance for a jump of the clock: no write that begins after a jump
is dated before it, so an effect is never dated before its cause.

Meanwhile an admin changes who may reach what, over the protocol as
`aries admin` does and under the same failures: a membership removed and
added, a principal disabled and enabled, a key removed and added (one
that only the control store holds, and one that is also in
`authorized_keys`), a project admin demoted and promoted, a project
archived and activated, and a membership of a third, quiet project
removed while its store is closed. Each kind happens once in the short
run, and at random in a long one. The test checks that:

- the connections the lost access had let in are closed, the next
  handshake is refused, and the one after the access is back is let in;
- no session was ever let into a project while the control log gave it
  no access, or refused one it had access to. This goes by the control
  log's order and no clock: a handshake is answered from the server's
  memory of the control store, which held every event the server had
  answered an admin for before the handshake began and none that the
  log gained after it ended, so the answer must be right after one of
  the events between (`dialFault`);
- the claims of whoever lost access end, each with a `claim.release`
  that gives a reason the control log bears out, by the admin who made
  the change or by the server's own sweep, and the issues are open
  again; a demotion releases nothing, and a store that is closed is not
  opened to release. A release is placed against the control log by
  time, and is allowed nothing for a jump of the clock: one dated before
  the loss of access it answers to fails the run;
- nothing of one project shows through a connection to another: no
  answer or push names another project's issue, a request for one by
  its id is refused, and no marker or usage record of one project's
  sessions is in another's log or tables;
- the control database's own event log has no gap and explains every
  project, principal, membership and key in its tables, the daemon's
  memory of them is the same at the end, and `aries admin status` counts
  the same.

Some principals' keys reach the in-process SSH server from the control
store, through the keys file the daemon writes, as they reach sshd; at
the end that file holds a line for exactly the keys the control tables
give an active principal.

### Locks

Every run also has sessions contending for two locks of the first
project, a mutex and a semaphore of three (`soak_locks_test.go`), the
gate of stage 6's slice G2: under the same dropped answers, dropped
connections, restarts, clock steps and access changes, with leases of a
minute on the store's clock, half a second of real time. A session
acquires, waiting in the queue or refused, holds a while renewing, and
releases; or it dies holding or waiting, or stalls past its lease and
finds the hold lost. The replay of the project's log checks, a
transaction at a time, that no grant finds the lock at its capacity,
that a grant goes to the first live waiter and an acquire at once only
to an empty queue, that every epoch is the project's last plus one, that
no hold or place ends before its lease, and that no transaction leaves
a slot free while someone waits; each snapshot holds the lock tables to
the replay; no epoch is told to two sessions; and at the end, every
hold that lapsed was ended within two ticks of the reaper and 3
seconds of real time after its lease, less any jump of the clock
settled meanwhile, which is what lets a dead holder's lock pass on,
unless a daemon restart fell between. The report
gives the counts and how late the lapsed holds were ended. Run it longer
after changing locks, the reaper or the clock:

```sh
ARIES_REQUIRE_POSTGRES=1 ARIES_SOAK=10m go test -race -timeout 45m -run 'TestSoak$' -v ./internal/e2e/
```

### With the network cut

`ARIES_SOAK_PARTITION=1` turns on the partition mode
(`soak_partition_test.go`), the gate of stage 5: the soak with the
client cut from the server for minutes at a time, and its event log
still equal to what the sessions were told. Run it after changing the
outbox, its replay, the cache, conflicts, or what a late `finish` or
`handoff` does:

```sh
ARIES_REQUIRE_POSTGRES=1 ARIES_SOAK_PARTITION=1 ARIES_SOAK=10m go test -race -timeout 45m -run 'TestSoak$' -v ./internal/e2e/
```

Each agent's `aries mcp` then has an outbox and a cache in its user's own
data directory, as `aries mcp` does by default, and is cut off from the
server for 2 to 30 minutes of the store's clock at a time, after
connected spans as long, each as the seed chooses. A session is cut by
its dials: they fail as an unreachable server's do, and the connection
it had fails its next request without sending it, as a dropped one does,
and `aries mcp` closes it; a dial under way when the cut comes is closed
as it completes. It
goes on working meanwhile: its writes are queued, its reads of its own
work answered from the cache, its starts refused, and its leases lapse
on the server, where others take its issues. Let back, its next
connection replays what waits, and a `finish` or `handoff` that arrives
after its lease lapsed is applied, with `claim.lapsed`, or kept as a
comment, with `issue.kept`. When the work ends every agent is let back
and every outbox sent: by a new session of the principal where the
agent that queued it vanished, as on a machine where the next `aries mcp`
sends what the last one left. An agent cut off late in a long run may
leave hundreds of writes, which take minutes to send; a minute in which
none is sent fails the run.

Every check of the soak holds as before, with a replayed request held
to its principal and not its session, since whichever session connects
next delivers it. The replay of each log also holds that a late write
applied only under a claim nobody took since, and was kept only where
the issue had moved on. And `partitionFaults` holds every write a
session was told it queued, by its idempotency key and its markers, to
its outbox at rest, the server's answers to its replays, and the events
stamped with its key: applied once, parked, kept as a comment exactly
when the server said so, or refused with a code and message the person
reads in `aries outbox`; never still waiting, lost, silently dropped or
applied twice. A refused entry may be in the log only if an attempt of
it went unanswered, as when its answer was lost and the outbox then
refused it unsent because the principal's access had ended. It goes by
keys, markers and the log's order, never a clock, so its verdict does
not depend on how the run was timed. `TestPartitionFaults` shows it
catching each of those. With `-v` the report says how many cuts there
were, what was queued, refused while cut and read from the cache, and
what became of the replays.

### With telemetry on

The daemon's telemetry is off in the soak unless
`ARIES_SOAK_TELEMETRY` asks for it (`soak_telemetry_test.go`), so the
short run in the gate is as long as it was. Run it after changing an
instrument, a span, or where either is recorded:

```sh
ARIES_REQUIRE_POSTGRES=1 ARIES_SOAK_TELEMETRY=check go test -race -run 'TestSoak$' -v ./internal/e2e/
```

| Value | The run |
|---|---|
| `check` | Reads the instruments and spans in process when the run is at rest, and fails if they do not say what the event logs say. The claims taken, expired and released, each release by its reason, are the projects' logs' exactly. Every change in the control log is a control write that succeeded, and the control writes are counted the same where the request is answered. Each project has at least as many writes as its log has transactions, and as many spans of a retried attempt as retries counted. Every request and handshake has one observation and one span, with one result, and the server answered at least what the sessions saw answered. No connection was closed for a reason the control log has no change for. Every span that says who says so by a stand-in. And every line of the audit stream copies the event of the control log whose `seq` it has, none comes twice, and every access change a request made has its line, but for those in flight at a restart |
| `refused` | Exports metrics and traces to a port nothing listens on. Every other check of the soak must hold, the daemon must count what it dropped and warn once for each signal, and stopping its telemetry must take no longer than the 5 seconds its last exports are given |
| `hang` | The same, with a collector that accepts every connection and never answers |

`check` counts the spans as they end and keeps none, about 700 to 850
for each second of work, so a long run costs no more memory than one
without it. With `-v` it prints what it compared. The latency of a run
with a dead collector is in the same report as any run's, to set beside
a run with telemetry off: it is reported, not asserted.

### With a flood

`ARIES_SOAK_FLOOD=1` sends the HTTPS listener unauthenticated traffic
beside the sessions (`soak_flood_test.go`), to see what it costs them
(transport.md §12.4): TCP connections and TLS handshakes abandoned,
upgrades that never prove a key, junk to `/oauth/token` and
`/oauth/authorize`, and connections that send their headers or body a
byte a second. It puts the run's sessions on HTTPS, as
`ARIES_E2E_TRANSPORT=https` does, and gives the daemon a sign-in
provider so that `/oauth/*` exists. Every check of the soak holds as
without it, and none is made of the flood: the report says what was
sent, how each kind ended at the client, and what the daemon counted
(including the sessions' own upgrades and key proofs), for a person to
judge by the sessions' latencies beside a run without it.

```sh
ARIES_REQUIRE_POSTGRES=1 ARIES_SOAK_FLOOD=1 go test -race -run 'TestSoak$' -v ./internal/e2e/
```

| Variable | |
|---|---|
| `ARIES_SOAK_FLOOD_RATE` | Attacks begun each second, 20 by default |
| `ARIES_SOAK_FLOOD_CONNS` | Most open at once, 100 by default; one past it is counted, not sent |
| `ARIES_SOAK_FLOOD_FROM` | A loopback address to send from, such as `127.0.0.2` (macOS: `sudo ifconfig lo0 alias 127.0.0.2`) |

From 127.0.0.1 the flood and the sessions are one address to the
listener, so the run raises `https_conns_per_addr`, which would shut the
sessions out; with `ARIES_SOAK_FLOOD_FROM` that cap keeps its default and
the flood is an address of its own. It cannot be combined with
`ARIES_SOAK_TELEMETRY=check`.

## Working with an AI agent

[AGENTS.md](AGENTS.md) is the instruction file for any coding agent.
[Agent specifications](AGENTS.md#agent-specifications) describes
`go-engineer`, a Go engineer and reviewer you can run as a subagent.
