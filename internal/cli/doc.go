// Package cli is aries, the Aries client: the commands people run in a
// terminal, `aries mcp` for agents, `aries prime` and its SessionStart hook,
// `aries usage --hook`, which sends Claude Code's token counts, `aries setup`
// and `aries upgrade`. cmd/aries only wires the process to
// [Run].
//
// [Run] executes one command line in an [Env], which holds everything the
// package takes from the process: its streams, environment variables,
// host name, home directory, platform and executable. Nothing here touches
// the process's own streams. A command connects to ariesd on first use,
// over SSH with the pinned host key (docs/design/aries.md §2), and the
// connection is closed when the command returns.
//
// Issue text, names and server messages are written by other people, so
// everything printed is escaped (§7, "As built (untrusted text)"): text
// output passes through a [safetext.Writer], and each single-line field is
// escaped on its own, so it cannot start a line. With --json a command
// prints one JSON document, errors included, written with [safetext.JSON];
// `aries watch` prints one per line, an object per event. A failure prints
// an `aries:` line and, when the error has one, a `fix:` line naming the
// next step.
package cli
