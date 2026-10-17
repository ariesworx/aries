# Security model

This page explains how Aries decides who you are, what each principal
may change, and how it handles text, files and releases from others. The
setup steps that apply these rules are in [Running a server](server.md).

## Identity comes from SSH keys

Two transports reach the server: HTTPS, which a fresh server serves,
and SSH, which an operator turns on ([SSH on and off](server.md#ssh-on-and-off)).
On either, a key the server knows decides who connects, or over HTTPS a
sign-in the server issued ([Sign-in and tokens](#sign-in-and-tokens)),
and the principal is the key's or the sign-in's, never a field the
client sends. Over HTTPS ariesd checks it itself ([The HTTPS
listener](#the-https-listener)); over SSH, sshd does, as follows.

The server's sshd decides who connects. Each key's `authorized_keys`
line forces one command, `ariesd stdio --principal NAME`, so the
principal comes from the key that matched. A client cannot claim to be
anyone else, and nothing depends on paths or environment variables. The
line comes from `~aries/.ssh/authorized_keys`, or, on a server of many
projects set up for it, from the keys file ariesd writes, which sshd
reads first ([Who can authenticate](#who-can-authenticate)).

- `restrict` on each line turns off forwarding and terminals, and the
  sshd `Match` block repeats that for the whole account
  ([step 6](server.md#6-let-the-first-principal-in)).
- The same block turns off, for the account, every other way sshd might
  find a key: a key command the image sets for all accounts, and a
  certificate authority it trusts for all accounts. Either would let
  someone in as `aries` with no forced command, which is a shell that
  can read ariesd's config file.
- A `ForceCommand` would override every key's command, and so its
  principal. Never add one for the account.
- A session id, which comes from the client's environment, only tells one
  principal's sessions apart. It grants nothing.
- Token usage records carry no identity: the server stamps each with the
  connection's principal, session and machine, and keys them by
  principal, so one principal's records cannot shadow another's. A
  principal can still report false counts for its own sessions; usage is
  for reporting, not billing proof.
- To revoke someone on a server of one project, delete their key lines
  and stop their open sessions ([Principals](server.md#principals)).
- On a server of [many projects](server.md#serving-many-projects) the key
  still only names the principal. Which projects that principal reaches
  is decided by the daemon at each connection, from the control
  database: a server admin reaches every project, a member reaches the
  projects it belongs to, and anyone else is refused with `forbidden`,
  whether disabled, unknown to the control database, or a member
  elsewhere. The first two are refused as soon as sshd has let the key
  in, before the client says which project it wants, so a key that
  still authenticates and reaches nothing cannot hold connections open.
  Access that ends closes the connections it had let in: when
  a membership is removed, that principal's connections to the project;
  when a principal is disabled, all of its connections; when a project is
  archived, every connection to it; when a key is removed, the
  connections that key signed in. The daemon closes them once the change
  is committed, and the control database then refuses the next
  handshake. The first three also end the claims the lost access held,
  each with a `claim.release` event naming the admin and the reason, so
  nobody without access goes on holding an issue.
- **Disabled wins.** A disabled principal is refused at every handshake
  and has no admin rights anywhere, even when the config file names it a
  server admin. The last server admin who is not disabled cannot be
  disabled.

## Who can authenticate

On a server of one project, a key authenticates if it has a line in
`~aries/.ssh/authorized_keys`, which only someone with a shell on the
server can write.

On a server of [many projects](server.md#serving-many-projects) whose
sshd is set up for it ([Keys from the store](server.md#keys-from-the-store)),
sshd reads two files for the `aries` account, in this order, and a
key authenticates by the first line that matches it:

- **The keys file ariesd writes**, `~aries/.ssh/aries_keys`, which
  holds a line for each key the control database has for a principal
  that is not disabled: `restrict`, and a forced command naming the
  principal and the key's fingerprint.
- **`authorized_keys`**, maintained by hand as before, which only
  someone with a shell on the server can write. sshd reaches it only
  for a key with no line in the first file, so it is the way in for a
  key the control database does not hold: the first admin's, and a
  server admin's own new key. A line of it names no fingerprint, so
  ariesd cannot check the key it let in or close its connections
  when a key is removed.

**A key in both files** signs in by ariesd's line while the control
database holds it, as the principal the database gives it. `aries admin
key remove` then closes its connections and takes its line out of
ariesd's file, after which sshd finds its line of `authorized_keys`
and lets it in again. So removing such a key from the control database
does not revoke it until that line is deleted on the server. ariesd
runs as the account whose file it is, so it reads `authorized_keys`, to
tell admins which keys are in both: `aries admin key list` marks them, and
`aries admin key remove` of one warns that its connections were closed,
that sshd will still accept it, and that its line must be deleted.
ariesd never writes to that file, and never prints or logs a line of
it. A connection that came in by it names no key, and `aries admin status`
and `aries admin key list` count those.

**The order has a cost, taken on purpose.** A line of ariesd's file
that is out of date is matched before the same key's line of
`authorized_keys`, and ariesd refuses it at the handshake. So while
ariesd cannot write its file, a removed key that `authorized_keys`
would let in is refused, and is let in again once the file is written.
The out-of-date file is stricter than the two files in step, never
looser. With `authorized_keys` first, as an earlier, unreleased build
had it, such a key came in by the line with no fingerprint, and its
removal closed nothing and showed nowhere.

**A key in `authorized_keys` keeps its principal.** The control
database refuses a key that another principal already has, and sshd
reads its keys first. So a key that is only a line of `authorized_keys`,
such as a server admin's own new key, must not be added to the control
database under another name: its owner would then sign in as that
principal, and a project admin, who may create a principal with keys,
could shut a server admin out with nothing but their public key.
ariesd therefore applies the same rule to that file: `aries admin key
add` and `principal add --key` refuse, in the same words and after the
same checks of who may add a key, a key whose line of `authorized_keys`
forces the command for another principal. A line that forces no Aries
command names no principal and reserves nothing. A project admin is
told only that the key belongs to another principal; a server admin is
told that it is a line of `authorized_keys`. The check is made inside
the control database's write, so it cannot be raced or skipped, and a
refusal records nothing. `ariesd control init`, the operator's, is not
subject to it: it imports each key for the principal its line names.

A line the operator adds later, for a key the database already holds
under another name, cannot be refused, so ariesd gives way to it: it
leaves that key out of the file it writes, so that sshd reaches the
operator's line, refuses meanwhile a connection that an older file let
in by the database's line, logs the key, and counts it in `aries admin
status`. So in every order, a key in `authorized_keys` signs in as the
principal its line there names. The limits are those of how ariesd
reads the file: the first mebibyte, the first line for a key, and a
change it notices by the file's size or time.

Either way the key only names a principal. What that principal reaches
is decided afterward, by its memberships.

**A key may have an end** (`aries admin key add --expires`;
[Keys that end](server.md#keys-that-end)), and from it the key signs in
over neither transport. Three things enforce it, each on its own clock:

- **sshd**, by the `expiry-time` of the key's line, when a connection
  authenticates, on the wall clock. ariesd writes it in UTC, marked
  `Z`. sshd reads fourteen digits without the `Z` in the server's own
  zone, so on a server set west of UTC such a line admitted a key that
  had ended, for as long as the zone's offset: measured on OpenSSH
  9.6p1 and 10.2p1, in three zones (`spike/identity/RESULTS.md`,
  section 4). A test pins the `Z`.
- **ariesd**, at the handshake and in the proof over HTTPS, on the
  store's clock, which is the later of the wall clock and the latest
  time it has used (decision 37): never behind the wall clock, so ariesd
  refuses at or before the moment sshd would. A clock set back revives
  the line at sshd, and ariesd still refuses.
- **ariesd again, for open connections**, which sshd never checks a
  second time: a connection admitted a minute before its line's end
  was still served after it (Measured). At a key's end ariesd closes
  every connection it signed in, over SSH and HTTPS (decision 59), and
  takes its line out of the keys file. A timer is set for the earliest
  end, and set again after each key added and each tick of the reaper,
  which reads the control store again: a key `ariesd control init`
  added beside the daemon is seen there, so its open connections may
  outlive its end by up to that half minute, though no new one is
  admitted.

A key that has ended is deleted an hour later, quietly: its end was
recorded with its `key.add` event. So a replay of the control store's
event log must take a key whose end is over an hour past as possibly
gone.

**Who may add a key.** Server admins, and a project admin for a principal
whose every project they administer; a principal in no project, and a
server admin, are beyond a project admin's reach. Nobody adds a key to
their own account, a server admin included: an agent that holds a
person's key could otherwise add a key of its own and outlive the
person's.

**Who may remove a key.** Whoever may add one to that principal, and the
principal itself: anyone may remove a key of their own account, except
the key their connection signed in with. Removing only ever takes access
away, and without it the owner of an account that someone planted a key
on could not clean it; for the last server admin that would have meant a
shell on the server. What this costs is on the record here. An agent
that runs shell commands with a person's key can run `aries admin key
remove` as that person. It can remove the person's other keys, which is
a nuisance that the person, signed in with the key the agent holds, has
another admin undo with `key add`. It cannot remove the key it is
using, cannot add a key, and gains no access by any of it; each removal
is an event under the person's name. No MCP tool removes a key. Every key added and removed is an event in the control
database, under the admin who did it. Only public keys are stored, and
`aries` refuses to send a file that holds a private key.

**Nothing of Aries runs before authentication.** sshd reads files; it
runs no program of Aries's and opens no connection to ariesd until a
key has authenticated and its forced command starts. A stranger who
offers keys to the `aries` account reaches sshd and nothing behind it.
(An earlier, unreleased build had sshd ask ariesd about each key
offered, through an `AuthorizedKeysCommand`. That path is gone.)

**The keys file is a cache, and the control database decides.** What
keeps a removed key out is not the file:

- Each line ariesd writes carries its key's fingerprint, which the
  forced command passes to ariesd. At the handshake ariesd refuses a
  connection that names a key the control database does not hold for
  that principal, and one whose principal is disabled.
- So a key that was removed, or a disabled principal's, that is still
  in the file sshd reads, gets past sshd and is refused by ariesd.
  The file can be stale because ariesd could not rewrite it, because
  it was restored from a backup, or because someone edited it. A test
  states this directly: it removes a key while the file cannot be
  written, and again with the old file put back by hand, and the
  connection is refused each time.
- Anyone who can write the file can already write `authorized_keys`
  beside it, which is the stronger position for a key the control
  database does not hold: a line there names no fingerprint, and
  ariesd cannot check it. Both are in a directory
  only the `aries` account can write to.
- **Every line holds only what the control database holds.** The
  principal must be a principal name that is not reserved, the
  fingerprint must be the stored key's own, and the key written is the
  stored one, with no options and no comment. The path of ariesd in
  the forced command is the operator's setting, and must need no
  quoting.
- ariesd writes the file as a new file of mode 0600, renamed into
  place, and refuses a path that is a symbolic link, a directory that
  group or others can write to, and a file it did not generate.
- A failed write does not undo the admin's change. The admin is warned,
  ariesd logs it and tries again every 30 seconds, and `aries admin
  status` shows it until a write succeeds.

This was run with a real sshd, OpenSSH 9.6 on Ubuntu 24.04, in the
Docker test server, and under systemd on an Ubuntu 24.04 virtual
machine; and with OpenSSH 10.2 on Ubuntu 26.04, under systemd in a
virtual machine set up by the server installer. It has not been run on
a cloud provider's image
([Keys from the store](server.md#keys-from-the-store) lists what was
checked). The supported
platforms for a server are Ubuntu 24.04 LTS and 26.04 LTS; systems
that run SELinux are not supported yet
([What a server needs](server.md#what-a-server-needs)).

## The server's host key is pinned

`aries` checks the server's ED25519 host key against `host_key` in
`.aries.yaml` on every connection, and never trusts a key on first use.
A mismatch is refused; its fix says to confirm the new key with the
admin before you change the file. `aries` runs its own SSH client, so
nothing in `~/.ssh/config` can weaken the check ([The host key is
pinned](cli.md#the-host-key-is-pinned)).

While a key is rotated the file pins two, under `host_keys`, and `aries`
accepts a server that presents either: each key listed is trusted as
the server, as fully as one `host_key` is, so the list is as sensitive
as the single pin, and the old key is removed once sshd no longer
presents it ([Rotating the host key](server.md#rotating-the-host-key)).
Which listed key matched is kept nowhere and changes nothing else. The
file is committed, so whoever can change it can add a key, as they could
always replace the one; review a change to it as you would one to the
server's address.

## Which key aries offers

The server takes the first key it knows among those a client offers, so
which keys `aries` offers decides who you are.

- **A configured key is the only key offered.** When
  `ARIES_KEY_FILE` in the environment, or `key:` in `.aries.yaml`,
  names a key, `aries` offers that key and no other, whatever `~/.ssh` and
  ssh-agent hold, as OpenSSH does with `IdentitiesOnly`. ssh-agent may
  still sign for that one key, which `aries` finds by its public half,
  when the file is passphrase-protected or is the `.pub` file. So a
  process that was given a narrow principal's key acts as that
  principal, even with a server admin's agent socket in its environment.
- **With no key configured, `aries` does as `ssh` does**: the default
  identity files of `~/.ssh` that exist, in `ssh`'s order, then the keys
  in ssh-agent, and the server's first match wins. Nothing is exclusive
  here: a process with your home directory or your agent socket is you.
  That is the convenient default for a person and the wrong one for an
  agent with a principal of its own, which is why an agent's key is
  configured. sshd closes the connection after a few refused keys (six
  by default), so `aries` offers five at most and says how many it left
  out. A passphrase-protected or hardware-backed default file is used
  only through ssh-agent, and `aries` never asks for a passphrase.
- **The variable holds a path, and `aries` never prints a key.**
  `ARIES_KEY_FILE` is not trusted to be a path: a value that is
  plainly key material is refused without being repeated. `aries` reads no
  variable named `ARIES_KEY`, which earlier instructions for CI filled
  with a private key's contents; it notes only that one is set, when
  sign-in fails. No refusal, `whoami` line or log line holds what a key
  file or either variable contains, only paths and fingerprints.
- **`aries whoami` says who you are**: the principal the server took the
  connection for, the project and server, and the fingerprint of the key
  that signed and where it came from, with any default file that was
  skipped. `prime`, which an agent reads at the start of a session,
  names the principal on its first line and the key on its second.
- **A committed `key:` stays inside the repository.** `.aries.yaml` is
  committed, so a cloned repository's file chooses its `key:`. `aries`
  follows it only to a file inside that repository, after resolving
  symbolic links; a path outside, absolute or under `~`, is refused. A
  key elsewhere on your machine is named with `ARIES_KEY_FILE`, which
  only you set. A cloned file can therefore not make `aries` read a key of
  yours and show its public half to a server of its choosing. It still
  names the server, so with no key configured the server it names is
  offered the public keys of your default identities and your agent, as
  any host you `ssh` to is;
  signatures are bound to the session and cannot be replayed.

## Who may change what

- **A claim protects an issue.** While someone holds it, only the holder's
  principal may change it, close it or hand it off. Others get `forbidden`,
  naming the holder ([Claims and leases](concepts.md#claims-and-leases)).
  Anyone may still comment on it, label it or link it.
- **Admins may override.** An admin's change to an issue another
  principal holds is recorded as an `admin.override` event naming the
  holder, and a forced close records the open acceptance items in its
  close event. Only admins set model prices ([Prices](server.md#prices))
  and subscription plans ([Plans](server.md#plans)). A person logs hours
  only as themselves and undoes only their own; an admin may undo
  anyone's, on the record.
  Only the server's config file or environment lists admins
  ([Admins](server.md#admins)). On a server of many projects those are
  the server admins, and a project has admins of its own: members whose
  role there is `admin`, with an admin's rights in that project alone. A
  project admin's role is read from the control database on every write,
  so taking it away counts at once.
- **Who manages access.** On a server of many projects, `aries admin`
  changes projects, principals, their keys and memberships over the protocol
  ([Managing access](server.md#managing-access)), and the control
  database decides inside each write's transaction whether the caller
  may. A server admin may make every change. A project admin may add and
  remove their project's members and set their roles, and may create a
  principal as a member of their project. No MCP tool does any of it,
  and an agent whose principal is refused is told to ask a person. An
  agent that can run shell commands with an admin's key can still run
  `aries admin`, with that admin's rights: give an agent that works on its
  own a principal of its own, which is no admin, and a key of its own
  ([Which key aries offers](#which-key-aries-offers)).
- **Disabling an account does not undo what it did.** Nobody adds a key
  to their own account, so an agent holding an admin's key cannot give
  that account a second key. It can do everything else the admin can: a
  server admin may add a key to another server admin's account, and
  create a principal with a key and make it an admin of every project; a
  project admin may do the second in their own projects. Those keys and
  principals keep working after the first account is disabled. Disabling
  ends that account's own access and nothing it created. So `aries admin
  principal disable NAME` says how many keys NAME added that are still
  in the store, how many principals it created and how many memberships
  it added, and names the three commands that list them (`aries admin key
  list --added-by NAME`, `principal list --created-by NAME`, `member
  list --added-by NAME`). Review them, and remove what should not be
  there: nothing is removed for you. Each was also recorded as an event
  under NAME when it was made.
- **A project admin's reach is bounded.** A project admin may disable or
  enable a principal only while every project that principal belongs to
  is one they administer; a server admin, and a principal in no project,
  are out of reach. They may add to their project only a principal that
  already belongs to a project, or one they created. Otherwise they could
  bring someone within reach and act on them. Removing a membership can
  put a principal within reach of the admins of the projects that
  remain; `aries admin member remove` says so when it does.
- **A new member's keys are shown before they count.** Reach decides who
  may add a key at the moment it is added. A key then signs in to every
  project its principal later joins, and a project admin may add keys to
  someone who belongs to their project alone. So `aries admin member add`
  of a principal who has any key adds nothing until whoever runs it has
  been shown every key, with who added it, when, and whether that was an
  admin of the project being joined, and has accepted them: on a
  terminal by answering, in a script with `--accept-keys`. This is the
  same for a server admin. An admin of a project may list the keys of
  its members at any time, with or without reach over them; adding and
  removing keys still needs reach.
- **`aries admin status` shows a project admin their own projects only:**
  the connections, sessions and writes of the active projects they
  administer, and nothing of the server or of any other project. A
  server admin sees all of it. No one else is shown anything.
- **A refusal does not say what exists elsewhere.** Someone who is no
  server admin gets the same `forbidden` for a project they have no role
  in as for one that does not exist; for a principal that does not exist
  as for one out of their reach, when they add a key to it, disable it,
  enable it or list its keys; and for a key that does not exist as for
  one out of reach, when they remove it. A key that some principal
  already has is refused to them the same way whoever has it, without
  saying that anyone does. Two things are still learned, by design.
  Principal names are one namespace for the server, so `principal add`
  of a name that is taken says so. And a key belongs to one principal,
  so a project admin who may add keys to someone can tell that a key is
  on the server: a new key is added, and one already there is refused.
- **A gate is passed by whom it names, from where it says.** A human
  gate's approvers pass it from the session `cli`, their own terminal; a
  ci gate's principals report its check from the session `ci`, a CI
  job's, or `cli` ([CI gates](ci.md#who-may-report)). A session is the
  client's word, not a credential: refusing an agent's session is a speed
  bump against an agent that signs in with a person's or CI's key, not a
  wall. A CI pass is worth what the CI principal's trust rule, or its
  key, and the workflow it admits are worth: whoever can edit a
  workflow the rule admits can pass the gate ([What this
  costs](ci.md#what-this-costs), [Service principals](#service-principals)). A report's evidence
  is refused when it looks like a secret, and a link with a user or
  password is refused outright.
- **Every change is an event**, written in the same transaction as the
  change, with its principal, session and time. Clients cannot edit or
  delete events.
- **What others see.** `aries who` shows every principal's machine names. A
  handoff's worktree path, a path on your machine, is shown only to your
  own principal. Hours are not private: every principal can list anyone's
  entries, with their notes, and see them in `show`, `digest` and `aries
  cost`. Undoing an entry removes it from those, but its `hours.log` and
  `hours.delete` events keep the note in the issue's history. Notes, like
  comments, are not checked for secrets, so keep credentials out of them.
- **A user memory is its author's alone.** The server reads and writes a
  `user`-scope memory only for its author's principal; to anyone else it
  does not exist. Its events record its scope and revision but not its
  key, body, tags or issue, so history, the digest and pushed events
  carry none of it; the digest's event count still includes its changes.
  Forgetting one clears its body, tags and issue but keeps its key and
  revision in the row, so its revisions keep rising if it is remembered
  again, until the row is deleted to make room under
  `memories_per_scope`.
  `team` and `project` memories, like issues, anyone may read and change
  ([Memory](concepts.md#memory)).

## The database role

ariesd signs in to Postgres as a role of its own, and the rows of its
schemas are the history: the event log is the truth, and everything
else is derived from it. Whoever can sign in as that role can change
the history behind ariesd's back, which is why only ariesd's own user
can (below). A role with more rights than its own database would reach
further: other databases, other roles, the server's files.

So every command that opens the store first checks the role it
connects as, in `pg_roles`, and refuses to continue, with the SQL that
fixes it, when the role:

- is a superuser;
- has `CREATEROLE`, `CREATEDB`, `REPLICATION` or `BYPASSRLS`;
- is a member of `pg_read_all_data`, `pg_write_all_data`,
  `pg_read_server_files`, `pg_write_server_files` or
  `pg_execute_server_program`.

It also refuses a Postgres older than 16.

The guide's role ([step 2](server.md#2-create-the-role-and-the-database))
may log in and owns one database, made for it, in which it makes its
schemas: the control store's, and one for each project. Every other
role is kept from connecting to that database (`REVOKE ALL … FROM
PUBLIC`). Rights in other databases are not checked: every role may
connect to every database through `PUBLIC` by default, and what it may
do there rests on what was granted in it.

- **On the machine, there is no password.** Over Postgres's unix socket,
  peer authentication signs the Unix user `aries` in as the role
  `aries`, and no other user as it but root. Whoever can act as the
  `aries` user already owns ariesd's config file and binary.
- **On another machine, or a managed service,** the role has a password,
  kept in a file of mode 0600 that the `aries` user owns,
  `/etc/aries/postgres/pgpass`, which the DSN names. `ariesd` refuses a
  `--dsn` that holds a password, and the installer a `--dsn` that does;
  a config file whose `dsn` holds one must be mode 0600. With
  `sslmode=verify-full` the connection is encrypted and the server's
  name checked ([Managed services](server.md#managed-services)).
- **A schema ariesd did not make is never taken over.** `aries admin
  project add` refuses a name whose schema is already in the database,
  whoever made it, and ariesd never drops a schema.
- **One writer for each schema.** A Postgres advisory lock, held on one
  connection of each ariesd, keeps two daemons from writing one project
  at once, however many share the database
  ([One writer for each project](server.md#one-writer-for-each-project)).
- **The status report's figures** are read with the role's own rights,
  from `pg_stat_activity`, `pg_stat_database` and the sizes of its
  schemas. The role has no `pg_monitor`: it sees how many sessions other
  roles hold, not what they run.
- On a developer's own machine, `--dev --allow-unsafe-account` lets an
  unsafe role through with a warning. No config file or environment
  variable can set it.

## The daemon's socket

`ariesd serve` listens on a unix socket in a directory that must be mode
0700 and owned by the daemon's user. On Linux it also checks each
connecting process's user ID and refuses any but its own, which is why
`serve` runs only on Linux. `ariesd stdio` runs as that same user,
because sshd runs every forced command as the `aries` account.
sshd itself never connects to the socket.

Two commands connect to it directly, as the daemon's user and under
reserved principals that no key can sign in as: the health check of
`ariesd upgrade`, and `ariesd status`, which prints the report `aries
admin status` gives a server admin. The directory's mode and the peer
check are what keep both from every other local user; a test connects as
another user and is answered nothing.

With SSH off, the default, the daemon refuses a bridge frame from
`ariesd stdio` (`unavailable`, `SSH is off on this server`), writes no
keys file for sshd and removes one it wrote: a default install exposes
no SSH path. Only the upgrade's health check and `ariesd status` use
the socket then.

## The HTTPS listener

[The HTTPS listener](server.md#the-https-listener) is ariesd's own
front door, and so, unlike sshd, its authentication is Aries's own
code, on a port other machines reach. What it serves, and what bounds
it:

- **Two paths, and the sign-in's.** `GET /.well-known/aries` answers
  the server's version, its transports and how one signs in, with the
  providers' names: no principal, no secret. `GET /v1/connect` is a
  WebSocket whose protocol starts only after a sign-in's token and its
  proof, checked before the upgrade, or a key proof; with providers
  set, the sign-in's paths ([Sign-in and tokens](#sign-in-and-tokens));
  every other path is 404, and
  the loopback listener's, the status socket's and the bridge's paths
  are not served over HTTPS. A test of a default install
  (`cmd/ariesd`, `TestDefaultInstallExposes`) holds both: no path
  reaches the protocol without a proof, and no SSH path is open.
- **The key proof** (decision 89), for a connection with no sign-in. The client offers its
  keys; the server challenges the first it knows with 32 random bytes,
  fresh for the connection; the key signs the challenge, the server's
  URL and the key itself. The signature cannot be replayed on another
  connection, and cannot be relayed to another server, whose URL it
  does not name. The server checks the URL against `public_urls`, its
  own configuration, never `Host` or `X-Forwarded-*`, which a proxy may
  rewrite. RSA must sign with SHA-256 or SHA-512. A key is taken as
  sshd would take it: the control store's, unless the hand-maintained
  `authorized_keys` gives it to another principal, and then a line of
  that file that forces the bridge; a line that runs another command,
  or names a reserved principal, is no one's. So is a line with an
  option under which sshd takes its key only on a condition the key
  proof does not check: `from=`, `expiry-time`, `cert-authority` or
  `verify-required`. Over HTTPS such a key is refused as unknown, rather
  than let in without the condition. The connection carries the
  store's key's fingerprint, as one by the keys file does, so removing
  the key closes it, and so does the key's end. A key past its end
  proves nothing (decision 89), and is refused saying when it ended
  once its proof verifies; before that it is told no more than any key
  whose proof fails, so its public key, which is no secret, does not
  name its principal.
- **Offering keys tells the server which you hold**, and a server tells
  anyone who offers a key whether it knows it, as sshd does. Proving a
  key needs its private half.
- **A browser cannot connect.** A request with an `Origin`, which
  every browser sends for a page's WebSocket, is refused; no cookie or
  header a browser holds would sign it in anyway.
- **Limits** ([Limits](server.md#limits)): connections in all and per
  address, header size and time, body and message size, the time from
  the upgrade to the proof, and refused proofs per address, past which
  an address is refused `busy`. Behind a proxy, the per-address limits
  are the proxy's.
- **TLS** 1.2 or 1.3, with Go's suites, HSTS on every response, and a
  key file ariesd refuses to read if others may. Behind a proxy that
  terminates TLS, ariesd speaks plain HTTP to the addresses it trusts
  alone; the proxy then sees every frame, as the operator's own
  component. A smart card checked by ariesd itself would need TLS at
  ariesd, since a client certificate does not pass a proxy; ariesd
  checks none in 1.0, so a card is the identity provider's to check.
- **The certificate is what names the server.** `aries` verifies it
  against the system's roots and the CA in `server.ca`, which is added
  to them for that server, never in their place
  ([cli.md](cli.md#connect-a-repository)); nothing pins the
  certificate itself, since every renewal would break a pin. A
  certificate that does not
  verify stops `aries`, with a fix that says something may stand
  between you and the server. The key proof is bound to the server's
  URL and the connection's nonce, not to the TLS session, so the
  certificate check is what stands between a client and a relay:
  whoever holds a certificate the client trusts for the server's name
  (a certificate issued in error, a CA in `server.ca` whose key leaked,
  or a proxy that terminates TLS) can pass a client's proof on to the
  server and act as that client for that connection. Keep `server.ca`
  to the CA that issues the server's certificate. Over SSH, the pinned
  host key ([below](#the-servers-host-key-is-pinned)) does this job.
- **Nothing on the line is secret to ariesd's log.** A refused proof is
  logged with its reason and the address, sampled per address, never
  with the key's text.

v0.5.0 authenticates by keys: the key proof here, or SSH (decision 91
of [the design](design/aries.md#14-decisions)). Sign-in at an identity
provider and service principals' token exchange
([below](#sign-in-and-tokens)) are in the build but off unless
`login:` or `workload:` is configured, and their security review comes
in 1.x.

The listener, the key proof, the proxy mode, the limits under a flood
of unauthenticated requests, secrets and SSH off by default had a
security review before v0.5.0
([§12.4](design/transport.md#124-what-a-security-review-must-look-at-before-10)):
[security-review-v1.md](design/security-review-v1.md) records each
finding and what became of it, and the weaknesses left for v0.6.0,
among them the binding of the proof to the TLS session above.

## Sign-in and tokens

With providers under `login:` ([Sign-in](server.md#sign-in)), a person
signs in at the company's identity provider, through ariesd, which is
its own authorization server (design: [transport.md §6](design/transport.md#6-tokens)).
What holds, and what bounds it:

- **Nobody signs themselves up** (decision 57). A sign-in is someone
  only by an identity an admin invited: the first sign-in whose address
  the provider says is verified binds the invitation, once, to the
  provider's issuer and subject, and from then on that pair alone is
  matched. A later account with the same address and another subject is
  refused (the lapsed-domain case); two providers are never joined by
  address. Nobody invites onto their own principal, so an agent holding
  someone's key cannot point their principal at an account it holds.
- **The provider's token is checked by ariesd, never by aries.** The
  server exchanges the provider's code itself, with its client secret
  in a 0600 file, and checks the ID token: RS256 or ES256 alone (`none`,
  HMAC and `crit` refused), the issuer exactly, the client id among the
  audiences, `exp`, `iat` and `nbf` on the store's clock, the nonce the
  server sent, and `auth_time` within `fresh_auth`, with `prompt=login`
  sent, so that a browser with a session open does not renew a day with
  nobody there. Keys come from the configured URL, refetched at most once
  a minute, or from a file with nothing fetched; nothing in the code names
  a provider's host, and Entra's authority has no default.
- **aries's half of the sign-in** is OAuth's for a native app: PKCE S256,
  a state checked on the way back, the server's issuer checked as `iss`
  (RFC 9207) against a mix-up of servers, a loopback redirect on
  `127.0.0.1` that answers one path once, and a code of the server's,
  good for a minute and once, worth nothing without the verifier.
  `/oauth/authorize` never redirects to an address it did not check: a
  request that is not aries's is answered with a page.
- **The token** is 256 random bits and more, of which the server keeps
  only the SHA-256; it carries nothing, and ending it is a row at once.
  It is bound to a P-256 key aries makes at each sign-in (DPoP, RFC
  9449): each connection proves the key, by a proof for `/v1/connect` at
  the server's configured URL, never the request's `Host`, made within a
  minute, for this token (`ath`), and used once. A token leaked alone,
  into a log or through a proxy, is useless without the key. What DPoP
  does not do: the key is kept beside the token, so whoever has the
  user's data directory, or a backup of it, has both, for the rest of
  the day; the keystore that would close that is 1.x.
- **A day, then nothing** (decision 58): no refresh token, and nothing
  renews a sign-in. Its end, `aries logout`, `aries admin signin end`
  and removing its identity close its connections at once, each told
  why (decision 59). Disabling the principal ends its sign-ins too, so
  enabling it again brings back its keys and none of its old tokens.
  Ending a membership closes them as it closes any. A revocation whose
  write failed after it took effect is caught by the reaper, which
  closes, on each tick, the connections of any sign-in no longer live.
- **Scopes** ([Scopes](server.md#scopes)) are checked at every op, by a
  table a test holds to every op of the protocol, so a new op is a
  person's terminal's alone until decided: `mcp` and `ci` never reach an
  admin op, never take the session `cli`, and never decide a person's
  gate (decision 68). `aries login` asks for `cli mcp`, so a person's
  machine holds both; what it cannot tell is a person typing `aries`
  from an agent with a shell running it, as over SSH.
- **Replay** of a DPoP proof is refused within the daemon that saw it,
  which remembers each for two minutes (`dpop_proofs_per_server`).
  Several daemons in front of one database would each remember their
  own, so a proof replayed to another within its minute would pass; they
  would need the proofs seen shared, or DPoP nonces. 1.0 runs one.
- **Nothing secret is printed or logged.** No token, code, verifier,
  address or subject is in a log line, a span, an event or an error;
  `whoami` and `admin signin list` show a sign-in's id, provider,
  machine and end. The token and its key are 0600 files in a 0700
  directory of the user's, refused if anyone else may read them.
- **Agents.** `aries mcp` presents the person's sign-in too; when it
  ends, the agent is told to ask the person to sign in, never to run
  `aries login` itself.

## Service principals

With providers under `workload:` ([Service principals](server.md#service-principals)),
a CI job or a cloud workload signs in as a principal of kind `service`
by exchanging its platform's token (RFC 8693), and holds no stored key
or secret (design: [identity.md §7](design/identity.md#7-service-principals),
[transport.md §7](design/transport.md#7-service-principals-over-https)).
What holds, and what bounds it:

- **A trust rule says who.** A token signs in as the one service whose
  active rules at its provider it satisfies. A rule's claims are matched
  exactly, never as patterns, and must include the platform's immutable
  ids (a repository's and its owner's for GitHub Actions, the account's
  numeric id and address for Google, the tenant and object ids for
  Azure), never a name that can be renamed or recreated, never `sub`
  alone. Rules of two principals matching one token are refused, so the
  server never chooses between them.
- **Who may make one.** A server admin, never for their own principal
  (decision 19's reasoning: a rule lets whoever runs the jobs it admits
  sign in as the principal). Every `trust.*` is an event naming its
  author, and a line of the audit stream. Changing a rule's claims,
  disabling or removing it ends the sign-ins it gave, with their
  connections.
- **The platform's token is checked by ariesd**, by the same verifier
  as a person's ID token: the provider's issuer exactly, its keys, the
  server's audience, and an `iat` within `workload: fresh` (2 minutes).
  Where the platform lets the job choose the audience (GitHub Actions,
  Google), aries asks for one bound to the thumbprint of the DPoP key
  it made for the job, and the server takes no other: a token copied
  from a job's log is useless without that key. Every token exchanged is
  remembered, by hash, until it could no longer be taken, and taken once;
  one bound to no key (Azure's) that was issued before the daemon
  started is refused, since a restart forgot what it spent. Only a
  token that verified is remembered, so anonymous requests fill nothing.
- **What a sign-in gets.** Scope `ci`: a member's ops and a ci gate's
  report from the session `ci`, never an admin op, a waive or a
  person's gate. It is DPoP-bound, lasts the rule's lifetime (an hour
  by default, 6 hours at most), has no refresh token, and ends at
  `aries logout`; `grants_per_trust_rule` bounds a rule's live
  sign-ins.
- **What bounds a compromised workflow.** A job the rule admits gets the
  service for the rule's lifetime: a branch that edits the workflow, if
  the rule admits pull requests' jobs. Pin `ref`, `event_name`,
  `environment` or `job_workflow_ref` so that it admits only the jobs
  it should ([What this costs](ci.md#what-this-costs)). GitHub gives a
  fork's pull request no OIDC token. Each sign-in's event records the
  run it was for, so what a service did is one join from the run that
  did it.
- **Not tested against the real platforms**: GitHub Actions, Google
  Cloud and Azure are from their documentation, and the tests run
  against a stand-in issuer.

## The telemetry listener

With `telemetry: listen:` set, `ariesd serve` opens one HTTP listener
for metrics and health ([Observability](server.md#observability)). It is
off by default.

- **Loopback only.** ariesd refuses, at start, an address that is not
  loopback, and a host name with any address that is not; it binds the
  address it checked, not the name. No other machine can reach it.
- **No authentication.** Any user on the server can read it. Do not
  forward the port, and do not put a proxy in front of it that listens
  elsewhere, without adding authentication there.
- **What it reveals:** how busy the server is, by operation and result;
  the UUIDs of the projects it serves and how busy each is
  (`telemetry: per_project: false` leaves the projects out); how many
  connections, handshakes, refusals, claims and access changes there
  were; when the keys file was last written and whether the database answers;
  the ariesd version; and the Go runtime's memory and goroutines.
- **What it does not:** no principal, session, machine or key, no issue
  id, title or text, no memory, no project's name, no setting and no
  secret. An attribute's values come from fixed lists in the code, so a
  client cannot put a string of its own into a metric.
- **It only answers.** `GET /metrics`, `/healthz` and `/readyz`; nothing
  changes the server. It serves a mux of its own with those three paths:
  the debug pages that libraries linked into ariesd register on Go's
  default HTTP mux (`/debug/vars`, `/debug/requests`, `/debug/events`) are
  not reachable, and a test asks for them. Requests in flight, connections and the time each
  may take are capped, so a local user cannot hold the daemon's file
  descriptors or goroutines with it.

Metrics pushed over OTLP carry the same instruments and nothing more.

## Traces

With an endpoint for traces in its environment, ariesd exports spans
over OTLP/HTTP ([Traces](server.md#traces)): to a collector on the
machine in the guide's first setup, and to wherever that collector, or
the endpoint, sends them. They leave the machine, so what a span may
hold is narrow, and decided in one place.

- **A span carries ids, codes and small numbers.** The operation, the
  result's code, the project's UUID, the protocol version, an attempt's
  number, and how a part of the work ended. It carries no issue's title
  or body, no memory's text, no comment, no handoff, no key, no DSN, no
  token and no error's message, and it has no events and no links.
- **This is enforced where spans are made** (`internal/telemetry`), not
  by each caller's care: a span's name, its attribute keys and each
  key's values come from lists in the code, and anything else given to
  a span is dropped or recorded as `unknown` or `other`. Tests send
  requests with marked text in every field of every operation, served
  and refused, and search what is exported.
- **Of a caller's trace context a span takes the ids and the sampled
  flag.** They are hex, and checked. The `tracestate`, which is the
  caller's text, is put on no span and no log line.
- **Who did it is a setting**, `telemetry: principals:`. By default a
  span names the principal and the session by stand-ins: `p_` or `s_`
  and 16 hex digits, the start of an HMAC-SHA-256 under a key that only
  ariesd holds. `name` sends the names; `omit` sends nothing about
  who. A metric never says who, whatever this is set to.
- **A stand-in hides a name from the backend, and that is all it does.**
  It is stable on purpose, so that one person's requests can be followed
  and counted: what they did, when and in which project is all there,
  and on a small team that can be enough to tell who it is. It is not
  anonymity. Use `omit` where a backend must not be able to follow a
  person at all.
- **The key is a secret of the server's.** It is in a file of mode 0600
  that ariesd makes and that only its user reads. Whoever holds it
  can test a name against a stand-in. `aries admin principal whois` does
  that for a server admin and for nobody else, and the server never
  sends the key anywhere.
- **Without the key a span says nothing about who.** When ariesd
  cannot make or read the key, it does not stop, since a server that is
  down for want of a label in its telemetry would be worse; it does not
  make a key up in memory, whose stand-ins would change at each start
  and that nobody could resolve; and it never sends the name in its
  place. Spans carry no principal and no session, as with `omit`, until
  the key is there, and the log, the status report and `whois` say so
  and why.
- **A key file others can read is not used.** With any mode looser than
  0600, or content that is not a key, ariesd treats the key as one
  that may have been read or changed: it says exactly that, names
  nobody, and leaves the choice to the operator, who replaces the file
  (earlier stand-ins then resolve to nobody) or, knowing nobody read
  it, tightens its mode.
- **Deleting the key file, and restarting ariesd, makes every earlier
  stand-in unlinkable**, to the operator too, which is what a retention
  rule may want. A backup of the file undoes that for as long as it
  exists.
- **The setting does not change ariesd's log on stderr**, which
  names principals and sessions. Whoever reads the journal can read the
  databases. It does govern the log records ariesd exports over OTLP
  ([Logs](#logs)).
- **Traces are not a record of who did what.** Under the default
  sampler a caller's own trace context decides whether its request is
  recorded: a client that marks its requests unsampled leaves no span,
  and one that marks them sampled has every one recorded, up to what
  the queue holds. The event log is the record, and no client decides
  what goes in it. To record a fixed share whatever callers say, set
  `OTEL_TRACES_SAMPLER=traceidratio`.
- **A client cannot make the server hold spans without bound.** Spans
  wait for export in a queue of a fixed size; one more is dropped and
  counted.

## Logs

ariesd's log is on stderr, for the journal ([Logs](server.md#logs)).
It is the server's own record, and whoever can read the journal can
read the databases.

- **No log line holds what a person or an agent wrote, or a secret:**
  no issue's title or body, no memory's text, no comment, no handoff,
  no key's text and no DSN. A refusal is logged by its code, not its
  message. A line does carry the ids a client gave itself, as they
  came: its session, its machine, its harness and the op it sent; and
  an `err` is the server's own error, which may name an issue's id, a
  principal or a project. Tests send requests with marked text in every
  field of every operation and search every line.
- **The audit stream is a copy, and the event log is the record.** Each
  access change in the control database's log is also a log line with
  `audit=true`, read from that log after the change committed. It names
  who made the change, and a key by its fingerprint, never its text.
  The journal is not tamper-evident, and the lines are written only
  while ariesd runs: an auditor's source is the control database.
- **A request's id is not a secret and grants nothing.** `req` is 16
  random hex digits that the server puts on its lines and in the
  refusal it sends. It lets a person's report find the lines. A client
  that repeats one learns nothing by it.
- **Exported log records are held to the rule of spans.** With
  `OTEL_LOGS_EXPORTER=otlp`, off by default, each line also leaves the
  machine as an OTLP record ([Logs over OTLP](server.md#logs-over-otlp)).
  The record has the line's message, which is fixed text in the code,
  its event, and the fields that are ids, codes, numbers or fixed
  words, each only in its own shape. It never has an error's text, a
  path, a name, a machine or any field the schema does not list, under
  any setting. This is enforced where records are made
  (`internal/telemetry`), by each field's kind in `internal/logs`, and
  a test fails when a log call's message is not a string literal.
- **Who a record is about follows `telemetry: principals:`**, in every
  field that names a person, exactly as a span does: a stand-in by
  default, under the same key; the name with `name`; nothing with
  `omit`; and nothing, never the name, while the key is unavailable.
  The audit stream's records are no exception, so by default a backend
  sees who changed whose access only as stand-ins.
- **A client cannot make the server hold log records without bound.**
  Refusal lines are capped per principal a minute (`refusal_logs_per_minute`), and
  records wait for export in a queue of a fixed size; one more is
  dropped and counted. A collector that is down never slows a request.

## Confinement on the server

Three things limit what ariesd can do on its machine, and they cover
different processes.

- **Its own user.** ariesd runs as `aries`, which is not root and
  cannot use `sudo` but for one command, the restart of its own unit.
  That user owns the binary (so that `ariesd upgrade` can replace
  it), the config file, which holds the database's DSN, and the account's
  `.ssh`. So Unix permissions alone let any ariesd process rewrite
  all three.
- **The systemd unit**, for the daemon only: a read-only file system
  but for its socket directory and, on a server of many projects, the
  account's `.ssh`; no access to `/home`; private `/tmp` and devices; no
  new privileges. The bridge, `ariesd stdio`, is started by sshd, not
  by the unit, and has none of this.
- **AppArmor profiles**, optional, for Ubuntu
  ([Confine ariesd with AppArmor](server.md#confine-ariesd-with-apparmor)).
  The bridge may read the config file and connect to the daemon's
  socket, and nothing else: no network, no other file, no program. The
  daemon may also reach Postgres, read the database's client files and
  write the keys file. Neither may write
  the binary, the config file or `authorized_keys`. The operator's
  commands run under a third profile, which the operator asks for; it
  alone may replace the binary and run `sudo`.

What the profiles do not change:

- **A daemon that has been taken over can still let a key in**, by
  writing a line of its own to the keys file, which sshd reads. Nothing
  on the server stands between the daemon and that file; the profile
  keeps the bridge and the operator's commands away from it.
- **The daemon may open TCP connections to any address**, unless the
  unit holds it to loopback.
- **sshd and Postgres are not confined** by AppArmor: Ubuntu ships no
  profile for either. Postgres runs as its own user under its own unit.

The profiles were tested on Ubuntu 24.04 on arm64, in a virtual machine,
on 12 October 2026, and on Ubuntu 26.04 the same way on 14 October,
where the kernel stacks the daemon's and the operator's profiles on
`unconfined` and both still apply
([Confine ariesd with AppArmor](server.md#confine-ariesd-with-apparmor)): everything the server guide sets up ran with them
enforced and nothing was refused, and each thing above that they
forbid was tried and refused. amd64 was not tested, nor an upgrade to a
newer release under them. Without them, stock Ubuntu confines neither
sshd nor ariesd.

## The server's clock

The model relies on the server's clock and does not trust it to be
right. A lease runs out by it, every event is dated by it, and windows
such as the daily cap on usage records are counted back from it. Nothing
a client says about time is enforced, with one bounded exception: the
time a usage record claims may be at most five minutes past the server's
(`clock_client_tolerance`). A person who changes the clock of their own
machine changes what their records claim, within that bound in the
future and freely in the past, and nothing else: not who holds an issue,
not when a lease ends, not a daily cap, which counts a record by when
the server stored it.

Ordinary network time is not authenticated, so whoever controls the
server's clock, as root on the machine or by answering for its time
source, should be assumed able to move it. What that buys them:

| With the clock, an attacker | Can | Cannot |
|---|---|---|
| Sets it back | Slow the server's time to half speed until the clock has caught up, so leases last longer, by at most how far it was set back: a vanished agent's issue stays held that much longer. Make the next start of ariesd refuse (`clock_behind: stop`, the default), which is an outage until someone corrects the clock | Put an event before its predecessor, reopen a daily cap or an idempotency key's day, or revive a lease that had run out: ariesd's time never goes back ([design §14, decision 37](design/aries.md#14-decisions)). Stop a running daemon: it reports the clock and keeps serving |
| Jumps it forward | Date events in the future and roll windows forward: a day's cap on usage records reopens, an idempotency key's day ends (its reuse is then refused, not replayed), read inbox items and old registry rows are pruned early. Leave the clock there for a while and then correct it, which is the case above with history in the future | End every claim on the server: a jump that the monotonic clock did not see is added to every live lease. Shorten a lease, while ariesd runs, by more than the step tolerance (5 seconds) and a second an hour after that: small steps made quickly add up to one that is caught |
| Does either across a restart of ariesd | Lapse every lease by starting it with the clock far ahead: with nothing to compare the clock with but history, a restart after a jump looks like a long outage, and the claims end. Their holders are told and the issues are ready again; no work recorded is lost | Start it with the clock behind its history unnoticed |

None of it touches fencing. An epoch rises with each new holder whatever
the clock says, and a stale epoch is refused: a moved clock can change
when a claim ends, never let two sessions hold one issue or a stale
holder write after a takeover. The stores, the server and the soak test
check that through steps in both directions.

What an operator does about it is in
[The server's clock](server.md#the-servers-clock): several time
sources, authenticated where they offer it, and an alert on the clock
being unsynchronized.

## Untrusted text

Issue text comes from other people, their agents and the server. Aries
keeps it from acting on a terminal or posing as Aries's own output:

- **The server refuses unsafe characters** in every field: control
  characters (C0, DEL, C1), the Unicode bidirectional controls and marks,
  line separators and invalid UTF-8. Titles, names, labels, reasons and
  handoff fields are one line. Bodies, design, acceptance, notes, comments
  and handoff notes may also hold newlines and tabs. Text stored before
  this rule is kept as it is.
- **`aries` escapes what still arrives.** Any such character prints as a
  visible escape (`\x1b`, `\u202e`), and a one-line field's newline as
  `\n`, in every command, server error and quoted server stderr. With
  `--json`, the JSON encoding escapes them, so the value is the same and
  the output stays valid.
- **`prime` fences data.** It quotes titles and inbox text, each on one
  line, inside a `--- aries data ---` fence, after a line saying they are
  data. A failed hook quotes the error.
- **MCP marks others' text.** Results that hold it carry `"untrusted"`,
  and the server's instructions tell the agent never to follow
  instructions in that text. A fix the server wrote is relayed quoted, for
  the user.

## Files in a repository

A cloned repository is someone else's code, so `aries` treats its files with
care:

- **`.aries.yaml`** must be yours and writable by no one else (on macOS
  and Linux), and `aries` never looks for it above the repository's top
  level or your home directory ([Where aries looks for the
  file](cli.md#where-aries-looks-for-the-file)). Its `key:` may name only
  a file inside the repository ([Which key aries
  offers](#which-key-aries-offers)).
- **No commands in config.** A command in a committed file would run
  whatever the repository says, so there is no proxy-command setting.
  Google Cloud IAP is built in instead, and runs `gcloud` without a shell
  ([IAP](cli.md#servers-without-a-public-ip-google-cloud-iap)).
- **`aries setup` follows no symbolic links** inside the repository, and
  keeps the MCP entry it writes to what Aries needs: `--check` reports
  anything else in the entry, such as `cwd` or `LD_PRELOAD`, and `--write`
  drops it. A JSON file with a duplicate key is refused ([What setup
  writes](agents.md#what-setup-writes-and-what-it-leaves-alone)).
- **Global hooks run everywhere.** A hook set up with `--global` runs in
  every repository that has a `.aries.yaml`, and connects to the server
  that file names; Claude Code's usage hooks send it the session's token
  counts. Prefer per-project setup
  ([`--global`](agents.md#setting-agents-up-for-every-repository---global)).

## Transcripts stay on your machine

Token capture reads Claude Code's transcripts, which hold the whole
conversation, so it is built to take only counts from them
([Token usage hooks](agents.md#token-usage-hooks-claude-code)):

- **Read locally.** `aries usage --hook` reads the transcript files on your
  machine. Nothing else reads them, and there is no other network call:
  records go over the same SSH connection as every `aries` command.
- **Only counts leave.** Each record is the harness name, its request
  id, the model, the request's time and the token counts. The server
  checks every field against a fixed pattern or range. Prompts, replies,
  tool inputs and file contents are never sent, stored or printed.
- **Not held in memory.** Each line is decoded into a structure that has
  only those fields, and is dropped once read. A line over 4 MiB is
  passed over, not buffered.
- **The offsets file** (`aries/usage-offsets.json` in your user cache
  directory) holds transcript paths, byte offsets and a hash of each
  file's first 4 KiB, nothing of their content. It is mode 0600 in a 0700
  directory.

## The outbox

A write that cannot reach the server is kept on your machine and sent
later ([When the server cannot be reached](cli.md#when-the-server-cannot-be-reached)).
Before this, `aries` kept nothing of who you are and nothing you wrote on
your disk: its only file was the offsets file above, in the cache
directory. The outbox and the record beside it were the first, and
[the cache](#the-cache) of what you read is the second.

- **What it holds.** The queued writes as they would have been sent: the
  text of handoffs, comments, new issues, memories and hours notes, with
  the ids they name, the session and machine that wrote them, and when.
  They are plain files, not encrypted: whoever can read your account's
  files can read them, as they can your repository. Beside them are
  counts (op names, numbers and durations, no text) and what the server
  answered a refused write.
- **Where, and for whom.** Under your data directory (`ARIES_DATA_DIR`
  names another), never in a repository, in files of mode 0600 in
  directories of mode 0700: one directory for each server and project,
  named by a hash of a host key the config pinned when it was made and
  the project's id,
  and in it one outbox for each principal, named by a hash of the
  principal's name. On Linux and macOS `aries` checks, before every use,
  that each of those directories is owned by you and closed to the group
  and to others, as it checks a key file's owner; it refuses otherwise,
  with a fix, and never changes the mode of a directory it finds open.
  On Windows it checks nothing: ownership and access are in access
  control lists, which `aries` does not read, and the local application
  data directory is your own by default.
- **Secrets are refused before anything is written.** Every string of a
  write is run through the check the server runs on memories
  ([Secrets](#secrets)); one that looks like a credential is refused and
  not queued, and the refusal names the kind, never the text. The server
  runs that check on memories only, so the outbox refuses text in a
  comment or a handoff that the server would have taken. The same limits
  apply: it is a guard against mistakes.
- **How long.** Until the server answers the write, or you drop it. A
  delivered write is deleted from the outbox at the end of that replay.
  A refused one stays, with the server's code, message and fix, until
  you retry or drop it. One that waited 7 days is not sent and stays
  as refused. `aries outbox drop N` deletes an entry: the outbox is
  rewritten without it, and it is never sent. It is an ordinary delete,
  not a secure erase: the bytes may remain on the disk, and in its
  backups, as with any file. Nothing removes an outbox when your access
  to a project ends: when the server refuses you the project as
  `forbidden`, naming you, every entry that waits is marked refused with
  that refusal, and stays until you retry or drop it. At most 1,000
  entries and 4 MiB wait in one.
- **Whose it is.** Only the server says which principal a key is. So at
  each connection `aries` writes a record, `seen.json`, in the server and
  project's directory. It holds the fingerprint of the host key the
  directory was made under, and of the host keys `.aries.yaml` pinned
  at the last connection when they were others or several, the
  project's id, a format version, and for each key the
  server accepted from this machine its public fingerprint, the
  principal's name and when that was last confirmed (at most 32 keys);
  and which fingerprint was accepted the last time no key was
  configured; and the server's address as `.aries.yaml` gave it. It
  holds no private key, no path to one and no token.
  Beside it, a file holds the time the server was last not reached,
  another the time it was last reached, and
  one small file for each outbox holds its principal's name. While the
  server is away a write goes to the outbox of the principal recorded
  for the configured key, or, with none configured, for the key accepted
  last time, if `aries` would still offer it; with no such record nothing
  is queued.
- **One principal's writes are never sent as another's.** An outbox is
  sent only on a connection the server takes for the outbox's own
  principal, whichever key `ARIES_KEY_FILE` or `key:` names by then.
  When the server has given a key to another principal, what the first
  one queued with it is left unsent, and `aries` says so. A queued write
  is sent with the arguments it had, and the server applies it as the
  principal of the connection, with that connection's session and
  machine: the outbox carries no identity to the server.
- **A changed host key sends nothing.** What was queued under a host
  key the server no longer has stays under it, and is never sent by
  itself, even once the new key is pinned: a changed host key is what a
  person must look at. A rotation is not such a change: what was queued
  under the old key is the new key's too when a connection was made
  while `.aries.yaml` pinned both, which is the file saying, and the
  server proving by presenting one of them, that they are one server.
  Without such a connection, the old key's outbox is left as for any
  changed key. `aries outbox` lists it; `aries outbox retry N
  --host-key KEY`, a person's decision, moves an entry to the current
  server's outbox, and only when `.aries.yaml` names the server at the
  address it had when the entry was queued.
- **What the record is trusted for.** Only for choosing an outbox, and
  for knowing an old host key's outbox as this project's. A
  record that is wrong, or that someone with your account's access
  rewrote, can put a write in the wrong principal's outbox on your
  machine, where it waits for a connection of that principal; it cannot
  make the server take you for anyone, since the server decides that
  from the key alone.
- **Several people on one machine** are kept apart by the operating
  system: each account has its own data directory, which the check above
  holds to its owner. **Two accounts that share one home directory** (a
  container or a CI runner that mounts a home it reuses) share the
  outbox and the record, and are outside what this protects: give each
  its own `ARIES_DATA_DIR`, or set `ARIES_NO_QUEUE=1`. A machine
  that is lost takes its outbox with it; nothing replicates it.
- **Agents.** A queued tool call tells the agent it was queued and to
  carry on. No MCP tool lists, retries or drops entries: that is
  `aries outbox`, for a person.

## The cache

What a session read of its own context is kept on your machine, to be
shown while the server cannot be reached
([What is shown from the cache](cli.md#what-is-shown-from-the-cache)).
It is a copy of what the server showed you, and it stays on your
machine after your access ends unless something deletes it: Aries
cannot take back what it has shown.

- **What it holds.** The server's answers, as returned, to `prime`, to
  `show` of each issue read (and of the issue a `start` took, with its
  acceptance items, last handoff and claim), to `inbox` and to `digest`:
  titles, bodies, design notes, acceptance criteria, handoff notes,
  inbox items, digests, and the memories `prime` returned. Each with
  when it was kept, by your machine's clock. Nothing of `ready`,
  `blocked`, `list`, `who` or `claims` is kept. Plain files, not
  encrypted, as the outbox is.
- **Never another principal's private memories.** The server never sends
  them, so the cache cannot hold them: it holds only what the server
  showed the principal it is for.
- **Where, and for whom.** In the same server and project directory as
  the outbox, under the same checks: one directory for each principal,
  named by a hash of the principal's name, of mode 0700, and in it one
  file of mode 0600 for each answer, named by a hash of what it answers,
  and one that holds the principal's name. While the server is away the
  cache shown is that of the principal the record above names for the
  key `aries` would offer, by the outbox's rule, so another principal using
  the same machine and data directory is not shown yours. That rule is
  trusted for this as it is for the outbox: someone with your account's
  access, who could read the files anyway, can rewrite the record.
- **Read as data.** A file that is not a whole answer to what its name
  says, of the version this `aries` writes, is read as absent. An answer
  shown from the cache is marked as cached, with its age, on stderr for a
  person and in the result for an agent, and is escaped and fenced as any
  answer from the server is: its text is still other people's.
- **How long.** 7 days at most; and at most 500 answers, 16 MiB, and 1 MiB
  for one answer, for each principal. Each write removes what is past
  them, the oldest first. A removal is an ordinary delete, not a secure
  erase.
- **When it is deleted.** When the server refuses a connection because
  the principal's access to the project has ended (`forbidden`, naming
  the principal: disabled, removed from the server, or no longer a
  member), that principal's cache of the project is deleted from the
  machine at once, in a SessionStart hook too, and the command says so.
  An agent's session deletes it the same way. `aries cache clear` deletes
  every principal's cache of the project. Nothing deletes a cache on a
  machine that never connects again, such as a laptop that is lost:
  answers older than 7 days are no longer shown, but their files go only
  at the next write to the cache, which needs a connection. Removing the
  data directory removes them.

## Limits

Every principal is bounded: write rates, `aries admin` changes, a
principal's keys, the principals one project admin may create,
connections, connections that have not yet said hello, sessions, inbox
size, labels, dependencies, acceptance items, token usage records and
memories each have a cap, and text
fields and replies have fixed sizes. [Limits](server.md#limits) lists the
caps, their settings and what a refusal past each one says.

## Secrets

- No password goes on a command line, and a config file that holds one
  must be mode 0600 ([Settings](server.md#settings)).
- `.aries.yaml` holds no secrets, and is meant to be committed.
- **A token for a metrics or traces backend** is read from ariesd's
  environment only (`OTEL_EXPORTER_OTLP_HEADERS`, and the two for one
  signal), from a unit's `EnvironmentFile` of mode 0600: no flag and no
  config key takes it. ariesd never logs it: not at start, and not in
  an export error of either signal, from which every header's value is
  removed even when the backend's answer repeats it. Sending to a
  collector on the same machine needs no token at all
  ([Observability](server.md#observability)).
- **The key of the stand-ins in traces** is made by ariesd, kept in a
  file of mode 0600 that it refuses when others can read it, and never
  logged or sent ([Traces](#traces)).
- **Memories refuse secrets.** A memory that looks like it holds a
  credential is refused in every scope, `user` included, and by
  `import-bd`: private keys, AWS access key ids, GitHub, GitLab, Slack and
  Stripe tokens, JSON web tokens, bearer tokens, passwords in a URL
  (`scheme://user:password@host`) or after `mysql -p`, `password=`,
  `pwd=` or `secret:` assignments with a real-looking value (a quoted
  passphrase after `=` included), and long random tokens (32 or more
  characters mixing cases and digits, at high entropy). The refusal names
  the kind, never the text. The check (`internal/secretscan`) errs toward
  letting text through, so it is a guard against mistakes, not a
  guarantee: it misses a password that looks like a word.
- The installer never uses sudo and never edits your shell files.

## Releases

Each release's `checksums.txt` is signed with Ed25519. The public key is
built into `aries` and `ariesd` (`internal/release/keys.go`) and into
`install.sh`. `aries upgrade` and `ariesd upgrade` check the signature and
the archive's checksum before they install anything, and run the new
binary's `version` before swapping it in. `install.sh` checks the
checksum, and the signature when OpenSSL 3 is installed; without it, the
script warns, or fails with `--require-signature`. Every archive carries
GitHub build provenance as well.

The server installer, `install-server.sh`, holds no release key: it
installs `ariesd` and `aries` through `install.sh` with
`--require-signature`, and has apt trust the PostgreSQL project's
repository only after checking that its key has the fingerprint pinned
in the installer, which a test keeps equal to CI's. Run outside
a checkout, it downloads `install.sh` and the AppArmor profiles from the
repository over HTTPS, as the server guide's steps do, so it trusts that
source as far as the guide does. It puts no secret on a command line or
in its output: with Postgres on the machine there is no password at
all, and with `--dsn` the role's password is read from the file
`--db-password-file` names, written to a file of mode 0600, and never
shown ([With the installer](server.md#with-the-installer)).

Where a release is downloaded from is not trusted: GitHub, a static
file host or a mirror named by `ARIES_RELEASES`
([Where releases are read from](install.md#where-releases-are-read-from)).
Each is held to the same signature and checksum. From a static host no
redirect is followed, plain HTTP is refused except to a loopback
address, and the list of releases it serves is used only to choose a
version, which must be a version. What any host can do is withhold: by
serving an old list it keeps clients on the release they have, and
nothing they read proves the list is fresh. `aries` and `ariesd` never
install a release older than the one running.

The private key is a secret of the repository's `release` environment, so
only release runs the maintainer approves can use it.
[RELEASING.md](../RELEASING.md) has the details, and [Verify a download by
hand](install.md#verify-a-download-by-hand) the commands.

## Checks in CI

| Check | Runs |
|---|---|
| golangci-lint, with gosec, staticcheck, errcheck, govet and revive | Every pull request and push to `main` |
| `govulncheck`, against the Go vulnerability database | Every pull request and push, weekly on `main` (Mondays), and before every release build |
| `go mod download && go mod verify` | Every pull request and push, and before every release build |
| shellcheck, on every shell script in the repository | Every pull request and push, and before every release build |
| CodeQL, with the `security-extended` queries | Every pull request and push, and weekly |
| The Postgres pin against the PostgreSQL project's releases: a newer minor release of its major version, or a major version the store opens past its end of life | Weekly on `main` (Mondays) |
| A failed weekly run of `govulncheck` or of the Postgres check opens an issue, or comments on the one still open | Weekly on `main` (Mondays) |
| Dependabot, for Go modules, GitHub Actions and the Docker test server's images, each version proposed only 14 days after its release (30 for a major version); a security update does not wait | Weekly; updates grouped |

## Dependencies

The dependency list is kept small on purpose, and a new dependency needs a
stated reason. The largest is ariesd's alone: the OpenTelemetry Go SDK,
its OTLP/HTTP exporters for metrics, traces and logs, its Prometheus exporter
and what they bring, which one
package (`internal/telemetry`) imports. `aries`, which runs on every
developer's machine and inside every agent's harness, links none of it,
and a test fails if it ever does. Dependencies are not vendored. `go.sum` pins each module's
content hash, the Go checksum database rejects a module changed after
publication, and `go mod verify` checks that the local module cache has not
changed since download. Every GitHub Action is pinned by commit SHA.
