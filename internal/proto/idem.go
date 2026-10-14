package proto

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
)

// NewIdem returns a fresh idempotency key: prefix, a dash and 32 random
// hex digits. A client makes one per request and sends it again only when
// it retries that request. prefix is a short constant such as "cli" or
// "mcp", so the server's events say where a request came from.
func NewIdem(prefix string) string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	return prefix + "-" + hex.EncodeToString(b[:])
}

// ProtoKeys is the first protocol in which every op an outbox may queue
// takes an idempotency key, and in which a key older than the server's
// window is refused as [CodeStale] (design §18.5).
const ProtoKeys = 6

// keyedSince is, for each op that takes an idempotency key, the first
// protocol this build speaks in which it does. The eight keyed before
// protocol 6 were keyed by protocol 5, this build's oldest.
var keyedSince = map[string]int{
	OpCreate: 5, OpComment: 5, OpHandoff: 5, OpFinish: 5,
	OpRemember: 5, OpForget: 5, OpHoursLog: 5, OpHoursDelete: 5,
	OpUpdate: ProtoKeys, OpLabelAdd: ProtoKeys, OpLabelRm: ProtoKeys, OpDepAdd: ProtoKeys,
	OpAccept: ProtoKeys, OpUsage: ProtoKeys,
}

// Keyed reports whether a server speaking protocol v takes op's
// idempotency key, and so applies a repeat of it once: whether op's
// arguments have idem in v. A write that is sent again later, as an
// outbox does, is sent only to a server for which this holds.
func Keyed(op string, v int) bool {
	since, ok := keyedSince[op]
	return ok && v >= since
}

// ForProtocol returns args as a request of op to a server of protocol v
// sends them: without their idempotency key when op takes none in v, so
// that a server of protocol 5 is not sent the key protocol 6 added, which
// it would refuse as an unknown argument. Otherwise, and for args that
// are not a JSON object, it returns args as they are. A request so sent
// is not deduplicated: the caller must not send it twice (Keyed).
func ForProtocol(v int, op string, args any) any {
	since, ok := keyedSince[op]
	if !ok || v >= since {
		return args
	}
	b, err := json.Marshal(args)
	if err != nil {
		return args
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(b, &fields) != nil {
		return args
	}
	if _, had := fields["idem"]; !had {
		return args
	}
	delete(fields, "idem")
	// A map's keys marshal sorted, not in the order of the struct's
	// fields: the server reads the object by name, so the order is not
	// part of the request.
	out, err := json.Marshal(fields)
	if err != nil {
		return args
	}
	return json.RawMessage(out)
}
