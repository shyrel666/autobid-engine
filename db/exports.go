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

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/compliance"
	"github.com/jackc/pgx/v5/pgconn"
)

// 导出任务状态与格式。
const (
	ExportQueued    = "queued"
	ExportRunning   = "running"
	ExportSucceeded = "succeeded"
	ExportFailed    = "failed"

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

// ExportPreflight 是导出门禁明细：6A 就绪度（阻断）+ 6B 图表提示（不阻断）。
type ExportPreflight struct {
	Readiness *ExportReadiness `json:"readiness"`
	Warnings  []ExportWarning  `json:"warnings"`
	Charts    int              `json:"charts"`
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

// ExportJob 是一次导出任务。
type ExportJob struct {
	ID             int64               `json:"id"`
	TaskID         int64               `json:"task_id"`
	OutlineID      int64               `json:"outline_id"`
	OutlineVersion int                 `json:"outline_version"`
	Format         string              `json:"format"`
	Status         string              `json:"status"`
	TemplateID     *int64              `json:"template_id,omitempty"`
	TemplateKey    string              `json:"template_key"`
	TemplateHash   string              `json:"template_hash"`
	RevSnapshot    map[string]int      `json:"rev_snapshot"`
	Charts         []SnapshotChart     `json:"chart_snapshot"`
	Matrix         []SnapshotMatrixRow `json:"matrix_snapshot"`
	Doc            DocSnapshot         `json:"doc_snapshot"`
	Gate           json.RawMessage     `json:"gate"`
	ArtifactPath   string              `json:"-"`
	ArtifactName   string              `json:"artifact_name"`
	ArtifactHash   string              `json:"artifact_hash"`
	ArtifactSize   int64               `json:"artifact_size"`
	TOCRefreshed   bool                `json:"toc_refreshed"`
	Warnings       []ExportWarning     `json:"warnings"`
	Error          string              `json:"error"`
	Attempts       int                 `json:"attempts"`
	MaxAttempts    int                 `json:"max_attempts"`
	LeaseOwner     string              `json:"lease_owner,omitempty"`
	LeaseToken     int64               `json:"lease_token"`
	LeaseUntil     *time.Time          `json:"lease_until,omitempty"`
	StartedAt      *time.Time          `json:"started_at,omitempty"`
	FinishedAt     *time.Time          `json:"finished_at,omitempty"`
	CreatedBy      string              `json:"created_by"`
	CreatedAt      time.Time           `json:"created_at"`
	UpdatedAt      time.Time           `json:"updated_at"`
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
	error, attempts, max_attempts, lease_owner, lease_token, lease_until, started_at, finished_at, created_by, created_at, updated_at`

func scanExportJob(sc rowScanner) (*ExportJob, error) {
	var j ExportJob
	var tpl sql.NullInt64
	var revs, charts, matrix, doc, gate, warns []byte
	var leaseUntil, started, finished sql.NullTime
	if err := sc.Scan(&j.ID, &j.TaskID, &j.OutlineID, &j.OutlineVersion, &j.Format, &j.Status, &tpl, &j.TemplateKey, &j.TemplateHash, &revs,
		&charts, &matrix, &doc, &gate, &j.ArtifactPath, &j.ArtifactName, &j.ArtifactHash, &j.ArtifactSize, &j.TOCRefreshed, &warns,
		&j.Error, &j.Attempts, &j.MaxAttempts, &j.LeaseOwner, &j.LeaseToken, &leaseUntil, &started, &finished, &j.CreatedBy,
		&j.CreatedAt, &j.UpdatedAt); err != nil {
		return nil, err
	}
	if tpl.Valid {
		v := tpl.Int64
		j.TemplateID = &v
	}
	for _, x := range []struct {
		raw []byte
		v   any
	}{{revs, &j.RevSnapshot}, {charts, &j.Charts}, {matrix, &j.Matrix}, {doc, &j.Doc}, {warns, &j.Warnings}} {
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

// ExportPreflight 返回导出门禁明细（单一快照只读）。
func (d *DB) ExportPreflight(ctx context.Context, tenant string, taskID int64) (*ExportPreflight, error) {
	tenant = tenantOr(tenant)
	var out *ExportPreflight
	err := d.readSnapshot(ctx, func(tx *sql.Tx) error {
		var err error
		out, _, err = exportPreflightTx(ctx, tx, tenant, taskID)
		return err
	})
	return out, err
}

func exportPreflightTx(ctx context.Context, q queryer, tenant string, taskID int64) (*ExportPreflight, []*ProposalChart, error) {
	r, err := exportReadinessTx(ctx, q, tenant, taskID)
	if err != nil {
		return nil, nil, err
	}
	p := &ExportPreflight{Readiness: r, Warnings: []ExportWarning{}}
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
	return p, charts, nil
}

// CreateExportJobs 执行导出门禁并在同一事务内固化快照、创建每种格式的导出任务。
func (d *DB) CreateExportJobs(ctx context.Context, tenant string, taskID int64, actor string, in CreateExportInput) ([]*ExportJob, *ExportPreflight, error) {
	tenant = tenantOr(tenant)
	seen := map[string]bool{}
	var formats []string
	for _, f := range in.Formats {
		if f != ExportDOCX && f != ExportPDF {
			return nil, nil, fmt.Errorf("%w: unknown format %q", ErrInvalidExport, f)
		}
		if !seen[f] {
			seen[f] = true
			formats = append(formats, f)
		}
	}
	if len(formats) == 0 {
		return nil, nil, fmt.Errorf("%w: at least one format is required", ErrInvalidExport)
	}
	if in.MaxAttempts <= 0 {
		in.MaxAttempts = 3
	}
	var jobs []*ExportJob
	var pre *ExportPreflight
	err := d.inRepeatableRead(ctx, func(tx *sql.Tx) error {
		jobs = nil
		templateKey, templateHash := DefaultTemplateKey, in.DefaultTemplateHash
		var templateID any
		if in.TemplateID > 0 {
			t, err := getTemplateTx(ctx, tx, tenant, in.TemplateID)
			if err != nil {
				return err
			}
			templateKey, templateHash, templateID = "template:"+strconv.FormatInt(t.ID, 10), t.SHA256, t.ID
		}
		approved, _, err := lockApprovedSectionsTx(ctx, tx, tenant, taskID, 0)
		if err != nil && !errors.Is(err, ErrOutlineNotApproved) {
			return err
		}
		var charts []*ProposalChart
		pre, charts, err = exportPreflightTx(ctx, tx, tenant, taskID)
		if err != nil {
			return err
		}
		if !pre.Readiness.Ready {
			return &ExportBlockedError{Preflight: pre}
		}
		var taskName string
		if err := tx.QueryRowContext(ctx, `SELECT name FROM tasks WHERE id = $1`, taskID).Scan(&taskName); err != nil {
			return err
		}
		doc := DocSnapshot{TaskName: taskName, Bidder: in.Bidder, OutlineVersion: approved.Version, Sections: []SnapshotSection{}, CreatedAt: time.Now().UTC()}
		revs := map[string]int{}
		rows, err := tx.QueryContext(ctx, `SELECT s.id, s.section_code, s.title, s.level, s.seq, s.approved_rev_no, r.id
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
		matrixRows, err := d.listMatrix(ctx, tx, tenant, taskID, approved.ID, MatrixFilter{})
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
		gate, _ := json.Marshal(pre)
		revJSON, _ := json.Marshal(revs)
		chartJSON, _ := json.Marshal(chartSnap)
		matrixJSON, _ := json.Marshal(matrixSnap)
		docJSON, _ := json.Marshal(doc)
		warnJSON, _ := json.Marshal(pre.Warnings)
		for _, f := range formats {
			j, err := scanExportJob(tx.QueryRowContext(ctx, `INSERT INTO export_jobs(tenant_id, task_id, outline_id, outline_version, format, template_id,
				template_key, template_hash, rev_snapshot, chart_snapshot, matrix_snapshot, doc_snapshot, gate, warnings, max_attempts, created_by, updated_by)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$16) RETURNING `+exportJobCols,
				tenant, taskID, approved.ID, approved.Version, f, templateID, templateKey, templateHash, string(revJSON), string(chartJSON),
				string(matrixJSON), string(docJSON), string(gate), string(warnJSON), in.MaxAttempts, actor))
			if err != nil {
				return err
			}
			jobs = append(jobs, j)
		}
		return nil
	})
	if err != nil {
		var be *ExportBlockedError
		if errors.As(err, &be) {
			return nil, be.Preflight, err
		}
		return nil, nil, err
	}
	return jobs, pre, nil
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
// 已用尽重试次数的过期任务先被置为 failed。taskID > 0 时只领取该任务的导出（测试与单任务调试用），0 表示全部。
func (d *DB) ClaimExportJob(ctx context.Context, owner string, lease time.Duration, taskID int64) (*ExportJobRef, error) {
	if _, err := d.ExecContext(ctx, `UPDATE export_jobs SET status = 'failed', finished_at = now(), lease_owner = '', lease_until = NULL,
		error = CASE WHEN error = '' THEN format('执行租约过期且已达最大重试次数 %s', max_attempts) ELSE error END
		WHERE status = 'running' AND lease_until < now() AND attempts >= max_attempts`); err != nil {
		return nil, err
	}
	var tenant string
	row := d.QueryRowContext(ctx, `UPDATE export_jobs j SET status = 'running', attempts = j.attempts + 1, lease_token = j.lease_token + 1,
			lease_owner = $1, lease_until = now() + make_interval(secs => $2), started_at = COALESCE(j.started_at, now()),
			warnings = CASE WHEN j.status = 'running' THEN j.warnings || jsonb_build_array(jsonb_build_object('kind', 'retried',
				'message', format('第 %s 次执行的租约（%s）已过期，任务被回收重试', j.attempts, j.lease_owner))) ELSE j.warnings END
		WHERE j.id = (SELECT id FROM export_jobs WHERE (status = 'queued' OR (status = 'running' AND lease_until < now())) AND attempts < max_attempts
			AND ($3::bigint = 0 OR task_id = $3) ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1)
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

// HeartbeatExportJob 延长租约；令牌不符（已被回收给其他执行者）时返回 ErrLeaseLost。
func (d *DB) HeartbeatExportJob(ctx context.Context, id, token int64, lease time.Duration) error {
	res, err := d.ExecContext(ctx, `UPDATE export_jobs SET lease_until = now() + make_interval(secs => $3)
		WHERE id = $1 AND lease_token = $2 AND status = 'running'`, id, token, lease.Seconds())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrLeaseLost
	}
	return nil
}

// ExportResult 是一次成功执行的产物。
type ExportResult struct {
	ArtifactPath string
	ArtifactName string
	ArtifactHash string
	ArtifactSize int64
	TOCRefreshed bool
	Warnings     []ExportWarning
}

// CompleteExportJob 以围栏令牌提交成功结果；令牌不符时返回 ErrLeaseLost（结果不写入）。
func (d *DB) CompleteExportJob(ctx context.Context, id, token int64, r ExportResult) error {
	warns, _ := json.Marshal(r.Warnings)
	if r.Warnings == nil {
		warns = []byte("[]")
	}
	res, err := d.ExecContext(ctx, `UPDATE export_jobs SET status = 'succeeded', artifact_path = $3, artifact_name = $4, artifact_hash = $5,
		artifact_size = $6, toc_refreshed = $7, warnings = warnings || $8::jsonb, error = '', finished_at = now(), lease_until = NULL, lease_owner = ''
		WHERE id = $1 AND lease_token = $2 AND status = 'running'`, id, token, r.ArtifactPath, r.ArtifactName, r.ArtifactHash, r.ArtifactSize,
		r.TOCRefreshed, string(warns))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrLeaseLost
	}
	return nil
}

// FailExportJob 记录失败：retry 且尚有重试次数时回到 queued，否则置为 failed。
func (d *DB) FailExportJob(ctx context.Context, id, token int64, reason string, retry bool) error {
	res, err := d.ExecContext(ctx, `UPDATE export_jobs SET error = $3, lease_until = NULL, lease_owner = '',
		status = CASE WHEN $4 AND attempts < max_attempts THEN 'queued' ELSE 'failed' END,
		finished_at = CASE WHEN $4 AND attempts < max_attempts THEN NULL ELSE now() END
		WHERE id = $1 AND lease_token = $2 AND status = 'running'`, id, token, reason, retry)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrLeaseLost
	}
	return nil
}

// ReclaimExpiredExportJobs 把租约过期的 running 任务放回队列（尚有重试次数）或置为 failed，返回回收的任务 ID。
// 服务启动时调用一次；运行期间 ClaimExportJob 也会直接领取过期任务。taskID > 0 时只回收该任务。
func (d *DB) ReclaimExpiredExportJobs(ctx context.Context, taskID int64) ([]int64, error) {
	rows, err := d.QueryContext(ctx, `UPDATE export_jobs SET
			status = CASE WHEN attempts < max_attempts THEN 'queued' ELSE 'failed' END,
			finished_at = CASE WHEN attempts < max_attempts THEN NULL ELSE now() END,
			error = CASE WHEN attempts < max_attempts THEN error ELSE format('执行租约过期且已达最大重试次数 %s', max_attempts) END,
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
