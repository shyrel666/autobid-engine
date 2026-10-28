// Package tool 定义工具契约（附录 A.22 CoreTool）、注册表、Todo 与延迟披露工具。
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/autobid/autobid-engine/internal/permission"
)

type CoreTool interface {
	Name() string
	Description() string
	Prompt() string
	InputSchema() map[string]any
	IsReadOnly(input json.RawMessage) bool
	IsConcurrencySafe(input json.RawMessage) bool
	CheckPermissions(ctx context.Context, input json.RawMessage, pc permission.Context) permission.Decision
	Call(ctx context.Context, input json.RawMessage, tc *ToolContext) (Result, error)
}

// ToolContext 是 harness 传给单次工具调用的执行上下文。
type ToolContext struct {
	SessionID  string
	ToolUseID  string
	Turn       int
	Settling   bool
	Permission permission.Context
}

type Result struct {
	Content string
	IsError bool
}

func Text(s string) Result { return Result{Content: s} }

func Errorf(format string, args ...any) Result {
	return Result{Content: fmt.Sprintf(format, args...), IsError: true}
}

// JSON 将 v 序列化为结果正文；序列化失败时返回错误结果。
func JSON(v any) Result {
	raw, err := json.Marshal(v)
	if err != nil {
		return Errorf("marshal result: %v", err)
	}
	return Text(string(raw))
}

// Spec 描述一个以函数实现的工具；ReadOnly/Concurrent 默认 false（写工具不与同会话其他工具并行）。
type Spec struct {
	Name        string
	Description string
	Prompt      string
	Schema      map[string]any
	ReadOnly    bool
	Concurrent  bool
	Permissions func(ctx context.Context, input json.RawMessage, pc permission.Context) permission.Decision
}

type funcTool struct {
	spec Spec
	fn   func(ctx context.Context, input json.RawMessage, tc *ToolContext) (Result, error)
}

// New 以函数构造 CoreTool。
func New(spec Spec, fn func(ctx context.Context, input json.RawMessage, tc *ToolContext) (Result, error)) CoreTool {
	if spec.Schema == nil {
		spec.Schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return &funcTool{spec: spec, fn: fn}
}

func (t *funcTool) Name() string                           { return t.spec.Name }
func (t *funcTool) Description() string                    { return t.spec.Description }
func (t *funcTool) Prompt() string                         { return t.spec.Prompt }
func (t *funcTool) InputSchema() map[string]any            { return t.spec.Schema }
func (t *funcTool) IsReadOnly(json.RawMessage) bool        { return t.spec.ReadOnly }
func (t *funcTool) IsConcurrencySafe(json.RawMessage) bool { return t.spec.Concurrent }
func (t *funcTool) Call(ctx context.Context, in json.RawMessage, tc *ToolContext) (Result, error) {
	return t.fn(ctx, in, tc)
}
func (t *funcTool) CheckPermissions(ctx context.Context, in json.RawMessage, pc permission.Context) permission.Decision {
	if t.spec.Permissions == nil {
		return permission.Allow()
	}
	return t.spec.Permissions(ctx, in, pc)
}

// Registry 按名称索引工具。名称冲突时后注册者覆盖。
type Registry struct {
	tools map[string]CoreTool
	order []string
}

func NewRegistry(tools ...CoreTool) *Registry {
	r := &Registry{tools: map[string]CoreTool{}}
	for _, t := range tools {
		r.Register(t)
	}
	return r
}

func (r *Registry) Register(t CoreTool) {
	if t == nil {
		return
	}
	if _, ok := r.tools[t.Name()]; !ok {
		r.order = append(r.order, t.Name())
	}
	r.tools[t.Name()] = t
}

func (r *Registry) Get(name string) (CoreTool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// List 按注册顺序返回工具。
func (r *Registry) List() []CoreTool {
	out := make([]CoreTool, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.tools[n])
	}
	return out
}

// ValidateInput 做轻量 Schema 校验：输入须为 JSON 对象，required 字段齐全，
// 顶层 properties 的标量 type 匹配。嵌套结构交由工具自身解析。
func ValidateInput(schema map[string]any, input json.RawMessage) error {
	var obj map[string]any
	if err := json.Unmarshal(input, &obj); err != nil {
		return fmt.Errorf("input must be a JSON object: %v", err)
	}
	if obj == nil {
		return fmt.Errorf("input must be a JSON object")
	}
	if req, ok := schema["required"].([]any); ok {
		for _, r := range req {
			if name, ok := r.(string); ok {
				if _, present := obj[name]; !present {
					return fmt.Errorf("missing required field %q", name)
				}
			}
		}
	} else if req, ok := schema["required"].([]string); ok {
		for _, name := range req {
			if _, present := obj[name]; !present {
				return fmt.Errorf("missing required field %q", name)
			}
		}
	}
	props, _ := schema["properties"].(map[string]any)
	names := make([]string, 0, len(obj))
	for k := range obj {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		p, ok := props[k].(map[string]any)
		if !ok {
			continue
		}
		want, _ := p["type"].(string)
		if want != "" && obj[k] != nil && !jsonTypeMatches(want, obj[k]) {
			return fmt.Errorf("field %q must be %s", k, want)
		}
	}
	return nil
}

func jsonTypeMatches(want string, v any) bool {
	switch want {
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == float64(int64(f))
	case "array":
		_, ok := v.([]any)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	}
	return true
}
