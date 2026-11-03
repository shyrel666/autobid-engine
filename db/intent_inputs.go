package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// AddRoutedHint records the Hint and all recipients atomically. An in-memory
// notification is only an accelerator; it never acknowledges delivery.
func (s *ExplorationStore) AddRoutedHint(ctx context.Context, text string, anchors []int64) (int64, []int64, error) {
	var id int64
	var recipients []int64
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = s.insertNodeTx(ctx, tx, KindHint, StateHintActive, map[string]any{"summary": text}, 0, "", anchors)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT n.id FROM exploration_nodes n WHERE n.exploration_id=$1 AND n.kind='intent' AND n.state='running' AND EXISTS(SELECT 1 FROM exploration_anchors a WHERE a.node_id=n.id AND a.asset_id=ANY($2::bigint[])) ORDER BY n.id FOR UPDATE OF n`, s.expID, anchors)
		if err != nil {
			return err
		}
		for rows.Next() {
			var iid int64
			if err = rows.Scan(&iid); err != nil {
				rows.Close()
				return err
			}
			recipients = append(recipients, iid)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, iid := range recipients {
			if _, err = tx.ExecContext(ctx, `INSERT INTO intent_inputs(exploration_id,hint_id,intent_id,body,created_by,updated_by) VALUES($1,$2,$3,$4,'user','user') ON CONFLICT(tenant_id,hint_id,intent_id) DO NOTHING`, s.expID, id, iid, text); err != nil {
				return err
			}
		}
		return nil
	})
	return id, recipients, err
}

// TakeIntentInput does not consume the Hint. The same delivery is replayed after
// lease takeover; within one attempt it is injected only once.
func (s *ExplorationStore) TakeIntentInput(ctx context.Context, intentID, epoch int64) (string, bool, error) {
	var id, hint int64
	var body string
	err := s.db.QueryRowContext(ctx, `UPDATE intent_inputs i SET state='delivered',delivery_epoch=$3,updated_at=now() WHERE i.id=(SELECT x.id FROM intent_inputs x JOIN exploration_nodes n ON n.id=x.intent_id WHERE x.exploration_id=$1 AND x.intent_id=$2 AND (x.state='pending' OR (x.state='delivered' AND x.delivery_epoch<>$3)) AND n.state='running' AND n.lease_epoch=$3 AND n.lease_until>now() ORDER BY x.id LIMIT 1 FOR UPDATE OF x SKIP LOCKED) RETURNING i.id,i.hint_id,i.body`, s.expID, intentID, epoch).Scan(&id, &hint, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return fmt.Sprintf("[持久化人工输入 #%d / Hint #%d] %s", id, hint, body), true, nil
}

// AcknowledgeIntentInputs is called only after the worker transcript is saved
// and its run completed successfully. Delivery is at-least-once, never exactly-once.
func (s *ExplorationStore) AcknowledgeIntentInputs(ctx context.Context, intentID, epoch int64) error {
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		var id int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM exploration_nodes WHERE id=$1 AND exploration_id=$2 AND state='running' AND lease_epoch=$3 AND lease_until>now() FOR UPDATE`, intentID, s.expID, epoch).Scan(&id); err != nil {
			return ErrLeaseLost
		}
		// Multiple recipients may finish together. Serialize on their shared
		// hints before checking the aggregate acknowledgement state.
		rows, err := tx.QueryContext(ctx, `SELECT h.id FROM exploration_nodes h WHERE h.exploration_id=$1 AND h.kind='hint'
		 AND EXISTS(SELECT 1 FROM intent_inputs i WHERE i.hint_id=h.id AND i.intent_id=$2 AND i.delivery_epoch=$3 AND i.state='delivered') ORDER BY h.id FOR UPDATE`, s.expID, intentID, epoch)
		if err != nil {
			return err
		}
		for rows.Next() {
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE intent_inputs SET state='acknowledged',updated_at=now() WHERE exploration_id=$1 AND intent_id=$2 AND delivery_epoch=$3 AND state='delivered'`, s.expID, intentID, epoch); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE exploration_nodes h SET state='consumed',content_version=content_version+1 WHERE h.exploration_id=$1 AND h.kind='hint' AND h.state='active' AND EXISTS(SELECT 1 FROM intent_inputs i WHERE i.hint_id=h.id AND i.intent_id=$2 AND i.delivery_epoch=$3) AND NOT EXISTS(SELECT 1 FROM intent_inputs i WHERE i.hint_id=h.id AND i.state<>'acknowledged')`, s.expID, intentID, epoch)
		return err
	})
}

func (s *ExplorationStore) ReturnIntentInputs(ctx context.Context, intentID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE intent_inputs SET state='returned',updated_at=now() WHERE exploration_id=$1 AND intent_id=$2 AND state IN ('pending','delivered')`, s.expID, intentID)
	return err
}
