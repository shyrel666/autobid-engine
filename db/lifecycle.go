package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/autobid/autobid-engine/internal/llm"
)

// MinPlanHeartbeatSeconds 是执行层生效的 Planner 心跳下限/默认值（§2.3）。
const MinPlanHeartbeatSeconds = 600

// IsTerminalTaskStatus 报告任务是否处于终态。
func IsTerminalTaskStatus(status string) bool {
	return status == TaskDone || status == TaskFailed || status == TaskTimeout
}

// SetTaskStatus 无条件写任务状态（管理/目标达成路径）。
func (d *DB) SetTaskStatus(ctx context.Context, id int64, status string) error {
	res, err := d.ExecContext(ctx, `UPDATE tasks SET status = $1 WHERE id = $2`, status, id)
	ok, err := affectedOne(res, err)
	if err == nil && !ok {
		err = fmt.Errorf("%w: task %d", ErrNotFound, id)
	}
	return err
}

// SetTaskStatusGuarded 仅在任务尚未终态时写入，先达终态者获胜。
func (d *DB) SetTaskStatusGuarded(ctx context.Context, id int64, status string) (bool, error) {
	res, err := d.ExecContext(ctx, `UPDATE tasks SET status = $1
		WHERE id = $2 AND status NOT IN ('done','failed','timeout')`, status, id)
	return affectedOne(res, err)
}

// SetTaskPaused 写暂停标记，并在 running/paused 之间同步状态（终态不受影响）。
func (d *DB) SetTaskPaused(ctx context.Context, id int64, paused bool) error {
	return d.Transaction(ctx, func(tx *DB) error {
		_, err := tx.ExecContext(ctx, `UPDATE tasks
		SET paused = $1,
		    status = CASE
		        WHEN $1 AND status IN ('created','running') THEN 'paused'
		        WHEN NOT $1 AND status = 'paused' THEN 'running'
		        ELSE status END
		WHERE id = $2`, paused, id)
		if err == nil && paused {
			_, err = tx.ExecContext(ctx, `UPDATE fixed_file_jobs SET status='cancelled',error='项目已暂停',error_code='CANCELLED',lease_token=lease_token+1,lease_until=NULL,retry_after=NULL,updated_by='task-pause',updated_at=now() WHERE task_id=$1 AND status IN ('queued','running')`, id)
		}
		if err == nil && paused {
			// Persist a pause fence even if resume happens before the next heartbeat.
			// Keep the token for release on reclaim; status prevents the old attempt
			// from writing and the original deadline/attempt budget is unchanged.
			_, err = tx.ExecContext(ctx, `UPDATE section_write_jobs SET status='queued',lease_until=NULL,error='项目已暂停，等待恢复',updated_by='task-pause',updated_at=now() WHERE task_id=$1 AND status='running'`, id)
		}
		if err == nil && paused {
			// S3: revoke in-flight chart renders. The token bump rejects the old
			// attempt's write-back; attempts and the original deadline are kept.
			_, err = tx.ExecContext(ctx, `UPDATE chart_render_jobs SET status='queued',lease_token=lease_token+1,lease_until=NULL,lease_owner='',error='项目已暂停，等待恢复',updated_by='task-pause' WHERE task_id=$1 AND status='running'`, id)
		}
		if err == nil && paused {
			// S4: revoke in-flight exports the same way; attempts, the deadline fixed at
			// submission and the snapshot are kept, the token bump rejects the old write-back.
			_, err = tx.ExecContext(ctx, `UPDATE export_jobs SET status='queued',lease_token=lease_token+1,lease_until=NULL,lease_owner='',error='项目已暂停，等待恢复',updated_by='task-pause' WHERE task_id=$1 AND status='running'`, id)
		}
		return err
	})
}

// ApplyTaskAdmission 将任务从 expected 迁到 running 并清暂停标记（创建、恢复、终态复活）。
// 从终态复活时同时清空首跑与截止时间，使任务时钟重新起算。
func (d *DB) ApplyTaskAdmission(ctx context.Context, id int64, expected string) (bool, error) {
	res, err := d.ExecContext(ctx, `UPDATE tasks
		SET status = 'running', paused = false, queued = false,
		    first_run_at = CASE WHEN status IN ('done','failed','timeout') THEN NULL ELSE first_run_at END,
		    deadline_at  = CASE WHEN status IN ('done','failed','timeout') THEN NULL ELSE deadline_at END
		WHERE id = $1 AND status = $2`, id, expected)
	return affectedOne(res, err)
}

// StampFirstRun 在首次真实执行时盖章 first_run_at，并按 timeout_seconds 固化 deadline_at（幂等）。
func (d *DB) StampFirstRun(ctx context.Context, id int64) (firstRun, deadline *time.Time, err error) {
	var fr, dl sql.NullTime
	err = d.QueryRowContext(ctx, `UPDATE tasks
		SET first_run_at = COALESCE(first_run_at, now()),
		    deadline_at = CASE
		        WHEN deadline_at IS NULL AND timeout_seconds > 0
		        THEN COALESCE(first_run_at, now()) + timeout_seconds * interval '1 second'
		        ELSE deadline_at END
		WHERE id = $1
		RETURNING first_run_at, deadline_at`, id).Scan(&fr, &dl)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf("%w: task %d", ErrNotFound, id)
	}
	return nullTimePtr(fr), nullTimePtr(dl), err
}

// GetTaskByExploration 按推演空间反查任务。
func (d *DB) GetTaskByExploration(ctx context.Context, expID int64) (*Task, error) {
	t, err := scanTask(d.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE exploration_id = $1`, expID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: task for exploration %d", ErrNotFound, expID)
	}
	return t, err
}

// HasOpenGoal 报告是否存在 open Goal。
func (s *ExplorationStore) HasOpenGoal(ctx context.Context) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM exploration_nodes
		WHERE exploration_id = $1 AND kind = 'goal' AND state = 'open')`, s.expID).Scan(&ok)
	return ok, err
}

// HasActiveIntent 报告是否存在 open/running Intent，或等待后台作业（blocked + waiting_job）的 Intent（不含 paused）。
// 等待中的 Intent 仍在进行，任务不能因“无在途意图”被判完成。
func (s *ExplorationStore) HasActiveIntent(ctx context.Context) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM exploration_nodes
		WHERE exploration_id = $1 AND kind = 'intent'
		  AND (state IN ('open','running') OR (state = 'blocked' AND blocked_reason = 'waiting_job')))`, s.expID).Scan(&ok)
	return ok, err
}

// GoalStats 返回非 abandoned 目标总数与其中 met 的数量。
func (s *ExplorationStore) GoalStats(ctx context.Context) (total, met int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE state = 'met')
		FROM exploration_nodes WHERE exploration_id = $1 AND kind = 'goal' AND state <> 'abandoned'`,
		s.expID).Scan(&total, &met)
	return total, met, err
}

// RecentNodes 按 id 倒序读取本空间节点（最近优先）。
func (s *ExplorationStore) RecentNodes(ctx context.Context, kind string, states []string, limit int) ([]*Node, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+nodeCols+` FROM exploration_nodes
		WHERE exploration_id = $1 AND kind = $2
		  AND (COALESCE(cardinality($3::text[]), 0) = 0 OR state = ANY($3::text[]))
		ORDER BY id DESC LIMIT $4`, s.expID, kind, states, limit)
	if err != nil {
		return nil, err
	}
	return scanNodes(rows)
}

// NodeEdges 返回与节点相连的本空间边（入边与出边）。
func (s *ExplorationStore) NodeEdges(ctx context.Context, id int64, limit int) ([]Edge, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT src_id, rel, dst_id FROM exploration_edges
		WHERE exploration_id = $1 AND (src_id = $2 OR dst_id = $2)
		ORDER BY created_at, src_id, dst_id LIMIT $3`, s.expID, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Edge
	for rows.Next() {
		var e Edge
		if err := rows.Scan(&e.From, &e.Rel, &e.To); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// YieldedNodes 返回 Intent 经 yields 产出的 Fact/Finding。
func (s *ExplorationStore) YieldedNodes(ctx context.Context, intentID int64, limit int) ([]*Node, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+prefixedNodeCols+` FROM exploration_edges e
		JOIN exploration_nodes n ON n.id = e.dst_id
		WHERE e.exploration_id = $1 AND e.src_id = $2 AND e.rel = 'yields'
		ORDER BY n.id LIMIT $3`, s.expID, intentID, limit)
	if err != nil {
		return nil, err
	}
	return scanNodes(rows)
}

const prefixedNodeCols = `n.id, n.exploration_id, n.kind, n.payload, n.priority, n.state,
	COALESCE(n.origin,''), COALESCE(n.owner,''), COALESCE(n.blocked_reason,''), COALESCE(n.delete_reason,''),
	n.content_version, n.active_round, n.cold_since_round, n.lease_until, n.lease_epoch, n.created_at, n.updated_at, n.completed_at`

// Activity 是推送给观察端的运行动态（Planner 轮次、工具调用、文本总结等），不持久化。
type Activity struct {
	TaskID  int64     `json:"task_id"`
	Worker  string    `json:"worker"`
	Kind    string    `json:"kind"` // round / text / tool / tool_result / state / error
	Summary string    `json:"summary"`
	Detail  string    `json:"detail,omitempty"`
	NodeID  *int64    `json:"node_id,omitempty"`
	At      time.Time `json:"at"`
}

// TranscriptStore 以 session_id 持久化 Agent 会话（实现 agentcore.TranscriptStore）。
type TranscriptStore struct {
	db *DB
}

func NewTranscriptStore(d *DB) *TranscriptStore { return &TranscriptStore{db: d} }

func (s *TranscriptStore) Load(ctx context.Context, sessionID string) ([]llm.Message, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT messages FROM transcripts WHERE session_id = $1`, sessionID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var msgs []llm.Message
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return nil, fmt.Errorf("decode transcript %s: %w", sessionID, err)
	}
	return msgs, nil
}

func (s *TranscriptStore) Save(ctx context.Context, sessionID string, messages []llm.Message) error {
	raw, err := json.Marshal(messages)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO transcripts(session_id, messages) VALUES ($1, $2::jsonb)
		ON CONFLICT (session_id) DO UPDATE SET messages = EXCLUDED.messages`, sessionID, string(raw))
	return err
}
