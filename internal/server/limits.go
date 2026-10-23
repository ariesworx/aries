package server

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
	"github.com/ariesworx/aries/internal/telemetry"
)

// Limits bound what one principal can make the daemon do: how many
// connections it holds, how long they idle, how fast it writes, and how
// many log lines its refusals cost (S-11, S-20, the write rate limit);
// the store's limits on what a request may hold come with them (S-4,
// S-6, S-7). They are set under limits: in ariesd's config file; a
// field left out, or zero, takes DefaultLimits.
type Limits struct {
	store.Limits `yaml:",inline"`

	// Conns caps the daemon's connections past the handshake; twice it
	// caps sockets still in the handshake.
	Conns int `yaml:"conns_per_server"`
	// ConnsPerPrincipal caps one principal's connections. Its default,
	// 64, is one person's sessions on several machines and a few
	// unattended runs on their key: a fleet of agents should have
	// principals of its own, and meets Conns.
	ConnsPerPrincipal int `yaml:"conns_per_principal"`
	// Handshakes caps one principal's connections that have sent their
	// bridge frame and not yet a hello, on the whole server: the project
	// is not known until the hello. Without it one key holder could fill
	// the sockets twice Conns allows and keep everyone else out.
	Handshakes int `yaml:"handshakes_per_principal"`
	// IdleTimeout closes a connection that sends nothing for this long,
	// unless it watches its inbox.
	IdleTimeout Duration `yaml:"idle_timeout"`
	// DBConns caps the daemon's pool of connections to Postgres, which
	// every project's store and the control store share (store.DB); the
	// daemon holds one more, which holds the writers' locks. Postgres's
	// max_connections must leave room for them, beside everything else
	// that connects. DBConnsPerProject caps the connections one project's
	// reads hold at once, so that a busy project leaves the pool to the
	// others; its writes hold one more, one at a time. It may not be
	// more than DBConns.
	DBConns           int `yaml:"db_conns_per_server"`
	DBConnsPerProject int `yaml:"db_conns_per_project"`
	// WriteRate is how many writes a second a principal's token bucket
	// refills; WriteBurst how many it holds.
	WriteRate  float64 `yaml:"writes_per_second"`
	WriteBurst int     `yaml:"write_burst"`
	// RefusalLogs caps the refusal lines logged per principal a minute;
	// the rest are counted in one line when the minute ends.
	RefusalLogs int `yaml:"refusal_logs_per_minute"`
	// AgentKeep is how long a registry row not seen is kept (a
	// principal's latest is always kept); InboxKeep how long a read inbox
	// item is.
	AgentKeep Duration `yaml:"agent_keep"`
	InboxKeep Duration `yaml:"inbox_keep"`
	// IdemWindow is how long an idempotency key gives back its request's
	// result (design §18.5, decision 41): a key used longer ago is
	// refused as stale. It is how long an outbox entry may wait and still
	// be delivered without a person. From an hour to a year.
	IdemWindow Duration `yaml:"idem_window"`
	// AwayMax is the longest a person's `aries away` may extend their
	// claims (design §18.6): every claim of theirs, in every session,
	// holds that long without renewals, and nobody else may take it.
	// From an hour to 30 days.
	AwayMax Duration `yaml:"away_max"`

	// The HTTPS listener's (transport.md §4.6), which apply before a
	// connection's handshake; the limits above apply after it, whatever
	// the transport.
	//
	// HTTPSConns caps the listener's open TCP connections, and
	// HTTPSConnsPerAddr those from one client address. A connection past
	// either is closed unanswered. Behind a proxy that terminates TLS
	// (https: tls: mode: proxy) every connection is the proxy's, so the
	// cap per address is the proxy's to keep, and does not apply.
	HTTPSConns        int `yaml:"https_conns_per_server"`
	HTTPSConnsPerAddr int `yaml:"https_conns_per_addr"`
	// HTTPSAddrPrefixV6 is how many leading bits of an IPv6 address
	// name its client for every limit per address (the connections, the
	// refused key proofs and the sign-ins in progress): 64 by default,
	// since a client is given a /64 at the least and has 2^64
	// addresses in it. An IPv4 address is counted alone.
	HTTPSAddrPrefixV6 int `yaml:"https_addr_prefix_v6"`
	// HTTPSHeaderBytes caps a request's headers, and HTTPSHeaderTimeout
	// how long they may take to arrive: the slow client's bound.
	HTTPSHeaderBytes   int      `yaml:"https_header_bytes"`
	HTTPSHeaderTimeout Duration `yaml:"https_header_timeout"`
	// HTTPSBodyBytes caps a request's body, and HTTPSBodyTimeout how
	// long all of it may take to arrive once the headers are in: a body
	// that is named and never comes, or comes a byte at a time, holds
	// no connection past it. A WebSocket's upgrade clears it.
	HTTPSBodyBytes   int      `yaml:"https_body_bytes"`
	HTTPSBodyTimeout Duration `yaml:"https_body_timeout"`
	// UpgradeTimeout is how long a WebSocket has from its upgrade to the
	// end of its key proof (keyproof); the hello then has helloTimeout,
	// as over SSH.
	UpgradeTimeout Duration `yaml:"upgrade_timeout"`
	// WSMessageBytes caps one message on a WebSocket, which carries one
	// frame: at most proto.MaxFrame.
	WSMessageBytes int `yaml:"ws_message_bytes"`
	// WSWriteTimeout is how long one frame the server writes to a
	// proved WebSocket may take: a client that stops reading is closed
	// when a write waits this long, and its slot freed.
	WSWriteTimeout Duration `yaml:"ws_write_timeout"`
	// AuthFailureRate is how many refused key proofs, and sign-ins
	// refused past SignInsPendingPerAddr, a minute one address's token
	// bucket refills, and AuthFailureBurst how many it holds. With none
	// left, an upgrade or a sign-in from the address is refused busy
	// until one is. Behind a proxy it does not apply, as
	// HTTPSConnsPerAddr does not.
	AuthFailureRate  float64 `yaml:"auth_failures_per_minute"`
	AuthFailureBurst int     `yaml:"auth_failure_burst"`
	// SignInsPending caps the sign-ins in the daemon's memory that were
	// started and not finished (transport.md §6.2): those sent to the
	// provider, 10 minutes each at most, and the codes not yet spent, a
	// minute each. Past it the oldest is let go of, so that a flood of
	// sign-ins never started in earnest cannot refuse a person's.
	SignInsPending int `yaml:"signins_pending_per_server"`
	// SignInsPendingPerAddr caps those started from one client address.
	// Past it a sign-in from the address is refused, and charged to its
	// AuthFailureRate bucket. Behind a proxy it does not apply, as
	// HTTPSConnsPerAddr does not.
	SignInsPendingPerAddr int `yaml:"signins_pending_per_addr"`
	// DPoPProofs caps the DPoP proofs the daemon remembers, each for its
	// two-minute window, so that none is used twice. Past it a proof is
	// refused busy until the oldest age out.
	DPoPProofs int `yaml:"dpop_proofs_per_server"`
	// WorkloadTokens caps the workload tokens the daemon remembers as
	// exchanged, each for the time it could still be taken (workload:
	// fresh, and the skew), so that none signs in twice. Past it an
	// exchange is refused busy until the oldest age out.
	WorkloadTokens int `yaml:"workload_tokens_per_server"`

	// The rest apply to a server of many projects (control_schema) only.

	// Control bounds the rows of the control store: projects,
	// principals, principals one creator who is no server admin makes,
	// keys a principal and members a project.
	Control store.ControlLimits `yaml:",inline"`
	// OpenStores caps the project stores open at once. At the cap, a
	// handshake for another project closes the longest-idle store with
	// no connection, and is refused when every store has one.
	OpenStores int `yaml:"open_stores_per_server"`
	// StoreIdle closes a project's store once it has had no connection
	// for this long. That gives back its connections to the database, and
	// the memory of the server processes that held them.
	StoreIdle Duration `yaml:"store_idle"`
	// ControlWriteRate is how many writes to the control store a second
	// a principal's token bucket refills, and ControlWriteBurst how many
	// it holds: adding, archiving and activating projects, adding,
	// disabling, enabling and setting principals, setting and removing
	// members, adding and removing keys, and inviting and removing
	// identities. The bucket is the principal's on the whole server,
	// apart from its write bucket in each project.
	ControlWriteRate  float64 `yaml:"control_writes_per_second"`
	ControlWriteBurst int     `yaml:"control_write_burst"`
}

// DefaultLimits are the limits a zero field takes.
var DefaultLimits = Limits{
	Limits: store.DefaultLimits, Conns: 1024, ConnsPerPrincipal: 64, Handshakes: 16, IdleTimeout: Duration(10 * time.Minute),
	DBConns: store.DefaultConns, DBConnsPerProject: store.DefaultReaders,
	WriteRate: 10, WriteBurst: 100, RefusalLogs: 20,
	AgentKeep: Duration(store.MaxAgentWindow), InboxKeep: Duration(30 * 24 * time.Hour),
	IdemWindow: Duration(store.DefaultIdemWindow), AwayMax: Duration(store.MaxLease),
	Control: store.DefaultControlLimits, OpenStores: 64, StoreIdle: Duration(10 * time.Minute),
	ControlWriteRate: 1, ControlWriteBurst: 30,
	HTTPSConns: 4096, HTTPSConnsPerAddr: 64, HTTPSAddrPrefixV6: 64, HTTPSHeaderBytes: 16 << 10, HTTPSHeaderTimeout: Duration(10 * time.Second), HTTPSBodyTimeout: Duration(10 * time.Second),
	HTTPSBodyBytes: 64 << 10, UpgradeTimeout: Duration(10 * time.Second), WSMessageBytes: proto.MaxFrame, WSWriteTimeout: Duration(30 * time.Second),
	AuthFailureRate: 30, AuthFailureBurst: 60, SignInsPending: 10000, SignInsPendingPerAddr: 16, DPoPProofs: 100000,
	WorkloadTokens: 10000,
}

// Duration is a YAML duration such as 90s, 10m, 8h or 7d.
type Duration time.Duration

// UnmarshalText reads a duration in proto.ParseDuration's form.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := proto.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("%q: %w", b, err)
	}
	*d = Duration(v)
	return nil
}

// WithDefaults fills each zero field from DefaultLimits.
func (l Limits) WithDefaults() Limits {
	d := DefaultLimits
	for _, f := range []struct{ v, d *int }{
		{&l.Labels, &d.Labels}, {&l.AcceptanceItems, &d.AcceptanceItems}, {&l.Deps, &d.Deps},
		{&l.Sessions, &d.Sessions}, {&l.InboxUnread, &d.InboxUnread}, {&l.Notices, &d.Notices},
		{&l.UsageRecords, &d.UsageRecords}, {&l.UsagePerDay, &d.UsagePerDay}, {&l.Paths, &d.Paths},
		{&l.Conns, &d.Conns}, {&l.ConnsPerPrincipal, &d.ConnsPerPrincipal}, {&l.Handshakes, &d.Handshakes}, {&l.WriteBurst, &d.WriteBurst},
		{&l.DBConns, &d.DBConns}, {&l.DBConnsPerProject, &d.DBConnsPerProject},
		{&l.RefusalLogs, &d.RefusalLogs}, {&l.MemoryBody, &d.MemoryBody}, {&l.MemoryTags, &d.MemoryTags},
		{&l.MemoryTagLength, &d.MemoryTagLength}, {&l.Memories, &d.Memories}, {&l.MemoryKeyLength, &d.MemoryKeyLength},
		{&l.Prices, &d.Prices}, {&l.HoursPerDay, &d.HoursPerDay}, {&l.HoursNote, &d.HoursNote}, {&l.Plans, &d.Plans},
		{&l.PlanPrincipals, &d.PlanPrincipals},
		{&l.ConflictsPerIssue, &d.ConflictsPerIssue}, {&l.ConflictsPerPrincipal, &d.ConflictsPerPrincipal},
		{&l.GateApprovers, &d.GateApprovers}, {&l.OpenGates, &d.OpenGates},
		{&l.GateChecks, &d.GateChecks}, {&l.GateEvidence, &d.GateEvidence},
		{&l.Locks, &d.Locks}, {&l.LockCapacity, &d.LockCapacity}, {&l.LockWaiters, &d.LockWaiters}, {&l.LockWaits, &d.LockWaits},
		{&l.WorkflowBytes, &d.WorkflowBytes}, {&l.WorkflowItems, &d.WorkflowItems}, {&l.WorkflowChecks, &d.WorkflowChecks},
		{&l.WorkflowCloseout, &d.WorkflowCloseout}, {&l.BriefBytes, &d.BriefBytes},
		{&l.Control.Projects, &d.Control.Projects}, {&l.Control.Principals, &d.Control.Principals},
		{&l.Control.PrincipalsPerCreator, &d.Control.PrincipalsPerCreator},
		{&l.Control.Keys, &d.Control.Keys}, {&l.Control.Members, &d.Control.Members}, {&l.OpenStores, &d.OpenStores},
		{&l.Control.Identities, &d.Control.Identities}, {&l.Control.Invites, &d.Control.Invites},
		{&l.ControlWriteBurst, &d.ControlWriteBurst},
		{&l.HTTPSConns, &d.HTTPSConns}, {&l.HTTPSConnsPerAddr, &d.HTTPSConnsPerAddr}, {&l.HTTPSAddrPrefixV6, &d.HTTPSAddrPrefixV6}, {&l.HTTPSHeaderBytes, &d.HTTPSHeaderBytes},
		{&l.HTTPSBodyBytes, &d.HTTPSBodyBytes}, {&l.WSMessageBytes, &d.WSMessageBytes}, {&l.AuthFailureBurst, &d.AuthFailureBurst},
		{&l.Control.Grants, &d.Control.Grants}, {&l.SignInsPending, &d.SignInsPending},
		{&l.SignInsPendingPerAddr, &d.SignInsPendingPerAddr}, {&l.DPoPProofs, &d.DPoPProofs},
		{&l.Control.TrustRules, &d.Control.TrustRules}, {&l.Control.TrustRulesPerPrincipal, &d.Control.TrustRulesPerPrincipal},
		{&l.Control.GrantsPerTrustRule, &d.Control.GrantsPerTrustRule}, {&l.WorkloadTokens, &d.WorkloadTokens},
	} {
		if *f.v == 0 {
			*f.v = *f.d
		}
	}
	for _, f := range []struct{ v, d *Duration }{{&l.IdleTimeout, &d.IdleTimeout}, {&l.AgentKeep, &d.AgentKeep}, {&l.InboxKeep, &d.InboxKeep},
		{&l.StoreIdle, &d.StoreIdle}, {&l.IdemWindow, &d.IdemWindow}, {&l.AwayMax, &d.AwayMax},
		{&l.HTTPSHeaderTimeout, &d.HTTPSHeaderTimeout}, {&l.HTTPSBodyTimeout, &d.HTTPSBodyTimeout}, {&l.UpgradeTimeout, &d.UpgradeTimeout}, {&l.WSWriteTimeout, &d.WSWriteTimeout}} {
		if *f.v == 0 {
			*f.v = *f.d
		}
	}
	for _, f := range []struct{ v, d *float64 }{{&l.WriteRate, &d.WriteRate}, {&l.ControlWriteRate, &d.ControlWriteRate},
		{&l.AuthFailureRate, &d.AuthFailureRate}} {
		if *f.v == 0 {
			*f.v = *f.d
		}
	}
	return l
}

// Validate refuses a negative limit, or a write rate that is NaN or
// infinite. Zero is valid: it takes the default.
func (l Limits) Validate() error {
	if err := l.Limits.Validate(); err != nil {
		return fmt.Errorf("%w; fix: correct limits: in the config file", err)
	}
	if err := l.Control.Validate(); err != nil {
		return fmt.Errorf("%w; fix: correct limits: in the config file", err)
	}
	for _, f := range []struct {
		name string
		bad  bool
	}{
		{"conns_per_server", l.Conns < 0}, {"conns_per_principal", l.ConnsPerPrincipal < 0}, {"handshakes_per_principal", l.Handshakes < 0},
		{"db_conns_per_server", l.DBConns < 0}, {"db_conns_per_project", l.DBConnsPerProject < 0},
		{"idle_timeout", l.IdleTimeout < 0},
		{"writes_per_second", l.WriteRate < 0 || math.IsNaN(l.WriteRate) || math.IsInf(l.WriteRate, 0)},
		{"write_burst", l.WriteBurst < 0}, {"refusal_logs_per_minute", l.RefusalLogs < 0},
		{"agent_keep", l.AgentKeep < 0}, {"inbox_keep", l.InboxKeep < 0},
		{"open_stores_per_server", l.OpenStores < 0}, {"store_idle", l.StoreIdle < 0},
		{"control_writes_per_second", l.ControlWriteRate < 0 || math.IsNaN(l.ControlWriteRate) || math.IsInf(l.ControlWriteRate, 0)},
		{"control_write_burst", l.ControlWriteBurst < 0},
		{"https_conns_per_server", l.HTTPSConns < 0}, {"https_conns_per_addr", l.HTTPSConnsPerAddr < 0},
		{"https_header_bytes", l.HTTPSHeaderBytes < 0}, {"https_header_timeout", l.HTTPSHeaderTimeout < 0},
		{"https_body_bytes", l.HTTPSBodyBytes < 0}, {"https_body_timeout", l.HTTPSBodyTimeout < 0}, {"upgrade_timeout", l.UpgradeTimeout < 0}, {"ws_write_timeout", l.WSWriteTimeout < 0},
		{"ws_message_bytes", l.WSMessageBytes < 0 || l.WSMessageBytes > proto.MaxFrame},
		{"auth_failures_per_minute", l.AuthFailureRate < 0 || math.IsNaN(l.AuthFailureRate) || math.IsInf(l.AuthFailureRate, 0)},
		{"auth_failure_burst", l.AuthFailureBurst < 0},
		{"signins_pending_per_server", l.SignInsPending < 0}, {"signins_pending_per_addr", l.SignInsPendingPerAddr < 0},
		{"dpop_proofs_per_server", l.DPoPProofs < 0}, {"workload_tokens_per_server", l.WorkloadTokens < 0},
	} {
		if f.bad {
			return fmt.Errorf("limit %s must be zero or a positive number; fix: correct limits: in the config file, or leave it out for the default", f.name)
		}
	}
	if p := l.HTTPSAddrPrefixV6; p < 0 || p > 128 {
		return fmt.Errorf("limit https_addr_prefix_v6 is %d; it must be from 1 to 128; fix: correct limits: in the config file, or leave it out for the default, %d", p, DefaultLimits.HTTPSAddrPrefixV6)
	}
	if w := time.Duration(l.IdemWindow); w != 0 && (w < minIdemWindow || w > maxIdemWindow) {
		return fmt.Errorf("limit idem_window is %s; it must be from %s to %s; fix: correct limits: in the config file, or leave it out for the default, %s",
			proto.Span(w), proto.Span(minIdemWindow), proto.Span(maxIdemWindow), proto.Span(store.DefaultIdemWindow))
	}
	if a := time.Duration(l.AwayMax); a != 0 && (a < minAwayMax || a > maxAwayMax) {
		return fmt.Errorf("limit away_max is %s; it must be from %s to %s; fix: correct limits: in the config file, or leave it out for the default, %s",
			proto.Span(a), proto.Span(minAwayMax), proto.Span(maxAwayMax), proto.Span(store.MaxLease))
	}
	if srv, prj := cmp.Or(l.DBConns, DefaultLimits.DBConns), cmp.Or(l.DBConnsPerProject, DefaultLimits.DBConnsPerProject); prj > srv {
		return fmt.Errorf("limit db_conns_per_project, %d, is more than db_conns_per_server, %d; fix: raise db_conns_per_server, or lower db_conns_per_project, under limits: in the config file", prj, srv)
	}
	return nil
}

// The bounds of Limits.AwayMax. An hour is the least an absence worth
// planning needs, and more than an agent's lease; past 30 days a claim
// holds work nobody is doing for longer than the work is likely to
// stay what it was, and nobody else may take it.
const (
	minAwayMax = time.Hour
	maxAwayMax = 30 * 24 * time.Hour
)

// The bounds of Limits.IdemWindow. Below an hour, a key would expire
// while a session that lost its connection still retries with it; past a
// year, nothing an outbox holds is still meant (its own caps are far
// shorter). The keys are kept whatever the window, so neither bounds
// what the server stores.
const (
	minIdemWindow = time.Hour
	maxIdemWindow = 365 * 24 * time.Hour
)

// readOps are the operations that write nothing; every other op counts
// against the principal's write bucket.
var readOps = map[string]bool{
	proto.OpShow: true, proto.OpList: true, proto.OpReady: true, proto.OpBlocked: true, proto.OpComments: true,
	proto.OpHistory: true, proto.OpDigest: true, proto.OpWho: true, proto.OpInbox: true, proto.OpWatch: true,
	proto.OpClaims: true, proto.OpRecall: true, proto.OpPrices: true, proto.OpCost: true,
	proto.OpHours: true, proto.OpPlans: true, proto.OpConflicts: true, proto.OpGates: true, proto.OpDates: true, proto.OpLocks: true,
	proto.OpWorkflow: true,
	proto.OpProjects: true, proto.OpPrincipals: true, proto.OpMembers: true, proto.OpKeys: true,
	proto.OpStatus: true, proto.OpPrincipalWhois: true,
	proto.OpPrincipalShow: true, proto.OpInvites: true, proto.OpIdentities: true, proto.OpSignIns: true,
	proto.OpTrustRules: true, proto.OpTrustShow: true,
}

// controlWrites are the operations that write to the control store.
// Each counts against the principal's control bucket
// (Limits.ControlWriteRate), not its write bucket in the project.
var controlWrites = map[string]bool{
	proto.OpProjectAdd: true, proto.OpProjectArchive: true, proto.OpProjectActivate: true,
	proto.OpPrincipalAdd: true, proto.OpPrincipalDisable: true, proto.OpPrincipalEnable: true,
	proto.OpMemberSet: true, proto.OpMemberRemove: true,
	proto.OpKeyAdd: true, proto.OpKeyRemove: true,
	proto.OpPrincipalSet: true, proto.OpInviteAdd: true, proto.OpInviteRemove: true, proto.OpIdentityRemove: true,
	proto.OpSignInEnd: true,
	proto.OpTrustAdd:  true, proto.OpTrustSet: true, proto.OpTrustRemove: true,
}

// adminOp reports whether op manages access (proto's admin.go): one of
// controlWrites, a list of projects, principals, members, keys,
// invitations or identities, a principal shown, the status report, or
// the lookup of a stand-in.
func adminOp(op string) bool {
	switch op {
	case proto.OpProjects, proto.OpPrincipals, proto.OpMembers, proto.OpKeys, proto.OpStatus, proto.OpPrincipalWhois,
		proto.OpPrincipalShow, proto.OpInvites, proto.OpIdentities, proto.OpSignIns, proto.OpTrustRules, proto.OpTrustShow:
		return true
	}
	return controlWrites[op]
}

// buckets is a token bucket per principal: each holds up to burst tokens,
// refilled at rate a second, and a write takes one.
type buckets struct {
	mu    sync.Mutex
	rate  float64
	burst float64
	m     map[string]*bucket // by principal, guarded by mu
}

// bucket is one principal's tokens, as of at.
type bucket struct {
	tokens float64
	at     time.Time
}

// take takes a token from key's bucket at now, or reports how long until
// one is there.
func (b *buckets) take(key string, now time.Time) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.m == nil {
		b.m = map[string]*bucket{}
	}
	k := b.m[key]
	if k == nil {
		k = &bucket{tokens: b.burst, at: now}
		b.m[key] = k
	}
	if el := now.Sub(k.at).Seconds(); el > 0 {
		k.tokens = math.Min(b.burst, k.tokens+el*b.rate)
	}
	k.at = now
	if k.tokens >= 1 {
		k.tokens--
		return true, 0
	}
	return false, time.Duration(math.Ceil((1-k.tokens)/b.rate)) * time.Second
}

// has reports whether key's bucket holds a token at now, without taking
// it, or how long until it will.
func (b *buckets) has(key string, now time.Time) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	k := b.m[key]
	if k == nil {
		return true, 0
	}
	tokens := k.tokens
	if el := now.Sub(k.at).Seconds(); el > 0 {
		tokens = math.Min(b.burst, tokens+el*b.rate)
	}
	if tokens >= 1 {
		return true, 0
	}
	return false, time.Duration(math.Ceil((1-tokens)/b.rate)) * time.Second
}

// prune lets go of every bucket that has filled again by now. A full
// bucket is what take makes for a key it has not seen, so letting it go
// changes no answer, and the map holds only the keys that wrote lately.
func (b *buckets) prune(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for key, k := range b.m {
		if k.tokens+max(0, now.Sub(k.at).Seconds())*b.rate >= b.burst {
			delete(b.m, key)
		}
	}
}

// conns counts connections past the handshake, in all and per principal:
// per principal per project in a server of many.
type conns struct {
	mu    sync.Mutex // guards total and by
	total int
	by    map[string]int
	// tel counts the refusals at each cap; nil counts none.
	tel *telemetry.Telemetry
}

// acquire takes a slot for key, a principal or a principal in a project
// (limitKey), or refuses, naming the cap that is full. Clients tell the
// principal's cap from the server's by proto.AtConnLimit.
func (c *conns) acquire(key string, total, per int) *proto.Error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.total >= total:
		c.tel.Count(telemetry.LimitRefusals, telemetry.LimitConns)
		return proto.Errf(proto.CodeBusy, "close other aries sessions, or wait for them to end, and retry",
			fmt.Sprintf("the server is at its connection limit (%d)", total))
	case c.by[key] >= per:
		c.tel.Count(telemetry.LimitRefusals, telemetry.LimitConnsPerPrincipal)
		return proto.ConnLimit(keyPrincipal(key), per)
	}
	if c.by == nil {
		c.by = map[string]int{}
	}
	c.total++
	c.by[key]++
	return nil
}

// release frees a slot acquire took for key.
func (c *conns) release(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total--
	if c.by[key]--; c.by[key] <= 0 {
		delete(c.by, key)
	}
}

// waiting counts, per principal, the connections that have sent their
// bridge frame and not yet a hello (Limits.Handshakes).
type waiting struct {
	mu sync.Mutex
	by map[string]int // guarded by mu
}

// enter counts one more such connection of principal, unless it has most
// already.
func (w *waiting) enter(principal string, most int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.by[principal] >= most {
		return false
	}
	if w.by == nil {
		w.by = map[string]int{}
	}
	w.by[principal]++
	return true
}

// leave uncounts a connection enter counted.
func (w *waiting) leave(principal string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.by[principal]--; w.by[principal] <= 0 {
		delete(w.by, principal)
	}
}

// count returns how many of principal's connections are counted.
func (w *waiting) count(principal string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.by[principal]
}

// sampler logs at most n refusal lines per principal a minute (S-20), so
// a principal looping refused requests cannot flood the journal; when a
// minute with suppressed lines ends, its next line says how many.
type sampler struct {
	mu sync.Mutex
	n  int
	m  map[string]*window // by principal ("" before one is known), guarded by mu
}

// window counts one principal's refusal lines in the minute from start.
type window struct {
	start      time.Time
	logged     int
	suppressed int
}

// line writes the line through log, as the event ev, unless key is over
// its allowance. ctx is the line's: a request's has its id.
func (s *sampler) line(ctx context.Context, log *slog.Logger, key string, now time.Time, level slog.Level, ev logs.Event, msg string, attrs ...any) {
	s.mu.Lock()
	if s.m == nil {
		s.m = map[string]*window{}
	}
	w := s.m[key]
	if w == nil || now.Sub(w.start) >= time.Minute || now.Before(w.start) {
		if w != nil && w.suppressed > 0 {
			log.Warn("refusal lines suppressed", "event", logs.LogSuppressed, "principal", keyPrincipal(key), "lines", w.suppressed, "per_minute", s.n)
		}
		w = &window{start: now}
		s.m[key] = w
		if len(s.m) > 4096 { // forget idle principals
			for k, v := range s.m {
				if now.Sub(v.start) >= time.Minute {
					delete(s.m, k)
				}
			}
			s.m[key] = w
		}
	}
	ok := w.logged < s.n
	if ok {
		w.logged++
	} else {
		w.suppressed++
	}
	s.mu.Unlock()
	if ok {
		log.With("event", ev).Log(ctx, level, msg, attrs...)
	}
}
