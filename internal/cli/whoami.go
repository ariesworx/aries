package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/ariesworx/starfix/internal/client"
)

// whoami is what `sfx whoami` reports: who the server takes this
// connection for, and which key signed it in.
type whoami struct {
	// Principal is the name the server has for the key that signed in.
	Principal string `json:"principal"`
	// Project is the project's UUID, from .starfix.yaml.
	Project string       `json:"project"`
	Server  whoamiServer `json:"server"`
	Session string       `json:"session"`
	// Key is the key that signed in: a fingerprint, never the key.
	Key client.KeyInfo `json:"key"`
	// Skipped names each default identity file that was found and could
	// not be used, with the reason.
	Skipped []string `json:"skipped,omitempty"`
}

type whoamiServer struct {
	Host    string `json:"host"`
	Port    int    `json:"port"`
	User    string `json:"user"`
	HostKey string `json:"host_key"`
}

// cmdWhoami connects and says who the server answered for. The server
// takes the first key it knows among those offered, so this is how a
// person, or an agent, checks which principal its commands act as
// (design §14, decision 35). It needs no operation of its own: the
// welcome names the principal.
func cmdWhoami(ctx context.Context, r *runner, args []string) error {
	const usage = "whoami"
	fs := r.newFlags("whoami")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef(usage, "whoami takes no arguments")
	}
	cfg, err := client.LoadConfig(r.dir)
	if err != nil {
		return err
	}
	c, err := r.connect(ctx)
	if err != nil {
		return err
	}
	out := whoami{Principal: c.Principal(), Project: cfg.Project, Session: c.Session(), Key: c.Key(), Skipped: c.SkippedKeys(),
		Server: whoamiServer{Host: cfg.Server.Host, Port: cfg.Server.Port, User: cfg.Server.User, HostKey: cfg.Server.HostKey}}
	if r.json {
		r.emit(out)
		return nil
	}
	printWhoami(r.env.Stdout, out)
	return nil
}

func printWhoami(w io.Writer, me whoami) {
	p := func(label, format string, a ...any) {
		_, _ = fmt.Fprintf(w, "%-10s "+format+"\n", append([]any{label}, a...)...)
	}
	who := me.Principal
	if who == "" {
		who = "unknown (the server is older than this sfx and does not say)"
	}
	p("principal", "%s", esc(who))
	p("project", "%s", esc(me.Project))
	p("server", "%s@%s:%d, host key %s", esc(me.Server.User), esc(me.Server.Host), me.Server.Port, esc(me.Server.HostKey))
	p("session", "%s", esc(me.Session))
	// How the key was chosen: named, and so the only one offered, or
	// found, among the default identities and ssh-agent's keys.
	chosen := ", which " + esc(me.Key.By) + " names"
	if me.Key.By == "default identity" {
		chosen = ", a default identity"
	}
	switch {
	case me.Key.Fingerprint == "":
		p("key", "unknown")
	case me.Key.Source == "file":
		p("key", "%s, from the file %s%s", esc(me.Key.Fingerprint), esc(me.Key.Path), chosen)
	case me.Key.Path != "":
		p("key", "%s, signed by ssh-agent for %s%s", esc(me.Key.Fingerprint), esc(me.Key.Path), chosen)
	default:
		p("key", "%s, from ssh-agent; no key is configured, so sfx offered your default identities and then ssh-agent's keys", esc(me.Key.Fingerprint))
	}
	for _, s := range me.Skipped {
		p("skipped", "%s", esc(s))
	}
}
