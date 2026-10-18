// Package proto is the wire protocol between aries and ariesd.
//
// # Transport
//
// The client opens an SSH session to the server's own sshd. Each developer
// key's authorized_keys line forces `ariesd stdio --principal NAME`, so
// the principal comes from the key that authenticated, never from the
// client. That bridge connects to the daemon's unix socket, writes a bridge
// frame naming the principal, then copies bytes both ways. The daemon
// trusts the bridge frame only because the socket peer is local and runs
// as the daemon's own user (checked with SO_PEERCRED on Linux, and by the
// socket directory's 0700 mode everywhere).
//
// # Frames
//
// The stream is newline-delimited JSON, one Frame per line, at most
// MaxFrame bytes. Empty fields are omitted. The short keys are:
//
//	t       frame type: bridge, hello, welcome, req, res, evt
//	id      request id, chosen by the client; a res echoes it
//	op      operation name (req); see the Op constants
//	a       operation arguments (req), one of the *Args types
//	ok      result (res), one of the *Result types
//	err     typed error (res, or welcome when refused): {c, m, fix}
//	v       sender's build version (hello, welcome)
//	p       client protocol version (hello)
//	min,max protocol range the server accepts (welcome)
//	proj    project UUID (hello)
//	s       session id: the client's (hello), the effective one (welcome)
//	m       machine name (hello)
//	h       agent harness, such as claude-code (hello; may be empty)
//	pr      principal: the bridge's claim, and the welcome's echo of it
//	k       the SHA-256 fingerprint of the key that authenticated (bridge)
//	e       payload of an evt frame, such as a pushed inbox item
//	tp      W3C traceparent of the caller (req; optional), such as
//	        00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01
//	ts      W3C tracestate of the caller (req; optional, with tp)
//
// # Sequence
//
//	bridge → daemon:  {"t":"bridge","pr":"alice"}
//	client → daemon:  {"t":"hello","v":"v0.5.0","p":9,"proj":"…","s":"…","m":"laptop"}
//	daemon → client:  {"t":"welcome","v":"v0.5.0","min":8,"max":9,"s":"…","pr":"alice"}
//	client → daemon:  {"t":"req","id":1,"op":"show","a":{"id":"ar-a1b2c3d4"}}
//	daemon → client:  {"t":"res","id":1,"ok":{…}}
//
// A request may carry the caller's trace context in tp and ts, so that
// what the server does for it can later be tied to the caller's trace.
// The server accepts only a version-00 traceparent, in lowercase hex
// with neither id all zeros, and a tracestate of one line of printable
// ASCII of at most 512 bytes (ParseTraceContext). Anything else is
// ignored, never refused. The server carries the context with the
// request and names the trace in the request's log line. A server that
// traces starts its span for the request under the caller's (design
// §16.4), and takes the ids and the flags of the context and nothing
// else: the tracestate is never put on a span or a log line.
//
// v0.5.0 speaks protocol 8 alone; this build speaks 9 and serves 8 too
// (Proto, ProtoMin, ProtoMax). A client whose protocol
// version is outside [min,max] gets a welcome carrying err with code
// "version", whose fix says which side to upgrade (CheckProto), and the
// connection closes; a client refused so says hello in no other version. The health check of `ariesd
// upgrade`, which only the daemon's own user can send, is welcomed
// whatever version it speaks.
//
// # Keys
//
// On a server of many projects the daemon writes the keys the control
// store holds to a file sshd reads, one authorized_keys line each. The
// forced command of such a line names the key, so its bridge frame does,
// {"t":"bridge","pr":"alice","k":"SHA256:…"}: the daemon refuses the
// handshake when the store no longer holds that key for that principal,
// and closes the connection when the key is removed. The bridge frame is
// not versioned; see FrameBridge.
//
// Frame type "evt" is a server-pushed event: no id, an op naming the
// event (EvInbox, EvEvent, EvResync) and its payload in e. The server
// sends them only to a connection that asked with the watch op, and
// sends EvEvent, every event committed on an issue, only when watch asked
// for events, so a client that never watches sees none:
//
//	client → daemon:  {"t":"req","id":2,"op":"watch"}
//	daemon → client:  {"t":"res","id":2,"ok":{"unread":0}}
//	daemon → client:  {"t":"evt","op":"inbox","e":{"id":9,"kind":"claim.lost",…}}
//	client → daemon:  {"t":"req","id":3,"op":"watch","a":{"events":true}}
//	daemon → client:  {"t":"res","id":3,"ok":{"unread":0}}
//	daemon → client:  {"t":"evt","op":"event","e":{"seq":41,"op":"claim.take","issue":"ar-a1b2c3d4",…}}
//
// A client ignores frame types, and event ops, it does not know, so the
// server can add them without a protocol bump; the server refuses unknown
// types from clients.
//
// # Names
//
// v0.5.0 settled the names of ops, fields and inbox kinds (decision 88);
// a later one follows the same rules. Writes are noun.verb (dep.add,
// conflict.resolve) and lists a bare plural (gates, keys). An issue is
// id in the arguments and results of an op about that one issue, and
// issue where the record is something else: a claim, an overlap, an
// hours entry, a memory, a conflict, an inbox item or an event; a gate
// names its issues by their part, blocks and subject. A record that is a
// principal names it name, and one that is a key fingerprint; another
// record names a principal principal. Who did something is by for an
// act, author for text, and X_by for a record's audit columns. An inbox
// kind, like an event's op, is noun.verb (issue.mentioned,
// conflict.parked).
package proto
