// Package agentcore 是 harness 之上的轻量会话封装：按稳定 Session ID 读写 transcript，
// 区分首轮输入与续跑输入，并从历史恢复 Skill 解锁集合（§2.6、§4.3）。
package agentcore

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/autobid/autobid-engine/internal/harness"
	"github.com/autobid/autobid-engine/internal/llm"
	"github.com/autobid/autobid-engine/internal/tool"
)

// TranscriptStore 持久化会话消息（不含 system）。Save 以整份会话覆盖。
type TranscriptStore interface {
	Load(ctx context.Context, sessionID string) ([]llm.Message, error)
	Save(ctx context.Context, sessionID string, messages []llm.Message) error
}

// MemoryStore 是进程内 TranscriptStore，用于测试与单实例开发。
type MemoryStore struct {
	mu   sync.Mutex
	data map[string][]llm.Message
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{data: map[string][]llm.Message{}} }

func (m *MemoryStore) Load(_ context.Context, id string) ([]llm.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]llm.Message(nil), m.data[id]...), nil
}

func (m *MemoryStore) Save(_ context.Context, id string, msgs []llm.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[id] = append([]llm.Message(nil), msgs...)
	return nil
}

type Session struct {
	ID      string
	store   TranscriptStore
	history []llm.Message
	loaded  bool
	loadErr error
}

func NewSession(id string, store TranscriptStore) *Session {
	return &Session{ID: id, store: store}
}

// Load 读取历史；读取失败记录在 LoadErr 中并按空历史继续（与原实现一致：稳定命名不等于存储故障可无损恢复）。
func (s *Session) Load(ctx context.Context) []llm.Message {
	if !s.loaded {
		s.loaded = true
		if s.store != nil {
			s.history, s.loadErr = s.store.Load(ctx, s.ID)
		}
	}
	return append([]llm.Message(nil), s.history...)
}

func (s *Session) LoadErr() error { return s.loadErr }

// HasHistory 报告是否存在可续跑的历史。
func (s *Session) HasHistory(ctx context.Context) bool { return len(s.Load(ctx)) > 0 }

// Resume 运行一轮：无历史时以 freshInput 开始，有历史时追加 continueInput（为空则不追加）。
// 无论终态如何都会保存会话；保存失败作为第二个返回值报告，不覆盖 Terminal.Err。
func (s *Session) Resume(ctx context.Context, opts harness.Options, freshInput, continueInput string, onEvent func(harness.Event)) (*harness.Terminal, error) {
	msgs := s.Load(ctx)
	input := freshInput
	if len(msgs) > 0 {
		input = continueInput
	}
	if input != "" {
		msgs = appendUser(msgs, input)
	}
	if opts.SessionID == "" {
		opts.SessionID = s.ID
	}
	t := harness.Run(ctx, opts, msgs, onEvent)
	if t == nil {
		return nil, fmt.Errorf("agentcore: session %s produced no terminal", s.ID)
	}
	s.history = t.Messages
	var saveErr error
	if s.store != nil {
		// 使用独立 Context：运行 Context 已被取消时仍尽量保留 transcript。
		saveErr = s.store.Save(context.WithoutCancel(ctx), s.ID, t.Messages)
	}
	return t, saveErr
}

// appendUser 追加 user 文本；末条为 user 时合并，避免连续同角色消息。
func appendUser(msgs []llm.Message, text string) []llm.Message {
	block := llm.ContentBlock{Type: llm.BlockText, Text: text}
	if n := len(msgs); n > 0 && msgs[n-1].Role == llm.RoleUser {
		last := msgs[n-1]
		last.Content = append(append([]llm.ContentBlock(nil), last.Content...), block)
		msgs[n-1] = last
		return msgs
	}
	return append(msgs, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{block}})
}

// SkillToolName 是 Skill 调用工具的约定名。
const SkillToolName = tool.SkillToolName

// InvokedSkills 扫描历史中名为 skillTool 的 tool_use，返回其 input.name（去重、保序）。
// 与原实现一致，不检查该次 Skill 调用是否成功；调用方据此对每个 Skill 重放 UnlockSkill。
func InvokedSkills(msgs []llm.Message, skillTool string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range msgs {
		if m.Role != llm.RoleAssistant {
			continue
		}
		for _, b := range m.ToolUses() {
			if b.Name != skillTool {
				continue
			}
			var in struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(b.Input, &in) != nil || in.Name == "" || seen[in.Name] {
				continue
			}
			seen[in.Name] = true
			out = append(out, in.Name)
		}
	}
	return out
}

// SeedUnlockFromHistory 对历史中调用过的每个 Skill 调用 unlockSkill，恢复本次组装实例的内存解锁集。
func SeedUnlockFromHistory(msgs []llm.Message, unlockSkill func(skillName string)) {
	if unlockSkill == nil {
		return
	}
	for _, name := range InvokedSkills(msgs, SkillToolName) {
		unlockSkill(name)
	}
}
