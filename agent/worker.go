package agent

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/agentcore"
	"github.com/autobid/autobid-engine/internal/harness"
	"github.com/autobid/autobid-engine/internal/llm"
	"github.com/autobid/autobid-engine/internal/tool"
)

// DeferredInfo 是宿主装配的延迟披露信息（附录 A.6）。
type DeferredInfo struct {
	FindingGuidance string
	Deferred        []tool.CoreTool
	GlobalNames     []string
	Unlock          *tool.UnlockSet
	UnlockSkill     func(skillName string)
	// Skills 非空时自动装配 Skill 工具；OnInvoke 可按技能动态给出要解锁的工具名（为 nil 时用 Skill.Tools）。
	Skills   []tool.Skill
	OnInvoke func(skillName string) []string
}

// ToolAugment 由宿主注入（Skill / MCP / 自定义工具，附录 A.20）；cleanup 关闭本轮建立的连接。
var ToolAugment func(ctx context.Context, agentKey string) (extra []tool.CoreTool, def DeferredInfo, cleanup func())

type WorkerConfig struct {
	Provider    llm.Provider
	Model       string
	MaxTurns    int           // 默认 40
	RunTimeout  time.Duration // 单次运行预算，默认 15 分钟；仍受任务 deadline 夹逼
	MaxTokens   int
	Transcripts agentcore.TranscriptStore
	ExtraTools  []tool.CoreTool // 领域执行工具（如标书检索），与图写工具一起下发
	// 静态延迟披露配置，与 ToolAugment 的结果合并：Deferred 只注册不下发 Schema；
	// GlobalDeferredNames 初始即解锁并列入名字目录；其余须经 Skill 解锁（§4.3）。
	Deferred            []tool.CoreTool
	GlobalDeferredNames []string
	Skills              []tool.Skill
}

type Worker struct {
	cfg WorkerConfig
}

func NewWorker(cfg WorkerConfig) *Worker {
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = 40
	}
	if cfg.RunTimeout <= 0 {
		cfg.RunTimeout = 15 * time.Minute
	}
	return &Worker{cfg: cfg}
}

// WorkerSessionID 以 exploration ID + intent ID 稳定命名；工作槽换手不改变会话身份（§2.6）。
func WorkerSessionID(explorationID, intentID int64) string {
	return fmt.Sprintf("exp%d-worker-i%d", explorationID, intentID)
}

func (w *Worker) Execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), notifyFinding func(int64, string)) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, notifyFinding, "")
}

// ExecuteWithMessage 以人工消息作为续跑输入（paused→running 直投）。
func (w *Worker) ExecuteWithMessage(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	_ = requestID
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, notifyFinding, message)
}

func (w *Worker) execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), notifyFinding func(int64, string), message string) (harness.TerminalReason, WriteCounts, error) {
	set := newToolSet(as, ts, name, taskID, intent.ID)
	set.notifyFinding = notifyFinding

	tools := append(set.workerTools(), w.cfg.ExtraTools...)
	var def DeferredInfo
	if ToolAugment != nil {
		extra, d, cleanup := ToolAugment(ctx, AgentWorker)
		if cleanup != nil {
			defer cleanup()
		}
		tools = append(tools, extra...)
		def = d
	}
	def.Deferred = append(append([]tool.CoreTool(nil), def.Deferred...), w.cfg.Deferred...)
	def.GlobalNames = append(append([]string(nil), def.GlobalNames...), w.cfg.GlobalDeferredNames...)
	def.Skills = append(append([]tool.Skill(nil), def.Skills...), w.cfg.Skills...)
	tools, deferredBlock, unlock, def := assembleDeferred(tools, def)
	if def.FindingGuidance != "" {
		deferredBlock = def.FindingGuidance + "\n\n" + deferredBlock
	}

	clock, _ := TaskClockFrom(ctx)
	maxDur, clamped := clampMaxDuration(clock.DeadlineUnix, w.cfg.RunTimeout)
	opts := harness.Options{
		Provider:      w.cfg.Provider,
		Model:         w.cfg.Model,
		SystemPrompt:  workerSystem(name, intent, deferredBlock),
		Tools:         tools,
		DeferredTools: def.Deferred,
		UnlockSet:     unlock,
		MaxTurns:      w.cfg.MaxTurns,
		MaxDuration:   maxDur,
		MaxTokens:     w.cfg.MaxTokens,
		Hooks:         hooks,
		Settlement:    wrapupSettlementForTask(AgentWorker, nil, WorkerWritebackTools, clamped),
	}

	session := agentcore.NewSession(WorkerSessionID(ts.ID(), intent.ID), w.cfg.Transcripts)
	agentcore.SeedUnlockFromHistory(session.Load(ctx), def.UnlockSkill)
	if err := session.LoadErr(); err != nil && emit != nil {
		emit(db.Activity{Worker: name, Kind: "error", Summary: "读取会话历史失败，按新会话执行", Detail: err.Error()})
	}

	var fresh string
	if !session.HasHistory(ctx) {
		overview, err := set.overview(ctx, false)
		if err != nil {
			return harness.ReasonModelError, set.Writes(), err
		}
		fresh = renderWorkerInput(overview)
	}
	cont := workerContinueInput
	if message != "" {
		cont = message
	}
	nodeID := intent.ID
	term, saveErr := session.Resume(ctx, opts, fresh, cont, activityEmitter(name, &nodeID, emit))
	if term == nil {
		return harness.ReasonModelError, set.Writes(), saveErr
	}
	if saveErr != nil && emit != nil {
		emit(db.Activity{Worker: name, Kind: "error", NodeID: &nodeID, Summary: "保存会话失败", Detail: saveErr.Error()})
	}
	return term.Reason, set.Writes(), terminalErr(term)
}

// activityEmitter 把 harness 事件折叠为面向观察端的 Activity：只上报工具调用、工具错误与最终文本。
func activityEmitter(worker string, nodeID *int64, emit func(db.Activity)) func(harness.Event) {
	if emit == nil {
		return nil
	}
	return func(ev harness.Event) {
		switch ev.Kind {
		case harness.KindToolUse:
			emit(db.Activity{Worker: worker, NodeID: nodeID, Kind: "tool", Summary: ev.ToolUse.Name, Detail: truncate(string(ev.ToolUse.Input), 500)})
		case harness.KindToolResult:
			if ev.ToolResult.IsError {
				emit(db.Activity{Worker: worker, NodeID: nodeID, Kind: "tool_error", Summary: ev.ToolResult.Name, Detail: truncate(ev.ToolResult.Content, 500)})
			}
		case harness.KindResult:
			t := ev.Terminal
			detail := ""
			if t.Err != nil {
				detail = t.Err.Error()
			}
			emit(db.Activity{Worker: worker, NodeID: nodeID, Kind: "text", Summary: truncate(t.Text, 500), Detail: string(t.Reason) + " " + detail})
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// assembleDeferred 实现 §4.3 的三阶段闭环：
// 阶段 A 宿主已连接并发现工具（def.Deferred 注册但不下发 Schema），全局名字初始解锁并列入名字目录；
// 阶段 B Skill 调用解锁关联工具名并在结果中披露名字；
// 阶段 C SearchExtraTools 只对已解锁工具返回 Schema，模型随后按名直调（harness 统一校验 UnlockSet）。
// 每次组装使用新的 UnlockSet；续跑时由 SeedUnlockFromHistory 经 def.UnlockSkill 重放历史 Skill 调用。
func assembleDeferred(tools []tool.CoreTool, def DeferredInfo) ([]tool.CoreTool, string, *tool.UnlockSet, DeferredInfo) {
	if len(def.Deferred) == 0 && len(def.Skills) == 0 {
		return tools, "", def.Unlock, def
	}
	deferredNames := map[string]bool{}
	for _, t := range def.Deferred {
		deferredNames[t.Name()] = true
	}
	var global []string
	for _, n := range def.GlobalNames {
		if deferredNames[n] {
			global = append(global, n)
		}
	}
	unlock := def.Unlock
	if unlock == nil {
		unlock = tool.NewUnlockSet(nil)
		def.Unlock = unlock
	}
	unlock.Add(global...)
	if len(def.Skills) > 0 {
		skills := tool.NewSkillSet(def.Skills, unlock, def.OnInvoke)
		tools = append(tools, skills.Tool())
		if def.UnlockSkill == nil {
			def.UnlockSkill = func(name string) { skills.Unlock(name) }
		}
	}
	block := ""
	if len(def.Deferred) > 0 {
		tools = append(tools, tool.NewSearchExtraTools(def.Deferred, unlock))
		block = tool.RenderDeferredToolsBlock(global)
	}
	return tools, block, unlock, def
}
