package export

// D4 单元测试（无外部依赖）：评分索引表与章节页码引用的生成与回读规则、文档属性清洗、平台字样、早期样式名兼容。
// 真实文档引擎刷新后的页码核对在 d4_integration_test.go。

import (
	"archive/zip"
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/ingest"
)

// samplePackage 给 sampleInput 的章节配上评分索引、承诺事项与证明材料索引。
func samplePackage() db.PackageSnapshot {
	ref := func(id int64, code, title string) db.SnapshotSectionRef {
		return db.SnapshotSectionRef{SectionID: id, Code: code, Title: title}
	}
	return db.PackageSnapshot{
		ScoreIndex: []db.SnapshotScoreRow{
			{RequirementID: 1, Code: "S-001", Title: "技术方案完整性与针对性", Score: 10, Sections: []db.SnapshotSectionRef{ref(1, "1", "总体技术方案"), ref(2, "1.1", "系统总体架构")}},
			{RequirementID: 2, Code: "S-002", Title: "实施进度安排", Score: 5, Sections: []db.SnapshotSectionRef{ref(4, "2", "实施进度计划")}},
			{RequirementID: 3, Code: "S-003", Title: "售后服务方案", Score: 2.5, Sections: []db.SnapshotSectionRef{ref(6, "4", "售后服务方案")}},
		},
		Commitments: []db.SnapshotCommitment{{ID: 7, Code: "C-SLA", Version: 2, Metric: "sla_response", MetricLabel: "故障响应时限", Unit: "小时",
			TenderThreshold: "不超过 2 小时", Committed: "1", Status: "confirmed", RequirementCode: "R-009", Sections: []db.SnapshotSectionRef{ref(6, "4", "售后服务方案")}}},
		Evidence: []db.SnapshotEvidence{{Domain: "credential", EvidenceID: 3, Code: "ISO9001", Name: "质量管理体系认证证书", Number: "00123Q30001R0M", Subject: "示例科技有限公司",
			Originals:        []db.SnapshotOriginal{{DocumentKind: "certificate", SHA256: "ab", PageStart: 1, PageEnd: 2, PageLabel: "正本"}},
			RequirementCodes: []string{"R03", "S-001"}, Sections: []db.SnapshotSectionRef{ref(1, "1", "总体技术方案")}}},
	}
}

func unzip(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		b, err := readZipFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[f.Name] = b
	}
	return out
}

func rezip(t *testing.T, files map[string][]byte, order []string) []byte {
	t.Helper()
	data, err := writeZip(order, files)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

var reCachedRef = regexp.MustCompile(`(PAGEREF (_Ref\d+) \\h </w:instrText></w:r><w:r><w:fldChar w:fldCharType="separate"/></w:r><w:r>(?:<w:rPr>.*?</w:rPr>)?<w:t xml:space="preserve">)` +
	regexp.QuoteMeta(PageRefPlaceholder) + `(</w:t>)`)

func TestCommittedText(t *testing.T) {
	for _, c := range []struct{ committed, unit, want string }{
		{"5", "年", "5 年"}, {"99.9", "%", "99.9 %"}, {"90 天", "天", "90 天"}, {"分两批交付", "批", "分两批交付"}, {"5", "", "5"}, {"", "年", ""},
	} {
		if got := committedText(c.committed, c.unit); got != c.want {
			t.Errorf("committedText(%q, %q) = %q, want %q", c.committed, c.unit, got, c.want)
		}
	}
}

// fillPageRefs 模拟文档引擎刷新：把每个页码引用的缓存结果写成 page(书签名)。
func fillPageRefs(t *testing.T, data []byte, page func(bookmark string) string) []byte {
	t.Helper()
	files := unzip(t, data)
	doc := string(files["word/document.xml"])
	n := 0
	doc = reCachedRef.ReplaceAllStringFunc(doc, func(m string) string {
		sm := reCachedRef.FindStringSubmatch(m)
		n++
		return sm[1] + page(sm[2]) + sm[3]
	})
	if n == 0 {
		t.Fatal("no cached page reference found to fill")
	}
	files["word/document.xml"] = []byte(doc)
	return rezip(t, files, nil)
}

func TestScoreIndexAndPageRefsAreBuilt(t *testing.T) {
	tpl, err := DefaultTemplate()
	if err != nil {
		t.Fatal(err)
	}
	in := sampleInput()
	in.Package = samplePackage()
	res, err := Build(tpl, in)
	if err != nil {
		t.Fatal(err)
	}
	// 评分索引 4 处（S-001 映射两个章节）+ 偏离表 60 行 + 承诺 1 + 证明材料 1。
	if want := 4 + len(in.Matrix) + 1 + 1; res.PageRefs != want || res.ScoreRows != 3 || len(res.Bookmarks) != len(in.Doc.Sections) {
		t.Fatalf("page refs = %d (want %d), score rows = %d, bookmarks = %d", res.PageRefs, want, res.ScoreRows, len(res.Bookmarks))
	}
	wantHeads := []string{ScoreIndexTitle, "1 总体技术方案"}
	if len(res.Headings) != len(in.Doc.Sections)+4 || res.Headings[0] != wantHeads[0] || res.Headings[1] != wantHeads[1] ||
		res.Headings[len(res.Headings)-1] != EvidenceIndexTitle || res.Headings[len(res.Headings)-2] != CommitmentTitle || res.Headings[len(res.Headings)-3] != DeviationTitle {
		t.Fatalf("headings = %q", res.Headings)
	}
	doc := string(unzip(t, res.Data)["word/document.xml"])
	for _, s := range in.Doc.Sections {
		if !strings.Contains(doc, `w:name="`+SectionBookmark(s.SectionID)+`"`) {
			t.Fatalf("section %s has no bookmark", s.Code)
		}
	}
	for _, want := range []string{"响应评分项：", "S-001 技术方案完整性与针对性（10 分）", "S-003 售后服务方案（2.5 分）", "故障响应时限", "质量管理体系认证证书", "证书 第 1-2 页（正本）"} {
		if !strings.Contains(doc, want) {
			t.Fatalf("document lacks %q", want)
		}
	}
	// 偏离表：有证明材料的条款（R03）在应答说明里指向证明材料索引的序号，其余条款没有这一行。
	if n := strings.Count(doc, "证明材料：见《证明材料索引》第 1 项"); n != 1 {
		t.Fatalf("deviation rows pointing at the evidence index = %d, want 1", n)
	}
	// 内部术语、段落锚点与私用区标记不得进入成稿。
	for _, bad := range []string{"导出快照", "锁定修订", "¶", refOpen, refClose} {
		if strings.Contains(doc, bad) {
			t.Fatalf("document contains %q", bad)
		}
	}
	refs, marks, err := ReadPageRefs(res.Data)
	if err != nil || len(refs) != res.PageRefs || len(marks) != len(in.Doc.Sections) {
		t.Fatalf("read back refs = %d marks = %d err = %v", len(refs), len(marks), err)
	}
	if marks[SectionBookmark(2)] != "1.1 系统总体架构" || refs[0].Bookmark != SectionBookmark(1) || refs[0].Result != PageRefPlaceholder {
		t.Fatalf("marks = %v first ref = %+v", marks, refs[0])
	}
	// 未刷新：每个被引用的章节报告一次“未刷新”，不重复刷屏。
	_, issues, err := CheckPageRefs(res.Data, res.Bookmarks, res.PageRefs, nil, nil)
	if err != nil || len(issues) != len(in.Doc.Sections) {
		t.Fatalf("unrefreshed issues = %d %v, want one per referenced section", len(issues), err)
	}
	for _, is := range issues {
		if is.Kind != IssuePageRefUnresolved {
			t.Fatalf("issue = %+v", is)
		}
	}
	// 快照没有附表时不生成索引、提示行与引用之外的内容（旧作业重跑保持原结构，偏离表页码除外）。
	plain, err := Build(tpl, sampleInput())
	if err != nil || plain.ScoreRows != 0 || plain.Headings[0] != "1 总体技术方案" || plain.PageRefs != len(in.Matrix) {
		t.Fatalf("plain build: rows=%d refs=%d headings=%q err=%v", plain.ScoreRows, plain.PageRefs, plain.Headings[:1], err)
	}
}

func TestCheckPageRefsAgainstTOCAndLayout(t *testing.T) {
	tpl, _ := DefaultTemplate()
	in := sampleInput()
	in.Package = samplePackage()
	res, err := Build(tpl, in)
	if err != nil {
		t.Fatal(err)
	}
	pages := map[string]int{}
	var toc []ingest.TOCEntry
	for i, s := range in.Doc.Sections {
		pages[SectionBookmark(s.SectionID)] = 5 + 3*i
		toc = append(toc, ingest.TOCEntry{Text: strings.TrimSpace(s.Code + " " + s.Title), Page: fmt.Sprint(5 + 3*i)})
	}
	good := fillPageRefs(t, res.Data, func(b string) string { return fmt.Sprint(pages[b]) })
	checks, issues, err := CheckPageRefs(good, res.Bookmarks, res.PageRefs, nil, toc)
	if err != nil || len(issues) != 0 || len(checks) != res.PageRefs {
		t.Fatalf("consistent refs: issues = %+v checks = %d err = %v", issues, len(checks), err)
	}
	for _, c := range checks {
		if c.Listed == 0 || c.Listed != c.Actual {
			t.Fatalf("check = %+v", c)
		}
	}
	// 页码与标题所在页不一致。
	bad := fillPageRefs(t, res.Data, func(b string) string {
		if b == SectionBookmark(4) {
			return "99"
		}
		return fmt.Sprint(pages[b])
	})
	_, issues, _ = CheckPageRefs(bad, res.Bookmarks, res.PageRefs, nil, toc)
	if len(issues) != 1 || issues[0].Kind != IssuePageRefPage || !strings.Contains(issues[0].Detail, "实施进度计划") || issues[0].Page != pages[SectionBookmark(4)] {
		t.Fatalf("mismatch issues = %+v", issues)
	}
	// 以 PDF 版面为准：表格里同样的章节名字号小，标题取字号最大的一处。
	r := &PDFReport{titles: map[string]titlePos{}}
	for _, s := range in.Doc.Sections {
		r.titles[normText(s.Code+" "+s.Title)] = titlePos{page: pages[SectionBookmark(s.SectionID)], size: 16}
	}
	if _, issues, _ = CheckPageRefs(good, res.Bookmarks, res.PageRefs, r, nil); len(issues) != 0 {
		t.Fatalf("layout-verified issues = %+v", issues)
	}
	delete(r.titles, normText("2 实施进度计划"))
	if _, issues, _ = CheckPageRefs(good, res.Bookmarks, res.PageRefs, r, nil); len(issues) != 1 || issues[0].Kind != IssueHeadingMissing {
		t.Fatalf("missing heading issues = %+v", issues)
	}
	// 引用被删（成稿中的引用少于生成时写入的个数）。
	files := unzip(t, good)
	doc := string(files["word/document.xml"])
	i := strings.Index(doc, "PAGEREF "+SectionBookmark(6))
	files["word/document.xml"] = []byte(doc[:i] + "REMOVED " + doc[i+len("PAGEREF "):])
	if _, issues, _ = CheckPageRefs(rezip(t, files, nil), res.Bookmarks, res.PageRefs, nil, toc); len(issues) != 1 || issues[0].Kind != IssuePageRefMissing {
		t.Fatalf("removed ref issues = %+v", issues)
	}
}

// 目录页之后的同名文字出现多处时，标题页码取字号最大的一处：评分索引表的单元格不会被当成标题。
func TestHeadingPagePrefersLargestText(t *testing.T) {
	tpl, _ := DefaultTemplate()
	box := [4]float64{0, 0, 595.3, 841.9}
	line := func(page int, text string, size, y float64) ingest.PDFLine {
		return ingest.PDFLine{Text: text, Size: size, Segments: []string{text}, Box: ingest.BBox{Page: page, X0: 80, Y0: y, X1: 300, Y1: y + size}}
	}
	pages := []ingest.PDFPageLayout{
		{Number: 1, Box: box, Lines: []ingest.PDFLine{line(1, "目  录", 16, 740), line(1, "评分索引表........2", 12, 700), line(1, "1 总体方案........3", 12, 680)}},
		{Number: 2, Box: box, Lines: []ingest.PDFLine{line(2, "评分索引表", 16, 740), line(2, "1 总体方案", 9, 700)}},
		{Number: 3, Box: box, Lines: []ingest.PDFLine{line(3, "1 总体方案", 16, 740), line(3, "正文。", 12, 700)}},
	}
	r := checkLayout(pages, tpl.Geometry)
	if got := r.HeadingPage("1 总体方案"); got != 3 {
		t.Fatalf("heading page = %d, want 3 (the table cell on page 2 is smaller text)", got)
	}
	for _, is := range r.Issues {
		if tocIssue(is.Kind) {
			t.Fatalf("TOC issue with an index table before the body: %+v", is)
		}
	}
}

func TestMetadataIsNeutralAndSanitized(t *testing.T) {
	tpl, _ := DefaultTemplate()
	in := sampleInput()
	in.Package = samplePackage()
	res, err := Build(tpl, in)
	if err != nil {
		t.Fatal(err)
	}
	// 生成的每个部件、内置模板本身都不带平台字样。
	for _, data := range [][]byte{res.Data, DefaultTemplateBytes()} {
		for name, b := range unzip(t, data) {
			if strings.HasPrefix(name, "word/media/") {
				continue
			}
			if m := hasMarker(string(b)); m != "" {
				t.Fatalf("%s contains the platform marker %q", name, m)
			}
		}
	}
	md, issues, err := CheckDOCXMetadata(res.Data, in.Bidder)
	if err != nil || len(issues) != 0 || md.Author != in.Bidder || md.LastModifiedBy != in.Bidder || md.Title != in.Doc.TaskName {
		t.Fatalf("built metadata = %+v issues = %+v err = %v", md, issues, err)
	}
	// 投标人为空时作者留空，不回退成任何固定值。
	in2 := sampleInput()
	in2.Bidder = ""
	res2, _ := Build(tpl, in2)
	if md, issues, _ := CheckDOCXMetadata(res2.Data, ""); md.Author != "" || len(issues) != 0 {
		t.Fatalf("empty bidder metadata = %+v %+v", md, issues)
	}

	// 模拟文档引擎另存：最后修改者是本机 WPS 账号，app.xml 带公司，custom.xml 带安装相关属性。
	files := unzip(t, res.Data)
	files["docProps/core.xml"] = []byte(strings.Replace(string(files["docProps/core.xml"]),
		"<cp:lastModifiedBy>"+in.Bidder+"</cp:lastModifiedBy>", "<cp:lastModifiedBy>WPS_1639839416</cp:lastModifiedBy>", 1))
	files["docProps/app.xml"] = []byte(`<?xml version="1.0"?><Properties xmlns="http://schemas.openxmlformats.org/officeDocument/2006/extended-properties">` +
		`<Pages>15</Pages><Company>某集成商</Company><Manager>张三</Manager><Template>Normal.dotm</Template><Application>WPS Office_12.1.0</Application></Properties>`)
	files["docProps/custom.xml"] = []byte(`<?xml version="1.0"?><Properties xmlns="http://schemas.openxmlformats.org/officeDocument/2006/custom-properties" ` +
		`xmlns:vt="http://schemas.openxmlformats.org/officeDocument/2006/docPropsVTypes"><property fmtid="{D5CDD505-2E9C-101B-9397-08002B2CF9AE}" pid="3" name="ICV">` +
		`<vt:lpwstr>7F895D98B6A84746B8D5BE54111A788B_13</vt:lpwstr></property></Properties>`)
	saved := rezip(t, files, nil)
	md, issues, err = CheckDOCXMetadata(saved, in.Bidder)
	if err != nil || md.LastModifiedBy != "WPS_1639839416" || md.Company != "某集成商" || len(issues) != 2 {
		t.Fatalf("engine-saved metadata = %+v issues = %+v err = %v", md, issues, err)
	}
	clean, err := SanitizeDOCX(saved, DocMeta{Title: in.Doc.TaskName, Author: in.Bidder, At: in.Doc.CreatedAt})
	if err != nil {
		t.Fatal(err)
	}
	md, issues, err = CheckDOCXMetadata(clean, in.Bidder)
	if err != nil || len(issues) != 0 || md.LastModifiedBy != in.Bidder || md.Company != "" || md.Application != "WPS Office_12.1.0" {
		t.Fatalf("sanitized metadata = %+v issues = %+v err = %v", md, issues, err)
	}
	after := unzip(t, clean)
	if strings.Contains(string(after["docProps/custom.xml"]), "ICV") || strings.Contains(string(after["docProps/app.xml"]), "张三") ||
		!strings.Contains(string(after["docProps/app.xml"]), "<Pages>15</Pages>") {
		t.Fatalf("custom / app properties after sanitize:\n%s\n%s", after["docProps/custom.xml"], after["docProps/app.xml"])
	}
	for name, b := range unzip(t, saved) {
		if !strings.HasPrefix(name, "docProps/") && !bytes.Equal(after[name], b) {
			t.Fatalf("sanitize changed %s", name)
		}
	}
	// 平台字样出现在样式表里同样报告（下载过旧版内置模板再上传的情况）。
	files = unzip(t, clean)
	files["word/styles.xml"] = []byte(strings.Replace(string(files["word/styles.xml"]), `w:val="Table Text"`, `w:val="Autobid Table Text"`, 1))
	if _, issues, _ = CheckDOCXMetadata(rezip(t, files, nil), in.Bidder); len(issues) != 1 || issues[0].Kind != IssueMetadata {
		t.Fatalf("style marker issues = %+v", issues)
	}
}

// 早期版本的模板里名为「Autobid ...」的样式仍被识别并沿用，不再另补同用途样式。
func TestLegacyStyleNamesStillResolve(t *testing.T) {
	files := unzip(t, DefaultTemplateBytes())
	styles := string(files["word/styles.xml"])
	if !strings.Contains(styles, `w:styleId="TableText"><w:name w:val="Table Text"/>`) {
		t.Fatal("default template lacks the neutral Table Text style")
	}
	files["word/styles.xml"] = []byte(strings.Replace(styles, `w:styleId="TableText"><w:name w:val="Table Text"/>`,
		`w:styleId="AutobidTableText"><w:name w:val="Autobid Table Text"/>`, 1))
	tpl, err := LoadTemplate("legacy", rezip(t, files, nil))
	if err != nil {
		t.Fatal(err)
	}
	ids, stylesXML := tpl.resolveStyles()
	if ids["table text"] != "AutobidTableText" || strings.Contains(string(stylesXML), `w:styleId="TableText"`) {
		t.Fatalf("legacy style not reused: id = %q", ids["table text"])
	}
	for _, m := range tpl.Meta().MissingStyles {
		if m == "table text" {
			t.Fatal("legacy style reported as missing")
		}
	}
}

// 一行排不下的长标题在版面里是相邻的两三行：仍要能定位到标题所在页，目录条目与页码引用才能核对。
func TestHeadingPageFindsWrappedHeadings(t *testing.T) {
	tpl, _ := DefaultTemplate()
	box := [4]float64{0, 0, 595.3, 841.9}
	line := func(page int, text string, size, y float64) ingest.PDFLine {
		return ingest.PDFLine{Text: text, Size: size, Segments: []string{text}, Box: ingest.BBox{Page: page, X0: 80, Y0: y, X1: 500, Y1: y + size}}
	}
	const title = "4 项目组织机构、人员配置、岗位职责与驻场服务保障安排（含项目经理及主要技术人员）"
	pages := []ingest.PDFPageLayout{
		{Number: 1, Box: box, Lines: []ingest.PDFLine{line(1, "目  录", 16, 740),
			line(1, "4 项目组织机构、人员配置、岗位职责与驻场服务保障安排（含项目经理及主要", 12, 700), line(1, "技术人员）........3", 12, 684)}},
		// 评分索引表的单元格里同一标题折成三行（小字号）。
		{Number: 2, Box: box, Lines: []ingest.PDFLine{line(2, "评分索引表", 16, 740), line(2, "4 项目组织机构、人员配置、岗位", 9, 700),
			line(2, "职责与驻场服务保障安排（含项目", 9, 690), line(2, "经理及主要技术人员）", 9, 680)}},
		{Number: 3, Box: box, Lines: []ingest.PDFLine{line(3, "4 项目组织机构、人员配置、岗位职责与驻场服务保障安排（含", 16, 740),
			line(3, "项目经理及主要技术人员）", 16, 720), line(3, "正文第一段。", 12, 690)}},
	}
	r := checkLayout(pages, tpl.Geometry)
	if got := r.HeadingPage(title); got != 3 {
		t.Fatalf("wrapped heading page = %d, want 3", got)
	}
	for _, is := range r.Issues {
		if tocIssue(is.Kind) {
			t.Fatalf("wrapped heading reported as a TOC problem: %+v", is)
		}
	}
	if len(r.TOC) != 1 || r.TOC[0].Listed != 3 || r.TOC[0].Actual != 3 {
		t.Fatalf("TOC check = %+v", r.TOC)
	}
}
