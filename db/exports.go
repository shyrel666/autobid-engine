package db

// Phase 6B（导出附件域）：锁定版本导出任务 export_jobs 与套版模板 export_templates。
//
// 门禁（CreateExportJobs）：在一个 REPEATABLE READ 事务内按「批准大纲 FOR SHARE -> 章节 FOR SHARE」加锁
// （与 SaveSectionRevision / 矩阵核验同序），调用 6A 的 exportReadinessTx；有任何阻断原因即返回 *ExportBlockedError（HTTP 422），
// 否则在同一事务、同一快照内固化：
//   rev_snapshot     section_id -> rev_no（全部章节的批准修订）；
//   doc_snapshot     章节顺序 / 层级 / 标题 / 修订 ID（修订正文不可篡改，渲染时按 ID 读取）；
//   chart_snapshot   与锁定修订一致的图表（源码、渲染状态与产物哈希）；
//   matrix_snapshot  《点对点应答及偏离表》数据（compliance_matrix 行会被原地重检，必须固化）。
// 快照列由触发器保证不可修改，导出后再编辑章节不影响已创建任务。
//
// 执行（ClaimExportJob / HeartbeatExportJob / CompleteExportJob / FailExportJob）：lease_token 是围栏令牌，每次领取递增；
// running 且租约过期的任务可被再次领取（服务重启后的回收重试），旧执行者的心跳与完成写入因令牌不符被拒绝（ErrLeaseLost）。
// S4 起：请求组与幂等见 export_requests.go；暂停任务的作业不被领取、在途执行失去写回资格；截止时间（首次提交固定）
// 与尝试预算耗尽时以 BUDGET_EXHAUSTED 结束；取消写终态并推进令牌，终态之后状态不再变化（schema 触发器）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// 导出任务状态与格式。
const (
	ExportQueued    = "queued"
	ExportRunning   = "running"
	ExportSucceeded = "succeeded"
	ExportFailed    = "failed"
	ExportCancelled = "cancelled"

	ExportDOCX = "docx"
	ExportPDF  = "pdf"

	DefaultTemplateKey = "default"
)

var (
	ErrInvalidExport  = errors.New("export: invalid request")
	ErrTemplateInUse  = errors.New("export: template is referenced by export jobs")
	ErrExportNotReady = errors.New("export: job has no artifact")
)

// ExportBlockedError 表示导出门禁未通过；Preflight 列出全部阻断原因。
type ExportBlockedError struct {
	Preflight *ExportPreflight
}

func (e *ExportBlockedError) Error() string {
	return fmt.Sprintf("export blocked: %d blocker(s)", len(e.Preflight.Readiness.Blockers))
}

// ExportWarning 是不阻断导出、但会体现在成稿里的提示（未渲染图表、未纳入的过期图表等）。
type ExportWarning struct {
	Kind      string `json:"kind"`
	Message   string `json:"message"`
	SectionID int64  `json:"section_id,omitempty"`
	ChartID   int64  `json:"chart_id,omitempty"`
}

// 提示类型。
const (
	WarnChartUnrendered = "chart_unrendered"
	WarnChartFailed     = "chart_failed"
	WarnChartStale      = "chart_stale"
	WarnTOCNotRefreshed = "toc_not_refreshed"
	WarnChartMissing    = "chart_artifact_missing"
	WarnRetried         = "retried"
	WarnSelfCheck       = "self_check"
)

// ExportPreflight 是导出门禁明细：6A 就绪度（阻断）+ 6B 图表提示（不阻断）。StrictRuntime 为真时按严格最终稿策略判定，
// 未达最终状态的图表另记为阻断（BlockChartNotFinal）。
type ExportPreflight struct {
	Readiness     *ExportReadiness `json:"readiness"`
	Warnings      []ExportWarning  `json:"warnings"`
	Charts        int              `json:"charts"`
	StrictRuntime bool             `json:"strict_runtime"`
}

// SnapshotSection 是导出快照中的一个章节。
type SnapshotSection struct {
	SectionID int64  `json:"section_id"`
	Code      string `json:"section_code"`
	Title     string `json:"title"`
	Level     int    `json:"level"`
	Seq       int    `json:"seq"`
	RevNo     int    `json:"rev_no"`
	RevID     int64  `json:"rev_id"`
}

// DocSnapshot 是导出时的文档骨架。
type DocSnapshot struct {
	TaskName       string            `json:"task_name"`
	Bidder         string            `json:"bidder,omitempty"`
	OutlineVersion int               `json:"outline_version"`
	Sections       []SnapshotSection `json:"sections"`
	CreatedAt      time.Time         `json:"created_at"`
}

// SnapshotChart 是导出快照中的一张图表。
type SnapshotChart struct {
	ChartID      int64           `json:"chart_id"`
	SectionID    int64           `json:"section_id"`
	RevNo        int             `json:"rev_no"`
	ChartType    string          `json:"chart_type"`
	Title        string          `json:"title"`
	SourceFormat string          `json:"source_format"`
	Source       string          `json:"source"`
	SourceHash   string          `json:"source_hash"`
	RenderStatus string          `json:"render_status"`
	RenderFormat string          `json:"render_format"`
	RenderError  string          `json:"render_error,omitempty"`
	ArtifactPath string          `json:"artifact_path,omitempty"`
	ArtifactHash string          `json:"artifact_hash,omitempty"`
	WidthPx      int             `json:"width_px,omitempty"`
	HeightPx     int             `json:"height_px,omitempty"`
	Spec         json.RawMessage `json:"spec,omitempty"`
}

// SnapshotMatrixRow 是《点对点应答及偏离表》的一行。
type SnapshotMatrixRow struct {
	RequirementID   int64  `json:"requirement_item_id"`
	Code            string `json:"requirement_code"`
	Title           string `json:"requirement_title"`
	Kind            string `json:"kind"`
	IsMandatory     bool   `json:"is_mandatory"`
	SectionID       int64  `json:"section_id"`
	SectionCode     string `json:"section_code"`
	SectionTitle    string `json:"section_title"`
	RevNo           int    `json:"rev_no"`
	Anchor          string `json:"anchor"`
	AnchorExcerpt   string `json:"anchor_excerpt"`
	ResponseSummary string `json:"response_summary"`
	Deviation       string `json:"deviation"`
	VerifyStatus    string `json:"verify_status"`
}

// SnapshotSectionRef 是附表中对一个章节的引用（页码由成稿中的书签解析）。
type SnapshotSectionRef struct {
	SectionID int64  `json:"section_id"`
	Code      string `json:"section_code"`
	Title     string `json:"title"`
}

// SnapshotScoreRow 是《评分索引表》的一行：技术 / 商务评分项及响应它的章节（按评分表顺序）。
type SnapshotScoreRow struct {
	RequirementID int64                `json:"requirement_item_id"`
	Code          string               `json:"requirement_code"`
	Title         string               `json:"requirement_title"`
	ScoreGroup    string               `json:"score_group,omitempty"`
	EvalKind      string               `json:"eval_kind,omitempty"`
	Score         float64              `json:"score"`
	Sections      []SnapshotSectionRef `json:"sections"`
}

// SnapshotOriginal 是证明材料的一份原件定位（原件文件内的页码范围，不是成稿页码）。
type SnapshotOriginal struct {
	DocumentKind string `json:"document_kind"`
	PageLabel    string `json:"page_label,omitempty"`
	SHA256       string `json:"sha256"`
	PageStart    int    `json:"page_start"`
	PageEnd      int    `json:"page_end"`
}

// SnapshotEvidence 是《证明材料索引》的一行：锁定修订引用的、当前可采的企业证据。
type SnapshotEvidence struct {
	Domain           string               `json:"evidence_domain"`
	EvidenceID       int64                `json:"evidence_id"`
	Code             string               `json:"code"`
	Name             string               `json:"name"`
	Number           string               `json:"number,omitempty"`
	Subject          string               `json:"subject,omitempty"`
	Originals        []SnapshotOriginal   `json:"originals"`
	RequirementCodes []string             `json:"requirement_codes"`
	Sections         []SnapshotSectionRef `json:"sections"`
}

// SnapshotCommitment 是《承诺事项一览表》的一行：锁定修订引用的、已确认或已会签的承诺台账条目。
type SnapshotCommitment struct {
	ID              int64                `json:"id"`
	Code            string               `json:"code"`
	Version         int                  `json:"version"`
	Metric          string               `json:"metric"`
	MetricLabel     string               `json:"metric_label"`
	Unit            string               `json:"unit,omitempty"`
	TenderThreshold string               `json:"tender_threshold,omitempty"`
	Committed       string               `json:"committed"`
	Status          string               `json:"status"`
	RequirementCode string               `json:"requirement_code,omitempty"`
	Sections        []SnapshotSectionRef `json:"sections"`
}

// PackageSnapshot 是 D4 成稿附表的快照（与 doc / chart / matrix 快照同一事务固化，不可修改）。S4 及更早的作业为空。
type PackageSnapshot struct {
	ScoreIndex  []SnapshotScoreRow   `json:"score_index,omitempty"`
	Evidence    []SnapshotEvidence   `json:"evidence,omitempty"`
	Commitments []SnapshotCommitment `json:"commitments,omitempty"`
}

// ExportSelfCheck 是成稿回读自检的结构化结果，随成功结果一次写入 export_jobs.self_check（之后不可修改）。
// 提交检查清单（submission.go）的系统可查项由它判定。LayoutChecked 为假表示没有 PDF 版面可核对（未刷新的预览稿）。
type ExportSelfCheck struct {
	Pages            int    `json:"pages,omitempty"`
	TOCEntries       int    `json:"toc_entries"`
	TOCIssues        int    `json:"toc_issues"`
	ScoreRows        int    `json:"score_rows"`
	PageRefs         int    `json:"page_refs"`
	PageRefsVerified int    `json:"page_refs_verified"`
	PageRefIssues    int    `json:"page_ref_issues"`
	VerifiedAgainst  string `json:"verified_against,omitempty"` // pdf（同一次刷新的 PDF 版面）/ toc（目录条目）
	MetadataChecked  bool   `json:"metadata_checked"`
	MetadataIssues   int    `json:"metadata_issues"`
	Author           string `json:"author,omitempty"`
	LastModifiedBy   string `json:"last_modified_by,omitempty"`
	LayoutChecked    bool   `json:"layout_checked"`
	OutOfBounds      int    `json:"out_of_bounds"`
	BlankPages       int    `json:"blank_pages"`
	FooterIssues     int    `json:"footer_issues"`
	ImageIssues      int    `json:"image_issues"`
	Images           int    `json:"images"`
	Tables           int    `json:"tables"`
}

// ExportJob 是一次导出任务。BusinessState 区分作业状态与业务状态：成功作业只有严格最终稿（文档引擎已刷新目录并记录引擎）
// 才是 final_ready，其余成功产物（含 S4 之前的旧作业）一律 preview_only。
type ExportJob struct {
	ID              int64               `json:"id"`
	TaskID          int64               `json:"task_id"`
	ExportRequestID *int64              `json:"export_request_id,omitempty"`
	StrictRuntime   bool                `json:"strict_runtime"`
	BusinessState   string              `json:"business_state"`
	Executor        string              `json:"executor,omitempty"`
	DeadlineAt      *time.Time          `json:"deadline_at,omitempty"`
	ErrorCode       string              `json:"error_code,omitempty"`
	OfficeEngine    string              `json:"office_engine,omitempty"`
	OfficeVersion   string              `json:"office_version,omitempty"`
	OutlineID       int64               `json:"outline_id"`
	OutlineVersion  int                 `json:"outline_version"`
	Format          string              `json:"format"`
	Status          string              `json:"status"`
	TemplateID      *int64              `json:"template_id,omitempty"`
	TemplateKey     string              `json:"template_key"`
	TemplateHash    string              `json:"template_hash"`
	RevSnapshot     map[string]int      `json:"rev_snapshot"`
	Charts          []SnapshotChart     `json:"chart_snapshot"`
	Matrix          []SnapshotMatrixRow `json:"matrix_snapshot"`
	Doc             DocSnapshot         `json:"doc_snapshot"`
	Package         PackageSnapshot     `json:"package_snapshot"`
	SelfCheck       *ExportSelfCheck    `json:"self_check,omitempty"`
	Gate            json.RawMessage     `json:"gate"`
	ArtifactPath    string              `json:"-"`
	ArtifactName    string              `json:"artifact_name"`
	ArtifactHash    string              `json:"artifact_hash"`
	ArtifactSize    int64               `json:"artifact_size"`
	TOCRefreshed    bool                `json:"toc_refreshed"`
	Warnings        []ExportWarning     `json:"warnings"`
	Error           string              `json:"error"`
	Attempts        int                 `json:"attempts"`
	MaxAttempts     int                 `json:"max_attempts"`
	LeaseOwner      string              `json:"lease_owner,omitempty"`
	LeaseToken      int64               `json:"lease_token"`
	LeaseUntil      *time.Time          `json:"lease_until,omitempty"`
	StartedAt       *time.Time          `json:"started_at,omitempty"`
	FinishedAt      *time.Time          `json:"finished_at,omitempty"`
	CreatedBy       string              `json:"created_by"`
	CreatedAt       time.Time           `json:"created_at"`
	UpdatedAt       time.Time           `json:"updated_at"`
}

// ExportTemplate 是上传的套版模板。
type ExportTemplate struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	FileName  string          `json:"file_name"`
	Path      string          `json:"-"`
	SHA256    string          `json:"sha256"`
	Size      int64           `json:"size"`
	Meta      json.RawMessage `json:"meta"`
	CreatedBy string          `json:"created_by"`
	CreatedAt time.Time       `json:"created_at"`
}

// CreateExportInput 是一次导出请求：可同时创建多种格式（共享同一快照）。
type CreateExportInput struct {
	Formats             []string
	TemplateID          int64  // 0 表示内置默认模板
	DefaultTemplateHash string // 内置默认模板的 SHA-256（由 export 包提供）
	Bidder              string // 封面投标人名称（写入快照）
	MaxAttempts         int
}

const exportJobCols = `id, task_id, outline_id, outline_version, format, status, template_id, template_key, template_hash, rev_snapshot,
	chart_snapshot, matrix_snapshot, doc_snapshot, gate, artifact_path, artifact_name, artifact_hash, artifact_size, toc_refreshed, warnings,
	error, attempts, max_attempts, lease_owner, lease_token, lease_until, started_at, finished_at, created_by, created_at, updated_at,
	export_request_id, strict_runtime, executor, deadline_at, error_code, office_engine, office_version, package_snapshot, self_check`

// 导出业务状态（作业成功之后的含义；未成功时与作业状态相同）。
const (
	ExportFinalReady  = "final_ready"
	ExportPreviewOnly = "preview_only"
)

func scanExportJob(sc rowScanner) (*ExportJob, error) {
	var j ExportJob
	var tpl, reqID sql.NullInt64
	var revs, charts, matrix, doc, gate, warns, pkg, selfCheck []byte
	var leaseUntil, started, finished, deadline sql.NullTime
	if err := sc.Scan(&j.ID, &j.TaskID, &j.OutlineID, &j.OutlineVersion, &j.Format, &j.Status, &tpl, &j.TemplateKey, &j.TemplateHash, &revs,
		&charts, &matrix, &doc, &gate, &j.ArtifactPath, &j.ArtifactName, &j.ArtifactHash, &j.ArtifactSize, &j.TOCRefreshed, &warns,
		&j.Error, &j.Attempts, &j.MaxAttempts, &j.LeaseOwner, &j.LeaseToken, &leaseUntil, &started, &finished, &j.CreatedBy,
		&j.CreatedAt, &j.UpdatedAt, &reqID, &j.StrictRuntime, &j.Executor, &deadline, &j.ErrorCode, &j.OfficeEngine, &j.OfficeVersion, &pkg, &selfCheck); err != nil {
		return nil, err
	}
	if tpl.Valid {
		v := tpl.Int64
		j.TemplateID = &v
	}
	if reqID.Valid {
		v := reqID.Int64
		j.ExportRequestID = &v
	}
	j.DeadlineAt = nullTimePtr(deadline)
	j.BusinessState = j.Status
	if j.Status == ExportSucceeded {
		j.BusinessState = ExportPreviewOnly
		if j.StrictRuntime && j.TOCRefreshed && j.OfficeEngine != "" {
			j.BusinessState = ExportFinalReady
		}
	}
	for _, x := range []struct {
		raw []byte
		v   any
	}{{revs, &j.RevSnapshot}, {charts, &j.Charts}, {matrix, &j.Matrix}, {doc, &j.Doc}, {warns, &j.Warnings}, {pkg, &j.Package}} {
		if err := json.Unmarshal(x.raw, x.v); err != nil {
			return nil, fmt.Errorf("export job %d snapshot: %w", j.ID, err)
		}
	}
	if j.Charts == nil {
		j.Charts = []SnapshotChart{}
	}
	if j.Matrix == nil {
		j.Matrix = []SnapshotMatrixRow{}
	}
	if j.Warnings == nil {
		j.Warnings = []ExportWarning{}
	}
	if len(selfCheck) > 2 { // 未写入时为 {}
		var sc ExportSelfCheck
		if err := json.Unmarshal(selfCheck, &sc); err != nil {
			return nil, fmt.Errorf("export job %d self_check: %w", j.ID, err)
		}
		j.SelfCheck = &sc
	}
	j.Gate = json.RawMessage(gate)
	j.LeaseUntil, j.StartedAt, j.FinishedAt = nullTimePtr(leaseUntil), nullTimePtr(started), nullTimePtr(finished)
	return &j, nil
}

// ---------------------------------------------------------------------------
// 门禁与快照
// ---------------------------------------------------------------------------

// chartWarningsTx 汇总批准大纲下图表的不阻断提示：绑定旧修订（不纳入导出）、未渲染 / 渲染失败（导出时插入源码占位框）。
func chartWarningsTx(ctx context.Context, q queryer, tenant string, taskID, outlineID int64) ([]ExportWarning, []*ProposalChart, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+chartCols+chartFrom+` WHERE c.tenant_id = $1 AND c.task_id = $2 AND s.outline_id = $3
		ORDER BY s.seq, c.id`, tenant, taskID, outlineID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	warns := []ExportWarning{}
	var current []*ProposalChart
	for rows.Next() {
		c, err := scanChart(rows)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case c.Stale:
			warns = append(warns, ExportWarning{Kind: WarnChartStale, SectionID: c.SectionID, ChartID: c.ID,
				Message: fmt.Sprintf("图表 #%d《%s》%s，不纳入导出（可重新绑定或重新生成）", c.ID, c.Title, c.StaleReason)})
			continue
		case c.SourceFormat == ChartSourceJSON && c.RenderStatus == ChartPending:
			// 设备选型表按结构化 JSON 原生成表，不依赖渲染结果。
		case c.RenderStatus == ChartPending && c.LatestJob.Active():
			warns = append(warns, ExportWarning{Kind: WarnChartUnrendered, SectionID: c.SectionID, ChartID: c.ID,
				Message: fmt.Sprintf("图表 #%d《%s》渲染作业 #%d 仍在进行，现在导出将插入源码占位框", c.ID, c.Title, c.LatestJob.ID)})
		case c.RenderStatus == ChartUnrendered || c.RenderStatus == ChartPending:
			warns = append(warns, ExportWarning{Kind: WarnChartUnrendered, SectionID: c.SectionID, ChartID: c.ID,
				Message: fmt.Sprintf("图表 #%d《%s》未渲染（%s），导出时插入 Mermaid 源码占位框", c.ID, c.Title, orText(c.RenderError, "尚未渲染"))})
		case c.RenderStatus == ChartFailed:
			warns = append(warns, ExportWarning{Kind: WarnChartFailed, SectionID: c.SectionID, ChartID: c.ID,
				Message: fmt.Sprintf("图表 #%d《%s》渲染失败（%s），导出时插入 Mermaid 源码占位框", c.ID, c.Title, c.RenderError)})
		}
		current = append(current, c)
	}
	return warns, current, rows.Err()
}

func orText(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// ExportPreflight 返回导出门禁明细（单一快照只读，预览稿口径）。
func (d *DB) ExportPreflight(ctx context.Context, tenant string, taskID int64) (*ExportPreflight, error) {
	return d.ExportPreflightPolicy(ctx, tenant, taskID, false)
}

// ExportPreflightPolicy 按导出策略返回门禁明细；strict 为真时按严格最终稿策略另行阻断未达最终状态的图表。
// 预检不承诺提交成功：提交时在锁定快照的事务内重新校验。
func (d *DB) ExportPreflightPolicy(ctx context.Context, tenant string, taskID int64, strict bool) (*ExportPreflight, error) {
	tenant = tenantOr(tenant)
	if d.tx != nil {
		return nil, errors.New("db: ExportPreflight opens its own snapshot and must not run inside a transaction")
	}
	var out *ExportPreflight
	err := d.readSnapshot(ctx, func(tx *sql.Tx) error {
		var err error
		out, _, err = exportPreflightTx(ctx, tx, tenant, taskID, strict)
		return err
	})
	return out, err
}

func exportPreflightTx(ctx context.Context, q queryer, tenant string, taskID int64, strict bool) (*ExportPreflight, []*ProposalChart, error) {
	r, err := exportReadinessTx(ctx, q, tenant, taskID)
	if err != nil {
		return nil, nil, err
	}
	p := &ExportPreflight{Readiness: r, Warnings: []ExportWarning{}, StrictRuntime: strict}
	var outlineID int64
	err = q.QueryRowContext(ctx, `SELECT id FROM proposal_outlines WHERE tenant_id = $1 AND task_id = $2 AND status = 'approved'`, tenant, taskID).Scan(&outlineID)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	warns, charts, err := chartWarningsTx(ctx, q, tenant, taskID, outlineID)
	if err != nil {
		return nil, nil, err
	}
	p.Warnings, p.Charts = warns, len(charts)
	if strict {
		for _, b := range strictChartBlockers(charts) {
			r.block(b)
		}
		r.Ready = len(r.Blockers) == 0
	}
	return p, charts, nil
}

// strictChartBlockers：严格最终稿不允许源码占位框。纳入导出的每张图表都须是原生设备选型表，或已渲染且登记了哈希的 PNG；
// 已有合格 PNG 时即使重新渲染作业仍在进行或 mmdc 临时离线也不阻断（导出使用快照中的 PNG）。
func strictChartBlockers(charts []*ProposalChart) []ExportBlocker {
	var out []ExportBlocker
	for _, c := range charts {
		switch {
		case c.SourceFormat == ChartSourceJSON:
		case c.RenderStatus == ChartRendered && c.RenderFormat == ChartFormatPNG && c.ArtifactHash != "" && c.ArtifactPath != "":
		default:
			reason := orText(c.RenderError, "尚未渲染")
			if c.LatestJob.Active() {
				reason = fmt.Sprintf("渲染作业 #%d 仍在进行", c.LatestJob.ID)
			}
			out = append(out, ExportBlocker{Kind: BlockChartNotFinal, SectionID: c.SectionID, SectionCode: c.SectionCode,
				Message: fmt.Sprintf("图表 #%d《%s》没有可用的渲染图（%s）：最终稿不允许源码占位框，请先完成渲染或删除该图表", c.ID, c.Title, reason)})
		}
	}
	return out
}

// CreateExportJobs 是 S4 之前的兼容入口：以服务端保留命名空间生成一次性请求键，按预览稿策略（不强制文档引擎）提交
// 请求组，返回各格式作业。新入口（HTTP request_key / Agent 工具）使用 SubmitExport。
func (d *DB) CreateExportJobs(ctx context.Context, tenant string, taskID int64, actor string, in CreateExportInput) ([]*ExportJob, *ExportPreflight, error) {
	formats, err := NormalizeExportFormats(in.Formats)
	if err != nil {
		return nil, nil, err
	}
	key := fmt.Sprintf("%slegacy:%d:%s", ReservedRequestKeyPrefix, time.Now().UnixNano(), randomHex(6))
	sub, err := d.SubmitExport(ctx, tenant, taskID, SubmitExportInput{RequestKey: key, RequestHash: key, Formats: formats, TemplateID: in.TemplateID,
		DefaultTemplateHash: in.DefaultTemplateHash, Bidder: in.Bidder, MaxAttempts: in.MaxAttempts, Actor: actor, allowReserved: true})
	if err != nil {
		var be *ExportBlockedError
		if errors.As(err, &be) {
			return nil, be.Preflight, err
		}
		return nil, nil, err
	}
	return sub.Jobs, sub.Preflight, nil
}

// ---------------------------------------------------------------------------
// 查询
// ---------------------------------------------------------------------------

func (d *DB) GetExportJob(ctx context.Context, tenant string, taskID, id int64) (*ExportJob, error) {
	j, err := scanExportJob(d.QueryRowContext(ctx, `SELECT `+exportJobCols+` FROM export_jobs WHERE tenant_id = $1 AND task_id = $2 AND id = $3`,
		tenantOr(tenant), taskID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

func (d *DB) ListExportJobs(ctx context.Context, tenant string, taskID int64, limit int) ([]*ExportJob, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := d.QueryContext(ctx, `SELECT `+exportJobCols+` FROM export_jobs WHERE tenant_id = $1 AND task_id = $2 ORDER BY id DESC LIMIT $3`,
		tenantOr(tenant), taskID, limit)
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

// RevisionContents 按修订 ID 读取正文（修订不可篡改，导出渲染使用快照中的 ID）。
func (d *DB) RevisionContents(ctx context.Context, tenant string, taskID int64, revIDs []int64) (map[int64]string, error) {
	out := map[int64]string{}
	if len(revIDs) == 0 {
		return out, nil
	}
	rows, err := d.QueryContext(ctx, `SELECT id, content_md FROM section_revisions WHERE tenant_id = $1 AND task_id = $2 AND id = ANY($3::bigint[])`,
		tenantOr(tenant), taskID, pgInt64Array(revIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var c string
		if err := rows.Scan(&id, &c); err != nil {
			return nil, err
		}
		out[id] = c
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 执行：领取、心跳、完成、失败与回收
// ---------------------------------------------------------------------------

// ExportJobRef 是领取到的任务（含租户与围栏令牌）。
type ExportJobRef struct {
	Tenant string
	*ExportJob
}

// ClaimExportJob 领取一个排队中、或租约已过期的运行中任务（FOR UPDATE SKIP LOCKED）；没有可领取任务时返回 nil。
// 暂停任务的作业不被领取。领取前先结束已耗尽尝试次数或截止时间的作业（failed + BUDGET_EXHAUSTED，终态触发器发布事件）。
// 租约不超过截止时间。taskID > 0 时只领取该任务的导出（测试与单任务调试用），0 表示全部。
func (d *DB) ClaimExportJob(ctx context.Context, owner string, lease time.Duration, taskID int64) (*ExportJobRef, error) {
	if _, err := d.ExecContext(ctx, `UPDATE export_jobs j SET status = 'failed', finished_at = now(), lease_owner = '', lease_until = NULL,
			lease_token = j.lease_token + 1, error_code = 'BUDGET_EXHAUSTED',
			error = CASE WHEN j.error <> '' THEN j.error
			             WHEN j.attempts >= j.max_attempts THEN format('执行租约过期且已达最大重试次数 %s', j.max_attempts)
			             ELSE '已过首次提交时固定的截止时间' END
		FROM tasks t WHERE t.id = j.task_id AND NOT t.paused AND ($1::bigint = 0 OR j.task_id = $1)
		  AND (j.status = 'queued' OR (j.status = 'running' AND j.lease_until < now()))
		  AND (j.attempts >= j.max_attempts OR j.deadline_at <= now())`, taskID); err != nil {
		return nil, err
	}
	var tenant string
	row := d.QueryRowContext(ctx, `UPDATE export_jobs j SET status = 'running', attempts = j.attempts + 1, lease_token = j.lease_token + 1,
			lease_owner = $1, lease_until = LEAST(COALESCE(j.deadline_at, 'infinity'::timestamptz), now() + make_interval(secs => $2)),
			started_at = COALESCE(j.started_at, now()),
			warnings = CASE WHEN j.status = 'running' THEN j.warnings || jsonb_build_array(jsonb_build_object('kind', 'retried',
				'message', format('第 %s 次执行的租约（%s）已过期，任务被回收重试', j.attempts, j.lease_owner))) ELSE j.warnings END
		WHERE j.id = (SELECT x.id FROM export_jobs x JOIN tasks t ON t.id = x.task_id
			WHERE NOT t.paused AND (x.status = 'queued' OR (x.status = 'running' AND x.lease_until < now())) AND x.attempts < x.max_attempts
			  AND (x.deadline_at IS NULL OR x.deadline_at > now()) AND ($3::bigint = 0 OR x.task_id = $3)
			ORDER BY x.id FOR UPDATE OF x SKIP LOCKED LIMIT 1)
		RETURNING j.tenant_id, `+prefixCols("j.", exportJobCols), owner, lease.Seconds(), taskID)
	var j *ExportJob
	j, err := scanExportJob(rowPrefixScanner{row: row, first: &tenant})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ExportJobRef{Tenant: tenant, ExportJob: j}, nil
}

// HeartbeatExportJob 延长租约（不超过截止时间）；令牌不符（已被回收、取消）、截止时间已过或任务已暂停时返回 ErrLeaseLost，
// 执行者应立即停止。
func (d *DB) HeartbeatExportJob(ctx context.Context, id, token int64, lease time.Duration) error {
	res, err := d.ExecContext(ctx, `UPDATE export_jobs j SET lease_until = LEAST(COALESCE(j.deadline_at, 'infinity'::timestamptz), now() + make_interval(secs => $3))
		FROM tasks t WHERE j.id = $1 AND j.lease_token = $2 AND j.status = 'running' AND (j.deadline_at IS NULL OR j.deadline_at > now())
		  AND t.id = j.task_id AND NOT t.paused`, id, token, lease.Seconds())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrLeaseLost
	}
	return nil
}

// ExportResult 是一次成功执行的产物。OfficeEngine / OfficeVersion 记录实际刷新文档的引擎（严格最终稿必填）。
type ExportResult struct {
	ArtifactPath  string
	ArtifactName  string
	ArtifactHash  string
	ArtifactSize  int64
	TOCRefreshed  bool
	OfficeEngine  string
	OfficeVersion string
	Warnings      []ExportWarning
	SelfCheck     *ExportSelfCheck // 回读自检结果（未回读时为 nil）
}

// lockRunningExportJobTx 是写回的围栏：任务行（共享，与暂停串行化）未暂停，作业仍以该令牌运行且未过截止时间。
func lockRunningExportJobTx(ctx context.Context, tx *DB, id, token int64) error {
	var paused bool
	err := tx.QueryRowContext(ctx, `SELECT t.paused FROM export_jobs j JOIN tasks t ON t.id = j.task_id WHERE j.id = $1 FOR SHARE OF t`, id).Scan(&paused)
	if errors.Is(err, sql.ErrNoRows) || paused {
		return ErrLeaseLost
	}
	if err != nil {
		return err
	}
	var ok int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM export_jobs WHERE id = $1 AND lease_token = $2 AND status = 'running'
		AND (deadline_at IS NULL OR deadline_at > now()) FOR UPDATE`, id, token).Scan(&ok)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLeaseLost
	}
	return err
}

// CompleteExportJob 以围栏令牌提交成功结果；令牌不符、作业已取消、任务已暂停或截止时间已过时返回 ErrLeaseLost（结果不写入）。
// 严格最终稿作业未刷新目录或未记录引擎时数据库拒绝写入（ck_export_jobs_final）。
func (d *DB) CompleteExportJob(ctx context.Context, id, token int64, r ExportResult) error {
	warns, _ := json.Marshal(r.Warnings)
	if r.Warnings == nil {
		warns = []byte("[]")
	}
	check := []byte("{}")
	if r.SelfCheck != nil {
		check, _ = json.Marshal(r.SelfCheck)
	}
	return d.Transaction(ctx, func(tx *DB) error {
		if err := lockRunningExportJobTx(ctx, tx, id, token); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE export_jobs SET status = 'succeeded', artifact_path = $3, artifact_name = $4, artifact_hash = $5,
			artifact_size = $6, toc_refreshed = $7, office_engine = $8, office_version = $9, warnings = warnings || $10::jsonb, error = '', error_code = '',
			self_check = $11::jsonb, finished_at = now(), lease_until = NULL, lease_owner = ''
			WHERE id = $1 AND lease_token = $2`, id, token, r.ArtifactPath, r.ArtifactName, r.ArtifactHash, r.ArtifactSize,
			r.TOCRefreshed, r.OfficeEngine, r.OfficeVersion, string(warns), string(check))
		return err
	})
}

// FailExportJob 记录失败：retry 且尚有重试次数时回到 queued，否则置为 failed。
func (d *DB) FailExportJob(ctx context.Context, id, token int64, reason string, retry bool) error {
	return d.FailExportJobCode(ctx, id, token, "", reason, retry)
}

// FailExportJobCode 同 FailExportJob 并记录错误码（RUNTIME_UNAVAILABLE、READBACK_FAILED 等）；围栏同 CompleteExportJob。
// 可重试失败在尝试预算之内回到 queued（不延长原截止时间，领取时再判定是否已过期），否则以终态 failed 结束。
func (d *DB) FailExportJobCode(ctx context.Context, id, token int64, code, reason string, retry bool) error {
	return d.Transaction(ctx, func(tx *DB) error {
		if err := lockRunningExportJobTx(ctx, tx, id, token); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE export_jobs SET error = $3, error_code = $4, lease_until = NULL, lease_owner = '',
			status = CASE WHEN $5 AND attempts < max_attempts THEN 'queued' ELSE 'failed' END,
			finished_at = CASE WHEN $5 AND attempts < max_attempts THEN NULL ELSE now() END
			WHERE id = $1 AND lease_token = $2`, id, token, truncateRunes(reason, 2000), code, retry)
		return err
	})
}

// ReclaimExpiredExportJobs 把租约过期的 running 任务放回队列（尚有重试次数且未过截止时间）或置为 failed，返回回收的任务 ID。
// 服务启动时调用一次；运行期间 ClaimExportJob 也会直接领取过期任务。taskID > 0 时只回收该任务。
func (d *DB) ReclaimExpiredExportJobs(ctx context.Context, taskID int64) ([]int64, error) {
	rows, err := d.QueryContext(ctx, `UPDATE export_jobs SET
			status = CASE WHEN attempts < max_attempts AND (deadline_at IS NULL OR deadline_at > now()) THEN 'queued' ELSE 'failed' END,
			finished_at = CASE WHEN attempts < max_attempts AND (deadline_at IS NULL OR deadline_at > now()) THEN NULL ELSE now() END,
			error_code = CASE WHEN attempts < max_attempts AND (deadline_at IS NULL OR deadline_at > now()) THEN error_code ELSE 'BUDGET_EXHAUSTED' END,
			error = CASE WHEN attempts < max_attempts AND (deadline_at IS NULL OR deadline_at > now()) THEN error
			             WHEN attempts >= max_attempts THEN format('执行租约过期且已达最大重试次数 %s', max_attempts)
			             ELSE '执行租约过期且已过首次提交时固定的截止时间' END,
			warnings = warnings || jsonb_build_array(jsonb_build_object('kind', 'retried',
				'message', format('第 %s 次执行的租约（%s）已过期，任务已回收', attempts, lease_owner))),
			lease_until = NULL, lease_owner = ''
		WHERE status = 'running' AND lease_until < now() AND ($1::bigint = 0 OR task_id = $1) RETURNING id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ---------------------------------------------------------------------------
// 模板
// ---------------------------------------------------------------------------

const templateCols = `id, name, file_name, path, sha256, size, meta, created_by, created_at`

func scanTemplate(sc rowScanner) (*ExportTemplate, error) {
	var t ExportTemplate
	var meta []byte
	if err := sc.Scan(&t.ID, &t.Name, &t.FileName, &t.Path, &t.SHA256, &t.Size, &meta, &t.CreatedBy, &t.CreatedAt); err != nil {
		return nil, err
	}
	t.Meta = json.RawMessage(meta)
	return &t, nil
}

func getTemplateTx(ctx context.Context, q queryer, tenant string, id int64) (*ExportTemplate, error) {
	t, err := scanTemplate(q.QueryRowContext(ctx, `SELECT `+templateCols+` FROM export_templates WHERE tenant_id = $1 AND id = $2`, tenant, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

func (d *DB) GetExportTemplate(ctx context.Context, tenant string, id int64) (*ExportTemplate, error) {
	return getTemplateTx(ctx, d, tenantOr(tenant), id)
}

// CreateExportTemplate 登记模板；同一租户上传相同内容时返回已有记录。
func (d *DB) CreateExportTemplate(ctx context.Context, tenant, actor string, t ExportTemplate) (*ExportTemplate, error) {
	tenant = tenantOr(tenant)
	if len(t.Meta) == 0 {
		t.Meta = json.RawMessage(`{}`)
	}
	out, err := scanTemplate(d.QueryRowContext(ctx, `INSERT INTO export_templates(tenant_id, name, file_name, path, sha256, size, meta, created_by, updated_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$8) ON CONFLICT (tenant_id, sha256) DO UPDATE SET name = EXCLUDED.name, updated_by = EXCLUDED.updated_by
		RETURNING `+templateCols, tenant, t.Name, t.FileName, t.Path, t.SHA256, t.Size, string(t.Meta), actor))
	return out, err
}

func (d *DB) ListExportTemplates(ctx context.Context, tenant string) ([]*ExportTemplate, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+templateCols+` FROM export_templates WHERE tenant_id = $1 ORDER BY id DESC`, tenantOr(tenant))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*ExportTemplate{}
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteExportTemplate 删除模板；已有导出任务引用时返回 ErrTemplateInUse（任务快照依赖模板文件可复现）。
func (d *DB) DeleteExportTemplate(ctx context.Context, tenant string, id int64) (*ExportTemplate, error) {
	tenant = tenantOr(tenant)
	t, err := d.GetExportTemplate(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	if _, err := d.ExecContext(ctx, `DELETE FROM export_templates WHERE tenant_id = $1 AND id = $2`, tenant, id); err != nil {
		if isForeignKeyViolation(err) {
			return nil, ErrTemplateInUse
		}
		return nil, err
	}
	return t, nil
}

// prefixCols 给逗号分隔的列名加表别名前缀（UPDATE ... FROM 子查询的 RETURNING 需要限定列名）。
func prefixCols(p, cols string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = p + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}

// rowPrefixScanner 先把首列扫描到 first，其余列交给调用方。
type rowPrefixScanner struct {
	row   *sql.Row
	first any
}

func (p rowPrefixScanner) Scan(dest ...any) error {
	return p.row.Scan(append([]any{p.first}, dest...)...)
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
