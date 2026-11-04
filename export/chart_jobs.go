package export

// S3（AGENT_TOOL_SKILLS_PLAN 3.2 / 4.3）：图表渲染作业的服务层与执行器。
//   - 服务层校验规格并生成确定性源码、计算规范化请求哈希、在首次提交时固定执行版本，再把纯数据交给 db；
//     同键重放返回原作业，不受当前运行能力影响，只有新请求检查渲染器是否可用（RUNTIME_UNAVAILABLE）。
//   - HTTP 章节 Writer 的图表计划先在服务层全部校验，无效规格在提交修订前报错；修订与图表作业在 db 同事务登记。
//   - ChartRunner 从 chart_render_jobs 领取（租约 + 围栏令牌），在数据库事务外渲染：产物按作业 + 令牌隔离写入
//     <tenant>/charts/task-<id>/job-<id>/t<token>/，写回被拒绝时清理本次产物，成功后清理同一作业的其他尝试目录。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/toolhealth"
)

// ExecutorNative 是设备选型表的执行版本：导出为原生 Word 表格，不依赖 mmdc。
const ExecutorNative = "native"

// ChartJobDeadline 是图表渲染作业从首次提交起的截止时长（AUTOBID_CHART_JOB_DEADLINE_SECONDS，默认 600 秒）。
func ChartJobDeadline() time.Duration {
	return time.Duration(envInt("AUTOBID_CHART_JOB_DEADLINE_SECONDS", int(db.DefaultChartJobDeadline/time.Second))) * time.Second
}

// RendererLabel 是渲染器的执行版本标签（如 "mmdc 11.17.0"），作业在首次提交时固定。
func RendererLabel(r Renderer) string {
	if r == nil {
		return ""
	}
	if v := rendererVersion(r); v != "" {
		return r.Name() + " " + v
	}
	return r.Name()
}

// ChartRenderRequest 是一次图表渲染提交（Tool 与 HTTP 共用）：新图表给出 section_id + expected_rev_no + 规格，
// 重试已有图表只给 chart_id。request_key 由调用方生成，同一次提交的重发复用，改变输入或明确重新执行时换新键。
type ChartRenderRequest struct {
	SectionID     int64           `json:"section_id"`
	ExpectedRevNo int             `json:"expected_rev_no"`
	ChartID       int64           `json:"chart_id"`
	ChartType     string          `json:"chart_type"`
	Title         string          `json:"title"`
	Spec          json.RawMessage `json:"spec"`
	RequestKey    string          `json:"request_key"`
}

// requestHash 对规范化的客户端输入求哈希（规格已由 BuildChartSource 规范化）。
func requestHash(parts ...any) string {
	b, _ := json.Marshal(parts)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// executorFor 返回新作业固定的执行版本；Mermaid 图表在渲染器不可用时拒绝提交。
func (s *Service) executorFor(format string) (string, error) {
	if format == db.ChartSourceJSON {
		return ExecutorNative, nil
	}
	if s == nil || s.Renderer == nil {
		reason := ""
		if s != nil {
			reason = strings.TrimPrefix(s.RendererReason, ErrRendererMissing.Error()+": ")
		}
		if reason == "" {
			return "", ErrRendererMissing
		}
		return "", fmt.Errorf("%w: %s", ErrRendererMissing, toolhealth.Sanitize(reason))
	}
	return RendererLabel(s.Renderer), nil
}

// SubmitChartRender 校验并提交一次图表渲染（新图表或重试已有图表）。宿主绑定 Intent 的调用同事务订阅该作业。
func (s *Service) SubmitChartRender(ctx context.Context, tenant string, taskID int64, actor string, req ChartRenderRequest) (*db.ChartRenderSubmission, error) {
	key := strings.TrimSpace(req.RequestKey)
	in := db.SubmitChartRenderInput{RequestKey: key, ExpectedRevNo: req.ExpectedRevNo, Deadline: ChartJobDeadline(), Actor: actor}
	var format string
	if req.ChartID > 0 {
		if req.SectionID > 0 || req.ChartType != "" || strings.TrimSpace(req.Title) != "" || len(req.Spec) > 0 {
			return nil, specErr("chart_id retries an existing chart; section_id, chart_type, title and spec must be empty")
		}
		c, err := s.DB.GetChart(ctx, tenant, taskID, req.ChartID)
		if err != nil {
			return nil, err
		}
		in.ChartID, format = c.ID, c.SourceFormat
		in.RequestHash = requestHash("retry", c.ID, c.SourceHash, req.ExpectedRevNo)
	} else {
		src, err := BuildChartSource(req.ChartType, req.Title, req.Spec)
		if err != nil {
			return nil, err
		}
		title := strings.TrimSpace(req.Title)
		in.SectionID, format = req.SectionID, src.Format
		in.Chart = db.ChartRenderSpec{ChartType: req.ChartType, Title: title, Spec: src.Spec, SourceFormat: src.Format, Source: src.Source, SourceHash: src.Hash}
		in.RequestHash = requestHash("create", req.SectionID, req.ExpectedRevNo, req.ChartType, title, src.Spec)
	}
	if err := db.ValidClientRequestKey(key); err != nil {
		return nil, err
	}
	// 同键重放返回原作业，不受当前运行能力影响；只有新请求检查渲染器并固定执行版本。
	if _, err := s.DB.FindChartRenderJob(ctx, tenant, taskID, key); errors.Is(err, db.ErrNotFound) {
		exec, err := s.executorFor(format)
		if err != nil {
			return nil, err
		}
		in.Chart.Executor = exec
	} else if err != nil {
		return nil, err
	}
	sub, err := s.DB.SubmitChartRender(ctx, tenant, taskID, in)
	if err == nil && !sub.Replayed {
		s.WakeChartRunner()
	}
	return sub, err
}

// ChartPlan 是 HTTP 章节 Writer 随草稿输出的一张图表计划（同一修订内 code 唯一）。
type ChartPlan struct {
	Code      string          `json:"code"`
	Title     string          `json:"title"`
	ChartType string          `json:"chart_type"`
	Spec      json.RawMessage `json:"spec"`
}

// BuildChartPlans 校验全部图表计划并生成确定性源码（任何一项无效即整体报错，调用方不得保存修订）。
// 渲染器不可用时 Mermaid 计划仍登记，执行时以 RUNTIME_UNAVAILABLE 失败并标记为未渲染；s 可为 nil。
func (s *Service) BuildChartPlans(plans []ChartPlan) (*db.ChartPlanBatch, error) {
	if len(plans) == 0 {
		return nil, nil
	}
	if len(plans) > db.MaxChartPlans {
		return nil, specErr("at most %d chart plans per section draft, got %d", db.MaxChartPlans, len(plans))
	}
	out := make([]db.ChartPlan, 0, len(plans))
	for _, p := range plans {
		code, title := strings.TrimSpace(p.Code), strings.TrimSpace(p.Title)
		src, err := BuildChartSource(p.ChartType, title, p.Spec)
		if err != nil {
			return nil, fmt.Errorf("chart plan %q: %w", code, err)
		}
		exec, _ := s.executorFor(src.Format)
		out = append(out, db.ChartPlan{Code: code, RequestHash: requestHash("plan", code, p.ChartType, title, src.Spec),
			ChartRenderSpec: db.ChartRenderSpec{ChartType: p.ChartType, Title: title, Spec: src.Spec, SourceFormat: src.Format, Source: src.Source,
				SourceHash: src.Hash, Executor: exec}})
	}
	if err := db.ValidateChartPlans(out); err != nil {
		return nil, err
	}
	return &db.ChartPlanBatch{Plans: out, Deadline: ChartJobDeadline()}, nil
}

// executeChartJob 在数据库事务外执行一次渲染；ctx 取消（租约丢失、截止、停机）时结果不得写回。
func (s *Service) executeChartJob(ctx context.Context, cl *db.ChartRenderClaim) db.ChartRenderResult {
	c, j := cl.Chart, cl.Job
	unavailable := func(msg string) db.ChartRenderResult {
		return db.ChartRenderResult{Render: db.ChartRender{Status: db.ChartUnrendered, Error: msg}, ErrorCode: db.JobErrRuntimeUnavailable}
	}
	failed := func(msg string) db.ChartRenderResult {
		return db.ChartRenderResult{Render: db.ChartRender{Status: db.ChartFailed, Error: truncateRunes(msg, 500)}, ErrorCode: db.JobErrRenderFailed, Retry: true}
	}
	switch {
	case c.SourceFormat == db.ChartSourceJSON:
		return db.ChartRenderResult{Render: db.ChartRender{Status: db.ChartRendered, Format: db.ChartFormatNative}}
	case s.Renderer == nil:
		reason := s.RendererReason
		if reason == "" {
			reason = ErrRendererMissing.Error()
		}
		return unavailable("mmdc 不可用：" + reason)
	case j.Executor != RendererLabel(s.Renderer):
		return unavailable(fmt.Sprintf("作业提交时固定的渲染器为 %q，当前为 %q；执行版本不随配置变化，请重新提交", j.Executor, RendererLabel(s.Renderer)))
	}
	png, err := s.Renderer.RenderPNG(ctx, c.Source)
	if err != nil {
		if ctx.Err() == nil {
			s.Health.Degrade(toolhealth.Mermaid, err)
		}
		return failed(err.Error())
	}
	s.Health.Recover(toolhealth.Mermaid)
	cfg, _, err := image.DecodeConfig(bytes.NewReader(png))
	if err != nil {
		return failed("mmdc 输出不是有效的 PNG：" + err.Error())
	}
	sum := sha256Hex(png)
	rel := s.Store.chartJobRel(cl.Tenant, j.TaskID, j.ID, j.LeaseToken, sum)
	if _, err := s.Store.WriteFile(rel, png); err != nil {
		return failed("写入图表产物失败：" + err.Error())
	}
	return db.ChartRenderResult{Render: db.ChartRender{Status: db.ChartRendered, Format: db.ChartFormatPNG, ArtifactPath: rel, ArtifactHash: sum,
		ArtifactSize: int64(len(png)), WidthPx: cfg.Width, HeightPx: cfg.Height}}
}

// removeChartAttempt 删除本次执行（令牌）的产物目录。
func (s *Service) removeChartAttempt(cl *db.ChartRenderClaim) {
	if dir, err := s.Store.Abs(s.Store.chartAttemptRel(cl.Tenant, cl.Job.TaskID, cl.Job.ID, cl.Job.LeaseToken)); err == nil {
		removeAllRetry(dir)
	}
}

// cleanupOtherChartAttempts 在成功写回后删除同一作业其他令牌的产物目录。
func (s *Service) cleanupOtherChartAttempts(cl *db.ChartRenderClaim) {
	dir, err := s.Store.Abs(s.Store.chartJobDir(cl.Tenant, cl.Job.TaskID, cl.Job.ID))
	if err != nil {
		return
	}
	entries, _ := os.ReadDir(dir)
	keep := "t" + strconv.FormatInt(cl.Job.LeaseToken, 10)
	for _, e := range entries {
		if e.Name() != keep {
			removeAllRetry(filepath.Join(dir, e.Name()))
		}
	}
}

// ChartRunner 从 chart_render_jobs 领取并执行渲染作业：心跳续租，租约被回收、作业被取消或任务被暂停时取消本次执行；
// 写回必须携带当前令牌。领取同时回收过期租约，并结束已耗尽或已过期（superseded）的作业。
type ChartRunner struct {
	Service  *Service
	Owner    string
	Workers  int
	Lease    time.Duration
	Poll     time.Duration
	Logger   *slog.Logger
	OnChange func(job *db.ChartRenderJob, summary string)
	// TaskID > 0 时只领取该任务的作业（测试与单任务调试），0 表示全部任务。
	TaskID int64

	wake chan struct{}
	once sync.Once
}

func (r *ChartRunner) init() {
	r.once.Do(func() {
		if r.Workers <= 0 {
			r.Workers = 2
		}
		if r.Lease <= 0 {
			r.Lease = 2 * time.Minute
		}
		if r.Poll <= 0 {
			r.Poll = 2 * time.Second
		}
		if r.Logger == nil {
			r.Logger = slog.Default()
		}
		r.wake = make(chan struct{}, r.Workers)
	})
}

// Wake 通知空闲执行者立即领取（提交作业后调用；作业已持久化，唤醒只是加速）。
func (r *ChartRunner) Wake() {
	if r == nil {
		return
	}
	r.init()
	for i := 0; i < r.Workers; i++ {
		select {
		case r.wake <- struct{}{}:
		default:
		}
	}
}

// Start 启动执行者（ctx 取消时退出）。
func (r *ChartRunner) Start(ctx context.Context) {
	r.init()
	for i := 0; i < r.Workers; i++ {
		go func() {
			t := time.NewTicker(r.Poll)
			defer t.Stop()
			for {
				for {
					ok, err := r.RunOnce(ctx)
					if err != nil && ctx.Err() == nil {
						r.Logger.Warn("chart render job", "err", err)
					}
					if !ok || ctx.Err() != nil {
						break
					}
				}
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				case <-r.wake:
				}
			}
		}()
	}
}

// RunOnce 处理一个作业；没有可处理的作业时返回 false。
func (r *ChartRunner) RunOnce(ctx context.Context) (bool, error) {
	r.init()
	cl, done, err := r.Service.DB.ClaimChartRenderJob(ctx, r.Owner, r.Lease, r.TaskID)
	if err != nil {
		return false, err
	}
	if done != nil {
		r.notify(done, chartJobSummary(done))
		return true, nil
	}
	if cl == nil {
		return false, nil
	}
	job := cl.Job
	r.notify(job, fmt.Sprintf("图表渲染作业 #%d（图表 #%d《%s》）开始第 %d 次执行", job.ID, job.ChartID, cl.Chart.Title, job.Attempts))
	jobCtx, cancel := context.WithDeadline(ctx, job.DeadlineAt)
	defer cancel()
	var lost atomic.Bool
	stop := make(chan struct{})
	var hb sync.WaitGroup
	hb.Add(1)
	go func() { // 心跳：租约丢失（取消、暂停、回收）时立即取消本次渲染
		defer hb.Done()
		t := time.NewTicker(r.Lease / 3)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if err := r.Service.DB.HeartbeatChartRenderJob(ctx, job.ID, job.LeaseToken, r.Lease); errors.Is(err, db.ErrLeaseLost) {
					lost.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	res := r.Service.executeChartJob(jobCtx, cl)
	close(stop)
	hb.Wait()
	if jobCtx.Err() != nil {
		// 租约丢失、截止或停机：不写回。截止 / 停机后的作业由下一次领取判定耗尽或重新执行。
		r.Service.removeChartAttempt(cl)
		if lost.Load() {
			return true, fmt.Errorf("chart job %d: lease lost", job.ID)
		}
		return true, nil
	}
	fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	out, err := r.Service.DB.FinishChartRenderJob(fctx, cl, res)
	fcancel()
	if err != nil {
		r.Service.removeChartAttempt(cl)
		return true, fmt.Errorf("chart job %d: %w", job.ID, err)
	}
	if out.Status == db.ChartJobSucceeded && res.Render.Format == db.ChartFormatPNG {
		r.Service.cleanupOtherChartAttempts(cl)
	} else {
		r.Service.removeChartAttempt(cl)
	}
	r.notify(out, chartJobSummary(out))
	return true, nil
}

func chartJobSummary(j *db.ChartRenderJob) string {
	switch j.Status {
	case db.ChartJobSucceeded:
		return fmt.Sprintf("图表渲染作业 #%d（图表 #%d）已完成", j.ID, j.ChartID)
	case db.ChartJobQueued:
		return fmt.Sprintf("图表渲染作业 #%d（图表 #%d）第 %d 次执行失败，将在原期限内重试：%s", j.ID, j.ChartID, j.Attempts, j.Error)
	case db.ChartJobSuperseded:
		return fmt.Sprintf("图表渲染作业 #%d（图表 #%d）已被章节新修订取代：%s", j.ID, j.ChartID, j.Error)
	default:
		return fmt.Sprintf("图表渲染作业 #%d（图表 #%d）%s：%s", j.ID, j.ChartID, j.Status, j.Error)
	}
}

func (r *ChartRunner) notify(job *db.ChartRenderJob, summary string) {
	if r.OnChange != nil {
		r.OnChange(job, summary)
	}
}
