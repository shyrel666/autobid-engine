package db

import (
	"regexp"
	"strconv"
	"strings"
)

// scoringCaption ties a score table to the category and weight declared in its title.
type scoringCaption struct {
	name   string
	group  string
	weight float64
}

var (
	reSeparateCaption    = regexp.MustCompile(`^(?:附表[\d一二三四五六七八九十]+[：:]?)?((技术(?:[（(]服务[）)])?|商务|服务|价格|报价)(?:评分(?:细则)?|评价|评审)表)(?:[（(](?:[\p{Han}（）()]{0,12}[：:])?(\d+(?:\.\d+)?)[分%％][）)])?$`)
	reScoreDeclaration   = regexp.MustCompile(`(技术|商务|服务|价格|报价)(?:部分|评分|评审|得分|分值)?[：:]?(\d+(?:\.\d+)?)分`)
	rePerformanceSection = regexp.MustCompile(`验收标准|履约(?:考核|评价)|每月(?:评分|考评|考核)|服务质量考核`)
	rePriceGroupLabel    = regexp.MustCompile(`^(?:投标)?(?:报价|价格)(?:部分|评分|评价|评审|得分|分)?$`)
	rePriceFormula       = regexp.MustCompile(`(?:报价|价格|评标价)得分[=＝]`)
)

func separateScoringCaption(text string) scoringCaption {
	lines := strings.Split(text, "\n")
	var caption scoringCaption
	for i := len(lines) - 1; i >= 0; i-- {
		previous := matchScoringCaption(lines[i])
		if previous.group == "" {
			continue
		}
		if caption.group != "" && caption.name != previous.name {
			break
		}
		caption = previous
		if caption.weight > 0 {
			break
		}
	}
	return caption
}

func matchScoringCaption(line string) scoringCaption {
	m := reSeparateCaption.FindStringSubmatch(normCell(line))
	if m == nil {
		return scoringCaption{}
	}
	weight, _ := strconv.ParseFloat(m[3], 64)
	return scoringCaption{name: m[1], group: classifyScoreGroup(m[2], ""), weight: weight}
}

func applyScoringCaption(items []*ScoredItem, caption scoringCaption) {
	if caption.group == "" {
		return
	}
	for _, item := range items {
		if item.Group == "" {
			item.Group, item.GroupWeight, item.ScoreGroup = caption.name, caption.weight, caption.group
		}
	}
}

// parseSeparateScoringTable recognizes tables with a declaration above a detailed header,
// isolated price calculations, and deductions assessed after contract award.
func parseSeparateScoringTable(table *scoreTable, caption scoringCaption, section string) ([]*ScoredItem, *scoreCarry, bool) {
	if len(table.Grid) == 0 {
		return nil, nil, false
	}
	if rePerformanceSection.MatchString(normCell(section)) {
		for _, cell := range table.Grid[0] {
			if tag := normCell(cell); tag == "扣分" || tag == "扣分记录" {
				return nil, nil, true
			}
		}
	}
	if items, carry := parseScoreTableWithDeclaration(table); len(items) > 0 {
		return items, carry, true
	}
	if caption.group == ScoreGroupPrice {
		if item := scoreIsolatedPriceTable(table, caption); item != nil {
			return []*ScoredItem{item}, nil, true
		}
	}
	return nil, nil, false
}

// The upper rows give the group maxima and the lower header gives the actual column roles.
// Only the header cells are normalized so the parser returns the original row indices.
func parseScoreTableWithDeclaration(table *scoreTable) ([]*ScoredItem, *scoreCarry) {
	for row, header := range table.Grid[:min(len(table.Grid), 4)] {
		if len(header) != 4 || normCell(header[0]) != "评审因素分类" || headerRole(header[1]) != "factor" ||
			(normCell(header[2]) != "详细描述" && headerRole(header[2]) != "criteria") || headerRole(header[3]) != "score" {
			continue
		}
		declared := map[string]float64{}
		for _, line := range table.Grid[:row] {
			if len(line) == 0 || normCell(line[0]) != "分值构成" {
				continue
			}
			for _, m := range reScoreDeclaration.FindAllStringSubmatch(normCell(strings.Join(line, "")), -1) {
				weight, _ := strconv.ParseFloat(m[2], 64)
				declared[classifyScoreGroup(m[1], "")] = weight
			}
		}
		if len(declared) == 0 {
			return nil, nil
		}
		adapted := *table
		adapted.Grid = make([][]string, len(table.Grid))
		adapted.Grid[row] = []string{"评分类别", "评分项", "评分标准", "分值"}
		copy(adapted.Grid[row+1:], table.Grid[row+1:])
		items, carry, _ := parseScoringTable(&adapted, nil)
		for _, item := range items {
			item.GroupWeight = declared[item.ScoreGroup]
		}
		return items, carry
	}
	return nil, nil
}

func scoreIsolatedPriceTable(table *scoreTable, caption scoringCaption) *ScoredItem {
	if len(table.Grid) == 1 && len(table.Grid[0]) == 1 {
		if criteria := table.Grid[0][0]; caption.weight > 0 && isBidPriceFormula(criteria) {
			return &ScoredItem{Group: caption.name, GroupWeight: caption.weight, ScoreGroup: ScoreGroupPrice,
				Name: "价格评分", Score: caption.weight, Criteria: criteria, Rows: []int{0}, Page: table.PageStart}
		}
		return nil
	}
	if len(table.Grid) != 2 {
		return nil
	}
	schema, ok := detectScoreSchema(table)
	if !ok || schema.header != 0 || schema.factor < 0 || schema.score < 0 || schema.criteria < 0 {
		return nil
	}
	row := table.Grid[1]
	if max(schema.factor, schema.score, schema.criteria) >= len(row) {
		return nil
	}
	name, _ := splitGroup(row[schema.factor])
	criteria := row[schema.criteria]
	_, score, ok := parseScoreText(row[schema.score])
	if !ok || score <= 0 || !rePriceGroupLabel.MatchString(name) || !isBidPriceFormula(criteria) {
		return nil
	}
	return &ScoredItem{Group: caption.name, GroupWeight: caption.weight, ScoreGroup: ScoreGroupPrice,
		Name: name, Score: score, Criteria: criteria, Rows: []int{1}, Page: table.PageStart}
}

// separatePriceFormula requires both the price calculation and an accompanying explicit weight.
// A weight allocation table on its own is a declaration, not a scoring factor.
func separatePriceFormula(source *chunkSource, tables []scoreTable) *ScoredItem {
	text := source.Content
	for _, table := range tables {
		if table.Markdown != "" {
			text = strings.Replace(text, table.Markdown, "\n", 1)
		}
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		caption := matchScoringCaption(line)
		if caption.group == ScoreGroupPrice && caption.weight > 0 {
			body := strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
			if isBidPriceFormula(body) {
				return separatePriceItem(caption, body)
			}
		}
	}
	title := source.SectionTitle
	if i := strings.IndexAny(title, "：:"); i >= 0 {
		title = title[:i]
	}
	if !rePriceGroupLabel.MatchString(normCell(title)) || !isBidPriceFormula(text) {
		return nil
	}
	for _, table := range tables {
		if score := declaredPriceColumn(&table); score > 0 {
			caption := scoringCaption{name: "价格评分", group: ScoreGroupPrice, weight: score}
			return separatePriceItem(caption, strings.TrimSpace(text))
		}
	}
	return nil
}

func separatePriceItem(caption scoringCaption, text string) *ScoredItem {
	return &ScoredItem{Group: caption.name, GroupWeight: caption.weight, ScoreGroup: ScoreGroupPrice,
		Name: "价格评分", Score: caption.weight, Criteria: text, Rows: []int{0}}
}

func declaredPriceColumn(table *scoreTable) float64 {
	if len(table.Grid) < 2 || len(table.Grid[0]) < 2 {
		return 0
	}
	for col, cell := range table.Grid[0] {
		if col == 0 || !rePriceGroupLabel.MatchString(normCell(cell)) {
			continue
		}
		for _, row := range table.Grid[1:] {
			if len(row) <= col || normCell(row[0]) != "分值" {
				continue
			}
			if _, score, ok := parseScoreText(row[col]); ok {
				return score
			}
		}
	}
	return 0
}

func isBidPriceFormula(text string) bool {
	text = normCell(text)
	return strings.Contains(text, "基准价") && rePriceFormula.MatchString(text)
}
