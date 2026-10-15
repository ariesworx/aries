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
| `--by` | a principal filter (`digest`, `log`); the grouping of a report (`cost --by account|issue|epic|person|model`); on `gate add --kind ci`, the principals who may report, as an alias of `--approver` that writes the same variable | `digest.go:18`, `hours.go:22`, `cost.go:23`, `gate.go:48-49` | rename: `cost --group-by`, and drop `gate add --by` in favor of `--approver`, which it already is. That leaves `--by` meaning "by this principal" everywhere. The MCP `cost` tool's `by` follows (§5) |
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
- **Defaults** for `-n` differ by command:
  - 50 for `list`, `log`, `cost` and `gate list`;
  - 20 for `inbox`, `conflicts`, `recall` and `memories`;
  - 100 for `who`;
  - every entry for `comments` and `history`, unless `-n` is given.

  Each is the right size for its list. Keep, and print the default in
  each command's help.
- **Hidden aliases** are not in `aries help`:
  - `gate ls` and `lock ls` (`internal/cli/gate.go:37`, `internal/cli/lock.go:55`);
  - `dep remove` and `label remove` (`internal/cli/commands.go:659`, `:694`).

  Whatever question 4 settles, freeze the aliases with it or drop them
  now. An alias left undocumented still has to be kept for good once
  1.0 ships.
- **Group actions are positional before flags** in `gate` and `lock`
  (`gate.go:31`, `lock.go:45`), so `aries gate --json list` fails, while
  `aries admin` finds its group anywhere (`internal/cli/cost.go:181`).
  Keep the rule (flags after the action) and document it; relaxing it
  later is compatible.
- **Single or repeatable** flags differ between commands:
  - `--tag` repeats in `remember` (`memory.go:51`) but takes one value
    in `recall` (`:108`);
  - `--label` repeats in `create` and `list` but takes one value in
    `digest` (`digest.go:19`).

  Make the single forms repeatable, which is a compatible change. Keep.
- **Naming the issue a gate is about**: `gate add --kind ci --subject ID`
  (`gate.go:53`) but `gate pass|fail --issue ID` (`gate.go:112`).
  Rename `--subject` to `--issue` before 1.0, keeping `--subject` as a
  hidden alias for the release.
- **`start --for`** defaults to 8 hours (`work.go:30`) and `lock --for`
  to 15 minutes (`lock.go:76`). Both are documented; keep.
- **`--no-queue`** is accepted by every command, the local ones too
  (`cli.go:546`). Keep: refusing it would make scripts fragile.

## 2. `ariesd` commands, flags and exit codes

The commands are dispatched in `cmd/ariesd/main.go:170-197`, with their
usage text at `main.go:65-123`. The guide's table is
`docs/server.md:3953-3965`. Flags that are only Dolt's or SSH's
(`--dsn`, `--allow-unsafe-dolt`, `stdio --principal`/`--key`,
`control init --authorized-keys`/`--restore-removed-access`) are listed in
[§10](#10-settled-elsewhere-dolt-and-ssh) and get no finding here.

| Item | Defined | Documented | Finding |
|---|---|---|---|
| `ariesd serve` `--config`, `--socket` | `cmd/ariesd/main.go:201-214` | `docs/server.md:2128`, `:2140`, `:3957` | keep |
| `serve --dev` | `cmd/ariesd/admin.go:120-123`; peer check `main.go:242` | `docs/server.md:2176-2179` | keep. Also accepted by `control init`, `import-bd`, `export-bd` and `upgrade`, which the table at `:3957` does not say: document |
| `serve --accept-clock-behind TIME` | `main.go:223`, parsed `:235-241` | `docs/server.md:2025`, `:3957` | keep; the package comment (`main.go:3`) leaves it out: document |
| `serve --project`, `--prefix`, `--log-level`, `--log-format` | `main.go:225-228` | `docs/server.md:2134-2146` | keep (see `prefix` in §3.2) |
| `ariesd stdio` | `main.go:583-610` | `docs/server.md:544`, `:3958` | settled elsewhere (§10) |
| `ariesd control init --name` | `cmd/ariesd/control.go:174-190` | `docs/server.md:1065`, `:3959` | decide with the Postgres port: `control` has one subcommand, `init`. Keep the group; P1 decides whether it survives |
| `ariesd import-bd [--project] [--principal import] [--dry-run] [--json] FILE|-` | `cmd/ariesd/admin.go:144-205` | `docs/migrate-from-bd.md:23-51` | keep. The option table at `docs/migrate-from-bd.md:44-49` omits `--project`, `--config`: document |
| `ariesd export-bd [--project] [-o FILE]` | `admin.go:209-230` | `docs/migrate-from-bd.md:108` | keep |
| `ariesd upgrade [--check] [--to vX.Y.Z] [--rollback] [--restart] [--socket]` | `cmd/ariesd/upgrade.go:83-120` | `docs/server.md:3466-3676`; `RELEASING.md:76`, `:147` | keep. `--socket` is undocumented at `:3962`: document. `aries upgrade` has no `--to`; question 10 |
| `ariesd status [--json]` | `cmd/ariesd/status.go:27-70` | `docs/server.md:3430-3465` | keep |
| `ariesd version`, `--version`, `help` | `main.go:189-192` | `docs/server.md:3964`; `help` undocumented | keep |
| Exit 0 and 1 | `main.go:136`, `:159` | `docs/server.md:3957` | keep |
| Exit 2, usage error | `main.go:155-158` | undocumented | document, beside `aries`'s |
| Exit 75, `serve` refuses a clock behind its history (`exitClockBehind`) | `main.go:142-149`, `:168`; `RestartPreventExitStatus=75` in the unit (`install-server.sh`) | `docs/server.md:470`, `:2010-2028` | keep: systemd's unit depends on it, so it is part of 1.0 |

## 3. Environment variables and config files

### 3.1 Environment variables

User-facing variables only. The tests' own (`ARIES_REQUIRE_DOLT`,
`ARIES_SOAK_*`, `ARIES_SMOKE_*`, and those `internal/clienttest` sets)
are not public and are not frozen.

| Variable | Read by | Defined | Documented | Finding |
|---|---|---|---|---|
| `ARIES_KEY_FILE` | `aries` | `internal/client/config.go:371`; written by `aries setup --key-file` (`internal/agentsetup/agentsetup.go:162`) | `docs/cli.md:138` | keep. It overlaps `.aries.yaml`'s `key:` on purpose and wins over it (decision 35); the guide says so |
| `ARIES_KEY` (refused, never read) | `aries` | `internal/client/config.go:377` | `docs/cli.md` §Your key is your identity | keep: a guard against a key's text in a variable |
| `ARIES_SESSION` | `aries`, `aries mcp` | `internal/client/session.go:17` | `docs/cli.md:141`, `docs/agents.md` §Sessions | keep |
| `CLAUDE_CODE_SESSION_ID`, `CLAUDE_SESSION_ID` | `aries` | `internal/client/session.go:17` | `CLAUDE.md`; `docs/agents.md` | remove `CLAUDE_SESSION_ID`: the comment calls it "the older spelling"; a 1.0 that reads it must read it for good |
| `ARIES_HARNESS`; `CLAUDECODE=1`, `GEMINI_CLI=1` as fallbacks | `aries` | `internal/client/session.go:85-96`; `internal/agentsetup/agentsetup.go:15` | `docs/cli.md:141`; the fallbacks at `docs/agents.md:479-480` | keep |
| `CI` (`true` → session `ci`) | `aries` | `internal/client/session.go:37` | `docs/cli.md:142`, `docs/ci.md` | keep |
| `GITHUB_HEAD_REF`, `GITHUB_REF_NAME`, `GITHUB_REF_TYPE`, `CI_MERGE_REQUEST_SOURCE_BRANCH_NAME`, `CI_COMMIT_BRANCH` | `aries gate --from-branch` | `internal/gitx/ci.go:21-28` | `docs/cli.md:143` | keep |
| `TRACEPARENT`, `TRACESTATE` | `aries`, `aries mcp` | `internal/client/session.go:46-47` | `docs/agents.md:488-517` | keep (OpenTelemetry's environment carrier) |
| `ARIES_LOCK_EPOCH` (set, never read) | the command `lock run` runs | `internal/proto/locks.go:19` | `docs/cli.md:144` | keep |
| `ARIES_NO_QUEUE` | `aries`, `aries mcp` | `internal/offline/client.go:18` | `docs/cli.md:145` | keep |
| `ARIES_DATA_DIR`, `XDG_DATA_HOME` | `aries` | `internal/offline/local.go:30`, `:54` | `docs/cli.md:146`, `:718-760` | keep |
| `ARIES_RELEASES` | `aries upgrade`, `ariesd upgrade`, `install.sh`, `install-server.sh` | `internal/release/static.go:20`; `install.sh:24-40` | `docs/cli.md:147`, `docs/install.md`, `docs/server.md:2144` | keep. It is the one variable `ariesd` reads under an `ARIES_` name (its setting is `releases:`); sharing it with `aries` and the installers is deliberate. Document why beside the Settings table |
| `ARIES_VERSION`, `ARIES_DIR` | `install.sh` | `install.sh:60-61` | `install.sh --help`; `docs/install.md` | rename `ARIES_DIR` to `ARIES_INSTALL_DIR`: `ARIES_DATA_DIR` and identity.md's `ARIES_PROJECT_DIR` are other directories, and a bare `ARIES_DIR` reads as any of them |
| `ARIES_BASE_URL` | `install.sh`, `install-server.sh` | `install.sh:30`, `:83-90` | `install.sh --help` ("for tests") | remove from the public surface, or document as tests only. `ARIES_RELEASES` already names a mirror |
| `ARIES_PUBKEY_PEM` | `install.sh`, passed on by `install-server.sh:661-663` | `install.sh:111-114` | `install-server.sh:643` says "for tests" | decide (question 7): it replaces the release signing key and only warns. A variable that turns off the one check a release is accepted on should not ship in the one-line installer of a 1.0 |
| `ARIES_DOLT_BASE_URL` | `install-server.sh:518` | `install-server.sh:49` | settled elsewhere (§10) |
| `ARIESD_CONFIG`, `ARIESD_SOCKET`, `ARIESD_PROJECT`, `ARIESD_LOG_LEVEL`, `ARIESD_LOG_FORMAT`, `ARIESD_ADMINS`, `ARIESD_CLOCK_BEHIND`, `ARIESD_TELEMETRY_LISTEN` | `ariesd` | `internal/server/settings.go:203-226`, read at `:266-331` | `docs/server.md:2131-2159` | keep. Precedence is flag, variable, file, default (`settings.go:272`); most settings have no variable, which is fine |
| `ARIESD_DSN`, `ARIESD_CONTROL_DB`, `ARIESD_KEYS_FILE`, `ARIESD_AUTHORIZED_KEYS`, `ARIESD_BRIDGE`, `ARIESD_TELEMETRY_DOLT_METRICS_URL` | `ariesd` | same | same | settled elsewhere (§10) |
| `OTEL_SDK_DISABLED`, `OTEL_{METRICS,TRACES,LOGS}_EXPORTER`, `OTEL_EXPORTER_OTLP_*` (endpoint, headers, protocol, timeout, and each per signal), `OTEL_METRIC_EXPORT_{INTERVAL,TIMEOUT}`, `OTEL_TRACES_SAMPLER[_ARG]`, `OTEL_BSP_*`, `OTEL_BLRP_*`, `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | `ariesd` | `internal/telemetry/env.go:20-53` | `docs/server.md:2847-2886` | keep: the specification's names, which is the point |
| `NOTIFY_SOCKET`, `WATCHDOG_USEC`, `WATCHDOG_PID` | `ariesd` | `internal/sdnotify/sdnotify.go:19-23` | `docs/server.md` §5 | keep (systemd's) |
| `ARIES_KEYS`, `ARIES_ADMINS`, `ARIES_PROJECT`, `ARIES_PORT`, `ARIES_IMAGE`, `ARIES_TELEMETRY` | the Docker test server | `deploy/docker/compose.yaml:3-36`, `deploy/docker/entrypoint.sh` | `docs/deploy/docker.md` | rename: `ARIES_PROJECT` here is the server's project UUID, which `ariesd` calls `ARIESD_PROJECT`, and identity.md gives `ARIES_PROJECT` to the client for the project's name or id (§9). Name the Docker ones `ARIESD_*` where `ariesd` has one (`ARIESD_PROJECT`, `ARIESD_ADMINS`) and `ARIES_DOCKER_*` for the rest. The test server is not a product, but its variables are copied into people's scripts |

### 3.2 `.aries.yaml`

`internal/client/config.go:24-75`, read strictly (`KnownFields(true)`,
`config.go:201`); the guide is `docs/cli.md:8-200`.

| Key | Defined | Documented | Finding |
|---|---|---|---|
| `project` (the project's UUID) | `config.go:28` | `docs/cli.md` §Connect a repository | keep |
| `server.host` | `config.go:32` | same | keep; the HTTPS design decides whether a URL joins it (§10) |
| `server.port` (22), `server.user` (`aries`), `server.host_key`, `server.host_keys`, `server.iap.{project,zone,instance}` | `config.go:34-73` | `docs/cli.md:31-70`, `:163-199` | settled elsewhere (§10) |
| `key` | `config.go:52` | `docs/cli.md:71-133` | settled elsewhere (§10) |
| Strict decoding: an unknown key is refused | `config.go:201` | undocumented | decide (question 2). The file is committed and read by every teammate's `aries`, of whatever version. Strict decoding means a key added in 1.1 (a server URL for HTTPS, say) makes every 1.0 client in the repository refuse to start. Ignore unknown keys with a one-line warning before 1.0; strictness can only be relaxed compatibly once, and this is that time |
| No version key | — | — | keep: with unknown keys ignored, none is needed |
| Search: up from the working directory to the repository's top or `$HOME`; must be the user's own and not group- or world-writable | `config.go:114-160` | `docs/cli.md:151-162` | keep |

### 3.3 `ariesd.yaml`: settings

`internal/server/settings.go:31-124`, read strictly (`settings.go:529`;
an operator controls both the file and the binary, so strictness is
right here). The guide's table is `docs/server.md:2131-2159`; every key
in the code is in it and no other.

| Key | Defined | Default | Finding |
|---|---|---|---|
| `socket` | `settings.go:36`; `internal/server/listen.go:14` | `/run/aries/ariesd.sock` | keep |
| `project` | `settings.go:62` | required | keep |
| `prefix` | `settings.go:63-64`, default `:270` | `sf` | rename the default. `sf` is what is left of the project's former name, which decision 80 rules out. `ar` would follow the rename; an issue's id carries the prefix forever, so the default must change before 1.0 or never. Question 1 |
| `latest` | `settings.go:66` | none | decide: an operator types the newest release by hand so that `prime` can say the server is older. It is the only setting that describes the world rather than the server, and goes stale. Remove, or keep and document that `prime` trusts it (question 11) |
| `systemd_unit` | `settings.go:70` | none (`--restart` uses `ariesd`) | keep |
| `releases` | `settings.go:72-75` | GitHub | keep |
| `log_level`, `log_format` | `settings.go:78-82` | `info`, `text` | keep |
| `admins` | `settings.go:86` | none | keep |
| `account` | `settings.go:99` | `internal` | keep |
| `clock_behind` | `settings.go:105` | `stop` | keep |
| `clock_behind_tolerance`, `clock_step_tolerance`, `clock_client_tolerance` | `settings.go:106-114` | `2m`, `5s`, `5m` | keep |
| `telemetry.listen`, `telemetry.per_project`, `telemetry.principals`, `telemetry.audit_log`, `telemetry.principals_key` | `settings.go:148-184` | none, `true`, `pseudonym`, `true`, `telemetry/principals.key` | keep. `telemetry_requests` belongs here rather than under `limits:` (3.4) |
| `dsn`, `control_db`, `project_dbs`, `keys_file`, `authorized_keys`, `bridge`, `telemetry.dolt_metrics_url` | `settings.go:34-60`, `:177` | | settled elsewhere (§10) |
| `prefix` beside `project_dbs.prefix` | `settings.go:64`; `internal/server/project.go:126` | | none here: `project_dbs` goes with Dolt, and the clash with it |

**Formats.** Durations go through `proto.ParseDuration`
(`internal/proto/duration.go:12`): Go's units plus `d` and `w`, and
never zero or negative. Keep, and document in the Settings section that
`0` is refused rather than meaning "off" or "default", since
`Limits.Validate`'s `< 0` checks (`internal/server/limits.go:180-185`)
suggest otherwise to a reader of the code.

### 3.4 `ariesd.yaml`: `limits:`

The keys are in three structs, inlined into one map:
- `store.Limits`: `internal/store/limits.go:20-107`;
- `store.ControlLimits`: `internal/store/control_limits.go:11-30`;
- `server.Limits`: `internal/server/limits.go:23-110`.

The guide's table is `docs/server.md:2181-2243`. All 52 keys are in it,
and it names none that the code lacks.

The names follow several patterns at once:

| Pattern | Keys | Finding |
|---|---|---|
| `THING_per_SCOPE` | `labels_per_issue`, `deps_per_issue`, `paths_per_issue`, `conflicts_per_issue`, `sessions_per_principal`, `conns_per_principal`, `handshakes_per_principal`, `lock_waits_per_principal`, `memories_per_scope`, `principals_per_creator`, `members_per_project`, `keys_per_principal`, `usage_per_day`, `hours_per_day`, `notices_per_minute` | keep: the clearest form, and most keys already have it |
| A bare plural, its scope implied | `conns` (server), `locks`, `open_gates`, `prices`, `plans` (project), `projects`, `principals` (server), `open_stores` | rename for one rule, if the maintainer wants one (question 3). Suggested: `conns_per_server`, `locks_per_project`, `gates_per_project` (for `open_gates`), `prices_rows`, `plans_rows`, `projects_per_server`, `principals_per_server`. Cost: renames in three structs, the guide's table, the installer's config and the tests, about a day. Without a rule, keep as they are: each is documented |
| An item count named by its owner | `acceptance_items`, `gate_approvers`, `gate_checks`, `lock_capacity`, `lock_waiters`, `memory_tags`, `plan_principals`, `usage_records`, `inbox_unread`, `refusal_logs` | as above: `acceptance_items_per_issue`, `approvers_per_gate`, `checks_per_issue`, `holders_per_lock`, `waiters_per_lock`, `tags_per_memory`, `principals_per_plan`, `records_per_usage_call`, `unread_per_principal`, `refusal_logs_per_minute` |
| A size in bytes with no unit | `memory_body`, `hours_note`, `gate_evidence`; `memory_key_length` and `memory_tag_length` | rename (question 3): `memory_body_bytes`, `hours_note_bytes`, `gate_evidence_bytes`, `memory_key_bytes`, `memory_tag_bytes`. "Length" reads as characters, and each is bytes |
| A rate | `write_rate`, `control_write_rate` (a second, float); with `write_burst`, `control_write_burst` | keep; but identity.md's `enroll_rate` is a minute (§9), so name every rate's unit if a minute one joins: `write_rate` stays a second and the new one becomes `enroll_per_minute` |
| A duration, with six suffixes | `agent_keep`, `inbox_keep`, `idem_window`, `conflict_age`, `store_idle`, `idle_timeout`, `due_soon`, `away_max` | keep the names: each says what the time is for, and none is a count. `conflict_age` and `due_soon` bound nothing (the guide says so) and are thresholds, not limits: move them out of `limits:` (question 3) |
| Set only with `control_db` | `open_stores`, `store_idle`, `projects`, `principals`, `principals_per_creator`, `keys_per_principal`, `members_per_project`, `control_write_rate`, `control_write_burst` | settled elsewhere for `open_stores` and `store_idle`, which are Dolt's store-per-database model (§10); keep the rest |
| `telemetry_requests` | `internal/server/limits.go:97` | | rename: move it to `telemetry.max_requests`. It applies only with `telemetry.listen` |
| Go field names that differ from the keys (`Labels` for `labels_per_issue`, `Handshakes`, `Sessions`, `Keys`, `Members`, `Memories`, `Notices`) | `internal/store/limits.go`, `internal/server/limits.go` | | keep: not public |
| `conflict_age`'s default | `internal/store/conflicts.go:59` (24 hours) | `docs/server.md:2224` says `1d` | keep: the same value |

## 4. The wire protocol

Everything is in `internal/proto`, documented in its package comment
(`internal/proto/doc.go:1-91`) and design §2. No guide documents the ops
or frames, which is right: they are `aries`'s and `ariesd`'s, and people
meet them through those two binaries. Error codes are what people do
meet, and the guides name each of them where it applies.

### 4.1 What decision 78 settles

| Item | Defined | Finding |
|---|---|---|
| `Proto = 8`, `ProtoMin = 7`, `ProtoMax = 8` | `internal/proto/version.go:197-201` | decide (question 5). Decision 78 says the release settles the protocol "to one version above any released one". v0.4.0 spoke 4, so read literally that is 5, while the maintainer's words in the same decision were "fold G0 into version 8". Either works, because no client of 5 to 8 was ever released. Recommend **8**: the code, the tests, `TestReleasedClients` and every comment already say 8, and settling it at 5 renumbers for no reader's benefit. Whichever is chosen, v1.0.0 speaks one version: `Proto = ProtoMin = ProtoMax` |
| `Speak` (step down after a version refusal) | `version.go:203-215` | remove: it chooses between unreleased versions |
| `ProtoKeys`, `keyedSince`, `Keyed`, `ForProtocol` | `internal/proto/idem.go:19-90` | remove; `idem` is then simply an argument of each op that takes one |
| `ProtoConflicts`, `ProtoLate` | `internal/proto/conflicts.go:16-23` | remove |
| `codeFor` (`stale` sent as `conflict` to a client below 6) | `internal/server/errors.go:51-59`, called at `internal/server/server.go:861` | remove |
| `stepDown` and the second hello | `internal/client/conn.go:221-224`, `:417-428` | remove |
| The outbox's protocol checks (`f1Proto`, `speaks`, the `Keyed` checks before queueing and replay) | `internal/offline/local.go:604-608`, `:682-698`, `:871-882` | remove. `seen.json`'s `proto` (`local.go:116-119`) can stay as a record |
| The version history | `version.go:11-196` | remove to a short note in design §11. The "(protocol N)" notes on types throughout `internal/proto` go with it |
| The handshake's example, `"p":5`, `"min":5,"max":5` | `internal/proto/doc.go:43-44` | document: it should show the settled version |
| `CheckProto`, the hello check, the welcome's `min` and `max`, the status report's `proto_min` and `proto_max` | `version.go:229-242`; `internal/server/server.go:974`, `:1021-1028`; `internal/proto/admin.go:424-425` | keep: rule 10's range is how 1.x adds ops |

### 4.2 Ops

The 76 op names are defined in these files:
- `internal/proto/ops.go:14-37`
- `inbox.go:11-13`
- `memory.go:10-13`
- `cost.go:19-21`
- `hours.go:18-22`
- `conflicts.go:11-13`
- `gates.go:10-19`
- `locks.go:10-14`
- `admin.go:22-37`

Dispatch is `internal/server/dispatch.go:53-108`, with admin ops in
`internal/server/admin.go`.

| Item | Defined | Finding |
|---|---|---|
| Writes are `noun.verb` (`dep.add`, `label.rm`, `gate.pass`, `lock.acquire`, `price.set`, `hours.log`, `project.add`, `key.remove`), lists are a bare plural (`gates`, `locks`, `prices`, `plans`, `hours`, `projects`, `principals`, `members`, `keys`, `claims`, `conflicts`) | as above | keep, and hold identity.md to it (§9): its `invite.list`, `identity.list` and `trust.list` should be `invites`, `identities` and `trusts`/`trust_rules` |
| `dep.rm`, `label.rm` against `member.remove`, `key.remove`, `hours.delete`, `project.archive` | `ops.go:23-25`; `admin.go:33-36`; `hours.go:19` | keep on the wire (renaming an op costs a compatibility path for nothing a person sees); the CLI question 4 is the visible one |
| `resolve` beside `conflict.propose` | `conflicts.go:12-13` | rename `resolve` to `conflict.resolve`, which the event already is (`internal/proto/event.go`). Free before the settle |
| `dates` (only `prime` and the cache use it) | `gates.go:15` | keep |
| `IDArgs`, used by no op | `ops.go:508` | remove |
| The comments on `update`, `handoff` (the result is `UpdateResult`/`HandoffResult`, not `WriteResult`) and `principals` (takes `PrincipalsArgs`) | `ops.go:19`, `:31`; `admin.go:27` | document (fix the comments) |
| `dep.rm` accepts `idem` and ignores it | `internal/server/dispatch.go:426-431` | decide with question 5: give it a key's effect or refuse the field. Recommend making it keyed, as `dep.add` is, so a replayed remove applies once |

### 4.3 Argument and result fields

| Item | Defined | Finding |
|---|---|---|
| snake_case JSON throughout the `*Args` and `*Result` types | `internal/proto/*.go` | keep |
| Frame keys are one or two letters (`t`, `id`, `op`, `a`, `ok`, `err`, `v`, `p`, `s`, `m`, `h`, `pr`, `k`, `e`, `tp`, `ts`) except `proj`, `latest`, `min`, `max` | `internal/proto/frame.go:39-78` | keep: frames are not printed, and the short keys are measured bytes on every request (design §9) |
| `Error` is `{"c", "m", "fix", "req"}` | `internal/proto/errors.go:46-56` | keep on the wire; but `aries --json` prints it as it is (§8.1) |
| The issue's id goes by several names: `id` in most args, `issue` in conflicts, hours, memories, inbox items and events, `blocks`/`subject` in gates, `parent` in args beside `parent_id` in `Issue`; and `Claim.id` and `Overlap.id` are issue ids | `ops.go`, `conflicts.go`, `hours.go`, `memory.go`, `inbox.go`, `gates.go`, `paths.go` | decide (question 6). `id` is the issue in an op about one issue, and `issue` where the record is something else (a memory, an hours entry, a conflict). That rule is sound and nearly holds. The exceptions to fix before the settle: `Issue.parent_id` (make it `parent`, as every argument is); `Claim.id` and `Overlap.id` (make them `issue`) |
| `id` is an hours entry in `HoursDeleteArgs` and `HoursLogResult` but the issue in `HoursLogArgs`; a principal's name in `PrincipalResult` and `MemberSetResult`; a fingerprint in `KeyResult` | `hours.go:28-52`; `admin.go:145`, `:232`, `:337` | rename: `HoursLogArgs.issue` for the issue; `name` and `fingerprint` where those are meant, as `Principal` and `Key` already have them |
| `released` is a count of claims ended (`ProjectResult`, `PrincipalResult`, `MemberRemoveResult`) and an epoch (`LockReleaseResult`) | `admin.go:62-73`, `:145`, `:309`; `locks.go:72` | rename `LockReleaseResult.released` to `epoch`, as `LockLeaveResult` has it |
| `finish`'s `ticked` and `waived` against `accept`'s `tick`, `untick` and `waive` | `ops.go:349`, `:621` | keep: `finish` reports what was done, `accept` does it |
| The note is `handoff` on `finish` and `note` on `handoff` | `ops.go:621`, `:657` | keep: `finish.handoff` is the handoff, `handoff.note` its text. Both are documented to agents |
| `due` in `create`/`update`, `due_at` in results; `defer_until` in both | `ops.go:152`, `:410`; `Issue` `ops.go:81-82` | rename the result field to `due` (or the arguments to `due_at`): one name for one date. `defer_until` shows the form |
| Who did something: `by` (claims, digests, gates, locks, conflicts), `principal` (agents, hours, lock holders), `author` (comments, memories), `from` (inbox), `created_by`, `added_by`, `set_by`, `updated_by` | throughout | keep: `by` for an act, `author` for text, `X_by` for a record's audit columns. Write that rule into `internal/proto/doc.go` so new types follow it |
| Paging four ways: `cursor` → `next` (string) in `list`; `before` → `earlier` and `total` in `comments`/`history`; `after` (int) → `next` (int) in `conflicts`; `after` (a name) → `more` (a count) in the admin lists | `ops.go:366-378`, `:516-533`; `conflicts.go:40-86`; `admin.go:87` | keep: each is the right cursor for its list, and 1.x can add one compatibly. Document the four in `doc.go` |
| `since`: a duration (`who`), a time, date or duration (`digest`, `dates`, `cost`) | `ops.go:43`, `:683`; `gates.go:122`; `cost.go:87` | keep; accepting a time in `who` later is compatible |
| Durations: strings (`15m`, `7d`) for leases; integer `seconds` for hours; `*_seconds` integers; `last_step_seconds` a float; `p50_us`, `p99_us` in microseconds | `ops.go:544`, `:582`; `hours.go:28`; `admin.go:421-589` | keep: strings where a person types them, numbers where a program reads them, the unit in the name |
| Money: `cost_usd` and `amortized_usd` as decimal strings; `Rates.*` and `PlanSetArgs.fee` as integer micro-dollars | `ops.go:273`; `cost.go:41-119`; `hours.go:97` | rename the integer fields to say their unit (`input_micro_usd`… or a `_micros` suffix), or document it in the type. The two forms are each right; the trap is a bare `fee` |
| `change` takes added/replaced/unchanged (prices, plans), added/changed/unchanged/pending (members), and another set for keys | `cost.go:60`; `hours.go:107`; `admin.go:232`, `:337` | keep; document each set in `doc.go` |
| `DigestTotals` mixes counts always present with later ones that are `omitempty` | `ops.go:706-726` | keep: a client ignores a missing count as zero; document that a missing total is zero |
| `Issue.expires_at`, `ephemeral`, `pinned`, `template`, `metadata`: set only by `import-bd`, read by no client | `ops.go:83-87`; `internal/server/dispatch.go:701`; `internal/bdimport/record.go:242`, `export.go:102` | remove from the wire before the settle (question 8). `export-bd` reads them from the store, not the protocol; `show --json` exposes fields nothing can set, and once frozen they stay |
| `Summary.overlaps` is a list of ids, `Files.overlaps` a list of `{id, by, session}` | `ops.go:103`; `paths.go:75-90` | keep; they summarize at two depths |

### 4.4 Error codes

`internal/proto/errors.go:9-42`. Each is named in the guides where it
applies (`docs/cli.md`, `docs/server.md`, `docs/agents.md`).

| Code | Finding |
|---|---|
| `not_found`, `conflict`, `exists`, `cycle`, `invalid`, `acceptance`, `forbidden`, `unavailable`, `busy`, `stale` | keep |
| `version` | keep; exit code 3 |
| `auth` | keep the code; its wording names the SSH host key, settled elsewhere (§10) |
| A missing `.aries.yaml` is `invalid` | `internal/cli/cli.go:411-447` | keep. No guide lists every code in one place, though: add a table to `docs/cli.md` §Exit codes (document) |
| The `Fix*` phrases the MCP server matches on are wire text | `errors.go:79-129` | keep, and say in `doc.go` that changing one is a protocol change, since an older `aries mcp` matches the text |
| `AtConnLimit` parses the message | `internal/proto/connlimit.go:29-44` | as above |

### 4.5 Frames and events

| Item | Defined | Finding |
|---|---|---|
| Frame types `bridge`, `hello`, `welcome`, `req`, `res`, `evt`; `MaxFrame` 4 MiB | `internal/proto/frame.go:15-32` | keep; `bridge` is settled elsewhere (§10) |
| Push events `inbox`, `resync`, `event` | `internal/proto/inbox.go:17-27` | keep |
| `tp`, `ts` trace context on a request | `frame.go:39-78`; `internal/proto/trace.go:45` | keep |
| Event kinds clients see: `issue.create`, `issue.update`, `issue.close`, `issue.reopen`, `issue.paths`, `issue.kept`, `label.add`, `label.remove`, `dep.add`, `dep.remove`, `acceptance.tick`/`untick`/`waive`, `hours.log`, `hours.delete`, `conflict.park`/`propose`/`resolve`, `claim.take`/`expire`/`release`/`away`/`lapsed` | `internal/proto/event.go:18-78` | keep. Events say `label.remove` where the op says `label.rm`; events are history and the ops are calls, so they may differ, but fix it now if `rm` is renamed (question 4) |
| The event's kind is `op`; an inbox item's is `kind` | `ops.go:138`; `inbox.go:34` | keep: an event records an op |
| Inbox kinds: `claim.lost`, `gate.pending`, `gate.passed`, `gate.failed`, `lock.granted`, `lock.lost` (noun.verb) beside `assigned`, `mention`, `handoff`, `conflict` (bare) | `internal/store/inbox.go:38-52`, `internal/store/gates.go:72-78`, `internal/store/locks.go:88-91`; listed at `internal/proto/inbox.go:29-31` | decide (question 6): agents and scripts filter on these. Recommend renaming the four bare ones to `issue.assigned`, `issue.mentioned`, `issue.handoff` and `conflict.parked` before the settle; the cost is the four constants, their tests and `docs/agents.md`'s inbox section |
