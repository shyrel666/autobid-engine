package ingest

import (
	"context"
	"strings"
	"testing"

	"github.com/autobid/autobid-engine/ingest/ingesttest"
)

// ruledScoringPDF 是带框线的评分表样本（Word 导出 PDF 的典型画法）：
//   - 第 1 页：单格外框说明（不是表格）、白色框线文字（不是表格）、标题，然后是 3 列评分表：
//     表头 + 「技术部分」纵向合并两行（类别格内文字垂直居中落在合并区中线）+ 单元格内两行编号细则 + 单元格内折行；
//     垂直边线用细长填充矩形绘制，水平边线用描边线段绘制；
//   - 第 2 页：同列宽的续表一行（类别格留空），应合并为跨页表格。
func ruledScoringPDF() []byte {
	xs := []float64{60, 160, 300, 500}
	ys := []float64{700, 680, 640, 600, 560}
	var fills [][4]float64
	for _, x := range xs {
		fills = append(fills, [4]float64{x - 0.25, ys[len(ys)-1], 0.5, ys[0] - ys[len(ys)-1]})
	}
	var rules [][4]float64
	for _, r := range ingesttest.GridRules(xs, ys, [3]float64{60, 640, 160}) {
		if r[1] == r[3] { // 只取水平线，垂直线由细长填充矩形绘制
			rules = append(rules, r)
		}
	}
	// 单格外框（不是表格）
	rules = append(rules, [4]float64{50, 780, 300, 780}, [4]float64{50, 805, 300, 805}, [4]float64{50, 780, 50, 805}, [4]float64{300, 780, 300, 805})
	white := "q 1 1 1 RG 0.5 w 60 755 m 300 755 l S 60 770 m 300 770 l S 60 755 m 60 770 l S 180 755 m 180 770 l S 300 755 m 300 770 l S Q\n"
	p1 := ingesttest.Page{Rules: rules, Fills: fills, Ops: white, Texts: []ingesttest.Text{
		{X: 60, Y: 788, Size: 10, S: "框内说明文字"},
		{X: 70, Y: 759, Size: 10, S: "白线甲"}, {X: 190, Y: 759, Size: 10, S: "白线乙"},
		{X: 60, Y: 725, Size: 12, S: "评标标准"},
		{X: 65, Y: 686, Size: 10, S: "评审因素"}, {X: 165, Y: 686, Size: 10, S: "评分标准"}, {X: 305, Y: 686, Size: 10, S: "分值"},
		{X: 65, Y: 636, Size: 10, S: "技术部分"},
		{X: 165, Y: 665, Size: 10, S: "方案完整得10分"}, {X: 305, Y: 665, Size: 10, S: "10"},
		{X: 165, Y: 625, Size: 10, S: "1.需求理解"}, {X: 165, Y: 612, Size: 10, S: "2.总体设计"}, {X: 305, Y: 618, Size: 10, S: "12"},
		{X: 65, Y: 578, Size: 10, S: "商务部分"},
		{X: 165, Y: 585, Size: 10, S: "投标人提供类似业"}, {X: 165, Y: 572, Size: 10, S: "绩，每个得2分。"}, {X: 305, Y: 578, Size: 10, S: "6"},
	}}
	ys2 := []float64{800, 770}
	p2 := ingesttest.Page{Rules: ingesttest.GridRules(xs, ys2), Texts: []ingesttest.Text{
		{X: 165, Y: 782, Size: 10, S: "安全设计方案"}, {X: 305, Y: 782, Size: 10, S: "6"},
	}}
	return ingesttest.BuildPDF("框线评分表", p1, p2)
}

func TestParsePDFRuledTableCells(t *testing.T) {
	p, err := Parse(context.Background(), File{Name: "ruled.pdf", Data: ruledScoringPDF()}, ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	var tables []*Table
	for _, l := range p.Lines {
		if l.Table != nil {
			tables = append(tables, l.Table)
			continue
		}
		texts = append(texts, l.Text)
	}
	if len(tables) != 1 {
		t.Fatalf("want exactly one ruled table (frame box and white lines are not tables), got %d: texts=%q", len(tables), texts)
	}
	joined := strings.Join(texts, "|")
	for _, want := range []string{"框内说明文字", "白线甲", "白线乙", "评标标准"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("text outside the table lost %q: %q", want, texts)
		}
	}
	tb := tables[0]
	want := [][]string{
		{"评审因素", "评分标准", "分值"},
		{"技术部分", "方案完整得10分", "10"},
		{"技术部分", "1.需求理解\n2.总体设计", "12"},
		{"商务部分", "投标人提供类似业绩，每个得2分。", "6"},
		{"", "安全设计方案", "6"},
	}
	if tb.Rows != len(want) || tb.Cols != 3 || tb.PageStart != 1 || tb.PageEnd != 2 {
		t.Fatalf("table shape %dx%d pages %d-%d: %q", tb.Rows, tb.Cols, tb.PageStart, tb.PageEnd, tb.Grid)
	}
	for r := range want {
		for c := range want[r] {
			if tb.Grid[r][c] != want[r][c] {
				t.Fatalf("cell (%d,%d) = %q, want %q; grid=%q", r, c, tb.Grid[r][c], want[r][c], tb.Grid)
			}
		}
	}
	merged := false
	for _, c := range tb.Cells {
		if c.Row == 1 && c.Col == 0 {
			merged = c.RowSpan == 2
		}
	}
	if !merged {
		t.Fatalf("vertically merged category cell not restored: %+v", tb.Cells)
	}
}

func TestCellTextJoinsWrappedLines(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"对相关内容进行专业", "校对。"}, "对相关内容进行专业校对。"},
		{[]string{"方案内容包括：", "1.常用校对方法；", "2.风险点。"}, "方案内容包括：\n1.常用校对方法；\n2.风险点。"},
		{[]string{"5.0（0,2.5,5）"}, "5.0（0,2.5,5）"},
		{[]string{"得分区间", "1.0（0,1）"}, "得分区间1.0（0,1）"},
	}
	for _, tc := range cases {
		out := &pdfPageOut{num: 1, box: [4]float64{0, 0, 595, 842}}
		var idx []int
		for i, line := range tc.in {
			x := 100.0
			for _, r := range line {
				out.glyphs = append(out.glyphs, glyphPos{x: x, y: 700 - float64(i)*14, w: 10, size: 10, text: string(r)})
				idx = append(idx, len(out.glyphs)-1)
				x += 10
			}
		}
		if got := cellText(out, idx); got != tc.want {
			t.Errorf("cellText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
