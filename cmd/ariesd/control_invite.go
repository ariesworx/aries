package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/safetext"
	"github.com/ariesworx/aries/internal/server"
	"github.com/ariesworx/aries/internal/store"
)

// controlInvite is `ariesd control invite --provider P NAME ADDRESS`: it
// invites ADDRESS at the login: provider P for the principal NAME, as
// `aries admin invite add` does, so that NAME's first sign-in there
// binds it.
//
// It is how a server whose only server admin is one person gives that
// person an identity: nobody invites onto their own principal (store,
// AccessOwnIdentity), so with no other admin there is nobody who may.
// Like control init, it is the operator's: run on the server as the
// daemon's account, it opens the control store itself and writes as
// store.OperatorPrincipal (ControlOptions.Operator), which no
// connection can be. It takes no writer's lock and runs beside the
// daemon, whose sign-ins read identities from the database.
func controlInvite(ctx context.Context, env adminEnv, args []string) (err error) {
	var fl server.Settings
	var cfgPath, provider string
	fs, err := adminFlags("control invite", args, &fl, &cfgPath, func(fs *flag.FlagSet) {
		fs.StringVar(&provider, "provider", "", "the login: provider the address signs in at")
	})
	if err != nil {
		return err
	}
	switch {
	case fs.NArg() != 2 && slices.ContainsFunc(fs.Args(), func(a string) bool { return strings.HasPrefix(a, "-") }):
		return usageError("control invite's flags come before NAME and ADDRESS: ariesd control invite --provider P NAME ADDRESS")
	case fs.NArg() != 2:
		return usageError("control invite takes NAME and ADDRESS: ariesd control invite --provider P NAME ADDRESS")
	case provider == "":
		return usageError("control invite needs --provider NAME, one of the login: providers in the config file")
	}
	name, address := fs.Arg(0), fs.Arg(1)
	s, err := server.ResolveSettings(fl, cfgPath, env.getenv)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(s.Login.Providers))
	for _, p := range s.Login.Providers {
		names = append(names, p.Name)
	}
	switch {
	case s.DSN == "":
		return errors.New("no database DSN; fix: set $ARIESD_DSN or dsn: in the config file")
	case s.ControlSchema == "":
		return fmt.Errorf("no control store; fix: set control_schema: in the config file or $%s, to a schema of its own such as aries_control", server.EnvControlSchema)
	case len(names) == 0:
		return errors.New("this server signs no one in: the config file has no login: providers; fix: add the provider under login: providers in the config file first (docs/server.md, Sign-in)")
	case !slices.Contains(names, provider):
		return fmt.Errorf("this server signs no one in at %s; fix: give --provider one of %s, the login: providers in the config file", provider, strings.Join(names, ", "))
	}

	// The operator's store makes a control store that is not there, as
	// control init needs; an invitation needs one that is, so look first.
	probe, err := openControl(ctx, s, nil, store.ControlOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	if err := probe.Close(); err != nil {
		return fmt.Errorf("close the control database: %w", err)
	}
	ctl, err := openControl(ctx, s, nil, store.ControlOptions{Operator: true})
	if err != nil {
		return err
	}
	defer func() { err = closeControl(err, ctl) }()

	host, _ := os.Hostname()
	if host == "" {
		host = "server"
	}
	actor := store.Actor{Principal: store.OperatorPrincipal, Session: "control-invite-" + time.Now().UTC().Format("20060102T150405Z"), Machine: host}
	id, _, err := ctl.Invite(ctx, actor, name, store.Invitation{Provider: provider, Address: address})
	if err != nil {
		return inviteErr(name, err)
	}
	how := "added"
	if id.Rev > 1 {
		how = "renewed"
	}
	_, _ = fmt.Fprintf(env.stdout, "invitation %d for %s: %s at %s, open until %s (%s); %s's first `aries login --provider %s` binds it\n",
		id.ID, id.Principal, safetext.Line(id.InvitedAs), id.Provider, id.InviteUntil.UTC().Format("2006-01-02 15:04 UTC"), how, id.Principal, id.Provider)
	return nil
}

// inviteErr is the refusal of an invitation of name, with its fix.
func inviteErr(name string, err error) error {
	if ie, ok := errors.AsType[*store.IdentityError](err); ok && ie.Rule == store.IdentityRuleService {
		return fmt.Errorf("%w; fix: a service signs in by a key or a trust rule: invite an address for a person, or make %s a person with `aries admin principal set %s --kind person`",
			err, name, name)
	}
	if ie, ok := errors.AsType[*store.InvitedError](err); ok {
		return fmt.Errorf("%w; fix: remove invitation %d first, with `aries admin invite remove %d`, if the address is %s's", err, ie.ID, ie.ID, name)
	}
	if le, ok := errors.AsType[*store.ControlLimitError](err); ok {
		return fmt.Errorf("%w; fix: remove an identity or invitation %s no longer uses (`aries admin identity list %s`, then `aries admin identity remove ID`), or raise %s under limits: in the config file",
			err, name, name, le.Limit)
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("no principal %s; fix: add %s first, with `aries admin principal add %s`, or by a line of authorized_keys that `ariesd control init` imports", name, name, name)
	case errors.Is(err, store.ErrInvalid):
		return fmt.Errorf("%w; fix: give the address as the provider names it, such as bob@example.com, and NAME as `aries admin principal list` shows it", err)
	}
	return fmt.Errorf("invite %s: %w; fix: check that Postgres is running and run it again (it is safe to rerun)", name, err)
}
