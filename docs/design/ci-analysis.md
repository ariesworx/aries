# CI and the test suite: where the time, CPU and memory go

13 Oct 2026. **Status: research. Proposed, not decided, and nothing here is built.** No workflow, script, test or product file was changed for it. It answers the maintainer's request to "analyze the memory usage and cleanup with testing to ensure we are optimizing our CI pipeline and not accumulating unnecessary CPU time", at commit `32a6eaf` (pull request #92) and its parent `7ea0d7e`.

**Since this was written (13 Oct 2026).** Proposals 1 to 4 of [§2](#2-proposed-changes-ranked) are in the workflows, and the rest of this document is as it was: its line numbers and "today" are those of the day it was written. None of the four had run on a GitHub runner when this note was written, so every saving below is still an estimate.

- **§2.1, the peer check.** As proposed, in two steps, so that the build and the tests are timed apart. The step fails unless `TestCheckPeerOtherUID` and `TestCheckPeerKeepsStatusFromOtherUsers` both passed and no test was skipped. `-test.count=1` is left out: a test binary run directly has no cache to read.
- **§2.2, the caches.** The design choice the section leaves open was taken the cheaper way: a cache is saved once for each `go.sum`, by a push to `main`, with no restore keys, so it holds the standard library, the dependencies and the repository's packages as they were at that commit. The module cache is a third cache, which `lint` saves and both jobs read. One thing differs from the section: the release's `gate` job, and any run that is neither a pull request nor on `main`, restores nothing, where it used to restore `setup-go`'s cache. Measured in the container: the `test` job's build cache is 629 MB, 159 MB through `gzip -1`.
- **§2.3, the temporary files.** On `/dev/shm`, when a step finds it is a memory file system with 4 GB free on a machine with 8 GB available, where a program written to it runs; otherwise in `/tmp`, with a warning. `GOTMPDIR` is `/tmp`. Measured in the container at CI's shape, as a user who is not root, while other work loaded the machine's disk: 90 s twice with the files on memory, every package passing; 657 s on disk in the run between them, with `internal/server` and `internal/store` stopped at their ten minutes and no test failed, which is §4.5 again and not a quiet machine's figure. The files were 219 MB at their most, sampled every two seconds. Still not verified: `/dev/shm` on a runner, and whether a runner's disk is the quiet one or the busy one.
- **§2.4, the timeouts.** `test` has 30 minutes, not 20, which is three and a half times the slowest job read; the others are as proposed. `sign-and-publish` has none, as §2.4 left it: GitHub's documentation of `jobs.<job_id>.environment` says a job is sent to a runner only when the environment's rules have passed, and the API's record of the two releases read has the job starting after its approval, so the wait is probably not the job's time; but that is an inference, the job is the one that signs, and a limit that cut it short would do more harm than a hang there costs. `gate` has none, because a job that calls a workflow may not have one.

Every statement is one of three kinds, and says which where it matters:

- **Measured**: from GitHub's records of this repository's runs, read on 13 Oct 2026, or run that day on one machine ([§10](#10-method-and-where-the-raw-files-are)).
- **Read**: from the repository's files, or from GitHub's documentation on 13 Oct 2026.
- **Inferred**: reasoning from the first two, not checked.

Two limits to keep in mind throughout. The local runs were on Apple silicon: the Linux container is arm64 and CI is amd64 on slower processors, so local seconds are shapes, not predictions. And from about 14:25 local time three other agents were loading the same machine; wall times taken after that are marked **loaded**, and where a wall time matters this document uses the runs taken before, or says so.

Three things are deliberately not here, because other pieces of work have them: profiles of `starfixd` and the store as a product, profiles of the `sfx` client, and running tests in parallel within a package (`t.Parallel`, shared fixtures). [§4.1](#41-numbers-for-the-work-on-parallel-tests) gives the numbers that last piece needs.

## 1. Summary

### Where it goes today

Real CI, the three runs since #92 (two of the pull request, one of `main`); the earlier figures are in [§3](#3-the-real-ci-from-githubs-records).

| | Wall time | What it is spent on |
|---|---|---|
| `test` job | 285 to 302 s | `go test` 218 to 229 s; the peer check 47 to 50 s; setup 15 to 20 s |
| of which, before `internal/server` starts | about 87 to 90 s | Compiling and linking every test binary with the race detector from an empty cache: the cache the job restores holds no race objects ([§7.1](#71-caching)) |
| of which, `internal/server` | 131 to 139 s | The longest package. It ends the step: 87 s of build plus 131 s of tests is the 218 s |
| of which, the peer check | 47 to 50 s | 4.6 s of tests, and a second cold race build of `internal/server` and everything under it, as root with an empty cache |
| `lint` job | 142 s (84 to 172 s in the 29 before) | The builds for macOS and Windows take 44 to 77 s of it, also uncached; golangci-lint 11 to 58 s |
| `codeql` job | 65 to 103 s | Not a required check |
| Queue | 3 to 4 s, twice about a minute | |

Locally, at CI's shape (a Linux container with four processors, `GOMAXPROCS=4 -p 4`, the race detector, before the machine was shared; four runs: CI's command twice, and once each without the coverage profile and without shuffle, neither of which changes the time):

| | Figure | |
|---|---|---|
| Wall, tests only | 94 to 100 s | `internal/server` 79 to 83 s, `internal/store` 38 to 50 s, `internal/e2e` 42 to 49 s; every other package under 11 s |
| CPU, tests only | 167 to 174 s | Dolt 80 to 82 s, the test binaries 74 to 78 s, the rest (go, the linker, the sampler) 12 to 13 s |
| CPU without the race detector | 128 s | Dolt 88 s, the test binaries 27 s: the race detector costs the test binaries 2.8 times their CPU and the run 30% of its wall time. Dolt is then 69% of all CPU |
| Build, empty cache | 38 s wall, 84 s CPU | 9 s wall and 6 s CPU from a warm cache |
| Memory, peak of every Dolt and test binary together | 2.0 GB | The container's own peak with file cache, 2.4 to 3.4 GB. The runner has 16 GB |
| Largest single process | 1.4 to 1.6 GB | The Dolt of `cmd/starfixd`, which drops no database |

The test binaries themselves do very little. Profiled without the race detector, the four heaviest spend 1% to 4% of their wall time on a processor, and at most 16% of that is the harness's own code ([§9](#9-profiles-of-the-test-binaries)). The time is Dolt's, and most of the wall time is Dolt waiting for the disk: the same tests took 3 to 7 times as long when other work loaded the machine's disk, with almost the same CPU, and did not slow at all with their files on memory ([§4.5](#45-the-disk)).

### What #92 changed in real CI

| | 13 Oct before #92 | After #92 |
|---|---|---|
| Successful `test` jobs read | 31 | 3 |
| `go test` step | 261 to 444 s, median 362 s | 218, 229 and 229 s |
| `test` job | 319 to 515 s, median 432 s | 285, 296 and 302 s |
| `internal/server` (from 5 and 3 logs) | 245 to 354 s | 131 to 139 s |
| `internal/store` | 154 to 216 s | 83 to 88 s |
| `internal/e2e` | 66 to 82 s | 61 to 69 s |

About 135 s, or a third, off the job, and `internal/server` from as much as 59% of `go test`'s ten minutes to 23%. Three runs are not a trend: they are three runs, all within twelve minutes of each other, and the earlier 31 varied by 180 s between themselves. The direction is not in doubt, since the cause was measured: the size of the saving is. In memory, before #92 the Dolt of `internal/server` reached 5.6 GB and that of `internal/store` 4.4 GB (measured on the host); after it they reach 0.5 and 0.4 GB.

### What a run leaves behind

On Linux, which is what CI is: nothing. No process, no file in `/tmp`, no listening port, no Docker object, nothing in the repository. A test binary that is killed leaves a Dolt that the `dolttest` watcher ends within a second, and its files with it. Killing `go test` alone is different: its test binaries run on as orphans to the end of their packages, 125 s when tried, and then clean up ([§6](#6-what-a-run-leaves-behind)).

On macOS, one thing: each `go test ./scripts` leaves six directories named `tmp.XXXXXXXXXX` in the user's temporary directory, because `mktemp -d` there ignores the `TMPDIR` the tests set. The machine this was run on held 478 of them, 7 MB, from two days.

The Go build cache is the only thing that grows: 0.5 GB after one run of the gate's tests, 1.2 GB after the variants run for this document.

### The cost is time to merge, not money

The repository is public and every job runs on `ubuntu-24.04`, a standard runner, which GitHub's documentation says is free and unlimited for public repositories; the API reports 0 billable milliseconds for the run sampled. What CI costs is the wait: a pull request's checks take as long as the `test` job, 5 minutes now and 7 before #92, and `main` requires a branch to be up to date, so a merge sends every other open pull request round again. The last 30 pull requests took 48 runs of `ci`, 1.6 each, and were open a median of 22 minutes.

## 2. Proposed changes, ranked

Ranked by saving against risk. **ci.yml** marks a change to the file that gates every merge and, through `release.yml`'s `gate` job, every release. **test-only** marks one that touches only `_test.go` files. Savings in real CI are **estimated** unless they say measured: none of these was tried on a runner.

| # | Change | Touches | Saving per `ci` run | Risk |
|---|---|---|---|---|
| 1 | Run the peer check from a test binary built with the job's own cache | **ci.yml** | 30 to 35 s of the `test` job (estimated; 54% of the step measured locally) | Low |
| 2 | Give `test` and `lint` build caches that hold what they build | **ci.yml** | 30 to 70 s of `test`, 35 to 60 s of `lint` (estimated; the build measured locally) | Low to medium |
| 3 | Put the tests' temporary files on memory | **ci.yml** | Unknown on a runner; locally 9% on a quiet machine, and no slowdown under disk load, against 3 to 7 times and a package past its ten minutes | Low; needs one trial run |
| 4 | `timeout-minutes` on every job | **ci.yml**, `codeql.yml`, `vuln.yml`, `release.yml` | None; bounds a hang at minutes instead of six hours | Very low |
| 5 | Give `mktemp` a template under `$TMPDIR` | scripts | None in CI; stops the leak on macOS | Very low |
| 6 | Skip `test` and `codeql` for a pull request that changes only `docs/design/` | **ci.yml**, `codeql.yml` | A whole run, for about one pull request in five | Medium |
| 7 | A merge queue in place of "update branch, wait, merge" | **ci.yml**, `codeql.yml`, repository settings | Reruns that cascade when several pull requests are open | Medium |
| 8 | Fixed cost per test: 32 ms of database setup, and up to 0.75 s of waiting for the disk | test-only | For the work on parallel tests to judge; the numbers are in §4.1 and §4.3 | |
| | **Do nothing**: sharding the `test` job, `-p`, `-shuffle`, the coverage profile, a `-timeout` flag, the weekly `vuln` run, the release's rerun of the gate, the dependency groups, the runs on `main`, dropping databases in `cmd/starfixd` and `internal/bdimport` | | | §2.9 |

### 2.1 Run the peer check from a built binary (ci.yml)

**What.** `ci.yml` lines 84 to 90 run `go test -race -count=1 -run '^TestCheckPeer' ./internal/server` under `sudo` with `GOCACHE=$RUNNER_TEMP/gocache-root`, an empty directory, so that root writes nothing into the runner's cache. Root therefore compiles the standard library, every dependency and `internal/server` with the race detector from nothing, to run two tests. Instead: build the test binary as the runner's user, from the cache the step before has just filled, and run only the binary as root.

```sh
go test -race -c -o "$RUNNER_TEMP/server.test" ./internal/server
cd internal/server && sudo env "PATH=$PATH" STARFIX_REQUIRE_DOLT=1 "$RUNNER_TEMP/server.test" -test.run '^TestCheckPeer' -test.count=1 -test.v
```

**Saving.** Measured in CI: the step took 34 to 52 s in the 31 runs before #92 and 47 to 50 s in the 3 after, and the tests in it 4.5 to 4.7 s (8 logs). It was 17 to 26 s a week ago: it grows with the dependencies. Measured locally, in the container at four processors: the step as CI runs it took 23.3 s and 49.2 s of CPU; building the binary from the warm cache took 4.3 s and 7.2 s of CPU and running the two tests 6.4 s, 10.7 s in all. That is 54% of the wall time and 84% of the CPU. Estimated for CI, where the step is about twice as long as in the container: 30 to 35 s off every `test` job, and about 80 s of CPU.

**What it could break.** Little. `TestCheckPeerOtherUID` copies its own binary into a directory it makes world-readable under the temporary directory and runs the copy as `nobody` (read: `peer_linux_test.go`), so where the built binary lies does not matter, but the temporary directory must stay one `nobody` can enter: it is `/tmp` today, because `sudo` does not pass `TMPDIR` on, and §2.3 must not change that for this step. `go test` runs a test in its package's directory, which a bare binary does not, hence the `cd`. The package's `TestMain` starts a Dolt whichever tests are asked for, as now. A binary run directly prints nothing for a skipped test unless given `-test.v`; without it, a step that skipped both tests would look like one that ran them.

**How to tell it worked.** The step takes under 15 s, and its log has `--- PASS: TestCheckPeerOtherUID` and `--- PASS: TestCheckPeerKeepsStatusFromOtherUsers`, not `SKIP`. Make the step fail if either line is missing.

**Moves together.** `scripts/gate.sh` line 218 prints the present command for a contributor on Linux to run; the note should print the new one. `CONTRIBUTING.md` ("What a local run cannot tell you") says the gate prints it. The Commands block of `AGENTS.md` does not list this command, so the test that holds the block to the script is not involved.

### 2.2 Build caches that hold what the jobs build (ci.yml)

**What.** All three jobs that set Go up (`lint`, `test`, `codeql`'s `analyze`) use `actions/setup-go`'s cache under one key, the hash of `go.sum`. A key is written once, by the first job to finish, and never again. That job is `analyze` or `lint`, never `test`, which finishes minutes later: in the run that followed the last change to `go.sum`, the `analyze` job's log has `Cache saved with the key: setup-go-Linux-x64-ubuntu24-go-1.27.2-3c55…` and the `lint` job's has `Failed to save: Unable to reserve cache … another job may be creating this cache`. So the 143 MB every job restores is what `go build ./...` leaves: plain Linux objects. It holds nothing compiled with `-race`, which is all `test` needs, and nothing for macOS or Windows, which is what `lint`'s last step builds. Both jobs pay for a restore and then build from nothing.

Give each job a cache of its own: `cache: false` on `setup-go` (lines 29 to 31 and 70 to 72), and `actions/cache`, pinned by commit as rule 4 asks, over `~/.cache/go-build` and `~/go/pkg/mod`, with the job's name and the Go version in the key and the hash of `go.sum` after it. Whether to save on every run of `main` (a key that includes the commit, with a prefix as the restore key) or once for each `go.sum` is the design choice: the first keeps the repository's own packages warm and writes a few hundred megabytes for each merge, which the 10 GB limit then evicts oldest first; the second keeps only the standard library and the dependencies warm and writes once.

**Saving.** Measured in CI: `internal/server`'s tests do not begin until 74 to 94 s into the `go test` step (8 logs: the time its result was printed, less the time it reports), and nothing shorter than that step can end before the build does. `lint`'s builds for other systems took 44 to 77 s in 29 pull request runs. Measured locally, in the container at four processors (the machine was loaded, so the CPU column is the sounder one):

| Build | Wall | CPU |
|---|---|---|
| The standard library with `-race`, from an empty cache | 10.0 s | 33.9 s |
| The third-party modules with `-race` | 5.2 s | 19.3 s |
| The repository's packages and all test binaries, `-race` and coverage | 20.2 s | 46.6 s |
| All of that again, from the cache it left | 0.6 s | 1.4 s |
| The same test build from a cache holding only a plain `go build ./...`, which is what CI restores | 30.5 s | 80.8 s |
| `lint`'s vet and build for macOS and Windows, from that plain cache | 30.5 s | 97.2 s |
| The same again, from the cache it left | 2.0 s | 3.7 s |

So the cache CI restores today saves a race build about a fifth of its CPU (81 s against 100 s) and the cross builds nothing worth naming. A cache written once for each `go.sum` would hold the first two rows, 43% of the build's wall time and 53% of its CPU; one written by each run of `main` would hold nearly all of it for a pull request that touches a leaf package, and less for one that touches a package everything imports. The race cache is 620 MB on disk before compression, against today's 143 MB compressed. Estimated for CI, where the build before `internal/server` starts is 87 to 90 s: 30 to 40 s off `test`'s critical path with the cache written once, 60 to 70 s with the one written by each run, and 35 to 60 s off `lint`, each less 5 to 10 s to restore a larger cache.

**What it could break.** Go's build cache is addressed by the content of its inputs, so a stale entry is not used, only kept (inferred from how the cache works; not tested here). A cache written by a pull request is visible only to that pull request, and `main`'s to all, which is GitHub's rule and protects `main` from a fork. The release's `gate` job calls `ci.yml` and would restore the same caches, as it restores today's; `release.yml`'s `build` job sets `cache: false` and is not affected. The real risk is ordinary: a wrong path or key saves nothing and nobody notices.

**How to tell it worked.** In the `test` job's log, the first package result is printed within about 20 s of the step's start, not 27 to 45 s, and `internal/server`'s result arrives at its own duration plus about 20 s. The cache list (`gh api repos/ariesworx/starfix/actions/caches`) shows a second and third key, and total use stays under 10 GB; it is 1.87 GB today in 37 caches.

**Moves together.** Nothing outside `ci.yml`: no command of the gate changes. `internal/release/workflow_test.go` requires every action to be pinned by commit with a version comment.

### 2.3 The tests' temporary files on memory (ci.yml)

**What.** Each package's Dolt keeps its data under a directory from `os.MkdirTemp`. Set `TMPDIR` for the `go test` step to a memory file system (`/dev/shm`, or a `tmpfs` mounted for the job), and `GOTMPDIR` to a directory on disk so that the test binaries are still built and run from disk.

**Saving.** Measured locally, CI's command at four processors: 87, 95 and 97 s with `/tmp` on memory against 94 to 100 s on disk before the machine was shared, and against 398, 306 and 704 s on disk in the runs that alternated with them while other work loaded the disk, the last of which lost `internal/server` to the ten-minute limit. CPU was the same in all of them (167 to 217 s). So on a quiet machine with a fast disk the gain is under a tenth, and on a busy one the tests on memory do not notice. Which of those a GitHub runner is, this work could not measure. Two things suggest it is not the quiet one: the runner takes 131 s for `internal/server` where the container took 79 s, and the 31 runs before #92 spread from 261 to 444 s for the same work.

**What it could break.** The files count against memory: the container's peak was 2.4 to 2.5 GB with them, no more than without (the disk runs' file cache is counted too), against the runner's 16 GB. A machine short of memory is worse off with them there than on disk: a last run, in a virtual machine that other containers had by then filled, spent 908 s of system time and failed three tests. `/dev/shm` on the runner is assumed to allow what the tests do there (unix sockets, a FIFO in one test); not verified.

**How to tell it worked.** One pull request that changes only this, and its `go test` step against the three runs since #92 (218 to 229 s). If it is not clearly shorter over several runs, drop it: do not keep a setting whose effect cannot be seen.

**Moves together.** Nothing: an environment setting of one step. `scripts/gate.sh` runs on contributors' machines and should not set it.

### 2.4 `timeout-minutes` on every job

**What.** No job or step in any of the four workflows has a `timeout-minutes` (read), so a job that hangs runs to GitHub's own limit for a job, six hours (general knowledge, not read for this work). `go test` ends a package after ten minutes by itself, and that is the only bound there is; the download of Dolt (`curl` with no time limit), golangci-lint, govulncheck, the CodeQL analysis and the release's build have none. Add `timeout-minutes: 20` to `test`, `10` to `lint` and to `vuln.yml`'s job, `15` to `codeql.yml`'s and to `release.yml`'s `build`. `sign-and-publish` waits for the maintainer's approval before it starts; whether that wait counts against a job's timeout was not checked, so leave that job without one.

**Saving.** None in a run that works. The slowest `test` job in 78 read was 515 s and the slowest `lint` 172 s, so the limits are two to three times the worst seen.

**Risk.** A limit set too low fails a slow run that would have passed. These leave room.

### 2.5 Give `mktemp` a template under `$TMPDIR` (scripts)

**What.** `scripts/gate.sh` line 75, `scripts/install-dolt.sh` line 106 and `install.sh` line 107 call `mktemp -d` with no template. On macOS that ignores `TMPDIR` (measured, macOS 27: `TMPDIR=/some/dir mktemp -d -u` printed a path under `/var/folders/…/T`, and `mktemp -d -u /some/dir/gate.XXXXXX` one under the directory given). `scripts/gate_test.go` runs `gate.sh` with `TMPDIR` set to the test's own directory, and `gate.sh` keeps its logs when a command fails, as the tests make it do; so on macOS each `go test ./scripts` leaves six directories outside the directory the test removes. Write the template out: `mktemp -d "${TMPDIR:-/tmp}/gate.XXXXXX"`. Each script's message, "set TMPDIR to a writable directory", is also untrue on macOS until then.

**Saving.** Nothing in CI: on Linux `mktemp` honors `TMPDIR`, and the container runs left nothing. On a contributor's Mac, six directories and about 90 KB for each run of that package.

**How to tell it worked.** On macOS, count `tmp.*` in `$TMPDIR` before and after `go test ./scripts`: equal. A test could assert it, by giving `gate.sh` a `TMPDIR` and checking that the directory it reports its logs in is under it.

### 2.6 Skip the tests for a pull request of design documents only (ci.yml)

**What.** Six of the last 30 pull requests changed nothing but files under `docs/design/` (#66, #68, #78, #79, #84, #85). Their seven runs of `ci` and seven of `codeql` took 49 minutes of run time between them, and each merge then ran both again on `main`. A seventh (#87) changed only `docs/server.md`, and that one could not have been skipped: tests read `docs/server.md`, `docs/cli.md`, `README.md`, `AGENTS.md`, the workflows and `deploy/` and fail when they disagree with the code. No test was found that opens a file under `docs/design/` (read: a search of the tests for the path and for the files they open; three mention the path, as sample data and in a message).

**How required checks make this awkward, and the way through.** `lint` and `test` are required checks of the ruleset on `main`. A workflow that does not start, because of `paths` or `paths-ignore` under `on:`, reports nothing, and a required check that reports nothing stays pending: the pull request can never merge. The filter must therefore not be on the trigger. A job that is skipped by its own `if:` does report, as skipped, and a skipped required check counts as passed (general knowledge of GitHub's behavior, not read or tried for this work: §11). So: keep the trigger as it is; add a first job that lists the files the pull request changes (`git diff --name-only` against the base, no new action) and sets an output; give `test` and `analyze` an `if:` on it. `lint` should still run: it is where `shellcheck` and the Markdown-independent checks are, and it is 2 minutes. The job that decides must say "run" for every event that is not a pull request, so that pushes to `main` and above all `workflow_call` from `release.yml` never skip.

**Saving.** For one pull request in five, the wait goes from 5 minutes to `lint`'s 2, and 6 to 8 runner-minutes are not spent. For the other four, about 10 s more, for the job that decides.

**What it could break.** This is the one proposal that can let a breaking change through: a filter wider than `docs/design/**` would. It also adds a job and conditions to the file every merge depends on. If the share of design-only pull requests falls, the 10 s on every other run is the larger number.

**How to tell it worked.** A pull request touching only `docs/design/` shows `test` as skipped and can merge; one touching `docs/server.md` runs everything; a tag still runs the whole gate.

### 2.7 A merge queue

**What.** The ruleset requires a branch to be up to date before it merges. With two sessions merging, each merge makes every other open pull request stale; someone, or an agent, must update it, wait for a full run and try again, and may lose the race again. A merge queue does the same check (the pull request on top of the `main` it will land on) once, in order, without the loop. `ci.yml` and `codeql.yml` would need `merge_group` among their triggers, and the ruleset the queue turned on, which is the maintainer's setting to change. Merge queues are available to public repositories owned by an organization (general knowledge, not read for this work), which this one is (measured: the API gives the owner's type as Organization).

**Saving.** Not minutes of one run. Today's 1.6 runs for each pull request would stay about that (one on the pull request, one in the queue). What goes is the cascade when three or more are open at once, which the numbers here are too few to show, and the polling.

**Risk.** A new trigger in both workflows and a change to how every merge is made. Worth it only if the two sessions do collide; the record of the last week does not show that they do yet.

### 2.8 The fixed cost of a test

Not a proposal of this document: the work on parallel tests has it. The numbers are in [§4.1](#41-numbers-for-the-work-on-parallel-tests) and [§4.3](#43-fixed-cost-per-test).

### 2.9 Where the numbers say to do nothing

- **Sharding the `test` job across runners.** The step is as long as its build plus its longest package: 87 s plus 131 s. A second runner does not shorten either, and pays the setup and the build again. With `internal/server` on a runner of its own and the rest on another, each shard would still take the build plus its longest package, about 220 s and 175 s (estimated from §3.3), and the setup and most of the build's CPU would be paid a second time. Sharding pays only once `internal/server` itself is split, which is the other piece of work, or its tests made shorter. `internal/release/workflow_test.go` (`TestCIRequiresDolt`) also expects one `go test … ./...` step in the job.
- **`-p`.** `go test` already runs four packages at once on four processors, and three packages are 71% of the work, so there is little for a fifth slot to do. Runs at `-p 1`, `2` and `8` were taken only after the machine was shared, and say nothing usable: the one at `-p 1` took 642 s for 215 s of CPU, waiting on a disk other work was using (§4.5), and the one at `-p 8` ran into the shared virtual machine's memory. Not measured.
- **`-shuffle=on`.** No cost: 94 s with it off, 95 to 96 s with it on.
- **The coverage profile.** `ci.yml` passes `-coverprofile=coverage.out`, and one thing reads it: the `coverage` step, which prints the total as one line in the run's summary. Nothing uploads it, compares it or fails on it. It costs nothing measurable at run time (95 and 96 s with and without; CPU 170 and 167 s). It does mean CI's test command differs from the gate's, which has no profile; `scripts/gate_test.go` says so and allows it.
- **A `-timeout` flag.** After #92 the longest package uses 23% of the default ten minutes ([§8](#8-the-ten-minute-limit)). A shorter limit would only make a slow runner fail. A longer one is not needed.
- **`vuln.yml`.** It repeats `lint`'s govulncheck step, 8 s, once a week, so that a new advisory fails a run without waiting for a change. 24 s a week.
- **The release's rerun of the gate.** Two release runs read: `lint` 87 and 88 s, `test` 144 and 171 s. It is what makes a tag's build independent of what CI said about a branch.
- **The dependency groups.** Dependabot opens at most one grouped pull request for each ecosystem a week; the two this week took two runs each.
- **The runs on `main` after each merge.** With branches required to be up to date, a merge's tree is the tree the pull request's last run tested, so the run repeats it. It also fills the caches every pull request reads, gives code scanning its baseline, and failed 9 times in 90; the two failures opened were tests that depended on timing. That is information, and nothing waits for it.
- **Dropping databases in `cmd/starfixd` and `internal/bdimport`.** [§5.3](#53-cmdstarfixd-and-internalbdimport).
- **The 16 MB buffer in `internal/e2e`'s leak check.** [§9](#9-profiles-of-the-test-binaries).

## 3. The real CI, from GitHub's records

Measured, read-only, with `gh api` and `gh run view --log` on 13 Oct 2026. The repository's first run was on 6 Oct, so "the last week" is all of it: 247 runs of `ci`, 186 of `codeql`, 6 of `release`, 6 of Dependabot's own, 1 of `vuln`.

### 3.1 Jobs and steps

Job and step times are from the check runs of the last 48 commits on `main` and of the head commits of pull requests #73 to #92: 78 `test` jobs, 67 of them successful. Seconds; minimum, median, maximum.

| Job | Step | Pushes to `main` (48) | Pull requests (30) |
|---|---|---|---|
| `lint` | whole job | 64, 96, 170 | 84, 123, 172 |
| | `setup-go` | 8, 11, 20 | 8, 11, 18 |
| | golangci-lint | 11, 20, 61 | 11, 24, 58 |
| | govulncheck | 6, 8, 39 | 6, 8, 19 |
| | builds for macOS and Windows | 30, 47, 93 | 44, 72, 77 |
| | `go mod verify`, shellcheck, AppArmor | 0 to 4 each | 0 to 3 each |
| `test` | whole job (successful) | 103, 237, 515 | 292, 406, 482 |
| | `setup-go` | 7, 10, 22 | 9, 12, 15 |
| | install Dolt | 1, 2, 3 | 1, 1, 3 |
| | `go test` | 63, 190, 444 | 229, 340, 412 |
| | peer check | 17, 28, 50 | 34, 48, 52 |
| | coverage | 0, 0, 1 | 0, 0, 1 |
| `analyze` (CodeQL) | whole job | 61, 86, 127 | 65, 94, 103 |
| | init | 8, 16, 27 | 10, 17, 21 |
| | `go build ./...` | 5, 11, 45 | 8, 16, 34 |
| | analyze | 27, 38, 50 | 27, 42, 45 |
| any | queue, from the run's creation to the job's start | 3, 4, 60 | 3, 4, 66 |

The columns are not comparable with each other: the pushes go back to 8 Oct, when the suite was a third of its size, and the pull requests are all from 12 and 13 Oct.

### 3.2 The trend

Successful `test` jobs, by day. Seconds; minimum, median, maximum.

| | Jobs | `test` job | `go test` step | Peer check |
|---|---|---|---|---|
| 8 to 11 Oct | 11 | 103, 149, 181 | 63, 105, 136 | 17, 21, 26 |
| 12 Oct | 22 | 159, 244, 408 | 121, 196, 344 | 18, 28, 48 |
| 13 Oct, before #92 | 31 | 319, 432, 515 | 261, 362, 444 | 34, 47, 52 |
| After #92 | 3 | 285, 296, 302 | 218, 229, 229 | 47, 48, 50 |

The step more than tripled in five days, as stages 3a and the telemetry slices added tests, and #92 took it back to about where it was on 12 Oct. The peer check doubled in the same days with no change to its two tests: it is a build, and the build grew.

### 3.3 Packages

From the `go test` step's log, which prints each package's time. Five runs before #92 (three of `main`, two of pull requests) and the three after. Seconds.

| Package | Before #92 (5 logs) | After #92 (3 logs) |
|---|---|---|
| `internal/server` | 245 to 354 | 131 to 139 |
| `internal/store` | 154 to 216 | 83 to 88 |
| `internal/e2e` | 66 to 82 | 61 to 69 |
| `cmd/starfixd` | 22 to 32 | 22 to 28 |
| `internal/telemetry` | 11 to 17 | 15 to 18 |
| `internal/client` | 13 to 17 | 17 |
| `internal/bdimport` | 9 to 14 | 10 to 12 |
| `internal/mcpserver` | 7 to 9 | 8 to 11 |
| `internal/dolttest` | 7 to 8 | 7 to 9 |
| `internal/board`, `internal/outbox` | 3 to 5 | 4 to 5 |
| each of the other 17 | 1.0 to 3.3 | 1.0 to 2.8 |
| Sum of all packages | 567 to 775 | 391 to 414 |
| The step | 319 to 444 | 218 to 229 |
| `internal/server`'s tests began, at the latest | 73 to 94 s into the step | 87 to 90 s |

Three packages are 71% of the sum after #92 and were 82% to 84% before. `internal/server` alone decides when the step ends.

### 3.4 Runs and minutes

| Workflow | Event | Runs | Succeeded | Failed | Canceled | Run time, median | Run time, total |
|---|---|---|---|---|---|---|---|
| `ci` | pull request | 157 | 130 | 7 | 20 | 121 s | 464 min |
| `ci` | push to `main` | 90 | 80 | 9 | 0 | 136 s | 285 min |
| `codeql` | pull request | 113 | 107 | 0 | 6 | 91 s | 173 min |
| `codeql` | push to `main` | 72 | 72 | 0 | 0 | 93 s | 111 min |
| `codeql` | schedule | 1 | 1 | 0 | 0 | 74 s | 1 min |
| `release` | tag | 6 | 6 | 0 | 0 | 348 s | 103 min, most of it waiting for approval |
| `vuln` | schedule | 1 | 1 | 0 | 0 | 28 s | 0.5 min |

Run time is from a run's start to its last update, so for `ci` it is the `test` job's length, not the two jobs' sum. The medians are over the whole week and are low for today's suite.

| Day | `ci` runs (`main`, pull request) | `codeql` runs | `ci` run time | `codeql` run time |
|---|---|---|---|---|
| 6 Oct | 2, 14 | 0 | 14 min | 0 |
| 7 Oct | 33, 58 | 45 | 140 min | 72 min |
| 8 Oct | 9, 11 | 20 | 35 min | 29 min |
| 9 Oct | 4, 5 | 9 | 24 min | 13 min |
| 10 Oct | 0, 2 | 2 | 5 min | 3 min |
| 11 Oct | 5, 10 | 15 | 38 min | 19 min |
| 12 Oct | 23, 34 | 57 | 245 min | 89 min |
| 13 Oct | 14, 23 | 37 | 247 min | 60 min |

**Runner-minutes** (estimated from the job medians of 13 Oct): one push to a pull request costs `lint` 2.0, `test` 7.2 before #92 and 4.9 after, and `analyze` 1.5: about 10.7 minutes of runner before and 8.4 after, or 13 and 10 as GitHub would bill them, a minute at a time for each job, if it billed. On 13 Oct that was about 390 runner-minutes for 37 runs of each workflow.

**Billing.** The API's timing for a run of 13 Oct (`actions/runs/38449901895/timing`) reports `billable.UBUNTU.total_ms` of 0 for its two jobs. Every job in the four workflows runs on `ubuntu-24.04`. GitHub's documentation ("GitHub-hosted runners", read 13 Oct 2026, [docs.github.com/en/actions/reference/runners/github-hosted-runners](https://docs.github.com/en/actions/reference/runners/github-hosted-runners)) says that use of the standard runners is free and unlimited on public repositories, and gives the Linux one for public repositories 4 processors, 16 GB of memory and 14 GB of disk.

**For each pull request.** The last 30 (#63 to #92) took 48 runs of `ci` and 48 of `codeql` on the pull request event: 1.6 each, 9.1 minutes of `ci` run time and 2.5 of `codeql` for each pull request, and one more run of each on `main` when it merged. They were open for 4 to 159 minutes, median 22.

**Canceled and failed.** 20 runs of `ci` and 6 of `codeql` were canceled, all on pull requests: the `concurrency` groups do cancel a run that a newer push supersedes. Two runs were ever run a second time. Seven of the 9 failures on `main` were between 12 Oct 16:43 and 23:47 UTC; the two logs opened failed on `TestLocalStatus` (`internal/server`) and `TestServeCaps` (`internal/telemetry`), both tests of timing, the kind #92 set out to rewrite.

## 4. Where the test time goes

### 4.1 Numbers for the work on parallel tests

The file `numbers/per-test-seconds.tsv` in the scratch directory ([§10](#10-method-and-where-the-raw-files-are)) has one row for each of the 1,116 top-level tests and each package, with its seconds in five runs: the container at CI's shape with and without the coverage profile, without the race detector, with one package at a time (loaded), and the host. From the container, CI's command, before the machine was shared:

| Package | Seconds | Top-level tests | Mean | Tests of 1 s or more | Tests under 0.1 s |
|---|---|---|---|---|---|
| `internal/server` | 79 | 221 | 0.35 s | 14, which are 51 s of it | 108 |
| `internal/e2e` | 42 | 79 | 0.51 s | 2: `TestSoak` 23 s, `TestCheckLeaksBaseline` 2.7 s | 26 |
| `internal/store` | 41 | 277 | 0.14 s | 2 | 172 |
| `cmd/starfixd` | 9 | 45 | 0.17 s | 0 | 20 |
| `internal/client` | 9 | 31 | 0.26 s | 5 | 26 |
| `internal/telemetry` | 8 | 58 | 0.15 s | 4 | 46 |
| `internal/bdimport` | 4 | 11 | 0.27 s | 1 | 4 |
| `internal/dolttest` | 5 | 12 | 0.31 s | 1 | 3 |
| the other 20 | 1 to 4 each | | | 0 | |

`internal/server` is two different things. Fourteen tests wait on the clock on purpose and take 51 s between them; the other 207 take 26 s, 0.13 s each. `internal/store` has no such tail: 277 tests at 0.14 s, nearly all of it the fixed cost of §4.3.

### 4.2 The slowest tests

The container at CI's shape, with the race detector, before the machine was shared. What each waits for is from the test's own comment where it has one and from its name where it has none: a guide to where to look, not a measurement.

| Seconds | Test | What the time is |
|---|---|---|
| 23.3 | `e2e.TestSoak` | By design: about 12 s of work by 25 sessions, then the checks of every log |
| 13.3 | `server.TestDeadCollector` | Requests timed in turns against a collector that is dead, and its export timeouts |
| 8.4 | `server.TestNewConnectionWithDoltFrozen` | Dolt is stopped with a signal; each refusal waits out the bound it tests |
| 4.4 | `server.TestStatusWithDoltFrozen` | The same, for the status report's bound of two seconds |
| 4.2 | `server.TestDeadCollectorLogs` | As `TestDeadCollector`, for exported logs |
| 3.2 | `store.TestDivideTimeGrowsWithHolds` | CPU: it times an algorithm at two sizes, many times, to compare the fastest |
| 3.2 | `server.TestSessionNotedAgainAfterFailure` | Waits out a write that is made to fail by running out of time |
| 3.0 | `server.TestSecSizes` | Frames at and past the size caps |
| 2.7 | `e2e.TestCheckLeaksBaseline` | A soak world, and the leak check's wait |
| 2.6 | `server.TestStatusOneStoreSlow` | One store's database made not to answer, to the report's bound |
| 2.4 | `client.TestWhichKeysAreOffered` | A real ssh-agent and SSH handshakes for each way a key is configured |
| 2.3 | `client.TestDefaultIdentities` | The same |
| 2.2 | `server.TestSessionNotedBeforeFirstStoreOp` | Waits as long as the report waits on Dolt |
| 2.1 | `server.TestCheckPeerKeepsStatusFromOtherUsers` | Starts the test binary again as another user |
| 2.1 | `server.TestConnClosedWhileSessionNoted` | A note that is held until the connection closes |
| 2.1 | `server.TestExportedLogsCarryNoText` | Every op once, with exported logs read back |
| 2.0 | `telemetry.TestDeadCollector` | Export timeouts |
| 1.8 | `telemetry.TestDeadCollectorLogs` | Export timeouts |
| 1.5 | `server.TestShowAndBlockedCapEdges` | Hundreds of edges written to reach the caps |
| 1.4 | `telemetry.TestDeadCollectorTraces` | Export timeouts |
| 1.3 | `telemetry.TestSeriesAreBounded` | Every combination of attribute values |
| 1.2 | `bdimport.TestImportProblems` | Imports, one database |
| 1.2 | `client.TestKeyRefusals` | SSH handshakes that are refused |
| 1.2 | `client.TestDialKeyRefusals` | The same |
| 1.2 | `store.TestFencingAcrossClockSteps` | A takeover at every point of a clock step: many stores |
| 1.1 | `server.TestAdminDrops` | Many connections, each ended a different way |
| 1.1 | `dolttest.TestStopLeavesNoWatcher` | Starts and stops a Dolt |
| 1.1 | `server.TestCheckPeerOtherUID` | Starts the test binary again as another user |
| 1.0 | `client.TestDialIAPRefusals` | A fake `gcloud`, run for each refusal |
| 1.0 | `store.TestReadyScanBound` | Hundreds of issues written |

These thirty are 100 s of the 227 s all packages took. On the host (macOS) the order is different, because there a write to Dolt costs far more than waiting does: `bdimport.TestImportProblems` took 25 s, `server.TestAdminDrops` 22 s and `mcpserver.TestRenewPathIssuesCap` 20 s.

### 4.3 Fixed cost per test

Measured by a test written for this, in a scratch copy of the repository (`zz_fixedcost_test.go`): one Dolt, then forty rounds of what `newStore` does and undoes. Medians.

| | Start Dolt, once | `CREATE DATABASE` and `GRANT` | Open a store (15 migrations, 48 statements) | One `CreateIssue` | Close | `DROP DATABASE` | Each test, in all |
|---|---|---|---|---|---|---|---|
| Host, macOS, on disk (**loaded**) | 278 ms | 169 ms | 334 ms | 9 ms | 8 ms | 43 ms | 554 ms |
| Container, Linux, on disk (**loaded**) | 245 ms | 229 ms | 485 ms | 11 ms | 6 ms | 39 ms | 759 ms |
| Container, Linux, `/tmp` on memory (**loaded**) | 135 ms | 4 ms | 26 ms | 3 ms | 1 ms | 1 ms | 32 ms |

The third row is the finding. The work of setting a test up and taking it down is 32 ms; everything above that in the other rows is Dolt waiting for the disk to confirm each write, and it is 17 to 24 times the work. No row was measured on disk on a quiet machine: there, `internal/store`'s 277 tests average 0.14 s each in the container, all told, so the fixed cost was at most that.

Without the drop, as before #92, the same steps slow as Dolt holds more, disk or no disk: with `/tmp` on memory, opening a store took 28 ms with the first twenty databases held, 50 ms with fifty to seventy and 95 ms with a hundred or more; on the host's disk 544 ms and 1,029 ms. Stopping a Dolt that held 120 took 91 ms on memory, 3.6 s on the container's disk and 6.5 s on the host's.

So a store test's own work is small beside its setup, and its setup is small beside the waiting. The migrations are the largest part of both, the same 48 statements each time: 26 of the 32 ms, and 334 of the 554.

### 4.4 The race detector, shuffle and coverage

The container, four processors, before the machine was shared, each but the first from a warm build cache. Seconds.

| Run | Wall | CPU | Dolt CPU | Test binaries' CPU | `server` | `store` | `e2e` |
|---|---|---|---|---|---|---|---|
| CI's command, first run (build cache empty) | 100 | 174 | not sampled | | 83 | 50 | 47 |
| CI's command | 95 | 170 | 80 | 78 | 79 | 41 | 42 |
| Without `-coverprofile` | 96 | 167 | 80 | 74 | 79 | 41 | 49 |
| Without `-shuffle` | 94 | 170 | 82 | 76 | 79 | 38 | 43 |
| Without `-race` | 67 | 128 | 88 | 27 | 60 | 29 | 36 |

The race detector is the only one of these that costs anything: 28 s of wall time in 95 and 42 s of CPU in 170, all of it in the test binaries. It is also the one nobody would give up. Coverage and shuffle are free. One sample each, except CI's command.

### 4.5 The disk

CPU time and wall time tell different stories, and the difference is the disk.

- On the host, `go test` of `internal/store` alone, without the race detector, took 284 s and used 48 s of CPU, Dolt's included: five sixths of the time nothing was on a processor. `internal/server` took 387 s for 64 s of CPU. The same packages take 41 and 79 s in the Linux container. Dolt makes its writes durable, and on macOS that is slow (inferred: the system calls were not traced).
- In the container, with other agents loading the machine, CI's command took 398, 306 and 704 s with its files on the container's disk, for 189, 208 and 217 s of CPU. With `/tmp` on memory, in the runs that alternated with those, it took 87, 95 and 97 s for 182, 203 and 194 s of CPU.
- The third of those disk runs is the kind of failure the maintainer saw come close in CI. `internal/server` was stopped by `go test` at ten minutes, 174 of its 221 tests done and none failed, in a run that used no more CPU than any other. Nothing was wrong but the disk.
- The fixed cost of a test is 32 ms with its files on memory and 554 to 759 ms on a loaded disk (§4.3).

So what `go test` waits for is mostly Dolt's disk, not a processor, and a run's length depends on what else is using the disk more than on anything in the tests. That is one reading of why 31 runs of the same suite in CI spread from 261 to 444 s; it is not verified ([§11](#11-not-verified)).

## 5. Memory

### 5.1 Before and after #92

Peak resident memory of each package's Dolt: in the container the kernel's own record of each process's peak, on the host the largest of samples taken twice a second. Megabytes.

| Dolt of | Before #92, host | Before #92, container (7 GB limit) | After #92, container | After #92, host |
|---|---|---|---|---|
| `internal/server` | 5,599 | 5,749 | 512 | 502 |
| `internal/store` | 4,443 | 3,510 when it was killed | 382 | 348 |
| `internal/e2e` | 1,402 | 1,462 | 918 | 497 |
| `cmd/starfixd` | 1,351 | 1,405 | 1,389 to 1,566 | 1,380 |
| `internal/bdimport` | 522 | 566 | 559 | 502 |
| a fresh Dolt (`internal/dolttest`, `internal/doltmetrics`) | 77 to 121 | | 98 | 58 to 77 |
| Every Dolt and test binary together, at the worst moment | 8,815 (five packages at once) | the limit | 1,962 to 2,180 | 2,884 |

The run of the parent commit in the container reached its 7 GB limit and the kernel killed `internal/store`'s Dolt, which failed 75 tests: the machine's Docker has 7.7 GB in all, so "enough memory not to be the limit" was not available for that run, and the host's figures stand in for it. The test binaries are small beside Dolt: the largest is `internal/e2e`'s, 362 MB with the race detector and 117 MB without; `internal/server`'s is 281 MB; every other is under 130 MB.

### 5.2 Headroom on the runner

The runner has 16 GB (read, §3.4), and all three jobs use that runner. Before #92, `internal/server` and `internal/store` ran side by side for minutes with Dolts of 5.6 and 4.4 GB, with `internal/e2e`'s and `cmd/starfixd`'s 1.4 GB each beside them early on: 9 to 13 GB at the worst, so 3 to 7 GB to spare, and less with each test added (inferred from the peaks; the runner's own memory was not measured). After #92 the same sum is about 2 GB, and the container's peak with file cache 2.4 to 3.4 GB: 12 GB or more to spare.

### 5.3 `cmd/starfixd` and `internal/bdimport`

Neither drops its databases, and the two are different cases.

- **`cmd/starfixd`** now has the largest Dolt of the run, 1.4 to 1.6 GB, about a hundred databases' worth at 14 MB each. Its tests ask their own helper for a database in only 14 places; the rest are project databases that `starfixd control init` and the daemon create, as the product does, so dropping them is not one line in a helper. It costs memory, not time: the package takes 9 s in the container and 22 to 28 s in CI, before and after #92 alike. With 12 GB to spare, **do nothing**, unless the work on parallel tests is in that package anyway.
- **`internal/bdimport`** has 11 tests and so 11 databases, about 150 MB of databases at most. Its Dolt reaches 560 MB because of what the imports write, not how many databases it holds (inferred from the count). Dropping them would not change it. **Do nothing.**

`internal/e2e`'s Dolt, 0.5 to 0.9 GB with every database dropped, is the same: a Dolt that has done a soak's writes holds that much whatever is dropped afterward.

## 6. What a run leaves behind

### 6.1 After a full run

Host: `go test -race -shuffle=on -count=1 ./...` in the worktree, before the machine was shared, with a list of everything before and after. Container: the same after each of the runs of §4.4, in a container that ran nothing else.

| | Host (macOS) | Container (Linux) |
|---|---|---|
| Processes: `dolt sql-server`, test binaries, watcher shells, ssh-agents, `gcloud` stand-ins | None new. The one ssh-agent was the machine's own | None: only the measuring script |
| Listening TCP ports | None new | None |
| The temporary directory | Six directories `tmp.XXXXXXXXXX`, 88 KB, from `scripts.TestGate…` through `gate.sh` (§2.5). Nothing from any Dolt-backed test: no `starfix-dolt-*`, `starfix-server-*`, `starfix-e2e-*`, `sf*` | Nothing but the coverage profile the command named |
| `/tmp` | Nothing new | (the same directory) |
| The home directory, `~/.ssh`, `~/.config`, `~/.cache` | Nothing new | Nothing from tests: `.cache` is the Dolt download, `.dolt` and `.config` are from the measuring script's own `dolt version` and from `go` |
| The repository | `git status` clean | Mounted read-only, and every test passed, so nothing tries to write there |
| Docker objects | None: no test uses Docker. `deploy/docker/smoke.sh` does and is not a test | |
| Go build cache | 531 MB after this one run, from empty | 519 MB after one run; 1,155 MB after the variants |

The directories that were already there say how it accumulates: 478 `tmp.*` directories, each holding `gate.sh`'s `notes` file, 369 dated 12 Oct and 109 dated 13 Oct, 7.1 MB. That is about eighty runs of `go test ./scripts` in two days on one machine.

### 6.2 After a test binary is killed

`dolttest` starts a watcher for each Dolt, a shell that kills Dolt and removes its files when the process that started it is gone. Confirmed, on the host and in the container, by starting `go test ./internal/store ./internal/server`, waiting until both packages had a Dolt, and following those processes and their directories by process id:

| What was done | Test binaries | Their two Dolts and two watchers | The Dolts' directories | Else |
|---|---|---|---|---|
| `SIGKILL` to both test binaries (host, and container twice) | Gone | Gone within a second | Removed within a second | In the container, one directory of 20 KB: the `t.TempDir` of the test that was running, which only its own process could have removed |
| `go test -timeout 25s`, so that the binaries end themselves with a panic (container) | Gone | Gone within a second | Removed within a second | |
| `SIGKILL` to `go test` itself, and not to its children (container) | **Still running**, as orphans, to the end of their tests: 125 s here | Running with them, then stopped as usual | Removed when they ended | |

The third row is the one to know about. A harness that stops a test run by killing only `go test` leaves each test binary running its whole package with a Dolt beside it, for as long as the package takes and at most the ten minutes `go test`'s limit allows, which the binary enforces itself. They do end, and they clean up when they do. Ctrl-C does not do this: the terminal signals the whole group. Nothing in the repository kills `go test` that way; an agent's harness might.

### 6.3 Who cares

A CI runner is a new virtual machine for each job, so nothing a test leaves there outlives the job; what matters on a runner is only what a leftover does during the job, which is memory, and #92 dealt with that. Each of the eight `test` job logs read ends with the runner's own "Cleaning up orphan processes" and names none.

A contributor's machine keeps everything. There the things to watch are, in order: the Go build cache, which only `go clean -cache` shrinks and which grows by hundreds of megabytes for each variant of flags; a Dolt that outlives a killed test, which the watcher handles; and small directories that nothing removes, of which the `tmp.*` ones on macOS are the only kind found.

## 7. The workflows, read critically

### 7.1 Caching

| Cache | Key | What is in it | Hit | Worth |
|---|---|---|---|---|
| `setup-go`, all three jobs | Go version and the hash of `go.sum` | 143 MB: the module cache, and the build cache as `analyze` or `lint` left it | Every run but the first after `go.sum` changes | The modules are not downloaded again. The build cache serves `analyze`'s `go build`, and neither `test` nor `lint`'s cross builds (§2.2) |
| golangci-lint's own | A number that changes weekly, and the hash of `go.mod` | 2 MB of analysis results | Every run | The step takes 11 to 61 s, median 20 to 24 s; its log does not say how long it takes without |
| CodeQL | None | Its log: "go does not support TRAP caching", "Using overlay database mode: none without caching" | | |
| Dolt | None | A 2 s download, verified by SHA-256 | | Not worth a cache |
| Go itself | None | `setup-go` downloads 1.27.2 in each job, about 6 s: the runner's image has an older one | | Not worth a cache |

The repository holds 37 caches, 1.87 GB of the 10 GB allowed. Each pull request that changes `go.sum` writes its own copy.

### 7.2 Work that is done more than once

- **The race build, twice** in the `test` job: once by `go test`, once by the peer check as root (§2.1).
- **The code is compiled by all three jobs**, for different ends: `analyze` plain, for CodeQL's extractor; `lint` for golangci-lint's analysis and then for macOS and Windows; `test` with the race detector and coverage. None can use another's output, and only the first is cached.
- **govulncheck** runs in `lint` (8 s), weekly in `vuln.yml`, and in the release's `build`. Each has a different trigger; the cost is seconds.

### 7.3 Jobs for changes that cannot affect them

Six of the last 30 pull requests changed only `docs/design/`, and all ran `test` and `analyze` in full (§2.6). No workflow has a path filter.

### 7.4 Concurrency

`ci` and `codeql` cancel a pull request's run when a newer push supersedes it and never cancel a run on `main`; the record shows 26 cancellations and none on `main`. `release` never cancels. `vuln` has no group and needs none.

### 7.5 Timeouts

None anywhere (§2.4).

### 7.6 Dependency updates

Weekly on Monday, minor and patch updates grouped, one group for Go modules and one for Actions. This week: two pull requests, two runs each, the second because the other had merged.

### 7.7 The release

`release.yml` calls `ci.yml` on the tag: two runs read, `lint` 87 and 88 s, `test` 144 and 171 s, before the `build` job's 122 to 155 s. The build sets `cache: false` on purpose.

## 8. The ten-minute limit

`go test` ends a package that runs for ten minutes. Where each stands in real CI, as a share of 600 s:

| Package | Before #92 (5 logs) | After #92 (3 logs) |
|---|---|---|
| `internal/server` | 41% to 59% | 22% to 23% |
| `internal/store` | 26% to 36% | 14% to 15% |
| `internal/e2e` | 11% to 14% | 10% to 11% |
| every other | 5% or less | 5% or less |

The run the maintainer recalls, 9 minutes 41 seconds, is not among the logs read. The worst of the five was 354 s, on a run whose every package was slow. With the worst run before #92 at 1.4 times the median, the worst to expect now from that kind of variation is about 200 s, a third of the limit.

That is not the whole of it. One local run of the code after #92 did pass the limit: `internal/server` at 601 s, in the container, while other work was using the machine's disk (§4.5). The package's time is set by how fast Dolt's disk answers, and nothing in the tests bounds that. A runner whose disk is as busy as that machine's was would fail the same way; whether one ever is, the records cannot say.

A `-timeout` is still not warranted in either direction. Raising it would hide the next regression of the kind #92 fixed, which the limit is how one would notice, and would not have saved a run seven times slower than usual. Lowering it would turn a slow runner into a failure. What takes the disk out of the question is §2.3, and what is missing besides is a limit on the job (§2.4), for the hangs `go test` does not see.

## 9. Profiles of the test binaries

Go's own profilers, on the host, without the race detector, which distorts both time and memory: `go test -count=1 -cpuprofile … -memprofile …` for each of the four heaviest packages, one at a time, in a scratch copy. The machine was loaded while these ran, which changes the wall times and not the shares.

Each sample was given to the starfix function nearest the work: **harness** if that function is in a `_test.go` file or in `internal/dolttest`, `internal/clienttest`, `internal/iaptest` or `internal/release/releasetest`; **product** if it is product code, whatever called it; **neither** if the stack has no starfix function at all, which is the collector, the scheduler waking goroutines, and the libraries' own goroutines (the SSH key exchange, the MCP SDK's readers).

| | `internal/store` | `internal/server` | `internal/e2e` | `cmd/starfixd` |
|---|---|---|---|---|
| Wall time, alone | 284 s | 387 s | 168 s | 115 s |
| CPU of `go test` and everything under it, Dolt included | 48 s | 64 s | 38 s | 16 s |
| CPU of the test binary (profile samples) | 4.7 s | 7.6 s | 5.9 s | 1.0 s |
| as a share of wall time | 1.7% | 2.0% | 4.0% | 1.0% |
| of which harness | 6% | 3% | 8% | 16% |
| of which product | 27% | 26% | 18% | 15% |
| of which neither | 68% | 71% | 74% | 70% |
| Allocated in the whole run | 367 MB | 617 MB | 1,612 MB | 87 MB |
| of which harness | 6% | 10% | 58% | 35% |
| of which product | 91% | 85% | 26% | 45% |
| In use at the end | 2.5 MB | 3.0 MB | 7.5 MB | 4.6 MB |

**CPU.** The test binaries are idle. Between 96% and 99% of the time no goroutine of theirs is on a processor, and of the little CPU they use two thirds is the runtime parking and waking goroutines around a socket (`runtime.kevent`, `pthread_cond_wait`, `pthread_cond_signal` and the system call itself are 80% of `internal/store`'s samples). The harness's own largest items are `dolttest.Server.DropDatabase` and `NewDatabase` (3.4% of `internal/store`'s 4.7 s, which is 0.16 s), `e2e.(*world).serveSSH` (2.5% of 5.9 s) and `cmd/starfixd.newSite` (7% of 1.0 s). Keys are not a cost: every key the tests make is ed25519, made in microseconds, and key generation does not appear; the SSH key exchange is 12% of `internal/e2e`'s 5.9 s, which is 0.7 s over 79 tests.

**Allocations.** Two things stand out, and neither matters.

- `internal/e2e.connGoroutines` (`soak_final_test.go` line 837) allocates a 16 MB buffer on every call, to hold the stacks of all goroutines, and `checkLeaks` calls it in a loop: 832 MB, 52% of everything that test binary allocates. A buffer kept between calls would remove it. The saving is a few tenths of a second of clearing memory and some of the 117 MB peak; **not worth a change on its own**, and worth knowing for whoever next edits the leak check.
- `internal/release/releasetest.TarGz` is 30% of `cmd/starfixd`'s allocations, which is 26 MB.

The largest product items are the ones the tests are there to exercise: `store.divide` is 54% of `internal/store`'s allocations (the tests of cost division at size), and `proto.NewDecoder` 20% of `internal/server`'s and 7.5% of `internal/e2e`'s, a buffer for each connection opened. Those belong to the profiling of the product and are only named here.

**The finding.** There is nothing in the Go of the tests or the harness worth optimizing for CI. A test binary that is on a processor 2% of the time cannot be made meaningfully cheaper; with the race detector its share rises to about 45% of the CPU (§4.4), which is the detector's price and not the harness's. The cost of the suite is the number of times it asks Dolt to create, migrate and drop a database, and how long Dolt's disk takes to answer: §4.3 and §4.5.

The profiles are sampled at Go's default rate (one allocation in every 512 KB), so counts of objects are estimates and small sites are missed; the totals and the large items are sound.

## 10. Method, and where the raw files are

Everything is in a scratch directory on the machine that ran it, outside the repository: a temporary directory that will not outlive the machine's cleanup.

| Directory | Holds |
|---|---|
| `gh/` | GitHub's records: every run of each workflow, the check runs and steps of 48 commits on `main` and of pull requests #73 to #92, the files of the last 40 pull requests, the caches, the ruleset, eight `test` job logs, two `lint` and one `analyze`, and GitHub's page on runners |
| `out-c/` | Each container run: `go test -json`, a line a second for every Dolt and test binary (resident memory, its peak, CPU), and a report of wall time, the container's CPU and memory, and what was left |
| `out/`, `snap/` | The host runs and the lists of the host before and after |
| `numbers/per-test-seconds.tsv` | Every test's and package's seconds in five runs (§4.1) |
| `prof/` | The profiles of §9 (`t-store`, `t-server`, `t-e2e`, `t-starfixd`: `.cpu`, `.mem`, the test binary, and a `.summary`). Also `ops-count-50.txt`, a count of round trips to Dolt for each op, taken before the profiling of the product passed to other work, and not written up here |
| `scripts/` | What produced each of them |
| `before-src/`, `after-src/`, `prof-src/` | `git archive` of `7ea0d7e` and of `32a6eaf`, and a copy of the second with the measuring tests added (`zz_*`). None is a git checkout |

**The container** is CONTRIBUTING.md's, at CI's shape rather than a slow machine's: `docker run --cpus 4 --memory 7g -e GOMAXPROCS=4 golang:1.27-trixie`, the source mounted read-only, Dolt 2.4.2 from `scripts/install-dolt.sh`, `go test -race -shuffle=on -coverprofile=… -p 4 ./...`, with the module and build caches on volumes. It is linux/arm64 on an Apple M4 Max; the runner is linux/amd64. 7 GB is all the machine's Docker has.

**The host** is darwin/arm64 with 16 processors and 64 GB, Go 1.27.2, Dolt 2.4.2, with a build cache of this work's own.

**CPU time** in the container is the container's own accounting (`cpu.stat`), and each process's from `/proc`; on the host, `/usr/bin/time -l`. **Memory** is each process's peak resident size from `/proc` (`VmHWM`) in the container and sampled on the host.

**Samples.** In the container, before the machine was shared: CI's command twice, and one run of each variant of §4.4, the four with the race detector between 94 and 100 s. After that, the runs marked loaded. Real CI: the counts given with each figure.

## 11. Not verified

- **Anything on a real runner.** No workflow was run, changed or triggered. Every saving in §2 is an estimate from GitHub's step times and from an arm64 container that is about twice as fast as the runner.
- **That the runner's disk is what makes CI's times vary** (§4.5). It fits what was measured locally. The runner's disk was not measured, and a busy neighbor's processors would look the same from here.
- **That Dolt's waiting on macOS is `fsync`.** The CPU is idle and the disk is the difference between the host and the container; no system call was traced.
- **`/dev/shm` on the runner**: its size, and whether everything the tests do in a temporary directory works there (§2.3).
- **That a required check whose job is skipped lets a pull request merge under this repository's ruleset** (§2.6). It is how GitHub is generally understood to behave; its documentation was not read for this and it was not tried.
- **The cache's behavior with a key for each job** (§2.2): sizes, restore times, and eviction under the 10 GB limit were not measured, only the local build cache's size.
- **The store's peak before #92 in a container**: the run hit the 7 GB limit. The host's 4.4 GB stands in.
- **amd64.** Nothing was run on it.
- **`-p`**, and what a package takes when it runs alone: the runs were taken after the machine was shared and are not usable (§2.9).
- **The fixed cost of a test on a quiet disk** (§4.3): all three rows are from a loaded machine. The row on memory does not depend on the disk; the two on disk do.
- **The commands of §2.1 on a runner.** In the container the built binary ran as root, as the container's user is, and the two tests ran; `sudo` from another user was not tried.
- **A memory disk on macOS**, where the waiting is longest: not tried, since it means creating a device on the host.
- **Memory of the runner itself** during a job: only what the logs show, which is nothing.
- **Wall times after 14:25**, when other agents shared the machine, except as marked.
