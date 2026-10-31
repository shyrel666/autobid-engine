package main

// E 级端到端验收执行器。直接调用 Go 业务接口，不经过 HTTP / SSE / 浏览器。
// 退出码：0 全部检查通过；1 有检查失败；2 环境不满足（数据库未配置、迁移失败、夹具版本不符）；3 未预期的内部错误。

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/export"
)

func main() { os.Exit(run()) }

func run() int {
	var (
		reportDir      = flag.String("report-dir", ".", "报告目录（默认项目根目录）")
		artifactsDir   = flag.String("artifacts-dir", "./e2e-artifacts", "夹具与导出产物目录")
		fixtureVersion = flag.Int("fixture-version", 1, "夹具版本，须与 golden.json 一致")
		verifyDet      = flag.Bool("verify-determinism", false, "同一进程内完整执行两轮并比较归一化结果")
		keep           = flag.Bool("keep", false, "保留本次创建的任务与数据（默认清理）")
		goldenPath     = flag.String("golden", filepath.Join("eval", "bid", "fixtures", "golden.json"), "黄金期望文件")
		companyDir     = flag.String("company-fixtures", filepath.Join("eval", "bid", "fixtures", "company"), "虚构企业资料目录（M1 只生成不消费）")
	)
	flag.Parse()

	dsn := strings.TrimSpace(os.Getenv("AUTOBID_TEST_DATABASE_URL"))
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "需要真实 PostgreSQL：请设置 AUTOBID_TEST_DATABASE_URL；未设置时不跳过、不假装通过。")
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pg, err := db.Open(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "打开 PostgreSQL 失败（连接串不写入报告）。")
		return 2
	}
	defer pg.Close()
	if err := pg.Migrate(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "数据库迁移失败（连接串不写入报告）。")
		return 2
	}

	golden, err := loadGolden(*goldenPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取黄金期望失败：%v\n", err)
		return 3
	}
	if *fixtureVersion != golden.FixtureVersion {
		fmt.Fprintf(os.Stderr, "夹具版本不一致：参数 %d，golden.json %d\n", *fixtureVersion, golden.FixtureVersion)
		return 2
	}

	fixtures, err := generateFixtures(*artifactsDir, *companyDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "生成确定性夹具失败：%v\n", err)
		return 3
	}

	absArtifacts, err := filepath.Abs(*artifactsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "解析产物目录失败：%v\n", err)
		return 3
	}
	a := &app{
		cfg:       appConfig{ArtifactsDir: *artifactsDir, GitCommit: gitCommit(), GoVersion: runtime.Version(), Keep: *keep},
		d:         pg,
		golden:    golden,
		fixtures:  fixtures,
		baseRunID: "E2E-" + time.Now().Format("20060102-150405"),
	}
	a.exportSvc = &export.Service{DB: pg, Store: &export.Store{Root: filepath.Join(absArtifacts, "export-store")}, Office: nil,
		OfficeReason: "E2E 验收按设计不启用 LibreOffice：目录页码刷新与 PDF 导出为 BLOCKED，DOCX 导出设置 Word 打开时更新域"}

	first := a.runRound(ctx, 1)
	a.addFixtureArtifacts(first, *companyDir)

	var det *Determinism
	tasks := append([]int64{}, first.tasks...)
	if *verifyDet {
		second := a.runRound(ctx, 2)
		det = &Determinism{Enabled: true, Differences: compareRounds(first.normalized(), second.normalized())}
		det.Consistent = len(det.Differences) == 0
		tasks = append(tasks, second.tasks...)
	}

	cleanup := cleanupTasks(a, tasks)
	rep := buildReport(a, first, det, cleanup)
	if err := writeReports(*reportDir, rep); err != nil {
		fmt.Fprintf(os.Stderr, "写入报告失败：%v\n", err)
		return 3
	}

	passed, failed := first.summary()
	fmt.Printf("E 级端到端验收：通过 %d，失败 %d，延后 %d；主链 ready=%v，DOCX=%s；报告 %s\n",
		passed, failed, len(rep.Deferred), mainReady(rep), exportSummary(rep),
		filepath.Join(*reportDir, "E2E_ACCEPTANCE_REPORT.md"))
	if failed > 0 {
		return 1
	}
	return 0
}

// runRound 执行一轮完整主链路与 G01 ~ G13，每个场景使用独立任务。
func (a *app) runRound(ctx context.Context, index int) *round {
	r := newRound(index)
	a.runID = fmt.Sprintf("%s-r%d", a.baseRunID, index)
	f := &flow{app: a, r: r, runID: a.runID}
	a.runMain(ctx, f)
	a.runGates(ctx, f)
	return r
}

func (a *app) addFixtureArtifacts(r *round, companyDir string) {
	for _, spec := range []struct {
		kind, path, note string
	}{
		{"tender_docx", a.fixtures.TenderPath, "确定性生成的虚构招标 DOCX"},
		{"correction_docx", a.fixtures.CorrectionPath, "确定性生成的虚构更正公告 DOCX"},
		{"g09_docx", a.fixtures.G09Path, "G09 一致性冲突专用夹具 DOCX"},
		{"company_profile", filepath.Join(companyDir, "company-profile.json"), "M1 只生成不消费的虚构企业资料"},
		{"company_credentials", filepath.Join(companyDir, "credentials.json"), "M1 只生成不消费的虚构资质资料"},
		{"company_performance", filepath.Join(companyDir, "performance.json"), "M1 只生成不消费的虚构业绩资料"},
	} {
		art, err := artifactOf(spec.kind, spec.path, spec.note)
		if err != nil {
			r.check("main.setup", "artifact."+spec.kind, "written", err.Error(), false, "")
			continue
		}
		r.artifacts = append(r.artifacts, art)
	}
}

func gitCommit() string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}
