package retrieval

import (
	"context"
	"github.com/autobid/autobid-engine/db"
)

type budgetKey struct{}

func withBudget(ctx context.Context, run *db.RetrievalRun) context.Context {
	return context.WithValue(ctx, budgetKey{}, run)
}
func (s *Service) reserve(ctx context.Context, calls, tokens int) error {
	if run, ok := ctx.Value(budgetKey{}).(*db.RetrievalRun); ok {
		return s.DB.ReserveRetrieval(ctx, run.TenantID, run.ID, calls, tokens)
	}
	return nil
}
