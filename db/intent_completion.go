package db

import (
	"context"
	"errors"
	"fmt"
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

// CompleteIntentOrContinue finalizes a turn that delivered no job events (see
// FinalizeIntentTurn). Intake either sees a running recipient and makes us
// continue, or leaves the new Hint active for the Planner; there is no
// check/update gap. A failed deliverable check blocks the intent atomically.
func (s *ExplorationStore) CompleteIntentOrContinue(ctx context.Context, id, epoch int64) error {
	outcome, detail, err := s.FinalizeIntentTurn(ctx, id, epoch, nil)
	if err == nil && outcome == TurnBlocked {
		err = errors.New(detail)
	}
	return err
}

func (s *ExplorationStore) HasPendingIntentInputs(ctx context.Context, id int64) (bool, error) {
	var pending bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM intent_inputs WHERE exploration_id=$1 AND intent_id=$2 AND state IN ('pending','delivered'))`, s.expID, id).Scan(&pending)
	return pending, err
}
