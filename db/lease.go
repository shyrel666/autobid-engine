package db

// Patch ④：Intent 租约抢占、周期续租、Fencing Token 受保护写回与过期回收。
// 取代原仓“条件 UPDATE + 启动时批量 running→open”的单实例假设。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ClaimIntentWithLease 以条件 UPDATE 争抢 open Intent，成功时返回新的 lease_epoch（Fencing Token）。
func (s *ExplorationStore) ClaimIntentWithLease(ctx context.Context, id int64, owner string, ttl time.Duration) (epoch int64, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `
		UPDATE exploration_nodes
		SET state = 'running',
		    owner = $1,
		    lease_until = now() + ($4::bigint * interval '1 millisecond'),
		    lease_epoch = lease_epoch + 1,
		    updated_at = now()
		WHERE id = $2 AND exploration_id = $3 AND kind = 'intent' AND state = 'open'
		RETURNING lease_epoch`,
		owner, id, s.expID, ttl.Milliseconds(),
	).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return epoch, true, nil
}

// ResumePausedIntentWithLease 是 paused→running（人工消息直投）的租约版本，同样递增 epoch。
func (s *ExplorationStore) ResumePausedIntentWithLease(ctx context.Context, id int64, owner string, ttl time.Duration) (epoch int64, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `
		UPDATE exploration_nodes
		SET state = 'running',
		    owner = $1,
		    lease_until = now() + ($4::bigint * interval '1 millisecond'),
		    lease_epoch = lease_epoch + 1,
		    completed_at = NULL,
		    content_version = content_version + 1
		WHERE id = $2 AND exploration_id = $3 AND kind = 'intent' AND state = 'paused'
		RETURNING lease_epoch`,
		owner, id, s.expID, ttl.Milliseconds(),
	).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return epoch, true, nil
}

// RenewIntentLease：Worker 执行期间每 ttl/3 调一次，若返回 false 说明租约已丢失，立即 cancel 本次 workCtx。
func (s *ExplorationStore) RenewIntentLease(ctx context.Context, id int64, owner string, epoch int64, ttl time.Duration) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE exploration_nodes
		SET lease_until = now() + ($5::bigint * interval '1 millisecond'),
		    updated_at = now()
		WHERE id = $1 AND exploration_id = $2 AND kind = 'intent'
		  AND state = 'running' AND owner = $3 AND lease_epoch = $4`,
		id, s.expID, owner, epoch, ttl.Milliseconds(),
	)
	return affectedOne(res, err)
}

// CompareAndSetIntentStateFenced：携带 lease_epoch 校验，阻止失联旧 Worker 脑裂覆写终态。
func (s *ExplorationStore) CompareAndSetIntentStateFenced(ctx context.Context, id int64, expected, state string, epoch int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE exploration_nodes
		SET state = $1,
		    lease_until = NULL,
		    blocked_reason = NULL,
		    content_version = content_version + 1,
		    completed_at = CASE WHEN $5::boolean THEN now() ELSE NULL END
		WHERE id = $2 AND exploration_id = $3 AND kind = 'intent'
		  AND state = $4 AND lease_epoch = $6`,
		state, id, s.expID, expected, IsTerminalIntentState(state), epoch,
	)
	return affectedOne(res, err)
}

// BlockIntentFenced 原子地将 running Intent 置为 blocked 并记录原因（如配额链耗尽）。
// 补齐原仓 SetIntentBlockedReason 无 running CAS 的缺口（§2.7）。
func (s *ExplorationStore) BlockIntentFenced(ctx context.Context, id int64, epoch int64, reason string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE exploration_nodes
		SET state = 'blocked',
		    blocked_reason = NULLIF($4, ''),
		    lease_until = NULL,
		    content_version = content_version + 1,
		    completed_at = now()
		WHERE id = $1 AND exploration_id = $2 AND kind = 'intent'
		  AND state = 'running' AND lease_epoch = $3`,
		id, s.expID, epoch, reason,
	)
	return affectedOne(res, err)
}

// ReapExpiredRunningIntents：后台定时回收本空间租约过期的僵尸 running 意图回 open。
func (s *ExplorationStore) ReapExpiredRunningIntents(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE exploration_nodes
		SET state = 'open',
		    owner = NULL,
		    lease_until = NULL,
		    completed_at = NULL,
		    content_version = content_version + 1
		WHERE exploration_id = $1 AND kind = 'intent'
		  AND state = 'running' AND lease_until IS NOT NULL AND lease_until < now()`,
		s.expID,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ReapExpiredIntents 跨全部推演空间回收过期租约，返回受影响的 exploration ID（去重），
// 供调用方逐一唤醒对应 Planner。替代原仓启动时的 ResetRunningIntents 批量重置。
func (d *DB) ReapExpiredIntents(ctx context.Context) ([]int64, error) {
	rows, err := d.QueryContext(ctx, `
		WITH reaped AS (
			UPDATE exploration_nodes
			SET state = 'open',
			    owner = NULL,
			    lease_until = NULL,
			    completed_at = NULL,
			    content_version = content_version + 1
			WHERE kind = 'intent' AND state = 'running'
			  AND lease_until IS NOT NULL AND lease_until < now()
			RETURNING exploration_id
		)
		SELECT DISTINCT exploration_id FROM reaped ORDER BY exploration_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// KeepIntentLease 在 ctx 存活期间每 ttl/3 续租一次，直到 ctx 结束返回。
// 租约被确认丢失（已被回收或抢占），或续租持续失败直到本地估算的租约到期时，调用一次 onLost 后返回；
// 调用方应在 onLost 中以租约丢失为 cause 取消 workCtx。
func (s *ExplorationStore) KeepIntentLease(ctx context.Context, id int64, owner string, epoch int64, ttl time.Duration, onLost func(error)) {
	interval := ttl / 3
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	validUntil := time.Now().Add(ttl)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		start := time.Now()
		rctx, cancel := context.WithTimeout(ctx, interval)
		ok, err := s.RenewIntentLease(rctx, id, owner, epoch, ttl)
		cancel()
		switch {
		case err == nil && ok:
			validUntil = start.Add(ttl)
		case err == nil:
			onLost(fmt.Errorf("%w: intent %d epoch %d", ErrLeaseLost, id, epoch))
			return
		case ctx.Err() != nil:
			return
		case time.Now().After(validUntil):
			onLost(fmt.Errorf("%w: intent %d epoch %d: %v", ErrLeaseLost, id, epoch, err))
			return
		}
	}
}
