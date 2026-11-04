package db

// Phase 6B（标书正文域）：章节图表 proposal_charts。
//
// 图表在生成时绑定到章节当前主版本（section_rev_id + rev_no，复合外键）。章节产生新的有效修订后，旧图表不会被改写，
// 而是在查询时判定为过期（Stale = rev_no <> 章节 current_rev_no）；导出快照只收录与锁定修订一致的图表。
// 渲染结果写回时校验 source_hash，避免并发「重新生成」后把旧源码的产物挂到新源码上。
// S3：异步渲染作业见 chart_jobs.go；兼容入口（同步渲染、重新绑定、删除）在图表有活跃作业时一律拒绝（ErrChartJobActive）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// 图表类型与渲染状态。
const (
	ChartArchitecture   = "architecture"
	ChartGantt          = "gantt"
	ChartOrg            = "org"
	ChartEquipmentTable = "equipment_table"

	ChartPending    = "pending"
	ChartRendered   = "rendered"
	ChartUnrendered = "unrendered"
	ChartFailed     = "failed"

	ChartSourceMermaid = "mermaid"
	ChartSourceJSON    = "json"

	ChartFormatPNG    = "png"
	ChartFormatNative = "native"
)

var (
	ErrInvalidChart     = errors.New("chart: invalid chart")
	ErrChartNoRevision  = errors.New("chart: section has no revision to bind the chart to")
	ErrChartSourceMoved = errors.New("chart: source changed while rendering")
	ErrChartJobActive   = errors.New("chart: a render job is queued or running for this chart")
)

// ProposalChart 是一张章节图表。Stale 表示章节当前主版本已不是图表绑定的修订。
type ProposalChart struct {
	ID           int64           `json:"id"`
	TaskID       int64           `json:"task_id"`
	SectionID    int64           `json:"section_id"`
	SectionCode  string          `json:"section_code"`
	SectionTitle string          `json:"section_title"`
	SectionRevID int64           `json:"section_rev_id"`
	RevNo        int             `json:"rev_no"`
	CurrentRevNo int             `json:"current_rev_no"`
	Stale        bool            `json:"stale"`
	StaleReason  string          `json:"stale_reason,omitempty"`
	ChartType    string          `json:"chart_type"`
	Title        string          `json:"title"`
	Spec         json.RawMessage `json:"spec"`
	SourceFormat string          `json:"source_format"`
	Source       string          `json:"source"`
	SourceHash   string          `json:"source_hash"`
	RenderStatus string          `json:"render_status"`
	RenderFormat string          `json:"render_format"`
	RenderError  string          `json:"render_error"`
	ArtifactPath string          `json:"-"`
	ArtifactHash string          `json:"artifact_hash"`
	ArtifactSize int64           `json:"artifact_size"`
	WidthPx      int             `json:"width_px"`
	HeightPx     int             `json:"height_px"`
	RenderedAt   *time.Time      `json:"rendered_at,omitempty"`
	PlanCode     string          `json:"plan_code,omitempty"` // HTTP 章节 Writer 图表计划编号
	LatestJob    *ChartJobBrief  `json:"latest_job,omitempty"`
	CreatedBy    string          `json:"created_by"`
	UpdatedBy    string          `json:"updated_by"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// ChartJobBrief 是图表最近一次渲染作业的摘要（完整记录见 ChartRenderJob）。
type ChartJobBrief struct {
	ID          int64      `json:"id"`
	Status      string     `json:"status"`
	Attempts    int        `json:"attempts"`
	MaxAttempts int        `json:"max_attempts"`
	DeadlineAt  time.Time  `json:"deadline_at"`
	ErrorCode   string     `json:"error_code,omitempty"`
	Error       string     `json:"error,omitempty"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

// Active 报告作业是否仍在排队或执行。
func (b *ChartJobBrief) Active() bool {
	return b != nil && (b.Status == ChartJobQueued || b.Status == ChartJobRunning)
}

// ChartInput 是一张新图表（源码由 export 包按类型从结构化规格生成）。
// ExpectedRevNo > 0 时要求章节当前修订仍是该版本（旧接口可缺省，表示绑定当前修订）。
type ChartInput struct {
	SectionID     int64
	ExpectedRevNo int
	ChartType     string
	Title         string
	Spec          json.RawMessage
	SourceFormat  string
	Source        string
	SourceHash    string
}

// ChartRender 是一次渲染的结果。
type ChartRender struct {
	Status       string
	Format       string
	Error        string
	ArtifactPath string
	ArtifactHash string
	ArtifactSize int64
	WidthPx      int
	HeightPx     int
}

func validChartType(t string) bool {
	switch t {
	case ChartArchitecture, ChartGantt, ChartOrg, ChartEquipmentTable:
		return true
	}
	return false
}

const chartCols = `c.id, c.task_id, c.section_id, s.section_code, s.title, c.section_rev_id, c.rev_no, s.current_rev_no, c.chart_type, c.title,
	c.spec, c.source_format, c.source, c.source_hash, c.render_status, c.render_format, c.render_error, c.artifact_path, c.artifact_hash,
	c.artifact_size, c.width_px, c.height_px, c.rendered_at, c.plan_code, c.created_by, c.updated_by, c.created_at, c.updated_at,
	lj.id, lj.status, lj.attempts, lj.max_attempts, lj.deadline_at, lj.error_code, lj.error, lj.created_by, lj.created_at, lj.finished_at`

const chartFrom = ` FROM proposal_charts c JOIN proposal_sections s ON s.tenant_id = c.tenant_id AND s.id = c.section_id
	LEFT JOIN LATERAL (SELECT j.id, j.status, j.attempts, j.max_attempts, j.deadline_at, j.error_code, j.error, j.created_by, j.created_at, j.finished_at
		FROM chart_render_jobs j WHERE j.tenant_id = c.tenant_id AND j.chart_id = c.id ORDER BY j.id DESC LIMIT 1) lj ON true`

func scanChart(sc rowScanner) (*ProposalChart, error) {
	var c ProposalChart
	var spec []byte
	var rendered sql.NullTime
	var (
		jobID, jobAttempts, jobMax sql.NullInt64
		jobStatus, jobCode, jobErr sql.NullString
		jobBy                      sql.NullString
		jobDeadline, jobCreated    sql.NullTime
		jobFinished                sql.NullTime
	)
	if err := sc.Scan(&c.ID, &c.TaskID, &c.SectionID, &c.SectionCode, &c.SectionTitle, &c.SectionRevID, &c.RevNo, &c.CurrentRevNo,
		&c.ChartType, &c.Title, &spec, &c.SourceFormat, &c.Source, &c.SourceHash, &c.RenderStatus, &c.RenderFormat, &c.RenderError,
		&c.ArtifactPath, &c.ArtifactHash, &c.ArtifactSize, &c.WidthPx, &c.HeightPx, &rendered, &c.PlanCode, &c.CreatedBy, &c.UpdatedBy,
		&c.CreatedAt, &c.UpdatedAt,
		&jobID, &jobStatus, &jobAttempts, &jobMax, &jobDeadline, &jobCode, &jobErr, &jobBy, &jobCreated, &jobFinished); err != nil {
		return nil, err
	}
	c.Spec = json.RawMessage(spec)
	c.RenderedAt = nullTimePtr(rendered)
	if jobID.Valid {
		c.LatestJob = &ChartJobBrief{ID: jobID.Int64, Status: jobStatus.String, Attempts: int(jobAttempts.Int64), MaxAttempts: int(jobMax.Int64),
			DeadlineAt: jobDeadline.Time, ErrorCode: jobCode.String, Error: jobErr.String, CreatedBy: jobBy.String, CreatedAt: jobCreated.Time,
			FinishedAt: nullTimePtr(jobFinished)}
	}
	if c.RevNo != c.CurrentRevNo {
		c.Stale = true
		c.StaleReason = fmt.Sprintf("章节 %s 已更新到 rev %d，图表绑定 rev %d", c.SectionCode, c.CurrentRevNo, c.RevNo)
	}
	return &c, nil
}

// CreateChart 新建图表并绑定到章节当前主版本（章节 FOR SHARE，期间不会产生新修订）。
func (d *DB) CreateChart(ctx context.Context, tenant string, taskID int64, actor string, in ChartInput) (*ProposalChart, error) {
	if err := CheckSectionScope(ctx, in.SectionID); err != nil {
		return nil, err
	}
	tenant = tenantOr(tenant)
	if !validChartType(in.ChartType) {
		return nil, fmt.Errorf("%w: unknown chart_type %q", ErrInvalidChart, in.ChartType)
	}
	if in.Title == "" || in.Source == "" || in.SourceHash == "" {
		return nil, fmt.Errorf("%w: title and source are required", ErrInvalidChart)
	}
	if len(in.Spec) == 0 {
		in.Spec = json.RawMessage(`{}`)
	}
	var out *ProposalChart
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		var cur int
		err := tx.QueryRowContext(ctx, `SELECT current_rev_no FROM proposal_sections WHERE tenant_id = $1 AND task_id = $2 AND id = $3 FOR SHARE`,
			tenant, taskID, in.SectionID).Scan(&cur)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if cur == 0 {
			return ErrChartNoRevision
		}
		if in.ExpectedRevNo > 0 && in.ExpectedRevNo != cur {
			return fmt.Errorf("%w: expected_rev_no %d, section is at rev %d", ErrRevisionConflict, in.ExpectedRevNo, cur)
		}
		var revID int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM section_revisions WHERE tenant_id = $1 AND section_id = $2 AND rev_no = $3`,
			tenant, in.SectionID, cur).Scan(&revID); err != nil {
			return err
		}
		var id int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO proposal_charts(tenant_id, task_id, section_id, section_rev_id, rev_no, chart_type, title, spec,
			source_format, source, source_hash, created_by, updated_by) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$12) RETURNING id`,
			tenant, taskID, in.SectionID, revID, cur, in.ChartType, in.Title, string(in.Spec), in.SourceFormat, in.Source, in.SourceHash, actor).Scan(&id); err != nil {
			return err
		}
		out, err = getChartTx(ctx, tx, tenant, taskID, id)
		return err
	})
	return out, err
}

func getChartTx(ctx context.Context, q queryer, tenant string, taskID, id int64) (*ProposalChart, error) {
	c, err := scanChart(q.QueryRowContext(ctx, `SELECT `+chartCols+chartFrom+` WHERE c.tenant_id = $1 AND c.task_id = $2 AND c.id = $3`, tenant, taskID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

func (d *DB) GetChart(ctx context.Context, tenant string, taskID, id int64) (*ProposalChart, error) {
	return getChartTx(ctx, d, tenantOr(tenant), taskID, id)
}

// ListCharts 列出任务（或单个章节）的图表，按章节顺序与创建顺序排列。
func (d *DB) ListCharts(ctx context.Context, tenant string, taskID, sectionID int64) ([]*ProposalChart, error) {
	q := `SELECT ` + chartCols + chartFrom + ` WHERE c.tenant_id = $1 AND c.task_id = $2`
	args := []any{tenantOr(tenant), taskID}
	if sectionID > 0 {
		q += ` AND c.section_id = $3`
		args = append(args, sectionID)
	}
	rows, err := d.QueryContext(ctx, q+` ORDER BY s.outline_version, s.seq, c.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*ProposalChart{}
	for rows.Next() {
		c, err := scanChart(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetChartRender 写回同步渲染结果（兼容入口）；源码已被并发改动（source_hash 不一致）时返回 ErrChartSourceMoved，
// 图表已有排队 / 执行中的渲染作业时返回 ErrChartJobActive（结果由作业写回，不并发改写）。
func (d *DB) SetChartRender(ctx context.Context, tenant string, taskID, id int64, sourceHash string, r ChartRender, actor string) (*ProposalChart, error) {
	tenant = tenantOr(tenant)
	var renderedAt any
	if r.Status == ChartRendered {
		renderedAt = time.Now().UTC()
	}
	var out *ProposalChart
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		// 先锁图表行，再用新语句检查活跃作业：等锁期间提交的作业对检查可见。
		var hash string
		err := tx.QueryRowContext(ctx, `SELECT source_hash FROM proposal_charts WHERE tenant_id = $1 AND task_id = $2 AND id = $3 FOR UPDATE`,
			tenant, taskID, id).Scan(&hash)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if active, err := chartHasActiveJobTx(ctx, tx, tenant, id); err != nil || active {
			if err == nil {
				err = ErrChartJobActive
			}
			return err
		}
		if hash != sourceHash {
			return ErrChartSourceMoved
		}
		if _, err := tx.ExecContext(ctx, `UPDATE proposal_charts SET render_status = $4, render_format = $5, render_error = $6, artifact_path = $7,
			artifact_hash = $8, artifact_size = $9, width_px = $10, height_px = $11, rendered_at = $12, updated_by = $13
			WHERE tenant_id = $1 AND task_id = $2 AND id = $3`,
			tenant, taskID, id, r.Status, r.Format, r.Error, r.ArtifactPath, r.ArtifactHash, r.ArtifactSize, r.WidthPx, r.HeightPx, renderedAt, actor); err != nil {
			return err
		}
		out, err = getChartTx(ctx, tx, tenant, taskID, id)
		return err
	})
	return out, err
}

// chartHasActiveJobTx 报告图表是否有排队 / 执行中的渲染作业（调用方已锁定图表行）。
func chartHasActiveJobTx(ctx context.Context, q queryer, tenant string, chartID int64) (bool, error) {
	var active bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM chart_render_jobs WHERE tenant_id = $1 AND chart_id = $2 AND status IN ('queued','running'))`,
		tenant, chartID).Scan(&active)
	return active, err
}

// RebindChart 把图表重新绑定到章节当前主版本（人工确认图表仍适用于新正文后使用）；有活跃渲染作业时拒绝。
// 锁序：章节 → 图表，与渲染作业提交 / 完成一致。
func (d *DB) RebindChart(ctx context.Context, tenant string, taskID, id int64, actor string) (*ProposalChart, error) {
	tenant = tenantOr(tenant)
	var out *ProposalChart
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		var sectionID int64
		err := tx.QueryRowContext(ctx, `SELECT section_id FROM proposal_charts WHERE tenant_id = $1 AND task_id = $2 AND id = $3`, tenant, taskID, id).Scan(&sectionID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var cur int
		if err := tx.QueryRowContext(ctx, `SELECT current_rev_no FROM proposal_sections WHERE tenant_id = $1 AND id = $2 FOR SHARE`, tenant, sectionID).Scan(&cur); err != nil {
			return err
		}
		if cur == 0 {
			return ErrChartNoRevision
		}
		if _, err := tx.ExecContext(ctx, `SELECT id FROM proposal_charts WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenant, id); err != nil {
			return err
		}
		if active, err := chartHasActiveJobTx(ctx, tx, tenant, id); err != nil || active {
			if err == nil {
				err = ErrChartJobActive
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE proposal_charts c SET section_rev_id = r.id, rev_no = r.rev_no, updated_by = $4
			FROM section_revisions r WHERE c.tenant_id = $1 AND c.id = $2 AND r.tenant_id = c.tenant_id AND r.section_id = c.section_id AND r.rev_no = $3`,
			tenant, id, cur, actor); err != nil {
			return err
		}
		out, err = getChartTx(ctx, tx, tenant, taskID, id)
		return err
	})
	return out, err
}

// DeleteChart 删除图表；有排队 / 执行中的渲染作业时拒绝（先取消），已终态的作业随图表级联删除。
func (d *DB) DeleteChart(ctx context.Context, tenant string, taskID, id int64) error {
	tenant = tenantOr(tenant)
	return d.InTx(ctx, func(tx *sql.Tx) error {
		var locked int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM proposal_charts WHERE tenant_id = $1 AND task_id = $2 AND id = $3 FOR UPDATE`, tenant, taskID, id).Scan(&locked)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if active, err := chartHasActiveJobTx(ctx, tx, tenant, id); err != nil || active {
			if err == nil {
				err = ErrChartJobActive
			}
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM proposal_charts WHERE tenant_id = $1 AND task_id = $2 AND id = $3`, tenant, taskID, id)
		return err
	})
}
