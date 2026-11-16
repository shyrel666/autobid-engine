package export

// 成稿自检（用 ingest 的解析器回读产物）：
//   - PDF：从目录页读出每个条目的页码，在正文页中定位同名标题实际所在的页，二者必须一致；页脚「第 N 页」与物理页序一致；
//     版芯外没有正文文字与图片（页眉 / 页脚带的文字只允许在页眉页脚区域内、左右不越出版芯），图片互不重叠、不压正文；
//     没有只含页眉页脚的空白页；
//   - DOCX：TOC 域存在，缓存结果非空且不是「请更新域」类占位文字，条目带页码；读出标题、表格与正文图片数；
//   - 页码引用（D4）：评分索引表、偏离表与附录中指向章节标题书签的 PAGEREF 域，缓存结果必须是数字，并且等于该标题实际
//     所在的页（以同一次刷新导出的 PDF 版面为准；没有 PDF 时与目录中同名条目的页码比对）。
// Runner 在导出成功后执行自检并把问题记为提示；严格最终稿另把目录结构问题与图片 / 表格缺失判为失败（exec.go readback）。
// 验收测试直接断言报告为空。

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/autobid/autobid-engine/ingest"
)

// Issue 是一条版式 / 目录问题。
type Issue struct {
	Page   int    `json:"page,omitempty"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// 问题类型。
const (
	IssueTOCMissing      = "toc_missing"
	IssueTOCPage         = "toc_page_mismatch"
	IssueHeadingMissing  = "heading_missing"
	IssueFooterPage      = "footer_page_mismatch"
	IssueOutOfBounds     = "out_of_bounds"
	IssueImageOverlap    = "image_overlap"
	IssueImageOverText   = "image_over_text"
	IssueBlankPage       = "blank_page"
	IssueTOCPlaceholder  = "toc_placeholder"
	IssueTOCEntryNoPage  = "toc_entry_without_page"
	IssueTOCFieldMissing = "toc_field_missing"

	// D4 页码引用：域或书签缺失、缓存结果不是页码、页码与标题实际所在页不一致。
	IssuePageRefMissing    = "pageref_missing"
	IssuePageRefUnresolved = "pageref_unresolved"
	IssuePageRefPage       = "pageref_page_mismatch"
)

// TOCCheck 是一个目录条目的核对结果。
type TOCCheck struct {
	Title  string `json:"title"`
	Listed int    `json:"listed_page"`
	Actual int    `json:"actual_page"`
}

// PDFReport 是 PDF 自检报告。
type PDFReport struct {
	Pages     int                    `json:"pages"`
	TOCPages  []int                  `json:"toc_pages"`
	TOC       []TOCCheck             `json:"toc"`
	Landscape []int                  `json:"landscape_pages"`
	Images    int                    `json:"images"`
	Issues    []Issue                `json:"issues"`
	Layout    []ingest.PDFPageLayout `json:"-"`

	titles map[string]titlePos // 目录页之后的正文行（去空白）-> 字号最大的一处所在页
}

type titlePos struct {
	page int
	size float64
}

// HeadingPage 返回标题文字实际所在的页（目录页之后；同一文字出现多处时取字号最大的一处，即标题而不是表格里的引用），
// 找不到时为 0。
func (r *PDFReport) HeadingPage(title string) int {
	return r.titles[normText(title)].page
}

var (
	reTOCLine   = regexp.MustCompile(`^(.+?)\s*[.．·…‥]{3,}\s*(\d{1,4})$`)
	reFooterNum = regexp.MustCompile(`第\s*(\d+)\s*页`)
	reSpaces    = regexp.MustCompile(`[\s\x{3000}]+`)
)

func normText(s string) string { return reSpaces.ReplaceAllString(s, "") }

type area struct{ x0, y0, x1, y1 float64 }

func (a area) contains(b ingest.BBox, tol float64) bool {
	return b.X0 >= a.x0-tol && b.X1 <= a.x1+tol && b.Y0 >= a.y0-tol && b.Y1 <= a.y1+tol
}

// closingPunct 是行末可压缩为半宽的全角收尾标点：WPS / Word 排版时它们的字形前进框会越过右边距最多半个字宽，
// 墨迹仍在版芯内，不算越界。
const closingPunct = "，。、；：？！）》」』】〕”’"

// containsLine 判断文字行是否在区域内；行末为全角收尾标点时，右侧多容许半个字宽。
func (a area) containsLine(l ingest.PDFLine, tol float64) bool {
	right := tol
	if r, _ := utf8.DecodeLastRuneInString(strings.TrimSpace(l.Text)); strings.ContainsRune(closingPunct, r) {
		right += l.Size / 2
	}
	b := l.Box
	return b.X0 >= a.x0-tol && b.X1 <= a.x1+right && b.Y0 >= a.y0-tol && b.Y1 <= a.y1+tol
}

func overlap(a, b ingest.BBox) (w, h float64) {
	return math.Min(a.X1, b.X1) - math.Max(a.X0, b.X0), math.Min(a.Y1, b.Y1) - math.Max(a.Y0, b.Y0)
}

// textArea 返回页面版芯（PDF 坐标，单位 pt）。横向页按页面宽高判断。
func textArea(p ingest.PDFPageLayout, g Geometry) area {
	if p.Width() > p.Height() {
		g = g.Landscape()
	}
	pt := func(tw int) float64 { return float64(tw) / 20 }
	return area{x0: p.Box[0] + pt(g.Left), y0: p.Box[1] + pt(g.Bottom), x1: p.Box[2] - pt(g.Right), y1: p.Box[3] - pt(g.Top)}
}

// CheckPDF 回读 PDF 并核对目录页码与版式。
func CheckPDF(data []byte, g Geometry) (*PDFReport, error) {
	pages, err := ingest.ReadPDFLayout(data)
	if err != nil {
		return nil, err
	}
	return checkLayout(pages, g), nil
}

func checkLayout(pages []ingest.PDFPageLayout, g Geometry) *PDFReport {
	r := &PDFReport{Pages: len(pages), TOC: []TOCCheck{}, TOCPages: []int{}, Landscape: []int{}, Issues: []Issue{}, Layout: pages}
	const tol = 1.5
	body := make([][]ingest.PDFLine, len(pages))
	for i, p := range pages {
		a := textArea(p, g)
		if p.Width() > p.Height() {
			r.Landscape = append(r.Landscape, p.Number)
		}
		for _, l := range p.Lines {
			cy := (l.Box.Y0 + l.Box.Y1) / 2
			switch {
			case cy > a.y1 || cy < a.y0: // 页眉 / 页脚带
				band := area{x0: a.x0, x1: a.x1, y0: p.Box[1], y1: a.y0}
				if cy > a.y1 {
					band = area{x0: a.x0, x1: a.x1, y0: a.y1, y1: p.Box[3]}
				}
				if !band.containsLine(l, tol) {
					r.Issues = append(r.Issues, Issue{Page: p.Number, Kind: IssueOutOfBounds,
						Detail: fmt.Sprintf("页眉页脚文字越界 %q [%.1f,%.1f,%.1f,%.1f]", l.Text, l.Box.X0, l.Box.Y0, l.Box.X1, l.Box.Y1)})
				}
				if m := reFooterNum.FindStringSubmatch(l.Text); m != nil && cy < a.y0 {
					if n, _ := strconv.Atoi(m[1]); n != p.Number {
						r.Issues = append(r.Issues, Issue{Page: p.Number, Kind: IssueFooterPage, Detail: fmt.Sprintf("页脚显示第 %d 页，实际为第 %d 页", n, p.Number)})
					}
				}
			default:
				if !a.containsLine(l, tol) {
					r.Issues = append(r.Issues, Issue{Page: p.Number, Kind: IssueOutOfBounds,
						Detail: fmt.Sprintf("正文文字越出版芯 %q [%.1f,%.1f,%.1f,%.1f]，版芯 [%.1f,%.1f,%.1f,%.1f]", l.Text, l.Box.X0, l.Box.Y0, l.Box.X1, l.Box.Y1, a.x0, a.y0, a.x1, a.y1)})
				}
				body[i] = append(body[i], l)
			}
		}
		for k, im := range p.Images {
			r.Images++
			if !a.contains(im, 1) {
				r.Issues = append(r.Issues, Issue{Page: p.Number, Kind: IssueOutOfBounds,
					Detail: fmt.Sprintf("图片越出版芯 [%.1f,%.1f,%.1f,%.1f]", im.X0, im.Y0, im.X1, im.Y1)})
			}
			for _, other := range p.Images[k+1:] {
				if w, h := overlap(im, other); w > 1 && h > 1 {
					r.Issues = append(r.Issues, Issue{Page: p.Number, Kind: IssueImageOverlap, Detail: fmt.Sprintf("图片重叠 %.1f×%.1fpt", w, h)})
				}
			}
			for _, l := range body[i] {
				if w, h := overlap(im, l.Box); w > 1 && h > 2 {
					r.Issues = append(r.Issues, Issue{Page: p.Number, Kind: IssueImageOverText, Detail: fmt.Sprintf("图片压住文字 %q", l.Text)})
				}
			}
		}
		if len(body[i]) == 0 && len(p.Images) == 0 {
			r.Issues = append(r.Issues, Issue{Page: p.Number, Kind: IssueBlankPage, Detail: "页面只有页眉页脚或完全空白"})
		}
	}

	// 目录页：从「目录」标题所在页开始，连续含目录条目的页。
	start := -1
	for i := range pages {
		for _, l := range body[i] {
			if normText(l.Text) == "目录" {
				start = i
				break
			}
		}
		if start >= 0 {
			break
		}
	}
	if start < 0 {
		r.Issues = append(r.Issues, Issue{Kind: IssueTOCMissing, Detail: "PDF 中没有找到「目录」页"})
		return r
	}
	type entry struct {
		title string
		page  int
	}
	var entries []entry
	last := start
	for i := start; i < len(pages); i++ {
		found, prefix, afterTitle := 0, "", false
		for _, l := range body[i] {
			t := strings.TrimSpace(l.Text)
			if normText(t) == "目录" {
				afterTitle = true
				continue
			}
			if m := reTOCLine.FindStringSubmatch(t); m != nil {
				n, _ := strconv.Atoi(m[2])
				entries = append(entries, entry{title: normText(prefix + m[1]), page: n})
				prefix = ""
				found++
			} else if i > start || found > 0 || afterTitle {
				// 换行的长条目。目录起始页上「目录」标题之前的文字不算（模板里目录可以接在其他内容后面）；
				// 标题之后的第一条就可能折行，所以不能等到出现过条目才开始拼接。
				prefix += t
			}
		}
		if found == 0 && i > start {
			break
		}
		last = i
		r.TOCPages = append(r.TOCPages, pages[i].Number)
	}
	if len(entries) == 0 {
		r.Issues = append(r.Issues, Issue{Page: start + 1, Kind: IssueTOCMissing, Detail: "目录页没有带页码的条目（目录未刷新）"})
		return r
	}
	// 评分索引表、偏离表等表格的单元格里也会出现章节标题文字（字号小于标题）：同一文字取字号最大的一处。
	r.titles = map[string]titlePos{}
	add := func(text string, page int, size float64) {
		k := normText(text)
		if cur, ok := r.titles[k]; k != "" && (!ok || size > cur.size+0.25) {
			r.titles[k] = titlePos{page: page, size: size}
		}
	}
	for i := last + 1; i < len(pages); i++ {
		for j, l := range body[i] {
			add(l.Text, pages[i].Number, l.Size)
			// 一行排不下的长标题在版面里是相邻的两三行：同字号的相邻行拼起来也登记，否则长标题会被判成“找不到标题”，
			// 严格最终稿因此失败。页码取首行所在页。
			text := l.Text
			for k := j + 1; k < len(body[i]) && k <= j+2 && math.Abs(body[i][k].Size-l.Size) <= 0.25; k++ {
				text += body[i][k].Text
				add(text, pages[i].Number, l.Size)
			}
		}
	}
	for _, e := range entries {
		actual := r.titles[e.title].page
		r.TOC = append(r.TOC, TOCCheck{Title: e.title, Listed: e.page, Actual: actual})
		switch {
		case actual == 0:
			r.Issues = append(r.Issues, Issue{Kind: IssueHeadingMissing, Detail: fmt.Sprintf("目录条目 %q 在正文中找不到同名标题", e.title)})
		case actual != e.page:
			r.Issues = append(r.Issues, Issue{Page: actual, Kind: IssueTOCPage, Detail: fmt.Sprintf("目录条目 %q 标注第 %d 页，标题实际在第 %d 页", e.title, e.page, actual)})
		}
	}
	return r
}

// DOCXReport 是 DOCX 回读报告。
type DOCXReport struct {
	Headings  []string          `json:"headings"`
	TOC       []ingest.TOCEntry `json:"toc"`
	TOCFields int               `json:"toc_fields"`
	Tables    []*ingest.Table   `json:"-"`
	Issues    []Issue           `json:"issues"`
}

var tocPlaceholders = []string{"请更新域", "更新域", "更新目录", "未找到目录项", "No table of contents entries found", "Update Field", TOCPlaceholder}

// CheckDOCX 回读 DOCX：标题、表格与 TOC 域缓存结果。
func CheckDOCX(ctx context.Context, data []byte) (*DOCXReport, error) {
	p, err := ingest.Parse(ctx, ingest.File{Name: "export.docx", Data: data}, ingest.ParseOptions{})
	if err != nil {
		return nil, err
	}
	r := &DOCXReport{TOC: p.TOC, TOCFields: p.TOCFields, Issues: []Issue{}}
	for _, l := range p.Lines {
		if l.Heading > 0 {
			r.Headings = append(r.Headings, strings.TrimSpace(l.Text))
		}
		if l.Table != nil {
			r.Tables = append(r.Tables, l.Table)
		}
	}
	if p.TOCFields == 0 {
		r.Issues = append(r.Issues, Issue{Kind: IssueTOCFieldMissing, Detail: "文档中没有 TOC 域"})
	}
	if len(p.TOC) == 0 {
		r.Issues = append(r.Issues, Issue{Kind: IssueTOCPlaceholder, Detail: "TOC 域缓存结果为空"})
	}
	for _, e := range p.TOC {
		for _, ph := range tocPlaceholders {
			if strings.Contains(e.Text, ph) {
				r.Issues = append(r.Issues, Issue{Kind: IssueTOCPlaceholder, Detail: fmt.Sprintf("TOC 缓存结果是占位文字 %q", e.Text)})
				break
			}
		}
		if _, err := strconv.Atoi(e.Page); err != nil {
			r.Issues = append(r.Issues, Issue{Kind: IssueTOCEntryNoPage, Detail: fmt.Sprintf("目录条目 %q 没有页码", e.Text)})
		}
	}
	return r, nil
}

// reBlip 匹配图片引用元素（文档引擎另存时可能改写命名空间前缀）。
var reBlip = regexp.MustCompile(`<(?:\w+:)?blip[\s/>]`)

// DOCXBodyImages 统计正文（word/document.xml）中嵌入的图片数（a:blip 引用），页眉页脚中的图片不计入。
func DOCXBodyImages(data []byte) (int, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return 0, err
	}
	for _, f := range zr.File {
		if f.Name != "word/document.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return 0, err
		}
		body, err := io.ReadAll(io.LimitReader(rc, 256<<20))
		rc.Close()
		if err != nil {
			return 0, err
		}
		return len(reBlip.FindAllIndex(body, -1)), nil
	}
	return 0, fmt.Errorf("word/document.xml not found")
}

// ---------------------------------------------------------------------------
// D4：页码引用（PAGEREF）回读
// ---------------------------------------------------------------------------

// PageRef 是成稿中一个指向章节书签的页码引用域。
type PageRef struct {
	Bookmark string `json:"bookmark"`
	Result   string `json:"result"` // 域的缓存结果（刷新后应为页码）
}

// PageRefCheck 是一个页码引用的核对结果；Actual 为 0 表示没有可比对的实际页。
type PageRefCheck struct {
	Bookmark string `json:"bookmark"`
	Title    string `json:"title"`
	Listed   int    `json:"listed_page"`
	Actual   int    `json:"actual_page"`
}

var rePageRefInstr = regexp.MustCompile(`PAGEREF\s+(_Ref\d+)`)

// ReadPageRefs 从 word/document.xml 读出指向章节书签的 PAGEREF 域及其缓存结果，以及这些书签所在段落的文字。
func ReadPageRefs(data []byte) (refs []PageRef, bookmarks map[string]string, err error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, nil, err
	}
	var doc []byte
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			if doc, err = readZipFile(f); err != nil {
				return nil, nil, err
			}
		}
	}
	if doc == nil {
		return nil, nil, fmt.Errorf("word/document.xml not found")
	}
	type field struct {
		instr, result strings.Builder
		inResult      bool
	}
	bookmarks = map[string]string{}
	var (
		fields    []*field
		paraText  strings.Builder
		paraMarks []string
		inInstr   bool
		inText    bool
	)
	closeField := func() {
		if len(fields) == 0 {
			return
		}
		f := fields[len(fields)-1]
		fields = fields[:len(fields)-1]
		if m := rePageRefInstr.FindStringSubmatch(f.instr.String()); m != nil {
			refs = append(refs, PageRef{Bookmark: m[1], Result: strings.TrimSpace(f.result.String())})
		}
	}
	attr := func(se xml.StartElement, name string) string {
		for _, a := range se.Attr {
			if a.Name.Local == name {
				return a.Value
			}
		}
		return ""
	}
	dec := xml.NewDecoder(bytes.NewReader(doc))
	for {
		tok, terr := dec.Token()
		if terr == io.EOF {
			break
		}
		if terr != nil {
			return nil, nil, terr
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "p":
				paraText.Reset()
				paraMarks = paraMarks[:0]
			case "bookmarkStart":
				if n := attr(t, "name"); strings.HasPrefix(n, "_Ref") {
					paraMarks = append(paraMarks, n)
				}
			case "fldChar":
				switch attr(t, "fldCharType") {
				case "begin":
					fields = append(fields, &field{})
				case "separate":
					if len(fields) > 0 {
						fields[len(fields)-1].inResult = true
					}
				case "end":
					closeField()
				}
			case "fldSimple":
				f := &field{inResult: true}
				f.instr.WriteString(attr(t, "instr"))
				fields = append(fields, f)
			case "instrText":
				inInstr = true
			case "t":
				inText = true
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "p":
				for _, n := range paraMarks {
					bookmarks[n] = strings.TrimSpace(paraText.String())
				}
			case "fldSimple":
				closeField()
			case "instrText":
				inInstr = false
			case "t":
				inText = false
			}
		case xml.CharData:
			switch {
			case inInstr && len(fields) > 0:
				fields[len(fields)-1].instr.Write(t)
			case inText:
				if len(fields) > 0 && fields[len(fields)-1].inResult {
					fields[len(fields)-1].result.Write(t)
				}
				paraText.Write(t)
			}
		}
	}
	return refs, bookmarks, nil
}

// CheckPageRefs 核对成稿 DOCX 中的章节页码引用。want 是生成时写入的书签（书签名 -> 标题文字），wantRefs 是写入的引用个数；
// pdf 非 nil 时以其版面中标题实际所在页为准，否则与 toc（DOCX 目录域缓存条目）中同名标题的页码比对。
func CheckPageRefs(docx []byte, want map[string]string, wantRefs int, pdf *PDFReport, toc []ingest.TOCEntry) ([]PageRefCheck, []Issue, error) {
	refs, marks, err := ReadPageRefs(docx)
	if err != nil {
		return nil, nil, err
	}
	issues := []Issue{}
	if len(refs) < wantRefs {
		issues = append(issues, Issue{Kind: IssuePageRefMissing, Detail: fmt.Sprintf("成稿中有 %d 个章节页码引用，生成时写入了 %d 个", len(refs), wantRefs)})
	}
	tocPage := map[string]int{}
	for _, e := range toc {
		if n, err := strconv.Atoi(strings.TrimSpace(e.Page)); err == nil {
			if _, dup := tocPage[normText(e.Text)]; !dup {
				tocPage[normText(e.Text)] = n
			}
		}
	}
	checks := make([]PageRefCheck, 0, len(refs))
	reported := map[string]bool{}
	report := func(key string, is Issue) {
		if !reported[key] {
			reported[key] = true
			issues = append(issues, is)
		}
	}
	for _, ref := range refs {
		title, ok := want[ref.Bookmark]
		if !ok {
			continue // 不是本次生成写入的引用（模板自带）
		}
		c := PageRefCheck{Bookmark: ref.Bookmark, Title: title}
		if _, ok := marks[ref.Bookmark]; !ok {
			report("m/"+ref.Bookmark, Issue{Kind: IssuePageRefMissing, Detail: fmt.Sprintf("章节 %q 的书签在成稿中丢失，页码引用无法解析", title)})
			checks = append(checks, c)
			continue
		}
		n, err := strconv.Atoi(ref.Result)
		if err != nil || n <= 0 {
			report("u/"+ref.Bookmark, Issue{Kind: IssuePageRefUnresolved, Detail: fmt.Sprintf("章节 %q 的页码引用未刷新（缓存结果 %q）", title, truncateRunes(ref.Result, 40))})
			checks = append(checks, c)
			continue
		}
		c.Listed = n
		if pdf != nil {
			c.Actual = pdf.HeadingPage(title)
			if c.Actual == 0 {
				report("h/"+ref.Bookmark, Issue{Kind: IssueHeadingMissing, Detail: fmt.Sprintf("页码引用指向的标题 %q 在 PDF 版面中找不到", title)})
			}
		} else {
			c.Actual = tocPage[normText(title)]
		}
		if c.Actual > 0 && c.Actual != c.Listed {
			report(fmt.Sprintf("p/%s/%d", ref.Bookmark, c.Listed), Issue{Page: c.Actual, Kind: IssuePageRefPage,
				Detail: fmt.Sprintf("页码引用标注章节 %q 在第 %d 页，标题实际在第 %d 页", title, c.Listed, c.Actual)})
		}
		checks = append(checks, c)
	}
	return checks, issues, nil
}
