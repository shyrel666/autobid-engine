package db

// 控制面查询：任务列表/删除、产出物、Profile 维护与熔断状态，供 HTTP API 使用。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ListTasks 按 id 倒序列出任务；status 为空表示全部。
func (d *DB) ListTasks(ctx context.Context, status string, limit int) ([]*Task, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+taskCols+` FROM tasks
		WHERE ($1 = '' OR status = $1) ORDER BY id DESC LIMIT $2`, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteTask 单事务删除任务及其推演空间（节点、边、锚点、产出物随外键级联；账本与审计保留，task_id 置空）。
// 全局实体（assets）不删除，只移除其 task_ids 中的该任务。调用方须先停止任务运行时。
func (d *DB) DeleteTask(ctx context.Context, id int64) error {
	return d.InTx(ctx, func(tx *sql.Tx) error {
		var expID int64
		err := tx.QueryRowContext(ctx, `DELETE FROM tasks WHERE id = $1 RETURNING exploration_id`, id).Scan(&expID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: task %d", ErrNotFound, id)
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE assets SET task_ids = array_remove(task_ids, $1::bigint)
			WHERE task_ids @> ARRAY[$1::bigint]`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE entity_edges SET task_ids = array_remove(task_ids, $1::bigint)
			WHERE task_ids @> ARRAY[$1::bigint]`, id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM explorations WHERE id = $1`, expID)
		return err
	})
}

// Finding 是产出物记录及其证据节点。
type Finding struct {
	ID         int64           `json:"id"`
	TaskID     int64           `json:"task_id"`
	NodeID     int64           `json:"node_id"`
	IntentID   int64           `json:"intent_id,omitempty"`
	Category   string          `json:"category"`
	Severity   string          `json:"severity"`
	Title      string          `json:"title"`
	Summary    string          `json:"summary"`
	Payload    json.RawMessage `json:"payload"`
	EvidenceID []int64         `json:"evidence_node_ids"`
	CreatedAt  time.Time       `json:"created_at"`
}

// ListFindings 按严重度（critical 优先）与时间列出任务产出物；category 为空表示全部。
func (d *DB) ListFindings(ctx context.Context, taskID int64, category string) ([]*Finding, error) {
	rows, err := d.QueryContext(ctx, `SELECT f.id, f.task_id, f.node_id, COALESCE(f.intent_id, 0), f.category, f.severity,
			f.title, f.summary, f.payload,
			COALESCE((SELECT to_jsonb(array_agg(e.node_id ORDER BY e.node_id)) FROM finding_evidence e WHERE e.finding_id = f.id), '[]'),
			f.created_at
		FROM findings f
		WHERE f.task_id = $1 AND ($2 = '' OR f.category = $2)
		ORDER BY array_position(ARRAY['critical','high','medium','low','info'], f.severity), f.id`, taskID, category)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Finding
	for rows.Next() {
		var (
			f        Finding
			payload  []byte
			evidence []byte
		)
		if err := rows.Scan(&f.ID, &f.TaskID, &f.NodeID, &f.IntentID, &f.Category, &f.Severity, &f.Title, &f.Summary,
			&payload, &evidence, &f.CreatedAt); err != nil {
			return nil, err
		}
		f.Payload = payload
		if err := json.Unmarshal(evidence, &f.EvidenceID); err != nil {
			return nil, err
		}
		out = append(out, &f)
	}
	return out, rows.Err()
}

func (d *DB) ListLLMProfiles(ctx context.Context) ([]*LLMProfile, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+llmProfileCols+` FROM llm_profiles ORDER BY rank, priority DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*LLMProfile
	for rows.Next() {
		p, err := scanLLMProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdateLLMProfile 整体覆盖 Profile 的可编辑字段（id 与 created_at 不变）。
func (d *DB) UpdateLLMProfile(ctx context.Context, p LLMProfile) error {
	res, err := d.ExecContext(ctx, `UPDATE llm_profiles SET name = $2, format = $3, base_url = $4, api_key_env = $5,
			model = $6, max_tokens = $7, effort = $8, thinking_display = $9, priority = $10, rank = $11,
			window_tokens = $12, enabled = $13, in_global_pool = $14, proxy = $15, session_header_key = $16
		WHERE id = $1`,
		p.ID, p.Name, p.Format, p.BaseURL, p.APIKeyEnv, p.Model, p.MaxTokens, p.Effort, p.ThinkingDisplay,
		p.Priority, p.Rank, p.WindowTokens, p.Enabled, p.InGlobalPool, p.Proxy, p.SessionHeaderKey)
	return requireOneRow(res, err, p.ID)
}

func (d *DB) DeleteLLMProfile(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `DELETE FROM llm_profiles WHERE id = $1`, id)
	return requireOneRow(res, err, id)
}

// ListLLMHealth 返回全部持久化的熔断状态（含已过冷却期的历史记录）。
func (d *DB) ListLLMHealth(ctx context.Context) ([]LLMHealth, error) {
	rows, err := d.QueryContext(ctx, `SELECT profile_id, fails, trips, COALESCE(open_until, 'epoch'::timestamptz),
			last_error, COALESCE(last_at, 'epoch'::timestamptz)
		FROM llm_profile_health ORDER BY profile_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LLMHealth
	for rows.Next() {
		var h LLMHealth
		if err := rows.Scan(&h.ProfileID, &h.Fails, &h.Trips, &h.OpenUntil, &h.LastError, &h.LastAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// TaskLLMChain 返回任务显式配置链（按 position）。
type ChainEntry struct {
	Position  int    `json:"position"`
	ProfileID int64  `json:"profile_id"`
	Status    string `json:"status"`
}

func (d *DB) TaskLLMChain(ctx context.Context, taskID int64) ([]ChainEntry, error) {
	rows, err := d.QueryContext(ctx, `SELECT position, profile_id, status FROM task_llm_profiles
		WHERE task_id = $1 ORDER BY position`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChainEntry
	for rows.Next() {
		var c ChainEntry
		if err := rows.Scan(&c.Position, &c.ProfileID, &c.Status); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
