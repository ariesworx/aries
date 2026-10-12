# Contributing to starfix

starfix is an issue tracker and coordination layer for AI coding agents,
written in Go and stored in Dolt ([README](README.md)). This page gets
you from a fresh clone to a pull request. The rules every change follows
and the map of the code are in [AGENTS.md](AGENTS.md); they apply to
people and agents alike, so read them before your first change.

## From a clone to a passing gate

The gate is the set of checks CI runs on every pull request.
`scripts/gate.sh` runs the same commands on your machine.

With [mise](https://mise.jdx.dev), which reads the tool versions from
[mise.toml](mise.toml):

```sh
git clone https://github.com/ariesworx/starfix && cd starfix
mise install        # Go, golangci-lint and shellcheck
scripts/gate.sh     # fetches the pinned Dolt, then runs what CI runs
```

Without mise, which is optional, install the tools below yourself, then
run `scripts/gate.sh`.

| Tool | Version | Used for |
|---|---|---|
| Go | the newest patch release of the `go` line in [go.mod](go.mod) | everything |
| golangci-lint | the `version:` CI pins in [ci.yml](.github/workflows/ci.yml), and mise.toml with it | lint, with the config in `.golangci.yml` |
| shellcheck | any recent; mise.toml names one | every `*.sh` in the repository |
| git | any recent | the gate, and tests of branch and worktree creation |
| Dolt | `Dolt` in [internal/version](internal/version/version.go) | the store, server, bd import and end-to-end tests. `scripts/install-dolt.sh` installs it |
| Docker | Docker Desktop, OrbStack, or Docker Engine 25 with Compose v2 | only the [Docker test server](#try-your-change-against-a-real-server) |

**Dolt** must be the pinned release exactly; a package manager's is
usually another, and `TestDoltVersion` then fails.
`scripts/install-dolt.sh` downloads the pinned release for Linux or
macOS from Dolt's GitHub releases, verifies its SHA-256 against
[internal/version/dolt.sha256](internal/version/dolt.sha256), and unpacks
it under your cache directory (`~/.cache/starfix`), never over a `dolt`
you installed. The gate runs it for you when the `dolt` on your `PATH`
is another version. To run `go test` yourself, put it on your `PATH`:

```sh
export PATH="$(scripts/install-dolt.sh):$PATH"
go test ./...
```

Tests that need Dolt or git skip without them, and a skipped store test
proves nothing; `STARFIX_REQUIRE_DOLT=1`, which the gate and CI set,
makes a missing Dolt a failure. [Commands](AGENTS.md#commands) lists the
gate command by command.

| Command | Does |
|---|---|
| `scripts/gate.sh` | The gate. Stops at the first failure, prints one line for each command, and ends with what the run did not cover |
| `scripts/gate.sh --keep-going` | The same, running every command whatever fails |
| `scripts/gate.sh --smoke` | The gate, then the [Docker smoke test](#try-your-change-against-a-real-server) |
| `scripts/gate.sh --quick` | Not the gate; for the loop between edits. Format, vet, lint, shellcheck and the tests without the race detector, so unchanged packages come from Go's test cache. It skips `go mod verify`, govulncheck and the builds for other systems |

**On Windows,** build and test `sfx` natively with Go (`go build
./cmd/sfx`, `go test ./...`). The scripts are for a POSIX shell, and
`starfixd serve` runs on Linux only, so run the scripts, the gate and
the server in WSL, where everything on this page works as on Linux.

## Try your change against a real server

The [Docker test server](docs/deploy/docker.md) runs sshd, `starfixd`
and Dolt in one container, built from your checkout and set up as
[Running a server](docs/server.md) sets up a machine. From the
repository root, with an SSH key in ssh-agent:

```sh
export COMPOSE_FILE=deploy/docker/compose.yaml
export STARFIX_KEYS="alice $(cat ~/.ssh/id_ed25519.pub)"
docker compose up -d --build --wait
docker compose logs --no-log-prefix starfix | grep -A 13 'test server ready' | tail -n 14
```

The last command prints the `.starfix.yaml` to save in a scratch
repository. [Connect a scratch repository](docs/deploy/docker.md#connect-a-scratch-repository)
goes on from there.

It is a server of [many projects](docs/server.md#serving-many-projects),
and `alice` is its admin, so `sfx admin` works against it with nothing
more to set up: `sfx admin project add web` makes a second project,
`sfx admin principal add bob --key FILE` adds a person with no restart,
and `sfx admin status` shows the server.

`deploy/docker/smoke.sh` builds the image and `sfx` from the checkout,
starts its own server on port 2223, drives `sfx` through the main
commands and through `sfx admin` end to end, and removes everything it
made. CI does not run it, so run it after changing the server, the
protocol, `deploy/docker` or the setup steps in docs/server.md:

```sh
deploy/docker/smoke.sh
```

Without Docker, the [Local](docs/deploy/local.md) example sets a server
up in a Linux VM you can throw away.

## Open a pull request

1. Branch, title and commit as [Git](AGENTS.md#git) says: a branch such
   as `fix/lease-renewal`, a title such as `fix: …`.
2. Write the test first, and see it fail
   ([go-engineer](.claude/agents/go-engineer.md) describes the loop).
3. Run `scripts/gate.sh`. Copy what its summary says the run did not
   cover into the pull request: a check that was not run is reported as
   not run, never as passing.
4. Update the docs the change makes untrue, in the same pull request
   (the table below).
5. Fill in the [template](.github/pull_request_template.md): why, what,
   tests, the gate, docs, and "No new dependencies" or the reason for
   the one you added.

| You changed | Update |
|---|---|
| An `sfx` command or flag, `sfx admin` among them | [docs/cli.md](docs/cli.md) |
| Agent setup, hooks or MCP tools | [docs/agents.md](docs/agents.md) |
| What an issue, claim, handoff, memory or cost is or does | [docs/concepts.md](docs/concepts.md) |
| A server setting or `starfixd` command | [Settings](docs/server.md#settings) and [starfixd commands](docs/server.md#starfixd-commands) in docs/server.md |
| A cap on what a client can grow (rule 18) | The [Limits](docs/server.md#limits) table in docs/server.md |
| A metric: an instrument, an attribute or a value an attribute may take | `internal/telemetry/instruments.go` and the tables of [What is measured](docs/server.md#what-is-measured) together; tests in `internal/telemetry` fail until they agree, and until every attribute's values come from a list in the code (rule 18) |
| The telemetry listener, a health check, an `OTEL_*` variable starfixd reads, or `starfixd status` | [Observability](docs/server.md#observability) in docs/server.md, and [The telemetry listener](docs/security-model.md#the-telemetry-listener) for what it reveals |
| The systemd unit, or what starfixd tells systemd | The unit and its table in [step 5](docs/server.md#5-run-starfixd-under-systemd), and a run under a real systemd ([What was run, and where](docs/server.md#what-was-run-and-where)): the tests reach only a socket that stands in for systemd's |
| Projects, principals, members, keys, the control store or the keys file | [Serving many projects](docs/server.md#serving-many-projects) in docs/server.md, and [Who can authenticate](docs/security-model.md#who-can-authenticate) |
| A message an operator sees when something is wrong | [Troubleshooting](docs/server.md#troubleshooting) in docs/server.md |
| A setup step in docs/server.md, or the Docker test server | [docs/deploy/docker.md](docs/deploy/docker.md), `deploy/docker`, and `smoke.sh` with its count of checks |
| A file `starfixd` reads or writes, a socket or address it connects to, or a program it runs | `deploy/apparmor`, and a run on a machine with AppArmor ([deploy/apparmor/README.md](deploy/apparmor/README.md)) |
| What a deployment needs or costs | The example under [docs/deploy](docs/deploy/README.md) |
| How `sfx` or `starfixd` is installed or upgraded, or `install.sh` | [docs/install.md](docs/install.md) |
| bd import or export | [docs/migrate-from-bd.md](docs/migrate-from-bd.md) |
| A security property, or a check CI makes | [docs/security-model.md](docs/security-model.md) |
| The protocol (rule 10) | The version in `internal/proto`, and what the change is in the pull request |
| The release process, or the Dolt pin | [RELEASING.md](RELEASING.md) |
| A command of the gate | `scripts/gate.sh`, [Commands](AGENTS.md#commands) and `ci.yml` together; a test fails until the first two agree |
| A tool's version | [Changing the Dolt pin](RELEASING.md#changing-the-dolt-pin) for Dolt; `ci.yml` and `mise.toml` for golangci-lint; `go.mod` and `mise.toml` for Go. Tests in `internal/version` fail until they agree |
| The layout of the code, a rule, or how agents should work | [AGENTS.md](AGENTS.md); the three [agent specifications](AGENTS.md#agent-specifications) together |
| The stage a feature is at | The status line and roadmap in [README.md](README.md) |
| Something that departs from a design | A dated **As built** note in [docs/design](docs/design/README.md) |

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

## What a local run cannot tell you

`scripts/gate.sh` ends with this list for the machine it ran on. CI
tests on linux/amd64 alone; for macOS and Windows it vets and builds
`sfx` and `starfixd`, and runs no test.

| On | A passing gate does not show |
|---|---|
| Linux | `TestCheckPeerOtherUID` (`internal/server`), which starts a socket peer as another user and so needs root. CI runs it under `sudo`; the gate's summary prints the command |
| macOS | Anything about the tests on Linux. The code only Linux builds, the socket peer check behind `starfixd serve` (`internal/server/peer_linux.go`), is vetted, built and linted there but its tests, `TestCheckPeer*`, cannot run. A test that depends on the platform can pass on one system and fail on the other. On Apple silicon the amd64 builds are compiled and vetted, never run |
| Windows, natively | The scripts, which need a POSIX shell, and the tests marked for Unix: those of the Google Cloud IAP transport, and of refusing a transcript that is a FIFO. Use WSL for the gate |
| Any | The Docker smoke test, unless you pass `--smoke`; the long soak test, below; and, with a Go older than the newest patch release, govulncheck's verdict on the standard library, which the gate reports apart from failures |

So a pull request says where its gate ran, and CI's run is the one that
counts for Linux.

## Troubleshooting

| You see | Cause and fix |
|---|---|
| `No version is set for shim: go` | mise manages your Go and no version is set where you ran it. Run `go` inside the checkout, where mise.toml sets one, after `mise install` |
| `gate.sh: golangci-lint is not installed, or not on PATH` (or another tool) | Install the tool in the table above, or run `mise install` |
| `TestDoltVersion`: `Version() = "2.3.3", want version.Dolt "2.4.2"` | The `dolt` on your `PATH` is not the pinned release. `export PATH="$(scripts/install-dolt.sh):$PATH"`; the gate does this itself |
| `STARFIX_REQUIRE_DOLT is set but dolt is not on PATH` | The same fix |
| `install-dolt.sh: checksum mismatch for dolt-…tar.gz` | The download is not the archive that was pinned. Nothing was installed. Run it again; if it persists, do not use the download, and report it |
| The gate's summary says `golangci-lint here is 2.13.2 and CI pins 2.14.0` | Your linter is another version, so CI may find what yours did not. Install the pinned one: `mise install`, or from [golangci-lint's releases](https://github.com/golangci/golangci-lint/releases) |
| govulncheck: `Your code is affected by 9 vulnerabilities from the Go standard library`; the gate prints `needs Go 1.27.2` | Your Go is an older patch release than CI's. The findings are not your change's. Install the newest patch release (`mise upgrade go`) and run the gate again |
| govulncheck fails and the gate prints `FAILED` | A finding in a dependency, or one no Go release fixes yet: a real failure, which CI will report too. Read the finding's `Fixed in:` line and update the module |
| `docker build`: `exec: "docker-credential-desktop": executable file not found in $PATH` | Docker Desktop's helper programs are not on your `PATH`: `export PATH="$HOME/.docker/bin:$PATH"` ([Docker test server](docs/deploy/docker.md#troubleshooting)) |
| A test passes here and fails in CI, or the reverse | [What a local run cannot tell you](#what-a-local-run-cannot-tell-you). `-shuffle=on` prints its seed; `go test -shuffle=SEED` repeats the order |
| A pull request closed when its base branch was deleted | It cannot be retargeted or reopened onto `main`. Open a new one from the same branch, and next time follow [Stacked pull requests](#stacked-pull-requests) |
| `git merge origin/main` conflicts in files you did not mean to change | The branch was stacked on one that was squash-merged: [Stacked pull requests](#stacked-pull-requests), step 2 |

## The soak test

`TestSoak` in `internal/e2e` is the gate of stages 3 and 3a. A short run
of about 12 seconds of work, 23 in all, is part of `go test`. Run a long
one before changing claims, the reaper, the event log, pushes, the write
path, the control store, or what happens when access ends:

```sh
STARFIX_REQUIRE_DOLT=1 STARFIX_SOAK=10m go test -race -timeout 45m -run 'TestSoak$' -v ./internal/e2e/
```

The run prints its seed first and with any failure, followed by the
events of each issue involved. `STARFIX_SOAK_SEED=<seed>` repeats a
run's choices; the interleaving of sessions still differs. With `-v` the
report lists each op's count, outcome and p50 and p99 latency, the
refusals by code, each project's events and claims, the control log, what
the admin changed, and what the chaos did. Latency is reported, not
asserted: only a request with no answer in 60 seconds fails the run.

### What it does

Five principals with five sessions each work the backlogs of two
projects on one server at once, through the in-process SSH server, a
real daemon and Dolt, while the test drops answers and connections,
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
for it, and every other check holds unchanged, event time that never
decreases among them. The run's own reckoning of the store's time
follows the same rule the store does (`vclock.now`).

Meanwhile an admin changes who may reach what, over the protocol as
`sfx admin` does and under the same failures: a membership removed and
added, a principal disabled and enabled, a key removed and added (one
that only the control store holds, and one that is also in
`authorized_keys`), a project admin demoted and promoted, a project
archived and activated, and a membership of a third, quiet project
removed while its store is closed. Each kind happens once in the short
run, and at random in a long one. The test checks that:

- the connections the lost access had let in are closed, the next
  handshake is refused, and the one after the access is back is let in;
- no session was ever let into a project while the control log gave it
  no access, or refused one it had access to;
- the claims of whoever lost access end, each with a `claim.release`
  that gives a reason the control log bears out, by the admin who made
  the change or by the server's own sweep, and the issues are open
  again; a demotion releases nothing, and a store that is closed is not
  opened to release;
- nothing of one project shows through a connection to another: no
  answer or push names another project's issue, a request for one by
  its id is refused, and no marker or usage record of one project's
  sessions is in another's log or tables;
- the control database's own event log has no gap and explains every
  project, principal, membership and key in its tables, the daemon's
  memory of them is the same at the end, and `sfx admin status` counts
  the same.

Some principals' keys reach the in-process SSH server from the control
store, through the keys file the daemon writes, as they reach sshd; at
the end that file holds a line for exactly the keys the control tables
give an active principal.

## Working with an AI agent

[AGENTS.md](AGENTS.md) is the instruction file for any coding agent.
[Agent specifications](AGENTS.md#agent-specifications) describes
`go-engineer`, a Go engineer and reviewer you can run as a subagent.
