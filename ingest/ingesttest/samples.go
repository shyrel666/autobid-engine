// Package ingesttest 构造内存中的 .docx / .pdf 固定样本，供解析器、知识库与 HTTP 测试使用。
// 样本只包含解析所需的最小结构：DOCX 为 WordprocessingML（标题样式、分页标记、合并单元格、重复表头），
// PDF 为 Type0/Identity-H 字体 + ToUnicode CMap + /W 字宽 + FlateDecode 内容流（可附带 DCT 扫描图像页）。
package ingesttest

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"fmt"
	"html"
	"sort"
	"strings"
	"unicode/utf16"
)

// ---------------------------------------------------------------------------
// DOCX
// ---------------------------------------------------------------------------

// Para 是一个段落。Heading 为 1..3 时使用 Heading{n} 样式；RenderedBreak 在段首写入 w:lastRenderedPageBreak。
type Para struct {
	Text          string
	Heading       int
	RenderedBreak bool
}

// Cell 是一个单元格：Span 为横向合并列数（gridSpan），VMerge 为 "restart" / "continue"（纵向合并）。
type Cell struct {
	Text   string
	Span   int
	VMerge string
}

// Row 是一行；Header 标记 w:tblHeader 重复表头；RenderedBreak 表示该行起于新页（在首个单元格段首写入分页标记）。
type Row struct {
	Header        bool
	RenderedBreak bool
	Cells         []Cell
}

type Table struct {
	Cols int
	Rows []Row
}

// BuildDOCX 生成 .docx 字节；blocks 元素为 Para 或 Table。
func BuildDOCX(title string, blocks ...any) []byte {
	var body strings.Builder
	for _, b := range blocks {
		switch v := b.(type) {
		case Para:
			body.WriteString(docxPara(v))
		case Table:
			body.WriteString(`<w:tbl><w:tblPr><w:tblStyle w:val="TableGrid"/></w:tblPr><w:tblGrid>`)
			for range v.Cols {
				body.WriteString(`<w:gridCol w:w="2000"/>`)
			}
			body.WriteString(`</w:tblGrid>`)
			for _, r := range v.Rows {
				body.WriteString(`<w:tr>`)
				if r.Header {
					body.WriteString(`<w:trPr><w:tblHeader/></w:trPr>`)
				}
				for i, c := range r.Cells {
					body.WriteString(`<w:tc><w:tcPr>`)
					if c.Span > 1 {
						fmt.Fprintf(&body, `<w:gridSpan w:val="%d"/>`, c.Span)
					}
					switch c.VMerge {
					case "restart":
						body.WriteString(`<w:vMerge w:val="restart"/>`)
					case "continue":
						body.WriteString(`<w:vMerge/>`)
					}
					body.WriteString(`</w:tcPr>`)
					body.WriteString(docxPara(Para{Text: c.Text, RenderedBreak: r.RenderedBreak && i == 0}))
					body.WriteString(`</w:tc>`)
				}
				body.WriteString(`</w:tr>`)
			}
			body.WriteString(`</w:tbl>`)
		}
	}
	doc := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` +
		body.String() + `<w:sectPr><w:pgSz w:w="11906" w:h="16838"/></w:sectPr></w:body></w:document>`
	styles := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style>` +
		`<w:style w:type="paragraph" w:styleId="Title"><w:name w:val="Title"/><w:basedOn w:val="Normal"/></w:style>` +
		`<w:style w:type="paragraph" w:styleId="Heading1"><w:name w:val="heading 1"/><w:basedOn w:val="Normal"/><w:pPr><w:outlineLvl w:val="0"/></w:pPr></w:style>` +
		// 中文版 Word 常见的数字样式 ID：只能通过样式名识别级别。
		`<w:style w:type="paragraph" w:styleId="2"><w:name w:val="heading 2"/><w:basedOn w:val="Normal"/></w:style>` +
		`<w:style w:type="paragraph" w:styleId="Heading3"><w:name w:val="Custom Clause"/><w:basedOn w:val="Normal"/><w:pPr><w:outlineLvl w:val="2"/></w:pPr></w:style>` +
		`</w:styles>`
	core := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/">` +
		`<dc:title>` + html.EscapeString(title) + `</dc:title></cp:coreProperties>`
	files := map[string]string{
		"[Content_Types].xml": `<?xml version="1.0" encoding="UTF-8"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
			`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/>` +
			`<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`,
		"_rels/.rels": `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`,
		"word/document.xml": doc,
		"word/styles.xml":   styles,
		"docProps/core.xml": core,
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		w, _ := zw.Create(n)
		_, _ = w.Write([]byte(files[n]))
	}
	_ = zw.Close()
	return buf.Bytes()
}

func docxPara(p Para) string {
	var b strings.Builder
	b.WriteString(`<w:p>`)
	switch p.Heading {
	case 1:
		b.WriteString(`<w:pPr><w:pStyle w:val="Heading1"/></w:pPr>`)
	case 2:
		b.WriteString(`<w:pPr><w:pStyle w:val="2"/></w:pPr>`)
	case 3:
		b.WriteString(`<w:pPr><w:pStyle w:val="Heading3"/></w:pPr>`)
	case -1:
		b.WriteString(`<w:pPr><w:pStyle w:val="Title"/></w:pPr>`)
	}
	b.WriteString(`<w:r>`)
	if p.RenderedBreak {
		b.WriteString(`<w:lastRenderedPageBreak/>`)
	}
	fmt.Fprintf(&b, `<w:t xml:space="preserve">%s</w:t></w:r></w:p>`, html.EscapeString(p.Text))
	return b.String()
}

// ---------------------------------------------------------------------------
// PDF
// ---------------------------------------------------------------------------

// Text 是页面上的一段文字（PDF 坐标，原点左下，单位 pt）。
type Text struct {
	X, Y, Size float64
	S          string
}

// Page 是一页：Texts 为文字；JPEG 非空时页面只绘制一张 DCT 图像（模拟扫描件）。
type Page struct {
	Texts []Text
	JPEG  []byte
}

// PageHeight 是样本页面高度（A4 竖版）。
const PageHeight = 842

// TableTexts 把表格行排布为页面文字：首行 y0，行距 lineGap，列起点为 xs；空字符串单元格不输出。
func TableTexts(xs []float64, y0, lineGap, size float64, rows ...[]string) []Text {
	var out []Text
	for r, row := range rows {
		for c, s := range row {
			if s == "" || c >= len(xs) {
				continue
			}
			out = append(out, Text{X: xs[c], Y: y0 - float64(r)*lineGap, Size: size, S: s})
		}
	}
	return out
}

// BuildPDF 生成 PDF 字节。全部文字使用同一 Type0 字体：每个不同字符分配一个 CID，ToUnicode 映射回原字符，
// 字宽 CJK 1000、其余 500（/W 数组）。
func BuildPDF(title string, pages ...Page) []byte {
	cids := map[rune]int{}
	var runes []rune
	for _, p := range pages {
		for _, t := range p.Texts {
			for _, r := range t.S {
				if _, ok := cids[r]; !ok {
					cids[r] = len(runes) + 1
					runes = append(runes, r)
				}
			}
		}
	}
	var objs []string // objs[i] 为对象 i+1 的正文
	add := func(s string) int {
		objs = append(objs, s)
		return len(objs)
	}
	reserve := func() int { return add("") }

	catalog := reserve()
	pagesObj := reserve()
	fontObj := reserve()
	descObj := reserve()
	var cmap strings.Builder
	cmap.WriteString("/CIDInit /ProcSet findresource begin\n12 dict begin\nbegincmap\n/CMapName /Test-UCS def\n/CMapType 2 def\n")
	cmap.WriteString("1 begincodespacerange\n<0000> <FFFF>\nendcodespacerange\n")
	for i := 0; i < len(runes); i += 100 {
		chunk := runes[i:min(i+100, len(runes))]
		fmt.Fprintf(&cmap, "%d beginbfchar\n", len(chunk))
		for _, r := range chunk {
			u := utf16.Encode([]rune{r})
			var hex strings.Builder
			for _, x := range u {
				fmt.Fprintf(&hex, "%04X", x)
			}
			fmt.Fprintf(&cmap, "<%04X> <%s>\n", cids[r], hex.String())
		}
		cmap.WriteString("endbfchar\n")
	}
	cmap.WriteString("endcmap\nCMapName currentdict /CMap defineresource pop\nend\nend\n")
	toUni := add(stream("", []byte(cmap.String()), false))
	var w strings.Builder
	w.WriteString("[")
	for _, r := range runes {
		width := 500
		if r > 0x2E80 {
			width = 1000
		}
		fmt.Fprintf(&w, " %d [%d]", cids[r], width)
	}
	w.WriteString(" ]")
	objs[fontObj-1] = fmt.Sprintf("<< /Type /Font /Subtype /Type0 /BaseFont /TestSans /Encoding /Identity-H /DescendantFonts [%d 0 R] /ToUnicode %d 0 R >>", descObj, toUni)
	objs[descObj-1] = fmt.Sprintf("<< /Type /Font /Subtype /CIDFontType2 /BaseFont /TestSans /CIDSystemInfo << /Registry (Adobe) /Ordering (Identity) /Supplement 0 >> /DW 1000 /W %s >>", w.String())

	var kids []string
	for _, p := range pages {
		var content strings.Builder
		res := fmt.Sprintf("<< /Font << /F1 %d 0 R >> >>", fontObj)
		if len(p.JPEG) > 0 {
			img := add(stream("/Type /XObject /Subtype /Image /Width 100 /Height 140 /ColorSpace /DeviceGray /BitsPerComponent 8 /Filter /DCTDecode", p.JPEG, false))
			res = fmt.Sprintf("<< /XObject << /Im1 %d 0 R >> >>", img)
			content.WriteString("q 595 0 0 842 0 0 cm /Im1 Do Q\n")
		}
		for _, t := range p.Texts {
			var hex strings.Builder
			for _, r := range t.S {
				fmt.Fprintf(&hex, "%04X", cids[r])
			}
			fmt.Fprintf(&content, "BT /F1 %.1f Tf 1 0 0 1 %.2f %.2f Tm <%s> Tj ET\n", t.Size, t.X, t.Y, hex.String())
		}
		contents := add(stream("", []byte(content.String()), true))
		page := add(fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 595 %d] /Resources %s /Contents %d 0 R >>", pagesObj, PageHeight, res, contents))
		kids = append(kids, fmt.Sprintf("%d 0 R", page))
	}
	objs[pagesObj-1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(kids))
	objs[catalog-1] = fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pagesObj)
	info := add(fmt.Sprintf("<< /Title <FEFF%s> /Producer (ingesttest) >>", utf16Hex(title)))

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.7\n%\xE2\xE3\xCF\xD3\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root %d 0 R /Info %d 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, catalog, info, xref)
	return buf.Bytes()
}

func utf16Hex(s string) string {
	var b strings.Builder
	for _, x := range utf16.Encode([]rune(s)) {
		fmt.Fprintf(&b, "%04X", x)
	}
	return b.String()
}

// stream 生成流对象正文；deflate 为 true 时以 FlateDecode 压缩。
func stream(dict string, data []byte, deflate bool) string {
	if deflate {
		var z bytes.Buffer
		zw := zlib.NewWriter(&z)
		_, _ = zw.Write(data)
		_ = zw.Close()
		data = z.Bytes()
		dict += " /Filter /FlateDecode"
	}
	return fmt.Sprintf("<< %s /Length %d >>\nstream\n%s\nendstream", strings.TrimSpace(dict), len(data), data)
}
