package store

import "testing"

// BenchmarkWrite backs the claim that a write pays nothing for looking,
// once its transaction has begun, for a step of the clock that is not
// settled (writeOnce): "empty" is the write path alone, a transaction
// that begins and commits, and "comment" a small write with its event.
func BenchmarkWrite(b *testing.B) {
	s := openStore(b, newDSN(b), Options{})
	ctx := b.Context()
	is, err := s.CreateIssue(ctx, alice, NewIssue{Title: "issue"})
	if err != nil {
		b.Fatal(err)
	}
	b.Run("empty", func(b *testing.B) {
		for b.Loop() {
			if err := s.write(ctx, alice, func(*wtx) error { return nil }); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("comment", func(b *testing.B) {
		for b.Loop() {
			if _, err := s.AddComment(ctx, alice, is.ID, "a comment", ""); err != nil {
				b.Fatal(err)
			}
		}
	})
}
