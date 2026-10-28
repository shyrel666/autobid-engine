package export

// Runner 从 export_jobs 领取任务（FOR UPDATE SKIP LOCKED + 租约 + 围栏令牌）并执行：心跳续租，租约被回收时取消本次执行；
// 完成 / 失败写入必须携带当前令牌；启动时先回收过期的 running 任务。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/autobid/autobid-engine/db"
)

// Runner 从 export_jobs 领取并执行任务。
type Runner struct {
	Service  *Service
	Owner    string
	Workers  int
	Lease    time.Duration
	Poll     time.Duration
	Logger   *slog.Logger
	OnChange func(job *db.ExportJob, summary string)
	// TaskID > 0 时只领取该任务的导出（测试与单任务调试），0 表示全部任务。
	TaskID int64

	wake chan struct{}
	once sync.Once
}

func (r *Runner) init() {
	r.once.Do(func() {
		if r.Workers <= 0 {
			r.Workers = 2
		}
		if r.Lease <= 0 {
			r.Lease = 2 * time.Minute
		}
		if r.Poll <= 0 {
			r.Poll = 3 * time.Second
		}
		if r.Logger == nil {
			r.Logger = slog.Default()
		}
		r.wake = make(chan struct{}, r.Workers)
	})
}

// Wake 通知空闲执行者立即领取（创建任务后调用）。
func (r *Runner) Wake() {
	r.init()
	for i := 0; i < r.Workers; i++ {
		select {
		case r.wake <- struct{}{}:
		default:
		}
	}
}

// Start 先回收过期任务，再启动执行者（ctx 取消时退出）。
func (r *Runner) Start(ctx context.Context) {
	r.init()
	if ids, err := r.Service.DB.ReclaimExpiredExportJobs(ctx, r.TaskID); err != nil {
		r.Logger.Warn("reclaim export jobs failed", "err", err)
	} else if len(ids) > 0 {
		r.Logger.Info("reclaimed expired export jobs", "jobs", ids)
	}
	for i := 0; i < r.Workers; i++ {
		go func() {
			t := time.NewTicker(r.Poll)
			defer t.Stop()
			for {
				for {
					ok, err := r.RunOnce(ctx)
					if err != nil && ctx.Err() == nil {
						r.Logger.Warn("export job failed", "err", err)
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

// RunOnce 领取并执行一个任务；没有可领取任务时返回 false。
func (r *Runner) RunOnce(ctx context.Context) (bool, error) {
	r.init()
	ref, err := r.Service.DB.ClaimExportJob(ctx, r.Owner, r.Lease, r.TaskID)
	if err != nil || ref == nil {
		return false, err
	}
	job := ref.ExportJob
	r.notify(job, fmt.Sprintf("导出任务 #%d（%s）开始第 %d 次执行", job.ID, job.Format, job.Attempts))
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var lost sync.WaitGroup
	lost.Add(1)
	stop := make(chan struct{})
	go func() { // 心跳：租约被回收给其他执行者时取消本次执行
		defer lost.Done()
		t := time.NewTicker(r.Lease / 3)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if err := r.Service.DB.HeartbeatExportJob(ctx, job.ID, job.LeaseToken, r.Lease); errors.Is(err, db.ErrLeaseLost) {
					cancel()
					return
				}
			}
		}
	}()
	res, execErr := r.Service.Execute(jobCtx, ref.Tenant, job)
	close(stop)
	lost.Wait()
	if execErr == nil {
		if err := r.Service.DB.CompleteExportJob(ctx, job.ID, job.LeaseToken, *res); err != nil {
			if errors.Is(err, db.ErrLeaseLost) {
				r.Service.removeAttempt(ref.Tenant, job)
			}
			return true, err
		}
		r.Service.cleanupOtherAttempts(ref.Tenant, job, job.LeaseToken)
		r.notify(job, fmt.Sprintf("导出任务 #%d（%s）完成：%s", job.ID, job.Format, res.ArtifactName))
		return true, nil
	}
	if jobCtx.Err() != nil && ctx.Err() == nil {
		r.Service.removeAttempt(ref.Tenant, job)
		return true, fmt.Errorf("export job %d: lease lost: %w", job.ID, execErr)
	}
	var perm permanentError
	retry := !errors.As(execErr, &perm)
	if err := r.Service.DB.FailExportJob(ctx, job.ID, job.LeaseToken, execErr.Error(), retry); err != nil {
		return true, err
	}
	r.Service.removeAttempt(ref.Tenant, job)
	r.notify(job, fmt.Sprintf("导出任务 #%d（%s）第 %d 次执行失败：%v", job.ID, job.Format, job.Attempts, execErr))
	return true, execErr
}

func (r *Runner) notify(job *db.ExportJob, summary string) {
	if r.OnChange != nil {
		r.OnChange(job, summary)
	}
}
