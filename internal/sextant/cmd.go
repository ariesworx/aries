package sextant

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
)

// Exit codes, as aries's: 0 ok, 1 a failure, 2 a mistake on the command
// line.
const (
	ExitOK      = 0
	ExitFailure = 1
	ExitUsage   = 2
)

// DefaultConfig is the config file read when -config names none.
const DefaultConfig = "/etc/sextant/sextant.yaml"

// CommandEnv is the process sextant runs in; main fills it.
type CommandEnv struct {
	Stdout, Stderr io.Writer
	Getenv         func(string) string
	Hostname       func() (string, error)
	Version        string
}

// Command runs sextant with args, the command line after the program's
// name, and returns its exit code.
func Command(ctx context.Context, args []string, env CommandEnv) int {
	fs := flag.NewFlagSet("sextant", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintf(env.Stderr, "usage: sextant [-config FILE] [-check] [-version]\n\n"+
			"Sextant reads the projects its config names from one ariesd and exports their cost\n"+
			"over OTLP and on a Prometheus endpoint (docs/sextant.md).\n\n")
		fs.PrintDefaults()
	}
	path := fs.String("config", DefaultConfig, "the config `file`")
	check := fs.Bool("check", false, "check the config, print what it says, and exit")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(env.Stderr, "sextant takes no argument, and was given %q\n", fs.Arg(0))
		fs.Usage()
		return ExitUsage
	}
	if *showVersion {
		_, _ = fmt.Fprintf(env.Stdout, "sextant %s\n", env.Version)
		return ExitOK
	}
	cfg, err := Load(*path)
	if err != nil {
		_, _ = fmt.Fprintln(env.Stderr, err)
		return ExitFailure
	}
	if *check {
		projects := "projects"
		if len(cfg.Projects) == 1 {
			projects = "project"
		}
		listen := "listening on " + cfg.Listen
		if cfg.Listen == ListenOff {
			listen = "serving no listener"
		}
		_, _ = fmt.Fprintf(env.Stdout, "sextant: %s is good: %d %s, every %s, a window of %s, %s\n",
			*path, len(cfg.Projects), projects, cfg.Interval, cfg.Window, listen)
		return ExitOK
	}
	machine := "sextant"
	if env.Hostname != nil {
		if h, err := env.Hostname(); err == nil && h != "" {
			machine = h
		}
	}
	log := slog.New(slog.NewTextHandler(env.Stderr, nil))
	if err := Run(ctx, cfg, RunEnv{Version: env.Version, Getenv: env.Getenv, Machine: machine, Logger: log}); err != nil {
		_, _ = fmt.Fprintln(env.Stderr, err)
		return ExitFailure
	}
	return ExitOK
}
