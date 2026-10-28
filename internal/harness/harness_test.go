package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/internal/harness"
	"github.com/autobid/autobid-engine/internal/llm"
	"github.com/autobid/autobid-engine/internal/tool"
)

// scripted 按顺序回放预设的 assistant 消息，并记录每次请求。
type scripted struct {
	mu       sync.Mutex
	replies  []llm.Message
	requests []llm.CompletionRequest
}

func (s *scripted) next(req llm.CompletionRequest) (llm.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, req)
	if len(s.replies) == 0 {
		return llm.Message{}, errors.New("script exhausted")
	}
	m := s.replies[0]
	s.replies = s.replies[1:]
	return m, nil
}

func (s *scripted) Complete(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	m, err := s.next(req)
	return m, "", llm.Usage{OutputTokens: 1}, err
}

func (s *scripted) Stream(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		m, err := s.next(req)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		if !yield(llm.StreamEvent{Type: llm.SEMessageStart}, nil) {
			return
		}
		for _, b := range m.Content {
			var evs []llm.StreamEvent
			switch b.Type {
			case llm.BlockText:
				evs = []llm.StreamEvent{{Type: llm.SETextDelta, Text: b.Text}}
			case llm.BlockToolUse:
				in := string(b.Input)
				half := len(in) / 2
				evs = []llm.StreamEvent{
					{Type: llm.SEToolUseStart, ToolUse: &llm.ToolUse{ID: b.ID, Name: b.Name}},
					{Type: llm.SEToolUseDelta, Text: in[:half]},
					{Type: llm.SEToolUseDelta, Text: in[half:]},
					{Type: llm.SEToolUseEnd},
				}
			}
			for _, ev := range evs {
				if !yield(ev, nil) {
					return
				}
			}
		}
		yield(llm.StreamEvent{Type: llm.SEMessageStop, Usage: &llm.Usage{OutputTokens: 1}}, nil)
	}
}

func (s *scripted) toolNamesAt(i int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, t := range s.requests[i].Tools {
		out = append(out, t.Name)
	}
	return out
}

func call(id, name, input string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(input)},
	}}
}

func say(text string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockText, Text: text}}}
}

type recorder struct {
	mu    sync.Mutex
	calls []string
	ctxs  map[string]context.Context
}

func (r *recorder) tool(name string, fn func(ctx context.Context) (tool.Result, error)) tool.CoreTool {
	return tool.New(tool.Spec{Name: name, Description: name}, func(ctx context.Context, _ json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
		r.mu.Lock()
		r.calls = append(r.calls, name)
		if r.ctxs == nil {
			r.ctxs = map[string]context.Context{}
		}
		r.ctxs[name] = ctx
		r.mu.Unlock()
		if fn != nil {
			return fn(ctx)
		}
		return tool.Text(name + " ok"), nil
	})
}

func toolResults(t *harness.Terminal) []llm.ContentBlock {
	var out []llm.ContentBlock
	for _, m := range t.Messages {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolResult {
				out = append(out, b)
			}
		}
	}
	return out
}

// MaxTurns 触发收尾：收尾提示以 user 追加；Schema 收缩；禁用/非白名单工具在 Dispatch 被硬拦截；返回原始 ReasonMaxTurns。
func TestSettlementOnMaxTurnsGatesTools(t *testing.T) {
	prov := &scripted{replies: []llm.Message{
		call("t1", "probe", `{}`),
		call("t2", "Bash", `{}`),
		call("t3", "probe", `{}`),
		call("t4", "record_fact", `{}`),
		say("summary"),
	}}
	rec := &recorder{}
	opts := harness.Options{
		Provider: prov,
		Tools:    []tool.CoreTool{rec.tool("probe", nil), rec.tool("Bash", nil), rec.tool("record_fact", nil)},
		MaxTurns: 1,
		Settlement: &harness.Settlement{
			Prompt:           "wrap up now",
			MaxTurns:         5,
			DisabledTools:    []string{"Bash"},
			AllowedTools:     []string{"record_fact"},
			WritebackTimeout: 5 * time.Second,
		},
	}
	term := harness.Run(context.Background(), opts, []llm.Message{llm.UserText("go")}, nil)

	if term.Reason != harness.ReasonMaxTurns {
		t.Fatalf("reason = %s, want max_turns (err=%v)", term.Reason, term.Err)
	}
	if got := rec.calls; !slices.Equal(got, []string{"probe", "record_fact"}) {
		t.Fatalf("executed tools = %v, want [probe record_fact]", got)
	}
	if got := prov.toolNamesAt(1); !slices.Equal(got, []string{"record_fact"}) {
		t.Fatalf("settlement-visible tools = %v, want [record_fact]", got)
	}
	res := toolResults(term)
	if len(res) != 4 || !res[1].IsError || !res[2].IsError || res[3].IsError {
		t.Fatalf("unexpected tool results: %+v", res)
	}
	if !strings.Contains(res[1].Text, "disabled") || !strings.Contains(res[2].Text, "allowlist") {
		t.Fatalf("gate messages: %q / %q", res[1].Text, res[2].Text)
	}
	// 收尾提示并入 probe 的 tool_result 所在 user 消息。
	if !strings.Contains(term.Messages[2].Text(), "wrap up now") {
		t.Fatalf("settlement prompt not appended as user message: %+v", term.Messages[2])
	}
}

// Patch ①：工具因 MaxDuration 中断后，收尾写回工具拿到基于父 Context 的新预算，而非已耗尽的旧 toolCtx。
func TestSettlementOnToolTimeoutGetsFreshWritebackContext(t *testing.T) {
	prov := &scripted{replies: []llm.Message{
		call("t1", "slow", `{}`),
		call("t2", "record_fact", `{}`),
		say("done"),
	}}
	rec := &recorder{}
	var writebackDeadline time.Time
	var writebackErr error
	opts := harness.Options{
		Provider: prov,
		Tools: []tool.CoreTool{
			rec.tool("slow", func(ctx context.Context) (tool.Result, error) {
				<-ctx.Done()
				return tool.Result{}, ctx.Err()
			}),
			rec.tool("record_fact", func(ctx context.Context) (tool.Result, error) {
				writebackDeadline, _ = ctx.Deadline()
				writebackErr = ctx.Err()
				return tool.Text("saved"), nil
			}),
		},
		MaxDuration: 50 * time.Millisecond,
		Settlement:  &harness.Settlement{Prompt: "save findings", WritebackTimeout: 2 * time.Second},
	}
	start := time.Now()
	term := harness.Run(context.Background(), opts, []llm.Message{llm.UserText("go")}, nil)

	if term.Reason != harness.ReasonTimeout {
		t.Fatalf("reason = %s, want timeout (err=%v)", term.Reason, term.Err)
	}
	if writebackErr != nil {
		t.Fatalf("write-back context already done: %v", writebackErr)
	}
	if writebackDeadline.Sub(start) < time.Second {
		t.Fatalf("write-back deadline %v not derived from WritebackTimeout", writebackDeadline.Sub(start))
	}
}

// Patch ①：从回合边界（MaxTurns）进入收尾时同样替换 toolCtx。
func TestSettlementFromTurnBoundaryResetsToolContext(t *testing.T) {
	prov := &scripted{replies: []llm.Message{
		call("t1", "probe", `{}`),
		call("t2", "record_fact", `{}`),
		say("done"),
	}}
	rec := &recorder{}
	opts := harness.Options{
		Provider:    prov,
		Tools:       []tool.CoreTool{rec.tool("probe", nil), rec.tool("record_fact", nil)},
		MaxTurns:    1,
		MaxDuration: time.Hour,
		Settlement:  &harness.Settlement{Prompt: "save", WritebackTimeout: 3 * time.Second},
	}
	term := harness.Run(context.Background(), opts, []llm.Message{llm.UserText("go")}, nil)
	if term.Reason != harness.ReasonMaxTurns {
		t.Fatalf("reason = %s", term.Reason)
	}
	probeDL, _ := rec.ctxs["probe"].Deadline()
	writeDL, _ := rec.ctxs["record_fact"].Deadline()
	if !writeDL.Before(probeDL) || time.Until(writeDL) > 3*time.Second {
		t.Fatalf("write-back deadline %v should come from WritebackTimeout, not MaxDuration %v", writeDL, probeDL)
	}
	if rec.ctxs["probe"].Err() == nil {
		t.Fatalf("old tool context should be cancelled when settlement begins")
	}
}

// Patch ③：未解锁的延迟工具既不能按名直调，也不能经 SearchExtraTools 看到 Schema；解锁后两者放行。
func TestDeferredToolUnlockGate(t *testing.T) {
	rec := &recorder{}
	secret := rec.tool("tender_lookup", nil)
	unlock := tool.NewUnlockSet(nil)
	search := tool.NewSearchExtraTools([]tool.CoreTool{secret}, unlock)
	prov := &scripted{replies: []llm.Message{
		call("t1", "tender_lookup", `{}`),
		call("t2", tool.SearchExtraToolsName, `{"query":"tender"}`),
		say("stuck"),
	}}
	opts := harness.Options{
		Provider:      prov,
		Tools:         []tool.CoreTool{search},
		DeferredTools: []tool.CoreTool{secret},
		UnlockSet:     unlock,
	}
	term := harness.Run(context.Background(), opts, []llm.Message{llm.UserText("go")}, nil)
	res := toolResults(term)
	if len(rec.calls) != 0 || !res[0].IsError || !strings.Contains(res[0].Text, "locked") {
		t.Fatalf("locked deferred tool executed or not denied: calls=%v res=%+v", rec.calls, res[0])
	}
	if strings.Contains(res[1].Text, "input_schema") || !strings.Contains(res[1].Text, "tender_lookup") {
		t.Fatalf("search leaked schema of locked tool: %s", res[1].Text)
	}
	if slices.Contains(prov.toolNamesAt(0), "tender_lookup") {
		t.Fatalf("deferred tool schema must not be sent to the model")
	}

	unlock.Add("tender_lookup")
	prov.replies = []llm.Message{
		call("t3", tool.SearchExtraToolsName, `{"query":"select:tender_lookup"}`),
		call("t4", "tender_lookup", `{}`),
		say("ok"),
	}
	term = harness.Run(context.Background(), opts, []llm.Message{llm.UserText("go")}, nil)
	res = toolResults(term)
	if res[0].IsError || !strings.Contains(res[0].Text, "input_schema") || res[1].IsError {
		t.Fatalf("unlocked deferred tool not usable: %+v", res)
	}
}

type nudgeHooks struct{ stops int }

func (h *nudgeHooks) PreToolUse(context.Context, string, []byte) (bool, string, []byte) {
	return false, "", nil
}
func (h *nudgeHooks) PostToolUse(context.Context, string, []byte, []byte, bool) {}
func (h *nudgeHooks) Stop(context.Context, []llm.Message) (bool, []string, string) {
	h.stops++
	return false, []string{"keep going"}, ""
}

// stopHookActive：连续空转最多一次续跑；真实工具回合后才重置。
func TestStopHookNudgesOncePerIdleChain(t *testing.T) {
	rec := &recorder{}
	hooks := &nudgeHooks{}
	prov := &scripted{replies: []llm.Message{
		say("thinking..."),
		say("still idle"),
	}}
	opts := harness.Options{Provider: prov, Tools: []tool.CoreTool{rec.tool("probe", nil)}, Hooks: hooks}
	term := harness.Run(context.Background(), opts, []llm.Message{llm.UserText("go")}, nil)
	if term.Reason != harness.ReasonCompleted || hooks.stops != 1 {
		t.Fatalf("reason=%s stops=%d, want completed after exactly one nudge", term.Reason, hooks.stops)
	}

	hooks.stops = 0
	prov.replies = []llm.Message{say("idle"), call("t1", "probe", `{}`), say("idle again"), say("final")}
	term = harness.Run(context.Background(), opts, []llm.Message{llm.UserText("go")}, nil)
	if term.Reason != harness.ReasonCompleted || hooks.stops != 2 {
		t.Fatalf("reason=%s stops=%d, want a second nudge after a real tool turn", term.Reason, hooks.stops)
	}
}

// 父 Context 取消时直接结束，不进入收尾。
func TestParentCancelAbortsWithoutSettlement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := &recorder{}
	prov := &scripted{replies: []llm.Message{call("t1", "kill", `{}`), say("never")}}
	opts := harness.Options{
		Provider: prov,
		Tools: []tool.CoreTool{rec.tool("kill", func(context.Context) (tool.Result, error) {
			cancel()
			return tool.Text("bye"), nil
		})},
		Settlement: &harness.Settlement{Prompt: "save"},
	}
	term := harness.Run(ctx, opts, []llm.Message{llm.UserText("go")}, nil)
	if term.Reason != harness.ReasonAbortedTools {
		t.Fatalf("reason = %s, want aborted_tools", term.Reason)
	}
}

func TestModelErrorReason(t *testing.T) {
	prov := &scripted{}
	term := harness.Run(context.Background(), harness.Options{Provider: prov}, []llm.Message{llm.UserText("go")}, nil)
	if term.Reason != harness.ReasonModelError || term.Err == nil {
		t.Fatalf("reason=%s err=%v, want model_error", term.Reason, term.Err)
	}
}
