package db

// D3 章节故事板（Section Storyboard）与 Pink Team（粉队）持久化评审状态机：
// 1. 每个章节在动笔写作前先建立故事板（核心论点句 Theme Sentence、覆盖全部绑定要求的得分点与“我方做法+采购人收益+支撑证据”、
//    引用的可采性证据明细 admissibility_item_ids、知识库证据 evidence_ids、承诺台账项 commitment_ids 与图表规划 chart_plans）；
// 2. 状态流转：draft -> pending_pink_review -> pink_approved / pink_rejected，上游大纲/要求/证据/承诺变更自动置为 stale；
// 3. 粉队评审强制执行作者与评审人分离（approved_by != created_by 且 approved_by != submitted_by）；
// 4. 已批准的故事板由数据库触发器 d3_storyboard_immutable 保证不可原地篡改，后续修改自动派生新版本并使旧版 superseded。

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

// D3 故事板状态常量。
const (
	StoryboardDraft        = "draft"
	StoryboardPendingPink  = "pending_pink_review"
	StoryboardPinkApproved = "pink_approved"
	StoryboardPinkRejected = "pink_rejected"
	StoryboardStale        = "stale"
	StoryboardSuperseded   = "superseded"
)

// D3 错误哨兵。
var (
	ErrStoryboardMissing         = errors.New("d3: section storyboard is missing")
	ErrStoryboardNotApproved     = errors.New("d3: section storyboard is not pink_approved")
	ErrStoryboardStale           = errors.New("d3: section storyboard is stale")
	ErrStoryboardState           = errors.New("d3: storyboard state conflict")
	ErrStoryboardInvalid         = errors.New("d3: invalid storyboard")
	ErrPinkSelfApproval          = errors.New("d3: pink team reviewer cannot be the storyboard author or submitter")
	ErrRedSelfApproval           = errors.New("d3: red team reviewer cannot be the section revision author")
	ErrUngroundedClaim           = errors.New("d3: section revision contains ungrounded fact or commitment claims")
	ErrWritingVerificationFailed = errors.New("d3: section writing verification failed")
)

// StoryboardScoringPoint 是故事板中单条招标要求/评分点的写作策划。
type StoryboardScoringPoint struct {
	RequirementCode string   `json:"requirement_code"`
	ScoringPoint    string   `json:"scoring_point"`
	OurApproach     string   `json:"our_approach"`
	BuyerBenefit    string   `json:"buyer_benefit"`
	EvidenceRefs    []string `json:"evidence_refs,omitempty"`
}

// StoryboardChartPlan 是故事板中规划的图表/架构图。
type StoryboardChartPlan struct {
	Title      string `json:"title"`
	ChartType  string `json:"chart_type"`
	Purpose    string `json:"purpose"`
	DataSource string `json:"data_source,omitempty"`
}

// SectionStoryboard 是章节故事板持久化对象。
type SectionStoryboard struct {
	ID                   int64                    `json:"id"`
	TenantID             string                   `json:"tenant_id"`
	TaskID               int64                    `json:"task_id"`
	SectionID            int64                    `json:"section_id"`
	SectionCode          string                   `json:"section_code,omitempty"`
	SectionTitle         string                   `json:"section_title,omitempty"`
	OutlineVersion       int                      `json:"outline_version"`
	Version              int                      `json:"version"`
	ThemeSentence        string                   `json:"theme_sentence"`
	ScoringPoints        []StoryboardScoringPoint `json:"scoring_points"`
	AdmissibilityItemIDs []int64                  `json:"admissibility_item_ids"`
	EvidenceIDs          []int64                  `json:"evidence_ids"`
	CommitmentIDs        []int64                  `json:"commitment_ids"`
	ChartPlans           []StoryboardChartPlan    `json:"chart_plans"`
	Status               string                   `json:"status"`
	StaleReason          string                   `json:"stale_reason,omitempty"`
	ReviewNote           string                   `json:"review_note,omitempty"`
	SubmittedBy          string                   `json:"submitted_by,omitempty"`
	SubmittedAt          *time.Time               `json:"submitted_at,omitempty"`
	ReviewedBy           string                   `json:"reviewed_by,omitempty"`
	ReviewedAt           *time.Time               `json:"reviewed_at,omitempty"`
	CreatedBy            string                   `json:"created_by"`
	UpdatedBy            string                   `json:"updated_by"`
	ApprovedBy           string                   `json:"approved_by,omitempty"`
	CreatedAt            time.Time                `json:"created_at"`
	UpdatedAt            time.Time                `json:"updated_at"`
}

// UpsertStoryboardInput 是创建或更新章节故事板的输入。
type UpsertStoryboardInput struct {
	TaskID               int64                    `json:"-"`
	SectionID            int64                    `json:"-"`
	ThemeSentence        string                   `json:"theme_sentence"`
	ScoringPoints        []StoryboardScoringPoint `json:"scoring_points"`
	AdmissibilityItemIDs []int64                  `json:"admissibility_item_ids"`
	EvidenceIDs          []int64                  `json:"evidence_ids"`
	CommitmentIDs        []int64                  `json:"commitment_ids"`
	ChartPlans           []StoryboardChartPlan    `json:"chart_plans"`
}

const storyboardCols = `sb.id, sb.tenant_id, sb.task_id, sb.section_id, COALESCE(s.section_code,''), COALESCE(s.title,''),
	sb.outline_version, sb.version, sb.theme_sentence, sb.scoring_points,
	to_jsonb(sb.admissibility_item_ids), to_jsonb(sb.evidence_ids), to_jsonb(sb.commitment_ids),
	sb.chart_plans, sb.status, sb.stale_reason, sb.review_note, sb.submitted_by, sb.submitted_at,
	sb.reviewed_by, sb.reviewed_at, sb.created_by, sb.updated_by, sb.approved_by, sb.created_at, sb.updated_at`

const storyboardJoin = ` FROM section_storyboards sb
	LEFT JOIN proposal_sections s ON s.tenant_id = sb.tenant_id AND s.id = sb.section_id `

func scanStoryboard(r rowScanner) (*SectionStoryboard, error) {
	var (
		sb                         SectionStoryboard
		spRaw, admRaw, evRaw, cRaw []byte
		chartRaw                   []byte
		subAt, revAt               sql.NullTime
	)
	err := r.Scan(
		&sb.ID, &sb.TenantID, &sb.TaskID, &sb.SectionID, &sb.SectionCode, &sb.SectionTitle,
		&sb.OutlineVersion, &sb.Version, &sb.ThemeSentence, &spRaw,
		&admRaw, &evRaw, &cRaw,
		&chartRaw, &sb.Status, &sb.StaleReason, &sb.ReviewNote, &sb.SubmittedBy, &subAt,
		&sb.ReviewedBy, &revAt, &sb.CreatedBy, &sb.UpdatedBy, &sb.ApprovedBy, &sb.CreatedAt, &sb.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sb.SubmittedAt = nullTimePtr(subAt)
	sb.ReviewedAt = nullTimePtr(revAt)
	sb.ScoringPoints = []StoryboardScoringPoint{}
	_ = json.Unmarshal(spRaw, &sb.ScoringPoints)
	if sb.ScoringPoints == nil {
		sb.ScoringPoints = []StoryboardScoringPoint{}
	}
	sb.AdmissibilityItemIDs = int64sJSON(admRaw)
	sb.EvidenceIDs = int64sJSON(evRaw)
	sb.CommitmentIDs = int64sJSON(cRaw)
	sb.ChartPlans = []StoryboardChartPlan{}
	_ = json.Unmarshal(chartRaw, &sb.ChartPlans)
	if sb.ChartPlans == nil {
		sb.ChartPlans = []StoryboardChartPlan{}
	}
	return &sb, nil
}

func currentStoryboardTx(ctx context.Context, q queryer, tenant string, taskID, sectionID int64, forUpdate bool) (*SectionStoryboard, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE OF sb"
	}
	return scanStoryboard(q.QueryRowContext(ctx, `SELECT `+storyboardCols+storyboardJoin+
		`WHERE sb.tenant_id = $1 AND sb.task_id = $2 AND sb.section_id = $3 AND sb.status <> 'superseded'`+lock,
		tenant, taskID, sectionID))
}

func validateStoryboardRefsTx(ctx context.Context, q queryer, tenant string, taskID int64, admIDs, evIDs, commitIDs []int64) error {
	if _, err := validateAdmissibilityRefsTx(ctx, q, tenant, taskID, admIDs); err != nil {
		return err
	}
	if len(commitIDs) > 0 {
		keep, dropped, err := currentCommitmentIDsTx(ctx, q, tenant, taskID, commitIDs)
		if err != nil {
			return err
		}
		if len(dropped) > 0 || len(keep) != len(uniqueIDs(commitIDs)) {
			return fmt.Errorf("%w: commitment %v is missing or unconfirmed", ErrCommitmentUnconfirmed, dropped)
		}
	}
	if len(evIDs) > 0 {
		states, err := evidenceStatesTx(ctx, q, tenant, taskID, evIDs)
		if err != nil {
			return err
		}
		for _, id := range evIDs {
			st, ok := states[id]
			if !ok {
				return fmt.Errorf("%w: retrieval evidence %d not found for task", ErrEvidenceInvalid, id)
			}
			if st.Status != "verified" || st.Relation != "supports" {
				return fmt.Errorf("%w: retrieval evidence %d is %s/%s (must be verified/supports)", ErrEvidenceInvalid, id, st.Status, st.Relation)
			}
		}
	}
	return nil
}

func sectionMappedReqCodesTx(ctx context.Context, q queryer, tenant string, sectionID int64) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT r.code FROM proposal_section_requirements m
		JOIN requirement_items r ON r.tenant_id = m.tenant_id AND r.id = m.requirement_id
		WHERE m.tenant_id = $1 AND m.section_id = $2 ORDER BY r.code`, tenant, sectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var codes []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		codes = append(codes, c)
	}
	return codes, rows.Err()
}

func validateStoryboardCompletenessTx(ctx context.Context, q queryer, tenant string, s *ProposalSection, sb *SectionStoryboard) error {
	if !compliance.ValidateThemeSentence(sb.ThemeSentence) {
		return fmt.Errorf("%w: theme_sentence must have at least 12 substantive characters", ErrStoryboardInvalid)
	}
	mappedCodes, err := sectionMappedReqCodesTx(ctx, q, tenant, s.ID)
	if err != nil {
		return err
	}
	var spCodes []string
	for i, sp := range sb.ScoringPoints {
		if strings.TrimSpace(sp.OurApproach) == "" || strings.TrimSpace(sp.BuyerBenefit) == "" {
			return fmt.Errorf("%w: scoring_points[%d] (%s) requires both our_approach and buyer_benefit", ErrStoryboardInvalid, i, sp.RequirementCode)
		}
		if strings.TrimSpace(sp.RequirementCode) != "" {
			spCodes = append(spCodes, strings.TrimSpace(sp.RequirementCode))
		}
	}
	if missing := compliance.StoryboardRequirementCoverageCheck(mappedCodes, spCodes); len(missing) > 0 {
		return fmt.Errorf("%w: storyboard scoring_points missing mapped requirements: %s", ErrStoryboardInvalid, strings.Join(missing, ", "))
	}
	if len(mappedCodes) == 0 && len(sb.ScoringPoints) == 0 {
		return fmt.Errorf("%w: storyboard requires at least one scoring_point entry", ErrStoryboardInvalid)
	}
	return validateStoryboardRefsTx(ctx, q, tenant, s.TaskID, sb.AdmissibilityItemIDs, sb.EvidenceIDs, sb.CommitmentIDs)
}

// UpsertStoryboard 创建或更新章节故事板（draft / pink_rejected 原地更新为 draft；pink_approved / pending_pink_review / stale 派生新版本）。
func (d *DB) UpsertStoryboard(ctx context.Context, tenant, actor string, in UpsertStoryboardInput) (*SectionStoryboard, error) {
	tenant = tenantOr(tenant)
	actor = strings.TrimSpace(actor)
	if actor == "" {
		actor = "system"
	}
	if !compliance.ValidateThemeSentence(in.ThemeSentence) {
		return nil, fmt.Errorf("%w: theme_sentence must have at least 12 substantive characters", ErrStoryboardInvalid)
	}
	in.AdmissibilityItemIDs = uniqueIDs(in.AdmissibilityItemIDs)
	in.EvidenceIDs = uniqueIDs(in.EvidenceIDs)
	in.CommitmentIDs = uniqueIDs(in.CommitmentIDs)
	if in.ScoringPoints == nil {
		in.ScoringPoints = []StoryboardScoringPoint{}
	}
	if in.ChartPlans == nil {
		in.ChartPlans = []StoryboardChartPlan{}
	}

	var out *SectionStoryboard
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		s, outlineStatus, approved, err := sectionGateTx(ctx, tx, tenant, in.TaskID, in.SectionID)
		if err != nil {
			return err
		}
		if err := requireApprovedSection(s, outlineStatus, approved); err != nil {
			return err
		}
		if err := validateStoryboardRefsTx(ctx, tx, tenant, in.TaskID, in.AdmissibilityItemIDs, in.EvidenceIDs, in.CommitmentIDs); err != nil {
			return err
		}
		spJSON, _ := json.Marshal(in.ScoringPoints)
		chartJSON, _ := json.Marshal(in.ChartPlans)

		cur, err := currentStoryboardTx(ctx, tx, tenant, in.TaskID, in.SectionID, true)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}

		if cur != nil && (cur.Status == StoryboardDraft || cur.Status == StoryboardPinkRejected) {
			if _, err := tx.ExecContext(ctx, `UPDATE section_storyboards
				SET outline_version = $2, theme_sentence = $3, scoring_points = $4::jsonb,
				    admissibility_item_ids = $5::bigint[], evidence_ids = $6::bigint[], commitment_ids = $7::bigint[],
				    chart_plans = $8::jsonb, status = 'draft', stale_reason = '', review_note = '',
				    submitted_by = '', submitted_at = NULL, reviewed_by = '', reviewed_at = NULL,
				    created_by = $9, updated_by = $9, approved_by = '', updated_at = NOW()
				WHERE id = $1`,
				cur.ID, approved.Version, strings.TrimSpace(in.ThemeSentence), string(spJSON),
				pgInt64Array(in.AdmissibilityItemIDs), pgInt64Array(in.EvidenceIDs), pgInt64Array(in.CommitmentIDs),
				string(chartJSON), actor); err != nil {
				return err
			}
		} else {
			if cur != nil {
				if _, err := tx.ExecContext(ctx, `UPDATE section_storyboards SET status = 'superseded', updated_by = $2, updated_at = NOW() WHERE id = $1`,
					cur.ID, actor); err != nil {
					return err
				}
			}
			var nextVer int
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) + 1 FROM section_storyboards WHERE tenant_id = $1 AND section_id = $2`,
				tenant, in.SectionID).Scan(&nextVer); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO section_storyboards(
				tenant_id, task_id, section_id, outline_version, version, theme_sentence, scoring_points,
				admissibility_item_ids, evidence_ids, commitment_ids, chart_plans, status, created_by, updated_by
			) VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8::bigint[],$9::bigint[],$10::bigint[],$11::jsonb,'draft',$12,$12)`,
				tenant, in.TaskID, in.SectionID, approved.Version, nextVer, strings.TrimSpace(in.ThemeSentence), string(spJSON),
				pgInt64Array(in.AdmissibilityItemIDs), pgInt64Array(in.EvidenceIDs), pgInt64Array(in.CommitmentIDs),
				string(chartJSON), actor); err != nil {
				return err
			}
		}
		out, err = currentStoryboardTx(ctx, tx, tenant, in.TaskID, in.SectionID, false)
		return err
	})
	return out, err
}

// SubmitStoryboard 提交章节故事板进入粉队评审（draft / pink_rejected -> pending_pink_review）。
func (d *DB) SubmitStoryboard(ctx context.Context, tenant string, taskID, sectionID int64, actor string) (*SectionStoryboard, error) {
	tenant = tenantOr(tenant)
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return nil, fmt.Errorf("%w: submitter actor is required", ErrStoryboardInvalid)
	}
	var out *SectionStoryboard
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		s, outlineStatus, approved, err := sectionGateTx(ctx, tx, tenant, taskID, sectionID)
		if err != nil {
			return err
		}
		if err := requireApprovedSection(s, outlineStatus, approved); err != nil {
			return err
		}
		sb, err := currentStoryboardTx(ctx, tx, tenant, taskID, sectionID, true)
		if errors.Is(err, ErrNotFound) {
			return ErrStoryboardMissing
		}
		if err != nil {
			return err
		}
		if sb.Status != StoryboardDraft && sb.Status != StoryboardPinkRejected {
			return fmt.Errorf("%w: storyboard v%d is %s, expected draft or pink_rejected", ErrStoryboardState, sb.Version, sb.Status)
		}
		if err := validateStoryboardCompletenessTx(ctx, tx, tenant, s, sb); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE section_storyboards
			SET status = 'pending_pink_review', submitted_by = $2, submitted_at = NOW(), updated_by = $2, updated_at = NOW()
			WHERE id = $1`, sb.ID, actor); err != nil {
			return err
		}
		out, err = currentStoryboardTx(ctx, tx, tenant, taskID, sectionID, false)
		return err
	})
	return out, err
}

// ApproveStoryboard 粉队评审通过故事板（pending_pink_review -> pink_approved），强制要求评审人与创建人/提交人不同。
func (d *DB) ApproveStoryboard(ctx context.Context, tenant string, taskID, sectionID int64, note, actor string) (*SectionStoryboard, error) {
	tenant = tenantOr(tenant)
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return nil, fmt.Errorf("%w: reviewer actor is required", ErrPinkSelfApproval)
	}
	var out *SectionStoryboard
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		s, outlineStatus, approved, err := sectionGateTx(ctx, tx, tenant, taskID, sectionID)
		if err != nil {
			return err
		}
		if err := requireApprovedSection(s, outlineStatus, approved); err != nil {
			return err
		}
		sb, err := currentStoryboardTx(ctx, tx, tenant, taskID, sectionID, true)
		if errors.Is(err, ErrNotFound) {
			return ErrStoryboardMissing
		}
		if err != nil {
			return err
		}
		if sb.Status != StoryboardPendingPink {
			return fmt.Errorf("%w: storyboard v%d is %s, expected pending_pink_review", ErrStoryboardState, sb.Version, sb.Status)
		}
		if actor == sb.CreatedBy || (sb.SubmittedBy != "" && actor == sb.SubmittedBy) {
			return fmt.Errorf("%w: reviewer %q cannot approve own storyboard (created_by=%q, submitted_by=%q)",
				ErrPinkSelfApproval, actor, sb.CreatedBy, sb.SubmittedBy)
		}
		if err := validateStoryboardCompletenessTx(ctx, tx, tenant, s, sb); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE section_storyboards
			SET status = 'pink_approved', review_note = $2, reviewed_by = $3, reviewed_at = NOW(),
			    approved_by = $3, updated_by = $3, updated_at = NOW()
			WHERE id = $1`, sb.ID, strings.TrimSpace(note), actor); err != nil {
			return err
		}
		out, err = currentStoryboardTx(ctx, tx, tenant, taskID, sectionID, false)
		return err
	})
	return out, err
}

// RejectStoryboard 粉队驳回故事板（pending_pink_review -> pink_rejected）。
func (d *DB) RejectStoryboard(ctx context.Context, tenant string, taskID, sectionID int64, note, actor string) (*SectionStoryboard, error) {
	tenant = tenantOr(tenant)
	actor = strings.TrimSpace(actor)
	note = strings.TrimSpace(note)
	if actor == "" || note == "" {
		return nil, fmt.Errorf("%w: reviewer actor and review_note are required for rejection", ErrStoryboardInvalid)
	}
	var out *SectionStoryboard
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		s, outlineStatus, approved, err := sectionGateTx(ctx, tx, tenant, taskID, sectionID)
		if err != nil {
			return err
		}
		if err := requireApprovedSection(s, outlineStatus, approved); err != nil {
			return err
		}
		sb, err := currentStoryboardTx(ctx, tx, tenant, taskID, sectionID, true)
		if errors.Is(err, ErrNotFound) {
			return ErrStoryboardMissing
		}
		if err != nil {
			return err
		}
		if sb.Status != StoryboardPendingPink {
			return fmt.Errorf("%w: storyboard v%d is %s, expected pending_pink_review", ErrStoryboardState, sb.Version, sb.Status)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE section_storyboards
			SET status = 'pink_rejected', review_note = $2, reviewed_by = $3, reviewed_at = NOW(),
			    updated_by = $3, updated_at = NOW()
			WHERE id = $1`, sb.ID, note, actor); err != nil {
			return err
		}
		out, err = currentStoryboardTx(ctx, tx, tenant, taskID, sectionID, false)
		return err
	})
	return out, err
}

// GetSectionStoryboard 返回章节当前活动故事板（不含 superseded）；不存在时返回 ErrNotFound。
func (d *DB) GetSectionStoryboard(ctx context.Context, tenant string, taskID, sectionID int64) (*SectionStoryboard, error) {
	return currentStoryboardTx(ctx, d, tenantOr(tenant), taskID, sectionID, false)
}

// ListTaskStoryboards 返回任务下全部章节的当前活动故事板（按章节顺序排列）。
func (d *DB) ListTaskStoryboards(ctx context.Context, tenant string, taskID int64) ([]*SectionStoryboard, error) {
	tenant = tenantOr(tenant)
	rows, err := d.QueryContext(ctx, `SELECT `+storyboardCols+storyboardJoin+
		`WHERE sb.tenant_id = $1 AND sb.task_id = $2 AND sb.status <> 'superseded' ORDER BY COALESCE(s.seq, 0), sb.section_id`,
		tenant, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*SectionStoryboard{}
	for rows.Next() {
		sb, err := scanStoryboard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	return out, rows.Err()
}
