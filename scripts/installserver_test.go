package scripts_test

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/version"
)

// The server installer sits beside install.sh, at the repository's root,
// and docs/server.md is what it follows.
const (
	installServer = "install-server.sh"
	serverGuide   = "docs/server.md"
)

// installStep is one numbered step: the installer's, or a row of the
// guide's table of them.
type installStep struct {
	n     string
	title string
}

func (s installStep) String() string { return s.n + ". " + s.title }

// scriptSteps returns the installer's steps, from its `step N 'TITLE'`
// lines, in the order they appear.
func scriptSteps(t *testing.T) []installStep {
	t.Helper()
	var steps []installStep
	for _, m := range regexp.MustCompile(`(?m)^\tstep ([0-9]+) '([^']+)'$`).FindAllStringSubmatch(read(t, installServer), -1) {
		steps = append(steps, installStep{m[1], m[2]})
	}
	return steps
}

// guideSection returns the text of the section of docs/server.md headed
// by heading, up to the next heading of its level or above.
func guideSection(t *testing.T, heading string) string {
	t.Helper()
	_, rest, ok := strings.Cut(read(t, serverGuide), "\n"+heading+"\n")
	if !ok {
		t.Fatalf("%s has no heading %q", serverGuide, heading)
	}
	level := strings.Index(heading, " ")
	if end := regexp.MustCompile(`(?m)^#{1,` + fmt.Sprint(level) + `} `).FindStringIndex(rest); end != nil {
		rest = rest[:end[0]]
	}
	return rest
}

// guideTable returns the rows of the table in the guide's "With the
// installer": each step's number and title, and the anchors its last
// column links to.
func guideTable(t *testing.T) ([]installStep, [][]string) {
	t.Helper()
	var steps []installStep
	var anchors [][]string
	link := regexp.MustCompile(`\]\(#([^)]+)\)`)
	for _, m := range regexp.MustCompile(`(?m)^\| ([0-9]+) \| ([^|]+?) \| (.+) \|$`).FindAllStringSubmatch(guideSection(t, "### With the installer"), -1) {
		steps = append(steps, installStep{m[1], m[2]})
		var as []string
		for _, l := range link.FindAllStringSubmatch(m[3], -1) {
			as = append(as, l[1])
		}
		anchors = append(anchors, as)
	}
	return steps, anchors
}

// guideAnchors returns the anchor GitHub gives each heading of the guide.
func guideAnchors(t *testing.T) map[string]bool {
	t.Helper()
	anchors := map[string]bool{}
	drop := regexp.MustCompile(`[^a-z0-9 _-]`)
	for _, m := range regexp.MustCompile(`(?m)^#+ (.+)$`).FindAllStringSubmatch(read(t, serverGuide), -1) {
		a := drop.ReplaceAllString(strings.ToLower(strings.ReplaceAll(m[1], "`", "")), "")
		anchors[strings.ReplaceAll(a, " ", "-")] = true
	}
	return anchors
}

// TestInstallServerStepsMatchGuide keeps the installer's steps and the
// table in docs/server.md that maps each to the sections it follows the
// same list, in the same order, and holds the table to the guide: every
// section it names exists, and every section of the setup is named.
func TestInstallServerStepsMatchGuide(t *testing.T) {
	t.Parallel()
	got := scriptSteps(t)
	want, anchors := guideTable(t)
	if len(got) == 0 {
		t.Fatalf("%s has no `step N 'TITLE'` line", installServer)
	}
	for i, s := range got {
		if s.n != fmt.Sprint(i+1) {
			t.Errorf("%s's steps are numbered %v; number them 1 to %d in order", installServer, got, len(got))
			break
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("%s's steps are\n  %v\n%s's \"With the installer\" table lists\n  %v\nchange both together", installServer, got, serverGuide, want)
	}

	exists := guideAnchors(t)
	named := map[string]bool{}
	for i, as := range anchors {
		if len(as) == 0 {
			t.Errorf("%s's \"With the installer\" table: step %s links to no section of the guide", serverGuide, want[i].n)
		}
		for _, a := range as {
			named[a] = true
			if !exists[a] {
				t.Errorf("%s's \"With the installer\" table: step %s links to #%s, which is no heading of the guide", serverGuide, want[i].n, a)
			}
		}
	}
	// Every step of "Set up a server", and the two sections that make it a
	// server of many projects, is what some step of the installer does.
	setup := []string{"opt-in", "keys-from-the-store"}
	for _, m := range regexp.MustCompile(`(?m)^### ([0-9]+\. .+)$`).FindAllStringSubmatch(guideSection(t, "## Set up a server"), -1) {
		setup = append(setup, regexp.MustCompile(`[^a-z0-9 -]`).ReplaceAllString(strings.ReplaceAll(strings.ToLower(m[1]), " ", "-"), ""))
	}
	if len(setup) < 4 {
		t.Fatalf("%s's \"Set up a server\" has no numbered steps (### N. TITLE)", serverGuide)
	}
	for _, a := range setup {
		if !named[a] {
			t.Errorf("%s's section #%s is not linked from its \"With the installer\" table: make %s do it, and name it there", serverGuide, a, installServer)
		}
	}
}

// scriptFiles returns the files the installer writes whose text it
// holds, by path: each function after a `# file: PATH` line that prints
// a here-document.
func scriptFiles(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, m := range regexp.MustCompile("(?s)# file: (\\S+)\n\\w+\\(\\) \\{\n\tcat <<'EOF'\n(.*?\n)EOF\n\\}").FindAllStringSubmatch(read(t, installServer), -1) {
		files[m[1]] = m[2]
	}
	return files
}

// match returns the first submatch of pattern in s, or fails the test.
func match(t *testing.T, s, what, pattern string) string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("%s: cannot find %s (%s)", serverGuide, what, pattern)
	}
	return m[1]
}

// guideFiles returns the text docs/server.md gives each file the
// installer writes, as the guide's commands would write it.
func guideFiles(t *testing.T) map[string]string {
	t.Helper()
	g := read(t, serverGuide)
	saved := func(path, lang string) string {
		return match(t, g, path, "(?s)Save this as\\s+`"+regexp.QuoteMeta(path)+"`:\\s+```"+lang+"\n(.*?\n)```")
	}
	printed := func(path string) string {
		return strings.ReplaceAll(match(t, g, path, `printf '([^']*)' > `+regexp.QuoteMeta(path)+"\n"), `\n`, "\n")
	}
	// sshd's drop-in is step 6's, with the line of "Keys from the store"
	// that names the keys file in its Match block.
	step6 := saved("/etc/ssh/sshd_config.d/00-aries.conf", "text")
	global, _, ok := strings.Cut(step6, "Match User aries\n")
	if !ok {
		t.Fatalf("%s: step 6's sshd drop-in has no Match User aries", serverGuide)
	}
	block := match(t, g, "Keys from the store's Match block", "(?s)\n   ```text\n(   Match User aries\n.*?\n)   ```\n")
	block = regexp.MustCompile(`(?m)^   `).ReplaceAllString(block, "")
	return map[string]string{
		"/etc/systemd/system/ariesd.service":                 saved("/etc/systemd/system/ariesd.service", "ini"),
		"/etc/systemd/system/ariesd.service.d/keys.conf":     printed("/etc/systemd/system/ariesd.service.d/keys.conf"),
		"/etc/systemd/system/ariesd.service.d/apparmor.conf": printed("/etc/systemd/system/ariesd.service.d/apparmor.conf"),
		"/etc/sudoers.d/aries":                               match(t, g, "the sudo rule", `echo '([^']*)' > /etc/sudoers\.d/aries`) + "\n",
		"/etc/ssh/sshd_config.d/00-aries.conf":               global + block,
	}
}

// TestInstallServerFilesMatchGuide holds each file the installer writes
// to the text docs/server.md gives it, byte for byte.
func TestInstallServerFilesMatchGuide(t *testing.T) {
	t.Parallel()
	got, want := scriptFiles(t), guideFiles(t)
	if paths := slices.Sorted(maps.Keys(got)); !slices.Equal(paths, slices.Sorted(maps.Keys(want))) {
		t.Errorf("%s holds the text of\n  %s\nthe test compares\n  %s\nwith %s; a file the installer writes needs its text from the guide",
			installServer, strings.Join(paths, "\n  "), strings.Join(slices.Sorted(maps.Keys(want)), "\n  "), serverGuide)
	}
	for _, path := range slices.Sorted(maps.Keys(want)) {
		if g, ok := got[path]; ok && g != want[path] {
			t.Errorf("%s writes %s as\n%s\n%s gives\n%s\nchange both together", installServer, path, g, serverGuide, want[path])
		}
	}
}

// testKey is an invented public key, for a test file only.
const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOyGBeIkKjClj7gv3HsjrdMuX6oyXTezqG45dBodBNvL alice@example.com\n"

// runInstallServer runs the installer with args, in a directory that
// holds key files the arguments may name, and returns its exit code and
// what it printed to stderr.
func runInstallServer(t *testing.T, args ...string) (int, string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"alice.pub": testKey,
		"two.pub":   testKey + testKey,
		"alice":     "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----\n",
		"junk.pub":  "not a key\n",
		"db.pass":   "an invented password\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script, err := filepath.Abs(filepath.Join("..", installServer))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", append([]string{script}, args...)...) //nolint:gosec // the repository's script
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return exit.ExitCode(), stderr.String()
	}
	if err != nil {
		t.Fatalf("sh %s %s: %v", installServer, strings.Join(args, " "), err)
	}
	return 0, stderr.String()
}

// TestInstallServerOptions checks the refusals that come before the
// installer looks at the machine, so they hold on any system: each exits
// 2 and says what to do instead.
func TestInstallServerOptions(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("install-server.sh is for Linux")
	}
	ok := []string{"--admin", "alice", "--admin-key", "alice.pub"}
	remote := append(slices.Clone(ok), "--dsn", "postgres://aries@db.example.com/aries?sslmode=verify-full")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no admin", []string{"--admin-key", "alice.pub"}, "--admin is required"},
		{"no key", []string{"--admin", "alice"}, "--admin-key is required"},
		{"bad admin", []string{"--admin", "Alice", "--admin-key", "alice.pub"}, "is not a principal name"},
		{"reserved admin", []string{"--admin", "import", "--admin-key", "alice.pub"}, "a name the server uses itself"},
		{"missing key file", []string{"--admin", "alice", "--admin-key", "nobody.pub"}, "cannot read --admin-key"},
		{"private key", []string{"--admin", "alice", "--admin-key", "alice"}, "is a private key"},
		{"two keys", []string{"--admin", "alice", "--admin-key", "two.pub"}, "more than one key"},
		{"not a key", []string{"--admin", "alice", "--admin-key", "junk.pub"}, "is not an SSH public key line"},
		{"bad project", append(slices.Clone(ok), "--project", "Acme Corp"), "is not a code name"},
		{"bad host", append(slices.Clone(ok), "--host", "a b"), "is not a host name"},
		{"bad port", append(slices.Clone(ok), "--port", "70000"), "is not a port"},
		{"bad version", append(slices.Clone(ok), "--version", "latest"), "is not a release version"},
		{"bad releases", append(slices.Clone(ok), "--releases", "ftp://example.com"), "--releases is not"},
		{"unknown option", append(slices.Clone(ok), "--yes"), "unknown option --yes"},
		{"flag with a value", append(slices.Clone(ok), "--dry-run=no"), "unknown option --dry-run=no"},
		{"no value", []string{"--admin"}, "--admin needs a value"},
		{"dsn not postgres", append(slices.Clone(ok), "--dsn", "mysql://aries@db.example.com/aries"), "--dsn is not a postgres:// URL"},
		{"dsn with a password", append(slices.Clone(ok), "--dsn", "postgres://aries:hunter2@db.example.com/aries"), "--dsn holds a password"},
		{"dsn with a password parameter", append(slices.Clone(ok), "--dsn", "postgres://aries@db.example.com/aries?password=hunter2"), "--dsn holds a password"},
		{"dsn with a space", append(slices.Clone(ok), "--dsn", "postgres://aries@db.example.com/aries?application_name=a b"), "--dsn holds a space or a quote"},
		{"password file without dsn", append(slices.Clone(ok), "--db-password-file", "db.pass"), "--db-password-file is for the Postgres --dsn names"},
		{"missing password file", append(slices.Clone(remote), "--db-password-file", "nobody.pass"), "cannot read --db-password-file"},
		{"password file of two lines", append(slices.Clone(remote), "--db-password-file", "two.pub"), "holds more than one line"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, stderr := runInstallServer(t, tc.args...)
			if code != 2 || !strings.Contains(stderr, tc.want) || !strings.Contains(stderr, "\nfix: ") {
				t.Errorf("install-server.sh %s = exit %d, stderr:\n%s\nwant exit 2, %q and a fix: line", strings.Join(tc.args, " "), code, stderr, tc.want)
			}
			// A refused --dsn may hold a password: it is never repeated.
			if strings.Contains(stderr, "hunter2") {
				t.Errorf("install-server.sh %s printed the password it was given:\n%s", strings.Join(tc.args, " "), stderr)
			}
		})
	}
	if code, stderr := runInstallServer(t, "-h"); code != 0 || stderr != "" {
		t.Errorf("install-server.sh -h = exit %d, stderr %q; want exit 0 and nothing on stderr", code, stderr)
	}
}

// scriptUbuntuVersions returns the Ubuntu releases the installer accepts,
// from its ubuntu_versions line.
func scriptUbuntuVersions(t *testing.T) []string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^ubuntu_versions='([0-9. ]+)'$`).FindStringSubmatch(read(t, installServer))
	if m == nil {
		t.Fatalf("%s has no ubuntu_versions='VERSION ...' line", installServer)
	}
	return strings.Fields(m[1])
}

// ubuntuReleases names versions as the guide and the installer word them,
// joining the last with conj: "Ubuntu 24.04 LTS", or "Ubuntu 24.04 LTS
// and 26.04 LTS".
func ubuntuReleases(versions []string, conj string) string {
	lts := make([]string, len(versions))
	for i, v := range versions {
		lts[i] = v + " LTS"
	}
	if len(lts) == 1 {
		return "Ubuntu " + lts[0]
	}
	return "Ubuntu " + strings.Join(lts[:len(lts)-1], ", ") + " " + conj + " " + lts[len(lts)-1]
}

// TestInstallServerVersionsMatchGuide keeps the Ubuntu releases the
// installer accepts the ones docs/server.md says are supported, and the
// ones its table of the installer's steps says step 1 checks for.
func TestInstallServerVersionsMatchGuide(t *testing.T) {
	t.Parallel()
	versions := scriptUbuntuVersions(t)
	if want := "**" + ubuntuReleases(versions, "and") + " are the supported platforms.**"; len(versions) > 1 &&
		!strings.Contains(guideSection(t, "## What a server needs"), want) {
		t.Errorf("%s accepts %s, and %s's \"What a server needs\" does not say %q; change both together",
			installServer, strings.Join(versions, " "), serverGuide, want)
	}
	steps, _ := guideTable(t)
	if len(steps) == 0 {
		t.Fatalf("%s's \"With the installer\" has no table of steps", serverGuide)
	}
	want := "Ubuntu " + strings.Join(versions, " or ")
	if row := "| 1 | " + steps[0].title + " | [What a server needs](#what-a-server-needs): " + want + ","; !strings.Contains(read(t, serverGuide), row) {
		t.Errorf("%s accepts %s, and step 1 of %s's \"With the installer\" table does not say %q; change both together",
			installServer, strings.Join(versions, " "), serverGuide, want)
	}
}

// TestInstallServerRefusesMachine checks that, with good options, the
// installer refuses before it changes anything when it is not root on a
// release it supports: off those for the system, and on one for want of
// root.
func TestInstallServerRefusesMachine(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("install-server.sh is for Linux")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root, where the installer would go on to set the machine up")
	}
	versions := scriptUbuntuVersions(t)
	want := "this installer runs only on " + ubuntuReleases(versions, "or")
	if b, err := os.ReadFile("/etc/os-release"); err == nil && regexp.MustCompile(`(?m)^ID=ubuntu$`).Match(b) {
		if m := regexp.MustCompile(`(?m)^VERSION_ID="?([0-9.]+)"?$`).FindSubmatch(b); m != nil && slices.Contains(versions, string(m[1])) {
			want = "this installer must run as root"
		}
	}
	code, stderr := runInstallServer(t, "--admin", "alice", "--admin-key", "alice.pub", "--dry-run")
	if code != 1 || !strings.Contains(stderr, want) || !strings.Contains(stderr, "\nfix: ") {
		t.Errorf("install-server.sh, not as root on %s = exit %d, stderr:\n%s\nwant exit 1, %q and a fix: line", runtime.GOOS, code, stderr, want)
	}
}

// TestInstallServerSQLIsTheGuides holds every statement the installer
// runs on Postgres to step 2 of docs/server.md, which an operator runs by
// hand, or has run on a managed service: each is a line of that step's
// block, so the installer makes no role or database the guide does not.
func TestInstallServerSQLIsTheGuides(t *testing.T) {
	t.Parallel()
	stmts := regexp.MustCompile(`(?m)^sql_[a-z]+="((?:CREATE|REVOKE|GRANT|ALTER) [^"]+;)"$`).FindAllStringSubmatch(read(t, installServer), -1)
	if len(stmts) < 3 {
		t.Fatalf(`%s has %d sql_NAME="STATEMENT;" lines; want the role, the database and the revoke`, installServer, len(stmts))
	}
	step := guideSection(t, "### 2. Create the role and the database")
	block := match(t, step, "step 2's SQL", "(?s)psql[^\n]*<<'EOF'\n(.*?\n)EOF\n")
	lines := strings.Split(block, "\n")
	for _, m := range stmts {
		if !slices.Contains(lines, m[1]) {
			t.Errorf("%s runs %q, which is no line of step 2's block in %s:\n%s\nchange both together", installServer, m[1], serverGuide, block)
		}
	}
}

// TestOperationsPostgresPins keeps the Postgres the installer installs,
// the oldest it accepts and the key it trusts PGDG's packages by at
// what the code and CI say: version.PostgresMajor, version.PostgresMin
// and ci.yml's PGDG_KEY_FINGERPRINT. And it keeps the Docker test
// server's Postgres, and the package the guide installs, at the same
// major version.
func TestOperationsPostgresPins(t *testing.T) {
	t.Parallel()
	script := read(t, installServer)
	fingerprint := regexp.MustCompile(`(?m)^\s*PGDG_KEY_FINGERPRINT:\s*"?([0-9A-F]{40})"?\s*$`).FindStringSubmatch(read(t, ".github/workflows/ci.yml"))
	if fingerprint == nil {
		t.Fatal("ci.yml has no PGDG_KEY_FINGERPRINT: line")
	}
	for _, c := range []struct{ what, pattern, want string }{
		{"pg_major", `(?m)^pg_major=([0-9]+)$`, version.PostgresMajor},
		{"pg_min", `(?m)^pg_min=([0-9]+)$`, strconv.Itoa(version.PostgresMin)},
		{"pgdg_key_fingerprint", `(?m)^pgdg_key_fingerprint=([0-9A-F]{40})$`, fingerprint[1]},
	} {
		m := regexp.MustCompile(c.pattern).FindStringSubmatch(script)
		switch {
		case m == nil:
			t.Errorf("%s has no line matching %s", installServer, c.pattern)
		case m[1] != c.want:
			t.Errorf("%s has %s=%s, want %s (internal/version and ci.yml); change them together", installServer, c.what, m[1], c.want)
		}
	}
	const compose = "deploy/docker/compose.yaml"
	if m := regexp.MustCompile(`(?m)^ *# postgres:([0-9]+)\.[0-9]+[^\n]*\n *image: postgres@sha256:[0-9a-f]{64}$`).FindStringSubmatch(read(t, compose)); m == nil {
		t.Errorf("%s has no postgres image pinned by digest under a comment naming its release (# postgres:MAJOR.MINOR)", compose)
	} else if m[1] != version.PostgresMajor {
		t.Errorf("%s runs Postgres %s, version.PostgresMajor is %s; change them together", compose, m[1], version.PostgresMajor)
	}
	if want := "apt-get install -y postgresql-" + version.PostgresMajor + "\n"; !strings.Contains(guideSection(t, "### 1. Install Postgres"), want) {
		t.Errorf("%s's step 1 does not run %q", serverGuide, strings.TrimSpace(want))
	}
}

// TestOperationsNameNoDolt keeps the database Aries left (decision 79)
// out of what sets a server up and runs it: the installer, the Docker
// test server, the AppArmor profiles, the operations skill and the
// guides for operators.
func TestOperationsNameNoDolt(t *testing.T) {
	t.Parallel()
	files := []string{installServer, serverGuide, "docs/security-model.md", "RELEASING.md"}
	for _, dir := range []string{"deploy/docker", "deploy/apparmor", "docs/deploy", "plugins/aries-ops"} {
		err := filepath.WalkDir(filepath.Join("..", filepath.FromSlash(dir)), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, err := filepath.Rel("..", path)
			files = append(files, filepath.ToSlash(rel))
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	dolt := regexp.MustCompile(`(?i)dolt`)
	for _, f := range files {
		for i, line := range strings.Split(read(t, f), "\n") {
			if dolt.MatchString(line) {
				t.Errorf("%s:%d names Dolt, which Aries no longer uses: %s", f, i+1, strings.TrimSpace(line))
				break
			}
		}
	}
}
