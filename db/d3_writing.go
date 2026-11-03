package db

// D3 引用标记确定性渲染、句子级证据绑定核验与写作 E 级指标持久化：
// 1. RenderSectionGroundedDraft：从任务当前可采企业证据、已核验知识库证据与已确认承诺台账确定性展开
//    [[EVID:...]] / [[KB:...]] / [[COMMIT:...]]，若故事板已批准则限制在故事板声明的白名单内；
// 2. VerifyAndPersistSectionWriting：对章节修订执行句子级断言抽取、证据/承诺核验与针对性/套话相似度计算，
//    结果持久化至 section_writing_verifications；
// 3. 在启用了 D3 的任务上（存在故事板、写作核验记录或显式 D3 引用标记）：
//    - ClaimSectionWrite 要求章节故事板已通过粉队评审（pink_approved）；
//    - SaveSectionRevision 自动展开引用 token、同步证据/承诺 ID，并在 ungrounded_claim_count > 0
//      或针对性/套话指标不达标时拒绝成为当前主版本；
//    - ApproveSectionRevision 强制执行红队评审人与章节作者分离（approved_by != rev.created_by），
//      并要求当前修订的写作核验状态为 passed。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/compliance"
)

// SectionWritingVerification 是章节修订的 D3 写作核验记录（Domain C）。
type SectionWritingVerification struct {
	ID                       int64                                  `json:"id"`
	TenantID                 string                                 `json:"tenant_id"`
	TaskID                   int64                                  `json:"task_id"`
	SectionID                int64                                  `json:"section_id"`
	SectionCode              string                                 `json:"section_code,omitempty"`
	SectionTitle             string                                 `json:"section_title,omitempty"`
	SectionRevID             int64                                  `json:"section_rev_id"`
	RevNo                    int                                    `json:"rev_no"`
	StoryboardID             *int64                                 `json:"storyboard_id,omitempty"`
	RuleVersion              string                                 `json:"rule_version"`
	Verdict                  string                                 `json:"verdict"`
	StaleReason              string                                 `json:"stale_reason,omitempty"`
	UngroundedClaimCount     int                                    `json:"ungrounded_claim_count"`
	FactBindingRate          float64                                `json:"fact_binding_rate"`
	CommitmentBindingRate    float64                                `json:"commitment_binding_rate"`
	SpecificityRatio         float64                                `json:"specificity_ratio"`
	KBSimilarityMax          float64                                `json:"kb_similarity_max"`
	BoilerplateSimilarityMax float64                                `json:"boilerplate_similarity_max"`
	Claims                   []compliance.SentenceClaim             `json:"claims"`
	Metrics                  compliance.WritingVerificationMetrics  `json:"metrics"`
	Reasons                  []compliance.WritingVerificationReason `json:"reasons"`
	CreatedBy                string                                 `json:"created_by"`
	UpdatedBy                string                                 `json:"updated_by"`
	ApprovedBy               string                                 `json:"approved_by,omitempty"`
	CreatedAt                time.Time                              `json:"created_at"`
	UpdatedAt                time.Time                              `json:"updated_at"`
}

const writingVerifCols = `wv.id, wv.tenant_id, wv.task_id, wv.section_id, COALESCE(s.section_code,''), COALESCE(s.title,''),
	wv.section_rev_id, wv.rev_no, wv.storyboard_id, wv.rule_version, wv.verdict, wv.stale_reason,
	wv.ungrounded_claim_count, wv.fact_binding_rate, wv.commitment_binding_rate, wv.specificity_ratio,
	wv.kb_similarity_max, wv.boilerplate_similarity_max, wv.claims, wv.metrics, wv.reasons,
	wv.created_by, wv.updated_by, wv.approved_by, wv.created_at, wv.updated_at`

const writingVerifJoin = ` FROM section_writing_verifications wv
	LEFT JOIN proposal_sections s ON s.tenant_id = wv.tenant_id AND s.id = wv.section_id `

func scanWritingVerification(r rowScanner) (*SectionWritingVerification, error) {
	var (
		wv                          SectionWritingVerification
		sbID                        sql.NullInt64
		claimsRaw, metricsRaw, rRaw []byte
	)
	err := r.Scan(
		&wv.ID, &wv.TenantID, &wv.TaskID, &wv.SectionID, &wv.SectionCode, &wv.SectionTitle,
		&wv.SectionRevID, &wv.RevNo, &sbID, &wv.RuleVersion, &wv.Verdict, &wv.StaleReason,
		&wv.UngroundedClaimCount, &wv.FactBindingRate, &wv.CommitmentBindingRate, &wv.SpecificityRatio,
		&wv.KBSimilarityMax, &wv.BoilerplateSimilarityMax, &claimsRaw, &metricsRaw, &rRaw,
		&wv.CreatedBy, &wv.UpdatedBy, &wv.ApprovedBy, &wv.CreatedAt, &wv.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if sbID.Valid {
		wv.StoryboardID = &sbID.Int64
	}
	wv.Claims = []compliance.SentenceClaim{}
	_ = json.Unmarshal(claimsRaw, &wv.Claims)
	if wv.Claims == nil {
		wv.Claims = []compliance.SentenceClaim{}
	}
	_ = json.Unmarshal(metricsRaw, &wv.Metrics)
	wv.Reasons = []compliance.WritingVerificationReason{}
	_ = json.Unmarshal(rRaw, &wv.Reasons)
	if wv.Reasons == nil {
		wv.Reasons = []compliance.WritingVerificationReason{}
	}
	return &wv, nil
}

func taskHasD3ActivityQ(ctx context.Context, q queryer, tenant string, taskID int64) (bool, error) {
	var active bool
	err := q.QueryRowContext(ctx, `SELECT
		EXISTS (SELECT 1 FROM section_storyboards WHERE tenant_id = $1 AND task_id = $2)
		OR EXISTS (SELECT 1 FROM section_writing_verifications WHERE tenant_id = $1 AND task_id = $2)`,
		tenant, taskID).Scan(&active)
	return active, err
}

func contentHasD3Tokens(md string) bool {
	return strings.Contains(md, "[[EVID:") || strings.Contains(md, "[[KB:") || strings.Contains(md, "[[COMMIT:") ||
		strings.Contains(md, "[EVID:") || strings.Contains(md, "[KB:") || strings.Contains(md, "[COMMIT:")
}

// loadTaskGroundedFactSourcesTx 加载任务下所有可采性运行明细及关联企业证据（用于引用展开与事实核验）。
func loadTaskGroundedFactSourcesTx(ctx context.Context, q queryer, tenant string, taskID int64) ([]compliance.GroundedFactSource, error) {
	rows, err := q.QueryContext(ctx, `SELECT i.id, req.code, b.evidence_domain, b.evidence_id, i.verdict, r.status
		FROM admissibility_items i
		JOIN admissibility_runs r ON r.tenant_id = i.tenant_id AND r.id = i.run_id
		JOIN requirement_evidence_bindings b ON b.tenant_id = i.tenant_id AND b.id = i.binding_id
		JOIN requirement_items req ON req.tenant_id = r.tenant_id AND req.id = r.requirement_id
		WHERE i.tenant_id = $1 AND r.task_id = $2
		ORDER BY CASE WHEN r.status = 'current' THEN 0 ELSE 1 END, i.id DESC`,
		tenant, taskID)
	if err != nil {
		return nil, err
	}
	type rawItem struct {
		itemID  int64
		reqCode string
		domain  string
		evID    int64
		verdict string
		runSt   string
	}
	var items []rawItem
	for rows.Next() {
		var it rawItem
		if err := rows.Scan(&it.itemID, &it.reqCode, &it.domain, &it.evID, &it.verdict, &it.runSt); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []compliance.GroundedFactSource
	for _, it := range items {
		verdict := it.verdict
		if it.runSt != "current" {
			verdict = "stale"
		}
		src := compliance.GroundedFactSource{
			AdmissibilityItemID: it.itemID,
			RequirementCode:     it.reqCode,
			EvidenceDomain:      it.domain,
			EvidenceID:          it.evID,
			Verdict:             verdict,
		}
		origs, err := loadOriginalsTx(ctx, q, tenant, it.domain, it.evID)
		if err != nil {
			return nil, err
		}
		for _, o := range origs {
			if !o.Superseded() && strings.TrimSpace(o.PageLabel) != "" {
				src.PageLabels = append(src.PageLabels, strings.TrimSpace(o.PageLabel))
			}
		}
		switch it.domain {
		case EvidenceDomainCredential:
			c, err := scanCredential(q.QueryRowContext(ctx, `SELECT `+credentialCols+` FROM company_credentials WHERE tenant_id = $1 AND id = $2`,
				tenant, it.evID))
			if err == nil && c.DeletedAt == nil {
				src.Code = c.Code
				src.Category = c.Category
				src.Title = c.Title
				src.SubjectName = c.SubjectName
				src.SubjectType = c.SubjectType
				src.CertificateNo = c.CertificateNo
				src.Scope = c.Scope
				var attrs map[string]any
				_ = json.Unmarshal(c.Attributes, &attrs)
				if v := attrString(attrs, "level", "qualification_level"); v != "" {
					src.QualificationLevel = v
				}
				if v := attrString(attrs, "model", "model_spec"); v != "" {
					src.ModelSpec = v
				}
				if m := attrInt(attrs, "social_security_months"); m > 0 {
					src.SocialSecurityMonths = m
				}
			} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			} else {
				src.Verdict = compliance.Inadmissible
			}
		case EvidenceDomainPerformance:
			p, err := scanPerformance(q.QueryRowContext(ctx, `SELECT `+performanceCols+` FROM company_performance WHERE tenant_id = $1 AND id = $2`,
				tenant, it.evID))
			if err == nil && p.DeletedAt == nil {
				src.Code = p.Code
				src.Category = "performance"
				src.Title = p.ProjectName
				src.ProjectName = p.ProjectName
				src.SubjectName = p.SubjectName
				src.SubjectType = p.SubjectType
				src.ClientName = p.Buyer
				src.Scope = p.Scope
				if p.AcceptanceDate != nil {
					src.AcceptanceDate = *p.AcceptanceDate
				}
				if amt, err := strconv.ParseFloat(strings.TrimSpace(p.Amount), 64); err == nil {
					src.ContractAmount = amt
				}
			} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			} else {
				src.Verdict = compliance.Inadmissible
			}
		}
		out = append(out, src)
	}
	return out, nil
}

func attrString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			if s := strings.TrimSpace(fmt.Sprint(v)); s != "" {
				return s
			}
		}
	}
	return ""
}

func attrInt(m map[string]any, key string) int {
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(x))
		return n
	}
	return 0
}

func loadTaskGroundedKBSourcesTx(ctx context.Context, q queryer, tenant string, taskID int64) ([]compliance.GroundedKBSource, []string, error) {
	rows, err := q.QueryContext(ctx, `SELECT e.id, e.status, e.relation, COALESCE(d.title,''), COALESCE(e.snapshot::text,''), COALESCE(c.content,'')
		FROM retrieval_evidence e
		JOIN retrieval_runs r ON r.tenant_id = e.tenant_id AND r.id = e.run_id
		LEFT JOIN knowledge_chunks c ON c.tenant_id = e.tenant_id AND e.source_domain = 'knowledge_chunk' AND c.id = e.source_id
		LEFT JOIN knowledge_documents d ON d.tenant_id = e.tenant_id AND d.id = e.doc_id
		WHERE e.tenant_id = $1 AND r.task_id = $2 ORDER BY e.id`, tenant, taskID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var sources []compliance.GroundedKBSource
	var chunks []string
	for rows.Next() {
		var (
			id               int64
			status, rel      string
			title, snap, cnt string
		)
		if err := rows.Scan(&id, &status, &rel, &title, &snap, &cnt); err != nil {
			return nil, nil, err
		}
		var snapMap map[string]any
		_ = json.Unmarshal([]byte(snap), &snapMap)
		excerpt := attrString(snapMap, "excerpt", "content", "summary", "title")
		if excerpt == "" {
			excerpt = truncateRunes(strings.TrimSpace(cnt), 240)
		}
		sources = append(sources, compliance.GroundedKBSource{
			EvidenceID: id,
			Status:     status,
			Relation:   rel,
			Title:      title,
			Excerpt:    excerpt,
		})
		if strings.TrimSpace(cnt) != "" {
			chunks = append(chunks, cnt)
		}
	}
	return sources, chunks, rows.Err()
}

func loadTaskGroundedCommitmentsTx(ctx context.Context, q queryer, tenant string, taskID int64) ([]compliance.GroundedCommitmentSource, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+commitmentCols+commitmentJoin+
		`WHERE c.tenant_id = $1 AND c.task_id = $2 ORDER BY c.code`, tenant, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []compliance.GroundedCommitmentSource
	for rows.Next() {
		c, err := scanCommitment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, compliance.GroundedCommitmentSource{
			ID:             c.ID,
			CommitmentCode: c.Code,
			Category:       c.Metric,
			Label:          c.MetricLabel,
			Dimension:      commitmentMetricDim(c.Metric),
			CommittedValue: c.CommittedValue,
			CommittedUnit:  c.Unit,
			CommittedRaw:   c.CommittedRaw,
			Status:         c.Status,
		})
	}
	return out, rows.Err()
}

func commitmentMetricDim(metric string) string {
	switch metric {
	case MetricDuration, compliance.MetricDuration, MetricWarranty:
		return compliance.DimDuration
	case MetricSLAResponse, MetricSLAOnsite, MetricSLARepair:
		return compliance.DimTime
	case MetricAvailability, compliance.MetricAvailability:
		return compliance.DimPercent
	case MetricTeamSize:
		return compliance.DimCount
	default:
		return ""
	}
}

func loadSectionRuleRequirementsTx(ctx context.Context, q queryer, tenant string, sectionID int64) ([]compliance.Requirement, []string, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+reqCols+` FROM requirement_items WHERE tenant_id = $1 AND id IN
		(SELECT requirement_id FROM proposal_section_requirements WHERE tenant_id = $1 AND section_id = $2) ORDER BY code`,
		tenant, sectionID)
	if err != nil {
		return nil, nil, err
	}
	var reqItems []*RequirementItem
	for rows.Next() {
		it, err := scanRequirement(rows)
		if err != nil {
			rows.Close()
			return nil, nil, err
		}
		reqItems = append(reqItems, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	var ruleReqs []compliance.Requirement
	var tenderFacts []string
	var linkedIDs []int64
	for _, r := range reqItems {
		ruleReqs = append(ruleReqs, compliance.Requirement{
			ID:          r.ID,
			Code:        r.Code,
			Category:    r.Category,
			Kind:        compliance.RequirementKind(r.Category, r.Title, r.Description, r.IsMandatory),
			Title:       r.Title,
			Description: r.Description,
			IsMandatory: r.IsMandatory,
			ScoreWeight: r.ScoreWeight,
		})
		for _, kw := range r.ProofRules.ScopeKeywords {
			if strings.TrimSpace(kw) != "" {
				tenderFacts = append(tenderFacts, strings.TrimSpace(kw))
			}
		}
		linkedIDs = append(linkedIDs, r.LinkedRequirementIDs...)
	}
	linkedIDs = uniqueIDs(linkedIDs)
	if len(linkedIDs) > 0 {
		lrows, err := q.QueryContext(ctx, `SELECT title, description FROM requirement_items WHERE tenant_id = $1 AND id = ANY($2::bigint[])`,
			tenant, pgInt64Array(linkedIDs))
		if err != nil {
			return nil, nil, err
		}
		for lrows.Next() {
			var t, d string
			if err := lrows.Scan(&t, &d); err != nil {
				lrows.Close()
				return nil, nil, err
			}
			ruleReqs = append(ruleReqs, compliance.Requirement{Title: t, Description: d})
		}
		lrows.Close()
	}
	return ruleReqs, tenderFacts, nil
}

func buildVerifyClaimsInputTx(ctx context.Context, q queryer, tenant string, s *ProposalSection, sb *SectionStoryboard, contentMD string) (compliance.VerifyClaimsInput, error) {
	facts, err := loadTaskGroundedFactSourcesTx(ctx, q, tenant, s.TaskID)
	if err != nil {
		return compliance.VerifyClaimsInput{}, err
	}
	kbs, kbChunks, err := loadTaskGroundedKBSourcesTx(ctx, q, tenant, s.TaskID)
	if err != nil {
		return compliance.VerifyClaimsInput{}, err
	}
	commits, err := loadTaskGroundedCommitmentsTx(ctx, q, tenant, s.TaskID)
	if err != nil {
		return compliance.VerifyClaimsInput{}, err
	}
	reqs, tenderFacts, err := loadSectionRuleRequirementsTx(ctx, q, tenant, s.ID)
	if err != nil {
		return compliance.VerifyClaimsInput{}, err
	}
	in := compliance.VerifyClaimsInput{
		SectionID:       s.ID,
		SectionCode:     s.Code,
		SectionTitle:    s.Title,
		ContentMD:       contentMD,
		Requirements:    reqs,
		TenderFacts:     tenderFacts,
		AdmissibleFacts: facts,
		VerifiedKB:      kbs,
		Commitments:     commits,
		KBChunks:        kbChunks,
	}
	if sb != nil && sb.Status == StoryboardPinkApproved {
		in.AllowedAdmissibilityItemIDs = sb.AdmissibilityItemIDs
		in.AllowedEvidenceIDs = sb.EvidenceIDs
		in.AllowedCommitmentIDs = sb.CommitmentIDs
	}
	return in, nil
}

// RenderSectionGroundedDraft 对章节草稿模板中的 [[EVID:...]] / [[KB:...]] / [[COMMIT:...]] 执行确定性渲染。
// 若存在无法解析、未可采、未确认或不在已批准故事板白名单内的 token，返回对应错误哨兵。
func (d *DB) RenderSectionGroundedDraft(ctx context.Context, tenant string, taskID, sectionID int64, templateMD string) (*compliance.RenderGroundedOutput, error) {
	if err := CheckSectionScope(ctx, sectionID); err != nil {
		return nil, err
	}
	tenant = tenantOr(tenant)
	var out *compliance.RenderGroundedOutput
	err := d.readSnapshot(ctx, func(tx *sql.Tx) error {
		s, err := scanSection(tx.QueryRowContext(ctx, `SELECT `+sectionCols+` FROM proposal_sections s WHERE s.tenant_id = $1 AND s.task_id = $2 AND s.id = $3`,
			tenant, taskID, sectionID))
		if err != nil {
			return err
		}
		sb, err := currentStoryboardTx(ctx, tx, tenant, taskID, s.ID, false)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		facts, err := loadTaskGroundedFactSourcesTx(ctx, tx, tenant, taskID)
		if err != nil {
			return err
		}
		kbs, _, err := loadTaskGroundedKBSourcesTx(ctx, tx, tenant, taskID)
		if err != nil {
			return err
		}
		commits, err := loadTaskGroundedCommitmentsTx(ctx, tx, tenant, taskID)
		if err != nil {
			return err
		}
		in := compliance.RenderGroundedInput{
			TemplateMD:      templateMD,
			AdmissibleFacts: facts,
			VerifiedKB:      kbs,
			Commitments:     commits,
		}
		if sb != nil && sb.Status == StoryboardPinkApproved {
			in.AllowedAdmissibilityItemIDs = sb.AdmissibilityItemIDs
			in.AllowedEvidenceIDs = sb.EvidenceIDs
			in.AllowedCommitmentIDs = sb.CommitmentIDs
		}
		res := compliance.RenderGroundedDraft(in)
		out = &res
		if len(res.Unresolved) > 0 {
			u := res.Unresolved[0]
			switch u.Kind {
			case "evid":
				return fmt.Errorf("%w: token %s (%s)", ErrEvidenceInadmissible, u.Token, u.Reason)
			case "commit":
				return fmt.Errorf("%w: token %s (%s)", ErrCommitmentUnconfirmed, u.Token, u.Reason)
			default:
				return fmt.Errorf("%w: token %s (%s)", ErrEvidenceInvalid, u.Token, u.Reason)
			}
		}
		return nil
	})
	return out, err
}

func requirePinkApprovedStoryboardTx(ctx context.Context, q queryer, tenant string, taskID, sectionID int64) (*SectionStoryboard, error) {
	sb, err := currentStoryboardTx(ctx, q, tenant, taskID, sectionID, false)
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("%w: section %d has no storyboard", ErrStoryboardMissing, sectionID)
	}
	if err != nil {
		return nil, err
	}
	switch sb.Status {
	case StoryboardPinkApproved:
		return sb, nil
	case StoryboardStale:
		return nil, fmt.Errorf("%w: storyboard v%d is stale (%s)", ErrStoryboardStale, sb.Version, sb.StaleReason)
	default:
		return nil, fmt.Errorf("%w: storyboard v%d status is %s (must be pink_approved)", ErrStoryboardNotApproved, sb.Version, sb.Status)
	}
}

func d3CheckSectionWriteGateTx(ctx context.Context, tx *sql.Tx, tenant string, taskID, sectionID int64) error {
	active, err := taskHasD3ActivityQ(ctx, tx, tenant, taskID)
	if err != nil || !active {
		return err
	}
	_, err = requirePinkApprovedStoryboardTx(ctx, tx, tenant, taskID, sectionID)
	return err
}

func d3PrepareAndVerifySaveRevisionTx(ctx context.Context, tx *sql.Tx, tenant string, s *ProposalSection, in *SaveRevisionInput) (*SectionStoryboard, *compliance.WritingVerificationResult, error) {
	active, err := taskHasD3ActivityQ(ctx, tx, tenant, s.TaskID)
	if err != nil {
		return nil, nil, err
	}
	hasTokens := contentHasD3Tokens(in.ContentMD)
	if !active && !hasTokens {
		return nil, nil, nil
	}

	var sb *SectionStoryboard
	if active {
		sb, err = requirePinkApprovedStoryboardTx(ctx, tx, tenant, s.TaskID, s.ID)
		if err != nil {
			return nil, nil, err
		}
	} else {
		sb, _ = currentStoryboardTx(ctx, tx, tenant, s.TaskID, s.ID, false)
	}

	facts, err := loadTaskGroundedFactSourcesTx(ctx, tx, tenant, s.TaskID)
	if err != nil {
		return nil, nil, err
	}
	kbs, kbChunks, err := loadTaskGroundedKBSourcesTx(ctx, tx, tenant, s.TaskID)
	if err != nil {
		return nil, nil, err
	}
	commits, err := loadTaskGroundedCommitmentsTx(ctx, tx, tenant, s.TaskID)
	if err != nil {
		return nil, nil, err
	}

	renderIn := compliance.RenderGroundedInput{
		TemplateMD:      in.ContentMD,
		AdmissibleFacts: facts,
		VerifiedKB:      kbs,
		Commitments:     commits,
	}
	if sb != nil && sb.Status == StoryboardPinkApproved {
		renderIn.AllowedAdmissibilityItemIDs = sb.AdmissibilityItemIDs
		renderIn.AllowedEvidenceIDs = sb.EvidenceIDs
		renderIn.AllowedCommitmentIDs = sb.CommitmentIDs
	}
	rendered := compliance.RenderGroundedDraft(renderIn)
	if len(rendered.Unresolved) > 0 {
		u := rendered.Unresolved[0]
		switch u.Kind {
		case "evid":
			return nil, nil, fmt.Errorf("%w: token %s (%s)", ErrEvidenceInadmissible, u.Token, u.Reason)
		case "commit":
			return nil, nil, fmt.Errorf("%w: token %s (%s)", ErrCommitmentUnconfirmed, u.Token, u.Reason)
		default:
			return nil, nil, fmt.Errorf("%w: token %s (%s)", ErrEvidenceInvalid, u.Token, u.Reason)
		}
	}
	in.ContentMD = rendered.RenderedMD
	in.AdmissibilityItemIDs = uniqueIDs(append(in.AdmissibilityItemIDs, rendered.UsedAdmissibilityItemIDs...))
	in.EvidenceIDs = uniqueIDs(append(in.EvidenceIDs, rendered.UsedEvidenceIDs...))
	in.CommitmentIDs = uniqueIDs(append(in.CommitmentIDs, rendered.UsedCommitmentIDs...))

	reqs, tenderFacts, err := loadSectionRuleRequirementsTx(ctx, tx, tenant, s.ID)
	if err != nil {
		return nil, nil, err
	}
	verifyIn := compliance.VerifyClaimsInput{
		SectionID:                   s.ID,
		SectionCode:                 s.Code,
		SectionTitle:                s.Title,
		ContentMD:                   in.ContentMD,
		Requirements:                reqs,
		TenderFacts:                 tenderFacts,
		AdmissibleFacts:             facts,
		VerifiedKB:                  kbs,
		Commitments:                 commits,
		AllowedAdmissibilityItemIDs: renderIn.AllowedAdmissibilityItemIDs,
		AllowedEvidenceIDs:          renderIn.AllowedEvidenceIDs,
		AllowedCommitmentIDs:        renderIn.AllowedCommitmentIDs,
		KBChunks:                    kbChunks,
	}
	res := compliance.VerifySectionClaims(verifyIn)
	if res.Metrics.UngroundedClaimCount > 0 {
		var msgs []string
		for _, r := range res.Reasons {
			if strings.HasPrefix(r.Code, "ungrounded_") || r.Code == "unresolved_citation_token" {
				msgs = append(msgs, r.Message)
			}
		}
		return nil, nil, fmt.Errorf("%w: %d ungrounded claim(s): %s", ErrUngroundedClaim, res.Metrics.UngroundedClaimCount, strings.Join(msgs, "; "))
	}
	if res.Verdict != compliance.WritingVerdictPassed {
		var msgs []string
		for _, r := range res.Reasons {
			msgs = append(msgs, r.Message)
		}
		return nil, nil, fmt.Errorf("%w: %s", ErrWritingVerificationFailed, strings.Join(msgs, "; "))
	}
	return sb, &res, nil
}

func persistWritingVerificationTx(ctx context.Context, q queryer, tenant string, s *ProposalSection, revID int64, revNo int, sb *SectionStoryboard, res compliance.WritingVerificationResult, actor string) (*SectionWritingVerification, error) {
	claimsJSON, _ := json.Marshal(res.Claims)
	metricsJSON, _ := json.Marshal(res.Metrics)
	reasonsJSON, _ := json.Marshal(res.Reasons)
	var sbID any
	if sb != nil {
		sbID = sb.ID
	}
	if strings.TrimSpace(actor) == "" {
		actor = "system"
	}
	if _, err := q.ExecContext(ctx, `INSERT INTO section_writing_verifications(
		tenant_id, task_id, section_id, section_rev_id, rev_no, storyboard_id, rule_version, verdict, stale_reason,
		ungrounded_claim_count, fact_binding_rate, commitment_binding_rate, specificity_ratio,
		kb_similarity_max, boilerplate_similarity_max, claims, metrics, reasons, created_by, updated_by
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'',$9,$10,$11,$12,$13,$14,$15::jsonb,$16::jsonb,$17::jsonb,$18,$18)
	ON CONFLICT (tenant_id, section_rev_id) DO UPDATE SET
		storyboard_id = EXCLUDED.storyboard_id,
		rule_version = EXCLUDED.rule_version,
		verdict = EXCLUDED.verdict,
		stale_reason = '',
		ungrounded_claim_count = EXCLUDED.ungrounded_claim_count,
		fact_binding_rate = EXCLUDED.fact_binding_rate,
		commitment_binding_rate = EXCLUDED.commitment_binding_rate,
		specificity_ratio = EXCLUDED.specificity_ratio,
		kb_similarity_max = EXCLUDED.kb_similarity_max,
		boilerplate_similarity_max = EXCLUDED.boilerplate_similarity_max,
		claims = EXCLUDED.claims,
		metrics = EXCLUDED.metrics,
		reasons = EXCLUDED.reasons,
		updated_by = EXCLUDED.updated_by,
		updated_at = NOW()`,
		tenant, s.TaskID, s.ID, revID, revNo, sbID, res.RuleVersion, res.Verdict,
		res.Metrics.UngroundedClaimCount, res.Metrics.FactBindingRate, res.Metrics.CommitmentBindingRate,
		res.Metrics.SpecificityRatio, res.Metrics.KBSimilarityMax, res.Metrics.BoilerplateSimilarityMax,
		string(claimsJSON), string(metricsJSON), string(reasonsJSON), actor); err != nil {
		return nil, err
	}
	return scanWritingVerification(q.QueryRowContext(ctx, `SELECT `+writingVerifCols+writingVerifJoin+
		`WHERE wv.tenant_id = $1 AND wv.section_rev_id = $2`, tenant, revID))
}

func d3CheckApproveSectionRevisionTx(ctx context.Context, tx *sql.Tx, tenant string, s *ProposalSection, rev *SectionRevision, actor string) error {
	active, err := taskHasD3ActivityQ(ctx, tx, tenant, s.TaskID)
	if err != nil || !active {
		return err
	}
	actor = strings.TrimSpace(actor)
	if actor == "" || actor == rev.CreatedBy {
		return fmt.Errorf("%w: red team reviewer %q cannot approve own section revision (created_by=%q)",
			ErrRedSelfApproval, actor, rev.CreatedBy)
	}
	sb, err := requirePinkApprovedStoryboardTx(ctx, tx, tenant, s.TaskID, s.ID)
	if err != nil {
		return err
	}
	verifyIn, err := buildVerifyClaimsInputTx(ctx, tx, tenant, s, sb, rev.ContentMD)
	if err != nil {
		return err
	}
	res := compliance.VerifySectionClaims(verifyIn)
	wv, err := persistWritingVerificationTx(ctx, tx, tenant, s, rev.ID, rev.RevNo, sb, res, actor)
	if err != nil {
		return err
	}
	if wv.UngroundedClaimCount > 0 {
		return fmt.Errorf("%w: %d ungrounded claim(s)", ErrUngroundedClaim, wv.UngroundedClaimCount)
	}
	if wv.Verdict != compliance.WritingVerdictPassed {
		return fmt.Errorf("%w: writing verification verdict is %s", ErrWritingVerificationFailed, wv.Verdict)
	}
	_, err = tx.ExecContext(ctx, `UPDATE section_writing_verifications SET approved_by = $2, updated_by = $2, updated_at = NOW() WHERE id = $1`,
		wv.ID, actor)
	return err
}

// EvaluateSectionWritingVerification 对章节当前主版本执行写作核验并持久化结果（不论是否通过均保存记录并返回）。
func (d *DB) EvaluateSectionWritingVerification(ctx context.Context, tenant string, taskID, sectionID int64, actor string) (*SectionWritingVerification, error) {
	tenant = tenantOr(tenant)
	var out *SectionWritingVerification
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		s, err := scanSection(tx.QueryRowContext(ctx, `SELECT `+sectionCols+` FROM proposal_sections s WHERE s.tenant_id = $1 AND s.task_id = $2 AND s.id = $3 FOR SHARE`,
			tenant, taskID, sectionID))
		if err != nil {
			return err
		}
		if s.CurrentRevNo <= 0 {
			return fmt.Errorf("%w: section %s has no current revision", ErrNotFound, s.Code)
		}
		rev, err := scanRevision(tx.QueryRowContext(ctx, `SELECT `+revisionCols+` FROM section_revisions WHERE tenant_id = $1 AND section_id = $2 AND rev_no = $3`,
			tenant, s.ID, s.CurrentRevNo))
		if err != nil {
			return err
		}
		sb, err := currentStoryboardTx(ctx, tx, tenant, taskID, s.ID, false)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		verifyIn, err := buildVerifyClaimsInputTx(ctx, tx, tenant, s, sb, rev.ContentMD)
		if err != nil {
			return err
		}
		res := compliance.VerifySectionClaims(verifyIn)
		out, err = persistWritingVerificationTx(ctx, tx, tenant, s, rev.ID, rev.RevNo, sb, res, actor)
		return err
	})
	return out, err
}

// GetSectionWritingVerification 返回章节当前主版本的写作核验记录；不存在时返回 ErrNotFound。
func (d *DB) GetSectionWritingVerification(ctx context.Context, tenant string, taskID, sectionID int64) (*SectionWritingVerification, error) {
	tenant = tenantOr(tenant)
	return scanWritingVerification(d.QueryRowContext(ctx, `SELECT `+writingVerifCols+writingVerifJoin+
		`WHERE wv.tenant_id = $1 AND wv.task_id = $2 AND wv.section_id = $3
		   AND wv.rev_no = (SELECT current_rev_no FROM proposal_sections WHERE tenant_id = $1 AND id = $3)`,
		tenant, taskID, sectionID))
}

// ListTaskWritingVerifications 返回任务下各章节当前主版本的写作核验记录（按章节顺序排列）。
func (d *DB) ListTaskWritingVerifications(ctx context.Context, tenant string, taskID int64) ([]*SectionWritingVerification, error) {
	tenant = tenantOr(tenant)
	rows, err := d.QueryContext(ctx, `SELECT `+writingVerifCols+writingVerifJoin+
		`WHERE wv.tenant_id = $1 AND wv.task_id = $2
		   AND s.current_rev_no = wv.rev_no
		 ORDER BY COALESCE(s.seq, 0), wv.section_id`, tenant, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*SectionWritingVerification{}
	for rows.Next() {
		wv, err := scanWritingVerification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, wv)
	}
	return out, rows.Err()
}
