package db

import (
	"context"
	"database/sql"
	"errors"
)

type intentExecutionKey struct{}
type intentExecution struct{ ID, Epoch int64 }

// WithIntentExecution is set by the executor, never by a tool argument.
func WithIntentExecution(ctx context.Context, id, epoch int64) context.Context {
	return context.WithValue(ctx, intentExecutionKey{}, intentExecution{id, epoch})
}

// Lock the producer before the outline/section, so a lease takeover cannot race
// a revision write. Outputs and their producer attribution commit together.
func lockIntentProducerTx(ctx context.Context, tx *sql.Tx, taskID int64) error {
	p, ok := ctx.Value(intentExecutionKey{}).(intentExecution)
	if !ok {
		return nil
	}
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT n.id FROM exploration_nodes n JOIN tasks t ON t.exploration_id=n.exploration_id
		WHERE n.id=$1 AND n.lease_epoch=$2 AND n.state='running' AND n.lease_until>now() AND t.id=$3 AND NOT t.paused FOR UPDATE OF n`, p.ID, p.Epoch, taskID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLeaseLost
	}
	return err
}

func recordIntentOutputTx(ctx context.Context, tx *sql.Tx, tenant string, rev *SectionRevision, actor string) error {
	p, ok := ctx.Value(intentExecutionKey{}).(intentExecution)
	if !ok {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO intent_section_outputs(tenant_id,intent_id,lease_epoch,section_rev_id,created_by,updated_by)
		VALUES($1,$2,$3,$4,$5,$5)`, tenant, p.ID, p.Epoch, rev.ID, actor)
	return err
}
