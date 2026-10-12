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
// its forced command names, the key without its options, and the key's
// fingerprint.
type keyLine struct {
	n           int
	principal   string
	key         string
	fingerprint string
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
		lines = append(lines, keyLine{n: n, principal: principal, key: key, fingerprint: ssh.FingerprintSHA256(pub)})
	}
	return lines, problems, sc.Err()
}

// forcedPrincipal returns the principal named by the forced command
// among a key line's options, when the command is starfixd's stdio
// bridge: `[PATH/]starfixd stdio ... --principal NAME`. The arguments
// are parsed as `starfixd stdio` parses them (stdioArgs), so the
// principal is the one sshd would sign the key in as: the last
// --principal of the command, and none for a command stdio would refuse.
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
		_, _, principal, _, err := stdioArgs(f[2:])
		return principal, err == nil
	}
	return "", false
}

// tally counts what an import added and what was there already.
type tally struct{ added, already int }

func (c tally) String() string { return fmt.Sprintf("%d added, %d already there", c.added, c.already) }

// undone counts what admins removed or disabled since an earlier run,
// among what the file still names: the keys, the memberships of the
// adopted project, and the principals.
type undone struct{ keys, members, principals int }

func (u undone) any() bool { return u.keys+u.members+u.principals > 0 }

// count is "1 key" or "3 keys".
func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// controlInit runs `starfixd control init --name NAME [--authorized-keys
// FILE]`: the one-time move of a server of one project to a server of
// many. It adopts the server's database as the first project, without
// touching what is in it, and imports the principals and keys of the
// starfix account's authorized_keys as members of that project. It is
// safe to run again: what is there already is left as it is, and so is
// what an admin removed or disabled since (importer), unless
// --restore-removed-access says to put it back. It says how
// many keys are then both in the store and in the file, where removing
// one from the store does not revoke it; it never edits the file.
//
// Its writes are the operator's: it opens the control database itself,
// as the daemon's account, and records them under store.OperatorPrincipal
// (ControlOptions.Operator), so the operator need not be an admin.
func controlInit(ctx context.Context, env adminEnv, args []string) (err error) {
	var fl server.Settings
	var cfgPath, name, keysFile string
	var restore bool
	fs, err := adminFlags("control init", args, &fl, &cfgPath, func(fs *flag.FlagSet) {
		fs.StringVar(&name, "name", "", "code name of the project adopted")
		fs.StringVar(&keysFile, "authorized-keys", defaultAuthorizedKeys, "authorized_keys file to import principals and keys from")
		fs.BoolVar(&restore, "restore-removed-access", false, "put back from the file what admins removed since: keys removed with `sfx admin key remove`, "+
			"memberships of the adopted project removed with `sfx admin member remove`, and principals disabled with `sfx admin principal disable`; it overrides them")
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

	imp := importer{ctl: ctl, actor: actor, project: p.ID, restore: restore, seen: map[string]*imported{}, notes: problems, skipped: len(problems)}
	for _, l := range lines {
		if err := imp.line(ctx, l); err != nil {
			return fmt.Errorf("import authorized_keys line %d: %w; fix: check that dolt sql-server is running and run it again (it is safe to rerun)", l.n, err)
		}
	}
	for _, note := range imp.notes {
		_, _ = fmt.Fprintf(env.stderr, "authorized_keys %s\n", note)
	}
	keys := imp.keys
	_, _ = fmt.Fprintf(env.stdout, "project %s (%s) adopted from database %s: %s\n", p.Name, p.ID, p.DBName, p.State)
	_, _ = fmt.Fprintf(env.stdout, "principals: %s; keys: %s; members: %s; %d lines skipped\n", imp.principals, keys, imp.members, imp.skipped)
	if k := imp.kept; k.any() {
		_, _ = fmt.Fprintf(env.stdout, "left as admins set them: %s removed, %s removed, %s disabled; to put them back from this file, run it again with --restore-removed-access\n",
			count(k.keys, "key"), count(k.members, "membership"), count(k.principals, "principal"))
	}
	if r := imp.restored; r.any() {
		_, _ = fmt.Fprintf(env.stdout, "restored from this file, over what admins had set: %s, %s, %s enabled\n",
			count(r.keys, "key"), count(r.members, "membership"), count(r.principals, "principal"))
	}
	// Every key this run added or found is a line of the file it read,
	// and sshd still reads that file. Nothing here edits it.
	switch n := keys.added + keys.already; {
	case n == 1:
		_, _ = fmt.Fprintf(env.stdout, "1 key in the store is also a line of %s: sshd reads that file too, so `sfx admin key remove` does not keep the key out until its line is deleted too\n", keysFile)
	case n > 1:
		_, _ = fmt.Fprintf(env.stdout, "%d keys in the store are also lines of %s: sshd reads that file too, so `sfx admin key remove` does not keep such a key out until its line is deleted too\n", n, keysFile)
	}
	return nil
}

// importer adds authorized_keys lines to the control store.
//
// Run again, it must not undo what an admin did since: the file is not
// edited when a member is removed, a key is removed or a principal is
// disabled, so its lines still name them (security review of stage 3a,
// F-7). The control store's event log says what was done. A key whose
// newest event is its removal, and a membership of the adopted project
// whose newest event is its removal, are left removed, and a disabled
// principal is left disabled, with nothing of its lines imported. A key
// or a membership an admin added again after removing it is in the
// store, and is simply already there. restore puts all three back from
// the file instead, each as an event of the operator's.
type importer struct {
	ctl     *store.Control
	actor   store.Actor
	project string
	restore bool
	// seen holds what is known of each principal a line has named.
	seen map[string]*imported

	principals, keys, members tally
	kept, restored            undone
	// notes are the lines for stderr, each "line N: …", and skipped how
	// many of them are lines that could not be used.
	notes   []string
	skipped int
}

// imported is one principal the file names: the fingerprints it had
// before its first line was imported and those added since, and whether
// it is left disabled, and why.
type imported struct {
	fps      map[string]bool
	disabled string
}

func (im *importer) note(l keyLine, format string, a ...any) {
	im.notes = append(im.notes, fmt.Sprintf("line %d: ", l.n)+fmt.Sprintf(format, a...))
}

// skip notes a line the store refused, and passes a failure on. The
// store's refusals name the principal or the fingerprint and never
// repeat the line.
func (im *importer) skip(l keyLine, err error) error {
	for _, e := range []error{store.ErrInvalid, store.ErrConflict, store.ErrForbidden, store.ErrNotFound} {
		if errors.Is(err, e) {
			im.note(l, "%s; skipped", strings.TrimPrefix(err.Error(), store.ErrInvalid.Error()+": "))
			im.skipped++
			return nil
		}
	}
	return err
}

// undid reports whether the newest event on target is op, a removal or
// a disabling, and who did it and when, for a note.
func (im *importer) undid(ctx context.Context, target string, op store.Op) (by string, ok bool, err error) {
	e, found, err := im.ctl.LastEvent(ctx, target)
	if err != nil || !found || e.Op != op {
		return "", false, err
	}
	return fmt.Sprintf("by %s on %s", e.Actor.Principal, e.At.Format(time.DateOnly)), true, nil
}

// line imports one line: the principal if it has no row, the key, and a
// membership of the adopted project unless the principal is a server
// admin, who needs none. It returns an error only when the store failed.
func (im *importer) line(ctx context.Context, l keyLine) error {
	st, seen := im.seen[l.principal]
	if !seen {
		var ok bool
		var err error
		st, ok, err = im.principal(ctx, l)
		if st != nil {
			im.seen[l.principal] = st
		}
		if err != nil || !ok {
			return err
		}
	}
	if st.disabled != "" {
		im.note(l, "%s was disabled %s; kept disabled, and nothing of the line imported", l.principal, st.disabled)
		return nil
	}
	if st.fps[l.fingerprint] {
		im.keys.already++
		return nil
	}
	by, removed, err := im.undid(ctx, l.fingerprint, store.OpKeyRemove)
	if err != nil {
		return err
	}
	if removed && !im.restore {
		im.kept.keys++
		im.note(l, "key %s of %s was removed %s; kept removed from the store, though sshd still accepts it by this line: delete the line to keep the key out",
			l.fingerprint, l.principal, by)
		return nil
	}
	if _, err := im.ctl.AddKey(ctx, im.actor, l.principal, l.key); err != nil {
		return im.skip(l, err)
	}
	st.fps[l.fingerprint] = true
	im.keys.added++
	if removed {
		im.restored.keys++
		im.note(l, "key %s of %s was removed %s; added again", l.fingerprint, l.principal, by)
	}
	return nil
}

// principal settles the principal a line names, the first time one
// does: its row, its state and its membership of the adopted project.
// ok is false, with the reason noted, when the store refused something
// and the line is to be skipped. The principal is returned once it has
// a row, so that it is not settled again by its next line.
func (im *importer) principal(ctx context.Context, l keyLine) (st *imported, ok bool, err error) {
	p, err := im.ctl.Principal(ctx, l.principal)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if _, err := im.ctl.AddPrincipal(ctx, im.actor, l.principal, ""); err != nil {
			return nil, false, im.skip(l, err)
		}
		im.principals.added++
	case err != nil:
		return nil, false, im.skip(l, err)
	default:
		im.principals.already++
	}
	st = &imported{fps: map[string]bool{}}
	if p.State == store.PrincipalDisabled {
		by, _, err := im.undid(ctx, l.principal, store.OpPrincipalDisable)
		if err != nil {
			return nil, false, err
		}
		if !im.restore {
			im.kept.principals++
			st.disabled = by
			return st, true, nil
		}
		if _, err := im.ctl.EnablePrincipal(ctx, im.actor, l.principal); err != nil {
			return nil, false, im.skip(l, err)
		}
		im.restored.principals++
		im.note(l, "%s was disabled %s; enabled", l.principal, by)
	}
	have, err := im.ctl.Keys(ctx, l.principal)
	if err != nil {
		return nil, false, err
	}
	for _, k := range have {
		st.fps[k.Fingerprint] = true
	}
	if im.ctl.ConfigAdmin(l.principal) {
		return st, true, nil
	}
	_, member, err := im.ctl.Role(ctx, l.principal, im.project)
	if err != nil {
		return nil, false, err
	}
	if member {
		im.members.already++ // whatever the role: a rerun demotes no one
		return st, true, nil
	}
	// A membership's events are on PROJECT/PRINCIPAL.
	by, removed, err := im.undid(ctx, im.project+"/"+l.principal, store.OpMemberRemove)
	if err != nil {
		return nil, false, err
	}
	if removed && !im.restore {
		im.kept.members++
		im.note(l, "%s was removed from the project %s; kept removed", l.principal, by)
		return st, true, nil
	}
	if _, err := im.ctl.SetMember(ctx, im.actor, l.principal, im.project, store.RoleMember); err != nil {
		return st, false, im.skip(l, err)
	}
	im.members.added++
	if removed {
		im.restored.members++
		im.note(l, "%s was removed from the project %s; added again", l.principal, by)
	}
	return st, true, nil
}
