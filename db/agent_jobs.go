package db

// S2（AGENT_TOOL_SKILLS_PLAN 5.1~5.3）：Agent 后台作业订阅、终态事件与回合状态机。
//
// 锁序：
//   - 提交/订阅：（需要时）任务行 → Intent 活租约 → 作业行 → 订阅/事件；
//   - 作业终态：作业行 → 订阅/事件（schema 触发器 agent_job_terminal，不锁 exploration_nodes）；
//   - 回合收尾 FinalizeIntentTurn：Intent → 大纲 → 章节 → 订阅/事件 → Hint，不反向锁业务作业；
//   - 唤醒 WakeWaitingIntents：独立短事务，Intent → 订阅/事件。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"time"
)

// 作业引用 kind。只列出已实现同事务校验的类型（chart_render 见 chart_jobs.go）。
const (
	JobKindTenderParse    = "tender_parse"
	JobKindProposalExport = "proposal_export"
)

var agentJobTables = map[string]string{
	JobKindTenderParse:    "fixed_file_jobs",
	JobKindProposalExport: "export_jobs",
	JobKindChartRender:    "chart_render_jobs",
}

// AgentJobKinds 返回已支持的作业类型（稳定顺序）。
func AgentJobKinds() []string {
	return []string{JobKindTenderParse, JobKindProposalExport, JobKindChartRender}
}

// ValidAgentJobKind 报告 kind 是否已实现订阅校验。
func ValidAgentJobKind(kind string) bool { _, ok := agentJobTables[kind]; return ok }

var (
	ErrNoIntentExecution  = errors.New("db: no host-bound intent execution")
	ErrJobKindUnsupported = errors.New("db: job kind is not supported")
)

// JobRef 是对外的作业引用；不同 kind 映射不同作业表，不以裸 ID 混查。
type JobRef struct {
	Kind string `json:"kind"`
	ID   int64  `json:"id"`
}

type AgentJobEvent struct {
	ID             int64           `json:"id"`
	TerminalStatus string          `json:"terminal_status"`
	BusinessState  string          `json:"business_state"`
	Payload        json.RawMessage `json:"payload"`
	State          string          `json:"state"`
	DeliveryEpoch  int64           `json:"delivery_epoch,omitempty"`
	DeliveredAt    *time.Time      `json:"delivered_at,omitempty"`
	AcknowledgedAt *time.Time      `json:"acknowledged_at,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

// AgentJobSubscription 是一条订阅及其作业的当前快照（作业字段读自作业表，不在订阅中冗余保存）。
type AgentJobSubscription struct {
	ID                 int64           `json:"id"`
	TaskID             int64           `json:"task_id"`
	IntentID           int64           `json:"intent_id"`
	CreatedIntentEpoch int64           `json:"created_intent_epoch"`
	Job                JobRef          `json:"job_ref"`
	TargetRef          json.RawMessage `json:"target_ref"`
	ExpectedVersion    *int64          `json:"expected_version,omitempty"`
	Required           bool            `json:"required"`
	State              string          `json:"state"`
	CreatedBy          string          `json:"created_by"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
	JobStatus          string          `json:"job_status"`
	JobAttempts        int             `json:"job_attempts"`
	JobMaxAttempts     int             `json:"job_max_attempts"`
	JobDeadlineAt      *time.Time      `json:"job_deadline_at,omitempty"`
	JobError           string          `json:"job_error,omitempty"`
	Event              *AgentJobEvent  `json:"event,omitempty"`
}

// SubscribeAgentJobInput 只接收受管作业引用；intent_id/epoch 来自宿主 Context。
type SubscribeAgentJobInput struct {
	Job             JobRef
	TargetRef       any
	ExpectedVersion *int64
	Actor           string
}

func intentExecutionFrom(ctx context.Context) (intentExecution, bool) {
	p, ok := ctx.Value(intentExecutionKey{}).(intentExecution)
	return p, ok && p.ID > 0
}

// IntentExecutionBound 报告 ctx 是否带有宿主注入的 Intent 执行身份（Tool 提交据此强制订阅当前 Intent）。
func IntentExecutionBound(ctx context.Context) bool {
	_, ok := intentExecutionFrom(ctx)
	return ok
}

// LockSubmittingIntent 锁定宿主绑定且持有活租约的 Intent（任务未暂停、未终态）。
// 提交路径在作业等业务锁之前调用；需要任务锁时先锁任务行。
func (d *DB) LockSubmittingIntent(ctx context.Context, taskID int64) (intentID, expID, epoch int64, err error) {
	p, ok := intentExecutionFrom(ctx)
	if !ok {
		return 0, 0, 0, ErrNoIntentExecution
	}
	err = d.QueryRowContext(ctx, `SELECT n.id, n.exploration_id FROM exploration_nodes n JOIN tasks t ON t.exploration_id = n.exploration_id
		WHERE n.id = $1 AND n.kind = 'intent' AND n.lease_epoch = $2 AND n.state = 'running' AND n.lease_until > now()
		  AND t.id = $3 AND NOT t.paused AND t.status NOT IN ('done','failed','timeout') FOR UPDATE OF n`, p.ID, p.Epoch, taskID).Scan(&intentID, &expID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, 0, ErrLeaseLost
	}
	return intentID, expID, p.Epoch, err
}

// lockAgentJob 锁定任务内的作业行并返回状态；类型与租户/任务不符时视为不存在。
func (d *DB) lockAgentJob(ctx context.Context, tenant string, taskID int64, ref JobRef) (string, error) {
	table, ok := agentJobTables[ref.Kind]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrJobKindUnsupported, ref.Kind)
	}
	var status string
	err := d.QueryRowContext(ctx, `SELECT status FROM `+table+` WHERE tenant_id = $1 AND task_id = $2 AND id = $3 FOR UPDATE`,
		tenantOr(tenant), taskID, ref.ID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %s #%d", ErrNotFound, ref.Kind, ref.ID)
	}
	return status, err
}

// SubscribeAgentJob 把作业绑定到宿主注入的在途 Intent，并入调用方事务（提交作业与订阅同事务）。
// 锁作业行后读状态：未终态建立 waiting，已终态建立 notified + pending 事件，避免“先完成后订阅”丢通知。
// 相同 Intent 重复订阅同一作业返回原订阅。
func (d *DB) SubscribeAgentJob(ctx context.Context, tenant string, taskID int64, in SubscribeAgentJobInput) (*AgentJobSubscription, error) {
	tenant = tenantOr(tenant)
	if !ValidAgentJobKind(in.Job.Kind) {
		return nil, fmt.Errorf("%w: %q", ErrJobKindUnsupported, in.Job.Kind)
	}
	if in.Job.ID <= 0 {
		return nil, fmt.Errorf("%w: job id must be positive", ErrNotFound)
	}
	if err := CheckJobKindScope(ctx, in.Job.Kind); err != nil {
		return nil, err
	}
	target, err := marshalPayload(in.TargetRef)
	if err != nil {
		return nil, err
	}
	var subID int64
	err = d.Transaction(ctx, func(tx *DB) error {
		intentID, expID, epoch, err := tx.LockSubmittingIntent(ctx, taskID)
		if err != nil {
			return err
		}
		status, err := tx.lockAgentJob(ctx, tenant, taskID, in.Job)
		if err != nil {
			return err
		}
		err = tx.QueryRowContext(ctx, `INSERT INTO agent_job_subscriptions(tenant_id, task_id, exploration_id, intent_id, created_intent_epoch,
				job_kind, job_id, target_ref, expected_version, created_by, updated_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10, $10)
			ON CONFLICT (tenant_id, intent_id, job_kind, job_id) DO NOTHING RETURNING id`,
			tenant, taskID, expID, intentID, epoch, in.Job.Kind, in.Job.ID, string(target), in.ExpectedVersion, in.Actor).Scan(&subID)
		if errors.Is(err, sql.ErrNoRows) {
			return tx.QueryRowContext(ctx, `SELECT id FROM agent_job_subscriptions WHERE tenant_id = $1 AND intent_id = $2 AND job_kind = $3 AND job_id = $4`,
				tenant, intentID, in.Job.Kind, in.Job.ID).Scan(&subID)
		}
		if err != nil || !isTerminalJobStatus(status) {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE agent_job_subscriptions SET state = 'notified', updated_by = $2 WHERE id = $1 AND state = 'waiting'`, subID, in.Actor); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO agent_job_events(tenant_id, subscription_id, terminal_status, business_state, payload, created_by, updated_by)
			SELECT $1, $2, o.terminal_status, o.business_state, o.payload, $5, $5 FROM agent_job_outcome($3, $4) o WHERE o.terminal_status IS NOT NULL
			ON CONFLICT (subscription_id) DO NOTHING`, tenant, subID, in.Job.Kind, in.Job.ID, in.Actor)
		return err
	})
	if err != nil {
		return nil, err
	}
	subs, err := d.ListAgentJobs(context.WithoutCancel(ctx), tenant, taskID, AgentJobFilter{SubscriptionID: subID, Limit: 1, Unscoped: true})
	if err != nil {
		return nil, err
	}
	if len(subs) == 0 {
		return nil, fmt.Errorf("%w: subscription %d", ErrNotFound, subID)
	}
	return subs[0], nil
}

func isTerminalJobStatus(s string) bool {
	return s == "succeeded" || s == "failed" || s == "cancelled" || s == ChartJobSuperseded
}

// AgentJobFilter 过滤订阅列表。宿主绑定 Intent 的执行只能读本 Intent 的订阅（Unscoped 仅供存储层内部使用）。
type AgentJobFilter struct {
	IntentID       int64
	Job            JobRef
	SubscriptionID int64
	Before         int64
	Limit          int
	Unscoped       bool
}

const agentJobCols = `s.id, s.task_id, s.intent_id, s.created_intent_epoch, s.job_kind, s.job_id, s.target_ref, s.expected_version, s.required,
	s.state, s.created_by, s.created_at, s.updated_at,
	COALESCE(f.status, x.status, c.status, ''), COALESCE(f.attempts, x.attempts, c.attempts, 0),
	COALESCE(f.max_attempts, x.max_attempts, c.max_attempts, 0), COALESCE(f.deadline_at, c.deadline_at),
	COALESCE(f.error, x.error, c.error, ''),
	e.id, e.terminal_status, e.business_state, e.payload, e.state, e.delivery_epoch, e.delivered_at, e.acknowledged_at, e.created_at`

const agentJobFrom = ` FROM agent_job_subscriptions s
	LEFT JOIN fixed_file_jobs f ON s.job_kind = 'tender_parse' AND f.tenant_id = s.tenant_id AND f.task_id = s.task_id AND f.id = s.job_id
	LEFT JOIN export_jobs x ON s.job_kind = 'proposal_export' AND x.tenant_id = s.tenant_id AND x.task_id = s.task_id AND x.id = s.job_id
	LEFT JOIN chart_render_jobs c ON s.job_kind = 'chart_render' AND c.tenant_id = s.tenant_id AND c.task_id = s.task_id AND c.id = s.job_id
	LEFT JOIN agent_job_events e ON e.subscription_id = s.id`

func scanAgentJob(r rowScanner) (*AgentJobSubscription, error) {
	var (
		s                       AgentJobSubscription
		target                  []byte
		expected                sql.NullInt64
		deadline                sql.NullTime
		evID, evEpoch           sql.NullInt64
		evStatus, evBiz, evStat sql.NullString
		evPayload               []byte
		evDelivered, evAcked    sql.NullTime
		evCreated               sql.NullTime
	)
	if err := r.Scan(&s.ID, &s.TaskID, &s.IntentID, &s.CreatedIntentEpoch, &s.Job.Kind, &s.Job.ID, &target, &expected, &s.Required,
		&s.State, &s.CreatedBy, &s.CreatedAt, &s.UpdatedAt,
		&s.JobStatus, &s.JobAttempts, &s.JobMaxAttempts, &deadline, &s.JobError,
		&evID, &evStatus, &evBiz, &evPayload, &evStat, &evEpoch, &evDelivered, &evAcked, &evCreated); err != nil {
		return nil, err
	}
	s.TargetRef = json.RawMessage(target)
	if expected.Valid {
		s.ExpectedVersion = &expected.Int64
	}
	s.JobDeadlineAt = nullTimePtr(deadline)
	if evID.Valid {
		s.Event = &AgentJobEvent{ID: evID.Int64, TerminalStatus: evStatus.String, BusinessState: evBiz.String, Payload: json.RawMessage(evPayload),
			State: evStat.String, DeliveryEpoch: evEpoch.Int64, DeliveredAt: nullTimePtr(evDelivered), AcknowledgedAt: nullTimePtr(evAcked), CreatedAt: evCreated.Time}
	}
	return &s, nil
}

// ListAgentJobs 读取任务内的订阅及作业快照（新者在前，Before 为订阅 ID 游标）。
// 宿主绑定 Intent 的执行强制只读本 Intent，查询其他 Intent 返回范围错误；作业类型同样受宿主范围约束。
func (d *DB) ListAgentJobs(ctx context.Context, tenant string, taskID int64, f AgentJobFilter) ([]*AgentJobSubscription, error) {
	if !f.Unscoped {
		if p, ok := intentExecutionFrom(ctx); ok {
			if f.IntentID != 0 && f.IntentID != p.ID {
				return nil, fmt.Errorf("execution scope: intent %d is not the current intent", f.IntentID)
			}
			f.IntentID = p.ID
		}
		if f.Job.Kind != "" {
			if err := CheckJobKindScope(ctx, f.Job.Kind); err != nil {
				return nil, err
			}
		}
	}
	if f.Job.Kind != "" && !ValidAgentJobKind(f.Job.Kind) {
		return nil, fmt.Errorf("%w: %q", ErrJobKindUnsupported, f.Job.Kind)
	}
	if f.Limit <= 0 || f.Limit > 100 {
		f.Limit = 50
	}
	rows, err := d.QueryContext(ctx, `SELECT `+agentJobCols+agentJobFrom+`
		WHERE s.tenant_id = $1 AND s.task_id = $2 AND ($3::bigint = 0 OR s.intent_id = $3) AND ($4::text = '' OR s.job_kind = $4)
		  AND ($5::bigint = 0 OR s.job_id = $5) AND ($6::bigint = 0 OR s.id = $6) AND ($7::bigint = 0 OR s.id < $7)
		ORDER BY s.id DESC LIMIT $8`, tenantOr(tenant), taskID, f.IntentID, f.Job.Kind, f.Job.ID, f.SubscriptionID, f.Before, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*AgentJobSubscription{}
	for rows.Next() {
		s, err := scanAgentJob(rows)
		if err != nil {
			return nil, err
		}
		if !f.Unscoped && CheckJobKindScope(ctx, s.Job.Kind) != nil {
			continue
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// AgentJobSnapshot 是没有订阅时的作业状态（如人工从页面提交的作业）。
type AgentJobSnapshot struct {
	Job         JobRef     `json:"job_ref"`
	Status      string     `json:"status"`
	Attempts    int        `json:"attempts"`
	MaxAttempts int        `json:"max_attempts"`
	DeadlineAt  *time.Time `json:"deadline_at,omitempty"`
	Error       string     `json:"error,omitempty"`
}

// GetAgentJobSnapshot 读取任务内一个作业的状态。宿主范围限制对象时，只能读本 Intent 已订阅的作业。
func (d *DB) GetAgentJobSnapshot(ctx context.Context, tenant string, taskID int64, ref JobRef) (*AgentJobSnapshot, error) {
	table, ok := agentJobTables[ref.Kind]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrJobKindUnsupported, ref.Kind)
	}
	if err := CheckJobKindScope(ctx, ref.Kind); err != nil {
		return nil, err
	}
	if err := d.checkJobOwnedByIntent(ctx, tenant, taskID, ref); err != nil {
		return nil, err
	}
	deadline := "NULL::timestamptz"
	if ref.Kind == JobKindTenderParse || ref.Kind == JobKindChartRender {
		deadline = "deadline_at"
	}
	snap := &AgentJobSnapshot{Job: ref}
	var dl sql.NullTime
	err := d.QueryRowContext(ctx, `SELECT status, attempts, max_attempts, `+deadline+`, error FROM `+table+` WHERE tenant_id = $1 AND task_id = $2 AND id = $3`,
		tenantOr(tenant), taskID, ref.ID).Scan(&snap.Status, &snap.Attempts, &snap.MaxAttempts, &dl, &snap.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s #%d", ErrNotFound, ref.Kind, ref.ID)
	}
	snap.DeadlineAt = nullTimePtr(dl)
	return snap, err
}

// HasAgentJobSubscriptions 报告 Intent 是否有未取消的持久化订阅（允许 RequireWriteback 释放本轮槽位，不代表完成）。
func (s *ExplorationStore) HasAgentJobSubscriptions(ctx context.Context, intentID int64) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agent_job_subscriptions WHERE exploration_id = $1 AND intent_id = $2 AND state <> 'cancelled')`,
		s.expID, intentID).Scan(&ok)
	return ok, err
}

// ---------------------------------------------------------------------------
// 事件投递与确认
// ---------------------------------------------------------------------------

// AgentJobDelivery 是一次投递给 Worker 的终态事件。
type AgentJobDelivery struct {
	EventID        int64           `json:"event_id"`
	SubscriptionID int64           `json:"subscription_id"`
	Job            JobRef          `json:"job_ref"`
	TargetRef      json.RawMessage `json:"target_ref"`
	TerminalStatus string          `json:"terminal_status"`
	BusinessState  string          `json:"business_state"`
	Payload        json.RawMessage `json:"payload"`
}

// TakeAgentJobEvent 按 Intent 当前 epoch 领取一个事件：pending，或旧 epoch 已投递未确认的（接管后重放）；
// 同一 epoch 不重复注入。领取不等于消费，确认见 FinalizeIntentTurn。
func (s *ExplorationStore) TakeAgentJobEvent(ctx context.Context, intentID, epoch int64) (*AgentJobDelivery, bool, error) {
	var (
		d       AgentJobDelivery
		target  []byte
		payload []byte
	)
	err := s.db.QueryRowContext(ctx, `WITH picked AS (
			SELECT e.id FROM agent_job_events e JOIN agent_job_subscriptions s ON s.id = e.subscription_id
			JOIN exploration_nodes n ON n.id = s.intent_id
			WHERE s.exploration_id = $1 AND s.intent_id = $2 AND (e.state = 'pending' OR (e.state = 'delivered' AND e.delivery_epoch <> $3))
			  AND n.state = 'running' AND n.lease_epoch = $3 AND n.lease_until > now()
			ORDER BY e.id LIMIT 1 FOR UPDATE OF e SKIP LOCKED)
		UPDATE agent_job_events e SET state = 'delivered', delivery_epoch = $3, delivered_at = now(), updated_by = 'worker'
		FROM picked, agent_job_subscriptions s
		WHERE e.id = picked.id AND s.id = e.subscription_id
		RETURNING e.id, s.id, s.job_kind, s.job_id, s.target_ref, e.terminal_status, e.business_state, e.payload`, s.expID, intentID, epoch).
		Scan(&d.EventID, &d.SubscriptionID, &d.Job.Kind, &d.Job.ID, &target, &d.TerminalStatus, &d.BusinessState, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	d.TargetRef, d.Payload = json.RawMessage(target), json.RawMessage(payload)
	return &d, true, nil
}

// AgentJobEventMarker 是写入 Worker 消息的事件标记；收尾时只确认已保存在 transcript 中的标记。
func AgentJobEventMarker(eventID int64) string { return fmt.Sprintf("后台作业事件 #%d", eventID) }

var agentJobMarkerRE = regexp.MustCompile(`后台作业事件 #(\d+)`)

// AgentJobEventIDsIn 从已保存的会话文本中提取事件标记 ID（去重、升序）。
func AgentJobEventIDsIn(text string) []int64 {
	var ids []int64
	for _, m := range agentJobMarkerRE.FindAllStringSubmatch(text, -1) {
		if id, err := strconv.ParseInt(m[1], 10, 64); err == nil && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// ---------------------------------------------------------------------------
// 原子回合收尾
// ---------------------------------------------------------------------------

// TurnOutcome 是 FinalizeIntentTurn 的统一决策。
type TurnOutcome string

const (
	TurnContinue TurnOutcome = "open"        // 尚有未消费的作业事件或人工输入：回到 open 继续
	TurnWaiting  TurnOutcome = "waiting_job" // 仅剩 waiting 订阅：blocked + waiting_job，释放执行槽
	TurnDone     TurnOutcome = "done"        // 真实产物校验通过
	TurnBlocked  TurnOutcome = "blocked"     // 交付物不满足（章节或必需作业），交由 Planner 重规划
)

// FinalizeIntentTurn 在一个事务内锁 Intent/活租约并统一决策本回合去向（替代先查后改）：
//  1. 只确认 saved 中、且本 epoch 已投递的作业事件；本 epoch 已投递但未保存的事件退回 pending 重放；
//  2. 有未消费事件或 Hint → open；仅剩 waiting 订阅 → blocked+waiting_job；
//  3. 否则校验章节与必需作业的真实产物：通过 → done（新 Hint 到达则 open），不通过 → blocked。
//
// 人工输入只在本回合不以失败告终时确认；失败时由触发器交回 Planner。detail 说明 TurnBlocked 的原因。
func (s *ExplorationStore) FinalizeIntentTurn(ctx context.Context, id, epoch int64, saved []int64) (outcome TurnOutcome, detail string, err error) {
	if saved == nil {
		saved = []int64{}
	}
	err = s.db.Transaction(ctx, func(tx *DB) error {
		var n Node
		var taskID int64
		err := tx.QueryRowContext(ctx, `SELECT n.id, n.payload, t.id FROM exploration_nodes n JOIN tasks t ON t.exploration_id = n.exploration_id
			WHERE n.id = $1 AND n.exploration_id = $2 AND n.state = 'running' AND n.lease_epoch = $3 AND n.lease_until > now() FOR UPDATE OF n`,
			id, s.expID, epoch).Scan(&n.ID, &n.Payload, &taskID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLeaseLost
		}
		if err != nil {
			return err
		}
		m := NodeMandate(&n)
		// 最终正文校验按大纲 → 章节加锁，先于订阅/事件。
		if m != nil {
			sids := uniqueIDs(m.RequiredSectionIDs)
			slices.Sort(sids)
			for _, sid := range sids {
				if _, _, _, err := sectionGateTx(ctx, tx.tx, DefaultTenant, taskID, sid); err != nil {
					return err
				}
			}
		}
		// 锁本 Intent 的全部订阅：与作业终态发布串行化。
		if _, err := tx.ExecContext(ctx, `SELECT id FROM agent_job_subscriptions WHERE intent_id = $1 ORDER BY id FOR UPDATE`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agent_job_events e SET state = 'acknowledged', acknowledged_at = now(), updated_by = 'worker'
			FROM agent_job_subscriptions s WHERE s.id = e.subscription_id AND s.intent_id = $1
			  AND e.state = 'delivered' AND e.delivery_epoch = $2 AND e.id = ANY($3::bigint[])`, id, epoch, saved); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agent_job_events e SET state = 'pending', updated_by = 'worker'
			FROM agent_job_subscriptions s WHERE s.id = e.subscription_id AND s.intent_id = $1
			  AND e.state = 'delivered' AND e.delivery_epoch = $2 AND NOT (e.id = ANY($3::bigint[]))`, id, epoch, saved); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agent_job_subscriptions s SET state = 'acknowledged', updated_by = 'worker'
			WHERE s.intent_id = $1 AND s.state = 'notified'
			  AND EXISTS(SELECT 1 FROM agent_job_events e WHERE e.subscription_id = s.id AND e.state = 'acknowledged')`, id); err != nil {
			return err
		}
		var pendingEvents, waiting bool
		if err := tx.QueryRowContext(ctx, `SELECT
				EXISTS(SELECT 1 FROM agent_job_events e JOIN agent_job_subscriptions s ON s.id = e.subscription_id
				       WHERE s.intent_id = $1 AND e.state IN ('pending','delivered')),
				EXISTS(SELECT 1 FROM agent_job_subscriptions WHERE intent_id = $1 AND state = 'waiting')`, id).Scan(&pendingEvents, &waiting); err != nil {
			return err
		}
		es := NewExplorationStore(tx, s.expID)
		switch {
		case pendingEvents:
			outcome = TurnContinue
		case waiting:
			outcome = TurnWaiting
		default:
			if derr := tx.CheckIntentDeliverables(ctx, taskID, &n); derr != nil {
				outcome, detail = TurnBlocked, derr.Error()
				return tx.blockIntentTx(ctx, id, "")
			}
			if jerr := tx.checkJobDeliverables(ctx, taskID, &n, m); jerr != nil {
				outcome, detail = TurnBlocked, jerr.Error()
				return tx.blockIntentTx(ctx, id, BlockedReasonRequiredJobFailed)
			}
			outcome = TurnDone
		}
		if err := es.AcknowledgeIntentInputs(ctx, id, epoch); err != nil {
			return err
		}
		switch outcome {
		case TurnWaiting:
			_, err = tx.ExecContext(ctx, `UPDATE exploration_nodes SET state = 'blocked', blocked_reason = $2, lease_until = NULL,
				completed_at = NULL, content_version = content_version + 1 WHERE id = $1`, id, BlockedReasonWaitingJob)
		case TurnContinue:
			_, err = tx.ExecContext(ctx, `UPDATE exploration_nodes SET state = 'open', blocked_reason = NULL, lease_until = NULL,
				completed_at = NULL, content_version = content_version + 1 WHERE id = $1`, id)
		default:
			var state string
			err = tx.QueryRowContext(ctx, `UPDATE exploration_nodes n SET
					state = CASE WHEN EXISTS(SELECT 1 FROM intent_inputs i WHERE i.intent_id = n.id AND i.state IN ('pending','delivered')) THEN 'open' ELSE 'done' END,
					blocked_reason = NULL, lease_until = NULL, content_version = content_version + 1,
					completed_at = CASE WHEN EXISTS(SELECT 1 FROM intent_inputs i WHERE i.intent_id = n.id AND i.state IN ('pending','delivered')) THEN NULL ELSE now() END
				WHERE id = $1 RETURNING state`, id).Scan(&state)
			if state == StateIntentOpen {
				outcome = TurnContinue
			}
		}
		return err
	})
	if err != nil {
		return "", "", err
	}
	return outcome, detail, nil
}

func (d *DB) blockIntentTx(ctx context.Context, id int64, reason string) error {
	_, err := d.ExecContext(ctx, `UPDATE exploration_nodes SET state = 'blocked', blocked_reason = NULLIF($2, ''), lease_until = NULL,
		completed_at = now(), content_version = content_version + 1 WHERE id = $1`, id, reason)
	return err
}

// checkJobDeliverables：每个必需订阅都须成功且结果仍有效；mandate.required_job_kinds 每类至少一个。
// ACK 失败事件不代表工作成功。只读作业与结果表，不锁业务作业。
func (d *DB) checkJobDeliverables(ctx context.Context, taskID int64, n *Node, m *IntentMandate) error {
	rows, err := d.QueryContext(ctx, `SELECT s.job_kind, s.job_id, s.required, s.state, COALESCE(e.terminal_status, '')
		FROM agent_job_subscriptions s LEFT JOIN agent_job_events e ON e.subscription_id = s.id
		WHERE s.intent_id = $1 AND s.state <> 'cancelled' ORDER BY s.id`, n.ID)
	if err != nil {
		return err
	}
	type sub struct {
		ref      JobRef
		required bool
		state    string
		status   string
	}
	var subs []sub
	for rows.Next() {
		var x sub
		if err := rows.Scan(&x.ref.Kind, &x.ref.ID, &x.required, &x.state, &x.status); err != nil {
			rows.Close()
			return err
		}
		subs = append(subs, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	succeeded := map[string]bool{}
	for _, x := range subs {
		ok := x.status == "succeeded"
		if ok {
			if ok, err = d.agentJobResultValid(ctx, taskID, x.ref); err != nil {
				return err
			}
		}
		if ok {
			succeeded[x.ref.Kind] = true
			continue
		}
		if x.required {
			if x.status == "" {
				return fmt.Errorf("required job %s #%d has no terminal event (%s)", x.ref.Kind, x.ref.ID, x.state)
			}
			if x.status == "succeeded" {
				return fmt.Errorf("required job %s #%d result is no longer valid", x.ref.Kind, x.ref.ID)
			}
			return fmt.Errorf("required job %s #%d ended %s", x.ref.Kind, x.ref.ID, x.status)
		}
	}
	if m != nil {
		for _, k := range m.RequiredJobKinds {
			if !succeeded[k] {
				return fmt.Errorf("required job kind %s has no successful result for this intent", k)
			}
		}
	}
	return nil
}

// agentJobResultValid 校验成功作业的结果版本仍有效：解析候选就绪且未被新解析修订取代（或已采用）；导出产物已登记；
// 图表与作业源码一致、绑定章节当前修订且有真实产物（PNG 已登记哈希，或设备表原生表格）。
// 严格最终稿策略（S4）落地前，导出结果一律按 preview_only 对待。
func (d *DB) agentJobResultValid(ctx context.Context, taskID int64, ref JobRef) (bool, error) {
	var ok bool
	var err error
	switch ref.Kind {
	case JobKindTenderParse:
		err = d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM fixed_file_jobs j JOIN file_processing_records r ON r.id = j.record_id
			LEFT JOIN tender_documents td ON td.id = r.target_document_id
			WHERE j.id = $1 AND j.task_id = $2 AND j.status = 'succeeded' AND r.source_hash = j.source_hash
			  AND (r.status = 'adopted' OR (r.status = 'ready' AND (r.operation = 'import' OR td.parse_revision = r.expected_revision))))`,
			ref.ID, taskID).Scan(&ok)
	case JobKindProposalExport:
		err = d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM export_jobs WHERE id = $1 AND task_id = $2 AND status = 'succeeded' AND artifact_hash <> '')`,
			ref.ID, taskID).Scan(&ok)
	case JobKindChartRender:
		err = d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM chart_render_jobs j
			JOIN proposal_charts c ON c.tenant_id = j.tenant_id AND c.task_id = j.task_id AND c.id = j.chart_id
			JOIN proposal_sections s ON s.tenant_id = c.tenant_id AND s.id = c.section_id
			WHERE j.id = $1 AND j.task_id = $2 AND j.status = 'succeeded' AND c.source_hash = j.source_hash AND c.rev_no = s.current_rev_no
			  AND c.render_status = 'rendered' AND (c.render_format = 'native' OR (c.render_format = 'png' AND c.artifact_hash <> '')))`,
			ref.ID, taskID).Scan(&ok)
	default:
		return false, fmt.Errorf("%w: %q", ErrJobKindUnsupported, ref.Kind)
	}
	return ok, err
}

// ---------------------------------------------------------------------------
// 唤醒与生命周期
// ---------------------------------------------------------------------------

// WokenIntent 是被作业事件重新置为 open 的 Intent。
type WokenIntent struct {
	ExplorationID int64
	IntentID      int64
}

// WakeWaitingIntents 扫描有未消费作业事件的 blocked+waiting_job Intent，逐个以独立短事务（先锁 Intent）重开。
// 暂停或终态任务不派发；done/stopped/手动暂停/其他阻断不会被复活。允许部分作业先完成先处理。
func (d *DB) WakeWaitingIntents(ctx context.Context, limit int) ([]WokenIntent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.QueryContext(ctx, `SELECT n.id FROM exploration_nodes n JOIN tasks t ON t.exploration_id = n.exploration_id
		WHERE n.kind = 'intent' AND n.state = 'blocked' AND n.blocked_reason = 'waiting_job'
		  AND NOT t.paused AND t.status NOT IN ('done','failed','timeout')
		  AND EXISTS(SELECT 1 FROM agent_job_subscriptions s JOIN agent_job_events e ON e.subscription_id = s.id
		             WHERE s.intent_id = n.id AND e.state IN ('pending','delivered'))
		ORDER BY n.id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []WokenIntent
	for _, id := range ids {
		var w WokenIntent
		err := d.Transaction(ctx, func(tx *DB) error {
			err := tx.QueryRowContext(ctx, `SELECT n.exploration_id FROM exploration_nodes n JOIN tasks t ON t.exploration_id = n.exploration_id
				WHERE n.id = $1 AND n.kind = 'intent' AND n.state = 'blocked' AND n.blocked_reason = 'waiting_job'
				  AND NOT t.paused AND t.status NOT IN ('done','failed','timeout') FOR UPDATE OF n`, id).Scan(&w.ExplorationID)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			var pending bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agent_job_subscriptions s JOIN agent_job_events e ON e.subscription_id = s.id
				WHERE s.intent_id = $1 AND e.state IN ('pending','delivered'))`, id).Scan(&pending); err != nil || !pending {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE exploration_nodes SET state = 'open', blocked_reason = NULL, owner = NULL, lease_until = NULL,
				completed_at = NULL, content_version = content_version + 1 WHERE id = $1`, id); err != nil {
				return err
			}
			w.IntentID = id
			return nil
		})
		if err != nil {
			return out, err
		}
		if w.IntentID > 0 {
			out = append(out, w)
		}
	}
	return out, nil
}

// SettleWaitingIntents 在任务进入终态时结束本空间的等待中 Intent（目标达成 → stopped，超时 → exhausted），
// 取消其 waiting 订阅并使未消费事件过期；返回结束的 Intent 数。
func (s *ExplorationStore) SettleWaitingIntents(ctx context.Context, state string) (int64, error) {
	if state != StateIntentStopped && state != StateIntentExhausted {
		return 0, fmt.Errorf("db: waiting intents settle to stopped or exhausted, not %q", state)
	}
	var n int64
	err := s.db.Transaction(ctx, func(tx *DB) error {
		rows, err := tx.QueryContext(ctx, `UPDATE exploration_nodes SET state = $2, blocked_reason = NULL, lease_until = NULL,
				completed_at = now(), content_version = content_version + 1
			WHERE exploration_id = $1 AND kind = 'intent' AND state = 'blocked' AND blocked_reason = 'waiting_job' RETURNING id`, s.expID, state)
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil || len(ids) == 0 {
			return err
		}
		n = int64(len(ids))
		if _, err := tx.ExecContext(ctx, `UPDATE agent_job_subscriptions SET state = 'cancelled', updated_by = 'task-settle'
			WHERE intent_id = ANY($1::bigint[]) AND state IN ('waiting','notified')`, ids); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE agent_job_events e SET state = 'expired', updated_by = 'task-settle'
			FROM agent_job_subscriptions s WHERE s.id = e.subscription_id AND s.intent_id = ANY($1::bigint[]) AND e.state IN ('pending','delivered')`, ids)
		return err
	})
	return n, err
}

// WaitingIntents 读取 blocked+waiting_job 的 Intent（id 升序）。
func (s *ExplorationStore) WaitingIntents(ctx context.Context, limit int) ([]*Node, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+nodeCols+` FROM exploration_nodes
		WHERE exploration_id = $1 AND kind = 'intent' AND state = 'blocked' AND blocked_reason = 'waiting_job'
		ORDER BY id ASC LIMIT $2`, s.expID, limit)
	if err != nil {
		return nil, err
	}
	return scanNodes(rows)
}

// IsWaitingIntent 报告节点是否处于 blocked + waiting_job。
func IsWaitingIntent(n *Node) bool {
	return n != nil && n.Kind == KindIntent && n.State == StateIntentBlocked && n.BlockedReason == BlockedReasonWaitingJob
}
