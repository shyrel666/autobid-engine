package db

// D2 承诺台账：指标、招标阈值、承诺值、单位、责任人、依据、能力上限、状态与会签。
// 低于阈值、未确认、超能力未会签均通过 sentinel 在确认 / 引用时阻断；已引用台账变更由触发器使章节失效。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/compliance"
)

var (
	ErrCommitmentBelowThreshold  = errors.New("d2: commitment is below the tender threshold")
	ErrCommitmentSignoffRequired = errors.New("d2: commitment requires a different delivery sign-off")
	ErrCommitmentUnconfirmed     = errors.New("d2: commitment is not confirmed")
	ErrCommitmentMismatch        = errors.New("d2: section text does not match commitment ledger")
)

// 承诺指标与状态。
const (
	MetricDuration     = "duration"
	MetricWarranty     = "warranty"
	MetricSLAResponse  = "sla_response"
	MetricSLAOnsite    = "sla_onsite"
	MetricSLARepair    = "sla_repair"
	MetricAvailability = "availability"
	MetricTeamSize     = "team_size"
	MetricPrice        = "price"
	MetricOther        = "other"

	CommitmentDraft     = "draft"
	CommitmentConfirmed = "confirmed"
	CommitmentSignedOff = "signed_off"
	CommitmentRejected  = "rejected"
)

func validMetric(m string) bool {
	switch m {
	case MetricDuration, MetricWarranty, MetricSLAResponse, MetricSLAOnsite, MetricSLARepair, MetricAvailability, MetricTeamSize, MetricPrice, MetricOther:
		return true
	}
	return false
}

// Commitment 是承诺台账的一行。
type Commitment struct {
	ID                         int64      `json:"id"`
	TenantID                   string     `json:"tenant_id"`
	TaskID                     int64      `json:"task_id"`
	Code                       string     `json:"code"`
	RequirementID              *int64     `json:"requirement_item_id,omitempty"`
	RequirementCode            string     `json:"requirement_code,omitempty"`
	Metric                     string     `json:"metric"`
	MetricLabel                string     `json:"metric_label"`
	Unit                       string     `json:"unit"`
	ThresholdCmp               string     `json:"threshold_cmp"`
	TenderThresholdRaw         string     `json:"tender_threshold_raw"`
	TenderThresholdValue       *float64   `json:"tender_threshold_value,omitempty"`
	CommittedRaw               string     `json:"committed_raw"`
	CommittedValue             *float64   `json:"committed_value,omitempty"`
	CapabilityLimit            *float64   `json:"capability_limit,omitempty"`
	CapabilityBasis            string     `json:"capability_basis"`
	Owner                      string     `json:"owner"`
	Rationale                  string     `json:"rationale"`
	SourceAdmissibilityItemIDs []int64    `json:"source_admissibility_item_ids"`
	Status                     string     `json:"status"`
	RequiresDeliverySignoff    bool       `json:"requires_delivery_signoff"`
	SignedOffBy                string     `json:"signed_off_by,omitempty"`
	SignedOffAt                *time.Time `json:"signed_off_at,omitempty"`
	Version                    int        `json:"version"`
	CreatedBy                  string     `json:"created_by,omitempty"`
	UpdatedBy                  string     `json:"updated_by,omitempty"`
	ApprovedBy                 string     `json:"approved_by,omitempty"`
	CreatedAt                  time.Time  `json:"created_at"`
	UpdatedAt                  time.Time  `json:"updated_at"`
	// ActorLegacy 为 true 表示创建人是升级前的声明式姓名（R2），会签时不能据此判定"另一个人"。
	ActorLegacy bool `json:"actor_legacy,omitempty"`
}

// CommitmentInput 是写入 / 修订台账的字段；Code 必填。
type CommitmentInput struct {
	Code                       string   `json:"code"`
	RequirementID              *int64   `json:"requirement_item_id"`
	Metric                     string   `json:"metric"`
	MetricLabel                string   `json:"metric_label"`
	Unit                       string   `json:"unit"`
	ThresholdCmp               string   `json:"threshold_cmp"`
	TenderThresholdRaw         string   `json:"tender_threshold_raw"`
	TenderThresholdValue       *float64 `json:"tender_threshold_value"`
	CommittedRaw               string   `json:"committed_raw"`
	CommittedValue             *float64 `json:"committed_value"`
	CapabilityLimit            *float64 `json:"capability_limit"`
	CapabilityBasis            string   `json:"capability_basis"`
	Owner                      string   `json:"owner"`
	Rationale                  string   `json:"rationale"`
	SourceAdmissibilityItemIDs []int64  `json:"source_admissibility_item_ids"`
}

const commitmentCols = `c.id, c.tenant_id, c.task_id, c.code, c.requirement_id, COALESCE(r.code,''), c.metric, c.metric_label, c.unit,
	c.threshold_cmp, c.tender_threshold_raw, c.tender_threshold_value::float8, c.committed_raw, c.committed_value::float8,
	c.capability_limit::float8, c.capability_basis, c.owner, c.rationale, to_jsonb(c.source_admissibility_item_ids),
	c.status, c.requires_delivery_signoff, c.signed_off_by, c.signed_off_at, c.version, c.created_by, c.updated_by, c.approved_by,
	c.created_at, c.updated_at, c.actor_legacy`

func scanCommitment(r rowScanner) (*Commitment, error) {
	var (
		c          Commitment
		reqID      sql.NullInt64
		threshold  sql.NullFloat64
		committed  sql.NullFloat64
		capability sql.NullFloat64
		source     []byte
		signedAt   sql.NullTime
	)
	err := r.Scan(&c.ID, &c.TenantID, &c.TaskID, &c.Code, &reqID, &c.RequirementCode, &c.Metric, &c.MetricLabel, &c.Unit,
		&c.ThresholdCmp, &c.TenderThresholdRaw, &threshold, &c.CommittedRaw, &committed, &capability, &c.CapabilityBasis,
		&c.Owner, &c.Rationale, &source, &c.Status, &c.RequiresDeliverySignoff, &c.SignedOffBy, &signedAt, &c.Version,
		&c.CreatedBy, &c.UpdatedBy, &c.ApprovedBy, &c.CreatedAt, &c.UpdatedAt, &c.ActorLegacy)
	if err != nil {
		return nil, err
	}
	if reqID.Valid {
		v := reqID.Int64
		c.RequirementID = &v
	}
	if threshold.Valid {
		v := threshold.Float64
		c.TenderThresholdValue = &v
	}
	if committed.Valid {
		v := committed.Float64
		c.CommittedValue = &v
	}
	if capability.Valid {
		v := capability.Float64
		c.CapabilityLimit = &v
	}
	c.SourceAdmissibilityItemIDs = []int64{}
	_ = json.Unmarshal(source, &c.SourceAdmissibilityItemIDs)
	if c.SourceAdmissibilityItemIDs == nil {
		c.SourceAdmissibilityItemIDs = []int64{}
	}
	c.SignedOffAt = nullTimePtr(signedAt)
	return &c, nil
}

const commitmentJoin = ` FROM commitment_ledger c LEFT JOIN requirement_items r
	ON r.tenant_id=c.tenant_id AND r.task_id=c.task_id AND r.id=c.requirement_id `

func (d *DB) GetCommitment(ctx context.Context, tenant string, taskID int64, code string) (*Commitment, error) {
	return scanCommitment(d.QueryRowContext(ctx, `SELECT `+commitmentCols+commitmentJoin+
		`WHERE c.tenant_id=$1 AND c.task_id=$2 AND c.code=$3`, tenantOr(tenant), taskID, code))
}

// ListCommitments 列出任务台账。
func (d *DB) ListCommitments(ctx context.Context, tenant string, taskID int64) ([]*Commitment, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+commitmentCols+commitmentJoin+
		`WHERE c.tenant_id=$1 AND c.task_id=$2 ORDER BY c.metric, c.code`, tenantOr(tenant), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Commitment
	for rows.Next() {
		c, err := scanCommitment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func validateCommitmentInput(in *CommitmentInput) error {
	if strings.TrimSpace(in.Code) == "" {
		return errors.New("d2: commitment code is required")
	}
	if !validMetric(in.Metric) {
		return fmt.Errorf("d2: invalid commitment metric %q", in.Metric)
	}
	switch in.ThresholdCmp {
	case "", ">=", "<=", "=":
	default:
		return fmt.Errorf("d2: invalid threshold_cmp %q", in.ThresholdCmp)
	}
	return nil
}

func (d *DB) validateCommitmentSources(ctx context.Context, tenant string, taskID int64, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	var count int
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM admissibility_items i
		JOIN admissibility_runs run ON run.tenant_id=i.tenant_id AND run.id=i.run_id
		WHERE i.tenant_id=$1 AND run.task_id=$2 AND run.status='current' AND i.verdict='admissible'
		  AND i.id = ANY($3::bigint[])`, tenant, taskID, pgInt64Array(uniqueIDs(ids))).Scan(&count); err != nil {
		return err
	}
	if count != len(uniqueIDs(ids)) {
		return fmt.Errorf("%w: commitment source admissibility items are not current admissible", ErrEvidenceInadmissible)
	}
	return nil
}

// UpsertCommitment 写入 / 修订台账草稿；确认后的承诺发生实质变更会退回 draft 并要求重新确认。
func (d *DB) UpsertCommitment(ctx context.Context, tenant string, taskID int64, in CommitmentInput, actor string) (*Commitment, error) {
	tenant = tenantOr(tenant)
	if err := validateCommitmentInput(&in); err != nil {
		return nil, err
	}
	if in.CommittedValue == nil {
		if v, ok := parseCommitmentValue(in.CommittedRaw, in.Unit); ok {
			in.CommittedValue = &v
		}
	}
	if in.TenderThresholdValue == nil {
		if v, ok := parseCommitmentValue(in.TenderThresholdRaw, in.Unit); ok {
			in.TenderThresholdValue = &v
		}
	}
	in.SourceAdmissibilityItemIDs = uniqueIDs(in.SourceAdmissibilityItemIDs)
	if err := d.validateCommitmentSources(ctx, tenant, taskID, in.SourceAdmissibilityItemIDs); err != nil {
		return nil, err
	}
	if in.RequirementID != nil {
		var ok bool
		if err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM requirement_items WHERE tenant_id=$1 AND task_id=$2 AND id=$3)`,
			tenant, taskID, *in.RequirementID).Scan(&ok); err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%w: requirement %d", ErrNotFound, *in.RequirementID)
		}
	}
	cmp := in.ThresholdCmp
	if cmp == "" {
		cmp = compliance.CmpGE
	}
	label := strings.TrimSpace(in.MetricLabel)
	if label == "" {
		label = in.Metric
	}
	out, err := scanCommitment(d.QueryRowContext(ctx, `WITH upsert AS (
			INSERT INTO commitment_ledger(tenant_id, task_id, code, requirement_id, metric, metric_label, unit, threshold_cmp,
				tender_threshold_raw, tender_threshold_value, committed_raw, committed_value, capability_limit, capability_basis,
				owner, rationale, source_admissibility_item_ids, status, created_by, updated_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17::bigint[],'draft',COALESCE(NULLIF($18,''),''),COALESCE(NULLIF($18,''),''))
			ON CONFLICT (tenant_id, task_id, code) DO UPDATE SET
				requirement_id=EXCLUDED.requirement_id, metric=EXCLUDED.metric, metric_label=EXCLUDED.metric_label,
				unit=EXCLUDED.unit, threshold_cmp=EXCLUDED.threshold_cmp, tender_threshold_raw=EXCLUDED.tender_threshold_raw,
				tender_threshold_value=EXCLUDED.tender_threshold_value, committed_raw=EXCLUDED.committed_raw,
				committed_value=EXCLUDED.committed_value, capability_limit=EXCLUDED.capability_limit,
				capability_basis=EXCLUDED.capability_basis, owner=EXCLUDED.owner, rationale=EXCLUDED.rationale,
				source_admissibility_item_ids=EXCLUDED.source_admissibility_item_ids,
				version=commitment_ledger.version + CASE WHEN (commitment_ledger.committed_value, commitment_ledger.tender_threshold_value,
					commitment_ledger.capability_limit, commitment_ledger.unit, commitment_ledger.metric)
					IS DISTINCT FROM (EXCLUDED.committed_value, EXCLUDED.tender_threshold_value, EXCLUDED.capability_limit,
					EXCLUDED.unit, EXCLUDED.metric) THEN 1 ELSE 0 END,
				status=CASE WHEN commitment_ledger.status <> 'draft' AND
					(commitment_ledger.committed_value, commitment_ledger.tender_threshold_value, commitment_ledger.capability_limit,
					 commitment_ledger.unit, commitment_ledger.metric)
					IS DISTINCT FROM (EXCLUDED.committed_value, EXCLUDED.tender_threshold_value, EXCLUDED.capability_limit,
					EXCLUDED.unit, EXCLUDED.metric) THEN 'draft' ELSE commitment_ledger.status END,
				signed_off_by=CASE WHEN commitment_ledger.status <> 'draft' AND
					(commitment_ledger.committed_value, commitment_ledger.tender_threshold_value, commitment_ledger.capability_limit,
					 commitment_ledger.unit, commitment_ledger.metric)
					IS DISTINCT FROM (EXCLUDED.committed_value, EXCLUDED.tender_threshold_value, EXCLUDED.capability_limit,
					EXCLUDED.unit, EXCLUDED.metric) THEN '' ELSE commitment_ledger.signed_off_by END,
				signed_off_at=CASE WHEN commitment_ledger.status <> 'draft' AND
					(commitment_ledger.committed_value, commitment_ledger.tender_threshold_value, commitment_ledger.capability_limit,
					 commitment_ledger.unit, commitment_ledger.metric)
					IS DISTINCT FROM (EXCLUDED.committed_value, EXCLUDED.tender_threshold_value, EXCLUDED.capability_limit,
					EXCLUDED.unit, EXCLUDED.metric) THEN NULL ELSE commitment_ledger.signed_off_at END,
				updated_by=EXCLUDED.updated_by
			RETURNING *)
		SELECT `+commitmentCols+` FROM upsert c LEFT JOIN requirement_items r
			ON r.tenant_id=c.tenant_id AND r.task_id=c.task_id AND r.id=c.requirement_id`,
		tenant, taskID, strings.TrimSpace(in.Code), in.RequirementID, in.Metric, label, strings.TrimSpace(in.Unit), cmp,
		strings.TrimSpace(in.TenderThresholdRaw), in.TenderThresholdValue, strings.TrimSpace(in.CommittedRaw), in.CommittedValue,
		in.CapabilityLimit, strings.TrimSpace(in.CapabilityBasis), strings.TrimSpace(in.Owner), strings.TrimSpace(in.Rationale),
		pgInt64Array(in.SourceAdmissibilityItemIDs), actor))
	if err != nil {
		return nil, err
	}
	return out, nil
}

// parseCommitmentValue 从原文解析数值，并换算到台账声明的单位：阈值、承诺值与能力上限在同一条台账里必须同口径，
// 页面与接口直接填写的数就是按 unit 计的（D3 的渲染与核验也按这个口径读）。早先这里一律归一到天 / 分钟，于是
// 「阈值原文 不少于 3 年 + 承诺值 5（单位 年）」会被比成 5 < 1095 而拒绝确认。单位为空或与原文量纲不同时保持归一值。
// 原文里没有带单位的数值时退回提取首个数字。
func parseCommitmentValue(raw, unit string) (float64, bool) {
	if qs := compliance.Quantities(raw); len(qs) > 0 {
		if dim, factor := compliance.UnitDim(unit); strings.TrimSpace(unit) != "" && dim == qs[0].Dim && factor > 0 {
			return qs[0].Value / factor, true
		}
		return qs[0].Value, true
	}
	var b strings.Builder
	dot := false
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if r == '.' && !dot {
			dot = true
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSuffix(b.String(), "."), 64)
	return v, err == nil
}

func thresholdSatisfied(c *Commitment) bool {
	if c.TenderThresholdValue == nil || c.CommittedValue == nil {
		return true
	}
	const eps = 1e-9
	switch c.ThresholdCmp {
	case compliance.CmpLE:
		return *c.CommittedValue <= *c.TenderThresholdValue+eps
	case compliance.CmpLT:
		return *c.CommittedValue+eps < *c.TenderThresholdValue
	case compliance.CmpGE:
		return *c.CommittedValue+eps >= *c.TenderThresholdValue
	case "=":
		return compliance.Close(*c.CommittedValue, *c.TenderThresholdValue)
	}
	return true
}

func capabilityExceeded(c *Commitment) bool {
	if c.CapabilityLimit == nil || c.CommittedValue == nil {
		return false
	}
	return *c.CommittedValue > *c.CapabilityLimit+1e-9
}

func confirmCommitment(c *Commitment) error {
	if c.Status == CommitmentRejected {
		return fmt.Errorf("%w: commitment %s is rejected", ErrCommitmentUnconfirmed, c.Code)
	}
	if c.RequirementID == nil && c.Metric != MetricPrice {
		return fmt.Errorf("%w: non-price commitment %s requires a tender requirement source", ErrCommitmentUnconfirmed, c.Code)
	}
	if strings.TrimSpace(c.Unit) == "" || strings.TrimSpace(c.Owner) == "" || strings.TrimSpace(c.Rationale) == "" {
		return fmt.Errorf("%w: commitment %s requires unit, owner and rationale", ErrCommitmentUnconfirmed, c.Code)
	}
	if strings.TrimSpace(c.TenderThresholdRaw) != "" && c.TenderThresholdValue == nil {
		return fmt.Errorf("%w: commitment %s threshold value cannot be normalized", ErrCommitmentUnconfirmed, c.Code)
	}
	if strings.TrimSpace(c.CommittedRaw) != "" && c.CommittedValue == nil {
		return fmt.Errorf("%w: commitment %s committed value cannot be normalized", ErrCommitmentUnconfirmed, c.Code)
	}
	if !thresholdSatisfied(c) {
		return fmt.Errorf("%w: %s %s %v does not satisfy %s %v", ErrCommitmentBelowThreshold, c.Code, c.CommittedRaw, derefFloat(c.CommittedValue), c.ThresholdCmp, derefFloat(c.TenderThresholdValue))
	}
	if capabilityExceeded(c) {
		if c.SignedOffBy == "" {
			return fmt.Errorf("%w: %s exceeds capability %v", ErrCommitmentSignoffRequired, c.Code, derefFloat(c.CapabilityLimit))
		}
		if c.SignedOffBy == c.CreatedBy {
			return fmt.Errorf("%w: sign-off must be by a different operator", ErrCommitmentSignoffRequired)
		}
	}
	return nil
}

func derefFloat(v *float64) any {
	if v == nil {
		return "未设置"
	}
	return math.Round(*v*10000) / 10000
}

// ConfirmCommitment 确认台账：低于阈值 / 超能力未会签 / 依据不完整都会返回 sentinel。
func (d *DB) ConfirmCommitment(ctx context.Context, tenant string, taskID int64, code, actor string) (*Commitment, error) {
	tenant = tenantOr(tenant)
	c, err := scanCommitment(d.QueryRowContext(ctx, `SELECT `+commitmentCols+commitmentJoin+
		`WHERE c.tenant_id=$1 AND c.task_id=$2 AND c.code=$3 FOR UPDATE OF c`, tenant, taskID, code))
	if err != nil {
		return nil, err
	}
	if err := d.validateCommitmentSources(ctx, tenant, taskID, c.SourceAdmissibilityItemIDs); err != nil {
		return nil, err
	}
	if err := confirmCommitment(c); err != nil {
		return nil, err
	}
	status := CommitmentConfirmed
	if c.SignedOffBy != "" && c.RequiresDeliverySignoff {
		status = CommitmentSignedOff
	}
	out, err := scanCommitment(d.QueryRowContext(ctx, `WITH upd AS (
			UPDATE commitment_ledger SET status=$4, requires_delivery_signoff=$5, approved_by=COALESCE(NULLIF($6,''),''), updated_by=COALESCE(NULLIF($6,''),'')
			WHERE tenant_id=$1 AND task_id=$2 AND code=$3 RETURNING *)
		SELECT `+commitmentCols+` FROM upd c LEFT JOIN requirement_items r
			ON r.tenant_id=c.tenant_id AND r.task_id=c.task_id AND r.id=c.requirement_id`,
		tenant, taskID, code, status, capabilityExceeded(c), actor))
	return out, err
}

// SignOffCommitment 由交付负责人会签；会签人必须与创建人不同。
func (d *DB) SignOffCommitment(ctx context.Context, tenant string, taskID int64, code, actor, note string) (*Commitment, error) {
	tenant = tenantOr(tenant)
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return nil, errors.New("d2: sign-off actor is required")
	}
	c, err := scanCommitment(d.QueryRowContext(ctx, `SELECT `+commitmentCols+commitmentJoin+
		`WHERE c.tenant_id=$1 AND c.task_id=$2 AND c.code=$3 FOR UPDATE OF c`, tenant, taskID, code))
	if err != nil {
		return nil, err
	}
	if c.Status == CommitmentRejected {
		return nil, fmt.Errorf("%w: commitment %s is rejected", ErrCommitmentUnconfirmed, code)
	}
	creator, err := separationPrincipalQ(ctx, d, tenant, c.CreatedBy, c.ActorLegacy)
	if err != nil {
		return nil, err
	}
	if actor == creator || actor == c.CreatedBy {
		return nil, fmt.Errorf("%w: sign-off must be by a different operator than the creator", ErrCommitmentSignoffRequired)
	}
	if !thresholdSatisfied(c) {
		return nil, fmt.Errorf("%w: %s does not satisfy the tender threshold", ErrCommitmentBelowThreshold, code)
	}
	rationale := c.Rationale
	if strings.TrimSpace(note) != "" {
		rationale = strings.TrimSpace(rationale + "\n会签说明：" + note)
	}
	out, err := scanCommitment(d.QueryRowContext(ctx, `WITH upd AS (
			UPDATE commitment_ledger SET status='signed_off', requires_delivery_signoff=true, signed_off_by=$4,
				signed_off_at=now(), approved_by=$4, rationale=$5, updated_by=$4
			WHERE tenant_id=$1 AND task_id=$2 AND code=$3 RETURNING *)
		SELECT `+commitmentCols+` FROM upd c LEFT JOIN requirement_items r
			ON r.tenant_id=c.tenant_id AND r.task_id=c.task_id AND r.id=c.requirement_id`,
		tenant, taskID, code, actor, rationale))
	return out, err
}

// CommitmentRenderValue 是渲染器写入正文的规范值（展示名 + 原文值 + 单位）。
func CommitmentRenderValue(c *Commitment) string {
	raw := strings.TrimSpace(c.CommittedRaw)
	if raw == "" && c.CommittedValue != nil {
		raw = fmt.Sprintf("%g", *c.CommittedValue)
	}
	return strings.TrimSpace(c.MetricLabel + " " + raw + " " + c.Unit)
}

// CheckCommitmentText 校验正文包含引用台账的规范值；不一致返回 ErrCommitmentMismatch。
func CheckCommitmentText(content string, commitments []*Commitment) error {
	for _, c := range commitments {
		if c == nil {
			continue
		}
		raw := strings.TrimSpace(c.CommittedRaw)
		if raw == "" {
			raw = strings.TrimSpace(CommitmentRenderValue(c))
		}
		if raw == "" {
			continue
		}
		if !strings.Contains(content, raw) {
			return fmt.Errorf("%w: %s should render as %q", ErrCommitmentMismatch, c.Code, CommitmentRenderValue(c))
		}
	}
	return nil
}
