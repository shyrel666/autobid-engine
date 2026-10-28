package retrieval

import (
	"context"
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
