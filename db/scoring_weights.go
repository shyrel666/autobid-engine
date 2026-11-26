package db

import (
	"math"
	"regexp"
	"strings"
)

var scoreFactorHeading = regexp.MustCompile(`(?m)^\s*(?:\d+(?:\.\d+)+\s*|[（(]\d+[）)]\s*)?([^\n：:（(]{2,32})[:：]?\s*[（(]((?:满分)?\d+(?:\.\d+)?分)[）)]`)

func scoreHeader(row []string, columns ...string) bool {
	if len(row) != len(columns) {
		return false
	}
	for i, name := range columns {
		if normCell(row[i]) != name {
			return false
		}
	}
	return true
}

func scoreRowPage(t *scoreTable, row int) int {
	for _, cell := range t.Cells {
		if cell.Row == row && cell.Page > 0 {
			return cell.Page
		}
	}
	return t.PageStart
}

func addScoreContinuation(item *ScoredItem, row int, text string) {
	if item == nil || strings.TrimSpace(text) == "" {
		return
	}
	item.Rows = append(item.Rows, row)
	item.Criteria = strings.TrimSpace(item.Criteria + "\n" + strings.TrimSpace(text))
	item.Mandatory = item.Mandatory || reMandatoryMrk.MatchString(text)
}

func weightedScoreItem(t *scoreTable, row int, name, group string, points, declared float64, criteria, note string) *ScoredItem {
	return &ScoredItem{Name: name, Group: group, GroupWeight: declared, ScoreGroup: classifyScoreGroup(group, name),
		Score: points, Criteria: strings.TrimSpace(criteria), Note: strings.TrimSpace(note),
		Rows: []int{row}, Page: scoreRowPage(t, row), Mandatory: reMandatoryMrk.MatchString(name + criteria)}
}

// parseWeightedScoringTable handles layouts whose header states where the
// weight lives. In particular, a weight is never a factor or a repeated score.
func parseWeightedScoringTable(t *scoreTable) ([]*ScoredItem, bool) {
	header := firstRow(t.Grid)
	switch {
	case scoreHeader(header, "序号", "评分项及权值", "分值", "评定细则"):
		var items []*ScoredItem
		var current *ScoredItem
		for i, row := range t.Grid[1:] {
			if len(row) != 4 {
				continue
			}
			name, weight := splitGroup(row[1])
			if _, points, ok := parseScoreText(row[2]); ok && points > 0 && name != "" {
				group := ""
				if rePriceName.MatchString(name) {
					group = name
				}
				current = weightedScoreItem(t, i+1, name, group, points, weight, row[3], "")
				items = append(items, current)
			} else if name == "" {
				addScoreContinuation(current, i+1, row[3])
			}
		}
		return items, true
	case scoreHeader(header, "评标内容", "评标原则与标准"):
		return parseInlineFactorScores(t)
	case scoreHeader(header, "分项", "技术及商务资信分", "投标报价得分", "政策性加分", "总分"):
		return nil, true // 横向综合评分汇总，分值由前面的明细表产出。
	case scoreHeader(header, "序号", "类型", "评分标准", "分值", "说明"),
		scoreHeader(header, "序号", "类型", "评分标准", "分值权重", "说明"):
		return parseScoringByType(t)
	case scoreHeader(header, "序号", "评分因素", "评分内容", "满分"):
		return parseNumberedScoreGroups(t)
	}
	return nil, false
}

func parseInlineFactorScores(t *scoreTable) ([]*ScoredItem, bool) {
	var items []*ScoredItem
	var current *ScoredItem
	var group string
	var declared float64
	for i, row := range t.Grid[1:] {
		if len(row) != 2 || strings.Contains(normCell(row[0]), "总得分") {
			continue
		}
		name, points := splitGroup(row[0])
		newGroup := name != "" && name != group
		if newGroup {
			group, declared = name, points
		}
		if m := scoreFactorHeading.FindStringSubmatch(row[1]); m != nil && group != "" {
			name = cleanName(m[1])
			_, points, _ = parseScoreText(strings.TrimPrefix(m[2], "满分"))
		} else if !newGroup {
			addScoreContinuation(current, i+1, row[1])
			continue
		}
		if points > 0 {
			current = weightedScoreItem(t, i+1, name, group, points, declared, row[1], "")
			items = append(items, current)
		}
	}
	return items, len(items) > 0
}

func parseScoringByType(t *scoreTable) ([]*ScoredItem, bool) {
	var items []*ScoredItem
	var current *ScoredItem
	for i, row := range t.Grid[1:] {
		if len(row) != 5 {
			continue
		}
		group, _ := splitGroup(row[1])
		_, points, ok := parseScoreText(row[3])
		if !ok || points <= 0 {
			if group == "" {
				addScoreContinuation(current, i+1, row[2])
			}
			continue
		}
		policy := strings.Contains(group, "政策性加分")
		children := scoreFactorHeading.FindAllStringSubmatchIndex(row[2], -1)
		var childrenTotal float64
		var parts []*ScoredItem
		for j, match := range children {
			name := cleanName(row[2][match[2]:match[3]])
			_, v, _ := parseScoreText(strings.TrimPrefix(row[2][match[4]:match[5]], "满分"))
			end := len(row[2])
			if j+1 < len(children) {
				end = children[j+1][0]
			}
			parts = append(parts, weightedScoreItem(t, i+1, name, group, v, 0, row[2][match[0]:end], row[4]))
			childrenTotal += v
		}
		if len(parts) > 0 && math.Abs(childrenTotal-points) < 0.001 {
			items = append(items, parts...)
			current = parts[len(parts)-1]
			if policy {
				for _, part := range parts {
					part.Group, part.ScoreGroup = "政策性加分", ""
				}
			}
			continue
		}
		name := group
		if name == "" || !rePriceGroup.MatchString(group) {
			if candidate := nameFromCriteria(row[2]); candidate != "" {
				name = candidate
			}
		}
		current = weightedScoreItem(t, i+1, name, group, points, 0, row[2], row[4])
		if policy {
			current.Group, current.ScoreGroup = "政策性加分", ""
		}
		items = append(items, current)
	}
	return items, len(items) > 0
}

func parseNumberedScoreGroups(t *scoreTable) ([]*ScoredItem, bool) {
	hasSubtotal := false
	for _, row := range t.Grid[1:] {
		if len(row) == 4 && strings.Contains(normCell(row[3]), "总分值") && normCell(row[2]) == "评审因素" {
			hasSubtotal = true
			break
		}
	}
	if !hasSubtotal {
		return nil, false
	}
	var items []*ScoredItem
	var current *ScoredItem
	var group string
	var declared float64
	for i, row := range t.Grid[1:] {
		if len(row) != 4 || strings.HasPrefix(normCell(row[0]), "总得分") {
			continue
		}
		if strings.Contains(normCell(row[3]), "总分值") && normCell(row[2]) == "评审因素" {
			group = cleanName(row[1])
			_, declared = splitGroup(row[3])
			current = nil
			continue
		}
		name := cleanName(row[1])
		_, points, ok := parseScoreText(row[3])
		if name == "" || !ok || points <= 0 {
			addScoreContinuation(current, i+1, row[2])
			continue
		}
		if rePriceName.MatchString(name) {
			group, declared = name, points
		}
		current = weightedScoreItem(t, i+1, name, group, points, declared, row[2], "")
		items = append(items, current)
	}
	return items, true
}
