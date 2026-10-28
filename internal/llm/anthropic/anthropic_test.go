package anthropic

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

const sse = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":120,"output_tokens":1,"cache_read_input_tokens":100,"cache_creation_input_tokens":20}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-1"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"核对资质"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"record_fact","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"summary\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"ok\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":42}}

event: message_stop
data: {"type":"message_stop"}

`

type captured struct {
	body   map[string]any
	header http.Header
}

func server(t *testing.T, status int, respBody string, headers map[string]string, cap *captured) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if cap != nil {
			_ = json.Unmarshal(raw, &cap.body)
			cap.header = r.Header.Clone()
		}
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		fmt.Fprint(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func request() llm.CompletionRequest {
	sys := "静态角色说明\n动态意图"
	return llm.CompletionRequest{
		System:          sys,
		DynamicBoundary: strings.Index(sys, "动态"),
		Messages: []llm.Message{
			llm.UserText("开始"),
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				{Type: llm.BlockThinking, Signature: "prev-sig"},
				{Type: llm.BlockToolUse, ID: "toolu_0", Name: "list_assets", Input: json.RawMessage(`{}`)},
			}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolUseID: "toolu_0", Text: "[]"}}},
		},
		Tools: []llm.ToolSpec{{Name: "record_fact", Description: "d", InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{"summary": map[string]any{"type": "string"}},
			"required": []string{"summary"}, "additionalProperties": false,
		}}},
	}
}

func TestStreamRequestShapeAndEvents(t *testing.T) {
	cap := &captured{}
	srv := server(t, 200, sse, map[string]string{"Content-Type": "text/event-stream"}, cap)
	p := New(Config{APIKey: "test-key", BaseURL: srv.URL, Effort: "high"})

	acc := llm.NewAccumulator()
	commits := 0
	for ev, err := range p.Stream(context.Background(), request()) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		if llm.CommitsOutput(ev) {
			commits++
		}
		acc.Add(ev)
	}
	msg, stop, usage := acc.Result()

	if stop != "tool_use" || usage.InputTokens != 120 || usage.OutputTokens != 42 || usage.CacheReadTokens != 100 {
		t.Fatalf("stop=%s usage=%+v", stop, usage)
	}
	if len(msg.Content) != 3 || msg.Content[0].Type != llm.BlockThinking || msg.Content[0].Signature != "sig-1" {
		t.Fatalf("thinking block not preserved: %+v", msg.Content)
	}
	if msg.Text() != "核对资质" || string(msg.ToolUses()[0].Input) != `{"summary":"ok"}` {
		t.Fatalf("text=%q tool=%s", msg.Text(), msg.ToolUses()[0].Input)
	}
	if commits == 0 {
		t.Fatal("expected committing events")
	}

	b := cap.body
	if b["model"] != DefaultModel || b["fallbacks"] != "default" || b["max_tokens"].(float64) != defaultStreamMaxTokens {
		t.Fatalf("model/fallbacks/max_tokens: %v %v %v", b["model"], b["fallbacks"], b["max_tokens"])
	}
	if !strings.Contains(cap.header.Get("anthropic-beta"), refusalFallbackBeta) || cap.header.Get("x-api-key") != "test-key" {
		t.Fatalf("headers: %v", cap.header)
	}
	if b["output_config"].(map[string]any)["effort"] != "high" || b["cache_control"] == nil {
		t.Fatalf("effort/cache_control: %v", b)
	}
	sys := b["system"].([]any)
	if len(sys) != 2 || sys[0].(map[string]any)["cache_control"] == nil || sys[1].(map[string]any)["cache_control"] != nil {
		t.Fatalf("system split: %v", sys)
	}
	tool := b["tools"].([]any)[0].(map[string]any)
	schema := tool["input_schema"].(map[string]any)
	if tool["eager_input_streaming"] != true || schema["additionalProperties"] != false {
		t.Fatalf("tool: %v", tool)
	}
	hist := b["messages"].([]any)[1].(map[string]any)["content"].([]any)
	if th := hist[0].(map[string]any); th["type"] != "thinking" || th["signature"] != "prev-sig" {
		t.Fatalf("thinking not replayed unchanged: %v", th)
	}
}

func TestErrorMapping(t *testing.T) {
	srv := server(t, 400, `{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API."}}`,
		map[string]string{"Content-Type": "application/json"}, nil)
	_, _, _, err := New(Config{APIKey: "k", BaseURL: srv.URL}).Complete(context.Background(), request())
	if !llm.IsQuotaExhausted(err) || llm.IsRetryable(err) {
		t.Fatalf("credit error must be quota and not retryable: %v", err)
	}

	srv = server(t, 429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`,
		map[string]string{"Content-Type": "application/json", "retry-after": "7"}, nil)
	_, _, _, err = New(Config{APIKey: "k", BaseURL: srv.URL}).Complete(context.Background(), request())
	ae, ok := err.(*llm.APIError)
	if !ok || ae.StatusCode != 429 || ae.Type != "rate_limit_error" || ae.RetryAfter.Seconds() != 7 {
		t.Fatalf("429 mapping: %#v", err)
	}
	if llm.IsQuotaExhausted(err) || !llm.IsRetryable(err) || !llm.ShouldFailover(err) {
		t.Fatalf("429 classification wrong: %v", err)
	}
}

func TestRefusalBecomesNonRetryableError(t *testing.T) {
	body := strings.Replace(sse, `"stop_reason":"tool_use","stop_sequence":null}`,
		`"stop_reason":"refusal","stop_sequence":null,"stop_details":{"type":"refusal","category":"cyber","explanation":"x"}}`, 1)
	srv := server(t, 200, body, map[string]string{"Content-Type": "text/event-stream"}, nil)
	var last error
	for _, err := range New(Config{APIKey: "k", BaseURL: srv.URL}).Stream(context.Background(), request()) {
		last = err
	}
	if last == nil || llm.IsRetryable(last) || llm.ShouldFailover(last) {
		t.Fatalf("refusal: %v", last)
	}
}

func TestCustomHTTPClientAndUnsignedThinkingDropped(t *testing.T) {
	cap := &captured{}
	srv := server(t, 200, sse, map[string]string{"Content-Type": "text/event-stream"}, cap)
	var used int
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		used++
		return http.DefaultTransport.RoundTrip(r)
	})}
	req := request()
	req.Messages[1].Content[0].Signature = "" // 来自 OpenAI 兼容端的推理块
	for _, err := range New(Config{APIKey: "k", BaseURL: srv.URL, HTTPClient: client}).Stream(context.Background(), req) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if used != 1 {
		t.Fatalf("custom http client used %d times, want exactly 1 (SDK retries must be off)", used)
	}
	hist := cap.body["messages"].([]any)[1].(map[string]any)["content"].([]any)
	if len(hist) != 1 || hist[0].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("unsigned thinking must be dropped: %v", hist)
	}
}

func TestSDKRetriesDisabled(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(529)
		fmt.Fprint(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	}))
	defer srv.Close()
	_, _, _, err := New(Config{APIKey: "k", BaseURL: srv.URL}).Complete(context.Background(), request())
	if hits != 1 || !llm.IsRetryable(err) {
		t.Fatalf("hits=%d err=%v; SDK must not retry 529 itself", hits, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
