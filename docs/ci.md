# CI gates: checks and landing from CI

A [ci gate](concepts.md#ci-gates) holds work back until CI reports a
check, such as `build` or `test`, passed on the issue the work depends
on. It is also how Aries tells *closed* from *landed*: an agent closes
an issue when it thinks it is done, and the check `landed` is reported by
the job on the default branch after the merge.

CI reports the way any client does: the job runs `aries` as a principal of
its own. ariesd polls nothing, receives no webhook, and calls no CI
host ([design §20.4](design/aries.md#204-how-ci-results-reach-the-server)).
Read [What this costs](#what-this-costs) before you set it up.

## Set it up

1. **A principal for CI, with its own key.** Make the key on your own
   machine, add its public half under a principal of its own, such as
   `ci-example`, and keep the private half only in the CI system's secret
   store, as the [team deployment](deploy/team.md) shows (steps 4 and 6):

   ```sh
   ssh-keygen -t ed25519 -N '' -C ci-example -f ci-example
   aries admin principal add ci-example --key ci-example.pub   # a server of many projects
   ```

   On a server of one project, add a line to `authorized_keys` instead
   ([Principals](server.md#principals)). Store the contents of
   `ci-example` as the secret `ARIES_CI_PRIVATE_KEY`, then delete both
   files. Never name the secret `ARIES_KEY_FILE`, which `aries` reads as
   a path.

2. **A path from CI to sshd.** The job connects as `aries` does from a
   laptop, with the repository's committed `.aries.yaml`: to a public
   address, or through [Google Cloud IAP](cli.md#servers-without-a-public-ip-google-cloud-iap)
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

## GitHub Actions

`.github/workflows/aries.yml`, beside your build. The `report` job runs
after `build` whether it passed or failed, and tells Aries which:

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
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - name: Connect to aries
        env:
          ARIES_CI_PRIVATE_KEY: ${{ secrets.ARIES_CI_PRIVATE_KEY }}
        run: |
          curl -fsSL https://raw.githubusercontent.com/ariesworx/aries/main/install.sh | sh -s -- --require-signature
          echo "$HOME/.local/bin" >> "$GITHUB_PATH"
          umask 077
          key="$RUNNER_TEMP/aries-ci-key"
          printf '%s\n' "$ARIES_CI_PRIVATE_KEY" > "$key"
          echo "ARIES_KEY_FILE=$key" >> "$GITHUB_ENV"
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
      - name: Remove the aries key
        if: always()
        run: rm -f "$ARIES_KEY_FILE"
```

The key reaches a file the way the [team deployment](deploy/team.md)
describes: written under `umask 077` by the shell's own `printf`, so it
is in no command line and no log, with only its path in `GITHUB_ENV`,
and removed at the end. `actions/checkout` is pinned by commit, as every
action should be; update the pin with the version you use. GitHub gives
no secrets to a pull request from a fork, so the report fails there;
never switch to `pull_request_target` to get them, which would hand the
key to code from the fork.

## GitLab CI

In `.gitlab-ci.yml`, beside your build's jobs. Store the key as a CI/CD
variable named `ARIES_CI_PRIVATE_KEY` of type *File*: GitLab writes it
to a file and puts the file's path in the variable, so the key is in no
command line and no log.

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

## What this costs

Plainly, as [design §20.4 and §20.13](design/aries.md#2013-what-this-does-not-solve)
put it:

- **A pass is worth what the CI principal's key is worth.** Whoever can
  edit the workflow can pass the gate: a pull request runs the workflow
  file from its own branch, so anyone who can push a branch, and so
  change the workflow in it, can make the job report `build` passed, or
  read the key. Branch protection and required checks stay the host's
  job; a ci gate adds a record and a hold, not a guarantee that the
  build was honest.
- **The key is stored, and lasts until revoked.** Until workload sign-in
  is built (decisions 52 and 56, [access research §7](design/access-research.md)),
  CI holds a long-lived private key in its secret store, not a
  credential that lasts the job. Revoke it with `aries admin key remove
  FINGERPRINT` when it may have leaked; that closes its connections too.
- **The CI principal has a member's rights.** It can create, update,
  close and comment on issues like any member, until decision 8 gives
  narrower roles. Give it a principal of its own, never a person's key,
  so that what it does shows under its name and it can be revoked alone.
- **CI needs a network path to sshd:** a public port, or IAP with
  `gcloud` signed in inside the job, which is another credential in the
  job.
- **The session `ci` is not a credential.** Anyone holding the key can
  set `CI=true`. Refusing an agent's session is a speed bump against an
  agent that signs in with the CI key, not a wall.
- **Evidence is text others read.** It is shown to every member and to
  agents, as data. Do not put secrets in a reason or a URL; the server
  refuses what looks like one, which is no promise that it finds every
  secret.
- **A revert does not un-land an issue.** Run `aries gate rearm GATE`
  after a revert; work already started on what the gate held goes on.

Not yet run on GitHub's or GitLab's hosted runners: the end-to-end test
(`TestCIGate` in `internal/e2e`) runs the same `aries` commands, with the
same variables, against a real daemon.
