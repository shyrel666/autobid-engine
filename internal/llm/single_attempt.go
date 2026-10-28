package llm

import "context"

type singleAttemptKey struct{}

// WithSingleAttempt disables application-level retries/failover for pre-budgeted calls.
func WithSingleAttempt(ctx context.Context) context.Context {
	return context.WithValue(ctx, singleAttemptKey{}, true)
}
func SingleAttempt(ctx context.Context) bool { v, _ := ctx.Value(singleAttemptKey{}).(bool); return v }
