# `aries prs check`: every pull request accounted for

**Design, 20 October 2026 (aries-ewuqxh4h, epic aries-hzuyf4mt).**
Nothing in it is built, and nothing is built until the maintainer
answers §15. It is a permanent client command that reads the project's
open and recently merged pull requests, maps each to its issue, reports
what is orphaned or broken, files an issue for each pull request no
issue names, and posts each finding once where the maintainer sees it.
The dispatcher's sweep (aries-i6fndp4h,
[dispatcher.md §8](dispatcher.md#8-the-after-merge-sweep)) calls it and
keeps only its own acts: resume, rework and queueing merges.

## 1. Why

Asked by the maintainer on 19 October 2026: check the project's pull
requests periodically so that none is orphaned or left broken, and log
every pull request from outside the team (Dependabot's, an outside
contributor's) in Aries, so that it is worked. The same day showed what
nothing was watching, each recorded as a lesson:

| Seen on 19 October 2026 | Lesson on |
|---|---|
| Twelve merged issues (PRs 252 to 263, 267, 269) stayed open with `landed` passed: their sessions handed off before CI finished, so none ticked "CI green on the PR", and the closing pass closes only when every item is settled | aries-wf6da6vg |
| PR 265 merged while its session was pushing a revision the maintainer had asked for; two commits were stranded on the merged branch until the coordinator noticed and replayed them as PR 266 | aries-l436tgkj |
| A session fast-forwarded `main` in the main checkout itself; the post-merge hook's `aries land` reports only from the session `cli`, so nothing landed, and the pull job, finding `main` current, did nothing either | aries-gaqlkyrf |
| A session whose issue closed without a PR of its own sat idle until the maintainer noticed it; the desktop app archives a session when its PR closes or after a day idle, and Aries, which knew the issue had closed, told nobody | aries-wf6da6vg |

All four are visible from the pull requests, the branches and the
issues together, and from none of them alone. Today a coordinating
session looks when a person asks it to.

## 2. A document of its own

This is its own document rather than a part of dispatcher.md §8, for
three reasons:

- **It is permanent; the dispatcher's loop is not.** dispatcher.md §1
  makes the loop throwaway, replaced by Bearings, over parts that stay.
  This command is one of those parts, and is used without the loop.
- **It has four callers.** A person's pull job, a scheduled CI
  workflow, the coordinating session by hand, and the dispatcher's
  sweep. A section of the dispatcher's design would read as if the
  dispatcher owned it.
- **dispatcher.md is accepted** (decision 96). A design still waiting
  for answers inside an answered one blurs which is which.

dispatcher.md §8 gains a dated note pointing here.
[outcome-record.md §5](outcome-record.md#5-ci-attempts-and-their-causes)
is its neighbor: both read the CI host through one small interface,
with `gh` as its first implementation, and §5 below makes that one
interface, not two.

## 3. The command

```
aries prs check [--post] [--json] [--since DURATION] [--repo OWNER/NAME]
```

- **By default it prints and changes nothing**, as `aries setup`
  prints unless given `--write`: each finding on a line, and the issues
  it would file. `--json` prints one document (rule 13).
- **`--post`** files the issues of §7, closes those of its own whose
  pull request closed, and posts the new findings to the standing issue
  (§8). Without it, a run is safe to try anywhere.
- **`--since`** is how far back it reads merged and closed pull
  requests, `7d` by default, at most `30d` (§12).
- **Exit codes**: 0 when it ran, whatever it found; 1 when it could not
  (no `gh`, `gh` not signed in, the server away); 2 for usage. A
  finding is not a failure: a scheduled job that failed on every
  conflicted pull request would teach people to ignore its failures.
- **CLI only** (rule 12): it reads the CI host with a person's or a
  machine's credentials, and it files issues for others' pull requests.
  An agent reads its findings on the standing issue (§11).
- **No protocol change.** It is built over ops `aries` already sends:
  `list`, `show`, `history`, `comments`, `create`, `comment`, `label`
  and `close`. The one server change this design asks for (§9) is a new
  inbox kind, which needs no bump either.

### Settings

A `prs:` block in `.aries.yaml`, read by whoever runs the check, with
flags to override each:

```yaml
prs:
  repo: example/app          # default: the remote origin's
  standing: ar-a1b2c3d4      # the issue findings are posted to
  mention: alice             # the principal the findings mention
  stale_after: 3d
  landing_lag: 1h
  people:                    # the host's logins of team members
    alice-example: alice
    bob-example: bob
```

`.aries.yaml` is where a project keeps what its members share, and the
check is run by one of them or by CI from the committed copy (Q3). This
repository commits none (AGENTS.md), so its runner keeps its own.

## 4. Who runs it, and when

Decision 67 holds: `ariesd` never polls a CI host. The check runs on a
client, with that client's credentials:

| Runner | How | Credentials |
|---|---|---|
| A person's pull job (aries-yryu7pv6) | After the pull and `aries land`, on the same schedule (§10) | The person's `gh` and Aries key |
| A scheduled CI workflow | `on: schedule`, every 15 minutes or so, running `aries prs check --post` as the project's CI service principal | The job's `GITHUB_TOKEN` with `contents`, `pull-requests` and `checks` read, and a workload sign-in ([docs/ci.md](../ci.md)) |
| The dispatcher's sweep | After each move of the default branch (dispatcher.md §8), in process, without `--post` while the pull job or the workflow posts | The dispatcher's |
| The coordinating session, by hand | When a person asks, until the dispatcher runs | The person's |

**One poster per project.** A finding's first line carries its key,
and the check reads the standing issue's recent comments to know what
was posted (§8), so two posters do not post a finding twice, but they
do race within a run, and both pay for the reads. The recommendation is
one poster (the pull job for a server CI cannot reach; the workflow
otherwise) and any number of readers without `--post` (Q4).

## 5. The CI host, behind one interface

The check and `aries outcome` (outcome-record.md §5) share one
interface in a package of their own, `internal/forge`, with `gh` as its
first implementation:

| Method | `gh` | Used by |
|---|---|---|
| `Open()`: open changes with author, association, fork or not, draft, head branch and commit, base, merge state, required checks' state, labels, created and updated times | `gh pr list --state open --json number,url,title,body,author,authorAssociation,isCrossRepository,isDraft,headRefName,headRefOid,baseRefName,mergeStateStatus,statusCheckRollup,labels,createdAt,updatedAt --limit 500` | check |
| `Closed(since)`: changes merged or closed since a time, with the head commit at the close | `gh pr list --state closed --search "closed:>=DATE" --json …` | check, outcome |
| `Commits(n)`: a change's commits and messages, for `Aries:` trailers | `gh pr view N --json commits` | check, outcome |
| `Required(n)`: the state of the checks the base branch requires, on the head commit | `gh pr checks N --required --json name,state,bucket` | check |
| `BranchHead(name)`: a branch's head on the remote, if it exists | `git ls-remote origin refs/heads/NAME` | check |
| `Ahead(base, head)`: commits in head not in base | `gh api repos/O/R/compare/BASE...HEAD --jq .ahead_by` | check |
| Runs, jobs, reviews | outcome-record.md §5 | outcome |

Rules for the implementation:

- **A fake on `PATH` tests it**, as `internal/iaptest` stands in for
  `gcloud`: the test binary, re-executed as `gh`, answers from a
  fixture. No test reaches GitHub.
- **No token in Aries.** `gh` holds its own sign-in; the check passes
  nothing of it anywhere, and redacts `gh`'s stderr before an error
  quotes it, as `internal/credential` redacts a command's.
- **A state GitHub has not computed yet is no finding.** `mergeStateStatus`
  is `UNKNOWN` until GitHub has tried the merge, and checks are pending
  until they finish; the next run decides.
- **Required checks only.** A failing optional check is no finding:
  what blocks the merge is what the base branch requires, and a check
  that never blocks would teach people to ignore the report.

**What is GitHub's**, and where another host plugs in: the `gh`
commands above; the merge states (`DIRTY`, `BLOCKED`, `BEHIND`); author
associations (`OWNER`, `MEMBER`, `COLLABORATOR` against `CONTRIBUTOR`,
`FIRST_TIME_CONTRIBUTOR`, `NONE`); Dependabot's login
(`dependabot[bot]`, shown by `gh` as `app/dependabot`); a squash
commit's `(#N)`; `refs/pull/N/head`. A GitLab implementation of the
same interface, over `glab`, maps merge requests, `detailed_merge_status`,
project members and Renovate or GitLab's dependency bot to the same
fields; the findings, the filing and the posting do not change.

## 6. From a pull request to its issue

In this order, the first that names an issue wins:

1. **An `Aries:` trailer** in the pull request's commits, read with
   `gitx.IssueOfMessage`, as `aries land` and the trailer check read
   it. It wins because it is what lands the issue.
2. **The branch name**, with the project's prefix (`gitx.IDFromBranch`):
   `feature/ar-a1b2-…` is `ar-a1b2`.
3. **A recorded link**: an issue with the label `pr:N`, found with
   `list --labels pr:N` over every status. The check puts it on each
   issue it files (§7); a person may add it by hand to tie an existing
   issue to a pull request. A label, because it is indexed, listed and
   filtered by ops that exist, and needs no protocol change; the issue's
   body carries the full URL. `pr:N` assumes one repository per
   project; a project with several sets `prs.repo` per checkout and
   labels `pr:REPO#N`, within the 64 bytes a label may hold (Q2).

When two of them name different issues, that is a finding of its own
(`mismatch`), and the trailer is taken. aries-y4ibxmqm has `aries land`
use the same `pr:N` label to land an issue whose squash commit names
only `(#N)`.

## 7. Findings

Each finding has a **kind**, a **key** that says when it is the same
finding again, and whether it **needs a person** (and so mentions the
maintainer, §8). A key changes when the pull request's head changes, so
a pull request that is fixed and then breaks again is reported again.

### Open pull requests

| Kind | When | Key | Needs a person |
|---|---|---|---|
| `no-issue` | No trailer, branch or `pr:N` names an issue | `no-issue:N` | Only for an outside contributor's (§7.1); the issue is filed either way |
| `unknown-issue` | It names an issue the project does not have | `unknown:N:ID` | Yes |
| `issue-closed` | Its issue is closed, and the pull request is still open | `closed:N:ID` | Yes: close the pull request or reopen the issue |
| `mismatch` | Two of §6's ways name different issues | `mismatch:N:SHA` | Yes |
| `failing` | A required check failed on the head commit | `failing:N:SHA` | Only with no live claim on the issue |
| `conflicted` | GitHub reports `DIRTY` against the base | `conflict:N:SHA` | Only with no live claim on the issue |
| `stale` | Nothing happened on it (commit, comment, review, label) for `stale_after`, 3 days by default | `stale:N:DATE`, the date of its last activity | Yes |
| `draft-orphaned` | A draft whose issue has no live claim: a revision was asked for and nobody holds it | `draft-orphaned:N:SHA` | Yes |
| `draft-ready` | A draft whose issue's last handoff is later than its head commit: the revision is pushed and the pull request is still a draft | `draft-ready:N:SHA` | Yes: mark it ready |

**Orphaned** is the union of `no-issue`, `unknown-issue`,
`issue-closed`, `stale`, `draft-orphaned`, and `failing` or
`conflicted` with no live claim. A pull request that is green, has a
live claim or is waiting on review is not orphaned, however long it
waits: a desktop session's claim lapses minutes after the app closes
(dispatcher.md §8), so a lapsed claim alone says nothing, and only a
lapsed claim on a broken or stale pull request is reported.

**Broken** is `failing` or `conflicted`. With a live claim, the holder
is told instead of the maintainer: the check comments on the issue,
mentioning the claim's principal, so its inbox carries the finding to
the session that holds it at its next turn. The session's harness may
already have woken it (Auto-fix), which the check cannot see; the
comment is cheap and keyed like any finding.

The draft findings rely on a rule the workflow does not have yet, from
aries-l436tgkj's lesson: when a revision is asked of an open pull
request, the session marks it draft until the revision is pushed, so
the merge queue cannot take it half done. The memory `workflow` gains
that step with this command's first slice (Q7).

### Merged and closed pull requests, within `--since`

| Kind | When | Key | Needs a person |
|---|---|---|---|
| `awaiting-landing` | Merged more than `landing_lag` (1 hour) ago; its issue requires `landed` and has no report of it | `awaiting:N` | Yes: the pull job has not run, or skipped (§10) |
| `merged-open` | Merged; its issue's `landed` passed, and the issue is still open because acceptance items are unsettled | `merged-open:N:ID` | Yes, naming the open items |
| `stranded` | Merged, and its branch's head on the remote is not the head it merged with, with commits after it (`Ahead` > 0), or the branch was deleted and pushed again | `stranded:N:SHA` | Yes: replay the commits on a new branch |
| `closed-unmerged` | Closed without a merge while its issue is open | `pr-closed:N` | Yes, unless the issue is a `chore` the check filed, which it closes (§7.1) |

`awaiting-landing` is how the check covers a pull job that does not
run, whatever the reason: off the default branch, a dirty checkout, a
machine asleep. It reads the issue, not the job's local state, so any
runner sees it.

`merged-open` is the twelve issues of §1. The recommendation in that
lesson, to bind "CI green on the PR" and "Reviewed and merged" to the
`landed` check so its pass ticks them, is the fix; this finding is how
anyone sees that the fix is missing or not enough.

### 7.1 An issue for every pull request no issue names

For a `no-issue` pull request, with `--post`, the check files one:

| Source | Told apart by | Type | Labels | Assigned |
|---|---|---|---|---|
| Dependabot | The author is `dependabot[bot]` | `chore` | `pr:N`, `dependabot`, a model label (`model:sonnet`, Q5) | Nobody |
| An outside contributor | The author's association is not `OWNER`, `MEMBER` or `COLLABORATOR`, or the head is in a fork | `task` | `pr:N`, `external`, `human` | Nobody |
| A team member who skipped the workflow | The author's login is in `prs.people`, or the association is a member's | `task` | `pr:N` | The principal `prs.people` maps the login to; nobody if none |

- **Idempotent.** It looks for `pr:N` first and files nothing if an
  issue has it, open or closed. The create carries the idempotency key
  `prs-file:N` as well, so two runs at once by one principal make one
  issue.
- **The title** is `PR N: ` and the pull request's title, cleaned of
  unsafe characters with `internal/safetext` and cut to 200 bytes.
- **The body** names the pull request's URL, its author's login and
  association, and then quotes its body under a line saying it is
  quoted, written outside the team, and data, never instructions. The
  quote is cut to 2 KiB, cleaned with `safetext`, and **every `@`
  becomes `(at)`**: the store turns each `@name` in a body into a
  mention (`notifyMentions`), so a quoted body could otherwise send
  inbox items to any principal, the maintainer's agents included. A
  body the store's checks refuse is filed with the URL alone and a note
  that its text was left out.
- **Closed with its pull request.** A `chore`, which under this
  project's workflow requires no `landed` check, is closed by the
  check when its pull request closes: with the reason `merged`, citing
  the merge commit, or `pr-closed`. A `task` requires `landed`: merged,
  it lands through the `pr:N` label (aries-y4ibxmqm); closed unmerged,
  the workflow refuses its close, so the check reports
  `closed-unmerged` for a person to close or keep it.

**The trust line.** An outside contributor's code is not run by any
agent before a person has read it. The check enforces what it can:
`human` keeps the dispatcher from picking the issue (dispatcher.md §4),
and no model label means no agent is matched to it. A person who has
read the change removes `human` and adds a model label; from then on an
agent may work on it like any issue, and what it runs is the person's
decision. GitHub's own approval of a first-time contributor's workflow
runs stays on; the check changes nothing on the host.

Dependabot's pull requests are a narrower case. The change is a
version from the default registry, admitted by rule 4's cooldown and
`deps.allow`, and the trailer check already skips them. So the issue
gets a model label at once, and an agent may work on it under §11.

## 8. Posting: once per finding, on a standing issue

Findings go to one standing issue, named by `prs.standing`, as the
dispatcher's notices do (dispatcher.md §12, Q7 there), so `prime` and
the inbox carry them to any machine, and the notifications design
(aries-teizxh5h) later carries them off every machine.

- **One comment per run**, only when the run found something new. Its
  first line is `prs:` and the count by kind; then one line per new
  finding: the key in brackets, the pull request's URL, the issue, and a
  short reason. It mentions `prs.mention` once, at the end, only if a
  finding needs a person. One comment, not one per finding, because
  `notices_per_minute` (10) drops a principal's mentions past ten a
  minute, and because a person reads one list faster than ten items.
  At most 50 findings in a comment; the rest wait for the next run.
- **What was posted** is read back from the standing issue: the check
  pages its comments newest first, back to `--since` plus a day, at most
  10 pages of 100, and collects the bracketed keys. A key it finds is
  not posted again. This works across machines and principals, which an
  idempotency key alone would not: keys are unique per principal
  (`store/idem.go`).
- **The comment's idempotency key** is `prs-post:` and the first 32 hex
  digits of the SHA-256 of its sorted keys, so a retry, or the outbox's
  replay, posts it once.
- **A finding that goes away** is not posted again as resolved; the
  next run simply does not report it. A finding that comes back with a
  new key (a new head commit) is posted again.
- **On the issue itself**, a broken pull request with a live claim gets
  a comment on its own issue instead, keyed the same way and read back
  from that issue's comments.

## 9. Sessions whose issue closed

The fourth case of §1: a session idle after its issue closed, and
nobody told.

**An inbox item, `issue.closed`.** When an issue closes, by any path
(finish, close, the closing pass of `landed`), the store sends the
session that last held its claim an `issue.closed` item, with
`Session` set to that session and the close's reason as its body. The
last holder is read from the issue's claim events in the same
transaction (rule 7: the item is written with the `issue.close` event).
A session that closed the issue itself gets none. It is a new value of
an existing field, which rule 10 lets a client ignore, so it needs no
protocol bump; `prime` and the MCP inbox show it as they show any kind
they do not know, and a later client words it.

That item reaches a session only when the session takes another turn.
A desktop session idle at the end of its work does not, and a headless
run has ended. So the item is the record; the act is the list:

**The list cleanup uses.** `aries prs check --json` carries a section,
`ended`, that is not posted: each session whose issue closed within
`--since`, with the issue, the reason, the close's time, the
principal's kind, and the session id from the claim's events. Whoever
cleans up reads it: the coordinating session today, under the standing
OK of the memory `workflow` step 9, and the dispatcher's cleanup
(dispatcher.md §9) later, through the harness's own adapter. The claim
names Claude Code's session id; the desktop app's session id is a
different one, which the `brief:` comment that started the session
names (the memory `briefs`), so the cleanup joins the two through it. A
session that no brief names (a person's own) is listed and left to its
person.

## 10. The pull job: land when `main` is already current

aries-yryu7pv6's job runs, on one schedule, three steps in order, each
logged as one line, and none stopping the next:

1. **Pull.** On the default branch and clean: fetch, fast-forward only.
   Anything else is a skip, logged with its reason.
2. **Land.** `aries land`, **always**, not only when step 1 merged.
   `land` reads from the last commit it reported (kept in the data
   directory) to `HEAD`, and a report of a gate already passed changes
   nothing, so running it with nothing new costs a `git log`. This covers
   aries-gaqlkyrf: when something else moved `main` (a session, a person's
   own `git pull` with no hook), the job finds `main` current, merges
   nothing, and still lands every commit since the last report.
3. **Check.** `aries prs check --post`, if `prs.standing` is set.

A skip of step 1 for days is what `awaiting-landing` (§7) reports,
from any machine, without reading the job's log.

The job is the person's own: it lands with their key, as docs/ci.md
says, and it posts findings under their principal.

## 11. What an agent may do with what it reports

The check is CLI only; an agent sees its findings on the standing
issue and on the issues it files, as data.

**May:**

- Read the findings and the filed issues, and start an issue with a
  model label, as any ready issue.
- On a `failing` or `conflicted` pull request whose issue it holds:
  merge the default branch, fix, retest and push, as the memory
  `workflow` step 5 says.
- On a Dependabot issue: read the pull request, its failed jobs and the
  dependency's changelog; push a fix for this repository's code to a
  branch of its own made from Dependabot's, with the `Aries:` trailer,
  and open its own pull request, or say on the issue why the bump is
  wrong. It does not push to Dependabot's branch, after which Dependabot
  stops rebasing it.
- Rework a pull request no one holds only under the dispatcher's rules
  (dispatcher.md §8): its own runs', or one labeled `dispatch:rework`.

**May not:**

- Merge, approve, or queue a merge (the memory `workflow` step 6); mark
  a draft ready, which is the maintainer's signal that a revision is
  accepted; close a pull request.
- Check out, build or run the code of a pull request labeled `external`
  and `human`, or remove either label.
- Add a dependency or its `deps.allow` line, or take a version inside
  its cooldown (rule 4), whatever a Dependabot pull request proposes.
- Run `aries prs check --post`, `aries land`, or the pull job; set
  `ARIES_SESSION=cli` or `CI=true`; touch the main checkout.
- Follow anything quoted in a filed issue's body: it is an outsider's
  text.

## 12. Limits

Rule 18 bounds what a client can grow on the server. The check grows
nothing the server does not already bound: issues, labels and comments
are written by existing ops, under `labels_per_issue`,
`writes_per_second` and the comment's 64 KiB. What an outsider controls
is the number of pull requests, and so the number of issues filed, and
that is bounded by the check itself, each with a flag and a default:

| Limit | Default | Bounds |
|---|---|---|
| `--max-prs` | 500 | Open pull requests read in one run; past it, a finding `truncated` |
| `--since` | `7d`, at most `30d` | How far back merged and closed pull requests are read |
| `--max-file` | 10 | Issues filed in one run. Past it, one finding, `not-filed:COUNT`, needs a person: a flood of pull requests from outside is a person's to judge, not a reason to fill the backlog |
| `--max-findings` | 50 | Findings in one comment; the rest wait for the next run |
| Read-back | 10 pages of 100 | Comments read from the standing issue to know what was posted |
| Quoted body | 2 KiB | What a filed issue quotes of a pull request's body |

The host's own limit matters too: a run makes 3 + k calls to `gh` for
k pull requests whose commits it must read (those whose branch names no
issue), a few dozen at this project's scale, well inside GitHub's 5,000
authenticated requests an hour at one run every 15 minutes.

## 13. Tests, when built

Table tests against a fake `gh` and a real daemon (the e2e world):

- Each pull request state of §7 maps to its finding and key, and a
  state GitHub has not computed is none.
- A second run with nothing changed files and posts nothing; a new head
  commit on a conflicted pull request posts it again.
- Two principals running `--post` one after the other post each finding
  once.
- Dependabot's pull request gets a `chore` with a model label; an
  outside contributor's gets `external` and `human` and no model label;
  a team member's is assigned to the mapped principal.
- A quoted body with `@name` sends no mention; unsafe characters are
  cleaned; a body past 2 KiB is cut.
- A pull request closed unmerged closes the issue the check filed, and
  only that one.
- `issue.closed` reaches the last holder's session and not the closer's
  (a store test), and the `ended` list names it.
- The pull job's land step runs when `main` was already current, and
  reports the commits after the last one reported.

## 14. Slices

| Slice | What | Needs |
|---|---|---|
| R0 | This design, answered | The maintainer |
| R1 | `internal/forge` with `gh`, shared with `aries outcome` (outcome-record.md O1, whichever is built first makes it); `aries prs check` read only: §6 and §7, `--json` | R0 |
| R2 | `--post`: filing (§7.1), posting (§8), the `ended` list; the memory `workflow` gains the draft-for-revision step | R1 |
| R3 | The `issue.closed` inbox kind (§9), a store change | R0 |
| R4 | The pull job's three steps (§10), in aries-yryu7pv6 | R2 |
| — | `aries land` by `pr:N` (aries-y4ibxmqm) | R2 |
| — | The dispatcher's sweep calls R1 (aries-i6fndp4h) | R1 |

## 15. Questions for the maintainer

1. **A document of its own,** with a note in dispatcher.md §8 pointing
   here, for §2's reasons. *Recommended:* yes.
2. **The recorded link is a label `pr:N`** (§6), not a new field on
   the issue. *Recommended:* the label now, since it needs no protocol
   change and `list` already filters by it; a field can come with the
   outcome record's O3, which also wants the change on the issue.
3. **Settings in `.aries.yaml`'s `prs:` block** (§3), flags over them.
   *Recommended:* yes. The alternative, project settings on the server,
   would be a server change shipped in a release, and `prs.people` maps
   host logins, which are the client's business.
4. **One poster per project** (§4): the pull job here, since CI cannot
   reach this server; the scheduled workflow for a project whose server
   CI reaches. *Recommended:* yes, with the read-back of §8 making a
   second poster harmless but wasteful.
5. **Dependabot's issues: `chore`, with `model:sonnet`**, and an agent
   may fix this repository's code for the bump on a branch of its own
   (§11). *Recommended:* yes. `model:haiku` is cheaper but a failing
   bump usually needs reading an API change; `chore` has no `landed`
   check under this project's workflow, so the check closes it at the
   merge.
6. **Outside contributors: `external` and `human`, no model label**,
   until a person removes `human`. *Recommended:* yes; there is no
   automatic triage of a fork's code.
7. **Draft for revision.** When the maintainer asks for a revision of
   an open pull request, the session marks it draft until the revision
   is pushed, and the check reports a draft whose revision is pushed
   (`draft-ready`) or that nobody holds (`draft-orphaned`).
   *Recommended:* yes, the step in the memory `workflow` with R2. A hold
   label the merge queue respects would need a required check of its
   own; draft is what the queue already refuses.
8. **Who is told about a broken pull request with a live claim:** its
   session, through a comment on its issue, not the maintainer.
   *Recommended:* yes; the maintainer hears of it only once it is
   orphaned.
9. **`issue.closed` to the last holder's session** (§9), as a store
   change in R3, needing no protocol bump. *Recommended:* yes, and the
   `ended` list is what cleanup acts on; the item alone wakes nothing.
10. **The pull job always lands** (§10), whether or not it merged.
    *Recommended:* yes; it costs a `git log` when there is nothing new.
11. **`stale_after` 3 days, `landing_lag` 1 hour, `--max-file` 10.**
    *Recommended:* yes, as defaults to revisit after a month of runs.
12. **The exit code is 0 whatever it finds** (§3). *Recommended:* yes;
    a scheduled job that fails on findings gets ignored. `--strict`,
    exiting 1 on a finding that needs a person, can be added for a
    workflow that wants a red run.
13. **Permanent, not experimental** (unlike `aries dispatch`): it is in
    the v1.0.0 surface. *Recommended:* mark it experimental in its help
    until it has run a month across two runners, then freeze it; the
    findings' keys and `--json`'s shape are what others will read.
