# Review: what it takes to use Aries for client work

Review, 23 October 2026. The maintainer asked: "I need to get aries to
a good production ready state so I can start using it with my client
work." This review says what that use requires, what is built today
against it, what is only designed and what is missing, whether it
needs v1.0.0, and the slices that would get there, ranked, each with
its cost and the Aries issue it fits where there is one. It is
written against `main` at 94fceb02; a line number is that commit's. It
changes no code and files no issue: the new work it proposes is for
the maintainer to file.

It follows [domain-review.md](domain-review.md) and
[cli-review.md](cli-review.md) in form: what was read (§1), the
requirements (§2), findings with their citations (§3), what holds
v1.0.0 (§4), ranked slices (§5), and questions for the maintainer, each
with this review's recommendation (§6).

No client is named here, nor anywhere in this repository (AGENTS.md
rule 1). "A client" below is any organization the maintainer does work
for, and "the client server" the server that would hold their
projects.

**The short answer.** Client work does not need v1.0.0. It needs a
server of its own on a real machine, backups that leave the machine
and have been restored, a key for each client that reaches that
client's project and no other, and a way to remove a client's data at
the end of an engagement. Most of that is built: many projects on one
server, membership at the handshake, per-project cost, a documented
daily dump and restore, signed upgrades that put the old binary back
when the new one fails. What is missing is small: a project delete and
a full project export, an encrypted off-machine copy and a restore
drill, a written support line, and one rule on the organization memory
still to be designed so that it can never cross from one client to
another. The rest of v1.0.0's list (pseudonyms, agent principals,
sign-in at an identity provider and its review) matters when people
other than the maintainer join, and can follow. §5 ranks ten slices;
the first four make a server fit for the maintainer's own client work,
and cost about a week, most of it the maintainer's time on a real
machine.

## 1. What was read

- [README.md](../../README.md) (status and roadmap) and
  [CHANGELOG.md](../../CHANGELOG.md); the release notes of
  [v0.5.0](../releases/v0.5.0.md), [v0.6.0](../releases/v0.6.0.md) and
  [v0.7.0](../releases/v0.7.0.md), their "Not in this release" and
  "Not verified" sections in full.
- [enterprise-review.md](enterprise-review.md),
  [surface-review.md](surface-review.md),
  [security-review-v1.md](security-review-v1.md),
  [identity.md](identity.md) and [transport.md](transport.md)'s status
  notes; [aries.md](aries.md) §14 decisions 78 to 101, §15 and §17.7.
- [docs/server.md](../server.md): serving many projects, access,
  sign-in, TLS, upgrades, backups and restore, Postgres, storage;
  [docs/security-model.md](../security-model.md); the deployment
  examples in [docs/deploy](../deploy/README.md).
- The code where a requirement turns on it: the handshake's admission
  (`internal/server/server.go`), the control store's access
  (`internal/store/control.go`), memories (`internal/store/memory.go`),
  cost (`internal/server/cost.go`), the project commands
  (`internal/cli/admin.go`), the export (`internal/bdimport/export.go`)
  and telemetry's people (`internal/telemetry/people.go`).
- The open issues on the dogfood server, by `list`, `ready` and
  `blocked`, and the held epic `aries-foz66bi6` (Release v1.0.0) with
  what hangs from it.

Not done: no server was set up, no restore was run, and nothing was
measured. Where this review says something "has not run", it is
quoting the guides' and the release notes' own records.

## 2. What "client work" requires

The maintainer's words name the goal and not the shape of it. This
review assumes the most likely shape and asks about it first (§6,
questions 1 to 3):

- **The maintainer and their agents are the users**, at first. Clients'
  own staff may join a project later.
- **Several clients' repositories**, each tracked in an Aries project,
  over months, with memories that carry what agents learned about each
  client's code.
- **On a server the maintainer runs**, separate from the dogfood server
  that tracks Aries itself.

From that, nine requirements. Each is a finding's heading in §3.

| # | Requirement | Why |
|---|---|---|
| C1 | One client's issues, memories, cost and people are never seen from another client's project | Confidentiality is what a client contract asks first |
| C2 | A client's data can be exported whole and deleted at the end of an engagement | Contracts commonly require it |
| C3 | The server runs on a real machine, reachable from the maintainer's machines and CI, not from the internet at large | Today's runs are in containers and on one VM |
| C4 | Backups leave the machine, are encrypted, and have been restored | Losing a client's backlog loses billable history |
| C5 | Upgrades keep the data, and the maintainer knows what each one promises | Months of use cross several releases |
| C6 | Others can sign in when they join: a subcontractor, a client's engineer | Later, not first |
| C7 | Cost can be reported per client | For billing, or for pricing the next engagement |
| C8 | No client's name reaches this public repository | AGENTS.md rule 1 |
| C9 | What is experimental is kept away from client work, or is safe there | The dispatcher, guard and outcome record are experimental (v0.7.0) |

## 3. Findings

Each finding says what is built, what is designed, and what is missing,
with its citation. F-numbers are for §5.

### 3.1 Confidentiality between clients (C1)

**F1. A project is a schema, and a connection is bound to one project at
the handshake.** Built. `admit` refuses a project that is unknown or
archived, a disabled principal, and a principal with no role in the
project (`internal/server/server.go:1440-1463`). Each connection's
`search_path` is that project's schema (`internal/store/db.go:143-160`),
and `who`, the inbox, the digest, cost and memories each read only the
bound project's store (`internal/server/agents.go:118`,
`inbox.go:150`, `digest.go:37`, `cost.go:58`, `memory.go:18-62`). A
refusal does not say whether the project exists
(`docs/security-model.md:384-391`). For C1 this is the right shape.

The isolation is the application's, not the database's: one Postgres
role owns every schema (`internal/store/dsn.go:110-119`), so a bug in
the store's routing would cross projects with nothing below it to
stop it. No finding shows one; the point is that a contract asking for
"logical separation" is met, and one asking for separate databases or
machines is not (§6, question 2).

**F2. A server admin is an admin of every project, and a key signs in to
every project its principal belongs to.** Built, and the main risk for
C1. `Control.Access` gives a config-file admin `RoleAdmin` in any
project without a membership (`internal/store/control.go:499-505`;
`docs/server.md:2035-2037`), and "a key signs in to every project its
principal is in" (`docs/server.md:2123-2125`). The maintainer is the
server admin, and an agent runs with the key its person uses. So an
agent working in client A's repository holds a key that reaches client
B's project: a prompt injected through A's code or issues could read
B's issues and memories with `aries` from the shell.

What exists to narrow it: a principal can be made in one project alone
(`aries admin principal add NAME --project P`,
`internal/cli/admin.go:203-216`); a repository can name its key with
`key:` in `.aries.yaml`, only a file inside the repository
(`docs/cli.md:220`), or `ARIES_KEY_FILE`; keys can end
(`--expires`, `internal/cli/admin_key.go:118-148`); and the guard can
deny `aries admin` and reads of a key's path to a Claude Code session
(`docs/design/guard.md:130`). None of these is a boundary on its own:
an agent under the same operating-system user can read any key file
the user can, and the guard says of itself that a deny rule is not one
(`docs/design/guard.md:107`). The boundary is a key per client that
the agent's user cannot read for another client, or a server per
client. Decision 97's agent principals by role
(`docs/design/aries.md:762`; `aries-6tu2ypf2`, design only) is the
long answer; a written recipe is the short one (§5, S3).

**F3. Memories, team scope included, stay in their project today; the
organization memory planned before v1.0.0 would cross projects.**
Memories are in each project's schema
(`internal/store/migrations/0001_init.sql:298-317`); the control store
has none (`internal/store/controlmigrations/0001_init.sql:17-77`). So
`team` and `project` reach the same people
(`docs/concepts.md:588-591`). But the guide says `team` "will reach
further in a later release" (`docs/concepts.md:591-592`), and
organization memory, "guidance leads write once for many projects", is
pulled forward before v1.0.0 (decision 93, `docs/design/aries.md:758`;
`aries-iakie56e`, not yet designed). Its issue proposes that it live in
the control store beside memberships. On a server of several clients,
a memory written there and seen in every project is a leak by design.
The design must make its reach a set of projects chosen by an admin
(one client's projects), never the server by default (§5, S5; §6,
question 6). The comment on `ScopeTeam`, "for everyone on the server"
(`internal/store/memory.go:34`), contradicts the package's own doc two
lines above (`memory.go:24`) and should say "the project's members"
until that design lands.

**F4. Inside a project, every member sees everything.** As designed:
"every member of a project reads every issue, comment, memory outside
`user` scope, everyone's hours and notes, and all cost. There is no
read-only role" (`docs/design/enterprise-review.md:257`;
`docs/security-model.md:409-417`; `who` shows machine names). This is
fine while the maintainer is the only person in a client's project. It
matters when a client's engineer joins: they would see the
maintainer's hours, notes and cost on their project (enterprise review
E6, open).

**F5. Text other than memories is not checked for secrets, and is kept
forever.** The secrets lint refuses a memory that looks like a
credential (`internal/store/memory.go:192-200`, `288-299`), and briefs, gates,
workflow values and the outbox; issue titles, bodies, comments,
handoffs and hours notes are not checked
(`docs/design/enterprise-review.md:111`;
`docs/security-model.md:415-417`), and the event log keeps every
version (`docs/server.md:4830-4834`). Client code review turns up
client credentials; an agent pasting one into a comment puts it in the
log for good (enterprise review E9, open).

**F6. People's names are in the daemon's log and every event.** A
principal's name is one namespace for the whole server
(`docs/security-model.md:391-395`). Telemetry puts a pseudonym in its
place by default (`internal/telemetry/people.go:22-33`), but "it does
not change the daemon's own log" (`people.go:23-24`), and events carry
the name. Decision 97 builds pseudonyms before v1.0.0 "before data
accumulates under real names" (`docs/design/aries.md:762`;
`aries-k723g26u`, open). For the maintainer alone it changes nothing;
once a client's staff have principals, every name made before
pseudonyms stays in that project's log.

### 3.2 Ending an engagement (C2)

**F7. No project can be deleted, and the export leaves most of a
project behind.** "No project is removed, and ariesd never drops a
schema" (`docs/server.md:2154-2155`); `aries admin project` has `add`,
`list`, `archive` and `activate` alone (`internal/cli/admin.go:69`).
The design names the gap: "`aries admin project delete` does not
exist; archiving is all there is. It would be the cheaper thing to
build first", and meets "a contract [that] requires a customer's data
gone at the end of an engagement" by deleting the project
(`docs/design/aries.md:1719`). Today the operator would run `DROP
SCHEMA` by hand, which the guide writes only as a restore step
(`docs/server.md:4771`), and the control store would keep the project's
row and members.

The export is `ariesd export-bd --project UUID`, which writes issues,
labels, dependencies and comments as bd JSONL
(`internal/bdimport/export.go:22-31`): no memories, hours, token usage,
prices or events. A whole project leaves only as `pg_dump --schema`
(`docs/server.md:4730-4733`), which a client cannot read without a
Postgres. And the week of daily dumps keeps a deleted project for seven
days more (`docs/server.md:4720-4721`), plus whatever the off-machine
copy keeps: a delete must say so.

### 3.3 Hosting, backups and restore (C3, C4)

**F8. The many-projects server has not run on a cloud provider's
machine, nor with ACME against a real CA.** Built and passing its gates
in the test stack (README, Roadmap, stage 3a). The installer ran in
Ubuntu systemd containers only: "no AppArmor enforcing, real boot,
time sync, network, ACME or the real release key" (`aries-g446t7vk`,
waiting on the maintainer for a machine; v0.7.0 "ships without it, by
the maintainer's decision", `docs/releases/v0.7.0.md:345-347`). ACME
has run against a test CA alone (`aries-pk55wedb`;
`docs/releases/v0.7.0.md:285-286`). A managed Postgres, a standby's
failover and point-in-time recovery have not run
(`docs/server.md:4826`, `4906-4909`, `5018`). The deployment guides
still say "Pre-1.0. Aries is not ready for production use yet"
(`docs/deploy/README.md:55-56`) and "There is no single sign-on"
(`docs/deploy/README.md:53-54`), and describe SSH-only setups where
HTTPS is now the default (`docs/deploy/team.md:10-13`): R1
(`aries-6sxub3rq`) may catch some of it.

**F9. Backups are documented and were rehearsed once; nothing encrypts
them, nothing restores them on a schedule, nothing alerts when they
stop.** The guide has a daily `pg_dump` by a systemd timer, consistent
across the control store and every project, a week kept, mode 0600,
and restores of the whole database or one project
(`docs/server.md:4664-4794`), run by hand on 15 October 2026 in an
Ubuntu container with two projects (`docs/server.md:4796-4806`); not on
a new machine nor under AppArmor (`docs/server.md:4806-4807`). Missing:

- **Encryption.** "Copy the files off the machine every day"
  (`docs/server.md:4727-4729`) is left to the operator; nothing says
  how to encrypt the copy. The enterprise example's bucket has a
  retention the VM cannot shorten (`docs/deploy/enterprise.md:99-111`).
- **A restore drill.** `deploy/docker/smoke.sh` dumps and restores
  nothing; enterprise review T7 asked for one ("Backups nobody has
  restored", `docs/design/enterprise-review.md:428`) and it is not
  filed.
- **An alert.** The alert rules watch the daemon, not the backup
  (`deploy/observability/alerts.yaml`); a timer that stopped firing
  is found at the restore.
- **A stated recovery point.** A daily dump loses up to a day
  (`docs/server.md:4811`); no guide says what a deployment should
  promise.

No `ariesd backup` command exists, and none is needed: the dump is
Postgres's, and the guide is right to use it.

**F10. One daemon, no standby.** As designed: DPoP's replay state is in
one daemon's memory, so "1.0 runs one daemon"
(`docs/design/transport.md:743`), and the examples run one VM
(`docs/deploy/README.md:47-52`). For one consultant this is fine: an
hour's outage costs an hour of agents' coordination, and the outbox
holds their writes meanwhile (stage 5).

### 3.4 Upgrades (C5)

**F11. Upgrades in place work and roll the binary back; the database
goes forward only, and the promise is one protocol version back.**
`ariesd upgrade` checks the signature, the checksum and the clock,
swaps the binary, restarts, and puts the old one back when the
30-second health check fails (`docs/server.md:4465-4481`). Migrations
run at start and an older `ariesd` refuses a database a newer one
migrated, so the guide has a dump taken before every upgrade
(`docs/server.md:4443-4455`); a full rollback is a restore of that dump
(`docs/server.md:4506-4510`). From v0.5.0 a release that changes the
protocol accepts the one before (decision 93,
`docs/design/aries.md:758`), and names on dashboards are kept
(`docs/server.md:3612`). Not run: `ariesd upgrade` from v0.6.0 itself,
and the migration on many or large projects
(`docs/releases/v0.7.0.md:348-349`).

What is not written down anywhere is a support line: which release
gets a security fix, how long one is supported, and where to report a
vulnerability. There is no `SECURITY.md` (enterprise review E15). For
the maintainer, who runs both ends, the one-version window is enough
if they upgrade the server first and every machine after, as the guide
says (`docs/server.md:4419-4432`); v0.7.0 already refused v0.5.0's
`aries`.

### 3.5 Sign-in for others (C6)

**F12. Keys are reviewed and enough; sign-in at an identity provider is
built, off, unreviewed, and its review waits behind v1.0.0.** The HTTPS
listener and key proof had a security review before v0.5.0
(`docs/security-model.md:622-628`), with the proof bound to the TLS
session since v0.6.0. Sign-in at a provider and workload sign-in are
"off unless an operator configures" them and "neither has had a
security review, and neither has run against a real provider"
(decision 91, `docs/design/aries.md:756`;
`docs/design/security-review-v1.md:512-520`). The tests and reviews
that would change that (`aries-hnbx3pz6`, `aries-2md7ubfv`,
`aries-hqvj47ft`, then `aries-ico4a37l`, `aries-ntlpdebi`,
`aries-ayyv6ju4`) are blocked by the held v1.0.0 epic through the 1.x
epic `aries-vojgudc4`. A subcontractor or a client's engineer can be
given a principal with a key, in one project, ending on a date. Sign-in
at a client's own provider would need those reviews pulled forward
(§6, question 9).

**F13. Facing the internet has two open edges.** No rate on new HTTPS
connections or handshakes from one address (finding L-5,
`aries-bt467whn`; `docs/releases/v0.7.0.md:287-288`), and behind a
proxy the key proof is bound to nothing
(`docs/server.md:1223-1229`). A server reached through a private
network (a VPN, or Google Cloud IAP as the enterprise example does)
has neither problem, and needs no ACME either if it uses
`tls: files` with a private CA.

### 3.6 Cost per client (C7)

**F14. Cost is per project, so per client, and there is no total
across projects.** `cost` reads the bound project
(`internal/server/cost.go:58-82`), and prices and plans are per project,
set by its admins (`cost.go:25-36`; `docs/server.md:2468-2549`): each
client's project needs the price table set again. A server admin
totals across projects by connecting to each, or with Sextant, which
reads up to 500 projects from its config
(`internal/sextant/config.go:30`, `51-62`) and is built from source
only (`aries-xriebv77`). Token capture is Claude Code's alone
(`aries-rnd3m44k`), and usage is what the client reports, "not billing
proof" (`docs/design/enterprise-review.md:129`). For pricing an
engagement and showing a client what agents cost, this is enough; for
an invoice line it is an estimate, and should be called one.

### 3.7 Keeping clients out of this repository (C8)

**F15. Rule 1 has no test that knows a client's name, and the dogfood
server is the likely leak.** `TestNoOldName` holds a former name out of
every file; nothing can hold out names the repository does not know.
The routes by which one would arrive: an Aries issue or memory on the
dogfood server quoting client work, then pasted into a design doc or a
PR body; a brief or an outcome record naming a client's repository; a
screenshot of a board or a dashboard with both servers' projects. The
cheapest guard is the separation itself: client projects on a
different server from the one that tracks Aries, never the same
`.aries.yaml`, and agents working on Aries never given a client
server's key. A second, optional one: a local, untracked file of names
that a pre-push hook greps for, which the repository can document
without holding any name (§5, S8).

### 3.8 Experimental parts (C9)

**F16. The dispatcher runs as its person, with that person's reach.**
The dispatcher, the guard and the outcome record are experimental, CLI
only (`docs/releases/v0.7.0.md:69-144`), and the dispatcher has "not
run unattended for long" (`docs/releases/v0.7.0.md:350-351`). Until
agent principals by role exist (`aries-6tu2ypf2`), a dispatched worker
acts as the maintainer: on a server of several clients, an admin of
every project (F2). Run it on client work, if at all, from a checkout
whose key is that client's alone. The guard is the opposite case: it
only denies, and is worth turning on in every client repository.

**F17. Offline work has not run on Windows** (`aries-rcc3pxec`). It
matters only if the maintainer or a client works from Windows.

## 4. v1.0.0: what holds it, and whether client work waits for it

Decision 93 holds v1.0.0 "as long as possible"
(`docs/design/aries.md:758`). The held epic `aries-foz66bi6` waits on:

| What | Issue | State | Needed for client work? |
|---|---|---|---|
| The project workflow setting | `aries-3yxddgjk` | Done (W0 to W4) | Already there |
| Organization memory | `aries-iakie56e` | Not designed | No; and its design must keep clients apart (F3) |
| The server-wide default workflow (W5) | `aries-42y6iz35` | Waits on organization memory | No |
| Pseudonyms and agent principals by role | `aries-fja447nw`, `aries-k723g26u`, `aries-6tu2ypf2` | Designs open | Agent principals help F2; pseudonyms before clients' staff join (F6) |
| P5 left: a real machine, AppArmor enforcing, ACME against a real CA | `aries-g446t7vk`, `aries-pk55wedb` | Waiting on the maintainer's machine | Yes, the machine; ACME only on a public name (F8, F13) |
| The surface review's renames after P1 | surface-review.md Q1, 3, 5, 6, 8, 13 | Partly done | No |

And v1.0.0 is when the protocol, names and settings freeze for 1.x
(rule 10; `aries-foz66bi6`). That promise is for people who run a
server and clients the maintainer does not control. For the
maintainer's own client work, they control both ends, and the promise
already in force (one protocol version back, names kept, migrations
forward, a dump before each upgrade) is what they need.

**Recommendation: start client work on the current minor release
(v0.7.0, or v0.8.0 if S1 to S4 change the server), with a support line
written down, and keep v1.0.0 held.** The support line, for the
README's status and `SECURITY.md`:

- The latest minor release is supported; a security fix ships as a
  patch of it (v0.7.1), not to older minors.
- Each release accepts the protocol before it, so the server is
  upgraded first and clients within one release.
- Migrations only go forward; take a dump before every upgrade.
- Names on dashboards and alerts are kept from v0.5.0.
- Sign-in at an identity provider, the dispatcher, the guard and the
  outcome record are outside that support until their reviews.

## 5. Slices, ranked

Ranked by how much of C1 to C9 each closes against what it costs. The
first four are what this review would want done before a client's
first issue goes in; the rest before a client's people join or a client
leaves. "Maintainer" marks work only they can do (a machine, a DNS
name, a decision).

**S1. Stand up the client server on a real machine** (C3, F8). One
small cloud VM, Ubuntu 24.04 LTS, through `install-server.sh`, with
AppArmor enforcing, reached over a private network; a separate server
from the dogfood one (F15). Record the run in the guide, closing
`aries-g446t7vk`; if it has a public name, run ACME against Let's
Encrypt's staging and then production, closing `aries-pk55wedb`.
*Cost:* half a day of the maintainer's time, about $19 a month
(`docs/deploy/team.md`), and a docs PR with what ran. *Issues:*
`aries-g446t7vk`, `aries-pk55wedb`.

**S2. Backups that leave the machine encrypted, a restore drill, and an
alert** (C4, F9). Docs: encrypt the dump before it leaves (to a
public key whose private half is off the server), or a bucket with a
retention the VM cannot shorten and the provider's encryption at rest;
say the recovery point. Code: a `smoke.sh` step that dumps, wipes,
restores and compares event counts (enterprise review T7); a gauge or
a textfile metric of the newest dump's age and an alert rule on it in
`deploy/observability`. *Cost:* small for the docs; a day or two for
the drill and the alert. *Issues:* new, "Restore drill in smoke.sh"
and "Alert when the newest backup is older than a day"; the docs can
ride with S1.

**S3. One key per client: a written recipe** (C1, F2). A section of
`docs/server.md` ("One person, several clients' projects"): a principal
per client in that client's project alone, its key in
`ARIES_KEY_FILE` for that client's checkouts (or `key:` with the key
inside the repository, kept untracked), the server admin's key never
on a machine agents run on, keys with `--expires`, the guard denying
`aries admin` and the other clients' key paths, and the limit stated
plainly: an agent under the same operating-system user can read what
that user can, so the strong form is an operating-system user, a VM or
a container per client. *Cost:* small, docs only, `go test ./scripts/`.
*Issues:* new, "docs: one person, several clients' projects".

**S4. Delete a project, and export one whole** (C2, F7). `aries admin
project delete PROJECT`: server admins only, of an archived project,
asking on a terminal and needing `--yes` without one; it drops the
schema, removes the project's members and row, and records
`project.delete` in the control store's log with who and when. The
guide says what remains: dumps until they rotate, off-machine copies,
telemetry already exported, and the people's principals, which other
projects may share. And an export of everything a project holds
(memories, hours, usage, events, with issues and comments) as JSONL a
client can read without Postgres, from `ariesd` on the server. A new op
is a protocol bump under rule 10. *Cost:* medium: a control-store
write, an op, a CLI command, tests against a real Postgres, the Limits
and Names-kept records, docs. *Issues:* new, "aries admin project
delete" and "ariesd export: a whole project as JSONL".

**S5. Organization memory's reach is a set of projects, never the
server** (C1, F3). Before `aries-iakie56e` is designed: add to its
issue the requirement that an organization memory reaches the projects
an admin names (an "organization" of one client's projects), that a
new project joins none by default, and that `prime` never shows one
outside its set. Fix the `ScopeTeam` comment
(`internal/store/memory.go:34`). *Cost:* a comment on the issue now; a
one-line code comment fix. *Issues:* `aries-iakie56e`, a comment;
the code comment can ride with R1 (`aries-6sxub3rq`) or any store PR.

**S6. A support line and `SECURITY.md`** (C5, F11). §4's five points
in the README's status and a `SECURITY.md` with where to report a
vulnerability and which releases get fixes; the deployment guides'
"Pre-1.0. Not ready for production" replaced by what is supported and
what is not. *Cost:* small, docs only. *Issues:* new, or folded into
`aries-6sxub3rq` if it is still open.

**S7. The secrets lint on issue text** (C1, F5). Warn, not refuse, on a
title, body, comment, handoff or hours note that looks like a
credential: a refusal of an agent's comment loses work, a warning in
the result lets it edit. Kept text is the enterprise review's E9.
*Cost:* medium: the store's write paths, result fields (no bump), MCP
wording, tests. *Issues:* new, "Secrets lint on issue text (E9)".

**S8. Keep client names out of this repository by habit and by a local
check** (C8, F15). A paragraph in CONTRIBUTING.md: client work on its
own server; nothing from it filed on the dogfood server; and,
optionally, a pre-push hook a contributor installs that greps the push
for names in an untracked file outside the repository. Rule 1 stays
the rule; this is how one keeps it. *Cost:* small, docs and a hook
script with its test. *Issues:* new.

**S9. Pseudonyms and agent principals before a client's people join**
(C1, C6, F2, F6, F16). The work is already filed and pre-1.0:
`aries-k723g26u` (pseudonyms), `aries-6tu2ypf2` (agent principals by
role). Both are designs first. *Cost:* large each. *Issues:* those.

**S10. Sign-in at a client's identity provider, only if a client asks**
(C6, F12). Pull the 1.x sign-in tests and reviews forward from behind
v1.0.0 (`aries-hnbx3pz6`, `aries-2md7ubfv`, then `aries-ico4a37l`,
`aries-ntlpdebi`; `aries-qm62r7t6` and `aries-yjskqbob` for one real
provider), by removing their block on `aries-foz66bi6`. *Cost:* large.
*Issues:* those.

**Not recommended now:**

- **A server per client** as the default. It is the strongest
  isolation (a database and a machine each, deletion by destroying the
  VM) and multiplies upgrades, backups and certificates by the number
  of clients. Keep it for a client whose contract asks for it (§6,
  question 2).
- **High availability.** One daemon and a daily dump fit one
  consultant (F10).
- **Cutting v1.0.0 for this.** §4.
- **Encryption at rest inside Aries.** The disk's and the bucket's
  encryption, which every cloud provider has, meet the same need
  without a key in `ariesd`.
- **The learning loop** (`aries-dmqqolg4` and its children), as the
  coordinator already decided: it does not move any of C1 to C9.

## 6. Questions for the maintainer

Each has this review's recommendation.

1. **Who will use the client server: you and your agents alone, or
   clients' engineers and subcontractors too?** *Recommendation:* you
   alone at first; S9 and S10 wait until someone else needs in, and
   F4 and F6 are read as written for then.
2. **One client server for every client, or one per client?**
   *Recommendation:* one, separate from the dogfood server, with a
   project per client; a server of its own for a client whose contract
   asks for separate infrastructure.
3. **Where will it run, and reached how?** *Recommendation:* a small
   cloud VM in a private network (a VPN, or IAP as the enterprise
   example), `tls: files` with a private CA or ACME if it has a public
   name; not open to the internet until L-5 (`aries-bt467whn`) is
   fixed.
4. **Can client work start on v0.x, with the support line of §4?**
   *Recommendation:* yes; keep v1.0.0 held.
5. **Is a project delete and a whole-project export needed before the
   first client, or before the first client leaves?**
   *Recommendation:* before the first client leaves, but build it
   soon: it is the one requirement with no workaround short of `psql`.
6. **Should organization memory reach only the projects an admin
   names, never the whole server?** *Recommendation:* yes, written
   into `aries-iakie56e` now, before its design (S5).
7. **Should pseudonyms come before any client's person gets a
   principal?** *Recommendation:* yes, as decision 97 says; while you
   are alone, they need not block.
8. **Will the dispatcher run on client repositories?**
   *Recommendation:* not until agent principals exist, or only from a
   checkout whose key is that client's alone (F16); the guard, yes, in
   every client repository.
9. **Does any client need sign-in at their own identity provider?**
   *Recommendation:* no until one asks; keys with an end date meet a
   subcontractor's case. If one does, pull S10 forward.
10. **How should backups be encrypted?** *Recommendation:* encrypt the
    dump to a public key before it leaves the machine, and keep the
    private key off it; or a bucket with a retention the VM cannot
    shorten and the provider's encryption, if you trust the provider
    with client data already.
11. **Should the README's status and the deployment guides say that
    Aries is supported for the maintainer's own use at v0.x, with §4's
    support line, once S1 to S3 are done?** *Recommendation:* yes (S6),
    and not before S1 has run on a real machine.
12. **Which slices should be filed?** *Recommendation:* S2's two
    issues, S3, S4's two, S6, S7 and S8 as new issues; S5 as a comment
    on `aries-iakie56e`; S1, S9 and S10 already have theirs.
