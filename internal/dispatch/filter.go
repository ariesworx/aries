package dispatch

import (
	"slices"
	"strings"

	"github.com/ariesworx/aries/internal/proto"
)

// Filter limits a run to some of the ready issues, so that a trial can be
// scoped (`--models`, `--labels`). The zero Filter limits nothing.
type Filter struct {
	// Models are the model aliases an issue's single model label may name.
	Models []string
	// Labels are labels an issue must all carry.
	Labels []string
}

// ParseFilter reads the comma-separated values of --models and --labels.
// A model must be one the dispatcher drives.
func ParseFilter(modelList, labelList string) (Filter, error) {
	var f Filter
	known := make([]string, 0, len(models))
	for _, m := range models {
		known = append(known, m)
	}
	slices.Sort(known)
	if modelList != "" {
		for _, m := range strings.Split(modelList, ",") {
			m = strings.ToLower(strings.TrimSpace(m))
			switch {
			case m == "":
				return f, proto.Errf(proto.CodeInvalid, "give model names alone: --models haiku,sonnet", "--models has an empty name")
			case !slices.Contains(known, m):
				return f, proto.Errf(proto.CodeInvalid, "use one of "+strings.Join(known, ", "), "unknown model "+quote(m)+" in --models")
			case !slices.Contains(f.Models, m):
				f.Models = append(f.Models, m)
			}
		}
	}
	if labelList != "" {
		for _, l := range strings.Split(labelList, ",") {
			l = strings.TrimSpace(l)
			switch {
			case l == "":
				return f, proto.Errf(proto.CodeInvalid, "give labels alone: --labels trial,docs", "--labels has an empty label")
			case !slices.Contains(f.Labels, l):
				f.Labels = append(f.Labels, l)
			}
		}
	}
	return f, nil
}

func quote(s string) string { return `"` + s + `"` }

// Active reports whether the filter limits anything.
func (f Filter) Active() bool { return len(f.Models) > 0 || len(f.Labels) > 0 }

// String says the filters in force, as the dry run and status print them.
func (f Filter) String() string {
	var parts []string
	if len(f.Models) > 0 {
		parts = append(parts, "only models "+strings.Join(f.Models, ","))
	}
	if len(f.Labels) > 0 {
		parts = append(parts, "only labels "+strings.Join(f.Labels, ","))
	}
	return strings.Join(parts, "; ")
}

// Apply skips a decision to start whose issue the filter leaves out,
// naming the rule; any other decision stands.
func (f Filter) Apply(sr proto.ShowResult, dec Decision) Decision {
	if dec.Action != ActStart {
		return dec
	}
	skip := func(rule string) Decision {
		return Decision{Action: ActSkip, Issue: dec.Issue, Rule: rule, Why: why[rule]}
	}
	if len(f.Models) > 0 && !slices.Contains(f.Models, dec.Model) {
		return skip(RuleOnlyModels)
	}
	for _, l := range f.Labels {
		if !slices.Contains(sr.Issue.Labels, l) {
			return skip(RuleOnlyLabels)
		}
	}
	return dec
}
