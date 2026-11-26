package db

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// complexScoreCarry connects a grouped evaluation annex to a directly adjacent,
// headerless continuation. Items always keep the table and row where they began.
type complexScoreCarry struct {
	group       string
	weight      float64
	lastPage    int
	last        *ScoredItem
	declaration map[string]float64
}

type complexScoreLayout struct {
	header, group, factor, criteria int
	clauseAnnex                     bool
}

var (
	reComplexScoreHeading = regexp.MustCompile(`^(.*?)(?:[（(](?:满分|最高得|最高|分值)?[:：]?(\d+(?:\.\d+)?)分[）)]|[:：](\d+(?:\.\d+)?)分)$`)
	reComplexScoreGroup   = regexp.MustCompile(`^(?:[一二三四五六七八九十]+[、.．])?(?:投标报价|报价|价格|商务|技术|资信(?:及其他)?)(?:部分|得分|分值|分|评分)?$`)
	reComplexDeclaration  = regexp.MustCompile(`((?:投标报价|报价|价格|商务|技术|资信(?:及其他)?)(?:部分|得分|分值|分|评分)?)[:：](\d+(?:\.\d+)?)分`)
	reComplexClause       = regexp.MustCompile(`^(?:\d+\.)+\d+`)
)

func complexScoreHeading(text string) (string, float64, bool) {
	m := reComplexScoreHeading.FindStringSubmatch(normCell(text))
	if m == nil || m[1] == "" {
		return "", 0, false
	}
	value := m[2]
	if value == "" {
		value = m[3]
	}
	score, err := strconv.ParseFloat(value, 64)
	return m[1], score, err == nil
}

func complexScoreGroup(text string) (string, float64, bool) {
	name, weight, ok := complexScoreHeading(text)
	if !ok {
		name, weight = splitGroup(text)
	}
	return name, weight, reComplexScoreGroup.MatchString(name)
}

func complexDeclaredGroup(group string) string {
	if strings.HasPrefix(reNumbering.ReplaceAllString(group, ""), "资信") {
		return "credit"
	}
	return classifyScoreGroup(group, "")
}

func detectComplexScoreLayout(t *scoreTable, previous *complexScoreCarry) (complexScoreLayout, bool) {
	for i, row := range t.Grid {
		switch len(row) {
		case 3:
			if (normCell(row[0]) == "评分内容" || normCell(row[0]) == "分值构成") &&
				headerRole(row[1]) == "factor" && headerRole(row[2]) == "criteria" &&
				containsGroupedScoreHeads(t.Grid[i+1:], 0, 1) {
				return complexScoreLayout{header: i, group: 0, factor: 1, criteria: 2}, true
			}
		case 4, 5:
			last := len(row) - 1
			if normCell(row[0]) == "条款号" && headerRole(row[last-1]) == "factor" && headerRole(row[last]) == "criteria" &&
				containsGroupedScoreHeads(t.Grid[i+1:], 1, last-1) {
				return complexScoreLayout{header: i, group: 1, factor: last - 1, criteria: last, clauseAnnex: true}, true
			}
			if len(row) == 4 && normCell(row[0]) == "序号" && headerRole(row[1]) == "factor" &&
				normCell(row[2]) == normCell(row[3]) && headerRole(row[3]) == "criteria" && containsGroupedScoreHeads(t.Grid[i+1:], 1, 2) {
				return complexScoreLayout{header: i, group: 1, factor: 2, criteria: 3}, true
			}
		}
	}
	if previous != nil && t.PageStart >= previous.lastPage && t.PageStart <= previous.lastPage+1 {
		for _, row := range t.Grid {
			if len(row) == 4 && reComplexClause.MatchString(normCell(row[0])) {
				if _, _, ok := complexScoreGroup(row[1]); ok && containsGroupedScoreHeads(t.Grid, 1, 2) {
					return complexScoreLayout{header: -1, group: 1, factor: 2, criteria: 3, clauseAnnex: true}, true
				}
			}
		}
	}
	return complexScoreLayout{}, false
}

func containsGroupedScoreHeads(rows [][]string, groupCol, factorCol int) bool {
	for _, row := range rows {
		if len(row) <= factorCol {
			continue
		}
		name, weight, group := complexScoreGroup(row[groupCol])
		_, _, factor := complexScoreHeading(row[factorCol])
		if group && (factor || weight > 0 && rePriceGroup.MatchString(name) && len(row) > factorCol+1) {
			return true
		}
	}
	return false
}

func readComplexScoreDeclarations(rows [][]string) map[string]float64 {
	declarations := map[string]float64{}
	for _, row := range rows {
		if len(row) < 2 {
			continue
		}
		for _, title := range row[:len(row)-1] {
			if !strings.HasPrefix(normCell(title), "分值构成") {
				continue
			}
			for _, m := range reComplexDeclaration.FindAllStringSubmatch(normCell(row[len(row)-1]), -1) {
				declared, err := strconv.ParseFloat(m[2], 64)
				if err == nil && declared > 0 {
					declarations[complexDeclaredGroup(m[1])] = declared
				}
			}
			break
		}
	}
	return declarations
}

// Grouped rubrics write a factor's entire maximum in its heading. The other
// numbers in the criteria are alternatives or components of that maximum.
func parseComplexScoringTable(t *scoreTable, previous *complexScoreCarry) (items []*ScoredItem, next *complexScoreCarry, handled bool) {
	layout, ok := detectComplexScoreLayout(t, previous)
	if !ok {
		return nil, nil, false
	}
	state := complexScoreCarry{declaration: readComplexScoreDeclarations(t.Grid)}
	if layout.header < 0 && previous != nil {
		state = *previous
	}
	origins := t.origins()
	rowPages := make([]int, len(t.Grid))
	state.lastPage = t.PageStart
	for _, cell := range t.Cells {
		if cell.Row >= 0 && cell.Row < len(rowPages) && cell.Page > 0 {
			rowPages[cell.Row] = cell.Page
			state.lastPage = max(state.lastPage, cell.Page)
		}
	}
	lastFactor := -1
	lastLocal := false
	for i := layout.header + 1; i < len(t.Grid); i++ {
		row := t.Grid[i]
		if len(row) <= layout.criteria || strings.HasPrefix(normCell(row[layout.group]), "分值构成") {
			continue
		}
		groupCell := row[layout.group]
		group, weight, groupOK := complexScoreGroup(groupCell)
		if groupOK {
			state.group, state.weight = group, weight
		}
		factor, criteria := row[layout.factor], row[layout.criteria]
		factorText := normCell(factor)
		name, score, scored := complexScoreHeading(factor)
		if !scored && i+1 < len(t.Grid) && len(t.Grid[i+1]) > layout.factor {
			suffix := normCell(t.Grid[i+1][layout.factor])
			if suffix == "分）" || suffix == "分)" {
				name, score, scored = complexScoreHeading(factor + suffix)
			}
		}
		if !scored && groupOK && weight > 0 && rePriceGroup.MatchString(group) &&
			(factor == criteria || rePriceGroup.MatchString(factor)) {
			name, score, scored = group, weight, true
		}
		if !scored && (factorText != "" && factorText != "分）" && factorText != "分)" || groupOK) {
			continue
		}
		factorOrigin := origins[i][layout.factor]
		if scored && state.group != "" && !(lastLocal && factorOrigin >= 0 && lastFactor == factorOrigin) {
			item := &ScoredItem{Group: state.group, GroupWeight: state.weight, Name: cleanName(name), Score: score,
				ScoreGroup: classifyScoreGroup(state.group, name), Page: rowPages[i], Rows: []int{i}}
			declared := state.declaration[complexDeclaredGroup(state.group)]
			if item.GroupWeight == 0 {
				item.GroupWeight = declared
			}
			// Compare the declared full price group with an explicit full-price
			// factor. A component's own maximum can legitimately be smaller.
			if item.ScoreGroup == ScoreGroupPrice && declared > 0 && weight == score && declared != weight && rePriceGroup.MatchString(name) {
				item.ScoreConflict = []float64{declared, score}
				sort.Float64s(item.ScoreConflict)
				item.Score, item.GroupWeight = 0, 0
			}
			items = append(items, item)
			state.last, lastFactor, lastLocal = item, factorOrigin, true
		}
		if state.last == nil || strings.TrimSpace(criteria) == "" {
			continue
		}
		if lastLocal && state.last.Rows[len(state.last.Rows)-1] != i {
			state.last.Rows = append(state.last.Rows, i)
		}
		if state.last.Criteria != "" {
			state.last.Criteria += "\n"
		}
		state.last.Criteria += strings.TrimSpace(criteria)
		state.last.Mandatory = state.last.Mandatory || reMandatoryMrk.MatchString(factor+criteria)
	}
	if layout.clauseAnnex {
		return items, &state, true
	}
	return items, nil, true
}
