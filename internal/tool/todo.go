package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

type TodoStatus string

const (
	TodoPending    TodoStatus = "pending"
	TodoInProgress TodoStatus = "in_progress"
	TodoCompleted  TodoStatus = "completed"
)

type Todo struct {
	Content string     `json:"content"`
	Status  TodoStatus `json:"status"`
}

// TodoStore 是实例内存便签（§2.4）：Planner 跨轮保留尚未形成可执行 Intent 的链条，不持久化。
type TodoStore struct {
	mu    sync.Mutex
	items []Todo
}

func NewTodoStore() *TodoStore { return &TodoStore{} }

// Set 整体替换待办列表。
func (s *TodoStore) Set(items []Todo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append([]Todo(nil), items...)
}

func (s *TodoStore) Items() []Todo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Todo(nil), s.items...)
}

// Render 以 Markdown 清单输出未完成项与已完成项，供注入下一轮 user 输入；为空时返回 ""。
func (s *TodoStore) Render() string {
	items := s.Items()
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	for _, t := range items {
		mark := " "
		switch t.Status {
		case TodoInProgress:
			mark = "~"
		case TodoCompleted:
			mark = "x"
		}
		fmt.Fprintf(&b, "- [%s] %s\n", mark, t.Content)
	}
	return b.String()
}

const TodoWriteName = "TodoWrite"

// NewTodoWriteTool 返回整体覆写 TodoStore 的工具。
func NewTodoWriteTool(store *TodoStore) CoreTool {
	return New(Spec{
		Name:        TodoWriteName,
		Description: "Replace the whole todo list. Use it to keep multi-step chains that are not yet executable intents.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"todos": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"content": map[string]any{"type": "string"},
							"status":  map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed"}},
						},
						"required": []string{"content", "status"},
					},
				},
			},
			"required": []string{"todos"},
		},
	}, func(_ context.Context, input json.RawMessage, _ *ToolContext) (Result, error) {
		var req struct {
			Todos []Todo `json:"todos"`
		}
		if err := json.Unmarshal(input, &req); err != nil {
			return Errorf("invalid todos: %v", err), nil
		}
		for i, t := range req.Todos {
			if strings.TrimSpace(t.Content) == "" {
				return Errorf("todo %d has empty content", i), nil
			}
			switch t.Status {
			case TodoPending, TodoInProgress, TodoCompleted:
			default:
				return Errorf("todo %d has invalid status %q", i, t.Status), nil
			}
		}
		store.Set(req.Todos)
		return Text(fmt.Sprintf("todo list updated (%d items)", len(req.Todos))), nil
	})
}
