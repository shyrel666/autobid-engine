package ingest

// DOCX 解析（WordprocessingML，标准库 archive/zip + encoding/xml）：
//   - 标题级别：段落 outlineLvl → 段落样式（沿 basedOn 链）的 outlineLvl → 样式名 "heading N" / "标题 N"；
//   - 页码：文档含 w:lastRenderedPageBreak（Word 保存时记录的实际分页位置）时只按它计页，
//     否则按显式分页符 w:br type=page、pageBreakBefore 与分节符计页；
//   - 表格：w:gridSpan 横向合并、w:vMerge 纵向合并、w:tblHeader 重复表头行、嵌套表格展平为单元格文本，
//     行内分页记录为表格跨页范围。

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

type docxStyle struct {
	name    string
	basedOn string
	outline int // -1 表示未设置
}

type docxParser struct {
	styles      map[string]docxStyle
	useRendered bool
	page        int
	block       int
	lines       []Line
	headings    int
	title       string
	warnings    []string
	fields      []docxField // 复杂域栈（fldChar begin / separate / end，可嵌套、可跨段落）
	inInstr     bool
	toc         []TOCEntry
	tocFields   int
}

type docxField struct {
	instr  string
	result bool // 已过 separate，后续文字是域的缓存结果
}

// inTOCResult 报告当前位置是否位于 TOC 域的缓存结果中。
func (p *docxParser) inTOCResult() bool {
	for _, f := range p.fields {
		if f.result && strings.HasPrefix(strings.ToUpper(strings.TrimSpace(f.instr)), "TOC") {
			return true
		}
	}
	return false
}

func attr(se xml.StartElement, local string) (string, bool) {
	for _, a := range se.Attr {
		if a.Name.Local == local {
			return a.Value, true
		}
	}
	return "", false
}

// onOff 解析 WordprocessingML 的开关属性：缺省 val 视为开启。
func onOff(se xml.StartElement) bool {
	v, ok := attr(se, "val")
	return !ok || (v != "0" && v != "false" && v != "off")
}

func readZipFile(zr *zip.Reader, name string) ([]byte, error) {
	for _, f := range zr.File {
		if strings.EqualFold(f.Name, name) {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(io.LimitReader(rc, 256<<20))
		}
	}
	return nil, nil
}

func parseDOCX(data []byte) (*Parsed, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("%w: invalid docx archive: %v", ErrUnsupportedFormat, err)
	}
	doc, err := readZipFile(zr, "word/document.xml")
	if err != nil || doc == nil {
		return nil, fmt.Errorf("%w: docx_missing_document_xml: docx has no word/document.xml", ErrUnsupportedFormat)
	}
	p := &docxParser{page: 1, useRendered: bytes.Contains(doc, []byte("lastRenderedPageBreak"))}
	if raw, _ := readZipFile(zr, "word/styles.xml"); raw != nil {
		p.styles = parseDocxStyles(raw)
	}
	if raw, _ := readZipFile(zr, "docProps/core.xml"); raw != nil {
		p.title = coreTitle(raw)
	}
	dec := xml.NewDecoder(bytes.NewReader(doc))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: docx xml: %v", ErrUnsupportedFormat, err)
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "body" {
			if err := p.parseBody(dec); err != nil {
				return nil, fmt.Errorf("%w: docx xml: %v", ErrUnsupportedFormat, err)
			}
		}
	}
	parser := "docx"
	if p.useRendered {
		parser = "docx(rendered-page-breaks)"
	} else {
		p.warnings = append(p.warnings, "文档未记录 Word 渲染分页位置，页码仅按显式分页符/分节符估算")
	}
	return &Parsed{Title: p.title, Lines: p.lines, TOC: p.toc, TOCFields: p.tocFields,
		Meta: ParseMeta{Parser: parser, Headings: p.headings, Pages: p.page, Warnings: p.warnings}}, nil
}

func (p *docxParser) parseBody(dec *xml.Decoder) error {
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "p":
				para, err := p.parsePara(dec)
				if err != nil {
					return err
				}
				p.emitPara(para)
			case "tbl":
				start := p.page
				tbl, err := p.parseTable(dec)
				if err != nil {
					return err
				}
				tbl.PageStart = min(tbl.PageStart, start)
				p.lines = append(p.lines, Line{Text: tbl.Markdown, Page: tbl.PageStart, PageEnd: tbl.PageEnd, Table: tbl, Block: p.block})
				p.block++
			}
			// sdt / sdtContent / customXml 等容器直接下钻，其中的 p / tbl 按上面处理。
		case xml.EndElement:
			if t.Name.Local == "body" {
				return nil
			}
		}
	}
}

type docxPara struct {
	text      string
	tocText   string // 位于 TOC 域缓存结果中的文字（制表符保留为 	）
	style     string
	outline   int
	pageStart int
	pageEnd   int
}

// parsePara 读取一个 w:p（已消费其开始标签）直到对应结束标签；文本框中的嵌套段落文本并入本段。
func (p *docxParser) parsePara(dec *xml.Decoder) (docxPara, error) {
	para := docxPara{outline: -1, pageStart: p.page}
	var b, tb strings.Builder
	depth, inText, inPPr := 0, false, false
	// brk 记录一次分页：段首分页时整段落在新页，段中分页时段落跨页。
	brk := func() {
		p.page++
		if strings.TrimSpace(b.String()) == "" {
			para.pageStart = p.page
		}
	}
	sectionBreak := false
	for {
		tok, err := dec.Token()
		if err != nil {
			return para, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "p":
				depth++
			case "pPr":
				if depth == 0 {
					inPPr = true
				}
			case "pStyle":
				if inPPr {
					para.style, _ = attr(t, "val")
				}
			case "outlineLvl":
				if inPPr {
					if v, ok := attr(t, "val"); ok {
						para.outline, _ = strconv.Atoi(v)
					}
				}
			case "pageBreakBefore":
				if inPPr && !p.useRendered && onOff(t) {
					brk()
				}
			case "sectPr":
				if inPPr {
					sectionBreak = true
				}
			case "type":
				if inPPr && sectionBreak {
					if v, _ := attr(t, "val"); v == "continuous" {
						sectionBreak = false
					}
				}
			case "lastRenderedPageBreak":
				if p.useRendered {
					brk()
				}
			case "br":
				if v, _ := attr(t, "type"); v == "page" {
					if !p.useRendered {
						brk()
					}
				} else if v != "column" {
					b.WriteString("\n")
				}
			case "cr":
				b.WriteString("\n")
			case "tab":
				if !inPPr {
					b.WriteString(" ")
					if p.inTOCResult() {
						tb.WriteString("	")
					}
				}
			case "fldChar":
				switch v, _ := attr(t, "fldCharType"); v {
				case "begin":
					p.fields = append(p.fields, docxField{})
				case "separate":
					if n := len(p.fields); n > 0 {
						p.fields[n-1].result = true
						if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(p.fields[n-1].instr)), "TOC") {
							p.tocFields++
						}
					}
				case "end":
					if n := len(p.fields); n > 0 {
						p.fields = p.fields[:n-1]
					}
				}
			case "instrText":
				p.inInstr = true
			case "noBreakHyphen":
				b.WriteString("-")
			case "t":
				inText = true
			}
		case xml.CharData:
			if inText {
				b.Write(t)
				if p.inTOCResult() {
					tb.Write(t)
				}
			}
			if p.inInstr {
				if n := len(p.fields); n > 0 {
					p.fields[n-1].instr += string(t)
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "instrText":
				p.inInstr = false
			case "pPr":
				inPPr = false
			case "p":
				if depth == 0 {
					para.text = b.String()
					para.tocText = tb.String()
					para.pageEnd = p.page
					if sectionBreak && !p.useRendered {
						p.page++
					}
					return para, nil
				}
				depth--
				b.WriteString("\n")
			}
		}
	}
}

var reHeadingName = regexp.MustCompile(`(?i)^(?:heading|标题)\s*(\d)$`)

// headingLevel 解析段落的大纲级别（1..9），0 表示正文。
func (p *docxParser) headingLevel(para docxPara) int {
	if para.outline >= 0 && para.outline < 9 {
		return para.outline + 1
	}
	id := para.style
	for range 10 {
		if id == "" {
			return 0
		}
		st, ok := p.styles[id]
		if !ok {
			if m := reHeadingName.FindStringSubmatch(strings.ReplaceAll(id, " ", "")); m != nil {
				n, _ := strconv.Atoi(m[1])
				return n
			}
			return 0
		}
		if st.outline >= 0 && st.outline < 9 {
			return st.outline + 1
		}
		if m := reHeadingName.FindStringSubmatch(strings.TrimSpace(st.name)); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
		id = st.basedOn
	}
	return 0
}

var reTOCStyle = regexp.MustCompile(`(?i)^(?:toc|目录)\s*(\d)$`)

// tocLevel 由目录段落样式（toc N / 目录 N）得到条目级别，未知时为 0。
func (p *docxParser) tocLevel(id string) int {
	name := id
	if st, ok := p.styles[id]; ok && st.name != "" {
		name = st.name
	}
	if m := reTOCStyle.FindStringSubmatch(strings.TrimSpace(name)); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

func (p *docxParser) isTitleStyle(id string) bool {
	if strings.EqualFold(id, "Title") {
		return true
	}
	st, ok := p.styles[id]
	return ok && (strings.EqualFold(st.name, "title") || st.name == "标题")
}

func (p *docxParser) emitPara(para docxPara) {
	if t := strings.TrimSpace(para.tocText); t != "" {
		e := TOCEntry{Level: p.tocLevel(para.style), Text: strings.Join(strings.Fields(strings.ReplaceAll(t, "	", " ")), " ")}
		if i := strings.LastIndex(t, "	"); i >= 0 {
			e.Title = strings.Join(strings.Fields(t[:i]), " ")
			e.Page = strings.TrimSpace(t[i+1:])
		} else {
			e.Title = e.Text
		}
		p.toc = append(p.toc, e)
	}
	level := p.headingLevel(para)
	text := strings.TrimRight(para.text, " \t\n")
	if p.isTitleStyle(para.style) && p.title == "" && strings.TrimSpace(text) != "" {
		p.title = strings.TrimSpace(text)
	}
	if level > 0 && strings.TrimSpace(text) != "" {
		p.headings++
		// 标题中的软换行合并为空格，避免拆成多行。
		p.lines = append(p.lines, Line{Text: strings.Join(strings.Fields(text), " "), Page: para.pageStart, PageEnd: para.pageEnd, Heading: level, Block: p.block})
		p.block++
		return
	}
	for _, seg := range strings.Split(text, "\n") {
		p.lines = append(p.lines, Line{Text: seg, Page: para.pageStart, PageEnd: para.pageEnd, Block: p.block})
	}
	p.block++
}

// parseTable 读取一个 w:tbl（已消费开始标签）。
func (p *docxParser) parseTable(dec *xml.Decoder) (*Table, error) {
	b := newTableBuilder()
	headerRows, headerOpen := 0, true
	var (
		row, col                    = -1, 0
		inTrPr, inTcPr, rowIsHeader bool
		span                        int
		vmerge                      string
		cellTexts                   []string
	)
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "tr":
				row, col, rowIsHeader = b.startRow(p.page), 0, false
			case "trPr":
				inTrPr = true
			case "tblHeader":
				if inTrPr && onOff(t) {
					rowIsHeader = true
				}
			case "gridBefore":
				if inTrPr {
					if v, ok := attr(t, "val"); ok {
						n, _ := strconv.Atoi(v)
						col += n
					}
				}
			case "tc":
				span, vmerge, cellTexts = 1, "", nil
			case "tcPr":
				inTcPr = true
			case "gridSpan":
				if inTcPr {
					if v, ok := attr(t, "val"); ok {
						span, _ = strconv.Atoi(v)
					}
				}
			case "vMerge":
				if inTcPr {
					vmerge = "continue"
					if v, _ := attr(t, "val"); v == "restart" {
						vmerge = "restart"
					}
				}
			case "p":
				para, err := p.parsePara(dec)
				if err != nil {
					return nil, err
				}
				cellTexts = append(cellTexts, strings.TrimSpace(para.text))
			case "tbl":
				nested, err := p.parseTable(dec)
				if err != nil {
					return nil, err
				}
				for _, r := range nested.Grid {
					cellTexts = append(cellTexts, strings.Join(r, " / "))
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "trPr":
				inTrPr = false
			case "tcPr":
				inTcPr = false
			case "tc":
				if row < 0 {
					continue
				}
				span = max(span, 1)
				text := strings.TrimSpace(strings.Join(nonEmpty(cellTexts), "\n"))
				if vmerge == "continue" {
					if _, ok := b.extendDown(row, col); !ok {
						b.add(row, col, span, text)
					}
				} else {
					b.add(row, col, span, text)
				}
				col += span
			case "tr":
				if headerOpen && rowIsHeader {
					headerRows++
				} else {
					headerOpen = false
				}
			case "tbl":
				t := b.build(headerRows, "docx")
				t.PageEnd = max(t.PageEnd, p.page)
				return t, nil
			}
		}
	}
}

func nonEmpty(ss []string) []string {
	out := ss[:0:0]
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func parseDocxStyles(raw []byte) map[string]docxStyle {
	out := map[string]docxStyle{}
	dec := xml.NewDecoder(bytes.NewReader(raw))
	var (
		cur   docxStyle
		curID string
		in    bool
	)
	for {
		tok, err := dec.Token()
		if err != nil {
			return out
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "style":
				typ, _ := attr(t, "type")
				in = typ == "" || typ == "paragraph"
				curID, _ = attr(t, "styleId")
				cur = docxStyle{outline: -1}
			case "name":
				if in {
					cur.name, _ = attr(t, "val")
				}
			case "basedOn":
				if in {
					cur.basedOn, _ = attr(t, "val")
				}
			case "outlineLvl":
				if in {
					if v, ok := attr(t, "val"); ok {
						cur.outline, _ = strconv.Atoi(v)
					}
				}
			}
		case xml.EndElement:
			if t.Name.Local == "style" && in && curID != "" {
				out[curID] = cur
				in = false
			}
		}
	}
}

func coreTitle(raw []byte) string {
	dec := xml.NewDecoder(bytes.NewReader(raw))
	in := false
	for {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		switch t := tok.(type) {
		case xml.StartElement:
			in = t.Name.Local == "title"
		case xml.CharData:
			if in {
				return strings.TrimSpace(string(t))
			}
		case xml.EndElement:
			in = false
		}
	}
}
