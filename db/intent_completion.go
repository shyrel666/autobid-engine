package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
)

// CheckIntentDeliverables checks authoritative section revisions, not tool names
// or model-written completion messages. Human approval remains a separate state.
func (d *DB) CheckIntentDeliverables(ctx context.Context, taskID int64, n *Node) error {
	m := NodeMandate(n)
	if m == nil {
		return nil
	}
	for _, id := range m.RequiredSectionIDs {
		var valid bool
		err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM proposal_sections s JOIN proposal_outlines o ON o.id=s.outline_id JOIN section_revisions r ON r.section_id=s.id AND r.rev_no=s.current_rev_no
		 JOIN intent_section_outputs p ON p.tenant_id=s.tenant_id AND p.section_rev_id=r.id AND p.intent_id=$4
		 WHERE s.tenant_id=$1 AND s.task_id=$2 AND s.id=$3 AND o.status='approved' AND s.status IN ('draft','reviewing','approved') AND r.status IN ('draft','reviewing','approved') AND jsonb_array_length(r.gaps)=0
		 AND NOT EXISTS(SELECT 1 FROM section_writing_verifications v WHERE v.section_rev_id=r.id AND v.verdict<>'passed'))`, DefaultTenant, taskID, id, n.ID).Scan(&valid)
		if err != nil {
			return err
		}
		if !valid {
			return fmt.Errorf("required section %d has no new valid revision for this intent", id)
		}
	}
	return nil
}

// CompleteIntentOrContinue serializes the last pending-input check with intake.
// Intake either sees a running recipient and makes us continue, or leaves the
// new Hint active for the Planner; there is no check/update gap.
func (s *ExplorationStore) CompleteIntentOrContinue(ctx context.Context, id, epoch int64) error {
	return s.db.Transaction(ctx, func(tx *DB) error {
		var n Node
		var taskID int64
		err := tx.QueryRowContext(ctx, `SELECT n.id,n.payload,t.id FROM exploration_nodes n JOIN tasks t ON t.exploration_id=n.exploration_id
		 WHERE n.id=$1 AND n.exploration_id=$2 AND n.state='running' AND n.lease_epoch=$3 AND n.lease_until>now() FOR UPDATE OF n`, id, s.expID, epoch).Scan(&n.ID, &n.Payload, &taskID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLeaseLost
		}
		if err != nil {
			return err
		}
		// Recheck at commit, locking each required section in the standard order.
		if m := NodeMandate(&n); m != nil {
			ids := uniqueIDs(m.RequiredSectionIDs)
			slices.Sort(ids)
			for _, sid := range ids {
				if _, _, _, err := sectionGateTx(ctx, tx.tx, DefaultTenant, taskID, sid); err != nil {
					return err
				}
			}
		}
		if err := tx.CheckIntentDeliverables(ctx, taskID, &n); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE exploration_nodes n SET state=CASE WHEN EXISTS(SELECT 1 FROM intent_inputs i WHERE i.intent_id=n.id AND i.state IN ('pending','delivered')) THEN 'open' ELSE 'done' END,lease_until=NULL,content_version=content_version+1,completed_at=CASE WHEN EXISTS(SELECT 1 FROM intent_inputs i WHERE i.intent_id=n.id AND i.state IN ('pending','delivered')) THEN NULL ELSE now() END WHERE id=$1`, id)
		return err
	})
}

func (s *ExplorationStore) HasPendingIntentInputs(ctx context.Context, id int64) (bool, error) {
	var pending bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM intent_inputs WHERE exploration_id=$1 AND intent_id=$2 AND state IN ('pending','delivered'))`, s.expID, id).Scan(&pending)
	return pending, err
}
