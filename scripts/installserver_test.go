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
		"/etc/systemd/system/ariesd.service.d/https.conf":    printed("/etc/systemd/system/ariesd.service.d/https.conf"),
		"/etc/systemd/system/ariesd.service.d/acme.conf":     printed("/etc/systemd/system/ariesd.service.d/acme.conf"),
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
		// Not a working pair: the options' checks read only their PEM
		// armor.
		"cert.pem": "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n",
		"key.pem":  "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n",
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
		{"unknown profile", append(slices.Clone(ok), "--profile", "cloud"), "--profile cloud is not internet, private, airgapped or ssh-only"},
		{"HTTPS's option with ssh-only", append(slices.Clone(ok), "--profile", "ssh-only", "--url", "https://aries.example.com"), "--profile ssh-only serves none"},
		{"unknown TLS mode", append(slices.Clone(ok), "--tls", "selfsigned"), "--tls selfsigned is not acme, files or proxy"},
		{"files without a key", append(slices.Clone(ok), "--profile", "private", "--cert", "cert.pem"), "--tls files needs --cert and --key"},
		{"a cert that is a key", append(slices.Clone(ok), "--profile", "private", "--cert", "key.pem", "--key", "key.pem"), "is not a certificate in PEM"},
		{"a key that is a cert", append(slices.Clone(ok), "--profile", "private", "--cert", "cert.pem", "--key", "cert.pem"), "is not a private key in PEM"},
		{"a cert with ACME", append(slices.Clone(ok), "--cert", "cert.pem", "--key", "key.pem"), "--cert and --key are for --tls files"},
		{"ACME's email with files", append(slices.Clone(ok), "--profile", "private", "--cert", "cert.pem", "--key", "key.pem", "--acme-email", "ops@example.com"), "are for --tls acme"},
		{"trusted without a proxy", append(slices.Clone(ok), "--trusted", "10.0.0.5"), "--trusted is for --tls proxy"},
		{"airgapped with Let's Encrypt", append(slices.Clone(ok), "--profile", "airgapped", "--tls", "acme"), "reaches nothing outside"},
		{"bad ACME email", append(slices.Clone(ok), "--acme-email", "ops at example"), "--acme-email ops at example is not an address"},
		{"ACME directory not https", append(slices.Clone(ok), "--acme-directory", "http://ca.example.com/directory"), "is not an https:// URL"},
		{"bad trusted", append(slices.Clone(ok), "--tls", "proxy", "--trusted", "proxy.example.com"), "--trusted proxy.example.com is not an address"},
		{"bad listen", append(slices.Clone(ok), "--listen", "443"), "--listen 443 is not HOST:PORT"},
		{"bad url", append(slices.Clone(ok), "--url", "http://aries.example.com"), "is not an https:// URL of the server"},
		{"sshd's port with SSH off", append(slices.Clone(ok), "--port", "2222"), "--port is sshd's, and SSH is off"},
		{"SSH on and off at once", append(slices.Clone(ok), "--enable-ssh", "--disable-ssh"), "--enable-ssh and --disable-ssh"},
		{"SSH off with SSH alone", append(slices.Clone(ok), "--profile", "ssh-only", "--disable-ssh"), "--profile ssh-only serves SSH alone"},
		{"sshd's port while turning SSH off", append(slices.Clone(ok), "--disable-ssh", "--port", "2222"), "--port is sshd's, and SSH is off"},
		{"an IPv6 host without a URL", append(slices.Clone(ok), "--host", "2001:db8::5"), "needs in brackets"},
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

// plan runs the installer with --plan and args, and returns what it
// printed, failing the test unless it exits 0.
func plan(t *testing.T, args ...string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"alice.pub": testKey,
		"cert.pem": "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n",
		"key.pem":  "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script, err := filepath.Abs(filepath.Join("..", installServer))
	if err != nil {
		t.Fatal(err)
	}
	args = append([]string{script, "--admin", "alice", "--admin-key", "alice.pub", "--host", "aries.example.com", "--plan"}, args...)
	cmd := exec.Command("sh", args...) //nolint:gosec // the repository's script
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("install-server.sh %s: %v\n%s", strings.Join(args[1:], " "), err, stderr.String())
	}
	return stdout.String()
}

// TestInstallServerProfiles holds each profile to what transport.md §3
// and §11.1 give it, by its plan, which comes from the options alone. A
// default install serves HTTPS and no SSH: no sshd drop-in, no
// authorized_keys, no keys file and no keys drop-in, and the first
// admin's key in a file no sshd reads (§12.4, item 10). SSH's files
// come with --enable-ssh, or alone with ssh-only.
func TestInstallServerProfiles(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("install-server.sh is for Linux")
	}
	sshFiles := []string{"sshd_config.d/00-aries.conf", ".ssh/authorized_keys", ".ssh/aries_keys", "keys.conf"}
	tests := []struct {
		name    string
		args    []string
		want    []string // lines of the plan
		without []string // what the plan must not name
	}{
		{name: "the default", args: nil,
			want: []string{"profile: internet", "transports: [https]", "https: https://aries.example.com, listening on :443, TLS by acme",
				"  /var/lib/aries/acme/", "  /etc/systemd/system/ariesd.service.d/https.conf", "  /etc/systemd/system/ariesd.service.d/acme.conf",
				"  /etc/aries/admin.keys", "opens, if ufw is active: 443/tcp"},
			without: sshFiles},
		{name: "internet with SSH", args: []string{"--enable-ssh"},
			want: []string{"transports: [https, ssh]", "  /var/lib/aries/.ssh/authorized_keys", "  /etc/ssh/sshd_config.d/00-aries.conf",
				"  /etc/systemd/system/ariesd.service.d/keys.conf", "  (/var/lib/aries/.ssh/aries_keys)"},
			without: []string{"admin.keys"}},
		{name: "private, the company CA's files", args: []string{"--profile", "private", "--cert", "cert.pem", "--key", "key.pem"},
			want:    []string{"profile: private", "transports: [https]", "https: https://aries.example.com, listening on :443, TLS by files", "  /etc/aries/tls/cert.pem", "  /etc/aries/tls/key.pem"},
			without: append([]string{"acme"}, sshFiles...)},
		{name: "private, behind a proxy", args: []string{"--profile", "private", "--tls", "proxy", "--trusted", "10.0.0.5,10.0.1.0/24"},
			want:    []string{"https: https://aries.example.com, listening on 127.0.0.1:8443, TLS by proxy"},
			without: append([]string{"https.conf", "acme", "ufw", "/etc/aries/tls"}, sshFiles...)},
		{name: "airgapped", args: []string{"--profile", "airgapped", "--cert", "cert.pem", "--key", "key.pem", "--listen", ":8443"},
			want:    []string{"profile: airgapped", "https: https://aries.example.com:8443, listening on :8443, TLS by files", "opens, if ufw is active: 8443/tcp"},
			without: append([]string{"https.conf", "acme"}, sshFiles...)},
		{name: "airgapped with an internal ACME server", args: []string{"--profile", "airgapped", "--tls", "acme", "--acme-directory", "https://ca.example.com/acme/directory"},
			want: []string{"https: https://aries.example.com, listening on :443, TLS by acme", "  /etc/systemd/system/ariesd.service.d/acme.conf"}, without: sshFiles},
		{name: "SSH alone", args: []string{"--profile", "ssh-only"},
			want:    []string{"profile: ssh-only", "transports: [ssh]", "  /etc/ssh/sshd_config.d/00-aries.conf", "  /var/lib/aries/.ssh/authorized_keys"},
			without: []string{"https", "ufw", "admin.keys", "acme", "/etc/aries/tls"}},
		// The bridge's AppArmor profile is SSH's: with SSH off nothing runs
		// as sshd's forced command, and the operator's rules, ariesd-default,
		// take the binary's path instead.
		{name: "AppArmor without SSH", args: []string{"--apparmor"},
			want: []string{"  /etc/apparmor.d/ariesd", "  /etc/apparmor.d/ariesd-default", "  /etc/apparmor.d/abstractions/ariesd-admin",
				"  /etc/apparmor.d/tunables/ariesd", "  /etc/systemd/system/ariesd.service.d/apparmor.conf"},
			without: append([]string{"ariesd-stdio"}, sshFiles...)},
		{name: "AppArmor with SSH", args: []string{"--apparmor", "--enable-ssh"},
			want:    []string{"  /etc/apparmor.d/ariesd", "  /etc/apparmor.d/ariesd-stdio", "  /etc/apparmor.d/abstractions/ariesd-admin"},
			without: []string{"ariesd-default"}},
		// --disable-ssh writes what SSH off writes, and removes what SSH on
		// wrote: every SSH path.
		{name: "turning SSH off", args: []string{"--disable-ssh", "--apparmor"},
			want: []string{"transports: [https]", "  /etc/aries/admin.keys", "removes, where they are:", "  /etc/ssh/sshd_config.d/00-aries.conf",
				"  /var/lib/aries/.ssh/authorized_keys", "  /var/lib/aries/.ssh/aries_keys", "  /var/lib/aries/.ssh/", "  /etc/systemd/system/ariesd.service.d/keys.conf",
				"  /etc/apparmor.d/ariesd-stdio", "  /etc/apparmor.d/ariesd-default"},
			without: append([]string{"ariesd-stdio"}, sshFiles...)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := plan(t, tc.args...)
			lines := strings.Split(out, "\n")
			for _, w := range tc.want {
				if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, w) }) {
					t.Errorf("the plan of %s lacks %q:\n%s", tc.name, w, out)
				}
			}
			// What the plan removes may name SSH's files; what it writes and
			// serves may not.
			writes, _, _ := strings.Cut(out, "removes, where they are:")
			for _, w := range tc.without {
				if strings.Contains(writes, w) {
					t.Errorf("the plan of %s writes or serves %q, which it must not:\n%s", tc.name, w, out)
				}
			}
			if removes := strings.Contains(out, "removes"); removes != slices.Contains(tc.args, "--disable-ssh") {
				t.Errorf("the plan of %s removes files: %t; want that only of --disable-ssh:\n%s", tc.name, removes, out)
			}
		})
	}
}

// TestInstallServerDisableSSHMatchesGuide holds what `--disable-ssh`
// removes to the list docs/server.md's "SSH on and off" gives, in the
// same order: every SSH path a server with SSH on has.
func TestInstallServerDisableSSHMatchesGuide(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("install-server.sh is for Linux")
	}
	_, removes, ok := strings.Cut(plan(t, "--disable-ssh", "--apparmor"), "removes, where they are:\n")
	if !ok {
		t.Fatal("the plan of --disable-ssh has no list of what it removes")
	}
	var got []string
	for line := range strings.Lines(removes) {
		if p, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "  "); ok {
			got = append(got, strings.Fields(p)[0])
		}
	}
	list := match(t, guideSection(t, "### SSH on and off"), "the list of what --disable-ssh removes",
		"(?s)`install-server.sh --disable-ssh` removes, each only where it is[^\n]*\n\n(.*?)\n\n")
	var want []string
	for _, m := range regexp.MustCompile("(?m)^- `([^`]+)`").FindAllStringSubmatch(list, -1) {
		want = append(want, m[1])
	}
	if !slices.Equal(got, want) {
		t.Errorf("install-server.sh --disable-ssh removes\n  %s\n%s's \"SSH on and off\" lists\n  %s\nchange both together",
			strings.Join(got, "\n  "), serverGuide, strings.Join(want, "\n  "))
	}
}

// TestAppArmorProfilesInstalledAndParsed holds the AppArmor profiles to
// what installs and checks them: every file of deploy/apparmor but its
// README, tunables and abstractions is one the installer fetches, the
// guide's commands install and CI's parser checks, in each of its jobs;
// the bridge's is SSH's alone, and the default profile, which takes
// the binary's path without SSH, is SSH off's.
func TestAppArmorProfilesInstalledAndParsed(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(filepath.Join("..", "deploy", "apparmor"))
	if err != nil {
		t.Fatal(err)
	}
	var profiles []string
	for _, e := range entries {
		if !e.IsDir() && e.Name() != "README.md" {
			profiles = append(profiles, e.Name())
		}
	}
	if !slices.Equal(profiles, []string{"ariesd", "ariesd-default", "ariesd-stdio"}) {
		t.Errorf("deploy/apparmor holds the profile files %v; want ariesd, ariesd-default and ariesd-stdio, or change this test with what installs them", profiles)
	}
	script, ci, section := read(t, installServer), read(t, ".github/workflows/ci.yml"), guideSection(t, "## Confine ariesd with AppArmor")
	parse := "apparmor_parser -Q -K -I deploy/apparmor deploy/apparmor/" + strings.Join(profiles, " deploy/apparmor/") + "\n"
	if n := strings.Count(ci, parse); n != 2 {
		t.Errorf("ci.yml runs %q in %d jobs; want both, lint's and apparmor-26-04's", strings.TrimSpace(parse), n)
	}
	for _, p := range profiles {
		if !strings.Contains(script, "fetch deploy/apparmor/"+p+" ") {
			t.Errorf("%s does not fetch deploy/apparmor/%s", installServer, p)
		}
		if !strings.Contains(section, "deploy/apparmor/"+p+"\n") || !strings.Contains(section, "/etc/apparmor.d/"+p+"\n") {
			t.Errorf("%s's \"Confine ariesd with AppArmor\" does not download deploy/apparmor/%s and install it as /etc/apparmor.d/%s", serverGuide, p, p)
		}
	}
	for _, only := range []string{"Only with SSH on", "Only with SSH off"} {
		if !strings.Contains(section, only) {
			t.Errorf("%s's \"Confine ariesd with AppArmor\" does not say %q: the bridge's profile is SSH on's, the default profile SSH off's", serverGuide, only)
		}
	}
	if !strings.Contains(section, "deploy/apparmor/abstractions/ariesd-admin\n") || !strings.Contains(section, "/etc/apparmor.d/abstractions/ariesd-admin\n") {
		t.Errorf("%s's \"Confine ariesd with AppArmor\" does not download deploy/apparmor/abstractions/ariesd-admin and install it as /etc/apparmor.d/abstractions/ariesd-admin", serverGuide)
	}
	if !strings.Contains(script, "fetch deploy/apparmor/abstractions/ariesd-admin ") {
		t.Errorf("%s does not fetch deploy/apparmor/abstractions/ariesd-admin", installServer)
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
	if m := regexp.MustCompile(`(?m)^ *image: postgres:([0-9]+)\.[0-9]+@sha256:[0-9a-f]{64}$`).FindStringSubmatch(read(t, compose)); m == nil {
		t.Errorf("%s has no postgres image pinned by release and digest (postgres:MAJOR.MINOR@sha256:…)", compose)
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
