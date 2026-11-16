package bidtools

// S4 合稿 Skill（proposal-export，AGENT_TOOL_SKILLS_PLAN 4.1 / 4.3 / 7）：
//   proposal_export_check   按严格最终稿策略预检导出门禁（只读；Planner 常驻，Worker 经技能解锁），预检不承诺提交成功；
//   proposal_export_submit  提交导出请求组（最终稿强制 strict_runtime=true，模型无权关闭），同事务订阅当前意图；
//                           request_key 同键同参重放返回原请求组（终态同样返回），同键异参拒绝；expected_outline_version 不符拒绝；
//   proposal_export_status  按 job_ref 或 export_request_id 查询快照、目录刷新、产物哈希与受鉴权下载路由；
//   proposal_export_cancel  取消作业或整组（逐项返回取消 / 已终态结果）。
// 提交是高影响入口：须由服务器配置授予任务导出能力（AUTOBID_AGENT_EXPORT_ENABLED，默认关闭；R2 可信身份已完成，仍默认关闭、不做按任务授权），
// 未授予时返回 FEATURE_DISABLED。Skill 解锁不是授权；Planner 的 mandate 只能收窄（allowed_job_kinds / allowed_template_ids）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/export"
	"github.com/autobid/autobid-engine/internal/tool"
	"github.com/autobid/autobid-engine/toolhealth"
)

const (
	ToolProposalExportCheck  = "proposal_export_check"
	ToolProposalExportSubmit = "proposal_export_submit"
	ToolProposalExportStatus = "proposal_export_status"
	ToolProposalExportCancel = "proposal_export_cancel"
)

// WithExportSubmit 设置宿主授予的任务导出能力（服务器配置，不来自模型或 Planner）；未授予时 proposal_export_submit 返回 FEATURE_DISABLED。
func (k *Kit) WithExportSubmit(granted bool) *Kit { k.exportSubmit = granted; return k }

// ExportCheckTools 返回只读导出预检工具（Planner 常驻装配）。
func (k *Kit) ExportCheckTools() []tool.CoreTool {
	return []tool.CoreTool{k.exportCheckTool()}
}

// ExportJobTools 返回 proposal-export 技能的延迟工具。
func (k *Kit) ExportJobTools() []tool.CoreTool {
	return []tool.CoreTool{k.exportCheckTool(), k.exportSubmitTool(), k.exportStatusTool(), k.exportCancelTool()}
}

var exportFormatsSchema = map[string]any{"type": "array", "minItems": 1, "maxItems": 2, "uniqueItems": true,
	"items": map[string]any{"type": "string", "enum": []any{db.ExportDOCX, db.ExportPDF}}, "description": "导出格式，docx / pdf 的非空子集"}

var exportRefSchema = obj(map[string]any{
	"kind": map[string]any{"type": "string", "enum": []any{db.JobKindProposalExport}},
	"id":   integer("proposal_export_submit 返回的作业 ID"),
}, "kind", "id")

// exportJobError 把提交 / 查询 / 取消错误映射为错误码；越界与不存在统一为 SCOPE_VIOLATION，不泄露其他范围对象是否存在。
func exportJobError(err error) (tool.Result, error) {
	msg := toolhealth.Sanitize(err.Error())
	var blocked *db.ExportBlockedError
	switch {
	case errors.As(err, &blocked):
		raw, _ := json.Marshal(map[string]any{"accepted": false, "error_code": codePrecondition, "retryable": false,
			"message": "导出门禁未通过：修正 blockers 后以新的 request_key 提交", "blockers": blocked.Preflight.Readiness.Blockers})
		return tool.Result{Content: string(raw), IsError: true}, nil
	case errors.Is(err, db.ErrExportNotGranted):
		return gateError(codeFeatureDisabled, false, "this server has not granted task export to agents (AUTOBID_AGENT_EXPORT_ENABLED); a person exports from the export page"), nil
	case errors.Is(err, db.ErrInvalidExport):
		return gateError(codeInvalidArgument, false, msg), nil
	case errors.Is(err, export.ErrOfficeUnavailable):
		return gateError(codeRuntimeUnavailable, false, msg+"; a final draft needs the WPS / Word document engine, record the gap instead of retrying"), nil
	case errors.Is(err, db.ErrIdempotencyKeyConflict):
		return gateError(codeIdempotencyKey, false, msg+"; this request_key was used with different input, use a new key"), nil
	case errors.Is(err, db.ErrRevisionConflict):
		return gateError(codeStaleRevision, false, msg+"; run proposal_export_check and use its outline_version"), nil
	case errors.Is(err, db.ErrJobPrecondition):
		return gateError(codePrecondition, false, msg), nil
	case errors.Is(err, db.ErrJobCapacity):
		return gateError(codeCapacityBusy, true, msg), nil
	case errors.Is(err, db.ErrNotFound), errors.Is(err, db.ErrLeaseLost), errors.Is(err, db.ErrNoIntentExecution),
		errors.Is(err, db.ErrJobKindUnsupported), strings.Contains(err.Error(), "execution scope"):
		return gateError(codeScopeViolation, false, "the export, template or job is not accessible in the current intent scope"), nil
	}
	return tool.Result{}, err
}

func exportNextAction(status, business string) string {
	switch {
	case status == db.ExportQueued || status == db.ExportRunning:
		return "wait_for_job_event"
	case business == db.ExportFinalReady:
		return "report_final_draft"
	case business == db.ExportPreviewOnly:
		return "report_preview_only"
	case status == db.ExportCancelled:
		return "replan_if_still_needed"
	case status == "partial":
		return "report_partial_result"
	}
	return "record_gap_or_replan"
}

func (k *Kit) exportJobView(j *db.ExportJob) map[string]any {
	out := map[string]any{"job_ref": db.JobRef{Kind: db.JobKindProposalExport, ID: j.ID}, "format": j.Format, "status": j.Status,
		"business_state": j.BusinessState, "strict_runtime": j.StrictRuntime, "attempts": j.Attempts, "max_attempts": j.MaxAttempts,
		"outline_version": j.OutlineVersion, "next_action": exportNextAction(j.Status, j.BusinessState),
		"snapshot": map[string]any{"sections": len(j.Doc.Sections), "charts": len(j.Charts), "deviation_rows": len(j.Matrix), "template": j.TemplateKey,
			"score_index_rows": len(j.Package.ScoreIndex), "commitments": len(j.Package.Commitments), "evidence": len(j.Package.Evidence)}}
	if j.ExportRequestID != nil {
		out["export_request_id"] = *j.ExportRequestID
	}
	if j.DeadlineAt != nil {
		out["deadline_at"] = j.DeadlineAt.UTC().Format(time.RFC3339)
	}
	if j.Executor != "" {
		out["executor"] = j.Executor
	}
	if j.ErrorCode != "" {
		out["error_code"] = j.ErrorCode
	}
	if j.Error != "" {
		out["error"] = truncate(toolhealth.Sanitize(j.Error), 300)
	}
	if j.Status == db.ExportSucceeded {
		out["toc_refreshed"], out["sha256"], out["artifact_size"] = j.TOCRefreshed, j.ArtifactHash, j.ArtifactSize
		out["office_engine"], out["office_version"] = j.OfficeEngine, j.OfficeVersion
		out["download_route"] = fmt.Sprintf("/api/tasks/%d/exports/%d/download", j.TaskID, j.ID)
		if n := len(j.Warnings); n > 0 {
			out["warnings"] = n
		}
		if sc := j.SelfCheck; sc != nil { // D4：成稿回读自检（页码引用逐处核对、文档属性）
			out["self_check"] = map[string]any{"pages": sc.Pages, "page_refs": sc.PageRefs, "page_refs_verified": sc.PageRefsVerified,
				"verified_against": sc.VerifiedAgainst, "toc_issues": sc.TOCIssues, "metadata_issues": sc.MetadataIssues}
		}
		if j.BusinessState == db.ExportFinalReady {
			// 提交检查清单（签章、交易平台导入与上传等）只能由人在导出页确认；Agent 没有对应工具，也不应声称已提交。
			out["human_step"] = "a person must complete the submission checklist on the export page before the file is handed in"
		}
	}
	return out
}

func (k *Kit) exportRequestView(q *db.ExportRequest) map[string]any {
	jobs := make([]map[string]any, 0, len(q.Jobs))
	for _, j := range q.Jobs {
		jobs = append(jobs, k.exportJobView(j))
	}
	return map[string]any{"export_request_id": q.ID, "request_key": q.RequestKey, "snapshot_hash": q.SnapshotHash, "formats": q.Formats,
		"outline_version": q.OutlineVersion, "strict_runtime": q.StrictRuntime, "policy_version": q.PolicyVersion, "executor": q.Executor,
		"deadline_at": q.DeadlineAt.UTC().Format(time.RFC3339), "status": q.Status, "business_state": q.BusinessState,
		"next_action": exportNextAction(q.Status, q.BusinessState), "jobs": jobs}
}

// decodeExportTarget 解析 job_ref 或 export_request_id（二选一）。
func decodeExportTarget(in json.RawMessage) (ref *db.JobRef, requestID int64, bad *tool.Result) {
	args, err := decode[struct {
		JobRef          *db.JobRef `json:"job_ref"`
		ExportRequestID int64      `json:"export_request_id"`
	}](in)
	invalid := gateError(codeInvalidArgument, false, `give exactly one of job_ref {"kind":"proposal_export","id":<positive>} and export_request_id`)
	switch {
	case err != nil || (args.JobRef == nil) == (args.ExportRequestID <= 0):
		return nil, 0, &invalid
	case args.JobRef != nil && (args.JobRef.Kind != db.JobKindProposalExport || args.JobRef.ID <= 0):
		return nil, 0, &invalid
	}
	return args.JobRef, args.ExportRequestID, nil
}

func (k *Kit) exportCheckTool() tool.CoreTool {
	return readTool(tool.Spec{Name: ToolProposalExportCheck,
		Description: "按严格最终稿策略预检技术标导出门禁：返回 ready、批准大纲版本（提交时作为 expected_outline_version）、阻断项 blockers、" +
			"不阻断的提示 warnings，以及文档引擎（WPS / Word）与 mermaid-cli 的自检状态。最终稿要求文档引擎刷新目录、纳入导出的图表都已渲染；" +
			"预检不承诺提交成功（提交时在锁定快照的事务内重新校验）。",
		Schema: obj(map[string]any{
			"formats":     exportFormatsSchema,
			"template_id": integer("上传的套版模板 ID；省略或 0 表示内置默认模板"),
		}, "formats")},
		func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
			args, err := decode[struct {
				Formats    []string `json:"formats"`
				TemplateID int64    `json:"template_id"`
			}](in)
			if err != nil {
				return gateError(codeInvalidArgument, false, "invalid input: "+err.Error()), nil
			}
			formats, err := db.NormalizeExportFormats(args.Formats)
			if err != nil || args.TemplateID < 0 {
				return gateError(codeInvalidArgument, false, "formats must be a non-empty subset of docx/pdf and template_id >= 0"), nil
			}
			if err := db.CheckJobKindScope(ctx, db.JobKindProposalExport); err != nil {
				return exportJobError(err)
			}
			if err := db.CheckTemplateScope(ctx, args.TemplateID); err != nil {
				return exportJobError(err)
			}
			if args.TemplateID > 0 {
				if _, err := k.pg.GetExportTemplate(ctx, k.tenant, args.TemplateID); err != nil {
					return exportJobError(err)
				}
			}
			pre, err := k.pg.ExportPreflightPolicy(ctx, k.tenant, k.taskID, true)
			if err != nil {
				return exportJobError(err)
			}
			blockers := pre.Readiness.Blockers
			runtime := map[string]any{}
			if k.runtime != nil {
				runtime["capabilities"] = k.runtime.List(toolhealth.Office, toolhealth.Mermaid)
			}
			if k.charts == nil || k.charts.Office == nil {
				reason := "document engine is not configured"
				if k.charts != nil && k.charts.OfficeReason != "" {
					reason = toolhealth.Sanitize(k.charts.OfficeReason)
				}
				blockers = append(blockers, db.ExportBlocker{Kind: "office_unavailable", Message: "文档引擎（WPS / Word）不可用，不能生成最终稿：" + reason})
			} else {
				runtime["office_executor"] = export.OfficeLabel(k.charts.Office)
			}
			return tool.JSON(map[string]any{"ready": len(blockers) == 0, "outline_version": pre.Readiness.OutlineVersion, "formats": formats,
				"policy_version": db.ExportPolicyFinal, "strict_runtime": true, "blockers": blockers, "warnings": pre.Warnings, "charts": pre.Charts,
				"checked_at": pre.Readiness.CheckedAt.UTC().Format(time.RFC3339), "runtime": runtime,
				"note": "precheck only: submission re-validates the gate inside the snapshot transaction"}), nil
		})
}

func (k *Kit) exportSubmitTool() tool.CoreTool {
	spec := tool.Spec{Name: ToolProposalExportSubmit,
		Description: "提交技术标导出请求组（异步，按格式各一个作业；结果以作业事件唤醒当前意图，无需轮询）。一律按严格最终稿策略执行：文档引擎（WPS / Word）" +
			"真实刷新目录，纳入导出的图表须已渲染，产物回读须有目录、图片与表格；不会降级为预览稿。项目名读取任务，bidder_name 写入封面。" +
			"expected_outline_version 必须是 proposal_export_check 返回的 outline_version，不一致返回 CONFLICT_STALE_REVISION；request_key 由你生成，" +
			"同一次提交重发时复用（返回原请求组，终态同样返回），改变输入或重新执行时换新键（同键异参返回 IDEMPOTENCY_KEY_CONFLICT）。" +
			"服务器未开放 Agent 导出时返回 FEATURE_DISABLED；拒绝时 accepted=false 并给出 error_code 与 retryable。",
		Schema: obj(map[string]any{
			"formats":                  exportFormatsSchema,
			"template_id":              integer("上传的套版模板 ID；省略或 0 表示内置默认模板"),
			"bidder_name":              str("投标人名称（必填，最多 200 字；与营业执照一致，写入封面与文档属性）"),
			"expected_outline_version": integer("当前批准大纲版本（>0）"),
			"request_key":              str("幂等请求键（1~200 字符，不得以 sys: 开头）"),
		}, "formats", "expected_outline_version", "request_key")}
	return tool.New(spec, func(ctx context.Context, in json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
		if k.charts == nil {
			return gateError(codeFeatureDisabled, false, "export is not configured on this server"), nil
		}
		if !k.exportSubmit {
			return exportJobError(db.ErrExportNotGranted)
		}
		if !db.IntentExecutionBound(ctx) {
			return gateError(codeScopeViolation, false, "proposal_export_submit must run inside a host-bound intent"), nil
		}
		req, err := decode[struct {
			Formats                []string `json:"formats"`
			TemplateID             int64    `json:"template_id"`
			Bidder                 string   `json:"bidder_name"`
			ExpectedOutlineVersion int      `json:"expected_outline_version"`
			RequestKey             string   `json:"request_key"`
		}](in)
		if err != nil {
			return gateError(codeInvalidArgument, false, "invalid input: "+err.Error()), nil
		}
		if req.ExpectedOutlineVersion <= 0 || strings.TrimSpace(req.RequestKey) == "" {
			return gateError(codeInvalidArgument, false, "expected_outline_version > 0 and request_key are required"), nil
		}
		sub, err := k.charts.SubmitExport(db.WithTaskExportGrant(ctx), k.tenant, k.taskID, "worker", export.ExportRequest{Formats: req.Formats,
			TemplateID: req.TemplateID, Bidder: req.Bidder, ExpectedOutlineVersion: req.ExpectedOutlineVersion, Mode: export.ExportModeFinal, RequestKey: req.RequestKey})
		if err != nil {
			return exportJobError(err)
		}
		out := k.exportRequestView(sub.Request)
		out["accepted"], out["replayed"] = true, sub.Replayed
		return tool.JSON(out), nil
	})
}

func (k *Kit) exportStatusTool() tool.CoreTool {
	return readTool(tool.Spec{Name: ToolProposalExportStatus,
		Description: "查询导出作业（job_ref）或整个请求组（export_request_id）：作业状态 queued / running / succeeded / failed / cancelled、" +
			"业务状态（final_ready 严格最终稿 / preview_only 仅预览）、快照规模、目录是否已刷新、产物 sha256、实际文档引擎与受鉴权下载路由。" +
			"只能查询当前意图提交的导出。",
		Schema: obj(map[string]any{"job_ref": exportRefSchema, "export_request_id": integer("导出请求组 ID")})},
		func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
			ref, rid, bad := decodeExportTarget(in)
			if bad != nil {
				return *bad, nil
			}
			if ref != nil {
				j, err := k.pg.GetExportJobScoped(ctx, k.tenant, k.taskID, ref.ID)
				if err != nil {
					return exportJobError(err)
				}
				return tool.JSON(k.exportJobView(j)), nil
			}
			q, err := k.pg.GetExportRequest(ctx, k.tenant, k.taskID, rid)
			if err != nil {
				return exportJobError(err)
			}
			return tool.JSON(k.exportRequestView(q)), nil
		})
}

func (k *Kit) exportCancelTool() tool.CoreTool {
	spec := tool.Spec{Name: ToolProposalExportCancel,
		Description: "取消排队或执行中的导出作业（job_ref）或整个请求组（export_request_id，逐项返回已取消 / 已终态）。取消后迟到的执行结果不会写回；" +
			"需要时以新的 request_key 重新提交。",
		Schema: obj(map[string]any{"job_ref": exportRefSchema, "export_request_id": integer("导出请求组 ID")})}
	return tool.New(spec, func(ctx context.Context, in json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
		ref, rid, bad := decodeExportTarget(in)
		if bad != nil {
			return *bad, nil
		}
		if ref != nil {
			j, cancelled, err := k.pg.CancelExportJob(ctx, k.tenant, k.taskID, ref.ID, "worker")
			if err != nil {
				return exportJobError(err)
			}
			out := k.exportJobView(j)
			out["cancelled"] = cancelled
			return tool.JSON(out), nil
		}
		q, res, err := k.pg.CancelExportRequest(ctx, k.tenant, k.taskID, rid, "worker")
		if err != nil {
			return exportJobError(err)
		}
		out := k.exportRequestView(q)
		out["results"] = res
		return tool.JSON(out), nil
	})
}
