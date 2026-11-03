package ingest

import (
	"testing"

	"github.com/autobid/autobid-engine/ingest/ingesttest"
)

// 版面行的右缘止于最后一个可见字形：Word 在恰好排满的行尾留一个越过右边距的空格字形，不能算作文字越界。
func TestReadPDFLayoutExcludesTrailingSpace(t *testing.T) {
	data := ingesttest.BuildPDF("layout", ingesttest.Page{Texts: []ingesttest.Text{
		{X: 100, Y: 700, Size: 10, S: "目录 3 "},
		{X: 100, Y: 600, Size: 10, S: "正文结束。"},
	}})
	pages, err := ReadPDFLayout(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 || len(pages[0].Lines) != 2 {
		t.Fatalf("pages = %+v", pages)
	}
	// 样本字宽：CJK 1000、其余 500（10pt 下为 10 / 5）。目 录 空格 3 = 100+10+10+5+5，末尾空格不计入。
	if got := pages[0].Lines[0]; got.Text != "目录 3" || got.Box.X1 != 130 {
		t.Fatalf("line with trailing space = %q x1=%v, want x1=130", got.Text, got.Box.X1)
	}
	if got := pages[0].Lines[1]; got.Box.X1 != 150 {
		t.Fatalf("line without trailing space x1=%v, want 150", got.Box.X1)
	}
}
