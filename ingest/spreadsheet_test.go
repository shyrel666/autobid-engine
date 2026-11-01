package ingest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "eval", "file-processing", "fixtures", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return data
}

func findSheet(t *testing.T, p *Parsed, name string) *Table {
	t.Helper()
	for _, tb := range p.SheetTables {
		if tb.Sheet == name {
			return tb
		}
	}
	t.Fatalf("sheet %q not found in %+v", name, p.Meta.Sheets)
	return nil
}

func findCell(t *testing.T, tb *Table, row, col int) *Cell {
	t.Helper()
	for i := range tb.Cells {
		if tb.Cells[i].Row == row && tb.Cells[i].Col == col {
			return &tb.Cells[i]
		}
	}
	t.Fatalf("cell (%d,%d) not found in %s", row, col, tb.Sheet)
	return nil
}

func TestParseXLSXComplexFixture(t *testing.T) {
	data := fixtureBytes(t, "clean.xlsx")
	p, err := Parse(context.Background(), File{Name: "clean.xlsx", Data: data, ObjectID: 42, SHA256: strings.Repeat("a", 64)}, ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Format != FormatXLSX || p.PageCount != 0 || p.Meta.Pages != 0 {
		t.Fatalf("format/pages = %s/%d/%d", p.Format, p.PageCount, p.Meta.Pages)
	}
	if len(p.SheetTables) != 4 || len(p.Meta.Sheets) != 4 {
		t.Fatalf("sheets = %d coverage = %+v", len(p.SheetTables), p.Meta.Sheets)
	}
	quote := findSheet(t, p, "报价清单")
	if quote.Rows != 6 || quote.Cols != 8 || quote.HeaderRows != 2 || quote.SheetHidden {
		t.Fatalf("quote shape = %+v", quote)
	}
	if len(quote.Merges) != 1 || quote.Merges[0] != "A1:F1" {
		t.Fatalf("merges = %+v", quote.Merges)
	}
	if len(quote.HiddenRows) != 1 || quote.HiddenRows[0] != 5 || len(quote.HiddenCols) != 1 || quote.HiddenCols[0] != "E" {
		t.Fatalf("hidden rows/cols = %+v / %+v", quote.HiddenRows, quote.HiddenCols)
	}
	if got := findCell(t, quote, 4, 3).Text; got != "00100" {
		t.Fatalf("leading zero = %q", got)
	}
	if got := findCell(t, quote, 2, 7).Text; got != "000012345678901234" {
		t.Fatalf("long code = %q", got)
	}
	date := findCell(t, quote, 2, 6)
	if date.Raw != "46371" || date.Display != "2026-12-15" {
		t.Fatalf("date = %+v", date)
	}
	money := findCell(t, quote, 2, 4)
	if money.Raw != "1250.5" || money.Display != "1,250.50" {
		t.Fatalf("money = %+v", money)
	}
	formula := findCell(t, quote, 2, 5)
	if formula.Formula != "D3*E3" || formula.FormulaCached {
		t.Fatalf("formula = %+v", formula)
	}
	if !findCell(t, quote, 2, 4).Hidden {
		t.Fatalf("hidden column E cell not marked hidden")
	}
	if !findCell(t, quote, 4, 0).Hidden {
		t.Fatalf("hidden row 5 cell not marked hidden")
	}
	hidden := findSheet(t, p, "隐藏页")
	if !hidden.SheetHidden || hidden.Rows != 1 || !strings.Contains(findCell(t, hidden, 0, 0).Text, "★") {
		t.Fatalf("hidden sheet = %+v", hidden)
	}
	empty := findSheet(t, p, "空表")
	if empty.Rows != 0 || len(empty.Cells) != 0 {
		t.Fatalf("empty sheet = %+v", empty)
	}
	joined := strings.Join(p.Meta.Warnings, "\n")
	for _, want := range []string{"hidden_sheet:隐藏页", "formula_cache_missing:报价清单!F3", "empty_sheet:空表"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing warning %q in %v", want, p.Meta.Warnings)
		}
	}
	// 行窗口切分：数据从第 3 行开始，表头重复，且不伪造物理页码。
	secs := p.Sections()
	if len(secs) == 0 {
		t.Fatal("no sections")
	}
	var window *Section
	for i := range secs {
		if strings.HasPrefix(secs[i].Title, "报价清单 行 ") {
			window = &secs[i]
			break
		}
	}
	if window == nil || window.PageStart != 0 || window.PageEnd != 0 {
		t.Fatalf("window = %+v", window)
	}
	if len(window.Tables) != 1 || window.Tables[0].HeaderRows != 2 || window.Tables[0].Range != "A3:H6" {
		t.Fatalf("window table = %+v", window.Tables)
	}
	if !strings.Contains(window.Content, "序号") || !strings.Contains(window.Content, "网络摄像机") {
		t.Fatalf("window content missing header/data: %s", window.Content)
	}
	loc := window.Tables[0].SourceLocator
	if loc == nil || loc.Kind != "spreadsheet" || loc.FileID != 42 || loc.Sheet != "报价清单" || loc.Range != "A3:H6" {
		t.Fatalf("source locator = %+v", loc)
	}
	if window.Tables[0].PageIsPhysical == nil || *window.Tables[0].PageIsPhysical {
		t.Fatalf("window page_is_physical = %v", window.Tables[0].PageIsPhysical)
	}
	if !strings.Contains(p.Text, "## 报价清单") || strings.TrimSpace(p.Text) == "" {
		t.Fatalf("rendered text = %q", p.Text)
	}
}

func TestParseCSVGB18030AndTSV(t *testing.T) {
	csvData := fixtureBytes(t, "gb18030.csv")
	p, err := Parse(context.Background(), File{Name: "gb18030.csv", MIME: "application/vnd.ms-excel", Data: csvData}, ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Format != FormatCSV || p.Meta.Encoding != "gb18030" || p.Meta.Delimiter != ";" || p.PageCount != 0 {
		t.Fatalf("csv meta = %+v page=%d", p.Meta, p.PageCount)
	}
	if len(p.SheetTables) != 1 || p.SheetTables[0].Rows != 3 || p.SheetTables[0].Cols != 6 {
		t.Fatalf("csv table = %+v", p.SheetTables)
	}
	if got := findCell(t, p.SheetTables[0], 1, 3).Text; got != "0008" {
		t.Fatalf("csv leading zero = %q", got)
	}
	if got := findCell(t, p.SheetTables[0], 1, 2).Text; got != "SW-000045" {
		t.Fatalf("csv long code = %q", got)
	}
	if got := findCell(t, p.SheetTables[0], 2, 5).Text; !strings.Contains(got, "报价;含税") || !strings.Contains(got, "\n") {
		t.Fatalf("csv multiline = %q", got)
	}

	tsvData := fixtureBytes(t, "quote.tsv")
	p, err = Parse(context.Background(), File{Name: "quote.tsv", Data: tsvData}, ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Format != FormatTSV || p.Meta.Delimiter != "\t" || p.PageCount != 0 {
		t.Fatalf("tsv meta = %+v page=%d", p.Meta, p.PageCount)
	}
	if got := findCell(t, p.SheetTables[0], 1, 1).Text; got != "000000000000000001" {
		t.Fatalf("tsv long code = %q", got)
	}
	if got := findCell(t, p.SheetTables[0], 1, 2).Text; got != "2026-12-15" {
		t.Fatalf("tsv date = %q", got)
	}
	if got := findCell(t, p.SheetTables[0], 1, 3).Text; got != "1250.50" {
		t.Fatalf("tsv amount = %q", got)
	}
}
