# The public surface before v1.0.0

A review, 15 October 2026, of everything v1.0.0 would freeze: the
commands and flags of `aries` and `ariesd`, environment variables, config
files, the wire protocol, the MCP tools, telemetry, files and paths, and
the output that people and scripts parse. After v1.0.0, semantic
versioning lets each of these change only compatibly (decision 81). The
review was made from the code at `547a3ab` (main), with the guides beside
it, and decides nothing. Each item gets one finding:

- **keep**: freeze it as it is;
- **rename**: to the name given, and why;
- **remove**: remove or hide it before 1.0 (experimental, unused, or premature);
- **document**: it stays, but no guide says so, or one says it wrongly;
- **decide**: a real question for the maintainer (numbered in
  [Questions for the maintainer](#questions-for-the-maintainer)).

References are `file:line` at that commit. "Undocumented" means no guide
in `docs/`, `README.md` or `RELEASING.md` names the item.

<!-- SUMMARY -->

## 1. `aries` commands, flags and exit codes

The command table is `internal/cli/cli.go:118-166`; `aries help` prints
it in that order. Every command is documented in `docs/cli.md`
§Commands (`docs/cli.md:200-325`), the agent workflow in
`docs/agents.md`, and `gate pass|fail` for CI in `docs/ci.md`.

### 1.1 Global flags and exit codes

| Item | Defined | Documented | Finding |
|---|---|---|---|
| `-C DIR` | `internal/cli/cli.go:360-380` | `docs/cli.md:151-156` | keep |
| `--json` (before or after the command) | `internal/cli/cli.go:371`, `:545` | `docs/cli.md`, throughout; rule 13 | keep |
| `--no-queue`, and `ARIES_NO_QUEUE` | `internal/cli/cli.go:546` | `docs/cli.md:145`, `:462` | keep |
| Exit 0, 1, 2, 3 | `internal/cli/cli.go:22-33`, mapped in `fail` at `:399-447` | `docs/cli.md:761-772`; rule 13 | keep |
| `lock run` exits with its command's code (`exitError`), 130 when the command died of the interrupt | `internal/cli/lock.go:428`, type at `:513-517`, read at `internal/cli/cli.go:342` | `docs/cli.md:257`, `:266-270`; not in §Exit codes | document: §Exit codes should say that `lock run` passes its command's code through, so a 1, 2 or 3 from it is the command's, not `aries`'s. Freezing the pass-through is right (it is what makes `lock run` usable in scripts); see question 9 |
| `watch --json` prints one object per line, `{"op":"inbox",…}` and `{"op":"resync"}` | `internal/cli/inbox.go:145-200` | `docs/cli.md:285` | document: rule 13 says `--json` prints exactly one document; `watch` is the one stream. Say so beside rule 13 and in §Exit codes' neighbor, and freeze the line shape |
| `--json` refused: `tui`, `lock run` | `internal/cli/tui.go:29`, `internal/cli/lock.go:400` | `docs/cli.md:257` (lock run); `tui` undocumented | keep; document `tui`'s refusal |

### 1.2 Commands

| Command | Defined | Documented | Finding |
|---|---|---|---|
| `create TITLE...` | `internal/cli/cli.go:120`; `cmdCreate` `internal/cli/commands.go:98-` | `docs/cli.md:222-246` | document: the usage line `aries help create` prints (`cli.go:120`) omits `--account` and `--paths`, which the command's own usage (`commands.go:100`) has. One usage string per command would end the drift |
| `show ID [--compact]` | `cli.go:121` | `docs/cli.md` §Find and change issues | keep |
| `list` | `cli.go:122`; flags `commands.go:362-380` | same | keep |
| `ready`, `blocked` | `cli.go:123-124` | same | keep |
| `update ID` | `cli.go:125`; flags `commands.go:45-67`, `:500-506` | same | document: the usage ends in `...`; `aries help update` should list every flag (`--design`, `--acceptance`, `--notes`, `--owner`, `--account`, `--paths`, `--defer-until`, `--due`) |
| `close`, `reopen` | `cli.go:126-127` | same | keep |
| `gate add|pass|fail|waive|rearm|list` | `cli.go:128`; `internal/cli/gate.go:40-300` | `docs/cli.md`, `docs/ci.md` | keep (see 1.4 on `--by`) |
| `lock acquire|renew|release|leave|list|run` | `cli.go:129`; `internal/cli/lock.go:70-430` | `docs/cli.md:247-279` | keep |
| `dep add|rm FROM TO` | `cli.go:130`; `commands.go:645-655` | `docs/cli.md` | decide: `rm` here and in `label`, `remove` in `admin member` and `admin key`, `drop` in `outbox`, `clear` in `cache` (question 4) |
| `label add|rm ID LABEL...` | `cli.go:131` | `docs/cli.md` | as `dep` |
| `comment`, `comments` | `cli.go:132-133` | `docs/cli.md` | keep: a verb to write, a plural noun to list, as `remember`/`memories` |
| `history` | `cli.go:134` | `docs/cli.md` | keep |
| `start`, `finish`, `accept`, `handoff` | `cli.go:135-138`; `internal/cli/work.go` | `docs/cli.md:212-221`, `docs/agents.md` | keep |
| `inbox`, `watch` | `cli.go:139-140` | `docs/cli.md:280-292` | keep |
| `tui` | `cli.go:141` | `docs/cli.md:326-370` | keep |
| `away DURATION` | `cli.go:142` | `docs/cli.md` | keep |
| `digest` | `cli.go:143` | `docs/cli.md` | keep |
| `who`, `whoami` | `cli.go:144-145` | `docs/cli.md` | keep. identity.md's `aries login` and `logout` join `whoami` (§9) |
| `cost` | `cli.go:146` | `docs/cli.md` | keep (see 1.4 on `--by`) |
| `log DURATION ID` / `log --undo ENTRY` / `log` | `cli.go:147`; `internal/cli/hours.go:15-130` | `docs/cli.md` | decide: `log` is a generic word for one narrow thing (hours), next to `history`, `ariesd`'s log and `--log-level`. `aries hours` (as the ops `hours.log` and `hours` already name it) would say what it is (question 4) |
| `remember`, `recall`, `memories`, `forget`, `pin`, `unpin` | `cli.go:148-153`; `internal/cli/memory.go` | `docs/cli.md:293-306` | decide: `pin` and `unpin` at the top level pin memories only, while gates, issues and locks are other nouns an operator might expect `pin` to act on. Keep if memories stay the only pinnable thing; otherwise `memory pin` (question 4) |
| `prime [--hook[=AGENT]]` | `cli.go:154`; `internal/cli/agent.go:20-60` | `docs/agents.md`; `CLAUDE.md` | keep |
| `usage --hook[=AGENT]` | `cli.go:155`; `internal/cli/capture.go:25-120` | `docs/agents.md:140-160` | keep |
| `conflicts`, `resolve N` | `cli.go:156-157`; `internal/cli/conflicts.go` | `docs/cli.md:590-644` | keep |
| `outbox [retry N|drop N|stats]` | `cli.go:158`; `internal/cli/outbox.go` | `docs/cli.md:371-590` | keep; `--host-key` is SSH's (§10) |
| `cache [clear]` | `cli.go:159`; `internal/cli/cache.go` | `docs/cli.md:645-760` | keep |
| `mcp` | `cli.go:160`; `agent.go:170-` | `docs/agents.md` | keep |
| `setup AGENT|--all` | `cli.go:161`; `internal/cli/setup.go:25-470`; agents `internal/agentsetup/agentsetup.go:87-130` (`claude-code`, `codex`, `gemini`, `cursor`, `vscode`, `junie`, `jetbrains`, `claude-desktop`) | `docs/agents.md` | keep |
| `admin prices|plans` | `cli.go:162`; `internal/cli/cost.go:205-` , `hours.go:120-` | `docs/server.md:1749-1798` | decide: plural nouns here, singular in every other `admin` group (`project`, `principal`, `member`, `key`). `admin price list|set` and `admin plan list|set` would match (question 4) |
| `admin project add|list|archive|activate` | `internal/cli/admin.go:60-150` | `docs/cli.md:307-325`, `docs/server.md:1378-1549` | keep |
| `admin principal add|list|disable|enable|whois` | `internal/cli/admin.go:195-320` | same | keep |
| `admin member add|list|remove` | `internal/cli/admin.go:340-450` | same | keep |
| `admin key add|list|remove` | `internal/cli/admin_key.go:110-200` | same | keep |
| `admin status [-n N] [--after NAME]` | `internal/cli/admin_status.go:15-40` | `docs/cli.md:322`, `docs/server.md:1550-` | keep |
| `upgrade [--check] [--rollback]` | `cli.go:164`; `internal/cli/upgrade.go:85-190` | `docs/install.md` | keep |
| `version` | `cli.go:165` | `docs/cli.md` | keep |
| `help [COMMAND]` | `internal/cli/cli.go:310-335` | `docs/cli.md:200` | keep |

### 1.3 Flags not shown by `aries help`

These flags are registered but neither `aries help` nor any guide names
them.

| Flag | Defined | Documented | Finding |
|---|---|---|---|
| `create --id ID` | `internal/cli/commands.go:109` | undocumented | decide: it lets a person choose an issue's id, which `import-bd` needs and nobody else does. Hide it from 1.0's surface, or document it (question 8) |
| `--design`, `--acceptance`, `--notes`, `--owner` on `create` and `update` | `internal/cli/commands.go:53-62` | undocumented (the fields are in `docs/concepts.md`) | document |
| `--priority`, `--type` (long forms of `-p`, `-t`) | `commands.go:57-60`, `:368-371` | undocumented | keep; document |
| `-l` (short for `--label`) on `create` and `list` | `commands.go:108`, `:373` | undocumented | keep; document |
| `dep -t` (short for the dependency `--type`) | `commands.go:651` | undocumented | rename: drop `-t` on `dep`. Everywhere else `-t` is an issue's type; here it is an edge's (`blocks`, `waits-for`…) |

### 1.4 Flags that mean different things

| Flag | Meanings | Defined | Finding |
|---|---|---|---|
| `--by` | a principal filter (`digest`, `log`); the grouping of a report (`cost --by account|issue|epic|person|model`); the principals who may report a CI gate (`gate add --kind ci --by P...`) | `digest.go:18`, `hours.go:22`, `cost.go:23`, `gate.go:49` | rename: `cost --group-by` (or `--per`), and `gate add --reporter P` (decision 68's word for who reports a check), leaving `--by` as "by this principal" |
| `--since` | a duration only (`who`), a duration, date or time (`digest`, `cost`) | `who.go:22`, `digest.go:17`, `cost.go:24` | keep; one parser for all three would let `who --since` take a time too, a compatible change later |
| `--after` | a name, for paging (`admin * list`, `admin status`); a conflict number (`conflicts`) | `admin.go:50`, `conflicts.go:27` | keep: both are "the cursor after which to list", of the list's own key |
| `--from` | a date or time when rates take effect (`admin prices set`), a month (`admin plans set`) | `cost.go:210`, `hours.go:125` | keep |
| `--undo` | a switch (`accept`), a flag with a value (`log --undo ENTRY`) | `work.go:241`, `hours.go:19` | keep; both read as "undo" |
| `--waive` | a reason for the listed items (`accept`), `N=REASON` repeated (`finish`); and `gate waive` is a subcommand | `work.go:242`, `work.go:100` | keep: `finish` names several items, each with a reason |
| `--rev` | the revision read, for compare-and-swap (`update`, `close`, `reopen`, `forget`); the memory to replace (`remember`) | `commands.go:505`, `:613`, `:628`; `memory.go:53`, `:160` | keep: in `remember` it is the same compare-and-swap |
| `--for` | a lease's length (`start`, `lock acquire`, `lock renew`, `lock run`) | `work.go:30`, `lock.go:76`, `:234` | keep |
| `-n` | a page or list size, everywhere | throughout | keep |
| `--branch` | a switch (`start --branch` makes one), a value (`finish`, `handoff --branch B`), a value for CI (`gate pass --branch NAME`) | `work.go:32`, `:298`, `gate.go:113` | decide: `start --branch` is the only boolean `--branch` (question 4). `start --new-branch` would remove the ambiguity |
| `--principal` | a plan's principals (`admin plans set`), which principal's outbox (`outbox`) | `hours.go:129`, `outbox.go:241` | keep |
| `--check` | a CI gate's check name (`gate`), a dry run (`setup --check`, `upgrade --check`) | `gate.go:52`, `:111`; `setup.go:28`; `upgrade.go:92` | keep: each is clear in its command, and `--check` as a dry run is the common idiom |

### 1.5 Findings that cut across commands

- **Verb and noun order** is consistent: `aries NOUN VERB` for groups
  (`gate`, `lock`, `dep`, `label`, `admin project`…), a bare verb for
  the work loop (`start`, `finish`, `remember`). Keep.
- **Removing** is spelled four ways: `rm` (`dep`, `label`), `remove`
  (`admin member`, `admin key`, `setup --remove`), `drop` (`outbox`),
  `clear` (`cache`). `drop` and `clear` say something different (the
  entry is discarded unsent; the whole cache goes) and can stay; `rm`
  and `remove` are the same act. Question 4.
- **Short flags** are few and consistent (`-C`, `-n`, `-p`, `-t`, `-l`),
  apart from `dep -t` (1.3).
- **`--json`** covers every command that prints data, the `admin`
  groups, `setup`, `upgrade` and `version` included; the exceptions are
  in 1.1.
- **Defaults**: `list -n` 50; `comments` and `history` the newest page
  of 100; `who` 5 minutes; `digest --since` 24 hours. Each is in
  `docs/cli.md`. Keep.
