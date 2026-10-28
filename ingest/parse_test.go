package ingest

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/autobid/autobid-engine/ingest/ingesttest"
)

// SampleTenderDOCX 是含标题样式、★ 条款与跨页合并单元格表格的固定 DOCX 样本：
// 第 1 页第一章；第 2 页第二章与设备参数表（表头 + 纵向合并“摄像机”），表格第 4 行起于第 3 页；第 4 页第三章。
func sampleTenderDOCX() []byte {
	return ingesttest.BuildDOCX("智慧园区安防系统采购项目招标文件",
		ingesttest.Para{Text: "智慧园区安防系统采购项目招标文件", Heading: -1},
		ingesttest.Para{Text: "第一章 投标人须知", Heading: 1},
		ingesttest.Para{Text: "1.1 投标人资格要求", Heading: 2},
		ingesttest.Para{Text: "★1.1.1 投标人须具备电子与智能化工程专业承包一级资质，否则投标无效。"},
		ingesttest.Para{Text: "1.1.2 投标人须提供近三年类似项目业绩。"},
		ingesttest.Para{Text: "第二章 技术要求", Heading: 1, RenderedBreak: true},
		ingesttest.Para{Text: "2.1 设备技术参数", Heading: 2},
		ingesttest.Table{Cols: 4, Rows: []ingesttest.Row{
			{Header: true, Cells: []ingesttest.Cell{{Text: "序号"}, {Text: "设备类别"}, {Text: "技术要求", Span: 2}}},
			{Cells: []ingesttest.Cell{{Text: "1"}, {Text: "摄像机", VMerge: "restart"}, {Text: "★分辨率"}, {Text: "≥400万像素"}}},
			{Cells: []ingesttest.Cell{{Text: "2"}, {VMerge: "continue"}, {Text: "夜视距离"}, {Text: "≥50米"}}},
			{RenderedBreak: true, Cells: []ingesttest.Cell{{Text: "3"}, {Text: "存储设备"}, {Text: "★存储容量"}, {Text: "≥200TB"}}},
			{Cells: []ingesttest.Cell{{Text: "4"}, {Text: "平台软件"}, {Text: "并发路数"}, {Text: "≥1000路"}}},
		}},
		ingesttest.Para{Text: "2.2 售后服务要求", Heading: 2},
		ingesttest.Para{Text: "质保期不少于3年。"},
		ingesttest.Para{Text: "第三章 评标办法", Heading: 1, RenderedBreak: true},
		ingesttest.Para{Text: "3.1 技术方案（40分）", Heading: 3},
		ingesttest.Para{Text: "评审技术方案的完整性与可行性。"},
	)
}

var pdfCols = []float64{72, 180, 300, 430}

// sampleTenderPDF 是 3 页 PDF 样本：设备参数表从第 1 页延续到第 2 页（第 2 页重复表头），
// 含首列纵向合并与单元格折行；第 2、3 页有重复页眉，每页有页码页脚。
func sampleTenderPDF() []byte {
	t := func(x, y, size float64, s string) ingesttest.Text {
		return ingesttest.Text{X: x, Y: y, Size: size, S: s}
	}
	header := []string{"设备类别", "参数项", "指标要求", "偏离说明"}
	p1 := []ingesttest.Text{
		t(72, 800, 16, "智慧园区安防系统采购项目招标文件"),
		t(72, 760, 14, "第一章 投标人须知"),
		t(72, 735, 12, "1.1 投标人资格要求"),
		t(72, 712, 10, "★1.1.1 投标人须具备电子与智能化工程专业承包一级资质，否则投标无效。"),
		t(72, 680, 14, "第二章 技术要求"),
		t(72, 655, 12, "2.1 设备技术参数"),
	}
	p1 = append(p1, ingesttest.TableTexts(pdfCols, 620, 20, 10, header, []string{"摄像机", "★分辨率", "≥400万像素", "无"})...)
	p1 = append(p1, t(300, 588, 10, "（星光级）"))
	p1 = append(p1, ingesttest.TableTexts(pdfCols, 568, 20, 10, []string{"", "夜视距离", "≥50米", "无"})...)
	p1 = append(p1, t(280, 40, 9, "- 1 -"))

	p2 := []ingesttest.Text{t(72, 815, 9, "智慧园区安防系统采购项目")}
	p2 = append(p2, ingesttest.TableTexts(pdfCols, 760, 20, 10, header,
		[]string{"存储设备", "★存储容量", "≥200TB", "无"}, []string{"平台软件", "并发路数", "≥1000路", "无"})...)
	p2 = append(p2, t(72, 680, 12, "2.2 售后服务要求"), t(72, 660, 10, "质保期不少于3年，投标人须承诺7×24小时响应。"), t(280, 40, 9, "- 2 -"))

	p3 := []ingesttest.Text{
		t(72, 815, 9, "智慧园区安防系统采购项目"),
		t(72, 760, 14, "第三章 评标办法"),
		t(72, 735, 12, "3.1 技术方案（40分）"),
		t(72, 712, 10, "评审技术方案的完整性与可行性。"),
		t(280, 40, 9, "- 3 -"),
	}
	return ingesttest.BuildPDF("智慧园区安防系统采购项目招标文件", ingesttest.Page{Texts: p1}, ingesttest.Page{Texts: p2}, ingesttest.Page{Texts: p3})
}

func findSection(t *testing.T, secs []Section, code string) Section {
	t.Helper()
	for _, s := range secs {
		if s.Code == code {
			return s
		}
	}
	t.Fatalf("section %q not found in %v", code, sectionCodes(secs))
	return Section{}
}

func sectionCodes(secs []Section) []string {
	out := make([]string, 0, len(secs))
	for _, s := range secs {
		out = append(out, s.Code)
	}
	return out
}

func TestParseDOCXHeadingsTablesAndPages(t *testing.T) {
	p, err := Parse(context.Background(), File{Name: "招标文件.docx", Data: sampleTenderDOCX()}, ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Format != FormatDOCX || p.Title != "智慧园区安防系统采购项目招标文件" || p.PageCount != 4 || len(p.Tables) != 1 || p.Meta.Headings != 7 {
		t.Fatalf("parsed: format=%s title=%q pages=%d tables=%d meta=%+v", p.Format, p.Title, p.PageCount, len(p.Tables), p.Meta)
	}
	secs := p.Sections()
	want := []struct {
		code      string
		level     int
		from, to  int
		mandatory bool
	}{
		{"", 0, 1, 1, false}, {"第一章", 1, 1, 1, false}, {"1.1", 2, 1, 1, false}, {"1.1.1", 5, 1, 1, true}, {"1.1.2", 5, 1, 1, false},
		{"第二章", 1, 2, 2, false}, {"2.1", 2, 2, 3, true}, {"2.2", 2, 3, 3, false}, {"第三章", 1, 4, 4, false}, {"3.1", 3, 4, 4, false},
	}
	if len(secs) != len(want) {
		t.Fatalf("sections = %v", sectionCodes(secs))
	}
	for i, w := range want {
		s := secs[i]
		if s.Code != w.code || s.Level != w.level || s.PageStart != w.from || s.PageEnd != w.to || s.Mandatory != w.mandatory {
			t.Fatalf("section %d: got code=%q level=%d pages=%d-%d mandatory=%v, want %+v", i, s.Code, s.Level, s.PageStart, s.PageEnd, s.Mandatory, w)
		}
	}
	spec := findSection(t, secs, "2.1")
	if len(spec.Tables) != 1 || spec.BlockStart < 0 || spec.BlockEnd <= spec.BlockStart {
		t.Fatalf("2.1 tables/blocks: %+v", spec)
	}
	tbl := spec.Tables[0]
	if tbl.Rows != 5 || tbl.Cols != 4 || tbl.HeaderRows != 1 || tbl.PageStart != 2 || tbl.PageEnd != 3 || !tbl.Mandatory || tbl.Source != "docx" {
		t.Fatalf("table: %+v", tbl)
	}
	var span, rowspan *Cell
	for i := range tbl.Cells {
		c := &tbl.Cells[i]
		if c.Text == "技术要求" {
			span = c
		}
		if c.Text == "摄像机" {
			rowspan = c
		}
	}
	if span == nil || span.ColSpan != 2 || !span.Header || rowspan == nil || rowspan.RowSpan != 2 || rowspan.Row != 1 || rowspan.Col != 1 {
		t.Fatalf("merged cells: span=%+v rowspan=%+v", span, rowspan)
	}
	if tbl.Grid[2][1] != "摄像机" || tbl.Grid[0][3] != "技术要求" || tbl.Grid[3][3] != "≥200TB" {
		t.Fatalf("grid: %v", tbl.Grid)
	}
	for _, line := range []string{"| 序号 | 设备类别 | 技术要求 |  |", "| --- | --- | --- | --- |", "| 2 | 摄像机 | 夜视距离 | ≥50米 |"} {
		if !strings.Contains(tbl.Markdown, line) {
			t.Fatalf("markdown missing %q:\n%s", line, tbl.Markdown)
		}
	}
	if !strings.Contains(spec.Content, tbl.Markdown) {
		t.Fatal("table markdown must be part of the section content for full-text search")
	}
	if !strings.Contains(p.Text, "\f") || !strings.Contains(p.Text, "| 4 | 平台软件 |") {
		t.Fatalf("raw text rendering: %q", p.Text)
	}
}

func TestParsePDFCrossPageTable(t *testing.T) {
	p, err := Parse(context.Background(), File{Name: "tender.pdf", Data: sampleTenderPDF()}, ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Format != FormatPDF || p.PageCount != 3 || p.Title != "智慧园区安防系统采购项目招标文件" || len(p.Tables) != 1 {
		t.Fatalf("parsed: %+v tables=%d", p.Meta, len(p.Tables))
	}
	if strings.Contains(p.Text, "- 1 -") || strings.Count(p.Text, "智慧园区安防系统采购项目\n") > 0 {
		t.Fatalf("page numbers / running headers must be removed:\n%s", p.Text)
	}
	secs := p.Sections()
	want := []struct {
		code     string
		from, to int
	}{{"", 1, 1}, {"第一章", 1, 1}, {"1.1", 1, 1}, {"1.1.1", 1, 1}, {"第二章", 1, 1}, {"2.1", 1, 2}, {"2.2", 2, 2}, {"第三章", 3, 3}, {"3.1", 3, 3}}
	if len(secs) != len(want) {
		t.Fatalf("sections = %v", sectionCodes(secs))
	}
	for i, w := range want {
		if secs[i].Code != w.code || secs[i].PageStart != w.from || secs[i].PageEnd != w.to {
			t.Fatalf("section %d: %q %d-%d, want %+v", i, secs[i].Code, secs[i].PageStart, secs[i].PageEnd, w)
		}
	}
	if s := findSection(t, secs, "1.1.1"); !s.Mandatory || len(s.Boxes) != 1 || s.Boxes[0].Page != 1 || s.Boxes[0].X0 != 72 {
		t.Fatalf("★ clause: %+v", s)
	}
	spec := findSection(t, secs, "2.1")
	if !spec.Mandatory || len(spec.Tables) != 1 || len(spec.Boxes) != 2 {
		t.Fatalf("2.1: mandatory=%v tables=%d boxes=%+v", spec.Mandatory, len(spec.Tables), spec.Boxes)
	}
	tbl := spec.Tables[0]
	if tbl.Rows != 5 || tbl.Cols != 4 || tbl.PageStart != 1 || tbl.PageEnd != 2 || tbl.HeaderRows != 1 || tbl.Source != "pdf" {
		t.Fatalf("table: rows=%d cols=%d pages=%d-%d grid=%v", tbl.Rows, tbl.Cols, tbl.PageStart, tbl.PageEnd, tbl.Grid)
	}
	wantGrid := [][]string{
		{"设备类别", "参数项", "指标要求", "偏离说明"},
		{"摄像机", "★分辨率", "≥400万像素（星光级）", "无"},
		{"摄像机", "夜视距离", "≥50米", "无"},
		{"存储设备", "★存储容量", "≥200TB", "无"},
		{"平台软件", "并发路数", "≥1000路", "无"},
	}
	for r := range wantGrid {
		for c := range wantGrid[r] {
			if tbl.Grid[r][c] != wantGrid[r][c] {
				t.Fatalf("grid[%d][%d] = %q, want %q\n%v", r, c, tbl.Grid[r][c], wantGrid[r][c], tbl.Grid)
			}
		}
	}
	for _, c := range tbl.Cells {
		if c.Text == "摄像机" && (c.RowSpan != 2 || c.Page != 1) {
			t.Fatalf("rowspan cell: %+v", c)
		}
		if c.Text == "存储设备" && c.Page != 2 {
			t.Fatalf("continued row page: %+v", c)
		}
	}
	if strings.Count(tbl.Markdown, "设备类别") != 1 {
		t.Fatalf("repeated header must be dropped:\n%s", tbl.Markdown)
	}
}

// 页眉只出现在续页（不足以判定为重复页眉而被删除）时，跨页续表仍应合并。
func TestParsePDFCrossPageTableWithSingleHeader(t *testing.T) {
	t1 := ingesttest.TableTexts(pdfCols, 620, 20, 10, []string{"设备", "参数", "要求", "备注"}, []string{"摄像机", "分辨率", "≥400万", "无"})
	t2 := append([]ingesttest.Text{{X: 72, Y: 815, Size: 9, S: "某项目招标文件"}},
		ingesttest.TableTexts(pdfCols, 760, 20, 10, []string{"设备", "参数", "要求", "备注"}, []string{"存储", "容量", "≥200TB", "无"})...)
	t2 = append(t2, ingesttest.Text{X: 72, Y: 600, Size: 10, S: "以上参数均为最低要求。"})
	pdf := ingesttest.BuildPDF("x", ingesttest.Page{Texts: append([]ingesttest.Text{{X: 72, Y: 700, Size: 12, S: "2.1 设备参数"}}, t1...)}, ingesttest.Page{Texts: t2})
	p, err := Parse(context.Background(), File{Name: "a.pdf", Data: pdf}, ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Tables) != 1 || p.Tables[0].Rows != 3 || p.Tables[0].PageEnd != 2 || !strings.Contains(p.Text, "某项目招标文件") {
		t.Fatalf("tables=%d %+v\n%s", len(p.Tables), p.Tables, p.Text)
	}
}

func TestParsePDFRebuildsBrokenXref(t *testing.T) {
	data := sampleTenderPDF()
	i := bytes.LastIndex(data, []byte("startxref\n"))
	broken := append(append([]byte(nil), data[:i]...), []byte("startxref\n999999\n%%EOF\n")...)
	p, err := Parse(context.Background(), File{Name: "broken.pdf", Data: broken}, ParseOptions{})
	if err != nil || len(p.Tables) != 1 || p.PageCount != 3 {
		t.Fatalf("rebuild xref: %v %+v", err, p)
	}
}

type fakeOCR struct {
	calls int
	mime  string
}

func (f *fakeOCR) Name() string { return "fake" }
func (f *fakeOCR) Recognize(_ context.Context, img []byte, mime string) (*OCRPage, error) {
	f.calls++
	f.mime = mime
	return &OCRPage{Width: 1000, Height: 1400, Lines: []OCRLine{
		{Text: "第一章 总则", BBox: [4]float64{80, 100, 300, 130}, Confidence: 0.98},
		{Text: "★1.1 投标人须提供营业执照原件，否则投标无效。", BBox: [4]float64{80, 150, 900, 180}, Confidence: 0.91},
		{Text: "1.2 投标保证金为人民币伍万元。", BBox: [4]float64{80, 200, 700, 230}, Confidence: 0.95},
		{Text: "第 1 页", BBox: [4]float64{450, 1350, 550, 1380}, Confidence: 0.99},
	}}, nil
}

func TestParseScannedPDFAndImageUseOCR(t *testing.T) {
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F', 0, 1, 0xFF, 0xD9}
	scanned := ingesttest.BuildPDF("扫描件", ingesttest.Page{JPEG: jpeg})
	if _, err := Parse(context.Background(), File{Name: "scan.pdf", Data: scanned}, ParseOptions{}); !errors.Is(err, ErrOCRUnavailable) {
		t.Fatalf("scanned PDF without OCR: %v", err)
	}
	ocr := &fakeOCR{}
	p, err := Parse(context.Background(), File{Name: "scan.pdf", Data: scanned}, ParseOptions{OCR: ocr})
	if err != nil {
		t.Fatal(err)
	}
	if ocr.calls != 1 || ocr.mime != "image/jpeg" || p.Meta.OCR != "fake" || len(p.Meta.OCRPages) != 1 {
		t.Fatalf("ocr: calls=%d mime=%s meta=%+v", ocr.calls, ocr.mime, p.Meta)
	}
	secs := p.Sections()
	s := findSection(t, secs, "1.1")
	if !s.Mandatory || len(s.Boxes) != 1 || s.Boxes[0].Conf != 0.91 || s.Boxes[0].X1 != 900 {
		t.Fatalf("ocr clause: %+v", s)
	}
	if strings.Contains(p.Text, "第 1 页") {
		t.Fatal("OCR page marks must be dropped")
	}

	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR")
	if _, err := Parse(context.Background(), File{Name: "scan.png", Data: png}, ParseOptions{}); !errors.Is(err, ErrOCRUnavailable) {
		t.Fatalf("image without OCR: %v", err)
	}
	p, err = Parse(context.Background(), File{Name: "scan.png", Data: png}, ParseOptions{OCR: ocr})
	if err != nil || p.Format != FormatImage || p.MIME != "image/png" || len(p.Sections()) != 3 {
		t.Fatalf("image: %v %+v", err, p)
	}
}

func TestParseTextMarkdownAndDetection(t *testing.T) {
	md := "# 第一章 总体方案\n\n## 1.1 系统架构\n采用云边协同架构。\n\n| 指标 | 要求 |\n| --- | :---: |\n| ★CPU | ≥32核 |\n| 内存 | ≥256GB |\n\n## 1.2 部署方案\n分两期部署。"
	p, err := Parse(context.Background(), File{Name: "方案.md", Data: []byte(md)}, ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	secs := p.Sections()
	if len(secs) != 3 || secs[0].Level != 1 || secs[1].Code != "1.1" || secs[1].Level != 2 || len(secs[1].Tables) != 1 || !secs[1].Mandatory {
		t.Fatalf("markdown sections: %+v", secs)
	}
	if tbl := secs[1].Tables[0]; tbl.Rows != 3 || tbl.Cols != 2 || tbl.Grid[1][0] != "★CPU" {
		t.Fatalf("markdown table: %+v", tbl)
	}

	gbk := []byte{0xb5, 0xda, 0xd2, 0xbb, 0xd5, 0xc2, 0x20, 0xd7, 0xdc, 0xd4, 0xf2} // "第一章 总则"
	p, err = Parse(context.Background(), File{Name: "gbk.txt", Data: gbk}, ParseOptions{})
	if err != nil || p.Meta.Encoding != "gb18030" || !strings.HasPrefix(p.Text, "第一章 总则") {
		t.Fatalf("gbk text: %v %+v", err, p)
	}

	if _, err := Parse(context.Background(), File{Name: "old.doc", Data: []byte{0xD0, 0xCF, 0x11, 0xE0}}, ParseOptions{}); !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf(".doc must be rejected: %v", err)
	}
	for name, want := range map[string]string{"a.pdf": FormatPDF, "a.DOCX": FormatDOCX, "a.jpeg": FormatImage, "a.md": FormatMarkdown} {
		if got := DetectFormat(name, "", nil); got != want {
			t.Fatalf("DetectFormat(%s) = %s", name, got)
		}
	}
	if DetectFormat("upload", "", sampleTenderPDF()) != FormatPDF || DetectFormat("upload", "", sampleTenderDOCX()) != FormatDOCX {
		t.Fatal("magic-byte detection failed")
	}
}

func TestParseTesseractTSV(t *testing.T) {
	tsv := "level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext\n" +
		"1\t1\t0\t0\t0\t0\t0\t0\t1000\t1400\t-1\t\n" +
		"5\t1\t1\t1\t1\t1\t10\t20\t40\t20\t90\t第一章\n" +
		"5\t1\t1\t1\t1\t2\t60\t20\t40\t20\t80\t总则\n" +
		"5\t1\t1\t1\t2\t1\t10\t60\t50\t20\t70\tPage\n" +
		"5\t1\t1\t1\t2\t2\t70\t60\t20\t20\t90\tone\n"
	page, err := ParseTesseractTSV([]byte(tsv))
	if err != nil || page.Width != 1000 || len(page.Lines) != 2 {
		t.Fatalf("tsv: %v %+v", err, page)
	}
	if page.Lines[0].Text != "第一章总则" || page.Lines[0].BBox != [4]float64{10, 20, 100, 40} || page.Lines[0].Confidence != 0.85 || page.Lines[1].Text != "Page one" {
		t.Fatalf("lines: %+v", page.Lines)
	}
}
