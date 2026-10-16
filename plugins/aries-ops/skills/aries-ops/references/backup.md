# Backups and a rehearsed restore

What to back up, and how, is docs/server.md#backups-and-restore. This
file is the workflow around it; the database's commands, the dump and
the restore, are in references/database.md, from the guide's sections
docs/server.md#the-daily-dump and docs/server.md#restore.

## What to back up

| What | How | Guide |
|---|---|---|
| The database: the control store and every project, in one dump | A daily `pg_dump` of the whole database, by a systemd timer on the server, carried off the machine every day | docs/server.md#the-daily-dump |
| `/etc/aries/ariesd.yaml`, and `/etc/aries/postgres` where it exists | With the operator's other secrets, by their own tool. You never copy or print them | references/remote.md |
| `/etc/ssh/ssh_host_ed25519_key*` and `~aries/.ssh/authorized_keys` | The same | docs/server.md#backups-and-restore |
| The disks | Snapshots. Restore from a dump: a snapshot of a running Postgres holds its files as a crash leaves them | docs/deploy/team.md#choices-this-example-makes |

## Set up the daily dump

1. **Look**: `systemctl list-timers aries-backup.timer --no-pager`,
   `ls -l /etc/cron.d/`, and `sudo -n test -d /var/backups/aries; echo $?`.
2. **Show** the service and the timer you would write, the guide's text
   as it is, and where they write. One dump holds every project, those
   added later too.
3. **With a yes**, make the directory and write the files
   (references/remote.md, "Write a file on the server"), as
   references/database.md, "The daily dump", lists.
4. **Run one dump now**, with a yes, and check its file is there, mode
   0600 and not empty (`sudo -n ls -l /var/backups/aries`).
5. **Off the machine:** a dump on the server's own disk is lost with
   it. Agree with the operator where copies go (a bucket with a
   retention policy the machine cannot override, the backup tool they
   use), and set up only what they approve
   (docs/deploy/enterprise.md#build-it, step 7). Credentials for that
   destination are theirs to put in place.

## Restore

A restore replaces the database, or one project's schema, with a dump's
rows and loses what changed since. It is destructive: the operator
names the database or the project, and the dump file, in their
reply.

1. **Take a fresh dump first** of what is there now, and keep it apart
   from the daily rotation, so the restore itself can be undone.
2. **Stop ariesd**: `sudo -n systemctl stop ariesd`.
3. **Restore** as docs/server.md#restore says, each command shown first
   (references/database.md, "Restore"): the whole database, or one
   project's schema, made again before `pg_restore --schema`.
4. **Start it**: `sudo -n systemctl start ariesd`. ariesd compares
   the clock with the restored history; a dump from a clock that ran
   ahead is refused, with the flag that starts it once
   (docs/server.md#when-the-clock-is-behind-the-servers-history).
5. **Verify**: `sudo -n -u aries -H ariesd status`, and from the
   workstation `aries -C REPO admin status` and `aries -C REPO ready`.
6. **After restoring the control database,** access is as it was then:
   check `aries -C REPO admin principal list` and the keys of anyone
   offboarded since, and redo what was undone
   (docs/server.md#opt-in, `--restore-removed-access`, only if the
   operator means the old `authorized_keys` to win).

On a new machine: install first (references/install.md), then put back
the config's `project`, the host keys and `authorized_keys` as the
guide says, then restore. A new host key means every `.aries.yaml`
must change (docs/server.md#rotating-the-host-key).

## Rehearse it

The guide's restore was run on a test machine, not on a server like
the operator's (docs/server.md#restore), so rehearse before you need
it, and never on the production server:

- On a scratch machine or VM the operator names, set up with the
  installer at the same release, or on the Docker test server
  (docs/deploy/docker.md), restore last night's dumps.
- Start it and compare with production, read-only: the projects and
  principals (`aries admin project list`, `aries admin principal list`), and
  in one project the counts of `aries ready` and a few issues' `aries show`.
- Time it, write the result and the date into the record, and tear the
  rehearsal down.
