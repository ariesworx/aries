package telemetry

import (
	"slices"

	"github.com/ariesworx/aries/internal/proto"
)

// Instrument names one of ariesd's instruments (design §16.4).
// docs/server.md, Observability, lists them, and a test holds its table
// to [Instruments].
type Instrument int

// The instruments. Each takes the attribute values its doc comment
// lists, in that order, as the trailing arguments of [Telemetry.Record].
const (
	// RequestDuration is how long a request took, from its frame being
	// read to its answer being ready: op, result.
	RequestDuration Instrument = iota
	// ProjectRequests counts requests: project, result class.
	ProjectRequests
	// Connections is how many connections are past the handshake:
	// project.
	Connections
	// Handshakes counts handshakes: result.
	Handshakes
	// ConnectionsDropped counts connections the server closed: reason.
	ConnectionsDropped
	// LimitRefusals counts refusals at a limit: limit.
	LimitRefusals
	// StoreWriteDuration is how long a write that changed a row took,
	// from reaching the store to its commit: project.
	StoreWriteDuration
	// StoreWriteRetries counts attempts of a write lost to a concurrent
	// transaction and tried again: project.
	StoreWriteRetries
	// StoresOpen is how many project stores are open; it is watched
	// ([Telemetry.Watch]), not recorded.
	StoresOpen
	// StoreOpenDuration is how long opening a project's store took:
	// migrated.
	StoreOpenDuration
	// Claims counts changes of a claim: event, reason.
	Claims
	// Locks counts changes of a lock's holds and queue: event (design
	// §20.9). A lock's name is a person's text, and no attribute.
	Locks
	// LockWaitDuration is how long a place in a lock's queue waited
	// before its grant, on the store's clock.
	LockWaitDuration
	// Gates counts changes of a gate: kind, event (design §20.9). A
	// gate's title and its check's name are text, and no attribute.
	Gates
	// ControlWrites counts writes to the control store: op, result.
	ControlWrites
	// KeysFileWrites counts writes of the keys file: result.
	KeysFileWrites
	// KeysFileLastWrite is when the keys file last took its content, in
	// Unix seconds.
	KeysFileLastWrite
	// ReaperDuration is how long one tick of the reaper took.
	ReaperDuration
	// ReaperLastRun is when the reaper's last tick ended, in Unix
	// seconds.
	ReaperLastRun
	// ClockBehind is how far the server's clock is behind the history
	// its time is held against, in seconds; zero when it is not behind.
	ClockBehind
	// ClockSteps counts the steps of the server's clock the daemon
	// detected: direction.
	ClockSteps
	// Dropped counts telemetry that could not be exported: signal. For
	// SignalMetrics it counts exports that failed, whose totals the next
	// export carries; for SignalTraces it counts spans, which are lost;
	// for SignalLogs it counts log records, which the journal still has.
	Dropped
	// DBPoolConns is how many connections to Postgres the daemon's pool
	// holds, and DBPoolInUse how many of them a request is using; both
	// are watched ([Telemetry.Watch]).
	DBPoolConns
	DBPoolInUse
	// DBPoolWaits counts the times a request waited for a connection of
	// the pool because every one was in use; it is watched.
	DBPoolWaits
	// HTTPSConnections is how many TCP connections the HTTPS listener
	// holds: in their TLS handshake, plain requests and WebSockets.
	HTTPSConnections
	// HTTPSConnectionsRefused counts connections the HTTPS listener
	// closed unanswered: reason.
	HTTPSConnectionsRefused
	// HTTPSUpgrades counts requests to upgrade to a WebSocket: result,
	// reason.
	HTTPSUpgrades
	// HTTPSKeyProofs counts key proofs on a WebSocket: result.
	HTTPSKeyProofs
	// HTTPSTokens counts sign-ins' tokens presented to connect, with
	// their DPoP proofs: result.
	HTTPSTokens
	// HTTPSTLSFailures counts connections to the HTTPS listener closed
	// before their TLS handshake completed.
	HTTPSTLSFailures
	// HTTPSCertReloads counts reads of the TLS certificate's files after
	// they changed: result.
	HTTPSCertReloads

	numInstruments
)

// Kind is what an instrument measures.
type Kind string

// The kinds of instrument.
const (
	Counter       Kind = "counter"
	UpDownCounter Kind = "up-down counter"
	Histogram     Kind = "histogram"
	Gauge         Kind = "gauge"
)

// Attribute keys. No instrument has both Project and Op (design §16.4).
const (
	KeyOp          = "op"
	KeyResult      = "result"
	KeyResultClass = "result_class"
	KeyProject     = "project"
	KeyReason      = "reason"
	KeyLimit       = "limit"
	KeyMigrated    = "migrated"
	KeyEvent       = "event"
	KeySignal      = "signal"
	KeyDirection   = "direction"
	KeyKind        = "kind"
)

// Attribute values that are not a protocol code or an op.
const (
	// Unknown stands for an op the server does not know.
	Unknown = "unknown"
	// Other stands for any other value that is not on its key's list,
	// and for a project past the cap on projects.
	Other = "other"

	// ResultOK is a request, handshake or write that succeeded.
	// ResultFailed is a handshake that ended without a protocol error to
	// name (a frame that could not be read, a welcome that could not be
	// written), and a write of the keys file that failed.
	ResultOK     = "ok"
	ResultFailed = "failed"

	// The result classes of ProjectRequests.
	ClassOK      = "ok"
	ClassRefused = "refused"
	ClassError   = "error"

	// Why the server closed a connection.
	DropMembership = "membership"
	DropDisabled   = "disabled"
	DropArchived   = "archived"
	DropKey        = "key"
	DropIdle       = "idle"
	DropLimit      = "limit"
	// DropAccess is a connection whose access ended while its handshake
	// ran.
	DropAccess = "access"
	// DropSignIn is a connection whose sign-in ended: at its end, by a
	// logout, or by an admin (transport.md §6.7).
	DropSignIn = "signin"

	// What happened to a claim.
	ClaimTake    = "take"
	ClaimRenew   = "renew"
	ClaimRelease = "release"
	ClaimExpire  = "expire"

	// What happened to a lock's holds and queue: the lock events, and a
	// renewal.
	LockAcquire = "acquire"
	LockWait    = "wait"
	LockGrant   = "grant"
	LockRenew   = "renew"
	LockRelease = "release"
	LockExpire  = "expire"
	LockLeave   = "leave"

	// The kinds of gate, and what happened to one: the gate events.
	GateHuman   = "human"
	GateCI      = "ci"
	GateAdded   = "added"
	GatePassed  = "passed"
	GateFailed  = "failed"
	GateWaived  = "waived"
	GateRearmed = "rearmed"

	// Why a claim was released, when the store's event gives a reason.
	// ReasonNone is every other change of a claim.
	ReasonNone       = "none"
	ReasonMembership = "membership"
	ReasonDisabled   = "disabled"
	ReasonArchived   = "archived"

	// Which way the clock stepped.
	StepForward  = "forward"
	StepBackward = "backward"

	// The signals exported.
	SignalMetrics = "metrics"
	SignalTraces  = "traces"
	SignalLogs    = "logs"

	// Why the HTTPS listener closed a connection unanswered: from an
	// address it does not trust behind a proxy, or past one of its
	// limits, which LimitRefusals names.
	ListenerUntrusted = "untrusted"
	ListenerLimit     = "limit"

	// How an upgrade to a WebSocket ended: ResultOK with ReasonNone, or
	// UpgradeRefused with why: a browser's Origin, an address past its
	// refused key proofs, or a request that is no WebSocket upgrade.
	UpgradeRefused      = "refused"
	UpgradeOrigin       = "origin"
	UpgradeAuthFailures = "auth_failures"
	UpgradeBadRequest   = "bad_request"
	// UpgradeToken is an upgrade refused for its sign-in's token, which
	// HTTPSTokens counts by why.
	UpgradeToken = "token"

	// How a key proof was refused: no key proof at all, one for another
	// server's URL, no key the server knows, a signature that does not
	// verify, and a key proved whose principal is disabled. A proof the
	// client left, or did not finish within
	// upgrade_timeout, is ResultFailed.
	ProofInvalid    = "invalid"
	ProofURL        = "url"
	ProofUnknownKey = "unknown_key"
	ProofBadProof   = "bad_proof"
	ProofDisabled   = "disabled"

	// How a sign-in's token presented to connect was refused: by
	// another scheme than DPoP, missing, unknown to the server, its
	// sign-in ended, bound to no key, or by a server with no login:;
	// and its DPoP proof missing, malformed, not signed by its key, for
	// another request or token, out of time, replayed, past the proofs
	// remembered, or signed by another key than the token's.
	TokenScheme   = "token_scheme"
	TokenMissing  = "token_missing"
	TokenUnknown  = "token_unknown"
	TokenEnded    = "token_ended"
	TokenBearer   = "token_bearer"
	TokenNoLogin  = "token_no_login"
	DPoPMissing   = "dpop_missing"
	DPoPMalformed = "dpop_malformed"
	DPoPSignature = "dpop_signature"
	DPoPRequest   = "dpop_request"
	DPoPToken     = "dpop_token"
	DPoPTime      = "dpop_time"
	DPoPReplayed  = "dpop_replayed"
	DPoPBusy      = "dpop_busy"
	DPoPKey       = "dpop_key"
)

// The limits a refusal is counted under, by the name of the setting in
// ariesd's config (under limits:, but for LimitTelemetryRequests), apart
// from LimitSockets, which is twice conns_per_server.
const (
	LimitConns             = "conns_per_server"
	LimitConnsPerPrincipal = "conns_per_principal"
	// LimitHandshakes is the cap on one principal's connections that
	// have not yet said hello.
	LimitHandshakes = "handshakes_per_principal"
	// LimitSockets is the cap on sockets still in their handshake.
	LimitSockets               = "handshake_sockets"
	LimitWriteRate             = "writes_per_second"
	LimitControlWriteRate      = "control_writes_per_second"
	LimitOpenStores            = "open_stores_per_server"
	LimitTelemetryRequests     = "telemetry.max_requests"
	LimitProjects              = "projects_per_server"
	LimitPrincipals            = "principals_per_server"
	LimitKeys                  = "keys_per_principal"
	LimitMembers               = "members_per_project"
	LimitIdentities            = "identities_per_principal"
	LimitInvites               = "invites_per_server"
	LimitPrices                = "prices_per_project"
	LimitPlans                 = "plans_per_project"
	LimitHoursPerDay           = "hours_per_day"
	LimitUsagePerDay           = "usage_per_day"
	LimitLocks                 = "locks_per_project"
	LimitLockWaiters           = "waiters_per_lock"
	LimitLockWaits             = "lock_waits_per_principal"
	LimitHTTPSConns            = "https_conns_per_server"
	LimitHTTPSConnsPerAddr     = "https_conns_per_addr"
	LimitAuthFailures          = "auth_failures_per_minute"
	LimitGrants                = "grants_per_principal"
	LimitSignInsPending        = "signins_pending_per_server"
	LimitSignInsPendingPerAddr = "signins_pending_per_addr"
	LimitDPoPProofs            = "dpop_proofs_per_server"
)

// Buckets are the upper bounds, in seconds, of every histogram's
// buckets: 5 ms to 10 s.
var Buckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// results are the values of KeyResult: ok, failed, and each protocol
// error code.
var results = []string{ResultOK, ResultFailed,
	string(proto.CodeNotFound), string(proto.CodeConflict), string(proto.CodeExists), string(proto.CodeCycle),
	string(proto.CodeInvalid), string(proto.CodeAcceptance), string(proto.CodeForbidden), string(proto.CodeUnavailable),
	string(proto.CodeVersion), string(proto.CodeAuth), string(proto.CodeBusy), string(proto.CodeStale)}

// limits are the values of KeyLimit.
var limits = []string{LimitConns, LimitConnsPerPrincipal, LimitHandshakes, LimitSockets, LimitWriteRate, LimitControlWriteRate,
	LimitOpenStores, LimitTelemetryRequests, LimitProjects, LimitPrincipals, LimitKeys, LimitMembers, LimitIdentities, LimitInvites,
	LimitPrices, LimitPlans, LimitHoursPerDay, LimitUsagePerDay, LimitLocks, LimitLockWaiters, LimitLockWaits,
	LimitHTTPSConns, LimitHTTPSConnsPerAddr, LimitAuthFailures,
	LimitGrants, LimitSignInsPending, LimitSignInsPendingPerAddr, LimitDPoPProofs}

// attr is one attribute of an instrument: its key and the values it may
// take. A value not among them is recorded as fallback. The values of
// ops come from Config.Ops, and a project's from the bounded set the
// Telemetry keeps (projects); both have a nil list here.
type attr struct {
	key      string
	values   []string
	fallback string
}

var (
	attrOp       = attr{key: KeyOp, fallback: Unknown}
	attrProject  = attr{key: KeyProject, fallback: Other}
	attrResult   = attr{key: KeyResult, values: results, fallback: Other}
	attrClass    = attr{key: KeyResultClass, values: []string{ClassOK, ClassRefused, ClassError}, fallback: Other}
	attrDrop     = attr{key: KeyReason, values: []string{DropMembership, DropDisabled, DropArchived, DropKey, DropIdle, DropLimit, DropAccess, DropSignIn}, fallback: Other}
	attrLimit    = attr{key: KeyLimit, values: limits, fallback: Other}
	attrMigrated = attr{key: KeyMigrated, values: []string{"true", "false"}, fallback: Other}
	attrEvent    = attr{key: KeyEvent, values: []string{ClaimTake, ClaimRenew, ClaimRelease, ClaimExpire}, fallback: Other}
	attrLockEvt  = attr{key: KeyEvent, values: []string{LockAcquire, LockWait, LockGrant, LockRenew, LockRelease, LockExpire, LockLeave}, fallback: Other}
	attrGateKind = attr{key: KeyKind, values: []string{GateHuman, GateCI}, fallback: Other}
	attrGateEvt  = attr{key: KeyEvent, values: []string{GateAdded, GatePassed, GateFailed, GateWaived, GateRearmed}, fallback: Other}
	attrWhy      = attr{key: KeyReason, values: []string{ReasonNone, ReasonMembership, ReasonDisabled, ReasonArchived}, fallback: Other}
	attrWrote    = attr{key: KeyResult, values: []string{ResultOK, ResultFailed}, fallback: Other}
	attrSignal   = attr{key: KeySignal, values: []string{SignalMetrics, SignalTraces, SignalLogs}, fallback: Other}
	attrStep     = attr{key: KeyDirection, values: []string{StepForward, StepBackward}, fallback: Other}
	attrListener = attr{key: KeyReason, values: []string{ListenerUntrusted, ListenerLimit}, fallback: Other}
	attrUpgraded = attr{key: KeyResult, values: []string{ResultOK, UpgradeRefused}, fallback: Other}
	attrUpgrade  = attr{key: KeyReason, values: []string{ReasonNone, UpgradeOrigin, UpgradeAuthFailures, UpgradeBadRequest, UpgradeToken}, fallback: Other}
	attrProof    = attr{key: KeyResult, values: []string{ResultOK, ResultFailed, ProofInvalid, ProofURL, ProofUnknownKey, ProofBadProof, ProofDisabled}, fallback: Other}
	attrToken    = attr{key: KeyResult, values: []string{ResultOK, TokenScheme, TokenMissing, TokenUnknown, TokenEnded, TokenBearer, TokenNoLogin,
		DPoPMissing, DPoPMalformed, DPoPSignature, DPoPRequest, DPoPToken, DPoPTime, DPoPReplayed, DPoPBusy, DPoPKey}, fallback: Other}
)

// spec declares one instrument.
type spec struct {
	name, unit, desc string
	kind             Kind
	attrs            []attr
	// watched marks a gauge read by a callback (Telemetry.Watch) rather
	// than recorded.
	watched bool
}

// specs declares every instrument. The names, kinds and attributes are
// design §16.4's, as built: docs/server.md lists where they differ.
var specs = [numInstruments]spec{
	RequestDuration:    {name: "aries.request.duration", unit: "s", kind: Histogram, attrs: []attr{attrOp, attrResult}, desc: "Time to answer a request."},
	ProjectRequests:    {name: "aries.project.requests", kind: Counter, attrs: []attr{attrProject, attrClass}, desc: "Requests answered, by project."},
	Connections:        {name: "aries.connections", kind: UpDownCounter, attrs: []attr{attrProject}, desc: "Connections past the handshake."},
	Handshakes:         {name: "aries.handshakes", kind: Counter, attrs: []attr{attrResult}, desc: "Handshakes, by how they ended."},
	ConnectionsDropped: {name: "aries.connections.dropped", kind: Counter, attrs: []attr{attrDrop}, desc: "Connections the server closed."},
	LimitRefusals:      {name: "aries.limit.refusals", kind: Counter, attrs: []attr{attrLimit}, desc: "Refusals at a limit."},
	StoreWriteDuration: {name: "aries.store.write.duration", unit: "s", kind: Histogram, attrs: []attr{attrProject}, desc: "Time for a write to commit, with the wait for the writer and any retry."},
	StoreWriteRetries:  {name: "aries.store.write.retries", kind: Counter, attrs: []attr{attrProject}, desc: "Write attempts lost to a concurrent transaction and retried."},
	StoresOpen:         {name: "aries.stores.open", kind: Gauge, watched: true, desc: "Project stores open."},
	StoreOpenDuration:  {name: "aries.store.open.duration", unit: "s", kind: Histogram, attrs: []attr{attrMigrated}, desc: "Time to open a project's store."},
	Claims:             {name: "aries.claims", kind: Counter, attrs: []attr{attrEvent, attrWhy}, desc: "Changes of a claim."},
	Locks:              {name: "aries.locks", kind: Counter, attrs: []attr{attrLockEvt}, desc: "Changes of a lock's holds and queue."},
	LockWaitDuration:   {name: "aries.lock.wait.duration", unit: "s", kind: Histogram, desc: "Time a place in a lock's queue waited for its grant."},
	Gates:              {name: "aries.gates", kind: Counter, attrs: []attr{attrGateKind, attrGateEvt}, desc: "Changes of a gate: added, passed, failed, waived and rearmed."},
	ControlWrites:      {name: "aries.control.writes", kind: Counter, attrs: []attr{attrOp, attrResult}, desc: "Writes to the control store."},
	KeysFileWrites:     {name: "aries.keys_file.writes", kind: Counter, attrs: []attr{attrWrote}, desc: "Writes of the keys file."},
	KeysFileLastWrite:  {name: "aries.keys_file.last_write.time", unit: "s", kind: Gauge, desc: "When the keys file last took its content, in Unix seconds."},
	ReaperDuration:     {name: "aries.reaper.duration", unit: "s", kind: Histogram, desc: "Time for one tick of the reaper."},
	ReaperLastRun:      {name: "aries.reaper.last_run.time", unit: "s", kind: Gauge, desc: "When the reaper's last tick ended, in Unix seconds."},
	ClockBehind:        {name: "aries.clock.behind", unit: "s", kind: Gauge, desc: "How far the server's clock is behind its own history; zero when it is not."},
	ClockSteps:         {name: "aries.clock.steps", kind: Counter, attrs: []attr{attrStep}, desc: "Steps of the server's clock detected."},
	Dropped:            {name: "aries.telemetry.dropped", kind: Counter, attrs: []attr{attrSignal}, desc: "Telemetry that could not be exported: failed exports of metrics, and spans and log records lost."},
	DBPoolConns:        {name: "aries.db.pool.conns", kind: Gauge, watched: true, desc: "Connections to Postgres the daemon's pool holds."},
	DBPoolInUse:        {name: "aries.db.pool.in_use", kind: Gauge, watched: true, desc: "Connections of the pool a request is using."},
	DBPoolWaits:        {name: "aries.db.pool.waits", kind: Counter, watched: true, desc: "Times a request waited for a connection of the pool, every one being in use."},

	HTTPSConnections:        {name: "aries.https.connections", kind: UpDownCounter, desc: "TCP connections the HTTPS listener holds."},
	HTTPSConnectionsRefused: {name: "aries.https.connections.refused", kind: Counter, attrs: []attr{attrListener}, desc: "Connections the HTTPS listener closed unanswered."},
	HTTPSUpgrades:           {name: "aries.https.upgrades", kind: Counter, attrs: []attr{attrUpgraded, attrUpgrade}, desc: "Requests to upgrade to a WebSocket, by how they ended."},
	HTTPSKeyProofs:          {name: "aries.https.key_proofs", kind: Counter, attrs: []attr{attrProof}, desc: "Key proofs on a WebSocket, by how they ended."},
	HTTPSTokens:             {name: "aries.https.tokens", kind: Counter, attrs: []attr{attrToken}, desc: "Sign-ins' tokens presented to connect, by how their check ended."},
	HTTPSTLSFailures:        {name: "aries.https.tls_handshakes.failed", kind: Counter, desc: "Connections to the HTTPS listener closed before their TLS handshake completed."},
	HTTPSCertReloads:        {name: "aries.https.cert.reloads", kind: Counter, attrs: []attr{attrWrote}, desc: "Reads of the TLS certificate's files after they changed."},
}

// Description is one instrument as the documentation and the tests list
// it.
type Description struct {
	// Name is the instrument's OpenTelemetry name, and Prometheus the
	// name its series have on the /metrics endpoint: a histogram's with
	// _bucket, _sum and _count after it.
	Name, Prometheus string
	Kind             Kind
	// Unit is "s" for seconds, or empty for a count.
	Unit string
	// Keys are the instrument's attribute keys, with per_project on.
	Keys []string
}

// Instruments describes every instrument, in the order of the constants.
func Instruments() []Description {
	out := make([]Description, 0, numInstruments)
	for _, sp := range specs {
		d := Description{Name: sp.name, Prometheus: prometheusName(sp), Kind: sp.kind, Unit: sp.unit}
		for _, a := range sp.attrs {
			d.Keys = append(d.Keys, a.key)
		}
		out = append(out, d)
	}
	return out
}

// prometheusName is the name the SDK's Prometheus exporter gives sp:
// dots become underscores, a unit of seconds adds _seconds, and a
// counter adds _total. TestPrometheusNames holds it to what the exporter
// writes.
func prometheusName(sp spec) string {
	b := []byte(sp.name)
	for i, c := range b {
		if c == '.' {
			b[i] = '_'
		}
	}
	name := string(b)
	if sp.unit == "s" {
		name += "_seconds"
	}
	if sp.kind == Counter {
		name += "_total"
	}
	return name
}

// allowed reports whether v is one of a's listed values.
func (a attr) allowed(v string) bool { return slices.Contains(a.values, v) }
