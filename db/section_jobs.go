package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SectionWriteJob preserves the original request across process restarts.
// Lock order: task -> job -> approved outline -> section. Generation holds no locks.
type SectionWriteJob struct {
	ID       int64  `json:"job_id"`
	TenantID string `json:"-"`
	SectionClaim
	RequestKey  string    `json:"request_key"`
	Instruction string    `json:"instruction"`
	Status      string    `json:"status"`
	Attempts    int       `json:"attempts"`
	MaxAttempts int       `json:"max_attempts"`
	DeadlineAt  time.Time `json:"deadline_at"`
	Error       string    `json:"error,omitempty"`
	ResultRevNo *int      `json:"result_rev_no,omitempty"`
	CreatedBy   string    `json:"created_by"`
}

const sectionJobCols = `id,tenant_id,task_id,section_id,section_code,base_rev_no,outline_version,request_key,instruction,status,attempts,max_attempts,deadline_at,lease_token,COALESCE(lease_until,'epoch'::timestamptz),error,result_rev_no,created_by`

func scanSectionJob(r rowScanner) (*SectionWriteJob, error) {
	j := new(SectionWriteJob)
	err := r.Scan(&j.ID, &j.TenantID, &j.TaskID, &j.SectionID, &j.SectionCode, &j.BaseRevNo, &j.BasedOnOutlineVersion, &j.RequestKey, &j.Instruction, &j.Status, &j.Attempts, &j.MaxAttempts, &j.DeadlineAt, &j.LeaseToken, &j.LeaseUntil, &j.Error, &j.ResultRevNo, &j.CreatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

func lockSectionJobTask(ctx context.Context, q queryer, taskID int64) (bool, error) {
	var paused bool
	err := q.QueryRowContext(ctx, `SELECT paused FROM tasks WHERE id=$1 FOR UPDATE`, taskID).Scan(&paused)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return paused, err
}

func (d *DB) GetSectionWriteJob(ctx context.Context, tenant string, taskID, id int64) (*SectionWriteJob, error) {
	return scanSectionJob(d.QueryRowContext(ctx, `SELECT `+sectionJobCols+` FROM section_write_jobs WHERE tenant_id=$1 AND task_id=$2 AND id=$3`, tenantOr(tenant), taskID, id))
}

func (d *DB) ListSectionWriteJobs(ctx context.Context, tenant string, taskID int64) ([]*SectionWriteJob, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+sectionJobCols+` FROM section_write_jobs WHERE tenant_id=$1 AND task_id=$2 ORDER BY id DESC LIMIT 100`, tenantOr(tenant), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*SectionWriteJob{}
	for rows.Next() {
		j, err := scanSectionJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (d *DB) EnqueueSectionWrite(ctx context.Context, tenant string, taskID, sectionID int64, key, instruction, actor string, base *int) (*SectionWriteJob, error) {
	tenant = tenantOr(tenant)
	key = strings.TrimSpace(key)
	if key == "" || len(key) > 200 || len(instruction) > 64000 {
		return nil, fmt.Errorf("%w: invalid request key or instruction", ErrInvalidRevision)
	}
	var out *SectionWriteJob
	err := d.Transaction(ctx, func(tx *DB) error {
		paused, err := lockSectionJobTask(ctx, tx, taskID)
		if err != nil {
			return err
		}
		old, err := scanSectionJob(tx.QueryRowContext(ctx, `SELECT `+sectionJobCols+` FROM section_write_jobs WHERE tenant_id=$1 AND task_id=$2 AND request_key=$3`, tenant, taskID, key))
		if err == nil {
			if old.SectionID != sectionID || old.Instruction != instruction || old.CreatedBy != actor || (base != nil && *base != old.BaseRevNo) {
				return fmt.Errorf("%w: idempotency key reused with different input", ErrRevisionConflict)
			}
			out = old
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		if paused {
			return fmt.Errorf("%w: task paused", ErrSectionBusy)
		}
		var busy bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM section_write_jobs WHERE tenant_id=$1 AND section_id=$2 AND status IN ('queued','running'))`, tenant, sectionID).Scan(&busy); err != nil {
			return err
		}
		if busy {
			return ErrSectionBusy
		}
		claim, err := tx.ClaimSectionWrite(ctx, tenant, taskID, sectionID, actor, SectionLeaseTTL)
		if err != nil {
			return err
		}
		if base != nil && *base != claim.BaseRevNo {
			return ErrRevisionConflict
		}
		// A queued request is protected by its unique active-job index, not an expiring section lease.
		if err = tx.ReleaseSectionClaim(ctx, tenant, sectionID, claim.LeaseToken); err != nil {
			return err
		}
		out, err = scanSectionJob(tx.QueryRowContext(ctx, `INSERT INTO section_write_jobs(tenant_id,task_id,section_id,section_code,base_rev_no,outline_version,request_key,instruction,created_by,updated_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$9) RETURNING `+sectionJobCols, tenant, taskID, sectionID, claim.SectionCode, claim.BaseRevNo, claim.BasedOnOutlineVersion, key, instruction, actor))
		return err
	})
	return out, err
}

// ClaimSectionWriteJob reclaims an expired attempt without extending its deadline.
// perTask is enforced under a task row lock across all API instances.
func (d *DB) ClaimSectionWriteJob(ctx context.Context, taskID int64, perTask int) (*SectionWriteJob, error) {
	if perTask < 1 {
		perTask = 2
	}
	var out *SectionWriteJob
	err := d.Transaction(ctx, func(tx *DB) error {
		paused, err := lockSectionJobTask(ctx, tx, taskID)
		if err != nil || paused {
			return err
		}
		var active int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM section_write_jobs WHERE task_id=$1 AND status='running' AND lease_until>now()`, taskID).Scan(&active); err != nil {
			return err
		}
		if active >= perTask {
			return nil
		}
		j, err := scanSectionJob(tx.QueryRowContext(ctx, `SELECT `+sectionJobCols+` FROM section_write_jobs WHERE task_id=$1 AND (status='queued' OR (status='running' AND lease_until<=now())) ORDER BY id LIMIT 1 FOR UPDATE`, taskID))
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if j.LeaseToken > 0 {
			if _, _, _, err := sectionGateTx(ctx, tx.tx, j.TenantID, j.TaskID, j.SectionID); err != nil {
				return err
			}
			if err = tx.ReleaseSectionClaim(ctx, j.TenantID, j.SectionID, j.LeaseToken); err != nil {
				return err
			}
		}
		fail := func(reason string) error {
			_, err := tx.ExecContext(ctx, `UPDATE section_write_jobs SET status='blocked',error=$2,lease_until=NULL,updated_at=now() WHERE id=$1`, j.ID, reason)
			return err
		}
		if j.Attempts >= j.MaxAttempts || !j.DeadlineAt.After(time.Now()) {
			return fail("执行次数或原始截止时间已耗尽")
		}
		claim, err := tx.ClaimSectionWrite(ctx, j.TenantID, j.TaskID, j.SectionID, j.CreatedBy, SectionLeaseTTL)
		if err != nil {
			return fail(err.Error())
		}
		if claim.BaseRevNo != j.BaseRevNo || claim.BasedOnOutlineVersion != j.BasedOnOutlineVersion {
			if err = tx.ReleaseSectionClaim(ctx, j.TenantID, j.SectionID, claim.LeaseToken); err != nil {
				return err
			}
			return fail("请求所依据的正文或大纲版本已变化，请重新派发")
		}
		out, err = scanSectionJob(tx.QueryRowContext(ctx, `UPDATE section_write_jobs SET status='running',attempts=attempts+1,lease_token=$2,lease_until=LEAST(deadline_at,$3),error='',updated_at=now() WHERE id=$1 RETURNING `+sectionJobCols, j.ID, claim.LeaseToken, claim.LeaseUntil))
		return err
	})
	return out, err
}

func (d *DB) guardSectionJob(ctx context.Context, j *SectionWriteJob) error {
	paused, err := lockSectionJobTask(ctx, d, j.TaskID)
	if err != nil {
		return err
	}
	if paused {
		return ErrLeaseLost
	}
	var id int64
	err = d.QueryRowContext(ctx, `SELECT id FROM section_write_jobs WHERE tenant_id=$1 AND task_id=$2 AND id=$3 AND status='running' AND lease_token=$4 AND lease_until>now() AND deadline_at>now() FOR UPDATE`, j.TenantID, j.TaskID, j.ID, j.LeaseToken).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLeaseLost
	}
	return err
}

func (d *DB) HeartbeatSectionWriteJob(ctx context.Context, j *SectionWriteJob) error {
	return d.Transaction(ctx, func(tx *DB) error {
		if err := tx.guardSectionJob(ctx, j); err != nil {
			return err
		}
		if err := tx.RenewSectionLease(ctx, j.TenantID, j.SectionID, j.LeaseToken, SectionLeaseTTL); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE section_write_jobs SET lease_until=LEAST(deadline_at,now()+$2*interval '1 second'),updated_at=now() WHERE id=$1`, j.ID, SectionLeaseTTL.Seconds())
		return err
	})
}

// FinishSectionWriteJob commits the fenced job result and revision atomically.
func (d *DB) FinishSectionWriteJob(ctx context.Context, j *SectionWriteJob, in SaveRevisionInput) (*SaveRevisionResult, error) {
	var out *SaveRevisionResult
	err := d.Transaction(ctx, func(tx *DB) error {
		if err := tx.guardSectionJob(ctx, j); err != nil {
			return err
		}
		in.TaskID = j.TaskID
		in.SectionID = j.SectionID
		in.BaseRevNo = j.BaseRevNo
		in.BasedOnOutlineVersion = j.BasedOnOutlineVersion
		in.LeaseToken = j.LeaseToken
		var token int64
		// Serialize against direct section claims before relying on SaveSectionRevision.
		if _, _, _, err := sectionGateTx(ctx, tx.tx, j.TenantID, j.TaskID, j.SectionID); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT lease_token FROM proposal_sections WHERE id=$1`, j.SectionID).Scan(&token); err != nil {
			return ErrLeaseLost
		}
		if token != j.LeaseToken {
			return ErrLeaseLost
		}
		var err error
		out, err = tx.SaveSectionRevision(ctx, j.TenantID, "ai:"+j.CreatedBy, in)
		if err != nil {
			return err
		}
		status := "awaiting_review"
		if out.Outcome != SaveApplied {
			status = "candidate"
		}
		_, err = tx.ExecContext(ctx, `UPDATE section_write_jobs SET status=$2,result_rev_no=$3,error=$4,lease_until=NULL,updated_at=now() WHERE id=$1`, j.ID, status, out.Revision.RevNo, out.Reason)
		return err
	})
	return out, err
}

func (d *DB) FailSectionWriteJob(ctx context.Context, j *SectionWriteJob, reason string) error {
	return d.Transaction(ctx, func(tx *DB) error {
		if _, err := lockSectionJobTask(ctx, tx, j.TaskID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE section_write_jobs SET status='blocked',error=$4,lease_until=NULL,updated_at=now() WHERE id=$1 AND tenant_id=$2 AND lease_token=$3 AND status='running'`, j.ID, j.TenantID, j.LeaseToken, reason)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return ErrLeaseLost
		}
		if _, _, _, err := sectionGateTx(ctx, tx.tx, j.TenantID, j.TaskID, j.SectionID); err != nil {
			return err
		}
		return tx.ReleaseSectionClaim(ctx, j.TenantID, j.SectionID, j.LeaseToken)
	})
}

func (d *DB) CancelSectionWriteJob(ctx context.Context, tenant string, taskID, id int64, actor string) error {
	return d.Transaction(ctx, func(tx *DB) error {
		if _, err := lockSectionJobTask(ctx, tx, taskID); err != nil {
			return err
		}
		j, err := tx.GetSectionWriteJob(ctx, tenant, taskID, id)
		if err != nil {
			return err
		}
		if j.Status != "queued" && j.Status != "running" {
			return nil
		}
		if _, err = tx.ExecContext(ctx, `UPDATE section_write_jobs SET status='cancelled',lease_until=NULL,error='用户取消',updated_by=$2,updated_at=now() WHERE id=$1`, id, actor); err != nil {
			return err
		}
		if _, _, _, err := sectionGateTx(ctx, tx.tx, j.TenantID, j.TaskID, j.SectionID); err != nil {
			return err
		}
		return tx.ReleaseSectionClaim(ctx, j.TenantID, j.SectionID, j.LeaseToken)
	})
}

func (d *DB) PendingSectionWriteTasks(ctx context.Context) ([]int64, error) {
	rows, err := d.QueryContext(ctx, `SELECT DISTINCT j.task_id FROM section_write_jobs j JOIN tasks t ON t.id=j.task_id WHERE NOT t.paused AND j.status IN ('queued','running') ORDER BY j.task_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
