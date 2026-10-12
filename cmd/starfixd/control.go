package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/ariesworx/starfix/internal/server"
	"github.com/ariesworx/starfix/internal/store"
	"github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/ssh"
)

// defaultAuthorizedKeys is the file sshd reads for the starfix account,
// as docs/server.md sets it up.
const defaultAuthorizedKeys = "/var/lib/starfix/.ssh/authorized_keys"

// maxAuthorizedKeys bounds the authorized_keys file control init reads.
// The control store's default caps allow far fewer keys than fit in it.
const maxAuthorizedKeys = 16 << 20

// controlCmd runs `starfixd control COMMAND`; init is the only command.
func controlCmd(ctx context.Context, env adminEnv, args []string) error {
	if len(args) == 0 || args[0] != "init" {
		return usageError("control takes one command: init")
	}
	return controlInit(ctx, env, args[1:])
}

// openControl opens the control database s names, which must name one.
// A failure names the SQL that creates and grants the database, since
// the usual cause is that nobody has yet.
func openControl(ctx context.Context, s server.Settings, opts store.ControlOptions) (*store.Control, error) {
	dsn, err := s.ControlDSN()
	if err != nil {
		return nil, err
	}
	opts.Admins, opts.Limits, opts.AllowUnsafeAccount = s.Admins, s.Limits.Control, s.AllowUnsafeDolt
	ctl, err := store.OpenControl(ctx, dsn, opts)
	if _, ok := errors.AsType[*store.UnsafeAccountError](err); ok {
		return nil, fmt.Errorf("open the control database: %w", err) // it names its own fix
	}
	if errors.Is(err, store.ErrInvalid) || errors.Is(err, store.ErrSchemaTooNew) {
		return nil, fmt.Errorf("open the control database %s: %w; fix: check control_db: and that this starfixd is the newest that has run here",
			s.ControlDB, err)
	}
	if err != nil {
		user := "starfix"
		if cfg, perr := mysql.ParseDSN(s.DSN); perr == nil && cfg.User != "" {
			user = cfg.User
		}
		return nil, fmt.Errorf("open the control database %s: %w; fix: check that dolt sql-server is running, and as Dolt's root create the database and grant it: "+
			"CREATE DATABASE IF NOT EXISTS %s; GRANT ALL ON %s.* TO '%s'@'localhost'; (docs/server.md, Serving many projects)",
			s.ControlDB, err, s.ControlDB, s.ControlDB, user)
	}
	return ctl, nil
}

// closeControl closes ctl, keeping err as it is when Close succeeds.
func closeControl(err error, ctl *store.Control) error {
	if cerr := ctl.Close(); cerr != nil {
		return errors.Join(err, fmt.Errorf("close the control database: %w", cerr))
	}
	return err
}

// keyLine is one usable line of an authorized_keys file: the principal
// its forced command names, and the key without its options.
type keyLine struct {
	n         int
	principal string
	key       string
}

// readAuthorizedKeys reads an authorized_keys file and returns the lines
// whose forced command is `starfixd stdio --principal NAME`, and a
// problem for each other line that is not blank or a comment. A problem
// gives the line's number and the reason, never its text.
func readAuthorizedKeys(file string) (lines []keyLine, problems []string, err error) {
	b, err := os.ReadFile(file) //nolint:gosec // the operator names the file
	if err != nil {
		return nil, nil, err
	}
	if len(b) > maxAuthorizedKeys {
		return nil, nil, fmt.Errorf("%s is larger than %d MiB", file, maxAuthorizedKeys>>20)
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, maxAuthorizedKeys)
	for n := 1; sc.Scan(); n++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		skip := func(why string) { problems = append(problems, fmt.Sprintf("line %d: %s; skipped", n, why)) }
		pub, comment, options, _, perr := ssh.ParseAuthorizedKey([]byte(text))
		if perr != nil {
			skip("not a public key line")
			continue
		}
		principal, ok := forcedPrincipal(options)
		if !ok {
			skip("no `starfixd stdio --principal NAME` forced command")
			continue
		}
		key := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
		if comment != "" {
			key += " " + comment
		}
		lines = append(lines, keyLine{n: n, principal: principal, key: key})
	}
	return lines, problems, sc.Err()
}

// forcedPrincipal returns the principal named by the forced command
// among a key line's options, when the command is starfixd's stdio
// bridge: `[PATH/]starfixd stdio ... --principal NAME`.
func forcedPrincipal(options []string) (string, bool) {
	for _, o := range options {
		cmd, ok := strings.CutPrefix(o, `command="`)
		if !ok {
			continue
		}
		f := strings.Fields(strings.TrimSuffix(cmd, `"`))
		if len(f) < 2 || path.Base(f[0]) != "starfixd" || f[1] != "stdio" {
			return "", false
		}
		for i, a := range f[2:] {
			if name, ok := strings.CutPrefix(a, "--principal="); ok {
				return name, name != ""
			}
			if a == "--principal" && i+3 < len(f) {
				return f[i+3], true
			}
		}
		return "", false
	}
	return "", false
}

// tally counts what an import added and what was there already.
type tally struct{ added, already int }

func (c tally) String() string { return fmt.Sprintf("%d added, %d already there", c.added, c.already) }

// controlInit runs `starfixd control init --name NAME [--authorized-keys
// FILE]`: the one-time move of a server of one project to a server of
// many. It adopts the server's database as the first project, without
// touching what is in it, and imports the principals and keys of the
// starfix account's authorized_keys as members of that project. It is
// safe to run again: what is there already is left as it is. It says how
// many keys are then both in the store and in the file, where removing
// one from the store does not revoke it; it never edits the file.
//
// Its writes are the operator's: it opens the control database itself,
// as the daemon's account, and records them under store.OperatorPrincipal
// (ControlOptions.Operator), so the operator need not be an admin.
func controlInit(ctx context.Context, env adminEnv, args []string) (err error) {
	var fl server.Settings
	var cfgPath, name, keysFile string
	fs, err := adminFlags("control init", args, &fl, &cfgPath, func(fs *flag.FlagSet) {
		fs.StringVar(&name, "name", "", "code name of the project adopted")
		fs.StringVar(&keysFile, "authorized-keys", defaultAuthorizedKeys, "authorized_keys file to import principals and keys from")
	})
	if err != nil {
		return err
	}
	switch {
	case fs.NArg() > 0:
		return usageError("control init takes no arguments")
	case name == "":
		return usageError("control init needs --name, a code name for the project this server already holds")
	}
	s, err := server.ResolveSettings(fl, cfgPath, env.getenv)
	if err != nil {
		return err
	}
	switch {
	case s.DSN == "":
		return errors.New("no database DSN; fix: set $STARFIXD_DSN or dsn: in the config file")
	case s.ControlDB == "":
		return fmt.Errorf("no control database; fix: set control_db: in the config file or $%s, to a database of its own such as starfix_control", server.EnvControlDB)
	case s.Project == "":
		return errors.New("no project to adopt; fix: set project: in the config file to the UUID this server serves")
	}
	cfg, err := mysql.ParseDSN(s.DSN)
	if err != nil {
		return fmt.Errorf("dsn: %w", err)
	}
	if !store.ValidDBName(cfg.DBName) {
		return fmt.Errorf("database %q in the DSN cannot be a project's: a project's database is named with a lowercase letter, then lowercase letters, digits and underscores; "+
			"fix: copy the data to a database named so and set dsn: to it", cfg.DBName)
	}
	// Read before anything is written, so a wrong path changes nothing.
	lines, problems, err := readAuthorizedKeys(keysFile)
	if err != nil {
		return fmt.Errorf("read authorized_keys: %w; fix: give the file sshd reads for the starfix account with --authorized-keys FILE", err)
	}

	// No background committer: Close makes the one Dolt commit.
	ctl, err := openControl(ctx, s, store.ControlOptions{CommitInterval: -1, Operator: true})
	if err != nil {
		return err
	}
	defer func() { err = closeControl(err, ctl) }()

	host, _ := os.Hostname()
	if host == "" {
		host = "server"
	}
	actor := store.Actor{Principal: store.OperatorPrincipal, Session: "control-init-" + time.Now().UTC().Format("20060102T150405Z"), Machine: host}

	p, err := ctl.AddProject(ctx, actor, store.NewProject{ID: s.Project, Name: name, Prefix: s.Prefix, Account: s.Account, DBName: cfg.DBName})
	if _, taken := errors.AsType[*store.TakenError](err); taken {
		return fmt.Errorf("adopt project %s: %w; fix: an adopted project keeps its name, prefix, account and database, so give the --name it was adopted with and leave prefix:, account: and dsn: as they were",
			s.Project, err)
	}
	if err != nil {
		return fmt.Errorf("adopt project %s: %w; fix: correct --name or the config file and run it again (it is safe to rerun)", s.Project, err)
	}
	// An archived project stays archived: running this again changes
	// nothing a server admin decided.
	if p.State == store.ProjectCreating {
		if p, err = ctl.ActivateProject(ctx, actor, p.ID); err != nil {
			return fmt.Errorf("activate project %s: %w; fix: run it again (it is safe to rerun)", s.Project, err)
		}
	}

	var principals, keys, members tally
	imp := importer{ctl: ctl, actor: actor, project: p.ID, had: map[string]map[string]bool{}}
	for _, l := range lines {
		why, err := imp.line(ctx, l, &principals, &keys, &members)
		if err != nil {
			return fmt.Errorf("import authorized_keys line %d: %w; fix: check that dolt sql-server is running and run it again (it is safe to rerun)", l.n, err)
		}
		if why != "" {
			problems = append(problems, fmt.Sprintf("line %d: %s; skipped", l.n, why))
		}
	}
	for _, pr := range problems {
		_, _ = fmt.Fprintf(env.stderr, "authorized_keys %s\n", pr)
	}
	_, _ = fmt.Fprintf(env.stdout, "project %s (%s) adopted from database %s: %s\n", p.Name, p.ID, p.DBName, p.State)
	_, _ = fmt.Fprintf(env.stdout, "principals: %s; keys: %s; members: %s; %d lines skipped\n", principals, keys, members, len(problems))
	// Every key this run added or found is a line of the file it read,
	// and sshd still reads that file. Nothing here edits it.
	switch n := keys.added + keys.already; {
	case n == 1:
		_, _ = fmt.Fprintf(env.stdout, "1 key in the store is also a line of %s: sshd falls back to that file, so `sfx admin key remove` does not keep the key out until its line is deleted too\n", keysFile)
	case n > 1:
		_, _ = fmt.Fprintf(env.stdout, "%d keys in the store are also lines of %s: sshd falls back to that file, so `sfx admin key remove` does not keep such a key out until its line is deleted too\n", n, keysFile)
	}
	return nil
}

// importer adds authorized_keys lines to the control store.
type importer struct {
	ctl     *store.Control
	actor   store.Actor
	project string
	// had holds, per principal seen, the fingerprints it had before its
	// first line was imported, and those added since.
	had map[string]map[string]bool
}

// line imports one line: the principal if it has no row, the key, and a
// membership of the adopted project unless the principal is a server
// admin, who needs none. It returns why the line was skipped, if the
// store refused it, and an error only when the store failed.
func (im *importer) line(ctx context.Context, l keyLine, principals, keys, members *tally) (skipped string, err error) {
	fps, seen := im.had[l.principal]
	if !seen {
		switch _, err := im.ctl.Principal(ctx, l.principal); {
		case errors.Is(err, store.ErrNotFound):
			if _, err := im.ctl.AddPrincipal(ctx, im.actor, l.principal, ""); err != nil {
				return refused(err)
			}
			principals.added++
		case err != nil:
			return refused(err)
		default:
			principals.already++
		}
		have, err := im.ctl.Keys(ctx, l.principal)
		if err != nil {
			return "", err
		}
		fps = map[string]bool{}
		for _, k := range have {
			fps[k.Fingerprint] = true
		}
		im.had[l.principal] = fps
		if !im.ctl.ConfigAdmin(l.principal) {
			switch _, ok, err := im.ctl.Role(ctx, l.principal, im.project); {
			case err != nil:
				return "", err
			case ok:
				members.already++ // whatever the role: a rerun demotes no one
			default:
				if _, err := im.ctl.SetMember(ctx, im.actor, l.principal, im.project, store.RoleMember); err != nil {
					return refused(err)
				}
				members.added++
			}
		}
	}
	k, err := im.ctl.AddKey(ctx, im.actor, l.principal, l.key)
	if err != nil {
		return refused(err)
	}
	if fps[k.Fingerprint] {
		keys.already++
	} else {
		fps[k.Fingerprint] = true
		keys.added++
	}
	return "", nil
}

// refused turns the control store's refusal of a line into the reason
// it is skipped, and passes a failure on. The store's refusals name the
// principal or the fingerprint and never repeat the line.
func refused(err error) (string, error) {
	for _, e := range []error{store.ErrInvalid, store.ErrConflict, store.ErrForbidden, store.ErrNotFound} {
		if errors.Is(err, e) {
			return strings.TrimPrefix(err.Error(), store.ErrInvalid.Error()+": "), nil
		}
	}
	return "", err
}
