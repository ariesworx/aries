package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ariesworx/aries/internal/server"
	"github.com/ariesworx/aries/internal/store"
)

// clockPreflight asks, before an upgrade swaps anything, what serve
// would find at its next start: whether the server's clock is behind
// the latest event of the control store or of an active project's by
// more than clock_behind_tolerance. It returns a
// [*server.ClockBehindError] when it is; whether that refuses the start
// is clock_behind's to say. It opens each project's schema to read only
// (store.Options.ReadOnly), as it is, beside the running daemon, which
// holds their writers' locks: it migrates none and writes nothing.
//
// The daemon the upgrade restarts would refuse to start for it, the
// health check would fail, and the upgrade would put the old binary
// back and blame the release. Asking first refuses the upgrade with the
// clock's own message, with nothing changed.
func clockPreflight(ctx context.Context, env adminEnv, fl server.Settings, cfgPath string) error {
	s, err := server.ResolveSettings(fl, cfgPath, env.getenv)
	if err != nil {
		return err
	}
	if s.DSN == "" {
		return errors.New("no database DSN; fix: set $ARIESD_DSN or dsn: in the config file")
	}
	tol := time.Duration(s.ClockBehindTolerance)
	var worst *server.ClockBehindError
	look := func(name string, st store.ClockState) {
		if b := server.BehindHistory(tol, name, st); b != nil {
			if worst == nil {
				worst = b
				return
			}
			if b.History.After(worst.History) {
				b.More, worst = worst.More, b
			}
			worst.More++
		}
	}
	opts := store.Options{ReadOnly: true, AllowUnsafeAccount: s.AllowUnsafeAccount, Limits: s.Limits.Limits}
	if s.ControlSchema == "" {
		st, err := store.Open(ctx, s.DSN, opts)
		if err != nil {
			return fmt.Errorf("open store: %w", err)
		}
		look(server.ProjectLog(s.Project, ""), st.ClockState())
		return closeStore(asError(worst), st)
	}
	ctl, err := openControl(ctx, s, nil, store.ControlOptions{ReadOnly: true})
	if errors.Is(err, store.ErrSchemaOld) {
		// A control store this binary must migrate before it can read
		// it: the restarted daemon migrates it, then checks the clock
		// itself. Migrating it here would change it under the daemon
		// that still serves it.
		return nil
	}
	if err != nil {
		return err
	}
	look(server.ControlLog, ctl.ClockState())
	projects, err := ctl.Projects(ctx)
	if err = closeControl(err, ctl); err != nil {
		return fmt.Errorf("control schema %s: %w", s.ControlSchema, err)
	}
	for _, p := range projects {
		if p.State != store.ProjectActive {
			continue
		}
		dsn, err := store.SchemaDSN(s.DSN, p.Schema)
		if err != nil {
			return err
		}
		o := opts
		o.Prefix = p.Prefix
		st, err := store.Open(ctx, dsn, o)
		if err != nil {
			// serve logs a project it cannot open and starts without
			// it; its clock check passes it over too.
			continue
		}
		look(server.ProjectLog(p.ID, p.Schema), st.ClockState())
		if err := st.Close(); err != nil {
			return fmt.Errorf("project %s (schema %s): %w", p.Name, p.Schema, err)
		}
	}
	return asError(worst)
}

// asError is e as an error, nil when e is.
func asError(e *server.ClockBehindError) error {
	if e == nil {
		return nil
	}
	return e
}
