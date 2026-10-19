package sextant

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/ariesworx/aries/internal/telemetry"
)

// RunEnv is what Run takes from its process.
type RunEnv struct {
	// Version is Sextant's build version.
	Version string
	// Getenv reads the process's environment: the OTEL_* variables, a
	// workload's platform variables and ARIES_TRANSPORT. Nil reads none.
	Getenv func(string) string
	// Machine names this machine to the server.
	Machine string
	// Logger is Sextant's log. Nil discards.
	Logger *slog.Logger
	// Listener, if set, is served in place of one opened at cfg.Listen.
	Listener net.Listener
}

// stopWait bounds the last export at the end.
const stopWait = 5 * time.Second

// Run exports what cfg's projects hold until ctx ends: it makes the
// exporters (OTLP when the OTEL_* variables name an endpoint, and
// Prometheus's text on the loopback listener unless listen is off),
// polls every interval, and at the end makes the last export.
func Run(ctx context.Context, cfg *Config, env RunEnv) error {
	log := env.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	serve := env.Listener != nil || cfg.Listen != ListenOff
	tel, err := telemetry.NewSextant(ctx, telemetry.SextantConfig{Version: env.Version, Getenv: env.Getenv, Logger: log, Pull: serve,
		Projects: cfg.Limits.Projects, Models: cfg.Limits.Models})
	if err != nil {
		return err
	}
	l := env.Listener
	if l == nil && serve {
		if l, err = telemetry.Listen(ctx, cfg.Listen); err != nil {
			return errors.Join(fmt.Errorf("sextant: %w", err), tel.Shutdown(context.WithoutCancel(ctx)))
		}
	}
	p := NewPoller(cfg, tel, PollerEnv{Version: env.Version, Getenv: env.Getenv, Machine: env.Machine, Logger: log})
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	served := make(chan error, 1)
	if l != nil {
		go func() {
			err := tel.Serve(ctx, l, telemetry.ServeOptions{Ready: p.Ready, Logger: log})
			if err != nil {
				// The listener failed while Sextant ran: stop, rather than
				// poll for no one.
				cancel()
			}
			served <- err
		}()
	} else {
		served <- nil
	}
	listen := ListenOff
	if l != nil {
		listen = l.Addr().String()
	}
	log.Info("sextant: started", "event", EventStart, "projects", len(cfg.Projects), "interval", cfg.Interval.String(),
		"window", cfg.Window.String(), "listen", listen, "sign_in", signInKind(cfg))
	p.Run(ctx)
	cancel()
	serveErr := <-served
	stop, done := context.WithTimeout(context.WithoutCancel(ctx), stopWait)
	defer done()
	err = errors.Join(serveErr, tel.Shutdown(stop))
	log.Info("sextant: stopped", "event", EventStop)
	return err
}

// signInKind says how Sextant signs in, for the log: key or a
// workload's kind, never the key's path or a token.
func signInKind(cfg *Config) string {
	if cfg.Workload != nil {
		return "workload " + cfg.Workload.Kind
	}
	return "key"
}
