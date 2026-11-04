package bidtools

// S3 图表 Skill（diagram-generation，AGENT_TOOL_SKILLS_PLAN 4.1 / 4.3）：
//   chart_render_submit  按结构化规格为章节当前修订登记图表并提交持久化渲染作业，同事务订阅当前意图；
//                        expected_rev_no 必须等于章节当前修订，request_key 同键同参重放返回原作业，同键异参拒绝。
//   chart_render_status  按 job_ref 查询作业与图表状态（尺寸、是否已对当前修订过期、失败原因）。
//   chart_render_cancel  取消排队 / 执行中的作业；已终态时原样返回。
// 渲染由服务端执行器在数据库事务外完成；作业结束后自动唤醒提交它的意图，模型无需轮询。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/export"
	"github.com/autobid/autobid-engine/internal/tool"
	"github.com/autobid/autobid-engine/toolhealth"
)

const (
	ToolChartRenderSubmit = "chart_render_submit"
	ToolChartRenderStatus = "chart_render_status"
	ToolChartRenderCancel = "chart_render_cancel"
)

// 提交门禁错误码（AGENT_TOOL_SKILLS_PLAN 4.2）；未纠正前不得自动盲重试，CAPACITY_BUSY 除外。
const (
	codeInvalidArgument    = "INVALID_ARGUMENT"
	codeFeatureDisabled    = "FEATURE_DISABLED"
	codeScopeViolation     = "SCOPE_VIOLATION"
	codePrecondition       = "PRECONDITION_FAILED"
	codeStaleRevision      = "CONFLICT_STALE_REVISION"
	codeIdempotencyKey     = "IDEMPOTENCY_KEY_CONFLICT"
	codeCapacityBusy       = "CAPACITY_BUSY"
	codeRuntimeUnavailable = db.JobErrRuntimeUnavailable
)

// gateError 是不插入伪成功作业的结构化拒绝结果。
func gateError(code string, retryable bool, msg string) tool.Result {
	raw, _ := json.Marshal(map[string]any{"accepted": false, "error_code": code, "retryable": retryable, "message": msg})
	return tool.Result{Content: string(raw), IsError: true}
}

// chartJobError 把提交 / 查询 / 取消错误映射为错误码；越界与不存在统一为 SCOPE_VIOLATION，不泄露其他范围对象是否存在。
func chartJobError(err error) (tool.Result, error) {
	msg := toolhealth.Sanitize(err.Error())
	switch {
	case errors.Is(err, export.ErrChartSpec), errors.Is(err, db.ErrInvalidChart):
		return gateError(codeInvalidArgument, false, msg+"; fix the spec and submit again with a new request_key"), nil
	case errors.Is(err, export.ErrRendererMissing):
		return gateError(codeRuntimeUnavailable, false, msg+"; Mermaid charts cannot be rendered on this server (equipment_table still works)"), nil
	case errors.Is(err, db.ErrIdempotencyKeyConflict):
		return gateError(codeIdempotencyKey, false, msg+"; this request_key was used with different input, use a new key"), nil
	case errors.Is(err, db.ErrRevisionConflict):
		return gateError(codeStaleRevision, false, msg+"; re-read the section and use its current rev_no"), nil
	case errors.Is(err, db.ErrChartNoRevision):
		return gateError(codePrecondition, false, "the section has no revision yet: write the section draft first, then submit its charts"), nil
	case errors.Is(err, db.ErrOutlineNotApproved), errors.Is(err, db.ErrOutlineStale), errors.Is(err, db.ErrJobPrecondition), errors.Is(err, db.ErrChartJobActive):
		return gateError(codePrecondition, false, msg), nil
	case errors.Is(err, db.ErrJobCapacity):
		return gateError(codeCapacityBusy, true, msg), nil
	case errors.Is(err, db.ErrNotFound), errors.Is(err, db.ErrLeaseLost), errors.Is(err, db.ErrNoIntentExecution),
		errors.Is(err, db.ErrJobKindUnsupported), strings.Contains(err.Error(), "execution scope"):
		return gateError(codeScopeViolation, false, "the chart, section or job is not accessible in the current intent scope"), nil
	}
	return tool.Result{}, err
}

// chartBusinessState 区分作业状态与业务状态：成功作业只有在图表仍绑定章节当前修订时才是 chart_ready。
func chartBusinessState(j *db.ChartRenderJob, c *db.ProposalChart) string {
	switch {
	case j.Active():
		return "pending"
	case j.Status == db.ChartJobSucceeded && c != nil && !c.Stale && c.SourceHash == j.SourceHash && c.RenderStatus == db.ChartRendered:
		return "chart_ready"
	case j.Status == db.ChartJobSucceeded:
		return "chart_stale"
	}
	return j.Status
}

func chartJobNextAction(j *db.ChartRenderJob, biz string) string {
	switch {
	case j.Active():
		return "wait_for_job_event"
	case biz == "chart_ready":
		return "verify_result"
	case biz == "chart_stale", j.Status == db.ChartJobSuperseded:
		return "resubmit_for_current_revision"
	case j.ErrorCode == db.JobErrRuntimeUnavailable:
		return "record_gap_runtime_unavailable"
	case j.Status == db.ChartJobCancelled:
		return "replan_if_still_needed"
	}
	return "record_gap_or_replan"
}

func chartJobView(j *db.ChartRenderJob, c *db.ProposalChart) map[string]any {
	biz := chartBusinessState(j, c)
	out := map[string]any{"job_ref": db.JobRef{Kind: db.JobKindChartRender, ID: j.ID}, "status": j.Status, "business_state": biz,
		"attempts": j.Attempts, "max_attempts": j.MaxAttempts, "deadline_at": j.DeadlineAt.UTC().Format(time.RFC3339),
		"executor": j.Executor, "next_action": chartJobNextAction(j, biz)}
	if j.ErrorCode != "" {
		out["error_code"] = j.ErrorCode
	}
	if j.Error != "" {
		out["error"] = truncate(toolhealth.Sanitize(j.Error), 300)
	}
	if c != nil {
		chart := map[string]any{"chart_id": c.ID, "title": c.Title, "chart_type": c.ChartType, "section_id": c.SectionID, "section_code": c.SectionCode,
			"rev_no": c.RevNo, "current_rev_no": c.CurrentRevNo, "stale": c.Stale, "render_status": c.RenderStatus, "render_format": c.RenderFormat,
			"source_hash": c.SourceHash}
		if c.RenderFormat == db.ChartFormatPNG {
			chart["width_px"], chart["height_px"], chart["artifact_hash"] = c.WidthPx, c.HeightPx, c.ArtifactHash
		}
		if c.Stale {
			chart["stale_reason"] = c.StaleReason
		}
		out["chart"] = chart
	}
	return out
}

func decodeChartJobRef(in json.RawMessage) (db.JobRef, *tool.Result) {
	args, err := decode[struct {
		JobRef *db.JobRef `json:"job_ref"`
	}](in)
	if err != nil || args.JobRef == nil || args.JobRef.Kind != db.JobKindChartRender || args.JobRef.ID <= 0 {
		r := gateError(codeInvalidArgument, false, `job_ref must be {"kind":"chart_render","id":<positive>}`)
		return db.JobRef{}, &r
	}
	return *args.JobRef, nil
}

var chartRefSchema = obj(map[string]any{
	"kind": map[string]any{"type": "string", "enum": []any{db.JobKindChartRender}},
	"id":   integer("chart_render_submit 返回的作业 ID"),
}, "kind", "id")

// ChartJobTools 返回 diagram-generation 技能的延迟工具。
func (k *Kit) ChartJobTools() []tool.CoreTool {
	return []tool.CoreTool{k.chartRenderSubmitTool(), k.chartRenderStatusTool(), k.chartRenderCancelTool()}
}

func (k *Kit) chartRenderSubmitTool() tool.CoreTool {
	spec := tool.Spec{Name: ToolChartRenderSubmit,
		Description: "为技术标章节的当前修订登记一张图表并提交后台渲染作业（异步，结果以作业事件唤醒当前意图，无需轮询）。" +
			ChartSpecGuide + "设备选型表导出为原生 Word 表格，不依赖 mmdc。" +
			"expected_rev_no 必须是 section_read_context 返回的当前修订号，不一致返回 CONFLICT_STALE_REVISION；request_key 由你生成，同一次提交重发时复用，" +
			"改变输入或重新执行时换新键（同键异参返回 IDEMPOTENCY_KEY_CONFLICT）。拒绝时 accepted=false 并给出 error_code 与 retryable。",
		Schema: obj(map[string]any{
			"section_id":      integer("所属技术标章节 ID（与 section_code 二选一）"),
			"section_code":    str("批准大纲中的章节编号（如 \"2.1\"，与 section_id 二选一）"),
			"expected_rev_no": integer("章节当前修订号（>0），图表绑定该修订"),
			"chart_type":      map[string]any{"type": "string", "enum": export.ChartTypes(), "description": "图表类型"},
			"title":           str("图表标题 / 图注"),
			"spec":            map[string]any{"type": "object", "description": "结构化图表规格对象（字段见工具说明）"},
			"request_key":     str("幂等请求键（1~200 字符，不得以 sys: 开头）"),
		}, "expected_rev_no", "chart_type", "title", "spec", "request_key")}
	return tool.New(spec, func(ctx context.Context, in json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
		if k.charts == nil {
			return gateError(codeFeatureDisabled, false, "chart rendering is not configured on this server"), nil
		}
		if !db.IntentExecutionBound(ctx) {
			return gateError(codeScopeViolation, false, "chart_render_submit must run inside a host-bound intent"), nil
		}
		req, err := decode[struct {
			SectionID     int64           `json:"section_id"`
			Code          string          `json:"section_code"`
			ExpectedRevNo int             `json:"expected_rev_no"`
			ChartType     string          `json:"chart_type"`
			Title         string          `json:"title"`
			Spec          json.RawMessage `json:"spec"`
			RequestKey    string          `json:"request_key"`
		}](in)
		if err != nil {
			return gateError(codeInvalidArgument, false, "invalid input: "+err.Error()), nil
		}
		if req.ExpectedRevNo <= 0 || strings.TrimSpace(req.RequestKey) == "" || (req.SectionID <= 0 && strings.TrimSpace(req.Code) == "") {
			return gateError(codeInvalidArgument, false, "section_id or section_code, expected_rev_no > 0 and request_key are required"), nil
		}
		id, err := k.resolveSection(ctx, req.SectionID, req.Code)
		switch {
		case errors.Is(err, db.ErrNotFound):
			return gateError(codeInvalidArgument, false, "section not found in this task"), nil
		case err != nil:
			return chartJobError(err)
		}
		sub, err := k.charts.SubmitChartRender(ctx, k.tenant, k.taskID, "worker", export.ChartRenderRequest{SectionID: id, ExpectedRevNo: req.ExpectedRevNo,
			ChartType: strings.TrimSpace(req.ChartType), Title: req.Title, Spec: req.Spec, RequestKey: req.RequestKey})
		if err != nil {
			return chartJobError(err)
		}
		out := chartJobView(sub.Job, sub.Chart)
		out["accepted"], out["replayed"] = true, sub.Replayed
		out["chart_id"], out["rev_no"], out["source_hash"] = sub.Chart.ID, sub.Chart.RevNo, sub.Chart.SourceHash
		return tool.JSON(out), nil
	})
}

func (k *Kit) chartRenderStatusTool() tool.CoreTool {
	return readTool(tool.Spec{Name: ToolChartRenderStatus,
		Description: "查询图表渲染作业：作业状态（queued/running/succeeded/failed/cancelled/superseded）、业务状态（chart_ready / chart_stale）、" +
			"图表尺寸、是否已对章节当前修订过期与失败原因。只能查询当前意图提交的作业。",
		Schema: obj(map[string]any{"job_ref": chartRefSchema}, "job_ref")},
		func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
			ref, bad := decodeChartJobRef(in)
			if bad != nil {
				return *bad, nil
			}
			j, c, err := k.pg.GetChartRenderJob(ctx, k.tenant, k.taskID, ref.ID)
			if err != nil {
				return chartJobError(err)
			}
			return tool.JSON(chartJobView(j, c)), nil
		})
}

func (k *Kit) chartRenderCancelTool() tool.CoreTool {
	spec := tool.Spec{Name: ToolChartRenderCancel,
		Description: "取消排队或执行中的图表渲染作业（已终态时原样返回）。取消后迟到的渲染结果不会写回；需要时以新的 request_key 重新提交。",
		Schema:      obj(map[string]any{"job_ref": chartRefSchema}, "job_ref")}
	return tool.New(spec, func(ctx context.Context, in json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
		ref, bad := decodeChartJobRef(in)
		if bad != nil {
			return *bad, nil
		}
		j, err := k.pg.CancelChartRenderJob(ctx, k.tenant, k.taskID, ref.ID, "worker")
		if err != nil {
			return chartJobError(err)
		}
		c, err := k.pg.GetChart(ctx, k.tenant, k.taskID, j.ChartID)
		if err != nil {
			return chartJobError(err)
		}
		return tool.JSON(chartJobView(j, c)), nil
	})
}
