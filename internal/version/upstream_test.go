package version

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// upstreamEnv names the PostgreSQL project's list of releases, a URL or a
// file, for TestPostgresUpstream. vuln.yml sets it weekly.
const upstreamEnv = "ARIES_POSTGRES_VERSIONS"

// pgRelease is one major version in the PostgreSQL project's list of
// releases (https://www.postgresql.org/versions.json).
type pgRelease struct {
	Major       string `json:"major"`
	LatestMinor string `json:"latestMinor"`
	Supported   bool   `json:"supported"`
	EOLDate     string `json:"eolDate"`
}

// postgresBehind returns what releases say is out of date in the pin and
// in the major versions the store opens, oldest to the pin's: a minor
// release newer than the pin, which is how the PostgreSQL project ships
// its security fixes, and a major version in that range it no longer
// supports. It returns nothing when all is current.
func postgresBehind(releases []pgRelease, pin string, oldest int) []string {
	major, minor, ok := strings.Cut(pin, ".")
	if !ok {
		return []string{fmt.Sprintf("version.Postgres %q is not MAJOR.MINOR", pin)}
	}
	byMajor := make(map[string]pgRelease, len(releases))
	for _, r := range releases {
		byMajor[r.Major] = r
	}
	var behind []string
	if r, ok := byMajor[major]; !ok {
		behind = append(behind, fmt.Sprintf("the PostgreSQL project lists no release of major version %s", major))
	} else if have, err := strconv.Atoi(minor); err != nil {
		behind = append(behind, fmt.Sprintf("version.Postgres %q has a minor version that is not a number", pin))
	} else if latest, err := strconv.Atoi(r.LatestMinor); err != nil {
		behind = append(behind, fmt.Sprintf("the PostgreSQL project lists latest minor %q for %s, not a number", r.LatestMinor, major))
	} else if latest > have {
		behind = append(behind, fmt.Sprintf("Postgres %s.%d is out; version.Postgres pins %s", major, latest, pin))
	}
	top, err := strconv.Atoi(major)
	if err != nil {
		return append(behind, fmt.Sprintf("version.Postgres %q has a major version that is not a number", pin))
	}
	for m := oldest; m <= top; m++ {
		if r, ok := byMajor[strconv.Itoa(m)]; ok && !r.Supported {
			behind = append(behind, fmt.Sprintf("Postgres %d reached its end of life on %s, and the store still opens it (version.PostgresMin is %d)", m, r.EOLDate, oldest))
		}
	}
	return behind
}

// TestPostgresBehind holds postgresBehind to canned lists of releases.
func TestPostgresBehind(t *testing.T) {
	t.Parallel()
	current := []pgRelease{
		{Major: "15", LatestMinor: "19", Supported: true},
		{Major: "16", LatestMinor: "15", Supported: true},
		{Major: "17", LatestMinor: "11", Supported: true},
		{Major: "18", LatestMinor: "6", Supported: true},
	}
	for _, c := range []struct {
		name     string
		releases []pgRelease
		pin      string
		oldest   int
		want     []string
	}{
		{name: "current", releases: current, pin: "18.6", oldest: 16},
		{name: "a newer minor", releases: current, pin: "18.5", oldest: 16,
			want: []string{"Postgres 18.6 is out; version.Postgres pins 18.5"}},
		{name: "a minor past nine", releases: []pgRelease{{Major: "18", LatestMinor: "10", Supported: true}}, pin: "18.9", oldest: 18,
			want: []string{"Postgres 18.10 is out; version.Postgres pins 18.9"}},
		{name: "the oldest opened is past its end of life",
			releases: append([]pgRelease{{Major: "16", LatestMinor: "16", Supported: false, EOLDate: "2028-11-09"}}, current[2:]...),
			pin:      "18.6", oldest: 16,
			want: []string{"Postgres 16 reached its end of life on 2028-11-09, and the store still opens it (version.PostgresMin is 16)"}},
		{name: "a major the project does not list", releases: current, pin: "19.0", oldest: 16,
			want: []string{"the PostgreSQL project lists no release of major version 19"}},
		{name: "not MAJOR.MINOR", releases: current, pin: "18", oldest: 16,
			want: []string{`version.Postgres "18" is not MAJOR.MINOR`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := postgresBehind(c.releases, c.pin, c.oldest)
			if strings.Join(got, "\n") != strings.Join(c.want, "\n") {
				t.Errorf("postgresBehind(%s, %d) = %q, want %q", c.pin, c.oldest, got, c.want)
			}
		})
	}
}

// TestPostgresUpstream fails when the PostgreSQL project has released a
// minor version newer than the pin, or no longer supports a major version
// the store opens. It runs only when ARIES_POSTGRES_VERSIONS names the
// project's list of releases, a URL or a file, because it reads the
// network: vuln.yml runs it weekly against
// https://www.postgresql.org/versions.json.
func TestPostgresUpstream(t *testing.T) {
	t.Parallel()
	src := os.Getenv(upstreamEnv)
	if src == "" {
		t.Skip(upstreamEnv + " is not set; it names the PostgreSQL project's list of releases")
	}
	var b []byte
	var err error
	if strings.HasPrefix(src, "https://") {
		b, err = fetch(t.Context(), src)
	} else {
		b, err = os.ReadFile(src) //nolint:gosec // a file the person running the test names
	}
	if err != nil {
		t.Fatal(err)
	}
	var releases []pgRelease
	if err := json.Unmarshal(b, &releases); err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	for _, msg := range postgresBehind(releases, Postgres, PostgresMin) {
		t.Errorf("%s; RELEASING.md, Changing the Postgres pin, says what to change", msg)
	}
}

// fetch reads url, at most 1 MiB of it, within a minute.
func fetch(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil) //nolint:gosec // a URL the person running the test names
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // the same URL
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}
