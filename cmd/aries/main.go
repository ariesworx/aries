// Command aries is the Aries client: a command line for people and, as
// `aries mcp`, an MCP server for agents, both talking to ariesd over SSH
// with a pinned host key (docs/design/aries.md §2). `aries help` lists
// the commands.
//
// It exits 0 on success, 1 on a failure, 2 on a mistake on the command
// line and 3 when its protocol version is outside the server's range,
// except that `aries lock run` exits with its command's code once the
// command has run, and 130 when an interrupt ended it.
// The work is all in [cli.Run]; main only hands it the process's streams,
// environment and version, and a context that ends on an interrupt or
// SIGTERM.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ariesworx/aries/internal/cli"
	"github.com/ariesworx/aries/internal/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], cli.Env{
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		Getenv: os.Getenv, Hostname: os.Hostname, Version: version.Version,
	})
	stop() // not deferred: os.Exit skips deferred calls
	os.Exit(code)
}
