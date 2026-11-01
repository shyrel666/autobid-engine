package main

// D2 验收报告：JSON 与 Markdown（只保留 E 级结论；不记录连接串、真实企业或个人信息）。

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
	Version       int     `json:"version"`
	DeclaredTotal string  `json:"declared_total"`
	Ceiling       string  `json:"ceiling,omitempty"`
	Status        string  `json:"status"`
	Checks        int     `json:"checks"`
	OK            bool    `json:"ok"`
}

type commitmentRow struct {
	Code          string  `json:"code"`
	Metric        string  `json:"metric"`
	Label         string  `json:"metric_label"`
	Threshold     string  `json:"threshold"`
	Committed     string  `json:"committed"`
	Status        string  `json:"status"`
	SignedOffBy   string  `json:"signed_off_by,omitempty"`
	RequiresSignoff bool  `json:"requires_delivery_signoff"`
	Value         float64 `json:"value"`
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

// Report 是 d2-acceptance-report.json 的结构。
type Report struct {
	Schema         string         `json:"schema"`
	Level          string         `json:"level"`
	GeneratedAt    string         `json:"generated_at"`
	GitCommit      string         `json:"git_commit"`
	GoVersion      string         `json:"go_version"`
	FixtureVersion int            `json:"fixture_version"`
	Database       map[string]any `json:"database"`
	Summary        map[string]int `json:"summary"`
	MainReady      bool           `json:"main_ready"`
	Export         string         `json:"export"`
	Stages         []*Stage       `json:"stages"`
	Gates          []*Gate        `json:"gates"`
	EvidenceCensus []censusRow    `json:"evidence_census"`
	Admissibility  []admissibilityRow `json:"admissibility"`
	ObjectiveScores []objectiveRow `json:"objective_scores"`
	Commitments    []commitmentRow `json:"commitments"`
	Quote          *quoteRow       `json:"quote,omitempty"`
	Determinism    *Determinism   `json:"determinism"`
	Deferred       []Deferred     `json:"deferred"`
	Artifacts      []Artifact     `json:"artifacts"`
	Cleanup        CleanupInfo    `json:"cleanup"`
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
		Schema: "autobid.d2.acceptance/v1", Level: "E", GeneratedAt: time.Now().In(beijingZone).Format(time.RFC3339),
		GitCommit: a.cfg.GitCommit, GoVersion: a.cfg.GoVersion, FixtureVersion: a.golden.FixtureVersion,
		Database: map[string]any{"kind": "postgresql", "schema_migrated": true},
		Summary:  map[string]int{"passed": passed, "failed": failed, "blocked": blocked},
		MainReady: first.mainReady, Export: first.exportReadback,
		Stages: stages, Gates: first.gates, EvidenceCensus: a.census, Admissibility: a.mainAdmissibility,
		ObjectiveScores: a.mainObjective, Commitments: a.mainCommitments, Quote: quoteRowOf(a), Determinism: det,
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
		{ID: "D3", Status: "BLOCKED", Reason: "故事板、逐句断言抽取与落证、未绑定断言的句子级阻断属于 D3；D2 只做到章节级证据 / 台账校验。"},
		{ID: "D4", Status: "BLOCKED", Reason: "评分索引表、页码引用、全文件一致性（商务/报价/承诺函/人员表/偏离表）与防雷同属于 D4。"},
		{ID: "V", Status: "BLOCKED", Reason: "真实企业原件、官方登记核验、人工盲评、真实投标结果与准确率 / 召回率需要合作企业与原件，当前不具备。"},
		{ID: "P", Status: "BLOCKED", Reason: "多租户 RBAC、预算、监控与生产试点属于 Phase 7 / P 级。"},
		{ID: "PDF", Status: "BLOCKED", Reason: "PDF 导出与 LibreOffice 目录页码刷新未启用；D2 复用 M1 的 DOCX 导出边界。"},
		{ID: "multi-agent", Status: "BLOCKED", Reason: "多 Agent 检索与写作在无稳定收益证据前保持关闭。"},
		{ID: "quote-optimization", Status: "BLOCKED", Reason: "报价优化、竞争对手情报与投标日历明确不做。"},
		{ID: "api-ui-e2e", Status: "BLOCKED", Reason: "D2 执行器只覆盖 Go 后端主链；API/UI 浏览器端到端验收另行执行。"},
		{ID: "semantic-scope", Status: "BLOCKED", Reason: "“同类项目 / 适用范围”语义判断只记 manual，由人工复核；D2 不引入 LLM 判断。"},
		{ID: "cross-file-price", Status: "BLOCKED", Reason: "技术标与报价的跨文件一致性属于 D4；D2 只做报价内部勾稽与限价。"},
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
	if err := os.WriteFile(filepath.Join(dir, "d2-acceptance-report.json"), append(raw, '\n'), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "D2_ACCEPTANCE_REPORT.md"), []byte(markdownReport(rep)), 0o644)
}

func markdownReport(rep *Report) string {
	var b strings.Builder
	b.WriteString("# D2 验收报告（证据可采性与承诺台账）\n\n")
	fmt.Fprintf(&b, "> **级别：E（工程实现验收）**。本报告只用固定、虚构、可重复生成的数据证明代码、PostgreSQL 状态机、规则与门禁按设计工作；"+
		"不代表 V 级业务效果或 P 级生产可用。生成时间：%s；Git：%s；夹具版本：%d。\n\n", rep.GeneratedAt, rep.GitCommit, rep.FixtureVersion)
	b.WriteString("## 1. 结论摘要\n\n")
	fmt.Fprintf(&b, "- 检查通过 %d，失败 %d，阻断 %d。\n", rep.Summary["passed"], rep.Summary["failed"], rep.Summary["blocked"])
	fmt.Fprintf(&b, "- 主链路 `ExportReadiness.Ready` = **%v**；DOCX 导出与回读：%s。\n", rep.MainReady, firstNonEmpty(rep.Export, "未执行"))
	if rep.Determinism != nil {
		fmt.Fprintf(&b, "- 两轮归一化一致：**%v**（差异 %d 条）。\n", rep.Determinism.Consistent, len(rep.Determinism.Differences))
	} else {
		b.WriteString("- 未启用两轮确定性比较。\n")
	}
	b.WriteString("\n## 2. 运行环境\n\n")
	fmt.Fprintf(&b, "- 日期：%s；Go：%s；数据库：PostgreSQL（已迁移）；规则版本：`%s`。\n", rep.GeneratedAt, rep.GoVersion, compliance.AdmissibilityRuleVersion)
	b.WriteString("- 报告不记录连接串、主机、用户名、口令、真实企业、真实项目或个人信息。\n")

	b.WriteString("\n## 3. 证据普查表\n\n")
	b.WriteString("| 类别 | 编号 | 名称 | 主体 | 有效期 | 原件数 | 原件页角色 |\n| --- | --- | --- | --- | --- | --- | --- |\n")
	for _, row := range rep.EvidenceCensus {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %d | %s |\n", md(row.Category), md(row.Code), md(row.Title), md(row.Subject),
			md(row.ValidUntil), row.Originals, md(row.PageLabels))
	}

	b.WriteString("\n## 4. 主链路阶段（S1~S16）\n\n")
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

	b.WriteString("\n## 5. 可采性规则核对\n\n")
	b.WriteString("要求级运行与逐证据明细（只展示主链；manual 不是通过）：\n\n")
	b.WriteString("| 要求 | 版本 | 要求级结论 | 状态 | MinCount | 逐证据 |\n| --- | --- | --- | --- | --- | --- |\n")
	for _, row := range rep.Admissibility {
		items := []string{}
		for _, it := range row.Items {
			items = append(items, fmt.Sprintf("%s:%d=%s", it.Domain, it.EvidenceID, it.Verdict))
		}
		fmt.Fprintf(&b, "| %s %s | v%d | %s | %s | %s | %s |\n", md(row.RequirementCode), md(row.RequirementTitle), row.RunVersion,
			row.Verdict, row.Status, md(row.MinCount), md(strings.Join(items, "; ")))
	}

	b.WriteString("\n## 6. 客观分、承诺台账与报价\n\n")
	b.WriteString("| 评分项 | 状态 | 得分 | 满分 | 可采证据数 | 缺口 |\n| --- | --- | --- | --- | --- | --- |\n")
	for _, row := range rep.ObjectiveScores {
		fmt.Fprintf(&b, "| %s | %s | %g | %g | %d | %s |\n", md(row.Requirement), row.Status, row.Score, row.MaxScore,
			len(row.EvidenceIDs), md(strings.Join(row.Gaps, ",")))
	}
	b.WriteString("\n| 台账 | 指标 | 阈值 | 承诺 | 状态 | 会签 |\n| --- | --- | --- | --- | --- | --- |\n")
	for _, row := range rep.Commitments {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n", md(row.Code), md(row.Label), md(row.Threshold), md(row.Committed),
			row.Status, md(row.SignedOffBy))
	}
	b.WriteString("\n| 报价版本 | 声明总额 | 最高限价 | 状态 | 逐项检查 | 结论 |\n| --- | --- | --- | --- | --- | --- |\n")
	if rep.Quote != nil {
		fmt.Fprintf(&b, "| v%d | %s | %s | %s | %d | %v |\n", rep.Quote.Version, rep.Quote.DeclaredTotal, md(rep.Quote.Ceiling),
			rep.Quote.Status, rep.Quote.Checks, rep.Quote.OK)
	}
	b.WriteString("\n报价使用 0.0001 定点整数计算：行金额 = 单价 × 数量；声明总额 = 行合计（容差 0.01）；声明总额 ≤ 最高限价；并与价格类承诺台账一致。\n")

	b.WriteString("\n## 7. 门禁矩阵（D2-G01 ~ G14）\n\n")
	b.WriteString("| 门禁 | 信号类型 | sentinel / blocker | 触发 | 观察 | 已修正 | 完整 ready |\n| --- | --- | --- | --- | --- | --- | --- |\n")
	for _, g := range rep.Gates {
		fmt.Fprintf(&b, "| %s %s | %s | %s | %s | %s | %v | %v |\n", g.ID, md(g.Gate), g.SignalType, md(firstNonEmpty(g.Sentinel, "-")),
			md(g.Trigger), md(g.Observed), g.Remediated, g.FullReady)
	}

	b.WriteString("\n## 8. 导出产物\n\n")
	for _, a := range rep.Artifacts {
		if a.Kind == "export_docx" {
			fmt.Fprintf(&b, "- `%s`：%d 字节，SHA-256 `%s`；DOCX 解析回读通过，必要章节标题存在。%s\n", a.Path, a.Size, a.SHA256, a.Note)
		}
	}
	if rep.Export == "" {
		b.WriteString("- 未生成导出产物。\n")
	} else {
		fmt.Fprintf(&b, "- 回读结论：%s。目录未刷新只记 `toc_not_refreshed` 警告，不外推为 PDF 可用。\n", rep.Export)
	}

	b.WriteString("\n## 9. 延后项（BLOCKED）\n\n")
	for _, d := range rep.Deferred {
		fmt.Fprintf(&b, "- **%s**：%s\n", d.ID, d.Reason)
	}

	b.WriteString("\n## 10. 复现命令与边界\n\n")
	b.WriteString("```bash\n")
	b.WriteString("go run ./eval/bid/d2 -report-dir . -artifacts-dir ./d2-artifacts -verify-determinism\n")
	b.WriteString("```\n\n")
	b.WriteString("- 只覆盖 Go 后端主链；HTTP/SSE/UI 浏览器端到端验收未执行（BLOCKED）。\n")
	b.WriteString("- 生成物 `D2_ACCEPTANCE_REPORT.md`、`d2-acceptance-report.json`、`d2-artifacts/` 只保留在本地，不提交 Git。\n")
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
