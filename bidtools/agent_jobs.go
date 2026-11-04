package bidtools

// 公共只读工具 agent_job_status（AGENT_TOOL_SKILLS_PLAN 4.1 / S2）：查询本任务的后台作业与 Agent 订阅。
// Worker 只能读当前宿主绑定 Intent 的订阅；Planner 可按 intent_id 查看任务内的订阅。job_ref 的 kind 决定作业表，
// 不能借类型或裸 ID 越权读取其他任务的作业。等待中的作业完成后会自动唤醒原 Worker，模型无需轮询。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/internal/tool"
	"github.com/autobid/autobid-engine/toolhealth"
)

// ToolAgentJobStatus 是公共只读作业状态查询工具（Planner 与 Worker 常驻，不经 Skill 解锁）。
const ToolAgentJobStatus = "agent_job_status"

// AgentJobTools 返回公共只读作业状态工具。
func (k *Kit) AgentJobTools() []tool.CoreTool {
	return []tool.CoreTool{AgentJobStatusTool(k.pg, k.taskID)}
}

type agentJobView struct {
	JobRef            db.JobRef       `json:"job_ref"`
	IntentID          int64           `json:"intent_id,omitempty"`
	SubscriptionState string          `json:"subscription_state,omitempty"`
	Status            string          `json:"status"`
	BusinessState     string          `json:"business_state,omitempty"`
	TargetRef         json.RawMessage `json:"target_ref,omitempty"`
	ExpectedVersion   *int64          `json:"expected_version,omitempty"`
	Attempts          int             `json:"attempts"`
	MaxAttempts       int             `json:"max_attempts"`
	DeadlineAt        *time.Time      `json:"deadline_at,omitempty"`
	Error             string          `json:"error,omitempty"`
	EventState        string          `json:"event_state,omitempty"`
	NextAction        string          `json:"next_action"`
}

// jobNextAction 只给出下一步建议；完成判据由宿主按真实结果校验，不由该字段决定。
func jobNextAction(status, businessState string) string {
	switch status {
	case "queued", "running":
		return "wait_for_job_event"
	case "failed":
		return "record_gap_or_replan"
	case "cancelled":
		return "replan_if_still_needed"
	case "succeeded":
		switch businessState {
		case "candidate_pending_review":
			return "await_human_adoption"
		case "preview_only":
			return "report_preview_only"
		case "final_ready":
			return "report_final_draft"
		case "chart_stale":
			return "resubmit_for_current_revision"
		}
		return "verify_result"
	case "superseded":
		return "resubmit_for_current_revision"
	}
	return "inspect_job"
}

func viewOfSubscription(s *db.AgentJobSubscription) agentJobView {
	v := agentJobView{JobRef: s.Job, IntentID: s.IntentID, SubscriptionState: s.State, Status: s.JobStatus, TargetRef: s.TargetRef,
		ExpectedVersion: s.ExpectedVersion, Attempts: s.JobAttempts, MaxAttempts: s.JobMaxAttempts, DeadlineAt: s.JobDeadlineAt,
		Error: truncate(toolhealth.Sanitize(s.JobError), 300)}
	if s.Event != nil {
		v.BusinessState, v.EventState = s.Event.BusinessState, s.Event.State
	}
	v.NextAction = jobNextAction(v.Status, v.BusinessState)
	return v
}

// AgentJobStatusTool 构造 agent_job_status。
func AgentJobStatusTool(pg *db.DB, taskID int64) tool.CoreTool {
	kinds := make([]any, 0, len(db.AgentJobKinds()))
	for _, k := range db.AgentJobKinds() {
		kinds = append(kinds, k)
	}
	return readTool(tool.Spec{
		Name: ToolAgentJobStatus,
		Description: "查询本任务的后台作业（tender_parse 招标解析、proposal_export 导出、chart_render 图表渲染）及 Agent 订阅状态：给出 job_ref 查单个作业，" +
			"否则分页列出订阅（Worker 只看当前意图）。status 为 queued / running / succeeded / failed / cancelled；business_state 区分" +
			"候选待人工采用（candidate_pending_review）、已采用、严格最终稿（final_ready）、仅预览（preview_only）、图表已就绪 / 已过期等。等待中的作业完成后会自动唤醒提交它的意图，无需轮询。",
		Schema: obj(map[string]any{
			"job_ref": obj(map[string]any{
				"kind": map[string]any{"type": "string", "enum": kinds},
				"id":   integer("作业 ID"),
			}, "kind", "id"),
			"intent_id": integer("列出该意图的订阅（仅 Planner；Worker 固定为当前意图）"),
			"cursor":    integer("上一页返回的 next_cursor"),
			"limit":     integer("每页条数 1..20，默认 10"),
		}),
	}, func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
		args, err := decode[struct {
			JobRef   *db.JobRef `json:"job_ref"`
			IntentID int64      `json:"intent_id"`
			Cursor   int64      `json:"cursor"`
			Limit    int        `json:"limit"`
		}](in)
		if err != nil || args.IntentID < 0 || args.Cursor < 0 || args.Limit < 0 || args.Limit > 20 {
			return tool.Errorf("INVALID_ARGUMENT: job_ref {kind,id}, intent_id/cursor >= 0, limit 1..20"), nil
		}
		if args.Limit == 0 {
			args.Limit = 10
		}
		if args.JobRef != nil {
			if !db.ValidAgentJobKind(args.JobRef.Kind) || args.JobRef.ID <= 0 {
				return tool.Errorf("INVALID_ARGUMENT: job_ref.kind must be one of %s and id positive", strings.Join(db.AgentJobKinds(), ", ")), nil
			}
			snap, err := pg.GetAgentJobSnapshot(ctx, db.DefaultTenant, taskID, *args.JobRef)
			if err != nil {
				return jobLookupError(err)
			}
			subs, err := pg.ListAgentJobs(ctx, db.DefaultTenant, taskID, db.AgentJobFilter{Job: *args.JobRef, Limit: 20})
			if err != nil {
				return jobLookupError(err)
			}
			v := agentJobView{JobRef: snap.Job, Status: snap.Status, Attempts: snap.Attempts, MaxAttempts: snap.MaxAttempts,
				DeadlineAt: snap.DeadlineAt, Error: truncate(toolhealth.Sanitize(snap.Error), 300)}
			views := make([]agentJobView, 0, len(subs))
			for _, s := range subs {
				sv := viewOfSubscription(s)
				views = append(views, sv)
				if v.BusinessState == "" {
					v.BusinessState = sv.BusinessState
				}
			}
			v.NextAction = jobNextAction(v.Status, v.BusinessState)
			return tool.JSON(map[string]any{"job": v, "subscriptions": views}), nil
		}
		subs, err := pg.ListAgentJobs(ctx, db.DefaultTenant, taskID, db.AgentJobFilter{IntentID: args.IntentID, Before: args.Cursor, Limit: args.Limit})
		if err != nil {
			return jobLookupError(err)
		}
		views := make([]agentJobView, 0, len(subs))
		for _, s := range subs {
			views = append(views, viewOfSubscription(s))
		}
		var next int64
		if len(subs) == args.Limit {
			next = subs[len(subs)-1].ID
		}
		return tool.JSON(map[string]any{"jobs": views, "next_cursor": next}), nil
	})
}

// jobLookupError 把越界与不存在统一为 SCOPE_VIOLATION，不泄露其他任务的作业是否存在。
func jobLookupError(err error) (tool.Result, error) {
	if errors.Is(err, db.ErrNotFound) || strings.Contains(err.Error(), "execution scope") {
		return tool.Errorf("SCOPE_VIOLATION: job is not readable in the current task scope"), nil
	}
	if errors.Is(err, db.ErrJobKindUnsupported) {
		return tool.Errorf("INVALID_ARGUMENT: %v", err), nil
	}
	return tool.Result{}, err
}
