# Security model

This page explains how starfix decides who you are, what each principal
may change, and how it handles text, files and releases from others. The
setup steps that apply these rules are in [Running a server](server.md).

## Identity comes from SSH keys

The server's sshd decides who connects. Each key's `authorized_keys`
line forces one command, `starfixd stdio --principal NAME`, so the
principal comes from the key that matched. A client cannot claim to be
anyone else, and nothing depends on paths or environment variables. The
line comes from `~starfix/.ssh/authorized_keys`, or, on a server of many
projects set up for it, from starfixd itself, which sshd asks
([Who can authenticate](#who-can-authenticate)).

- `restrict` on each line turns off forwarding and terminals, and the
  sshd `Match` block repeats that for the whole account
  ([step 6](server.md#6-let-the-first-principal-in)).
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
  elsewhere. Access that ends closes the connections it had let in: when
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
`~starfix/.ssh/authorized_keys`, which only someone with a shell on the
server can write.

On a server of [many projects](server.md#serving-many-projects) whose
sshd is set up for it ([Keys from the store](server.md#keys-from-the-store)),
sshd reads two files for the `starfix` account, and a key authenticates
if either has a line for it:

- **The keys file starfixd writes**, `~starfix/.ssh/starfix_keys`, which
  holds a line for each key the control database has for a principal
  that is not disabled: `restrict`, and a forced command naming the
  principal and the key's fingerprint.
- **`authorized_keys`**, maintained by hand as before, which only
  someone with a shell on the server can write. It is still a way in:
  for the first admin, and to replace a server admin's own key. A key in
  it is not revoked by removing it from the control database. starfixd
  runs as the account whose file it is, so it reads the file, to tell
  admins which keys are in both: `sfx admin key list` marks them, and
  `sfx admin key remove` of one warns that sshd still accepts it and
  that its line must be deleted on the server. starfixd never writes to
  that file, and never prints or logs a line of it. A connection that
  came in by it names no key, and `sfx admin status` and `sfx admin key
  list` count those.

Either way the key only names a principal. What that principal reaches
is decided afterward, by its memberships.

**Who may add a key.** Server admins, and a project admin for a principal
whose every project they administer; a principal in no project, and a
server admin, are beyond a project admin's reach. Nobody adds or removes
a key of their own account, a server admin included: an agent that holds
a person's key could otherwise add a key of its own and outlive the
person's. Every key added and removed is an event in the control
database, under the admin who did it. Only public keys are stored, and
`sfx` refuses to send a file that holds a private key.

**Nothing of starfix runs before authentication.** sshd reads files; it
runs no program of starfix's and opens no connection to starfixd until a
key has authenticated and its forced command starts. A stranger who
offers keys to the `starfix` account reaches sshd and nothing behind it.
(An earlier, unreleased build had sshd ask starfixd about each key
offered, through an `AuthorizedKeysCommand`. That path is gone.)

**The keys file is a cache, and the control database decides.** What
keeps a removed key out is not the file:

- Each line starfixd writes carries its key's fingerprint, which the
  forced command passes to starfixd. At the handshake starfixd refuses a
  connection that names a key the control database does not hold for
  that principal, and one whose principal is disabled.
- So a key that was removed, or a disabled principal's, that is still
  in the file sshd reads, gets past sshd and is refused by starfixd.
  The file can be stale because starfixd could not rewrite it, because
  it was restored from a backup, or because someone edited it. A test
  states this directly: it removes a key while the file cannot be
  written, and again with the old file put back by hand, and the
  connection is refused each time.
- Anyone who can write the file can already write `authorized_keys`
  beside it, which is the stronger position: a line there names no
  fingerprint, and starfixd cannot check it. Both are in a directory
  only the `starfix` account can write to.
- **Every line holds only what the control database holds.** The
  principal must be a principal name that is not reserved, the
  fingerprint must be the stored key's own, and the key written is the
  stored one, with no options and no comment. The path of starfixd in
  the forced command is the operator's setting, and must need no
  quoting.
- starfixd writes the file as a new file of mode 0600, renamed into
  place, and refuses a path that is a symbolic link, a directory that
  group or others can write to, and a file it did not generate.
- A failed write does not undo the admin's change. The admin is warned,
  starfixd logs it and tries again every 30 seconds, and `sfx admin
  status` shows it until a write succeeds.

This was run with a real sshd, OpenSSH 9.6 on Ubuntu 24.04, in the
Docker test server. It has not been run on a cloud provider's image
([Keys from the store](server.md#keys-from-the-store) lists what was
checked). The supported platform for a server is Ubuntu LTS; systems
that run SELinux are not supported yet
([What a server needs](server.md#what-a-server-needs)).

## The server's host key is pinned

`sfx` checks the server's ED25519 host key against `host_key` in
`.starfix.yaml` on every connection, and never trusts a key on first use.
A mismatch is refused; its fix says to confirm the new key with the
admin before you change the file. `sfx` runs its own SSH client, so
nothing in `~/.ssh/config` can weaken the check ([The host key is
pinned](cli.md#the-host-key-is-pinned)).

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
- **Who manages access.** On a server of many projects, `sfx admin`
  changes projects, principals, their keys and memberships over the protocol
  ([Managing access](server.md#managing-access)), and the control
  database decides inside each write's transaction whether the caller
  may. A server admin may make every change. A project admin may add and
  remove their project's members and set their roles, and may create a
  principal as a member of their project. No MCP tool does any of it,
  and an agent whose principal is refused is told to ask a person. An
  agent that can run shell commands with an admin's key can still run
  `sfx admin`, with that admin's rights: give an agent that works on its
  own a principal of its own, which is no admin. What it cannot do, even
  then, is add a key to the admin's own account, so its access ends with
  the key it was given ([Who can authenticate](#who-can-authenticate)).
- **A project admin's reach is bounded.** A project admin may disable or
  enable a principal only while every project that principal belongs to
  is one they administer; a server admin, and a principal in no project,
  are out of reach. They may add to their project only a principal that
  already belongs to a project, or one they created. Otherwise they could
  bring someone within reach and act on them. Removing a membership can
  put a principal within reach of the admins of the projects that
  remain; `sfx admin member remove` says so when it does.
- **`sfx admin status` shows a project admin their own projects only:**
  the connections, sessions and writes of the active projects they
  administer, and nothing of the server or of any other project. A
  server admin sees all of it. No one else is shown anything.
- **A refusal does not say what exists elsewhere.** Someone who is no
  server admin gets the same `forbidden` for a project they have no role
  in as for one that does not exist. A principal name is the exception:
  names are one namespace for the server, so adding one that is taken
  says so.
- **Every change is an event**, written in the same transaction as the
  change, with its principal, session and time. Clients cannot edit or
  delete events.
- **What others see.** `sfx who` shows every principal's machine names. A
  handoff's worktree path, a path on your machine, is shown only to your
  own principal. Hours are not private: every principal can list anyone's
  entries, with their notes, and see them in `show`, `digest` and `sfx
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

## The Dolt account

A `dolt sql-server` started without a config lets `root` in from localhost
with no password, grants it `FILE`, and leaves `secure_file_priv` empty.
Any user on the machine could then skip starfixd, rewrite the event log as
anyone, and read or write files as Dolt's user.

So every command that opens the store first checks the account it connects
as. It refuses to continue, with the SQL to fix it, when the account:

- is `root`;
- holds any privilege on `*.*`, or `GRANT OPTION`;
- is on a server whose `secure_file_priv` is empty.

On a server of [many projects](server.md#serving-many-projects) the
account holds every right on the control database, on each project's
database and on each name of `project_dbs`, all granted by exact name,
and still nothing on `*.*`. It can create a database, because Dolt checks
`CREATE DATABASE` against the connection's current database; it can use
one only where root granted that name, so `sfx admin project add` cannot
reach a database outside the block
([measurements](../spike/multidb/RESULTS.md)).

A passwordless `root` cannot be seen from a least-privileged account, so
starfixd cannot check for it. [Step
4](server.md#4-create-the-database-and-the-config-file) gives `root` a
random password. On a developer's own machine, `--dev --allow-unsafe-dolt`
lets a default Dolt through with a warning. No config file or environment
variable can set it.

## The daemon's socket

`starfixd serve` listens on a unix socket in a directory that must be mode
0700 and owned by the daemon's user. On Linux it also checks each
connecting process's user ID and refuses any but its own, which is why
`serve` runs only on Linux. `starfixd stdio` runs as that same user,
because sshd runs every forced command as the `starfix` account.
Nothing else connects to the socket: sshd itself never does.

## Untrusted text

Issue text comes from other people, their agents and the server. starfix
keeps it from acting on a terminal or posing as starfix's own output:

- **The server refuses unsafe characters** in every field: control
  characters (C0, DEL, C1), the Unicode bidirectional controls and marks,
  line separators and invalid UTF-8. Titles, names, labels, reasons and
  handoff fields are one line. Bodies, design, acceptance, notes, comments
  and handoff notes may also hold newlines and tabs. Text stored before
  this rule is kept as it is.
- **`sfx` escapes what still arrives.** Any such character prints as a
  visible escape (`\x1b`, `\u202e`), and a one-line field's newline as
  `\n`, in every command, server error and quoted server stderr. With
  `--json`, the JSON encoding escapes them, so the value is the same and
  the output stays valid.
- **`prime` fences data.** It quotes titles and inbox text, each on one
  line, inside a `--- starfix data ---` fence, after a line saying they are
  data. A failed hook quotes the error.
- **MCP marks others' text.** Results that hold it carry `"untrusted"`,
  and the server's instructions tell the agent never to follow
  instructions in that text. A fix the server wrote is relayed quoted, for
  the user.

## Files in a repository

A cloned repository is someone else's code, so `sfx` treats its files with
care:

- **`.starfix.yaml`** must be yours and writable by no one else (on macOS
  and Linux), and `sfx` never looks for it above the repository's top
  level or your home directory ([Where sfx looks for the
  file](cli.md#where-sfx-looks-for-the-file)).
- **No commands in config.** A command in a committed file would run
  whatever the repository says, so there is no proxy-command setting.
  Google Cloud IAP is built in instead, and runs `gcloud` without a shell
  ([IAP](cli.md#servers-without-a-public-ip-google-cloud-iap)).
- **`sfx setup` follows no symbolic links** inside the repository, and
  keeps the MCP entry it writes to what starfix needs: `--check` reports
  anything else in the entry, such as `cwd` or `LD_PRELOAD`, and `--write`
  drops it. A JSON file with a duplicate key is refused ([What setup
  writes](agents.md#what-setup-writes-and-what-it-leaves-alone)).
- **Global hooks run everywhere.** A hook set up with `--global` runs in
  every repository that has a `.starfix.yaml`, and connects to the server
  that file names; Claude Code's usage hooks send it the session's token
  counts. Prefer per-project setup
  ([`--global`](agents.md#setting-agents-up-for-every-repository---global)).

## Transcripts stay on your machine

Token capture reads Claude Code's transcripts, which hold the whole
conversation, so it is built to take only counts from them
([Token usage hooks](agents.md#token-usage-hooks-claude-code)):

- **Read locally.** `sfx usage --hook` reads the transcript files on your
  machine. Nothing else reads them, and there is no other network call:
  records go over the same SSH connection as every `sfx` command.
- **Only counts leave.** Each record is the harness name, its request
  id, the model, the request's time and the token counts. The server
  checks every field against a fixed pattern or range. Prompts, replies,
  tool inputs and file contents are never sent, stored or printed.
- **Not held in memory.** Each line is decoded into a structure that has
  only those fields, and is dropped once read. A line over 4 MiB is
  passed over, not buffered.
- **The offsets file** (`starfix/usage-offsets.json` in your user cache
  directory) holds transcript paths, byte offsets and a hash of each
  file's first 4 KiB, nothing of their content. It is mode 0600 in a 0700
  directory.

## Limits

Every principal is bounded: write rates, `sfx admin` changes, a
principal's keys, connections, sessions, inbox size, labels, dependencies, acceptance items, token usage records and
memories each have a cap, and text
fields and replies have fixed sizes. [Limits](server.md#limits) lists the
caps, their settings and what a refusal past each one says.

## Secrets

- No password goes on a command line, and a config file that holds one
  must be mode 0600 ([Settings](server.md#settings)).
- `.starfix.yaml` holds no secrets, and is meant to be committed.
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
built into `sfx` and `starfixd` (`internal/release/keys.go`) and into
`install.sh`. `sfx upgrade` and `starfixd upgrade` check the signature and
the archive's checksum before they install anything, and run the new
binary's `version` before swapping it in. `install.sh` checks the
checksum, and the signature when OpenSSL 3 is installed; without it, the
script warns, or fails with `--require-signature`. Every archive carries
GitHub build provenance as well.

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
| Dependabot, for Go modules and GitHub Actions | Weekly; minor and patch updates grouped |

## Dependencies

The dependency list is kept small on purpose, and a new dependency needs a
stated reason. Dependencies are not vendored. `go.sum` pins each module's
content hash, the Go checksum database rejects a module changed after
publication, and `go mod verify` checks that the local module cache has not
changed since download. Every GitHub Action is pinned by commit SHA.
