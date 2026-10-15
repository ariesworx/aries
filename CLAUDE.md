@AGENTS.md

## Claude Code specifics

- This file only imports `AGENTS.md`; put shared guidance there.
- Claude Code sets `CLAUDE_CODE_SESSION_ID`, so `aries` and `aries mcp` run
  from a session take it as their session id; `ARIES_SESSION` wins if
  set (`internal/client/session.go`).
- This repository commits no `.aries.yaml`, so `aries setup claude-code`
  finds none here (it also searches parent directories) and refuses. Try
  setup changes in a scratch repository that has one, with
  `aries -C DIR setup claude-code`; it only prints unless given `--write`.
- `aries prime --hook` is Claude Code's SessionStart hook, and
  `--hook=AGENT` the other harnesses'. It always exits 0, prints nothing
  outside an Aries repository, and otherwise prints one JSON document in
  the harness's format (`Agent.HookOutput`), turning any error into a
  one-line note. Keep it that way (`internal/cli/agent.go`).
