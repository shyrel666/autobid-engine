package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Task struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	Description    string `json:"description,omitempty"`
	ExplorationID  int64  `json:"exploration_id"`
	Status         string `json:"status"`
	Paused         bool   `json:"paused"`
	Queued         bool   `json:"queued"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	// PlanHeartbeatSeconds 为 Planner 兜底心跳；执行层对 <MinPlanHeartbeatSeconds 的值取默认。
	PlanHeartbeatSeconds int        `json:"plan_heartbeat_seconds"`
	FirstRunAt           *time.Time `json:"first_run_at,omitempty"`
	DeadlineAt           *time.Time `json:"deadline_at,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

// TaskScope 声明任务覆盖的招标项目/标段；LotNo 为空表示整个项目。
type TaskScope struct {
	TenderNo string
	LotNo    string
}

type CreateTaskInput struct {
	Name           string
	Description    string
	Goal           string // 推演空间总目标，同时作为 origin Fact 的 summary
	TimeoutSeconds int
	// PlanHeartbeatSeconds<=0 时使用库默认 600。
	PlanHeartbeatSeconds int
	Scopes               []TaskScope
	SourceTaskIDs        []int64 // 显式直接来源任务
}

const taskCols = `id, name, description, exploration_id, status, paused, queued, timeout_seconds,
	plan_heartbeat_seconds, first_run_at, deadline_at, created_at, updated_at`

func scanTask(r rowScanner) (*Task, error) {
	var (
		t               Task
		firstRun, deadl sql.NullTime
	)
	if err := r.Scan(&t.ID, &t.Name, &t.Description, &t.ExplorationID, &t.Status, &t.Paused, &t.Queued,
		&t.TimeoutSeconds, &t.PlanHeartbeatSeconds, &firstRun, &deadl, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	t.FirstRunAt, t.DeadlineAt = nullTimePtr(firstRun), nullTimePtr(deadl)
	return &t, nil
}

// CreateTask 单事务创建「推演空间 -> 任务 -> origin Fact -> 声明范围 -> 直接来源关系」。
func (d *DB) CreateTask(ctx context.Context, in CreateTaskInput) (*Task, error) {
	if strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.Goal) == "" {
		return nil, errors.New("db: task name and goal are required")
	}
	var task *Task
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		var expID int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO explorations(description, goal)
			VALUES (NULLIF($1, ''), $2) RETURNING id`, in.Description, in.Goal).Scan(&expID); err != nil {
			return err
		}
		t, err := scanTask(tx.QueryRowContext(ctx, `INSERT INTO tasks(name, description, exploration_id, timeout_seconds, plan_heartbeat_seconds)
			VALUES ($1, $2, $3, $4, CASE WHEN $5::int > 0 THEN $5::int ELSE 600 END) RETURNING `+taskCols,
			in.Name, in.Description, expID, in.TimeoutSeconds, in.PlanHeartbeatSeconds))
		if err != nil {
			return err
		}
		es := &ExplorationStore{db: d, expID: expID}
		if _, err := es.insertNodeTx(ctx, tx, KindFact, StateOrigin, map[string]any{"summary": in.Goal}, 0, "", nil); err != nil {
			return err
		}
		for _, sc := range in.Scopes {
			if _, err := tx.ExecContext(ctx, `INSERT INTO task_scopes(task_id, tender_no, lot_no, source)
				VALUES ($1, $2, $3, 'explicit')
				ON CONFLICT (task_id, tender_no, lot_no) DO UPDATE SET source = 'explicit'`,
				t.ID, strings.TrimSpace(sc.TenderNo), strings.TrimSpace(sc.LotNo)); err != nil {
				return err
			}
		}
		for _, src := range in.SourceTaskIDs {
			if err := addTaskRelationTx(ctx, tx, t.ID, src); err != nil {
				return err
			}
		}
		task = t
		return nil
	})
	return task, err
}

func (d *DB) GetTask(ctx context.Context, id int64) (*Task, error) {
	t, err := scanTask(d.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: task %d", ErrNotFound, id)
	}
	return t, err
}

// AddTaskRelation 声明 sourceTaskID 为 taskID 的直接来源；重复声明幂等。
func (d *DB) AddTaskRelation(ctx context.Context, taskID, sourceTaskID int64) error {
	return d.InTx(ctx, func(tx *sql.Tx) error {
		return addTaskRelationTx(ctx, tx, taskID, sourceTaskID)
	})
}

func addTaskRelationTx(ctx context.Context, tx *sql.Tx, taskID, sourceTaskID int64) error {
	if taskID == sourceTaskID {
		return fmt.Errorf("db: task %d cannot be its own source", taskID)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO task_relations(task_id, source_task_id) VALUES ($1, $2)
		ON CONFLICT (task_id, source_task_id) DO NOTHING`, taskID, sourceTaskID)
	return err
}

// CompareAndSetTaskStatus 仅当当前状态为 expected 时迁移，用于防止终态被后台回调覆盖。
func (d *DB) CompareAndSetTaskStatus(ctx context.Context, id int64, expected, status string) (bool, error) {
	res, err := d.ExecContext(ctx, `UPDATE tasks SET status = $1 WHERE id = $2 AND status = $3`, status, id, expected)
	return affectedOne(res, err)
}

// Coverage 是 TaskCoverageWithSources 的口径（§1.4）：
// target = 上下文任务声明范围匹配实体 ∪ 上下文任务推演节点锚定实体；tested = target ∩ kind='fact' 节点锚定实体。
// 与原仓一致，分子不过滤 fact 的 state，origin/dismissed Fact 有锚点也会计入。
type Coverage struct {
	Target int64    `json:"target"`
	Tested int64    `json:"tested"`
	Pct    *float64 `json:"pct"` // Target=0 时为 nil
}

// TaskCoverageWithSources 以当前任务及直接来源任务计算覆盖率。
func (d *DB) TaskCoverageWithSources(ctx context.Context, taskID int64) (Coverage, error) {
	var c Coverage
	err := d.QueryRowContext(ctx, `WITH `+contextTasksCTE+`,
		ctx_exps AS (
			SELECT t.exploration_id FROM tasks t WHERE t.id IN (SELECT id FROM ctx_tasks)
		),
		scoped AS (
			SELECT a.id FROM assets a
			WHERE EXISTS (
				SELECT 1 FROM task_scopes sc
				WHERE sc.task_id IN (SELECT id FROM ctx_tasks) AND `+scopeMatch+`)
		),
		anchored AS (
			SELECT an.asset_id AS id, n.kind
			FROM exploration_anchors an
			JOIN exploration_nodes n ON n.id = an.node_id
			WHERE n.exploration_id IN (SELECT exploration_id FROM ctx_exps)
		),
		target AS (
			SELECT id FROM scoped UNION SELECT id FROM anchored
		)
		SELECT
			(SELECT COUNT(*) FROM target),
			(SELECT COUNT(*) FROM target WHERE id IN (SELECT id FROM anchored WHERE kind = 'fact'))`,
		taskID).Scan(&c.Target, &c.Tested)
	if err != nil {
		return c, err
	}
	if c.Target > 0 {
		pct := float64(c.Tested) / float64(c.Target)
		c.Pct = &pct
	}
	return c, nil
}
