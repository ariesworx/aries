package store

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
)

// stageKey marks the context a staged Tracer returned for a stage.
type stageKey struct{}

// staged is a Tracer that keeps what it was told, in order: "begin
// STAGE" with the attempt's number when it has one, and "end STAGE
// OUTCOME". under is the stage each began beneath, read from the context
// it was given.
type staged struct {
	mu     sync.Mutex
	calls  []string
	under  map[string]string
	callID any
}

func (s *staged) Begin(ctx context.Context, stage Stage, attempt int) (context.Context, func(Outcome)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := string(stage)
	if attempt > 0 {
		name = fmt.Sprintf("%s %d", stage, attempt)
	}
	s.calls = append(s.calls, "begin "+name)
	if s.under == nil {
		s.under = map[string]string{}
	}
	parent, _ := ctx.Value(stageKey{}).(string)
	s.under[name] = parent
	if v := ctx.Value(callerKey{}); v != nil {
		s.callID = v
	}
	return context.WithValue(ctx, stageKey{}, name), func(o Outcome) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.calls = append(s.calls, fmt.Sprintf("end %s %s", name, o))
	}
}

// take returns the calls since the last take.
func (s *staged) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.calls
	s.calls = nil
	return out
}

// callerKey marks the context a test passes to the store.
type callerKey struct{}

// The tracer is told where a write's time goes, under the caller's
// context: each attempt, with the wait for the writer inside it, a new
// attempt for each retry, each Dolt commit, and the migrations of an
// open. A read tells it nothing.
func TestTracer(t *testing.T) {
	tr := &staged{}
	s := openStore(t, newDSN(t), Options{Tracer: tr})
	if got, want := tr.take(), []string{"begin migrate", "end migrate ok"}; !slices.Equal(got, want) {
		t.Errorf("opening the store told the tracer %v, want %v", got, want)
	}
	ctx := context.WithValue(t.Context(), callerKey{}, "the request")
	once := []string{"begin write.attempt 1", "begin write.wait", "end write.wait ok", "end write.attempt 1 ok"}

	a, err := s.CreateIssue(ctx, alice, NewIssue{Title: "a"})
	if err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		name string
		do   func() error
		want []string
	}{
		{name: "a write", do: func() error { return nil }, want: once},
		{name: "a read", do: func() error { _, err := s.GetIssue(ctx, a.ID); return err }},
		{name: "a write that loses twice is three attempts", do: func() error {
			lost := 0
			s.beforeCommit = func(context.Context) error {
				if lost++; lost <= 2 {
					return fmt.Errorf("%w: injected", errRetry)
				}
				return nil
			}
			defer func() { s.beforeCommit = nil }()
			_, _, err := s.StartIssue(ctx, alice, a.ID, 0, false)
			return err
		}, want: []string{
			"begin write.attempt 1", "begin write.wait", "end write.wait ok", "end write.attempt 1 retry",
			"begin write.attempt 2", "begin write.wait", "end write.wait ok", "end write.attempt 2 retry",
			"begin write.attempt 3", "begin write.wait", "end write.wait ok", "end write.attempt 3 ok",
		}},
		{name: "a write the store refuses", do: func() error {
			if _, _, err := s.StartIssue(ctx, bob, a.ID, 0, false); err == nil {
				return fmt.Errorf("bob started an issue alice holds")
			}
			return nil
		}, want: []string{"begin write.attempt 1", "begin write.wait", "end write.wait ok", "end write.attempt 1 error"}},
		{name: "a Dolt commit", do: func() error { return s.Flush(ctx) }, want: []string{"begin commit", "end commit ok"}},
		{name: "a flush with nothing pending", do: func() error { return s.Flush(ctx) }},
	}
	for _, st := range steps {
		if err := st.do(); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		if got := tr.take(); !slices.Equal(got, st.want) {
			t.Errorf("%s: the tracer was told\n  %v\nwant\n  %v", st.name, got, st.want)
		}
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.callID != "the request" {
		t.Errorf("the tracer was not given the caller's context: its value is %v", tr.callID)
	}
	// An attempt begins under the caller's context, and its wait under
	// the attempt's: the last wait was the refused write's.
	for stage, want := range map[string]string{"write.attempt 1": "", "write.attempt 3": "", "write.wait": "write.attempt 1", "commit": ""} {
		if got := tr.under[stage]; got != want {
			t.Errorf("%s began under %q, want %q", stage, got, want)
		}
	}
}

// A store with no tracer does the same work, and a write allocates
// nothing for the hook.
func TestNoTracer(t *testing.T) {
	s := openStore(t, newDSN(t), Options{})
	ctx, end := s.begin(t.Context(), StageAttempt, 1)
	if ctx != t.Context() {
		t.Errorf("begin changed the context with no tracer")
	}
	end(OutcomeOK)
	if n := testing.AllocsPerRun(100, func() {
		_, end := s.begin(t.Context(), StageWait, 0)
		end(OutcomeOK)
	}); n != 0 {
		t.Errorf("the hook allocates %v times with no tracer, want 0", n)
	}
}
