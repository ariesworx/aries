package release

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// workflow is the part of a GitHub Actions workflow these tests read.
type workflow struct {
	On   map[string]any `yaml:"on"`
	Jobs map[string]struct {
		Uses  string            `yaml:"uses"`
		If    string            `yaml:"if"`
		Needs any               `yaml:"needs"`
		Env   map[string]string `yaml:"env"`
		Steps []struct {
			Uses string            `yaml:"uses"`
			Run  string            `yaml:"run"`
			With map[string]string `yaml:"with"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func readWorkflow(t *testing.T, name string) workflow {
	t.Helper()
	b, err := os.ReadFile("../../.github/workflows/" + name) //nolint:gosec // a fixed workflow name
	if err != nil {
		t.Fatal(err)
	}
	var w workflow
	if err := yaml.Unmarshal(b, &w); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return w
}

func needs(v any) []string {
	switch n := v.(type) {
	case string:
		return []string{n}
	case []any:
		var out []string
		for _, s := range n {
			if s, ok := s.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// TestReleaseRunsGate pins the release gate: the CI gate runs on the tagged
// commit and the build waits for it, in the run a pushed tag starts.
func TestReleaseRunsGate(t *testing.T) {
	t.Parallel()
	rel := readWorkflow(t, "release.yml")
	if got := rel.Jobs["gate"].Uses; got != "./.github/workflows/ci.yml" {
		t.Errorf("release.yml gate job uses %q, want ./.github/workflows/ci.yml", got)
	}
	if got := needs(rel.Jobs["build"].Needs); !slices.Contains(got, "gate") {
		t.Errorf("release.yml build needs %v, want gate", got)
	}
	const onPush = "github.event_name == 'push'"
	for _, job := range []string{"gate", "build"} {
		if got := rel.Jobs[job].If; got != onPush {
			t.Errorf("release.yml %s runs if %q, want %q: it belongs to the run a pushed tag starts", job, got, onPush)
		}
	}
	ci := readWorkflow(t, "ci.yml")
	if _, ok := ci.On["workflow_call"]; !ok {
		t.Error("ci.yml cannot be called by release.yml: no workflow_call trigger")
	}
}

// TestReleaseSigningIsApproved pins how a release is approved. Signing is
// a run of its own, started by hand; it reads the key from the release
// environment; and before anything else it checks who started it and that
// the build run it was given is this tag's own, on this commit, and green.
// The archives it signs are that run's and no other's.
func TestReleaseSigningIsApproved(t *testing.T) {
	t.Parallel()
	rel := readWorkflow(t, "release.yml")
	if _, ok := rel.On["workflow_dispatch"]; !ok {
		t.Fatal("release.yml has no workflow_dispatch trigger: nothing starts the signing run")
	}
	sign, ok := rel.Jobs["sign-and-publish"]
	if !ok {
		t.Fatal("release.yml has no sign-and-publish job")
	}
	if want := "github.event_name == 'workflow_dispatch'"; sign.If != want {
		t.Errorf("sign-and-publish runs if %q, want %q: a pushed tag must not sign by itself", sign.If, want)
	}
	if got := needs(sign.Needs); len(got) != 0 {
		t.Errorf("sign-and-publish needs %v, want nothing: the build is another run, checked by its ID", got)
	}
	if len(sign.Steps) == 0 {
		t.Fatal("sign-and-publish has no steps")
	}
	check := sign.Steps[0].Run
	for _, want := range []string{
		`"$ACTOR" != "$APPROVER"`, `"$TRIGGERING_ACTOR" != "$APPROVER"`, // who
		`"$GITHUB_REF_TYPE" != tag`,                           // at a tag
		`actions/runs/$BUILD_RUN`,                             // the run named
		`.path, .event, .head_branch, .head_sha, .conclusion`, // what is compared
		`.github/workflows/release.yml push $TAG $GITHUB_SHA success`,
	} {
		if !strings.Contains(check, want) {
			t.Errorf("sign-and-publish's first step does not check %q", want)
		}
	}
	var downloads int
	for i, st := range sign.Steps {
		if strings.Contains(st.Run, "ARIES_RELEASE_KEY") && i == 0 {
			t.Error("sign-and-publish reads the key in its first step, before its checks")
		}
		if strings.HasPrefix(st.Uses, "actions/download-artifact@") {
			downloads++
			if got, want := st.With["run-id"], "${{ inputs.build_run }}"; got != want {
				t.Errorf("sign-and-publish downloads from run %q, want %q, the run its first step checked", got, want)
			}
		}
	}
	if downloads != 1 {
		t.Errorf("sign-and-publish has %d download steps, want 1", downloads)
	}
}

// TestCIRequiresDolt pins that CI runs the Dolt-backed tests rather than
// skipping them, in random order.
func TestCIRequiresDolt(t *testing.T) {
	t.Parallel()
	test := readWorkflow(t, "ci.yml").Jobs["test"]
	if got := test.Env["ARIES_REQUIRE_DOLT"]; got != "1" {
		t.Errorf("ci.yml test job ARIES_REQUIRE_DOLT = %q, want 1", got)
	}
	var shuffled bool
	for _, s := range test.Steps {
		if strings.Contains(s.Run, "go test") && strings.Contains(s.Run, "./...") {
			shuffled = strings.Contains(s.Run, "-shuffle=on")
		}
	}
	if !shuffled {
		t.Error("ci.yml runs go test ./... without -shuffle=on")
	}
}

// TestActionsPinned checks AGENTS.md rule 4: every third-party action is
// pinned by full commit SHA, with a version comment.
func TestActionsPinned(t *testing.T) {
	t.Parallel()
	pinned := regexp.MustCompile(`^\s*(?:-\s+)?uses:\s+[\w.-]+/[\w./-]+@[0-9a-f]{40} # v\S+\s*$`)
	uses := regexp.MustCompile(`^\s*(?:-\s+)?uses:\s+(\S+)`)
	files, err := os.ReadDir("../../.github/workflows")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		b, err := os.ReadFile("../../.github/workflows/" + f.Name())
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			m := uses.FindStringSubmatch(line)
			if m == nil || strings.HasPrefix(m[1], "./") {
				continue
			}
			if !pinned.MatchString(line) {
				t.Errorf("%s:%d: %q is not pinned by commit SHA with a version comment", f.Name(), i+1, strings.TrimSpace(line))
			}
		}
	}
}
