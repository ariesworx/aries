# Review: one factory flow or two

Status: review, 20 October 2026 (aries-lm22dgzp, under epic
aries-hzuyf4mt). Nothing here is decided; §6 lists the questions for
the maintainer.

## 1. The question

Agents on this project are started two ways:

- **The coordinator.** A person-driven session in the Claude desktop app
  starts worker sessions with briefs written by hand from the project
  memories `workflow`, `briefs`, `dispatch`, `merges`, `holds` and
  `lessons`. It queues merges, watches the default branch, and closes
  issues once they land. A scheduled pull job on the maintainer's
  machine runs `aries land` after merges.
- **The dispatcher.** `aries dispatch run` (`internal/dispatch`, decision
  96) picks issues itself, builds briefs in code, and launches headless
  `claude -p` under its own allow and deny lists, budget and stops.

The dispatcher's design promises that "Nothing that matters is written
twice" (dispatcher.md:38). This review checks that promise against the
code at `b25f3cf`, the memories as recalled on 20 October 2026, open PR
288, and the events of the first live run. Every finding cites a file
and line, an issue, a comment or an event; where something could not be
verified, it says so.

Method: read `internal/dispatch` (brief.go, select.go, claude.go,
stops.go, config.go, dispatcher.go, doc.go), `internal/outcome`
(capture.go), `.aries/workflow.yaml`, `.claude/settings.json`, the
design documents named in the issue, the six memories, and the issues
under aries-hzuyf4mt and aries-dmqqolg4 with their `brief:`, `lesson:`
and handoff comments. One claim (§3.1) was checked by running the two
parsers against each other in a throwaway test, not committed.

## 2. Where the two paths share one source of truth

These hold for both paths today, because both read the same server
data or the same files.

| What | Source both read | Evidence |
|---|---|---|
| Acceptance items, required checks, close-out lines | The workflow setting, applied from `.aries/workflow.yaml` (decision 95, aries.md:737) | `.aries/workflow.yaml:15-39`; prime shows "workflow rev 1: bug, feature, task: 4 items, landed" to every session |
| Closed means landed | The `landed` check and the closing pass | `.aries/workflow.yaml:23-25`; aries-4zjc7i7m shows "landed passed at b25f3cf" |
| Claims and leases | `start` on the server; the dispatcher claims before launch | dispatcher.go:294-305; dispatcher.md:89-95; `RuleClaimed` skips an issue a Desktop session holds (select.go:122) |
| Model labels and the `human` and `waiting:maintainer` labels | Issue labels | select.go:110-113, 125-137; memory `dispatch` rules 1-2; memory `holds` item 2 |
| The `workflow`, `lessons` and `briefs` memories | `recall` by key | The dispatcher's brief tells the run to recall them (brief.go:64, 71); the coordinator's briefs say the same (brief comment on aries-vhbljxmz, 21:41 UTC) |
| Usage and cost | The `Stop`, `SubagentStop` and `SessionEnd` hooks running `aries usage --hook` | `.claude/settings.json`, which both a Desktop session and `claude -p` load in a worktree; the headless run on aries-nyzudqaq shows usage "claude-haiku-5-5 … cost $0.03" |
| The budget signal | `aries cost --group-by model` for the UTC day | exec.go:115-122 sums Opus output across every session, Desktop ones included |
| Landing | `aries land` from the session `cli`, by the pull job | memory `workflow` step 7; dispatcher.md:212-218 says the dispatcher never runs it |
| The `pgtest/HOST` lock | `scripts/gate.sh --pgtest-lock` | brief.go:69; the coordinator's session for aries-vhbljxmz reports running `gate.sh --quick --pgtest-lock` in its handoff (21:45 UTC). Whether every coordinator brief asks for it was not checked |
| Lessons format | The memory `lessons` | Both paths' runs posted `lesson:` comments in that format: aries-nyzudqaq (21:39, headless), aries-4zjc7i7m (21:19) and aries-vhbljxmz (21:45) (Desktop) |
| The last line of defense on merges | The host's merge queue and branch protection | AGENTS.md, Git; claude.go:57-63 names branch protection as the backstop |

The server-side parts are sound: claims, labels, the workflow setting,
`landed`, usage and cost are one truth that either path, and Bearings
later, reads the same way.

## 3. Where they have diverged

Ranked by risk: first what silently corrupts the learning loop's data,
then what lets the two paths act against each other, then what only
costs effort.

### 3.1 The brief record does not parse, on either path (high)

The memory `briefs` asks that a `brief:` comment's "first line names
harness, model, effort and the session id" but fixes no format.
Three formats exist:

- The dispatcher writes `brief: harness claude-code, model haiku,
  effort default, session UUID` (brief.go:83).
- The coordinator writes `brief: Claude Code desktop / claude-sonnet-5-5
  / default effort / session local_… (name), own worktree` (comments on
  aries-4zjc7i7m, 21:17 UTC, and aries-vhbljxmz, 21:41 UTC).
- `aries outcome` reads `key=value` fields (capture.go:252-263), as its
  test writes them (capture_test.go:108).

Neither path's line has an `=`, so every outcome record gets an empty
harness, model and effort from it. Checked by running `briefs()` on
`BriefComment()`'s output and on the coordinator's line: all three
fields came back empty for both. Outcome-record.md's slice O2 is gated
on "Its runs' records have no harness or brief missing"
(outcome-record.md:450), which no run can meet as built.

Two more breaks in the same record:

- **The hash differs.** The dispatcher's log keeps `sha256(brief)`
  (dispatcher.go:329, brief.go:87-90). `aries outcome` hashes the
  comment's body after the first line with surrounding space trimmed
  (capture.go:265). The brief ends in a newline (brief.go:74), so the
  two hashes of one brief differ (checked in the same test). The log
  cannot be joined to the record by hash.
- **Briefs are matched to runs by order, not by session**
  (capture.go:296-308). aries-nyzudqaq has two `brief:` comments for
  one session at 21:38 UTC (events 3165 and 3167), the second naming
  the model `claude-haiku-5-5` where the first says `haiku`; who
  posted the second could not be verified, since both are under the
  same principal. With positional matching, a second comment for one
  run shifts every later run's brief by one.
- **The session ids differ in kind.** The dispatcher's comment names
  the claim's session (`ARIES_SESSION` equals `--session-id`,
  dispatcher.go:336-351). The coordinator's names the desktop app's
  `local_…` id, which is not the session Aries records: per CLAUDE.md,
  `aries` takes `CLAUDE_CODE_SESSION_ID`. The history output did not
  show the claim's session, so the mismatch on a given issue was not
  confirmed from events.
- **Effort is a constant on the headless path.** Every dispatched run
  records effort `default` (config.go:120, dispatcher.go:325), and no
  flag sets it. The coordinator varies effort per session. Comparisons
  by effort will draw on one path only.

Risk: the learning loop's primary signal (decision 101's revision;
lessons.md §12) reads the brief, harness and model of every run. As
built, every record lists them as missing or wrong, on both paths, and
lessons.md §11's join of a lesson to its brief by session works only
for headless runs.

### 3.2 Permissions come from two unrelated places (high)

- A Desktop session runs with "the user's" permissions
  (dispatcher.md:274): the desktop app's permission mode. The
  repository's `.claude/settings.json` holds hooks only, no allow or
  deny list.
- A headless run gets `DefaultAllowed` and `DefaultDenied`, Go constants
  (claude.go:44-63), under `--permission-mode dontAsk`.

The first live run showed the cost. On aries-nyzudqaq (20 Oct, 21:38
UTC) the run was denied `GOTOOLCHAIN=local GOFLAGS=-mod=readonly go
test …`, because `Bash(go:*)` does not match a command that begins with
an assignment; it took the denial as a block on all Bash and ended with
four files edited, nothing committed and no handoff (its `lesson:`
comment, 21:39 UTC; aries-vhbljxmz's body). The coordinator's sessions
ran the same command forms without a denial. PR 288 (open) sets the
variables in the run's environment, adds a brief line, and widens the
list (`gh pr ready`, `gh pr edit`, `cd`) while denying `find -exec`.

What remains after PR 288:

- The list is still a second, private copy of "what the workflow needs".
  The memory `workflow` step 9 tells every session to run `docker
  compose -p NAME down -v` on a stack it started, and the list has no
  `docker` (claude.go:44-51, and PR 288's version). The `.aries/workflow.yaml`
  close-out lines (lines 27-29) say the same. A headless run told to
  follow the memory "exactly" (brief.go:64) is denied a step of it.
- The list is this repository's (Go, `scripts/gate.sh`,
  `scripts/install-postgres.sh`, `internal/release/keys.go`) compiled
  into a product package. Another project using `aries dispatch` gets
  this repository's toolchain and none of its own.
- Nothing checks the list against the memory or AGENTS.md's commands.
  The design couples them by hand, saying "the list and the memory
  `workflow` change together" (dispatcher.md:471-472), but the memory is
  edited by `remember` and the list by a pull request, and no test joins
  them. aries-vhbljxmz's lesson (21:45 UTC) recommends a preflight that
  does.
- The `pgtest/HOST` lock is taken only if the run obeys its brief. The
  design says the lock "is taken around each run's whole-suite test"
  (dispatcher.md:116-122). As built, the run is told to use
  `gate.sh --pgtest-lock` (dispatcher.md:464-467; brief.go:69), and the
  dispatcher takes no lock itself. A coordinator's session is bound only
  if its brief says so too.

Risk: every edit to the memory `workflow` or AGENTS.md's commands can
silently break headless runs, and only a live run finds out.

### 3.3 The brief: one by reference, one restated by hand (medium)

The dispatcher's brief (brief.go:35-76) quotes the issue as data, then
nine fixed steps that point to the memories by key. The coordinator's
briefs restate the memories in prose: the commit trailer, Auto-fix,
lessons, "never finish and never merge" (the brief comments on
aries-4zjc7i7m and aries-vhbljxmz; this review's own brief too).

- The dispatcher's form drifts less from the memory, since it points to
  it. But four of its steps are rules that exist only in Go: the test
  limits (brief.go:69), no background work (68), re-starting a lapsed
  claim (70), and, with PR 288, bare commands. The coordinator's briefs
  do not carry them, and the memory does not hold them.
- The coordinator's form drifts with each memory revision (`workflow` is
  at rev 9) and with each person's wording.
- **A brief step promises what does not exist.** brief.go:67 tells the
  run that "The dispatcher's sweep sends a conflicted or failing PR back
  to this session". The sweep is not built (doc.go:26-29; aries-i6fndp4h
  open). The coordinator's sweep (memory `workflow` step 8) "sends each
  conflicted or failing one to its session", but a headless run has no
  desktop session to receive a message, and step 8 does not say who
  takes a dispatcher PR. Until the sweep ships, a headless run's failing
  PR has no owner.
- **Brief changes skip the learning loop's rules.** lessons.md §12
  (lines 433-442) freezes the memory `workflow` for a rule's window and
  randomizes brief-text rules at the dispatcher. A change to brief.go's
  fixed steps (PR 288 adds one) is a brief-text rule shipped by code
  review, outside both, and only the headless half of the runs gets it.
  The two populations' briefs then differ by more than the rule under
  test.

Risk: the comparison of briefs that lessons.md §11-12 depends on is
confounded by the path a run came from.

### 3.4 Picking: the same rules, coded once and applied once by judgment (medium)

The dispatcher codes the memory `dispatch` rules 1 and 2 and more
(select.go:98-138, filter.go:74-90). Where they part:

| Rule | Dispatcher | Coordinator | Evidence |
|---|---|---|---|
| Model labels | A fixed map of three (select.go:83-87); anything else is "no model" | Whatever the label says | This issue carries `model:fable`, which neither the memory `dispatch` rule 1 nor the map names |
| Release signing | Substring match on four words (select.go:91, 141-149) | Judgment | aries-x5zjzld7: `keys.go` matches `internal/server/keys.go` |
| A run that failed before | Skipped until `--clear` (dispatcher.go:275-288), from its local state file | Not known to it | The state file is the dispatcher's alone (dispatcher.md:228-236) |
| Epics | Never run (select.go:108) | The memory `merges` condition (2) reads an epic's approval gate | — |
| Order | `ready`'s order, up to 500 (config.go:37-41, after PR 287) | "Fill free slots from the top of `ready`" (memory `dispatch` rule 1) | aries-4zjc7i7m: the dispatcher read 20 and missed two matches |
| Pace | `dispatch/runs`, capacity 4, its runs only (config.go:52; claude.go:21) | "at most 4 sessions" by count (memory `dispatch` rule 1) | The memory names neither lock; Desktop sessions take no slot |
| Pause | `dispatch/pause` (stops.go:41-42) | Not in any memory | — |

The last two matter most. Rule 1's limit of four exists because the
machine's Postgres runs out of shared memory past it (memory `dispatch`
rule 1; dispatcher.md:111-123). With both paths running, four
dispatcher runs and four Desktop sessions can run at once, since
neither counts the other. `pgtest/HOST` still bounds whole-suite tests
to two, which caps the damage, but only where the brief asks for it.
And `aries dispatch` pause stops only the dispatcher.

Risk: the two paths race for the same machine, and a person who pauses
"dispatch" has paused half the factory.

### 3.5 Merges, holds and release (medium, rising)

- The coordinator queues under rule 4 (memory `dispatch`) and, since 20
  October, the wider memory `merges`, whose condition (4) requires that
  "a session the maintainer started opened it". A headless run's PR
  never qualifies for the wider rule, by construction.
- The dispatcher queues nothing yet (doc.go:26-29). Its design queues
  under rule 4 alone (dispatcher.md:202-211), and the memory `merges`
  keeps it there "until its live trial (aries-5ahrfm4h) is read".
- Holds: the coordinator marks a held PR draft (memory `holds` item 1).
  dispatcher.md §8 does not mention drafts; the merge queue refuses a
  draft, so this holds for now by the host, not by the dispatcher.
- Both rule sets are prose in memories, applied by a model reading
  them. The planned classifier (aries-i6fndp4h item 1;
  aries-ssgn3lve's research) codes rule 4 for the dispatcher alone.

- Bearings' plan says otherwise again. Its merge train queues "until
  the server runs" and "a human merges" (bearings.md:194-198), and its
  decision 17 is "Unattended work never merges into `main`"
  (bearings.md:1462). Rule 4 lets the coordinator, and later the
  dispatcher, queue PRs into `main`'s merge queue unattended. Bearings
  also resolves conflicts "by rebasing" (bearings.md:174-176), while the
  memory `workflow` step 5 says to merge `origin/main` and never rebase
  a pushed branch.

Risk: the day the sweep ships, rule 4 exists as code in one path and
as prose in the other, and the wider `merges` rule as prose only. The
holds memory records the failure this invites: PRs 277 and 278 merged
after a hold agreed in conversation and had to be reverted.

### 3.6 Failure handling and notices (medium)

| Failure | Dispatcher | Coordinator |
|---|---|---|
| Two CI failures in a row | Counts its own runs' PRs only (stops.go:52-59; dispatcher.go:419-441) | Memory `dispatch` rule 3, by judgment, over what it sees |
| Budget | Opus output past 1.3M (config.go:61; stops.go:39-40) | Same rule, read by hand |
| Auth or billing failure | Stops (claude.go:204-245; stops.go:63-67) | Not applicable: the app's sign-in |
| A run past its time | 90 minutes, then interrupt (config.go:59; dispatcher.go:498-518) | None |
| A run that ended without a handoff | Releases its claim (dispatcher.go:411-414); the issue stays `failed-before` | Auto-fix wakes the session |
| Where a stop is reported | Its terminal and `log.jsonl` (dispatcher.go:210-227) | The conversation |

The standing-issue notices of decision 96 are not built (aries-i6fndp4h),
so a dispatcher stop reaches no one who is not watching its terminal.
aries-ierget3t shows its own status can be wrong after a run exits.
The two CI-failure counters can each be at one while the factory has
had two failures in a row.

Risk: each path stops on half the evidence.

### 3.7 Lessons and outcome records (low to medium)

- Lessons: both paths follow the memory `lessons` (§2). The headless
  run's lesson on aries-nyzudqaq was posted though the run could not
  hand off, which is the format working.
- Outcome records: `aries outcome` is CLI only (outcome-record.md:449).
  O1 puts "the coordinator's sweep step in the memory `workflow`", but
  the memory (rev 9) has no outcome step, and outcome-record.md lists it
  as "Not yet built" (lines 597-599). Who posts `outcome:` comments
  today, and how often, was not verified. O2 gives it to the
  dispatcher's sweep (outcome-record.md:450), which does not exist. No
  run captures its own outcome, rightly, since it is captured after the
  outcome.
- The budget is in Opus tokens (config.go:61). The headless runs bill
  per token on an API key, the Desktop sessions on the plan's limits
  (dispatcher.md:291-297; aries-5ahrfm4h notes). `aries cost` adds the
  two at list prices as one figure; the hard stop on the headless half
  is the provider's spend limit and `--max-budget-usd` (decision 99,
  aries.md:741). Haiku's long-prompt rates are not modeled yet
  (aries-itlyj3yw), so the trial's Haiku cost reads low.

### 3.8 What each keeps privately

| Kept | Coordinator | Dispatcher |
|---|---|---|
| In Aries | Memories (rules as prose), `brief:` and `lesson:` comments, claims | Claims, `brief:` comments, releases with handoff notes |
| In code | — | Skip rules, signing words, model map, brief steps, allow and deny lists, caps and timers (select.go, brief.go, claude.go, config.go) |
| On the machine | Its conversation; the shared notes folder; the desktop app's session list; agreements made in conversation (memory `holds` item 1 cites one) | State file and `log.jsonl` (dispatcher.md:226-236), run logs, flags given at the command line |
| Neither | The pull job: a hand-written script on a 10-minute schedule outside the repository (aries-yryu7pv6, open). Its notes record that the default branch was twice fast-forwarded outside the job, so `land` did not run (19 and 20 Oct) | |

Bearings would add a third place: its plan puts dispatch policy, pools,
budgets and merge mode in `bearings.toml` and a policy repository
(bearings.md:1297-1330), with the policy in force applied to ariesd by
`policy.apply` (bearings.md:1160).

## 4. How long the dispatcher lives

Decision 96 removes the loop "when Bearings' B0 takes it over"
(aries.md:738). B0 is the supervisor, typed state, the provider
allowlist and one provider (bearings.md:1390); dispatch policy is B2
and the merge train B3 (bearings.md:1391-1393). Bearings is 1.x work
(bearings.md:1406) and "still waits for v1.0.0" (decision 100,
aries.md:742), which decision 93 holds "as long as possible". So the
dispatcher, and with it two paths, will run for a long time, and B0
alone would not take over picking, briefs or merges. bearings.md never
mentions the dispatcher or briefs, so nothing in Bearings' plan yet
says which of the dispatcher's rules it inherits. Whatever the two
paths share must therefore live where both, and Bearings after them,
read it: in Aries.

## 5. Convergence plan, ranked

Each step is small, and the first four need no protocol change.

1. **One brief record format, now.** Fix the first line as `key=value`
   fields (`harness=`, `model=`, `effort=`, `session=` the claim's
   session), have `BriefComment` write it, the memory `briefs` state
   it, and `aries outcome` match briefs to runs by session rather than
   by order, with one hash rule shared by the log and the record.
   Backfill is a reparse. This fixes §3.1 and unblocks O2 and
   lessons.md §11's join. It precedes aries-2qx4mlm2, whose stored
   brief then takes the same fields.
2. **One pace and one pause.** The memory `dispatch` says a Desktop
   session takes a `dispatch/runs` slot for its life (`aries lock run`
   or an acquire at start) and that the coordinator starts nothing while
   `dispatch/pause` is held. A coordinator can do this with today's
   locks. Later the lock is renamed to something both paths own. Fixes
   §3.4's race.
3. **One picker.** The coordinator picks with `aries dispatch run
   --dry-run --once` (after aries-nyzudqaq makes a dry run write
   nothing) and starts what it says it would, or says why not. Judge
   becomes the single statement of rules 1 and 2. The model labels in
   use (`model:fable` among them) are listed once, in the map or as
   server data, and the signing rule is fixed (aries-x5zjzld7).
4. **One permission list, as a file in the repository.** Move
   `DefaultAllowed` and `DefaultDenied` out of Go into a project file
   (beside `.aries/workflow.yaml`, or a section of it), which the
   dispatcher reads and `aries setup claude-code` writes into
   `.claude/settings.json` as the permissions for Desktop sessions too.
   Add the preflight aries-vhbljxmz's lesson proposes: every command the
   memory `workflow` and the gate name is checked against the list
   before a run starts. Fixes §3.2 and makes the dispatcher usable by
   another project.
5. **The brief's fixed steps as project data.** The dispatcher's nine
   steps, and the rules that today live only in Go or only in a
   coordinator's prose, move into a brief template the project keeps
   (a section of the workflow setting, applied by an admin and
   versioned by `workflow.set`, or a memory at first). `aries dispatch
   brief ID` renders it for the coordinator to paste, so both paths send
   the same text and the record's hash identifies the template version.
   This is what lessons.md §12's randomization needs, and what L4
   (aries-bbpl7hmp) calls "the dispatcher's brief template and dispatch
   rules as files rather than this repository's memories".
6. **Name an owner for every PR, before the sweep.** Until aries-i6fndp4h
   ships, either brief.go:67 stops promising a sweep, or the memory
   `workflow` step 8 says the coordinator starts a session on a
   headless run's failing PR. Then build the sweep with its notices on
   a standing issue, so a dispatcher stop reaches the coordinator
   through `prime` and the inbox, and both CI-failure counters read the
   same PRs.
7. **Merge rules as one classifier.** The rule-4 classifier the sweep
   needs is built as a command both paths call (beside `aries prs
   check`), with the memory `merges`'s conditions added as further
   rules that a setting enables, and the draft check of the memory
   `holds` among them.
8. **The pull job in the product** (aries-yryu7pv6), running `land`
   whenever commits are unreported, and `aries prs check`. The outcome
   capture rides on the same job or on the closing pass (O3), so it no
   longer depends on a sweep step that neither memory has.
9. **Then Bearings.** B0 adopts what steps 1-8 put in Aries: the
   template, the permission file, the classifier and the locks. Its
   `bearings.toml` points to them rather than restating them, so
   policy has one home when B2 and B3 arrive.

Steps 1, 2 and 6 are memory edits or small code changes and can follow
PR 288 before the trial resumes (aries-5ahrfm4h). Steps 3-5 and 7 are
best done before the trial widens past Haiku, since widening is when
the two paths run side by side at scale.

## 6. Questions for the maintainer

None is decided here.

1. Is the coordinator path meant to stay as a peer of the dispatcher,
   or to shrink to supervision (starting only what the dispatcher
   cannot, such as Desktop-only work) once the trial is read?
2. Should Desktop sessions count against `dispatch/runs` and obey
   `dispatch/pause`, so that "at most four" and "pause" mean the whole
   factory?
3. Where should the brief template and the permission list live: the
   workflow setting (admin-applied, versioned on the server), a file in
   the repository, or a memory?
4. Should coordinator briefs be rendered from the same template
   (`aries dispatch brief ID`) instead of written by hand, accepting
   less tailoring per issue?
5. Which model labels are valid, and is `model:fable` one the
   dispatcher should run?
6. Until the sweep ships, who owns a headless run's conflicted or
   failing PR: the coordinator, or no one (and the brief says so)?
7. Should the budget become dollars across all models, and should the
   Desktop sessions' plan usage count toward the same cap as the API
   key's?
8. After the trial, should the memory `merges`'s wider rule apply to
   PRs that headless runs open?
9. Should one CI-failure count cover both paths' PRs, so that two
   failures in a row stop both?
10. Is a brief-text change in brief.go a rule change under lessons.md
    §12 (pre-registered, one at a time), or a fix that ships by review?
11. Bearings' decision 17 and its train ("a human merges") conflict with
    rule 4's unattended queueing into `main`. Which one should the
    dispatcher's sweep follow, and should bearings.md say which of the
    dispatcher's rules B0 to B3 inherit?
