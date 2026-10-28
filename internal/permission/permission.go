// Package permission 定义工具自检权限的上下文与判定结果（CoreTool.CheckPermissions）。
// 人工审批与规则拦截属于 intercept/guard 控制面，经 harness.HookRunner.PreToolUse 接入。
package permission

import "encoding/json"

type Mode string

const (
	ModeDefault Mode = "default"
	ModeBypass  Mode = "bypass" // 跳过工具自检（Hook 仍执行）
)

type Context struct {
	Mode     Mode
	AgentKey string
	TaskID   int64
}

type Behavior string

const (
	BehaviorAllow Behavior = "allow"
	BehaviorDeny  Behavior = "deny"
	// BehaviorAsk 交由 PreToolUse Hook（审批拦截器）裁决；未配置 Hook 时按拒绝处理。
	BehaviorAsk Behavior = "ask"
)

type Decision struct {
	Behavior     Behavior
	Message      string
	UpdatedInput json.RawMessage
}

func Allow() Decision              { return Decision{Behavior: BehaviorAllow} }
func Deny(message string) Decision { return Decision{Behavior: BehaviorDeny, Message: message} }
func Ask(message string) Decision  { return Decision{Behavior: BehaviorAsk, Message: message} }
