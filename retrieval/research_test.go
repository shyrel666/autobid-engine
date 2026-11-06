package retrieval

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
)

type fakeReasoner struct {
	calls    atomic.Int32
	plan     string
	decision string
}

func (f *fakeReasoner) Decide(ctx context.Context, prompt string, planner bool) (string, int, error) {
	f.calls.Add(1)
	if planner {
		return f.plan, 10, nil
	}
	return f.decision, 10, nil
}

func TestResearchIdempotencyAndDisabledMulti(t *testing.T) {
	s, tenant, id := fixture(t)
	ctx := context.Background()
	c := &Coordinator{Service: s}
	req := ResearchRequest{Key: "idempotency", Question: "恢复时间不超过30秒", Query: db.KnowledgeQuery{MustTerms: []string{"X-1", "恢复时间"}}, Claim: &Claim{Text: "恢复时间不超过30秒", Parameter: "恢复时间", Operator: "<=", Value: 30, Unit: "秒", Product: "X-1"}}
	r, err := c.Run(ctx, tenant, 0, "test", req)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "completed" || len(r.Jobs) != 2 {
		t.Fatalf("%+v", r)
	}
	before, _ := s.DB.GetRetrievalRun(ctx, tenant, r.RunID)
	repeated, err := (&Coordinator{Service: New(s.DB, nil, nil)}).Run(ctx, tenant, 0, "test", req)
	if err != nil || repeated.RunID != r.RunID {
		t.Fatalf("replay %+v %v", repeated, err)
	}
	after, _ := s.DB.GetRetrievalRun(ctx, tenant, r.RunID)
	if before.UsedCalls != after.UsedCalls {
		t.Fatal("replay spent budget")
	}
	req.Question = "different"
	if _, err = c.Run(ctx, tenant, 0, "test", req); !errors.Is(err, db.ErrRetrievalConflict) {
		t.Fatal("idempotency conflict", err)
	}
	req.Multi = true
	if _, err = c.Run(ctx, tenant, 0, "test", req); !errors.Is(err, db.ErrRetrievalConflict) {
		t.Fatal("multi not gated", err)
	}
	if err = s.DB.DeleteKnowledgeDocument(ctx, tenant, id); err != nil {
		t.Fatal(err)
	}
	stale, _ := s.DB.GetRetrievalRun(ctx, tenant, r.RunID)
	if stale.Status != "partial" {
		t.Fatal("completed run survived stale source")
	}
}

func TestConcurrentResearchSharedBudgetAndGates(t *testing.T) {
	s, tenant, _ := fixture(t)
	ctx := context.Background()
	model := &fakeReasoner{plan: `{"facets":[{"query":"恢复时间","domain":"knowledge","role":"product"},{"query":"质保期","domain":"knowledge","role":"solution"},{"query":"不存在的独立范围","domain":"knowledge","role":"other"}]}`, decision: `{"next_query":"","gap":"needs review"}`}
	c := &Coordinator{Service: s, Reasoner: model, MultiEnabled: true, MaxCalls: 3, MaxTokens: 30000}
	req := ResearchRequest{Key: "concurrent", Question: "恢复和质保", Multi: true}
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.Run(ctx, tenant, 0, "test", req)
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	r, err := c.Run(ctx, tenant, 0, "test", req)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status == "completed" || r.Status == "running" || len(r.Gaps) == 0 || len(r.Jobs) != 4 {
		t.Fatalf("%+v", r)
	}
	run, _ := s.DB.GetRetrievalRun(ctx, tenant, r.RunID)
	if run.UsedCalls > 3 || run.ReservedTokens > 30000 {
		t.Fatalf("budget overflow %+v", run)
	}
}

type reasonerFunc func(context.Context, string, bool) (string, int, error)

func (f reasonerFunc) Decide(ctx context.Context, prompt string, planner bool) (string, int, error) {
	return f(ctx, prompt, planner)
}

// stagedRun stores the run Coordinator.Run would create for req, so a test can set its state first.
func stagedRun(t *testing.T, s *Service, tenant string, req ResearchRequest, deadline time.Time) *db.RetrievalRun {
	t.Helper()
	req.Query.TenantID = tenant
	req.Query.Limit = 8
	raw, _ := json.Marshal(req)
	run, _, err := s.DB.CreateRetrievalRun(context.Background(), db.RetrievalRun{TenantID: tenant, RequestKey: req.Key, Request: raw, Kind: "research", MaxCalls: 24, MaxTokens: 48000, Deadline: deadline}, "test")
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// holdRunKeyLock stalls EnsureRetrievalJobs, whose FOR UPDATE conflicts with FOR KEY SHARE, and
// nothing else: the budget, usage and finish writes on the run row do not touch its key.
func holdRunKeyLock(t *testing.T, s *Service, tenant, requestKey string) *sql.Tx {
	t.Helper()
	lock, err := s.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Rollback() })
	var id int64
	if err = lock.QueryRow(`SELECT id FROM retrieval_runs WHERE tenant_id=$1 AND request_key=$2 FOR KEY SHARE`, tenant, requestKey).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return lock
}

func awaitBlockedBy(t *testing.T, s *Service, lock *sql.Tx) {
	t.Helper()
	var pid int
	if err := lock.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		var blocked int
		if err := s.DB.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked > 0 {
			return
		}
	}
	t.Fatal("Run never reached EnsureRetrievalJobs")
}

// A replay that read the run as running must return the result when another request ends it.
func TestReplayReturnsRunEndedBeforePlanJobEnsured(t *testing.T) {
	s, tenant, _ := fixture(t)
	ctx := context.Background()
	c := &Coordinator{Service: s}
	req := ResearchRequest{Key: "late-plan", Question: "恢复时间"}
	run := stagedRun(t, s, tenant, req, time.Now().Add(time.Minute))
	lock := holdRunKeyLock(t, s, tenant, req.Key)
	type outcome struct {
		result ResearchResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		r, err := c.Run(ctx, tenant, 0, "test", req)
		done <- outcome{r, err}
	}()
	awaitBlockedBy(t, s, lock)
	if _, err := lock.Exec(`UPDATE retrieval_runs SET status='partial',finished_at=now() WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := lock.Commit(); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil || got.result.RunID != run.ID || got.result.Status != "partial" {
		t.Fatalf("%+v %v", got.result, got.err)
	}
	req.Question = "different"
	if _, err := c.Run(ctx, tenant, 0, "test", req); !errors.Is(err, db.ErrRetrievalConflict) {
		t.Fatal("same key with other parameters must stay a conflict", err)
	}
}

func TestRunEndedBeforeChildJobsEnsuredReturnsResult(t *testing.T) {
	s, tenant, _ := fixture(t)
	req := ResearchRequest{Key: "late-children", Question: "恢复和质保", Multi: true}
	c := &Coordinator{Service: s, MultiEnabled: true, Reasoner: reasonerFunc(func(ctx context.Context, _ string, _ bool) (string, int, error) {
		_, err := s.DB.ExecContext(ctx, `UPDATE retrieval_runs SET status='partial',finished_at=now() WHERE tenant_id=$1 AND request_key=$2`, tenant, req.Key)
		return `{"facets":[{"query":"恢复时间","domain":"knowledge","role":"product"}]}`, 10, err
	})}
	r, err := c.Run(context.Background(), tenant, 0, "test", req)
	if err != nil || r.Status != "partial" || len(r.Jobs) != 1 {
		t.Fatalf("%+v %v", r, err)
	}
}

// planOwnerBeforeChildren leaves the run where the plan owner stands between its two writes.
func planOwnerBeforeChildren(t *testing.T, s *Service, tenant string, run *db.RetrievalRun, req ResearchRequest) []json.RawMessage {
	t.Helper()
	ctx := context.Background()
	plan, _ := json.Marshal(Facet{Query: req.Question, Role: "planner"})
	if err := s.DB.EnsureRetrievalJobs(ctx, tenant, run.ID, []json.RawMessage{plan}); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.DB.RetrievalJobs(ctx, tenant, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.DB.ClaimRetrievalJob(ctx, tenant, jobs[0].ID, jobLease)
	if err != nil {
		t.Fatal(err)
	}
	facet := Facet{Query: req.Question, Domain: "knowledge", Role: "researcher"}
	if err = s.DB.FinishRetrievalJob(ctx, tenant, jobs[0].ID, token, "completed", []Facet{facet}); err != nil {
		t.Fatal(err)
	}
	child, _ := json.Marshal(facet)
	return []json.RawMessage{plan, child}
}

// A replay that read the plan as running can aggregate inside this gap. Ending the run there
// dropped the plan: the run closed with no evidence and the owner then failed with a conflict.
func TestReplayCannotEndRunBetweenPlanAndChildJobs(t *testing.T) {
	for _, tc := range []struct {
		name         string
		ownerStaysUp bool
	}{{"owner writes children", true}, {"owner crashed", false}} {
		t.Run(tc.name, func(t *testing.T) {
			s, tenant, _ := fixture(t)
			ctx := context.Background()
			c := &Coordinator{Service: s}
			req := ResearchRequest{Key: "plan-gap", Question: "恢复时间"}
			run := stagedRun(t, s, tenant, req, time.Now().Add(time.Minute))
			specs := planOwnerBeforeChildren(t, s, tenant, run, req)
			r, err := c.finish(ctx, run, req)
			if err != nil || r.Status != "running" {
				t.Fatalf("%+v %v", r, err)
			}
			other := req
			other.Question = "different"
			if _, err = c.Run(ctx, tenant, 0, "test", other); !errors.Is(err, db.ErrRetrievalConflict) {
				t.Fatal("same key with other parameters must stay a conflict", err)
			}
			if tc.ownerStaysUp {
				if err = s.DB.EnsureRetrievalJobs(ctx, tenant, run.ID, specs); err != nil {
					t.Fatal(err)
				}
			}
			r, err = c.Run(ctx, tenant, 0, "test", req)
			if err != nil || r.Status != "partial" || len(r.Jobs) != 2 || r.Jobs[1].Status != "completed" {
				t.Fatalf("%+v %v", r, err)
			}
		})
	}
}

func TestDeadlineEndsRunStuckBetweenPlanAndChildJobs(t *testing.T) {
	s, tenant, _ := fixture(t)
	req := ResearchRequest{Key: "plan-gap-deadline", Question: "恢复时间"}
	run := stagedRun(t, s, tenant, req, time.Now().Add(time.Minute))
	planOwnerBeforeChildren(t, s, tenant, run, req)
	if _, err := s.DB.Exec(`UPDATE retrieval_runs SET deadline=now()-interval '1 second' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	r, err := (&Coordinator{Service: s}).Run(context.Background(), tenant, 0, "test", req)
	if err != nil || r.Status != "budget_exhausted" {
		t.Fatalf("%+v %v", r, err)
	}
}

// The deadline expiring inside either ensure call ends the run the same way: not as a raw error.
func TestDeadlineWhileEnsuringJobsEndsRun(t *testing.T) {
	const window = 500 * time.Millisecond
	t.Run("plan", func(t *testing.T) {
		s, tenant, _ := fixture(t)
		req := ResearchRequest{Key: "deadline-plan", Question: "恢复时间"}
		stagedRun(t, s, tenant, req, time.Now().Add(window))
		holdRunKeyLock(t, s, tenant, req.Key)
		r, err := (&Coordinator{Service: s}).Run(context.Background(), tenant, 0, "test", req)
		if err != nil || r.Status != "budget_exhausted" {
			t.Fatalf("%+v %v", r, err)
		}
	})
	t.Run("children", func(t *testing.T) {
		s, tenant, _ := fixture(t)
		req := ResearchRequest{Key: "deadline-children", Question: "恢复和质保", Multi: true}
		// Planning passed the first ensure, so the lock can only stall the second one.
		c := &Coordinator{Service: s, MultiEnabled: true, Duration: window, Reasoner: reasonerFunc(func(context.Context, string, bool) (string, int, error) {
			holdRunKeyLock(t, s, tenant, req.Key)
			return `{"facets":[{"query":"恢复时间","domain":"knowledge","role":"product"}]}`, 10, nil
		})}
		r, err := c.Run(context.Background(), tenant, 0, "test", req)
		if err != nil || r.Status != "budget_exhausted" {
			t.Fatalf("%+v %v", r, err)
		}
	})
}

func TestRestartRecoveryAndCancellation(t *testing.T) {
	s, tenant, _ := fixture(t)
	ctx := context.Background()
	req := ResearchRequest{Key: "restart", Question: "恢复时间", Query: db.KnowledgeQuery{TenantID: tenant, Limit: 8}}
	raw, _ := json.Marshal(req)
	run, _, err := s.DB.CreateRetrievalRun(ctx, db.RetrievalRun{TenantID: tenant, RequestKey: req.Key, Request: raw, Kind: "research", MaxCalls: 24, MaxTokens: 48000, Deadline: time.Now().Add(time.Minute)}, "test")
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(Facet{Query: req.Question, Role: "planner"})
	if err = s.DB.EnsureRetrievalJobs(ctx, tenant, run.ID, []json.RawMessage{spec}); err != nil {
		t.Fatal(err)
	}
	jobs, _ := s.DB.RetrievalJobs(ctx, tenant, run.ID)
	old, err := s.DB.ClaimRetrievalJob(ctx, tenant, jobs[0].ID, jobLease)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(`UPDATE retrieval_jobs SET lease_until=now()-interval '1 second' WHERE id=$1`, jobs[0].ID); err != nil {
		t.Fatal(err)
	}
	recovered, err := (&Coordinator{Service: New(s.DB, nil, nil)}).Run(ctx, tenant, 0, "test", req)
	if err != nil || recovered.Status != "partial" || len(recovered.Jobs) != 2 {
		t.Fatalf("%+v %v", recovered, err)
	}
	if err = s.DB.FinishRetrievalJob(ctx, tenant, jobs[0].ID, old, "completed", nil); !errors.Is(err, db.ErrLeaseLost) {
		t.Fatal("old process wrote", err)
	}
	cancelled, _, err := s.DB.CreateRetrievalRun(ctx, db.RetrievalRun{TenantID: tenant, RequestKey: "cancel", Request: raw, Kind: "research", MaxCalls: 1, MaxTokens: 100, Deadline: time.Now().Add(time.Minute)}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DB.CancelRetrievalRun(ctx, tenant, cancelled.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.ReserveRetrieval(ctx, tenant, cancelled.ID, 1, 1); !errors.Is(err, db.ErrRetrievalBudget) {
		t.Fatal("cancelled run spent", err)
	}
}

func TestSourceRestrictionsAndTableAmbiguity(t *testing.T) {
	s := Snapshot{Document: &db.KnowledgeDocument{Category: db.KnowledgeProductSpec, Extra: json.RawMessage(`{"product":"X","valid_until":"2020-01-01"}`)}, Chunk: &db.KnowledgeChunk{Content: "恢复时间：30秒"}}
	claim := Claim{Text: "恢复时间≤30秒", Parameter: "恢复时间", Operator: "<=", Value: 30, Unit: "秒", Product: "X"}
	if VerifyNumeric(s, claim, time.Now()).Relation != "insufficient" {
		t.Fatal("expired")
	}
	s.Document.Extra = json.RawMessage(`{"product":"X"}`)
	s.Chunk.Content = "仅用于实验室\n恢复时间：30秒"
	if VerifyNumeric(s, claim, time.Now()).Relation != "insufficient" {
		t.Fatal("scope")
	}
	s.Chunk.Content = ""
	s.Chunk.Tables = json.RawMessage(`[{"grid":[["参数","X","Y"],["恢复时间","30秒","60秒"]]}]`)
	if VerifyNumeric(s, claim, time.Now()).Relation != "insufficient" {
		t.Fatal("ambiguous columns")
	}
}
