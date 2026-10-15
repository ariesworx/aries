package store

import (
	"errors"
	"strings"
	"testing"
)

func TestProjectSchema(t *testing.T) {
	t.Parallel()
	longest := strings.Repeat("a", MaxProjectNameForSchema)
	for _, tc := range []struct {
		name, in, want string
	}{
		{"a word", "web", "project_web"},
		{"hyphens become underscores", "web-app-2", "project_web_app_2"},
		{"digits", "p1", "project_p1"},
		{"the longest", longest, "project_" + longest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ProjectSchema(tc.in)
			if err != nil || got != tc.want {
				t.Fatalf("ProjectSchema(%q) = %q, %v, want %q", tc.in, got, err, tc.want)
			}
			if !ValidSchemaName(got) {
				t.Errorf("ProjectSchema(%q) = %q, which is no valid schema name", tc.in, got)
			}
		})
	}
	for _, tc := range []struct {
		name, in, want string
	}{
		{"too long", longest + "b", "at most 55"},
		{"not a code name", "Web", "code name"},
		{"an underscore", "web_app", "code name"},
		{"empty", "", "code name"},
		{"a quote", `web"; DROP`, "code name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ProjectSchema(tc.in)
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ProjectSchema(%q) = %q, %v, want ErrInvalid saying %q", tc.in, got, err, tc.want)
			}
		})
	}
}

// Every code name maps to a schema of its own: a hyphen is the one
// character changed, and a code name has no underscore to meet it.
func TestProjectSchemaNamesDiffer(t *testing.T) {
	t.Parallel()
	a, errA := ProjectSchema("web-app")
	b, errB := ProjectSchema("webapp")
	if errA != nil || errB != nil || a == b {
		t.Errorf("ProjectSchema(web-app), ProjectSchema(webapp) = %q, %q (%v, %v), want two schemas", a, b, errA, errB)
	}
}
