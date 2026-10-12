package telemetry

import (
	"slices"

	"github.com/ariesworx/starfix/internal/proto"
)

// Instrument names one of starfixd's instruments (design §16.4).
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
	// StoreCommitDuration is how long a Dolt commit took.
	StoreCommitDuration
	// StoresOpen is how many project stores are open; it is watched
	// ([Telemetry.Watch]), not recorded.
	StoresOpen
	// StoreOpenDuration is how long opening a project's store took:
	// migrated.
	StoreOpenDuration
	// Claims counts changes of a claim: event, reason.
	Claims
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
	// Dropped counts telemetry that could not be exported: signal.
	Dropped

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

	// What happened to a claim.
	ClaimTake    = "take"
	ClaimRenew   = "renew"
	ClaimRelease = "release"
	ClaimExpire  = "expire"

	// Why a claim was released, when the store's event gives a reason.
	// ReasonNone is every other change of a claim.
	ReasonNone       = "none"
	ReasonMembership = "membership"
	ReasonDisabled   = "disabled"
	ReasonArchived   = "archived"

	// SignalMetrics is the one signal this slice exports.
	SignalMetrics = "metrics"
)

// The limits a refusal is counted under, by the name of the setting
// under limits: in starfixd's config, apart from LimitSockets, which is
// twice conns.
const (
	LimitConns             = "conns"
	LimitConnsPerPrincipal = "conns_per_principal"
	// LimitSockets is the cap on sockets still in their handshake.
	LimitSockets           = "handshake_sockets"
	LimitWriteRate         = "write_rate"
	LimitControlWriteRate  = "control_write_rate"
	LimitOpenStores        = "open_stores"
	LimitTelemetryRequests = "telemetry_requests"
	LimitProjects          = "projects"
	LimitPrincipals        = "principals"
	LimitKeys              = "keys_per_principal"
	LimitMembers           = "members_per_project"
	LimitPrices            = "prices"
	LimitPlans             = "plans"
	LimitHoursPerDay       = "hours_per_day"
	LimitUsagePerDay       = "usage_per_day"
)

// Buckets are the upper bounds, in seconds, of every histogram's
// buckets: 5 ms to 10 s.
var Buckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// results are the values of KeyResult: ok, failed, and each protocol
// error code.
var results = []string{ResultOK, ResultFailed,
	string(proto.CodeNotFound), string(proto.CodeConflict), string(proto.CodeExists), string(proto.CodeCycle),
	string(proto.CodeInvalid), string(proto.CodeAcceptance), string(proto.CodeForbidden), string(proto.CodeUnavailable),
	string(proto.CodeVersion), string(proto.CodeAuth), string(proto.CodeBusy)}

// limits are the values of KeyLimit.
var limits = []string{LimitConns, LimitConnsPerPrincipal, LimitSockets, LimitWriteRate, LimitControlWriteRate,
	LimitOpenStores, LimitTelemetryRequests, LimitProjects, LimitPrincipals, LimitKeys, LimitMembers, LimitPrices,
	LimitPlans, LimitHoursPerDay, LimitUsagePerDay}

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
	attrDrop     = attr{key: KeyReason, values: []string{DropMembership, DropDisabled, DropArchived, DropKey, DropIdle, DropLimit, DropAccess}, fallback: Other}
	attrLimit    = attr{key: KeyLimit, values: limits, fallback: Other}
	attrMigrated = attr{key: KeyMigrated, values: []string{"true", "false"}, fallback: Other}
	attrEvent    = attr{key: KeyEvent, values: []string{ClaimTake, ClaimRenew, ClaimRelease, ClaimExpire}, fallback: Other}
	attrWhy      = attr{key: KeyReason, values: []string{ReasonNone, ReasonMembership, ReasonDisabled, ReasonArchived}, fallback: Other}
	attrWrote    = attr{key: KeyResult, values: []string{ResultOK, ResultFailed}, fallback: Other}
	attrSignal   = attr{key: KeySignal, values: []string{SignalMetrics}, fallback: Other}
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
	RequestDuration:     {name: "starfix.request.duration", unit: "s", kind: Histogram, attrs: []attr{attrOp, attrResult}, desc: "Time to answer a request."},
	ProjectRequests:     {name: "starfix.project.requests", kind: Counter, attrs: []attr{attrProject, attrClass}, desc: "Requests answered, by project."},
	Connections:         {name: "starfix.connections", kind: UpDownCounter, attrs: []attr{attrProject}, desc: "Connections past the handshake."},
	Handshakes:          {name: "starfix.handshakes", kind: Counter, attrs: []attr{attrResult}, desc: "Handshakes, by how they ended."},
	ConnectionsDropped:  {name: "starfix.connections.dropped", kind: Counter, attrs: []attr{attrDrop}, desc: "Connections the server closed."},
	LimitRefusals:       {name: "starfix.limit.refusals", kind: Counter, attrs: []attr{attrLimit}, desc: "Refusals at a limit."},
	StoreWriteDuration:  {name: "starfix.store.write.duration", unit: "s", kind: Histogram, attrs: []attr{attrProject}, desc: "Time for a write to commit, with the wait for the writer and any retry."},
	StoreWriteRetries:   {name: "starfix.store.write.retries", kind: Counter, attrs: []attr{attrProject}, desc: "Write attempts lost to a concurrent transaction and retried."},
	StoreCommitDuration: {name: "starfix.store.commit.duration", unit: "s", kind: Histogram, desc: "Time for a Dolt commit."},
	StoresOpen:          {name: "starfix.stores.open", kind: Gauge, watched: true, desc: "Project stores open."},
	StoreOpenDuration:   {name: "starfix.store.open.duration", unit: "s", kind: Histogram, attrs: []attr{attrMigrated}, desc: "Time to open a project's store."},
	Claims:              {name: "starfix.claims", kind: Counter, attrs: []attr{attrEvent, attrWhy}, desc: "Changes of a claim."},
	ControlWrites:       {name: "starfix.control.writes", kind: Counter, attrs: []attr{attrOp, attrResult}, desc: "Writes to the control store."},
	KeysFileWrites:      {name: "starfix.keys_file.writes", kind: Counter, attrs: []attr{attrWrote}, desc: "Writes of the keys file."},
	KeysFileLastWrite:   {name: "starfix.keys_file.last_write.time", unit: "s", kind: Gauge, desc: "When the keys file last took its content, in Unix seconds."},
	ReaperDuration:      {name: "starfix.reaper.duration", unit: "s", kind: Histogram, desc: "Time for one tick of the reaper."},
	ReaperLastRun:       {name: "starfix.reaper.last_run.time", unit: "s", kind: Gauge, desc: "When the reaper's last tick ended, in Unix seconds."},
	Dropped:             {name: "starfix.telemetry.dropped", kind: Counter, attrs: []attr{attrSignal}, desc: "Exports that failed."},
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
