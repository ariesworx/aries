package version

import (
	"bufio"
	"os"
	"regexp"
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
	ciFile     = "../../.github/workflows/ci.yml"
	dockerFile = "../../deploy/docker/Dockerfile"
	goModFile  = "../../go.mod"
	guideFile  = "../../docs/server.md"
	miseFile   = "../../mise.toml"
	sumsFile   = "dolt.sha256"
)

// TestDoltMatchesCI keeps the pin and the version CI tests against equal.
func TestDoltMatchesCI(t *testing.T) {
	if got := setting(t, ciFile, `(?m)^\s*DOLT_VERSION:\s*(\S+)\s*$`); got != Dolt {
		t.Errorf("ci.yml DOLT_VERSION is %s, version.Dolt is %s; change both together", got, Dolt)
	}
}

// TestDoltMatchesDocker keeps the Docker test server's Dolt at the pin.
func TestDoltMatchesDocker(t *testing.T) {
	if got := setting(t, dockerFile, `(?m)^ARG DOLT_VERSION=(\S+)$`); got != Dolt {
		t.Errorf("Dockerfile DOLT_VERSION is %s, version.Dolt is %s; change both together", got, Dolt)
	}
}

// TestDoltMatchesServerGuide keeps the release the server guide tells an
// operator to download at the pin.
func TestDoltMatchesServerGuide(t *testing.T) {
	if got := setting(t, guideFile, `dolthub/dolt/releases/download/v([0-9.]+)/dolt-linux-amd64\.tar\.gz`); got != Dolt {
		t.Errorf("docs/server.md downloads Dolt %s, version.Dolt is %s; change both together", got, Dolt)
	}
}

// doltSums reads dolt.sha256: a checksum for each "vVERSION/ARCHIVE".
func doltSums(t *testing.T) map[string]string {
	t.Helper()
	f, err := os.Open(sumsFile)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sums := map[string]string{}
	line := regexp.MustCompile(`^([0-9a-f]{64})  (\S+)$`)
	for sc := bufio.NewScanner(f); sc.Scan(); {
		if sc.Text() == "" || strings.HasPrefix(sc.Text(), "#") {
			continue
		}
		m := line.FindStringSubmatch(sc.Text())
		if m == nil {
			t.Fatalf("%s: line %q is not a lowercase SHA-256, two spaces and a name", sumsFile, sc.Text())
		}
		if _, dup := sums[m[2]]; dup {
			t.Errorf("%s lists %s twice", sumsFile, m[2])
		}
		sums[m[2]] = m[1]
	}
	return sums
}

// TestDoltChecksums keeps dolt.sha256, which scripts/install-dolt.sh
// verifies a download against, at the pinned release and equal to the
// checksums CI and the Docker test server verify.
func TestDoltChecksums(t *testing.T) {
	sums := doltSums(t)
	tests := []struct {
		platform string
		// file and pattern name another pin of the same archive, if any.
		file, pattern string
	}{
		{"linux-amd64", ciFile, `(?m)^\s*DOLT_SHA256:\s*(\S+)\s*$`},
		{"linux-amd64", dockerFile, `(?m)^ARG DOLT_SHA256_AMD64=(\S+)$`},
		{"linux-arm64", dockerFile, `(?m)^ARG DOLT_SHA256_ARM64=(\S+)$`},
		{"darwin-amd64", "", ""},
		{"darwin-arm64", "", ""},
	}
	want := map[string]bool{}
	for _, tc := range tests {
		name := "v" + Dolt + "/dolt-" + tc.platform + ".tar.gz"
		want[name] = true
		sum, ok := sums[name]
		if !ok {
			t.Errorf("%s has no checksum for %s; pin one for every platform when version.Dolt changes (RELEASING.md)", sumsFile, name)
			continue
		}
		if tc.file == "" {
			continue
		}
		if got := setting(t, tc.file, tc.pattern); got != sum {
			t.Errorf("%s pins %s for %s, %s pins %s; change both together", tc.file, got, name, sumsFile, sum)
		}
	}
	for name := range sums {
		if !want[name] {
			t.Errorf("%s lists %s, which is not an archive of version.Dolt %s; remove it", sumsFile, name, Dolt)
		}
	}
}

// TestMiseMatchesGoMod keeps the Go that mise installs at go.mod's go
// line, which is also what CI's setup-go reads.
func TestMiseMatchesGoMod(t *testing.T) {
	want := setting(t, goModFile, `(?m)^go (\S+)$`)
	if got := setting(t, miseFile, `(?m)^go = "([^"]+)"$`); got != want {
		t.Errorf("mise.toml go is %s, go.mod's go line is %s; change both together", got, want)
	}
}

// TestMiseMatchesCI keeps the golangci-lint that mise installs at the
// version CI's lint job pins.
func TestMiseMatchesCI(t *testing.T) {
	want := setting(t, ciFile, `(?m)^\s*- uses: golangci/golangci-lint-action@.*\n\s*with:\n\s*version: v(\S+)$`)
	if got := setting(t, miseFile, `(?m)^golangci-lint = "([^"]+)"$`); got != want {
		t.Errorf("mise.toml golangci-lint is %s, ci.yml pins v%s; change both together", got, want)
	}
}
