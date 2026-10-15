# aries: Postgres spike results

**Verdict: switch to Postgres, and only to Postgres, before stage 6 adds more migrations.** The store ports with little change: its whole test suite runs on Postgres 18 with about 130 lines of the store changed (in 10 files, besides the new connector) and the migrations translated mechanically, and every test that fails there tests Dolt itself or Dolt's optimistic concurrency. On the same machine, with the same durability, Postgres makes 2.5 times as many claims a second as Dolt with one claimer in raw SQL and up to 11 times with twenty (1.3 times in the one autocommit run), 4.6 times as many through the store as ariesd runs it, 2.3 to 6 times the writes of stage 0's mix from 8 writers up (though a third of Dolt's with a single writer, section 3.2), answers the ready, blocked and digest queries 2.7 to 3.9 times faster at 10,000 issues, and runs the store's tests in a quarter of the time. It needs about a tenth of Dolt's memory per project (13 MiB against 120) and gives it back when a project closes. Writes through the store come out even, since one writer per project waits for one flush per write on either (section 3.3). Its replication carries roles and grants, where Dolt's does not, and buying a standby with failover is ordinary. The cost is not the store; it is the installer, the AppArmor profile, the Docker server and the server guide, about 7 to 9 weeks in all (section 5). Keep one backend, put an interface in front of the store for the server's sake, and move existing servers once with a copy (`ariesd migrate-from-dolt`), not by supporting both (section 6). A MySQL-family database (MySQL 8.4 with InnoDB) would be the cheaper port, since the store's SQL is already MySQL's and it has row locks and `SKIP LOCKED`, but it has neither transactional DDL nor `LISTEN/NOTIFY`, and was not measured here.

Two things in the store are worth fixing on main whatever is decided (section 2): `Blocked` and the digest pick the ancestor an issue is blocked through by the order Dolt happens to emit rows, and one recursive query in gates.go is outside the SQL standard.

15 October 2026. Code: this directory, a Go module of its own (`go.mod`, so the main module and its gate are untouched):

| Path | What |
|---|---|
| `store/` | A copy of `internal/store` at `5d6e951` that runs on Dolt or Postgres, chosen by the DSN (`postgres://` opens pgx). What differs from `internal/store` is below (section 2) |
| `store/pgmigrations`, `store/pgcontrolmigrations` | Both migration sets in Postgres's dialect, translated mechanically and reviewed |
| `store/pgdialect.go` | The Postgres connector: a `driver.Connector` over pgx that rewrites each statement from the store's dialect as it is sent, error codes, the account check |
| `store/spike_race_test.go` | The races of `cas_test.go`, `work_test.go` and `memory_test.go` without their barrier, over four stores on one database |
| `pgtest/` | Starts the pinned Postgres for tests and measurements as `internal/dolttest` starts Dolt: a temporary data directory, loopback, a superuser only it uses, an `aries` role that owns a database (or schema) per test, and dolttest's watcher, which kills the server and removes its files if the process that started it dies |
| `install.sh` | Installs the pinned Postgres under `~/.cache/aries/postgres-18.6`, checking the SHA-256 of the download and of the archive inside it |
| `race/` | The measurements: stage 0's claim race and write mix in raw SQL on both backends, the same through the store, and memory per project |

To rerun: `spike/postgres/install.sh`, put Dolt 2.4.2 on `PATH` (`scripts/install-dolt.sh`), then from `spike/postgres`:

```sh
ARIES_BACKEND=postgres go test ./store          # or postgres-schema, or dolt (the default)
go run ./race -backend postgres claims -workers 20 -pick 16
go test ./store -run '^$' -bench 'Scale/issues=10000/' -benchtime 20x   # with ARIES_BACKEND
```

## Environment

| | |
|---|---|
| Machine | Apple M4 Max, 16 cores, 64 GB, macOS 27; client and server on the same machine. The same Mac as stage 3a's macOS section, not stage 0's 4 vCPU Linux VM, so stage 0's figures are not comparable with these: Dolt is re-measured here beside Postgres |
| Dolt | 2.4.2 (the pin), started by `internal/dolttest`: unix socket, default settings |
| Postgres | 18.6 (zonky.io's build of the release, from Maven Central, SHA-256 pinned in `install.sh`), started by `pgtest`: loopback TCP, `max_connections = 1000`, otherwise defaults. For the measurements `wal_sync_method = fsync_writethrough` (below) |
| Durability | Go's `File.Sync`, and so Dolt's, is `F_FULLFSYNC` on macOS, a flush of the drive's cache taking several milliseconds. Postgres's default on macOS does not flush the drive's cache and is about 30 times faster at a single writer (rows marked *default sync* in section 3); `fsync_writethrough` is the like-for-like setting and every unmarked Postgres row uses it. On Linux both use `fdatasync`, and the gap between the two settings would mostly disappear. The store's tests run Postgres with `fsync` off and Dolt at its defaults, so test times compare test suites, not durable writes |
| Client | Go 1.27.2, go-sql-driver/mysql v1.10.1, pgx v5.11.0 through `database/sql` |
| Load | Another session ran the repository's gate (with Dolt servers of its own) on the same machine during some runs. Each run records the load average; it ran from 4 to 11 against 16 cores. Treat differences under about 20% as noise |

## 1. Inventory: what in Aries is Dolt's or MySQL's

The store's SQL is far more portable than the review assumed. Of about 230 statements, all use `?` placeholders (Postgres numbers them), and fewer than 30 use anything else that differs:

| Where | What is Dolt's or MySQL's | Count | Port |
|---|---|---|---|
| `internal/store/store.go` | `mysql.ParseDSN`/`NewConnector`; the committer (`CALL DOLT_COMMIT` once a second), `Flush`, the dirty flag | 1 driver, 1 statement | Small: a pgx connector; the committer is deleted |
| `errors.go` | MySQL error numbers 1213, 1205, 1105 ("transaction sequencing"), 1062 | 4 | Small: 40001, 40P01, 23505 (section 2 says why 23505 must retry) |
| `account.go` | `CURRENT_USER()`, `@@secure_file_priv`, `information_schema.USER_PRIVILEGES` and `SCHEMA_PRIVILEGES`, the `GRANT` in its fix | 4 | Small: `pg_roles` (superuser, create role, create database), 25 lines in the spike |
| `migrate.go` | `information_schema.tables WHERE table_schema = DATABASE()` | 2 | Trivial: `current_schema()`. Postgres's DDL is transactional, so each migration could commit with its version row |
| `backup.go` | `DOLT_COMMIT`, `HASHOF('HEAD')`, `dolt_tags`, `DOLT_TAG` | 4 | No equivalent: a `pg_dump` before upgrade (section 4.4) |
| `control_admin.go` | `CREATE DATABASE IF NOT EXISTS`; a cross-database count (`` `db`.claims ``); `CURRENT_USER()`; Dolt's "access denied" (1105/1044) | 4 | Medium: databases cannot be queried across in Postgres; with a schema per project it is a schema-qualified name |
| `digest.go` | `JSON_UNQUOTE(JSON_EXTRACT(e.after_state, '$.k'))` | 4 | Trivial: `after_state->>'k'` |
| `issues.go`, `import.go`, `notes.go`, `usage.go` | `INSERT IGNORE` | 4 | Trivial: `ON CONFLICT DO NOTHING` |
| `graph.go`, `gates.go` | `WITH RECURSIVE` with `CAST(? AS CHAR(64))` (Postgres pads `CHAR`, and the two terms' types differ); one CTE with two recursive terms, which Postgres refuses | 3 + 1 | Small: `VARCHAR`; one query rewritten portably |
| `inbox.go`, `limits.go` | `UPDATE`/`DELETE … ORDER BY … LIMIT` | 2 | Small: a subquery, portable |
| `notes.go` | `SUM(label = ?)`, a boolean summed | 1 | Trivial: `SUM(CASE …)`, portable |
| `paths.go` | `CASE path WHEN ? THEN ? END` setting a time: Postgres types the placeholders as text | 1 | Trivial: a cast |
| `control_keys.go` | `` `key` `` quoted with backquotes | 2 | Trivial |
| `similar.go` | FULLTEXT was planned and dropped (Dolt's `MATCH` was unreliable); similar titles are scored in Go | 0 | None; Postgres's `pg_trgm` could do it in SQL |
| `doc.go`, rule 6 | Every `UPDATE` sets `write_id` because Dolt detects conflicts per cell | All `UPDATE`s | None needed; harmless on Postgres and unnecessary (section 3.1) |
| Migrations, both sets | `DATETIME(6)`, `TINYINT`, `KEY` inside `CREATE TABLE`, `COLLATE utf8mb4_0900_bin`, backquotes, `DROP INDEX … ON` | 19 files, 570 lines | Mechanical: a 15-line script translated them; reviewed by hand |
| `internal/dolttest` | Starts Dolt, grants databases by name, the pool | 1,100 lines | Replace: `pgtest` here is 330 lines |
| `internal/server` | `mysql.ParseDSN` to derive a project's DSN and refuse a password in it (`project.go`, `settings.go`); the `GRANT` in a fix (`admin.go`); `doltwatch.go` and `internal/doltmetrics` (Dolt's Prometheus figures in the status report) | 4 sites, 600 lines | Medium: the DSN logic moves behind the store; Dolt's figures become `pg_stat_*` |
| `cmd/ariesd` | `mysql.ParseDSN`, `GRANT` text (`control.go`), `dolt_metrics_url` | 3 | Small |
| Operations | `install-server.sh` (1,002 lines, 100 mentions of Dolt), `deploy/docker` (Dockerfile, entrypoint, `smoke.sh`), `deploy/apparmor`, `docs/server.md` (3,946 lines, 304 mentions), `docs/deploy`, `scripts/install-dolt.sh`, the pin and `dolt.sha256`, CI's `DOLT_VERSION` | about 2,000 lines touched | Large: this, not the store, is most of the port |

## 2. The port, and the store's tests on both backends

The copy of the store in `store/` runs on either backend; what changed is listed in section 6.2 (10 of the store's 55 files, about 130 changed lines, a new 215-line connector, and the Postgres migrations). Its whole test suite (328 tests, 1,175 with subtests) was run on each:

| Backend | Tests passed | Failed | Time (`go test ./store`, one run, same machine) |
|---|---|---|---|
| Dolt 2.4.2 | 328 of 328 (1,175 of 1,175) | 0 | 167 s |
| Postgres 18.6, a database per test | 308 (1,144) | 20 (31) | 43 s |
| Postgres 18.6, a schema per test in one database | 308 (1,144) | 20 (31) | 46 s |

Dolt passing confirms that the six portable rewrites (section 6.2) change nothing there. The 20 that fail on Postgres, run one at a time to read them:

- **11 test Dolt itself**, and fail as they should: the account check against Dolt's grants and DSN format (`TestOpenRefusesUnsafeAccount`, `TestOpenControl`, `TestControlAccount`, `TestDatabaseDenied`, `TestControlCreateDatabase`, `TestControlAndProjectDatabases`), the commit loop (`TestBatchedCommits` reads `dolt_log`), tags (`TestBackupTag`, `TestControlBackupTag`), Dolt's FULLTEXT probe and its per-cell merging (`TestDoltNeedsWriteID`).
- **8 hold one writer's transaction open, with a barrier, until a second writer reaches its commit** (`TestStartRace`, `TestUpdateOverlappingTransactions`, `TestRememberConcurrentEditsConflict`, `TestRememberNewKeysMerge`, `TestIdempotentReplayAfterRetry`, `TestControlConcurrentWriters`, `TestMergeRerun`, `TestTouchAgentRetryKeepsNewHarness`). Dolt is optimistic: both run, the loser fails at `COMMIT`. Postgres makes the second wait on the first's row lock, so it never reaches the barrier; without a lock timeout these tests hang. They test the retry path under Dolt's model, not a property of the store.
- **1 sums a boolean in the test's own SQL** (`TestAddUsageAcceptsUnknownCountsAndSpans`); fixed in the copy, it passes on both.

For the second group, `store/spike_race_test.go` races four stores on one database without a barrier: 8 agents starting the same issue, 8 updating one issue from the same rev, and 8 commenting on their own issues, 15 rounds each. On Dolt all three pass as they are. **On Postgres two failed at first, and that is a finding:** when several stores claim one unclaimed issue at once, or append the event log's next seq at once, the losers' `INSERT` fails with a unique violation (23505) once the winner commits, and the store did not retry it. Dolt reports the same collision as 1213 at commit, which it retries. With 23505 retried (the store already turns the duplicates that mean something, an issue id or an idempotency key, into their own errors first, and already retries one on `memories`), all three pass three runs in a row on both backends: exactly one winner each round, rev 2, one event, a gapless log. The full Postgres suite was rerun with that change: 312 of its 331 tests pass (the 328 and these three), and the 19 that fail are the 11 and the 8 above; nothing else changed.

**Two things the port found in the store as it is**, on Dolt too:

- **`Blocked` and the digest choose the ancestor an issue is blocked through by the order Dolt emits a recursive CTE's rows.** `blockedCTE` carries `(id, via)`, and the query orders ties by `(via <> id)` only, so of two blocked ancestors, which one is reported depends on the engine's execution order; the test (`TestBlockedLimitIsAPrefix`) says "as the recursive query reaches it first". Postgres reached the farther one. The copy adds a `depth` column and orders by it, which passes on both. Worth fixing on main whatever the backend: a Dolt upgrade could change the order.
- **`heldAssignees` (gates.go) has a recursive CTE with two recursive terms**, which Postgres (and the SQL standard) refuse. Rewritten over an `edges` CTE as `holdingGates` already is; passes on both.

## 3. Measurements

### 3.1 The claim race in raw SQL (stage 0's experiment)

2,000 open issues; each attempt reads the head of the ready queue (`pick` = choose at random among the top N; 0 = the queue partitioned so workers never collide) and claims it with `UPDATE … WHERE id = ? AND rev = ? AND assignee IS NULL` plus an upsert of the claim row, in one transaction (`tx`) or the update alone in autocommit (`auto`). Postgres runs at its default, read committed. Latency is from the first attempt to the win, retries included. "Failed" is attempts that did not win (a CAS that matched no row on Postgres, mostly 1213 at commit on Dolt). Every run was verified: no issue won twice, the database's assignee and claim row agree with the client's record.

| Workers | Mode | pick | Dolt claims/s | Dolt failed | Dolt p50 / p99 ms | Postgres claims/s | Postgres failed | Postgres p50 / p99 ms |
|---|---|---|---|---|---|---|---|---|
| 1 | tx | 1 | 82 | 0% | 12.0 / 28.1 | **207** | 0% | **5.0 / 5.9** |
| 4 | tx | 16 | 94 | 16% | 35.0 / 133 | **442** | 14% | **8.0 / 21.1** |
| 20 | tx | 1 | 84 | 94% | 150 / 1,355 | **212** | 95% | **4.9 / 10.1** |
| 20 | tx | 16 | 176 | 56% | 94.0 / 399 | **1,300** | 49% | **9.9 / 67.1** |
| 20 | auto | 1 | 84 | 94% | 181 / 1,264 | 109 | 83% | 105 / 1,079 |
| 20 | tx | 0 (no collisions) | 194 | 1% | 101 / 127 | **2,184** | 1% | **8.1 / 17.6** |
| 4 | `FOR UPDATE SKIP LOCKED` | - | n/a | | | **478** | 0% | **8.0 / 12.8** |
| 20 | `FOR UPDATE SKIP LOCKED` | - | n/a | | | **2,322** | 1% | **8.0 / 15.9** |
| 1 | tx, *default sync* | 1 | | | | 7,246 | 0% | 0.1 / 0.3 |
| 20 | tx, *default sync* | 16 | | | | 14,333 | 51% | 1.0 / 5.3 |

(The 1% at 20 workers on both is one empty-queue attempt per worker at the end.)

- **Dolt tops out at 80 to 200 claims/s here however many connections push, as in stage 0**; concurrency adds conflicts and queueing. On this Mac, with a full fsync per commit, a single claimer is slower than stage 0's Linux figure (82/s against 182/s).
- **Postgres scales with writers**: 207/s with one, 1,300 with 20 contending on the top 16, 2,184 to 2,322 with 20 that do not collide. It group-commits concurrent transactions into one WAL flush; Dolt does not.
- **`SKIP LOCKED` is the work queue Dolt cannot have**: 20 workers, 2,322 claims/s, no failed attempt, p99 16 ms. On Dolt, the best is ariesd serializing claims itself (stage 0's rule 2).
- **Rule 6 is Dolt's alone.** Under row locks, a second writer of the same row waits for the first and then either matches no row (read committed: "failed" above, 49% to 95% under contention, with no lost update, verified) or gets 40001 (repeatable read). `rev = rev + 1` is a true compare-and-swap; no `write_id` is needed.

### 3.2 The write mix in raw SQL (stage 0's experiment)

Each request: read an issue's rev, CAS-update its title and priority, insert a comment half the time, insert an event; one transaction. Dolt with a `DOLT_COMMIT` once a second, as ariesd does. 15 s a run.

| Writers | Dolt requests/s | Dolt p50 / p99 ms | Postgres requests/s | Postgres p50 / p99 ms |
|---|---|---|---|---|
| 1 | **177** | **5.1 / 11.8** | 63 | 16.0 / 31.9 |
| 8 | 176 | 43.1 / 77.1 | **411** | **17.0 / 38.0** |
| 20 | 157 | 108.7 / 432 | **991** | **16.1 / 48.8** |
| 8, *default sync* | | | 25,798 | 0.3 / 0.4 |

Dolt's commit once a second took 7.7 ms p50 with one writer and 109 ms with 20. Here a single Postgres writer was slower than Dolt's: each of its commits waits for one `F_FULLFSYNC` of the WAL (16 ms), while Dolt's single writer evidently did not wait for one per transaction (5.1 ms, under the 12 ms its single claimer took). Whether Dolt syncs each transaction to the drive was not established; if it does not, its single-writer figures are not durable ones. From 8 writers up, Postgres's group commit wins, 2.3 to 6 times.

### 3.3 Through the store, as ariesd runs it

One store (one writer connection, as ariesd has per project), 1,000 open issues, each worker an agent calling `StartIssue` for the top ready issue until none is left. Verified: no issue taken twice, every claim held by its taker at epoch 1.

| Agents | Dolt claims/s | Dolt p50 / p99 ms | Postgres claims/s | Postgres p50 / p99 ms |
|---|---|---|---|---|
| 1 | 34 | 28.0 / 46.8 | **158** | **6.0 / 8.0** |
| 4 | 35 | 89.0 / 350 | **164** | **19.1 / 81.0** |
| 20 | 41 | 343 / 2,199 | **150** | **93.2 / 570** |
| 4, *default sync* | | | 1,328 | 2.6 / 7.5 |

The store serializes writes, so neither scales with agents; what differs is the cost of one claim (ready query, CAS, claim row, event, all in one transaction): 28 ms on Dolt, 6 ms on Postgres. The store's write mix (read an issue, update its title and priority at that rev, and half the time comment on it; 500 issues; run while section 3.5's Postgres memory run was loading on the same machine):

| Agents | Dolt requests/s | Dolt p50 / p99 ms | Postgres requests/s | Postgres p50 / p99 ms |
|---|---|---|---|---|
| 1 | 48 | 19.9 / 50.0 | 67 | 12.0 / 32.0 |
| 8 | 75 | 81.9 / 433 | 59 | 108 / 478 |
| 20 | 78 | 185 / 958 | 82 | 175 / 1,080 |
| 8, *default sync* | | | 1,342 | 4.5 / 23.5 |

**Through the store, writes are about even**, because it has one writer connection per project: each write waits for its own flush to the drive on either backend, and Postgres's group commit has nothing to group. What Postgres gains here is the cost of what the write reads (the claim above includes the ready query). On Linux, with `fdatasync`, both would rise; a single writer per project caps a project at one flush per write on any database. A pool of a few writers per project would let Postgres group commits; it would not help Dolt (section 3.2), and it needs the barrier-free races of section 2, which pass.

### 3.4 Reads and writes at 10,000 issues

`BenchmarkScale/issues=10000` from the store's own benchmarks (half closed, a twentieth deferred, a tenth of the open blocked; 20 iterations), on each backend. Milliseconds per operation. The Postgres writes ran with `fsync` off (the test server), so its write rows are not durable figures; the reads are comparable.

| Operation | Dolt | Postgres | |
|---|---|---|---|
| `ready` (limit 10) | 46.2 | **12.6** | 3.7× |
| `blocked` (limit 50) | 10.7 | **4.0** | 2.7× |
| `digest` | 96.7 | **26.2** | 3.7× |
| `start-next` (ready + claim) | 54.3 | **14.0** | 3.9× |
| `ready-check` (one issue) | 0.97 | 0.76 | |
| `get` | 0.50 | 0.19 | |
| `list` / `list/full` | 0.28 / 1.32 | 0.23 / 1.13 | |
| `recall` (memories) | 2.40 | 0.69 | |
| `prime` | 0.82 | 0.37 | |
| `similar` (cold) | 2.50 | 2.49 | scored in Go |
| create / comment / update / close | 6.5 / 5.0 / 7.1 / 6.4 | 0.5 / 0.3 / 0.4 / 0.7 | not like for like |

The recursive queries (ready, blocked, digest) are where Postgres gains most. The minutes-long prepared `blockedCTE` that database.md records on Dolt did not occur on either.

### 3.5 Memory per project

Stage 3a's experiment: stores opened one after another and left open, each loaded through the store with 300 issues, each issue with a 540-byte body, a comment of the same size, a label and a claim (1,200 writes a store), then one ready query so a reader connection opens. Memory 5 seconds after each step. **Footprint** is macOS's physical footprint of the server's processes together (`footprint(1)`), which counts memory they share once; for Postgres, which runs a process per connection that each map the same shared buffers, the sum of resident sizes counts those buffers again in every process, so it is shown only for reference. For Dolt, one process, the two agree.

| Stores open | Dolt footprint MiB | Postgres, database per project: footprint (RSS sum) MiB | Postgres, schema per project: footprint (RSS sum) MiB | Postgres processes |
|---|---|---|---|---|
| 0 | 17 | 68 (100) | 68 (100) | 10 |
| 1 | 240 | 95 (174) | 95 (174) | 12 |
| 5 | 967 | 175 (365) | 144 (333) | 20 |
| 10 | 1,601 | 275 (601) | 206 (537) | 30 |
| 20 | 2,514 | 403 (1,123) | 330 (951) | 50 |
| 50 | (stage 3a on this Mac, RSS: 5,117) | 716 (2,518) | 703 (2,261) | 110 |
| all closed | (stage 3a: keeps 58 to 86%) | 203 (433) | 113 (179) | 10 |

- **Dolt: about 120 MiB a project**, growing with what was written and kept after the stores close (stage 3a). Dolt's run was stopped at 20 stores, where it matched stage 3a's 2,540 MiB on this Mac; stage 3a has the rest.
- **Postgres: about 12 to 13 MiB a project**, most of it, by the process count, the project's two backend processes (a writer and one reader here): catalog and plan caches per connection. Closing the stores gives it back. A project with all eight readers busy would hold nine backends, roughly 50 MiB. With one pool shared by every schema (not built, section 7), a project at rest would cost nothing beyond its share of the pool.
- **A database or a schema per project costs about the same memory**; the layouts differ in what they allow (section 4.3), not in size.

## 4. Availability, sharing a database, placement, history (written up, not built)

### 4.1 A warm standby with fencing

| | Dolt 2.4 (`cluster:` replication) | Postgres (streaming replication) |
|---|---|---|
| What reaches the standby | Every SQL transaction and branch head of every database, and new databases. **Not users or grants**, and not `DROP DATABASE`: each must be run on every server ([Dolt's replication guide](https://www.dolthub.com/docs/sql-reference/server/replication)) | The whole cluster, byte for byte: every database, schema, role and grant |
| Loss on failover | A planned switch (`dolt_assume_cluster_role` on the primary) is lossless. Promoting a standby after the primary died "is not guaranteed to be lossless"; the guide says nothing of a commit waiting for the standby | Asynchronous by default; with `synchronous_standby_names` and `synchronous_commit = on` (or `remote_apply`) a commit returns only once the standby has it, so promotion loses nothing acknowledged |
| Who promotes | Nobody: "a server never automatically transitions to be a primary." Something outside must call `dolt_assume_cluster_role('primary', N)` | Nobody, in core Postgres either. Patroni (with etcd or Consul), pg_auto_failover, or a managed service (Amazon RDS and Aurora, Google Cloud SQL and AlloyDB, Azure Database for PostgreSQL) does it, each widely run |
| Fencing | Epochs: a role change needs a higher epoch; a primary that meets a higher epoch becomes a standby and kills its connections; two primaries at one epoch both go read-only (`detected_broken_config`) | Patroni holds a leader lease in its store and demotes a primary that loses it, with a watchdog to reset a node that cannot. Managed services fence inside their control plane |
| What ariesd adds on either | One daemon per project as the only writer, so after a failover the daemon must follow the primary and no old daemon may write. Ariesd can fence itself on Postgres: its writer connection takes a session advisory lock on the project (`pg_try_advisory_lock(project)`), which the database drops when the connection dies, so a second daemon cannot open the project's writer while the first holds it. Dolt has no advisory locks; it would need its own lease row and the clock rule (19). Not built or measured here |
| The host key | Every `.aries.yaml` pins the server's SSH host key, so a standby daemon must hold the same key, or clients a list of keys; the same on both |

**Reading.** The database side of a standby is a solved problem on Postgres that an operator can buy; on Dolt it is a feature that replicates data but not the accounts that guard it, with promotion left to the operator. ariesd's own part (moving the daemon and its host key, and fencing so two daemons never write a project) is the same work on either, and slightly smaller on Postgres because of advisory locks.

### 4.2 Several ariesd processes on one Postgres

Postgres has what Dolt lacks for this: row locks that block (`SELECT … FOR UPDATE`), `SKIP LOCKED` for a work queue, and `LISTEN/NOTIFY` to tell other processes of a write. Section 3 measures the first two. But ariesd is built on one writer per project, and its in-memory state would block several daemons writing one project:

| State in ariesd's memory | Why it blocks a second writer |
|---|---|
| The event log's next seq (`lastEventSeq`, then seq+1 in the same transaction), and the inbox's next id | Two writers append the same seq; one fails and retries. The spike's barrier-free races (section 2) show it stays correct on Postgres, but every write to a project then contends on the log's tail, which is a single writer by another name. A sequence would not fix it: sequence values become visible out of order, and readers that page the log by seq would skip events |
| The store's time (`storeTime`, `Clock`, rule 19): the latest time it gave out, the steps it settled into leases | Two daemons would each settle a clock step into the same leases, and their times would interleave backward. The clock rule assumes one process per database |
| Watchers (event push to connected clients), and `Store.publish` after commit | A client connected to daemon A misses B's writes. `LISTEN/NOTIFY` (or polling the log) would carry them; nothing does today |
| The reaper and pruning, run by the daemon for its open stores | Two daemons would reap the same claims; harmless only because writes are CAS, and doubled |
| Limits per principal (token buckets, connection caps), write statistics, the similar-titles cache | Per daemon: a principal gets N times the quota |
| The control index (principals, keys, memberships) read at each handshake, and the keys file for sshd | Each daemon needs it current; a change on one must reach the others (`LISTEN/NOTIFY` again) |

So "several daemons on one Postgres" works today only as **several daemons, each the writer of different projects**, on one Postgres server. That is placement, not shared writing, and it needs nothing of the above. Several daemons writing one project would need a lease per project (the advisory lock above), the clock and watchers made shared, and quotas moved into the database: a stage of its own, and not needed until one project outgrows one daemon, which section 3's numbers put far off.

### 4.3 Placing projects across servers

- **Shard by project.** A project is already one database with one writer, so it is the unit: a project lives on one database server and is served by one daemon at a time. The control database records where (`projects.db_name` today; it would gain a server or cluster name), and `aries`'s `.aries.yaml` names the daemon it connects to, so a client never needs to know.
- **Database or schema per project on Postgres.** Both pass the whole store suite (section 2). A database per project needs a connection pool per project, since a Postgres connection cannot change database, and Postgres runs a process per connection (section 3.5 measures it). A schema per project in one database lets one pool serve every project (`SET search_path` per transaction, or schema-qualified names), and lets the control store and stage 6's cross-project mirror read another project's tables in one query, as `Control.HeldClaims` does on Dolt today with `` `db`.claims `` (impossible across Postgres databases). The cost: one database's catalog grows with every project (22 tables with the migrations table, and 34 indexes, each), and a dump or restore is per schema rather than per database. **Schema per project is the better fit**, with one database per server.
- **The control database** stays one per server (or one for a set of servers, holding where each project lives). On Postgres it can be a schema beside the projects', so the handshake's membership check and the keys file read the same database.
- **Moving a project** between servers is a dump of its schema and a restore (or logical replication of that schema), during which its daemon refuses writes; on Dolt it is a `dolt push` of the database plus grants by hand.

### 4.4 What replaces tag-before-upgrade, and history

- **Tag before upgrade** (`BackupTag`: a Dolt commit and tag, which `ariesd upgrade` takes before migrating) has no Postgres equivalent. Two things replace it. Postgres runs DDL in transactions, so a migration and its `schema_migrations` row commit together or not at all; a failed migration leaves the old schema, which Dolt cannot promise (its DDL commits as it goes, as MySQL's does). And the restore point becomes `pg_dump` of the project's schema before migrating, or a point-in-time recovery target where the operator runs WAL archiving or a managed service with snapshots. `BackupTag` would become "dump to `/var/backups/aries` before migrating", a few dozen lines.
- **History.** Aries reads none of Dolt's history: conflicts, audit, the digest and offline replay all come from the event log (rule 7), which is the truth on either backend. What Dolt's commit-a-second gives today is a second copy for forensics (`dolt diff`, `AS OF`) and the backups' consistency. On Postgres the event log plus backups with WAL archiving (point-in-time recovery to any second) cover both. The commit-a-second loop, `Flush`, the dirty flag and the `Committed` instrument go away.

## 5. Cost of the full port

An estimate from the inventory (section 1) and what the spike took, not a measurement. In the project's own unit, slices of a stage, each a pull request with the gate green:

| Slice | What | Size |
|---|---|---|
| P0 | The store interface (6.1), on Dolt, with the server's tests on a fake where they test the server and not the database. Worth doing whatever is decided | 1 to 2 weeks |
| P1 | The store on Postgres, in place: Postgres's SQL written where each query is (no rewriting driver), one migration set in Postgres's dialect (squashed to the current version, numbered to match), error mapping with 23505 retried, the account check on `pg_roles`, the committer and `BackupTag` removed, the eight barrier tests rewritten as section 2's barrier-free races, the eleven Dolt tests replaced. `pgtest` replaces `dolttest` in the packages that start a database (store, server, e2e, bdimport, `cmd/ariesd`, doltmetrics), with its pin and checksums for linux-amd64, linux-arm64 and darwin-arm64 | 2 weeks |
| P2 | The daemon: a schema per project in one database, created by the daemon's role (`CREATE SCHEMA … AUTHORIZATION`) instead of root's grant of a database name; the control store as a schema beside them; `HeldClaims` as a schema-qualified query; the DSN logic in `internal/server` and `cmd/ariesd` behind the store; the status report's database figures from `pg_stat_*` in place of `internal/doltmetrics` and `doltwatch.go`; the writer's advisory lock per project | 1 to 2 weeks |
| P3 | `ariesd migrate-from-dolt` (6.4), for existing servers | 1 week |
| P4 | Operations: `install-server.sh`, `deploy/docker` and `smoke.sh`, the AppArmor profile, `docs/server.md` and `docs/deploy`, backups (`pg_dump` per schema, or WAL archiving), upgrades, CI (`DOLT_VERSION` and the Dolt jobs), `RELEASING.md`'s pin procedure | 2 weeks |
| P5 | The soak gates of stages 3, 3a and 5 rerun on Postgres, on Linux | days, mostly waiting |

About 7 to 9 weeks in all, of which the store is the smaller part. Stage 6 has decided its design and built one slice (G0, migration 0018); each further migration written for Dolt now is one more to translate.

## 6. A store interface, the dialect seam, and the cost of two backends

### 6.1 The store interface

What `internal/server`, `cmd/ariesd` and `internal/bdimport` use of `internal/store` today (counted on `5d6e951`):

| | Count |
|---|---|
| Exported methods on `*Store` | 104, of which **86** are called from those packages |
| Exported methods on `*Control` | 53, of which **49** are called |
| Distinct `store.X` identifiers those packages name | **281** (742 references), from 35 files: 26 in `internal/server`, 5 in `cmd/ariesd`, 4 in `internal/bdimport` |
| Error types and sentinels they match | 35: `ErrNotFound`, `ErrConflict`, `ErrExists`, `ErrCycle`, `ErrInvalid`, `ErrForbidden`, `ErrBusy`, `ErrNothingReady`, `ErrSchemaOld`, `ErrSchemaTooNew`, `ErrStatusInProgress`, and 24 typed errors (`HeldError`, `StaleEpochError`, `ForbiddenError`, `IdemError`, `UnsafeAccountError`, …) |

An interface would be two: **`Project`** (about 86 methods: issues, deps, work, claims, inbox, memory, usage, hours, prices, plans, gates, conflicts, digest, the reaper's `ReapClaims`/`EndClaims`/`Prune`, `Stats`, `Ping`, `Close`, `Now`) and **`Controller`** (about 49: projects, principals, keys, members, the index reads at the handshake, `CreateDatabase`, `HeldClaims`). Both are large because the store is the domain layer, not a repository: each method is one authorized, evented, idempotent operation. Splitting either by area (work, memory, cost, gates, admin) would give interfaces of 10 to 20 methods each, which is the size Go fakes stay readable at.

Where the server reaches past the methods, and would keep reaching:

- **Types.** The 281 identifiers are mostly the domain types (`Issue`, `IssueID`, `Actor`, `Claim`, `NewIssue`, `IssuePatch`, …), the errors above, and constants. They are not Dolt's and would stay in the package (or move to a `store/model` the backends share); the interface does not hide them, nor should it.
- **The clock.** `store.NewClock`, `ClockOptions`, `ClockState` and `Options.Clock`: the daemon makes one `Clock` and gives it to every store (rule 19). Backend-neutral; it would be an argument to each backend's `Open`.
- **Observer and Tracer** (`Options.Observer`, `Options.Tracer`, `StageAttempt`, `StageWait`, `StageMigrate`, `StageCommit`): the telemetry hooks. `Observer.Committed` and `StageCommit` report Dolt's commit-a-second and would mean nothing on Postgres.
- **Construction and the account.** `store.Open`, `OpenControl`, `OpenConflicts`, `Options`, `ControlOptions`, `Limits`, `ControlLimits`, `ValidDBName`, `DatabaseDenied` (which matches Dolt's error 1105/1044), and `UnsafeAccountError`, whose fix text is Dolt's `GRANT`. Each backend would have its own `Open`; the error helpers would move behind it.
- **The DSN.** `internal/server/project.go`, `settings.go` and `cmd/ariesd/control.go` parse it with `mysql.ParseDSN` to derive a project's DSN from the control database's and to refuse a password in it (rule 2). That is backend knowledge outside the store, and would move behind the interface (a `Backend.ProjectDSN(name)`).
- **Dolt's own telemetry.** `internal/doltmetrics` and `internal/server/doltwatch.go` read Dolt's Prometheus endpoint and set its collector's cycles against the stores' writes. Dolt-only by nature; Postgres's equivalent is `pg_stat_*` or `postgres_exporter`.

**What a fake behind the interface would make easier.** `internal/server`'s tests (and `internal/e2e`'s) start a real Dolt for every test today, through `dolttest`'s pool. A fake `Project` would let the dispatch, error mapping (`errors.go`, rule 11), authorization at the handshake, connection limits, watchers and the registry's open and close be tested in milliseconds, and let the rare paths (a store that fails to open, a write that returns `ErrBusy`, a migration refused as too new) be driven directly instead of staged. The store's own tests would still run against the real database: what they test (CAS, events, recursive queries, the clock against the database's history) is exactly what a fake cannot.

### 6.2 The dialect seam built for the comparison

| Where | What | Size |
|---|---|---|
| `pgdialect.go` (new) | A `driver.Connector` over pgx that rewrites each statement as it is sent: `?` to `$n` (all ~230 statements), `INSERT IGNORE` (4), `JSON_UNQUOTE(JSON_EXTRACT(…))` (4, the digest), `DATABASE()` (2, migrate), `CAST(… AS CHAR(n))` (3, recursive CTEs), backquotes (3, `principal_keys.key`), one `CASE` whose placeholders need a type (paths). Also Postgres's error codes and the account check | 220 lines |
| `store.go` | `open` picks the driver by DSN; the committer does nothing on Postgres | 40 lines changed |
| `errors.go` | `retryable` adds 40001, 40P01 and 23505 (section 2); `isDuplicate` adds 23505 | 14 lines |
| `migrate.go` | Embeds the Postgres migration sets; picks one | 20 lines |
| `backup.go` | Refuses on Postgres | 3 lines |
| `pgmigrations`, `pgcontrolmigrations` | Both sets, translated: `DATETIME(6)`→`TIMESTAMP(6)`, `TINYINT`→`SMALLINT`, inline `KEY`→`CREATE INDEX`, `COLLATE utf8mb4_0900_bin`→`COLLATE "C"`, `DROP INDEX … ON`. JSON stays `JSON` (not `JSONB`): tests compare event JSON as text, which Dolt keeps compact and `JSONB` reformats | 19 files, 570 lines |
| Portable edits, valid on both | `SUM(boolean)`→`SUM(CASE …)` (notes.go, and usage_test.go); `UPDATE`/`DELETE … ORDER BY … LIMIT`→ a subquery (inbox.go, limits.go); `heldAssignees`' two recursive terms → one over an `edges` CTE, as `holdingGates` already has it (gates.go); an explicit `depth` in `blockedCTE` (graph.go, digest.go; see section 2) | 6 statements |

So the seam touches 10 of the store's 55 files and changes about 130 of its 18,400 lines, plus the new connector and the migrations. It is a rewrite at the driver, which is right for a comparison and wrong to keep: it hides the dialect from the reader of each query, and it costs a map lookup per statement. A kept port would write Postgres's SQL where the query is.

### 6.3 Keeping both Dolt and Postgres, against picking one

| | One backend | Both, permanently |
|---|---|---|
| Test matrix and gate time | The store suite as today | Every store, server, e2e and bdimport test on each backend. Measured here on the store suite alone, run one after another on the same machine (the copy, `go test ./store`, no `-race`): **Dolt 167 s, Postgres 43 s** (database per test) or **46 s** (schema per test). Both, in CI, is the sum on one runner or a second job; the soak gates (stages 3, 3a, 5) would need a run on each |
| Migrations | One set | Two sets forever, kept equal by hand, each append-only (rule 9), with a test that they define the same tables and columns. Every stage 6 migration written twice |
| SQL | One dialect | A seam, as above, or two implementations of each query that differs. The six statements that needed portable rewrites here show what a reviewer must catch on every change: Postgres refused one recursive CTE Dolt accepts, and the `Blocked` "via" relied on Dolt's emission order |
| Error mapping | One table | Two: 1213/1205/1105/1062/1044 and 40001/40P01/23505/55P03/42501, and every new error code mapped twice |
| Rule 6, `write_id` | Postgres: not needed. A `WHERE rev = ?` CAS is exact under row locks (the second writer waits, then matches no row or fails with 40001); section 3.1's raw races show no lost update without it | Kept for Dolt, so every `UPDATE` carries it on both |
| Concurrency model | Postgres blocks a writer on another's row lock; Dolt lets both run and fails one at commit | Both models in tests: 8 of the store's tests hold one transaction open with a barrier while another runs, which works only on Dolt (section 2), and need a second form for Postgres |
| Full-text search | Postgres has `tsvector` and `pg_trgm` (trigram similarity, which `similar.go` approximates in Go because Dolt's FULLTEXT was unreliable) | Whatever both support, which is Go |
| Vectors | `pgvector` | Brute force in Go, as planned (database.md) |
| Installer, AppArmor, Docker | `install-server.sh` (1,002 lines, 100 mentions of Dolt), the AppArmor profile, `deploy/docker` (Dockerfile, entrypoint, `smoke.sh`), each for one database | Each twice, or one with branches, and both tested |
| Docs and operations | `docs/server.md` (3,946 lines, 304 mentions of Dolt), backups, upgrades, the status report's Dolt figures (`internal/doltmetrics`), the Dolt pin and its checksums | All of it twice; an operator must choose, and support must know both |

The second backend's cost is not the port, which section 2 shows is small for the store; it is that every later change pays it again. **Pick one.** Breadth (managed, on-premises, every cloud) comes from the many services that speak Postgres's protocol, not from a second engine: a store written for Postgres runs unchanged on RDS, Aurora, Cloud SQL, AlloyDB, Azure, Crunchy, Neon or a self-run Patroni cluster. This confirms the view in the request, with one refinement: build the interface (6.1) for the server's tests and for clarity, not as a plug for a second backend.

### 6.4 A one-way move: `ariesd migrate-from-dolt`

If the switch is made, a project moves once, by copy, not by replay:

1. With the daemon stopped (or the project archived, which refuses connections), open the Dolt store read-only and the Postgres schema, migrated to the same version (both sets carry the same numbers).
2. Copy each table in key order, in batches, inside one Postgres transaction: 22 tables, all with plain types; the only conversions are `TINYINT`→`SMALLINT` and JSON as text. The event log is copied as is, seqs included, so its history and every client's cursor survive.
3. Check: row counts per table, the event log's last seq and a hash of each table's rows in key order, on both sides. Then record the move in the control database and start the daemon on Postgres.

Replaying the event log instead would rebuild state from events that compact large texts (`EventTextMax`) and that some bookkeeping skips (`wtx.quiet`), so it cannot reproduce the tables; copying can. bdimport's export and import (`export-bd`, `import-bd`) are not a substitute either: they carry issues, deps and comments, not claims, memories, usage, gates or the log.

Size: a command in `cmd/ariesd` and a copier in the store, about 400 to 600 lines with tests (a table list, a batch copier, the checks), run against `dolttest` and `pgtest` side by side. The control database moves the same way. Kept for one or two releases, then removed with the Dolt code.

## 7. What was not measured

- **Linux.** Everything ran on macOS on Apple silicon, where a durable commit costs a full cache flush on both. Production is Linux; the soak gates should be rerun there before any switch (slice P5). Stage 0's Linux Dolt figures are not comparable with these.
- **Whether Dolt syncs each transaction.** Its single writer committed in 5 ms where a full flush costs about 12 to 16 ms here (section 3.2). If Dolt does not flush each transaction, its single-writer figures are not durable ones and the gap there is larger than shown.
- **A standby on either backend**, failover times, or a synchronous standby's cost per commit. Section 4.1 is from the documentation.
- **Several ariesd processes on one database**, `LISTEN/NOTIFY`, and the advisory-lock fence: written up (4.1, 4.2), not built. The barrier-free races (section 2) are the nearest evidence: several stores on one database stay correct on Postgres.
- **MySQL 8.4 or MariaDB.** Not installed; no figures.
- **One connection pool for many schemas.** The store opens its own pool per project on either layout, so the schema-per-project memory figures still have a writer and readers per project. A shared pool (`SET search_path` per transaction) would cut Postgres's per-project memory further; not built.
- **Hundreds of projects.** A run of 200 schemas (with 2 readers each) was stopped at 10, where it matched the 50-schema run; loading through the store takes about half a minute a project, so it would have run over an hour.
- **Long runs and growth:** minutes, not days; no vacuum or bloat on Postgres, no garbage collection on Dolt beyond stage 3a's.
- **Load from another session**, which ran the repository's gate during some runs (Environment). Each run records its load average.
- **The rest of the module's tests** (`internal/server`, `internal/e2e`, `cmd/ariesd`, `internal/bdimport`) on Postgres: only the store was ported.
- **Vectors and full-text search** on Postgres (`pgvector`, `pg_trgm`).
