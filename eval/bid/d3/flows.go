package main

// D3 主链路与门禁共享业务流程。直接调用 db / ingest / compliance / export 的 Go 业务接口，不经过 HTTP。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/compliance"
	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/evidence"
	"github.com/autobid/autobid-engine/export"
	"github.com/autobid/autobid-engine/ingest"
)

type appConfig struct {
	ArtifactsDir string
	GitCommit    string
	GoVersion    string
	Keep         bool
}

type app struct {
	cfg               appConfig
	d                 *db.DB
	golden            *goldenSpec
	defects           *defectsFile
	fixtures          *fixtureSet
	store             *evidence.Store
	exportSvc         *export.Service
	baseRunID         string
	runID             string
	census            []censusRow
	mainAdmissibility []admissibilityRow
	mainObjective     []objectiveRow
	mainCommitments   []commitmentRow
	mainStoryboards   []storyboardRow
	mainWritingVerifs []writingVerificationRow
	mainQuote         *db.BidQuote
	mainQuoteCheck    *db.BidQuoteValidation
}

type flow struct {
	app    *app
	r      *round
	runID  string
	tenant string
	task   *db.Task
}

func parseCST(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.In(beijingZone), nil
}

var beijingZone = time.FixedZone("CST", 8*3600)

func (f *flow) newTask(ctx context.Context, scenario, goal string) (*db.Task, string, error) {
	tenant := fmt.Sprintf("d3-%s-%s", f.runID, scenario)
	name := fmt.Sprintf("D3-%s-%s", f.runID, scenario)
	t, err := f.app.d.CreateTask(ctx, db.CreateTaskInput{Name: name, Description: "D3 E 级验收（虚构演示数据，默认自动清理）", Goal: goal})
	if err != nil {
		return nil, tenant, err
	}
	f.tenant, f.task = tenant, t
	f.r.addTask(t.ID)
	f.r.addTenant(tenant)
	return t, tenant, nil
}

func (f *flow) ingestDoc(ctx context.Context, data []byte, name, title, role, published string) (*ingest.Result, error) {
	at, err := parseCST(published)
	if err != nil {
		return nil, err
	}
	as := db.NewAssetStore(f.app.d, f.task.ID)
	return ingest.Ingest(ctx, f.app.d, as, ingest.Input{TenderNo: tenderNo, Title: title,
		File: &ingest.File{Name: name, MIME: demoDOCXMIME, Data: data}, TenantID: f.tenant, CreatedBy: actor,
		DocRole: role, PublishedAt: &at})
}

func (f *flow) ingestTender(ctx context.Context) (*ingest.Result, error) {
	return f.ingestDoc(ctx, f.app.fixtures.Tender, "tender.docx", tenderName, db.DocRoleTender, tenderPublished)
}

func (f *flow) ingestCorrection(ctx context.Context) (*ingest.Result, error) {
	return f.ingestDoc(ctx, f.app.fixtures.Correction, "correction.docx", correctionName, db.DocRoleCorrection, correctionPublished)
}

func (f *flow) requirements(ctx context.Context) (*db.RequirementsView, map[int64]*db.RequirementItem, error) {
	view, err := f.app.d.ListRequirements(ctx, f.tenant, f.task.ID)
	if err != nil {
		return nil, nil, err
	}
	byID := map[int64]*db.RequirementItem{}
	for _, it := range view.Items {
		byID[it.ID] = it
	}
	return view, byID, nil
}

func (f *flow) requirementBy(ctx context.Context, needle string) (*db.RequirementItem, error) {
	view, _, err := f.requirements(ctx)
	if err != nil {
		return nil, err
	}
	for _, it := range view.Items {
		if it.IsSuperseded() {
			continue
		}
		if strings.Contains(it.Title, needle) || strings.Contains(it.Description, needle) {
			return it, nil
		}
	}
	return nil, fmt.Errorf("requirement containing %q not found", needle)
}

func (f *flow) passVetoes(ctx context.Context, view *db.RequirementsView, note string) (int, error) {
	n := 0
	for _, it := range view.Items {
		if it.Category != db.ReqVetoItem || it.OutOfBid || it.IsSuperseded() {
			continue
		}
		if _, err := f.app.d.SetVetoStatus(ctx, f.tenant, f.task.ID, it.ID, actor, db.VetoPassed, note); err != nil {
			return n, fmt.Errorf("veto %s: %w", it.Code, err)
		}
		n++
	}
	return n, nil
}

func (f *flow) patchRequirements(ctx context.Context) error {
	view, _, err := f.requirements(ctx)
	if err != nil {
		return err
	}
	for _, it := range view.Items {
		if it.IsSuperseded() {
			continue
		}
		text := it.Title + " " + it.Description
		upd := db.RequirementInput{}
		switch {
		case strings.Contains(text, "资质要求"):
			upd.ProofRules = &db.ProofRules{BidderOnly: true, ValidOnBidDeadline: true, RequiredDocs: []string{"certificate"},
				ScopeKeywords: []string{"安全技术防范"}, MinCount: 1}
		case strings.Contains(text, "业绩要求"):
			upd.ProofRules = &db.ProofRules{BidderOnly: true, NeedAcceptance: true, DateAnchor: "acceptance_date", YearsWindow: 3,
				RequiredDocs: []string{"contract", "acceptance_report"}, ContractPages: []string{"标的页", "金额页", "签章页"},
				ScopeKeywords: []string{"安防"}, MinCount: 2}
		case strings.Contains(text, "类似项目业绩") && it.Category == db.ReqScoringItem:
			w, kind, group := 30.0, db.EvalObjective, db.ScoreGroupTechnical
			upd.ScoreWeight, upd.EvalKind, upd.ScoreGroup = &w, &kind, &group
			upd.ProofRules = &db.ProofRules{BidderOnly: true, NeedAcceptance: true, DateAnchor: "acceptance_date", YearsWindow: 3,
				RequiredDocs: []string{"contract", "acceptance_report"}, ContractPages: []string{"关键页"},
				ScopeKeywords: []string{"安防"}, MinCount: 2}
		case strings.Contains(text, "项目经理"):
			upd.ProofRules = &db.ProofRules{RequiredDocs: []string{"certificate", "social_security"}, SocialSecurityMonths: 6,
				ValidOnBidDeadline: true, MinCount: 1}
		case strings.Contains(text, "摄像机型号"):
			upd.ProofRules = &db.ProofRules{RequiredDocs: []string{"product_catalog"},
				ModelParams: map[string]string{"model": "SP-D2-4000", "resolution": "400万"}, MinCount: 1}
		case strings.Contains(text, "工期要求") || strings.Contains(text, "质保要求") || strings.Contains(text, "可用性要求") ||
			strings.Contains(text, "响应与到场要求") || strings.Contains(text, "修复要求") || strings.Contains(text, "团队人数要求"):
			upd.ProofRules = &db.ProofRules{}
		case strings.Contains(text, "更正"):
			upd.ProofRules = &db.ProofRules{}
		}
		if upd.ProofRules == nil && upd.ScoreWeight == nil {
			continue
		}
		if _, err := f.app.d.UpdateRequirement(ctx, f.tenant, f.task.ID, it.ID, actor, upd); err != nil {
			return fmt.Errorf("patch requirement %s: %w", it.Code, err)
		}
	}
	return nil
}

func (f *flow) confirmAndOutline(ctx context.Context) (*db.OutlineDetail, error) {
	if _, err := f.app.d.ConfirmRequirements(ctx, f.tenant, f.task.ID, actor, 0); err != nil {
		return nil, fmt.Errorf("confirm requirements: %w", err)
	}
	view, _, err := f.requirements(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := f.passVetoes(ctx, view, "D3 虚构演示：否决项已核对，证明材料索引见投标文件"); err != nil {
		return nil, err
	}
	det, err := f.app.d.CreateOutline(ctx, f.tenant, f.task.ID, actor, db.OutlineInput{Note: "D3 默认目录（由确认后的清单生成）"})
	if err != nil {
		return nil, fmt.Errorf("create outline: %w", err)
	}
	if _, err := f.app.d.SubmitOutline(ctx, f.tenant, f.task.ID, det.Outline.Version, actor); err != nil {
		return nil, err
	}
	return f.app.d.ApproveOutline(ctx, f.tenant, f.task.ID, det.Outline.Version, pinkReviewer)
}

func (f *flow) prepareTask(ctx context.Context, scenario string, correction bool) (*db.OutlineDetail, error) {
	if _, _, err := f.newTask(ctx, scenario, "D3 S1~S16 主链路（虚构）"); err != nil {
		return nil, err
	}
	if _, err := f.ingestTender(ctx); err != nil {
		return nil, fmt.Errorf("ingest tender: %w", err)
	}
	if correction {
		if _, err := f.ingestCorrection(ctx); err != nil {
			return nil, fmt.Errorf("ingest correction: %w", err)
		}
	}
	if _, err := f.app.d.ExtractRequirements(ctx, f.tenant, f.task.ID, actor); err != nil {
		return nil, fmt.Errorf("extract requirements: %w", err)
	}
	if err := f.patchRequirements(ctx); err != nil {
		return nil, err
	}
	return f.confirmAndOutline(ctx)
}

type evidenceSet struct {
	QualValid    *db.CompanyCredential
	QualCritical *db.CompanyCredential
	QualExpired  *db.CompanyCredential
	Person       *db.CompanyCredential
	ProductOK    *db.CompanyCredential
	ProductBad   *db.CompanyCredential
	Perf1        *db.CompanyPerformance
	Perf2        *db.CompanyPerformance
	PerfSub      *db.CompanyPerformance
	PerfOld      *db.CompanyPerformance
	PerfNoAcc    *db.CompanyPerformance
	Originals    int
}

func (f *flow) importEvidence(ctx context.Context) (*evidenceSet, error) {
	es := &evidenceSet{}
	cred := func(c *db.CompanyCredential) error {
		c.TenantID = f.tenant
		c.CreatedBy = actor
		saved, _, err := f.app.d.UpsertCompanyCredential(ctx, c)
		if err == nil {
			*c = *saved
		}
		return err
	}
	if err := cred(&db.CompanyCredential{Category: "company_profile", Code: "PROFILE", Title: "企业主体关系（虚构）",
		SubjectName: bidderName, SubjectType: "bidder", CertificateNo: "", Attributes: json.RawMessage(fmt.Sprintf(
			`{"legal_name":%q,"parent_name":%q,"subsidiary_names":[%q]}`, bidderName, parentName, subsidiaryName))}); err != nil {
		return nil, err
	}
	type credSpec struct {
		dst **db.CompanyCredential
		c   db.CompanyCredential
	}
	specs := []credSpec{
		{&es.QualValid, db.CompanyCredential{Category: db.CredentialQualification, Code: "QUAL-VALID", Title: "安全技术防范系统设计施工二级（虚构·有效）",
			SubjectName: bidderName, SubjectType: "bidder", CertificateNo: "DEMO-AF-0001", Issuer: "虚构发证机构", IssuedOn: strp("2024-07-01"),
			ValidUntil: strp("2027-06-30"), Scope: "安全技术防范系统设计施工", Attributes: json.RawMessage(`{"level":"二级"}`)}},
		{&es.QualCritical, db.CompanyCredential{Category: db.CredentialQualification, Code: "QUAL-CRITICAL", Title: "安全技术防范系统设计施工二级（虚构·临界）",
			SubjectName: bidderName, SubjectType: "bidder", CertificateNo: "DEMO-AF-0002", Issuer: "虚构发证机构", IssuedOn: strp("2023-11-25"),
			ValidUntil: strp("2026-11-25"), Scope: "安全技术防范系统设计施工", Attributes: json.RawMessage(`{"level":"二级"}`)}},
		{&es.QualExpired, db.CompanyCredential{Category: db.CredentialQualification, Code: "QUAL-EXPIRED", Title: "安全技术防范系统设计施工二级（虚构·过期）",
			SubjectName: bidderName, SubjectType: "bidder", CertificateNo: "DEMO-AF-0003", Issuer: "虚构发证机构", IssuedOn: strp("2020-10-01"),
			ValidUntil: strp("2026-10-01"), Scope: "安全技术防范系统设计施工", Attributes: json.RawMessage(`{"level":"二级"}`)}},
		{&es.Person, db.CompanyCredential{Category: db.CredentialPersonnel, Code: "PM-1", Title: "项目经理（虚构）", SubjectName: "演示项目经理（虚构）",
			SubjectType: "person", CertificateNo: "DEMO-PM-0001", Issuer: "虚构行业协会", IssuedOn: strp("2024-01-01"), ValidUntil: strp("2027-12-31"),
			Attributes: json.RawMessage(`{"person":"演示项目经理（虚构）","role":"项目经理","social_security_months":12}`)}},
		{&es.ProductOK, db.CompanyCredential{Category: "product", Code: "CAM-OK", Title: "核心网络摄像机 SP-D2-4000（虚构）", SubjectName: bidderName,
			SubjectType: "bidder", ValidUntil: strp("2027-12-31"), Scope: "视频监控设备",
			Attributes: json.RawMessage(`{"model":"SP-D2-4000","parameters":{"resolution":"400万","protocol":"ONVIF"}}`)}},
		{&es.ProductBad, db.CompanyCredential{Category: "product", Code: "CAM-BAD", Title: "核心网络摄像机 SP-D2-2000（虚构）", SubjectName: bidderName,
			SubjectType: "bidder", ValidUntil: strp("2027-12-31"), Scope: "视频监控设备",
			Attributes: json.RawMessage(`{"model":"SP-D2-2000","parameters":{"resolution":"200万","protocol":"ONVIF"}}`)}},
	}
	for _, sp := range specs {
		c := sp.c
		if err := cred(&c); err != nil {
			return nil, fmt.Errorf("credential %s: %w", c.Code, err)
		}
		*sp.dst = &c
	}
	perf := func(p *db.CompanyPerformance) error {
		p.TenantID = f.tenant
		p.CreatedBy = actor
		saved, _, err := f.app.d.UpsertCompanyPerformance(ctx, p)
		if err == nil {
			*p = *saved
		}
		return err
	}
	perfs := []struct {
		dst **db.CompanyPerformance
		p   db.CompanyPerformance
	}{
		{&es.Perf1, db.CompanyPerformance{Code: "PERF-1", ProjectName: "虚构智慧园区安防系统建设项目", Buyer: "虚构产业园区管理委员会", ContractNo: "DEMO-HT-0001",
			ContractDate: strp("2024-03-01"), AcceptanceDate: strp("2024-09-01"), Amount: "8600000.0000", SubjectName: bidderName, SubjectType: "bidder",
			Scope: "安防视频监控、门禁与停车诱导系统集成"}},
		{&es.Perf2, db.CompanyPerformance{Code: "PERF-2", ProjectName: "虚构政务服务中心安防改造项目", Buyer: "虚构市政务服务中心", ContractNo: "DEMO-HT-0002",
			ContractDate: strp("2025-02-10"), AcceptanceDate: strp("2025-08-15"), Amount: "4200000.0000", SubjectName: bidderName, SubjectType: "bidder",
			Scope: "安防平台升级与运维服务"}},
		{&es.PerfSub, db.CompanyPerformance{Code: "PERF-SUB", ProjectName: "子公司虚构安防项目", Buyer: "虚构客户", ContractNo: "DEMO-HT-SUB",
			ContractDate: strp("2025-01-01"), AcceptanceDate: strp("2025-05-01"), Amount: "3000000.0000", SubjectName: subsidiaryName, SubjectType: "subsidiary",
			IsSubsidiary: true, Scope: "视频监控系统集成"}},
		{&es.PerfOld, db.CompanyPerformance{Code: "PERF-OLD", ProjectName: "窗口外虚构安防项目", Buyer: "虚构客户", ContractNo: "DEMO-HT-OLD",
			ContractDate: strp("2019-01-01"), AcceptanceDate: strp("2020-01-01"), Amount: "1000000.0000", SubjectName: bidderName, SubjectType: "bidder",
			Scope: "安防系统集成"}},
		{&es.PerfNoAcc, db.CompanyPerformance{Code: "PERF-NOACC", ProjectName: "缺验收虚构安防项目", Buyer: "虚构客户", ContractNo: "DEMO-HT-NOACC",
			ContractDate: strp("2025-03-01"), Amount: "900000.0000", SubjectName: bidderName, SubjectType: "bidder", Scope: "安防系统集成"}},
	}
	for _, sp := range perfs {
		p := sp.p
		if err := perf(&p); err != nil {
			return nil, fmt.Errorf("performance %s: %w", p.Code, err)
		}
		*sp.dst = &p
	}
	add := func(domain string, ownerID int64, key string) error {
		spec, ok := originalSpecByKey(key)
		if !ok {
			return fmt.Errorf("unknown original %s", key)
		}
		rel, sum, err := f.app.fixtures.putOriginal(f.app.store, f.tenant, key)
		if err != nil {
			return err
		}
		if _, err := f.app.d.AddCompanyEvidenceOriginal(ctx, &db.CompanyEvidenceOriginal{TenantID: f.tenant, OwnerDomain: domain, OwnerID: ownerID,
			DocumentKind: spec.Kind, PageLabel: spec.PageLabel, OriginalPath: rel, OriginalSHA256: sum,
			PageStart: spec.PageStart, PageEnd: spec.PageEnd, CreatedBy: actor}, false); err != nil {
			return err
		}
		es.Originals++
		return nil
	}
	for _, x := range []struct {
		domain string
		id     int64
		keys   []string
	}{
		{db.EvidenceDomainCredential, es.QualValid.ID, []string{"qual_valid", "qual_valid_validity"}},
		{db.EvidenceDomainCredential, es.QualCritical.ID, []string{"qual_critical"}},
		{db.EvidenceDomainCredential, es.QualExpired.ID, []string{"qual_expired"}},
		{db.EvidenceDomainCredential, es.Person.ID, []string{"person_cert", "person_ss"}},
		{db.EvidenceDomainCredential, es.ProductOK.ID, []string{"product_ok"}},
		{db.EvidenceDomainCredential, es.ProductBad.ID, []string{"product_bad"}},
		{db.EvidenceDomainPerformance, es.Perf1.ID, []string{"perf1_contract", "perf1_amount", "perf1_seal", "perf1_accept"}},
		{db.EvidenceDomainPerformance, es.Perf2.ID, []string{"perf2_contract", "perf2_amount", "perf2_seal", "perf2_accept"}},
		{db.EvidenceDomainPerformance, es.PerfSub.ID, []string{"perf_sub_contract", "perf_sub_accept"}},
		{db.EvidenceDomainPerformance, es.PerfOld.ID, []string{"perf_old_contract", "perf_old_accept"}},
		{db.EvidenceDomainPerformance, es.PerfNoAcc.ID, []string{"perf_noacc_contract"}},
	} {
		for _, key := range x.keys {
			if err := add(x.domain, x.id, key); err != nil {
				return nil, err
			}
		}
	}
	return es, nil
}

func (f *flow) bind(ctx context.Context, reqID int64, domain string, evidenceID int64) error {
	_, err := f.app.d.BindRequirementEvidence(ctx, f.tenant, f.task.ID, reqID, domain, evidenceID, actor)
	return err
}

func (f *flow) evaluate(ctx context.Context, reqID int64) (*db.AdmissibilityRun, error) {
	return f.app.d.EvaluateAdmissibility(ctx, f.tenant, f.task.ID, reqID, f.app.store, actor)
}

func (f *flow) bindMainEvidence(ctx context.Context, es *evidenceSet) (map[int64]*db.AdmissibilityRun, error) {
	qual, err := f.requirementBy(ctx, "资质要求")
	if err != nil {
		return nil, err
	}
	perf, err := f.requirementBy(ctx, "业绩要求")
	if err != nil {
		return nil, err
	}
	obj, err := f.requirementBy(ctx, "类似项目业绩")
	if err != nil {
		return nil, err
	}
	pm, err := f.requirementBy(ctx, "项目经理")
	if err != nil {
		return nil, err
	}
	prod, err := f.requirementBy(ctx, "摄像机型号")
	if err != nil {
		return nil, err
	}
	binds := []struct {
		req    *db.RequirementItem
		domain string
		id     int64
	}{
		{qual, db.EvidenceDomainCredential, es.QualValid.ID},
		{perf, db.EvidenceDomainPerformance, es.Perf1.ID},
		{perf, db.EvidenceDomainPerformance, es.Perf2.ID},
		{obj, db.EvidenceDomainPerformance, es.Perf1.ID},
		{obj, db.EvidenceDomainPerformance, es.Perf2.ID},
		{pm, db.EvidenceDomainCredential, es.Person.ID},
		{prod, db.EvidenceDomainCredential, es.ProductOK.ID},
	}
	for _, b := range binds {
		if err := f.bind(ctx, b.req.ID, b.domain, b.id); err != nil {
			return nil, fmt.Errorf("bind %s: %w", b.req.Code, err)
		}
	}
	runs := map[int64]*db.AdmissibilityRun{}
	for _, req := range []*db.RequirementItem{qual, perf, obj, pm, prod} {
		run, err := f.evaluate(ctx, req.ID)
		if err != nil {
			return nil, fmt.Errorf("evaluate %s: %w", req.Code, err)
		}
		runs[req.ID] = run
	}
	return runs, nil
}

type commitmentSeed struct {
	Code            string
	Needle          string
	Metric          string
	Label           string
	Unit            string
	Cmp             string
	ThresholdRaw    string
	Threshold       float64
	CommittedRaw    string
	Committed       float64
	Capability      *float64
	CapabilityBasis string
	Owner           string
	Rationale       string
	Signoff         bool
}

func defaultCommitmentSeeds() []commitmentSeed {
	cap90 := 90.0
	cap5 := 5.0
	cap2 := 2.0
	cap4 := 4.0
	cap24 := 24.0
	capAvail := 99.95
	capTeam := 12.0
	return []commitmentSeed{
		{Code: "C-DURATION", Needle: "工期要求", Metric: db.MetricDuration, Label: "总工期", Unit: "天", Cmp: "<=",
			ThresholdRaw: "不超过 90 日历天", Threshold: 90, CommittedRaw: "90 天", Committed: 90, Capability: &cap90,
			CapabilityBasis: "自有施工班组与实施计划", Owner: "项目经理", Rationale: "实施进度计划"},
		{Code: "C-WARRANTY", Needle: "质保要求", Metric: db.MetricWarranty, Label: "质保期", Unit: "年", Cmp: ">=",
			ThresholdRaw: "不少于 3 年", Threshold: 3, CommittedRaw: "3 年", Committed: 3, Capability: &cap5,
			CapabilityBasis: "原厂质保与备件协议", Owner: "售后负责人", Rationale: "质保服务方案"},
		{Code: "C-AVAILABILITY", Needle: "可用性要求", Metric: db.MetricAvailability, Label: "系统可用性", Unit: "%", Cmp: ">=",
			ThresholdRaw: "不低于 99.9%", Threshold: 99.9, CommittedRaw: "99.9%", Committed: 99.9, Capability: &capAvail,
			CapabilityBasis: "双机热备与运维值守", Owner: "技术负责人", Rationale: "可用性设计"},
		{Code: "C-SLA-RESPONSE", Needle: "响应与到场要求", Metric: db.MetricSLAResponse, Label: "故障响应时间", Unit: "小时", Cmp: "<=",
			ThresholdRaw: "2 小时内响应", Threshold: 2, CommittedRaw: "2 小时", Committed: 2, Capability: &cap2,
			CapabilityBasis: "7×24 服务热线", Owner: "售后负责人", Rationale: "服务承诺"},
		{Code: "C-SLA-ONSITE", Needle: "响应与到场要求", Metric: db.MetricSLAOnsite, Label: "到场时间", Unit: "小时", Cmp: "<=",
			ThresholdRaw: "4 小时内到达现场", Threshold: 4, CommittedRaw: "4 小时", Committed: 4, Capability: &cap4,
			CapabilityBasis: "本地服务网点", Owner: "售后负责人", Rationale: "服务承诺"},
		{Code: "C-SLA-REPAIR", Needle: "修复要求", Metric: db.MetricSLARepair, Label: "修复时间", Unit: "小时", Cmp: "<=",
			ThresholdRaw: "24 小时内修复", Threshold: 24, CommittedRaw: "24 小时", Committed: 24, Capability: &cap24,
			CapabilityBasis: "备件库与二线支持", Owner: "售后负责人", Rationale: "服务承诺"},
		{Code: "C-TEAM", Needle: "团队人数要求", Metric: db.MetricTeamSize, Label: "团队人数", Unit: "人", Cmp: ">=",
			ThresholdRaw: "不少于 12 人", Threshold: 12, CommittedRaw: "12 人", Committed: 12, Capability: &capTeam,
			CapabilityBasis: "在岗人员名册 12 人", Owner: "交付负责人", Rationale: "人员配置计划"},
		{Code: "C-PRICE", Needle: "投标报价", Metric: db.MetricPrice, Label: "投标总价", Unit: "元", Cmp: "<=",
			ThresholdRaw: "最高限价 500000 元", Threshold: 500000, CommittedRaw: "300000.00 元", Committed: 300000,
			Owner: "商务负责人", Rationale: "成本测算"},
	}
}

func (f *flow) seedCommitments(ctx context.Context, seeds []commitmentSeed) (map[string]*db.Commitment, error) {
	out := map[string]*db.Commitment{}
	for _, s := range seeds {
		req, err := f.requirementBy(ctx, s.Needle)
		if err != nil {
			return nil, err
		}
		in := db.CommitmentInput{Code: s.Code, RequirementID: &req.ID, Metric: s.Metric, MetricLabel: s.Label, Unit: s.Unit,
			ThresholdCmp: s.Cmp, TenderThresholdRaw: s.ThresholdRaw, TenderThresholdValue: floatp(s.Threshold),
			CommittedRaw: s.CommittedRaw, CommittedValue: floatp(s.Committed), CapabilityLimit: s.Capability,
			CapabilityBasis: s.CapabilityBasis, Owner: s.Owner, Rationale: s.Rationale}
		if _, err := f.app.d.UpsertCommitment(ctx, f.tenant, f.task.ID, in, actor); err != nil {
			return nil, fmt.Errorf("upsert commitment %s: %w", s.Code, err)
		}
		if s.Signoff {
			if _, err := f.app.d.SignOffCommitment(ctx, f.tenant, f.task.ID, s.Code, signer, "交付能力已确认"); err != nil {
				return nil, fmt.Errorf("signoff %s: %w", s.Code, err)
			}
		}
		c, err := f.app.d.ConfirmCommitment(ctx, f.tenant, f.task.ID, s.Code, actor)
		if err != nil {
			return nil, fmt.Errorf("confirm commitment %s: %w", s.Code, err)
		}
		out[s.Metric] = c
	}
	return out, nil
}

func (f *flow) computeObjective(ctx context.Context) ([]*db.ObjectiveScoreRun, error) {
	return f.app.d.ComputeObjectiveScores(ctx, f.tenant, f.task.ID, 0, actor)
}

func (f *flow) createQuote(ctx context.Context, variant string) (*db.BidQuote, *db.BidQuoteValidation, error) {
	in := db.BidQuoteInput{Lot: "", DeclaredTotal: "300000.00", Ceiling: strp("500000.00"), Currency: "CNY",
		Note: "D3 虚构报价", Items: []db.BidQuoteItemInput{
			{LineNo: 1, Code: "L1", Name: "核心网络摄像机", UnitPrice: "1000.00", Quantity: "200", LineAmount: "200000.00"},
			{LineNo: 2, Code: "L2", Name: "安防平台软件", UnitPrice: "500.00", Quantity: "200", LineAmount: "100000.00"},
		}}
	q, err := f.app.d.CreateBidQuote(ctx, f.tenant, f.task.ID, in, "commercial")
	if err != nil {
		return nil, nil, err
	}
	val, err := f.app.d.ValidateBidQuote(ctx, f.tenant, f.task.ID, q.ID)
	if err != nil {
		return nil, nil, err
	}
	if variant != "" {
		return q, val, nil
	}
	if !val.OK {
		return q, val, fmt.Errorf("expected quote to validate, checks=%+v", val.Checks)
	}
	q, val, err = f.app.d.ConfirmBidQuote(ctx, f.tenant, f.task.ID, q.ID, "commercial")
	return q, val, err
}

func (f *flow) allCommitments(ctx context.Context) ([]*db.Commitment, error) {
	return f.app.d.ListCommitments(ctx, f.tenant, f.task.ID)
}

// writeSectionsD3 执行完整 D3 章节写作流：
// 1) 创建/更新章节故事板（actor）；
// 2) 提交粉队评审（actor）并由独立粉队评审人（pinkReviewer）批准；
// 3) 领用写作租约（ClaimSectionWrite）；
// 4) 调用 RenderSectionGroundedDraft 确定性展开 [[EVID:...]] 与 [[COMMIT:...]]；
// 5) 保存章节修订（SaveSectionRevision）并持久化句子级写作核验指标；
// 6) 由独立红队评审人（redReviewer != actor）批准定稿。
func (f *flow) writeSectionsD3(ctx context.Context, det *db.OutlineDetail, runs map[int64]*db.AdmissibilityRun, commits []*db.Commitment) error {
	_, byID, err := f.requirements(ctx)
	if err != nil {
		return err
	}
	for _, sec := range det.Sections {
		reqs := make([]*db.RequirementItem, 0, len(sec.RequirementIDs))
		for _, rid := range sec.RequirementIDs {
			if it := byID[rid]; it != nil && !it.IsSuperseded() {
				reqs = append(reqs, it)
			}
		}
		sort.Slice(reqs, func(i, j int) bool { return reqs[i].Code < reqs[j].Code })
		plan := buildSectionPlan(renderInput{Section: sec, Requirements: reqs, Runs: runs, Commitments: commits})
		plan.StoryboardIn.TaskID = f.task.ID
		plan.StoryboardIn.SectionID = sec.ID
		if _, err := f.app.d.UpsertStoryboard(ctx, f.tenant, actor, plan.StoryboardIn); err != nil {
			return fmt.Errorf("upsert storyboard %s: %w", sec.Code, err)
		}
		if _, err := f.app.d.SubmitStoryboard(ctx, f.tenant, f.task.ID, sec.ID, actor); err != nil {
			return fmt.Errorf("submit storyboard %s: %w", sec.Code, err)
		}
		if _, err := f.app.d.ApproveStoryboard(ctx, f.tenant, f.task.ID, sec.ID, "粉队评审通过：得分点、证据与承诺白名单齐备", pinkReviewer); err != nil {
			return fmt.Errorf("approve storyboard %s: %w", sec.Code, err)
		}
		claim, err := f.app.d.ClaimSectionWrite(ctx, f.tenant, f.task.ID, sec.ID, actor, time.Minute)
		if err != nil {
			return fmt.Errorf("claim section %s: %w", sec.Code, err)
		}
		rendered, err := f.app.d.RenderSectionGroundedDraft(ctx, f.tenant, f.task.ID, sec.ID, plan.TemplateMD)
		if err != nil {
			return fmt.Errorf("render section %s: %w", sec.Code, err)
		}
		if strings.Contains(rendered.RenderedMD, "[[EVID:") || strings.Contains(rendered.RenderedMD, "[[COMMIT:") {
			return fmt.Errorf("section %s still has unresolved citation tokens", sec.Code)
		}
		res, err := f.app.d.SaveSectionRevision(ctx, f.tenant, actor, db.SaveRevisionInput{
			TaskID:                f.task.ID,
			SectionID:             sec.ID,
			BaseRevNo:             claim.BaseRevNo,
			BasedOnOutlineVersion: det.Outline.Version,
			Source:                db.RevHumanEdit,
			ContentMD:             plan.TemplateMD,
			SummaryNote:           sectionSummary(sec),
			DiffSummary:           "D3 受控引用确定性渲染与句子级核验初稿",
			LeaseToken:            claim.LeaseToken,
		})
		if err != nil {
			return fmt.Errorf("save section %s: %w", sec.Code, err)
		}
		if res.Outcome != db.SaveApplied {
			return fmt.Errorf("section %s outcome=%s: %s", sec.Code, res.Outcome, res.Reason)
		}
		if _, err := f.app.d.ApproveSectionRevision(ctx, f.tenant, f.task.ID, sec.ID, res.Revision.RevNo, redReviewer); err != nil {
			return fmt.Errorf("red approve %s: %w", sec.Code, err)
		}
	}
	return nil
}

func (f *flow) matrixAndConsistency(ctx context.Context) (*db.VerifyMatrixResult, *db.ConsistencyRunDetail, error) {
	mres, err := f.app.d.VerifyComplianceMatrix(ctx, f.tenant, f.task.ID, 0, compliance.RuleVerifier{}, actor)
	if err != nil {
		return nil, nil, err
	}
	cres, err := f.app.d.RunConsistencyCheck(ctx, f.tenant, f.task.ID, actor)
	if err != nil {
		return nil, nil, err
	}
	return mres, cres, nil
}

func (f *flow) readiness(ctx context.Context) (*db.ExportReadiness, error) {
	return f.app.d.ExportReadiness(ctx, f.tenant, f.task.ID)
}

func (f *flow) buildReadyD3(ctx context.Context, scenario string, es *evidenceSet, correction bool) (*db.OutlineDetail, map[int64]*db.AdmissibilityRun, *evidenceSet, error) {
	det, err := f.prepareTask(ctx, scenario, correction)
	if err != nil {
		return nil, nil, nil, err
	}
	if es == nil {
		if es, err = f.importEvidence(ctx); err != nil {
			return nil, nil, nil, err
		}
	}
	runs, err := f.bindMainEvidence(ctx, es)
	if err != nil {
		return nil, nil, nil, err
	}
	for reqID, run := range runs {
		if run.Verdict != compliance.Admissible {
			return nil, nil, nil, fmt.Errorf("requirement %d admissibility=%s", reqID, run.Verdict)
		}
	}
	if _, err := f.seedCommitments(ctx, defaultCommitmentSeeds()); err != nil {
		return nil, nil, nil, err
	}
	if _, err := f.computeObjective(ctx); err != nil {
		return nil, nil, nil, err
	}
	if _, _, err := f.createQuote(ctx, ""); err != nil {
		return nil, nil, nil, err
	}
	commits, err := f.allCommitments(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := f.writeSectionsD3(ctx, det, runs, commits); err != nil {
		return nil, nil, nil, err
	}
	mres, cres, err := f.matrixAndConsistency(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	if mres.Verified != mres.Checked || mres.Failed > 0 || mres.Pending > 0 || mres.Stale > 0 {
		return nil, nil, nil, fmt.Errorf("matrix not fully verified: checked=%d verified=%d failed=%d pending=%d stale=%d (%s)",
			mres.Checked, mres.Verified, mres.Failed, mres.Pending, mres.Stale, failedRowText(mres.Rows))
	}
	if len(cres.Findings) != 0 {
		return nil, nil, nil, fmt.Errorf("consistency findings: %d", len(cres.Findings))
	}
	rd, err := f.readiness(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	if !rd.Ready {
		return nil, nil, nil, fmt.Errorf("not ready: %s", kindList(rd.Blockers))
	}
	return det, runs, es, nil
}

func floatp(v float64) *float64 { return &v }
func strp(s string) *string     { return &s }

func evidenceSummary(es *evidenceSet) map[string]int {
	return map[string]int{
		"qualification": 3,
		"performance":   5,
		"personnel":     1,
		"product":       2,
		"originals":     es.Originals,
	}
}

type censusRow struct {
	Category   string `json:"category"`
	Code       string `json:"code"`
	Title      string `json:"title"`
	Subject    string `json:"subject"`
	ValidUntil string `json:"valid_until,omitempty"`
	Originals  int    `json:"originals"`
	PageLabels string `json:"page_labels"`
}

func (f *flow) writeEvidenceCensus(ctx context.Context, es *evidenceSet) ([]censusRow, error) {
	creds, err := f.app.d.ListCompanyCredentials(ctx, db.CredentialFilter{Tenant: f.tenant, Limit: 1000})
	if err != nil {
		return nil, err
	}
	perfs, err := f.app.d.ListCompanyPerformance(ctx, f.tenant, false, 1000)
	if err != nil {
		return nil, err
	}
	var rows []censusRow
	for _, c := range creds {
		orig, err := f.app.d.ListCompanyEvidenceOriginals(ctx, f.tenant, db.EvidenceDomainCredential, c.ID, false)
		if err != nil {
			return nil, err
		}
		labels := []string{}
		for _, o := range orig {
			labels = append(labels, o.DocumentKind+":"+o.PageLabel)
		}
		sort.Strings(labels)
		valid := ""
		if c.ValidUntil != nil {
			valid = *c.ValidUntil
		}
		rows = append(rows, censusRow{Category: c.Category, Code: c.Code, Title: c.Title, Subject: c.SubjectName,
			ValidUntil: valid, Originals: len(orig), PageLabels: strings.Join(labels, ", ")})
	}
	for _, p := range perfs {
		orig, err := f.app.d.ListCompanyEvidenceOriginals(ctx, f.tenant, db.EvidenceDomainPerformance, p.ID, false)
		if err != nil {
			return nil, err
		}
		labels := []string{}
		for _, o := range orig {
			labels = append(labels, o.DocumentKind+":"+o.PageLabel)
		}
		sort.Strings(labels)
		rows = append(rows, censusRow{Category: "performance", Code: p.Code, Title: p.ProjectName, Subject: p.SubjectName,
			ValidUntil: "", Originals: len(orig), PageLabels: strings.Join(labels, ", ")})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Category != rows[j].Category {
			return rows[i].Category < rows[j].Category
		}
		return rows[i].Code < rows[j].Code
	})
	path := filepath.Join(f.app.cfg.ArtifactsDir, fmt.Sprintf("evidence-census-r%d.json", f.r.index))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return rows, err
	}
	raw, _ := json.MarshalIndent(rows, "", "  ")
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		return rows, err
	}
	if art, err := artifactOf("evidence_census", path, "证据普查表（主体/有效期/原件/页码）"); err == nil {
		f.r.artifacts = append(f.r.artifacts, art)
	}
	return rows, nil
}

func (f *flow) createExport(ctx context.Context) ([]*db.ExportJob, *db.ExportPreflight, error) {
	return f.app.d.CreateExportJobs(ctx, f.tenant, f.task.ID, actor, db.CreateExportInput{Formats: []string{db.ExportDOCX},
		DefaultTemplateHash: export.DefaultTemplateHash(), Bidder: bidderName})
}

func (a *app) exportDOCX(ctx context.Context, f *flow, jobID int64) {
	r := f.r
	ref, err := a.d.ClaimExportJob(ctx, "d3-acceptance", 2*time.Minute, f.task.ID)
	if err != nil {
		r.fail("S15", "export.claim", "claimed", err, "")
		return
	}
	if ref == nil || ref.ID != jobID {
		r.check("S15", "export.claim", "claimed", "no claimable job", false, "")
		return
	}
	r.check("S15", "export.claim", "running", ref.Status, ref.Status == db.ExportRunning, "")
	res, err := a.exportSvc.Execute(ctx, ref.Tenant, ref.ExportJob)
	if err != nil {
		_ = a.d.FailExportJob(ctx, ref.ID, ref.LeaseToken, err.Error(), false)
		r.fail("S15", "export.execute", "succeeded", err, "")
		return
	}
	if err := a.d.CompleteExportJob(ctx, ref.ID, ref.LeaseToken, *res); err != nil {
		r.fail("S15", "export.complete", "succeeded", err, "")
		return
	}
	job, err := a.d.GetExportJob(ctx, f.tenant, f.task.ID, ref.ID)
	if err != nil {
		r.fail("S15", "export.reload", "succeeded", err, "")
		return
	}
	r.check("S15", "export.status", db.ExportSucceeded, job.Status, job.Status == db.ExportSucceeded, job.Error)
	abs, err := a.exportSvc.ArtifactPath(job)
	if err != nil {
		r.fail("S15", "export.artifact", "exists", err, "")
		return
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		r.fail("S15", "export.artifact", "exists", err, "")
		return
	}
	r.check("S15", "export.artifact_size", "matches task record", boolText(job.ArtifactSize == int64(len(data)) && len(data) > 0),
		job.ArtifactSize == int64(len(data)) && len(data) > 0, "")
	r.check("S15", "export.artifact_hash", "matches task record", boolText(job.ArtifactHash == sha256Hex(data)),
		job.ArtifactHash == sha256Hex(data), "")
	dst := filepath.Join(a.cfg.ArtifactsDir, "exports", fmt.Sprintf("main-r%d.docx", r.index))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		r.fail("S15", "export.copy", "copied", err, "")
		return
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		r.fail("S15", "export.copy", "copied", err, "")
		return
	}
	if art, err := artifactOf("export_docx", dst, "D3 主链路导出的技术标 DOCX（未启用 文档引擎）"); err == nil {
		r.artifacts = append(r.artifacts, art)
	}
	parsed, perr := ingest.Parse(ctx, ingest.File{Name: "main-export.docx", MIME: demoDOCXMIME, Data: data}, ingest.ParseOptions{})
	if perr != nil {
		r.fail("S15", "export.parse", "parsed", perr, "")
		return
	}
	r.check("S15", "export.parse", "no error", "ok", len(parsed.Text) > 0, "")
	det, err := a.d.GetOutline(ctx, f.tenant, f.task.ID, job.OutlineVersion)
	if err != nil {
		r.fail("S15", "export.headings", "readable", err, "")
		return
	}
	var missing []string
	r.headings = r.headings[:0]
	for _, sec := range det.Sections {
		heading := strings.TrimSpace(sec.Code + " " + sec.Title)
		r.headings = append(r.headings, heading)
		if !strings.Contains(parsed.Text, heading) {
			missing = append(missing, heading)
		}
	}
	r.check("S15", "export.headings", strconv.Itoa(len(det.Sections)), strconv.Itoa(len(det.Sections)-len(missing)),
		len(missing) == 0, strings.Join(missing, "; "))
	r.check("S15", "export.body", "present", boolText(strings.Contains(parsed.Text, "我方承诺完全满足上述要求")),
		strings.Contains(parsed.Text, "我方承诺完全满足上述要求"), "")
	kinds := warningKinds(job.Warnings)
	sort.Strings(kinds)
	r.warningKinds = kinds
	tocOK := job.TOCRefreshed || containsString(kinds, db.WarnTOCNotRefreshed)
	r.check("S15", "export.toc", "refreshed or warned", fmt.Sprintf("refreshed=%v warnings=%s", job.TOCRefreshed, strings.Join(kinds, ",")),
		tocOK, "D3 只导出 DOCX；目录未刷新允许为 false，但必须记录 toc_not_refreshed 警告")
	r.exportReadback = "ok"
	r.mainExported = job.Status == db.ExportSucceeded && len(missing) == 0
}

func warningKinds(warns []db.ExportWarning) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range warns {
		if !seen[w.Kind] {
			seen[w.Kind] = true
			out = append(out, w.Kind)
		}
	}
	return out
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func (f *flow) checkGolden(view *db.RequirementsView) {
	veto, scoring, objective, mandatory, superseded, nonScoring := 0, 0, 0, 0, 0, 0
	for _, it := range view.Items {
		if it.IsSuperseded() {
			superseded++
			continue
		}
		switch it.Category {
		case db.ReqVetoItem:
			veto++
		case db.ReqMandatoryClause:
			mandatory++
		case db.ReqNonScoringRequired:
			nonScoring++
		case db.ReqScoringItem:
			if it.ParentID == nil {
				scoring++
			}
		}
		if it.EvalKind == db.EvalObjective && it.ScoreGroup != db.ScoreGroupPrice {
			objective++
		}
	}
	g := f.app.golden.Requirements
	f.r.check("S4", "S4.veto_items", fmt.Sprint(g["veto_items"]), fmt.Sprint(veto), veto == g["veto_items"], "")
	f.r.check("S4", "S4.scoring_items", fmt.Sprint(g["scoring_items"]), fmt.Sprint(scoring), scoring == g["scoring_items"], "")
	f.r.check("S4", "S4.objective_items", fmt.Sprint(g["objective_items"]), fmt.Sprint(objective), objective == g["objective_items"], "")
	f.r.check("S4", "S4.mandatory_clauses", fmt.Sprint(g["mandatory_clauses"]), fmt.Sprint(mandatory), mandatory == g["mandatory_clauses"], "")
	f.r.check("S4", "S4.non_scoring_required", fmt.Sprint(g["non_scoring_required"]), fmt.Sprint(nonScoring), nonScoring == g["non_scoring_required"], "")
	f.r.check("S4", "S4.superseded", fmt.Sprint(g["superseded"]), fmt.Sprint(superseded), superseded == g["superseded"], "")
}

func (f *flow) writeChunkCensus(ctx context.Context) error {
	chunks, err := f.app.d.TaskChunks(ctx, f.task.ID, 0, 5000)
	if err != nil {
		return err
	}
	docs, err := f.app.d.ListTenderDocuments(ctx, f.task.ID)
	if err != nil {
		return err
	}
	roles := map[int64]string{}
	for _, d := range docs {
		roles[d.ID] = d.DocRole
	}
	rows := []map[string]any{}
	for _, c := range chunks {
		rows = append(rows, map[string]any{"doc_role": roles[c.DocID], "seq": c.Seq, "level": c.Level, "code": c.SectionCode,
			"title": c.SectionTitle, "page_start": c.PageStart, "page_end": c.PageEnd, "mandatory": c.IsMandatory,
			"table_count": chunkTableCount(c), "content": excerpt(c.Content, 120)})
	}
	path := filepath.Join(f.app.cfg.ArtifactsDir, fmt.Sprintf("chunks-census-r%d.json", f.r.index))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(rows, "", "  ")
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	if art, err := artifactOf("chunk_census", path, "分块普查表（角色/编号/标题/内容摘要/表格数）"); err == nil {
		f.r.artifacts = append(f.r.artifacts, art)
	}
	f.r.check("S2", "S2.chunk_census", ">0", strconv.Itoa(len(rows)), len(rows) > 0, "")
	return nil
}

func chunkTableCount(c *db.DocumentChunk) int {
	if c == nil || len(c.Tables) <= 2 {
		return 0
	}
	var tables []json.RawMessage
	_ = json.Unmarshal(c.Tables, &tables)
	return len(tables)
}

func excerpt(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func (f *flow) writeRequirementCensus(view *db.RequirementsView) error {
	rows := []map[string]any{}
	for _, it := range view.Items {
		rows = append(rows, map[string]any{"code": it.Code, "category": it.Category, "title": it.Title,
			"description": it.Description, "eval_kind": it.EvalKind, "score_group": it.ScoreGroup, "score_weight": it.ScoreWeight,
			"proof_rules": it.ProofRules, "superseded": it.IsSuperseded()})
	}
	path := filepath.Join(f.app.cfg.ArtifactsDir, fmt.Sprintf("requirements-census-r%d.json", f.r.index))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(rows, "", "  ")
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	if art, err := artifactOf("requirements_census", path, "要求清单普查（类别/标题/证明规则/评分属性）"); err == nil {
		f.r.artifacts = append(f.r.artifacts, art)
	}
	return nil
}

func sectionForRequirement(det *db.OutlineDetail, reqID int64) *db.ProposalSection {
	if det == nil {
		return nil
	}
	for _, sec := range det.Sections {
		for _, rid := range sec.RequirementIDs {
			if rid == reqID {
				return sec
			}
		}
	}
	return nil
}

func findAdmissibilityItem(run *db.AdmissibilityRun, domain string, evidenceID int64) *db.AdmissibilityItem {
	if run == nil {
		return nil
	}
	for _, it := range run.Items {
		if it.EvidenceDomain == domain && it.EvidenceID == evidenceID {
			return it
		}
	}
	return nil
}

func isErr(err error, target error) bool { return errors.Is(err, target) }
