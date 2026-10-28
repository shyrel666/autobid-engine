package retrieval

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/autobid/autobid-engine/ingest"
)

type TablePage struct {
	Index     int           `json:"table_index"`
	Header    [][]string    `json:"header"`
	Rows      [][]string    `json:"rows"`
	Cells     []ingest.Cell `json:"cells"`
	RowOffset int           `json:"row_offset"`
	NextRow   int           `json:"next_row"`
	Complete  bool          `json:"complete"`
	PageStart int           `json:"page_start"`
	PageEnd   int           `json:"page_end"`
}

// PageTable retains headers and original coordinates; never drops a partial cell silently.
func PageTable(raw json.RawMessage, index, offset, limit, budget int) (TablePage, error) {
	var tables []ingest.Table
	if err := json.Unmarshal(raw, &tables); err != nil {
		return TablePage{}, err
	}
	if index < 0 || index >= len(tables) {
		return TablePage{}, fmt.Errorf("table index out of range")
	}
	t := tables[index]
	header := min(max(t.HeaderRows, 0), len(t.Grid))
	offset = max(offset, header)
	if offset > len(t.Grid) {
		return TablePage{}, fmt.Errorf("row offset out of range")
	}
	p := TablePage{Index: index, Header: t.Grid[:header], Rows: [][]string{}, Cells: []ingest.Cell{}, RowOffset: offset, NextRow: offset, PageStart: t.PageStart, PageEnd: t.PageEnd}
	budget = min(max(budget, 1024), 32000)
	limit = min(max(limit, 1), 100)
	for _, cell := range t.Cells {
		if cell.Row < header {
			p.Cells = append(p.Cells, cell)
		}
	}
	b, _ := json.Marshal(p)
	if len(b) > budget {
		return p, fmt.Errorf("table header exceeds context budget")
	}
	for row := offset; row < min(len(t.Grid), offset+limit); row++ {
		n := len(p.Cells)
		p.Rows = append(p.Rows, t.Grid[row])
		for _, cell := range t.Cells {
			if cell.Row >= header && ((cell.Row == row) || (row == offset && cell.Row < offset && cell.Row+max(1, cell.RowSpan) > offset)) {
				p.Cells = append(p.Cells, cell)
			}
		}
		p.NextRow = row + 1
		b, _ = json.Marshal(p)
		if len(b) > budget {
			p.Rows = p.Rows[:len(p.Rows)-1]
			p.Cells = p.Cells[:n]
			p.NextRow = row
			if row == offset {
				return p, fmt.Errorf("single table row exceeds context budget")
			}
			break
		}
	}
	p.Complete = p.NextRow == len(t.Grid)
	return p, nil
}

// TextPage uses UTF-8 byte offsets, which also conservatively bound textual tokens.
func TextPage(text string, offset, budget int) (string, int, error) {
	if offset < 0 || offset > len(text) || (offset < len(text) && !utf8.RuneStart(text[offset])) {
		return "", offset, fmt.Errorf("invalid UTF-8 text offset")
	}
	end := min(len(text), offset+min(max(budget, 256), 32000))
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[offset:end], end, nil
}
