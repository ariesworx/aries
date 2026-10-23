# The merge rule as a setting

**Design, 22 October 2026 (aries-zxxoccdq); decided the same day**
(§10: the maintainer accepted all nine recommendations). M1 is built
(§9, As built); nothing else in it is. It compares three places the rule could
live, chooses the server setting, and amends Bearings' decision 17 to
follow the rule ([bearings.md §7](bearings.md#7-decisions)).

## 1. Why

The maintainer, answering
[factory-review.md](factory-review.md#6-questions-for-the-maintainer)
question 11 on 20 October 2026: "Bearings should follow the merge rule
until otherwise directed. I want to explore whether we can make the
merges a configurable setting in ariesd and/or aries."

Today the rule is prose in two project memories, applied by a model's
judgment:

- **The memory `dispatch`, rule 4** (17 October 2026, from
  aries-ssgn3lve): a pull request whose required checks are green on
  its head commit and whose every changed path is a guide under `docs/`
  or a test, with the exceptions it lists.
- **The memory `merges`** (20 October 2026): a wider rule. A pull
  request may be queued when its checks are green, its issue sits under
  an epic the maintainer approved, it touches none of a list of
  sensitive areas, a session the maintainer started opened it, and no
  hold or freeze is on. "Releases, tags, signing and the release notes
  always" wait for the maintainer.

The memory `holds` adds the hold (a held pull request is marked draft,
which the merge queue refuses) and says that widening the rule waits
for the dispatcher's live trial and a week of outcome records
(aries-5ahrfm4h, aries-4x4vo7cq).

Three things are wrong with prose:

- **Memories are agents' to write.** `remember` changes a memory, and
  any agent of a member can call it. aries.md §21.3 rejected the pinned
  memory as the home of the workflow setting for this reason: "a setting
  that an agent's `remember` can change enforces nothing." The merge
  rule decides what reaches `main` without a person, so it is the last
  rule that should be agent-writable.
- **Judgment is not repeatable.** The coordinator applies the rule by
  reading it. Two coordinators, or one at two times, can read it
  differently, and nothing records why a pull request qualified beyond a
  line in a conversation (the memory `merges`: "Say in a line which PRs
  were queued and why"). The factory review (§3.5) ranks this "medium,
  rising": the day the dispatcher's sweep ships, rule 4 exists as code
  in one path and prose in the other.
- **Bearings cannot read prose.** Its merge train needs the rule as
  data, and its plan says otherwise today: "a human merges"
  (bearings.md §3.5) and "Unattended work never merges into `main`"
  (decision 17).

## 2. The rule, as inputs

Whatever its placement, the rule is a classifier: for one pull request,
**queue** or **hold, naming the condition that failed**. These are its
inputs, from both memories, with where each is read and how far it can
be trusted.

| Input | Condition | Read from | Who controls it | Trust |
|---|---|---|---|---|
| **Changed paths** | Rule 4's allowed set (docs guides, tests) and its exceptions; the memory `merges`'s forbidden areas (protocol, migrations, security code, what an agent may run, release and signing, dependencies, workflows) | The CI host: the files of the pull request against its base, both sides of a rename (`internal/forge`, [prs-check.md §5](prs-check.md#5-the-ci-host-behind-one-interface), gains `Files(n)`) | Whoever pushes | High: the merge queue tests the same commit |
| **Content of a test change** | Rule 4: a test-only change that deletes test lines or adds `t.Skip` is held (dispatcher.md §8), since paths cannot see it | The diff (`gh pr diff`) | Whoever pushes | Medium: a heuristic, which holds rather than guesses |
| **Required checks** | Green on the head commit | `forge.Required(n)`, required checks only, as prs-check.md §5 rules | CI | High; the queue reruns them anyway |
| **Epic approval** | The issue is an epic the maintainer approved, or a bug or task under one | The server: the issue's parent and the epic's approval gate (aries.md §21.6), which passes from the session `cli` only (decision 68) | The approvers the workflow names | High: an agent cannot pass a human gate |
| **Opener** | "A session the maintainer started opened it"; later a dispatcher run the maintainer started | Today only the `brief:` comment on the issue (the memory `briefs`), which names the harness and session; the host's author is the same account for every session | Any member's agent can post a comment | **Low** until aries-2qx4mlm2 stores briefs with the principal and session that started them (§6) |
| **Draft** | Not a draft | `forge.Open()`'s `isDraft` | Whoever can mark it | High: the queue refuses a draft whatever the classifier says |
| **Hold or freeze** | No hold on this pull request, no release freeze on any | A draft holds one pull request; the lock `merges/freeze` holds them all, as `dispatch/pause` pauses the dispatcher (decision 6) | Any member or agent may take the lock; only a person releases it | High: taking it only stops merges, and releasing it is a person's act |
| **The always-human floor** | Releases, tags, signing, the release notes; and the rule's own file and the files that govern agents (§5) | Built into the classifier, not the setting | Nobody, short of a release | High |

Two inputs need a person's act on the server (epic approval, a freeze);
three come from the CI host (paths, checks, draft); one has no trusted
source yet (opener). That last row is why the placement matters less
than it seems: wherever the rule lives, it is only as strong as its
weakest input.

**Fail closed.** No rule set, an input the host has not computed
(`mergeStateStatus` `UNKNOWN`, checks pending), an issue the server
cannot be reached for, or a pull request no issue names: each is a
hold, with the reason. A classifier that queued on a missing input
would turn every outage into a merge.

## 3. One classifier, whatever the placement

The placements below differ in where the rule's **document** lives and
who may change it. In every one, the rule is **evaluated by one client
command** that every caller runs (factory review, step 7), so that the
coordinator, the dispatcher's sweep and later Bearings' train cannot
read it three ways:

```
aries prs merges [--json] [--queue] [--since DURATION] [PR...]
```

- **By default it prints and changes nothing**: one line per open pull
  request, `queue` or `hold`, with the rule's version and, for a hold,
  the condition that failed (`hold #312 paths: internal/proto/frame.go
  (protocol)`). `--json` prints one document (rule 13).
- **`--queue`** queues each pull request it passes through the host's
  merge queue (`enqueuePullRequest`), and records the act on the issue
  (§4). It is the coordinator's, the sweep's and the train's step; a
  worker's harness keeps denying merges and `gh api`, as guard.md's
  built-in denies do.
- **It shares `internal/forge`** with `aries prs check` and `aries
  outcome`, and runs as part of the same pass where both are wanted.
- **CLI only** (rule 12): it reads the host with a person's or a
  machine's credentials and acts on `main`.
- **It never merges.** It queues; the host's queue reruns the required
  checks and merges, through branch protection. That stays the last
  line of defense (factory review §2).

The rule's document is the same in every placement: a version, a list
of named **classes**, each with its conditions, and the **floor** of §5
that no class can lift. What this repository would hold, from the two
memories:

```yaml
# .aries/merges.yaml
version: 1
classes:
  docs-and-tests:            # the memory `dispatch`, rule 4
    checks: green
    paths:
      allow:
        - docs/**/*.md
        - "**/*_test.go"
        - "**/testdata/**"
      except:
        - docs/design/**
        - docs/releases/**
        - docs/server.md
        - docs/security-model.md
        - "**/names_test.go"
        - scripts/**
        - "**/*.golden"
    tests: no-deletions      # held if a test loses lines or gains t.Skip
  approved-epic:             # the memory `merges`
    checks: green
    epic: approved
    opener: {harness: [claude-code-desktop], started_by: [alice]}
    paths:
      deny:
        - internal/proto/**
        - internal/store/migrations/**
        - internal/store/controlmigrations/**
        - internal/keyproof/**
        # … the rest of the memory's list
holds: draft                 # a draft is a hold; plus the lock merges/freeze
```

A pull request is queued when one class passes it and the floor does
not hold it. A class names the conditions it checks, so rule 4's class
asks nothing of the epic or the opener, and the memory `merges`'s asks
nothing of the paths beyond its deny list.

## 4. The three placements

### A. A project setting on the server

A `merges` document in the project's `settings` table, as the workflow
is ([aries.md §21.3](aries.md#213-the-setting), decision 95) and as
prs-check.md §3 puts `prs`. It is kept as a file in the repository
(`.aries/merges.yaml`), reviewed in a pull request, and applied by a
project admin with `aries admin merges set FILE --rev N`, from the
session `cli` only (`store.InPersonError`, as `workflow.set`). Every
runner of the classifier reads the server's copy.

### B. A file in the repository

`.aries/merges.yaml` alone, read by the classifier from a snapshot of
`origin/main`, as guard.md §8 reads `.aries/guard.yaml`. A change is a
pull request under the maintainer's review (a CODEOWNERS entry, guard.md
§13 step 1). Nothing is applied; the merge is the change.

### C. Rules in the client

The classifier with its rules in Go (the two memories' conditions coded
once), and a project choosing among them by flags or a short list in
`.aries.yaml`. A change to a rule is a change to `aries` and a release.

### Compared

| | A. Server setting | B. Repository file | C. Client code |
|---|---|---|---|
| **Who may change it** | A project admin, in person, from the session `cli`. The file's pull request is reviewed first, but the apply is the act | Whoever can merge to `main`: the maintainer, with CODEOWNERS required. Without CODEOWNERS, anyone whose pull request is merged | Whoever ships `aries`; a project picks among built-in rules with a flag, which any runner can change for itself |
| **Can an agent change it?** | No: an agent's session is not `cli`, and no MCP tool sets it (rule 12) | Only by a pull request a person merges; the file is in the floor (§5), so the rule never queues its own change | Only by a pull request to Aries itself |
| **How a change is audited** | A `merges.set` event in the project's log with the document before and after, the rev and who (rule 7), in the audit stream; joined to every queue act by rev | Git history and the pull request's review; the server sees nothing, so an act cannot be joined to a rule version except by commit | The release notes; the flags a runner passed are seen by nobody |
| **Every act audited** | Each `--queue` records a `merge:` comment on the issue naming the class and the setting's rev | The same comment, naming the file's commit | The same comment, naming the release |
| **Protocol cost** | New ops, so one bump (rule 10): see below | None | None |
| **One standard for the team** | Yes, the server's; the answer to prs-check.md Q3 ("a project/team based standard not an individual developer's settings") | Yes, if every runner reads `origin/main`; a runner on a stale fetch reads an older rule | No: each runner's flags are its own |
| **How the coordinator reads it** | `aries prs merges`, which reads the setting | Same, which reads the snapshot | Same, with flags |
| **How the dispatcher's sweep reads it** | In process, the same code | Same | Same |
| **How Bearings reads it** | Through the server, as `policy.apply` builds on `workflow.set` (aries.md §21.9; bearings.md §3.18) | From the repository, which Bearings' policy repository would then copy or point at | It links the same package |
| **A project with several repositories** | One setting per project, paths qualified by repository if needed | One file per repository, each its own rule | Flags per runner |
| **Offline** | The read is cacheable (`internal/offline`); a classifier that cannot reach the server holds everything (§2, fail closed) | Works offline; but the epic and freeze inputs need the server anyway | As B |
| **A floor across projects** | A server admin's default that projects may tighten and not loosen, as the workflow's (aries.md §21.9) | None | Built in |
| **Effort** | The most: ops, validation, events, `aries admin merges`, docs/server.md's limits | A loader and the snapshot (guard.md has both) | The least |

**The protocol cost of A**, and how to keep it to one bump. A `merges`
setting needs a write and a read, as `workflow.set` and `workflow` are:
two new ops, so rule 10's bump, the first protocol after 10. prs-check.md
slice R0b already needs the same for `prs`. Two ways to pay once:

- **A generic pair** (chosen, decision 2): `setting.set {name, document, rev}`
  and `setting {name}`, for named project documents the server stores,
  validates by name and logs as `setting.set` with before and after.
  `prs` and `merges` are its first two names, and a later one (a
  pointer to `guard.yaml`, guard.md decision 8) adds no op. Validation
  stays per name, in `internal/proto`, so a typo is refused as the
  workflow's is.
- **A section of the workflow document** (`merges:` beside `types:`):
  no new op, `workflow.set`'s event audits it, and Bearings' P0 already
  builds on it. But the workflow is checked strictly
  (`proto.ParseWorkflow` refuses an unknown key), so an older `aries`
  or `ariesd` refuses the whole document once it holds the section, and
  every workflow change is then a merge-rule change too, under the same
  apply. That couples two documents that change at different rates.

Either shares one bump with R0b's `pr` field and `prs` setting if they
land in the same release.

## 5. The floor: what no setting lifts

*Decided 22 Oct 2026 (decision 4):* the floor is in two parts.

**The product's floor**, built into the classifier for every project,
held whatever a class says; a project's setting may add to it and never
shorten it:

- **Releases and signing** (the memory `merges`: "always"; AGENTS.md
  rule 3): the release workflow, tags, the release notes and the
  release and signing code.
- **CI workflows**: `.github/workflows/**`, which hold the checks and
  the branch protection the queue relies on.
- **Dependency files**: `go.mod`, `go.sum` and `deps.allow` (rule 4: an
  agent never adds a dependency).
- **What agents may run**: the guard policy (`.aries/guard.yaml`), the
  brief template (`.aries/brief.txt`) and the harnesses' settings
  (`.claude/settings*.json` and their kin for other harnesses).
- **The merge rule's own file** (`.aries/merges.yaml`). A rule that
  could queue its own loosening would be no rule; it also keeps the
  file and the server's copy from drifting by an unreviewed merge.

The product names these by role, and a project whose files sit
elsewhere names its paths in its setting; for this repository the
defaults above are the paths.

**This repository's defaults**, in its own `.aries/merges.yaml`, as
deny lists of its classes: its security-sensitive paths
(`internal/keyproof`, `dpop`, `jose`, `oidc`, `credential`, the
server's sign-in, exchange, trust and HTTPS key proof, `secretscan`,
`safetext`), the protocol (`internal/proto`, rule 10) and the
migrations (`internal/store/migrations`, `controlmigrations`, rule 9).
They are this repository's judgment, not every project's, so they live
in its file, where a change to them is reviewed and applied like any
change to the rule.

## 6. The opener, and why it is the weak input

The memory `merges` condition (4) asks who opened the pull request.
Every session on this project pushes with the maintainer's `gh`, so the
host's author says nothing. What tells a coordinator's session from a
headless run, or from a session nobody briefed, is the `brief:` comment
the memory `briefs` asks for, whose first line names
`harness=…` and `session=…`.

That comment is any member's to write. An agent that posts a `brief:`
line on its own issue makes its pull request look like one the
maintainer started. The merge queue's rerun of CI does not cover this:
it is a question of who should have been trusted, not of whether the
code passes.

*Decided 22 Oct 2026 (decision 5):* until aries-2qx4mlm2 stores the
brief with the principal and session that started the run, as a record
an agent cannot write for itself, **an unverified opener never
queues**. The command reads the `brief:` comment, reports the opener as
`unverified` in its verdict, and holds any pull request whose only
passing class needs the opener. The coordinator's judgment covers the
memory `merges`'s condition (4) meanwhile, as it does today; once briefs
are stored, the command reads the stored record and the condition
becomes mechanical.

*Stored 23 Oct 2026 (aries-2qx4mlm2):* briefs are recorded by the op
`brief`, which no MCP tool sends, as comments of kind `brief` joined to
the claim of the session they start, and `show` returns the latest
([aries.md](aries.md) §22). What the record proves, and the choice it
leaves M4 within one principal, is in §22.4 there. Until M4, this
command still reads the comment's first line, and the opener stays
unverified.

## 7. Chosen: the server setting

*Decided 22 Oct 2026 (decisions 1 to 3).* **A, the server setting,
applied from a file in the repository, and evaluated by one client
command** (§3), with the generic `setting.set` and `setting` pair of
§4, shared with `prs`, not a section of the workflow document. Project
admins apply it for now, from the session `cli`, each change recorded
as a `setting.set` event; it moves under the enterprise `policy` role
(enterprise review E6) when that role exists. The reasons it was
recommended:

- **The authority is the server's.** Of the three, only A puts the
  change in a person's act that no agent can take and that the project's
  log records with before and after. That is the bar aries.md §21 set
  for the workflow, the bar prs-check.md Q3's answer set for `prs`, and
  the one that matters most for the rule that decides what reaches
  `main` without a person.
- **The source is the repository's.** The file is reviewed like code
  (B's strength), and `aries admin merges diff` and `aries merges check
  FILE` show and test drift between the file and the server, as the
  workflow's commands do.
- **The evaluation is the client's.** Decision 67 holds: `ariesd` never
  reads the CI host. The command is C's strength without C's weakness,
  since its rules come from the setting rather than from flags.
- **Bearings follows it for free.** Its train calls the same command or
  links the same package, and its P0 `policy.apply` sets the document
  through the same op, with Bearings' cosign on top (bearings.md
  §3.18), not a second store.

Against it: the most work, a protocol bump, and an apply step a person
must remember after the file's pull request merges, until Bearings'
`policy-apply` does it. The first slices (§9) need none of that: the
command runs read-only against a local file before the setting exists,
as `aries prs check` runs from flags before R0b.

**What this says for where project rules live** (guard.md decision 8,
which waited on this issue): a **team standard that grants authority**
(the workflow, `prs`, the merge rule) is a server setting applied from
a reviewed file; a **per-session control read at session start** (the
guard's policy, the brief template) stays a file read from a snapshot
of `origin/main`, since it must work before a session can reach the
server and grants nothing. The workflow setting may point at the
guard's file later without the guard moving.

## 8. Bearings, and what it inherits from the dispatcher

**Decision 17 is amended** (bearings.md §7, with §3.5, §3.14 and
decision 10): unattended work queues into `main` only what the
project's merge rule passes, through the host's queue and branch
protection, and everything else waits for a person. Below level D that
replaces "a human merges"; at level D, decision 25 and §3.16's
verification still apply on top. Until the setting exists, the rule is
the memories `dispatch` (rule 4) and `merges`, and Bearings follows
them "until otherwise directed".

**Which of the dispatcher's rules B0 to B3 inherit.** Decision 96
retires the dispatcher's loop "when Bearings' B0 takes it over", so B0
must carry everything the loop enforces on the day it replaces it, even
where a later stage builds the full version:

| The dispatcher's rule (dispatcher.md) | Inherited at | Becomes |
|---|---|---|
| Picking: model labels; never `human`, an epic, a `done` handoff, `waiting:maintainer` or release signing (§4; the memory `dispatch` rules 1-2) | **B0**, as they stand | B2's dispatch policy and label affinity (§3.9), keeping every exclusion |
| Claim before launch, under the run's session (§4) | **B0** | The supervisor's lease coupling (§3.1) |
| Pace: `dispatch/runs`, capacity 4, and `pgtest/HOST` (§5) | **B0**, the same locks | B2's governor (§3.7), still on Aries locks so the Desktop path counts too (factory review step 2) |
| Pause: `dispatch/pause` (§12) | **B0** | The governor's stop and, later, §3.16's pauses |
| Budget: the day's Opus output, the per-run wall clock and `--max-budget-usd` (§6) | **B0** | B2's governor and envelopes (§3.22) |
| Stops: two CI failures in a row, the first auth failure (§6) | **B0** | B2's circuit breakers (§3.14) |
| The brief template and its `brief:` record (§7; `.aries/brief.txt`) | **B0** | Unchanged; Bearings renders the same template |
| The guard's snapshot at launch (guard.md §8) | **B0** | Unchanged; sandboxes (§3.12) add to it |
| The sweep's resume and rework rules (§8) | **B1** | Patrol (§3.3) and handoff (§3.6) |
| Never run `aries land`; landing from the session `cli` (§8) | **B0** | Unchanged until a server CI reaches reports `landed` itself |
| Queueing merges under the merge rule (§8, this design) | **B3** | The train in queue mode (§3.5), calling the classifier of §3 |
| Conflicts: merge `origin/main`, never rebase a pushed branch (the memory `workflow` step 5) | **B3** | Unchanged; bearings.md §3.4 is amended to match (decision 8) |
| The state file and `log.jsonl` (§9, §12) | Not inherited | Typed state as Aries events (§3.2) |

## 9. Slices

| Slice | What | Needs | Protocol |
|---|---|---|---|
| M0 | This design, answered | The maintainer | — |
| M1 | **Starts now** (decision 7): `aries prs merges`, read only, with the rule from a local file (`--rules FILE`) and the product's floor built in: both memories' conditions as classes, `forge.Files` and the diff check, the opener reported unverified, fail closed, `--json`. The coordinator runs it beside its judgment and pastes its verdicts. It widens nothing, so it does not wait for the trial's evidence; widening the rule still does (the memory `holds` item 3) | M0; prs-check R1's `internal/forge` | None |
| M2 | `--queue` and the `merge:` comment on each issue it queues; the memory `merges` points at the command | M1 | None |
| M3 | The setting: `setting.set` and `setting`, `aries admin merges set|show|diff`, `aries merges check FILE`, validation, the `setting.set` event, limits; the command reads the server's copy | M1; joins R0b's bump | One bump, shared |
| M4 | The command reads the lock `merges/freeze`, whose release is refused to any session but a person's; and the stored opener, so the opener's class can queue | M3; aries-2qx4mlm2 | Per the lock's change: none if the release check is the store's behavior on an existing op |
| — | The dispatcher's sweep calls M2 (aries-i6fndp4h) | M2 | None |
| — | Bearings B3's train calls it | M3 | None |

**As built (M1, 22 Oct 2026; aries-5mk74jmh).** `aries prs merges
[--rules FILE] [PR...]`, read only ([docs/cli.md](../cli.md#the-merge-rule)):

- **The package.** `internal/merges` holds the rule file's loader
  (`rules.go`, strict: an unknown key or value is refused), the
  product's floor (`floor.go`, §5), a pure classifier (`classify.go`),
  the issue reader for the epic and the opener (`issue.go`), and the
  host (`github.go`). `internal/cli/prs.go` wires it to the command.
- **No `internal/forge` yet.** prs-check R1 has not built it, so the
  host is a small `merges.Forge` interface over three `gh` calls, `gh
  pr list --json …,isDraft,files`, `gh pr checks N --required` and `gh
  pr diff N`, never `gh api`. R1's `internal/forge` can take it over
  whole. The diff gives both sides of a rename, which the list's
  `files` does not, and the test condition's counts; the list's paths
  are joined to the diff's, so a path either names is classified.
- **The rule's file.** This repository's `.aries/merges.yaml` holds the
  two memories as the classes `docs-and-tests` (rule 4, with its
  exceptions as `except` and `deny`) and `approved-epic` (the memory
  `merges`, its protocol, migration, security and agent-permission
  paths as `deny`). It is read from `origin/<default branch>` as the
  last fetch left it, never the working tree, so a branch cannot loosen
  its own verdict; `--rules FILE` reads a file not yet merged, and the
  floor holds that file's path too. The memories stay the rule.
- **The epic.** Read through `show`, with no new op: the issue, or an
  epic above it (up to eight parents), has a human gate that blocks it
  with the outcome `passed`, and `show` lists no open gate holding the
  issue. A waived gate, or an epic made before the workflow and so
  without an approval gate, does not pass.
- **The opener.** The last `brief:` comment's fields are reported, and
  the class that asks for an opener always holds (decision 5): so in M1
  only `docs-and-tests` can pass, and the coordinator judges the memory
  `merges`'s pull requests as before.
- **Held whatever the class.** A draft, a base other than the default
  branch, a path that cannot be classified (quoted by git past reading,
  or not clean), and a floor path. A skipped required check is not
  green.
- **Not built.** `--since`, the lock `merges/freeze` (M4), `--queue`
  (M2) and the server setting (M3).

The memories stay the rule until M3 is applied; from then on each says
only that the setting is the rule, so that there is one statement of it.

## 10. Decisions (maintainer, 22 Oct 2026)

The maintainer accepted all nine recommendations (a comment on
aries-zxxoccdq, 22 October 2026). The questions as they were put are in
this file's history.

1. **Placement:** A, a server setting applied from a reviewed file in the repository, evaluated by one client command (§7).
2. **One bump:** a generic `setting.set` and `setting` pair, shared with `prs`, not a section of the workflow document (§4).
3. **Who applies it:** project admins for now, each change recorded as an event; it moves under the enterprise `policy` role (E6) when that role exists (§7).
4. **The floor** splits in two: the product's, for every project (releases and signing, CI workflows, dependency files, what agents may run, the merge rule's own file), and this repository's defaults in its file (its security-sensitive paths, the protocol, the migrations) (§5).
5. **The opener:** until briefs are stored (aries-2qx4mlm2), the command reports it as unverified and does not queue on it; the coordinator's judgment covers it meanwhile (§6).
6. **The freeze:** a lock `merges/freeze` that any member or agent may take and only a person may release; a draft stays the hold for one pull request (§2, M4).
7. **Evidence:** M1 does not wait for the trial, since it is read-only and widens nothing; widening the rule still waits (the memory `holds` item 3) (§9).
8. **Rebase or merge:** B3's train merges `origin/main` and never rebases a pushed branch, as the memory `workflow` step 5 says; bearings.md §3.4 is amended to match (§8).
9. **The name:** `aries prs merges`; `aries merges check FILE` stays free for M3 (§3, §9).
