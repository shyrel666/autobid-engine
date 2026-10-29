package retrieval

import (
	"context"
	"github.com/autobid/autobid-engine/db"
)

type budgetKey struct{}

func withBudget(ctx context.Context, run *db.RetrievalRun) context.Context {
	return context.WithValue(ctx, budgetKey{}, run)
}

// tokensLeft reports the unreserved token budget of the run bound to ctx; ok is false outside a budgeted run.
func (s *Service) tokensLeft(ctx context.Context) (int, bool) {
	run, ok := ctx.Value(budgetKey{}).(*db.RetrievalRun)
	if !ok {
		return 0, false
	}
	left, err := s.DB.RetrievalTokensLeft(ctx, run.TenantID, run.ID)
	if err != nil {
		return 0, true
	}
	return left, true
}
func (s *Service) reserve(ctx context.Context, calls, tokens int) error {
	if run, ok := ctx.Value(budgetKey{}).(*db.RetrievalRun); ok {
		return s.DB.ReserveRetrieval(ctx, run.TenantID, run.ID, calls, tokens)
	}
	return nil
}
