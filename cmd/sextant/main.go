// Command sextant reads the cost of the projects its config names from
// one ariesd, through the ops aries itself sends, and exports it over
// OTLP and on a Prometheus endpoint (docs/sextant.md,
// docs/design/sextant.md). It is a binary of its own: aries links none
// of it.
//
// It exits 0 when stopped by an interrupt or SIGTERM, 1 on a failure and
// 2 on a mistake on the command line. The work is all in
// [sextant.Command]; main only hands it the process's streams,
// environment and version, and a context that ends on a signal.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ariesworx/aries/internal/sextant"
	"github.com/ariesworx/aries/internal/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := sextant.Command(ctx, os.Args[1:], sextant.CommandEnv{
		Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv, Hostname: os.Hostname, Version: version.Version,
	})
	stop() // not deferred: os.Exit skips deferred calls
	os.Exit(code)
}
