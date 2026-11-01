package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

type Node struct {
	FindingID      int64           `json:"finding_id,omitempty"`
	ID             int64           `json:"id"`
	ExplorationID  int64           `json:"exploration_id"`
	Kind           string          `json:"kind"`
	Payload        json.RawMessage `json:"payload"`
	Priority       int             `json:"priority"`
	State          string          `json:"state"`
	Origin         string          `json:"origin,omitempty"`
	Owner          string          `json:"owner,omitempty"`
	BlockedReason  string          `json:"blocked_reason,omitempty"`
	DeleteReason   string          `json:"delete_reason,omitempty"`
	ContentVersion int             `json:"content_version"`
	ActiveRound    int64           `json:"active_round"`
	ColdSinceRound *int64          `json:"cold_since_round,omitempty"`
	LeaseUntil     *time.Time      `json:"lease_until,omitempty"`
	LeaseEpoch     int64           `json:"lease_epoch,omitempty"`
	Anchors        []int64         `json:"anchors,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	CompletedAt    *time.Time      `json:"completed_at,omitempty"`
	SourceTaskID   int64           `json:"source_task_id,omitempty"`
	Inherited      bool            `json:"inherited,omitempty"`
}

type Edge struct {
	From int64
	Rel  string
	To   int64
}

// ExplorationStore 以 expID 过滤落实单任务推演空间隔离（§1.1）。
type ExplorationStore struct {
	db    *DB
	expID int64
}

func NewExplorationStore(d *DB, expID int64) *ExplorationStore {
	return &ExplorationStore{db: d, expID: expID}
}

func (s *ExplorationStore) ID() int64 { return s.expID }

func (s *ExplorationStore) DB() *DB { return s.db }

const nodeCols = `id, exploration_id, kind, payload, priority, state,
	COALESCE(origin,''), COALESCE(owner,''), COALESCE(blocked_reason,''), COALESCE(delete_reason,''),
	content_version, active_round, cold_since_round, lease_until, lease_epoch, created_at, updated_at, completed_at`

func scanNode(r rowScanner) (*Node, error) {
	var (
		n         Node
		payload   []byte
		lease     sql.NullTime
		completed sql.NullTime
		cold      sql.NullInt64
	)
	if err := r.Scan(&n.ID, &n.ExplorationID, &n.Kind, &payload, &n.Priority, &n.State,
		&n.Origin, &n.Owner, &n.BlockedReason, &n.DeleteReason,
		&n.ContentVersion, &n.ActiveRound, &cold, &lease, &n.LeaseEpoch, &n.CreatedAt, &n.UpdatedAt, &completed); err != nil {
		return nil, err
	}
	n.Payload = json.RawMessage(payload)
	n.LeaseUntil = nullTimePtr(lease)
	n.CompletedAt = nullTimePtr(completed)
	if cold.Valid {
		n.ColdSinceRound = &cold.Int64
	}
	return &n, nil
}

func scanNodes(rows *sql.Rows) ([]*Node, error) {
	defer rows.Close()
	var out []*Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func marshalPayload(payload any) ([]byte, error) {
	switch p := payload.(type) {
	case nil:
		return []byte("{}"), nil
	case json.RawMessage:
		if len(p) == 0 {
			return []byte("{}"), nil
		}
		if !json.Valid(p) {
			return nil, errors.New("db: payload is not valid JSON")
		}
		return p, nil
	case []byte:
		return marshalPayload(json.RawMessage(p))
	default:
		return json.Marshal(p)
	}
}

// insertNodeTx 插入节点及初始锚点；任一外键失败由调用方事务整体回滚。
func (s *ExplorationStore) insertNodeTx(ctx context.Context, tx *sql.Tx, kind, state string, payload any, priority int, owner string, anchors []int64) (int64, error) {
	raw, err := marshalPayload(payload)
	if err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO exploration_nodes(exploration_id, kind, payload, priority, state, owner)
		VALUES ($1, $2, $3::jsonb, $4, $5, NULLIF($6, ''))
		RETURNING id`, s.expID, kind, string(raw), priority, state, owner).Scan(&id); err != nil {
		return 0, err
	}
	if err := anchorTx(ctx, tx, id, anchors); err != nil {
		return 0, err
	}
	return id, nil
}

func anchorTx(ctx context.Context, tx *sql.Tx, nodeID int64, assetIDs []int64) error {
	for _, aid := range assetIDs {
		if aid <= 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO exploration_anchors(node_id, asset_id) VALUES ($1, $2)
			ON CONFLICT (node_id, asset_id) DO NOTHING`, nodeID, aid); err != nil {
			return err
		}
	}
	return nil
}

// AddNode 在一个事务里插入节点及初始锚点（§1.4）。
func (s *ExplorationStore) AddNode(ctx context.Context, kind, state string, payload any, priority int, owner string, anchors []int64) (id int64, err error) {
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		id, err = s.insertNodeTx(ctx, tx, kind, state, payload, priority, owner, anchors)
		return err
	})
	return id, err
}

// Anchor 为已存在的本空间节点补锚；非正 ID 跳过，重复锚点忽略。
func (s *ExplorationStore) Anchor(ctx context.Context, nodeID int64, assetIDs ...int64) error {
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.nodeKindTx(ctx, tx, nodeID, false); err != nil {
			return err
		}
		return anchorTx(ctx, tx, nodeID, assetIDs)
	})
}

// edgeRule 是 v2 新增的 rel × kind 端点矩阵；srcCrossSpace 表示源端点可来自直接来源任务（§1.1 的显式例外）。
type edgeRule struct {
	src, dst      []string // dst 为空表示任意 kind
	srcCrossSpace bool
}

var edgeRules = map[string]edgeRule{
	RelSpawns:      {src: []string{KindFact, KindBegin}, dst: []string{KindGoal}},
	RelDerivedFrom: {src: []string{KindFact, KindFinding}, dst: []string{KindIntent}, srcCrossSpace: true},
	RelYields:      {src: []string{KindIntent}, dst: []string{KindFact, KindFinding}},
	RelProves:      {src: []string{KindFact, KindFinding}, dst: []string{KindGoal}, srcCrossSpace: true},
	RelCovers:      {src: []string{KindDigest}},
}

// nodeKindTx 读取节点 kind；allowSources 时允许节点属于直接来源任务的推演空间。
func (s *ExplorationStore) nodeKindTx(ctx context.Context, tx *sql.Tx, id int64, allowSources bool) (string, error) {
	if id <= 0 {
		return "", fmt.Errorf("%w: node %d", ErrNotFound, id)
	}
	q := `
		SELECT n.kind FROM exploration_nodes n
		WHERE n.id = $1 AND (n.exploration_id = $2 OR ($3::boolean AND n.exploration_id IN (
			SELECT st.exploration_id
			FROM tasks t
			JOIN task_relations r ON r.task_id = t.id
			JOIN tasks st ON st.id = r.source_task_id
			WHERE t.exploration_id = $2)))`
	var kind string
	err := tx.QueryRowContext(ctx, q, id, s.expID, allowSources).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: node %d in exploration %d", ErrForeignNode, id, s.expID)
	}
	return kind, err
}

// linkTx 在调用方事务内校验端点归属与 kind 矩阵后写边；重复边幂等。
func (s *ExplorationStore) linkTx(ctx context.Context, tx *sql.Tx, src int64, rel string, dst int64) error {
	rule, ok := edgeRules[rel]
	if !ok {
		return fmt.Errorf("%w: unknown rel %q", ErrInvalidEdge, rel)
	}
	if src == dst {
		return fmt.Errorf("%w: self loop on %d", ErrInvalidEdge, src)
	}
	srcKind, err := s.nodeKindTx(ctx, tx, src, rule.srcCrossSpace)
	if err != nil {
		return err
	}
	dstKind, err := s.nodeKindTx(ctx, tx, dst, false)
	if err != nil {
		return err
	}
	if !slices.Contains(rule.src, srcKind) || (len(rule.dst) > 0 && !slices.Contains(rule.dst, dstKind)) {
		return fmt.Errorf("%w: %s(%d) -[%s]-> %s(%d)", ErrInvalidEdge, srcKind, src, rel, dstKind, dst)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO exploration_edges(exploration_id, src_id, rel, dst_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (exploration_id, src_id, rel, dst_id) DO NOTHING`, s.expID, src, rel, dst)
	return err
}

// Link 单独写一条经校验的边。组合写入请使用 graph_tx.go 中的单事务入口。
func (s *ExplorationStore) Link(ctx context.Context, src int64, rel string, dst int64) error {
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		return s.linkTx(ctx, tx, src, rel, dst)
	})
}

// GetNode 读取本空间节点及其锚点。
func (s *ExplorationStore) GetNode(ctx context.Context, id int64) (*Node, error) {
	n, err := scanNode(s.db.QueryRowContext(ctx,
		`SELECT `+nodeCols+` FROM exploration_nodes WHERE id = $1 AND exploration_id = $2`, id, s.expID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: node %d", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT asset_id FROM exploration_anchors WHERE node_id = $1 ORDER BY asset_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var aid int64
		if err := rows.Scan(&aid); err != nil {
			return nil, err
		}
		n.Anchors = append(n.Anchors, aid)
	}
	return n, rows.Err()
}

// Frontier 按 priority DESC, id ASC 读取 open Intent 候选；领取由 ClaimIntentWithLease 争抢。
func (s *ExplorationStore) Frontier(ctx context.Context, limit int) ([]*Node, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+nodeCols+` FROM exploration_nodes
		WHERE exploration_id = $1 AND kind = 'intent' AND state = 'open'
		ORDER BY priority DESC, id ASC LIMIT $2`, s.expID, limit)
	if err != nil {
		return nil, err
	}
	return scanNodes(rows)
}

// NodesByKind 按 kind（及可选 state）读取本空间节点，id 升序。
func (s *ExplorationStore) NodesByKind(ctx context.Context, kind string, states []string, limit int) ([]*Node, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+nodeCols+` FROM exploration_nodes
		WHERE exploration_id = $1 AND kind = $2
		  AND (COALESCE(cardinality($3::text[]), 0) = 0 OR state = ANY($3::text[]))
		ORDER BY id ASC LIMIT $4`, s.expID, kind, states, limit)
	if err != nil {
		return nil, err
	}
	return scanNodes(rows)
}

// AllNodes 读取本推演空间全部节点（含锚点列表与关联 FindingID），用于推演图与覆盖图可视化。
func (s *ExplorationStore) AllNodes(ctx context.Context, limit int) ([]*Node, error) {
	if limit <= 0 || limit > 2000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+nodeCols+` FROM exploration_nodes
		WHERE exploration_id = $1 ORDER BY id ASC LIMIT $2`, s.expID, limit)
	if err != nil {
		return nil, err
	}
	nodes, err := scanNodes(rows)
	if err != nil || len(nodes) == 0 {
		return nodes, err
	}
	byID := make(map[int64]*Node, len(nodes))
	ids := make([]int64, 0, len(nodes))
	for _, n := range nodes {
		byID[n.ID] = n
		ids = append(ids, n.ID)
	}
	arows, err := s.db.QueryContext(ctx, `SELECT node_id, asset_id FROM exploration_anchors
		WHERE node_id = ANY($1) ORDER BY node_id, asset_id`, ids)
	if err == nil {
		for arows.Next() {
			var nid, aid int64
			if err := arows.Scan(&nid, &aid); err == nil {
				if n := byID[nid]; n != nil {
					n.Anchors = append(n.Anchors, aid)
				}
			}
		}
		arows.Close()
	}
	frows, err := s.db.QueryContext(ctx, `SELECT id, node_id FROM findings WHERE node_id = ANY($1)`, ids)
	if err == nil {
		for frows.Next() {
			var fid, nid int64
			if err := frows.Scan(&fid, &nid); err == nil {
				if n := byID[nid]; n != nil {
					n.FindingID = fid
				}
			}
		}
		frows.Close()
	}
	return nodes, nil
}

// Edges 读取本空间边（有界）。
func (s *ExplorationStore) Edges(ctx context.Context, limit int) ([]Edge, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT src_id, rel, dst_id FROM exploration_edges
		WHERE exploration_id = $1 ORDER BY created_at, src_id, dst_id LIMIT $2`, s.expID, limit)
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

// OriginFactID 返回任务根事实；不存在时返回 0。
func (s *ExplorationStore) OriginFactID(ctx context.Context) (int64, error) {
	return originFactID(ctx, s.db, s.expID)
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func originFactID(ctx context.Context, q queryRower, expID int64) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM exploration_nodes
		WHERE exploration_id = $1 AND kind = 'fact' AND state = 'origin'
		ORDER BY id LIMIT 1`, expID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// SetNodeState 是无旧状态条件的管理入口（不设置 completed_at），写入递增 content_version。
func (s *ExplorationStore) SetNodeState(ctx context.Context, id int64, state string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE exploration_nodes
		SET state = $1, content_version = content_version + 1
		WHERE id = $2 AND exploration_id = $3`, state, id, s.expID)
	return requireOneRow(res, err, id)
}

// SetIntentState 是无 CAS 的管理/回滚入口；终态设置 completed_at，其他状态清空。
// 运行中 Worker 的终态写回必须使用 CompareAndSetIntentStateFenced。
func (s *ExplorationStore) SetIntentState(ctx context.Context, id int64, state string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE exploration_nodes
		SET state = $1,
		    content_version = content_version + 1,
		    lease_until = CASE WHEN $1 = 'running' THEN lease_until ELSE NULL END,
		    completed_at = CASE WHEN $4::boolean THEN now() ELSE NULL END
		WHERE id = $2 AND exploration_id = $3 AND kind = 'intent'`,
		state, id, s.expID, IsTerminalIntentState(state))
	return requireOneRow(res, err, id)
}

// CompareAndSetIntentState 是不带租约代数的 CAS，用于控制面迁移（如 paused→open）。
// expected='running' 的 Worker 写回请使用 CompareAndSetIntentStateFenced。
func (s *ExplorationStore) CompareAndSetIntentState(ctx context.Context, id int64, expected, state string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE exploration_nodes
		SET state = $1,
		    content_version = content_version + 1,
		    lease_until = CASE WHEN $1 = 'running' THEN lease_until ELSE NULL END,
		    completed_at = CASE WHEN $5::boolean THEN now() ELSE NULL END
		WHERE id = $2 AND exploration_id = $3 AND kind = 'intent' AND state = $4`,
		state, id, s.expID, expected, IsTerminalIntentState(state))
	return affectedOne(res, err)
}

// ReopenIntent 只允许 blocked/exhausted/stopped 回到 open，保留节点 ID、血缘与会话。
func (s *ExplorationStore) ReopenIntent(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE exploration_nodes
		SET state = 'open', owner = NULL, blocked_reason = NULL, lease_until = NULL,
		    completed_at = NULL, content_version = content_version + 1
		WHERE id = $1 AND exploration_id = $2 AND kind = 'intent'
		  AND state IN ('blocked','exhausted','stopped')`, id, s.expID)
	return affectedOne(res, err)
}

// ReopenIntentsByBlockedReason 批量重开指定原因阻塞的 Intent（如配额恢复后）。
func (s *ExplorationStore) ReopenIntentsByBlockedReason(ctx context.Context, reason string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE exploration_nodes
		SET state = 'open', owner = NULL, blocked_reason = NULL, lease_until = NULL,
		    completed_at = NULL, content_version = content_version + 1
		WHERE exploration_id = $1 AND kind = 'intent' AND state = 'blocked' AND blocked_reason = $2`,
		s.expID, reason)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SoftDeleteIntent 锁行后将 open/running/paused 转为 deleted，保留血缘并记录原因。
// 迁出 running 后，旧 Worker 的 fenced 写回因 state 不匹配而失败。
func (s *ExplorationStore) SoftDeleteIntent(ctx context.Context, id int64, reason string) (ok bool, err error) {
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		var state string
		err := tx.QueryRowContext(ctx, `SELECT state FROM exploration_nodes
			WHERE id = $1 AND exploration_id = $2 AND kind = 'intent' FOR UPDATE`, id, s.expID).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: intent %d", ErrNotFound, id)
		}
		if err != nil {
			return err
		}
		if state != StateIntentOpen && state != StateIntentRunning && state != StateIntentPaused {
			return nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE exploration_nodes
			SET state = 'deleted', delete_reason = NULLIF($1, ''), lease_until = NULL,
			    content_version = content_version + 1
			WHERE id = $2`, reason, id)
		ok = err == nil
		return err
	})
	return ok, err
}

func affectedOne(res sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func requireOneRow(res sql.Result, err error, id int64) error {
	ok, err := affectedOne(res, err)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: node %d", ErrNotFound, id)
	}
	return nil
}
