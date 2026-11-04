package db

// S3（AGENT_TOOL_SKILLS_PLAN 3.2 / 4.3 / 5.1~5.2）：持久化图表渲染作业 chart_render_jobs（域 C）。
//
// 作业只引用图表（复合外键），修订从绑定图表读取。请求幂等范围为 (tenant, task, request_key)：相同 request_hash
// 返回原作业（终态同样返回），不同则 ErrIdempotencyKeyConflict；只有新键才解析当前修订、固定执行版本并检查提交门禁。
// 截止时间与尝试预算在首次提交时固定，回收与暂停恢复都不延长；lease_token 是围栏令牌，取消 / 暂停 / 终态都推进令牌，
// 旧执行者的成功、失败与心跳都无法写回。
//
// 锁序：
//   - 提交：任务行 → Intent 活租约 → 大纲 → 章节 → 图表 → 作业 → 订阅/事件；
//   - 领取 / 取消：任务行（共享）→ 图表 → 作业，不申请大纲 / 章节锁；
//   - 完成：任务行（共享）→ 大纲 → 章节 → 图表 → 作业 → 订阅/事件（终态触发器 agent_job_terminal）；
//   - HTTP Writer（FinishSectionWriteJob）：任务 → section_write_jobs → 大纲 → 章节，同事务只插入本次新图表 / 作业。
//
// 数据库事务内不渲染、不发网络请求；渲染在 export.ChartRunner 中执行。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// JobKindChartRender 是图表渲染作业的 job_ref.kind（映射 chart_render_jobs）。
const JobKindChartRender = "chart_render"

// 图表渲染作业状态（六态）。
const (
	ChartJobQueued     = "queued"
	ChartJobRunning    = "running"
	ChartJobSucceeded  = "succeeded"
	ChartJobFailed     = "failed"
	ChartJobCancelled  = "cancelled"
	ChartJobSuperseded = "superseded"
)

// 后台作业错误码（AGENT_TOOL_SKILLS_PLAN 4.2）。
const (
	JobErrRuntimeUnavailable = "RUNTIME_UNAVAILABLE"
	JobErrBudgetExhausted    = "BUDGET_EXHAUSTED"
	JobErrStaleRevision      = "CONFLICT_STALE_REVISION"
	JobErrRenderFailed       = "RENDER_FAILED"
	JobErrCancelled          = "CANCELLED"
)

// ReservedRequestKeyPrefix 是服务端保留的请求键命名空间（HTTP 章节 Writer 的图表计划），客户端请求键不得使用。
const ReservedRequestKeyPrefix = "sys:"

const (
	DefaultChartJobDeadline    = 10 * time.Minute
	DefaultChartJobMaxAttempts = 3
	// MaxChartPlans 是一次章节撰写最多登记的图表计划数。
	MaxChartPlans             = 20
	maxActiveChartJobsPerTask = 50
)

var (
	ErrIdempotencyKeyConflict = errors.New("db: request_key was already used with different input")
	ErrJobPrecondition        = errors.New("db: job precondition failed")
	ErrJobCapacity            = errors.New("db: too many active jobs, retry later")
)

// ChartRenderJob 是一次图表渲染请求。图表与修订从 ChartID 读取，不在作业中冗余保存。
type ChartRenderJob struct {
	ID          int64      `json:"id"`
	TaskID      int64      `json:"task_id"`
	TenantID    string     `json:"-"`
	ChartID     int64      `json:"chart_id"`
	RequestKey  string     `json:"request_key"`
	RequestHash string     `json:"-"`
	SourceHash  string     `json:"source_hash"`
	Executor    string     `json:"executor"`
	Status      string     `json:"status"`
	Attempts    int        `json:"attempts"`
	MaxAttempts int        `json:"max_attempts"`
	DeadlineAt  time.Time  `json:"deadline_at"`
	LeaseOwner  string     `json:"-"`
	LeaseToken  int64      `json:"-"`
	LeaseUntil  *time.Time `json:"lease_until,omitempty"`
	ErrorCode   string     `json:"error_code,omitempty"`
	Error       string     `json:"error,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// Active 报告作业是否仍在排队或执行。
func (j *ChartRenderJob) Active() bool {
	return j != nil && (j.Status == ChartJobQueued || j.Status == ChartJobRunning)
}

const chartJobCols = `id, tenant_id, task_id, chart_id, request_key, request_hash, source_hash, executor, status, attempts, max_attempts,
	deadline_at, lease_owner, lease_token, lease_until, error_code, error, started_at, finished_at, created_by, created_at, updated_at`

func scanChartJob(r rowScanner) (*ChartRenderJob, error) {
	j := new(ChartRenderJob)
	var leaseUntil, started, finished sql.NullTime
	err := r.Scan(&j.ID, &j.TenantID, &j.TaskID, &j.ChartID, &j.RequestKey, &j.RequestHash, &j.SourceHash, &j.Executor, &j.Status,
		&j.Attempts, &j.MaxAttempts, &j.DeadlineAt, &j.LeaseOwner, &j.LeaseToken, &leaseUntil, &j.ErrorCode, &j.Error, &started, &finished,
		&j.CreatedBy, &j.CreatedAt, &j.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	j.LeaseUntil, j.StartedAt, j.FinishedAt = nullTimePtr(leaseUntil), nullTimePtr(started), nullTimePtr(finished)
	return j, nil
}

// ChartRenderSpec 是一张待渲染图表的纯数据输入：源码由 export 包按结构化规格确定性生成，db 不反向导入 export。
type ChartRenderSpec struct {
	ChartType    string          `json:"chart_type"`
	Title        string          `json:"title"`
	Spec         json.RawMessage `json:"spec"`
	SourceFormat string          `json:"source_format"`
	Source       string          `json:"-"`
	SourceHash   string          `json:"source_hash"`
	// Executor 是首次提交时固定的执行版本（如 "mmdc 11.17.0"、"native"）；为空表示提交时没有可用渲染器。
	Executor string `json:"executor"`
}

func (s *ChartRenderSpec) validate() error {
	s.Title = strings.TrimSpace(s.Title)
	switch {
	case !validChartType(s.ChartType):
		return fmt.Errorf("%w: unknown chart_type %q", ErrInvalidChart, s.ChartType)
	case s.Title == "" || s.Source == "" || s.SourceHash == "":
		return fmt.Errorf("%w: title and source are required", ErrInvalidChart)
	case (s.ChartType == ChartEquipmentTable) != (s.SourceFormat == ChartSourceJSON):
		return fmt.Errorf("%w: source_format %q does not match chart_type %q", ErrInvalidChart, s.SourceFormat, s.ChartType)
	}
	if len(s.Spec) == 0 {
		s.Spec = json.RawMessage(`{}`)
	}
	return nil
}

// ChartPlan 是 HTTP 章节 Writer 随草稿给出的一张图表计划；同一修订内 code 唯一，最多 MaxChartPlans 项。
type ChartPlan struct {
	Code        string `json:"code"`
	RequestHash string `json:"request_hash"`
	ChartRenderSpec
}

var reChartPlanCode = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,39}$`)

// ValidateChartPlans 在写入任何修订之前校验全部图表计划（数量、编号唯一与字符集、规格字段）。
func ValidateChartPlans(plans []ChartPlan) error {
	if len(plans) > MaxChartPlans {
		return fmt.Errorf("%w: at most %d chart plans per revision", ErrInvalidChart, MaxChartPlans)
	}
	seen := map[string]bool{}
	for i := range plans {
		p := &plans[i]
		if !reChartPlanCode.MatchString(p.Code) {
			return fmt.Errorf("%w: chart plan code %q must be 1-40 letters, digits, '_', '.' or '-'", ErrInvalidChart, p.Code)
		}
		if seen[p.Code] {
			return fmt.Errorf("%w: duplicate chart plan code %q", ErrInvalidChart, p.Code)
		}
		seen[p.Code] = true
		if err := p.validate(); err != nil {
			return fmt.Errorf("chart plan %s: %w", p.Code, err)
		}
	}
	return nil
}

// ValidClientRequestKey 校验客户端请求键：1~200 字符，不得使用服务端保留前缀。
func ValidClientRequestKey(key string) error {
	switch {
	case key == "" || len(key) > 200:
		return fmt.Errorf("%w: request_key must be 1-200 characters", ErrInvalidChart)
	case strings.HasPrefix(key, ReservedRequestKeyPrefix):
		return fmt.Errorf("%w: request_key prefix %q is reserved for the server", ErrInvalidChart, ReservedRequestKeyPrefix)
	}
	return nil
}

// SubmitChartRenderInput 是一次渲染提交：新图表（SectionID + ExpectedRevNo + Chart）或重试已有图表（ChartID）。
type SubmitChartRenderInput struct {
	SectionID     int64
	ExpectedRevNo int // 新图表必填（>0）；重试时可选，>0 时须等于图表绑定的修订
	ChartID       int64
	Chart         ChartRenderSpec // 新图表的规格；重试时只使用 Executor
	RequestKey    string
	RequestHash   string // 规范化客户端输入的哈希（服务层计算）
	Deadline      time.Duration
	MaxAttempts   int
	Actor         string
}

// ChartRenderSubmission 是提交结果；Replayed 表示同键同参的重放，返回的是原作业（可能已终态）。
type ChartRenderSubmission struct {
	Job          *ChartRenderJob       `json:"job"`
	Chart        *ProposalChart        `json:"chart"`
	Subscription *AgentJobSubscription `json:"subscription,omitempty"`
	Replayed     bool                  `json:"replayed"`
}

// SubmitChartRender 在一个事务内登记图表（或锁定待重试的图表）与渲染作业；宿主绑定 Intent 的执行同事务订阅该作业
// （Tool 提交强制订阅当前 Intent，HTTP 用户提交没有订阅）。
func (d *DB) SubmitChartRender(ctx context.Context, tenant string, taskID int64, in SubmitChartRenderInput) (*ChartRenderSubmission, error) {
	tenant = tenantOr(tenant)
	in.RequestKey = strings.TrimSpace(in.RequestKey)
	if err := ValidClientRequestKey(in.RequestKey); err != nil {
		return nil, err
	}
	switch {
	case in.RequestHash == "":
		return nil, fmt.Errorf("%w: request_hash is required", ErrInvalidChart)
	case (in.SectionID > 0) == (in.ChartID > 0):
		return nil, fmt.Errorf("%w: exactly one of section_id and chart_id is required", ErrInvalidChart)
	case in.ExpectedRevNo < 0 || (in.SectionID > 0 && in.ExpectedRevNo == 0):
		return nil, fmt.Errorf("%w: expected_rev_no > 0 is required", ErrInvalidChart)
	}
	if in.SectionID > 0 {
		if err := in.Chart.validate(); err != nil {
			return nil, err
		}
		if err := CheckSectionScope(ctx, in.SectionID); err != nil {
			return nil, err
		}
	}
	if err := CheckJobKindScope(ctx, JobKindChartRender); err != nil {
		return nil, err
	}
	if in.Deadline <= 0 {
		in.Deadline = DefaultChartJobDeadline
	}
	if in.MaxAttempts <= 0 {
		in.MaxAttempts = DefaultChartJobMaxAttempts
	}
	in.MaxAttempts = min(in.MaxAttempts, 10)
	_, bound := intentExecutionFrom(ctx)
	out := &ChartRenderSubmission{}
	err := d.Transaction(ctx, func(tx *DB) error {
		// 任务行 FOR UPDATE：同键并发提交与暂停串行化。
		paused, err := lockSectionJobTask(ctx, tx, taskID)
		if err != nil {
			return err
		}
		old, err := scanChartJob(tx.QueryRowContext(ctx, `SELECT `+chartJobCols+` FROM chart_render_jobs WHERE tenant_id = $1 AND task_id = $2 AND request_key = $3`,
			tenant, taskID, in.RequestKey))
		switch {
		case err == nil:
			if old.RequestHash != in.RequestHash {
				return fmt.Errorf("%w: %s", ErrIdempotencyKeyConflict, in.RequestKey)
			}
			c, err := getChartTx(ctx, tx, tenant, taskID, old.ChartID)
			if err != nil {
				return err
			}
			if err := CheckSectionScope(ctx, c.SectionID); err != nil {
				return err
			}
			out.Job, out.Chart, out.Replayed = old, c, true
		case errors.Is(err, ErrNotFound):
			if paused {
				return fmt.Errorf("%w: task is paused", ErrJobPrecondition)
			}
			if bound {
				if _, _, _, err := tx.LockSubmittingIntent(ctx, taskID); err != nil {
					return err
				}
			}
			var active int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM chart_render_jobs WHERE tenant_id = $1 AND task_id = $2 AND status IN ('queued','running')`,
				tenant, taskID).Scan(&active); err != nil {
				return err
			}
			if active >= maxActiveChartJobsPerTask {
				return fmt.Errorf("%w: at most %d queued or running chart jobs per task", ErrJobCapacity, maxActiveChartJobsPerTask)
			}
			var chartID int64
			if in.ChartID > 0 {
				chartID, err = tx.lockChartForRetryTx(ctx, tenant, taskID, in)
			} else {
				chartID, err = tx.insertChartForJobTx(ctx, tenant, taskID, in)
			}
			if err != nil {
				return err
			}
			var hash string
			if err := tx.QueryRowContext(ctx, `SELECT source_hash FROM proposal_charts WHERE id = $1`, chartID).Scan(&hash); err != nil {
				return err
			}
			if out.Job, err = insertChartJobTx(ctx, tx, tenant, taskID, chartID, hash, in.RequestKey, in.RequestHash, in.Chart.Executor,
				in.Deadline, in.MaxAttempts, in.Actor); err != nil {
				return err
			}
			if out.Chart, err = getChartTx(ctx, tx, tenant, taskID, chartID); err != nil {
				return err
			}
		default:
			return err
		}
		if !bound {
			return nil
		}
		rev := int64(out.Chart.RevNo)
		out.Subscription, err = tx.SubscribeAgentJob(ctx, tenant, taskID, SubscribeAgentJobInput{Job: JobRef{Kind: JobKindChartRender, ID: out.Job.ID},
			TargetRef:       map[string]any{"chart_id": out.Chart.ID, "section_id": out.Chart.SectionID, "section_code": out.Chart.SectionCode, "rev_no": out.Chart.RevNo},
			ExpectedVersion: &rev, Actor: in.Actor})
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// insertChartForJobTx 校验章节属于批准大纲且当前修订等于 expected_rev_no，再登记绑定该修订的待渲染图表。
func (d *DB) insertChartForJobTx(ctx context.Context, tenant string, taskID int64, in SubmitChartRenderInput) (int64, error) {
	s, outlineStatus, approved, err := sectionGateTx(ctx, d.tx, tenant, taskID, in.SectionID)
	if err != nil {
		return 0, err
	}
	if err := requireApprovedSection(s, outlineStatus, approved); err != nil {
		return 0, err
	}
	if s.CurrentRevNo == 0 {
		return 0, ErrChartNoRevision
	}
	if s.CurrentRevNo != in.ExpectedRevNo {
		return 0, fmt.Errorf("%w: expected_rev_no %d, section %s is at rev %d", ErrRevisionConflict, in.ExpectedRevNo, s.Code, s.CurrentRevNo)
	}
	return insertChartRowTx(ctx, d, tenant, taskID, s.ID, s.CurrentRevNo, "", in.Chart, in.Actor)
}

// lockChartForRetryTx 锁定待重试的图表：须绑定章节当前修订、属于批准大纲且没有活跃作业。锁序：大纲 → 章节 → 图表。
func (d *DB) lockChartForRetryTx(ctx context.Context, tenant string, taskID int64, in SubmitChartRenderInput) (int64, error) {
	var sectionID int64
	err := d.QueryRowContext(ctx, `SELECT section_id FROM proposal_charts WHERE tenant_id = $1 AND task_id = $2 AND id = $3`, tenant, taskID, in.ChartID).Scan(&sectionID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: chart %d", ErrNotFound, in.ChartID)
	}
	if err != nil {
		return 0, err
	}
	s, outlineStatus, approved, err := sectionGateTx(ctx, d.tx, tenant, taskID, sectionID)
	if err != nil {
		return 0, err
	}
	if err := requireApprovedSection(s, outlineStatus, approved); err != nil {
		return 0, err
	}
	var revNo int
	if err := d.QueryRowContext(ctx, `SELECT rev_no FROM proposal_charts WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenant, in.ChartID).Scan(&revNo); err != nil {
		return 0, err
	}
	if revNo != s.CurrentRevNo {
		return 0, fmt.Errorf("%w: chart %d is bound to rev %d, section %s is at rev %d; rebind or generate a new chart", ErrRevisionConflict,
			in.ChartID, revNo, s.Code, s.CurrentRevNo)
	}
	if in.ExpectedRevNo > 0 && in.ExpectedRevNo != revNo {
		return 0, fmt.Errorf("%w: expected_rev_no %d, chart is bound to rev %d", ErrRevisionConflict, in.ExpectedRevNo, revNo)
	}
	if active, err := chartHasActiveJobTx(ctx, d, tenant, in.ChartID); err != nil || active {
		if err == nil {
			err = ErrChartJobActive
		}
		return 0, err
	}
	return in.ChartID, nil
}

// insertChartRowTx 登记一张绑定 (section, rev_no) 的待渲染图表。
func insertChartRowTx(ctx context.Context, q queryer, tenant string, taskID, sectionID int64, revNo int, planCode string, c ChartRenderSpec, actor string) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `INSERT INTO proposal_charts(tenant_id, task_id, section_id, section_rev_id, rev_no, chart_type, title, spec,
			source_format, source, source_hash, plan_code, created_by, updated_by)
		SELECT $1, $2, $3, r.id, r.rev_no, $5, $6, $7::jsonb, $8, $9, $10, $11, $12, $12
		  FROM section_revisions r WHERE r.tenant_id = $1 AND r.section_id = $3 AND r.rev_no = $4 RETURNING id`,
		tenant, taskID, sectionID, revNo, c.ChartType, c.Title, string(c.Spec), c.SourceFormat, c.Source, c.SourceHash, planCode, actor).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrChartNoRevision
	}
	return id, err
}

func insertChartJobTx(ctx context.Context, q queryer, tenant string, taskID, chartID int64, sourceHash, key, reqHash, executor string,
	deadline time.Duration, maxAttempts int, actor string) (*ChartRenderJob, error) {
	return scanChartJob(q.QueryRowContext(ctx, `INSERT INTO chart_render_jobs(tenant_id, task_id, chart_id, request_key, request_hash, source_hash,
			executor, max_attempts, deadline_at, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now() + make_interval(secs => $9), $10, $10) RETURNING `+chartJobCols,
		tenant, taskID, chartID, key, reqHash, sourceHash, executor, maxAttempts, deadline.Seconds(), actor))
}

// insertChartPlansTx 为刚保存为当前主版本的修订登记图表计划及其渲染作业（HTTP 章节 Writer，与修订同事务）。
// 请求键位于服务端保留命名空间，由章节作业 ID、修订 ID 与计划编号确定性生成。
func insertChartPlansTx(ctx context.Context, q queryer, tenant string, j *SectionWriteJob, rev *SectionRevision, plans []ChartPlan, deadline time.Duration, actor string) error {
	if deadline <= 0 {
		deadline = DefaultChartJobDeadline
	}
	for _, p := range plans {
		chartID, err := insertChartRowTx(ctx, q, tenant, j.TaskID, j.SectionID, rev.RevNo, p.Code, p.ChartRenderSpec, actor)
		if err != nil {
			return fmt.Errorf("chart plan %s: %w", p.Code, err)
		}
		key := fmt.Sprintf("%ssection-job:%d:rev:%d:chart:%s", ReservedRequestKeyPrefix, j.ID, rev.ID, p.Code)
		hash := p.RequestHash
		if hash == "" {
			sum := sha256.Sum256([]byte(p.Code + "\n" + p.ChartType + "\n" + p.Title + "\n" + p.SourceHash))
			hash = hex.EncodeToString(sum[:])
		}
		if _, err := insertChartJobTx(ctx, q, tenant, j.TaskID, chartID, p.SourceHash, key, hash, p.Executor, deadline, DefaultChartJobMaxAttempts, actor); err != nil {
			return fmt.Errorf("chart plan %s: %w", p.Code, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 执行：领取、心跳、写回
// ---------------------------------------------------------------------------

// ChartRenderClaim 是领取到的一次执行：作业（含本次围栏令牌）与领取时的图表快照。
type ChartRenderClaim struct {
	Tenant string
	Job    *ChartRenderJob
	Chart  *ProposalChart
}

// ClaimChartRenderJob 领取一个 queued 或租约已过期的 running 作业（任务未暂停）。领取前在同一短事务内结束已耗尽截止时间 /
// 尝试次数（failed + BUDGET_EXHAUSTED）或图表已不绑定章节当前修订（superseded）的作业，这些作业经 finalized 返回；
// 两者都为 nil 表示没有可处理的作业。只锁任务（共享）→ 图表 → 作业，不申请大纲 / 章节锁：图表过期是单调的
// （有活跃作业时禁止重新绑定，章节修订号只增不减），无需章节锁即可判定。
func (d *DB) ClaimChartRenderJob(ctx context.Context, owner string, lease time.Duration, taskID int64) (claim *ChartRenderClaim, finalized *ChartRenderJob, err error) {
	var id, tid, chartID int64
	var tenant string
	err = d.QueryRowContext(ctx, `SELECT j.id, j.task_id, j.tenant_id, j.chart_id FROM chart_render_jobs j JOIN tasks t ON t.id = j.task_id
		WHERE NOT t.paused AND ($1::bigint = 0 OR j.task_id = $1) AND (j.status = 'queued' OR (j.status = 'running' AND j.lease_until < now()))
		ORDER BY j.id LIMIT 1`, taskID).Scan(&id, &tid, &tenant, &chartID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	err = d.Transaction(ctx, func(tx *DB) error {
		var paused bool
		if err := tx.QueryRowContext(ctx, `SELECT paused FROM tasks WHERE id = $1 FOR SHARE`, tid).Scan(&paused); err != nil || paused {
			return err
		}
		if _, err := tx.ExecContext(ctx, `SELECT id FROM proposal_charts WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenant, chartID); err != nil {
			return err
		}
		c, err := getChartTx(ctx, tx, tenant, tid, chartID)
		if errors.Is(err, ErrNotFound) {
			return nil // 图表已删除，作业随之级联删除
		}
		if err != nil {
			return err
		}
		var exhausted bool
		err = tx.QueryRowContext(ctx, `SELECT attempts >= max_attempts OR deadline_at <= now() FROM chart_render_jobs
			WHERE id = $1 AND (status = 'queued' OR (status = 'running' AND lease_until < now())) FOR UPDATE`, id).Scan(&exhausted)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // 已被其他执行者领取或已结束
		}
		if err != nil {
			return err
		}
		j, err := scanChartJob(tx.QueryRowContext(ctx, `SELECT `+chartJobCols+` FROM chart_render_jobs WHERE id = $1`, id))
		if err != nil {
			return err
		}
		switch {
		case exhausted:
			msg := fmt.Sprintf("执行次数或截止时间已耗尽（已执行 %d/%d 次，截止 %s）", j.Attempts, j.MaxAttempts, j.DeadlineAt.UTC().Format(time.RFC3339))
			if err := failChartRenderTx(ctx, tx, tenant, chartID, ChartFailed, msg, owner); err != nil {
				return err
			}
			finalized, err = finishChartJobTx(ctx, tx, id, ChartJobFailed, JobErrBudgetExhausted, msg, owner)
			return err
		case c.Stale || c.SourceHash != j.SourceHash:
			finalized, err = finishChartJobTx(ctx, tx, id, ChartJobSuperseded, JobErrStaleRevision,
				fmt.Sprintf("章节 %s 已更新到 rev %d，图表绑定 rev %d，不再渲染", c.SectionCode, c.CurrentRevNo, c.RevNo), owner)
			return err
		}
		j, err = scanChartJob(tx.QueryRowContext(ctx, `UPDATE chart_render_jobs SET status = 'running', attempts = attempts + 1,
				lease_token = lease_token + 1, lease_until = LEAST(deadline_at, now() + make_interval(secs => $2)), lease_owner = $3,
				started_at = COALESCE(started_at, now()), updated_by = $3
			WHERE id = $1 RETURNING `+chartJobCols, id, lease.Seconds(), owner))
		if err != nil {
			return err
		}
		claim = &ChartRenderClaim{Tenant: tenant, Job: j, Chart: c}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return claim, finalized, nil
}

// HeartbeatChartRenderJob 续租；令牌不符、已非 running、截止时间已过或任务已暂停时返回 ErrLeaseLost（执行者应立即停止）。
func (d *DB) HeartbeatChartRenderJob(ctx context.Context, id, token int64, lease time.Duration) error {
	res, err := d.ExecContext(ctx, `UPDATE chart_render_jobs j SET lease_until = LEAST(j.deadline_at, now() + make_interval(secs => $3))
		FROM tasks t WHERE j.id = $1 AND j.lease_token = $2 AND j.status = 'running' AND j.lease_until > now() AND j.deadline_at > now()
		  AND t.id = j.task_id AND NOT t.paused`, id, token, lease.Seconds())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrLeaseLost
	}
	return nil
}

// ChartRenderResult 是一次执行的结果。Retry 为真且尝试预算未耗尽时作业回到 queued（非终态），否则按 Render 结束。
type ChartRenderResult struct {
	Render    ChartRender
	ErrorCode string
	Retry     bool
}

// FinishChartRenderJob 以围栏令牌写回一次执行结果并返回作业新状态。锁序：任务（共享）→ 大纲 → 章节 → 图表 → 作业。
// 租约已丢失、作业已取消、任务已暂停或截止时间已过时返回 ErrLeaseLost（调用方清理本次产物）；图表已不绑定章节当前修订或
// 源码已变时作业以 superseded 结束且不改写图表（不把结果挂到新修订）。失败不覆盖图表已有的有效渲染结果。
func (d *DB) FinishChartRenderJob(ctx context.Context, cl *ChartRenderClaim, res ChartRenderResult) (*ChartRenderJob, error) {
	tenant, j := tenantOr(cl.Tenant), cl.Job
	actor := j.LeaseOwner
	var out *ChartRenderJob
	err := d.Transaction(ctx, func(tx *DB) error {
		var paused bool
		if err := tx.QueryRowContext(ctx, `SELECT paused FROM tasks WHERE id = $1 FOR SHARE`, j.TaskID).Scan(&paused); err != nil {
			return err
		}
		if paused {
			return ErrLeaseLost
		}
		if _, err := approvedOutlineTx(ctx, tx.tx, tenant, j.TaskID); err != nil {
			return err
		}
		var cur int
		if err := tx.QueryRowContext(ctx, `SELECT current_rev_no FROM proposal_sections WHERE tenant_id = $1 AND id = $2 FOR SHARE`,
			tenant, cl.Chart.SectionID).Scan(&cur); err != nil {
			return err
		}
		var revNo int
		var hash string
		err := tx.QueryRowContext(ctx, `SELECT rev_no, source_hash FROM proposal_charts WHERE tenant_id = $1 AND task_id = $2 AND id = $3 FOR UPDATE`,
			tenant, j.TaskID, j.ChartID).Scan(&revNo, &hash)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLeaseLost
		}
		if err != nil {
			return err
		}
		var canRetry bool
		err = tx.QueryRowContext(ctx, `SELECT attempts < max_attempts FROM chart_render_jobs WHERE id = $1 AND lease_token = $2 AND status = 'running'
			AND lease_until > now() AND deadline_at > now() FOR UPDATE`, j.ID, j.LeaseToken).Scan(&canRetry)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLeaseLost
		}
		if err != nil {
			return err
		}
		r := res.Render
		switch {
		case revNo != cur || hash != j.SourceHash:
			out, err = finishChartJobTx(ctx, tx, j.ID, ChartJobSuperseded, JobErrStaleRevision,
				fmt.Sprintf("章节已更新到 rev %d，图表绑定 rev %d，渲染结果不挂到新修订", cur, revNo), actor)
		case r.Status == ChartRendered:
			if _, err = tx.ExecContext(ctx, `UPDATE proposal_charts SET render_status = 'rendered', render_format = $3, render_error = '',
				artifact_path = $4, artifact_hash = $5, artifact_size = $6, width_px = $7, height_px = $8, rendered_at = now(), updated_by = $9
				WHERE tenant_id = $1 AND id = $2`, tenant, j.ChartID, r.Format, r.ArtifactPath, r.ArtifactHash, r.ArtifactSize, r.WidthPx, r.HeightPx, actor); err != nil {
				return err
			}
			out, err = finishChartJobTx(ctx, tx, j.ID, ChartJobSucceeded, "", "", actor)
		case res.Retry && canRetry:
			out, err = scanChartJob(tx.QueryRowContext(ctx, `UPDATE chart_render_jobs SET status = 'queued', lease_until = NULL, lease_owner = '',
				error_code = $2, error = $3, updated_by = $4 WHERE id = $1 RETURNING `+chartJobCols, j.ID, res.ErrorCode, truncateRunes(r.Error, 500), actor))
		default:
			status := r.Status
			if status != ChartUnrendered {
				status = ChartFailed
			}
			if err = failChartRenderTx(ctx, tx, tenant, j.ChartID, status, r.Error, actor); err != nil {
				return err
			}
			out, err = finishChartJobTx(ctx, tx, j.ID, ChartJobFailed, res.ErrorCode, r.Error, actor)
		}
		return err
	})
	return out, err
}

// finishChartJobTx 把作业写为终态并推进令牌（终态触发器在同一事务内发布订阅事件）。
func finishChartJobTx(ctx context.Context, q queryer, id int64, status, code, msg, actor string) (*ChartRenderJob, error) {
	return scanChartJob(q.QueryRowContext(ctx, `UPDATE chart_render_jobs SET status = $2, error_code = $3, error = $4, lease_until = NULL,
		lease_token = lease_token + 1, finished_at = now(), updated_by = $5 WHERE id = $1 RETURNING `+chartJobCols,
		id, status, code, truncateRunes(msg, 500), actor))
}

// failChartRenderTx 记录渲染失败 / 未渲染；图表已有有效渲染结果（重新渲染失败）时保留原结果。
func failChartRenderTx(ctx context.Context, q queryer, tenant string, chartID int64, status, msg, actor string) error {
	_, err := q.ExecContext(ctx, `UPDATE proposal_charts SET render_status = $3, render_format = '', render_error = $4, updated_by = $5
		WHERE tenant_id = $1 AND id = $2 AND render_status <> 'rendered'`, tenant, chartID, status, truncateRunes(msg, 500), actor)
	return err
}

// ---------------------------------------------------------------------------
// 取消与查询
// ---------------------------------------------------------------------------

// FindChartRenderJob 按请求键查找作业（未找到返回 ErrNotFound）。
func (d *DB) FindChartRenderJob(ctx context.Context, tenant string, taskID int64, key string) (*ChartRenderJob, error) {
	return scanChartJob(d.QueryRowContext(ctx, `SELECT `+chartJobCols+` FROM chart_render_jobs WHERE tenant_id = $1 AND task_id = $2 AND request_key = $3`,
		tenantOr(tenant), taskID, strings.TrimSpace(key)))
}

// checkJobOwnedByIntent：宿主范围限定对象时，只能访问本 Intent 已订阅的作业。
func (d *DB) checkJobOwnedByIntent(ctx context.Context, tenant string, taskID int64, ref JobRef) error {
	p, ok := intentExecutionFrom(ctx)
	if !ok || !restrictsObjects(ctx) {
		return nil
	}
	var owned bool
	if err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agent_job_subscriptions WHERE tenant_id = $1 AND task_id = $2 AND intent_id = $3 AND job_kind = $4 AND job_id = $5)`,
		tenantOr(tenant), taskID, p.ID, ref.Kind, ref.ID).Scan(&owned); err != nil {
		return err
	}
	if !owned {
		return fmt.Errorf("execution scope: %s #%d is not subscribed by the current intent", ref.Kind, ref.ID)
	}
	return nil
}

// GetChartRenderJob 读取任务内一个渲染作业及其图表。宿主绑定 Intent 的执行受作业类型、对象范围与章节范围约束。
func (d *DB) GetChartRenderJob(ctx context.Context, tenant string, taskID, id int64) (*ChartRenderJob, *ProposalChart, error) {
	tenant = tenantOr(tenant)
	if err := CheckJobKindScope(ctx, JobKindChartRender); err != nil {
		return nil, nil, err
	}
	if err := d.checkJobOwnedByIntent(ctx, tenant, taskID, JobRef{Kind: JobKindChartRender, ID: id}); err != nil {
		return nil, nil, err
	}
	j, err := scanChartJob(d.QueryRowContext(ctx, `SELECT `+chartJobCols+` FROM chart_render_jobs WHERE tenant_id = $1 AND task_id = $2 AND id = $3`, tenant, taskID, id))
	if err != nil {
		return nil, nil, err
	}
	c, err := getChartTx(ctx, d, tenant, taskID, j.ChartID)
	if err != nil {
		return nil, nil, err
	}
	if err := CheckSectionScope(ctx, c.SectionID); err != nil {
		return nil, nil, err
	}
	return j, c, nil
}

// CancelChartRenderJob 取消排队 / 执行中的作业：写终态、推进令牌，旧执行者的成功、失败与心跳都无法写回；已终态时原样返回。
// 范围约束同 GetChartRenderJob。锁序：任务（共享）→ 图表 → 作业。
func (d *DB) CancelChartRenderJob(ctx context.Context, tenant string, taskID, id int64, actor string) (*ChartRenderJob, error) {
	tenant = tenantOr(tenant)
	j, _, err := d.GetChartRenderJob(ctx, tenant, taskID, id)
	if err != nil {
		return nil, err
	}
	var out *ChartRenderJob
	err = d.Transaction(ctx, func(tx *DB) error {
		var paused bool
		if err := tx.QueryRowContext(ctx, `SELECT paused FROM tasks WHERE id = $1 FOR SHARE`, taskID).Scan(&paused); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `SELECT id FROM proposal_charts WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenant, j.ChartID); err != nil {
			return err
		}
		cur, err := scanChartJob(tx.QueryRowContext(ctx, `SELECT `+chartJobCols+` FROM chart_render_jobs WHERE id = $1 FOR UPDATE`, id))
		if err != nil {
			return err
		}
		if !cur.Active() {
			out = cur
			return nil
		}
		msg := "已取消"
		if who := strings.TrimSpace(actor); who != "" {
			msg += "（" + who + "）"
		}
		out, err = finishChartJobTx(ctx, tx, id, ChartJobCancelled, JobErrCancelled, msg, actor)
		return err
	})
	return out, err
}

// ChartJobFilter 过滤任务内的渲染作业列表（新者在前，Before 为作业 ID 游标）。
type ChartJobFilter struct {
	ChartID   int64
	SectionID int64
	Before    int64
	Limit     int
}

// ListChartRenderJobs 列出任务内的渲染作业（HTTP 页面恢复与诊断；Agent 经 agent_job_status / chart_render_status 读取）。
func (d *DB) ListChartRenderJobs(ctx context.Context, tenant string, taskID int64, f ChartJobFilter) ([]*ChartRenderJob, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	rows, err := d.QueryContext(ctx, `SELECT `+prefixCols("j.", chartJobCols)+` FROM chart_render_jobs j
		JOIN proposal_charts c ON c.tenant_id = j.tenant_id AND c.id = j.chart_id
		WHERE j.tenant_id = $1 AND j.task_id = $2 AND ($3::bigint = 0 OR j.chart_id = $3) AND ($4::bigint = 0 OR c.section_id = $4)
		  AND ($5::bigint = 0 OR j.id < $5)
		ORDER BY j.id DESC LIMIT $6`, tenantOr(tenant), taskID, f.ChartID, f.SectionID, f.Before, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*ChartRenderJob{}
	for rows.Next() {
		j, err := scanChartJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
