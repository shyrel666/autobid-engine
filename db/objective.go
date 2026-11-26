package db

// D2 客观分：只统计当前可采性运行中 verdict=admissible 的证据；规则无法解析时记 manual，不静默当作 0 分通过。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/compliance"
)

// ObjectiveScoreRun 是一次客观分审计快照。
type ObjectiveScoreRun struct {
	ID                 int64     `json:"id"`
	TenantID           string    `json:"tenant_id"`
	TaskID             int64     `json:"task_id"`
	RequirementID      int64     `json:"requirement_item_id"`
	RequirementCode    string    `json:"requirement_code"`
	RequirementTitle   string    `json:"requirement_title"`
	AdmissibilityRunID *int64    `json:"admissibility_run_id,omitempty"`
	RuleVersion        string    `json:"rule_version"`
	Status             string    `json:"status"`
	Score              float64   `json:"score"`
	MaxScore           float64   `json:"max_score"`
	EvidenceIDs        []int64   `json:"evidence_ids"`
	Gaps               []string  `json:"gaps"`
	ComputedAt         time.Time `json:"computed_at"`
}

const objectiveCols = `o.id, o.tenant_id, o.task_id, o.requirement_id, req.code, req.title, o.admissibility_run_id, o.rule_version,
	o.status, o.score::float8, o.max_score::float8, to_jsonb(o.evidence_ids), o.gaps, o.computed_at`

func scanObjective(r rowScanner) (*ObjectiveScoreRun, error) {
	var out ObjectiveScoreRun
	var runID sql.NullInt64
	var ids, gaps []byte
	err := r.Scan(&out.ID, &out.TenantID, &out.TaskID, &out.RequirementID, &out.RequirementCode, &out.RequirementTitle, &runID,
		&out.RuleVersion, &out.Status, &out.Score, &out.MaxScore, &ids, &gaps, &out.ComputedAt)
	if err != nil {
		return nil, err
	}
	if runID.Valid {
		v := runID.Int64
		out.AdmissibilityRunID = &v
	}
	out.EvidenceIDs, out.Gaps = []int64{}, []string{}
	_ = json.Unmarshal(ids, &out.EvidenceIDs)
	_ = json.Unmarshal(gaps, &out.Gaps)
	if out.EvidenceIDs == nil {
		out.EvidenceIDs = []int64{}
	}
	if out.Gaps == nil {
		out.Gaps = []string{}
	}
	return &out, nil
}

func objectiveCriteria(req *RequirementItem) string {
	var b strings.Builder
	b.WriteString(req.Title)
	b.WriteString("\n")
	b.WriteString(req.Description)
	for _, t := range req.ScoringTiers {
		b.WriteString("\n")
		b.WriteString(t.Label)
		b.WriteString(" ")
		b.WriteString(t.Criteria)
	}
	return b.String()
}

// ComputeObjectiveScores 计算任务的客观评分项（requirementID=0 表示全部）；只读取当前可采性运行。
func (d *DB) ComputeObjectiveScores(ctx context.Context, tenant string, taskID, requirementID int64, actor string) ([]*ObjectiveScoreRun, error) {
	tenant = tenantOr(tenant)
	rows, err := d.QueryContext(ctx, `SELECT `+reqCols+` FROM requirement_items
		WHERE tenant_id=$1 AND task_id=$2 AND category='scoring_item' AND parent_id IS NULL AND eval_kind='objective'
		  AND score_group <> 'price' AND NOT out_of_bid AND superseded_by_id IS NULL AND review_status <> 'stale'
		  AND NOT (review_status='needs_revision' AND COALESCE(source_loc->>'scoring_orphan','')='true')
		  AND ($3::bigint=0 OR id=$3::bigint) ORDER BY code`, tenant, taskID, requirementID)
	if err != nil {
		return nil, err
	}
	var reqs []*RequirementItem
	for rows.Next() {
		r, err := scanRequirement(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		reqs = append(reqs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]*ObjectiveScoreRun, 0, len(reqs))
	for _, req := range reqs {
		run, err := d.CurrentAdmissibilityRun(ctx, tenant, taskID, req.ID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		var (
			runID        any
			admissible   int
			evidenceIDs  []int64
			gaps         []string
			status       = "computed"
			score, maxim float64
			ruleVersion  = compliance.AdmissibilityRuleVersion
		)
		maxim = req.ScoreWeight
		if run == nil {
			status = "manual"
			gaps = []string{"admissibility_run_missing"}
		} else {
			runID = run.ID
			for _, it := range run.Items {
				if it.Verdict == compliance.Admissible {
					admissible++
					evidenceIDs = append(evidenceIDs, it.EvidenceID)
				}
			}
			res := compliance.ComputeObjective(objectiveCriteria(req), req.ScoreWeight, req.ProofRules.MinCount, admissible)
			status, score, maxim, gaps = res.Status, res.Score, res.Max, res.Gaps
			if run.Verdict == compliance.Manual && status == "computed" {
				status = "manual"
				gaps = append(gaps, "admissibility_manual")
			}
		}
		if evidenceIDs == nil {
			evidenceIDs = []int64{}
		}
		if gaps == nil {
			gaps = []string{}
		}
		gapsJSON, _ := json.Marshal(gaps)
		scoreRun, err := scanObjective(d.QueryRowContext(ctx, `WITH ins AS (
				INSERT INTO objective_score_runs(tenant_id, task_id, requirement_id, admissibility_run_id, rule_version, status,
					score, max_score, evidence_ids, gaps, created_by, updated_by)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::bigint[],$10::jsonb,COALESCE(NULLIF($11,''),''),COALESCE(NULLIF($11,''),'')) RETURNING *)
			SELECT `+objectiveCols+` FROM ins o JOIN requirement_items req
			  ON req.tenant_id=o.tenant_id AND req.task_id=o.task_id AND req.id=o.requirement_id`,
			tenant, taskID, req.ID, runID, ruleVersion, status, score, maxim, pgInt64Array(evidenceIDs), string(gapsJSON), actor))
		if err != nil {
			return nil, fmt.Errorf("objective score %s: %w", req.Code, err)
		}
		out = append(out, scoreRun)
	}
	return out, nil
}

type bidEvidenceResult struct {
	verdict    string
	admissible int
	pending    int
	score      sql.NullFloat64
}

// Read the score paired with the current admissibility run. Historical scores
// remain available for audit even after an evidence or deadline change.
func currentBidEvidence(ctx context.Context, q queryer, tenant string, taskID int64) (map[int64]bidEvidenceResult, error) {
	rows, err := q.QueryContext(ctx, `SELECT r.requirement_id, r.verdict, counts.admissible, counts.pending, score.score::float8
		FROM admissibility_runs r
		LEFT JOIN LATERAL (
			SELECT count(*) FILTER (WHERE verdict='admissible') AS admissible,
			       count(*) FILTER (WHERE verdict='manual') AS pending
			FROM admissibility_items WHERE tenant_id=r.tenant_id AND run_id=r.id
		) counts ON true
		LEFT JOIN LATERAL (
			SELECT o.score FROM objective_score_runs o
			WHERE o.tenant_id=r.tenant_id AND o.task_id=r.task_id AND o.requirement_id=r.requirement_id
			  AND o.admissibility_run_id=r.id AND o.rule_version=$3 ORDER BY o.id DESC LIMIT 1
		) score ON true
		WHERE r.tenant_id=$1 AND r.task_id=$2 AND r.status='current' AND r.rule_version=$3`,
		tenant, taskID, compliance.AdmissibilityRuleVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bidEvidenceResult{}
	for rows.Next() {
		var id int64
		var evidence bidEvidenceResult
		if err := rows.Scan(&id, &evidence.verdict, &evidence.admissible, &evidence.pending, &evidence.score); err != nil {
			return nil, err
		}
		out[id] = evidence
	}
	return out, rows.Err()
}

// Only current evidence contributes to the range. Pending evidence represents
// possible points. Absent and rejected evidence contribute zero to both bounds.
func (e bidEvidenceResult) objectiveBounds(req *RequirementItem) (lower, upper float64) {
	if e.admissible+e.pending == 0 {
		return 0, 0
	}
	if e.score.Valid {
		lower = min(req.ScoreWeight, e.score.Float64)
	}
	possible := compliance.ComputeObjective(objectiveCriteria(req), req.ScoreWeight, req.ProofRules.MinCount, e.admissible+e.pending)
	if possible.Status == "manual" {
		return lower, req.ScoreWeight
	}
	return lower, max(lower, possible.Score)
}

// LatestObjectiveScores 返回每个客观评分项最新一次计算快照。
func (d *DB) LatestObjectiveScores(ctx context.Context, tenant string, taskID int64) ([]*ObjectiveScoreRun, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+objectiveCols+` FROM objective_score_runs o
		JOIN requirement_items req ON req.tenant_id=o.tenant_id AND req.task_id=o.task_id AND req.id=o.requirement_id
		WHERE o.tenant_id=$1 AND o.task_id=$2
		  AND o.id = (SELECT MAX(x.id) FROM objective_score_runs x
		              WHERE x.tenant_id=o.tenant_id AND x.task_id=o.task_id AND x.requirement_id=o.requirement_id)
		ORDER BY req.code`, tenantOr(tenant), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ObjectiveScoreRun
	for rows.Next() {
		r, err := scanObjective(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
