package db

// Phase 5（域 C）：版本化技术标大纲的持久化审批状态机、带依赖门禁的章节写作租约，以及乐观锁多版本章节修订。
//
// 不变量（均在 PostgreSQL 事务内校验，不依赖 intercept 或 ExplorationStore.Frontier）：
//  1. 写作派发（ClaimSectionWrite）只在任务存在 approved 大纲、章节属于该版本且全部前置依赖已有有效稿时放行。
//  2. 写回（SaveSectionRevision）在同一事务内锁定批准大纲（FOR SHARE）与章节（FOR UPDATE）：
//     based_on_outline_version 不是当前批准版本 -> 归档为 stale_outline 候选稿；base_rev_no != current_rev_no ->
//     归档为 conflict_candidate；二者都不会成为当前主版本。只有两者都一致时才分配新修订并推进 current_rev_no。
//  3. 章节批准绑定具体 rev_no；任何新的有效修订（AI 重写 / 人工编辑 / 回滚）都会清空 approved_rev_no。
//  4. 回滚生成新的 rollback 修订，修订记录本身不可篡改（触发器保证）。
//  5. 引用的 retrieval_evidence 变为 stale 时（触发器）修订与章节降级为 stale_evidence；stale / contradicts 证据不能批准。
// 锁顺序统一为「大纲行 -> 章节行」，避免与要求变更（锁大纲后经触发器更新章节）死锁。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// 大纲状态。
const (
	OutlineDraft      = "draft"
	OutlinePending    = "pending_approval"
	OutlineApproved   = "approved"
	OutlineSuperseded = "superseded"
)

// 章节状态（proposal_sections.status）。
const (
	SectionPending         = "pending"
	SectionReady           = "ready"
	SectionWriting         = "writing"
	SectionDraft           = "draft"
	SectionReviewing       = "reviewing"
	SectionApproved        = "approved"
	SectionRejected        = "rejected"
	SectionStaleOutline    = "stale_outline"
	SectionStaleEvidence   = "stale_evidence"
	SectionStaleCommitment = "stale_commitment"
)

// 修订来源与状态（section_revisions）。
const (
	RevAIInitial       = "ai_initial"
	RevAIRewrite       = "ai_rewrite"
	RevHumanEdit       = "human_edit"
	RevRollback        = "rollback"
	RevDraft           = "draft"
	RevReviewing       = "reviewing"
	RevApproved        = "approved"
	RevRejected        = "rejected"
	RevStaleOut        = "stale_outline"
	RevStaleEvid       = "stale_evidence"
	RevStaleCommitment = "stale_commitment"
	RevConflict        = "conflict_candidate"
	SaveApplied        = "applied"
	SaveStale          = "stale_outline"
	SaveConflict       = "conflict_candidate"
	SectionLeaseTTL    = 45 * time.Second
)

var (
	ErrProposalConflict       = errors.New("proposal: conflicting state")
	ErrInvalidOutline         = errors.New("proposal: invalid outline")
	ErrOutlineState           = errors.New("proposal: outline status does not allow this operation")
	ErrOutlineIncomplete      = errors.New("proposal: outline leaves requirements unmapped")
	ErrOutlineNotApproved     = errors.New("proposal: outline not approved")
	ErrOutlineStale           = errors.New("proposal: section belongs to a superseded outline version")
	ErrSectionDependencyUnmet = errors.New("proposal: section dependencies have no valid draft yet")
	ErrSectionBusy            = errors.New("proposal: section is already being written")
	ErrRevisionConflict       = errors.New("proposal: base_rev_no does not match the current revision")
	ErrInvalidRevision        = errors.New("proposal: invalid revision")
	ErrEvidenceInvalid        = errors.New("proposal: evidence invalid")
)

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func isAIRevision(source string) bool { return source == RevAIInitial || source == RevAIRewrite }

// readSnapshot 在 REPEATABLE READ 事务中执行多语句读取，避免章节、修订与租约之间的撕裂读
// （例如看到了新写回的候选稿却仍看到写回前的租约）。
func (d *DB) readSnapshot(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := d.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// 类型
// ---------------------------------------------------------------------------

type ProposalOutline struct {
	ID                 int64      `json:"id"`
	TenantID           string     `json:"tenant_id"`
	TaskID             int64      `json:"task_id"`
	Version            int        `json:"version"`
	RequirementsEpoch  int64      `json:"requirements_epoch"`
	Status             string     `json:"status"`
	DerivedFromVersion *int       `json:"derived_from_version,omitempty"`
	Note               string     `json:"note"`
	SubmittedBy        string     `json:"submitted_by"`
	SubmittedAt        *time.Time `json:"submitted_at,omitempty"`
	ApprovedBy         string     `json:"approved_by"`
	ApprovedAt         *time.Time `json:"approved_at,omitempty"`
	SupersededAt       *time.Time `json:"superseded_at,omitempty"`
	CreatedBy          string     `json:"created_by"`
	UpdatedBy          string     `json:"updated_by"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

type ProposalSection struct {
	ID                   int64      `json:"id"`
	TenantID             string     `json:"tenant_id"`
	TaskID               int64      `json:"task_id"`
	OutlineID            int64      `json:"outline_id"`
	OutlineVersion       int        `json:"outline_version"`
	Code                 string     `json:"section_code"`
	Title                string     `json:"title"`
	Level                int        `json:"level"`
	Seq                  int        `json:"seq"`
	WritingGuide         string     `json:"writing_guide"`
	TargetWords          int        `json:"target_words"`
	DependsOn            []int64    `json:"depends_on_section_ids"`
	DerivedFromSectionID *int64     `json:"derived_from_section_id,omitempty"`
	CurrentRevNo         int        `json:"current_rev_no"`
	ApprovedRevNo        *int       `json:"approved_rev_no,omitempty"`
	Status               string     `json:"status"`
	LeaseToken           int64      `json:"lease_token"`
	LeaseUntil           *time.Time `json:"lease_until,omitempty"`
	CreatedBy            string     `json:"created_by"`
	UpdatedBy            string     `json:"updated_by"`
	ApprovedBy           string     `json:"approved_by"`
	UpdatedAt            time.Time  `json:"updated_at"`
	// 计算字段
	RequirementIDs []int64  `json:"requirement_item_ids"`
	DependsOnCodes []string `json:"depends_on"`
	UnmetDeps      []string `json:"unmet_dependencies"`
	DepsReady      bool     `json:"dependencies_ready"`
	Writing        bool     `json:"writing"`
}

// RevisionGap 是修订中显式记录的证据 / 要求缺口；未核验、证据不足、冲突或失效证据由系统自动追加。
type RevisionGap struct {
	EvidenceID    int64  `json:"evidence_id,omitempty"`
	RequirementID int64  `json:"requirement_id,omitempty"`
	Kind          string `json:"kind"`
	Note          string `json:"note"`
}

type SectionRevision struct {
	ID                    int64         `json:"id"`
	SectionID             int64         `json:"section_id"`
	RevNo                 int           `json:"rev_no"`
	BaseRevNo             int           `json:"base_rev_no"`
	BasedOnOutlineVersion int           `json:"based_on_outline_version"`
	Source                string        `json:"source"`
	RollbackOfRevNo       *int          `json:"rollback_of_rev_no,omitempty"`
	ContentMD             string        `json:"content_md"`
	SummaryNote           string        `json:"summary_note"`
	DiffSummary           string        `json:"diff_summary"`
	EvidenceIDs           []int64       `json:"evidence_ids"`
	CommitmentIDs         []int64       `json:"commitment_ids"`
	Gaps                  []RevisionGap `json:"gaps"`
	Status                string        `json:"status"`
	CreatedBy             string        `json:"created_by"`
	ApprovedBy            string        `json:"approved_by"`
	ApprovedAt            *time.Time    `json:"approved_at,omitempty"`
	CreatedAt             time.Time     `json:"created_at"`
	// ActorLegacy 为 true 表示作者是升级前的声明式姓名（R2），不能参与红队职责分离判定。
	ActorLegacy bool `json:"actor_legacy,omitempty"`
}

// EvidenceState 是证据的实时有效性；Usable 仅当 status=verified 且 relation=supports。
type EvidenceState struct {
	ID       int64  `json:"id"`
	RunID    int64  `json:"run_id"`
	Domain   string `json:"source_domain"`
	SourceID int64  `json:"source_id"`
	DocID    int64  `json:"doc_id"`
	Relation string `json:"relation"`
	Status   string `json:"status"`
	Reason   string `json:"reason"`
	Usable   bool   `json:"usable"`
	Missing  bool   `json:"missing,omitempty"`
}

// ---------------------------------------------------------------------------
// 扫描与查询辅助
// ---------------------------------------------------------------------------

const outlineCols = `id, tenant_id, task_id, version, requirements_epoch, status, derived_from_version, note, submitted_by, submitted_at,
	approved_by, approved_at, superseded_at, created_by, updated_by, created_at, updated_at`

func scanOutline(r rowScanner) (*ProposalOutline, error) {
	var o ProposalOutline
	var derived sql.NullInt32
	var sub, appr, sup sql.NullTime
	err := r.Scan(&o.ID, &o.TenantID, &o.TaskID, &o.Version, &o.RequirementsEpoch, &o.Status, &derived, &o.Note, &o.SubmittedBy, &sub,
		&o.ApprovedBy, &appr, &sup, &o.CreatedBy, &o.UpdatedBy, &o.CreatedAt, &o.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if derived.Valid {
		v := int(derived.Int32)
		o.DerivedFromVersion = &v
	}
	o.SubmittedAt, o.ApprovedAt, o.SupersededAt = nullTimePtr(sub), nullTimePtr(appr), nullTimePtr(sup)
	return &o, nil
}

const sectionCols = `s.id, s.tenant_id, s.task_id, s.outline_id, s.outline_version, s.section_code, s.title, s.level, s.seq, s.writing_guide,
	s.target_words, to_jsonb(s.depends_on_section_ids), s.derived_from_section_id, s.current_rev_no, s.approved_rev_no, s.status,
	s.lease_token, s.lease_until, s.created_by, s.updated_by, s.approved_by, s.updated_at`

func scanSection(r rowScanner) (*ProposalSection, error) {
	var s ProposalSection
	var deps []byte
	var approved sql.NullInt32
	var lease sql.NullTime
	err := r.Scan(&s.ID, &s.TenantID, &s.TaskID, &s.OutlineID, &s.OutlineVersion, &s.Code, &s.Title, &s.Level, &s.Seq, &s.WritingGuide,
		&s.TargetWords, &deps, &s.DerivedFromSectionID, &s.CurrentRevNo, &approved, &s.Status, &s.LeaseToken, &lease,
		&s.CreatedBy, &s.UpdatedBy, &s.ApprovedBy, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	s.DependsOn = int64sJSON(deps)
	if approved.Valid {
		v := int(approved.Int32)
		s.ApprovedRevNo = &v
	}
	s.LeaseUntil = nullTimePtr(lease)
	s.Writing = s.LeaseUntil != nil && s.LeaseUntil.After(time.Now())
	s.RequirementIDs, s.DependsOnCodes, s.UnmetDeps = []int64{}, []string{}, []string{}
	return &s, nil
}

const revisionCols = `id, section_id, rev_no, base_rev_no, based_on_outline_version, source, rollback_of_rev_no, content_md, summary_note,
	diff_summary, to_jsonb(evidence_ids), to_jsonb(commitment_ids), gaps, status, created_by, approved_by, approved_at, created_at, actor_legacy`

func scanRevision(r rowScanner) (*SectionRevision, error) {
	var x SectionRevision
	var rb sql.NullInt32
	var ev, commits, gaps []byte
	var at sql.NullTime
	err := r.Scan(&x.ID, &x.SectionID, &x.RevNo, &x.BaseRevNo, &x.BasedOnOutlineVersion, &x.Source, &rb, &x.ContentMD, &x.SummaryNote,
		&x.DiffSummary, &ev, &commits, &gaps, &x.Status, &x.CreatedBy, &x.ApprovedBy, &at, &x.CreatedAt, &x.ActorLegacy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if rb.Valid {
		v := int(rb.Int32)
		x.RollbackOfRevNo = &v
	}
	x.EvidenceIDs = int64sJSON(ev)
	x.CommitmentIDs = int64sJSON(commits)
	x.Gaps = []RevisionGap{}
	_ = json.Unmarshal(gaps, &x.Gaps)
	x.ApprovedAt = nullTimePtr(at)
	return &x, nil
}

// evidenceStatesTx 读取证据实时状态；仅返回同租户、且来自本任务或租户级（task_id 为空）检索运行的证据。
func evidenceStatesTx(ctx context.Context, q queryer, tenant string, taskID int64, ids []int64) (map[int64]EvidenceState, error) {
	out := map[int64]EvidenceState{}
	ids = uniqueIDs(ids)
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT e.id, e.run_id, e.source_domain, e.source_id, e.doc_id, e.relation, e.status, e.reason
		FROM retrieval_evidence e JOIN retrieval_runs r ON r.tenant_id = e.tenant_id AND r.id = e.run_id
		WHERE e.tenant_id = $1 AND e.id = ANY($2::bigint[]) AND (r.task_id IS NULL OR r.task_id = $3)`, tenant, pgInt64Array(ids), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e EvidenceState
		if err := rows.Scan(&e.ID, &e.RunID, &e.Domain, &e.SourceID, &e.DocID, &e.Relation, &e.Status, &e.Reason); err != nil {
			return nil, err
		}
		e.Usable = e.Status == "verified" && e.Relation == "supports"
		out[e.ID] = e
	}
	return out, rows.Err()
}

// EvidenceStates 返回证据实时状态列表（缺失的证据以 Missing 标出，视同失效）。
func (d *DB) EvidenceStates(ctx context.Context, tenant string, taskID int64, ids []int64) ([]EvidenceState, error) {
	return evidenceStateListTx(ctx, d, tenantOr(tenant), taskID, ids)
}

func evidenceStateListTx(ctx context.Context, q queryer, tenant string, taskID int64, ids []int64) ([]EvidenceState, error) {
	m, err := evidenceStatesTx(ctx, q, tenant, taskID, ids)
	if err != nil {
		return nil, err
	}
	out := []EvidenceState{}
	for _, id := range uniqueIDs(ids) {
		e, ok := m[id]
		if !ok {
			e = EvidenceState{ID: id, Status: "stale", Reason: "evidence missing or not visible to this task", Missing: true}
		}
		out = append(out, e)
	}
	return out, nil
}

// EvidenceSnapshots 返回本任务可见证据的原文快照（写作上下文摘录用）。
func (d *DB) EvidenceSnapshots(ctx context.Context, tenant string, taskID int64, ids []int64) (map[int64]json.RawMessage, error) {
	out := map[int64]json.RawMessage{}
	ids = uniqueIDs(ids)
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := d.QueryContext(ctx, `SELECT e.id, e.snapshot FROM retrieval_evidence e
		JOIN retrieval_runs r ON r.tenant_id = e.tenant_id AND r.id = e.run_id
		WHERE e.tenant_id = $1 AND e.id = ANY($2::bigint[]) AND (r.task_id IS NULL OR r.task_id = $3)`, tenantOr(tenant), pgInt64Array(ids), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		out[id] = raw
	}
	return out, rows.Err()
}

// evidenceGaps 为不可作为已证实依据的证据生成缺口记录。
func evidenceGaps(states map[int64]EvidenceState, ids []int64) (gaps []RevisionGap, anyStale bool) {
	for _, id := range ids {
		e, ok := states[id]
		switch {
		case !ok:
			anyStale = true
			gaps = append(gaps, RevisionGap{EvidenceID: id, Kind: "stale", Note: "evidence missing or not visible to this task"})
		case e.Status == "stale":
			anyStale = true
			gaps = append(gaps, RevisionGap{EvidenceID: id, Kind: "stale", Note: e.Reason})
		case e.Relation == "contradicts":
			gaps = append(gaps, RevisionGap{EvidenceID: id, Kind: "contradicts", Note: e.Reason})
		case e.Status != "verified":
			gaps = append(gaps, RevisionGap{EvidenceID: id, Kind: "unverified", Note: "evidence not verified; must not be stated as a confirmed commitment"})
		case e.Relation != "supports":
			gaps = append(gaps, RevisionGap{EvidenceID: id, Kind: e.Relation, Note: e.Reason})
		}
	}
	return gaps, anyStale
}

func mergeGaps(user, auto []RevisionGap) []RevisionGap {
	out := []RevisionGap{}
	seen := map[string]bool{}
	for _, g := range append(append([]RevisionGap{}, user...), auto...) {
		g.Kind, g.Note = strings.TrimSpace(g.Kind), strings.TrimSpace(g.Note)
		if g.Kind == "" {
			g.Kind = "gap"
		}
		key := fmt.Sprintf("%d|%d|%s", g.EvidenceID, g.RequirementID, g.Kind)
		if g.EvidenceID == 0 && g.RequirementID == 0 {
			key += "|" + g.Note
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, g)
		}
	}
	return out
}

// lockActiveOutlineTx 锁定任务最新的 approved / pending_approval 大纲（要求变更前调用，变更后据此派生新草案）。
func lockActiveOutlineTx(ctx context.Context, tx *sql.Tx, tenant string, taskID int64) (*ProposalOutline, error) {
	o, err := scanOutline(tx.QueryRowContext(ctx, `SELECT `+outlineCols+` FROM proposal_outlines
		WHERE tenant_id = $1 AND task_id = $2 AND status IN ('approved','pending_approval')
		ORDER BY version DESC LIMIT 1 FOR UPDATE`, tenant, taskID))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return o, err
}

// afterRequirementChangeTx：要求清单实质变更已由触发器使 before 失效时，派生 version+1 草案（复制章节结构与仍存在的要求映射）。
// 已存在在编草案时不派生（直接编辑该草案即可）。
func afterRequirementChangeTx(ctx context.Context, tx *sql.Tx, tenant string, taskID int64, actor string, before *ProposalOutline) error {
	if before == nil {
		// F1：原文变更可能在提取开始前就把批准大纲置为 superseded（source_epoch 推进），
		// 此时 lockActiveOutlineTx 返回 nil；仍应按最近被取代的版本派生新的在编草案。
		latest, err := scanOutline(tx.QueryRowContext(ctx, `SELECT `+outlineCols+` FROM proposal_outlines
			WHERE tenant_id = $1 AND task_id = $2 AND status = 'superseded'
			ORDER BY version DESC LIMIT 1 FOR UPDATE`, tenant, taskID))
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		before = latest
	}
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM proposal_outlines WHERE id = $1`, before.ID).Scan(&status); err != nil {
		return err
	}
	if status != OutlineSuperseded {
		return nil
	}
	var open bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM proposal_outlines WHERE tenant_id = $1 AND task_id = $2
		AND status IN ('draft','pending_approval'))`, tenant, taskID).Scan(&open); err != nil || open {
		return err
	}
	_, err := deriveOutlineTx(ctx, tx, tenant, taskID, before, nil, "要求清单变更后自动派生", actor)
	return err
}

func outlineByVersionTx(ctx context.Context, q queryer, tenant string, taskID int64, version int, lock bool) (*ProposalOutline, error) {
	query := `SELECT ` + outlineCols + ` FROM proposal_outlines WHERE tenant_id = $1 AND task_id = $2 AND version = $3`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanOutline(q.QueryRowContext(ctx, query, tenant, taskID, version))
}

// approvedOutlineTx 返回任务当前批准大纲（FOR SHARE 阻止并发失效直到事务结束）；不存在时返回 nil。
func approvedOutlineTx(ctx context.Context, tx *sql.Tx, tenant string, taskID int64) (*ProposalOutline, error) {
	o, err := scanOutline(tx.QueryRowContext(ctx, `SELECT `+outlineCols+` FROM proposal_outlines
		WHERE tenant_id = $1 AND task_id = $2 AND status = 'approved' FOR SHARE`, tenant, taskID))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return o, err
}

func sectionsOfOutlineTx(ctx context.Context, q queryer, tenant string, outlineID int64) ([]*ProposalSection, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+sectionCols+` FROM proposal_sections s WHERE s.tenant_id = $1 AND s.outline_id = $2 ORDER BY s.seq`,
		tenant, outlineID)
	if err != nil {
		return nil, err
	}
	var out []*ProposalSection
	byID := map[int64]*ProposalSection{}
	for rows.Next() {
		s, err := scanSection(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, s)
		byID[s.ID] = s
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	mrows, err := q.QueryContext(ctx, `SELECT m.section_id, m.requirement_id FROM proposal_section_requirements m
		JOIN proposal_sections s ON s.tenant_id = m.tenant_id AND s.id = m.section_id
		WHERE m.tenant_id = $1 AND s.outline_id = $2 ORDER BY m.requirement_id`, tenant, outlineID)
	if err != nil {
		return nil, err
	}
	defer mrows.Close()
	for mrows.Next() {
		var sid, rid int64
		if err := mrows.Scan(&sid, &rid); err != nil {
			return nil, err
		}
		if s := byID[sid]; s != nil {
			s.RequirementIDs = append(s.RequirementIDs, rid)
		}
	}
	for _, s := range out {
		annotateDeps(s, byID)
	}
	return out, mrows.Err()
}

// annotateDeps 计算依赖章节编码与就绪状态（依赖章节 current_rev_no > 0 即具备有效初稿 / 定稿）。
func annotateDeps(s *ProposalSection, byID map[int64]*ProposalSection) {
	s.DependsOnCodes, s.UnmetDeps = []string{}, []string{}
	for _, id := range s.DependsOn {
		dep := byID[id]
		if dep == nil {
			continue
		}
		s.DependsOnCodes = append(s.DependsOnCodes, dep.Code)
		if dep.CurrentRevNo == 0 {
			s.UnmetDeps = append(s.UnmetDeps, dep.Code)
		}
	}
	s.DepsReady = len(s.UnmetDeps) == 0
}

// unmetDepsTx 返回章节尚无有效稿的前置依赖编码。
func unmetDepsTx(ctx context.Context, q queryer, tenant string, s *ProposalSection) ([]string, error) {
	if len(s.DependsOn) == 0 {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT section_code FROM proposal_sections
		WHERE tenant_id = $1 AND outline_id = $2 AND id = ANY($3::bigint[]) AND current_rev_no = 0 ORDER BY seq`,
		tenant, s.OutlineID, pgInt64Array(s.DependsOn))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// refreshReadinessTx 重新计算尚无正文章节的 pending / ready（含租约已过期的 writing）。
func refreshReadinessTx(ctx context.Context, q queryer, tenant string, outlineID int64) error {
	_, err := q.ExecContext(ctx, `UPDATE proposal_sections s SET status = CASE WHEN NOT EXISTS (
			SELECT 1 FROM proposal_sections p WHERE p.id = ANY(s.depends_on_section_ids) AND p.current_rev_no = 0)
			THEN 'ready' ELSE 'pending' END
		WHERE s.tenant_id = $1 AND s.outline_id = $2 AND s.current_rev_no = 0
		  AND (s.status IN ('pending','ready') OR (s.status = 'writing' AND (s.lease_until IS NULL OR s.lease_until <= now())))`, tenant, outlineID)
	return err
}

// ---------------------------------------------------------------------------
// 大纲：创建 / 编辑 / 版本派生 / 提交 / 批准
// ---------------------------------------------------------------------------

// SectionInput 描述大纲中的一个章节；数组顺序即 seq，depends_on 使用同一大纲内的章节编码。
type SectionInput struct {
	Code           string   `json:"section_code"`
	Title          string   `json:"title"`
	Level          int      `json:"level"`
	WritingGuide   string   `json:"writing_guide"`
	TargetWords    int      `json:"target_words"`
	RequirementIDs []int64  `json:"requirement_item_ids"`
	DependsOn      []string `json:"depends_on"`
}

// OutlineInput：Sections 为空时，创建接口从 FromVersion 复制章节，FromVersion 也为 0 时按要求清单生成默认目录。
type OutlineInput struct {
	Note        string         `json:"note"`
	FromVersion int            `json:"from_version"`
	Sections    []SectionInput `json:"sections"`
}

// OutlineCoverage 统计技术标范围内要求的映射情况；OutOfScope 为由报价文件响应的价格评分项（不要求映射）。
type OutlineCoverage struct {
	Total      int              `json:"total"`
	Mapped     int              `json:"mapped"`
	Unmapped   []RequirementRef `json:"unmapped"`
	OutOfScope int              `json:"out_of_scope"`
}

type RequirementRef struct {
	ID          int64  `json:"id"`
	Code        string `json:"code"`
	Title       string `json:"title"`
	Category    string `json:"category"`
	IsMandatory bool   `json:"is_mandatory"`
}

type OutlineDetail struct {
	Outline         *ProposalOutline     `json:"outline"`
	Sections        []*ProposalSection   `json:"sections"`
	Coverage        OutlineCoverage      `json:"coverage"`
	Manifest        *RequirementManifest `json:"manifest"`
	ApprovedVersion int                  `json:"approved_version"`
	EpochCurrent    bool                 `json:"requirements_epoch_current"`
}

type OutlineList struct {
	Outlines        []*ProposalOutline   `json:"outlines"`
	ApprovedVersion int                  `json:"approved_version"`
	OpenVersion     int                  `json:"open_version"`
	Manifest        *RequirementManifest `json:"manifest"`
}

func (d *DB) ListOutlines(ctx context.Context, tenant string, taskID int64) (*OutlineList, error) {
	tenant = tenantOr(tenant)
	m, err := d.RequirementManifest(ctx, tenant, taskID)
	if err != nil {
		return nil, err
	}
	rows, err := d.QueryContext(ctx, `SELECT `+outlineCols+` FROM proposal_outlines WHERE tenant_id = $1 AND task_id = $2 ORDER BY version DESC`, tenant, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &OutlineList{Outlines: []*ProposalOutline{}, Manifest: m}
	for rows.Next() {
		o, err := scanOutline(rows)
		if err != nil {
			return nil, err
		}
		switch o.Status {
		case OutlineApproved:
			out.ApprovedVersion = o.Version
		case OutlineDraft, OutlinePending:
			out.OpenVersion = o.Version
		}
		out.Outlines = append(out.Outlines, o)
	}
	return out, rows.Err()
}

func outlineCoverageTx(ctx context.Context, q queryer, tenant string, taskID, outlineID int64) (OutlineCoverage, error) {
	cov := OutlineCoverage{Unmapped: []RequirementRef{}}
	rows, err := q.QueryContext(ctx, `SELECT r.id, r.code, r.title, r.category, r.is_mandatory, r.score_group, r.out_of_bid,
		(r.superseded_by_id IS NOT NULL OR r.review_status = 'stale' OR r.parent_id IS NOT NULL),
		EXISTS (SELECT 1 FROM proposal_section_requirements m JOIN proposal_sections s ON s.tenant_id = m.tenant_id AND s.id = m.section_id
		        WHERE m.tenant_id = r.tenant_id AND m.requirement_id = r.id AND s.outline_id = $3)
		FROM requirement_items r WHERE r.tenant_id = $1 AND r.task_id = $2 ORDER BY r.code`, tenant, taskID, outlineID)
	if err != nil {
		return cov, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref RequirementRef
		var group string
		var outBid, inactive, mapped bool
		if err := rows.Scan(&ref.ID, &ref.Code, &ref.Title, &ref.Category, &ref.IsMandatory, &group, &outBid, &inactive, &mapped); err != nil {
			return cov, err
		}
		if inactive {
			if !mapped {
				cov.OutOfScope++
			}
			continue
		}
		if (ref.Category == ReqVetoItem || outBid) && !mapped {
			continue // 否决项由投标方逐项核对，不投的分包不作答：不计入大纲覆盖
		}
		if !inTechnicalScope(ref.Category, group) && !mapped {
			cov.OutOfScope++
			continue
		}
		cov.Total++
		if mapped {
			cov.Mapped++
		} else {
			cov.Unmapped = append(cov.Unmapped, ref)
		}
	}
	return cov, rows.Err()
}

func outlineDetailTx(ctx context.Context, q queryer, tenant string, o *ProposalOutline) (*OutlineDetail, error) {
	secs, err := sectionsOfOutlineTx(ctx, q, tenant, o.ID)
	if err != nil {
		return nil, err
	}
	if secs == nil {
		secs = []*ProposalSection{}
	}
	cov, err := outlineCoverageTx(ctx, q, tenant, o.TaskID, o.ID)
	if err != nil {
		return nil, err
	}
	m, err := ensureManifestTx(ctx, q, tenant, o.TaskID, false)
	if err != nil {
		return nil, err
	}
	det := &OutlineDetail{Outline: o, Sections: secs, Coverage: cov, Manifest: m, EpochCurrent: o.RequirementsEpoch == m.Epoch}
	err = q.QueryRowContext(ctx, `SELECT COALESCE((SELECT version FROM proposal_outlines WHERE tenant_id = $1 AND task_id = $2 AND status = 'approved'), 0)`,
		tenant, o.TaskID).Scan(&det.ApprovedVersion)
	return det, err
}

// GetOutline 返回指定版本大纲、章节、依赖就绪状态与要求覆盖率。
func (d *DB) GetOutline(ctx context.Context, tenant string, taskID int64, version int) (*OutlineDetail, error) {
	tenant = tenantOr(tenant)
	var det *OutlineDetail
	err := d.readSnapshot(ctx, func(tx *sql.Tx) error {
		o, err := outlineByVersionTx(ctx, tx, tenant, taskID, version, false)
		if err != nil {
			return err
		}
		det, err = outlineDetailTx(ctx, tx, tenant, o)
		return err
	})
	return det, err
}

// CreateOutline 创建新版本草案大纲。任务已有在编（draft / pending_approval）大纲时返回 ErrProposalConflict。
// 已批准大纲在新版本获批前继续有效。
func (d *DB) CreateOutline(ctx context.Context, tenant string, taskID int64, actor string, in OutlineInput) (*OutlineDetail, error) {
	tenant = tenantOr(tenant)
	var out *OutlineDetail
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := ensureManifestTx(ctx, tx, tenant, taskID, true); err != nil {
			return err
		}
		var from *ProposalOutline
		if in.FromVersion > 0 {
			var err error
			if from, err = outlineByVersionTx(ctx, tx, tenant, taskID, in.FromVersion, false); err != nil {
				return err
			}
		}
		sections := in.Sections
		if len(sections) == 0 && from == nil {
			reqs, err := listRequirementsTx(ctx, tx, tenant, taskID)
			if err != nil {
				return err
			}
			if len(reqs) == 0 {
				return fmt.Errorf("%w: requirement list is empty; extract or add requirements first", ErrInvalidOutline)
			}
			sections = DefaultOutlineSections(reqs)
		}
		o, err := deriveOutlineTx(ctx, tx, tenant, taskID, from, sections, in.Note, actor)
		if err != nil {
			return err
		}
		out, err = outlineDetailTx(ctx, tx, tenant, o)
		return err
	})
	return out, err
}

// UpdateOutline 调整章节目录、依赖与要求映射：draft 原地替换；pending_approval 退回 draft 后替换；
// approved 自动派生 version+1 草案并把旧版置为 superseded；superseded 派生新草案。sections 为空时仅更新说明 / 复制结构。
func (d *DB) UpdateOutline(ctx context.Context, tenant string, taskID int64, version int, actor string, in OutlineInput) (*OutlineDetail, error) {
	tenant = tenantOr(tenant)
	var out *OutlineDetail
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		o, err := outlineByVersionTx(ctx, tx, tenant, taskID, version, true)
		if err != nil {
			return err
		}
		m, err := ensureManifestTx(ctx, tx, tenant, taskID, true)
		if err != nil {
			return err
		}
		switch o.Status {
		case OutlineDraft, OutlinePending:
			note := o.Note
			if in.Note != "" {
				note = in.Note
			}
			if _, err := tx.ExecContext(ctx, `UPDATE proposal_outlines SET status = 'draft', note = $2, requirements_epoch = $3,
				submitted_by = '', submitted_at = NULL, updated_by = $4 WHERE id = $1`, o.ID, note, m.Epoch, actor); err != nil {
				return err
			}
			if len(in.Sections) > 0 {
				old, err := sectionsOfOutlineTx(ctx, tx, tenant, o.ID)
				if err != nil {
					return err
				}
				derived := map[string]int64{}
				for _, s := range old {
					if s.DerivedFromSectionID != nil {
						derived[s.Code] = *s.DerivedFromSectionID
					}
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM proposal_sections WHERE tenant_id = $1 AND outline_id = $2`, tenant, o.ID); err != nil {
					return err
				}
				o.Version = version
				if err := insertSectionsTx(ctx, tx, tenant, taskID, o, in.Sections, derived, actor); err != nil {
					return err
				}
			}
			if o, err = outlineByVersionTx(ctx, tx, tenant, taskID, version, false); err != nil {
				return err
			}
		case OutlineApproved, OutlineSuperseded:
			var open bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM proposal_outlines WHERE tenant_id = $1 AND task_id = $2
				AND status IN ('draft','pending_approval'))`, tenant, taskID).Scan(&open); err != nil {
				return err
			}
			if open {
				return fmt.Errorf("%w: an open draft outline already exists; edit it instead", ErrProposalConflict)
			}
			if o.Status == OutlineApproved {
				if _, err := tx.ExecContext(ctx, `UPDATE proposal_outlines SET status = 'superseded', superseded_at = now(), updated_by = $2
					WHERE id = $1`, o.ID, actor); err != nil {
					return err
				}
			}
			note := in.Note
			if note == "" {
				note = fmt.Sprintf("基于 v%d 调整", o.Version)
			}
			if o, err = deriveOutlineTx(ctx, tx, tenant, taskID, o, in.Sections, note, actor); err != nil {
				return err
			}
		}
		out, err = outlineDetailTx(ctx, tx, tenant, o)
		return err
	})
	return out, err
}

// deriveOutlineTx 插入 version = max+1 的草案大纲。sections 为空时复制 from 的章节结构、依赖与仍存在的要求映射。
func deriveOutlineTx(ctx context.Context, tx *sql.Tx, tenant string, taskID int64, from *ProposalOutline, sections []SectionInput, note, actor string) (*ProposalOutline, error) {
	m, err := ensureManifestTx(ctx, tx, tenant, taskID, false)
	if err != nil {
		return nil, err
	}
	derived := map[string]int64{}
	var fromVersion *int
	if from != nil {
		v := from.Version
		fromVersion = &v
		old, err := sectionsOfOutlineTx(ctx, tx, tenant, from.ID)
		if err != nil {
			return nil, err
		}
		byID := map[int64]*ProposalSection{}
		for _, s := range old {
			byID[s.ID] = s
			derived[s.Code] = s.ID
		}
		if len(sections) == 0 {
			supMap := map[int64]int64{}
			srows, err := tx.QueryContext(ctx, `SELECT id, superseded_by_id FROM requirement_items
				WHERE tenant_id = $1 AND task_id = $2 AND superseded_by_id IS NOT NULL`, tenant, taskID)
			if err != nil {
				return nil, err
			}
			for srows.Next() {
				var oldID, newID int64
				if err := srows.Scan(&oldID, &newID); err != nil {
					srows.Close()
					return nil, err
				}
				supMap[oldID] = newID
			}
			srows.Close()
			if err := srows.Err(); err != nil {
				return nil, err
			}
			for _, s := range old {
				deps := []string{}
				for _, id := range s.DependsOn {
					if dep := byID[id]; dep != nil {
						deps = append(deps, dep.Code)
					}
				}
				rids := make([]int64, 0, len(s.RequirementIDs))
				for _, rid := range s.RequirementIDs {
					for guard := 0; guard < 8; guard++ {
						next, ok := supMap[rid]
						if !ok {
							break
						}
						rid = next
					}
					rids = append(rids, rid)
				}
				sections = append(sections, SectionInput{Code: s.Code, Title: s.Title, Level: s.Level, WritingGuide: s.WritingGuide,
					TargetWords: s.TargetWords, RequirementIDs: uniqueIDs(rids), DependsOn: deps})
			}
		}
	}
	o, err := scanOutline(tx.QueryRowContext(ctx, `INSERT INTO proposal_outlines(tenant_id, task_id, version, requirements_epoch, status,
		derived_from_version, note, created_by, updated_by)
		VALUES ($1, $2, (SELECT COALESCE(MAX(version), 0) + 1 FROM proposal_outlines WHERE tenant_id = $1 AND task_id = $2), $3, 'draft', $4, $5, $6, $6)
		RETURNING `+outlineCols, tenant, taskID, m.Epoch, fromVersion, note, actor))
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: an open draft outline already exists", ErrProposalConflict)
		}
		return nil, err
	}
	if err := insertSectionsTx(ctx, tx, tenant, taskID, o, sections, derived, actor); err != nil {
		return nil, err
	}
	return o, nil
}

// insertSectionsTx 校验并写入章节：编码唯一、依赖指向本大纲章节且无环、要求属于本任务（复合外键二次保证）。
func insertSectionsTx(ctx context.Context, tx *sql.Tx, tenant string, taskID int64, o *ProposalOutline, in []SectionInput, derived map[string]int64, actor string) error {
	if len(in) == 0 {
		return fmt.Errorf("%w: outline needs at least one section", ErrInvalidOutline)
	}
	idx := map[string]int{}
	for i, s := range in {
		s.Code, s.Title = strings.TrimSpace(s.Code), strings.TrimSpace(s.Title)
		if s.Code == "" || s.Title == "" {
			return fmt.Errorf("%w: section %d needs section_code and title", ErrInvalidOutline, i+1)
		}
		if _, dup := idx[s.Code]; dup {
			return fmt.Errorf("%w: duplicate section_code %q", ErrInvalidOutline, s.Code)
		}
		if s.TargetWords < 0 {
			return fmt.Errorf("%w: target_words must be >= 0", ErrInvalidOutline)
		}
		idx[s.Code] = i
		in[i] = s
	}
	// 依赖校验：存在性 + DFS 判环。
	for _, s := range in {
		for _, dep := range s.DependsOn {
			if _, ok := idx[strings.TrimSpace(dep)]; !ok {
				return fmt.Errorf("%w: section %q depends on unknown section %q", ErrInvalidOutline, s.Code, dep)
			}
			if strings.TrimSpace(dep) == s.Code {
				return fmt.Errorf("%w: section %q depends on itself", ErrInvalidOutline, s.Code)
			}
		}
	}
	state := make([]int, len(in)) // 0 未访问 1 访问中 2 完成
	var visit func(i int, path []string) error
	visit = func(i int, path []string) error {
		switch state[i] {
		case 1:
			return fmt.Errorf("%w: dependency cycle %s -> %s", ErrInvalidOutline, strings.Join(path, " -> "), in[i].Code)
		case 2:
			return nil
		}
		state[i] = 1
		for _, dep := range in[i].DependsOn {
			if err := visit(idx[strings.TrimSpace(dep)], append(path, in[i].Code)); err != nil {
				return err
			}
		}
		state[i] = 2
		return nil
	}
	for i := range in {
		if err := visit(i, nil); err != nil {
			return err
		}
	}
	// 要求归属校验。
	var reqIDs []int64
	for _, s := range in {
		reqIDs = append(reqIDs, s.RequirementIDs...)
	}
	reqIDs = uniqueIDs(reqIDs)
	if len(reqIDs) > 0 {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM requirement_items WHERE tenant_id = $1 AND task_id = $2 AND id = ANY($3::bigint[])`,
			tenant, taskID, pgInt64Array(reqIDs)).Scan(&n); err != nil {
			return err
		}
		if n != len(reqIDs) {
			return fmt.Errorf("%w: requirement_item_ids must belong to this task", ErrInvalidOutline)
		}
	}
	ids := make([]int64, len(in))
	for i, s := range in {
		level := s.Level
		if level <= 0 {
			level = strings.Count(s.Code, ".") + 1
		}
		var from *int64
		if id, ok := derived[s.Code]; ok {
			from = &id
		}
		if err := tx.QueryRowContext(ctx, `INSERT INTO proposal_sections(tenant_id, task_id, outline_id, outline_version, section_code, title, level, seq,
			writing_guide, target_words, derived_from_section_id, status, created_by, updated_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'pending',$12,$12) RETURNING id`,
			tenant, taskID, o.ID, o.Version, s.Code, s.Title, level, i, s.WritingGuide, s.TargetWords, from, actor).Scan(&ids[i]); err != nil {
			return err
		}
		for _, rid := range uniqueIDs(s.RequirementIDs) {
			if _, err := tx.ExecContext(ctx, `INSERT INTO proposal_section_requirements(tenant_id, task_id, section_id, requirement_id, created_by, updated_by)
				VALUES ($1,$2,$3,$4,$5,$5)`, tenant, taskID, ids[i], rid, actor); err != nil {
				return err
			}
		}
	}
	for i, s := range in {
		if len(s.DependsOn) == 0 {
			continue
		}
		var deps []int64
		for _, dep := range s.DependsOn {
			deps = append(deps, ids[idx[strings.TrimSpace(dep)]])
		}
		if _, err := tx.ExecContext(ctx, `UPDATE proposal_sections SET depends_on_section_ids = $2::bigint[] WHERE id = $1`,
			ids[i], pgInt64Array(uniqueIDs(deps))); err != nil {
			return err
		}
	}
	return nil
}

// DefaultOutlineSections 按要求清单生成默认技术标目录：总体方案（其余章节的前置依赖）+ 按招标评分表顺序逐个技术评分项成节 +
// 商务评分项响应（资信与证明材料索引）+ ★实质性条款响应 + 其他必答项 + 格式与附件。价格评分项由报价文件响应，不进入技术标目录；
// 其余要求均被映射。
func DefaultOutlineSections(reqs []*RequirementItem) []SectionInput {
	by := map[string][]*RequirementItem{}
	byID := map[int64]*RequirementItem{}
	var commercial []*RequirementItem
	var passedVetoes []*RequirementItem
	for _, r := range reqs {
		byID[r.ID] = r
		if !r.OutOfBid && !r.IsSuperseded() && r.Category == ReqVetoItem && r.VetoStatus == VetoPassed && strings.TrimSpace(r.VetoNote) != "" {
			passedVetoes = append(passedVetoes, r)
		}
		switch {
		case !r.InTechnicalScope():
		case r.Category == ReqScoringItem && r.ScoreGroup == ScoreGroupCommercial:
			commercial = append(commercial, r)
		default:
			by[r.Category] = append(by[r.Category], r)
		}
	}
	byOrder := func(items []*RequirementItem) {
		sort.SliceStable(items, func(i, j int) bool { return items[i].ID < items[j].ID }) // 提取顺序即招标评分表顺序
	}
	ids := func(items []*RequirementItem) []int64 {
		out := make([]int64, 0, len(items))
		for _, r := range items {
			out = append(out, r.ID)
		}
		return out
	}
	out := []SectionInput{{Code: "1", Title: "总体技术方案", Level: 1, TargetWords: 3000,
		WritingGuide: "阐述项目理解、总体架构与技术路线；summary_note 作为后续各章的衔接基准（术语、架构名称、关键承诺）。"}}
	n := 1
	if items := by[ReqScoringItem]; len(items) > 0 {
		n++
		chap := fmt.Sprint(n)
		out = append(out, SectionInput{Code: chap, Title: "技术评分项逐项响应", Level: 1, DependsOn: []string{"1"},
			WritingGuide: "本章为技术评分项索引，按招标评分表顺序概述各评分点的响应策略。"})
		byOrder(items)
		for i, r := range items {
			guide := fmt.Sprintf("按评分标准逐点响应评分项 %s（分值 %.4g）；评分标准列出的每个要点都要有对应内容，"+
				"只陈述有核验证据支撑的能力，缺证据处写明缺口。", r.Code, r.ScoreWeight)
			if len(r.ScoringTiers) > 0 {
				tiers := make([]string, 0, len(r.ScoringTiers))
				for _, t := range r.ScoringTiers {
					tiers = append(tiers, fmt.Sprintf("%s(%.4g分): %s", t.Label, t.ScoreMax, firstLine(t.Criteria, 36)))
				}
				guide += " 评分档次：" + strings.Join(tiers, "；") + "。"
			}
			if r.ProofRules.RawSummary != "" {
				guide += " 证明要求：" + r.ProofRules.RawSummary + "。"
			}
			if len(r.LinkedRequirementIDs) > 0 {
				var lcodes []string
				for _, lid := range r.LinkedRequirementIDs {
					if lr := byID[lid]; lr != nil && !lr.IsSuperseded() {
						lcodes = append(lcodes, lr.Code)
					}
				}
				if len(lcodes) > 0 {
					guide += " 联动响应采购需求条款：" + strings.Join(lcodes, "、") + "。"
				}
			}
			out = append(out, SectionInput{Code: fmt.Sprintf("%s.%d", chap, i+1), Title: r.Title, Level: 2, TargetWords: 1500,
				RequirementIDs: []int64{r.ID}, DependsOn: []string{"1"}, WritingGuide: guide})
		}
	}
	if len(commercial) > 0 {
		n++
		byOrder(commercial)
		ids := make([]int64, 0, len(commercial))
		var proofs []string
		for _, r := range commercial {
			ids = append(ids, r.ID)
			if r.ProofRules.RawSummary != "" {
				proofs = append(proofs, fmt.Sprintf("%s(%s)", r.Code, r.ProofRules.RawSummary))
			}
		}
		guide := "逐条列明满足商务评分项的资质、业绩、人员与认证证明材料及其在投标文件中的位置，材料从企业资信库选取，不得编造。"
		if len(proofs) > 0 {
			guide += " 证明细则：" + strings.Join(proofs, "；") + "。"
		}
		if len(passedVetoes) > 0 {
			guide += fmt.Sprintf(" 同时与 %d 项已核对通过的资格/符合性审查项证明位置保持一致。", len(passedVetoes))
		}
		out = append(out, SectionInput{Code: fmt.Sprint(n), Title: "商务评分项响应（资信与证明材料）", Level: 1, RequirementIDs: ids,
			DependsOn: []string{"1"}, TargetWords: 800, WritingGuide: guide})
	}
	for _, g := range []struct{ cat, title, guide string }{
		{ReqMandatoryClause, "★ 实质性条款逐条响应", "逐条对应 ★/▲/废标条款给出明确响应与证明材料索引，不得遗漏或模糊表述。"},
		{ReqNonScoringRequired, "其他必答项响应", "逐条响应非评分但必须答复的要求。"},
		{ReqFormatAttachment, "投标文件格式与附件", "列明格式、签章、装订与附件要求的落实情况。"},
	} {
		if items := by[g.cat]; len(items) > 0 {
			n++
			out = append(out, SectionInput{Code: fmt.Sprint(n), Title: g.title, Level: 1, RequirementIDs: ids(items), DependsOn: []string{"1"},
				WritingGuide: g.guide, TargetWords: 1000})
		}
	}
	return out
}

func checkOutlineReadyTx(ctx context.Context, tx *sql.Tx, tenant string, o *ProposalOutline) error {
	m, err := ensureManifestTx(ctx, tx, tenant, o.TaskID, true)
	if err != nil {
		return err
	}
	if m.Status != ManifestConfirmed {
		return fmt.Errorf("%w: manifest status is %s", ErrRequirementsNotConfirmed, m.Status)
	}
	if m.Epoch != o.RequirementsEpoch {
		return fmt.Errorf("%w: outline built for epoch %d, manifest is at %d; update the outline", ErrRequirementsNotConfirmed, o.RequirementsEpoch, m.Epoch)
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM proposal_sections WHERE tenant_id = $1 AND outline_id = $2`, tenant, o.ID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: outline has no sections", ErrInvalidOutline)
	}
	srows, err := tx.QueryContext(ctx, `SELECT DISTINCT r.code FROM proposal_section_requirements m
		JOIN proposal_sections s ON s.tenant_id = m.tenant_id AND s.id = m.section_id
		JOIN requirement_items r ON r.tenant_id = m.tenant_id AND r.id = m.requirement_id
		WHERE m.tenant_id = $1 AND s.outline_id = $2 AND (r.superseded_by_id IS NOT NULL OR r.review_status = 'stale')
		ORDER BY r.code`, tenant, o.ID)
	if err != nil {
		return err
	}
	var staleCodes []string
	for srows.Next() {
		var code string
		if err := srows.Scan(&code); err != nil {
			srows.Close()
			return err
		}
		staleCodes = append(staleCodes, code)
	}
	srows.Close()
	if err := srows.Err(); err != nil {
		return err
	}
	if len(staleCodes) > 0 {
		return fmt.Errorf("%w: outline still maps superseded requirement(s) (%s)", ErrOutlineIncomplete, strings.Join(staleCodes, ", "))
	}
	cov, err := outlineCoverageTx(ctx, tx, tenant, o.TaskID, o.ID)
	if err != nil {
		return err
	}
	if len(cov.Unmapped) > 0 {
		codes := make([]string, 0, len(cov.Unmapped))
		for _, r := range cov.Unmapped {
			codes = append(codes, r.Code)
		}
		return fmt.Errorf("%w: %d unmapped (%s)", ErrOutlineIncomplete, len(codes), strings.Join(codes, ", "))
	}
	return nil
}

// SubmitOutline：draft -> pending_approval（持久化；在此状态下所有写作派发与写回均被拒绝）。
func (d *DB) SubmitOutline(ctx context.Context, tenant string, taskID int64, version int, actor string) (*OutlineDetail, error) {
	return d.transitionOutline(ctx, tenant, taskID, version, actor, OutlineDraft, func(tx *sql.Tx, o *ProposalOutline) error {
		_, err := tx.ExecContext(ctx, `UPDATE proposal_outlines SET status = 'pending_approval', submitted_by = $2, submitted_at = now(), updated_by = $2
			WHERE id = $1`, o.ID, actor)
		return err
	})
}

// ApproveOutline：pending_approval -> approved。旧批准版本被取代；章节按依赖计算 pending / ready。
func (d *DB) ApproveOutline(ctx context.Context, tenant string, taskID int64, version int, actor string) (*OutlineDetail, error) {
	tenant = tenantOr(tenant)
	return d.transitionOutline(ctx, tenant, taskID, version, actor, OutlinePending, func(tx *sql.Tx, o *ProposalOutline) error {
		if _, err := tx.ExecContext(ctx, `UPDATE proposal_outlines SET status = 'superseded', superseded_at = now(), updated_by = $3
			WHERE tenant_id = $1 AND task_id = $2 AND status = 'approved'`, tenant, taskID, actor); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE proposal_outlines SET status = 'approved', approved_by = $2, approved_at = now(), updated_by = $2
			WHERE id = $1`, o.ID, actor); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE proposal_sections SET approved_by = $3, updated_by = $3 WHERE tenant_id = $1 AND outline_id = $2`,
			tenant, o.ID, actor); err != nil {
			return err
		}
		return refreshReadinessTx(ctx, tx, tenant, o.ID)
	})
}

func (d *DB) transitionOutline(ctx context.Context, tenant string, taskID int64, version int, actor, from string, apply func(*sql.Tx, *ProposalOutline) error) (*OutlineDetail, error) {
	tenant = tenantOr(tenant)
	var out *OutlineDetail
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		o, err := outlineByVersionTx(ctx, tx, tenant, taskID, version, true)
		if err != nil {
			return err
		}
		if o.Status != from {
			return fmt.Errorf("%w: outline v%d is %s, expected %s", ErrOutlineState, o.Version, o.Status, from)
		}
		if err := checkOutlineReadyTx(ctx, tx, tenant, o); err != nil {
			return err
		}
		if err := apply(tx, o); err != nil {
			return err
		}
		if o, err = outlineByVersionTx(ctx, tx, tenant, taskID, version, false); err != nil {
			return err
		}
		out, err = outlineDetailTx(ctx, tx, tenant, o)
		return err
	})
	return out, err
}

// ---------------------------------------------------------------------------
// 章节：派发门禁、写回门禁、批准与回滚
// ---------------------------------------------------------------------------

// sectionGateTx 依「大纲 -> 章节」顺序加锁并返回章节、其大纲状态与当前批准大纲。
// 章节所属大纲仍在 draft / pending_approval 时返回 ErrOutlineNotApproved。
func sectionGateTx(ctx context.Context, tx *sql.Tx, tenant string, taskID, sectionID int64) (*ProposalSection, string, *ProposalOutline, error) {
	if err := CheckSectionScope(ctx, sectionID); err != nil {
		return nil, "", nil, err
	}
	var outlineStatus string
	err := tx.QueryRowContext(ctx, `SELECT o.status FROM proposal_sections s JOIN proposal_outlines o ON o.id = s.outline_id
		WHERE s.tenant_id = $1 AND s.task_id = $2 AND s.id = $3`, tenant, taskID, sectionID).Scan(&outlineStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", nil, ErrNotFound
	}
	if err != nil {
		return nil, "", nil, err
	}
	approved, err := approvedOutlineTx(ctx, tx, tenant, taskID)
	if err != nil {
		return nil, "", nil, err
	}
	s, err := scanSection(tx.QueryRowContext(ctx, `SELECT `+sectionCols+` FROM proposal_sections s
		WHERE s.tenant_id = $1 AND s.task_id = $2 AND s.id = $3 FOR UPDATE`, tenant, taskID, sectionID))
	if err != nil {
		return nil, "", nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT status FROM proposal_outlines WHERE id = $1`, s.OutlineID).Scan(&outlineStatus); err != nil {
		return nil, "", nil, err
	}
	return s, outlineStatus, approved, nil
}

func requireApprovedSection(s *ProposalSection, outlineStatus string, approved *ProposalOutline) error {
	if outlineStatus == OutlineDraft || outlineStatus == OutlinePending {
		return fmt.Errorf("%w: outline v%d is %s", ErrOutlineNotApproved, s.OutlineVersion, outlineStatus)
	}
	if approved == nil {
		return fmt.Errorf("%w: task has no approved outline", ErrOutlineNotApproved)
	}
	if approved.ID != s.OutlineID || approved.Version != s.OutlineVersion {
		return fmt.Errorf("%w: section is in v%d, approved outline is v%d", ErrOutlineStale, s.OutlineVersion, approved.Version)
	}
	return nil
}

// SectionClaim 是派发成功后的写作快照：Worker 写回时必须原样携带 BaseRevNo 与 BasedOnOutlineVersion。
type SectionClaim struct {
	TaskID                int64     `json:"task_id"`
	SectionID             int64     `json:"section_id"`
	SectionCode           string    `json:"section_code"`
	LeaseToken            int64     `json:"lease_token"`
	LeaseUntil            time.Time `json:"lease_until"`
	BaseRevNo             int       `json:"base_rev_no"`
	BasedOnOutlineVersion int       `json:"based_on_outline_version"`
}

// ClaimSectionWrite 是写作派发前门禁：任务须有批准大纲且章节属于该版本、全部前置依赖已有有效稿、无在途租约。
func (d *DB) ClaimSectionWrite(ctx context.Context, tenant string, taskID, sectionID int64, actor string, ttl time.Duration) (*SectionClaim, error) {
	tenant = tenantOr(tenant)
	if ttl < 20*time.Second || ttl > 60*time.Second {
		ttl = SectionLeaseTTL
	}
	var claim *SectionClaim
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		s, outlineStatus, approved, err := sectionGateTx(ctx, tx, tenant, taskID, sectionID)
		if err != nil {
			return err
		}
		if err := requireApprovedSection(s, outlineStatus, approved); err != nil {
			return err
		}
		unmet, err := unmetDepsTx(ctx, tx, tenant, s)
		if err != nil {
			return err
		}
		if len(unmet) > 0 {
			return fmt.Errorf("%w: waiting for %s", ErrSectionDependencyUnmet, strings.Join(unmet, ", "))
		}
		if s.Writing {
			return fmt.Errorf("%w: lease held until %s", ErrSectionBusy, s.LeaseUntil.Format(time.RFC3339))
		}
		if err := d3CheckSectionWriteGateTx(ctx, tx, tenant, taskID, s.ID); err != nil {
			return err
		}
		claim = &SectionClaim{TaskID: taskID, SectionID: s.ID, SectionCode: s.Code, BasedOnOutlineVersion: approved.Version}
		return tx.QueryRowContext(ctx, `UPDATE proposal_sections SET lease_token = lease_token + 1,
			lease_until = now() + make_interval(secs => $2), updated_by = $3,
			status = CASE WHEN current_rev_no = 0 THEN 'writing' ELSE status END
			WHERE id = $1 RETURNING lease_token, lease_until, current_rev_no`, s.ID, ttl.Seconds(), actor).
			Scan(&claim.LeaseToken, &claim.LeaseUntil, &claim.BaseRevNo)
	})
	return claim, err
}

// RenewSectionLease 续期在途写作租约；令牌已被新的派发取代或大纲已失效时返回 ErrLeaseLost。
func (d *DB) RenewSectionLease(ctx context.Context, tenant string, sectionID, token int64, ttl time.Duration) error {
	if ttl < 20*time.Second || ttl > 60*time.Second {
		ttl = SectionLeaseTTL
	}
	res, err := d.ExecContext(ctx, `UPDATE proposal_sections SET lease_until = now() + make_interval(secs => $4)
		WHERE tenant_id = $1 AND id = $2 AND lease_token = $3 AND lease_until IS NOT NULL AND status <> 'stale_outline'`,
		tenantOr(tenant), sectionID, token, ttl.Seconds())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrLeaseLost
	}
	return nil
}

// ReleaseSectionClaim 在写作失败 / 取消时释放租约（令牌不匹配时什么也不做）。
func (d *DB) ReleaseSectionClaim(ctx context.Context, tenant string, sectionID, token int64) error {
	tenant = tenantOr(tenant)
	return d.InTx(ctx, func(tx *sql.Tx) error {
		var outlineID int64
		err := tx.QueryRowContext(ctx, `UPDATE proposal_sections SET lease_until = NULL WHERE tenant_id = $1 AND id = $2 AND lease_token = $3
			RETURNING outline_id`, tenant, sectionID, token).Scan(&outlineID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return refreshReadinessTx(ctx, tx, tenant, outlineID)
	})
}

// SaveRevisionInput 是一次章节写回（人工保存或 AI Worker 写回）。
type SaveRevisionInput struct {
	TaskID                int64         `json:"-"`
	SectionID             int64         `json:"-"`
	BaseRevNo             int           `json:"base_rev_no"`
	BasedOnOutlineVersion int           `json:"based_on_outline_version"`
	Source                string        `json:"source"`
	ContentMD             string        `json:"content_md"`
	SummaryNote           string        `json:"summary_note"`
	DiffSummary           string        `json:"diff_summary"`
	EvidenceIDs           []int64       `json:"evidence_ids"`
	CommitmentIDs         []int64       `json:"commitment_ids"`
	AdmissibilityItemIDs  []int64       `json:"admissibility_item_ids"`
	Gaps                  []RevisionGap `json:"gaps"`
	LeaseToken            int64         `json:"lease_token"`
}

// SaveRevisionResult：Outcome 为 applied（成为当前主版本）、stale_outline 或 conflict_candidate（仅归档候选稿）。
type SaveRevisionResult struct {
	Outcome  string           `json:"outcome"`
	Revision *SectionRevision `json:"revision"`
	Section  *ProposalSection `json:"section"`
	Reason   string           `json:"reason,omitempty"`
}

func insertRevisionTx(ctx context.Context, tx *sql.Tx, tenant string, s *ProposalSection, in SaveRevisionInput, rollbackOf *int, gaps []RevisionGap, status, actor string) (*SectionRevision, error) {
	raw, _ := json.Marshal(gaps)
	rev, err := scanRevision(tx.QueryRowContext(ctx, `INSERT INTO section_revisions(tenant_id, task_id, section_id, rev_no, base_rev_no,
		based_on_outline_version, source, rollback_of_rev_no, content_md, summary_note, diff_summary, evidence_ids, commitment_ids, gaps,
		status, created_by, updated_by)
		VALUES ($1, $2, $3, (SELECT COALESCE(MAX(rev_no), 0) + 1 FROM section_revisions WHERE tenant_id = $1 AND section_id = $3),
		        $4, $5, $6, $7, $8, $9, $10, $11::bigint[], $12::bigint[], $13::jsonb, $14, $15, $15)
		RETURNING `+revisionCols, tenant, s.TaskID, s.ID, in.BaseRevNo, in.BasedOnOutlineVersion, in.Source, rollbackOf, in.ContentMD,
		in.SummaryNote, in.DiffSummary, pgInt64Array(in.EvidenceIDs), pgInt64Array(in.CommitmentIDs), string(raw), status, actor))
	if err != nil {
		return nil, err
	}
	if err := insertSectionRevisionEvidenceTx(ctx, tx, tenant, s.TaskID, rev.ID, in.AdmissibilityItemIDs, actor); err != nil {
		return nil, err
	}
	return rev, nil
}

func sectionByIDTx(ctx context.Context, q queryer, tenant string, id int64) (*ProposalSection, error) {
	return scanSection(q.QueryRowContext(ctx, `SELECT `+sectionCols+` FROM proposal_sections s WHERE s.tenant_id = $1 AND s.id = $2`, tenant, id))
}

// SaveSectionRevision 是写回时二次门禁（见文件头不变量 2）。
func (d *DB) SaveSectionRevision(ctx context.Context, tenant, actor string, in SaveRevisionInput) (*SaveRevisionResult, error) {
	if err := CheckSectionScope(ctx, in.SectionID); err != nil {
		return nil, err
	}
	tenant = tenantOr(tenant)
	switch in.Source {
	case RevAIInitial, RevAIRewrite, RevHumanEdit:
	default:
		return nil, fmt.Errorf("%w: source must be ai_initial, ai_rewrite or human_edit", ErrInvalidRevision)
	}
	if in.BaseRevNo < 0 || in.BasedOnOutlineVersion <= 0 {
		return nil, fmt.Errorf("%w: base_rev_no >= 0 and based_on_outline_version > 0 are required", ErrInvalidRevision)
	}
	if strings.TrimSpace(in.ContentMD) == "" {
		return nil, fmt.Errorf("%w: content_md is empty", ErrInvalidRevision)
	}
	in.EvidenceIDs = uniqueIDs(in.EvidenceIDs)
	in.CommitmentIDs = uniqueIDs(in.CommitmentIDs)
	in.AdmissibilityItemIDs = uniqueIDs(in.AdmissibilityItemIDs)
	var out *SaveRevisionResult
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		if err := lockIntentProducerTx(ctx, tx, in.TaskID); err != nil {
			return err
		}
		s, outlineStatus, approved, err := sectionGateTx(ctx, tx, tenant, in.TaskID, in.SectionID)
		if err != nil {
			return err
		}
		if outlineStatus == OutlineDraft || outlineStatus == OutlinePending {
			return fmt.Errorf("%w: outline v%d is %s", ErrOutlineNotApproved, s.OutlineVersion, outlineStatus)
		}
		sb, d3Res, err := d3PrepareAndVerifySaveRevisionTx(ctx, tx, tenant, s, &in)
		if err != nil {
			return err
		}
		if _, err := validateCommitmentRefsTx(ctx, tx, tenant, in.TaskID, in.CommitmentIDs, in.ContentMD); err != nil {
			return err
		}
		if _, err := validateAdmissibilityRefsTx(ctx, tx, tenant, in.TaskID, in.AdmissibilityItemIDs); err != nil {
			return err
		}
		states, err := evidenceStatesTx(ctx, tx, tenant, in.TaskID, in.EvidenceIDs)
		if err != nil {
			return err
		}
		for _, id := range in.EvidenceIDs {
			if _, ok := states[id]; !ok {
				return fmt.Errorf("%w: evidence %d not found for this task", ErrEvidenceInvalid, id)
			}
		}
		auto, anyStale := evidenceGaps(states, in.EvidenceIDs)
		gaps := mergeGaps(in.Gaps, auto)
		releaseLease := in.LeaseToken != 0 && in.LeaseToken == s.LeaseToken
		archive := func(status, reason string) error {
			rev, err := insertRevisionTx(ctx, tx, tenant, s, in, nil, gaps, status, actor)
			if err != nil {
				return err
			}
			if releaseLease {
				if _, err := tx.ExecContext(ctx, `UPDATE proposal_sections SET lease_until = NULL WHERE id = $1`, s.ID); err != nil {
					return err
				}
				if err := refreshReadinessTx(ctx, tx, tenant, s.OutlineID); err != nil {
					return err
				}
			}
			cur, err := sectionByIDTx(ctx, tx, tenant, s.ID)
			out = &SaveRevisionResult{Outcome: status, Revision: rev, Section: cur, Reason: reason}
			return err
		}
		// 情况 A：在途大纲已失效 —— 只能归档为 stale_outline 候选稿。
		if approved == nil || approved.Version != in.BasedOnOutlineVersion || s.OutlineID != approved.ID || s.OutlineVersion != in.BasedOnOutlineVersion {
			cur := 0
			if approved != nil {
				cur = approved.Version
			}
			return archive(SaveStale, fmt.Sprintf("based_on_outline_version %d is not the approved outline (current approved v%d, section in v%d)",
				in.BasedOnOutlineVersion, cur, s.OutlineVersion))
		}
		if isAIRevision(in.Source) {
			unmet, err := unmetDepsTx(ctx, tx, tenant, s)
			if err != nil {
				return err
			}
			if len(unmet) > 0 {
				return fmt.Errorf("%w: waiting for %s", ErrSectionDependencyUnmet, strings.Join(unmet, ", "))
			}
		}
		// 情况 B：并发版本冲突 —— 不覆盖当前主版本。
		if in.BaseRevNo != s.CurrentRevNo {
			return archive(SaveConflict, fmt.Sprintf("base_rev_no %d != current_rev_no %d", in.BaseRevNo, s.CurrentRevNo))
		}
		// 情况 C：正常写回。
		status := RevDraft
		if isAIRevision(in.Source) {
			status = RevReviewing
		}
		if anyStale {
			status = RevStaleEvid
		}
		rev, err := insertRevisionTx(ctx, tx, tenant, s, in, nil, gaps, status, actor)
		if err != nil {
			return err
		}
		if d3Res != nil {
			if _, err := persistWritingVerificationTx(ctx, tx, tenant, s, rev.ID, rev.RevNo, sb, *d3Res, actor); err != nil {
				return err
			}
		}
		if err := recordIntentOutputTx(ctx, tx, tenant, rev, actor); err != nil {
			return err
		}
		lease := "lease_until"
		if releaseLease || isAIRevision(in.Source) {
			lease = "NULL"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE proposal_sections SET current_rev_no = $2, approved_rev_no = NULL, approved_by = '',
			status = $3, lease_until = `+lease+`, updated_by = $4 WHERE id = $1`, s.ID, rev.RevNo, status, actor); err != nil {
			return err
		}
		if err := refreshReadinessTx(ctx, tx, tenant, s.OutlineID); err != nil {
			return err
		}
		cur, err := sectionByIDTx(ctx, tx, tenant, s.ID)
		out = &SaveRevisionResult{Outcome: SaveApplied, Revision: rev, Section: cur}
		return err
	})
	return out, err
}

// ApproveSectionRevision 批准指定 rev_no：须为当前主版本、大纲仍是批准版本、修订未失效且引用证据无 stale / contradicts。
func (d *DB) ApproveSectionRevision(ctx context.Context, tenant string, taskID, sectionID int64, revNo int, actor string) (*ProposalSection, error) {
	tenant = tenantOr(tenant)
	var out *ProposalSection
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		s, outlineStatus, approved, err := sectionGateTx(ctx, tx, tenant, taskID, sectionID)
		if err != nil {
			return err
		}
		if err := requireApprovedSection(s, outlineStatus, approved); err != nil {
			return err
		}
		if revNo <= 0 || revNo != s.CurrentRevNo {
			return fmt.Errorf("%w: rev %d is not the current revision %d", ErrRevisionConflict, revNo, s.CurrentRevNo)
		}
		rev, err := scanRevision(tx.QueryRowContext(ctx, `SELECT `+revisionCols+` FROM section_revisions WHERE tenant_id = $1 AND section_id = $2 AND rev_no = $3`,
			tenant, s.ID, revNo))
		if err != nil {
			return err
		}
		if rev.Status != RevDraft && rev.Status != RevReviewing && rev.Status != RevApproved {
			return fmt.Errorf("%w: revision status is %s", ErrEvidenceInvalid, rev.Status)
		}
		if err := d3CheckApproveSectionRevisionTx(ctx, tx, tenant, s, rev, actor); err != nil {
			return err
		}
		if _, err := validateCommitmentRefsTx(ctx, tx, tenant, taskID, rev.CommitmentIDs, rev.ContentMD); err != nil {
			return err
		}
		linkedItems, err := linkedAdmissibilityItemIDsTx(ctx, tx, tenant, rev.ID)
		if err != nil {
			return err
		}
		if _, err := validateAdmissibilityRefsTx(ctx, tx, tenant, taskID, linkedItems); err != nil {
			return err
		}
		states, err := evidenceStatesTx(ctx, tx, tenant, taskID, rev.EvidenceIDs)
		if err != nil {
			return err
		}
		for _, id := range rev.EvidenceIDs {
			e, ok := states[id]
			if !ok || e.Status == "stale" || e.Relation == "contradicts" {
				return fmt.Errorf("%w: evidence %d is stale, missing or contradicting; save a corrected revision first", ErrEvidenceInvalid, id)
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE section_revisions SET status = 'approved', approved_by = $2, approved_at = now(), updated_by = $2
			WHERE id = $1`, rev.ID, actor); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE proposal_sections SET approved_rev_no = $2, status = 'approved', approved_by = $3, updated_by = $3
			WHERE id = $1`, s.ID, revNo, actor); err != nil {
			return err
		}
		out, err = sectionByIDTx(ctx, tx, tenant, s.ID)
		return err
	})
	return out, err
}

// RollbackSectionRevision 基于当前主版本生成一条 rollback 修订，拷贝目标修订的正文、摘要与仍有效的证据；
// 目标为本章节的冲突候选稿时等价于「采用候选稿」。expectedBase 非 nil 时须等于 current_rev_no。
func (d *DB) RollbackSectionRevision(ctx context.Context, tenant string, taskID, sectionID int64, targetRevNo int, expectedBase *int, actor string) (*SaveRevisionResult, error) {
	tenant = tenantOr(tenant)
	var out *SaveRevisionResult
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		s, outlineStatus, approved, err := sectionGateTx(ctx, tx, tenant, taskID, sectionID)
		if err != nil {
			return err
		}
		if err := requireApprovedSection(s, outlineStatus, approved); err != nil {
			return err
		}
		if expectedBase != nil && *expectedBase != s.CurrentRevNo {
			return fmt.Errorf("%w: base_rev_no %d != current_rev_no %d", ErrRevisionConflict, *expectedBase, s.CurrentRevNo)
		}
		if targetRevNo == s.CurrentRevNo {
			return fmt.Errorf("%w: rev %d is already current", ErrInvalidRevision, targetRevNo)
		}
		target, err := scanRevision(tx.QueryRowContext(ctx, `SELECT `+revisionCols+` FROM section_revisions WHERE tenant_id = $1 AND section_id = $2 AND rev_no = $3`,
			tenant, s.ID, targetRevNo))
		if err != nil {
			return err
		}
		if target.Status == RevStaleOut {
			return fmt.Errorf("%w: rev %d was written against a superseded outline", ErrInvalidRevision, targetRevNo)
		}
		states, err := evidenceStatesTx(ctx, tx, tenant, taskID, target.EvidenceIDs)
		if err != nil {
			return err
		}
		var keep []int64
		var dropped []RevisionGap
		for _, id := range target.EvidenceIDs {
			if e, ok := states[id]; ok && e.Status != "stale" {
				keep = append(keep, id)
			} else {
				dropped = append(dropped, RevisionGap{EvidenceID: id, Kind: "stale", Note: "evidence invalidated; removed on rollback"})
			}
		}
		auto, _ := evidenceGaps(states, keep)
		var userGaps []RevisionGap
		for _, g := range target.Gaps {
			if g.EvidenceID == 0 {
				userGaps = append(userGaps, g)
			}
		}
		gaps := mergeGaps(userGaps, append(auto, dropped...))
		commitIDs, droppedCommits, err := currentCommitmentIDsTx(ctx, tx, tenant, taskID, target.CommitmentIDs)
		if err != nil {
			return err
		}
		for _, id := range droppedCommits {
			gaps = append(gaps, RevisionGap{Kind: "commitment_stale", Note: fmt.Sprintf("commitment %d is not confirmed; removed on rollback", id)})
		}
		itemIDs, droppedItems, err := currentAdmissibilityItemIDsTx(ctx, tx, tenant, taskID, target.ID)
		if err != nil {
			return err
		}
		for _, id := range droppedItems {
			gaps = append(gaps, RevisionGap{Kind: "admissibility_stale", Note: fmt.Sprintf("admissibility item %d is not current admissible; removed on rollback", id)})
		}
		in := SaveRevisionInput{TaskID: taskID, SectionID: s.ID, BaseRevNo: s.CurrentRevNo, BasedOnOutlineVersion: approved.Version,
			Source: RevRollback, ContentMD: target.ContentMD, SummaryNote: target.SummaryNote,
			DiffSummary: fmt.Sprintf("回滚至 rev %d", targetRevNo), EvidenceIDs: uniqueIDs(keep),
			CommitmentIDs: commitIDs, AdmissibilityItemIDs: itemIDs}
		rb := targetRevNo
		rev, err := insertRevisionTx(ctx, tx, tenant, s, in, &rb, gaps, RevDraft, actor)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE proposal_sections SET current_rev_no = $2, approved_rev_no = NULL, approved_by = '',
			status = 'draft', updated_by = $3 WHERE id = $1`, s.ID, rev.RevNo, actor); err != nil {
			return err
		}
		if err := refreshReadinessTx(ctx, tx, tenant, s.OutlineID); err != nil {
			return err
		}
		cur, err := sectionByIDTx(ctx, tx, tenant, s.ID)
		out = &SaveRevisionResult{Outcome: SaveApplied, Revision: rev, Section: cur}
		return err
	})
	return out, err
}

// ---------------------------------------------------------------------------
// 读取：章节详情与写作上下文
// ---------------------------------------------------------------------------

type DependencyState struct {
	ID           int64  `json:"id"`
	Code         string `json:"section_code"`
	Title        string `json:"title"`
	CurrentRevNo int    `json:"current_rev_no"`
	Status       string `json:"status"`
	Ready        bool   `json:"ready"`
	SummaryNote  string `json:"summary_note,omitempty"`
}

type SectionDetail struct {
	Section             *ProposalSection            `json:"section"`
	Outline             *ProposalOutline            `json:"outline"`
	ApprovedVersion     int                         `json:"approved_version"`
	Writable            bool                        `json:"writable"`
	BlockedReason       string                      `json:"blocked_reason,omitempty"`
	Requirements        []*RequirementItem          `json:"requirements"`
	Dependencies        []DependencyState           `json:"dependencies"`
	Current             *SectionRevision            `json:"current_revision,omitempty"`
	Revisions           []*SectionRevision          `json:"revisions"`
	Evidence            []EvidenceState             `json:"evidence"`
	Warnings            []string                    `json:"warnings"`
	Storyboard          *SectionStoryboard          `json:"storyboard,omitempty"`
	WritingVerification *SectionWritingVerification `json:"writing_verification,omitempty"`
}

// GetSection 返回章节详情、前置依赖就绪状态、全部修订历史与当前修订证据的实时状态（单一快照读取）。
func (d *DB) GetSection(ctx context.Context, tenant string, taskID, sectionID int64) (*SectionDetail, error) {
	return d.getSection(ctx, tenant, taskID, sectionID, true)
}

// GetSectionForWriting 返回写作所需的章节详情和当前主修订。调用者按自身上下文读取证据状态。
func (d *DB) GetSectionForWriting(ctx context.Context, tenant string, taskID, sectionID int64) (*SectionDetail, error) {
	if d.tx != nil {
		return nil, errors.New("cannot read section writing snapshot inside a transaction")
	}
	return d.getSection(ctx, tenant, taskID, sectionID, false)
}

func (d *DB) getSection(ctx context.Context, tenant string, taskID, sectionID int64, includeHistory bool) (*SectionDetail, error) {
	if err := CheckSectionScope(ctx, sectionID); err != nil {
		return nil, err
	}
	var det *SectionDetail
	err := d.readSnapshot(ctx, func(tx *sql.Tx) error {
		var err error
		det, err = getSectionTx(ctx, tx, tenantOr(tenant), taskID, sectionID, includeHistory)
		return err
	})
	return det, err
}

func getSectionTx(ctx context.Context, q queryer, tenant string, taskID, sectionID int64, includeHistory bool) (*SectionDetail, error) {
	s, err := scanSection(q.QueryRowContext(ctx, `SELECT `+sectionCols+` FROM proposal_sections s WHERE s.tenant_id = $1 AND s.task_id = $2 AND s.id = $3`,
		tenant, taskID, sectionID))
	if err != nil {
		return nil, err
	}
	o, err := scanOutline(q.QueryRowContext(ctx, `SELECT `+outlineCols+` FROM proposal_outlines WHERE id = $1`, s.OutlineID))
	if err != nil {
		return nil, err
	}
	det := &SectionDetail{Section: s, Outline: o, Requirements: []*RequirementItem{}, Dependencies: []DependencyState{},
		Revisions: []*SectionRevision{}, Evidence: []EvidenceState{}, Warnings: []string{}}
	if err := q.QueryRowContext(ctx, `SELECT COALESCE((SELECT version FROM proposal_outlines WHERE tenant_id = $1 AND task_id = $2 AND status = 'approved'), 0)`,
		tenant, taskID).Scan(&det.ApprovedVersion); err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT `+reqCols+` FROM requirement_items WHERE tenant_id = $1 AND id IN
		(SELECT requirement_id FROM proposal_section_requirements WHERE tenant_id = $1 AND section_id = $2) ORDER BY code`, tenant, s.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		it, err := scanRequirement(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		s.RequirementIDs = append(s.RequirementIDs, it.ID)
		det.Requirements = append(det.Requirements, it)
	}
	rows.Close()
	if len(s.DependsOn) > 0 {
		drows, err := q.QueryContext(ctx, `SELECT p.id, p.section_code, p.title, p.current_rev_no, p.status, COALESCE(r.summary_note, '')
			FROM proposal_sections p LEFT JOIN section_revisions r ON r.tenant_id = p.tenant_id AND r.section_id = p.id AND r.rev_no = p.current_rev_no
			WHERE p.tenant_id = $1 AND p.id = ANY($2::bigint[]) ORDER BY p.seq`, tenant, pgInt64Array(s.DependsOn))
		if err != nil {
			return nil, err
		}
		for drows.Next() {
			var dep DependencyState
			if err := drows.Scan(&dep.ID, &dep.Code, &dep.Title, &dep.CurrentRevNo, &dep.Status, &dep.SummaryNote); err != nil {
				drows.Close()
				return nil, err
			}
			dep.Ready = dep.CurrentRevNo > 0
			s.DependsOnCodes = append(s.DependsOnCodes, dep.Code)
			if !dep.Ready {
				s.UnmetDeps = append(s.UnmetDeps, dep.Code)
			}
			det.Dependencies = append(det.Dependencies, dep)
		}
		drows.Close()
	}
	s.DepsReady = len(s.UnmetDeps) == 0
	const revisionQuery = `SELECT ` + revisionCols + ` FROM section_revisions WHERE tenant_id = $1 AND section_id = $2`
	if includeHistory {
		rrows, err := q.QueryContext(ctx, revisionQuery+` ORDER BY rev_no DESC`, tenant, s.ID)
		if err != nil {
			return nil, err
		}
		for rrows.Next() {
			rev, err := scanRevision(rrows)
			if err != nil {
				rrows.Close()
				return nil, err
			}
			det.Revisions = append(det.Revisions, rev)
			if rev.RevNo == s.CurrentRevNo {
				det.Current = rev
			}
		}
		rrows.Close()
		if err := rrows.Err(); err != nil {
			return nil, err
		}
	} else if s.CurrentRevNo > 0 {
		det.Current, err = scanRevision(q.QueryRowContext(ctx, revisionQuery+` AND rev_no = $3`, tenant, s.ID, s.CurrentRevNo))
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	switch {
	case o.Status == OutlineDraft || o.Status == OutlinePending:
		det.BlockedReason = fmt.Sprintf("大纲 v%d 处于 %s，未批准前不可写作", o.Version, o.Status)
	case o.Status == OutlineSuperseded:
		det.BlockedReason = fmt.Sprintf("大纲 v%d 已被取代（当前批准 v%d）", o.Version, det.ApprovedVersion)
	case !s.DepsReady:
		det.BlockedReason = "前置章节尚无有效稿：" + strings.Join(s.UnmetDeps, ", ")
	}
	det.Writable = o.Status == OutlineApproved
	if includeHistory && det.Current != nil {
		det.Evidence, err = evidenceStateListTx(ctx, q, tenant, taskID, det.Current.EvidenceIDs)
		if err != nil {
			return nil, err
		}
		for _, e := range det.Evidence {
			switch {
			case e.Status == "stale":
				det.Warnings = append(det.Warnings, fmt.Sprintf("证据 %d 已失效：%s", e.ID, e.Reason))
			case e.Relation == "contradicts":
				det.Warnings = append(det.Warnings, fmt.Sprintf("证据 %d 与主张冲突：%s", e.ID, e.Reason))
			case !e.Usable:
				det.Warnings = append(det.Warnings, fmt.Sprintf("证据 %d 未核验（%s/%s），不得写成已证实承诺", e.ID, e.Status, e.Relation))
			}
		}
	}
	if det.Current != nil {
		if wv, err := scanWritingVerification(q.QueryRowContext(ctx, `SELECT `+writingVerifCols+writingVerifJoin+
			`WHERE wv.tenant_id = $1 AND wv.section_rev_id = $2`, tenant, det.Current.ID)); err == nil {
			det.WritingVerification = wv
		}
	}
	if sb, err := currentStoryboardTx(ctx, q, tenant, taskID, s.ID, false); err == nil {
		det.Storyboard = sb
	}
	return det, nil
}

// ListSectionRevisions 返回章节修订历史（新到旧）。
func (d *DB) ListSectionRevisions(ctx context.Context, tenant string, taskID, sectionID int64) ([]*SectionRevision, error) {
	det, err := d.GetSection(ctx, tenant, taskID, sectionID)
	if err != nil {
		return nil, err
	}
	return det.Revisions, nil
}

// SectionByCode 返回任务当前批准大纲中指定编码的章节 ID（供工具按编码定位章节）。
func (d *DB) SectionByCode(ctx context.Context, tenant string, taskID int64, code string) (int64, error) {
	var id int64
	err := d.QueryRowContext(ctx, `SELECT s.id FROM proposal_sections s JOIN proposal_outlines o ON o.id = s.outline_id
		WHERE s.tenant_id = $1 AND s.task_id = $2 AND o.status = 'approved' AND s.section_code = $3`, tenantOr(tenant), taskID, code).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

// RequirementSource 是要求绑定的招标原文摘录（写作上下文使用）。
type RequirementSource struct {
	RequirementID int64           `json:"requirement_id"`
	ChunkID       int64           `json:"chunk_id"`
	Excerpt       string          `json:"excerpt"`
	CurrentHash   string          `json:"current_hash"`
	SourceChanged bool            `json:"source_changed"`
	Loc           json.RawMessage `json:"source_loc"`
}

// RequirementSources 读取要求绑定原文分块的当前内容，并标出原文是否已在绑定后变化。
func (d *DB) RequirementSources(ctx context.Context, tenant string, taskID int64, items []*RequirementItem, maxRunes int) ([]RequirementSource, error) {
	tenant = tenantOr(tenant)
	out := []RequirementSource{}
	for _, it := range items {
		if it.SourceChunkID == nil {
			continue
		}
		cs, err := chunkSourceTx(ctx, d, tenant, taskID, *it.SourceChunkID)
		if errors.Is(err, ErrInvalidRequirement) {
			continue
		}
		if err != nil {
			return nil, err
		}
		r := []rune(cs.Content)
		if maxRunes > 0 && len(r) > maxRunes {
			r = append(r[:maxRunes], '…')
		}
		out = append(out, RequirementSource{RequirementID: it.ID, ChunkID: cs.ChunkID, Excerpt: string(r), CurrentHash: cs.Hash,
			SourceChanged: it.SourceHash != "" && it.SourceHash != cs.Hash, Loc: it.SourceLoc})
	}
	return out, nil
}
