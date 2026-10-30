// Package dispatch is the client-side dispatcher's loop
// (docs/design/dispatcher.md; decision 96): `aries dispatch run`, an
// internal, experimental command that keeps agents busy between waves,
// within a budget, with no model in its loop.
//
// The loop is throwaway: Bearings' B0 replaces it, so it is kept apart
// from internal/cli, which only wires it to the process. What it calls is
// permanent and lives elsewhere: `aries watch --events`, the locks
// dispatch/runs, dispatch/pause and pgtest/HOST (`aries lock`), claims
// (`aries start`), `aries cost`, and internal/credential for the API key.
// It reaches them through the aries binary itself ([Exec]), as a script
// would, so each run's claim is taken under the run's own session.
//
// One pass of the loop is [Dispatcher.Wake]: reconcile its runs with
// Aries and git, then, while a slot is free, the budget allows and
// nothing paused it, pick a ready issue ([Judge]), claim it, make its
// worktree, and start a headless run under the lock ([Claude]). With
// Config.DryRun it only says what it would start, skip and queue, and
// why. [Dispatcher.Loop] wakes it on inbox items and issue events, a
// run's exit, the default branch moving, a pause released or a slot
// freed while it holds for one, and a fallback timer.
//
// It runs on macOS and Linux: runs are process groups, interrupted by
// signal. It builds everywhere, and on Windows the command refuses.
//
// At the start, when the default branch moves, and at the next wake when
// the last one left a PR it may act on, the wake also sweeps (sweep.go,
// dispatcher.md §8): each open PR that conflicts or
// whose required checks failed is resumed in its own ended run's session,
// left to a live session that holds it, given a new run on its branch when
// its runs opened it or it carries [ReworkLabel], or named; each rework
// posts a rework: comment naming the merge.
//
// Not here yet: merges under dispatch rule 4 and the notices on a
// standing issue (dispatcher.md §8, §12), which wait on merges.md.
package dispatch
