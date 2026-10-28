package db

// Patch ⑤：节点、锚点、血缘边与目标状态翻转收敛进单个 PostgreSQL 事务。
// 唤醒 Planner 的通知仍是提交后的独立步骤（非 outbox），由调用方在成功返回后发出。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// RecordFactWithYieldsTx：单事务完成「校验父 Intent -> 插入 Fact 节点 -> 绑定静态实体锚点 -> 写入 yields 边」。
// intentID<=0 时允许孤立事实（与原仓 record_fact 语义一致）。
func (s *ExplorationStore) RecordFactWithYieldsTx(ctx context.Context, intentID int64, payload any, priority int, worker string, anchors []int64) (factID int64, err error) {
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if intentID > 0 {
			kind, err := s.nodeKindTx(ctx, tx, intentID, false)
			if err != nil || kind != KindIntent {
				return fmt.Errorf("invalid parent intent %d in exploration %d", intentID, s.expID)
			}
		}
		if factID, err = s.insertNodeTx(ctx, tx, KindFact, StateConfirmed, payload, priority, worker, anchors); err != nil {
			return err
		}
		if intentID > 0 {
			return s.linkTx(ctx, tx, intentID, RelYields, factID)
		}
		return nil
	})
	return factID, err
}

// ProveGoalTx：单事务完成「锁 Goal 行 -> 校验证据 -> 写 proves 边 -> 置 Goal=met 并递增 content_version -> 精确统计剩余未达成 Goal 数」。
// 证据须为本空间或直接来源任务的 Fact/Finding（由 linkTx 的 kind 矩阵校验）；abandoned 目标不可被证明。
func (s *ExplorationStore) ProveGoalTx(ctx context.Context, goalID, evidenceID int64) (allMet bool, totalGoals int, err error) {
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		var goalKind, goalState string
		if err := tx.QueryRowContext(ctx, `
			SELECT kind, state FROM exploration_nodes
			WHERE id = $1 AND exploration_id = $2 FOR UPDATE`, goalID, s.expID).Scan(&goalKind, &goalState); err != nil || goalKind != KindGoal {
			return fmt.Errorf("goal %d not found in exploration %d", goalID, s.expID)
		}
		if goalState == StateGoalAbandoned {
			return fmt.Errorf("goal %d is abandoned", goalID)
		}
		if err := s.linkTx(ctx, tx, evidenceID, RelProves, goalID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE exploration_nodes
			SET state = 'met',
			    blocked_reason = NULL,
			    content_version = content_version + 1,
			    completed_at = COALESCE(completed_at, now()),
			    updated_at = now()
			WHERE id = $1 AND exploration_id = $2 AND kind = 'goal'`, goalID, s.expID); err != nil {
			return err
		}
		var unmet int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*), COUNT(*) FILTER (WHERE state <> 'met')
			FROM exploration_nodes
			WHERE exploration_id = $1 AND kind = 'goal' AND state <> 'abandoned'`, s.expID).Scan(&totalGoals, &unmet); err != nil {
			return err
		}
		allMet = totalGoals > 0 && unmet == 0
		return nil
	})
	if err != nil {
		return false, 0, err
	}
	return allMet, totalGoals, nil
}

type AddIntentInput struct {
	Payload  any
	Priority int     // 0 取 DefaultIntentPriority
	Parents  []int64 // 父 Fact/Finding，可来自直接来源任务；为空时回落 origin Fact
	Anchors  []int64
}

// AddIntentTx：单事务完成「插入 open Intent -> 锚点 -> 全部 derived_from 父边」；任一父节点非法则整体回滚。
func (s *ExplorationStore) AddIntentTx(ctx context.Context, in AddIntentInput) (intentID int64, err error) {
	priority := in.Priority
	if priority == 0 {
		priority = DefaultIntentPriority
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		parents := in.Parents
		if len(parents) == 0 {
			origin, err := originFactID(ctx, tx, s.expID)
			if err != nil {
				return err
			}
			if origin > 0 {
				parents = []int64{origin}
			}
		}
		if intentID, err = s.insertNodeTx(ctx, tx, KindIntent, StateIntentOpen, in.Payload, priority, "", in.Anchors); err != nil {
			return err
		}
		for _, p := range parents {
			if err := s.linkTx(ctx, tx, p, RelDerivedFrom, intentID); err != nil {
				return err
			}
		}
		return nil
	})
	return intentID, err
}

// AddGoalTx：单事务插入 open Goal 并从 origin Fact 写 spawns 边（无根时不写边）。
func (s *ExplorationStore) AddGoalTx(ctx context.Context, payload any, anchors []int64) (goalID int64, err error) {
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if goalID, err = s.insertNodeTx(ctx, tx, KindGoal, StateGoalOpen, payload, 0, "", anchors); err != nil {
			return err
		}
		origin, err := originFactID(ctx, tx, s.expID)
		if err != nil || origin == 0 {
			return err
		}
		return s.linkTx(ctx, tx, origin, RelSpawns, goalID)
	})
	return goalID, err
}

type RecordFindingInput struct {
	TaskID   int64
	IntentID int64 // >0 时写 intent -yields-> finding
	Category string
	Severity string
	Title    string
	Summary  string
	Detail   any     // 结构化正文，存入 findings.payload 与节点 payload.detail
	Evidence []int64 // 支撑该结论的 Fact/Finding，可来自直接来源任务
	Anchors  []int64
	Worker   string
}

type RecordedFinding struct {
	FindingID int64
	NodeID    int64
}

// RecordFinding：单事务完成「校验任务归属与 Intent -> finding 节点+锚点 -> yields -> findings 记录 -> 证据关联」。
func (s *ExplorationStore) RecordFinding(ctx context.Context, in RecordFindingInput) (*RecordedFinding, error) {
	if strings.TrimSpace(in.Title) == "" {
		return nil, fmt.Errorf("finding title is required")
	}
	if in.Severity == "" {
		in.Severity = SeverityInfo
	}
	detail, err := marshalPayload(in.Detail)
	if err != nil {
		return nil, err
	}
	nodePayload := map[string]any{
		"title":    in.Title,
		"summary":  in.Summary,
		"category": in.Category,
		"severity": in.Severity,
		"detail":   jsonRaw(detail),
	}
	out := &RecordedFinding{}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		var expID int64
		if err := tx.QueryRowContext(ctx, `SELECT exploration_id FROM tasks WHERE id = $1`, in.TaskID).Scan(&expID); err != nil || expID != s.expID {
			return fmt.Errorf("task %d does not own exploration %d", in.TaskID, s.expID)
		}
		if in.IntentID > 0 {
			kind, err := s.nodeKindTx(ctx, tx, in.IntentID, false)
			if err != nil || kind != KindIntent {
				return fmt.Errorf("invalid parent intent %d in exploration %d", in.IntentID, s.expID)
			}
		}
		nodeID, err := s.insertNodeTx(ctx, tx, KindFinding, StateConfirmed, nodePayload, 0, in.Worker, in.Anchors)
		if err != nil {
			return err
		}
		if in.IntentID > 0 {
			if err := s.linkTx(ctx, tx, in.IntentID, RelYields, nodeID); err != nil {
				return err
			}
		}
		var findingID int64
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO findings(task_id, exploration_id, node_id, intent_id, category, severity, title, summary, payload)
			VALUES ($1, $2, $3, NULLIF($4::bigint, 0), $5, $6, $7, $8, $9::jsonb)
			RETURNING id`,
			in.TaskID, s.expID, nodeID, in.IntentID, in.Category, in.Severity, in.Title, in.Summary, string(detail),
		).Scan(&findingID); err != nil {
			return err
		}
		for _, ev := range in.Evidence {
			kind, err := s.nodeKindTx(ctx, tx, ev, true)
			if err != nil {
				return err
			}
			if kind != KindFact && kind != KindFinding {
				return fmt.Errorf("%w: evidence %d is %s", ErrInvalidEdge, ev, kind)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO finding_evidence(finding_id, node_id) VALUES ($1, $2)
				ON CONFLICT DO NOTHING`, findingID, ev); err != nil {
				return err
			}
		}
		out.FindingID, out.NodeID = findingID, nodeID
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

type jsonRaw []byte

func (r jsonRaw) MarshalJSON() ([]byte, error) { return r, nil }
