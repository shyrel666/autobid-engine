package tool

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"github.com/autobid/autobid-engine/internal/permission"
)

// UnlockSet 记录当前组装实例中已解锁的延迟工具名（§4.3 阶段 B）。它是内存集合，不是持久 ACL。
type UnlockSet struct {
	mu    sync.RWMutex
	names map[string]bool
}

func NewUnlockSet(initial []string) *UnlockSet {
	s := &UnlockSet{names: make(map[string]bool, len(initial))}
	s.Add(initial...)
	return s
}

func (s *UnlockSet) Add(names ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range names {
		if n != "" {
			s.names[n] = true
		}
	}
}

func (s *UnlockSet) IsUnlocked(name string) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.names[name]
}

// Has 是 IsUnlocked 的别名，对齐蓝图原文调用点。
func (s *UnlockSet) Has(name string) bool { return s.IsUnlocked(name) }

// Names 返回已解锁名称（排序后）。
func (s *UnlockSet) Names() []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	out := make([]string, 0, len(s.names))
	for n := range s.names {
		out = append(out, n)
	}
	s.mu.RUnlock()
	sort.Strings(out)
	return out
}

const (
	SearchExtraToolsName = "SearchExtraTools"
	ExecuteExtraToolName = "ExecuteExtraTool"
)

// RenderDeferredToolsBlock 渲染延迟工具名目录：只披露名字，Schema 需经 SearchExtraTools 按需获取。
func RenderDeferredToolsBlock(names []string) string {
	if len(names) == 0 {
		return ""
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	var b strings.Builder
	b.WriteString("<deferred-tools>\n")
	b.WriteString("The following tools are available but their schemas are not loaded. ")
	b.WriteString("Call " + SearchExtraToolsName + " with a keyword or \"select:<name>[,<name>...]\" to load a schema before calling the tool.\n")
	for _, n := range sorted {
		b.WriteString("- " + n + "\n")
	}
	b.WriteString("</deferred-tools>")
	return b.String()
}

type searchExtraTools struct {
	deferred []CoreTool
	unlock   *UnlockSet
}

// NewSearchExtraTools 构造延迟工具 Schema 检索工具。
// Patch ③：只返回已在 unlock 中解锁的工具；unlock 为 nil 时不披露任何延迟工具。
func NewSearchExtraTools(deferred []CoreTool, unlock *UnlockSet) CoreTool {
	return &searchExtraTools{deferred: deferred, unlock: unlock}
}

func (s *searchExtraTools) Name() string { return SearchExtraToolsName }
func (s *searchExtraTools) Description() string {
	return "Load the description and input schema of unlocked deferred tools by keyword, or exactly with \"select:<name>[,<name>...]\"."
}
func (s *searchExtraTools) Prompt() string { return "" }
func (s *searchExtraTools) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "keywords, or select:<name>[,<name>...]"},
		},
		"required": []string{"query"},
	}
}
func (s *searchExtraTools) IsReadOnly(json.RawMessage) bool        { return true }
func (s *searchExtraTools) IsConcurrencySafe(json.RawMessage) bool { return true }
func (s *searchExtraTools) CheckPermissions(context.Context, json.RawMessage, permission.Context) permission.Decision {
	return permission.Allow()
}

func (s *searchExtraTools) Call(_ context.Context, input json.RawMessage, _ *ToolContext) (Result, error) {
	var req struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return Errorf("invalid search input: %v", err), nil
	}
	match := keywordMatcher(req.Query)
	matched := []map[string]any{}
	var locked []string
	for _, t := range s.deferred {
		if !match(t) {
			continue
		}
		// 强制门禁：未通过全局披露或 Skill 解锁的延迟工具，禁止暴露 Schema
		if !s.unlock.IsUnlocked(t.Name()) {
			locked = append(locked, t.Name())
			continue
		}
		matched = append(matched, map[string]any{
			"name":         t.Name(),
			"description":  t.Description(),
			"input_schema": t.InputSchema(),
		})
	}
	out := map[string]any{"tools": matched}
	if len(locked) > 0 {
		out["locked"] = locked
		out["note"] = "locked tools require invoking their owning Skill first"
	}
	return JSON(out), nil
}

// keywordMatcher 支持 "select:a,b" 精确选择，其余按空白切分的关键词对名称与描述做不区分大小写的全部匹配。
func keywordMatcher(query string) func(CoreTool) bool {
	q := strings.TrimSpace(query)
	if rest, ok := strings.CutPrefix(q, "select:"); ok {
		want := map[string]bool{}
		for _, n := range strings.Split(rest, ",") {
			want[strings.TrimSpace(n)] = true
		}
		return func(t CoreTool) bool { return want[t.Name()] }
	}
	terms := strings.Fields(strings.ToLower(q))
	return func(t CoreTool) bool {
		hay := strings.ToLower(t.Name() + " " + t.Description())
		for _, term := range terms {
			if !strings.Contains(hay, term) {
				return false
			}
		}
		return true
	}
}

type executeExtraTool struct {
	deferred map[string]CoreTool
	unlock   *UnlockSet
}

// NewExecuteExtraTool 构造延迟工具的间接执行入口：解析目标名与参数 -> 校验解锁 -> 验参 -> 自检权限 -> 调用。
// harness 同样允许模型按名直调已解锁的延迟工具，两条路径共用同一 UnlockSet 门禁。
func NewExecuteExtraTool(deferred []CoreTool, unlock *UnlockSet) CoreTool {
	m := make(map[string]CoreTool, len(deferred))
	for _, t := range deferred {
		m[t.Name()] = t
	}
	return &executeExtraTool{deferred: m, unlock: unlock}
}

func (e *executeExtraTool) Name() string { return ExecuteExtraToolName }
func (e *executeExtraTool) Description() string {
	return "Execute an unlocked deferred tool by name with the given input object."
}
func (e *executeExtraTool) Prompt() string { return "" }
func (e *executeExtraTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":  map[string]any{"type": "string"},
			"input": map[string]any{"type": "object"},
		},
		"required": []string{"name", "input"},
	}
}

func (e *executeExtraTool) target(input json.RawMessage) (CoreTool, json.RawMessage, bool) {
	var req struct {
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(input, &req) != nil {
		return nil, nil, false
	}
	t, ok := e.deferred[req.Name]
	return t, req.Input, ok
}

func (e *executeExtraTool) IsReadOnly(input json.RawMessage) bool {
	t, in, ok := e.target(input)
	return ok && t.IsReadOnly(in)
}

func (e *executeExtraTool) IsConcurrencySafe(input json.RawMessage) bool {
	t, in, ok := e.target(input)
	return ok && t.IsConcurrencySafe(in)
}

func (e *executeExtraTool) CheckPermissions(ctx context.Context, input json.RawMessage, pc permission.Context) permission.Decision {
	t, in, ok := e.target(input)
	if !ok {
		return permission.Allow() // 由 Call 返回具体错误
	}
	return t.CheckPermissions(ctx, in, pc)
}

func (e *executeExtraTool) Call(ctx context.Context, input json.RawMessage, tc *ToolContext) (Result, error) {
	var req struct {
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return Errorf("invalid input: %v", err), nil
	}
	t, ok := e.deferred[req.Name]
	if !ok {
		return Errorf("unknown deferred tool %q", req.Name), nil
	}
	if !e.unlock.IsUnlocked(req.Name) {
		return Errorf("permission denied: deferred tool %q is locked; invoke its owning Skill first", req.Name), nil
	}
	if len(req.Input) == 0 {
		req.Input = json.RawMessage("{}")
	}
	if err := ValidateInput(t.InputSchema(), req.Input); err != nil {
		return Errorf("invalid input for %s: %v", req.Name, err), nil
	}
	return t.Call(ctx, req.Input, tc)
}

// IsDeferredWrapper 报告工具是否为延迟工具的间接执行入口，并返回其目标名（供 Hook 按真实工具名审计）。
func IsDeferredWrapper(name string, input json.RawMessage) (string, bool) {
	if name != ExecuteExtraToolName {
		return "", false
	}
	var req struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(input, &req) != nil || req.Name == "" {
		return "", false
	}
	return req.Name, true
}

// UnwrapDeferred 解包 ExecuteExtraTool 的目标工具名与内层入参；非包装调用或未携带合法目标名时原样返回。
func UnwrapDeferred(name string, input json.RawMessage) (string, json.RawMessage) {
	if name != ExecuteExtraToolName {
		return name, input
	}
	var req struct {
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(input, &req) != nil || req.Name == "" {
		return name, input
	}
	if len(req.Input) == 0 {
		req.Input = json.RawMessage("{}")
	}
	return req.Name, req.Input
}
