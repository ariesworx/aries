# Review: the `aries` CLI's commands and options

Review, 22 October 2026, for issue aries-ia7oob4y. The maintainer asked:
"Review the aries CLI and see if there are opportunities to streamline
the tools options. I feel like this tool is getting very complicated."

This document inventories every command, subcommand and flag of `aries`
as of `main` at e2eda2a, says where it has grown harder than it needs to
be, and ranks proposals by what each saves a person against what it
breaks, with how each would be staged. The questions it raised were
answered by the maintainer the same day (§5, Decisions), and §6 cuts
the accepted proposals into two slices. **Nothing in it is built yet.**

It follows [surface-review.md](surface-review.md) (15 October 2026),
whose §1 reviewed the same surface for what v1.0.0 would freeze, and
whose question 4 (CLI verbs and nouns) the maintainer accepted and the
code has since made: `dep remove`, `label remove`, `admin price` and
`admin plan` in the singular, `aries hours` for `aries log`, `start
--new-branch`, `cost --group-by`, `gate add --issue`. Those are not
reopened here. It also follows the help redesign of aries-3klzojmg
(#237), which grouped `aries help` by task and gave each command its
flags and examples; this review proposes against that design only where
it says so, and keeps its tests' rules (every command under one
heading, two or three examples each, 80 columns).

## 1. What was read

- `aries help`, `aries help COMMAND` for all 53 commands, and
  `aries help admin GROUP` for the twelve admin groups, from a binary
  built from this commit (`go build ./cmd/aries`).
- `internal/cli`: the command table (`cli.go`, `commands` in `init`),
  the help (`help.go`), each command's flag set, and the usage strings.
- [docs/cli.md](../cli.md), [docs/ci.md](../ci.md) and
  [docs/agents.md](../agents.md), for what the guides promise and what
  scripts, CI workflows and hooks call.
- `internal/agentsetup` (what `aries setup` writes into a harness's
  files), `internal/dispatch/exec.go` (what the dispatcher runs),
  `plugins/aries-ops` (what the operations skill tells an operator to
  run), and `.github/workflows`.
- [AGENTS.md](../../AGENTS.md), rules 12 (what is CLI only) and 13
  (output stays terse).

A finding cites the file, the help text or the command line it was
seen on. Where a line number is given it is this commit's.

## 2. Inventory

### 2.1 Size

| What | Count | Where |
|---|---|---|
| Commands `aries help` lists | 53, under eight headings | `internal/cli/help.go:29-97` (`helpGroups`) |
| Hidden aliases | `log` (for `hours`); `dep rm`, `label rm`; `gate ls`, `lock ls` | `cli.go:203` (`aliases`); `commands.go:697`, `:732`; `gate.go:37`; `lock.go:55` |
| Subcommands of the groups | `gate` 6, `lock` 6, `dispatch` 5 (`key` has 2 more), `dep` 2, `label` 2, `outbox` 3, `cache` 1, `workflow` 1, `hours` 3 forms, `login` 2 forms | the usage constants in each file |
| `admin` groups and subcommands | 12 groups, 40 subcommands: `price` 2, `plan` 2, `project` 4, `principal` 7, `member` 3, `key` 4, `invite` 3, `identity` 2, `signin` 2, `trust` 7, `workflow` 3, `status` 1 | `admin.go:29`, `:204`; `admin_*.go`; `cost.go:226`; `hours.go:125`; `workflow.go:24` |
| `setup` targets | 8 agents (`claude-code`, `claude-desktop`, `codex`, `cursor`, `gemini`, `jetbrains`, `junie`, `vscode`), `--all`, and two that are no agent: `post-merge`, `pull-job` | `internal/agentsetup/agentsetup.go:92-135`; `setup.go:27` |
| Flags, as help lists them | 149 lines across the 53 commands' help, plus 52 across the admin groups' | `aries help COMMAND`, counted |
| Global flags | `-C DIR`, `--json`, `--no-queue`, and `-h`, `--version`, which the help does not list | `cli.go:406-432` (`globals`) |
| Environment variables | 13 rows | `docs/cli.md:277-296` |
| Exit codes | 0, 1, 2, 3, and `lock run`'s pass-through | `docs/cli.md:1084-1137` |

For comparison, the MCP server offers agents 24 tools
(`internal/mcpserver`), and AGENTS.md rule 12 keeps it that way.

### 2.2 Every command

The columns: who runs it in practice (a person at a terminal, an agent
through MCP or the CLI, a hook or a job that `aries setup` installed, a
CI job, an admin, or a script), and what else calls it, since that is
what a rename breaks first. "Stable" is this review's reading of the
docs: `docs/cli.md:487-495` says the dispatcher is "internal and
experimental: Bearings will replace it, and the v1.0.0 freeze does not
keep it", and `docs/cli.md:588` marks `outcome` the same.

| Command | Flags (help) | Who | Also called by | State |
|---|---|---|---|---|
| `create` | 16 | person, agent, script | | stable |
| `show` | 1 | person, agent, dispatcher (`exec.go`) | | stable |
| `list` | 9 | person, agent, dispatcher | | stable |
| `ready` | 1 | person, agent, dispatcher | | stable |
| `blocked` | 1 | person, agent | | stable |
| `update` | 16 | person, agent | | stable |
| `close` | 3 | person, agent; `--force` admins | | stable |
| `reopen` | 1 | person, agent | | stable |
| `dep` | 1 | person, agent | `docs/ci.md:78` | stable |
| `label` | 0 | person, agent | | stable |
| `comment` | 0 | person, agent, dispatcher | | stable |
| `comments` | 1 | person, agent | | stable |
| `history` | 1 | person, agent | | stable |
| `prime` | 1 | agent; `--hook` by every harness's session-start hook | `agentsetup` (18 call sites of `aries prime --hook`) | stable |
| `start` | 4 | person, agent, dispatcher | `docs/ci.md:92` | stable |
| `finish` | 11 | agent, person | | stable |
| `accept` | 2 | agent, person | | stable |
| `handoff` | 7 | agent, person, dispatcher (`exec.go:103`) | | stable |
| `away` | 0 | person only (session `cli`) | | stable |
| `lock` | 0 shown (8 in code) | script, CI, person; dispatcher (`lock run dispatch/runs`) | `docs/cli.md:454-485`; ops skill | stable |
| `dispatch` | 0 shown (13 in code) | person, on their own machine | its own state file | experimental |
| `inbox` | 4 | person, agent, dispatcher | | stable |
| `watch` | 1 | script, dispatcher (`exec.go:168`), `tui` | | stable |
| `who` | 2 | person, agent | | stable |
| `digest` | 3 | person, agent | | stable |
| `tui` | 0 | person | | stable |
| `remember`, `recall`, `memories`, `forget`, `pin`, `unpin` | 5, 4, 2, 2, 1, 1 | person, agent | | stable |
| `cost` | 4 | person, admin; dispatcher (the budget check) | | stable |
| `hours` | 6 | person only | | stable |
| `usage` | 1 | hook only: Claude Code's Stop, SubagentStop, SessionEnd | `agentsetup` (7 sites) | stable |
| `outcome` | 4 | person, coordinator | | experimental |
| `gate` | 0 shown (16 in code) | agent (`add`, `list`); person (`pass`, `rearm`, `waive`); CI (`pass`, `fail --check`) | `docs/ci.md:72-74`, `:211`, `:216`, `:289-305`, `:375`, `:532` | stable |
| `land` | 4 | person, after a pull; `--hook` by git's post-merge hook | `docs/ci.md:397-444` | stable |
| `pull-job` | 4 | the scheduled job only | the launchd, systemd or Task Scheduler entry `setup pull-job --write` makes | stable |
| `workflow` | 0 | any member, CI | | stable |
| `outbox` | 2 | person | | stable |
| `cache` | 0 | person | | stable |
| `conflicts` | 2 | person | | stable |
| `resolve` | 4 | person | | stable |
| `setup` | 8 | person, operator | ops skill (`aries setup AGENT`, `--global --key-file`) | stable |
| `login` | 3 | person; `--workload` by a CI job | `docs/ci.md:222`, `:230`; ops skill | stable |
| `logout` | 0 | person | `docs/ci.md` (twice) | stable |
| `whoami` | 0 | person, operator | ops skill (3 sites) | stable |
| `mcp` | 0 | the harness, from its MCP config | `agentsetup` (every agent's entry) | stable |
| `guard` | 4 | hook (`--hook`, Claude Code's PreToolUse); person (`--check`) | `agentsetup`; the dispatcher's `--settings` | decided 21 Oct, built in #315 |
| `upgrade` | 2 | person, operator | ops skill | stable |
| `version` | 0 | person, script | ops skill | stable |
| `admin` | 52 across 12 groups | admins | `docs/ci.md` (`admin trust`, `admin key`, `admin principal`); ops skill (`admin project`, `admin principal list`, `admin status`) | stable |

Of the 53, by who runs them: 28 are the everyday work of a person or an
agent on issues, claims and memories (the Issues and Memory headings and
the work loop of Coordination); 11 are a person's alone (`away`,
`hours`, `tui`, `outbox`, `cache`, `conflicts`, `resolve`, `login`,
`logout`, `whoami`, `upgrade`); 5 exist to be run by a program
(`mcp`, `usage`, `guard --hook`, `pull-job`, `land --hook`); 2 are
experimental; and `admin` holds 40 subcommands for admins. A person who
runs `aries help` to find out what to do next sees all 53 on one
screen of 78 lines.

## 3. Findings

### 3.1 Overlapping commands

**F1. Two ways to end work, and three to settle acceptance items.**
`finish ID` is `close` plus `handoff` plus `accept --tick`/`--waive`
plus `create --discovered`, in one write (`cli.go`, the `finish`
entry: 11 flags; `docs/cli.md:406`). `accept ID N...` ticks, unticks or
waives the same items by hand (`docs/cli.md:407`), and `finish --tick`
and `--waive N=REASON` do it again with another syntax (`N,N` and
`N=REASON` against `accept`'s positional `N...` and `--waive REASON`).
`handoff --release` returns an issue to open with a note, which is what
`finish --state partial` would mean if `finish` did not close. The
overlap is by design (one write per step, decision in
[aries.md](aries.md) §7 As built) and each form is used, but a person
learning the CLI meets three spellings of "tick item 2" and two of
"stop working on this".

**F2. `memories` is `recall` without a query.** `recall [TEXT...]
[--key K] [--tag T] [--scope S] [-n N]` with no TEXT lists "pinned then
newest first" (`aries help recall`), and `memories [--scope S] [-n N]`
lists "the memories you can see, pinned then newest first" (`aries help
memories`). The difference is that `memories` takes no filter. MCP has
`recall` and no `memories` tool.

**F3. Four entry points for landing on a server CI cannot reach.**
`land`, `land --hook`, `pull-job --run`, `setup post-merge` and `setup
pull-job` all serve one feature ([docs/ci.md:397-448](../ci.md)):
report `landed` for the default branch's commits. `pull-job --run` is
`git pull` then `land`, as its summary says ("one run of the job
`setup pull-job` schedules: pull the default branch, then report
landed"), and takes `--branch` as `land` does, plus `--remote` and
`--key-file`. Two of the five are commands a person is told never to
type (`land --hook`, `pull-job --run`: "pull-job is the job's own
command", its usage error says), yet they are listed under Gates beside
`gate`.

**F4. `gate pass` is two protocols under one verb.** `gate pass ID` is a
person's act (session `cli` only; `docs/cli.md:442`), and `gate pass
--check NAME --from-branch` is a CI job's report (session `ci`;
`:443`), with ten flags that the first form refuses. `gate fail` has
only the CI form, `gate waive` and `gate rearm` only the person's. The
usage block has six lines for it (`gate.go:16-20`), and the usage-error
fix line that every mistake prints is 403 characters (`aries gate`,
measured).

**F5. `workflow check FILE` and `admin workflow diff FILE` compare the
same two documents.** One exits 1 and lists what differs
(`docs/cli.md:446`); the other prints `+`/`-` lines (`:617`). `workflow`
has that one subcommand (`workflow.go:26`), and `admin workflow` has
`show`, `set` and `diff`. A member looking for the project's workflow
finds half of it under `admin`.

**F6. Dry runs are spelled three ways.** `land --dry-run`, `outcome
--dry-run` and `dispatch run --dry-run`; `upgrade --check` and `setup
--check`; and `setup AGENT` with no flag, which prints what `--write`
would do (`aries help setup`: "See what setting Claude Code up would
change: aries setup claude-code"). `setup --check` is a third thing
again: "fail unless everything is in place". `guard --check` is a fourth
(validate the policy and judge commands). `--check` thus means a dry
run, a verification, a policy check and, in `gate`, a CI check's name.

**F7. Paging is spelled two ways.** `list --cursor C` (an opaque cursor
from the `more:` line) against `conflicts --after N`, `admin * list
--after NAME` and `admin status --after NAME`. surface-review.md §1.4
kept both; they stay different mechanisms, but a script author meets
both.

**F8. `watch` and `tui` read the same stream.** `tui` is `watch
--events` drawn on a terminal (`docs/cli.md:659-663`). Both are a
person's; the overlap is fine, and is noted only because `watch` is
listed under Coordination with a summary that reads as if it were an
agent's tool.

### 3.2 Flags that mean the same thing under different names, and the same name for different things

**F9. `--key` and `--key-file` are opposites.** `admin principal add
--key FILE` and `admin key add NAME FILE` take a *public* key to add to
the server (`docs/cli.md:619-620`); `setup --key-file PATH` and
`pull-job --key-file PATH` name the *private* key file an agent or job
signs in with (`ARIES_KEY_FILE`; `aries help setup`). A person who has
just added bob's key with `--key bob.pub` may well write `--key` where
`--key-file` is meant, and the error will not say why.

**F10. The person on the other side of an act has five names.** `--to P`
(`handoff`, `finish`: the principal it goes to), `--assignee A`
(`create`, `update`, `list`), `--by PRINCIPAL` (`digest`, `hours`: a
filter), `--principal P` (`outbox`, `admin plan set`, `admin trust add`),
`--approver P` (`gate add`), and `--owner NAME` (`create`, `update`, and
`admin principal add`, where it is a service's owner). `--by` and
`--principal` both filter a list by a principal. surface-review.md §1.4
left `--by` as "by this principal everywhere" after `cost --group-by`,
and `outbox --principal` is the one filter that does not follow.

**F11. `--all` has four meanings.** `list --all` includes closed issues,
`inbox --all` read items, `gate list --all` passed and waived gates, and
`setup --all` every supported agent. The first three are "include the
finished"; the fourth is a target. `aries setup --all --check`
reads naturally only once you know.

**F12. `--issue ID` is a filter, a link and a subject.** `hours --issue
ID` filters a list, `remember --issue ID` links a memory, `gate add
--kind ci --issue ID` names the issue a check runs on, and everywhere
else the issue is the positional `ID`. `gate add --blocks ID` is the
issue the gate holds, so `gate add --kind ci --check build --issue
ar-a1b2 --blocks ar-c3d4` (`docs/ci.md:72`) names two issues by two
flags that both read as "the issue".

**F13. Compare-and-swap tokens have three names.** `--rev N` on
`update`, `close`, `reopen`, `forget`, `remember` and `admin workflow
set`; `--epoch N` on `finish` and `handoff` (the claim's epoch); and the
positional `EPOCH` of `lock renew|release`. Each names a different
counter, so the names are right; the finding is that `finish --epoch`
and `handoff --epoch` are the only two flags that let a *caller* guard a
claim, and nothing in the guides says when a person would use them.

**F14. `--worktree DIR` creates in `start` and records in `finish`,
`handoff` and `dispatch brief`.** `start --worktree DIR` makes a
worktree; `finish --worktree DIR` says which one the work is in. Same
flag, one acts and one reports.

**F15. `--tag` and `--label` are repeatable in one command and single
in another.** `remember --tag T...` repeats (`memory.go`), `recall --tag
T` takes one; `create --label L...` and `list --label L...` repeat,
`digest --label L` takes one. surface-review.md §1.5 asked for the single
forms to be made repeatable; they still are not.

**F16. `--json` goes anywhere except in front of a group's action.**
The help says "--json and --no-queue may also follow the command"
(`aries help`, last block), and `aries list --json --no-queue` works,
but `aries gate --json list` fails with `unknown gate action "--json"`
(`gate.go:28-40`: the action is read before any flag), while `aries gate
list --json` works. `lock` and `dispatch` are the same; `admin` finds
its group anywhere.

**F17. `--for` is a lease in four places and `--every` an interval in
one**; `--since` takes a duration, date or time in `digest`, `cost` and
`outcome`, and a duration alone in `who`. Consistent enough; listed so
the inventory is complete.

### 3.3 Options nobody needs at the default

**F18. `create` and `update` show 16 flags each, and their usage lines
name 10.** `aries help create` lists `--acceptance`, `--design`,
`--notes`, `--owner`, `--status`, `--assignee`, `--priority` and `--type`
that the usage line (`cli.go`, the `create` entry) leaves out, so the
flags block is where a person first meets them. Four of the 16 are the
long text fields that MCP agents fill (`body`, `design`, `acceptance`,
`notes`); a person at a terminal writes `--body -` and little else.

**F19. `create --id` is meant to be hidden and is not.** The comment at
`commands.go:108-110` says "Hidden: no usage line or guide names
`--id`, which only a script that imports issues needs; it is not part
of 1.0's surface (decision 88)", but `flagLines` (`help.go:509`)
hides only `json` and `no-queue`, so `aries help create` prints
`--id TEXT  issue id (default: generated)`. surface-review.md question 8
decided to hide it; the help redesign undid that without meaning to.

**F20. `finish` carries the handoff's whole field set.** `--branch`,
`--worktree`, `--to`, `--state`, `--next` and `--epoch` are `handoff`'s
flags repeated on `finish` (`cli.go`, the two entries), so `finish` has
11 flags. `--state done|partial|blocked` on a command that closes the
issue is odd on its face: `docs/cli.md:406` explains it as the handoff
note's state, but the help line does not.

**F21. `cost --since` is required; `digest --since` defaults to 24h.**
`aries cost` with no flag is a usage error (`cost.go:19`, the usage
line `--since 7d|DATE|TIME` is mandatory); `digest` defaults. The
dispatcher calls `cost` every wake for the UTC day, so a default would
not change it, and surface-review.md question 12 asked for a default on
the MCP `cost` tool before removing it.

**F22. Rarely needed flags listed beside everyday ones.** `start
--take` (a second session of yours), `outbox --host-key FINGERPRINT`
and `--principal P` (a machine that queued as several principals, or
under a host key the server had before), `setup --command PATH`,
`gate pass --run N` (defaults to the CI variables), `pull-job
--key-file` (repeats `setup pull-job --key-file`), `login --provider`.
None is wrong; each is a line the common case reads past.

**F23. `admin price set` needs six rates on the command line, or
twelve.** `aries help admin price` lists 12 flags of which 5 are
required and 6 more "all together" (`docs/cli.md:615`). A person sets
prices a few times a year and will copy the line from the guide.

### 3.4 Admin and person-only acts mixed with everyday ones

**F24. The Gates heading holds one agent command and three that are
not.** `gate` (agents add and list, people pass; `AGENTS.md` rule 12),
`land` (a person's machine), `pull-job` (a job) and `workflow` (a
member or CI). The heading reads as if gates were the theme; the theme
is "a server CI cannot reach".

**F25. Setup holds the identity commands, the hooks and the upgrade.**
`setup`, `login`, `logout`, `whoami`, `mcp`, `guard`, `upgrade`,
`version` (`help.go:86-94`). `mcp` and `guard --hook` are run by a
program; `login`, `logout` and `whoami` are a person's identity, which
is a theme of its own in `docs/cli.md` ("Sign in", `:85-168`).

**F26. Cost and usage holds a hook and an experiment.** `cost` and
`hours` are reports and entries; `usage` is Claude Code's hook, which
refuses to run without `--hook` ("usage runs from an agent's hooks: give
--hook"), and `outcome` is experimental (`help.go:68-73`).

**F27. Person-only acts are not marked as such in help.** `away` is
refused from an agent's session (`docs/cli.md:409`), `gate pass|rearm`
too (`:442`), `hours` is "your hours", `resolve` and `close --force`
are a person's or an admin's. Only `close --force` says "admins only"
in its one line. An agent reading `aries help` cannot tell which lines
are not for it; the MCP tool list tells it, by leaving them out.

### 3.5 Experimental commands beside stable ones

**F28. `dispatch` is the 8th line of Coordination, between `lock` and
`inbox`**, and `outcome` the 4th of Cost and usage (`help.go:53`, `:72`).
Both say "experimental" in their one line, and `docs/cli.md:487-495`
says the v1.0.0 freeze does not keep the dispatcher. Together they are
5 subcommands and 17 flags on the stable listing. `guard` has no such
label although [guard.md](guard.md) was decided and built within the
week (#315).

**F29. `dispatch` runs on two systems and refuses on the third.** Its
summary says "experimental, macOS and Linux"; `helpGroups` is one list,
so `aries help` lists it on Windows too.

### 3.6 Names that do not say what they do

**F30. Verbs a newcomer cannot guess.** `prime` (orient a session),
`away` (extend my claims), `land` (report that commits landed),
`accept` (tick acceptance items, which reads like accepting an issue or
an assignment), `usage` (*send* token counts, not show them), `digest`
(a summary of a window), `resolve` (a conflict's, only), `pin`/`unpin`
(a memory's, only), `dep` (an abbreviation where `label`, `comment` are
whole words), `tui`. Each line of help explains its verb; the verb
itself does not. `prime`, `away` and `land` are in every guide and in
the hooks, so they are the most expensive to touch (§4).

**F31. `who` and `whoami` sit under different headings** (Coordination
and Setup, `help.go:56`, `:90`), and `login`/`logout`/`whoami` are three
of Setup's eight while `admin signin` is the admin's view of the same
sign-ins.

**F32. `finish` and `close`.** Both close an issue; `finish` closes
*yours* (ends the claim) and `close` any. The pair is right, and the
one-line summaries say so ("close your issue" against "close an
issue"), but the two words are near synonyms, and `close` on your own
claimed issue leaves the claim to the reaper where `finish` would end it.

**F33. `workflow` the command and `admin workflow` the group** (F5),
and `gate rearm`, `lock leave`, `dispatch brief`, `admin trust`:
each is a word that is right once you know the concept.

### 3.7 Help that is too long or out of order

**F34. `aries help` is 78 lines.** Eight headings, 53 commands, the
global flags and the exit codes (`aries help | wc -l`). A 50-line
terminal shows the second half. The redesign's own test holds every
*command's* help to 80 columns and not the listing to a height.

**F35. `gate`, `lock` and `dispatch` print no flags.** "A command that
dispatches before it parses (gate, lock, admin) gets its own help,
without flags" (`help.go:391-393`, the comment on `commandHelp`), so
`aries help gate` and `aries gate add --help` print the six usage forms
and no `flags:` block: `gate`'s 16 flags, `lock`'s 8 and `dispatch`'s
13 are documented only in `docs/cli.md`. `admin` is the exception that
works: `aries help admin member` lists its flags because each admin
group parses its own set.

**F36. Admin subcommand help prints the group's summary and the
group's examples.** `aries help admin status` prints the whole `admin`
summary ("list model prices and subscription plans, or set one; manage a
server's…") and then the example `aries admin principal add bob --key
bob.pub` (`help.go:458-470`: examples are chosen by the subcommand's
first word, and `status` has none of its own, so all are printed).

**F37. Usage errors print the whole usage as the fix.** `aries gate`
prints a `fix:` line of 403 characters and `aries dispatch` one of 360
(measured), the six or five forms joined by ` | ` on one line, where
`aries help gate` wraps them at 80 columns. A mistake in `gate pass`
quotes `gate add`'s forms too.

**F38. Small defects.** `aries help setup` prints `--every DURATION
how often the pull job runs (setup pull-job; default 15m) (default 0s)`:
the flag's default is 0 (`setup.go:37`) and the real default is in its
usage text, so `flagLines` appends a second. `aries help bogus` prints
the whole listing and exits 0 without saying that `bogus` is no command
(`cli.go:329-339`: a name `lookup` does not know falls through to the
listing). `show` has two summaries: "show an issue and its dependencies"
(`cli.go`, the entry) and "show an issue with its checklist, dependencies
and gates" (`help.go:32`). `watch`'s usage line is bare `watch` while
its flags list `--events`. `update`'s usage line ends in `...`. The
global help does not mention `--version` or `help` itself.

### 3.8 What this means for MCP (for the session changing `internal/mcpserver`)

Nothing here changes a tool. Three things to know while editing their
descriptions: (1) 24 CLI commands and MCP tools share a name (`create`
to `who`), so any rename in §4 that reaches an MCP name would be a
protocol-visible change; this review proposes none that does. (2) The
CLI-only acts of rule 12 (`accept`'s MCP equivalent is `finish`'s
`ticked`/`waived`; `away`, `hours`, `gate pass`, `memories`, `pin` have
no tool) are the lines an agent should not be sent to by a tool's error
text; `internal/mcpserver/errors.go` already rewords fixes. (3) F21: if
`cost`'s `since` is given a default on the CLI, the tool, if it stays,
should match.

## 4. Proposals, ranked

Ranked by what each saves a person against what it breaks. "Breaks"
names the callers of §2.2. Staging follows the repository's rules: a
rename keeps the old name as a hidden alias through 1.x (as `log` is
kept, `cli.go:203`), a removal gets a deprecation line in the release
notes a release before, and anything a hook or a scheduled job on a
person's machine calls keeps its old spelling for as long as such a job
can be running, since `aries upgrade` replaces the binary and not the
launchd plist, systemd unit or Task Scheduler task that `setup pull-job
--write` wrote.

### P1. Repair the help (F19, F35, F36, F37, F38)

Hide `create --id`; give `gate`, `lock` and `dispatch` a `flags:` block
(parse each action's flag set with `-h` before dispatching, as `admin`
does); give each admin subcommand its own summary line and show only its
examples; have a usage error quote only the form it failed in; print one
default for `setup --every`; say `unknown help topic` and exit 2 for
`aries help bogus`; one summary for `show`; `--events` in `watch`'s usage
line; `help`, `-h` and `--version` in the global block.

- Saves: the 37 flags that only `docs/cli.md` lists today become
  visible where a person asks; the 400-character fix lines go.
- Breaks: nothing. `TestHelpFits` and `TestHelpExamples` change with it.
  No script reads help.
- Staging: one pull request, no alias, no note.

### P2. Mark what is not for everyday use, in the listing (F24-F29, F34)

**As decided (§5, answers 1, 2, 3 and 5).** Keep the eight task
headings. Take the lines a person never types out of the main listing:
`mcp`, `usage`, `guard` and `pull-job` are listed by `aries help hooks`
alone, each one's own `--help` keeps working, and the main listing ends
with one line pointing at `aries help hooks`; `prime --hook` and `land
--hook` stay named in `prime`'s and `land`'s lines. Add one heading at
the end, **Experimental**, for `dispatch`, `outcome` and, for now,
`guard` (which the hooks listing also carries): the label means the
v1.0.0 freeze does not keep it yet, and `guard`'s comes off once it has
run a while here and in the dispatcher. `dispatch` and `outcome` keep
their names. Put `login`, `logout`, `whoami` under a heading **Sign in**
with `who` staying where it is, or move `who` beside `whoami`. Mark
person-only commands with a short phrase in their one line, "(a
person's)", as `close` marks `--force`: `away`, `hours`, `gate
pass|waive|rearm`, `resolve`; not a column.

- Saves: a person reads 47 lines of commands they might type and none
  they will not; an agent sees which lines are not its own.
- Breaks: nothing a program calls. `TestHelpGroups` names the eight
  headings (`help_test.go:36`) and `docs/cli.md:361-363` lists them;
  both change in the same pull request, and `TestHelpFits` gains `help
  hooks`.
- Staging: one pull request, with P1 and the 50-line height test
  (answer 9): a test beside `TestHelpFits` that holds `aries help` to 50
  lines, so that the next command is weighed against the screen.

### P3. One repeatable form for `--tag` and `--label`, a default for `cost --since` (F15, F21)

Make `recall --tag` and `digest --label` repeatable, and give `cost
--since` the default `7d` its own usage line already suggests.

- Saves: two surprises and one usage error.
- Breaks: nothing; a single value still parses, and a script that
  passed `--since` still does. The `cost` op's args are unchanged.
- Staging: one pull request; the MCP `cost` tool's `since` follows if
  it stays (§3.8).

### P4. Let `--json` and `--no-queue` go before a group's action (F16)

Read the global flags off the front of `gate`, `lock` and `dispatch`
arguments before the action, as `admin` does.

- Saves: the one place the global-flag promise in `aries help` is
  false.
- Breaks: nothing; every line that works today still does.
- Staging: one pull request.

### P5. Shorter `create` and `update` help (F18, F20)

Keep every flag, and have `aries help create` list the ten of its usage
line under `flags:` and the six long-text and ownership fields under a
second block, `also:` (`--design`, `--acceptance`, `--notes`, `--owner`,
`--status`, and the long forms `--priority`, `--type` folded into `-p`,
`-t`'s lines, which `flagLines` already does for `-t, --type`). Do the
same for `finish`: its own six flags, then "handoff fields, as
`handoff` takes them".

- Saves: 16 lines to 10 on the two commands people type most.
- Breaks: nothing. It is presentation; `flagLines` gains a rule.
- Staging: one pull request. This is the one proposal that argues with
  the help redesign's choice of "flags with their defaults, taken from
  the command's own flag set" (#237): it keeps the source of truth and
  changes the order.

### P6. Fold `memories` into `recall` (F2)

Make `memories` a hidden alias of `recall` and drop its line from help.

- Saves: one command, and one line of the Memory heading.
- Breaks: `docs/cli.md:601` and wherever else the guides name it;
  nothing calls it from a program. MCP has no `memories` tool.
- Staging: alias now (the line goes from help, the name keeps working),
  a deprecation line in the next release's notes, removal no sooner than
  2.0. The MCP protocol's `memory.list` op is unaffected.

### P7. Bring the workflow's read-only commands together (F5, F33)

**As decided (§5, answer 7).** The `workflow` group holds `show`,
`diff FILE`, `check FILE` and `set FILE [--rev N]`, so the workflow has
one home; the admin check on `set` stays on the server, where it is
(`workflow.set` is refused to anyone but a project admin or server
admin, from their own terminal). `admin workflow show|diff|set` stays as
an alias.

- Saves: a member finds the project's workflow under one word, and an
  admin applies it from the same group.
- Breaks: `docs/cli.md:446`, `:617` and the guides that name `admin
  workflow`; the ops skill does not cite it, and nothing in CI calls it.
- Staging: additive first (new names, the `admin` forms kept and
  unlisted); the aliases can be removed at 2.0.

### P8. One word for the private key file (F9)

**As decided (§5, answer 6).** `admin principal add --key FILE` becomes
`--pubkey FILE`, and `--key-file` keeps its name on `setup` and
`pull-job`, so the two stop sharing a stem. This is the cheaper side:
`--key` is on one admin command and the ops skill does not cite it,
while `--key-file` is in `docs/agents.md` (4 sites), the ops skill
(`setup --global --key-file`) and `setup`'s desktop entries.

- Saves: a confusion that costs a person one failed command and a read
  of the guide.
- Breaks: `docs/cli.md:619`, `docs/server.md` where `admin principal add
  --key` is quoted; an operator's notes.
- Staging: new name now, old name a hidden alias through 1.x, note in
  the release notes.

### P9. One filter name for "by this principal" (F10)

Accept `--by P` on `outbox` beside `--principal P`, and say in help that
`--by` is the filter everywhere a list is filtered by a principal
(`digest`, `hours`, `outbox`); `--principal` stays where it is a
*member* of something (`admin plan set`, `admin trust add`).

- Saves: one rule to learn instead of a list.
- Breaks: nothing; `--principal` stays on `outbox` as an alias.
- Staging: one pull request.

### P10. Join the landing entry points (F3): dropped

**Dropped (§5, answer 4): `pull-job --run` stays where it is.** Kept
here as it was proposed, for the record.

Make `pull-job --run` a form of `land`: `aries land --pull [--remote
NAME]` does what `pull-job --run` does, and `setup pull-job` writes that
line. Keep `pull-job --run` as a hidden alias.

- Saves: one top-level command, and the Gates heading reads as "gates,
  and landing for a server CI cannot reach".
- Breaks: every scheduled job already written by `setup pull-job
  --write` on every contributor's machine calls `aries pull-job --run`
  (`docs/cli.md:445`), and `aries upgrade` does not rewrite it. The alias
  must therefore live for as long as any such job can be running, which
  is "indefinitely" unless `setup pull-job --write` rewrites the entry
  on its next run and the guide tells people to run it. `docs/ci.md`
  (`:447-448`) and the coordinator's notes in this repository name
  `pull-job`.
- Staging: add `land --pull` now; have `setup pull-job --check` report an
  entry that still calls `pull-job --run` and `--write` rewrite it; keep
  the alias through 1.x with no removal date. This saves one line of
  help at the price of an alias that never goes: ranked here, not
  higher, for that reason.

### P11. Mark `dispatch` as what it is in the name (F28, F29): dropped

**Dropped (§5, answer 3): no `x` namespace. The Experimental heading
of P2 is enough, and `dispatch` and `outcome` keep their names.** Kept
here as it was proposed, for the record.

Put the experimental commands under one word, `aries x dispatch …` and
`aries x outcome …`, with `dispatch` and `outcome` kept as hidden
aliases, so that `aries help` lists one line, `x  experimental
commands: dispatch, outcome (aries help x)`, and the stable listing
stops changing when an experiment does.

- Saves: 2 lines now and every future experiment's line; the "the
  v1.0.0 freeze does not keep it" promise becomes visible in the name.
- Breaks: `docs/cli.md:487-573` and `docs/design/dispatcher.md`, the
  coordinator's memory files, and this repository's own habits; the
  dispatcher does not call `aries dispatch` itself (`exec.go` runs
  `lock`, `start`, `handoff`, `show`, `ready`, `list`, `inbox`, `cost`,
  `comment`, `watch`), so nothing automated breaks. Bearings' B0 replaces
  the whole package (`AGENTS.md`, Layout).
- Staging: aliases now, docs in the same pull request, no removal date
  since the experiment itself has none. Alternatively do nothing until
  Bearings replaces it, which is the cheaper path if B0 is near: a
  question below.

### P12. Rename the verbs that do not say what they do (F30)

**As decided (§5, answer 8).** `tick` becomes an alias of `accept`
(with `--undo`, `--waive` unchanged); `accept` stays, listed, and
`tick` is hidden, as `log` is for `hours`. `usage` is a hook's alone
already (the hooks listing of P2), and `prime`, `away`, `land`,
`digest` are left alone.

- Saves: one misreading (`accept`). `prime`, `away` and `land` are in
  every guide, the hooks (`prime --hook` at 18 sites in `agentsetup`),
  the ops skill and the dispatcher: renaming any of them costs a
  release of aliases in `.claude/settings.json` files on every
  contributor's machine for a word that is explained on the next line
  of help.
- Breaks: nothing. `accept` keeps its name and its line; `tick` is
  additive, and the MCP `finish` tool is unaffected.
- Staging: one pull request, in the second slice (§6). Ranked last
  because the saving is one word and the help line already explains it.

### Not proposed

- **Splitting `admin` into its own binary or hiding it from `aries
  help`.** It is one line of the listing already (`help.go:96`), its
  40 subcommands are behind it, and the ops skill, `docs/ci.md` and
  `docs/server.md` call `aries admin …` throughout. P1 fixes its help.
- **Removing `finish`'s handoff fields or `accept`** (F1, F20). Each is
  used and each saves a write; the cost is in help, which P5 addresses.
- **One paging idiom** (F7): `--cursor` and `--after` are different
  mechanisms, as surface-review.md §1.4 said.
- **Dropping `--no-queue` from the local commands, or `--json` from
  `tui`.** surface-review.md §1.5 and §1.1 kept them.
- **`pin`/`unpin` under `memory`**: surface-review.md question 4 kept
  them at the top level while memories are the only pinnable thing.

## 5. Decisions (maintainer, 22 October 2026)

The maintainer answered the review's nine questions the same day,
accepting the coordinator's recommendation on each:

1. **Hide the hook and job commands.** `mcp`, `usage`, `guard --hook`
   and `pull-job --run` leave the main listing and are listed by `aries
   help hooks`; each one's own `--help` keeps working (P2).
2. **`guard` is experimental for now.** It carries the label, meaning
   the v1.0.0 freeze does not keep it yet; the label comes off once it
   has run a while here and in the dispatcher (P2).
3. **No `x` namespace.** P11 is dropped: an Experimental heading is
   enough, and `dispatch` and `outcome` keep their names.
4. **`pull-job --run` stays where it is.** P10 is dropped.
5. **The person-only mark is a phrase, not a column.** "(a person's)"
   in the command's one line (P2).
6. **`--pubkey`.** `admin principal add --key` becomes `--pubkey`, and
   `--key-file` keeps its name (P8).
7. **The `workflow` group holds `show`, `diff`, `check` and `set`**, with
   the admin check on `set` kept on the server; `admin workflow …` stays
   as an alias (P7).
8. **`tick` is an alias of `accept`**; `accept` stays (P12).
9. **`aries help` is held to 50 command lines** by a test beside
   `TestHelpFits` (P2). Amended when slice 1 was built: the decision first
   said 50 lines, but the headings, the usage line, the hooks pointer, the
   global flags and the exit codes bring the screen to 78, and this review
   counted command lines only. The maintainer chose to hold the commands to
   50 (aries-2basuc7q).

## 6. Slices

- **Slice 1, nothing breaks:** P1's help repairs; P2's headings, with
  the hook and job commands behind `aries help hooks`, the Experimental
  heading (`dispatch`, `outcome`, `guard`) and the "(a person's)" marks;
  and the 50-line height test. One pull request, no alias, no
  deprecation line; `TestHelpGroups`, `TestHelpFits` and
  `docs/cli.md:361-368` change with it.
- **Slice 2, the renames and their aliases:** `--pubkey` with `--key`
  kept hidden (P8); the `workflow` group with `admin workflow …` kept
  as an alias (P7); `tick` as an alias of `accept` (P12). Each alias
  stays through 1.x, with a line in the release notes; the docs that
  name the old forms change in the same pull request.

P3, P4, P5, P6 and P9 were not asked about and stand as proposed; none
breaks anything and each is one pull request when taken.
