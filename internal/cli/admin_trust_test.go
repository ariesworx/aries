package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// The trust commands refuse bad usage before dialing.
func TestAdminTrustUsage(t *testing.T) {
	t.Parallel()
	empty := t.TempDir()
	add := []string{"admin", "trust", "add", "ci-main", "--principal", "ci", "--provider", "github-actions"}
	for _, tc := range []struct {
		name   string
		args   []string
		stderr string
	}{
		{"an action", []string{"admin", "trust"}, "admin trust needs add, list, show, set, disable, enable or remove"},
		{"an unknown action", []string{"admin", "trust", "rm", "ci-main"}, `unknown trust action "rm"`},
		{"add needs a claim", add, "trust add needs --principal, --provider and at least one --claim"},
		{"add needs a principal", []string{"admin", "trust", "add", "ci-main", "--provider", "p", "--claim", "a=b"}, "needs --principal"},
		{"add needs a name", []string{"admin", "trust", "add", "--principal", "ci", "--provider", "p", "--claim", "a=b"}, "trust add needs one rule's name"},
		{"a claim with no value", append(add, "--claim", "repository_id"), "--claim takes NAME=VALUE"},
		{"a claim twice", append(add, "--claim", "a=1", "--claim", "a=2"), "--claim a is given twice"},
		{"list takes one service", []string{"admin", "trust", "list", "a", "b"}, "trust list takes at most one service principal"},
		{"list takes no claim", []string{"admin", "trust", "list", "--claim", "a=b"}, "trust list does not take --claim"},
		{"show needs a name", []string{"admin", "trust", "show"}, "trust show needs one rule's name"},
		{"set needs a change", []string{"admin", "trust", "set", "ci-main"}, "trust set needs --claim or --token-lifetime"},
		{"set takes no principal", []string{"admin", "trust", "set", "ci-main", "--principal", "x"}, "trust set does not take --principal"},
		{"disable takes no claim", []string{"admin", "trust", "disable", "ci-main", "--claim", "a=b"}, "trust disable does not take --claim"},
		{"remove needs a name", []string{"admin", "trust", "remove"}, "trust remove needs one rule's name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errb := runCLI(t, append([]string{"-C", empty}, tc.args...)...)
			if code != ExitUsage || out != "" || !strings.Contains(errb, tc.stderr) || !strings.Contains(errb, "fix: usage: aries admin trust ") {
				t.Errorf("aries %s: exit %d, stdout %q, stderr %q; want usage naming %q", strings.Join(tc.args, " "), code, out, errb, tc.stderr)
			}
		})
	}
	_, help, _ := runCLI(t, "help", "admin")
	if !strings.Contains(help, "admin trust add NAME --principal SERVICE --provider P --claim NAME=VALUE... [--token-lifetime D]") {
		t.Errorf("aries help admin lacks trust:\n%s", help)
	}
}

func TestPrintTrust(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	rule := proto.TrustRule{Name: "ci-main", Principal: "ci-example", Provider: "github-actions", TokenLifetime: "1h", State: proto.TrustActive,
		Claims:    map[string]string{"repository_owner_id": "111111111", "repository_id": "123456789", "ref": "refs/heads/main\x1b[2J"},
		CreatedBy: "dana", CreatedAt: at, UpdatedAt: at, Rev: 1}
	var b bytes.Buffer
	r := &runner{env: Env{Stdout: &b}}
	if err := r.printTrust(proto.TrustResult{TrustRule: rule, Change: "added", Required: []string{"repository_owner_id", "repository_id"}}, "added"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"trust rule ci-main added\n",
		"ci-main signs in as ci-example by a token of github-actions carrying ref=refs/heads/main\\x1b[2J repository_id=123456789 repository_owner_id=111111111, for 1h (active, rev 1)\n"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("printTrust(added) =\n%s\nwant %q", b.String(), want)
		}
	}
	b.Reset()
	_ = r.printTrust(proto.TrustResult{TrustRule: rule, Required: []string{"repository_owner_id", "repository_id"}}, "")
	if !strings.Contains(b.String(), "a rule at github-actions must pin repository_owner_id and repository_id\n") || !strings.Contains(b.String(), "added by dana") {
		t.Errorf("printTrust(show) =\n%s", b.String())
	}
	b.Reset()
	_ = r.printTrust(proto.TrustResult{TrustRule: rule, Change: "removed", Ended: 2}, "removed")
	if b.String() != "trust rule ci-main removed; 2 sign-ins it gave ended, and their connections closed\n" {
		t.Errorf("printTrust(removed) = %q", b.String())
	}
	b.Reset()
	_ = r.printTrust(proto.TrustResult{TrustRule: rule}, "changed")
	if b.String() != "trust rule ci-main was already so\n" {
		t.Errorf("printTrust(unchanged) = %q", b.String())
	}
	b.Reset()
	printTrustRules(&b, []proto.TrustRule{rule})
	if !strings.Contains(b.String(), "NAME") || !strings.Contains(b.String(), "ci-main  ci-example  github-actions  active  1h") {
		t.Errorf("printTrustRules =\n%s", b.String())
	}
	b.Reset()
	printTrustRules(&b, nil)
	if b.String() != "no trust rules\n" {
		t.Errorf("printTrustRules(none) = %q", b.String())
	}
}
