package export

// D4 集成测试（真实 PostgreSQL + 本机文档引擎 WPS / Word，未安装时跳过并写明缺少的依赖）：
//   - 带评分项、已确认承诺台账与可采企业证据的标书导出严格最终稿（PDF + DOCX）；
//   - 成稿里有《评分索引表》、章节开头的「响应评分项」、偏离表页码列、《承诺事项一览表》与《证明材料索引》；
//   - 每一处章节页码引用都由引擎刷新成数字，并与同一次排版的 PDF 中标题实际所在页一致；
//   - DOCX 与 PDF 的文档属性只含投标人名称，不带平台字样、不带本机办公软件账号；
//   - 回读自检结果随作业落库，提交检查清单据此判定系统项；人工确认后清单就绪，正文再改则成稿过期。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/db/dbtest"
	"github.com/autobid/autobid-engine/ingest"
)

var reAnchor = regexp.MustCompile(`p\d+-[0-9a-f]{6,}`)

func tableWithHeader(tables []*ingest.Table, header []string) *ingest.Table {
	for _, tb := range tables {
		if len(tb.Grid) > 0 && slices.Equal(tb.Grid[0], header) {
			return tb
		}
	}
	return nil
}

func TestD4FinalDraftWithRealEngine(t *testing.T) {
	d := dbtest.Open(t)
	office := requireOffice(t)
	ctx := context.Background()
	p, app := dbtest.ReadyWithAppendices(t, d, t.TempDir())
	svc := &Service{DB: d, Store: &Store{Root: t.TempDir()}, Renderer: stubRenderer{}, Office: office}
	const bidder = "示例科技有限公司"

	sub, err := svc.SubmitExport(ctx, db.DefaultTenant, p.TaskID, "exporter", finalRequest("d4-final", db.ExportPDF, db.ExportDOCX))
	if err != nil || sub.Request.PolicyVersion != db.ExportPolicyFinalV2 {
		t.Fatalf("SubmitExport = %+v %v", sub, err)
	}
	pkg := sub.Jobs[0].Package
	if len(pkg.ScoreIndex) != 3 || len(pkg.Commitments) != 2 || len(pkg.Evidence) != 1 {
		t.Fatalf("package snapshot = %d score rows, %d commitments, %d evidence", len(pkg.ScoreIndex), len(pkg.Commitments), len(pkg.Evidence))
	}
	runAll(t, &Runner{Service: svc, Owner: "d4-e2e", Lease: 5 * time.Minute, TaskID: p.TaskID})
	jobs := map[string]*db.ExportJob{}
	for _, j := range sub.Jobs {
		j = reloadJob(t, d, p.TaskID, j.ID)
		if j.Status != db.ExportSucceeded || j.BusinessState != db.ExportFinalReady {
			t.Fatalf("job %d (%s) = %s/%s %s", j.ID, j.Format, j.Status, j.BusinessState, j.Error)
		}
		jobs[j.Format] = j
	}

	// 回读自检结果已落库：页码引用全部与 PDF 版面核对一致，文档属性已检查。
	// 引用数：评分索引 3 + 偏离表每行 1 + 承诺 2 + 证明材料 1。
	wantRefs := 3 + len(sub.Jobs[0].Matrix) + 2 + 1
	for f, j := range jobs {
		sc := j.SelfCheck
		if sc == nil {
			t.Fatalf("%s job has no self check", f)
		}
		if sc.ScoreRows != 3 || sc.PageRefs != wantRefs || sc.PageRefsVerified != wantRefs || sc.PageRefIssues != 0 || sc.VerifiedAgainst != "pdf" ||
			sc.TOCIssues != 0 || sc.TOCEntries == 0 || !sc.LayoutChecked || sc.Pages == 0 || !sc.MetadataChecked || sc.MetadataIssues != 0 ||
			sc.Author != bidder {
			t.Fatalf("%s self check = %+v (want %d page refs verified against the PDF)", f, *sc, wantRefs)
		}
	}

	// 直接回读产物，不依赖作业里记的结论。
	tpl, _ := DefaultTemplate()
	pdf, docx := artifact(t, svc, jobs[db.ExportPDF]), artifact(t, svc, jobs[db.ExportDOCX])
	if dir := os.Getenv("AUTOBID_D4_KEEP_DIR"); dir != "" { // 留一份成稿供人工查看版式（验收记录用）
		for name, data := range map[string][]byte{"d4-final.pdf": pdf, "d4-final.docx": docx} {
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
				t.Fatalf("keep artifact: %v", err)
			}
		}
	}
	rep, err := CheckPDF(pdf, tpl.Geometry)
	if err != nil || len(rep.Issues) != 0 {
		t.Fatalf("PDF readback: %+v %v", rep.Issues, err)
	}
	refs, marks, err := ReadPageRefs(docx)
	if err != nil || len(refs) != wantRefs || len(marks) != len(p.Spec.Sections) {
		t.Fatalf("DOCX page refs = %d, bookmarks = %d, %v", len(refs), len(marks), err)
	}
	pages := map[string]int{}
	for _, r := range refs {
		n, err := strconv.Atoi(r.Result)
		if err != nil || n <= 0 {
			t.Fatalf("page reference to %s was not refreshed: %q", r.Bookmark, r.Result)
		}
		title := marks[r.Bookmark]
		if actual := rep.HeadingPage(title); actual != n {
			t.Fatalf("page reference to %q says page %d, the PDF has the heading on page %d", title, n, actual)
		}
		pages[title] = n
	}
	// 引用确实落在不同的页上（都指向同一页的话核对没有意义）。
	distinct := map[int]bool{}
	for _, n := range pages {
		distinct[n] = true
	}
	if len(distinct) < 4 {
		t.Fatalf("page references point at %d distinct pages: %v", len(distinct), pages)
	}
	// 第 4 章的标题一行排不下（标题、目录、评分索引表里都折行）：页码引用同样核对到了它实际所在的页。
	long := "4 " + dbtest.LongSectionTitle
	if pages[long] == 0 || pages[long] != rep.HeadingPage(long) || pages[long] <= pages["3 质保与售后"] {
		t.Fatalf("long heading %q: page reference %d, heading on page %d", long, pages[long], rep.HeadingPage(long))
	}

	// 附表内容。
	drep, err := CheckDOCX(ctx, docx)
	if err != nil || len(drep.Issues) != 0 {
		t.Fatalf("DOCX readback: %+v %v", drep, err)
	}
	score := tableWithHeader(drep.Tables, ScoreIndexHeader)
	if score == nil || score.Rows != 4 || !strings.Contains(score.Grid[3][3], dbtest.LongSectionTitle) || score.Grid[3][4] != strconv.Itoa(pages[long]) || !strings.Contains(score.Grid[1][1], "S1 技术方案") || score.Grid[1][2] != "10" ||
		!strings.Contains(score.Grid[1][3], "1 总体技术方案") || score.Grid[1][4] != strconv.Itoa(pages["1 总体技术方案"]) ||
		!strings.Contains(score.Grid[2][1], "S2 质保期承诺") || score.Grid[2][4] != strconv.Itoa(pages["3 质保与售后"]) {
		t.Fatalf("score index table = %+v", score)
	}
	commit := tableWithHeader(drep.Tables, CommitmentHeader)
	if commit == nil || commit.Rows != 3 {
		t.Fatalf("commitment table = %+v", commit)
	}
	var warranty []string
	for _, row := range commit.Grid[1:] {
		switch row[1] {
		case "免费质保期":
			warranty = row
		case "总工期":
			if row[3] != "90 天" { // 承诺原文自带单位：不再重复拼一次单位
				t.Fatalf("duration commitment cell = %q", row[3])
			}
		}
	}
	if warranty == nil || warranty[2] != "不少于 3 年" || warranty[3] != "5 年" || warranty[4] != "S2" || !strings.Contains(warranty[5], "3 质保与售后") ||
		warranty[6] != strconv.Itoa(pages["3 质保与售后"]) {
		t.Fatalf("warranty commitment row = %q", warranty)
	}
	ev := tableWithHeader(drep.Tables, EvidenceIndexHeader)
	if ev == nil || ev.Rows != 2 || ev.Grid[1][1] != app.CredentialName || ev.Grid[1][2] != app.CertificateNo || ev.Grid[1][3] != bidder ||
		!strings.Contains(ev.Grid[1][4], "证书 第 1-2 页") || ev.Grid[1][5] != "★1" || !strings.Contains(ev.Grid[1][6], "2 资格证明") ||
		ev.Grid[1][7] != strconv.Itoa(pages["2 资格证明"]) {
		t.Fatalf("evidence index table = %+v", ev)
	}
	dev := tableWithHeader(drep.Tables, DeviationHeader)
	if dev == nil || dev.Rows-1 != len(sub.Jobs[0].Matrix) {
		t.Fatalf("deviation table = %+v", dev)
	}
	for _, row := range dev.Grid[1:] {
		if n, err := strconv.Atoi(row[5]); err != nil || n <= 0 {
			t.Fatalf("deviation row without a page number: %q", row)
		}
		// 有证明材料的条款（★1）指向证明材料索引的序号，其余条款不带这一行。
		if has := strings.Contains(row[6], "证明材料：见《证明材料索引》第 1 项"); has != (row[1] == "★1") {
			t.Fatalf("deviation row %s evidence pointer = %v: %q", row[1], has, row[6])
		}
		if reAnchor.MatchString(row[4]) { // 内部段落锚点（p<序号>-<哈希>）不进成稿
			t.Fatalf("deviation row leaks an internal anchor: %q", row[4])
		}
	}
	parsed, err := ingest.Parse(ctx, ingest.File{Name: "final.docx", Data: docx}, ingest.ParseOptions{})
	if err != nil || !strings.Contains(parsed.Text, "响应评分项：S1 技术方案（10 分）") || !strings.Contains(parsed.Text, "响应评分项：S2 质保期承诺（5 分）") {
		t.Fatalf("sections do not open with the scoring items they answer (%v)", err)
	}

	// 文档属性：只含投标人；不带平台字样与本机办公软件账号。
	md, issues, err := CheckDOCXMetadata(docx, bidder)
	if err != nil || len(issues) != 0 || md.Author != bidder || md.LastModifiedBy != bidder || md.Company != "" {
		t.Fatalf("DOCX metadata = %+v issues = %+v %v", md, issues, err)
	}
	pmd, issues, err := CheckPDFMetadata(pdf, bidder)
	if err != nil || len(issues) != 0 || (pmd.Author != "" && pmd.Author != bidder) {
		t.Fatalf("PDF metadata = %+v issues = %+v %v", pmd, issues, err)
	}
	for name, raw := range unzip(t, docx) {
		if strings.HasPrefix(name, "docProps/") || name == "word/styles.xml" {
			if l := strings.ToLower(string(raw)); strings.Contains(l, "autobid") || strings.Contains(l, "wps_") {
				t.Fatalf("%s carries a platform or account marker", name)
			}
		}
	}
	t.Logf("final draft: %d pages, %d page references verified against the PDF (%d distinct pages); DOCX author %q, PDF author %q, application %q; engine %s",
		rep.Pages, len(refs), len(distinct), md.Author, pmd.Author, pmd.Application, jobs[db.ExportDOCX].OfficeVersion)

	// 提交检查清单：真实回读结果下系统项通过，内置模板需人确认知悉，人工项逐条确认后就绪。
	rid := sub.Request.ID
	cl, err := d.SubmissionChecklist(ctx, db.DefaultTenant, p.TaskID, rid)
	if err != nil || !cl.Final || !cl.Current || cl.Ready {
		t.Fatalf("checklist = %+v %v", cl, err)
	}
	status := map[string]string{}
	for _, it := range cl.Items {
		status[it.Key] = it.Status
	}
	for _, k := range []string{"final_draft", "current_content", "bidder_name", "toc_pages", "score_index", "layout", "page_numbers", "metadata"} {
		if status[k] != db.CheckPass {
			t.Errorf("system check %s = %s, want pass: %+v", k, status[k], cl.Items)
		}
	}
	if status["own_template"] != db.CheckAttention || status["evidence_attached"] != db.CheckPending {
		t.Fatalf("checklist statuses = %v", status)
	}
	for _, it := range cl.Items {
		if !it.Confirmable {
			continue
		}
		in := db.SubmissionCheckInput{Status: db.CheckConfirmed, Note: "已核对"}
		if cl, err = d.SetSubmissionCheck(ctx, db.DefaultTenant, p.TaskID, rid, it.Key, in, "投标经理"); err != nil {
			t.Fatalf("confirm %s: %v", it.Key, err)
		}
	}
	if !cl.Ready || cl.Open != 0 {
		t.Fatalf("checklist not ready after confirming everything: %+v", cl)
	}
	// 导出之后正文又改了：这份成稿不再是当前内容，清单不再就绪，也不能再确认。
	p.Save(t, "4", "项目组织与人员安排已根据最新要求调整。")
	cl, err = d.SubmissionChecklist(ctx, db.DefaultTenant, p.TaskID, rid)
	if err != nil || cl.Current || cl.Ready || cl.StaleReason == "" {
		t.Fatalf("checklist after editing a section = %+v %v", cl, err)
	}
	if _, err := d.SetSubmissionCheck(ctx, db.DefaultTenant, p.TaskID, rid, "cover_check", db.SubmissionCheckInput{Status: db.CheckConfirmed}, "投标经理"); !errors.Is(err, db.ErrSubmissionNotFinal) {
		t.Fatalf("confirming a stale final draft: %v", err)
	}
}
