# starfix stage 3a: many databases in one Dolt

**Verdict: one Dolt server can hold a database per project.** A database costs no memory to speak of by existing or by being open; Dolt's memory follows how much data has been written across all of them, and closing a project's store gives none of it back. A project's database can be created by starfixd's own least-privileged account, provided root granted it that database's name beforehand.

11 October 2026. Code: this directory (`go run ./spike/multidb [-projects 1,5,20,50] [-issues 300]`, with `dolt` at `version.Dolt` on `PATH`). It answers open questions 1 and 2 of [the design, §15.5](../../docs/design/starfix.md#155-open-questions).

## Environment

| | |
|---|---|
| Dolt | 2.4.2, started by `internal/dolttest`: loopback and a unix socket, `secure_file_priv` set, default settings otherwise, fresh data directory |
| Account | `starfix@localhost`, as `dolttest` makes it: every right on the databases it is granted, none on `*.*` |
| Machine | Apple M4 Max, 16 cores, 64 GB RAM, macOS 27; client and server on the same machine, over the socket |
| Client | Go 1.27.1, `internal/store` as starfixd uses it (`store.Open` with default options: one writer, a commit a second) |
| Memory | Resident set size of the `dolt sql-server` process, from `ps`, read 5 seconds after each step |

The grants were also run at Dolt 2.3.3, with the same results.

## 1. Who can create a database

`CREATE DATABASE IF NOT EXISTS name` as the least-privileged account, on a connection whose current database is one it has every right on:

| Grant from root beforehand | Create | The account can then use it |
|---|---|---|
| None | allowed | no |
| `GRANT ALL ON` a name pattern (`` `sfx\_%`.* `` or `` `sfx_%`.* ``) | allowed | no |
| `GRANT ALL ON name.*`, the exact name, before the database exists | allowed | **yes** |
| None, on a connection with no current database | refused | |
| Root creates the database and grants it, run twice | reruns without error | yes |

After all of it, the account still holds nothing on `*.*` (`information_schema.USER_PRIVILEGES` is empty), so `store.Open`'s account check (S-2) still passes.

- **Dolt checks `CREATE DATABASE` against the connection's current database**, not against the database being created. An account with `CREATE` on any one database can create others; it cannot use them without a grant.
- **A grant on a name pattern does nothing**, to create or to use.
- **A grant on an exact name can be given before the database exists.** So root can grant starfixd's account a set of names once, and starfixd can create those databases later, itself, as projects are added.
- **For today's single-project server:** starfixd's account can create empty databases it cannot use. No client can make starfixd run SQL of its choosing, so nothing reaches this, but the account is not as confined as the server guide says.

## 2. Memory

Each store was loaded with 300 issues, each with a 540-byte body, a comment of the same size, a label and a claim: four writes an issue, 1,200 a store. Stores were opened and loaded one after another and all left open.

| Open stores | Dolt memory | Connections |
|---|---|---|
| 0, server started | 86 MiB | 0 |
| 1 | 336 MiB | 1 |
| 5 | 1,072 MiB | 5 |
| 20 | 2,540 MiB | 20 |
| 50 | 5,117 MiB | 50 |
| 50, idle for 30 seconds | 5,118 MiB | 50 |
| 1, the other 49 closed, a minute later | 5,118 MiB | 1 |
| 50, reopened | 5,118 MiB | 50 |

Two more runs say what that memory is:

| Run | Dolt memory | A write |
|---|---|---|
| 5 stores of 300 issues (above) | 1,072 MiB | 7 ms |
| 1 store of 1,500 issues, the same writes in one database | 917 MiB | 6 ms |
| 20 stores of 300 issues (above) | 2,540 MiB | 7 ms |
| 20 stores of 300 issues, Dolt started with `GOMEMLIMIT=512MiB` | 1,140 MiB | 30 ms |

- **Memory follows the data written, not the number of databases.** The same writes cost about the same in one database as in five. A database that is merely open, or merely exists, adds nothing measurable.
- **Closing stores returns nothing.** Dolt kept all of it with one store open. An idle close in starfixd saves connections, not memory.
- **About half is garbage the Go runtime had not collected.** A memory limit halved the figure, but Dolt went past the limit, so the rest is in use, and writes took four times as long while the collector worked to hold it. A limit set well above what is in use should cost less; that was not measured.
- **It is a lot of memory for the data:** about 100 MiB for a project of 300 issues without a limit, 50 MiB with one. That is Dolt's, and a single-project server pays it too (the load test before this one reached 1.4 GB).
- **Opening is cheap.** The first open of a new database, running all 15 migrations, took 220 to 250 ms; reopening one already migrated, 3 ms.
- **An idle store holds one connection.** The reader pool fills only under load.
- **Writes did not slow as stores were added**: 7 ms each with 1 open or 50.

## Not measured

- Memory after Dolt restarts on the same data, which is what a server settles to between restarts if most of the above is cache.
- Projects written to at the same time. Stores were loaded one after another, so nothing here says how writers in different databases contend for the CPU and the disk.
- Linux. The server runs there; these figures are from macOS.
- Larger projects, and whether memory keeps growing in step with the data.

## What it means for the design

1. `sfx admin project add` creates a project's database from anywhere, from a block of database names the operator grants starfixd's account at setup (decision 16). A command the operator runs on the server for each project would also work, and is not needed.
2. Size the machine by the data in all its projects together, not by their number. Closing idle stores is worth doing for connections alone.
3. Whether to run Dolt under a memory limit, and what a restart gives back, decide the machine size and are still to be measured, on Linux.
