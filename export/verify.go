package export

// 成稿自检（用 ingest 的解析器回读产物）：
//   - PDF：从目录页读出每个条目的页码，在正文页中定位同名标题实际所在的页，二者必须一致；页脚「第 N 页」与物理页序一致；
//     版芯外没有正文文字与图片（页眉 / 页脚带的文字只允许在页眉页脚区域内、左右不越出版芯），图片互不重叠、不压正文；
//     没有只含页眉页脚的空白页；
//   - DOCX：TOC 域存在，缓存结果非空且不是「请更新域」类占位文字，条目带页码；读出标题、表格与正文图片数。
// Runner 在导出成功后执行自检并把问题记为提示；严格最终稿另把目录结构问题与图片 / 表格缺失判为失败（exec.go readback）。
// 验收测试直接断言报告为空。

import (
	"archive/zip"
	"bytes"
	"context"
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
		found, prefix := 0, ""
		for _, l := range body[i] {
			t := strings.TrimSpace(l.Text)
			if normText(t) == "目录" {
				continue
			}
			if m := reTOCLine.FindStringSubmatch(t); m != nil {
				n, _ := strconv.Atoi(m[2])
				entries = append(entries, entry{title: normText(prefix + m[1]), page: n})
				prefix = ""
				found++
			} else if i > start || found > 0 {
				prefix += t // 换行的长条目
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
	for _, e := range entries {
		actual := 0
		for i := last + 1; i < len(pages) && actual == 0; i++ {
			for _, l := range body[i] {
				if normText(l.Text) == e.title {
					actual = pages[i].Number
					break
				}
			}
		}
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
