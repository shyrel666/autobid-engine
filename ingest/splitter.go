// Package ingest 把招标文件文本切分为章节/条款段落并写入 tender_documents / document_chunks，
// 同时幂等创建根 tender（及可选 lot）静态实体。
package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/autobid/autobid-engine/db"
)

// Section 是切分结果中的一个段落。Level：0 文首，1 章/篇，2 节或中文序号标题，
// 3+ 为数字编号（"1"=3、"1.1"=4 …），★ 条款取其所在层级 +1。
type Section struct {
	Level     int
	Code      string
	Title     string
	PageNo    int
	Content   string
	Mandatory bool
}

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
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	var (
		out   []Section
		cur   *Section
		body  []string
		page  = 1
		depth = 0
		// afterClause：上一个标题是正文条款（见 heading.clause）
		afterClause bool
		starSeq     = map[string]int{}
		parent      = map[int]string{} // 各层级最近编号，用于给无编号 ★ 条款生成编号
	)
	flush := func() {
		if cur == nil {
			if content := strings.TrimSpace(strings.Join(body, "\n")); content != "" {
				out = append(out, Section{Level: 0, Title: "文首", PageNo: 1, Content: content, Mandatory: MandatoryPattern.MatchString(content)})
			}
			body = nil
			return
		}
		cur.Content = strings.TrimSpace(strings.Join(body, "\n"))
		cur.Mandatory = cur.Mandatory || MandatoryPattern.MatchString(cur.Content)
		out = append(out, splitLong(*cur)...)
		cur, body = nil, nil
	}
	// 有换页符时以换页符计页、页码标记行只丢弃；否则把页码标记视为页脚，遇到即翻页。
	formFeeds := strings.Contains(text, "\f")
	for _, raw := range strings.Split(text, "\n") {
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
		h, ok := classify(trimmed, depth, afterClause)
		if !ok {
			body = append(body, line)
			page += pageBreaks
			continue
		}
		startPage := page
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
		cur = &Section{Level: h.level, Code: h.code, Title: h.title, PageNo: startPage, Mandatory: h.star}
		body = append(body, trimmed)
		page += pageBreaks
	}
	flush()
	return out
}

func nearestCode(parent map[int]string, level int) string {
	for l := level - 1; l >= 0; l-- {
		if c, ok := parent[l]; ok {
			return c
		}
	}
	return ""
}

// splitLong 把超长段落按空行/行边界续切，后续部分标题加“（续n）”。
func splitLong(s Section) []Section {
	if utf8.RuneCountInString(s.Content) <= MaxSectionRunes {
		return []Section{s}
	}
	var (
		parts []Section
		buf   []string
		size  int
	)
	emit := func() {
		if len(buf) == 0 {
			return
		}
		p := s
		p.Content = strings.TrimSpace(strings.Join(buf, "\n"))
		if len(parts) > 0 {
			p.Title = fmt.Sprintf("%s（续%d）", s.Title, len(parts))
		}
		p.Mandatory = s.Mandatory && (len(parts) == 0 || MandatoryPattern.MatchString(p.Content))
		parts = append(parts, p)
		buf, size = nil, 0
	}
	for _, line := range strings.Split(s.Content, "\n") {
		for _, piece := range hardWrap(line, MaxSectionRunes) {
			n := utf8.RuneCountInString(piece) + 1
			if size+n > MaxSectionRunes {
				emit()
			}
			buf = append(buf, piece)
			size += n
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

// ---------------------------------------------------------------------------
// 写库
// ---------------------------------------------------------------------------

type Input struct {
	TenderNo string
	LotNo    string
	Title    string
	Text     string
}

type Result struct {
	Document      *db.TenderDocument `json:"document"`
	Created       bool               `json:"created"`
	Chunks        int                `json:"chunks"`
	Mandatory     int                `json:"mandatory_chunks"`
	TenderAssetID int64              `json:"tender_asset_id"`
	LotAssetID    int64              `json:"lot_asset_id,omitempty"`
}

// Ingest 切分并写入一份招标文档，幂等创建根 tender（LotNo 非空时再建 lot 与 tender -contains-> lot 边）。
// 实体经任务 AssetStore 写入，因此会归集 task_ids 并自动补任务范围。相同原文重复上传不会重复切分。
func Ingest(ctx context.Context, pg *db.DB, as *db.AssetStore, in Input) (*Result, error) {
	in.TenderNo, in.LotNo = strings.TrimSpace(in.TenderNo), strings.TrimSpace(in.LotNo)
	if in.TenderNo == "" {
		return nil, errors.New("ingest: tender_no is required")
	}
	if strings.TrimSpace(in.Text) == "" {
		return nil, errors.New("ingest: text is empty")
	}
	if as == nil || as.TaskID() <= 0 {
		return nil, errors.New("ingest: a task-bound asset store is required")
	}
	if in.Title == "" {
		in.Title = firstLine(in.Text)
	}
	docRef := map[string]any{"doc_title": in.Title}
	res := &Result{}
	var err error
	res.TenderAssetID, _, err = as.UpsertAsset(ctx, &db.Asset{Type: db.AssetTender, TenderNo: in.TenderNo, Title: in.Title, SourceRef: mustJSON(docRef)}, 0)
	if err != nil {
		return nil, fmt.Errorf("ingest: upsert tender: %w", err)
	}
	if in.LotNo != "" {
		res.LotAssetID, _, err = as.UpsertAsset(ctx, &db.Asset{Type: db.AssetLot, TenderNo: in.TenderNo, LotNo: in.LotNo, Title: in.Title, SourceRef: mustJSON(docRef)}, 0)
		if err != nil {
			return nil, fmt.Errorf("ingest: upsert lot: %w", err)
		}
		if _, err := as.UpsertEntityEdge(ctx, res.TenderAssetID, db.EntityRelContains, res.LotAssetID, as.TaskID(), nil, 1, nil); err != nil {
			return nil, fmt.Errorf("ingest: link lot: %w", err)
		}
	}

	sections := Split(in.Text)
	chunks := make([]db.DocumentChunk, 0, len(sections))
	for i, s := range sections {
		chunks = append(chunks, db.DocumentChunk{Seq: i, Level: s.Level, SectionCode: s.Code, SectionTitle: s.Title,
			PageNo: s.PageNo, Content: s.Content, TokenEstimate: EstimateTokens(s.Content), IsMandatory: s.Mandatory})
		if s.Mandatory {
			res.Mandatory++
		}
	}
	docID, created, err := pg.InsertTenderDocument(ctx, &db.TenderDocument{TaskID: as.TaskID(), TenderNo: in.TenderNo,
		LotNo: in.LotNo, Title: in.Title, RawText: in.Text}, chunks)
	if err != nil {
		return nil, fmt.Errorf("ingest: %w", err)
	}
	res.Created = created
	if res.Document, err = pg.GetTenderDocument(ctx, as.TaskID(), docID); err != nil {
		return nil, err
	}
	res.Chunks = res.Document.ChunkCount
	if !created {
		res.Mandatory = 0
		outline, err := pg.DocumentOutline(ctx, as.TaskID(), docID, 100000)
		if err != nil {
			return nil, err
		}
		for _, c := range outline {
			if c.IsMandatory {
				res.Mandatory++
			}
		}
	}
	return res, nil
}

func firstLine(text string) string {
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(strings.ReplaceAll(l, "\f", "")); l != "" {
			return clauseTitle(l)
		}
	}
	return ""
}

func mustJSON(v map[string]any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}
