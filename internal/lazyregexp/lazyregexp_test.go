package lazyregexp

import (
	"regexp"
	"slices"
	"sync"
	"testing"
)

// Each method answers as regexp's own does for the same expression.
func TestMethods(t *testing.T) {
	t.Parallel()
	const expr = `([a-z]+)=([0-9]+)`
	lazy, want := New(expr), regexp.MustCompile(expr)
	for _, s := range []string{"", "a=1", "x a=1 bb=22 c=", "A=1"} {
		if got, w := lazy.MatchString(s), want.MatchString(s); got != w {
			t.Errorf("MatchString(%q) = %v, want %v", s, got, w)
		}
		if got, w := lazy.FindString(s), want.FindString(s); got != w {
			t.Errorf("FindString(%q) = %q, want %q", s, got, w)
		}
		if got, w := lazy.FindStringIndex(s), want.FindStringIndex(s); !slices.Equal(got, w) {
			t.Errorf("FindStringIndex(%q) = %v, want %v", s, got, w)
		}
		if got, w := lazy.FindStringSubmatch(s), want.FindStringSubmatch(s); !slices.Equal(got, w) {
			t.Errorf("FindStringSubmatch(%q) = %q, want %q", s, got, w)
		}
		if got, w := lazy.FindAllStringIndex(s, -1), want.FindAllStringIndex(s, -1); !slices.EqualFunc(got, w, slices.Equal) {
			t.Errorf("FindAllStringIndex(%q, -1) = %v, want %v", s, got, w)
		}
		if got, w := lazy.FindAllStringSubmatch(s, -1), want.FindAllStringSubmatch(s, -1); !slices.EqualFunc(got, w, slices.Equal) {
			t.Errorf("FindAllStringSubmatch(%q, -1) = %q, want %q", s, got, w)
		}
		if got, w := lazy.FindAllStringSubmatchIndex(s, -1), want.FindAllStringSubmatchIndex(s, -1); !slices.EqualFunc(got, w, slices.Equal) {
			t.Errorf("FindAllStringSubmatchIndex(%q, -1) = %v, want %v", s, got, w)
		}
	}
	if got := lazy.String(); got != expr {
		t.Errorf("String() = %q, want %q", got, expr)
	}
}

// Goroutines that use a Regexp for the first time at once all get the
// same answer, and -race finds no race.
func TestFirstUseConcurrent(t *testing.T) {
	t.Parallel()
	r := &Regexp{expr: `^[a-z]{1,8}$`} // not compiled yet, as outside a test binary
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if !r.MatchString("alice") {
				t.Error(`MatchString("alice") = false, want true`)
			}
		})
	}
	wg.Wait()
}

// In a test binary New compiles at once, so an expression that does not
// compile fails the tests of the package that declares it as it loads,
// as regexp.MustCompile in a package variable would.
func TestNewCompilesInTests(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("New(`(`) in a test binary did not panic")
		}
	}()
	New(`(`)
}
