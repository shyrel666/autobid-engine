package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/autobid/autobid-engine/agent"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/llm"
	"github.com/autobid/autobid-engine/internal/tool"
)

func TestWatchdogMandatePreToolUseEnforcement(t *testing.T) {
	mandate := &db.IntentMandate{
		AllowedSectionIDs:     []int64{101, 102},
		AllowedRequirementIDs: []int64{501},
		TargetWordsMin:        10,
		TargetWordsMax:        30,
		MaxToolCalls:          2,
		ForbiddenTools:        []string{"delete_everything"},
		RequireWriteback:      true,
	}
	w := newWatchdogHooks(nil, 42, "work#1", mandate, WatchdogConfig{}, nil, nil, nil)
	ctx := context.Background()

	// 1. Forbidden tool blocked (even when wrapped via ExecuteExtraTool)
	deferredInput, _ := json.Marshal(map[string]any{
		"name":  "delete_everything",
		"input": map[string]any{},
	})
	if block, reason, _ := w.PreToolUse(ctx, tool.ExecuteExtraToolName, deferredInput); !block || !strings.Contains(reason, "Mandate 边界拦截") {
		t.Fatalf("expected forbidden tool to be blocked, got block=%v reason=%q", block, reason)
	}

	// 2. Out-of-scope section_id blocked
	badSecInput, _ := json.Marshal(map[string]any{
		"section_id": 999,
		"content_md": "这是一段长度完全符合区间要求的技术方案正文内容。",
	})
	if block, reason, _ := w.PreToolUse(ctx, "section_write_draft", badSecInput); !block || !strings.Contains(reason, "section_id") {
		t.Fatalf("expected out-of-scope section_id to be blocked, got block=%v reason=%q", block, reason)
	}

	// 3. Word count below TargetWordsMin blocked
	shortInput, _ := json.Marshal(map[string]any{
		"section_id": 101,
		"content_md": "太短了",
	})
	if block, reason, _ := w.PreToolUse(ctx, "section_write_draft", shortInput); !block || !strings.Contains(reason, "低于 Mandate 要求下限") {
		t.Fatalf("expected short draft to be blocked, got block=%v reason=%q", block, reason)
	}

	// 4. Valid section_id and word count allowed
	goodSecInput, _ := json.Marshal(map[string]any{
		"section_id": 101,
		"content_md": "这是一段长度完全符合区间要求的技术方案正文内容。",
	})
	if block, reason, _ := w.PreToolUse(ctx, "section_write_draft", goodSecInput); block {
		t.Fatalf("expected valid section_write_draft to be allowed, got reason=%q", reason)
	}

	// 5. Out-of-scope requirement_id blocked
	badReqInput, _ := json.Marshal(map[string]any{
		"requirement_id": 888,
	})
	if block, reason, _ := w.PreToolUse(ctx, "requirement_bind_evidence", badReqInput); !block || !strings.Contains(reason, "requirement_id") {
		t.Fatalf("expected out-of-scope requirement_id to be blocked, got block=%v reason=%q", block, reason)
	}

	// 6. MaxToolCalls blocks read-only tools once reached, but still allows writeback tools
	w.PostToolUse(ctx, "graph_overview", []byte(`{}`), []byte(`{}`), false)
	w.PostToolUse(ctx, "node_detail", []byte(`{"id":1}`), []byte(`{}`), false)

	if block, reason, _ := w.PreToolUse(ctx, "read_chunk", []byte(`{"chunk_id":1}`)); !block || !strings.Contains(reason, "工具调用已达上限") {
		t.Fatalf("expected read-only tool after MaxToolCalls to be blocked, got block=%v reason=%q", block, reason)
	}
	if block, reason, _ := w.PreToolUse(ctx, agent.ToolRecordFact, []byte(`{"summary":"ok"}`)); block {
		t.Fatalf("expected writeback tool after MaxToolCalls to be allowed, got reason=%q", reason)
	}
}

func TestWatchdogRepeatCallAutoSteerAndKill(t *testing.T) {
	var steered []string
	var killed error
	var activities []db.Activity

	w := newWatchdogHooks(
		nil,
		77,
		"work#1",
		nil,
		WatchdogConfig{
			RepeatCallSteerThreshold: 3,
			RepeatCallKillThreshold:  5,
		},
		func(msg string) { steered = append(steered, msg) },
		func(cause error) { killed = cause },
		func(a db.Activity) { activities = append(activities, a) },
	)
	ctx := context.Background()

	// Call 1 & 2 (different whitespace, same canonical JSON)
	w.PostToolUse(ctx, "read_chunk", []byte(`{"chunk_id": 10, "before": 1}`), []byte(`ok`), false)
	w.PostToolUse(ctx, "read_chunk", []byte(`{"before":1,"chunk_id":10}`), []byte(`ok`), false)
	if len(steered) != 0 || killed != nil {
		t.Fatalf("unexpected early trigger: steered=%v killed=%v", steered, killed)
	}

	// Call 3 -> Auto-Steer
	w.PostToolUse(ctx, "read_chunk", []byte(`{"chunk_id":10,"before":1}`), []byte(`ok`), false)
	if len(steered) != 1 || killed != nil {
		t.Fatalf("expected 1 auto-steer at threshold 3, got steered=%v killed=%v", steered, killed)
	}

	// Call 4 -> No duplicate steer, no kill yet
	w.PostToolUse(ctx, "read_chunk", []byte(`{"chunk_id":10,"before":1}`), []byte(`ok`), false)
	if len(steered) != 1 || killed != nil {
		t.Fatalf("unexpected state at call 4: steered=%v killed=%v", steered, killed)
	}

	// Call 5 -> Circuit break with AbortKilledByWatchdog
	w.PostToolUse(ctx, "read_chunk", []byte(`{"chunk_id":10,"before":1}`), []byte(`ok`), false)
	if !errors.Is(killed, agent.AbortKilledByWatchdog) {
		t.Fatalf("expected AbortKilledByWatchdog at call 5, got %v", killed)
	}
	if len(activities) < 2 {
		t.Fatalf("expected watchdog activities to be emitted, got %d", len(activities))
	}
}

func TestWatchdogRequireWritebackStopGate(t *testing.T) {
	mandate := &db.IntentMandate{RequireWriteback: true}
	w := newWatchdogHooks(nil, 88, "work#1", mandate, WatchdogConfig{}, nil, nil, nil)
	ctx := context.Background()

	// Stopping before any writeback returns a blocking nudge on first attempt
	_, blocking, _ := w.Stop(ctx, []llm.Message{llm.UserText("done")})
	if len(blocking) != 1 || !strings.Contains(blocking[0], "require_writeback") {
		t.Fatalf("expected require_writeback blocking error on Stop, got %v", blocking)
	}

	// Once writeback tool succeeds, Stop passes cleanly
	w2 := newWatchdogHooks(nil, 89, "work#1", mandate, WatchdogConfig{}, nil, nil, nil)
	w2.PostToolUse(ctx, agent.ToolRecordFact, []byte(`{"summary":"verified"}`), []byte(`{"fact_id":1}`), false)
	if _, blocking2, _ := w2.Stop(ctx, nil); len(blocking2) != 0 {
		t.Fatalf("expected Stop to pass after writeback, got %v", blocking2)
	}
}

func TestEngineRouteHintPassthrough(t *testing.T) {
	m := NewManager(nil)
	var emitted []db.Activity
	e := NewEngine(m, nil, Config{
		OnActivity: func(a db.Activity) { emitted = append(emitted, a) },
	})
	task := &Task{ID: 10}

	// Register two running intents on task 10: intent 201 anchored to [11, 12], intent 202 anchored to [99]
	e.registerWork(201, func(error) {}, task.ID, []int64{11, 12})
	e.registerWork(202, func(error) {}, task.ID, []int64{99})
	defer func() {
		e.detachWork(201)
		e.detachWork(202)
	}()

	routed := e.RouteHintToRunningWork(task, 500, "注意补充最新的二级等保测评报告编号", []int64{12, 77})
	if len(routed) != 1 || routed[0] != 201 {
		t.Fatalf("expected hint to route only to intent 201, got %v", routed)
	}
	msg, ok := e.drainSteer(201)
	if !ok || !strings.Contains(msg, "Hint #500") || !strings.Contains(msg, "二级等保测评报告编号") {
		t.Fatalf("expected steerBox[201] to contain passthrough hint, got ok=%v msg=%q", ok, msg)
	}
	if _, ok2 := e.drainSteer(202); ok2 {
		t.Fatalf("expected intent 202 steerBox to remain empty")
	}
	if len(emitted) != 1 || emitted[0].Kind != "passthrough" {
		t.Fatalf("expected passthrough activity, got %+v", emitted)
	}
}
