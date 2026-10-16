package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/oidc"
	"github.com/ariesworx/aries/internal/oidc/oidctest"
	"github.com/ariesworx/aries/internal/server"
)

// workload is the daemon's sign-in of service principals, with
// daemonOpts.workload: the provider github-actions, which is the world's
// stand-in platform, made at the first start, for the server's first
// URL. now is the control store's clock.
func (w *world) workload(now func() time.Time, web *server.HTTPS) (*server.Workload, error) {
	if !w.opts.workload || web == nil {
		return nil, nil
	}
	if w.platform == nil {
		w.platform = oidctest.New(w.t)
	}
	p, err := oidc.OpenWorkload(oidc.WorkloadSettings{Name: "github-actions", Kind: oidc.KindGitHubActions, Issuer: w.platform.URL,
		CAFile: w.platform.CAFile}, web.URLs[0], oidc.Options{Now: now, Skew: 30 * time.Second, MaxAge: 2 * time.Minute, Refetch: time.Minute})
	if err != nil {
		return nil, err
	}
	return &server.Workload{Providers: []*oidc.Workload{p}, TokenLifetime: time.Hour, MaxTokenLifetime: 6 * time.Hour, Fresh: 2 * time.Minute}, nil
}

// actions is a stand-in for GitHub Actions' OIDC token endpoint, on
// loopback: it signs, as the world's platform, a token for the audience
// a job asks for, with the claims of the job's repository, and only for
// the runtime's bearer token.
type actions struct {
	srv    *httptest.Server
	bearer string

	mu     sync.Mutex
	claims map[string]any
}

func (w *world) newActions(t *testing.T, claims map[string]any) *actions {
	t.Helper()
	a := &actions{bearer: "runtime-token-of-the-job", claims: claims}
	a.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "bearer "+a.bearer {
			http.Error(rw, "no", http.StatusUnauthorized)
			return
		}
		a.mu.Lock()
		now := time.Now()
		c := map[string]any{"iss": w.platform.URL, "aud": r.URL.Query().Get("audience"), "iat": now.Unix(), "nbf": now.Unix(),
			"exp": now.Add(5 * time.Minute).Unix(), "jti": now.Format(time.RFC3339Nano)}
		for k, v := range a.claims {
			c[k] = v
		}
		a.mu.Unlock()
		_ = json.NewEncoder(rw).Encode(map[string]string{"value": w.platform.Sign(c)})
	}))
	t.Cleanup(a.srv.Close)
	return a
}

// set changes a claim of the next tokens.
func (a *actions) set(k string, v any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.claims[k] = v
}

// job makes u a GitHub Actions job: CI, its branch, the runtime's token
// endpoint, and a data directory of the job's.
func (a *actions) job(u *user, branch string) {
	u.env["CI"], u.env["GITHUB_HEAD_REF"] = "true", branch
	u.env["ACTIONS_ID_TOKEN_REQUEST_URL"] = a.srv.URL + "/token?api-version=2.0"
	u.env["ACTIONS_ID_TOKEN_REQUEST_TOKEN"] = a.bearer
	u.env["RUNNER_TEMP"] = u.env["ARIES_DATA_DIR"]
}

// TestWorkloadEndToEnd: a CI job signs in as its service principal with
// the token GitHub Actions gives it (`aries login --workload
// github-actions`), with no key or secret of its own, reports a ci
// gate, and can do nothing a person's terminal may; a job from another
// repository is refused, told which claims a rule would pin; and
// `aries logout` ends the job's sign-in.
func TestWorkloadEndToEnd(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{many: true, keys: true, https: true, workload: true, admins: []string{"dana"}})
	dana := w.newUser("dana", "")
	dana.ok("admin", "principal", "add", "alice")
	dana.ok("admin", "principal", "add", "ci-example", "--kind", "service", "--owner", "alice")
	out := dana.ok("admin", "trust", "add", "ci-example-main", "--principal", "ci-example", "--provider", "github-actions",
		"--claim", "repository_owner_id=111111111", "--claim", "repository_id=123456789", "--claim", "ref=refs/heads/main")
	if !strings.Contains(out, "trust rule ci-example-main added") {
		t.Fatalf("aries admin trust add = %q", out)
	}
	if out := dana.ok("admin", "trust", "list"); !strings.Contains(out, "ci-example-main  ci-example  github-actions  active  1h") {
		t.Errorf("aries admin trust list =\n%s", out)
	}
	impl := strings.TrimSpace(dana.ok("create", "Implement the parser"))
	gate := strings.TrimSpace(dana.ok("gate", "add", "--kind", "ci", "--check", "build", "--issue", impl, "--approver", "ci-example"))

	// The job: a key the server does not know, and no secret at all.
	gh := w.newActions(t, map[string]any{"repository_owner_id": "111111111", "repository_id": "123456789", "ref": "refs/heads/main",
		"repository": "example/parser", "run_id": "1001", "run_attempt": "1", "event_name": "push", "workflow_ref": "example/parser/.github/workflows/ci.yml@refs/heads/main"})
	ci := w.newUser("", "")
	gh.job(ci, "feature/"+impl+"-implement-the-parser")
	if r := ci.run("v0.2.0", "gate", "pass", "--check", "build", "--from-branch"); r.code == 0 {
		t.Fatalf("aries gate pass before signing in = exit 0\n%s", r.stdout)
	}
	out = ci.ok("login", "--workload", "github-actions")
	if !strings.HasPrefix(out, "signed in as ci-example by trust rule ci-example-main at github-actions, until ") {
		t.Fatalf("aries login --workload = %q, want ci-example by its rule", out)
	}
	if out := ci.ok("whoami"); !strings.Contains(out, "principal  ci-example") || !strings.Contains(out, "by trust rule ci-example-main") {
		t.Errorf("whoami as the job =\n%s", out)
	}
	if out := ci.ok("gate", "pass", "--check", "build", "--from-branch", "--sha", "fed4321"); !strings.Contains(out, gate+" passed") {
		t.Errorf("aries gate pass as the job = %q, want %s passed", out, gate)
	}
	if r := ci.run("v0.2.0", "admin", "principal", "list"); r.code == 0 || !strings.Contains(r.stderr, "scope ci") {
		t.Errorf("aries admin principal list as the job = exit %d\n%s%s\nwant refused for the scope ci", r.code, r.stdout, r.stderr)
	}
	if out := dana.ok("admin", "signin", "list", "ci-example"); !strings.Contains(out, "github-actions (rule ci-example-main)") || !strings.Contains(out, "live") {
		t.Errorf("aries admin signin list ci-example =\n%s", out)
	}
	if out := ci.ok("logout"); !strings.Contains(out, "signed out of") {
		t.Errorf("aries logout as the job = %q", out)
	}
	if r := ci.run("v0.2.0", "gate", "pass", "--check", "build", "--from-branch"); r.code == 0 {
		t.Errorf("aries gate pass after logout = exit 0\n%s", r.stdout)
	}

	// A job of another repository: no rule, and the refusal names the
	// claims a rule would pin.
	gh.set("repository_id", "987654321")
	r := ci.run("v0.2.0", "login", "--workload", "github-actions")
	if r.code == 0 || !strings.Contains(r.stderr, "no trust rule at github-actions matches") ||
		!strings.Contains(r.stderr, "--claim repository_owner_id=111111111 --claim repository_id=987654321") {
		t.Errorf("aries login --workload from another repository = exit %d\n%s%s\nwant refused, naming the claims to pin", r.code, r.stdout, r.stderr)
	}
	// A job whose workflow may not have a token.
	delete(ci.env, "ACTIONS_ID_TOKEN_REQUEST_URL")
	if r := ci.run("v0.2.0", "login", "--workload", "github-actions"); r.code == 0 || !strings.Contains(r.stderr, "id-token: write") {
		t.Errorf("aries login --workload with no id-token permission = exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
}
