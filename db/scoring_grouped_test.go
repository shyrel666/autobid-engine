package db

import (
	"reflect"
	"testing"
)

func TestGroupedScoreTableUsesNativeHeadingsAndRows(t *testing.T) {
	table := groupedScoreTestTable([][]string{
		{"第一部分 价格（12分）", "分值"},
		{"1", "报价", "评标基准价除以投标报价再乘12。", "12"},
		{"第二部分 客观分（22分）", "分值"},
		{"1", "投标人业绩", "每项业绩得2分。", "8"},
		{"2", "服务能力", "每项具备的能力得2分。", "14"},
		{"第三部分 主观分（32分）", "分值"},
		{"1", "价格测算方案评价", "根据价格测算方案的完整性评分。", "12"},
		{"2", "★项目理解", "根据业务要求评分。", "18"},
		{"合计", "64"},
	})

	items, ok := parseGroupedScoreTable(table)
	if !ok {
		t.Fatal("did not recognize the merged group headings")
	}
	checkItems(t, items, []wantItem{
		{"报价", ScoreGroupPrice, 12},
		{"投标人业绩", ScoreGroupCommercial, 8},
		{"服务能力", ScoreGroupTechnical, 14},
		{"价格测算方案评价", ScoreGroupTechnical, 12},
		{"★项目理解", ScoreGroupTechnical, 18},
	})
	groups := []string{"价格", "客观分", "客观分", "主观分", "主观分"}
	declared := []float64{12, 22, 22, 32, 32}
	sourceRows := []int{1, 3, 4, 6, 7}
	for i, item := range items {
		if item.Group != groups[i] || item.GroupWeight != declared[i] ||
			!reflect.DeepEqual(item.Rows, []int{sourceRows[i]}) || item.Page != 2+sourceRows[i] ||
			item.Criteria != table.Grid[sourceRows[i]][2] || item.Mandatory != (i == 4) {
			t.Errorf("item %d lost source traits: %+v", i, item)
		}
	}
	if groupedScoreEvalKind(items[2].Group) != EvalObjective || groupedScoreEvalKind(items[3].Group) != EvalSubjective {
		t.Fatal("explicit evaluation mode was not preserved")
	}
}

func TestGroupedScoreTableRequiresUnambiguousSourceColumns(t *testing.T) {
	table := groupedScoreTestTable([][]string{
		{"第一部分 价格（20分）", "分值"},
		{"1", "报价", "价格计算说明。", "20"},
		{"第二部分 主观分（80分）", "分值"},
		{"1", "方案", "评分标准由评审判定。", ""},
	})
	if _, ok := parseGroupedScoreTable(table); ok {
		t.Fatal("accepted an ambiguous score cell as a complete grouped table")
	}
	table.Grid[3][3] = "80"
	table.Cells[0].ColSpan = 1
	if _, ok := parseGroupedScoreTable(table); ok {
		t.Fatal("treated unrelated columns as a merged group heading")
	}
}

// Group headings and the total row contain two source cells. Factor rows
// contain four independent cells.
func groupedScoreTestTable(rows [][]string) *scoreTable {
	table := &scoreTable{Rows: len(rows), Cols: 4}
	for i, row := range rows {
		if len(row) == 2 {
			table.Grid = append(table.Grid, []string{row[0], row[0], row[0], row[1]})
			table.Cells = append(table.Cells,
				scoreCell{Row: i, Col: 0, RowSpan: 1, ColSpan: 3, Text: row[0], Page: i + 2},
				scoreCell{Row: i, Col: 3, RowSpan: 1, ColSpan: 1, Text: row[1], Page: i + 2},
			)
			continue
		}
		table.Grid = append(table.Grid, row)
		for j, value := range row {
			table.Cells = append(table.Cells, scoreCell{Row: i, Col: j, RowSpan: 1, ColSpan: 1, Text: value, Page: i + 2})
		}
	}
	return table
}
