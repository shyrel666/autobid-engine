package export

// .docx 套版模板：保留模板的样式、主题、页眉页脚、页面设置与封面，只替换正文中的 {{content}} 段落，
// 并替换 {{title}} / {{task}} / {{bidder}} / {{date}} / {{outline_version}} 文本占位符（须位于同一个 w:t 内）。
// 生成正文所需而模板缺少的样式（标题 1~6、目录 1~3、题注、表格网格与正文辅助样式）按样式名补齐。
// 补齐的样式使用中性名称（Contents Title、Subheading、Table Text 等），不带平台字样：同一平台导出的不同企业文件不应在
// styles.xml 里留下可识别的共同标记。早期模板中名为「Autobid ...」的同用途样式仍被识别并沿用（legacy）。

import (
	"archive/zip"
	"bytes"
	_ "embed"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

//go:embed templates/default.docx
var defaultTemplateDocx []byte

var ErrTemplate = errors.New("export: invalid .docx template")

// Geometry 是模板最后一节的页面设置（单位 twip，1/20 pt）。
type Geometry struct {
	PageW, PageH                           int
	Top, Right, Bottom, Left, Header, Foot int
	Gutter                                 int
}

// TextWidth / TextHeight 是版芯尺寸（twip）。
func (g Geometry) TextWidth() int  { return g.PageW - g.Left - g.Right - g.Gutter }
func (g Geometry) TextHeight() int { return g.PageH - g.Top - g.Bottom }

// Landscape 返回横向页面的几何（宽高互换，页边距不变）。
func (g Geometry) Landscape() Geometry {
	l := g
	if g.PageW < g.PageH {
		l.PageW, l.PageH = g.PageH, g.PageW
	}
	return l
}

// Template 是解析后的模板包。
type Template struct {
	Name     string
	Hash     string
	names    []string
	files    map[string][]byte
	styles   map[string]string // 小写样式名 -> styleId
	styleIDs map[string]bool
	Geometry Geometry
	sectPr   string // 正文最后一节的 w:sectPr 原文
	docRoot  string // document.xml 根元素开始标签
	bodyPre  string // {{content}} 段落之前的正文 XML
	bodyPost string // {{content}} 段落之后、最后 sectPr 之前的正文 XML
	needsBrk bool   // 正文前没有分节 / 分页时，目录需要另起一页
}

// TemplateMeta 是模板登记时记录的摘要。
type TemplateMeta struct {
	PageWidthMM   float64   `json:"page_width_mm"`
	PageHeightMM  float64   `json:"page_height_mm"`
	MarginsMM     []float64 `json:"margins_mm"`
	HasContentTag bool      `json:"has_content_placeholder"`
	MissingStyles []string  `json:"missing_styles"`
}

// DefaultTemplate 返回内置默认模板。
func DefaultTemplate() (*Template, error) { return LoadTemplate("default", defaultTemplateDocx) }

// DefaultTemplateBytes 返回内置默认模板文件（供用户下载后在 Word 中修改再上传）。
func DefaultTemplateBytes() []byte { return append([]byte(nil), defaultTemplateDocx...) }

// DefaultTemplateHash 是内置默认模板的 SHA-256。
func DefaultTemplateHash() string { return sha256Hex(defaultTemplateDocx) }

var (
	reSectPr   = regexp.MustCompile(`(?s)<w:sectPr[ >].*?</w:sectPr>`)
	reAttrVal  = regexp.MustCompile(`w:(\w+)="(-?\d+)"`)
	reBodyOpen = regexp.MustCompile(`<w:body\s*>`)
	reRootOpen = regexp.MustCompile(`(?s)<w:document\b[^>]*>`)
)

// LoadTemplate 解析 .docx 模板。
func LoadTemplate(name string, data []byte) (*Template, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTemplate, err)
	}
	t := &Template{Name: name, Hash: sha256Hex(data), files: map[string][]byte{}}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrTemplate, err)
		}
		b, err := io.ReadAll(io.LimitReader(rc, 64<<20))
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrTemplate, err)
		}
		t.names = append(t.names, f.Name)
		t.files[f.Name] = b
	}
	doc, ok := t.files["word/document.xml"]
	if !ok {
		return nil, fmt.Errorf("%w: missing word/document.xml", ErrTemplate)
	}
	for _, need := range []string{"[Content_Types].xml", "word/_rels/document.xml.rels", "word/styles.xml"} {
		if _, ok := t.files[need]; !ok {
			return nil, fmt.Errorf("%w: missing %s", ErrTemplate, need)
		}
	}
	if err := t.splitBody(string(doc)); err != nil {
		return nil, err
	}
	t.styles, t.styleIDs = parseStyleNames(t.files["word/styles.xml"])
	return t, nil
}

func (t *Template) splitBody(doc string) error {
	root := reRootOpen.FindString(doc)
	bo := reBodyOpen.FindStringIndex(doc)
	bc := strings.LastIndex(doc, "</w:body>")
	if root == "" || bo == nil || bc < bo[1] {
		return fmt.Errorf("%w: document.xml has no w:body", ErrTemplate)
	}
	t.docRoot = root
	body := doc[bo[1]:bc]
	// 最后一节的 sectPr 是 body 的最后一个子元素。
	locs := reSectPr.FindAllStringIndex(body, -1)
	if len(locs) == 0 {
		return fmt.Errorf("%w: document has no section properties (w:sectPr)", ErrTemplate)
	}
	last := locs[len(locs)-1]
	if strings.TrimSpace(body[last[1]:]) != "" {
		return fmt.Errorf("%w: the final w:sectPr must be the last child of w:body", ErrTemplate)
	}
	t.sectPr = body[last[0]:last[1]]
	content := body[:last[0]]
	g, err := parseGeometry(t.sectPr)
	if err != nil {
		return err
	}
	t.Geometry = g
	if i := strings.Index(content, "{{content}}"); i >= 0 {
		ps := lastParaStart(content[:i])
		pe := strings.Index(content[i:], "</w:p>")
		if ps < 0 || pe < 0 {
			return fmt.Errorf("%w: {{content}} must be inside a paragraph", ErrTemplate)
		}
		t.bodyPre, t.bodyPost = content[:ps], content[i+pe+len("</w:p>"):]
	} else {
		t.bodyPre = content
	}
	tailPara := t.bodyPre[max(0, lastParaStart(t.bodyPre)):]
	hasText := strings.Contains(t.bodyPre, "<w:t")
	t.needsBrk = hasText && !strings.Contains(tailPara, "<w:sectPr") && !strings.Contains(tailPara, `w:type="page"`)
	return nil
}

// lastParaStart 返回最后一个 <w:p> / <w:p ...> 开始标签的位置（不匹配 w:pPr 等）。
func lastParaStart(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		i = strings.LastIndex(s[:i+1], "<w:p")
		if i < 0 {
			return -1
		}
		if n := i + 4; n < len(s) && (s[n] == '>' || s[n] == ' ') {
			return i
		}
	}
	return -1
}

func parseGeometry(sect string) (Geometry, error) {
	var g Geometry
	get := func(tag string) map[string]int {
		out := map[string]int{}
		re := regexp.MustCompile(`<w:` + tag + `\b[^>]*/?>`)
		el := re.FindString(sect)
		for _, m := range reAttrVal.FindAllStringSubmatch(el, -1) {
			v, _ := strconv.Atoi(m[2])
			out[m[1]] = v
		}
		return out
	}
	sz, mar := get("pgSz"), get("pgMar")
	g.PageW, g.PageH = sz["w"], sz["h"]
	g.Top, g.Right, g.Bottom, g.Left = abs(mar["top"]), mar["right"], abs(mar["bottom"]), mar["left"]
	g.Header, g.Foot, g.Gutter = mar["header"], mar["footer"], mar["gutter"]
	if g.PageW <= 0 || g.PageH <= 0 || g.TextWidth() <= 1440 || g.TextHeight() <= 1440 {
		return g, fmt.Errorf("%w: unusable page size / margins in the final w:sectPr", ErrTemplate)
	}
	return g, nil
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func parseStyleNames(raw []byte) (map[string]string, map[string]bool) {
	names, ids := map[string]string{}, map[string]bool{}
	dec := xml.NewDecoder(bytes.NewReader(raw))
	var cur string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "style":
			cur = ""
			for _, a := range se.Attr {
				if a.Name.Local == "styleId" {
					cur = a.Value
					ids[cur] = true
				}
			}
		case "name":
			if cur == "" {
				continue
			}
			for _, a := range se.Attr {
				if a.Name.Local == "val" {
					k := strings.ToLower(strings.TrimSpace(a.Value))
					if _, dup := names[k]; !dup {
						names[k] = cur
					}
				}
			}
		}
	}
	return names, ids
}

// Meta 返回模板摘要（上传时登记）。
func (t *Template) Meta() TemplateMeta {
	mm := func(tw int) float64 { return float64(tw) / 56.6929 }
	g := t.Geometry
	m := TemplateMeta{PageWidthMM: round1(mm(g.PageW)), PageHeightMM: round1(mm(g.PageH)),
		MarginsMM:     []float64{round1(mm(g.Top)), round1(mm(g.Right)), round1(mm(g.Bottom)), round1(mm(g.Left))},
		HasContentTag: t.bodyPost != "" || strings.Contains(string(t.files["word/document.xml"]), "{{content}}"), MissingStyles: []string{}}
	for _, s := range builtinStyles {
		_, ok := t.styles[s.name]
		if _, old := t.styles[s.legacy]; !ok && !(old && s.legacy != "") {
			m.MissingStyles = append(m.MissingStyles, s.name)
		}
	}
	sort.Strings(m.MissingStyles)
	return m
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

// ---------------------------------------------------------------------------
// 样式
// ---------------------------------------------------------------------------

// zeroInd 显式清零首行缩进：Word 中 firstLineChars 优先于 firstLine，必须两者同时写 0，
// 否则正文样式的「首行缩进 2 字符」会被表格、图片与题注继承，把图片推出版芯、把窄列表头挤成竖排。
const zeroInd = `<w:ind w:firstLineChars="0" w:firstLine="0"/>`

type builtinStyle struct {
	name, id, kind, xml string
	legacy              string // 早期版本的样式名（小写），模板里有它时沿用模板定义
}

// builtinStyles 是导出正文用到的样式；模板已有同名样式时沿用模板定义。
var builtinStyles = func() []builtinStyle {
	h := func(n int, size int) builtinStyle {
		id := fmt.Sprintf("Heading%d", n)
		return builtinStyle{name: fmt.Sprintf("heading %d", n), id: id, kind: "paragraph", xml: fmt.Sprintf(
			`<w:style w:type="paragraph" w:styleId="%s"><w:name w:val="heading %d"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:uiPriority w:val="9"/><w:qFormat/>`+
				`<w:pPr><w:keepNext/><w:keepLines/><w:spacing w:before="%d" w:after="%d" w:line="240" w:lineRule="auto"/>`+zeroInd+`<w:outlineLvl w:val="%d"/></w:pPr>`+
				`<w:rPr><w:rFonts w:ascii="Arial" w:hAnsi="Arial" w:eastAsia="SimHei" w:cs="Arial"/><w:b/><w:bCs/><w:sz w:val="%d"/><w:szCs w:val="%d"/></w:rPr></w:style>`,
			id, n, 360-40*min(n, 4), 200-30*min(n, 4), n-1, size, size)}
	}
	toc := func(n int) builtinStyle {
		id := fmt.Sprintf("TOC%d", n)
		return builtinStyle{name: fmt.Sprintf("toc %d", n), id: id, kind: "paragraph", xml: fmt.Sprintf(
			`<w:style w:type="paragraph" w:styleId="%s"><w:name w:val="toc %d"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:uiPriority w:val="39"/>`+
				`<w:pPr><w:tabs><w:tab w:val="right" w:leader="dot" w:pos="9638"/></w:tabs><w:spacing w:before="60" w:after="60" w:line="300" w:lineRule="auto"/><w:ind w:leftChars="0" w:left="%d" w:firstLineChars="0" w:firstLine="0"/></w:pPr>`+
				`<w:rPr><w:sz w:val="%d"/></w:rPr></w:style>`, id, n, (n-1)*420, 24-2*min(n-1, 1))}
	}
	return []builtinStyle{
		h(1, 32), h(2, 30), h(3, 28), h(4, 26), h(5, 24), h(6, 24),
		toc(1), toc(2), toc(3),
		{name: "caption", id: "Caption", kind: "paragraph", xml: `<w:style w:type="paragraph" w:styleId="Caption"><w:name w:val="caption"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/>` +
			`<w:pPr><w:keepLines/><w:spacing w:before="60" w:after="120" w:line="240" w:lineRule="auto"/>` + zeroInd + `<w:jc w:val="center"/></w:pPr><w:rPr><w:rFonts w:eastAsia="SimHei"/><w:sz w:val="20"/></w:rPr></w:style>`},
		{name: "table grid", id: "TableGrid", kind: "table", xml: `<w:style w:type="table" w:styleId="TableGrid"><w:name w:val="Table Grid"/><w:basedOn w:val="TableNormal"/><w:uiPriority w:val="59"/>` +
			`<w:tblPr><w:tblBorders><w:top w:val="single" w:sz="4" w:space="0" w:color="000000"/><w:left w:val="single" w:sz="4" w:space="0" w:color="000000"/>` +
			`<w:bottom w:val="single" w:sz="4" w:space="0" w:color="000000"/><w:right w:val="single" w:sz="4" w:space="0" w:color="000000"/>` +
			`<w:insideH w:val="single" w:sz="4" w:space="0" w:color="000000"/><w:insideV w:val="single" w:sz="4" w:space="0" w:color="000000"/></w:tblBorders>` +
			`<w:tblCellMar><w:left w:w="85" w:type="dxa"/><w:right w:w="85" w:type="dxa"/></w:tblCellMar></w:tblPr></w:style>`},
		{name: "contents title", id: "ContentsTitle", kind: "paragraph", legacy: "autobid toc title", xml: `<w:style w:type="paragraph" w:customStyle="1" w:styleId="ContentsTitle"><w:name w:val="Contents Title"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/>` +
			`<w:pPr><w:spacing w:before="240" w:after="240" w:line="240" w:lineRule="auto"/>` + zeroInd + `<w:jc w:val="center"/></w:pPr><w:rPr><w:rFonts w:eastAsia="SimHei"/><w:b/><w:sz w:val="32"/></w:rPr></w:style>`},
		{name: "subheading", id: "Subheading", kind: "paragraph", legacy: "autobid subheading", xml: `<w:style w:type="paragraph" w:customStyle="1" w:styleId="Subheading"><w:name w:val="Subheading"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/>` +
			`<w:pPr><w:keepNext/><w:spacing w:before="120" w:after="60"/>` + zeroInd + `</w:pPr><w:rPr><w:rFonts w:eastAsia="SimHei"/><w:b/></w:rPr></w:style>`},
		{name: "list text", id: "ListText", kind: "paragraph", legacy: "autobid list", xml: `<w:style w:type="paragraph" w:customStyle="1" w:styleId="ListText"><w:name w:val="List Text"/><w:basedOn w:val="Normal"/>` +
			`<w:pPr><w:spacing w:before="0" w:after="0"/>` + zeroInd + `</w:pPr></w:style>`},
		{name: "table text", id: "TableText", kind: "paragraph", legacy: "autobid table text", xml: `<w:style w:type="paragraph" w:customStyle="1" w:styleId="TableText"><w:name w:val="Table Text"/><w:basedOn w:val="Normal"/>` +
			`<w:pPr><w:spacing w:before="20" w:after="20" w:line="240" w:lineRule="auto"/><w:ind w:firstLineChars="0" w:firstLine="0"/><w:jc w:val="left"/></w:pPr><w:rPr><w:sz w:val="18"/><w:szCs w:val="18"/></w:rPr></w:style>`},
		{name: "code text", id: "CodeText", kind: "paragraph", legacy: "autobid code", xml: `<w:style w:type="paragraph" w:customStyle="1" w:styleId="CodeText"><w:name w:val="Code Text"/><w:basedOn w:val="Normal"/>` +
			`<w:pPr><w:spacing w:before="0" w:after="0" w:line="240" w:lineRule="auto"/><w:ind w:firstLineChars="0" w:firstLine="0"/><w:jc w:val="left"/></w:pPr><w:rPr><w:rFonts w:ascii="Consolas" w:hAnsi="Consolas" w:eastAsia="SimSun" w:cs="Consolas"/><w:sz w:val="16"/><w:szCs w:val="16"/></w:rPr></w:style>`},
		{name: "figure", id: "Figure", kind: "paragraph", legacy: "autobid figure", xml: `<w:style w:type="paragraph" w:customStyle="1" w:styleId="Figure"><w:name w:val="Figure"/><w:basedOn w:val="Normal"/><w:next w:val="Caption"/>` +
			`<w:pPr><w:keepNext/><w:spacing w:before="120" w:after="0" w:line="240" w:lineRule="auto"/><w:ind w:firstLineChars="0" w:firstLine="0"/><w:jc w:val="center"/></w:pPr></w:style>`},
	}
}()

// resolveStyles 返回「样式名 -> styleId」，并把模板缺少的样式定义补进 styles.xml。
func (t *Template) resolveStyles() (map[string]string, []byte) {
	out := map[string]string{}
	var add strings.Builder
	for _, s := range builtinStyles {
		if id, ok := t.styles[s.name]; ok {
			out[s.name] = id
			continue
		}
		if id, ok := t.styles[s.legacy]; ok && s.legacy != "" {
			out[s.name] = id
			continue
		}
		id := s.id
		x := s.xml
		for t.styleIDs[id] { // styleId 被其他样式占用
			id += "A"
			x = strings.Replace(x, `w:styleId="`+s.id+`"`, `w:styleId="`+id+`"`, 1)
		}
		out[s.name] = id
		add.WriteString(x)
	}
	styles := t.files["word/styles.xml"]
	if add.Len() > 0 {
		s := string(styles)
		if i := strings.LastIndex(s, "</w:styles>"); i >= 0 {
			styles = []byte(s[:i] + add.String() + s[i:])
		}
	}
	return out, styles
}
