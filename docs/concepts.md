# How Aries works

This page explains the ideas behind every `aries` command and MCP tool:
identity, issues, claims, handoffs, the inbox, the registry of who is at
work and memory.
The [CLI reference](cli.md) and the [agent guide](agents.md) build on it.

## Identity: principals and sessions

- A **principal** is a person or an agent identity, such as `alice`. The
  server decides it from the SSH key that connected, so a client cannot
  claim to be anyone else
  ([Security](security-model.md#identity-comes-from-ssh-keys)). A key
  belongs to one principal, and a principal may have several. Nobody adds
  or removes a key of their own account: an admin does
  ([Principals](server.md#principals)).
- A principal is a **person** or a **service**. A service, such as a
  CI job or an unattended agent, answers to an owner, a person, and
  signs in with its platform's token by a **trust rule**, which names
  the repository or the cloud account whose jobs it admits, or by a key;
  never as a person does
  ([Service principals](server.md#service-principals)). Either may have
  a **display name**, what people see beside its name.
- A person's **identities** are their accounts at the company's identity
  provider: an address at a provider, such as `bob@example.com` at
  `google`. Nobody becomes a principal by signing in: a server admin
  adds the principal and invites the address, and the person's first
  sign-in binds the invitation, which lapses unused after 7 days
  ([Identities and invitations](server.md#identities-and-invitations)).
  A person signs in with `aries login`, through ariesd, at a provider
  the server's `login:` setting names ([Sign-in](server.md#sign-in)).
- A **session** is one run of an agent, or a person's terminal on one
  machine. Its id comes from the environment
  ([Sessions](agents.md#sessions)).

The names `ariesd` (the server's own actions, such as ending expired
claims), `import` (the bd importer and `ariesd control init`) and
`ariesd-upgrade` (the health check `ariesd upgrade` runs) and
`ariesd-status` (`ariesd status`, run on the server) are
reserved.

## Issues

An issue has an id such as `ar-a1b2c3d4`, a title, a type (`bug`,
`feature`, `task`, `epic`, `chore`, or `gate`, which only
[`aries gate add`](#gates) makes), a priority from 0 (critical) to 4
(backlog) and a status: `open`, `in_progress`, `blocked`, `deferred` or
`closed`. It can also carry a body, labels, a parent issue, comments,
acceptance criteria and [dates](#dates).

Every change is recorded as an event in the same transaction as the change
itself. The event log is the history: `aries history ID` reads it, and
`ready`, `blocked` and `digest` are computed from it when you ask.

## Dependencies: ready and blocked

`aries dep add FROM TO` records that FROM depends on TO. A dependency of type
`blocks` (the default) or `conditional-blocks` holds FROM back until TO is
closed. The other types (`related`, `discovered-from`, `duplicates`,
`supersedes`, `waits-for`) only link issues.

- **Ready** issues are open, and nothing holds them back: no open blocker
  on the issue or on any parent, nothing deferred, and no child of its
  own that is not closed. They come out highest priority first, then
  oldest first, except that an issue whose files overlap work another
  session holds comes after the rest
  ([Files](#files-what-an-issue-touches)).
- **Blocked** issues are not closed and have at least one open blocker,
  their own or a parent's, or are waiting on open children of their own:
  `blocked` says `waiting on 3 open children`.

### Parents and their children

An issue made with `--parent ID` (or given one with `update --parent`)
is a child of that issue. The two wait on each other in different ways:

- A child is held back by whatever holds its parent back: an open
  blocker of the parent's, or a deferral, holds every issue under it.
- A parent, of any type, waits on its children. While any child is not
  closed (open, in progress, blocked or deferred), the parent is not
  ready, so `ready` and `start` never offer an epic beside its own
  work; `blocked` lists it with how many children are open. Its
  children are not held by that.
- Once its last child closes, the parent is ready again: what is left
  is to check the whole and close it.

So an epic waits on its children with no dependency to add, and
`aries dep add EPIC CHILD` is refused as a cycle, saying so: the child
would be held behind its own parent, and neither would ever be ready.
For the same reason a child cannot depend on its own parent, or on
anything under it. Order children among themselves with dependencies
between siblings.

A `blocks`, `conditional-blocks` or `waits-for` dependency that would form
a cycle is refused, counting parents' waits as above.

A status of `blocked` or `deferred` is a note anyone may change back to
`open`, an agent included. To hold work back so that it stays held, use
a [gate](#gates).

## Gates

A gate holds issues back until a person passes it. It is an issue of
type `gate`, and what it holds depends on it with an ordinary `blocks`
edge, so everything above applies: a gate on an epic holds every issue
under it, and whatever waits on those. `aries gate add --blocks ID`
makes the gate and the edge in one step, naming its approvers with
`--approver P`, or none, which leaves it to the project's admins. An
agent may add one too (the MCP `gate` tool), and so hold its own plan
for a person to approve before any of it starts.

- **Held means held.** A gate is never ready and cannot be started, and
  neither can anything it holds. `close`, `finish`, `reopen` and a status
  change of a gate are refused: only `aries gate pass`, `waive` and
  `rearm` open or close one. Closing or finishing what a gate holds,
  removing its edge to the gate, or moving it out from under a gated
  parent is refused too, unless you could pass the gate yourself. Setting
  a held issue's status back to `open` is allowed and undoes nothing.
- **Who passes it.** The principals the gate names; when it names none,
  the project's admins, but not the one who made it (a person working
  alone names themselves). Only from your own terminal: a pass from an
  agent's session, `aries gate pass` run in an agent's shell included, is
  refused, since the agent signs in with your key. That is a speed bump,
  not a wall: an agent holding your key can pass your gates until agents
  have identities of their own.
- **Waive and rearm.** `aries gate waive ID --reason TEXT` passes a gate
  as an admin may, whoever it names, on the record. `aries gate rearm ID`
  reopens a passed gate, by its approvers or an admin, so that it holds
  again; work already started on what it holds goes on.
- **Why it is not ready.** An issue under a gated epic, such as one
  whose type the [workflow](#the-workflow) gives an approval before
  start, leaves `ready` with nothing said there, so the reason is given
  where you look. `aries blocked` and `aries show` on the issue name
  the gate that holds it, through its parent or a blocker, and who may
  pass it; the MCP `blocked` gives `gates` and `show` gives `held`, one
  line each with the command a person runs (`aries gate pass ID`). The
  result of creating an issue under such a parent, or of an update that
  sets one (MCP `create` and `update`, `aries create`), says it is held
  until the gate passes (`held`). These are fields of existing results,
  so they need no new protocol version, and an agent is still told to ask a person
  rather than pass the gate.
- **Who is told.** The approvers, in their inbox, when a gate is added
  and when more work is put behind it (`gate.pending`); for a gate that
  names none, the project's admins other than its creator, the
  project's own before the server's, at most the server's
  `approvers_per_gate` of them. The assignees of what it held, when it
  passes (`gate.passed`). Every change is an event, and a watching board
  (`aries tui`) shows the pass as it happens.

### CI gates

A gate of kind `ci` waits for a check rather than a person: CI reports
it, by running `aries` as a principal of its own
([CI gates](ci.md)). The gate names the check (`build`, `test`,
`landed`, …), its *subject*, the issue whose branch the check runs on,
and the CI principals that report it:

```sh
aries gate add --kind ci --check build --issue ar-a1b2 --approver ci-example --blocks ar-c3d4
```

`ar-c3d4` is then held until CI reports `build` passed on `ar-a1b2`'s
branch. An issue has at most one ci gate for each check, so a job that
knows only its branch and its check finds the gate; to hold another
issue behind the same check, `aries dep add ISSUE GATE`. A ci gate may
hold nothing back, and then only records the check on its subject.

- **Who reports.** Only a principal the gate names, from a CI job (the
  session `ci`, which `aries` takes where `CI=true`) or from that
  principal's own terminal; never from an agent's session. A person the
  gate does not name, an admin included, cannot pass it; an admin
  waives it, as any gate. A human gate is unchanged: only its approvers,
  from their own terminal.
- **Pass and fail.** `aries gate pass` closes the gate, and a pass of a
  closed gate changes nothing, so reruns are harmless. `aries gate fail`
  records the evidence (the commit, a link to the run, a short reason),
  leaves the gate open, and tells the subject's holder, or its assignee
  when nobody holds it (`gate.failed`); a later pass closes the matter
  and tells them too. A failure of a passed gate, from a later commit,
  reopens it, so what it held is held again; work already started goes
  on. A waived gate takes no report until it is rearmed.
- **Runs out of order.** A report carries its CI run's number, and a
  report of an older run than the gate's last changes nothing: a slow
  run of an old commit neither reopens a gate a newer run passed nor
  passes one a newer run failed. A failure of the commit that last
  passed still reopens the gate, whatever its run
  ([Runs that finish out of order](ci.md#runs-that-finish-out-of-order)).
- **Closed is not landed.** Closing an issue is its worker's say-so.
  The check `landed` is reported by the job on the default branch after
  the merge; until then `aries show` says `closed, not landed`, and after
  it `closed, landed` and when. A revert re-arms it only if something
  runs `aries gate rearm`. Where the project's
  [workflow](#the-workflow) requires `landed`, closed means landed:
  the issue cannot be closed before the check passes, and the pass
  closes it.

`aries show` lists an issue's checks, each with its last report.

## Dates

An issue may have a `defer_until` date, before which it is not ready,
and a `due` date. Set them with `--defer-until DATE` and `--due DATE` on
`aries create` and `aries update` (a date, `2026-11-02`, is midnight UTC; or
an RFC 3339 time); an empty value clears one. A deferred parent defers
its children.

Nothing runs when a date passes: an issue deferred to a date becomes
ready when the date is past, as `ready` computes it, and nothing ends,
expires or is reassigned when an issue falls due. `aries digest` and
`prime` notice them instead: the issues that became ready in their
window because their date came, those due within the server's
`due_soon` (a day by default), and those overdue, each from the
server's clock when you ask. A maintenance window is an issue deferred
to the window's start and due at its end, behind a gate if a person
must approve it first.

## Locks

A **lock** is a name in the project, such as `merge/main` or
`deploy/prod`, held around a critical section: a merge train, a deploy,
a shared staging database, an API with a rate limit. It is tied to no
issue, and holding one changes no issue; a claim is the lock on an
issue. A script, a CI job or a person takes one, most simply with
`aries lock run NAME -- CMD`, which holds the lock while CMD runs
([Locks](cli.md#locks)). Agents do not take locks themselves: there is
no agent tool for them.

- **Capacity.** A lock holds one holder at a time, or with `--capacity
  N` up to N at once, a semaphore. Its first acquire sets the capacity,
  and it keeps it while anyone holds or waits for it; once nobody does,
  the lock is gone, and the next acquire makes it again.
- **Holders and leases.** A holder is a session: a principal, a session
  id and a machine. A hold has a lease, 15 minutes unless asked
  otherwise (at most a day), and lasts only while it is renewed, as a
  claim's does. A hold whose lease ran out holds nothing, at once, even
  before the server's reaper ends it.
- **Epochs fence.** Every grant gets the project's next **epoch**, so a
  lock's epochs only rise. A renewal or a release names its epoch, and
  one that no longer holds is refused. `aries lock run` gives the command
  `ARIES_LOCK_EPOCH`, so that a resource that remembers the highest
  epoch it has seen can refuse a holder that lost the lock without
  knowing it.
- **Waiting.** A lock that is full is refused, or with `--wait` the
  acquirer joins its **queue**. The queue is served first in, first out,
  among the waiters still renewing their place: a place lapses two
  minutes after its last renewal, so a waiter that died never holds the
  queue up. A release grants the lock to the next waiter at once, and
  the reaper does so within its 30-second tick when a hold's lease runs
  out. The waiter's session is told with a `lock.granted` inbox item.
- **Losing a lock.** A hold ends when it is released, when its lease
  runs out, or when its principal loses access to the project; the
  holder's session is told (`lock.lost`) unless it released the lock
  itself. Each grant, wait, release, expiry and leave is an event of
  the project, `lock:NAME` in its log.
- **Never offline.** No lock command waits in the outbox: without the
  server, `aries lock` fails at once, before the critical section.

## Claims and leases

Before an agent or a person works on an issue, they **claim** it with
`start`. The claim makes the issue `in_progress`, assigns it to you and
leases it to your session. Only a claim can set `in_progress`; `update`
cannot.

While the lease runs, other principals cannot change the issue. Their
`start` is refused and points them at the next ready issue. Their
`update`, `close`, `reopen`, `finish`, `handoff` and `accept` are refused
with `forbidden`, naming the holder. Anyone can still comment on it, label
it or link it.

| Who claims | Lease | Kept alive by |
|---|---|---|
| An agent, through MCP | 15 minutes | `aries mcp` renews it every minute while the agent runs |
| A person, from a terminal | 8 hours (`--for`, at most 24 hours) | `aries away 4h` extends all your claims, up to 7 days unless the server sets another `away_max` |

When a lease runs out, the server returns the issue to `open`. So a
session that crashes lets its work go within 15 minutes, and nothing stays
stuck. A claim also ends at once, the same way, when its holder loses
access to the project: the session is told in its inbox (`claim.lost`),
and the issue is ready for someone else.

A claim's **epoch** is a number that rises with each new holder, and
`show` prints it.
`finish` and `handoff --release` refuse a stale epoch, so a session that
lost its claim cannot close work that someone else has since taken.
`aries mcp` sends the epoch of each claim it took; from a terminal, pass
`--epoch N` for the same check. Another session of your own takes over a
live claim only when you ask (`aries start ID --take`). The same session
reconnecting keeps its claim.

**Admins** are principals named in the server's config. Only an admin may
change an issue another principal holds, which is recorded as an
`admin.override` event naming the holder, or force a close past open
acceptance items. See [Admins](server.md#admins).

A server may hold **many projects**, each with its own issues, memories
and members. You reach a project only as a member of it, or as one of
the server's admins. A member whose role in a project is `admin` has an
admin's rights in that project, and adds and removes its members with
`aries admin member`; the server's admins also create and archive
projects. When your membership is removed, your sessions in that project
end and your claims there are released; when your principal is
disabled, that happens in every project, and you are refused everywhere
until it is enabled, whoever you are
([Managing access](server.md#managing-access)).

## A session: start and finish

A unit of work is two steps:

1. `start` claims the top ready issue (or the one you name). It returns the
   issue, its acceptance checklist, the last handoff note, the
   [memories](#memory) relevant to it and a branch name such as
   `fix/ar-a1b2c3d4-fix-the-login-redirect`.
2. `finish` closes the issue, records a handoff note and files any work
   found on the way as new issues, linked `discovered-from`.

`aries start --new-branch` also checks the branch out, and `--worktree DIR`
creates a worktree on it. The MCP `start` tool never runs git; the agent
does.

**When the server is away.** The server is the one authority, and
nothing on your machine stands in for it. A session that cannot reach
it finishes what it holds and files new work: `finish`, `handoff`,
`comment`, `create`, `remember` and logged hours are kept in an outbox on
the machine and sent, once each, when the server answers. It starts
nothing new: `start` takes a claim, and no claim is made without the
server. A lease still runs out on the server's clock while you are
away (`aries away` extends yours before a planned absence). A queued
`finish` or `handoff` that arrives after its lease ran out still
applies if nobody took the issue since, and the issue's history says
the lease had lapsed (`claim.lapsed`); if somebody did take it, or the
issue was closed, it is kept as a comment on the issue, a `finish`'s
discovered issues are still filed, and you and the session that holds
the issue now are told in your inboxes. Nothing written is thrown
away, and nothing applies over someone else's claim. Reads
need the server: there is no local copy of the project
([When the server cannot be reached](cli.md#when-the-server-cannot-be-reached)).

## Files: what an issue touches

The server keeps, for each issue, the paths its work is likely to touch,
so that two sessions are not sent to edit the same files. They come from
two sources:

- **Commit paths** are what the work did. `aries` reads them from git and
  sends them: `aries mcp` with a renewal (at most every five minutes per
  issue) and with `finish` and `handoff`, and a terminal's `aries finish`,
  `handoff` and `away`. An issue's paths are those of the commits since
  the default branch whose `Aries:` trailer names it, or, on the
  issue's own branch, every commit on it and the uncommitted files.
  Commits a merge brought in are not counted. Earlier holders' paths
  stay with the issue.
- **Declared paths** are what someone expects the work to touch, given
  with `aries create --paths` or `aries update --paths`, relative to the
  current directory. A path ending in `/` is a directory and covers
  everything under it. `update --paths`
  replaces the declared set, and an empty value clears it.

Paths are relative to the repository's root, with forward slashes. An
issue keeps at most 200 (the server's `paths_per_issue`); past that, the
oldest commit paths make way for new ones, and more declared paths than
that are refused.

`ready` ranks an issue down, never out, when one of its paths equals a
path of an issue another session holds, or when a declared directory of
either covers a path of the other. It names those issues (`overlaps
ID`), and `show` lists the likely files and who holds the overlapping
work. Your own session's claims do not count, nor does a path that more
than 10 held issues share, such as `go.mod`. `start` with no id takes
the first issue that overlaps nothing, if there is one.

Reading git is a hint, never a requirement: with no git, no repository
or a detached HEAD, `aries` sends nothing and the work goes on.

## Acceptance criteria

The acceptance text of an issue becomes a checklist: each Markdown list
item (`- [ ] x`, `- x`, `1. x`) is one item, or the whole text is one item.
`aries accept ID N` ticks item N; `--undo` unticks it, and
`--waive REASON` waives it. `finish` can tick or waive items in the same
step (`--tick 1,3`, `--waive 2=REASON`).

An issue with an open item cannot be closed. An admin's `close --force`
closes it anyway and records the open items in the event.

A `- [x]` box counts as ticked only in the text an issue is created or
imported with. Later edits to the text tick nothing.

## The workflow

A project may have one workflow: a file in its repository, by
convention `.aries/workflow.yaml`, that a project admin applies with
`aries admin workflow set FILE` once the pull request that changed it
merges. For each issue type it names a profile; `default` is the
profile of every type not named, and a named type replaces it whole.

```yaml
version: 1
ci: [alice]                 # principals that may report a required check
types:
  default:
    acceptance:
      - PR opened from the template, saying what ran and what was left to CI
      - Tests run as the project's test tiers require
      - text: Reviewed and merged by the maintainer
        check: landed       # the check whose pass will tick it
    checks: [landed]
    closeout:
      - Delete files you left outside the repository
  epic:
    approve: {approvers: [alice]}
  chore:
    acceptance: [The result is posted where the issue says]
```

- **Default items, at create.** The server appends the profile's
  `acceptance` items to the acceptance text of every issue it creates,
  after the creator's own and each once, in the create's own write: a
  `create`, from any client, the issues a `finish` files as discovered,
  and an `update` that changes the type, which adds the new profile's
  items and keeps the old ones. The create's event records the
  workflow's rev and the profile, and `aries create` says how many items
  it added. Text that holds no list becomes the first item of one.
  Templates, gates and `ariesd import-bd` take none.
- **Leaving.** What the workflow added is the issue's own text: an item
  leaves only ticked or waived (`aries accept ID N --waive REASON`), on
  the record, as any item does, since an update that drops an open
  item is refused. An admin's `close --force` stays the override.
- **Not retroactive.** A later set changes no issue already made; the
  new rev applies to the next create. A create queued while the server
  could not be reached gets the workflow in force when it is sent.
- **Who sets it.** A project admin, or a server admin, from their own
  terminal: a set from an agent's session, `aries admin workflow set`
  run in an agent's shell included, is refused, as is a member's. No MCP
  tool sets or reads it. `aries admin workflow show` prints it, `diff
  FILE` what a set would change, and `aries workflow check FILE` exits 1
  when the server's differs from the file, for any member or a CI job.
  `set --rev N` refuses a set over a rev applied since.
- **Checked at set.** Unknown keys and types are refused, naming the
  key; each line must be one line of safe text with no secret in it,
  and an item must read back as itself; the principals it names must be
  members of the project; and a profile holds at most the server's
  [limits](server.md#limits) `workflow_items_per_type` items,
  `workflow_checks_per_type` checks and `workflow_closeout_per_type`
  close-out lines, in a document of at most `workflow_bytes`.
- **Required checks: closed means landed.** An issue whose profile
  names `checks` cannot be finished or closed until a [ci
  gate](#ci-gates) of each, with the issue as its subject, has passed
  (or an admin waived it): `finish` and `close` are refused with code
  `acceptance`, naming the checks and their last reports, and a fix to
  hand the work off (`aries handoff ID --state done`, with the pull
  request's link) instead. Nobody adds those gates: the first report of
  a required check makes it, in the report's write, reported by the
  workflow's `ci` principals, and a report by anyone else is refused. A
  pass ticks the items bound to its check; an agent may tick them too.
  An agent settles the rest before the merge, in its handoff: `handoff`
  with state `done` takes `ticked` and `waived` as `finish` does (the
  CLI's `aries accept` does the same), so the pass finds no item open.
  When a pass leaves every required check passed, no item open and no
  open gate holding the issue, the same write closes it, as the
  reporter, with the reason `landed`, ending any claim, and tells the
  assignee and the last handoff's author (`gate.passed`); with an item
  still open it closes nothing and says what is left, and whoever then
  finishes it is not refused. A later failure of the check, a revert's,
  unticks those items and reopens the issue (`check failed`), telling
  its assignee (`gate.failed`). An admin's `close --force` still closes
  it, recording the checks it overrode. What an issue requires is read
  from its own log, the workflow it was made under, so a later set
  changes nothing it requires. Meanwhile the issue waits in progress,
  holding nothing once its claim lapses: `prime` and `digest` list it
  as *awaiting landing*, with its checks' last reports.
- **Review before close.** A profile's `review: {approvers: [...]}`
  makes, in the create's own write, a [human gate](#gates) on the new
  issue that holds nothing: the issue can be started and worked as any
  other. `finish` and `close` are refused (`acceptance`, with the fix
  `ask a person to review …`) until the gate passes or an admin waives
  it, naming the gate and who passes it. It is passed as any human gate
  is: by an approver it names (else an admin other than the one who made
  it), and only from the session `cli`, a person's own terminal, never an
  agent's. The assignee and holder are told (`gate.passed`) that it
  passed; they then finish the issue. An admin's `close --force` closes
  past it, recording the gate it overrode. It is off unless a profile
  names it: where the host already requires a review before merge,
  `landed` means reviewed and a second review is ceremony. Use it for
  what the host does not see, such as a design, a decision or an
  operations task. "Not the author" waits for agent identities.
- **Approval before start.** A profile's `approve: {approvers: [...]}`
  makes a human gate that holds the new issue back, so that it and every
  issue under it stay out of `ready`, and cannot be started, until the
  gate passes: for an epic, a plan approved before its children begin.
  Its approvers are told (`gate.pending`).
- **The close-out checklist.** A profile's `closeout` lines are what
  the harness does when work ends: stop the compose stack you started,
  delete the files you left outside the repository, and for an epic
  prune worktrees and merged branches. Aries sees none of that, so the
  lines are shown, never ticked or checked. They come as `closeout` in
  the result of `handoff` with state `done` and of `finish`, and of the
  `finish` or `close` that closes an epic; in a second `gate.passed`
  inbox item when the closing pass closes an issue, since the session
  that did the work may be gone by then; and in `digest` as *epics to
  close out*, once an epic's last child has closed. An issue is shown
  the lines of every profile stamped on it, as with items and checks.

`prime` gives the workflow in one line, such as `workflow rev 4: bug,
feature, task: 4 items, landed; epic: approval`.

## Handoffs

`aries handoff ID NOTE` leaves a note for whoever continues. It can say how
far the work got (`--state done|partial|blocked`), what to do next
(`--next`), and which `--branch` or `--worktree` holds it. `--to P` puts
the handoff in P's inbox, and `--release` ends your claim and unassigns the
issue so someone else can start it.

`start` and `show` print the latest handoff. The worktree path is shown
only to your own principal, because it is a path on your machine.

## The inbox

Every principal has an inbox in each project. The server adds an item
in the same transaction as its cause, except a sign-in's, which follows
it in each of your projects:

| Item | When |
|---|---|
| `claim.lost` | Your lease ran out and the server ended the claim, another session took it over, someone else closed or released the issue, or, after your lease ran out, someone changed its status or assignee |
| `issue.handoff` | A handoff names you with `--to` |
| `issue.mentioned` | A comment, handoff or finish note says `@you`. Only principals the server has seen can be mentioned, and never yourself |
| `issue.assigned` | Someone else assigned you an issue |
| `signin.granted` | You signed in (`aries login`): in every active project you are a member of, from `ariesd`, with the provider, the flow, the client, the machine it said it was and when the sign-in ends. If it was not you, end it with the `aries admin signin end ID` the item names; `aries logout` on that machine ends it too. It names no address and no token, and the per-minute limit on notices never drops it |
| `conflict.parked` | An update of yours, queued while the server was away, parked fields someone else had changed, or was kept as a comment because the issue was closed meanwhile; a `finish` or `handoff` of yours, queued the same way, was kept as a comment because the issue had moved on; or another principal's parked fields, or another session's late `finish` or `handoff` kept as a comment, on an issue you hold ([Conflicts](cli.md#conflicts)). An item about your own write reaches every session of yours; one about the issue you hold, the session that holds it |

`aries inbox` lists unread items and `--ack` marks them read. `aries watch`
prints them as they arrive. An agent's `aries mcp` asks the server to push
new items, and adds `inbox: N new (call inbox)` to the agent's next tool
result.

## Who is at work

`aries who` lists every session that connected or renewed in the last 5
minutes, with its principal, machine, harness and the issues it holds. The
server records a session when it connects, and `aries mcp` keeps it present
while the agent runs.

`aries tui` shows the same work live: what is ready, each held issue with
its holder and how long they have held it, what is blocked, and every
change as the server pushes it ([The live board](cli.md#the-live-board)).

## The digest

`aries digest` summarizes a time window (`--since 24h` by default, `7d`, a
date or a time): what closed, started, is in progress or stalled, is
blocked, was handed off, created or discovered. An issue in progress with
no activity for 48 hours counts as stalled. The digest is structured data
for a standup or a status report. Aries runs no model; the agent writes
any narrative. It also totals the time issues were held in the window,
the hours people logged, the tokens reported in it and their cost at
list price ([below](#accounts-time-and-tokens)).

## Memory

A memory is something worth knowing in a later session: how deploys
work, a decision and its reason, a person's preferences. Each has a
**key**, a **body**, optional **tags**, an optional **issue** it is
about, and a **pin**. `aries remember` and the MCP `remember` tool write
one, `recall` searches them and `forget` deletes one.

**Scopes.** Every memory has one of three:

| Scope | Who can read and change it |
|---|---|
| `project` (the default) | Everyone on the server |
| `team` | Everyone on the server |
| `user` | Its author alone |

A `user` memory is private, and the server, not the client, enforces
it: no other principal can recall, list, change or forget it, and its
key and body never appear in an issue's history, the digest, the events
the server pushes, `prime` or `start`. The digest's count of events does
include a change to one, without saying what it was. Two people's `user`
memories with the same key are two records.

Each project's memories are in its own database, whether its server
holds one project or several, so today `team` and `project` reach the
same people: the project's. `team` is for what holds across the team's
projects, such as a review rule, and will reach further in a later
release; the scope is stored, and `recall --scope` filters by it, now. An agent remembers in `project` scope unless the
memory is the person's own preference, when it uses `user` and asks if
unsure, and uses `team` only when someone chose it.

**Keys and revisions.** A key names one memory in its scope. Each
memory has a revision, `rev`, that every change raises. `remember`
without `--rev` creates the key and is refused if it exists, naming the
stored revision. With `--rev N` it replaces revision N and is refused
if someone changed the memory since, so two edits of the same key never
overwrite each other silently. A replace sets the body, and leaves the
tags, the issue and the pin as they are unless it gives them; an empty
`--tag ''` or `--issue ''` clears them. New keys never conflict. `forget --rev
N` is refused the same way. Forgetting removes the memory but not its
revision count: remembering its key again continues from the next
revision, so a revision read before the forget does not match again.
A principal's oldest forgotten memories are deleted for good to make
room when new keys reach `memories_per_scope`; a key whose record is
gone starts again at revision 1.

**Recall** finds memories by text (in the key or body, in any case), by
exact key, by tag and by scope: pinned first, then the newest. There is
no search by meaning yet.

**Prime and start.** `prime` shows the pinned memories, then those
relevant to your in-progress issues (linked to one of them, or tagged
with one of their labels), then the newest, as many as fit its budget;
never in key order. `start` shows up to five memories relevant to the
issue it takes. Pinning is a person's call: `aries pin KEY` and `aries
unpin KEY`. Agents cannot pin.

**No secrets.** The server refuses a memory that looks like it holds a
credential, in every scope: a private key, an AWS access key id, a
GitHub, GitLab, Slack or Stripe token, a JSON web token, a bearer
token, a password in a URL or after `mysql -p`, a `password=` or
`secret:` assignment, or a long random token. The refusal names what
it looks like, never the text. Write where the secret is kept instead,
such as a vault path or an environment variable's name.

How large a memory may be, and how many each principal may keep, are
[server limits](server.md#limits).

## Accounts, time and tokens

Every issue reports its time and tokens against an **account**: a
client's engagement code name or an internal department, never a real
client name. Set one with `aries create --account NAME` or `aries update ID
--account NAME`; an issue without one inherits its parent's, up the chain,
and then the server's `account:` setting, `internal` by default. Account
names are lowercase letters, digits and inner hyphens.

**Time** is the time an issue was held under claims: from `start` until
`finish`, close, a releasing handoff, a takeover, or the lease running
out. Every harness gets it, even one that reports no tokens. People's own
time is logged by hand, apart from it ([below](#human-hours)).

**Tokens** come from the harness, never from the model's own account of
them: the client sends what the harness recorded for each request (model,
input, output, cache write and cache read), keyed by the harness's own
request id, so sending a record twice stores it once. A count the harness
did not report stays unknown, which `show` leaves out rather than printing
0. So far `aries` captures Claude Code's tokens, from its local transcripts
through hooks that `aries setup claude-code` installs, and sends only the
counts ([Token usage hooks](agents.md#token-usage-hooks-claude-code)).
Other harnesses report time only until their capture is built.

**Attribution** is worked out when you read it, from the claim history,
and never stored. A request goes to the issue its session held at that
moment. A turn or session record that spans several issues is split by
time held, and `show` and `digest` say some tokens were split; a stretch
when the session held nothing stays unattributed, and `digest` counts it
apart.
Splitting by time is an estimate when a session switches issues. A
record is split in whole tokens, so the parts add up to the record: the
issues' tokens plus the unattributed ones are the total.

### Cost

**Cost** is the tokens' list-price equivalent: what they would cost at
each model's API rates, whatever plan pays for them. An admin records the
rates ([Prices](server.md#prices)); a token is priced by its model's
rate in effect at the record's time, so a price change never rewrites a
past cost. Cost is worked out when read, like attribution, and exactly:
an issue's share of a split record is priced as the tokens it got, so the
issues' costs add up to the record's. A model with no price is shown with
its tokens and marked unpriced, never guessed at. A server with no
prices at all shows no cost.

`show` gives an issue's running cost, `digest` a window's, and `aries cost
--since 7d --group-by account` a report by account, issue, epic (the nearest
epic up the parent chain), person (who reported the tokens), model or
[outcome](#cost-per-outcome).
Tokens no issue was held for are `(unattributed)`, issues under no epic
are `(no epic)`, and a group is marked `split` when part of it came from
a record shared with another group. Invoicing stays outside Aries.

**Amortized cost** is what flat-rate plans really cost, spread over the
work. An admin records each plan's fee a seat a month, its seats and the
principals whose usage it pays for ([Plans](server.md#plans)). For each
calendar month (UTC) a report touches, a plan's monthly total, fee times
seats, is split across the report's groups in proportion to the tokens,
of all kinds, its principals reported that month; the share of tokens no
issue was held for goes to `(unattributed)`, so a whole month adds up to
the total. A window that covers part of a month gets the part of the fee
that its tokens are of the month's: from the 15th, with two thirds of a
month's tokens after it, two thirds of the fee. A month whose principals
reported no tokens still cost its fee: it goes to `(unattributed)`,
prorated by the share of the month the window covers. A fee accrues by
time: the current month has cost only the part of its fee that has
passed (on the 8th of a 31-day month, a week's worth), split the same
way, and a window that reaches past now charges nothing for the months
to come. A principal on no plan adds nothing. Reports show amortized cost beside the list-price
equivalent, never instead of it, and only `aries cost` shows it, since
`show` and `digest` would have to read whole months.

### Cost per outcome

Tokens alone say how busy agents were; what a team pays for is a change
that landed. `aries cost --since 30d --group-by outcome` sorts the
window's tokens by how the issue they were attributed to stood **at the
end of the window**, read from the issue's close and reopen events:

| Group | Holds |
|---|---|
| `landed` | issues whose last close gave the reason `landed` (what the closing pass of a required check writes, or a person who closes with that reason) |
| `closed` | issues closed for any other reason: by hand, forced, abandoned. The reason is free text, so Aries does not split them |
| `open` | issues never closed, or reopened since, in progress or not |
| `(unattributed)` | tokens nobody held an issue for |

Each group gives its tokens, list price, the issues counted (those with
tokens in the window) and the **rework**: the part of its tokens that
came after the first close of the issue they went to. A landed issue
that CI reopened and that took 30k tokens more before it landed again
shows those 30k as rework, while it stays one landed issue. `landed`
also gives the **cost per landed issue**, the same by issue type, and
then each landed issue, dearest first (at most `-n`): its cost, rework,
**held time over cycle time** (how long a session held it, against the
time from its creation to its last close; a small ratio means it waited
more than it was worked on) and the acceptance items and gates **waived**
on it.

How to read it:

- Cost per landed issue is the sum of the landed group's list price over
  its issues. It counts only tokens inside the window, so an issue that
  was worked before the window and landed in it looks cheaper than it
  was; use a window longer than your issues take, or `show` for one.
- A group's `open` cost is spent and not yet landed: work in progress,
  or work abandoned without a close. A large `closed` share is work that
  landed by hand or was dropped.
- The unattributed share (`(unattributed)` over the total) is spend that
  no issue explains: sessions that worked without `start`.
- A model without a price is shown as `unpriced`, never as $0: the cost
  of a group with unpriced tokens is a lower bound.
- Figures are computed when asked, from the prices in effect at each
  record's time. They are not measured, so there is no metric to alert
  on; the per-issue values live in this report on purpose.
- Held time counts every hold of the issue, wherever in time, and a
  waiver counts items and gates waived now.

### Human hours

People log their own time with `aries hours 1.5h ID`: an entry is one
person's time on one issue for one day, from a minute to 24 hours, with
an optional note. The day is the logger's calendar day: `aries hours` sends
today's date in your time zone unless `--on` gives another, and the
server takes any date from a year back to a day past its own today
(UTC). It counts against the issue's account, resolved
when read like tokens, and carries no money. A person's entries on one
day add up to at most 24 hours. Each entry and each undo is an event on
the issue, so `history` shows it; a person may undo only their own entry,
and an admin anyone's.

`show` gives an issue's hours by person, `digest` and `aries cost` the
hours of the days their window overlaps: a day counts whole when the
window touches it, so the default `digest --since 24h` at a morning
standup counts yesterday's hours, and windows from midnight to midnight
count each day once. Days are dates, compared with the window in UTC; a
window that reaches now also counts entries for tomorrow (UTC), already
today east of it. In `aries cost` the hours go with their issue's
account, epic or issue, or with who logged them; by model they are the
group `(human)`.
