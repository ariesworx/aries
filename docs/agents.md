# Agents

Agents use Aries through MCP (Model Context Protocol) and never need a
shell. `aries mcp` is the MCP server: the app that runs the agent (its
harness, such as Claude Code) starts it, and it talks to the Aries
server for the agent. This page covers setting agents up, the tools they
get and how sessions are identified. Claims, handoffs and the inbox are
explained in [How Aries works](concepts.md).

## Set up an agent

Run setup once per repository, inside a repository that has a
[`.aries.yaml`](cli.md#connect-a-repository), and commit the files it
writes:

| Command | Does |
|---|---|
| `aries setup claude-code` | Print what it would write, and where |
| `aries setup claude-code --write` | Write it; safe to rerun |
| `aries setup codex --check` | Fail, with a fix, if anything is missing |
| `aries setup codex --remove` | Take Aries out again |
| `aries setup --all --write` | Every agent below at once, one summary line each |

Setup writes three things for each agent: the MCP config that starts
`aries mcp`, a short pointer in the agent's instruction file, and a
session-start hook. For Claude Code the hook file also gets the
[token usage hooks](#token-usage-hooks-claude-code):

| Agent | MCP config | Pointer | Hooks | You still |
|---|---|---|---|---|
| `claude-code` | `.mcp.json` | `CLAUDE.md` | `.claude/settings.json`: session start, and usage on Stop, SubagentStop and SessionEnd | approve the project's MCP server when Claude Code asks |
| `codex` | `.codex/config.toml` | `AGENTS.md` | `.codex/hooks.json` | trust the project, and the hooks with `/hooks` |
| `gemini` | `.gemini/settings.json` | `GEMINI.md` | `.gemini/settings.json` | trust the folder when Gemini CLI asks |
| `cursor` | `.cursor/mcp.json` | `.cursor/rules/aries.mdc` | `.cursor/hooks.json` | turn Aries on in Cursor Settings › MCP |
| `vscode` | `.vscode/mcp.json` | `.github/copilot-instructions.md` | `.github/hooks/aries.json` (Preview) | trust the server; hooks run only where VS Code's Preview hooks are on |
| `junie` | `.junie/mcp/mcp.json` | `AGENTS.md` | `~/.junie/config.json`, with `--global` only | run `aries setup junie --global --write` for the hook |
| `jetbrains` | none: added in the IDE | `.aiassistant/rules/aries.md` | none | add the printed JSON under Settings › Tools › AI Assistant › Model Context Protocol (MCP), scope Project |

Setup prints the "you still" step after it adds a part.

### What setup writes, and what it leaves alone

- **The pointer** is a short block between `<!-- aries:begin -->` and
  `<!-- aries:end -->`. It tells the agent to use the Aries tools and to
  `prime`, `start` and `finish`. The rest of the file is left alone.
  Cursor's and AI Assistant's pointers are rule files of Aries's own.
  Codex and Junie share `AGENTS.md`, so `setup --all` writes the block once,
  and removing either agent takes it out.
- **The MCP entry** holds `command`, `args` and `ARIES_HARNESS` in its
  env (and `"type": "stdio"` for Claude Code and VS Code), and nothing
  else. `--check` fails on any extra key or variable
  (`cwd`, `PATH`, `LD_PRELOAD` and the like), and `--write` drops them.
  The one exception is `ARIES_KEY_FILE`, which `--key-file PATH` adds
  to an entry in your own files and never to a project's
  ([Give an agent its own identity](#give-an-agent-its-own-identity)).
- **Existing files** keep their other keys, their order and their mode, and
  a second run changes nothing. A JSON file with a duplicate key is refused,
  since the harness would run the last copy, and so is one with comments
  or text after the closing brace, which a rewrite would lose. Setup edits
  only Aries's own tables in Codex's `config.toml`; if it cannot read the
  file safely, it refuses and prints the snippet to add by hand.
- **`--remove`** takes Aries's parts out. A pointer file left empty is
  deleted, and so is VS Code's hook file; an MCP config or a shared hook
  file stays, even when empty.
- **Symbolic links** in the repository are refused, file included, so a
  cloned repository cannot point `.mcp.json` at a secret or `.claude` at
  your home directory. New project files are 0644.
- **`--command PATH`** sets how the agent runs `aries` when it is not on
  `PATH`.

### Setting agents up for every repository (`--global`)

`--global` edits the files in your home directory instead of the
repository's: for Claude Code `~/.claude.json`, `~/.claude/CLAUDE.md` and
`~/.claude/settings.json`; for Codex, Gemini CLI and Junie their
`~/.codex/`, `~/.gemini/` and `~/.junie/` files; for Cursor the MCP config
and hook. Nothing outside the repository is touched without it. New files
in your home directory are 0600, in 0700 directories. A home file may be a
symbolic link (a dotfiles repository) only to something of yours inside
your home directory; setup edits the target and keeps the link. VS Code and
AI Assistant keep their user settings elsewhere, so `--global` is refused
for `vscode` and `jetbrains`, and `--all --global` skips them.

A global hook runs in every repository with a `.aries.yaml` that you
open with the agent, and connects to the server that file names; Claude
Code's usage hooks send that server the session's token counts. Prefer
per-project setup, or turn the global hooks off before you open a
repository you do not trust.

### Claude Desktop

Claude Desktop (macOS and Windows) has no project files, hooks or working
directory. Run `aries setup claude-desktop --write` inside the repository,
then quit and reopen Claude Desktop. It adds one entry per project to the
app's config:

| OS | Config |
|---|---|
| macOS | `~/Library/Application Support/Claude/claude_desktop_config.json` |
| Windows | `%APPDATA%\Claude\claude_desktop_config.json` |

The entry is named `aries-<directory name>` and runs
`/absolute/path/to/aries -C /path/to/repo mcp`, because the app starts
servers without your shell's `PATH`. The path is your `aries` on `PATH` (for
example Homebrew's link, which survives upgrades) or else the running
binary; `--command` must be absolute. Several projects coexist, and
`--check` and `--remove` touch only this checkout's entry. `--all` leaves
Claude Desktop out, because its file is outside the repository. With no
pointer or hook, the agent learns Aries from the MCP server's
instructions.

## Session-start hooks

Each hook runs `aries prime --hook=AGENT` (Claude Code's runs `--hook` alone)
when a session starts, and replies in the harness's own format. Gemini CLI
matches sources exactly, so it gets one entry each for `startup`, `resume`
and `clear`; Codex's matcher is `startup|resume|clear|compact`.

The hook reads the session id from the harness's input, adds `prime`'s
orientation to the session's context, and prints nothing outside an
Aries repository. On any error it adds a one-line note instead of
failing the session.

A hook belongs to Aries only when its whole command is the one setup
writes, `aries prime --hook[=AGENT]` or `aries usage --hook[=AGENT]` through
`aries` or the `--command` program. Other hooks, including your own
commands that end in `aries prime --hook`, are never touched.

## Token usage hooks (Claude Code)

Aries reports the tokens each issue took
([Accounts, time and tokens](concepts.md#accounts-time-and-tokens)).
Only Claude Code's are captured so far; Codex, Gemini CLI and the others
follow once their formats are checked against real files. Every harness
still gets the time each issue was held.

Setup adds `aries usage --hook` to Claude Code's Stop, SubagentStop and
SessionEnd hooks. Stop and SubagentStop run with `"async": true`, so the
turn never waits on the server, and a 30-second timeout. SessionEnd runs
synchronously with a 10-second timeout: it sends the session's last
response, and Claude Code waits for it as it exits, where an async hook
might not finish. Each run:

- reads the session's transcript, named by the hook's `transcript_path`,
  and its subagents' transcripts beside it
  (`<session id>/subagents/agent-*.jsonl`);
- reads each file from where the last run stopped, up to its last
  complete line. The offsets are kept in `aries/usage-offsets.json`
  under your user cache directory (`~/.cache` on Linux,
  `~/Library/Caches` on macOS, `%LocalAppData%` on Windows), mode 0600 in
  a 0700 directory. A file that was truncated or replaced is read from
  its start;
- sends one record per API request: the harness, the message id, the
  model, the time, and the input, output, cache-write (with its one-hour
  part) and cache-read counts. A count the transcript lacks is sent as
  unknown, not 0. The offsets move only once the server has accepted the
  records, so a failed send is retried by the next hook, and the server
  keeps one record per request, so nothing is counted twice;
- waits for a response to finish. Claude Code writes a response one line
  per content block, its output count growing to the last line's, and
  tools run while it streams, so tool results can fall among its lines.
  A response is sent only once a line of the next response follows it,
  since the next request starts only after it ends. A file's last
  response waits for the next hook; SubagentStop sends the stopped
  subagent's, and SessionEnd whatever is left;
- drops a record the server refuses as invalid, such as one dated more
  than five minutes past the server's clock, so it cannot hold back the
  records after it, and says so. A server that is busy or unreachable
  drops nothing: the next hook sends the records again.

**Only counts leave your machine.** The transcripts hold your whole
conversation, but `aries` reads them locally and keeps only the fields
above; no prompt, reply, tool input or file content is sent, stored or
printed. The connection is the same SSH connection every `aries` command
uses.

The hook exits 0 whatever happens and prints nothing outside an Aries
repository or when there is nothing new. Otherwise it writes one line to
stderr with a fix. Claude Code keeps hook output out of the
conversation, so the line may go unseen; `claude --debug` shows it.

If Claude Code changes its transcript format, `aries` never guesses
counts. When none of the response lines new in a file can be read, the
hook sends nothing for it, says the format was not recognized, names the
Claude Code version, and keeps the file's offset, so an upgraded `aries`
reads those lines. Lines it cannot read among lines it can are skipped,
and the offset moves past them: an upgrade does not recover those.

An older setup upgrades with `aries setup claude-code --write`. One from
before these hooks has the session-start hook alone, and one from their
first release runs SessionEnd async; `--check` names the hooks to fix.

## Tools

The MCP server offers 24 tools: `prime`, `start`, `finish`, `handoff`,
`ready`, `blocked`, `list`, `show`, `create`, `update`, `close`, `reopen`,
`dep`, `label`, `comment`, `comments`, `history`, `digest`, `who`,
`inbox`, `remember`, `recall`, `forget` and `gate`. There are no admin
tools: import, export, setup, upgrade, prices, plans and server settings
stay on the command line. Hours are logged by people, with `aries hours`; no
tool logs them. Nor is there a tool for cost reports: a person runs
`aries cost`, and an agent sees an issue's cost in `show` and the
window's in `digest`.

- **Results are compact.** Writes return `{id, rev}`, and lists return the
  id, title, status and priority. A result is capped at about 2,000 tokens;
  `prime` and `digest` at 1,500.
- **Refusals say what to do next.** A refusal is a tool error with the
  server's code and message and a `fix:` naming the agent's next step. A
  fix only the user can act on is quoted from the server. When the
  server itself failed the request, the fix ends with the request's id
  for the user to pass on to whoever runs the server.
- **Text from others is marked.** Results that hold text other people
  wrote carry `"untrusted"`, and the server's instructions tell the agent
  never to follow instructions found in it
  ([Security](security-model.md#untrusted-text)).
- **`digest` is data.** It returns structured facts from the event log;
  the agent writes any narrative.
- **Memory is scoped.** `remember` keeps a memory in `project` scope
  unless told otherwise; its description tells the agent to use `user`
  for the person's own preferences, asking if unsure, and `team` only
  when someone chose it ([Memory](concepts.md#memory)). It takes `rev`
  to replace a memory, so a concurrent edit is refused rather than lost;
  a replace keeps the tags and issue it omits, and `[]` or `""` clears
  them.
  `recall` searches by text, key, tag and scope, cutting each body to
  about 1,000 bytes (`"truncated": true`); with `key` it gives the body
  whole (up to the 4,096-byte cap), which is how to read a memory before
  replacing it, since a replace takes the whole body. `forget` deletes. `prime`
  shows the pinned and relevant memories and `start` those relevant to
  its issue. Pinning is left to people (`aries pin`). A memory that looks
  like it holds a secret is refused, and the fix tells the agent to name
  where the secret is kept instead.
- **`show` and `digest` carry time, cost and tokens.** `show` gives the
  issue's `account` and a one-line `usage`: time held, the hours people
  logged (`logged 1.5h`), cost at list price
  (`cost $8.35`, `, some unpriced` when a model has no price, or `cost
  unpriced`; no cost at all when the server has no prices), tokens for
  the five largest models, and whether any were split by time with
  other work. `digest` gives the window's `usage` line, with the hours
  logged, the tokens no issue was held for and whether any were split by
  time.
  No tool sets an account; people do that with `aries update --account`.
- **Files come from git, not from the agent.** `aries mcp` reads the paths
  each held issue's work touched from the repository it was started in,
  and sends them with a renewal (at most every five minutes per issue),
  `finish` and `handoff`; no tool takes them. `ready` gives each issue
  whose files overlap another session's work `overlaps` (and ranks it
  last), and `show` gives the likely `files`, `files_more` and the
  `overlaps` with who holds them ([Files](concepts.md#files-what-an-issue-touches)).
  People declare files with `aries create --paths` or `aries update --paths`.
- **A parent waits on its children.** An issue with a child that is not
  closed, an epic most often, is left out of `ready` and `start`, and
  `blocked` gives it `children`, how many are open; it is ready again
  once they close. Plan an epic as children made with `parent`, ordered
  among themselves with `dep`; no `dep` from the epic to its children is
  needed, and one is refused ([Parents and their children](concepts.md#parents-and-their-children)).
- **Agents propose, people resolve conflicts.** See [Conflicts](#conflicts).
- **Agents hold work back; people let it go.** See [Gates](#gates).
- **Dates.** `create` and `update` take `defer_until` (not ready before)
  and `due`, each a date (`2026-11-02`) or an RFC 3339 time; with
  `update`, `""` clears one. `prime` lists, at most three each, the
  issues `overdue`, `due_soon` (within the server's `due_soon`, a day
  by default) and `ready_by_date` (ready in the last day because their
  date came), and `digest` the same for its window. A date ends,
  expires and reassigns nothing ([Dates](concepts.md#dates)).
- **The workflow is the project's, not the agent's.** Where a project
  admin set a [workflow](concepts.md#the-workflow), every issue an agent
  creates, or files through `finish`, gets its type's acceptance items
  after the agent's own, and `prime` says so in one line (`workflow rev
  4: bug, feature, task: 4 items, landed; …`). An agent ticks or waives
  them as any item, and cannot drop one; no tool sets or reads the
  workflow, and `aries admin workflow set` refuses an agent's session.
  Where the workflow requires a check such as `landed`, `finish` is
  refused (`acceptance`) until the check passes: the agent calls
  `handoff` with state `done`, the pull request's link and `ticked`
  (the items it met, and `waived` with a reason for any it will not)
  instead, which settles those items in the handoff's own write, and
  CI's pass then closes the issue with nothing more from the agent. If
  an item is still open then, the pass says so in the agent's `inbox`,
  after which `finish` with the items ticked closes it. `ticked` and
  `waived` on `handoff` need state `done`; before protocol 9 (v0.5.0)
  only `finish` took them, so an agent that handed off without them left
  its issue open after landing. `prime` lists such issues as `awaiting landing`, with what
  each waits for ([Required checks](concepts.md#the-workflow)).
  Where it requires a review, `finish` is refused the same way until a
  person passes the review gate from their own terminal: the agent
  hands off, tells the user the issue waits for the approvers the
  refusal names, and never runs `aries gate` itself; its `inbox` says
  when the review passed. An epic created under an approval stays out
  of `ready`, with its children, until a person approves it.
  The profile's close-out lines come as a short `closeout:` list after
  the result line of `handoff` (state `done`), `finish` and the `close`
  of an epic, and in `digest` once an epic's last child has closed:
  what to tidy up (a compose stack, scratch files, worktrees,
  sessions). They are shown, never ticked, and are dropped before the
  result is, if its 2,000 tokens are short.

One SSH connection serves an MCP session. It opens on the first tool call
and is redialed if it drops. Reads, `create`, `comment`, `finish`,
`handoff`, `inbox`, `remember` and `forget` are retried on the new
connection, since a retry cannot write twice. `start`, `update`, `close`, `reopen`, `dep` and
`label` report that they may have applied. A request the server has not
answered within 75 seconds counts as a dropped connection: the server
gives up on a request after a minute and answers that it did, so by then
the connection itself has stalled. `aries mcp` closes it, the call is
retried or reported as above, and the claims' renewal, which waits
behind the call, goes out on the next connection.

A person's sessions together hold at most `conns_per_principal`
connections, 64 unless the server says otherwise
([Limits](server.md#limits)). A session refused past it cannot reach the
server until one of them closes, and every tool call fails with:

```text
busy: alice is at its connection limit (64)
fix: the user's other aries sessions hold every connection the server allows them, so this session cannot reach it until one closes. Tell the user now: they can close some, or ask the server's operator to raise conns_per_principal. Until then, issues you hold go back to open unless renewed within their 15m lease, and writes are not kept for later; never run an admin command yourself
```

The session does not dial again for 5 seconds after such a refusal, so
a burst of calls costs the server one refused handshake; after that,
the next call or renewal takes a connection as soon as one is free, and
the session goes on as before. Writes are not queued in the outbox
here: `busy` is a refusal, not an outage.

### Conflicts

A queued update that arrives after someone else changed the same field
is parked as a conflict for a person to resolve
([Conflicts](cli.md#conflicts)). An agent hears of one through
`prime`, which counts the open conflicts on its principal's work, and
`inbox`, whose `conflict.parked` items name the issue. `show` gives the
issue's open `conflicts`: each one's `id`, `field`, `base`, `mine` and
`theirs` (cut to fit the result), who made each side, the server's
`advice`, and any proposal waiting.

The agent may propose a resolution with `comment`, giving `conflict`
(the conflict's id) and `propose`: `mine`, `theirs`, or the new text;
the `body` says why. It is kept with the conflict, replacing any earlier
proposal, and changes nothing. There is no resolve tool: a person
accepts the proposal with `aries resolve N --accept`, or resolves another
way (decision 42 of [the design](design/aries.md#14-decisions)).
Proposing is guarded as a change to the issue is, so it is refused on
an issue another principal holds. Proposing rides `comment` rather than
a tool of its own to keep the tools' schemas within their budget.

### Gates

A gate holds issues back until a person passes it
([Gates](concepts.md#gates)). The `gate` tool has two actions:
`add`, with `blocks` (the issue to hold; a gate on an epic holds the
whole plan), `approvers` (principals; none leaves it to the project's
admins) and an optional `title`; and `list`, the open gates with their
approvers and what each holds. A planning agent gates its epic before
filing the children, so none is started before a person approved the
plan, and every edge exists by then.

No tool passes, waives or rearms a gate, and none of the tools gets
around one: `start` of a held issue, and `close`, `finish`, `reopen`
or a status change of a gate, are refused `invalid`, and so are a
`close` or `finish` of a held issue, a `dep` removal of the edge, and an
`update` that moves a held issue out from under its gated parent. The
fix tells the agent to pick other work or tell the user, naming the
approvers. `show` gives an issue's `gates` that hold it; `blocked`
lists the issue with the gate among its blockers. An agent hears that
a gate passed through its `inbox` (`gate.passed`) when it is the
assignee of what the gate held. Running `aries gate pass` in the agent's
shell is refused too: a pass counts only from the person's own
terminal, the session `cli`, which an agent's harness session is not.

A [ci gate](ci.md) waits for CI to report a check, such as `build` or
`landed`, on the issue it names as its subject. An agent sees it and
cannot pass or fail it. `show` gives an issue's `checks`, one line each:
`build failed at abc1234: URL REASON (GATE)`, or `landed waits for
ci-example (GATE)`; a closed issue whose `landed` check waits has not
landed. The `gate` tool's `list` gives a ci gate's `check`, `subject`
and last `report`. When a check fails on an issue the agent holds, its
`inbox` gets a `gate.failed` item with the run's link and the reason:
fix the work and push, and CI's next pass closes the matter. `aries gate
pass` or `fail` from the agent's shell is refused, even with CI's key:
CI reports from a CI job's session, which an agent's harness session is
not.

### Locks

Agents do not take [locks](concepts.md#locks) themselves, and no tool
takes, renews or lists one (decision 70). Locks guard what runs around
an agent's work, a merge or a deploy, and the script, CI job or
orchestrator that runs it holds the lock, renewing it, with
`aries lock run NAME -- CMD`. An agent whose harness runs such a command
in a shell gets the lock that command takes, for that command's life
only, and nothing in the agent's tool results changes.

### When the server cannot be reached

When `create`, `comment`, `handoff`, `remember`, `finish`, `update` with
a `rev`, `dep` (adding) or `label` cannot reach the server, `aries mcp`
keeps the write in the outbox on the machine
([When the server cannot be reached](cli.md#when-the-server-cannot-be-reached))
and the tool returns one line, which is not an error:

```text
queued: aries cannot reach its server, so this finish waits on this machine and is sent when the server answers. Carry on, and do not repeat it.
```

Its structured content is `{"queued": true, "op": …, "entry": …}`, and
never an `id` and a `rev`. A queued `create` adds that the issue has no id
until then, so nothing can refer to it, and that the project's
[workflow](concepts.md#the-workflow) adds its items when it is sent: the
workflow in force at the replay applies. A queued `finish` of an issue
whose workflow requires a check that has not passed is refused when it
is sent, and waits in the refused list with the fix to hand it off. A `label` call of several labels
queues a write for each, and its line says how many. The agent is told
to carry on: it finishes what it holds and files what it finds.

A server whose sshd closes the connection before the SSH handshake, as
it does for a while to an address with failed connections, counts as
away too: writes are queued as above. Any other tool call fails with
`unavailable: HOST:PORT closed the connection before the ssh handshake`
and a fix telling the agent not to call aries tools again in a loop,
to carry on with the work it holds and to tell the user, quoting what
the person should do. `aries mcp` itself dials again no sooner than 30
seconds later: the calls in between, and the renewals, get the same
refusal without a connection.

An `update` without a `rev` reads the issue's rev first, which needs the
server: with the server away it fails, saying it was not queued and to
give `rev`. A queued `update` is replayed so that what someone else
changed meanwhile is parked as a [conflict](#conflicts) rather than
refused.

**A late finish.** While the server is away `aries mcp` cannot renew the
agent's leases, so a claim lapses 15 minutes after the last renewal and
the issue may be taken by another session. A queued `finish` or
`handoff` that arrives after that still applies if nobody took the
issue meanwhile, and the issue's history says the lease had lapsed. If
somebody did, or the issue was closed, it is kept as a comment on the
issue, a `finish`'s `discovered` issues are still filed, and nothing
else of it applies. The agent learns of it in its inbox, in the next
tool result's `inbox: N new` line and in `prime`: a `conflict.parked` item on
the issue, `your finish made at epoch 3, delivered late, was kept as
comment …: the claim was taken since, by bob/… at epoch 4`. Its next
step is to leave that issue alone: the new holder got an item too,
naming the comment, so the work done is not lost to them. A person can
extend every claim of theirs ahead of a planned absence with `aries
away`, which agents have no tool for.

Nothing else is queued. `start` fails, and tells the agent that it was
not queued and to go on with the work it holds: no claim is taken
without the server. A write the outbox refuses (text that looks like a
credential, an outbox that is full, a machine that has never reached the
server with this key) is an error whose fix is the agent's next step, or
to tell the user.

The session's next connection sends what waits first, in order, before
the call that opened it, for five seconds at most, and the rest in the
background between calls. A session that makes no call still connects,
when its renewal comes round each minute, if something waits; and each
renewal on a connection it has sends what an earlier round left, as
when another session of the principal on the machine was sending the
same outbox then. What a
session has not sent when it ends stays in the outbox, and the next
`aries` command or session of the same principal on that machine sends it.

There is no tool for the outbox, and the agent is never asked to manage
it. When the server refuses the principal the project as `forbidden`,
the tool call fails with that refusal, telling the agent to ask a
person, and what waits is marked refused for that person to retry or
drop. A person hears of it through `prime`: when writes wait, or the
server refused some, `prime` has one more notice, in the tool's result
and in the session-start hook's context alike:

```text
outbox: 1 write made while the server was away was refused and 2 wait to be sent: tell the user to run `aries outbox`
```

`aries --no-queue mcp`, or `ARIES_NO_QUEUE=1` in the server's
environment, turns the outbox off for a session: its writes then fail
when the server is away, as they did before. The hooks never queue and
never send what waits.

**Reads from the cache.** What a session reads of its own context is
kept on the machine as the server returned it: `prime`, `show` of each
issue it looked at, the issue `start` took, `inbox` and `digest`
([What is shown from the cache](cli.md#what-is-shown-from-the-cache)).
With the server away those tools answer from that copy, never while it
answers, and say so in one line after the result, within its budget:

```text
cached: aries cannot reach its server, so this is this machine's copy from 2026-10-13 14:02 UTC (3h old), and may be out of date
```

`prime` says it as its first notice instead, and leaves out the ready
issues: nothing can be started without the server. So the session goes
on with the work it holds, with its acceptance items and last handoff
(`show`) and the memories `prime` showed, and the SessionStart hook of a new session gets the
same prime from the cache, marked the same way. A tool that asks for
something the session never read fails as before.

`ready`, `blocked`, `list` and `who` are refused with the server away,
whatever was read before: from a copy, what others are doing now would be
wrong in the way that has two sessions take the same work. The refusal
says when the server was last reached, and its fix is to carry on with
the work the agent holds, which `prime` and `show` answer for. The cache
is on for every `aries mcp`, `--no-queue` or not; it is the principal's,
chosen as the outbox is, and is deleted from the machine when the server
refuses the principal the project as `forbidden`.

## Sessions

The server knows the principal from the SSH key. The session id comes
from the agent's environment:

| Agent | Session id |
|---|---|
| Claude Code | `CLAUDE_CODE_SESSION_ID`, which it sets |
| Codex, Gemini CLI, Cursor, VS Code, Junie, AI Assistant | none set; `aries mcp` picks one per process (`m-…`) |
| Claude Desktop | none set; each app launch is one `aries mcp` process, so one session (`m-…`) |
| A person's own `aries` commands | `cli`, one per machine |
| Any | `ARIES_SESSION`, if set, wins (for example in the MCP entry's `env`) |

The harness name comes from `ARIES_HARNESS`, which setup writes into the
MCP entry; failing that, `claude-code` when `CLAUDECODE=1` and `gemini`
when `GEMINI_CLI=1`.

The hooks take the session id from their input, unless `ARIES_SESSION`
is set. In Claude Code that id is `CLAUDE_CODE_SESSION_ID`, the session
`aries mcp` claims issues under, so the usage hooks' tokens go to the
issues that session held. If you set `ARIES_SESSION` for `aries mcp`, set
it for the hooks too.

### Trace context

A harness or a script that traces its own work can tie Aries's part to
it. `aries` passes a
[W3C trace context](https://www.w3.org/TR/trace-context/) to the server
with each request, and a server that exports traces
([Traces](server.md#traces)) starts its span for the request as a child
of the caller's. A server that does not still names the trace in its log
line for the request. `aries` itself starts no span and exports nothing.

| From | Read | Applies to |
|---|---|---|
| The environment: `TRACEPARENT` and `TRACESTATE` | Once, when `aries` starts | Every request of an `aries` command; in `aries mcp`, every request of a tool call that brings none of its own, and the renewals it makes between calls |
| A tool call's metadata: `traceparent` and `tracestate` in the request's `_meta` | At each call | The requests that one tool call makes of the server |

- **The metadata keys are the Model Context Protocol's own**: its
  specification reserves `traceparent`, `tracestate` and `baggage` in
  `_meta` for trace context, from the 2026-07-28 version on, after
  OpenTelemetry's convention for MCP. `aries mcp` reads the first two.
  `baggage` is not read or passed on.
- **Only a valid one is sent**: a version-00 `traceparent`, in lowercase
  hex, and a `tracestate` only with one. Anything else is left out, and
  never fails a command or a tool call: a call whose metadata is
  malformed falls back to the environment's.
- **No tool's schema or arguments changed.** It is metadata of the
  request, which a harness that traces adds by itself; an agent does
  not pass it, and cannot.
- **Of what `aries` sends, the server keeps the ids and the sampled
  flag.** The `tracestate` goes with the request and is put on no span
  and no log line.

## Give an agent its own identity

An agent started from your terminal has your environment: your home
directory, with its `~/.ssh`, and your ssh-agent socket. So by default it
signs in as you, with your rights, by the same
[order of keys](cli.md#your-key-is-your-identity) as your own `aries`. An
agent that works on its own should be a principal of its own that is no
admin ([Security](security-model.md#who-may-change-what)). To set that
up:

1. Make a key pair for the agent, with no passphrase, readable by you
   alone: `ssh-keygen -t ed25519 -N '' -f ~/.ssh/aries_agent`. Give it
   a name of its own, not a default one such as `id_ed25519`.
2. Have an admin add a principal for it with the public half: `aries admin
   principal add alice-agent --key ~/.ssh/aries_agent.pub`
   ([Managing access](server.md#managing-access)).
3. Start the agent with `ARIES_KEY_FILE` holding the private key's
   path:

   ```sh
   ARIES_KEY_FILE=~/.ssh/aries_agent claude
   ```

`aries` then offers that key and no other, whatever your `~/.ssh` and
ssh-agent hold, so every tool call and hook of that agent is the agent's
principal's. The variable holds the file's path, never the key.

### Where each harness gets the variable

`aries mcp` and the hooks are separate programs that the harness starts.
Each signs in with whatever `ARIES_KEY_FILE` its own environment
holds:

| Agent | `aries mcp` sees the variable | The hooks see it |
|---|---|---|
| Claude Code, Codex, Gemini CLI, Junie | when the harness was started with it (step 3, or your shell's profile), or from the entry `aries setup AGENT --global --key-file PATH` writes | only when the harness was started with it |
| Cursor, VS Code, AI Assistant | when the IDE was started with it. An IDE opened from the desktop does not read your shell's profile: start it from a terminal, or for Cursor use `aries setup cursor --global --key-file PATH` | only when the IDE was started with it |
| Claude Desktop | only from `aries setup claude-desktop --write --key-file PATH`: the app passes no environment on | it has no hooks |

The files setup writes into a repository are committed and shared, and a
key file's path is one person's, so setup never writes the variable
there: `--key-file` without `--global` is refused, except for Claude
Desktop, whose config is yours. In a project that commits its `.mcp.json`
and the like, each person sets the variable in the environment the
harness starts in.

`aries setup AGENT --global --write --key-file PATH` writes
`ARIES_KEY_FILE` into the `env` of your own MCP entry, beside
`ARIES_HARNESS`. The path is made absolute, must be a file, and is
refused unrepeated when it is plainly a key. `--check --key-file PATH`
checks for it; `--check` without the option fails on an entry that has
one and says which file it names. `--write` without the option replaces
the entry, and so takes the variable out, saying that it did. `--remove`
removes the entry and the variable with it.

**Hooks do not read the MCP entry.** With the variable only in the entry,
the agent's tool calls are the agent's principal's, while the
session-start and usage hooks sign in by your default keys, as you.
Nothing fails: the hook's `prime` text says `you are alice` where the
`prime` tool says `you are alice-agent`, `aries who` shows a session under
each, and the usage hooks send their token counts as you.
Setup says this when it writes the variable for an agent that has hooks.
To have both agree, start the harness with the variable set (step 3).

To check, read the first two lines of `prime`: the first names the
principal (`you are alice-agent`) and the second the key that signed in
and the file it came from. `aries whoami`, run in the same environment,
shows the same and more. If the harness does not pass its environment
on, `prime` shows your own name: then use setup's `--key-file`, or put
the key inside the agent's checkout and name it with `key:` in that
checkout's `.aries.yaml`, uncommitted.

## Connect an agent over HTTPS

An agent reaches the server the way you do: over HTTPS when the
repository's `.aries.yaml` names a `server.url`, and over SSH when it
names only a host. Both use the same key. Over HTTPS there is no password
and no token to set up: `aries` proves it holds the agent's key by
signing a challenge that names the server's URL, so the key never leaves
the machine and the proof cannot be replayed on another connection.

From a repository with a `.aries.yaml` that names the server:

1. Set up the agent once, as above: `aries setup claude-code --write`
   (or the agent you use), and commit the files it writes.
2. Make the agent's key and give it to an admin, as in
   [Give an agent its own identity](#give-an-agent-its-own-identity). The
   admin adds the principal with the public half, which is the only part
   the server keeps.
3. Start the agent with `ARIES_KEY_FILE` holding the private key's path,
   so that `aries` offers that key and no other
   ([Which key aries offers](security-model.md#which-key-aries-offers)).
4. Check from the agent's environment: `aries whoami` names the principal,
   the server, the transport (HTTPS) and the key it signed with. The first
   two lines of `prime` say the same.

If the server's certificate is issued by a company's or a site's own
authority, the repository's `.aries.yaml` names it with `server.ca`, a PEM
certificate or a file in the repository. The authority is added to the
system's roots for that server and replaces none of them
([The HTTPS listener](server.md#the-https-listener)). A certificate that
does not verify is refused with a fix that names `server.ca`.

To use SSH where both are named, set `ARIES_TRANSPORT=ssh` in the agent's
environment. `ARIES_TRANSPORT=https` insists on the URL and refuses to
fall back.

`aries login` is optional for an agent. It signs a person in through a
company identity provider, and an agent that holds its own key does not
need it. The provider's setup is in
[Sign-in](server.md#sign-in).

## Sessions and hooks in harnesses other than Claude Code

One limitation affects every harness except Claude Code. The session-start
hook learns the harness's session id from its input, but `aries mcp` cannot,
so it picks its own. The hook's `prime` and the agent's tool calls then
show as two sessions in `aries who`, and the hook's session holds no lease.
Claude Code avoids this because it sets `CLAUDE_CODE_SESSION_ID` for both.
