package main

// D3 验收报告：JSON 与 Markdown（只保留 E 级结论；不记录连接串、真实企业或个人信息）。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/compliance"
	"github.com/autobid/autobid-engine/db"
)

type admissibilityItemRow struct {
	Domain     string `json:"evidence_domain"`
	EvidenceID int64  `json:"evidence_id"`
	Verdict    string `json:"verdict"`
	Checks     int    `json:"checks"`
	Reasons    string `json:"reasons,omitempty"`
}

type admissibilityRow struct {
	RequirementCode  string                 `json:"requirement_code"`
	RequirementTitle string                 `json:"requirement_title"`
	RunVersion       int                    `json:"run_version"`
	Verdict          string                 `json:"verdict"`
	Status           string                 `json:"status"`
	MinCount         string                 `json:"min_count,omitempty"`
	Items            []admissibilityItemRow `json:"items"`
}

type objectiveRow struct {
	Requirement string   `json:"requirement"`
	Status      string   `json:"status"`
	Score       float64  `json:"score"`
	MaxScore    float64  `json:"max_score"`
	EvidenceIDs []int64  `json:"evidence_ids"`
	Gaps        []string `json:"gaps"`
}

type quoteRow struct {
	Version       int    `json:"version"`
	DeclaredTotal string `json:"declared_total"`
	Ceiling       string `json:"ceiling,omitempty"`
	Status        string `json:"status"`
	Checks        int    `json:"checks"`
	OK            bool   `json:"ok"`
}

type commitmentRow struct {
	Code            string  `json:"code"`
	Metric          string  `json:"metric"`
	Label           string  `json:"metric_label"`
	Threshold       string  `json:"threshold"`
	Committed       string  `json:"committed"`
	Status          string  `json:"status"`
	SignedOffBy     string  `json:"signed_off_by,omitempty"`
	RequiresSignoff bool    `json:"requires_delivery_signoff"`
	Value           float64 `json:"value"`
}

type storyboardRow struct {
	SectionCode   string `json:"section_code"`
	SectionTitle  string `json:"section_title"`
	Version       int    `json:"version"`
	Status        string `json:"status"`
	ScoringPoints int    `json:"scoring_points"`
	EvidenceRefs  int    `json:"evidence_refs"`
	CommitRefs    int    `json:"commitment_refs"`
	CreatedBy     string `json:"created_by"`
	ApprovedBy    string `json:"approved_by"`
}

type writingVerificationRow struct {
	SectionCode              string  `json:"section_code"`
	SectionTitle             string  `json:"section_title"`
	RevNo                    int     `json:"rev_no"`
	Verdict                  string  `json:"verdict"`
	TotalSentences           int     `json:"total_sentences"`
	BidderFactSentences      int     `json:"bidder_fact_sentences"`
	CommitmentSentences      int     `json:"commitment_sentences"`
	UngroundedClaimCount     int     `json:"ungrounded_claim_count"`
	FactBindingRate          float64 `json:"fact_binding_rate"`
	CommitmentBindingRate    float64 `json:"commitment_binding_rate"`
	SpecificityRatio         float64 `json:"specificity_ratio"`
	KBSimilarityMax          float64 `json:"kb_similarity_max"`
	BoilerplateSimilarityMax float64 `json:"boilerplate_similarity_max"`
}

func admissibilityRows(runs map[int64]*db.AdmissibilityRun) []admissibilityRow {
	out := []admissibilityRow{}
	for _, run := range runs {
		items := []admissibilityItemRow{}
		minCount := ""
		for _, c := range run.Checks {
			if c.Rule == "min_count" {
				minCount = "expected=" + c.Expected + " actual=" + c.Actual
			}
		}
		for _, it := range run.Items {
			reasons := []string{}
			for _, r := range it.Reasons {
				reasons = append(reasons, r.Code)
			}
			sort.Strings(reasons)
			items = append(items, admissibilityItemRow{Domain: it.EvidenceDomain, EvidenceID: it.EvidenceID, Verdict: it.Verdict,
				Checks: len(it.Checks), Reasons: strings.Join(reasons, ",")})
		}
		sort.Slice(items, func(i, j int) bool {
			if items[i].Domain != items[j].Domain {
				return items[i].Domain < items[j].Domain
			}
			return items[i].EvidenceID < items[j].EvidenceID
		})
		out = append(out, admissibilityRow{RequirementCode: run.RequirementCode, RequirementTitle: run.RequirementTitle,
			RunVersion: run.Version, Verdict: run.Verdict, Status: run.Status, MinCount: minCount, Items: items})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RequirementCode < out[j].RequirementCode })
	return out
}

func objectiveRows(scores []*db.ObjectiveScoreRun) []objectiveRow {
	out := []objectiveRow{}
	for _, s := range scores {
		out = append(out, objectiveRow{Requirement: s.RequirementTitle, Status: s.Status, Score: s.Score, MaxScore: s.MaxScore,
			EvidenceIDs: s.EvidenceIDs, Gaps: s.Gaps})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Requirement < out[j].Requirement })
	return out
}

func commitmentRows(commits map[string]*db.Commitment) []commitmentRow {
	out := []commitmentRow{}
	for _, c := range commits {
		v := 0.0
		if c.CommittedValue != nil {
			v = *c.CommittedValue
		}
		out = append(out, commitmentRow{Code: c.Code, Metric: c.Metric, Label: c.MetricLabel,
			Threshold: c.ThresholdCmp + " " + c.TenderThresholdRaw, Committed: c.CommittedRaw, Status: c.Status,
			SignedOffBy: c.SignedOffBy, RequiresSignoff: c.RequiresDeliverySignoff, Value: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metric < out[j].Metric })
	return out
}

func storyboardRows(sbs []*db.SectionStoryboard) []storyboardRow {
	out := make([]storyboardRow, 0, len(sbs))
	for _, sb := range sbs {
		out = append(out, storyboardRow{
			SectionCode:   sb.SectionCode,
			SectionTitle:  sb.SectionTitle,
			Version:       sb.Version,
			Status:        sb.Status,
			ScoringPoints: len(sb.ScoringPoints),
			EvidenceRefs:  len(sb.AdmissibilityItemIDs) + len(sb.EvidenceIDs),
			CommitRefs:    len(sb.CommitmentIDs),
			CreatedBy:     sb.CreatedBy,
			ApprovedBy:    sb.ApprovedBy,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SectionCode < out[j].SectionCode })
	return out
}

func writingVerificationRows(wvs []*db.SectionWritingVerification) []writingVerificationRow {
	out := make([]writingVerificationRow, 0, len(wvs))
	for _, wv := range wvs {
		out = append(out, writingVerificationRow{
			SectionCode:              wv.SectionCode,
			SectionTitle:             wv.SectionTitle,
			RevNo:                    wv.RevNo,
			Verdict:                  wv.Verdict,
			TotalSentences:           wv.Metrics.TotalSentences,
			BidderFactSentences:      wv.Metrics.BidderFactSentences,
			CommitmentSentences:      wv.Metrics.CommitmentSentences,
			UngroundedClaimCount:     wv.UngroundedClaimCount,
			FactBindingRate:          wv.FactBindingRate,
			CommitmentBindingRate:    wv.CommitmentBindingRate,
			SpecificityRatio:         wv.SpecificityRatio,
			KBSimilarityMax:          wv.KBSimilarityMax,
			BoilerplateSimilarityMax: wv.BoilerplateSimilarityMax,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SectionCode < out[j].SectionCode })
	return out
}

type Report struct {
	Schema               string                   `json:"schema"`
	Level                string                   `json:"level"`
	GeneratedAt          string                   `json:"generated_at"`
	GitCommit            string                   `json:"git_commit"`
	GoVersion            string                   `json:"go_version"`
	FixtureVersion       int                      `json:"fixture_version"`
	Database             map[string]any           `json:"database"`
	Summary              map[string]int           `json:"summary"`
	MainReady            bool                     `json:"main_ready"`
	Export               string                   `json:"export"`
	Stages               []*Stage                 `json:"stages"`
	Gates                []*Gate                  `json:"gates"`
	EvidenceCensus       []censusRow              `json:"evidence_census"`
	Admissibility        []admissibilityRow       `json:"admissibility"`
	ObjectiveScores      []objectiveRow           `json:"objective_scores"`
	Commitments          []commitmentRow          `json:"commitments"`
	Storyboards          []storyboardRow          `json:"storyboards"`
	WritingVerifications []writingVerificationRow `json:"writing_verifications"`
	Quote                *quoteRow                `json:"quote,omitempty"`
	Determinism          *Determinism             `json:"determinism"`
	Deferred             []Deferred               `json:"deferred"`
	Artifacts            []Artifact               `json:"artifacts"`
	Cleanup              CleanupInfo              `json:"cleanup"`
}

func buildReport(a *app, first *round, det *Determinism, cleanup CleanupInfo) *Report {
	passed, failed := first.summary()
	blocked := 0
	for _, g := range first.gates {
		if g.Error != "" || !g.Remediated {
			blocked++
		}
	}
	if cleanup.Status != statusPass {
		blocked++
	}
	stages := append([]*Stage{}, first.stages...)
	s16 := &Stage{ID: "S16", Title: "清理与回归", Status: statusPass, Checks: []Check{}}
	c := Check{ID: "S16.cleanup", Expected: "no residual tasks or tenant evidence", Actual: cleanup.Detail, Status: statusPass}
	if cleanup.Status != statusPass {
		c.Status, s16.Status = statusFail, statusFail
	}
	s16.Checks = append(s16.Checks, c)
	stages = append(stages, s16)
	return &Report{
		Schema: "autobid.d3.acceptance/v1", Level: "E", GeneratedAt: time.Now().In(beijingZone).Format(time.RFC3339),
		GitCommit: a.cfg.GitCommit, GoVersion: a.cfg.GoVersion, FixtureVersion: a.golden.FixtureVersion,
		Database: map[string]any{"kind": "postgresql", "schema_migrated": true},
		Summary:  map[string]int{"passed": passed, "failed": failed, "blocked": blocked},
		MainReady: first.mainReady, Export: first.exportReadback,
		Stages: stages, Gates: first.gates, EvidenceCensus: a.census, Admissibility: a.mainAdmissibility,
		ObjectiveScores: a.mainObjective, Commitments: a.mainCommitments,
		Storyboards: a.mainStoryboards, WritingVerifications: a.mainWritingVerifs,
		Quote: quoteRowOf(a), Determinism: det,
		Deferred: deferredItems(), Artifacts: first.artifacts, Cleanup: cleanup,
	}
}

func quoteRowOf(a *app) *quoteRow {
	if a.mainQuote == nil {
		return nil
	}
	ceiling := ""
	if a.mainQuote.Ceiling != nil {
		ceiling = *a.mainQuote.Ceiling
	}
	row := &quoteRow{Version: a.mainQuote.Version, DeclaredTotal: a.mainQuote.DeclaredTotal, Ceiling: ceiling, Status: a.mainQuote.Status}
	if a.mainQuoteCheck != nil {
		row.Checks, row.OK = len(a.mainQuoteCheck.Checks), a.mainQuoteCheck.OK
	}
	return row
}

func deferredItems() []Deferred {
	return []Deferred{
		{ID: "D4", Status: "BLOCKED", Reason: "评分索引表、页码引用、全文件一致性（商务/报价/承诺函/人员表/偏离表）与防雷同属于 D4。"},
		{ID: "V", Status: "BLOCKED", Reason: "真实企业原件、真实历史标书盲评、评委打分提升与真实投标结果验证属于 V 级，当前无脱敏真实标书集。"},
		{ID: "P", Status: "BLOCKED", Reason: "多租户 RBAC、预算、监控与生产试点属于 Phase 7 / P 级。"},
		{ID: "PDF", Status: "BLOCKED", Reason: "PDF 导出与 文档引擎 目录页码刷新未启用；D3 复用 M1/D2 的 DOCX 导出边界。"},
		{ID: "multi-agent", Status: "BLOCKED", Reason: "多 Agent 检索与写作在无稳定收益证据前保持关闭。"},
		{ID: "api-ui-e2e", Status: "BLOCKED", Reason: "D3 执行器只覆盖 Go 后端主链；HTTP/UI 浏览器端到端验收另行执行。"},
	}
}

func exportSummary(rep *Report) string {
	if rep == nil || rep.Export == "" {
		return "not-run"
	}
	return rep.Export
}

func writeReports(dir string, rep *Report) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "d3-acceptance-report.json"), append(raw, '\n'), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "D3_ACCEPTANCE_REPORT.md"), []byte(markdownReport(rep)), 0o644)
}

func markdownReport(rep *Report) string {
	var b strings.Builder
	b.WriteString("# D3 验收报告（故事板、句子级证据绑定与写作核验 E 级指标）\n\n")
	fmt.Fprintf(&b, "> **级别：E（工程实现验收）**。本报告只用固定、虚构、可重复生成的数据证明代码、PostgreSQL 状态机、受控引用渲染与句子级写作核验门禁按设计工作；"+
		"不代表 V 级业务效果或 P 级生产可用。生成时间：%s；Git：%s；夹具版本：%d。\n\n", rep.GeneratedAt, rep.GitCommit, rep.FixtureVersion)
	b.WriteString("## 1. 结论摘要\n\n")
	fmt.Fprintf(&b, "- 检查通过 %d，失败 %d，阻断 %d。\n", rep.Summary["passed"], rep.Summary["failed"], rep.Summary["blocked"])
	fmt.Fprintf(&b, "- 主链路 `ExportReadiness.Ready` = **%v**；DOCX 导出与回读：%s。\n", rep.MainReady, firstNonEmpty(rep.Export, "未执行"))
	if rep.Determinism != nil {
		fmt.Fprintf(&b, "- 两轮归一化一致：**%v**（差异 %d 条，第二轮失败 %d 项）。\n", rep.Determinism.Consistent, len(rep.Determinism.Differences), rep.Determinism.SecondFailed)
	} else {
		b.WriteString("- 未启用两轮确定性比较。\n")
	}
	b.WriteString("\n## 2. 运行环境\n\n")
	fmt.Fprintf(&b, "- 日期：%s；Go：%s；数据库：PostgreSQL（已迁移）；写作核验规则版本：`%s`。\n",
		rep.GeneratedAt, rep.GoVersion, compliance.WritingVerificationRuleVersion)
	b.WriteString("- 报告不记录连接串、主机、用户名、口令、真实企业、真实项目或个人信息。\n")

	b.WriteString("\n## 3. 主链路阶段（S1~S16）\n\n")
	b.WriteString("| 阶段 | 状态 | 检查 |\n| --- | --- | --- |\n")
	for _, s := range rep.Stages {
		fails := []string{}
		for _, c := range s.Checks {
			if c.Status != statusPass {
				fails = append(fails, fmt.Sprintf("%s: expected=%s actual=%s", c.ID, c.Expected, c.Actual))
			}
		}
		detail := fmt.Sprintf("%d 项通过", len(s.Checks)-len(fails))
		if len(fails) > 0 {
			detail = strings.Join(fails, "; ")
		}
		fmt.Fprintf(&b, "| %s %s | %s | %s |\n", s.ID, md(s.Title), s.Status, md(detail))
	}

	b.WriteString("\n## 4. 章节故事板与粉队评审（Pink Team）\n\n")
	b.WriteString("| 章节 | 版本 | 状态 | 得分点数 | 证据白名单数 | 承诺白名单数 | 作者 | 粉队批准人 |\n| --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, sb := range rep.Storyboards {
		fmt.Fprintf(&b, "| %s %s | v%d | %s | %d | %d | %d | %s | %s |\n",
			md(sb.SectionCode), md(sb.SectionTitle), sb.Version, sb.Status, sb.ScoringPoints, sb.EvidenceRefs, sb.CommitRefs,
			md(sb.CreatedBy), md(sb.ApprovedBy))
	}

	b.WriteString("\n## 5. 句子级写作核验 E 级指标\n\n")
	b.WriteString("| 章节 | 修订 | 结论 | 总句数 | 事实句 | 承诺句 | 无证据断言数 | 事实绑定率 | 承诺绑定率 | 针对性占比 | 套话最大相似度 |\n| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, wv := range rep.WritingVerifications {
		fmt.Fprintf(&b, "| %s %s | r%d | %s | %d | %d | %d | %d | %.0f%% | %.0f%% | %.1f%% | %.1f%% |\n",
			md(wv.SectionCode), md(wv.SectionTitle), wv.RevNo, wv.Verdict, wv.TotalSentences, wv.BidderFactSentences,
			wv.CommitmentSentences, wv.UngroundedClaimCount, wv.FactBindingRate*100, wv.CommitmentBindingRate*100,
			wv.SpecificityRatio*100, wv.BoilerplateSimilarityMax*100)
	}

	b.WriteString("\n## 6. 门禁矩阵（D3-G01 ~ G10）\n\n")
	b.WriteString("| 门禁 | 信号类型 | sentinel / blocker | 触发 | 观察 | 已修正 | 完整 ready |\n| --- | --- | --- | --- | --- | --- | --- |\n")
	for _, g := range rep.Gates {
		fmt.Fprintf(&b, "| %s %s | %s | %s | %s | %s | %v | %v |\n", g.ID, md(g.Gate), g.SignalType, md(firstNonEmpty(g.Sentinel, "-")),
			md(g.Trigger), md(g.Observed), g.Remediated, g.FullReady)
	}

	b.WriteString("\n## 7. 延后项（BLOCKED）\n\n")
	for _, d := range rep.Deferred {
		fmt.Fprintf(&b, "- **%s**：%s\n", d.ID, d.Reason)
	}

	b.WriteString("\n## 8. 复现命令与边界\n\n")
	b.WriteString("```bash\n")
	b.WriteString("go run ./eval/bid/d3 -report-dir . -artifacts-dir ./d3-artifacts -verify-determinism\n")
	b.WriteString("```\n\n")
	b.WriteString("- 只覆盖 Go 后端主链；HTTP/SSE/UI 浏览器端到端验收未执行（BLOCKED）。\n")
	b.WriteString("- 生成物 `D3_ACCEPTANCE_REPORT.md`、`d3-acceptance-report.json`、`d3-artifacts/` 只保留在本地，不提交 Git。\n")
	return b.String()
}

func md(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if s == "" {
		return "-"
	}
	return s
}
