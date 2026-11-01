package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// 拦截控制面持久化（§6.1 / §6.3）。审批“只决定一次”完全由 ResolveIntercept 的
// WHERE status='pending' 条件 UPDATE 保证；内存 Channel 只负责唤醒。

const (
	InterceptPending = "pending"
	InterceptAllowed = "allowed"
	InterceptDenied  = "denied"
	InterceptTimeout = "timeout"
)

type InterceptRule struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	ToolFilter     string `json:"tool_filter"`  // 空表示全部启用拦截的工具
	MatchTarget    string `json:"match_target"` // tool_name / tool_input
	MatchType      string `json:"match_type"`   // string / regex
	Pattern        string `json:"pattern"`
	Action         string `json:"action"` // allow / deny / ask
	Message        string `json:"message"`
	Priority       int    `json:"priority"`
	Enabled        bool   `json:"enabled"`
	TimeoutEnabled bool   `json:"timeout_enabled"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	TimeoutAction  string `json:"timeout_action"`
}

// CreateInterceptRule 写入规则；MatchTarget/MatchType 为空时取列默认值。
func (d *DB) CreateInterceptRule(ctx context.Context, r InterceptRule) (int64, error) {
	if r.MatchTarget == "" {
		r.MatchTarget = "tool_name"
	}
	if r.MatchType == "" {
		r.MatchType = "string"
	}
	if r.TimeoutAction == "" {
		r.TimeoutAction = "deny"
	}
	if r.TimeoutSeconds <= 0 {
		r.TimeoutSeconds = 300
	}
	var id int64
	err := d.QueryRowContext(ctx, `INSERT INTO intercept_rules
		(name, tool_filter, match_target, match_type, pattern, action, message, priority, enabled,
		 timeout_enabled, timeout_seconds, timeout_action)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING id`,
		r.Name, r.ToolFilter, r.MatchTarget, r.MatchType, r.Pattern, r.Action, r.Message, r.Priority, r.Enabled,
		r.TimeoutEnabled, r.TimeoutSeconds, r.TimeoutAction).Scan(&id)
	return id, err
}

// UpdateInterceptRule 更新已有拦截规则。
func (d *DB) UpdateInterceptRule(ctx context.Context, r InterceptRule) error {
	if r.MatchTarget == "" {
		r.MatchTarget = "tool_name"
	}
	if r.MatchType == "" {
		r.MatchType = "string"
	}
	if r.TimeoutAction == "" {
		r.TimeoutAction = "deny"
	}
	if r.TimeoutSeconds <= 0 {
		r.TimeoutSeconds = 300
	}
	res, err := d.ExecContext(ctx, `UPDATE intercept_rules SET
		name = $2, tool_filter = $3, match_target = $4, match_type = $5, pattern = $6, action = $7,
		message = $8, priority = $9, enabled = $10, timeout_enabled = $11, timeout_seconds = $12, timeout_action = $13
		WHERE id = $1`,
		r.ID, r.Name, r.ToolFilter, r.MatchTarget, r.MatchType, r.Pattern, r.Action,
		r.Message, r.Priority, r.Enabled, r.TimeoutEnabled, r.TimeoutSeconds, r.TimeoutAction)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}

func (d *DB) SetInterceptRuleEnabled(ctx context.Context, id int64, enabled bool) error {
	res, err := d.ExecContext(ctx, `UPDATE intercept_rules SET enabled = $2 WHERE id = $1`, id, enabled)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}

func (d *DB) DeleteInterceptRule(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, `DELETE FROM intercept_rules WHERE id = $1`, id)
	return err
}

// ListInterceptRules 按 priority DESC, id ASC 返回全部拦截规则（含已停用规则，供控制面管理）。
func (d *DB) ListInterceptRules(ctx context.Context) ([]InterceptRule, error) {
	rows, err := d.QueryContext(ctx, `SELECT id, name, tool_filter, match_target, match_type, pattern, action, message,
		priority, enabled, timeout_enabled, timeout_seconds, timeout_action
		FROM intercept_rules ORDER BY priority DESC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]InterceptRule, 0)
	for rows.Next() {
		var r InterceptRule
		if err := rows.Scan(&r.ID, &r.Name, &r.ToolFilter, &r.MatchTarget, &r.MatchType, &r.Pattern, &r.Action, &r.Message,
			&r.Priority, &r.Enabled, &r.TimeoutEnabled, &r.TimeoutSeconds, &r.TimeoutAction); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// EnabledInterceptRules 按 priority DESC, id ASC 返回启用规则（§6.1）；正则编译由调用方在缓存阶段完成。
func (d *DB) EnabledInterceptRules(ctx context.Context) ([]InterceptRule, error) {
	rows, err := d.QueryContext(ctx, `SELECT id, name, tool_filter, match_target, match_type, pattern, action, message,
		priority, enabled, timeout_enabled, timeout_seconds, timeout_action
		FROM intercept_rules WHERE enabled ORDER BY priority DESC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InterceptRule
	for rows.Next() {
		var r InterceptRule
		if err := rows.Scan(&r.ID, &r.Name, &r.ToolFilter, &r.MatchTarget, &r.MatchType, &r.Pattern, &r.Action, &r.Message,
			&r.Priority, &r.Enabled, &r.TimeoutEnabled, &r.TimeoutSeconds, &r.TimeoutAction); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// InterceptConfig 是控制面单行配置；EnabledTools 为 nil 表示使用内置默认集合。
type InterceptConfig struct {
	EnabledTools []string
	Judge        json.RawMessage
}

func (d *DB) GetInterceptConfig(ctx context.Context) (InterceptConfig, error) {
	var (
		cfg   InterceptConfig
		tools sql.NullString
		judge []byte
	)
	// 数组经 JSON 往返，避免依赖驱动对 TEXT[] 的扫描支持。
	err := d.QueryRowContext(ctx, `SELECT array_to_json(enabled_tools)::text, judge FROM intercept_config WHERE id = 1`).Scan(&tools, &judge)
	if errors.Is(err, sql.ErrNoRows) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if tools.Valid {
		cfg.EnabledTools = []string{}
		if err := json.Unmarshal([]byte(tools.String), &cfg.EnabledTools); err != nil {
			return cfg, err
		}
	}
	cfg.Judge = judge
	return cfg, nil
}

// SaveInterceptConfig 覆盖写入配置；EnabledTools 为 nil 时恢复默认集合。
func (d *DB) SaveInterceptConfig(ctx context.Context, cfg InterceptConfig) error {
	var tools any
	if cfg.EnabledTools != nil {
		raw, err := json.Marshal(cfg.EnabledTools)
		if err != nil {
			return err
		}
		tools = string(raw)
	}
	judge := cfg.Judge
	if len(judge) == 0 {
		judge = json.RawMessage(`{}`)
	}
	_, err := d.ExecContext(ctx, `INSERT INTO intercept_config(id, enabled_tools, judge) VALUES (1,
		CASE WHEN $1::jsonb IS NULL THEN NULL ELSE ARRAY(SELECT jsonb_array_elements_text($1::jsonb)) END, $2::jsonb)
		ON CONFLICT (id) DO UPDATE SET enabled_tools = EXCLUDED.enabled_tools, judge = EXCLUDED.judge, updated_at = now()`,
		tools, string(judge))
	return err
}

// InterceptPendingRow 是一条审批挂起记录。
type InterceptPendingRow struct {
	ID              int64           `json:"id"`
	RuleID          int64           `json:"rule_id"`
	TaskID          int64           `json:"task_id"`
	IntentID        int64           `json:"intent_id"`
	AgentName       string          `json:"agent_name"`
	ToolName        string          `json:"tool_name"`
	ToolInput       string          `json:"tool_input"`
	Message         string          `json:"message"`
	Status          string          `json:"status"`
	EffectiveAction string          `json:"effective_action"`
	Reason          string          `json:"reason"`
	TimeoutAction   string          `json:"timeout_action"`
	Audit           json.RawMessage `json:"audit"`
	Waiter          string          `json:"waiter"`
	WaiterUntil     *time.Time      `json:"waiter_until"`
	ExecOutcome     string          `json:"exec_outcome"`
	ExecDetail      string          `json:"exec_detail"`
	CreatedAt       time.Time       `json:"created_at"`
	DecidedAt       *time.Time      `json:"decided_at"`
}

// Allowed 报告已决记录是否允许执行：timeout 本身不说明是否执行，以 effective_action 为准（§6.3 第 5 步）。
func (r *InterceptPendingRow) Allowed() bool {
	return r.Status == InterceptAllowed || (r.Status != InterceptPending && r.EffectiveAction == "allow")
}

type NewInterceptPending struct {
	RuleID        int64
	TaskID        int64
	IntentID      int64
	AgentName     string
	ToolName      string
	ToolInput     []byte
	Message       string
	TimeoutAction string
	Audit         json.RawMessage
	Waiter        string
	WaiterTTL     time.Duration
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// CreateInterceptPending 插入 status='pending' 行并登记等待方心跳（§6.3 第 1 步）。
func (d *DB) CreateInterceptPending(ctx context.Context, p NewInterceptPending) (int64, error) {
	audit := p.Audit
	if len(audit) == 0 {
		audit = json.RawMessage(`{}`)
	}
	if p.TimeoutAction == "" {
		p.TimeoutAction = "deny"
	}
	var id int64
	err := d.QueryRowContext(ctx, `INSERT INTO intercept_pending
		(rule_id, task_id, intent_id, agent_name, tool_name, tool_input, message, timeout_action, audit, waiter, waiter_until)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10, now() + make_interval(secs => $11))
		RETURNING id`,
		nullID(p.RuleID), nullID(p.TaskID), nullID(p.IntentID), p.AgentName, p.ToolName, string(p.ToolInput), p.Message,
		p.TimeoutAction, string(audit), p.Waiter, p.WaiterTTL.Seconds()).Scan(&id)
	return id, err
}

func (d *DB) GetInterceptPending(ctx context.Context, id int64) (*InterceptPendingRow, error) {
	var (
		r                      InterceptPendingRow
		ruleID, taskID, intent sql.NullInt64
		waiterUntil, decidedAt sql.NullTime
		audit                  []byte
	)
	err := d.QueryRowContext(ctx, `SELECT id, rule_id, task_id, intent_id, agent_name, tool_name, tool_input, message,
		status, effective_action, reason, timeout_action, audit, waiter, waiter_until, exec_outcome, exec_detail,
		created_at, decided_at FROM intercept_pending WHERE id = $1`, id).Scan(
		&r.ID, &ruleID, &taskID, &intent, &r.AgentName, &r.ToolName, &r.ToolInput, &r.Message,
		&r.Status, &r.EffectiveAction, &r.Reason, &r.TimeoutAction, &audit, &r.Waiter, &waiterUntil, &r.ExecOutcome, &r.ExecDetail,
		&r.CreatedAt, &decidedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.RuleID, r.TaskID, r.IntentID = ruleID.Int64, taskID.Int64, intent.Int64
	r.Audit = audit
	r.WaiterUntil, r.DecidedAt = nullTimePtr(waiterUntil), nullTimePtr(decidedAt)
	return &r, nil
}

// ListOpenInterceptPending 返回待审批记录（供审批端列表）；taskID=0 表示全部任务。
func (d *DB) ListOpenInterceptPending(ctx context.Context, taskID int64) ([]int64, error) {
	rows, err := d.QueryContext(ctx, `SELECT id FROM intercept_pending
		WHERE status = 'pending' AND ($1 = 0 OR task_id = $1) ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ResolveIntercept 是超时、取消与人工审批共用的 CAS（§6.3 第 6 步）：仅 status='pending' 时写入，
// 返回本次调用是否赢得决定。requireLiveWaiter=true 时还要求等待方心跳未过期（人工审批不能批准一个已无人等待的调用）。
func (d *DB) ResolveIntercept(ctx context.Context, id int64, status, effectiveAction, reason string, requireLiveWaiter bool) (bool, error) {
	res, err := d.ExecContext(ctx, `UPDATE intercept_pending
		SET status = $2, effective_action = $3, reason = $4, decided_at = now()
		WHERE id = $1 AND status = 'pending' AND (NOT $5 OR waiter_until > now())`,
		id, status, effectiveAction, reason, requireLiveWaiter)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// TouchInterceptWaiter 续期等待方心跳；返回记录是否仍为 pending。
func (d *DB) TouchInterceptWaiter(ctx context.Context, id int64, ttl time.Duration) (bool, error) {
	res, err := d.ExecContext(ctx, `UPDATE intercept_pending SET waiter_until = now() + make_interval(secs => $2)
		WHERE id = $1 AND status = 'pending'`, id, ttl.Seconds())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// CompleteIntercept 记录审批之后工具的实际执行结果（executed / not_executed / error）。
func (d *DB) CompleteIntercept(ctx context.Context, id int64, outcome, detail string) error {
	_, err := d.ExecContext(ctx, `UPDATE intercept_pending SET exec_outcome = $2, exec_detail = $3 WHERE id = $1`,
		id, outcome, truncateText(detail, 2000))
	return err
}

// ReapOrphanedIntercepts 把等待方心跳过期的 pending 落为 denied（进程重启后旧请求不会被重新绑定，
// 至少不再悬挂；§8.2 “审批挂起可自动跨进程恢复”缺口的最小兜底）。返回回收的 ID。
func (d *DB) ReapOrphanedIntercepts(ctx context.Context) ([]int64, error) {
	rows, err := d.QueryContext(ctx, `UPDATE intercept_pending
		SET status = 'denied', effective_action = 'deny', reason = '等待方已失联（进程重启或心跳过期），按拒绝处理',
		    decided_at = now(), exec_outcome = 'not_executed'
		WHERE status = 'pending' AND (waiter_until IS NULL OR waiter_until <= now())
		RETURNING id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// InterceptAuditRow 是一条已决审计。
type InterceptAuditRow struct {
	TaskID          int64  `json:"task_id"`
	IntentID        int64  `json:"intent_id"`
	AgentName       string `json:"agent_name"`
	ToolName        string `json:"tool_name"`
	InputDigest     string `json:"input_digest"`
	Source          string `json:"source"` // rule / judge / default / ask
	RuleID          int64  `json:"rule_id"`
	ProfileID       int64  `json:"profile_id"`
	Action          string `json:"action"`
	EffectiveAction string `json:"effective_action"`
	PendingID       int64  `json:"pending_id"`
	Reason          string `json:"reason"`
}

func (d *DB) InsertInterceptAudit(ctx context.Context, a InterceptAuditRow) error {
	_, err := d.ExecContext(ctx, `INSERT INTO intercept_audit
		(task_id, intent_id, agent_name, tool_name, input_digest, source, rule_id, profile_id, action, effective_action, pending_id, reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		nullID(a.TaskID), nullID(a.IntentID), a.AgentName, a.ToolName, a.InputDigest, a.Source, nullID(a.RuleID),
		nullID(a.ProfileID), a.Action, a.EffectiveAction, nullID(a.PendingID), truncateText(a.Reason, 2000))
	return err
}

// InterceptAuditForTask 返回任务的已决审计（按时间升序），供测试与审批端查询。
func (d *DB) InterceptAuditForTask(ctx context.Context, taskID int64) ([]InterceptAuditRow, error) {
	rows, err := d.QueryContext(ctx, `SELECT COALESCE(task_id,0), COALESCE(intent_id,0), agent_name, tool_name, input_digest,
		source, COALESCE(rule_id,0), COALESCE(profile_id,0), action, effective_action, COALESCE(pending_id,0), reason
		FROM intercept_audit WHERE task_id = $1 ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InterceptAuditRow
	for rows.Next() {
		var a InterceptAuditRow
		if err := rows.Scan(&a.TaskID, &a.IntentID, &a.AgentName, &a.ToolName, &a.InputDigest, &a.Source, &a.RuleID,
			&a.ProfileID, &a.Action, &a.EffectiveAction, &a.PendingID, &a.Reason); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// InterceptApprovalRow 扩展 InterceptPendingRow，附带命中规则名称与决策来源（rule / model）。
type InterceptApprovalRow struct {
	InterceptPendingRow
	RuleName       string `json:"rule_name"`
	DecisionSource string `json:"decision_source"`
}

// InterceptApprovalFilter 控制审批记录查询条件。
type InterceptApprovalFilter struct {
	TaskID int64
	Status string
	Limit  int
	Offset int
}

// ListInterceptApprovals 返回包含历史已决与当前待审批的多状态审批记录及总条数（pending 优先置顶，其次按 ID 倒序）。
func (d *DB) ListInterceptApprovals(ctx context.Context, f InterceptApprovalFilter) ([]InterceptApprovalRow, int, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	var total int
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM intercept_pending p
		WHERE ($1::bigint = 0 OR p.task_id = $1)
		  AND ($2::text = '' OR p.status = $2)`, f.TaskID, f.Status).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := d.QueryContext(ctx, `SELECT p.id, p.rule_id, p.task_id, p.intent_id, p.agent_name, p.tool_name,
		p.tool_input, p.message, p.status, p.effective_action, p.reason, p.timeout_action, p.audit,
		p.waiter, p.waiter_until, p.exec_outcome, p.exec_detail, p.created_at, p.decided_at,
		COALESCE(r.name, '')
		FROM intercept_pending p
		LEFT JOIN intercept_rules r ON r.id = p.rule_id
		WHERE ($1::bigint = 0 OR p.task_id = $1)
		  AND ($2::text = '' OR p.status = $2)
		ORDER BY CASE WHEN p.status = 'pending' THEN 0 ELSE 1 END, p.id DESC
		LIMIT $3 OFFSET $4`, f.TaskID, f.Status, f.Limit, f.Offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]InterceptApprovalRow, 0)
	for rows.Next() {
		var (
			row                    InterceptApprovalRow
			ruleID, taskID, intent sql.NullInt64
			waiterUntil, decidedAt sql.NullTime
			audit                  []byte
		)
		if err := rows.Scan(
			&row.ID, &ruleID, &taskID, &intent, &row.AgentName, &row.ToolName, &row.ToolInput, &row.Message,
			&row.Status, &row.EffectiveAction, &row.Reason, &row.TimeoutAction, &audit, &row.Waiter, &waiterUntil,
			&row.ExecOutcome, &row.ExecDetail, &row.CreatedAt, &decidedAt, &row.RuleName,
		); err != nil {
			return nil, 0, err
		}
		row.RuleID, row.TaskID, row.IntentID = ruleID.Int64, taskID.Int64, intent.Int64
		row.Audit = audit
		row.WaiterUntil, row.DecidedAt = nullTimePtr(waiterUntil), nullTimePtr(decidedAt)
		if row.RuleID > 0 {
			row.DecisionSource = "rule"
		} else {
			var a struct {
				Source string `json:"source"`
			}
			_ = json.Unmarshal(audit, &a)
			if a.Source == "judge" {
				row.DecisionSource = "model"
			} else if a.Source != "" {
				row.DecisionSource = a.Source
			} else {
				row.DecisionSource = "rule"
			}
		}
		out = append(out, row)
	}
	return out, total, rows.Err()
}

// JudgeDayUsage 是单日裁判模型 Token 消耗汇总。
type JudgeDayUsage struct {
	Date         string `json:"date"`
	Calls        int    `json:"calls"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
}

// JudgeUsageStats 是裁判模型（worker='judge'）累计与按日 Token 消耗。
type JudgeUsageStats struct {
	Calls            int             `json:"calls"`
	InputTokens      int64           `json:"input_tokens"`
	OutputTokens     int64           `json:"output_tokens"`
	CacheReadTokens  int64           `json:"cache_read_tokens"`
	CacheWriteTokens int64           `json:"cache_write_tokens"`
	Daily            []JudgeDayUsage `json:"daily"`
}

func (d *DB) JudgeUsage(ctx context.Context, days int) (JudgeUsageStats, error) {
	if days <= 0 || days > 90 {
		days = 30
	}
	var st JudgeUsageStats
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_write),0)
		FROM llm_usage WHERE worker = 'judge'`).Scan(
		&st.Calls, &st.InputTokens, &st.OutputTokens, &st.CacheReadTokens, &st.CacheWriteTokens,
	); err != nil {
		return st, err
	}
	rows, err := d.QueryContext(ctx, `SELECT to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS day,
		COUNT(*), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0)
		FROM llm_usage
		WHERE worker = 'judge' AND created_at >= now() - make_interval(days => $1)
		GROUP BY day ORDER BY day ASC`, days)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	st.Daily = make([]JudgeDayUsage, 0)
	for rows.Next() {
		var day JudgeDayUsage
		if err := rows.Scan(&day.Date, &day.Calls, &day.InputTokens, &day.OutputTokens); err != nil {
			return st, err
		}
		st.Daily = append(st.Daily, day)
	}
	return st, rows.Err()
}
