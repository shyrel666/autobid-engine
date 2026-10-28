package agent

import (
	"time"

	"github.com/autobid/autobid-engine/internal/harness"
)

// 收尾配置覆盖钩子（附录 A.20）；由宿主按 DB 配置注入，未设置时使用内置值。
var (
	WrapupOverride                 func(agentKey string) (string, bool)
	WrapupMaxTurnsOverride         func(agentKey string) (int, bool)
	WrapupTaskTimeoutOverride      func(agentKey string) (string, bool)
	WrapupTaskTimeoutTurnsOverride func(agentKey string) (int, bool)
	WrapupWritebackTimeout         = 60 * time.Second
)

const defaultWrapupTurns = 10

var builtinWrapup = map[string]string{
	AgentWorker: "【收尾】本轮预算已用尽，请停止新的检索与分析。立即把已经核实的内容写回推演图：" +
		"新识别的招标要素用 insert_assets，要素之间的关系用 link_assets，核验过程事实用 record_fact，" +
		"已确认的合规/投策结论用 report_finding。写回完成后用两三句话总结进展与未完成事项，然后结束。",
	AgentPlanner: "【收尾】本轮规划预算已用尽。请只做必要的收束：如有证据充分的目标立即 prove_goal，" +
		"用 TodoWrite 记录尚未投放的后续步骤，然后简短总结并结束。",
}

var builtinTaskTimeoutWrapup = map[string]string{
	AgentWorker: "【任务即将超时】整个投标推演任务的时间已到。不要再做任何新的探索，" +
		"只把已掌握的事实、要素与结论写回推演图（insert_assets / link_assets / record_fact / report_finding），" +
		"然后用一句话说明未完成部分并结束。",
	AgentPlanner: "【任务即将超时】请勿投放新意图，只核对目标是否已有充分证据并 prove_goal，然后结束。",
}

func resolveWrapup(agentKey string) string {
	if WrapupOverride != nil {
		if s, ok := WrapupOverride(agentKey); ok && s != "" {
			return s
		}
	}
	return builtinWrapup[agentKey]
}

func resolveWrapupTurns(agentKey string) int {
	if WrapupMaxTurnsOverride != nil {
		if n, ok := WrapupMaxTurnsOverride(agentKey); ok && n > 0 {
			return n
		}
	}
	return defaultWrapupTurns
}

func resolveTaskTimeoutWrapup(agentKey string) string {
	if WrapupTaskTimeoutOverride != nil {
		if s, ok := WrapupTaskTimeoutOverride(agentKey); ok && s != "" {
			return s
		}
	}
	return builtinTaskTimeoutWrapup[agentKey]
}

func resolveTaskTimeoutTurns(agentKey string) int {
	if WrapupTaskTimeoutTurnsOverride != nil {
		if n, ok := WrapupTaskTimeoutTurnsOverride(agentKey); ok && n > 0 {
			return n
		}
	}
	return resolveWrapupTurns(agentKey)
}

// wrapupSettlement 是无任务 deadline 时的收尾配置；allowed 为 Patch ② 写回白名单。
func wrapupSettlement(agentKey string, disabled, allowed []string) *harness.Settlement {
	return &harness.Settlement{
		Prompt:           resolveWrapup(agentKey),
		MaxTurns:         resolveWrapupTurns(agentKey),
		DisabledTools:    disabled,
		AllowedTools:     allowed,
		WritebackTimeout: WrapupWritebackTimeout,
	}
}

// wrapupSettlementForTask 在单次预算被任务 deadline 夹逼时，按触发原因区分收尾提示（骨架 2-A）。
func wrapupSettlementForTask(agentKey string, disabled, allowed []string, clamped bool) *harness.Settlement {
	st := wrapupSettlement(agentKey, disabled, allowed)
	if clamped {
		if tt := resolveTaskTimeoutWrapup(agentKey); tt != "" {
			st.PromptByReason = map[harness.TerminalReason]string{
				harness.ReasonTimeout:  tt,
				harness.ReasonMaxTurns: st.Prompt,
			}
			st.MaxTurns = resolveTaskTimeoutTurns(agentKey)
		}
	}
	return st
}
