package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/gitx"
	"github.com/ariesworx/aries/internal/proto"
)

// pullJob is the name of the setup that is no agent's: a scheduled job of
// the person's own that pulls the default branch, so that git's post-merge
// hook, and `aries land`, run for what was merged.
const pullJob = "pull-job"

// Every is how often the job runs unless --every says, and the least it
// may be: a job that fetches each minute is a poll, not a chore.
const (
	pullJobEvery    = 15 * time.Minute
	pullJobEveryMin = time.Minute
)

// pullJobMark says in each file the job is aries's, which is how --write
// and --remove tell their own file from someone else's.
const pullJobMark = "Written by `aries setup pull-job`; remove it with `aries setup pull-job --remove`."

// jobSpec is what the job is made of; the files are a function of it.
type jobSpec struct {
	GOOS    string
	Name    string // the job's name, unique to the checkout
	Exe     string // the aries to run
	Top     string // the checkout's top
	Branch  string // the default branch
	Remote  string
	KeyFile string // the person's key file, "" for the default identities
	Path    string // PATH, for the scheduler that starts the job with none
	Every   time.Duration
	Home    string
	Config  string // $XDG_CONFIG_HOME, where set
}

// jobFile is a file of the job.
type jobFile struct{ Path, Body string }

// args are the job's command line after the program.
func (j jobSpec) args() []string {
	a := []string{"-C", j.Top, pullJob, "--run", "--branch", j.Branch, "--remote", j.Remote}
	if j.KeyFile != "" {
		a = append(a, "--key-file", j.KeyFile)
	}
	return a
}

// jobName is the job's name: the checkout's directory and a short hash of
// its whole path, so that two checkouts of one name have two jobs.
func jobName(top string) string {
	sum := sha256.Sum256([]byte(top))
	base := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, filepath.Base(top))
	return "aries-pull-job-" + base + "-" + hex.EncodeToString(sum[:4])
}

// files returns the files the job is, in the order they are written; none
// for Windows, whose Task Scheduler keeps its tasks in no file of ours.
func (j jobSpec) files() []jobFile {
	switch j.GOOS {
	case "darwin":
		return []jobFile{{filepath.Join(j.Home, "Library", "LaunchAgents", j.Name+".plist"), j.plist()}}
	case "windows":
		return nil
	}
	dir := filepath.Join(j.Home, ".config", "systemd", "user")
	if j.Config != "" {
		dir = filepath.Join(j.Config, "systemd", "user")
	}
	return []jobFile{
		{filepath.Join(dir, j.Name+".service"), j.service()},
		{filepath.Join(dir, j.Name+".timer"), j.timer()},
	}
}

func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func (j jobSpec) plist() string {
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!-- " + pullJobMark + " -->\n")
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\">\n<dict>\n")
	b.WriteString("\t<key>Label</key>\n\t<string>" + xmlText(j.Name) + "</string>\n")
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, a := range append([]string{j.Exe}, j.args()...) {
		b.WriteString("\t\t<string>" + xmlText(a) + "</string>\n")
	}
	b.WriteString("\t</array>\n")
	b.WriteString("\t<key>StartInterval</key>\n\t<integer>" + strconv.Itoa(int(j.Every/time.Second)) + "</integer>\n")
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	if j.Path != "" {
		b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n\t\t<key>PATH</key>\n\t\t<string>" + xmlText(j.Path) + "</string>\n\t</dict>\n")
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// unitArg quotes one word of a systemd command line.
func unitArg(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, "$", "$$").Replace(s)
	return `"` + s + `"`
}

func (j jobSpec) service() string {
	words := make([]string, 0, 12)
	for _, a := range append([]string{j.Exe}, j.args()...) {
		words = append(words, unitArg(a))
	}
	return "# " + pullJobMark + "\n[Unit]\nDescription=Pull the default branch of " + strings.ReplaceAll(j.Top, "%", "%%") + " and report landed\n\n[Service]\nType=oneshot\nExecStart=" + strings.Join(words, " ") + "\n"
}

func (j jobSpec) timer() string {
	return "# " + pullJobMark + "\n[Unit]\nDescription=Run " + j.Name + ".service every " + j.Every.String() + "\n\n[Timer]\nOnBootSec=1min\nOnUnitActiveSec=" +
		strconv.Itoa(int(j.Every/time.Second)) + "s\n\n[Install]\nWantedBy=timers.target\n"
}

// winCommand is the task's command line, as schtasks takes it in /TR.
func (j jobSpec) winCommand() string {
	words := make([]string, 0, 12)
	for _, a := range append([]string{j.Exe}, j.args()...) {
		words = append(words, `"`+strings.ReplaceAll(a, `"`, `\"`)+`"`)
	}
	return strings.Join(words, " ")
}

// create, remove and query are the scheduler's commands: the program and
// its arguments. start is what runs after the files are written; stop
// before they are removed.
func (j jobSpec) create() [][]string {
	switch j.GOOS {
	case "darwin":
		return [][]string{{"launchctl", "load", "-w", j.files()[0].Path}}
	case "windows":
		return [][]string{{"schtasks", "/Create", "/F", "/TN", j.Name, "/SC", "MINUTE", "/MO", strconv.Itoa(int(j.Every / time.Minute)), "/TR", j.winCommand()}}
	}
	return [][]string{{"systemctl", "--user", "daemon-reload"}, {"systemctl", "--user", "enable", "--now", j.Name + ".timer"}}
}

func (j jobSpec) stop() [][]string {
	switch j.GOOS {
	case "darwin":
		return [][]string{{"launchctl", "unload", "-w", j.files()[0].Path}}
	case "windows":
		return [][]string{{"schtasks", "/Delete", "/F", "/TN", j.Name}}
	}
	return [][]string{{"systemctl", "--user", "disable", "--now", j.Name + ".timer"}}
}

func (j jobSpec) query() []string { return []string{"schtasks", "/Query", "/TN", j.Name} }

func words(c []string) string { return strings.Join(c, " ") }

// pullJobNote says what the job needs that aries cannot give it.
const pullJobNote = "The job fetches with git, unattended: the remote must need nothing a person types (an HTTPS URL of a public repository, a credential helper, or an ssh key in ssh-agent that the job's session can reach). It reports landed with your key, from the session cli, and what it reports is worth what this checkout saw; the key is the one in %s for the job, or else your default identities (`key:` in your own config, never the project's)."

// setupPullJob prints, checks, writes or removes the scheduled job that
// pulls the checkout's default branch. It writes only when asked, never
// replaces a file that is not its own, and removes only its own job.
func (r *runner) setupPullJob(ctx context.Context, mode setupMode, every time.Duration, keyFile string, global, all bool, usage string) error {
	switch {
	case global:
		return usagef(usage, "pull-job is one repository's job: it takes no --global")
	case all:
		return usagef(usage, "--all sets up agents, and the job is set up by name: `aries setup pull-job`")
	case every != 0 && every < pullJobEveryMin:
		return usagef(usage, "--every must be at least %s", pullJobEveryMin)
	}
	if every == 0 {
		every = pullJobEvery
	}
	every = every.Truncate(time.Second)
	top, err := gitx.Top(ctx, r.dir)
	if err != nil {
		return proto.Errf(proto.CodeInvalid, "run it in a checkout of the project's repository", fmt.Sprintf("find the repository's top: %v", err))
	}
	exe, err := r.env.Executable()
	if err != nil || exe == "" {
		return proto.Errf(proto.CodeInvalid, "run aries by its path", "find the aries program")
	}
	home, err := r.env.UserHomeDir()
	if err != nil || home == "" {
		return proto.Errf(proto.CodeInvalid, "set HOME", "cannot find the home directory")
	}
	spec := jobSpec{GOOS: r.env.GOOS, Exe: exe, Top: top, Remote: "origin", KeyFile: keyFile, Path: r.env.Getenv("PATH"), Every: every,
		Home: home, Config: r.env.Getenv("XDG_CONFIG_HOME"), Name: jobName(top)}
	if !gitx.HasRemote(ctx, top, spec.Remote) {
		return proto.Errf(proto.CodeInvalid, "add the remote the default branch is pulled from: `git remote add origin URL`", "this repository has no remote named origin")
	}
	if spec.Branch, err = gitx.DefaultBranch(ctx, top); err != nil || spec.Branch == "" {
		return proto.Errf(proto.CodeInvalid, "fetch origin once, so that git knows its default branch", "git does not say which branch is the default")
	}
	out := r.env.Stdout
	files := spec.files()
	cur := make([][]byte, len(files))
	for i, f := range files {
		b, err := os.ReadFile(f.Path) //nolint:gosec // the job's own file, in the person's scheduler directory
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("read %s: %w", f.Path, err)
		}
		cur[i] = b
	}
	again := "aries setup pull-job"
	switch {
	case mode.check:
		for i, f := range files {
			if string(cur[i]) != f.Body {
				return proto.Errf(proto.CodeNotFound, "run `"+again+" --write`", fmt.Sprintf("%s is not the job aries would write", f.Path))
			}
		}
		if len(files) == 0 {
			if r.env.RunProgram == nil {
				return proto.Errf(proto.CodeInvalid, "run `"+words(spec.query())+"`", "this aries cannot ask Task Scheduler")
			}
			if err := r.env.RunProgram(ctx, spec.query()[0], spec.query()[1:]...); err != nil {
				return proto.Errf(proto.CodeNotFound, "run `"+again+" --write`", "Task Scheduler has no task "+spec.Name)
			}
		}
		_, _ = fmt.Fprintf(out, "the job %s is in place, every %s\n", esc(spec.Name), every)
	case mode.remove:
		for i, f := range files {
			if len(cur[i]) > 0 && !strings.Contains(string(cur[i]), pullJobMark) {
				return proto.Errf(proto.CodeInvalid, "remove "+f.Path+" by hand", f.Path+" is not a file aries wrote, so aries leaves it")
			}
		}
		if err := r.runJob(ctx, spec.stop(), true); err != nil {
			return err
		}
		for i, f := range files {
			if len(cur[i]) == 0 {
				continue
			}
			if err := os.Remove(f.Path); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "removed %s\n", esc(f.Path))
		}
		if len(files) == 0 {
			_, _ = fmt.Fprintf(out, "removed the task %s\n", esc(spec.Name))
		}
		if spec.GOOS != "darwin" && spec.GOOS != "windows" {
			_ = r.runJob(ctx, [][]string{{"systemctl", "--user", "daemon-reload"}}, true)
		}
	case mode.write:
		for i, f := range files {
			if len(cur[i]) > 0 && !strings.Contains(string(cur[i]), pullJobMark) {
				return proto.Errf(proto.CodeInvalid, "remove "+f.Path+" by hand, or work in another checkout", f.Path+" is not a file aries wrote, and aries does not replace it")
			}
		}
		for i, f := range files {
			if string(cur[i]) == f.Body {
				_, _ = fmt.Fprintf(out, "%s is already as aries would write it\n", esc(f.Path))
				continue
			}
			if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil { //nolint:gosec // a scheduler's directory
				return err
			}
			if err := os.WriteFile(f.Path, []byte(f.Body), 0o644); err != nil { //nolint:gosec // a scheduler reads it
				return err
			}
			_, _ = fmt.Fprintf(out, "wrote %s\n", esc(f.Path))
		}
		if len(files) > 0 && spec.GOOS == "darwin" {
			_ = r.runJob(ctx, spec.stop(), true) // an older copy of this job, if one is loaded
		}
		if err := r.runJob(ctx, spec.create(), false); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "the job runs `aries pull-job --run` every %s\n"+pullJobNote+"\n", every, keyText(keyFile))
	default:
		if r.json {
			type snip struct {
				Path    string `json:"path,omitempty"`
				Snippet string `json:"snippet"`
			}
			doc := struct {
				Files    []snip     `json:"files"`
				Commands [][]string `json:"commands"`
			}{[]snip{}, spec.create()}
			for _, f := range files {
				doc.Files = append(doc.Files, snip{f.Path, f.Body})
			}
			if len(files) == 0 {
				doc.Files = append(doc.Files, snip{Snippet: spec.winCommand()})
			}
			r.emit(doc)
			return nil
		}
		_, _ = fmt.Fprintf(out, "# the job runs `aries pull-job --run` every %s; %s\n", every, "`"+again+" --write` writes it")
		for _, f := range files {
			_, _ = fmt.Fprintf(out, "\n# %s\n%s", esc(f.Path), f.Body)
		}
		_, _ = fmt.Fprintf(out, "\n# then it starts with:\n")
		for _, c := range spec.create() {
			_, _ = fmt.Fprintf(out, "%s\n", esc(words(c)))
		}
		_, _ = fmt.Fprintf(out, "\n# "+pullJobNote+"\n", keyText(keyFile))
	}
	return nil
}

// runJob runs the scheduler's commands. Without a way to run programs it
// prints them for the person to run, and a command that fails with soft
// set is a note, since the job may not have been loaded.
func (r *runner) runJob(ctx context.Context, cmds [][]string, soft bool) error {
	for _, c := range cmds {
		if r.env.RunProgram == nil {
			_, _ = fmt.Fprintf(r.env.Stdout, "run: %s\n", esc(words(c)))
			continue
		}
		if err := r.env.RunProgram(ctx, c[0], c[1:]...); err != nil && !soft {
			return proto.Errf(proto.CodeUnavailable, "run `"+words(c)+"` yourself, and read its message", err.Error())
		}
	}
	return nil
}

func keyText(keyFile string) string {
	if keyFile == "" {
		return "ARIES_KEY_FILE (--key-file)"
	}
	return esc(keyFile)
}
