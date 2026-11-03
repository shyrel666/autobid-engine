package db

import (
	"context"
	"fmt"
	"slices"
)

type executionScopeKey struct{}

// WithExecutionScope carries host-supplied capabilities, never model input.
func WithExecutionScope(ctx context.Context, m *IntentMandate) context.Context {
	if m == nil {
		return ctx
	}
	copy := *m
	copy.AllowedSectionIDs = slices.Clone(m.AllowedSectionIDs)
	copy.AllowedRequirementIDs = slices.Clone(m.AllowedRequirementIDs)
	return context.WithValue(ctx, executionScopeKey{}, &copy)
}
func CheckSectionScope(ctx context.Context, id int64) error {
	if m, _ := ctx.Value(executionScopeKey{}).(*IntentMandate); m != nil && (m.RestrictObjects || len(m.AllowedSectionIDs) > 0) && !slices.Contains(m.AllowedSectionIDs, id) {
		return fmt.Errorf("execution scope: section %d is not authorized", id)
	}
	return nil
}
func CheckRequirementScope(ctx context.Context, id int64) error {
	if m, _ := ctx.Value(executionScopeKey{}).(*IntentMandate); m != nil && (m.RestrictObjects || len(m.AllowedRequirementIDs) > 0) && !slices.Contains(m.AllowedRequirementIDs, id) {
		return fmt.Errorf("execution scope: requirement %d is not authorized", id)
	}
	return nil
}
