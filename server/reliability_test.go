package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/agent"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/llm"
)

// A model can ignore the Stop nudge and answer "done" again. The host must
// still reject completion; the language of the answer is not a deliverable.
func TestIntentCompletionCannotIgnoreWritebackGate(t *testing.T) {
	pg := testDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m := NewManager(pg)
	task, err := m.CreateTask(ctx, db.CreateTaskInput{Name: fmt.Sprintf("completion-%d", time.Now().UnixNano()), Goal: "核验完成条件"}, "")
	if err != nil {
		t.Fatal(err)
	}
	iid, err := task.Store.AddNode(ctx, db.KindIntent, db.StateIntentOpen, map[string]any{"summary": "produce evidence", "mandate": db.IntentMandate{RequireWriteback: true}}, 1, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	epoch, ok, err := task.Store.ClaimIntentWithLease(ctx, iid, "worker", time.Minute)
	if err != nil || !ok {
		t.Fatal(err)
	}
	n, err := task.Store.GetNode(ctx, iid)
	if err != nil {
		t.Fatal(err)
	}
	worker := agent.NewWorker(agent.WorkerConfig{Provider: brain(func(llm.CompletionRequest) llm.Message { return say("全部完成") }), Transcripts: db.NewTranscriptStore(pg)})
	e := NewEngine(m, nil, Config{ModelErrorRetries: -1, LeaseTTL: time.Minute})
	defer e.StopAll()
	e.runIntent(ctx, task, "worker", worker, n, epoch, "", "")
	n, err = task.Store.GetNode(ctx, iid)
	if err != nil || n.State != db.StateIntentBlocked {
		t.Fatalf("unproven completion accepted: %+v %v", n, err)
	}
}
