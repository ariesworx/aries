package logs

import "slices"

// Event is the stable name of what a log line reports, the value of its
// event field. An alert matches the name, not the line's message, which
// may be reworded (design §16.4). A name is never changed once it has
// shipped; a new event is added to the list below, to [Events] and to
// the table in docs/server.md, which tests hold together.
type Event string

// The events of starfixd's log.
const (
	// The daemon.
	DaemonServing Event = "daemon.serving"
	AdminCheck    Event = "admin.check_failed"
	AdminWarning  Event = "admin.unvouched"
	SystemdNotify Event = "systemd.notify_failed"
	LogSuppressed Event = "log.suppressed"

	// Connections and the handshake.
	ConnAccept      Event = "conn.accept_failed"
	ConnCapRefused  Event = "conn.cap_refused"
	ConnPeerRefused Event = "conn.peer_refused"
	ConnOpened      Event = "conn.opened"
	ConnIdleClosed  Event = "conn.idle_closed"
	ConnReadFailed  Event = "conn.read_failed"
	ConnWriteFailed Event = "conn.write_failed"
	ConnAccessEnded Event = "conn.access_ended"
	ConnDropped     Event = "conn.dropped"
	ConnKeyLookup   Event = "conn.key_lookup_ignored"
	HandshakeFailed Event = "handshake.failed"
	AgentHarness    Event = "agent.harness_ignored"
	AgentRegister   Event = "agent.register_failed"

	// Requests.
	RequestDone    Event = "request.done"
	RequestRefused Event = "request.refused"
	RequestFailed  Event = "request.failed"
	RequestPart    Event = "request.part_failed"

	// Projects and their stores.
	ProjectAdded       Event = "project.added"
	ProjectOpened      Event = "project.opened"
	ProjectOpenFailed  Event = "project.open_failed"
	ProjectNotOpened   Event = "project.not_opened"
	ProjectClosed      Event = "project.closed"
	ProjectCloseFailed Event = "project.close_failed"
	StoreSchemaTooNew  Event = "store.schema_too_new"
	StoreCommitFailed  Event = "store.commit_failed"
	StoreUnsafeAccount Event = "store.unsafe_account"
	ControlReload      Event = "control.reload_failed"
	ControlIndexStale  Event = "control.index_stale"

	// Claims, the reaper and the access sweep.
	ClaimExpired      Event = "claim.expired"
	ClaimReleased     Event = "claim.released"
	ReaperFailed      Event = "reaper.failed"
	ReaperPruneFailed Event = "reaper.prune_failed"
	ReaperPruned      Event = "reaper.pruned"
	SweepFailed       Event = "access.sweep_failed"

	// The keys file.
	KeysFileWritten     Event = "keys_file.written"
	KeysFileWriteFailed Event = "keys_file.write_failed"
	KeysFileHeldBack    Event = "keys_file.key_held_back"
	KeysFileLeftOut     Event = "keys_file.key_left_out"

	// The clock.
	ClockBehind       Event = "clock.behind"
	ClockCaughtUp     Event = "clock.caught_up"
	ClockStepped      Event = "clock.stepped"
	ClockStepSettled  Event = "clock.step_settled"
	ClockSettleFailed Event = "clock.settle_failed"

	// Dolt.
	DoltCheckFailed      Event = "dolt.check_failed"
	DoltDriver           Event = "dolt.driver"
	DoltMetricsReading   Event = "dolt_metrics.reading"
	DoltMetricsFailed    Event = "dolt_metrics.read_failed"
	DoltMetricsRecovered Event = "dolt_metrics.read_recovered"

	// Telemetry.
	TelemetryOn              Event = "telemetry.on"
	TelemetryOff             Event = "telemetry.off"
	TelemetryStopFailed      Event = "telemetry.stop_failed"
	TelemetryExportFailed    Event = "telemetry.export_failed"
	TelemetryExportRecovered Event = "telemetry.export_recovered"
	TelemetrySDKError        Event = "telemetry.sdk_error"
	TelemetryScrapeFailed    Event = "telemetry.scrape_failed"
	TelemetryListening       Event = "telemetry.listening"
	TelemetryListenerError   Event = "telemetry.listener_error"
	TelemetryListenerStopped Event = "telemetry.listener_stopped"
	TelemetryKeyMade         Event = "telemetry.key_made"
	TelemetryKeyFound        Event = "telemetry.key_found"
	TelemetryKeyUnavailable  Event = "telemetry.key_unavailable"

	// The audit stream (decision 30): one line for each event of the
	// control store's log that changes who has access to what, named
	// for the event's op. AuditReadFailed is the stream's own failure:
	// the lines are late, and the event log has lost nothing.
	AuditProjectAdd       Event = "audit.project.add"
	AuditProjectArchive   Event = "audit.project.archive"
	AuditProjectActivate  Event = "audit.project.activate"
	AuditPrincipalAdd     Event = "audit.principal.add"
	AuditPrincipalDisable Event = "audit.principal.disable"
	AuditPrincipalEnable  Event = "audit.principal.enable"
	AuditKeyAdd           Event = "audit.key.add"
	AuditKeyRemove        Event = "audit.key.remove"
	AuditMemberSet        Event = "audit.member.set"
	AuditMemberRemove     Event = "audit.member.remove"
	AuditReadFailed       Event = "audit.read_failed"
)

// events is every Event, in the order the guide lists them. A test
// fails when a constant above is missing from it.
var events = []Event{
	DaemonServing, AdminCheck, AdminWarning, SystemdNotify, LogSuppressed,
	ConnAccept, ConnCapRefused, ConnPeerRefused, ConnOpened, ConnIdleClosed, ConnReadFailed, ConnWriteFailed, ConnAccessEnded,
	ConnDropped, ConnKeyLookup, HandshakeFailed, AgentHarness, AgentRegister,
	RequestDone, RequestRefused, RequestFailed, RequestPart,
	ProjectAdded, ProjectOpened, ProjectOpenFailed, ProjectNotOpened, ProjectClosed, ProjectCloseFailed,
	StoreSchemaTooNew, StoreCommitFailed, StoreUnsafeAccount, ControlReload, ControlIndexStale,
	ClaimExpired, ClaimReleased, ReaperFailed, ReaperPruneFailed, ReaperPruned, SweepFailed,
	KeysFileWritten, KeysFileWriteFailed, KeysFileHeldBack, KeysFileLeftOut,
	ClockBehind, ClockCaughtUp, ClockStepped, ClockStepSettled, ClockSettleFailed,
	DoltCheckFailed, DoltDriver, DoltMetricsReading, DoltMetricsFailed, DoltMetricsRecovered,
	TelemetryOn, TelemetryOff, TelemetryStopFailed, TelemetryExportFailed, TelemetryExportRecovered, TelemetrySDKError,
	TelemetryScrapeFailed, TelemetryListening, TelemetryListenerError, TelemetryListenerStopped,
	TelemetryKeyMade, TelemetryKeyFound, TelemetryKeyUnavailable,
	AuditProjectAdd, AuditProjectArchive, AuditProjectActivate, AuditPrincipalAdd, AuditPrincipalDisable, AuditPrincipalEnable,
	AuditKeyAdd, AuditKeyRemove, AuditMemberSet, AuditMemberRemove, AuditReadFailed,
}

// Events returns every event's name, in the order the guide lists them.
func Events() []Event { return slices.Clone(events) }

// IsEvent reports whether name is an event's.
func IsEvent(name string) bool { return slices.Contains(events, Event(name)) }

// auditPrefix opens the name of every event of the audit stream.
const auditPrefix = "audit."

// Audit returns the audit stream's event for op, an op of the control
// store's event log, and false for an op that changes nobody's access.
func Audit(op string) (Event, bool) {
	ev := Event(auditPrefix + op)
	if ev == AuditReadFailed || !slices.Contains(events, ev) {
		return "", false
	}
	return ev, true
}
