package main

// 根目录 JSON + Markdown 报告。所有结论固定标注 E 级；延后项逐条 BLOCKED。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const reportSchema = "autobid.e2e.acceptance/v1"

type databaseInfo struct {
	Kind           string `json:"kind"`
	SchemaMigrated bool   `json:"schema_migrated"`
}

type summaryInfo struct {
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Blocked int `json:"blocked"`
}

type reportJSON struct {
	Schema         string       `json:"schema"`
	Level          string       `json:"level"`
	GeneratedAt    string       `json:"generated_at"`
	GitCommit      string       `json:"git_commit"`
	RunID          string       `json:"run_id"`
	GoVersion      string       `json:"go_version"`
	FixtureVersion int          `json:"fixture_version"`
	Database       databaseInfo `json:"database"`
	Summary        summaryInfo  `json:"summary"`
	Stages         []*Stage     `json:"stages"`
	Gates          []*Gate      `json:"gates"`
	Determinism    *Determinism `json:"determinism,omitempty"`
	Deferred       []Deferred   `json:"deferred"`
	Artifacts      []Artifact   `json:"artifacts"`
	Cleanup        CleanupInfo  `json:"cleanup"`
}

var (
	reDSN      = regexp.MustCompile(`(?i)\b(?:postgres|postgresql)://\S+`)
	rePassword = regexp.MustCompile(`(?i)\bpassword\s*=\s*\S+`)
)

// sanitize 移除可能出现的连接串或口令片段，报告与检查详情一律经过此函数。
func sanitize(s string) string {
	s = reDSN.ReplaceAllString(s, "[redacted-dsn]")
	s = rePassword.ReplaceAllString(s, "password=[redacted]")
	return s
}

func buildReport(a *app, r *round, det *Determinism, cleanup CleanupInfo) *reportJSON {
	passed, failed := 0, 0
	for _, s := range r.stages {
		for i := range s.Checks {
			s.Checks[i].Expected = sanitize(s.Checks[i].Expected)
			s.Checks[i].Actual = sanitize(s.Checks[i].Actual)
			s.Checks[i].Detail = sanitize(s.Checks[i].Detail)
			if s.Checks[i].Status == statusPass {
				passed++
			} else {
				failed++
			}
		}
	}
	for _, g := range r.gates {
		g.Observed = sanitize(g.Observed)
		g.Error = sanitize(g.Error)
		for i := range g.Checks {
			g.Checks[i].Expected = sanitize(g.Checks[i].Expected)
			g.Checks[i].Actual = sanitize(g.Checks[i].Actual)
			g.Checks[i].Detail = sanitize(g.Checks[i].Detail)
			if g.Checks[i].Status == statusPass {
				passed++
			} else {
				failed++
			}
		}
	}
	arts := make([]Artifact, len(r.artifacts))
	copy(arts, r.artifacts)
	deferred := deferredItems()
	return &reportJSON{
		Schema: reportSchema, Level: "E", GeneratedAt: time.Now().In(cst).Format(time.RFC3339),
		GitCommit: a.cfg.GitCommit, RunID: a.baseRunID, GoVersion: a.cfg.GoVersion, FixtureVersion: a.golden.FixtureVersion,
		Database: databaseInfo{Kind: "postgresql", SchemaMigrated: true},
		Summary:  summaryInfo{Passed: passed, Failed: failed, Blocked: len(deferred)},
		Stages:   r.stages, Gates: r.gates, Determinism: det, Deferred: deferred, Artifacts: arts, Cleanup: cleanup,
	}
}

func writeReports(reportDir string, rep *reportJSON) error {
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(reportDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(reportDir, "e2e-acceptance-report.json"), append(raw, '\n'), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(reportDir, "E2E_ACCEPTANCE_REPORT.md"), []byte(markdownReport(rep)), 0o644)
}

func mdCell(s string) string { return mdInline(sanitize(s)) }

func statusText(s string) string {
	if s == statusPass {
		return "PASS"
	}
	return "FAIL"
}

func markdownReport(rep *reportJSON) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	w("# E 级端到端验收报告\n\n")
	w("> **结论级别：E（工程实现验收）。** 本报告只证明代码、PostgreSQL 状态机与门禁在固定虚构数据上按设计工作；")
	w("不代表 V 级业务效果（真实招标文件与人工金标准），也不代表 P 级生产启用（权限、租户隔离、预算、监控与人工复核流程）。\n\n")

	// 1. 结论摘要。
	w("## 1. 结论摘要\n\n")
	verdict := "全部检查通过"
	if rep.Summary.Failed > 0 {
		verdict = fmt.Sprintf("存在 %d 项失败检查", rep.Summary.Failed)
	}
	w("| 项目 | 结果 |\n| --- | --- |\n")
	w("| 级别 | E（工程实现） |\n")
	w("| 检查结果 | %s |\n", verdict)
	w("| 通过 / 失败 / 延后 | %d / %d / %d |\n", rep.Summary.Passed, rep.Summary.Failed, rep.Summary.Blocked)
	w("| 主链导出就绪 `ExportReadiness.Ready` | %v |\n", mainReady(rep))
	w("| DOCX 导出任务 | %s |\n", exportSummary(rep))
	w("| 两轮归一化一致性 | %s |\n\n", determinismText(rep.Determinism))

	// 2. 运行环境。
	w("## 2. 运行环境\n\n")
	w("| 项 | 值 |\n| --- | --- |\n")
	w("| 生成时间 | %s |\n", rep.GeneratedAt)
	w("| Git commit | %s |\n", rep.GitCommit)
	w("| Go 版本 | %s |\n", rep.GoVersion)
	w("| 数据库 | %s（schema_migrated=%v；不记录连接串、主机、用户名或口令） |\n", rep.Database.Kind, rep.Database.SchemaMigrated)
	w("| 夹具版本 | %d |\n", rep.FixtureVersion)
	w("| 运行 ID | %s |\n\n", rep.RunID)

	// 3. 主链路阶段表。
	w("## 3. 主链路阶段表\n\n")
	w("| 阶段 | 检查 | 预期 | 实际 | 状态 | 说明 |\n| --- | --- | --- | --- | --- | --- |\n")
	for _, s := range rep.Stages {
		for _, c := range s.Checks {
			w("| %s | %s | %s | %s | %s | %s |\n", mdCell(s.Title), mdCell(c.ID), mdCell(c.Expected), mdCell(c.Actual),
				statusText(c.Status), mdCell(c.Detail))
		}
	}
	w("\n")

	// 4. 黄金期望核对表。
	w("## 4. 黄金期望核对表\n\n")
	w("| 期望项 | 黄金值 | 实际 | 状态 |\n| --- | --- | --- | --- |\n")
	for _, s := range rep.Stages {
		for _, c := range s.Checks {
			if !strings.HasPrefix(c.ID, "golden.") {
				continue
			}
			w("| %s | %s | %s | %s |\n", mdCell(c.ID), mdCell(c.Expected), mdCell(c.Actual), statusText(c.Status))
		}
	}
	w("\n")

	// 5. 门禁矩阵。
	w("## 5. 门禁矩阵\n\n")
	w("| 编号 | 门禁 | 触发阻断 | 观察到的信号 | 修正后放行 | 完整恢复 ready |\n| --- | --- | --- | --- | --- | --- |\n")
	for _, g := range rep.Gates {
		full := ""
		if g.ID == "G06" || g.ID == "G09" || g.ID == "G11" {
			full = fmt.Sprint(g.FullReady)
		}
		w("| %s | %s | %v | %s | %v | %s |\n", g.ID, mdCell(g.Gate), g.Blocked, mdCell(g.Observed), g.Remediated, full)
	}
	w("\n")
	for _, g := range rep.Gates {
		if len(g.Checks) == 0 {
			continue
		}
		w("### %s 检查明细\n\n", g.ID)
		w("| 检查 | 预期 | 实际 | 状态 | 说明 |\n| --- | --- | --- | --- | --- |\n")
		for _, c := range g.Checks {
			w("| %s | %s | %s | %s | %s |\n", mdCell(c.ID), mdCell(c.Expected), mdCell(c.Actual), statusText(c.Status), mdCell(c.Detail))
		}
		w("\n")
	}

	// 6. 导出产物。
	w("## 6. 导出产物\n\n")
	w("| 类型 | 路径 | 大小 | SHA-256 | 说明 |\n| --- | --- | --- | --- | --- |\n")
	for _, a := range rep.Artifacts {
		w("| %s | %s | %d | `%s` | %s |\n", mdCell(a.Kind), mdCell(a.Path), a.Size, a.SHA256, mdCell(a.Note))
	}
	w("\n")
	w("- DOCX 回读：%s。\n", exportReadbackText(rep))
	w("- 目录刷新：%s。\n\n", tocText(rep))

	// 7. 延后项。
	w("## 7. 延后项（BLOCKED）\n\n")
	w("| 能力 | 状态 | 原因 |\n| --- | --- | --- |\n")
	for _, d := range rep.Deferred {
		w("| %s | %s | %s |\n", mdCell(d.ID), strings.ToUpper(d.Status), mdCell(d.Reason))
	}
	w("\n")

	// 8. 复现命令与边界声明。
	w("## 8. 复现命令与边界声明\n\n")
	w("```bash\n")
	w("# 1. 静态检查\ngo vet ./...\n\n")
	w("# 2. 真实 PostgreSQL 测试（连接串只从环境变量读取）\n#    AUTOBID_TEST_DATABASE_URL 已由运行环境提供\ngo test -race ./...\n\n")
	w("# 3. 端到端验收（报告写到项目根目录）\ngo run ./eval/bid/accept -report-dir . -artifacts-dir ./e2e-artifacts -verify-determinism\n```\n\n")
	w("**边界声明**：本执行器直接调用 `db`、`ingest`、`export` 与 `bidtools`，覆盖 Go 后端主链、PostgreSQL 状态机与核验门禁；")
	w("不覆盖 HTTP REST 路由、SSE 与浏览器操作。合成数据结果不代表真实投标文件的写作质量，也不代表抽取准确率或召回率已达到业务门槛。")
	w("执行器只生成本次 E2E 运行证据，不代替外部 `go vet` 与 `go test -race` 结果；这两项须按第 8 节命令在具备真实 PostgreSQL 的环境中单独执行。")
	w("M1 使用确定性替身写作，不调用真实大模型；证据检索与绑定、承诺台账、报价核验、PDF 导出与多 Agent 均列为 BLOCKED。\n\n")

	// 附录：提取普查 + 确定性。
	w("## 附录 A：提取普查表\n\n")
	w("分块普查表随本次运行生成于 `%s`，记录每个分块的编号、标题、内容摘要、表格数与提取出的要求编码。\n\n", artifactPath(rep, "extraction_census"))
	w("## 附录 B：确定性与清理\n\n")
	w("- 两轮归一化比较：%s\n", determinismText(rep.Determinism))
	w("- 任务清理：%s（%d 个任务）。%s\n\n", rep.Cleanup.Status, rep.Cleanup.Tasks, mdCell(rep.Cleanup.Detail))
	w("> 本报告与生成代码中不包含数据库连接串、真实企业、真实项目或个人信息；全部项目与企业名称均为虚构演示标识。\n")
	return b.String()
}

func mainReady(rep *reportJSON) bool {
	for _, s := range rep.Stages {
		for _, c := range s.Checks {
			if c.ID == "main.readiness.ready" {
				return c.Status == statusPass && c.Actual == "true"
			}
		}
	}
	return false
}

func exportSummary(rep *reportJSON) string {
	for _, s := range rep.Stages {
		for _, c := range s.Checks {
			if c.ID == "export.status" {
				if c.Status == statusPass {
					return "succeeded"
				}
				return "failed"
			}
		}
	}
	return "not run / failed"
}

func exportReadbackText(rep *reportJSON) string {
	for _, s := range rep.Stages {
		for _, c := range s.Checks {
			if c.ID == "export.parse" {
				return statusText(c.Status) + "（" + c.Actual + "）"
			}
		}
	}
	return "未执行"
}

func tocText(rep *reportJSON) string {
	for _, s := range rep.Stages {
		for _, c := range s.Checks {
			if c.ID == "export.toc" {
				return c.Actual
			}
		}
	}
	return "未执行"
}

func determinismText(d *Determinism) string {
	if d == nil || !d.Enabled {
		return "未启用（未传 -verify-determinism）"
	}
	if d.Consistent {
		return "一致"
	}
	return "不一致：" + strings.Join(d.Differences, "；")
}

func artifactPath(rep *reportJSON, kind string) string {
	for _, a := range rep.Artifacts {
		if a.Kind == kind {
			return a.Path
		}
	}
	return "（未生成）"
}

// cleanupTasks 精确删除本次运行创建的任务；只依赖任务级 ON DELETE CASCADE。
func cleanupTasks(a *app, taskIDs []int64) CleanupInfo {
	ids := uniqueIDs(taskIDs)
	info := CleanupInfo{Mode: "delete-created-tasks", Tasks: len(ids)}
	if a.cfg.Keep {
		info.Mode, info.Status = "keep", "kept"
		info.Detail = "按 -keep 保留本次创建的全部任务"
		return info
	}
	if len(ids) == 0 {
		info.Status = "removed"
		info.Detail = "本次未创建任务"
		return info
	}
	if _, err := a.d.ExecContext(contextBackground(), `DELETE FROM tasks WHERE id = ANY($1::bigint[])`, pgInt64Text(ids)); err != nil {
		info.Status = "BLOCKED"
		info.Detail = sanitize("删除任务失败：" + err.Error())
		return info
	}
	tables := []string{"tender_documents", "document_chunks", "requirement_manifests", "requirement_items", "proposal_outlines",
		"proposal_sections", "proposal_section_requirements", "section_revisions", "compliance_matrix", "consistency_runs",
		"consistency_findings", "export_jobs", "proposal_charts", "task_scopes", "task_relations", "task_asset_links"}
	var total int64
	for _, table := range tables {
		var n int64
		q := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE task_id = ANY($1::bigint[])`, table)
		if err := a.d.QueryRowContext(contextBackground(), q, pgInt64Text(ids)).Scan(&n); err != nil {
			info.Status = "BLOCKED"
			info.Detail = sanitize(fmt.Sprintf("检查级联清理失败（%s）：%v", table, err))
			return info
		}
		total += n
	}
	info.Status = "removed"
	info.Detail = "已按 tasks.id 精确删除；未 DROP 表、未删除其他任务、未清理全局资产"
	if total > 0 {
		info.Status = "BLOCKED"
		info.Detail = fmt.Sprintf("任务删除后仍有 %d 行任务级数据未级联，未做更大范围删除", total)
	}
	info.Detail += "。推演空间（explorations）不随任务删除，保留为历史记录，不影响下一次运行"
	return info
}

func uniqueIDs(ids []int64) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, id := range ids {
		if id > 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func pgInt64Text(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprint(id)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// 报告中只需要一个短生命周期的背景上下文。
func contextBackground() context.Context { return context.Background() }
