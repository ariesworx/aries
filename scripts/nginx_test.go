package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// docsNginxBlock returns the nginx server block docs/server.md gives
// under Behind a proxy.
func docsNginxBlock(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "docs", "server.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(b), "```nginx\n")
	if !ok {
		t.Fatal("docs/server.md has no nginx block")
	}
	block, _, ok := strings.Cut(rest, "```\n")
	if !ok {
		t.Fatal("docs/server.md's nginx block does not end")
	}
	return block
}

// Behind a proxy none of ariesd's limits per address applies (every
// connection is the proxy's), so the proxy keeps them: the block that
// docs/server.md gives, which is the Docker test server's nginx.conf
// with the server's name changed, limits the connections and the
// requests that start a sign-in, per client address.
func TestNginxLimitsPerAddress(t *testing.T) {
	t.Parallel()
	conf, err := os.ReadFile(filepath.Join("..", "deploy", "docker", "nginx.conf"))
	if err != nil {
		t.Fatal(err)
	}
	doc := docsNginxBlock(t)
	if got := strings.Replace(string(conf), "server_name localhost 127.0.0.1;", "server_name aries.example.com;", 1); got != doc {
		t.Errorf("docs/server.md's nginx block differs from deploy/docker/nginx.conf, apart from the server's name:\n--- docs\n%s\n--- nginx.conf\n%s", doc, got)
	}
	for name, text := range map[string]string{"docs/server.md": doc, "deploy/docker/nginx.conf": string(conf)} {
		for _, want := range []struct{ what, re string }{
			{"a limit_conn_zone keyed on the client address", `(?m)^\s*limit_conn_zone \$binary_remote_addr zone=\w+:\d+m;`},
			{"a limit_conn", `(?m)^\s*limit_conn \w+ \d+;`},
			{"a limit_req_zone for the key proof", `(?m)^\s*limit_req_zone \$aries_connect_addr zone=\w+:\d+m rate=\d+r/m;`},
			{"a limit_req_zone for the sign-in", `(?m)^\s*limit_req_zone \$aries_signin_addr zone=\w+:\d+m rate=\d+r/m;`},
			{"a limit_req with a burst on each", `(?s)limit_req zone=\w+ burst=\d+ nodelay;.*limit_req zone=\w+ burst=\d+ nodelay;`},
			{"a refusal of 429", `(?m)^\s*limit_req_status 429;`},
			{"limit_conn's refusal of 429", `(?m)^\s*limit_conn_status 429;`},
			{"/v1/connect among the paths limited", `/v1/connect`},
			{"/oauth/authorize among the paths limited", `/oauth/authorize`},
			{"/oauth/token among the paths limited", `/oauth/token`},
		} {
			if !regexp.MustCompile(want.re).MatchString(text) {
				t.Errorf("%s has no %s", name, want.what)
			}
		}
	}
}
