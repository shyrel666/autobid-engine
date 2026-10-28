package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/autobid/autobid-engine/internal/llm"
)

func chunk(v string) string { return "data: " + v + "\n\n" }

var streamBody = chunk(`{"choices":[{"delta":{"role":"assistant","reasoning_content":"先看资质"}}]}`) +
	chunk(`{"choices":[{"delta":{"content":"核对中"}}]}`) +
	chunk(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"record_fact","arguments":"{\"summary\":"}}]}}]}`) +
	chunk(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ok\"}"}}]}}]}`) +
	chunk(`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"list_assets","arguments":"{}"}}]}}]}`) +
	chunk(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`) +
	chunk(`{"choices":[],"usage":{"prompt_tokens":200,"completion_tokens":30,"prompt_tokens_details":{"cached_tokens":150}}}`) +
	"data: [DONE]\n\n"

type captured struct {
	body   map[string]any
	header http.Header
	path   string
}

func newServer(t *testing.T, status int, body string, headers map[string]string, cap *captured) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if cap != nil {
			_ = json.Unmarshal(raw, &cap.body)
			cap.header, cap.path = r.Header.Clone(), r.URL.Path
		}
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func history() llm.CompletionRequest {
	return llm.CompletionRequest{
		System: "你是 Worker",
		Messages: []llm.Message{
			llm.UserText("开始"),
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				{Type: llm.BlockThinking, Text: "推理", Signature: "sig"},
				{Type: llm.BlockToolUse, ID: "toolu_1", Name: "list_assets", Input: json.RawMessage(`{"limit":5}`)},
			}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{
				{Type: llm.BlockToolResult, ToolUseID: "toolu_1", Text: "denied", IsError: true},
				{Type: llm.BlockText, Text: "【收尾】请写回"},
			}},
		},
		Tools: []llm.ToolSpec{{Name: "record_fact", Description: "d", InputSchema: map[string]any{"type": "object"}}},
	}
}

func TestStreamConversion(t *testing.T) {
	cap := &captured{}
	srv := newServer(t, 200, streamBody, map[string]string{"Content-Type": "text/event-stream"}, cap)
	p := New(Config{APIKey: "sk-test", BaseURL: srv.URL + "/v1/", Model: "deepseek-chat", ReasoningEffort: "high"})

	acc := llm.NewAccumulator()
	for ev, err := range p.Stream(context.Background(), history()) {
		if err != nil {
			t.Fatal(err)
		}
		acc.Add(ev)
	}
	msg, stop, usage := acc.Result()
	if stop != llm.StopToolUse || usage.InputTokens != 200 || usage.OutputTokens != 30 || usage.CacheReadTokens != 150 {
		t.Fatalf("stop=%s usage=%+v", stop, usage)
	}
	uses := msg.ToolUses()
	if len(uses) != 2 || string(uses[0].Input) != `{"summary":"ok"}` || uses[1].Name != "list_assets" || uses[1].ID != "call_b" {
		t.Fatalf("tool uses: %+v", uses)
	}
	if msg.Content[0].Type != llm.BlockThinking || msg.Text() != "核对中" {
		t.Fatalf("content: %+v", msg.Content)
	}

	if cap.path != "/v1/chat/completions" || cap.header.Get("Authorization") != "Bearer sk-test" {
		t.Fatalf("path=%s auth=%s", cap.path, cap.header.Get("Authorization"))
	}
	b := cap.body
	if b["model"] != "deepseek-chat" || b["stream"] != true || b["reasoning_effort"] != "high" || b["stream_options"] == nil {
		t.Fatalf("request: %v", b)
	}
	msgs := b["messages"].([]any)
	roles := []string{}
	for _, m := range msgs {
		roles = append(roles, m.(map[string]any)["role"].(string))
	}
	if strings.Join(roles, ",") != "system,user,assistant,tool,user" {
		t.Fatalf("roles = %v", roles)
	}
	asst := msgs[2].(map[string]any)
	if asst["content"] != nil || asst["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"] != `{"limit":5}` {
		t.Fatalf("assistant: %v", asst)
	}
	if tool := msgs[3].(map[string]any); tool["tool_call_id"] != "toolu_1" || !strings.HasPrefix(tool["content"].(string), "ERROR:") {
		t.Fatalf("tool message: %v", tool)
	}
	if strings.Contains(fmt.Sprint(b), "推理") {
		t.Fatal("thinking must not be sent to openai-compatible endpoints")
	}
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		status           int
		body             string
		quota, retryable bool
	}{
		{429, `{"error":{"message":"You exceeded your current quota","type":"insufficient_quota","code":"insufficient_quota"}}`, true, false},
		{429, `{"error":{"message":"Rate limit reached","type":"requests","code":"rate_limit_exceeded"}}`, false, true},
		{402, `{"error":{"message":"Insufficient Balance","type":"unknown_error"}}`, true, false},
		{503, `upstream overloaded`, false, true},
		{400, `{"error":{"message":"bad tools","type":"invalid_request_error"}}`, false, false},
	}
	for _, c := range cases {
		srv := newServer(t, c.status, c.body, map[string]string{"Content-Type": "application/json"}, nil)
		_, _, _, err := New(Config{BaseURL: srv.URL, Model: "m"}).Complete(context.Background(), history())
		if llm.IsQuotaExhausted(err) != c.quota || llm.IsRetryable(err) != c.retryable {
			t.Errorf("%d %s: quota=%v retryable=%v err=%v", c.status, c.body, llm.IsQuotaExhausted(err), llm.IsRetryable(err), err)
		}
	}
}

func TestStreamErrorObjectAndContentFilter(t *testing.T) {
	srv := newServer(t, 200, chunk(`{"error":{"message":"quota exceeded","type":"insufficient_quota"}}`), nil, nil)
	var last error
	for _, err := range New(Config{BaseURL: srv.URL, Model: "m"}).Stream(context.Background(), history()) {
		last = err
	}
	if !llm.IsQuotaExhausted(last) {
		t.Fatalf("mid-stream quota error: %v", last)
	}

	srv = newServer(t, 200, chunk(`{"choices":[{"delta":{"content":"x"},"finish_reason":"content_filter"}]}`)+"data: [DONE]\n\n", nil, nil)
	last = nil
	for _, err := range New(Config{BaseURL: srv.URL, Model: "m"}).Stream(context.Background(), history()) {
		last = err
	}
	if last == nil || llm.IsRetryable(last) || llm.ShouldFailover(last) {
		t.Fatalf("content filter: %v", last)
	}
}

func TestCompleteParsesToolCalls(t *testing.T) {
	body := `{"choices":[{"message":{"content":null,"reasoning_content":"r","tool_calls":[{"id":"c1","type":"function","function":{"name":"record_fact","arguments":""}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":9,"completion_tokens":3,"prompt_cache_hit_tokens":4}}`
	srv := newServer(t, 200, body, map[string]string{"Content-Type": "application/json"}, nil)
	msg, stop, usage, err := New(Config{BaseURL: srv.URL, Model: "m"}).Complete(context.Background(), history())
	if err != nil || stop != llm.StopToolUse || usage.CacheReadTokens != 4 {
		t.Fatalf("err=%v stop=%s usage=%+v", err, stop, usage)
	}
	if u := msg.ToolUses(); len(u) != 1 || string(u[0].Input) != "{}" {
		t.Fatalf("tool uses: %+v", u)
	}
}
