// Command spike runs the HTTPS transport spike (RESULTS.md): a stand-in
// for ariesd's HTTPS listener and its authorization server, the client
// side of `aries login` and of the pipes, a remote MCP client, and the
// measurements. run.sh runs all of it on a Docker network with no route
// out. Each subcommand prints what it found, and never a password, a
// client secret, a token or a private key.
//
//	spike ca        make the spike's certificate authority, certificates and CRL
//	spike setup     make Keycloak's realm, users and clients over HTTPS
//	spike jwks      write Keycloak's key set to a file, as an operator would
//	spike serve     the stand-in ariesd: HTTPS, the pipes, the authorization server, /mcp
//	spike login     `aries login` against it, and the refusals
//	spike workload  a workload token exchanged for an Aries token
//	spike mcp       a remote MCP client through the MCP authorization flow
//	spike bench     round trips and push over each pipe, direct and through the proxy, and SSH
//	spike egress    try to reach the internet, and say whether it could
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: spike ca|setup|jwks|serve|login|workload|mcp|bench|egress [flags]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmds := map[string]func(context.Context, []string) error{
		"ca":       cmdCA,
		"setup":    cmdSetup,
		"jwks":     cmdJWKS,
		"serve":    cmdServe,
		"login":    cmdLogin,
		"workload": cmdWorkload,
		"mcp":      cmdMCP,
		"bench":    cmdBench,
		"egress":   cmdEgress,
	}
	run, ok := cmds[os.Args[1]]
	if !ok {
		fmt.Fprintf(os.Stderr, "spike: unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
	if err := run(ctx, os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "spike %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}
