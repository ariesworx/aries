package bdimport

import (
	"context"

	"github.com/ariesworx/aries/internal/store"
)

// Source is what Export reads of a project's store.
type Source interface {
	List(ctx context.Context, f store.Filter) (store.IssuePage, error)
	AllDeps(ctx context.Context) ([]store.Dep, error)
	AllComments(ctx context.Context) ([]store.Comment, error)
}

// Target is what Import reads and writes of a project's store: what is
// there, and each record planned, for a dry run, or imported.
type Target interface {
	List(ctx context.Context, f store.Filter) (store.IssuePage, error)
	AllDeps(ctx context.Context) ([]store.Dep, error)
	PlanImportIssue(ctx context.Context, in store.Issue) (store.ImportResult, error)
	ImportIssue(ctx context.Context, actor store.Actor, in store.Issue) (store.ImportResult, error)
	PlanImportDep(ctx context.Context, d store.Dep) (store.ImportOutcome, error)
	ImportDep(ctx context.Context, actor store.Actor, d store.Dep) (store.ImportOutcome, error)
	PlanImportComment(ctx context.Context, c store.Comment) (store.ImportOutcome, error)
	ImportComment(ctx context.Context, actor store.Actor, c store.Comment) (store.ImportOutcome, error)
	PlanImportMemory(ctx context.Context, in store.NewMemory) (store.ImportOutcome, error)
	ImportMemory(ctx context.Context, actor store.Actor, in store.NewMemory) (store.ImportOutcome, error)
}

var (
	_ Source = (*store.Store)(nil)
	_ Target = (*store.Store)(nil)
)
