// Package offline is what sfx does with a write that did not reach the
// server: it keeps the write in an outbox on this machine and sends it
// on a later connection (design §18.3; stage 5, slice F1). It joins a
// connection (internal/client) to the queue (internal/outbox), for the
// CLI and for `sfx mcp` alike. Nothing here writes to the process's
// streams.
//
// # When a write is kept
//
// When the server was not reached with it ([client.Unreachable]), the op
// is one the outbox queues ([outbox.Queueable]), and this machine knows
// whose write it is. Otherwise the write fails as it always has, and
// [Local.Keep] says why it was not queued either, with a fix (rule 11).
//
// # Where
//
// Under the user's data directory ([DataDir]), never in a working tree
// (design principle 8): $XDG_DATA_HOME/starfix or ~/.local/share/starfix
// on Linux, ~/Library/Application Support/starfix on macOS, and
// %LocalAppData%\starfix on Windows, which is the local application data
// directory and not the roaming one, since a lock does not cross
// machines. STARFIX_DATA_DIR names another place. internal/capture keeps
// its offsets under the cache directory; an outbox is not a cache, since
// what it holds exists nowhere else, so it is not kept where a system
// may clear files.
//
// In it there is one directory for each server and project, named by a
// hash of a host key the config pinned when it was made and the
// project's id, with:
//
//	seen.json            who the server took each key for; 0600
//	missed               when the server was last not reached; 0600
//	outbox-HASH          one outbox for each principal (internal/outbox),
//	                     named by a hash of the principal's name; 0700
//	outbox-HASH.principal  that principal's name; 0600
//	reached              when the server was last reached; 0600
//	cache-HASH           one cache for each principal ([Cache]), named
//	                     by a hash of the principal's name; 0700
//
// # The cache
//
// What a session read, kept so that it can be shown while the server
// cannot be reached (design §18.6; stage 5, slice F3): a principal's
// prime, whole; show of each issue it looked at, by the issue; the issue
// a start took, kept as a show of it; its inbox and its digests, by
// their arguments. Each is the server's answer as it was returned, with
// when, in a file of its own named by a hash of what it answers, written
// whole to a temporary file and renamed into place, and given that time
// as its own, which is what the bounds read. A file that is not whole, or
// not of its key, is read as absent. In it is also a file that names the
// principal.
//
// A cache is written on the way past, by a connection the server let in,
// for that connection's principal ([Client.CacheOf]), and read only when
// the server cannot be reached, for the principal the keys this process
// would offer belong to, by the outbox's rule ([Client.Cached]). It is
// never shown in place of a server that answers, and never what ready,
// blocked, list, who, claims, gates or dates would say ([RefusedOffline]).
//
// Its bounds are [CacheOptions]: 500 answers, 16 MiB and 1 MiB an answer
// for each principal, and 7 days, the outbox's age cap. Each write
// removes what is past them, the oldest first. They are constants of sfx,
// as the outbox's are: what grows is on the person's own disk.
//
// A refusal of the handshake that says a principal's access has ended
// deletes that principal's cache ([Client.Forget]); `sfx cache clear`
// deletes every principal's for the project ([Local.ClearCaches]).
//
// The server is its host keys, not its address: a server that moves keeps
// its outboxes, and a server rebuilt with a new key gets new ones. The
// config may pin several keys, so that a key is rotated without a flag
// day: the server is then one server whichever of them it presents.
// [Find] takes the directory of the first pinned key that has a record;
// failing that, the directory whose record says a handshake was made
// while the config pinned one of the keys pinned now, which seen.json
// notes ("host_keys") whenever they are not the directory's own key
// alone; and failing that, the first pinned key's. So across a rotation
// as docs/server.md has it (the old key, then the old and the new, then
// the new) the outboxes and seen.json stay where they were, provided a
// command reached the server while both were pinned; if none did, the
// new key gets a directory of its own, as for a rebuilt server. What is
// kept under a key that no handshake linked to the keys pinned now is
// left where it is and never sent by itself, since a changed host key
// is for a person to look at: [Local.Rekeyed] finds
// them, by the project and the old key seen.json names, with the
// address the config gave at the last handshake, and [Local.Adopt]
// moves an entry to the current server's outbox when a person says so,
// and only when the config names the server at that same address.
//
// Before every use, each of those directories that is there must be the
// current user's own and closed to the group and to others (private),
// as a key file's owner is checked. One that is not is refused, with a
// fix, and never chmodded: sfx did not open it. Several users of one
// machine are kept apart by that and by each having a data directory of
// their own. Two accounts that share a home directory share all of it,
// which nothing here prevents.
//
// # Whose
//
// An outbox belongs to one principal, and only the server says who a
// key is. So at every handshake sfx records, in seen.json, the public
// fingerprint of the key the server accepted, the principal it took it
// for and when ([Local.Reached]); and which fingerprint was accepted the
// last time no key was configured. The record has a version, holds
// several keys for one principal, the server's address as the config
// gave it and, when the config pinned several, the fingerprints of the
// host keys it pinned, and holds nothing else: no key, no path to one,
// no token.
//
// With the server away ([Local.Principal]): when a key is configured
// (STARFIX_KEY_FILE, or key:), which is then the only key offered, a
// write goes to the outbox of the principal recorded for it. When none
// is, the server would take the first offered key it knows, which this
// machine cannot work out; the write goes to the outbox of the key
// accepted the last time none was configured, and only while that key
// is still among those sfx would offer. Otherwise nothing is queued.
//
// An outbox is named for its principal and never for a key, so a
// principal's next key sends what the last one queued. It is replayed
// only on a connection whose principal is its own ([Local.Replay]), so
// one principal's writes are never delivered as another's, whichever
// key the environment names by then. When the server has given a key to
// another principal, what the first queued with it stays where it is:
// [Client.Connected] says so once, and `sfx outbox` lists it.
//
// # When access ends
//
// A handshake the server refuses as forbidden, naming the principal it
// refuses ([client.AccessEnded]), says that principal's access to the
// project has ended: disabled, unknown to the server, or no longer a
// member. What waits in that principal's outbox is then refused with
// the server's message and fix ([Local.Ended]), for a person to retry
// once access is back or to drop, rather than left to go stale (design
// §18.3). Nothing else does that: not unavailable or busy, not a
// project that is not served (archived, which activation mends), not a
// key the server no longer holds, and not a refusal that names nobody,
// as an older server's does.
//
// # What the server sees
//
// A replayed entry is a request like any other, with the arguments and
// the idempotency key it had when it was first sent, but for an update,
// which is sent with park (protocol 7): the server compares it with the
// issue at the rev it was based on and parks what someone else changed
// since, rather than refusing it as stale (design §18.7). One that
// parked is delivered, and counted as parked. A server of 6 is sent it
// without park (proto.ForProtocol), and refuses it when stale, as
// before. A finish or handoff is sent with park too (protocol 8): made
// under a claim whose lease ran out before it arrived, it applies if
// nobody took the issue since, and is kept as a comment if somebody did
// (design §18.6); one kept is delivered, and counted as kept. A server
// of 7 is sent them without park, and refuses them then. Its session and machine are those of the connection that
// delivers it, not of the one that made it: no protocol has a field for
// the latter, and the entry keeps them only on this machine.
//
// # Which protocol
//
// Protocol 6 gave update, label.add, label.rm, dep.add, accept and usage
// a key; a server of 5 takes none for them, and so could apply one of
// them twice. At each handshake seen.json notes the protocol the
// connection speaks ([Local.Reached]), and while the server is away a
// write is queued only when that protocol takes its op's key
// ([Local.Keep]; a record that names none is taken for 5, which the sfx
// of slice F1 that wrote it spoke). This build does not speak 5 (its
// ProtoMin is 6), so it is never sent such an entry either way. A replay
// sends an entry only on a connection whose protocol takes its key
// ([Local.Replay]): one queued for a server of 6 that comes back at 5 is
// refused with code version, unsent, for a person to retry once the
// server is upgraded.
//
// # Windows
//
// Nothing here has run on Windows. It builds and vets for it. The check
// that a directory is the user's own is not made there: ownership and
// access are in ACLs, which this does not read. See internal/outbox for
// what else that leaves untested.
package offline
