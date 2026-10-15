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
// the latest event of the control database or of an active project's by
// more than clock_behind_tolerance. It returns a
// [*server.ClockBehindError] when it is; whether that refuses the start
// is clock_behind's to say. It opens each database as it is, migrating
// none, and writes nothing.
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
	opts := store.Options{CommitInterval: -1, NoMigrate: true, AllowUnsafeAccount: s.AllowUnsafeDolt, Limits: s.Limits.Limits}
	if s.ControlDB == "" {
		st, err := store.Open(ctx, s.DSN, opts)
		if err != nil {
			return fmt.Errorf("open store: %w", err)
		}
		look(server.ProjectLog(s.Project, ""), st.ClockState())
		return closeStore(asError(worst), st)
	}
	ctl, err := openControl(ctx, s, store.ControlOptions{CommitInterval: -1})
	if err != nil {
		return err
	}
	look(server.ControlLog, ctl.ClockState())
	projects, err := ctl.Projects(ctx)
	if err = closeControl(err, ctl); err != nil {
		return fmt.Errorf("control database %s: %w", s.ControlDB, err)
	}
	for _, p := range projects {
		if p.State != store.ProjectActive {
			continue
		}
		dsn, err := server.DatabaseDSN(s.DSN, p.DBName)
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
		look(server.ProjectLog(p.ID, p.DBName), st.ClockState())
		if err := st.Close(); err != nil {
			return fmt.Errorf("project %s (database %s): %w", p.Name, p.DBName, err)
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
