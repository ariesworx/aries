# Security review of stage 3a: projects and access

An independent, adversarial review of the access control that stage 3a
added (design [§15](starfix.md#15-projects-and-access)), made on 12 Oct
2026 against `main` at `ea749e5`. It reports what was found and proves
what it can; it changes no product code. Each finding has a test that
reproduces it, named `TestReviewF…`, in a file named
`security_review_3a_test.go` in the package concerned. A test that
reproduces its finding skips with a message that states it, and fails
instead when `STARFIX_REVIEW_FAIL=1` is set; once the finding is fixed
the test passes and can become a regression test.

```sh
STARFIX_REQUIRE_DOLT=1 STARFIX_REVIEW_FAIL=1 go test -run TestReviewF -v \
  ./internal/server ./internal/client ./cmd/starfixd
```

## Summary

The core of the stage is sound. The control store's rules are enforced
inside each write's transaction, the handshake and the second check as a
connection is recorded close the races that were looked for, a project's
requests can only reach the store of the project its connection was
admitted to, and nothing a client sends reaches SQL, the keys file or a
log line unvalidated. No way was found for a member of one project to
read another, or for a project admin to become a server admin, by the
protocol alone.

What was found is at the edges: which key `sfx` offers, what an admin of
one project is shown about a principal who arrives from another, what
`control init` does when run again, and what a holder of any accepted
key can do before the hello. Nine findings, none critical:

| | Severity | Finding | Proof |
|---|---|---|---|
| F-1 | high | `sfx` offers ssh-agent's keys before the key `.starfix.yaml` names, so an agent given a narrow key acts as the developer | demonstrated |
| F-3 | high | A project admin's key, planted on a principal within reach, follows that principal into another project unseen | demonstrated |
| F-2 | medium | Decision 19 does not make an agent's access end with the admin key it was given | demonstrated |
| F-7 | medium | `starfixd control init`, run again, restores a membership an admin removed | demonstrated |
| F-4 | medium | One key holder, even a disabled one, can keep everyone from connecting | demonstrated |
| F-5 | medium with the change in flight, low on `main` | The store accepts a key that is another principal's line of `authorized_keys` | demonstrated up to the keys file |
| F-9 | low | Two archives sent together leave no active project, and the server cannot be managed or restarted | demonstrated |
| F-8 | low | A project admin can use up the server's principals, for good | demonstrated |
| F-6 | low | `key add` tells a project admin which keys and names exist | demonstrated |

## Demonstrated

### F-1 (high): the agent's keys are offered before the configured key

**Severity.** High: a process that was given a narrow principal's key
acts, without anyone being told, with the rights of whoever's ssh-agent
socket is in its environment, a server admin's included.

**Adversary.** A prompt-injected agent (4); and an honest person who
acts as someone other than they believe.

**Where.** `internal/client/conn.go:424` (`signers`): the signers are
ssh-agent's, then the file `key:` names. `ssh.PublicKeys` offers them in
that order and the server takes the first it knows.

**Reproduce.**
`TestReviewF1AgentKeyWinsOverConfiguredKey` (`internal/client`): the
config names the key of a principal `sandbox`, the agent holds a server
admin's key, the server knows both, and the connection is the admin's.

**What is gained.** `docs/security-model.md` advises giving an agent
that works on its own "a principal of its own, which is no admin". With
`key:` set to that principal's key and `SSH_AUTH_SOCK` inherited from the
developer's shell, which is the usual case for an agent started from a
terminal, every `sfx` command and every MCP call is made as the
developer. If the developer is an admin, the agent has `sfx admin`, and
F-2 then turns one injected command into access that lasts. Nothing
tells the person: only `prime` (`You:`) and the board show the principal
the server answered with; no other command prints it, and nothing
compares it with the key that was configured. `docs/cli.md` describes
`key` as what is used in place of ssh-agent, which a reader takes to mean
that naming a key chooses it.

Two smaller consequences of the same order, both demonstrated:

- **A configured key that is never tried.** With six other keys in the
  agent, the configured key is the seventh offered and sshd's default
  `MaxAuthTries` is 6. The refusal comes back as `unavailable` with the
  fix "check server.host and server.port", because
  `conn.go:166` recognizes only the text "unable to authenticate"
  (`TestReviewF1AgentKeysCrowdOutConfiguredKey`).
- **What a cloned repository's file chooses.** `.starfix.yaml` is
  committed. It cannot choose which of the agent's keys is used, but it
  names the server, pins that server's host key, and names a key file by
  any path on the machine (`~/.ssh/id_ed25519`, say). So a repository's
  author gets every public key in the agent, and that file's, offered to
  a server of their choosing by any `sfx` command run in the checkout,
  and by a hook set up with `--global`
  (`TestReviewF1ClonedConfigIsOfferedEveryKey`). Only public keys are
  disclosed, and signatures are bound to the session, so nothing can be
  replayed; it identifies the person, as plain `ssh` to an unknown host
  does. The server the file names also answers `prime`, whose text
  reaches the agent, fenced as data. The file must be the user's own and
  not writable by others, which a cloned file is.

**Smallest fix.** When `key:` is set, offer that key alone, using the
agent only to sign for it when the file is passphrase-protected (match
by public key). Map "too many authentication failures" to the `auth`
refusal. Separately, print the principal once where a person will see
it (`sfx admin …` results, or a `sfx whoami`).

### F-3 (high): a planted key follows a principal into another project

**Severity.** High: an admin of project A reads and writes project B,
which they have no role in, after B's admin does something ordinary and
is shown nothing.

**Adversary.** A project admin (2), against another project (1).

**Where.** `internal/server/admin.go:514-534` (`keysCarried`) shows the
keys a new member brings only when the caller `Reaches` the principal
(`admin.go:521`), and `keys` (`admin.go:635-643`) refuses `key list` on
the same rule. An admin of B who adds a principal from A never has reach
over it, since it belongs to A. The control store's rule
(`internal/store/control_authz.go:245`, `within`) guards the moment a
key is added, not what happens to the key afterward.

**Reproduce.** `TestReviewF3PlantedKeyRidesIntoAnotherProject`
(`internal/server`): alice administers A only; victor is a member of A
only; alice adds a key of her own to victor, which the rules allow. erin,
who administers B only, runs `member add victor`. The result lists no
key, and `key list victor` is refused to her. A connection with alice's
key is welcomed to B as victor and lists its issues; alice herself is
still refused there.

**What is gained.** The design's own argument for bounding reach is that
"a project admin could add a key of their own to someone with access
elsewhere and act as them there". The bound holds only for the order
"elsewhere first, key second". In the other order the key is added while
it is allowed and the access arrives later. The plant can also be a
principal the admin creates for the purpose and then asks another
project to add. The As built note of §15.4 says the admin who adds
someone from another project "should see what comes with them"; as
built, only a server admin does. `key.add` is in the control log under
alice's name, but no op reads that log, and victor is the only other
party who can list the keys.

A related case, read from the code and not run: roles are honored in a
project of any state (`administered`, `control_authz.go:167`, joins no
project state), so the admin of an archived project keeps reach over
every principal whose only project it is, and can add keys to them
before a server admin places them elsewhere or activates the project.

**Smallest fix.** Show every caller of `member add` the fingerprints,
`added_by` and age of the keys the principal already has, whenever it
has any, and let a project's admin list the keys of that project's own
members. A stronger rule, if wanted: a key added by a project admin is
valid only in the projects that admin administered when it was added.

### F-2 (medium): an agent's access does not end with the admin key it was given

**Severity.** Medium: it needs an admin's key to begin with, but it
defeats the one protection the design built against exactly that
adversary, and the security model says otherwise.

**Adversary.** A prompt-injected agent with a shell and an admin's key
(4).

**Where.** `internal/store/control_authz.go:234` (`ownKeys`) refuses a
key for the actor's own account and nothing else.

**Reproduce.** `TestReviewF2ServerAdminKeyOutlivesTheAdmin`
(`internal/server`): with server admin dana's connection, `key add dana`
is refused; `key add dave`, for the other server admin, succeeds, and so
does `principal add ghost --no-project --key …` followed by `member add
ghost --role admin` in every project. dana is then disabled. The key
planted on dave signs in and is shown the server's status as a server
admin; ghost signs in as a project admin.

**What is gained.** `docs/security-model.md:172-175` says of an agent
running `sfx admin` with an admin's key: "What it cannot do, even then,
is add a key to the admin's own account, so its access ends with the key
it was given." It does not: the same call with another admin's name, or
a new principal's, gives a second way in. A project admin's agent can do
the second half in its own project (create a principal with a key,
promote it), and that principal can later enable a principal a server
admin disabled, if it is within reach. Every step is an event in the
control database; nothing reads it over the protocol.

**Smallest fix.** Correct the claim in the security model now. Then
decide what the rule is for: if it is to stop an injected agent, adding
a key (to anyone) or creating a principal with one needs something the
agent does not hold: a second admin's approval, or confirmation on a
terminal that `sfx` refuses to give when it is not one. At the least,
refuse a key for any server admin's account over the protocol, as the
last-admin rule already treats them apart.

### F-7 (medium): `control init` restores a removed membership

**Severity.** Medium: a person removed from a project gets back in, with
no admin's action, when the operator runs a command the guide calls safe
to run again.

**Adversary.** A removed person (3), helped by an operator's routine
step (8).

**Where.** `cmd/starfixd/control.go:299-311`: for each principal with a
line in `authorized_keys` that has no role in the adopted project, the
importer calls `SetMember`. It cannot tell "never a member" from
"removed".

**Reproduce.** `TestReviewF7ControlInitRestoresARemovedMembership`
(`cmd/starfixd`): `control init` imports bob; a server admin removes him
from the adopted project; `control init` runs again with the same file;
the daemon then welcomes bob to the project.

**What is gained.** The line cannot simply be deleted to prevent it: a
line names a principal, not a project, so it stays while its owner
belongs to any other project, until their key is moved into the store.
The Docker image runs `control init` at every start
(`deploy/docker/entrypoint.sh`, `adopt`), so there every restart undoes
every removal from the first project of a principal still in
`STARFIX_KEYS`. The same run puts back a key removed with `key remove`,
which is part of the known weakness of a key in both places. A disabled
principal is not enabled, and stays out.

**Smallest fix.** Give a membership only to a principal the same run
created; for a principal that already has a row, add nothing and say so
in the summary.

### F-4 (medium): half-open connections shut everyone out

**Severity.** Medium: anyone whose key sshd accepts, including a
disabled principal and one in no project, can keep every person and
admin from connecting for as long as they keep at it; no data is
exposed.

**Adversary.** A removed or disabled person (3); a malicious client (7).

**Where.** `internal/server/server.go:348`: at most twice `conns`
connections exist, counted from accept. A connection takes its
per-principal slot only after its hello (`server.go:756`), and has 30
seconds to send one (`server.go:431`). Between the bridge frame, which
names the principal, and the hello, nothing is counted against anyone.

**Reproduce.** `TestReviewF4HalfOpenConnectionsShutEveryoneOut`
(`internal/server`), with `conns: 3` and `conns_per_principal: 1`: six
connections of one disabled principal, each past its bridge frame, and
the server admin's connection is closed unread.

**What is gained.** With the defaults it takes 2,048 SSH sessions, which
is about 205 connections at sshd's default of ten sessions each,
reopened every 30 seconds. sshd's `MaxStartups` does not apply: the
connections are authenticated. The admin who would disable the principal
cannot connect to do it, and disabling does not help while the key is a
line of `authorized_keys`, since the principal is checked only after the
hello. Rule 18 asks for a cap on what a principal can grow; this is one
it can.

**Smallest fix.** Count a connection against its principal as soon as
the bridge frame is read (`conns_per_principal`, summed over projects,
for those still in the handshake), and refuse there a principal that is
disabled or unknown to the control store.

### F-5 (medium with the change in flight, low on `main`): the store shadows a hand-maintained key

**Severity.** On `main`, low: nothing changes for the key's owner. With
the generated file read first, medium: a project admin can take away a
server admin's identity.

**Adversary.** A project admin (2), against a server admin.

**Where.** `internal/store/control_keys.go:146-165` (`addKey`) refuses a
key another principal of the store has, and knows nothing of
`authorized_keys`; `internal/server/admin.go:599` (`keyAdd`) does not
consult `byHand`, which `keyRemove` does.

**Reproduce.** `TestReviewF5StoreShadowsAHandMaintainedKey`
(`internal/server`): server admin dana's key is a line of
`authorized_keys` only, the documented way in for the first admin and
for replacing an admin's own key. Project admin alice runs `key add bob`
with dana's public key. It is accepted with no warning, and the
generated file gains `… --principal bob --key FINGERPRINT" DANAS-KEY`.

**What is gained.** sshd takes the first matching line. In the order of
`main` the hand-maintained line wins and dana is dana. In the order of
the change in flight, the generated line wins: dana's key signs in as
bob, a member of alice's project, with no admin rights, and F-1's lack
of any "you are" makes that hard to notice; her changes are recorded as
bob's. Public keys are not secret. That half was not run on a real
sshd; it follows from the first-match behavior the design measured
(§15.2, "Which file sshd takes"). On `main` the same write makes
`control init`, run later, skip dana's line as taken.

**Smallest fix.** In `keyAdd` and `principalAdd`, refuse a key that
`byHand` holds unless the line names the same principal; and before the
order changes, have the handshake refuse a store key whose material is a
line of `authorized_keys` for another principal.

### F-9 (low): two archives leave no active project

**Severity.** Low: it takes a server admin's two requests at the same
moment, but what it leaves needs SQL by hand to repair.

**Adversary.** An operator's mistake (8), or an agent with a server
admin's key (4).

**Where.** `internal/server/admin.go:330` reads the count of active
projects from the index, and `admin.go:335` archives in a transaction of
its own; the two are not one step.

**Reproduce.** `TestReviewF9TwoArchivesLeaveNoActiveProject`
(`internal/server`): two projects, a connection to each, each archives
its own, the sends staggered by up to 3 ms. In each of four runs both
committed within a handful of attempts (the test allows 24). The next
handshake is refused, for every project.

**What is gained.** Nobody can connect, so nobody can activate anything.
After a restart the daemon refuses to start (`cmd/starfixd/main.go`,
"holds no active project"), and its fix, `control init`, leaves an
archived project archived (`cmd/starfixd/control.go:229`).

**Smallest fix.** Make the check part of the write: in
`setProjectState`, refuse to archive when no other project is active,
inside the transaction.

### F-8 (low): a project admin can use up the server's principals

**Severity.** Low: availability only, and slow, but permanent.

**Adversary.** A project admin (2).

**Where.** `internal/store/control_principals.go:115` counts every
principal on the server against one cap; no op deletes a principal.

**Reproduce.** `TestReviewF8ProjectAdminFillsTheServersPrincipals`
(`internal/server`), with the cap set low: a project admin adds
principals to their own project until it is reached, and the server
admin's `principal add` for another project is refused.

**What is gained.** At the default rate (30, then one a second) the
default 5,000 take under an hour and a half. Each name is also taken for
good in the server's one namespace, so the same admin can hold the names
of people who have not been added yet. Recovery is the operator raising
`principals`.

**Smallest fix.** A cap on the principals one project admin may create
(counted by `created_by`), beside the server's.

### F-6 (low): `key add` tells a project admin what exists

**Severity.** Low: a project admin learns whether a given public key
belongs to anyone on the server, and whether a name exists outside their
projects.

**Adversary.** A project admin (2), about other projects (1).

**Where.** `internal/store/control_keys.go:151` (taken), and the order
of checks in `AddKey` (`control_keys.go:116-129`) and
`setPrincipalState`, which read the principal before the reach rule.

**Reproduce.** `TestReviewF6KeyAddTellsWhatExists` (`internal/server`):
"key … already belongs to another principal" for a key of someone out of
reach; "principal zed not found" against "frank belongs to a project
alice does not administer, or to none".

**What is gained.** `key remove` was written to answer alike in both
cases; `key add`, `principal disable` and `principal enable` are not.
The design accepts that a taken name says so at `principal add`; this is
the same fact by three more routes, plus the one about keys, which lets
someone who holds a person's public key (from a code host, say) confirm
that they use this server.

**Smallest fix.** For a caller who is no server admin, answer a missing
principal with the reach refusal, and a taken key as an invalid key.

## Read from the code, not demonstrated

- **The last-admin rule counts admins who cannot sign in.**
  `anotherAdmin` (`internal/store/control_principals.go:230`) treats a
  server admin the config names as available when it has no row, whether
  or not any key exists for it. With `admins: [dana, spare]` and no key
  for `spare`, disabling dana is allowed and nobody can then sign in as
  a server admin. The claim "a server is never left with nobody able to
  enable anyone remotely" holds only if every configured admin has a
  key. Note.
- **A name becomes a server admin after someone else made its row.**
  A project admin may create a principal of any free name, with their
  own key. If the operator later adds that name to `admins:`, the
  project admin's key is a server admin's. Nothing at start says that a
  configured admin's row was created by, or holds keys added by, someone
  who is no server admin. Note.
- **`control init` and the flag parser can disagree on a line.**
  `forcedPrincipal` (`cmd/starfixd/control.go:124`) takes the first
  `--principal` of a forced command, and `starfixd stdio` the last. Only
  the operator writes that file, so this is a note.
- **`key add` reads before it is authorized.** `keyAdd`
  (`internal/server/admin.go:604`) lists the named principal's keys
  before the store decides. The result is used only after the write
  succeeds, so nothing leaks; it is one database read any member can
  cause, limited by the control bucket.
- **Two daemons on one control database, and a restore.** Each daemon's
  index follows its own writes and rereads the database every 30
  seconds, so a removal made through one is honored by the other up to
  30 seconds late, and its connections are not dropped at all. The
  socket check stops two daemons on one socket, not on one database. A
  control database restored from a backup brings back removed keys and
  memberships, and the next tick writes them to the keys file. Both are
  operator matters; the server guide could say so.

## Could not determine

- **Anything that depends on a real sshd.** No Docker run was made for
  this review. The order of F-5, that `DisableForwarding yes` stops a
  client from opening the daemon's socket through a hand-written line
  that lacks `restrict`, and that no client-chosen environment reaches
  `starfixd stdio` (which reads `STARFIXD_*`), are read from OpenSSH's
  documentation and the guide's `Match` block, not run.
- **The peer check.** `peer_linux.go` was read and looks right; the
  review ran on macOS, where it is not compiled, and
  `TestCheckPeerOtherUID` was not run.
- **systemd, AppArmor and the providers' agents**, which the design
  already lists as unverified.

## Known weaknesses, verified

- **A key in both the generated file and `authorized_keys`.** As
  described. `keyHolds` (`internal/server/conns.go:49`) skips a
  connection that names no key, and `drop` matches on the fingerprint,
  so such a key's connections are neither refused nor closed by `key
  remove`; the warning is given. Disabling the principal does keep it
  out, as the warning says, because `admit` refuses a disabled principal
  before the key is looked at. Worse than described in one respect:
  `control init`, run again, puts the removed key back in the store
  (F-7).
- **The reach a project admin gains when a person leaves their other
  projects.** As described; `ReachGained` counts the admins and the
  warning is given. Not worse, but F-3 is its mirror image and has no
  warning.
- **Releases recorded under the server's name.** As described: the sweep
  writes as `starfixd`, session `access`
  (`internal/server/keys.go:355`); the admin's own release names the
  admin.
- **`forbidden` reveals that a project exists.** As described
  (`internal/server/server.go:798-813`). It is open to anyone whose key
  sshd accepts, a disabled principal included, since the project's state
  is checked before the principal's. Project ids are random UUIDs, so
  this confirms an id someone already has and finds none.
- **The loopback surfaces.** The metrics listener is a design only and
  not on `main`. Dolt's port on loopback was not probed.

On the three changes in flight: F-5 becomes serious with the keys-file
order reversed, and should be fixed before or with that change. Ending
claims in archived projects at start does not touch any finding. The
protocol reset to version 5 does not either; F-4 is before the version
is read.

## Looked at and found sound

Each of these was an attempt to break something, by reading the path end
to end and, where marked, by running it.

- **Isolation between projects.** Every issue op is dispatched to
  `sess.proj`, set once in the handshake from the project the principal
  was admitted to (`server.go:598`); no op takes a project or database
  argument except the admin ops, which go to the control store and are
  resolved through `Server.project` (`admin.go:118`). A caller who is no
  server admin gets one refusal for a project that does not exist and
  for one they have no role in. Ops before the handshake, a second
  bridge frame and a non-request frame all close the connection.
- **Authorization in the transaction.** Each control write decides
  inside its transaction, on the single writer, under `wmu`, and
  rereads the actor's own row so that a disabled server admin has no
  rights (`control.go:178-204`). Tried: a project admin acting on a
  server admin, on a principal in no project, on one with a project
  elsewhere, creating a server admin's name, adding an unassigned
  principal they did not create, and a plain member calling each write.
  All refused, by the existing tests and by reading.
- **Access that ends.** The index changes before the write returns, the
  drop comes after, and `track` records the owner before it asks again
  (`conns.go:28-39`), so a handshake that straddles a removal is seen by
  one or the other. Demotion is live because the project store asks the
  index on each write (`project.go:102`), and project stores are opened
  with no static admin list, so a disabled server admin has no rights
  there either.
- **Injection.** Database names are matched against a strict pattern
  where written and again where used, and come from the operator's
  block, never from a client. A keys-file line is built from a principal
  name that starts with a letter and holds nothing that needs quoting, a
  fingerprint of fixed form and the key as the parser writes it back;
  comments are never written. Key lines are refused without being
  echoed.
- **The keys file.** New file of mode 0600, renamed into place; refuses
  a link, a directory others can write, and a file it did not generate;
  the settings and the server both refuse it being `authorized_keys`.
  A stale file is not a way in: the handshake refuses a fingerprint the
  store does not hold for that principal.
- **The socket.** Directory 0700 and owned by the daemon's user,
  socket 0600, peer user checked on Linux, reserved names refused in
  the bridge and the daemon.
- **MCP.** The tool set sends no admin op, and takes no principal,
  session or project as an argument. The worst an injected agent does
  through MCP alone is what its principal may do in its one project.
- **Limits.** Frames are capped at 4 MiB in both directions, admin lists
  are paged, control writes have their own bucket per principal across
  projects, and the four control caps are enforced in the transaction.
  The gaps are F-4 and F-8.
- **Private keys.** `sfx admin key add` refuses a file that holds a
  private key before it connects, and sends only what the parser writes
  back.

## Not reviewed

`starfixd upgrade` and the release checks; `internal/safetext` and the
CLI's escaping of what it prints, beyond noting that refusals carry
caller-supplied names; the soak and its checks; the bd importer; the
project store's own rules (claims, memories, usage) except the admin
hook and `EndClaims`; the IAP transport; Windows. The Docker image and
`smoke.sh` were read, not run.

## Disposition

Added 12 Oct 2026, with the fixes, on the branch `fix/security-review-3a`.
The review above is left as it was written. Each fixed finding's
`TestReviewF…` test became an ordinary test that always runs, under the
name given here; commits are named by their short hash on that branch.
The maintainer decided two of the fixes, recorded as decisions 35 and 36
of the design ([§14](starfix.md#14-decisions)), and the design's As
built note of 12 Oct 2026 ([§15.4](starfix.md#154-sfx-admin)) says what
was built.

| | Disposition | Where |
|---|---|---|
| F-1 | Fixed (decision 35). A configured key is the only key `sfx` offers; `STARFIX_KEY_FILE` names it by any path (renamed from `STARFIX_KEY`, which `sfx` never reads, 12 Oct 2026); with none configured, the default identities and then ssh-agent's keys, five at most; a committed `key:` may name only a file inside the repository; `sfx whoami`; the refusals for no key accepted and for too many refused keys name the cause | `32c0c76`. `internal/client`: `TestConfiguredKeyIsTheOnlyKeyOffered`, `TestWhichKeysAreOffered`, `TestKeyRefusals`, `TestKeyPathStaysInTheRepository`, `TestKeyFile`, `TestKeyOfAConnection`, `TestDefaultIdentities`, `TestOffersAreCapped`, `TestAKeyIsNeverTakenForAPath`. `internal/e2e`: `TestWhoami`. `deploy/docker/smoke.sh`, on a real sshd |
| F-2 | Fixed as visibility, not as a rule: a server admin has total control (decision 14), so nothing refuses the scenario. The security model no longer claims that disabling ends it; `principal disable` counts what the account left; three lists show it | `2ae5aae`. `internal/server`: `TestWhatADisabledAdminAddedIsFoundAndRemoved`, `TestTrailReadRules`. `internal/store`: `TestControlTrail`. `internal/e2e`: `TestAdminTrail` |
| F-3 | Fixed (decision 36). `member add` of a principal with any key adds nothing until the keys, shown to every caller, are accepted; an admin of a project may list its members' keys | `1302d99`. `internal/server`: `TestPlantedKeyIsShownBeforeItReachesAnotherProject`, `TestMemberSetAcceptance`, `TestKeysReadableByAProjectsAdmin`. `internal/store`: `TestProposeMember`, `TestControlAdministersProjectOf`. `internal/e2e`: `TestAdminPrompts`, and the soak's `join` |
| F-4 | Fixed. A principal's connections without a hello are counted and capped (`handshakes_per_principal`); a server of many projects refuses a disabled or unknown principal on the bridge frame; the hello's deadline is 10 seconds; at the socket cap the oldest connection without a hello is closed | `1f39cc5`. `internal/server`: `TestHalfOpenConnectionsDoNotShutOthersOut`, `TestBridgeFrameRefusals`, `TestHandshakesPerPrincipal`, `TestConnectionCapClosesTheOldestHandshake`, `TestHelloDeadline`. `internal/store`: `TestControlStanding` |
| F-5 | Fixed on `main`, in pull request 67, with the change to the order sshd reads the two keys files in: the store refuses a key that `authorized_keys` gives to another principal, and holds back a store key that a later hand line gives to another. Merged here, a project admin is refused such a key in the words used for a key the store holds (F-6), so the refusal says nothing of that file. The review's test was removed as a duplicate | `internal/server`: `TestKeyOfAuthorizedKeysKeepsItsPrincipal`, `TestHandLineAddedLaterHoldsTheStoreKeyBack`, `TestRefusalsDoNotSayWhatExists` |
| F-6 | Fixed for what a refusal says. For a caller who is no server admin, a name no one has is refused as one out of reach is, by `key add`, `principal disable` and `principal enable`, and a key some principal has is refused the same way whoever has it. Not removed: someone who may add keys can still infer that a key is on the server, because a new key is added and a held one is not | `3dac290`. `internal/server`: `TestRefusalsDoNotSayWhatExists` |
| F-7 | Fixed. `control init`, run again, leaves a removed key, a removed membership and a disabled principal as an admin set them, and reports each; `--restore-removed-access` overrides | `964e973`. `cmd/starfixd`: `TestControlInitRerunKeepsWhatAdminsRemoved`, `TestControlInitRestoreRemovedAccess`, `TestControlInitRestoreFlagIsExplained`. `internal/store`: `TestControlLastEvent`. `deploy/docker/smoke.sh`, across a restart |
| F-8 | Fixed. A principal who is no server admin creates at most `principals_per_creator` principals (500). Removing a principal is not built | `62aa15e`. `internal/store`: `TestControlPrincipalsPerCreator`. `internal/server`: `TestProjectAdminCannotFillTheServersPrincipals` |
| F-9 | Fixed on `main`, in pull request 67: the check for the last active project and the archive are one step, under a lock. The review's test was removed as a duplicate | `internal/server`: `TestArchivesTogetherLeaveOneProject` |

The notes read from the code:

- **The last-admin rule counts admins who cannot sign in.** Left as it
  is. An admin's only key may be a line of `authorized_keys`, which
  starfixd does not judge, so the store cannot tell an admin who can
  sign in from one who cannot, and refusing to disable on that ground
  would refuse the documented way an admin is set up. The server guide
  now says to name only admins who can sign in, and what happens
  otherwise.
- **A name becomes a server admin after someone else made its row.**
  Fixed as a warning: starfixd logs at start each configured admin whose
  row, or one of whose keys, was set up by someone who is neither a
  server admin nor `control init` (`964e973`, `cmd/starfixd`:
  `TestServeWarnsOfAnAdminSomeoneElseMade`).
- **`control init` and the flag parser can disagree on a line.** Fixed:
  the importer parses a forced command with the code that parses
  `starfixd stdio`'s arguments (`964e973`, `cmd/starfixd`:
  `TestControlInitReadsAForcedCommandAsStdioDoes`).
- **`key add` reads before it is authorized.** Left: as the review says,
  nothing leaks, and the read is bounded by the control bucket.
- **Two daemons on one control database, and a restore.** Left to the
  operator. The server guide's `--restore-removed-access` paragraph
  names the restore case.

Found while fixing, and not in the review:

- **A server admin could not remove a key planted on their own
  account.** Decision 19 refused removal as well as adding, so in F-2's
  scenario the admin whose account was given a second key had to ask
  another server admin, or the operator. The maintainer amended it the
  same day (decision 38): anyone may remove a key of their own account,
  and nobody may add one; the daemon refuses removing the key the
  connection itself signed in with. F-2's test now ends with the victim
  cleaning their own account (`internal/server`:
  `TestWhatADisabledAdminAddedIsFoundAndRemoved`, `TestOwnKeyRemoval`;
  `internal/store`: `TestControlOwnKeyRemoval`).
- **The review's F-4 test raced.** It read the server's connection map
  under the test fixture's mutex, not the server's, and so failed under
  `-race` whether or not the finding reproduced. Its replacement uses
  the server's.
- **`key:` with a path outside the repository is now refused**, which
  changes what an existing `.starfix.yaml` with `key: ~/.ssh/…` does.
  The refusal names `STARFIX_KEY_FILE`.
