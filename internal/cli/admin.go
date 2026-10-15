package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/ariesworx/aries/internal/client"
	"github.com/ariesworx/aries/internal/proto"
)

// Access management (design §15.4): `aries admin project`, `principal`,
// `member`, `key` (admin_key.go) and `status` (admin_status.go), for the
// admins of a server of many projects. Each command
// rides the connection of the repository it is run in, and acts on that
// repository's project unless --project names another, by UUID or name.
// The server decides who may run what.

// adminGroups are the first words `aries admin` takes.
var adminGroups = []string{"prices", "plans", "project", "principal", "member", "key", "status"}

// adminUsage is the usage of the access commands, for the command table.
const adminUsage = "admin project add NAME [--prefix P] [--account A] | list | archive PROJECT | activate PROJECT" +
	" | admin principal add NAME [--key FILE]... [--project P | --no-project] | list [--created-by NAME] | disable NAME | enable NAME | whois STAND-IN" +
	" | admin member add NAME [--project P] [--role admin] [--accept-keys] | list [--project P] [--added-by NAME] | remove NAME [--project P]" +
	" | admin key add NAME FILE|- | list NAME | list --added-by NAME | remove FINGERPRINT" +
	" | admin status [-n N] [--after NAME]"

// only refuses a flag the action does not take: any that was given and
// is not among allowed. --json is always allowed.
func only(fs *flag.FlagSet, usage, action string, allowed ...string) error {
	var err error
	fs.Visit(func(f *flag.Flag) {
		if err == nil && f.Name != "json" && !slices.Contains(allowed, f.Name) {
			err = usagef(usage, "%s does not take --%s", action, f.Name)
		}
	})
	return err
}

// listFlags registers an admin list's paging flags.
func listFlags(fs *flag.FlagSet) *proto.AdminListArgs {
	var in proto.AdminListArgs
	fs.IntVar(&in.Limit, "n", 0, fmt.Sprintf("rows to list (default %d, at most %d)", proto.AdminListDefault, proto.AdminListMax))
	fs.StringVar(&in.After, "after", "", "list the rows whose name sorts after this one")
	return &in
}

// more prints the line that says a list has rows it did not show, and
// how to see them.
func more(w io.Writer, n int, last string) {
	if n > 0 {
		_, _ = fmt.Fprintf(w, "and %d more; list them with --after %s\n", n, esc(last))
	}
}

func adminProject(ctx context.Context, r *runner, args []string) error {
	const usage = "admin project add NAME [--prefix P] [--account A] | list [-n N] [--after NAME] | archive PROJECT | activate PROJECT"
	fs := r.newFlags("admin project")
	var add proto.ProjectAddArgs
	fs.StringVar(&add.Prefix, "prefix", "", "what the project's issue ids start with (default: its name)")
	fs.StringVar(&add.Account, "account", "", "the account its issues report against by default (default: internal)")
	list := listFlags(fs)
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef(usage, "admin project needs add, list, archive or activate")
	}
	switch action := pos[0]; action {
	case "add":
		if err := only(fs, usage, "project add", "prefix", "account"); err != nil {
			return err
		}
		if len(pos) != 2 {
			return usagef(usage, "project add needs one name, a code name such as acme")
		}
		add.Name = pos[1]
		return projectAdd(ctx, r, add)
	case "list":
		if err := only(fs, usage, "project list", "n", "after"); err != nil {
			return err
		}
		if len(pos) != 1 {
			return usagef(usage, "project list takes no arguments")
		}
		var out proto.ProjectsResult
		if err := r.call(ctx, proto.OpProjects, *list, &out); err != nil {
			return err
		}
		if r.json {
			r.emit(out)
			return nil
		}
		printProjects(r.env.Stdout, out)
		return nil
	case "archive", "activate":
		if err := only(fs, usage, "project "+action); err != nil {
			return err
		}
		if len(pos) != 2 {
			return usagef(usage, "project %s needs one project, a UUID or a name from `aries admin project list`", action)
		}
		op := proto.OpProjectArchive
		if action == "activate" {
			op = proto.OpProjectActivate
		}
		var out proto.ProjectResult
		if err := r.call(ctx, op, proto.ProjectArgs{Project: pos[1]}, &out); err != nil {
			return err
		}
		if r.json {
			r.emit(out)
			return nil
		}
		_, _ = fmt.Fprintf(r.env.Stdout, "project %s (%s): %s\n", esc(out.Name), esc(out.ID), esc(out.State))
		printReleased(r.env.Stdout, out.Released, "its members")
		return nil
	}
	return usagef(usage, "unknown project action %q", pos[0])
}

// projectAdd creates a project and prints its id and the .aries.yaml
// to commit to its repository: this repository's server, with the new
// project. The config is read first, so that a project is not created
// and then left without the one thing its admin needs next.
func projectAdd(ctx context.Context, r *runner, in proto.ProjectAddArgs) error {
	cfg, err := client.LoadConfig(r.dir)
	if err != nil {
		return err
	}
	var out proto.ProjectAddResult
	if err := r.call(ctx, proto.OpProjectAdd, in, &out); err != nil {
		return err
	}
	yaml, err := cfg.ForProject(out.ID)
	if err != nil {
		return proto.Errf(proto.CodeInvalid, "write its "+client.ConfigFile+" by hand: this repository's, with that project",
			fmt.Sprintf("project %s was created as %s, but the server sent an id that is no UUID", in.Name, out.ID))
	}
	if r.json {
		r.emit(struct {
			proto.ProjectAddResult
			Config string `json:"aries_yaml"`
		}{out, string(yaml)})
		return nil
	}
	how := "created"
	if out.Resumed {
		how = "created, finishing an earlier attempt,"
	}
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(r.env.Stdout, format, a...) }
	p("project %s (%s) %s in schema %s: %s\n", esc(out.Name), esc(out.ID), how, esc(out.Database), esc(out.State))
	p("commit this as %s in the project's repository:\n\n%s", client.ConfigFile, yaml)
	return nil
}

// printProjects renders a project list. The role and database columns
// show when a row has one: a role is the caller's own, and the database
// is shown to server admins.
func printProjects(w io.Writer, res proto.ProjectsResult) {
	if len(res.Projects) == 0 {
		_, _ = fmt.Fprintln(w, "no projects")
		return
	}
	roles := slices.ContainsFunc(res.Projects, func(p proto.Project) bool { return p.Role != "" })
	dbs := slices.ContainsFunc(res.Projects, func(p proto.Project) bool { return p.Database != "" })
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	head := "NAME\tID\tPREFIX\tACCOUNT\tSTATE"
	if roles {
		head += "\tROLE"
	}
	if dbs {
		head += "\tSCHEMA"
	}
	_, _ = fmt.Fprintln(tw, head)
	for _, p := range res.Projects {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s", esc(p.Name), esc(p.ID), esc(p.Prefix), esc(p.Account), esc(p.State))
		if roles {
			_, _ = fmt.Fprintf(tw, "\t%s", esc(p.Role))
		}
		if dbs {
			_, _ = fmt.Fprintf(tw, "\t%s", esc(p.Database))
		}
		_, _ = fmt.Fprintln(tw)
	}
	_ = tw.Flush()
	more(w, res.More, res.Projects[len(res.Projects)-1].Name)
}

func adminPrincipal(ctx context.Context, r *runner, args []string) error {
	const usage = "admin principal add NAME [--key FILE]... [--project P | --no-project] | list [--created-by NAME] [-n N] [--after NAME] | disable NAME | enable NAME | whois STAND-IN"
	fs := r.newFlags("admin principal")
	var add proto.PrincipalAddArgs
	var keyFiles fileFlag
	createdBy := fs.String("created-by", "", "list the principals this principal created")
	fs.StringVar(&add.Project, "project", "", "the project the new principal joins: a UUID or a name (default: this repository's)")
	fs.BoolVar(&add.Unassigned, "no-project", false, "add the principal to no project (server admins only)")
	fs.Var(&keyFiles, "key", "a public key file to add with the principal, such as ~/.ssh/id_ed25519.pub, or - for standard input (repeatable)")
	list := listFlags(fs)
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef(usage, "admin principal needs add, list, disable, enable or whois")
	}
	switch action := pos[0]; action {
	case "add":
		if err := only(fs, usage, "principal add", "project", "no-project", "key"); err != nil {
			return err
		}
		switch {
		case len(pos) != 2:
			return usagef(usage, "principal add needs one name, such as bob")
		case add.Unassigned && add.Project != "":
			return usagef(usage, "give --project or --no-project, not both")
		case len(keyFiles) > proto.MaxKeysPerAdd:
			return usagef(usage, "principal add takes at most %d keys; add the rest with `aries admin key add %s FILE`", proto.MaxKeysPerAdd, pos[1])
		}
		add.Name = pos[1]
		// Every file is read before anything is sent.
		for _, f := range keyFiles {
			line, err := r.publicKey(f)
			if err != nil {
				return err
			}
			add.Keys = append(add.Keys, line)
		}
		var out proto.PrincipalAddResult
		if err := r.call(ctx, proto.OpPrincipalAdd, add, &out); err != nil {
			return err
		}
		if r.json {
			r.emit(out)
			return nil
		}
		if out.Project == "" {
			_, _ = fmt.Fprintf(r.env.Stdout, "principal %s added, in no project\n", esc(out.ID))
		} else {
			_, _ = fmt.Fprintf(r.env.Stdout, "principal %s added, a member of project %s\n", esc(out.ID), esc(out.ProjectName))
		}
		for _, k := range out.Keys {
			_, _ = fmt.Fprintf(r.env.Stdout, "key %s added to %s\n", esc(k), esc(out.ID))
		}
		r.warn(out.Warning)
		return nil
	case "list":
		if err := only(fs, usage, "principal list", "n", "after", "created-by"); err != nil {
			return err
		}
		if len(pos) != 1 {
			return usagef(usage, "principal list takes no arguments")
		}
		var out proto.PrincipalsResult
		if err := r.call(ctx, proto.OpPrincipals, proto.PrincipalsArgs{CreatedBy: *createdBy, AdminListArgs: *list}, &out); err != nil {
			return err
		}
		if r.json {
			r.emit(out)
			return nil
		}
		printPrincipals(r.env.Stdout, out)
		return nil
	case "disable", "enable":
		if err := only(fs, usage, "principal "+action); err != nil {
			return err
		}
		if len(pos) != 2 {
			return usagef(usage, "principal %s needs one name", action)
		}
		op := proto.OpPrincipalDisable
		if action == "enable" {
			op = proto.OpPrincipalEnable
		}
		var out proto.PrincipalResult
		if err := r.call(ctx, op, proto.PrincipalArgs{Name: pos[1]}, &out); err != nil {
			return err
		}
		if r.json {
			r.emit(out)
			return nil
		}
		_, _ = fmt.Fprintf(r.env.Stdout, "%s: %s\n", esc(out.ID), esc(out.State))
		printReleased(r.env.Stdout, out.Released, out.ID)
		r.warn(out.Warning)
		return nil
	case "whois":
		// The stand-in a trace carries in place of a principal's name
		// (the server's `telemetry: principals: pseudonym`), resolved by
		// the server, which holds the key: for server admins.
		if err := only(fs, usage, "principal whois"); err != nil {
			return err
		}
		if len(pos) != 2 {
			return usagef(usage, "principal whois needs one stand-in, such as p_0123456789abcdef")
		}
		var out proto.PrincipalWhoisResult
		if err := r.call(ctx, proto.OpPrincipalWhois, proto.PrincipalWhoisArgs{StandIn: pos[1]}, &out); err != nil {
			return err
		}
		if r.json {
			r.emit(out)
			return nil
		}
		printWhois(r.env.Stdout, out)
		return nil
	}
	return usagef(usage, "unknown principal action %q", pos[0])
}

// printWhois says whose a stand-in is, and that the principal is
// disabled when it is.
func printWhois(w io.Writer, res proto.PrincipalWhoisResult) {
	note := ""
	if res.State != "" && res.State != "active" {
		note = " (" + esc(res.State) + ")"
	}
	_, _ = fmt.Fprintf(w, "%s is %s%s\n", esc(res.StandIn), esc(res.Name), note)
}

func printPrincipals(w io.Writer, res proto.PrincipalsResult) {
	if len(res.Principals) == 0 {
		_, _ = fmt.Fprintln(w, "no principals")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tSTATE\tADDED BY\tADDED")
	for _, p := range res.Principals {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", esc(p.Name), esc(p.State), esc(p.CreatedBy), when(p.CreatedAt))
	}
	_ = tw.Flush()
	more(w, res.More, res.Principals[len(res.Principals)-1].Name)
}

func adminMember(ctx context.Context, r *runner, args []string) error {
	const usage = "admin member add NAME [--project P] [--role member|admin] [--accept-keys] | list [--project P] [--added-by NAME] [-n N] [--after NAME] | remove NAME [--project P]"
	fs := r.newFlags("admin member")
	addedBy := fs.String("added-by", "", "list the memberships this principal added, in every project you may list, or in --project")
	project := fs.String("project", "", "the project: a UUID or a name (default: this repository's)")
	role := fs.String("role", "member", "member or admin")
	accept := fs.Bool("accept-keys", false, "add a principal who has keys without being shown them and asked: each of its keys signs in to the project too")
	list := listFlags(fs)
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef(usage, "admin member needs add, list or remove")
	}
	switch pos[0] {
	case "add":
		if err := only(fs, usage, "member add", "project", "role", "accept-keys"); err != nil {
			return err
		}
		switch {
		case len(pos) != 2:
			return usagef(usage, "member add needs one principal, such as bob")
		case *role != "member" && *role != "admin":
			return usagef(usage, "--role must be member or admin")
		}
		in := proto.MemberSetArgs{Name: pos[1], Project: *project, Role: *role, AcceptKeys: *accept}
		var out proto.MemberSetResult
		if err := r.call(ctx, proto.OpMemberSet, in, &out); err != nil {
			return err
		}
		if out.Change == proto.MemberPending {
			return memberPending(ctx, r, in, out)
		}
		if r.json {
			r.emit(out)
			return nil
		}
		printMemberSet(r.env.Stdout, out)
		return nil
	case "list":
		if err := only(fs, usage, "member list", "project", "n", "after", "added-by"); err != nil {
			return err
		}
		if len(pos) != 1 {
			return usagef(usage, "member list takes no arguments; name a project with --project")
		}
		var out proto.MembersResult
		if err := r.call(ctx, proto.OpMembers, proto.MembersArgs{Project: *project, AddedBy: *addedBy, AdminListArgs: *list}, &out); err != nil {
			return err
		}
		if r.json {
			r.emit(out)
			return nil
		}
		if *addedBy != "" {
			printMembersAddedBy(r.env.Stdout, *addedBy, out)
			return nil
		}
		printMembers(r.env.Stdout, out)
		return nil
	case "remove":
		if err := only(fs, usage, "member remove", "project"); err != nil {
			return err
		}
		if len(pos) != 2 {
			return usagef(usage, "member remove needs one principal")
		}
		var out proto.MemberRemoveResult
		if err := r.call(ctx, proto.OpMemberRemove, proto.MemberArgs{Name: pos[1], Project: *project}, &out); err != nil {
			return err
		}
		if r.json {
			r.emit(out)
			return nil
		}
		_, _ = fmt.Fprintf(r.env.Stdout, "%s removed from project %s\n", esc(out.ID), esc(out.ProjectName))
		printReleased(r.env.Stdout, out.Released, out.ID)
		r.warn(out.Warning)
		return nil
	}
	return usagef(usage, "unknown member action %q", pos[0])
}

// memberPending handles a `member add` the server did not make, because
// the principal has keys and each would sign in to the project too
// (design §14, decision 36). It shows the keys. On a terminal it asks,
// and adds the principal on a yes. Anywhere else it refuses, naming
// --accept-keys, so that a script never adds by accident and can add on
// purpose; with --json the one document holds the refusal and the keys.
func memberPending(ctx context.Context, r *runner, in proto.MemberSetArgs, shown proto.MemberSetResult) error {
	again := "aries admin member add " + in.Name
	if in.Project != "" {
		again += " --project " + in.Project
	}
	if in.Role == "admin" {
		again += " --role admin"
	}
	refusal := proto.Errf(proto.CodeConflict,
		fmt.Sprintf("check each key with %s, then run `%s --accept-keys`; on a terminal, without --json, aries shows the keys and asks", in.Name, again),
		fmt.Sprintf("%s was not added to project %s: it has %s, and each would sign in to the project too", shown.ID, shown.ProjectName, count(len(shown.Carried), "key")))
	if r.json {
		return &refusedWith{err: refusal, key: "member", value: shown}
	}
	out := r.env.Stdout
	_, _ = fmt.Fprintf(out, "%s has %s, and each would sign in to project %s too:\n", esc(shown.ID), count(len(shown.Carried), "key"), esc(shown.ProjectName))
	printCarried(out, shown)
	if !r.env.Terminal() {
		return refusal
	}
	_, _ = fmt.Fprintf(out, "Add %s to project %s with these keys? [y/N] ", esc(shown.ID), esc(shown.ProjectName))
	r.flush()
	// A line that was not finished, or could not be read, is a no.
	line, err := bufio.NewReader(r.env.Stdin).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); err != nil || (a != "y" && a != "yes") {
		return proto.Errf(proto.CodeConflict, "nothing changed; run the command again to add them",
			fmt.Sprintf("%s was not added to project %s", shown.ID, shown.ProjectName))
	}
	in.AcceptKeys = true
	var added proto.MemberSetResult
	if err := r.call(ctx, proto.OpMemberSet, in, &added); err != nil {
		return err
	}
	printMemberSet(out, added)
	// A key added between the question and the answer came in unseen.
	for _, k := range added.Carried {
		if !slices.ContainsFunc(shown.Carried, func(s proto.CarriedKey) bool { return s.Fingerprint == k.Fingerprint }) {
			r.warn(fmt.Sprintf("key %s was added to %s by %s after you were asked, and signs in to project %s too; check it, or undo with `aries admin member remove %s`",
				k.Fingerprint, added.ID, k.AddedBy, added.ProjectName, added.ID))
		}
	}
	return nil
}

// count is "1 key" or "3 keys".
func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// printCarried lists the keys of a principal joining a project: who
// added each and when, and whether that was an admin of the project. A
// line after the table says what a "no" means.
func printCarried(w io.Writer, m proto.MemberSetResult) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "FINGERPRINT\tTYPE\tCOMMENT\tADDED BY\tADDED\tADMIN HERE")
	other := false
	for _, k := range m.Carried {
		here := "yes"
		if !k.AdderAdministers {
			here, other = "no", true
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", esc(k.Fingerprint), esc(k.Type), esc(k.Comment), esc(k.AddedBy), when(k.AddedAt), here)
	}
	_ = tw.Flush()
	if other {
		_, _ = fmt.Fprintf(w, "ADMIN HERE no: whoever added the key is no admin of project %s; ask %s whether the key is theirs\n", esc(m.ProjectName), esc(m.ID))
	}
}

// printMemberSet says what member add did, and which keys came with a
// principal it added: a key signs in to every project its principal is
// in.
func printMemberSet(w io.Writer, m proto.MemberSetResult) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	switch m.Change {
	case "added":
		p("%s added to project %s as %s\n", esc(m.ID), esc(m.ProjectName), esc(m.Role))
	case "changed":
		p("%s is now %s in project %s\n", esc(m.ID), esc(m.Role), esc(m.ProjectName))
	default:
		p("%s is already %s in project %s\n", esc(m.ID), esc(m.Role), esc(m.ProjectName))
	}
	if len(m.Carried) > 0 {
		p("%s now signs in to project %s with %s:\n", esc(m.ID), esc(m.ProjectName), count(len(m.Carried), "key"))
		printCarried(w, m)
	}
}

// printMembersAddedBy lists the memberships one principal added, across
// projects: what that principal's account left behind.
func printMembersAddedBy(w io.Writer, by string, res proto.MembersResult) {
	if len(res.Members) == 0 {
		_, _ = fmt.Fprintf(w, "no memberships added by %s\n", esc(by))
		return
	}
	_, _ = fmt.Fprintf(w, "memberships added by %s\n", esc(by))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PROJECT\tPRINCIPAL\tROLE\tADDED")
	for _, m := range res.Members {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", esc(m.ProjectName), esc(m.Principal), esc(m.Role), when(m.AddedAt))
	}
	_ = tw.Flush()
	last := res.Members[len(res.Members)-1]
	more(w, res.More, last.ProjectName+"/"+last.Principal)
}

func printMembers(w io.Writer, res proto.MembersResult) {
	if len(res.Members) == 0 {
		_, _ = fmt.Fprintf(w, "no members of project %s\n", esc(res.ProjectName))
		return
	}
	_, _ = fmt.Fprintf(w, "members of project %s (%s)\n", esc(res.ProjectName), esc(res.Project))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PRINCIPAL\tROLE\tADDED BY\tADDED")
	for _, m := range res.Members {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", esc(m.Principal), esc(m.Role), esc(m.AddedBy), when(m.AddedAt))
	}
	_ = tw.Flush()
	more(w, res.More, res.Members[len(res.Members)-1].Principal)
}

// adminNeeds is what `aries admin` says when no group was named.
var adminNeeds = "admin needs " + strings.Join(adminGroups[:len(adminGroups)-1], ", ") + " or " + adminGroups[len(adminGroups)-1]
