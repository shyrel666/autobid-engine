package db

// 评分表结构化解析（纯规则，不访问数据库）：把招标原文中的评分办法表格还原为逐个评分因素。
//   - 按表头识别列角色：序号 / 评分类别 / 评分因素 / 分值 / 评分标准 / 说明；两个“因素类”列时前一列为类别；
//   - 评分类别（「技术部分（50%）」「商务部分（13分）」）纵向合并或跨页后留空时沿用上一行，并解析其声明分值；
//   - 评分因素名取自因素列，因素列实为类别时取分值列标签（「基础服务20」「项目的认识（10分）」）或评分标准首行；
//   - 同一因素的多行（纵向合并的因素单元格、跨页续行、同名分项）合并为一项，分项分值相加，同一分值单元格只计一次；
//   - 按类别与因素名分为技术 / 商务 / 价格三组；履约考核表、资格 / 符合性审查表和只有类别合计的汇总表不产出评分项。
// 规则对真实招标文件的召回 / 准确率未经人工标注评测，提取结果须人工复核；分值合计校验用于提示漏提或重复。

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// 评分项分组（requirement_items.score_group）。空串表示未分类（人工新增或非评分项）。
const (
	ScoreGroupTechnical  = "technical"
	ScoreGroupCommercial = "commercial"
	ScoreGroupPrice      = "price"
)

func validScoreGroup(g string) bool {
	switch g {
	case "", ScoreGroupTechnical, ScoreGroupCommercial, ScoreGroupPrice:
		return true
	}
	return false
}

// InTechnicalScope 报告要求是否需要在技术标中作答：价格评分项由报价文件响应，否决项由投标方逐项核对，
// 不投的分包、已被更正覆盖的旧条款与拆分出的子档次不单列大纲映射。
func (r *RequirementItem) InTechnicalScope() bool {
	return !r.OutOfBid && !r.IsSuperseded() && r.ParentID == nil && inTechnicalScope(r.Category, r.ScoreGroup)
}

func inTechnicalScope(category, scoreGroup string) bool {
	return category != ReqVetoItem && !(category == ReqScoringItem && scoreGroup == ScoreGroupPrice)
}

// scoreTable 是 document_chunks.tables_json 中一张表格的解析视图。
type scoreTable struct {
	Rows      int         `json:"rows"`
	Cols      int         `json:"cols"`
	PageStart int         `json:"page_start"`
	Cells     []scoreCell `json:"cells"`
	Grid      [][]string  `json:"grid"`
	Markdown  string      `json:"markdown"`
}

type scoreCell struct {
	Row     int    `json:"row"`
	Col     int    `json:"col"`
	RowSpan int    `json:"row_span"`
	ColSpan int    `json:"col_span"`
	Text    string `json:"text"`
	Page    int    `json:"page"`
}

// origins 返回每个网格位置所属原始单元格下标（-1 表示无单元格）。
func (t *scoreTable) origins() [][]int {
	out := make([][]int, len(t.Grid))
	for r := range out {
		out[r] = make([]int, len(t.Grid[r]))
		for c := range out[r] {
			out[r][c] = -1
		}
	}
	for i, c := range t.Cells {
		for r := c.Row; r < c.Row+max(c.RowSpan, 1) && r < len(out); r++ {
			for cc := c.Col; cc < c.Col+max(c.ColSpan, 1) && cc < len(out[r]); cc++ {
				out[r][cc] = i
			}
		}
	}
	return out
}

// scoreSchema 是评分表的列角色（-1 表示不存在）；header 为表头行下标，-1 表示沿用前表表头的续表。
type scoreSchema struct {
	header                                    int
	cols                                      int
	seq, group, factor, score, criteria, note int
	groupAsFactor                             bool
}

// ScoredItem 是从评分表解析出的一个评分因素。
type ScoredItem struct {
	Lot           string    `json:"lot,omitempty"`
	Group         string    `json:"group,omitempty"`        // 评分类别原文（去掉声明分值），如「技术部分」
	GroupWeight   float64   `json:"group_weight,omitempty"` // 类别声明分值 / 权重（百分比按 100 分制折算）
	ScoreGroup    string    `json:"score_group"`
	Name          string    `json:"name"`
	Score         float64   `json:"score"`
	ScoreConflict []float64 `json:"score_conflict,omitempty"`
	Criteria      string    `json:"criteria"`
	Note          string    `json:"note,omitempty"`
	Mandatory     bool      `json:"mandatory,omitempty"`
	Rows          []int     `json:"rows"`
	Page          int       `json:"page,omitempty"`
}

var (
	reWS           = regexp.MustCompile(`\s+`)
	reAssessTable  = regexp.MustCompile(`考核|扣分标准|加分标准`)
	reReviewOnly   = regexp.MustCompile(`审查|资格|符合性`)
	reRangeScore   = regexp.MustCompile(`^(\d+(?:\.\d+)?)分?[-—–~～至](\d+(?:\.\d+)?)分?$`)
	reBareScore    = regexp.MustCompile(`^(\d+(?:\.\d+)?)分?(?:[（(][^）)]*[）)])?$`)
	reParenScore   = regexp.MustCompile(`^(.*?)[（(](?:满分|最高|分值)?[:：]?(\d+(?:\.\d+)?)分[）)]$`)
	reTailScore    = regexp.MustCompile(`^(.*?[^\d.\-（(])(\d+(?:\.\d+)?)分?$`)
	reGroupWeight  = regexp.MustCompile(`[（(]?\s*(\d+(?:\.\d+)?)\s*(%|％|分)\s*[）)]?\s*$`)
	reCritScore    = regexp.MustCompile(`(?:满分|最高得|最多得|最高|本项|分值)\s*[:：为]?\s*(\d+(?:\.\d+)?)\s*分|(\d+(?:\.\d+)?)\s*分为满分`)
	reNumbering    = regexp.MustCompile(`^(?:\d{1,2}[.、．)）]\s*|[（(]\d{1,2}[)）]\s*|[一二三四五六七八九十]{1,3}[、.．]\s*)`)
	reGroupLike    = regexp.MustCompile(`部分|[%％]|^(?:价格|报价|商务|技术|服务|资信|客观|主观)(?:分|评分)`)
	reSummaryName  = regexp.MustCompile(`^(?:价格|报价|商务|技术|服务|资信|投标报价)(?:部分|分|评分)?$`)
	reTotalRow     = regexp.MustCompile(`^(?:合计|总计|总分|小计)`)
	rePriceGroup   = regexp.MustCompile(`价格|报价`)
	reBizGroup     = regexp.MustCompile(`商务|资信|信誉`)
	reTechGroup    = regexp.MustCompile(`技术|服务|方案|质量|实施`)
	rePriceName    = regexp.MustCompile(`^(?:投标)?(?:报价|价格)|价格分|价格评审|评标价`)
	reBizName      = regexp.MustCompile(`业绩|资质|证书|认证|体系|信誉|财务|社保|纳税|荣誉|著作权|知识产权|商务|企业实力|投标人实力|中小企业|类似项目|合同`)
	reMandatoryMrk = regexp.MustCompile(`[★▲]`)
	reDatePrefix   = regexp.MustCompile(`^(?:自|从)?\d{4}\s*年`)
)

func normCell(s string) string { return reWS.ReplaceAllString(strings.TrimSpace(s), "") }

var (
	reHdrCriteria = regexp.MustCompile(`评分标准|评审标准|评标标准|评分细则|评审细则|评分说明|评分办法|具体要求|评分要求|评审要求`)
	reHdrFactor   = regexp.MustCompile(`因素|评审项|评分项|评审内容|评分内容|考评项|^项目$|^内容$`)
	reHdrScore    = regexp.MustCompile(`分值|分数|满分|标准分|最高分|^得分$|^权重$|^权值$`)
	reHdrGroup    = regexp.MustCompile(`类别|类型|部分`)
	reHdrWeakCrit = regexp.MustCompile(`标准|细则|说明|要求|办法`)
)

// headerRole 判定表头单元格的列角色。「评分标准说明（…★代表实质性指标…）」这类长表头先按评分标准识别。
func headerRole(h string) string {
	h = normCell(h)
	switch {
	case h == "":
		return ""
	case strings.Contains(h, "序号"):
		return "seq"
	case strings.Contains(h, "备注") || strings.Contains(h, "证明材料") || strings.Contains(h, "佐证"):
		return "note"
	case reHdrCriteria.MatchString(h):
		return "criteria"
	case reHdrFactor.MatchString(h):
		return "factor"
	case reHdrScore.MatchString(h):
		return "score"
	case reHdrGroup.MatchString(h):
		return "group"
	case reHdrWeakCrit.MatchString(h):
		return "criteria"
	}
	return ""
}

// detectScoreSchema 在前 4 行中查找评分表表头。履约考核表与无分值的审查表返回 ok=false。
// 横向合并的表头（如两列都是「评审因素」）按两列处理：前一列为评分类别，后一列为评分因素。
func detectScoreSchema(t *scoreTable) (scoreSchema, bool) {
	if scoreHeader(firstRow(t.Grid), "评审分项", "分项分值", "评审因素") {
		return scoreSchema{header: 0, cols: 3, seq: -1, group: 0, factor: -1, score: 1, criteria: 2, note: -1, groupAsFactor: true}, true
	}
	for r := 0; r < len(t.Grid) && r < 4; r++ {
		row := t.Grid[r]
		joined := normCell(strings.Join(row, "|"))
		if reAssessTable.MatchString(joined) {
			return scoreSchema{}, false
		}
		s := scoreSchema{header: r, cols: len(row), seq: -1, group: -1, factor: -1, score: -1, criteria: -1, note: -1}
		for c, h := range row {
			switch headerRole(h) {
			case "seq":
				if s.seq < 0 {
					s.seq = c
				}
			case "note":
				if s.note < 0 {
					s.note = c
				}
			case "factor":
				switch {
				case s.factor < 0:
					s.factor = c
				case s.group < 0 && s.factor < c:
					s.group, s.factor = s.factor, c // 两个因素类列：前一列为评分类别
				}
			case "score":
				if s.score < 0 {
					s.score = c
				}
			case "group":
				if s.group < 0 {
					s.group = c
				}
			case "criteria":
				if s.criteria < 0 {
					s.criteria = c
				} else if s.note < 0 {
					s.note = c
				}
			}
		}
		if s.factor < 0 && s.criteria < 0 {
			continue
		}
		if s.score < 0 && !strings.Contains(joined, "评分") && !strings.Contains(joined, "评审") && !strings.Contains(joined, "评标") {
			continue
		}
		if s.score < 0 && reReviewOnly.MatchString(joined) {
			return scoreSchema{}, false
		}
		return s, true
	}
	return scoreSchema{}, false
}

// parseScoreText 解析分值单元格，返回标签（如「基础服务」）与分值；无法解析时 ok=false。
func parseScoreText(s string) (label string, score float64, ok bool) {
	s = normCell(s)
	if s == "" {
		return "", 0, false
	}
	num := func(x string) float64 { v, _ := strconv.ParseFloat(x, 64); return v }
	if m := reRangeScore.FindStringSubmatch(s); m != nil {
		return "", math.Max(num(m[1]), num(m[2])), true
	}
	if m := reBareScore.FindStringSubmatch(s); m != nil {
		return "", num(m[1]), true
	}
	if m := reParenScore.FindStringSubmatch(s); m != nil {
		return strings.TrimSpace(m[1]), num(m[2]), true
	}
	if m := reTailScore.FindStringSubmatch(s); m != nil && utf8.RuneCountInString(m[1]) <= 20 {
		return strings.TrimSpace(m[1]), num(m[2]), true
	}
	return "", 0, false
}

// splitGroup 把类别单元格拆为类别名与声明分值（「技术部分（50%）」→ 技术部分, 50）。
func splitGroup(s string) (string, float64) {
	s = normCell(s)
	w := 0.0
	if m := reGroupWeight.FindStringSubmatch(s); m != nil {
		w, _ = strconv.ParseFloat(m[1], 64)
		s = s[:len(s)-len(m[0])]
	}
	return strings.Trim(s, ":："), w
}

// nameFromCriteria 在没有因素名时取评分标准首行作名称（「1. 项目理解⏎投标人…」→ 项目理解）。
func nameFromCriteria(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "\n"); i > 0 {
		s = s[:i]
	}
	s = reNumbering.ReplaceAllString(normCell(s), "")
	if i := strings.IndexAny(s, "：:，,。；;（("); i > 0 && utf8.RuneCountInString(s[:i]) >= 2 {
		// 以时间范围开头的短前缀（「2023年1月1日以来」）不足以表达评分点，延伸到下一个标点
		if reDatePrefix.MatchString(s[:i]) {
			if j := strings.IndexAny(s[i+1:], "：:，,。；;（("); j > 0 {
				i += 1 + j
			} else {
				i = len(s)
			}
		}
		s = s[:i]
	}
	return firstLine(s, 30)
}

// cleanName 去掉编号、括注分值与尾部标点，过长时截取到首个标点。
func cleanName(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\n"); i > 0 {
		s = s[:i]
	}
	s = normCell(s)
	s = reNumbering.ReplaceAllString(s, "")
	if m := reParenScore.FindStringSubmatch(s); m != nil {
		s = m[1]
	}
	s = strings.TrimRight(s, "：:，,。；;")
	if utf8.RuneCountInString(s) > 30 {
		if i := strings.IndexAny(s, "：:，,。；;（("); i > 0 && utf8.RuneCountInString(s[:i]) >= 2 {
			s = s[:i]
		}
	}
	return firstLine(s, 40)
}

// classifyScoreGroup 依据评分类别与因素名分为技术 / 商务 / 价格。类别同时含商务与技术（如「客观分（商务、技术及服务部分）」）
// 或缺失时按因素名判断，仍无法判断的视为技术。
func classifyScoreGroup(group, name string) string {
	if rePriceGroup.MatchString(group) {
		return ScoreGroupPrice
	}
	biz, tech := reBizGroup.MatchString(group), reTechGroup.MatchString(group)
	switch {
	case biz && !tech:
		return ScoreGroupCommercial
	case tech && !biz:
		return ScoreGroupTechnical
	}
	switch {
	case rePriceName.MatchString(name):
		return ScoreGroupPrice
	case reBizName.MatchString(name):
		return ScoreGroupCommercial
	}
	return ScoreGroupTechnical
}

// scoreCarry 是紧邻的上一张评分表留给续表的上下文：列角色（供无表头续表沿用）与末行评分类别
// （跨页拆成两张表时，下一张表开头的类别单元格为空）。
type scoreCarry struct {
	schema scoreSchema
	group  string
	weight float64
}

// parseScoringTable 解析一张评分表。prev 非空时表示本表紧跟上一张评分表，可沿用其列角色与末行评分类别；
// 返回的 carry 供下一张表使用。summary 非空表示该表只是类别分值汇总（如「商务部分 30 / 技术部分 60 / 合计 100」），不产出评分项。
func parseScoringTable(t *scoreTable, prev *scoreCarry) (items []*ScoredItem, carry *scoreCarry, summary map[string]float64) {
	var s scoreSchema
	if found, ok := detectScoreSchema(t); ok {
		s = found
	} else if prev != nil && prev.schema.cols == len(firstRow(t.Grid)) {
		s = prev.schema
		s.header = -1
	} else {
		return nil, nil, nil
	}
	org := t.origins()
	cell := func(r, c int) string {
		if c < 0 || c >= len(t.Grid[r]) {
			return ""
		}
		return strings.TrimSpace(t.Grid[r][c])
	}
	origin := func(r, c int) int {
		if c < 0 || c >= len(org[r]) {
			return -1
		}
		return org[r][c]
	}
	page := func(r int) int {
		for c := range org[r] {
			if i := org[r][c]; i >= 0 && t.Cells[i].Row == r {
				return t.Cells[i].Page
			}
		}
		return 0
	}
	var (
		cur           *ScoredItem
		group         string
		groupWeight   float64
		curFactor     = -1
		factorIsGroup bool // 因素列实际填的是评分类别（名称取自分值列标签或评分标准）
	)
	if prev != nil {
		group, groupWeight = prev.group, prev.weight
	}
	var (
		counted   = map[int]bool{} // 已计入分值的分值单元格
		lastScore = map[*ScoredItem]string{}
		lastPage  = 0
	)
	headerText := ""
	if s.header >= 0 {
		headerText = strings.Join(t.Grid[s.header], "\x1f")
	}
	appendText := func(dst *string, txt string) {
		txt = strings.TrimSpace(txt)
		if txt == "" || strings.Contains(*dst, txt) {
			return
		}
		if *dst != "" {
			*dst += "\n"
		}
		*dst += txt
	}
	for r := s.header + 1; r < len(t.Grid); r++ {
		row := t.Grid[r]
		if strings.Join(row, "\x1f") == headerText {
			continue // 跨页重复表头
		}
		nonEmpty, first, same := 0, "", true
		for _, c := range row {
			if c = strings.TrimSpace(c); c != "" {
				if nonEmpty == 0 {
					first = c
				} else if c != first {
					same = false
				}
				nonEmpty++
			}
		}
		if nonEmpty == 0 || reTotalRow.MatchString(normCell(first)) || (same && nonEmpty > 1 && len(row) > 2) {
			continue // 空行、合计行、整行合并的标题 / 注释行
		}
		pg := page(r)
		newPage := lastPage > 0 && pg > lastPage
		lastPage = max(lastPage, pg)

		if g := cell(r, s.group); g != "" {
			group, groupWeight = splitGroup(g)
		}
		factorText, crit := cell(r, s.factor), cell(r, s.criteria)
		rawFactorEmpty := factorText == ""
		if factorText != "" && factorText == crit {
			factorText = "" // 因素与评分标准横向合并为一个单元格：没有独立的因素名
		}
		if s.group < 0 && factorText != "" && reGroupLike.MatchString(normCell(factorText)) {
			group, groupWeight = splitGroup(factorText) // 因素列实为评分类别
			factorText, factorIsGroup = "", true
		}
		scoreText := cell(r, s.score)
		label, score, hasScore := parseScoreText(scoreText)
		if f := normCell(factorText); !hasScore && f != "" && (reParenScore.MatchString(f) || reBareScore.MatchString(f)) {
			_, score, hasScore = parseScoreText(f) // 分值写在因素名中，如「项目实施团队（16分）」
		}
		if !hasScore && s.score < 0 {
			if m := reCritScore.FindStringSubmatch(crit); m != nil {
				v := m[1]
				if v == "" {
					v = m[2]
				}
				score, _ = strconv.ParseFloat(v, 64)
				hasScore = score > 0
			}
		}
		so := origin(r, s.score)
		fresh := hasScore && (so < 0 || !counted[so]) // 本行带有尚未计分的分值单元格
		fo := origin(r, s.factor)
		name := cleanName(factorText)
		if name == "" && label != "" {
			name = cleanName(label)
		}

		cont := false // 本行是否属于上一评分项
		switch {
		case cur == nil:
		case factorText != "" && fo >= 0 && fo == curFactor:
			cont = true // 纵向合并的因素单元格：同一评分因素的下一分项
		case factorText == "" && label == "" && !fresh:
			cont = true // 因素与分值都为空的续行（跨页拆开的单元格或分项细则）
		case rawFactorEmpty && s.factor >= 0 && !factorIsGroup && label == "" && group == cur.Group:
			cont = true // 因素单元格留空、自带分值的分项（因素单元格跨页拆开后留空）
		case newPage && factorText != "" && scoreText == "" && fo != curFactor && !(s.score < 0 && hasScore && name != ""):
			// 新页首行：上一页末尾被拆开的因素单元格续写（如「安全保密管理措施和技」+「术方案」、「管理体系」+「（8分）」）
			cont = true
			if frag := cleanName(factorText); frag != "" && utf8.RuneCountInString(frag) <= 8 && !reGroupLike.MatchString(frag) {
				cur.Name += frag
			}
			curFactor = fo
		case name != "" && name == cur.Name && group == cur.Group:
			cont = true // 同名分项：分值单元格相同且带标签（如两行「人员配置24」）时不重复计分
			if label != "" && normCell(scoreText) == lastScore[cur] {
				fresh = false
			}
		}
		if !cont {
			if s.groupAsFactor && cell(r, s.group) == "" && label != "" &&
				reBizName.MatchString(label) && groupWeight > 0 {
				var groupedPoints float64
				for _, item := range items {
					if item.Group == group {
						groupedPoints += item.Score
					}
				}
				if groupedPoints >= groupWeight {
					group, groupWeight = cleanName(label), 0
				}
			}
			switch {
			case name != "":
			case rePriceGroup.MatchString(group), s.groupAsFactor && group != "":
				name = group
			default:
				name = nameFromCriteria(crit)
			}
			if name == "" {
				name = group
			}
			if name == "" {
				continue
			}
			cur = &ScoredItem{Group: group, GroupWeight: groupWeight, Name: name, Page: pg}
			items = append(items, cur)
			curFactor = fo
		}
		cur.Rows = append(cur.Rows, r)
		if fresh {
			if so >= 0 {
				counted[so] = true
			}
			cur.Score += score
			lastScore[cur] = normCell(scoreText)
		}
		appendText(&cur.Criteria, crit)
		appendText(&cur.Note, cell(r, s.note))
		if reMandatoryMrk.MatchString(factorText + crit) {
			cur.Mandatory = true
		}
	}
	// 只含类别名的表是分值汇总表
	allGroups := len(items) > 0
	for _, it := range items {
		if !reSummaryName.MatchString(it.Name) || it.Group != "" && it.Group != it.Name {
			allGroups = false
			break
		}
	}
	if allGroups {
		summary = map[string]float64{}
		for _, it := range items {
			summary[it.Name] += it.Score
		}
		return nil, &scoreCarry{schema: s}, summary
	}
	scored := 0
	for _, it := range items {
		it.ScoreGroup = classifyScoreGroup(it.Group, it.Name)
		if it.Score > 0 {
			scored++
		}
	}
	if scored == 0 {
		return nil, nil, nil
	}
	return items, &scoreCarry{schema: s, group: group, weight: groupWeight}, nil
}

func firstRow(g [][]string) []string {
	if len(g) == 0 {
		return nil
	}
	return g[0]
}

// ---------------------------------------------------------------------------
// 分包标识
// ---------------------------------------------------------------------------

var (
	reLotBefore = regexp.MustCompile(`(?:第\s*)?([A-Za-z]|\d{1,2}|[一二三四五六七八九十]{1,3})\s*(?:包|分包|子包|标段|标包)(?:[：:\s（(]|评|详|初|采|$)`)
	reLotAfter  = regexp.MustCompile(`(?:包|分包|标段|标包)\s*([A-Za-z]|\d{1,2}|[一二三四五六七八九十]{1,3})(?:[^0-9A-Za-z]|$)`)
)

var cnDigits = map[rune]int{'一': 1, '二': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9, '十': 10}

func normalizeLot(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	if r, _ := utf8.DecodeRuneInString(s); cnDigits[r] > 0 {
		n := 0
		for _, ch := range s {
			v := cnDigits[ch]
			switch {
			case v == 10 && n == 0:
				n = 10
			case v == 10:
				n *= 10
			default:
				n += v
			}
		}
		return strconv.Itoa(n)
	}
	return s
}

// lastLotMarker 返回文本中最后一个分包标识（规范化为「1」「A」等），没有时返回空串。
func lastLotMarker(text string) string {
	best, pos := "", -1
	for _, re := range []*regexp.Regexp{reLotBefore, reLotAfter} {
		for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
			if m[0] > pos {
				pos, best = m[0], text[m[2]:m[3]]
			}
		}
	}
	return normalizeLot(best)
}

// LotLabel 是分包标识的展示文字。
func LotLabel(lot string) string {
	if lot == "" {
		return ""
	}
	return "包" + lot
}

// ---------------------------------------------------------------------------
// 分值汇总与校验
// ---------------------------------------------------------------------------

// ScoringGroupSummary 是一个评分类别的声明分值与已提取分值。
type ScoringGroupSummary struct {
	Group      string  `json:"group"`
	ScoreGroup string  `json:"score_group"`
	Declared   float64 `json:"declared"`
	Extracted  float64 `json:"extracted"`
	Items      int     `json:"items"`
}

// ScoringSummary 汇总一个分包（无分包时 lot 为空）的评分项分值，用于提示漏提 / 重复。
type ScoringSummary struct {
	Lot        string                `json:"lot"`
	Items      int                   `json:"items"`
	Total      float64               `json:"total"`
	Technical  float64               `json:"technical"`
	Commercial float64               `json:"commercial"`
	Price      float64               `json:"price"`
	Other      float64               `json:"unclassified"`
	Groups     []ScoringGroupSummary `json:"groups"`
	Warnings   []string              `json:"warnings"`
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

type scoringSourceLocation struct {
	Lot           string    `json:"lot"`
	Group         string    `json:"group"`
	GroupWeight   float64   `json:"group_weight"`
	ScoreConflict []float64 `json:"score_conflict"`
	ScoringOrphan bool      `json:"scoring_orphan"`
}

func scoringSource(r *RequirementItem) scoringSourceLocation {
	var loc scoringSourceLocation
	_ = json.Unmarshal(r.SourceLoc, &loc)
	return loc
}

// summarizeScoring 按分包汇总评分类要求的分值，并与类别声明分值及 100 分制对照。
func summarizeScoring(items []*RequirementItem) []ScoringSummary {
	byLot := map[string]*ScoringSummary{}
	groups := map[string]map[string]*ScoringGroupSummary{}
	unresolved := map[string]bool{}
	var lots []string
	for _, r := range items {
		if r.Category != ReqScoringItem || r.IsSuperseded() || r.ParentID != nil {
			continue
		}
		loc := scoringSource(r)
		if loc.ScoringOrphan && r.ReviewStatus == ReviewNeedsRevision {
			continue
		}
		if r.Lot != "" {
			loc.Lot = r.Lot
		}
		s := byLot[loc.Lot]
		if s == nil {
			s = &ScoringSummary{Lot: loc.Lot, Groups: []ScoringGroupSummary{}, Warnings: []string{}}
			byLot[loc.Lot], groups[loc.Lot] = s, map[string]*ScoringGroupSummary{}
			lots = append(lots, loc.Lot)
		}
		s.Items++
		if len(loc.ScoreConflict) > 1 && r.ScoreWeight == 0 {
			unresolved[loc.Lot] = true
			s.Warnings = append(s.Warnings, fmt.Sprintf("%s「%s」原件分值冲突：%s，待人工复核，未计入合计",
				LotLabel(loc.Lot), loc.Group, scoreAlternatives(loc.ScoreConflict)))
			continue
		}
		s.Total += r.ScoreWeight
		switch r.ScoreGroup {
		case ScoreGroupTechnical:
			s.Technical += r.ScoreWeight
		case ScoreGroupCommercial:
			s.Commercial += r.ScoreWeight
		case ScoreGroupPrice:
			s.Price += r.ScoreWeight
		default:
			s.Other += r.ScoreWeight
		}
		if loc.Group != "" {
			g := groups[loc.Lot][loc.Group]
			if g == nil {
				g = &ScoringGroupSummary{Group: loc.Group, ScoreGroup: r.ScoreGroup}
				groups[loc.Lot][loc.Group] = g
			} else if g.ScoreGroup != r.ScoreGroup {
				g.ScoreGroup = ""
			}
			g.Declared = math.Max(g.Declared, loc.GroupWeight)
			g.Extracted += r.ScoreWeight
			g.Items++
		}
	}
	sort.Strings(lots)
	out := make([]ScoringSummary, 0, len(lots))
	for _, lot := range lots {
		s := byLot[lot]
		names := make([]string, 0, len(groups[lot]))
		for n := range groups[lot] {
			names = append(names, n)
		}
		sort.Strings(names)
		declared := 0.0
		for _, n := range names {
			g := *groups[lot][n]
			g.Extracted = round1(g.Extracted)
			s.Groups = append(s.Groups, g)
			declared += g.Declared
			if g.Declared > 0 && math.Abs(g.Declared-g.Extracted) > 0.05 {
				s.Warnings = append(s.Warnings, fmt.Sprintf("%s「%s」声明 %.4g 分，已提取 %.4g 分（%d 项），请核对是否漏提、重复或分值有误",
					LotLabel(lot), n, g.Declared, g.Extracted, g.Items))
			}
		}
		s.Total, s.Technical, s.Commercial, s.Price, s.Other = round1(s.Total), round1(s.Technical), round1(s.Commercial), round1(s.Price), round1(s.Other)
		if !unresolved[lot] && math.Abs(s.Total-100) > 0.05 {
			s.Warnings = append(s.Warnings, fmt.Sprintf("%s评分项合计 %.4g 分，不是 100 分，请核对评分表是否完整提取", strings.TrimSpace(LotLabel(lot)+" "), s.Total))
		}
		out = append(out, *s)
	}
	return out
}

// ---------------------------------------------------------------------------
// 提取：按原文顺序遍历分块中的评分表
// ---------------------------------------------------------------------------

func chunkKey(cs *chunkSource) string { return fmt.Sprintf("D%d-%04d", cs.DocID, cs.Loc["seq"]) }

// scoringCandidates 按文档原文顺序解析全部评分表，返回评分项候选、含评分表的分块与含评分表的文档。
// 分包标识取表格之前最近出现的「包1 / A包 / 第一包 / 标段2」等标记；同一文档的评分项只有一个分包时不标分包。
// 没有表头的表格紧跟在评分表之后（中间只有标题级短文本）且列数一致时，视为跨页 / 跨段续表沿用前表列角色。
func scoringCandidates(order []*chunkSource) ([]reqCandidate, map[int64]bool, map[int64]bool) {
	type parsed struct {
		cs      *chunkSource
		table   int
		items   []*ScoredItem
		scope   []string
		special bool
	}
	var (
		all          []parsed
		formulas     []parsed
		chunks       = map[int64]bool{}
		docs         = map[int64]bool{}
		docLots      = map[int64]map[string]bool{}
		declared     = map[int64]map[string]map[string]float64{}
		lastDoc      = int64(-1)
		lot          string
		tail         string
		scope        []string
		carry        *scoreCarry
		complexCarry *complexScoreCarry
	)
	documentLots := readDocumentScoreLots(order)
	for _, cs := range order {
		if cs.DocID != lastDoc {
			lastDoc, lot, tail, scope, carry, complexCarry = cs.DocID, "", "", nil, nil, nil
		}
		if m := lastLotMarker(cs.SectionCode + " " + cs.SectionTitle); m != "" {
			lot = m
		}
		if named := lotsFromScoringCaption(cs.SectionTitle); len(named) > 0 {
			scope = named
		}
		var tables []scoreTable
		if len(cs.Tables) > 2 {
			_ = json.Unmarshal(cs.Tables, &tables)
		}
		pos := 0
		for ti := range tables {
			t := &tables[ti]
			if len(t.Grid) == 0 {
				continue
			}
			between := ""
			if at := strings.Index(cs.Content[pos:], t.Markdown); t.Markdown != "" && at >= 0 {
				between = cs.Content[pos : pos+at]
				if m := lastLotMarker(between); m != "" {
					lot = m
				}
				if named := lotsFromScoringCaption(between); len(named) > 0 {
					scope = named
					carry, complexCarry = nil, nil
				}
				if utf8.RuneCountInString(strings.TrimSpace(between)) > 40 {
					carry, complexCarry = nil, nil
				}
				pos += at + len(t.Markdown)
			}
			caption := separateScoringCaption(between)
			if caption.group == "" && strings.TrimSpace(between) == "" {
				caption = separateScoringCaption(tail)
			}
			var items []*ScoredItem
			var handled, special bool
			var summary map[string]float64
			items, complexCarry, handled = parseComplexScoringTable(t, complexCarry)
			if handled {
				carry = nil
			} else if items, handled = parseGroupedScoreTable(t); handled {
				carry = nil
			} else if items, handled = parseWeightedScoringTable(t); handled {
				carry = nil
			} else if parsedItems, next, found := parseSeparateScoringTable(t, caption, cs.SectionTitle+"\n"+between); found {
				items, carry, handled = parsedItems, next, true
			} else {
				items, carry, summary = parseScoringTable(t, carry)
			}
			special = handled || carry != nil && carry.schema.groupAsFactor
			if handled {
				chunks[cs.ChunkID] = true
			}
			if summary != nil {
				if declared[cs.DocID] == nil {
					declared[cs.DocID] = map[string]map[string]float64{}
				}
				if declared[cs.DocID][lot] == nil {
					declared[cs.DocID][lot] = map[string]float64{}
				}
				for label, weight := range summary {
					declared[cs.DocID][lot][label] = weight
				}
				continue
			}
			if len(items) == 0 {
				continue
			}
			applyScoringCaption(items, caption)
			for _, it := range items {
				it.Lot = lot
			}
			all = append(all, parsed{cs: cs, table: ti, items: items, scope: scope, special: special})
			chunks[cs.ChunkID], docs[cs.DocID] = true, true
			if docLots[cs.DocID] == nil {
				docLots[cs.DocID] = map[string]bool{}
			}
			docLots[cs.DocID][lot] = true
		}
		if formula := separatePriceFormula(cs, tables); formula != nil {
			formula.Lot = lot
			formulas = append(formulas, parsed{cs: cs, table: -1, items: []*ScoredItem{formula}, scope: scope, special: true})
		}
		text := cs.Content[pos:]
		if named := lotsFromScoringCaption(text); len(named) > 0 {
			scope = named
			carry, complexCarry = nil, nil
		}
		if m := lastLotMarker(text); m != "" {
			lot = m
		}
		if len(tables) == 0 && utf8.RuneCountInString(cs.Content) > 80 {
			carry, complexCarry = nil, nil
		}
		tail = text
	}
	scopeFor := func(p parsed, it *ScoredItem) []string {
		doc := documentLots[p.cs.DocID]
		switch {
		case doc.cover != "":
			return []string{doc.cover}
		case len(p.scope) > 0:
			return p.scope
		case p.special && len(doc.all) > 1:
			return doc.all
		case len(docLots[p.cs.DocID]) > 1:
			return []string{it.Lot}
		default:
			return []string{""}
		}
	}
	coveredPrice := map[int64]map[string]bool{}
	for _, p := range all {
		for _, item := range p.items {
			if item.ScoreGroup != ScoreGroupPrice {
				continue
			}
			if coveredPrice[p.cs.DocID] == nil {
				coveredPrice[p.cs.DocID] = map[string]bool{}
			}
			for _, lot := range scopeFor(p, item) {
				coveredPrice[p.cs.DocID][lot] = true
			}
		}
	}
	for _, p := range formulas {
		for _, lot := range scopeFor(p, p.items[0]) {
			if coveredPrice[p.cs.DocID][lot] || coveredPrice[p.cs.DocID][""] {
				continue
			}
			if coveredPrice[p.cs.DocID] == nil {
				coveredPrice[p.cs.DocID] = map[string]bool{}
			}
			coveredPrice[p.cs.DocID][lot] = true
			p.scope = []string{lot}
			all = append(all, p)
			chunks[p.cs.ChunkID], docs[p.cs.DocID] = true, true
		}
	}
	var out []reqCandidate
	for _, p := range all {
		docKey := chunkKey(p.cs)
		for k, item := range p.items {
			lots := scopeFor(p, item)
			for _, lot := range lots {
				it := *item
				it.Lot = lot
				if it.GroupWeight == 0 {
					it.GroupWeight = declared[p.cs.DocID][lot][it.Group]
				}
				code := fmt.Sprintf("SC-%s-T%d-%02d", docKey, p.table+1, k+1)
				if len(lots) > 1 || p.table < 0 && lot != "" {
					code += "-L" + lot
				}
				out = append(out, scoringRequirement(p.cs, p.table, code, &it))
			}
		}
	}
	return out, chunks, docs
}

func scoringRequirement(cs *chunkSource, table int, code string, it *ScoredItem) reqCandidate {
	title := it.Name
	if it.Lot != "" {
		title = "【" + LotLabel(it.Lot) + "】" + title
	}
	var desc strings.Builder
	if it.Group != "" {
		fmt.Fprintf(&desc, "评分类别：%s", it.Group)
		if it.GroupWeight > 0 {
			fmt.Fprintf(&desc, "（%.4g 分）", it.GroupWeight)
		}
		desc.WriteString("\n")
	}
	if len(it.ScoreConflict) > 1 {
		desc.WriteString("分值：原件冲突，待复核（" + scoreAlternatives(it.ScoreConflict) + "）\n")
	} else {
		fmt.Fprintf(&desc, "分值：%.4g 分\n", it.Score)
	}
	if it.Criteria != "" {
		desc.WriteString("评分标准：" + it.Criteria + "\n")
	}
	if it.Note != "" {
		desc.WriteString("说明：" + it.Note + "\n")
	}
	loc := cloneLoc(cs.Loc, "lot", it.Lot, "group", it.Group, "group_weight", it.GroupWeight)
	if table >= 0 && len(it.Rows) > 0 {
		loc["table"], loc["row"], loc["rows"] = table, it.Rows[0], it.Rows
	}
	if it.Page > 0 {
		loc["item_page"] = it.Page
	}
	if len(it.ScoreConflict) > 1 {
		loc["score_conflict"] = it.ScoreConflict
	}
	tiers, evalKind, scoreMin := ParseScoringTiers(it.Criteria+"\n"+it.Note, it.Score, it.ScoreGroup)
	if native := groupedScoreEvalKind(it.Group); native != "" {
		evalKind = native
	}
	proof := ParseProofRules(title+"\n"+it.Criteria+"\n"+it.Note, ReqScoringItem, it.ScoreGroup)
	return reqCandidate{code: code, category: ReqScoringItem, title: firstLine(title, 60),
		description: strings.TrimSpace(desc.String()), mandatory: it.Mandatory, score: it.Score,
		scoreMin: scoreMin, scoreGroup: it.ScoreGroup, evalKind: evalKind, atomType: AtomScoringItem,
		scoringTiers: tiers, proofRules: proof, lot: it.Lot, tableItem: true, src: cs, loc: loc}
}

func scoreAlternatives(values []float64) string {
	text := make([]string, len(values))
	for i, value := range values {
		text[i] = fmt.Sprintf("%.4g 分", value)
	}
	return strings.Join(text, " / ")
}

// ---------------------------------------------------------------------------
// D1：评分细则拆解（档次 / 得分点 / 主客观属性）与证明材料硬规则解析
// ---------------------------------------------------------------------------

const (
	AtomVetoItem      = "veto_item"
	AtomScoringItem   = "scoring_item"
	AtomScoringPoint  = "scoring_point"
	AtomTierCriterion = "tier_criterion"
	AtomDemandItem    = "demand_item"
	AtomProofRule     = "proof_rule"
	AtomFormatRule    = "format_rule"

	EvalObjective  = "objective"
	EvalSubjective = "subjective"
	EvalMixed      = "mixed"
)

func validAtomType(a string) bool {
	switch a {
	case "", AtomVetoItem, AtomScoringItem, AtomScoringPoint, AtomTierCriterion, AtomDemandItem, AtomProofRule, AtomFormatRule:
		return true
	}
	return false
}

func validEvalKind(e string) bool {
	switch e {
	case "", EvalObjective, EvalSubjective, EvalMixed:
		return true
	}
	return false
}

// DefaultAtomType 按要求类别与是否有父节点推断默认原子粒度类型。
func DefaultAtomType(category string, hasParent bool) string {
	switch category {
	case ReqVetoItem:
		return AtomVetoItem
	case ReqScoringItem:
		if hasParent {
			return AtomScoringPoint
		}
		return AtomScoringItem
	case ReqFormatAttachment:
		return AtomFormatRule
	default:
		return AtomDemandItem
	}
}

// ScoringTier 表示一条评分项拆解出的档次判据或逐条得分点。
type ScoringTier struct {
	Seq       int     `json:"seq"`
	Label     string  `json:"label"`
	Kind      string  `json:"kind"` // tier（优/良/中/差档次）| point（逐条得分点）| deduction（扣分项）| formula（价格公式）
	EvalKind  string  `json:"eval_kind,omitempty"`
	ScoreMin  float64 `json:"score_min,omitempty"`
	ScoreMax  float64 `json:"score_max,omitempty"`
	Criteria  string  `json:"criteria"`
	ProofHint string  `json:"proof_hint,omitempty"`
}

// ProofRules 表示从评分标准、审查表或实质性条款中抽取的取证硬规则。
// ModelParams / ScopeKeywords 是 D2 可采性扩展：型号参数门槛与认证 / 合同范围关键词。
type ProofRules struct {
	RequiredDocs         []string          `json:"required_docs,omitempty"`
	ContractPages        []string          `json:"contract_pages,omitempty"`
	NeedAcceptance       bool              `json:"need_acceptance,omitempty"`
	DateAnchor           string            `json:"date_anchor,omitempty"` // signing_date | acceptance_date
	YearsWindow          int               `json:"years_window,omitempty"`
	SinceDate            string            `json:"since_date,omitempty"`
	ValidOnBidDeadline   bool              `json:"valid_on_bid_deadline,omitempty"`
	SocialSecurityMonths int               `json:"social_security_months,omitempty"`
	BidderOnly           bool              `json:"bidder_only,omitempty"`
	MinCount             int               `json:"min_count,omitempty"`
	ModelParams          map[string]string `json:"model_params,omitempty"`
	ScopeKeywords        []string          `json:"scope_keywords,omitempty"`
	RawSummary           string            `json:"raw_summary,omitempty"`
}

func (p ProofRules) IsZero() bool {
	return len(p.RequiredDocs) == 0 && len(p.ContractPages) == 0 && !p.NeedAcceptance && p.DateAnchor == "" &&
		p.YearsWindow == 0 && p.SinceDate == "" && !p.ValidOnBidDeadline && p.SocialSecurityMonths == 0 &&
		!p.BidderOnly && p.MinCount == 0 && len(p.ModelParams) == 0 && len(p.ScopeKeywords) == 0 && p.RawSummary == ""
}

var (
	reMetaPrefix     = regexp.MustCompile(`^(?:评分类别|分值|评分标准|说明)[：:]\s*`)
	reBaseScore      = regexp.MustCompile(`起评分[：:为]?\s*([0-9]+(?:\.[0-9]+)?)\s*分`)
	reTierBandLabel  = regexp.MustCompile(`(?:^|[；;。\n\s])(第[一二三四五1-5]档|[一二三四五]档|优秀?|良好?|中等?|较差|差|好|较好|一般)(?:[（(\[【]\s*(\d+(?:\.\d+)?)\s*(?:[-~～至,，]\s*(\d+(?:\.\d+)?))?\s*分?\s*[）)\]】]|[：:、，,\s])`)
	reSubPointSplit  = regexp.MustCompile(`(?m)(?:^|[；;\n])\s*(?:[（(]\d+[)）]|\d+[.、）)]|[①②③④⑤⑥⑦⑧⑨⑩]|[（(][一二三四五六七八九十]+[)）])`)
	reClauseScore    = regexp.MustCompile(`(?:最高(?:得|计)?|各?(?:得|计)|[（(\[【])\s*[（(\[【]?\s*([0-9]+(?:\.[0-9]+)?)(?:\s*[-~～至,，]\s*([0-9]+(?:\.[0-9]+)?))?\s*[）)\]】]?\s*分`)
	reDeductionScore = regexp.MustCompile(`扣\s*([0-9]+(?:\.[0-9]+)?)\s*分`)
	reSubjectiveCue  = regexp.MustCompile(`合理|完善|详实|清晰|透彻|完整|可行|科学|针对性|深入|准确|优|良|中|差|第一档|第二档|第三档|一档|二档|三档|酌情|比较|一般|欠缺|贴合`)
	reObjectiveCue   = regexp.MustCompile(`每提供|每个得|每项得|每名|每人|每份|每套|具有.*证书|持有.*证书|具备.*资质|通过.*认证|认证证书|合同|业绩|案例|著作权|软著|专利|检测报告|测试报告|审计报告|社保|基准价|计算公式|低价优先`)
)

func stripScoringMeta(text string) string {
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "评分类别：") || strings.HasPrefix(t, "分值：") {
			continue
		}
		t = reMetaPrefix.ReplaceAllString(t, "")
		lines = append(lines, t)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func classifyClauseEval(clause, scoreGroup string) string {
	if scoreGroup == ScoreGroupPrice {
		return EvalObjective
	}
	obj := reObjectiveCue.MatchString(clause)
	sub := reSubjectiveCue.MatchString(clause)
	switch {
	case obj && !sub:
		return EvalObjective
	case sub && !obj:
		return EvalSubjective
	case obj && sub:
		if strings.Contains(clause, "档") || strings.Contains(clause, "优") || strings.Contains(clause, "良") {
			return EvalSubjective
		}
		return EvalObjective
	case scoreGroup == ScoreGroupCommercial:
		return EvalObjective
	default:
		return EvalSubjective
	}
}

func extractProofHint(clause string) string {
	p := ParseProofRules(clause, ReqScoringItem, "")
	if len(p.RequiredDocs) == 0 {
		return ""
	}
	return strings.Join(p.RequiredDocs, "+")
}

// ParseScoringTiers 将评分标准文本拆解为结构化档次 / 得分点列表，并推断主客观属性（objective / subjective / mixed）与最低起评分。
func ParseScoringTiers(text string, maxScore float64, scoreGroup string) ([]ScoringTier, string, float64) {
	body := stripScoringMeta(text)
	if body == "" {
		body = strings.TrimSpace(text)
	}
	if scoreGroup == ScoreGroupPrice {
		if body == "" {
			body = "按评标基准价公式计算价格得分"
		}
		return []ScoringTier{{Seq: 1, Label: "基准价公式", Kind: "formula", EvalKind: EvalObjective, ScoreMin: 0, ScoreMax: maxScore, Criteria: body}}, EvalObjective, 0
	}
	scoreMin := 0.0
	if m := reBaseScore.FindStringSubmatch(body); m != nil && !reDeductionScore.MatchString(body) {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil && v >= 0 && v <= maxScore {
			scoreMin = v
		}
	}

	// 将正文按分号或换行切分为候选子句。
	rawParts := splitScoringClauses(body)
	var tiers []ScoringTier
	sumMax := 0.0
	hasBand := reTierBandLabel.MatchString(body)
	for _, part := range rawParts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var sMin, sMax float64
		kind := "point"
		label := fmt.Sprintf("得分点%d", len(tiers)+1)
		if bm := reTierBandLabel.FindStringSubmatch(part); bm != nil {
			kind = "tier"
			label = bm[1]
			if bm[2] != "" {
				v1, _ := strconv.ParseFloat(bm[2], 64)
				if bm[3] != "" {
					v2, _ := strconv.ParseFloat(bm[3], 64)
					sMin, sMax = math.Min(v1, v2), math.Max(v1, v2)
				} else {
					sMax = v1
				}
			}
		}
		if sMax == 0 {
			if dm := reDeductionScore.FindStringSubmatch(part); dm != nil {
				kind = "deduction"
				label = fmt.Sprintf("扣分项%d", len(tiers)+1)
				sMax, _ = strconv.ParseFloat(dm[1], 64)
			} else if sm := reClauseScore.FindAllStringSubmatch(part, -1); len(sm) > 0 {
				last := sm[len(sm)-1]
				v1, _ := strconv.ParseFloat(last[1], 64)
				if last[2] != "" {
					v2, _ := strconv.ParseFloat(last[2], 64)
					sMin, sMax = math.Min(v1, v2), math.Max(v1, v2)
				} else {
					sMax = v1
				}
			}
		}
		if len(rawParts) > 1 && sMax == 0 && kind != "tier" {
			// 若没有分值且上一条已有档次/得分点，并入上一条说明。
			if len(tiers) > 0 {
				tiers[len(tiers)-1].Criteria += "；" + part
				if tiers[len(tiers)-1].ProofHint == "" {
					tiers[len(tiers)-1].ProofHint = extractProofHint(part)
				}
				continue
			}
		}
		if sMax == 0 && maxScore > 0 && len(rawParts) == 1 {
			sMax = maxScore
		}
		ev := classifyClauseEval(part, scoreGroup)
		if kind == "tier" {
			ev = EvalSubjective
		}
		sumMax += sMax
		tiers = append(tiers, ScoringTier{
			Seq:       len(tiers) + 1,
			Label:     label,
			Kind:      kind,
			EvalKind:  ev,
			ScoreMin:  sMin,
			ScoreMax:  sMax,
			Criteria:  part,
			ProofHint: extractProofHint(part),
		})
	}
	if len(tiers) == 0 {
		ev := classifyClauseEval(body, scoreGroup)
		kind := "point"
		if ev == EvalSubjective {
			kind = "tier"
		}
		tiers = []ScoringTier{{Seq: 1, Label: "评分标准", Kind: kind, EvalKind: ev, ScoreMin: scoreMin, ScoreMax: maxScore,
			Criteria: body, ProofHint: extractProofHint(body)}}
	} else if len(tiers) > 1 && (hasBand || (maxScore > 0 && sumMax > maxScore*1.15 && classifyClauseEval(body, scoreGroup) == EvalSubjective)) {
		// 多个互斥分值档次（如：得10分；得7分；得4分）：归一为 tier。
		for i := range tiers {
			if tiers[i].Kind == "point" {
				tiers[i].Kind = "tier"
				tiers[i].EvalKind = EvalSubjective
				if strings.HasPrefix(tiers[i].Label, "得分点") {
					tiers[i].Label = fmt.Sprintf("第%d档", i+1)
				}
			}
		}
	}
	hasObj, hasSub := false, false
	for _, t := range tiers {
		switch t.EvalKind {
		case EvalObjective:
			hasObj = true
		case EvalSubjective:
			hasSub = true
		}
	}
	evalKind := EvalSubjective
	switch {
	case hasObj && hasSub:
		evalKind = EvalMixed
	case hasObj:
		evalKind = EvalObjective
	}
	return tiers, evalKind, scoreMin
}

func splitScoringClauses(body string) []string {
	if idxs := reSubPointSplit.FindAllStringIndex(body, -1); len(idxs) >= 2 {
		var out []string
		start := 0
		for _, idx := range idxs {
			seg := strings.TrimSpace(body[start:idx[0]])
			if seg != "" {
				out = append(out, seg)
			}
			start = idx[0]
			for start < len(body) && (body[start] == '\n' || body[start] == ';') {
				start++
			}
			if strings.HasPrefix(body[start:], "；") {
				start += len("；")
			}
		}
		if tail := strings.TrimSpace(body[start:]); tail != "" {
			out = append(out, tail)
		}
		if len(out) >= 2 {
			return out
		}
	}
	var out []string
	for _, line := range strings.Split(body, "\n") {
		for _, sub := range strings.Split(line, "；") {
			if s := strings.TrimSpace(sub); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

var (
	reYearsWindow          = regexp.MustCompile(`近\s*([一二三四五六七八九十0-9]+)\s*年`)
	reSinceDate            = regexp.MustCompile(`(20\d{2})\s*年\s*(\d{1,2})\s*月(?:\s*(\d{1,2})\s*日)?\s*(?:以来|起|至今|后|（含）|\(含\))?`)
	reSocialSecurityMonths = regexp.MustCompile(`(?:(?:近|连续|不少于|至少)\s*([一二三四五六七八九十0-9]+)\s*个月.{0,14}(?:社保|社会保险|养老保险)|(?:社保|社会保险|养老保险).{0,14}?([一二三四五六七八九十0-9]+)\s*个月)`)
	reBidderOnly           = regexp.MustCompile(`(?:母公司|子公司|关联公司|总公司|分公司|分支机构|控股|上级单位).{0,16}(?:不予认可|不接受|不得|不算|无效|不计|不作为)|(?:不接受|不予认可|不认可|不得使用).{0,16}(?:母公司|子公司|关联公司|总公司|分公司|分支机构|控股|上级单位)|仅认可投标人(?:自身|本单位|独立)`)
	reDateAnchorSign       = regexp.MustCompile(`以.{0,8}(?:合同)?签订(?:时间|日期)为准|按合同签订(?:时间|日期)`)
	reDateAnchorAccept     = regexp.MustCompile(`以.{0,8}(?:项目|竣工|终验|合同|履约)?验收(?:合格)?(?:时间|日期)为准|按验收(?:时间|日期)`)
	reNeedAcceptance       = regexp.MustCompile(`(?:同时|并|及|和|附|含|连同).{0,14}(?:验收报告|验收证明|验收合格|履约评价|用户评价|业主评价)|(?:合同|协议).{0,20}(?:验收报告|验收单|验收证明)`)
	reValidDeadline        = regexp.MustCompile(`有效期内|有效的|在有效期|有效期至|处于有效`)
	reMinCount             = regexp.MustCompile(`(?:不少于|至少|提供|具备)\s*([0-9]+)\s*(?:个|份|项|名|人|套)`)
	reProofSentence        = regexp.MustCompile(`[^。；;\n]*(?:提供|须附|应附|复印件|扫描件|证明|原件|盖章|签章|有效期|不予认可|为准)[^。；;\n]*`)
)

func parseChineseInt(s string) int {
	s = strings.TrimSpace(s)
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	m := map[string]int{"一": 1, "二": 2, "两": 2, "三": 3, "四": 4, "五": 5, "六": 6, "七": 7, "八": 8, "九": 9, "十": 10, "十二": 12}
	return m[s]
}

// ParseProofRules 从要求或评分标准文本中抽取结构化证明材料硬规则。
func ParseProofRules(text, category, scoreGroup string) ProofRules {
	var p ProofRules
	if strings.TrimSpace(text) == "" || scoreGroup == ScoreGroupPrice {
		return p
	}
	docRules := []struct {
		key string
		re  *regexp.Regexp
	}{
		{"license", regexp.MustCompile(`营业执照|法人证书|事业单位法人|登记证书`)},
		{"certificate", regexp.MustCompile(`证书|认证|资质|职称|执业资格|CCRC|CMMI|ITSS|ISO\s*\d+|等级保护`)},
		{"contract", regexp.MustCompile(`合同|协议书|中标通知书|成交通知书`)},
		{"acceptance_report", regexp.MustCompile(`验收报告|验收单|验收证明|履约验收|用户评价|业主评价|完工证明`)},
		{"social_security", regexp.MustCompile(`社保|社会保险|养老保险|参保证明`)},
		{"financial_report", regexp.MustCompile(`审计报告|财务报告|财务报表|资产负债表|资信证明`)},
		{"tax_proof", regexp.MustCompile(`纳税|完税|税收证明`)},
		{"test_report", regexp.MustCompile(`检测报告|测试报告|检验报告|测评报告`)},
		{"ip_patent", regexp.MustCompile(`著作权|软著|专利|知识产权`)},
		{"invoice", regexp.MustCompile(`发票`)},
		{"commitment", regexp.MustCompile(`承诺函|承诺书|声明函|声明书|保证书|资格声明`)},
	}
	hasDoc := map[string]bool{}
	for _, r := range docRules {
		if r.re.MatchString(text) {
			p.RequiredDocs = append(p.RequiredDocs, r.key)
			hasDoc[r.key] = true
		}
	}
	if hasDoc["contract"] {
		for _, kw := range []string{"首页", "金额页", "标的页", "内容页", "清单页", "签章页", "签字盖章页", "签署页", "关键页"} {
			if strings.Contains(text, kw) {
				p.ContractPages = append(p.ContractPages, kw)
			}
		}
	}
	if (hasDoc["contract"] && hasDoc["acceptance_report"]) || reNeedAcceptance.MatchString(text) {
		p.NeedAcceptance = true
	}
	switch {
	case reDateAnchorAccept.MatchString(text):
		p.DateAnchor = "acceptance_date"
	case reDateAnchorSign.MatchString(text):
		p.DateAnchor = "signing_date"
	}
	if m := reYearsWindow.FindStringSubmatch(text); m != nil {
		p.YearsWindow = parseChineseInt(m[1])
	}
	if m := reSinceDate.FindStringSubmatch(text); m != nil {
		mo, _ := strconv.Atoi(m[2])
		day := 1
		if m[3] != "" {
			day, _ = strconv.Atoi(m[3])
		}
		if mo >= 1 && mo <= 12 && day >= 1 && day <= 31 {
			p.SinceDate = fmt.Sprintf("%s-%02d-%02d", m[1], mo, day)
		}
	}
	if reValidDeadline.MatchString(text) || hasDoc["certificate"] || hasDoc["license"] {
		p.ValidOnBidDeadline = true
	}
	if m := reSocialSecurityMonths.FindStringSubmatch(text); m != nil {
		raw := m[1]
		if raw == "" {
			raw = m[2]
		}
		p.SocialSecurityMonths = parseChineseInt(raw)
	}
	if reBidderOnly.MatchString(text) {
		p.BidderOnly = true
	}
	if m := reMinCount.FindStringSubmatch(text); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 && n <= 100 {
			p.MinCount = n
		}
	}
	if matches := reProofSentence.FindAllString(text, 3); len(matches) > 0 {
		var clean []string
		for _, s := range matches {
			if s = strings.TrimSpace(s); s != "" {
				clean = append(clean, s)
			}
		}
		p.RawSummary = truncateRunes(strings.Join(clean, "；"), 180)
	}
	return p
}
