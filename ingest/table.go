package ingest

import (
	"strings"
)

// Table 是从 DOCX / PDF / Markdown 还原的结构化表格。
//   - Cells 只列出合并区域的左上角原始单元格（含 RowSpan / ColSpan），按行优先排列；
//   - Grid 为展开后的 Rows×Cols 文本网格，被合并覆盖的位置重复原始单元格文本，便于逐行读取；
//   - Markdown 为 GFM 表格：纵向合并的覆盖位置重复原文（每行自洽），横向合并的覆盖位置留空。
//
// 跨页表格会合并为一张表，PageStart~PageEnd 为其跨越的页码范围，重复表头只保留一次。
type Table struct {
	Rows       int        `json:"rows"`
	Cols       int        `json:"cols"`
	HeaderRows int        `json:"header_rows"`
	PageStart  int        `json:"page_start"`
	PageEnd    int        `json:"page_end"`
	Source     string     `json:"source"`
	Mandatory  bool       `json:"mandatory,omitempty"`
	Cells      []Cell     `json:"cells"`
	Grid       [][]string `json:"grid"`
	Markdown   string     `json:"markdown"`
}

// Cell 是一个原始单元格；Row/Col 为 0 基网格坐标，Page 为该单元格所在行的起始页。
type Cell struct {
	Row     int    `json:"row"`
	Col     int    `json:"col"`
	RowSpan int    `json:"row_span"`
	ColSpan int    `json:"col_span"`
	Text    string `json:"text"`
	Header  bool   `json:"header,omitempty"`
	Page    int    `json:"page"`
}

// tableBuilder 按行追加单元格并维护网格占用，用于还原横向（ColSpan）与纵向（RowSpan）合并。
type tableBuilder struct {
	cells    []Cell
	occupied map[[2]int]int // (row, col) -> cells 下标
	rows     int
	cols     int
	rowPages []int
}

func newTableBuilder() *tableBuilder {
	return &tableBuilder{occupied: map[[2]int]int{}}
}

// startRow 开始新的一行并返回其行号。
func (b *tableBuilder) startRow(page int) int {
	b.rows++
	b.rowPages = append(b.rowPages, page)
	return b.rows - 1
}

// nextFree 返回本行 col 起第一个未被上方纵向合并占用的列。
func (b *tableBuilder) nextFree(row, col int) int {
	for {
		if _, ok := b.occupied[[2]int{row, col}]; !ok {
			return col
		}
		col++
	}
}

// add 在 (row, col) 放置一个 colSpan 宽的单元格，返回其下标。
func (b *tableBuilder) add(row, col, colSpan int, text string) int {
	colSpan = max(colSpan, 1)
	b.cells = append(b.cells, Cell{Row: row, Col: col, RowSpan: 1, ColSpan: colSpan, Text: text, Page: b.rowPages[row]})
	idx := len(b.cells) - 1
	for c := col; c < col+colSpan; c++ {
		b.occupied[[2]int{row, c}] = idx
	}
	b.cols = max(b.cols, col+colSpan)
	return idx
}

// extendDown 把 (row-1, col) 处的单元格向下延伸一行（纵向合并的续行）；上方没有单元格时返回 false。
func (b *tableBuilder) extendDown(row, col int) (int, bool) {
	if row == 0 {
		return 0, false
	}
	idx, ok := b.occupied[[2]int{row - 1, col}]
	if !ok {
		return 0, false
	}
	c := &b.cells[idx]
	if c.Row+c.RowSpan != row {
		return 0, false
	}
	c.RowSpan++
	for cc := c.Col; cc < c.Col+c.ColSpan; cc++ {
		b.occupied[[2]int{row, cc}] = idx
	}
	return idx, true
}

func (b *tableBuilder) build(headerRows int, source string) *Table {
	t := &Table{Rows: b.rows, Cols: b.cols, HeaderRows: min(headerRows, b.rows), Source: source, Cells: b.cells}
	if t.Cells == nil {
		t.Cells = []Cell{}
	}
	t.PageStart, t.PageEnd = 0, 0
	for _, p := range b.rowPages {
		if t.PageStart == 0 || p < t.PageStart {
			t.PageStart = p
		}
		t.PageEnd = max(t.PageEnd, p)
	}
	t.finish()
	return t
}

// finish 根据 Cells 重新计算 Grid、Markdown、表头标记与强制性标记。
func (t *Table) finish() {
	t.Grid = make([][]string, t.Rows)
	md := make([][]string, t.Rows)
	for r := range t.Grid {
		t.Grid[r] = make([]string, t.Cols)
		md[r] = make([]string, t.Cols)
	}
	for i := range t.Cells {
		c := &t.Cells[i]
		c.Header = c.Row < t.HeaderRows
		if MandatoryPattern.MatchString(c.Text) {
			t.Mandatory = true
		}
		for r := c.Row; r < c.Row+c.RowSpan && r < t.Rows; r++ {
			for cc := c.Col; cc < c.Col+c.ColSpan && cc < t.Cols; cc++ {
				t.Grid[r][cc] = c.Text
				if cc == c.Col {
					md[r][cc] = c.Text
				}
			}
		}
	}
	t.Markdown = renderMarkdownTable(md)
}

func mdCell(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "<br>")
}

// renderMarkdownTable 以第一行为 GFM 表头渲染网格。
func renderMarkdownTable(grid [][]string) string {
	if len(grid) == 0 || len(grid[0]) == 0 {
		return ""
	}
	var b strings.Builder
	row := func(cells []string) {
		b.WriteString("|")
		for _, c := range cells {
			b.WriteString(" ")
			b.WriteString(mdCell(c))
			b.WriteString(" |")
		}
		b.WriteString("\n")
	}
	row(grid[0])
	b.WriteString("|")
	for range grid[0] {
		b.WriteString(" --- |")
	}
	b.WriteString("\n")
	for _, r := range grid[1:] {
		row(r)
	}
	return strings.TrimRight(b.String(), "\n")
}

// appendRows 把 next 的行接到 t 末尾（跨页续表）；dropHeader 为 true 时跳过 next 的前 HeaderRows 行重复表头。
func (t *Table) appendRows(next *Table, dropHeader bool) {
	skip := 0
	if dropHeader {
		skip = max(next.HeaderRows, 1)
	}
	offset := t.Rows - skip
	for _, c := range next.Cells {
		if c.Row < skip {
			continue
		}
		c.Row += offset
		t.Cells = append(t.Cells, c)
	}
	t.Rows += next.Rows - skip
	t.Cols = max(t.Cols, next.Cols)
	t.PageStart = min(t.PageStart, next.PageStart)
	t.PageEnd = max(t.PageEnd, next.PageEnd)
	t.Mandatory = false
	t.finish()
}

// rowTexts 返回展开网格的一行文本，用于比较跨页重复表头。
func (t *Table) rowText(r int) string {
	if r < 0 || r >= len(t.Grid) {
		return ""
	}
	return strings.Join(t.Grid[r], "\x1f")
}

// parseMarkdownTables 识别 Markdown 管道表格（表头行 + 分隔行 + 数据行）。
func parseMarkdownTable(lines []string) (*Table, bool) {
	if len(lines) < 2 || !isMDSeparator(lines[1]) {
		return nil, false
	}
	b := newTableBuilder()
	for i, l := range lines {
		if i == 1 {
			continue
		}
		r := b.startRow(1)
		for c, cell := range splitMDRow(l) {
			b.add(r, c, 1, cell)
		}
	}
	return b.build(1, "markdown"), true
}

func isMDSeparator(l string) bool {
	cells := splitMDRow(l)
	if len(cells) == 0 {
		return false
	}
	for _, c := range cells {
		c = strings.Trim(strings.TrimSpace(c), ":")
		if c == "" || strings.Trim(c, "-") != "" {
			return false
		}
	}
	return true
}

func splitMDRow(l string) []string {
	l = strings.TrimSpace(l)
	l = strings.TrimPrefix(l, "|")
	l = strings.TrimSuffix(l, "|")
	var (
		out []string
		cur strings.Builder
	)
	for i := 0; i < len(l); i++ {
		switch {
		case l[i] == '\\' && i+1 < len(l) && l[i+1] == '|':
			cur.WriteByte('|')
			i++
		case l[i] == '|':
			out = append(out, strings.TrimSpace(strings.ReplaceAll(cur.String(), "<br>", "\n")))
			cur.Reset()
		default:
			cur.WriteByte(l[i])
		}
	}
	return append(out, strings.TrimSpace(strings.ReplaceAll(cur.String(), "<br>", "\n")))
}
