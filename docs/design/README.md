# Design documents

These documents say why starfix is built the way it is. The user guides in
[docs/](../../README.md#documentation) say how to use it.

| Document | Covers |
|---|---|
| [starfix.md](starfix.md) | The plan: principles, architecture, data model, bd parity, MCP tools, memory, coordination, offline use, upgrades, developer experience, cost tracking, the stages and the decisions |
| [database.md](database.md) | Why Dolt; the Postgres fallback; vector search and embeddings |
| [bearings.md](bearings.md) | Bearings, the orchestrator planned on top of starfix, for Linux and macOS (Windows through WSL2): components, defaults, plan, decisions and open questions |
| [enterprise-review.md](enterprise-review.md) | A review of `sfx`, `starfixd` and Bearings against the goal of a tool a team or an enterprise can use, as of 12 October 2026: what is built, what is only designed, what is missing, and what to do first. It decides nothing |
| [access-research.md](access-research.md) | Research, 13 October 2026: how people and agents could sign in to a server (keys, a company's certificates, a sign-on by `sfx login`, OpenPubkey, brokers), each identity provider, managing users, the operator's journey and the threats, with what was measured in Docker. It decides nothing and nothing in it is built |
| [profile-starfixd.md](profile-starfixd.md) | Research, 13 October 2026: what starfixd holds in memory and what bounds it, what a request allocates and how many statements it sends Dolt, how the store's reads and writes grow with a project, what a soak retains, what the garbage collector costs, and the changes worth making, ranked, with the benchmarks that reproduce each number. It decides nothing and changes no product code |
| [profile-sfx.md](profile-sfx.md) | Research, 13 October 2026: what `sfx` costs in memory, start-up time and CPU as a short command and as `sfx mcp`, alone and 200 at once; what a long session keeps; the binary; and proposed optimizations ranked by saving against risk, with the benchmarks that reproduce the numbers. It changes no product code and decides nothing |
| [ci-analysis.md](ci-analysis.md) | Research, 13 October 2026: where CI and the test suite spend time, CPU and memory, from GitHub's records and from runs on one machine; what pull request #92 changed; what a test run leaves behind; profiles of the test binaries; and changes to the workflows, ranked. It decides nothing; a note at its head says which of its proposals have since been built |
| [spike/dolt/RESULTS.md](../../spike/dolt/RESULTS.md) | Stage 0: how Dolt behaves under concurrent writers, and the rules starfixd follows because of it |

## How the documents change

- **The plan stays a plan.** Where the build departs from a design, the
  section gets a dated **As built** note instead of a rewrite, so the
  reasoning stays readable ([AGENTS.md](../../AGENTS.md#rules), rule 17).
- **The status lives in the README.** The [roadmap](../../README.md#roadmap)
  says which stage is in progress; `starfix.md` §13 holds the plan behind
  it.
- **Decisions are numbered.** `starfix.md` §14 and `bearings.md` §7 list
  them, and code comments cite them, for example "decision D2".
