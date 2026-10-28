package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/autobid/autobid-engine/internal/permission"
)

// SkillToolName 是 Skill 工具名；agentcore.SeedUnlockFromHistory 按此名扫描历史恢复解锁集。
const SkillToolName = "Skill"

// Skill 是一个可按需加载的技能：调用时返回指令正文，并解锁其关联的延迟工具名（§4.3 阶段 B）。
type Skill struct {
	Name        string
	Description string // 出现在 Skill 工具描述的技能目录中
	Body        string // 调用时返回的指令正文
	Tools       []string
}

// SkillSet 绑定一组技能与本次组装实例的 UnlockSet。
type SkillSet struct {
	mu       sync.Mutex
	skills   map[string]Skill
	order    []string
	unlock   *UnlockSet
	onInvoke func(skillName string) []string
}

// NewSkillSet 创建技能集合。onInvoke 返回该技能要解锁的工具名（宿主可按 MCP 连接动态解析）；
// 为 nil 时使用 Skill.Tools。
func NewSkillSet(skills []Skill, unlock *UnlockSet, onInvoke func(skillName string) []string) *SkillSet {
	s := &SkillSet{skills: map[string]Skill{}, unlock: unlock, onInvoke: onInvoke}
	for _, sk := range skills {
		if sk.Name == "" {
			continue
		}
		if _, dup := s.skills[sk.Name]; !dup {
			s.order = append(s.order, sk.Name)
		}
		s.skills[sk.Name] = sk
	}
	sort.Strings(s.order)
	return s
}

// Unlock 解锁技能关联的工具名，返回本次新解锁的名字（已解锁的不重复返回）。未知技能返回 nil。
func (s *SkillSet) Unlock(skillName string) []string {
	s.mu.Lock()
	sk, ok := s.skills[skillName]
	s.mu.Unlock()
	if !ok || s.unlock == nil {
		return nil
	}
	names := sk.Tools
	if s.onInvoke != nil {
		names = s.onInvoke(skillName)
	}
	var fresh []string
	for _, n := range names {
		if n != "" && !s.unlock.IsUnlocked(n) {
			fresh = append(fresh, n)
		}
	}
	s.unlock.Add(names...)
	return fresh
}

// Tool 返回模型可调用的 Skill 工具。
func (s *SkillSet) Tool() CoreTool { return &skillTool{set: s} }

type skillTool struct{ set *SkillSet }

func (t *skillTool) Name() string { return SkillToolName }

func (t *skillTool) Description() string {
	var b strings.Builder
	b.WriteString("Load a skill: returns its instructions and unlocks the deferred tools it needs. Available skills:\n")
	for _, name := range t.set.order {
		fmt.Fprintf(&b, "- %s: %s\n", name, t.set.skills[name].Description)
	}
	return b.String()
}

func (t *skillTool) Prompt() string { return "" }

func (t *skillTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string", "enum": t.set.order, "description": "skill name"},
		},
		"required": []string{"name"},
	}
}

func (t *skillTool) IsReadOnly(json.RawMessage) bool        { return true }
func (t *skillTool) IsConcurrencySafe(json.RawMessage) bool { return false } // 修改 UnlockSet
func (t *skillTool) CheckPermissions(context.Context, json.RawMessage, permission.Context) permission.Decision {
	return permission.Allow()
}

func (t *skillTool) Call(_ context.Context, input json.RawMessage, _ *ToolContext) (Result, error) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return Errorf("invalid input: %v", err), nil
	}
	t.set.mu.Lock()
	sk, ok := t.set.skills[req.Name]
	t.set.mu.Unlock()
	if !ok {
		return Errorf("unknown skill %q; available: %s", req.Name, strings.Join(t.set.order, ", ")), nil
	}
	fresh := t.set.Unlock(req.Name)
	out := sk.Body
	if block := RenderDeferredToolsBlock(fresh); block != "" {
		out += "\n\n" + block
	}
	return Text(out), nil
}
