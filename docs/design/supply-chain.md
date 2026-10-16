# Supply chain security and provenance: the groundwork for CMMC and FedRAMP

16 Oct 2026. **Status: design, proposed. Nothing in it is built, and it decides nothing the maintainer has not decided;** its decisions are listed as proposed in [§10](#10-decisions-for-the-maintainer), numbered after the last in [aries.md](aries.md) §14 (decision 89), and its open questions in [§11](#11-open-questions-for-the-maintainer). It inventories what the repository does today ([§4](#4-current-state)), names the gaps ([§5](#5-gaps)), maps each control to the frameworks an assessor will ask about ([§6](#6-compliance-mapping)), and lays out slices that can be built in order of risk reduced for the effort ([§8](#8-specification-the-slices)).

**Why a document of its own.** Aries coordinates an organization's agentic coding: every repository its agents work in takes instructions, memories and handoffs from it, and Bearings ([bearings.md](bearings.md)) will dispatch agents, hold model credentials and merge their work. A compromised `aries` runs on every developer's machine and inside every agent's harness; a compromised `ariesd` steers every agent in every project it serves. That makes the two binaries, and the code that reaches them through 50 modules, a build on rented machines and an installer fetched over HTTPS, a high-value target for a supply chain attack, and it makes the evidence that they were not tampered with part of what an enterprise or an agency will ask for before it deploys them. [security-model.md](../security-model.md) says what the running system protects; this document says how the software itself is kept honest from source to install, and what an assessor can be shown.

**Evidence.** What is marked **Observed** was read from the repository on 16 Oct 2026, citing files. What is marked **Read** comes from a standard's, a vendor's or a reporter's publication, fetched on the same day and cited in [§13](#13-references); where a source is secondary (a law firm's or a vendor's summary of a rule, not the rule), it says so. What is marked **Not confirmed** could not be checked against a primary source on that day and must be before anyone relies on it. The rest is design.

**What this is not.** It is not a System Security Plan, not a C-SCRM plan in NIST SP 800-161's sense, and not a claim that Aries or Bearings is authorized, certified or assessed under any program. It is the engineering groundwork: the controls, the tests that hold them, and the artifacts an assessor is handed. Whether to seek an authorization, and under which program, is the maintainer's and a customer's decision ([§11](#11-open-questions-for-the-maintainer)).

## 1. Summary

**Aries already does more than most projects its size** (§4): modules are pinned by `go.sum` and checked against the Go checksum database, `go mod verify` runs on every pull request and before every release build, govulncheck runs on every push, weekly and before a release, every GitHub Action is pinned by commit, releases are built from a clean module cache with `CGO_ENABLED=0 -trimpath`, their checksums are signed with an Ed25519 key only the maintainer's run can use, every archive carries GitHub build provenance, the OpenTelemetry SDK is confined to `ariesd` by tests, the installer checks the Postgres repository's key fingerprint, and every Docker image is pinned by digest.

**What is missing is of three kinds** (§5). First, **the dependency list is policy by convention, not by test**: nothing fails when an agent or a person adds a module, when a module gains the ability to open a socket or run a program, or when a tool run with `go run pkg@version` is fetched outside `go.sum`. Second, **the evidence an assessor asks for is not produced**: no bill of materials, no VEX statement for what govulncheck finds and rules out, no independent rebuild that shows the archives are what the source says, no written dependency policy, no `SECURITY.md`. Third, **two paths trust a mutable source**: `install.sh` and `install-server.sh` are fetched from the `main` branch, and GitHub releases can still be edited after they are published.

**The design** (§7) adds controls in the order of risk reduced for the effort, almost all of them in CI and in tests, none of them a new runtime dependency of either binary (rule 4): a dependency allowlist and a per-binary link set held by tests (slice SC1); a capability baseline from capslock, so that a dependency that learns to reach the network or run a process fails the gate (SC2); a bill of materials and a VEX statement written from the binaries' own build information by a tool in this repository, signed with the release (SC3); an independent rebuild that compares byte for byte, with the toolchain pinned by `go.mod` (SC4); an installer fetched at a tag and immutable releases (SC5); the source-side evidence, from branch rules to a security policy, with the honest statement of what one maintainer cannot attest to (SC6); a FIPS 140-3 build variant, as a question with a measured answer (SC7); the same rules written down for the TypeScript front end and the containers before either exists (SC8); and, last and in 1.x, a dependency gate that Aries itself holds agent work behind, recorded in the event log (SC9). A dependency admission policy and an update and cooldown policy ([§7.2](#72-the-dependency-admission-policy), [§7.3](#73-the-update-and-cooldown-policy)) are written here and become rules in `AGENTS.md` when SC1 lands.

**The compliance picture, briefly** (§6). CMMC Level 2 assesses NIST SP 800-171 Revision 2 today, which has no supply chain family; its Revision 3 adds one (03.17), and Phase 2 of CMMC's rollout was suspended for review in July 2026 (Read, secondary). The federal attestation form of M-22-18 was rescinded by OMB M-26-05 in January 2026; agencies may still ask a vendor for an attestation or an SBOM by their own risk decision (Read, secondary). FedRAMP's 2026 Consolidated Rules replaced Low, Moderate and High with Classes B, C and D and made 20x the standard path, with two supply chain indicators, KSI-SCR-MIT and KSI-SCR-MON (Read, primary). NIST's SSDF v1.2 is a draft (Read, primary). So what is stable enough to build to is the SP 800-53 Revision 5 catalog (SR, SA, CM and SI families), the SSDF's practices, SLSA's levels and CISA's 2026 SBOM minimum elements, and that is what [§6](#6-compliance-mapping) maps to.

## 2. Scope

| In scope now | Later, designed here so the rules exist first |
|---|---|
| The Go module `github.com/ariesworx/aries`: `aries` and `ariesd`, their 50 modules, the Go toolchain and the tools CI runs | Bearings, when it has code (it is a design, bearings.md) |
| The release pipeline: `ci.yml`, `release.yml`, `internal/release`, `internal/tools/releasekey` and `releaseindex`, `install.sh`, `install-server.sh` | The web UI's TypeScript front end (bearings.md §3.20): npm, Node, the embedded assets |
| What a deployment takes from elsewhere: Postgres from the PostgreSQL project's apt repository or a built source archive, the Docker test server's images, the AppArmor profiles | Container images of `ariesd` for production, which do not exist (`deploy/docker` is a test fixture) |
| What agents can introduce: a dependency, an instruction in an issue or a memory, an MCP server or a harness plugin | Aries as the enforcement point for other repositories' dependencies (SC9) |

**Non-goals.** Writing a System Security Plan or a C-SCRM plan; assessing the identity providers, clouds or model gateways a deployment uses; hardware; the security of the harnesses (Claude Code, Codex, Gemini CLI) themselves beyond what `aries setup` writes into them.

## 3. Threat model

**The attacker's goals.** In order of what Aries is worth to them:

1. **Steer every agent.** `ariesd` is the source of `prime`, `show`, `inbox` and `recall` for every agent of every project it serves. Code that changes what the server says, or what the client prints, changes what thousands of agent sessions do next, in every repository they hold a checkout of. No credential for those repositories is needed: the agent already has one.
2. **Run on every developer's machine.** `aries` runs as the developer, with their shell, their keys in `~/.ssh` or an agent, their git credentials and their harness's configuration. `aries mcp` runs for the length of every agent session. A backdoored `aries` is a backdoored developer.
3. **Take the server.** `ariesd` reaches Postgres with the project's data, holds the control store (principals, keys, memberships, identities), writes the file sshd reads, and will hold tokens (transport.md §6) and, in Bearings, model credentials and the merge train (bearings.md §3.5, §3.24).
4. **Take the release key** or the ability to publish a release, which gives all three at once through `aries upgrade` and `ariesd upgrade`.

**The vectors.**

| Vector | How it reaches Aries | Example, from the record |
|---|---|---|
| **Source** | A maintainer's or contributor's account or token; a pull request that looks right; an agent working in this repository that commits what it was told to by an issue, a comment or a file it read; a moved tag | The `tj-actions/changed-files` tags moved to a malicious commit (March 2025); the Trivy repository wiped and its tags force-pushed by way of a leaked token (March 2026); the xz-utils maintainer takeover, with the payload only in the release tarball (2024) (Read) |
| **Dependency** | One of 21 direct or 29 indirect modules changes hands or gains a maintainer; a typosquat of a module an agent or a person means to add; a name a model hallucinates and someone registers; a module the proxy cached that no longer matches its repository | A backdoored `boltdb-go/bolt` served from the module mirror for three years after its tags were changed upstream (Feb 2025); seven typosquats of `hypert` and `layout` dropping loaders on Linux and macOS (March 2025); 19.7% of generated code samples naming a package that does not exist, with 43% of the names recurring (USENIX Security 2025) (Read) |
| **Toolchain and tools** | The Go toolchain `setup-go` downloads; `golangci-lint`, `govulncheck`, `shellcheck`, `apparmor_parser`, `gh`, CodeQL; the Actions themselves | A flaw in the `go` command's checksum validation that let an untrusted proxy or checksum database serve a malicious toolchain (CVE-2026-42501, Read, secondary: **Not confirmed** against Go's release notes) |
| **Build** | GitHub-hosted runners, a reusable workflow, the environment that holds the key; a build that is not reproducible cannot be checked by anyone but the builder | The `release` environment's trade-offs are stated in RELEASING.md: GitHub and the organization's owners can reach the key |
| **Distribution** | The releases page and its assets; `install.sh` and `install-server.sh` fetched from a branch; a static host or mirror; `raw.githubusercontent.com` | Releases on GitHub stayed editable after publication until immutable releases shipped (Oct 2025, Read) |
| **Deployment** | Postgres packages and the apt key; the Docker base images; the Keycloak, Collector and Prometheus images of the test fixture; the AppArmor profiles fetched by the installer | Covered today by a fingerprint check and digests (§4) |
| **Agent-introduced** | An agent adds a module to `go.mod`, an npm package to the web UI, an MCP server to a harness's config, a plugin to a marketplace, or a `go run` of a tool at a version it chose; an issue, comment, memory or README it reads tells it to | Prompt injection by a GitHub issue title reaching a project's publishing credentials through an AI triage bot (Cline, Feb 2026); one pattern working against three vendors' agent workflows (April 2026) (Read, secondary) |
| **Runtime** | `aries upgrade` and `ariesd upgrade` swap the binary; `install.sh` runs as the user; the Claude Desktop extension bundle (identity.md §9) will carry `aries` | Signed today; the bundle's signing is deferred (decision 85) |

**What Aries is to the agents that read it.** Every issue, comment, handoff and memory is text an agent will act on, and anyone who can write one can try to steer an agent that reads it. `internal/safetext` strips what could hide text (controls, bidi), and the agent guide says what an agent may do; neither makes an issue's words safe. That is a prompt injection channel Aries owns, and it belongs to this threat model because a dependency's README, a comment on a pull request and an Aries issue are the same vector with different doors. The mitigation here is not a filter but policy: the controls of §7 make what an agent can change to the supply chain (a module, a tool, a workflow) something a test or a person refuses, whatever the agent was told.

## 4. Current state

Everything in this section is **Observed**, on 16 Oct 2026, at commit `ff60610` with uncommitted changes in `go.mod` and `go.sum` (below).

### 4.1 The dependencies

| | Count | Source |
|---|---|---|
| Direct modules | 21 | `go.mod` at HEAD |
| Indirect modules | 29 | `go.mod` at HEAD |
| Modules in `go list -m all` | 50 at HEAD; 427 in the working tree, which adds golangci-lint as a `tool` directive and its 377 modules | `git show HEAD:go.mod`; `go list -m all` |
| Third-party modules linked into `aries` | 12: `jsonschema-go`, `modelcontextprotocol/go-sdk`, `segmentio/asm`, `segmentio/encoding`, `yosida95/uritemplate/v3`, `go.yaml.in/yaml/v3`, `x/crypto`, `x/oauth2`, `x/sync`, `x/sys`, `x/term`, `x/time` | `go list -deps -f '{{.Module.Path}}' ./cmd/aries` |
| Third-party modules linked into `ariesd` | 42, the 12 above less the MCP SDK's, plus pgx and its three, the OpenTelemetry SDK, exporters and what they bring (grpc, protobuf, genproto, grpc-gateway, prometheus client and model), `logr`, `uuid`, `backoff` | the same for `./cmd/ariesd` |

The direct list is: the MCP SDK and its JSON schema module (the agent interface), pgx (the store), the OpenTelemetry API, SDK, three OTLP exporters, Prometheus exporter and runtime instrumentation with `client_golang` and `logr` (telemetry, `ariesd` only, decision 28), `go.yaml.in/yaml/v3` (config files), `x/crypto` (SSH and Ed25519), `x/sys`, `x/term`. The reason each is there is rule 4's "stated reason in the pull request", which lives in pull requests, not in a file.

**The uncommitted change.** The working tree's `go.mod` adds `tool github.com/golangci/golangci-lint/v2/cmd/golangci-lint` and the 377 modules it needs, and `go.sum` the 490 lines that pin them. It is not this document's to make or revert; §7.1 says what it means for the design, since a `tool` directive is one of the two ways to pin a tool by `go.sum`.

### 4.2 The gate and CI

`scripts/gate.sh` runs what CI runs (AGENTS.md, Commands): `gofmt`, `go vet`, golangci-lint v2.14.0 (errcheck, govet, staticcheck, gosec, revive, ineffassign, unused; `.golangci.yml`), `go mod verify`, shellcheck, `govulncheck@v1.8.0`, cross-OS vet and build, and the tests under `scripts/leftovers.sh`. Tests in `scripts/` fail when the script, `AGENTS.md`'s block and `ci.yml` differ (`TestGateMatchesAgents`, `TestGateMatchesCI`, `TestGovulncheckPin`). CodeQL runs with `security-extended` on every push and weekly (`codeql.yml`); govulncheck runs weekly on `main` (`vuln.yml`). Dependabot opens weekly pull requests for Go modules and Actions, minor and patch grouped, with no `cooldown` key (`.github/dependabot.yml`); GitHub's documentation says version updates now wait 3 days after a release by default (Read; whether that default applies without the key is **Not confirmed**). Every `uses:` in every workflow is pinned by a 40-character commit with a version comment (`ci.yml`, `release.yml`, `codeql.yml`, `vuln.yml`, `cla.yml`). CI installs Postgres from the PostgreSQL project's apt repository only after checking the downloaded key's fingerprint against `PGDG_KEY_FINGERPRINT` (`ci.yml`), which `scripts/installserver_test.go` keeps equal to the installer's. `setup-go` reads the Go version from `go.mod`'s `go 1.27` line and installs the newest patch release; the toolchain's own checksum is whatever `setup-go` checks, which this document did not read (**Not confirmed**). `ci.yml` and `scripts/gate.sh` set no `GOTOOLCHAIN`, `GOFLAGS` or `GOPROXY`; `go.mod` has no `toolchain` directive; the Dockerfile sets `GOTOOLCHAIN=local`.

### 4.3 The release

`release.yml` runs the whole gate on the tagged commit, then builds from a clean module cache (`go mod download && go mod verify`, `govulncheck`), eight static builds with `CGO_ENABLED=0 -trimpath -ldflags "-s -w -X …Version=TAG"`, VCS information kept (`go version -m` shows the commit), archives with fixed order, owner and `SOURCE_DATE_EPOCH` from the commit, `checksums.txt`, and `actions/attest-build-provenance` over the checksums; then stops. A second run, started by hand by the one login named `RELEASE_APPROVER`, at a `v*` tag only repository admins can push (a tag ruleset), checks that the build run is this workflow's own for this tag and commit and green, downloads its archives, checks them against `checksums.txt`, signs it with `ARIES_RELEASE_KEY` from the `release` environment through `internal/tools/releasekey` (which refuses a key `keys.go` at the tag does not list), and creates the GitHub release with `--verify-tag`. `internal/release/workflow_test.go` pins the checks the signing job makes before it reads the key. RELEASING.md states the trade-off: GitHub and the organization's owners can reach the key. The attestation is made by the release workflow itself, not a reusable workflow, which GitHub's documentation describes as SLSA v1 Build L2, with the reusable-workflow pattern needed for L3 (Read). The repository is public, so the attestation is in Sigstore's public transparency log; for a private repository GitHub uses its own Sigstore instance with no log (Read).

### 4.4 What clients check

`aries upgrade` and `ariesd upgrade` accept a release only when `checksums.txt.sig`, a raw Ed25519 signature, verifies against a key in `internal/release/keys.go`, and the archive's sha256 is in `checksums.txt`; the archive's name carries the version, so one release's signed file cannot stand in for another's (`internal/release/verify.go`, `doc.go`). Neither installs a version older than the one running. Where a release is read from is not trusted: GitHub's API, or a static tree with no redirect followed and HTTP only to loopback (`static.go`); `internal/tools/releaseindex` checks what a client would before it writes a tree. `install.sh` carries the same public key as a PEM, checks the signature when OpenSSL 3 is present, and warns or fails (`--require-signature`) when it is not; a test keeps its key in `keys.go`. `install-server.sh` holds no key, installs through `install.sh --require-signature`, and checks the PGDG fingerprint with `gpg` into a keyring of its own. Both scripts are fetched from `https://raw.githubusercontent.com/ariesworx/aries/main/…`, as README.md and docs/install.md say; `install-server.sh` also fetches `install.sh` and the AppArmor profiles from that `main` URL when run outside a checkout (`raw_url`, line 118). [Verify a download by hand](../install.md#verify-a-download-by-hand) gives `releasekey verify`, `sha256sum -c` and `gh attestation verify`.

### 4.5 Confinement, pins and the rest

- Rule 4 confines the OpenTelemetry SDK to `internal/telemetry`; `TestAriesLinksNoSDK`, `TestAriesdLinksTheSDK` and `TestOnlyTelemetryImportsTheSDK` (`internal/telemetry/boundary_test.go`) enforce it. `internal/server/boundary_test.go` holds pgx to the store and `cmd/ariesd`.
- `internal/version` pins the Postgres release and its source archive's SHA-256 (`postgres.sha256`); `scripts/install-postgres.sh` verifies the archive before building it; tests keep CI's major version, the installer's, the guide's and the compose file's digest in step (RELEASING.md, Changing the Postgres pin).
- `deploy/docker`: the `golang` build image, `ubuntu:24.04`, `postgres`, the Collector, Prometheus and Keycloak are each pinned by digest with a comment naming the release. The test server builds from the checkout and is "for testing only, never for production".
- `deploy/apparmor` confines `ariesd` on Ubuntu; CI checks the profiles' syntax.
- Rule 2 keeps text out of telemetry; `internal/secretscan` lints memories and the outbox for credentials.
- `internal/lazyregexp` and `TestInitAllocations` bound what the binary does at start.
- No `SECURITY.md`, no `CODEOWNERS`, no OpenSSF Scorecard workflow, no bill of materials, no VEX, no rebuild check, no capability analysis, no written dependency policy beyond rule 4's sentence and the pull request template's "No new dependencies" line.

### 4.6 What agents can do today

An agent reaches the server only over MCP (rule 12): it may add and list gates and never pass one; a human gate is passed only from the session `cli`. The `go-engineer` agent specification delegates Go work in this repository to an agent that runs the tests and the gate; nothing stops it from adding a module, only the gate's `go mod verify` (which checks the cache, not the policy) and a reviewer's eye on the pull request. `aries setup` writes an MCP entry that runs the `aries` binary itself; no other MCP server is configured by Aries. The `aries-ops` plugin is a set of Markdown files read by Claude Code, with no code; `scripts/opsskill_test.go` holds it to the guides.

## 5. Gaps

Each gap names the slice that closes it.

| # | Gap | Risk | Slice |
|---|---|---|---|
| G1 | No test fails when a module is added, removed or replaced, or when a module newly reaches `aries` rather than `ariesd` alone | An agent or a person adds a module and the gate passes; the per-binary confinement holds only for the OpenTelemetry SDK and pgx | SC1 |
| G2 | `govulncheck` runs as `go run …@v1.8.0`, which the Go checksum database verifies but `go.sum` does not record; the golangci-lint action fetches a binary by version | A tool the gate runs is pinned by a version string and the checksum database's word, not by a hash in this repository | SC1 |
| G3 | No `toolchain` line in `go.mod` and no `GOTOOLCHAIN` in CI: `setup-go` installs the newest 1.27.x, and the `go` command may download a newer toolchain if a module asks | The toolchain that built a release is not stated in the source at that tag; a rebuild cannot know which to use | SC1, SC4 |
| G4 | No capability analysis: a dependency that gains `os/exec`, a listener, `unsafe` or a `//go:linkname` is indistinguishable from one that did not | The `boltdb-go` and `hypert` cases were loaders: new capabilities in a patch release | SC2 |
| G5 | No bill of materials, no VEX | An assessor or a customer asks for one and gets `go version -m`; a scanner reports a vulnerability govulncheck already showed unreachable, with no statement to say so | SC3 |
| G6 | No independent rebuild: the archives are checked against the builder's own checksums and attestation, never against a second build | A compromised runner or action produces archives that verify, are attested and are signed | SC4 |
| G7 | `install.sh` and `install-server.sh` are fetched from `main`; the AppArmor profiles too | Whoever can push to `main` (or GitHub) changes what every new install runs as root, with no signature between them; the release key never covers the installer | SC5 |
| G8 | GitHub releases are not immutable; the release attestation that immutable releases add is not made | An asset can be replaced after publication; the signature catches it for `aries upgrade`, but a person following "Download by hand" without verifying does not | SC5 |
| G9 | No `SECURITY.md`, no vulnerability disclosure path, no `CODEOWNERS`, no Scorecard, no written statement of branch rules; one maintainer | Nothing to hand an assessor for SR-8, RA-5's reporting side or SLSA's source track; two-party review is not possible as the project stands | SC6 |
| G10 | Dependabot has no explicit cooldown; `go get` and `go mod tidy` by an agent have none | A malicious version is taken within hours of publication, before anyone has looked (the npm worms of 2025 were caught in hours to days) | SC1, §7.3 |
| G11 | No FIPS 140-3 build; `x/crypto/ssh` and the Ed25519 release signature use the standard library's crypto in its default mode | A FedRAMP or CMMC deployment that must use validated cryptography cannot use Aries as shipped | SC7 |
| G12 | No rule for npm, Node or container images, and the web UI's build is designed in bearings.md §3.20 alone | The first `package.json` sets the policy by accident | SC8 |
| G13 | Aries cannot hold work behind a dependency check; a `ci` gate can carry one only if someone writes the job | Every repository Aries coordinates relies on its own CI's judgment of what an agent added | SC9 |

## 6. Compliance mapping

The map is from the control this document proposes to what an assessor will cite. **Vendor** means evidence the Aries project produces and ships; **operator** means what the deploying organization configures or writes into its own plan. Control names and identifiers below are checked against the sources in §13; FedRAMP's 2026 reference pages give each SP 800-53 control's classes (Read, primary), and the consultant mapping of Class B to Low and Li-SaaS, C to Moderate and D to High is secondary (**Read**, not primary). SSDF identifiers are v1.1's, since v1.2 is a draft. SP 800-171 Revision 2 identifiers are its 3.x requirements; Revision 3's are its 03.xx.yy.

| Control (slice) | SP 800-53 r5 (FedRAMP classes) | SP 800-171 r2 → r3 | SSDF v1.1 | SLSA v1.2 | Who |
|---|---|---|---|---|---|
| Dependency allowlist with reasons; per-binary link sets; `go mod tidy` held clean; tools pinned by `go.sum`; toolchain pinned (SC1) | CM-7 Least Functionality (B, C, D), CM-7(5) Authorized Software, allow-by-exception (C, D), CM-8 System Component Inventory (B, C, D), SA-15 Development Process, Standards, and Tools (C, D), SR-3 Supply Chain Controls and Processes (B, C, D), SR-5 Acquisition Strategies, Tools, and Methods (B, C, D) | 3.4.8 (allow-by-exception), 3.4.9 (user-installed software), 3.4.1 (baseline) → 03.04.08, 03.04.01, 03.17.03 | PO.3 (toolchains), PW.4 (reuse well-secured software), PW.6 (build tool configuration) | Build: producer's consistent build process | Vendor |
| Capability baseline, golden per binary (SC2) | SA-11 Developer Testing and Evaluation (C, D), SA-15(3) Criticality Analysis (C, D), SR-6 Supplier Assessments and Reviews (C, D), SR-10 Inspection of Systems or Components (B, C, D) | — → 03.17.03 | PW.4, PW.8 (test executable code), PO.3 | — | Vendor |
| SBOM and VEX written at release, inside the signed checksums (SC3) | SR-4 Provenance and SR-4(3) Validate as Genuine and Not Altered (no FedRAMP class), CM-8 (B, C, D), SA-10 Developer Configuration Management (C, D), SI-2 Flaw Remediation (B, C, D), RA-5 Vulnerability Monitoring and Scanning (B, C, D) | 3.14.1 (flaw remediation), 3.11.2 (vulnerability scans) → 03.14.01, 03.11.02, 03.17.01 | PS.3 (archive and protect each release; PS.3.2 is the bill of materials), RV.1 (identify vulnerabilities), PW.4.4 | Build L1 and up: provenance identifies the package by digest | Vendor; the operator keeps it with their inventory (M-26-05 lets an agency ask for one) |
| Independent rebuild and the comparison record (SC4) | SI-7 Software, Firmware, and Information Integrity (C, D) and SI-7(1) Integrity Checks (C, D), SR-4(3), SR-9 Tamper Resistance and Detection (D), SA-10 | 3.14.1, 3.13.x (none exact) → 03.14.01, 03.17.03 | PS.2 (integrity verification mechanism), PW.6 | Build L3 is the platform's claim; a rebuild is the consumer's check of it | Vendor |
| Installer at a tag, immutable releases, release attestation (SC5) | SR-11 Component Authenticity (B, C, D), SI-7(15) Code Authentication (D), CM-14 Signed Components (D), SR-4(3) | 3.14.1 → 03.17.03 | PS.2, PS.3.1 | Build L2: provenance authentic, distributed | Vendor; the operator verifies with `releasekey verify` and `gh attestation verify` or pins a mirror |
| Security policy, disclosure path, branch rules stated, Scorecard, cooldown, signed tags (SC6) | SR-8 Notification Agreements (B, C, D), SR-2 Supply Chain Risk Management Plan (B, C, D; the operator's), SA-8 Security and Privacy Engineering Principles (B, C, D), RA-5(2) and SI-5 Security Alerts (B, C, D) | 3.14.3 (alerts and advisories) → 03.14.03, 03.17.01 | PO.1, PO.4, PS.1 (protect all forms of code), PW.7 (code review), RV.1.3 | Source L1 (version control) and L2 (history, provenance, no force-push) are reachable; L3 needs the controls recorded; L4 (two-party review) is not reachable with one maintainer | Vendor |
| FIPS 140-3 build variant (SC7) | SC-13 Cryptographic Protection (B, C, D), IA-7 Cryptographic Module Authentication (B, C, D), SC-8(1) | 3.13.11 (FIPS-validated cryptography) → 03.13.11 | — | — | Vendor builds it; the operator chooses it |
| npm and container rules before they are needed (SC8) | CM-7(5), SR-3, SR-5, SR-11 | 3.4.8 → 03.04.08 | PW.4, PW.6, PS.3 | Build L2 for npm provenance | Vendor |
| A dependency gate in Aries, recorded in the event log (SC9) | CM-3 Configuration Change Control (B, C, D), CM-7(5), SA-15, AU-2 Event Logging (B, C, D), SR-3 | 3.4.3 (change control), 3.3.1 → 03.04.03, 03.03.01, 03.17.03 | PW.4, PO.5 (environments) | — | Vendor builds it; the operator sets the policy per project |

**FedRAMP 20x.** Its two supply chain indicators (Read, primary): KSI-SCR-MIT, "Persistently identify, review, and mitigate potential supply chain risks", lists among its related controls SA-10, SA-11, SA-15(3), SA-22, SI-7(1), SI-7(15), SR-3, SR-5 and SR-6; KSI-SCR-MON, that third-party software is automatically monitored for upstream vulnerabilities, lists RA-5, SI-5, SR-3, SR-5, SR-6 and SR-8. The weekly `vuln.yml`, Dependabot and CodeQL already answer MON for the vendor's own software; SC1 to SC4 are MIT's evidence. The 2026 Consolidated Rules were launched on 24 June 2026 and retire Rev5 authorizations by the end of 2028 (Read: fedramp.gov's changelog for the date; the retirement dates are secondary).

**What is not in a baseline and still worth having.** SR-4 (Provenance) and SR-4(3) carry no FedRAMP class, and CM-14 and SI-7(15) are Class D only (Read, primary). They are listed because they are what a customer's own SR-4 asks a vendor for, and because the SSDF's PS.2 and PS.3 ask for the same evidence under other names.

## 7. Design

### 7.1 Principles kept, and the rules this adds

- **Rule 4 stands.** No control here adds a runtime dependency to `aries` or `ariesd`. Where a tool is needed (capslock, a rebuild), it runs in CI or as a `go run` of a pinned module, and where a format must be written (the bill of materials), it is written by a tool in `internal/tools` on the standard library, as `releaseindex` is, so that it uses the same parsers and bounds as the clients and needs no secret.
- **Rule 3 stands.** Nothing here touches `keys.go`, the signing step or the key. The Ed25519 signature stays the one thing a release is trusted on; GitHub's attestation and the rebuild record are independent checks beside it, never replacements (aries.md §11, As built).
- **Policy is a test, or it is a wish.** The repository already holds docs to code with tests in `scripts/` and `internal/version`; the dependency policy, the capability baseline and the toolchain pin get the same treatment. A rule that lives in a pull request template is a reminder; one that fails `go test` is a control an assessor can watch run.
- **Evidence is a release asset.** What an assessor is shown (the bill of materials, the VEX statement, the rebuild record) is produced by the release workflow, named in `checksums.txt`, and so covered by the signature. A document written by hand after the fact is not evidence.
- **Say what is not done.** One maintainer cannot attest to two-party review; a public Sigstore log is not where an air-gapped site looks; a capability baseline is static analysis. Each slice's acceptance says what it does not prove.

### 7.2 The dependency admission policy

Written here, and moved into `AGENTS.md` as rule 4's second paragraph when SC1 lands, so that every agent reads it at session start.

**A Go module may be added only when all of these hold**, and the pull request says so under "No new dependencies" (which becomes "Dependencies"):

1. **The standard library would not do**, and the pull request says what was tried. A module that wraps a standard library package, formats output or parses a format the standard library parses is refused.
2. **It is named in the allowlist** (`deps.allow`, SC1) with its reason, which binary may link it (`aries`, `ariesd`, both, or tests and tools only), and the date. Adding the line is part of the pull request.
3. **It carries no capability the allowlist does not grant it** (SC2): a module for parsing may not reach the network, run a process or use `unsafe`; the golden capability file shows what it reaches, and the pull request explains each new capability.
4. **It has been published for at least 14 days at the version taken** (§7.3), or the pull request says why not (a security fix is the one reason).
5. **It is served by the Go module proxy and the checksum database**: `GOPROXY` and `GOSUMDB` are the defaults, `GONOSUMDB` and `GOPRIVATE` are empty, and `GOFLAGS` includes nothing that bypasses `go.sum`. A module from a private host is a question for the maintainer.
6. **Its license is one the AGPL combines with** (decision 82): Apache-2.0, BSD, MIT, MPL-2.0 or ISC. Anything else is a question for the maintainer.
7. **Its name is the one its documentation gives**, checked at pkg.go.dev by a person: not a near match, not a fork, not a name a model suggested without a source. An agent that proposes a module says where it learned the name.
8. **It is reviewed by a person** before it merges, whoever wrote the pull request. The `go-engineer` agent may propose a dependency and may not add one to the allowlist on its own; its specification says so (`.claude/agents`, `.codex/agents`, `.gemini/agents`, kept in step by `internal/agentspec`).

**An indirect module** needs no allowlist line of its own, since it arrives with its parent, but it is in the per-binary link set (SC1) and the capability baseline (SC2), so a direct module that gains an indirect one with a new capability fails the gate the same way.

**A tool** (something the gate runs that is not linked into a binary) is held to 1, 2, 5, 7 and 8, and is pinned by `go.sum`: a `tool` directive in `go.mod` or an entry in a tools module (§7.4).

**A GitHub Action** is held to 2, 7 and 8, pinned by commit (as today), and is listed with its reason in the same allowlist.

### 7.3 The update and cooldown policy

- **A version is taken no sooner than 14 days after publication**, for modules, tools and Actions alike, unless it fixes a vulnerability govulncheck or an advisory names, in which case it is taken as soon as the gate passes. The number is a judgment: the npm worms of 2025 were pulled in hours to days; `boltdb-go` sat for three years, which no cooldown catches. Dependabot's `cooldown` is set to it (`default-days: 14`), with `semver-major-days: 30`; pnpm's `minimumReleaseAge` and npm's `min-release-age` are set to the same when the web UI exists (SC8) (Read: GitHub's `cooldown` and pnpm 11's default are documented; npm's `min-release-age` is secondary, **Not confirmed** against npm's documentation).
- **Updates are grouped and reviewed**, as Dependabot does now; a grouped pull request's diff of `go.sum` is read, not skimmed, and the capability golden's diff is the review's first page.
- **A module is replaced before it is abandoned**: one with no release in two years and no response to an issue in six months is listed for replacement in an Aries issue; that is SA-22's "unsupported component" question asked early.
- **The toolchain moves on its own schedule**: Go's patch releases are taken within the week (they are often security releases), the minor release when `go.mod`'s `go` line moves, by a pull request that also moves `mise.toml` and the Dockerfile's image, which tests in `internal/version` hold together.

### 7.4 Pinning the toolchain and the tools

Three things the gate runs are not pinned by a hash in this repository today: the Go toolchain, `govulncheck` and `golangci-lint` (G2, G3).

**The toolchain.** `go.mod` gains a `toolchain go1.27.N` line and CI sets `GOTOOLCHAIN=local` with `setup-go` installing exactly that version, so that the toolchain a release was built with is stated at the tag and nothing downloads another one. The rebuild (SC4) uses the same line. Patch releases then move the line (§7.3). What `setup-go` checks of the download it makes is to be read before this is relied on (**Not confirmed**); if it checks nothing, the workflow verifies the archive's SHA-256 against go.dev's published sum, which the Go project's own reproducibility report makes an independent check possible of (Read: go.dev/rebuild).

**The tools.** Two ways pin a tool by `go.sum`: a `tool` directive in the main `go.mod` (Go 1.24 and later), or a separate module (`internal/tools/go.mod`) whose `go.sum` holds them. The directive is simpler and is what the working tree tries today; its cost is that golangci-lint brings 377 modules into `go list -m all`, into every `go mod verify`, and into any reader's first impression of the dependency list, while `govulncheck ./...` does not scan them (it scans what is built). The separate module keeps the product's `go.mod` at 50 lines and the tools' pins beside it, at the cost of one more module for `go mod verify` to cover and for the gate to `cd` into. **Recommendation:** the separate module for both tools, so that the dependency list an assessor reads is the product's, and so that SC1's allowlist can say "tools only" in one place. Either way `govulncheck` leaves `go run …@v1.8.0` for a pinned entry, and `TestGovulncheckPin` moves with it. The golangci-lint action keeps installing its binary by version string if the action stays; running the pinned module instead is slower (it compiles) and is the price of the pin; the maintainer's call ([§11](#11-open-questions-for-the-maintainer)).

### 7.5 Capabilities, not just versions

A version pin says a module did not change; it says nothing about what the module does. capslock (Google, BSD-3; Read) walks the call graph of a Go program and reports which packages reach which capabilities: network, file system, process execution, `unsafe`, reflection, arbitrary execution through `//go:linkname`, and so on. deps.dev publishes its results for public modules. Run over `./cmd/aries` and `./cmd/ariesd`, it gives a baseline per binary: which third-party package may open a socket (the MCP SDK's transport, the OTLP exporters, pgx, `x/crypto/ssh`), which may run a program (none should, in `aries`, but `internal/client`'s IAP transport runs `gcloud`, and `internal/cli` runs `git`), which uses `unsafe` (`segmentio/asm`, the protobuf runtime).

The control is a golden file per binary (`internal/depcheck/testdata/caps-aries.golden`, `caps-ariesd.golden`) that a CI job compares the fresh analysis to; a diff fails the job, and `-update` after review records the new baseline. Its limits, stated for the assessor: it is static analysis, so it sees what the call graph can reach, not what runs; it does not see through cgo (there is none, rule 4) or through code generated at run time (none); a capability can hide behind an interface the analysis resolves conservatively, which shows as a capability that is there but unused, never as one that is missing. It is the kind of check that would have flagged a loader in a patch release of a testing library: new `os/exec`, new network.

### 7.6 Provenance that a stranger can check

Three artifacts, each produced by the release workflow and named in `checksums.txt`:

- **A bill of materials** (`sbom.spdx.json` and `sbom.cdx.json`, one each per binary and platform is overkill; one per binary, since the module set does not vary by platform except `x/sys` and `x/term`'s per-OS files, which are the same module). It is written by `internal/tools/sbom` from the binaries' own build information (`debug/buildinfo`, which every Go binary carries: module path, version and `h1:` checksum of every module linked, the Go version, the build settings including `-trimpath`, `CGO_ENABLED` and the VCS revision), not from `go.mod`, so that it lists what is in the archive and nothing more. It writes SPDX 2.3 JSON and CycloneDX 1.6 JSON, both of which CISA's 2026 minimum elements accept as machine-processable; writing SPDX 3.0.1 or CycloneDX 1.7 waits until a consumer asks, since the tool is ours to extend. The CISA 2026 elements (Read: the publication and date are primary; the field list is secondary and **Not confirmed** against the PDF) add a component hash, a license, the tool's name and the generation context to the 2021 fields: the hash field carries the module's `h1:` sum with its algorithm named (it is a hash of the module's file tree, not of a file, and the SBOM says so), the license comes from the allowlist's line (§7.2), the tool is `aries-sbom` at the release's version, and the context is "build", from the binary. Why not syft or cyclonedx-gomod: either is a large tool with its own dependency tree run in the release job, for data the binary already carries; a tool of our own is a few hundred lines on the standard library, tested against the binaries the same workflow built, and consistent with `releaseindex`.
- **A VEX statement** (`vex.openvex.json`) from `govulncheck -format openvex ./...`, run in the release job: each finding at the package level with no finding at the symbol level becomes `not_affected` with `vulnerable_code_not_in_execute_path`, which is what a customer's scanner needs to stop reporting a vulnerability the binary cannot reach. Known limit (Read): govulncheck has marked vulnerabilities fixed by the version in use as `not_affected` too, which is true but for the wrong reason; the statement is reviewed before a release that has findings. OpenVEX over CSAF: the former is what govulncheck writes; CSAF waits for a consumer.
- **A rebuild record** (`rebuild.txt`, SC4): the checksums a second build produced, the toolchain it used, where it ran, and whether every line matched.

Where these are published is the same place as the archives: the GitHub release and, when it exists, the static tree (`releaseindex` copies whatever `checksums.txt` names). A mirror at an air-gapped site carries them in with the archives.

### 7.7 Sigstore, keys, and the air gap

GitHub's attestation already signs through Sigstore: for this public repository, with a certificate from Fulcio and an entry in Rekor's public log, which anyone can verify with `gh attestation verify` or `cosign verify-blob-attestation` against Sigstore's public trust root. That is the right thing for the open-source consumer. For a customer behind an air gap, or one whose policy forbids trusting a public transparency log, it is not reachable, and self-hosting Fulcio, Rekor, a timestamp authority and a certificate transparency log is a service to run and monitor (Read: Sigstore's guide to a private instance says a private log needs monitoring to be worth anything), which an operator installing one daemon and one database will not do.

The design keeps what RELEASING.md chose: **the Ed25519 signature on `checksums.txt` is what a release is trusted on**, verified offline by a key compiled into the binary and printed in `install.sh`, with no infrastructure, no network and no transparency log needed, which is what an air gap wants. GitHub's attestation is the second, independent check for those who can reach it. Keyless signing with cosign is not the trust anchor (it was dropped as that on 7 Oct 2026, aries.md §11 As built, for the same reason: the key's custody is the maintainer's, not an OIDC provider's). What this does not give is a public, append-only record that the key signed a given release; a release's `checksums.txt.sig` is in the release itself, and the GitHub attestation stands in for the log. If a customer requires a log, the answer is the attestation's Rekor entry for a public repository, or a private repository's GitHub-Sigstore bundle with no log (Read), and the rebuild record, which is evidence of another kind. Whether to add a second, key-based cosign signature in Sigstore's bundle format for consumers whose tooling speaks only that is a question ([§11](#11-open-questions-for-the-maintainer)); it would be the same key or a sibling, never Fulcio's.

**Decided 16 Oct 2026: Sigstore's format, signed with a key.** Releases are signed with a key the maintainer holds, not with a certificate from Fulcio. The release workflow also signs `checksums.txt` with cosign using a release key kept, like the Ed25519 key, as a secret of the gated `release` environment (ECDSA P-256, cosign's default and approved under FIPS 186-5, unless cosign accepts the Ed25519 key itself, which SC3 checks: **Not confirmed**). It publishes the result as a Sigstore bundle, `checksums.txt.sigstore.json`, a release asset and in the static tree, and the public key beside it (`cosign.pub`, its fingerprint also in docs/install.md and on the project's website). While the repository is public the signature is also entered in Rekor's public log, which gives the append-only record the key alone lacks; a verifier that wants no network checks it with `cosign verify-blob --key cosign.pub --bundle checksums.txt.sigstore.json`, and with the log's proof when it has Sigstore's trusted root. The Ed25519 signature stays what `aries upgrade` and `install.sh` check, with the key compiled in; the cosign signature is for an agency's tooling and policy engines that verify Sigstore. Keyless signing stays dropped (7 Oct, aries.md §11 As built): custody of the key is the maintainer's, not an OIDC provider's. The key is made, held and rotated as RELEASING.md says for the Ed25519 key, and the signing step is in `release.yml`'s signing job, so it falls under rule 3: it is added when the maintainer asks, as part of SC3.

### 7.8 The source side, honestly

SLSA's Source track (v1.2, Read) is where one maintainer meets a wall. Level 1 is version control, held. Level 2 is continuous history with no force-push, authenticated actors and provenance the hosting system records; `main` is protected and squash-merged through a merge queue, tags are restricted by a ruleset, and GitHub records the rest, so L2 is a matter of writing down the settings and keeping a test on them (`gh api` can read a ruleset; a test in `scripts/` can compare it to a checked-in copy when a token is present, and skip when not). Level 3 is those controls recorded in provenance, which GitHub does not yet issue for source (**Not confirmed** whether it does by now). Level 4 is two trusted persons approving every change to a protected branch. The `go-engineer` agent is not a trusted person, the CLA keeps outside contributions out until it is signed, and the maintainer is one. The design says so, and leaves the question to the maintainer: a second reviewer with write access is a governance decision, not a tool.

The rest of SC6 is paperwork that is also a control: `SECURITY.md` with a disclosure address and a response commitment (SR-8's "notification agreements" is what a customer writes against it), `CODEOWNERS` naming the maintainer for `go.mod`, `go.sum`, `deps.allow`, `internal/release`, `.github/workflows` and `install*.sh`, signed tags (already `git tag -s`; say so and verify it in `release.yml` with `git verify-tag` against a key in the repository), and an OpenSSF Scorecard workflow, whose checks (Pinned-Dependencies, Token-Permissions, Branch-Protection, Dangerous-Workflow, Signed-Releases) are the shortest independent statement of §4 an assessor can read.

### 7.9 FIPS 140-3

Go 1.24 and later can build with `GOFIPS140=v1.0.0`, which links the Go Cryptographic Module v1.0.0, CMVP certificate #5247, FIPS 140-3 Level 1, active, sunset 26 April 2031 (Read, primary: NIST's certificate page and go.dev). Module v1.26.0 was pending review in the Modules In Process list on 28 April 2026 (Read); `GOFIPS140=certified` selects the latest with a certificate. In FIPS mode `crypto/tls` ignores non-approved versions, suites and key exchanges, and key generation gets pairwise consistency tests that can slow some key types by up to 2x (Read).

What this means for Aries, to be measured in SC7 and not assumed: `crypto/ed25519` (the release signature, the SSH-key proof and host keys), `crypto/ecdh`, `crypto/tls` (the HTTPS listener and the WebSocket client) and `crypto/rand` are in the module; `golang.org/x/crypto/ssh` uses the standard library for Ed25519 and ECDH but carries its own ChaCha20-Poly1305 and Curve25519 code paths, which are not in the module, so an SSH connection in FIPS mode must be restricted to approved algorithms (AES-GCM, ECDH over P-256 or P-384, ECDSA or RSA host keys, with Ed25519 as FIPS 186-5 allows if the module version includes it) or SSH stays off, which transport.md already makes the default. Whether `x/crypto/ssh` with those restrictions is "using a validated module" is an assessor's judgment, not Go's (Read: go.dev says the validated module alone may not satisfy a requirement). The variant is a second set of archives (`aries-fips_…`, `ariesd-fips_…`) from the same workflow with `GOFIPS140=certified` and a test that the FIPS binary refuses a non-approved algorithm; it is not the default, because the default build must run on OpenBSD and 32-bit Windows, which FIPS mode does not support (Read), and because the module lags the standard library.

### 7.10 The TypeScript front end and the containers, before they exist

bearings.md §3.20 already says most of it (Observed): `web/` with an exact lockfile and a short list, Node pinned in `mise.toml` and `.nvmrc`, `npm ci --ignore-scripts` with an allowlist of install scripts if esbuild's or Tailwind's native packages need one, the assets built in the release workflow behind a `web` build tag so the Go gate needs no Node, `npm audit` on a schedule and not in the gate, the lockfile's hash recorded, the build run twice and compared, and a bill of materials of the front end in the release. This document adds what 2025 taught (Read): a cooldown (`minimumReleaseAge` in pnpm 11, where it is one day by default, or npm's `min-release-age`), set to §7.3's 14 days; `npm audit signatures` or the equivalent so that every package in the lockfile has registry provenance or is named as an exception; publishing nothing to npm (the UI is embedded, not published), so no npm token exists to steal; the same allowlist file format as Go's, with the package manager as a field; and the SBOM tool extended to read the lockfile, since the front end's components are not in a Go binary's build information. For containers, when a production image exists: built in the release workflow from the release's own archive, base image pinned by digest as the test fixture's is, the image's SBOM from the same tool plus the base image's package list, signed by the same key over the image's digest, with the attestation GitHub makes for a container image beside it.

### 7.11 Aries as the enforcement point

Last, because it is the only control here that changes the product, and because rule 12 and decision 68 shape it: **an agent may hold work back; a person or a check passes it.**

The 1.x shape is a `ci` gate with a named check, which exists (aries.md §20.3): a project's CI job runs the dependency checks of SC1 and SC2 against a pull request's branch and reports `aries gate pass --check deps` or `aries gate fail`, as it does for `build`. Nothing in Aries changes for that; it is a workflow in docs/ci.md. What SC9 adds, later, is a project setting that makes the gate exist without anyone adding it: `deps.gate: true` has `start` create a `ci` gate with check `deps` on every issue whose branch changes a manifest the project lists (`go.mod`, `package.json`, `pnpm-lock.yaml`, `Dockerfile`, `.github/workflows/*`), blocking the issue's close until CI reports it. The event log records `gate.pending`, `gate.passed` with the check's evidence (a run URL and a commit, 1 KiB at most) and `gate.failed`, which is the audit trail an assessor reads for CM-3 and AU-2: every dependency change an agent made, when, in which repository, and what checked it. A project's allowlist is the repository's `deps.allow`, read by the CI job, not by the server, which keeps rule 12 and keeps the server from reading repositories. Bearings's policy as code (bearings.md §3.18) is where the organization-wide rule lives: a portfolio's `deps.gate` that a project cannot loosen.

What SC9 does not do: it does not inspect manifests in the server (the server never sees a repository), does not let an agent pass the gate (it is a `ci` gate, passed by the CI principal, or waived by an admin with a reason), and does not replace a repository's own branch protection, which should require the check too.

## 8. Specification: the slices

Each slice leaves `main` working and changes nothing for a deployment unless it says so. Order is by risk reduced for the effort; estimates are one engineer's days, judged from comparable work in this repository, not from a breakdown. Every slice names the evidence it produces, since that is half of why it exists.

### Slice SC1: the dependency policy as tests

**Changes.**
- `deps.allow` at the repository root: one line per direct module, tool and Action: `path  scope  license  since  reason`, where scope is `aries`, `ariesd`, `both`, `tools` or `actions`. Comments with `#`.
- `internal/depcheck` (tests only, no product code): `TestDirectModulesAreAllowed` reads `go.mod`'s direct requirements and fails on one not in `deps.allow`, or one in `deps.allow` not in `go.mod`; `TestLinkSets` runs `go list -deps` for each binary and compares the module set to `testdata/links-aries.golden` and `links-ariesd.golden`, refusing a module whose scope does not permit the binary; `TestActionsAreAllowed` reads every `uses:` in `.github/workflows` and holds it to `deps.allow` and to a 40-character commit; `TestToolsArePinned` fails on a `go run …@version` in any workflow or in `scripts/gate.sh`.
- `internal/tools/go.mod` (or the `tool` directive, §7.4): govulncheck and golangci-lint pinned by `go.sum`; `scripts/gate.sh`, `ci.yml`, `release.yml` and `vuln.yml` run them from there; `TestGovulncheckPin` becomes a test that the three agree on the module version.
- `go.mod`: a `toolchain` line; `ci.yml`, `release.yml`, `scripts/gate.sh`: `GOTOOLCHAIN=local`, `GOFLAGS=-mod=readonly` (the default since Go 1.16, stated so a `GOFLAGS` from the environment cannot change it); `setup-go` with `go-version-file` keeps reading `go.mod`, now the `toolchain` line's version.
- The gate gains `go mod tidy -diff` (Go 1.23 and later) so a stale `go.mod` fails.
- `.github/dependabot.yml`: `cooldown` per §7.3.
- `AGENTS.md`: rule 4 gains §7.2 and §7.3; the pull request template's last line becomes a "Dependencies" heading; the three `go-engineer` specifications say the agent proposes and never adds a dependency.

**Tests that hold it.** The four tests above; `scripts/gate_test.go`'s existing tests, extended to the new commands; `internal/agentspec` for the specifications; `TestAgentsListsPolicy` in `scripts/` that `AGENTS.md` names `deps.allow`.

**Acceptance.** A pull request that adds a module to `go.mod` without a `deps.allow` line fails `go test ./internal/depcheck/`; one that makes `aries` link a module scoped `ariesd` fails `TestLinkSets`; one that writes `go run x@v1` in a workflow fails `TestToolsArePinned`; the gate at a tag names the toolchain it used in `go version -m` of the archives.

**Evidence.** `deps.allow` (the inventory with reasons, CM-8, SR-5), the goldens (which binary carries what), a CI run showing each test, and `AGENTS.md`'s text (the policy, SA-15).

**Estimate.** 3 days. Nothing in it runs at run time.

### Slice SC2: the capability baseline

**Changes.** `internal/depcheck/testdata/caps-aries.golden` and `caps-ariesd.golden`, written by capslock in its compact, package-granular output over `./cmd/aries` and `./cmd/ariesd`; a CI job `capabilities` in `ci.yml` (and the gate) that runs capslock from the tools module and diffs; `-update` through a `go test` flag in `internal/depcheck` that reruns capslock and rewrites the goldens, for review. Each capability a third-party package holds gets a line in `deps.allow`'s reason or a comment in the golden saying why it is expected (the MCP SDK's HTTP transport, pgx's sockets, `segmentio/asm`'s `unsafe`).

**Tests that hold it.** `TestCapabilitiesMatchBaseline`; a test that every third-party package in the golden with `CAPABILITY_EXEC`, `CAPABILITY_NETWORK` or `CAPABILITY_ARBITRARY_EXECUTION` is named in `deps.allow` as allowed to hold it.

**Acceptance.** Bumping a dependency to a version that adds `os/exec` fails the job with the package and the capability named; the goldens are read in review; the job takes under two minutes (to be measured; capslock on `ariesd`'s 42 modules is the case to time).

**Evidence.** The goldens and the diffs in each dependency pull request (SA-11, SR-6, SR-10: "what we inspected and what we found").

**Not proven.** Dynamic behavior; see §7.5.

**Estimate.** 2 days, after SC1's tools module exists.

### Slice SC3: the bill of materials and the VEX statement

**Changes.** `internal/tools/sbom`: reads one or more binaries with `debug/buildinfo`, joins `deps.allow` for licenses, writes SPDX 2.3 JSON and CycloneDX 1.6 JSON with the fields of §7.6, deterministic (same input, same bytes), standard library only. `release.yml`'s `build` job runs it over the eight binaries, writes `sbom-aries.spdx.json`, `sbom-aries.cdx.json`, `sbom-ariesd.spdx.json`, `sbom-ariesd.cdx.json`, runs `govulncheck -format openvex ./...` to `vex.openvex.json`, and adds all five to `checksums.txt` before the attestation step, so that the signature and the attestation cover them. With the maintainer's go-ahead under rule 3, the signing job also writes the key-signed Sigstore bundle `checksums.txt.sigstore.json` (§7.7) and uploads it beside `checksums.txt.sig`. `releaseindex` already copies what `checksums.txt` names. docs/install.md's "Verify a download by hand" gains the SBOM and what it lists; docs/security-model.md's Releases section gains a paragraph.

**Tests that hold it.** Golden tests of the tool over a binary built in the test (`go build` of a tiny module with two dependencies in a `t.TempDir`); a test that the two formats name the same modules and sums; a test in `internal/release` that `checksums.txt`'s expected file names include the five (the archive-name test exists; extend it); a schema check of the JSON against the SPDX 2.3 and CycloneDX 1.6 schemas, vendored as test data (two JSON files; they are data, not dependencies).

**Acceptance.** A release's SBOM lists exactly the modules `go version -m` lists for its binary, with `h1:` sums; `sha256sum -c checksums.txt` covers it; the VEX statement names every govulncheck finding with a status; a customer's scanner given the SBOM and the VEX reports what govulncheck reports and no more.

**Evidence.** The five files on every release (PS.3.2, SR-4, CM-8, RV.1).

**Not proven.** That the SBOM is complete for the build environment (the toolchain is named, the runner's OS is not), and license accuracy beyond the allowlist's word.

**Estimate.** 4 days.

### Slice SC4: the independent rebuild

**Changes.** A job `rebuild` in `release.yml`, after `build`, on a different runner image (`ubuntu-24.04-arm` if available, else a second `ubuntu-24.04` run with no shared cache), which checks out the tag, installs the toolchain the `toolchain` line names, verifies the toolchain archive's SHA-256 against go.dev's published sum, runs `go mod download && go mod verify`, builds the eight binaries with the same flags and `SOURCE_DATE_EPOCH`, packs them with the same `tar` and `zip` invocations (which is why the archive steps move into one script, `scripts/release-build.sh`, both jobs run), and compares its `checksums.txt` to `build`'s line by line; writes `rebuild.txt` (the result, the toolchain, the runner, the commit) and uploads it; `sign-and-publish` refuses to sign if `rebuild.txt` is missing or reports a mismatch, and adds it to the release. RELEASING.md gains the section and the command for a rebuild on the maintainer's machine (`scripts/release-build.sh` with the tag), whose result is pasted into the release notes once per release.

**Tests that hold it.** `internal/release/workflow_test.go` pins the new refusal in `sign-and-publish`; a test that `scripts/release-build.sh` is what both jobs run (as `TestGateMatchesCI` does for the gate); a test that builds `./cmd/aries` twice in a temporary directory with the script's flags and compares, which fails if anything non-deterministic creeps in (a timestamp, a map order in embedded data).

**Acceptance.** Two builds on two runners produce byte-identical archives for every platform; a deliberate one-byte change in a build input (a test, locally) produces a mismatch the signing job refuses; the maintainer's rebuild on macOS (cross-compiling, `CGO_ENABLED=0`) matches too.

**Evidence.** `rebuild.txt` on every release; the release notes' line (SI-7, SI-7(1), PS.2; what a SLSA Build L3 claim is checked against from outside).

**Not proven.** That the runner the second build ran on is independent of GitHub; it is not. A rebuild on the maintainer's machine, or by a customer following RELEASING.md, is.

**Estimate.** 3 days, plus a release to try it on.

### Slice SC5: the installer at a tag, immutable releases

**Changes.** `install.sh` and `install-server.sh` join the release: the `build` job copies them into `dist/` and `checksums.txt` names them, so that the signed file covers the installers of that release; README.md and docs/install.md change the one-line install to fetch `install.sh` from the latest release's assets (`https://github.com/ariesworx/aries/releases/latest/download/install.sh`), and the server guide the same for `install-server.sh`; `install-server.sh`'s `raw_url` takes a tag (`--version`, which it has) and fetches `install.sh` and the AppArmor profiles at `refs/tags/TAG`, never `main`, and verifies `install.sh` against the release's `checksums.txt` before running it. The repository enables immutable releases (a setting; RELEASING.md's one-time setup gains the `gh api` call), and `release.yml` creates the release as a draft, uploads the assets, then publishes, as GitHub's documentation says the release attestation needs (Read, with a user-reported caveat); docs/install.md's "Verify a download by hand" adds `gh attestation verify --bundle-from-oci`'s equivalent for the release attestation once its command is confirmed (**Not confirmed**).

**Tests that hold it.** `internal/release/install_test.go` and `install_static_test.go` run the installer against each source already; extend them to the installer served as a release asset; `scripts/installserver_test.go` fails if `raw_url` names a branch; a test that README.md's and docs/install.md's install commands name no branch.

**Acceptance.** A new machine installs with no fetch from `main`; a changed `install.sh` on `main` changes nothing for anyone until a release ships it, signed; an asset on a published release cannot be replaced (checked by hand once, recorded in RELEASING.md).

**Evidence.** The installers in `checksums.txt`; the repository's immutable-releases setting (SR-11, CM-14, SI-7(15)).

**Estimate.** 2 days.

### Slice SC6: the source side

**Changes.** `SECURITY.md` (a disclosure address at example.com's equivalent for the project, a response time, the supported versions, which is the latest release); `.github/CODEOWNERS`; a Scorecard workflow (`ossf/scorecard-action`, pinned, publishing to code scanning, not to the public API by default); `release.yml` verifies the tag's signature against `.github/release-signers.asc` or the maintainer's key as GitHub holds it (`git verify-tag`, with the key's fingerprint pinned in the workflow as the PGDG key's is); `docs/security-model.md` gains a section, "The repository", that states the branch rules, the tag ruleset, the merge queue, who can approve, and that two-party review is not in place, with the date; a test in `scripts/` that compares the ruleset `gh api` returns to `testdata/rulesets.json` when `GITHUB_TOKEN` is present and skips otherwise. The `cla.yml` workflow is already pinned.

**Tests that hold it.** The ruleset test; a test that `SECURITY.md` names the address `docs/security-model.md` names; `scripts/gate_test.go` for the new workflow's pins (SC1's `TestActionsAreAllowed` covers it).

**Acceptance.** Scorecard reports Pinned-Dependencies, Token-Permissions and Dangerous-Workflow at full marks and Branch-Protection as configured; a tag that is not signed by the pinned key is not built.

**Evidence.** `SECURITY.md` (SR-8), the security model's new section (SR-2's input for a customer's plan; SLSA Source L2 stated), Scorecard's results, the signed-tag check in the workflow (PS.1).

**Not proven.** Two-party review (Source L4); provenance for source (L3) until GitHub issues it.

**Estimate.** 2 days.

### Slice SC7: the FIPS variant, measured

**Changes.** A spike first (`spike/fips`, kept as evidence like the others): build both binaries with `GOFIPS140=certified`, run the e2e suite over HTTPS and SSH, list which algorithms `x/crypto/ssh` negotiates and which the FIPS binary refuses, measure the start-up cost of the self-tests and the key generation cost, and record which standard library and `x/crypto` paths the certificate's boundary covers (from the certificate's security policy, read, not assumed). Then, if it holds: a `fips` build in `release.yml` of `aries` for Linux and Windows and `ariesd` for Linux (amd64 and arm64 each; §11, question 3), named `aries-fips_…` and `ariesd-fips_…`, listed in `checksums.txt` and the SBOM (which names the module version linked), a `--fips` flag on `install.sh` and the server installer, and a line in `ariesd status` and `aries version` saying FIPS mode is on and which module version.

**Tests that hold it.** A build-tagged test that, in a FIPS binary, `crypto/tls` refuses a non-approved suite and `ssh` refuses ChaCha20; `internal/version` keeps the module version named in the release notes.

**Acceptance.** The e2e suite passes in FIPS mode over HTTPS; SSH passes with the restricted algorithm list or is documented as off in FIPS mode; the spike's RESULTS.md answers §11's question with numbers.

**Evidence.** The certificate number and module version in the release notes and the SBOM (SC-13, IA-7, 3.13.11).

**Estimate.** 3 days for the spike; 2 for the variant.

### Slice SC8: npm and container rules, written before `web/` exists

**Changes.** `deps.allow` gains the package manager field and the format for npm; `AGENTS.md`'s rule 4 text covers npm and images; a `docs/design/bearings.md` §3.20 cross-reference to §7.10; a test in `scripts/` that fails if a `package.json` or a `Dockerfile` outside `deploy/docker` appears without a corresponding lockfile, `.npmrc` or `pnpm-workspace.yaml` with the cooldown and `ignore-scripts`, and an allowlist line. The SBOM tool gains a lockfile reader when `web/` lands, not before.

**Acceptance.** The test passes today (nothing to check) and fails on a `web/package.json` with no lockfile.

**Estimate.** 1 day.

### Slice SC9: the dependency gate in Aries (1.x)

**Changes.** docs/ci.md gains a `deps` check beside `build` and `landed`, with the GitHub Actions and GitLab CI jobs that run SC1 and SC2's checks on a branch and report by `aries gate pass --check deps` (no server change; this part can ship with SC2). Later: the `deps` project setting (`deps.gate`, `deps.manifests`), `start` creating the `ci` gate when the branch's diff touches a listed manifest (the client reads the diff, as `--from-branch` reads the branch; the server only receives the gate), a protocol change under rule 10 (a bump, `ProtoMin` one back), the Limits row for `deps.manifests` (rule 18), the events as §7.11 lists them, and the guide. Bearings's policy as code carries the portfolio-level setting when it exists.

**Tests that hold it.** The store's gate tests extended for the automatic gate; an e2e test in which an agent's `start` on a branch that changes `go.mod` leaves the issue blocked until a CI principal reports `deps`; `TestSpansCarryNoText` for the new op's arguments; the Limits table test.

**Acceptance.** An agent cannot close an issue whose branch changed a manifest until CI reports the check; the event log shows the gate's life; an admin's waiver carries a reason.

**Evidence.** The event log's `gate.*` events for every dependency change, exportable with `aries digest` or the audit stream (CM-3, AU-2).

**Estimate.** 1 day for the docs/ci.md part; 5 days for the setting and the automatic gate, after the Postgres port's P1 and P2 and stage 6's G2.

### Order and total

SC1 (3 days), SC5 (2), SC2 (2), SC3 (4), SC4 (3), SC6 (2), SC8 (1), SC7 (3 + 2), SC9 (1 + 5): about 28 days, with SC1, SC5 and SC6 (one week) removing the gaps an attacker would try first, and SC3 and SC4 producing what an assessor asks for first. SC1 before SC2 (the tools module); SC3 before SC4 only for the shared build script; everything else independent.

## 9. Settings, refusals and the protocol

Only SC9 touches the product. Its setting:

| Setting | Default | Is |
|---|---|---|
| `deps.gate` | `false` | Whether `start` creates a `ci` gate with check `deps` on an issue whose branch changes a manifest |
| `deps.manifests` | `go.mod, go.sum, package.json, pnpm-lock.yaml, package-lock.json, Dockerfile, .github/workflows/*` | The paths that count; at most 32 (`limits.deps_manifests_per_project`, rule 18) |

A `start` that would create the gate and cannot (the CI principal is not a member, say) is refused `invalid` with a fix naming `aries admin member add` for a person to run; the agent is told the issue waits for a person, never to run an admin command (rule 12). The op is `issue.start`'s existing `gate` field made automatic, so the protocol gains no op, only a result field (`gate`) that older clients ignore; if a new argument is needed it is a bump (rule 10).

## 10. Decisions for the maintainer

Proposed, numbered after decision 89 in aries.md §14; each is recorded there only when made.

90. **Dependencies are policy by test.** A direct module, a tool the gate runs and a GitHub Action is admitted only by a line in `deps.allow` with its reason and scope, held to `go.mod`, the workflows and each binary's link set by tests (SC1); the admission and cooldown policies of §7.2 and §7.3 join `AGENTS.md`'s rule 4. An agent proposes a dependency and never adds one. Cost: a line and a review per dependency; the tools move from `go run …@version` and an action's version string to a pinned module.
91. **The toolchain is named at the tag.** `go.mod` carries a `toolchain` line, CI runs with `GOTOOLCHAIN=local`, and a release names in `go version -m` the toolchain the rebuild must use. Cost: patch releases of Go are a pull request, which they should be.
92. **A dependency's capabilities are part of its pin.** capslock's baseline per binary is a golden file the gate holds (SC2); a new capability is a reviewed change. Cost: a CI job, and a golden to update when the MCP SDK or pgx grows.
93. **A release carries its own bill of materials, VEX statement and rebuild record,** written by tools in this repository on the standard library from the binaries' build information and govulncheck, named in `checksums.txt` and so signed (SC3, SC4). The Ed25519 signature stays what a release is trusted on; GitHub's attestation and the rebuild are independent checks beside it; a cosign signature over `checksums.txt`, made with a release key and published as a Sigstore bundle, is added beside it for verifiers that speak Sigstore (§7.7); keyless signing and a self-hosted Sigstore are not adopted. Cost: about a week of tooling; a release takes minutes longer.
94. **Nothing a new install runs is fetched from a branch.** The installers are release assets covered by `checksums.txt`, the server installer fetches at a tag, and GitHub's releases are immutable (SC5). Cost: the one-line install's URL changes once; a fix to `install.sh` reaches new installs with the next release, not the next push.
95. **The source-side controls are written down, with their limit.** `SECURITY.md`, `CODEOWNERS`, Scorecard, a signed-tag check, and the security model's statement that two-party review is not in place (SC6). Whether to add a second reviewer is a governance decision this document only asks.
96. **A FIPS 140-3 variant is measured before it is promised** (SC7): a spike against the certified module, then a second set of archives if it holds, `aries` for Linux and Windows and `ariesd` for Linux, never the default build.
97. **Aries holds agent work behind a dependency check, in 1.x,** as a `ci` gate created by `start` when a branch changes a manifest, with the policy in the repository and the check in CI, recorded in the event log (SC9). Rule 12 stands: an agent never passes it.

## 11. Open questions for the maintainer

1. **Sigstore.** *Answered 16 Oct 2026:* use Sigstore, signed with a key the maintainer holds, not keyless. The release also carries a cosign signature over `checksums.txt` in Sigstore's bundle format (§7.7, decision 93), beside the Ed25519 signature, which stays what `aries upgrade` and `install.sh` check. Whether the signature still goes to Rekor's public log if the repository is made private (RELEASING.md, Before the repository is made private) is decided then; the key signature verifies without it.
2. **Where the tools are pinned.** *Answered 16 Oct 2026:* a separate module, `internal/tools/go.mod`, as §7.4 recommends; the product's `go.mod` carries no `tool` directive. Still open: whether the golangci-lint action stays, with its binary fetched by version, or the gate runs the pinned module and pays the compile.
3. **FIPS.** *Answered 16 Oct 2026:* a FIPS 140-3 variant is offered for Linux and Windows: `aries` for both (amd64 and arm64), and `ariesd` for Linux, the only system `ariesd serve` runs on (rule 14). SC7 says so. Whether certificate #5247's tested operating environments include Windows on each architecture is read from the certificate during the spike (**Not confirmed**).
4. **Where evidence is published.** *Answered 16 Oct 2026:* the files are release assets, named in `checksums.txt`, on the GitHub release and in the static tree, so an air-gapped mirror (`ARIES_RELEASES`) carries them as a matter of course. A security page on the project's website, outside this repository, is where a reviewer starts: how to verify a release, the key's fingerprint, the disclosure address, a summary of §6, and links to the latest release's files. It links to the files and never hosts copies, so there is no second, unsigned source.
5. **The cooldown.** *Answered 16 Oct 2026:* 14 days, as §7.3 has it; a security fix skips the wait, not the review.
6. **Two-party review.** *Answered 16 Oct 2026:* a single reviewer for now; the project has one developer. SC6 writes that statement into the security model, with its date, and SLSA Source Level 4 is not claimed.
7. **Which program, if any.** *Answered 16 Oct 2026:* FedRAMP is the target; CMMC assesses how an organization handles controlled unclassified information, not a product, so for a defense contractor Aries is a tool whose evidence supports their SP 800-171 assessment, and the mapping in §6 stays for that. One question follows from it: FedRAMP authorizes a cloud service offering, so it applies to Aries as a hosted service the project runs. Software an agency or its contractor installs on its own servers falls inside their system's boundary and its authorization, where the vendor's part is the evidence of §6 and §8, not an authorization of its own. *Answered 16 Oct 2026:* installed software comes first. Agencies run Aries in their own environment, under their own authorization, and the project supplies the evidence package: the signed release, its SBOM, VEX statement and rebuild record (SC3, SC4), the FIPS variant (SC7), the security model and this document's mapping (§6), marked vendor and operator. A hosted service, and a FedRAMP authorization and System Security Plan of the project's own, come later.
8. **The capability job's cost.** capslock's time over `ariesd` has not been measured; if it is minutes, it runs on `main` and in the release gate, not on every pull request.
9. **SC9's scope.** Build the docs/ci.md `deps` check now (no server change) and leave the automatic gate to 1.x after the port's P1 and P2 and G2, as proposed, or fold the setting into stage 6's gate work?

## 12. Not established

- Whether `setup-go` verifies the toolchain archive it downloads, and against what; SC1 and SC4 depend on the answer.
- CVE-2026-42501's affected and fixed Go versions; the advisories read were secondary. The gate is at the newest patch of Go 1.27 either way (AGENTS.md, Tools).
- The exact minimum data fields of CISA's 2026 SBOM elements (17, with hash algorithm, license, tool name and generation context, per secondary reports); the PDF is to be read when SC3 is built.
- Whether npm's `min-release-age` exists under that name and in which version; pnpm's `minimumReleaseAge` is documented.
- Whether GitHub issues SLSA source provenance or a source VSA for repositories as of this date.
- Which of `x/crypto/ssh`'s paths fall inside the Go Cryptographic Module's boundary; SC7's spike reads the certificate's security policy.
- Dependabot's default cooldown without a `cooldown` key.
- The outcome of the CMMC Reform Task Force's 60-day review (due about 13 Sept 2026) and whether Phase 2 has been reinstated; what is cited here is the July 2026 suspension, from law firms' alerts.
- Whether SP 800-218 Rev. 1 (SSDF v1.2) has been finalized since its December 2025 draft; NIST's project page listed it as a draft on 16 Oct 2026.
- FedRAMP's own mapping of Classes B, C and D to the former Low, Moderate and High; the mapping used in §6 is a consultant's.
- Nothing in this document has been run: no capslock baseline, no SBOM, no rebuild, no FIPS build. The slices are specified so that each produces its evidence the first time it runs.

## 13. References

Fetched 16 Oct 2026. Primary sources first within each group.

**Frameworks and rules**
- NIST SP 800-218 Rev. 1 (ipd), SSDF v1.2, 17 Dec 2025, comments closed 30 Jan 2026: <https://csrc.nist.gov/pubs/sp/800/218/r1/ipd>; SSDF publications: <https://csrc.nist.gov/Projects/ssdf/publications>
- Executive Order 14306, 6 June 2025 (CSRC topic page; the Federal Register text could not be fetched): <https://csrc.nist.gov/Topics/Laws-and-Regulations/executive-documents/Executive-Order-14306>; the order's text as published by the White House: <https://www.whitehouse.gov/presidential-actions/2025/06/sustaining-select-efforts-to-strengthen-the-nations-cybersecurity/>
- OMB M-26-05 rescinding M-22-18 and M-23-16, 23 Jan 2026 (secondary): <https://www.insidegovernmentcontracts.com/2026/02/omb-rescinds-the-common-form-secure-software-attestation-requirement/>; <https://www.crowell.com/en/insights/client-alerts/software-de-simplified-trump-administration-rescinds-standardized-secure-software-development-attestation-requirements>
- CISA Secure Software Development Attestation Form (2024): <https://www.cisa.gov/resources-tools/resources/secure-software-development-attestation-form>
- CISA 2026 Minimum Elements for a Software Bill of Materials, 29 July 2026: <https://www.cisa.gov/resources-tools/resources/2026-minimum-elements-software-bill-materials-sbom>; the 2025 draft: <https://www.cisa.gov/resources-tools/resources/2025-minimum-elements-software-bill-materials-sbom>; field changes (secondary): <https://fossa.com/blog/cisa-releases-2026-minimum-sbom-elements/>, <https://www.medcrypt.com/blog/cisa-sbom-minimum-elements-2026-update>
- NIST SP 800-161r1 (with its 2022 update), C-SCRM: <https://nvlpubs.nist.gov/nistpubs/SpecialPublications/NIST.SP.800-161r1-upd1.pdf>
- FedRAMP 2026 reference, SR family with classes: <https://fedramp.gov/2026/reference/controls/supply-chain-risk-management/>; SA family: <https://fedramp.gov/2026/reference/controls/system-and-services-acquisition/>; CM family: <https://fedramp.gov/2026/reference/controls/configuration-management/>; SI family: <https://fedramp.gov/2026/reference/controls/system-and-information-integrity/>; KSI Supply Chain Risk: <https://fedramp.gov/2026/providers/20x/key-security-indicators/supply-chain-risk/>; RFC 0030: <https://www.fedramp.gov/rfcs/0030>
- FedRAMP Consolidated Rules for 2026 and the classes (secondary): <https://labs.cloudsecurityalliance.org/research/csa-research-note-fedramp-20x-consolidated-rules-transition/>; <https://www.crowell.com/en/insights/client-alerts/time-for-a-change-fedramp-fundamentally-revamps-program-with-consolidated-rules-for-2026>; <https://elevateconsult.com/insights/fedramp-levels-explained/>
- CMMC: the 48 CFR rule effective 10 Nov 2025 (secondary): <https://kpmg.com/us/en/articles/2025/cybersecurity-dod-final-rule-on-cmmc-contract-requirements-reg-alert.html>; the Phase II suspension of 13 July 2026 (secondary): <https://www.jenner.com/en/news-insights/client-alerts/department-of-war-suspends-cmmc-phase-iibut-compliance-obligations-remain-as-does-enforcement-risk>; Revision 2 kept by class deviation (secondary): <https://lockheedmartin.com/en-us/suppliers/news/features/2024/cybersecurity-dod-deviation.html>
- NIST SP 800-171 Rev. 3's new families (secondary): <https://cloudsecurityalliance.org/blog/2024/01/09/nist-sp-800-171-r3-an-overview-of-the-changes>; <https://hklaw.com/en/insights/publications/2024/05/foundational-cybersecurity-standards-for-contractors-updated>

**Provenance, signing and formats**
- SLSA v1.2: <https://slsa.dev/spec/v1.2/onepage>; build requirements: <https://slsa.dev/spec/v1.2/build-requirements>; source requirements: <https://slsa.dev/spec/v1.2/source-requirements>; what's new: <https://slsa.dev/spec/v1.2/whats-new>; announcement: <https://slsa.dev/blog/2025/11/announce-slsa-v1.2>
- GitHub artifact attestations: <https://docs.github.com/en/actions/concepts/security/artifact-attestations>; SLSA Build L3 with reusable workflows: <https://docs.github.com/en/actions/how-tos/security-for-github-actions/using-artifact-attestations/using-artifact-attestations-and-reusable-workflows-to-achieve-slsa-v1-build-level-3>
- GitHub immutable releases, GA 28 Oct 2025: <https://github.blog/changelog/2025-10-28-immutable-releases-are-now-generally-available/>; <https://docs.github.com/en/code-security/concepts/supply-chain-security/immutable-releases>
- Sigstore Rekor v2 GA: <https://blog.sigstore.dev/rekor-v2-ga/>; running a private instance: <https://blog.sigstore.dev/a-guide-to-running-sigstore-locally-f312dfac0682>
- Go reproducible toolchains: <https://go.dev/blog/rebuild>; <https://go.dev/rebuild>; the original design: <https://research.swtch.com/vgo-repro>
- Go FIPS 140-3: <https://go.dev/doc/security/fips140>; CMVP certificate #5247: <https://csrc.nist.gov/projects/cryptographic-module-validation-program/certificate/5247>
- govulncheck's OpenVEX output and its limits: <https://pkg.go.dev/golang.org/x/vuln/internal/openvex>; <https://golang.org/issue/68338>; <https://golang.org/issue/62486>
- capslock: <https://github.com/google/capslock>; results on deps.dev, for example <https://deps.dev/go/google.golang.org%2Fgrpc/v1.48.0-dev.0.20220701230310-8c494a9aed3e/analysis>
- OpenSSF Scorecard: <https://github.com/ossf/scorecard>
- CycloneDX 1.7: <https://cyclonedx.org/news/cyclonedx-v1.7-released>; SPDX: <https://spdx.dev>
- Go modules reference, the checksum database and `GOSUMDB`, `GONOSUMDB`, `GOPRIVATE`: <https://go.dev/ref/mod#checksum-database>; toolchains: <https://go.dev/doc/toolchain>
- Dependabot `cooldown`: <https://github.blog/changelog/2025-07-01-dependabot-supports-configuration-of-a-minimum-package-age/>; <https://docs.github.com/en/code-security/reference/supply-chain-security/dependabot-options-reference>
- npm, pnpm and Yarn release-age settings (secondary): <https://gist.github.com/mcollina/b294a6c39ee700d24073c0e5a4e93104>; <https://lilting.ch/en/articles/pnpm-11-minimum-release-age-default-shai-hulud-defense>; npm trusted publishing (secondary): <https://www.speakeasy.com/blog/npm-trusted-publishing-security>

**Incidents**
- Backdoored `boltdb-go/bolt` on the Go module mirror, Feb 2025: <https://www.infoworld.com/article/3817921/malicious-package-found-in-the-go-ecosystem.html>; <https://heise.de/-10270367>
- Typosquatted `hypert` and `layout` Go modules, March 2025: <https://thehackernews.com/2025/03/seven-malicious-go-packages-found.html>
- `tj-actions/changed-files`, CVE-2025-30066, March 2025: <https://thehackernews.com/2025/03/github-action-compromise-puts-cicd.html>; <https://legitsecurity.com/blog/github-actions-tj-actions-changed-files-attack>
- Shai-Hulud npm worm, Sept and Nov 2025: <https://www.elastic.co/blog/shai-hulud-worm-2-0-updated-response>; <https://www.aikido.dev/blog/bugs-in-shai-hulud-debugging-the-desert>
- xz-utils, CVE-2024-3094: <https://tag-security.cncf.io/community/catalog/compromises/2024/xz/>
- Trivy's repository and tags, Feb and March 2026: <https://thehackernews.com/2026/03/trivy-security-scanner-github-actions.html>; <https://safedep.io/trivy-teampcp-supply-chain-compromise/>
- Prompt injection reaching publishing credentials (Cline, Feb 2026) and agent workflows (April 2026): <https://labs.cloudsecurityalliance.org/research/csa-research-note-clinejection-prompt-injection-cicd-cache-p/>; <https://pillar.security/blog/my-agentic-trust-issues-from-prompt-injection-to-supply-chain-compromise-on-gemini-cli>
- Package hallucination ("We Have a Package for You!", USENIX Security 2025), summarized: <https://labs.cloudsecurityalliance.org/research/csa-research-note-slopsquatting-ai-supply-chain-20260419/>; <https://www.endorlabs.com/learn/hallucinated-packages-how-ai-invents-dependencies-attackers-exploit>
- `mcp-remote`, CVE-2025-6514: <https://osv.dev/vulnerability/CVE-2025-6514>
- The `go` command's checksum validation, CVE-2026-42501 (secondary): <https://ubuntu.com/security/CVE-2026-42501>
