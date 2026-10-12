# Contributing to starfix

This page gets you from a fresh clone to a pull request. The rules every
change follows, the map of the code and the CI gate are in
[AGENTS.md](AGENTS.md). They apply to people and agents alike, so read
them before your first change.

## What you need

| Tool | Version | Used for |
|---|---|---|
| Go | the `go` line in [go.mod](go.mod) | everything |
| Dolt | `Dolt` in [internal/version](internal/version/version.go) | the store, server, bd import and end-to-end tests |
| golangci-lint | the version CI pins | lint, with the config in `.golangci.yml` |
| shellcheck | any recent | `install.sh` |
| git | any recent | tests of branch and worktree creation |

Get Dolt from [Dolt's releases](https://github.com/dolthub/dolt/releases).

## Build and test

```sh
go build ./cmd/...
go test ./...
```

Tests that need Dolt, git or root skip without them, and a skipped store
test proves nothing. [Commands](AGENTS.md#commands) says which tests need
what, and how CI makes a missing Dolt fail instead. To try a real server
by hand, start the [Docker test server](docs/deploy/docker.md), which
builds from your checkout, or follow the [Local](docs/deploy/local.md)
example in a Linux VM you can throw away. `deploy/docker/smoke.sh`
drives that server through the main commands end to end; CI does not
run it.

### The soak test

`TestSoak` in `internal/e2e` is the gate of stages 3 and 3a: five
principals with five sessions each work the backlogs of two projects on
one server at once, through the in-process SSH server, a real daemon and
Dolt, while the test drops answers and connections, restarts the daemon,
and lets sessions vanish or stall past their leases. It checks claims,
fencing, the event log, idempotent retries, the inbox and event pushes,
token usage, rule 18's caps, and that the daemon leaks nothing: for each
project, from that project's own event log and tables.

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

A short run of about 12 seconds of work, 23 in all, is part of `go
test`. Run a long one before changing
claims, the reaper, the event log, pushes, the write path, the control
store, or what happens when access ends:

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

## Before you open a pull request

Run the whole gate in [Commands](AGENTS.md#commands), as CI does. Then
check that the docs are still true: a change in behavior updates the page
that describes it, in the same pull request.

| You changed | Update |
|---|---|
| An `sfx` command or flag | [docs/cli.md](docs/cli.md) |
| Agent setup, hooks or MCP tools | [docs/agents.md](docs/agents.md) |
| A server setting, limit or `starfixd` command | [docs/server.md](docs/server.md) |
| A setup step in docs/server.md, or the Docker test server | [docs/deploy/docker.md](docs/deploy/docker.md) and `deploy/docker` |
| A security property | [docs/security-model.md](docs/security-model.md) |
| The release process | [RELEASING.md](RELEASING.md) |
| Something that departs from a design | an **As built** note in [docs/design](docs/design/README.md) |

Branches, pull request titles and commit messages follow
[Git](AGENTS.md#git), and a new dependency needs a reason in the pull
request.

## Working with an AI agent

[AGENTS.md](AGENTS.md) is the instruction file for any coding agent.
[Agent specifications](AGENTS.md#agent-specifications) describes
`go-engineer`, a Go engineer and reviewer you can run as a subagent.
