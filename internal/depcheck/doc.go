// Package depcheck holds the repository to its dependency policy
// (AGENTS.md, rule 4; docs/design/supply-chain.md, §7.2 and slice SC1):
// every module go.mod requires directly, every tool the gate runs and
// every GitHub Action has a line in deps.allow, what aries and ariesd
// link is recorded in testdata, and no tool is fetched by a version
// string that go.sum does not cover. It is tests only; this file holds
// nothing but the package's name.
package depcheck
