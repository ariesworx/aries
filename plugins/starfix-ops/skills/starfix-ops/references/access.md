# Projects, principals, keys and members

On a server of many projects, as the installer sets up, admins manage
access with `sfx admin`, from any repository connected to the server
(docs/server.md#managing-access; every command and who may run it). You
run them on the workstation, signed in as the operator, who must be a
server admin or the project's admin. A command acts on the repository's
project unless `--project` names another.

On a server of one project (no `control_db`), these commands are refused
(`this server holds one project`); access there is lines of
`authorized_keys` (docs/server.md#principals). Ask before you change
such a server, and prefer opting in to many projects
(docs/server.md#opt-in).

## Look first (read-only)

| What | Command |
|---|---|
| Who the operator is | `sfx -C REPO whoami` |
| Projects | `sfx -C REPO admin project list` |
| Principals | `sfx -C REPO admin principal list` |
| A project's members | `sfx -C REPO admin member list --project P` |
| A principal's keys | `sfx -C REPO admin key list NAME` |
| What someone added | `sfx -C REPO admin key list --added-by NAME`, `sfx -C REPO admin principal list --created-by NAME`, `sfx -C REPO admin member list --added-by NAME` |
| The server | `sfx -C REPO admin status` |

## Changes

Show each command and wait for a yes. Destructive ones (marked) need the
target named in the operator's reply, as SKILL.md's rule 5 says.

| To | Command | Note |
|---|---|---|
| Add a project | `sfx -C REPO admin project add NAME` | Prints the new project's `.starfix.yaml`. A failed run is finished by running it again. If its database name was never granted, the fix gives the grant for Dolt's root (references/database-dolt.md) |
| Add a person or an agent | `sfx -C REPO admin principal add NAME --key FILE --project P` | FILE is the `.pub` they sent (references/onboarding.md). `--no-project` adds them to none (server admins) |
| Add a key | `sfx -C REPO admin key add NAME FILE` | Never to the operator's own account: another admin adds those |
| Add someone to a project | `sfx -C REPO admin member add NAME --project P` | See "A new member's keys", below |
| Make a project admin | `sfx -C REPO admin member add NAME --project P --role admin` | |
| Remove a key (destructive) | `sfx -C REPO admin key remove FINGERPRINT` | Closes the connections it signed in. List the keys first, and confirm the fingerprint |
| Remove from a project (destructive) | `sfx -C REPO admin member remove NAME --project P` | Ends their claims there |
| Disable a principal (destructive) | `sfx -C REPO admin principal disable NAME` | Everywhere, at once; ends every claim. Undo with `sfx -C REPO admin principal enable NAME` |
| Archive a project (destructive) | `sfx -C REPO admin project archive PROJECT` | Keeps its data; `sfx -C REPO admin project activate PROJECT` serves it again. There is no command that deletes a project |

Run each with `--json` when you need to read the result, and report what
it printed: the counts of claims released, and every warning on stderr.

**A new member's keys.** A key signs in to every project its principal
is in. `member add` of someone with keys adds nothing at first: without
a terminal it lists their keys, with who added each and whether that
was an admin here, and exits 1 naming `--accept-keys`. Show that list
to the operator. Only after they have checked each key marked `no` with
its owner and said yes, run it again with `--accept-keys`. Never add
`--accept-keys` on your own.

**`authorized_keys` still lets in a key it holds.** If `key remove`
warns that the key is also a line of `authorized_keys`, sshd still
accepts it by that line until the line is deleted on the server
(docs/server.md#keys-from-the-store, step 5). Deleting that line is a
root change: show the line by its fingerprint (`sudo -n ssh-keygen -lf
/var/lib/starfix/.ssh/authorized_keys`), never the first admin's, and
ask.

**A second server admin** is a name under `admins:` in the config file,
read at start (docs/server.md#admins). That file holds the database
password, so the operator edits it themselves (`sudoedit
/etc/starfix/starfixd.yaml`); then check it with `sudo -n grep -vi dsn
/etc/starfix/starfixd.yaml` and `stat`, and restart starfixd with
approval. Name only admins who can sign in.

## Offboarding

Destructive: the operator names the person in their reply. Then, in
order:

1. **Record what they hold.** `sfx -C REPO admin key list NAME`, `sfx -C
   REPO admin project list`, and for each of their projects `sfx -C REPO
   admin member list --project P`. Put the fingerprints and projects in
   the record.
2. **Disable the principal**: `sfx -C REPO admin principal disable NAME`.
   Their connections close and their claims end, in every project; the
   command says how many. A last server admin cannot be disabled.
3. **Remove their keys**, one `sfx -C REPO admin key remove FINGERPRINT`
   each, confirming each fingerprint. Act on every warning about
   `authorized_keys`, as above.
4. **Review what they did to others.** Disabling undoes nothing:
   `sfx -C REPO admin key list --added-by NAME`, `sfx -C REPO admin
   principal list --created-by NAME`, `sfx -C REPO admin member list
   --added-by NAME`. Show the lists; remove only what the operator names.
5. **Outside starfix,** say what is theirs to do: the network path (an
   IAP group, a VPN account), a CI secret holding the person's or an
   agent's key, and their cloud access
   (docs/deploy/enterprise.md#day-to-day).
6. **Verify**: `sfx -C REPO admin status` (no connection of theirs), and
   `sfx -C REPO admin key list NAME`.
