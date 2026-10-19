# Lessons: an annotation on handoff and finish, beside the run's outcome

**Design, 19 October 2026; answered the same day (decision 101), and
revised the same day after a critical review the maintainer adopted
(aries-l436tgkj, L2 of the learning loop, epic aries-dmqqolg4).**
Nothing in it is built.

The maintainer first answered §14's questions: twelve on the field,
then five on lessons across projects. They then adopted a review of the
learning loop, in their words: "Adopt the review's revised sequence:
per-run record primary, lessons as annotation; v0.6.0 ships without
lessons (protocol 10); pre-registered rules, one change at a time;
defaults from one project, checks after two; open stages, optional
recommend, no org lessons in prime." The sections below follow the
revision. §14 keeps every earlier answer visible, and marks the ones the
review changed as superseded on 19 Oct 2026, with the reason.

The learning loop has four slices:

- L1 (aries-wf6da6vg) captures lessons now, as `lesson:` comments at
  handoff, in the template of the project memory `lessons`.
- L2, this design, makes a lesson a field with a query.
- L3 (aries-g34cdfb3) is a scheduled distillation. It reads the work's
  outcomes, with lessons beside them, and proposes workflow changes for
  the maintainer to approve.
- L4 (aries-bbpl7hmp) builds what survives evaluation (§12) into `aries`
  and `ariesd` as defaults and checks.

Since the revision, the loop's primary signal is not the lesson. It is
**a per-run outcome record**, which the epic tracks as an issue of its
own (§2).

This is a document of its own, not a section of
[aries.md](aries.md), for the reasons [dispatcher.md](dispatcher.md) and
[sextant.md](sextant.md) are: `aries.md` is past 2,600 lines, and the
loop's later slices (L3's distillation, L4's playbook) will want sections
beside this one rather than scattered through the plan. Where the build
departs from it, this document gets the **As built** notes (rule 17), and
`aries.md` §14 holds the decision.

## 1. The line the maintainer drew

A lesson is about **how the agentic dark factory works**, never what the
repository's code does (the maintainer, 19 Oct 2026; the memory
`lessons`, rev 3). That means how work is picked, briefed, claimed, run,
tested, reviewed, merged, landed and cleaned up. It means how sessions
run in parallel and hand off, what needed a person and why, and
permissions, tools and harnesses. It covers CI and its failures as
events to handle, and cost, tokens and idle time.

A bug's cause, an API or a file's layout belongs in the issue, the pull
request or the docs. Do not record "wsconn's max raced with Close".
Record the process point it shows: "a docs-only PR failed tests on a
flake; triage the failing test before suspecting the PR, file it,
rerun".

The maintainer added, the same day: "The lessons learned stored in the
repo should be general enough to be baked into the aries and ariesd
tools." A lesson the playbook keeps is one the product can carry for
every project, not advice for this repository's sessions to remember.

Each lesson has a **scope**:

- `factory`: general enough to be built into `aries` or `ariesd` for
  every project, as a default, a check or a refusal with a Fix, text
  that `aries setup` installs, or a step of the dispatcher. It is stated
  without this repository's names, paths or machine. These feed L3, and
  through what survives evaluation, the tools.
- `project`: it holds for this repository or this machine only (its
  tools, limits, quirks). It may change this repository's memories,
  never the product. It is kept and queryable, and left out of the
  playbook.

`recommend`, when given, says where in the tools the change would live.

Aries cannot tell a lesson about the factory from one about the code.
The line is held by what agents are told (the memory, the brief, the
tool description) and by the distillation, which weighs only `factory`
lessons for the playbook. The store's part is to make the scope a
required field, so that every lesson says which side of the line its
author put it.

## 2. The outcome record first, the lesson beside it

**The primary signal is a per-run outcome record**, captured after the
outcome is known rather than written by the agent at handoff. It is
filed separately, as the outcome record issue under epic
aries-dmqqolg4. For each run it records:

- the brief, the model and the duration;
- CI attempts and the cause of each failure;
- merge conflicts, reopens, and waived items and gates;
- held time and cycle time;
- tokens, and pre-merge rework: the tokens spent after the first
  handoff with state `done`, or after the first CI failure.

Those facts are measured, not remembered. They are what L3 counts and
what §12's rules are judged by.

**A lesson is an annotation read beside the record.** It says what the
agent thought happened and what it would change. That is useful for
forming a hypothesis, and weak as evidence. A lesson never moves a
metric by itself. L3 reads lessons to find what to test, and the outcome
records to decide whether it worked.

**What L1 has shown so far, and what it has not.** L1's template is a
lesson per comment, a field per line: `lesson`, `stage`, `worked`,
`failed`, `fix`, `recommend`, `tags`, `scope`. The seven seed lessons of
19 Oct 2026 are on aries-o5jxfa7h twice, aries-2cjs2bbp, aries-p2r6hqnb,
aries-w5kyz3hj, aries-hzuyf4mt and aries-wf6da6vg. They were **written
by one coordinator session in one batch, after the fact**, not by the
sessions that did the work, at their handoffs. So they do not show that
the template is right, or that working sessions write it well, and an
earlier draft's conclusion that it did is struck. The template is fixed
only after a week of lessons from working sessions (L1's own done-when).

Two things the seeds do show hold whoever wrote them:

- **Evidence is links buried in prose.** `failed` held "PR 257, job
  118131675020" as text. The distillation needs them as links it can
  count and follow, so the field adds `evidence`, a short list of typed
  links, beside the prose.
- **Names drift.** `general` and `repo` became `factory` and `project`
  within a day, and tags were invented per lesson (`parallel-sessions`,
  `ci-flake`, `ci`). Only `scope` is fixed by the store; `stage` and
  tags stay open while the week's lessons show what sessions reach for
  (§3).

## 3. The lesson

| Field | Required | Holds |
|---|---|---|
| `lesson` | yes | One line about the workflow |
| `stage` | yes | A kebab-case word. Suggested values: `pick`, `brief`, `claim`, `run`, `test`, `review`, `merge`, `land`, `cleanup`, `parallel`, `escalate`, `cost`, `tooling` |
| `scope` | yes | `factory` (general enough to build into `aries` or `ariesd` for every project, stated without this repository's names, paths or machine) or `project` (this repository or machine only; may change its memories, never the product) |
| `worked` | no | What in the workflow went right |
| `failed` | no | What in the workflow went wrong, with evidence: a change, a CI run, an error line, time lost or tokens spent |
| `fix` | no | What was done |
| `recommend` | no | The change to the workflow, and where in the tools it would live: a default, a check or a refusal with a Fix, text `aries setup` installs, a dispatcher step, a brief line |
| `tags` | no | Lowercase kebab-case words (`ci-flake`, `permissions`, `human-needed`) |
| `evidence` | no | Typed links: `ci:ID`, `change:ID`, `issue:ID`, `commit:SHA` or an `https` URL |

The store records, without being sent them: the issue, the principal,
the session, the time (`wtx.now`, rule 19), and the issue's rev the
handoff or finish left.

- **`stage` is open** for at least a month from the field's release, and
  then reconsidered with what was written. Any kebab-case value is
  accepted. A value outside the suggested list is kept, and the result
  carries a warning naming the list, not a refusal. A closed list would
  freeze names before anyone knows which stages sessions need, and
  would cost a handoff for a word.
- **`recommend` is optional.** A lesson may only record what happened.
  The distillation, not the author, decides what to change.
- **"Nothing to report" is valid, and counts.** A handoff or finish may
  send `lessons_none: true` instead of lessons. The store records
  `lesson.none` on the issue, so that a session that had nothing to say
  is told apart from one that was never asked. The share of runs that
  reported (lessons or none) is L1's measure.
- **Tags are free** within their shape. They are how a new theme shows
  up before anyone names it.
- **Evidence is typed but not tied to one forge or CI system.**
  - `ci:ID` is a CI run or job, in whatever CI the project uses.
  - `change:ID` is a pull request or a merge request.
  - `issue:ID` is an Aries issue.
  - `commit:SHA` is a commit.
  - An `https` URL is anything else.

  L3 resolves each type with the project's own settings. Aries fetches
  nothing. An evidence link holds no credentials: the secrets lint reads
  it, and a URL with userinfo is refused.
- **No person's name.** The memory says "the maintainer" or "a
  session". The store cannot enforce it; the guide and the tool
  description say it.

## 4. Where it is stored, and the event log

Lessons go in a table of each project's schema, `lessons` (the next
migration, rule 9), with `lesson_tags` and `lesson_evidence` beside it,
as memories have `memory_tags`. A lesson is written through
`Store.write` (rule 5), in the same transaction as the handoff or
finish that carries it. Each lesson appends an event, `lesson.create`,
targeting the issue (`wtx.event`, rule 7), and so does `lesson.none`.
The issue's history shows them, and the event log stays the truth.

Lessons are **append-only**. A lesson is not edited; a better one is
written.

"Beside the issue's outcome and cost" means a join, never a copy. Once
the outcome record exists (§2), the query and the export join a lesson
to the record of the run that wrote it, by session. Until then they
derive the issue's outcome (`landed`, `closed`, `open`), its rework and
its cost the way
[`aries cost --group-by outcome`](../concepts.md#cost-per-outcome)
does. A lesson written at handoff, before its PR lands, would otherwise
carry an outcome already out of date.

## 5. Writing and reading

### Writing: handoff and finish

`handoff` and `finish` gain `lessons`, a list of lessons, and
`lessons_none`. On the wire (`internal/proto`), each lesson is a
structured object with the fields of §3. The server validates each one
and writes all of them or none, with the handoff or finish. A refused
lesson refuses the call, with a fix naming the lesson's index and its
field: an agent never loses its handoff to a typo silently, and never
records half a lesson. The refusals are a missing required field, a
value of the wrong shape, the secrets lint, safetext and the caps (§7).
An unknown stage is not refused.

A lesson is written where the workflow ends:

- a session that opens a pull request hands off with state `done` (the
  memory `workflow`, step 5);
- read-only work finishes (step 11).

A session that holds no issue, such as a coordinator between waves,
writes its lesson on the issue it is about. There is no lesson without
an issue, since the issue is what joins it to an outcome.

**MCP.** The tool budget (rule 12) is 3,000 estimated tokens for every
tool's schema. The tools cost about 2,823 today (measured by
`TestTools`, 19 Oct 2026), leaving 177. `handoff` and `finish` take
`lessons` as **a structured list**, each item an object with the fields
of §3, with `scope` as an enum and `stage` as a string. A harness then
validates the shape before the call, and an agent never writes a
template by hand. That costs about 250 tokens and the read (below)
about 90, so the budget rises to **3,200**, as decision 76 raised it to
3,000 for stage 6's tools (Q5). Descriptions stay terse: what counts
lives in the memory `lessons` and the brief, not in the schema.

The CLI takes the same fields from L1's template text, with `--lesson
FILE` (or `-` for stdin), repeatable, on `aries handoff` and `aries
finish`. It refuses a file it cannot parse with a fix naming the line,
before anything reaches the server.

### Reading: `aries lessons`

```
aries lessons --since 7d [--until T] [--stage S] [--tag T] [--scope factory|project] [--issue ID] [-n N] [--json]
```

Results come newest first, 50 by default and at most 500. Each line
gives the lesson's date, issue, stage, scope and `lesson`, then
`recommend` if there is one. `--json` prints one document (rule 13)
with every field and each issue's outcome in the window (§4). `show`
returns an issue's lessons with it, as a result field (no protocol
bump), counted in its budget and cut first.

**The MCP read** is a `lessons` tool with `since`, `stage`, `tag` and
`scope` (Q4). It returns at most 20 lessons, each cut to `lesson`,
`recommend`, its issue and stage, within the 2,000-token result cap,
and marked `untrusted` as every result is. Prime and start show no
lessons, a project's own or an organization's (§10). Their budgets are
for the work at hand, and a lesson there would read like an
instruction.

The read is refused offline, as `list` and `ready` are. A stale answer
about recent lessons is worse than none, and the cache is for the work
a session holds (design §18.6).

## 6. The export for the distillation

L3 reads a window of lessons beside the outcome records:

```
aries lessons --since 2026-10-12 --until 2026-10-19 --export > lessons.json
```

The export is one JSON document: the window, then each lesson with
every field and:

- its issue's id, type, labels and epic, and its outcome in the window
  (§4);
- the **session id** and the **principal's kind** (agent, person,
  service), and **nothing else of the principal** (Q9, revised);
- the `lesson.none` reports in the window, so that L3 can measure
  coverage.

The session id joins a lesson to its run's outcome record, its brief,
model and effort (aries-2qx4mlm2), and its usage. Counts are per
session, never per person (decision 97, Sextant's §4). The distillation
needs the run, not the author.

- **Paged, not unbounded.** At most 2,000 lessons a call. A longer
  window gives `next`, which `--after` takes.
- **Scope is a filter, not a redaction.** `--scope factory` is what L3
  reads for the playbook. `project` lessons are exported only when
  asked for, for a project's own retrospective.

The export is the CLI's alone, as `aries cost` is. It is for a
scheduled run and a person, never an agent's tool call (rule 12).

## 7. Limits, the secrets lint, safetext, and lessons as data

**Limits (rule 18)**, kept to what L2a needs until the field is
validated. Each limit freezes a `limits:` name for v1.0.0:

| Setting | Default | Caps |
|---|---|---|
| `lessons_per_issue` | 50 | lessons on one issue, all time |
| `lesson_bytes` | 4,096 | one lesson, all its fields, tags and evidence together |

Per call, at most 5 lessons, and per lesson at most 8 tags and 8
evidence links. These are fixed in `internal/proto` as `MaxPaths` is,
not settings. A refusal past any of them is `invalid`, with a fix
naming the cap. The per-field caps of the first draft (a `limits:` name
for each field) are deferred until a month of lessons shows which fields
grow.

**The secrets lint** (`internal/secretscan`) runs on every field and
every evidence link, as it runs on a memory's key, body and tags. It
also runs on the outbox's entries before they are written: a handoff
with lessons, queued offline, is linted when it is queued, not first at
replay. A finding refuses the whole call with `invalid` and a fix
naming the field, for example "remove the credential from lesson 2's
failed and retry". Lessons are where an agent pastes an error line, so
this is the lint's most likely catch.

**safetext**: the store refuses control and bidi characters in every
field, as it does for an issue's text, and every client escapes what it
prints.

**Lessons are untrusted data.** They are written by agents, about
agents, and read by the next agent and by L3's model. Each reader is
told so: MCP results carry `untrusted`, the export's header says it, and
L3's prompt quotes lessons as data and never as instructions. A
lesson's `recommend` is a hypothesis for §12's evaluation, never a rule
anything applies. Nothing in Aries reads a lesson and changes behavior.

**Redaction is deferred** (Q11, revised). It is an admin op that would
freeze before the field is validated. Until it is built, a secret the
lint missed is handled as any leaked secret: rotated, which is the fix
that matters. The row then waits for redaction, which is designed with
the v1.0.0 surface review.

## 8. The protocol

`handoff` and `finish` gain argument fields (`lessons`, `lessons_none`),
and there is a new op, `lessons`, so this is a protocol change (rule
10). **v0.6.0 ships without lessons, and does not wait for them** (Q6,
revised). This change takes **protocol 10**, the release after v0.6.0
(the memory `release-versioning`, rule 3): `Proto = ProtoMax = 10`, and
`ProtoMin` at v0.6.0's 9.

A protocol 9 client sends no lessons and never calls `lessons`, and a
protocol 10 server works without either. A protocol 10 client against a
9 server is refused at the handshake and told to have the server
upgraded. The `lessons` op's scope (`internal/server/scopes.go`) is a
read, as `list`'s is.

## 9. L1's comments beside the field

L1's comments stay as they are. **The import is deferred** (Q7,
revised): its `from-comments` command and its admin op would freeze
before the template is fixed, and a week of comments is small enough to
read as text. L3's first run reads L1's `lesson:` comments directly,
through `comments`, beside whatever the field holds. Whether an import
is worth building is decided once the template is fixed (§2), with the
first month of the field's lessons in hand.

## 10. Lessons across the organization

Asked by the maintainer, 19 Oct 2026: the server (`ariesd`) should give
a team or an organization the lessons learned across its projects, so
that a new team or project starts from them. Q13 to Q17 settled its
shape. The review then held it back: **the organization tier (L2f,
aries-ymuolcla) stays a design until a second project exists and an
injection test exists.** The injection test checks that an organization
lesson carrying instructions is not followed by the sessions that read
it. **No organization lessons appear in prime or start**, even when
built (Q15, revised).

The design, as it stands for when those two conditions hold:

| Tier | Where | Who reads it |
|---|---|---|
| A project's lessons, `project` scope | The project's schema | The project's members |
| A project's lessons, `factory` scope | The project's schema | The project's members; across projects, the organization role (Q14) |
| Organization lessons: curated, approved | Organization memory, at the organization's scope | Every project's members, on request |

- **Raw lessons never cross projects** (Q10, Q13). A member of one
  project does not read another project's lessons, whatever their
  scope. They hold that project's issue ids, change numbers and
  evidence, which are its members' alone.
- **Reading across projects is the organization's.** `aries admin
  lessons --all-projects` is served by `ariesd` from every project's
  schema, `factory` scope only. It is CLI only, for server admins and
  the role that writes organization memory (Q14). Each read is recorded
  as an event in the control store (`lessons.read_all`) (Q17).
- **Organization lessons are organization memories of the kind
  `lesson`** (Q16), with their provenance as a field. What the
  distillation proposes, and survives §12's evaluation, a person with
  the organization role approves.
- **A new project starts from the defaults**, not from lessons in its
  prime. What survived evaluation is already built into `aries` and
  `ariesd` as defaults (§12). A new project gets the server's default
  workflow (design §21.9), and `aries admin playbook export` writes the
  playbook as files its repository commits. Organization lessons are
  read on request, through `recall` and the CLI.

The admin ops of this tier (`--all-projects`, approval, the playbook
export and seed) are deferred with it. They take the protocol of the
release they ship in.

## 11. Briefs

The dispatcher's brief (dispatcher.md §7) gains one item, after the
workflow's: "Before handoff or finish, record lessons in `lessons` (the
memory `lessons` says what counts), or `lessons_none` when nothing
surprised you." A coordinator's briefs say the same until the
dispatcher exists.

The brief's own record (aries-2qx4mlm2) and the lesson share the
session id. That lets the export join a lesson to the brief that
started its session, and §12 compare briefs that differ by one rule.

## 12. Evaluation

L3 proposes and L4 ships only what this section lets through. A
lesson, however well argued, is a hypothesis.

1. **Pre-registered.** Each proposed rule is written down before it is
   tried, with:
   - one metric, from the outcome record (§2), such as pre-merge rework
     tokens, CI attempts per landed issue, reopens, or held time over
     cycle time;
   - a predicted direction;
   - a minimum number of runs;
   - a kill criterion, the result at which the rule is withdrawn.

   Nothing measured after the fact is promoted to the rule's metric.
2. **One change at a time.** One rule is under evaluation at a time,
   and the memory `workflow` is frozen for its window. Otherwise two
   changes' effects cannot be told apart.
3. **Randomized where it can be.** A rule that is text in a brief is
   given to a random half of runs at the brief, once the dispatcher
   builds briefs (dispatcher.md §7), and the halves are compared. Until
   then it is compared before and after, which this section counts as
   weaker evidence.
4. **Defaults from one project, checks after two.** A rule that
   survives its window on one project's evidence ships first as a
   **default with a setting**, which a project may turn off. It becomes
   a **check or a refusal with a Fix** only after it has survived its
   window and a second project's evidence too.

The epic's own measure (aries-p2r6hqnb) compares the work metrics
before and after each rule, and records them on the epic.

## 13. Slices

| Slice | What | Done when |
|---|---|---|
| L2a | The migration, `lessons` and `lessons_none` on `handoff` and `finish`, the two limits and the fixed caps, the lint, safetext, `lesson.create` and `lesson.none`, `show`; protocol 10 | A handoff with lessons records them once, and `show` returns them. A lesson the lint flags is refused with a fix. An unknown stage is kept with a warning. `lessons_none` is recorded |
| L2b | `aries lessons` and the `lessons` op; the structured `lessons` on the MCP `handoff` and `finish`, and the MCP `lessons` read; the CLI's parsing of the template; the schema budget at 3,200 | The query filters by window, stage, tag, scope and issue. The tool budget test passes at 3,200 |
| L2c | The export, joined to the outcome record once it exists (to the derived outcome until then) | The export of a window names sessions and principals' kinds only, and matches the outcome records for its runs |
| L2e | docs/agents.md and docs/cli.md; docs/server.md's limits; the memories `lessons` and `workflow` step 12 moved to the field; dispatcher.md §7 | docs/agents.md says what to record and who can read it |

**Deferred** until the field is validated, because each would freeze
surface for v1.0.0 first:

- the per-field limits (§7);
- redaction (§7);
- the import of L1's comments (§9);
- the kept counter of lessons by stage and scope (Q12), whose stage
  attribute an open stage would make unbounded;
- the organization tier and its admin ops (§10, L2f);
- closing the stage list (§3).

L2d, the import, is dropped from the plan until then.

## 14. Questions for the maintainer

Answered by the maintainer, 19 Oct 2026 (decision 101). Some answers
were superseded the same day, when the maintainer adopted the critical
review of the learning loop. The earlier answer stays visible, followed
by the revision and its reason.

1. **A document of its own.** This design is `docs/design/lessons.md`,
   with a row in docs/design/README.md, rather than a section of
   `aries.md`; L3 and L4 would add sections here. *Recommended:* yes.
   *Answered 19 Oct 2026:* as recommended.
2. **The fields.** The template's eight fields plus `evidence`, with
   `lesson`, `stage`, `scope` and `recommend` required, and the
   issue's `kind` dropped (§2, §3). *Recommended:* as proposed.
   *Answered 19 Oct 2026:* as recommended. *Superseded 19 Oct 2026:*
   `recommend` is optional, and "nothing to report" (`lessons_none`) is
   valid and counts. The reason: a lesson annotates the outcome record,
   and the distillation, not the author, decides what to change; a
   session with nothing to say should be counted, not pushed to invent
   a recommendation.
3. **Evidence required?** The epic says a lesson without evidence does
   not count. Either the store refuses a lesson with `failed` and no
   evidence link, or it keeps it and L3 weighs it at zero.
   *Recommended:* keep it, and weigh it at zero. A refusal at handoff
   costs the whole handoff for a missing link, and some lessons (a
   permission prompt, a time lost) have no link to give. *Answered 19
   Oct 2026:* as recommended. *Revised 19 Oct 2026:* the evidence types
   are not specific to GitHub. They are `ci:`, `change:` (a pull or
   merge request), `issue:`, `commit:` and a URL (§3). The reason: the
   loop is meant for any project's forge and CI.
4. **The MCP read.** A `lessons` tool spends most of the 177 tokens left
   of the schema budget for a use no session has yet. Either build it in
   L2b, or leave agents the CLI's `--json` until L3 or the dispatcher
   asks for it. *Recommended:* leave it out of L2, and keep the budget
   for the write. *Answered 19 Oct 2026:* build it in L2b (§5), within
   the budget Q5 raises.
5. **The MCP write as the template's text.** `aries mcp` parses the
   template text into the structured lesson, instead of a structured
   schema on two tools that would break the budget. *Recommended:* yes;
   the alternative is raising the budget to 3,200 (decision 76's
   precedent), which the next feature will want too. *Answered 19 Oct
   2026:* a structured lesson on `handoff` and `finish`, with the
   budget raised to 3,200 (§5). The CLI keeps the template text.
6. **Join protocol 9.** v0.6.0 is not tagged, so this joins 9; if it
   ships first, this takes 10. *Recommended:* join 9, and build L2a
   before v0.6.0 is cut if the release can wait for it. *Answered 19
   Oct 2026:* as recommended: v0.6.0 waits for L2a and L2b, which join
   9. *Superseded 19 Oct 2026:* v0.6.0 ships without lessons and does
   not wait; lessons take protocol 10 (§8). The reason: the field is
   not validated yet, and a protocol carries what it adds for good.
7. **L1's comments.** Import once, through a file a person checks, and
   leave the comments as they are (§9). *Recommended:* as proposed.
   *Answered 19 Oct 2026:* as recommended. *Superseded 19 Oct 2026:*
   the import is deferred; L3 reads the comments as text (§9). The
   reason: its command and admin op would freeze before the template is
   fixed, for a week of data.
8. **Stages a closed list, tags free.** The store refuses an unknown
   stage and accepts any tag of the right shape (§3). *Recommended:* as
   proposed; L3 proposes new stages like any other change. *Answered 19
   Oct 2026:* as recommended. *Superseded 19 Oct 2026:* `stage` is
   open, any kebab-case value, with a warning rather than a refusal, for
   at least a month, and then reconsidered (§3). The reason: a closed
   list freezes names before anyone knows which stages working sessions
   need.
9. **No principal in the export.** The export names a principal's kind
   and the session, never the principal (decision 97). *Recommended:*
   yes; `aries lessons` without `--export` shows who wrote each one to
   the project's members, who can see it in the issue's history anyway.
   *Answered 19 Oct 2026:* include the principal in the export; counts
   stay per session. *Superseded 19 Oct 2026:* the export names the
   session and the principal's kind only (§6). The reason: the
   distillation joins a lesson to its run's outcome record by session,
   and needs the run, not the author (decision 97).
10. **Organization memory only through approval.** Raw lessons never
    reach organization memory; only what L3 proposes and the maintainer
    approves does (§10). *Recommended:* yes. *Answered 19 Oct 2026:* as
    recommended. Since the revision, what is approved must also have
    survived §12's evaluation.
11. **Redaction by a project admin.** The one change a lesson can have
    (§7). *Recommended:* yes, CLI only. *Answered 19 Oct 2026:* as
    recommended. *Superseded 19 Oct 2026:* deferred (§7, §13). The
    reason: it is an admin op that would freeze for v1.0.0 before the
    field is validated.
12. **A metric.** A counter of lessons recorded, by stage and scope,
    added to the kept names (rule 2), so that Sextant can show the share
    of issues with a lesson. *Recommended:* yes, in L2a, since names are
    kept from v0.5.0 and adding one later costs the same. *Answered 19
    Oct 2026:* as recommended. *Superseded 19 Oct 2026:* deferred (§13).
    The reason: kept names freeze, and with stages open a stage
    attribute would have no fixed set of values (rule 2). Coverage is
    measured from the export's `lesson.none` reports until then.

### Asked after the first twelve were answered (19 Oct 2026)

13. **What crosses projects.** Curated organization lessons for every
    project, raw `factory` lessons queryable by an organization role,
    or every project's members reading every project's `factory`
    lessons. *Recommended:* the first two together. *Answered 19 Oct
    2026:* curated only: raw lessons never cross to another project's
    members (§10). Since the revision, the whole tier stays a design
    until a second project and an injection test exist.
14. **Who reads raw lessons across projects.** *Recommended:* server
    admins and the role that writes organization memory, which the
    distillation's service principal holds. *Answered 19 Oct 2026:* as
    recommended. Deferred with the tier (§10).
15. **How a new project starts from them.** *Recommended:* organization
    lessons in prime and start within their caps, and the playbook as a
    seed when the project is created. *Answered 19 Oct 2026:* as
    recommended. *Superseded 19 Oct 2026:* no organization lessons in
    prime or start. A new project starts from the defaults built into
    the tools, and from the playbook export (§10). The reason: prime is
    the surface most likely to be read as instructions, and what has
    survived evaluation belongs in the tools' defaults, not in a
    reminder.
16. **An organization lesson's storage.** An organization memory of a
    kind `lesson`, or a table of its own beside organization memory.
    *Recommended:* an organization memory of a kind `lesson`, with
    provenance as a field, so that prime has one ranked list of
    organization guidance and one cap; settled in aries-iakie56e's
    design. *Answered 19 Oct 2026:* as recommended; aries-iakie56e's
    design builds it so. Deferred with the tier (§10).
17. **The organization role across projects.** Reading every project's
    `factory` lessons gives the role every project's issue ids and
    evidence. *Recommended:* accept it for the role, since it already
    writes what every project reads, and record each cross-project read
    as an event in the control store. *Answered 19 Oct 2026:* as
    recommended. Deferred with the tier (§10).
