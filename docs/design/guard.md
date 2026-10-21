# The guard: one permission policy for every agent session

Status: design, 21 October 2026 (aries-kqoyjyft), decided the same day:
the maintainer accepted the ten recommendations, recorded in §12, and
the sections below state what was chosen. Nothing in it is built; §13
is the plan. It changes what an unattended agent may run, so it is held
for the maintainer's review under the memory `merges`.

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
`cwd`, reads the session's snapshot of the policy (§8) and gives one of
four answers:

| Answer | Output | When |
|---|---|---|
| Deny | `permissionDecision: "deny"` with the reason and fix (§7), exit 0 | Any part of the command, read through its wrappers (§5), matches a deny rule; or a file tool's path matches a path rule; or, in a headless run, a part cannot be read |
| Ask | `permissionDecision: "ask"` with the reason, exit 0 | In a Desktop session, a part of the command cannot be read (§5): the person answers |
| Allow | `permissionDecision: "allow"`, exit 0 | Every part of the command matches an allow rule after the guard's own reading |
| No decision | Nothing, exit 0 | None of these: Claude Code's own rules and the session's mode decide, as they do today |

The guard both allows and denies (decision 2): the policy file is the
allow list as well as the deny list. An `allow` is what fixes the
denials of §1 without a brief line about `VAR=value`:
`GOTOOLCHAIN=local go test ./...` and `scripts/leftovers.sh go test
./...` are allowed because the guard reads the assignment and the
wrapper, while Claude Code's deny rules, which keep a coarse copy of the
policy's (§8), stay in force under it.

A headless run is told apart from a Desktop session by the snapshot's
origin: the dispatcher writes the run's and marks it headless (§8).
`permission_mode` alone would not do, since a person may run a Desktop
session in `dontAsk`.

It never calls the server, reads no `.aries.yaml`, opens no network
connection and starts no git process. The one file it writes is the
denial count (§7). With no policy file, it applies its built-in denies
(decision 4): `gh pr merge`, `gh api`, force pushes and pushes to
`main`, each with its fix, and decides nothing else. Outside a git
repository it prints nothing, as `aries prime --hook` does outside an
Aries repository.

## 4. The policy file

`.aries/guard.yaml`, a file of its own beside `.aries/workflow.yaml` and
`.aries/brief.txt` (factory review, step 4; decision 8: the workflow
setting may point at it later, once aries-zxxoccdq settles where
project rules live). It is the repository's, is changed by pull request
with the maintainer's review (a CODEOWNERS entry, §13), and is read with the YAML module `aries`
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
- **Built in, always.** Whatever the file says, edits to the policy
  file and to the guard's snapshot directory (§8) are denied, through
  the file tools and through Bash's redirections and `tee`, and so are
  the built-in denies of §3.

## 5. How a command is read

The guard parses the command as POSIX `sh` (with bash's `[[`, `|&` and
`$'…'`), in Aries's own small parser on the standard library, and checks
every simple command it finds (decision 9: no new dependency). The
parser is conservative: what it is not sure of, it calls unreadable. It
is fuzzed as `internal/jose` is, with a target that holds it never to
call readable a command whose words it got wrong. No `sh` runs, and nothing is expanded
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
or one it fails to parse. When any part of a command is unreadable,
since that part could be anything (decision 3), a headless run's
command is denied with the fix "write it as plain commands, one program
each", and a Desktop session's gets `ask`, with the same text as the
reason, so the person decides.

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
| `eval "gh pr merge 1"`, `$CMD pr merge 1`, `sh <<EOF` | headless: deny, unreadable; Desktop: ask |
| Any command, with no policy file | `gh pr merge`, `gh api`, force and `main` pushes denied; the rest no decision |
| Edit or Write of `.claude/settings.json`, `.mcp.json`, `.aries/guard.yaml`; `echo x > .mcp.json` | deny: the maintainer's |

## 6. Failing open or closed

Three things can go wrong, and the design treats them apart:

1. **The guard does not run**: `aries` is not on the session's `PATH`,
   it crashes, or it passes its timeout. Claude Code lets the call go
   ahead with a `hook error` notice, unless the hook sets
   `"onFailure": "block"` (§2).
2. **The guard runs and cannot decide**: the session's snapshot is
   missing, unreadable, invalid or of a newer version, or the hook's
   input is not the JSON it expects. (No policy file on `origin/main` is
   not this case: the built-in denies apply, §3.)
3. **The command cannot be read** (§5). This is not an error: it is
   denied headless and asked in a Desktop session, with a fix.

For 1 and 2 the guard fails **open in a Desktop session and closed for
a headless run** (decision 1):

- A Desktop session has a person who sees the notice, and Claude Code's
  own rules and mode still apply. Failing closed there would let one bad
  merge to `.aries/guard.yaml` refuse every Bash and Edit call in every
  session of the repository, the session sent to fix it included. The
  guard applies its built-in denies, exits 0 and prints a
  `systemMessage` of one line, `aries guard: policy not applied:
  .aries/guard.yaml: line 12: unknown key "alow"`, which the person
  reads, as `prime --hook` turns its errors into one line (CLAUDE.md).
  It holds no command text. `prime --hook` checks the snapshot it takes
  (§8) and says the same at the session's start.
- A headless run has no one to read a notice, and its claim and branch
  run unattended. Before it starts a run, the dispatcher runs
  `aries guard --check` in the run's worktree, which loads the policy
  as the hook would and runs the table of §5's starting rows; a failure
  starts no run, with the check's fix in the dispatcher's log and
  status. The run then gets `--settings` with the same hook and
  `"onFailure": "block"`, so a guard that breaks mid-run blocks rather
  than waves through. That needs Claude Code 2.1.295 or later on the
  dispatcher's machine, which `--check` tests, refusing an older one
  with a fix.

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
- Each denial appends one line to `guard/denials.jsonl` in aries's data
  directory (0600, in a 0700 directory, as the dispatcher's log is):
  the time, the session, the policy's rule number (or `builtin` or
  `unreadable`) and the tool, never the command, a path or a value
  (decision 7). A failed write is ignored and changes no answer. The
  file is bounded like the dispatcher's log, and the outcome record
  (outcome-record.md) counts each run's denials from it by session.
  An allow, an ask or no decision writes nothing.

## 8. One policy file, two readers

**Which copy is read** (decision 6). Never a file the agent can edit,
and no git process per call: each session reads a snapshot of
`.aries/guard.yaml` taken from `origin/main` once, at its start, with
one `git show origin/main:.aries/guard.yaml`. The snapshot is kept
under aries's data directory, in `guard/SESSION.yaml` (0600), outside
every worktree, and the built-in rule of §4 denies writes to it:

- A headless run's snapshot is taken by the dispatcher at launch,
  marked headless, and named in the run's environment by
  `ARIES_GUARD_POLICY`, which a tool call cannot change for the harness.
- A Desktop session's is taken by `prime --hook` at session start, by
  the session id the hook's input carries; the guard finds it by the
  same id.
- A change to the policy reaches a session only through a merge to
  `main`, which the CODEOWNERS entry (§13) puts under the maintainer's
  review, and only at the session's next start. A session that began
  before the merge keeps its snapshot.
- A worker's edits to `.aries/guard.yaml` in its own branch change
  nothing it may run; they are a pull request like any other.

**The dispatcher** reads the run's snapshot and builds the run's lists
from it:

- `--allowedTools`: the `tools:` names, and each `run:` allow rule as
  Claude Code's `Bash(words *)`, for the forms Claude Code matches
  itself (the guard's `allow` covers the rest).
- `--disallowedTools`: the coarse copy of the policy's denies (decision
  5): each word-only deny rule as `Bash(words *)`, each `path:` as
  `Edit(path)`, and for rules with `flags:`, `refs:` or `config:`, which
  have no faithful prefix form, the nearest coarse form, as today's
  `Bash(git push --force:*)` and `Bash(find*-exec*)` are. The guard
  remains the exact check; the copy is the layer that holds when the
  guard does not run.
- `DefaultAllowed` and `DefaultDenied` go from `claude.go`. A
  repository with no policy file gets no run, with the fix "add
  .aries/guard.yaml (`aries guard init` writes a starting one)", as the
  dispatcher refuses without `.aries/brief.txt` (decision 4).
  `TestDefaultPermissions` becomes the guard's table test, and its
  `matches` helper, a model of Claude Code's matching, stays to check
  the rendered lists.

**`aries setup claude-code`** writes the PreToolUse hook of §3 and the
same coarse copy of the denies into `.claude/settings.json`'s
`permissions.deny` (decision 5); `--check` reports either missing or out
of step. Deny rules need no workspace trust, so they hold in a `-p` run
in a folder never trusted (§2). It writes no `permissions.allow`: the
guard's `allow` is the one allow list. One function renders the coarse
copy for both, and a test holds the dispatcher's `--disallowedTools`
and the repository's `permissions.deny` to the policy file, so the two
copies cannot drift.

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

Claude Code comes first (decision 10). Codex and Gemini get their
guards when the dispatcher drives them, and until then `aries setup
codex` and `aries setup gemini` write only their SessionStart hooks and
say the guard is not installed. When they come, both get a deny-only
guard, with the same policy and fixes in each harness's shape. Neither gets the guard's `allow`: a Codex or Gemini session's
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
  no git process, no DNS (decision 6: the one `git show` is at the
  session's start, §8). The repository's top is found by walking up
  from `cwd` to `.git`; the session's snapshot is parsed fresh each
  call, since a parsed cache would be one more file to keep and to
  trust. Only a denial writes, one appended line (§7).
- **A 5 s timeout** in the hook entry, far above the budget and far
  below Claude Code's 600 s default, so a guard that hangs fails in the
  way §6 chooses, not after ten minutes.
- `aries guard` must not grow `aries`'s start-up for every other
  command: what it links is the parser and the policy, and
  `TestInitAllocations`-style checks, as `cmd/ariesd` has, can hold it.
  A benchmark in the code session's PR records the number.

## 12. Decisions (maintainer, 21 October 2026)

The maintainer accepted the ten recommendations of the questions this
section first held (a comment on aries-kqoyjyft, 21 October 2026):

1. **Fail open or closed:** open in Desktop sessions, closed in headless runs (§6).
2. **Allow and deny:** the guard allows a command whose every part it reads and allows, and denies; the policy file is the allow list too (§3).
3. **Unreadable commands:** denied in headless runs, `ask` in Desktop sessions (§5).
4. **No policy file:** built-in denies (merges, `gh api`, force pushes, pushes to `main`); the dispatcher refuses to start without the file, as without `.aries/brief.txt` (§3, §8).
5. **A second layer:** a coarse copy of the denies in the dispatcher's `--disallowedTools` and, through `aries setup`, in `.claude/settings.json`'s `permissions.deny`, held in step by a test (§8).
6. **Which copy:** a snapshot from `origin/main` once at session start, by the dispatcher at launch and by `prime --hook` for Desktop sessions; never a file the agent can edit, no git process per call; CODEOWNERS entries for `.aries/guard.yaml` and `.aries/brief.txt` (§8, §13).
7. **Denials counted:** one appended line per denial with the rule number and the session, never the command; write errors ignored; the count feeds the outcome record (§7).
8. **Where the rules live:** `.aries/guard.yaml`, a file of its own; the workflow setting may point at it once aries-zxxoccdq settles where project rules live (§4).
9. **The parser:** Aries's own small `sh` parser on the standard library, conservative and fuzzed like `internal/jose`; what it cannot read falls under decision 3; no new dependency (§5).
10. **Harnesses:** Claude Code first; Codex and Gemini get their guards when the dispatcher drives them (§9).

## 13. Plan

The code is a separate issue the coordinator files; these slices are
its suggested order, each a pull request of its own.

1. **For the maintainer, before G1 merges:** add `.github/CODEOWNERS`
   (the repository has none) with entries that require the
   maintainer's review for `.aries/guard.yaml` and `.aries/brief.txt`,
   and turn on "Require review from Code Owners" for `main` if it is
   not on. `.github/` is the maintainer's to change, so no agent edits
   it.
2. **G1, the guard.** `aries guard --hook` and `--check`: the parser
   (§5) with its fuzz target, the policy file's loader (§4), the
   built-in denies, the four answers (§3), the reasons (§7), the denial
   line, and the starting `.aries/guard.yaml` holding today's
   `DefaultAllowed` and `DefaultDenied` with fixes. The table test of
   §5 and the latency benchmark of §11. `docs/cli.md` gains the command.
3. **G2, the snapshot and setup.** `prime --hook` takes the Desktop
   snapshot (§8); `aries setup claude-code` writes the PreToolUse hook
   and the coarse `permissions.deny`, and `--check` reports both; the
   test that holds the coarse copy to the file.
4. **G3, the dispatcher.** It takes the headless snapshot at launch,
   refuses a repository without the file, runs `aries guard --check`
   before each run, passes `--settings` with `"onFailure": "block"`,
   renders its lists from the snapshot, and drops `DefaultAllowed` and
   `DefaultDenied`; the brief template's line about `VAR=value` can then
   go, raising its version.
5. **Later.** The outcome record counts denials by session; Codex's and
   Gemini's guards when the dispatcher drives them (§9).
