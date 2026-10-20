package dispatch

import (
	"github.com/ariesworx/aries/internal/brief"
)

// Brief is a headless run's brief, rendered from the project's template
// (.aries/brief.txt; dispatcher.md §7, As built: the brief template)
// with the issue as start returned it, never written by a model. The
// same input gives the same text, byte for byte, which is what the
// brief: comment records and the outcome record hashes. It holds no
// secret.
func Brief(t *brief.Template, st Started, session, dir string) string {
	return t.Render(brief.Input{Harness: brief.Headless, Issue: st.Issue, Items: st.Items, Handoff: st.Handoff,
		Session: session, Worktree: dir, Branch: st.Branch})
}
