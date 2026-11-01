package db

// D2 可采性运行与逐证据明细：加载要求、绑定证据与原件，调用 compliance.Evaluate 纯规则，
// 在同一事务内迭代版本并使旧运行 superseded。manual 运行必须经 ResolveAdmissibility 生成新版本。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/compliance"
)

// OriginalChecker 由 evidence.Store.Inspect 实现；可采性纯规则不直接访问文件系统。
type OriginalChecker interface {
	Inspect(rel, wantHash string) (exists, readable, match bool, err error)
}

// AdmissibilityItem 是运行内逐证据结论。
type AdmissibilityItem struct {
	ID             int64                 `json:"id"`
	TenantID       string                `json:"tenant_id"`
	RunID          int64                 `json:"run_id"`
	BindingID      int64                 `json:"binding_id"`
	EvidenceDomain string                `json:"evidence_domain"`
	EvidenceID     int64                 `json:"evidence_id"`
	Verdict        string                `json:"verdict"`
	Checks         []compliance.AdmissibilityCheck    `json:"checks"`
	Reasons        []compliance.AdmissibilityReason   `json:"reasons"`
}

// AdmissibilityRun 是一次要求级聚合评估。
type AdmissibilityRun struct {
	ID                 int64               `json:"id"`
	TenantID           string              `json:"tenant_id"`
	TaskID             int64               `json:"task_id"`
	RequirementID      int64               `json:"requirement_item_id"`
	RequirementCode    string              `json:"requirement_code"`
	RequirementTitle   string              `json:"requirement_title"`
	RuleVersion        string              `json:"rule_version"`
	BidDeadline        *time.Time          `json:"bid_deadline,omitempty"`
	SourceFingerprint  string              `json:"source_fingerprint"`
	Verdict            string              `json:"verdict"`
	Checks             []compliance.AdmissibilityCheck  `json:"checks"`
	Reasons            []compliance.AdmissibilityReason `json:"reasons"`
	Status             string              `json:"status"`
	EvaluatedBy        string              `json:"evaluated_by"`
	EvaluatedAt        time.Time           `json:"evaluated_at"`
	Version            int                 `json:"version"`
	Items              []*AdmissibilityItem `json:"items,omitempty"`
}

const admissibilityRunCols = `r.id, r.tenant_id, r.task_id, r.requirement_id, req.code, req.title, r.rule_version, r.bid_deadline,
	r.source_fingerprint, r.verdict, r.checks, r.reasons, r.status, r.evaluated_by, r.evaluated_at, r.version`

func scanAdmissibilityRun(r rowScanner) (*AdmissibilityRun, error) {
	var out AdmissibilityRun
	var deadline sql.NullTime
	var checks, reasons []byte
	err := r.Scan(&out.ID, &out.TenantID, &out.TaskID, &out.RequirementID, &out.RequirementCode, &out.RequirementTitle,
		&out.RuleVersion, &deadline, &out.SourceFingerprint, &out.Verdict, &checks, &reasons, &out.Status, &out.EvaluatedBy,
		&out.EvaluatedAt, &out.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	out.BidDeadline = nullTimePtr(deadline)
	out.Checks = []compliance.AdmissibilityCheck{}
	out.Reasons = []compliance.AdmissibilityReason{}
	_ = json.Unmarshal(checks, &out.Checks)
	_ = json.Unmarshal(reasons, &out.Reasons)
	if out.Checks == nil {
		out.Checks = []compliance.AdmissibilityCheck{}
	}
	if out.Reasons == nil {
		out.Reasons = []compliance.AdmissibilityReason{}
	}
	return &out, nil
}

func loadAdmissibilityItemsTx(ctx context.Context, q queryer, tenant string, runID int64) ([]*AdmissibilityItem, error) {
	rows, err := q.QueryContext(ctx, `SELECT i.id, i.tenant_id, i.run_id, i.binding_id, b.evidence_domain, b.evidence_id,
			i.verdict, i.checks, i.reasons
		FROM admissibility_items i JOIN requirement_evidence_bindings b ON b.tenant_id=i.tenant_id AND b.id=i.binding_id
		WHERE i.tenant_id=$1 AND i.run_id=$2 ORDER BY b.evidence_domain, b.evidence_id`, tenant, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AdmissibilityItem
	for rows.Next() {
		var it AdmissibilityItem
		var checks, reasons []byte
		if err := rows.Scan(&it.ID, &it.TenantID, &it.RunID, &it.BindingID, &it.EvidenceDomain, &it.EvidenceID, &it.Verdict,
			&checks, &reasons); err != nil {
			return nil, err
		}
		it.Checks, it.Reasons = []compliance.AdmissibilityCheck{}, []compliance.AdmissibilityReason{}
		_ = json.Unmarshal(checks, &it.Checks)
		_ = json.Unmarshal(reasons, &it.Reasons)
		if it.Checks == nil {
			it.Checks = []compliance.AdmissibilityCheck{}
		}
		if it.Reasons == nil {
			it.Reasons = []compliance.AdmissibilityReason{}
		}
		out = append(out, &it)
	}
	return out, rows.Err()
}

func runWithItemsTx(ctx context.Context, q queryer, tenant string, id int64) (*AdmissibilityRun, error) {
	run, err := scanAdmissibilityRun(q.QueryRowContext(ctx, `SELECT `+admissibilityRunCols+`
		FROM admissibility_runs r JOIN requirement_items req
		  ON req.tenant_id=r.tenant_id AND req.task_id=r.task_id AND req.id=r.requirement_id
		WHERE r.tenant_id=$1 AND r.id=$2`, tenant, id))
	if err != nil {
		return nil, err
	}
	items, err := loadAdmissibilityItemsTx(ctx, q, tenant, id)
	if err != nil {
		return nil, err
	}
	run.Items = items
	return run, nil
}

// CurrentAdmissibilityRun 返回要求当前运行（含明细）；不存在返回 ErrNotFound。
func (d *DB) CurrentAdmissibilityRun(ctx context.Context, tenant string, taskID, requirementID int64) (*AdmissibilityRun, error) {
	tenant = tenantOr(tenant)
	var id int64
	err := d.QueryRowContext(ctx, `SELECT id FROM admissibility_runs WHERE tenant_id=$1 AND task_id=$2 AND requirement_id=$3 AND status='current'`,
		tenant, taskID, requirementID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return runWithItemsTx(ctx, d, tenant, id)
}

// ListAdmissibilityRuns 列出任务的可采性运行（按版本倒序）。
func (d *DB) ListAdmissibilityRuns(ctx context.Context, tenant string, taskID, requirementID int64, limit int) ([]*AdmissibilityRun, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := d.QueryContext(ctx, `SELECT `+admissibilityRunCols+`
		FROM admissibility_runs r JOIN requirement_items req
		  ON req.tenant_id=r.tenant_id AND req.task_id=r.task_id AND req.id=r.requirement_id
		WHERE r.tenant_id=$1 AND r.task_id=$2 AND ($3::bigint=0 OR r.requirement_id=$3::bigint)
		ORDER BY r.requirement_id, r.version DESC LIMIT $4`, tenantOr(tenant), taskID, requirementID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AdmissibilityRun
	for rows.Next() {
		r, err := scanAdmissibilityRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range out {
		items, err := loadAdmissibilityItemsTx(ctx, d, tenantOr(tenant), r.ID)
		if err != nil {
			return nil, err
		}
		r.Items = items
	}
	return out, nil
}

// evidenceLoad 是加载后的单条证据及其指纹输入。
type evidenceLoad struct {
	Evidence compliance.AdmissibilityEvidence
	Finger   map[string]any
}

type fingerprintInput struct {
	RuleVersion   string           `json:"rule_version"`
	RequirementID int64            `json:"requirement_id"`
	Code          string           `json:"code"`
	Title         string           `json:"title"`
	Description   string           `json:"description"`
	ProofRules    ProofRules       `json:"proof_rules"`
	BidDeadline   string           `json:"bid_deadline"`
	BidderName    string           `json:"bidder_name"`
	Evidence      []map[string]any `json:"evidence"`
}

func loadEvidenceForRequirementTx(ctx context.Context, q queryer, tenant string, taskID, requirementID int64,
	checker OriginalChecker) ([]evidenceLoad, string, error) {
	bindings, err := listBindingsTx(ctx, q, tenant, taskID, requirementID)
	if err != nil {
		return nil, "", err
	}
	var out []evidenceLoad
	for _, b := range bindings {
		ev := compliance.AdmissibilityEvidence{Domain: b.Domain, ID: b.EvidenceID, Attributes: map[string]any{}, Originals: []compliance.AdmissibilityOriginal{}}
		fp := map[string]any{"domain": b.Domain, "id": b.EvidenceID}
		switch b.Domain {
		case EvidenceDomainCredential:
			c, err := scanCredential(q.QueryRowContext(ctx, `SELECT `+credentialCols+` FROM company_credentials WHERE tenant_id=$1 AND id=$2`,
				tenant, b.EvidenceID))
			if errors.Is(err, sql.ErrNoRows) {
				ev.Deleted = true
				fp["missing"] = true
			} else if err != nil {
				return nil, "", err
			} else {
				ev.Category, ev.Code, ev.Title = c.Category, c.Code, c.Title
				ev.SubjectName, ev.SubjectType = c.SubjectName, c.SubjectType
				ev.CertificateNo, ev.Issuer, ev.Scope = c.CertificateNo, c.Issuer, c.Scope
				ev.ValidUntil = dateStringPtr(c.ValidUntil)
				ev.IssuedOn = dateStringPtr(c.IssuedOn)
				ev.Deleted = c.DeletedAt != nil
				_ = json.Unmarshal(c.Attributes, &ev.Attributes)
				fp["credential"] = c
			}
		case EvidenceDomainPerformance:
			p, err := scanPerformance(q.QueryRowContext(ctx, `SELECT `+performanceCols+` FROM company_performance WHERE tenant_id=$1 AND id=$2`,
				tenant, b.EvidenceID))
			if errors.Is(err, sql.ErrNoRows) {
				ev.Deleted = true
				fp["missing"] = true
			} else if err != nil {
				return nil, "", err
			} else {
				ev.Category, ev.Code, ev.Title = "performance", p.Code, p.ProjectName
				ev.SubjectName, ev.SubjectType = p.SubjectName, p.SubjectType
				ev.Scope = p.Scope
				ev.ContractDate = dateStringPtr(p.ContractDate)
				ev.AcceptanceDate = dateStringPtr(p.AcceptanceDate)
				ev.Amount = p.Amount
				ev.IsSubsidiary = p.IsSubsidiary
				ev.Deleted = p.DeletedAt != nil
				_ = json.Unmarshal(p.Attributes, &ev.Attributes)
				fp["performance"] = p
			}
		}
		originals, err := loadOriginalsTx(ctx, q, tenant, b.Domain, b.EvidenceID)
		if err != nil {
			return nil, "", err
		}
		fpOriginals := []map[string]any{}
		for _, o := range originals {
			exists, readable, match := false, false, false
			if checker != nil && !o.Superseded() {
				var err error
				exists, readable, match, err = checker.Inspect(o.OriginalPath, o.OriginalSHA256)
				if err != nil {
					exists, readable, match = false, false, false
				}
			}
			ev.Originals = append(ev.Originals, compliance.AdmissibilityOriginal{
				DocumentKind: o.DocumentKind, PageLabel: o.PageLabel, PageStart: o.PageStart, PageEnd: o.PageEnd,
				SHA256: o.OriginalSHA256, Present: true, Exists: exists, Readable: readable, HashMatch: match,
				Superseded: o.SupersededAt != nil,
			})
			fpOriginals = append(fpOriginals, map[string]any{
				"id": o.ID, "kind": o.DocumentKind, "label": o.PageLabel, "path": o.OriginalPath, "sha256": o.OriginalSHA256,
				"start": o.PageStart, "end": o.PageEnd, "superseded": o.SupersededAt != nil,
			})
		}
		fp["originals"] = fpOriginals
		out = append(out, evidenceLoad{Evidence: ev, Finger: fp})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Evidence.Domain != out[j].Evidence.Domain {
			return out[i].Evidence.Domain < out[j].Evidence.Domain
		}
		return out[i].Evidence.ID < out[j].Evidence.ID
	})
	fps := make([]map[string]any, 0, len(out))
	for _, e := range out {
		fps = append(fps, e.Finger)
	}
	return out, EvidenceSourceHash(fps), nil
}

func dateStringPtr(s *string) *time.Time {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil
	}
	t, err := time.Parse(time.DateOnly, strings.TrimSpace(*s))
	if err != nil {
		return nil
	}
	return &t
}

func listBindingsTx(ctx context.Context, q queryer, tenant string, taskID, requirementID int64) ([]*RequirementEvidenceBinding, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, tenant_id, task_id, requirement_id, evidence_domain, evidence_id, source_hash, created_by, created_at
		FROM requirement_evidence_bindings WHERE tenant_id=$1 AND task_id=$2 AND requirement_id=$3 ORDER BY evidence_domain, evidence_id`,
		tenant, taskID, requirementID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*RequirementEvidenceBinding
	for rows.Next() {
		var b RequirementEvidenceBinding
		if err := rows.Scan(&b.ID, &b.TenantID, &b.TaskID, &b.RequirementID, &b.Domain, &b.EvidenceID, &b.SourceHash, &b.CreatedBy, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &b)
	}
	return out, rows.Err()
}

func (o *CompanyEvidenceOriginal) Superseded() bool { return o.SupersededAt != nil }

func loadOriginalsTx(ctx context.Context, q queryer, tenant, domain string, ownerID int64) ([]*CompanyEvidenceOriginal, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+originalCols+` FROM company_evidence_originals
		WHERE tenant_id=$1 AND owner_domain=$2 AND owner_id=$3 ORDER BY document_kind, page_start, id`, tenant, domain, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CompanyEvidenceOriginal
	for rows.Next() {
		o, err := scanOriginal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func runVerdict(items []*AdmissibilityItem, min int) string {
	if min <= 0 {
		min = 1
	}
	admissible, manual := 0, 0
	for _, it := range items {
		switch it.Verdict {
		case compliance.Admissible:
			admissible++
		case compliance.Manual:
			manual++
		}
	}
	switch {
	case manual > 0:
		return compliance.Manual
	case admissible < min:
		return compliance.Inadmissible
	}
	return compliance.Admissible
}

// EvaluateAdmissibility 加载要求与绑定证据，按纯规则计算并持久化新运行（相同指纹幂等返回当前运行）。
func (d *DB) EvaluateAdmissibility(ctx context.Context, tenant string, taskID, requirementID int64, checker OriginalChecker, actor string) (*AdmissibilityRun, error) {
	tenant = tenantOr(tenant)
	bidder, err := d.CompanyLegalName(ctx, tenant)
	if err != nil {
		return nil, err
	}
	var out *AdmissibilityRun
	err = d.InTx(ctx, func(tx *sql.Tx) error {
		req, err := scanRequirement(tx.QueryRowContext(ctx, `SELECT `+reqCols+` FROM requirement_items
			WHERE tenant_id=$1 AND task_id=$2 AND id=$3 FOR UPDATE`, tenant, taskID, requirementID))
		if err != nil {
			return err
		}
		m, err := ensureManifestTx(ctx, tx, tenant, taskID, false)
		if err != nil {
			return err
		}
		if m.BidDeadline == nil {
			return ErrBidDeadlineMissing
		}
		loads, evidenceHash, err := loadEvidenceForRequirementTx(ctx, tx, tenant, taskID, requirementID, checker)
		if err != nil {
			return err
		}
		evidence := make([]compliance.AdmissibilityEvidence, 0, len(loads))
		for _, l := range loads {
			evidence = append(evidence, l.Evidence)
		}
		fp := EvidenceSourceHash(fingerprintInput{
			RuleVersion: compliance.AdmissibilityRuleVersion, RequirementID: req.ID, Code: req.Code, Title: req.Title,
			Description: req.Description, ProofRules: req.ProofRules,
			BidDeadline: m.BidDeadline.In(beijing).Format(time.RFC3339),
			BidderName: bidder, Evidence: loadsFinger(loads),
		}) + "|" + evidenceHash
		var currentID int64
		var currentFP string
		err = tx.QueryRowContext(ctx, `SELECT id, source_fingerprint FROM admissibility_runs
			WHERE tenant_id=$1 AND task_id=$2 AND requirement_id=$3 AND status='current'`, tenant, taskID, requirementID).Scan(&currentID, &currentFP)
		if err == nil && currentFP == fp {
			out, err = runWithItemsTx(ctx, tx, tenant, currentID)
			return err
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if _, err := tx.ExecContext(ctx, `UPDATE admissibility_runs SET status='superseded', updated_by=$4
				WHERE tenant_id=$1 AND task_id=$2 AND requirement_id=$3 AND status='current'`, tenant, taskID, requirementID, actor); err != nil {
				return err
			}
		}
		var version int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM admissibility_runs
			WHERE tenant_id=$1 AND task_id=$2 AND requirement_id=$3`, tenant, taskID, requirementID).Scan(&version); err != nil {
			return err
		}
		res := compliance.Evaluate(compliance.AdmissibilityInput{
			Requirement: compliance.AdmissibilityRequirement{Code: req.Code, Category: req.Category, Title: req.Title, Description: req.Description,
				ProofRules: toRuleProof(req.ProofRules)},
			Evidence: evidence, BidDeadline: *m.BidDeadline, BidderName: bidder,
		})
		checksJSON, _ := json.Marshal(res.Checks)
		reasonsJSON, _ := json.Marshal(res.Reasons)
		var runID int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO admissibility_runs(tenant_id, task_id, requirement_id, rule_version, bid_deadline,
				source_fingerprint, verdict, checks, reasons, status, evaluated_by, evaluated_at, version, created_by, updated_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9::jsonb,'current',COALESCE(NULLIF($10,''),''),now(),$11,COALESCE(NULLIF($10,''),''),COALESCE(NULLIF($10,''),''))
			RETURNING id`, tenant, taskID, requirementID, res.RuleVersion, *m.BidDeadline, fp, res.Verdict, string(checksJSON),
			string(reasonsJSON), actor, version).Scan(&runID); err != nil {
			return err
		}
		if err := insertAdmissibilityItemsTx(ctx, tx, tenant, runID, req, loads, res, actor); err != nil {
			return err
		}
		out, err = runWithItemsTx(ctx, tx, tenant, runID)
		return err
	})
	return out, err
}

func loadsFinger(loads []evidenceLoad) []map[string]any {
	out := make([]map[string]any, 0, len(loads))
	for _, l := range loads {
		out = append(out, l.Finger)
	}
	return out
}

func insertAdmissibilityItemsTx(ctx context.Context, tx *sql.Tx, tenant string, runID int64, req *RequirementItem,
	loads []evidenceLoad, res compliance.AdmissibilityResult, actor string) error {
	bindings, err := listBindingsTx(ctx, tx, tenant, req.TaskID, req.ID)
	if err != nil {
		return err
	}
	byEvidence := map[string]int64{}
	for _, b := range bindings {
		byEvidence[b.Domain+"|"+fmt.Sprint(b.EvidenceID)] = b.ID
	}
	for _, item := range res.Items {
		bindingID := byEvidence[item.Domain+"|"+fmt.Sprint(item.EvidenceID)]
		if bindingID == 0 {
			continue
		}
		checks, _ := json.Marshal(item.Checks)
		reasons, _ := json.Marshal(item.Reasons)
		if _, err := tx.ExecContext(ctx, `INSERT INTO admissibility_items(tenant_id, run_id, binding_id, verdict, checks, reasons, created_by, updated_by)
			VALUES ($1,$2,$3,$4,$5::jsonb,$6::jsonb,COALESCE(NULLIF($7,''),''),COALESCE(NULLIF($7,''),''))`,
			tenant, runID, bindingID, item.Verdict, string(checks), string(reasons), actor); err != nil {
			return fmt.Errorf("insert admissibility item: %w", err)
		}
	}
	return nil
}

func toRuleProof(p ProofRules) compliance.AdmissibilityProofRules {
	return compliance.AdmissibilityProofRules{
		RequiredDocs: p.RequiredDocs, ContractPages: p.ContractPages, NeedAcceptance: p.NeedAcceptance,
		DateAnchor: p.DateAnchor, YearsWindow: p.YearsWindow, SinceDate: p.SinceDate,
		ValidOnBidDeadline: p.ValidOnBidDeadline, SocialSecurityMonths: p.SocialSecurityMonths,
		BidderOnly: p.BidderOnly, MinCount: p.MinCount, ModelParams: p.ModelParams,
		ScopeKeywords: p.ScopeKeywords, RawSummary: p.RawSummary,
	}
}

// AdmissibilityDecision 是人工复核结论；必须填写理由与操作人。
type AdmissibilityDecision struct {
	ItemID  int64  `json:"item_id"`
	Verdict string `json:"verdict"` // admissible | inadmissible
	Reason  string `json:"reason"`
}

// ResolveAdmissibility 对 manual 明细提交人工结论并生成新版本运行；旧运行保留为 superseded。
func (d *DB) ResolveAdmissibility(ctx context.Context, tenant string, taskID, runID int64, decisions []AdmissibilityDecision, actor string) (*AdmissibilityRun, error) {
	tenant = tenantOr(tenant)
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return nil, errors.New("d2: resolver actor is required")
	}
	if len(decisions) == 0 {
		return nil, errors.New("d2: at least one manual decision is required")
	}
	seen := map[int64]bool{}
	for _, dec := range decisions {
		if dec.Verdict != compliance.Admissible && dec.Verdict != compliance.Inadmissible {
			return nil, fmt.Errorf("d2: decision verdict must be admissible or inadmissible")
		}
		if strings.TrimSpace(dec.Reason) == "" {
			return nil, errors.New("d2: decision reason is required")
		}
		seen[dec.ItemID] = true
	}
	var out *AdmissibilityRun
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		run, err := scanAdmissibilityRun(tx.QueryRowContext(ctx, `SELECT `+admissibilityRunCols+`
			FROM admissibility_runs r JOIN requirement_items req
			  ON req.tenant_id=r.tenant_id AND req.task_id=r.task_id AND req.id=r.requirement_id
			WHERE r.tenant_id=$1 AND r.task_id=$2 AND r.id=$3 FOR UPDATE`, tenant, taskID, runID))
		if err != nil {
			return err
		}
		if run.Status != "current" {
			return fmt.Errorf("%w: run %d is %s", ErrAdmissibilityStale, runID, run.Status)
		}
		items, err := loadAdmissibilityItemsTx(ctx, tx, tenant, runID)
		if err != nil {
			return err
		}
		manual := map[int64]*AdmissibilityItem{}
		for _, it := range items {
			if it.Verdict == compliance.Manual {
				manual[it.ID] = it
			}
		}
		for id := range seen {
			if manual[id] == nil {
				return fmt.Errorf("d2: item %d is not a manual item of run %d", id, runID)
			}
		}
		if len(seen) != len(manual) {
			return errors.New("d2: every manual item requires a decision")
		}
		req, err := scanRequirement(tx.QueryRowContext(ctx, `SELECT `+reqCols+` FROM requirement_items
			WHERE tenant_id=$1 AND task_id=$2 AND id=$3`, tenant, taskID, run.RequirementID))
		if err != nil {
			return err
		}
		decByID := map[int64]AdmissibilityDecision{}
		for _, dec := range decisions {
			decByID[dec.ItemID] = dec
		}
		if _, err := tx.ExecContext(ctx, `UPDATE admissibility_runs SET status='superseded', updated_by=$4
			WHERE tenant_id=$1 AND task_id=$2 AND requirement_id=$3 AND status='current'`, tenant, taskID, run.RequirementID, actor); err != nil {
			return err
		}
		var version int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM admissibility_runs
			WHERE tenant_id=$1 AND task_id=$2 AND requirement_id=$3`, tenant, taskID, run.RequirementID).Scan(&version); err != nil {
			return err
		}
		fpBase := run.SourceFingerprint
		decKeys := make([]int64, 0, len(decByID))
		for id := range decByID {
			decKeys = append(decKeys, id)
		}
		sort.Slice(decKeys, func(i, j int) bool { return decKeys[i] < decKeys[j] })
		var b strings.Builder
		for _, id := range decKeys {
			fmt.Fprintf(&b, "%d:%s:%s;", id, decByID[id].Verdict, decByID[id].Reason)
		}
		sum := sha256.Sum256([]byte(b.String() + "|" + actor))
		fp := fpBase + "|manual-review|" + hex.EncodeToString(sum[:])
		verdict := runVerdictForItems(items, decByID, req.ProofRules.MinCount)
		min := req.ProofRules.MinCount
		if min <= 0 {
			min = 1
		}
		admissible := 0
		for _, it := range items {
			v := it.Verdict
			if dec, ok := decByID[it.ID]; ok {
				v = dec.Verdict
			}
			if v == compliance.Admissible {
				admissible++
			}
		}
		newChecks := []compliance.AdmissibilityCheck{}
		for _, c := range run.Checks {
			if c.Rule != "min_count" {
				newChecks = append(newChecks, c)
			}
		}
		minStatus := compliance.CheckPass
		if admissible < min {
			minStatus = compliance.CheckFail
		}
		newChecks = append(newChecks, compliance.AdmissibilityCheck{Rule: "min_count", Status: minStatus, Expected: fmt.Sprint(min),
			Actual: fmt.Sprintf("admissible=%d", admissible)})
		newChecks = append(newChecks, compliance.AdmissibilityCheck{Rule: "manual_review", Status: compliance.CheckPass,
			Expected: fmt.Sprintf("%d manual items", len(manual)), Actual: fmt.Sprintf("%d resolved", len(seen)), Detail: actor})
		checksJSON, _ := json.Marshal(newChecks)
		reasonsJSON, _ := json.Marshal(run.Reasons)
		var newID int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO admissibility_runs(tenant_id, task_id, requirement_id, rule_version, bid_deadline,
				source_fingerprint, verdict, checks, reasons, status, evaluated_by, evaluated_at, version, created_by, updated_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9::jsonb,'current',$10,now(),$11,$10,$10) RETURNING id`,
			tenant, taskID, run.RequirementID, run.RuleVersion, run.BidDeadline, fp, verdict, string(checksJSON), string(reasonsJSON),
			actor, version).Scan(&newID); err != nil {
			return err
		}
		for _, it := range items {
			v := it.Verdict
			checks, reasons := it.Checks, it.Reasons
			if dec, ok := decByID[it.ID]; ok {
				v = dec.Verdict
				checks = append(append([]compliance.AdmissibilityCheck{}, checks...), compliance.AdmissibilityCheck{Rule: "manual_review",
					Status: map[bool]string{true: compliance.CheckPass, false: compliance.CheckFail}[dec.Verdict == compliance.Admissible],
					Detail: dec.Reason})
				reasons = append(append([]compliance.AdmissibilityReason{}, reasons...), compliance.AdmissibilityReason{Code: "manual_review", Kind: "manual",
					Message: fmt.Sprintf("%s：%s（操作人 %s）", dec.Verdict, dec.Reason, actor)})
			}
			cj, _ := json.Marshal(checks)
			rj, _ := json.Marshal(reasons)
			if _, err := tx.ExecContext(ctx, `INSERT INTO admissibility_items(tenant_id, run_id, binding_id, verdict, checks, reasons, created_by, updated_by)
				VALUES ($1,$2,$3,$4,$5::jsonb,$6::jsonb,$7,$7)`, tenant, newID, it.BindingID, v, string(cj), string(rj), actor); err != nil {
				return err
			}
		}
		out, err = runWithItemsTx(ctx, tx, tenant, newID)
		return err
	})
	return out, err
}

func runVerdictForItems(items []*AdmissibilityItem, decisions map[int64]AdmissibilityDecision, min int) string {
	if min <= 0 {
		min = 1
	}
	admissible, manual := 0, 0
	for _, it := range items {
		v := it.Verdict
		if dec, ok := decisions[it.ID]; ok {
			v = dec.Verdict
		}
		switch v {
		case compliance.Admissible:
			admissible++
		case compliance.Manual:
			manual++
		}
	}
	switch {
	case manual > 0:
		return compliance.Manual
	case admissible < min:
		return compliance.Inadmissible
	}
	return compliance.Admissible
}

// GetAdmissibilityRun 读取任务内一次运行（含明细）。
func (d *DB) GetAdmissibilityRun(ctx context.Context, tenant string, taskID, runID int64) (*AdmissibilityRun, error) {
	tenant = tenantOr(tenant)
	run, err := scanAdmissibilityRun(d.QueryRowContext(ctx, `SELECT `+admissibilityRunCols+`
		FROM admissibility_runs r JOIN requirement_items req
		  ON req.tenant_id=r.tenant_id AND req.task_id=r.task_id AND req.id=r.requirement_id
		WHERE r.tenant_id=$1 AND r.task_id=$2 AND r.id=$3`, tenant, taskID, runID))
	if err != nil {
		return nil, err
	}
	items, err := loadAdmissibilityItemsTx(ctx, d, tenant, runID)
	if err != nil {
		return nil, err
	}
	run.Items = items
	return run, nil
}
