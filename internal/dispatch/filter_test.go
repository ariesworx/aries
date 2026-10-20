package dispatch

import (
	"errors"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
)

func TestParseFilter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, models, labels string
		want                 Filter
		err                  string
	}{
		{name: "none"},
		{name: "one model", models: "haiku", want: Filter{Models: []string{"haiku"}}},
		{name: "two models, spaced and cased", models: " Haiku, sonnet ", want: Filter{Models: []string{"haiku", "sonnet"}}},
		{name: "a model twice is one", models: "haiku,haiku", want: Filter{Models: []string{"haiku"}}},
		{name: "labels", labels: "trial, docs", want: Filter{Labels: []string{"trial", "docs"}}},
		{name: "both", models: "haiku", labels: "trial", want: Filter{Models: []string{"haiku"}, Labels: []string{"trial"}}},
		{name: "unknown model", models: "haiku,gpt", err: `unknown model "gpt"`},
		{name: "empty model", models: "haiku,", err: "empty"},
		{name: "empty label", labels: "trial,,x", err: "empty"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseFilter(tc.models, tc.labels)
			if tc.err != "" {
				var pe *proto.Error
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("ParseFilter error = %v; want %q", err, tc.err)
				}
				if !errors.As(err, &pe) || pe.Fix == "" {
					t.Errorf("the error has no Fix: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got.Models, ",") != strings.Join(tc.want.Models, ",") || strings.Join(got.Labels, ",") != strings.Join(tc.want.Labels, ",") {
				t.Errorf("ParseFilter = %+v; want %+v", got, tc.want)
			}
		})
	}
}

func TestFilterApply(t *testing.T) {
	t.Parallel()
	start := func(model string) Decision { return Decision{Action: ActStart, Issue: "ar-a1", Model: model} }
	tests := []struct {
		name   string
		f      Filter
		labels []string
		dec    Decision
		action Action
		rule   string
	}{
		{name: "no filter starts", dec: start("opus"), labels: []string{"model:opus"}, action: ActStart},
		{name: "model in the list", f: Filter{Models: []string{"haiku", "sonnet"}}, dec: start("sonnet"), labels: []string{"model:sonnet"}, action: ActStart},
		{name: "model not in the list", f: Filter{Models: []string{"haiku"}}, dec: start("sonnet"), labels: []string{"model:sonnet"}, action: ActSkip, rule: RuleOnlyModels},
		{name: "opus not in the list", f: Filter{Models: []string{"haiku", "sonnet"}}, dec: start("opus"), action: ActSkip, rule: RuleOnlyModels},
		{name: "label present", f: Filter{Labels: []string{"trial"}}, dec: start("haiku"), labels: []string{"model:haiku", "trial"}, action: ActStart},
		{name: "label missing", f: Filter{Labels: []string{"trial"}}, dec: start("haiku"), labels: []string{"model:haiku"}, action: ActSkip, rule: RuleOnlyLabels},
		{name: "all labels needed", f: Filter{Labels: []string{"trial", "docs"}}, dec: start("haiku"), labels: []string{"trial"}, action: ActSkip, rule: RuleOnlyLabels},
		{name: "both met", f: Filter{Models: []string{"haiku"}, Labels: []string{"trial"}}, dec: start("haiku"), labels: []string{"trial"}, action: ActStart},
		{name: "both unmet names models first", f: Filter{Models: []string{"haiku"}, Labels: []string{"trial"}}, dec: start("opus"), action: ActSkip, rule: RuleOnlyModels},
		{name: "model met, label unmet", f: Filter{Models: []string{"haiku"}, Labels: []string{"trial"}}, dec: start("haiku"), action: ActSkip, rule: RuleOnlyLabels},
		{name: "a skip keeps its own rule", f: Filter{Labels: []string{"trial"}}, dec: Decision{Action: ActSkip, Issue: "ar-a1", Rule: RuleHuman, Why: "w"}, action: ActSkip, rule: RuleHuman},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sr := proto.ShowResult{Issue: proto.Issue{ID: "ar-a1", Labels: tc.labels}}
			got := tc.f.Apply(sr, tc.dec)
			if got.Action != tc.action || got.Rule != tc.rule {
				t.Fatalf("Apply = %+v; want action %s rule %q", got, tc.action, tc.rule)
			}
			if got.Action == ActSkip && got.Why == "" {
				t.Errorf("a skip by %s says no why", got.Rule)
			}
		})
	}
}

// A dry run names the filters in force and skips what they leave out,
// each with its rule.
func TestDryRunFilters(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.tracker.add("ar-1", "feature", "model:opus", "trial")
	w.tracker.add("ar-2", "task", "model:sonnet", "trial")
	w.tracker.add("ar-3", "task", "model:haiku")
	w.tracker.add("ar-4", "task", "model:haiku", "trial")
	w.tracker.add("ar-5", "task", "human", "trial")
	cfg := DefaultConfig()
	cfg.DryRun = true
	cfg.Filter = Filter{Models: []string{"haiku"}, Labels: []string{"trial"}}
	d, _ := w.dispatcher(cfg)
	if err := d.Wake(t.Context(), WakeStart); err != nil {
		t.Fatal(err)
	}
	out := w.out.String()
	for _, want := range []string{
		"only models haiku; only labels trial",
		"skip ar-1 [only-models]: ",
		"skip ar-2 [only-models]: ",
		"skip ar-3 [only-labels]: ",
		"would start ar-4 (model haiku)",
		"skip ar-5 [human]: ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry run's output lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "would start") != 1 {
		t.Errorf("more than one would start:\n%s", out)
	}
}

// A real run records the filters in its state, for dispatch status.
func TestWakeRecordsFilters(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	cfg := DefaultConfig()
	cfg.Filter = Filter{Models: []string{"haiku"}, Labels: []string{"trial"}}
	d, _ := w.dispatcher(cfg)
	if err := d.Wake(t.Context(), WakeStart); err != nil {
		t.Fatal(err)
	}
	if strings.Join(d.State.Models, ",") != "haiku" || strings.Join(d.State.Labels, ",") != "trial" {
		t.Errorf("state models %v labels %v", d.State.Models, d.State.Labels)
	}
}
