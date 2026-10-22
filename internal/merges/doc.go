// Package merges is the merge rule's classifier (docs/design/merges.md,
// slice M1): for one open pull request, queue or hold, naming the
// condition that failed. `aries prs merges` prints its verdicts; it
// queues nothing (that is M2).
//
// The rule is a file (rules.go): a version and named classes, each with
// the conditions it checks. A pull request is queued when one class
// passes it and the product's floor (floor.go), which no file lifts,
// does not hold it. The classifier (classify.go) is a pure function of
// the rule and what was read of the pull request, so that every caller
// reads the rule one way.
//
// It fails closed: a pull request it could not read, a path it cannot
// classify, a check it cannot see or an issue it cannot read is a hold,
// with the reason. The opener (who started the session that opened the
// pull request) has no trusted source until briefs are stored
// (aries-2qx4mlm2), so it is reported as unverified and never satisfies
// a condition (§6, decision 5).
//
// The pull request is read through [Forge]; [GH] reads GitHub with the
// `gh` on PATH, through `gh pr list`, `gh pr checks` and `gh pr diff`,
// never `gh api`.
package merges
