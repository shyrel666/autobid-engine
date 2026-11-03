package export

// 成稿 .docx 生成（WordprocessingML）：
//   - 目录：TOC 域（\o "1-3" \h \z \u），未刷新时的缓存结果是一段提示文字，由文档引擎（WPS / Word）刷新为带页码的条目；
//   - 标题：章节编码 + 标题写入「标题 N」样式（大纲级别 = 章节层级，正文内的 Markdown 标题降为不进目录的小标题），
//     一级标题另起一页（紧跟分节符时不再额外分页，避免空白页）；
//   - 正文 Markdown：段落、加粗 / 斜体 / 行内代码、有序 / 无序列表（缩进表示层级）、引用、代码块与 GFM 表格；
//   - 表格：表宽 = 版芯宽、固定布局、列宽按内容估算比例分配（保证最小列宽）、表头行跨页重复；
//     列数 >= 10 或最小列宽之和超出纵向版芯时放进横向页面分节；
//   - 图表：PNG 按版芯宽高等比缩放后内嵌（宽高比 > 2.4 的放进横向分节），未渲染的 Mermaid 插入源码占位框，
//     设备选型表渲染为原生表格；
//   - 《点对点应答及偏离表》：来自导出快照中的 compliance_matrix 行。

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"image"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/autobid/autobid-engine/compliance"
	"github.com/autobid/autobid-engine/db"
)

// TOCPlaceholder 是未刷新目录的缓存文字（回读时用来判定目录未刷新）。
const TOCPlaceholder = "目录待更新：请在 Word 中右键目录选择「更新域」"

// DeviationTitle 是偏离表章节标题。
const DeviationTitle = "附录 点对点应答及偏离表"

// BuildInput 是一次生成的全部输入（均来自导出快照）。
type BuildInput struct {
	Doc         db.DocSnapshot
	Bodies      map[int64]string // 修订 ID -> Markdown 正文
	Charts      []db.SnapshotChart
	ChartImages map[int64][]byte // 图表 ID -> PNG
	Matrix      []db.SnapshotMatrixRow
	Bidder      string
	Date        string
	// UpdateFieldsOnOpen 在 settings.xml 中要求 Word 打开时更新域（目录未经文档引擎刷新时使用）。
	UpdateFieldsOnOpen bool
}

// BuildResult 是生成结果。
type BuildResult struct {
	Data       []byte
	Warnings   []db.ExportWarning
	Headings   []string // 写入的标题文字（按顺序，含偏离表标题）
	MatrixRows int
	Landscape  int // 横向分节数
}

type block struct {
	table     bool
	pPr       string // 段落属性（不含 sectPr）
	content   string // 段落 runs 或完整表格 XML
	sticky    bool   // 标题 / 题注：分节时与后续内容同节
	pageBreak bool   // 段前分页（紧跟分节符时省略）
	sectPr    string // 本段结束一节
}

type rel struct{ id, typ, target string }

type docWriter struct {
	t         *Template
	styles    map[string]string
	geom      Geometry
	blocks    []block
	orient    string
	sections  int
	rels      []rel
	media     map[string][]byte
	nextDocPr int
	fig, tbl  int
	warnings  []db.ExportWarning
	headings  []string
}

func (w *docWriter) style(name string) string { return w.styles[name] }

func (w *docWriter) textWidth() int {
	if w.orient == "landscape" {
		return w.geom.Landscape().TextWidth()
	}
	return w.geom.TextWidth()
}

func (w *docWriter) textHeight() int {
	if w.orient == "landscape" {
		return w.geom.Landscape().TextHeight()
	}
	return w.geom.TextHeight()
}

// ---------------------------------------------------------------------------
// 分节
// ---------------------------------------------------------------------------

var (
	rePgSz      = regexp.MustCompile(`<w:pgSz\b[^>]*/>`)
	reSectType  = regexp.MustCompile(`<w:type\b[^>]*/>`)
	reTitlePg   = regexp.MustCompile(`<w:titlePg\b[^>]*/>`)
	rePgNumType = regexp.MustCompile(`<w:pgNumType\b[^>]*/>`)
)

// sectPrFor 由模板最后一节派生指定方向的分节属性（下一页开始、不重复首页不同、不重新起始页码）。
func (w *docWriter) sectPrFor(orient string) string {
	s := w.t.sectPr
	g := w.geom
	if orient == "landscape" {
		g = g.Landscape()
	}
	pg := fmt.Sprintf(`<w:pgSz w:w="%d" w:h="%d"/>`, g.PageW, g.PageH)
	if orient == "landscape" {
		pg = fmt.Sprintf(`<w:pgSz w:w="%d" w:h="%d" w:orient="landscape"/>`, g.PageW, g.PageH)
	}
	s = rePgSz.ReplaceAllString(s, pg)
	s = reTitlePg.ReplaceAllString(s, "")
	s = reSectType.ReplaceAllString(s, "")
	if w.sections > 0 {
		s = rePgNumType.ReplaceAllStringFunc(s, func(m string) string {
			return regexp.MustCompile(`\s+w:start="[^"]*"`).ReplaceAllString(m, "")
		})
	}
	// w:type 必须是 sectPr 的靠前子元素：放在页眉页脚引用之后、其余属性之前。
	i := strings.Index(s, ">") + 1
	for {
		j := strings.Index(s[i:], "<w:")
		if j < 0 {
			break
		}
		k := i + j
		if !strings.HasPrefix(s[k:], "<w:headerReference") && !strings.HasPrefix(s[k:], "<w:footerReference") {
			i = k
			break
		}
		i = k + strings.Index(s[k:], ">") + 1
	}
	return s[:i] + `<w:type w:val="nextPage"/>` + s[i:]
}

// setOrient 切换页面方向：在当前内容末尾结束一节。末尾的标题 / 题注（sticky）随后续内容进入新节。
func (w *docWriter) setOrient(orient string) {
	if orient == w.orient {
		return
	}
	end := len(w.blocks)
	for end > 0 && w.blocks[end-1].sticky {
		end--
	}
	sect := w.sectPrFor(w.orient)
	if end > 0 && !w.blocks[end-1].table && w.blocks[end-1].sectPr == "" {
		w.blocks[end-1].sectPr = sect
	} else {
		tiny := block{pPr: `<w:spacing w:before="0" w:after="0" w:line="20" w:lineRule="exact"/><w:rPr><w:sz w:val="2"/></w:rPr>`, sectPr: sect}
		w.blocks = append(w.blocks[:end], append([]block{tiny}, w.blocks[end:]...)...)
	}
	w.sections++
	w.orient = orient
}

// ---------------------------------------------------------------------------
// 段落与文字
// ---------------------------------------------------------------------------

func esc(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(stripControl(s)))
	return b.String()
}

func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' {
			return r
		}
		if r < 0x20 || r == 0xFFFE || r == 0xFFFF || (r >= 0xD800 && r <= 0xDFFF) {
			return -1
		}
		return r
	}, s)
}

type runStyle struct{ bold, italic, code bool }

// eastAsianSymbols 是 Unicode 宽度不定的符号：不加 w:hint="eastAsia" 时 Word / WPS 按西文字体排版，
// 西文字体缺字形便回退到 Segoe UI Symbol / MS PGothic；加上后改用中文字体（宋体有这些字形）。
const eastAsianSymbols = "★☆▲△■□●○◆◇※"

func run(text string, st runStyle) string {
	if text == "" {
		return ""
	}
	var rpr strings.Builder
	switch {
	case st.code:
		rpr.WriteString(`<w:rFonts w:ascii="Consolas" w:hAnsi="Consolas" w:cs="Consolas"/>`)
	case strings.ContainsAny(text, eastAsianSymbols):
		rpr.WriteString(`<w:rFonts w:hint="eastAsia"/>`)
	}
	if st.bold {
		rpr.WriteString(`<w:b/><w:bCs/>`)
	}
	if st.italic {
		rpr.WriteString(`<w:i/><w:iCs/>`)
	}
	var b strings.Builder
	b.WriteString("<w:r>")
	if rpr.Len() > 0 {
		b.WriteString("<w:rPr>" + rpr.String() + "</w:rPr>")
	}
	parts := strings.Split(text, "\n")
	for i, p := range parts {
		if i > 0 {
			b.WriteString("<w:br/>")
		}
		segs := strings.Split(p, "\t")
		for j, sg := range segs {
			if j > 0 {
				b.WriteString("<w:tab/>")
			}
			if sg != "" {
				b.WriteString(`<w:t xml:space="preserve">` + esc(sg) + `</w:t>`)
			}
		}
	}
	b.WriteString("</w:r>")
	return b.String()
}

var reInline = regexp.MustCompile("(\\*\\*[^*]+\\*\\*|__[^_]+__|`[^`]+`|\\*[^*\\s][^*]*\\*|\\[[^\\]]+\\]\\([^)]*\\)|<br\\s*/?>)")

// inline 把一行 Markdown 行内格式转换为 runs。
func inline(s string, base runStyle) string {
	var b strings.Builder
	last := 0
	for _, m := range reInline.FindAllStringIndex(s, -1) {
		b.WriteString(run(s[last:m[0]], base))
		tok := s[m[0]:m[1]]
		st := base
		switch {
		case strings.HasPrefix(tok, "**") || strings.HasPrefix(tok, "__"):
			st.bold = true
			b.WriteString(inline(tok[2:len(tok)-2], st))
		case strings.HasPrefix(tok, "`"):
			st.code = true
			b.WriteString(run(tok[1:len(tok)-1], st))
		case strings.HasPrefix(tok, "["):
			b.WriteString(run(tok[1:strings.Index(tok, "]")], base))
		case strings.HasPrefix(tok, "<br"):
			b.WriteString(`<w:r><w:br/></w:r>`)
		default:
			st.italic = true
			b.WriteString(inline(tok[1:len(tok)-1], st))
		}
		last = m[1]
	}
	b.WriteString(run(s[last:], base))
	return b.String()
}

// plain 去掉行内 Markdown 标记（用于宽度估算与标题比较）。
func plain(s string) string {
	s = reInline.ReplaceAllStringFunc(s, func(tok string) string {
		switch {
		case strings.HasPrefix(tok, "**") || strings.HasPrefix(tok, "__"):
			return tok[2 : len(tok)-2]
		case strings.HasPrefix(tok, "`"), strings.HasPrefix(tok, "*"):
			return tok[1 : len(tok)-1]
		case strings.HasPrefix(tok, "["):
			return tok[1:strings.Index(tok, "]")]
		}
		return " "
	})
	return strings.TrimSpace(s)
}

func (w *docWriter) para(style, extraPPr, runs string) {
	ppr := ""
	if style != "" {
		ppr = `<w:pStyle w:val="` + style + `"/>`
	}
	w.blocks = append(w.blocks, block{pPr: ppr + extraPPr, content: runs})
}

func (w *docWriter) heading(level int, text string, pageBreak bool) {
	level = min(max(level, 1), 6)
	w.blocks = append(w.blocks, block{pPr: `<w:pStyle w:val="` + w.style(fmt.Sprintf("heading %d", level)) + `"/>`,
		content: run(text, runStyle{}), sticky: true, pageBreak: pageBreak})
	w.headings = append(w.headings, text)
}

// ---------------------------------------------------------------------------
// Markdown 正文
// ---------------------------------------------------------------------------

var (
	reMDHeading = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*#*\s*$`)
	reMDList    = regexp.MustCompile(`^(\s*)([-*+]|\d{1,3}[.)])\s+(.*)$`)
	reMDRule    = regexp.MustCompile(`^\s*(?:(?:-\s*){3,}|(?:\*\s*){3,}|(?:_\s*){3,})$`)
	reNumPrefix = regexp.MustCompile(`^[\d.．、\s]+`)
)

func normTitle(s string) string {
	s = plain(s)
	s = reNumPrefix.ReplaceAllString(s, "")
	return strings.Join(strings.Fields(s), "")
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || (r >= 0x3000 && r <= 0x303F) || (r >= 0xFF00 && r <= 0xFFEF)
}

// joinLines 合并软换行：CJK 之间不加空格。
func joinLines(lines []string) string {
	var b strings.Builder
	for i, l := range lines {
		l = strings.TrimSpace(l)
		if i > 0 && b.Len() > 0 {
			pr, _ := utf8.DecodeLastRuneInString(b.String())
			nr, _ := utf8.DecodeRuneInString(l)
			if !isCJK(pr) || !isCJK(nr) {
				b.WriteByte(' ')
			}
		}
		b.WriteString(l)
	}
	return b.String()
}

// markdown 渲染一个章节的正文；sectionTitle 用于跳过与章节标题重复的首个 Markdown 标题。
func (w *docWriter) markdown(md, sectionTitle string) {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	var para []string
	flush := func() {
		if len(para) > 0 {
			w.para("", "", inline(joinLines(para), runStyle{}))
			para = nil
		}
	}
	first := true
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		t := strings.TrimSpace(l)
		if t != "" && !(strings.HasPrefix(t, "|") && i+1 < len(lines) && isTableSep(lines[i+1])) {
			w.setOrient("portrait")
		}
		switch {
		case t == "":
			flush()
			continue
		case strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~"):
			flush()
			fence := t[:3]
			var code []string
			for i++; i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), fence); i++ {
				code = append(code, lines[i])
			}
			for _, c := range code {
				w.para(w.style("autobid code"), "", run(c, runStyle{}))
			}
		case reMDHeading.MatchString(t):
			flush()
			m := reMDHeading.FindStringSubmatch(t)
			if first && normTitle(m[2]) == normTitle(sectionTitle) {
				break
			}
			w.blocks = append(w.blocks, block{pPr: `<w:pStyle w:val="` + w.style("autobid subheading") + `"/>`, content: inline(m[2], runStyle{}), sticky: true})
		case reMDRule.MatchString(t):
			flush()
		case strings.HasPrefix(t, "|") && i+1 < len(lines) && isTableSep(lines[i+1]):
			flush()
			var rows []string
			for ; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "|"); i++ {
				rows = append(rows, strings.TrimSpace(lines[i]))
			}
			i--
			header := splitRow(rows[0])
			var body [][]string
			for _, r := range rows[2:] {
				cells := splitRow(r)
				for len(cells) < len(header) {
					cells = append(cells, "")
				}
				body = append(body, cells[:len(header)])
			}
			w.table("", header, body, nil)
		case reMDList.MatchString(l):
			flush()
			m := reMDList.FindStringSubmatch(l)
			level := len(strings.ReplaceAll(m[1], "\t", "    ")) / 2
			marker := "•"
			if m[2][0] >= '0' && m[2][0] <= '9' {
				marker = strings.TrimRight(m[2], ".)") + "."
			}
			left := 420 + 420*min(level, 4)
			w.para(w.style("autobid list"), fmt.Sprintf(`<w:ind w:leftChars="0" w:left="%d" w:firstLineChars="0" w:firstLine="0"/>`, left),
				run(marker+" ", runStyle{})+inline(m[3], runStyle{}))
		case strings.HasPrefix(t, ">"):
			flush()
			w.para("", `<w:ind w:leftChars="0" w:left="420" w:firstLineChars="0" w:firstLine="0"/>`, inline(strings.TrimSpace(strings.TrimLeft(t, "> ")), runStyle{italic: true}))
		default:
			para = append(para, t)
		}
		first = false
	}
	flush()
}

func isTableSep(l string) bool {
	l = strings.TrimSpace(l)
	if !strings.HasPrefix(l, "|") {
		return false
	}
	for _, c := range splitRow(l) {
		c = strings.Trim(strings.TrimSpace(c), ":")
		if c == "" || strings.Trim(c, "-") != "" {
			return false
		}
	}
	return true
}

func splitRow(l string) []string {
	l = strings.TrimSpace(l)
	l = strings.TrimPrefix(l, "|")
	l = strings.TrimSuffix(l, "|")
	var cells []string
	var cur strings.Builder
	for i := 0; i < len(l); i++ {
		switch {
		case l[i] == '\\' && i+1 < len(l) && l[i+1] == '|':
			cur.WriteByte('|')
			i++
		case l[i] == '|':
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(l[i])
		}
	}
	return append(cells, strings.TrimSpace(cur.String()))
}

// ---------------------------------------------------------------------------
// 表格
// ---------------------------------------------------------------------------

const (
	tableUnitTw = 100 // 表格正文 9pt：一个半角单位按 5pt 估算（汉字 = 2 单位，留出字宽与边框余量）
	cellPadTw   = 170 // 左右单元格边距（85 + 85）
)

// textUnits 估算显示宽度（半角 = 1，全角 / 汉字 = 2）。
func textUnits(s string) int {
	n := 0
	for _, r := range s {
		if isCJK(r) || unicode.Is(unicode.Hangul, r) || r > 0x2E80 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// longestToken 是不可断开的最长片段（连续半角字符）；汉字可在任意位置断行。
func longestToken(s string) int {
	best, cur := 0, 0
	for _, r := range s {
		if unicode.IsSpace(r) || isCJK(r) || r > 0x2E80 {
			cur = 0
			if isCJK(r) || r > 0x2E80 {
				best = max(best, 2)
			}
			continue
		}
		cur++
		best = max(best, cur)
	}
	return best
}

// columnWidths 按内容为列分配宽度（twip），并判断是否需要横向页面。
func columnWidths(header []string, rows [][]string, portraitWidth int) (desired, minimum []float64, landscape bool) {
	n := len(header)
	desired, minimum = make([]float64, n), make([]float64, n)
	minSum := 0.0
	for c := 0; c < n; c++ {
		h := float64(textUnits(plain(header[c])))
		maxLen, total, cnt, tok := 0.0, 0.0, 0.0, float64(longestToken(plain(header[c])))
		for _, r := range rows {
			if c >= len(r) {
				continue
			}
			p := plain(r[c])
			u := float64(textUnits(p))
			maxLen = max(maxLen, u)
			total += u
			cnt++
			tok = max(tok, float64(longestToken(p)))
		}
		avg := 0.0
		if cnt > 0 {
			avg = total / cnt
		}
		desired[c] = min(max(max(min(h, 16), 0.6*maxLen+0.4*avg), 4), 60)
		minimum[c] = min(max(max(min(h, 8), tok)+1, 4), 17)
		minSum += minimum[c]*tableUnitTw + cellPadTw
	}
	landscape = n >= 10 || minSum > float64(portraitWidth)
	return desired, minimum, landscape
}

func fitWidths(desired, minimum []float64, width int) []int {
	n := len(desired)
	sum := 0.0
	for _, d := range desired {
		sum += d
	}
	w := make([]float64, n)
	for i := range w {
		w[i] = float64(width) * desired[i] / sum
	}
	// 保证最小列宽：从最宽的列借。
	for iter := 0; iter < 3*n; iter++ {
		moved := false
		for i := range w {
			need := minimum[i]*tableUnitTw + cellPadTw
			if w[i] >= need {
				continue
			}
			wide := 0
			for j := range w {
				if w[j]-(minimum[j]*tableUnitTw+cellPadTw) > w[wide]-(minimum[wide]*tableUnitTw+cellPadTw) {
					wide = j
				}
			}
			spare := w[wide] - (minimum[wide]*tableUnitTw + cellPadTw)
			take := min(need-w[i], spare)
			if take <= 0 {
				break
			}
			w[i] += take
			w[wide] -= take
			moved = true
		}
		if !moved {
			break
		}
	}
	out := make([]int, n)
	total := 0
	for i := range w {
		out[i] = int(w[i])
		total += out[i]
	}
	out[n-1] += width - total
	return out
}

// tableOpts 允许调用方指定列权重（偏离表）。
type tableOpts struct {
	weights []float64
}

// table 写入一个表格（caption 非空时题注在表上方）。
func (w *docWriter) table(caption string, header []string, rows [][]string, opt *tableOpts) {
	if len(header) == 0 {
		return
	}
	desired, minimum, landscape := columnWidths(header, rows, w.geom.TextWidth())
	if opt != nil && len(opt.weights) == len(header) {
		desired = opt.weights
	}
	if landscape {
		w.setOrient("landscape")
	} else {
		w.setOrient("portrait")
	}
	if caption != "" {
		w.tbl++
		w.blocks = append(w.blocks, block{pPr: `<w:pStyle w:val="` + w.style("caption") + `"/><w:keepNext/>`,
			content: run(fmt.Sprintf("表 %d %s", w.tbl, caption), runStyle{}), sticky: true})
	}
	width := w.textWidth()
	cols := fitWidths(desired, minimum, width)
	var b strings.Builder
	fmt.Fprintf(&b, `<w:tbl><w:tblPr><w:tblStyle w:val="%s"/><w:tblW w:w="%d" w:type="dxa"/><w:jc w:val="center"/><w:tblInd w:w="0" w:type="dxa"/>`+
		`<w:tblBorders><w:top w:val="single" w:sz="4" w:space="0" w:color="000000"/><w:left w:val="single" w:sz="4" w:space="0" w:color="000000"/>`+
		`<w:bottom w:val="single" w:sz="4" w:space="0" w:color="000000"/><w:right w:val="single" w:sz="4" w:space="0" w:color="000000"/>`+
		`<w:insideH w:val="single" w:sz="4" w:space="0" w:color="000000"/><w:insideV w:val="single" w:sz="4" w:space="0" w:color="000000"/></w:tblBorders>`+
		`<w:tblLayout w:type="fixed"/><w:tblCellMar><w:left w:w="85" w:type="dxa"/><w:right w:w="85" w:type="dxa"/></w:tblCellMar><w:tblLook w:val="04A0" w:firstRow="1" w:lastRow="0" w:firstColumn="0" w:lastColumn="0" w:noHBand="0" w:noVBand="1"/></w:tblPr><w:tblGrid>`,
		w.style("table grid"), width)
	for _, c := range cols {
		fmt.Fprintf(&b, `<w:gridCol w:w="%d"/>`, c)
	}
	b.WriteString(`</w:tblGrid>`)
	cell := func(text string, wd int, head bool) {
		fmt.Fprintf(&b, `<w:tc><w:tcPr><w:tcW w:w="%d" w:type="dxa"/>`, wd)
		if head {
			b.WriteString(`<w:shd w:val="clear" w:color="auto" w:fill="E7EEF7"/>`)
		}
		b.WriteString(`<w:vAlign w:val="center"/></w:tcPr>`)
		jc := ""
		if head {
			jc = `<w:jc w:val="center"/>`
		}
		for _, line := range strings.Split(text, "\n") {
			fmt.Fprintf(&b, `<w:p><w:pPr><w:pStyle w:val="%s"/>%s</w:pPr>%s</w:p>`, w.style("autobid table text"), jc, inline(line, runStyle{bold: head}))
		}
		b.WriteString(`</w:tc>`)
	}
	b.WriteString(`<w:tr><w:trPr><w:tblHeader/><w:cantSplit/></w:trPr>`)
	for i, h := range header {
		cell(h, cols[i], true)
	}
	b.WriteString(`</w:tr>`)
	for _, r := range rows {
		b.WriteString(`<w:tr>`)
		for i := range header {
			v := ""
			if i < len(r) {
				v = r[i]
			}
			cell(v, cols[i], false)
		}
		b.WriteString(`</w:tr>`)
	}
	b.WriteString(`</w:tbl>`)
	w.blocks = append(w.blocks, block{table: true, content: b.String()})
	// 表后留一个小段落：Word 要求表格后有段落，也使相邻表格不会粘连。
	w.para("", `<w:spacing w:before="0" w:after="60" w:line="240" w:lineRule="auto"/>`, "")
}

// ---------------------------------------------------------------------------
// 图表
// ---------------------------------------------------------------------------

const emuPerTwip = 635

func (w *docWriter) addMedia(name string, data []byte) string {
	if w.media == nil {
		w.media = map[string][]byte{}
	}
	id := fmt.Sprintf("rIdAutobid%d", len(w.rels)+1)
	w.media["word/media/"+name] = data
	w.rels = append(w.rels, rel{id: id, typ: "http://schemas.openxmlformats.org/officeDocument/2006/relationships/image", target: "media/" + name})
	return id
}

func (w *docWriter) chart(c db.SnapshotChart, png []byte) {
	switch {
	case c.ChartType == db.ChartEquipmentTable:
		t, err := ParseTableSource(c.Source)
		if err != nil {
			w.warnings = append(w.warnings, db.ExportWarning{Kind: db.WarnChartFailed, ChartID: c.ChartID, SectionID: c.SectionID,
				Message: fmt.Sprintf("设备选型表 #%d《%s》数据无效：%v", c.ChartID, c.Title, err)})
			return
		}
		w.table(c.Title, t.Columns, t.Rows, nil)
	case png != nil:
		cfg, _, err := image.DecodeConfig(bytes.NewReader(png))
		if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
			w.placeholder(c, "图片无法解码")
			return
		}
		aspect := float64(cfg.Width) / float64(cfg.Height)
		if aspect > 2.4 {
			w.setOrient("landscape")
		} else {
			w.setOrient("portrait")
		}
		maxW := float64(w.textWidth() * emuPerTwip)
		maxH := float64(w.textHeight()*emuPerTwip) * 0.72
		natW := float64(cfg.Width) * 9525 // 96 dpi
		cx := min(maxW, natW)
		cy := cx / aspect
		if cy > maxH {
			cy = maxH
			cx = cy * aspect
		}
		id := w.addMedia(fmt.Sprintf("chart%d.png", c.ChartID), png)
		w.nextDocPr++
		n := w.nextDocPr
		drawing := fmt.Sprintf(`<w:r><w:drawing><wp:inline distT="0" distB="0" distL="0" distR="0"><wp:extent cx="%d" cy="%d"/><wp:effectExtent l="0" t="0" r="0" b="0"/>`+
			`<wp:docPr id="%d" name="Chart %d" descr="%s"/><wp:cNvGraphicFramePr><a:graphicFrameLocks noChangeAspect="1"/></wp:cNvGraphicFramePr>`+
			`<a:graphic><a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/picture"><pic:pic><pic:nvPicPr><pic:cNvPr id="%d" name="chart%d.png"/><pic:cNvPicPr/></pic:nvPicPr>`+
			`<pic:blipFill><a:blip r:embed="%s"/><a:stretch><a:fillRect/></a:stretch></pic:blipFill><pic:spPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="%d" cy="%d"/></a:xfrm>`+
			`<a:prstGeom prst="rect"><a:avLst/></a:prstGeom></pic:spPr></pic:pic></a:graphicData></a:graphic></wp:inline></w:drawing></w:r>`,
			int64(cx), int64(cy), n, n, esc(c.Title), n, c.ChartID, id, int64(cx), int64(cy))
		w.blocks = append(w.blocks, block{pPr: `<w:pStyle w:val="` + w.style("autobid figure") + `"/>`, content: drawing})
		w.caption(c.Title)
	default:
		reason := c.RenderError
		if reason == "" {
			reason = "未渲染"
		}
		w.placeholder(c, reason)
	}
}

func (w *docWriter) caption(title string) {
	w.fig++
	w.para(w.style("caption"), "", run(fmt.Sprintf("图 %d %s", w.fig, title), runStyle{}))
}

// placeholder 插入 Mermaid 源码占位框（单格表格），并记录提示。
func (w *docWriter) placeholder(c db.SnapshotChart, reason string) {
	w.setOrient("portrait")
	width := w.textWidth()
	var b strings.Builder
	fmt.Fprintf(&b, `<w:tbl><w:tblPr><w:tblW w:w="%d" w:type="dxa"/><w:jc w:val="center"/><w:tblBorders>`+
		`<w:top w:val="dashed" w:sz="8" w:space="0" w:color="C0504D"/><w:left w:val="dashed" w:sz="8" w:space="0" w:color="C0504D"/>`+
		`<w:bottom w:val="dashed" w:sz="8" w:space="0" w:color="C0504D"/><w:right w:val="dashed" w:sz="8" w:space="0" w:color="C0504D"/></w:tblBorders>`+
		`<w:tblLayout w:type="fixed"/></w:tblPr><w:tblGrid><w:gridCol w:w="%d"/></w:tblGrid><w:tr><w:tc><w:tcPr><w:tcW w:w="%d" w:type="dxa"/>`+
		`<w:shd w:val="clear" w:color="auto" w:fill="F7F7F7"/></w:tcPr>`, width, width, width)
	fmt.Fprintf(&b, `<w:p><w:pPr><w:pStyle w:val="%s"/></w:pPr>%s</w:p>`, w.style("autobid table text"),
		run(fmt.Sprintf("【图表未渲染：%s】%s（Mermaid 源码如下，请渲染后替换）", reason, c.Title), runStyle{bold: true}))
	lines := strings.Split(strings.TrimRight(c.Source, "\n"), "\n")
	if len(lines) > 60 {
		lines = append(lines[:60], fmt.Sprintf("……（其余 %d 行省略）", len(lines)-60))
	}
	for _, l := range lines {
		fmt.Fprintf(&b, `<w:p><w:pPr><w:pStyle w:val="%s"/></w:pPr>%s</w:p>`, w.style("autobid code"), run(l, runStyle{}))
	}
	b.WriteString(`</w:tc></w:tr></w:tbl>`)
	w.blocks = append(w.blocks, block{table: true, content: b.String()})
	w.caption(c.Title + "（源码占位）")
	w.warnings = append(w.warnings, db.ExportWarning{Kind: db.WarnChartUnrendered, ChartID: c.ChartID, SectionID: c.SectionID,
		Message: fmt.Sprintf("图表 #%d《%s》以 Mermaid 源码占位框导出（%s）", c.ChartID, c.Title, reason)})
}

// ---------------------------------------------------------------------------
// 偏离表
// ---------------------------------------------------------------------------

var kindLabels = map[string]string{
	compliance.KindStar: "★ 实质性条款", compliance.KindTriangle: "▲ 重要条款", compliance.KindScoring: "评分项",
	compliance.KindRequired: "必答项", compliance.KindFormat: "格式项",
}

var deviationLabels = map[string]string{
	compliance.DeviationNone: "无偏离", compliance.DeviationPositive: "正偏离", compliance.DeviationNegative: "负偏离",
}

// DeviationHeader 是偏离表表头。
var DeviationHeader = []string{"序号", "招标条款", "条款类型", "招标要求", "应答章节", "应答说明", "偏离情况"}

func (w *docWriter) deviationTable(rows []db.SnapshotMatrixRow) int {
	w.setOrient("portrait")
	w.heading(1, DeviationTitle, true)
	w.para("", "", run(fmt.Sprintf("本表依据导出快照中已核验通过的响应矩阵生成，共 %d 条，与正文锁定修订一致。", len(rows)), runStyle{}))
	body := make([][]string, 0, len(rows))
	for i, r := range rows {
		resp := r.ResponseSummary
		if resp == "" {
			resp = r.AnchorExcerpt
		}
		where := r.SectionCode + " " + r.SectionTitle
		if r.Anchor != "" {
			where += "（" + r.Anchor + "）"
		}
		kind := kindLabels[r.Kind]
		if kind == "" {
			kind = r.Kind
		}
		dev := deviationLabels[r.Deviation]
		if dev == "" {
			dev = r.Deviation
		}
		body = append(body, []string{strconv.Itoa(i + 1), r.Code, kind, r.Title, where, resp, dev})
	}
	w.table("点对点应答及偏离表", DeviationHeader, body, &tableOpts{weights: []float64{4, 7, 8, 22, 14, 30, 7}})
	return len(rows)
}

// ---------------------------------------------------------------------------
// 组装
// ---------------------------------------------------------------------------

// Build 套用模板生成成稿 .docx。
func Build(t *Template, in BuildInput) (*BuildResult, error) {
	styles, stylesXML := t.resolveStyles()
	w := &docWriter{t: t, styles: styles, geom: t.Geometry, orient: "portrait", nextDocPr: 1000}

	// 目录
	tocPPr := ""
	if t.needsBrk {
		tocPPr = `<w:pageBreakBefore/>`
	}
	w.para(w.style("autobid toc title"), tocPPr, run("目  录", runStyle{}))
	w.blocks = append(w.blocks,
		block{pPr: `<w:pStyle w:val="` + w.style("toc 1") + `"/>`, content: `<w:r><w:fldChar w:fldCharType="begin"/></w:r>` +
			`<w:r><w:instrText xml:space="preserve"> TOC \o "1-3" \h \z \u </w:instrText></w:r><w:r><w:fldChar w:fldCharType="separate"/></w:r>` +
			run(TOCPlaceholder, runStyle{})},
		block{content: `<w:r><w:fldChar w:fldCharType="end"/></w:r>`})

	charts := map[int64][]db.SnapshotChart{}
	for _, c := range in.Charts {
		charts[c.SectionID] = append(charts[c.SectionID], c)
	}
	for _, s := range in.Doc.Sections {
		w.setOrient("portrait")
		w.heading(s.Level, strings.TrimSpace(s.Code+" "+s.Title), s.Level == 1)
		body, ok := in.Bodies[s.RevID]
		if !ok {
			return nil, fmt.Errorf("export: revision %d of section %s is missing", s.RevID, s.Code)
		}
		w.markdown(body, s.Title)
		for _, c := range charts[s.SectionID] {
			var png []byte
			if c.RenderStatus == db.ChartRendered && c.RenderFormat == db.ChartFormatPNG {
				png = in.ChartImages[c.ChartID]
				if png == nil {
					w.warnings = append(w.warnings, db.ExportWarning{Kind: db.WarnChartMissing, ChartID: c.ChartID, SectionID: c.SectionID,
						Message: fmt.Sprintf("图表 #%d《%s》的渲染产物缺失或哈希不符，改用源码占位框", c.ChartID, c.Title)})
					c.RenderError = "渲染产物缺失"
				}
			}
			w.chart(c, png)
		}
	}
	rows := w.deviationTable(in.Matrix)
	final := w.t.sectPr
	if w.orient == "landscape" {
		final = w.sectPrFor("landscape")
	}
	landscape := 0
	for _, b := range w.blocks {
		if strings.Contains(b.sectPr, `w:orient="landscape"`) {
			landscape++
		}
	}
	if strings.Contains(final, `w:orient="landscape"`) {
		landscape++
	}

	var body strings.Builder
	afterBreak := !t.needsBrk
	for _, b := range w.blocks {
		if b.table {
			body.WriteString(b.content)
			afterBreak = false
			continue
		}
		ppr := b.pPr + `<w:overflowPunct w:val="0"/>`
		if b.pageBreak && !afterBreak {
			ppr += `<w:pageBreakBefore/>`
		}
		ppr = orderPPr(ppr) + b.sectPr
		body.WriteString("<w:p>")
		if ppr != "" {
			body.WriteString("<w:pPr>" + ppr + "</w:pPr>")
		}
		body.WriteString(b.content + "</w:p>")
		afterBreak = b.sectPr != ""
	}

	repl := strings.NewReplacer("{{title}}", esc(in.Doc.TaskName), "{{task}}", esc(in.Doc.TaskName), "{{bidder}}", esc(in.Bidder),
		"{{date}}", esc(in.Date), "{{outline_version}}", strconv.Itoa(in.Doc.OutlineVersion))
	doc := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n" + rootWithNamespaces(t.docRoot) + "<w:body>" +
		repl.Replace(t.bodyPre) + body.String() + repl.Replace(t.bodyPost) + final + "</w:body></w:document>"

	out := map[string][]byte{}
	for name, data := range t.files {
		out[name] = data
	}
	out["word/document.xml"] = []byte(doc)
	out["word/styles.xml"] = stylesXML
	for name, data := range t.files {
		if strings.HasPrefix(name, "word/header") || strings.HasPrefix(name, "word/footer") {
			out[name] = []byte(repl.Replace(string(data)))
		}
	}
	for name, data := range w.media {
		out[name] = data
	}
	out["word/_rels/document.xml.rels"] = addRels(t.files["word/_rels/document.xml.rels"], w.rels)
	out["[Content_Types].xml"] = ensureContentTypes(t.files["[Content_Types].xml"])
	if s, ok := out["word/settings.xml"]; ok && in.UpdateFieldsOnOpen {
		out["word/settings.xml"] = withUpdateFields(s)
	}
	if core, ok := out["docProps/core.xml"]; ok {
		out["docProps/core.xml"] = []byte(repl.Replace(string(core)))
	}
	data, err := writeZip(t.names, out)
	if err != nil {
		return nil, err
	}
	return &BuildResult{Data: data, Warnings: w.warnings, Headings: w.headings, MatrixRows: rows, Landscape: landscape}, nil
}

// pPrOrder 是 CT_PPr 子元素的 schema 顺序（Word 对乱序的段落属性会报文件损坏）。
var pPrOrder = map[string]int{}

func init() {
	for i, n := range []string{"pStyle", "keepNext", "keepLines", "pageBreakBefore", "framePr", "widowControl", "numPr", "suppressLineNumbers",
		"pBdr", "shd", "tabs", "suppressAutoHyphens", "kinsoku", "wordWrap", "overflowPunct", "topLinePunct", "autoSpaceDE", "autoSpaceDN",
		"bidi", "adjustRightInd", "snapToGrid", "spacing", "ind", "contextualSpacing", "mirrorIndents", "suppressOverlap", "jc",
		"textDirection", "textAlignment", "textboxTightWrap", "outlineLvl", "divId", "cnfStyle", "rPr"} {
		pPrOrder[n] = i
	}
}

var rePPrChild = regexp.MustCompile(`(?s)<w:(\w+)\b[^>]*?/>|<w:(\w+)\b[^>]*>.*?</w:\w+>`)

// orderPPr 把拼接出来的段落属性按 schema 顺序排列，重复的元素保留最后一个。
func orderPPr(s string) string {
	type el struct {
		name, xml string
	}
	var els []el
	idx := map[string]int{}
	for _, m := range rePPrChild.FindAllStringSubmatch(s, -1) {
		name := m[1]
		if name == "" {
			name = m[2]
		}
		if j, ok := idx[name]; ok {
			els[j].xml = m[0]
			continue
		}
		idx[name] = len(els)
		els = append(els, el{name: name, xml: m[0]})
	}
	rank := func(n string) int {
		if o, ok := pPrOrder[n]; ok {
			return o
		}
		return 999
	}
	sort.SliceStable(els, func(a, b int) bool { return rank(els[a].name) < rank(els[b].name) })
	var b strings.Builder
	for _, e := range els {
		b.WriteString(e.xml)
	}
	return b.String()
}

var docNamespaces = []struct{ prefix, uri string }{
	{"w", "http://schemas.openxmlformats.org/wordprocessingml/2006/main"},
	{"r", "http://schemas.openxmlformats.org/officeDocument/2006/relationships"},
	{"wp", "http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing"},
	{"a", "http://schemas.openxmlformats.org/drawingml/2006/main"},
	{"pic", "http://schemas.openxmlformats.org/drawingml/2006/picture"},
}

func rootWithNamespaces(root string) string {
	for _, ns := range docNamespaces {
		if !strings.Contains(root, "xmlns:"+ns.prefix+"=") {
			root = strings.TrimSuffix(root, ">") + ` xmlns:` + ns.prefix + `="` + ns.uri + `">`
		}
	}
	return root
}

func addRels(raw []byte, rels []rel) []byte {
	s := string(raw)
	var add strings.Builder
	for _, r := range rels {
		fmt.Fprintf(&add, `<Relationship Id="%s" Type="%s" Target="%s"/>`, r.id, r.typ, r.target)
	}
	if i := strings.LastIndex(s, "</Relationships>"); i >= 0 {
		s = s[:i] + add.String() + s[i:]
	}
	return []byte(s)
}

func ensureContentTypes(raw []byte) []byte {
	s := string(raw)
	if !strings.Contains(strings.ToLower(s), `extension="png"`) {
		if i := strings.LastIndex(s, "</Types>"); i >= 0 {
			s = s[:i] + `<Default Extension="png" ContentType="image/png"/>` + s[i:]
		}
	}
	return []byte(s)
}

// withUpdateFields 在 settings.xml 中按 CT_Settings 的元素顺序插入 <w:updateFields w:val="true"/>。
func withUpdateFields(raw []byte) []byte {
	s := string(raw)
	if strings.Contains(s, "<w:updateFields") {
		return raw
	}
	for _, tag := range []string{"<w:hdrShapeDefaults", "<w:footnotePr", "<w:endnotePr", "<w:compat", "<w:docVars", "<w:rsids", "<m:mathPr",
		"<w:attachedSchema", "<w:themeFontLang", "<w:clrSchemeMapping", "<w:doNotIncludeSubdocsInStats", "<w:doNotAutoCompressPictures",
		"<w:forceUpgrade", "<w:captions", "<w:readModeInkLockDown", "<w:smartTagType", "<sl:schemaLibrary", "<w:shapeDefaults",
		"<w:doNotEmbedSmartTags", "<w:decimalSymbol", "<w:listSeparator", "</w:settings>"} {
		if i := strings.Index(s, tag); i >= 0 {
			return []byte(s[:i] + `<w:updateFields w:val="true"/>` + s[i:])
		}
	}
	return raw
}

// writeZip 按模板原有顺序（[Content_Types].xml 在最前）写出压缩包，时间戳固定以便同输入得到同内容。
func writeZip(order []string, files map[string][]byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	seen := map[string]bool{}
	names := append([]string{"[Content_Types].xml"}, order...)
	var extra []string
	for name := range files {
		extra = append(extra, name)
	}
	sort.Strings(extra)
	names = append(names, extra...)
	mod := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, name := range names {
		data, ok := files[name]
		if !ok || seen[name] {
			continue
		}
		seen[name] = true
		h := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: mod}
		f, err := zw.CreateHeader(h)
		if err != nil {
			return nil, err
		}
		if _, err := f.Write(data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
