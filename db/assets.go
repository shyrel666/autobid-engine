package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Asset 是全局共享的招标文件静态要素；身份由类型自然键唯一索引去重（schema.sql uq_asset_*）。
type Asset struct {
	ID          int64           `json:"id"`
	Type        string          `json:"type"`
	TenderNo    string          `json:"tender_no"`
	LotNo       string          `json:"lot_no,omitempty"`
	Code        string          `json:"code,omitempty"`
	Title       string          `json:"title,omitempty"`
	Content     string          `json:"content,omitempty"`
	Category    string          `json:"category,omitempty"`
	IsMandatory bool            `json:"is_mandatory,omitempty"`
	ScoreMax    *float64        `json:"score_max,omitempty"`
	Amount      *string         `json:"amount,omitempty"` // NUMERIC 十进制文本，避免金额浮点误差
	Currency    string          `json:"currency,omitempty"`
	Quantity    *string         `json:"quantity,omitempty"` // NUMERIC 十进制文本
	Unit        string          `json:"unit,omitempty"`
	SourceRef   json.RawMessage `json:"source_ref,omitempty"` // 例：{"file":"招标文件.pdf","page":37,"section":"第三章"}
	Extra       json.RawMessage `json:"extra,omitempty"`
	TaskIDs     []int64         `json:"task_ids,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	LastSeen    time.Time       `json:"last_seen"`
}

// AssetStore 绑定当前任务：写入时归集 task_ids / task_asset_links，读取走当前任务及直接来源任务的声明范围。
type AssetStore struct {
	db     *DB
	taskID int64
}

func NewAssetStore(d *DB, taskID int64) *AssetStore {
	return &AssetStore{db: d, taskID: taskID}
}

const assetCols = `id, type, tender_no, lot_no, code, title, content, category, is_mandatory,
	score_max::float8, amount::text, COALESCE(currency,''), quantity::text, COALESCE(unit,''),
	source_ref, extra, to_jsonb(task_ids), created_at, last_seen`

func scanAsset(r rowScanner) (*Asset, error) {
	var (
		a                       Asset
		scoreMax                sql.NullFloat64
		amount, quantity        sql.NullString
		sourceRef, extra, tasks []byte
	)
	if err := r.Scan(&a.ID, &a.Type, &a.TenderNo, &a.LotNo, &a.Code, &a.Title, &a.Content, &a.Category, &a.IsMandatory,
		&scoreMax, &amount, &a.Currency, &quantity, &a.Unit,
		&sourceRef, &extra, &tasks, &a.CreatedAt, &a.LastSeen); err != nil {
		return nil, err
	}
	if scoreMax.Valid {
		a.ScoreMax = &scoreMax.Float64
	}
	if amount.Valid {
		a.Amount = &amount.String
	}
	if quantity.Valid {
		a.Quantity = &quantity.String
	}
	a.SourceRef, a.Extra = sourceRef, extra
	if err := json.Unmarshal(tasks, &a.TaskIDs); err != nil {
		return nil, err
	}
	return &a, nil
}

// assetConflictTarget 返回与部分唯一索引精确匹配的 ON CONFLICT 推断子句。
func assetConflictTarget(typ string) (string, error) {
	switch typ {
	case AssetTender:
		return `(tender_no) WHERE type = 'tender'`, nil
	case AssetLot:
		return `(tender_no, lot_no) WHERE type = 'lot'`, nil
	case AssetClause, AssetQualification, AssetScoringItem, AssetCommercialItem:
		return `(type, tender_no, lot_no, code) WHERE type IN ('clause','qualification','scoring_item','commercial_item')`, nil
	}
	return "", fmt.Errorf("db: unknown asset type %q", typ)
}

// UpsertAsset 单事务完成「按自然键 UPSERT 实体 -> 归集 task_ids -> task_asset_links 来源记录
// -> 为 sourceNodeID 补锚 -> 自动范围」，补齐原仓 insert_assets 逐条非事务、后续错误未上抛的缺口。
// 非空字段覆盖旧值，extra/source_ref 做 JSONB 合并，is_mandatory 一经标记不回退。
func (s *AssetStore) UpsertAsset(ctx context.Context, a *Asset, sourceNodeID int64) (id int64, created bool, err error) {
	target, err := assetConflictTarget(a.Type)
	if err != nil {
		return 0, false, err
	}
	sourceRef, err := marshalPayload(a.SourceRef)
	if err != nil {
		return 0, false, err
	}
	extra, err := marshalPayload(a.Extra)
	if err != nil {
		return 0, false, err
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO assets(type, tender_no, lot_no, code, title, content, category, is_mandatory,
			                   score_max, amount, currency, quantity, unit, source_ref, extra, task_ids)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8,
			        $9::numeric, $10::numeric, NULLIF($11, ''), $12::numeric, NULLIF($13, ''), $14::jsonb, $15::jsonb,
			        CASE WHEN $16::bigint > 0 THEN ARRAY[$16::bigint] ELSE '{}'::bigint[] END)
			ON CONFLICT `+target+` DO UPDATE
			SET title        = COALESCE(NULLIF(EXCLUDED.title, ''), assets.title),
			    content      = COALESCE(NULLIF(EXCLUDED.content, ''), assets.content),
			    category     = COALESCE(NULLIF(EXCLUDED.category, ''), assets.category),
			    is_mandatory = assets.is_mandatory OR EXCLUDED.is_mandatory,
			    score_max    = COALESCE(EXCLUDED.score_max, assets.score_max),
			    amount       = COALESCE(EXCLUDED.amount, assets.amount),
			    currency     = COALESCE(EXCLUDED.currency, assets.currency),
			    quantity     = COALESCE(EXCLUDED.quantity, assets.quantity),
			    unit         = COALESCE(EXCLUDED.unit, assets.unit),
			    source_ref   = assets.source_ref || EXCLUDED.source_ref,
			    extra        = assets.extra || EXCLUDED.extra,
			    task_ids     = CASE
			        WHEN $16::bigint > 0 AND NOT (assets.task_ids @> ARRAY[$16::bigint])
			        THEN array_append(assets.task_ids, $16::bigint)
			        ELSE assets.task_ids
			    END,
			    last_seen    = now()
			RETURNING id, (xmax = 0)`,
			a.Type, strings.TrimSpace(a.TenderNo), strings.TrimSpace(a.LotNo), strings.TrimSpace(a.Code),
			a.Title, a.Content, a.Category, a.IsMandatory,
			a.ScoreMax, a.Amount, a.Currency, a.Quantity, a.Unit, string(sourceRef), string(extra),
			s.taskID,
		).Scan(&id, &created); err != nil {
			return err
		}
		if s.taskID <= 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO task_asset_links(task_id, asset_id, source_node_id)
			VALUES ($1, $2, NULLIF($3::bigint, 0))
			ON CONFLICT (task_id, asset_id) DO UPDATE
			SET source_node_id = COALESCE(task_asset_links.source_node_id, EXCLUDED.source_node_id),
			    last_seen = now()`, s.taskID, id, sourceNodeID); err != nil {
			return err
		}
		if sourceNodeID > 0 {
			if err := anchorTx(ctx, tx, sourceNodeID, []int64{id}); err != nil {
				return err
			}
		}
		// 自动范围只补“尚未被任何现有范围覆盖”的实体，避免全标段通用要素把已限定到某标段的任务扩大到整个项目。
		_, err := tx.ExecContext(ctx, `
			INSERT INTO task_scopes(task_id, tender_no, lot_no, source)
			SELECT $1::bigint, a.tender_no, a.lot_no, 'auto'
			FROM assets a
			WHERE a.id = $2
			  AND NOT EXISTS (
				SELECT 1 FROM task_scopes sc
				WHERE sc.task_id = $1::bigint AND `+scopeMatch+`)
			ON CONFLICT (task_id, tender_no, lot_no) DO NOTHING`, s.taskID, id)
		return err
	})
	return id, created, err
}

func (s *AssetStore) GetAsset(ctx context.Context, id int64) (*Asset, error) {
	a, err := scanAsset(s.db.QueryRowContext(ctx, `SELECT `+assetCols+` FROM assets WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: asset %d", ErrNotFound, id)
	}
	return a, err
}

// contextTasksCTE 定义当前任务 ∪ 直接来源任务（不递归），$1 为当前任务 ID。
const contextTasksCTE = `ctx_tasks AS (
		SELECT $1::bigint AS id
		UNION
		SELECT source_task_id FROM task_relations WHERE task_id = $1::bigint
	)`

// scopeMatch 判定实体 a 是否落入范围 sc：sc.lot_no=” 覆盖整个项目；指定标段时同时包含全标段通用要素（a.lot_no=”）。
const scopeMatch = `a.tender_no = sc.tender_no AND (sc.lot_no = '' OR a.lot_no = sc.lot_no OR a.lot_no = '')`

// ListScopedAssets 读取当前任务及直接来源任务声明范围内的实体，可按类型过滤，id 升序。
func (s *AssetStore) ListScopedAssets(ctx context.Context, types []string, limit int) ([]*Asset, error) {
	rows, err := s.db.QueryContext(ctx, `WITH `+contextTasksCTE+`
		SELECT `+assetCols+` FROM assets a
		WHERE EXISTS (
			SELECT 1 FROM task_scopes sc
			WHERE sc.task_id IN (SELECT id FROM ctx_tasks) AND `+scopeMatch+`)
		  AND (COALESCE(cardinality($2::text[]), 0) = 0 OR a.type = ANY($2::text[]))
		ORDER BY a.id LIMIT $3`, s.taskID, types, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Asset
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// EntityEdge 是 Patch ⑥ 的静态实体关系边。
type EntityEdge struct {
	ID           int64           `json:"id"`
	SrcAssetID   int64           `json:"src_asset_id"`
	Rel          string          `json:"rel"`
	DstAssetID   int64           `json:"dst_asset_id"`
	TaskIDs      []int64         `json:"task_ids,omitempty"`
	SourceNodeID *int64          `json:"source_node_id,omitempty"`
	Weight       float64         `json:"weight"`
	Props        json.RawMessage `json:"props,omitempty"`
	LastSeen     time.Time       `json:"last_seen"`
}

// UpsertEntityEdge：幂等写入静态实体关系边，自动去重合并 task_ids 并合并 JSONB 属性（Patch ⑥）。
func (s *AssetStore) UpsertEntityEdge(ctx context.Context, srcAssetID int64, rel string, dstAssetID int64, taskID int64, sourceNodeID *int64, weight float64, props map[string]any) (int64, error) {
	if srcAssetID <= 0 || dstAssetID <= 0 || srcAssetID == dstAssetID || strings.TrimSpace(rel) == "" {
		return 0, fmt.Errorf("invalid entity edge (%d -[%s]-> %d)", srcAssetID, rel, dstAssetID)
	}
	rawProps, err := json.Marshal(props)
	if err != nil || props == nil {
		rawProps = []byte("{}")
	}
	var edgeID int64
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO entity_edges(src_asset_id, rel, dst_asset_id, task_ids, source_node_id, weight, props)
		VALUES ($1, $2, $3, CASE WHEN $4::bigint > 0 THEN ARRAY[$4::bigint] ELSE '{}'::BIGINT[] END, $5::bigint, $6, $7::jsonb)
		ON CONFLICT (src_asset_id, rel, dst_asset_id) DO UPDATE
		SET task_ids = CASE
		        WHEN $4::bigint > 0 AND NOT (entity_edges.task_ids @> ARRAY[$4::bigint])
		        THEN array_append(entity_edges.task_ids, $4::bigint)
		        ELSE entity_edges.task_ids
		    END,
		    source_node_id = COALESCE(EXCLUDED.source_node_id, entity_edges.source_node_id),
		    weight         = EXCLUDED.weight,
		    props          = entity_edges.props || EXCLUDED.props,
		    last_seen      = now(),
		    updated_at     = now()
		RETURNING id`,
		srcAssetID, rel, dstAssetID, taskID, sourceNodeID, weight, string(rawProps),
	).Scan(&edgeID)
	return edgeID, err
}

// EntityEdgesOf 读取与实体相连的边；outgoing 为 true 取出边，否则取入边；rel 为空表示全部关系。
func (s *AssetStore) EntityEdgesOf(ctx context.Context, assetID int64, rel string, outgoing bool, limit int) ([]*EntityEdge, error) {
	col := "dst_asset_id"
	if outgoing {
		col = "src_asset_id"
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, src_asset_id, rel, dst_asset_id, to_jsonb(task_ids), source_node_id, weight, props, last_seen
		FROM entity_edges
		WHERE `+col+` = $1 AND ($2 = '' OR rel = $2)
		ORDER BY id LIMIT $3`, assetID, rel, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*EntityEdge
	for rows.Next() {
		var (
			e            EntityEdge
			tasks, props []byte
			src          sql.NullInt64
		)
		if err := rows.Scan(&e.ID, &e.SrcAssetID, &e.Rel, &e.DstAssetID, &tasks, &src, &e.Weight, &props, &e.LastSeen); err != nil {
			return nil, err
		}
		e.Props = props
		if err := json.Unmarshal(tasks, &e.TaskIDs); err != nil {
			return nil, err
		}
		if src.Valid {
			e.SourceNodeID = &src.Int64
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

func (s *AssetStore) TaskID() int64 { return s.taskID }

// Coverage 计算当前任务（含直接来源任务）的覆盖率。
func (s *AssetStore) Coverage(ctx context.Context) (Coverage, error) {
	return s.db.TaskCoverageWithSources(ctx, s.taskID)
}
