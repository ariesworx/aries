# The outcome record: what each run of an issue led to

**Design, 19 October 2026 (aries-4x4vo7cq, L0 of the learning loop,
epic aries-dmqqolg4).** Proposed; nothing in it is built, and nothing is
built until the maintainer answers §14.

The learning loop's primary signal is not what an agent says about its
run. It is what the run led to, measured after the outcome is known:
whether the change landed, how many CI attempts it took and why each
failed, whether it conflicted with the default branch, whether it was
reopened, what was waived, how long it was held and waited, and what it
cost before and after it was first thought done. The maintainer adopted
that order on 19 Oct 2026, from a critical review of the loop: "per-run
record primary, lessons as annotation." A lesson
([lessons.md](lessons.md)) is read beside this record. L0b
(aries-4fe6kw3o) builds a deterministic weekly report over it, and L3
(aries-g34cdfb3) annotates that report. [lessons.md §12](lessons.md#12-evaluation)'s
pre-registered rules are judged on the record's fields.

This is a document of its own rather than a section of
[lessons.md](lessons.md), for three reasons. Lessons are the annotation
and this is the signal, and a section inside the annotation's design
would invert that. Its readers are wider than lessons': L0b's report,
L3, the dispatcher's sweep ([dispatcher.md §8](dispatcher.md#8-the-after-merge-sweep))
and the productivity metrics' M36
([productivity-metrics.md §4.7](productivity-metrics.md#47-briefs-and-their-outcomes))
all read it. And its first form needs no protocol change, while
lessons' field takes protocol 10, so the two ship on separate clocks.
[lessons.md §2](lessons.md#2-the-outcome-record-first-the-lesson-beside-it)
points here.

## 1. What a record is about

**An issue's outcome, with the runs that led to it.**

- A **run** is one session's work on an issue: its holds of the issue's
  claim, from each `claim.take` to the hold's end (`claim.release`,
  `claim.expire`, `claim.lapsed`, `claim.away`, or a take by another
  session), as `store/attribution.go` already reads them. A session is
  the claim's principal and session id. An issue may have several runs:
  a first session, a woken one for a conflict, a dispatcher's rework run.
- An **outcome** is one of:

  | Outcome | When | Read from |
  |---|---|---|
  | `landed` | The issue is closed with the reason `landed`: the closing pass of the check `landed`, after the merge | `issue.close` |
  | `closed` | The issue is closed for any other reason: by hand, forced, a duplicate, dropped | `issue.close` |
  | `pr-closed` | The issue's pull request was closed without a merge while the issue stays open | The CI host (§5) |

  `pr-closed` is the abandoned run whose issue lives on: the work is
  redone, in another pull request, and its next outcome gets a record
  of its own. An issue open, unclaimed and handed off `partial` or
  `blocked` has no outcome yet. It is waiting, not abandoned, and gets
  no record until one of the three happens (Q3).

**A record per outcome, not per issue.** An issue that lands, is
reopened by a revert and lands again has two records, `seq` 1 and 2.
Each covers the issue's whole life up to its outcome, so the latest
record stands alone; a reader takes the latest `seq` of each issue and
never adds records (Q2). Records are append-only: a capture that was
wrong is corrected by a new record with the same `seq` and `supersedes`
set, never by an edit.

**Per session, never per person** (decision 97). A record names sessions
and each session's principal **kind** (agent, person, service), never a
principal, and names no reviewer. The comment that carries it (§8) has an
author, the capturing session's principal, as every comment does; the
record's fields do not depend on it.

## 2. The fields

Each field's source, and whether the record **records** it (the value
lives outside Aries, or is gone later) or **derives** it (it can be read
from the store again at any time; §9 says why it is still written down
in the first form).

| Field | Holds | Source | Recorded or derived |
|---|---|---|---|
| `issue`, `type`, `epic` | The issue, its type and its epic | `show` | derived |
| `seq`, `outcome`, `supersedes` | §1 | `history`: the count of `issue.close` events and `pr-closed` records before this one | derived |
| `rev` | The issue's rev the capture read | `show` | recorded (it pins what was read) |
| `captured_at`, `capturer` | When it was captured, and by what: `coordinator`, `aries outcome`, `dispatcher` | The capture | recorded |
| `change` | The pull request: host, number, URL, base branch | The CI host, by the issue's branch (`gitx`) or `Aries:` trailer | recorded |
| `brief` | `sha256` of the brief as the harness received it, and where it was read | The `brief:` comment (dispatcher.md §7), then aries-2qx4mlm2's record | recorded until aries-2qx4mlm2; derived after |
| `runs[]` | Per run: `session`, principal `kind`, `harness`, `models`, `effort`, `first_take`, `held_s`, handoff states in order | `history` (claims, handoffs); models from `show`'s usage; harness and effort from the brief's record | derived, except harness and effort until aries-2qx4mlm2 |
| `times` | `created`, `first_start`, `change_opened`, `first_done` (first handoff with state `done`), `merged`, `outcome_at` | `history`; `change_opened` and `merged` from the CI host | derived; the host's times recorded |
| `held_s`, `cycle_s` | Time under claims; time from creation to the outcome | `history`, as `aries cost --group-by outcome` computes them | derived |
| `ci` | Attempts and failures with their causes (§5) | The CI host, through `gh` first | recorded |
| `default_branch` | Merges of the default branch into the change's branch, and how many conflicted (§6) | The CI host's commits and `git` | recorded |
| `review` | Reviews requesting changes, and review comments: counts only | The CI host | recorded |
| `reopens[]` | Each reopen's time and reason (`check failed` from a revert, or the text given) | `issue.reopen` | derived |
| `waived` | Acceptance items waived (their numbers) and gates waived (their names); `forced` when a close set `acceptance_overridden` or `checks_overridden` | `acceptance.waive`, `gate.waive`, `issue.close` | derived |
| `tokens` | The issue's tokens by kind (`input`, `output`, `cache_write`, `cache_write_1h`, `cache_read`) and by model, with `split` and `capped` | `show`'s usage, or `aries cost --group-by issue` over the issue's life | derived |
| `rework` | `from` and its `trigger`, `pre_merge` tokens by kind, `post_close` tokens by kind, and `attributed` (§4) | `aries cost --group-by issue` over two windows; `history`; the CI host | derived, except the boundary's CI time |
| `lessons` | The number of `lesson:` comments (and `lesson: nothing to report`) on the issue | `comments` | derived |
| `missing[]` | The fields the capture could not read, each with a one-word reason | The capture | recorded |

What is **not** in it, and why:

- **A CI job's log or an error line.** It is text, may hold a secret,
  and the host keeps it for a limited time. The record carries the run's
  id as `ci:ID` (lessons.md §3's evidence type), which a person or L3
  follows.
- **Tokens per run.** No read returns a session's tokens on one issue:
  `aries cost` groups by account, issue, epic, person, model and
  outcome, and grouping by person would name the person. Until a read by
  session exists (Q8), tokens are the issue's, and `runs[]` carries each
  run's held time and models, which are enough to see which run was the
  costly one in most cases.
- **Money.** Prices are applied on read and change the past
  (productivity-metrics.md §2), so the record holds tokens and L0b
  prices them.
- **A person's hours** (`aries hours`): they are logged by people, not
  measured; L0b reads them from `aries cost` beside the record if it
  wants them.

## 3. Times, held and cycle

`created` is the issue's `issue.create`; `first_start` its first
`claim.take`; `first_done` the first `issue.handoff` with state `done`;
`merged` the host's merge time; `outcome_at` the close's time (for
`landed`, the closing pass, which runs when the maintainer's pull job
runs `aries land`), or the host's close time for `pr-closed`.

From them L0b derives, and the record does not store, the durations that
matter to the factory:

| Duration | From, to | Says |
|---|---|---|
| Queue | `created` → `first_start` | How long ready work waited for a slot (M8) |
| Work | `first_start` → `first_done` | How long until the first session thought it done |
| Awaiting merge | `first_done` → `merged` | CI, rework and the maintainer's review (M9) |
| Awaiting landing | `merged` → `outcome_at` | The pull job's lag; it should be minutes |
| Cycle | `created` → `outcome_at` | As `aries cost --group-by outcome` defines it |

`held_s` is the time under claims, summed over every run, as the cost
report computes it. Held over cycle is the cost report's flow ratio;
held is not working (productivity-metrics.md §5, caution 4), since a
desktop session waiting on Auto-fix holds its claim while CI runs.

## 4. Pre-merge rework, defined

Today's rework, in `aries cost --group-by outcome`, is the tokens an
issue took **after its first close**. Under the workflow setting a close
means landed, so that counts only work after a merge: a revert's reopen
and what followed. It misses the rework the workflow actually has, which
happens between "done" and the merge: CI failures, conflicts with the
default branch, review changes.

**Definition.** For an issue's record:

- **The rework boundary** `rework.from` is the earliest of:
  1. the first `issue.handoff` with state `done` (`trigger: done`);
  2. the completion of the first failed CI attempt on the change
     (`trigger: ci`), whatever its cause (§5).

  A run that pushes early and fails CI before it hands off has started
  rework at the failure, not at its handoff. An issue with neither (it
  was closed without a change, or handed off only `partial`) has no
  boundary, and its pre-merge rework is 0 by definition, not missing.
- **Pre-merge rework** `rework.pre_merge` is the tokens attributed to
  the issue from `rework.from` to `merged` (for `pr-closed`, to the
  host's close; for `closed` with no merge, to `outcome_at`).
- **Post-close rework** `rework.post_close` is today's figure, the
  tokens after the first close, kept under its own name so the two are
  never added by mistake. For a first landed record it is 0.
- **The rework share** is `pre_merge` over the issue's tokens up to
  `merged`. L0b reports it; the record does not store a ratio.

**Computed by** two reads of `aries cost --group-by issue` (the op
`cost` with `by: issue`, no protocol change): one over
`[created, outcome_at]` for the issue's tokens, one over
`[rework.from, merged]` for its pre-merge rework, each taking the
issue's group from the result with `-n` at its maximum (500). A record
that straddles the boundary (a turn or session record spanning
`rework.from`) is divided by time as attribution divides one over a
hold (`store/attribution.go`), so the two windows' parts add up.
If the issue was summed into "the rest" or the read was `capped`, the
field is listed in `missing[]` rather than given as a lower bound.

**The attribution gap.** Usage reaches an issue only while a session
holds it. Rework done by a session that does not hold the claim (a
woken session whose claim lapsed and that did not `start` again, or a
coordinator fixing a PR from its own session) falls to
`(unattributed)`, and pre-merge rework is undercounted. The capture
checks it: a commit on the change after `rework.from` whose time falls
outside every hold of the issue sets `rework.attributed` to `partial`
(otherwise `full`). A `partial` record is complete (the capture read
everything) but L0b reports the share of `partial` records beside the
rework figure, so a gap in the workflow shows up as a number rather than
as cheap rework. The fix is the workflow's, not the record's: a session
that works on an issue holds it.

**Tested** (when built) against a fixture of an issue with a
handoff, a CI failure before it, a conflict after it, usage records at
request and turn granularity on both sides of each boundary, a reopen,
and a commit outside every hold: the boundary, both rework figures and
`attributed` are held to hand-computed values.

## 5. CI attempts and their causes

**The shape is not GitHub's.** A record holds:

```json
"ci": {
  "host": "github",
  "attempts": 3,
  "first_pass": false,
  "failures": [
    {"run": "ci:900000001", "attempt": 1, "sha": "1a2b3c4", "event": "change",
     "jobs": ["test"], "cause": "flake", "at": "2026-10-19T14:02:00Z"}
  ],
  "queue_attempts": 1
}
```

- An **attempt** is one run of the workflows the default branch
  requires, on one commit of the change, including each rerun of the
  same commit. `attempts` counts them up to the merge; `queue_attempts`
  counts the merge queue's (or the host's equivalent pre-merge build) on
  its own, since a queue failure is not the change's commit.
- `event` is `change` (a run on the change's own commit) or `queue`.
- `jobs` names the failed jobs, from the workflow's own definition (a
  fixed, small set per project; not free text an agent wrote).
- `first_pass` is true when the first attempt on the change passed and
  nothing after it failed.

**Causes** come from a fixed list, decided by rule from what the host
reports, never by a model reading logs:

| Cause | Rule |
|---|---|
| `flake` | The same commit later passed on a rerun with no new commit between |
| `change` | A later commit on the change passed; the change's own fault, as far as a rule can tell |
| `base` | The failure was on a commit that only merged the default branch (§6), and the commit before it had passed |
| `queue` | A merge-queue run failed while the change's own last run passed |
| `infra` | The host reported the run cancelled, timed out or not started |
| `unknown` | None of the above: no later pass (an abandoned change), or two rules apply |

Rules are applied in that order and the first that holds wins. A cause
is a fact about the sequence of runs, not about what broke: a `change`
failure may still have been a flake that a new commit happened to dodge.
L0b reports causes as counts; L3 reads the runs (`ci:ID`) for the why,
and lessons say what the session thought.

**Through `gh` first.** The capture asks the host behind one small
interface: the change for a branch, its commits with their parents, its
reviews, and its CI runs with attempts, conclusions and failed jobs. The
first implementation is `gh` (`gh pr view --json`, `gh api` for a pull
request's commits, `gh run list --branch` and `--event merge_group`,
`gh run view --json jobs`), as the dispatcher's sweep uses it. Another
forge or CI system is another implementation of the same interface; the
record does not change.

**Decision 67 holds.** `ariesd` never polls a CI host and receives no
webhook. Every value in `ci`, `default_branch`, `review` and `change` is
gathered by a client with the person's or the machine's own `gh`
credentials, and sent to the server as an ordinary comment (§8). The
server never holds a host's token for this.

**Retention.** A host keeps runs and their logs for a limited time
(90 days by default on GitHub, configurable by the repository). The
record keeps only ids, conclusions and job names, read within days of
the outcome, so retention bounds only a backfill (Q9).

## 6. Merges of the default branch, and conflicts

The workflow merges `origin/main` into a pushed branch and never rebases
it (the memory `workflow`, step 5). So the change's history shows each
catch-up as a merge commit:

- `default_branch.merges` counts the change's commits with two parents
  whose second parent is on the default branch.
- `default_branch.conflicted` counts those whose parents did not merge
  cleanly: `git merge-tree --write-tree P1 P2` (git 2.38 or later) run
  on each, which reports conflicts without touching a worktree. The
  commits come from the host's ref for the change (`refs/pull/N/head` on
  GitHub), which outlives the branch's deletion.
- `default_branch.sweeps` counts the sweep's `rework:` comments on the
  issue (dispatcher.md §8), when there are any: a conflict the sweep saw
  and sent to a session.

A rebased or force-pushed change loses its catch-ups, and the capture
cannot tell; it records `default_branch` as missing with the reason
`rewritten` when the host's timeline shows a force push. That makes the
workflow's "never rebase a pushed branch" visible in the capture
measure.

## 7. Who captures it, and when

**Now: the coordinator's sweep**, the memory `workflow`'s step 8. After
each merge the coordinator already lists open PRs and names merged
issues not yet closed. It adds one step: for each issue that reached an
outcome (§1) since its last sweep and has no `outcome:` comment with
that `seq`, capture the record and post it. The outcome is known by
then: `landed` is written by the closing pass when the pull job runs
`aries land`, a few minutes after the merge, and the sweep that follows
the next merge, or the next fallback, finds it. Issues are found with
`list` (closed since the last sweep), the sweep's own `gh pr list
--state closed` (for `pr-closed`), and `comments` (for an existing
record).

**How the coordinator captures it is the first decision (Q5).** By hand,
from `show`, `history`, `comments`, two `cost` reads and five or more
`gh` and `git` calls, a model session spends tokens on arithmetic it can
get wrong, and two coordinators would round differently, which defeats a
record meant to be objective. The recommendation is a small client
command, `aries outcome ID [--post]`, that does exactly §2 to §6 and
prints the record (`--json`) or posts it. It is CLI only, never an MCP
tool (rule 12's spirit: it reads a CI host with a person's credentials),
experimental like `aries dispatch`, and built over existing ops, so it
needs no protocol change. It is one of the dispatcher's permanent parts
(dispatcher.md §1) rather than a part of its loop.

**Later: the dispatcher's sweep** (dispatcher.md §8) runs the same
command after its cleanup step, for every issue that reached an outcome,
its own runs' and others'. Its runs know their harness, model and effort
and post their `brief:` comment, so its records have no `missing`
harness or brief.

**Not the closing pass**, though it runs at the right moment. It is
`ariesd`'s, and by decision 67 it cannot read the host, so it could
record only the store's half, which is derivable anyway. Nor `aries
land`, which also runs at the right moment, on the maintainer's machine
with `git` and `gh`: it runs in a post-merge hook on every pull, a slow
or failing CI host must never hold up a pull, and it never sees
`closed` or `pr-closed` outcomes. If `aries land` ever captures, it does
so as a separate step the hook starts in the background, not inline.

**With L2: a field** (protocol 10, or the release after). An op
`outcome` that takes only the recorded half (§9) and stores it as a row
with an event `outcome.record` (rule 7); `show`, `history` and the
lessons export derive the rest on read. The comment form then stops, and
the comments already posted are imported by a person-run command, or
read as they are by L0b for the weeks they cover (Q6).

## 8. The first form: an `outcome:` comment

One comment per record, on the issue. The first line is for people, the
second for L0b; nothing else is in the comment.

```text
outcome: v1 landed seq 1, 3 CI attempts (1 flake), 1 conflict, rework 18% pre-merge
{"v":1,"issue":"aries-0a1b2c3d","seq":1,"outcome":"landed","rev":14,"captured_at":"2026-10-20T09:12:00Z","capturer":"aries outcome","change":{"host":"github","id":"271","url":"https://example.com/acme/widgets/pull/271","base":"main"},"brief":{"sha256":"5f2c…","from":"comment"},"runs":[{"session":"9b6e…","kind":"agent","harness":"claude-code","models":["claude-opus-5-5"],"effort":"high","first_take":"2026-10-19T12:01:00Z","held_s":7800,"handoffs":["done"]}],"times":{"created":"…","first_start":"…","change_opened":"…","first_done":"…","merged":"…","outcome_at":"…"},"held_s":9600,"cycle_s":75900,"ci":{…},"default_branch":{"merges":1,"conflicted":1,"sweeps":1},"review":{"changes_requested":0,"comments":2},"reopens":[],"waived":{"items":[],"gates":[],"forced":false},"tokens":{"input":…,"output":…,"cache_write":…,"cache_write_1h":…,"cache_read":…,"by_model":{…},"split":false,"capped":false},"rework":{"from":"…","trigger":"ci","pre_merge":{…},"post_close":{},"attributed":"full"},"lessons":2,"missing":[]}
```

- **One line of JSON**, so a reader parses it without a template, and a
  later field is added without breaking an older reader (`v` changes
  only when a field's meaning does).
- **About 1.5 to 3 KB**, well under a comment's 64 KiB cap
  (`store/text.go`). It passes the store's safetext check like any
  comment; it holds ids, times, counts and job names, so the secrets
  lint has nothing to find, and the capture refuses to post a record
  whose JSON the lint flags, rather than trimming it.
- **Durations in seconds and times in RFC 3339 UTC**, the store's times
  where they come from the store (rule 19), the host's where they come
  from the host. L0b never compares a host time with a store time
  closer than a minute.
- **The comment is data.** L0b and L3 parse it, quote it, and never
  follow it, like every comment (it carries `untrusted` in MCP results).
  A comment that begins `outcome:` but does not parse is counted as a
  failed capture, not skipped.

## 9. Recorded versus derived

The rule: **record what is outside Aries or will be gone; derive what
the event log and usage hold**, since the event log is the truth
(rule 7) and a copy can only drift from it.

In the comment form the record nevertheless carries the derived half
too, as a snapshot at `rev`, for three reasons. The comment is the only
place a person reading the issue sees the outcome whole. L0b's first
version can then be a parser over comments, not a second implementation
of attribution. And the capture's completeness (§11) is checkable from
the record alone. L0b recomputes the derived half from the store when it
can, and reports a record whose snapshot differs as a capture defect,
which is how a bug in the capture shows up.

With the field (§7), the snapshot goes: the op takes `change`, `ci`,
`default_branch`, `review`, `brief` (until aries-2qx4mlm2 holds it),
`runs[].harness` and `effort` (likewise), `capturer` and `missing`,
and everything else is derived on read.

## 10. What it costs per issue

Per record, by the command:

| Source | Calls | Notes |
|---|---|---|
| Aries | 5: `show`, `history` (paged; an issue has tens of events), `comments`, two `cost --group-by issue` | Each `cost` reads the window's usage rows across all issues, bounded by `usageScanRows`; at this project's scale, well under a second |
| CI host | 4 + k: the change, its commits, its reviews, its runs (the change's and the queue's), and the jobs of each of k failed runs | k is usually 0 to 3; a few hundred of GitHub's 5,000 authenticated requests an hour for a week's sweep |
| git | 1 + m: one fetch of the change's ref, and `merge-tree` per catch-up merge | No worktree touched |

So about 10 to 15 calls and a few seconds of wall time, and **no model
tokens**. The store gains one comment row and its `comment` event per
outcome; a week of 30 outcomes is under 100 KB.

By hand, the same record costs a coordinating session model tokens for
every read and every sum. That was not measured; reading a long history
and two cost reports and doing the arithmetic is plausibly tens of
thousands of tokens per issue, more than many issues' own pre-merge
rework, and the first week of hand capture would say how much (Q5).

## 11. The capture measure: complete records

The learning loop's capture measure is **the share of outcomes with a
complete record**, not the share of issues with a lesson (the epic's
notes, 19 Oct 2026). For a week:

- **Coverage** = outcomes with a record ÷ outcomes in the week, the
  outcomes counted from the store (`issue.close` events) and the host
  (changes closed unmerged), not from the records.
- **Completeness** = records with `missing` empty ÷ outcomes in the
  week.

A field counts as complete when its source was read, so a 0 or an empty
list from a source that answered is complete, and a field the source
could not answer (`gh` not signed in, a rewritten history, a `capped`
read, an issue summed past `-n`, a brief never posted) is in `missing`
with its reason. `rework.attributed: partial` is complete (§4).

L0b reports both every week, with the reasons in `missing` counted, so
a capture that fails for one reason every time (a brief never posted
for desktop sessions, say) is visible as one line rather than as a lower
rate. Rates in L0b's report are computed over complete records only,
each with its n (productivity-metrics.md §5, caution 9).

## 12. What a record does not prove

- **It is confounded by difficulty.** The model, the effort and the
  brief were chosen for the issue, so a strong model given hard issues
  looks costly (productivity-metrics.md M36's caveat). The record is
  evidence for a pre-registered comparison, not a ranking.
- **Causes are rules, not diagnoses** (§5).
- **Rework is tokens, not effort.** A rework run that waits on CI costs
  held time and few tokens.
- **One record is one issue**, and issues differ in size; L0b reads
  them by type, against the project's own past.

## 13. Slices

| Slice | What | Done when |
|---|---|---|
| O0 | This design, answered | The maintainer has answered §14 |
| O1 | `aries outcome ID [--json] [--post]`, CLI only and experimental, over existing ops; the CI host interface with `gh` as its first implementation; §4's rework, §5's causes and §6's merges, each tested against fixtures (a fake `gh` on `PATH`, as `internal/iaptest` fakes `gcloud`, and a real git repository); the coordinator's sweep step in the memory `workflow`; `brief:` comments from the coordinator (Q7) | A week's landed issues each have a record, the share complete is reported on the epic, and pre-merge rework is held to hand-computed values by a test |
| O2 | The dispatcher's sweep runs O1's command (dispatcher.md §8) | Its runs' records have no harness or brief missing |
| O3 | The field: the `outcome` op and table, the event, the derived half on read, the export's join (lessons.md §6); a read of tokens by session (Q8) | Protocol 10 or the release after; designed when O1 has run a month |

O1 is the only slice the learning loop needs before L0b. It changes no
protocol and no store, so it can ship before v0.6.0 or after it without
touching the release.

## 14. Questions for the maintainer

1. **A document of its own.** This design is
   `docs/design/outcome-record.md`, with a row in the design README, and
   lessons.md §2 points to it, for the reasons in the introduction.
   *Recommended:* yes.
2. **One record per outcome, each covering the issue's life to date.**
   A reopened and relanded issue has two records; readers take the
   latest `seq`; a correction supersedes, never edits (§1).
   *Recommended:* yes. The alternative, a record per run, has no
   outcome to attach to a run that only handed off `partial`.
3. **Outcomes: `landed`, `closed` and `pr-closed`.** An issue waiting
   with a `partial` or `blocked` handoff is not abandoned and gets no
   record until it reaches one (§1). *Recommended:* yes. A time-based
   "abandoned" (unclaimed for N days) would record a run as failed while
   the maintainer is simply away.
4. **Pre-merge rework's boundary** is the earlier of the first `done`
   handoff and the first failed CI attempt, whatever its cause, and runs
   to the merge; today's figure is kept as `post_close` (§4).
   *Recommended:* yes, counting `flake` and `infra` failures too, since
   the tokens were spent either way; L0b can split rework by the
   boundary's trigger and the failures' causes.
5. **Capture by a command, `aries outcome`, from O1's first week**,
   CLI only and experimental, run by the coordinator's sweep (§7), rather
   than the coordinator reading and summing by hand. *Recommended:* the
   command. By hand costs model tokens per issue and gives records two
   sessions would compute differently, which is the self-report the
   record exists to replace. If the maintainer wants data before the
   command exists, hand capture for at most one week, marked
   `"capturer":"coordinator"`, and L0b reports those apart.
6. **The comment carries the derived half as a snapshot** until the
   field exists, and L0b recomputes it and reports differences (§9).
   *Recommended:* yes. With the field, only the recorded half is sent.
7. **Briefs now.** Until aries-2qx4mlm2 records briefs, the coordinator
   posts a `brief:` comment with the brief's full text on the issue when
   it starts a session, as the dispatcher will (dispatcher.md §7), and
   the record hashes it. *Recommended:* yes, with the brief's first line
   naming the harness, model and effort, so the record's `runs[]` has
   them. Without it, every record of a desktop session lists `brief`
   and `harness` as missing, and the capture measure says so.
8. **Tokens per run.** A read of an issue's tokens by session needs a
   new `cost` grouping, a protocol change. *Recommended:* defer it to
   O3, with the field; until then tokens are the issue's and `runs[]`
   gives each run's held time and models (§2).
9. **Backfill.** Capture records for the issues that landed in the 30
   days before O1, marked `"backfill":true`, so L0b's first report has
   past weeks to compare against. *Recommended:* yes, within the host's
   retention. Their brief and harness are missing and count against
   completeness honestly; L0b's first weeks report coverage and
   completeness for backfilled and live records apart.
10. **When L0b's rates speak.** *Recommended:* L0b always reports
    coverage and completeness, computes rates over complete records
    only with their n, and marks a week below 80% complete as not
    comparable with the weeks before it. The threshold is a guess to
    revisit after a month.
11. **Review counts from the host.** The issue's list does not name
    them; `review` adds the number of reviews requesting changes and of
    review comments, never who wrote them (§2). *Recommended:* yes. They
    are a cause of pre-merge rework that neither CI nor conflicts
    explain, and they cost nothing beyond the change's read.
