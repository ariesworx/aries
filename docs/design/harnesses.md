# Headless harnesses beyond Claude Code

Status: **research, 29 October 2026** (ar-uhs4k7ja). It extends
[dispatcher.md §10](dispatcher.md#10-harnesses) to Cursor's CLI, Gemini
CLI and OpenAI's Codex CLI, and proposes how their usage reaches Aries
without Claude Code's usage hook (dispatcher.md §10, Q8). It decides
nothing and changes no code. Open-weight models, and who holds the
credentials for them, are ar-hcjyxibf's, not this note's.

## 1. How this was done

Each CLI was installed on the maintainer's macOS machine, none of them
signed in: Codex CLI 0.165.0, Gemini CLI 0.65.0 and Cursor's
`cursor-agent` build 2026.10.22. Each was run in a scratch git
repository with a throwaway prompt, in `CODEX_HOME` of its own for
Codex, up to the point where the provider refused the missing
credential. So the flags, the first events, the files written before the
first request, the hooks that fire before it, and the errors for a
missing credential were **tried**; nothing that needs a model's answer
(a token count, a tool call, a resume of a real session) was. Every other
fact was **read** in the provider's documentation or source on 29
October 2026, and is cited. In the tables, *tried* and *read* say which.
No client was pointed at an Aries server.

What a slice must try before it builds on a row: a signed-in run's token
counts against the provider's console, a resume, and a hook that denies a
tool call.

## 2. Launch, session and permissions

| | Codex CLI | Gemini CLI | Cursor CLI |
|---|---|---|---|
| Headless command | `codex exec [PROMPT]`, the prompt from stdin with `-` or none (*tried*: it reads stdin) | `gemini -p PROMPT`, appended to stdin (*tried*) | `cursor-agent -p PROMPT` (`agent` is the same binary) (*tried*) |
| Machine output | `--json`: JSONL events `thread.started`, `turn.started`, `turn.completed` (with `usage`), `turn.failed`, `item.*`, `error` (*tried* up to `turn.failed`; [exec_events.rs][codex-events]) | `--output-format json`, one document with `session_id`, `response`, `stats`, `error`; or `stream-json`, NDJSON `init`, `message`, `tool_use`, `tool_result`, `result` (*tried*: json; [headless][gemini-headless]) | `--output-format json`, one `result` object; or `stream-json`, NDJSON `system/init`, `assistant`, `tool_call`, `result` ([output format][cursor-output]) |
| Model | `-m MODEL`; with none, the account's default, recorded in the rollout's `turn_context` (*tried*) | `-m MODEL` (*tried*: the flag) | `--model MODEL`, `--list-models` (*tried*: the flag) |
| Exit on a missing credential | 1, after `turn.failed` whose message names `401 Unauthorized`, after about 15 seconds of retries (*tried*) | 41, with `error.code` 41 in the JSON document (*tried*) | 1, with a plain-text `Authentication required` on stderr, no JSON even under `--output-format json` (*tried*) |
| Other exits | 0 or 1 only ([lib.rs][codex-lib]) | 42 input, 44 sandbox, 52 config, 53 turn limit, 54 tool, 55 untrusted folder, 130 cancel ([errors.ts][gemini-errors]) | Not documented beyond nonzero ([output format][cursor-output]) |
| Pick the session id | **No.** The id is a UUIDv7 Codex makes, the first event's `thread_id` (*tried*); no flag sets it ([cli.rs][codex-cli]) | **Yes**, `--session-id UUID`, echoed as `session_id` (*tried*); not combined with `--resume` | **In two steps**: `create-chat` prints a new chat's id (*tried*: it printed one without a sign-in, then did not exit until killed); that `-p --resume ID` then runs in it is not documented |
| Resume for rework | `codex exec resume ID [PROMPT]`; the sandbox and approval policy come from the new command line, not the session's, so a resume passes them again ([lib.rs][codex-lib]) | `--resume UUID`, or `latest`, or an index ([session management][gemini-sessions]) | `--resume CHAT_ID`, `--continue` for the last ([parameters][cursor-params]) |
| Prompts off | `codex exec` never asks: approval is `never` (*tried*: the rollout records `approval_policy: never`) | `--approval-mode yolo`; a rule that asks is a deny under `-p` ([policy engine][gemini-policy]) | `--force`, which still honors explicit denies; `-p` needs it to edit files ([parameters][cursor-params]) |
| Sandbox | `-s read-only` (the default), `workspace-write`, `danger-full-access` (*tried*: the flag) | `-s` (*tried*: the flag) | `--sandbox enabled\|disabled`, undocumented beyond that |
| Deny list | Execpolicy `.rules` files: `prefix_rule(pattern=[…], decision="forbidden")` on command prefixes, the strictest match winning; a plain `a && b \| c` is split, anything with redirection or substitution is matched whole ([execpolicy][codex-rules]). No rule names a path: paths are the sandbox's and permission profiles' | Policy TOML, `--policy FILE` (*tried*: the flag): `[[rule]]` with `toolName`, `commandPrefix` or `commandRegex`, `argsPattern`, `decision = "deny"` and a tier and priority; a user-tier deny outranks yolo's allow-all, by the tiers' order, not a stated rule ([policy engine][gemini-policy]) | `permissions.deny` in `.cursor/cli.json` or the global `cli-config.json` (dir `CURSOR_CONFIG_DIR`): `Shell(git:*)`, `Write(glob)`, `Read(glob)`; deny beats allow ([permissions][cursor-perms]) (*tried*: the global file's shape) |
| Trusting the worktree | A project's `.codex/` (its config, rules and hooks) loads only once its path is trusted in `config.toml`'s `[projects."PATH"] trust_level = "trusted"` (*tried*: a project hook ran with that and not without it; `-c projects…` on the command line did not do it) | `GEMINI_CLI_TRUST_WORKSPACE=true` or `--skip-trust`; untrusted is exit 55 ([configuration][gemini-config]) | `--trust` (*tried*: the flag) |

**How the guard's lists map** (`internal/guard/coarse.go`). Claude Code
gets them as `--allowedTools` and `--disallowedTools`. For the others,
the dispatcher would write the coarse copy in each harness's own form,
in a configuration of the run's own, never the person's or the
repository's:

- **Codex**: a `CODEX_HOME` per run, under the dispatcher's directory,
  holding `config.toml` (the worktree trusted, telemetry as §4 says),
  `rules/aries.rules` (each `run:` deny as a `forbidden` prefix rule)
  and `hooks.json` (prime and the guard). The key comes from
  `CODEX_API_KEY` in the environment, so the directory holds no
  `auth.json`. Flags and refspecs anywhere in a command do not fit a
  prefix rule; those, and every `path:` rule, are the PreToolUse guard's
  alone. The project's own `.codex/` is loaded too once its path is
  trusted, so the per-run `config.toml` trusts it only when its
  `.codex/` holds nothing but what `aries setup codex` writes, and
  otherwise the run is refused.
- **Gemini**: a policy file per run, passed with `--policy`: each `run:`
  deny as `commandPrefix`, each `path:` deny as `argsPattern` on
  `write_file` and `replace`, at a priority above the yolo rule's 998.
- **Cursor**: a `CURSOR_CONFIG_DIR` per run whose `cli-config.json`
  carries the deny list as `Shell(…)` and `Write(…)`, so nothing is
  written in the worktree.

None of these reads past a wrapper (`env`, `sh -c`) or a variable the
way the guard does, so each stays the coarse layer, and the exact check
is the PreToolUse hook (§3) where the harness runs one.

## 3. Hooks

| | Codex CLI | Gemini CLI | Cursor CLI |
|---|---|---|---|
| SessionStart (`prime`) | Fires under `codex exec`, from `CODEX_HOME/hooks.json`, and from a trusted project's `.codex/hooks.json`, with `--dangerously-bypass-hook-trust` (*tried*); its input carried `session_id` (the thread id), `transcript_path` (the rollout), `cwd`, `model`, `permission_mode` and `source: startup` (*tried*). Without that flag a hook runs only once trusted by its hash in `/hooks` ([hooks][codex-hooks]) | Did not fire before the missing credential stopped the run, from a trusted project's `.gemini/settings.json` (*tried*); whether it fires under `-p` once signed in is not documented | `sessionStart` in `.cursor/hooks.json` or the user's; whether the CLI runs it headless is documented only in forum reports ([hooks][cursor-hooks]) |
| PreToolUse (the guard) | `PreToolUse` blocks with `permissionDecision: "deny"`, `decision: "block"` or exit 2; covers shell, `apply_patch` and MCP tools ([hooks][codex-hooks]; [guard.md §9](guard.md#9-codex-gemini-and-the-others)) | `BeforeTool` blocks with `decision: "deny"` or exit 2 (guard.md §9) | `preToolUse` and `beforeShellExecution` deny with `permission: "deny"` or exit 2 ([hooks][cursor-hooks]) |
| A hook for usage | `Stop` and `SessionEnd`, with `transcript_path` | `AfterAgent` and `SessionEnd`, with `transcript_path` | `stop`, with `transcript_path` |
| What `aries setup` writes | SessionStart in `.codex/hooks.json` | SessionStart in `.gemini/settings.json`, one group per source | `sessionStart` in `.cursor/hooks.json`, version 1 |

## 4. What each records

| | Codex CLI | Gemini CLI | Cursor CLI |
|---|---|---|---|
| Transcript | `CODEX_HOME/sessions/YYYY/MM/DD/rollout-TIME-ID.jsonl`, written before the first request, mode 0644 (*tried*). Lines `{timestamp, type, payload}`: `session_meta` (id, cwd, `cli_version`, `originator: codex_exec`, `source: exec`), `turn_context` (model, approval and sandbox policy), `response_item`, `event_msg` (*tried*); the format is not documented. Also a SQLite `state_5.sqlite` whose `threads` table has `model`, `tokens_used`, `cwd`, git branch and sha (*tried*: the schema) | `~/.gemini/tmp/PROJECT/chats/`, JSONL per session, each Gemini message with `tokens` and `model` and its tool calls ([chatRecordingService.ts][gemini-chats]); kept 30 days by default. A failed sign-in left only the project's directory (*tried*) | `~/.cursor/chats/HASH/ID/`, by a Cursor reply on its forum ([forum][cursor-chats]); format undocumented. A failed sign-in left an empty directory under `~/.cursor/projects/` (*tried*) |
| Tokens | `turn.completed.usage`: `input_tokens`, `cached_input_tokens`, `cache_write_input_tokens`, `output_tokens`, `reasoning_output_tokens`, per turn ([exec_events.rs][codex-events]); in the rollout, each `token_count` event's `last_token_usage` and `total_token_usage` ([protocol.rs][codex-protocol]) | `stats.models[NAME].tokens`: `prompt`, `input` (prompt less cached), `cached`, `candidates`, `thoughts`, `tool`, `total`, per model for the session ([headless][gemini-headless]) | **None** in the CLI's output or documented files; per request only in the team Admin API's usage events ([admin API][cursor-admin]) |
| Cost | None | None | None in the CLI; `chargedCents` per event in the Admin API, and an estimate in Enterprise telemetry |
| Model | `turn_context.model` per turn (*tried*) | Keys of `stats.models` | `system/init.model` in `stream-json` |
| Tool calls | `item.completed` of type `command_execution` (command, exit code), `file_change`, `mcp_tool_call` | `stats.tools`: calls, successes, failures, duration by name | `tool_call` events, started and completed |
| Timings | `task_complete.duration_ms` per turn in the rollout (*tried*) | `stats.models[…].api.totalLatencyMs`, tools' `durationMs` | `duration_ms`, `duration_api_ms` in the result |
| Rate limits | `token_count.rate_limits`: windows with `used_percent` and `resets_at` ([protocol.rs][codex-protocol]) | None | None |
| OpenTelemetry | `[otel]` in `config.toml`, off by default: logs `codex.api_request`, `codex.sse_event`, `codex.tool_decision`, `codex.tool_result` (with an output snippet); metrics `codex.turn.token_usage` by token type, durations; `metrics_exporter` defaults to `statsig`, OpenAI's; `log_user_prompt` adds prompts ([advanced config][codex-config]) | `telemetry.*` settings or `GEMINI_TELEMETRY_*`, off by default; `logPrompts` defaults to **true**; metrics `gemini_cli.token.usage`, `gemini_cli.api.request.count`, `gemini_cli.tool.call.count`; log `gemini_cli.api_response` with token counts and optional response text ([telemetry][gemini-telemetry]) | Enterprise only, set by a team admin, not the client: `cursor.token.usage`, `cursor.cost.usage`, `cursor.tool.calls` ([OpenTelemetry export][cursor-otel]) |

So Codex and Gemini each keep enough on disk for Aries's usage records,
and Cursor keeps none that is documented. Each transcript holds the
prompt and the code it saw, readable by the user's other processes
(Codex's mode 0644), as Claude Code's does; the dispatcher's run
directories should be 0700, and a per-run `CODEX_HOME` puts Codex's
there.

**Agent metrics** (`internal/agenttel`). For Codex, a per-run
`config.toml` can export metrics alone over OTLP to the same Collector
under `aries-agents`, with `log_user_prompt` off, logs off (the tool
result snippet is content), and `metrics_exporter` set away from its
default. For Gemini, `GEMINI_TELEMETRY_ENABLED`, `…_LOG_PROMPTS=false`
and the OTLP endpoint, metrics alone for the same reason. The test that
holds agenttel to no content needs each harness's list of variables
that add it. The agents dashboard reads Claude Code's series by name, so
each harness's series need queries of their own. Cursor's telemetry is
the team's, not the run's.

## 5. Credentials and terms

Bearings decision 14 and dispatcher.md §11 chose an API key for
unattended runs. Each provider's terms, as read:

| | API key | Subscription sign-in | For unattended, parallel runs |
|---|---|---|---|
| OpenAI (Codex) | `CODEX_API_KEY` for `codex exec`, or `codex login --with-api-key` reading it on stdin ([auth][codex-auth]) | ChatGPT sign-in, kept in `CODEX_HOME/auth.json` or the keyring; plan limits by five-hour and weekly windows | OpenAI's CI guide says automation should authenticate with an API key, and allows ChatGPT sign-in only on trusted private runners, one `auth.json` per machine or serialized job stream, never for concurrent jobs sharing one ([CI auth][codex-ci]). The dispatcher's parallel runs are such jobs |
| Google (Gemini) | `GEMINI_API_KEY` (paid or unpaid), or Vertex AI with a service account ([authentication][gemini-auth]) | Sign-in with Google, which **stopped serving requests on 18 June 2026** for Google AI Pro and Ultra and free Code Assist users; Code Assist Standard and Enterprise remain ([Google's notice][gemini-retire]) | Only an API key, Vertex or a Code Assist Standard or Enterprise seat is left. On the unpaid API, prompts may be used to improve Google's products and read by reviewers; on the paid API they are not ([Gemini API terms][gemini-terms]). Google names Antigravity CLI as Gemini CLI's successor |
| Cursor | `CURSOR_API_KEY`, a user API key from the dashboard, or a service account's key on Enterprise, which Cursor recommends for CI ([authentication][cursor-auth]); `--api-key` puts it in argv and is never used (rule 2) | `cursor-agent login` in a browser | The terms say nothing of headless or automated use; the account holder answers for all its activity, and the plan's usage pool is shared with the person's own ([terms][cursor-terms], [pricing][cursor-pricing]). Provider keys of one's own are documented for the editor only |

The key reaches each run as dispatcher.md §11 has it: from the
credential store into the child's environment, under a name of each
provider's (`openai-api-key`, `gemini-api-key`, `cursor-api-key`), and
never in argv. The auth failure each CLI prints (§2) already matches
`authWords` in `internal/dispatch/claude.go`, so the first one stops
the dispatcher as Claude Code's does.

## 6. Usage without a hook (Q8)

The `usage` op already takes what is needed: `UsageRecord` names its
`harness` (any 1–32 lowercase letters, digits or hyphens), a request id,
a model, a time, a granularity of `request`, `turn` or `session` and the
token counts, and the server keeps one record per session and id, so a
record sent twice counts once (`internal/store/usage.go`). Nothing in
the protocol changes. What is missing is a reader per harness and a
caller that is not a hook.

**Readers in `internal/capture`**, beside `Claude`, each decoding only
the fields of §4 and dropping the line, as `capture.Claude` does:

- `Codex`: the rollout's `token_count` events, each response's
  `last_token_usage` a `request` record, its id the thread id and the
  event's position in its turn, its model the turn's `turn_context`, its
  time the line's. OpenAI counts cached input inside `input_tokens` and
  reasoning inside `output_tokens`, so input is sent less cached and
  output as given; the slice checks both against the console.
- `Gemini`: the chat file's Gemini messages, each a `request` record by
  its message id; or, when the file is not found, the JSON result's
  `stats.models`, one `session` record per model, its id the session id
  and the model. Its `input` is already less cached, and output is
  `candidates` plus `thoughts`.
- `Cursor`: none. A Cursor run is counted by its time and its number,
  not its tokens, until a team's Admin API key is in the credential
  store, when a reader of `filtered-usage-events` by `conversationId`
  could send `session` records. That key reads the whole team's usage
  and is a second secret, so it waits for a team that asks.

**Two callers of the same readers.**

1. `aries usage --hook=codex|gemini`, on Codex's `Stop` and
   `SessionEnd` and Gemini's `AfterAgent` and `SessionEnd`, with the
   hook's `transcript_path`, as `aries usage --hook` is for Claude Code.
   `aries setup codex|gemini` writes them, and a person's own sessions
   are counted too.
2. The dispatcher, after each run's exit and at each fallback tick while
   it runs, reads the run's transcript from the offset it kept and sends
   it with `ARIES_SESSION` set to the run's session. It knows the
   transcript's path without a hook: the rollout under the run's own
   `CODEX_HOME`, named by the thread id of its log's first line; the
   Gemini chat of the session id it chose. This is the path that holds
   when a hook does not run (a changed hook not yet trusted in Codex,
   Gemini under `-p` if it skips them), and it is Bearings' capture
   (bearings.md §3.7.1). Both send the same ids, so a run read by both
   counts once.

**The budget.** The cap counts a day's Opus output tokens (rule 3,
`Config.OpusCap`). Other providers' models need caps of their own, by
model or in dollars once their prices are set with `price.set`, and a
harness with no tokens (Cursor) a cap on runs per day; the dispatcher
refuses to start a harness it cannot meter unless the configuration
says so.

## 7. What Aries needs, per harness

- **A launcher** beside `Claude` (`internal/dispatch/claude.go`)
  implementing the same `Launcher`: the argv of §2, the per-run
  configuration of §2, the brief on stdin, the run's log, and its own
  `Classify` (Codex's `turn.failed`, Gemini's exit codes, Cursor's
  plain text). `Adopt` finds a run by its session in the command line;
  a Codex run's argv carries no session, so it finds the run by
  `aries lock run`'s process and the worktree instead.
- **The session.** The run's Aries session stays the one the dispatcher
  picks, through `ARIES_SESSION`, which `prime --hook` and `aries mcp`
  already prefer over the harness's id; the harness's own id (Codex's
  thread id, Cursor's chat id) is kept in the run's state for the
  resume and the transcript.
- **Picking.** A label `harness:codex`, `harness:gemini` or
  `harness:cursor` picks the harness, Claude Code without one; the
  `model:*` labels map, per harness, to that provider's model ids in
  the dispatcher's configuration, and a harness with no map uses the
  account's default.
- **Briefs.** `internal/brief` gains a harness name for each
  (`codex`, `gemini`, `cursor`) and its line tag, and the template
  says what each lacks (no Auto-fix, the sweep instead). The `brief:`
  record's `harness=` and `model=` take the new names with no change.
- **The guard** in each harness's hook form (guard.md §9, which adds
  Cursor's), and `aries setup cursor` writes Cursor's.
- **No protocol change** for any of this.

Order: Codex first, since it was tried furthest (its hooks fire under
`exec`) and records the most; Gemini second, after checking whether the
maintainer would run it on a paid key while Google moves to Antigravity
CLI; Cursor last, since its runs cannot be metered. Each is its own
slice and issue: ar-owbab6v2 (Codex), ar-ennqoomf (Gemini, waiting on
the maintainer's choice of Gemini CLI or Antigravity CLI) and
ar-dnqnflbs (Cursor).

## 8. Sources

Read 29 October 2026.

[codex-events]: https://github.com/openai/codex/blob/main/codex-rs/exec/src/exec_events.rs
[codex-lib]: https://github.com/openai/codex/blob/main/codex-rs/exec/src/lib.rs
[codex-cli]: https://github.com/openai/codex/blob/main/codex-rs/exec/src/cli.rs
[codex-protocol]: https://github.com/openai/codex/blob/main/codex-rs/protocol/src/protocol.rs
[codex-rules]: https://github.com/openai/codex/blob/main/codex-rs/execpolicy/README.md
[codex-hooks]: https://learn.chatgpt.com/docs/hooks
[codex-config]: https://learn.chatgpt.com/docs/config-file/config-advanced
[codex-auth]: https://learn.chatgpt.com/docs/auth
[codex-ci]: https://learn.chatgpt.com/docs/auth/ci-cd-auth
[gemini-headless]: https://github.com/google-gemini/gemini-cli/blob/main/docs/cli/headless.md
[gemini-errors]: https://github.com/google-gemini/gemini-cli/blob/main/packages/core/src/utils/errors.ts
[gemini-sessions]: https://github.com/google-gemini/gemini-cli/blob/main/docs/cli/session-management.md
[gemini-chats]: https://github.com/google-gemini/gemini-cli/blob/main/packages/core/src/services/chatRecordingService.ts
[gemini-policy]: https://geminicli.com/docs/reference/policy-engine/
[gemini-config]: https://geminicli.com/docs/reference/configuration/
[gemini-telemetry]: https://github.com/google-gemini/gemini-cli/blob/main/docs/cli/telemetry.md
[gemini-auth]: https://geminicli.com/docs/get-started/authentication/
[gemini-terms]: https://ai.google.dev/gemini-api/terms
[gemini-retire]: https://developers.googleblog.com/en/an-important-update-transitioning-gemini-cli-to-antigravity-cli/
[cursor-output]: https://cursor.com/docs/cli/reference/output-format
[cursor-params]: https://cursor.com/docs/cli/reference/parameters
[cursor-perms]: https://cursor.com/docs/cli/reference/permissions
[cursor-hooks]: https://cursor.com/docs/agent/hooks
[cursor-chats]: https://forum.cursor.com/t/cursor-agent-create-chat-and-how-to-delete-old-chats/156640
[cursor-admin]: https://cursor.com/docs/account/teams/admin-api
[cursor-otel]: https://cursor.com/docs/enterprise/opentelemetry-export
[cursor-auth]: https://cursor.com/docs/cli/reference/authentication
[cursor-terms]: https://cursor.com/terms-of-service
[cursor-pricing]: https://cursor.com/pricing

- Codex CLI: [exec events][codex-events], [exec][codex-lib],
  [exec flags][codex-cli], [protocol][codex-protocol],
  [execpolicy][codex-rules], [hooks][codex-hooks],
  [advanced configuration][codex-config], [authentication][codex-auth],
  [CI authentication][codex-ci].
- Gemini CLI: [headless][gemini-headless], [exit codes][gemini-errors],
  [sessions][gemini-sessions], [chat recording][gemini-chats],
  [policy engine][gemini-policy], [configuration][gemini-config],
  [telemetry][gemini-telemetry], [authentication][gemini-auth],
  [Gemini API terms][gemini-terms], [the move to Antigravity CLI][gemini-retire].
- Cursor CLI: [output format][cursor-output], [parameters][cursor-params],
  [permissions][cursor-perms], [hooks][cursor-hooks],
  [chat storage][cursor-chats], [Admin API][cursor-admin],
  [OpenTelemetry export][cursor-otel], [authentication][cursor-auth],
  [terms][cursor-terms], [pricing][cursor-pricing].
