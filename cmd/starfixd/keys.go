package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/ariesworx/starfix/internal/server"
)

// keysUsage is how sshd is told to run `starfixd keys`.
const keysUsage = "starfixd keys [--config FILE] [--socket PATH] [--bridge PATH] TYPE BASE64"

// keysCmd runs `starfixd keys TYPE BASE64`, sshd's AuthorizedKeysCommand
// for the starfix account on a server of many projects (design §15.2):
//
//	Match User starfix
//	    AuthorizedKeysCommand /usr/local/sbin/starfixd-keys keys %t %k
//	    AuthorizedKeysCommandUser starfix
//
// sshd runs it for every key anyone offers, before it has authenticated
// them. For a key of an active principal it prints the one
// authorized_keys line that lets the key in as that principal
// (server.AuthorizedKey). For anything else it prints nothing on
// standard output and still exits 0: an unknown key, a disabled
// principal, a daemon that is down, a server of one project, arguments
// that are not one key. sshd then reads the authorized_keys file, which
// stays the way in for the first admin and while the daemon is down.
//
// So it always returns nil. Why there was no line goes to standard
// error in one line, for an operator who runs it by hand; it never
// repeats its arguments, which are a stranger's.
func keysCmd(ctx context.Context, env adminEnv, args []string) error {
	line, err := authorizedLine(ctx, env, args)
	if err != nil {
		_, _ = fmt.Fprintf(env.stderr, "starfixd keys: no line: %v\n", err)
		return nil
	}
	_, _ = fmt.Fprintln(env.stdout, line)
	return nil
}

// authorizedLine is keysCmd's line, or why there is none.
func authorizedLine(ctx context.Context, env adminEnv, args []string) (string, error) {
	var fl server.Settings
	var cfgPath string
	bridge := server.DefaultBridge
	fs := flag.NewFlagSet("keys", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfgPath, "config", "", "config file")
	fs.StringVar(&fl.Socket, "socket", "", "unix socket path")
	fs.StringVar(&bridge, "bridge", bridge, "path of starfixd in the forced command of the line printed")
	// A flag error would repeat an argument, so it is not passed on.
	if err := fs.Parse(args); err != nil || fs.NArg() != 2 {
		return "", errors.New("it takes a key's type and base64, as sshd's %t and %k give them: " + keysUsage)
	}
	s, err := server.ResolveSettings(fl, cfgPath, env.getenv)
	if err != nil {
		return "", err
	}
	return server.AuthorizedKey(ctx, s.Socket, bridge, fs.Arg(0), fs.Arg(1))
}
