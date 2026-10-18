# CI gates: checks and landing from CI

A [ci gate](concepts.md#ci-gates) holds work back until CI reports a
check, such as `build` or `test`, passed on the issue the work depends
on. It is also how Aries tells *closed* from *landed*: an agent closes
an issue when it thinks it is done, and the check `landed` is reported by
the job on the default branch after the merge.

CI reports the way any client does: the job runs `aries` as a principal of
its own. ariesd polls nothing, receives no webhook, and calls no CI
host but to read the platform's published signing keys, which check a
job's token ([design §20.4](design/aries.md#204-how-ci-results-reach-the-server)).
Read [What this costs](#what-this-costs) before you set it up.

## Set it up

1. **A service principal for CI, signed in by a trust rule.** On a
   server with HTTPS whose operator set up the platform under
   `workload: providers` ([Service principals](server.md#service-principals)),
   a GitHub Actions job signs in with the OIDC token GitHub gives it,
   and nothing is stored anywhere. A server admin adds a principal of
   kind `service`, owned by the person who answers for it, and a trust
   rule that names the repository by its immutable ids:

   ```sh
   aries admin principal add ci-example --kind service --owner alice
   aries admin trust add ci-example-ci --principal ci-example --provider github-actions \
       --claim repository_owner_id=111111111 --claim repository_id=123456789
   ```

   `gh api repos/example/app --jq '.owner.id, .id'` prints the two ids.
   A rule matches a token whose claims each equal the rule's, exactly; it
   must pin both ids, which GitHub never reuses, where a repository's
   name can be renamed or taken again. Pinning more narrows it, as
   [What this costs](#what-this-costs) says: `--claim
   event_name=pull_request` for a rule that admits pull requests' jobs
   alone, `--claim ref=refs/heads/main --claim event_name=push` for the
   `landed` job, or `--claim environment=release` for a job of a
   protected environment. A job of a pull request carries
   `ref=refs/pull/N/merge`, so a rule pinned to `refs/heads/main` does
   not admit it. `aries admin trust show NAME` prints what a rule pins,
   and a job no rule admits is refused with the claims of its own token
   a rule would pin.

   A CI system with no workload identity, such as GitLab CI, or a
   server that serves SSH alone, uses a key of the principal's own
   instead, with an end:

   ```sh
   ssh-keygen -t ed25519 -N '' -C ci-example -f ci-example
   aries admin principal add ci-example --kind service --owner alice   # a server of many projects
   aries admin key add ci-example ci-example.pub --expires 90d
   ```

   On a server of one project, add a line to `authorized_keys` instead
   ([Principals](server.md#principals)). Store the contents of
   `ci-example` as the secret `ARIES_CI_PRIVATE_KEY`, then delete both
   files. Never name the secret `ARIES_KEY_FILE`, which `aries` reads as
   a path.

2. **A path from CI to the server.** The job connects as `aries` does
   from a laptop, with the repository's committed `.aries.yaml`: over
   HTTPS by `server.url`, or to sshd on a server that serves SSH, at a
   public address or through
   [Google Cloud IAP](cli.md#servers-without-a-public-ip-google-cloud-iap)
   with `gcloud` signed in within the job.

3. **The gates.** Whoever plans the work adds them; agents cannot:

   ```sh
   # ar-c3d4 waits until CI reports build passed on ar-a1b2's branch
   aries gate add --kind ci --check build --issue ar-a1b2 --approver ci-example --blocks ar-c3d4
   # record whether ar-a1b2 landed; nothing waits on it
   aries gate add --kind ci --check landed --issue ar-a1b2 --approver ci-example
   ```

   An issue has one ci gate for each check. To hold another issue behind
   the same check, `aries dep add ar-e5f6 GATE`.

4. **The workflow.** Add one of the examples below. Each runs on every
   pull request or merge request and on the default branch, and passes
   `--optional`, so a branch that names no issue, or an issue with no gate
   for the check, reports nothing and does not fail the job.

## How a job finds its issue

`aries gate pass --check build --from-branch` reads the branch from the CI
system's variables (`GITHUB_HEAD_REF`, `GITHUB_REF_NAME`,
`CI_MERGE_REQUEST_SOURCE_BRANCH_NAME`, `CI_COMMIT_BRANCH`, in that
order), since CI checks out a detached HEAD, and else from git. The
server reads the issue from the branch with the project's prefix, as
`aries start --new-branch` names one: `feature/ar-a1b2-parse-the-config` is
`ar-a1b2`, and so is `alice/feature/ar-a1b2`.

After a merge the default branch names no issue, so the `landed` job
passes `--from-commit`: the issue named by HEAD's `Aries: ar-a1b2`
trailer, or else the branch a merge commit merged (`Merge pull request
#12 from example/feature/ar-a1b2-…`, `Merge branch 'feature/ar-a1b2-…'
into 'main'`). A squash merge keeps neither unless the squashed message
holds the trailer: on GitHub, make the default squash message the pull
request's title and description, and put `Aries: ar-a1b2` in the
description.

To keep a pull request from merging without the trailer, this
repository runs `internal/tools/trailer` on every pull request
(`.github/workflows/trailer.yml`, which a project can copy: it needs the
tool, which reads the commits with `git log` and the trailer with
`internal/gitx`, as `aries land` does). It fails unless a commit from the
pull request's base to its head, merge commits left out, ends with an
`Aries: ar-a1b2` trailer, and every such trailer names the same issue.
Its failure says which fix to make: amend a commit with `git commit
--amend --trailer "Aries: ar-a1b2"`, or push an empty one (`git commit
--allow-empty -m "Name the issue" --trailer "Aries: ar-a1b2"`); the
squash keeps either. A pull request with no Aries issue takes the label
`no-issue`, which only someone with write access can add; the check runs
again when a label changes, and the exemption is printed. Dependabot's
pull requests are skipped by the workflow itself. The job is skipped on
the merge queue's run, so that the check can be required in branch
protection, which is a repository setting and not part of the workflow.

`--issue ID` and `--branch NAME` name the issue directly, and `aries gate
pass GATE` reports on a gate by its id.

## Who may report

Only a principal the gate names (`--approver`), and only from a CI job's
session or that principal's own terminal: never from an agent's session.
`aries` runs in the session `ci` where `CI=true`, as GitHub Actions and
GitLab CI set it, unless an agent harness names its own session. Nobody
else passes or fails a ci gate, an admin included: an admin waives one
(`aries gate waive GATE --reason TEXT`), as any gate. Human gates are
unchanged: only their approvers, from their own terminals; the session
`ci` does not pass one.

A pass closes the gate, and the issues it held become ready. A pass of a
closed gate changes nothing, so a rerun is harmless. A failure keeps its
evidence (`--sha`, `--url`, `--reason`), leaves the gate open, and puts
a `gate.failed` item, with the evidence, in the inbox of the issue's
holder, in the session that holds it, or of its assignee when nobody
holds it. A later pass tells them too. A failure on a gate that had
passed, from a later commit, reopens it. Evidence is at most the
server's `gate_evidence_bytes` (1,024 bytes) in all, a URL is `http` or
`https` with no user or password, and evidence that looks like a secret
is refused. `gate.fail` and every other gate command fail at once when
the server cannot be reached; nothing is queued.

## Runs that finish out of order

Each report carries its run's number, which `aries` reads from
`GITHUB_RUN_NUMBER` (GitHub Actions) or `CI_PIPELINE_IID` (GitLab CI),
or from `--run N`: it rises with each run of the workflow, and stays
the same when a run is retried, so the workflows below send it with no
change. The server takes no report of a lower run than the gate's last
one, so a slow run of an older commit that fails after a newer one
passed does not hold the work back again, and one that passes after a
newer one failed does not let it through. The one exception fails
closed: a failure of the very commit that last passed (`--sha`, whole
or abbreviated) reopens the gate whatever its run. A report that loses
so exits 0, changes nothing, and says which run it lost to. Reports
without a number are taken in the order they arrive, as before, and
`aries gate rearm` starts the order again, for a workflow whose
numbering started over. GitHub numbers each workflow's runs on their
own, so report a check from one workflow only.

## GitHub Actions

`.github/workflows/aries.yml`, beside your build. The `report` job runs
after `build` whether it passed or failed, signs in with the job's OIDC
token, and tells Aries which:

```yaml
name: aries
on:
  pull_request:
  push:
    branches: [main]
permissions:
  contents: read
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - run: make test # your build and tests

  report:
    needs: build
    if: ${{ !cancelled() }}
    runs-on: ubuntu-latest
    permissions:
      contents: read
      id-token: write # the job's OIDC token, which aries exchanges
    env:
      ARIES_DATA_DIR: ${{ runner.temp }}/aries # the sign-in, gone with the job
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - name: Sign in to aries
        run: |
          curl -fsSL https://raw.githubusercontent.com/ariesworx/aries/main/install.sh | sh -s -- --require-signature
          echo "$HOME/.local/bin" >> "$GITHUB_PATH"
          "$HOME/.local/bin/aries" login --workload github-actions
      - name: Report the build
        if: github.event_name == 'pull_request'
        env:
          RESULT: ${{ needs.build.result }}
          SHA: ${{ github.event.pull_request.head.sha }}
          RUN: ${{ github.server_url }}/${{ github.repository }}/actions/runs/${{ github.run_id }}
        run: |
          verb=fail
          if [ "$RESULT" = success ]; then verb=pass; fi
          aries gate "$verb" --check build --from-branch --optional --sha "$SHA" --url "$RUN" --reason "build: $RESULT"
      - name: Report landed
        if: github.event_name == 'push' && needs.build.result == 'success'
        env:
          RUN: ${{ github.server_url }}/${{ github.repository }}/actions/runs/${{ github.run_id }}
        run: aries gate pass --check landed --from-commit --optional --sha "$GITHUB_SHA" --url "$RUN"
      - name: Sign out of aries
        if: always()
        run: aries logout
```

`aries login --workload github-actions` makes a key for the job, asks
GitHub for the job's OIDC token for the server's audience bound to that
key, and exchanges it at the server for a sign-in as the service the
trust rule names, of scope `ci`, which every later `aries` of the job
presents. It lasts the rule's lifetime, an hour by default, and
`aries logout` ends it at the job's end, so that a matrix of jobs does
not fill the rule's `grants_per_trust_rule`. The sign-in and its key
are kept in `ARIES_DATA_DIR`, in the runner's temporary directory,
emptied after each job; `aries login --workload github-actions` refuses
to run on a runner without it, since a self-hosted runner's home
outlives the job. `id-token: write` is the report job's alone, and
nothing is stored in the repository's secrets. GitHub gives a pull
request from a fork no OIDC token, so the report fails there; never
switch to `pull_request_target`, which would run the fork's code with
the base repository's permissions. `actions/checkout` is pinned by
commit, as every action should be; update the pin with the version you
use.

**With a stored key** (a server with SSH alone, or no workload provider
for GitHub): give the job the key as the [GitLab example](#gitlab-ci)
does, written under `umask 077` by the shell's own `printf` so that it
is in no command line and no log, with only its path in `GITHUB_ENV`,
and removed in a step that always runs:

```yaml
      - name: Connect to aries
        env:
          ARIES_CI_PRIVATE_KEY: ${{ secrets.ARIES_CI_PRIVATE_KEY }}
        run: |
          umask 077
          key="$RUNNER_TEMP/aries-ci-key"
          printf '%s\n' "$ARIES_CI_PRIVATE_KEY" > "$key"
          echo "ARIES_KEY_FILE=$key" >> "$GITHUB_ENV"
```

## GitLab CI

In `.gitlab-ci.yml`, beside your build's jobs. GitLab's own ID tokens
are not a workload kind Aries 1.0 takes, so the job signs in with a key
of the principal's, best one that ends (`aries admin key add NAME FILE
--expires 90d`). Store it as a CI/CD variable named
`ARIES_CI_PRIVATE_KEY` of type *File*: GitLab writes it to a file and
puts the file's path in the variable, so the key is in no command line
and no log.

```yaml
stages: [test, report]

test:
  stage: test
  script:
    - make test # your build and tests

.aries:
  stage: report
  variables:
    ARIES_KEY_FILE: $ARIES_CI_PRIVATE_KEY # a File variable: its value is a path
  before_script:
    - curl -fsSL https://raw.githubusercontent.com/ariesworx/aries/main/install.sh | sh -s -- --require-signature
    - export PATH="$HOME/.local/bin:$PATH"

build passed:
  extends: .aries
  rules:
    - if: $CI_PIPELINE_SOURCE == "merge_request_event"
      when: on_success
  script:
    - aries gate pass --check build --from-branch --optional --sha "$CI_COMMIT_SHA" --url "$CI_PIPELINE_URL"

build failed:
  extends: .aries
  rules:
    - if: $CI_PIPELINE_SOURCE == "merge_request_event"
      when: on_failure
  script:
    - aries gate fail --check build --from-branch --optional --sha "$CI_COMMIT_SHA" --url "$CI_PIPELINE_URL" --reason "pipeline failed"

landed:
  extends: .aries
  rules:
    - if: $CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH
      when: on_success
  script:
    - aries gate pass --check landed --from-commit --optional --sha "$CI_COMMIT_SHA" --url "$CI_PIPELINE_URL"
```

The image the report jobs run in needs `curl`, `git` and CA
certificates. A variable that is *protected* reaches only pipelines of
protected branches, and merge request pipelines of other branches then
cannot report; leaving it unprotected is the cost the next section
names.

## A server CI cannot reach

A server on a laptop, or behind a VPN or an IAP a hosted runner cannot
use, never hears from the `landed` job above. There are four ways out,
in the order [design §21.8](design/aries.md#218-when-ci-cannot-reach-the-server)
recommends them for one maintainer:

1. **`aries land`, on a person's machine.** After a `git pull` of the
   default branch, it reads each commit since the last it reported and
   reports `landed` for the issue each names, with the commit as the
   evidence, from your own terminal:

   ```sh
   git switch main && git pull
   aries land
   ```

   A commit names its issue as `gate pass --check landed --from-commit`
   reads it: an `Aries: ID` trailer, else the branch a merge commit
   merged. One that names none is skipped, with a note; so is one whose
   issue neither requires `landed` under the
   [workflow](concepts.md#the-workflow) nor has a `landed` gate someone
   added. For an issue that requires it, the first report makes the
   gate, and a pass that leaves nothing else open closes the issue,
   landed. The gate is the workflow's: its `ci`
   names the principal who runs `aries land` (`ci: [ed]`), and the
   report is a person's act, so it is made from the session `cli` even
   when an agent's environment says otherwise. What a pass is worth is
   what your checkout of the default branch saw, as much as your key.

   - **Where it starts.** The last commit reported is kept per server
     and project in aries's data directory (`landed`; see [Where it is
     kept](cli.md#where-it-is-kept)). The first run, with none kept,
     reads HEAD alone, so that a long history is not reported by
     accident: `--from COMMIT` reports the commits after COMMIT. It
     reads at most 500 commits, along first parents.
   - **Running it twice changes nothing.** A report of a gate already
     passed is a no-op, so a second run, or a run on two machines, is
     harmless.
   - **Offline it reports nothing and remembers nothing.** A report is
     never queued ([When the server cannot be reached](cli.md#when-the-server-cannot-be-reached));
     run it again when the server answers, and it reads the same
     commits. A refusal, such as a principal the gate does not name,
     stops it at that commit, which the next run reads again.
   - **Only on the default branch.** On another branch it refuses, as
     what it would report is not merged. `--branch NAME` names the
     default branch when git cannot tell; `--dry-run` says what would be
     reported and keeps nothing.
   - **A `post-merge` hook.** `aries setup post-merge` prints a git hook
     that runs `aries land --hook` after each pull that merges, and
     `--write` makes it. It never writes unasked, never replaces a hook
     of someone else's (add the line to it by hand), and `--remove`
     takes out only its own. As a hook, `aries land` says nothing off the
     default branch and exits 0 whatever happens, so it never fails a
     pull.
2. **A runner inside the network, or a tunnel.** The workflow above on a
   self-hosted runner that reaches the server, or with Tailscale,
   WireGuard or an IAP tunnel (design decision 65). A pass is worth what
   it is above.
3. **A relay that polls the host's API.** A client process holding a host
   token, built outside ariesd if asked ([design §20.4](design/aries.md#204-how-ci-results-reach-the-server)).
   Deferred.
4. **No required checks.** `checks: []` in the [workflow](concepts.md#the-workflow):
   the default items stay, and ending an issue is a person's again.

`build`, which only CI runs, cannot be reported this way: leave it out of
`checks`, and keep "CI green on the PR" an item the agent ticks.

## What this costs

Plainly, as [design §20.4 and §20.13](design/aries.md#2013-what-this-does-not-solve)
put it:

- **A pass is worth what the trust rule admits.** Whoever can edit the
  workflow in a job the rule admits can pass the gate: a pull request
  runs the workflow file from its own branch, so anyone who can push a
  branch, and so change the workflow in it, gets a job that a rule
  pinning only the repository admits, which can report `build` passed.
  Narrow the rule to what the job needs: `event_name=pull_request` for
  the build report, `ref=refs/heads/main` and `event_name=push` for
  `landed`, `environment=NAME` for a protected environment whose
  reviewers GitHub enforces, or `job_workflow_ref` to a reviewed
  reusable workflow at a branch nobody pushes to unreviewed. Branch
  protection and required checks stay the host's job; a ci gate adds a
  record and a hold, not a guarantee that the build was honest.
- **Nothing is stored, and a sign-in lasts an hour.** With a trust rule,
  no key or secret lives in the CI system; a job's sign-in lasts the
  rule's lifetime, an hour by default and at most `workload:
  max_token_lifetime` (6 hours), and is bound to a key the job made and
  never leaves the runner. A token copied from a job's log is useless
  without that key, and GitHub's own token is exchanged once, within 2
  minutes of being issued, and only with the key its audience names.
  `aries admin trust disable NAME` stops a rule at once, and ends the
  sign-ins it gave, with their connections; every sign-in names its
  rule and the run it was for (`run_id`, `workflow_ref`, `actor_id`,
  `event_name`, `ref`) in the audit stream.
- **A stored key lasts until it ends or is revoked.** Where a CI system
  has no workload identity, it holds a private key in its secret store.
  Give it an end with `--expires`, and revoke it with `aries admin key
  remove FINGERPRINT` when it may have leaked; that closes its
  connections too.
- **The CI principal has a member's rights.** It can create, update,
  close and comment on issues like any member, until decision 8 gives
  narrower roles; a sign-in of scope `ci` runs no admin op and decides
  no person's gate. Give it a principal of its own, a `service` with an
  owner, never a person's, so that what it does shows under its name and
  it can be disabled alone.
- **CI needs a network path to the server:** its HTTPS URL, or on a
  server that serves SSH a public port, or IAP with `gcloud` signed in
  inside the job, which is another credential in the job.
- **The session `ci` is not a credential.** Anyone holding the key or
  the sign-in can set `CI=true`. Refusing an agent's session is a speed
  bump against an agent that signs in as CI, not a wall.
- **Evidence is text others read.** It is shown to every member and to
  agents, as data. Do not put secrets in a reason or a URL; the server
  refuses what looks like one, which is no promise that it finds every
  secret.
- **A revert does not un-land an issue.** Run `aries gate rearm GATE`
  after a revert; work already started on what the gate held goes on.

Not yet run on GitHub's or GitLab's hosted runners: the end-to-end tests
(`TestCIGate` and `TestWorkloadEndToEnd` in `internal/e2e`) run the same
`aries` commands, with the same variables, against a real daemon, the
second against a stand-in for GitHub's token endpoint and issuer.
