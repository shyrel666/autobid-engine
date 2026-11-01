package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type LLMProfile struct {
	LLMOptions
	IsDefault        bool   `json:"is_default"`
	APIKey           string `json:"-"` // resolved in memory only
	APIKeyCiphertext string `json:"-"`
	APIKeyHint       string `json:"api_key_hint,omitempty"`
	KeyConfigured    bool   `json:"key_configured"`
	CreatedBy        string `json:"-"`
	UpdatedBy        string `json:"-"`
	ID               int64  `json:"id"`
	Name             string `json:"name"`
	Format           string `json:"format"`
	BaseURL          string `json:"base_url,omitempty"`
	APIKeyEnv        string `json:"api_key_env"`
	Model            string `json:"model"`
	MaxTokens        int    `json:"max_tokens,omitempty"`
	Effort           string `json:"effort,omitempty"`
	ThinkingDisplay  string `json:"thinking_display,omitempty"`
	Priority         int    `json:"priority"`
	Rank             int    `json:"rank"`
	WindowTokens     int    `json:"window_tokens,omitempty"`
	Enabled          bool   `json:"enabled"`
	InGlobalPool     bool   `json:"in_global_pool"`
	// Proxy 为出站 HTTP(S) 代理 URL，空表示沿用环境代理设置。
	Proxy string `json:"proxy,omitempty"`
	// SessionHeaderKey 非空时，每个请求携带该请求头，值为 Agent 会话 ID（网关会话亲和/缓存命中）。
	SessionHeaderKey string `json:"session_header_key,omitempty"`
}

const llmProfileCols = `id, name, format, base_url, api_key_env, model, max_tokens, effort, thinking_display,
	priority, rank, window_tokens, enabled, in_global_pool, proxy, session_header_key,
	is_default, api_key_ciphertext, api_key_hint, options, created_by, updated_by`

func scanLLMProfile(r rowScanner) (*LLMProfile, error) {
	var p LLMProfile
	var options []byte
	err := r.Scan(&p.ID, &p.Name, &p.Format, &p.BaseURL, &p.APIKeyEnv, &p.Model, &p.MaxTokens, &p.Effort,
		&p.ThinkingDisplay, &p.Priority, &p.Rank, &p.WindowTokens, &p.Enabled, &p.InGlobalPool, &p.Proxy, &p.SessionHeaderKey,
		&p.IsDefault, &p.APIKeyCiphertext, &p.APIKeyHint, &options, &p.CreatedBy, &p.UpdatedBy)
	if err == nil {
		err = json.Unmarshal(options, &p.LLMOptions)
	}
	return &p, err
}

// CreateLLMProfile 写入 Profile。Format 默认 anthropic；APIKeyEnv 按格式默认 ANTHROPIC_API_KEY / OPENAI_API_KEY；
// Effort 为空时不发送；旧记录的显式 high 保留。
func (d *DB) CreateLLMProfile(ctx context.Context, p LLMProfile) (int64, error) {
	if p.Format == "" {
		p.Format = "anthropic"
	}
	if p.APIKeyEnv == "" {
		p.APIKeyEnv = "ANTHROPIC_API_KEY"
		if p.Format == "openai" || p.Format == "openai-responses" {
			p.APIKeyEnv = "OPENAI_API_KEY"
		}
	}
	options, err := json.Marshal(p.LLMOptions)
	if err != nil {
		return 0, err
	}
	var id int64
	err = d.QueryRowContext(ctx, `INSERT INTO llm_profiles(name, format, base_url, api_key_env, model, max_tokens,
			effort, thinking_display, priority, rank, window_tokens, enabled, in_global_pool, proxy, session_header_key,
			api_key_ciphertext, api_key_hint, options, created_by, updated_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)
		RETURNING id`,
		p.Name, p.Format, p.BaseURL, p.APIKeyEnv, p.Model, p.MaxTokens, p.Effort, p.ThinkingDisplay,
		p.Priority, p.Rank, p.WindowTokens, p.Enabled, p.InGlobalPool, p.Proxy, p.SessionHeaderKey,
		p.APIKeyCiphertext, p.APIKeyHint, options, p.CreatedBy, p.UpdatedBy).Scan(&id)
	return id, err
}

func (d *DB) GetLLMProfile(ctx context.Context, id int64) (*LLMProfile, error) {
	p, err := scanLLMProfile(d.QueryRowContext(ctx, `SELECT `+llmProfileCols+` FROM llm_profiles WHERE id = $1 AND tenant_id='default'`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: llm profile %d", ErrNotFound, id)
	}
	return p, err
}

// GlobalPoolProfiles 返回参与全局池的启用 Profile，按 rank、priority DESC、id 排序。
func (d *DB) GlobalPoolProfiles(ctx context.Context) ([]*LLMProfile, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+llmProfileCols+` FROM llm_profiles
		WHERE tenant_id='default' AND enabled AND (in_global_pool OR is_default) ORDER BY is_default DESC, priority DESC, id`)
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

// SetAgentLLMBinding 绑定（profileID>0）或解绑（profileID<=0）角色 Profile。
func (d *DB) SetAgentLLMBinding(ctx context.Context, agentKey string, profileID int64) error {
	if profileID <= 0 {
		_, err := d.ExecContext(ctx, `DELETE FROM agent_llm_bindings WHERE agent_key = $1`, agentKey)
		return err
	}
	_, err := d.ExecContext(ctx, `INSERT INTO agent_llm_bindings(agent_key, profile_id) VALUES ($1, $2)
		ON CONFLICT (agent_key) DO UPDATE SET profile_id = EXCLUDED.profile_id, updated_at = now()`, agentKey, profileID)
	return err
}

// AgentLLMBinding 返回角色绑定的启用 Profile ID；未绑定或已禁用时返回 0。
func (d *DB) AgentLLMBinding(ctx context.Context, agentKey string) (int64, error) {
	var id int64
	err := d.QueryRowContext(ctx, `SELECT b.profile_id FROM agent_llm_bindings b
		JOIN llm_profiles p ON p.id = b.profile_id
		WHERE b.agent_key = $1 AND p.enabled`, agentKey).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// SetTaskLLMChain 以给定顺序替换任务显式 Profile 链；active 指向首项，revision 递增。空列表表示清除显式链。
func (d *DB) SetTaskLLMChain(ctx context.Context, taskID int64, profileIDs []int64) error {
	return d.SetTaskLLMChainWithActive(ctx, taskID, profileIDs, 0)
}

// SetTaskLLMChainWithActive 替换任务显式 Profile 链并支持显式指定活动配置游标（若未指定或不在链中则默认首项）。
func (d *DB) SetTaskLLMChainWithActive(ctx context.Context, taskID int64, profileIDs []int64, activeProfileID int64) error {
	return d.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT 1 FROM tasks WHERE id = $1 FOR UPDATE`, taskID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM task_llm_profiles WHERE task_id = $1`, taskID); err != nil {
			return err
		}
		var active any
		if len(profileIDs) > 0 {
			active = profileIDs[0]
		}
		for i, pid := range profileIDs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO task_llm_profiles(task_id, position, profile_id)
				VALUES ($1, $2, $3)`, taskID, i, pid); err != nil {
				return err
			}
			if activeProfileID > 0 && pid == activeProfileID {
				active = pid
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE tasks SET active_llm_profile_id = $2,
			llm_chain_revision = llm_chain_revision + 1, llm_failover_state = '', llm_failover_reason = ''
			WHERE id = $1`, taskID, active)
		return err
	})
}

// ResetTaskLLMChain 在配额恢复后把整条链重置为 ready 并从首项重新开始。
func (d *DB) ResetTaskLLMChain(ctx context.Context, taskID int64) error {
	return d.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT 1 FROM tasks WHERE id = $1 FOR UPDATE`, taskID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE task_llm_profiles SET status = 'ready', exhausted_at = NULL
			WHERE task_id = $1`, taskID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE tasks SET
			active_llm_profile_id = (SELECT profile_id FROM task_llm_profiles WHERE task_id = $1 ORDER BY position LIMIT 1),
			llm_chain_revision = llm_chain_revision + 1, llm_failover_state = '', llm_failover_reason = ''
			WHERE id = $1`, taskID)
		return err
	})
}

// TaskLLMState 是一次 LLM 调用选择时读到的任务链快照。
type TaskLLMState struct {
	HasChain        bool
	ActiveProfileID *int64
	Revision        int64
	FailoverState   string
	FailoverReason  string
}

func (d *DB) TaskLLMState(ctx context.Context, taskID int64) (TaskLLMState, error) {
	var (
		s      TaskLLMState
		active sql.NullInt64
	)
	err := d.QueryRowContext(ctx, `SELECT t.active_llm_profile_id, t.llm_chain_revision,
			t.llm_failover_state, t.llm_failover_reason,
			EXISTS (SELECT 1 FROM task_llm_profiles c WHERE c.task_id = t.id)
		FROM tasks t WHERE t.id = $1`, taskID).Scan(&active, &s.Revision, &s.FailoverState, &s.FailoverReason, &s.HasChain)
	if errors.Is(err, sql.ErrNoRows) {
		return s, fmt.Errorf("%w: task %d", ErrNotFound, taskID)
	}
	if active.Valid {
		s.ActiveProfileID = &active.Int64
	}
	return s, err
}

// TaskLLMTransition 是配额耗尽标记的结果（附录 A.12）。
type TaskLLMTransition struct {
	PreviousProfileID int64
	NextProfileID     *int64
	ChainExhausted    bool
	Advanced          bool
	Stale             bool
}

// MarkTaskLLMProfileQuotaExhaustedAtRevision 在任务行锁下标记 Profile 配额耗尽并推进共享游标（§5.4）：
// revision 已变化返回 Stale；耗尽的是当前 active 时推进到 position 更大的 ready 条目，无后继则链耗尽（不回绕）。
func (d *DB) MarkTaskLLMProfileQuotaExhaustedAtRevision(ctx context.Context, taskID, profileID, revision int64, reason string) (TaskLLMTransition, error) {
	tr := TaskLLMTransition{PreviousProfileID: profileID}
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		var (
			rev    int64
			active sql.NullInt64
		)
		if err := tx.QueryRowContext(ctx, `SELECT llm_chain_revision, active_llm_profile_id FROM tasks
			WHERE id = $1 FOR UPDATE`, taskID).Scan(&rev, &active); err != nil {
			return err
		}
		if rev != revision || !active.Valid || active.Int64 != profileID {
			tr.Stale = true
			if active.Valid {
				tr.NextProfileID = &active.Int64
			}
			return nil
		}
		var position int
		if err := tx.QueryRowContext(ctx, `UPDATE task_llm_profiles SET status = 'quota_exhausted', exhausted_at = now()
			WHERE task_id = $1 AND profile_id = $2 RETURNING position`, taskID, profileID).Scan(&position); err != nil {
			return fmt.Errorf("mark profile %d exhausted: %w", profileID, err)
		}
		var next sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT profile_id FROM task_llm_profiles
			WHERE task_id = $1 AND status = 'ready' AND position > $2 ORDER BY position LIMIT 1`,
			taskID, position).Scan(&next)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		state := "advanced"
		if next.Valid {
			tr.NextProfileID = &next.Int64
		} else {
			tr.ChainExhausted = true
			state = "exhausted"
		}
		tr.Advanced = true
		var nextArg any
		if next.Valid {
			nextArg = next.Int64
		}
		_, err = tx.ExecContext(ctx, `UPDATE tasks SET active_llm_profile_id = $2,
			llm_chain_revision = llm_chain_revision + 1, llm_failover_state = $3, llm_failover_reason = $4
			WHERE id = $1`, taskID, nextArg, state, reason)
		return err
	})
	return tr, err
}

// LLMUsage 是 token 账本一行（附录 A.19 扩展）。
type LLMUsage struct {
	TaskID        int64
	ExplorationID int64
	SessionID     string
	Worker        string
	Model         string
	ProfileID     int64
	ProfileName   string
	Effort        string
	Streaming     bool
	LatencyMs     int
	InputTokens   int
	OutputTokens  int
	CacheRead     int
	CacheWrite    int
	Status        string // ok / error / cancelled
	Error         string
}

func (d *DB) InsertLLMUsage(ctx context.Context, u LLMUsage) error {
	_, err := d.ExecContext(ctx, `INSERT INTO llm_usage(task_id, exploration_id, session_id, worker, model, profile_id,
			profile_name, effort, streaming, latency_ms, input_tokens, output_tokens, cache_read, cache_write, status, error)
		VALUES (NULLIF($1::bigint, 0), NULLIF($2::bigint, 0), $3, $4, $5, NULLIF($6::bigint, 0), $7, $8, $9, $10,
			$11, $12, $13, $14, $15, $16)`,
		u.TaskID, u.ExplorationID, u.SessionID, u.Worker, u.Model, u.ProfileID, u.ProfileName, u.Effort, u.Streaming,
		u.LatencyMs, u.InputTokens, u.OutputTokens, u.CacheRead, u.CacheWrite, u.Status, truncateText(u.Error, 2000))
	return err
}

// TaskUsageTotals 按模型汇总任务 token 用量。
type UsageTotal struct {
	Model        string
	Calls        int
	InputTokens  int64
	OutputTokens int64
	CacheRead    int64
	CacheWrite   int64
}

func (d *DB) TaskUsageTotals(ctx context.Context, taskID int64) ([]UsageTotal, error) {
	rows, err := d.QueryContext(ctx, `SELECT model, COUNT(*), SUM(input_tokens), SUM(output_tokens),
			SUM(cache_read), SUM(cache_write)
		FROM llm_usage WHERE task_id = $1 GROUP BY model ORDER BY model`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageTotal
	for rows.Next() {
		var u UsageTotal
		if err := rows.Scan(&u.Model, &u.Calls, &u.InputTokens, &u.OutputTokens, &u.CacheRead, &u.CacheWrite); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func truncateText(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// LLMHealth 是 llmpool 熔断状态的持久化形式。
type LLMHealth struct {
	ProfileID int64
	Fails     int
	Trips     int
	OpenUntil time.Time
	LastError string
	LastAt    time.Time
}

func (d *DB) SaveLLMHealth(ctx context.Context, h LLMHealth) error {
	_, err := d.ExecContext(ctx, `INSERT INTO llm_profile_health(profile_id, fails, trips, open_until, last_error, last_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (profile_id) DO UPDATE SET fails = EXCLUDED.fails, trips = EXCLUDED.trips,
			open_until = EXCLUDED.open_until, last_error = EXCLUDED.last_error, last_at = EXCLUDED.last_at`,
		h.ProfileID, h.Fails, h.Trips, nullableTime(h.OpenUntil), truncateText(h.LastError, 1000), nullableTime(h.LastAt))
	return err
}

func (d *DB) DeleteLLMHealth(ctx context.Context, profileID int64) error {
	_, err := d.ExecContext(ctx, `DELETE FROM llm_profile_health WHERE profile_id = $1`, profileID)
	return err
}

// ActiveLLMHealth 返回熔断仍在生效的状态，用于启动恢复。
func (d *DB) ActiveLLMHealth(ctx context.Context) ([]LLMHealth, error) {
	rows, err := d.QueryContext(ctx, `SELECT profile_id, fails, trips, open_until, last_error, COALESCE(last_at, now())
		FROM llm_profile_health WHERE open_until > now()`)
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

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// LLMRecord 是一次 HTTP 往返的原始正文录制（不含请求头）。
type LLMRecord struct {
	TaskID        int64
	ExplorationID int64
	SessionID     string
	Worker        string
	ProfileID     int64
	ProfileName   string
	Method        string
	URL           string
	StatusCode    int
	LatencyMs     int
	RequestBody   string
	ResponseBody  string
	Truncated     bool
	Error         string
}

func (d *DB) InsertLLMRecord(ctx context.Context, r LLMRecord) error {
	_, err := d.ExecContext(ctx, `INSERT INTO llm_records(task_id, exploration_id, session_id, worker, profile_id,
			profile_name, method, url, status_code, latency_ms, request_body, response_body, truncated, error)
		VALUES (NULLIF($1::bigint, 0), NULLIF($2::bigint, 0), $3, $4, NULLIF($5::bigint, 0), $6, $7, $8, $9, $10,
			$11, $12, $13, $14)`,
		r.TaskID, r.ExplorationID, r.SessionID, r.Worker, r.ProfileID, r.ProfileName, r.Method, r.URL,
		r.StatusCode, r.LatencyMs, r.RequestBody, r.ResponseBody, r.Truncated, truncateText(r.Error, 2000))
	return err
}

// LLMRecordSummary 是列表展示用的原始报文录制元数据（不含大体积正文）。
type LLMRecordSummary struct {
	ID            int64     `json:"id"`
	TaskID        int64     `json:"task_id"`
	ExplorationID int64     `json:"exploration_id"`
	SessionID     string    `json:"session_id"`
	Worker        string    `json:"worker"`
	ProfileID     int64     `json:"profile_id"`
	ProfileName   string    `json:"profile_name"`
	Method        string    `json:"method"`
	URL           string    `json:"url"`
	StatusCode    int       `json:"status_code"`
	LatencyMs     int       `json:"latency_ms"`
	Truncated     bool      `json:"truncated"`
	Error         string    `json:"error"`
	CreatedAt     time.Time `json:"created_at"`
}

// LLMRecordDetail 包含完整请求与响应正文。
type LLMRecordDetail struct {
	LLMRecordSummary
	RequestBody  string `json:"request_body"`
	ResponseBody string `json:"response_body"`
}

// LLMRecordFilter 控制原始报文录制查询条件。
type LLMRecordFilter struct {
	TaskID      int64
	SessionID   string
	ProfileName string
	Limit       int
	Offset      int
}

func (d *DB) ListLLMRecords(ctx context.Context, f LLMRecordFilter) ([]LLMRecordSummary, int, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	var total int
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM llm_records
		WHERE ($1::bigint = 0 OR task_id = $1)
		  AND ($2::text = '' OR session_id ILIKE '%' || $2 || '%')
		  AND ($3::text = '' OR profile_name ILIKE '%' || $3 || '%')`,
		f.TaskID, f.SessionID, f.ProfileName).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := d.QueryContext(ctx, `SELECT id, COALESCE(task_id,0), COALESCE(exploration_id,0), session_id,
		worker, COALESCE(profile_id,0), profile_name, method, url, status_code, latency_ms, truncated, error, created_at
		FROM llm_records
		WHERE ($1::bigint = 0 OR task_id = $1)
		  AND ($2::text = '' OR session_id ILIKE '%' || $2 || '%')
		  AND ($3::text = '' OR profile_name ILIKE '%' || $3 || '%')
		ORDER BY id DESC LIMIT $4 OFFSET $5`,
		f.TaskID, f.SessionID, f.ProfileName, f.Limit, f.Offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]LLMRecordSummary, 0)
	for rows.Next() {
		var r LLMRecordSummary
		if err := rows.Scan(&r.ID, &r.TaskID, &r.ExplorationID, &r.SessionID, &r.Worker, &r.ProfileID,
			&r.ProfileName, &r.Method, &r.URL, &r.StatusCode, &r.LatencyMs, &r.Truncated, &r.Error, &r.CreatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

func (d *DB) GetLLMRecord(ctx context.Context, id int64) (*LLMRecordDetail, error) {
	var r LLMRecordDetail
	err := d.QueryRowContext(ctx, `SELECT id, COALESCE(task_id,0), COALESCE(exploration_id,0), session_id,
		worker, COALESCE(profile_id,0), profile_name, method, url, status_code, latency_ms, truncated, error, created_at,
		request_body, response_body
		FROM llm_records WHERE id = $1`, id).Scan(
		&r.ID, &r.TaskID, &r.ExplorationID, &r.SessionID, &r.Worker, &r.ProfileID,
		&r.ProfileName, &r.Method, &r.URL, &r.StatusCode, &r.LatencyMs, &r.Truncated, &r.Error, &r.CreatedAt,
		&r.RequestBody, &r.ResponseBody)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (d *DB) DeleteLLMRecordsByTask(ctx context.Context, taskID int64) (int64, error) {
	var (
		res sql.Result
		err error
	)
	if taskID > 0 {
		res, err = d.ExecContext(ctx, `DELETE FROM llm_records WHERE task_id = $1`, taskID)
	} else {
		res, err = d.ExecContext(ctx, `DELETE FROM llm_records`)
	}
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
