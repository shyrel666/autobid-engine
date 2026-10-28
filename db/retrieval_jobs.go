package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type RetrievalJob struct {
	ID         int64           `json:"id"`
	RunID      int64           `json:"run_id"`
	Ordinal    int             `json:"ordinal"`
	Spec       json.RawMessage `json:"spec"`
	Status     string          `json:"status"`
	LeaseToken int64           `json:"lease_token"`
	Attempts   int             `json:"attempts"`
	Result     json.RawMessage `json:"result"`
}

func (d *DB) EnsureRetrievalJobs(ctx context.Context, tenant string, runID int64, specs []json.RawMessage) error {
	return d.InTx(ctx, func(tx *sql.Tx) error {
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM retrieval_runs WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenantOr(tenant), runID).Scan(&status); err != nil {
			return err
		}
		if status != "running" {
			return ErrRetrievalConflict
		}
		for i, spec := range specs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO retrieval_jobs(tenant_id,run_id,ordinal,spec) VALUES($1,$2,$3,$4::jsonb) ON CONFLICT DO NOTHING`, tenantOr(tenant), runID, i, spec); err != nil {
				return err
			}
		}
		return nil
	})
}
func (d *DB) RetrievalJobs(ctx context.Context, tenant string, runID int64) ([]RetrievalJob, error) {
	rows, err := d.QueryContext(ctx, `SELECT id,run_id,ordinal,spec,status,lease_token,attempts,result FROM retrieval_jobs WHERE tenant_id=$1 AND run_id=$2 ORDER BY ordinal`, tenantOr(tenant), runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RetrievalJob{}
	for rows.Next() {
		var j RetrievalJob
		if err := rows.Scan(&j.ID, &j.RunID, &j.Ordinal, &j.Spec, &j.Status, &j.LeaseToken, &j.Attempts, &j.Result); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
func (d *DB) ClaimRetrievalJob(ctx context.Context, tenant string, jobID int64, lease time.Duration) (int64, error) {
	var token int64
	err := d.QueryRowContext(ctx, `UPDATE retrieval_jobs j SET status='running',lease_token=lease_token+1,attempts=attempts+1,lease_until=now()+$3::double precision*interval '1 second'
 WHERE j.tenant_id=$1 AND j.id=$2 AND attempts<2 AND (j.status='pending' OR (j.status='running' AND lease_until<now()))
 AND EXISTS(SELECT 1 FROM retrieval_runs r WHERE r.tenant_id=j.tenant_id AND r.id=j.run_id AND r.status='running' AND r.deadline>now()) RETURNING lease_token`, tenantOr(tenant), jobID, lease.Seconds()).Scan(&token)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrLeaseLost
	}
	return token, err
}
func (d *DB) FinishRetrievalJob(ctx context.Context, tenant string, id, token int64, status string, result any) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	res, err := d.ExecContext(ctx, `UPDATE retrieval_jobs SET status=$4,result=$5::jsonb,lease_until=NULL WHERE tenant_id=$1 AND id=$2 AND lease_token=$3 AND status='running' AND lease_until>now()`, tenantOr(tenant), id, token, status, raw)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (d *DB) RenewRetrievalJob(ctx context.Context, tenant string, id, token int64, lease time.Duration) error {
	res, err := d.ExecContext(ctx, `UPDATE retrieval_jobs j SET lease_until=now()+$4::double precision*interval '1 second' WHERE tenant_id=$1 AND id=$2 AND lease_token=$3 AND status='running' AND lease_until>now() AND EXISTS(SELECT 1 FROM retrieval_runs r WHERE r.id=j.run_id AND r.tenant_id=j.tenant_id AND r.status='running' AND r.deadline>now())`, tenantOr(tenant), id, token, lease.Seconds())
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrLeaseLost
	}
	return nil
}

// Expired jobs are terminalized before aggregation; active unexpired jobs block it.
func (d *DB) ExpireRetrievalJobs(ctx context.Context, tenant string, runID int64) error {
	_, err := d.ExecContext(ctx, `UPDATE retrieval_jobs j SET status='budget_exhausted',result='{"gap":"deadline or retry budget exhausted"}' WHERE j.tenant_id=$1 AND j.run_id=$2 AND j.status IN ('pending','running') AND
 (EXISTS(SELECT 1 FROM retrieval_runs r WHERE r.id=j.run_id AND r.tenant_id=j.tenant_id AND r.deadline<=now()) OR (j.attempts>=2 AND j.lease_until<now()))`, tenantOr(tenant), runID)
	return err
}

func (d *DB) CancelRetrievalRun(ctx context.Context, tenant string, runID int64, actor string) error {
	return d.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE retrieval_runs SET status='cancelled',finished_at=now(),updated_by=$3 WHERE tenant_id=$1 AND id=$2 AND status='running'`, tenantOr(tenant), runID, actor)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return ErrRetrievalConflict
		}
		_, err = tx.ExecContext(ctx, `UPDATE retrieval_jobs SET status='cancelled',lease_token=lease_token+1,lease_until=NULL,updated_by=$3 WHERE tenant_id=$1 AND run_id=$2 AND status IN ('running','pending')`, tenantOr(tenant), runID, actor)
		return err
	})
}
