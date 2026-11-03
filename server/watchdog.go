package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/autobid/autobid-engine/agent"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/harness"
	"github.com/autobid/autobid-engine/internal/llm"
	"github.com/autobid/autobid-engine/internal/tool"
)

// WatchdogConfig 控制 Worker 执行期间的确定性跑偏检测、自动 Steer 纠偏与熔断阈值。
// 任意阈值设为负数即关闭对应维度的检测；0 取默认值。
type WatchdogConfig struct {
	RepeatCallSteerThreshold     int // 相同工具+相同入参重复调用触发自动 Steer 阈值，默认 3
	RepeatCallKillThreshold      int // 相同工具+相同入参重复调用触发熔断阈值，默认 5
	ConsecutiveErrSteerThreshold int // 连续工具报错触发自动 Steer 阈值，默认 4
	ConsecutiveErrKillThreshold  int // 连续工具报错触发熔断阈值，默认 7
	NoWritebackSteerTurns        int // 累计工具调用零写回触发自动 Steer 阈值，默认 15
	NoWritebackKillTurns         int // 累计工具调用零写回触发熔断阈值，默认 25
}

func (c *WatchdogConfig) defaults() {
	if c.RepeatCallSteerThreshold == 0 {
		c.RepeatCallSteerThreshold = 3
	}
	if c.RepeatCallKillThreshold == 0 {
		c.RepeatCallKillThreshold = 5
	}
	if c.ConsecutiveErrSteerThreshold == 0 {
		c.ConsecutiveErrSteerThreshold = 4
	}
	if c.ConsecutiveErrKillThreshold == 0 {
		c.ConsecutiveErrKillThreshold = 7
	}
	if c.NoWritebackSteerTurns == 0 {
		c.NoWritebackSteerTurns = 15
	}
	if c.NoWritebackKillTurns == 0 {
		c.NoWritebackKillTurns = 25
	}
}

// isWatchdogWritebackTool 判断工具是否属于推演图或标书域的有效写回工具。
func isWatchdogWritebackTool(name string) bool {
	switch name {
	case agent.ToolInsertAssets,
		agent.ToolLinkAssets,
		agent.ToolRecordFact,
		agent.ToolReportFinding,
		agent.ToolAddGoal,
		"requirement_bind_evidence",
		"section_write_draft",
		"chart_generate",
		"compliance_verify",
		"fulltext_consistency_check":
		return true
	}
	return false
}

// isWatchdogMetaTool 判断工具是否为解锁写回所需的元工具（达到 MaxToolCalls 时不阻断）。
func isWatchdogMetaTool(name string) bool {
	return name == "Skill" || name == tool.SearchExtraToolsName || name == agent.ToolListAssets
}

func canonicalToolSig(name string, raw []byte) string {
	trimmed := bytes.TrimSpace(raw)
	var v any
	if len(trimmed) > 0 && json.Unmarshal(trimmed, &v) == nil {
		if norm, err := json.Marshal(v); err == nil {
			trimmed = norm
		}
	}
	sum := sha256.Sum256(trimmed)
	return name + ":" + hex.EncodeToString(sum[:12])
}

// watchdogHooks 在 Worker 工具生命周期内执行：
// 1. PreToolUse：强制校验 IntentMandate（白名单章节/要求、字数区间、禁用工具、最大调用上限）；
// 2. PostToolUse：监控重复调用死循环、连续报错风暴、长回合零写回空转，分两级执行自动 Steer 或 AbortKilledByWatchdog 熔断；
// 3. Stop：当 Mandate.RequireWriteback 为真且尚未写回时，阻断空手退出并督促写回。
type watchdogHooks struct {
	inner    harness.HookRunner
	intentID int64
	worker   string
	mandate  *db.IntentMandate
	cfg      WatchdogConfig
	steer    func(msg string)
	kill     func(cause error)
	emit     func(db.Activity)
	// durableWork 报告本 Intent 是否已有持久化后台作业订阅：有则 Stop 不因缺少同步写回而督促，
	// 本轮可释放槽位进入等待；完成门禁仍校验真实结果。可为 nil。
	durableWork func() bool

	mu              sync.Mutex
	totalCalls      int
	consecutiveErrs int
	hasWriteback    bool
	noWriteSteered  bool
	callCounts      map[string]int
	writebackNudges atomic.Int64
}

func newWatchdogHooks(
	inner harness.HookRunner,
	intentID int64,
	worker string,
	mandate *db.IntentMandate,
	cfg WatchdogConfig,
	steer func(msg string),
	kill func(cause error),
	emit func(db.Activity),
) *watchdogHooks {
	cfg.defaults()
	return &watchdogHooks{
		inner:      inner,
		intentID:   intentID,
		worker:     worker,
		mandate:    mandate,
		cfg:        cfg,
		steer:      steer,
		kill:       kill,
		emit:       emit,
		callCounts: make(map[string]int),
	}
}

func (w *watchdogHooks) PreToolUse(ctx context.Context, name string, input []byte) (bool, string, []byte) {
	targetName, targetInput := tool.UnwrapDeferred(name, input)
	raw := []byte(targetInput)

	if m := w.mandate; m != nil {
		if slices.Contains(m.ForbiddenTools, targetName) || slices.Contains(m.ForbiddenTools, name) {
			return true, fmt.Sprintf("【Mandate 边界拦截】当前意图禁止调用工具 %q，请严格在 Mandate 允许范围内执行。", targetName), nil
		}
		if m.MaxToolCalls > 0 {
			w.mu.Lock()
			calls := w.totalCalls
			w.mu.Unlock()
			if calls >= m.MaxToolCalls && !isWatchdogWritebackTool(targetName) && !isWatchdogMetaTool(targetName) {
				return true, fmt.Sprintf("【Mandate 边界拦截】当前意图工具调用已达上限（%d/%d），禁止继续只读探索，请立即调用写回工具（如 record_fact / report_finding / section_write_draft）保存成果并结束。", calls, m.MaxToolCalls), nil
			}
		}
		if block, reason := checkMandatePayload(m, targetName, raw); block {
			return true, reason, nil
		}
	}

	if w.inner != nil {
		return w.inner.PreToolUse(ctx, name, input)
	}
	return false, "", nil
}

func checkMandatePayload(m *db.IntentMandate, toolName string, raw []byte) (bool, string) {
	switch toolName {
	case "section_write_draft", "chart_generate":
		var req struct {
			SectionID int64  `json:"section_id"`
			ContentMD string `json:"content_md"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return false, ""
		}
		if len(m.AllowedSectionIDs) > 0 && req.SectionID > 0 && !slices.Contains(m.AllowedSectionIDs, req.SectionID) {
			return true, fmt.Sprintf("【Mandate 边界拦截】当前意图仅允许修改章节 section_id=%v，越界请求修改 section_id=%d 已被拒绝。", m.AllowedSectionIDs, req.SectionID)
		}
		if toolName == "section_write_draft" && (m.TargetWordsMin > 0 || m.TargetWordsMax > 0) {
			n := utf8.RuneCountInString(strings.TrimSpace(req.ContentMD))
			if m.TargetWordsMin > 0 && n < m.TargetWordsMin {
				return true, fmt.Sprintf("【Mandate 边界拦截】当前章节草稿字数/字符数为 %d，低于 Mandate 要求下限 %d，请充实技术细节后再提交。", n, m.TargetWordsMin)
			}
			if m.TargetWordsMax > 0 && n > m.TargetWordsMax {
				return true, fmt.Sprintf("【Mandate 边界拦截】当前章节草稿字数/字符数为 %d，超出 Mandate 要求上限 %d，请精简后再提交。", n, m.TargetWordsMax)
			}
		}
	case "requirement_bind_evidence":
		if len(m.AllowedRequirementIDs) > 0 {
			var req struct {
				RequirementID int64 `json:"requirement_id"`
			}
			if err := json.Unmarshal(raw, &req); err == nil && req.RequirementID > 0 && !slices.Contains(m.AllowedRequirementIDs, req.RequirementID) {
				return true, fmt.Sprintf("【Mandate 边界拦截】当前意图仅允许操作 requirement_id=%v，越界请求 requirement_id=%d 已被拒绝。", m.AllowedRequirementIDs, req.RequirementID)
			}
		}
	}
	return false, ""
}

func (w *watchdogHooks) PostToolUse(ctx context.Context, name string, input, result []byte, isErr bool) {
	if w.inner != nil {
		w.inner.PostToolUse(ctx, name, input, result, isErr)
	}
	targetName, targetInput := tool.UnwrapDeferred(name, input)
	sig := canonicalToolSig(targetName, []byte(targetInput))

	w.mu.Lock()
	w.totalCalls++
	total := w.totalCalls
	w.callCounts[sig]++
	rep := w.callCounts[sig]
	if isErr {
		w.consecutiveErrs++
	} else {
		w.consecutiveErrs = 0
		var outcome struct {
			Outcome string `json:"outcome"`
		}
		_ = json.Unmarshal(result, &outcome)
		if isWatchdogWritebackTool(targetName) && (outcome.Outcome == "" || outcome.Outcome == db.SaveApplied) {
			w.hasWriteback = true
		}
	}
	consecErr := w.consecutiveErrs
	wrote := w.hasWriteback
	alreadyNoWriteSteered := w.noWriteSteered
	if !wrote && w.cfg.NoWritebackSteerTurns > 0 && total >= w.cfg.NoWritebackSteerTurns {
		w.noWriteSteered = true
	}
	w.mu.Unlock()

	// 1. 熔断判定（Kill）优先于纠偏（Steer）
	if w.cfg.RepeatCallKillThreshold > 0 && rep >= w.cfg.RepeatCallKillThreshold {
		reason := fmt.Sprintf("工具 %s 以相同参数重复调用达 %d 次，疑似陷入死循环", targetName, rep)
		w.reportAndKill(reason)
		return
	}
	if w.cfg.ConsecutiveErrKillThreshold > 0 && consecErr >= w.cfg.ConsecutiveErrKillThreshold {
		reason := fmt.Sprintf("连续 %d 次工具调用报错（最近工具：%s），执行受阻熔断", consecErr, targetName)
		w.reportAndKill(reason)
		return
	}
	if !wrote && w.cfg.NoWritebackKillTurns > 0 && total >= w.cfg.NoWritebackKillTurns {
		reason := fmt.Sprintf("累计调用工具 %d 次仍无任何推演图或标书写回产出，空转超限熔断", total)
		w.reportAndKill(reason)
		return
	}

	// 2. 自动纠偏判定（Auto-Steer）
	if w.cfg.RepeatCallSteerThreshold > 0 && rep == w.cfg.RepeatCallSteerThreshold {
		msg := fmt.Sprintf("【Watchdog 自动纠偏】你已对工具 %q 使用完全相同的参数重复调用了 %d 次。禁止再次重复相同调用！请基于已有结果直接调用 record_fact / report_finding / section_write_draft 写回，或更换查询条件。", targetName, rep)
		w.reportAndSteer("检测到重复工具调用，已注入自动纠偏", msg)
		return
	}
	if w.cfg.ConsecutiveErrSteerThreshold > 0 && consecErr == w.cfg.ConsecutiveErrSteerThreshold {
		msg := fmt.Sprintf("【Watchdog 自动纠偏】你已连续 %d 次工具调用失败（最近：%q）。请检查参数格式、实体/章节 ID 是否存在，或直接将当前已确认的事实与缺口通过 record_fact 写回后结束。", consecErr, targetName)
		w.reportAndSteer("检测到连续工具报错，已注入自动纠偏", msg)
		return
	}
	if !wrote && !alreadyNoWriteSteered && w.cfg.NoWritebackSteerTurns > 0 && total >= w.cfg.NoWritebackSteerTurns {
		msg := fmt.Sprintf("【Watchdog 自动纠偏】你已累计调用 %d 次只读工具但尚未产生任何写回（record_fact / report_finding / insert_assets / section_write_draft）。请立即停止发散检索，收敛并将已核实结论写回系统！", total)
		w.reportAndSteer("检测到长回合零写回空转，已注入收敛指令", msg)
	}
}

func (w *watchdogHooks) HasWriteback() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.hasWriteback
}

func (w *watchdogHooks) Stop(ctx context.Context, messages []llm.Message) (bool, []string, string) {
	if w.inner != nil {
		prevent, blocking, msg := w.inner.Stop(ctx, messages)
		if prevent || len(blocking) > 0 {
			return prevent, blocking, msg
		}
	}
	if w.mandate != nil && w.mandate.RequireWriteback {
		w.mu.Lock()
		wrote := w.hasWriteback
		w.mu.Unlock()
		if !wrote && (w.durableWork == nil || !w.durableWork()) && w.writebackNudges.Add(1) <= 1 {
			return false, []string{"【Mandate 完结门禁】当前意图设置了 require_writeback=true，但你尚未成功调用任何写回工具（如 record_fact、report_finding、section_write_draft、requirement_bind_evidence）。请先调用写回工具落盘结果后再结束。"}, ""
		}
	}
	return false, nil, ""
}

func (w *watchdogHooks) reportAndSteer(summary, steerMsg string) {
	if w.steer != nil {
		w.steer(steerMsg)
	}
	if w.emit != nil {
		id := w.intentID
		w.emit(db.Activity{
			Worker:  "watchdog",
			Kind:    "watchdog",
			NodeID:  &id,
			Summary: summary,
			Detail:  steerMsg,
		})
	}
}

func (w *watchdogHooks) reportAndKill(reason string) {
	if w.emit != nil {
		id := w.intentID
		w.emit(db.Activity{
			Worker:  "watchdog",
			Kind:    "watchdog",
			NodeID:  &id,
			Summary: "Watchdog 触发跑偏熔断",
			Detail:  reason,
		})
	}
	if w.kill != nil {
		w.kill(&agent.AbortCause{Code: agent.AbortKilledByWatchdog.Code, Message: reason})
	}
}
