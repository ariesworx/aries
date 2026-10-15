// Package server is ariesd: the daemon that owns the store and serves the
// protocol on a unix socket, and the stdio bridge that sshd runs as each
// developer key's forced command.
//
// # Trust
//
// The daemon never authenticates anyone itself. sshd authenticates the
// developer's key, and that key's authorized_keys line forces
//
//	restrict,command="ariesd stdio --principal NAME" ssh-ed25519 AAAA…
//
// so the principal is fixed by which key logged in; whatever command the
// client asks for is ignored. The bridge connects to the daemon's socket
// and sends a bridge frame naming that principal before any client byte.
// The daemon believes the bridge frame only because the socket peer is
// local and runs as the daemon's own Unix user: the socket lives in a 0700
// directory, and on Linux each connection's SO_PEERCRED uid must equal the
// daemon's. The SSH login account (server.user) is therefore the account
// ariesd runs as, and its authorized_keys holds only restricted lines.
// A bridge frame anywhere but first is refused.
//
// # Serving
//
// [New] checks a [Config] and returns a [Server]; [Listen] opens the
// socket, and [Server.Serve] accepts on it until its context ends. Each
// connection gets a goroutine that checks the peer, reads the bridge frame
// and the hello, answers with a welcome, then answers requests in order.
// [Server.Dispatch] decodes each op's arguments strictly and calls the
// store; a refusal is a [proto.Error] whose fix names the next action. A
// connection that sends watch gets a second goroutine, which pushes its
// new inbox items and never outlives the connection. A reaper goroutine
// ends expired claims and prunes old rows ([Server.Reap], [Server.Prune]).
// Serve waits for all of them before it returns.
//
// # Many projects
//
// A server is given one store and the project it holds ([Config.Store]),
// or a control store ([Config.Control]) and holds many, each in a
// database and a store of its own. Then the hello's project is looked up
// in the control store, and the principal must be a server admin or an
// active member of it. A project's store is opened by its first
// connection and shared by the rest; a registry closes it when it has
// had none for a while, and caps how many are open. The reaper works
// through every open store, and a store is reaped as it opens. Limits
// per principal are then per principal in each project.
//
// Keys then come from the control store too. The daemon writes them to a
// file in authorized_keys form ([Server.WriteKeys]), which sshd reads
// beside the authorized_keys file kept by hand; sshd asks the daemon
// nothing. Each line names its key's fingerprint, the bridge passes it
// on, and the daemon refuses a key the store no longer holds, whatever
// the file says, and closes the connections of a key that is removed.
// When access ends (a membership,
// a principal disabled, a project archived) the daemon closes the
// connections it had let in and ends the claims it held, in the stores
// that are open, and in the others as they open ([Server.Sweep]).
//
// [Limits] bound what one principal can make the daemon do: its
// connections, their idle time, its write rate and its refusal log lines,
// besides the store's limits. They are counted in memory, per daemon.
//
// [ResolveSettings] builds the ariesd commands' [Settings] from flags,
// the environment and a config file. [Bridge] is `ariesd stdio`, and
// [Probe] the health check `ariesd upgrade` runs after a restart.
// [Status] is `ariesd status`: the status report, asked for over the
// socket by the daemon's own user under a reserved principal, as Probe
// connects.
//
// The daemon measures itself through Config.Telemetry (observe.go): its
// requests, handshakes, connections, limits, stores, claims and reaper,
// with attributes whose values a client cannot choose. [Server.Ready] is
// what /readyz answers from. With no Telemetry nothing is recorded.
//
// The design is docs/design/aries.md: §2 for the transport and the
// trust model, the "As built" notes in §7 for claims, the inbox,
// authorization and the limits, §15 for many projects, and §16 for
// observability.
package server
