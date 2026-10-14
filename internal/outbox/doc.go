// Package outbox keeps the writes a session makes while the server
// cannot be reached, and delivers them when it can be again (design
// §18.3 and §18.4; stage 5, slice F1).
//
// It is a library. A caller appends an operation when its request did
// not reach the server ([Outbox.Append]), replays when a connection is
// back ([Outbox.Replay]), and shows a person what waits and what was
// refused ([Outbox.List], [Outbox.Retry], [Outbox.Drop]). The package
// knows nothing of connections: Replay is given the func that sends.
// Nothing here writes to the process's streams, and the only clock is
// the one in [Options].
//
// # What is queued
//
// Twelve ops, design §18.3's table ([Queueable]): create, comment,
// handoff, remember, hours.log and finish, which carry an idempotency
// key in protocol 5, and update, label.add, label.rm, dep.add, accept and
// usage, which carry one from protocol 6 (stage 5, slice F2). Their
// replay cannot do harm nobody asked for. Every other op is refused with
// [ErrNotQueueable] and why: start takes a claim, and no claim is made
// without the server (decision 39); close, reopen, forget, dep.rm and
// hours.delete undo or end something on a state the client cannot see.
// Which protocol the server speaks is the caller's to know: it queues,
// and sends, an entry only for a server that takes its op's key
// (proto.Keyed; internal/offline).
//
// An entry is refused before it is written when its arguments carry no
// key ([ErrNoKey]), when any string in them looks like a credential to
// internal/secretscan ([ErrSecret]: the outbox holds no secret, and the
// server lints only memories, so this refuses text the server would have
// taken), and when the outbox is at a cap ([ErrFull], [ErrTooLarge]). A
// refused operation is not queued anywhere: nothing is kept in memory
// alone.
//
// # Files
//
// An outbox is one directory, mode 0700, which the caller chooses: one
// for each server, project and principal, since an entry written under
// one principal must not be replayed by another. Its files are 0600:
//
//	outbox.ndjson    the entries, one line of JSON each, in replay order
//	answered.ndjson  what became of an entry: delivered, refused, dropped,
//	                 or to be sent again; the last line for an entry wins
//	stats.json       counts ([Stats]); safe to lose
//	append.lock      locked for one append, or one read of the files
//	replay.lock      locked by the one process that replays
//	*.tmp            a file being written, renamed over its name when whole
//
// The directory belongs on a local disk that is not synced or roamed: a
// lock does not cross machines, and a sync client replaces files on its
// own schedule. On Windows that is the local application data
// directory, not the roaming one.
//
// # Rules, and why
//
// Each of these is here because slice F0 measured what happens without
// it, with 100 and 500 processes on one directory on macOS and Linux
// (spike/outbox/RESULTS.md, on the branch of that slice).
//
//   - The lock is flock on Linux and macOS and LockFileEx on Windows, on
//     a lock file of its own that is never the data file and is never
//     renamed or removed. The kernel frees it when its holder dies. Not
//     fcntl record locks, which do not keep out a second handle of the
//     same process and are dropped when the process closes any
//     descriptor of the file; not a lock file made with O_EXCL and a
//     lease, which stalled everyone for the lease when its holder died
//     and let up to 9 processes hold it at once (section 1). A lock is a
//     handle opened for that acquisition, so two goroutines of one
//     process exclude each other too.
//   - There are two locks. The append lock is held for one append. The
//     replay lock is tried, never waited for, by the process that
//     replays, and has no lease (section 4). Where both are held the
//     replay lock is taken first.
//   - An appender opens the outbox after it has the append lock, every
//     time. Appenders that kept it open across a compaction lost 77 to
//     94 percent of their entries, each append having returned without
//     error (section 5).
//   - An entry is one line, written with one write and synced before
//     Append returns. Before it writes, an appender ends a last line
//     that has no newline, which a writer killed partway leaves; a
//     reader skips a line that does not parse and takes no bytes after
//     the last newline for a line (section 3).
//   - Replay is at-least-once: a replayer that dies between sending an
//     entry and noting the answer leaves it to be sent again. So nothing
//     is queued without an idempotency key (section 4).
//   - The answered file is not synced for each answer. An answer lost
//     with the power is an entry sent again with its key (section 4).
//   - Compaction runs under the replay lock and then the append lock,
//     and replaces the outbox before the answered file (section 5). A
//     sequence number is never used twice, which the first line of a
//     compacted outbox ensures, so an answer left behind by a death
//     between the two means nothing.
//
// One rule is this package's and not F0's: every open of the outbox,
// the answered file and the stats file, by a reader as by a writer, is
// under the append lock, except the replayer's appends to the answered
// file, which are under the replay lock. So while a compaction holds
// both locks, nothing else has any of the three open.
//
// # Bounds
//
// The caps are entries (pending and refused together), the bytes of the
// outbox file, the bytes of one entry, and age, each with a default
// constant and a field of [Options]. The age cap is 7 days, the
// server's idempotency window by default (idem_window, design §18.5).
// An entry past it is not sent: Replay moves it to the refused list as
// [CodeStale], for a person to retry or drop.
//
// The cap is the default window and not the server's own, which a
// client cannot ask of a server it cannot reach. That is safe whatever
// the server's window: the server keeps every key it applied, and never
// applies one again, so an entry sent later than the window either
// applies once (its key is new to the server) or is refused stale (its
// key was applied, and the answer lost). The cap only decides when a
// person, rather than sfx, says that a write that old still stands. The byte cap is on the file as it is, so entries that
// were answered and not yet compacted away count until the next replay
// ends. The answered file holds one line for an entry that waits, at
// most, after each compaction, the text of a refusal is cut to 1,024
// bytes a field, and the stats file has a fixed size.
//
// # Counts
//
// [Stats] measures how often and for how long sessions lose the server,
// which nothing else does: for each op, the entries queued, delivered,
// refused, stale and dropped, a summary of how long the delivered ones
// waited, and the outages. They hold op names, counts and durations, and
// no text of any entry. They are written under the append lock, which an
// append holds already and a replay takes once as it ends, so a replayer
// that is killed loses the counts of that replay. An outage closes at
// the time of the first answer of a replay and is written when that
// replay ends; an entry queued in between does not open another.
//
// # Windows
//
// Nothing in this package has run on Windows. It builds and vets for
// it, and F0 measured nothing there. In particular:
//
//   - lock_windows.go, which calls LockFileEx through kernel32, has
//     never run: that it excludes, that it blocks when asked to wait,
//     and how soon Windows frees a dead holder's lock, which its
//     documentation does not bound.
//   - Go opens files on Windows without FILE_SHARE_DELETE, so a rename
//     over a file that anything has open is expected to be refused. The
//     rule above is there so that nothing of this package has a file
//     open when one is renamed over. Another program that does (a virus
//     scanner, an indexer) would make a compaction or a stats write
//     fail. That is safe by construction: a rename that fails leaves the
//     old file whole, a failed compaction is returned as an error by a
//     Replay whose deliveries stand, and the next one tries again; a
//     failed stats write is dropped. If compaction kept failing, the
//     outbox file would grow to its byte cap and Append would refuse.
//   - What a sync costs there, and that a write with O_APPEND lands
//     whole at the end of the file as it does on Linux and macOS.
//
// The tests that run real processes use nothing particular to a system,
// so `go test ./internal/outbox/` on a Windows machine is the first
// thing to run there.
//
// Also not tested anywhere: power loss or a crash of the operating
// system (every kill in the tests is of a process), a short write from
// a full disk, and a directory on a network or synced file system.
package outbox
