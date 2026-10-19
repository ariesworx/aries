# Lessons: a field on handoff and finish, and a query

**Design, 19 October 2026; answered the same day (decision 101)
(aries-l436tgkj, L2 of the learning loop, epic aries-dmqqolg4).**
Nothing in it is built. The maintainer answered §13: nine questions as
recommended, and three otherwise. The MCP read is built in L2b (Q4).
The MCP write takes a structured lesson, with the schema budget raised
to 3,200 (Q5). The export names each lesson's principal (Q9). The
sections below are revised to match. The maintainer then asked for
lessons learned across the server's projects, for new teams and
projects to start from: §10, with Q13 to Q17 answered.

The learning loop has four slices: L1 (aries-wf6da6vg) captures a lesson
on every issue now, as a `lesson:` comment at handoff, in the template of
the project memory `lessons`; L2, this design, makes it a field with a
query; L3 (aries-g34cdfb3) is a scheduled distillation that reads lessons
beside the work's outcomes and proposes workflow changes, each approved
by the maintainer; L4 (aries-bbpl7hmp) ships what is approved as the
playbook and defaults, measured against rework, CI failures, idle time
and cost per landed issue (aries-p2r6hqnb).

This is a document of its own, not a section of
[aries.md](aries.md), for the reasons [dispatcher.md](dispatcher.md) and
[sextant.md](sextant.md) are: `aries.md` is past 2,600 lines, and the
loop's later slices (L3's distillation, L4's playbook) will want sections
beside this one rather than scattered through the plan. Where the build
departs from it, this document gets the **As built** notes (rule 17), and
`aries.md` §14 gets the decision once the maintainer answers.

## 1. The line the maintainer drew

A lesson is about **how the agentic dark factory works**, never what the
repository's code does (the maintainer, 19 Oct 2026; the memory
`lessons`, rev 2). How work is picked, briefed, claimed, run, tested,
reviewed, merged, landed and cleaned up; how sessions run in parallel
and hand off; what needed a person and why; permissions, tools and
harnesses; CI and its failures as events to handle; cost, tokens and
idle time. A bug's cause, an API or a file's layout belongs in the
issue, the pull request or the docs. Not "wsconn's max raced with
Close", but "a docs-only PR failed tests on a flake; triage the failing
test before suspecting the PR, file it, rerun".

Each lesson has a **scope**:

- `factory`: it would change the workflow of any project. These feed the
  playbook (L4), through the distillation (L3).
- `project`: it holds for this repository or this machine only (its
  tools, limits, quirks). Kept, queryable, and left out of the playbook.

Aries cannot tell a lesson about the factory from one about the code;
the line is held by what agents are told (the memory, the brief, the
tool description) and by the distillation, which weighs only
`factory` lessons for the playbook. The store's part is to make the
scope a required field, so that every lesson says which side of the
line its author put it.

## 2. What L1 taught the shape

L1's template is a lesson per comment, a field per line: `lesson`,
`stage`, `worked`, `failed`, `fix`, `recommend`, `tags`, `scope`. The
seven seed lessons of 19 Oct 2026 (on aries-o5jxfa7h twice,
aries-2cjs2bbp, aries-p2r6hqnb, aries-w5kyz3hj, aries-hzuyf4mt and
aries-wf6da6vg) were written before `stage` existed and with the scopes
`general` and `repo`, and a later comment on aries-wf6da6vg maps each
to its stage and to `factory` or `project`. Three things follow:

- **The template is right, and agents write it well.** Every seed lesson
  filled every field, and each `failed` named its evidence: a pull
  request, a CI job id, an error line, a time. The field keeps the
  template's fields and their order, and the MCP tools take the same
  fields (§5), so nothing an agent learned in L1 is thrown away.
- **Evidence is links buried in prose.** `failed` held "PR 257, job
  118131675020" as text. The distillation needs them as links it can
  count and follow, so the field adds `evidence`, a short list of typed
  links, beside the prose.
- **Names drift.** `general` and `repo` became `factory` and `project`
  within a day; tags were invented per lesson (`parallel-sessions`,
  `ci-flake`, `ci`). The store fixes what must be counted (scope, stage)
  and leaves tags free, normalized by shape (§3).

The issue's first sketch had a `kind` (worked, failed, fix, recommend)
per lesson. The template does better: one lesson carries all four as
fields, since a lesson is usually the four together (what went wrong,
what was done, what to change), and the distillation reads them
together.

## 3. The lesson

| Field | Required | Holds |
|---|---|---|
| `lesson` | yes | One line about the workflow, at most 200 bytes |
| `stage` | yes | One of `pick`, `brief`, `claim`, `run`, `test`, `review`, `merge`, `land`, `cleanup`, `parallel`, `escalate`, `cost`, `tooling` |
| `scope` | yes | `factory` or `project` |
| `worked` | no | What in the workflow went right |
| `failed` | no | What in the workflow went wrong, with evidence: a PR, a CI run, an error line, time lost or tokens spent |
| `fix` | no | What was done |
| `recommend` | yes | The change to the workflow: a rule, a brief line, a check, a tool, a default |
| `tags` | no | Up to 8, each lowercase kebab-case (`ci-flake`, `permissions`, `human-needed`) |
| `evidence` | no (see Q3) | Up to 8 links, each `pr:N`, `run:ID`, `job:ID`, `issue:ID`, `commit:SHA` or an `https` URL |

Recorded by the store, never sent: the issue, the principal, the
session, the time (`wtx.now`, rule 19), and the issue's rev the
handoff or finish left.

- **`stage` is a closed list,** validated by the server, because the
  distillation and Sextant count by it. A value the list does not know is
  refused as `invalid`, with a fix naming the list. The list grows in a
  release, and an older server refuses a newer client's new stage with
  that fix; that is a value, not a field, so it needs no protocol bump.
- **`recommend` is required.** A lesson that changes nothing is not
  worth the distillation's tokens: the memory says "only what would
  change how the next session or the dispatcher works; none is fine".
- **Tags are free** within their shape: they are how a new theme shows
  up before anyone names it. The guide lists the common ones; the
  distillation merges near-duplicates and proposes additions to that
  list, as it proposes any change.
- **Evidence is typed** so that L3 can resolve it: `pr:` and `run:` and
  `job:` against the repository's forge, `issue:` against this project,
  `commit:` against git. A URL is kept as given; Aries fetches nothing.
  An evidence link holds no credentials: the secrets lint reads it, and
  a URL with userinfo is refused.
- **No person's name.** The memory says "the maintainer" or "a
  session"; the store cannot enforce it, and the guide and the tool
  description say it. Who wrote a lesson is the principal recorded with
  it, which the export names (§6) and no count is made by.

## 4. Where it is stored, and the event log

A table of each project's schema, `lessons` (the next migration, rule
9), with `lesson_tags` and `lesson_evidence` beside it, as memories have
`memory_tags`. A lesson is written in the same transaction as the
handoff or finish that carries it, through `Store.write` (rule 5), and
each appends an event, `lesson.create`, targeting the issue
(`wtx.event`, rule 7), so the issue's history shows it and the event log
stays the truth.

Lessons are **append-only**. A lesson is not edited; a better one is
written. The one exception is a redaction (§7).

"Beside the issue's outcome and cost" means a join, never a copy. The
outcome of the issue a lesson was written on (`landed`, `closed`, `open`),
its rework and its cost per landed issue are what
[`aries cost --group-by outcome`](../concepts.md#cost-per-outcome)
already derives from the close and reopen events and the usage records.
The query (§5) and the export (§6) compute them at read time for the
issues they return, for the window asked. Nothing derived is stored,
as rule 7 has it for ready, blocked and digest: a lesson written at
handoff, before its PR lands, would otherwise carry an outcome that is
already out of date.

## 5. Writing and reading

### Writing: handoff and finish

`handoff` and `finish` gain `lessons`, a list of lessons (at most 5 a
call). On the wire (`internal/proto`), each is a structured object with
the fields of §3. The server validates each one and writes all of them
or none, with the handoff or finish: a refused lesson refuses the call,
with a fix naming the lesson's index and its field, so that an agent
never loses its handoff to a typo silently and never records half a
lesson.

The two places a lesson is written are the two the workflow ends on: a
session that opens a pull request hands off with state `done` (the
memory `workflow`, step 5), and read-only work finishes (step 11). A
lesson written by a session that holds no issue (a coordinator between
waves) goes on the issue it is about, through a handoff on that issue
or on an issue it starts for the purpose; there is no lesson without an
issue, since the issue is what joins it to an outcome.

**MCP.** The tool budget (rule 12) is 3,000 estimated tokens for every
tool's schema, and the tools cost about 2,823 today (measured by
`TestTools`, 19 Oct 2026): 177 to spare. `handoff` and `finish` take
`lessons` as **a structured list**, each item an object with the fields
of §3, `stage` and `scope` as enums, so that a harness validates the
shape before the call and an agent never writes a template by hand.
That costs about 250 tokens, and the read (below) about 90, so the
budget rises to **3,200**, as decision 76 raised it to 3,000 for stage
6's tools (Q5, answered: structured, budget raised). `TestTools` holds
it, and its comment says why. Descriptions stay terse: the template's
guidance (what counts, no person's name) lives in the memory `lessons`
and the brief, not in the schema.

The CLI takes the same fields from L1's template text with `--lesson
FILE` (or `-` for stdin), repeatable, on `aries handoff` and `aries
finish`, and refuses one it cannot parse with a fix naming the line,
before anything reaches the server: a person writes the text a session
learned in L1.

### Reading: `aries lessons`

```
aries lessons --since 7d [--until T] [--stage S] [--tag T] [--scope factory|project] [--issue ID] [-n N] [--json]
```

Newest first, 50 by default, at most 500. Each line is the lesson's
date, issue, stage, scope and `lesson`, then `recommend`; `--json`
prints one document (rule 13) with every field and, for each issue, its
outcome in the window (§4). `show` returns an issue's lessons with it,
as a result field (no protocol bump), counted in its budget and cut
first.

**The MCP read** is a `lessons` tool with `since`, `stage`, `tag` and
`scope`, returning at most 20 lessons, each cut to `lesson`,
`recommend`, its issue and stage, within the 2,000-token result cap,
marked `untrusted` as every result is. It costs about 90 tokens of
schema, inside the 3,200 above, and is built in L2b (Q4, answered:
build it). What it is for: a session about to plan a wave, or a
dispatcher writing a brief, can read what the last week's sessions said
about that stage. Prime does not show a project's own lessons: its 1,500
tokens are for the work at hand, and a lesson there would read like an
instruction (§7). Organization lessons, which a person approved, are
the exception (§10).

The read is refused offline, as `list` and `ready` are: a stale answer
about the factory's recent lessons is worse than none, and the cache is
for the work a session holds (design §18.6).

## 6. The export for the distillation

L3 reads a window of lessons with their outcomes, and everything a
lesson holds:

```
aries lessons --since 2026-10-12 --until 2026-10-19 --export > lessons.json
```

One JSON document: the window, then each lesson with every field, its
issue's id, type, labels, epic and outcome in the window (landed,
closed or open; rework tokens; list-price cost; held time over cycle
time; waived items and gates; reopen count), the principal who wrote
it, its kind (agent, person, service), and the session id. The
session id is what joins a lesson to its brief, model and effort once
aries-2qx4mlm2 records them, and to the session's own usage.

- **The principal is named, and counts stay per session** (Q9,
  answered: include the principal). The export is the project's
  own data, read by its members and a scheduled run they start, who see
  the author in the issue's history anyway; naming it lets the
  distillation tell one agent's habit from a pattern across agents. What
  the distillation reports is counted per session, never per person
  (decision 97, Sextant's §4), and its proposals cite lessons by issue
  and session, not by author.
- **Paged, not unbounded.** At most 2,000 lessons a call; a longer window
  gives `next`, which `--after` takes. A week of this project's issues
  is far below one page.
- **Scope is a filter, not a redaction.** `--scope factory` is what L3
  reads for the playbook; `project` lessons are exported only when asked
  for, for a project's own retrospective.

The export is the CLI's alone, as `aries cost` is: it is for a scheduled
run and a person, never an agent's tool call (rule 12).

## 7. Limits, the secrets lint, safetext, and lessons as data

**Limits (rule 18)**, each with a default, a `limits:` setting, a
table-driven test and a row in docs/server.md's Limits table:

| Setting | Default | Caps |
|---|---|---|
| `lessons_per_call` | 5 | lessons on one handoff or finish |
| `lessons_per_issue` | 50 | lessons on one issue, all time |
| `lesson_line_bytes` | 200 | `lesson` |
| `lesson_field_bytes` | 1,000 | each of `worked`, `failed`, `fix`, `recommend` |
| `lesson_tags` | 8 | tags on a lesson, each at most 64 bytes |
| `lesson_evidence` | 8 | evidence links on a lesson, each at most 512 bytes |

A refusal past one is `invalid` with a fix naming the cap and the
field. A lesson is at most about 9 KB, and an issue holds at most 50 of
them, so what a principal can add is bounded by the issues it may write
to, which the issue caps already bound.

**The secrets lint** (`internal/secretscan`) runs on every field and
every evidence link, as it runs on a memory's key, body and tags, and on
the outbox's entries before they are written (a handoff with lessons,
queued offline, is linted when queued, not first at replay). A finding
refuses the whole call with `invalid` and a fix naming the field:
"remove the credential from lesson 2's failed and retry". Lessons are
where an agent pastes an error line, so this is the lint's most likely
catch.

**safetext**: the store refuses controls and bidi characters in every
field, as it does for an issue's text, and every client escapes what it
prints.

**Lessons are untrusted data.** They are written by agents, about agents,
and read by the next agent and by L3's model. Each reader is told so:
MCP results carry `untrusted`; the export's header says it; L3's prompt
quotes lessons as data and never as instructions (its design says how).
A lesson's `recommend` is a proposal for a person to approve, never a
rule anything applies: nothing in Aries reads a lesson and changes
behavior. That is also why prime does not show them.

**Redaction.** A secret the lint missed, or a person's name, is
redacted by a project admin, `aries admin lessons redact ID`, CLI only
(rule 12), which clears the lesson's text and evidence, keeps its
stage, scope and tags, and records `lesson.redact` with who did it. It
is the only change a lesson ever has.

## 8. The protocol

`handoff` and `finish` gain an argument field, `lessons`, and there is a
new op, `lessons`, so this is a protocol change (rule 10). v0.6.0 has not
been tagged (its notes are a draft, and no `v0.6.*` tag exists on 19 Oct
2026), so this change **joins protocol 9**, v0.6.0's, beside `handoff`'s
`ticked` and `waived` and the key proof's binding (the memory
`release-versioning`, rule 3): `Proto = ProtoMax = 9`, `ProtoMin = 8`,
with the test messages combined. If v0.6.0 is tagged before this is
built, it takes 10, which is then the next release's. The maintainer
answered that v0.6.0 waits for L2a and L2b (Q6), so this joins 9.

A protocol 8 client sends no lessons and never calls `lessons`; a
protocol 9 server works without either. A protocol 9 client against an
8 server is already refused at the handshake. The `lessons` op's scope
(`internal/server/scopes.go`) is a read, as `list`'s is; `lessons.redact`
is an admin op.

## 9. L1's comments beside the field

L1's comments are kept, and read into the field once. The recommendation
is an import, not a query that reads comments forever:

1. `aries lessons from-comments --since DATE > lessons.jsonl` reads the
   project's comments that begin `lesson:` and prints one lesson per
   line in the field's shape, mapping `general` to `factory` and `repo`
   to `project`, each with its comment's id, issue, author's session and
   time. A comment it cannot parse is printed as an error line naming
   the comment, never guessed at.
2. A person reads and edits the file: the seven seed lessons have no
   `stage`, which the mapping comment on aries-wf6da6vg supplies.
3. `aries admin lessons import lessons.jsonl` writes them, CLI only, as
   the operator, keeping each one's original time and session, and
   recording `lesson.import`. It is idempotent by the comment's id, so a
   second run adds only what is new.

Until the import runs, nothing reads the comments; after it, L1's
comments stay as they are, and the workflow's step 12 moves to the
field. A query that read both would parse free text on every call, for
a week of data.

## 10. Lessons across the organization

Asked by the maintainer, 19 Oct 2026, after §13 was answered: the
server (`ariesd`) should give a team or an organization the lessons
learned across its projects, so that a new team or project starts from
them rather than relearning them. The maintainer settled its shape the
same day (Q13 to Q15): only curated lessons cross projects, an
organization role reads the raw ones across projects, and a new project
gets the curated ones in prime and the playbook as its seed.

There are three tiers, and each lesson stays in the tier it was written
in until a person moves what it taught to the next:

| Tier | Where | Who reads it |
|---|---|---|
| A project's lessons, `project` scope | The project's schema | The project's members |
| A project's lessons, `factory` scope | The project's schema | The project's members; across projects, the organization role (Q14) |
| Organization lessons: curated, approved | The control store, at the organization's scope | Every project's sessions and members |

**Raw lessons never cross projects** (Q13, answered: curated only;
Q10). A member of one project does not read another project's lessons,
whatever their scope: they hold that project's issue ids, PR numbers and
evidence, which are its members' alone. `project` lessons never leave
their project at all.

**Reading across projects is the organization's.** `aries admin lessons
--all-projects` (with §5's filters and `--export`) is served by `ariesd`
from every project's schema, each lesson labeled with its project,
`factory` scope only. It is CLI only (rule 12), and refused unless the
caller is a server admin or holds the role that writes organization
memory (enterprise review E6; aries-iakie56e), which the distillation's
service principal is given (Q14, answered: the organization role). This
is what the distillation (L3) reads when a server holds several
projects: patterns that recur across teams are the strongest evidence
it can bring. Each cross-project read is recorded as an event in the
control store (`lessons.read_all`), with who read and which projects,
since it shows the role every project's issue ids and evidence (Q17).

**Organization lessons.** What the distillation proposes and a person
with the organization role approves becomes an organization lesson: a
record at the organization's scope key in the control store, beside
organization memory and the server's default workflow (design §21.9),
holding `lesson`, `stage`, `recommend` and tags, the approver and time,
and its provenance: how many lessons, from how many projects and
sessions, over which window, cited by project and lesson id. Its
provenance is readable by the organization role alone; projects see the
lesson, the count of projects behind it, and the date. It is written
by `aries admin lessons approve`, never over MCP, and is the
organization's playbook in Aries. L4's playbook in a repository is the
same text, exported (below).

Organization lessons are built on organization memory (aries-iakie56e):
its scope key, its role and its reach into prime. Whether an organization
lesson is an organization memory of a kind `lesson`, or a table of its
own beside it, was Q16: an organization memory of a kind `lesson`,
with its provenance as a field, so that prime has one ranked list of
organization guidance and one cap; aries-iakie56e's design builds it
so.

**A new project starts from them** (Q15, answered: prime and a playbook
seed):

- **Prime and start.** A project's sessions see organization lessons as
  they see pinned memories: prime shows up to 3, those whose tags match
  the labels of the caller's issues first, then the newest, each cut to
  `lesson` and `recommend`, dropped before the inbox and ready lists to
  fit its 1,500 tokens; `start` shows up to 2 matching its issue. They
  are marked as approved guidance with their date and the number of
  projects behind them, and carry the `untrusted` marker every result
  does. Unlike a raw lesson (§7), an organization lesson was approved by
  a person, which is why it may appear in prime.
- **The playbook as a seed.** A project created on the server already
  takes the server's default workflow, a floor it may add to (design
  §21.9, W5), and organization memory. `aries admin playbook export`
  writes the organization's lessons and its default workflow as files a
  new repository commits (the playbook's form in L4: a workflow file and
  the guidance an agent file points to), and `aries admin projects
  create --playbook DIR` writes them for the new project in one step.
  The repository's copy is the one its agents read before Aries is
  reached, and the one a person reviews in a pull request when the
  organization's playbook moves.

**Not a channel for instructions.** An organization lesson is approved
guidance, not a rule anything applies: the workflow setting is what the
server enforces, and a lesson that should be enforced is proposed as a
change to it. Organization lessons pass the same secrets lint, safetext
and limits as lessons (`org_lessons` 500, `org_lessons_in_prime` 3,
`org_lesson_sources` 200), and approval is recorded as an event in the
control store (`org_lesson.approve`, `org_lesson.retire`), as its other
writes are.

**The protocol.** `admin.lessons`, `admin.lessons.approve` and the
playbook ops are new admin ops, and prime's and start's organization
lessons are result fields. The ops join the protocol of the release
they ship in: they wait for organization memory, so most likely 10,
after v0.6.0 (rule 10, the memory `release-versioning`).

## 11. Briefs

The dispatcher's brief (dispatcher.md §7) gains one item, after the
workflow's: "Before handoff or finish, record lessons in `lessons` (the
memory `lessons` says what counts); none is fine when nothing surprised
you." A coordinator's briefs say the same until the dispatcher exists.

The brief's own record (aries-2qx4mlm2) and the lesson share the
session id, so the export can join a lesson to the brief that started
its session, and L3 can ask whether a brief line changed what sessions
wrote. The dispatcher may count the share of its runs that left a
lesson, which is L1's measure of success, and report it with the run.

## 12. Slices

| Slice | What | Done when |
|---|---|---|
| L2a | The migration, `lessons` on `handoff` and `finish`, the limits, the lint, `lesson.create`, the kept counter of lessons by stage and scope (Q12); protocol 9 | A handoff with lessons records them once; `show` returns them; a lesson the lint flags is refused with a fix |
| L2b | `aries lessons` and the `lessons` op; the structured `lessons` on the MCP `handoff` and `finish`, and the MCP `lessons` read; the CLI's parsing of the template; the schema budget at 3,200 | The query filters by window, stage, tag, scope and issue; the tool budget test passes at 3,200 |
| L2c | The export, with outcomes joined | The export of a window matches `aries cost --group-by outcome` for its issues |
| L2d | `from-comments` and the import; redaction | L1's comments are imported once, idempotently |
| L2e | docs/agents.md, docs/cli.md, docs/server.md's limits; the memories `lessons` and `workflow` step 12 moved to the field; dispatcher.md §7 | docs/agents.md says what to record and who can read it |
| L2f | Lessons across the organization (§10): `aries admin lessons --all-projects`, organization lessons and their approval, prime and start, the playbook export and seed; after organization memory (aries-iakie56e) | A lesson in two projects is read by the organization role and by no member of either; an approved organization lesson appears in a third project's prime; a new project created with `--playbook` gets its files |

L2a and L2b are the issue's test; L2c is what L3 needs first; L2d can
wait for the week of L1's comments to end. L2f waits for organization
memory, and is an issue of its own under the epic.

## 13. Questions for the maintainer

Answered by the maintainer, 19 Oct 2026 (decision 101).

1. **A document of its own.** This design is `docs/design/lessons.md`,
   with a row in docs/design/README.md, rather than a section of
   `aries.md`; L3 and L4 would add sections here. *Recommended:* yes. *Answered 19 Oct 2026:* as recommended.
2. **The fields.** The template's eight fields plus `evidence`, with
   `lesson`, `stage`, `scope` and `recommend` required, and the
   issue's `kind` dropped (§2, §3). *Recommended:* as proposed. *Answered 19 Oct 2026:* as
   recommended.
3. **Evidence required?** The epic says a lesson without evidence does
   not count. Either the store refuses a lesson with `failed` and no
   evidence link, or it keeps it and L3 weighs it at zero.
   *Recommended:* keep it, and weigh it at zero: a refusal at handoff
   costs the whole handoff for a missing link, and some lessons (a
   permission prompt, a time lost) have no link to give. *Answered 19 Oct 2026:* as
   recommended.
4. **The MCP read.** A `lessons` tool spends most of the 177 tokens left
   of the schema budget for a use no session has yet. Either build it in
   L2b, or leave agents the CLI's `--json` until L3 or the dispatcher
   asks for it. *Recommended:* leave it out of L2, and keep the budget
   for the write. *Answered 19 Oct 2026:* build it in L2b (§5), within the
   budget Q5 raises.
5. **The MCP write as the template's text.** `aries mcp` parses the
   template text into the structured lesson, instead of a structured
   schema on two tools that would break the budget. *Recommended:* yes;
   the alternative is raising the budget to 3,200 (decision 76's
   precedent), which the next feature will want too. *Answered 19 Oct 2026:* a
   structured lesson on `handoff` and `finish`, with the budget raised
   to 3,200 (§5). The CLI keeps the template text.
6. **Join protocol 9.** v0.6.0 is not tagged, so this joins 9; if it
   ships first, this takes 10. *Recommended:* join 9, and build L2a
   before v0.6.0 is cut if the release can wait for it. *Answered 19 Oct 2026:* as
   recommended: v0.6.0 waits for L2a and L2b, which join 9 (§8).
7. **L1's comments.** Import once, through a file a person checks, and
   leave the comments as they are (§9). *Recommended:* as proposed.
   *Answered 19 Oct 2026:* as recommended.
8. **Stages a closed list, tags free.** The store refuses an unknown
   stage and accepts any tag of the right shape (§3). *Recommended:* as
   proposed; L3 proposes new stages like any other change. *Answered 19 Oct 2026:* as
   recommended.
9. **No principal in the export.** The export names a principal's kind
   and the session, never the principal (decision 97). *Recommended:*
   yes; `aries lessons` without `--export` shows who wrote each one to
   the project's members, who can see it in the issue's history anyway. *Answered 19 Oct 2026:* include the principal in the export; counts stay per
   session (§6).
10. **Organization memory only through approval.** Raw lessons never
    reach organization memory; only what L3 proposes and the maintainer
    approves does (§10). *Recommended:* yes. *Answered 19 Oct 2026:* as
    recommended.
11. **Redaction by a project admin.** The one change a lesson can have
    (§7). *Recommended:* yes, CLI only. *Answered 19 Oct 2026:* as recommended.
12. **A metric.** A counter of lessons recorded, by stage and scope,
    added to the kept names (rule 2), so that Sextant can show the share
    of issues with a lesson. *Recommended:* yes, in L2a, since names are
    kept from v0.5.0 and adding one later costs the same. *Answered 19 Oct 2026:* as
    recommended.

### Asked after §13 was answered (19 Oct 2026)

13. **What crosses projects.** Curated organization lessons for every
    project, raw `factory` lessons queryable by an organization role,
    or every project's members reading every project's `factory`
    lessons. *Recommended:* the first two together. *Answered 19 Oct
    2026:* curated only: raw lessons never cross to another project's
    members (§10).
14. **Who reads raw lessons across projects.** *Recommended:* server
    admins and the role that writes organization memory, which the
    distillation's service principal holds. *Answered 19 Oct 2026:* as
    recommended.
15. **How a new project starts from them.** *Recommended:* organization
    lessons in prime and start within their caps, and the playbook as a
    seed when the project is created. *Answered 19 Oct 2026:* as
    recommended.
16. **An organization lesson's storage.** An organization memory of a
    kind `lesson`, or a table of its own beside organization memory.
    *Recommended:* an organization memory of a kind `lesson`, with
    provenance as a field, so that prime has one ranked list of
    organization guidance and one cap; settled in aries-iakie56e's
    design. *Answered 19 Oct 2026:* as recommended; aries-iakie56e's
    design builds it so.
17. **The organization role across projects.** Reading every project's
    `factory` lessons gives the role every project's issue ids and
    evidence. *Recommended:* accept it for the role, since it already
    writes what every project reads, and record each cross-project read
    as an event in the control store. *Answered 19 Oct 2026:* as
    recommended.
