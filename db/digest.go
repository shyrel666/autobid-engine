package db

// 冷节点归档与 Digest 折叠（§1.6、附录 B.4 / B.7 KindDigest）。
// Digest 是无损压缩：成员节点原样保留，covers 边从摘要指向成员，可按 id 还原。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
)

// DefaultColdAfterRounds 是节点最近一次写入后经过多少规划轮次即可冷却。
const DefaultColdAfterRounds = 3

// AdvanceRound 递增推演空间的规划轮次并返回新轮次（Planner 每轮调用一次）。
func (s *ExplorationStore) AdvanceRound(ctx context.Context) (int64, error) {
	var round int64
	err := s.db.QueryRowContext(ctx, `UPDATE explorations SET round_no = round_no + 1 WHERE id = $1 RETURNING round_no`,
		s.expID).Scan(&round)
	return round, err
}

func (s *ExplorationStore) CurrentRound(ctx context.Context) (int64, error) {
	var round int64
	err := s.db.QueryRowContext(ctx, `SELECT round_no FROM explorations WHERE id = $1`, s.expID).Scan(&round)
	return round, err
}

// coldEligible 是可冷却节点的完整条件：终态 Intent、非根 Fact、已消费 Hint。
// Goal、在途 Intent、活跃 Hint、Finding（交付物）与 Digest 永不冷却。
const coldEligible = `((kind = 'intent' AND state IN ('done','blocked','exhausted','stopped','deleted'))
	OR (kind = 'fact' AND state IN ('confirmed','dismissed'))
	OR (kind = 'hint' AND state = 'consumed'))`

// StampColdNodes 把最近一次写入早于 currentRound-coldAfterRounds 轮的可冷却节点标记为冷（幂等），返回新标记数。
// 节点之后的任何状态/内容写入会由触发器清空标记（回温）。
func (s *ExplorationStore) StampColdNodes(ctx context.Context, currentRound, coldAfterRounds int64) (int64, error) {
	if coldAfterRounds <= 0 {
		coldAfterRounds = DefaultColdAfterRounds
	}
	res, err := s.db.ExecContext(ctx, `UPDATE exploration_nodes SET cold_since_round = $2::bigint
		WHERE exploration_id = $1 AND cold_since_round IS NULL AND active_round <= $2::bigint - $3::bigint AND `+coldEligible,
		s.expID, currentRound, coldAfterRounds)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ColdNodes 读取冷节点（冷却越晚越靠前）。
func (s *ExplorationStore) ColdNodes(ctx context.Context, limit int) ([]*Node, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+nodeCols+` FROM exploration_nodes
		WHERE exploration_id = $1 AND cold_since_round IS NOT NULL
		ORDER BY cold_since_round DESC, id DESC LIMIT $2`, s.expID, limit)
	if err != nil {
		return nil, err
	}
	return scanNodes(rows)
}

// MemberVersion 是成员在被折叠时的状态快照；成员任一字段变化即说明摘要已过期。
type MemberVersion struct {
	State          string `json:"state"`
	ContentVersion int    `json:"content_version"`
}

type digestPayload struct {
	Title          string                   `json:"title"`
	Body           string                   `json:"body"`
	MemberIDs      []int64                  `json:"member_ids"`
	MemberVersions map[string]MemberVersion `json:"member_versions"`
	Round          int64                    `json:"round"`
}

var ErrInvalidDigest = errors.New("db: invalid digest")

// CreateDigest 单事务完成「校验并锁定成员 -> 被新成员集完全覆盖的旧 active Digest 置 superseded 并删除其 covers
// -> 插入新 digest(active) -> 写 covers 边」，返回新 Digest ID 与被取代的 Digest ID。
// 成员只能是可冷却类别（终态 Intent、非根 Fact、已消费 Hint）及 Finding；不允许折叠 Goal、在途工作或其他 Digest。
func (s *ExplorationStore) CreateDigest(ctx context.Context, title, body string, memberIDs []int64, currentRound int64) (digestID int64, superseded []int64, err error) {
	members := slices.Clone(memberIDs)
	slices.Sort(members)
	members = slices.Compact(members)
	if len(members) == 0 || title == "" {
		return 0, nil, fmt.Errorf("%w: title and at least one member are required", ErrInvalidDigest)
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id, state, content_version,
				(`+coldEligible+` OR kind = 'finding') AS foldable
			FROM exploration_nodes
			WHERE exploration_id = $1 AND id = ANY($2::bigint[])
			ORDER BY id FOR SHARE`, s.expID, members)
		if err != nil {
			return err
		}
		versions := map[string]MemberVersion{}
		for rows.Next() {
			var (
				id       int64
				mv       MemberVersion
				foldable bool
			)
			if err := rows.Scan(&id, &mv.State, &mv.ContentVersion, &foldable); err != nil {
				rows.Close()
				return err
			}
			if !foldable {
				rows.Close()
				return fmt.Errorf("%w: node %d (%s) cannot be folded", ErrInvalidDigest, id, mv.State)
			}
			versions[strconv.FormatInt(id, 10)] = mv
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(versions) != len(members) {
			return fmt.Errorf("%w: some members are not in exploration %d", ErrInvalidDigest, s.expID)
		}

		// 旧摘要的全部成员都在新成员集中时，视为被完全覆盖。
		old, err := tx.QueryContext(ctx, `SELECT d.id FROM exploration_nodes d
			WHERE d.exploration_id = $1 AND d.kind = 'digest' AND d.state = 'active'
			  AND EXISTS (SELECT 1 FROM exploration_edges e
			              WHERE e.src_id = d.id AND e.rel = 'covers' AND e.dst_id = ANY($2::bigint[]))
			  AND NOT EXISTS (SELECT 1 FROM exploration_edges e
			              WHERE e.src_id = d.id AND e.rel = 'covers' AND NOT (e.dst_id = ANY($2::bigint[])))
			ORDER BY d.id FOR UPDATE OF d`, s.expID, members)
		if err != nil {
			return err
		}
		for old.Next() {
			var id int64
			if err := old.Scan(&id); err != nil {
				old.Close()
				return err
			}
			superseded = append(superseded, id)
		}
		old.Close()
		if err := old.Err(); err != nil {
			return err
		}
		if len(superseded) > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE exploration_nodes SET state = 'superseded', content_version = content_version + 1
				WHERE id = ANY($1::bigint[])`, superseded); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM exploration_edges
				WHERE exploration_id = $1 AND rel = 'covers' AND src_id = ANY($2::bigint[])`, s.expID, superseded); err != nil {
				return err
			}
		}

		payload := digestPayload{Title: title, Body: body, MemberIDs: members, MemberVersions: versions, Round: currentRound}
		if digestID, err = s.insertNodeTx(ctx, tx, KindDigest, StateDigestActive, payload, 0, "", nil); err != nil {
			return err
		}
		for _, m := range members {
			if err := s.linkTx(ctx, tx, digestID, RelCovers, m); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return digestID, superseded, nil
}

// DigestView 是活跃 Digest 及其成员新鲜度。Stale=true 表示至少一个成员自折叠后发生了状态或内容变化（或已消失），
// 该摘要不再可信：调用方应展开其成员，而不是继续折叠。
type DigestView struct {
	Node           *Node   `json:"node"`
	Title          string  `json:"title"`
	Body           string  `json:"body"`
	MemberIDs      []int64 `json:"member_ids"`
	Round          int64   `json:"round"`
	Stale          bool    `json:"stale"`
	ChangedMembers []int64 `json:"changed_members,omitempty"`
}

// ActiveDigestsWithMembers 读取活跃 Digest，并比对成员当前 state + content_version 与折叠时快照。
func (s *ExplorationStore) ActiveDigestsWithMembers(ctx context.Context) ([]*DigestView, error) {
	digests, err := s.NodesByKind(ctx, KindDigest, []string{StateDigestActive}, 500)
	if err != nil || len(digests) == 0 {
		return nil, err
	}
	views := make([]*DigestView, 0, len(digests))
	var all []int64
	payloads := make([]digestPayload, len(digests))
	for i, d := range digests {
		if err := json.Unmarshal(d.Payload, &payloads[i]); err != nil {
			return nil, fmt.Errorf("decode digest %d: %w", d.ID, err)
		}
		all = append(all, payloads[i].MemberIDs...)
	}
	current := map[int64]MemberVersion{}
	rows, err := s.db.QueryContext(ctx, `SELECT id, state, content_version FROM exploration_nodes
		WHERE exploration_id = $1 AND id = ANY($2::bigint[])`, s.expID, all)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id int64
			mv MemberVersion
		)
		if err := rows.Scan(&id, &mv.State, &mv.ContentVersion); err != nil {
			return nil, err
		}
		current[id] = mv
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, d := range digests {
		p := payloads[i]
		v := &DigestView{Node: d, Title: p.Title, Body: p.Body, MemberIDs: p.MemberIDs, Round: p.Round}
		for _, id := range p.MemberIDs {
			now, ok := current[id]
			if !ok || now != p.MemberVersions[strconv.FormatInt(id, 10)] {
				v.ChangedMembers = append(v.ChangedMembers, id)
			}
		}
		v.Stale = len(v.ChangedMembers) > 0
		views = append(views, v)
	}
	return views, nil
}

// ConsumeHint 把 active Hint 置为 consumed；已消费或不存在时返回 false。
func (s *ExplorationStore) ConsumeHint(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE exploration_nodes SET state = 'consumed', content_version = content_version + 1
		WHERE id = $1 AND exploration_id = $2 AND kind = 'hint' AND state = 'active'
		AND NOT EXISTS(SELECT 1 FROM intent_inputs i WHERE i.hint_id=$1 AND i.state IN ('pending','delivered'))`, id, s.expID)
	return affectedOne(res, err)
}
