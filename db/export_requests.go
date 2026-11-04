package db

// S4（AGENT_TOOL_SKILLS_PLAN 4.1 / 4.3 / 5.1~5.2）：导出请求组 export_requests 与严格最终稿策略（导出附件域）。
//
// 请求幂等范围为 (tenant, task, request_key)：相同 request_hash 返回原请求组及其全部格式作业（成功 / 失败 / 取消终态同样返回），
// 不同则 ErrIdempotencyKeyConflict；只有新键才解析当前批准大纲、冻结快照并检查提交门禁。snapshot_hash 在首次提交时冻结，
// 之后章节、图表变化不影响原请求的重放。一个请求组与全部格式作业、订阅在同一事务内创建，不会半组入队。
//
// 严格最终稿策略（ExportPolicyFinalV1，strict_runtime=true，Agent 提交强制）：
//   - 提交：服务层确认文档引擎（WPS / Word）可用并固定执行版本 executor；纳入导出的图表须是已登记哈希的 PNG 或原生设备选型表；
//   - 执行（export.Service.Execute）：引擎缺失或与固定版本不符时失败（RUNTIME_UNAVAILABLE），不降级为未刷新目录的 DOCX；
//     PNG 须可读且哈希一致；产物回读须有刷新后的目录、全部图片与表格（READBACK_FAILED）；
//   - 结果：ck_export_jobs_final 保证严格作业只有在目录已刷新并记录引擎时才能成功，业务状态为 final_ready。
// 预览稿（ExportPolicyPreviewV1）保持 Phase 6B 行为，成功结果为 preview_only，不能当作最终交付。
//
// 锁序：
//   - 提交：任务行（FOR NO KEY UPDATE，与材料门禁、同键并发与暂停串行化）→（宿主绑定时）Intent 活租约 → 模板 → 批准大纲 → 章节
//     → 请求 / 作业 → 订阅；
//   - 取消：任务行（共享）→ 作业（按 ID 升序）。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/compliance"
	"github.com/jackc/pgx/v5/pgconn"
)

// 导出质量策略（显式版本化；修改判定规则时升级版本号，旧请求保留原版本）。
const (
	ExportPolicyFinalV1   = "export-final/v1"
	ExportPolicyPreviewV1 = "export-preview/v1"
)

// 导出作业错误码（与 AGENT_TOOL_SKILLS_PLAN 4.2 的作业错误码同一命名空间）。
const (
	JobErrReadbackFailed = "READBACK_FAILED"
	JobErrExportFailed   = "EXPORT_FAILED"
)

const (
	DefaultExportJobDeadline    = 30 * time.Minute
	DefaultExportJobMaxAttempts = 3
	maxActiveExportJobsPerTask  = 20
)

var ErrExportNotGranted = errors.New("db: the host has not granted task export to this execution")

// ExportRequest 是一次导出请求组（一个请求键对应一组格式作业）。Status / BusinessState 由组内作业汇总。
type ExportRequest struct {
	ID                     int64        `json:"id"`
	TaskID                 int64        `json:"task_id"`
	RequestKey             string       `json:"request_key"`
	RequestHash            string       `json:"-"`
	SnapshotHash           string       `json:"snapshot_hash"`
	Formats                []string     `json:"formats"`
	TemplateID             *int64       `json:"template_id,omitempty"`
	TemplateKey            string       `json:"template_key"`
	TemplateHash           string       `json:"template_hash"`
	Bidder                 string       `json:"bidder,omitempty"`
	ExpectedOutlineVersion *int         `json:"expected_outline_version,omitempty"`
	OutlineID              int64        `json:"outline_id"`
	OutlineVersion         int          `json:"outline_version"`
	StrictRuntime          bool         `json:"strict_runtime"`
	PolicyVersion          string       `json:"policy_version"`
	Executor               string       `json:"executor,omitempty"`
	DeadlineAt             time.Time    `json:"deadline_at"`
	CreatedBy              string       `json:"created_by"`
	CreatedAt              time.Time    `json:"created_at"`
	UpdatedAt              time.Time    `json:"updated_at"`
	Status                 string       `json:"status"`
	BusinessState          string       `json:"business_state"`
	Jobs                   []*ExportJob `json:"jobs"`
}

const exportRequestCols = `id, task_id, request_key, request_hash, snapshot_hash, formats, template_id, template_key, template_hash, bidder,
	expected_outline_version, outline_id, outline_version, strict_runtime, policy_version, executor, deadline_at, created_by, created_at, updated_at`

func scanExportRequest(r rowScanner) (*ExportRequest, error) {
	q := new(ExportRequest)
	var formats []byte
	var tpl, expected sql.NullInt64
	err := r.Scan(&q.ID, &q.TaskID, &q.RequestKey, &q.RequestHash, &q.SnapshotHash, &formats, &tpl, &q.TemplateKey, &q.TemplateHash, &q.Bidder,
		&expected, &q.OutlineID, &q.OutlineVersion, &q.StrictRuntime, &q.PolicyVersion, &q.Executor, &q.DeadlineAt, &q.CreatedBy, &q.CreatedAt, &q.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(formats, &q.Formats); err != nil {
		return nil, fmt.Errorf("export request %d formats: %w", q.ID, err)
	}
	if tpl.Valid {
		v := tpl.Int64
		q.TemplateID = &v
	}
	if expected.Valid {
		v := int(expected.Int64)
		q.ExpectedOutlineVersion = &v
	}
	q.Jobs = []*ExportJob{}
	return q, nil
}

// summarize 汇总组内作业：任一排队 / 执行中即未结束；全部成功时，严格请求且每个作业都是最终稿才是 final_ready。
func (q *ExportRequest) summarize() {
	var queued, running, ok, cancelled, final int
	for _, j := range q.Jobs {
		switch j.Status {
		case ExportQueued:
			queued++
		case ExportRunning:
			running++
		case ExportSucceeded:
			ok++
			if j.BusinessState == ExportFinalReady {
				final++
			}
		case ExportCancelled:
			cancelled++
		}
	}
	n := len(q.Jobs)
	switch {
	case n == 0:
		q.Status = ExportQueued
	case running > 0:
		q.Status = ExportRunning
	case queued > 0:
		q.Status = ExportQueued
	case ok == n:
		q.Status = ExportSucceeded
	case cancelled == n:
		q.Status = ExportCancelled
	case ok > 0:
		q.Status = "partial"
	default:
		q.Status = ExportFailed
	}
	q.BusinessState = q.Status
	if q.Status == ExportSucceeded {
		q.BusinessState = ExportPreviewOnly
		if q.StrictRuntime && final == n {
			q.BusinessState = ExportFinalReady
		}
	}
}

// NormalizeExportFormats 去重并按 docx、pdf 固定顺序返回；未知格式或空集合返回 ErrInvalidExport。
func NormalizeExportFormats(in []string) ([]string, error) {
	seen := map[string]bool{}
	for _, f := range in {
		f = strings.ToLower(strings.TrimSpace(f))
		if f != ExportDOCX && f != ExportPDF {
			return nil, fmt.Errorf("%w: unknown format %q", ErrInvalidExport, f)
		}
		seen[f] = true
	}
	var out []string
	for _, f := range []string{ExportDOCX, ExportPDF} {
		if seen[f] {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: at least one format is required", ErrInvalidExport)
	}
	return out, nil
}

// ValidExportRequestKey 校验客户端请求键：1~200 字符，不得使用服务端保留前缀。
func ValidExportRequestKey(key string) error {
	switch {
	case key == "" || len(key) > 200:
		return fmt.Errorf("%w: request_key must be 1-200 characters", ErrInvalidExport)
	case strings.HasPrefix(key, ReservedRequestKeyPrefix):
		return fmt.Errorf("%w: request_key prefix %q is reserved for the server", ErrInvalidExport, ReservedRequestKeyPrefix)
	}
	return nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// SubmitExportInput 是一次导出提交。RequestHash 是规范化客户端输入的哈希（服务层计算，含策略版本）；Executor 是首次提交时
// 固定的文档引擎执行版本（严格最终稿必填）。
type SubmitExportInput struct {
	RequestKey             string
	RequestHash            string
	Formats                []string
	TemplateID             int64  // 0 表示内置默认模板
	DefaultTemplateHash    string // 内置默认模板的 SHA-256（由 export 包提供）
	Bidder                 string
	ExpectedOutlineVersion int // >0 时须等于当前批准大纲版本（CONFLICT_STALE_REVISION）
	StrictRuntime          bool
	Executor               string
	Deadline               time.Duration
	MaxAttempts            int
	Actor                  string

	allowReserved bool // 兼容入口使用服务端保留命名空间的一次性请求键
}

// ExportSubmission 是提交结果；Replayed 表示同键同参的重放，返回原请求组（可能已终态），Preflight 为 nil。
type ExportSubmission struct {
	Request       *ExportRequest          `json:"request"`
	Jobs          []*ExportJob            `json:"jobs"`
	Subscriptions []*AgentJobSubscription `json:"subscriptions,omitempty"`
	Preflight     *ExportPreflight        `json:"preflight,omitempty"`
	Replayed      bool                    `json:"replayed"`
}

// SubmitExport 执行导出门禁并在一个 REPEATABLE READ 事务内固化快照、登记请求组与每种格式的导出作业；宿主绑定 Intent 的执行
// 须持有宿主授予的任务导出能力（WithTaskExportGrant），并在同一事务内订阅全部作业。门禁未通过返回 *ExportBlockedError。
// 该方法自行开启事务，不得在 Transaction / InTx 回调内调用。
func (d *DB) SubmitExport(ctx context.Context, tenant string, taskID int64, in SubmitExportInput) (*ExportSubmission, error) {
	tenant = tenantOr(tenant)
	if d.tx != nil {
		return nil, errors.New("db: SubmitExport opens its own REPEATABLE READ transaction and must not run inside another transaction")
	}
	in.RequestKey = strings.TrimSpace(in.RequestKey)
	if !in.allowReserved {
		if err := ValidExportRequestKey(in.RequestKey); err != nil {
			return nil, err
		}
	}
	formats, err := NormalizeExportFormats(in.Formats)
	if err != nil {
		return nil, err
	}
	in.Formats, in.Bidder = formats, strings.TrimSpace(in.Bidder)
	switch {
	case in.RequestHash == "":
		return nil, fmt.Errorf("%w: request_hash is required", ErrInvalidExport)
	case in.TemplateID < 0 || in.ExpectedOutlineVersion < 0 || len([]rune(in.Bidder)) > 200:
		return nil, fmt.Errorf("%w: invalid template_id, expected_outline_version or bidder", ErrInvalidExport)
	}
	if err := CheckJobKindScope(ctx, JobKindProposalExport); err != nil {
		return nil, err
	}
	if err := CheckTemplateScope(ctx, in.TemplateID); err != nil {
		return nil, err
	}
	_, bound := intentExecutionFrom(ctx)
	if bound && !TaskExportGranted(ctx) {
		return nil, ErrExportNotGranted
	}
	if in.Deadline <= 0 {
		in.Deadline = DefaultExportJobDeadline
	}
	if in.MaxAttempts <= 0 {
		in.MaxAttempts = DefaultExportJobMaxAttempts
	}
	in.MaxAttempts = min(in.MaxAttempts, 10)
	var out *ExportSubmission
	for attempt := 0; attempt < 3; attempt++ {
		out = &ExportSubmission{}
		err = d.inRepeatableRead(ctx, func(stx *sql.Tx) error {
			out = &ExportSubmission{}
			return (&DB{DB: d.DB, tx: stx}).submitExportTx(ctx, tenant, taskID, in, bound, out)
		})
		// 同键并发提交：后到者的快照早于先到者提交，插入撞唯一键后重试一次即可看到原请求并重放。
		if !isConstraintViolation(err, "uq_export_requests_request") {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

func isConstraintViolation(err error, name string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == name
}

func (d *DB) submitExportTx(ctx context.Context, tenant string, taskID int64, in SubmitExportInput, bound bool, out *ExportSubmission) error {
	var gateVersion int64
	var paused bool
	err := d.QueryRowContext(ctx, `SELECT file_gate_version, paused FROM tasks WHERE id = $1 FOR NO KEY UPDATE`, taskID).Scan(&gateVersion, &paused)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: task %d", ErrNotFound, taskID)
	}
	if err != nil {
		return err
	}
	old, err := scanExportRequest(d.QueryRowContext(ctx, `SELECT `+exportRequestCols+` FROM export_requests
		WHERE tenant_id = $1 AND task_id = $2 AND request_key = $3`, tenant, taskID, in.RequestKey))
	switch {
	case err == nil:
		if old.RequestHash != in.RequestHash {
			return fmt.Errorf("%w: %s", ErrIdempotencyKeyConflict, in.RequestKey)
		}
		if old.TemplateID != nil {
			if err := CheckTemplateScope(ctx, *old.TemplateID); err != nil {
				return err
			}
		}
		if old.Jobs, err = listRequestJobsTx(ctx, d, tenant, taskID, old.ID); err != nil {
			return err
		}
		old.summarize()
		out.Request, out.Jobs, out.Replayed = old, old.Jobs, true
	case errors.Is(err, ErrNotFound):
		// 只有新请求需要执行版本：重放沿用原请求固定的版本，不受当前运行能力影响。
		if in.StrictRuntime && strings.TrimSpace(in.Executor) == "" {
			return fmt.Errorf("%w: a strict final draft needs the document engine executor fixed at submission", ErrInvalidExport)
		}
		if paused {
			return fmt.Errorf("%w: task is paused", ErrJobPrecondition)
		}
		if bound {
			if _, _, _, err := d.LockSubmittingIntent(ctx, taskID); err != nil {
				return err
			}
		}
		if err := d.insertExportRequestTx(ctx, tenant, taskID, in, out); err != nil {
			return err
		}
	default:
		return err
	}
	if !bound {
		return nil
	}
	for _, j := range out.Jobs {
		ver := int64(j.OutlineVersion)
		s, err := d.SubscribeAgentJob(ctx, tenant, taskID, SubscribeAgentJobInput{Job: JobRef{Kind: JobKindProposalExport, ID: j.ID},
			TargetRef: map[string]any{"export_request_id": out.Request.ID, "format": j.Format, "outline_version": j.OutlineVersion,
				"strict_runtime": j.StrictRuntime}, ExpectedVersion: &ver, Actor: in.Actor})
		if err != nil {
			return err
		}
		out.Subscriptions = append(out.Subscriptions, s)
	}
	return nil
}

// insertExportRequestTx 解析当前批准大纲并在同一快照内重验门禁、固化快照、登记请求组与全部格式作业。
func (d *DB) insertExportRequestTx(ctx context.Context, tenant string, taskID int64, in SubmitExportInput, out *ExportSubmission) error {
	var active int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM export_jobs WHERE tenant_id = $1 AND task_id = $2 AND status IN ('queued','running')`,
		tenant, taskID).Scan(&active); err != nil {
		return err
	}
	if active+len(in.Formats) > maxActiveExportJobsPerTask {
		return fmt.Errorf("%w: at most %d queued or running export jobs per task", ErrJobCapacity, maxActiveExportJobsPerTask)
	}
	templateKey, templateHash := DefaultTemplateKey, in.DefaultTemplateHash
	var templateID any
	if in.TemplateID > 0 {
		t, err := getTemplateTx(ctx, d, tenant, in.TemplateID)
		if err != nil {
			return err
		}
		templateKey, templateHash, templateID = "template:"+strconv.FormatInt(t.ID, 10), t.SHA256, t.ID
	}
	approved, _, err := lockApprovedSectionsTx(ctx, d.tx, tenant, taskID, 0)
	if err != nil && !errors.Is(err, ErrOutlineNotApproved) {
		return err
	}
	if approved != nil && in.ExpectedOutlineVersion > 0 && approved.Version != in.ExpectedOutlineVersion {
		return fmt.Errorf("%w: expected_outline_version %d, the approved outline is v%d", ErrRevisionConflict, in.ExpectedOutlineVersion, approved.Version)
	}
	pre, charts, err := exportPreflightTx(ctx, d.tx, tenant, taskID, in.StrictRuntime)
	if err != nil {
		return err
	}
	if !pre.Readiness.Ready || approved == nil {
		return &ExportBlockedError{Preflight: pre}
	}
	var taskName string
	if err := d.QueryRowContext(ctx, `SELECT name FROM tasks WHERE id = $1`, taskID).Scan(&taskName); err != nil {
		return err
	}
	doc := DocSnapshot{TaskName: taskName, Bidder: in.Bidder, OutlineVersion: approved.Version, Sections: []SnapshotSection{}, CreatedAt: time.Now().UTC()}
	revs := map[string]int{}
	rows, err := d.QueryContext(ctx, `SELECT s.id, s.section_code, s.title, s.level, s.seq, s.approved_rev_no, r.id
		FROM proposal_sections s JOIN section_revisions r ON r.tenant_id = s.tenant_id AND r.section_id = s.id AND r.rev_no = s.approved_rev_no
		WHERE s.tenant_id = $1 AND s.outline_id = $2 ORDER BY s.seq`, tenant, approved.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var s SnapshotSection
		if err := rows.Scan(&s.SectionID, &s.Code, &s.Title, &s.Level, &s.Seq, &s.RevNo, &s.RevID); err != nil {
			rows.Close()
			return err
		}
		doc.Sections = append(doc.Sections, s)
		revs[strconv.FormatInt(s.SectionID, 10)] = s.RevNo
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	chartSnap := []SnapshotChart{}
	for _, c := range charts {
		chartSnap = append(chartSnap, SnapshotChart{ChartID: c.ID, SectionID: c.SectionID, RevNo: c.RevNo, ChartType: c.ChartType, Title: c.Title,
			SourceFormat: c.SourceFormat, Source: c.Source, SourceHash: c.SourceHash, RenderStatus: c.RenderStatus, RenderFormat: c.RenderFormat,
			RenderError: c.RenderError, ArtifactPath: c.ArtifactPath, ArtifactHash: c.ArtifactHash, WidthPx: c.WidthPx, HeightPx: c.HeightPx, Spec: c.Spec})
	}
	matrixRows, err := d.listMatrix(ctx, d, tenant, taskID, approved.ID, MatrixFilter{})
	if err != nil {
		return err
	}
	matrixSnap := []SnapshotMatrixRow{}
	for _, m := range matrixRows {
		if m.VerifyStatus != compliance.StatusVerified || m.RevNo == nil {
			continue // 就绪度已保证每条要求至少有一行 verified；同一要求映射多章节时其余行不进入偏离表
		}
		matrixSnap = append(matrixSnap, SnapshotMatrixRow{RequirementID: m.RequirementID, Code: m.RequirementCode, Title: m.RequirementTitle,
			Kind: m.Kind, IsMandatory: m.IsMandatory, SectionID: m.SectionID, SectionCode: m.SectionCode, SectionTitle: m.SectionTitle,
			RevNo: *m.RevNo, Anchor: m.Anchor, AnchorExcerpt: m.AnchorExcerpt, ResponseSummary: m.ResponseSummary, Deviation: m.Deviation,
			VerifyStatus: m.VerifyStatus})
	}
	policy := ExportPolicyPreviewV1
	if in.StrictRuntime {
		policy = ExportPolicyFinalV1
	}
	snapHash := exportSnapshotHash(templateKey, templateHash, approved.ID, approved.Version, doc.Sections, chartSnap, matrixSnap, in.Bidder, in.StrictRuntime, policy)
	formatsJSON, _ := json.Marshal(in.Formats)
	var expected any
	if in.ExpectedOutlineVersion > 0 {
		expected = in.ExpectedOutlineVersion
	}
	req, err := scanExportRequest(d.QueryRowContext(ctx, `INSERT INTO export_requests(tenant_id, task_id, request_key, request_hash, snapshot_hash, formats,
			template_id, template_key, template_hash, bidder, expected_outline_version, outline_id, outline_version, strict_runtime, policy_version,
			executor, deadline_at, created_by, updated_by)
		VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16, now() + make_interval(secs => $17), $18, $18)
		RETURNING `+exportRequestCols, tenant, taskID, in.RequestKey, in.RequestHash, snapHash, string(formatsJSON), templateID, templateKey, templateHash,
		in.Bidder, expected, approved.ID, approved.Version, in.StrictRuntime, policy, strings.TrimSpace(in.Executor), in.Deadline.Seconds(), in.Actor))
	if err != nil {
		return err
	}
	gate, _ := json.Marshal(pre)
	revJSON, _ := json.Marshal(revs)
	chartJSON, _ := json.Marshal(chartSnap)
	matrixJSON, _ := json.Marshal(matrixSnap)
	docJSON, _ := json.Marshal(doc)
	warnJSON, _ := json.Marshal(pre.Warnings)
	for _, f := range in.Formats {
		j, err := scanExportJob(d.QueryRowContext(ctx, `INSERT INTO export_jobs(tenant_id, task_id, outline_id, outline_version, format, template_id,
				template_key, template_hash, rev_snapshot, chart_snapshot, matrix_snapshot, doc_snapshot, gate, warnings, max_attempts, created_by, updated_by,
				export_request_id, strict_runtime, executor, deadline_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$16,$17,$18,$19,$20) RETURNING `+exportJobCols,
			tenant, taskID, approved.ID, approved.Version, f, templateID, templateKey, templateHash, string(revJSON), string(chartJSON),
			string(matrixJSON), string(docJSON), string(gate), string(warnJSON), in.MaxAttempts, in.Actor,
			req.ID, req.StrictRuntime, req.Executor, req.DeadlineAt))
		if err != nil {
			return err
		}
		req.Jobs = append(req.Jobs, j)
	}
	req.summarize()
	out.Request, out.Jobs, out.Preflight = req, req.Jobs, pre
	return nil
}

// exportSnapshotHash 是快照指纹：模板、批准大纲、锁定修订（含修订 ID）、图表（源码与产物哈希）、偏离表与策略。
func exportSnapshotHash(templateKey, templateHash string, outlineID int64, outlineVersion int, sections []SnapshotSection, charts []SnapshotChart,
	matrix []SnapshotMatrixRow, bidder string, strict bool, policy string) string {
	type chartPart struct {
		ID           int64  `json:"id"`
		RevNo        int    `json:"rev_no"`
		SourceHash   string `json:"source_hash"`
		RenderStatus string `json:"render_status"`
		ArtifactHash string `json:"artifact_hash"`
	}
	parts := make([]chartPart, 0, len(charts))
	for _, c := range charts {
		parts = append(parts, chartPart{c.ChartID, c.RevNo, c.SourceHash, c.RenderStatus, c.ArtifactHash})
	}
	b, _ := json.Marshal(map[string]any{"template_key": templateKey, "template_hash": templateHash, "outline_id": outlineID,
		"outline_version": outlineVersion, "sections": sections, "charts": parts, "matrix": matrix, "bidder": bidder, "strict": strict, "policy": policy})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func listRequestJobsTx(ctx context.Context, q queryer, tenant string, taskID, requestID int64) ([]*ExportJob, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+exportJobCols+` FROM export_jobs WHERE tenant_id = $1 AND task_id = $2 AND export_request_id = $3 ORDER BY id`,
		tenant, taskID, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*ExportJob{}
	for rows.Next() {
		j, err := scanExportJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 查询与取消
// ---------------------------------------------------------------------------

// checkRequestOwnedByIntent：宿主范围限定对象时，只能访问本 Intent 订阅过其作业的请求组。
func (d *DB) checkRequestOwnedByIntent(ctx context.Context, tenant string, taskID, requestID int64) error {
	p, ok := intentExecutionFrom(ctx)
	if !ok || !restrictsObjects(ctx) {
		return nil
	}
	var owned bool
	if err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agent_job_subscriptions s JOIN export_jobs j
			ON j.tenant_id = s.tenant_id AND j.task_id = s.task_id AND j.id = s.job_id
		WHERE s.tenant_id = $1 AND s.task_id = $2 AND s.intent_id = $3 AND s.job_kind = 'proposal_export' AND j.export_request_id = $4)`,
		tenantOr(tenant), taskID, p.ID, requestID).Scan(&owned); err != nil {
		return err
	}
	if !owned {
		return fmt.Errorf("execution scope: export request #%d is not subscribed by the current intent", requestID)
	}
	return nil
}

// FindExportRequest 按请求键查找请求组（不含作业；未找到返回 ErrNotFound）。
func (d *DB) FindExportRequest(ctx context.Context, tenant string, taskID int64, key string) (*ExportRequest, error) {
	return scanExportRequest(d.QueryRowContext(ctx, `SELECT `+exportRequestCols+` FROM export_requests WHERE tenant_id = $1 AND task_id = $2 AND request_key = $3`,
		tenantOr(tenant), taskID, strings.TrimSpace(key)))
}

// GetExportRequest 读取任务内一个请求组及其作业。宿主绑定 Intent 的执行受作业类型与对象范围约束。
func (d *DB) GetExportRequest(ctx context.Context, tenant string, taskID, id int64) (*ExportRequest, error) {
	tenant = tenantOr(tenant)
	if err := CheckJobKindScope(ctx, JobKindProposalExport); err != nil {
		return nil, err
	}
	if err := d.checkRequestOwnedByIntent(ctx, tenant, taskID, id); err != nil {
		return nil, err
	}
	q, err := scanExportRequest(d.QueryRowContext(ctx, `SELECT `+exportRequestCols+` FROM export_requests WHERE tenant_id = $1 AND task_id = $2 AND id = $3`,
		tenant, taskID, id))
	if err != nil {
		return nil, err
	}
	if q.Jobs, err = listRequestJobsTx(ctx, d, tenant, taskID, id); err != nil {
		return nil, err
	}
	q.summarize()
	return q, nil
}

// ListExportRequests 列出任务内的请求组（新者在前，Before 为请求 ID 游标），每组附带作业（HTTP 页面恢复）。
func (d *DB) ListExportRequests(ctx context.Context, tenant string, taskID, before int64, limit int) ([]*ExportRequest, error) {
	tenant = tenantOr(tenant)
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	rows, err := d.QueryContext(ctx, `SELECT `+exportRequestCols+` FROM export_requests WHERE tenant_id = $1 AND task_id = $2
		AND ($3::bigint = 0 OR id < $3) ORDER BY id DESC LIMIT $4`, tenant, taskID, before, limit)
	if err != nil {
		return nil, err
	}
	out := []*ExportRequest{}
	ids := []int64{}
	byID := map[int64]*ExportRequest{}
	for rows.Next() {
		q, err := scanExportRequest(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, q)
		ids = append(ids, q.ID)
		byID[q.ID] = q
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(ids) == 0 {
		return out, err
	}
	jrows, err := d.QueryContext(ctx, `SELECT `+exportJobCols+` FROM export_jobs WHERE tenant_id = $1 AND task_id = $2
		AND export_request_id = ANY($3::bigint[]) ORDER BY id`, tenant, taskID, pgInt64Array(ids))
	if err != nil {
		return nil, err
	}
	defer jrows.Close()
	for jrows.Next() {
		j, err := scanExportJob(jrows)
		if err != nil {
			return nil, err
		}
		if q := byID[*j.ExportRequestID]; q != nil {
			q.Jobs = append(q.Jobs, j)
		}
	}
	for _, q := range out {
		q.summarize()
	}
	return out, jrows.Err()
}

// GetExportJobScoped 读取一个导出作业；宿主绑定 Intent 的执行受作业类型与对象范围约束（Agent 工具使用）。
func (d *DB) GetExportJobScoped(ctx context.Context, tenant string, taskID, id int64) (*ExportJob, error) {
	if err := CheckJobKindScope(ctx, JobKindProposalExport); err != nil {
		return nil, err
	}
	if err := d.checkJobOwnedByIntent(ctx, tenant, taskID, JobRef{Kind: JobKindProposalExport, ID: id}); err != nil {
		return nil, err
	}
	return d.GetExportJob(ctx, tenant, taskID, id)
}

// ExportCancelResult 是取消中一个作业的结果：Cancelled 表示本次调用将其取消，否则作业已是终态（原样返回）。
type ExportCancelResult struct {
	JobID     int64  `json:"job_id"`
	Format    string `json:"format"`
	Status    string `json:"status"`
	Cancelled bool   `json:"cancelled"`
}

// CancelExportJob 取消排队 / 执行中的导出作业：写终态、推进令牌，旧执行者的成功、失败与心跳都无法写回；已终态时原样返回。
// 范围约束同 GetExportJobScoped。
func (d *DB) CancelExportJob(ctx context.Context, tenant string, taskID, id int64, actor string) (*ExportJob, bool, error) {
	tenant = tenantOr(tenant)
	if _, err := d.GetExportJobScoped(ctx, tenant, taskID, id); err != nil {
		return nil, false, err
	}
	res, err := d.cancelExportJobs(ctx, tenant, taskID, []int64{id}, actor)
	if err != nil {
		return nil, false, err
	}
	j, err := d.GetExportJob(ctx, tenant, taskID, id)
	return j, len(res) == 1 && res[0].Cancelled, err
}

// CancelExportRequest 逐项取消请求组内未终态的作业，返回每个作业的结果（已终态的原样列出）。范围约束同 GetExportRequest。
func (d *DB) CancelExportRequest(ctx context.Context, tenant string, taskID, requestID int64, actor string) (*ExportRequest, []ExportCancelResult, error) {
	tenant = tenantOr(tenant)
	q, err := d.GetExportRequest(ctx, tenant, taskID, requestID)
	if err != nil {
		return nil, nil, err
	}
	ids := make([]int64, 0, len(q.Jobs))
	for _, j := range q.Jobs {
		ids = append(ids, j.ID)
	}
	res, err := d.cancelExportJobs(ctx, tenant, taskID, ids, actor)
	if err != nil {
		return nil, nil, err
	}
	q, err = d.GetExportRequest(ctx, tenant, taskID, requestID)
	return q, res, err
}

// cancelExportJobs 在一个事务内按 ID 升序取消作业。锁序：任务行（共享）→ 作业。终态触发器在同一事务内发布 cancelled 事件。
func (d *DB) cancelExportJobs(ctx context.Context, tenant string, taskID int64, ids []int64, actor string) ([]ExportCancelResult, error) {
	ids = slices.Clone(ids)
	slices.Sort(ids)
	msg := "已取消"
	if who := strings.TrimSpace(actor); who != "" {
		msg += "（" + truncateRunes(who, 60) + "）"
	}
	var out []ExportCancelResult
	err := d.Transaction(ctx, func(tx *DB) error {
		out = nil
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id = $1 FOR SHARE`, taskID).Scan(&one); err != nil {
			return err
		}
		for _, id := range ids {
			var r ExportCancelResult
			err := tx.QueryRowContext(ctx, `SELECT id, format, status FROM export_jobs WHERE tenant_id = $1 AND task_id = $2 AND id = $3 FOR UPDATE`,
				tenant, taskID, id).Scan(&r.JobID, &r.Format, &r.Status)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: export job %d", ErrNotFound, id)
			}
			if err != nil {
				return err
			}
			if r.Status == ExportQueued || r.Status == ExportRunning {
				if _, err := tx.ExecContext(ctx, `UPDATE export_jobs SET status = 'cancelled', error_code = 'CANCELLED', error = $2, lease_token = lease_token + 1,
					lease_until = NULL, lease_owner = '', finished_at = now(), updated_by = $3 WHERE id = $1`, id, msg, actor); err != nil {
					return err
				}
				r.Status, r.Cancelled = ExportCancelled, true
			}
			out = append(out, r)
		}
		return nil
	})
	return out, err
}
