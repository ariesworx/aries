# starfix stage 5, slice F0: the local store under many processes

**Verdict: on macOS and on Linux, the plain-file store of [§18.4](../../docs/design/starfix.md#184-the-local-store-without-sqlite) holds with 100 and with 500 processes, provided the design names its lock and adds four rules it does not state. Windows was not run, so the choice is confirmed for two systems of three, and the third is the one the design doubts.** With `flock` on a lock file of its own, no entry was lost, doubled, torn or reordered in any run on either system, with up to 250 of 500 writers killed as they wrote; a killed holder's lock was free to the next process in 2 ms at most on macOS and 0.8 ms on Linux; and a reader of the cache never saw a partial or missing file in 418,000 reads. The two systems agree in everything that was counted. They differ in what a sync costs, which on Linux was not measured on a real disk, and in how `flock` queues. What must change:

1. **Name the lock: `flock` on Linux and macOS, `LockFileEx` on Windows, on a lock file that is never the data file.** Not `fcntl` record locks, which on both systems do not keep a second handle of the same process out and are dropped when the process closes any descriptor of the file. Not a lock file made with `O_EXCL` and a lease: a killed holder stalled every other process for the whole lease, and in 24 of 50 take-overs of a stale lock file, on each system, more than one process came away holding it: up to 4 on macOS and up to 9 on Linux.
2. **The replay lock needs no lease.** The lease is there "so a session that dies mid-replay does not hold the rest back"; the kernel does that, within 9 ms on macOS and 11 ms on Linux. A lease that can be taken over is what lets two replayers run at once, and on Linux it did: with the lock file, 56 deliveries began while another process's was still going, and 14 entries were delivered more than twice. What a kernel lock does not cover is a holder that is alive and stuck, which a deadline on each replayed request does.
3. **An appender opens the outbox after it has the lock, every time.** Appenders that kept it open across a compaction lost 4,669 and 4,689 of 5,000 entries in two runs on macOS and 9,283 of 10,000 in a third, and 3,862 of 5,000 and 8,374 of 10,000 on Linux, each append having returned without error.
4. **A writer ends a torn last line before it appends, and a reader skips a line that does not parse and does not treat bytes after the last newline as a line.** With both, 24 torn lines on macOS and 124 on Linux cost no entry that had been acknowledged.
5. **Replay is at-least-once, so the key is what makes it safe.** A replayer killed between sending an entry and noting the answer has that entry sent again by the next: 5 to 20 of 3,000 entries on macOS and 15 to 20 on Linux, in runs with 20 kills. Nothing may be queued without an idempotency key, as §18.5 already has it.
6. **On macOS the sync on each append is the whole cost, and it is serialized by the lock:** 4.4 ms each, so about 207 appends a second for the machine however many processes there are, and a median wait of 0.46 s when 100 append at once and 2.3 s when 500 do. One append alone takes 4.8 ms. That is usable. On Linux a sync took 0.3 to 0.5 ms and the machine did 1,700 to 2,100 appends a second, with a median of 39 ms among 100 and 158 ms among 500; but that was a container's overlay file system on a virtual machine's disk, where a sync very likely does not reach the physical disk. It shows that nothing but the sync is expensive on Linux. It is not the cost of a durable append on a Linux disk, so macOS's figure stays as the conservative one, and the sync is still the figure to check on a real Linux disk and on Windows. The cache should not sync at all: on macOS a synced cache write that takes 5.7 ms alone had a 99th percentile of 5 s among 100 processes and 28 s among 500.

Two sets of measurements, on two systems, both on 13 October 2026. Each section gives macOS first and Linux after it:

| Sections | System |
|---|---|
| 1 to 6 | macOS 26.6 on an Apple M2 Ultra, APFS on the internal disk |
| 1 to 6 | Linux 7.0 arm64 in a container, in Docker Desktop's virtual machine on the same Mac, overlayfs on the virtual machine's disk. Section 6 at 100 and 500 processes only |
| None | Windows: there was no Windows machine. The spike vets and builds for `windows/amd64`; the command to run is under [Not measured](#not-measured) |

Code: this directory, standard library only. `go run ./spike/outbox -exp all -n 1,10,100 -lock all` is the run reported here, with `-exp all -n 500 -m 20 -ops 100 -lock flock` for the 500-process rows; the header of `main.go` lists each experiment's flags. It re-executes itself as that many real processes on one store directory, kills some with `SIGKILL` (`TerminateProcess` on Windows), and then checks what is on disk. It answers the second half of slice F0 in [§18.10](../../docs/design/starfix.md#1810-plan) and the first item of [§18.13](../../docs/design/starfix.md#1813-not-tested); how often real sessions lose the server is the other half, and is not here. The Linux figures are from the same two runs of the spike built on the Mac (`GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o outbox ./spike/outbox`) and run in the container.

## Environment

| | |
|---|---|
| macOS: machine | Apple M2 Ultra, 24 cores, 128 GB RAM, macOS 26.6.2. **Shared:** other work ran on it throughout the macOS runs, with a load average of 3 to 8 before they began |
| macOS: disk | The internal solid-state disk, APFS. The store directories were under `/private/tmp` |
| macOS: Go | 1.27.1, darwin/arm64. `File.Sync` on macOS is `fcntl(F_FULLFSYNC)`, which waits for the disk itself and not only for the operating system |
| Linux: machine | Ubuntu 24.04.5 in a new `ubuntu:24.04` container, kernel 7.0.12-linuxkit, arm64: the virtual machine of Docker Desktop 29.7.2 on the same Mac, given 24 CPUs and 8 GiB. The Mac was otherwise idle |
| Linux: disk | The container's own `/tmp`: overlayfs on the virtual machine's virtual disk. **A sync there very likely does not reach the physical disk,** so Linux's sync times are not a disk's |
| Linux: Go | 1.27.1, linux/arm64, built on the Mac without cgo. `File.Sync` is `fsync` |
| Processes | 100 unless a row says otherwise; 500 for the rows marked so. Each is this binary started again, waiting on a file so that all begin together |
| Entries | 300 to 4,096 bytes of JSON on one line, the size of a handoff, each written with one `write` unless a row says two |
| Stand-in server | A log file for each replayer, with the wall-clock time each delivery began and ended. It takes 2 ms to answer. No network, no Dolt |
| Kills | `SIGKILL` from the parent, at random moments or aimed at whoever holds the lock |

The spike prints every figure here; its raw output is not in the repository. Each row is one run. Latencies are of a machine with other work on it, and the 500-process runs put its load average near 280, so read them for their size, not their second digit. Counts of entries lost, doubled or torn do not depend on the load. This is of the macOS runs; the Linux runs had the machine to themselves.

## 1. The lock

§18.4 says "a lock file" and does not say how it is locked. Four ways need no dependency. `golang.org/x/sys` is in `go.mod` already, and nothing here needed it: `syscall` has `Flock` and `FcntlFlock` on Linux and macOS, and `LockFileEx` is reached through `kernel32.dll` with `syscall.NewLazyDLL`.

| Primitive | Where | What owns the lock | When its holder dies |
|---|---|---|---|
| `flock` | Linux, macOS | The open file description | Released by the kernel when the process's descriptors close: measured, 2.0 ms at most on macOS and 0.8 ms on Linux |
| `fcntl` record locks (`F_SETLK`) | Linux, macOS | The process and the file, together | Released the same way: measured, 2.3 ms at most on macOS and 0.8 ms on Linux. Also released whenever the process closes any descriptor of that file |
| Open file description locks (`F_OFD_SETLK`) | Linux 3.15 and later. macOS has the constants behind a private define, and they worked here | The open file description | Measured, 2.0 ms at most on macOS and 0.9 ms on Linux |
| `LockFileEx` | Windows | The handle | **Not measured.** Microsoft's documentation says the system unlocks when the process ends, and that how long it takes "depends upon available system resources" |
| A lock file made with `O_EXCL`, taken over when its modification time is older than a lease | Everywhere | Whoever created the file | Left behind. Measured: every other process waits for the lease, here 2 s |

The spike has all of them behind one interface (`lock.go`), chosen by build tags: `lock_unix.go` for Linux and macOS, `lock_windows.go` for Windows. The Go command locks its own files the same way (`cmd/go/internal/lockedfile`): `flock` on Linux and macOS, `LockFileEx` on Windows.

### Does it exclude

100 processes each add one to a number in a file 200 times under the lock, reading it and writing it back in separate calls, so two holders at once lose an addition.

| Lock | Additions | Lost | Locks a second | Wait for the lock: p50 | p99 | max |
|---|---|---|---|---|---|---|
| `flock` | 20,000 | 0 | 15,443 | 0.001 ms | 120 ms | 1,251 ms |
| `fcntl` | 20,000 | 0 | 3,247 | 17.1 ms | 175 ms | 432 ms |
| ofd | 20,000 | 0 | 3,373 | 17.7 ms | 149 ms | 342 ms |
| `O_EXCL` with a lease | 20,000 | 0 | 935 | 33.3 ms | 719 ms | 1,612 ms |
| `flock`, 500 processes | 100,000 | 0 | 13,123 | 0.001 ms | 211 ms | 7,208 ms |

On Linux:

| Lock | Additions | Lost | Locks a second | Wait for the lock: p50 | p99 | max |
|---|---|---|---|---|---|---|
| `flock` | 20,000 | 0 | 6,163 | 10.3 ms | 75.7 ms | 203 ms |
| `fcntl` | 20,000 | 0 | 6,279 | 10.1 ms | 75.1 ms | 254 ms |
| ofd | 20,000 | 0 | 6,206 | 10.2 ms | 75.1 ms | 190 ms |
| `O_EXCL` with a lease | 20,000 | 0 | 2,053 | 8.1 ms | 332 ms | 801 ms |
| `flock`, 500 processes | 100,000 | 0 | 5,370 | 62.8 ms | 403 ms | 940 ms |

- **All four exclude** when nobody dies, on both systems.
- **`flock` queues differently on the two systems.** On macOS it is not fair when the lock is held for microseconds. It was four times as fast as the others because the process that releases the lock usually takes it again: half of all waits were a microsecond, and one process waited 1.25 s of a 1.3 s run, 7.2 s of 7.6 s among 500. On Linux the lock went round: the median wait was 10.3 ms and the longest 203 ms of a 3.3 s run (62.8 ms and 940 ms of 18.6 s among 500), at 6,163 locks a second against macOS's 15,443, and the three kernel locks were alike. With a sync inside the lock, macOS queued evenly too (section 2). Nothing in the design holds the lock that briefly.
- **The `O_EXCL` lock is polled** (here every millisecond), which is why it is slowest, and it costs 124 µs to take and release alone on macOS and 19 µs on Linux, against 0.5 to 0.7 µs for the kernel's locks.

### What the lock belongs to

One process, holding the lock, tries it again through a second handle; then opens and closes the lock file once more, and another process tries the lock.

| Lock | A second handle in the same process is kept out | Still held after the holder opens and closes the lock file again |
|---|---|---|
| `flock` | yes | yes |
| `fcntl` | **no** | **no** |
| ofd | yes | yes |
| `O_EXCL` with a lease | yes | yes |

Linux answered the same in every cell.

- **`fcntl` record locks are wrong for `sfx mcp`,** which replays in the background of the process that also appends for its tool calls: the two would not exclude each other, and whichever closed the file first would drop the other's lock. Both failures are silent.

### When the holder is killed

A process holds the lock, a second is blocked waiting for it, and the first is killed. From the kill being sent to the second holding the lock, 20 rounds each:

| Lock | min | p50 | max |
|---|---|---|---|
| `flock` | 0.5 ms | 1.2 ms | 2.0 ms |
| `fcntl` | 0.5 ms | 1.3 ms | 2.3 ms |
| ofd | 0.5 ms | 1.2 ms | 2.0 ms |
| `O_EXCL` with a lease of 2 s | 1,896 ms | 1,921 ms | 1,943 ms |

On Linux:

| Lock | min | p50 | max |
|---|---|---|---|
| `flock` | 0.2 ms | 0.3 ms | 0.6 ms |
| `fcntl` | 0.2 ms | 0.3 ms | 0.8 ms |
| ofd | 0.3 ms | 0.6 ms | 0.9 ms |
| `O_EXCL` with a lease of 2 s | 1,898 ms | 1,916 ms | 1,941 ms |
| `flock`, in the 500-process run | 0.2 ms | 0.4 ms | 0.8 ms |

- **A kernel lock is not left held.** That is the property the design wants from its lease, and it comes from the lock belonging to something the kernel closes when the process ends, however it ends.
- **A lock file is left held, for the lease.** A shorter lease shortens the stall and brings the take-over nearer to a holder that is merely slow, whose sync here had a 99th percentile of up to 17 ms and whose compaction held the lock for up to 806 ms (section 5).

### Taking over a stale lock file

A lock file is left with a modification time two leases old, and 100 processes each try the lock once, at the same moment. 50 rounds.

| Processes that came away holding the lock | Rounds |
|---|---|
| 1 | 26 |
| 2 | 20 |
| 3 | 3 |
| 4 | 1 |

On Linux, the same 50 rounds:

| Processes that came away holding the lock | Rounds |
|---|---|
| 1 | 26 |
| 2 | 11 |
| 3 | 6 |
| 4 | 1 |
| 6 | 1 |
| 7 | 1 |
| 8 | 3 |
| 9 | 1 |

- **The take-over is not atomic, and it failed in 24 rounds of 50 on each system,** worse on Linux: up to 9 holders, and 6 or more in 6 rounds, against 4 at most on macOS. Two processes both see a stale file; the first removes it and creates its own, and the second then removes the first's new file and creates another. The standard library has no call that removes a file only if it is still the one that was looked at. A lock that makes the take-over safe is a kernel lock, at which point the lock file is not needed.
- **On macOS the same lock was taken over 33 times in the other experiments without two holders being seen** (13 in section 3, 20 in section 4), and on Linux 4 times in section 3: there the contenders poll at different moments. **In Linux's replay two holders were seen** (section 4). The race needs two contenders inside the same few microseconds, which a machine that wakes hundreds of sessions at once will give it.

### On a network or a synced folder

**Not measured: this is from documentation.** `flock(2)` on Linux says that since 2.6.12 an NFS client emulates `flock` as a byte-range lock on the whole file, which needs the file open for writing (the spike opens it so), and that with the `nolock` or `local_lock` mount options the lock is local to one machine and excludes nobody on another. A holder that dies on an NFS client has its lock released by that client; a client machine that dies holds it until the server's lease or its own restart. A synced folder (a cloud drive's client) carries no lock from one machine to another, replaces files on its own schedule, and on Windows may hold them open. The store belongs on a local disk, in a directory that is not synced or roamed: [§18.2](../../docs/design/starfix.md#182-principles-kept-and-what-they-rule-out) says "under the user's data directory", and on Windows that should be the local one, not the roaming one.

## 2. Append

Each process appends 50 entries: take the append lock, open the outbox, write the line, sync, close, release. Afterward the file is checked for exactly the entries expected, every line whole and valid JSON with its checksum, and each process's entries in the order it wrote them. Every row passed. Times are of one append.

`flock`:

| Processes | Sync | Entries | Appends a second | p50 | p95 | p99 | max | Waiting for the lock, p50 | Open and write, p50 | Sync, p50 | Sync, p99 |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | yes | 50 | 207 | 4.8 ms | 5.2 ms | 6.6 ms | 6.6 ms | 0.006 ms | 0.28 ms | 4.4 ms | 6.5 ms |
| 1 | no | 50 | 7,523 | 0.07 ms | 0.15 ms | 0.34 ms | 0.34 ms | 0.001 ms | 0.05 ms | | |
| 10 | yes | 500 | 214 | 46.5 ms | 51.2 ms | 54.6 ms | 55.5 ms | 42.0 ms | 0.08 ms | 4.4 ms | 5.6 ms |
| 10 | no | 500 | 16,383 | 0.49 ms | 0.86 ms | 1.4 ms | 1.5 ms | 0.44 ms | 0.03 ms | | |
| 100 | yes | 5,000 | 207 | 459 ms | 503 ms | 1,103 ms | 1,111 ms | 454 ms | 0.08 ms | 4.3 ms | 5.5 ms |
| 100 | no | 5,000 | 11,656 | 8.2 ms | 10.0 ms | 16.9 ms | 30.3 ms | 8.1 ms | 0.05 ms | | |
| 500, 20 entries each | yes | 10,000 | 206 | 2,350 ms | 3,066 ms | 3,140 ms | 5,537 ms | 2,345 ms | 0.10 ms | 4.4 ms | 6.7 ms |
| 500, 20 entries each | no | 10,000 | 8,010 | 58.8 ms | 93.9 ms | 131 ms | 239 ms | 58.7 ms | 0.06 ms | | |

The four primitives at 100 processes:

| Lock | Sync | Appends a second | p50 | p95 | p99 | max |
|---|---|---|---|---|---|---|
| `flock` | yes | 207 | 459 ms | 503 ms | 1,103 ms | 1,111 ms |
| `fcntl` | yes | 215 | 307 ms | 1,275 ms | 1,959 ms | 3,924 ms |
| ofd | yes | 184 | 320 ms | 1,603 ms | 2,970 ms | 5,903 ms |
| `O_EXCL` with a lease | yes | 138 | 440 ms | 2,092 ms | 3,535 ms | 7,780 ms |
| `flock` | no | 11,656 | 8.2 ms | 10.0 ms | 16.9 ms | 30.3 ms |
| `fcntl` | no | 4,181 | 13.2 ms | 74.5 ms | 123 ms | 260 ms |
| ofd | no | 4,340 | 12.9 ms | 71.4 ms | 111 ms | 227 ms |
| `O_EXCL` with a lease | no | 910 | 56.4 ms | 382 ms | 613 ms | 1,366 ms |

On Linux, `flock`:

| Processes | Sync | Entries | Appends a second | p50 | p95 | p99 | max | Waiting for the lock, p50 | Open and write, p50 | Sync, p50 | Sync, p99 |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | yes | 50 | 2,085 | 0.36 ms | 0.70 ms | 2.5 ms | 2.5 ms | under 0.001 ms | 0.007 ms | 0.34 ms | 2.5 ms |
| 1 | no | 50 | 20,892 | 0.007 ms | 0.012 ms | 0.028 ms | 0.028 ms | under 0.001 ms | 0.005 ms | | |
| 10 | yes | 500 | 2,141 | 4.8 ms | 6.5 ms | 7.9 ms | 9.4 ms | 4.3 ms | 0.010 ms | 0.33 ms | 1.8 ms |
| 10 | no | 500 | 40,746 | 0.14 ms | 0.57 ms | 0.87 ms | 1.1 ms | 0.11 ms | 0.010 ms | | |
| 100 | yes | 5,000 | 1,678 | 38.8 ms | 159 ms | 249 ms | 501 ms | 38.3 ms | 0.013 ms | 0.48 ms | 1.9 ms |
| 100 | no | 5,000 | 7,666 | 7.6 ms | 40.8 ms | 68.3 ms | 111 ms | 7.5 ms | 0.015 ms | | |
| 500, 20 entries each | yes | 10,000 | 1,863 | 158 ms | 723 ms | 1,078 ms | 2,557 ms | 157 ms | 0.016 ms | 0.46 ms | 0.77 ms |
| 500, 20 entries each | no | 10,000 | 3,363 | 80.9 ms | 448 ms | 736 ms | 1,673 ms | 80.6 ms | 0.016 ms | | |

On Linux, the four primitives at 100 processes:

| Lock | Sync | Appends a second | p50 | p95 | p99 | max |
|---|---|---|---|---|---|---|
| `flock` | yes | 1,678 | 38.8 ms | 159 ms | 249 ms | 501 ms |
| `fcntl` | yes | 1,790 | 37.5 ms | 151 ms | 226 ms | 441 ms |
| ofd | yes | 1,736 | 40.8 ms | 144 ms | 230 ms | 450 ms |
| `O_EXCL` with a lease | yes | 444 | 140 ms | 675 ms | 1,032 ms | 2,102 ms |
| `flock` | no | 7,666 | 7.6 ms | 40.8 ms | 68.3 ms | 111 ms |
| `fcntl` | no | 8,871 | 6.9 ms | 35.5 ms | 56.3 ms | 108 ms |
| ofd | no | 8,128 | 7.2 ms | 38.7 ms | 64.1 ms | 156 ms |
| `O_EXCL` with a lease | no | 7,819 | 6.2 ms | 42.7 ms | 69.1 ms | 147 ms |

- **Nothing was lost, torn, mixed or reordered,** by any primitive at any count on either system: 64,400 entries across 26 runs on each.
- **On macOS the sync is the cost, and the lock makes it everyone's.** One sync took 4.3 to 4.4 ms at the median whether 1 process ran or 500, and the machine did 206 to 214 appends a second at every count. So an append's latency is the queue: 4.8 ms alone, 46 ms among 10, 0.46 s among 100, 2.3 s among 500, when all of them append without pause.
- **On Linux a sync took 0.29 to 0.50 ms at the median, a tenth of macOS's, and that is not a disk's figure.** The store was a container's overlayfs on a virtual machine's virtual disk, where a sync very likely returns before anything reaches the physical disk; macOS's `F_FULLFSYNC` does wait for it. With that sync an append alone took 0.36 ms and the machine did 1,700 to 2,100 appends a second: a median of 4.8 ms among 10, 39 ms among 100 and 158 ms among 500. What this shows is that nothing else in an append is expensive on Linux: opening and writing took 0.007 to 0.016 ms. What it does not show is what a durable append costs on a Linux server's or laptop's disk, which is still to be measured; macOS's 4.4 ms is the figure to plan by until it is.
- **Among 500 processes on Linux the lock's own queue cost as much as the sync.** Without a sync the machine did 3,363 appends a second with a median of 81 ms, against 1,863 and 158 ms with one, where 100 processes without a sync did 7,666.
- **On macOS, without the sync an append alone takes 0.07 ms, a seventieth,** and 10 processes or more made 8,000 to 16,000 appends a second.
- **That load is not the one the design expects.** A session writes a handoff every few minutes, and only while the server cannot be reached. Stage 0 put a busy team at about 25 writes a second on the server; a machine's own sessions are fewer. At 207 a second the queue is empty nearly always, and the figure that matters is the 4.8 ms. The 0.46 s is what the last of 100 sessions waits if all write in the same instant, as they might when a server comes back or goes away.
- **`flock` had the shortest tail on macOS, and not on Linux.** With a sync inside the lock macOS gave it a 99th percentile of 1.1 s against 2.0 to 3.5 s for the others at the same throughput. On Linux the three kernel locks were alike, 226 to 249 ms, so this is how macOS queues and not a reason to choose `flock`; section 1's are. The lock file was the slowest with a sync on both: 444 appends a second on Linux and a 99th percentile of 1.0 s.

## 3. Processes killed as they append

100 processes each append 50 entries with a sync, and 50 of them are killed as they go, every other kill aimed at the process holding the lock. Each appender notes, in a file of its own, when it has the lock and when an append has finished; "returned" below is an append the process was told had succeeded. Once with each entry written by one `write`, and once by two with 1 ms between them, as a short write would make it.

| Lock | An entry is | Killed | Of them, holding the lock for certain | Survivors that finished | Appends that returned | Of them missing | Entries twice | Out of order | Torn lines | Longest wait for the lock |
|---|---|---|---|---|---|---|---|---|---|---|
| `flock` | one write | 50 | 24 | 50 of 50 | 2,642 | 0 | 0 | 0 | 0 | 0.8 s |
| `flock` | two writes | 50 | 23 | 50 of 50 | 2,606 | 0 | 0 | 0 | 6 | 1.2 s |
| `fcntl` | one write | 50 | 26 | 50 of 50 | 2,680 | 0 | 0 | 0 | 0 | 2.8 s |
| `fcntl` | two writes | 50 | 24 | 50 of 50 | 2,604 | 0 | 0 | 0 | 2 | 5.3 s |
| ofd | one write | 50 | 25 | 50 of 50 | 2,643 | 0 | 0 | 0 | 0 | 3.2 s |
| ofd | two writes | 50 | 23 | 50 of 50 | 2,631 | 0 | 0 | 0 | 1 | 2.7 s |
| `O_EXCL` with a lease | one write | 50 | 2 | 50 of 50 | 2,506 | 0 | 0 | 0 | 0 | 6.0 s |
| `O_EXCL` with a lease | two writes | 50 | 2 | 50 of 50 | 2,512 | 0 | 0 | 0 | 0 | 6.7 s |
| `flock`, 500 processes, 20 entries each | one write | 250 | 115 | 250 of 250 | 5,628 | 0 | 0 | 0 | 0 | 3.5 s |
| `flock`, 500 processes, 20 entries each | two writes | 250 | 119 | 250 of 250 | 5,508 | 0 | 0 | 0 | 15 | 5.2 s |

On Linux:

| Lock | An entry is | Killed | Of them, holding the lock for certain | Survivors that finished | Appends that returned | Of them missing | Entries twice | Out of order | Torn lines | Longest wait for the lock |
|---|---|---|---|---|---|---|---|---|---|---|
| `flock` | one write | 50 | 24 | 50 of 50 | 3,256 | 0 | 0 | 0 | 0 | 0.5 s |
| `flock` | two writes | 50 | 24 | 50 of 50 | 2,909 | 0 | 0 | 0 | 14 | 0.4 s |
| `fcntl` | one write | 50 | 22 | 50 of 50 | 3,310 | 0 | 0 | 0 | 0 | 0.6 s |
| `fcntl` | two writes | 50 | 26 | 50 of 50 | 2,952 | 0 | 0 | 0 | 17 | 0.3 s |
| ofd | one write | 50 | 24 | 50 of 50 | 3,588 | 0 | 0 | 0 | 0 | 0.4 s |
| ofd | two writes | 50 | 24 | 50 of 50 | 2,924 | 0 | 0 | 0 | 18 | 0.6 s |
| `O_EXCL` with a lease | one write | 50 | 2 | 50 of 50 | 2,518 | 0 | 0 | 0 | 0 | 4.6 s |
| `O_EXCL` with a lease | two writes | 50 | 2 | 50 of 50 | 2,511 | 0 | 0 | 0 | 1 | 4.6 s |
| `flock`, 500 processes, 20 entries each | one write | 143 | 66 | 357 of 357 | 8,336 | 0 | 0 | 0 | 0 | 3.2 s |
| `flock`, 500 processes, 20 entries each | two writes | 240 | 116 | 260 of 260 | 7,190 | 0 | 0 | 0 | 74 | 4.3 s |

The 500-process runs finished on Linux before all 250 kills had been sent.

- **No append that returned was missing, and no entry was there twice,** in any run on either system. The file stayed readable throughout.
- **No lock was left held.** Every survivor finished, with 23 to 26 holders of 50 killed while they held a kernel lock, and 115 to 119 of 250. "For certain" is a lower bound: a process whose last note is of taking the lock. On Linux, 22 to 26 of 50, and 66 of 143 and 116 of 240.
- **With the lock file, each dead holder cost the lease.** The parent's kills aimed at the holder mostly found the same dead one, which is why only 2 are counted; the lock was taken over 5 and 8 times, and one append waited 6.0 and 6.7 s. On Linux it was taken over twice in each run, and one append waited 4.6 s.
- **One `write` was never torn by a kill:** 0 torn lines in 450 kills on macOS and 343 on Linux. A torn line needs the entry to reach the file in more than one piece. That happens when a `write` is short (a full disk, a quota) or when the machine loses power, neither of which was produced here; the two-write rows stand in for them.
- **A torn line is the first part of an entry and no newline.** For example, 2,026 bytes that begin `{"proc":53,"seq":0,"key":"53-0","op":"ha` and stop in the middle of the text. It can never parse, since the closing brace is the last byte of an entry; the one fragment that does parse is a whole entry that lacks only its newline, and it is a good entry.
- **The next writer must end it.** Under the lock, before it writes, an appender reads the file's last byte and writes a newline if it is not one. Without that, its own entry is joined to the fragment and a reader throws both away. The 24 torn lines on macOS and 124 on Linux were ended so, became lines a reader skips, and cost nothing that had returned. With a kernel lock, 17 to 26 entries in each 100-process run on macOS, and 6 to 24 on Linux, were in the file without having returned: written, and the process killed before it could say so. Those are replayed, which the key makes harmless.
- **A torn last line, with no writer after it:** a writer was killed between its two writes at the end of a file of three entries. A reader found 3 whole entries and 1,368 bytes after the last newline, which it must not take for a line. The lock was free at once. Truncating to the last newline under the lock, and appending, left 4 whole entries and nothing else. Truncating and ending the line both work; ending it needs one byte written and nothing destroyed. Linux did the same with a fragment of 412 bytes.

## 4. Replay

An outbox of 3,000 entries. 100 processes wait for the replay lock, and whoever holds it reads the answered file, then delivers each unanswered entry in order to the stand-in server, noting each answer. The holder is killed 20 times, each after 30 to 150 ms of holding the lock. The lock file has a lease of 2 s, renewed by its holder every 0.5 s and polled by the others every millisecond.

| Lock | Answers synced | Kill to another process holding the lock: p50 | max | Kill to the next delivery: p50 | max | Delivered once | Twice | Never | While another's was going | Out of order | Took |
|---|---|---|---|---|---|---|---|---|---|---|---|
| `flock` | yes | 2.6 ms | 5.0 ms | 12.5 ms | 21.9 ms | 2,995 | 5 | 0 | 0 | 0 | 24.8 s |
| `fcntl` | yes | 3.9 ms | 7.0 ms | 14.1 ms | 21.6 ms | 2,994 | 6 | 0 | 0 | 0 | 24.4 s |
| ofd | yes | 3.7 ms | 8.2 ms | 14.9 ms | 20.2 ms | 2,995 | 5 | 0 | 0 | 0 | 24.2 s |
| `O_EXCL` with a lease | yes | 1,900 ms | 1,966 ms | 1,907 ms | 1,976 ms | 2,987 | 13 | 0 | 0 | 0 | 59.2 s |
| `flock` | no | 1.7 ms | 5.7 ms | 13.5 ms | 22.2 ms | 2,980 | 20 | 0 | 0 | 0 | 9.5 s |
| `flock`, 500 processes | yes | 3.1 ms | 7.6 ms | 11.8 ms | 19.9 ms | 2,991 | 9 | 0 | 0 | 0 | 34.5 s |
| `flock`, 500 processes | no | 3.7 ms | 8.5 ms | 17.1 ms | 23.8 ms | 2,980 | 20 | 0 | 0 | 0 | 20.1 s |

On Linux:

| Lock | Answers synced | Kill to another process holding the lock: p50 | max | Kill to the next delivery: p50 | max | Delivered once | Twice | More than twice | Never | While another's was going | Out of order | Took |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| `flock` | yes | 0.8 ms | 5.1 ms | 10.2 ms | 18.5 ms | 2,985 | 15 | 0 | 0 | 0 | 0 | 12.3 s |
| `fcntl` | yes | 0.9 ms | 10.1 ms | 12.5 ms | 25.4 ms | 2,982 | 18 | 0 | 0 | 0 | 0 | 12.4 s |
| ofd | yes | 0.8 ms | 9.0 ms | 10.8 ms | 21.1 ms | 2,984 | 16 | 0 | 0 | 0 | 0 | 13.0 s |
| **`O_EXCL` with a lease** | yes | 1,909 ms | 1,962 ms | 1,909 ms | 1,970 ms | 2,962 | 24 | **14** | 0 | **56** | 0 | 48.1 s |
| `flock` | no | 0.8 ms | 1.6 ms | 13.9 ms | 20.3 ms | 2,980 | 20 | 0 | 0 | 0 | 0 | 10.3 s |
| `flock`, 500 processes | yes | 1.2 ms | 7.0 ms | 14.7 ms | 23.9 ms | 2,985 | 15 | 0 | 0 | 0 | 0 | 22.7 s |
| `flock`, 500 processes | no | 0.8 ms | 6.1 ms | 11.3 ms | 18.4 ms | 2,980 | 20 | 0 | 0 | 0 | 0 | 20.6 s |

- **With a kernel lock, no entry was skipped, none was delivered by two processes at once, none was delivered more than twice, and first deliveries were in the outbox's order,** in every run on both systems. No run with any lock skipped an entry.
- **The lock file with a lease let two replayers run at once, on Linux.** 56 deliveries began while another process's was still going; 24 entries were delivered twice and 14 more than twice, for 20 kills. The holders' own check at each renewal caught none of it: no lease was found lost. On macOS the same lock showed no delivery overlapping another, and 13 entries delivered twice where the kernel locks gave 5 or 6. That excess was not explained on macOS; Linux shows that this lock repeats deliveries by a second route, two holders; whether that is what happened on macOS is not known.
- **Another process had the lock within 9 ms of the kill on macOS and 11 ms on Linux** with a kernel lock, at a median of 2 to 4 ms and of 0.8 to 1.2 ms, and was delivering within 24 and 26 ms; the difference is reading about 6 MiB of outbox and the answered file. With the lock file it was the lease, 1.9 s, twenty times over, on both.
- **These waiters block in the lock call,** which is the fastest a take-over can be. A session should not: it should try the lock, and if another process is replaying, leave it to it. Then a dead replayer's work resumes when the next session next tries, which is the design's "at the next command", not 3 ms.
- **Replay is at-least-once.** An entry is delivered twice when the holder dies after sending it and before the answer is in the answered file. With the answers synced that window is the 2 ms the server takes out of a 7 ms cycle, and 5 to 9 of 20 kills fell in it (13 with the lock file; see above). Without the sync the window is the whole cycle and all 20 did. Neither is wrong: the second delivery carries the same key. Linux agrees with that account: its sync is a fraction of a millisecond, so the window is nearly the whole cycle with the answers synced too, and 15 to 18 of 20 kills fell in it.
- **Syncing each answer cost more than it saved, on macOS.** It made replay 2.6 times as slow (24.8 s against 9.5 s for 3,000 entries, the server's 2 ms included) and spared 15 repeat deliveries in 20 kills. After a power loss, answers that were not synced are entries delivered again, each with its key. On Linux, with the sync it has there, it cost a fifth more (12.3 s against 10.3 s) and spared 5.

## 5. Compaction while appenders append

100 processes each append 50 entries with a sync. A replayer delivers them, with each answer synced, and after every 200 answers it compacts: still holding the replay lock, it takes the append lock, rewrites the outbox without the answered entries (temporary file, sync, rename), then replaces the answered file with an empty one the same way, and syncs the directory. Every entry appended is then looked for in what the server was sent and in what is left in the outbox.

| Run | Appended | Delivered once | Twice | Lost | Compactions timed | A compaction waits for the append lock: p50 | max | Then holds it: p50 | max | An append: p50 | p99 | max |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| As designed | 5,000 | 5,000 | 0 | 0 | 25 | 440 ms | 520 ms | 34 ms | 56 ms | 0.8 s | 1.5 s | 2.4 s |
| The replayer killed 10 times | 5,000 | 4,997 | 3 | 0 | 24 | 434 ms | 1,191 ms | 32 ms | 50 ms | 0.9 s | 3.2 s | 3.9 s |
| **Appenders keep the outbox open** | 5,000 | 311 | 0 | **4,689** | 1 | 417 ms | 417 ms | 19 ms | 19 ms | 0.5 s | 1.5 s | 2.1 s |
| As designed, 500 processes, 20 entries each | 10,000 | 10,000 | 0 | 0 | 50 | 0.03 ms | 3,114 ms | 74 ms | 806 ms | 3.3 s | 4.9 s | 6.9 s |
| The replayer killed 10 times, 500 processes | 10,000 | 9,994 | 6 | 0 | 48 | 0.02 ms | 3,104 ms | 73 ms | 104 ms | 3.3 s | 4.4 s | 7.8 s |
| **Appenders keep the outbox open,** 500 processes | 10,000 | 717 | 0 | **9,283** | 3 | 2,875 ms | 4,497 ms | 26 ms | 31 ms | 2.4 s | 3.7 s | 6.0 s |

"Compactions timed" are those of replayers that were not killed. An earlier run of the first three rows, before the spike timed the two parts of a compaction apart, lost nothing in the first two, with 7 entries delivered twice for 10 kills, and lost 4,669 in the third.

On Linux:

| Run | Appended | Delivered once | Twice | Lost | Compactions timed | A compaction waits for the append lock: p50 | max | Then holds it: p50 | max | An append: p50 | p99 | max |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| As designed | 5,000 | 5,000 | 0 | 0 | 25 | 0.02 ms | 84 ms | 44 ms | 72 ms | 32 ms | 232 ms | 426 ms |
| The replayer killed 10 times | 5,000 | 4,991 | 9 | 0 | 20 | 0.01 ms | 0.02 ms | 48 ms | 88 ms | 33 ms | 230 ms | 413 ms |
| **Appenders keep the outbox open** | 5,000 | 1,138 | 0 | **3,862** | 5 | 7.2 ms | 17.9 ms | 11 ms | 15 ms | 33 ms | 216 ms | 438 ms |
| As designed, 500 processes, 20 entries each | 10,000 | 10,000 | 0 | 0 | 50 | 0.01 ms | 429 ms | 74 ms | 532 ms | 179 ms | 1.5 s | 2.8 s |
| The replayer killed 10 times, 500 processes | 10,000 | 9,992 | 8 | 0 | 45 | 0.01 ms | 301 ms | 74 ms | 119 ms | 177 ms | 1.2 s | 2.7 s |
| **Appenders keep the outbox open,** 500 processes | 10,000 | 1,626 | 0 | **8,374** | 8 | 95 ms | 665 ms | 13 ms | 21 ms | 169 ms | 1.2 s | 2.6 s |

- **As designed, nothing was lost or doubled** with compaction running 25 and 50 times under the appenders, and with the replayer killed in and around it: 3 and 6 repeat deliveries for 10 kills, each of the kind section 4 describes. Linux gave the same: nothing lost, and 9 and 8 repeat deliveries for 10 kills.
- **An appender that keeps the outbox open loses nearly everything, silently.** The rename leaves its descriptor on the old file, which no longer has a name; its writes and syncs succeed, and the entries are gone. 94% and 93% of entries were lost, every append having returned without error. The outbox must be opened after the lock is taken, each time. This is Linux's and macOS's behavior; on Windows the rename itself would be refused while anything has the file open (section 6), which stops compaction instead of losing entries, and was not measured. Linux lost 3,862 of 5,000 and 8,374 of 10,000 the same way, 77% and 84%.
- **The lock file must not be the outbox,** for the same reason: a lock on a file that has been renamed away excludes nobody. The design's "lock file" is right, and the lock file itself is never replaced or removed.
- **Two things about order, neither in the design.** The two locks are taken in one order only, replay and then append. And the outbox is replaced before the answered file: a death between the two leaves answers with no entry, which is harmless, where the other order leaves entries with no answer, all to be delivered again.
- **Appenders wait while it runs:** 19 to 74 ms at the median, and once 806 ms, for an outbox whose size at each compaction was not recorded and cannot have passed the 10.7 and 21.6 MiB appended in all. The compaction itself waited its turn for the append lock like any appender, 0.4 s among 100. On Linux, 44 to 74 ms at the median, and once 532 ms.
- **A reader without the lock saw nothing wrong.** The replayer reads the outbox while appenders write to it and takes only whole lines; that held with one `write` an entry.

## 6. The cache

Each process makes 500 reads and writes, three writes in ten, of 8 files of 0.2 to 16 KiB. A write is a temporary file in the same directory (`os.CreateTemp`, which makes it 0600), then a rename over the name. Every read is checked to be one whole file, by its JSON and a checksum.

| Processes | Sync before the rename | Killed | Reads | Partial | Missing | Failed | Writes | Failed | Read: p50 | p99 | Write: p50 | p99 | max | Temporary files left |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | no | 0 | 342 | 0 | 0 | 0 | 158 | 0 | 0.02 ms | 0.06 ms | 0.20 ms | 0.35 ms | 0.36 ms | 0 |
| 1 | yes | 0 | 353 | 0 | 0 | 0 | 147 | 0 | 0.07 ms | 0.32 ms | 5.7 ms | 6.9 ms | 7.1 ms | 0 |
| 10 | no | 0 | 3,569 | 0 | 0 | 0 | 1,431 | 0 | 0.08 ms | 0.40 ms | 1.2 ms | 3.1 ms | 4.5 ms | 0 |
| 10 | yes | 0 | 3,466 | 0 | 0 | 0 | 1,534 | 0 | 0.11 ms | 11.3 ms | 44.9 ms | 88.3 ms | 114 ms | 0 |
| 10 | no | 1 | 3,103 | 0 | 0 | 0 | 1,397 | 0 | 0.07 ms | 0.36 ms | 1.0 ms | 2.6 ms | 3.4 ms | 1 |
| 100 | no | 0 | 35,049 | 0 | 0 | 0 | 14,951 | 0 | 0.41 ms | 94 ms | 90 ms | 658 ms | 1.5 s | 0 |
| 100 | yes | 0 | 34,884 | 0 | 0 | 0 | 15,116 | 0 | 0.09 ms | 12.9 ms | 84 ms | 5.0 s | 15.1 s | 0 |
| 100 | no | 10 | 31,549 | 0 | 0 | 0 | 13,451 | 0 | 0.32 ms | 59 ms | 54 ms | 457 ms | 0.9 s | 5 |
| 500, 100 operations each | no | 0 | 34,876 | 0 | 0 | 0 | 15,124 | 0 | 0.42 ms | 1.1 s | 418 ms | 2.9 s | 6.4 s | 0 |
| 500, 100 operations each | yes | 0 | 34,976 | 0 | 0 | 0 | 15,024 | 0 | 0.09 ms | 16.5 ms | 99 ms | 28.2 s | 68.4 s | 0 |
| 500, 100 operations each | no | 50 | 31,495 | 0 | 0 | 0 | 13,505 | 0 | 0.42 ms | 0.6 s | 416 ms | 2.8 s | 6.2 s | 23 |

In every run all 8 files were whole at the end, no rename had to be tried again, and the files were mode 0600 in a 0700 directory.

On Linux, at 100 and 500 processes; 1 and 10 were not run there:

| Processes | Sync before the rename | Killed | Reads | Partial | Missing | Failed | Writes | Failed | Read: p50 | p99 | Write: p50 | p99 | max | Temporary files left |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 100 | no | 0 | 35,002 | 0 | 0 | 0 | 14,998 | 0 | 0.008 ms | 0.25 ms | 1.1 ms | 61 ms | 119 ms | 0 |
| 100 | yes | 0 | 35,061 | 0 | 0 | 0 | 14,939 | 0 | 0.009 ms | 0.52 ms | 14.2 ms | 30 ms | 42 ms | 0 |
| 100 | no | 10 | 31,666 | 0 | 0 | 0 | 13,334 | 0 | 0.008 ms | 0.25 ms | 1.1 ms | 52 ms | 88 ms | 4 |
| 500, 100 operations each | no | 0 | 35,007 | 0 | 0 | 0 | 14,993 | 0 | 0.009 ms | 0.29 ms | 1.3 ms | 293 ms | 515 ms | 0 |
| 500, 100 operations each | yes | 0 | 34,923 | 0 | 0 | 0 | 15,077 | 0 | 0.011 ms | 0.68 ms | 87 ms | 183 ms | 198 ms | 0 |
| 500, 100 operations each | no | 35 | 32,618 | 0 | 0 | 0 | 13,882 | 0 | 0.009 ms | 0.33 ms | 1.2 ms | 255 ms | 472 ms | 17 |

There too all 8 files were whole at the end of every run, no rename was tried again, and the files were 0600 in a 0700 directory.

- **A reader never saw a partial file, and never found a name missing:** 0 of 214,000 reads against 92,000 renames on macOS, and 0 of 204,000 against 87,000 on Linux, over the same 8 names.
- **Renaming over a file that is open, on macOS and on Linux:** allowed. The handle that was open goes on reading the old content, and a new open of the name reads the new. Removing the name while it is open is allowed too. No lock is needed for the cache, and none was used.
- **Do not sync the cache.** A synced write took 5.7 ms alone and 28 times an unsynced one. Under load the syncs queue behind each other across files: a 99th percentile of 5 s among 100 processes and 28 s among 500, and one write of 68 s. The cache is a copy of what the server holds; a file lost with the power is a miss. What the rename guarantees without a sync, after a power loss, depends on the file system, so a reader should treat a cache file that does not parse as absent, which costs it nothing it does not already check. On Linux a synced write had a median of 14 ms among 100 processes and 87 ms among 500, against 1.1 and 1.3 ms unsynced, with a 99th percentile of 30 and 183 ms. That is with the sync of section 2, which is not a disk's, so macOS's figures are the ones to plan by.
- **Without a sync, 100 processes writing flat out slowed each other:** 0.2 ms alone, 1.2 ms among 10, 90 ms among 100, 0.4 s among 500. Creating and renaming files in one directory is serialized by the file system here. A session caches a few answers per request; it is not this load. That is macOS's file system and not the design: on Linux the median stayed at 1.1 ms among 100 and 1.3 ms among 500, with a 99th percentile of 61 and 293 ms.
- **A killed writer leaves its temporary file:** 5 from 10 kills and 23 from 50 on macOS, 4 from 10 and 17 from 35 on Linux. Nothing reads them, and nothing removes them either. The cache needs a sweep of temporary files older than some minutes, at open or by `sfx cache clear`.

## Not measured

- **A Linux disk.** Linux ran in a container on a virtual machine: overlayfs on a virtual disk, on arm64. Its sync of 0.3 to 0.5 ms very likely returns before anything reaches the physical disk, so nothing here is the cost of a durable append on a Linux server or laptop, and nothing is of ext4 or XFS used directly, or of amd64. Run the two commands of the next item on such a machine and read section 2's sync columns. The cache at 1 and 10 processes, and the rerun of section 5 by itself, were macOS's only.
- **Windows.** Nothing, and it is the system §18.13 names. The spike vets and builds for it (`GOOS=windows GOARCH=amd64 go vet ./spike/outbox/` and `go build ./spike/outbox/`), and `lock_windows.go` has never run. On a Windows machine with Go 1.27, from the repository's root:

  ```
  go run ./spike/outbox -exp all -n 1,10,100 -lock all
  go run ./spike/outbox -exp all -n 500 -m 20 -ops 100 -lock lockfileex
  ```

  What to look for, in the order it would change the design:
  1. **Section 6's first lines and its "renames tried again" and "failed" columns.** Go opens files on Windows without `FILE_SHARE_DELETE` (`syscall.Open`, in Go 1.27.1), so a rename over a file that any process has open should be refused. Then a cache write fails or retries whenever a reader has the file open, and a compaction fails while anything is reading the outbox. The spike retries a cache rename 100 times a millisecond apart and counts; it does not retry compaction's renames, so section 5 may fail outright, and that would be the finding.
  2. **Section 1's "holder is killed" table for `lockfileex`:** how long Windows takes to release a dead process's lock, which its documentation does not bound.
  3. **Section 2's sync cost** (`FlushFileBuffers`), and whether antivirus scanning of each new file shows in sections 2 and 6.
  4. **Section 3's zeros.**
  5. The `O_EXCL` lock on Windows, where removing a file that is open elsewhere is refused or deferred; the spike treats "access denied" on create as contention.
- **Power loss and a crash of the operating system.** Every kill was of a process, with the kernel alive to finish its writes. Whether a synced append survives, and what a file looks like after power is lost mid-append or mid-rename, was not tested and cannot be from a process. The torn lines of section 3 were made with two writes.
- **A short write.** A full disk or a quota was not produced; the two-write rows stand in for it.
- **A network file system or a synced folder.** Section 1 says what the documentation says.
- **A holder that is alive and stuck.** A kernel lock is held for as long as its holder lives. A replayer blocked on a server that never answers was not run; a deadline on each request is the remedy proposed, and it is untested.
- **Realistic load.** Every process here writes as fast as it can. Sessions that write once every few minutes were not simulated; the single-process rows are the nearest figure.
- **A large outbox,** its size when compacted, or the caps of §18.3, which set how long a compaction holds the append lock.
- **`fsync` without `F_FULLFSYNC`** on macOS, which would be faster and promise less.
- **Batching appends under one sync,** which would raise the 207 a second. Nothing measured says it is needed.
- **Another machine.** Both systems ran on one Apple M2 Ultra, Linux in a virtual machine on it. Counts carry over; times are this machine's.
- **The other half of F0:** how often and for how long real sessions lose the server.

## What it means for the design

`docs/design/starfix.md` was not edited. These are proposed for §18.3 ("Many processes") and §18.4.

1. **Say which lock.** "Serialized by a lock file" becomes: an exclusive `flock` on Linux and macOS and `LockFileEx` on Windows, taken on a lock file of its own that is created once and never renamed, replaced or removed, through `syscall` with no new dependency. Say why not the others, since both are what one would reach for: `fcntl` record locks belong to the process, and a lock file with a lease stalls on a death and cannot be taken over safely.
2. **Drop the lease from the replay lock.** "A second lock with a lease" becomes a second lock of the same kind, which the kernel releases when its holder dies. A session tries it and, if it is held, leaves replay to the holder. Each replayed request has a deadline, so a holder that is alive cannot hold it without bound. If the maintainer wants a lease anyway, it must not be one that lets a second process in while the first may still be sending.
3. **State how a writer and a reader treat the file.** A writer, under the lock: open the outbox, end the last line if it has no newline, write the entry with one `write`, sync, close. A reader: take whole lines only, skip a line that does not parse and count it, and never take bytes after the last newline for a line. An entry needs no checksum for this: a fragment cannot parse, and the lock keeps two entries from mixing.
4. **State compaction's order.** Replay lock, then append lock. Outbox replaced first, answered file second. Nothing but the holder of both has the outbox open, which on Windows is likely a condition of the rename working at all.
5. **Say that replay is at-least-once** and that the idempotency key is what makes that correct, so that nobody later queues an op without one or reads the replay lock as a guarantee of once.
6. **Do not sync the answered file or the cache.** The outbox keeps its sync on each append: it is the promise of §18.2 that nothing is queued in memory alone, and at 4.8 ms an append it is affordable. An answer lost with the power is a repeat delivery with a key. One case to decide: an entry answered more than 7 days after it was queued, whose answer is then lost, would be refused as `stale` on the repeat and shown to the person as refused though it was applied. On Linux as measured, syncing the answered file cost little and saved little, so this rests on macOS's figures and on what a real Linux disk is expected to cost.
7. **Sweep the cache's temporary files,** and have a reader treat a cache file that does not parse as absent.
8. **Keep the store on a local disk,** and on Windows under the local application data directory, not the roaming one.
9. **F0's gate is met for two systems of three.** §18.4 is confirmed on macOS and on Linux, with the same zeros and the same failures of the alternatives. Windows is the system the design itself doubts, and the rename over an open file is a reason to expect a real difference there, in compaction and in the cache. F1 should not rely on either on Windows until the two commands above have been run there. What a sync costs on a real Linux disk is also still open; macOS's 4.4 ms is the figure to plan by until it is measured.
