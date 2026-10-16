# The database

Aries keeps its data in Postgres 16 or newer: one database, `aries`,
holding a schema for the control store (`aries_control`) and one for
each project (`project_NAME`; the first project is in `public`), and
one role, `aries`, which owns that database and has no other right.
On a server the installer set up, Postgres runs on the machine and
ariesd signs in by its unix socket with no password; with `--dsn` it is
on another machine or a managed service, and the role's password is in
a file. The installer makes all of it (references/install.md). This file
is what to do with it after, and the rules for touching it. Each step
names the section of the guides it follows.

## Rules

- **As the `postgres` user, on the server.** Administer Postgres the
  guide's way: `sudo -n -u postgres psql`, by the unix socket. Never
  give ariesd's role more rights to make something work: the store
  refuses a role with too many (docs/security-model.md#the-database-role),
  and the refusal is the safety, not the fault.
- **No password passes through you.** On the machine there is none.
  With a Postgres elsewhere, the role's password is in
  `/etc/aries/postgres/pgpass` (references/remote.md, "Never printed").
  A password is set by its owner in a terminal of their own: the
  administrator runs `\password aries` themselves, and the operator
  writes the file the installer's `--db-password-file` reads. You show
  the statements; you do not run one that holds a password.
- **Destructive, so the target named and a dump first**: `DROP` of
  anything, `dropdb`, a restore, `ALTER ROLE`, `pg_upgradecluster`, and
  `VACUUM FULL`, which locks tables (SKILL.md, rule 5).
- **A schema ariesd did not make is not yours to change**, and ariesd
  never drops a project's schema: an archived project keeps its own
  (docs/server.md#managing-access). The guide's one-project restore
  drops and remakes one schema, with ariesd stopped
  (docs/server.md#restore).

## Look (read-only)

| What | Command, on the server | Expect | Guide |
|---|---|---|---|
| The cluster | `pg_lsclusters` | `18 main 5432 online` (with Postgres on the machine) | docs/server.md#1-install-postgres |
| The units | `systemctl is-active postgresql@18-main ariesd` | `active` twice | docs/server.md#5-run-ariesd-under-systemd |
| The role | `sudo -n -u postgres psql -Atc "SELECT rolsuper, rolcreaterole, rolcreatedb, rolreplication, rolbypassrls FROM pg_roles WHERE rolname = 'aries'"` | `f\|f\|f\|f\|f` | docs/security-model.md#the-database-role |
| The database | `sudo -n -u postgres psql -c '\l aries'` | Owner `aries`, `C` in `Collate` and `Ctype` | docs/server.md#2-create-the-role-and-the-database |
| Its schemas | `sudo -n -u postgres psql --dbname=aries -c '\dn'` | `aries_control`, `public` and a `project_…` for each added project, all owned by `aries` | docs/server.md#opt-in |
| Connections | `sudo -n -u postgres psql -c "SELECT application_name, state, count(*) FROM pg_stat_activity WHERE datname = 'aries' GROUP BY 1, 2"` and `sudo -n -u postgres psql -Atc 'SHOW max_connections'` | ariesd's pool and its one connection for the writers' locks, well under `max_connections` | docs/server.md#sizing-postgres |
| What ariesd sees of it | `sudo -n -u aries -H ariesd status`, or `aries -C REPO admin status` from the workstation | The `postgres` lines, and each project's `SIZE` | docs/server.md#watching-a-server |
| Which kind of DSN, without printing it | `sudo -n grep -c 'host=/var/run/postgresql' /etc/aries/ariesd.yaml` (1 on the machine) and `sudo -n grep -c 'passfile=' /etc/aries/ariesd.yaml` (1 elsewhere) | One of the two | docs/server.md#managed-services |

## The daily dump

Set it up as docs/server.md#the-daily-dump says: the service and the
timer, `/var/backups/aries`, one run now, its file checked, and the
copies off the machine (references/backup.md has the workflow).

1. **Look**: `systemctl list-timers aries-backup.timer --no-pager`, and
   `sudo -n ls -l /var/backups/aries`.
2. **Write** `/etc/systemd/system/aries-backup.service` and
   `aries-backup.timer` with the guide's text, each shown whole
   (references/remote.md, "Write a file on the server"); then, each
   with a yes: `sudo -n install -d -o postgres -g postgres -m 0700
   /var/backups/aries`, `sudo -n systemctl daemon-reload`, `sudo -n
   systemctl enable --now aries-backup.timer`, `sudo -n systemctl start
   aries-backup.service`.
3. **Check**: `sudo -n ls -l /var/backups/aries` shows today's file,
   mode 0600, owned by `postgres`, not empty; `journalctl -u
   aries-backup.service -n 5 --no-pager` ends `Finished`.
4. **With a Postgres elsewhere**, the dump runs from the server as
   ariesd's role, with the DSN of its config file, and only when that
   DSN holds no password, so that none is on a command line. Check:
   `sudo -n grep -cE '^dsn: postgres(ql)?://[^/@:]*:[^/@]*@|password=' /etc/aries/ariesd.yaml`
   must print 0 (the installer's DSN never holds one). The guide's
   section has the command. The service's own snapshots and
   point-in-time restore are its console's (docs/server.md#point-in-time-recovery).

## Restore

The workflow, and what to tell the operator, is references/backup.md.
The commands are docs/server.md#restore, each shown and run with a yes:
the whole database (`dropdb`, then `pg_restore --create`), or one
project (its schema dropped and made again, then `pg_restore
--schema`), always with ariesd stopped. After it: `sudo -n -u aries -H
ariesd status`, and `aries -C REPO admin status` and `aries -C REPO
ready` from the workstation.

## Sizing and settings

docs/server.md#sizing-postgres: the package's defaults serve a team.
A change to Postgres's settings is a change to the machine:

1. **Look**: `sudo -n -u postgres psql -Atc 'SHOW max_connections'`,
   and the status report's connections against it.
2. **Show** the statement, such as `sudo -n -u postgres psql -c 'ALTER
   SYSTEM SET max_connections = 200'`, and that it needs a restart of
   the cluster, which ariesd outlives (docs/server.md#5-run-ariesd-under-systemd);
   requests fail with `unavailable` for those seconds.
3. **With a yes**, run it, then `sudo -n systemctl restart
   postgresql@18-main`, and check `aries -C REPO admin status`.

`db_conns_per_server` and `db_conns_per_project` are ariesd's settings,
under `limits:` (docs/server.md#limits); keep `max_connections` above
the first.

## Postgres's own upgrades

docs/server.md#upgrades ("Postgres's own upgrades"). A minor release
comes with `apt-get upgrade`, which restarts the cluster: show it, run
it with a yes, check the status report after. A new major version is
`pg_upgradecluster` with ariesd stopped and after a dump; the guide
gives no tested steps for it, so stop and agree each one with the
operator.

## A Postgres elsewhere: managed services

docs/server.md#managed-services. Before the installer:

- **The administrator makes the role and the database**, with the
  guide's statements, in a terminal of their own, since `\password`
  asks for the password there. You show the statements and nothing
  else. Where the role may not own a database, the guide's step 4 makes
  the two schemas beforehand, and `aries admin project add` will be
  refused until the role may make schemas: say so before the operator
  chooses it.
- **The operator writes the password file** for `--db-password-file`,
  in a terminal of their own, mode 0600: `umask 077 && cat > db.pass`,
  typed, then Ctrl-D. You never see or move it but by the installer.
- **TLS**: `sslmode=verify-full` in the DSN, and the service's
  certificate authority in `/etc/aries/postgres`, named by
  `sslrootcert=`. The file is public; write it as any other file.
- **The installer** then runs with `--dsn URL --db-password-file FILE`
  (references/install.md). It refuses a URL that holds a password.

## Standby and failover

docs/server.md#standby-and-failover describes a standby with streaming
replication, Patroni or a managed service's high availability, and what
ariesd does when the primary moves. The guide gives no steps for
setting one up, and none was run: help the operator read the section,
and stop before any change to replication, which is theirs or their
database administrator's.

## Troubleshooting

| Seen | First read | Guide |
|---|---|---|
| `postgres: unknown`, or `SESSIONS` `unknown`, in the status report | `systemctl status postgresql@18-main --no-pager`, `journalctl -u postgresql@18-main -n 50 --no-pager` | docs/server.md#when-postgres-does-not-answer |
| `/readyz` says `the last check of the database failed` | The same | docs/server.md#the-listener-metrics-to-scrape-and-health |
| ariesd refuses its role, or the server's version | The refusal's `fix:` names the SQL; the role check above | docs/server.md#troubleshooting |
| `Peer authentication failed`, `password authentication failed`, `too many clients` | The guide's row for each | docs/server.md#troubleshooting |
| `aries admin project add` refused with `GRANT CREATE ON DATABASE` | `sudo -n -u postgres psql -c '\l aries'`: who owns the database | docs/server.md#managed-services |
