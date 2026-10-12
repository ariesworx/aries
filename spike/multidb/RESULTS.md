# starfix stage 3a: many databases in one Dolt

**Verdict: one Dolt server can hold a database per project, and its memory is set by what has been written since Dolt last started, not by the data it holds.** A project's database can be created by starfixd's own least-privileged account, provided root granted it that database's name beforehand. Writing to a project costs memory that closing its store does not give back and a restart of Dolt does: 58 to 86% of it at rest, and a half to three quarters once the projects are in use again. Dolt's own garbage collection (`dolt gc`) and a restart bring a server back to 110 to 200 MiB whatever it holds, up to the 30,000 issues measured. A Go memory limit (`GOMEMLIMIT`) of 0.6 of what Dolt would otherwise take cost at most twice the CPU; one of 0.5 cost up to 25 times, and lower ones up to a hundred. Writers in different databases do not starve each other, and throughput stops growing between 5 and 10 of them.

Two sets of measurements, on two systems. Tell them apart by section:

| Sections | System | Date |
|---|---|---|
| [macOS](#macos-11-october-2026): 1 (grants), 2 (memory) | macOS 27 on an Apple M4 Max, Dolt on the host | 11 October 2026 |
| [Linux](#linux-12-october-2026): 3 to 8 (the same table, restart, memory limit, concurrent writers, larger projects, how the readings agree) | Linux 7.0 arm64 in a container, in Docker Desktop's VM on the same Mac | 12 October 2026 |

Code: this directory. With `dolt` at `version.Dolt` on `PATH`, `go run ./spike/multidb -exp grants`, `-exp table`, `-exp restart`, `-exp limit` or `-exp concurrent`; the header of `main.go` lists each experiment's flags. It answers open questions 1 and 2 of [the design, §15.5](../../docs/design/starfix.md#155-open-questions).

## macOS, 11 October 2026

These are the first spike's results, as written then. Section 2's reading of them is corrected by the Linux measurements in three places, each marked *(see section N)*: the figures themselves stand, and Linux reproduced them (section 3).

### Environment

| | |
|---|---|
| Dolt | 2.4.2, started by `internal/dolttest`: loopback and a unix socket, `secure_file_priv` set, default settings otherwise, fresh data directory |
| Account | `starfix@localhost`, as `dolttest` makes it: every right on the databases it is granted, none on `*.*` |
| Machine | Apple M4 Max, 16 cores, 64 GB RAM, macOS 27; client and server on the same machine, over the socket |
| Client | Go 1.27.1, `internal/store` as starfixd uses it (`store.Open` with default options: one writer, a commit a second) |
| Memory | Resident set size of the `dolt sql-server` process, from `ps`, read 5 seconds after each step |

The grants were also run at Dolt 2.3.3, with the same results.

### 1. Who can create a database

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

### 2. Memory

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

- **Memory follows the data written, not the number of databases.** The same writes cost about the same in one database as in five. A database that is merely open, or merely exists, adds nothing measurable. *(See section 7: true at this size only. The same issues spread over more databases cost up to three times as much, and it is the writing since Dolt started that costs, not the data.)*
- **Closing stores returns nothing.** Dolt kept all of it with one store open. An idle close in starfixd saves connections, not memory.
- **About half is garbage the Go runtime had not collected.** A memory limit halved the figure, but Dolt went past the limit, so the rest is in use, and writes took four times as long while the collector worked to hold it. A limit set well above what is in use should cost less; that was not measured. *(See section 5: measured. 512 MiB was about 0.2 of what that server would have taken, far below where a limit starts to cost.)*
- **It is a lot of memory for the data:** about 100 MiB for a project of 300 issues without a limit, 50 MiB with one. That is Dolt's, and a single-project server pays it too (the load test before this one reached 1.4 GB). *(See section 4: a restart gives most of it back.)*
- **Opening is cheap.** The first open of a new database, running all 15 migrations, took 220 to 250 ms; reopening one already migrated, 3 ms.
- **An idle store holds one connection.** The reader pool fills only under load.
- **Writes did not slow as stores were added**: 7 ms each with 1 open or 50.

## Linux, 12 October 2026

### Environment

| | |
|---|---|
| Dolt | 2.4.2 for linux/arm64, from the image `deploy/docker/Dockerfile` builds, started by `internal/dolttest` as on macOS, with its metrics endpoint on (`metrics:` in its config) and, for section 5, `GOMEMLIMIT` in its environment |
| Kernel | Linux 7.0.14-linuxkit, aarch64: Docker Desktop's virtual machine (Docker 29.8.2), with 16 CPUs, 7.75 GiB of memory (`MemTotal` 7,933 MiB) and 1 GiB of swap |
| Container | Ubuntu 24.04, as root, one run at a time. No CPU limit. A memory limit on every run (`--memory`, with `--memory-swap` equal to it, so no swap): 4, 5 or 6 GiB, named where it mattered. Dolt's data in the container's own filesystem (overlay on the VM's disk) |
| Host | The same Apple M4 Max, 16 cores, 64 GB, macOS 27. **Shared:** other agents ran Go test suites and a virtual machine on it throughout |
| Client | The spike built for linux/arm64 with Go 1.27.1, in the same container as Dolt, over the socket; `internal/store` with default options |
| Memory | Read 5 seconds after each step, three ways, printed side by side: `/proc/PID/status` of `dolt sql-server` (`VmRSS`, `RssAnon`, `RssFile` plus `RssShmem`, `VmSwap`); the container's cgroup (`memory.current`, `memory.stat`, `memory.events`); and Dolt's own `process_resident_memory_bytes` and `go_memstats_*`. "Dolt memory" below is `VmRSS`. Section 8 says how they agree |
| Data | As on macOS: an issue is four writes, with a 540-byte body and comment. Synthetic, one shape, every issue left in progress |

What the shared machine does to these figures: **memory repeated to within a few percent** between runs and is reported as measured. **Latency and throughput did not repeat.** The same single-store load had a median write of 2.0 ms in a quiet quarter of an hour and 10 to 12 ms in a busy one, and one run of five writers ran at 295 writes a second and the next at 773. Timing figures below give every run or the range, and say which runs were quiet; nothing is concluded from a difference smaller than that spread. Dolt's CPU per write did repeat (2.0 to 3.6 ms without a limit, in every run), so the cost of a memory limit is given in CPU, not in latency.

The spike prints every figure here; its raw output is not in the repository. No run was killed by the kernel: all 209 readings show `oom_kill` at 0. One run was stopped by hand, the first attempt at section 5's matrix, when limits of 320 and 400 MiB turned out to take over 8 minutes a run; its three finished runs are in that table.

### 3. The macOS table again

`-exp table`, in a 6 GiB container: stores of 300 issues opened and loaded one after another.

| Open stores | Dolt memory, Linux | macOS (section 2) | Connections |
|---|---|---|---|
| 0, server started | 81 MiB | 86 MiB | 0 |
| 1 | 311 MiB | 336 MiB | 1 |
| 5 | 1,039 MiB | 1,072 MiB | 5 |
| 20 | 2,573 MiB | 2,540 MiB | 20 |
| 50 | 5,228 MiB | 5,117 MiB | 50 |
| 50, idle for 30 seconds | 5,229 MiB | 5,118 MiB | 50 |
| 1, the other 49 closed, a minute later | 5,241 MiB | 5,118 MiB | 1 |
| 50, reopened | 5,383 MiB | 5,118 MiB | 50 |

- **Linux agrees with macOS** to within 8% at every step. Other Linux runs of the same loads, eleven at each size, gave 1,021 to 1,074 MiB at 5 stores and 2,511 to 2,728 MiB at 20.
- **Closing stores returns nothing** on Linux either, and reopening the 49 added 142 MiB.
- **Writes were faster than on macOS, which says nothing about Dolt:** a median of 2.4 to 3.1 ms and a 99th percentile of 7 to 16 ms, against a mean of 7 ms there. The disks differ, and so may what a sync to them costs.
- **Opening:** 38 to 88 ms for a first open with its migrations, 3 to 15 ms for a reopen.
- **At 50 stores the container was at its limit** (cgroup 6,139 of 6,144 MiB, 914 MiB of it page cache), and one write took 4.7 seconds. Nothing was killed. See section 8 on what a container's figure includes.

### 4. Restarting Dolt on the same data

`-exp restart -gc`: load, close every store, stop Dolt, start it on the same data directory, then open every store, read, and write a little. A round of reads is, for each store, `ready`, a page of `list`, `blocked`, and `show` with its comments for 50 issues: 103 reads. Dolt memory, in MiB:

| Step | 5 stores of 300 issues | 20 stores of 300 | 4 stores of 3,000 |
|---|---|---|---|
| Loaded | 1,021 | 2,511 | 1,934 |
| Every store closed | 1,021 | 2,513 | 1,936 |
| **Restarted, at rest** | **176** | **342** | **398** |
| Every store open again | 178 | 446 | 432 |
| After one round of reads | 229 | 608 | 541 |
| After three rounds | 240 | 658 | 638 |
| After 10 more issues a store (40 writes each) | 274 | 779 | 642 |
| 30 seconds later | 274 | 779 | 642 |
| Data directory on disk | 178 | 707 | 1,881 |
| After `CALL DOLT_GC()` in each database, stores closed | 316 | 525 | 426 |
| Data directory on disk after it | 6 | 15 | 116 |
| **Restarted again, at rest** | **131** | **163** | **153** |
| Opened, read and 10 more issues a store | 250 | 664 | 299 |

- **A restart gives back most of it.** At rest Dolt held 14 to 21% of what it held before the restart, and after every store was opened, read from and written to again, 27 to 33%. Dolt took 0.1 to 0.8 seconds to accept connections again; a store reopened in 3 to 6 ms.
- **So the memory of sections 2 and 3 is what Dolt accumulates while it writes,** not what the data needs. After the restart, memory followed what was done next: reading added 60 to 210 MiB, and 40 writes a store added up to 120 MiB across 20 stores.
- **What a restart leaves depends on Dolt's files, not on the issues.** The data directory was 28 to 56 KiB for each write made, which `dolt gc` made 7 to 47 times smaller. After it and a second restart, Dolt was at 131 to 163 MiB at rest in all three, and at 111 to 195 MiB for a single project of any size measured (section 7).
- **`dolt gc` alone, without the restart, freed the Go heap but not yet the process:** in the 20-store run the heap in use fell from 651 to 139 MiB while resident memory fell only from 779 to 525 MiB in the 5 seconds before the reading.
- **Once, Dolt shrank its own files.** In the first of two runs of one 3,000-issue project, the data directory went from 465 to 16 MiB during the 40 writes after the restart, with no `dolt gc` asked for, and one of those writes took 18.7 seconds. Dolt's automatic garbage collection (`behavior.auto_gc_behavior`, enabled in the sample configuration its help prints) is the likely cause, but its log was not kept for that run. It did not happen in the second run, whose log was kept, or in the four-store run, and what sets it off was not found.

### 5. A Go memory limit

`-exp limit`: a fresh Dolt under `GOMEMLIMIT`, ten stores opened, then 300 issues loaded into each, one store after another: 12,000 writes. Without a limit this load ends with Dolt at 1,672 to 1,690 MiB and `go_memstats_next_gc_bytes`, the heap size at which the collector next runs, at 1,609 to 1,638 MiB. CPU is Dolt's `process_cpu_seconds_total` over the load; cycles are `go_gc_duration_seconds_count`.

| `GOMEMLIMIT` | Of the unlimited `next_gc` | Dolt memory at the end | CPU a write | Against no limit | Collector cycles | Write p50, p99 | Load took |
|---|---|---|---|---|---|---|---|
| none (3 runs) | | 1,672 to 1,690 MiB | 3.0 to 3.4 ms | 1 | 43 to 46 | 8 to 12 ms, 28 to 35 ms | 1m59s to 2m28s |
| 1,600 MiB (2 runs) | 0.99 | 1,562, 1,569 | 3.6, 2.7 | 1 | 44, 48 | 16.0, 38 and 3.1, 14 | 3m22s, 45s |
| 1,280 MiB (2 runs) | 0.79 | 1,272, 1,264 | 3.2, 2.7 | 1 | 54, 55 | 3.9, 49 and 3.0, 13 | 2m06s, 44s |
| 960 MiB (2 runs) | 0.59 | 972, 968 | 3.2, 3.4 | 1 | 100, 98 | 3.0, 15 and 3.2, 28 | 44s, 58s |
| 900 MiB | 0.55 | 923 | 5.7 | 1.7 | 220 | 13.5, 37 | 2m49s |
| 850 MiB | 0.52 | 891 | 29.6 | 9 | 1,762 | 13.8, 95 | 3m08s |
| 800 MiB (2 runs) | 0.49 | 871, 857 | 47.9, 44.5 | 14 to 15 | 3,439, 3,090 | 17.1, 73 and 4.6, 76 | 4m03s, 1m53s |
| 640 MiB (2 runs) | 0.39 | 791, 787 | 133, 119 | 36 to 41 | 11,343, 10,652 | 4.8, 113 and 3.5, 88 | 3m55s, 3m20s |
| 480 MiB | 0.30 | 712 | 199 | 61 | 23,724 | 21.5, 99 | 5m23s |
| 400 MiB | 0.25 | 643 | 296 | 91 | 39,808 | 40.1, 129 | 8m21s |
| 320 MiB (2 runs) | 0.20 | 554, 557 | 320, 342 | 98 to 105 | 54,855, 57,295 | 40.9, 115 and 46.2, 120 | 8m20s, 9m26s |
| 128 MiB | 0.08 | 373 | 236 | 72 | 67,341 | 25.5, 72 | 5m39s |

The same question at half the data and at twice:

| Load | `GOMEMLIMIT` | Of the unlimited `next_gc` | Dolt memory at the end | CPU a write | Against no limit | Collector cycles |
|---|---|---|---|---|---|---|
| 5 stores of 300 | none | | 1,041 MiB (`next_gc` 998) | 2.4 ms | 1 | 44 |
| | 600 MiB | 0.60 | 624 | 3.0 | 1.3 | 61 |
| | 500 MiB | 0.50 | 540 | 3.5 | 1.5 | 109 |
| | 64 MiB | 0.06 | 239 | 120 | 51 | 31,437 |
| 20 stores of 300 | none | | 2,604 MiB (`next_gc` 2,537) | 3.0 ms | 1 | 49 |
| | 1,500 MiB | 0.59 | 1,486 | 6.0 | 2.0 | 268 |
| | 1,250 MiB | 0.49 | 1,431 | 73.5 | 25 | 6,302 |

- **There is a cliff, and it is narrow.** At ten stores a limit of 960 MiB cost nothing that could be measured and held Dolt to 58% of what it would have taken; 900 MiB cost 1.7 times the CPU, 850 MiB nine times and 800 MiB fourteen. Below that Dolt used 7 to 8 of the 16 cores throughout, nearly all of it for its collector, and memory still went past the limit, because the limit is a target the Go runtime gives up on rather than a cap.
- **A limit too low does not fail; it burns CPU.** No run was refused memory or killed. Writes kept succeeding. Their 99th percentile was 72 to 129 ms in every run at 850 MiB or less, against 13 to 49 ms in the runs at 960 MiB or more; their medians overlap (3.5 to 46 ms against 3.0 to 16 ms), because the machine was shared. So the CPU column is the evidence, and the latency columns only agree with it.
- **The observable: `go_memstats_next_gc_bytes` of a Dolt running without a limit.** In all three loads a limit of 0.6 of it cost at most twice the CPU (1.0, 1.3 and 2.0 times), and at ten stores 0.8 of it cost nothing. A limit of 0.5 of it cost 1.5 times at five stores, 14 times at ten and 25 times at twenty. So: no lower than 0.6 of it, and 0.75 for room. Resident memory without a limit ran 3 to 4% above `next_gc` in these runs, so 0.6 to 0.75 of that is the same rule for someone without Dolt's metrics.
- **It is a moving target.** What Dolt holds grows as it writes (sections 3 and 7), so a limit with room today is at the cliff after enough writes, and a restart moves it back. A limit does not replace the restart; it turns running out of memory into running out of CPU.
- **What to watch under a limit:** the collector's cycles against the writes made. Every run that cost at most twice the CPU, and every run without a limit in section 6, made fewer than 3 cycles for each 100 writes; every run past the cliff made more than 14, and the lowest limits several for each write. Resident memory against the limit separated them less well: it was at most 4.0% over the limit in the runs that cost at most twice, and from 4.8% to nearly three times the limit past the cliff, but 8% over in the five-store run that cost only 1.5 times. `go_memstats_next_gc_bytes` against the limit did not: it was 76 to 87% of the limit in runs that cost nothing and 83 to 103% in runs that did.
- **The first spike's 512 MiB for 20 stores** was 0.2 of the `next_gc` the same load reached here, deep in the last rows of the table, which is why it cost four times the latency.
- **The collector's own CPU could not be read.** Dolt 2.4.2 exposes no `go_cpu_classes_*`; `go_gc_duration_seconds` is its pauses only, which stayed under 6 seconds in total even in the worst run. The CPU column is the whole process.

### 6. Projects written to at the same time

`-exp concurrent`: N stores on a fresh Dolt, each loaded with 300 issues (1,200 writes) by its own writer, all at once. The run of 15:24 UTC was the quiet one: its three repeats agree to within 10% in median latency and, but for the single store, 7% in throughput.

| Stores written at once | Write p50 | Write p99 | Writes a second, all stores | Dolt CPU, cores | CPU a write |
|---|---|---|---|---|---|
| 1 | 2.0 to 2.2 ms | 4.1 to 5.9 ms | 407 to 493 | 0.93 to 0.99 | 2.0 to 2.3 ms |
| 2 | 2.3 to 2.4 | 4.7 to 4.8 | 798 to 831 | 1.6 to 1.7 | 2.0 to 2.1 |
| 5 | 2.9 to 3.0 | 6.7 to 8.4 | 1,531 to 1,632 | 3.4 to 3.6 | 2.2 |
| 10 | 4.2 to 4.3 | 10.8 to 11.9 | 2,111 to 2,148 | 5.6 to 5.7 | 2.6 |
| 20 | 7.0 to 7.2 | 21.0 to 22.9 | 2,445 to 2,574 | 7.1 to 7.3 | 2.8 to 3.0 |

The same matrix at two other times, on a busier machine, with every run's figure:

| Stores | 15:00 UTC: p50, ms | Writes a second | 16:21 UTC: p50, ms | Writes a second |
|---|---|---|---|---|
| 1 | 10.0, 11.8, 12.0 | 91, 89, 85 | 2.4, 2.7, 2.3 | 333, 326, 261 |
| 2 | 16.2, 6.7, 6.5 | 116, 233, 232 | 2.6, 2.7, 2.6 | 684, 504, 704 |
| 5 | 11.1, 11.1, 5.1 | 331, 295, 773 | 3.0, 3.0, 3.4 | 1,509, 1,405, 1,285 |
| 10 | 4.7, 6.0, 4.5 | 1,731, 1,291, 1,243 | 7.2, 4.8, 5.0 | 981, 1,881, 1,813 |
| 20 | 8.2, 7.6, 31.0 | 1,580, 415, 457 | 7.8, 7.2, 8.3 | 2,260, 2,288, 1,549 |
| 40 | | | 24.7, 48.3, 22.4 | 412, 557, 1,101 |
| 40, of 100 issues each (16:45 UTC) | | | 18.8, 14.0, 12.4 | 1,548, 2,102, 2,773 |

- **Writers in different databases do slow each other, a little up to 5 and plainly from 10.** In the quiet run a write's median went from 2.0 ms alone to 2.9 ms among 5, 4.3 ms among 10 and 7.1 ms among 20. No store was starved: the slowest store's median was within 2% of the pooled one in every quiet run.
- **The knee in throughput is between 5 and 10 writers.** It nearly doubled from 1 to 2 and again from 2 to 5, then grew 36% to 10 and 17% to 20. The most this Dolt did, on this machine, in any run was 2,400 to 2,800 writes a second.
- **It is not the CPU that runs out.** At 20 writers Dolt used 7 of 16 cores, and its CPU a write rose only from 2.0 to 2.9 ms. Something the databases share is the limit; whether that is inside Dolt or the one disk was not found.
- **The 40-store runs of 300 issues say nothing about Dolt.** They filled the container (cgroup 5,731 to 5,973 of 6,144 MiB) and ran at a sixth to a half of the 20-store rate. With 100 issues a store, at under half the limit, 40 writers did 1,548 to 2,773 writes a second: the same ceiling, with each write's median at 12 to 19 ms. That run was itself on a busy machine: its 10 stores did 487 to 2,132 writes a second and its 20 did 1,001 to 1,480.
- **The busy runs show the spread, and no more.** At 15:00 one writer alone managed 85 to 91 writes a second, a fifth of the quiet figure, with the machine doing other work. A knee could not be read from those runs.
- **For scale:** stage 0 put a busy team of agents at about 25 writes a second.

### 7. Larger projects

`-exp restart -stores 1 -gc` with 300, 3,000 (twice) and 30,000 issues in one project. Dolt memory, in MiB:

| | 300 issues | 3,000 issues (two runs) | 30,000 issues |
|---|---|---|---|
| Writes | 1,200 | 12,000 | 120,000 |
| Loaded | 304 | 1,140; 1,130 | 2,107 |
| For each issue | 1.0 | 0.38 | 0.07 |
| Data directory on disk | 33 | 465; 460 | 6,608 |
| Restarted, at rest | 110 | 175; 163 | 895 |
| Store open again | 114 | 183; 167 | 897 |
| After three rounds of reads | 134 | 226; 235 | 1,032 |
| After 10 more issues | 144 | 290 (see section 4); 236 | 1,035 |
| A read after the restart | 0.6 to 0.8 ms | 1.1 to 1.7 ms | 8.2 to 8.5 ms |
| `CALL DOLT_GC()` took | 0.08 s | 0.05 s; 0.36 s | 13.4 s |
| Data directory on disk after it | under 1 | 14; 14 | 997 |
| Restarted again, at rest | 111 | 123; 121 | 195 |
| Opened, read and 10 more issues | 147 | 176; 169 | 294 |

As the 30,000-issue project loaded, in a 6 GiB container:

| Issues written | 2,500 | 5,000 | 10,000 | 15,000 | 20,000 | 25,000 | 30,000 |
|---|---|---|---|---|---|---|---|
| Dolt memory, MiB | 1,119 | 1,225 | 1,387 | 1,631 | 1,689 | 2,081 | 2,107 |
| Data directory, MiB | 371 | 828 | 1,850 | 2,994 | 4,182 | 5,385 | 6,608 |

Every load, by how the issues were spread (Dolt memory when loaded, before any restart):

| Issues in all | In one database | In several |
|---|---|---|
| 1,500 | 917 MiB (macOS) | 1,021 to 1,074 MiB in 5 |
| 3,000 | 1,130 to 1,140 MiB | 1,672 to 1,752 MiB in 10 |
| 1,000 | | 709 to 715 MiB in 10 |
| 2,000 | | 1,323 to 1,365 MiB in 20 |
| 4,000 | | 2,502 to 2,651 MiB in 40 |
| 6,000 | 1,225 MiB at 5,000 and 1,366 MiB at 7,500 | 2,511 to 2,728 MiB in 20 |
| 12,000 | 1,608 MiB at 12,500 | 1,934 MiB in 4; 4,465 to 4,806 MiB in 40 |
| 15,000 | 1,631 MiB | 5,228 MiB in 50 |

- **Memory does not grow in step with a project.** A hundred times the issues cost seven times the memory. A project reached about 1.1 GiB within its first 2,500 issues and then added about 36 MiB for each further thousand.
- **It does grow in step with the number of projects written to,** which the macOS comparison, of 1,500 issues in one database and in five, was too small to show. Fifty projects of 300 issues cost 5.1 GiB where one project of the same 15,000 issues cost 1.6 GiB. The first project of 300 issues cost about 230 MiB over an empty server; each further one added about 180 MiB up to 5 projects, 135 MiB from 5 to 10 and 95 MiB from 10 to 40. Each further project of 100 issues added about 60 MiB, from 10 to 40.
- **A restart gave back 58 to 86%,** less for the largest project: at 30,000 issues Dolt restarted at 895 MiB, with 6.5 GiB of files behind it. After `dolt gc` those files were 1 GiB and Dolt restarted at 195 MiB, and at 294 MiB once the project was open, read and written to.
- **Disk is the other cost.** Dolt wrote 28 KiB to its data directory for each write in a 300-issue project and 56 KiB in a 30,000-issue one, for an issue whose text is about 1 KiB: 220 KiB of disk an issue. `dolt gc` made it 7 to 47 times smaller, in 13 seconds for the largest.
- **Reads slowed as the project grew:** from under a millisecond each to 8 ms at 30,000 issues, after the restart and before `dolt gc`. Writes did not: a median of 5.1 ms over the whole 30,000-issue load, on a busy machine, with a 99th percentile of 27 ms and one write of 2.5 seconds.
- **30,000 issues was the largest tried.** Its load took 14 minutes and its files 6.5 GiB; 100,000 would have needed over 20 GiB of the virtual machine's disk and most of an hour, and was not run.

### 8. How the readings agree

- **`VmRSS` and Dolt's own `process_resident_memory_bytes` agree** to within 2 MiB in every reading.
- **Nearly all of it is anonymous memory.** `RssFile` stayed at 57 to 67 MiB from an empty server to a 5 GiB one, so it is not Dolt's storage (most likely its binary). None of what grows is file pages the kernel could drop.
- **`go_memstats_sys_bytes` less `go_memstats_heap_released_bytes`** was within 8% of `RssAnon` wherever that was 300 MiB or more, so the Go runtime accounts for all of it. The exception is the reading 5 seconds after `dolt gc`, where it was 63 to 87% of `RssAnon`: the runtime had released memory the kernel had not yet taken back.
- **The container's figure is not Dolt's.** The cgroup's `memory.current` adds the page cache of everything Dolt wrote, which was about the size of its data directory, until the limit forced the kernel to drop some: 5,855 MiB for a Dolt of 2,107 MiB at 30,000 issues, and 6,139 of a 6,144 MiB limit at 50 stores, without a kill. The cgroup's `anon` was Dolt's `RssAnon` plus 7 to 24 MiB, the spike itself. Size a container or a unit's `MemoryMax` from Dolt's resident memory with room for the cache; do not read Dolt's need from the container's total.
- **Nothing swapped.** `VmSwap` was 0 throughout; the containers had no swap.

## Not measured

- **Another machine.** Everything on Linux is arm64, in a container, in a virtual machine on a laptop with other work running. No amd64, no cloud VM, no disk of the kind a server has. Memory figures should carry over; every latency and throughput figure is this machine's.
- **Real traffic.** The data is synthetic and of one shape, written as fast as one writer can. A real server writes a few times a second or less, and starfix commits once a second, so each real write is more nearly a Dolt commit of its own; whether a write then costs more disk and memory than the 28 to 56 KiB and roughly 0.1 MiB measured here was not tested. Nor were issues that are edited, closed and reread, or days of uptime.
- **What sets off Dolt's own garbage collection,** what a write waits for when it runs (one of 18.7 seconds was seen, once), and whether to rely on it or turn it off.
- **`dolt gc` on a serving server.** It was run as root with every store closed. Running it with starfixd connected, and starfixd riding through a restart of Dolt, were not tried.
- **A memory limit over a restart, under reads, or with a `MemoryMax` on the unit** as well. The limit runs only wrote.
- **Where the ceiling on concurrent writers comes from,** Dolt or the disk.
- **Projects larger than 30,000 issues.**

## What it means for the design

1. `sfx admin project add` creates a project's database from anywhere, from a block of database names the operator grants starfixd's account at setup (decision 16). A command the operator runs on the server for each project would also work, and is not needed.
2. Size the machine by what is written between restarts of Dolt and by how many projects it is spread over, not by the data held. From these runs: 95 to 180 MiB for each project of 300 issues written to, 1.1 GiB for one of 3,000, 2.1 GiB for one of 30,000, and 80 MiB for Dolt itself. Closing idle stores is worth doing for connections alone.
3. A restart of Dolt is the operator's way to get memory back, and `dolt gc` before it the way to get nearly all of it back along with the disk. Both belong in the server guide's routine, not only in its advice on storage. Whether starfixd should run them, or warn when they are due, is a question for the design.
4. `GOMEMLIMIT` is worth setting only with a figure from the running server: no lower than 0.6 of `go_memstats_next_gc_bytes` read without a limit, or of resident memory. Below about half, Dolt takes every core it can for its collector and still serves. The alert of §16.5 on that metric against `go_gc_gomemlimit_bytes` is the right one, with the collector's rate beside it.
5. Writers in separate databases share something in Dolt or the disk that caps the server at 2,400 to 2,800 writes a second here, a hundred times a busy team's rate. It is not a reason to change one store, one writer per project.
