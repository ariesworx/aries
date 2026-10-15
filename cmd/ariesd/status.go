package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/safetext"
	"github.com/ariesworx/aries/internal/server"
	"github.com/ariesworx/aries/internal/statustext"
	"github.com/ariesworx/aries/internal/version"
)

// statusCmd runs `starfixd status [--json]`: it asks the running daemon,
// over its socket, for the report `sfx admin status` gives a server
// admin, and prints it the same way. It is for the operator, a script or
// an agent on the server with no admin key. Only the user starfixd runs
// as can reach the socket, so only that user can run it.
//
// With --json it prints exactly one JSON document: the report, or
// {"error": {...}} when there is none.
func statusCmd(ctx context.Context, env adminEnv, args []string) error {
	var fl server.Settings
	var cfgPath string
	var asJSON bool
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfgPath, "config", "", "config file")
	fs.StringVar(&fl.Socket, "socket", "", "unix socket path")
	fs.BoolVar(&asJSON, "json", false, "print one JSON document")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error())
	}
	if fs.NArg() > 0 {
		return usageError("status takes no arguments")
	}
	res, err := localStatus(ctx, env, fl, cfgPath)
	if !asJSON {
		if err != nil {
			return err
		}
		statustext.Print(env.stdout, res)
		return nil
	}
	var doc any = res
	if err != nil {
		perr, ok := errors.AsType[*proto.Error](err)
		if !ok {
			perr = &proto.Error{Code: proto.CodeUnavailable, Message: err.Error()}
		}
		doc = map[string]*proto.Error{"error": perr}
	}
	b, merr := json.Marshal(doc)
	if merr != nil {
		return fmt.Errorf("encode the status: %w", merr)
	}
	if _, werr := fmt.Fprintf(env.stdout, "%s\n", safetext.JSON(b)); werr != nil {
		return werr
	}
	if err != nil {
		return exitError(1) // the document said why
	}
	return nil
}

// localStatus resolves the daemon's socket from its settings and asks
// the daemon there for its status.
func localStatus(ctx context.Context, env adminEnv, fl server.Settings, cfgPath string) (proto.StatusResult, error) {
	s, err := server.ResolveSettings(fl, cfgPath, env.getenv)
	if errors.Is(err, fs.ErrPermission) {
		// The config file is the daemon's user's alone. root is refused it
		// too where the AppArmor profiles are loaded, which give this
		// command no capability.
		return proto.StatusResult{}, fmt.Errorf("%w; fix: run it as the user starfixd runs as, for example `sudo -u starfix -H starfixd status`", err)
	}
	if err != nil {
		return proto.StatusResult{}, err
	}
	return server.Status(ctx, s.Socket, version.Version)
}
