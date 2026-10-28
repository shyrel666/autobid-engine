package export

// 纯单元测试（无外部依赖）：Markdown / 表格 / 分节生成、段落属性顺序、自检规则能检出问题、导出目录路径约束。

import (
	"context"
	"strings"
	"testing"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/ingest"
)

func TestColumnWidthsAndLandscape(t *testing.T) {
	header := []string{"序号", "名称", "型号"}
	rows := [][]string{{"1", "核心交换机", "S7706"}, {"2", "存储服务器", "R5300 G5"}}
	d, m, land := columnWidths(header, rows, 9070)
	if land {
		t.Fatal("3-column table put on a landscape page")
	}
	w := fitWidths(d, m, 9070)
	sum := 0
	for i, x := range w {
		sum += x
		if float64(x) < m[i]*tableUnitTw+cellPadTw-1 {
			t.Fatalf("column %d narrower than minimum: %v", i, w)
		}
	}
	if sum != 9070 {
		t.Fatalf("widths %v sum to %d", w, sum)
	}
	wide := make([]string, 12)
	for i := range wide {
		wide[i] = "参数"
	}
	if _, _, land := columnWidths(wide, nil, 9070); !land {
		t.Fatal("12-column table not put on a landscape page")
	}
}

func TestOrderPPr(t *testing.T) {
	got := orderPPr(`<w:pStyle w:val="A"/><w:ind w:left="1"/><w:spacing w:before="0"/><w:keepNext/><w:overflowPunct w:val="0"/><w:pageBreakBefore/>`)
	want := `<w:pStyle w:val="A"/><w:keepNext/><w:pageBreakBefore/><w:overflowPunct w:val="0"/><w:spacing w:before="0"/><w:ind w:left="1"/>`
	if got != want {
		t.Fatalf("orderPPr =\n%s\nwant\n%s", got, want)
	}
}

// 生成的 docx（未经 LibreOffice）：标题、偏离表行数、横向分节与占位 TOC 能被 ingest 回读。
func TestBuildDocxStructure(t *testing.T) {
	tpl, err := DefaultTemplate()
	if err != nil {
		t.Fatal(err)
	}
	in := sampleInput()
	res, err := Build(tpl, in)
	if err != nil {
		t.Fatal(err)
	}
	if res.MatrixRows != len(in.Matrix) || res.Landscape < 2 {
		t.Fatalf("build result rows=%d landscape=%d", res.MatrixRows, res.Landscape)
	}
	rep, err := CheckDOCX(context.Background(), res.Data)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Headings) != len(in.Doc.Sections)+1 || rep.Headings[0] != "1 总体技术方案" || rep.Headings[len(rep.Headings)-1] != DeviationTitle {
		t.Fatalf("headings = %q", rep.Headings)
	}
	if rep.TOCFields != 1 || len(rep.Issues) == 0 || rep.Issues[0].Kind != IssueTOCPlaceholder {
		t.Fatalf("unrefreshed TOC = %+v", rep)
	}
	for _, tb := range rep.Tables {
		if len(tb.Grid[0]) > 1 && tb.Grid[0][1] == DeviationHeader[1] && tb.Rows != len(in.Matrix)+1 {
			t.Fatalf("deviation rows = %d", tb.Rows)
		}
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Kind != db.WarnChartUnrendered {
		t.Fatalf("warnings = %+v", res.Warnings)
	}
	in.UpdateFieldsOnOpen = true
	res2, _ := Build(tpl, in)
	if !zipContains(t, res2.Data, "word/settings.xml", `<w:updateFields w:val="true"/>`) {
		t.Fatal("updateFields not set when TOC is not refreshed")
	}
	if !zipContains(t, res.Data, "word/document.xml", `w:orient="landscape"`) || !zipContains(t, res.Data, "word/document.xml", "<w:tblHeader/>") {
		t.Fatal("landscape section / repeated header row missing")
	}
}

func zipContains(t *testing.T, data []byte, name, needle string) bool {
	t.Helper()
	tpl, err := LoadTemplate("x", data)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(tpl.files[name]), needle)
}

// 自检规则本身能检出问题（防止验收断言恒为真）。
func TestCheckLayoutDetectsProblems(t *testing.T) {
	tpl, _ := DefaultTemplate()
	g := tpl.Geometry
	box := [4]float64{0, 0, 595.3, 841.9}
	line := func(page int, text string, x0, y0, x1, y1 float64) ingest.PDFLine {
		return ingest.PDFLine{Text: text, Segments: []string{text}, Box: ingest.BBox{Page: page, X0: x0, Y0: y0, X1: x1, Y1: y1}}
	}
	pages := []ingest.PDFPageLayout{
		{Number: 1, Box: box, Lines: []ingest.PDFLine{line(1, "目  录", 270, 740, 320, 755), line(1, "1 总体方案........3", 71, 700, 520, 712),
			line(1, "2 售后服务........5", 71, 680, 520, 692), line(1, "第 1 页", 270, 50, 320, 60)}},
		{Number: 2, Box: box, Lines: []ingest.PDFLine{line(2, "第 2 页", 270, 50, 320, 60)}}, // 空白页
		{Number: 3, Box: box, Lines: []ingest.PDFLine{line(3, "1 总体方案", 71, 740, 200, 755), line(3, "越界的正文", 71, 600, 560, 612), line(3, "第 4 页", 270, 50, 320, 60)},
			Images: []ingest.BBox{{Page: 3, X0: 80, Y0: 300, X1: 400, Y1: 500}, {Page: 3, X0: 300, Y0: 400, X1: 500, Y1: 580}}},
		{Number: 4, Box: box, Lines: []ingest.PDFLine{line(4, "2 售后服务", 71, 740, 200, 755)}},
	}
	r := checkLayout(pages, g)
	kinds := map[string]int{}
	for _, is := range r.Issues {
		kinds[is.Kind]++
	}
	for _, k := range []string{IssueBlankPage, IssueOutOfBounds, IssueImageOverlap, IssueTOCPage, IssueFooterPage} {
		if kinds[k] == 0 {
			t.Fatalf("%s not detected: %+v", k, r.Issues)
		}
	}
	if len(r.TOC) != 2 || r.TOC[0].Actual != 3 || r.TOC[1].Listed != 5 || r.TOC[1].Actual != 4 {
		t.Fatalf("toc = %+v", r.TOC)
	}
}

func TestStorePathsAreConfined(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	for _, bad := range []string{"", "../x", "/etc/passwd", `C:\x`, "a/../../b"} {
		if _, err := s.Abs(bad); err == nil {
			t.Fatalf("path %q accepted", bad)
		}
	}
	if rel := s.jobRel("../evil tenant", 3, 4); strings.Contains(rel, "..") || !strings.HasPrefix(rel, "evil_tenant/") {
		t.Fatalf("tenant segment not sanitized: %s", rel)
	}
}
