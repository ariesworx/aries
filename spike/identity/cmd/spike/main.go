// Command spike runs the identity spike against a local Keycloak and a
// real sshd (RESULTS.md). run.sh starts both and calls it; each
// subcommand prints what it found, and never a password, a client secret,
// a token or a private key.
//
//	spike setup    make the realm, users and clients in Keycloak
//	spike login    sign alice in by -mode loopback, paste, oob or device
//	spike workload get a client-credentials token and map it to a principal
//	spike sshkeys  write keys and an aries_keys file with expiry-time lines
//	spike probe    connect with each key and compare with what was expected
//	spike hold     keep a connection open past its key's expiry-time
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: spike setup|login|workload|sshkeys|probe|hold [flags]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cmds := map[string]func(context.Context, []string) error{
		"setup":    cmdSetup,
		"login":    cmdLogin,
		"workload": cmdWorkload,
		"sshkeys":  cmdSSHKeys,
		"probe":    cmdProbe,
		"hold":     cmdHold,
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
