package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/autobid/autobid-engine/internal/llm"
)

func TestResponsesToolRoundtripAndSSE(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("wrong request endpoint/auth")
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		reasoning := `{"id":"rs_1","type":"reasoning","encrypted_content":"opaque","summary":[{"type":"summary_text","text":"plan"}]}`
		call := `{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"id\":1}"}`
		final := `{"status":"completed","output":[` + reasoning + `,` + call + `],"usage":{"input_tokens":9,"output_tokens":4,"input_tokens_details":{"cached_tokens":2}}}`
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":%s}\n\ndata: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":%s}\n\ndata: {\"type\":\"response.completed\",\"response\":%s}\n\n", reasoning, call, final)
		} else {
			fmt.Fprint(w, final)
		}
	}))
	defer srv.Close()
	p := NewResponses(Config{APIKey: "fixture", BaseURL: srv.URL + "/v1", Model: "test", ReasoningEffort: "xhigh", MaxTokens: 5000})
	req := llm.CompletionRequest{System: "rules", Messages: []llm.Message{llm.UserText("hello")}, Tools: []llm.ToolSpec{{Name: "lookup", InputSchema: map[string]any{"type": "object"}}}}
	msg, stop, u, err := p.Complete(context.Background(), req)
	if err != nil || stop != llm.StopToolUse || u.CacheReadTokens != 2 || len(msg.ToolUses()) != 1 {
		t.Fatalf("complete: %v %s %+v %+v", err, stop, u, msg)
	}
	if body["max_output_tokens"] != float64(5000) || body["reasoning"].(map[string]any)["effort"] != "xhigh" || body["store"] != false {
		t.Fatalf("body %v", body)
	}
	req.Messages = append(req.Messages, msg, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolUseID: "call_1", Text: "found"}}})
	acc := llm.NewAccumulator()
	for ev, e := range p.Stream(context.Background(), req) {
		if e != nil {
			t.Fatal(e)
		}
		acc.Add(ev)
	}
	got, stop, u := acc.Result()
	if len(got.ToolUses()) != 1 || stop != llm.StopToolUse || u.InputTokens != 9 {
		t.Fatalf("duplicate/lost tool: %+v", got)
	}
	raw, _ := json.Marshal(body["input"])
	if !strings.Contains(string(raw), "encrypted_content") || !strings.Contains(string(raw), "function_call_output") {
		t.Fatalf("lost roundtrip data: %s", raw)
	}
}
func TestResponsesBrokenStreamFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
	}))
	defer srv.Close()
	p := NewResponses(Config{BaseURL: srv.URL, Model: "test"})
	var last error
	for _, err := range p.Stream(context.Background(), llm.CompletionRequest{}) {
		if err != nil {
			last = err
		}
	}
	if last == nil {
		t.Fatal("unterminated stream treated as success")
	}
}

func TestChatBrokenStreamFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
	}))
	defer srv.Close()
	var last error
	for _, err := range New(Config{BaseURL: srv.URL, Model: "fixture"}).Stream(context.Background(), llm.CompletionRequest{}) {
		if err != nil {
			last = err
		}
	}
	if last == nil {
		t.Fatal("unterminated Chat stream treated as success")
	}
}
func TestChatIndependentThinkingAndMaxField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		if _, ok := b["max_tokens"]; ok {
			t.Error("wrong max field")
		}
		if b["max_completion_tokens"] != float64(8192) || b["reasoning_effort"] != "max" || b["thinking"].(map[string]any)["type"] != "disabled" || b["stream"] != false {
			t.Errorf("body: %v", b)
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	_, _, _, err := New(Config{BaseURL: srv.URL, Model: "test", MaxTokens: 8192, MaxTokensField: "max_completion_tokens", ReasoningEffort: "max", ThinkingType: "disabled"}).Complete(context.Background(), llm.CompletionRequest{})
	if err != nil {
		t.Fatal(err)
	}
}
