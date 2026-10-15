# A profile of starfixd: memory, allocation and where the cost is

**Research, 13 October 2026.** What `starfixd` holds in memory and what
bounds it, what a request allocates, how many statements it sends Dolt,
how the store's work grows with a project, what a soak retains, and
what the garbage collector costs. It ends with the changes worth
making, ranked. It decides nothing, and no product code was changed for
it: every number comes from a benchmark or a test added beside the
code ([§5](#5-the-benchmarks-added)), or from a prototype kept outside
the repository.

| | |
|---|---|
| Commit | `32a6eaf` (`main`, 13 Oct 2026) |
| Go | 1.27.2, `darwin/arm64` |
| Machine | Apple M4 Max, 16 cores, 64 GB, macOS (Darwin 27.0.0) |
| Dolt | 2.4.2, started by `internal/dolttest`, reached over its unix socket with the locked-down account |
| Build | The tests' own, without `-race`; the process measurements use the release flags (`CGO_ENABLED=0 -trimpath -ldflags "-s -w"`) |

The machine was shared with three other agents running soaks and test
suites, with a load average of 4 to 20 throughout. **Counts are
exact and load does not move them:** allocations, bytes, retained heap
after a collection, statements, round trips. **Times are not:** each is
one run's mean, and the same benchmark gave times up to three times
apart between runs ([§4.8](#48-what-load-did-to-the-times)). Read the
times for their order of size and their shape, never for a few percent.

Dolt's own memory is not here. Stage 3a measured it
([spike/multidb/RESULTS.md](https://github.com/ariesworx/aries/blob/fbbbf4655665845db81a72d2bbd1845472fd1c5a/spike/multidb/RESULTS.md),
[docs/server.md](../server.md#dolts-memory)): 95 to 180 MiB for each
small project written to. That is Dolt's, beside which everything
below is small.

## 1. Summary

### 1.1 Where the cost is

**In Dolt, and in the processes beside the daemon. Not in the daemon's
heap, and not in its allocations.**

- **Time: a request waits on Dolt.** A write spends 94 to 98% of its
  time waiting for Dolt's answer, and 63 to 88% of it in the one
  `COMMIT` ([§1.4](#14-what-a-request-costs)). A read spends 71 to 91%.
  In a two-minute soak at 114 requests a second, every connection
  handler of the daemon together used 5.2 seconds of CPU: 4% of one
  core, of which 85% was `database/sql` and the MySQL driver talking to
  Dolt, and 6% JSON ([§4.6](#46-the-soak)). Saving allocations in the
  daemon's own code would not show in any request's time.
- **The commit is Dolt's, whatever was written.** A transaction that
  changes nothing takes 0.06 ms. One that changes a single row, any
  row, with or without an event, takes 7 to 9 ms
  ([§4.5](#45-the-store-at-size)). Nothing in the store's Go shortens
  that.
- **Under load, writes wait for the writer.** In the same soak the
  handlers spent 1,007 seconds blocked waiting for the single writer
  connection, against 121 seconds of wall time: eight requests queued
  behind the one in progress, on average. A write alone takes 9 to
  15 ms here; in the soak its median was 45 ms. That is rule 5 and
  Dolt's commit time, and it is the ceiling on a project's writes: 70
  to 110 a second on this machine. **More than half of that waiting,
  577 seconds, was `renew`**, which takes the writer twice, for its
  claims and for its session's note, whether or not it has anything to
  change ([§2](#2-proposed-optimizations), 4).
- **Three reads grow with the project, in Dolt.** `ready` takes 3 ms at
  100 issues, 48 ms at 10,000 and 226 ms at 50,000; `blocked` and
  `digest` grow the same way. `start` with no id ranks the ready issues
  **inside the write**, so at 50,000 issues it holds the project's
  writer for 0.4 s ([§1.5](#15-how-the-store-grows-with-a-project)).
  This is where the optimization path is.
- **Memory: the daemon is small, and one buffer is most of it.** An
  idle daemon is 23 MiB resident, 11 MiB of it Go's. A connection
  holds 76 KB, of which 64 KiB is the first buffer of its frame
  decoder. A connection that has once sent a frame near the 4 MiB
  limit holds 4.2 MB for the rest of its life
  ([§1.2](#12-what-the-daemon-holds)).
- **The bridge costs more than the daemon.** Each connection has a
  `starfixd stdio` process on the server, started by sshd. One is
  9.9 MiB of memory of its own (21 MiB resident). Twenty agents are
  26 MiB of daemon and 198 MiB of bridges
  ([§1.2](#12-what-the-daemon-holds)).
- **Nothing leaks.** Over a ten-minute soak the daemon's part of the
  heap was 2.4 to 4.0 MB while the binary around it grew from 10 MB to
  113 MB; what moved it is bounded, and its goroutines followed its
  connections ([§4.6](#46-the-soak)). Every map a client can add to has
  a bound; one has no eviction ([§1.6](#16-what-grows)).
- **The collector needs no tuning** ([§4.7](#47-the-garbage-collector)).

### 1.2 What the daemon holds

Heap and stacks are read in the test binary after two forced
collections (`runtime.ReadMemStats`), as the difference a number of
units makes, less what the benchmark's own end of each socket holds
(`BenchmarkRetained…`, [§4.3](#43-retained-memory)). Process figures
are a real `starfixd serve --dev`, read with `ps` and `vmmap`
(`BenchmarkProcess`, [§4.2](#42-the-process)).

| What | Holds | Method |
|---|---|---|
| Idle daemon, one project, telemetry off | 22.6 to 23.6 MiB resident | Process, `ps` |
| Idle daemon, metrics on (`telemetry.listen`) | 24.2 to 24.3 MiB resident; 11.3 MiB by Go's own account (`go_memory_used_bytes`); 39 goroutines | Process, `ps` and `/metrics` |
| Daemon with 200 idle connections | 22.9 MiB of its own with telemetry off, 26.2 MiB with metrics on (physical footprint) | Process, `vmmap` |
| One connection, past its handshake, idle | 67.5 KB heap, 8.4 KB stack, 1 goroutine. 65.5 KB of the heap is the decoder's first buffer (`proto.NewDecoder`) | In process |
| The same, by the process's own figures | 59 to 85 KB resident; 88 KB by Go's account | Process |
| One connection after one frame near 4 MiB | 4.20 MB heap, until it closes | In process |
| One socket before its hello | 66.4 KB heap, 6 to 8 KB stack, 1 goroutine, for up to 10 s | In process |
| One socket whose hello never ends | 4.20 MB heap, for up to 10 s | In process |
| One watcher (`sfx tui`, `sfx watch`, `sfx mcp`), beyond its connection | 0.84 KB heap, 3.2 KB stack, 1 goroutine; its queue holds at most 64 items of about 0.4 KB | In process; the queue from `store.WatchQueue` and `InboxBodyMax` |
| The bridge, `starfixd stdio`, one process for each connection | 9.8 to 9.9 MiB of its own; 20.8 to 21.0 MiB resident, most of it the binary's text, which every bridge shares | Process, `vmmap` and `ps` |
| One open project store, just opened | 19 KB heap, 5 goroutines (a committer, two pool openers, two driver watchers) | In process, less its one connection |
| One open project store whose eight readers have each read a page of long issues | 1.6 MB heap, 12 goroutines, 0.1 MB stacks. The driver keeps each connection's buffer, up to 256 KiB | In process |
| One store's cache of closed titles | 375 B a title, 0.75 MB at its bound of 2,000 | In process (`BenchmarkSimilarCache`) |
| Control index: one principal | 70 to 116 B | In process (`BenchmarkControlIndex`) |
| Control index: one key (ed25519) | 257 to 301 B | In process |
| Control index: one membership | 164 to 187 B | In process |
| Control index: one project | 328 B | In process |
| One write bucket: a principal in a project that has written once | 131 B, never removed | In process |
| Metrics on | 65 KB, and 16.9 KB for each project that has had a request | In process |
| Traces on, the collector not answering | 2.8 MB: the batch queue full at 2,048 spans and 512 in the export, 1.1 KB a span | In process (`BenchmarkRetainedTelemetry`) |
| Logs exported too, the collector not answering | 2.1 MB more: 2,048 records of 1.0 KB | In process |

### 1.3 Every limit, as memory

The daemon's own memory for each limit in `server.Limits`,
`store.ControlLimits` and `store.Limits`, at its default, and what the
worst case holds. "Rows" means the limit bounds rows in Dolt, and the
daemon holds nothing for it between requests.

| Limit | Default | For each | Worst case at the default |
|---|---|---|---|
| `conns` | 1,024 | 76 KB (heap and stack) | 78 MB. **4.3 GB** if every connection has sent one frame near 4 MiB |
| `conns`, the sockets before a hello | 1,024 more | 73 KB | 74 MB. **4.3 GB** for ten seconds at a time if each sends a hello that never ends, which takes 64 principals |
| `conns_per_principal` | 32 | 76 KB | 2.4 MB a principal in a project; 134 MB with the buffers grown |
| `handshakes_per_principal` | 16 | 73 KB | 1.2 MB a principal; 67 MB with endless hellos, for ten seconds |
| `idle_timeout` | 10 m | | Frees a connection's 76 KB, or 4.2 MB. A connection that watches is never idle, and `sfx mcp` watches |
| Watchers (no limit of their own; at most `conns`) | 1,024 | 4 KB, and a queue of up to 26 KB | 4 MB; 31 MB with every queue full |
| `open_stores` | 64 | 19 KB opened; 1.6 MB once read; 0.75 MB of closed titles | 155 MB, measured; 196 MB if every driver buffer is at its 256 KiB |
| `store_idle` | 10 m | | Frees all of a store's |
| `projects` | 500 | 328 B in the index; 16.9 KB of metric series | 0.2 MB and 8.4 MB |
| `principals` | 5,000 | 70 to 116 B | 0.6 MB |
| `principals_per_creator` | 500 | | Part of `principals` |
| `keys_per_principal` | 20 | 257 to 301 B | 30 MB, at 100,000 keys |
| `members_per_project` | 1,000 | 164 to 187 B in the index; 131 B for a write bucket | 94 MB of index and 66 MB of buckets, at 500,000 memberships |
| The control index, all of it | | | 124 MB, and as much again while the reaper rereads it, every 30 s |
| `write_rate`, `write_burst` | 10, 100 | 131 B a bucket | With `members_per_project`, above |
| `control_write_rate`, `control_write_burst` | 1, 30 | About 100 B a principal | 0.5 MB |
| `refusal_logs` | 20 a minute | About 100 B a principal in a project refused in the last minute | Pruned past 4,096 entries |
| `telemetry_requests` | 8 | One HTTP request of the listener | Not measured |
| OTLP queues (`OTEL_BSP_MAX_QUEUE_SIZE`, `OTEL_BLRP_MAX_QUEUE_SIZE`) | 2,048 each | 1.1 KB a span, 1.0 KB a record | 2.8 MB and 2.1 MB; 90 MB and 66 MB at the most the settings take, 65,536 |
| `agent_keep`, `inbox_keep`, `sessions_per_principal`, `inbox_unread`, `notices_per_minute`, `usage_per_day`, `memories_per_scope`, `hours_per_day`, `deps_per_issue`, `paths_per_issue` | | Rows | Nothing held. A request reads at most a page of them |
| `labels_per_issue` | 50 | Rows; one `INSERT` each in a create ([§1.4](#14-what-a-request-costs)) | A create of 50 labels is 59 statements and 115 round trips |
| `acceptance_items` | 200 | Rows; parsed from the text on every `show` and `start` | 74 KB and 809 allocations for a checklist of 200, in 95 µs |
| `usage_records` | 500 | Rows; one statement for the batch | 6 statements for 1 record or for 101; 40 ms for 101 against 16 ms for 1 |
| `memory_body`, `memory_tags`, `memory_tag_length`, `memory_key_length`, `hours_note` | 4,096 B and less | Rows; the secrets lint reads a memory's body | 1.2 ms of CPU for 4 KiB |
| `prices`, `plans`, `plan_principals` | 1,000, 1,000, 100 | Rows; a cost report reads the whole tables | Not measured |
| The largest frame (`proto.MaxFrame`, not a setting) | 4 MiB | A request's transient: the line, a copy of its arguments, the decoded arguments | About three times the frame for as long as the request runs, then the 4.2 MB above |
| A page of issues (`list`, at most 500) | 50 | Transient: every column of every row, the four texts among them | 0.2 MB at the default with short texts; 17.7 MB for 500 issues of 32 KiB; over 130 MB for 500 at the texts' limits |

**A server at its default limits, in the worst case, holds about
0.54 GB** in everything that is bounded as it should be: 78 MB of
connections, 74 MB of sockets before their hello, 31 MB of watchers,
155 MB of open stores, 124 MB of control index (twice that for a moment
every 30 seconds), 66 MB of write buckets and 13 MB of telemetry. **It
holds 4.8 GB if each of its 1,024 connections has ever sent one frame
near 4 MiB, and about 9 GB for ten seconds at a time if 1,024 more
sockets send hellos that never end.** The buffer is the whole of the
difference. Beside the daemon, on the same machine, the 1,024
connections' bridges are **9.9 GiB**.

A team of 20 agents on one project is another matter: 26 MiB of
daemon, 198 MiB of bridges, and Dolt.

Since 14 October 2026 `conns_per_principal` is 64, after the bridge
was made lighter (proposal 2, Done): 4.9 MB of the daemon's for a
principal in a project, 268 MB with the buffers grown, and about
320 MiB on the machine with each connection's bridge, sshd and kernel,
measured on Linux.

### 1.4 What a request costs

| Op | Time | Bytes | Allocations | Statements | Round trips | Commits | Waiting on Dolt |
|---|---|---|---|---|---|---|---|
| `show` | 2.9 ms | 62.2 KB | 780 | 23 | 38 | 0 | 83% |
| `list` | 1.1 ms | 188.5 KB | 2,078 | 4 | 6 | 0 | 74% |
| `ready` | 2.8 ms | 49.2 KB | 631 | 6 | 10 | 0 | 91% |
| `blocked` | 1.7 ms | 69.5 KB | 732 | 5 | 7 | 0 | 91% |
| `digest` | 7.8 ms | 333.1 KB | 3,513 | 18 | 31 | 0 | 90% |
| `who` | 0.31 ms | 7.9 KB | 136 | 4 | 6 | 0 | 80% |
| `inbox` | 0.31 ms | 5.9 KB | 99 | 4 | 6 | 0 | 80% |
| `claims` | 0.30 ms | 7.5 KB | 123 | 4 | 6 | 0 | 82% |
| `comments` | 0.27 ms | 10.7 KB | 138 | 4 | 6 | 0 | 79% |
| `history` | 0.44 ms | 32.3 KB | 243 | 4 | 6 | 0 | 79% |
| `recall` | 0.76 ms | 16.3 KB | 217 | 5 | 8 | 0 | 87% |
| `prime` | 4.6 ms | 95.4 KB | 1,286 | 23 | 38 | 0 | 88% |
| `refused` | 0.25 ms | 8.2 KB | 120 | 3 | 4 | 0 | 71% |
| `create` | 13.2 ms | 23.8 KB | 270 | 9 | 15 | 1 | 97%, the commit 84% |
| `update` | 12.8 ms | 27.6 KB | 425 | 10 | 17 | 1 | 96%, the commit 79% |
| `comment` | 12.5 ms | 10.1 KB | 154 | 7 | 11 | 1 | 98%, the commit 86% |
| `start` | 15.4 ms | 53.4 KB | 727 | 21 | 35 | 1 | 94%, the commit 63% |
| `finish` | 16.9 ms | 44.5 KB | 625 | 16 | 29 | 1 | 95%, the commit 70% |
| `handoff` | 26.4 ms | 38.5 KB | 561 | 15 | 27 | 1 | 95%, the commit 72% |
| `renew`, with nothing to extend and its session's note fresh | 0.34 ms | 10.7 KB | 178 | 6 | 8 | 2, of nothing | 75% |
| `usage`, one record | 23.6 ms | 11.2 KB | 161 | 6 | 9 | 1 | 98%, the commit 88% |
| `remember` | 18.4 ms | 16.8 KB | 193 | 7 | 11 | 1 | 97%, the commit 85% |
| `status`, on a server of two projects | 0.28 ms | 8.7 KB | 96 | | | | |
| The handshake | 0.25 ms | 76 KB | About 120 | 3 | 4 | 1, of nothing | |

- **Time** is one run's mean on a loaded machine. **Bytes and
  allocations** are the daemon's for one request, from the frame read
  off a unix socket to the answer written to it, telemetry off, on a
  project of about 100 issues; the client's side allocates nothing
  (`BenchmarkOp`, [§4.4](#44-a-request)). **Statements, round trips
  and commits** are read off the wire between the store and Dolt by a
  proxy, and so is **the wait on Dolt**: from each command's last byte
  sent to its answer's first byte received, as a share of the
  request's time in the same run (`BenchmarkOpStatements`).
- `prime` is the four requests an agent's session sends when it
  starts: `list` of its own issues in progress, `ready`, `inbox` and
  `recall`. It is a client's composition; the daemon has no such op.
- `refused` is a `show` of an issue that does not exist: one read, the
  error's mapping, and a line in the log that is sampled.
- **`renew`** here finds every lease with more than half its time
  left and extends nothing, as most renews do: of a lease of 15
  minutes renewed every minute, seven in eight. It still begins and
  commits two write transactions, one for the claims and one for the
  session's note in the registry. The benchmark's renews follow one
  another at once, so the note is fresh too; on a real clock a note is
  rewritten when it is a minute old (`AgentTouchEvery`), which a renew
  every minute mostly finds, and that is a commit of 7 to 9 ms, as a
  renew that extends a lease is.
- **The handshake** is a connection from its first frame to its
  welcome and its end. 65.5 KB of its 76 KB is the decoder's first
  buffer. Its three statements are the session's note in the registry,
  which is a row written only for a new session, and at most every
  `AgentTouchEvery` after.
- **A statement is not a round trip.** The store sends every statement
  that has arguments as a prepare, an execute and a close, so each is
  two round trips: 15 of `show`'s 38 are prepares. And a read of
  several queries begins a read-only transaction and rolls it back, so
  `who`, whose work is two queries, is four statements and six round
  trips.

**Where a request's allocations are made.** From profiles that record
every allocation (`-memprofilerate 1`), for the stacks under the
connection's handler ([§4.4](#44-a-request)):

| Layer | `who` (a read of two queries) | `show` | `list` | `comment` (a write) | `create` |
|---|---|---|---|---|---|
| All of the request path | 7.8 KB, 117 | 66.9 KB, 672 | 188 KB, 1,903 | 12.5 KB, 140 | 24.5 KB, 240 |
| Frame decode (`internal/proto`) | 0.45 KB, 2 | 1.2 KB, 3 | 2.8 KB, 4 | 0.9 KB, 2 | 1.2 KB, 2 |
| Arguments decode (`decodeArgs`) | 0.63 KB, 7 | 0.65 KB, 7 | 0.78 KB, 7 | 1.0 KB, 9 | 3.4 KB, 12 |
| Answer encode and write | 0.89 KB, 4 | 6.1 KB, 7 | 22.5 KB, 10 | 0.47 KB, 4 | 0.52 KB, 4 |
| Log (`slog`, `internal/logs`), at info | 0.11 KB, 3 | 0.25 KB, 4 | 0.55 KB, 5 | 0.14 KB, 4 | 0.12 KB, 4 |
| The MySQL driver | 1.8 KB, 44 | 13.0 KB, 246 | 20.2 KB, 700 | 1.6 KB, 37 | 5.0 KB, 77 |
| `database/sql` above the driver | 1.9 KB, 34 | 17.4 KB, 218 | 25.6 KB, 335 | 3.7 KB, 47 | 7.4 KB, 75 |
| The store above `database/sql` | 1.3 KB, 13 | 24.5 KB, 169 | 111 KB, 835 | 3.9 KB, 31 | 6.1 KB, 59 |
| of which the event's JSON and the idempotency hash | | | | 0.9 KB, 4 | 2.0 KB, 4 |

Each cell is bytes and allocations for one request. The layers do not
sum to the first row: the rest is the handler's own (a context, a
timer, the request's id, the dispatch).

- **Two thirds of a small request's allocations are `database/sql` and
  the driver:** 78 of `who`'s 117. Every statement with arguments is
  prepared, executed and closed, and reads its column definitions
  twice.
- **The protocol is cheap.** Reading a request and writing its answer
  are 13 allocations and 2 KB, on any op that returns little.
- **`list` is the store's own.** 835 allocations and 111 KB to scan 50
  rows of 26 columns, of which the answer uses four.
- **Telemetry.** Metrics add 8 allocations and 0.6 KB to a request
  (3 to 17 across the ops). Traces add 43 allocations and 4.3 KB to a
  read on the request's own path, and 67 and 7.4 KB to a write, which
  has a span for each of its stages. **Half of what traces add to a
  read is the stand-in for the principal and the session**, two HMACs
  made again for every request: 23 allocations and 1.95 KB. The export
  is on top, on the exporter's goroutine: with a collector that
  answers, a read costs 68 allocations and 6.0 KB more than with
  telemetry off, end to end, and a write 70 to 190 and 8 to 20 KB, by
  how many spans it has and where their export fell. Exporting the log
  as well adds nothing at `info`, which logs nothing for a request
  that succeeds. `debug`, with telemetry off, adds 0 to 4 allocations
  a request for its line.
- **An event pushed to watchers** costs the write 5 allocations and
  1.3 KB for each watcher: 152 allocations and 10.8 KB for a comment
  that nobody watches, 668 and 143.5 KB for one that 100 connections
  do (`BenchmarkPush`). Its time is the comment's.

**Statements for each item.** Requests that carry several things, at
two sizes each (`BenchmarkOpStatementsBatch`):

| Request | Statements | Round trips | Time | For each item |
|---|---|---|---|---|
| `create` with 1 label | 10 | 17 | 12.4 ms | |
| `create` with 21 labels | 30 | 57 | 17.8 ms | **1 statement, 2 round trips a label** |
| `finish` with nothing discovered | 13 | 23 | 12.7 ms | |
| `finish` with 5 discovered issues | 58 | 113 | 23.5 ms | **9 statements, 18 round trips an issue** |
| `usage` with 1 record | 6 | 9 | 16.4 ms | |
| `usage` with 101 records | 6 | 9 | 39.5 ms | None: one statement holds them all |
| `renew` with 2 claims held | 6 | 8 | 0.39 ms | |
| `renew` with 12 claims held | 6 | 8 | 0.43 ms | None |

What does work for each of N things: **a create's labels** (one
`INSERT` each, so 59 statements at the limit of 50); **a finish's
discovered issues** (each is a create inside the finish); **every
event** (one `INSERT` each, `wtx.flush`); and **`show`**, whose eight
parts are read one after another ([§2](#2-proposed-optimizations),
7). `usage` and `renew` do not. None of it is the unbounded kind: each
N has a limit, and the cost is round trips of 50 µs beside a commit of
10 ms.

### 1.5 How the store grows with a project

The store's reads and writes on databases of 100, 1,000, 10,000 and
50,000 issues, half of them closed, with bodies of 400 bytes
(`BenchmarkScale`, [§4.5](#45-the-store-at-size)). Times are Dolt's for
the most part; bytes are the store's and its driver's, for one call.

| Call | 100 | 1,000 | 10,000 | 50,000 | Grows |
|---|---|---|---|---|---|
| `GetIssue` | 0.48 ms, 7 KB | 0.47 ms | 0.61 ms | 0.41 ms, 7 KB | No |
| `List`, a page of 50 | 1.3 ms, 167 KB | 1.3 ms | 1.5 ms | 1.4 ms, 167 KB | No |
| `Ready`, 10 | 3.4 ms, 89 KB | 10.9 ms, 169 KB | 48 ms, 177 KB | 226 ms, 177 KB | **Time, with the issues.** In Dolt: the recursive query over every dependency and issue. Bytes do not |
| `StartIssue` with no id | 19 ms | 26 ms | 121 ms | 411 ms | **The same, inside the write** |
| `Blocked`, 50 | 1.6 ms, 50 KB | 3.8 ms, 380 KB | 13 ms, 3.2 MB | 67 ms, 15.5 MB | **Time and bytes, with the blocked issues:** every blocked row is read whole, and the limit applied after |
| `Digest`, 24 hours | 10 ms, 0.19 MB | 19 ms, 0.31 MB | 128 ms, 5.9 MB | 320 ms, 17.8 MB | **Time and bytes, with the events in the window** (3, 33, 333 and 1,667 here), up to 10,000 rows a section |
| `SimilarClosed`, titles cached | 4.5 µs, 8 KB | 45 µs, 65 KB | 207 µs, 320 KB | 313 µs, 320 KB | With the closed titles cached, up to 2,000 |
| `SimilarClosed`, after a close | 0.44 ms | 1.7 ms | 14 ms, 2.2 MB | 75 ms, 2.2 MB | Time with the closed issues, in Dolt's sort; bytes stop at 2,000 titles |
| `Recall`, `PrimeMemories`, `WhoPage`, `ActiveClaims` | 2.3, 0.7, 0.16, 0.42 ms | The same | The same | The same | No |
| `CreateIssue`, `AddComment`, `UpdateIssue`, `CloseIssue` | 12 to 16 ms | 12 to 15 ms | 11 to 20 ms | 13 to 18 ms | No: the differences are the machine's load |

Nothing is worse than linear. What is linear is linear in Dolt, but
for `Blocked` and `Digest`, which also read rows the answer does not
use.

**With long texts** (`BenchmarkLongBodies`, 1,000 issues whose bodies
are 32 KiB each):

| Call | Time | Bytes |
|---|---|---|
| `List`, 50 | 3.9 ms | 1.8 MB |
| `List`, 500 | 61 ms | 17.7 MB |
| `Ready`, 10 | 11.9 ms | 0.49 MB |
| `Blocked`, 50 | 8.7 ms | 3.6 MB |
| `GetIssue`, four texts of 64 KiB | 0.89 ms | 0.56 MB |
| `UpdateIssue` of one of those texts | 17.6 ms | 4.5 MB |

`list`, `ready` and `blocked` answer with four fields of each issue and
read all 26, the texts among them. An update of an issue at its limits
allocates 4.5 MB to write an event that keeps 512 bytes of each long
text.

**The work done in Go**, on inputs at their limits (`BenchmarkGo`):

| Work | Time | Bytes, allocations |
|---|---|---|
| `safetext` validation of a 64 KiB text | 74 µs (880 MB/s) | 0, 0 |
| The secrets lint of a 4 KiB memory | 1.2 ms (3.4 MB/s) | 0.2 KB, 0 |
| A title's tokens | 0.5 µs | 0.5 KB, 6 |
| Scoring 2,000 closed titles | 86 µs | 0.5 KB, 6 |
| Parsing an acceptance checklist of 200 items | 95 µs | 74 KB, 809 |
| An event's state for a small issue | 1.0 µs | 2.2 KB, 5 |
| An event's state for an issue with four 64 KiB texts | 0.9 ms | 2.6 MB, 160 |
| The idempotency hash of a small create | 1.5 µs | 1.7 KB, 7 |
| A write's id (`write_id`) | 52 ns | 0, 0 |

None of it is a cost beside a commit of 10 ms, but the secrets lint,
which is a tenth of a `remember`'s time for a memory at the limit.

### 1.6 What grows

**Nothing without bound.** Every cache, map and slice a client can add
to, what bounds it and what evicts:

| What | Keyed by | Bound | Evicts |
|---|---|---|---|
| `Server.conns` (`server.go`) | The connection | Twice `conns` | The handler's return |
| `conns.by`, `waiting.by` (`limits.go`) | Principal, or project and principal | Those with a connection now | Deleted at zero |
| `buckets.m`, the write buckets and the control-write buckets (`limits.go`) | Project and principal; principal | Every pair that has ever sent a write since the daemon started: at most the memberships there have been | **Nothing.** [§2](#2-proposed-optimizations), 10 |
| `sampler.m`, the refusal log's windows (`limits.go`) | Project and principal | 4,096 and those refused in the last minute | Past 4,096, the windows older than a minute |
| The registry of open stores (`registry.go`) | Project | `open_stores` | Idle for `store_idle`; the longest idle at the cap |
| `Server.unopened` (`server.go`) | Project | `projects` | Opened or archived |
| The control index (`store/control.go`) | Fingerprint; principal; project; membership | The control limits | Rebuilt whole on every reload |
| The similar-title cache (`store/similar.go`) | One for each open store | 2,000 titles | Replaced when stale; freed with the store |
| A watch's queue (`store/inbox.go`) | The connection | 64 items | Overflow ends the watch |
| `Clock.settled` (`store/clock.go`), added in #82 | Dolt's address and the database's name | One for each database a store of the process has opened: at most the block of project databases, 999 | Nothing, and about 100 bytes each |
| The write counters (`store/stats.go`) | One for each open store | 256 samples and 60 seconds, fixed arrays | |
| The audit cursor (`server/audit.go`) | | One integer | |
| Idempotency keys | | No memory: they are rows of the event log | |
| Telemetry's projects (`telemetry.go`) | Project id | `projects`, and only ids shaped as UUIDs | Nothing; 16.9 KB each |
| Telemetry's stand-ins (`people.go`) | | No cache: made again for each request | |
| Dolt's metrics (`doltwatch.go`) | The open store | `open_stores`; rebuilt at each read | |
| The keys file's view of `authorized_keys` (`keys.go`) | Fingerprint | The operator's file | Reread when the file changes |
| The span and log queues | | 2,048 each by default, 65,536 at most | Dropped and counted when full |

Two things **hold more than they should**, and neither is a map:

- **A frame decoder never gives a buffer back.** It starts at 64 KiB,
  for requests that are a few hundred bytes, and grows to hold the
  largest frame its connection has sent, up to 4 MiB, for the life of
  the connection. A client chooses the size of its frames, and a
  connection that watches is never closed for being idle. The limits
  that bound it are `conns` and `conns_per_principal`, so it is bounded,
  at 4.3 GB.
- **A socket that has not said hello may send 4 MiB.** A hello is a
  few hundred bytes. The decoder reads up to the largest frame before
  it has one, so a key holder's 16 unanswered handshakes hold 67 MB for
  ten seconds, and can be made again.

Under rule 18 the first is the finding: the cap exists and its cost in
memory is fifty times what the work needs.

## 2. Proposed optimizations

Ranked by what each saves against what it risks. "Measured" means a
prototype outside the repository gave the number; "estimated" means it
follows from a measurement of the code as it is.

### 1. Give the frame decoder a small buffer, and give large ones back

- **Where:** `proto.NewDecoder`, `internal/proto/frame.go:113`, and
  `Decoder.Decode` below it.
- **Now:** 65.5 KB of the 67.5 KB a connection holds; 4.2 MB after one
  large frame; 4.2 MB for a hello that never ends.
- **Change,** in three parts that stand alone:
  - **(a)** Start the scanner's buffer at 4 KiB. One line.
  - **(b)** Read lines through a 4 KiB `bufio.Reader`, gather a longer
    line in a buffer of its own, and drop that buffer when the frame is
    decoded. About thirty lines; `ErrFrameTooLarge` as now.
  - **(c)** Before the handshake is done, refuse a frame over 64 KiB.
    `MaxFrame` means what it did for every frame after the hello.
- **Saving, measured** (`decproto`, [§4.9](#49-the-prototypes)):
  - (a) A new decoder holds 4.3 KB, not 65.7 KB: **61 KB a connection,
    63 MB at 1,024 connections, and as much again for the sockets
    before their hello.** A frame of 60 bytes, of 4 KiB and of 256 KiB
    decodes in the same time with the same two allocations.
  - (b) After a 4 MiB frame a decoder holds 4.3 KB, not 4.19 MB:
    **4.3 GB off the worst case.** A frame that fits the 4 KiB buffer
    costs the same. One that does not costs more while it is read: a
    256 KiB frame takes 284 µs and 1.4 MB against 210 µs and 0.27 MB,
    and a 4 MiB one 4.4 ms against 3.2 ms. Growing the long buffer to
    the line's size in fewer steps would take most of that back; it was
    not tried.
  - (c) Estimated: the 4.3 GB of unanswered hellos becomes 67 MB.
- **Risk:** low. No protocol change: the frames and their limit are the
  same. (b) trades a retained buffer for garbage on large frames, which
  are rare (an issue with long texts, an import). (c) gives the limit a
  second value, to be stated in the protocol's documentation; a hello
  is under 1 KiB, and the bridge frame comes from `starfixd stdio`.
- **Since this was written (14 Oct 2026):** (a) and (b) done together:
  `proto.Decoder` reads through a 4 KiB `bufio.Reader` and gathers a
  longer line in a buffer of its own, doubled exactly and to `MaxFrame`
  at most, which it lets go of once the frame is decoded. Frames,
  errors and the limit are as they were: `TestDecoderMatchesScanner`
  and `FuzzDecoder` (2 million executions run) hold it to the old
  Scanner, at the limit and around it, with a stream read whole, a byte
  at a time and in pieces, ending or breaking mid-line. One call
  differs, which no caller makes: after `ErrFrameTooLarge` the Scanner
  gave the first `MaxFrame` bytes of that line as the next frame, and
  the decoder now gives `ErrFrameTooLarge` again. `BenchmarkRetained`,
  this machine, before and after: an idle connection 67.5 KB to 6.1 KB;
  after a frame just under 4 MiB 4.20 MB to 6.1 KB; before its hello
  66.4 KB to 4.9 KB. A frame over 4 KiB now costs more garbage while it
  is read: steady state, a frame of 8 KiB 10.8 to 12.8 µs and 9.8 KB to
  34 KB, 64 KiB 81 to 100 µs, 1 MiB 1.27 to 1.52 ms, 3.9 MiB 4.9 to
  5.5 ms (minimum of 5); the first frame of a connection is as fast as
  before or faster. (c) was not done: a hello that never ends still
  holds up to 4.2 MB until its timeout. It needs a limit per call in
  the decoder, a refusal whose message does not name 4 MiB, and a line
  in the protocol's documentation, which is more than a small change.

### 2. A bridge that is not the whole of starfixd

- **Where:** `stdio`, `cmd/starfixd/main.go:583`, which runs
  `server.Bridge` (`internal/server/bridge.go:30`).
- **Now:** 9.9 MiB of memory of its own for each connection, and 21 MiB
  resident. The bridge writes one frame and copies two streams, but it
  is the daemon's binary: 20.9 MB, whose packages allocate 2.1 MB in
  21,700 allocations as they initialize, before `main` runs, 1.2 MB of
  it `internal/store` and `internal/proto`.
- **Change:** a second, small binary for the forced command, which
  imports `internal/proto` at most; or `starfixd stdio` kept for
  compatibility and the keys file written to name the small one.
- **Saving, measured:** a bridge of the standard library alone
  (`bridgeproto`, [§4.9](#49-the-prototypes)) is 3.6 MiB of its own
  and 5.7 MiB resident, a 3.1 MB binary: **6.3 MiB less for each
  connection, 6.3 GiB at 1,024.** It is the largest saving here, and
  the only one a small server would notice. `GOGC=25` and
  `GOMEMLIMIT=4MiB` on the bridge as it is made it larger, 11.0 and
  10.8 MiB.
- **Risk:** the most of any proposal, and none of it in the code. A
  second binary to build, sign, install and upgrade
  ([RELEASING.md](../../RELEASING.md)); the upgrade swaps a binary
  while old bridges run, and the bridge frame's rule, that either side
  may be the older, then holds between two files; the AppArmor profiles
  and the forced command in `authorized_keys` name the path
  (`Config.Bridge`); rule 4 asks that the binaries stay few and
  auditable. Measured on macOS only: Linux's figure for the same
  processes was not taken.
- **Short of that:** building what `internal/store` and
  `internal/proto` build at initialization on first use instead would
  take up to 1.2 MB of allocations off every start of every `starfixd`
  process. That figure is from `GODEBUG=inittrace=1`. What the 1.2 MB
  is was not profiled (the two packages compile some two dozen regular
  expressions as package variables), and nor was what removing it does
  to the resident size.
- **Done, 14 October 2026: short of that, measured on Linux.** No second
  binary. Every package-level regular expression of the packages
  starfixd links is compiled on first use (`internal/lazyregexp`):
  they were 1.5 MB of the 2.2 MB that initialization allocated, mostly
  a handful with counted repetitions such as `{0,254}`, and starfix's
  own packages now allocate 24 KB of the 0.67 MB. `stdio` reads only the
  socket from the settings (`server.ResolveSocket`): checking the rest
  compiled 0.4 MiB of expressions more in each bridge of a server with
  a fuller config. `TestInitAllocations` (`cmd/starfixd`) holds what
  initialization allocates to 32 KiB for starfix's packages and 1 MiB
  in all; it counts allocations, which are the same on every run,
  rather than measuring memory, which is not. Nothing else changed:
  the forced command, the bridge frame, the peer check, the keys file,
  the releases and the AppArmor profiles are as they were.

  Measured in Docker on macOS (Linux 7.0, arm64, 16 processors),
  Ubuntu 24.04 with OpenSSH 9.6p1, a real `starfixd serve --dev` and
  sshd with the guide's `Match` block; each bridge connected through
  sshd as alice, bob, carol or dave and idle after its welcome;
  memory from `/proc/PID/smaps_rollup`, averaged over the processes.
  The kernel in Docker runs transparent huge pages `always`; Ubuntu's
  default is `madvise`, under which Go asks for none, so each run was
  also made with THP off for sshd, starfixd and their children
  (`prctl(PR_SET_THP_DISABLE)`, which they inherit):

  | Per connection, 100 connections | THP off, before | THP off, after | THP `always`, before | THP `always`, after |
  |---|---|---|---|---|
  | The bridge, its own (`Private_*`) | 4.7 MiB | 2.5 MiB | 8.6 MiB | 6.0 MiB |
  | The bridge, threads | 8.6 | 7.4 | 8.6 | 7.3 |
  | The shell sshd runs the command with | 0.09 MiB | 0.09 MiB | 0.09 MiB | 0.09 MiB |
  | sshd's two processes (`Pss`; 0.95 MiB is theirs alone) | 2.0 MiB | 2.0 MiB | 2.0 MiB | 2.0 MiB |
  | Kernel: page tables, and 16 KiB of stack a thread | 0.49 MiB | 0.47 MiB | 0.49 MiB | 0.47 MiB |
  | The daemon's share | 30 KB | 40 KB | 70 KB | 75 KB |
  | **The connection on the server** | **7.3 MiB** | **5.1 MiB** | **11.2 MiB** | **8.6 MiB** |

  At 1, 10 and 100 connections the bridge was 5.6, 4.4 and 4.9 MiB
  before and 2.6, 2.6 and 2.5 after with THP off, and 6.7 to 11.4, 8.2
  to 8.8 and 8.6 before and 6.6, 6.0 and 6.0 after with THP `always`; a single
  process moves by a huge page. The machine's memory in use grew by
  11.2 MiB a connection before and 8.4 after with THP off (14.4 and
  12.0 with it on), the client's `ssh` and shell, in the same
  container, included. The daemon idle held 5.6 MiB of its own before
  and 2.7 after. With the fuller config of a server of many projects
  (`control_db`, `project_dbs`, `admins`, `systemd_unit`, `telemetry`)
  the bridge was 4.5 MiB before and 2.5 after with THP off, 8.3 and 6.0
  with it on. `bridgeproto` ([§4.9](#49-the-prototypes)), built again,
  was 1.0 MiB with THP off and 3.5 MiB with it on: a bridge of its own
  would save 1.5 MiB more a connection on Ubuntu's default, and
  2.5 MiB with THP `always`. What the bridge still pays for is the
  dependencies' initialization (0.65 MB: gRPC, Prometheus, the
  OpenTelemetry and protobuf descriptors, among others), which no
  change inside one binary can skip. `GOMAXPROCS=1` set by the bridge
  itself, and a forced collection with the memory given back, made no
  difference that the runs' spread did not hide: the runtime has made
  its threads and spread its heap before `main` runs.

### 3. Read only what a summary needs

- **Where:** `Store.List`, `internal/store/list.go:101`; `loadIssues`
  for `Ready`, `graph.go:247`; `Store.Blocked`, `graph.go:282`, which
  also applies its limit in Go, at `graph.go:298`. Their callers,
  `list`, `ready` and `blocked` in `internal/server/dispatch.go`, keep
  the id, title, status and priority of each issue.
- **Now:** `list` of 50 short issues is 188 KB and 2,078 allocations,
  41 an issue, 835 of them in the store's scan of the rows. With 32 KiB bodies it
  is 1.8 MB, and 17.7 MB for a page of 500; at the texts' limits a
  page of 500 is over 130 MB, for an answer of about 0.1 MB. `Blocked` at 50,000
  issues reads 15.5 MB to answer with 50.
- **Change:** a second column list, of what `proto.Summary` holds, for
  the three reads; and `Blocked`'s limit in its query. The full row
  stays for `show`, `start` and the export.
- **Saving, estimated:** four columns of 26, none of them a text. About
  three quarters of `list`'s allocations with short issues, and all but
  a few percent of its bytes with long ones. `Blocked` becomes flat in
  bytes as `Ready` is.
- **Risk:** low. Two ways to scan an issue where there is one; the
  store's API gains a summary type or the filter an option. No
  protocol change. It is the proposal that takes the largest
  allocation a client can ask for, over 130 MB with one request, down to
  nothing.
- **Since this was written (14 October 2026): done.** `list` reads
  `Store.ListSummaries`, the same pages and cursors as `List` with the
  four columns of a summary and no labels; `List` stays for the export.
  `Ready` and `Blocked` return a `Summary` of each issue. `Blocked`
  counts its page in issues inside the query (`DENSE_RANK` over its
  rows, which keeps an issue's own blockers before an ancestor's), and
  reads the blockers of the page's issues alone; a first try that
  chose the page with `IN (SELECT id FROM blocked)` ran for over 20
  minutes at 50,000 issues and was dropped. On the 100-issue project
  (`BenchmarkOp`, `BenchmarkOpStatements`): `list` 186 KB and 2,075
  allocations to 45 KB and 531, 4 statements and 6 round trips to 1 and
  2, 0.94 ms to 0.22; `ready` 48.8 KB and 630 to 17.3 KB and 257, 6 and
  10 to 5 and 8; `blocked` 69 KB and 731 to 25 KB and 320, 5 and 7 to 4
  and 6, its time the same. A page of 50 (`BenchmarkScale`) is 167 KB
  and 2,090 allocations at 1,000, 10,000 and 50,000 issues before and
  24 KB and 504 after, 1.4 ms to 0.26. With bodies of 32 KiB
  (`BenchmarkLongBodies`) a page of 50 is 1.82 MB and 3.7 ms before and
  24 KB and 0.19 ms after, a page of 500 17.7 MB and 55 ms before and
  0.21 MB and 0.5 ms after; `Ready` 0.49 MB to 0.14 MB; `Blocked` 3.6 MB
  and 8.4 ms to 57 KB and 3.4 ms. `Blocked` at 1,000, 10,000 and 50,000
  issues: 0.38, 3.2 and 15.5 MB before, 57 KB at each after; 3.6, 14
  and 67 ms before, 4.7, 10.5 and 41 ms after.

### 4. A renew that extends nothing should not wait for the writer

- **Where:** `Store.RenewClaims`, `internal/store/claims.go:329`, and
  `Store.TouchAgent`, `internal/store/agents.go:53`, which `renew`
  calls after it (`internal/server/dispatch.go:552`). Both open a write
  transaction, read, and then decide: a lease is extended only when
  less than half of it is left, which seven renews in eight do not
  find, and a session's note is rewritten only when it is a minute
  old.
- **Now:** alone, such a renew is 0.34 ms. Under load it waits for the
  writer twice, behind every commit in progress. In the two-minute soak
  `renew` was a quarter of the requests, its median was 101 ms, and it
  was **577 of the 1,007 seconds the handlers spent waiting for the
  writer**: 301 s in `RenewClaims` and 276 s in the note
  ([§4.6](#46-the-soak)).
- **Change:** read first, on a reader. When no lease is past its half
  and the request brings no paths, answer from the read; when the
  session's note is fresh, skip it. Take the writer only to change
  something.
- **Saving, estimated from those waits:** the claims' half of a
  renew's wait, for the seven in eight that extend nothing, and the
  note's half whenever the note is fresh. The note that is due stays a
  write: a session's notes could be gathered and written once a tick
  by the reaper, which would take a commit a minute for each session
  off the writer, at the price of a `who` that is up to a tick behind.
  The other writes gain little either way, since a write that changes
  nothing holds the writer for a fraction of a millisecond. The soak's
  leases are seconds long, so more of its renews extend than a real
  server's would.
- **Risk:** low to moderate. The decision moves from inside the write
  to a snapshot just before it. A lease with more than half its time
  left cannot run out in between; a claim released or taken in between
  is answered as it was a moment earlier, as a renew that had arrived
  a moment earlier would. `wtx.quiet` and rule 7 are untouched: no
  write is made at all.
- **Since this was written (14 October 2026): done.** `RenewClaims`
  with no paths, and `TouchAgent`, read on a reader first, after
  settling any step of the clock as a write would, and take the writer
  only to change something; the write decides again on its own time.
  A renew with nothing to extend and its note fresh: 6 statements, 8
  round trips and 2 commits of nothing to 2, 4 and none; 10.7 KB and
  178 allocations to 9.1 KB and 152; 0.32 ms to 0.25. The handshake of
  a session whose note is fresh: 3 statements and a commit of nothing
  to 1 and none. In three two-minute soaks of each, renew's median went
  from 84 to 101 ms to 33 to 40 ms, and its wait for the writer from
  659 to 707 s to 272 to 362 s, of which `RenewClaims` 331 to 356 s to
  117 to 168 s. What is left is the soak's: leases of a few minutes on
  a clock 120 times fast, and notes due at nearly every renew. On a real
  clock the note is still mostly due at a renew a minute apart, and
  that write remains; gathering notes into the reaper's tick was not
  done.

### 5. Do not read an event's state where the digest does not use it

- **Where:** `digestQuery.windowed`, `internal/store/digest.go:245`,
  which selects `e.after_state` for all five sections and decodes it
  for two, handoffs and discovered work (`digest.go:280`).
- **Now:** for each issue created, closed or started in the window, the
  whole of the event's state, which for a create is the issue, crosses
  the wire and is copied, up to 10,000 rows a section. `digest` on the
  100-issue project is 333 KB and 3,513 allocations; at 1,667 events in
  the window it is 17.8 MB.
- **Change:** select `NULL` for the state in the three sections that
  never read it.
- **Saving, estimated:** most of the digest's bytes, which are those
  states; the worst case, 10,000 creates of issues with texts of 8 KiB,
  goes from about 330 MB a section to a few MB.
- **Risk:** very low. One query's column.
- **Since this was written (14 October 2026): done.** The closed,
  started and created sections select `NULL` for the state. `digest` on
  the 100-issue project, against the commit before it: 336 KB and 3,527
  allocations to 278 KB and 3,242, with the same 18 statements and 32
  round trips. On `BenchmarkScale`, whose seeded events carry a state of
  17 bytes, 0.30 MB to 0.28 MB at 1,000 issues, 5.6 MB to 5.4 MB at
  10,000 and 16.4 MB to 15.9 MB at 50,000; the rest of those bytes is
  the blocked section, which reads every blocked row to count them. The
  worst case of the estimate, creates of issues with texts of 8 KiB,
  was not measured.

### 6. Make a connection's stand-ins once

- **Where:** `Span.SetWho`, `internal/telemetry/trace.go:197`, and
  `standIn`, `people.go:316`; called by `Server.endRequest`,
  `internal/server/observe.go:92`.
- **Now, with traces on:** two HMAC-SHA-256 for every request, of a
  principal and a session that do not change while the connection
  lasts: 1.95 KB and 23 allocations a request, 45% of the bytes and
  53% of the allocations that tracing adds to a read.
- **Change:** keep the pair with the session, made at its first span
  and again only when the key they are made with changes
  (`Telemetry.RetryKey`).
- **Saving, measured as the share above:** tracing's cost for a read
  goes from 43 allocations to about 20.
- **Risk:** low. The stand-in is a pure function of the key and the
  name, so a kept one is the same one. It is state on the connection,
  not a map a client can add to. With traces off it changes nothing.

### 7. One snapshot for `show`

- **Where:** `show`, `internal/server/dispatch.go:161`: eight calls of
  the store, one after another, the acceptance items reading the issue
  a second time.
- **Now:** 23 statements and 38 round trips, the most of any read, and
  2.9 ms: 15 queries, and the begin and the rollback of four read
  transactions. The parts are read at eight moments, so a `show` can
  pair an issue with a claim taken after the issue was read.
- **Change:** one read of the store that takes them from one snapshot.
- **Saving, estimated:** three transactions not opened and the issue
  read once: about 16 statements and 30 round trips, and 0.4 ms of the
  2.9.
- **Risk:** low to moderate: a new read in the store's API. It makes
  `show` right as well as cheaper.
- **Since this was written (14 October 2026): done.** `Store.ShowIssue`
  reads the issue, its edges, its claim, the latest handoff, the
  acceptance items, the usage and the files on one read-only
  transaction and at one time, and `TestReadsAreOneSnapshot` holds it to
  that; the similar titles still come from the store's cache. `show`:
  23 statements and 38 round trips to 14 and 26, 64 KB and 781
  allocations to 51 KB and 578, 2.6 ms to 2.0.

### 8. Do not rank the ready issues inside the write

- **Where:** `Store.StartIssue`, `internal/store/work.go:48`, which
  calls `rankReady` (`paths.go:535`) on the writer's transaction when
  the request names no issue.
- **Now:** the project's one writer is held for the whole of `Ready`'s
  query: 19 ms at 100 issues, 121 ms at 10,000, 411 ms at 50,000. Every
  other write of the project waits for it.
- **Change:** choose the candidate from a read snapshot, then in the
  write check that this one issue is still ready and unclaimed, and
  choose again if it is not. Or make the query cheaper in Dolt, which
  helps `ready` as well; what Dolt spends its time on in the recursive
  query was not profiled.
- **Saving, estimated:** the writer is held as long as a `start` that
  names its issue: about 10 ms at any size.
- **Risk:** moderate. `start` must still take what `ready` shows first
  (`readyFilter`'s promise), and a write closure must be safe to rerun
  (rule 8); the check inside the write is a query that must agree with
  the ranking. Principle 3 rules out the other way, a stored ready
  list. Worth it only for projects past a few thousand open issues.
- **Since this was written (14 October 2026): done.** `StartIssue`
  with no id ranks the first five ready issues on a read, and the write
  takes the first that is still ready then (`readyNow`, a query over
  the issue and its ancestors that a test holds to `readyFilter` on
  every issue of a project); if none is, it ranks under the writer as
  before. The claim, its epoch and the refusals are the same code. The
  writer held by a start with no id (`BenchmarkScale`, `writer-ms/op`):
  16.9, 49 and 203 ms at 1,000, 10,000 and 50,000 issues before, 9.3,
  10.1 and 13.1 ms after; the check is 0.9 to 1.9 ms of that. The
  request itself is no faster, and at 100 issues is 26 statements and
  43 round trips against 23 and 39.

### 9. Reread the control store only when it has changed

- **Where:** `Server.refresh`, `internal/server/server.go:391`, on
  every tick of the reaper; `Control.load`,
  `internal/store/control.go:426`; and `keyLines`,
  `internal/server/keys.go:183`, which parses every key again to
  compare the keys file.
- **Now:** every 30 seconds the whole index is read and rebuilt: 5.5 ms
  and 3.0 MB for 1,000 principals, 2,000 keys and 4,000 memberships;
  20 ms and 13.2 MB in 257,000 allocations for five times that. At the
  default limits' worst it is about 130 MB each time, with two copies
  alive while it happens.
- **Change:** ask for the control log's last sequence number first, and
  reload only when it has moved. Every write to the control store
  appends an event, `starfixd control init`'s included.
- **Saving, estimated:** all of it, on every tick but the one after a
  change.
- **Risk:** low to moderate. A row changed by hand in SQL would no
  longer be picked up within the half minute, only at the next change
  or restart. That is not a supported way to change access, and the
  guide would say so.

### 10. Let a full write bucket go

- **Where:** `buckets.take`, `internal/server/limits.go:218`.
- **Now:** 131 bytes for every principal in every project that has
  written since the daemon started, 66 MB at the default limits'
  memberships, and more as memberships come and go.
- **Change:** on the reaper's prune, delete each bucket that has
  refilled. A bucket that is full is the same as one that is not there.
- **Saving:** small on any real server. It gives the one map without an
  eviction one.
- **Risk:** very low.
- **Since this was written (14 October 2026): done.** `Server.Prune`,
  every 10 minutes on the reaper, lets go of each write bucket and
  control-write bucket that has filled again. A full bucket is what
  `take` makes for a key it has not seen, so no answer changes; the map
  holds the keys that wrote since the last prune.

### 11. Small things

- **`renew` builds a logger it does not use.**
  `internal/server/dispatch.go:552` calls `pr.log.With(...)` for every
  renew, to name the session on a line written only if the registry
  write fails: 0.7 KB and 12 of its 178 allocations, on the request
  agents send most. Build it where the error is logged.
- **`SimilarClosed` keeps every match to return three.**
  `internal/store/similar.go:127` appends each closed issue that
  matches and sorts them all: 320 KB a call when most of 2,000 titles
  share two words. Keep the best three as it scans. On titles as they
  are in a real project the cost is small.
- **A close makes the next `show` or `create` read 2,000 titles
  again:** 14 ms and 2.2 MB at 10,000 issues. The store could add the
  closed title to the cache it has, since it knows what it wrote
  (`wtx.closedChanged`). Moderate risk for a small gain: left.

## 3. Not worth doing

- **Pooling buffers in the protocol, or another JSON library.** A
  frame's decode and encode are 6 allocations and 1 KB of a request's
  136 and 8 KB, and JSON is 1% of the soak's CPU. Any gain would be a
  dependency (rule 4) or a `sync.Pool` whose bugs are data shown to the
  wrong connection.
- **Preparing each statement once, or sending arguments inline**
  (`InterpolateParams`, `internal/store/store.go:158`). Every
  statement with arguments is a prepare, an execute and a close: two
  round trips where one would do, 15 of `show`'s 38. It would take a
  third of the round trips off a read, about 0.1 ms of `who`'s 0.31,
  and some 30 of its 136 allocations. But a write would gain 3%, since
  its time is the commit; text results are not the binary protocol's
  results, and the store already works around what Dolt sends for a
  `SUM`; and with `database/sql` a statement prepared outside a
  transaction is prepared again inside one unless it was first used on
  that connection, so a cache needs care for the eight readers. Worth
  a spike only if read latency under a millisecond is ever the goal.
- **Committing several writes in one transaction.** It is the only
  thing that would raise the ceiling on a project's writes, and it is
  against rule 5 (one SQL transaction for each operation) and makes
  rule 8's reruns a batch's. Not while 70 writes a second is enough.
- **Tuning the collector** ([§4.7](#47-the-garbage-collector)).
- **The event's JSON, the idempotency hash, the write id and
  `safetext`:** 1 to 2 KB and microseconds a write. The 4.5 MB of an
  update at the texts' limits is the price of S-10's compaction, on a
  write that is rare.
- **Goroutine stacks.** A connection's is 8 KB, a watcher's 3 KB.
- **The acceptance checklist, parsed on every read:** 95 µs at the
  limit of 200 items, a few microseconds for five.

## 4. The evidence

Every command is run from the repository's root with the pinned Dolt
on `PATH` (`export PATH="$(scripts/install-dolt.sh):$PATH"`), without
`-race`.

### 4.1 Method

- **In process.** The daemon's tests run it inside the test binary, and
  so do most of these benchmarks: a real `server.Server` on a real
  store on a real Dolt. Its client is a unix socket and two byte
  slices. It builds each request before the benchmark's clock starts
  and reads the answer into a buffer it keeps, so `-benchmem`'s bytes
  and allocations are the daemon's. Requests that need another first
  (a `finish` needs a `start`) make it with the clock stopped.
- **Retained memory** is `runtime.ReadMemStats` after two
  `runtime.GC()`: `HeapAlloc`, `StackInuse`, and `runtime.NumGoroutine`,
  before and after several hundred of a thing, less the same reading
  for the benchmark's own ends of the sockets.
- **The process** is the binary, built with the release flags, run as
  `starfixd serve --dev` on a `dolttest` database: `ps -o rss=` for
  resident memory, `vmmap --summary` for the physical footprint (the
  memory that is the process's alone; on Linux the benchmark reads
  `Private_*` from `smaps_rollup`), and the daemon's own `/metrics`.
- **Statements** are counted on the wire. `sqlCounter`
  (`internal/server/sqlcount_bench_test.go`) is a unix socket between
  the store and Dolt that passes every byte on and reads the MySQL
  packet headers: a client packet with sequence number 0 is a command,
  and its first byte says which. It also times each command, from its
  last byte sent to the first byte of its answer.
- **Profiles** with every allocation recorded (`-memprofilerate 1`) for
  the short runs that split a request by layer; one allocation in each
  4 KiB (`-memprofilerate 4096`) for the soaks. With every allocation
  recorded, the runtime's small-object batching is off, so such a
  profile counts a few more objects than `-benchmem` does for work on
  the handler's goroutine, and it does not see the handful a request
  causes on `database/sql`'s own goroutines; both are within 15%.

### 4.2 The process

```sh
go test -run '^$' -bench 'Process' -benchtime 2x ./cmd/starfixd/
```

```text
BenchmarkProcess/off-16      2   9.907 bridge-own-MiB   20.97 bridge-rss-MiB   22.90 daemon-200-conns-own-MiB   22.62 idle-rss-MiB   62505 rss-B/conn
BenchmarkProcess/metrics-16  2   9.770 bridge-own-MiB   20.84 bridge-rss-MiB   26.20 daemon-200-conns-own-MiB   87921 go-memory-B/conn   11.29 idle-go-memory-MiB   39.00 idle-goroutines   24.20 idle-rss-MiB   71025 rss-B/conn
```

Each run is two daemons. An earlier run of the same gave 23.63 and
24.30 MiB idle, and 58,982 and 84,582 bytes resident a connection:
resident memory moves by pages, and 200 connections are 12 to 17 MB of
them. `go-memory-B/conn` is the change in `go_memory_used_bytes`, which
the runtime refreshes at most every 15 seconds, so the benchmark waits
16 before it reads.

What initialization allocates, for any `starfixd` command:

```sh
go build -trimpath -ldflags "-s -w" -o /tmp/starfixd ./cmd/starfixd
GODEBUG=inittrace=1 /tmp/starfixd stdio --socket /nonexistent/s --principal alice 2>&1 >/dev/null | grep '^init '
```

218 packages, 2,103,960 bytes in 21,711 allocations: `internal/store`
727 KB in 9,413, `internal/proto` 454 KB in 5,839, `internal/server`
148 KB, `internal/secretscan` 144 KB, and the telemetry SDK with
everything it brings 385 KB in 1,743.

### 4.3 Retained memory

```sh
go test -run '^$' -bench 'Retained' -benchtime 3x ./internal/server/
go test -run '^$' -bench 'SimilarCache|ControlIndex' -benchtime 5x ./internal/store/
```

```text
BenchmarkRetained/conn/idle-16                3   1.000 goroutines/conn      67548 heap-B/conn      8438 stack-B/conn
BenchmarkRetained/conn/after-4MiB-frame-16    3   1.000 goroutines/conn    4196318 heap-B/conn      8110 stack-B/conn
BenchmarkRetained/conn/before-hello-16        3   1.000 goroutines/conn      66376 heap-B/conn      6308 stack-B/conn
BenchmarkRetained/conn/before-hello-4MiB-16   3   1.000 goroutines/conn    4195161 heap-B/conn     -81.92 stack-B/conn
BenchmarkRetained/watcher-16                  3   1.000 goroutines/watcher   837.2 heap-B/watcher   3168 stack-B/watcher
BenchmarkRetained/limiter-keys-16             3   0 goroutines/key           130.9 heap-B/key          0 stack-B/key
BenchmarkRetained/telemetry-project-16        3   0 goroutines/project       16857 heap-B/project      0 stack-B/project
BenchmarkRetainedTelemetry/off-16             3   6.000 goroutines/daemon   163216 heap-B/daemon
BenchmarkRetainedTelemetry/metrics-16         3   6.000 goroutines/daemon   227837 heap-B/daemon
BenchmarkRetainedTelemetry/traces-stuck-16    3   11.00 goroutines/daemon  2955573 heap-B/daemon
BenchmarkRetainedTelemetry/logs-stuck-16      3   15.00 goroutines/daemon  5036483 heap-B/daemon
BenchmarkRetainedStores-16                    3   6.000 goroutines/store-opened   13.00 goroutines/store-read   86378 heap-B/store-opened   1669457 heap-B/store-read   10923 stack-B/store-opened   101035 stack-B/store-read
BenchmarkSimilarCache-16                    322   750659 heap-B/cache   375.3 heap-B/title   2000 titles
BenchmarkControlIndex/principals=1000-16      5   5547108 ns/op   300.6 heap-B/key   187.1 heap-B/member   115.9 heap-B/principal   328.1 heap-B/project   2995900 B/op    53251 allocs/op
BenchmarkControlIndex/principals=5000-16      5  20466483 ns/op   256.8 heap-B/key   164.0 heap-B/member   69.24 heap-B/principal   327.3 heap-B/project  13195372 B/op   257426 allocs/op
```

- `BenchmarkRetainedTelemetry` is a whole server made, and 6,000
  requests answered at `debug`, each ending a span and logging a line,
  against a collector that accepts every export and never answers. The
  `off` row is the server and its store; the others are read against
  it.
- `BenchmarkRetainedStores` counts the one connection that holds each
  store open: take `conn/idle` off the opened figure.
- A stack figure near zero or below it is the runtime's stack spans,
  which are freed in whole spans, moving under the reading.
- The control index is planted, not written through the API: 5,000
  principals with their keys and memberships would be 35,000 writes.
  `ns/op`, `B/op` and `allocs/op` there are one `Control.Reload`.

### 4.4 A request

```sh
go test -run '^$' -bench 'BenchmarkOp$|Handshake|Push|Status' -benchmem ./internal/server/
go test -run '^$' -bench 'OpStatements' -benchtime 100x ./internal/server/
go test -run '^$' -bench 'BenchmarkOp$/off$/who$' -benchtime 1000x -benchmem -memprofile who.mem -memprofilerate 1 -o server.test ./internal/server/
go tool pprof -sample_index=alloc_space -top -cum -show_from='server\.\(\*Server\)\.handle$' -focus='go-sql-driver/mysql' server.test who.mem
```

The table of [§1.4](#14-what-a-request-costs) is the first two
commands' output. The layers are the last two, once for each op and
layer: with `-show_from`, the handler is the root of every stack shown,
so its cumulative figure under a `-focus` is that layer's samples;
`-ignore` takes the layers beneath one out of it. The benchmark's
project is seeded through the store: 60 open issues, 10 of them
blocked, 40 closed, one with a body of 2 KiB and five acceptance items
that `show` reads, 30 memories, and two claims.

The top of `who`'s profile, by bytes and by objects:

```text
      flat  flat%        cum   cum%
 1562.50kB  7.07%  4905.80kB 22.19%  internal/store.queryClaims
 1492.11kB  6.75%  1492.11kB  6.75%  go-sql-driver/mysql.(*mysqlConn).readColumns
  968.81kB  4.38%   968.81kB  4.38%  bytes.Clone
  937.50kB  4.24%  4069.75kB 18.40%  database/sql.(*DB).queryDC
  781.25kB  3.53%  9757.89kB 44.13%  internal/store.(*Store).Who
  625.62kB  2.83%   890.96kB  4.03%  internal/proto.(*Decoder).Decode
     625kB  2.83%      625kB  2.83%  encoding/json/jsontext.NewDecoder
     625kB  2.83%  1282.09kB  5.80%  internal/proto.Response

     24000  7.73%      36000 11.60%  go-sql-driver/mysql.(*binaryRows).readRow
     19495  6.28%      19495  6.28%  go-sql-driver/mysql.(*mysqlConn).readColumns
     14000  4.51%     181643 58.53%  internal/store.(*Store).Who
     12000  3.87%      12000  3.87%  go-sql-driver/mysql.parseBinaryDateTime
     10000  3.22%      10000  3.22%  database/sql.convertAssignRows
     10000  3.22%      85563 27.57%  internal/store.queryClaims
      8001  2.58%       8001  2.58%  database/sql.driverArgsConnLocked
      8000  2.58%      63634 20.51%  database/sql.(*DB).queryDC
```

(These are of a run of 2,000 requests.) With traces on, the same
request's telemetry, 1,000 requests:

```text
 2919.39kB  internal/server.(*Server).endRequest
 1952.58kB  internal/telemetry.Span.SetWho
 1326.95kB  internal/telemetry.standIn
  938.58kB  crypto/hmac.New
  671.97kB  internal/telemetry.(*Telemetry).StartRequest
  583.98kB  internal/server.(*Server).observeRequest
  516.23kB  go.opentelemetry.io/otel/sdk/trace.(*tracer).Start
  406.66kB  go.opentelemetry.io/otel/sdk/trace.(*recordingSpan).End
```

The frame alone, with no server (`BenchmarkFrame…`):

```sh
go test -run '^$' -bench 'Frame' -benchmem ./internal/proto/
```

```text
BenchmarkFrameDecode/show-16              365.8 ns/op   153 MB/s      344 B/op   2 allocs/op
BenchmarkFrameDecode/create-4KiB-16        4198 ns/op  1017 MB/s     5189 B/op   2 allocs/op
BenchmarkFrameDecode/create-4x64KiB-16   244710 ns/op  1095 MB/s   271415 B/op   2 allocs/op
BenchmarkFrameEncode/write-16             472.7 ns/op    59 MB/s      440 B/op   4 allocs/op
BenchmarkFrameEncode/list-50-16           12864 ns/op   474 MB/s    12674 B/op   4 allocs/op
BenchmarkFrameEncode/show-4x64KiB-16     465990 ns/op   575 MB/s   542487 B/op   4 allocs/op
BenchmarkFrameTraceContext/valid-16       19.72 ns/op                   0 B/op   0 allocs/op
```

A request's arguments are copied once out of the line, and an answer's
result is encoded once and then again inside its frame: an issue with
four texts of 64 KiB is 271 KB to read and 542 KB to send.

### 4.5 The store at size

```sh
go test -run '^$' -bench 'Scale|LongBodies|BenchmarkGo' -benchmem ./internal/store/
STARFIX_BENCH_LARGE=1 go test -run '^$' -bench 'Scale/issues=50000' -benchmem ./internal/store/
```

The issues are planted in rows, 500 to a statement, through the
store's writer: 50,000 creates through the API would take a quarter of
an hour. Half are closed, a twentieth deferred, a tenth of the open
ones blocked by another; each has a label and one create event, spread
over the last 30 days, so a digest of 24 hours sees a thirtieth of
them; there are 200 memories, and 20 issues held by other sessions
with paths, made through the API, so that `Ready` ranks by overlap.
The seeded titles share most of their words, which is the worst case
for `SimilarClosed`: nearly every closed title matches.

The tables of [§1.5](#15-how-the-store-grows-with-a-project) are this
output.

**A write, taken apart** (`BenchmarkCommit`): the same transaction on
the writer, with one thing in it at a time, two runs of each:

```sh
go test -run '^$' -bench 'BenchmarkCommit' -benchmem -count 2 ./internal/store/
```

| The transaction | Time | Bytes, allocations |
|---|---|---|
| Changes nothing | 0.06 ms | 0.9 KB, 18 |
| Updates one row, no event | 7.6 and 9.3 ms | 1.2 KB, 28 |
| Inserts one row, no event | 7.1 and 9.4 ms | 1.5 KB, 32 |
| Appends one event | 9.3 and 13.1 ms | 3.7 KB, 61 |
| Updates one row and appends its event | 16.1 and 21.3 ms | 3.9 KB, 73 |

The store's own work for a write is 18 to 73 allocations. The time is
Dolt making a change durable, about the same for any one row; the last
row's times are the machine's load of the moment as much as its second
statement.

### 4.6 The soak

```sh
STARFIX_SOAK_PROFILE=/tmp/soak-2m STARFIX_SOAK=2m GODEBUG=gctrace=1 go test -run 'TestSoakProfile$' \
  -memprofilerate 4096 -cpuprofile /tmp/soak-2m/cpu.prof -blockprofile /tmp/soak-2m/block.prof -blockprofilerate 10000 \
  -mutexprofile /tmp/soak-2m/mutex.prof -o e2e.test -timeout 30m -v ./internal/e2e/
STARFIX_SOAK_PROFILE=/tmp/soak-10m STARFIX_SOAK=10m go test -run 'TestSoakProfile$' -memprofilerate 4096 -o e2e.test -timeout 60m -v ./internal/e2e/
go tool pprof -sample_index=inuse_space -top -base /tmp/soak-10m/heap-early.pb.gz e2e.test /tmp/soak-10m/heap-late.pb.gz
```

`TestSoakProfile` is `TestSoak` with a heap profile and a goroutine
profile taken after two forced collections early in the work, late in
it and at rest, and `runtime/metrics` read every five seconds. **The
soak's binary is not the daemon:** it holds the daemon, the SSH server
before it, 25 sessions' clients, five of them `sfx mcp`, and the
checker's record of everything that happened, which grows for as long
as the run does. So the heap is split by stack: the daemon's is every
sample whose stack passes through `internal/server`, `store`,
`telemetry`, `logs` or `proto` and through none of the test's, the
client's or the SSH server's packages.

**Two minutes**, with the CPU, block and mutex profiles on: 13,763
requests in 121 s, 114 a second, from 25 sessions on two projects.

| | Early (20 s) | Late (2 m 2 s) | At rest |
|---|---|---|---|
| Heap in use, the whole binary | 16.2 MB | 29.2 MB | 29.8 MB |
| of it the test's own | 13.3 MB | 26.0 MB | 26.2 MB |
| of it the daemon's | 2.74 MB | 2.85 MB | 2.92 MB |
| of that, under a connection's handler | 2.08 MB | 2.17 MB | 2.27 MB |
| Goroutines, the whole binary | 542 | 587 | 510 |
| The daemon's: handlers, pushers | 25, 15 | 35, 10 | 25, 25 |
| The daemon's: committers, reaper, pool openers, driver watchers | 3, 1, 10, 20 | 3, 1, 10, 20 | 3, 1, 10, 20 |

**Ten minutes**, with the heap profile alone: 75,759 requests in
600 s, 126 a second.

| | Early (25 s) | Late (9 m 2 s) | At rest (10 m 8 s) |
|---|---|---|---|
| Heap in use, the whole binary | 10.3 MB | 99.2 MB | 113.2 MB |
| of it the test's own | 7.7 MB | 97.5 MB | 110.4 MB |
| of it the daemon's | 2.36 MB | 2.88 MB | 3.96 MB |
| of that, under a connection's handler | 1.75 MB | 2.59 MB | 3.46 MB |
| Goroutines, the whole binary | 537 | 567 | 510 |
| The daemon's: handlers, pushers | 24, 13 | 27, 11 | 25, 25 |
| The daemon's: committers, reaper, pool openers, driver watchers | 3, 1, 10, 16 | 3, 1, 10, 18 | 3, 1, 10, 21 |

- **The binary's heap grows tenfold and the daemon's does not.** The
  growth is the checker's record of the run (`e2e.(*line).push`,
  `e2e.(*monitor).record`) and the clients' own state.
- **The daemon's 1.6 MB between the first and the last snapshot is
  four things, each bounded:** 0.56 MB of closed titles, three stores'
  caches filling toward their 2,000 as the run closes issues; 0.26 to
  0.56 MB of frame decoders, which is four to nine more connections
  alive at the moment of one snapshot than of another, at 64 KiB each;
  0.13 to 0.31 MB of the bridge's copy buffers, since the bridge runs
  inside this binary; and 0.47 MB of strings made by `encoding/json`
  on the request path, with 0.25 MB of the driver's result headers.
  The strings were not traced to what holds them. The JSON package
  keeps a cache of short strings in each of its pooled decoders, at
  most one decoder for each processor, which is the size seen; nothing
  of the daemon's keeps a request's arguments.
- **The profiles sample one allocation in each 4 KiB**, so a count of
  small objects is an estimate, in steps of 256 for a 16-byte object.
  Bytes are what to read; the object counts of the daemon's part,
  8,000 to 18,000, moved by more than they can be trusted for.
- **Goroutines follow connections.** One handler for each connection,
  one pusher for each that watches, and a fixed 30 to 35 for three
  open stores and the control store. At rest every session of the run
  watches, so the pushers equal the handlers. The other 450 goroutines
  are the SSH server's and the clients'.

**Where the CPU went**, two minutes, 26.1 s sampled over 127 s:

| | CPU | Of all |
|---|---|---|
| The test's own: sessions, clients, the SSH server, the checks | 7.70 s | 29.5% |
| The daemon | 5.49 s | 21.0% |
| A connection's handler (`Server.handle`) | 5.19 s | 19.9% |
| of that, in `database/sql` and the MySQL driver | 4.40 s | 16.9% |
| of that, `encoding/json` | 0.29 s | 1.1% |
| of that, the log | 0.02 s | 0.1% |
| The reaper, every 100 ms in the soak | 0.39 s | 1.5% |
| The committers | 0.03 s | 0.1% |
| The collector | 2.50 s | 9.6% |
| System calls, wherever they were made | 8.87 s | 34.0% |

The daemon's handlers used 5.2 s of CPU in 127 s to answer 13,763
requests: 0.38 ms of CPU a request, 4% of one core.

**Where the handlers waited** (`-blockprofile`): 1,094 s in all, of
which 1,007 s in `store.beginWrite`, waiting for the single writer
connection (`database/sql.(*DB).conn`), and 70 s in `presence.ensure`,
a request waiting for its session's note in the registry, which is a
write too. The mutex profile holds the test's own locks and none of
the daemon's.

The wait for the writer, by the op that waited
(`go tool pprof -top -cum -focus='store\.\(\*Store\)\.beginWrite' e2e.test block.prof`):

| Op | Waited for the writer |
|---|---|
| `renew` | 577 s: 301 s for its claims (`RenewClaims`) and 276 s for its session's note (`TouchAgent`) |
| `start` | 130 s |
| `comment` | 48 s |
| `update` | 47 s |
| `finish` | 38 s |
| `create` | 31 s |
| `handoff`, `label.add`, `usage`, `dep.add`, `ack` | 18 to 20 s each |

The session's note waited 605 s in all: `renew`'s 276 s, and the rest
the note each connection makes beside its first request, on a
goroutine of its own. The soak's leases are one to three minutes on a
clock that runs 120 times fast, so its sessions renew every second or
so, far more often than an agent does.

**What a request took**, as the soak's sessions saw it, median and
99th percentile: `inbox` 3 ms and 33 ms; `list` 3 ms and 44 ms; `ready`
9 ms and 546 ms; `create` 41 ms and 319 ms; `comment` 45 ms and
523 ms; `finish` 56 ms and 508 ms; `renew` 101 ms and 797 ms. A
`renew` is a write that changes nothing but a lease, takes 0.5 ms
alone, and was a quarter of the requests: it waits for the writer
behind every commit in progress.

### 4.7 The garbage collector

**The daemon alone.** `BenchmarkOp` with the collector's trace on,
from the built test binary so that no other Go process's trace is in
the output (`dolttest` sends Dolt's to a file):

```sh
go test -c -o server.test ./internal/server/
cd internal/server && GODEBUG=gctrace=1 ../../server.test -test.run '^$' -test.bench 'BenchmarkOp$/off$/who$' -test.benchtime 10s -test.benchmem
```

| Workload | Allocated | Collections | One collection | Live heap, goal |
|---|---|---|---|---|
| `who`, 2,800 a second | 22 MB a second | 11 a second | 0.25 ms of wall time, 1 to 2 ms of CPU | 1 MB, 4 MB |
| `list`, 650 a second | 121 MB a second | 76 a second | The same | 1 MB, 4 MB |
| `comment`, 78 a second | 0.9 MB a second | 1 a second | The same | 1 MB, 4 MB |

The trace's own figure for the collector's share of the process's CPU
is 0% in all three. The daemon's live heap is 1 MB, so the heap's goal
is the runtime's floor of 4 MB whatever `GOGC` says, and a collection
has 1 MB to mark. At 650 `list` a second, which no team sends, the
collector is 9% of one core.

**The soak's binary,** from `runtime/metrics`, first reading to last
(`TestSoakProfile` logs the line):

| Run | Collections | Allocated | Collector's CPU | Pauses |
|---|---|---|---|---|
| 12 s | 13, 1.2 a second | 7.5 MiB a second | 0.12 s of 5.4 s busy: 2.2% | 1.3 ms in all |
| 2 minutes, with the CPU, block and mutex profiles | 158, 1.25 a second | 23.4 MiB a second | 3.8 s of 50.5 s: 7.5% | 316 pauses, 35.8 ms in all |
| 10 minutes | 483, 0.80 a second | 31.2 MiB a second, 290,000 objects | 27.6 s of 267.9 s: 10.3%, and 0.29% of the 16 processors' time | 966 pauses, 118.5 ms in all, none over 5.2 ms |

That binary's heap is the test's: 100 MB of the checker's records by
the end, marked at every collection, and 31 MiB a second allocated of
which the daemon's requests are about a tenth (126 a second at 8 to
190 KB each). Its 10% is the ceiling on what the collector costs with
the daemon in the process, not the daemon's figure.

**No `GOGC` or `GOMEMLIMIT` advice for starfixd belongs in the guide,**
beyond a sentence that it needs neither, so that an operator who has
just set Dolt's does not look for starfixd's:

- Its heap is megabytes. There is nothing for a limit to give back.
- `GOGC` above 100 would save collections on a server that mostly
  reads, at a cost of a few MB. The collections are 1 ms of CPU each
  and under a tenth of a core at a load no team reaches.
- A `GOMEMLIMIT` would not bound the worst case of
  [§1.3](#13-every-limit-as-memory). That memory is live, 4 MiB
  buffers that connections hold, not garbage. A limit below it makes
  the collector run without pause and frees nothing, as the guide
  describes for Dolt. The way to bound it is the decoder
  ([§2](#2-proposed-optimizations), 1), and until then
  `conns_per_principal`.

### 4.8 What load did to the times

The same benchmark with telemetry off, in runs minutes apart:

| | Time in each run |
|---|---|
| `who` | 0.31, 0.33 and 0.36 ms |
| `ready` | 2.7 and 2.8 ms; 8.6 ms later in the first of those runs, with the log at `debug` |
| `comment` | 8.9, 12.5 and 12.8 ms; 30 ms with every allocation profiled while a soak ran beside it |
| `handoff` | 12.1 and 26.4 ms |
| `CreateIssue` at four project sizes, which does not grow | 12.6, 14.3, 15.0 and 19.1 ms |
| A comment pushed to 10 watchers | 17.9 and 38.0 ms |

Bytes and allocations for the same three runs of `who`: 7,856, 7,842
and 7,860 bytes, and 136 allocations each time.

The first runs of `BenchmarkOp` used one project for all five settings
of telemetry, so each setting read the history the one before it had
written, and `digest` went from 0.33 MB to 20 MB across them. The
benchmark now seeds a project for each setting; the table is from the
run after that change.

### 4.9 The prototypes

Both are outside the repository and are not part of this change. They
are small enough to write again from this.

**`decproto`:** three decoders over the same reader. The one as built,
a `bufio.Scanner` with a first buffer of 64 KiB; the same with 4 KiB;
and a `bufio.Reader` of 4 KiB whose `ReadSlice` gives a short line in
place, and whose long lines are appended to a buffer that is dropped
after `json.Unmarshal`. Each decodes one frame of a given size again
and again, and 100 of each are read for what they hold when new and
after one frame of just under 4 MiB:

```text
BenchmarkDecode/body=60/scanner-64KiB(as-built)-16        396.8 ns/op       160 B/op    2 allocs/op
BenchmarkDecode/body=60/scanner-4KiB-16                   394.3 ns/op       160 B/op    2 allocs/op
BenchmarkDecode/body=60/line-reader-4KiB-16               403.3 ns/op       160 B/op    2 allocs/op
BenchmarkDecode/body=4096/scanner-64KiB(as-built)-16       3525 ns/op      4931 B/op    2 allocs/op
BenchmarkDecode/body=4096/scanner-4KiB-16                  3666 ns/op      4931 B/op    2 allocs/op
BenchmarkDecode/body=4096/line-reader-4KiB-16              4582 ns/op     21334 B/op    3 allocs/op
BenchmarkDecode/body=262144/scanner-64KiB(as-built)-16   210261 ns/op    271114 B/op    2 allocs/op
BenchmarkDecode/body=262144/scanner-4KiB-16              200527 ns/op    271055 B/op    2 allocs/op
BenchmarkDecode/body=262144/line-reader-4KiB-16          283871 ns/op   1413004 B/op   15 allocs/op
BenchmarkDecode/body=4194104/scanner-64KiB(as-built)-16  3194433 ns/op   4217224 B/op    2 allocs/op
BenchmarkDecode/body=4194104/scanner-4KiB-16             3273032 ns/op   4217150 B/op    2 allocs/op
BenchmarkDecode/body=4194104/line-reader-4KiB-16         4422227 ns/op  25265348 B/op   34 allocs/op
BenchmarkHeld/scanner-64KiB(as-built)-16     4194505 heap-B/decoder-after-4MiB   65737 heap-B/decoder-new
BenchmarkHeld/scanner-4KiB-16                4194522 heap-B/decoder-after-4MiB    4296 heap-B/decoder-new
BenchmarkHeld/line-reader-4KiB-16               4267 heap-B/decoder-after-4MiB    4265 heap-B/decoder-new
```

**`bridgeproto`:** forty lines of the standard library that dial a
unix socket, write the bridge frame and copy stdin and stdout, built
with the release flags, beside `starfixd stdio`, both connected to the
same listener that holds its connections:

```text
bridgeproto: 3.1 MB binary, 5,680 KiB resident, physical footprint 3,553 K
starfixd stdio: 20.9 MB binary, 21,056 KiB resident, physical footprint 9,873 K
starfixd stdio with GOGC=25: 22,912 KiB resident, physical footprint 11.0 M
starfixd stdio with GOMEMLIMIT=4MiB: 24,432 KiB resident, physical footprint 10.8 M
```

## 5. The benchmarks added

All are new files. They run only with `-bench` (the soak profile only
with `STARFIX_SOAK_PROFILE`), skip without `dolt` as the tests beside
them do, make their databases through `dolttest` and drop them, and
remove what they write.

| File | Benchmark | Measures | Run |
|---|---|---|---|
| `internal/server/op_bench_test.go` | `BenchmarkOp` | Each op end to end in the daemon, for each of five settings of telemetry (`off`, `metrics`, `traces`, `logs`, `debug`) | `go test -run '^$' -bench 'BenchmarkOp$/off' -benchmem ./internal/server/` |
| | `BenchmarkOpStatements` | Each op's statements, round trips, prepares and commits, and its wait on Dolt | `-bench 'OpStatements$' -benchtime 100x` |
| | `BenchmarkOpStatementsBatch` | The same for requests that carry 1 and 21 labels, 1 and 101 usage records, 0 and 5 discovered issues, 2 and 12 claims | `-bench 'OpStatementsBatch' -benchtime 20x` |
| | `BenchmarkHandshake` | A connection from its first frame to its end | `-bench 'Handshake' -benchmem` |
| | `BenchmarkPush` | A write whose event goes to 0, 1, 10 and 100 watchers | `-bench 'Push' -benchmem` |
| `internal/server/retained_bench_test.go` | `BenchmarkRetained` | What a connection, a watcher, a write bucket and a project's metric series hold | `-bench 'Retained$' -benchtime 3x` |
| | `BenchmarkRetainedTelemetry` | What telemetry holds, its queues at their bounds | `-bench 'RetainedTelemetry' -benchtime 3x` |
| | `BenchmarkRetainedStores` | What an open project store holds, opened and read | `-bench 'RetainedStores' -benchtime 3x` |
| | `BenchmarkStatus` | The status report and a handshake, on a server of many projects | `-bench 'Status' -benchmem` |
| `internal/server/sqlcount_bench_test.go` | | `sqlCounter`, the proxy the statement counts come from | |
| `internal/store/scale_bench_test.go` | `BenchmarkScale` | The store's reads and writes at 100, 1,000 and 10,000 issues; 50,000 with `STARFIX_BENCH_LARGE=1` | `go test -run '^$' -bench 'Scale' -benchmem ./internal/store/` |
| | `BenchmarkLongBodies` | The reads that return issues, with 32 KiB bodies, and an update at the texts' limits | `-bench 'LongBodies' -benchmem` |
| | `BenchmarkGo` | The store's work that never reaches Dolt, at its limits | `-bench 'BenchmarkGo' -benchmem` |
| | `BenchmarkSimilarCache` | What the closed-title cache holds | `-bench 'SimilarCache'` |
| | `BenchmarkCommit` | A write taken apart: nothing, an update, an insert, an event, an update with its event | `-bench 'BenchmarkCommit' -benchmem` |
| `internal/store/control_bench_test.go` | `BenchmarkControlIndex` | The control index for each principal, key, project and membership, and one reload | `-bench 'ControlIndex' -benchtime 5x` |
| `internal/proto/frame_bench_test.go` | `BenchmarkFrameDecode`, `BenchmarkFrameEncode`, `BenchmarkFrameTraceContext` | A frame alone | `go test -run '^$' -bench 'Frame' -benchmem ./internal/proto/` |
| `cmd/starfixd/process_bench_test.go` | `BenchmarkProcess` | The real process: resident memory, its own account, a connection, a bridge. Needs `go` and `ps`; skipped on Windows | `go test -run '^$' -bench 'Process' -benchtime 1x ./cmd/starfixd/` |
| `internal/e2e/soak_profile_test.go` | `TestSoakProfile` | The soak with heap and goroutine profiles early, late and at rest, and the collector's figures | [§4.6](#46-the-soak) |

`internal/server/bench_test.go` already had a `BenchmarkRequest`, for
what tracing costs a request (design §16.6); it is unchanged, and
`BenchmarkOp` does not replace it.

The retained-memory benchmarks report custom metrics (`heap-B/conn`)
and set `ns/op` to zero: their time is setup. One iteration is one
reading.

## 6. Not measured

- **Linux.** Everything here is macOS on arm64. The heap figures and
  the counts will be the same on Linux; resident memory, the bridge's
  size and every time will not. The commit is where it matters most:
  stage 0 measured 233 writes a second for one writer on Linux, 4.3 ms
  each ([spike/dolt/RESULTS.md](https://github.com/ariesworx/aries/blob/fbbbf4655665845db81a72d2bbd1845472fd1c5a/spike/dolt/RESULTS.md)), against
  9 to 15 ms here. Whether the difference is the operating system's
  durable write, this machine's load or Dolt's version was not looked
  into, and nor was what Dolt does in a commit.
- **A quiet machine.** No time here is from one.
- **A real team's project.** The seeded issues are of one shape; the
  seeded titles are the worst case for the similar-title scorer.
- **`cost`, `hours`, `plans`, `prices`, `accept`, `close`, `reopen`,
  the dependency and label ops, `ack`, `forget`, `pin`, and every
  `sfx admin` op but `status`.** `cost` reads the whole of the prices
  and plans tables and attributes usage in Go (`attribution.go`), which
  is the largest piece of the store this profile did not open.
- **The reaper's tick on a server of many projects with many keys:**
  the reload was measured, the keys file's rendering was not.
- **`import-bd` and `export-bd`,** which only the operator runs.
- **The telemetry listener** under requests, and what one Prometheus
  scrape allocates.
- **What Dolt spends `ready`'s time on.** The query was timed, not
  profiled.
- **sshd's own processes** for each connection, which are beside the
  bridge and larger than nothing.
- **A daemon over days.** Ten minutes of soak show no growth; they
  cannot show a slow one.
- **The `-blockprofile` and `-mutexprofile` of the control index.**
  The soak's mutex profile holds only the test's own locks; nothing of
  the daemon's was contended enough to appear.
