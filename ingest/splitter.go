// Package ingest 把招标文件（纯文本/Markdown/DOCX/PDF/扫描件）解析并切分为章节/条款段落，写入 tender_documents /
// document_chunks 并幂等创建根 tender（及可选 lot）静态实体；同一解析器也用于企业方案知识库入库。
package ingest

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Section 是切分结果中的一个段落。Level：0 文首，1 章/篇，2 节或中文序号标题，
// 3+ 为数字编号（"1"=3、"1.1"=4 …），★ 条款取其所在层级 +1。
// PageNo 等于 PageStart；PageStart~PageEnd 为段落（含其表格）跨越的页码范围。
type Section struct {
	Level     int
	Code      string
	Title     string
	PageNo    int
	PageStart int
	PageEnd   int
	Content   string
	Mandatory bool
	// Tables 是段落内还原的结构化表格（其 Markdown 同时写入 Content，便于全文检索）。
	Tables []*Table
	// Boxes 是版面坐标溯源（PDF / OCR 每页一条外接矩形）。
	Boxes []BBox
	// OCRLines 是逐行 OCR 识别框（像素坐标、置信度与图像尺寸，S5a），供对照原件定位；每段最多 maxOCRLineBoxes 条。
	OCRLines []BBox
	// BlockStart~BlockEnd 为来源文档中的块序号范围（文本行号、DOCX 段落/表格序号、PDF/OCR 版面行序号），-1 表示未知。
	BlockStart, BlockEnd int
}

// BBox 是一个页面内的外接矩形（PDF 用户空间坐标，原点左下；OCR 为像素坐标，原点左上）。
type BBox struct {
	Page int     `json:"page"`
	X0   float64 `json:"x0"`
	Y0   float64 `json:"y0"`
	X1   float64 `json:"x1"`
	Y1   float64 `json:"y1"`
	Conf float64 `json:"confidence,omitempty"`
	// Coord 为空表示 PDF 页面坐标（点，原点左下）；"ocr_px" 表示 OCR 输入图像的像素坐标（原点左上），
	// ImgW / ImgH 是该图像尺寸，供对照原件定位（S5a）。
	Coord string  `json:"coord,omitempty"`
	ImgW  float64 `json:"img_w,omitempty"`
	ImgH  float64 `json:"img_h,omitempty"`
}

// CoordOCRPixels 标记 OCR 图像像素坐标。
const CoordOCRPixels = "ocr_px"

// maxOCRLineBoxes 是每段保留的逐行 OCR 识别框上限。
const maxOCRLineBoxes = 400

// Line 是切分器的输入行。纯文本按行生成；DOCX/PDF/OCR 解析器按段落/版面行生成。
type Line struct {
	Text    string
	Page    int // 起始页（1 基）
	PageEnd int // 结束页；0 表示与 Page 相同
	// Heading > 0 表示来源样式给出的标题级别（DOCX 标题样式 / 大纲级别、Markdown #），优先于编号规则定级。
	Heading int
	// Table 非空时本行是一张表格，Text 为其 Markdown。
	Table *Table
	Boxes []BBox // 版面坐标（跨页表格每页一条）
	Block int    // 来源块序号，-1 表示未知
}

func (l Line) lastPage() int { return max(l.Page, l.PageEnd) }

// MaxSectionRunes 是单个段落的正文上限，超长正文按段落边界续切为同编号的多个段落。
const MaxSectionRunes = 2000

// MandatoryPattern 识别强制性条款：行首 ★ / * / ▲ 标记，或废标、否决、无效投标、实质性要求等字样。
var MandatoryPattern = regexp.MustCompile(`(?m)^\s*[★☆*＊▲]|废标|否决|无效投标|投标无效|实质性(?:要求|条款|响应|内容)`)

var (
	reChapter  = regexp.MustCompile(`^(第[一二三四五六七八九十百零〇两\d]+[章篇部])\s*[：:、.．]?\s*(.*)$`)
	reSection  = regexp.MustCompile(`^(第[一二三四五六七八九十百零〇两\d]+节)\s*[：:、.．]?\s*(.*)$`)
	reCNNum    = regexp.MustCompile(`^([一二三四五六七八九十]{1,3})[、.．]\s*(.+)$`)
	reDotted   = regexp.MustCompile(`^(\d{1,3}(?:[.．]\d{1,3}){1,5})[.．]?[、\s]*(.*)$`)
	reSingle   = regexp.MustCompile(`^(\d{1,3})(?:[.．]\s*|\s+)(\S.*)$`)
	reStar     = regexp.MustCompile(`^[★☆*＊▲]\s*(.*)$`)
	rePageMark = regexp.MustCompile(`^(?:[-—–]\s*\d{1,4}\s*[-—–]|第\s*\d{1,4}\s*页(?:\s*[/，,]?\s*共\s*\d{1,4}\s*页)?|(?i:page)\s+\d{1,4}(?:\s+(?i:of)\s+\d{1,4})?)$`)
)

type heading struct {
	level int
	code  string
	title string
	star  bool
	// clause 表示编号条款本身是一句正文（如 "2.1.3 近三年完成过…。"），而不是下设子条款的标题。
	clause bool
}

// shortTitle 判定短标题：不超过 40 个字符且不以句末标点结尾（避免把编号正文句子误作章节）。
func shortTitle(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || utf8.RuneCountInString(s) > 40 {
		return false
	}
	last, _ := utf8.DecodeLastRuneInString(s)
	return !strings.ContainsRune("。；;，,：:", last)
}

// clauseTitle 取条款正文的首个分句作为标题，截断到 40 个字符。
func clauseTitle(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "。；;，,：:"); i > 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > 40 {
		return string(r[:40]) + "…"
	}
	return s
}

// classify 判定一行是否为标题；depth 为最近章节层级，用于 ★ 条款定级。
// afterClause 为 true 时上一个标题是正文条款，无编号 ★ 条款与它同级；否则作为上一标题的子条款。
func classify(line string, depth int, afterClause bool) (heading, bool) {
	if m := reChapter.FindStringSubmatch(line); m != nil && (m[2] == "" || shortTitle(m[2])) {
		return heading{level: 1, code: m[1], title: strings.TrimSpace(m[2])}, true
	}
	if m := reSection.FindStringSubmatch(line); m != nil && (m[2] == "" || shortTitle(m[2])) {
		return heading{level: 2, code: m[1], title: strings.TrimSpace(m[2])}, true
	}
	if m := reCNNum.FindStringSubmatch(line); m != nil && shortTitle(m[2]) {
		return heading{level: 2, code: m[1], title: strings.TrimSpace(m[2])}, true
	}
	if m := reStar.FindStringSubmatch(line); m != nil {
		rest := strings.TrimSpace(m[1])
		h := heading{level: depth + 1, title: clauseTitle(rest), star: true, clause: true}
		if afterClause && depth > 0 {
			h.level = depth
		}
		if d := reDotted.FindStringSubmatch(rest); d != nil && clauseNumber(normalizeCode(d[1]), d[2]) {
			h.code, h.title = normalizeCode(d[1]), clauseTitle(d[2])
			h.level = 3 + strings.Count(h.code, ".")
		}
		return h, true
	}
	// 多级数字编号条款（1.1 / 3.2.1）总是切分，正文句子取首分句作标题。
	if m := reDotted.FindStringSubmatch(line); m != nil {
		code := normalizeCode(m[1])
		if !clauseNumber(code, m[2]) {
			return heading{}, false
		}
		return heading{level: 3 + strings.Count(code, "."), code: code, title: clauseTitle(m[2]), clause: !shortTitle(m[2])}, true
	}
	// 单级数字只在短标题时切分（"1 总则"），"1、xxx" 视为列表项。
	if m := reSingle.FindStringSubmatch(line); m != nil && shortTitle(m[2]) && !startsWithDigitUnit(m[2]) {
		return heading{level: 3, code: m[1], title: strings.TrimSpace(m[2])}, true
	}
	return heading{}, false
}

func normalizeCode(code string) string { return strings.ReplaceAll(code, "．", ".") }

// clauseNumber 排除金额、日期一类的小数（如 "3.50 元"、"2024.06"、"1.5倍"）：
// 各级编号不超过 99 且无前导零，其后不能紧跟数字或计量单位。
func clauseNumber(code, rest string) bool {
	if r, _ := utf8.DecodeRuneInString(rest); unicode.IsDigit(r) || startsWithDigitUnit(rest) {
		return false
	}
	for i, p := range strings.Split(code, ".") {
		n, err := strconv.Atoi(p)
		if err != nil || n > 99 || (len(p) > 1 && p[0] == '0') || (i == 0 && n == 0) {
			return false
		}
	}
	return true
}

// startsWithDigitUnit 排除 "3 个工作日" 这类数量短语被当作单级编号标题。
func startsWithDigitUnit(s string) bool {
	// 只列不会作为标题首字的单位；"项目""年度""人员" 等常见标题开头不在其中。
	for _, unit := range []string{"个", "日内", "天", "元", "万", "亿", "%", "％", "‰", "倍", "米", "吨", "平方米", "公里"} {
		if strings.HasPrefix(s, unit) {
			return true
		}
	}
	return false
}

// Split 按标题层级切分招标文件文本。页码来自换页符（\f）或独占一行的页码标记（"- 12 -"、"第 12 页"），
// 页码标记行本身不进入正文。返回的段落保持文档顺序；文首无标题的正文作为 Level 0 段落。
func Split(text string) []Section {
	return SplitLines(TextLines(text))
}

// TextLines 把纯文本转换为切分器输入行，并按换页符/页码标记计页（规则见 Split）。
func TextLines(text string) []Line {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	var lines []Line
	page := 1
	// 有换页符时以换页符计页、页码标记行只丢弃；否则把页码标记视为页脚，遇到即翻页。
	formFeeds := strings.Contains(text, "\f")
	for i, raw := range strings.Split(text, "\n") {
		for strings.HasPrefix(raw, "\f") {
			page++
			raw = raw[1:]
		}
		pageBreaks := strings.Count(raw, "\f")
		line := strings.TrimRightFunc(strings.ReplaceAll(raw, "\f", ""), unicode.IsSpace)
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && rePageMark.MatchString(trimmed) {
			if !formFeeds {
				page++
			}
			page += pageBreaks
			continue
		}
		lines = append(lines, Line{Text: line, Page: page, Block: i})
		page += pageBreaks
	}
	return lines
}

// SplitLines 是切分核心：按标题层级把输入行归并为段落。表格行不参与标题识别，归入当前段落。
func SplitLines(lines []Line) []Section {
	var (
		out   []Section
		cur   *Section
		body  []Line
		depth = 0
		// afterClause：上一个标题是正文条款（见 heading.clause）
		afterClause bool
		starSeq     = map[string]int{}
		parent      = map[int]string{} // 各层级最近编号，用于给无编号 ★ 条款生成编号
	)
	flush := func() {
		body = trimBlankLines(body)
		if cur == nil {
			if len(body) > 0 {
				s := Section{Level: 0, Title: "文首"}
				s.fill(body)
				s.Mandatory = MandatoryPattern.MatchString(s.Content) || tablesMandatory(s.Tables)
				out = append(out, splitLong(s, body)...)
			}
			body = nil
			return
		}
		cur.fill(body)
		cur.Mandatory = cur.Mandatory || MandatoryPattern.MatchString(cur.Content) || tablesMandatory(cur.Tables)
		out = append(out, splitLong(*cur, body)...)
		cur, body = nil, nil
	}
	for _, ln := range lines {
		if ln.Page <= 0 {
			ln.Page = 1
		}
		if ln.Table != nil {
			body = append(body, ln)
			continue
		}
		trimmed := strings.TrimSpace(ln.Text)
		var (
			h  heading
			ok bool
		)
		if ln.Heading > 0 && trimmed != "" {
			h, ok = styledHeading(trimmed, ln.Heading, depth, afterClause), true
		} else {
			h, ok = classify(trimmed, depth, afterClause)
		}
		if !ok {
			body = append(body, ln)
			continue
		}
		flush()
		if h.star && h.code == "" {
			base := nearestCode(parent, h.level)
			starSeq[base]++
			h.code = fmt.Sprintf("%s★%d", base, starSeq[base])
		}
		// ★ 条款不改变章节深度，连续的 ★ 条款保持同级。
		if !h.star {
			depth = h.level
		}
		afterClause = h.clause
		for l := range parent {
			if l >= h.level {
				delete(parent, l)
			}
		}
		parent[h.level] = h.code
		cur = &Section{Level: h.level, Code: h.code, Title: h.title, Mandatory: h.star}
		ln.Text = trimmed
		body = append(body, ln)
	}
	flush()
	return out
}

// styledHeading 处理来源样式明确标注的标题：级别取样式级别，编号与标题尽量沿用编号规则的解析结果。
func styledHeading(text string, level, depth int, afterClause bool) heading {
	level = min(max(level, 1), 9)
	if h, ok := classify(text, depth, afterClause); ok && !h.star {
		h.level, h.clause = level, false
		return h
	}
	title := strings.TrimLeft(text, "★☆▲*＊ ")
	if !shortTitle(title) {
		title = clauseTitle(title)
	}
	return heading{level: level, title: title, star: reStar.MatchString(text)}
}

func tablesMandatory(ts []*Table) bool {
	for _, t := range ts {
		if t.Mandatory {
			return true
		}
	}
	return false
}

func trimBlankLines(body []Line) []Line {
	for len(body) > 0 && body[0].Table == nil && strings.TrimSpace(body[0].Text) == "" {
		body = body[1:]
	}
	for len(body) > 0 && body[len(body)-1].Table == nil && strings.TrimSpace(body[len(body)-1].Text) == "" {
		body = body[:len(body)-1]
	}
	return body
}

// fill 由正文行计算段落正文、页码范围、表格、坐标与来源块范围。
func (s *Section) fill(body []Line) {
	texts := make([]string, 0, len(body))
	s.PageStart, s.PageEnd, s.BlockStart, s.BlockEnd = 0, 0, -1, -1
	s.Tables, s.Boxes, s.OCRLines = nil, nil, nil
	boxes := map[int]*BBox{}
	var pages []int
	for _, l := range body {
		texts = append(texts, l.Text)
		if s.PageStart == 0 || l.Page < s.PageStart {
			s.PageStart = l.Page
		}
		s.PageEnd = max(s.PageEnd, l.lastPage())
		if l.Table != nil {
			s.Tables = append(s.Tables, l.Table)
			s.PageEnd = max(s.PageEnd, l.Table.PageEnd)
		}
		if l.Block >= 0 {
			if s.BlockStart < 0 {
				s.BlockStart = l.Block
			}
			s.BlockEnd = l.Block
		}
		for _, lb := range l.Boxes {
			if lb.Coord == CoordOCRPixels && len(s.OCRLines) < maxOCRLineBoxes {
				s.OCRLines = append(s.OCRLines, lb)
			}
			b, ok := boxes[lb.Page]
			if !ok {
				cp := lb
				boxes[lb.Page] = &cp
				pages = append(pages, lb.Page)
				continue
			}
			b.X0, b.Y0 = min(b.X0, lb.X0), min(b.Y0, lb.Y0)
			b.X1, b.Y1 = max(b.X1, lb.X1), max(b.Y1, lb.Y1)
			if lb.Conf > 0 && (b.Conf == 0 || lb.Conf < b.Conf) {
				b.Conf = lb.Conf // 取最低置信度，提示人工复核
			}
		}
	}
	for _, p := range pages {
		s.Boxes = append(s.Boxes, *boxes[p])
	}
	s.PageStart = max(s.PageStart, 1)
	s.PageEnd = max(s.PageEnd, s.PageStart)
	s.PageNo = s.PageStart
	s.Content = strings.TrimSpace(strings.Join(texts, "\n"))
}

func nearestCode(parent map[int]string, level int) string {
	for l := level - 1; l >= 0; l-- {
		if c, ok := parent[l]; ok {
			return c
		}
	}
	return ""
}

// splitLong 把超长段落按空行/行边界续切，后续部分标题加“（续n）”。表格行不拆分，整体归入一个部分。
func splitLong(s Section, body []Line) []Section {
	if utf8.RuneCountInString(s.Content) <= MaxSectionRunes {
		return []Section{s}
	}
	var (
		parts []Section
		buf   []Line
		size  int
	)
	emit := func() {
		buf = trimBlankLines(buf)
		if len(buf) == 0 {
			return
		}
		p := s
		p.fill(buf)
		if len(parts) > 0 {
			p.Title = fmt.Sprintf("%s（续%d）", s.Title, len(parts))
		}
		p.Mandatory = s.Mandatory && (len(parts) == 0 || MandatoryPattern.MatchString(p.Content) || tablesMandatory(p.Tables))
		parts = append(parts, p)
		buf, size = nil, 0
	}
	for _, line := range body {
		if line.Table != nil {
			n := utf8.RuneCountInString(line.Text) + 1
			if size > 0 && size+n > MaxSectionRunes {
				emit()
			}
			buf = append(buf, line)
			size += n
			continue
		}
		for _, text := range strings.Split(line.Text, "\n") {
			for _, piece := range hardWrap(text, MaxSectionRunes) {
				n := utf8.RuneCountInString(piece) + 1
				if size+n > MaxSectionRunes {
					emit()
				}
				l := line
				l.Text = piece
				buf = append(buf, l)
				size += n
			}
		}
	}
	emit()
	return parts
}

func hardWrap(line string, max int) []string {
	r := []rune(line)
	if len(r) <= max {
		return []string{line}
	}
	var out []string
	for len(r) > max {
		out = append(out, string(r[:max]))
		r = r[max:]
	}
	return append(out, string(r))
}

// EstimateTokens 粗估 token 数：CJK 字符约 1 token/字，其余约 4 字符/token。
func EstimateTokens(s string) int {
	var cjk, other int
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Han, r) || unicode.In(r, unicode.Hiragana, unicode.Katakana, unicode.Hangul):
			cjk++
		case unicode.IsSpace(r):
		default:
			other++
		}
	}
	return cjk + (other+3)/4
}
