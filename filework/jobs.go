package filework

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/ingest"
	"github.com/autobid/autobid-engine/toolhealth"
)

func (p *Processor) Enqueue(ctx context.Context, taskID int64, in ProcessInput, actor string) (*db.FixedFileJob, error) {
	in.RequestKey = strings.TrimSpace(in.RequestKey)
	if len(in.RequestKey) == 0 || len(in.RequestKey) > 128 || strings.HasPrefix(in.RequestKey, "_fixed:") {
		return nil, ingest.ErrInvalidInput
	}
	fingerprint := db.ContentHash(string(marshal(in)))
	// Replays (any state) return the original job before any capability check: the route was fixed then.
	old, err := p.DB.FindFixedFileJob(ctx, db.DefaultTenant, taskID, in.RequestKey)
	if err == nil {
		if old.Fingerprint != fingerprint {
			return nil, fmt.Errorf("%w: %w", db.ErrFileProcessingConflict, db.ErrIdempotencyKeyConflict)
		}
		return old, nil
	}
	if !errors.Is(err, db.ErrNotFound) {
		return nil, err
	}
	if in.Operation == "archive" && (in.CandidateOnly || in.ParserProfile != "") {
		return nil, fmt.Errorf("%w: ZIP 展开不接受解析配置或候选模式", ingest.ErrInvalidInput)
	}
	if in.DocumentID > 0 {
		doc, err := p.DB.GetTenderDocument(ctx, taskID, in.DocumentID)
		if err != nil {
			return nil, err
		}
		if doc.OriginalFileID == nil || doc.ParseRevision != in.ExpectedRevision {
			return nil, db.ErrParseRevisionConflict
		}
		in.FileID = *doc.OriginalFileID
		in.TenderNo = doc.TenderNo
		in.LotNo = doc.LotNo
		in.Title = doc.Title
		in.DocRole = doc.DocRole
		in.PublishedAt = doc.PublishedAt
	} else if strings.TrimSpace(in.TenderNo) == "" && in.Operation != "archive" {
		return nil, ingest.ErrInvalidInput
	}
	obj, err := p.checkJobFile(ctx, taskID, in)
	if err != nil {
		return nil, err
	}
	executor := ArchiveExecutor
	if in.Operation != "archive" {
		if _, executor, err = p.ResolveRoute(ctx, obj.Format, in.ParserProfile); err != nil {
			return nil, err
		}
	}
	return p.DB.EnqueueFixedFileJob(ctx, db.FixedFileJob{TenantID: db.DefaultTenant, TaskID: taskID, FileID: obj.ID, SourceHash: obj.SHA256, RequestKey: in.RequestKey,
		Fingerprint: fingerprint, Input: marshal(in), Executor: executor, CreatedBy: actor})
}

// checkJobFile validates the file a fixed job will read: no scripts or unsupported formats, ZIP only for archive
// expansion, and ZIP children only from a retained original ZIP.
func (p *Processor) checkJobFile(ctx context.Context, taskID int64, in ProcessInput) (*db.TaskFileObject, error) {
	obj, _, err := p.DB.GetTaskFileObject(ctx, db.DefaultTenant, taskID, in.FileID)
	if err != nil {
		return nil, err
	}
	if in.Operation != "" && in.Operation != "archive" {
		return nil, ingest.ErrInvalidInput
	}
	if obj.Kind == db.FileKindScript || obj.Format == "unsupported" {
		return nil, ingest.ErrUnsupportedFormat
	}
	if (obj.Format == ingest.FormatZIP) != (in.Operation == "archive") || (in.Operation == "archive" && (in.DocumentID > 0 || obj.Kind != db.FileKindOriginal)) {
		return nil, ingest.ErrUnsupportedFormat
	}
	if obj.Kind == db.FileKindDerived {
		if obj.ParentFileID == nil {
			return nil, db.ErrInvalidParseRevision
		}
		parent, _, err := p.DB.GetTaskFileObject(ctx, db.DefaultTenant, taskID, *obj.ParentFileID)
		if err != nil {
			return nil, err
		}
		if parent.Kind != db.FileKindOriginal || parent.Format != ingest.FormatZIP {
			return nil, db.ErrInvalidParseRevision
		}
	}
	return obj, nil
}

type Runner struct {
	Processor *Processor
	Owner     string
	Workers   int
	Lease     time.Duration
	Poll      time.Duration
	TaskID    int64
	// RetryDelay is the wait before a job released after a transient OCR failure is claimed again (default 5s x attempts).
	RetryDelay time.Duration
	OnChange   func(*db.FixedFileJob)
	once       sync.Once
	wake       chan struct{}
	active     sync.Map // job ID -> fixedExecution
}

type fixedExecution struct {
	taskID int64
	cancel context.CancelFunc
	done   chan struct{}
}

func (r *Runner) init() {
	r.once.Do(func() {
		if r.Workers <= 0 {
			r.Workers = 2
		}
		if r.Workers > 8 {
			r.Workers = 8
		}
		if r.Lease <= 0 {
			r.Lease = 30 * time.Second
		}
		if r.Lease < 300*time.Millisecond {
			r.Lease = 300 * time.Millisecond
		}
		if r.Poll <= 0 {
			r.Poll = time.Second
		}
		r.wake = make(chan struct{}, r.Workers)
	})
}
func (r *Runner) Wake() {
	r.init()
	for i := 0; i < r.Workers; i++ {
		select {
		case r.wake <- struct{}{}:
		default:
		}
	}
}
func (r *Runner) Cancel(id int64) {
	if v, ok := r.active.Load(id); ok {
		v.(*fixedExecution).cancel()
	}
}

// The caller first persists pause/cancellation, preventing any new job claim.
func (r *Runner) StopTask(ctx context.Context, taskID int64) error {
	var waits []chan struct{}
	r.active.Range(func(_, value any) bool {
		x := value.(*fixedExecution)
		if x.taskID == taskID {
			x.cancel()
			waits = append(waits, x.done)
		}
		return true
	})
	for _, done := range waits {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// Run blocks until all workers stop; the caller owns the service lifecycle.
func (r *Runner) Run(ctx context.Context) {
	r.init()
	var wg sync.WaitGroup
	for i := 0; i < r.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(r.Poll)
			defer ticker.Stop()
			for {
				if ctx.Err() != nil {
					return
				}
				ok, err := r.RunOnce(ctx)
				if err != nil && ctx.Err() == nil {
					slog.Warn("fixed file job failed", "error", err)
				}
				if ok {
					continue
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				case <-r.wake:
				}
			}
		}()
	}
	wg.Wait()
}

// retryDelay backs off after a transient runtime failure; RetryDelay overrides it (tests).
func (r *Runner) retryDelay(j *db.FixedFileJob) time.Duration {
	if r.RetryDelay > 0 {
		return r.RetryDelay
	}
	return time.Duration(max(1, j.Attempts)) * 5 * time.Second
}

func (r *Runner) notify(j *db.FixedFileJob) {
	if r.OnChange != nil {
		r.OnChange(j)
	}
}
func (r *Runner) RunOnce(ctx context.Context) (bool, error) {
	r.init()
	j, err := r.Processor.DB.ClaimFixedFileJob(ctx, r.Owner, r.Lease, r.TaskID)
	if err != nil || j == nil {
		return false, err
	}
	r.notify(j)
	jobCtx, cancel := context.WithDeadline(ctx, j.DeadlineAt)
	defer cancel()
	execution := &fixedExecution{taskID: j.TaskID, cancel: cancel, done: make(chan struct{})}
	r.active.Store(j.ID, execution)
	defer func() { r.active.Delete(j.ID); close(execution.done) }()
	// Close the claim -> registration race with pause/delete before touching inputs.
	if err := r.Processor.DB.HeartbeatFixedFileJob(jobCtx, j, r.Lease); err != nil {
		return true, err
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(r.Lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-jobCtx.Done():
				return
			case <-ticker.C:
				if err := r.Processor.DB.HeartbeatFixedFileJob(jobCtx, j, r.Lease); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	var in ProcessInput
	err = json.Unmarshal(j.Input, &in)
	if err == nil {
		in.RequestKey = fmt.Sprintf("_fixed:%d", j.ID)
		if in.Operation == "archive" {
			err = r.Processor.expandArchive(jobCtx, j)
		} else {
			_, err = r.Processor.process(jobCtx, j.TaskID, in, j.CreatedBy, j)
		}
	}
	close(stop)
	<-done
	if err == nil && r.Processor.Runtime != nil {
		if _, ok := executorOCR(j.Executor); ok {
			r.Processor.Runtime.Recover(toolhealth.OCR)
		}
	}
	if err != nil && jobCtx.Err() == nil && !errors.Is(err, db.ErrLeaseLost) {
		var saveErr error
		if errors.Is(err, ErrRuntimeTransient) {
			// The local OCR service is restarting or busy: retry within the original deadline and attempt budget.
			saveErr = r.Processor.DB.ReleaseFixedFileJob(ctx, j, db.JobErrRuntimeUnavailable, toolhealth.Sanitize(err.Error()), r.retryDelay(j))
			if r.Processor.Runtime != nil {
				r.Processor.Runtime.Degrade(toolhealth.OCR, err)
			}
		} else {
			// Deterministic processing failures are terminal; explicit retry uses a new key.
			code := db.JobErrParseFailed
			if errors.Is(err, ErrRuntimeChanged) {
				code = db.JobErrRuntimeUnavailable
			}
			saveErr = r.Processor.DB.Transaction(ctx, func(tx *db.DB) error {
				if e := tx.LockFileProcessingTask(ctx, j.TenantID, j.TaskID); e != nil {
					return e
				}
				return tx.FinishFixedFileJobCode(ctx, j, nil, code, toolhealth.Sanitize(err.Error()))
			})
		}
		if saveErr != nil {
			return true, saveErr
		}
	}
	// On shutdown/lost lease, leave the durable attempt for bounded recovery.
	if latest, e := r.Processor.DB.GetFixedFileJob(ctx, j.TenantID, j.TaskID, j.ID); e == nil {
		r.notify(latest)
	}
	return true, err
}

func (p *Processor) expandArchive(ctx context.Context, j *db.FixedFileJob) error {
	data, obj, err := p.Store.Read(ctx, j.TenantID, j.TaskID, j.FileID)
	if err != nil {
		return err
	}
	if obj.SHA256 != j.SourceHash {
		return db.ErrInvalidParseRevision
	}
	type child struct {
		entry ingest.ArchiveEntry
		data  []byte
	}
	var children []child
	_, err = ingest.WalkArchive(data, ingest.DefaultArchiveLimits(), func(e *ingest.ArchiveEntry, b []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		children = append(children, child{*e, b})
		return nil
	})
	if err != nil {
		return err
	}
	return p.DB.Transaction(ctx, func(tx *db.DB) error {
		if err := tx.LockFileProcessingTask(ctx, j.TenantID, j.TaskID); err != nil {
			return err
		}
		if err := tx.GuardFixedFileJob(ctx, j); err != nil {
			return err
		}
		store, err := NewStore(tx, p.Store.Root())
		if err != nil {
			return err
		}
		for _, c := range children {
			status := "listed"
			if c.entry.Format == "unsupported" {
				status = "unsupported"
			}
			if _, err := store.PutBytes(ctx, PutOptions{TenantID: j.TenantID, TaskID: j.TaskID, Kind: db.FileKindDerived, OriginalName: path.Base(c.entry.Path), MediaType: c.entry.MediaType, ArchivePath: c.entry.Path, Format: c.entry.Format, ProcessStatus: status, ParentFileID: &j.FileID, CreatedBy: j.CreatedBy}, c.data); err != nil {
				return err
			}
		}
		return tx.FinishFixedFileJob(ctx, j, nil, "")
	})
}
