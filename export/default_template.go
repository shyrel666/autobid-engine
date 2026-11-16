package export

// 内置默认模板 templates/default.docx 的生成代码（A4 纵向，页边距 上下 2.54cm / 左右 2.5cm；封面单独一节且首页不同，
// 无页眉页脚；正文页眉为项目名称，页脚为「第 N 页 共 M 页」；宋体小四正文、黑体标题）。
// 仓库中提交的 .docx 必须与本函数的输出一致（TestDefaultTemplateUpToDate 校验；设置 AUTOBID_UPDATE_TEMPLATE=1 重新生成）。

import (
	"strings"
)

const (
	wNS = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"`
	// DefaultPageGeometry 与 default.docx 的最后一节一致（twip）。
	defaultSect = `<w:headerReference w:type="default" r:id="rId10"/><w:footerReference w:type="default" r:id="rId11"/>` +
		`<w:pgSz w:w="11906" w:h="16838"/><w:pgMar w:top="1440" w:right="1418" w:bottom="1440" w:left="1418" w:header="851" w:footer="992" w:gutter="0"/>` +
		`<w:cols w:space="425"/><w:docGrid w:linePitch="312"/>`
)

func buildDefaultTemplate() ([]byte, error) {
	files := map[string][]byte{}
	order := []string{"[Content_Types].xml", "_rels/.rels", "docProps/core.xml", "word/document.xml", "word/styles.xml", "word/settings.xml",
		"word/header1.xml", "word/footer1.xml", "word/_rels/document.xml.rels"}
	files["[Content_Types].xml"] = []byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
		`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
		`<Default Extension="xml" ContentType="application/xml"/><Default Extension="png" ContentType="image/png"/>` +
		`<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>` +
		`<Override PartName="/word/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.styles+xml"/>` +
		`<Override PartName="/word/settings.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.settings+xml"/>` +
		`<Override PartName="/word/header1.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.header+xml"/>` +
		`<Override PartName="/word/footer1.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.footer+xml"/>` +
		`<Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/></Types>`)
	files["_rels/.rels"] = []byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>` +
		`<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/></Relationships>`)
	files["docProps/core.xml"] = []byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/">` +
		`<dc:title>{{title}}</dc:title><dc:creator>{{bidder}}</dc:creator></cp:coreProperties>`)
	files["word/_rels/document.xml.rels"] = []byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>` +
		`<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/settings" Target="settings.xml"/>` +
		`<Relationship Id="rId10" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/header" Target="header1.xml"/>` +
		`<Relationship Id="rId11" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/footer" Target="footer1.xml"/></Relationships>`)
	files["word/settings.xml"] = []byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><w:settings ` + wNS + `>` +
		`<w:zoom w:percent="100"/><w:defaultTabStop w:val="420"/><w:characterSpacingControl w:val="compressPunctuation"/>` +
		`<w:compat><w:compatSetting w:name="compatibilityMode" w:uri="http://schemas.microsoft.com/office/word" w:val="15"/></w:compat></w:settings>`)
	files["word/header1.xml"] = []byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><w:hdr ` + wNS + `>` +
		`<w:p><w:pPr><w:pStyle w:val="Header"/></w:pPr><w:r><w:t xml:space="preserve">{{title}} · 技术标</w:t></w:r></w:p></w:hdr>`)
	files["word/footer1.xml"] = []byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><w:ftr ` + wNS + `>` +
		`<w:p><w:pPr><w:pStyle w:val="Footer"/></w:pPr><w:r><w:t xml:space="preserve">第 </w:t></w:r>` +
		`<w:fldSimple w:instr=" PAGE "><w:r><w:t>1</w:t></w:r></w:fldSimple><w:r><w:t xml:space="preserve"> 页 共 </w:t></w:r>` +
		`<w:fldSimple w:instr=" NUMPAGES "><w:r><w:t>1</w:t></w:r></w:fldSimple><w:r><w:t xml:space="preserve"> 页</w:t></w:r></w:p></w:ftr>`)

	var st strings.Builder
	st.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><w:styles ` + wNS + `>`)
	st.WriteString(`<w:docDefaults><w:rPrDefault><w:rPr><w:rFonts w:ascii="Times New Roman" w:hAnsi="Times New Roman" w:eastAsia="SimSun" w:cs="Times New Roman"/>` +
		`<w:kern w:val="2"/><w:sz w:val="24"/><w:szCs w:val="24"/><w:lang w:val="en-US" w:eastAsia="zh-CN" w:bidi="ar-SA"/></w:rPr></w:rPrDefault>` +
		`<w:pPrDefault><w:pPr><w:spacing w:after="0" w:line="360" w:lineRule="auto"/></w:pPr></w:pPrDefault></w:docDefaults>`)
	st.WriteString(`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/><w:qFormat/>` +
		`<w:pPr><w:widowControl/><w:overflowPunct w:val="0"/><w:ind w:firstLine="480"/><w:jc w:val="both"/></w:pPr></w:style>`)
	st.WriteString(`<w:style w:type="character" w:default="1" w:styleId="DefaultParagraphFont"><w:name w:val="Default Paragraph Font"/><w:uiPriority w:val="1"/><w:semiHidden/></w:style>`)
	st.WriteString(`<w:style w:type="table" w:default="1" w:styleId="TableNormal"><w:name w:val="Normal Table"/><w:semiHidden/>` +
		`<w:tblPr><w:tblInd w:w="0" w:type="dxa"/><w:tblCellMar><w:top w:w="0" w:type="dxa"/><w:left w:w="108" w:type="dxa"/><w:bottom w:w="0" w:type="dxa"/><w:right w:w="108" w:type="dxa"/></w:tblCellMar></w:tblPr></w:style>`)
	st.WriteString(`<w:style w:type="paragraph" w:styleId="Title"><w:name w:val="Title"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/>` +
		`<w:pPr><w:spacing w:before="240" w:after="240" w:line="240" w:lineRule="auto"/><w:ind w:firstLineChars="0" w:firstLine="0"/><w:jc w:val="center"/></w:pPr>` +
		`<w:rPr><w:rFonts w:ascii="Arial" w:hAnsi="Arial" w:eastAsia="SimHei"/><w:b/><w:sz w:val="52"/><w:szCs w:val="52"/></w:rPr></w:style>`)
	st.WriteString(`<w:style w:type="paragraph" w:styleId="Subtitle"><w:name w:val="Subtitle"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/>` +
		`<w:pPr><w:spacing w:before="120" w:after="120" w:line="240" w:lineRule="auto"/><w:ind w:firstLineChars="0" w:firstLine="0"/><w:jc w:val="center"/></w:pPr>` +
		`<w:rPr><w:rFonts w:eastAsia="SimHei"/><w:sz w:val="36"/><w:szCs w:val="36"/></w:rPr></w:style>`)
	st.WriteString(`<w:style w:type="paragraph" w:customStyle="1" w:styleId="CoverInfo"><w:name w:val="Cover Info"/><w:basedOn w:val="Normal"/>` +
		`<w:pPr><w:spacing w:before="120" w:after="120" w:line="240" w:lineRule="auto"/><w:ind w:firstLineChars="0" w:firstLine="0"/><w:jc w:val="center"/></w:pPr><w:rPr><w:sz w:val="30"/></w:rPr></w:style>`)
	st.WriteString(`<w:style w:type="paragraph" w:styleId="Header"><w:name w:val="header"/><w:basedOn w:val="Normal"/>` +
		`<w:pPr><w:pBdr><w:bottom w:val="single" w:sz="6" w:space="1" w:color="auto"/></w:pBdr><w:spacing w:line="240" w:lineRule="auto"/><w:ind w:firstLineChars="0" w:firstLine="0"/><w:jc w:val="right"/></w:pPr>` +
		`<w:rPr><w:sz w:val="18"/><w:szCs w:val="18"/></w:rPr></w:style>`)
	st.WriteString(`<w:style w:type="paragraph" w:styleId="Footer"><w:name w:val="footer"/><w:basedOn w:val="Normal"/>` +
		`<w:pPr><w:spacing w:line="240" w:lineRule="auto"/><w:ind w:firstLineChars="0" w:firstLine="0"/><w:jc w:val="center"/></w:pPr><w:rPr><w:sz w:val="18"/><w:szCs w:val="18"/></w:rPr></w:style>`)
	for _, s := range builtinStyles {
		x := s.xml
		if strings.HasPrefix(s.name, "toc ") {
			x = strings.Replace(x, `w:pos="9638"`, `w:pos="9070"`, 1)
		}
		st.WriteString(x)
	}
	st.WriteString(`</w:styles>`)
	files["word/styles.xml"] = []byte(st.String())

	p := func(style, text string) string {
		if text == "" {
			return `<w:p><w:pPr><w:pStyle w:val="` + style + `"/></w:pPr></w:p>`
		}
		return `<w:p><w:pPr><w:pStyle w:val="` + style + `"/></w:pPr><w:r><w:t xml:space="preserve">` + text + `</w:t></w:r></w:p>`
	}
	var doc strings.Builder
	doc.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><w:document ` + wNS + `><w:body>`)
	for range 4 {
		doc.WriteString(p("CoverInfo", ""))
	}
	doc.WriteString(p("Title", "{{title}}"))
	doc.WriteString(p("Subtitle", "投标文件 · 技术标"))
	for range 6 {
		doc.WriteString(p("CoverInfo", ""))
	}
	doc.WriteString(p("CoverInfo", "投标人：{{bidder}}"))
	doc.WriteString(p("CoverInfo", "大纲版本：v{{outline_version}}"))
	// 封面所在节：首页不同（titlePg）且没有首页页眉页脚，因此封面不显示页眉页码。
	doc.WriteString(`<w:p><w:pPr><w:pStyle w:val="CoverInfo"/><w:sectPr>` + defaultSect + `<w:titlePg/></w:sectPr></w:pPr>` +
		`<w:r><w:t xml:space="preserve">日期：{{date}}</w:t></w:r></w:p>`)
	doc.WriteString(`<w:p><w:r><w:t>{{content}}</w:t></w:r></w:p>`)
	doc.WriteString(`<w:sectPr>` + defaultSect + `</w:sectPr></w:body></w:document>`)
	files["word/document.xml"] = []byte(doc.String())
	return writeZip(order, files)
}
