# Agents

Agents use starfix through MCP (Model Context Protocol) and never need a
shell. `sfx mcp` is the MCP server: the app that runs the agent (its
harness, such as Claude Code) starts it, and it talks to the starfix
server for the agent. This page covers setting agents up, the tools they
get and how sessions are identified. Claims, handoffs and the inbox are
explained in [How starfix works](concepts.md).

## Set up an agent

Run setup once per repository, inside a repository that has a
[`.starfix.yaml`](cli.md#connect-a-repository), and commit the files it
writes:

| Command | Does |
|---|---|
| `sfx setup claude-code` | Print what it would write, and where |
| `sfx setup claude-code --write` | Write it; safe to rerun |
| `sfx setup codex --check` | Fail, with a fix, if anything is missing |
| `sfx setup codex --remove` | Take starfix out again |
| `sfx setup --all --write` | Every agent below at once, one summary line each |

Setup writes three things for each agent: the MCP config that starts
`sfx mcp`, a short pointer in the agent's instruction file, and a
session-start hook. For Claude Code the hook file also gets the
[token usage hooks](#token-usage-hooks-claude-code):

| Agent | MCP config | Pointer | Hooks | You still |
|---|---|---|---|---|
| `claude-code` | `.mcp.json` | `CLAUDE.md` | `.claude/settings.json`: session start, and usage on Stop, SubagentStop and SessionEnd | approve the project's MCP server when Claude Code asks |
| `codex` | `.codex/config.toml` | `AGENTS.md` | `.codex/hooks.json` | trust the project, and the hooks with `/hooks` |
| `gemini` | `.gemini/settings.json` | `GEMINI.md` | `.gemini/settings.json` | trust the folder when Gemini CLI asks |
| `cursor` | `.cursor/mcp.json` | `.cursor/rules/starfix.mdc` | `.cursor/hooks.json` | turn starfix on in Cursor Settings › MCP |
| `vscode` | `.vscode/mcp.json` | `.github/copilot-instructions.md` | `.github/hooks/starfix.json` (Preview) | trust the server; hooks run only where VS Code's Preview hooks are on |
| `junie` | `.junie/mcp/mcp.json` | `AGENTS.md` | `~/.junie/config.json`, with `--global` only | run `sfx setup junie --global --write` for the hook |
| `jetbrains` | none: added in the IDE | `.aiassistant/rules/starfix.md` | none | add the printed JSON under Settings › Tools › AI Assistant › Model Context Protocol (MCP), scope Project |

Setup prints the "you still" step after it adds a part.

### What setup writes, and what it leaves alone

- **The pointer** is a short block between `<!-- starfix:begin -->` and
  `<!-- starfix:end -->`. It tells the agent to use the starfix tools and to
  `prime`, `start` and `finish`. The rest of the file is left alone.
  Cursor's and AI Assistant's pointers are rule files of starfix's own.
  Codex and Junie share `AGENTS.md`, so `setup --all` writes the block once,
  and removing either agent takes it out.
- **The MCP entry** holds `command`, `args` and `STARFIX_HARNESS` in its
  env (and `"type": "stdio"` for Claude Code and VS Code), and nothing
  else. `--check` fails on any extra key or variable
  (`cwd`, `PATH`, `LD_PRELOAD` and the like), and `--write` drops them.
  The one exception is `STARFIX_KEY_FILE`, which `--key-file PATH` adds
  to an entry in your own files and never to a project's
  ([Give an agent its own identity](#give-an-agent-its-own-identity)).
- **Existing files** keep their other keys, their order and their mode, and
  a second run changes nothing. A JSON file with a duplicate key is refused,
  since the harness would run the last copy, and so is one with comments
  or text after the closing brace, which a rewrite would lose. Setup edits
  only starfix's own tables in Codex's `config.toml`; if it cannot read the
  file safely, it refuses and prints the snippet to add by hand.
- **`--remove`** takes starfix's parts out. A pointer file left empty is
  deleted, and so is VS Code's hook file; an MCP config or a shared hook
  file stays, even when empty.
- **Symbolic links** in the repository are refused, file included, so a
  cloned repository cannot point `.mcp.json` at a secret or `.claude` at
  your home directory. New project files are 0644.
- **`--command PATH`** sets how the agent runs `sfx` when it is not on
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

A global hook runs in every repository with a `.starfix.yaml` that you
open with the agent, and connects to the server that file names; Claude
Code's usage hooks send that server the session's token counts. Prefer
per-project setup, or turn the global hooks off before you open a
repository you do not trust.

### Claude Desktop

Claude Desktop (macOS and Windows) has no project files, hooks or working
directory. Run `sfx setup claude-desktop --write` inside the repository,
then quit and reopen Claude Desktop. It adds one entry per project to the
app's config:

| OS | Config |
|---|---|
| macOS | `~/Library/Application Support/Claude/claude_desktop_config.json` |
| Windows | `%APPDATA%\Claude\claude_desktop_config.json` |

The entry is named `starfix-<directory name>` and runs
`/absolute/path/to/sfx -C /path/to/repo mcp`, because the app starts
servers without your shell's `PATH`. The path is your `sfx` on `PATH` (for
example Homebrew's link, which survives upgrades) or else the running
binary; `--command` must be absolute. Several projects coexist, and
`--check` and `--remove` touch only this checkout's entry. `--all` leaves
Claude Desktop out, because its file is outside the repository. With no
pointer or hook, the agent learns starfix from the MCP server's
instructions.

## Session-start hooks

Each hook runs `sfx prime --hook=AGENT` (Claude Code's runs `--hook` alone)
when a session starts, and replies in the harness's own format. Gemini CLI
matches sources exactly, so it gets one entry each for `startup`, `resume`
and `clear`; Codex's matcher is `startup|resume|clear|compact`.

The hook reads the session id from the harness's input, adds `prime`'s
orientation to the session's context, and prints nothing outside a
starfix repository. On any error it adds a one-line note instead of
failing the session.

A hook belongs to starfix only when its whole command is the one setup
writes, `sfx prime --hook[=AGENT]` or `sfx usage --hook[=AGENT]` through
`sfx` or the `--command` program. Other hooks, including your own
commands that end in `sfx prime --hook`, are never touched.

## Token usage hooks (Claude Code)

starfix reports the tokens each issue took
([Accounts, time and tokens](concepts.md#accounts-time-and-tokens)).
Only Claude Code's are captured so far; Codex, Gemini CLI and the others
follow once their formats are checked against real files. Every harness
still gets the time each issue was held.

Setup adds `sfx usage --hook` to Claude Code's Stop, SubagentStop and
SessionEnd hooks. Stop and SubagentStop run with `"async": true`, so the
turn never waits on the server, and a 30-second timeout. SessionEnd runs
synchronously with a 10-second timeout: it sends the session's last
response, and Claude Code waits for it as it exits, where an async hook
might not finish. Each run:

- reads the session's transcript, named by the hook's `transcript_path`,
  and its subagents' transcripts beside it
  (`<session id>/subagents/agent-*.jsonl`);
- reads each file from where the last run stopped, up to its last
  complete line. The offsets are kept in `starfix/usage-offsets.json`
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
conversation, but `sfx` reads them locally and keeps only the fields
above; no prompt, reply, tool input or file content is sent, stored or
printed. The connection is the same SSH connection every `sfx` command
uses.

The hook exits 0 whatever happens and prints nothing outside a starfix
repository or when there is nothing new. Otherwise it writes one line to
stderr with a fix. Claude Code keeps hook output out of the
conversation, so the line may go unseen; `claude --debug` shows it.

If Claude Code changes its transcript format, `sfx` never guesses
counts. When none of the response lines new in a file can be read, the
hook sends nothing for it, says the format was not recognized, names the
Claude Code version, and keeps the file's offset, so an upgraded `sfx`
reads those lines. Lines it cannot read among lines it can are skipped,
and the offset moves past them: an upgrade does not recover those.

An older setup upgrades with `sfx setup claude-code --write`. One from
before these hooks has the session-start hook alone, and one from their
first release runs SessionEnd async; `--check` names the hooks to fix.

## Tools

The MCP server offers 24 tools: `prime`, `start`, `finish`, `handoff`,
`ready`, `blocked`, `list`, `show`, `create`, `update`, `close`, `reopen`,
`dep`, `label`, `comment`, `comments`, `history`, `digest`, `who`,
`inbox`, `remember`, `recall`, `forget` and `cost`. There are no admin
tools: import, export, setup, upgrade, prices, plans and server settings
stay on the command line. Hours are logged by people, with `sfx log`; no
tool logs them.

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
  `recall` searches by text, key, tag and scope; `forget` deletes. `prime`
  shows the pinned and relevant memories and `start` those relevant to
  its issue. Pinning is left to people (`sfx pin`). A memory that looks
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
  No tool sets an account; people do that with `sfx update --account`.
- **`cost` reports spend.** It takes `by` (`account`, the default,
  `issue`, `epic`, `person` or `model`), `since` and `until`, and returns
  a line per group (`ID title: $8.35, 4.3M tokens, split`), the total and
  the models with no price ([Cost](concepts.md#cost)). Once the server
  has a plan, each line also gives the amortized cost (`$8.35, amortized
  $20.00, 4.3M tokens`), and a group with hours its hours (`1.5h
  logged`). It lists 20
  groups, fewer if they would pass the result cap, and sums the rest as
  `(other)`, so the groups still add up to the total.
- **Files come from git, not from the agent.** `sfx mcp` reads the paths
  each held issue's work touched from the repository it was started in,
  and sends them with a renewal (at most every five minutes per issue),
  `finish` and `handoff`; no tool takes them. `ready` gives each issue
  whose files overlap another session's work `overlaps` (and ranks it
  last), and `show` gives the likely `files`, `files_more` and the
  `overlaps` with who holds them ([Files](concepts.md#files-what-an-issue-touches)).
  People declare files with `sfx create --paths` or `sfx update --paths`.

One SSH connection serves an MCP session. It opens on the first tool call
and is redialed if it drops. Reads, `create`, `comment`, `finish`,
`handoff`, `inbox`, `remember` and `forget` are retried on the new
connection, since a retry cannot write twice. `start`, `update`, `close`, `reopen`, `dep` and
`label` report that they may have applied. A request the server has not
answered within 75 seconds counts as a dropped connection: the server
gives up on a request after a minute and answers that it did, so by then
the connection itself has stalled. `sfx mcp` closes it, the call is
retried or reported as above, and the claims' renewal, which waits
behind the call, goes out on the next connection.

A person's sessions together hold at most `conns_per_principal`
connections, 64 unless the server says otherwise
([Limits](server.md#limits)). A session refused past it cannot reach the
server until one of them closes, and every tool call fails with:

```text
busy: alice is at its connection limit (64)
fix: the user's other starfix sessions hold every connection the server allows them, so this session cannot reach it until one closes. Tell the user now: they can close some, or ask the server's operator to raise conns_per_principal. Until then, issues you hold go back to open unless renewed within their 15m lease, and writes are not kept for later; never run an admin command yourself
```

The session does not dial again for 5 seconds after such a refusal, so
a burst of calls costs the server one refused handshake; after that,
the next call or renewal takes a connection as soon as one is free, and
the session goes on as before. Writes are not queued in the outbox
here: `busy` is a refusal, not an outage.

### When the server cannot be reached

When `create`, `comment`, `handoff`, `remember`, `finish`, `update` with
a `rev`, `dep` (adding) or `label` cannot reach the server, `sfx mcp`
keeps the write in the outbox on the machine
([When the server cannot be reached](cli.md#when-the-server-cannot-be-reached))
and the tool returns one line, which is not an error:

```text
queued: starfix cannot reach its server, so this finish waits on this machine and is sent when the server answers. Carry on, and do not repeat it.
```

Its structured content is `{"queued": true, "op": …, "entry": …}`, and
never an `id` and a `rev`. A queued `create` adds that the issue has no id
until then, so nothing can refer to it. A `label` call of several labels
queues a write for each, and its line says how many. The agent is told
to carry on: it finishes what it holds and files what it finds.

An `update` without a `rev` reads the issue's rev first, which needs the
server: with the server away it fails, saying it was not queued and to
give `rev`. Against a server that speaks only protocol 5, `update`,
`dep` and `label` are not queued, since that server could apply one
twice.

Nothing else is queued. `start` fails, and tells the agent that it was
not queued and to go on with the work it holds: no claim is taken
without the server. A write the outbox refuses (text that looks like a
credential, an outbox that is full, a machine that has never reached the
server with this key) is an error whose fix is the agent's next step, or
to tell the user.

The session's next connection sends what waits first, in order, before
the call that opened it, for five seconds at most, and the rest in the
background between calls. A session that makes no call still connects,
when its renewal comes round each minute, if something waits. What a
session has not sent when it ends stays in the outbox, and the next
`sfx` command or session of the same principal on that machine sends it.

There is no tool for the outbox, and the agent is never asked to manage
it. When the server refuses the principal the project as `forbidden`,
the tool call fails with that refusal, telling the agent to ask a
person, and what waits is marked refused for that person to retry or
drop. A person hears of it through `prime`: when writes wait, or the
server refused some, `prime` has one more notice, in the tool's result
and in the session-start hook's context alike:

```text
outbox: 1 write made while the server was away was refused and 2 wait to be sent: tell the user to run `sfx outbox`
```

`sfx --no-queue mcp`, or `STARFIX_NO_QUEUE=1` in the server's
environment, turns the outbox off for a session: its writes then fail
when the server is away, as they did before. The hooks never queue and
never send what waits.

## Sessions

The server knows the principal from the SSH key. The session id comes
from the agent's environment:

| Agent | Session id |
|---|---|
| Claude Code | `CLAUDE_CODE_SESSION_ID`, which it sets |
| Codex, Gemini CLI, Cursor, VS Code, Junie, AI Assistant | none set; `sfx mcp` picks one per process (`m-…`) |
| Claude Desktop | none set; each app launch is one `sfx mcp` process, so one session (`m-…`) |
| A person's own `sfx` commands | `cli`, one per machine |
| Any | `STARFIX_SESSION`, if set, wins (for example in the MCP entry's `env`) |

The harness name comes from `STARFIX_HARNESS`, which setup writes into the
MCP entry; failing that, `claude-code` when `CLAUDECODE=1` and `gemini`
when `GEMINI_CLI=1`.

The hooks take the session id from their input, unless `STARFIX_SESSION`
is set. In Claude Code that id is `CLAUDE_CODE_SESSION_ID`, the session
`sfx mcp` claims issues under, so the usage hooks' tokens go to the
issues that session held. If you set `STARFIX_SESSION` for `sfx mcp`, set
it for the hooks too.

### Trace context

A harness or a script that traces its own work can tie starfix's part to
it. `sfx` passes a
[W3C trace context](https://www.w3.org/TR/trace-context/) to the server
with each request, and a server that exports traces
([Traces](server.md#traces)) starts its span for the request as a child
of the caller's. A server that does not still names the trace in its log
line for the request. `sfx` itself starts no span and exports nothing.

| From | Read | Applies to |
|---|---|---|
| The environment: `TRACEPARENT` and `TRACESTATE` | Once, when `sfx` starts | Every request of an `sfx` command; in `sfx mcp`, every request of a tool call that brings none of its own, and the renewals it makes between calls |
| A tool call's metadata: `traceparent` and `tracestate` in the request's `_meta` | At each call | The requests that one tool call makes of the server |

- **The metadata keys are the Model Context Protocol's own**: its
  specification reserves `traceparent`, `tracestate` and `baggage` in
  `_meta` for trace context, from the 2026-07-28 version on, after
  OpenTelemetry's convention for MCP. `sfx mcp` reads the first two.
  `baggage` is not read or passed on.
- **Only a valid one is sent**: a version-00 `traceparent`, in lowercase
  hex, and a `tracestate` only with one. Anything else is left out, and
  never fails a command or a tool call: a call whose metadata is
  malformed falls back to the environment's.
- **No tool's schema or arguments changed.** It is metadata of the
  request, which a harness that traces adds by itself; an agent does
  not pass it, and cannot.
- **Of what `sfx` sends, the server keeps the ids and the sampled
  flag.** The `tracestate` goes with the request and is put on no span
  and no log line.

## Give an agent its own identity

An agent started from your terminal has your environment: your home
directory, with its `~/.ssh`, and your ssh-agent socket. So by default it
signs in as you, with your rights, by the same
[order of keys](cli.md#your-key-is-your-identity) as your own `sfx`. An
agent that works on its own should be a principal of its own that is no
admin ([Security](security-model.md#who-may-change-what)). To set that
up:

1. Make a key pair for the agent, with no passphrase, readable by you
   alone: `ssh-keygen -t ed25519 -N '' -f ~/.ssh/starfix_agent`. Give it
   a name of its own, not a default one such as `id_ed25519`.
2. Have an admin add a principal for it with the public half: `sfx admin
   principal add alice-agent --key ~/.ssh/starfix_agent.pub`
   ([Managing access](server.md#managing-access)).
3. Start the agent with `STARFIX_KEY_FILE` holding the private key's
   path:

   ```sh
   STARFIX_KEY_FILE=~/.ssh/starfix_agent claude
   ```

`sfx` then offers that key and no other, whatever your `~/.ssh` and
ssh-agent hold, so every tool call and hook of that agent is the agent's
principal's. The variable holds the file's path, never the key.

### Where each harness gets the variable

`sfx mcp` and the hooks are separate programs that the harness starts.
Each signs in with whatever `STARFIX_KEY_FILE` its own environment
holds:

| Agent | `sfx mcp` sees the variable | The hooks see it |
|---|---|---|
| Claude Code, Codex, Gemini CLI, Junie | when the harness was started with it (step 3, or your shell's profile), or from the entry `sfx setup AGENT --global --key-file PATH` writes | only when the harness was started with it |
| Cursor, VS Code, AI Assistant | when the IDE was started with it. An IDE opened from the desktop does not read your shell's profile: start it from a terminal, or for Cursor use `sfx setup cursor --global --key-file PATH` | only when the IDE was started with it |
| Claude Desktop | only from `sfx setup claude-desktop --write --key-file PATH`: the app passes no environment on | it has no hooks |

The files setup writes into a repository are committed and shared, and a
key file's path is one person's, so setup never writes the variable
there: `--key-file` without `--global` is refused, except for Claude
Desktop, whose config is yours. In a project that commits its `.mcp.json`
and the like, each person sets the variable in the environment the
harness starts in.

`sfx setup AGENT --global --write --key-file PATH` writes
`STARFIX_KEY_FILE` into the `env` of your own MCP entry, beside
`STARFIX_HARNESS`. The path is made absolute, must be a file, and is
refused unrepeated when it is plainly a key. `--check --key-file PATH`
checks for it; `--check` without the option fails on an entry that has
one and says which file it names. `--write` without the option replaces
the entry, and so takes the variable out, saying that it did. `--remove`
removes the entry and the variable with it.

**Hooks do not read the MCP entry.** With the variable only in the entry,
the agent's tool calls are the agent's principal's, while the
session-start and usage hooks sign in by your default keys, as you.
Nothing fails: the hook's `prime` text says `you are alice` where the
`prime` tool says `you are alice-agent`, `sfx who` shows a session under
each, and the usage hooks send their token counts as you.
Setup says this when it writes the variable for an agent that has hooks.
To have both agree, start the harness with the variable set (step 3).

To check, read the first two lines of `prime`: the first names the
principal (`you are alice-agent`) and the second the key that signed in
and the file it came from. `sfx whoami`, run in the same environment,
shows the same and more. If the harness does not pass its environment
on, `prime` shows your own name: then use setup's `--key-file`, or put
the key inside the agent's checkout and name it with `key:` in that
checkout's `.starfix.yaml`, uncommitted.

## Sessions and hooks in harnesses other than Claude Code

One limitation affects every harness except Claude Code. The session-start
hook learns the harness's session id from its input, but `sfx mcp` cannot,
so it picks its own. The hook's `prime` and the agent's tool calls then
show as two sessions in `sfx who`, and the hook's session holds no lease.
Claude Code avoids this because it sets `CLAUDE_CODE_SESSION_ID` for both.
