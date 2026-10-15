# The database

**Aries's backend is moving from Dolt to Postgres**, and the port
removes Dolt before the next release. Postgres will hold one schema per
project, in one database, and run either from the PostgreSQL project's
own apt repository (PGDG) or as a managed service. This file will hold
the database steps when the port lands; until then it holds none, and
no server runs on Dolt.

Until then:

- **Do not improvise database steps**, for Dolt or for Postgres. Where a
  request needs one (creating a database for a project, a dump, a
  restore, a grant, the database's memory or disk), read the guide's
  current section at the release in question, tell the operator what it
  says and that it is about to change, and stop and ask before you run
  any of it. The current sections: docs/server.md#1-install-dolt,
  docs/server.md#2-run-dolt-under-its-own-account,
  docs/server.md#4-create-the-database-and-the-config-file,
  docs/server.md#opt-in (database names for new projects),
  docs/server.md#backups-and-restore, docs/server.md#storage-growth,
  docs/server.md#dolts-memory, docs/server.md#dolts-figures and
  docs/server.md#when-dolt-does-not-answer.
- **The rules hold whatever the database:** its passwords are generated
  on the server, into 0600 files, and travel on standard input, never in
  an argument (docs/server.md#4-create-the-database-and-the-config-file);
  ariesd's account has rights on Aries's databases and nothing else
  (docs/security-model.md#the-dolt-account); and you never print the
  config file's `dsn:` line (references/remote.md).
- **What is not specific to the database** stays in the other reference
  files: the status report, `aries admin`, `ariesd upgrade`'s check, and
  the workflow of a backup and a rehearsed restore.
