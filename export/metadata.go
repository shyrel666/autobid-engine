package export

// D4 成稿元数据（防雷同）：
//   87 号令第三十七条把「不同投标人的投标文件由同一单位或者个人编制」「异常一致」视为串通投标。多家企业共用同一套系统导出时，
//   文件里任何可识别的共同标记（固定作者、平台字样的样式名、操作系统用户名）都会成为这类线索。因此：
//   - 生成时 docProps/core.xml 整体重写：标题 = 项目名称，作者 / 最后修改者 = 投标人（为空则留空），时间 = 快照时间；
//   - 文档引擎另存 DOCX 后再清洗一次（SanitizeDOCX）：本机实测 WPS 12.1 把最后修改者写成登录的 WPS 账号（WPS_<数字>），
//     并在 custom.xml 写入 ICV 等与安装环境相关的属性；Word 会写本机用户名、公司与管理者；
//   - 回读（CheckDOCXMetadata / CheckPDFMetadata）：作者类字段只能是投标人或空，全部元数据不得出现平台字样。
// 文档引擎自己的产品标识（Application、Producer）保留：它对所有使用同一办公软件的投标人都一样，不构成共同编制的线索。

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/ingest"
)

// IssueMetadata 是元数据问题（平台字样或与投标人不符的作者）。严格最终稿不容许。
const IssueMetadata = "metadata_fingerprint"

// platformMarkers 是不得出现在成稿元数据与样式表中的平台字样（小写比较）。
var platformMarkers = []string{"autobid"}

// DocMeta 是写入成稿的文档属性。
type DocMeta struct {
	Title  string
	Author string // 投标人名称；为空时作者字段留空
	At     time.Time
}

// coreProps 生成 docProps/core.xml。
func coreProps(m DocMeta) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/"` +
		` xmlns:dcterms="http://purl.org/dc/terms/" xmlns:dcmitype="http://purl.org/dc/dcmitype/" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">`)
	b.WriteString(`<dc:title>` + esc(m.Title) + `</dc:title><dc:creator>` + esc(m.Author) + `</dc:creator><cp:lastModifiedBy>` + esc(m.Author) + `</cp:lastModifiedBy>`)
	if !m.At.IsZero() {
		at := m.At.UTC().Format("2006-01-02T15:04:05Z")
		b.WriteString(`<dcterms:created xsi:type="dcterms:W3CDTF">` + at + `</dcterms:created><dcterms:modified xsi:type="dcterms:W3CDTF">` + at + `</dcterms:modified>`)
	}
	b.WriteString(`</cp:coreProperties>`)
	return []byte(b.String())
}

const emptyCustomProps = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
	`<Properties xmlns="http://schemas.openxmlformats.org/officeDocument/2006/custom-properties" xmlns:vt="http://schemas.openxmlformats.org/officeDocument/2006/docPropsVTypes"/>`

var reAppScrub = regexp.MustCompile(`(?s)<(Company|Manager|Template)>.*?</(Company|Manager|Template)>`)

// scrubAppProps 清空 docProps/app.xml 中的公司、管理者与模板名（保留文档引擎的产品标识与统计信息）。
func scrubAppProps(raw []byte) []byte {
	return reAppScrub.ReplaceAllFunc(raw, func(m []byte) []byte {
		name := reAppScrub.FindSubmatch(m)[1]
		return []byte("<" + string(name) + "></" + string(name) + ">")
	})
}

// SanitizeDOCX 重写文档引擎另存后的文档属性：core.xml 换成 coreProps(m)，app.xml 清空公司 / 管理者 / 模板名；
// 其余条目按原压缩数据原样复制。
func SanitizeDOCX(data []byte, m DocMeta) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	mod := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, f := range zr.File {
		var repl []byte
		switch f.Name {
		case "docProps/core.xml":
			repl = coreProps(m)
		case "docProps/app.xml":
			raw, err := readZipFile(f)
			if err != nil {
				return nil, err
			}
			repl = scrubAppProps(raw)
		case "docProps/custom.xml":
			// WPS 写入的自定义属性（如 ICV）与安装环境相关，全部去掉。
			repl = []byte(emptyCustomProps)
		default:
			if err := zw.Copy(f); err != nil {
				return nil, err
			}
			continue
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: f.Name, Method: zip.Deflate, Modified: mod})
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(repl); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func readZipFile(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, 256<<20))
}

// Metadata 是回读到的成稿元数据（作者类字段与文档引擎标识）。
type Metadata struct {
	Title          string `json:"title,omitempty"`
	Author         string `json:"author,omitempty"`
	LastModifiedBy string `json:"last_modified_by,omitempty"`
	Company        string `json:"company,omitempty"`
	Application    string `json:"application,omitempty"`
}

func hasMarker(s string) string {
	l := strings.ToLower(s)
	for _, m := range platformMarkers {
		if strings.Contains(l, m) {
			return m
		}
	}
	return ""
}

// authorIssue 判断作者类字段：只能为空或等于投标人。
func authorIssue(field, value, bidder string) *Issue {
	v := strings.TrimSpace(value)
	if v == "" || v == strings.TrimSpace(bidder) {
		return nil
	}
	return &Issue{Kind: IssueMetadata, Detail: fmt.Sprintf("文档属性「%s」为 %q，应为投标人名称或留空", field, truncateRunes(v, 60))}
}

// xmlTexts 返回 XML 中各元素（按本地名）的文本内容，同名元素取第一个非空值。
func xmlTexts(raw []byte) map[string]string {
	out := map[string]string{}
	dec := xml.NewDecoder(bytes.NewReader(raw))
	var stack []string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			stack = append(stack, t.Name.Local)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			if len(stack) > 0 {
				if v := strings.TrimSpace(string(t)); v != "" && out[stack[len(stack)-1]] == "" {
					out[stack[len(stack)-1]] = v
				}
			}
		}
	}
	return out
}

// CheckDOCXMetadata 回读 DOCX 的文档属性与样式表：作者 / 最后修改者 / 公司只能是投标人或空；docProps 与 styles.xml 中不得
// 出现平台字样。
func CheckDOCXMetadata(data []byte, bidder string) (*Metadata, []Issue, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, nil, err
	}
	md := &Metadata{}
	issues := []Issue{}
	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, "docProps/") && f.Name != "word/styles.xml" {
			continue
		}
		raw, err := readZipFile(f)
		if err != nil {
			return nil, nil, err
		}
		if m := hasMarker(string(raw)); m != "" {
			issues = append(issues, Issue{Kind: IssueMetadata, Detail: fmt.Sprintf("%s 中出现平台字样 %q", f.Name, m)})
		}
		switch f.Name {
		case "docProps/core.xml":
			t := xmlTexts(raw)
			md.Title, md.Author, md.LastModifiedBy = t["title"], t["creator"], t["lastModifiedBy"]
		case "docProps/app.xml":
			t := xmlTexts(raw)
			md.Company, md.Application = t["Company"], t["Application"]
		}
	}
	for _, c := range []struct{ field, value string }{{"作者", md.Author}, {"最后修改者", md.LastModifiedBy}, {"公司", md.Company}} {
		if is := authorIssue(c.field, c.value, bidder); is != nil {
			issues = append(issues, *is)
		}
	}
	return md, issues, nil
}

// CheckPDFMetadata 回读 PDF 文档信息字典与 XMP：Author 只能是投标人或空；任何文本项与 XMP 中不得出现平台字样。
func CheckPDFMetadata(data []byte, bidder string) (*Metadata, []Issue, error) {
	info, xmp, err := ingest.ReadPDFInfo(data)
	if err != nil {
		return nil, nil, err
	}
	md := &Metadata{Title: info["Title"], Author: info["Author"], Application: strings.TrimSpace(info["Creator"] + " / " + info["Producer"])}
	issues := []Issue{}
	for k, v := range info {
		if m := hasMarker(v); m != "" {
			issues = append(issues, Issue{Kind: IssueMetadata, Detail: fmt.Sprintf("PDF 文档信息 %s 中出现平台字样 %q", k, m)})
		}
	}
	if m := hasMarker(xmp); m != "" {
		issues = append(issues, Issue{Kind: IssueMetadata, Detail: fmt.Sprintf("PDF XMP 元数据中出现平台字样 %q", m)})
	}
	if is := authorIssue("作者", md.Author, bidder); is != nil {
		issues = append(issues, *is)
	}
	return md, issues, nil
}
