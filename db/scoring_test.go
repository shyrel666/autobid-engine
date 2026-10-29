package db

// 评分表结构化解析单元测试（纯规则，不需要数据库）。样本取自真实招标评分表的几种版式（文字已缩短）。

import (
	"encoding/json"
	"strings"
	"testing"
)

// cellSpec 描述一个原始单元格；rowSpan > 1 表示纵向合并（网格中重复该文字）。
type cellSpec struct {
	row, col, rowSpan int
	text              string
	page              int
}

// buildScoreTable 由逐格文字构造表格：rows 中空串且被上方 merges 覆盖的位置不单独成格。
func buildScoreTable(rows [][]string, merges ...cellSpec) *scoreTable {
	t := &scoreTable{Rows: len(rows), Cols: len(rows[0])}
	covered := map[[2]int]bool{}
	for _, m := range merges {
		for r := m.row + 1; r < m.row+m.rowSpan; r++ {
			covered[[2]int{r, m.col}] = true
		}
	}
	t.Grid = make([][]string, len(rows))
	for r, row := range rows {
		t.Grid[r] = append([]string(nil), row...)
	}
	for r, row := range rows {
		for c := range row {
			if covered[[2]int{r, c}] {
				continue
			}
			cs := scoreCell{Row: r, Col: c, RowSpan: 1, ColSpan: 1, Text: row[c], Page: 1}
			for _, m := range merges {
				if m.rowSpan > 0 && m.row == r && m.col == c {
					cs.RowSpan = m.rowSpan
					for rr := r; rr < r+m.rowSpan; rr++ {
						t.Grid[rr][c] = row[c]
					}
				}
			}
			for _, m := range merges {
				if m.rowSpan == 0 && m.row == r && m.page > 0 { // 只标页码
					cs.Page = m.page
				}
			}
			t.Cells = append(t.Cells, cs)
		}
	}
	return t
}

type wantItem struct {
	name, group string
	score       float64
}

func checkItems(t *testing.T, items []*ScoredItem, want []wantItem) {
	t.Helper()
	if len(items) != len(want) {
		var got []string
		for _, it := range items {
			got = append(got, it.Name)
		}
		t.Fatalf("got %d items %q, want %d", len(items), got, len(want))
	}
	for i, w := range want {
		it := items[i]
		if it.Name != w.name || it.ScoreGroup != w.group || it.Score != w.score {
			t.Fatalf("item %d = {%q %s %.4g}, want {%q %s %.4g}", i, it.Name, it.ScoreGroup, it.Score, w.name, w.group, w.score)
		}
	}
}

func TestParseScoreText(t *testing.T) {
	cases := []struct {
		in    string
		label string
		score float64
		ok    bool
	}{
		{"30", "", 30, true},
		{"20分", "", 20, true},
		{"0-12", "", 12, true},
		{"5.0（0,2.5,5）", "", 5, true},
		{"10.0（0.0- 10.0）", "", 10, true},
		{"基础服务20", "基础服务", 20, true},
		{"现场\n演示24分", "现场演示", 24, true},
		{"项目的认识（10分）", "项目的认识", 10, true},
		{"报价\n（20分）", "报价", 20, true},
		{"", "", 0, false},
		{"详见评分标准", "", 0, false},
	}
	for _, c := range cases {
		label, score, ok := parseScoreText(c.in)
		if label != c.label || score != c.score || ok != c.ok {
			t.Errorf("parseScoreText(%q) = %q %.4g %v, want %q %.4g %v", c.in, label, score, ok, c.label, c.score, c.ok)
		}
	}
}

// 重庆版式：「评分因素及权重」列填评分类别，评分因素名与分值写在「分值」列；同一因素纵向合并的分值格只计一次。
func TestParseScoringTableNameInScoreColumn(t *testing.T) {
	tb := buildScoreTable([][]string{
		{"序号", "评分因素\n及权重", "分值", "评分标准", "说明"},
		{"1", "投标报价\n（30%）", "30", "最低价为评标基准价……", ""},
		{"2", "服务部分\n（40%）", "基础服务20", "1.起评分：20分……", ""},
		{"2", "服务部分\n（40%）", "现状分析8", "阐述对本项目现状的理解……", "提供相关方案"},
		{"2", "服务部分\n（40%）", "服务\n方案\n12", "提出服务方案……", "提供相关方案"},
		{"3", "商务部分\n（30%）", "人员配置24", "项目负责人具备……", "提供证书"},
		{"3", "商务部分\n（30%）", "", "技术负责人具备……", "提供证书"},
		{"3", "商务部分\n（30%）", "投标人技术能力2", "具有软件著作权……", ""},
		{"3", "商务部分\n（30%）", "项目经验4", "类似业绩……", ""},
	}, cellSpec{row: 5, col: 2, rowSpan: 2, text: "人员配置24"})
	items, _, summary := parseScoringTable(tb, nil)
	if summary != nil {
		t.Fatal("scoring table taken as a summary")
	}
	checkItems(t, items, []wantItem{
		{"投标报价", ScoreGroupPrice, 30}, {"基础服务", ScoreGroupTechnical, 20}, {"现状分析", ScoreGroupTechnical, 8},
		{"服务方案", ScoreGroupTechnical, 12}, {"人员配置", ScoreGroupCommercial, 24}, {"投标人技术能力", ScoreGroupCommercial, 2},
		{"项目经验", ScoreGroupCommercial, 4},
	})
	if it := items[4]; !strings.Contains(it.Criteria, "项目负责人") || !strings.Contains(it.Criteria, "技术负责人") || it.Group != "商务部分" || it.GroupWeight != 30 {
		t.Fatalf("merged criteria / group lost: %+v", it)
	}
	// 同名分项各自独立成格（不是纵向合并）且带相同标签时同样只计一次。
	tb2 := buildScoreTable([][]string{
		{"序号", "评分因素及权重", "分值", "评分标准"},
		{"1", "商务部分（30%）", "人员配置24", "项目负责人……"},
		{"1", "商务部分（30%）", "人员配置24", "技术负责人……"},
	})
	items2, _, _ := parseScoringTable(tb2, nil)
	checkItems(t, items2, []wantItem{{"人员配置", ScoreGroupCommercial, 24}})
}

// 发改委版式：分值列只有分值，评分因素名取自评分标准首行；以年份开头的短前缀延伸到下一个标点。
func TestParseScoringTableNameFromCriteria(t *testing.T) {
	tb := buildScoreTable([][]string{
		{"序号", "评分因素及权重", "分值", "评分标准", "说明"},
		{"1", "投标报价（20%）", "20分", "最低价为评标基准价", ""},
		{"2", "技术部分\n（50%）", "10分", "1. 项目理解\n投标人对本项目……", ""},
		{"2", "技术部分\n（50%）", "10分", "2. 运维工作方案\n投标人提出……", ""},
		{"3", "商务部分（30%）", "9分", "2023年1月1日以来，与本项目类似的信息化项目（运维类）……", ""},
	})
	items, _, _ := parseScoringTable(tb, nil)
	checkItems(t, items, []wantItem{
		{"投标报价", ScoreGroupPrice, 20}, {"项目理解", ScoreGroupTechnical, 10}, {"运维工作方案", ScoreGroupTechnical, 10},
		{"2023年1月1日以来，与本项目类似的信息化项目", ScoreGroupCommercial, 9},
	})
}

// PDF 框线版式：两列「评审因素」表头（前列为类别）、类别格纵向合并后留空、同一因素纵向合并的分项分值相加、
// 因素格留空的自带分值分项、跨页拆开的因素名续写、区间分值与「合计」行。
func TestParseScoringTableMergedFactorsAndPageSplit(t *testing.T) {
	tb := buildScoreTable([][]string{
		{"评审因素", "评审因素", "具体要求", "分值"},
		{"价格部分（10分）", "投标报价", "最低价为基准价", "10"},
		{"技术部分（30分）", "可信管控响应方案", "提供方案……", "4"},
		{"", "可信管控响应方案", "（1）功能描述详细……", "5"},
		{"", "人员配备方案", "项目经理……", "5"},
		{"", "", "项目实施团队人员包含……", "4"},
		{"", "安全保密管理措施和技", "提供保密承诺，得1分", "1"},
		{"", "术方案", "注：保密承诺函……", ""},
		{"", "术方案", "（1）内容详实……", "3"},
		{"", "需求分析", "投标人应结合……", "0-8"},
		{"", "", "阐述比较清晰，得6分……", ""},
		{"合计", "合计", "", "100"},
	}, cellSpec{row: 2, col: 1, rowSpan: 2, text: "可信管控响应方案"},
		cellSpec{row: 7, col: 1, rowSpan: 2, text: "术方案"},
		cellSpec{row: 7, page: 2}, cellSpec{row: 8, page: 2}, cellSpec{row: 9, page: 2}, cellSpec{row: 10, page: 2}, cellSpec{row: 11, page: 2})
	items, _, _ := parseScoringTable(tb, nil)
	checkItems(t, items, []wantItem{
		{"投标报价", ScoreGroupPrice, 10}, {"可信管控响应方案", ScoreGroupTechnical, 9}, {"人员配备方案", ScoreGroupTechnical, 9},
		{"安全保密管理措施和技术方案", ScoreGroupTechnical, 4}, {"需求分析", ScoreGroupTechnical, 8},
	})
	if !strings.Contains(items[4].Criteria, "得6分") {
		t.Fatalf("continuation row criteria lost: %q", items[4].Criteria)
	}
}

// 履约考核表、符合性审查表不是评分表；只有类别合计的表是分值汇总表。
func TestParseScoringTableRejectsNonScoringTables(t *testing.T) {
	assess := buildScoreTable([][]string{
		{"序号", "考核项目", "考核内容", "扣分标准"},
		{"1", "系统稳定性", "对系统日常运行……", "单次时长≤60分钟扣2分"},
	})
	if items, _, _ := parseScoringTable(assess, nil); items != nil {
		t.Fatalf("assessment table parsed as scoring: %+v", items)
	}
	review := buildScoreTable([][]string{
		{"序号", "评审因素", "评审因素", "评审标准"},
		{"1", "有效性审查", "投标文件签署或盖章", "法定代表人签字……"},
	})
	if items, _, _ := parseScoringTable(review, nil); items != nil {
		t.Fatalf("compliance review table parsed as scoring: %+v", items)
	}
	summaryTable := buildScoreTable([][]string{
		{"序号", "评分因素", "分值", "备注"},
		{"1", "商务部分", "30", ""},
		{"2", "技术部分", "60", ""},
		{"3", "价格部分", "10", ""},
		{"合计", "合计", "100", ""},
	})
	items, carry, summary := parseScoringTable(summaryTable, nil)
	if items != nil || carry == nil || summary["技术部分"] != 60 || summary["商务部分"] != 30 || summary["价格部分"] != 10 {
		t.Fatalf("summary table: items=%+v summary=%v", items, summary)
	}
}

// 跨页拆成两张表时，下一张表开头留空的类别格沿用上一张表末行类别；没有表头的续表沿用列角色。
func TestParseScoringTableCarriesGroupAcrossTables(t *testing.T) {
	first := buildScoreTable([][]string{
		{"评分内容", "评分因素", "评分标准", "分值"},
		{"商务部分（40分）", "资质", "ISO9001……", "6分"},
	})
	items, carry, _ := parseScoringTable(first, nil)
	checkItems(t, items, []wantItem{{"资质", ScoreGroupCommercial, 6}})
	second := buildScoreTable([][]string{
		{"", "业绩案例", "近5年……", "14分"},
		{"技术部分（50分）", "需求理解", "理解透彻……", "5分"},
	})
	items2, _, _ := parseScoringTable(second, carry)
	checkItems(t, items2, []wantItem{{"业绩案例", ScoreGroupCommercial, 14}, {"需求理解", ScoreGroupTechnical, 5}})
	if items2[0].GroupWeight != 40 {
		t.Fatalf("carried group weight lost: %+v", items2[0])
	}
	if items3, _, _ := parseScoringTable(second, nil); items3 != nil {
		t.Fatalf("header-less table without a preceding scoring table must not parse: %+v", items3)
	}
}

func TestClassifyScoreGroup(t *testing.T) {
	cases := []struct{ group, name, want string }{
		{"投标报价", "投标报价", ScoreGroupPrice},
		{"价格分", "价格评审", ScoreGroupPrice},
		{"技术（质量）部分", "现场演示", ScoreGroupTechnical},
		{"服务部分", "人员配置", ScoreGroupTechnical},
		{"商务评分", "人员要求", ScoreGroupCommercial},
		{"客观分（商务、技术及服务部分）", "投标人可提供的其他商务材料", ScoreGroupCommercial},
		{"客观分（商务、技术及服务部分）", "稿件内容校对", ScoreGroupTechnical},
		{"", "类似项目业绩", ScoreGroupCommercial},
		{"", "总体设计方案", ScoreGroupTechnical},
	}
	for _, c := range cases {
		if got := classifyScoreGroup(c.group, c.name); got != c.want {
			t.Errorf("classifyScoreGroup(%q, %q) = %s, want %s", c.group, c.name, got, c.want)
		}
	}
}

func TestLastLotMarker(t *testing.T) {
	cases := map[string]string{
		"（一）包1评审因素":              "1",
		"包二评审因素":                 "2",
		"1.A包：2026年度门户网站群运维":     "A",
		"3.c包：综合业务管理平台优化完善":      "C",
		"第2包详细评审标准":              "2",
		"第一标段评分标准":               "1",
		"评分内容包括项目理解":             "",
		"推荐排名第一的为本包（项目）中标候选人":    "",
		"包1：……\n说明文字\n包2评审因素见下表": "2",
	}
	for in, want := range cases {
		if got := lastLotMarker(in); got != want {
			t.Errorf("lastLotMarker(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSummarizeScoringWarnsOnMismatch(t *testing.T) {
	loc := func(lot, group string, w float64) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"lot": lot, "group": group, "group_weight": w})
		return b
	}
	items := []*RequirementItem{
		{Category: ReqScoringItem, ScoreGroup: ScoreGroupPrice, ScoreWeight: 10, SourceLoc: loc("A", "价格部分", 10)},
		{Category: ReqScoringItem, ScoreGroup: ScoreGroupTechnical, ScoreWeight: 50, SourceLoc: loc("A", "技术部分", 60)},
		{Category: ReqScoringItem, ScoreGroup: ScoreGroupCommercial, ScoreWeight: 40, SourceLoc: loc("A", "商务部分", 40)},
		{Category: ReqScoringItem, ScoreGroup: ScoreGroupPrice, ScoreWeight: 20, SourceLoc: loc("B", "", 0)},
		{Category: ReqScoringItem, ScoreGroup: ScoreGroupTechnical, ScoreWeight: 80, SourceLoc: loc("B", "", 0)},
		{Category: ReqMandatoryClause, ScoreWeight: 5, SourceLoc: loc("B", "", 0)},
	}
	out := summarizeScoring(items)
	if len(out) != 2 || out[0].Lot != "A" || out[0].Total != 100 || out[0].Technical != 50 || out[1].Total != 100 || out[1].Price != 20 {
		t.Fatalf("summary: %+v", out)
	}
	if len(out[0].Warnings) != 1 || !strings.Contains(out[0].Warnings[0], "技术部分") || len(out[1].Warnings) != 0 {
		t.Fatalf("warnings: %+v / %+v", out[0].Warnings, out[1].Warnings)
	}
	short := summarizeScoring(items[:2])
	if len(short) != 1 || len(short[0].Warnings) != 2 {
		t.Fatalf("total != 100 must warn: %+v", short)
	}
}

func TestDefaultOutlineSectionsScopesScoringGroups(t *testing.T) {
	reqs := []*RequirementItem{
		{ID: 5, Code: "SC-5", Category: ReqScoringItem, ScoreGroup: ScoreGroupTechnical, Title: "运维方案", ScoreWeight: 10},
		{ID: 2, Code: "SC-2", Category: ReqScoringItem, ScoreGroup: ScoreGroupTechnical, Title: "项目理解", ScoreWeight: 20},
		{ID: 1, Code: "SC-1", Category: ReqScoringItem, ScoreGroup: ScoreGroupPrice, Title: "投标报价", ScoreWeight: 30},
		{ID: 3, Code: "SC-3", Category: ReqScoringItem, ScoreGroup: ScoreGroupCommercial, Title: "类似业绩", ScoreWeight: 8},
		{ID: 4, Code: "SC-4", Category: ReqScoringItem, Title: "人工新增评分项", ScoreWeight: 2},
		{ID: 6, Code: "MC-6", Category: ReqMandatoryClause, Title: "★ 工期", IsMandatory: true},
	}
	secs := DefaultOutlineSections(reqs)
	var titles []string
	mapped := map[int64]bool{}
	for _, s := range secs {
		titles = append(titles, s.Code+" "+s.Title)
		for _, id := range s.RequirementIDs {
			mapped[id] = true
		}
	}
	got := strings.Join(titles, "|")
	want := "1 总体技术方案|2 技术评分项逐项响应|2.1 项目理解|2.2 人工新增评分项|2.3 运维方案|3 商务评分项响应（资信与证明材料）|4 ★ 实质性条款逐条响应"
	if got != want {
		t.Fatalf("outline:\n got %s\nwant %s", got, want)
	}
	if mapped[1] || !mapped[2] || !mapped[3] || !mapped[4] || !mapped[5] || !mapped[6] {
		t.Fatalf("price item must stay unmapped, all others mapped: %v", mapped)
	}
}
