# Profile of `aries`: memory, start-up and CPU

Research, 13 Oct 2026, against `main` at `32a6eaf`. It measures the
client, `aries`, as a short command and as `aries mcp`, says where it
allocates, what it keeps and how that grows, and proposes changes. It
changes no product code and decides nothing.

**Why this client is measured this way.** A developer may run from one to
hundreds of agent sessions on one machine. Each runs an `aries mcp` for
hours, and each runs short `aries` commands from hooks: `aries prime --hook`
at every session start and `aries usage --hook` after turns. So three
numbers matter that would not for an ordinary command line: the memory of
one idle `aries mcp`, times hundreds; the cost of starting a short command,
paid thousands of times a day; and what a long-lived process keeps.

**Where the numbers are from.**

| | |
|---|---|
| Go | 1.27.2, `CGO_ENABLED=0 -trimpath -ldflags "-s -w"`, never `-race` |
| macOS | 27.0 on arm64 (Apple M4 Max, 16 cores, 64 GB, 16 KiB pages). Every number is from here unless it says Linux |
| Linux | kernel 7.0.14 on arm64, 4 KiB pages, in a container of Docker Desktop on the same machine, talking to the test server on the host |
| Windows | Not run. `aries` was built and vetted for it and nothing else |
| Server | The end-to-end tests' own: an SSH server, the bridge, the daemon and a store on Dolt 2.4.2, in one test process (`TestProfileServer`, below) |
| Load | The machine was shared with three other agents running soaks and test suites: the load average was 3 to 12 throughout |

Allocation counts, bytes, retained heap and the size of an idle process
do not move with load, and are what this relies on. Times are the minimum
and the median of 30 or 40 runs, and the minimum is the one to read.

## Summary

### A short command, start to exit

macOS, release binary, 40 runs each, a project of 10 issues. Allocations
are from the same code built with a `main` that writes
`runtime.MemStats` as it exits ([How it was measured](#how-it-was-measured)).
`hello` is a Go program that prints one line: the floor.

| Command | Wall ms, min / median | User + system CPU ms | Peak RSS MiB | Allocations | Bytes |
|---|---|---|---|---|---|
| `hello` | 2.4 / 2.9 | 0.7 + 0.9 | 4.1 | | |
| `aries --version` | 5.1 / 5.5 | 2.3 + 1.6 | 14.6 | 13,398 | 1.50 MB |
| `aries --help` | 5.1 / 5.7 | 2.4 + 1.7 | 14.7 | 13,475 | 1.50 MB |
| `aries prime --hook`, not an Aries repository | 5.2 / 5.8 | 2.4 + 1.7 | 15.0 | 13,493 | 1.52 MB |
| `aries usage --hook`, not an Aries repository | 5.4 / 5.7 | 2.4 + 1.8 | 15.0 | 13,489 | 1.52 MB |
| `aries usage --hook`, nothing new in the transcript | 5.9 / 6.4 | 2.8 + 2.0 | 15.3 | 13,950 | 1.63 MB |
| `aries whoami` | 7.1 / 7.8 | 3.6 + 2.2 | 16.6 | 14,509 | 1.78 MB |
| `aries admin prices` | 7.5 / 8.2 | 3.6 + 2.3 | 16.6 | 14,431 | 1.77 MB |
| `aries list` | 8.3 / 9.0 | 3.7 + 2.4 | 16.7 | 14,802 | 1.80 MB |
| `aries ready` | 10.3 / 11.3 | 4.0 + 2.7 | 16.7 | 14,734 | 1.79 MB |
| `aries show ID` | 10.8 / 12.0 | 4.0 + 2.7 | 16.7 | 14,894 | 1.81 MB |
| `aries prime --hook` | 11.6 / 12.6 | 3.9 + 2.6 | 16.8 | 15,073 | 1.83 MB |
| `aries create` | 14.0 / 20.7 | 4.0 + 2.6 | 16.6 | 14,579 | 1.79 MB |
| `aries usage --hook`, 5 new responses (one turn) | 21 to 36, single runs | 4.7 + 4.0 | 18.0 | 15,190 | 1.92 MB |

**The fixed share.** `aries --version` is what every command pays before it
does anything: 5.1 ms, 3.9 ms of CPU, 14.6 MiB and 13,398 allocations.
That is 72% of `whoami`'s time, 67% of its CPU, 88% of its peak memory and
92% of its allocations. Of the 13,398 allocations, 13,194 are package
`init` functions, and 11,464 of those compile 21 regular expressions that
most commands never match against.

**Scaling with the project.** The client's cost does not grow with the
project: allocations are the same at 10, 1,000 and 10,000 issues, because
every list is a page (50 by default, 500 at most). The server's time
grows, and the client waits for it:

| Command | 10 issues | about 1,000 | about 10,000 |
|---|---|---|---|
| `aries ready`, wall ms min | 10.3 | 15.8 | 50.1 |
| `aries prime --hook` | 11.6 | 15.0 | 50.1 |
| `aries list` | 8.3 | 11.0 | 29.7 |
| `aries list -n 500` | 8.6 | 34.3 | 56.3 |
| `aries show ID` | 10.8 | 12.6 | 10.3 |
| `aries create` | 14.0 | 14.9 | 25.7 |

`aries list -n 500`, printing a page of 500, allocates 26,096 times and
2.56 MB as text and 15,578 times and 2.61 MB with `--json`, and spends
5.2 ms in the kernel as text against 2.8 ms as JSON: the table reaches
standard output in 4,000 writes.

**`usage --hook` on a transcript.** It streams from the offset it kept.
A run with nothing new connects to nothing and costs what `--version`
does. The first run on a file reads all of it:

| Transcript | First run: wall, user CPU, peak RSS, bytes allocated | Later run, one turn |
|---|---|---|
| 2 MB, 300 responses | 63 ms, 8.8 ms, 19.6 MiB, 2.8 MB | 21 to 36 ms, 4.7 ms, 18 MiB |
| 62 MB, 5,000 responses | 645 ms, 94 ms, 24.7 MiB, 22 MB | the same |
| 761 MB, 20,000 responses, a 3 MB line every 200 | 3.6 s, 732 ms, 37.0 MiB, 60 MB | the same |

Most of the first run's wall time is the server taking 500 records a
request.

### One `aries mcp`, and N of them

| | macOS arm64 | Linux arm64 |
|---|---|---|
| Idle, MCP handshake done, not yet connected: RSS | 20.3 MiB | 18.7 MiB |
| Idle and connected: RSS | 22.1 MiB (21.6 to 22.7 over 200) | 20.6 to 21.4 MiB |
| of which the process's own (macOS physical footprint; Linux `Private_Dirty`) | 11.5 to 12.0 MB | 10.9 to 11.7 MiB |
| of which the binary's pages, shared by every `aries` | about 7 MB | 9.7 MiB |
| Go heap in use after a collection | 1.40 MiB, 8,100 objects | 1.40 MiB |
| Goroutines; their stacks | 16; 0.7 MiB | 16; 0.9 MiB |
| Threads; file descriptors | 14; 7 | 14; 7 |
| 50 idle and connected: RSS summed; their own | 1,107 MiB; 598 MB | 542 MiB; 531 MiB (binary pages evicted, see below) |
| 200 idle and connected: RSS summed; their own | 4,426 MiB; 2,386 MB (11.9 each) | 1,988 MiB; 1,967 MiB (9.8 each) |
| After 3,000 tool calls: its own | 15.8 MB, level from about call 1,500 | not measured |

One tool call allocates 160 to 240 times and 250 to 300 KB, wherever it
goes; about 225 KB of that is seven 32 KiB buffers inside the MCP SDK.
`list` with `limit: 500` allocates 16 MB and 2,583 times. At rest a
connected `aries mcp` sends one `renew` a minute and does nothing else.

**What grows.** Nothing without bound. Over 4,000 tool calls with 16
dropped connections and results of up to 300 KB, the heap in use after a
collection went from 2,316 KiB at call 100 to 2,348 KiB at call 4,000.
The one thing a session grows and keeps is the frame decoder's line
buffer, which doubles to fit the largest frame the connection has seen
and stays that size until the connection ends: 64 KiB to start, 4 MiB at
most.

### The binary

| Target | Bytes |
|---|---|
| linux/amd64 | 11,927,712 |
| linux/arm64 | 11,010,208 |
| darwin/arm64 | 11,289,170 |
| windows/amd64 | 12,281,344 |

### What 200 sessions on one laptop cost today

- **Memory.** 200 idle `aries mcp` are 2.4 GB of memory that is theirs
  alone on macOS (4.4 GB as `ps` sums RSS, which counts the binary's
  shared pages 200 times), and 2.0 GB on Linux. Sessions that have made
  thousands of calls settle near 15.8 MB each, so 3.2 GB. A session that
  has read one large page of comments keeps up to 2 MiB more, and 4 MiB
  at the very most: 800 MB over 200 sessions in the worst case.
- **CPU at rest.** None in the client beyond 200 `renew` requests a
  minute, which the server sees as 3.3 a second.
- **Hooks.** A session start costs one `aries prime --hook`: 12 to 50 ms
  of wall time by the project's size, 6.5 ms of CPU. A turn costs one
  `aries usage --hook`: 21 to 36 ms, 8.7 ms of CPU, 18 MiB for that long.
  200 sessions ending a turn each minute are 1.7 s of CPU a minute.
- **The first limit met is not memory.** With the server's default
  limits the 33rd session of one principal is refused: `busy: alice is
  at its connection limit (32)`. `conns_per_principal` is 32
  (`internal/server/limits.go:83`), every `aries mcp` holds a connection
  for as long as it runs, and a developer's sessions all sign in with
  one key. 200 sessions on one laptop do not work today without raising
  it. Measured: 40 started, the 33rd refused.
- **On the server**, in the test's single process, each connection is 9
  goroutines, about 163 KB of heap and 41 KB of stack. A real server
  adds an `sshd` child and an `ariesd stdio` bridge process for each,
  which this did not measure.

## Proposed optimizations

Ranked by saving against risk. "Measured" means a prototype was built
outside the repository with `go build -overlay` and measured as above;
"estimated" means it follows from a measurement of today's code.

### 1. Cut the `list` tool's page by search, not one issue at a time

`internal/mcpserver/tools.go:624` (`list`), lines 641 to 644. A page over
the 2,000-token budget is cut by marshaling the whole page, dropping one
issue, and marshaling again. About 48 issues fit, so `limit: 500`
marshals a shrinking 60 KB document 450 times.

- **Now:** `limit: 500` is 20.3 ms and 17.1 MB in the tool, before the
  second request it then makes; in the whole process, 16 MB and 803
  collections per 100 calls. `limit: 100` is 0.68 ms and 537 KB.
- **Change:** `sort.Search` over the same predicate. The size grows with
  the count, so the answer is the same.
- **Saving, measured:** `limit: 500` 0.23 ms and 0.44 MB (87 times less
  time, 39 times fewer bytes); `limit: 100` 0.08 ms and 74 KB. `limit:
  50`, where one or two drops were enough, goes from 35 to 61
  microseconds. The package's tests pass with the prototype.
- **Risk:** low. Readability is the same. The same loop is in `ready`,
  `blocked`, `who` (`tools.go:532`, `542`, `552`), `comments` and
  `history` (`tools.go:775`, `796`), `show` (`719`, `723`), `recall`
  (`memory.go:135`), `inbox` (`inbox.go:108`) and `cost` (`cost.go:70`).
  `comments` with `limit: 100` on long comments is 3.4 ms and 6.2 MB;
  the others are bounded by small limits and are not worth touching
  alone, but one helper would serve all.

**Since this was written (14 Oct 2026).** Done: one helper,
`fitPrefix` in `budget.go`, searches for how many items fit at every
site above but `cost` and `show`'s files, which still drop one at a
time because a count or a sum there could make the search's answer
differ. It also cuts `start`'s memories, and `digest`, whose order of
drops it keeps. Tests hold every converted site to the old loop byte
for byte. Measured as above, by the minimum of six runs: `limit: 500`
17.2 ms and 17.0 MB to 0.21 ms and 0.44 MB; `limit: 100` 0.57 ms and
537 KB to 70 microseconds and 74 KB; `limit: 50` 29 to 54
microseconds, since two drops were enough there (trying one drop first
did not help); `comments` at 100 3.0 ms and 6.2 MB to 0.20 ms and
0.80 MB; the largest digest 1.1 ms and 1.4 MB to 90
microseconds and 125 KB. `prime` drops at most a couple of dozen items
from a document of a few kilobytes (0.11 ms in the worst case tried),
in an order that also depends on which memories are pinned, and was
left as it was.

### 2. Compile no regular expression until it is used

Package-level `regexp.MustCompile` in `internal/proto/ops.go:288` and
`289`, `cost.go:147` and `191`, `hours.go:131`, `version.go:180`;
`internal/client/config.go:70` to `79`; `internal/gitx/gitx.go:92` to
`97` and `changes.go:153`; `internal/capture/claude.go:45`;
`internal/release/release.go:45`; `internal/agentsetup/hook.go:104` and
`107`; `internal/cli/admin_key.go:35`. Twenty-one patterns, compiled by
every `aries` whether it is `--version`, a hook outside an Aries
repository, or `aries mcp`. `UsageRequestID` (`{0,254}`) and the config's
`hostRE` (`{0,252}`) are the expensive ones: a counted repetition expands
to that many states.

- **Now:** 938 KB, 11,464 allocations and 0.52 ms of the 1.04 ms all
  `init` functions take: `internal/proto` 454 KB and 5,839 allocations,
  `internal/client` 330 KB and 3,924.
- **Change:** compile on first use (`sync.OnceValue`), or for the four
  checks of `.aries.yaml` that every connecting command makes, a loop
  over the bytes in place of the pattern.
- **Saving, measured** with every pattern made lazy: `aries --version`
  and both hooks outside a repository go from 13,398 allocations to
  2,054, from 1.50 MB to 0.57 MB, from 15.5 to 13.7 MiB of peak RSS and
  from 2.9 to 2.6 ms of user CPU. `whoami` goes from 14,510 to 7,032
  allocations and 1.78 to 1.15 MB: it still compiles the four config
  patterns. Written as loops those would go too: about 3,000
  allocations, **estimated**. An idle `aries mcp` keeps 243 KiB less heap
  (1,409 to 1,166 KiB).
- **Risk:** low. A pattern that does not compile would panic at first
  use, not at start, so a test must touch each one. `proto.UsageRequestID`
  and `UsageModel` are exported variables the store reads: they become
  functions, a change inside the module. Nothing differs by platform.
- **Coming:** `internal/outbox` calls `internal/secretscan`, whose 13
  patterns cost 144 KB and 937 allocations at `init`. `aries` does not
  link the outbox at this commit; when it does, every command pays that
  too unless those are lazy.

**Since this was written (14 Oct 2026).** Done, with
`internal/lazyregexp`. `internal/proto`'s, `release`'s and
`secretscan`'s were already lazy, for `ariesd`'s sake; the rest that
`aries` links (`client`, `gitx`, `capture`, `agentsetup`, `cli`) are now
too. The config's patterns stay patterns, compiled by the commands that
read a config.
Measured as above on macOS, before and after, at that later `main`:
`aries --version` 7,044 allocations and 1.01 MB at exit to 1,993 and
0.57 MB, Aries's packages' `init` 446 KB to 10 KB; peak RSS 14.2 to
13.1 MiB (minimum of 40), CPU 4.25 to 3.97 ms (minimum; median 4.48 to
4.17); `prime --hook` outside a repository the same. `TestInitAllocations`
(`cmd/aries`) holds Aries's packages' `init` to 32 KiB and all of it to
1 MiB.

### 3. Collect sooner in the long-lived commands

`internal/cli/agent.go:179` (`cmdMCP`), and `aries tui` and `aries watch`.
The runtime does not collect until the heap reaches 4 MiB, and `aries mcp`
allocates 4.4 MB before its first tool call (package `init` 1.2 MB,
`mcpserver.New` 1.85 MB, the MCP handshake and `tools/list` the rest), of
which 1.2 MiB stays in use. The pages touched on the way stay with the
process.

- **Now:** 11.7 MB of its own on macOS, 10.9 to 11.7 MiB on Linux.
- **Change:** `debug.SetGCPercent(50)` in those three commands, unless
  `GOGC` is set.
- **Saving, measured** by setting `GOGC=50` in the environment: Linux
  7.05 and 7.49 MiB of `Private_Dirty` against 11.74 and 10.90, about
  3.4 MiB or 31% a process, 680 MiB over 200. macOS 10.8 to 11.4 MB
  against 11.5 to 11.8, about 0.5 MB. Two processes each on Linux and
  three on macOS: small samples.
- **Risk:** low. A collection every three tool calls in place of every
  six, each on a heap of 1.4 MiB. Short commands must not get it: they
  exit before their first collection today.
- **Not this:** `GOMEMLIMIT=6MiB` measured the same on Linux and 10.4 MB
  on macOS, but a limit below what one large result needs makes the
  collector run continuously while that result is held.
  `debug.FreeOSMemory` once after start gave back 1.5 to 2.0 MiB on
  Linux and nothing on macOS.

**Since this was written (14 Oct 2026).** Measured again, larger, and
not done: the saving holds, but the CPU it costs is not negligible.
Fifty idle `aries mcp` against the test server, each after one `prime`,
two rounds each, `GOGC=50` in the environment against none: on Linux
(the container above, `Private_Dirty`) 10.1 to 10.4 MiB a process
against 8.0 to 8.1, about 2.2 MiB or 22% less; on macOS (physical
footprint) 11.2 against 10.5 MiB, 0.7 MiB or 6% less. A session of 300
tool calls (`prime`, `ready`, `list`, `show`, `comment`, `comments`,
`who`, `inbox`, `digest`, `history` in turn), five runs each: the
client's CPU went from 228 to 253 ms to 345 to 421 ms on Linux, and
from 143 to 195 ms to 224 to 269 ms on macOS, about half as much
again, or 0.3 to 0.5 ms a call. A collection is due at a 2 MiB heap in
place of 4 MiB, and a call allocates about 250 KB, so it runs about
twice as often. Whether 2 MiB a session is worth that is a choice
between memory and CPU, not a free saving.

### 4. Parse a private key only when the server asks it to sign

`internal/client/conn.go:733` (`readDefaultIdentity`) and `780`
(`readKeyFile`). With no key configured, every default identity file of
`~/.ssh` is read and parsed before the dial, in the order `id_rsa`,
`id_ecdsa`, `id_ed25519` (line 478). Every one parsed is offered, up to
five, so none is parsed and then left unoffered; but SSH asks only the
key the server accepts to sign, and a public key is enough to offer.

- **Now:** an Ed25519 key costs 15 microseconds and 57 allocations, an
  ECDSA key 25 microseconds, an RSA 3072 key **2.5 ms** and 75 KB:
  parsing validates it. Someone with an `id_rsa` beside the Ed25519 key
  the server knows pays 2.5 ms on every command and every hook, as much
  CPU as all of `aries --version`, and the RSA key is offered, and
  refused, before the Ed25519 one. A configured key file
  (`ARIES_KEY_FILE`, or `key:`) is read alone and nothing else is
  touched; ssh-agent is asked only when no file can sign.
- **Change:** take the public half from `NAME.pub`, or from the OpenSSH
  container, which holds it unencrypted, and parse the private key on
  the first call to `Sign`.
- **Saving, estimated** from the benchmark: 2.5 ms of CPU a command for
  people with an RSA default identity; nothing for anyone else.
- **Risk:** medium. This is decision 35's code. What cannot be read or
  parsed is known later, at signing, so the lists `whoami` and a
  refusal show (`skipped`) must still be right, and a key file that is
  passphrase-protected must still be told from one that is corrupt
  before the offer. Not prototyped.

**Since this was written (14 Oct 2026).** Not done: it would change
which keys are offered and what `whoami` and a refusal say, for one
kind of file. Nearly all of the 2.5 ms is `crypto/rsa` checking that
the key's numbers agree (`ParsePrivateKey` calls `rsa.PrivateKey`'s
`Validate` and `Precompute`), and that check tells a usable RSA key from
a file that parses but cannot sign. Today such a file is skipped
before the offer, and `whoami` and a refusal list it as "not a private
key aries can read"; parsed at signing, it would be offered by its public
half, counted among the five, and fail only if the server accepted it.
Measured: `whoami` with default identities, an RSA 3072 key the server
does not know and the Ed25519 key it does, 11.2 ms and 9.7 ms of CPU
(minimum of 40) against 8.4 ms and 6.8 ms with the Ed25519 key alone.
The options are to accept that difference for files that are
corrupt, to parse in the background while the connection is made
(which saves the wall time and none of the CPU, and changes the point
at which "no SSH key available" is reported), or to leave it: naming
the key with `ARIES_KEY_FILE` or `key:` already reads that file
alone.

### 5. Do not keep the decoder's buffer at its largest

`internal/proto/frame.go:112` to `116` (`NewDecoder`) and `123`
(`Decode`). `bufio.Scanner` starts at 64 KiB and doubles to fit a line,
to `MaxFrame` (4 MiB), and never shrinks.

- **Now:** the first frame over 64 KiB allocates three times its size
  (a 1 MiB frame: 5.2 MB; a 3.9 MiB frame: 12.4 MB) and leaves a buffer
  of the next power of two for the life of the connection. A result is
  then held three times at its peak: in that buffer, in `Frame.OK`
  (`json.Unmarshal` copies it), and as the decoded value, whose strings
  are copies again (16 MB allocated to decode a 3.9 MiB list, about
  that size live). The store caps a text field at 65,535 bytes and a
  page at about 1 MiB, so a frame over about 1.3 MiB should not occur,
  and the buffer a session ends up with is 2 MiB at most in practice.
  Measured in the long session: 0.8 MB kept after pages of 300 KB.
- **Change:** read with a `bufio.Reader` of 64 KiB and gather a longer
  line in a buffer of its own that is dropped once the frame is decoded
  (as `internal/capture` already reads transcripts, `claude.go:246`).
- **Saving, estimated:** up to 4 MiB a connection, 2 MiB in practice,
  in each `aries mcp`, `aries tui` and `aries watch` that has ever read a
  large result, and in the daemon for each connection. The doubling's
  garbage goes too.
- **Risk:** medium-low. It is the wire decoder of both ends, so the
  refusal of a frame over `MaxFrame`, the skipping of a bad line and
  the end of stream must stay exactly as tested; it wants a fuzz test
  against the present decoder. Not prototyped.

**Since this was written (14 Oct 2026).** Done, as `profile-ariesd.md`'s
proposal 1 says, with a 4 KiB buffer rather than 64 KiB. Measured with
ten idle `aries mcp` against the test server, heap in use after two
collections: 1.10 to 1.04 MiB after one `prime`, and 1.71 to 1.47 MiB
after one `show` of an issue with three texts of 60,000 bytes. Their
private memory moved by less than its spread between processes
(10.6 to 12.1 MiB).

### 6. Write a table in one piece

`internal/cli/commands.go:276` (`printSummaries`), with
`internal/safetext/safetext.go:186` (`Writer.Write`), which copies what
it is given twice and flushes to the terminal on every call.
`text/tabwriter` writes each cell and each run of padding separately.

- **Now:** 8 writes to standard output a row: 80 for `aries ready`, 400
  for `aries list`, 4,000 for `aries list -n 500`, with 10,539 allocations.
- **Change:** a `bufio.Writer` between the table and the escaping
  writer, flushed after the table.
- **Saving, measured:** 1, 2 and 11 writes, and 3,043 allocations at 500
  rows; in the whole process that is the 1.6 to 2.4 ms of system time
  by which the text of 500 rows exceeds its JSON. For the default page
  it is a fifth of a millisecond.
- **Risk:** low, if the buffer is the table's and not the `Writer`'s:
  `aries watch` relies on each line reaching the terminal as it is
  printed. The other tables (`blocked`, `who`, `admin`) print the same
  way.

**Since this was written (14 Oct 2026).** Done for the issue table
of `list`, `ready` and `blocked`: `printSummaries` lays the table
out in a buffer of its own and writes it once, as `cost` and `hours`
already did; the escaping writer is unchanged. `BenchmarkClientPrintSummaries`,
before and after: 10 rows 80 writes, 232 allocations and 8.2 µs to 1,
90 and 6.7 µs; 50 rows 400, 1,078 and 38.6 µs to 2, 339 and 30.8 µs;
500 rows 4,000, 10,539 and 374 µs to 11, 3,054 and 291 µs, and 444 KB
to 550 KB allocated. Its writer counts 4 KiB pieces; standard output,
which takes a string whole, gets one write. Tests pin the table's text
and the single write. `who`, `history`, `digest` and the `admin` tables
were left as they were.

### Smaller, and questions rather than optimizations

- **`conns_per_principal` and many sessions.** Not a cost but a wall:
  see the summary. Either the default rises, or an idle `aries mcp` that
  holds no claim lets its connection go and dials again at its next
  call (2 ms of CPU, measured as `whoami` less `--version`). The second
  also saves the server its side of 200 idle connections, and about
  0.9 MB a process in the client (10.8 against 11.7 MB, measured). It
  changes what `aries who` shows, since the minute's `renew` is what
  keeps a session "seen", and it delays "inbox: N new" to the next
  call, which is when it is delivered anyway. A design question, not
  prototyped.
- **`internal/capture/claude.go:277`.** `f(append(buf, chunk...), n)`
  does not keep the grown slice, so each line longer than the 64 KiB
  read buffer allocates its length again: 32 MB to read a 30 MB
  transcript of 100 KiB tool results. `buf = append(buf, chunk...)`
  then `f(buf, n)` fixes it. Low risk, and it matters only on a first
  run.
- **`internal/board/render.go:130` (`boardLines`).** Each frame builds
  a line for every row the board holds, up to 700, then shows a window:
  0.39 ms, 7,281 allocations and 400 KB a frame, one frame an event.
  At ten events a second that is 4 MB a second of garbage and under 1%
  of a core. Not worth a change yet.

### Not worth doing

- **Replacing the MCP SDK.** It, `jsonschema-go` and what only they
  bring (`encoding/gob`, `segmentio/encoding`, `uritemplate`,
  `log/slog`) are 786 KB of the 5.8 MB of code and data that can be
  attributed to a package, so about 1.6 MB of the binary; building the
  24 tools at start is 1.4 ms, 1.85 MB and 22,313 allocations, paid by
  `aries mcp` alone; and each tool call allocates about 225 KB inside it,
  because its `internal/json.Unmarshal` makes a decoder with a new 32
  KiB buffer each time and a call makes about seven. That last is worth
  reporting upstream. None of it is worth owning a JSON-RPC server for.
- **Replacing YAML.** 197 KB of symbols and 25 KB at `init`; finding,
  reading and validating `.aries.yaml` is 36 microseconds and 149
  allocations.
- **Fewer goroutines, smaller SSH buffers.** Of the 16 goroutines of a
  connected `aries mcp`, 9 are `golang.org/x/crypto/ssh`'s, 3 the MCP
  SDK's, 2 `os/signal`'s, and 2 are Aries's (the connection's reader
  and the renewal ticker). All their stacks together are 0.7 MiB, and
  being connected adds 180 KiB of heap, 64 KiB of it the decoder's
  buffer. The SSH library's windows and packet buffers are not
  settable.
- **Dropping `aries upgrade`'s HTTP stack.** `net/http`, `crypto/tls`,
  HTTP/2 and what they need are 778 KB of attributed symbols, about
  1.6 MB of the binary. The MCP SDK imports `net/http` as well, so both
  would have to go.
- **Skipping the decode of transcript lines that are not responses.**
  Every line is unmarshaled to learn its type, tool results included.
  It would cut most of the 732 ms a first run spends on a 761 MB
  transcript, but a later run reads one turn, and matching on the
  line's bytes ties the reader more tightly to a format that is not
  Aries's.

## Evidence

### 1. A short command

**Start to exit, by difference** (macOS, minimum of 40):

| Step | Adds | How it is known |
|---|---|---|
| Process start and Go runtime | 2.4 ms wall, 1.6 ms CPU, 4.1 MiB | `hello` |
| Being `aries`: an 11 MB binary mapped, 112 packages initialized | 2.7 ms wall, 2.3 ms CPU, 10.5 MiB, 13,194 allocations, 1.20 MB | `aries --version` less `hello`; `GODEBUG=inittrace=1` |
| of which `init` functions | 1.04 ms (each package's minimum over 40 runs, summed) | `inittrace` |
| `cli.Run` for `--version` | 1.2 microseconds, 14 allocations | `BenchmarkClientRun` |
| Reading the hook's JSON, flags, the search that finds no `.aries.yaml` | 0.1 ms, 95 allocations | `prime --hook` outside a repository less `--version`; `BenchmarkClientLoadConfigNone`: 15 microseconds, 40 allocations |
| Finding, checking and parsing `.aries.yaml` three directories up | 36 microseconds, 149 allocations, 15.8 KB | `BenchmarkClientLoadConfig` |
| Loading one Ed25519 key file | 15 microseconds, 57 allocations, 3 KB | `BenchmarkClientIdentity` |
| Dial, key exchange, host key check, authentication, session, hello and welcome, one request, close | 2.0 ms wall, 1.9 ms CPU, 2.0 MiB, 1,111 allocations, 274 KB | `whoami` less `--version` |
| The same with both ends in one process, on loopback | 0.87 ms, 954 allocations, 283 KB | `BenchmarkClientDial` |
| One request and its empty result, both ends | 29 microseconds, 17 allocations | `BenchmarkClientCall` |
| Decoding and printing | below | |

**`init`, by who pays for it** (`GODEBUG=inittrace=1 aries --version`, 40
runs, each package's minimum):

| Group | ms | Bytes | Allocations |
|---|---|---|---|
| Aries packages (9) | 0.520 | 938,160 | 11,464 |
| MCP SDK, `jsonschema-go`, `encoding/gob`, `segmentio` (11) | 0.194 | 165,512 | 886 |
| the rest of the standard library (31) | 0.118 | 29,056 | 119 |
| `go.yaml.in/yaml/v3` | 0.022 | 25,664 | 274 |
| `golang.org/x/crypto`, `x/sys`, `x/term` (7) | 0.092 | 19,472 | 224 |
| `net`, `net/http`, `crypto/tls`, `crypto/x509`, `mime` (19) | 0.054 | 19,064 | 134 |
| `crypto` (34) | 0.040 | 6,560 | 93 |
| **All 112** | **1.040** | **1,203,488** | **13,194** |

The largest single packages: `internal/proto` 454 KB and 5,839
allocations, `internal/client` 330 KB and 3,924, `jsonschema-go` 81 KB,
`encoding/gob` 69 KB (it registers its types; `aries` never encodes gob),
`internal/release` 40 KB, `internal/gitx` 40 KB, `internal/capture`
31 KB. With the prototype of proposal 2 the total is 0.64 ms, 273 KB and
1,847 allocations. Command and flag tables are not a cost: the command
table is about 40 struct literals, and flags are built by the command that
runs.

**What a command that never connects pays that it need not.** `--version`,
`--help`, and `prime --hook` and `usage --hook` outside an Aries
repository (which print nothing and exit 0, as they must) do 100 to 300
allocations of their own work on top of 13,194 of initialization, 11,464
of which are the regular expressions. They also carry the 10.5 MiB by
which `aries` exceeds `hello`, which is the binary's size and is not
something a command can decline.

**Keys (decision 35).** With `ARIES_KEY_FILE` or `key:` set, that one
file is read, tried as a public key and then parsed as a private one,
and nothing in `~/.ssh` is opened; ssh-agent is dialed only if the file
cannot sign. With neither set, ssh-agent is asked first, then each of
`id_rsa`, `id_ecdsa`, `id_ecdsa_sk`, `id_ed25519`, `id_ed25519_sk` is
read and, except the `_sk` ones, parsed; all that parse are offered in
that order. The agent was never used here, by rule, so its cost is not
measured.

**The handshake.** The key exchange is `mlkem768x25519-sha256`, the
library's default; the host key is Ed25519 and the pinned check is one
SHA-256 and a string comparison. In a CPU profile of
`BenchmarkClientDial`, which is about 1 ms of CPU a dial for both ends,
the client's half of the key exchange is 9% of the samples and the
server's 13%; nearly all the rest is the two ends and their goroutines
waking each other. The handshake is not where a short command's time
goes: the 2 ms it adds, most of it starting threads and touching code
for the first time, is less than the 2.7 ms `aries` spends before `main`.

**Printing** (`BenchmarkClientPrintSummaries`, `BenchmarkClientLine`,
`BenchmarkClientJSON`, `BenchmarkClientWriter`):

| | Time | Allocations | Writes to standard output |
|---|---|---|---|
| Table of 10 issues | 8 microseconds | 232, 12.7 KB | 80 |
| Table of 50 | 38 microseconds | 1,078, 46.6 KB | 400 |
| Table of 500 | 369 microseconds | 10,539, 444 KB | 4,000 |
| `safetext.Line`, clean title | 76 ns | 0 | |
| `safetext.Line`, title with an escape sequence | 341 ns | 3, 116 B | |
| `safetext.JSON`, a clean 62 KB document | 73 microseconds | 1, 64 KB (the conversion to a string to scan it) | |
| `safetext.JSON`, the same with one bidi character | 201 microseconds | 27, 482 KB | |

Escaping is cheap where nothing needs escaping, which is the usual
case. `--json` is one `Marshal`, one scan and one write.

### 2. `aries mcp` at rest and at work

**What it holds.** Go heap in use after two collections: 1,224 KiB and
7,292 objects after the MCP handshake and `tools/list`; 1,403 KiB and
8,106 objects once connected. Being connected is 180 KiB: the decoder's
64 KiB buffer, the SSH client's state, and 9 goroutines. The runtime has
11.2 MiB of heap mapped and 17.9 MiB in all (`MemStats.Sys`), of which
8.5 MB is dirty on macOS; the other 3.5 MB of the footprint is 1.2 MB of
the binary's data that relocation dirties, 0.5 MB of page tables, 0.5 MB
of allocator metadata and the stacks.

**Goroutines** of a connected `aries mcp`, 16: `main` in the SDK's
`Server.Run`; `os/signal`'s loop and `NotifyContext`'s; the SDK's session
wait, its reader of incoming messages and its reader of standard input;
the renewal ticker (`mcpserver.renewLoop`); and nine of the SSH client's
(`mux.loop`, `kexLoop`, the transport's read loop, `handleGlobalRequests`,
`handleChannelOpens`, `Session.wait`, `mux.Wait`, and the two channel
readers that are Aries's `Conn.read` and the copy of the session's
standard error). Their stacks are 0.7 MiB together.

**Tools and schemas.** 24 tools. `tools/list` answers with 10,336 bytes
of JSON, 10,371 on the wire, which is the tool schema set a harness
reads at every session start. Building them at start, by reflection
(`mcpserver.New`): 1.4 ms, 1.85 MB, 22,313 allocations. One `tools/list`
with both ends in one process: 0.95 ms, 1.69 MB, 5,244 allocations.

**Per tool call** (whole process, 100 calls of each after one to warm
it, a project of about 10,000 issues; a snapshot's own cost, 37
allocations and 14.6 KB a call, is in these numbers):

| Tool | Allocations | Bytes | Request, result bytes | ms a call | Collections per 100 |
|---|---|---|---|---|---|
| `prime` | 240 | 294,433 | 90, 2,381 | 45.3 | 22 |
| `ready` | 157 | 270,220 | 90, 2,845 | 45.5 | 15 |
| `ready`, `limit: 100` | 508 | 922,049 | 101, 12,803 | 53.7 | 54 |
| `blocked` | 156 | 252,094 | 92, 483 | 1.9 | 16 |
| `list` | 181 | 273,321 | 89, 2,965 | 15.3 | 16 |
| `list`, `limit: 500` | 2,583 | 15,953,591 | 100, 13,173 | 87.4 | 803 |
| `show` | 202 | 270,491 | 107, 1,703 | 2.9 | 18 |
| `show`, `full` | 221 | 303,442 | 119, 5,317 | 3.0 | 20 |
| `comments` | 221 | 334,049 | 111, 9,425 | 0.7 | 24 |
| `history` | 470 | 305,928 | 111, 1,842 | 0.7 | 22 |
| `who` | 181 | 266,630 | 89, 1,570 | 0.6 | 18 |
| `inbox` | 161 | 251,787 | 91, 148 | 0.6 | 17 |
| `digest` | 183 | 272,929 | 92, 2,482 | 86.8 | 16 |
| `recall` | 197 | 256,016 | 106, 566 | 1.0 | 18 |
| `cost` | 199 | 256,135 | 102, 336 | 0.9 | 17 |
| `create` | 216 | 257,806 | 518, 160 | 20.2 | 16 |
| `comment` | 198 | 255,874 | 513, 152 | 21.7 | 15 |
| `update` | 244 | 256,858 | 128, 164 | 24.9 | 16 |
| `remember` | 215 | 256,596 | 158, 539 | 10.0 | 17 |
| a refusal (`show` of no such issue) | 191 | 255,303 | 104, 289 | 0.4 | 18 |

The time is the server's. The floor of about 250 KB is the SDK's: an
allocation profile of the run puts 457 MB of 2,165 MB in
`segmentio/encoding/json.(*Decoder).readValue` (`json.go:421`, the 32 KiB
buffer of a decoder made for each `Unmarshal`), which is 218 KB a call,
and 1,513 MB in `bytes.Clone` under `encoding/json/v2.Marshal`, nearly
all of it `list` with `limit: 500` measuring itself.

**The budget** (`internal/mcpserver/budget.go`). `Tokens` is the bytes
plus two, over three. `size` marshals the whole result to count them:
1.8 microseconds and 1.4 KB for 10 issues, 8.4 microseconds for 50, 81
microseconds and 66 KB for 500 (`BenchmarkClientSize`). A result under
budget pays it once. One over budget pays it once for each item
dropped. `prime` with its own cuts is 6.8 microseconds and 62
allocations (`BenchmarkClientPrime`). `BenchmarkClientListFit` and
`BenchmarkClientCommentsFit` are the costly cases, in proposal 1.

**Timers.** One: `renewLoop`'s ticker, every 60 s. A tick does nothing
unless the session holds a claim or is connected; connected, it is one
`renew` request, and every 5 minutes for each held issue the paths its
work touched, which runs `git` (`gitx.IssuePaths`; not measured).

**The outbox** is not linked into `aries` at this commit (`go list -deps
./cmd/aries` names neither `internal/outbox` nor `internal/secretscan`).
As a library, on macOS (`BenchmarkClientAppend`, `BenchmarkClientList`):

| Entries waiting | `Append` | Allocations | `List` |
|---|---|---|---|
| 0 | 8.9 ms | 174, 61 KB | 21 microseconds |
| 100 | 14.6 ms | 488, 259 KB | 0.22 ms, 185 KB |
| 900 | 9.0 ms | 2,929, 1.83 MB | 1.06 ms, 1.52 MB |

An entry of a 270-byte comment is 475 bytes in the file. An append is
dominated by its sync, which on macOS is `F_FULLFSYNC`; it reads the
whole file to count what waits, so its allocations grow with the queue,
to the cap of 1,000 entries and 4 MiB.

**N of them.** In the summary. macOS: `ps` for RSS, `vmmap --summary`
and `footprint` for the physical footprint, which is dirty memory and
leaves out the binary's clean pages. Linux: `/proc/PID/smaps_rollup`.
In the container the binary was on a read-only bind mount and its pages
were evicted once many processes ran (`Private_Clean` and `Shared_Clean`
near zero at 50 and 200), so the Linux RSS of many processes understates
what a machine with the binary on its own disk would show; `Private_Dirty`
is the number to trust there.

### 3. Long-lived growth

**One `aries mcp`, 4,000 tool calls**: `prime`, `ready`, `list` of 100,
`show`, `comment`, `start` then `finish`, `comments`, `create`, `digest`
and `remember` in turn; every 100 calls a `show`, `comments` and
`history` of an issue with a 58 KB body and 40 comments of 7.8 KB; every
250 calls every connection dropped at the server. 400 calls were refused
(the same memory key written again) and so exercised the error path.

| After | Heap in use | Objects | Heap mapped | Stacks | RSS | Collections |
|---|---|---|---|---|---|---|
| the first call | 1,403 KiB | 8,106 | 7.2 MiB | 768 KiB | 23.1 MiB | 4 |
| 100 calls | 2,316 KiB | 10,251 | 11.0 MiB | 992 KiB | 25.9 MiB | 43 |
| 1,000 | 2,306 KiB | 10,611 | 10.9 MiB | 1,120 KiB | 27.8 MiB | 273 |
| 2,000 | 2,335 KiB | 10,664 | 10.8 MiB | 1,216 KiB | 27.7 MiB | 531 |
| 3,000 | 2,332 KiB | 10,642 | 10.9 MiB | 1,120 KiB | 28.3 MiB | 794 |
| 4,000 | 2,348 KiB | 10,745 | 10.8 MiB | 1,216 KiB | 29.1 MiB | 1,054 |

The difference of the heap profiles at the first call and at 100 is one
line: `bufio.(*Scanner).Scan` under `proto.(*Decoder).Decode`, 810 KB.
Between 100 and 4,000 nothing in Aries or the SDK differs. A second
run of 3,000 calls sampled the physical footprint: 12.7 MB after the
first call, 14.4 MB at 100, 15.4 MB at 750, 15.7, 15.6 and 15.8 MB at
1,500, 2,250 and 3,000. The climb to 750 with a flat heap is the
runtime touching the rest of the 10.8 MiB it has mapped; it stops
there.

**Everything that can grow, and its bound:**

| What | Where | Bound |
|---|---|---|
| The decoder's line buffer | `proto.Decoder` (`frame.go:115`) | 4 MiB (`MaxFrame`) a connection; reset by a redial |
| Claims held and when their paths were sent | `mcpserver.claims.held`, `.sent` (`mcpserver.go:97`) | The issues the session holds; `keep` drops the rest at each `renew` |
| Pushed inbox items | `mcpserver.pushed` (`inbox.go:18`) | A count and a flag |
| The server's standard error | `client.capBuffer` (`conn.go:887`) | 4,096 bytes |
| Responses waiting for a call | `Conn.res` (`conn.go:83`) | One frame |
| `aries watch`'s queue | `watchQueue` (`cli/inbox.go:115`) | 256 pushes, then a resync in their place |
| The board's pushes | `Live.pushes` (`board/live.go:53`) | 256, then a reread |
| The board's lists | `Model.snap` (`board/model.go:118`) | 100 ready, 100 blocked, 500 held; a reread replaces them |
| The board's event tail | `Model.tail` | 100 events (`TailMax`), each with its before and after |
| The open issue's detail | `Model.detail` | One issue; replaced |
| Transcript offsets | `usage-offsets.json` | One entry a transcript that still exists; 827 bytes for three |

**`aries tui`.** The model does not see the project's size. A refresh
reads at most 100 ready issues, 100 blocked, 500 claims and 500 issues
in progress, whatever the project holds, and `Snapshot.apply` replaces
the previous snapshot and reuses the row slice. Applying the largest
snapshot and drawing a frame of 160 by 50: 0.39 ms, 7,281 allocations,
399 KB (`BenchmarkClientModelRefresh`); an event and a frame the same
(`BenchmarkClientModelEvents`), with the tail checked to stay at 100.
Rereads are once a burst, 250 ms after its first event. `aries tui` and
`aries watch` were not run as processes: the board needs a terminal.

### 4. Big inputs

**The largest frame** is 4 MiB with its newline (`proto.MaxFrame`); a
longer line ends the connection with `ErrFrameTooLarge`.
`BenchmarkClientDecode`, `BenchmarkClientDecodeFirst` and
`BenchmarkClientDecodeResult`, for a list of issues of each size:

| Frame | First on a connection | Later ones | Decoding the result into its type |
|---|---|---|---|
| 300 B | 66 KB, 4 allocations (the 64 KiB buffer) | 704 B, 2 | 656 B, 7 |
| 8 KiB | 75 KB | 9.8 KB, 2 | 14 KB, 95 |
| 64 KiB | 271 KB | 74 KB, 2 | 138 KB, 1,037 |
| 1 MiB | 5.19 MB, 15 | 1.06 MB, 2 | 3.36 MB, 16,874 |
| 3.9 MiB | 12.4 MB, 16 | 4.10 MB, 2 | 16.1 MB, 65,382 |

So the whole frame is buffered, then `json.Unmarshal` copies the result
out of it into `Frame.OK`, then the call unmarshals that into strings of
its own: three copies of a large result at the peak, and the text or
JSON printed from it is a fourth, briefly. Decoding runs at about 830
MB/s for the frame and 400 MB/s for the result.

**A list of 10,000** does not reach the client: a page is 500 issues at
most (`store.MaxPage`), about 60 KB. `aries list -n 500`: 26,096
allocations and 2.56 MB as text, 15,578 and 2.61 MB as `--json`, 17.5
MiB peak RSS either way. One call for a page of 500 on an open
connection: 0.49 ms, 588 KB, 1,047 allocations, both ends.

**Transcripts.** `internal/capture` seeks to the offset it kept, reads
through a 64 KiB buffer a line at a time, passes over a line longer than
4 MiB without holding it, and unmarshals every other line into a struct
that keeps only the usage. It holds one record a response until they are
sent, 500 a request. `BenchmarkClientReadClaude`: a turn of 5 responses
with 4 KiB tool results, 42 microseconds and 71 KB; a session of 300,
2.4 ms and 352 KB at 650 MB/s; 300 with 100 KiB tool results, 27 ms and
32 MB, which is the reallocation named above. The offset stays before
the last response, which may still be growing, so whatever follows that
response is read again by each run until another response arrives: with
a 3 MB line there, each run allocates 17.6 MB and peaks at 25 MiB
instead of 15.

**bd import and export** are `ariesd`'s (`import-bd`, `export-bd`);
`aries` links none of `internal/bdimport`.

### 5. The binary

Sizes are in the summary. darwin/arm64, by section: code 4.78 MB, the
table of functions and lines (`gopclntab`) 3.99 MB, type descriptors
1.27 MB, read-only data 0.43 MB, initialized data 0.40 MB, the link-edit
segment 0.38 MB. `-s -w` has already taken the symbol table and DWARF;
the table of functions cannot go.

linux/amd64, `go tool nm -size` on the same code built with symbols:
6.56 MB of text, read-only data and data can be given to a package, a
little over half the file; the rest is that function table and type
data, in proportion.

| Group | Bytes | Linked for |
|---|---|---|
| Go runtime | 1,313,366 | everything |
| `crypto`, `math/big`, `encoding/asn1` | 1,055,926 | SSH, and TLS for `aries upgrade` |
| `encoding/json`, `reflect`, `fmt`, `strings`, `slices`, `time` | 912,776 | everything |
| MCP SDK, `jsonschema-go`, `segmentio`, `uritemplate`, `encoding/gob` | 740,221 | `aries mcp` |
| Aries | 678,339 | |
| `net/http`, HTTP/2, `idna`, `mime`, `compress`, `archive` | 598,893 | `aries upgrade` (and imported by the MCP SDK) |
| `net`, `os`, `syscall` | 289,598 | everything |
| `crypto/tls` | 254,760 | `aries upgrade` |
| `golang.org/x/crypto` | 212,471 | every connection |
| `go.yaml.in/yaml/v3` | 196,703 | `.aries.yaml` |
| `regexp` | 93,950 | validation |
| `golang.org/x/term`, `x/sys/unix` | 1,576 | `aries tui` |

Within aries: `internal/cli` 224 KB, `internal/mcpserver` 180 KB,
`internal/agentsetup` 77 KB, `internal/board` 57 KB, `internal/client`
48 KB (the IAP transport is part of it and runs `gcloud`: it links
nothing), `internal/release` 24 KB, `internal/capture` 20 KB,
`internal/gitx` 14 KB.

**Linked and never called.** The linker drops what nothing reaches, and
`golang.org/x/oauth2`, which the SDK imports, contributes no symbol at
all. What stays without being used: `encoding/gob`, 123 KB, kept by its
`init`; `uritemplate`, 16 KB, for MCP resources `aries` does not serve;
the SDK's HTTP transports' share of `net/http`, which `aries upgrade`
would link anyway. `crypto/internal/fips140/drbg.memory` is 32 MiB of
zero-filled data in every Go 1.27 binary's address space; it is never
touched and costs no memory. The largest function is
`jsonschema.(*state).validate` at 30 KB. No package of the OpenTelemetry
SDK, the store or the server is among the 266 that `aries` links.

**What the size costs at start.** The 2.7 ms and 10.5 MiB between
`hello` and `aries --version` are 1.0 ms of `init` and, for the rest, the
loading of the binary: its pages faulted in (0.7 ms more system time),
and the runtime's tables sized to it. About 7 MB of that RSS is the
binary's own pages, shared by every `aries` running; the footprint of a
process counts none of them.

## How it was measured

**The server.** `TestProfileServer` (`internal/e2e/profile_test.go`)
serves a world to `aries` binaries started outside the test. It skips
unless `ARIES_PROFILE_DIR` names a directory; then it writes the
repository, port and host key to `server.env` there and serves until a
file named `stop` appears. A file named `drop` closes every SSH
connection, and `stat` writes the server side's goroutines and heap. It
raises the limits a profile meets first; `ARIES_PROFILE_LIMITS=default`
leaves them.

```sh
ARIES_REQUIRE_DOLT=1 ARIES_PROFILE_DIR=DIR \
  go test -run 'TestProfileServer$' -timeout 0 ./internal/e2e/
```

**The client's environment.** Every `aries` ran with `HOME` set to an
empty directory made for it, `SSH_AUTH_SOCK` unset, and the key the test
made for its repository. The real `~/.ssh` and ssh-agent were not read.

**Whole processes.** A small program ran each command 30 or 40 times and
took wall time and, from `wait4`, user and system CPU and the peak RSS.
For allocations the same tree was built with `go build -overlay`
replacing `cmd/aries/main.go` by a copy that also writes
`runtime.MemStats` to a file as it exits and, on `SIGUSR1`, collects
twice and writes `MemStats`, the heap profile and the goroutine profile.
Nothing else differs: its `--version` allocates 13,398 times, of which
13,194 are `init`. `aries mcp` was driven over its standard input by a
program speaking MCP's JSON lines, which took those reports before and
after 100 calls of each tool, and at marks through a long session.

```sh
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o aries ./cmd/aries
GODEBUG=inittrace=1 ./aries --version
/usr/bin/time -l ./aries -C REPO whoami          # macOS: peak RSS in bytes
vmmap --summary PID; footprint PID             # macOS: a process's own memory
cat /proc/PID/smaps_rollup                     # Linux
go tool nm -size -sort size aries-with-symbols
go list -deps ./cmd/aries
```

**Prototypes** were overlays too: copies of the files concerned, edited
outside the repository and named in an overlay file, so the tree was
never changed. Proposals 1, 2 and 6 were built and measured that way,
and the tests of `internal/mcpserver` and `internal/cli` run against
them; proposal 3 by `GOGC` in the environment.

**Benchmarks added**, all in files named `bench_client_test.go`, all
named `BenchmarkClient...`, none needing Dolt:

```sh
go test -run '^$' -bench Client -benchmem ./internal/proto/ ./internal/client/ \
  ./internal/mcpserver/ ./internal/cli/ ./internal/safetext/ ./internal/capture/ \
  ./internal/board/ ./internal/outbox/
```

| Package | Benchmarks |
|---|---|
| `internal/proto` | `Decode`, `DecodeFirst`, `DecodeResult` at five frame sizes; `EncodeRequest` |
| `internal/client` | `LoadConfig`, `LoadConfigNone`, `Identity` (configured Ed25519, ECDSA, RSA 3072; the default identities), `Dial`, `Call` (empty; a page of 500) against an SSH server in the process |
| `internal/mcpserver` | `New`, `ToolsList`, `Size`, `ListFit`, `CommentsFit`, `Prime` |
| `internal/cli` | `Run` for commands that do not connect; `PrintSummaries`, counting writes |
| `internal/safetext` | `Line`, `JSON`, `Writer` |
| `internal/capture` | `ReadClaude` on a turn and on two sessions |
| `internal/board` | `ModelRefresh`, `ModelEvents` |
| `internal/outbox` | `Append`, `List` with 0, 100 and 900 entries waiting |

`BenchmarkClientDial` and `BenchmarkClientCall` have the SSH server in
the same process, so their allocations are both ends'.

## Not measured

- **Windows.** Nothing ran there. The binary builds and vets.
- **linux/amd64 at run time.** The container was arm64; the amd64 binary
  was only built and its symbols read.
- **A real server's side of N sessions:** `sshd` and one `ariesd
  stdio` bridge process a connection. The server here was one test
  process.
- **ssh-agent**, by rule, so not what asking it costs; and the IAP
  transport, which starts `gcloud`.
- **`aries tui` and `aries watch` as processes.** Their state is read from
  the code and the model is benchmarked.
- **`gitx.IssuePaths`**, the `git` processes a renewal starts for held
  issues.
- **A cold start:** the first run of a binary after it is written, or
  after its pages left the cache. A freshly built binary was seen to
  take seconds to start, repeatedly, on this busy machine, in the
  kernel before `main`; that was not pursued.
- **Where a short command's CPU goes inside the runtime.** A command of
  5 ms is too short to sample; the split above is by difference between
  commands.
- **Proposals 4 and 5** are estimates, not prototypes.
- **`aries setup` and `aries upgrade`.**
