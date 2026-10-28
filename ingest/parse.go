package ingest

// 多格式文档解析：.txt / .md / .docx / .pdf / 图片扫描件统一解析为切分器输入行（Line），
// 再经 SplitLines 归并为带页码范围、结构化表格与版面溯源的段落。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// 文档格式。
const (
	FormatText     = "text"
	FormatMarkdown = "markdown"
	FormatDOCX     = "docx"
	FormatPDF      = "pdf"
	FormatImage    = "image"
)

var (
	// ErrUnsupportedFormat 表示无法识别或不支持的文件格式（如旧版 .doc、加密 PDF）。
	ErrUnsupportedFormat = errors.New("ingest: unsupported document format")
	// ErrNoText 表示文档中没有可提取的文本（扫描件且 OCR 不可用时）。
	ErrNoText = errors.New("ingest: no extractable text")
)

// File 是一份待解析的原始文件。
type File struct {
	Name string
	MIME string
	Data []byte
}

// ParseMeta 是解析过程的元数据，写入 tender_documents.parse_meta / knowledge_documents.parse_meta。
type ParseMeta struct {
	Format   string   `json:"format"`
	Parser   string   `json:"parser"`
	Pages    int      `json:"pages"`
	Lines    int      `json:"lines"`
	Tables   int      `json:"tables"`
	Headings int      `json:"styled_headings,omitempty"`
	Encoding string   `json:"encoding,omitempty"`
	OCR      string   `json:"ocr,omitempty"`
	OCRPages []int    `json:"ocr_pages,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// Parsed 是解析结果：Lines 供切分，Text 为纯文本渲染（页间以 \f 分隔、表格渲染为 Markdown），写入 raw_text。
type Parsed struct {
	Format    string
	MIME      string
	Title     string
	PageCount int
	Lines     []Line
	Tables    []*Table
	Text      string
	Meta      ParseMeta
	// TOC 是 DOCX 目录域（TOC）缓存结果中的条目；TOCFields 是文档中 TOC 域的个数（有域但没有条目表示目录未生成）。
	TOC       []TOCEntry
	TOCFields int
}

// TOCEntry 是一条目录条目：Title 为制表符前的文字，Page 为最后一个制表符后的页码文字。
type TOCEntry struct {
	Level int    `json:"level"`
	Title string `json:"title"`
	Page  string `json:"page"`
	Text  string `json:"text"`
}

// Sections 把解析结果切分为段落。
func (p *Parsed) Sections() []Section { return SplitLines(p.Lines) }

// ParseOptions 配置解析器；OCR 为 nil 时扫描件解析返回 ErrOCRUnavailable。
type ParseOptions struct {
	OCR OCRProvider
}

// DetectFormat 依据扩展名、MIME 与文件头判定格式；无法判定时返回 ""。
func DetectFormat(name, mime string, data []byte) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".txt", ".text":
		return FormatText
	case ".md", ".markdown":
		return FormatMarkdown
	case ".docx":
		return FormatDOCX
	case ".pdf":
		return FormatPDF
	case ".png", ".jpg", ".jpeg", ".tif", ".tiff", ".bmp", ".gif", ".webp":
		return FormatImage
	case ".doc", ".wps", ".rtf":
		return ""
	}
	switch {
	case bytes.HasPrefix(data, []byte("%PDF-")):
		return FormatPDF
	case bytes.HasPrefix(data, []byte("PK\x03\x04")) && bytes.Contains(data, []byte("word/document.xml")):
		return FormatDOCX
	case imageMIME(data) != "":
		return FormatImage
	}
	mime = strings.ToLower(mime)
	switch {
	case strings.HasPrefix(mime, "text/markdown"):
		return FormatMarkdown
	case strings.HasPrefix(mime, "text/"):
		return FormatText
	case strings.Contains(mime, "wordprocessingml"):
		return FormatDOCX
	case mime == "application/pdf":
		return FormatPDF
	case strings.HasPrefix(mime, "image/"):
		return FormatImage
	}
	if len(data) > 0 && !bytes.Contains(data[:min(len(data), 4096)], []byte{0}) {
		return FormatText
	}
	return ""
}

// imageMIME 依据文件头识别常见图片格式。
func imageMIME(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(data, []byte("II*\x00")), bytes.HasPrefix(data, []byte("MM\x00*")):
		return "image/tiff"
	case bytes.HasPrefix(data, []byte("BM")):
		return "image/bmp"
	case bytes.HasPrefix(data, []byte("GIF8")):
		return "image/gif"
	case len(data) > 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

var formatMIME = map[string]string{
	FormatText:     "text/plain",
	FormatMarkdown: "text/markdown",
	FormatDOCX:     "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	FormatPDF:      "application/pdf",
}

// Parse 解析一份文件。
func Parse(ctx context.Context, f File, opt ParseOptions) (*Parsed, error) {
	if len(f.Data) == 0 {
		return nil, fmt.Errorf("%w: empty file", ErrNoText)
	}
	format := DetectFormat(f.Name, f.MIME, f.Data)
	if format == "" {
		return nil, fmt.Errorf("%w: %s（旧版 .doc/.wps 请另存为 .docx 后上传）", ErrUnsupportedFormat, displayName(f))
	}
	var (
		p   *Parsed
		err error
	)
	switch format {
	case FormatText, FormatMarkdown:
		p, err = parseText(f.Data, format)
	case FormatDOCX:
		p, err = parseDOCX(f.Data)
	case FormatPDF:
		p, err = parsePDF(ctx, f.Data, opt.OCR)
	case FormatImage:
		p, err = parseImage(ctx, f.Data, f.MIME, opt.OCR)
	}
	if err != nil {
		return nil, err
	}
	p.Format = format
	if p.MIME == "" {
		p.MIME = formatMIME[format]
	}
	if f.MIME != "" && !strings.HasPrefix(f.MIME, "application/octet-stream") {
		p.MIME = strings.TrimSpace(strings.SplitN(f.MIME, ";", 2)[0])
	}
	p.finish()
	if strings.TrimSpace(p.Text) == "" {
		return nil, fmt.Errorf("%w: %s", ErrNoText, displayName(f))
	}
	return p, nil
}

func displayName(f File) string {
	if f.Name != "" {
		return f.Name
	}
	return "uploaded file"
}

// finish 汇总表格、页数并渲染纯文本。
func (p *Parsed) finish() {
	p.Tables = p.Tables[:0]
	var b strings.Builder
	page := 1
	for _, l := range p.Lines {
		for page < l.Page {
			b.WriteString("\f")
			page++
		}
		b.WriteString(l.Text)
		b.WriteString("\n")
		if l.Table != nil {
			p.Tables = append(p.Tables, l.Table)
		}
		page = max(page, l.lastPage())
		p.PageCount = max(p.PageCount, l.lastPage())
	}
	p.Text = strings.TrimRight(b.String(), "\n")
	p.PageCount = max(p.PageCount, p.Meta.Pages, 1)
	p.Meta.Format, p.Meta.Pages, p.Meta.Lines, p.Meta.Tables = p.Format, p.PageCount, len(p.Lines), len(p.Tables)
	if p.Title == "" {
		for _, l := range p.Lines {
			if l.Table == nil && strings.TrimSpace(l.Text) != "" {
				p.Title = clauseTitle(l.Text)
				break
			}
		}
	}
}

// DecodeText 把文本文件字节解码为 UTF-8：去除 BOM，非 UTF-8 时按 GB18030（兼容 GBK/GB2312）解码。
func DecodeText(data []byte) (string, string) {
	data = bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF"))
	if utf8.Valid(data) {
		return string(data), "utf-8"
	}
	if out, err := simplifiedchinese.GB18030.NewDecoder().Bytes(data); err == nil {
		return string(out), "gb18030"
	}
	return strings.ToValidUTF8(string(data), "�"), "invalid"
}

var reMDHeading = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*#*\s*$`)

// parseText 解析纯文本 / Markdown：页码规则同 Split；Markdown 额外识别 # 标题与管道表格。
func parseText(data []byte, format string) (*Parsed, error) {
	text, enc := DecodeText(data)
	p := &Parsed{Meta: ParseMeta{Parser: "text", Encoding: enc}}
	lines := TextLines(text)
	if format != FormatMarkdown {
		p.Lines = lines
		return p, nil
	}
	p.Meta.Parser = "markdown"
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		t := strings.TrimSpace(l.Text)
		if m := reMDHeading.FindStringSubmatch(t); m != nil {
			l.Text, l.Heading = m[2], len(m[1])
			p.Meta.Headings++
			p.Lines = append(p.Lines, l)
			continue
		}
		if strings.HasPrefix(t, "|") && i+1 < len(lines) && isMDSeparator(lines[i+1].Text) {
			j := i
			var rows []string
			for j < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[j].Text), "|") {
				rows = append(rows, lines[j].Text)
				j++
			}
			if tbl, ok := parseMarkdownTable(rows); ok {
				tbl.PageStart, tbl.PageEnd = l.Page, lines[j-1].Page
				for k := range tbl.Cells {
					tbl.Cells[k].Page = l.Page
				}
				p.Lines = append(p.Lines, Line{Text: tbl.Markdown, Page: l.Page, PageEnd: tbl.PageEnd, Table: tbl, Block: l.Block})
				i = j - 1
				continue
			}
		}
		p.Lines = append(p.Lines, l)
	}
	return p, nil
}

// parseImage 用 OCR 识别单张扫描图片。
func parseImage(ctx context.Context, data []byte, mime string, ocr OCRProvider) (*Parsed, error) {
	if m := imageMIME(data); m != "" {
		mime = m
	}
	if ocr == nil {
		return nil, ErrOCRUnavailable
	}
	page, err := ocr.Recognize(ctx, data, mime)
	if err != nil {
		return nil, err
	}
	p := &Parsed{MIME: mime, Meta: ParseMeta{Parser: "ocr", OCR: ocr.Name(), OCRPages: []int{1}, Pages: 1}}
	p.Lines = ocrLines(page, 1, 0)
	return p, nil
}

// ocrLines 把一页 OCR 结果转为切分器输入行，block 为起始块序号。
func ocrLines(page *OCRPage, pageNo, block int) []Line {
	var out []Line
	if page == nil {
		return nil
	}
	for i, l := range page.Lines {
		text := strings.TrimSpace(l.Text)
		if text == "" || rePageMark.MatchString(text) {
			continue
		}
		box := BBox{Page: pageNo, X0: l.BBox[0], Y0: l.BBox[1], X1: l.BBox[2], Y1: l.BBox[3], Conf: l.Confidence}
		out = append(out, Line{Text: text, Page: pageNo, Boxes: []BBox{box}, Block: block + i})
	}
	return out
}
