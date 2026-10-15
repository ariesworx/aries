# Backups and a rehearsed restore

What to back up, and how, is docs/server.md#backups-and-restore. The
database's own commands (the dump, the restore, the list of databases)
are in references/database-dolt.md; this file is the workflow around
them.

## What to back up

| What | How | Guide |
|---|---|---|
| Every database: on a server of many projects, the control database and each project's | A daily dump of each, on the server, carried off the machine every day | docs/server.md#backups-and-restore |
| `/etc/starfix/starfixd.yaml` | With the operator's other secrets, by their own tool. You never copy or print it | references/remote.md |
| `/etc/ssh/ssh_host_ed25519_key*` and `~starfix/.ssh/authorized_keys` | The same | docs/server.md#backups-and-restore |
| The disks | Snapshots, which also hold the database's own history | docs/deploy/team.md#choices-this-example-makes |

## Set up the daily dump

1. **Look**: list the databases (references/database-dolt.md), `ls -l
   /etc/cron.d/`, and `sudo -n test -d /var/backups/starfix; echo $?`.
2. **Show** the cron file you would write, one line per database, each
   the guide's line for that database's directory, and the directory
   it writes to. A project added later needs its own line: say so, and
   add it to the record's "left to do".
3. **With a yes**, make the directory and write the file
   (references/remote.md, "Write a file on the server").
4. **Run one dump now**, with a yes, and check its file is there and
   not empty (`sudo -n ls -l /var/backups/starfix`).
5. **Off the machine:** a dump on the server's own disk is lost with
   it. Agree with the operator where copies go (a bucket with a
   retention policy the machine cannot override, the backup tool they
   use), and set up only what they approve
   (docs/deploy/enterprise.md#build-it, step 7). Credentials for that
   destination are theirs to put in place.

## Restore

A restore replaces a database with a dump's rows and loses what changed
since. It is destructive: the operator names the database and the dump
file in their reply.

1. **Take a fresh dump first** of what is there now, and keep it apart
   from the daily rotation, so the restore itself can be undone.
2. **Stop starfixd**: `sudo -n systemctl stop starfixd`.
3. **Restore** with the database's commands (references/database-dolt.md).
4. **Start it**: `sudo -n systemctl start starfixd`. starfixd compares
   the clock with the restored history; a dump from a clock that ran
   ahead is refused, with the flag that starts it once
   (docs/server.md#when-the-clock-is-behind-the-servers-history).
5. **Verify**: `sudo -n -u starfix -H starfixd status`, and from the
   workstation `sfx -C REPO admin status` and `sfx -C REPO ready`.
6. **After restoring the control database,** access is as it was then:
   check `sfx -C REPO admin principal list` and the keys of anyone
   offboarded since, and redo what was undone
   (docs/server.md#opt-in, `--restore-removed-access`, only if the
   operator means the old `authorized_keys` to win).

On a new machine: install first (references/install.md), then put back
the config's `project`, the host keys and `authorized_keys` as the
guide says, then restore. A new host key means every `.starfix.yaml`
must change (docs/server.md#rotating-the-host-key).

## Rehearse it

The guide has not run a restore from a dump (docs/server.md#what-was-run-and-where,
"Not run"), so rehearse before you need it, and never on the production
server:

- On a scratch machine or VM the operator names, set up with the
  installer at the same release, or on the Docker test server
  (docs/deploy/docker.md), restore last night's dumps.
- Start it and compare with production, read-only: the projects and
  principals (`sfx admin project list`, `sfx admin principal list`), and
  in one project the counts of `sfx ready` and a few issues' `sfx show`.
- Time it, write the result and the date into the record, and tear the
  rehearsal down.
