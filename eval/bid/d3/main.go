package main

// D3 E 级验收执行器：直接调用 Go 业务接口，不经过 HTTP / SSE / 浏览器。
// 覆盖：章节故事板（Pink Team 粉队评审）、受控引用标记确定性渲染、句子级断言抽取与核验、红队独立定稿与失效级联。
// 退出码：0 全部通过；1 有检查失败；2 环境不满足（数据库未配置、迁移失败、夹具版本不符、golden 未冻结）；
// 3 未预期的内部错误。

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
	"github.com/autobid/autobid-engine/evidence"
	"github.com/autobid/autobid-engine/export"
)

func main() { os.Exit(run()) }

func run() int {
	var (
		reportDir      = flag.String("report-dir", ".", "报告目录（默认项目根目录）")
		artifactsDir   = flag.String("artifacts-dir", "./d3-artifacts", "夹具、原件与导出产物目录（不提交 Git）")
		fixtureVer     = flag.Int("fixture-version", fixtureVersion, "夹具版本，须与 golden.json 一致")
		verifyDet      = flag.Bool("verify-determinism", false, "同一进程内完整执行两轮并比较归一化结果")
		keep           = flag.Bool("keep", false, "保留本次创建的任务与企业证据（默认清理）")
		goldenPath     = flag.String("golden", filepath.Join("eval", "bid", "fixtures", "d3", "golden.json"), "黄金期望文件")
		defectsPath    = flag.String("defects", filepath.Join("eval", "bid", "fixtures", "d3", "defects.json"), "缺陷库")
		companyDirFlag = flag.String("company-fixtures", filepath.Join("eval", "bid", "fixtures", "d3", "company"), "虚构企业资料目录")
	)
	flag.Parse()

	dsn := strings.TrimSpace(os.Getenv("AUTOBID_TEST_DATABASE_URL"))
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "需要真实 PostgreSQL：请设置 AUTOBID_TEST_DATABASE_URL；未设置时不跳过、不假装通过。")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
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
		return 2
	}
	if *fixtureVer != golden.FixtureVersion {
		fmt.Fprintf(os.Stderr, "夹具版本不一致：参数 %d，golden.json %d\n", *fixtureVer, golden.FixtureVersion)
		return 2
	}
	defects, err := loadDefects(*defectsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取缺陷库失败：%v\n", err)
		return 3
	}
	fixtures, err := generateFixtures(*artifactsDir, *companyDirFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "生成确定性夹具失败：%v\n", err)
		return 3
	}
	absArtifacts, err := filepath.Abs(*artifactsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "解析产物目录失败：%v\n", err)
		return 3
	}
	evDir := filepath.Join(absArtifacts, "evidence")
	if err := os.Setenv("AUTOBID_EVIDENCE_DIR", evDir); err != nil {
		fmt.Fprintf(os.Stderr, "设置原件目录失败：%v\n", err)
		return 3
	}
	a := &app{
		cfg:       appConfig{ArtifactsDir: *artifactsDir, GitCommit: gitCommit(), GoVersion: runtime.Version(), Keep: *keep},
		d:         pg,
		golden:    golden,
		defects:   defects,
		fixtures:  fixtures,
		store:     evidence.New(evDir),
		baseRunID: "D3-" + time.Now().Format("20060102-150405"),
	}
	a.exportSvc = &export.Service{DB: pg, Store: &export.Store{Root: filepath.Join(absArtifacts, "export-store")}, Office: nil,
		OfficeReason: "D3 验收按设计不启用 LibreOffice：目录页码刷新与 PDF 导出为 BLOCKED，DOCX 导出设置 Word 打开时更新域"}

	first := a.runRound(ctx, 1)
	a.addFixtureArtifacts(first, *companyDirFlag)
	var det *Determinism
	tasks := append([]int64{}, first.tasks...)
	tenants := append([]string{}, first.tenants...)
	if *verifyDet {
		second := a.runRound(ctx, 2)
		det = &Determinism{Enabled: true, Differences: compareRounds(first.normalized(), second.normalized())}
		det.Consistent = len(det.Differences) == 0
		tasks = append(tasks, second.tasks...)
		tenants = append(tenants, second.tenants...)
	}
	cleanup := cleanupRun(a, tasks, tenants)
	first.cleanup = cleanup
	rep := buildReport(a, first, det, cleanup)
	if err := writeReports(*reportDir, rep); err != nil {
		fmt.Fprintf(os.Stderr, "写入报告失败：%v\n", err)
		return 3
	}
	passed, failed := first.summary()
	fmt.Printf("D3 E 级验收：通过 %d，失败 %d，延后 %d；主链 ready=%v，DOCX=%s；报告 %s\n",
		passed, failed, len(rep.Deferred), rep.MainReady, exportSummary(rep), filepath.Join(*reportDir, "D3_ACCEPTANCE_REPORT.md"))
	if !cleanupOK(cleanup) {
		fmt.Printf("清理未完成：%s\n", cleanup.Detail)
		return 1
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func (a *app) runRound(ctx context.Context, index int) *round {
	r := newRound(index)
	a.runID = fmt.Sprintf("%s-r%d", a.baseRunID, index)
	f := &flow{app: a, r: r, runID: a.runID}
	mctx, mcancel := context.WithTimeout(ctx, 120*time.Second)
	a.runMain(mctx, f)
	mcancel()
	a.runGates(ctx, f)
	return r
}

func (a *app) addFixtureArtifacts(r *round, companyDir string) {
	for _, spec := range []struct {
		kind, path, note string
	}{
		{"tender_docx", filepath.Join(a.cfg.ArtifactsDir, "fixtures", "tender.docx"), "确定性生成的虚构招标 DOCX"},
		{"correction_docx", filepath.Join(a.cfg.ArtifactsDir, "fixtures", "correction.docx"), "确定性生成的虚构更正 DOCX"},
		{"company_credentials", filepath.Join(companyDir, "credentials.json"), "虚构企业证据与主体关系"},
		{"company_performance", filepath.Join(companyDir, "performance.json"), "虚构业绩证据"},
		{"company_originals", filepath.Join(companyDir, "originals.json"), "虚构原件登记与页码"},
	} {
		art, err := artifactOf(spec.kind, spec.path, spec.note)
		if err != nil {
			r.check("setup", "artifact."+spec.kind, "written", err.Error(), false, "")
			continue
		}
		r.artifacts = append(r.artifacts, art)
	}
}

func cleanupOK(c CleanupInfo) bool {
	return c.Status == "pass"
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
