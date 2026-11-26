package db

// D1 否决项与投标范围：
//   - 资格性 / 符合性审查表逐行转为否决项（veto_item）：表格前的标题或表头（「资格审查」「符合性审查」）判定类型，
//     表头须同时有审查因素列与审查内容列且没有分值列（投标文件格式中的承诺表、函件模板不算）；只引用其他章节的父行跳过，
//     序号与因素均为空的续行并入上一项；
//   - 87 号令第六十三条（投标无效情形）与第三十七条（视为串通投标）作为法定否决项，每个有招标原文的任务都会产出；
//   - 投标截止时间从「投标截止时间：2026年8月26日北京时间10时00分」类语句抽取；多份文件时后发布的更正 / 澄清优先，
//     人工指定的值不被重新提取覆盖；
//   - 本次投标分包（requirement_manifests.bid_lots）之外的要求标为 out_of_bid，不进入技术标大纲、覆盖率与否决项门禁。
// 工程回归见 eval/bid/README.md。业务召回率与准确率以 eval/bid/PROTOCOL.md 的独立金标准为准。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/autobid/autobid-engine/compliance"
)

// ReqVetoItem 是否决项类别：不在技术标中作答，由投标方逐项核对。
const ReqVetoItem = "veto_item"

// 否决项来源（requirement_items.veto_kind）与核对状态（veto_status）。
const (
	VetoQualification = "qualification"
	VetoConformity    = "conformity"
	VetoStatutory     = "statutory"

	VetoPending = "pending"
	VetoPassed  = "passed"
	VetoFailed  = "failed"

	BidDecisionPending = "pending"
	BidDecisionBid     = "bid"
	BidDecisionNoBid   = "no_bid"
)

func validVetoKind(k string) bool {
	switch k {
	case VetoQualification, VetoConformity, VetoStatutory:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// 审查表解析（纯规则）
// ---------------------------------------------------------------------------

var (
	reReviewConformity = regexp.MustCompile(`符合性(?:审查|检查)|初步审查`)
	reReviewQualify    = regexp.MustCompile(`资格(?:性)?(?:审查|检查)`)
	reReviewSeqHdr     = regexp.MustCompile(`^(?:序号|编号|条款号)$`)
	reReviewFactorHdr  = regexp.MustCompile(`因素|审查项|检查项|评审项`)
	reReviewFormatHdr  = regexp.MustCompile(`格式|证明材料|提供材料|证明文件`)
	reReviewCritHdr    = regexp.MustCompile(`内容|标准|要求|说明`)
	reReviewScoreHdr   = regexp.MustCompile(`分值|分数|得分|权重|满分`)
	reReviewParentRef  = regexp.MustCompile(`^(?:具体)?(?:规定|要求)?(?:详)?见|^如有[，,]?见`)
)

// reviewSchema 是审查表的列角色（-1 表示不存在）。
type reviewSchema struct {
	seq, group, factor, criteria, format int
}

// detectReviewSchema 识别表头列角色：两个因素列时前一列为类别；须同时有因素列与内容列，且不得有分值列。
func detectReviewSchema(header []string) (reviewSchema, bool) {
	s := reviewSchema{seq: -1, group: -1, factor: -1, criteria: -1, format: -1}
	var factors []int
	for i, cell := range header {
		h := normCell(cell)
		switch {
		case h == "":
		case reReviewScoreHdr.MatchString(h):
			return s, false
		case reReviewSeqHdr.MatchString(h):
			s.seq = i
		case reReviewFactorHdr.MatchString(h):
			factors = append(factors, i)
		case reReviewFormatHdr.MatchString(h):
			s.format = i
		case reReviewCritHdr.MatchString(h) && s.criteria < 0:
			s.criteria = i
		}
	}
	if len(factors) == 0 || s.criteria < 0 {
		return s, false
	}
	s.factor = factors[len(factors)-1]
	if len(factors) > 1 {
		s.group = factors[0]
	}
	return s, true
}

// reviewKindOf 由表格前的文字与表头判定审查表类型；符合性优先（「对符合资格的投标人进行符合性审查」）。
func reviewKindOf(context string, header []string) string {
	text := context + " " + strings.Join(header, " ")
	switch {
	case reReviewConformity.MatchString(text):
		return VetoConformity
	case reReviewQualify.MatchString(text):
		return VetoQualification
	}
	return ""
}

// ReviewItem 是从审查表解析出的一项否决项。
type ReviewItem struct {
	Kind     string `json:"kind"`
	Seq      string `json:"seq,omitempty"`
	Group    string `json:"group,omitempty"`
	Factor   string `json:"factor"`
	Criteria string `json:"criteria"`
	Format   string `json:"format,omitempty"`
	Rows     []int  `json:"rows"`
}

func cellAt(row []string, i int) string {
	if i < 0 || i >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[i])
}

// parseReviewTable 把一张审查表还原为否决项；kind 为空或表头不符时返回 nil。
func parseReviewTable(t *scoreTable, context string) []*ReviewItem {
	if len(t.Grid) < 2 {
		return nil
	}
	schema, ok := detectReviewSchema(t.Grid[0])
	if !ok {
		return nil
	}
	kind := reviewKindOf(context, t.Grid[0])
	if kind == "" {
		return nil
	}
	var items []*ReviewItem
	for r := 1; r < len(t.Grid); r++ {
		row := t.Grid[r]
		seq, factor, criteria := cellAt(row, schema.seq), cellAt(row, schema.factor), cellAt(row, schema.criteria)
		group, format := cellAt(row, schema.group), cellAt(row, schema.format)
		if normCell(factor) == normCell(t.Grid[0][schema.factor]) && normCell(criteria) == normCell(t.Grid[0][schema.criteria]) {
			continue // 跨页重复的表头
		}
		if seq == "" && factor == "" {
			if criteria != "" && len(items) > 0 { // 续行：并入上一项
				last := items[len(items)-1]
				last.Criteria = strings.TrimSpace(last.Criteria + "\n" + criteria)
				last.Rows = append(last.Rows, r)
			}
			continue
		}
		if factor == "" {
			factor = group
		}
		if factor == "" && criteria == "" {
			continue
		}
		items = append(items, &ReviewItem{Kind: kind, Seq: seq, Group: group, Factor: factor, Criteria: criteria, Format: format, Rows: []int{r}})
	}
	// 只引用其他章节且有子项（「1」后跟「1-1」）的父行不单列。
	var out []*ReviewItem
	for i, it := range items {
		parent := false
		if it.Seq != "" {
			for _, next := range items[i+1:] {
				if strings.HasPrefix(next.Seq, it.Seq+"-") || strings.HasPrefix(next.Seq, it.Seq+".") {
					parent = true
					break
				}
			}
		}
		if parent && (it.Criteria == "" || reReviewParentRef.MatchString(normCell(it.Criteria))) {
			continue
		}
		out = append(out, it)
	}
	return out
}

func stripNumbering(s string) string {
	return strings.TrimSpace(reNumbering.ReplaceAllString(strings.TrimSpace(s), ""))
}

// title 是「【资格】因素」；同一表中因素名重复时（如符合性审查的「技术部分 / 商务部分」都叫「投标文件内容」）前置类别。
func (it *ReviewItem) title(repeated bool) string {
	label := map[string]string{VetoQualification: "资格", VetoConformity: "符合性"}[it.Kind]
	name := stripNumbering(it.Factor)
	if repeated && it.Group != "" && normCell(it.Group) != normCell(it.Factor) {
		name = stripNumbering(it.Group) + "·" + name
	}
	return firstLine("【"+label+"】"+name, 60)
}

// singleLotMarker 返回文本中唯一出现的分包标识；没有或出现多个分包时返回空串。
// 审查表通常对全部分包适用，只有紧邻表格的文字只点名一个分包时才归入该分包，避免把全项目否决项误归到某个分包。
func singleLotMarker(text string) string {
	lots := map[string]bool{}
	for _, re := range []*regexp.Regexp{reLotBefore, reLotAfter} {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			lots[normalizeLot(m[1])] = true
		}
	}
	if len(lots) != 1 {
		return ""
	}
	for l := range lots {
		return l
	}
	return ""
}

func (it *ReviewItem) description() string {
	var b strings.Builder
	if it.Group != "" && normCell(it.Group) != normCell(it.Factor) {
		b.WriteString("审查类别：" + it.Group + "\n")
	}
	b.WriteString("审查因素：" + stripNumbering(it.Factor) + "\n")
	if c := strings.TrimSpace(it.Criteria); c != "" && c != "/" {
		b.WriteString("审查内容：" + c + "\n")
	}
	if f := strings.TrimSpace(it.Format); f != "" && f != "/" {
		b.WriteString("格式要求：" + f + "\n")
	}
	return strings.TrimSpace(b.String())
}

// reviewCandidates 按原文顺序解析全部审查表，返回否决项候选与含审查表的分块。
// 分包只取紧邻表格的文字（章节标题与表前 160 字）中唯一点名的分包，不沿用前文出现过的分包标识。
func reviewCandidates(order []*chunkSource) ([]reqCandidate, map[int64]bool) {
	var (
		out    []reqCandidate
		chunks = map[int64]bool{}
	)
	for _, cs := range order {
		var tables []scoreTable
		if len(cs.Tables) <= 2 || json.Unmarshal(cs.Tables, &tables) != nil || len(tables) == 0 {
			continue
		}
		pos := 0
		for ti := range tables {
			t := &tables[ti]
			if len(t.Grid) == 0 {
				continue
			}
			context := cs.SectionCode + " " + cs.SectionTitle
			if at := strings.Index(cs.Content[pos:], t.Markdown); t.Markdown != "" && at >= 0 {
				between := cs.Content[pos : pos+at]
				if r := []rune(between); len(r) > 160 {
					between = string(r[len(r)-160:])
				}
				context += " " + between
				pos += at + len(t.Markdown)
			}
			items := parseReviewTable(t, context)
			if len(items) == 0 {
				continue
			}
			chunks[cs.ChunkID] = true
			lot := singleLotMarker(context)
			factors := map[string]int{}
			for _, it := range items {
				factors[normCell(it.Factor)]++
			}
			docKey := chunkKey(cs)
			for k, it := range items {
				prefix := map[string]string{VetoQualification: "VQ", VetoConformity: "VC"}[it.Kind]
				loc := cloneLoc(cs.Loc, "table", ti, "row", it.Rows[0], "rows", it.Rows, "lot", lot, "review_kind", it.Kind)
				out = append(out, reqCandidate{code: fmt.Sprintf("%s-%s-T%d-%02d", prefix, docKey, ti+1, k+1), category: ReqVetoItem,
					title: it.title(factors[normCell(it.Factor)] > 1), description: it.description(), mandatory: true, vetoKind: it.Kind, lot: lot,
					atomType: AtomVetoItem, proofRules: ParseProofRules(it.Factor+"\n"+it.Criteria+"\n"+it.Format, ReqVetoItem, ""),
					tableItem: true, src: cs, loc: loc})
			}
		}
	}
	return out, chunks
}

// ---------------------------------------------------------------------------
// 法定否决项（《政府采购货物和服务招标投标管理办法》，财政部令第 87 号）
// ---------------------------------------------------------------------------

type statutoryVeto struct {
	code, title, description string
	covers                   *regexp.Regexp // 审查表中对应同一要求的行
}

var statutoryVetoes = []statutoryVeto{
	{"VS-63-1", "【法定】按招标文件规定提交投标保证金", "87 号令第六十三条第（一）项：未按照招标文件的规定提交投标保证金的，投标无效。",
		regexp.MustCompile(`保证金|保函`)},
	{"VS-63-2", "【法定】投标文件按招标文件要求签署、盖章", "87 号令第六十三条第（二）项：投标文件未按招标文件要求签署、盖章的，投标无效。",
		regexp.MustCompile(`签署|盖章|签字|签章`)},
	{"VS-63-3", "【法定】具备招标文件规定的资格要求", "87 号令第六十三条第（三）项：不具备招标文件中规定的资格要求的，投标无效。",
		nil},
	{"VS-63-4", "【法定】报价不超过预算金额或最高限价", "87 号令第六十三条第（四）项：报价超过招标文件中规定的预算金额或者最高限价的，投标无效。",
		regexp.MustCompile(`预算金额|最高限价|控制价`)},
	{"VS-63-5", "【法定】投标文件不含采购人不能接受的附加条件", "87 号令第六十三条第（五）项：投标文件含有采购人不能接受的附加条件的，投标无效。",
		regexp.MustCompile(`附加条件`)},
	{"VS-37", "【法定】不存在视为串通投标的情形", "87 号令第三十七条：不同投标人的投标文件由同一单位或者个人编制、委托同一单位或者个人办理投标事宜、" +
		"载明的项目管理成员或者联系人员为同一人、异常一致或者投标报价呈规律性差异、相互混装、投标保证金从同一单位或者个人的账户转出的，" +
		"视为串通投标，投标无效。使用同一写标工具或模板时，须检查文件元数据与通用表述是否与他人异常一致。",
		regexp.MustCompile(`串通|异常一致|同一单位`)},
}

// statutoryCandidates 产出法定否决项；covered_by 记录审查表中对应同一要求的行，供人工核对时参照。
func statutoryCandidates(table []reqCandidate) []reqCandidate {
	out := make([]reqCandidate, 0, len(statutoryVetoes))
	for _, s := range statutoryVetoes {
		covered := []string{}
		for _, c := range table {
			if (s.covers == nil && c.vetoKind == VetoQualification) || (s.covers != nil && s.covers.MatchString(c.title+" "+c.description)) {
				covered = append(covered, c.code)
			}
		}
		out = append(out, reqCandidate{code: s.code, category: ReqVetoItem, title: s.title, description: s.description, mandatory: true,
			vetoKind: VetoStatutory, atomType: AtomVetoItem, proofRules: ParseProofRules(s.title+"\n"+s.description, ReqVetoItem, ""),
			loc: map[string]any{"statute": "财政部令第87号", "covered_by": covered}})
	}
	return out
}

// ---------------------------------------------------------------------------
// 投标截止时间
// ---------------------------------------------------------------------------

var (
	reDeadlineCue        = regexp.MustCompile(`(?:投标|递交投标文件|提交投标文件|响应文件|投标文件递交|投标文件提交)[^。；;，,\n]{0,8}截止`)
	reDeadlineSkip       = regexp.MustCompile(`保证金|保函|获取|售卖|购买|质疑|澄清|答疑|踏勘`)
	reDeadlineSection    = regexp.MustCompile(`^(?:(?:[一二三四五六七八九十]+|\d+)[、.．])?(?:投标截止时间|(?:提交|递交)投标文件截止时间|投标文件(?:提交|递交)(?:截止时间)?)(?:(?:、|及|和|与)开标时间)?(?:(?:及|和|与)地点)?[:：]?$`)
	reDeadlineValueLabel = regexp.MustCompile(`^(?:\d+(?:[.．]\d+)*[、.．]?\s*)?(?:截止时间|时间)\s*[:：]\s*`)
	reDateTime           = regexp.MustCompile(`(\d{4})\s*[年\-/.]\s*(\d{1,2})\s*[月\-/.]\s*(\d{1,2})\s*日?\s*(?:[（(]?北京时间[）)]?)?\s*(上午|下午)?\s*(?:(\d{1,2})\s*[时点:：]\s*(\d{1,2})?)?`)
	beijing              = time.FixedZone("CST", 8*3600)
)

// ParseBidDeadline 从一行文字中解析投标截止时间（北京时间）；没有截止语义或没有完整日期时 ok 为 false。
func ParseBidDeadline(line string) (time.Time, bool) {
	cue := reDeadlineCue.FindStringIndex(line)
	if cue == nil || reDeadlineSkip.MatchString(line[:cue[1]]) {
		return time.Time{}, false
	}
	return parseBidDateTime(line[cue[0]:], false)
}

func parseBidDateTime(value string, leading bool) (time.Time, bool) {
	value = strings.TrimSpace(value)
	m := reDateTime.FindStringSubmatch(value)
	if m == nil || leading && (m[5] == "" || !strings.HasPrefix(value, m[0])) {
		return time.Time{}, false
	}
	num := func(s string) int { n, _ := strconv.Atoi(s); return n }
	year, month, day, hour, minute := num(m[1]), num(m[2]), num(m[3]), num(m[5]), num(m[6])
	if m[4] == "下午" && hour < 12 {
		hour += 12
	}
	if month < 1 || month > 12 || day < 1 || day > 31 || hour > 23 || minute > 59 {
		return time.Time{}, false
	}
	at := time.Date(year, time.Month(month), day, hour, minute, 0, 0, beijing)
	return at, at.Day() == day
}

type bidDeadlineMatch struct {
	at    time.Time
	chunk *chunkSource
	text  string
	page  any
}

// findChunkBidDeadline reads an explicit deadline label and its immediate value.
// A heading may end one chunk and its first numbered field start the next.
func findChunkBidDeadline(cs, next *chunkSource) (bidDeadlineMatch, bool) {
	lines := strings.Split(cs.Content, "\n")
	for i, line := range lines {
		if at, ok := ParseBidDeadline(line); ok {
			return bidDeadlineMatch{at, cs, line, cs.Loc["page_start"]}, true
		}
		if !reDeadlineSection.MatchString(normCell(line)) {
			continue
		}
		valueChunk := cs
		value := strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
		if value == "" && next != nil && next.DocID == cs.DocID {
			valueChunk, value = next, strings.TrimSpace(next.Content)
		}
		value, _, _ = strings.Cut(value, "\n")
		value = strings.TrimSpace(value)
		label := reDeadlineValueLabel.FindString(value)
		if valueChunk != cs && label == "" {
			continue
		}
		candidate := strings.TrimPrefix(value, label)
		if at, ok := parseBidDateTime(candidate, true); ok {
			return bidDeadlineMatch{at, valueChunk, strings.TrimSpace(line) + " " + value, valueChunk.Loc["page_start"]}, true
		}
	}
	return findScheduleBidDeadline(cs)
}

func findScheduleBidDeadline(cs *chunkSource) (bidDeadlineMatch, bool) {
	if !strings.Contains(cs.Content, "截止时间") && !strings.Contains(cs.Content, "结束时间") {
		return bidDeadlineMatch{}, false
	}
	var tables []scoreTable
	if json.Unmarshal(cs.Tables, &tables) != nil {
		return bidDeadlineMatch{}, false
	}
	for _, table := range tables {
		stepCol, endCol := -1, -1
		for rowIndex, row := range table.Grid {
			if stepCol < 0 || endCol < 0 {
				stepCol, endCol = -1, -1
				for col, cell := range row {
					switch normCell(cell) {
					case "步骤名称", "步骤":
						stepCol = col
					case "截止时间", "结束时间":
						endCol = col
					}
				}
				continue
			}
			if max(stepCol, endCol) >= len(row) {
				continue
			}
			switch normCell(row[stepCol]) {
			case "网上应答（上传电子投标文件）", "网上应答(上传电子投标文件)", "上传电子投标文件", "提交电子投标文件":
				if at, ok := parseBidDateTime(row[endCol], true); ok {
					page := cs.Loc["page_start"]
					if table.PageStart > 0 {
						page = table.PageStart
					}
					for _, cell := range table.Cells {
						if cell.Row == rowIndex && cell.Col == endCol && cell.Page > 0 {
							page = cell.Page
							break
						}
					}
					return bidDeadlineMatch{at, cs, row[stepCol] + " 截止时间：" + row[endCol], page}, true
				}
			}
		}
	}
	return bidDeadlineMatch{}, false
}

type deadlineDoc struct {
	id        int64
	role      string
	published sql.NullTime
}

// laterDoc 报告 a 的效力是否在 b 之后：都有发布时间时按发布时间，否则更正 / 澄清 / 答疑在原招标文件之后，再按上传顺序。
func laterDoc(a, b deadlineDoc) bool {
	if a.published.Valid && b.published.Valid && !a.published.Time.Equal(b.published.Time) {
		return a.published.Time.After(b.published.Time)
	}
	if ra, rb := a.role != DocRoleTender, b.role != DocRoleTender; ra != rb {
		return ra
	}
	return a.id > b.id
}

// extractBidDeadlineTx 返回效力最后的文件中第一处投标截止时间及其来源；没有时返回 nil。
func extractBidDeadlineTx(ctx context.Context, q queryer, tenant string, taskID int64, order []*chunkSource) (*time.Time, map[string]any, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, doc_role, published_at FROM tender_documents WHERE tenant_id = $1 AND task_id = $2`, tenant, taskID)
	if err != nil {
		return nil, nil, err
	}
	docs := map[int64]deadlineDoc{}
	for rows.Next() {
		var d deadlineDoc
		if err := rows.Scan(&d.id, &d.role, &d.published); err != nil {
			rows.Close()
			return nil, nil, err
		}
		docs[d.id] = d
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var (
		best    *deadlineDoc
		bestAt  time.Time
		bestSrc map[string]any
	)
	for i, cs := range order {
		d, ok := docs[cs.DocID]
		if !ok || (best != nil && (best.id == d.id || !laterDoc(d, *best))) {
			continue
		}
		var next *chunkSource
		if i+1 < len(order) {
			next = order[i+1]
		}
		if match, ok := findChunkBidDeadline(cs, next); ok {
			best, bestAt = &d, match.at
			bestSrc = map[string]any{"origin": "extracted", "doc_id": d.id, "chunk_id": match.chunk.ChunkID, "doc_role": d.role,
				"page": match.page, "text": firstLine(match.text, 120)}
		}
	}
	if best == nil {
		return nil, nil, nil
	}
	return &bestAt, bestSrc, nil
}

// ---------------------------------------------------------------------------
// 投标范围：分包与截止时间
// ---------------------------------------------------------------------------

func normalizeLots(lots []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, l := range lots {
		n := lastLotMarker(l)
		if n == "" {
			n = normalizeLot(l)
		}
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// outOfBid 报告要求是否在本次投标分包之外（未选分包或要求不分包时都在范围内）。
func outOfBid(lot string, bidLots []string) bool {
	if lot == "" || len(bidLots) == 0 {
		return false
	}
	for _, l := range bidLots {
		if l == lot {
			return false
		}
	}
	return true
}

func bidLotsTx(ctx context.Context, q queryer, tenant string, taskID int64) ([]string, error) {
	var raw []byte
	err := q.QueryRowContext(ctx, `SELECT to_jsonb(bid_lots) FROM requirement_manifests WHERE tenant_id = $1 AND task_id = $2`, tenant, taskID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	var lots []string
	_ = json.Unmarshal(raw, &lots)
	return lots, err
}

// BidScopeInput 设置本次投标分包、人工指定的投标截止时间与投/不投决策；nil 字段保持不变。
// Deadline 为空串时清除人工值，下次提取重新从原文抽取；否则为 RFC 3339 或「2006-01-02 15:04」（北京时间）。
type BidScopeInput struct {
	Lots         *[]string `json:"bid_lots"`
	Deadline     *string   `json:"bid_deadline"`
	Decision     *string   `json:"bid_decision"`
	DecisionNote *string   `json:"bid_decision_note"`
}

// SetBidScope 更新投标范围与投/不投决策。改变分包会使相关要求的 out_of_bid 变化，清单 epoch 递增、审批中 / 已批准大纲失效并派生新草案。
func (d *DB) SetBidScope(ctx context.Context, tenant string, taskID int64, actor string, in BidScopeInput) (*RequirementManifest, error) {
	tenant = tenantOr(tenant)
	var out *RequirementManifest
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		before, err := lockActiveOutlineTx(ctx, tx, tenant, taskID)
		if err != nil {
			return err
		}
		if _, err := ensureManifestTx(ctx, tx, tenant, taskID, true); err != nil {
			return err
		}
		if in.Lots != nil {
			lots := normalizeLots(*in.Lots)
			var known []string
			rows, err := tx.QueryContext(ctx, `SELECT DISTINCT lot FROM requirement_items WHERE tenant_id = $1 AND task_id = $2 AND lot <> '' ORDER BY lot`,
				tenant, taskID)
			if err != nil {
				return err
			}
			for rows.Next() {
				var l string
				if err := rows.Scan(&l); err != nil {
					rows.Close()
					return err
				}
				known = append(known, l)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			for _, l := range lots {
				if !containsString(known, l) {
					return fmt.Errorf("%w: lot %q does not appear in the requirement list (lots: %s)", ErrInvalidRequirement, l, strings.Join(known, ", "))
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE requirement_manifests SET bid_lots = $3::text[], updated_by = $4 WHERE tenant_id = $1 AND task_id = $2`,
				tenant, taskID, pgTextArray(lots), actor); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE requirement_items SET out_of_bid = NOT out_of_bid, updated_by = $4
				WHERE tenant_id = $1 AND task_id = $2
				  AND out_of_bid <> (lot <> '' AND cardinality($3::text[]) > 0 AND NOT (lot = ANY($3::text[])))`,
				tenant, taskID, pgTextArray(lots), actor); err != nil {
				return err
			}
		}
		if in.Deadline != nil {
			raw := strings.TrimSpace(*in.Deadline)
			if raw == "" {
				if _, err := tx.ExecContext(ctx, `UPDATE requirement_manifests SET bid_deadline = NULL, bid_deadline_source = '{}', updated_by = $3
					WHERE tenant_id = $1 AND task_id = $2`, tenant, taskID, actor); err != nil {
					return err
				}
			} else {
				at, err := time.Parse(time.RFC3339, raw)
				if err != nil {
					if at, err = time.ParseInLocation("2006-01-02 15:04", raw, beijing); err != nil {
						return fmt.Errorf("%w: bid_deadline must be RFC 3339 or \"2006-01-02 15:04\"", ErrInvalidRequirement)
					}
				}
				src, _ := json.Marshal(map[string]any{"origin": "manual", "by": actor, "at": time.Now().UTC()})
				if _, err := tx.ExecContext(ctx, `UPDATE requirement_manifests SET bid_deadline = $3, bid_deadline_source = $4::jsonb, updated_by = $5
					WHERE tenant_id = $1 AND task_id = $2`, tenant, taskID, at, string(src), actor); err != nil {
					return err
				}
			}
		}
		if in.Decision != nil {
			dec := strings.TrimSpace(*in.Decision)
			note := ""
			if in.DecisionNote != nil {
				note = strings.TrimSpace(*in.DecisionNote)
			}
			switch dec {
			case BidDecisionPending:
				if _, err := tx.ExecContext(ctx, `UPDATE requirement_manifests SET bid_decision = 'pending', bid_decision_note = $3,
					bid_decided_by = '', bid_decided_at = NULL, updated_by = $4 WHERE tenant_id = $1 AND task_id = $2`,
					tenant, taskID, note, actor); err != nil {
					return err
				}
			case BidDecisionBid:
				var failed int
				if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM requirement_items
					WHERE tenant_id = $1 AND task_id = $2 AND category = 'veto_item' AND NOT out_of_bid
					  AND superseded_by_id IS NULL AND review_status <> 'stale' AND veto_status = 'failed'`,
					tenant, taskID).Scan(&failed); err != nil {
					return err
				}
				if failed > 0 {
					return fmt.Errorf("%w: cannot decide to bid while %d in-scope veto item(s) failed", ErrInvalidRequirement, failed)
				}
				if _, err := tx.ExecContext(ctx, `UPDATE requirement_manifests SET bid_decision = 'bid', bid_decision_note = $3,
					bid_decided_by = $4, bid_decided_at = now(), updated_by = $4 WHERE tenant_id = $1 AND task_id = $2`,
					tenant, taskID, note, actor); err != nil {
					return err
				}
			case BidDecisionNoBid:
				if note == "" {
					return fmt.Errorf("%w: bid_decision_note is required when decision is no_bid", ErrInvalidRequirement)
				}
				if _, err := tx.ExecContext(ctx, `UPDATE requirement_manifests SET bid_decision = 'no_bid', bid_decision_note = $3,
					bid_decided_by = $4, bid_decided_at = now(), updated_by = $4 WHERE tenant_id = $1 AND task_id = $2`,
					tenant, taskID, note, actor); err != nil {
					return err
				}
			default:
				return fmt.Errorf("%w: invalid bid_decision %q", ErrInvalidRequirement, dec)
			}
		} else if in.DecisionNote != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE requirement_manifests SET bid_decision_note = $3, updated_by = $4
				WHERE tenant_id = $1 AND task_id = $2`, tenant, taskID, strings.TrimSpace(*in.DecisionNote), actor); err != nil {
				return err
			}
		}
		if err := afterRequirementChangeTx(ctx, tx, tenant, taskID, actor, before); err != nil {
			return err
		}
		out, err = ensureManifestTx(ctx, tx, tenant, taskID, false)
		return err
	})
	return out, err
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// SetVetoStatus 记录投标方对一个否决项的核对结论。通过与不通过都必须写明依据（证明材料位置或不满足的原因），
// 否决项内容变化后结论自动退回 pending（触发器 trg_requirement_veto_reset）。
func (d *DB) SetVetoStatus(ctx context.Context, tenant string, taskID, id int64, actor, status, note string) (*RequirementItem, error) {
	note = strings.TrimSpace(note)
	switch status {
	case VetoPending:
	case VetoPassed, VetoFailed:
		if note == "" {
			return nil, fmt.Errorf("%w: a note naming the evidence or the unmet condition is required", ErrInvalidRequirement)
		}
	default:
		return nil, fmt.Errorf("%w: invalid veto status %q", ErrInvalidRequirement, status)
	}
	if utf8.RuneCountInString(note) > 2000 {
		return nil, fmt.Errorf("%w: note is too long", ErrInvalidRequirement)
	}
	return scanRequirement(d.QueryRowContext(ctx, `UPDATE requirement_items SET veto_status = $4, veto_note = $5, veto_checked_by = $6,
		veto_checked_at = CASE WHEN $4 = 'pending' THEN NULL ELSE now() END, updated_by = $6
		WHERE tenant_id = $1 AND task_id = $2 AND id = $3 AND category = 'veto_item' RETURNING `+reqCols,
		tenantOr(tenant), taskID, id, status, note, actor))
}

// ---------------------------------------------------------------------------
// 投 / 不投评估（D1：否决项一票否决 + 客观分/主观分/价格分 + 证据支撑度）
// ---------------------------------------------------------------------------

// BidEvaluation 汇总投标范围与否决项核对后的投/不投评估指标。
type BidEvaluation struct {
	Decision             string     `json:"decision"`
	DecisionNote         string     `json:"decision_note"`
	DecidedBy            string     `json:"decided_by"`
	DecidedAt            *time.Time `json:"decided_at,omitempty"`
	VetoGate             string     `json:"veto_gate"` // passed / pending / blocked
	TotalScore           float64    `json:"total_score"`
	PriceScore           float64    `json:"price_score"`
	PriceScoreUnresolved bool       `json:"price_score_unresolved,omitempty"`
	ObjectiveScore       float64    `json:"objective_score"`
	ObjectiveScoreLower  float64    `json:"objective_score_lower"`
	ObjectiveScoreUpper  float64    `json:"objective_score_upper"`
	SubjectiveScore      float64    `json:"subjective_score"`
	EvidenceBackedScore  float64    `json:"evidence_backed_score"`
	MandatoryCount       int        `json:"mandatory_count"`
	MandatoryWithEv      int        `json:"mandatory_with_evidence"`
	SupersededCount      int        `json:"superseded_count"`
	Recommendation       string     `json:"recommendation"` // bid / caution / no_bid
	Gaps                 []string   `json:"gaps"`
}

func evaluateBidReadiness(m *RequirementManifest, items []*RequirementItem, summaries []ScoringSummary, evidence map[int64]bidEvidenceResult) BidEvaluation {
	ev := BidEvaluation{
		Decision: BidDecisionPending,
		VetoGate: "passed",
		Gaps:     []string{},
	}
	if m != nil {
		if m.BidDecision != "" {
			ev.Decision = m.BidDecision
		}
		ev.DecisionNote = m.BidDecisionNote
		ev.DecidedBy = m.BidDecidedBy
		ev.DecidedAt = m.BidDecidedAt
	}
	var vetoPending, vetoFailed int
	var scoreNeedsReview bool
	for _, it := range items {
		if it.IsSuperseded() {
			ev.SupersededCount++
			continue
		}
		if it.OutOfBid || it.ParentID != nil {
			continue
		}
		if it.Category == ReqVetoItem {
			switch it.VetoStatus {
			case VetoFailed:
				vetoFailed++
			case VetoPassed:
			default:
				vetoPending++
			}
			continue
		}
		if it.Category == ReqScoringItem {
			loc := scoringSource(it)
			if loc.ScoringOrphan && it.ReviewStatus == ReviewNeedsRevision {
				continue
			}
			result := evidence[it.ID]
			ev.TotalScore = round1(ev.TotalScore + it.ScoreWeight)
			switch {
			case it.ScoreGroup == ScoreGroupPrice:
				ev.PriceScore = round1(ev.PriceScore + it.ScoreWeight)
				if it.ScoreWeight == 0 && len(loc.ScoreConflict) > 1 {
					ev.PriceScoreUnresolved = true
					scoreNeedsReview = true
				}
			case it.EvalKind == EvalObjective:
				ev.ObjectiveScore = round1(ev.ObjectiveScore + it.ScoreWeight)
				lower, upper := result.objectiveBounds(it)
				ev.ObjectiveScoreLower += lower
				ev.ObjectiveScoreUpper += upper
				if lower < it.ScoreWeight {
					scoreNeedsReview = true
					if len(ev.Gaps) < 6 {
						ev.Gaps = append(ev.Gaps, fmt.Sprintf("评分项 %s 已核实 %.4g 分，当前材料可支持上限 %.4g 分，原件满分 %.4g 分", it.Code, lower, upper, it.ScoreWeight))
					}
				}
			default:
				ev.SubjectiveScore = round1(ev.SubjectiveScore + it.ScoreWeight)
				if !it.ProofRules.IsZero() && result.verdict != compliance.Admissible {
					scoreNeedsReview = true
					if len(ev.Gaps) < 6 {
						ev.Gaps = append(ev.Gaps, fmt.Sprintf("评分项 %s（%.1f分）的证明材料待核对", it.Code, it.ScoreWeight))
					}
				}
			}
		}
		if it.IsMandatory {
			ev.MandatoryCount++
			if evidence[it.ID].verdict == compliance.Admissible || it.ProofRules.IsZero() && len(it.EvidenceIDs) > 0 {
				ev.MandatoryWithEv++
			}
		}
	}
	ev.ObjectiveScoreLower = round4(ev.ObjectiveScoreLower)
	ev.ObjectiveScoreUpper = round4(ev.ObjectiveScoreUpper)
	ev.EvidenceBackedScore = ev.ObjectiveScoreLower
	for _, summary := range summaries {
		if m != nil && len(m.BidLots) > 0 && summary.Lot != "" && !containsString(m.BidLots, summary.Lot) {
			continue
		}
		for _, group := range summary.Groups {
			if group.Declared > 0 && math.Abs(group.Declared-group.Extracted) > 0.05 {
				scoreNeedsReview = true
			}
		}
		ev.Gaps = append(ev.Gaps, summary.Warnings...)
	}
	switch {
	case vetoFailed > 0:
		ev.VetoGate = "blocked"
		ev.Gaps = append([]string{fmt.Sprintf("存在 %d 项不满足的否决项（一票否决）", vetoFailed)}, ev.Gaps...)
	case vetoPending > 0:
		ev.VetoGate = "pending"
		ev.Gaps = append([]string{fmt.Sprintf("尚有 %d 项否决项待核对", vetoPending)}, ev.Gaps...)
	}
	if ev.MandatoryCount > ev.MandatoryWithEv {
		ev.Gaps = append(ev.Gaps, fmt.Sprintf("实质性响应（★）条款共 %d 项，其中 %d 项尚未绑定核验证据", ev.MandatoryCount, ev.MandatoryCount-ev.MandatoryWithEv))
	}
	switch {
	case ev.VetoGate == "blocked":
		ev.Recommendation = "no_bid"
	case ev.VetoGate == "pending" || ev.MandatoryWithEv < ev.MandatoryCount || scoreNeedsReview:
		ev.Recommendation = "caution"
	default:
		ev.Recommendation = "bid"
	}
	return ev
}

// ---------------------------------------------------------------------------
// 后发文件（更正 / 澄清 / 答疑）逐条覆盖与采购需求关联
// ---------------------------------------------------------------------------

var (
	reAmendmentSplit  = regexp.MustCompile(`(?:现(?:更正|修改|变更|调整|澄清|补充)为|更正后[：:为]?|修改后[：:为]?|变更后[：:为]?|调整后[：:为]?|(?:更正|修改|变更|调整|澄清)为)[：:\s]*`)
	reTitleTagPrefix  = regexp.MustCompile(`^(?:【[^】]+】|[★▲※*]+|\d+(?:\.\d+)*\s*)+`)
	reSectionRefToken = regexp.MustCompile(`(?:第\s*)?(\d+(?:\.\d+){1,4})\s*(?:条|款|项|节|章)?`)
	reDomainKeyword   = regexp.MustCompile(`架构|安全|运维|性能|响应|培训|售后|接口|数据库|部署|测试|方案|实施|进度|质量|应急|信创|备份|迁移|监控|加密|高可用|容灾|功能|进度|验收|保修|巡检`)
)

// splitAmendmentText 拆分「原……现更正为……」语句，返回被更正的原文线索与更正后的新要求正文。
func splitAmendmentText(text string) (before, after string, ok bool) {
	loc := reAmendmentSplit.FindStringIndex(text)
	if loc == nil {
		return "", "", false
	}
	before = strings.TrimSpace(text[:loc[0]])
	after = strings.TrimSpace(text[loc[1]:])
	if before == "" || after == "" {
		return "", "", false
	}
	return before, after, true
}

func cleanReqTitle(s string) string {
	s = strings.TrimSpace(s)
	for {
		next := strings.TrimSpace(reTitleTagPrefix.ReplaceAllString(s, ""))
		if next == s {
			break
		}
		s = next
	}
	s = stripNumbering(s)
	return normCell(s)
}

func normSectionCode(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "第条款项章节. ")
	return normCell(s)
}

// resolveSupersedingCandidates 识别后发文件（更正/澄清/答疑或发布时间更晚的文件）对先发文件同一事项的覆盖关系，
// 将被覆盖的旧候选标上 supersededByCode，并让更正条目继承被覆盖条目的类别、评分分组与分包。
func resolveSupersedingCandidates(cands []reqCandidate) {
	for j := range cands {
		b := &cands[j]
		if b.src == nil {
			continue
		}
		bDoc := b.src.doc()
		isAmendmentDoc := bDoc.role != "" && bDoc.role != DocRoleTender
		if !isAmendmentDoc && b.amendBefore == "" && !bDoc.published.Valid {
			continue
		}
		bSec := normSectionCode(b.src.SectionCode)
		bTitle := cleanReqTitle(b.title)
		bBeforeNorm := normCell(b.amendBefore)
		bFullNorm := normCell(b.title + " " + b.description)
		refSecs := map[string]bool{}
		for _, m := range reSectionRefToken.FindAllStringSubmatch(b.amendBefore+" "+b.title+" "+b.description, -1) {
			refSecs[m[1]] = true
		}

		bestIdx, bestScore := -1, 0
		for i := range cands {
			if i == j {
				continue
			}
			a := &cands[i]
			if a.src == nil || a.src.DocID == b.src.DocID || a.supersededByCode != "" || a.vetoKind == VetoStatutory {
				continue
			}
			aDoc := a.src.doc()
			if !laterDoc(bDoc, aDoc) {
				continue
			}
			// 两份均为普通招标文件且无更正措辞时，须发布时间确实有先后才视为后发覆盖。
			if !isAmendmentDoc && b.amendBefore == "" &&
				!(aDoc.published.Valid && bDoc.published.Valid && bDoc.published.Time.After(aDoc.published.Time)) {
				continue
			}
			if a.lot != "" && b.lot != "" && a.lot != b.lot {
				continue
			}
			aSec := normSectionCode(a.src.SectionCode)
			aTitle := cleanReqTitle(a.title)
			score := 0
			if aSec != "" && len(aSec) >= 2 && (aSec == bSec || refSecs[aSec]) {
				score += 4
			}
			if aTitle != "" && utf8.RuneCountInString(aTitle) >= 2 {
				switch {
				case aTitle == bTitle:
					score += 5
				case bBeforeNorm != "" && strings.Contains(bBeforeNorm, aTitle):
					score += 4
				case (isAmendmentDoc || b.amendBefore != "") && utf8.RuneCountInString(aTitle) >= 3 && strings.Contains(bFullNorm, aTitle):
					score += 3
				}
			}
			if score > 0 && a.category == b.category {
				score++
			}
			if score > bestScore {
				bestScore, bestIdx = score, i
			}
		}
		if bestIdx < 0 || bestScore < 3 {
			continue
		}
		a := &cands[bestIdx]
		a.supersededByCode = b.code
		if a.loc == nil {
			a.loc = map[string]any{}
		}
		a.loc["superseded_by_code"] = b.code
		if b.loc == nil {
			b.loc = map[string]any{}
		}
		b.loc["supersedes_code"] = a.code
		if b.lot == "" && a.lot != "" {
			b.lot = a.lot
		}
		// 更正公告常以散文段落发布：若被更正项是评分项、★实质性条款或审查表否决项，继承其类别与评分拆解。
		if b.category == ReqNonScoringRequired && a.category != ReqNonScoringRequired {
			b.category = a.category
			b.mandatory = b.mandatory || a.mandatory
			b.vetoKind = a.vetoKind
		}
		if b.category == ReqScoringItem {
			if b.scoreGroup == "" {
				b.scoreGroup = a.scoreGroup
			}
			text := b.description
			if b.amendAfter != "" {
				text = b.amendAfter
			}
			if m := reScore.FindStringSubmatch(text); m != nil {
				if v, err := strconv.ParseFloat(m[1], 64); err == nil && v > 0 {
					b.score = v
				}
			}
			if b.score == 0 {
				b.score = a.score
			}
			if !strings.HasPrefix(b.title, "【") && strings.HasPrefix(a.title, "【") {
				b.title = a.title
			}
			b.scoringTiers, b.evalKind, b.scoreMin = ParseScoringTiers(text, b.score, b.scoreGroup)
			b.proofRules = ParseProofRules(b.title+"\n"+text, b.category, b.scoreGroup)
		} else if b.amendAfter != "" {
			b.proofRules = ParseProofRules(b.title+"\n"+b.amendAfter, b.category, b.scoreGroup)
		}
		b.atomType = DefaultAtomType(b.category, false)
	}
}

// resolveDemandLinks 为每个有效的非价格评分项关联同分包下的采购需求条目（★实质性条款或一般要求），
// 供大纲生成、章节上下文组装与合规核验联动引用。
func resolveDemandLinks(cands []reqCandidate) {
	type demandCand struct {
		code    string
		lot     string
		secCode string
		title   string
		kw      map[string]bool
	}
	var demands []demandCand
	for i := range cands {
		c := &cands[i]
		if c.supersededByCode != "" || (c.category != ReqMandatoryClause && c.category != ReqNonScoringRequired) {
			continue
		}
		sec := ""
		if c.src != nil {
			sec = normSectionCode(c.src.SectionCode)
		}
		kw := map[string]bool{}
		for _, m := range reDomainKeyword.FindAllString(c.title+" "+c.description, -1) {
			kw[m] = true
		}
		demands = append(demands, demandCand{
			code:    c.code,
			lot:     c.lot,
			secCode: sec,
			title:   cleanReqTitle(c.title),
			kw:      kw,
		})
	}
	if len(demands) == 0 {
		return
	}
	for i := range cands {
		s := &cands[i]
		if s.supersededByCode != "" || s.category != ReqScoringItem || s.scoreGroup == ScoreGroupPrice {
			continue
		}
		sTitle := cleanReqTitle(s.title)
		sText := s.title + " " + s.description
		sKW := map[string]bool{}
		for _, m := range reDomainKeyword.FindAllString(sText, -1) {
			sKW[m] = true
		}
		type scored struct {
			code  string
			score int
		}
		var matches []scored
		for _, d := range demands {
			if s.lot != "" && d.lot != "" && s.lot != d.lot {
				continue
			}
			sc := 0
			if d.secCode != "" && len(d.secCode) >= 2 && strings.Contains(sText, d.secCode) {
				sc += 5
			}
			if sTitle != "" && d.title != "" && utf8.RuneCountInString(sTitle) >= 2 && utf8.RuneCountInString(d.title) >= 2 {
				if strings.Contains(d.title, sTitle) || strings.Contains(sTitle, d.title) {
					sc += 4
				}
			}
			for k := range sKW {
				if d.kw[k] {
					sc += 2
				}
			}
			if sc >= 2 {
				matches = append(matches, scored{code: d.code, score: sc})
			}
		}
		sort.SliceStable(matches, func(a, b int) bool {
			if matches[a].score != matches[b].score {
				return matches[a].score > matches[b].score
			}
			return matches[a].code < matches[b].code
		})
		if len(matches) > 5 {
			matches = matches[:5]
		}
		s.linkedCodes = make([]string, len(matches))
		for k, m := range matches {
			s.linkedCodes[k] = m.code
		}
	}
}
