package sextant

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/ariesworx/aries/internal/client"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/telemetry"
)

// Sink takes what a poll read: *telemetry.Sextant is one.
type Sink interface {
	// SetCost replaces project's figures; nil clears them.
	SetCost(project string, costs []telemetry.SextantCost)
	// ScrapeError counts one failure to read project.
	ScrapeError(project, op, code string)
}

// PollerEnv is what a Poller takes from its process.
type PollerEnv struct {
	// Version is Sextant's build version, which it tells the server.
	Version string
	// Getenv reads the process's environment: a workload's platform
	// variables, and ARIES_TRANSPORT. Nil reads none.
	Getenv func(string) string
	// Machine names this machine to the server.
	Machine string
	// Logger receives a line when a project's reads start failing, while
	// they fail (every errEvery), and when they recover. Nil discards.
	Logger *slog.Logger
	// Now is the clock a sign-in's end is read on. Nil is time.Now.
	Now func() time.Time
}

// errEvery is how often a project's failure that goes on is logged
// again.
const errEvery = 10 * time.Minute

// signInMargin is how long before its end a workload's sign-in is
// renewed.
const signInMargin = 2 * time.Minute

// Poller reads each project of a config, one after another, through the
// ops `aries` itself sends, and hands what it read to a Sink. It keeps a
// connection to each project open between polls, and makes a new one
// when it is lost. Poll and Close are not to be called at once.
type Poller struct {
	cfg  *Config
	sink Sink
	env  PollerEnv
	log  *slog.Logger

	mu    sync.Mutex
	conns map[string]*client.Conn
	// failing is, by project, when its failure was last logged.
	failing map[string]time.Time
	// polled is closed after the first poll, whatever it found.
	polled chan struct{}
	once   sync.Once
}

// NewPoller makes a Poller of cfg's projects that reports to sink.
func NewPoller(cfg *Config, sink Sink, env PollerEnv) *Poller {
	if env.Getenv == nil {
		env.Getenv = func(string) string { return "" }
	}
	if env.Now == nil {
		env.Now = time.Now
	}
	log := env.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Poller{cfg: cfg, sink: sink, env: env, log: log, conns: map[string]*client.Conn{}, failing: map[string]time.Time{},
		polled: make(chan struct{})}
}

// Run polls at once and then every cfg.Interval until ctx ends, and
// closes the connections.
func (p *Poller) Run(ctx context.Context) {
	defer p.Close()
	tick := time.NewTicker(p.cfg.Interval)
	defer tick.Stop()
	for {
		_ = p.Poll(ctx) // each failure is counted and logged
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Ready is "" once a poll has run, and otherwise what /readyz says.
func (p *Poller) Ready(context.Context) string {
	select {
	case <-p.polled:
		return ""
	default:
		return "no poll has run yet"
	}
}

// Poll reads every project once, and returns the failures joined.
func (p *Poller) Poll(ctx context.Context) error {
	defer p.once.Do(func() { close(p.polled) })
	var errs []error
	signInErr := p.signIn(ctx)
	for _, project := range p.cfg.Projects {
		if ctx.Err() != nil {
			return errors.Join(append(errs, ctx.Err())...)
		}
		err := signInErr
		if err == nil {
			err = p.pollProject(ctx, project)
		} else {
			p.failed(project, telemetry.SextantOpConnect, err)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", project, err))
		}
	}
	return errors.Join(errs...)
}

// pollProject reads one project's cost.
func (p *Poller) pollProject(ctx context.Context, project string) error {
	conn, err := p.conn(ctx, project)
	if err != nil {
		p.failed(project, telemetry.SextantOpConnect, err)
		return err
	}
	var prices proto.PricesResult
	if err := p.call(ctx, conn, proto.OpPrices, proto.PricesArgs{}, &prices); err != nil {
		p.dropIfLost(project, conn)
		p.failed(project, telemetry.SextantOpPrices, err)
		return err
	}
	var cost proto.CostResult
	args := proto.CostArgs{By: "model", Since: p.cfg.Window.String(), Limit: p.cfg.Limits.Models}
	if err := p.call(ctx, conn, proto.OpCost, args, &cost); err != nil {
		p.dropIfLost(project, conn)
		p.failed(project, telemetry.SextantOpCost, err)
		return err
	}
	p.sink.SetCost(project, costsByModel(cost, prices))
	p.recovered(project)
	return nil
}

// costsByModel is a cost report's groups by model, each a model the
// server has a price for or else (other): a model's name is text a
// client reported, and only the price table is the server's (decision
// 92), so no other reaches a label.
func costsByModel(cost proto.CostResult, prices proto.PricesResult) []telemetry.SextantCost {
	priced := map[string]bool{}
	for _, pr := range prices.Prices {
		priced[pr.Model] = true
	}
	out := make([]telemetry.SextantCost, 0, len(cost.Groups))
	for _, g := range cost.Groups {
		usd, err := strconv.ParseFloat(g.CostUSD, 64)
		if err != nil {
			// The server sends an exact decimal; a figure that is not one
			// is left out rather than shown as some other.
			continue
		}
		model := g.Key
		if !priced[model] {
			model = telemetry.ModelOther
		}
		out = append(out, telemetry.SextantCost{Model: model, USD: usd})
	}
	return out
}

// call sends op on conn, bounded by the request timeout.
func (p *Poller) call(ctx context.Context, conn *client.Conn, op string, args, result any) error {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Limits.RequestTimeout)
	defer cancel()
	return conn.Call(ctx, op, args, result)
}

// conn is project's open connection, or a new one.
func (p *Poller) conn(ctx context.Context, project string) (*client.Conn, error) {
	p.mu.Lock()
	c := p.conns[project]
	p.mu.Unlock()
	if c != nil {
		if c.Err() == nil {
			return c, nil
		}
		p.drop(project, c)
	}
	cc, err := p.cfg.Client(project)
	if err != nil {
		return nil, err
	}
	opts := client.Options{Version: p.env.Version, Machine: p.env.Machine, Getenv: p.getenv, UserHomeDir: noHome,
		Timeout: p.cfg.Limits.RequestTimeout, Now: p.env.Now}
	if p.cfg.Workload != nil {
		opts.DataDir = p.cfg.DataDir
	}
	c, err = client.Dial(ctx, cc, opts)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.conns[project] = c
	p.mu.Unlock()
	return c, nil
}

// getenv is the environment the client reads: the configured key as
// its key file, and no ssh-agent, so that Sextant offers its own key and
// nothing else.
func (p *Poller) getenv(k string) string {
	switch k {
	case client.EnvKeyFile:
		return p.cfg.KeyFile()
	case "SSH_AUTH_SOCK":
		return ""
	}
	return p.env.Getenv(k)
}

// noHome is the home directory Sextant has none of: it never offers the
// default identities of a home's .ssh.
func noHome() (string, error) {
	return "", errors.New("sextant offers only the key its config names")
}

// signIn renews a workload's sign-in when it is missing or about to end;
// with a key it does nothing.
func (p *Poller) signIn(ctx context.Context) error {
	if p.cfg.Workload == nil {
		return nil
	}
	cc, err := p.cfg.Client(p.cfg.Projects[0])
	if err != nil {
		return err
	}
	if s, err := client.LoadSignIn(p.cfg.DataDir, cc.Server.URL); err == nil && s != nil && s.Live(p.env.Now().Add(signInMargin)) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Limits.RequestTimeout)
	defer cancel()
	info, err := client.FetchServerInfo(ctx, cc)
	if err != nil {
		return err
	}
	s, err := client.WorkloadLogin(ctx, cc, info, p.cfg.Workload.Kind, p.cfg.Workload.Provider, p.env.Machine, client.WorkloadEnv{Getenv: p.env.Getenv})
	if err != nil {
		return err
	}
	if err := client.SaveSignIn(p.cfg.DataDir, s); err != nil {
		return err
	}
	// The connections signed in with the last one end with it.
	p.Close()
	return nil
}

// dropIfLost closes project's connection if it can no longer be used.
func (p *Poller) dropIfLost(project string, c *client.Conn) {
	if c.Err() != nil {
		p.drop(project, c)
	}
}

func (p *Poller) drop(project string, c *client.Conn) {
	p.mu.Lock()
	if p.conns[project] == c {
		delete(p.conns, project)
	}
	p.mu.Unlock()
	_ = c.Close()
}

// failed counts a failure to read project at op, clears its figures, and
// logs it unless it was logged within errEvery.
func (p *Poller) failed(project, op string, err error) {
	code := "other"
	if pe, ok := errors.AsType[*proto.Error](err); ok {
		code = string(pe.Code)
	}
	p.sink.ScrapeError(project, op, code)
	p.sink.SetCost(project, nil)
	now := time.Now()
	p.mu.Lock()
	last, was := p.failing[project]
	logIt := !was || now.Sub(last) >= errEvery
	if logIt {
		p.failing[project] = now
	}
	p.mu.Unlock()
	if logIt {
		p.log.Warn("sextant: a project could not be read; its figures are cleared until it can", "event", EventScrapeFailed,
			"project", project, "op", op, "code", code, "err", err.Error())
	}
}

// recovered notes that project was read, and says so if it had failed.
func (p *Poller) recovered(project string) {
	p.mu.Lock()
	_, was := p.failing[project]
	delete(p.failing, project)
	p.mu.Unlock()
	if was {
		p.log.Info("sextant: a project is read again", "event", EventScrapeRecovered, "project", project)
	}
}

// Close closes every connection.
func (p *Poller) Close() {
	p.mu.Lock()
	conns := p.conns
	p.conns = map[string]*client.Conn{}
	p.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// Events of Sextant's log.
const (
	EventStart           = "sextant.start"
	EventStop            = "sextant.stop"
	EventScrapeFailed    = "sextant.scrape.failed"
	EventScrapeRecovered = "sextant.scrape.recovered"
)
