package retrieval

import (
	"encoding/json"
	"github.com/autobid/autobid-engine/ingest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPagedTablesKeepHeadersAndOriginalMergedCells(t *testing.T) {
	table := ingest.Table{HeaderRows: 1, Grid: [][]string{{"型号", "值"}, {"X", "30秒"}, {"X", "60秒"}}, Cells: []ingest.Cell{{Row: 0, Col: 0, Text: "型号", Header: true}, {Row: 1, Col: 0, RowSpan: 2, Text: "X"}}}
	raw, _ := json.Marshal([]ingest.Table{table})
	page, err := PageTable(raw, 0, 2, 1, 2048)
	if err != nil || !page.Complete || page.RowOffset != 2 || len(page.Header) != 1 || len(page.Cells) != 2 || page.Cells[1].Row != 1 {
		t.Fatalf("%+v %v", page, err)
	}
	table.Grid[1][1] = strings.Repeat("x", 3000)
	raw, _ = json.Marshal([]ingest.Table{table})
	if _, err = PageTable(raw, 0, 1, 1, 1024); err == nil {
		t.Fatal("oversized cell silently truncated")
	}
	text := strings.Repeat("汉字", 200)
	first, next, err := TextPage(text, 0, 256)
	second, _, e := TextPage(text, next, 32000)
	if err != nil || e != nil || !utf8.ValidString(first) || first+second != text {
		t.Fatal("UTF-8 pagination corrupted source")
	}
}
