# The guard: one permission policy for every agent session

Status: design, 21 October 2026, proposed (aries-kqoyjyft). Nothing in
it is built, and nothing here is decided: §12 lists the questions for
the maintainer, and where the text below says what the guard does, it
says what this design proposes until they are answered. It changes what
an unattended agent may run, so it is held for the maintainer's review
under the memory `merges`.

## 1. Why

A worker's permissions come from two unrelated places
([factory-review.md](factory-review.md) §3.2):

- A headless run (`aries dispatch run`) gets `DefaultAllowed` and
  `DefaultDenied`, Go variables in `internal/dispatch/claude.go`, as
  `--allowedTools` and `--disallowedTools` under `--permission-mode
  dontAsk`.
- A Desktop session gets the desktop app's permission mode and whatever
  the person's own settings hold. The repository's `.claude/settings.json`
  holds hooks only.

Claude Code matches those lists by prefix against the command's text,
and that broke twice in two days:

- `GOTOOLCHAIN=local GOFLAGS=-mod=readonly go test …` was denied, because
  an allow rule does not match past a leading assignment of a variable
  Claude Code does not know to be safe (aries-vhbljxmz; PR 288 set the
  variables in the run's environment and told the brief to issue
  commands bare).
- `scripts/leftovers.sh gh pr merge 1` would have been allowed had
  `Bash(scripts/leftovers.sh:*)` stayed on the list, since a prefix rule
  allows whatever follows a launcher (PR 288's second commit took it off
  and denied `find`'s `-exec`, `-execdir`, `-ok` and `-okdir`).

A denial also reads only as a refused tool call. The first Haiku run
took one for a block on all Bash and ended with its edits uncommitted
(factory-review.md §3.2).

Reading the permission rules for this design turned up a third gap: the
six `Write(…)` entries in `DefaultDenied` are never consulted. Claude
Code checks file paths against `Edit(path)` and `Read(path)` rules only,
and "accepts the rule but never consults it" for a `Write` path rule
([Read and Edit](https://code.claude.com/docs/en/permissions#read-and-edit)).
The matching `Edit(…)` entries cover the Write tool, so nothing is open
today, but the list says more than it does.

The guard is one program, `aries guard --hook`, that Claude Code runs
before every Bash, Edit and Write call in both kinds of session. It
reads the whole command, through wrappers and compound commands, against
one policy file in the repository, and every denial says what to do
instead (AGENTS.md rule 11). It adds to Claude Code's rules and never
replaces them, and branch protection stays the backstop.

## 2. What Claude Code gives a hook

From [Hooks reference](https://code.claude.com/docs/en/hooks) and
[Configure permissions](https://code.claude.com/docs/en/permissions),
read 21 October 2026:

- **Input.** A PreToolUse hook gets one JSON object on stdin: the common
  fields (`session_id`, `transcript_path`, `cwd`, `permission_mode`,
  `hook_event_name` and others), and `tool_name`, `tool_input` and
  `tool_use_id`. For Bash, `tool_input.command` is the whole command
  text. For Write, Edit and Read, `tool_input.file_path` "is always
  absolute": Claude Code expands `~` and relative paths first, so a path
  check cannot be got round by spelling a path another way ("PreToolUse
  input").
- **Decision.** Printed as JSON on stdout with exit 0:
  `hookSpecificOutput.permissionDecision` is `allow`, `deny`, `ask` or
  `defer`, with `permissionDecisionReason`, which is "shown to Claude"
  for a deny, and in a `-p` run is what Claude reads in the tool result
  ("PreToolUse decision control"). `additionalContext` adds text beside
  the tool result; `systemMessage` is "shown to the user" ("JSON
  output"). A hook that prints nothing and exits 0 makes no decision,
  and the call "continues through the normal permission flow".
- **Exit codes.** 0 applies the JSON. 2 blocks the call, with stderr as
  the reason. Any other code, a hook that cannot start, invalid JSON or
  a timeout is a non-blocking error: "the action goes ahead", with a
  `hook error` notice ("Exit code output", "Timeouts").
- **Failing closed is opt-in.** `"onFailure": "block"` on a command hook
  makes each of those failures block the call instead; it needs Claude
  Code v2.1.295 or later ("Block the action when a hook fails").
- **Precedence.** "PreToolUse hook decisions don't bypass permission
  rules": a matching deny rule blocks and an ask rule prompts whatever
  the hook returned. A hook's deny, or exit 2, blocks even where an
  allow rule matches ("Precedence among rules, hooks, and mods").
  Between hooks, deny > defer > ask > allow. A hook's `allow` "skips the
  permission prompt", so under `dontAsk` it lets through a call no allow
  rule covers. In plan mode a hook's `allow` has no effect on a call
  that is not read-only.
- **Where hooks run.** Every matching hook runs, in parallel. A `-p`
  run uses the hooks in a repository's `.claude/settings.json` even in a
  folder never trusted, while that file's `permissions.allow` rules are
  not used there ("What runs before you trust a folder"). An interactive
  session runs hooks once the folder is trusted. `--bare` skips hooks
  ([headless](https://code.claude.com/docs/en/headless), "Start faster
  with bare mode"), which is why the dispatcher never uses it
  (dispatcher.md §11).
- **What prefix rules miss.** Claude Code splits compound commands at
  `&&`, `||`, `;`, `|`, `|&`, `&` and newlines and checks deny rules in
  subshells, command substitutions and loop bodies. It strips `timeout`,
  `time`, `nice`, `nohup`, `stdbuf`, `command`, `builtin`, `noglob` and
  a flagless `xargs`, and a deny rule matches past any leading
  assignment. It does not see `/usr/bin/curl` as `curl`, nor
  `sh -c '…'`, nor `git -C . push`, nor `git 'push'`, and its own page
  says a deny rule "isn't a security boundary around the program"; for
  "the full command text with your own logic", it points to a PreToolUse
  hook ("What a Bash rule doesn't match").

So a hook is the one place that sees every call on both paths, can
deny with a reason the agent reads, and cannot loosen a deny rule.

## 3. What the guard does

`aries setup claude-code` writes, into `.claude/settings.json` beside
the SessionStart and usage hooks:

```json
{"hooks": {"PreToolUse": [{"matcher": "Bash|Edit|Write|NotebookEdit",
  "hooks": [{"type": "command", "command": "aries guard --hook", "timeout": 5}]}]}}
```

For each call it reads the hook's input, finds the repository from
`cwd`, reads the policy file (§4) and gives one of three answers:

| Answer | Output | When |
|---|---|---|
| Deny | `permissionDecision: "deny"` with the reason and fix (§7), exit 0 | Any part of the command, read through its wrappers (§5), matches a deny rule; or a file tool's path matches a path rule |
| Allow | `permissionDecision: "allow"`, exit 0 | Every part of the command matches an allow rule after the guard's own reading (Q2) |
| No decision | Nothing, exit 0 | Neither: Claude Code's own rules and the session's mode decide, as they do today |

An `allow` is what fixes the denials of §1 without a brief line about
`VAR=value`: `GOTOOLCHAIN=local go test ./...` and
`scripts/leftovers.sh go test ./...` are allowed because the guard reads
the assignment and the wrapper, while Claude Code's deny rules, which
the dispatcher still passes (§8), stay in force under it.

It never calls the server, reads no `.aries.yaml`, opens no network
connection and writes no file. Outside a repository with a policy file
it prints nothing, as `aries prime --hook` does outside an Aries
repository (Q4).

## 4. The policy file

`.aries/guard.yaml`, beside `.aries/workflow.yaml` and
`.aries/brief.txt` (factory review, step 4). It is the repository's, is
changed by pull request, and is read with the YAML module `aries`
already links (`go.yaml.in/yaml/v3`), so it needs no new dependency. The
starting file holds this repository's `DefaultAllowed` and
`DefaultDenied` as they stand on `main`, with a fix on every deny:

```yaml
version: 1

# Variables a command may set in front of a program. One that is listed
# with `env:` is allowed when its value equals the session's (it does
# nothing); another value is denied with the fix. One not listed leaves
# the command to Claude Code's rules (allow) and never hides a denied
# program (deny).
env:
  GOTOOLCHAIN: {fix: "drop GOTOOLCHAIN=…: it is already set to local"}
  GOFLAGS: {fix: "drop GOFLAGS=…: it is already set to -mod=readonly"}
  ARIES_REQUIRE_POSTGRES: {any: true}
  ARIES_E2E_TRANSPORT: {any: true}

# Programs that run the command that follows them. The guard reads the
# inner command; the wrapper alone allows nothing.
wrappers:
  - scripts/leftovers.sh

# Options that take a value, per program, so that the guard finds the
# subcommand past them: `git -C dir push`, `gh -R owner/repo pr merge`.
options:
  git: [-C, -c, --git-dir, --work-tree, --namespace]
  gh: [-R, --repo]
  go: [-C]

allow:
  - tools: [Read, Edit, Write, Glob, Grep, TodoWrite, mcp__aries]
  - run: [git, go, gofmt, golangci-lint, shellcheck]
  - run: [scripts/gate.sh, scripts/install-postgres.sh]
  - run: [gh pr create, gh pr view, gh pr checks, gh pr list, gh pr ready, gh pr edit, gh run view]
  - run: [cd, ls, cat, head, tail, wc, grep, find, sed -n, mkdir, diff]

deny:
  - run: gh pr merge
    fix: "merges are the coordinator's: hand off with the PR's link"
  - run: gh api
    fix: "gh api is not allowed: use gh pr view, gh pr checks or gh run view"
  - run: git push
    flags: [--force, -f, --force-with-lease, --mirror, --delete, -d]
    fix: "push without force to your own branch: git push -u origin HEAD"
  - run: git push
    refs: [main]          # a destination of main, however written
    fix: "push to your branch, never main: git push -u origin HEAD"
  - run: git
    config: [alias.*]     # git -c alias.x='!…' runs a shell command
    fix: "write the git command out; aliases are not read"
  - run: find
    flags: [-exec, -execdir, -ok, -okdir, -delete]
    fix: "list with find, then act on each path in a command of its own"
  - path: [.claude/**, .mcp.json, .aries/guard.yaml, internal/release/keys.go]
    fix: "this file is the maintainer's: say what it needs in your handoff"
```

The rules, as the code session would build them:

- **A `run:` rule is words, not a pattern.** The first word is the
  program, matched on the name after the last `/`, so `/usr/bin/gh`
  is `gh`; a path that names a repository script (`scripts/gate.sh`) is
  matched as a path from the repository's top. The other words must be
  the command's leading words once options are skipped, those in
  `options:` with their values. So `gh pr merge` matches
  `gh -R o/r pr merge 1` and `gh pr merge --squash 1`, and does not
  match `gh pr view 1`.
- **`flags:`** match an argument anywhere after the rule's words,
  exactly or as `--flag=value`, and short flags combined (`-fu` holds
  `-f`).
- **`refs:`** are for `git push`: a destination of `main` is any
  refspec whose right side is `main` or `refs/heads/main`, with or
  without `+`, or a deletion (`:main`). A push with no refspec, or `HEAD`
  alone, goes where the branch's upstream goes, which the guard does not
  read (§6); branch protection holds that case.
- **`config:`** match `git -c key=value` keys, by glob.
- **`path:`** rules use gitignore patterns from the repository's top, as
  Claude Code's `Edit` rules do, and apply to the file tools' absolute
  `file_path`, to Bash's output redirections and `tee`'s files, and to
  each path in a Codex patch (§9). A path is compared after symlinks are
  resolved, and one outside the repository is left to Claude Code.
- **`tools:`** are tool names Claude Code's own lists take; the guard
  decides only Bash and the file tools, and passes the rest to the
  dispatcher's `--allowedTools` (§8).
- **Deny first.** A call that matches a deny rule is denied whatever
  else it matches. The file is checked on load: an unknown key, a rule
  with no fix, or a version the binary does not know is an error (§6).
- **Built in, always.** Whatever the file says, edits to the policy file
  itself are denied (Q6).

## 5. How a command is read

The guard parses the command as POSIX `sh` (with bash's `[[`, `|&` and
`$'…'`), in its own small parser on the standard library, and checks
every simple command it finds. No `sh` runs, and nothing is expanded
that needs the environment beyond the session's own variables.

**Compound commands.** The command is split at `&&`, `||`, `;`, `|`,
`|&`, `&` and newlines, and into the bodies of `( … )`, `{ …; }`,
`if`/`then`/`else`, `for`, `while`, `until` and `case`. Command
substitutions `$( … )` and `` `…` `` and process substitutions
`<( … )` are parsed as commands of their own wherever they appear,
inside double quotes included. Every simple command is checked: one
denied part denies the whole, and the whole is allowed only when every
part is.

**Quotes and words.** Single and double quotes and backslashes are
removed as the shell would, so `git 'push' origin main` is
`git push origin main`. A word with an unquoted `$VAR`, `${…}` or a
glob in the program's place (`$CMD x`, `g* push`) cannot be read.

**Wrappers.** A wrapper's own options are skipped and the inner command
checked as a command of its own, as many levels deep as it goes (eight
at most, then unreadable):

| Wrapper | Inner command |
|---|---|
| `VAR=value cmd` | `cmd`, with the assignment checked against `env:` |
| `env [-i] [-u NAME] [NAME=value…] cmd`, `env -S 'string'` | `cmd`; the `-S` string parsed |
| `command`, `builtin`, `exec`, `nohup`, `time`, `noglob` | what follows (`command -v` runs nothing) |
| `timeout [opts] DURATION cmd`, `nice [-n N] cmd`, `stdbuf -o… cmd`, `ionice`, `setsid`, `flock FILE cmd`, `watch [opts] cmd` | `cmd` |
| `sh`, `bash`, `zsh`, `dash` with `-c STRING` (and `-lc`, `-ec`) | `STRING`, parsed as a command |
| `xargs [opts] cmd args` | `cmd args`, with unknown arguments appended |
| `find … -exec cmd … ;` (and `+`, `-execdir`, `-ok`, `-okdir`) | `cmd …`, with `{}` an unknown argument; and the find itself matches the `find` deny rule's `flags:` |
| `git -c alias.…`, `git` with an alias | a `config:` rule (above) |
| Each of the policy file's `wrappers:` | what follows its own options |
| `sudo`, `doas`, `su -c` | denied by the starting file |

**What it cannot read.** `eval`, `source` or `.` of a file, a
here-document piped to a shell (`sh <<EOF`), a program named by a
variable, a command over the parser's limits (64 KiB, nesting of eight)
or one it fails to parse. Such a command is denied with the fix "write
it as plain commands, one program each" when any part of it is
unreadable, in both kinds of session (Q3), since an unreadable part
could be anything.

**Arguments it cannot see.** `xargs` and `find -exec` add arguments the
guard does not know. A deny rule with `flags:` or `refs:` reads
arguments, so where some are unknown the guard denies: `… | xargs git
push origin` is denied with "write the arguments out". A rule of words
alone needs no arguments: `… | xargs gh pr merge` matches it as
written.

**Rows for the table test.** The code session's table test holds these,
and every command form the brief template and AGENTS.md's Test tiers
issue, to the starting file:

| Command | Answer |
|---|---|
| `go test -race ./internal/dispatch/`, `go test -p 2 ./...`, `go test ./scripts/`, `go test -count=3 -run TestX ./internal/store/` | allow |
| `GOTOOLCHAIN=local GOFLAGS=-mod=readonly go test -race ./...` | allow (values equal the session's) |
| `GOFLAGS=-mod=mod go test ./...` | deny: "drop GOFLAGS=…" |
| `ARIES_REQUIRE_POSTGRES=1 scripts/leftovers.sh go test -race -shuffle=on ./...` | allow |
| `scripts/gate.sh --quick --pgtest-lock`, `gofmt -l .`, `go vet ./...`, `golangci-lint run ./...` | allow |
| `git push -u origin HEAD`, `gh pr create --title x --body-file b.md`, `git merge origin/main` | allow |
| `cd internal/dispatch && go test ./...`, `go test ./... \| tail -20` | allow |
| `gh pr merge 1`, `FOO=bar gh pr merge 1`, `/usr/local/bin/gh pr merge 1`, `gh -R o/r pr merge 1` | deny: merges |
| `scripts/leftovers.sh gh pr merge 1`, `env gh pr merge 1`, `command gh pr merge 1`, `timeout 5 gh api user`, `nohup gh pr merge 1 &` | deny |
| `sh -c 'gh pr merge 1'`, `bash -lc "gh api user"`, `echo $(gh pr merge 1)`, `` x=`gh api user` ``, `(cd x && gh pr merge 1)` | deny |
| `if true; then gh pr merge 1; fi`, `for i in 1; do gh api x; done` | deny |
| `find . -name x -exec gh pr merge 1 \;`, `find . -delete` | deny: find's actions |
| `echo 1 \| xargs gh pr merge`, `echo main \| xargs git push origin` | deny |
| `git -C . push origin main`, `git 'push' origin main`, `git push origin HEAD:refs/heads/main`, `git push origin :main`, `git push origin +fix/x` | deny: pushes |
| `git -c alias.m='!gh pr merge 1' m` | deny: aliases |
| `eval "gh pr merge 1"`, `$CMD pr merge 1`, `sh <<EOF` | deny: unreadable |
| Edit or Write of `.claude/settings.json`, `.mcp.json`, `.aries/guard.yaml`; `echo x > .mcp.json` | deny: the maintainer's |

## 6. Failing open or closed

Three things can go wrong, and the design treats them apart:

1. **The guard does not run**: `aries` is not on the session's `PATH`,
   it crashes, or it passes its timeout. Claude Code lets the call go
   ahead with a `hook error` notice, unless the hook sets
   `"onFailure": "block"` (§2).
2. **The guard runs and cannot decide**: the policy file is missing
   (with the hook written), unreadable, invalid or of a newer version,
   or the hook's input is not the JSON it expects.
3. **The command cannot be read** (§5). This is not an error: it is
   denied, with a fix.

For 1 and 2 the design proposes failing **open in a Desktop session and
closed for a headless run** (Q1):

- A Desktop session has a person who sees the notice, and Claude Code's
  own rules and mode still apply. Failing closed there would let one bad
  merge to `.aries/guard.yaml` refuse every Bash and Edit call in every
  session of the repository, the session sent to fix it included. The
  guard exits 0 and prints a `systemMessage` of one line, `aries guard:
  policy not applied: .aries/guard.yaml: line 12: unknown key "alow"`,
  which the person reads, as `prime --hook` turns its errors into one
  line (CLAUDE.md). It holds no command text.
- A headless run has no one to read a notice, and its claim and branch
  run unattended. Before it starts a run, the dispatcher runs
  `aries guard --check` in the run's worktree, which loads the policy
  as the hook would and runs the table of §5's starting rows; a failure
  starts no run, with the check's fix in the dispatcher's log and
  status. The run then gets `--settings` with the same hook and
  `"onFailure": "block"`, so a guard that breaks mid-run blocks rather
  than waves through (Q1: this needs Claude Code 2.1.295 or later on the
  dispatcher's machine, which `--check` can test).

Either way, Claude Code's deny rules and branch protection hold: a guard
that fails open leaves a session where it is today, never below it.

## 7. What a denial says

The reason is the agent's next step, in rule 11's form, and says that
only this call was refused:

```
aries guard: `gh pr merge` is not allowed here (.aries/guard.yaml, deny 1).
fix: merges are the coordinator's: hand off with the PR's link.
Only this command was refused; other commands, Bash included, still run.
```

- It names the rule's own words and where the rule is, never the
  command's arguments or a variable's value: `drop GOFLAGS=…`, not the
  value. A command can hold a token (`gh auth login --with-token`, a
  `curl -H` header), and the reason goes into the transcript and the
  run's log (rule 2).
- It is one reason for the first denied part; the agent fixes that and
  tries again.
- It goes out as `permissionDecisionReason` with exit 0, not exit 2's
  stderr, so the agent reads the guard's text and not a `hook error`
  prefix.
- The guard keeps no log of what it denied. Counting denials for the
  outcome record (outcome-record.md) is a later choice (Q7).

## 8. One policy file, two readers

**The dispatcher** reads `.aries/guard.yaml` from the run's worktree, a
fresh branch from `origin/main`, and builds the run's lists from it:

- `--allowedTools`: the `tools:` names, and each `run:` allow rule as
  Claude Code's `Bash(words *)`, for the forms Claude Code matches
  itself (the guard's `allow` covers the rest).
- `--disallowedTools`: each word-only deny rule as `Bash(words *)`, and
  each `path:` as `Edit(path)`. Rules with `flags:`, `refs:` or `config:`
  have no faithful prefix form and are the guard's alone; the coarse
  forms of today (`Bash(git push --force:*)`, `Bash(find*-exec*)`) may
  stay as a second layer (Q5).
- `DefaultAllowed` and `DefaultDenied` go from `claude.go`. A
  repository with no policy file gets no run, with the fix "add
  .aries/guard.yaml (`aries guard init` writes a starting one)" (Q4).
  `TestDefaultPermissions` becomes the guard's table test, and its
  `matches` helper, a model of Claude Code's matching, stays to check
  the rendered lists.

**`aries setup claude-code`** writes the PreToolUse hook of §3, and
`--check` reports it missing like the others. It does not copy the
rules into `.claude/settings.json`'s `permissions`: one copy is the
point, and a `-p` run in a folder never trusted would not use them
anyway (§2). Whether it should mirror the deny rules there, as a layer
that holds when `aries` is not on `PATH`, is Q5.

**Which copy is read.** A worker edits its own worktree, the policy
file in it included. The built-in path rule denies the file tools and
redirections there, but a program the worker wrote and ran could still
rewrite it (§10). The design proposes that a headless run's hook read
the policy the dispatcher copied, at launch, into the run's directory
under aries's data directory, named by `ARIES_GUARD_POLICY` in the run's
environment, which a tool call cannot change for the harness; a Desktop
session reads its worktree's file (Q6).

**The preflight** of aries-vhbljxmz's lesson becomes `aries guard
--check`: it runs the brief template's commands and the gate's lines
(AGENTS.md, Commands) through the policy and fails when one is not
allowed, so a change to the template, AGENTS.md or the file that breaks
a run fails a test in `scripts/` before any run finds out.

## 9. Codex, Gemini and the others

From [Codex hooks](https://learn.chatgpt.com/docs/hooks) and the
[Gemini CLI hooks reference](https://geminicli.com/docs/hooks/reference/),
read 21 October 2026. `aries setup` already writes SessionStart hooks
for both (`internal/agentsetup/hook.go`).

| | Claude Code | Codex CLI | Gemini CLI |
|---|---|---|---|
| Event | `PreToolUse` | `PreToolUse` | `BeforeTool` |
| Shell tool | `Bash`, `command` | `Bash` and `exec_command` | `run_shell_command`, `command` |
| File tools | `Edit`, `Write`, `NotebookEdit`, absolute `file_path` | `apply_patch` (matchable as `Edit` or `Write`): a patch, whose paths the guard reads from its headers | `write_file`, `replace`, `file_path` |
| Deny | `hookSpecificOutput.permissionDecision: "deny"` with a reason | the same shape, or legacy `decision: "block"`, or exit 2 | `decision: "deny"` with `reason`, sent "as a tool error", or exit 2 |
| Allow | `permissionDecision: "allow"` | `allow` with `updatedInput` only, a rewrite; `ask` is unsupported | not documented as approving a call |
| A hook that fails | runs the call; `onFailure: "block"` to refuse | runs the call: "a useful guardrail, not a complete enforcement boundary" | other exit codes are "non-fatal": runs the call |
| Timeout | seconds, default 600 | seconds, default 600 | milliseconds, default 60000 |
| Project hooks run | when the folder is trusted; always under `-p` | once the `.codex/` layer is trusted and each hook trusted by hash in `/hooks` | in a trusted folder |
| Form `aries setup` writes | `aries guard --hook` | `aries guard --hook=codex` in `.codex/hooks.json` | `aries guard --hook=gemini` in `.gemini/settings.json` |

So both get a deny-only guard through `aries setup codex` and
`aries setup gemini`, with the same policy and fixes in each harness's
shape. Neither gets the guard's `allow`: a Codex or Gemini session's
approvals stay its own sandbox and approval policy (dispatcher.md §10).
Neither has a documented way to fail closed, so for them the guard is a
guardrail only. Codex's trust by hash means a changed hook command is
skipped until a person trusts it again in `/hooks`; `aries setup codex`
says so, as it does for the SessionStart hook. Whether Gemini's hook
runs under `gemini -p`, and Codex's under `codex exec` without
`--dangerously-bypass-hook-trust`, the pages do not say; the slice that
adds each verifies it, and the dispatcher drives Claude Code alone
until then.

Cursor's `beforeShellExecution`, VS Code's agent hooks and Junie were
not read for this design; `aries setup` keeps writing only their
SessionStart hooks, and says the guard is not installed for them.

## 10. What it does not do

- **It is not a sandbox.** It reads command text. A program the agent
  wrote and ran (`go run`, `go test` with a test that execs, a git hook,
  a `Makefile`) can do anything the user can, as PR 288 recorded for
  `git` and `go`. Claude Code's sandbox and the operating system are the
  boundary for that; branch protection, the merge queue and the
  maintainer's review are the boundary for what reaches `main`.
- **It does not know where a bare push goes**: `git push` with no
  refspec follows the branch's upstream, and an agent that set its
  upstream to `main` gets past `refs:`. Branch protection refuses the
  push.
- **It does not replace Claude Code's rules or mode**, and does not read
  the person's own settings: it only adds to them.
- **It reads no other tools.** Web fetches, MCP tools and subagents stay
  under Claude Code's rules; the aries tools' limits are the server's.

## 11. Latency

The hook runs before every Bash and file call, in a process of its own
for each, and parallel tool calls start one each. The budget:

- **Under 20 ms at the 99th percentile** on a developer's machine, from
  process start to exit, for a 1 KiB command and the starting file.
  `aries version` took 10 ms or less, wall clock, three runs on an
  Apple-silicon Mac (21 October 2026), so start-up leaves room for
  reading a file of a few kilobytes and parsing a short command.
- **Nothing slow on the path**: no server, no `.aries.yaml`, no outbox,
  no git process, no DNS. The repository's top is found by walking up
  from `cwd` to `.git`; the policy is parsed fresh each call, since a
  cache would be one more file to keep and to trust.
- **A 5 s timeout** in the hook entry, far above the budget and far
  below Claude Code's 600 s default, so a guard that hangs fails in the
  way §6 chooses, not after ten minutes.
- `aries guard` must not grow `aries`'s start-up for every other
  command: what it links is the parser and the policy, and
  `TestInitAllocations`-style checks, as `cmd/ariesd` has, can hold it.
  A benchmark in the code session's PR records the number.

## 12. Questions for the maintainer

None is decided here.

1. **Fail open or closed.** Should a guard that cannot run or cannot
   read its policy fail open in Desktop sessions and closed for headless
   runs (§6), closed everywhere (every session of the repository blocked
   by one bad policy file), or open everywhere (headless runs fall back
   to Claude Code's rules alone)?
2. **Allow, or deny only.** Should the guard return `allow` for a
   command whose every part its own reading allows (§3), which fixes
   `VAR=value` and wrapper denials and makes the policy file the allow
   list too, or only deny, leaving every approval to Claude Code's
   prefix rules and the brief's "issue commands bare"?
3. **Unreadable commands.** Deny in both kinds of session (§5), or deny
   headless and `ask` in a Desktop session, where a person can answer?
4. **No policy file.** With the hook written and no
   `.aries/guard.yaml`, should the guard do nothing (as proposed), or
   apply built-in deny rules (merges, `gh api`, force and `main`
   pushes)? And should the dispatcher refuse to run without the file, or
   fall back to the same built-in rules?
5. **A second layer.** Should Claude Code's own deny rules be kept as a
   coarse copy of the policy's, in the dispatcher's
   `--disallowedTools` and, through `aries setup`, in
   `.claude/settings.json`'s `permissions.deny`, so that they hold when
   `aries` is missing, at the price of a second copy that a test must
   keep in step?
6. **Which copy of the policy.** Should a headless run's guard read a
   copy the dispatcher made at launch, outside the worktree (§8), and a
   Desktop session the file in its worktree? Or should every session
   read the file at `origin/main` through git, which costs a git process
   per call and trusts a ref the agent can move? And is the policy file,
   like `.claude/`, a path only the maintainer changes (a CODEOWNERS
   entry), so that widening what agents may run is always a person's
   review?
7. **Counting denials.** Should the guard record denials (rule number
   and session, never the command) for the outcome record, which means
   it writes a file on the hot path, or stay write-free?
8. **Where the rules live.** A file of its own, `.aries/guard.yaml`, as
   proposed, or a section of `.aries/workflow.yaml`, which the server
   applies and versions (factory-review.md question 3)? A file read
   locally works offline and needs no admin; the workflow setting is the
   project's one record of its rules.
9. **The parser.** The design proposes a small `sh` parser of aries's
   own (§5), on the standard library, fuzzed like `internal/jose`.
   Should the code session instead propose `mvdan.cc/sh`, a full and
   maintained parser (BSD-3-Clause), as a new dependency under rule 4,
   for a person to admit or refuse?
10. **Codex and Gemini.** Ship the deny-only guard for both in the same
    slice as Claude Code's, or after the dispatcher drives them?
