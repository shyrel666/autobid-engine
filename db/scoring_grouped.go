package db

import (
	"regexp"
	"strconv"
	"strings"
)

// 分组评分表用带声明分值的合并标题覆盖序号、因素和细则三列，分值列保留独立表头。
var reGroupedScoreHeading = regexp.MustCompile(`^第[一二三四五六七八九十百0-9]+部分[、.．:：]?(价格|报价|(?:价格|报价|客观|主观|技术|商务|服务|资信)(?:部分|评分|分))[（(]([0-9]+(?:\.[0-9]+)?)分[）)]$`)

func groupedScoreHeading(row []string, sharedHeading bool) (name string, weight float64) {
	if !sharedHeading || headerRole(row[3]) != "score" {
		return "", 0
	}
	match := reGroupedScoreHeading.FindStringSubmatch(normCell(row[0]))
	if match == nil {
		return "", 0
	}
	weight, _ = strconv.ParseFloat(match[2], 64)
	return match[1], weight
}

func parseGroupedScoreTable(t *scoreTable) ([]*ScoredItem, bool) {
	if len(firstRow(t.Grid)) != 4 {
		return nil, false
	}
	origins := t.origins()
	var (
		items       []*ScoredItem
		group       string
		groupWeight float64
		groups      = make(map[string]bool)
	)
	for i, row := range t.Grid {
		if len(row) != 4 {
			return nil, false
		}
		first := normCell(row[0])
		if normCell(strings.Join(row, "")) == "" {
			continue
		}
		source := origins[i]
		merged := source[0] >= 0 && source[3] >= 0 &&
			source[0] == source[1] && source[1] == source[2] && source[2] != source[3] &&
			t.Cells[source[0]].Row == i && t.Cells[source[3]].Row == i
		if name, weight := groupedScoreHeading(row, merged); name != "" {
			group, groupWeight = name, weight
			groups[group] = true
			continue
		}
		if group == "" {
			return nil, false
		}
		if reTotalRow.MatchString(first) {
			continue
		}
		ordinal, err := strconv.Atoi(first)
		name, criteria := cleanName(row[1]), strings.TrimSpace(row[2])
		label, score, hasScore := parseScoreText(row[3])
		if err != nil || ordinal <= 0 || !hasScore || label != "" || name == "" || criteria == "" {
			return nil, false
		}
		for column, cell := range source {
			if cell < 0 || t.Cells[cell].Row != i || t.Cells[cell].Col != column {
				return nil, false
			}
		}
		scoreGroup := classifyScoreGroup(group, name)
		if scoreGroup == ScoreGroupPrice && !rePriceGroup.MatchString(group) {
			// 单列于主观或客观组的价格测算方案仍是方案评分。投标报价只由价格组计分。
			scoreGroup = ScoreGroupTechnical
		}
		items = append(items, &ScoredItem{
			Group: group, GroupWeight: groupWeight, ScoreGroup: scoreGroup,
			Name: name, Score: score, Criteria: criteria,
			Mandatory: reMandatoryMrk.MatchString(name + criteria),
			Rows:      []int{i}, Page: t.Cells[source[0]].Page,
		})
	}
	if len(groups) < 2 || len(items) == 0 {
		return nil, false
	}
	return items, true
}

func groupedScoreEvalKind(group string) string {
	switch group {
	case "客观分", "客观部分", "客观评分":
		return EvalObjective
	case "主观分", "主观部分", "主观评分":
		return EvalSubjective
	default:
		return ""
	}
}
