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
	copy.AllowedJobKinds = slices.Clone(m.AllowedJobKinds)
	copy.RequiredJobKinds = slices.Clone(m.RequiredJobKinds)
	copy.AllowedTemplateIDs = slices.Clone(m.AllowedTemplateIDs)
	copy.AllowedFileIDs = slices.Clone(m.AllowedFileIDs)
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

// CheckJobKindScope applies the host-granted job capability to submit, status,
// list and cancel paths alike.
func CheckJobKindScope(ctx context.Context, kind string) error {
	if m, _ := ctx.Value(executionScopeKey{}).(*IntentMandate); m != nil && (m.RestrictObjects || len(m.AllowedJobKinds) > 0) && !slices.Contains(m.AllowedJobKinds, kind) {
		return fmt.Errorf("execution scope: job kind %q is not authorized", kind)
	}
	return nil
}

// CheckTemplateScope applies the granted upload templates to export submission.
// The built-in default template (id 0) is not a tenant object and stays allowed.
func CheckTemplateScope(ctx context.Context, id int64) error {
	if m, _ := ctx.Value(executionScopeKey{}).(*IntentMandate); id > 0 && m != nil && (m.RestrictObjects || len(m.AllowedTemplateIDs) > 0) && !slices.Contains(m.AllowedTemplateIDs, id) {
		return fmt.Errorf("execution scope: export template %d is not authorized", id)
	}
	return nil
}

// CheckFileScope applies the granted task files (originals and ZIP children) to
// tender parse submission, status, result paging and cancellation alike.
func CheckFileScope(ctx context.Context, id int64) error {
	if m, _ := ctx.Value(executionScopeKey{}).(*IntentMandate); m != nil && (m.RestrictObjects || len(m.AllowedFileIDs) > 0) && !slices.Contains(m.AllowedFileIDs, id) {
		return fmt.Errorf("execution scope: file %d is not authorized", id)
	}
	return nil
}

type taskExportGrantKey struct{}

// WithTaskExportGrant records that the host (server configuration, never the
// model or a Planner mandate) lets this execution submit whole-task exports.
// Intent-bound export submissions without it are rejected; a mandate can only
// narrow the grant further (allowed_job_kinds, allowed_template_ids).
func WithTaskExportGrant(ctx context.Context) context.Context {
	return context.WithValue(ctx, taskExportGrantKey{}, true)
}

// TaskExportGranted reports whether the host granted whole-task export.
func TaskExportGranted(ctx context.Context) bool {
	ok, _ := ctx.Value(taskExportGrantKey{}).(bool)
	return ok
}

// restrictsObjects reports whether the host scope limits reads to granted objects.
func restrictsObjects(ctx context.Context) bool {
	m, _ := ctx.Value(executionScopeKey{}).(*IntentMandate)
	return m != nil && m.RestrictObjects
}
