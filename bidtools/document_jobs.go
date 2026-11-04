package bidtools

// S5a 招标解析 Skill（document-processing，AGENT_TOOL_SKILLS_PLAN 4.1 / 4.3 / 7）：
//   document_parse_submit  对任务内的招标原件（或 ZIP 子文件）提交固定解析作业，强制候选模式，同事务订阅当前意图；
//                          只接受 source_kind=tender_file，operation=import / reparse；parser_profile 与执行版本首次提交时固定。
//   document_parse_status  按 job_ref 查询作业状态、解析配置、执行版本与候选记录引用。
//   document_parse_result  按 record_ref 分页读取候选：原件定位（file_id / sha256 / 页码 / 版面框 / OCR 置信度）、缺口与 next_cursor。
//   document_parse_cancel  取消排队或执行中的作业（不等于拒绝候选）。
// 首版不注册采用工具：候选只能由人在材料处理页采用或拒绝；作业成功只表示“候选就绪”。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/filework"
	"github.com/autobid/autobid-engine/ingest"
	"github.com/autobid/autobid-engine/internal/tool"
	"github.com/autobid/autobid-engine/toolhealth"
)

const (
	ToolDocumentParseSubmit = "document_parse_submit"
	ToolDocumentParseStatus = "document_parse_status"
	ToolDocumentParseResult = "document_parse_result"
	ToolDocumentParseCancel = "document_parse_cancel"
)

// 解析记录引用类型（record_ref.kind）。
const RecordKindTenderParse = "tender_parse_record"

const codeHumanAction = "HUMAN_ACTION_REQUIRED"

// FileJobControl 是固定解析执行器的加速与取消入口（filework.Runner）。
type FileJobControl interface {
	Wake()
	Cancel(id int64)
}

// WithFileProcessing 设置招标解析处理器与执行器；为 nil 时 document-processing 工具返回 FEATURE_DISABLED。
func (k *Kit) WithFileProcessing(p *filework.Processor, runner FileJobControl) *Kit {
	k.files, k.fileJobs = p, runner
	return k
}

// DocumentJobTools 返回 document-processing 技能的延迟工具。
func (k *Kit) DocumentJobTools() []tool.CoreTool {
	return []tool.CoreTool{k.documentParseSubmitTool(), k.documentParseStatusTool(), k.documentParseResultTool(), k.documentParseCancelTool()}
}

var parseJobRefSchema = obj(map[string]any{
	"kind": map[string]any{"type": "string", "enum": []any{db.JobKindTenderParse}},
	"id":   integer("document_parse_submit 返回的作业 ID"),
}, "kind", "id")

var parseRecordRefSchema = obj(map[string]any{
	"kind": map[string]any{"type": "string", "enum": []any{RecordKindTenderParse}},
	"id":   integer("作业结果中的候选记录 ID"),
}, "kind", "id")

// documentJobError 把提交 / 查询 / 取消错误映射为错误码；越界与不存在统一为 SCOPE_VIOLATION，不泄露其他范围对象是否存在。
func documentJobError(err error) (tool.Result, error) {
	msg := toolhealth.Sanitize(err.Error())
	var meta *filework.MetadataRequiredError
	switch {
	case errors.As(err, &meta):
		raw, _ := json.Marshal(map[string]any{"accepted": false, "error_code": codeHumanAction, "retryable": false, "missing_fields": meta.Missing,
			"message": meta.Reason + "；请记录缺项并交由人工在材料处理页补充，不要自行编造招标编号"})
		return tool.Result{Content: string(raw), IsError: true}, nil
	case errors.Is(err, filework.ErrSourceKindDisabled), errors.Is(err, filework.ErrProfileDisabled):
		return gateError(codeFeatureDisabled, false, msg), nil
	case errors.Is(err, filework.ErrOCRRequired):
		return gateError(codeRuntimeUnavailable, false, msg+"; the local OCR service is not available, record the gap instead of retrying"), nil
	case errors.Is(err, db.ErrIdempotencyKeyConflict):
		return gateError(codeIdempotencyKey, false, "this request_key was used with different input, use a new key"), nil
	case errors.Is(err, db.ErrParseRevisionConflict):
		return gateError(codeStaleRevision, false, "the document's parse_revision changed; re-read the document list and use its current parse_revision"), nil
	case errors.Is(err, db.ErrJobPrecondition):
		return gateError(codePrecondition, false, msg), nil
	case errors.Is(err, db.ErrJobCapacity):
		return gateError(codeCapacityBusy, true, msg), nil
	case errors.Is(err, ingest.ErrInvalidInput), errors.Is(err, ingest.ErrUnsupportedFormat), errors.Is(err, db.ErrInvalidChart):
		return gateError(codeInvalidArgument, false, msg), nil
	case errors.Is(err, db.ErrInvalidParseRevision), errors.Is(err, filework.ErrNoCandidate), errors.Is(err, filework.ErrHashMismatch):
		return gateError(codePrecondition, false, msg), nil
	case errors.Is(err, db.ErrNotFound), errors.Is(err, db.ErrLeaseLost), errors.Is(err, db.ErrNoIntentExecution),
		errors.Is(err, db.ErrJobKindUnsupported), strings.Contains(err.Error(), "execution scope"):
		return gateError(codeScopeViolation, false, "the file, document, job or record is not accessible in the current intent scope"), nil
	}
	return tool.Result{}, err
}

// parseBusinessState 区分作业状态与业务状态：成功作业只表示候选就绪，采用 / 拒绝由人决定。
func parseBusinessState(j *db.FixedFileJob) string {
	switch {
	case j.Active():
		return "pending"
	case j.Status == "succeeded":
		switch j.RecordStatus {
		case db.RecordReady:
			return "candidate_pending_review"
		case db.RecordAdopted:
			return "adopted"
		case db.RecordRejected:
			return "rejected"
		case db.RecordFailed:
			return "failed"
		}
		return "completed"
	}
	return j.Status
}

func parseNextAction(j *db.FixedFileJob, biz string) string {
	switch {
	case j.Active():
		return "wait_for_job_event"
	case biz == "candidate_pending_review":
		return "read_candidate_then_await_human_adoption"
	case biz == "adopted":
		return "verify_result"
	case biz == "rejected", j.Status == "cancelled":
		return "replan_if_still_needed"
	case j.ErrorCode == db.JobErrRuntimeUnavailable:
		return "record_gap_runtime_unavailable"
	}
	return "record_gap_or_replan"
}

func (k *Kit) parseJobView(j *db.FixedFileJob) map[string]any {
	var in filework.ProcessInput
	_ = json.Unmarshal(j.Input, &in)
	op := "import"
	if in.DocumentID > 0 {
		op = "reparse"
	}
	profile := in.ParserProfile
	if profile == "" {
		profile = db.ParserProfileAuto
	}
	biz := parseBusinessState(j)
	out := map[string]any{"job_ref": db.JobRef{Kind: db.JobKindTenderParse, ID: j.ID}, "status": j.Status, "business_state": biz,
		"operation": op, "file_id": j.FileID, "source_hash": j.SourceHash, "candidate_only": in.CandidateOnly || op == "reparse",
		"parser_profile": profile, "executor": j.Executor, "attempts": j.Attempts, "max_attempts": j.MaxAttempts,
		"deadline_at": j.DeadlineAt.UTC().Format(time.RFC3339), "next_action": parseNextAction(j, biz)}
	if op == "reparse" {
		out["document_id"], out["expected_parse_revision"] = in.DocumentID, in.ExpectedRevision
	} else {
		out["tender_no"] = in.TenderNo
	}
	if j.RecordID != nil {
		out["record_ref"] = map[string]any{"kind": RecordKindTenderParse, "id": *j.RecordID}
		out["record_status"] = j.RecordStatus
	}
	if j.ErrorCode != "" {
		out["error_code"] = j.ErrorCode
	}
	if j.Error != "" {
		out["error"] = truncate(toolhealth.Sanitize(j.Error), 300)
	}
	return out
}

func decodeParseJobRef(in json.RawMessage) (db.JobRef, *tool.Result) {
	args, err := decode[struct {
		JobRef *db.JobRef `json:"job_ref"`
	}](in)
	if err != nil || args.JobRef == nil || args.JobRef.Kind != db.JobKindTenderParse || args.JobRef.ID <= 0 {
		r := gateError(codeInvalidArgument, false, `job_ref must be {"kind":"tender_parse","id":<positive>}`)
		return db.JobRef{}, &r
	}
	return *args.JobRef, nil
}

func (k *Kit) documentParseSubmitTool() tool.CoreTool {
	spec := tool.Spec{Name: ToolDocumentParseSubmit,
		Description: "对本任务留存的招标原件（或 ZIP 子文件）提交后台解析作业（异步，结果以作业事件唤醒当前意图，无需轮询）。" +
			"只生成候选：导入不会直接成为正式招标文档，重新解析也不会覆盖当前解析，都须由人在材料处理页核对后采用。" +
			"source_kind 只能是 tender_file；operation=import 时给 file_id（document_id 与 expected_parse_revision 必须为空），" +
			"招标编号默认取本任务已有正式文档的编号，缺失时返回 HUMAN_ACTION_REQUIRED；operation=reparse 时给 document_id 与 " +
			"expected_parse_revision（文档当前的 parse_revision 整数，不是文档 ID 或 source_epoch）。parser_profile：auto（默认，文本层 + 扫描页 OCR）/ " +
			"native（不调用 OCR）/ ocr_only（PDF 每页与图片都用 OCR）；enhanced_pdf 尚未交付。request_key 由你生成，同一次提交重发时复用，" +
			"改变输入时换新键。拒绝时 accepted=false 并给出 error_code 与 retryable。",
		Schema: obj(map[string]any{
			"source_kind":             map[string]any{"type": "string", "enum": []any{filework.SourceKindTenderFile}, "description": "来源类型，S5a 只开放 tender_file"},
			"operation":               map[string]any{"type": "string", "enum": []any{"import", "reparse"}},
			"file_id":                 integer("任务原件或 ZIP 子文件 ID（import 必填；reparse 可选，须等于文档留存原件）"),
			"document_id":             integer("reparse 的目标招标文档 ID"),
			"expected_parse_revision": integer("reparse 的目标文档当前 parse_revision（整数，可为 0）"),
			"parser_profile":          map[string]any{"type": "string", "enum": []any{"auto", "native", "ocr_only", "enhanced_pdf"}, "description": "解析配置，默认 auto"},
			"request_key":             str("幂等请求键（1~128 字符，不得以 sys: 或 _fixed: 开头）"),
			"tender_no":               str("import 可选：本任务已有正式招标文档中的招标编号（多个编号时必填）"),
			"lot_no":                  str("import 可选：该招标编号已有的标段"),
			"title":                   str("import 可选：文档标题，默认取文件标题或文件名"),
			"doc_role":                map[string]any{"type": "string", "enum": []any{db.DocRoleTender, db.DocRoleCorrection, db.DocRoleClarification, db.DocRoleQA, db.DocRoleAttachment}, "description": "import 可选：文件角色，默认按文件名推断"},
			"published_at":            str("import 可选：公告发布日期 YYYY-MM-DD（只填原文中能找到的日期）"),
		}, "source_kind", "operation", "request_key")}
	return tool.New(spec, func(ctx context.Context, in json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
		if k.files == nil {
			return gateError(codeFeatureDisabled, false, "tender file processing is not enabled on this server (AUTOBID_FILE_PROCESSING_ENABLED)"), nil
		}
		if !db.IntentExecutionBound(ctx) {
			return gateError(codeScopeViolation, false, "document_parse_submit must run inside a host-bound intent"), nil
		}
		req, err := decode[struct {
			SourceKind       string `json:"source_kind"`
			Operation        string `json:"operation"`
			FileID           int64  `json:"file_id"`
			DocumentID       int64  `json:"document_id"`
			ExpectedRevision *int   `json:"expected_parse_revision"`
			ParserProfile    string `json:"parser_profile"`
			RequestKey       string `json:"request_key"`
			TenderNo         string `json:"tender_no"`
			LotNo            string `json:"lot_no"`
			Title            string `json:"title"`
			DocRole          string `json:"doc_role"`
			PublishedAt      string `json:"published_at"`
		}](in)
		if err != nil {
			return gateError(codeInvalidArgument, false, "invalid input: "+err.Error()), nil
		}
		sub, err := k.files.SubmitAgentParse(ctx, k.taskID, "worker", filework.AgentParseRequest{SourceKind: req.SourceKind, Operation: req.Operation,
			FileID: req.FileID, DocumentID: req.DocumentID, ExpectedRevision: req.ExpectedRevision, ParserProfile: req.ParserProfile, RequestKey: req.RequestKey,
			TenderNo: req.TenderNo, LotNo: req.LotNo, Title: req.Title, DocRole: req.DocRole, PublishedAt: req.PublishedAt})
		if err != nil {
			return documentJobError(err)
		}
		if k.fileJobs != nil && sub.Job.Active() {
			k.fileJobs.Wake()
		}
		out := k.parseJobView(sub.Job)
		out["accepted"], out["replayed"] = true, sub.Replayed
		return tool.JSON(out), nil
	})
}

func (k *Kit) documentParseStatusTool() tool.CoreTool {
	return readTool(tool.Spec{Name: ToolDocumentParseStatus,
		Description: "查询招标解析作业：作业状态（queued/running/succeeded/failed/cancelled）、业务状态（pending / candidate_pending_review / adopted / rejected）、" +
			"解析配置、首次提交时固定的执行版本、尝试次数、截止时间、失败原因与候选记录引用 record_ref。只能查询当前意图范围内的作业。",
		Schema: obj(map[string]any{"job_ref": parseJobRefSchema}, "job_ref")},
		func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
			ref, bad := decodeParseJobRef(in)
			if bad != nil {
				return *bad, nil
			}
			j, err := k.pg.GetTenderParseJob(ctx, k.tenant, k.taskID, ref.ID)
			if err != nil {
				return documentJobError(err)
			}
			return tool.JSON(k.parseJobView(j)), nil
		})
}

func (k *Kit) documentParseResultTool() tool.CoreTool {
	return readTool(tool.Spec{Name: ToolDocumentParseResult,
		Description: "分页读取解析候选：每段给出正文（受 max_chars 约束）与原件定位（file_id、sha256、物理页码、版面框、OCR 识别置信度；表格类文档给工作表区域且没有物理页码），" +
			"另给缺口 gaps（扫描页未识别、OCR 失败或低置信度等）、警告与 next_cursor（为 0 表示读完）。重新解析候选另给与当前解析的差异计数。" +
			"OCR 文字是机器识别结果，未经人工核验，不能当作已核验的证据；候选在人工采用前不是正式招标文档。",
		Schema: obj(map[string]any{
			"record_ref":     parseRecordRefSchema,
			"cursor":         integer("上一页返回的 next_cursor，首次为 0"),
			"limit_sections": integer("每页段数 1..20，默认 5"),
			"max_chars":      integer("本页正文总字数上限 500..8000，默认 4000"),
		}, "record_ref")},
		func(ctx context.Context, in json.RawMessage) (tool.Result, error) {
			args, err := decode[struct {
				RecordRef *db.JobRef `json:"record_ref"`
				Cursor    int        `json:"cursor"`
				Limit     int        `json:"limit_sections"`
				MaxChars  int        `json:"max_chars"`
			}](in)
			if err != nil || args.RecordRef == nil || args.RecordRef.Kind != RecordKindTenderParse || args.RecordRef.ID <= 0 {
				return gateError(codeInvalidArgument, false, `record_ref must be {"kind":"tender_parse_record","id":<positive>}`), nil
			}
			if args.Limit == 0 {
				args.Limit = 5
			}
			if args.MaxChars == 0 {
				args.MaxChars = 4000
			}
			if args.Cursor < 0 || args.Limit < 1 || args.Limit > 20 || args.MaxChars < 500 || args.MaxChars > 8000 {
				return gateError(codeInvalidArgument, false, "cursor >= 0, limit_sections 1..20, max_chars 500..8000"), nil
			}
			r, err := k.pg.GetTenderParseRecord(ctx, k.tenant, k.taskID, args.RecordRef.ID)
			if err != nil {
				return documentJobError(err)
			}
			if r.Status == db.RecordFailed {
				return tool.JSON(map[string]any{"record_ref": args.RecordRef, "status": r.Status, "error": truncate(toolhealth.Sanitize(r.Error), 300),
					"next_action": "record_gap_or_replan"}), nil
			}
			page, err := filework.Candidate(r, args.Cursor, args.Limit, args.MaxChars, 0)
			if err != nil {
				return documentJobError(err)
			}
			out := map[string]any{"record_ref": args.RecordRef, "candidate": page, "machine_recognized": len(page.OCRPages) > 0,
				"review_state": map[string]any{"status": r.Status, "adopted": r.Status == db.RecordAdopted, "rejected": r.Status == db.RecordRejected,
					"reject_reason": r.RejectReason}}
			if r.Status == db.RecordReady {
				out["next_action"] = "await_human_adoption"
			}
			return tool.JSON(out), nil
		})
}

func (k *Kit) documentParseCancelTool() tool.CoreTool {
	spec := tool.Spec{Name: ToolDocumentParseCancel,
		Description: "取消排队或执行中的招标解析作业（已终态时原样返回）。取消作业不等于拒绝候选：已生成的候选只能由人在材料处理页采用或拒绝。",
		Schema:      obj(map[string]any{"job_ref": parseJobRefSchema}, "job_ref")}
	return tool.New(spec, func(ctx context.Context, in json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
		ref, bad := decodeParseJobRef(in)
		if bad != nil {
			return *bad, nil
		}
		if _, err := k.pg.GetTenderParseJob(ctx, k.tenant, k.taskID, ref.ID); err != nil {
			return documentJobError(err)
		}
		if err := k.pg.CancelFixedFileJob(ctx, k.tenant, k.taskID, ref.ID, "worker"); err != nil {
			return documentJobError(err)
		}
		if k.fileJobs != nil {
			k.fileJobs.Cancel(ref.ID)
		}
		j, err := k.pg.GetTenderParseJob(ctx, k.tenant, k.taskID, ref.ID)
		if err != nil {
			return documentJobError(err)
		}
		return tool.JSON(k.parseJobView(j)), nil
	})
}
