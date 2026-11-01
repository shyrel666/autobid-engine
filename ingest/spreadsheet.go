package ingest

// XLSX / CSV / TSV 固定解析：保留工作表、A1 坐标、合并区域、原始值、显示值、公式缓存状态、
// 隐藏行列/工作表与编码/分隔符信息。表格类文档没有稳定物理页码，page_count/page_start 必须为空，
// 不得用占位页码冒充 PDF 页码。

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
)

var (
	// ErrSpreadsheetLimit 表示表格超过单元格/行列/工作表限额（HTTP 422，不猜测截断内容）。
	ErrSpreadsheetLimit = errors.New("ingest: spreadsheet exceeds limits")
	// ErrSpreadsheetInvalid 表示文件不是可解析的 XLSX/CSV/TSV（HTTP 422）。
	ErrSpreadsheetInvalid = errors.New("ingest: invalid spreadsheet")
)

// 固定解析限额（与 eval/file-processing/CONTRACT.md 同步）。
const (
	maxSpreadsheetSheets = 200
	maxSpreadsheetRows   = 200_000
	maxSpreadsheetCols   = 512
	maxSpreadsheetCells  = 2_000_000
	maxSpreadsheetWarn   = 200
	parserVersionXLSX    = "excelize/v2.9.1"
	// MaxTableWindowTokens 是行窗口的 token 估算上限；与 F0 样本冻结值一致。
	MaxTableWindowTokens = 600
)

// SourceLocator 是表格/单元格到原件的来源定位。Kind=spreadsheet 时没有物理页码。
type SourceLocator struct {
	Kind   string `json:"kind"`
	FileID int64  `json:"file_id,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Sheet  string `json:"sheet,omitempty"`
	Range  string `json:"range,omitempty"`
	Row    int    `json:"row,omitempty"`
	Column string `json:"column,omitempty"`
}

// SheetCoverage 汇总一个工作表的处理状态，供预检/预览显示缺口。
type SheetCoverage struct {
	Name    string `json:"name"`
	Hidden  bool   `json:"hidden,omitempty"`
	Rows    int    `json:"rows"`
	Cols    int    `json:"cols"`
	Windows int    `json:"windows,omitempty"`
	Empty   bool   `json:"empty,omitempty"`
	Note    string `json:"note,omitempty"`
}

func parseXLSX(f File) (*Parsed, error) {
	x, err := excelize.OpenReader(bytes.NewReader(f.Data), excelize.Options{
		UnzipSizeLimit:    256 << 20,
		UnzipXMLSizeLimit: 32 << 20,
	})
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "password") || strings.Contains(strings.ToLower(err.Error()), "encrypt") {
			return nil, fmt.Errorf("%w: 加密 XLSX 不支持；请移除密码后另存", ErrSpreadsheetInvalid)
		}
		return nil, fmt.Errorf("%w: %v", ErrSpreadsheetInvalid, err)
	}
	defer x.Close()
	date1904 := false
	if props, err := x.GetWorkbookProps(); err == nil && props.Date1904 != nil {
		date1904 = *props.Date1904
	}
	sheets := x.GetSheetList()
	if len(sheets) == 0 {
		return nil, fmt.Errorf("%w: xlsx 没有工作表", ErrSpreadsheetInvalid)
	}
	if len(sheets) > maxSpreadsheetSheets {
		return nil, fmt.Errorf("%w: sheets %d > %d", ErrSpreadsheetLimit, len(sheets), maxSpreadsheetSheets)
	}
	p := &Parsed{Format: FormatXLSX, MIME: formatMIME[FormatXLSX], Meta: ParseMeta{
		Parser: "excelize", ParserVersion: parserVersionXLSX, Pages: 0,
	}}
	totalCells := 0
	for idx, name := range sheets {
		hidden := false
		if vis, err := x.GetSheetVisible(name); err == nil {
			hidden = !vis
		}
		rows, err := x.GetRows(name, excelize.Options{ShortDatePattern: "yyyy-mm-dd", LongDatePattern: "yyyy-mm-dd hh:mm:ss"})
		if err != nil {
			return nil, fmt.Errorf("%w: sheet %q: %v", ErrSpreadsheetInvalid, name, err)
		}
		rawRows, err := x.GetRows(name, excelize.Options{RawCellValue: true})
		if err != nil {
			return nil, fmt.Errorf("%w: sheet %q raw: %v", ErrSpreadsheetInvalid, name, err)
		}
		if len(rows) > maxSpreadsheetRows {
			return nil, fmt.Errorf("%w: sheet %q rows %d > %d", ErrSpreadsheetLimit, name, len(rows), maxSpreadsheetRows)
		}
		cols := 0
		for _, r := range rows {
			cols = max(cols, len(r))
		}
		for _, r := range rawRows {
			cols = max(cols, len(r))
		}
		if cols > maxSpreadsheetCols {
			return nil, fmt.Errorf("%w: sheet %q cols %d > %d", ErrSpreadsheetLimit, name, cols, maxSpreadsheetCols)
		}
		totalCells += len(rows) * max(cols, 1)
		if totalCells > maxSpreadsheetCells {
			return nil, fmt.Errorf("%w: cells > %d", ErrSpreadsheetLimit, maxSpreadsheetCells)
		}
		table, warnings := buildXLSXSheet(x, name, rows, rawRows, cols, f, date1904)
		if len(warnings) > 0 {
			p.Meta.Warnings = appendCapped(p.Meta.Warnings, warnings...)
		}
		table.Sheet, table.SheetIndex, table.SheetHidden = name, idx, hidden
		table.PageIsPhysical = boolPtr(false)
		table.Source = FormatXLSX
		table.finish()
		table.HeaderRows = detectHeaderRows(table)
		table.finish()
		table.Range = tableRange(table)
		table.SourceLocator = &SourceLocator{Kind: "spreadsheet", FileID: f.ObjectID, SHA256: f.SHA256, Sheet: name, Range: table.Range}
		p.SheetTables = append(p.SheetTables, table)
		empty := table.Rows == 0 || table.Cols == 0 || len(table.Cells) == 0
		cov := SheetCoverage{Name: name, Hidden: hidden, Rows: table.Rows, Cols: table.Cols, Empty: empty,
			Windows: len(sheetWindows(table))}
		if hidden {
			p.Meta.Warnings = appendCapped(p.Meta.Warnings, "hidden_sheet:"+name)
			cov.Note = "hidden sheet retained"
		}
		if empty {
			p.Meta.Warnings = appendCapped(p.Meta.Warnings, "empty_sheet:"+name)
		}
		p.Meta.Sheets = append(p.Meta.Sheets, cov)
	}
	p.Meta.Tables = len(p.SheetTables)
	return p, nil
}

func buildXLSXSheet(x *excelize.File, name string, rows, rawRows [][]string, cols int, f File, date1904 bool) (*Table, []string) {
	var warnings []string
	cells := []Cell{}
	rowCount := max(len(rows), len(rawRows))
	hidden := map[int]bool{}
	for r := 1; r <= rowCount; r++ {
		vis, err := x.GetRowVisible(name, r)
		if err == nil && !vis {
			hidden[r] = true
		}
	}
	hiddenCols := []string{}
	for c := 1; c <= cols; c++ {
		colName, _ := excelize.ColumnNumberToName(c)
		if vis, err := x.GetColVisible(name, colName); err == nil && !vis {
			hiddenCols = append(hiddenCols, colName)
		}
	}
	for r := 0; r < rowCount; r++ {
		for c := 0; c < cols; c++ {
			display := cellAt(rows, r, c)
			raw := cellAt(rawRows, r, c)
			cellName, _ := excelize.CoordinatesToCellName(c+1, r+1)
			formula, _ := x.GetCellFormula(name, cellName)
			if display == "" && raw == "" && formula == "" {
				continue
			}
			if raw != "" {
				if styleID, err := x.GetCellStyle(name, cellName); err == nil {
					if style, err := x.GetStyle(styleID); err == nil {
						if iso, ok := excelDateValue(raw, style, date1904); ok {
							display = iso
						}
					}
				}
			}
			cell := Cell{Row: r, Col: c, RowSpan: 1, ColSpan: 1, Text: display, Display: display, Raw: raw,
				Hidden: hidden[r+1] || containsString(hiddenCols, columnName(c))}
			if formula != "" {
				cell.Formula = strings.TrimPrefix(formula, "=")
				cell.FormulaCached = raw != "" || display != ""
				if !cell.FormulaCached {
					warnings = appendCapped(warnings, fmt.Sprintf("formula_cache_missing:%s!%s", name, cellName))
				}
			}
			cells = append(cells, cell)
		}
	}
	table := &Table{Rows: rowCount, Cols: cols, Source: FormatXLSX, Cells: cells}
	table.Merges = mergeRefs(x, name)
	applyMerges(table, x, name)
	table.HiddenRows = sortedIntKeys(hidden)
	table.HiddenCols = hiddenCols
	return table, warnings
}

func mergeRefs(x *excelize.File, sheet string) []string {
	merges, err := x.GetMergeCells(sheet)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(merges))
	for _, m := range merges {
		if len(m) > 0 && strings.TrimSpace(m[0]) != "" {
			out = append(out, m[0])
		}
	}
	return out
}

func applyMerges(t *Table, x *excelize.File, sheet string) {
	merges, err := x.GetMergeCells(sheet)
	if err != nil {
		return
	}
	covered := map[[2]int]int{}
	for _, m := range merges {
		if len(m) == 0 {
			continue
		}
		ref := strings.TrimSpace(m[0])
		parts := strings.Split(ref, ":")
		if len(parts) != 2 {
			continue
		}
		c1, r1, err1 := excelize.CellNameToCoordinates(parts[0])
		c2, r2, err2 := excelize.CellNameToCoordinates(parts[1])
		if err1 != nil || err2 != nil {
			continue
		}
		r1, r2, c1, c2 = r1-1, r2-1, c1-1, c2-1
		if r2 < r1 || c2 < c1 {
			continue
		}
		idx := cellIndex(t.Cells, r1, c1)
		if idx < 0 {
			text := ""
			if len(m) > 1 {
				text = m[1]
			}
			if strings.TrimSpace(text) == "" {
				continue
			}
			t.Cells = append(t.Cells, Cell{Row: r1, Col: c1, RowSpan: 1, ColSpan: 1, Text: text, Display: text})
			idx = len(t.Cells) - 1
		}
		t.Cells[idx].RowSpan = r2 - r1 + 1
		t.Cells[idx].ColSpan = c2 - c1 + 1
		for r := r1; r <= r2; r++ {
			for c := c1; c <= c2; c++ {
				if r == r1 && c == c1 {
					continue
				}
				covered[[2]int{r, c}] = idx
			}
		}
	}
	if len(covered) > 0 {
		out := t.Cells[:0]
		for _, c := range t.Cells {
			if _, ok := covered[[2]int{c.Row, c.Col}]; ok {
				continue
			}
			out = append(out, c)
		}
		t.Cells = out
	}
}

func cellIndex(cells []Cell, row, col int) int {
	for i := range cells {
		if cells[i].Row == row && cells[i].Col == col {
			return i
		}
	}
	return -1
}

func cellAt(rows [][]string, r, c int) string {
	if r < len(rows) && c < len(rows[r]) {
		return rows[r][c]
	}
	return ""
}

func columnName(col int) string {
	name, _ := excelize.ColumnNumberToName(col + 1)
	return name
}

// parseDelimited 解析 CSV / TSV：保留编码、分隔符、行列位置与原始文本；前导零和长编号不转数字。
func parseDelimited(f File, format string) (*Parsed, error) {
	text, enc := DecodeText(f.Data)
	delim := detectDelimiter(text, format)
	r := csv.NewReader(strings.NewReader(text))
	r.Comma = delim
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSpreadsheetInvalid, err)
	}
	if len(records) > maxSpreadsheetRows {
		return nil, fmt.Errorf("%w: rows %d > %d", ErrSpreadsheetLimit, len(records), maxSpreadsheetRows)
	}
	cols := 0
	ragged := false
	for _, rec := range records {
		cols = max(cols, len(rec))
	}
	for _, rec := range records {
		if len(rec) != cols {
			ragged = true
			break
		}
	}
	if cols > maxSpreadsheetCols {
		return nil, fmt.Errorf("%w: cols %d > %d", ErrSpreadsheetLimit, cols, maxSpreadsheetCols)
	}
	p := &Parsed{Format: format, MIME: formatMIME[format], Meta: ParseMeta{Parser: format, ParserVersion: "stdlib/csv",
		Encoding: enc, Delimiter: string(delim), Pages: 0, Sheets: []SheetCoverage{}}}
	if ragged {
		p.Meta.Warnings = append(p.Meta.Warnings, "ragged_rows")
	}
	table := &Table{Rows: len(records), Cols: cols, Source: format, PageIsPhysical: boolPtr(false)}
	for r, rec := range records {
		for c, v := range rec {
			table.Cells = append(table.Cells, Cell{Row: r, Col: c, RowSpan: 1, ColSpan: 1, Text: v, Display: v, Raw: v})
		}
	}
	table.finish()
	table.HeaderRows = detectHeaderRows(table)
	table.finish()
	sheet := strings.TrimSuffix(filepath.Base(f.Name), filepath.Ext(f.Name))
	if sheet == "" {
		sheet = strings.ToUpper(format)
	}
	table.Sheet = sheet
	table.SheetIndex = 0
	table.Range = tableRange(table)
	table.SourceLocator = &SourceLocator{Kind: "spreadsheet", FileID: f.ObjectID, SHA256: f.SHA256, Sheet: sheet, Range: table.Range}
	p.SheetTables = []*Table{table}
	p.Meta.Tables = 1
	p.Meta.Sheets = append(p.Meta.Sheets, SheetCoverage{Name: sheet, Rows: table.Rows, Cols: table.Cols,
		Empty: table.Rows == 0, Windows: len(sheetWindows(table))})
	if enc != "utf-8" {
		p.Meta.Warnings = append(p.Meta.Warnings, "encoding:"+enc)
	}
	if table.Rows == 0 {
		p.Meta.Warnings = append(p.Meta.Warnings, "empty_sheet:"+sheet)
	}
	return p, nil
}

// detectDelimiter 对 .csv 用前几条记录试读逗号、分号、Tab，取列数最多者；.tsv 固定 Tab。
func detectDelimiter(text, format string) rune {
	if format == FormatTSV {
		return '\t'
	}
	best, bestCols := ',', 0
	for _, d := range []rune{',', ';', '\t'} {
		r := csv.NewReader(strings.NewReader(text))
		r.Comma = d
		r.FieldsPerRecord = -1
		r.LazyQuotes = true
		if rec, err := r.Read(); err == nil && len(rec) > bestCols {
			best, bestCols = d, len(rec)
		}
	}
	return best
}

// detectHeaderRows 返回包含标题行在内的表头行数：优先取前 5 行中第一行有至少两个非空单元格的行，
// 其之前的合并标题行也重复到每个窗口。
func detectHeaderRows(t *Table) int {
	if t.Rows == 0 {
		return 0
	}
	limit := min(t.Rows, 5)
	for r := 0; r < limit; r++ {
		n := 0
		for _, c := range t.Cells {
			if c.Row == r && strings.TrimSpace(c.Text) != "" {
				n++
			}
		}
		if n >= 2 {
			return r + 1
		}
	}
	return 0
}

func tableRange(t *Table) string {
	if t == nil || t.Rows == 0 || t.Cols == 0 {
		return ""
	}
	return fmt.Sprintf("A1:%s%d", columnName(t.Cols-1), t.Rows)
}

// spreadsheetSections 按工作表 → 行窗口切分，窗口重复表头；每个窗口是一段可检索/可定位的表格。
func spreadsheetSections(tables []*Table) []Section {
	var out []Section
	for _, t := range tables {
		out = append(out, sheetWindows(t)...)
	}
	return out
}

const maxTableWindowRows = 40

func sheetWindows(t *Table) []Section {
	if t == nil || t.Rows == 0 || len(t.Cells) == 0 {
		return nil
	}
	headerRows := min(max(t.HeaderRows, 0), t.Rows)
	start := headerRows
	if start >= t.Rows {
		start = t.Rows - 1
	}
	var out []Section
	for start < t.Rows {
		rows := []int{}
		end := start
		budget := 0
		for end < t.Rows && len(rows) < maxTableWindowRows {
			rows = append(rows, end)
			probe := windowTable(t, headerRows, rows)
			probe.finish()
			budget = EstimateTokens(probe.Markdown)
			if budget > MaxTableWindowTokens && len(rows) > 1 {
				rows = rows[:len(rows)-1]
				end--
				break
			}
			end++
		}
		if len(rows) == 0 {
			break
		}
		last := rows[len(rows)-1]
		w := windowTable(t, headerRows, rows)
		w.HeaderRows = headerRows
		w.finish()
		w.Range = windowRange(t, headerRows, rows)
		w.SourceLocator = &SourceLocator{Kind: "spreadsheet", Sheet: t.Sheet, Range: w.Range}
		if t.SourceLocator != nil {
			w.SourceLocator.FileID, w.SourceLocator.SHA256 = t.SourceLocator.FileID, t.SourceLocator.SHA256
		}
		title := fmt.Sprintf("%s 行 %d–%d", t.Sheet, start+1, last+1)
		sec := Section{Level: 1, Code: fmt.Sprintf("%s!%d-%d", t.Sheet, start+1, last+1), Title: title,
			PageStart: 0, PageEnd: 0, Content: w.Markdown, Tables: []*Table{w}, BlockStart: headerRows, BlockEnd: last}
		sec.Mandatory = MandatoryPattern.MatchString(sec.Content) || tablesMandatory(sec.Tables)
		out = append(out, sec)
		start = last + 1
	}
	return out
}

func windowTable(t *Table, headerRows int, rows []int) *Table {
	want := map[int]bool{}
	for r := 0; r < headerRows && r < t.Rows; r++ {
		want[r] = true
	}
	for _, r := range rows {
		want[r] = true
	}
	w := &Table{Rows: 0, Cols: t.Cols, HeaderRows: headerRows, Source: t.Source, Sheet: t.Sheet, SheetIndex: t.SheetIndex,
		SheetHidden: t.SheetHidden, Merges: t.Merges, HiddenRows: t.HiddenRows, HiddenCols: t.HiddenCols,
		PageIsPhysical: t.PageIsPhysical}
	rowMap := map[int]int{}
	for _, r := range sortedIntKeys(want) {
		rowMap[r] = len(rowMap)
	}
	w.Rows = len(rowMap)
	// 合并单元格可能从窗口上方跨入窗口：按覆盖关系保留一次并裁剪 RowSpan。
	for _, c := range t.Cells {
		span := max(c.RowSpan, 1)
		var inside []int
		for r := c.Row; r < c.Row+span; r++ {
			if want[r] {
				inside = append(inside, r)
			}
		}
		if len(inside) == 0 {
			continue
		}
		cc := c
		cc.Row = rowMap[inside[0]]
		cc.RowSpan = len(inside)
		w.Cells = append(w.Cells, cc)
	}
	return w
}

func windowRange(t *Table, headerRows int, rows []int) string {
	if len(rows) == 0 || t.Cols == 0 {
		return ""
	}
	return fmt.Sprintf("A%d:%s%d", rows[0]+1, columnName(t.Cols-1), rows[len(rows)-1]+1)
}

func appendCapped(dst []string, vals ...string) []string {
	for _, v := range vals {
		if len(dst) >= maxSpreadsheetWarn {
			return dst
		}
		dst = append(dst, v)
	}
	return dst
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func boolPtr(v bool) *bool { return &v }

func sortedIntKeys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// excelDateValue 把日期/时间样式单元格的原始序列号规范为 ISO 文本；无法判定时返回 false，
// 此时保留 Excel 自带的显示值，原始序列号仍在 Cell.Raw 中。
func excelDateValue(raw string, style *excelize.Style, date1904 bool) (string, bool) {
	serial, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return "", false
	}
	isDate, isTime := dateStyleKind(style)
	if !isDate && !isTime {
		return "", false
	}
	base := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)
	if date1904 {
		base = time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	days := int(serial)
	frac := serial - float64(days)
	t := base.AddDate(0, 0, days).Add(time.Duration(frac * float64(24*time.Hour)))
	switch {
	case isDate && (isTime || frac > 0):
		return t.Format("2006-01-02 15:04:05"), true
	case isDate:
		return t.Format("2006-01-02"), true
	default:
		return t.Format("15:04:05"), true
	}
}

func dateStyleKind(style *excelize.Style) (isDate, isTime bool) {
	if style == nil {
		return false, false
	}
	if style.NumFmt >= 14 && style.NumFmt <= 17 {
		return true, false
	}
	if style.NumFmt >= 18 && style.NumFmt <= 21 || style.NumFmt >= 45 && style.NumFmt <= 47 {
		return false, true
	}
	if style.NumFmt == 22 {
		return true, true
	}
	code := ""
	if style.CustomNumFmt != nil {
		code = strings.ToLower(*style.CustomNumFmt)
	}
	if code == "" {
		return false, false
	}
	// 去掉引号文本与颜色/条件段，避免把 "年月日" 字面量误判为日期。
	var clean strings.Builder
	quoted, bracket := false, false
	for _, r := range code {
		switch r {
		case '"':
			if !bracket {
				quoted = !quoted
			}
			continue
		case '[':
			bracket = true
			continue
		case ']':
			bracket = false
			continue
		}
		if !quoted && !bracket {
			clean.WriteRune(r)
		}
	}
	code = clean.String()
	isDate = strings.ContainsAny(code, "yd") || strings.Contains(code, "m月") || strings.Contains(code, "e")
	isTime = strings.ContainsAny(code, "hs") || strings.Contains(code, "m:s")
	return isDate, isTime
}
