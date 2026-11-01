package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"

	"github.com/autobid/autobid-engine/internal/llm"
)

// Responses uses stateless requests. Opaque encrypted reasoning items are retained
// in the transcript and replayed with their associated function calls.
type Responses struct{ *Provider }

func NewResponses(cfg Config) *Responses { return &Responses{New(cfg)} }
func (p *Responses) request(ctx context.Context, req llm.CompletionRequest, stream bool) (*http.Request, error) {
	input := []any{}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			switch b.Type {
			case llm.BlockText:
				if b.Text != "" {
					input = append(input, map[string]any{"role": m.Role, "content": b.Text})
				}
			case llm.BlockToolUse:
				args := string(b.Input)
				if args == "" {
					args = "{}"
				}
				input = append(input, map[string]any{"type": "function_call", "call_id": b.ID, "name": b.Name, "arguments": args})
			case llm.BlockToolResult:
				input = append(input, map[string]any{"type": "function_call_output", "call_id": b.ToolUseID, "output": b.Text})
			case llm.BlockResponseItem:
				if json.Valid(b.Input) {
					input = append(input, json.RawMessage(b.Input))
				}
			}
		}
	}
	model := req.Model
	if model == "" {
		model = p.cfg.Model
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = p.cfg.MaxTokens
	}
	body := map[string]any{"model": model, "input": input, "stream": stream, "store": false, "include": []string{"reasoning.encrypted_content"}}
	if req.System != "" {
		body["instructions"] = req.System
	}
	if maxTokens > 0 {
		body["max_output_tokens"] = maxTokens
	}
	if p.cfg.ReasoningEffort != "" {
		body["reasoning"] = map[string]string{"effort": p.cfg.ReasoningEffort}
	}
	if p.cfg.ThinkingType != "" {
		body["thinking"] = map[string]string{"type": p.cfg.ThinkingType}
	}
	if len(req.Tools) > 0 {
		ts := []any{}
		for _, t := range req.Tools {
			ts = append(ts, map[string]any{"type": "function", "name": t.Name, "description": t.Description, "parameters": t.InputSchema, "strict": false})
		}
		body["tools"] = ts
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	hr, err := http.NewRequestWithContext(ctx, "POST", p.cfg.BaseURL+"/responses", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	if stream {
		hr.Header.Set("Accept", "text/event-stream")
	}
	if p.cfg.APIKey != "" {
		hr.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}
	return hr, nil
}

type responseItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Content   []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
	Summary []struct {
		Text string `json:"text"`
	} `json:"summary"`
}
type responseBody struct {
	Status     string            `json:"status"`
	Output     []json.RawMessage `json:"output"`
	Error      *apiErrorBody     `json:"error"`
	Incomplete struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Usage struct {
		Input   int `json:"input_tokens"`
		Output  int `json:"output_tokens"`
		Details struct {
			Cached int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
}

func (b responseBody) result() (llm.Message, string, llm.Usage, error) {
	msg := llm.Message{Role: llm.RoleAssistant}
	stop := llm.StopEndTurn
	u := llm.Usage{InputTokens: b.Usage.Input, OutputTokens: b.Usage.Output, CacheReadTokens: b.Usage.Details.Cached}
	if b.Error != nil {
		return msg, "", u, streamError(*b.Error)
	}
	if b.Status == "failed" || b.Status == "cancelled" {
		return msg, "", u, errors.New("Responses request " + b.Status)
	}
	if b.Status == "incomplete" {
		if b.Incomplete.Reason == "max_output_tokens" {
			stop = llm.StopMaxTokens
		} else {
			return msg, "", u, &llm.APIError{StatusCode: 400, Message: "Responses incomplete: " + b.Incomplete.Reason}
		}
	}
	for _, raw := range b.Output {
		var item responseItem
		if err := json.Unmarshal(raw, &item); err != nil {
			return msg, "", u, err
		}
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Type == "refusal" {
					return msg, "", u, &llm.APIError{StatusCode: 400, Type: "refusal", Message: c.Refusal}
				}
				if c.Text != "" {
					msg.Content = append(msg.Content, llm.ContentBlock{Type: llm.BlockText, Text: c.Text})
				}
			}
		case "function_call":
			if !json.Valid([]byte(item.Arguments)) {
				return msg, "", u, errors.New("invalid Responses tool arguments")
			}
			msg.Content = append(msg.Content, llm.ContentBlock{Type: llm.BlockToolUse, ID: item.CallID, Name: item.Name, Input: json.RawMessage(item.Arguments)})
			if stop != llm.StopMaxTokens {
				stop = llm.StopToolUse
			}
		case "reasoning":
			for _, s := range item.Summary {
				if s.Text != "" {
					msg.Content = append(msg.Content, llm.ContentBlock{Type: llm.BlockThinking, Text: s.Text})
				}
			}
			msg.Content = append(msg.Content, llm.ContentBlock{Type: llm.BlockResponseItem, Input: raw})
		}
	}
	return msg, stop, u, nil
}
func (p *Responses) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	hr, err := p.request(ctx, req, false)
	if err != nil {
		return llm.Message{}, "", llm.Usage{}, err
	}
	resp, err := p.client.Do(hr)
	if err != nil {
		return llm.Message{}, "", llm.Usage{}, transportErr(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return llm.Message{}, "", llm.Usage{}, httpError(resp)
	}
	var b responseBody
	if err = json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&b); err != nil {
		return llm.Message{}, "", llm.Usage{}, transportErr(ctx, err)
	}
	return b.result()
}
func (p *Responses) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		hr, err := p.request(ctx, req, true)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		resp, err := p.client.Do(hr)
		if err != nil {
			yield(llm.StreamEvent{}, transportErr(ctx, err))
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			yield(llm.StreamEvent{}, httpError(resp))
			return
		}
		if !yield(llm.StreamEvent{Type: llm.SEMessageStart}, nil) {
			return
		}
		textSeen := map[int]bool{}
		thinkingSeen := map[int]bool{}
		itemDone := map[int]bool{}
		emitItem := func(index int, raw json.RawMessage) bool {
			var item responseItem
			if json.Unmarshal(raw, &item) != nil {
				return false
			}
			switch item.Type {
			case "function_call":
				if !json.Valid([]byte(item.Arguments)) {
					yield(llm.StreamEvent{}, errors.New("invalid Responses tool arguments"))
					return false
				}
				if !yield(llm.StreamEvent{Type: llm.SEToolUseStart, ToolUse: &llm.ToolUse{ID: item.CallID, Name: item.Name}}, nil) {
					return false
				}
				if !yield(llm.StreamEvent{Type: llm.SEToolUseEnd, ToolUse: &llm.ToolUse{Input: json.RawMessage(item.Arguments)}}, nil) {
					return false
				}
			case "message":
				if !textSeen[index] {
					for _, c := range item.Content {
						if c.Type == "refusal" {
							yield(llm.StreamEvent{}, &llm.APIError{StatusCode: 400, Type: "refusal", Message: c.Refusal})
							return false
						}
						if c.Text != "" && !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: c.Text}, nil) {
							return false
						}
					}
				}
			case "reasoning":
				if !thinkingSeen[index] {
					for _, s := range item.Summary {
						if s.Text != "" && !yield(llm.StreamEvent{Type: llm.SEThinkingDelta, Text: s.Text}, nil) {
							return false
						}
					}
				}
				if !yield(llm.StreamEvent{Type: llm.SEResponseItem, Text: string(raw)}, nil) {
					return false
				}
			}
			itemDone[index] = true
			return true
		}
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64<<10), maxSSELine)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			data, ok := strings.CutPrefix(line, "data:")
			if !ok {
				continue
			}
			data = strings.TrimSpace(data)
			if data == "[DONE]" {
				break
			}
			var ev struct {
				Type     string          `json:"type"`
				Index    int             `json:"output_index"`
				Delta    string          `json:"delta"`
				Item     json.RawMessage `json:"item"`
				Response responseBody    `json:"response"`
				Error    *apiErrorBody   `json:"error"`
				Message  string          `json:"message"`
				Code     string          `json:"code"`
			}
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				yield(llm.StreamEvent{}, transportErr(ctx, err))
				return
			}
			switch ev.Type {
			case "response.output_text.delta":
				textSeen[ev.Index] = true
				if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: ev.Delta}, nil) {
					return
				}
			case "response.reasoning_summary_text.delta":
				thinkingSeen[ev.Index] = true
				if !yield(llm.StreamEvent{Type: llm.SEThinkingDelta, Text: ev.Delta}, nil) {
					return
				}
			case "response.output_item.done":
				if !itemDone[ev.Index] && !emitItem(ev.Index, ev.Item) {
					return
				}
			case "response.completed", "response.incomplete", "response.failed":
				_, stop, u, e := ev.Response.result()
				if e != nil {
					yield(llm.StreamEvent{}, e)
					return
				}
				for i, raw := range ev.Response.Output {
					if !itemDone[i] && !emitItem(i, raw) {
						return
					}
				}
				yield(llm.StreamEvent{Type: llm.SEMessageStop, Usage: &u, StopReason: stop}, nil)
				return
			case "error":
				e := ev.Error
				if e == nil {
					e = &apiErrorBody{Message: ev.Message, Code: ev.Code}
				}
				yield(llm.StreamEvent{}, streamError(*e))
				return
			}
		}
		if err := scanner.Err(); err != nil {
			yield(llm.StreamEvent{}, transportErr(ctx, err))
		} else {
			yield(llm.StreamEvent{}, &llm.APIError{StatusCode: 502, Message: fmt.Sprint("Responses stream ended before completion")})
		}
	}
}
