package server

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/llm"
)

func toolTurnPair(id int, payload string) (llm.Message, llm.Message) {
	uID := fmt.Sprintf("call_%d", id)
	ast := llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			{Type: llm.BlockToolUse, ID: uID, Name: "read_chunk", Input: []byte(fmt.Sprintf(`{"chunk_id":%d}`, id))},
		},
	}
	usr := llm.Message{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{
			{Type: llm.BlockToolResult, ToolUseID: uID, Text: payload},
		},
	}
	return ast, usr
}

func TestPrepareContextStage1SpillWithoutLLMCall(t *testing.T) {
	var baseCalls atomic.Int32
	base := brain(func(req llm.CompletionRequest) llm.Message {
		baseCalls.Add(1)
		return say("summary")
	})

	bigPayload := fmt.Sprintf(`{"chunk_id":42,"doc_id":7,"evidence_id":99,"content":%q}`, strings.Repeat("高价值招标技术参数段落正文", 200))
	msgs := []llm.Message{llm.UserText("请开始执行当前意图")}
	for i := 1; i <= 4; i++ {
		a, u := toolTurnPair(i, bigPayload)
		msgs = append(msgs, a, u)
	}
	req := llm.CompletionRequest{Messages: msgs}
	origTokens := estimateRequestTokens(req)

	// Set window so that origTokens >= window*3/4, but after spilling the first 2 tool results it drops below window*3/4.
	prov := &configuredProvider{
		base:    base,
		profile: &db.LLMProfile{WindowTokens: origTokens},
	}
	compacted, err := prov.prepareContext(context.Background(), req)
	if err != nil {
		t.Fatalf("prepareContext failed: %v", err)
	}
	if baseCalls.Load() != 0 {
		t.Fatalf("expected Stage 1 spill to avoid LLM summary call, got %d calls", baseCalls.Load())
	}
	// Earlier tool_result (index 2 and 4) must be spilled with recall_keys preserved
	firstToolRes := compacted.Messages[2].Content[0].Text
	if !strings.HasPrefix(firstToolRes, "[Spilled tool_result") || !strings.Contains(firstToolRes, "chunk_id=42") || !strings.Contains(firstToolRes, "evidence_id=99") {
		t.Fatalf("expected older tool_result to be spilled with recall keys, got %q", firstToolRes)
	}
	// Most recent 2 tool_result messages (index 6 and 8) must remain verbatim Inline
	lastToolRes := compacted.Messages[8].Content[0].Text
	if lastToolRes != bigPayload {
		t.Fatalf("expected recent tool_result to remain verbatim inline")
	}
}

func TestPrepareContextStage2PureToolChainCompactionNo413(t *testing.T) {
	var baseCalls atomic.Int32
	base := brain(func(req llm.CompletionRequest) llm.Message {
		baseCalls.Add(1)
		return say("Earlier turns verified chunk_id=1..3 and asset_id=10.")
	})

	// Build a pure Worker tool-call conversation (only Messages[0] has BlockText; turns 1..4 are Assistant(tool_use)->User(tool_result))
	medPayload := fmt.Sprintf(`{"chunk_id":11,"content":%q}`, strings.Repeat("段落内容", 120))
	msgs := []llm.Message{llm.UserText(strings.Repeat("初始态势与长提示词", 300))}
	for i := 1; i <= 4; i++ {
		a, u := toolTurnPair(i, medPayload)
		msgs = append(msgs, a, u)
	}
	req := llm.CompletionRequest{Messages: msgs}
	// Tight window so Stage 1 alone isn't enough (because Messages[0] itself is large), forcing Stage 2 compaction.
	prov := &configuredProvider{
		base:    base,
		profile: &db.LLMProfile{WindowTokens: 3200},
	}
	compacted, err := prov.prepareContext(context.Background(), req)
	if err != nil {
		t.Fatalf("expected Stage 2 turn-boundary compaction to succeed without 413, got err: %v", err)
	}
	if baseCalls.Load() != 1 {
		t.Fatalf("expected 1 LLM summary call in Stage 2, got %d", baseCalls.Load())
	}
	// Verify role sequence and preserved recent tool_use/tool_result pairs:
	// [0] RoleUser (summary) -> [1] RoleAssistant (tool_use) -> [2] RoleUser (tool_result) ...
	if len(compacted.Messages) < 3 {
		t.Fatalf("expected at least 3 messages after compaction, got %d", len(compacted.Messages))
	}
	if compacted.Messages[0].Role != llm.RoleUser || !strings.Contains(compacted.Messages[0].Text(), "Earlier conversation summary") {
		t.Fatalf("expected first message to be summary RoleUser, got %+v", compacted.Messages[0])
	}
	if compacted.Messages[1].Role != llm.RoleAssistant || len(compacted.Messages[1].ToolUses()) == 0 {
		t.Fatalf("expected second message to be RoleAssistant with intact tool_use, got %+v", compacted.Messages[1])
	}
	if compacted.Messages[2].Role != llm.RoleUser || compacted.Messages[2].Content[0].Type != llm.BlockToolResult {
		t.Fatalf("expected third message to be RoleUser with matching tool_result, got %+v", compacted.Messages[2])
	}
}
