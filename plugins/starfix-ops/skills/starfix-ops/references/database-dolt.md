# The database: Dolt

**This file is the only one specific to the database, and it is
temporary.** On 15 October 2026 the maintainer decided to move
starfix's backend from Dolt to Postgres: a schema per project in one
database, with managed services such as Amazon RDS, Cloud SQL, AlloyDB
and Azure's supported. When that port lands in a release, this file is
replaced by one for Postgres, which adds connections to a managed
database. Until a release says so, every server runs Dolt, as below.
Do not improvise Postgres steps.

## How Dolt is set up

The installer does all of this (docs/server.md#with-the-installer); the
sections are the reference.

| What | Where | Guide |
|---|---|---|
| The release | The one starfix pins, checked against a SHA-256: `dolt version` | docs/server.md#1-install-dolt |
| Its own user | `dolt`, data in `/var/lib/dolt/data` (one directory per database), accounts in `/var/lib/dolt/cfg`, on loopback `127.0.0.1:3306` | docs/server.md#2-run-dolt-under-its-own-account |
| starfixd's account | `starfix`@`localhost`, with rights on starfix's databases by exact name and nothing else; the password only in `/etc/starfix/starfixd.yaml`, mode 0600 | docs/server.md#4-create-the-database-and-the-config-file, docs/security-model.md#the-dolt-account |
| Root | A generated password nobody keeps; Dolt's own user administers it from a database's directory | docs/server.md#4-create-the-database-and-the-config-file |
| The databases | `starfix` (the first project), `starfix_control`, and `starfix_p001` to `starfix_p100` as projects are added | docs/server.md#opt-in |
| Its metrics | `http://127.0.0.1:11228/metrics`, loopback only, no authentication | docs/server.md#dolts-figures |

**Passwords.** Every password is generated on the server and travels on
standard input, never in an argument (the installer, and step 4 of the
guide). You never generate, print or read one. Dolt's own user reaches
the server from a database's directory with no password, which is how
every command below runs.

## Commands

Each runs on the server as root through `sudo -n`, and as Dolt's user
from a directory it can read (references/remote.md). Read-only ones run
freely; the rest need a yes.

| To | Command | Kind |
|---|---|---|
| List the databases | `sudo -n ls /var/lib/dolt/data` | Read |
| Their size | `sudo -n du -sh /var/lib/dolt/data/*` | Read |
| Dolt answers its metrics | `curl -s http://127.0.0.1:11228/metrics \| grep -c '^dss_'` (14 at Dolt 2.4.2) | Read |
| The latest event of a database, against the clock | `cd /var/lib/dolt/data && sudo -n -u dolt -H dolt sql -q "SELECT MAX(at) FROM starfix.events"` and `date -u` | Read (docs/server.md#upgrades) |
| Grant a database name for a new project | `cd /var/lib/dolt/data/starfix && sudo -n -u dolt -H dolt sql -q "GRANT ALL ON starfix_p101.* TO 'starfix'@'localhost';"`, then raise `project_dbs: count` | Change (docs/server.md#opt-in, step 4) |
| Make the dump directory | `sudo -n install -d -o dolt -g dolt -m 0700 /var/backups/starfix` | Change |
| Dump one database now | `cd /var/lib/dolt/data/DB && sudo -n -u dolt -H dolt dump -f -r sql -fn /var/backups/starfix/DB-manual.sql` | Change (writes a file) |
| Collect garbage, in a quiet hour | `cd /var/lib/dolt/data/DB && sudo -n -u dolt -H dolt gc` | Change (docs/server.md#storage-growth) |
| Restart Dolt | `sudo -n systemctl restart dolt`, which restarts starfixd too and drops every connection for a few seconds | Change (docs/server.md#dolts-memory) |

## The daily dump

The guide's cron line, `/etc/cron.d/starfix-backup`, dumps the `starfix`
database. On a server of many projects write one line for each database
directory, the same line with that directory and that name
(docs/server.md#backups-and-restore):

```text
15 3 * * * root cd /var/lib/dolt/data/starfix && sudo -u dolt -H dolt dump -f -r sql -fn /var/backups/starfix/starfix-$(date -u +\%a).sql
15 3 * * * root cd /var/lib/dolt/data/starfix_control && sudo -u dolt -H dolt dump -f -r sql -fn /var/backups/starfix/starfix_control-$(date -u +\%a).sql
```

It keeps a week, one file per weekday. A dump holds a database's rows,
the event log included, and not Dolt's commit history, which disk
snapshots keep.

## Restore one database

As a script on stdin, shown whole, after the steps of
references/backup.md (a fresh dump first, starfixd stopped):

```sh
set -e
cd /var/lib/dolt/data
sudo -u dolt -H dolt sql -q "DROP DATABASE DB"
sudo -u dolt -H dolt sql < /var/backups/starfix/DB-Mon.sql
```

The dump recreates the database it came from. That the account's grants
on the name still hold after the drop has not been checked for this
skill: starfixd checks its account at start and refuses with a fix if
not.

## When Dolt is the trouble

| Seen | Read | Guide |
|---|---|---|
| `Dolt: unknown` in the status report | `curl -s http://127.0.0.1:11228/metrics \| head -3`, `journalctl -u dolt -n 50 --no-pager` | docs/server.md#dolts-figures |
| `SESSIONS unknown`, or `the server's database does not answer` | `systemctl status dolt --no-pager`, `journalctl -u dolt -n 50 --no-pager` | docs/server.md#when-dolt-does-not-answer |
| `Dolt's collector: … OVER 14` | The `Dolt` lines of the report: resident memory against its limit | docs/server.md#dolts-memory |
| starfixd refuses the Dolt account | The refusal's fix names the SQL; show it, and run it only with a yes | docs/server.md#troubleshooting |
| The disk fills | `du` above; `dolt gc` in a quiet hour | docs/server.md#storage-growth |

Raising `GOMEMLIMIT` for Dolt is a change to its unit and a restart
(docs/server.md#dolts-memory): show both.

## Upgrades and Dolt

`starfixd upgrade` commits each database and tags it `starfix-<old
version>` before it swaps the binary (docs/server.md#upgrades). Dolt's
own release changes only with a starfix release that names a new one
(docs/server.md#upgrades, last paragraph); its notes say how.
