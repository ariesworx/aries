package server

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/ariesworx/starfix/internal/logs"
	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
	"github.com/ariesworx/starfix/internal/telemetry"
)

// Limits bound what one principal can make the daemon do: how many
// connections it holds, how long they idle, how fast it writes, and how
// many log lines its refusals cost (S-11, S-20, the write rate limit);
// the store's limits on what a request may hold come with them (S-4,
// S-6, S-7). They are set under limits: in starfixd's config file; a
// field left out, or zero, takes DefaultLimits.
type Limits struct {
	store.Limits `yaml:",inline"`

	// Conns caps the daemon's connections past the handshake; twice it
	// caps sockets still in the handshake.
	Conns int `yaml:"conns"`
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
	// WriteRate is how many writes a second a principal's token bucket
	// refills; WriteBurst how many it holds.
	WriteRate  float64 `yaml:"write_rate"`
	WriteBurst int     `yaml:"write_burst"`
	// RefusalLogs caps the refusal lines logged per principal a minute;
	// the rest are counted in one line when the minute ends.
	RefusalLogs int `yaml:"refusal_logs"`
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
	// ConflictAge is how long a conflict stays open before digest lists
	// it (design §18.7).
	ConflictAge Duration `yaml:"conflict_age"`
	// DueSoon is how far ahead of its due date digest, dates and prime
	// list an issue as due soon (design §20.3). Up to a year.
	DueSoon Duration `yaml:"due_soon"`
	// AwayMax is the longest a person's `sfx away` may extend their
	// claims (design §18.6): every claim of theirs, in every session,
	// holds that long without renewals, and nobody else may take it.
	// From an hour to 30 days.
	AwayMax Duration `yaml:"away_max"`

	// The rest apply to a server of many projects (control_db) only.

	// Control bounds the rows of the control store: projects,
	// principals, principals one creator who is no server admin makes,
	// keys a principal and members a project.
	Control store.ControlLimits `yaml:",inline"`
	// OpenStores caps the project stores open at once. At the cap, a
	// handshake for another project closes the longest-idle store with
	// no connection, and is refused when every store has one.
	OpenStores int `yaml:"open_stores"`
	// StoreIdle closes a project's store once it has had no connection
	// for this long. That gives back its connections to Dolt, not Dolt's
	// memory.
	StoreIdle Duration `yaml:"store_idle"`
	// ControlWriteRate is how many writes to the control store a second
	// a principal's token bucket refills, and ControlWriteBurst how many
	// it holds: adding, archiving and activating projects, adding,
	// disabling and enabling principals, setting and removing members,
	// and adding and removing keys. The bucket is the principal's on the whole server, apart
	// from its write bucket in each project.
	ControlWriteRate  float64 `yaml:"control_write_rate"`
	ControlWriteBurst int     `yaml:"control_write_burst"`

	// TelemetryRequests caps the requests the telemetry listener
	// (telemetry: listen:) serves at once; one more is answered 503.
	// Twice it caps the connections the listener holds open.
	TelemetryRequests int `yaml:"telemetry_requests"`
}

// DefaultLimits are the limits a zero field takes.
var DefaultLimits = Limits{
	Limits: store.DefaultLimits, Conns: 1024, ConnsPerPrincipal: 64, Handshakes: 16, IdleTimeout: Duration(10 * time.Minute),
	WriteRate: 10, WriteBurst: 100, RefusalLogs: 20,
	AgentKeep: Duration(store.MaxAgentWindow), InboxKeep: Duration(30 * 24 * time.Hour),
	IdemWindow: Duration(store.DefaultIdemWindow), ConflictAge: Duration(store.DefaultConflictAge), AwayMax: Duration(store.MaxLease),
	DueSoon: Duration(store.DefaultDueSoon),
	Control: store.DefaultControlLimits, OpenStores: 64, StoreIdle: Duration(10 * time.Minute),
	ControlWriteRate: 1, ControlWriteBurst: 30,
	TelemetryRequests: telemetry.DefaultRequests,
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
		{&l.RefusalLogs, &d.RefusalLogs}, {&l.MemoryBody, &d.MemoryBody}, {&l.MemoryTags, &d.MemoryTags},
		{&l.MemoryTagLength, &d.MemoryTagLength}, {&l.Memories, &d.Memories}, {&l.MemoryKeyLength, &d.MemoryKeyLength},
		{&l.Prices, &d.Prices}, {&l.HoursPerDay, &d.HoursPerDay}, {&l.HoursNote, &d.HoursNote}, {&l.Plans, &d.Plans},
		{&l.PlanPrincipals, &d.PlanPrincipals},
		{&l.ConflictsPerIssue, &d.ConflictsPerIssue}, {&l.ConflictsPerPrincipal, &d.ConflictsPerPrincipal},
		{&l.GateApprovers, &d.GateApprovers}, {&l.OpenGates, &d.OpenGates},
		{&l.Control.Projects, &d.Control.Projects}, {&l.Control.Principals, &d.Control.Principals},
		{&l.Control.PrincipalsPerCreator, &d.Control.PrincipalsPerCreator},
		{&l.Control.Keys, &d.Control.Keys}, {&l.Control.Members, &d.Control.Members}, {&l.OpenStores, &d.OpenStores},
		{&l.ControlWriteBurst, &d.ControlWriteBurst}, {&l.TelemetryRequests, &d.TelemetryRequests},
	} {
		if *f.v == 0 {
			*f.v = *f.d
		}
	}
	for _, f := range []struct{ v, d *Duration }{{&l.IdleTimeout, &d.IdleTimeout}, {&l.AgentKeep, &d.AgentKeep}, {&l.InboxKeep, &d.InboxKeep},
		{&l.StoreIdle, &d.StoreIdle}, {&l.IdemWindow, &d.IdemWindow}, {&l.ConflictAge, &d.ConflictAge},
		{&l.AwayMax, &d.AwayMax}, {&l.DueSoon, &d.DueSoon}} {
		if *f.v == 0 {
			*f.v = *f.d
		}
	}
	if l.WriteRate == 0 {
		l.WriteRate = d.WriteRate
	}
	if l.ControlWriteRate == 0 {
		l.ControlWriteRate = d.ControlWriteRate
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
		{"conns", l.Conns < 0}, {"conns_per_principal", l.ConnsPerPrincipal < 0}, {"handshakes_per_principal", l.Handshakes < 0},
		{"idle_timeout", l.IdleTimeout < 0},
		{"write_rate", l.WriteRate < 0 || math.IsNaN(l.WriteRate) || math.IsInf(l.WriteRate, 0)},
		{"write_burst", l.WriteBurst < 0}, {"refusal_logs", l.RefusalLogs < 0},
		{"agent_keep", l.AgentKeep < 0}, {"inbox_keep", l.InboxKeep < 0}, {"conflict_age", l.ConflictAge < 0},
		{"due_soon", l.DueSoon < 0 || time.Duration(l.DueSoon) > store.MaxDueSoon},
		{"open_stores", l.OpenStores < 0}, {"store_idle", l.StoreIdle < 0},
		{"control_write_rate", l.ControlWriteRate < 0 || math.IsNaN(l.ControlWriteRate) || math.IsInf(l.ControlWriteRate, 0)},
		{"control_write_burst", l.ControlWriteBurst < 0}, {"telemetry_requests", l.TelemetryRequests < 0},
	} {
		if f.bad {
			return fmt.Errorf("limit %s must be zero or a positive number; fix: correct limits: in the config file, or leave it out for the default", f.name)
		}
	}
	if w := time.Duration(l.IdemWindow); w != 0 && (w < minIdemWindow || w > maxIdemWindow) {
		return fmt.Errorf("limit idem_window is %s; it must be from %s to %s; fix: correct limits: in the config file, or leave it out for the default, %s",
			proto.Span(w), proto.Span(minIdemWindow), proto.Span(maxIdemWindow), proto.Span(store.DefaultIdemWindow))
	}
	if a := time.Duration(l.AwayMax); a != 0 && (a < minAwayMax || a > maxAwayMax) {
		return fmt.Errorf("limit away_max is %s; it must be from %s to %s; fix: correct limits: in the config file, or leave it out for the default, %s",
			proto.Span(a), proto.Span(minAwayMax), proto.Span(maxAwayMax), proto.Span(store.MaxLease))
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
	proto.OpHours: true, proto.OpPlans: true, proto.OpConflicts: true, proto.OpGates: true, proto.OpDates: true,
	proto.OpProjects: true, proto.OpPrincipals: true, proto.OpMembers: true, proto.OpKeys: true,
	proto.OpStatus: true, proto.OpPrincipalWhois: true,
}

// controlWrites are the operations that write to the control store.
// Each counts against the principal's control bucket
// (Limits.ControlWriteRate), not its write bucket in the project.
var controlWrites = map[string]bool{
	proto.OpProjectAdd: true, proto.OpProjectArchive: true, proto.OpProjectActivate: true,
	proto.OpPrincipalAdd: true, proto.OpPrincipalDisable: true, proto.OpPrincipalEnable: true,
	proto.OpMemberSet: true, proto.OpMemberRemove: true,
	proto.OpKeyAdd: true, proto.OpKeyRemove: true,
}

// adminOp reports whether op manages access (proto's admin.go): one of
// controlWrites, a list of projects, principals, members or keys, the
// status report, or the lookup of a stand-in.
func adminOp(op string) bool {
	switch op {
	case proto.OpProjects, proto.OpPrincipals, proto.OpMembers, proto.OpKeys, proto.OpStatus, proto.OpPrincipalWhois:
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
		return proto.Errf(proto.CodeBusy, "close other starfix sessions, or wait for them to end, and retry",
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
