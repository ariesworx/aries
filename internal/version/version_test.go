package version

import (
	"bufio"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// setting returns the first submatch of pattern in the file at path.
func setting(t *testing.T, path, pattern string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // a file of this repository, named by the test
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(pattern).FindSubmatch(b)
	if m == nil {
		t.Fatalf("%s matches no %s", path, pattern)
	}
	return string(m[1])
}

const (
	ciFile           = "../../.github/workflows/ci.yml"
	contributingFile = "../../CONTRIBUTING.md"
	goModFile        = "../../go.mod"
	miseFile         = "../../mise.toml"
	sumsFile         = "postgres.sha256"
	releaseFile      = "../../.github/workflows/release.yml"
)

// TestPostgresMajor keeps PostgresMajor the major version of the pinned
// release, and at least the oldest the store opens.
func TestPostgresMajor(t *testing.T) {
	t.Parallel()
	if major, _, _ := strings.Cut(Postgres, "."); major != PostgresMajor {
		t.Errorf("version.Postgres is %s, version.PostgresMajor is %s; change both together", Postgres, PostgresMajor)
	}
	if n, err := strconv.Atoi(PostgresMajor); err != nil || n < PostgresMin {
		t.Errorf("version.PostgresMajor is %s, which is not a version at least version.PostgresMin %d", PostgresMajor, PostgresMin)
	}
}

// TestPostgresMatchesCI keeps the major version CI installs from the
// PostgreSQL project's apt repository at the pin's.
func TestPostgresMatchesCI(t *testing.T) {
	t.Parallel()
	if got := setting(t, ciFile, `(?m)^\s*POSTGRES_MAJOR:\s*"?(\d+)"?\s*$`); got != PostgresMajor {
		t.Errorf("ci.yml POSTGRES_MAJOR is %s, version.PostgresMajor is %s; change both together", got, PostgresMajor)
	}
}

// TestPostgresMatchesContributing keeps the release CONTRIBUTING.md says
// scripts/install-postgres.sh builds at the pin.
func TestPostgresMatchesContributing(t *testing.T) {
	t.Parallel()
	if got := setting(t, contributingFile, `install-postgres\.sh`+"`"+` (?:builds|installs) Postgres ([0-9.]+)`); got != Postgres {
		t.Errorf("CONTRIBUTING.md says scripts/install-postgres.sh builds Postgres %s, version.Postgres is %s; change both together", got, Postgres)
	}
}

// TestReleaseNotesNameThePin keeps the release's notes saying which
// Postgres it was tested with: the release workflow reads the pin from
// version.go with sed, and fails the release if it finds none.
func TestReleaseNotesNameThePin(t *testing.T) {
	t.Parallel()
	got := setting(t, releaseFile, `(?m)^\s*postgres="\$\(sed -n '(s/\^const Postgres = .*/p)' internal/version/version\.go\)"$`)
	sed := regexp.MustCompile(`^s/\^const Postgres = "\\\(\.\*\\\)"\$/\\1/p$`)
	if !sed.MatchString(got) {
		t.Fatalf("release.yml reads the pin with sed %q, want s/^const Postgres = \"\\(.*\\)\"$/\\1/p", got)
	}
	if setting(t, "version.go", `(?m)^const Postgres = "(.*)"$`) != Postgres {
		t.Errorf("version.go has no line const Postgres = %q for release.yml to read", Postgres)
	}
}

// TestPostgresChecksum keeps postgres.sha256, which
// scripts/install-postgres.sh takes the release from and verifies a
// download against, at the pinned release: one source archive, of
// version.Postgres.
func TestPostgresChecksum(t *testing.T) {
	t.Parallel()
	f, err := os.Open(sumsFile)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	want := "v" + Postgres + "/postgresql-" + Postgres + ".tar.gz"
	line := regexp.MustCompile(`^([0-9a-f]{64})  (\S+)$`)
	var names []string
	for sc := bufio.NewScanner(f); sc.Scan(); {
		if sc.Text() == "" || strings.HasPrefix(sc.Text(), "#") {
			continue
		}
		m := line.FindStringSubmatch(sc.Text())
		if m == nil {
			t.Fatalf("%s: line %q is not a lowercase SHA-256, two spaces and a name", sumsFile, sc.Text())
		}
		names = append(names, m[2])
	}
	if len(names) != 1 || names[0] != want {
		t.Errorf("%s pins %q, want only %s, the source archive of version.Postgres; RELEASING.md says how to change the pin", sumsFile, names, want)
	}
}

// TestMiseMatchesGoMod keeps the Go that mise installs at go.mod's go
// line, which is also what CI's setup-go reads.
func TestMiseMatchesGoMod(t *testing.T) {
	t.Parallel()
	want := setting(t, goModFile, `(?m)^go (\S+)$`)
	if got := setting(t, miseFile, `(?m)^go = "([^"]+)"$`); got != want {
		t.Errorf("mise.toml go is %s, go.mod's go line is %s; change both together", got, want)
	}
}

// TestMiseMatchesCI keeps the golangci-lint that mise installs at the
// version CI's lint job pins.
func TestMiseMatchesCI(t *testing.T) {
	t.Parallel()
	want := setting(t, ciFile, `(?m)^\s*- uses: golangci/golangci-lint-action@.*\n\s*with:\n\s*version: v(\S+)$`)
	if got := setting(t, miseFile, `(?m)^golangci-lint = "([^"]+)"$`); got != want {
		t.Errorf("mise.toml golangci-lint is %s, ci.yml pins v%s; change both together", got, want)
	}
}
